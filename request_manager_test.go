package main

// 「任务找经理协调」通道（requestmgr.go 的 request_manager）的测试：
//   - 工具目录：任务类五个执行模式都有它，数字助理（被找的那个）与 aux 没有；
//   - 求助落到经理会话：kind=aiuser、origin=help、【任务求助】前缀与四个字段；
//   - 语义是「发完就走」：工具立即返回、返回值里写明不用等（不阻塞）；
//   - 经理空闲 → 占住它这一轮并唤醒；经理正忙 → 进它的排队队列（同一套队列）；
//   - 边界：子对话调用被硬拦、计划模式暂停、topic/detail 必填、
//     求助不改动任何会话的权限档（这条通道不构成提权路径）；
//   - 提示词成对到位：执行模式拿「找经理帮忙」、数字助理拿「任务来找你协调」。
// 刻意不配模型提供方：被唤醒的那一轮会在后台立刻失败收尾，不影响断言。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newRequestMgrTestApp 建一个带临时库与临时工作区的 App，**不预置长期助理会话**
// （求助通道要能自己找/建它），并返回一个顶层任务会话。
func newRequestMgrTestApp(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{} // NewApp 才有的一份运行态（sessLock 用），测试里补上
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = nil
	task := &SessionMeta{ID: "task-a", Title: "登录页样式", Mode: "coding",
		Workspace: a.Cfg.Workspace, Permission: permWorkspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(task); err != nil {
		t.Fatalf("建任务会话失败: %v", err)
	}
	return a, task
}

// managerOf 从库里取数字助理主对话（没建出来就返回 nil）。
func managerOf(t *testing.T, a *App) *SessionMeta {
	t.Helper()
	list, err := a.Store.List()
	if err != nil {
		t.Fatalf("读会话列表失败: %v", err)
	}
	for _, m := range list {
		if m.Mode == "assistant" && m.ParentID == "" {
			return m
		}
	}
	return nil
}

// 工具按模式分发：任务类五个执行模式都有 request_manager；被找的数字助理与
// 只读的 aux 都不该有（它是发起方的能力，不是接收方的）。
func TestRequestManagerToolVisibility(t *testing.T) {
	a, _ := newRequestMgrTestApp(t)
	for _, mode := range []string{"task", "office", "coding", "beginner-coding", "skill-create"} {
		if !toolNames(t, a, mode)["request_manager"] {
			t.Fatalf("模式 %s 的工具目录里缺少 request_manager", mode)
		}
	}
	for _, mode := range []string{"assistant", "aux"} {
		if toolNames(t, a, mode)["request_manager"] {
			t.Fatalf("模式 %s 不该有 request_manager（它是被找的那个，不是发起方）", mode)
		}
	}
	a.assertManagerChannel() // 启动自检同一份口径（不一致即 panic）
}

// 计划模式下暂停：目录里留着（请求形状稳定），调用时硬拦。
func TestRequestManagerSuspendedInPlan(t *testing.T) {
	if !suspendedInPlan("coding", "request_manager") {
		t.Fatal("计划模式下 request_manager 应被暂停（计划阶段只做只读调研）")
	}
	if suspendedInPlan("coding", "read") {
		t.Fatal("只读工具不该被暂停")
	}
}

// 主路径：求助落进经理会话（自建经理 + 消息身份/正文），工具立即返回且写明不用等；
// 经理空闲时被占住执行权唤醒（Started 路径），任务自己的权限档不受影响。
func TestRequestManagerDeliversToManager(t *testing.T) {
	a, task := newRequestMgrTestApp(t)
	if m := managerOf(t, a); m != nil {
		t.Fatal("前置条件：此时还没有数字助理主对话，应由求助通道自己建")
	}
	out, err := runRequestManager(dispatchCtx(a, task), mustArgs(t, map[string]any{
		"topic":       "仓库 X 该跑测试了",
		"detail":      "我把登录页样式改完了（web/app-chat.js、web/style.css），想统一跑一次测试",
		"resource":    `仓库 D:\x 的跑测试`,
		"expectation": "排个顺序",
	}))
	if err != nil {
		t.Fatalf("求助失败: %v", err)
	}
	if !strings.Contains(out, "已发给长期助理") || !strings.Contains(out, "不用等") {
		t.Fatalf("返回值应写明已发出且不用等：%s", out)
	}
	mgr := managerOf(t, a)
	if mgr == nil {
		t.Fatal("求助通道应自己建出数字助理主对话")
	}
	rows, err := a.Store.RawMessagesAll(mgr.ID)
	if err != nil {
		t.Fatalf("读经理会话历史失败: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("经理会话应恰好收到 1 条求助, got %d", len(rows))
	}
	r := rows[0]
	if r.Kind != kindAiUser || r.Origin != "help" {
		t.Fatalf("求助行身份应为 kind=%s origin=help, got kind=%s origin=%s", kindAiUser, r.Kind, r.Origin)
	}
	text := r.Text()
	for _, want := range []string{"【任务求助】", "登录页样式", task.ID, "仓库 X 该跑测试了",
		"web/app-chat.js", `仓库 D:\x 的跑测试`, "排个顺序", "不等答复"} {
		if !strings.Contains(text, want) {
			t.Fatalf("求助正文缺少 %q：\n%s", want, text)
		}
	}
	// 权限档不因求助发生任何变化（通道只传消息，不构成提权路径）
	if after, _ := a.Store.Get(task.ID); permModeOf(after) != permWorkspace {
		t.Fatalf("求助不该改发起方权限档, got %s", permModeOf(after))
	}
	// 建会话走 createSession 单源：assistant 主对话的默认档是完全访问
	//（见 sessioncreate.go——总控先站到最高档，派出去的任务才有完整的档位空间；
	// 前端建主对话一直走这条路）。求助通道只传消息，不改变任何一边的档位。
	if permModeOf(mgr) != permFull {
		t.Fatalf("新建的经理会话应按 assistant 默认档（完全访问）, got %s", permModeOf(mgr))
	}
}

// 经理正忙：求助插话注入它正在执行的那一轮（AI 发话一律插话，不排队）。
func TestRequestManagerQueuesWhenManagerBusy(t *testing.T) {
	a, task := newRequestMgrTestApp(t)
	mgr := &SessionMeta{ID: "mgr", Title: "数字助理主对话", Mode: "assistant",
		Workspace: a.Cfg.Workspace, Permission: permWorkspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(mgr); err != nil {
		t.Fatal(err)
	}
	run, ok := a.beginRun(mgr.ID) // 占住经理这一轮，模拟它正在忙
	if !ok {
		t.Fatal("占住经理执行权失败")
	}
	defer a.finishRun(mgr.ID)

	out, err := runRequestManager(dispatchCtx(a, task), mustArgs(t, map[string]any{
		"topic":  "要跑测试",
		"detail": "改完了，想知道现在合不合适跑",
	}))
	if err != nil {
		t.Fatalf("求助失败: %v", err)
	}
	if !strings.Contains(out, "已插话") {
		t.Fatalf("经理忙时应说明已插话：%s", out)
	}
	st := a.pendingOf(mgr.ID)
	if st == nil || len(st.Steer) != 1 {
		t.Fatalf("求助应插进经理的插话队列, got %+v", st)
	}
	if st.Steer[0].Origin != "help" || st.Steer[0].Kind != kindAiUser {
		t.Fatalf("插话项身份应为 kind=%s origin=help, got %+v", kindAiUser, st.Steer[0])
	}
	if !strings.Contains(st.Steer[0].Content, "【任务求助】") {
		t.Fatalf("插话正文应有【任务求助】前缀：%s", st.Steer[0].Content)
	}
	if len(st.Queue) != 0 {
		t.Fatalf("求助不该进排队队列（AI 发话一律插话），got %+v", st.Queue)
	}
	_ = run
}

// 边界：子对话调不通（与「子对话只有一级」同口径）；topic/detail 必填。
func TestRequestManagerGuards(t *testing.T) {
	a, task := newRequestMgrTestApp(t)
	child := &SessionMeta{ID: "child", Title: "子对话", Mode: "coding", ParentID: task.ID,
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	if _, err := runRequestManager(dispatchCtx(a, child), mustArgs(t, map[string]any{
		"topic": "要跑测试", "detail": "改完了",
	})); err == nil || !strings.Contains(err.Error(), "子对话") {
		t.Fatalf("子对话调用应被硬拦，got %v", err)
	}
	if _, err := runRequestManager(dispatchCtx(a, task), mustArgs(t, map[string]any{
		"detail": "改完了",
	})); err == nil || !strings.Contains(err.Error(), "topic") {
		t.Fatalf("缺 topic 应报错，got %v", err)
	}
	if _, err := runRequestManager(dispatchCtx(a, task), mustArgs(t, map[string]any{
		"topic": "要跑测试",
	})); err == nil || !strings.Contains(err.Error(), "detail") {
		t.Fatalf("缺 detail 应报错，got %v", err)
	}
	// 被硬拦/参数报错时不该有任何消息落到经理会话
	mgr := managerOf(t, a)
	if mgr != nil {
		rows, _ := a.Store.RawMessagesAll(mgr.ID)
		if len(rows) != 0 {
			t.Fatalf("报错路径不该投递消息, got %d 条", len(rows))
		}
	}
}

// 提示词成对到位：执行模式拿「找经理帮忙」，数字助理拿「任务来找你协调」，aux 都没有。
func TestManagerPromptWiring(t *testing.T) {
	a, _ := newRequestMgrTestApp(t)
	for _, mode := range []string{"task", "office", "coding", "beginner-coding", "skill-create"} {
		sys := a.systemPrompt(&SessionMeta{Mode: mode, Workspace: a.Cfg.Workspace})
		if !strings.Contains(sys, "## 找经理帮忙") || !strings.Contains(sys, "request_manager") {
			t.Fatalf("模式 %s 的系统提示词缺少「找经理帮忙」段", mode)
		}
		if !strings.Contains(sys, "不要滥用") {
			t.Fatalf("模式 %s 的提示词应带一句「不要滥用」", mode)
		}
		if strings.Contains(sys, "## 任务来找你协调") {
			t.Fatalf("模式 %s 不该拿到数字助理的处理准则", mode)
		}
	}
	as := a.systemPrompt(&SessionMeta{Mode: "assistant", Workspace: a.Cfg.Workspace})
	if !strings.Contains(as, "## 任务来找你协调") || !strings.Contains(as, "不会等你") {
		t.Fatalf("数字助理的系统提示词缺少「任务来找你协调」段：%.400s", as)
	}
	if strings.Contains(as, "## 找经理帮忙") {
		t.Fatal("数字助理不该拿到发起侧的「找经理帮忙」段")
	}
	aux := a.systemPrompt(&SessionMeta{Mode: "aux", Workspace: a.Cfg.Workspace})
	if strings.Contains(aux, "## 找经理帮忙") || strings.Contains(aux, "## 任务来找你协调") {
		t.Fatal("aux 两侧都不该有（它既不发求助也不处理求助）")
	}
}

// 工具说明与参数表就位（模型据此判断何时求助、怎么填）。
func TestRequestManagerToolSchema(t *testing.T) {
	props := paramTypes(t, requestManagerTool())
	for _, k := range []string{"topic", "detail", "resource", "expectation"} {
		if _, ok := props[k]; !ok {
			t.Fatalf("request_manager 缺参数 %s", k)
		}
	}
	spec := requestManagerTool().Spec.Function
	for _, want := range []string{"跑测试", "不要滥用", "不用等", "dispatch_sub"} {
		if !strings.Contains(spec.Description, want) {
			t.Fatalf("工具说明缺少 %q：%s", want, spec.Description)
		}
	}
	req, _ := spec.Parameters["required"].([]string)
	if len(req) != 2 {
		t.Fatalf("required 应恰好是 topic/detail，got %v", req)
	}
}

// ---------- 端到端（假模型）：任务求助 → 经理被唤醒 → 用既有工具回话 ----------

// fakeModelServer 起一个 OpenAI 兼容的假模型服务（SSE，不联网、不花额度），
// 按请求里带的工具分流，把整条链路跑通：
//   - 带 request_manager 的（任务会话）：第一次先调 request_manager 求助，
//     拿到工具结果后收尾；
//   - 带 send_to_session 的（数字助理）：第一次调 send_to_session 把话插回任务
//     会话（**用既有工具**，本功能不新增经理侧工具），之后收尾。
//
// requests 记下每个请求的形态，断言失败时好排查。
func fakeModelServer(t *testing.T, taskID string, requests *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req ChatRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		tools := map[string]bool{}
		for _, tl := range req.Tools {
			tools[tl.Function.Name] = true
		}
		results := 0
		for _, m := range req.Messages {
			if m.Role == "tool" {
				results++
			}
		}
		which := "other"
		switch {
		case tools["request_manager"]:
			which = "task"
		case tools["send_to_session"]:
			which = "manager"
		}
		mu.Lock()
		*requests = append(*requests, which+"/toolResults="+itoa(results))
		mu.Unlock()

		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		write := func(payload []byte) {
			fmt.Fprintf(w, "data: %s\n\n", payload)
			if flusher != nil {
				flusher.Flush()
			}
		}
		say := func(text string) {
			chunk, _ := json.Marshal(map[string]any{"choices": []any{
				map[string]any{"delta": map[string]any{"content": text}},
			}})
			write(chunk)
		}
		call := func(id, name string, args map[string]any) {
			raw, _ := json.Marshal(args)
			chunk, _ := json.Marshal(map[string]any{"choices": []any{
				map[string]any{"delta": map[string]any{"tool_calls": []any{
					map[string]any{"index": 0, "id": id,
						"function": map[string]any{"name": name, "arguments": string(raw)}},
				}}},
			}})
			write(chunk)
		}
		switch {
		case which == "task" && results == 0:
			call("call_task_1", "request_manager", map[string]any{
				"topic":    "仓库 X 该跑测试了",
				"detail":   "我把登录页样式改完了（web/style.css），想统一跑一次测试",
				"resource": "仓库 X 的跑测试",
			})
		case which == "task":
			say("已通知经理，我继续改代码。")
		case which == "manager" && results == 0:
			call("call_mgr_1", "send_to_session", map[string]any{
				"session_id": taskID,
				"task":       "先别动，等 B 完事",
			})
		default:
			say("收到，正在协调。")
		}
		write([]byte("[DONE]"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// waitFor 轮询等待条件成立（后台唤醒体是 goroutine，断言必须等它落库）。
func waitFor(t *testing.T, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", desc)
}

// 整条链路（真执行体 + 假模型）：任务会话调 request_manager → 经理会话收到
// 【任务求助】并被唤醒 → 经理用既有工具 send_to_session 把话插回任务会话 →
// 任务会话继续干完这一轮。全程不联网、不花额度。
func TestRequestManagerEndToEndWithFakeModel(t *testing.T) {
	a, task := newRequestMgrTestApp(t)
	var mu sync.Mutex
	requests := []string{}
	srv := fakeModelServer(t, task.ID, &requests, &mu)
	a.Cfg.Providers = []ProviderCfg{{ID: "fake", Name: "假模型", BaseURL: srv.URL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-1", Name: "假模型"}}}}
	a.Cfg.BaseURL = "" // 不留裸配置回落，调用一律走假模型
	// 默认模型体系：会话没手选时跟全局默认模型（不再拿第一个渠道充数）
	a.Cfg.DefaultModel = ModelRef{Provider: "fake", Model: "fake-1"}

	run, ok := a.beginRun(task.ID)
	if !ok {
		t.Fatal("占住任务会话执行权失败")
	}
	kickSessionRound(a, task, run) // 与真实路径同一套唤醒体

	// ① 模型真的调到了 request_manager，工具结果写明已发出且不用等
	waitFor(t, "任务侧工具调用落库", func() bool { return msgHas(t, a, task.ID, "request_manager") })
	waitFor(t, "任务侧工具结果落库", func() bool { return msgHas(t, a, task.ID, "已发给长期助理", "不用等") })
	// ② 经理会话收到求助（没经理会话就自动建出来），身份是 aiuser/origin=help
	mgr := managerOf(t, a)
	if mgr == nil {
		t.Fatal("这轮跑完应已自动建出数字助理主对话")
	}
	waitFor(t, "经理收到【任务求助】", func() bool {
		return msgHas(t, a, mgr.ID, "【任务求助】", "仓库 X 该跑测试了")
	})
	rows, err := a.Store.RawMessagesAll(mgr.ID)
	if err != nil {
		t.Fatal(err)
	}
	reqCount := 0
	for _, r := range rows {
		if r.Origin == "help" {
			reqCount++
			if r.Kind != kindAiUser {
				t.Fatalf("求助行 kind 应为 %s, got %s", kindAiUser, r.Kind)
			}
		}
	}
	if reqCount != 1 {
		t.Fatalf("求助应恰好落一条, got %d", reqCount)
	}
	// ③ 经理被唤醒后用既有工具把话插回任务会话（本功能没有新增经理侧工具）
	waitFor(t, "经理回话到任务会话", func() bool {
		return msgHas(t, a, task.ID, "【数字助理转达】", "先别动，等 B 完事")
	})
	// ④ 任务没有被求助卡住：这一轮照常收尾
	waitFor(t, "任务本轮正常收尾", func() bool {
		return msgHas(t, a, task.ID, "已通知经理，我继续改代码")
	})

	mu.Lock()
	got := strings.Join(requests, " | ")
	mu.Unlock()
	if !strings.Contains(got, "task/toolResults=0") || !strings.Contains(got, "manager/toolResults=0") {
		t.Fatalf("链路没走全（任务发起 + 经理处理）：%s", got)
	}
}
