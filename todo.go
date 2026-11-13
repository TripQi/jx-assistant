package main

// 待办事项与定时任务 —— 宿主半边（配套原生组件 组件/todo-scheduler）。
//
// 一条持久化的任务清单 + 一个 20 秒一拍的调度循环。任务分两类：
//   - assistant（AI 任务）：到点按**执行目标**（Target）分发——
//     manager=长期助理（默认，现状）：把描述作为一条用户消息投给数字助理主对话，
//       由它按自己的编排能力安排执行（派子会话、选工作区、汇报结果都是它的事）。
//       这里不做任何模型/模式设置，也【不】等它跑完——记录落「已转交」即完成。
//     task=新的工作任务：走 createSession 单源（sessioncreate.go，与 dispatch_task
//       同一入口收敛）建一个独立的工作任务会话，把描述直接投给它执行——不打扰
//       长期助理，结果在工作任务会话与执行记录里看（sessionId 指向新会话）。
//   - command（程序任务）：到点用系统 shell 在指定工作目录直接运行命令（不经过
//     模型）。默认进 OS 级沙箱（修改工作区），任务上可显式改成完全访问；
//     退出码与输出节选进执行记录。
//
// 防重复触发：每一拍先算「刚到的槽点」，触发前立刻把 lastRunAt 落盘为该槽点，
// 同一任务在途时不再触发；时间未到的槽点一律不跑。
// 程序没开着时错过的执行不自动补跑：detectMissed 找出最近一个错过的槽点，
// 由页面提示用户「补跑 / 忽略」。
//
// 数据落在 data/jx.db 的 meta 表（globaldata:todo_tasks 一个 JSON 文档），
// 复用既有存储，不动表结构。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 常量 ----------

const (
	todoTickMs        = 20_000           // 调度循环的节拍
	todoDueFreshMs    = 10 * 60_000      // 槽点的新鲜窗口：远大于一拍，服务开着不会漏拍
	todoIntervalGrace = 60_000           // 间隔类的追赶宽限
	todoMaxRecords    = 200              // 执行记录上限（超出裁掉最旧的）
	todoCmdTimeout    = 10 * time.Minute // 程序任务单次超时
	todoDataKey       = "todo_tasks"     // meta 表里的数据键
	todoDataVersion   = 2                // 数据版本（2：任务加了「任务模型」三档字段）
	todoComponentID   = "todo-scheduler" // 承载本功能的组件 id
)

// 执行方式的种类。
const (
	schedManual   = "manual"
	schedOnce     = "once"
	schedDaily    = "daily"
	schedWeekly   = "weekly"
	schedWeekdays = "weekdays"
	schedInterval = "interval"
)

// 任务类型。
const (
	execAssistant = "assistant"
	execCommand   = "command"
)

// AI 任务（executor=assistant）的执行目标：到点把任务描述发给谁。
//   - manager：长期助理（数字助理主对话）——既有行为，默认值；
//   - task：新的工作任务——建独立工作任务会话直接执行（见 handToTaskSession）。
const (
	targetManager = "manager"
	targetTask    = "task"
)

// ---------- 数据模型 ----------

// Schedule 是执行方式。全部本地时区、分钟粒度：
//   - manual：只记在清单里，不自动执行（页面「立即执行」或用户自行处理）。
//   - once：指定时刻执行一次（epoch 毫秒）。
//   - daily / weekdays / weekly：按当天 Time（HH:mm）重复；weekly 的 Days 是 0（周日）…6（周六）。
//   - interval：每隔 EveryMinutes 分钟一次。
type Schedule struct {
	Kind         string `json:"kind"`
	At           int64  `json:"at,omitempty"`           // 仅 once
	Time         string `json:"time,omitempty"`         // 仅 daily/weekly/weekdays，"HH:mm"
	Days         []int  `json:"days,omitempty"`         // 仅 weekly
	EveryMinutes int    `json:"everyMinutes,omitempty"` // 仅 interval
}

// TaskItem 是一条待办/定时任务。
type TaskItem struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Description string   `json:"description"` // 将来原样派出去的那段话（自足，不依赖聊天记录）
	Enabled     bool     `json:"enabled"`
	Executor    string   `json:"executor"`         // assistant | command
	Target      string   `json:"target,omitempty"` // 仅 assistant 类：manager=长期助理（默认）| task=新工作任务
	// 任务模型（仅 assistant 类）三档：空=跟随派发设置（默认，老数据没这个字段就是它）/ "global"=全局默认
	// 模型 / "custom"=指定渠道+模型（Model 存完整指向，含 Key 归属）。取值链与
	// 「不可用就报错」的判定见 defaultmodels.go。
	ModelMode string    `json:"modelMode,omitempty"`
	Model     *ModelRef `json:"model,omitempty"`
	Schedule    Schedule `json:"schedule"`
	// 以下仅 command 类：工作目录、要在那里运行的命令、执行档位（沙箱 / 完全访问）。
	Dir        string `json:"dir,omitempty"`
	Command    string `json:"command,omitempty"`
	Shell      string `json:"shell,omitempty"`      // powershell（默认） | cmd
	Permission string `json:"permission,omitempty"` // workspace-write（默认） | full
	// 已完成：收进页面的「已完成」页签，调度不再触发（终态归档，与「停用」的
	// 暂停语义不同）。完成/重开由用户在卡片上标记（模型走 todo_task_update 的
	// done 参数）；DoneAt 与 CreatedAt 一样是系统时间，不接受用户指定。
	Done      bool  `json:"done,omitempty"`
	DoneAt    int64 `json:"doneAt,omitempty"` // 标记完成的时刻（重开时清零）
	CreatedAt int64 `json:"createdAt"`
	UpdatedAt int64 `json:"updatedAt"`
	// 最近一次（或最近一个被处理掉的）槽点；防重复触发与错过判定的基准。
	LastRunAt     int64  `json:"lastRunAt,omitempty"`
	LastStatus    string `json:"lastStatus,omitempty"` // running | ok | failed
	LastError     string `json:"lastError,omitempty"`
	LastSessionID string `json:"lastSessionId,omitempty"`
}

// TaskView 是任务本体加调度器算出的下次执行时间（展示用派生值）。
type TaskView struct {
	TaskItem
	NextRunAt int64 `json:"nextRunAt,omitempty"`
	// ModelView 待办卡片「模型：…（生效：…）」那行的数据：三档取值链算出来的
	// 当前实际生效值与可用性（仅 AI 任务；不可用时 Error 里是点名的原因）。
	ModelView *ModelRefView `json:"modelView,omitempty"`
}

// RunRecord 是一次执行（触发）的记录。
type RunRecord struct {
	ID        string `json:"id"`
	TaskID    string `json:"taskId"`
	Trigger   string `json:"trigger"` // schedule | manual | catch-up
	StartedAt int64  `json:"startedAt"`
	Status    string `json:"status"` // running | ok | failed
	SessionID string `json:"sessionId,omitempty"`
	Summary   string `json:"summary,omitempty"` // assistant=转交说明；command=退出码与输出节选
	Error     string `json:"error,omitempty"`
}

// MissedItem 是一条已错过、等用户决定的执行槽点。
type MissedItem struct {
	TaskID   string `json:"taskId"`
	SlotTime int64  `json:"slotTime"`
}

// TodoData 是落盘的单一文档。
type TodoData struct {
	Version int          `json:"version"`
	Tasks   []*TaskItem  `json:"tasks"`
	Records []*RunRecord `json:"records"`
}

// ---------- 调度纯函数 ----------

// parseTimeToMinutes 把 "HH:mm" 解析为当天分钟数；格式不对返回 false。
// 小时 1–2 位、分钟必须 2 位（「9:5」这类含混写法按非法处理）。
func parseTimeToMinutes(t string) (int, bool) {
	parts := strings.Split(strings.TrimSpace(t), ":")
	if len(parts) != 2 || len(parts[0]) < 1 || len(parts[0]) > 2 || len(parts[1]) != 2 {
		return 0, false
	}
	hours, err1 := parseIntStrict(parts[0])
	minutes, err2 := parseIntStrict(parts[1])
	if err1 != nil || err2 != nil || hours < 0 || hours > 23 || minutes < 0 || minutes > 59 {
		return 0, false
	}
	return hours*60 + minutes, true
}

// parseIntStrict 只接受纯数字（拒绝空串与带符号的写法）。
func parseIntStrict(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("空")
	}
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("非数字")
		}
		n = n*10 + int(r-'0')
	}
	return n, nil
}

// slotOnDay 是本地时区「某天 @ 当天分钟数」的 epoch 毫秒。
func slotOnDay(day time.Time, minutes int) int64 {
	d := time.Date(day.Year(), day.Month(), day.Day(), minutes/60, minutes%60, 0, 0, time.Local)
	return d.UnixMilli()
}

// minuteOf 取按时刻的执行方式的当天分钟数；manual/once/interval 与非法 time 返回 false。
func minuteOf(s Schedule) (int, bool) {
	switch s.Kind {
	case schedDaily, schedWeekly, schedWeekdays:
		return parseTimeToMinutes(s.Time)
	}
	return 0, false
}

// matchesDays 判断某天是否匹配 weekly 的 Days（0=周日…6=周六）。
func matchesDays(day time.Time, days []int) bool {
	for _, d := range days {
		if int(day.Weekday()) == d {
			return true
		}
	}
	return false
}

// isWeekday 是周一…周五。
func isWeekday(day time.Time) bool {
	w := int(day.Weekday())
	return w >= 1 && w <= 5
}

// lastDailySlot 取「<= now 的最近一个每天槽点」（今天或昨天）。
func lastDailySlot(now int64, minutes int) int64 {
	day := time.UnixMilli(now)
	today := slotOnDay(day, minutes)
	if today <= now {
		return today
	}
	return slotOnDay(day.AddDate(0, 0, -1), minutes)
}

// nextDailySlot 取「> from 的下一个每天槽点」。
func nextDailySlot(from int64, minutes int) int64 {
	day := time.UnixMilli(from)
	today := slotOnDay(day, minutes)
	if today > from {
		return today
	}
	return slotOnDay(day.AddDate(0, 0, 1), minutes)
}

// lastWeeklySlot 取「<= now 的最近一个匹配 Days 的槽点」（最多往回找 7 天）。
func lastWeeklySlot(now int64, days []int, minutes int) (int64, bool) {
	day := time.UnixMilli(now)
	for back := 0; back < 8; back++ {
		cursor := day.AddDate(0, 0, -back)
		if matchesDays(cursor, days) {
			if slot := slotOnDay(cursor, minutes); slot <= now {
				return slot, true
			}
		}
	}
	return 0, false
}

// nextWeeklySlot 取「> from 的下一个匹配 Days 的槽点」（最多往后找 7 天）。
func nextWeeklySlot(from int64, days []int, minutes int) (int64, bool) {
	day := time.UnixMilli(from)
	if len(days) == 0 {
		return 0, false
	}
	for ahead := 0; ahead < 8; ahead++ {
		cursor := day.AddDate(0, 0, ahead)
		if matchesDays(cursor, days) {
			if slot := slotOnDay(cursor, minutes); slot > from {
				return slot, true
			}
		}
	}
	return 0, false
}

// lastWeekdaySlot 取「<= now 的最近一个工作日槽点」。
func lastWeekdaySlot(now int64, minutes int) int64 {
	day := time.UnixMilli(now)
	for back := 0; back < 8; back++ {
		cursor := day.AddDate(0, 0, -back)
		if isWeekday(cursor) {
			if slot := slotOnDay(cursor, minutes); slot <= now {
				return slot
			}
		}
	}
	// 8 天内必有工作日，这里不可达。
	return now
}

// nextWeekdaySlot 取「> from 的下一个工作日槽点」。
func nextWeekdaySlot(from int64, minutes int) int64 {
	day := time.UnixMilli(from)
	for ahead := 0; ahead < 9; ahead++ {
		cursor := day.AddDate(0, 0, ahead)
		if isWeekday(cursor) {
			if slot := slotOnDay(cursor, minutes); slot > from {
				return slot
			}
		}
	}
	return from
}

// computeNextRunAt 是展示用的「下次执行时间」；不自动执行（manual）或永不匹配
// （weekly 空天集、时刻非法、once 已过期）时返回 false。
func computeNextRunAt(s Schedule, from, lastRunAt int64) (int64, bool) {
	switch s.Kind {
	case schedManual:
		return 0, false
	case schedOnce:
		if s.At > from {
			return s.At, true
		}
		return 0, false
	case schedDaily:
		minutes, ok := minuteOf(s)
		if !ok {
			return 0, false
		}
		return nextDailySlot(from, minutes), true
	case schedWeekly:
		minutes, ok := minuteOf(s)
		if !ok {
			return 0, false
		}
		return nextWeeklySlot(from, s.Days, minutes)
	case schedWeekdays:
		minutes, ok := minuteOf(s)
		if !ok {
			return 0, false
		}
		return nextWeekdaySlot(from, minutes), true
	case schedInterval:
		every := int64(s.EveryMinutes) * 60_000
		if every <= 0 {
			return 0, false
		}
		anchor := from
		if lastRunAt > 0 {
			anchor = lastRunAt
		}
		if next := anchor + every; next > from {
			return next, true
		}
		return from + every, true
	}
	return 0, false
}

// computeDueSlot 是调度循环用的：现在这一拍要不要触发，返回「本次槽点」
// （写进 lastRunAt 的那个值）。只认刚到的槽点：slot <= now 且 slot > lastRunAt；
// 间隔类落后超过一个节拍（加宽限）就不自动追——那属于「错过」，交给用户决定。
func computeDueSlot(s Schedule, now, lastRunAt, anchor int64) (int64, bool) {
	if s.Kind == schedManual {
		return 0, false
	}
	if s.Kind == schedOnce {
		if s.At <= now && s.At > lastRunAt && now-s.At <= todoDueFreshMs {
			return s.At, true
		}
		return 0, false
	}
	if s.Kind == schedInterval {
		every := int64(s.EveryMinutes) * 60_000
		if every <= 0 {
			return 0, false
		}
		base := lastRunAt
		if base <= 0 {
			base = anchor
		}
		next := base + every
		if next > now {
			return 0, false
		}
		// 停了很久才回来：落后超过一个节拍就不追，转错过提示。
		if now-next > every+todoIntervalGrace {
			return 0, false
		}
		return next, true
	}
	minutes, ok := minuteOf(s)
	if !ok {
		return 0, false
	}
	day := time.UnixMilli(now)
	if s.Kind == schedWeekly && !matchesDays(day, s.Days) {
		return 0, false
	}
	if s.Kind == schedWeekdays && !isWeekday(day) {
		return 0, false
	}
	slot := slotOnDay(day, minutes)
	if slot <= now && slot > lastRunAt && now-slot <= todoDueFreshMs {
		return slot, true
	}
	return 0, false
}

// detectMissed 找出服务没开着时错过的**最近一个**槽点（slot < now 且 slot > lastRunAt）；
// 没有错过返回 false。循环类只回最近一个，不连环补跑。
func detectMissed(s Schedule, now, lastRunAt, anchor int64) (int64, bool) {
	if s.Kind == schedManual {
		return 0, false
	}
	pick := func(slot int64, ok bool) (int64, bool) {
		if ok && slot < now && slot > lastRunAt {
			return slot, true
		}
		return 0, false
	}
	switch s.Kind {
	case schedOnce:
		return pick(s.At, true)
	case schedDaily:
		minutes, ok := minuteOf(s)
		if !ok {
			return 0, false
		}
		return pick(lastDailySlot(now, minutes), true)
	case schedWeekly:
		minutes, ok := minuteOf(s)
		if !ok {
			return 0, false
		}
		return pick(lastWeeklySlot(now, s.Days, minutes))
	case schedWeekdays:
		minutes, ok := minuteOf(s)
		if !ok {
			return 0, false
		}
		return pick(lastWeekdaySlot(now, minutes), true)
	case schedInterval:
		every := int64(s.EveryMinutes) * 60_000
		if every <= 0 {
			return 0, false
		}
		base := lastRunAt
		if base <= 0 {
			base = anchor
		}
		next := base + every
		// 与 computeDueSlot 的「不追」条件对齐：深落后（tick 不会追）才算错过；
		// 浅落后 tick 会自己追上，不算错过。
		return pick(next, true)
	}
	return 0, false
}

// ---------- 校验与文案 ----------

// validateSchedule 校验执行方式，不合法返回给人看的错误。
func validateSchedule(s Schedule) error {
	switch s.Kind {
	case schedManual:
		return nil
	case schedOnce:
		if s.At <= 0 {
			return fmt.Errorf("定时执行需要一个有效的执行时刻")
		}
		return nil
	case schedDaily, schedWeekdays:
		if _, ok := parseTimeToMinutes(s.Time); !ok {
			return fmt.Errorf("时刻必须是 HH:mm，收到 %q", s.Time)
		}
		return nil
	case schedWeekly:
		if _, ok := parseTimeToMinutes(s.Time); !ok {
			return fmt.Errorf("时刻必须是 HH:mm，收到 %q", s.Time)
		}
		if len(s.Days) == 0 {
			return fmt.Errorf("每周执行至少勾选一个星期")
		}
		for _, d := range s.Days {
			if d < 0 || d > 6 {
				return fmt.Errorf("星期取值必须是 0（周日）到 6（周六）")
			}
		}
		return nil
	case schedInterval:
		if s.EveryMinutes < 1 || s.EveryMinutes > 60*24*30 {
			return fmt.Errorf("间隔必须是 1 到 43200 之间的整数分钟")
		}
		return nil
	}
	return fmt.Errorf("执行方式必须是 manual / once / daily / weekly / weekdays / interval 之一")
}

// describeSchedule 是执行方式的中文一句话（模型工具与卡片共用）。
func describeSchedule(s Schedule) string {
	switch s.Kind {
	case schedManual:
		return "不定时（只记录，不自动执行）"
	case schedOnce:
		return "定时一次（" + formatMillis(s.At) + "）"
	case schedDaily:
		return "每天 " + s.Time
	case schedWeekly:
		names := make([]string, 0, len(s.Days))
		for _, d := range s.Days {
			names = append(names, weekdayName(d))
		}
		return "每周" + strings.Join(names, "、") + " " + s.Time
	case schedWeekdays:
		return "工作日每天 " + s.Time
	case schedInterval:
		return fmt.Sprintf("每隔 %d 分钟", s.EveryMinutes)
	}
	return "（未知执行方式）"
}

// describeExecutor 是任务类型（含 AI 任务的执行目标）的中文一句话。
func describeExecutor(t *TaskItem) string {
	if t.Executor == execCommand {
		return "程序任务（$ " + strings.TrimSpace(t.Command) + "｜工作目录 " + t.Dir + "）"
	}
	if t.Target == targetTask {
		return "AI 任务（到点新建工作任务会话执行）"
	}
	return "AI 任务（到点转交长期助理执行）"
}

// describeTarget 是 AI 任务执行目标的中文称呼（工具详情与立即执行的回执共用）。
func describeTarget(t *TaskItem) string {
	if t.Target == targetTask {
		return "新的工作任务"
	}
	return "长期助理"
}

// weekdayName 是星期编号的中文名。
func weekdayName(d int) string {
	names := []string{"周日", "周一", "周二", "周三", "周四", "周五", "周六"}
	if d < 0 || d > 6 {
		return fmt.Sprintf("星期%d", d)
	}
	return names[d]
}

// formatMillis 把 epoch 毫秒格式化为本地时间文本；0 显示为「—」。
func formatMillis(ms int64) string {
	if ms <= 0 {
		return "—"
	}
	return time.UnixMilli(ms).Format("2006-01-02 15:04")
}

// triggerLabel 是触发方式的中文标签。
func triggerLabel(trigger string) string {
	switch trigger {
	case "schedule":
		return "定时触发"
	case "manual":
		return "手动执行"
	case "catch-up":
		return "错过补跑"
	}
	return trigger
}

// ---------- 调度器 ----------

// todoScheduler 持有任务清单的内存副本、调度循环与两类执行。
// 全部方法并发安全；落盘复用 Store 的 meta 表（globaldata:todo_tasks）。
type todoScheduler struct {
	app *App

	mu    sync.Mutex
	data  *TodoData
	busy  map[string]bool // 在途执行（按任务 id），防同一任务并发/重复触发
	tombs map[string]bool // 本次进程内删除过的任务 id（防止磁盘合并把删除复活）
}

// todoEnabled 待办组件是否启用：组件缺失视为未启用（本功能整体是可关的）。
func (a *App) todoEnabled() bool {
	if a.Components == nil {
		return false
	}
	if _, ok := a.Components.Manifest(todoComponentID); !ok {
		return false
	}
	return a.Components.IsEnabled(todoComponentID)
}

// newTodoScheduler 从存储载入清单并启动调度循环。
func newTodoScheduler(a *App) *todoScheduler {
	s := &todoScheduler{app: a, busy: map[string]bool{}, tombs: map[string]bool{}}
	s.data = s.load()
	go s.loop()
	return s
}

// load 从 meta 表读数据；没有数据则（组件启用时）种一条示例任务；数据损坏
// 则原样保留在库里不动、按空清单重建——坏文档不会覆盖，现场还在。
func (s *todoScheduler) load() *TodoData {
	raw := s.app.Store.GetGlobalData(todoDataKey)
	if strings.TrimSpace(raw) == "" {
		if s.app.todoEnabled() {
			d := seedTodoData()
			s.persist(d)
			return d
		}
		return &TodoData{Version: todoDataVersion}
	}
	var d TodoData
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		log.Printf("[待办] 数据解析失败，按空清单重建（原文档仍留在库中 todo_bad 键下）: %v", err)
		_ = s.app.Store.SetGlobalData(todoDataKey+"_bad_"+time.Now().Format("20060102150405"), raw)
		return &TodoData{Version: todoDataVersion}
	}
	if d.Tasks == nil {
		d.Tasks = []*TaskItem{}
	}
	if d.Records == nil {
		d.Records = []*RunRecord{}
	}
	if d.Version == 0 {
		d.Version = todoDataVersion
	}
	s.migrateData(&d)
	return &d
}

// migrateData 数据版本升级（落盘时机：load 之后由调用方统一持久化）。
// v1 → v2：任务多了「任务模型」三档字段。老单据读出来 ModelMode 为空——正好是
// 默认档「跟随派发设置」，Model 为空，不需要逐条改写（只是把版本号推上去，并把
// 「存了指向却没写档位」这种半残数据补成 custom）。
func (s *todoScheduler) migrateData(d *TodoData) {
	if d.Version >= todoDataVersion {
		return
	}
	for _, t := range d.Tasks {
		if t.ModelMode == todoModelFollowDispatch && t.Model != nil && t.Model.Complete() {
			t.ModelMode = todoModelCustom
		}
		if t.ModelMode != todoModelCustom {
			t.Model = nil
		} else if t.Model != nil {
			m := t.Model.norm()
			t.Model = &m
		}
	}
	d.Version = todoDataVersion
	s.persist(d)
}

// seedTodoData 首启种子：只有一条示例任务（不定时，只记着不自动执行）。
func seedTodoData() *TodoData {
	ts := time.Now().UnixMilli()
	return &TodoData{
		Version: todoDataVersion,
		Tasks: []*TaskItem{{
			ID:    "task-sample-dispatch",
			Title: "示例任务：点「立即执行」看看它怎么转交给数字助理",
			Description: strings.Join([]string{
				"这是一条示例待办，本身没有任何实际工作要做，只用来演示效果：",
				"",
				"- 它是不定时任务，不会自己触发；",
				"- 点卡片上的「立即执行」，任务描述会原样转交给长期助理，长期助理那边像收到一条新消息一样开始处理；",
				"- 想看定时/循环定时的效果，可以编辑它把执行方式改成「每天 21:00」之类，或者直接新建一条。",
				"",
				"（给数字助理的说明：收到本条时不要执行任何实际操作、不要读写文件、不要派人干活，只回一句「示例任务已收到，转交链路正常」即可。看完可以删掉这条示例。）",
			}, "\n"),
			Enabled:   true,
			Executor:  execAssistant,
			Schedule:  Schedule{Kind: schedManual},
			CreatedAt: ts,
			UpdatedAt: ts,
		}},
		Records: []*RunRecord{},
	}
}

// persist 把文档写回存储（调用方负责加锁或持有独占副本）。
func (s *todoScheduler) persist(d *TodoData) {
	if len(d.Records) > todoMaxRecords {
		d.Records = d.Records[len(d.Records)-todoMaxRecords:]
	}
	b, err := json.Marshal(d)
	if err != nil {
		log.Printf("[待办] 序列化失败: %v", err)
		return
	}
	if err := s.app.Store.SetGlobalData(todoDataKey, string(b)); err != nil {
		log.Printf("[待办] 保存失败: %v", err)
	}
}

// save 落盘当前内存副本（调用方需持有 s.mu）。
func (s *todoScheduler) save() { s.persist(s.data) }

// loop 是调度循环：每 todoTickMs 一拍；组件被关掉时整拍跳过（开关即时生效，
// 不需要重启服务）。
func (s *todoScheduler) loop() {
	ticker := time.NewTicker(time.Duration(todoTickMs) * time.Millisecond)
	defer ticker.Stop()
	for range ticker.C {
		s.tick()
	}
}

// tick 一拍：找出每条启用任务「刚到的槽点」，至多触发一次。
func (s *todoScheduler) tick() {
	if !s.app.todoEnabled() {
		return
	}
	now := time.Now().UnixMilli()
	s.mu.Lock()
	pending := []*TaskItem{}
	for _, t := range s.data.Tasks {
		if !t.Enabled || t.Done || s.busy[t.ID] {
			continue
		}
		if _, ok := computeDueSlot(t.Schedule, now, t.LastRunAt, t.CreatedAt); ok {
			pending = append(pending, t)
		}
	}
	s.mu.Unlock()
	for _, t := range pending {
		// 再算一次：上面的判断与这里的执行之间可能有别的 tick 落过基准。
		slot, ok := computeDueSlot(t.Schedule, time.Now().UnixMilli(), s.taskLastRunAt(t.ID), t.CreatedAt)
		if !ok {
			continue
		}
		if !s.markBusy(t.ID) {
			continue
		}
		go func(task *TaskItem, slot int64) {
			defer s.clearBusy(task.ID)
			if _, err := s.dispatch(task, "schedule", slot); err != nil {
				log.Printf("[待办] 任务 %s「%s」定时执行失败: %v", task.ID, task.Title, err)
			}
		}(t, slot)
	}
}

// markBusy 登记在途执行；已在途返回 false。
func (s *todoScheduler) markBusy(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.busy[id] {
		return false
	}
	s.busy[id] = true
	return true
}

func (s *todoScheduler) clearBusy(id string) {
	s.mu.Lock()
	delete(s.busy, id)
	s.mu.Unlock()
}

// taskLastRunAt 读取某任务当前的调度基准。
func (s *todoScheduler) taskLastRunAt(id string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, t := range s.data.Tasks {
		if t.ID == id {
			return t.LastRunAt
		}
	}
	return 0
}

// collectMissed 汇总全部任务的已错过槽点（每条任务至多一条：最近一个）。
func (s *todoScheduler) collectMissed() []MissedItem {
	now := time.Now().UnixMilli()
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []MissedItem{}
	for _, t := range s.data.Tasks {
		if !t.Enabled || t.Done {
			continue
		}
		if slot, ok := detectMissed(t.Schedule, now, t.LastRunAt, t.CreatedAt); ok {
			out = append(out, MissedItem{TaskID: t.ID, SlotTime: slot})
		}
	}
	return out
}

// snapshot 组装前端/工具用的完整视图。
func (s *todoScheduler) snapshot() map[string]any {
	now := time.Now().UnixMilli()
	s.mu.Lock()
	views := make([]TaskView, 0, len(s.data.Tasks))
	for _, t := range s.data.Tasks {
		views = append(views, s.toView(t, now))
	}
	records := make([]*RunRecord, len(s.data.Records))
	copy(records, s.data.Records)
	s.mu.Unlock()

	// 按下次执行时间排序：无下次（停用/不定时）的排后面；同类按更新时间倒序。
	rank := func(v TaskView) int64 {
		if v.Enabled && v.NextRunAt > 0 {
			return v.NextRunAt
		}
		if v.Enabled && v.Schedule.Kind != schedManual {
			return 1<<62 - 1
		}
		return 1 << 62
	}
	sort.SliceStable(views, func(i, j int) bool {
		ri, rj := rank(views[i]), rank(views[j])
		if ri != rj {
			return ri < rj
		}
		return views[i].UpdatedAt > views[j].UpdatedAt
	})
	// 记录按时间倒序，最多 200 条。
	sort.SliceStable(records, func(i, j int) bool { return records[i].StartedAt > records[j].StartedAt })
	if len(records) > todoMaxRecords {
		records = records[:todoMaxRecords]
	}
	return map[string]any{
		"tasks":   views,
		"records": records,
		"missed":  s.collectMissed(),
		"store":   s.app.Store.dataPath(),
	}
}

// toView 给任务附上下次执行时间（停用、已完成或不自动执行的不附）与「任务模型」
// 三档取值链算出的当前生效值（AI 任务才有：卡片上要写清「生效：…」）。
func (s *todoScheduler) toView(t *TaskItem, now int64) TaskView {
	v := TaskView{TaskItem: *t}
	if t.Executor == execAssistant {
		mv := s.app.todoModelView(t.ModelMode, derefModel(t.Model))
		v.ModelView = &mv
	}
	if !t.Enabled || t.Done {
		return v
	}
	if next, ok := computeNextRunAt(t.Schedule, now, t.LastRunAt); ok {
		v.NextRunAt = next
	}
	return v
}

// derefModel 取指向的副本（nil＝没存）。
func derefModel(r *ModelRef) ModelRef {
	if r == nil {
		return ModelRef{}
	}
	return *r
}

// find 按 id 取任务（返回内存副本的指针；调用方需持有 s.mu 才能改）。
func (s *todoScheduler) find(id string) (*TaskItem, bool) {
	for _, t := range s.data.Tasks {
		if t.ID == id {
			return t, true
		}
	}
	return nil, false
}

// ---------- 增删改 ----------

// todoRequest 是新建/修改的请求体（HTTP 与模型工具共用）。
type todoRequest struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Executor    string    `json:"executor"`
	Target      string    `json:"target"`
	Schedule    *Schedule `json:"schedule"`
	Dir         string    `json:"dir"`
	Command     string    `json:"command"`
	Shell       string    `json:"shell"`
	Permission  string    `json:"permission"`
	// 任务模型（仅 AI 任务）：modelMode = dispatch（跟随派发设置，默认）| global（全局
	// 默认模型）| custom（指定渠道+模型，model 里给完整指向）。只传 modelMode 即可
	// 换档；custom 必须带 model。
	ModelMode string    `json:"modelMode,omitempty"`
	Model     *ModelRef `json:"model,omitempty"`
	Enabled     *bool     `json:"enabled"`
	Done        *bool     `json:"done"` // 标记完成 / 重新打开（仅 update：完成是终态归档，调度随之停）
}

// normalize 把请求体收敛成一份规范化配置；只填给出的字段（create 用全量）。
// 返回的 target 是 AI 任务的执行目标（manager/task）；程序任务一律空。
func (r todoRequest) normalize() (executor, dir, command, shell, perm, target string, err error) {
	executor = execAssistant
	if r.Executor == execCommand {
		executor = execCommand
	}
	shell = strings.ToLower(strings.TrimSpace(r.Shell))
	if shell != "cmd" {
		shell = "powershell"
	}
	perm = strings.TrimSpace(r.Permission)
	if perm != permFull {
		perm = permWorkspace
	}
	if executor == execCommand {
		command = strings.TrimSpace(r.Command)
		if command == "" {
			return "", "", "", "", "", "", fmt.Errorf("程序任务需要填写要运行的命令")
		}
		dir = strings.TrimSpace(r.Dir)
		if dir == "" {
			return "", "", "", "", "", "", fmt.Errorf("程序任务必须先选择工作目录（命令在那里运行）")
		}
		if info, serr := os.Stat(dir); serr != nil || !info.IsDir() {
			return "", "", "", "", "", "", fmt.Errorf("工作目录不存在或不是目录: %s", dir)
		}
		return executor, dir, command, shell, perm, "", nil
	}
	// AI 任务的执行目标：空与未知值一律回落长期助理（历史数据没有该字段，
	// 读出来就是空——全部按既有行为走，不需要数据迁移）。
	target = strings.ToLower(strings.TrimSpace(r.Target))
	if target != targetTask {
		target = targetManager
	}
	return executor, dir, command, shell, perm, target, nil
}

// normalizeModel 归一三档「任务模型」字段：程序任务一律清空（它不经过模型）；
// custom 必须带完整的渠道+模型。返回存进 TaskItem 的模态与指向。
func (r todoRequest) normalizeModel(executor string) (string, *ModelRef, error) {
	if executor != execAssistant {
		return todoModelFollowDispatch, nil, nil
	}
	mode := normTodoModelMode(r.ModelMode)
	if mode != todoModelCustom {
		return mode, nil, nil
	}
	if r.Model == nil || !r.Model.Complete() {
		return "", nil, fmt.Errorf("待办选了「指定渠道+模型」，但渠道或模型没选全（编辑弹窗里选一个）")
	}
	m := r.Model.norm()
	return mode, &m, nil
}

// create 新建一条任务。
func (s *todoScheduler) create(r todoRequest) (*TaskView, error) {
	title := strings.TrimSpace(r.Title)
	if title == "" {
		return nil, fmt.Errorf("任务标题不能为空")
	}
	description := strings.TrimSpace(r.Description)
	if description == "" {
		return nil, fmt.Errorf("任务描述不能为空（将来要原样派出去）")
	}
	if r.Schedule == nil {
		return nil, fmt.Errorf("缺少执行方式")
	}
	if err := validateSchedule(*r.Schedule); err != nil {
		return nil, err
	}
	executor, dir, command, shell, perm, target, err := r.normalize()
	if err != nil {
		return nil, err
	}
	modelMode, modelRef, err := r.normalizeModel(executor)
	if err != nil {
		return nil, err
	}
	ts := time.Now().UnixMilli()
	t := &TaskItem{
		ID: "task-" + newID()[:8], Title: title, Description: description,
		Enabled:  r.Enabled == nil || *r.Enabled,
		Executor: executor, Schedule: *r.Schedule,
		Dir: dir, Command: command, Shell: shell, Permission: perm,
		ModelMode: modelMode, Model: modelRef,
		CreatedAt: ts, UpdatedAt: ts,
	}
	if executor == execAssistant {
		t.Dir, t.Command, t.Shell, t.Permission = "", "", "", ""
		t.Target = target
	}
	s.mu.Lock()
	s.data.Tasks = append(s.data.Tasks, t)
	s.save()
	view := s.toView(t, ts)
	s.mu.Unlock()
	log.Printf("[待办] 新建任务 %s「%s」（%s/%s）", t.ID, t.Title, t.Executor, t.Schedule.Kind)
	s.app.broadcast(Event{Type: "todo_tasks"})
	return &view, nil
}

// update 修改一条任务：只改给出的字段；换执行方式或类型时重置调度基准。
func (s *todoScheduler) update(r todoRequest) (*TaskView, error) {
	id := strings.TrimSpace(r.ID)
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.find(id)
	if !ok {
		return nil, fmt.Errorf("找不到任务 %s", id)
	}
	next := *t
	if r.Title != "" {
		title := strings.TrimSpace(r.Title)
		if title == "" {
			return nil, fmt.Errorf("任务标题不能为空")
		}
		next.Title = title
	}
	if r.Description != "" {
		next.Description = strings.TrimSpace(r.Description)
	}
	if r.Enabled != nil {
		next.Enabled = *r.Enabled
	}
	// 完成/重开：完成时刻只在这里落（用户/模型都只给布尔值，时间由系统记）。
	if r.Done != nil {
		next.Done = *r.Done
		if *r.Done {
			next.DoneAt = time.Now().UnixMilli()
		} else {
			next.DoneAt = 0
		}
	}
	resetBase := false
	if r.Schedule != nil {
		if err := validateSchedule(*r.Schedule); err != nil {
			return nil, err
		}
		next.Schedule = *r.Schedule
		resetBase = true
	}
	// 类型/命令/目录/档位/目标：任一项给出就按整套重新规范化（几类字段必须自洽）。
	if r.Executor != "" || r.Command != "" || r.Dir != "" || r.Permission != "" || r.Shell != "" || r.Target != "" {
		merged := todoRequest{
			Executor:   firstNonEmpty(r.Executor, next.Executor),
			Command:    firstNonEmpty(r.Command, next.Command),
			Dir:        firstNonEmpty(r.Dir, next.Dir),
			Shell:      firstNonEmpty(r.Shell, next.Shell),
			Permission: firstNonEmpty(r.Permission, next.Permission),
			Target:     firstNonEmpty(r.Target, next.Target),
		}
		executor, dir, command, shell, perm, target, err := merged.normalize()
		if err != nil {
			return nil, err
		}
		if executor != next.Executor {
			resetBase = true // 两种执行的「上次跑没跑过」语义不同
		}
		next.Executor, next.Dir, next.Command, next.Shell, next.Permission = executor, dir, command, shell, perm
		if executor == execAssistant {
			next.Dir, next.Command, next.Shell, next.Permission = "", "", "", ""
			next.Target = target
		} else {
			next.Target = "" // 程序任务没有执行目标一说
		}
	}
	if resetBase {
		next.LastRunAt = 0
	}
	// 任务模型（三档）：显式给了档位或指向就重算（"dispatch" 就是改回跟随派发设置）
	if r.ModelMode != "" || r.Model != nil {
		if next.Executor != execAssistant {
			return nil, fmt.Errorf("程序任务没有「任务模型」（它不经过模型）")
		}
		merged := todoRequest{ModelMode: firstNonEmpty(r.ModelMode, next.ModelMode)}
		if r.Model != nil {
			merged.Model = r.Model
		} else {
			merged.Model = next.Model
		}
		mode, ref, err := merged.normalizeModel(next.Executor)
		if err != nil {
			return nil, err
		}
		next.ModelMode, next.Model = mode, ref
	}
	next.UpdatedAt = time.Now().UnixMilli()
	*t = next
	s.save()
	view := s.toView(t, next.UpdatedAt)
	log.Printf("[待办] 更新任务 %s「%s」", t.ID, t.Title)
	s.app.broadcast(Event{Type: "todo_tasks"})
	return &view, nil
}

// firstNonEmpty 取第一个非空串。
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// delete 删除一条任务（执行记录保留，任务 id 仍在记录里可读）。
func (s *todoScheduler) delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.data.Tasks {
		if t.ID == id {
			s.tombs[id] = true
			s.data.Tasks = append(s.data.Tasks[:i], s.data.Tasks[i+1:]...)
			s.save()
			log.Printf("[待办] 删除任务 %s「%s」", t.ID, t.Title)
			s.app.broadcast(Event{Type: "todo_tasks"})
			return nil
		}
	}
	return fmt.Errorf("找不到任务 %s", id)
}

// resolveMissed 处理一条已错过的槽点：补跑一次，或标记忽略（推进基准）。
func (s *todoScheduler) resolveMissed(id, action string) error {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	t, ok := s.find(id)
	if !ok {
		s.mu.Unlock()
		return fmt.Errorf("找不到任务 %s", id)
	}
	task := *t // 拿副本去执行，执行期间不持有锁
	nowMs := time.Now().UnixMilli()
	if action == "dismiss" || nowMs > task.LastRunAt {
		// 忽略与补跑都先把基准推进到现在：补跑失败已有 failed 记录，不再连环提示。
		t.LastRunAt = nowMs
		s.save()
	}
	s.mu.Unlock()
	if action == "dismiss" {
		s.app.broadcast(Event{Type: "todo_tasks"})
		return nil
	}
	_, err := s.dispatch(&task, "catch-up", nowMs)
	return err
}

// runNow 立即执行一次（不等调度）。
func (s *todoScheduler) runNow(id string) (string, error) {
	id = strings.TrimSpace(id)
	s.mu.Lock()
	t, ok := s.find(id)
	if !ok {
		s.mu.Unlock()
		return "", fmt.Errorf("找不到任务 %s", id)
	}
	task := *t
	s.mu.Unlock()
	if !s.markBusy(id) {
		return "", fmt.Errorf("这条任务正在执行中，稍候再试")
	}
	defer s.clearBusy(id)
	return s.dispatch(&task, "manual", 0)
}

// ---------- 执行 ----------

// dispatch 执行一次任务：先落基准与「running」记录（防重复触发的关键一步），
// 再按任务类型分流。返回关联的会话 id（assistant=数字助理会话；command 无）。
func (s *todoScheduler) dispatch(task *TaskItem, trigger string, slot int64) (string, error) {
	nowMs := time.Now().UnixMilli()
	record := &RunRecord{
		ID: "run-" + newID()[:8], TaskID: task.ID, Trigger: trigger,
		StartedAt: nowMs, Status: "running",
	}
	s.mu.Lock()
	if t, ok := s.find(task.ID); ok {
		if trigger == "schedule" && slot > 0 {
			t.LastRunAt = slot
		} else {
			t.LastRunAt = nowMs
		}
		t.LastStatus = "running"
		t.LastError = ""
	}
	s.data.Records = append(s.data.Records, record)
	if len(s.data.Records) > todoMaxRecords {
		s.data.Records = s.data.Records[len(s.data.Records)-todoMaxRecords:]
	}
	s.save()
	s.mu.Unlock()

	var sessionID string
	var err error
	switch {
	case task.Executor == execCommand:
		err = s.runCommand(task, record)
	case task.Target == targetTask:
		sessionID, err = s.handToTaskSession(task, record)
	default:
		sessionID, err = s.handToAssistant(task, record)
	}

	s.mu.Lock()
	if err != nil {
		s.patchRecord(record.ID, map[string]any{"status": "failed", "error": err.Error()})
		if t, ok := s.find(task.ID); ok {
			t.LastStatus = "failed"
			t.LastError = err.Error()
		}
	} else if t, ok := s.find(task.ID); ok {
		t.LastStatus = "ok"
		if sessionID != "" {
			t.LastSessionID = sessionID
		}
	}
	s.save()
	s.mu.Unlock()
	s.app.broadcast(Event{Type: "todo_tasks"})
	if err != nil {
		return sessionID, err
	}
	log.Printf("[待办] 任务 %s「%s」已执行（%s，%s）", task.ID, task.Title, task.Executor, triggerLabel(trigger))
	return sessionID, nil
}

// patchRecord 就地更新一条执行记录（调用方需持有 s.mu）。
func (s *todoScheduler) patchRecord(id string, patch map[string]any) {
	for _, r := range s.data.Records {
		if r.ID != id {
			continue
		}
		if v, ok := patch["status"].(string); ok {
			r.Status = v
		}
		if v, ok := patch["summary"].(string); ok {
			r.Summary = v
		}
		if v, ok := patch["error"].(string); ok {
			r.Error = v
		}
		if v, ok := patch["sessionId"].(string); ok {
			r.SessionID = v
		}
		return
	}
}

// handToAssistant 是 AI 任务的执行体：把描述作为一条用户消息投给数字助理主对话。
// 派发/汇报是数字助理的职责，这里落「已转交」即完成。
func (s *todoScheduler) handToAssistant(task *TaskItem, record *RunRecord) (string, error) {
	sess, err := s.app.assistantSession()
	if err != nil {
		return "", err
	}
	text := fmt.Sprintf("【待办任务派发｜%s】\n任务：%s\n\n%s", triggerLabel(record.Trigger), task.Title, task.Description)
	// 待办转交与数字助理派发同一性质：AI 发出的用户消息（kind=aiuser）——
	// 走与用户发消息同一套投递（deliverToSession）：主对话空闲就落库并起一轮，
	// 正忙就插话注入它正在执行的那一轮（下一次调用模型就能看到——AI 发话
	// 一律插话，不排队）。按用户消息同等对待：开新回合、后面跟时间提示词，
	// 前端右对齐偏白气泡。
	d := s.app.deliverToSession(sess, text, kindAiUser, "todo")
	if d.Started {
		run := d.Run
		go func() {
			defer s.app.finishRunOf(sess.ID, run) // 按身份收口：硬终结后开的新一轮不会被迟到收口误摘
			// 转交即完成：任务记录不因这一轮的成败改写，失败只写日志（用户可在会话里看到原因）。
			err := s.app.runRoundsWithQueue(sess, run, func(ev Event) {
				ev.SessionID = sess.ID
				run.publish(ev)
			}, true, s.app.queueAutoSendOn())
			if err != nil {
				log.Printf("[待办] 任务 %s 转交后长期助理（会话 %s）本轮执行失败: %v", task.ID, sess.ID, err)
			} else {
				run.markCleanEnd() // 正常收尾：收口时兜底续发窗口期内新挤进队列的消息
			}
			s.app.broadcast(Event{Type: "session_updated", SessionID: sess.ID})
		}()
	}
	summary := fmt.Sprintf("已转交长期助理（会话 %s），由它安排执行并在完成后汇报结果。", sess.ID)
	s.mu.Lock()
	s.patchRecord(record.ID, map[string]any{"status": "ok", "sessionId": sess.ID, "summary": summary})
	s.mu.Unlock()
	return sess.ID, nil
}

// handToTaskSession 是 AI 任务「新的工作任务」目标的执行体：建一个独立的工作
// 任务会话，把描述作为首条任务消息交给它直接执行——不经长期助理转手，也不
// 打扰它。建会话必须走 createSession 单源（sessioncreate.go，与 dispatch_task
// 同一入口收敛，不另写建会话逻辑）：字段构造、权限默认、.jxai/tmp 预建与人工
// /派发建会话同一处。派生标记 dispatched_by 记待办调度器（todoComponentID，
// 系统/定时派生）：它不是会话 id，回报链解析（dispatchOwner）按它找派发方时
// 查无此会话，即如实「无回报对象」——长期助理不会被定时任务自动叫醒；执行
// 体与收尾广播用现有任务回报链同一套 runSubAgent（sub_end / session_updated /
// done、侧栏未读绿点照发），结果在工作任务会话与待办执行记录（sessionId 指向
// 新会话，页面「查看会话」直达）里看。
func (s *todoScheduler) handToTaskSession(task *TaskItem, record *RunRecord) (string, error) {
	title := strings.TrimSpace(task.Title)
	if title == "" {
		title = truncateRunes(task.Description, 20)
	}
	// 待办是系统调度器派出的（DispatchedBy=todo-scheduler），没有主会话可跟：
	// 模型按三档链路取（待办自选 → 派发设置 → 全局默认），任一环不可用就报错，
	// 不建会话、不留跑不起来的废会话。
	provider, providerKey, model, _, err := s.app.todoDispatchModel(task)
	if err != nil {
		return "", err
	}
	sub, err := s.app.createSession(createSessionParams{
		Mode:         "task", // 按任务性质默认通用工作任务（与 dispatch_task 不传 mode 同一默认）
		Title:        title,
		Workspace:    s.app.Cfg.Workspace, // 定时任务没有「当前会话」，工作目录取默认工作区（与长期助理主对话同口径）
		Permission:   permWorkspace,
		Provider:     provider, ProviderKey: providerKey, Model: model,
		DispatchedBy: todoComponentID, // 系统派生标记：回报链查无此会话，不自动回报、不打扰长期助理
	})
	if err != nil {
		return "", fmt.Errorf("创建工作任务会话失败: %w", err)
	}
	text := fmt.Sprintf("【待办任务派发｜%s】\n任务：%s\n\n%s", triggerLabel(record.Trigger), task.Title, task.Description)
	// 投递走统一收口 deliverToSession（与用户发消息/长期助理派发同路）：AI 发出的
	// 用户消息（kind=aiuser），空闲即落库并占住执行权，忙则插话（AI 发话一律
	// 插话）。新建会话必然空闲（Started 恒真），防御分支照抄不省略。
	d := s.app.deliverToSession(sub, text, kindAiUser, "todo")
	if d.Started {
		go s.app.runSubAgent(nil, sub, d.Run) // 执行体与 dispatch_task 同一个；派发方为空＝只执行不回报
	}
	summary := fmt.Sprintf("已新建工作任务会话 %s「%s」并投递执行；结果在该会话与执行记录里查看。", sub.ID, sub.Title)
	s.mu.Lock()
	s.patchRecord(record.ID, map[string]any{"status": "ok", "sessionId": sub.ID, "summary": summary})
	s.mu.Unlock()
	return sub.ID, nil
}

// assistantSession 取当前版本的数字助理主对话；没有版本就建第一版（与前端
// 「跟长期助理对话」入口、设置页版本清单同一套参数、同一个「当前版本」指针，
// 见 assistant_versions.go）。定时任务转交与任务求助都认这一版——用户切到哪一版，
// 任务就找哪一版，不会各自找各自「最近更新的那个」。
func (a *App) assistantSession() (*SessionMeta, error) {
	if m := a.currentAssistant(); m != nil {
		return m, nil
	}
	v, err := a.createAssistantVersion()
	if err != nil {
		return nil, fmt.Errorf("创建数字助理主对话失败: %w", err)
	}
	m, err := a.Store.Get(v.SessionID)
	if err != nil {
		return nil, fmt.Errorf("创建数字助理主对话失败: %w", err)
	}
	log.Printf("[会话] 已自动创建数字助理主对话 %s（第 %d 版，待办转交 / 任务求助按需找它）", m.ID, v.Version)
	return m, nil
}

// runCommand 是程序任务的执行体：在指定工作目录直接运行命令（不经过模型）。
// 默认进 OS 级沙箱（修改工作区，写边界就是这个目录）；任务上显式选了
// 「完全访问」才不受限。退出码与输出节选进执行记录。
func (s *todoScheduler) runCommand(task *TaskItem, record *RunRecord) error {
	dir := task.Dir
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return fmt.Errorf("工作目录不存在或不是目录：%s（编辑任务换一个）", dir)
	}
	if !s.app.Cfg.AllowShell {
		return fmt.Errorf("shell 执行已在配置中禁用（allowShell=false），程序任务无法运行")
	}
	run := sandboxRun{
		Dir: dir, Command: task.Command, Shell: task.Shell,
		Timeout: todoCmdTimeout, Policy: permWorkspace, Workspace: dir,
	}
	ctx := context.Background()
	var out string
	var err error
	if task.Permission == permFull {
		out, err = runUnconfinedLocal(ctx, run)
	} else {
		out, err = spawnConfinedLocal(ctx, run)
	}
	if err != nil {
		return err
	}
	body := fmt.Sprintf("$ %s\n[工作目录] %s\n[权限] %s\n%s", task.Command, dir, permModeLabel(task.Permission), truncateRunes(out, 800))
	s.mu.Lock()
	s.patchRecord(record.ID, map[string]any{"status": "ok", "summary": body})
	s.mu.Unlock()
	return nil
}

// listWorkspaces 列出可选的工作目录（程序任务选工作目录用）：默认工作区 +
// 历史会话用过的目录，去重后按路径排序。
func (a *App) listWorkspaces() []map[string]string {
	seen := map[string]bool{}
	out := []map[string]string{}
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || seen[p] {
			return
		}
		seen[p] = true
		out = append(out, map[string]string{"path": p, "title": filepath.Base(p)})
	}
	add(a.Cfg.Workspace)
	if sessions, err := a.Store.List(); err == nil {
		for _, m := range sessions {
			add(m.Workspace)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["path"] < out[j]["path"] })
	return out
}

// ---------- HTTP 处理器 ----------

// todoGuard 组件未启用时本功能的接口一律 404（与组件资产同一语义）。
func (a *App) todoGuard(w http.ResponseWriter) bool {
	if a.Todos == nil {
		writeErr(w, 503, "待办调度尚未就绪")
		return false
	}
	if !a.todoEnabled() {
		writeErr(w, 404, "「待办与定时任务」组件未启用，请到 设置 → 内置组件 打开")
		return false
	}
	return true
}

func (a *App) handleTodoList(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	writeJSON(w, 200, a.Todos.snapshot())
}

func (a *App) handleTodoCreate(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	var body todoRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return
	}
	task, err := a.Todos.create(body)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "task": task})
}

func (a *App) handleTodoUpdate(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	var body todoRequest
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return
	}
	task, err := a.Todos.update(body)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "task": task})
}

func (a *App) handleTodoDelete(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return
	}
	if err := a.Todos.delete(strings.TrimSpace(body.ID)); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1})
}

func (a *App) handleTodoRun(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return
	}
	sessionID, err := a.Todos.runNow(strings.TrimSpace(body.ID))
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	res := map[string]any{"ok": true}
	if sessionID != "" {
		res["sessionId"] = sessionID
	}
	writeJSON(w, 200, res)
}

func (a *App) handleTodoMissed(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	var body struct {
		ID     string `json:"id"`
		Action string `json:"action"` // run | dismiss
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return
	}
	if err := a.Todos.resolveMissed(strings.TrimSpace(body.ID), body.Action); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1})
}

func (a *App) handleTodoWorkspaces(w http.ResponseWriter, r *http.Request) {
	if !a.todoGuard(w) {
		return
	}
	writeJSON(w, 200, map[string]any{"items": a.listWorkspaces()})
}
