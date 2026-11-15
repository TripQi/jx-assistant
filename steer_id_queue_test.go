package main

import (
	"context"
	"encoding/json"
	"testing"
)

func TestSteerIDQueueFallback(t *testing.T) {
	a, _, task, _ := newMgrNotifyTestApp(t)
	pm := a.pushPending(task.ID, "steer", "same", kindChat, "interject")
	a.requeueSteer(task.ID)
	if len(a.pendingOf(task.ID).Queue) != 1 || a.pendingOf(task.ID).Queue[0].ID != pm.ID {
		t.Fatal("requeue lost stable ID")
	}
	var id string
	run := &sessionRun{ctx: context.Background()}
	err := a.drainQueue(task, run, func(ev Event) {
		if ev.Type == "message" && ev.Message != nil && ev.Message.Role == "user" {
			b, _ := json.Marshal(ev)
			var wire struct {
				PendingID string `json:"pendingId"`
			}
			_ = json.Unmarshal(b, &wire)
			id = wire.PendingID
		}
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	if id != pm.ID {
		t.Fatalf("queue landing ID=%q want=%q", id, pm.ID)
	}
	t.Log("steer -> requeue -> queue landing preserves ID")
}

func TestSteerIDAtomicFailure(t *testing.T) {
	a, _, task, _ := newMgrNotifyTestApp(t)
	// 确定性使 audit 写失败，验证不能留下有消息无映射的半次提交。
	_, err := a.Store.db.Exec(`CREATE TRIGGER reject_pending_audit BEFORE INSERT ON messages WHEN NEW.origin='pending_landed' BEGIN SELECT RAISE(ABORT,'test audit failure'); END`)
	if err != nil {
		t.Fatal(err)
	}
	called := false
	got := a.landUserSide(task, "same", kindChat, "interject", func(Event) { called = true }, "atomic-id")
	if got != nil || called {
		t.Fatal("failed transaction must not emit landed receipt")
	}
	msgs, err := a.Store.RawMessagesAll(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("partial transaction left %d rows", len(msgs))
	}
	t.Log("audit failure rolls back message and suppresses landed receipt")
}
