package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The first real model request stays blocked while HTTP PATCH loads and saves its
// own Store.Get copy. No mutation of the running SessionMeta is used as evidence.
func b2RunApp(t *testing.T, mode string) (*App, *SessionMeta, Project, Project) {
	t.Helper()
	a := projectCapabilityTestApp(t)
	a.Cfg = DefaultConfig()
	a.Cfg.DataDir = t.TempDir()
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.CompactInterval = 0
	a.Cfg.CompactLlmPct = 100
	a.Cfg.MaxSteps = 3
	a.initRuntime()
	a.SessionCX = loadSessionCX(a.Store)
	pa := projectBindTest(t, a, "b2-project-a", filepath.Join(t.TempDir(), "a"))
	pb := projectBindTest(t, a, "b2-project-b", filepath.Join(t.TempDir(), "b"))
	saveKnowledgeEnvTest(t, a, pa.ID, "B2_ONLY_A_CONSTRAINT")
	saveKnowledgeEnvTest(t, a, pb.ID, "B2_ONLY_B_CONSTRAINT")
	s := &SessionMeta{ID: "b2-running", Mode: mode, Workspace: pa.Workspaces[0].DisplayPath, Permission: permFull}
	if err := a.Store.Create(s); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow(s.ID, &StoredMessage{Message: TextMessage("user", "Check current project knowledge."), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	return a, s, pa, pb
}

func b2PatchSession(t *testing.T, a *App, id string, body any) {
	t.Helper()
	// Exercise the actual handler and its sessionFromPath -> Store.Get path.
	mux := http.NewServeMux()
	mux.HandleFunc("PATCH /api/sessions/{id}", a.handlePatchSession)
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodPatch, "/api/sessions/"+id, strings.NewReader(string(data))))
	if w.Code != http.StatusOK {
		t.Fatalf("PATCH status=%d: %s", w.Code, w.Body.String())
	}
}

func b2BlockedModel(t *testing.T, a *App, args any) (<-chan ChatRequest, func()) {
	t.Helper()
	requests := make(chan ChatRequest, 3)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	var mu sync.Mutex
	count := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		mu.Lock()
		count++
		n := count
		mu.Unlock()
		requests <- req
		w.Header().Set("Content-Type", "text/event-stream")
		if n == 1 {
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
			raw, _ := json.Marshal(args)
			delta := map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "b2-call", "type": "function", "function": map[string]any{"name": "component_call", "arguments": string(raw)}}}}
			chunk, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": "tool_calls"}}})
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", chunk)
			return
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"Finished.\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(unblock)
	a.Cfg.Providers = []ProviderCfg{{ID: "b2-mock", BaseURL: srv.URL, APIKey: "fake", Models: []ModelCfg{{ID: "b2-model", ContextWindow: 262144}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "b2-mock", Model: "b2-model"}
	return requests, unblock
}

func b2NextRequest(t *testing.T, requests <-chan ChatRequest) ChatRequest {
	t.Helper()
	select {
	case req := <-requests:
		return req
	case <-time.After(10 * time.Second):
		t.Fatal("real RunAgent model request did not arrive")
		return ChatRequest{}
	}
}

func b2StartAgent(t *testing.T, a *App, s *SessionMeta) <-chan error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- a.RunAgent(ctx, s, func(Event) {}) }()
	return done
}

func b2FinishAgent(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunAgent: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("RunAgent did not finish (possible Store reentry deadlock)")
	}
}

func TestB2RunAgentHTTPPatchWorkspaceRefreshRevokesOldKnowledge(t *testing.T) {
	a, live, pa, pb := b2RunApp(t, "task")
	requests, release := b2BlockedModel(t, a, map[string]any{"component": projectKnowledgeComponentID, "capability": "project_knowledge_get", "args": map[string]any{}})
	done := b2StartAgent(t, a, live)
	first := b2NextRequest(t, requests)
	firstJSON, _ := json.Marshal(first)
	if !strings.Contains(string(firstJSON), "B2_ONLY_A_CONSTRAINT") || strings.Contains(string(firstJSON), "B2_ONLY_B_CONSTRAINT") {
		t.Fatal("first request did not contain only project A knowledge")
	}
	independent, err := a.Store.Get(live.ID)
	if err != nil || independent == live || independent.Workspace != pa.Workspaces[0].DisplayPath {
		t.Fatalf("not an independent stored session: %+v %v", independent, err)
	}
	b2PatchSession(t, a, independent.ID, map[string]any{"workspace": pb.Workspaces[0].DisplayPath})
	release()
	second := b2NextRequest(t, requests)
	// Historical A remains history; the last effective knowledge block must be B,
	// and an explicit replacement/revocation must accompany the transition.
	text := ""
	for _, msg := range second.Messages {
		text += messageTextOf(msg) + "\n"
	}
	start := strings.LastIndex(text, projectKnowledgeEnvStart)
	if start < 0 {
		t.Fatal("second request has no effective knowledge block")
	}
	block := projectKnowledgeEnvPart(text[start:])
	requireKnowledgeText(t, block, pb.ID, "B2_ONLY_B_CONSTRAINT", "整体替代")
	if strings.Contains(block, "B2_ONLY_A_CONSTRAINT") || strings.Contains(block, pa.ID) {
		t.Fatalf("project A still effective: %s", block)
	}
	if !strings.Contains(text, "整体替换或撤销") && !strings.Contains(text, "不再适用") {
		t.Fatal("missing explicit revocation of prior knowledge")
	}
	// The tool returned by the already in-flight first response also reads B.
	foundTool := false
	for _, msg := range second.Messages {
		if msg.Role == "tool" && msg.ToolCallID == "b2-call" {
			foundTool = true
			requireKnowledgeText(t, messageTextOf(msg), pb.ID, "B2_ONLY_B_CONSTRAINT")
		}
	}
	if !foundTool {
		t.Fatal("in-flight response tool result missing from second request")
	}
	b2FinishAgent(t, done)
	if live.Workspace != pb.Workspaces[0].DisplayPath {
		t.Fatal("running pointer was not refreshed")
	}
}

func TestB2RunAgentHTTPPatchRejectsInflightKnowledgeUpdate(t *testing.T) {
	for _, change := range []string{"read-only", "plan-mode"} {
		t.Run(change, func(t *testing.T) {
			a, live, pa, _ := b2RunApp(t, "assistant")
			before := projectTestKnowledge(t, a.Store, pa.ID)
			requests, release := b2BlockedModel(t, a, map[string]any{"component": projectKnowledgeComponentID, "capability": "project_knowledge_update", "args": map[string]any{"project_id": pa.ID, "expected_revision": before.Revision, "content": "MUST_NOT_BE_SAVED"}})
			done := b2StartAgent(t, a, live)
			b2NextRequest(t, requests)
			copy, err := a.Store.Get(live.ID)
			if err != nil || copy == live {
				t.Fatalf("independent Get: %v", err)
			}
			patch := map[string]any{"permission": permReadOnly}
			if change == "plan-mode" {
				patch = map[string]any{"planMode": true}
			}
			b2PatchSession(t, a, copy.ID, patch)
			release()
			second := b2NextRequest(t, requests)
			denied := false
			for _, msg := range second.Messages {
				if msg.Role == "tool" && msg.ToolCallID == "b2-call" {
					out := messageTextOf(msg)
					denied = strings.Contains(out, "只读") || strings.Contains(out, "计划模式")
				}
			}
			if !denied {
				t.Fatal("same-step in-flight update was not explicitly denied")
			}
			b2FinishAgent(t, done)
			if after := projectTestKnowledge(t, a.Store, pa.ID); after != before {
				t.Fatalf("denied write changed content/revision/time: before=%+v after=%+v", before, after)
			}
		})
	}
}

func TestB2SaveKnowledgeUsesTransactionSessionNotPreflightPointer(t *testing.T) {
	for _, change := range []string{"read-only", "plan-mode", "role", "deleted"} {
		t.Run(change, func(t *testing.T) {
			a, live, pa, _ := b2RunApp(t, "assistant")
			tctx := &toolContext{app: a, sess: live, ctx: context.Background()}
			if err := projectCapabilityContext(tctx, true); err != nil {
				t.Fatal(err)
			}
			before := projectTestKnowledge(t, a.Store, pa.ID)
			current, err := a.Store.Get(live.ID)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "read-only":
				current.Permission = permReadOnly
			case "plan-mode":
				current.PlanMode = true
			case "role":
				current.Mode = "task"
			}
			if change == "deleted" {
				err = a.Store.Delete(current.ID)
			} else {
				err = a.Store.SaveMeta(current)
			}
			if err != nil {
				t.Fatal(err)
			}
			_, err = a.Store.SaveProjectKnowledgeForSession(pa.ID, before.Revision, "must fail", live.ID, func(_ Project, snapshot *SessionMeta) error {
				auth := *tctx
				auth.sess = snapshot
				return projectCapabilitySessionContext(&auth, true)
			})
			if err == nil {
				t.Fatal("transaction trusted stale preflight authorization")
			}
			if after := projectTestKnowledge(t, a.Store, pa.ID); after != before {
				t.Fatal("transaction denial mutated knowledge")
			}
		})
	}
}

func TestB2SyncRunAuthorizationPreservesCountersAndRefreshesIdentity(t *testing.T) {
	a, live, _, _ := b2RunApp(t, "assistant")
	current, err := a.Store.Get(live.ID)
	if err != nil {
		t.Fatal(err)
	}
	current.Workspace = t.TempDir()
	current.Remote = &RemoteWorkspace{Host: "b2.invalid", User: "test", Dir: "/b2"}
	current.Permission, current.Mode, current.PlanMode = permReadOnly, "task", true
	current.ParentID, current.DispatchedBy = "parent", "manager"
	if err := a.Store.SaveMeta(current); err != nil {
		t.Fatal(err)
	}
	live.UsagePrompt, live.UsageCacheHit, live.UsageOutput, live.UsageCalls = 101, 102, 103, 104
	a.syncRunPerms(live)
	if live.Workspace != current.Workspace || live.Remote == nil || live.Remote.Host != current.Remote.Host || !live.PlanMode || live.Permission != permReadOnly || live.Mode != "task" || live.ParentID != "parent" || live.DispatchedBy != "manager" {
		t.Fatalf("authorization identity not refreshed: %+v", live)
	}
	if live.UsagePrompt != 101 || live.UsageCacheHit != 102 || live.UsageOutput != 103 || live.UsageCalls != 104 {
		t.Fatal("runtime counters overwritten by stored snapshot")
	}
}
