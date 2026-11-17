package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestThinkingContinueRoundTrip(t *testing.T) {
	var bodies []ChatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		bodies = append(bodies, req)
		w.Header().Set("Content-Type", "text/event-stream")
		if len(bodies) == 1 {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\" 原样思考\\n🧭 \"},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":12}}\n\ndata: [DONE]\n\n")
		} else {
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"完成\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":3}}\n\ndata: [DONE]\n\n")
		}
	}))
	defer srv.Close()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{ID: "test", BaseURL: srv.URL, APIKey: "synthetic", Models: []ModelCfg{{ID: "model"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "test", Model: "model"}
	a.Cfg.MaxSteps = 8
	sess := &SessionMeta{ID: "thinking-roundtrip", Mode: "task", Thinking: "high", Workspace: t.TempDir(), CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	a.appendMessage(sess, TextMessage("user", "无敏感合成任务"))
	var events []Event
	err := a.RunAgent(context.Background(), sess, func(e Event) { events = append(events, e) })
	if err != nil {
		t.Fatalf("pure thinking must continue: %v", err)
	}
	if len(bodies) != 2 {
		t.Fatalf("requests=%d", len(bodies))
	}
	count := 0
	for _, m := range bodies[1].Messages {
		if m.ReasoningContent != nil && *m.ReasoningContent == " 原样思考\n🧭 " {
			count++
			if m.Role != "assistant" || m.Text() != "" || len(m.ToolCalls) > 0 {
				t.Fatal("invalid overlay")
			}
		}
	}
	if count != 1 {
		t.Fatalf("reasoning copies=%d", count)
	}
	if !strings.Contains(bodies[1].Messages[0].Text(), "上一响应仅返回思考") {
		t.Fatal("continuation instruction missing")
	}
	if bodies[1].ReasoningEffort != "high" || bodies[1].MaxTokens != 0 || bodies[1].Model != "model" {
		t.Fatal("target/settings changed")
	}
	if sess.UsageCalls != 2 || sess.UsageOutput != 15 {
		t.Fatalf("usage=%+v", sess)
	}
	found := false
	for _, e := range events {
		if e.Type == "thinking_continue" && strings.Contains(e.Output, "1/2") {
			found = true
		}
	}
	if !found {
		t.Fatal("visible continuation missing")
	}
	rows, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	pure, final := 0, 0
	for _, r := range rows {
		if r.Role == "assistant" && r.Thinking == " 原样思考\n🧭 " && r.Text() == "" {
			pure++
		}
		if r.Role == "assistant" && r.Text() == "完成" {
			final++
		}
	}
	if pure != 1 || final != 1 {
		t.Fatalf("stored pure=%d final=%d", pure, final)
	}
	for _, m := range a.buildContext(sess, "sys", 0) {
		if m.ReasoningContent != nil && *m.ReasoningContent == " 原样思考\n🧭 " {
			t.Fatal("overlay leaked into ordinary history")
		}
	}
	recs, err := a.ctxEngine().RecentRequests(sess, 2)
	if err != nil || len(recs) != 2 {
		t.Fatalf("request journal: %v %d", err, len(recs))
	}
	if recs[1].MsgCount != len(bodies[1].Messages) || recs[1].EstTokens <= recs[0].EstTokens || recs[1].SysHash == recs[0].SysHash {
		t.Fatal("overlay measurement/journal mismatch")
	}
}

func thinkingTestApp(t *testing.T, protocol string, handler http.HandlerFunc) (*App, *SessionMeta) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{ID: "test", BaseURL: srv.URL, APIKey: "synthetic", Protocol: protocol, Models: []ModelCfg{{ID: "model"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "test", Model: "model"}
	a.Cfg.MaxSteps = 12
	sess := &SessionMeta{ID: "thinking-test", Mode: "task", Thinking: "high", Workspace: t.TempDir(), CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	a.appendMessage(sess, TextMessage("user", "合成任务"))
	return a, sess
}

func thinkingSSE(w http.ResponseWriter, reasoning, text, reason string, done bool, fragment bool) {
	w.Header().Set("Content-Type", "text/event-stream")
	delta := map[string]any{"reasoning_content": reasoning, "content": text}
	if fragment {
		delta["tool_calls"] = []any{map[string]any{"index": 0, "function": map[string]any{"arguments": "{"}}}
	}
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta, "finish_reason": reason}}})
	fmt.Fprintf(w, "data: %s\n\n", b)
	if done {
		fmt.Fprint(w, "data: [DONE]\n\n")
	}
}

func TestThinkingContinueTriggerMatrix(t *testing.T) {
	for _, tt := range []struct {
		name, reasoning, text, reason string
		done, fragment, want          bool
	}{
		{"normal", "thought", "", "stop", true, false, true},
		{"length", "thought", "", "length", true, false, true},
		{"no-marker", "thought", "", "stop", false, false, false},
		{"no-reason", "thought", "", "", true, false, false},
		{"unknown", "thought", "", "unknown", true, false, false},
		{"filter", "thought", "", "content_filter", true, false, false},
		{"empty", "", "", "stop", true, false, false},
		{"blank", " \n ", "", "stop", true, false, false},
		{"text", "thought", "answer", "stop", true, false, false},
		{"fragment", "thought", "", "stop", true, true, false},
		{"tool-ending", "thought", "", "tool_calls", true, false, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := 0
			a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					thinkingSSE(w, tt.reasoning, tt.text, tt.reason, tt.done, tt.fragment)
				} else {
					thinkingSSE(w, "", "done", "stop", true, false)
				}
			})
			err := a.RunAgent(context.Background(), sess, func(Event) {})
			if tt.want {
				if n != 2 || err != nil {
					t.Fatalf("requests=%d err=%v", n, err)
				}
			} else if n != 1 {
				t.Fatalf("unexpected continuation requests=%d err=%v", n, err)
			}
		})
	}
}

func TestThinkingContinueLimit(t *testing.T) {
	n := 0
	a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
		n++
		thinkingSSE(w, fmt.Sprintf("thought-%d", n), "", "length", true, false)
	})
	err := a.RunAgent(context.Background(), sess, func(Event) {})
	if n != 3 || err == nil || !strings.Contains(err.Error(), "2 次") {
		t.Fatalf("requests=%d err=%v", n, err)
	}
	rows, _ := a.Store.RawMessagesAll(sess.ID)
	count := 0
	for _, r := range rows {
		if r.Role == "assistant" && r.Thinking != "" {
			count++
		}
	}
	if count != 3 {
		t.Fatalf("saved thoughts=%d", count)
	}
}

func TestThinkingContinueStopAndSteerBeforeRequest(t *testing.T) {
	for _, steer := range []bool{false, true} {
		t.Run(fmt.Sprint(steer), func(t *testing.T) {
			var bodies []ChatRequest
			a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
				var req ChatRequest
				json.NewDecoder(r.Body).Decode(&req)
				bodies = append(bodies, req)
				if len(bodies) == 1 {
					thinkingSSE(w, "thought", "", "stop", true, false)
				} else {
					thinkingSSE(w, "", "new answer", "stop", true, false)
				}
			})
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := a.RunAgent(ctx, sess, func(e Event) {
				if e.Type == "thinking_continue" && e.Output != "" {
					if steer {
						a.pushPending(sess.ID, "steer", "新插话", "", "")
					} else {
						cancel()
					}
				}
			})
			if !steer {
				if len(bodies) != 1 || err != context.Canceled {
					t.Fatalf("after stop requests=%d err=%v", len(bodies), err)
				}
			} else {
				if len(bodies) != 2 || err != nil {
					t.Fatalf("steer requests=%d err=%v", len(bodies), err)
				}
				if strings.Contains(bodies[1].Messages[0].Text(), thinkingContinueInstruction) {
					t.Fatal("stale continuation instruction")
				}
				found := false
				for _, m := range bodies[1].Messages {
					if m.Text() == "新插话" {
						found = true
					}
				}
				if !found {
					t.Fatal("steer lost")
				}
			}
		})
	}
}

func TestThinkingContinueFailureNotReposted(t *testing.T) {
	for _, status := range []int{429, 500, 400, 413} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			n := 0
			a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
				n++
				if n == 1 {
					thinkingSSE(w, "thought", "", "length", true, false)
				} else {
					w.WriteHeader(status)
					if status == 413 {
						fmt.Fprint(w, `{"error":{"message":"context length exceeded"}}`)
					} else {
						fmt.Fprint(w, `{"error":{"message":"synthetic failure"}}`)
					}
				}
			})
			err := a.RunAgent(context.Background(), sess, func(Event) {})
			if n != 2 || err == nil {
				t.Fatalf("failed continuation reposted: n=%d err=%v", n, err)
			}
		})
	}
}

func TestThinkingContinueNativeProtocolsUnchanged(t *testing.T) {
	for _, protocol := range []string{"anthropic", "responses"} {
		t.Run(protocol, func(t *testing.T) {
			n := 0
			a, sess := thinkingTestApp(t, protocol, func(w http.ResponseWriter, r *http.Request) {
				n++
				w.Header().Set("Content-Type", "text/event-stream")
				if protocol == "anthropic" {
					fmt.Fprint(w, "data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"thought\"}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n")
				} else {
					fmt.Fprint(w, "data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"thought\"}\n\ndata: {\"type\":\"response.completed\"}\n\n")
				}
			})
			err := a.RunAgent(context.Background(), sess, func(Event) {})
			if n != 1 || err == nil || !strings.Contains(err.Error(), "只返回了思考") {
				t.Fatalf("native changed: n=%d err=%v", n, err)
			}
		})
	}
}

func TestThinkingContinueMaxSteps(t *testing.T) {
	n := 0
	a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) { n++; thinkingSSE(w, "thought", "", "stop", true, false) })
	a.Cfg.MaxSteps = 1
	err := a.RunAgent(context.Background(), sess, func(e Event) {
		if e.Type == "thinking_continue" && e.Output != "" {
			t.Error("must not promise continuation beyond maxsteps")
		}
	})
	if n != 1 || err == nil || !strings.Contains(err.Error(), "连续 1 步") {
		t.Fatalf("n=%d err=%v", n, err)
	}
}

func TestThinkingContinueLedgerAcrossTools(t *testing.T) {
	n := 0
	var bodies []ChatRequest
	a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
		var req ChatRequest
		json.NewDecoder(r.Body).Decode(&req)
		bodies = append(bodies, req)
		n++
		if n == 2 {
			w.Header().Set("Content-Type", "text/event-stream")
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"function\":{\"name\":\"progress_write\",\"arguments\":\"{\\\"progress\\\":[]}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		} else {
			thinkingSSE(w, fmt.Sprintf("thinking-%d", n), "", "stop", true, false)
		}
	})
	err := a.RunAgent(context.Background(), sess, func(Event) {})
	if n != 4 || err == nil || !strings.Contains(err.Error(), "2 次") {
		t.Fatalf("tool reset ledger: n=%d err=%v", n, err)
	}
	// 第三请求是正常工具轮，不应带一次性说明，但先前纯思考仍按seq在工具前。
	if strings.Contains(bodies[2].Messages[0].Text(), thinkingContinueInstruction) {
		t.Fatal("instruction survived normal tool step")
	}
	positions := map[string]int{}
	for i, m := range bodies[2].Messages {
		if m.ReasoningContent != nil && *m.ReasoningContent == "thinking-1" {
			positions["thought"] = i
		}
		if len(m.ToolCalls) > 0 {
			positions["call"] = i
		}
		if m.Role == "tool" {
			positions["result"] = i
		}
	}
	if len(positions) != 3 || !(positions["thought"] < positions["call"] && positions["call"] < positions["result"]) {
		t.Fatalf("overlay order: %v", positions)
	}
	// 不重新派发工具；工具行仅一条，结果仅一条。
	rows, _ := a.Store.RawMessagesAll(sess.ID)
	calls, results := 0, 0
	for _, r := range rows {
		calls += len(r.ToolCalls)
		if r.Role == "tool" {
			results++
		}
	}
	if calls != 1 || results != 1 {
		t.Fatalf("replayed calls=%d results=%d", calls, results)
	}
}

func TestThinkingContinueMalformedAndDSML(t *testing.T) {
	for _, kind := range []string{"malformed", "error-frame", "error-usage", "dsml"} {
		t.Run(kind, func(t *testing.T) {
			n, hints := 0, 0
			a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
				n++
				if n > 1 {
					thinkingSSE(w, "", "done", "stop", true, false)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thought\"}}]}\n\n")
				switch kind {
				case "malformed":
					fmt.Fprint(w, "data: {invalid\n\n")
				case "error-frame":
					fmt.Fprint(w, "data: {\"error\":{\"message\":\"failed\"}}\n\n")
				case "error-usage":
					fmt.Fprint(w, "data: {\"error\":{\"message\":\"failed\"},\"usage\":{}}\n\n")
				case "dsml":
					fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"</｜DSML｜tool_calls>\"}}]}\n\n")
				}
				fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			})
			_ = a.RunAgent(context.Background(), sess, func(e Event) {
				if e.Type == "thinking_continue" && e.Output != "" {
					hints++
				}
			})
			if hints != 0 {
				t.Fatalf("%s triggered thinking continuation", kind)
			}
			if kind != "dsml" && n != 1 {
				t.Fatalf("malformed stream continued: %d", n)
			}
		})
	}
}

func TestThinkingContinueOverlayCoveredStops(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "covered", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	sm := a.appendMessageThinking(sess, TextMessage("assistant", ""), "preserve exactly")
	s := thinkingContinuation{}
	if err := s.schedule(sm); err != nil {
		t.Fatal(err)
	}
	if !s.completeMaterials(a.buildContextWithThinking(sess, "sys", 0, s.rows)) {
		t.Fatal("overlay not included")
	}
	a.appendRow(sess, StoredMessage{Message: TextMessage("user", "summary"), Kind: kindSummary, CoversFrom: sm.Seq, CoversUpTo: sm.Seq, CreatedAt: now()})
	if s.completeMaterials(a.buildContextWithThinking(sess, "sys", 0, s.rows)) {
		t.Fatal("covered thought silently missing")
	}
}

func TestThinkingContinueNoAlternateAddress(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 0)
	var addresses []string
	withStreamHTTP(t, &http.Client{Transport: p1RoundTripFunc(func(r *http.Request) (*http.Response, error) {
		addresses = append(addresses, r.URL.Host)
		return nil, errors.New("connection refused")
	})})
	req := connTestReq()
	req.AutoContinuation = true
	err := StreamChat(context.Background(), connTestTarget("", "http://primary.test", "http://alternate.test"), req, StreamCallbacks{})
	if err == nil || len(addresses) != 2 {
		t.Fatalf("err=%v attempts=%v", err, addresses)
	}
	for _, addr := range addresses {
		if addr != "primary.test" {
			t.Fatalf("continuation changed address: %v", addresses)
		}
	}
	addresses = nil
	req.ContinuationBaseURL = "http://previous-success.test"
	err = StreamChat(context.Background(), connTestTarget("", "http://primary.test", "http://alternate.test"), req, StreamCallbacks{})
	if err == nil || len(addresses) != 2 {
		t.Fatalf("pinned err=%v attempts=%v", err, addresses)
	}
	for _, addr := range addresses {
		if addr != "previous-success.test" {
			t.Fatalf("lost previous response address: %v", addresses)
		}
	}
}

func TestThinkingContinueStopInFlight(t *testing.T) {
	var hits atomic.Int32
	a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			thinkingSSE(w, "first", "", "stop", true, false)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"partial continuation\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	err := a.RunAgent(ctx, sess, func(e Event) {
		if e.Type == "reasoning" && e.Delta == "partial continuation" {
			cancel()
		}
	})
	if hits.Load() != 2 || err == nil {
		t.Fatalf("hits=%d err=%v", hits.Load(), err)
	}
	rows, _ := a.Store.RawMessagesAll(sess.ID)
	found := false
	for _, r := range rows {
		if r.Thinking == "partial continuation" {
			found = true
		}
	}
	if !found {
		t.Fatal("cancelled continuation thinking lost")
	}
}

func TestThinkingContinueRegisteredRunSteerInFlight(t *testing.T) {
	var hits atomic.Int32
	var last ChatRequest
	a, sess := thinkingTestApp(t, "", func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		var req ChatRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		if n == 1 {
			thinkingSSE(w, "first", "", "stop", true, false)
			return
		}
		if n == 3 {
			last = req
			thinkingSSE(w, "", "after steer", "stop", true, false)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"interrupt me\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
		}
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	run := &sessionRun{ctx: ctx, cancel: cancel, subs: map[int]chan Event{}}
	a.runners[sess.ID] = run
	err := a.RunAgent(ctx, sess, func(e Event) {
		run.observe(e)
		if e.Type == "reasoning" && e.Delta == "interrupt me" {
			a.pushPending(sess.ID, "steer", "runtime steer", "", "")
		}
	})
	if err != nil || hits.Load() != 3 {
		t.Fatalf("steer failed hits=%d err=%v", hits.Load(), err)
	}
	if strings.Contains(last.Messages[0].Text(), thinkingContinueInstruction) {
		t.Fatal("stale instruction after kick")
	}
	found := false
	for _, m := range last.Messages {
		if m.Text() == "runtime steer" {
			found = true
		}
	}
	if !found {
		t.Fatal("registered kick lost steer")
	}
	rows, _ := a.Store.RawMessagesAll(sess.ID)
	found = false
	for _, r := range rows {
		if r.Thinking == "interrupt me" {
			found = true
		}
	}
	if !found {
		t.Fatal("interrupted reasoning lost")
	}
}

func TestThinkingContinuePlanResetKeepsLedger(t *testing.T) {
	b, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	if strings.Count(src, "var thinkingNext thinkingContinuation") != 1 {
		t.Fatal("ledger must be created once per RunAgent")
	}
	reset := src[strings.LastIndex(src, "if planReset {"):]
	if strings.Contains(reset, "thinkingNext") {
		t.Fatal("plan reset must not change continuation ledger")
	}
}
func TestThinkingContinueUnrelatedHistoryDoesNotBlock(t *testing.T) {
	thought := "unrelated reasoning"
	unrelated := Message{Role: "assistant", Content: "", ReasoningContent: &thought}
	var s thinkingContinuation
	if !s.completeMaterials([]Message{unrelated}) {
		t.Fatal("ordinary request blocked by non-overlay reasoning")
	}
	if err := s.schedule(&StoredMessage{Seq: 42, Thinking: thought}); err != nil {
		t.Fatal(err)
	}
	if s.completeMaterials([]Message{unrelated}) {
		t.Fatal("non-overlay history impersonated selected seq")
	}
}

func TestThinkingContinueOverlayMissingAndLedger(t *testing.T) {
	var s thinkingContinuation
	row := &StoredMessage{Seq: 10, Thinking: "same"}
	if err := s.schedule(row); err != nil {
		t.Fatal(err)
	}
	s.pending = false // 普通工具轮/插话/计划重置不应刷新额度
	if err := s.schedule(&StoredMessage{Seq: 20, Thinking: "same"}); err != nil {
		t.Fatal(err)
	}
	if err := s.schedule(&StoredMessage{Seq: 30, Thinking: "last"}); err == nil {
		t.Fatal("ledger reset")
	}
	thought := "same"
	one := Message{Role: "assistant", Content: "", ReasoningContent: &thought, thinkingOverlaySeq: 10}
	two := one
	two.thinkingOverlaySeq = 20
	if s.completeMaterials([]Message{one}) || s.completeMaterials([]Message{one, one}) || !s.completeMaterials([]Message{one, two}) {
		t.Fatal("duplicate seq text multiplicity lost")
	}
}
