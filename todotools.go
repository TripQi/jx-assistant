package main

// 面向模型的待办/定时任务工具（todo_task_* / todo_workspace_list）。
//
// 工具集：查看（list/get）、增删改（create/update/delete）、立即执行（run）、
// 工作目录枚举（workspace_list）。schedule 是明确的结构化参数（六种形态），
// 不收自由字符串；返回值一律含任务 id 与人类可读确认，便于多轮操作。
//
// 审批纪律：delete 与 run 走 perm.go 的审批链（弹官方审批卡），用户批准才执行；
// 拿不到用户决定（超时/取消）一律视为拒绝，绝不静默放行。

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// todoTools 返回全部待办工具（组件未启用时返回空，调用方据此挂载）。
func (a *App) todoTools() []Tool {
	if !a.todoEnabled() {
		return nil
	}
	return []Tool{
		todoTaskListTool(), todoTaskGetTool(), todoTaskCreateTool(),
		todoTaskUpdateTool(), todoTaskDeleteTool(), todoTaskRunTool(), todoWorkspaceListTool(),
	}
}

// scheduleParam 是 schedule 参数的统一 schema 片段（create 与 update 共用）。
func scheduleParam() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "执行方式。kind 决定其余字段：manual 不需要别的；once 要 at；daily/weekdays 要 time；weekly 要 time 与 days；interval 要 every_minutes。",
		"properties": map[string]any{
			"kind": map[string]any{
				"type":        "string",
				"enum":        []string{schedManual, schedOnce, schedDaily, schedWeekly, schedWeekdays, schedInterval},
				"description": "执行方式：manual=不定时（只记录，不自动执行）；once=定时一次；daily=每天；weekly=每周某几天；weekdays=仅工作日每天；interval=每隔 N 分钟。",
			},
			"at":            strProp("仅 kind=once：执行时刻，格式 “YYYY-MM-DD HH:mm”（本地时区）。"),
			"time":          strProp("仅 kind=daily/weekly/weekdays：当天执行时刻，格式 “HH:mm”（如 “21:00”）。"),
			"days":          map[string]any{"type": "array", "items": map[string]any{"type": "number"}, "description": "仅 kind=weekly：星期几的数组，0=周日、1=周一 … 6=周六（至少一个）。"},
			"every_minutes": intProp("仅 kind=interval：间隔分钟数（1–43200 的整数）。"),
		},
		"required": []string{"kind"},
	}
}

// scheduleArgs 是模型传来的 schedule 参数（宽松形状，逐字段校验）。
type scheduleArgs struct {
	Kind         string `json:"kind"`
	At           string `json:"at"`
	Time         string `json:"time"`
	Days         []int  `json:"days"`
	EveryMinutes int    `json:"every_minutes"`
}

// toSchedule 把工具参数收敛成 Schedule；不合法返回人话错误。
func (p scheduleArgs) toSchedule() (Schedule, error) {
	switch p.Kind {
	case schedManual:
		return Schedule{Kind: schedManual}, nil
	case schedOnce:
		at, ok := parseLocalDateTime(p.At)
		if !ok {
			return Schedule{}, fmt.Errorf("定时一次需要 at（“YYYY-MM-DD HH:mm”，本地时区），收到 %q", p.At)
		}
		return Schedule{Kind: schedOnce, At: at}, nil
	case schedDaily, schedWeekdays:
		if _, ok := parseTimeToMinutes(p.Time); !ok {
			return Schedule{}, fmt.Errorf("每天/仅工作日需要 time（“HH:mm”），收到 %q", p.Time)
		}
		return Schedule{Kind: p.Kind, Time: strings.TrimSpace(p.Time)}, nil
	case schedWeekly:
		if _, ok := parseTimeToMinutes(p.Time); !ok {
			return Schedule{}, fmt.Errorf("每周执行需要 time（“HH:mm”），收到 %q", p.Time)
		}
		if len(p.Days) == 0 {
			return Schedule{}, fmt.Errorf("每周执行需要 days（非空数组，0=周日 … 6=周六）")
		}
		s := Schedule{Kind: schedWeekly, Time: strings.TrimSpace(p.Time), Days: append([]int{}, p.Days...)}
		if err := validateSchedule(s); err != nil {
			return Schedule{}, err
		}
		return s, nil
	case schedInterval:
		s := Schedule{Kind: schedInterval, EveryMinutes: p.EveryMinutes}
		if err := validateSchedule(s); err != nil {
			return Schedule{}, err
		}
		return s, nil
	}
	return Schedule{}, fmt.Errorf("schedule.kind 必须是 manual / once / daily / weekly / weekdays / interval 之一")
}

// parseLocalDateTime 把 “YYYY-MM-DD HH:mm”（本地时区）解析为 epoch 毫秒；
// 也接受可被 time.Parse 的常见形式做兜底。
func parseLocalDateTime(text string) (int64, bool) {
	text = strings.TrimSpace(text)
	for _, layout := range []string{"2006-01-02 15:04", "2006-01-02T15:04", "2006-01-02 15:04:05", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.ParseInLocation(layout, text, time.Local); err == nil {
			return t.UnixMilli(), true
		}
	}
	return 0, false
}

// taskLine 是一条任务的清单行（列表工具用）。
func taskLine(v TaskView) string {
	next := ""
	if v.NextRunAt > 0 {
		next = "｜下次 " + formatMillis(v.NextRunAt)
	}
	status := ""
	switch v.LastStatus {
	case "ok":
		status = "｜上次 成功"
	case "failed":
		status = "｜上次 失败"
	case "running":
		status = "｜上次 运行中"
	}
	enabled := "启用"
	if !v.Enabled {
		enabled = "停用"
	}
	if v.Done {
		enabled = "已完成（调度已停）"
	}
	return fmt.Sprintf("- %s｜id=%s｜%s｜%s｜%s%s%s", v.Title, v.ID, describeSchedule(v.Schedule), describeExecutor(&v.TaskItem), enabled, next, status)
}

// taskDetail 是单条任务的详情文本。
func taskDetail(v TaskView, records []*RunRecord) string {
	mine := []*RunRecord{}
	for _, r := range records {
		if r.TaskID == v.ID && len(mine) < 5 {
			mine = append(mine, r)
		}
	}
	last := "无"
	if v.LastStatus != "" {
		last = map[string]string{"ok": "成功", "failed": "失败", "running": "运行中"}[v.LastStatus]
	}
	status := map[bool]string{true: "启用", false: "停用"}[v.Enabled] + "｜上次执行 " + last
	if v.Done {
		status += "｜已完成（" + formatMillis(v.DoneAt) + " 标记，调度已停）"
	}
	lines := []string{
		"任务：" + v.Title,
		"id：" + v.ID,
		"执行方式：" + describeSchedule(v.Schedule),
		"类型：" + describeExecutor(&v.TaskItem),
		"执行目标：" + describeTarget(&v.TaskItem) + "（程序任务无此项）",
		"状态：" + status,
		"下次执行：" + formatMillis(v.NextRunAt),
	}
	if v.Executor == execAssistant {
		lines = append(lines, "思考档位（待办覆盖）："+firstNonEmpty(v.Thinking, "不指定"))
		if v.ModelView != nil {
			lines = append(lines, "模型（当前生效）："+v.ModelView.Text, "思考（当前生效）："+v.ModelView.ThinkingText)
		}
	}
	if v.LastError != "" {
		lines = append(lines, "上次错误："+v.LastError)
	}
	lines = append(lines, "", "描述：", v.Description, "")
	if len(mine) == 0 {
		lines = append(lines, "（还没有执行记录）")
	} else {
		lines = append(lines, "最近执行记录（最多 5 条）：")
		for _, r := range mine {
			st := map[string]string{"ok": "成功", "failed": "失败", "running": "运行中"}[r.Status]
			extra := ""
			if r.Summary != "" {
				extra = "｜" + truncateRunes(r.Summary, 200)
			}
			if r.Error != "" {
				extra += "｜" + truncateRunes(r.Error, 200)
			}
			lines = append(lines, fmt.Sprintf("- %s｜%s｜%s%s", formatMillis(r.StartedAt), triggerLabel(r.Trigger), st, extra))
		}
	}
	return strings.Join(lines, "\n")
}

// recentRecordsText 是列表工具附带的最近执行记录摘要。
func recentRecordsText(views []TaskView, records []*RunRecord) string {
	if len(records) == 0 {
		return ""
	}
	titles := map[string]string{}
	for _, v := range views {
		titles[v.ID] = v.Title
	}
	lines := []string{}
	for i, r := range records {
		if i >= 5 {
			break
		}
		name := titles[r.TaskID]
		if name == "" {
			name = r.TaskID
		}
		st := map[string]string{"ok": "成功", "failed": "失败", "running": "运行中"}[r.Status]
		lines = append(lines, fmt.Sprintf("- %s｜%s｜%s", formatMillis(r.StartedAt), name, st))
	}
	return "\n\n最近执行记录（最多 5 条）：\n" + strings.Join(lines, "\n")
}

// todoSnapshotParts 从调度器取一份视图与记录（列表/详情工具共用）。
func (a *App) todoSnapshotParts() ([]TaskView, []*RunRecord, error) {
	if a.Todos == nil || !a.todoEnabled() {
		return nil, nil, fmt.Errorf("「待办与定时任务」组件未启用（设置 → 内置组件 里打开）")
	}
	snap := a.Todos.snapshot()
	views, _ := snap["tasks"].([]TaskView)
	records, _ := snap["records"].([]*RunRecord)
	return views, records, nil
}

// findTodoView 按 id 在视图里找一条任务。
func findTodoView(views []TaskView, id string) (TaskView, bool) {
	for _, v := range views {
		if v.ID == id {
			return v, true
		}
	}
	return TaskView{}, false
}

// ---------- 工具定义 ----------

func todoTaskListTool() Tool {
	return Tool{
		Spec: toolSpec("todo_task_list",
			"列出用户的全部待办事项与定时任务：每条含 id、标题、执行方式（不定时/定时一次/每天/每周/工作日/间隔）、类型（AI 任务/程序任务）、启用状态、下次执行时间与上次执行结果，另附最近 5 条执行记录。用户提到“待办”“任务清单”“定时任务”时先用这个看全貌；后续增删改都要用这里返回的 id。",
			objSchema(map[string]any{})),
		Run: runTodoTaskList,
	}
}

func runTodoTaskList(t *toolContext, _ json.RawMessage) (string, error) {
	views, records, err := t.app.todoSnapshotParts()
	if err != nil {
		return "", err
	}
	if len(views) == 0 {
		return "（待办清单是空的）", nil
	}
	lines := []string{}
	for _, v := range views {
		lines = append(lines, taskLine(v))
	}
	return fmt.Sprintf("共 %d 条任务：\n%s%s", len(views), strings.Join(lines, "\n"), recentRecordsText(views, records)), nil
}

func todoTaskGetTool() Tool {
	return Tool{
		Spec: toolSpec("todo_task_get",
			"按 id 查看一条待办/定时任务的完整详情：描述原文、执行方式、类型、启用状态、下次执行时间与最近执行记录。",
			objSchema(map[string]any{
				"task_id": strProp("任务 id（来自 todo_task_list）。"),
			}, "task_id")),
		Run: runTodoTaskGet,
	}
}

func runTodoTaskGet(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.TaskID)
	if id == "" {
		return "需要 task_id（来自 todo_task_list）。", nil
	}
	views, records, err := t.app.todoSnapshotParts()
	if err != nil {
		return "", err
	}
	v, ok := findTodoView(views, id)
	if !ok {
		return fmt.Sprintf("找不到任务 %q；用 todo_task_list 看现有 id。", id), nil
	}
	return taskDetail(v, records), nil
}

// todoThinkingParam 与保存校验共用系统档位，不为待办维护第二套清单。
func todoThinkingParam() map[string]any {
	return map[string]any{"type": "string", "enum": append([]string{""}, thinkingLevels()...), "description": "仅 AI 任务：独立思考档位；空串=不指定（更新时清除覆盖），省略=保留。"}
}

func todoModelModeParam() map[string]any {
	return map[string]any{"type": "string", "enum": []string{"dispatch", "global", "custom"}, "description": "仅 AI 任务：dispatch 跟随派发设置、global 全局默认、custom 指定 model。"}
}

func todoModelParam() map[string]any {
	return objSchema(map[string]any{
		"provider": strProp("渠道 id。"), "providerKey": strProp("模型归属 Key id（可省略）。"), "model": strProp("模型 id。"),
	}, "provider", "model")
}

func todoTaskCreateTool() Tool {
	return Tool{
		Spec: toolSpec("todo_task_create",
			"新建一条待办事项或定时任务。任务分两类：executor=\"assistant\"（AI 任务，默认：到点把描述转交长期助理执行，用户说“记个待办/提醒我几点做某事”用这类；可用 target 指定执行目标：manager=长期助理（默认）/ task=新的工作任务（到点自动创建工作任务会话直接执行））与 executor=\"command\"（程序任务：到点在指定工作目录直接运行命令，需同时给 command 与 dir）。描述要写成自足的一段话（将来会原样派出去执行）。",
			objSchema(map[string]any{
				"title":       strProp("任务标题（简短一行）。"),
				"description": strProp("任务描述：将来原样派发执行的完整需求，自足、不依赖聊天上下文。"),
				"schedule":    scheduleParam(),
				"executor": map[string]any{
					"type":        "string",
					"enum":        []string{execAssistant, execCommand},
					"description": "任务类型：assistant=AI 任务（默认，转交长期助理）；command=程序任务（到点运行命令）。",
				},
				"target": map[string]any{
					"type":        "string",
					"enum":        []string{targetManager, targetTask},
					"description": "仅 AI 任务：执行目标——manager=长期助理（默认，转交数字助理主对话处理）；task=新的工作任务（到点自动创建工作任务会话直接执行，不打扰长期助理）。与界面「执行目标」选框同一取值。",
				},
				"thinking":   todoThinkingParam(),
				"model_mode": todoModelModeParam(),
				"model":      todoModelParam(),
				"command":    strProp("仅程序任务：要运行的命令行（如 “git pull”）。"),
				"dir":        strProp("仅程序任务：工作目录的绝对路径（先用 todo_workspace_list 查可选目录）。"),
				"shell":      strProp("仅程序任务：powershell（默认）或 cmd。"),
				"permission": strProp("仅程序任务：workspace-write（默认，命令在沙箱内只能写工作目录）或 full（不受限，谨慎使用）。"),
				"enabled":    boolProp("是否启用，默认 true。"),
			}, "title", "description", "schedule")),
		Run: runTodoTaskCreate,
	}
}

func runTodoTaskCreate(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Title       string       `json:"title"`
		Description string       `json:"description"`
		Schedule    scheduleArgs `json:"schedule"`
		Executor    string       `json:"executor"`
		Target      string       `json:"target"`
		ModelMode   string       `json:"model_mode"`
		Model       *ModelRef    `json:"model"`
		Thinking    *string      `json:"thinking"`
		Command     string       `json:"command"`
		Dir         string       `json:"dir"`
		Shell       string       `json:"shell"`
		Permission  string       `json:"permission"`
		Enabled     *bool        `json:"enabled"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if t.app.Todos == nil || !t.app.todoEnabled() {
		return "「待办与定时任务」组件未启用（设置 → 内置组件 里打开）。", nil
	}
	sched, err := p.Schedule.toSchedule()
	if err != nil {
		return "新建失败：" + err.Error(), nil
	}
	task, err := t.app.Todos.create(todoRequest{
		Title: p.Title, Description: p.Description, Schedule: &sched,
		Executor: p.Executor, Target: p.Target, Command: p.Command, Dir: p.Dir, Shell: p.Shell,
		Permission: p.Permission, Enabled: p.Enabled,
		ModelMode: p.ModelMode, Model: p.Model, Thinking: p.Thinking,
	})
	if err != nil {
		return "新建失败：" + err.Error(), nil
	}
	return fmt.Sprintf("已新建任务「%s」（id=%s）｜%s｜%s｜%s。页面「待办与定时任务」里立即可见；后续用这个 id 修改/删除/立即执行。",
		task.Title, task.ID, describeSchedule(task.Schedule), describeExecutor(&task.TaskItem),
		map[bool]string{true: "启用", false: "停用"}[task.Enabled]), nil
}

func todoTaskUpdateTool() Tool {
	return Tool{
		Spec: toolSpec("todo_task_update",
			"修改一条待办/定时任务：只传要改的字段（未传的保持不变）。可改标题、描述、执行方式（schedule 结构与新建相同）、任务类型（executor 与 command/dir/shell/permission 配套）、执行目标（target，仅 AI 任务：manager=长期助理 / task=新的工作任务）、启用状态、完成标记（done：true＝收进「已完成」页并停掉调度，false＝重新打开）。",
			objSchema(map[string]any{
				"task_id":     strProp("任务 id（来自 todo_task_list）。"),
				"title":       strProp("新的标题。"),
				"description": strProp("新的任务描述（整体替换）。"),
				"schedule":    scheduleParam(),
				"executor": map[string]any{
					"type": "string",
					"enum": []string{execAssistant, execCommand},
				},
				"target": map[string]any{
					"type":        "string",
					"enum":        []string{targetManager, targetTask},
					"description": "仅 AI 任务：新的执行目标——manager=长期助理；task=新的工作任务。",
				},
				"thinking":   todoThinkingParam(),
				"model_mode": todoModelModeParam(),
				"model":      todoModelParam(),
				"command":    strProp("仅程序任务：新的命令行。"),
				"dir":        strProp("仅程序任务：新的工作目录绝对路径。"),
				"shell":      strProp("仅程序任务：powershell 或 cmd。"),
				"permission": strProp("仅程序任务：workspace-write 或 full。"),
				"enabled":    boolProp("启用/停用。"),
				"done":       boolProp("标记完成 / 重新打开：true＝收进「已完成」页并停掉调度（终态归档，与停用的暂停不同）；false＝重新放回待执行。"),
			}, "task_id")),
		Run: runTodoTaskUpdate,
	}
}

func runTodoTaskUpdate(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		TaskID      string        `json:"task_id"`
		Title       string        `json:"title"`
		Description string        `json:"description"`
		Schedule    *scheduleArgs `json:"schedule"`
		Executor    string        `json:"executor"`
		Target      string        `json:"target"`
		ModelMode   string        `json:"model_mode"`
		Model       *ModelRef     `json:"model"`
		Thinking    *string       `json:"thinking"`
		Command     string        `json:"command"`
		Dir         string        `json:"dir"`
		Shell       string        `json:"shell"`
		Permission  string        `json:"permission"`
		Enabled     *bool         `json:"enabled"`
		Done        *bool         `json:"done"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.TaskID)
	if id == "" {
		return "需要 task_id（来自 todo_task_list）。", nil
	}
	if t.app.Todos == nil || !t.app.todoEnabled() {
		return "「待办与定时任务」组件未启用（设置 → 内置组件 里打开）。", nil
	}
	views, _, err := t.app.todoSnapshotParts()
	if err != nil {
		return "", err
	}
	if _, ok := findTodoView(views, id); !ok {
		return fmt.Sprintf("找不到任务 %q；用 todo_task_list 看现有 id。", id), nil
	}
	req := todoRequest{
		ID: id, Title: p.Title, Description: p.Description, Executor: p.Executor, Target: p.Target,
		Command: p.Command, Dir: p.Dir, Shell: p.Shell, Permission: p.Permission, Enabled: p.Enabled, Done: p.Done,
		ModelMode: p.ModelMode, Model: p.Model, Thinking: p.Thinking,
	}
	if p.Schedule != nil {
		sched, serr := p.Schedule.toSchedule()
		if serr != nil {
			return "更新失败：" + serr.Error(), nil
		}
		req.Schedule = &sched
	}
	task, err := t.app.Todos.update(req)
	if err != nil {
		return "更新失败：" + err.Error(), nil
	}
	return fmt.Sprintf("已更新任务「%s」（id=%s）｜%s｜%s｜%s。", task.Title, task.ID,
		describeSchedule(task.Schedule), describeExecutor(&task.TaskItem),
		map[bool]string{true: "启用", false: "停用"}[task.Enabled]), nil
}

func todoTaskDeleteTool() Tool {
	return Tool{
		Spec: toolSpec("todo_task_delete",
			"按 id 删除一条待办/定时任务（不可恢复；历史执行记录保留）。删除前会请求用户审批。",
			objSchema(map[string]any{
				"task_id": strProp("任务 id（来自 todo_task_list）。"),
			}, "task_id")),
		Run: runTodoTaskDelete,
	}
}

func runTodoTaskDelete(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.TaskID)
	if id == "" {
		return "需要 task_id（来自 todo_task_list）。", nil
	}
	views, _, err := t.app.todoSnapshotParts()
	if err != nil {
		return "", err
	}
	v, ok := findTodoView(views, id)
	if !ok {
		return fmt.Sprintf("找不到任务 %q；用 todo_task_list 看现有 id。", id), nil
	}
	approved, _, _ := t.app.requestApproval(t.ctx, t.sess, "todo_task_delete",
		fmt.Sprintf("删除待办任务「%s」（%s）。删除后不可恢复。", v.Title, describeSchedule(v.Schedule)),
		"模型请求删除一条待办/定时任务。执行记录会保留，任务本身不可恢复。")
	if !approved {
		return fmt.Sprintf("用户没有批准删除「%s」。任务保持原样。", v.Title), nil
	}
	if err := t.app.Todos.delete(id); err != nil {
		return "删除失败：" + err.Error(), nil
	}
	return fmt.Sprintf("已删除任务「%s」（id=%s）。", v.Title, id), nil
}

func todoTaskRunTool() Tool {
	return Tool{
		Spec: toolSpec("todo_task_run",
			"立即执行一条待办/定时任务（不等它的定时点）：AI 任务立刻转交它的执行目标（长期助理或新的工作任务），程序任务立刻运行命令。执行前会请求用户审批。想验证任务配置或用户说“现在就跑”时用。",
			objSchema(map[string]any{
				"task_id": strProp("任务 id（来自 todo_task_list）。"),
			}, "task_id")),
		Run: runTodoTaskRun,
	}
}

func runTodoTaskRun(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.TaskID)
	if id == "" {
		return "需要 task_id（来自 todo_task_list）。", nil
	}
	views, _, err := t.app.todoSnapshotParts()
	if err != nil {
		return "", err
	}
	v, ok := findTodoView(views, id)
	if !ok {
		return fmt.Sprintf("找不到任务 %q；用 todo_task_list 看现有 id。", id), nil
	}
	approved, _, _ := t.app.requestApproval(t.ctx, t.sess, "todo_task_run",
		fmt.Sprintf("立即执行待办任务「%s」（%s，%s）。", v.Title, describeSchedule(v.Schedule), describeExecutor(&v.TaskItem)),
		"模型请求立刻执行一条待办/定时任务，不等它的定时点。")
	if !approved {
		return fmt.Sprintf("用户没有批准立即执行「%s」。任务保持原样。", v.Title), nil
	}
	sessionID, err := t.app.Todos.runNow(id)
	if err != nil {
		return "执行失败：" + err.Error(), nil
	}
	if v.Executor == execCommand {
		return fmt.Sprintf("已执行任务「%s」（id=%s）：命令已运行，结果见执行记录（todo_task_get 可查）。", v.Title, id), nil
	}
	where := sessionID
	if where == "" {
		where = "未知"
	}
	return fmt.Sprintf("已执行任务「%s」（id=%s）：已转交数字助理（会话 %s）。", v.Title, id, where), nil
}

func todoWorkspaceListTool() Tool {
	return Tool{
		Spec: toolSpec("todo_workspace_list",
			"列出可选的工作目录（绝对路径）。新建/修改**程序任务**时要选工作目录（dir），先来这里查。",
			objSchema(map[string]any{})),
		Run: runTodoWorkspaceList,
	}
}

func runTodoWorkspaceList(t *toolContext, _ json.RawMessage) (string, error) {
	items := t.app.listWorkspaces()
	if len(items) == 0 {
		return "（还没有登记过工作目录；程序任务需要先在工作区里建立目录。）", nil
	}
	lines := []string{}
	for _, it := range items {
		lines = append(lines, fmt.Sprintf("- %s｜%s", it["path"], it["title"]))
	}
	return strings.Join(lines, "\n"), nil
}
