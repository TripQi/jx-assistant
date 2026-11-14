package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

// 走完整摘要纠偏和 HTTP 序列化，不以内部 Message 值代替 wire 证据。
func TestSummarizerProtocolNativeReasoningHop(t *testing.T) {
	for _, protocol := range []string{"responses", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			thinking := " 首跳思考\n原文\"🧭 "
			signature := "opaque+/Signature=="
			reasoning := map[string]any{"type": "reasoning", "id": "rs_1", "summary": []any{map[string]any{"type": "summary_text", "text": thinking}}, "encrypted_content": "opaque-encrypted", "status": "completed"}
			call := map[string]any{"type": "function_call", "id": "fc_1", "call_id": "call_1", "name": "read", "arguments": "{}", "status": "completed"}
			var bodies []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					w.WriteHeader(400)
					return
				}
				bodies = append(bodies, body)
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(v any) { b, _ := json.Marshal(v); fmt.Fprintf(w, "data: %s\n\n", b) }
				if protocol == "responses" {
					if len(bodies) == 1 {
						send(map[string]any{"type": "response.reasoning_summary_text.delta", "delta": thinking})
						send(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": reasoning})
						send(map[string]any{"type": "response.output_item.done", "output_index": 1, "item": call})
					} else {
						send(map[string]any{"type": "response.output_text.delta", "delta": strings.Repeat("完整摘要。", 20)})
					}
					send(map[string]any{"type": "response.completed"})
				} else {
					if len(bodies) == 1 {
						send(map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "thinking", "thinking": ""}})
						for _, delta := range []map[string]any{{"type": "thinking_delta", "thinking": thinking}, {"type": "signature_delta", "signature": "opaque+/"}, {"type": "signature_delta", "signature": "Signature=="}} {
							send(map[string]any{"type": "content_block_delta", "index": 0, "delta": delta})
						}
						send(map[string]any{"type": "content_block_stop", "index": 0})
						send(map[string]any{"type": "content_block_start", "index": 1, "content_block": map[string]any{"type": "tool_use", "id": "call_1", "name": "read", "input": map[string]any{}}})
						send(map[string]any{"type": "content_block_stop", "index": 1})
					} else {
						send(map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": strings.Repeat("完整摘要。", 20)}})
					}
					reason := "end_turn"
					if len(bodies) == 1 {
						reason = "tool_use"
					}
					send(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": reason}})
					send(map[string]any{"type": "message_stop"})
				}
			}))
			defer srv.Close()
			a := newCompactTestApp(t)
			a.initRuntime()
			a.Cfg.Providers = []ProviderCfg{{ID: "test", BaseURL: srv.URL, APIKey: "synthetic", Protocol: protocol, Models: []ModelCfg{{ID: "model"}}}}
			a.Cfg.DefaultModel = ModelRef{Provider: "test", Model: "model"}
			sess := &SessionMeta{ID: "native-hop", Mode: "task", Thinking: "high", CreatedAt: now(), UpdatedAt: now()}
			if err := a.Store.Create(sess); err != nil {
				t.Fatal(err)
			}
			_, err := a.ctxEngine().CallSummarizer(sess, []Message{TextMessage("user", "压缩历史")}, []ToolDef{{Type: "function", Function: ToolSpec{Name: "read"}}})
			if err != nil {
				t.Fatal(err)
			}
			if len(bodies) != 2 {
				t.Fatalf("hops=%d", len(bodies))
			}
			if protocol == "responses" {
				items := bodies[1]["input"].([]any)
				expectedReasoning := map[string]any{"type": "reasoning", "summary": reasoning["summary"]}
				if !reflect.DeepEqual(items[1], expectedReasoning) || !reflect.DeepEqual(items[2], call) {
					t.Fatalf("native items lost: %v", items)
				}
				if items[3].(map[string]any)["call_id"] != "call_1" {
					t.Fatal("unpaired tool result")
				}
				for _, body := range bodies {
					if _, ok := body["store"]; ok {
						t.Fatal("store must be omitted")
					}
					if _, ok := body["include"]; ok {
						t.Fatal("include must be omitted")
					}
					wire, _ := json.Marshal(body)
					if strings.Contains(string(wire), "encrypted_content") {
						t.Fatal("encrypted_content must not be sent")
					}
				}
				b, _ := json.Marshal(items[1:4])
				t.Logf("second-hop wire: %s", b)
			} else {
				msgs := bodies[1]["messages"].([]any)
				blocks := msgs[1].(map[string]any)["content"].([]any)
				block := blocks[0].(map[string]any)
				if block["thinking"] != thinking || block["signature"] != signature {
					t.Fatalf("thinking/signature lost: %v", block)
				}
				if blocks[1].(map[string]any)["id"] != "call_1" {
					t.Fatal("tool pairing lost")
				}
				b, _ := json.Marshal(blocks)
				t.Logf("second-hop wire: %s", b)
			}
		})
	}
}
