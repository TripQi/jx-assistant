package main

// 「决议请求的调用者身份」越权口子的验收（server.go 两处决议入口 + mgrnotify.go
// 的身份判定）。缺陷事实：AI 会话调 call_api 时 runCallAPI 统一带
// X-JX-Caller-Session=<自己的会话 id>（子对话带的就是子会话 id），而修复前的
// 判定只认与 currentAssistant 逐字相等的调用者——经理派发的子对话因此被当成
// 「用户」走普通决议路径，跳过全部轮次级授权，可代决任意会话的卡。
//
// 本文件的用例全部走真实 HTTP handler（与界面按钮 / call_api 直调同一条路径），
// 场景：
//   · 子对话代决与自己无关的卡 → 403，卡片原地等用户（红证：修复前 200）；
//   · 子对话代决经理当前轮次的卡 → 与经理本级同一套轮次级授权：轮内 200，
//     用户接管 / 轮收口后 403；
//   · 真用户（不带调用者头）决定任意卡 → 照常 200（最关键的回归，不能被误伤）；
//   · 认不出身份的 AI 会话（普通工作任务会话、父会话已删除的孤儿子对话、普通
//     任务派生的子对话、aux）→ 403，哪怕那张卡正是经理轮内的卡；
//   · 审批与提问两条路都测。
// 修复前跑本文件即红（越权 200），修复后全绿；经理自身「外卡 403 / 自己那轮
// 200」的既有断言在 mgrnotify_forbid_test.go，不放宽、不重复。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// addChildSession 建一个挂在 parent 下的会话（dispatch_sub 建子对话时 ParentID
// 就是派发者；这里直接用 Store.Create 落盘，不经过建会话的参数校验）。
func addChildSession(t *testing.T, a *App, id, title, mode, parent string) *SessionMeta {
	t.Helper()
	m := &SessionMeta{ID: id, Title: title, Mode: mode, Workspace: a.Cfg.Workspace,
		Permission: permWorkspace, ParentID: parent, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话 %s（parent=%q）失败: %v", id, parent, err)
	}
	return m
}

// postDecisionAs 打真实决议入口：callerID 空串＝真用户（界面按钮与远程转发都
// 不带 X-JX-Caller-Session），非空＝该 SessionMeta.ID 对应的 AI 会话经 call_api
// 代决（runCallAPI 注入的就是它自己的会话 id）。
func postDecisionAs(t *testing.T, a *App, callerID, kind, id string) *httptest.ResponseRecorder {
	t.Helper()
	path, body := "/api/questions/"+id, `{"answer":"代选"}`
	if kind == "approval" {
		path, body = "/api/approvals/"+id, `{"approved":true}`
	}
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.SetPathValue("id", id)
	if callerID != "" {
		r.Header.Set("X-JX-Caller-Session", callerID)
	}
	w := httptest.NewRecorder()
	if kind == "approval" {
		a.handleResolveApproval(w, r)
	} else {
		a.handleResolveQuestion(w, r)
	}
	who := "真用户（无调用者头）"
	if callerID != "" {
		who = "调用者 " + callerID
	}
	t.Logf("POST %s body=%s 调用者=%s -> HTTP %d %s", path, body, who, w.Code, strings.TrimSpace(w.Body.String()))
	return w
}

// cardIDOf 取本 kind 要打的那张卡；decidedOf 判断该卡是否真的落了决定。
func cardIDOf(kind string, ap *Approval, q *UserQuestion) string {
	if kind == "approval" {
		return ap.ID
	}
	return q.ID
}

func decidedOf(a *App, kind string, ap *Approval, q *UserQuestion) (decided, byManager bool, delivered int) {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	if kind == "approval" {
		return ap.decided, ap.byManager, len(ap.ch)
	}
	return q.decided, q.byManager, len(q.ch)
}

// 红证场景（修复前 200＝越权成功）：经理派发的子对话（子会话 parent 指向主对话）
// 带自己的会话 id，去代决一张属于「经理从未投递过的另一个会话」的卡。
// 修复后必须 403，卡片继续等用户——经理自己那一轮同时在跑也不影响这条判定
//（授权按卡片所属会话的轮次算，不是「经理有授权就全放行」）。
func TestSubDialogDecisionForeignCardDenied(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, solo := newMgrNotifyTestApp(t)
			sub := addChildSession(t, a, "sub-1", "子对话", "coding", mgr.ID)
			grantTestTurn(t, a, task.ID) // 经理自己那一轮正在执行（与卡片所属会话无关）
			ap, q := addCards(a, solo.ID, "foreign")
			id := cardIDOf(kind, ap, q)
			w := postDecisionAs(t, a, sub.ID, kind, id)
			if w.Code != http.StatusForbidden {
				t.Fatalf("子对话代决与自己无关的卡必须 403，实际 %d %s", w.Code, w.Body.String())
			}
			if strings.TrimSpace(w.Body.String()) == "" {
				t.Error("403 正文应给出可读原因")
			}
			if !cardsUntouched(a, ap, q) {
				t.Fatal("403 之后卡片必须继续等用户：不可标注代决来源、不可投递决定")
			}
			if decided, byManager, n := decidedOf(a, kind, ap, q); decided || byManager || n != 0 {
				t.Fatalf("403 之后卡片状态必须不变，实际 decided=%v byManager=%v ch=%d", decided, byManager, n)
			}
		})
	}
}

// 子对话代决「经理当前轮次」的卡：规则与经理本级完全一致——同一套轮次级授权
//（managerResolveAllowed 预检 + commitManagerDecisionLocked 原子提交），所以
// 轮内 200 并标注代决来源；用户接管（撤销）后同一调用者转为 403；轮收口后同样 403。
func TestSubDialogDecisionManagerTurnCard(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, _ := newMgrNotifyTestApp(t)
			sub := addChildSession(t, a, "sub-1", "子对话", "coding", mgr.ID)
			grantTestTurn(t, a, task.ID)

			ap, q := addCards(a, task.ID, "mgr-turn")
			w := postDecisionAs(t, a, sub.ID, kind, cardIDOf(kind, ap, q))
			if w.Code != http.StatusOK {
				t.Fatalf("子对话代决经理当前轮次的卡应放行（与经理本级同一套授权），实际 %d %s", w.Code, w.Body.String())
			}
			decided, byManager, n := decidedOf(a, kind, ap, q)
			if !decided || !byManager || n != 1 {
				t.Fatalf("放行必须真的落决定并标注代决来源，实际 decided=%v byManager=%v ch=%d", decided, byManager, n)
			}

			// 用户亲发消息＝接管：授权撤销，同一调用者立刻转为 403
			a.revokeManagerTurn(task.ID)
			ap2, q2 := addCards(a, task.ID, "after-revoke")
			if w := postDecisionAs(t, a, sub.ID, kind, cardIDOf(kind, ap2, q2)); w.Code != http.StatusForbidden {
				t.Fatalf("用户接管后子对话代决必须 403，实际 %d %s", w.Code, w.Body.String())
			}
			if !cardsUntouched(a, ap2, q2) {
				t.Fatal("接管后 403 的卡不能被代决")
			}

			// 轮收口（正常完成/失败/停止）：授权随轮失效，子对话同样 403
			a.finishRun(task.ID) // 先收口上一轮，才能开下一轮
			grantTestTurn(t, a, task.ID)
			ap3, q3 := addCards(a, task.ID, "after-finish")
			a.finishRun(task.ID)
			if w := postDecisionAs(t, a, sub.ID, kind, cardIDOf(kind, ap3, q3)); w.Code != http.StatusForbidden {
				t.Fatalf("轮收口后子对话代决必须 403，实际 %d %s", w.Code, w.Body.String())
			}
			if !cardsUntouched(a, ap3, q3) {
				t.Fatal("轮收口后 403 的卡不能被代决")
			}
		})
	}
}

// 真用户（不带 X-JX-Caller-Session，界面按钮与远程转发都是这一路）决定任意卡
// 照常生效——最关键的回归：新规则只能管 AI 调用方，绝不能误伤用户。
func TestUserDecisionAnyCardStillWorks(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, _, task, solo := newMgrNotifyTestApp(t)

			// ① 卡片属于用户自建会话、没有任何经理轮授权：用户照常决定
			ap, q := addCards(a, solo.ID, "user-decides")
			if w := postDecisionAs(t, a, "", kind, cardIDOf(kind, ap, q)); w.Code != http.StatusOK {
				t.Fatalf("用户决定任意卡应照常成功，实际 %d %s", w.Code, w.Body.String())
			}
			decided, byManager, n := decidedOf(a, kind, ap, q)
			if !decided || byManager || n != 1 {
				t.Fatalf("用户决定必须生效且不标代理来源，实际 decided=%v byManager=%v ch=%d", decided, byManager, n)
			}

			// ② 卡片属于经理当前轮次：用户仍然优先（用户永远有权决定任何卡）
			grantTestTurn(t, a, task.ID)
			ap2, q2 := addCards(a, task.ID, "user-in-mgr-turn")
			if w := postDecisionAs(t, a, "", kind, cardIDOf(kind, ap2, q2)); w.Code != http.StatusOK {
				t.Fatalf("用户决定经理轮内的卡同样应成功，实际 %d %s", w.Code, w.Body.String())
			}
			if decided, byManager, n := decidedOf(a, kind, ap2, q2); !decided || byManager || n != 1 {
				t.Fatalf("用户决定必须生效且不标代理来源，实际 decided=%v byManager=%v ch=%d", decided, byManager, n)
			}
		})
	}
}

// 认不出身份的 AI 会话一律 403：既不是主对话、也不在它派生的 parent 链上。
// 用「经理轮内、经理自己代决能过的那张卡」做靶子，证明 403 只因为调用者身份
// 认不出——而不是卡片本身不可代决。
func TestUnidentifiedAiCallerDenied(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, _ := newMgrNotifyTestApp(t)
			addChildSession(t, a, "sub-orphan", "父会话已删除的子对话", "coding", "ghost-parent")
			addChildSession(t, a, "sub-of-task", "普通任务派生的子对话", "coding", task.ID)
			aux := addChildSession(t, a, "aux-1", "辅助答疑", "aux", "")
			grantTestTurn(t, a, task.ID)

			ap, q := addCards(a, task.ID, "mgr-turn")
			id := cardIDOf(kind, ap, q)
			callers := []struct{ name, id string }{
				{"普通工作任务会话（经理 dispatch_task 派生，不在 parent 链上）", task.ID},
				{"父会话已删除的孤儿子对话", "sub-orphan"},
				{"普通任务会话派生的子对话（链没到主对话）", "sub-of-task"},
				{"aux 辅助答疑会话", aux.ID},
				{"根本不存在的会话", "no-such-session"},
			}
			for _, c := range callers {
				w := postDecisionAs(t, a, c.id, kind, id)
				if w.Code != http.StatusForbidden {
					t.Fatalf("%s 代决必须 403，实际 %d %s", c.name, w.Code, w.Body.String())
				}
				if !cardsUntouched(a, ap, q) {
					t.Fatalf("%s 被拒后卡片必须仍在等待", c.name)
				}
			}

			// 对照：同一张卡由经理自己代决 → 200（既有语义不变）
			if w := postDecisionAs(t, a, mgr.ID, kind, id); w.Code != http.StatusOK {
				t.Fatalf("经理代决自己那一轮的卡应放行，实际 %d %s", w.Code, w.Body.String())
			}
			if decided, byManager, n := decidedOf(a, kind, ap, q); !decided || !byManager || n != 1 {
				t.Fatalf("经理放行必须落决定并标来源，实际 decided=%v byManager=%v ch=%d", decided, byManager, n)
			}
		})
	}
}
