package main

import (
	"testing"
	"time"
)

func stopThinkingTestSession(t *testing.T, a *App, id string) {
	t.Helper()
	a.stopRun(id)
	deadline := time.Now().Add(3 * time.Second)
	for a.IsRunning(id) && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if a.IsRunning(id) {
		t.Errorf("test session %s did not stop", id)
	}
}

func TestDispatchTaskDefaultThinkingStored(t *testing.T) {
	for _, tc := range []struct{ global, dispatch, want string }{{"", "", ""}, {"high", "", "high"}, {"high", "xhigh", "xhigh"}, {"", "none", "none"}} {
		t.Run(tc.global+"/"+tc.dispatch, func(t *testing.T) {
			a, parent := newDispatchTestApp(t)
			a.Cfg.DefaultModel.Thinking = tc.global
			a.Cfg.DispatchModel.Thinking = tc.dispatch
			if _, err := runDispatchTask(dispatchCtx(a, parent), mustArgs(t, map[string]any{"task": "test default thinking"})); err != nil {
				t.Fatal(err)
			}
			subs := otherSessions(t, a)
			if len(subs) != 1 {
				t.Fatalf("sessions=%d", len(subs))
			}
			got, err := a.Store.Get(subs[0].ID)
			stopThinkingTestSession(t, a, subs[0].ID)
			if err != nil || got.Thinking != tc.want {
				t.Fatalf("thinking=%+v err=%v want=%q", got, err, tc.want)
			}
		})
	}
}

func TestTodoDispatchDefaultThinkingStored(t *testing.T) {
	for _, mode := range []string{"dispatch", "global", "custom"} {
		for _, tc := range []struct{ own, dispatch, global, want string }{{"", "", "", ""}, {"", "", "high", "high"}, {"", "medium", "high", "medium"}, {"xhigh", "medium", "high", "xhigh"}, {"none", "max", "high", "none"}} {
			t.Run(mode+"/"+tc.own+"/"+tc.dispatch+"/"+tc.global, func(t *testing.T) {
				a, s, _ := newTodoDispatchApp(t)
				a.Cfg.DefaultModel.Thinking = tc.global
				a.Cfg.DispatchModel.Thinking = tc.dispatch
				req := todoRequest{Title: "test", Description: "test default thinking", Executor: execAssistant, Target: targetTask, Schedule: &Schedule{Kind: schedManual}, ModelMode: mode, Thinking: &tc.own}
				if mode == "custom" {
					r := a.Cfg.DefaultModel
					r.Thinking = ""
					req.Model = &r
				}
				v, err := s.create(req)
				if err != nil {
					t.Fatal(err)
				}
				sid, err := s.dispatch(findTodoTask(t, s, v.ID), "manual", time.Now().UnixMilli())
				if err != nil {
					t.Fatal(err)
				}
				got, err := a.Store.Get(sid)
				stopThinkingTestSession(t, a, sid)
				if err != nil || got.Thinking != tc.want {
					t.Fatalf("thinking=%+v err=%v want=%q", got, err, tc.want)
				}
			})
		}
	}
}
