package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"
)

// 走真实 drainSteer -> 落库 -> SSE -> GET，断言不依赖新增 Go 字段，便于与基线对照。
func TestSteerIDPersistence(t *testing.T) {
	a, mgr, task, _ := newMgrNotifyTestApp(t)
	a.landUserSide(task, "same", kindChat, "", func(Event) {}) // 同正文历史不建立关联
	p1 := a.pushPending(task.ID, "steer", "same", kindChat, "interject")
	p2 := a.pushPending(task.ID, "steer", "same", kindChat, "interject")
	other := a.pushPending(mgr.ID, "steer", "same", kindChat, "interject")
	linked := map[string]int64{}
	if err := a.RunAgent(context.Background(), task, func(ev Event) {
		if ev.Type != "message" || ev.Message == nil || ev.Message.Role != "user" {
			return
		}
		b, _ := json.Marshal(ev)
		var wire struct {
			PendingID string `json:"pendingId"`
			Seq       int64  `json:"seq"`
		}
		_ = json.Unmarshal(b, &wire)
		if wire.PendingID != "" {
			linked[wire.PendingID] = wire.Seq
		}
	}); err != nil {
		t.Fatal(err)
	}
	if linked[p1.ID] == 0 || linked[p2.ID] == 0 || linked[p1.ID] == linked[p2.ID] {
		t.Fatalf("SSE missing distinct pending links: %v", linked)
	}
	if len(a.pendingOf(mgr.ID).Steer) != 1 || a.pendingOf(mgr.ID).Steer[0].ID != other.ID {
		t.Fatal("other session drained")
	}
	raw, err := a.Store.RawMessagesAll(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	audit := map[string]int64{}
	for _, sm := range raw {
		if sm.Kind == kindAudit {
			var row struct {
				Type      string `json:"type"`
				PendingID string `json:"pendingId"`
				Seq       int64  `json:"seq"`
			}
			_ = json.Unmarshal([]byte(sm.Text()), &row)
			if row.Type == "pending_landed" {
				audit[row.PendingID] = row.Seq
			}
		}
	}
	if audit[p1.ID] != linked[p1.ID] || audit[p2.ID] != linked[p2.ID] {
		t.Fatalf("durable audit mismatch: %v vs %v", audit, linked)
	}
	// 新 Store 连接模拟进程内关联内存完全丢失；仍从同一库恢复 UI 投影。
	reopened, err := NewStore(a.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	old := a.Store
	a.Store = reopened
	defer func() { a.Store = old }()
	r := httptest.NewRequest("GET", "/api/sessions/"+task.ID+"?tail=100", nil)
	r.SetPathValue("id", task.ID)
	w := httptest.NewRecorder()
	a.handleGetSession(w, r)
	var resp struct {
		Messages []struct {
			Seq       int64  `json:"seq"`
			PendingID string `json:"pendingId"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != 200 {
		t.Fatalf("GET %d: %v", w.Code, err)
	}
	seen := map[string]int64{}
	for _, m := range resp.Messages {
		if m.PendingID != "" {
			seen[m.PendingID] = m.Seq
		}
	}
	if len(seen) != 2 || seen[p1.ID] != linked[p1.ID] || seen[p2.ID] != linked[p2.ID] {
		t.Fatalf("reopened history projection mismatch: %v", seen)
	}
	t.Logf("two identical pending bodies: distinct SSE links, durable audits and reopened GET projection agree")
}
