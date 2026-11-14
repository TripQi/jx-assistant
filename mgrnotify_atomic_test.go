package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// permMu 是真实卡片提交门：经理已发起请求但未进入提交时，用户完成撤销，
// 随后放开门也不得代决。两种卡片都走真实 HTTP handler。
func TestManagerDecisionRevokeBeforeCommitHTTP(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, _ := newMgrNotifyTestApp(t)
			grantTestTurn(t, a, task.ID)
			q := &UserQuestion{ID: "atomic-q", SessionID: task.ID, ch: make(chan string, 1)}
			ap := &Approval{ID: "atomic-ap", SessionID: task.ID, ch: make(chan approvalResult, 1)}
			a.permMu.Lock()
			a.questions[q.ID], a.approvals[ap.ID] = q, ap
			id, path, body := q.ID, "/api/questions/", `{"answer":"经理代选"}`
			if kind == "approval" {
				id, path, body = ap.ID, "/api/approvals/", `{"approved":true}`
			}
			post := func() *httptest.ResponseRecorder {
				r := httptest.NewRequest(http.MethodPost, path+id, strings.NewReader(body))
				r.SetPathValue("id", id)
				r.Header.Set("X-JX-Caller-Session", mgr.ID)
				w := httptest.NewRecorder()
				if kind == "approval" {
					a.handleResolveApproval(w, r)
				} else {
					a.handleResolveQuestion(w, r)
				}
				return w
			}
			started := make(chan struct{})
			result := make(chan *httptest.ResponseRecorder, 1)
			go func() { close(started); result <- post() }()
			<-started
			// 撤销只需 runMu，不应等卡片 permMu，也不做 IO。
			a.revokeManagerTurn(task.ID)
			a.permMu.Unlock()
			select {
			case w := <-result:
				t.Logf("POST %s%s body=%s -> %d %s", path, id, body, w.Code, strings.TrimSpace(w.Body.String()))
				if w.Code != http.StatusForbidden {
					t.Fatalf("撤销先完成必须403: %d %s", w.Code, w.Body.String())
				}
			case <-time.After(2 * time.Second):
				t.Fatal("经理决定与撤销发生死锁")
			}
			a.permMu.Lock()
			untouched := !q.decided && !q.byManager && len(q.ch) == 0 && !ap.decided && !ap.byManager && len(ap.ch) == 0
			a.permMu.Unlock()
			if !untouched {
				t.Fatal("拒绝后不能提交决定或来源")
			}
			// 新授权下决定先完成，之后撤销不能追溯已提交决定。
			a.finishRun(task.ID)
			grantTestTurn(t, a, task.ID)
			w := post()
			if w.Code != http.StatusOK {
				t.Fatalf("有效授权决定: %d %s", w.Code, w.Body.String())
			}
			a.revokeManagerTurn(task.ID)
			a.permMu.Lock()
			decided := q.decided && q.byManager && len(q.ch) == 1
			if kind == "approval" {
				decided = ap.decided && ap.byManager && len(ap.ch) == 1
			}
			a.permMu.Unlock()
			if !decided || a.managerTurnActive(task.ID) {
				t.Fatal("决定先完成应保留决定，撤销仅关闭后续授权")
			}
			t.Logf("决定先完成 HTTP %d %s；随后撤销不追溯", w.Code, strings.TrimSpace(w.Body.String()))
		})
	}
}

// 在真正的原子提交入口暂停，证明授权校验到提交之间仍持有 runMu；
// 不往产品放测试 hook，使用入口现有 commit 回调和锁控制确定顺序。
func TestManagerDecisionCommitHoldsRevocationBoundary(t *testing.T) {
	a, _, task, _ := newMgrNotifyTestApp(t)
	grantTestTurn(t, a, task.ID)
	entered, release, committed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		a.permMu.Lock()
		err := a.commitManagerDecisionLocked(task.ID, func() {
			close(entered)
			<-release
		})
		a.permMu.Unlock()
		committed <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("提交未进入")
	}
	if a.runMu.TryLock() {
		a.runMu.Unlock()
		close(release)
		t.Fatal("授权已通过但提交期间没有保持撤销锁")
	}
	revokeStarted, revoked := make(chan struct{}), make(chan struct{})
	go func() { close(revokeStarted); a.revokeManagerTurn(task.ID); close(revoked) }()
	<-revokeStarted
	select {
	case <-revoked:
		close(release)
		t.Fatal("撤销不应越过正在提交的决定")
	default:
	}
	close(release)
	select {
	case err := <-committed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("提交死锁")
	}
	select {
	case <-revoked:
	case <-time.After(2 * time.Second):
		t.Fatal("撤销未完成")
	}
	if a.managerTurnActive(task.ID) {
		t.Fatal("决定提交后的撤销应正常关闭授权")
	}
}
