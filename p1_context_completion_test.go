package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func completionFixture(protocol, ending string) string {
	text := strings.Repeat("完整摘要需要保留所有关键决定。", 8)
	b, _ := json.Marshal(text)
	switch protocol {
	case "responses":
		s := fmt.Sprintf("data: {\"type\":\"response.output_text.delta\",\"delta\":%s}\n\n", b)
		if ending == "normal" {
			return s + "data: {\"type\":\"response.completed\"}\n\n"
		}
		if ending == "truncated" {
			return s + "data: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n"
		}
		return s
	case "anthropic":
		s := fmt.Sprintf("data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%s}}\n\n", b)
		reason := "end_turn"
		if ending == "truncated" {
			reason = "max_tokens"
		}
		if ending != "missing" {
			s += fmt.Sprintf("data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":%q}}\n\ndata: {\"type\":\"message_stop\"}\n\n", reason)
		}
		return s
	case "chat":
		if ending == "reason-only" {
			return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s},\"finish_reason\":\"stop\"}]}\n\n", b)
		}
		if ending == "marker-only" {
			return fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\ndata: [DONE]\n\n", b)
		}
		fallthrough
	default:
		s := fmt.Sprintf("data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", b)
		reason := "stop"
		if ending == "truncated" {
			reason = "length"
		}
		if ending != "missing" {
			s += fmt.Sprintf("data: {\"choices\":[{\"delta\":{},\"finish_reason\":%q}]}\n\ndata: [DONE]\n\n", reason)
		}
		return s
	}
}

func TestP1F25LongSummary(t *testing.T) {
	for _, protocol := range []string{"chat", "responses", "anthropic"} {
		endings := []string{"truncated", "missing", "normal"}
		if protocol == "chat" {
			endings = append(endings, "reason-only", "marker-only")
		}
		for _, ending := range endings {
			t.Run(protocol+"/"+ending, func(t *testing.T) {
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "text/event-stream")
					fmt.Fprint(w, completionFixture(protocol, ending))
				}))
				defer srv.Close()
				a := newCompactTestApp(t)
				a.initRuntime()
				a.Cfg.Providers = []ProviderCfg{{ID: "test", BaseURL: srv.URL, APIKey: "synthetic", Protocol: protocol, Models: []ModelCfg{{ID: "model"}}}}
				a.Cfg.DefaultModel = ModelRef{Provider: "test", Model: "model"}
				sess := &SessionMeta{ID: "completion", Mode: "task", CreatedAt: now(), UpdatedAt: now()}
				if err := a.Store.Create(sess); err != nil {
					t.Fatal(err)
				}
				seedRound(t, a, sess.ID, "original", false)
				_, err := a.doSummarize(sess, "auto", 0, 0)
				rows, rerr := a.Store.RawMessagesAll(sess.ID)
				if rerr != nil {
					t.Fatal(rerr)
				}
				summaries, originals := 0, 0
				for _, r := range rows {
					if r.Kind == kindSummary {
						summaries++
					}
					if r.Kind == kindChat {
						originals++
					}
				}
				t.Logf("ending=%s err=%v summaries=%d originalRows=%d", ending, err, summaries, originals)
				if originals != 2 {
					t.Fatal("original history lost")
				}
				if ending == "normal" {
					if err != nil || summaries != 1 {
						t.Fatal("normal summary rejected")
					}
				} else if err == nil || summaries != 0 {
					t.Fatal("incomplete long summary must not persist")
				}
			})
		}
	}
}
