package main

// 「审批/提问的经理代理」（mgrnotify.go）的验收——代批资格是**轮次级授权**：
//   · 窗口：dispatch_task / send_to_session 投递开出的那一轮才授予
//     （grantTestTurn 模拟投递开轮）；轮收口（正常完成/用户停止）、用户亲发
//     消息（接管）即撤销；用户自己发起的轮不通知、经理代决被 403 拒；
//   · 通知时机：窗口内的卡挂出设置时长（短延时桩）仍无人处理 → 经理会话
//     收到一条 kind=aiuser origin=mgrcard 的通知，含卡片 id、摘要、可选项与
//     代批路径；窗口外的卡到点不通知（用户在场的轮用户自己看得见）；
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
	// 通知链要真的跑模型轮（经理被叫醒后要用工具代答），配一个离线 mock 渠道
	// 当全局默认模型：BaseURL=mock 不要密钥，也不会真的发请求。
	a.Cfg.Providers = []ProviderCfg{{ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "mock", Model: "mock-1"}
	mk := func(id, title, mode, dispatchedBy string) *SessionMeta {
		m := &SessionMeta{ID: id, Title: title, Mode: mode, Workspace: a.Cfg.Workspace,
			Permission: permWorkspace, DispatchedBy: dispatchedBy, CreatedAt: now(), UpdatedAt: now()}
		if err := a.Store.Create(m); err != nil {
			t.Fatalf("建会话 %s 失败: %v", id, err)
		}
		return m
	}
	mgr = mk("mgr-main", "长期助理", "assistant", "")
	task = mk("task-disp", "登录页样式", "coding", "mgr-main") // dispatch_task 派生（DispatchedBy 只走回报链，不再当经理代批依据）
	solo = mk("task-solo", "私人调研", "coding", "")            // 用户自建，经理从未触达
	// 通知会唤醒经理跑一轮真模型（夹具配的离线 mock 渠道），它会在 workspace 里
	// 写文件且比测试体收尾得晚；不先等它收口，t.TempDir 的清理会撞「目录非空」。
	// 等不到就硬收口（与看门狗同路），不留后台轮给下个用例。
	t.Cleanup(func() {
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			a.runMu.Lock()
			busy := len(a.runners) > 0
			a.runMu.Unlock()
			if !busy {
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
		a.runMu.Lock()
		ids := make([]string, 0, len(a.runners))
		for sid := range a.runners {
			ids = append(ids, sid)
		}
		a.runMu.Unlock()
		for _, sid := range ids {
			if r := a.runOf(sid); r != nil {
				a.forceStopRun(sid, r)
			}
		}
	})
	return a, mgr, task, solo
}

// grantTestTurn 模拟「经理投递开出的那一轮」：beginRun 登记即视为正在执行
//（测试没有真实执行体），投递侧授权走产品同款 grantManagerTurn。测试结束统一
// 收口（幂等，多次 finishRunOf 只收一次）。
func grantTestTurn(t *testing.T, a *App, sid string) *sessionRun {
	t.Helper()
	run, ok := a.beginRun(sid)
	if !ok {
		t.Fatalf("会话 %s 开轮失败（已在执行？）", sid)
	}
	t.Cleanup(func() { a.finishRunOf(sid, run) })
	a.grantManagerTurn(sid, run)
	return run
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

// ---------- 轮次级授权的授予/撤销判定（四种时机的地基） ----------

// TestManagerTurnGrantLifecycle 授权生命周期：未授权无效；授权只对那一轮有效；
// 轮收口/用户亲发消息撤销；旧授权不跨轮生效；竞态窗口里迟到的旧授权作废；
// 无关轮的收口不误删本轮授权；插话（run=nil）不建立授权。
func TestManagerTurnGrantLifecycle(t *testing.T) {
	a, _, task, _ := newMgrNotifyTestApp(t)
	if a.managerTurnActive(task.ID) {
		t.Fatal("从未授权的会话不该有经理代批窗口")
	}
	run1, ok := a.beginRun(task.ID)
	if !ok {
		t.Fatal("开轮失败")
	}
	t.Cleanup(func() { a.finishRunOf(task.ID, run1) })
	if a.managerTurnActive(task.ID) {
		t.Fatal("轮已在执行但未授权（不是经理投递开出的）：不该有窗口")
	}
	a.grantManagerTurn(task.ID, run1)
	if !a.managerTurnActive(task.ID) {
		t.Fatal("经理投递开出的那一轮应有代批窗口")
	}

	// 用户亲发消息（接管）：窗口立即撤销——即使那一轮还在跑
	a.revokeManagerTurn(task.ID)
	if a.managerTurnActive(task.ID) {
		t.Fatal("用户亲发消息后经理代批资格应撤销")
	}

	// 轮收口后再开的新轮：旧授权（指向死轮）不跨轮生效；经理再投递才再授权
	a.finishRunOf(task.ID, run1)
	run2, ok := a.beginRun(task.ID)
	if !ok {
		t.Fatal("第二轮开轮失败")
	}
	t.Cleanup(func() { a.finishRunOf(task.ID, run2) })
	if a.managerTurnActive(task.ID) {
		t.Fatal("上一轮的授权不该跨轮生效（新一轮需要经理重新投递）")
	}
	a.grantManagerTurn(task.ID, run2)
	if !a.managerTurnActive(task.ID) {
		t.Fatal("经理再次投递开出的新一轮应重新授权")
	}

	// 真实投递窗口：经理已开轮，用户先接管，之后迟到的 grant 不能恢复授权。
	a.revokeManagerTurn(task.ID)
	a.grantManagerTurn(task.ID, run2)
	if a.managerTurnActive(task.ID) {
		t.Fatal("撤销后迟到的旧授权应作废")
	}

	// 用户接管之后真正新开的经理轮仍能授权。
	a.finishRunOf(task.ID, run2)
	run3, ok := a.beginRun(task.ID)
	if !ok {
		t.Fatal("第三轮开轮失败")
	}
	t.Cleanup(func() { a.finishRunOf(task.ID, run3) })
	a.grantManagerTurn(task.ID, run3)
	// 收口按轮身份清授权：无关轮的收口不能误删本轮授权，本轮收口才清
	a.dropManagerTurnOf(task.ID, run2) // 旧轮迟到收口
	if !a.managerTurnActive(task.ID) {
		t.Fatal("无关轮的收口不该清掉本轮授权")
	}
	a.dropManagerTurnOf(task.ID, run3) // 本轮收口
	if a.managerTurnActive(task.ID) {
		t.Fatal("本轮收口应清掉授权")
	}

	// 插话（send_to_session 对执行中会话的投递，run=nil）：注入的是别人发起的
	// 那一轮，不建立授权
	a.grantManagerTurn(task.ID, nil)
	if a.managerTurnActive(task.ID) {
		t.Fatal("插话不该建立经理代批授权")
	}
}

// ---------- 四种时机 ----------

// 时机一：派发后有效 → 轮次结束失效。窗口内挂卡经理收到通知；同一会话用户
// 接手后的下一轮再挂卡，到点不通知。
func TestMgrNotifyWindowClosesAtRoundEnd(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	run := grantTestTurn(t, a, task.ID)

	// 窗口内：挂卡到点 → 经理收到通知
	q := &UserQuestion{Kind: askKindAsk, Question: "第一张卡？", AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q) }()
	waitRegistered(t, a, q)
	waitMgrRows(t, a, mgr.ID, 1)
	_ = a.resolveQuestion(q.ID, "办")

	// 轮收口（正常完成走 closeRun）：授权随轮撤销
	a.finishRunOf(task.ID, run)
	if a.managerTurnActive(task.ID) {
		t.Fatal("轮收口后经理代批资格应撤销")
	}

	// 用户接手开的下一轮（无授权）再挂卡：到点不通知
	q2 := &UserQuestion{Kind: askKindAsk, Question: "用户自己的卡？", AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q2) }()
	waitRegistered(t, a, q2)
	_ = a.resolveQuestion(q2.ID, "自己定")
	time.Sleep(150 * time.Millisecond) // 越过短延时窗口
	if n, _ := mgrNoticeRows(t, a, mgr.ID); n != 1 {
		t.Fatalf("非经理轮的卡不该通知经理，mgrcard 共 %d 条", n)
	}
}

// 时机二：派发后用户停止 → 失效。stopRunBy（用户点停止/数字助理代停同一入口）
// 后执行体收口，授权撤销，之后的卡不再通知。
func TestMgrNotifyStopRevokesGrant(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	run := grantTestTurn(t, a, task.ID)
	if !a.managerTurnActive(task.ID) {
		t.Fatal("前置失败：授权的轮应在窗口内")
	}

	// 用户停止（协作取消）→ 执行体响应取消收口（closeRun）
	if !a.stopRunBy(task.ID, stopActorUser, stopTriggerNote(stopActorUser)) {
		t.Fatal("停止请求没有发出")
	}
	a.finishRunOf(task.ID, run)
	if a.managerTurnActive(task.ID) {
		t.Fatal("用户停止后经理代批资格应撤销")
	}

	// 停止后的卡不再有通知
	q := &UserQuestion{Kind: askKindAsk, Question: "停了之后的卡？", AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q) }()
	waitRegistered(t, a, q)
	_ = a.resolveQuestion(q.ID, "知道了")
	time.Sleep(150 * time.Millisecond)
	if n, _ := mgrNoticeRows(t, a, mgr.ID); n != 0 {
		t.Fatalf("停止后的卡不该通知经理，mgrcard 共 %d 条", n)
	}
}

// 时机三：用户亲发消息（接管）→ 失效。经理轮正在跑、用户插话/排队，窗口立即
// 撤销（handleChat / handleInterject 同款调用），当轮已挂出的卡到点不通知。
func TestMgrNotifyUserTakeoverRevokesGrant(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	grantTestTurn(t, a, task.ID)

	q := &UserQuestion{Kind: askKindAsk, Question: "用户接管前挂的卡？", AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q) }()
	waitRegistered(t, a, q)
	a.revokeManagerTurn(task.ID) // 用户亲发消息＝接管（与 handleInterject 同款调用）
	if a.managerTurnActive(task.ID) {
		t.Fatal("用户亲发消息后经理代批资格应撤销")
	}
	time.Sleep(150 * time.Millisecond)
	if n, _ := mgrNoticeRows(t, a, mgr.ID); n != 0 {
		t.Fatalf("用户接管后当轮的卡不该通知经理，mgrcard 共 %d 条", n)
	}
	_ = a.resolveQuestion(q.ID, "用户来")
}

// 时机四：非经理轮次的卡片不代决。用户自己的轮（无授权）挂卡，经理经 call_api
// 直调代答 → 403 拒绝，卡片仍等用户；同一张卡用户界面照常能答。对照：经理轮内
// 同样直调 → 放行并标注来源。
func TestMgrResolveRejectedOutsideWindow(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)

	// 无授权窗口：经理直调代决被拒
	q := &UserQuestion{Kind: askKindAsk, Question: "谁的卡？", AllowFree: true}
	done := make(chan struct{})
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q); close(done) }()
	waitRegistered(t, a, q)
	post := func(cardID, answer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/api/questions/"+cardID,
			strings.NewReader(`{"answer":"`+answer+`"}`))
		r.Header.Set("X-JX-Caller-Session", mgr.ID) // 经理经 call_api 直调的调用者头
		r.SetPathValue("id", cardID)
		w := httptest.NewRecorder()
		a.handleResolveQuestion(w, r)
		return w
	}
	if w := post(q.ID, "我来定"); w.Code != 403 {
		t.Fatalf("非经理轮的代决应 403 拒绝，实际 %d（%s）", w.Code, w.Body.String())
	}
	if a.managerTurnActive(task.ID) {
		t.Fatal("用户自己的轮不该有经理代批窗口")
	}
	// 卡片没被代决掉，用户界面照常能答
	if err := a.resolveQuestion(q.ID, "用户定"); err != nil {
		t.Fatalf("用户作答应成功: %v", err)
	}
	<-done

	// 对照：经理投递开出的那一轮内，同样直调放行并标注来源
	grantTestTurn(t, a, task.ID)
	q2 := &UserQuestion{Kind: askKindAsk, Question: "经理轮的卡？", AllowFree: true}
	done2 := make(chan struct{})
	go func() { _, _ = a.requestUserAnswer(context.Background(), task, q2); close(done2) }()
	waitRegistered(t, a, q2)
	if w := post(q2.ID, "经理定"); w.Code != 200 {
		t.Fatalf("经理轮内的代决应放行，实际 %d（%s）", w.Code, w.Body.String())
	}
	a.permMu.Lock()
	byMgr := q2.byManager
	a.permMu.Unlock()
	if !byMgr {
		t.Fatal("窗口内的经理代决应标注 byManager（回告来源）")
	}
	<-done2
}

// ---------- 窗口内的既有行为（30 秒代决链路不回归） ----------

// 主路径：经理轮的提问卡挂着没人答 → 经理收到通知（身份与要素齐全）；
// 用户随后作答 → 回告（含答案与来源）。
func TestMgrNotifyDispatchedAskThenUserResolves(t *testing.T) {
	a, mgr, task, solo := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	grantTestTurn(t, a, task.ID)

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

	// 无关会话（经理从未触达、无授权窗口）同样挂卡：不通知（防骚扰）
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
	grantTestTurn(t, a, task.ID)

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

// 经理代答（计划审批）：通知含计划摘要与哨兵答案说明；经理代选后回告标「长期助理代选」。
func TestMgrNotifyManagerProxyOnPlan(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	grantTestTurn(t, a, task.ID)

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
	for _, want := range []string{"【提问结果】", "已批准（按计划执行）", "长期助理代选"} {
		if !strings.Contains(texts[1], want) {
			t.Fatalf("代选回告应含 %q：\n%s", want, texts[1])
		}
	}
}

// 等待超时：通知先到，超时收卡后回告「等待超时」且不落来源（不是谁的决议）。
func TestMgrNotifyTimeoutReport(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	grantTestTurn(t, a, task.ID)

	q := &UserQuestion{Kind: askKindAsk, Question: "定了没？", TimeoutSec: 1, AllowFree: true}
	done := make(chan bool, 1)
	go func() {
		_, ok := a.requestUserAnswer(context.Background(), task, q)
		done <- !ok // 超时 ok=false
	}()
	waitRegistered(t, a, q)
	texts := waitMgrRows(t, a, mgr.ID, 1) // 通知先于超时（30ms < 1s）

	select {
	case timedOut := <-done:
		if !timedOut {
			t.Fatal("1 秒限时提问应按超时收尾")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("提问没有按 1 秒超时")
	}
	texts = waitMgrRows(t, a, mgr.ID, 2)
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
	grantTestTurn(t, a, task.ID)

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
	grantTestTurn(t, a, task.ID)

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

// 端到端（真执行体 + 假模型，不联网不花额度）：经理轮的提问卡挂到点 →
// 经理收到【提问代理】并被唤醒 → 经理用 call_api 代答（POST /api/questions，
// 直调请求带调用者头，窗口内放行）→ 任务等待方拿到答案，决议回告插话回到经理
// 并标「长期助理代选」→ 经理带着回告收尾。验收的是整条产品链路：通知、唤醒、
// call_api 代批通路（含轮次级授权的放行）、来源标注、回告，一个不缺。
func TestMgrNotifyEndToEndManagerProxiesViaCallAPI(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	withShortMgrNotify(t, 30*time.Millisecond)
	grantTestTurn(t, a, task.ID) // 经理投递开出的那一轮（call_api 代答要过它的窗口）

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
	// 默认模型体系：会话没手选时跟全局默认模型
	a.Cfg.DefaultModel = ModelRef{Provider: "fake", Model: "fake-1"}

	// 任务会话（经理投递开出的那一轮在执行）挂一张没人答的提问卡，等通知把经理叫醒
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
	// ③ 决议回告落进经理会话，来源标「长期助理代选」（直调调用者头被认到）
	texts := waitMgrRows(t, a, mgr.ID, 2)
	if !strings.Contains(texts[1], "【提问结果】") || !strings.Contains(texts[1], "方案甲") ||
		!strings.Contains(texts[1], "长期助理代选") {
		t.Fatalf("端到端回告应含结果与「长期助理代选」：\n%s", texts[1])
	}
	// ④ 经理带着回告正常收尾（假模型第二轮说话）
	waitFor(t, "经理收尾发言落库", func() bool { return msgHas(t, a, mgr.ID, "我已替它选了方案甲") })
	mu.Lock()
	defer mu.Unlock()
	if len(actions) < 2 || !strings.HasPrefix(actions[0], "proxy:") {
		t.Fatalf("假模型应先代答后收尾，实际动作 %v", actions)
	}
}
