package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 通过真实用户插话入口制造 deliverSub 已开轮、grant 尚未来到的窗口；
// 卡片使用隔离 App 的内存等待通道，不启动模型/定时通知或接触真实配置。
func TestManagerTurnLateGrantHTTP(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, mgr, task, _ := newMgrNotifyTestApp(t)
			run, ok := a.beginRun(task.ID)
			if !ok {
				t.Fatal("经理投递开轮失败")
			}
			t.Cleanup(func() { a.finishRunOf(task.ID, run) })

			// 用户在开轮与 grant 之间插话接管（真正的 HTTP handler 会 revoke）。
			r := httptest.NewRequest(http.MethodPost, "/api/sessions/"+task.ID+"/interject",
				strings.NewReader(`{"content":"这轮由我接管，请等我决定","mode":"steer"}`))
			r.SetPathValue("id", task.ID)
			w := httptest.NewRecorder()
			a.handleInterject(w, r)
			t.Logf("用户插话 HTTP %d %s", w.Code, strings.TrimSpace(w.Body.String()))
			if w.Code != http.StatusOK {
				t.Fatalf("用户插话失败: %d %s", w.Code, w.Body.String())
			}
			a.grantManagerTurn(task.ID, run) // 同一个旧 run 的迟到授权
			if a.managerTurnActive(task.ID) {
				t.Error("用户接管后迟到 grant 恢复了经理权限")
			}

			q := &UserQuestion{ID: "late-q", SessionID: task.ID, ch: make(chan string, 1)}
			ap := &Approval{ID: "late-ap", SessionID: task.ID, ch: make(chan approvalResult, 1)}
			a.permMu.Lock()
			a.questions[q.ID] = q
			a.approvals[ap.ID] = ap
			a.permMu.Unlock()
			post := func() *httptest.ResponseRecorder {
				id, path, body := q.ID, "/api/questions/", `{"answer":"经理代选"}`
				if kind == "approval" {
					id, path, body = ap.ID, "/api/approvals/", `{"approved":true}`
				}
				r := httptest.NewRequest(http.MethodPost, path+id, strings.NewReader(body))
				r.SetPathValue("id", id)
				r.Header.Set("X-JX-Caller-Session", mgr.ID)
				t.Logf("REQUEST POST %s%s X-JX-Caller-Session: %s body=%s", path, id, mgr.ID, body)
				w := httptest.NewRecorder()
				if kind == "approval" {
					a.handleResolveApproval(w, r)
				} else {
					a.handleResolveQuestion(w, r)
				}
				return w
			}
			w = post()
			t.Logf("撤销后晚 grant，经理 %s 代决 HTTP %d %s", kind, w.Code, strings.TrimSpace(w.Body.String()))
			if w.Code != http.StatusForbidden {
				t.Fatalf("应拒绝经理旧轮代决，实际 %d %s", w.Code, w.Body.String())
			}
			a.permMu.Lock()
			untouched := !q.decided && !q.byManager && len(q.ch) == 0 && !ap.decided && !ap.byManager && len(ap.ch) == 0
			a.permMu.Unlock()
			if !untouched {
				t.Fatal("403 之后卡片必须继续等待用户，不可标注经理来源或投递决定")
			}

			// 用户接管后的真正新经理轮应可授权；旧轮迟到 grant/收口不能覆盖它。
			a.finishRunOf(task.ID, run)
			next, ok := a.beginRun(task.ID)
			if !ok {
				t.Fatal("新经理轮开轮失败")
			}
			t.Cleanup(func() { a.finishRunOf(task.ID, next) })
			a.grantManagerTurn(task.ID, next)
			a.grantManagerTurn(task.ID, run)
			a.dropManagerTurnOf(task.ID, run)
			a.grantManagerTurn(task.ID, nil)
			if !a.managerTurnActive(task.ID) {
				t.Fatal("旧轮 grant/收口或 nil 插话不应误伤新经理轮")
			}
			// 新一轮使用新挂出的卡，旧轮的卡仍未被经理处理。
			q = &UserQuestion{ID: "next-q", SessionID: task.ID, ch: make(chan string, 1)}
			ap = &Approval{ID: "next-ap", SessionID: task.ID, ch: make(chan approvalResult, 1)}
			a.permMu.Lock()
			delete(a.questions, "late-q")
			delete(a.approvals, "late-ap")
			a.questions[q.ID] = q
			a.approvals[ap.ID] = ap
			a.permMu.Unlock()
			w = post()
			t.Logf("新经理轮 %s 代决 HTTP %d %s", kind, w.Code, strings.TrimSpace(w.Body.String()))
			if w.Code != http.StatusOK {
				t.Fatalf("新经理轮应可代决，实际 %d %s", w.Code, w.Body.String())
			}
			a.permMu.Lock()
			resolved := q.decided && q.byManager && len(q.ch) == 1
			if kind == "approval" {
				resolved = ap.decided && ap.byManager && len(ap.ch) == 1
			}
			a.permMu.Unlock()
			if !resolved {
				t.Fatal("新经理轮应实际登记并投递决定，且标记经理来源")
			}
		})
	}
}
