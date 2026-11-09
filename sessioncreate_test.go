package main

// createSession / applyPermissionChange 的验收测试（service 单源）：
//  1. 默认值口径与原 handleCreateSession 一致（模式未知退 task、权限默认
//     workspace-write（创建技能/长期助理主对话 full）、Tools 缺席补默认、
//     完全访问捆绑能力）；
//  2. 分支克隆：字段照抄父会话、Origin=human、标题默认、计划/目标重置、
//     历史复制到子对话；
//  3. 三入口等价：HTTP 建会话与 dispatch_task 建会话走同一 service，
//     同参数落出同字段（派发多出 dispatched_by 与投递，这是入口语义）；
//  4. applyPermissionChange：升 full 捆绑能力、降回取消捆绑并补默认——
//     send_to_session 降权与人工 PATCH 同一套联动。
// runDispatchTask / runDispatchSub 的端到端行为由 dispatch_tools_test.go 与
// orchestration_test.go 既有用例回归（它们跑的就是收敛后的新路径）。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ① 默认值口径。
func TestCreateSessionDefaults(t *testing.T) {
	a, _ := newDispatchTestApp(t)

	// 模式未知退 task；权限默认 workspace-write；tools 缺席补默认能力
	m, err := a.createSession(createSessionParams{Mode: "nosuch", Title: "d1"})
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "task" || m.Permission != permWorkspace {
		t.Fatalf("模式未知应退 task、权限默认 workspace-write: %+v", m)
	}
	if len(m.Tools) == 0 {
		t.Fatal("tools 缺席应补默认能力（修改工作区默认开「操作浏览器」）")
	}
	// 创建技能模式默认完全访问 + 捆绑能力
	m2, err := a.createSession(createSessionParams{Mode: "skill-create"})
	if err != nil {
		t.Fatal(err)
	}
	if m2.Permission != permFull || len(m2.Tools) == 0 {
		t.Fatalf("创建技能应默认完全访问并捆绑能力: %+v", m2)
	}
	// 长期助理主对话默认完全访问 + 捆绑能力（新装机首启建的会话）
	m2b, err := a.createSession(createSessionParams{Mode: "assistant", Title: "数字助理主对话"})
	if err != nil {
		t.Fatal(err)
	}
	if m2b.Permission != permFull || len(m2b.Tools) == 0 {
		t.Fatalf("长期助理主对话应默认完全访问并捆绑能力: %+v", m2b)
	}
	// 显式传空 tools = 用户主动取消勾选：不补默认
	m3, err := a.createSession(createSessionParams{Mode: "task", Tools: []string{}})
	if err != nil {
		t.Fatal(err)
	}
	if len(m3.Tools) != 0 {
		t.Fatalf("显式空 tools 应尊重（不补默认）: %+v", m3.Tools)
	}
	// 目标模式创建即 active
	m4, err := a.createSession(createSessionParams{Mode: "task", GoalMode: true, GoalText: "g"})
	if err != nil {
		t.Fatal(err)
	}
	if m4.GoalStatus != "active" {
		t.Fatalf("创建即带目标模式应 active: %+v", m4)
	}
}

// ② 分支克隆（人工右栏建子对话）。
func TestCreateSessionCloneFromParent(t *testing.T) {
	a, main := newDispatchTestApp(t)
	main.Mode = "coding"
	main.Provider, main.Model = "zhipu", "glm-5.3"
	main.Thinking = "high"
	main.Tools = []string{capBrowser}
	if err := a.Store.SaveMeta(main); err != nil {
		t.Fatal(err)
	}
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("user", "父会话的第一条"), CreatedAt: now()})

	m, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if m.Mode != "coding" || m.Provider != "zhipu" || m.Model != "glm-5.3" || m.Thinking != "high" {
		t.Fatalf("克隆应照抄父会话字段: %+v", m)
	}
	if strings.Join(m.Tools, ",") != capBrowser {
		t.Fatalf("克隆应照抄能力开关（默认项合并幂等不重复）: %+v", m.Tools)
	}
	if m.Origin != "human" || m.Title != "子对话" || m.PlanMode || m.GoalMode {
		t.Fatalf("克隆应 Origin=human、标题默认、计划/目标重置: %+v", m)
	}
	rows, err := a.Store.RawMessagesAll(m.ID)
	if err != nil || len(rows) == 0 {
		t.Fatalf("克隆应复制父会话历史: %v %d", err, len(rows))
	}
	// 父会话的一级限制：克隆出的子对话不能再挂子对话
	if _, err := a.createSession(createSessionParams{ParentID: m.ID}); err == nil || !strings.Contains(err.Error(), "不能再建子对话") {
		t.Fatalf("子对话不能再建子对话应被拒: %v", err)
	}
}

// ③ 三入口等价：HTTP 与 dispatch_task 同参数落出同字段。
func TestCreateSessionHTTPDispatchParity(t *testing.T) {
	a, main := newDispatchTestApp(t)

	// HTTP 入口（前端建会话走的同一 handler）
	req, _ := http.NewRequestWithContext(context.Background(), http.MethodPost, "/api/sessions",
		strings.NewReader(`{"title":"HTTP 建的","mode":"office","permission":"read-only"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	a.apiMuxOf().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("HTTP 建会话应 200: %d %s", rec.Code, rec.Body.String())
	}
	var httpMade *SessionMeta
	for _, m := range otherSessions(t, a) {
		if m.Title == "HTTP 建的" {
			httpMade = m
		}
	}
	if httpMade == nil {
		t.Fatal("HTTP 建的会话没落库")
	}

	// dispatch_task 入口：同模式同权限
	ctx := dispatchCtx(a, main)
	if _, err := runDispatchTask(ctx, mustArgs(t, map[string]any{
		"task": "派发的任务", "title": "派发 建的", "mode": "office", "permission": "read-only",
	})); err != nil {
		t.Fatal(err)
	}
	var dispatched *SessionMeta
	for _, m := range otherSessions(t, a) {
		if m.Title == "派发 建的" {
			dispatched = m
		}
	}
	if dispatched == nil {
		t.Fatal("dispatch 建的会话没落库")
	}

	// 关键字段逐项一致（派发多出的是入口语义：dispatched_by 与首条任务消息）
	if dispatched.Mode != httpMade.Mode || dispatched.Permission != httpMade.Permission {
		t.Fatalf("两入口建出的会话模式/权限应一致: http=%+v dispatch=%+v", httpMade, dispatched)
	}
	if dispatched.DispatchedBy != main.ID {
		t.Fatalf("派发的会话应带 dispatched_by: %+v", dispatched)
	}
	if httpMade.DispatchedBy != "" {
		t.Fatalf("人工会话不该有 dispatched_by: %+v", httpMade)
	}
}

// ④ 权限档应用联动（人工 PATCH 与 send_to_session 共用）。
func TestApplyPermissionChange(t *testing.T) {
	m := &SessionMeta{Mode: "task", Permission: permWorkspace, Tools: []string{}}
	if applyPermissionChange(m, permFull, false) {
		// 升到完全访问：捆绑能力点亮
		if len(m.Tools) == 0 || m.Permission != permFull {
			t.Fatalf("升 full 应捆绑能力: %+v", m)
		}
	}
	fullTools := m.Tools
	if !applyPermissionChange(m, permWorkspace, false) {
		t.Fatalf("降回应改变能力开关")
	}
	if strings.Join(m.Tools, ",") == strings.Join(fullTools, ",") || len(m.Tools) == 0 {
		t.Fatalf("降回应取消捆绑并补默认能力: %+v -> %+v", fullTools, m.Tools)
	}
}

// ④b send_to_session 降权走同一联动（full 会话降到 ws，捆绑能力被取消）。
func TestSendToSessionDowngradeFullStripsCaps(t *testing.T) {
	a, main := newDispatchTestApp(t)
	full := mkSession(t, a, "t-full", "完全访问的任务", "task", "")
	full.Permission = permFull
	full.Tools = bundleFullAccessCaps(nil)
	if err := a.Store.SaveMeta(full); err != nil {
		t.Fatal(err)
	}
	ctx := dispatchCtx(a, main)
	out, err := runSendToSession(ctx, mustArgs(t, map[string]any{
		"session_id": "t-full", "task": "顺带把权限降到读写", "permission": "workspace-write",
	}))
	if err != nil {
		t.Fatalf("降权发送失败: %v\n%s", err, out)
	}
	m, err := a.Store.Get("t-full")
	if err != nil {
		t.Fatal(err)
	}
	if m.Permission != permWorkspace {
		t.Fatalf("权限应已降为 workspace-write: %+v", m)
	}
	if strings.Contains(strings.Join(m.Tools, ","), "cap-computer") || strings.Contains(strings.Join(m.Tools, ","), "cap-outside-exec") {
		t.Fatalf("降权后捆绑能力应被取消: %+v", m.Tools)
	}
	if len(m.Tools) == 0 {
		t.Fatalf("降权后应补回默认能力: %+v", m.Tools)
	}
}
