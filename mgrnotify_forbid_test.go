package main

// 「经理只能代批自己那一轮」的决议入口守卫（mgrnotify.go 的 cardSession /
// managerResolveAllowed + server.go 两处 403）：
//   · 越权路：经理经 call_api 主动翻清单，去批一张不属于自己那一轮（没人投递过、
//     或其会话的经理轮已收口/被用户接管）的卡 → 403，卡片原地等待用户；
//   · 正当路：同一时刻经理仍有自己那一轮的授权，批自己轮内挂出的卡 → 200；
//   · 判据层：cardSession / managerResolveAllowed 本身的口径（未知卡片、卡已收走、
//     审批与提问不串门、会话 id 为空）。
// 通知侧「窗口外不投」的既有口径见 mgrnotify_test.go；撤销/TOCTOU 见
// mgrnotify_revoke_test.go / mgrnotify_atomic_test.go（本文件不碰它们的断言）。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postManagerDecision 以「经理经 call_api 代决」的身份打真实 HTTP handler
// （runCallAPI 注入的 X-JX-Caller-Session 就是经理会话 id）。
func postManagerDecision(t *testing.T, a *App, mgrID, kind, id string) *httptest.ResponseRecorder {
	t.Helper()
	path, body := "/api/questions/", `{"answer":"经理代选"}`
	if kind == "approval" {
		path, body = "/api/approvals/", `{"approved":true}`
	}
	r := httptest.NewRequest(http.MethodPost, path+id, strings.NewReader(body))
	r.SetPathValue("id", id)
	r.Header.Set("X-JX-Caller-Session", mgrID)
	w := httptest.NewRecorder()
	if kind == "approval" {
		a.handleResolveApproval(w, r)
	} else {
		a.handleResolveQuestion(w, r)
	}
	t.Logf("POST %s%s（经理 %s 代决）-> %d %s", path, id, mgrID, w.Code, strings.TrimSpace(w.Body.String()))
	return w
}

// addCards 往等待清单里挂一张审批卡与一张提问卡（都属 sid）。
func addCards(a *App, sid, id string) (*Approval, *UserQuestion) {
	ap := &Approval{ID: id + "-ap", SessionID: sid, ch: make(chan approvalResult, 1)}
	q := &UserQuestion{ID: id + "-q", SessionID: sid, ch: make(chan string, 1)}
	a.permMu.Lock()
	a.approvals[ap.ID], a.questions[q.ID] = ap, q
	a.permMu.Unlock()
	return ap, q
}

func cardsUntouched(a *App, ap *Approval, q *UserQuestion) bool {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	return !ap.decided && !ap.byManager && len(ap.ch) == 0 && !q.decided && !q.byManager && len(q.ch) == 0
}

// 越权：不属于自己那一轮的卡，代批/代答一律 403；同一 App 里经理另有自己那一轮
// 的授权也不影响这条判定（授权是按卡片所属会话与轮次算的，不是「经理有授权」）。
func TestManagerDecisionForeignCardForbiddenHTTP(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, solo := newMgrNotifyTestApp(t)
			grantTestTurn(t, a, task.ID) // 经理自己那一轮（另一个会话）正在执行

			// ① 卡片属于经理从未投递过的会话（solo）：窗口外，必须拒绝。
			ap, q := addCards(a, solo.ID, "foreign")
			id := q.ID
			if kind == "approval" {
				id = ap.ID
			}
			w := postManagerDecision(t, a, mgr.ID, kind, id)
			if w.Code != http.StatusForbidden {
				t.Fatalf("经理代决不属于自己轮次的卡必须 403，实际 %d %s", w.Code, w.Body.String())
			}
			if strings.TrimSpace(w.Body.String()) == "" || !strings.Contains(w.Body.String(), "轮") {
				t.Errorf("403 正文应说明轮次级授权，实际 %q", w.Body.String())
			}
			if !cardsUntouched(a, ap, q) {
				t.Fatal("403 之后卡片必须继续等待用户：不可标注经理来源、不可投递决定")
			}
			if a.managerResolveAllowed(kind == "approval", id) {
				t.Fatal("窗口外的卡不应被判为可代决")
			}

			// ② 卡片所属会话的经理轮确实存在过，但已被用户接管（撤销）：
			// 同一张卡在授权在手时放行、撤销后拒绝——越权闸跟着轮次走。
			grantTestTurn(t, a, solo.ID)
			ap2, q2 := addCards(a, solo.ID, "owned")
			id2 := q2.ID
			if kind == "approval" {
				id2 = ap2.ID
			}
			if w := postManagerDecision(t, a, mgr.ID, kind, id2); w.Code != http.StatusOK {
				t.Fatalf("经理代决自己那一轮的卡应 200，实际 %d %s", w.Code, w.Body.String())
			}
			a.permMu.Lock()
			ok := q2.decided && q2.byManager && len(q2.ch) == 1
			if kind == "approval" {
				ok = ap2.decided && ap2.byManager && len(ap2.ch) == 1
			}
			a.permMu.Unlock()
			if !ok {
				t.Fatal("自己那一轮应实际登记决定并标注经理来源")
			}

			a.revokeManagerTurn(solo.ID) // 用户接管
			ap3, q3 := addCards(a, solo.ID, "after-revoke")
			id3 := q3.ID
			if kind == "approval" {
				id3 = ap3.ID
			}
			if w := postManagerDecision(t, a, mgr.ID, kind, id3); w.Code != http.StatusForbidden {
				t.Fatalf("用户接管后经理代决必须 403，实际 %d %s", w.Code, w.Body.String())
			}
			if !cardsUntouched(a, ap3, q3) {
				t.Fatal("接管后 403 的卡不能被代决")
			}
		})
	}
}

// 判据层：cardSession 取卡所属会话，managerResolveAllowed 只认「所属会话的经理轮
// 仍在执行」。边界都要钉住——空串会话、未知卡片、已收走的卡、两类卡不串门。
func TestManagerResolveAllowedPredicate(t *testing.T) {
	a, _, task, solo := newMgrNotifyTestApp(t)
	grantTestTurn(t, a, task.ID)

	ap, q := addCards(a, task.ID, "pred")
	addCards(a, solo.ID, "solo")
	empty := &Approval{ID: "empty-ap", SessionID: "", ch: make(chan approvalResult, 1)}
	a.permMu.Lock()
	a.approvals[empty.ID] = empty
	a.permMu.Unlock()

	cases := []struct {
		name       string
		isApproval bool
		id         string
		wantSid    string
		wantAllow  bool
	}{
		{"经理轮内的审批卡", true, ap.ID, task.ID, true},
		{"经理轮内的提问卡", false, q.ID, task.ID, true},
		{"没人投递过的会话的卡", false, "solo-q", solo.ID, false},
		{"会话 id 为空", true, empty.ID, "", false},
		{"未知卡片 id", true, "nope", "", false},
		{"未知卡片 id（提问）", false, "nope", "", false},
		{"审批 id 当提问查：不串门", false, ap.ID, "", false},
		{"提问 id 当审批查：不串门", true, q.ID, "", false},
	}
	for _, c := range cases {
		if got := a.cardSession(c.isApproval, c.id); got != c.wantSid {
			t.Errorf("%s：cardSession=%q 期望=%q", c.name, got, c.wantSid)
		}
		if got := a.managerResolveAllowed(c.isApproval, c.id); got != c.wantAllow {
			t.Errorf("%s：managerResolveAllowed=%v 期望=%v", c.name, got, c.wantAllow)
		}
	}

	// 轮收口即失效：同一张卡在 task 轮开着的期间放行，收口后不放行。
	if !a.managerResolveAllowed(true, ap.ID) {
		t.Fatal("轮内应放行")
	}
	a.finishRun(task.ID)
	if a.managerResolveAllowed(true, ap.ID) {
		t.Fatal("经理轮收口后不应再放行（授权按轮次算）")
	}
	// 卡片被系统收走（已不在等待清单）也不算可代决。
	a.permMu.Lock()
	delete(a.approvals, ap.ID)
	a.permMu.Unlock()
	if a.managerResolveAllowed(true, ap.ID) || a.cardSession(true, ap.ID) != "" {
		t.Fatal("已不在等待清单的卡不应被判为可代决")
	}
}
