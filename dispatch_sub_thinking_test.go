package main

import "testing"

// 子对话不读取默认模型/档位，只继承派发者当前档位；未设置仍是空串。
func TestDispatchSubThinkingInheritance(t *testing.T) {
	for _, level := range []string{"", "xhigh"} {
		t.Run("level="+level, func(t *testing.T) {
			a, parent := newDispatchTestApp(t)
			parent.Thinking = level
			a.Cfg.DefaultModel.Thinking = "high"
			a.Cfg.DispatchModel = ModelRef{Thinking: "max"}
			if _, err := runDispatchSub(dispatchCtx(a, parent), mustArgs(t, map[string]any{"task": "test thinking inheritance"})); err != nil {
				t.Fatal(err)
			}
			subs := otherSessions(t, a)
			if len(subs) != 1 {
				t.Fatalf("sessions=%d", len(subs))
			}
			stored, err := a.Store.Get(subs[0].ID)
			stopThinkingTestSession(t, a, subs[0].ID)
			if err != nil {
				t.Fatal(err)
			}
			if stored.Thinking != level {
				t.Fatalf("thinking=%q want %q", stored.Thinking, level)
			}
		})
	}
}
