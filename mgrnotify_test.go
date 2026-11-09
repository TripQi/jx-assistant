package main

// 「审批/提问的经理代理」（mgrnotify.go）的验收：
//   · 通知范围：经理关联（dispatch_task 派生 DispatchedBy / send_to_session 补标
//     mgrlink）的会话才通知；用户自建且经理从未触达的不通知（防骚扰）；经理
//     主对话自己与待办系统派生不算；
//   · 通知时机：卡挂出设置时长（短延时桩）仍无人处理 → 经理会话收到一条
//     kind=aiuser origin=mgrcard 的通知，含卡片 id、摘要、可选项与代批路径；
//   · 决议回告：用户在界面选 / 经理代选（byManager 标注）/ 等待超时三种收尾
//     都回告，来源标注正确；
//   · 不越位：定时器到点前已有决议的卡不通知；用户停表（碰过卡片=有人在场）
//     不通知；决议回告只发通知过的卡。
// 刻意不配模型提供方：被通知唤醒的经理那一轮会在后台立刻失败收尾，不影响
// 断言（与 request_manager_test.go 同一做法）。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// newMgrNotifyTestApp：带临时库的 App + 一个经理主对话（currentAssistant 认得到）
// + 一个经理派生的任务会话 + 一个经理从未触达的无关会话。
func newMgrNotifyTestApp(t *testing.T) (a *App, mgr, task, solo *SessionMeta) {
	t.Helper()
	a = newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = nil
	mk := func(id, title, mode, dispatchedBy string) *SessionMeta {
		m := &SessionMeta{ID: id, Title: title, Mode: mode, Workspace: a.Cfg.Workspace,
			Permission: permWorkspace, DispatchedBy: dispatchedBy, CreatedAt: now(), UpdatedAt: now()}
		if err := a.Store.Create(m); err != nil {
			t.Fatalf("建会话 %s 失败: %v", id, err)
		}
		return m
	}
	mgr = mk("mgr-main", "数字经理", "assistant", "")
	task = mk("task-disp", "登录页样式", "coding", "mgr-main") // dispatch_task 派生（存量直接拿 DispatchedBy 当依据）
	solo = mk("task-solo", "私人调研", "coding", "")            // 用户自建，经理从未触达
	return a, mgr, task, solo
}

// mgrNoticeRows 数经理会话里 mgrcard 消息的条数并取全文。
func mgrNoticeRows(t *testing.T, a *App, mgrID string) (int, []string) {
	t.Helper()
	rows, err := a.Store.RawMessagesAll(mgrID)
	if err != nil {
		t.Fatalf("读经理会话历史失败: %v", err)
	}
	var texts []string
	for _, r := range rows {
		if r.Origin == mgrNotifyOrigin {
			texts = append(texts, r.Text())
		}
	}
	return len(texts), texts
}

// waitMgrRows 等经理会话攒够 want 条 mgrcard 消息（投递在通知 goroutine 里，
// 短延时桩下几十毫秒内到达）。
func waitMgrRows(t *testing.T, a *App, mgrID string, want int) []string {
	t.Helper()
	for i := 0; i < 400; i++ {
		n, texts := mgrNoticeRows(t, a, mgrID)
		if n >= want {
			return texts
		}
		time.Sleep(5 * time.Millisecond)
	}
	n, _ := mgrNoticeRows(t, a, mgrID)
	t.Fatalf("经理会话的 mgrcard 消息始终没到 %d 条（现在 %d 条）", want, n)
	return nil
}

// waitApprovalOf 等审批卡登记进清单（requestApproval 在 goroutine 里，id 由其
// 内部生成，只能轮询清单拿；perm_alert_test.go 已有按 id 等的版本，这里取实体）。
func waitApprovalOf(t *testing.T, a *App) *Approval {
	t.Helper()
	for i := 0; i < 400; i++ {
		if aps := a.pendingApprovals(); len(aps) > 0 {
			return aps[0]
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("审批卡没有登记进等待清单")
	return nil
}

// withShortMgrNotify 把通知延时换成本次测试的短桩，测试完复原。
func withShortMgrNotify(t *testing.T, d time.Duration) {
	t.Helper()
	old := mgrNotifyDelay
	mgrNotifyDelay = func(*App) time.Duration { return d }
	t.Cleanup(func() { mgrNotifyDelay = old })
}

// 主路径：派生会话的提问卡挂着没人答 → 经理收到通知（身份与要素齐全）；
// 用户随后作答 → 回告（含答案与来源）。
func TestMgrNotifyDispatchedAskThenUserResolves(t *testing.T) {
	a, mgr, task, solo := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)

	q := &UserQuestion{Kind: askKindAsk, Question: "选哪个方案？",
		Options: []string{"方案甲", "方案乙"}, AllowFree: true}
	done := make(chan string, 1)
	go func() {
		s, ok := a.requestUserAnswer(context.Background(), task, q)
		if ok {
			done <- s
		}
	}()
	waitRegistered(t, a, q)

	texts := waitMgrRows(t, a, mgr.ID, 1)
	r := texts[0]
	for _, want := range []string{"【提问代理】", "登录页样式", task.ID, q.ID, "选哪个方案",
		"方案甲 / 方案乙", "/api/questions/" + q.ID} {
		if !strings.Contains(r, want) {
			t.Fatalf("通知正文应含 %q：\n%s", want, r)
		}
	}

	// 用户在界面作答 → 回告写明答案与来源
	if err := a.resolveQuestion(q.ID, "方案乙"); err != nil {
		t.Fatalf("作答失败: %v", err)
	}
	select {
	case ans := <-done:
		if ans != "方案乙" {
			t.Fatalf("等待中的提问工具应拿到答案，实际 %q", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("作答没有唤醒等待中的提问工具")
	}
	texts = waitMgrRows(t, a, mgr.ID, 2)
	for _, want := range []string{"【提问结果】", "方案乙", "用户在界面处理"} {
		if !strings.Contains(texts[1], want) {
			t.Fatalf("回告正文应含 %q：\n%s", want, texts[1])
		}
	}

	// 无关会话（经理从未触达）同样挂卡：不通知（防骚扰）
	q2 := &UserQuestion{Kind: askKindAsk, Question: "私事怎么处理？", AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), solo, q2) }()
	waitRegistered(t, a, q2)
	_ = a.resolveQuestion(q2.ID, "自己定")
	time.Sleep(150 * time.Millisecond) // 越过短延时窗口，确认没多发一条
	if n, _ := mgrNoticeRows(t, a, mgr.ID); n != 2 {
		t.Fatalf("经理从未触达的会话不该产生通知，mgrcard 共 %d 条", n)
	}
}

// 审批卡主路径：通知（申请/理由/代批路径）→ 用户批准 → 回告「已批准」。
func TestMgrNotifyApprovalFlow(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)

	res := make(chan approvalResult, 1)
	go func() {
		approved, always, _ := a.requestApproval(context.Background(), task,
			"run_command", "删除临时目录 data/tmp", "清理构建产物")
		res <- approvalResult{approved: approved, always: always}
	}()
	ap := waitApprovalOf(t, a)

	texts := waitMgrRows(t, a, mgr.ID, 1)
	r := texts[0]
	for _, want := range []string{"【审批代理】", "登录页样式", task.ID, ap.ID, "run_command",
		"删除临时目录", "/api/approvals/" + ap.ID, `"approved":true`} {
		if !strings.Contains(r, want) {
			t.Fatalf("通知正文应含 %q：\n%s", want, r)
		}
	}

	if err := a.resolveApproval(ap.ID, true, false); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	select {
	case got := <-res:
		if !got.approved || got.always {
			t.Fatalf("等待方应收到「批准但非常设」，实际 %+v", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("审批决定没有唤醒等待方")
	}
	texts = waitMgrRows(t, a, mgr.ID, 2)
	for _, want := range []string{"【审批结果】", ap.ID, "已批准", "用户在界面处理"} {
		if !strings.Contains(texts[1], want) {
			t.Fatalf("回告正文应含 %q：\n%s", want, texts[1])
		}
	}
}

// 经理代答（计划审批）：通知含计划摘要与哨兵答案说明；经理代选后回告标「你已代选」。
func TestMgrNotifyManagerProxyOnPlan(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)

	q := &UserQuestion{Kind: askKindPlan, Question: "请审批以下执行计划",
		Plan: "# 重构方案\n第一步只读调研；第二步小步改并逐段编译。", AllowFree: true}
	done := make(chan string, 1)
	go func() {
		s, ok := a.requestUserAnswer(context.Background(), task, q)
		if ok {
			done <- s
		}
	}()
	waitRegistered(t, a, q)

	texts := waitMgrRows(t, a, mgr.ID, 1)
	for _, want := range []string{"计划审批", q.ID, "__approve__", "__ignore__", "第一步只读调研"} {
		if !strings.Contains(texts[0], want) {
			t.Fatalf("计划审批通知应含 %q：\n%s", want, texts[0])
		}
	}

	// 经理经 call_api 代批：handler 的两步（标注来源 → 登记决定），此处同序直调
	a.markQuestionByManager(q.ID)
	if err := a.resolveQuestion(q.ID, "__approve__"); err != nil {
		t.Fatalf("代批失败: %v", err)
	}
	select {
	case ans := <-done:
		if ans != "__approve__" {
			t.Fatalf("等待方应收到批准，实际 %q", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("代批没有唤醒等待方")
	}
	texts = waitMgrRows(t, a, mgr.ID, 2)
	for _, want := range []string{"【提问结果】", "已批准（按计划执行）", "你已代选"} {
		if !strings.Contains(texts[1], want) {
			t.Fatalf("代选回告应含 %q：\n%s", want, texts[1])
		}
	}
}

// 等待超时：通知先到，超时收卡后回告「等待超时」且不落来源（不是谁的决议）。
func TestMgrNotifyTimeoutReport(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)

	q := &UserQuestion{Kind: askKindAsk, Question: "定了没？", TimeoutSec: 1, AllowFree: true}
	done := make(chan bool, 1)
	go func() {
		_, ok := a.requestUserAnswer(context.Background(), task, q)
		done <- !ok // 超时 ok=false
	}()
	waitRegistered(t, a, q)
	waitMgrRows(t, a, mgr.ID, 1) // 通知先于超时（30ms < 1s）

	select {
	case timedOut := <-done:
		if !timedOut {
			t.Fatal("1 秒限时提问应按超时收尾")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("提问没有按 1 秒超时")
	}
	texts := waitMgrRows(t, a, mgr.ID, 2)
	for _, want := range []string{"【提问结果】", "等待超时未获回答"} {
		if !strings.Contains(texts[1], want) {
			t.Fatalf("超时回告应含 %q：\n%s", want, texts[1])
		}
	}
	if strings.Contains(texts[1], "来源") {
		t.Fatalf("超时不是谁的决议，不该落来源：\n%s", texts[1])
	}
}

// 不越位一：定时器到点前已有决议（用户手快）→ 不通知。
func TestMgrNotifyResolvedBeforeDueNotSent(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 250*time.Millisecond)

	q := &UserQuestion{Kind: askKindAsk, Question: "快问快答？", AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q) }()
	waitRegistered(t, a, q)
	_ = a.resolveQuestion(q.ID, "答了") // 决议赶在通知之前
	time.Sleep(600 * time.Millisecond)
	if n, _ := mgrNoticeRows(t, a, mgr.ID); n != 0 {
		t.Fatalf("到点前已有决议的卡不该通知经理，mgrcard=%d 条", n)
	}
}

// 不越位二：用户碰过卡片（停表=有人在场）→ 到点不通知（与声光提醒同口径）。
func TestMgrNotifyHeldQuestionQuiet(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)

	q := &UserQuestion{Kind: askKindAsk, Question: "选哪个？", Options: []string{"甲", "乙"}, AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q) }()
	waitRegistered(t, a, q)
	a.holdQuestion(q.ID) // 悬停/触碰卡片：有人在场
	time.Sleep(150 * time.Millisecond)
	if n, _ := mgrNoticeRows(t, a, mgr.ID); n != 0 {
		t.Fatalf("用户已碰卡的提问不该通知经理，mgrcard=%d 条", n)
	}
	_ = a.resolveQuestion(q.ID, "甲")
}

// 标记判定：派生即标（DispatchedBy）、发话补标（mgrlink）、排除经理自己与
// 待办系统派生、nil 安全。
func TestManagerLinkedMarking(t *testing.T) {
	a, mgr, task, solo := newMgrNotifyTestApp(t)
	if !a.managerLinked(task) {
		t.Fatal("dispatch_task 派生（DispatchedBy=经理）应算经理关联")
	}
	if a.managerLinked(solo) {
		t.Fatal("用户自建且经理从未触达的会话不该算经理关联")
	}
	if a.managerLinked(mgr) {
		t.Fatal("经理主对话自己不算（不会通知自己）")
	}
	if a.managerLinked(&SessionMeta{ID: "x", Mode: "coding", DispatchedBy: todoComponentID}) {
		t.Fatal("待办调度器的系统派生不算（它明确不打扰数字经理）")
	}
	if a.managerLinked(nil) {
		t.Fatal("nil 会话不该算经理关联")
	}
	a.markManagerLinked(solo) // send_to_session 首次发话补标
	if !a.managerLinked(solo) {
		t.Fatal("补标之后应算经理关联")
	}
}

// 代批来源判定：直调请求带调用者会话头，只有当前经理主对话自己才算代选；
// 没有头（前端按钮/远程转发）与非经理会话一律算用户。
func TestManagerResolveCaller(t *testing.T) {
	a, mgr, _, _ := newMgrNotifyTestApp(t)
	if cur := a.currentAssistant(); cur == nil || cur.ID != mgr.ID {
		t.Fatalf("currentAssistant 应认到测试建的经理主对话，实际 %+v", cur)
	}
	r, _ := http.NewRequest(http.MethodPost, "/api/questions/q-1", nil)
	if managerResolveCaller(a, r) {
		t.Fatal("不带调用者头的请求是用户（前端按钮/远程转发），不算经理代选")
	}
	r.Header.Set("X-JX-Caller-Session", "task-disp")
	if managerResolveCaller(a, r) {
		t.Fatal("非经理会话的调用不算经理代选")
	}
	r.Header.Set("X-JX-Caller-Session", mgr.ID)
	if !managerResolveCaller(a, r) {
		t.Fatal("当前经理主对话自己的 call_api 直调应算经理代选")
	}
}

// 端到端（真执行体 + 假模型，不联网不花额度）：派生会话的提问卡挂到点 →
// 经理收到【提问代理】并被唤醒 → 经理用 call_api 代答（POST /api/questions，
// 直调请求带调用者头）→ 任务等待方拿到答案，决议回告插话回到经理并标
// 「你已代选」→ 经理带着回告收尾。验收的是整条产品链路：通知、唤醒、
// call_api 代批通路、来源标注、回告，一个不缺。
func TestMgrNotifyEndToEndManagerProxiesViaCallAPI(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)

	// 假模型：经理第一轮从通知正文里抠出卡片 id，调 call_api 代答；之后收尾
	var mu sync.Mutex
	actions := []string{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req ChatRequest
		_ = json.Unmarshal(body, &req)
		hasCallAPI := false
		results := 0
		cardID := ""
		for _, m := range req.Messages {
			if m.Role == "tool" {
				results++
			}
			// 环境快照等也是 user 消息且排在通知之后，扫全部 user 消息找卡片 id
			if m.Role == "user" {
				if s, ok := m.Content.(string); ok {
					if mm := regexp.MustCompile(`卡片：提问 (q-[0-9A-Za-z]+)`).FindStringSubmatch(s); mm != nil && cardID == "" {
						cardID = mm[1]
					}
				}
			}
		}
		for _, tl := range req.Tools {
			if tl.Function.Name == "call_api" {
				hasCallAPI = true
			}
		}
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		write := func(payload []byte) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		mu.Lock()
		defer mu.Unlock()
		switch {
		case hasCallAPI && results == 0 && cardID != "":
			actions = append(actions, "proxy:"+cardID)
			args, _ := json.Marshal(map[string]any{
				"path": "/api/questions/" + cardID, "method": "POST",
				"params": map[string]any{"answer": "方案甲"},
			})
			chunk, _ := json.Marshal(map[string]any{"choices": []any{
				map[string]any{"delta": map[string]any{"tool_calls": []any{
					map[string]any{"index": 0, "id": "call_mgr_1",
						"function": map[string]any{"name": "call_api", "arguments": string(args)}},
				}}},
			}})
			write(chunk)
		default:
			actions = append(actions, "say:card="+cardID)
			chunk, _ := json.Marshal(map[string]any{"choices": []any{
				map[string]any{"delta": map[string]any{"content": "我已替它选了方案甲，已回告。"}},
			}})
			write(chunk)
		}
		write([]byte("[DONE]"))
	}))
	t.Cleanup(srv.Close)
	a.Cfg.Providers = []ProviderCfg{{ID: "fake", Name: "假模型", BaseURL: srv.URL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-1", Name: "假模型"}}}}
	a.Cfg.BaseURL = ""

	// 任务会话（经理派生）挂一张没人答的提问卡，等通知把经理叫醒
	q := &UserQuestion{Kind: askKindAsk, Question: "选哪个方案？",
		Options: []string{"方案甲", "方案乙"}, AllowFree: true}
	done := make(chan string, 1)
	go func() {
		s, ok := a.requestUserAnswer(context.Background(), task, q)
		if ok {
			done <- s
		}
	}()
	waitRegistered(t, a, q)

	// ① 经理收到通知并被唤醒；② 它经 call_api 代答成功（任务等待方拿到答案）
	select {
	case ans := <-done:
		if ans != "方案甲" {
			t.Fatalf("代答应到达任务等待方，实际 %q", ans)
		}
	case <-time.After(10 * time.Second):
		mu.Lock()
		acts := strings.Join(actions, ",")
		mu.Unlock()
		rows, _ := a.Store.RawMessagesAll(mgr.ID)
		var dump []string
		for _, rr := range rows {
			dump = append(dump, rr.Role+"/"+rr.Kind+"/"+rr.Origin+"/"+truncateRunes(rr.Text(), 80))
		}
		t.Fatalf("经理没有在时限内代答。actions=[%s]；经理会话消息：%s", acts, strings.Join(dump, " | "))
	}
	// ③ 决议回告落进经理会话，来源标「你已代选」（直调调用者头被认到）
	texts := waitMgrRows(t, a, mgr.ID, 2)
	if !strings.Contains(texts[1], "【提问结果】") || !strings.Contains(texts[1], "方案甲") ||
		!strings.Contains(texts[1], "你已代选") {
		t.Fatalf("端到端回告应含结果与「你已代选」：\n%s", texts[1])
	}
	// ④ 经理带着回告正常收尾（假模型第二轮说话）
	waitFor(t, "经理收尾发言落库", func() bool { return msgHas(t, a, mgr.ID, "我已替它选了方案甲") })
	mu.Lock()
	defer mu.Unlock()
	if len(actions) < 2 || !strings.HasPrefix(actions[0], "proxy:") {
		t.Fatalf("假模型应先代答后收尾，实际动作 %v", actions)
	}
}
