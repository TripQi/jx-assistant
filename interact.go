package main

// 模型与用户的交互工具（对齐官方标准栈）：
//
//   - ask_user_question：模型向用户提一个结构化问题（可带选项），同步等
//     用户回答后把答案返回给模型。对应官方 dsh-tool-ask-user。
//   - enter_plan_mode：模型判断任务值得先计划（改动面大/多文件/有风险），
//     征得用户同意后把会话切入计划模式。对应官方 EnterPlanMode——
//     切换会话模式要用户点头（fail-closed），同意即生效，拒绝维持原模式。
//   - exit_plan_mode：计划模式下，模型完成探索后把计划提交给用户批准；
//     批准即退出计划模式开始执行，拒绝则带着反馈留在计划模式。
//     对应官方 dsh-plan-mode 的 exit_plan_mode。
//   - update_goal：目标模式下汇报目标的最终状态（complete/blocked）。
//     常驻注册，目录跨模式不变——与 exit_plan_mode 同策略，保持请求形状/
//     缓存稳定；未开目标模式时调用直接报错。目标模式的自动续跑由
//     runtime.go 的 runGoalRounds 驱动：目标仍 active 时每轮结束自动注入
//     一条 goal_round 消息继续推进，直到 complete/blocked/上限。
//
// 两者共用同一条「等待用户输入」的通道：与审批（perm.go）同构——前端经
// 全局事件流收到 question_required 弹卡，回答经 POST /api/questions/{id}
// 回传；超时/取消返回"用户未回应"（fail-closed：不替用户做决定）。
//
// 两类卡的时间限制就是它们的关键差别：
//   - ask / enter-plan：等 5 分钟（questionTTL），超时按「用户未回应」返回；
//   - plan（计划审批）：**不设时限**——计划一旦提交就一直在那儿等用户审批，
//     关掉软件、重启进程之后再打开也还是等着（提交即落库，见 store.go 的
//     PendingPlan；重启后内存里的等待者没了，用户批准/修改时走
//     resolvePersistedPlan 那条「恢复执行」分支）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"
)

// questionTTL 是普通提问（ask）与「进入计划模式」确认的等待上限。
// 计划审批（kind=plan）刻意不适用：它不设时限，见 requestUserAnswer。
const questionTTL = 5 * time.Minute

// questionTTLLabel 把 questionTTL 换算成给模型看的中文时长（环境快照的
// 「提问等待」公告用）——改档只动 questionTTL 常量，公告随之更新。
func questionTTLLabel() string {
	if m := int(questionTTL.Minutes()); m >= 1 && questionTTL == time.Duration(m)*time.Minute {
		return fmt.Sprintf("%d 分钟", m)
	}
	return questionTTL.String()
}

// clampAskTimeoutSec 钳制提问等待秒数（ask_user_question 的 timeout_sec）：
// 太短用户来不及看（也来不及被声光提醒），太长一轮干等（用户停表入口本就
// 能转无限等待，必须确认的问题直接传 0 不限时，时长不必给大）。
func clampAskTimeoutSec(sec int) int {
	if sec < 30 {
		return 30
	}
	if sec > 600 {
		return 600
	}
	return sec
}

// 提问/审批的种类（UserQuestion.Kind）：前端按它分卡渲染，后端按它决定
// 等待时限与是否落库（计划审批无时限、且要跨进程重启存活）。
const (
	askKindAsk   = "ask"        // 普通提问
	askKindPlan  = "plan"       // 计划审批（无时限、提交即落库、重启后仍在等）
	askKindEnter = "enter-plan" // 进入计划模式的确认
)

// UserQuestion 是一次等待用户输入的请求（提问或计划批准）。
type UserQuestion struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	// SessionTitle 是发起提问的会话标题：提问卡可能弹在别的对话里（卡片顶部
	// 标「来自「某对话」」并可点击跳过去），标题随请求下发，前端不必再查。
	SessionTitle string   `json:"sessionTitle,omitempty"`
	Kind         string   `json:"kind"` // ask=普通提问 plan=计划批准
	Question     string   `json:"question"`
	Plan         string   `json:"plan,omitempty"` // kind=plan 时的计划正文
	Options      []string `json:"options,omitempty"`
	AllowFree    bool     `json:"allowFree"`
	// 倒计时（仅 kind=ask）：TimeoutSec 是本次等待秒数（0=默认 300）；
	// NoTimeout=true 表示模型显式要求不限时（timeout_sec=0，无推荐方案或必须
	// 确认的问题）——同样没有倒计时与截止时刻，一直等用户作答。
	// DeadlineMs 是截止的绝对毫秒时间戳（前端据此跳倒计时）；TimerHeld=true
	// 表示用户已停表（悬停/触碰卡片或点停止按钮）——无限等待直到作答。
	TimeoutSec int    `json:"timeoutSec,omitempty"`
	NoTimeout  bool   `json:"noTimeout,omitempty"`
	DeadlineMs int64  `json:"deadlineMs,omitempty"`
	TimerHeld  bool   `json:"timerHeld,omitempty"`
	CreatedAt  string `json:"createdAt"`
	expiresAt  time.Time
	decided    bool
	ch         chan string
	timer      *time.Timer // 停表入口（holdQuestion）用它掐掉超时；permMu 保护
	// 经理代理通知（mgrnotify.go）：notified=已通知过长期助理（决议据此回告）；
	// byManager=决议由经理经 call_api 代选（回告标注来源）。permMu 保护。
	notified  bool
	byManager bool
}

// requestUserAnswer 广播提问并阻塞等待用户回答；返回用户的回答文本，
// 超时/取消返回 ok=false。
func (a *App) requestUserAnswer(ctx context.Context, sess *SessionMeta, q *UserQuestion) (string, bool) {
	q.ID = "q-" + newID()
	q.SessionID = sess.ID
	q.SessionTitle = sess.Title
	q.CreatedAt = now()
	// 计划审批不设时限：提交那一刻就落库，等待一直有效。进程重启后内存里的
	// 等待者没了，但库里的计划还在——前端照样看到待审批卡，用户批准/修改走
	// resolvePersistedPlan（恢复执行）。其余提问按 timeout_sec（缺省 300 秒）
	// 限时，用户悬停/触碰卡片可停表（holdQuestion）转为无限等待。
	ttl := questionTTL
	if q.Kind == askKindPlan {
		ttl = 0
		if err := a.Store.SetPendingPlan(PendingPlan{
			ID: q.ID, SessionID: sess.ID, Plan: q.Plan, CreatedAt: q.CreatedAt,
		}); err != nil {
			log.Printf("[对话] 会话 %s 的待审批计划落库失败（重启后这张卡会丢）: %v", sess.ID, err)
		}
	} else if q.NoTimeout {
		ttl = 0 // 模型显式不限时（timeout_sec=0）：无推荐方案/必须确认的问题
	} else if q.TimeoutSec > 0 {
		ttl = time.Duration(q.TimeoutSec) * time.Second
	}
	if ttl > 0 {
		q.expiresAt = time.Now().Add(ttl)
		q.DeadlineMs = q.expiresAt.UnixMilli()
	}
	q.ch = make(chan string, 1)
	var timer *time.Timer
	if ttl > 0 {
		timer = time.NewTimer(ttl) // 先建表后登记：登记瞬间就可能有人停表
		q.timer = timer
	}
	a.permMu.Lock()
	a.questions[q.ID] = q
	a.permMu.Unlock()
	ev := Event{Type: "question_required", QuestionID: q.ID, SessionID: sess.ID, Title: sess.Title,
		Kind: q.Kind, Summary: q.Question, Plan: q.Plan, Options: q.Options, Reason: q.Question,
		DeadlineMs: q.DeadlineMs}
	a.broadcast(ev)
	// 声光提醒（设置·常规，默认开）：等待达到设定时长仍未答且未停表时，系统
	// 叮一声（MessageBeep，不走语音组件）+ 广播 ask_alert（前端全屏闪烁，并在
	// 浮窗上走完 15 秒倒计时后才朗读配置文案）。作答/超时/停表/停止都会被下面
	// 的 defer 掐掉；触发那一刻已停表的直接不响——停表=用户碰到过卡片，或已从
	// 任何入口打开对应的计划页/浮窗在看（在场），连提醒一起暂停。
	var alertTimer *time.Timer
	if a.Cfg.askAlertOn() {
		alertTimer = time.AfterFunc(time.Duration(a.Cfg.askAlertAfterSec())*time.Second, func() {
			if !a.askAlertDue(q.ID) {
				return
			}
			a.fireAskAlert(q.ID, "", q.SessionID, q.SessionTitle, q.Question, a.Cfg.askAlertText())
		})
	}
	// 经理代理通知（mgrnotify.go）：经理投递开启的那一轮，卡挂了设置时长（默认
	// 30 秒）仍无人回答（且未停表——用户碰过卡片=有人在场）就通知长期助理
	//——与上面的用户声光提醒并行，各管各。
	var mgrNotifyTimer *time.Timer
	if a.Cfg.mgrNotifyOn() {
		mgrNotifyTimer = time.AfterFunc(mgrNotifyDelay(a), func() {
			a.notifyManagerOfQuestion(q.ID)
		})
	}
	defer func() {
		if alertTimer != nil {
			alertTimer.Stop()
		}
		if mgrNotifyTimer != nil {
			mgrNotifyTimer.Stop()
		}
		a.permMu.Lock()
		delete(a.questions, q.ID)
		a.permMu.Unlock()
		// 停止连带取消待审批：本轮被停止（ctx 取消）时，无时限的待审批计划随
		// 本轮作废（拒绝/撤销语义，不是批准）。不清的话它会一直挂在库里，
		// /api/questions 兜底轮询还会把它重新挂出来——用户点完停止，审批卡
		// 仍浮在界面上。作废后落到下面的统一收卡（question_resolved）。
		if ctx.Err() != nil {
			if pp, ok := a.Store.PendingPlanOf(sess.ID); ok && pp.ID == q.ID {
				_ = a.Store.ClearPendingPlan(sess.ID)
				log.Printf("[对话] 会话 %s 被停止，待审批计划 %s 一并作废", sess.ID, q.ID)
			}
		}
		// 计划仍在库里等审批（用户已批/已驳，清库与收卡由 runExitPlanMode 的
		// 回答分支负责）：这里不发「已解决」，只让会话刷新一次，卡片原样留着。
		if pp, ok := a.Store.PendingPlanOf(sess.ID); ok && pp.ID == q.ID {
			a.broadcast(Event{Type: "session_updated", SessionID: sess.ID})
			return
		}
		a.broadcast(Event{Type: "question_resolved", QuestionID: q.ID, SessionID: sess.ID})
	}()
	// 无时限的卡（计划审批）让 timeout 永远为 nil：超时分支永不触发，
	// 只有用户批准/修改或本轮被停止（ctx 取消）才会结束等待。带时限的提问
	// 用可掐的 timer：holdQuestion 停表后，Stop 前已发出的那次触发按持有标记
	// 丢弃（时间差内到点的误报），此后无限等待。
	var timeout <-chan time.Time
	if timer != nil {
		timeout = timer.C
	}
	for {
		select {
		case ans := <-q.ch:
			a.reportQuestionOutcome(q, ans, false)
			return ans, true
		case <-timeout:
			a.permMu.Lock()
			held := q.TimerHeld
			a.permMu.Unlock()
			if held {
				continue // 已停表：丢弃误触的超时，转无限等待
			}
			a.reportQuestionOutcome(q, "", true)
			return "", false
		case <-ctx.Done():
			return "", false
		}
	}
}

// resolveQuestion 登记用户的回答并唤醒等待中的工具。
func (a *App) resolveQuestion(id, answer string) error {
	a.permMu.Lock()
	q, ok := a.questions[id]
	if ok && !q.decided {
		q.decided = true
		q.ch <- answer
	}
	a.permMu.Unlock()
	if !ok {
		// 内存里没有等待中的提问：可能是进程重启后留下来的待审批计划
		// （exit_plan_mode 提交时落了库，重启后谁也不在阻塞等待）。
		// 那种情况按「恢复执行」处理：把批准/修改落成一条通知并接续这一轮。
		return a.resolvePersistedPlan(id, answer)
	}
	return nil
}

// holdQuestion 标记「用户已经碰到/已经在看着这条请求」，并停掉它的计时：
//   - 悬停/触碰卡片或点停止按钮（前端 askHoldTimer）：带时限的提问停表转不限时；
//   - 从任何入口打开这条请求对应的页面（前端 notePlanHandled：右下角角标堆、
//     输入区上方的浮窗、消息流里的计划卡、右栏「计划」页签）：计划审批这类
//     本来就没有时限（timer == nil），没有表可停，但同样要置位 TimerHeld——
//     久等声光提醒（askAlertDue）与后端那次已排定的触发据此判定「已在处理」，
//     不再叮、前端也不朗读。
//
// 单向操作：置位后一直有效，直到作答或本轮被停止。停表即广播
// question_deadline，所有端的倒计时统一换成「已停止计时」。
func (a *App) holdQuestion(id string) {
	a.permMu.Lock()
	q, ok := a.questions[id]
	held := false
	if ok && !q.TimerHeld {
		q.TimerHeld = true
		if q.timer != nil { // 有时限的提问：停表转不限时（无时限的没有表可停）
			q.DeadlineMs = 0
			q.timer.Stop()
		}
		held = true
	}
	a.permMu.Unlock()
	if held {
		a.broadcast(Event{Type: "question_deadline", QuestionID: id, SessionID: q.SessionID, Held: true})
	}
}

// askAlertBeep 久等提醒到点的叮声（平台实现：MessageBeep；测试里换成计数桩，
// 跑测试时不真响）。
var askAlertBeep = systemBeep

// alertScreenHook 久等提醒的「桌面全屏蒙层」入口（平台实现 alertfull_windows.go；
// 测试里换桩，不在 CI 里真弹窗口）。qid=questionId/approvalId，关闭请求按它
// 防串轮；onDismiss=用户点击蒙层时的回调（广播 ask_alert_dismiss，让前端把
// 主窗口的提醒层一起收起——两处提醒点任意一处、两边同退）。
var alertScreenHook = alertScreenShow

// fireAskAlert 发一次久等提醒到点动作（唤起窗口 + 全屏蒙层 + 叮声 + 广播 ask_alert）。
// 提问 / 计划审批 / 越权审批共用同一套机制与同一个设置项（设置·常规·询问提醒）：
// questionID / approvalID 二选一填（前端按它把对应卡片与提醒层联动收起），
// sessionTitle/summary 进浮窗文案，output 是朗读文案。
// 前端两处出：主窗口内的提醒层（SSE 的 ask_alert）与桌面全屏蒙层（本函数直接
// 盖屏，主窗口被埋住/在别的虚拟桌面也看得见）；任意一处被点掉，另一处同退
//（前端收层 → POST /api/alert/screen；蒙层被点 → 广播 ask_alert_dismiss）。
func (a *App) fireAskAlert(questionID, approvalID, sessionID, sessionTitle, summary, output string) {
	alertFrontHook("坚祥数字助理 · 有个请求在等你处理",
		alertFrontText(sessionTitle, summary))
	qid := questionID
	if qid == "" {
		qid = approvalID
	}
	alertScreenHook("坚祥数字助理 · 有个请求在等你处理",
		alertFrontText(sessionTitle, summary), qid, func() {
			a.broadcast(Event{Type: "ask_alert_dismiss"})
		})
	askAlertBeep()
	a.broadcast(Event{Type: "ask_alert", QuestionID: questionID, ApprovalID: approvalID,
		SessionID: sessionID, Title: sessionTitle, Summary: summary, Output: output})
}

// askAlertDue 询问提醒（久等声光提醒）到点那一刻是否还该响：这条请求还在等、
// 用户没作答、也没停表（悬停/触碰/点了停止，或从任何入口打开对应页面进入处理）。
// 单独拆出来是为了可测——Timer 到点时的判定条件只有这一处。
func (a *App) askAlertDue(id string) bool {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	q, pending := a.questions[id]
	return pending && !q.decided && !q.TimerHeld
}

// resolvePersistedPlan 处理「库里那份待审批计划」的用户决定（批准/修改）。
// 走到这里的都是**没有内存等待者**的情形：最典型的是进程重启——计划提交时
// 就落了库，重启后内存里的等待者没了，可计划仍在等用户审批。
//
// 恢复执行的办法：把决定落成一条用户侧通知投递给会话，并按需要拉起新一轮
// （批准 → 退出计划模式并让模型按计划开始执行；修改 → 留在计划模式，模型
// 带着反馈修订后重新提交）。会话若正在跑，投递自然走排队/插话，不会打断它。
func (a *App) resolvePersistedPlan(id, answer string) error {
	var hit *PendingPlan
	for _, pp := range a.Store.PendingPlans() {
		if pp.ID == id {
			p := pp
			hit = &p
			break
		}
	}
	if hit == nil {
		return fmt.Errorf("提问不存在或已处理（可能已超时）")
	}
	sess, err := a.Store.Get(hit.SessionID)
	if err != nil {
		_ = a.Store.ClearPendingPlan(hit.SessionID)
		return fmt.Errorf("计划所属的会话已不存在（计划已作废）")
	}
	answer = strings.TrimSpace(answer)
	if answer == "__ignore__" {
		// 搁置：会话本来就没在跑（遗留计划多半来自重启之后），计划继续留着等处置
		a.broadcast(Event{Type: "session_updated", SessionID: sess.ID})
		log.Printf("[对话] 会话 %s 的遗留待审批计划 %s 被用户搁置（保留，继续等待审批）", sess.ID, id)
		return nil
	}
	// 批准/修改都让这份待审批记录作废（忽略那条路在上面提前返回了：计划要留着）
	_ = a.Store.ClearPendingPlan(hit.SessionID)
	var content string
	if answer == "__approve__" {
		sess.PlanMode = false
		_ = a.Store.SaveMeta(sess)
		content = "【计划已批准】用户批准了这份计划（提交时间 " + hit.CreatedAt + "）。会话已退出计划模式，请按该计划开始执行：" +
			"先做第一步，执行中如实汇报进展；写文件与执行命令仍受当前权限模式约束。"
	} else {
		content = "【计划未通过】用户没有批准这份计划，会话仍处于计划模式。用户反馈：" + answer +
			" 请据此修订计划（保留仍然有效的部分），完成后重新调用 exit_plan_mode 提交审批。"
	}
	d := a.deliverToSession(sess, content, kindReport, "plan")
	a.broadcast(Event{Type: "session_updated", SessionID: sess.ID})
	a.broadcast(Event{Type: "question_resolved", QuestionID: id, SessionID: sess.ID})
	if d.Started {
		go a.executeRun(sess, d.Run, nil)
	}
	log.Printf("[对话] 会话 %s 的遗留待审批计划 %s 已按用户决定处理（%s）", sess.ID, id, map[bool]string{true: "批准", false: "修改"}[answer == "__approve__"])
	return nil
}

// pendingQuestions 返回全部待回答提问（前端轮询兜底用）：内存里活着的等待者，
// 加上库里遗留的待审批计划——进程重启后等待者已不存在，但那份计划仍在等审批。
func (a *App) pendingQuestions() []*UserQuestion {
	a.permMu.Lock()
	out := []*UserQuestion{}
	seen := map[string]bool{}
	for _, q := range a.questions {
		out = append(out, q)
		seen[q.ID] = true
	}
	a.permMu.Unlock()
	// 内存里没有等待者、但库里还挂着待审批计划（典型场景：软件关掉又重开）：
	// 一并列出来，前端照样弹一张「计划审批」卡，批准/修改走 resolvePersistedPlan。
	for _, pp := range a.pendingPlanQuestions() {
		if seen[pp.ID] {
			continue
		}
		out = append(out, pp)
	}
	return out
}

// pendingPlanQuestions 从库里读回待审批计划，包装成提问项（承载「关软件重开
// 之后计划还在等审批」的那一半）。每条都顺手校验会话还在、且仍在计划模式：
// 会话已删掉、或计划模式已被关掉（用户在输入区手动关了，或已在别处批过）的
// 都算过期计划，就地清掉、不再挂着。
func (a *App) pendingPlanQuestions() []*UserQuestion {
	out := []*UserQuestion{}
	for _, pp := range a.Store.PendingPlans() {
		sess, err := a.Store.Get(pp.SessionID)
		if err != nil {
			_ = a.Store.ClearPendingPlan(pp.SessionID)
			continue
		}
		if !sess.PlanMode {
			_ = a.Store.ClearPendingPlan(pp.SessionID)
			continue
		}
		out = append(out, &UserQuestion{
			ID: pp.ID, SessionID: pp.SessionID, SessionTitle: sess.Title,
			Kind: askKindPlan, Question: "请审批以下执行计划", Plan: pp.Plan,
			AllowFree: true, CreatedAt: pp.CreatedAt,
		})
	}
	return out
}

// ---------- 工具 ----------

func askUserQuestionTool() Tool {
	return Tool{
		Spec: toolSpec("ask_user_question", "向用户提出一个需要其决定的问题并等待回答。用于：方案二选一、确认破坏性操作、缺失的关键信息。可带 2-6 个选项（用户也可自行输入），有推荐方案就写进选项。不要用提问代替能自己查到的事实。timeout_sec 分档见系统提示词「提问的分寸」；界面显示倒计时，用户悬停/触碰卡片可停表转不限时，到点未答视为放行——按你的默认方案继续执行。",
			objSchema(map[string]any{
				"question":    strProp("问题本身，一句话说清楚要用户决定什么"),
				"options":     arrayProp("可选：2-6 个选项的字符串数组，每项一个选项，如 [\"选项A\",\"选项B\",\"选项C\"]；推荐方案放第一个", strProp("选项文本，一句话说清这个选项")),
				"allow_free":  boolProp("是否允许用户自由输入（默认 true）"),
				"timeout_sec": intProp("可选：等待秒数。非必要提问 30；一般缺省 300；无推荐方案或必须确认传 0=不限时。到点未答视为放行，按默认方案继续"),
			}, "question")),
		Run: runAskUserQuestion,
	}
}

func runAskUserQuestion(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Question   string          `json:"question"`
		Options    json.RawMessage `json:"options"` // schema 声明为字符串数组；其他格式（如换行分隔的字符串、对象数组）直接报错，让模型按提示重发
		AllowFree  *bool           `json:"allow_free"`
		TimeoutSec *int            `json:"timeout_sec"` // 指针区分「没传」（默认 300）与「显式传 0」（不限时）
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	q := strings.TrimSpace(p.Question)
	if q == "" {
		return "", fmt.Errorf("question 不能为空")
	}
	noTimeout := false
	timeoutSec := 0
	if p.TimeoutSec != nil {
		if *p.TimeoutSec == 0 {
			noTimeout = true // 显式不限时：无推荐方案/必须确认的问题
		} else {
			timeoutSec = clampAskTimeoutSec(*p.TimeoutSec)
		}
	}
	options := []string{}
	if len(p.Options) > 0 {
		var arr []string
		if err := json.Unmarshal(p.Options, &arr); err != nil {
			return "", fmt.Errorf("options 必须是字符串数组（每项一个选项），如 [\"选项A\",\"选项B\",\"选项C\"]；推荐方案放第一个，不要传换行分隔的字符串或对象数组")
		}
		for _, o := range arr {
			if s := strings.TrimSpace(o); s != "" {
				options = append(options, s)
			}
		}
	}
	if len(options) > 6 {
		options = options[:6]
	}
	allowFree := true
	if p.AllowFree != nil {
		allowFree = *p.AllowFree
	}
	if len(options) == 0 && !allowFree {
		return "", fmt.Errorf("没有选项且不允许自由输入，用户无从回答")
	}
	if t.emit != nil {
		t.emit(Event{Type: "tool_wait", Name: "ask_user_question", Output: "等待用户回答…"})
	}
	uq := &UserQuestion{Kind: askKindAsk, Question: q, Options: options, AllowFree: allowFree, TimeoutSec: timeoutSec, NoTimeout: noTimeout}
	ans, ok := t.app.requestUserAnswer(t.ctx, t.sess, uq)
	if !ok {
		if t.ctx.Err() != nil {
			// 本轮被停止：提问随停止取消（没有答案），不是超时放行——AI 侧
			// 不得把停止当成「按默认方案继续」。
			return "（本轮被用户停止，提问已随停止取消——没有等到答案，不视为放行。）", nil
		}
		return "（用户未在时限内回答——视为放行：按你的默认方案继续执行，做完在汇报里说明这点。）", nil
	}
	ans = strings.TrimSpace(ans)
	if ans == "" {
		return "（用户跳过了这个问题。）", nil
	}
	return "用户回答：" + ans, nil
}

// requestPermissionTool 是通用的「提权申请」入口：模型把「把会话切到更宽
// 权限档」作为一次显式请求交给用户决定（弹审批卡），批准后立即切档并随
// 环境快照公告、权限芯片同步刷新。它补拒绝驱动的盲区：git_sync、MCP
// 连接器、register_skill 这类只读档整体不可用的通道，以及可预判要连续
// 多个写动作的场景——一次申请整档畅通，比逐个动作撞墙申请高效。
func requestPermissionTool() Tool {
	return Tool{
		Spec: toolSpec("request_permission",
			"申请把当前会话的权限档位调宽（全电脑只读 → 修改工作区 → 完全访问），弹审批卡由用户决定，批准后立即生效（权限芯片同步刷新）。适用于：任务确需写入而当前全电脑只读（git_sync、MCP 连接器、register_skill 等通道在全电脑只读档整体不可用），或可预判要连续多个写动作（一次申请比逐个撞墙申请高效）。仅在被真实拦住或明确预判做不了时使用，不要为省事预防性申请；用户拒绝或超时后本轮终局：如实说明并停止，不要绕过。",
			objSchema(map[string]any{
				"target": map[string]any{
					"type":        "string",
					"enum":        []string{permWorkspace, permFull, "danger-full-access"},
					"description": "申请切到的权限档：workspace-write（修改工作区）或 full / danger-full-access（完全访问），必须比当前档宽",
				},
				"justification": strProp("一句话向用户说明为什么要调宽权限（展示在审批卡上）"),
			}, "target", "justification")),
		Run: runRequestPermission,
	}
}

func runRequestPermission(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Target        string `json:"target"`
		Justification string `json:"justification"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	target := strings.TrimSpace(p.Target)
	if target == "danger-full-access" {
		target = permFull // 官方词汇表的 full 档别名
	}
	cur := permModeOf(t.sess)
	if target != permWorkspace && target != permFull {
		return "", fmt.Errorf("target 仅支持 %s / %s / danger-full-access，收到 %q", permWorkspace, permFull, p.Target)
	}
	if sandboxPolicyRank(target) <= sandboxPolicyRank(cur) {
		return "", fmt.Errorf("当前会话已是「%s」，target=%s 不比它宽，无需申请；更宽才值得申请，或直接执行", permModeLabel(cur), target)
	}
	just := strings.TrimSpace(p.Justification)
	if just == "" {
		return "", fmt.Errorf("justification 不能为空：一句话说明为什么要调宽权限（给用户看的理由）")
	}
	summary := fmt.Sprintf("把会话权限从「%s」调宽到「%s」｜理由：%s", permModeLabel(cur), permModeLabel(target), truncateRunes(just, 120))
	if t.emit != nil {
		t.emit(Event{Type: "tool_wait", Name: t.toolName, Output: "等待用户审批（申请调整会话权限）…"})
	}
	approved, _, timedOut := t.app.requestApproval(t.ctx, t.sess, t.toolName, summary,
		fmt.Sprintf("当前会话为「%s」，模型申请切到「%s」（批准后立即生效，本会话后续动作按新档执行）", permModeLabel(cur), permModeLabel(target)))
	if !approved {
		if timedOut {
			return "", fmt.Errorf("等待审批超时（上限 %s），按拒绝处理：会话保持「%s」。可请用户直接在权限芯片上调档，或稍后重新发起", approvalTTL, permModeLabel(cur))
		}
		return "", fmt.Errorf("用户未批准调整权限，会话保持「%s」。本轮终局：如实说明并停止，不要换法绕过", permModeLabel(cur))
	}
	wasMode, wasTools := permModeOf(t.sess), t.sess.Tools
	t.app.switchSessionPerm(t.sess, target)
	// 「权限变更」即时落地：注记随新环境快照落库（env 通道），权限芯片同步刷新
	t.app.landPermChange(t.sess, permSrcApproval, wasMode, wasTools, t.emit)
	return fmt.Sprintf("已批准：会话权限已从「%s」切换为「%s」，后续动作按新档执行，不再逐次申请", permModeLabel(wasMode), permModeLabel(target)), nil
}

func enterPlanModeTool() Tool {
	return Tool{
		Spec: toolSpec("enter_plan_mode", "请求把会话切换到计划模式：先只用只读工具调研，拟出完整计划交用户批准后再动手。适合改动面大、跨多文件、有风险、或用户明确要先看方案的任务；简单任务不要用，直接做。调用会弹出确认卡，用户同意才切换，拒绝则维持原模式。已在计划模式或目标模式运行中时调用会报错。",
			objSchema(map[string]any{
				"reason": strProp("为何值得先计划：任务概况与复杂点，一两句话，展示在用户的确认卡上"),
			}, "reason")),
		Run: runEnterPlanMode,
	}
}

func runEnterPlanMode(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	reason := strings.TrimSpace(p.Reason)
	if reason == "" {
		return "", fmt.Errorf("reason 不能为空")
	}
	if t.sess.PlanMode {
		return "", fmt.Errorf("当前会话已在计划模式，无需重复进入；完成调研后调用 exit_plan_mode 提交计划")
	}
	if t.sess.GoalMode {
		return "", fmt.Errorf("计划/目标模式二选一；当前目标模式运行中（目标「%s」），如需改为先计划，请让用户关闭目标模式后再试", t.sess.GoalText)
	}
	if t.emit != nil {
		t.emit(Event{Type: "tool_wait", Name: "enter_plan_mode", Output: "等待用户确认进入计划模式…"})
	}
	uq := &UserQuestion{Kind: askKindEnter, Question: truncateRunes(reason, 1000), AllowFree: true}
	ans, ok := t.app.requestUserAnswer(t.ctx, t.sess, uq)
	if !ok {
		if t.ctx.Err() != nil {
			return "（本轮被用户停止，进入计划模式的确认已随停止取消，会话维持原模式。）", nil
		}
		return "（用户未在时限内确认，会话维持原模式。可直接执行；仍觉得该先计划的话稍后再提。）", nil
	}
	ans = strings.TrimSpace(ans)
	if ans == "__approve__" {
		t.sess.PlanMode = true
		_ = t.app.Store.SaveMeta(t.sess)
		t.app.broadcast(Event{Type: "session_updated", SessionID: t.sess.ID})
		return "用户同意进入计划模式，会话已切换。此阶段写入/执行/派发类工具暂停，只做只读探索；调研完成后调用 exit_plan_mode 提交完整计划（目标/步骤/涉及文件/验收标准）供用户批准，批准后再动手。", nil
	}
	return "用户未同意进入计划模式，会话维持原模式。用户反馈：" + ans, nil
}

func exitPlanModeTool() Tool {
	return Tool{
		Spec: toolSpec("exit_plan_mode", "计划模式下使用：把探索得到的完整执行计划提交给用户审批。批准后会话自动退出计划模式，你随即开始执行；拒绝则带着用户反馈留在计划模式修订。不在计划模式时调用会报错。计划要具体：目标、步骤、涉及文件、验收标准。",
			objSchema(map[string]any{
				"plan": strProp("完整计划（Markdown），交给用户审批的正是这段文字"),
			}, "plan")),
		Run: runExitPlanMode,
	}
}

func runExitPlanMode(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	plan := strings.TrimSpace(p.Plan)
	if plan == "" {
		return "", fmt.Errorf("plan 不能为空")
	}
	if !t.sess.PlanMode {
		return "", fmt.Errorf("当前会话不在计划模式，无需提交计划；直接执行即可")
	}
	if t.emit != nil {
		t.emit(Event{Type: "tool_wait", Name: "exit_plan_mode", Output: "等待用户审批计划…"})
	}
	// kind=plan：这份提问不设时限，且 requestUserAnswer 会在提交那一刻把它落库
	// ——关软件重开之后，它仍以「待审批计划」的形态挂在会话里等用户决定。
	uq := &UserQuestion{Kind: askKindPlan, Question: "请审批以下执行计划", Plan: truncateRunes(plan, 4000), AllowFree: true}
	ans, ok := t.app.requestUserAnswer(t.ctx, t.sess, uq)
	if !ok {
		// 计划审批不设时限：走到这里只可能是本轮被停止/中断（ctx 取消）。待审批
		// 计划已随停止一并作废（requestUserAnswer 的收尾清库并广播收卡）——不再
		// 挂在界面上等用户处置；如仍要推进，重新提交一份计划等审批。
		return "（本轮被用户停止，计划审批已随停止一并取消（作废，不是批准）。如仍需要按计划推进，请重新提交计划等待用户审批。）", nil
	}
	ans = strings.TrimSpace(ans)
	if ans == "__approve__" {
		if ans == "__ignore__" {
			// 「忽略计划」= 搁置这一轮：把本轮停下来（等同点「停止」），计划本身不撤销
			// ——它仍留在对话里（也留在库里继续等处置），用户之后随时能再批准或修改。
			t.app.stopRun(t.sess.ID)
			return "（用户先搁置了这份计划：本轮到此结束。计划仍留在对话里等待处置，用户之后可以再批准或修改。）", nil
		}
		// 用户批了：撤掉库里那份待审批记录（内存等待者的收尾由 requestUserAnswer 做）
		_ = t.app.Store.ClearPendingPlan(t.sess.ID)
		t.sess.PlanMode = false
		// 步数重算：计划获批是新工作段的起点，主循环检测到信号就把 step 从头计
		// （探索阶段已消耗的步数不挤占执行阶段；planApproved 只在主循环构造的 tctx 里非 nil）
		if t.planApproved != nil {
			*t.planApproved = true
		}
		_ = t.app.Store.SaveMeta(t.sess)
		t.app.broadcast(Event{Type: "session_updated", SessionID: t.sess.ID})
		// 决定已落实：广播「已解决」让各窗口都收掉这张审批卡（requestUserAnswer 的
		// 收尾只负责保留场景，不负责这里的收卡）
		t.app.broadcast(Event{Type: "question_resolved", QuestionID: uq.ID, SessionID: t.sess.ID})
		return "用户批准了计划，会话已退出计划模式。现在按计划开始执行；执行中写文件与执行命令仍受当前权限模式约束。", nil
	}
	// 用户没批：这份待审批记录作废（模型带着反馈修订后会重新提交一份新的）
	_ = t.app.Store.ClearPendingPlan(t.sess.ID)
	t.app.broadcast(Event{Type: "question_resolved", QuestionID: uq.ID, SessionID: t.sess.ID})
	return "用户未批准计划，会话仍处于计划模式。用户反馈：" + ans, nil
}

func updateGoalTool() Tool {
	return Tool{
		Spec: toolSpec("update_goal", "目标模式下汇报目标的最终状态：确实达成时 result=complete；确认无法推进（缺权限/缺信息/外部受阻、继续没有意义）时 result=blocked。summary 用一两句话说明结果或阻碍。这是结束目标推进的唯一正式方式；未开目标模式时调用会报错。不要在目标未真正完成时谎报 complete。",
			objSchema(map[string]any{
				"result":  strProp("complete 或 blocked"),
				"summary": strProp("一两句话：达成了什么，或卡在哪里"),
			}, "result", "summary")),
		Run: runUpdateGoal,
	}
}

func runUpdateGoal(t *toolContext, args json.RawMessage) (string, error) {
	var in struct {
		Result  string `json:"result"`
		Summary string `json:"summary"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("参数不是合法 JSON")
	}
	in.Result = strings.TrimSpace(in.Result)
	in.Summary = strings.TrimSpace(in.Summary)
	if in.Result != "complete" && in.Result != "blocked" {
		return "", fmt.Errorf("result 只能是 complete 或 blocked")
	}
	if !t.sess.GoalMode {
		return "", fmt.Errorf("当前未开启目标模式，无需调用 update_goal；直接正常回复即可")
	}
	if in.Summary == "" {
		return "", fmt.Errorf("summary 不能为空：请说明达成了什么或卡在哪里")
	}
	t.sess.GoalStatus = in.Result
	if err := t.app.Store.SaveMeta(t.sess); err != nil {
		return "", err
	}
	t.app.broadcast(Event{Type: "session_updated", SessionID: t.sess.ID})
	label := "已达成"
	if in.Result == "blocked" {
		label = "无法推进（受阻）"
	}
	return fmt.Sprintf("目标已记录为「%s」：%s。请基于以上全部进展，用面向用户的中文写最终总结（结果、产物位置、未尽事项），不要再调用工具。", label, in.Summary), nil
}
