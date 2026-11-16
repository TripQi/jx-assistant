package main

// 「子对话身份识别」的判据层验收（mgrnotify.go 的 resolveCallerOf /
// managerLineage）：parent 链只有落到当前主对话才算「经理派生」；链断、链停在
// 别的顶层会话、脏数据成环，一律认不出（决议入口 403），不得回落成用户语义。

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagerLineagePredicate(t *testing.T) {
	a, mgr, task, solo := newMgrNotifyTestApp(t)
	addChildSession(t, a, "sub-1", "经理派发的子对话", "coding", mgr.ID)
	addChildSession(t, a, "sub-orphan", "父会话已删除的子对话", "coding", "ghost-parent")
	addChildSession(t, a, "sub-of-task", "普通任务派生的子对话", "coding", task.ID)
	aux := addChildSession(t, a, "aux-1", "辅助答疑", "aux", "")
	// 脏数据成环（正常建会话只有一级，走不到）：必须终止且不认。
	cyc1 := addChildSession(t, a, "cycle-1", "环1", "coding", "")
	addChildSession(t, a, "cycle-2", "环2", "coding", "cycle-1")
	cyc1.ParentID = "cycle-2"
	if err := a.Store.SaveMeta(cyc1); err != nil {
		t.Fatalf("造环失败: %v", err)
	}

	cases := []struct {
		name string
		sid  string
		want resolveCallerKind
	}{
		{"经理主对话自己", mgr.ID, resolveCallerManager},
		{"经理派发的子对话（parent=主对话）", "sub-1", resolveCallerManager},
		{"经理 dispatch_task 派出的普通任务会话（无 parent）", task.ID, resolveCallerForeign},
		{"用户自建会话", solo.ID, resolveCallerForeign},
		{"父会话已删除的孤儿子对话", "sub-orphan", resolveCallerForeign},
		{"普通任务会话派生的子对话（链没到主对话）", "sub-of-task", resolveCallerForeign},
		{"aux 辅助答疑会话", aux.ID, resolveCallerForeign},
		{"根本不存在的会话 id", "no-such-session", resolveCallerForeign},
	}
	for _, c := range cases {
		r := httptest.NewRequest(http.MethodPost, "/api/questions/x", nil)
		r.Header.Set("X-JX-Caller-Session", c.sid)
		if got := a.resolveCallerOf(r); got != c.want {
			t.Errorf("%s：resolveCallerOf(%q)=%v 期望 %v", c.name, c.sid, got, c.want)
		}
	}
	if !a.managerLineage(mgr.ID) || !a.managerLineage("sub-1") {
		t.Error("主对话自己与它派发的子对话都应在经理派生链上")
	}
	if a.managerLineage("sub-of-task") || a.managerLineage("sub-orphan") || a.managerLineage("cycle-1") {
		t.Error("链没落到主对话 / 链断 / 成环都不能算经理派生")
	}

	// 无头才是用户；空白的头也按用户（前端不发的形态），带任何非空头都不得回落成用户。
	r := httptest.NewRequest(http.MethodPost, "/api/questions/x", nil)
	if got := a.resolveCallerOf(r); got != resolveCallerUser {
		t.Errorf("不带调用者头应是用户，实际 %v", got)
	}
	r.Header.Set("X-JX-Caller-Session", "  ")
	if got := a.resolveCallerOf(r); got != resolveCallerUser {
		t.Errorf("空白调用者头等同不带（前端不发），应是用户，实际 %v", got)
	}
	// 主对话不存在时（没建过任何版本）任何带头调用都认不出——主对话是唯一锚点。
	empty := newCompactTestApp(t)
	empty.initRuntime()
	r.Header.Set("X-JX-Caller-Session", mgr.ID)
	if got := empty.resolveCallerOf(r); got != resolveCallerForeign {
		t.Errorf("没有主对话时带头调用必须判为 foreign，实际 %v", got)
	}
	if empty.managerLineage(mgr.ID) {
		t.Error("没有主对话时谁都不在经理派生链上")
	}
}
