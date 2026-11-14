package main

// 「停止任务」编排工具（stop_session）的验收测试：
//  1. 停一个真在跑的会话：取消真的送到执行体（ctx 取消链路）、停止原因按
//     「数字助理（受用户指示）代停」落库（停止请求已发出而本轮尚未收口时带
//     Manual 标志——重启恢复按人为停止认；收口完成后由 ClearStopPending 清除，
//     停止记录本身保留）、给数字助理的【派发回报】首句写明「已由用户手动停止」
//     并标出触发者，且**不自动拉起**数字助理那一轮；
//  2. 停空闲会话：返回「不在运行」，不留下停止记录（不污染停止原因）；
//  3. 停不存在的 id / 停本会话 / 停另一个总控：拒绝并给明确原因；
//  4. 工具 schema：参数与描述出现在数字助理工具清单里、不泄漏给任务会话，
//     数字助理提示词同步（工具清单 + 使用规则）；
//  5. 回归：人工点「停止」按钮的旧口径不变（触发者仍是用户、文案不变）。
//
// 「在跑」不靠 sleep 抢时机：用一个「模型永不返回」的本机假渠道，请求到达即
// 算跑起来了，取消一律由被测的停止逻辑触发。

import (
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// hangChannel 挂一个「永不返回」的模型渠道：请求打到本机测试服务上就一直挂着，
// 直到客户端取消（ctx 取消 → 客户端断开 → handler 的 r.Context() 结束）。
// 返回渠道 BaseURL 与「请求已到达」的信号通道。每个请求的到达/结束都打日志。
//
// handler 必须先把请求体读完再挂起：真实模型服务器总是先收完请求再处理；
// 更要紧的是「服务端不读 body + 大请求体（派发任务的首轮上下文常有几十 KB）
// + 客户端 ctx 取消」在 Windows 上会让连接关不干净——httptest.Server.Close
// 卡在 active 连接上，整个测试进程挂死（已用最小用例复现锁定）。读完 body
// 即根除，且更贴近真实。
func hangChannel(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	reached := make(chan struct{}, 1)
	var reqSeq int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		i := atomic.AddInt32(&reqSeq, 1)
		_, _ = io.Copy(io.Discard, r.Body)
		log.Printf("[hang] 请求#%d 已读完整请求 %s", i, r.URL.Path)
		select {
		case reached <- struct{}{}:
		default:
		}
		<-r.Context().Done() // 挂着等取消：停止请求一到就随之返回
		log.Printf("[hang] 请求#%d 结束", i)
	}))
	t.Cleanup(srv.Close)
	return srv.URL, reached
}

// mkSession 建一个测试会话（id 固定，便于断言）。
func mkSession(t *testing.T, a *App, id, title, mode, dispatchedBy string) *SessionMeta {
	t.Helper()
	m := &SessionMeta{ID: id, Title: title, Mode: mode, Workspace: a.Cfg.Workspace,
		DispatchedBy: dispatchedBy, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话 %s 失败: %v", id, err)
	}
	return m
}

// ① 停一个真在跑的会话。
func TestStopSessionStopsRunningSession(t *testing.T) {
	a, main := newDispatchTestApp(t)
	base, reached := hangChannel(t)
	a.Cfg.Providers = []ProviderCfg{{ID: "hang", Name: "挂起渠道", BaseURL: base, APIKey: "test-key",
		Models: []ModelCfg{{ID: "hang-1"}}}}
	// 替换渠道时同步默认指向，保证任务实际请求本机假渠道，而非公共夹具的 mock 模型。
	a.Cfg.DefaultModel = ModelRef{Provider: "hang", Model: "hang-1"}
	sub := mkSession(t, a, "t-run", "在跑的任务", "task", main.ID)

	// 与派发工具同一条投递路径把任务跑起来：deliverToSession 占住执行权 → runSubAgent
	d := a.deliverToSession(sub, "【数字助理派发】\n\n做一件很久的事", kindAiUser, "task")
	if !d.Started {
		t.Fatal("任务没有跑起来（deliverToSession 未占住执行权）")
	}
	go a.runSubAgent(main, sub, d.Run)
	select {
	case <-reached:
	case <-time.After(30 * time.Second):
		t.Fatal("任务没有跑到模型调用（假渠道未收到请求）")
	}
	if !a.IsRunning(sub.ID) {
		t.Fatal("此刻会话应在运行中")
	}
	// 取消是否真的送到执行体：等执行体那一侧的 ctx 结束
	canceled := make(chan struct{})
	go func() { <-d.Run.ctx.Done(); close(canceled) }()

	out, err := runStopSession(dispatchCtx(a, main), mustArgs(t, map[string]any{"session_id": sub.ID}))
	if err != nil {
		t.Fatalf("停止失败: %v", err)
	}
	if !strings.Contains(out, "已发出停止请求") || !strings.Contains(out, "协作式") {
		t.Fatalf("停止回报应说明已发出请求且是协作式停止：\n%s", out)
	}
	select {
	case <-canceled:
	case <-time.After(15 * time.Second):
		t.Fatal("执行体没有收到取消信号")
	}
	// 停止请求已发出、本轮**尚未收口**（协作式：执行体还在收尾）：此刻的停止
	// 记录必须是「人为叫停 + 数字助理触发 + Manual（重启恢复按人为停止认）」。
	// 这个检查点放在收口轮询之前——收口一完成 finishRun 就会 ClearStopPending
	// 把 Manual 清掉（那是下一轮的语义），收口后再读 Manual=true 永远靠竞态。
	rec := a.Store.GetStopRecord(sub.ID)
	if rec.Kind != stopByUser || rec.Actor != stopActorAssistant {
		t.Fatalf("停止记录应是「人为叫停 + 数字助理触发」, got %+v", rec)
	}
	if !rec.Manual {
		t.Fatalf("停止请求已发出而本轮尚未收口时应带 Manual 标志（重启恢复按人为停止认）: %+v", rec)
	}
	if !strings.Contains(rec.Note, "数字助理") {
		t.Fatalf("停止说明应写明是数字助理代停：%q", rec.Note)
	}
	// 执行体收到取消后自行收尾（协作式：不靠第二次调用、也不硬杀）
	deadline := time.Now().Add(30 * time.Second)
	for a.IsRunning(sub.ID) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if a.IsRunning(sub.ID) {
		t.Fatal("会话没有在收到取消后收尾")
	}
	// 收口完成后：停止记录本身保留（历史可查），Manual 由 finishRun 里的
	// ClearStopPending 清除——「用户已请求停止」只在本轮未收口时算 pending。
	// ClearStopPending 在 IsRunning 变 false 之后几步才执行（同一个 finishRun
	// 的后半段），这里轮询等它落地，不跟调度赛跑。
	deadlineClear := time.Now().Add(5 * time.Second)
	for {
		rec = a.Store.GetStopRecord(sub.ID)
		if rec.Kind != stopByUser || rec.Actor != stopActorAssistant {
			t.Fatalf("收口后停止记录应保留（人为叫停 + 数字助理触发）, got %+v", rec)
		}
		if !rec.Manual {
			break
		}
		if time.Now().After(deadlineClear) {
			t.Fatalf("收口完成后 Manual 应已清除（ClearStopPending：停止请求只在本轮未收口时算 pending）: %+v", rec)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// 回报口径：数字助理收到一条【派发回报】，首句必须保留「已由用户手动停止」
	//（上游提示词按这句话认人为叫停），并把触发者与次数标记写清
	rows, err := a.Store.RawMessagesAll(main.ID)
	if err != nil {
		t.Fatal(err)
	}
	report := ""
	for _, m := range rows {
		if m.Kind == kindAiUser && strings.Contains(m.Text(), "【派发回报】") {
			report = m.Text()
		}
	}
	if report == "" {
		t.Fatalf("数字助理应收到【派发回报】（当前 %d 条消息）", len(rows))
	}
	for _, want := range []string{"已由用户手动停止", "触发者：数字助理（受用户指示）",
		"不要再自动重新派发", "不是异常中断"} {
		if !strings.Contains(report, want) {
			t.Fatalf("回报缺 %q：\n%s", want, report)
		}
	}
	if strings.Contains(report, "触发者：用户｜") {
		t.Fatalf("数字助理代停不该被标成用户亲自点的停止按钮：\n%s", report)
	}
	// 停掉的是「用户叫停」：不自动拉起数字助理那一轮（那正是助理自作主张的来源）
	if a.IsRunning(main.ID) {
		t.Fatal("人为叫停后的回报不该自动起数字助理那一轮")
	}
}

// ② 停空闲会话：返回「不在运行」，且不留下停止记录。
func TestStopSessionIdleSession(t *testing.T) {
	a, main := newDispatchTestApp(t)
	idle := mkSession(t, a, "t-idle", "闲着的任务", "task", "")

	out, err := runStopSession(dispatchCtx(a, main), mustArgs(t, map[string]any{"session_id": idle.ID}))
	if err != nil {
		t.Fatalf("停空闲会话不该报错: %v", err)
	}
	if !strings.Contains(out, "不在运行") || !strings.Contains(out, "无需停止") {
		t.Fatalf("空闲会话应回报「不在运行」，got：\n%s", out)
	}
	if rec := a.Store.GetStopRecord(idle.ID); rec.Kind != "" {
		t.Fatalf("空闲会话不该留下停止记录（否则重启恢复会误以为它被叫停过）: %+v", rec)
	}
}

// ③ 不该停的目标：不存在的 id、本会话（数字助理自己）、另一个总控会话。
func TestStopSessionRefusesBadTargets(t *testing.T) {
	a, main := newDispatchTestApp(t)
	mkSession(t, a, "a2", "另一个总控", "assistant", "")
	ctx := dispatchCtx(a, main)

	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"id 为空", map[string]any{"session_id": "  "}, "session_id 不能为空"},
		{"id 不存在", map[string]any{"session_id": "nope"}, "不存在"},
		{"停本会话", map[string]any{"session_id": main.ID}, "不能停本会话"},
		{"停另一个总控", map[string]any{"session_id": "a2"}, "总控"},
	}
	for _, tc := range cases {
		_, err := runStopSession(ctx, mustArgs(t, tc.args))
		if err == nil {
			t.Fatalf("%s：应被拒绝", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s：报错应含 %q，got %v", tc.name, tc.want, err)
		}
	}
	// 被拒绝的调用不该在任何一个会话上留下停止记录
	for _, id := range []string{main.ID, "a2", "nope"} {
		if rec := a.Store.GetStopRecord(id); rec.Kind != "" {
			t.Fatalf("被拒绝的停止不该留记录（%s）: %+v", id, rec)
		}
	}
}

// ④ 工具 schema 与提示词同步。
func TestStopSessionToolSchemaAndPrompt(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	names := toolNames(t, a, "assistant")
	if !names["stop_session"] {
		t.Fatal("数字助理的工具清单里应有 stop_session")
	}
	// 停止是总控的编排手段：任务会话自己不该有（它停不了别人，也不该自己掐自己）
	if toolNames(t, a, "task")["stop_session"] {
		t.Fatal("任务会话不该有 stop_session")
	}

	tl := stopSessionTool()
	props, _ := tl.Spec.Function.Parameters["properties"].(map[string]any)
	p, ok := props["session_id"].(map[string]any)
	if !ok || p["type"] != "string" {
		t.Fatalf("stop_session 应有 string 型 session_id 参数，got %+v", props)
	}
	req, _ := tl.Spec.Function.Parameters["required"].([]string)
	if len(req) != 1 || req[0] != "session_id" {
		t.Fatalf("session_id 应是唯一必填参数，got %+v", req)
	}
	if len(props) != 1 {
		t.Fatalf("stop_session 只该有 session_id 一个参数，got %d 个", len(props))
	}
	desc := tl.Spec.Function.Description
	for _, want := range []string{"停止", "协作式", "已由用户手动停止", "数字助理（受用户指示）", "不要再重派"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("工具描述缺要点 %q：\n%s", want, desc)
		}
	}
	// 系统提示词：工具清单加一条 + 一条使用规则（仅按用户指示停；停完告知用户）
	sys := a.systemPrompt(&SessionMeta{Mode: "assistant"})
	if !strings.Contains(sys, "- stop_session：") {
		t.Fatalf("数字助理提示词的工具清单里应有 stop_session：\n%s", sys)
	}
	if !strings.Contains(sys, "只在用户明确要求时停") {
		t.Fatalf("数字助理提示词应写明「只在用户明确要求时停」：\n%s", sys)
	}
}

// ⑤ 回归：人工点「停止」按钮的旧口径一字不变（触发者仍是用户）。
func TestStopRunKeepsManualButtonSemantics(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	sub := mkSession(t, a, "t-manual", "手动叫停的任务", "task", "")
	if _, ok := a.beginRun(sub.ID); !ok {
		t.Fatal("登记执行失败")
	}
	defer a.finishRun(sub.ID)
	if !a.stopRun(sub.ID) {
		t.Fatal("在跑的会话应停止成功")
	}
	rec := a.Store.GetStopRecord(sub.ID)
	if rec.Kind != stopByUser || rec.Actor != stopActorUser || rec.Note != "用户点击停止按钮" {
		t.Fatalf("人工点按钮的停止记录口径变了: %+v", rec)
	}
	if !rec.Manual {
		t.Fatal("尚未收口时应带 Manual 标志")
	}
	head := stopReportHead(sub, rec, "2026-10-14 23:59:00", 3, 5)
	for _, want := range []string{"已由用户手动停止", "用户点击停止按钮", "触发者：用户", "共 3 轮 / 5 步"} {
		if !strings.Contains(head, want) {
			t.Fatalf("人工停止的回报首句缺 %q：\n%s", want, head)
		}
	}
	if strings.Contains(head, "数字助理") {
		t.Fatalf("人工停止的回报不该提数字助理：\n%s", head)
	}
	// 触发者标签本身也要能分开
	if stopActorLabel(stopActorUser) == stopActorLabel(stopActorAssistant) {
		t.Fatal("人工停止与数字助理代停的触发者标签应分得开")
	}
	// 停止记录里的累计计数沿用旧口径（人为叫停都算「用户停过这个会话」）
	if rec.UserStops != 1 {
		t.Fatalf("人为叫停应累计到 UserStops, got %d", rec.UserStops)
	}
}
