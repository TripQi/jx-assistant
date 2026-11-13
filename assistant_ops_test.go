package main

// 设置 · 对话历史与压缩 两个主对话动作的测试：
//  1. 清空数字助理主对话（POST /api/assistant/clear）：消息与随消息产生的
//     工件（快照/交付/上传）真删、会话本体与设置保留、子会话与别的会话不受
//     影响、待生效排队与报错条一并清掉、执行中拒绝、没有主对话时 404。
//  2. 压缩数字助理主对话（POST /api/assistant/compact）：固定走模型摘要
//     （即使全局策略是 rule 或 off 也不做规则折叠）、摘要行落库（origin=manual、
//     带覆盖边界）、无可压缩历史时给友好报错。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// assistantOpsFixture 是测试用的一套会话格局：
//
//	main：数字助理主对话（mode=assistant，无父会话）——两个动作的作用对象
//	sub ：挂在主对话下的子对话（清空不该动它）
//	task：普通工作任务会话（清空不该动它）
//
// 三个会话各带消息、快照、交付与上传登记；主对话另有 3 轮历史（够模型摘要
// 压缩出可压区间）。提供方配成 mock，摘要调用直接给占位摘要、不发网络请求。
type assistantOpsFixture struct {
	app  *App
	main *SessionMeta
	sub  *SessionMeta
	task *SessionMeta
}

func newAssistantOpsFixture(t *testing.T) *assistantOpsFixture {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime() // 运行态各表（排队/报错/执行注册表）就位，与真实进程同一套
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	newSess := func(id, title, mode, parent string) *SessionMeta {
		m := &SessionMeta{ID: id, Title: title, Mode: mode, ParentID: parent,
			Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
		if err := a.Store.Create(m); err != nil {
			t.Fatalf("建会话 %s 失败: %v", id, err)
		}
		return m
	}
	main := newSess("main", "数字助理主对话", "assistant", "")
	sub := newSess("sub", "子对话", "coding", main.ID)
	task := newSess("task", "工作任务", "task", "")

	seed := func(sid string, rounds int) {
		for i := 0; i < rounds; i++ {
			seedRound(t, a, sid, sid+string(rune('A'+i)), true)
		}
		if _, err := a.Store.AppendCheckpoint(&Checkpoint{
			SessionID: sid, Seq: 2, ToolName: "write", Path: sid + ".txt",
			ExistedBefore: true, BeforeSize: 5, CreatedAt: now(),
		}); err != nil {
			t.Fatalf("落快照 %s 失败: %v", sid, err)
		}
		if err := a.Store.AddDelivery(&Delivery{
			SessionID: sid, Seq: 4, Path: sid + "-out.txt", Note: "成果", CreatedAt: now(),
		}); err != nil {
			t.Fatalf("落交付 %s 失败: %v", sid, err)
		}
		if err := a.Store.AddUpload(&Upload{
			SessionID: sid, OrigPath: "x.txt", CopyPath: "y.txt", Name: "up.txt",
			Size: 3, CreatedAt: now(),
		}); err != nil {
			t.Fatalf("落上传 %s 失败: %v", sid, err)
		}
	}
	seed(main.ID, 3)
	seed(sub.ID, 1)
	seed(task.ID, 1)
	return &assistantOpsFixture{app: a, main: main, sub: sub, task: task}
}

// post 跑一次处理函数，返回记录器（处理函数只吃 *http.Request 与 ResponseWriter）。
func post(t *testing.T, h func(http.ResponseWriter, *http.Request), url string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	h(w, httptest.NewRequest("POST", url, nil))
	return w
}

func msgCount(t *testing.T, a *App, sid string) int {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读会话 %s 历史失败: %v", sid, err)
	}
	return len(all)
}

// 清空：消息与随消息工件真删、会话本体与设置保留、别的会话不受影响。
func TestAssistantClearWipesMainConversation(t *testing.T) {
	f := newAssistantOpsFixture(t)
	a, main := f.app, f.main

	// 清空前主对话还有一堆会话级状态：待生效排队/插话、报错条、异常中断标记
	a.pushPending(main.ID, "queue", "排队的消息", "", "")
	a.pushPending(main.ID, "steer", "插话", "", "")
	a.setRunError(main.ID, "上一轮失败")
	if err := a.Store.SetLastStop(main.ID, "error"); err != nil {
		t.Fatal(err)
	}
	before := msgCount(t, a, main.ID)
	if before != 12 { // 3 轮 × 4 行
		t.Fatalf("主对话应有 12 条消息, got %d", before)
	}

	w := post(t, a.handleAssistantClear, "/api/assistant/clear")
	if w.Code != 200 {
		t.Fatalf("清空应成功, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK        int    `json:"ok"`
		SessionID string `json:"sessionId"`
		Cleared   int64  `json:"cleared"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是预期 JSON: %v (%s)", err, w.Body.String())
	}
	if resp.OK != 1 || resp.SessionID != main.ID || resp.Cleared != int64(before) {
		t.Fatalf("响应字段不对: %+v（清空前 %d 条）", resp, before)
	}
	if resp.SessionID == "" {
		t.Fatalf("响应应带上被清空的会话 id")
	}

	// 消息与工件清空
	if n := msgCount(t, a, main.ID); n != 0 {
		t.Fatalf("清空后主对话不该有消息, got %d", n)
	}
	if cps, err := a.Store.CheckpointsAfter(main.ID, 0); err != nil || len(cps) != 0 {
		t.Fatalf("清空后不该有快照登记, got %d err=%v", len(cps), err)
	}
	if dvs, err := a.Store.DeliveriesFor(main.ID); err != nil || len(dvs) != 0 {
		t.Fatalf("清空后不该有交付登记, got %d err=%v", len(dvs), err)
	}
	if ups, err := a.Store.UploadsFor(main.ID); err != nil || len(ups) != 0 {
		t.Fatalf("清空后不该有上传登记, got %d err=%v", len(ups), err)
	}

	// 会话本体保留：同一个 id、同一个标题与工作目录、模式不变、状态回空闲
	m, err := a.Store.Get(main.ID)
	if err != nil {
		t.Fatalf("清空不该删掉会话本身: %v", err)
	}
	if m.Mode != "assistant" || m.Title != "数字助理主对话" || m.Workspace != main.Workspace {
		t.Fatalf("清空不该改会话设置: %+v", m)
	}
	if m.Status != "idle" || m.Unread || m.UsageCalls != 0 || m.UsagePrompt != 0 {
		t.Fatalf("清空后会话运行态应归零: status=%q unread=%v calls=%d prompt=%d",
			m.Status, m.Unread, m.UsageCalls, m.UsagePrompt)
	}
	if a.Store.LastStops()[main.ID] != "" {
		t.Fatalf("清空后不该还有异常中断标记")
	}

	// 待生效排队与报错条一并清掉
	if st := a.pendingOf(main.ID); len(st.Queue) != 0 || len(st.Steer) != 0 {
		t.Fatalf("清空后不该还有待生效消息: queue=%d steer=%d", len(st.Queue), len(st.Steer))
	}
	if a.runError(main.ID) != "" {
		t.Fatalf("清空后报错条应消失, got %q", a.runError(main.ID))
	}

	// 子会话与别的会话不受影响
	if n := msgCount(t, a, f.sub.ID); n != 4 {
		t.Fatalf("子会话历史不该被动：期望 4 条, got %d", n)
	}
	if n := msgCount(t, a, f.task.ID); n != 4 {
		t.Fatalf("工作任务会话历史不该被动：期望 4 条, got %d", n)
	}
	if cps, _ := a.Store.CheckpointsAfter(f.sub.ID, 0); len(cps) != 1 {
		t.Fatalf("子会话的快照登记不该被动, got %d", len(cps))
	}
	if sm, err := a.Store.Get(f.sub.ID); err != nil || sm.ParentID != main.ID {
		t.Fatalf("子会话本体不该被动: %+v err=%v", sm, err)
	}
}

// 执行中拒绝清空与压缩（正在跑的一轮会继续往历史里写，清了也会被冲回来）。
func TestAssistantOpsRefusedWhileRunning(t *testing.T) {
	f := newAssistantOpsFixture(t)
	a := f.app
	a.runMu.Lock()
	a.runners = map[string]*sessionRun{f.main.ID: {}}
	a.runMu.Unlock()
	defer func() {
		a.runMu.Lock()
		delete(a.runners, f.main.ID)
		a.runMu.Unlock()
	}()

	for _, tc := range []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
	}{
		{"clear", a.handleAssistantClear},
		{"compact", a.handleAssistantCompact},
	} {
		w := post(t, tc.h, "/api/assistant/"+tc.name)
		if w.Code != 409 {
			t.Fatalf("%s 执行中应 409, got %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if n := msgCount(t, a, f.main.ID); n != 12 {
		t.Fatalf("被拒绝的请求不该改动历史, got %d 条", n)
	}
}

// 没有数字助理主对话时：两个动作都给 404，且不代为建出一个空对话。
func TestAssistantOpsWithoutMainSession(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.Providers = []ProviderCfg{{ID: "mock", BaseURL: "mock"}}
	for _, tc := range []struct {
		name string
		h    func(http.ResponseWriter, *http.Request)
	}{
		{"clear", a.handleAssistantClear},
		{"compact", a.handleAssistantCompact},
	} {
		w := post(t, tc.h, "/api/assistant/"+tc.name)
		if w.Code != 404 {
			t.Fatalf("%s 没有主对话应 404, got %d %s", tc.name, w.Code, w.Body.String())
		}
	}
	if list, err := a.Store.List(); err != nil || len(list) != 0 {
		t.Fatalf("404 路径不该顺手建会话, got %d 个 err=%v", len(list), err)
	}
}

// 压缩：即使全局压缩策略是「规则折叠」（CompactMode=rule），设置页这个按钮
// 也必须走模型摘要——落 origin=manual 的摘要行，且绝不产生规则折叠标记。
func TestAssistantCompactAlwaysModelSummary(t *testing.T) {
	f := newAssistantOpsFixture(t)
	a, main := f.app, f.main
	a.Cfg.CompactMode = "rule" // 全局策略故意设成规则折叠
	a.Cfg.KeepChars = 10       // 预算极小：除最近一轮外的历史都在可压区间里

	// 先塞一条规则折叠标记，确认压缩不会去改它、也不会新增
	if err := a.Store.SetCompact(main.ID, 12, "旧折叠视图"); err != nil {
		t.Fatal(err)
	}

	w := post(t, a.handleAssistantCompact, "/api/assistant/compact")
	if w.Code != 200 {
		t.Fatalf("压缩应成功, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		OK        int    `json:"ok"`
		SessionID string `json:"sessionId"`
		Strategy  string `json:"strategy"`
		Summary   string `json:"summary"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是预期 JSON: %v (%s)", err, w.Body.String())
	}
	if resp.Strategy != "llm" {
		t.Fatalf("压缩方式必须固定为模型摘要(llm), got %q", resp.Strategy)
	}
	if resp.SessionID != main.ID {
		t.Fatalf("响应应带上主对话 id, got %q", resp.SessionID)
	}

	all, err := a.Store.RawMessagesAll(main.ID)
	if err != nil {
		t.Fatal(err)
	}
	summaries, folds := 0, 0
	for _, m := range all {
		if m.Kind == kindSummary {
			summaries++
			if m.Origin != "manual" {
				t.Fatalf("摘要来源应是 manual（手动按钮），got %q", m.Origin)
			}
			if m.CoversUpTo <= 0 {
				t.Fatalf("摘要应带覆盖边界 coversUpTo, got %d", m.CoversUpTo)
			}
			if !strings.Contains(m.Text(), "历史摘要") {
				t.Fatalf("摘要行正文不像摘要: %q", m.Text())
			}
		}
		if m.Compact == "旧折叠视图" { // 预置的那条保持原样
			continue
		}
		if m.Compact != "" {
			folds++
		}
	}
	if summaries != 1 {
		t.Fatalf("应落 1 条摘要行, got %d", summaries)
	}
	if folds != 0 {
		t.Fatalf("rule 模式下也不该发生规则折叠, 新增折叠行 %d 条", folds)
	}
	// 对话区留痕：一条【已压缩】系统通知（report/origin=compact）——摘要行只是
	// 上下文材料、不进普通消息流，用户靠这条通知在对话里看到压缩结果
	note := ""
	for _, m := range all {
		if m.Kind == kindReport && m.Origin == "compact" {
			note = m.Text()
		}
	}
	if note == "" {
		t.Fatalf("对话区应有【已压缩】系统通知")
	}
	if !strings.HasPrefix(note, "【已压缩】") || !strings.Contains(note, "覆盖至第 ") {
		t.Fatalf("通知文案不对: %q", note)
	}
	if !strings.Contains(note, "离线演示摘要") { // 摘要开头被带进通知
		t.Fatalf("通知里应带上摘要开头: %q", note)
	}
	if resp.Summary == "" {
		t.Fatalf("响应应带上摘要开头")
	}
}

// 全局压缩「关闭」（CompactMode=off）时，设置页这个手动入口仍照做模型摘要：
// 这是用户显式点按钮的动作，不受自动压缩开关约束。
func TestAssistantCompactWorksWhenAutoCompactOff(t *testing.T) {
	f := newAssistantOpsFixture(t)
	a, main := f.app, f.main
	a.Cfg.CompactMode = "off"
	a.Cfg.KeepChars = 10

	w := post(t, a.handleAssistantCompact, "/api/assistant/compact")
	if w.Code != 200 {
		t.Fatalf("手动压缩不该被 off 拦住, got %d %s", w.Code, w.Body.String())
	}
	summaries := 0
	all, _ := a.Store.RawMessagesAll(main.ID)
	for _, m := range all {
		if m.Kind == kindSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("应落 1 条模型摘要行, got %d", summaries)
	}
}

// 只有一轮历史且该轮自身超预算：按「命中轮整轮保留」的硬口径——预算压缩
// 不切轮，如实报「没有可压缩的区间」（压缩边界只在轮与轮之间）；真撞模型
// 窗口时由溢出兜底（keep=0，整轮强压）处理，不走预算路径。
// 早年这里断言「超预算单轮也要压掉」——那个口径靠把一轮切成两半来降上下文，
// 实测普通任务轮也被判成巨轮、压缩落在轮中间，已砍掉。
func TestAssistantCompactSingleOversizedRoundKeptWhole(t *testing.T) {
	f := newAssistantOpsFixture(t)
	a, main := f.app, f.main
	if _, err := a.Store.ClearSession(main.ID); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, main.ID, "唯一一轮", true)
	a.Cfg.KeepChars = 10 // 预算小于这一轮：命中轮整轮保留，预算压缩压不动

	w := post(t, a.handleAssistantCompact, "/api/assistant/compact")
	if w.Code != 400 || !strings.Contains(w.Body.String(), "没有可压缩的区间") {
		t.Fatalf("超预算单轮应整轮保留、如实报无可压区间, got %d %s", w.Code, w.Body.String())
	}
	if all, _ := a.Store.RawMessagesAll(main.ID); len(all) != 0 {
		for _, m := range all {
			if m.Kind == kindSummary {
				t.Fatal("压不动时不应落摘要行")
			}
		}
	}
	// 溢出兜底（keep=0：摘要覆盖全部历史）能压——撞窗口的最后防线
	if _, err := a.doSummarize(main, "manual", 0, 0); err != nil {
		t.Fatalf("溢出兜底应能压超长单轮: %v", err)
	}
	summaries := 0
	all, _ := a.Store.RawMessagesAll(main.ID)
	for _, m := range all {
		if m.Kind == kindSummary {
			summaries++
		}
	}
	if summaries != 1 {
		t.Fatalf("溢出兜底应落 1 条模型摘要行, got %d", summaries)
	}
}

// 历史都在保留预算内（预算极大）：按钮与自动压缩同一口径——没有可压区间
// 就是 400 报错，不额外发明「退化为只留最近一轮」之类的私有行为。
func TestAssistantCompactWithinBudgetIsNoop(t *testing.T) {
	f := newAssistantOpsFixture(t)
	a, main := f.app, f.main
	a.Cfg.KeepChars = 1000000 // 预算足够大：全部历史都在保留窗口内

	w := post(t, a.handleAssistantCompact, "/api/assistant/compact")
	if w.Code != 400 {
		t.Fatalf("预算内没东西可压应 400 透传现有文案, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "没有可压缩的区间") {
		t.Fatalf("应透传现有错误文案, got %s", w.Body.String())
	}
	if n := msgCount(t, a, main.ID); n != 12 {
		t.Fatalf("无操作的压缩不该改动历史, got %d 条", n)
	}
	all, _ := a.Store.RawMessagesAll(main.ID)
	for _, m := range all {
		if m.Kind == kindSummary {
			t.Fatalf("不该落摘要行")
		}
		if m.Compact != "" {
			t.Fatalf("不该有折叠标记（第 %d 行 %q）", m.Seq, m.Compact)
		}
	}
}

// 摘要指令按会话模式二选一：数字助理主对话用专属指令（记住用户偏好与长期
// 约定），其他会话沿用通用指令。
func TestSummarizeInstructionForMode(t *testing.T) {
	ast := summarizeSectionsFor(&SessionMeta{Mode: "assistant"})
	if !strings.Contains(ast, "用户偏好与习惯") || !strings.Contains(ast, "长期有效的约定") {
		t.Fatalf("数字助理主对话应用专属指令, got %q", ast)
	}
	for _, mode := range []string{"task", "coding", "office", "aux", ""} {
		if got := summarizeSectionsFor(&SessionMeta{Mode: mode}); got != summarizeSections {
			t.Fatalf("mode=%q 应回退通用指令", mode)
		}
	}
	if summarizeSectionsFor(nil) != summarizeSections {
		t.Fatalf("nil 会话应回退通用指令")
	}
}
