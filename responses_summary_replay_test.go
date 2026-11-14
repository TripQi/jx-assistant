package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestResponsesSummaryOnlyWire(t *testing.T) {
	for _, summary := range []string{`[]`, `[{"type":"summary_text","text":" 第一段\n"},{"type":"summary_text","text":"第二段🧭 "}]`} {
		raw := json.RawMessage(`{"type":"reasoning","id":"unused","status":"completed","encrypted_content":"must-not-send","summary":` + summary + `}`)
		body, err := marshalResponses(LLMTarget{Model: "model"}, ChatRequest{Messages: []Message{{Role: "assistant", ResponsesItems: []json.RawMessage{raw}}}})
		if err != nil {
			t.Fatal(err)
		}
		var wire map[string]any
		if err = json.Unmarshal(body, &wire); err != nil {
			t.Fatal(err)
		}
		item := wire["input"].([]any)[0].(map[string]any)
		var expected any
		if err = json.Unmarshal([]byte(summary), &expected); err != nil {
			t.Fatal(err)
		}
		if len(item) != 2 || item["type"] != "reasoning" || !reflect.DeepEqual(item["summary"], expected) {
			t.Fatalf("summary-only wire: %s", body)
		}
		for _, forbidden := range []string{"encrypted_content", "store", "include", "unused"} {
			if strings.Contains(string(body), forbidden) {
				t.Fatalf("unexpected %s: %s", forbidden, body)
			}
		}
	}
}

func TestResponsesEncryptedReplayError(t *testing.T) {
	const upstream = "This channel requires encrypted_content for reasoning replay"
	for _, streaming := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", streaming), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if streaming {
					w.Header().Set("Content-Type", "text/event-stream")
					b, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{"error": map[string]any{"message": upstream}}})
					fmt.Fprintf(w, "data: %s\n\n", b)
				} else {
					w.WriteHeader(http.StatusBadRequest)
					json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"message": upstream}})
				}
			}))
			defer srv.Close()
			err := StreamChat(context.Background(), LLMTarget{BaseURL: srv.URL, Model: "model", Protocol: "responses"}, ChatRequest{Messages: []Message{TextMessage("user", "test")}}, StreamCallbacks{})
			if err == nil || !strings.Contains(err.Error(), upstream) || !strings.Contains(err.Error(), "该渠道要求加密推理回传") {
				t.Fatalf("unreadable error: %v", err)
			}
			t.Log(err)
		})
	}
	ordinary := fmt.Errorf("rate limited")
	if responsesReplayError(ordinary) != ordinary {
		t.Fatal("unrelated errors must stay unchanged")
	}
}
