package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// 抓取真实 HTTP 第二跳，不通过内部 Message 值推断线上思考是否存在。
func TestSummarizerReasoningRoundTrip(t *testing.T) {
	for _, tc := range []struct {
		name, reasoning string
		secondTool      bool
	}{
		{"real-reasoning", " 原始思考\n保留空白、引号\"与 Unicode：思考🧭\n", false},
		{"no-reasoning-reported", "", false},
		{"two-tool-hops-still-fail", "真实首跳思考，不可替成空串", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const firstText = " 首跳正文\n尚未生成摘要，不得丢失。 "
			tools := []ToolDef{{Type: "function", Function: ToolSpec{Name: "read", Description: "只用于测试"}}}
			toolCalls := []ToolCall{
				{ID: "call-a", Type: "function", Function: ToolCallFunc{Name: "read", Arguments: `{"files":[{"path":"a.go"}]}`}},
				{ID: "call-b", Type: "function", Function: ToolCallFunc{Name: "read", Arguments: `{"files":[{"path":"b.go"}]}`}},
			}
			var mu sync.Mutex
			var bodies []map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				hop := len(bodies)
				bodies = append(bodies, body)
				mu.Unlock()
				w.Header().Set("Content-Type", "text/event-stream")
				send := func(delta map[string]any) {
					chunk, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": delta}}})
					if err != nil {
						t.Errorf("encode SSE: %v", err)
						return
					}
					fmt.Fprintf(w, "data: %s\n\n", chunk)
				}
				if hop == 0 || tc.secondTool {
					// 多增量串接必须保真；不 trim、不重排、不把思考混到正文。
					if tc.reasoning != "" {
						runes := []rune(tc.reasoning)
						mid := len(runes) / 2
						send(map[string]any{"reasoning_content": string(runes[:mid])})
						send(map[string]any{"reasoning_content": string(runes[mid:])})
					}
					send(map[string]any{"content": firstText[:1]})
					send(map[string]any{"content": firstText[1:]})
					for i, call := range toolCalls {
						send(map[string]any{"tool_calls": []any{map[string]any{
							"index": i, "id": call.ID, "type": call.Type, "function": call.Function,
						}}})
					}
				} else {
					send(map[string]any{"content": strings.Repeat("完整摘要。", 20)})
				}
				fmt.Fprint(w, "data: [DONE]\n\n")
			}))
			defer srv.Close()
			a := newCompactTestApp(t)
			a.initRuntime()
			a.Cfg.Providers = []ProviderCfg{{ID: "reasoning-test", BaseURL: srv.URL, APIKey: "synthetic",
				Models: []ModelCfg{{ID: "reasoning-model"}}}}
			a.Cfg.DefaultModel = ModelRef{Provider: "reasoning-test", Model: "reasoning-model"}
			sess := &SessionMeta{ID: "reasoning-roundtrip", Mode: "task", Thinking: "high", CreatedAt: now(), UpdatedAt: now()}
			if err := a.Store.Create(sess); err != nil {
				t.Fatal(err)
			}
			prefix := []Message{TextMessage("system", "合成系统提示"), TextMessage("user", "压缩合成历史")}
			out, err := a.ctxEngine().CallSummarizer(sess, prefix, tools)
			if tc.secondTool {
				if err == nil || !strings.Contains(err.Error(), "连续两次调用工具") || out != "" {
					t.Fatalf("连续工具仍应失败: out=%q err=%v", out, err)
				}
			} else if err != nil || out != strings.Repeat("完整摘要。", 20) {
				t.Fatalf("正文摘要失败: out=%q err=%v", out, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if len(bodies) != 2 {
				t.Fatalf("应恰好两跳，不得无限纠偏: %d", len(bodies))
			}
			h1 := bodies[0]["messages"].([]any)
			h2 := bodies[1]["messages"].([]any)
			if len(h2) != len(h1)+4 {
				t.Fatalf("两工具纠偏应追加assistant+两tool+user: %d -> %d", len(h1), len(h2))
			}
			for i := range h1 {
				if !reflect.DeepEqual(h1[i], h2[i]) {
					t.Fatalf("纠偏改写前缀第%d条: first=%v second=%v", i, h1[i], h2[i])
				}
			}
			assistant := h2[len(h1)].(map[string]any)
			if assistant["role"] != "assistant" || assistant["content"] != firstText {
				t.Errorf("第二跳必须回传首跳正文原文: %v", assistant)
			}
			if actual, ok := assistant["reasoning_content"].(string); !ok || actual != tc.reasoning {
				t.Errorf("第二跳真实思考未逐字回传: got=%#v want=%q", assistant["reasoning_content"], tc.reasoning)
			}
			encodedCalls, _ := json.Marshal(assistant["tool_calls"])
			var actualCalls []ToolCall
			if err := json.Unmarshal(encodedCalls, &actualCalls); err != nil || !reflect.DeepEqual(actualCalls, toolCalls) {
				t.Errorf("工具调用必须原样回传: %s err=%v", encodedCalls, err)
			}
			for i, call := range toolCalls {
				response := h2[len(h1)+1+i].(map[string]any)
				if response["role"] != "tool" || response["tool_call_id"] != call.ID || !strings.Contains(fmt.Sprint(response["content"]), "不允许调用工具") {
					t.Errorf("工具拒绝响应不配对: %v", response)
				}
			}
			if !reflect.DeepEqual(bodies[0]["tools"], bodies[1]["tools"]) || bodies[1]["reasoning_effort"] != "high" {
				t.Error("纠偏不能改工具目录或会话思考档位")
			}
			raw, err := a.Store.RawMessagesAll(sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			missingExplained := false
			for _, row := range raw {
				if row.Kind == kindAudit && strings.Contains(row.Text(), "未收到非空思考增量") {
					missingExplained = true
				}
			}
			if missingExplained != (tc.reasoning == "") {
				t.Errorf("缺失思考须如实说明，不得误报实际存在的思考: note=%v reasoning=%q", missingExplained, tc.reasoning)
			}
			t.Logf("第二跳首跳正文=%q reasoning_content=%q tools=%d; missing_reasoning_explained=%v", assistant["content"], assistant["reasoning_content"], len(actualCalls), missingExplained)
		})
	}
}
