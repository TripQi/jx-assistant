package main

// call_api 的验收测试：
//  1. 完整目录：全档接口都在（不按会话档位过滤）、每条标注最低权限档；
//     ⚠ 标记危险接口；
//  2. 直调真的打到 handler（GET /api/modes 返回与前端同一份模式清单）；
//  3. 未开放接口与低档位调用被拒（目录闸 + 权限闸）；
//  4. 危险接口走审批卡：用户拒绝则取消、批准才执行（真的删掉会话）；
//  5. 工具注册与模式集（仅 assistant 注入），工具说明携带完整目录，环境
//     快照不再渲染「可调接口」区块。

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// mkCallAPISession 建一个指定权限档的 task 会话并返回其 toolContext。
func mkCallAPISession(t *testing.T, a *App, id, perm string) *toolContext {
	t.Helper()
	m := &SessionMeta{ID: id, Title: "callapi 测试", Mode: "task", Permission: perm,
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话 %s 失败: %v", id, err)
	}
	return &toolContext{app: a, sess: m, emit: func(Event) {}, ctx: context.Background()}
}

// ① 完整目录：各档接口齐全（档位差异由调用时的权限闸校验，目录不过滤），
// 每条标注最低权限档；危险接口带 ⚠。
func TestCallAPICatalogFull(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	cat := a.apiCatalogText()

	// 各档接口齐全：只读 / 读写 / 完全访问 / 危险各至少一条
	for _, want := range []string{"GET /api/status", "POST /api/sessions [", "POST /api/config [", "DELETE /api/sessions/{id} [",
		// 组件（内置/用户两档）与插件的清单与开关：长期助理要能查能开关（含会话级覆盖）
		"GET /api/components [", "POST /api/components [", "GET /api/plugins [", "POST /api/plugins ["} {
		if !strings.Contains(cat, want) {
			t.Fatalf("完整目录应含 %s：\n%s", want, cat)
		}
	}
	// 每条都标注最低权限档（三档之一）
	for _, l := range strings.Split(cat, "\n") {
		if !strings.Contains(l, "[全电脑只读]") && !strings.Contains(l, "[修改工作区]") && !strings.Contains(l, "[完全访问]") {
			t.Fatalf("目录每行都应标注最低权限档，got：%q", l)
		}
	}
	// 危险标记：DELETE 会话带 ⚠（调用前审批）
	line := ""
	for _, l := range strings.Split(cat, "\n") {
		if strings.Contains(l, "DELETE /api/sessions/{id}") {
			line = l
		}
	}
	if line == "" || !strings.HasPrefix(strings.TrimSpace(line), "⚠") {
		t.Fatalf("危险接口应带 ⚠ 前缀，got：%q", line)
	}
}

// ② 直调真的执行 handler：GET /api/modes 返回前端模式下拉同一份清单。
func TestCallAPIDirectInvokeModes(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ctx := mkCallAPISession(t, a, "t-ws", permWorkspace)
	out, err := runCallAPI(ctx, mustArgs(t, map[string]any{"path": "/api/modes"}))
	if err != nil {
		t.Fatalf("GET /api/modes 失败: %v", err)
	}
	if !strings.Contains(out, "HTTP 200") {
		t.Fatalf("应返回 HTTP 200：\n%s", out)
	}
	for _, m := range chatModes {
		if !strings.Contains(out, m.Value) || !strings.Contains(out, m.Label) {
			t.Fatalf("响应应含模式清单（%s=%s）：\n%s", m.Value, m.Label, out)
		}
	}
}

// ②b 带路径参数与请求体的直调：POST /api/sessions 真的建出会话（与前端同一
// handler、同一校验），再 PATCH 改标题。
func TestCallAPIDirectCreateAndPatch(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ctx := mkCallAPISession(t, a, "t-ws", permWorkspace)
	out, err := runCallAPI(ctx, mustArgs(t, map[string]any{
		"path": "/api/sessions", "method": "POST",
		"params": map[string]any{"title": "callapi 建的会话", "mode": "office"},
	}))
	if err != nil {
		t.Fatalf("POST /api/sessions 失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "HTTP 200") || !strings.Contains(out, "callapi 建的会话") {
		t.Fatalf("建会话应成功并回显标题：\n%s", out)
	}
	sessions, err := a.Store.List()
	if err != nil {
		t.Fatal(err)
	}
	var id string
	for _, m := range sessions {
		if m.Title == "callapi 建的会话" && m.Mode == "office" {
			id = m.ID
		}
	}
	if id == "" {
		t.Fatal("会话没有真的落库")
	}
	out, err = runCallAPI(ctx, mustArgs(t, map[string]any{
		"path": "/api/sessions/" + id, "method": "PATCH",
		"params": map[string]any{"title": "改过的标题"},
	}))
	if err != nil || !strings.Contains(out, "HTTP 200") || !strings.Contains(out, "改过的标题") {
		t.Fatalf("PATCH 改标题应成功：%v\n%s", err, out)
	}
}

// ②c POST params 的字符串形态：params 的 schema 曾声明为 string，按 schema
// 行事的模型会把请求体对象先序列化成字符串再传（args 里是
// "params":"{\"title\":…}" 的双重编码形态）——剥掉外壳后到达处理器的必须是
// 对象本体，POST 建会话与对象形态行为一致。
func TestCallAPIDirectPostStringParams(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ctx := mkCallAPISession(t, a, "t-str", permWorkspace)
	// params 传 Go 字符串：mustArgs 序列化后正是模型双重编码出的形态
	out, err := runCallAPI(ctx, mustArgs(t, map[string]any{
		"path": "/api/sessions", "method": "POST",
		"params": `{"title":"字符串形态建的会话","mode":"task"}`,
	}))
	if err != nil {
		t.Fatalf("POST /api/sessions（字符串形态 params）失败: %v\n%s", err, out)
	}
	if !strings.Contains(out, "HTTP 200") || !strings.Contains(out, "字符串形态建的会话") {
		t.Fatalf("字符串形态 params 应与对象形态同样建出会话并回显标题：\n%s", out)
	}
	found := false
	if sessions, _ := a.Store.List(); err == nil {
		for _, m := range sessions {
			if m.Title == "字符串形态建的会话" && m.Mode == "task" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("会话没有真的落库（处理器拿到的 body 不是对象）")
	}
}

// ②d 实报 bug 的还原：call_api POST /api/sessions/{id}/interject——修复前
// params 被序列化成字符串，处理器解码失败被报成「content 不能为空」。会话
// 标记为执行中：steer 生效且不会顺手拉起真执行。对象与字符串两种形态都要
// 把 content 送进插话队列。
func TestCallAPIDirectInterject(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ctx := mkCallAPISession(t, a, "t-inj", permWorkspace)
	a.runners["t-inj"] = &sessionRun{} // 执行中：interject 走 steer，不 kickQueue
	defer delete(a.runners, "t-inj")

	for name, params := range map[string]any{
		"对象形态":   map[string]any{"content": "对象形态插话", "mode": "steer"},
		"字符串形态": `{"content":"字符串形态插话","mode":"steer"}`,
	} {
		out, err := runCallAPI(ctx, mustArgs(t, map[string]any{
			"path": "/api/sessions/t-inj/interject", "method": "POST", "params": params,
		}))
		if err != nil || !strings.Contains(out, "HTTP 200") || !strings.Contains(out, `"mode":"steer"`) {
			t.Fatalf("%s的 interject 应成功并入插话队列：%v\n%s", name, err, out)
		}
	}
	st := a.pendingOf("t-inj")
	if len(st.Steer) != 2 {
		t.Fatalf("插话队列应有 2 条（对象/字符串形态各一），实际 %d", len(st.Steer))
	}
	joined := st.Steer[0].Content + "|" + st.Steer[1].Content
	if !strings.Contains(joined, "对象形态插话") || !strings.Contains(joined, "字符串形态插话") {
		t.Fatalf("插话内容不对：%q", joined)
	}
}

// ②e 前端链路（真 HTTP 打同一 mux、同一处理器）：POST JSON 对象 body 的
// interject 与建会话都正常——call_api 的修复不改变前端两条链路的行为。
func TestCallAPIFrontendHTTPChain(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	mkCallAPISession(t, a, "t-http", permWorkspace)
	a.runners["t-http"] = &sessionRun{} // 执行中：steer 生效，不拉起真执行
	defer delete(a.runners, "t-http")
	srv := httptest.NewServer(a.apiMuxOf())
	defer srv.Close()

	// interject：前端 fetch 同款（JSON.stringify 的对象 body）
	resp, err := http.Post(srv.URL+"/api/sessions/t-http/interject", "application/json",
		strings.NewReader(`{"content":"前端链路插话","mode":"steer"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), `"mode":"steer"`) {
		t.Fatalf("前端链路 interject 应 200 且 mode=steer：%d %s", resp.StatusCode, raw)
	}
	if st := a.pendingOf("t-http"); len(st.Steer) != 1 || st.Steer[0].Content != "前端链路插话" {
		t.Fatalf("前端链路插话应入队：%+v", st.Steer)
	}

	// 建会话：前端同一 handler 收对象 body
	resp, err = http.Post(srv.URL+"/api/sessions", "application/json",
		strings.NewReader(`{"title":"前端链路建的会话","mode":"task"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ = io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(raw), "前端链路建的会话") {
		t.Fatalf("前端链路建会话应 200 并回显标题：%d %s", resp.StatusCode, raw)
	}
}

// ③ 目录闸与权限闸：未开放接口拒绝；只读会话调写接口拒绝。
func TestCallAPIRejectsUnknownAndLowPerm(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ws := mkCallAPISession(t, a, "t-ws2", permWorkspace)
	// 未开放（对 AI）：远程外壳通道
	_, err := runCallAPI(ws, mustArgs(t, map[string]any{"path": "/api/p2p-shell"}))
	if err == nil || !strings.Contains(err.Error(), "不在可调目录") {
		t.Fatalf("未开放接口应拒绝并指路，got %v", err)
	}
	// 不存在
	_, err = runCallAPI(ws, mustArgs(t, map[string]any{"path": "/api/no-such-thing"}))
	if err == nil || !strings.Contains(err.Error(), "不在可调目录") {
		t.Fatalf("不存在接口应拒绝，got %v", err)
	}
	// 只读会话调写接口：权限闸
	ro := mkCallAPISession(t, a, "t-ro", permReadOnly)
	_, err = runCallAPI(ro, mustArgs(t, map[string]any{"path": "/api/sessions", "method": "POST",
		"params": map[string]any{"title": "不该建出来"}}))
	if err == nil || !strings.Contains(err.Error(), "修改工作区") {
		t.Fatalf("全电脑只读档调写接口应被权限闸拒绝（说明需要什么档），got %v", err)
	}
	if sessions, _ := a.Store.List(); len(sessions) != 3 { // main + t-ws2 + t-ro，被拒调用不添会话
		t.Fatalf("被拒的调用不该建出会话（现有 %d 个）", len(sessions))
	}
}

// ④ 危险接口审批：拒绝则取消；批准则真的执行（会话被删）。
func TestCallAPIDangerApproval(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ws := mkCallAPISession(t, a, "t-ws3", permWorkspace)
	victim := mkSession(t, a, "victim", "待删的会话", "task", "")
	_ = victim // 会话 id 固定为 victim，断言用字符串 id 即可；这里保住变量供阅读

	// 用户拒绝
	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := runCallAPI(ws, mustArgs(t, map[string]any{"path": "/api/sessions/victim", "method": "DELETE"}))
		ch <- result{out, err}
	}()
	resolveFirstApproval(t, a, false)
	r := <-ch
	if r.err == nil || !strings.Contains(r.err.Error(), "未批准") {
		t.Fatalf("拒绝审批后应取消，got %v / %s", r.err, r.out)
	}
	if _, err := a.Store.Get("victim"); err != nil {
		t.Fatal("被拒绝的删除不该执行")
	}

	// 用户批准
	go func() {
		out, err := runCallAPI(ws, mustArgs(t, map[string]any{"path": "/api/sessions/victim", "method": "DELETE"}))
		ch <- result{out, err}
	}()
	resolveFirstApproval(t, a, true)
	r = <-ch
	if r.err != nil || !strings.Contains(r.out, "HTTP 2") {
		t.Fatalf("批准后应执行删除，got %v / %s", r.err, r.out)
	}
	if _, err := a.Store.Get("victim"); err == nil {
		t.Fatal("批准后会话应真的被删除（与前端 DELETE 同一 handler）")
	}
}

// resolveFirstApproval 等第一张审批卡出现并作出决定（测试里替代用户点击）。
func resolveFirstApproval(t *testing.T, a *App, approved bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if aps := a.pendingApprovals(); len(aps) > 0 {
			if err := a.resolveApproval(aps[0].ID, approved, false); err != nil {
				t.Fatalf("审批决定失败: %v", err)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("审批卡没有出现（危险接口调用应触发 requestApproval）")
}

// ⑤ 工具注册与模式集（仅 assistant 注入）；工具说明携带完整目录；环境快照
// 不再渲染「可调接口」区块（目录已挪进工具定义）。
func TestCallAPIToolRegistrationAndEnv(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	if !toolNames(t, a, "assistant")["call_api"] {
		t.Fatal("数字助理应有 call_api")
	}
	// 其余模式一律不注入（范围收窄：界面级接口操作归总控编排）
	for _, mode := range []string{"task", "office", "coding", "beginner-coding", "skill-create", "aux"} {
		if toolNames(t, a, mode)["call_api"] {
			t.Fatalf("%s 模式不该有 call_api（只在 assistant 注入）", mode)
		}
	}
	// 工具说明携带完整目录（含档位标注与 ⚠，运行时按会话档位校验）
	desc := ""
	for _, tl := range a.toolsFor("assistant") {
		if tl.Spec.Function.Name == "call_api" {
			desc = tl.Spec.Function.Description
		}
	}
	for _, want := range []string{"GET /api/status [全电脑只读]", "POST /api/sessions [修改工作区]", "⚠ DELETE /api/sessions/{id}"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("call_api 工具说明应含完整目录（%s）：\n%s", want, desc)
		}
	}
	// 环境快照：所有模式都不再渲染接口目录区块
	for _, mode := range []string{"assistant", "task", "aux"} {
		env := a.envPrompt(&SessionMeta{Mode: mode, Permission: permWorkspace, Workspace: a.Cfg.Workspace})
		if strings.Contains(env, "可调接口") || strings.Contains(env, "GET /api/status") {
			t.Fatalf("%s 模式环境快照不该再含接口目录：\n%s", mode, env)
		}
	}
}
