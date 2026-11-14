package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestManagerDecisionConcurrentHTTP(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, _ := newMgrNotifyTestApp(t)
			grantTestTurn(t, a, task.ID)
			q := &UserQuestion{ID: "concurrent-q", SessionID: task.ID, ch: make(chan string, 1)}
			ap := &Approval{ID: "concurrent-ap", SessionID: task.ID, ch: make(chan approvalResult, 1)}
			a.permMu.Lock()
			a.questions[q.ID] = q
			a.approvals[ap.ID] = ap
			a.permMu.Unlock()
			start := make(chan struct{})
			result := make(chan *httptest.ResponseRecorder, 2)
			for i := 0; i < 2; i++ {
				go func() {
					<-start
					id, path, body := q.ID, "/api/questions/", `{"answer":"经理代选"}`
					if kind == "approval" {
						id, path, body = ap.ID, "/api/approvals/", `{"approved":true}`
					}
					r := httptest.NewRequest(http.MethodPost, path+id, strings.NewReader(body))
					r.SetPathValue("id", id)
					r.Header.Set("X-JX-Caller-Session", mgr.ID)
					w := httptest.NewRecorder()
					if kind == "approval" {
						a.handleResolveApproval(w, r)
					} else {
						a.handleResolveQuestion(w, r)
					}
					result <- w
				}()
			}
			close(start)
			for i := 0; i < 2; i++ {
				select {
				case w := <-result:
					if w.Code != 200 {
						t.Fatalf("并发决定 HTTP %d %s", w.Code, w.Body.String())
					}
				case <-time.After(2 * time.Second):
					t.Fatal("并发经理代决死锁")
				}
			}
			a.permMu.Lock()
			n := len(q.ch)
			if kind == "approval" {
				n = len(ap.ch)
			}
			a.permMu.Unlock()
			if n != 1 {
				t.Fatalf("同一张卡只能提交一次，实际%d", n)
			}
			t.Logf("%s: 两个并发请求均200，决定通道仅一次投递", kind)
		})
	}
}
