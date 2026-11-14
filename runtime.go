package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 后台运行时：会话执行注册表 + 全局事件总线。
// 派发的子任务在独立 goroutine 里跑；主对话的 SSE 断了也不影响执行，
// 进度通过 /api/events 全局事件流推给所有在线前端。
//
// 会话执行（sessionRun）拥有独立于 HTTP 请求的 ctx 与订阅者集合：发送消息的
// 请求只是「接上这条流」，浏览器刷新/关页只断开订阅，执行继续跑完并落库；
// 重新打开页面用 GET /api/sessions/{id}/stream 再接上继续看。只有显式停止
// （POST /stop）或进程退出才会终止执行。

type sessionRun struct {
	ctx       context.Context
	cancel    context.CancelFunc
	startedAt time.Time
	done      chan struct{} // 执行结束即关闭，订阅者据此退出等待

	mu       sync.Mutex
	subs     map[int]chan Event
	nextSub  int
	partial  string // 当前步骤已流出的正文（重连时补发，避免丢半句）
	thinking string // 当前步骤已流出的思考原文
	stopped  bool   // 是否由用户显式停止
	killed   bool   // 是否已被停止看门狗强制终结（执行体迟到醒来后据此跳过终态交代）
	cleanEnd bool   // 本轮是否正常收尾（没出错、没被停止）：收口时据此决定要不要兜底续发队列

	modelAbort *modelAbortEntry // 模型调用进行中的步级打断句柄（插话立即打断用）。只在主循环调模型的窗口内非 nil——工具执行段为 nil，插话此刻到达就不打断、照旧等下一步调用模型前注入
}

// PendingMsg 是执行中的一条待生效用户消息：插话（下一步调用模型前注入）
// 或排队（本轮结束后作为独立一轮发送）。放在服务端，刷新页面不丢。
// Kind/Origin 是它落库时的类型与来源（空 = chat，即用户自己发的）；
// 数字助理派发注入的需求带 report/task——排队或插话后排到落库，
// 界面照旧按「数字助理说的」渲染成左对齐通知，不会变成用户气泡。
type PendingMsg struct {
	ID        string `json:"id"`
	Content   string `json:"content"`
	CreatedAt string `json:"createdAt"`
	Kind      string `json:"kind,omitempty"`
	Origin    string `json:"origin,omitempty"`
}

// PendingState 是某个会话当前的待生效消息快照（随会话详情与事件下发）。
type PendingState struct {
	Steer  []*PendingMsg `json:"steer"`
	Queue  []*PendingMsg `json:"queue"`
	Paused bool          `json:"paused"` // 队列暂停：本轮结束后排队消息保留不自动发送
}

func (a *App) initRuntime() {
	a.runners = map[string]*sessionRun{}
	a.mgrTurns = map[string]mgrTurnGrant{}
	a.mgrTurnGen = map[string]int64{}
	a.subscribers = map[chan Event]bool{}
	a.grants = map[string]*Grant{}
	a.approvals = map[string]*Approval{}
	a.questions = map[string]*UserQuestion{}
	a.remotes = map[string]*remoteClient{}
	a.pendingSteer = map[string][]*PendingMsg{}
	a.pendingQueue = map[string][]*PendingMsg{}
	a.queuePaused = map[string]bool{}
	a.runErrors = map[string]string{}
	a.stopWatchdog = stopWatchdogDefault // 停止看门狗：协作取消后 3 秒仍未收口就强制终结（测试可缩短）
	a.streamKills = map[string]func(){}  // 在跑模型流的强断句柄登记（llm.go 注册，硬终结时调用）
	// 受保护路径：助手配置文件本身与数据目录（含 API Key 与会话记录），
	// 任何模式禁访问。注意只保护配置文件本身，不保护其所在目录——默认
	// 工作区就是程序目录，整目录保护会让开箱即用的会话什么都写不了。
	a.protectPaths = []string{}
	if abs, err := filepath.Abs(a.CfgPath); err == nil {
		a.protectPaths = append(a.protectPaths, strings.ToLower(filepath.Clean(abs)))
	}
	if abs, err := filepath.Abs(a.Cfg.DataDir); err == nil {
		a.protectPaths = append(a.protectPaths, strings.ToLower(filepath.Clean(abs)))
	}
}

// beginRun 登记一次会话执行（ctx 独立于发起它的 HTTP 请求）；已在跑则返回 false。
func (a *App) beginRun(id string) (*sessionRun, bool) {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if _, ok := a.runners[id]; ok {
		return nil, false
	}
	ctx, cancel := context.WithCancel(context.Background())
	r := &sessionRun{ctx: ctx, cancel: cancel, startedAt: time.Now(), done: make(chan struct{}),
		subs: map[int]chan Event{}}
	a.runners[id] = r
	a.clearRunError(id)             // 新一轮开始：上一次的报错条自动消失
	_ = a.Store.SetLastStop(id, "") // 侧栏「⚠ 已中断」随之收掉（这一轮正在跑）
	// 新一轮开始：上一轮遗留的「用户已请求停止」标志作废——那一次停止已经收口，
	// 不能再拿来解释这一轮的中断（见 Store.ClearStopPending）
	_ = a.Store.ClearStopPending(id)
	_ = a.Store.UpdateStatus(id, "running")
	a.markRunOwner(id)     // 记下「本实例在跑它」：别的实例启动时不会把它误判成中断
	a.ensureRunHeartbeat() // 有在跑的会话时才起心跳，空闲时零唤醒（见 runowner.go）
	return r, true
}

// finishRun 注销执行、关闭订阅者并置为空闲：按 id 摘注册表里**当前**那一轮。
// 看门狗硬终结（forceStopRun）与少数无执行体引用的调用点用这个入口。
func (a *App) finishRun(id string) {
	a.runMu.Lock()
	r, ok := a.runners[id]
	if ok {
		delete(a.runners, id)
	}
	a.runMu.Unlock()
	if !ok {
		return
	}
	a.closeRun(id, r)
}

// finishRunOf 带执行体身份的收口：只有注册表里还是这一轮（指针相同）才动手。
// 停止看门狗硬终结之后，用户可能立刻开新一轮；卡死的旧执行体迟到醒来再按 id
// 盲摘会把新一轮误摘掉（状态被改回空闲、执行权凭空消失）。按身份对上才收口，
// 对不上说明这一轮已被别人收掉（正常收口 / 硬终结都算），全部动作幂等跳过。
// 执行体自己的 defer 一律走这个入口。
func (a *App) finishRunOf(id string, r *sessionRun) {
	a.runMu.Lock()
	cur, ok := a.runners[id]
	if !ok || cur != r {
		a.runMu.Unlock()
		return
	}
	delete(a.runners, id)
	a.runMu.Unlock()
	a.closeRun(id, r)
}

// closeRun 是 finishRun / finishRunOf 共用的收口体（调用时已从注册表摘下这一轮）：
// 关闭订阅者与 done、取消残余 ctx、置空闲、清归属与停止待收标志、触发交付与
// 兜底续发。
func (a *App) closeRun(id string, r *sessionRun) {
	a.dropManagerTurnOf(id, r) // 经理代批授权随轮撤销（mgrnotify.go）：收口即失去代决窗口
	r.mu.Lock()
	for i, ch := range r.subs {
		close(ch)
		delete(r.subs, i)
	}
	r.mu.Unlock()
	close(r.done)
	r.cancel()
	_ = a.Store.UpdateStatus(id, "idle")
	a.clearRunOwner(id) // 归属随本轮收口一起清掉（下一轮的归属由下一次 beginRun 写）
	// 这一轮已经收口（正常完成 / 用户停止 / 报错都走这里）：待收口的「用户已请求
	// 停止」标志随之作废——停止记录本身保留，历史仍查得出来（Store.StopRecord）
	_ = a.Store.ClearStopPending(id)
	// 本轮结束（正常/停止/报错收口都走这里）：执行已登记的会话后交付。
	// 异步执行避免大文件搬运拖住收口；认领机制保证与启动恢复不重不漏。
	go a.flushDeliveries(id)
	// 收口这一侧兜底续发：本轮已经在收尾、消息却恰好在续发循环走完之后才挤进
	// 队列（派发回报 / 插话 / 前端排队都可能撞上这个窗口）——执行权此刻已从注册
	// 表摘掉，再查一次：还有待发消息就再拉一轮把它发出去，不再出现「消息躺在
	// 队列里、界面上等用户点继续发送」。只认正常收尾＋开关开着；出错或被用户
	// 停止的队列照旧保留给用户处置。
	if r.endedCleanly() && a.queueAutoSendOn() && !a.queuePausedOf(id) && a.hasQueued(id) {
		if m, err := a.Store.Get(id); err == nil {
			log.Printf("[排队] 会话 %s 收口时队列仍有待发消息，自动拉起下一轮", id)
			a.kickQueue(m)
		}
	}
}

// runOf 取会话执行中的运行体；未在执行返回 nil。
func (a *App) runOf(id string) *sessionRun {
	a.runMu.Lock()
	r := a.runners[id]
	a.runMu.Unlock()
	return r
}

// 停止的触发者口径（StopRecord.Actor 的取值）：两种都是「人为叫停」——Kind 一律
// 记 stopByUser（重启恢复据此按人为停止认，而不是当成服务重启那种意外中断），
// 区别只在落库说明与回报首句里怎么称呼触发者。
const (
	stopActorUser      = "user"      // 用户亲自点界面上的停止按钮（POST /stop → App.stopRun）
	stopActorAssistant = "assistant" // 数字助理按用户指示代停（stop_session 工具 → App.stopRunBy）
)

// stopActorLabel 停止触发者的中文称呼（回报首句用）。
func stopActorLabel(actor string) string {
	if actor == stopActorAssistant {
		return "数字助理（受用户指示）"
	}
	return "用户"
}

// stopTriggerNote 触发方式的一句话说明：落库的 StopRecord.Note 与回报首句共用
// 同一份文案，两处口径不会漂。
func stopTriggerNote(actor string) string {
	if actor == stopActorAssistant {
		return "数字助理按用户指示代停（等效用户点停止按钮）"
	}
	return "用户点击停止按钮"
}

// stopRun 请求停止会话执行（人工点「停止」按钮的入口：POST /api/sessions/{id}/stop
// → handleStop）。分级停止：先协作式取消（模型流、工具、等待都会收到 ctx 取消），
// 3 秒仍未收口由停止看门狗强制终结（见 watchStop/forceStopRun）；并连带终止该
// 会话全部后台作业——停止要彻底：作业的 ctx 独立挂 Background()（跨轮存活是它的
// 常态），不在这里一并取消的话，停止后会话看着空闲、作业还在后台跑（见 jobs.go
// 的 stopSessionJobs）。
// 同时把「人为停止」这件事落库（见 store.go 的 StopRecord）：只记内存的话，
// 服务一重启意图就丢了，启动恢复把「用户主动叫停」误报成「服务重启中断」，
// 数字助理据此还会重新派发/续跑用户已经叫停的任务。
func (a *App) stopRun(id string) bool {
	return a.stopRunBy(id, stopActorUser, stopTriggerNote(stopActorUser))
}

// stopRunBy 是带**触发者**的停止入口：停止动作与 stopRun 完全一致（同一套协作式
// 取消 + 终止该会话后台作业 + 停止原因落库），只有落库的 Actor/Note 不同——人工点
// 按钮走 stopActorUser，数字助理的 stop_session 工具按用户指示代停走
// stopActorAssistant。这样「谁把任务停了」在停止记录、重启恢复与派发回报里都分得开
//（两者都算人为叫停：Kind 都是 stopByUser，不会被当成故障而重新派发）。
func (a *App) stopRunBy(id, actor, note string) bool {
	jobsKilled := a.stopSessionJobs(id)
	r := a.runOf(id)
	if r == nil {
		return jobsKilled > 0 // 执行体不在跑但作业还在：终止了作业也算停止成功
	}
	r.mu.Lock()
	first := !r.stopped // 连点停止只记一次，不把计数打花
	r.stopped = true
	r.mu.Unlock()
	if first {
		nowMS := time.Now().UnixMilli()
		if strings.TrimSpace(note) == "" {
			note = stopTriggerNote(actor)
		}
		_ = a.Store.SetStopRecord(id, StopRecord{
			Kind: stopByUser, Actor: actor, At: now(), AtMs: nowMS,
			Manual: true, // 这一轮还没收口：重启恢复按「人为停止」优先认它
			Note:   note,
		})
		log.Printf("[停止] 会话 %s 由%s请求停止（停止原因已落库，重启后仍可查）", id, stopActorLabel(actor))
	}
	r.cancel()
	// 停止看门狗：协作取消只是「请求」，不保证生效——模型流/工具卡死不响应
	// ctx 时，会话会永远停在「停止中」。首次停止请求时挂一个兜底定时器，到点
	// 仍未见收口就强制终结，保证停止请求最终必然生效（见 watchStop）。
	if first {
		go a.watchStop(id, r)
	}
	return true
}

// stopWatchdogDefault 停止看门狗的默认等待：协作取消发出后等这么久仍未收口，
// 就升级为强制终结。取消传播到收口正常在亚秒级（模型流读被 transport 掐断、
// 子进程被 Kill、等待走 ctx.Done），3 秒只兜「卡死不响应取消」的极端情况。
const stopWatchdogDefault = 3 * time.Second

// watchStop 是停止的看门狗（stopRunBy 在首次停止请求时启动）：先给协作取消
// 一个收口的窗口——执行体正常收口会关闭 done，这里就地退出、什么都不做；
// 到点仍在注册表里（且还是同一轮）说明取消没能让执行体停下来（模型流/工具
// 卡死不响应 ctx），升级为强制终结（forceStopRun）。分级停止保证「点停止」
// 最终必然生效，会话不会永远停在「停止中」。
func (a *App) watchStop(id string, r *sessionRun) {
	delay := a.stopWatchdog
	if delay <= 0 {
		delay = stopWatchdogDefault
	}
	t := time.NewTimer(delay)
	defer t.Stop()
	select {
	case <-r.done: // 协作取消已生效、执行体自行收口
		return
	case <-t.C:
	}
	// 只处理还是同一轮的情况：等待期间若已收口又开了新一轮（指针不同），
	// 新一轮不是这次停止请求的对象，绝不动它。
	if a.runOf(id) != r {
		return
	}
	a.forceStopRun(id, r)
}

// forceStopRun 强制终结一轮对取消无响应的执行（分级停止的第二级）：
//  1. 再发一次取消（幂等）；
//  2. 强制断开该会话在跑的模型流连接（不依赖 ctx 的硬断，见 killSessionStreams）；
//  3. 终止该会话仍在跑的后台作业与子进程（协作阶段已发过取消，这里对还没
//     死的按 PID 补刀，见 killSessionChildren）；
//  4. 强制收口（finishRun：摘注册表、关闭订阅者、置空闲、清归属）——之后
//     执行体再醒来，finishRunOf 认出注册表里已不是自己，收口幂等空转，
//     不会把状态改回去；终态交代被 wasKilled 拦住，不再写库发事件。
// 只动这一个会话：其他正在执行的会话、发布流程（隔离目录构建）一概不碰。
func (a *App) forceStopRun(id string, r *sessionRun) {
	if !r.markKilled() {
		return // 已被终结过（并发触发）
	}
	delay := a.stopWatchdog
	if delay <= 0 {
		delay = stopWatchdogDefault
	}
	log.Printf("[停止] 会话 %s 协作取消 %v 后仍未收口，强制终结：断模型流、终止子进程与后台作业、置为停止态", id, delay)
	r.cancel()
	a.killSessionStreams(id)
	a.stopSessionJobs(id)
	killSessionChildren(id)
	a.finishRun(id) // 此刻注册表里还是这一轮，等价于按身份收口
	// 执行体不会再来交代终态（wasKilled 拦住），由这里代发：前端立即见到
	// 停止完成、会话回到可交互的空闲态
	a.broadcast(Event{Type: "stopped", SessionID: id})
	a.broadcast(Event{Type: "session_updated", SessionID: id})
}

// registerStreamKill 登记该会话当前模型流的强制断开器（agent.go 的流回调注册，
// llm.go streamChatOnce 在拿到响应体时建立）。同一会话同一时刻只有一条在跑的
// 模型流（主循环串行调用），后注册的覆盖先注册的。
func (a *App) registerStreamKill(id string, abort func()) {
	a.streamKillMu.Lock()
	a.streamKills[id] = abort
	a.streamKillMu.Unlock()
}

// clearStreamKill 注销该会话的模型流断开器（流正常结束时调用）。
func (a *App) clearStreamKill(id string) {
	a.streamKillMu.Lock()
	delete(a.streamKills, id)
	a.streamKillMu.Unlock()
}

// modelAbortEntry 是一次模型调用的打断句柄：beginModelStream 创建、模型调用
// 结束时经 end() 按身份注销。为什么带身份：强停收口与新轮开跑之间存在竞态
// 窗口，迟到醒来的旧循环不能误清注册表里已属于新一轮的句柄。
type modelAbortEntry struct {
	r      *sessionRun
	cancel context.CancelFunc
}

// beginModelStream 登记该会话当前模型调用的步级取消器（RunAgent 每次调模型前
// 注册、调用结束注销）。插话到达时 kickModelStream 用它掐断当前模型流，让
// 插话立即生效——而不是等模型把这轮思考/正文全部吐完。cancel 独立于 run.ctx：
// 只取消本步的模型调用，主循环继续（带着插话重组上下文重调）；用户停止仍走
// run.ctx 取消，两者不混。会话不在执行注册表里（直接调 RunAgent 的测试等
// 场景）时返回 nil，kick 静默无效、插话照旧走下一步注入。
func (a *App) beginModelStream(id string, cancel context.CancelFunc) *modelAbortEntry {
	r := a.runOf(id)
	if r == nil {
		return nil
	}
	e := &modelAbortEntry{r: r, cancel: cancel}
	r.mu.Lock()
	r.modelAbort = e
	r.mu.Unlock()
	return e
}

// end 注销本次注册（模型调用结束、即将进入工具执行段时调用）。注册表里的
// 句柄必须还是自己（见 modelAbortEntry 的身份说明）才清。
func (e *modelAbortEntry) end() {
	if e == nil {
		return
	}
	e.r.mu.Lock()
	if e.r.modelAbort == e {
		e.r.modelAbort = nil
	}
	e.r.mu.Unlock()
}

// kickModelStream 打断该会话正在进行的模型调用（若有）：插话入队后调用。
// 不在模型调用中（工具执行段 / 会话空闲 / 已收口）就是空操作——插话留在
// 队列里，主循环下一步调用模型前照旧注入，与原有行为一致。
func (a *App) kickModelStream(id string) {
	r := a.runOf(id)
	if r == nil {
		return
	}
	r.mu.Lock()
	e := r.modelAbort
	r.mu.Unlock()
	if e != nil {
		e.cancel()
	}
}

// killSessionStreams 强制断开该会话在跑的模型流连接。ctx 取消本应让 HTTP 流
// 中断（请求挂在 run.ctx 上），这里是硬终结的第二重保险：万一某条流不响应
// 取消，直接关闭响应体，读流立刻以错误返回。
func (a *App) killSessionStreams(id string) {
	a.streamKillMu.Lock()
	abort := a.streamKills[id]
	delete(a.streamKills, id)
	a.streamKillMu.Unlock()
	if abort != nil {
		abort()
	}
}

// stopReportHead 组织「这一轮被停掉」的派发回报首句（见 runSubAgent 的 stopped
// 分支）：触发者与触发方式分开写（人工点按钮 / 数字助理按用户指示代停），但
// 「已由用户手动停止」这个说法必须原样保留——上游提示词（agent.go 的
// assistantPrompt「回报里的中断原因必须照着认」）与重启恢复（runowner.go 的
// reapInterruptedSession）都按这句话认「人为叫停」，措辞一漂就会有人把它当成
// 意外中断而重新派发。
func stopReportHead(sub *SessionMeta, rec StopRecord, at string, rounds, steps int) string {
	note := strings.TrimSpace(rec.Note)
	if note == "" {
		note = stopTriggerNote(rec.Actor)
	}
	return "【派发回报】任务会话「" + sub.Title + "」（" + sub.ID + "）已由用户手动停止（" + note +
		"，停止时间 " + at + "｜触发者：" + stopActorLabel(rec.Actor) + "｜进度：见下方摘要（共 " +
		itoa(rounds) + " 轮 / " + itoa(steps) + " 步））。这是用户主动叫停，不是异常中断、不是故障。"
}

// publish 把一个执行事件发给该会话的全部订阅者（订阅者跟不上就丢帧，绝不阻塞执行）。
func (r *sessionRun) publish(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, ch := range r.subs {
		select {
		case ch <- ev:
		default:
		}
	}
}

// observe 记录当前步骤已流出的正文/思考，供重连补发。
func (r *sessionRun) observe(ev Event) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch ev.Type {
	case "delta":
		r.partial += ev.Delta
	case "reasoning":
		r.partial += ""
		r.thinking += ev.Delta
	case "message", "done", "retry":
		r.partial, r.thinking = "", ""
	}
}

// snapshot 取当前进行中的流式文本（重连时先补上，再接后续增量）。
func (r *sessionRun) snapshot() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.partial, r.thinking
}

// subscribe 订阅该执行的后续事件；返回的收尾函数必须调用（断开订阅）。
func (r *sessionRun) subscribe() (chan Event, func()) {
	ch := make(chan Event, 128)
	r.mu.Lock()
	id := r.nextSub
	r.nextSub++
	r.subs[id] = ch
	r.mu.Unlock()
	return ch, func() {
		r.mu.Lock()
		if _, ok := r.subs[id]; ok {
			delete(r.subs, id)
			close(ch)
		}
		r.mu.Unlock()
	}
}

// IsRunning 判断会话是否在执行（内存注册表为准，数据库状态只作启动恢复用）。
func (a *App) IsRunning(id string) bool {
	return a.runOf(id) != nil
}

// broadcast 向所有在线前端广播事件（无订阅者时丢弃，不阻塞）。
func (a *App) broadcast(ev Event) {
	a.evtMu.Lock()
	defer a.evtMu.Unlock()
	for ch := range a.subscribers {
		select {
		case ch <- ev:
		default: // 订阅者跟不上就丢帧，事件流只做进度通知
		}
	}
}

// pushPending 记下一条待生效消息：mode=steer 插话（下一步调用模型前注入），
// 其他按排队（本轮结束后作为独立一轮发送）。kind/origin 决定它落库后的身份
// （空 = chat，即用户自己发的；数字助理派发注入的传 report/task）。
// steer 入队后顺手 kick 当前模型调用：模型正在思考/吐正文就立即掐断，主循环
// 下一步带着插话重组重调（工具执行段 kick 无效，照旧等下一步注入）——所有
// 插话入口（用户 interject、排队坞提前、AI 派发投递）在此统一生效。
func (a *App) pushPending(sessionID, mode, content, kind, origin string) *PendingMsg {
	pm := &PendingMsg{ID: newID(), Content: content, CreatedAt: now(), Kind: kind, Origin: origin}
	a.pendMu.Lock()
	if mode == "steer" {
		a.pendingSteer[sessionID] = append(a.pendingSteer[sessionID], pm)
	} else {
		a.pendingQueue[sessionID] = append(a.pendingQueue[sessionID], pm)
	}
	a.pendMu.Unlock()
	if mode == "steer" {
		a.kickModelStream(sessionID)
	}
	a.broadcastPending(sessionID)
	return pm
}

// delivery 是一次投递的结果：Started=true 表示消息已落库并已占住执行权
// （Run 是执行权句柄，由调用方接上自己的执行体）；Started=false 表示目标会话
// 正在执行、消息已进待生效队列，Mode 是排队还是插话。
type delivery struct {
	Started bool
	Run     *sessionRun
	Mode    string
}

// deliverToSession 是「往会话里投递一条用户侧消息」的统一收口：前端发消息
// （handleChat / handleInterject）与数字助理派发注入共用同一套语义——空闲就
// 落库并占住执行权，执行中一律**插话**：AI 发话（派发回报、求助、待办转达、
// 转达需求）要的是对方立刻看到，排队等下一轮既滞后也传不上话。用户自己在
// 执行中发消息才按其「对话中途发送行为」设置选排队/插话（handleInterject）。
// kind/origin 决定消息身份（chat=用户气泡，aiuser=AI 发出的用户消息，
// report=系统通知），语义上的差异只体现在这里，投递行为完全一致。
func (a *App) deliverToSession(m *SessionMeta, content, kind, origin string) *delivery {
	lock := a.sessLock(m.ID)
	lock.Lock()
	defer lock.Unlock()
	// 执行中：一律插话（与 handleInterject 同一套队列与落库时机）
	if a.IsRunning(m.ID) {
		a.pushPending(m.ID, "steer", content, kind, origin)
		return &delivery{Mode: "steer"}
	}
	run, ok := a.beginRun(m.ID)
	if !ok { // 极小概率竞态：刚被别的入口占住，同样插话（注入它接下来的一步）
		a.pushPending(m.ID, "steer", content, kind, origin)
		return &delivery{Mode: "steer"}
	}
	a.landUserSide(m, content, kind, origin, nil)
	return &delivery{Started: true, Run: run}
}

// deliverToSessionNoKick 是「投递但绝不拉起执行」的变体（人为叫停的回报专用）：
// 目标会话忙则照常插话进它正在执行的那一轮（那是它自己已在跑的一轮，下一次
// 调用模型前注入——不是我们拉起的）；空闲则**只落库**，不占执行权、不起汇报轮。
// 为什么叫停要走它：用户刚把任务停掉，数字助理不该被这条回报自动叫醒——那是
// 它自作主张（重新派发/续跑）的来源；用户下次与它对话时自然看到回报（见
// stop_session_test.go 的验收：回报必须落库、但主对话不得自动起轮）。投递路径
// 仍然唯一（同一把会话锁、同一个 landUserSide/pushPending），与 deliverToSession
// 只差「空闲时是否占住执行权」一件事，不存在第三条旁路。
func (a *App) deliverToSessionNoKick(m *SessionMeta, content, kind, origin string) {
	lock := a.sessLock(m.ID)
	lock.Lock()
	defer lock.Unlock()
	if a.IsRunning(m.ID) {
		a.pushPending(m.ID, "steer", content, kind, origin)
		return
	}
	a.landUserSide(m, content, kind, origin, nil)
}

// subQueueNote 组织「目标会话正在执行，派发需求已插话」时回给模型的文案：
// 模型调用中插话会立即打断其当前输出、下一步就带着这句话重调；工具执行中
// 则等下一步调用模型前注入——两种情况对方都立刻能看到。
func subQueueNote(sub *SessionMeta, note string) string {
	return fmt.Sprintf("已插话：目标会话正在执行——若它正在调用模型（思考/输出中），当前输出会立即被打断、下一步就带着这句话重新调模型；若在执行工具，则等它下一步调用模型前注入。两种情况对方立刻都能看到（会话 %s「%s」）。%s本工具立即返回，不要等待；"+
		"被派发的会话完成后系统会自动收到压缩摘要并触发你处理回报。现在可以先回用户一句「已经安排」。",
		sub.ID, sub.Title, note)
}

// landUserSide 是「用户侧消息落库」的唯一收口：chat（真实用户发言）走
// appendMessage——锚定本轮环境时间、刷新会话活动时间，与用户自己发完全一致
//（chat 带 origin 时——如插话 origin=interject——按同口径落库，只是多存一个
// 来源标记）；aiuser（AI 发出的用户消息）与注入类（report 等）走 appendRow，
// 并按 AGENT.md 的约定显式刷新会话时间（touchesActivity 的口径不含这两类行，
// 不能去改它）。落库后广播同型 message 事件：emit 非空时走本轮订阅通道
//（执行体内部），否则走全局广播。
func (a *App) landUserSide(m *SessionMeta, content, kind, origin string, emit func(Event)) *StoredMessage {
	if kind == "" {
		kind = kindChat
	}
	var sm *StoredMessage
	if kind == kindChat && origin == "" {
		sm = a.appendMessage(m, TextMessage("user", content))
	} else if kind == kindChat {
		sm = &StoredMessage{Message: TextMessage("user", content), Kind: kindChat, Origin: origin, CreatedAt: now()}
		_ = a.Store.AppendRow(m.ID, sm)
		if sm.touchesActivity() {
			m.UpdatedAt = now()
		}
		_ = a.Store.TouchRunMeta(m) // 与 appendMessage 同口径回写运行列
	} else {
		sm = a.appendRow(m, StoredMessage{Message: TextMessage("user", content), Kind: kind, Origin: origin, CreatedAt: now()})
		m.UpdatedAt = now()
		_ = a.Store.SaveMeta(m)
	}
	ev := Event{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kind, Origin: origin}
	if emit != nil {
		emit(ev)
	} else {
		a.broadcast(ev)
	}
	return sm
}

// drainSteer 取走并清空会话的插话队列（agent 循环步间排水）。
func (a *App) drainSteer(sessionID string) []*PendingMsg {
	a.pendMu.Lock()
	q := a.pendingSteer[sessionID]
	delete(a.pendingSteer, sessionID)
	a.pendMu.Unlock()
	return q
}

// popQueue 取走排队队列里的第一条（本轮结束后串行发送）。
func (a *App) popQueue(sessionID string) *PendingMsg {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	q := a.pendingQueue[sessionID]
	if len(q) == 0 {
		return nil
	}
	first := q[0]
	if len(q) == 1 {
		delete(a.pendingQueue, sessionID)
	} else {
		a.pendingQueue[sessionID] = q[1:]
	}
	return first
}

// requeueSteer 把本轮没来得及注入的插话转为排队（不丢消息，下一轮生效）。
func (a *App) requeueSteer(sessionID string) {
	left := a.drainSteer(sessionID)
	if len(left) == 0 {
		return
	}
	a.pendMu.Lock()
	a.pendingQueue[sessionID] = append(a.pendingQueue[sessionID], left...)
	a.pendMu.Unlock()
	a.broadcastPending(sessionID)
}

// pendingOf 取会话当前的待生效消息快照（永不返回 nil，前端可直接取用）。
func (a *App) pendingOf(sessionID string) *PendingState {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	st := &PendingState{Steer: []*PendingMsg{}, Queue: []*PendingMsg{}}
	st.Paused = a.queuePaused[sessionID]
	st.Steer = append(st.Steer, a.pendingSteer[sessionID]...)
	st.Queue = append(st.Queue, a.pendingQueue[sessionID]...)
	return st
}

// updatePending 处理排队坞上的操作：移除，或把排队项提前为插话。
func (a *App) updatePending(sessionID, msgID, action string) bool {
	a.pendMu.Lock()
	moved := (*PendingMsg)(nil)
	found := false
	for i, pm := range a.pendingQueue[sessionID] {
		if pm.ID != msgID {
			continue
		}
		found = true
		a.pendingQueue[sessionID] = append(a.pendingQueue[sessionID][:i], a.pendingQueue[sessionID][i+1:]...)
		if len(a.pendingQueue[sessionID]) == 0 {
			delete(a.pendingQueue, sessionID)
		}
		if action == "steer" {
			if pm.Origin == "" {
				pm.Origin = "interject" // 排队项提前为插话：同样打插话标记（派发注入的 origin 非空，不动）
			}
			moved = pm
		}
		break
	}
	if !found {
		for i, pm := range a.pendingSteer[sessionID] {
			if pm.ID != msgID {
				continue
			}
			found = true
			a.pendingSteer[sessionID] = append(a.pendingSteer[sessionID][:i], a.pendingSteer[sessionID][i+1:]...)
			if len(a.pendingSteer[sessionID]) == 0 {
				delete(a.pendingSteer, sessionID)
			}
			break
		}
	}
	if moved != nil {
		a.pendingSteer[sessionID] = append(a.pendingSteer[sessionID], moved)
	}
	a.pendMu.Unlock()
	if found {
		a.broadcastPending(sessionID)
	}
	if moved != nil {
		a.kickModelStream(sessionID) // 提前为插话：与直接插话同款——模型调用中立见生效
	}
	return found
}

// movePending 拖拽排序：把排队项 id 移到 before 之前（before 空 = 移到队尾）。
// 只调整排队队列的内部顺序；id 或锚点不在队列里（可能刚被消费）返回 false，
// 顺序保持不动，由前端如实提示。
func (a *App) movePending(sessionID, id, before string) bool {
	a.pendMu.Lock()
	list := a.pendingQueue[sessionID]
	from := -1
	for i, pm := range list {
		if pm.ID == id {
			from = i
			break
		}
	}
	if from < 0 {
		a.pendMu.Unlock()
		return false
	}
	if before == id { // 放回原位：不动
		a.pendMu.Unlock()
		return true
	}
	to := len(list) // 默认队尾
	if before != "" {
		found := -1
		for i, pm := range list {
			if pm.ID == before {
				found = i
				break
			}
		}
		if found < 0 {
			a.pendMu.Unlock()
			return false
		}
		to = found
	}
	pm := list[from]
	list = append(list[:from], list[from+1:]...)
	if to > from {
		to-- // 挖掉自身后，其后的锚点下标前移一位
	}
	list = append(list[:to], append([]*PendingMsg{pm}, list[to:]...)...)
	a.pendingQueue[sessionID] = list
	a.pendMu.Unlock()
	a.broadcastPending(sessionID)
	return true
}

// setQueuePaused 暂停/恢复会话队列的自动发送。暂停后本轮照常跑完，排队消息
// 保留不自动续发（新来且会话空闲的也只排队不拉起）；状态随 pending 快照广播。
func (a *App) setQueuePaused(sessionID string, paused bool) {
	a.pendMu.Lock()
	a.queuePaused[sessionID] = paused
	a.pendMu.Unlock()
	a.broadcastPending(sessionID)
}

// queuePausedOf 会话队列当前是否暂停（排水循环每条消息前都来问一次，
// 暂停开关在执行中切换也能即时生效）。
func (a *App) queuePausedOf(sessionID string) bool {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	return a.queuePaused[sessionID]
}

// dropPending 丢掉会话的全部待生效消息（插话与排队）并复位队列暂停标志，
// 随后广播一份空快照——前端排队坞立即清空。用于清空主对话（历史都没了，
// 挂在旧历史上下文上的待发消息也不该再发）。调用方须保证会话不在执行中。
func (a *App) dropPending(sessionID string) {
	a.pendMu.Lock()
	delete(a.pendingSteer, sessionID)
	delete(a.pendingQueue, sessionID)
	delete(a.queuePaused, sessionID)
	a.pendMu.Unlock()
	a.broadcastPending(sessionID)
}

// broadcastPending 把待生效消息的最新快照推给所有在线前端（多窗口/刷新后一致）。
func (a *App) broadcastPending(sessionID string) {
	a.broadcast(Event{Type: "pending", SessionID: sessionID, Pending: a.pendingOf(sessionID)})
}

func (a *App) subscribe() (chan Event, func()) {
	ch := make(chan Event, 64)
	a.evtMu.Lock()
	a.subscribers[ch] = true
	a.evtMu.Unlock()
	return ch, func() {
		a.evtMu.Lock()
		delete(a.subscribers, ch)
		a.evtMu.Unlock()
	}
}

// maxQueueDrainRounds 是单次收尾里自动续发排队消息的条数上限（防循环风暴）：
// 排队消息每条只被消费一次，正常达不到这个数；真撞上了就停下，把剩下的留在
// 队列里由用户点「继续发送」（不静默丢消息）。
const maxQueueDrainRounds = 20

// hasQueued 会话排队队列里是否还有待发消息（拉起与收口兜底判定用）。
func (a *App) hasQueued(sessionID string) bool {
	a.pendMu.Lock()
	defer a.pendMu.Unlock()
	return len(a.pendingQueue[sessionID]) > 0
}

// markCleanEnd / endedCleanly 记「本轮是否正常收尾」（没出错、没被用户停止）：
// 收口（finishRun）据此决定要不要兜底续发队列——出错与用户停止后队列照旧保留，
// 留给用户处置（既有的明确口径，不改）。
func (r *sessionRun) markCleanEnd() {
	r.mu.Lock()
	r.cleanEnd = true
	r.mu.Unlock()
}

func (r *sessionRun) endedCleanly() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.cleanEnd
}

// queueAutoSendOn 是「执行结束后自动续发排队消息」开关（设置·常规，默认开）。
// 关掉后本轮结束不自动发队列，排队坞给「继续发送」入口（点它照发）。
func (a *App) queueAutoSendOn() bool {
	return a.Cfg.queueAutoSendOn()
}

// drainQueue 是排队消息的统一续发口：**不分来源**——真人消息（chat）、派发
// 回报/派发需求（aiuser）、系统注入（report）走的是同一个队列、同一条规则。
// 会话被停止/出错、队列暂停、开关关闭都停下，剩下的留在队列里（前端排队坞给
// 「继续发送」入口）；auto=false 表示「用户显式要求把队列发完」（点继续发送、
// 空闲时新到消息的拉起），不受开关约束。
func (a *App) drainQueue(sess *SessionMeta, run *sessionRun, emit func(Event), auto bool) error {
	err := error(nil)
	for sent := 0; ; sent++ {
		if run.ctx.Err() != nil || a.queuePausedOf(sess.ID) {
			break
		}
		if auto && !a.queueAutoSendOn() {
			break // 开关关闭：本轮结束不自动续发，留给用户点「继续发送」
		}
		if sent >= maxQueueDrainRounds {
			log.Printf("[排队] 会话 %s 单次收尾已自动续发 %d 条，暂停自动推进（剩余消息留在队列里）", sess.ID, sent)
			break
		}
		pm := a.popQueue(sess.ID)
		if pm == nil {
			break
		}
		a.broadcastPending(sess.ID)
		a.landUserSide(sess, pm.Content, pm.Kind, pm.Origin, emit)
		if err = a.runGoalRounds(run.ctx, sess, emit); err != nil {
			break
		}
		a.requeueSteer(sess.ID) // 本轮没来得及注入的插话转为排队，接着发
	}
	a.broadcastPending(sess.ID)
	return err
}

// runRoundsWithQueue 是「一轮执行 + 排队续发」的统一收口：会话执行体
// （executeRun）与派发任务执行体（runSubAgent）共用，两条路径的续发口径因此
// 完全一致。opening=false 表示这次拉起没有新正文（空闲时把队列发起来，见
// kickQueue）：此时若队列里已有待发消息就直接续发——先空跑一轮模型调用既白花
// 一次调用，也不是用户点「继续发送」的意思。
func (a *App) runRoundsWithQueue(sess *SessionMeta, run *sessionRun, emit func(Event), opening, auto bool) error {
	if opening || !a.hasQueued(sess.ID) {
		err := a.runGoalRounds(run.ctx, sess, emit)
		a.requeueSteer(sess.ID) // 本轮结束仍没被注入的插话转为排队：作为独立一轮接着发
		if err != nil {
			a.broadcastPending(sess.ID)
			return err
		}
	}
	return a.drainQueue(sess, run, emit, auto)
}

// maxGoalRounds 是目标模式自动续跑的轮数上限（防止无限消耗）。
const maxGoalRounds = 8

// runGoalRounds 是目标模式的续跑驱动：跑一轮 RunAgent；轮末若目标仍
// active 且未达上限，注入一条 goal_round 消息自动开下一轮，直到模型调用
// update_goal 标记 complete/blocked、达到上限、出错或被取消。
// 未开目标模式时就是普通的一轮 RunAgent。
func (a *App) runGoalRounds(ctx context.Context, sess *SessionMeta, emit func(Event)) error {
	for round := 0; ; round++ {
		err := a.RunAgent(ctx, sess, emit)
		if err != nil {
			return err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !sess.GoalMode || sess.GoalStatus != "active" {
			return nil
		}
		if round+1 >= maxGoalRounds {
			sm := a.appendRow(sess, StoredMessage{
				Message: TextMessage("user", "【目标续跑】已达到自动续跑上限（"+itoa(maxGoalRounds)+" 轮），暂停自动推进；目标尚未标记完成。可补充指示后继续。"),
				Kind:    kindReport, Origin: "goal", CreatedAt: now(),
			})
			emit(Event{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kindReport, Origin: "goal"})
			return nil
		}
		sm := a.appendRow(sess, StoredMessage{
			Message: TextMessage("user", "<goal_round> 继续推进当前目标（第 "+itoa(round+2)+" 轮）。若已达成或确认无法推进，调用 update_goal 汇报并写最终总结；否则继续执行下一步，保持进度、不要重复已完成的步骤。"),
			Kind:    kindReport, Origin: "goal", CreatedAt: now(),
		})
		emit(Event{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kindReport, Origin: "goal"})
	}
}

func itoa(n int) string {
	return fmt.Sprintf("%d", n)
}

// executeRun 是会话执行体（在独立 goroutine 里跑，与发起它的 HTTP 请求无关）：
// 先把 opening 事件发出去（用户消息/标题），再跑当前这一轮（含目标模式续跑），
// 然后把排队消息逐条作为独立一轮发出，最后给出明确的终态交代。
// 异常结束一律落一条可见记录并写日志——绝不出现「模型没输出、对话就停在
// 半路」的静默停止。
func (a *App) executeRun(m *SessionMeta, run *sessionRun, opening []Event) {
	defer a.finishRunOf(m.ID, run)
	emit := func(ev Event) {
		ev.SessionID = m.ID
		run.observe(ev)
		run.publish(ev)
		// 只在「消息级」里程碑广播会话更新（前端据此刷侧栏/列表）；
		// delta/reasoning 这类高频事件不广播，否则会打出一串无效请求
		switch ev.Type {
		case "delta", "reasoning", "tool_start", "tool_end", "tool_wait", "resume", "pending", "retry", "auto_retry":
		case "done", "stopped":
			// 终态原样进全局总线：前端据此点亮「完成未查看」绿点并刷新侧栏
			a.broadcast(ev)
		default:
			a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
		}
	}
	defer func() { // 执行体自身出问题时也要留下痕迹，不能静默消失
		if p := recover(); p != nil {
			log.Printf("[对话] 会话 %s 执行体异常: %v", m.ID, p)
			emit(Event{Type: "error", Error: fmt.Sprintf("执行体异常: %v", p)})
			a.auditWrite(m, map[string]any{"type": "error", "error": fmt.Sprintf("执行体异常: %v", p)})
		}
	}()
	for _, ev := range opening {
		emit(ev)
	}

	// 本轮 + 排队续发（统一收口，见 runRoundsWithQueue）：opening 非空＝真人发
	// 消息/重试，这一轮收尾后的续发是「自动续发」，受设置开关约束；opening 为空
	// ＝空闲时把队列发起来（kickQueue：点「继续发送」/空闲时新到的消息），那是
	// 用户显式要求，发完为止，不看开关。排队消息**不分来源**（真人 / 派发回报 /
	// 系统注入）一律照发；停止、出错、队列暂停后余下的才留在队列里（留给用户处置）。
	err := a.runRoundsWithQueue(m, run, emit, len(opening) > 0, len(opening) > 0)
	if err == nil {
		run.markCleanEnd() // 正常收尾：收口时兜底续发窗口期内新挤进队列的消息
	}
	// 停止看门狗已强制终结过这一轮（执行体卡死、迟到才醒）：终态交代全部跳过
	// ——看门狗已代为收口（置空闲、广播停止事件），这里再写库/发事件只会污染
	// 可能已经开始的新一轮。留一行日志即可。
	if run.wasKilled() {
		log.Printf("[对话] 会话 %s 的执行体在硬终结后才醒来，终态交代由停止看门狗代完成，跳过", m.ID)
		return
	}

	// 终态：正常完成与用户停止都不往界面塞话（对话里只有真实对话）；
	// 只有真实的失败（模型/网络/费用等）才进前端的报错区，并带重试按钮。
	// 三种终态都在审计日志留痕（导出日志时间线可见，不发给模型）；
	// 失败同时给会话打上「异常中断」标记——左侧栏把时间位换成「⚠ 已中断」，
	// 用户没看着这个会话也能一眼发现这轮没跑完（打开会话不清，重跑一轮即清）。
	hookStatus, hookErr := "done", ""
	switch {
	case err == nil:
		_ = a.Store.SetLastStop(m.ID, "")
		emit(Event{Type: "done"})
	case a.isStopped(run) || run.ctx.Err() != nil:
		hookStatus = "stopped"
		log.Printf("[对话] 会话 %s 已被用户停止", m.ID)
		a.clearRunError(m.ID)
		_ = a.Store.SetLastStop(m.ID, "") // 用户主动停止不算异常，不留标记
		a.auditWrite(m, map[string]any{"type": "stop"})
		emit(Event{Type: "stopped"})
	default:
		hookStatus = "error"
		hookErr = err.Error()
		log.Printf("[对话] 会话 %s 本轮结束于错误: %v", m.ID, err)
		a.setRunError(m.ID, err.Error())
		_ = a.Store.SetLastStop(m.ID, stopKindOf(err))
		a.auditWrite(m, map[string]any{"type": "error", "error": err.Error()})
		emit(Event{Type: "error", Error: err.Error()})
	}
	// 组件钩子通知：run.end（通知型）——一轮执行结束（含排队续跑全部完成），
	// 订阅组件的 worker 收到事件帧即止，主流程不等应答（emitComponentHooks）
	a.emitComponentHooks("run.end", map[string]any{
		"sessionID": m.ID,
		"status":    hookStatus,
		"error":     hookErr,
	})
	emit(Event{Type: "session_updated"})
}

// setRunError 记下最近一次失败原因（前端在输入框上方显示报错条 + 重试按钮）。
func (a *App) setRunError(id, msg string) {
	a.errMu.Lock()
	a.runErrors[id] = msg
	a.errMu.Unlock()
}

// clearRunError 清掉报错（新一轮开始/重试成功即消失）。
func (a *App) clearRunError(id string) {
	a.errMu.Lock()
	delete(a.runErrors, id)
	a.errMu.Unlock()
}

// runError 取会话最近一次失败原因；没有则返回空串。
func (a *App) runError(id string) string {
	a.errMu.Lock()
	defer a.errMu.Unlock()
	return a.runErrors[id]
}

// stopKindOf 把本轮失败归类成侧栏标记用的类型码（见 Store.SetLastStop）：
// 达到步数上限是「按规则自动收手」，与真正的执行失败（网关/网络/费用等）文案不同，
// 所以在这里分辨一次，前端只管按码取话术。
func stopKindOf(err error) string {
	var mse errMaxSteps
	if errors.As(err, &mse) {
		return stopMaxStep
	}
	return stopError
}

// isStopped 判断本次执行是否由用户显式停止。
func (a *App) isStopped(run *sessionRun) bool {
	run.mu.Lock()
	defer run.mu.Unlock()
	return run.stopped
}

// markKilled 标记这一轮已被停止看门狗强制终结（幂等，返回是否首次标记）。
func (r *sessionRun) markKilled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	first := !r.killed
	r.killed = true
	return first
}

// wasKilled 判断这一轮是否已被看门狗强制终结（执行体据此跳过迟到的终态交代）。
func (r *sessionRun) wasKilled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.killed
}

// dispatchOwner 取会话的派发方（先看派发链 dispatched_by，退回父会话）：被派发
// 会话跑完要回报给它。重启恢复（runowner.go）与收口续发（server.go 的
// serveQueue）共用这一处口径，不各写一份。
func (a *App) dispatchOwner(s *SessionMeta) *SessionMeta {
	if s == nil {
		return nil
	}
	for _, id := range []string{s.DispatchedBy, s.ParentID} {
		if id == "" {
			continue
		}
		if m, err := a.Store.Get(id); err == nil {
			return m
		}
	}
	return nil
}

// runSubAgent 是派发子任务的后台执行体：执行 → 广播进度 → 回报主对话 →
// 主对话空闲时自动跑一轮面向用户的汇报。run 由派发方通过 beginRun 登记后传入。
func (a *App) runSubAgent(parent *SessionMeta, sub *SessionMeta, run *sessionRun) {
	defer a.finishRunOf(sub.ID, run)
	a.broadcast(Event{Type: "sub_start", SessionID: sub.ID, Title: sub.Title})
	emit := func(ev Event) {
		if ev.Type == "tool_start" || ev.Type == "tool_end" {
			a.broadcast(Event{Type: "sub_tool", SessionID: sub.ID, Name: ev.Name, Args: ev.Args, Output: ev.Output})
		}
		ev.SessionID = sub.ID
		run.observe(ev)
		run.publish(ev)
	}
	// 本轮 + 排队续发（统一收口）：任务会话执行期间收到的派发需求 / 用户消息
	// 与本轮跑完一起处理，同一个队列、同一条规则——不再有「排在队列里等用户
	// 点继续发送」这一说。
	err := a.runRoundsWithQueue(sub, run, emit, true, a.queueAutoSendOn())
	if err == nil {
		run.markCleanEnd() // 正常收尾：收口时兜底续发窗口期内新挤进队列的消息
	}
	// 硬终结后迟到醒来：不做任何交代（interrupt 报告、派发回报、sub_end 广播都
	// 不再发——那一刻可能已有新一轮在跑），收口已由停止看门狗代完成。
	if run.wasKilled() {
		log.Printf("[派发] 任务会话 %s 的执行体在硬终结后才醒来，跳过回报与交代", sub.ID)
		return
	}
	// 用户手动停止（POST /stop → stopRun 取消 ctx）与真实的执行失败要分开交代：
	// 停止不是异常——别把 "context canceled" 当错误抛给用户/模型，也别让数字助理
	// 以为任务是被意外打断的（那正是它自作主张重新派发/续跑的来源）。
	stopped := a.isStopped(run) || run.ctx.Err() != nil
	if err != nil {
		if stopped {
			// 触发者照落库的停止记录写：人工点按钮＝「用户点击停止按钮」，
			// 数字助理按用户指示代停＝「数字助理按用户指示代停…」（见 stopRunBy）
			rec := a.Store.GetStopRecord(sub.ID)
			a.broadcast(Event{Type: "sub_end", SessionID: sub.ID, Error: "已由用户手动停止"})
			_ = a.Store.AppendRow(sub.ID, &StoredMessage{
				Message: TextMessage("user", "【执行中断】本轮已按用户要求停止（"+stopTriggerNote(rec.Actor)+"）。未完成的部分不会自动续跑，需要时发新消息即可接着做。"),
				Kind:    kindReport, Origin: "interrupt", CreatedAt: now(),
			})
		} else {
			a.broadcast(Event{Type: "sub_end", SessionID: sub.ID, Error: err.Error()})
			_ = a.Store.AppendRow(sub.ID, &StoredMessage{
				Message: TextMessage("user", "【执行中断】上一轮执行中断："+err.Error()+"。可发「继续」接续。"),
				Kind:    kindReport, Origin: "interrupt", CreatedAt: now(),
			})
		}
	} else {
		a.broadcast(Event{Type: "sub_end", SessionID: sub.ID})
	}
	a.broadcast(Event{Type: "session_updated", SessionID: sub.ID})
	if parent == nil {
		return
	}
	rows, _ := a.Store.MessagesWithSeq(sub.ID)
	summary := renderSummary(a, sub, rows, stopped || err != nil) // 回报体例：完整最终答复 + 结尾消息 + 折叠过程 + 核对过的改动文件
	// 回报指令按父会话形态分流：数字助理向用户汇报；工作任务带着结果继续推进
	followUp := "请用用户听得懂的话汇报这件事的结果。"
	if parent.Mode != "assistant" {
		followUp = "请结合这个结果继续推进当前任务；若已全部完成，给出最终答复。"
	}
	// 回报首句 = 中断原因：用户停止与服务重启/故障中断必须一眼分得开（见
	// runowner.go 的重启回报，两处口径对齐）
	head := "【派发回报】任务会话「" + sub.Title + "」（" + sub.ID + "）本轮结束。"
	if stopped {
		rec := a.Store.GetStopRecord(sub.ID)
		at := strings.TrimSpace(rec.At)
		if at == "" {
			at = now()
		}
		// 进度与下方摘要同口径（同一批消息算出来的），所以两处数字必然一致。
		// 首句措辞（含触发者口径）由 stopReportHead 统一组织：人工点按钮与数字助理
		// 按用户指示代停都要写成「已由用户手动停止」——上游提示词与重启恢复都按这句
		// 话认「人为叫停」，不能被当成意外中断。
		rounds, steps := a.sessionProgress(sub.ID)
		head = stopReportHead(sub, rec, at, rounds, steps)
		followUp = "这是用户主动叫停，不是故障：不要再自动重新派发、续跑或补做这个任务，用一句话向用户确认已停止即可。"
	}
	reportText := head + "\n\n" + summary + "\n\n" + followUp
	// 桥派子任务（run_python 脚本经 jx.dispatch 派的）：回报定向存给脚本的
	// jx.wait 收取，不投父会话、不起汇报轮——脚本结束后统一经【后台脚本回报】
	// 或转交投递到达，主对话只被打扰一次（bridge.go）。
	if a.bridgeInterceptReport(sub, reportText) {
		a.broadcast(Event{Type: "session_updated", SessionID: sub.ID})
		return
	}
	// 回报的投递路径仍然统一在 deliverToSession 一族（按「用户侧消息」投，父会话
	// 空闲就落库并起汇报轮；正被占用就插话注入它正在执行的那一轮——AI 发话
	// 一律插话，不排队）：正常结束与失败都走 deliverToSession；唯一例外是人为
	// 叫停——改走 NoKick 变体（见下），差别只在「空闲时是否拉起汇报轮」，
	// 落库/插话的行为完全一致，不存在直写旁路。
	// 广播回报就绪：前端收到 report_ready 重查会话（正常路径投递即起轮，必见
	// running、能接上这轮流；叫停路径本轮不拉起，前端只会看到未读绿点）。

	// 投递方式按收尾形态分：人为叫停（stopped）走 deliverToSessionNoKick——回报
	// 落库/插话，但绝不自动拉起父会话那一轮（用户刚叫停，数字助理被自动叫醒正是
	// 它自作主张的来源）；正常完成/失败仍走 deliverToSession，父空闲即起汇报轮。
	var d *delivery
	if stopped {
		a.deliverToSessionNoKick(parent, reportText, kindAiUser, "dispatch")
		d = &delivery{}
	} else {
		d = a.deliverToSession(parent, reportText, kindAiUser, "dispatch")
	}
	a.broadcast(Event{Type: "report_ready", SessionID: parent.ID, Title: sub.Title})
	if d.Started {
		go func() {
			prun := d.Run
			defer a.finishRunOf(parent.ID, prun)
			prun.publish(Event{Type: "run_start"})
			err := a.runRoundsWithQueue(parent, prun, func(ev Event) {
				ev.SessionID = parent.ID
				prun.observe(ev)
				prun.publish(ev)
			}, true, a.queueAutoSendOn())
			if err == nil {
				prun.markCleanEnd() // 正常收尾：收口时兜底续发窗口期内新挤进队列的消息
			}
			a.broadcast(Event{Type: "session_updated", SessionID: parent.ID})
			a.broadcast(Event{Type: "done", SessionID: parent.ID}) // 汇报轮结束：未查看的会话点亮绿点
		}()
	}
}

// repairDanglingToolCalls 扫描会话历史，为「assistant 发起了、但没有任何
// tool 结果」的悬空调用补一条合成结尾行（对齐 DSH 的「每个 tool_call 都必须
// 有结果，回放才不失效」）。正常运行路径上工具循环已保证每个调用都有结尾
// （含中断时刻的合成结尾）；这里兜底的是异常路径：进程崩溃/服务重启落在
// 「调用已落库、结果未落库」窗口的调用。幂等——已补齐的不再补。
// emit 可用时把补的行实时下发给前端；启动恢复等无前端场景传 nil。
func (a *App) repairDanglingToolCalls(sess *SessionMeta, emit func(Event)) {
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return
	}
	answered := map[string]bool{}
	for _, m := range raw {
		if m.Role == "tool" && m.ToolCallID != "" {
			answered[m.ToolCallID] = true
		}
	}
	for _, m := range raw {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 || m.Kind != kindChat {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.ID == "" || answered[tc.ID] {
				continue
			}
			sm := a.appendRow(sess, StoredMessage{
				Message: Message{Role: "tool", Content: interruptNoteNotRun, ToolCallID: tc.ID},
				Kind:    kindChat, Origin: "interrupt", CreatedAt: now(),
			})
			answered[tc.ID] = true
			if emit != nil {
				emit(Event{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kindChat, Origin: "interrupt"})
			}
		}
	}
}

// recoverInterrupted 服务启动时的中断恢复：把重启前 running 的会话补一条
// 中断记录并置为空闲；被派发的子会话同时给主对话补中断回报。
func (a *App) recoverInterrupted() {
	sessions, err := a.Store.RunningSessions()
	if err != nil {
		return
	}
	for _, s := range sessions {
		// 会话可能正被「另一个活着的实例」执行（桌面版与控制台版共用同一 data
		// 目录是常态，单实例互斥体只挡桌面形态）：那种情况绝不能补中断——
		// 归属里记的 pid 还活着且心跳新鲜，就是别人正在跑，跳过
		if pid, live := a.otherInstanceRunning(s.ID); live {
			log.Printf("[恢复] 会话 %s 正由另一实例（pid %d）执行，跳过中断恢复", s.ID, pid)
			continue
		}
		a.reapInterruptedSession(s) // 收口逻辑收在 runowner.go，30s 补扫共用
	}
}
