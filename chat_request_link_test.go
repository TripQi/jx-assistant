package main

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// 独立临时库，不接触运行中的 config.json/data，也不启动外部服务。
func newChatRequestTestApp(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	if err := os.MkdirAll(".jxai/tmp", 0755); err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp(".jxai/tmp", "chat-request-")
	if err != nil {
		t.Fatal(err)
	}
	dir, err = filepath.Abs(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := DefaultConfig()
	cfg.DataDir, cfg.Workspace = dir, dir
	cfg.Providers = nil // 执行体立即报未配置模型，不联网、不写工作文件。
	a := &App{Cfg: cfg, Store: s, sessLocks: map[string]*sync.Mutex{}, Components: InitComponents(filepath.Join(dir, "components"), filepath.Join(dir, "components.json"))}
	a.initRuntime()
	m := &SessionMeta{ID: "chat-request", Mode: "task", Workspace: dir, Title: "请求关联", CreatedAt: now(), UpdatedAt: now()}
	if err := s.Create(m); err != nil {
		t.Fatal(err)
	}
	return a, m
}

func chatRequestRow(t *testing.T, content, requestID string) *StoredMessage {
	t.Helper()
	// 用 wire 字段造记录，修前 overlay 也能编译，并由真实存储断言暴露缺失。
	b, err := json.Marshal(map[string]any{"role": "user", "content": content, "kind": "chat", "requestId": requestID, "createdAt": now()})
	if err != nil {
		t.Fatal(err)
	}
	var sm StoredMessage
	if err := json.Unmarshal(b, &sm); err != nil {
		t.Fatal(err)
	}
	return &sm
}

func chatRequestProjection(t *testing.T, a *App, sid string) map[string]int64 {
	t.Helper()
	r := httptest.NewRequest("GET", "/api/sessions/"+sid+"?tail=100", nil)
	r.SetPathValue("id", sid)
	w := httptest.NewRecorder()
	a.handleGetSession(w, r)
	var resp struct {
		Messages []struct {
			Seq       int64  `json:"seq"`
			RequestID string `json:"requestId"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || w.Code != 200 {
		t.Fatalf("GET %d: %v %s", w.Code, err, w.Body.String())
	}
	out := map[string]int64{}
	for _, m := range resp.Messages {
		if m.RequestID != "" {
			out[m.RequestID] = m.Seq
		}
	}
	return out
}

func TestChatRequestPersistenceAndAPIProjection(t *testing.T) {
	a, m := newChatRequestTestApp(t)
	want := map[string]int64{}
	for _, pair := range [][2]string{{"repeat", ""}, {"repeat", "req-1"}, {"repeat", "req-2"}, {"rewritten by hook", "req-hook"}} {
		sm := chatRequestRow(t, pair[0], pair[1])
		if err := a.Store.AppendRow(m.ID, sm); err != nil {
			t.Fatal(err)
		}
		if pair[1] != "" {
			want[pair[1]] = sm.Seq
		}
	}
	other := &SessionMeta{ID: "other", Mode: "task", CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(other); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow(other.ID, chatRequestRow(t, "repeat", "foreign")); err != nil {
		t.Fatal(err)
	}
	raw, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	links := map[string]int64{}
	for _, sm := range raw {
		if sm.Kind == kindAudit && sm.Origin == "req_landed" {
			var link struct {
				Type      string `json:"type"`
				RequestID string `json:"requestId"`
				Seq       int64  `json:"seq"`
			}
			if err := json.Unmarshal([]byte(sm.Text()), &link); err != nil {
				t.Fatal(err)
			}
			if link.Type != "req_landed" {
				t.Fatalf("wrong audit: %s", sm.Text())
			}
			links[link.RequestID] = link.Seq
		}
		if sm.Role == "user" {
			wire, _ := json.Marshal(sm)
			model, _ := json.Marshal(sm.Message)
			if strings.Contains(string(wire), "requestId") || strings.Contains(string(model), "requestId") {
				t.Fatal("request identity leaked into ordinary storage read/model Message")
			}
		}
	}
	if len(links) != len(want) {
		t.Fatalf("durable links: got %v want %v", links, want)
	}
	for id, seq := range want {
		if links[id] != seq {
			t.Fatalf("wrong audit for %s", id)
		}
	}
	reopened, err := NewStore(a.Cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	a.Store = reopened
	got := chatRequestProjection(t, a, m.ID)
	if len(got) != len(want) {
		t.Fatalf("reopened GET: got %v want %v", got, want)
	}
	for id, seq := range want {
		if got[id] != seq {
			t.Fatalf("GET identity mismatch %s: %v", id, got)
		}
	}
}

func TestChatRequestAuditFailureRollsBackUser(t *testing.T) {
	a, m := newChatRequestTestApp(t)
	_, err := a.Store.db.Exec(`CREATE TRIGGER reject_req_landed BEFORE INSERT ON messages WHEN NEW.origin='req_landed' BEGIN SELECT RAISE(ABORT, 'reject request audit'); END`)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow(m.ID, chatRequestRow(t, "must roll back", "req-fail")); err == nil {
		t.Fatal("expected transactional audit failure")
	}
	rows, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("user row survived failed audit: %d rows", len(rows))
	}
}

func TestChatRequestHandleChatWiring(t *testing.T) {
	a, m := newChatRequestTestApp(t)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r := httptest.NewRequest("POST", "/api/sessions/"+m.ID+"/chat", strings.NewReader(`{"content":"repeat","requestId":"post-request"}`)).WithContext(ctx)
	r.SetPathValue("id", m.ID)
	w := httptest.NewRecorder()
	a.handleChat(w, r)
	if run := a.runOf(m.ID); run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			t.Fatal("chat run did not finish")
		}
	}
	if w.Code != 200 {
		t.Fatalf("POST %d: %s", w.Code, w.Body.String())
	}
	got := chatRequestProjection(t, a, m.ID)
	if got["post-request"] == 0 {
		t.Fatalf("POST body identity not persisted/projected: %v", got)
	}
}
