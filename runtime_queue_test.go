package main

// 排队消息自动续发（统一规则）+ AI 投递一律插话的最小测试。
//
// 规则：**用户**在执行中发消息才分排队/插话（自己的「对话中途发送行为」设置）；
// **AI 侧投递**（派发回报/转达/求助/待办）执行中一律**插话**——注入对方正在
// 执行的这一轮，下一次调用模型就能看到，不再按偏好排队。排队队列本身的统一
// 规则不变：消息不分来源，进了排队队列就在本轮结束后自动逐条发出。
//
// 覆盖：
//  1. 用户排队消息本轮结束自动续发（不回归）；AI 投递（deliverToSession）
//     执行中一律插话（Mode=steer、进插话队列不进排队队列），插话在续发轮
//     调用模型前被注入落库
//  2. 开关（queueAutoSend）关闭 → 本轮结束不自动发；点「继续发送」仍照发
//  3. 收口兜底：续发循环走完之后才挤进队列的消息，由 finishRun 再拉一轮发出
//  4. 派发回报的唯一投递路径：runSubAgent 在父会话正忙时把回报插进父会话的
//     插话队列（不是另写一条旁路），父会话下一步调用模型即注入
//  5. 插话在下一步调用模型前被注入落库的同一时刻广播空插话快照（排队路径
//     popQueue 后有同一广播），前端的「插话中」气泡随即退场，不挂到本轮收尾
//
// 模型走内置 mock（BaseURL=mock），工具落在临时工作区，不联网、不碰真实工作区。

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// newQueueTestApp 建一个「带临时库 + 临时工作区 + 离线 mock 模型」的 App，
// 并给一个通用工作任务会话（mock 会依次发起 write / run_command /
// deliver_result 三个工具调用后收尾，足以证明「这一轮真的跑了」）。
func newQueueTestApp(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	// 默认模型体系：会话没手选时一律跟全局默认模型
	a.Cfg.DefaultModel = ModelRef{Provider: "mock", Model: "mock-1"}
	// 执行体收尾会向声明式组件广播 run.end：测试里给一个空注册表（真进程由
	// NewApp 建好），免得 nil 指针在收尾路径上打日志
	a.Components = &ComponentRegistry{}
	m := &SessionMeta{ID: "main", Title: "排队测试会话", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return a, m
}

// rowsOf 取会话历史里的用户侧行（role=user）的「正文 + kind + origin」三元组，
// 用来核对排队消息最终以什么身份落库。
func rowsOf(t *testing.T, a *App, sid string) []string {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	out := []string{}
	for _, m := range all {
		if m.Role != "user" {
			continue
		}
		txt, _ := m.Content.(string)
		out = append(out, m.Kind+"|"+m.Origin+"|"+txt)
	}
	return out
}

// hasPrefixRow 历史里是否存在「kind|origin|正文以 text 开头」的用户侧行。
func hasPrefixRow(t *testing.T, a *App, sid, kind, origin, text string) bool {
	t.Helper()
	for _, r := range rowsOf(t, a, sid) {
		if strings.HasPrefix(r, kind+"|"+origin+"|"+text) {
			return true
		}
	}
	return false
}

// waitIdle 等会话跑完（自动续发是后台执行的，测试要等它落地）。
func waitIdle(t *testing.T, a *App, sid string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if !a.IsRunning(sid) {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("会话 %s 在 60 秒内没有跑完", sid)
}

// assistantRows 数一数助手侧行：自动续发真的跑了轮次时它会增长。
func assistantRows(t *testing.T, a *App, sid string) int {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	n := 0
	for _, m := range all {
		if m.Role == "assistant" {
			n++
		}
	}
	return n
}

// 1：用户排队消息自动续发；AI 投递执行中一律插话，插话在续发轮注入落库。
func TestQueueDrainSendsAllSources(t *testing.T) {
	a, main := newQueueTestApp(t)
	// 占住执行权＝「助手正在回话」，随后进来的消息只能排队/插话
	run, ok := a.beginRun(main.ID)
	if !ok {
		t.Fatal("应能占住执行权")
	}
	before := assistantRows(t, a, main.ID)

	// 真人补充要求选「等待排队」（handleInterject mode=queue 的同一落点）
	a.pushPending(main.ID, "queue", "真人的补充要求", kindChat, "")
	// 派发回报（aiuser/dispatch）：AI 投递一律插话——进插话队列，不进排队队列
	if d := a.deliverToSession(main, "【派发回报】任务会话「X」本轮结束。", kindAiUser, "dispatch"); d.Started || d.Mode != "steer" {
		t.Fatalf("执行中 AI 投递应插话, got %+v", d)
	}
	q := a.pendingOf(main.ID).Queue
	if len(q) != 1 {
		t.Fatalf("排队队列里应只有用户那条消息，got %d", len(q))
	}
	if q[0].Kind != kindChat {
		t.Fatalf("排队项身份应原样保留：%+v", q[0])
	}
	st := a.pendingOf(main.ID).Steer
	if len(st) != 1 || st[0].Kind != kindAiUser || st[0].Origin != "dispatch" {
		t.Fatalf("回报应进插话队列且身份原样保留：%+v", st)
	}

	// 本轮收尾：不再空跑一轮（队列里的就是要发的东西），直接逐条自动发出；
	// 发用户排队那条的轮里，插话在调用模型前被注入
	err := a.runRoundsWithQueue(main, run, func(Event) {}, false, true)
	a.finishRun(main.ID)
	if err != nil {
		t.Fatalf("本轮 + 续发应无错误: %v", err)
	}
	if n := len(a.pendingOf(main.ID).Queue); n != 0 {
		t.Fatalf("续发后队列应清空，剩 %d 条（否则界面上会卡「继续发送」）", n)
	}
	if n := len(a.pendingOf(main.ID).Steer); n != 0 {
		t.Fatalf("插话应已注入落库，插话队列剩 %d 条", n)
	}
	if !hasPrefixRow(t, a, main.ID, kindChat, "", "真人的补充要求") {
		t.Fatal("真人消息未按 chat 身份落库")
	}
	if !hasPrefixRow(t, a, main.ID, kindAiUser, "dispatch", "【派发回报】") {
		t.Fatal("派发回报未按 aiuser/dispatch 身份落库")
	}
	if got := assistantRows(t, a, main.ID); got <= before {
		t.Fatalf("排队消息应各自开出独立一轮（助手行 %d → %d）", before, got)
	}
}

// 2：开关关闭＝本轮结束不自动发（留「继续发送」）；显式拉起（点它）仍照发。
func TestQueueAutoSendSwitchOff(t *testing.T) {
	a, main := newQueueTestApp(t)
	off := false
	a.Cfg.QueueAutoSend = &off
	if a.queueAutoSendOn() {
		t.Fatal("关掉后 queueAutoSendOn 应为 false")
	}
	run, _ := a.beginRun(main.ID)
	a.pushPending(main.ID, "queue", "排队的消息", kindChat, "")
	a.pushPending(main.ID, "queue", "【派发回报】回报", kindAiUser, "dispatch")

	if err := a.runRoundsWithQueue(main, run, func(Event) {}, false, true); err != nil {
		t.Fatalf("收尾不应报错: %v", err)
	}
	a.finishRun(main.ID)
	if n := len(a.pendingOf(main.ID).Queue); n != 2 {
		t.Fatalf("开关关闭时队列应原样保留 2 条，got %d", n)
	}
	if hasPrefixRow(t, a, main.ID, kindChat, "", "排队的消息") {
		t.Fatal("开关关闭时不该自动发出")
	}

	// 用户点「继续发送」：显式拉起（不受开关约束）
	a.kickQueue(main)
	waitIdle(t, a, main.ID)
	if n := len(a.pendingOf(main.ID).Queue); n != 0 {
		t.Fatalf("显式拉起后队列应发完，剩 %d 条", n)
	}
	if !hasPrefixRow(t, a, main.ID, kindAiUser, "dispatch", "【派发回报】回报") {
		t.Fatal("显式拉起后回报也未落库")
	}
}

// 3：收口兜底——续发循环走完之后才挤进队列的消息，由 finishRun 再拉一轮。
func TestFinishRunResumesLeftoverQueue(t *testing.T) {
	a, main := newQueueTestApp(t)
	run, _ := a.beginRun(main.ID)
	run.markCleanEnd()
	// 续发循环已经走完（队列那一刻是空的），消息此刻才到：执行权还在，只能排队
	a.pushPending(main.ID, "queue", "窗口期挤进来的消息", kindChat, "")
	a.finishRun(main.ID) // 收口：应自动再拉一轮把队列发出去
	waitIdle(t, a, main.ID)
	if n := len(a.pendingOf(main.ID).Queue); n != 0 {
		t.Fatalf("收口兜底应把队列发完，剩 %d 条", n)
	}
	if !hasPrefixRow(t, a, main.ID, kindChat, "", "窗口期挤进来的消息") {
		t.Fatal("窗口期挤进来的消息没有被续发")
	}
}

// 4：派发回报的唯一投递路径——runSubAgent 把回报投给派发方（父会话正忙就插
// 进它的插话队列），父会话下一步调用模型即注入生效。
func TestSubAgentReportGoesThroughQueue(t *testing.T) {
	a, main := newQueueTestApp(t)
	sub := &SessionMeta{ID: "sub", Title: "被派发的任务", Mode: "task",
		Workspace: t.TempDir(), DispatchedBy: main.ID, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sub); err != nil {
		t.Fatalf("建任务会话失败: %v", err)
	}
	// 派发：目标会话空闲＝落库并起 runSubAgent（与数字助理派发同一条投递）
	d := a.deliverToSession(sub, "【数字助理派发】\n\n做一件小事", kindAiUser, "task")
	if !d.Started {
		t.Fatalf("空闲会话应立刻起执行: %+v", d)
	}
	// 父会话（数字助理）此刻正忙着和用户对话
	mrun, ok := a.beginRun(main.ID)
	if !ok {
		t.Fatal("应能占住父会话执行权")
	}
	go a.runSubAgent(main, sub, d.Run)
	waitIdle(t, a, sub.ID)

	// 回报没有走旁路：它插进了父会话的插话队列，身份照旧 aiuser/dispatch
	st := a.pendingOf(main.ID).Steer
	if len(st) != 1 {
		t.Fatalf("父会话忙时回报应进它的插话队列（1 条），got %d", len(st))
	}
	if st[0].Kind != kindAiUser || st[0].Origin != "dispatch" || !strings.HasPrefix(st[0].Content, "【派发回报】") {
		head := st[0].Content
		if len(head) > 20 {
			head = head[:20]
		}
		t.Fatalf("回报身份不对: kind=%q origin=%q content=%q", st[0].Kind, st[0].Origin, head)
	}

	// 父会话继续自己的这一轮：下一步调用模型前插话被注入（统一规则）
	if err := a.runRoundsWithQueue(main, mrun, func(Event) {}, false, true); err != nil {
		t.Fatalf("父会话收尾不应报错: %v", err)
	}
	a.finishRun(main.ID)
	if n := len(a.pendingOf(main.ID).Steer); n != 0 {
		t.Fatalf("回报应被注入落库，插话队列剩 %d 条", n)
	}
	if !hasPrefixRow(t, a, main.ID, kindAiUser, "dispatch", "【派发回报】") {
		t.Fatal("回报未落到父会话历史")
	}
}

// 5：插话被注入落库的同一时刻必须广播最新 pending 快照（排队路径 popQueue 后有
// 同一广播）：不广播的话前端「插话中」气泡从注入起一直挂到本轮收尾才消失。
// 收口兜底广播也发空快照，所以按时机断言——模型第一个增量输出（注入后的第一
// 个可观测界标）时，订阅流里就应已有「steer 已空」的 pending。
func TestSteerInjectBroadcastsEmptyPending(t *testing.T) {
	a, main := newQueueTestApp(t)
	ch, unsub := a.subscribe()
	defer unsub()

	// 订阅流只收广播事件（本轮的 emit 事件不进它）；扫一遍非阻塞通道
	emptySteerSeen := func() bool {
		for draining := true; draining; {
			select {
			case ev := <-ch:
				if ev.Type == "pending" && ev.SessionID == main.ID && len(ev.Pending.Steer) == 0 {
					return true
				}
			default:
				draining = false
			}
		}
		return false
	}
	clearedAtInject := false
	emit := func(ev Event) {
		if ev.Type == "delta" && !clearedAtInject {
			clearedAtInject = emptySteerSeen()
		}
	}

	run, ok := a.beginRun(main.ID)
	if !ok {
		t.Fatal("应能占住执行权")
	}
	a.pushPending(main.ID, "steer", "执行中插话：请改看方案二", kindChat, "")

	// opening=true：跑一轮 RunAgent，第一步调用模型前插话被注入
	if err := a.runRoundsWithQueue(main, run, emit, true, true); err != nil {
		t.Fatalf("一轮执行应无错误: %v", err)
	}
	a.finishRun(main.ID)
	if n := len(a.pendingOf(main.ID).Steer); n != 0 {
		t.Fatalf("插话应已注入落库，插话队列剩 %d 条", n)
	}
	if !hasPrefixRow(t, a, main.ID, kindChat, "", "执行中插话：") {
		t.Fatal("插话未按 chat 身份落库")
	}
	if !clearedAtInject {
		t.Fatal("插话注入时未广播空插话快照（「插话中」气泡会一直挂到本轮收尾）")
	}
}
