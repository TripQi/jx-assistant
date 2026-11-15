package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestP2L11ImageModelDiagnostics(t *testing.T) {
	for _, mode := range []string{"unavailable", "text", "vision"} {
		t.Run(mode, func(t *testing.T) {
			a := newCompactTestApp(t)
			a.initRuntime()
			dir := t.TempDir()
			a.Cfg.Workspace = dir
			a.Cfg.Providers = []ProviderCfg{{ID: "p", BaseURL: "mock", Models: []ModelCfg{{ID: "m", SupportsImages: mode == "vision"}}}}
			a.Cfg.DefaultModel = ModelRef{Provider: "p", Model: "m"}
			sess := &SessionMeta{ID: "image-diag", Workspace: dir, Permission: permFull}
			if mode == "unavailable" {
				sess.Provider, sess.Model = "missing-provider", "m"
			}
			var b bytes.Buffer
			if err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(dir, "image.png")
			if err := os.WriteFile(path, b.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			args, _ := json.Marshal(map[string]string{"path": path})
			out, err := runReadImage(&toolContext{ctx: context.Background(), app: a, sess: sess, emit: func(Event) {}}, args)
			if err != nil {
				t.Fatalf("attachment ingestion must succeed: %v", err)
			}
			att, err := a.storeAttachment(b.Bytes(), "image/png")
			if err != nil {
				t.Fatal(err)
			}
			raw, _, err := a.loadAttachment(att.File)
			if err != nil || !bytes.Equal(raw, b.Bytes()) || !strings.Contains(out, imageEnvelope(att, path)) {
				t.Fatal("successful attachment/envelope lost")
			}
			if mode == "unavailable" {
				_, cause := a.resolveTarget(sess)
				if cause == nil || !strings.Contains(out, cause.Error()) || strings.Contains(out, "当前模型不支持图片") {
					t.Fatalf("model unavailable cause missing or mislabeled: %s", out)
				}
			} else if strings.Contains(out, "当前模型不支持图片") != (mode == "text") {
				t.Fatalf("wrong image capability diagnostic: %s", out)
			}
			t.Logf("%s: ingestion and envelope preserved; diagnostic correct", mode)
		})
	}
}

func TestP2L12BodyMarkersCannotBypassBudget(t *testing.T) {
	for _, marker := range []string{"过长已截断：原始共", "工具结果过长已截断", "输出过长已截断：原始共 999 字节"} {
		t.Run(marker, func(t *testing.T) {
			small := fmt.Sprintf("const example = %q", marker)
			if clipToolResult(small) != small || clipHeadTail(small, cmdOutputMaxBytes, cmdTruncNote) != small {
				t.Fatal("legal source example changed")
			}
			big := small + strings.Repeat("字😀", toolResultMaxBytes) + "TAIL"
			for name, got := range map[string]string{"tool": clipToolResult(big), "command": clipHeadTail(big, cmdOutputMaxBytes, cmdTruncNote)} {
				limit := toolResultMaxBytes
				if name == "command" {
					limit = cmdOutputMaxBytes
				}
				if len(got) > limit || !utf8.ValidString(got) || !strings.HasPrefix(got, small) || !strings.HasSuffix(got, "TAIL") {
					t.Errorf("%s marker bypass/bad cut: got=%d limit=%d validUTF8=%v", name, len(got), limit, utf8.ValidString(got))
				}
			}
		})
	}
}

func TestP2L13ExpertFallbackFollowsDefault(t *testing.T) {
	// 检查竞态窗口的结构契约：本轮必须用已成功解析的快照，不得清字段后再次解析。
	raw, err := os.ReadFile("agent.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "if !modelFallback && sess.Expert")
	if start < 0 {
		t.Fatal("fallback branch missing")
	}
	end := strings.Index(src[start:], "// 半截正文落盘")
	if end < 0 {
		t.Fatal("fallback branch missing")
	}
	branch := src[start : start+end]
	if strings.Contains(branch, "a.resolveTarget(sess)") || !strings.Contains(branch, "target = def") {
		t.Error("fallback re-resolves mutable default instead of reusing resolved def")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Model == "expert" {
			http.Error(w, `{"error":{"message":"model not found"}}`, 404)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}))
	defer srv.Close()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{ID: "p", BaseURL: srv.URL, APIKey: "synthetic", Models: []ModelCfg{{ID: "expert"}, {ID: "default"}, {ID: "next"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "p", Model: "default"}
	sess := &SessionMeta{ID: "expert-follow", Expert: "test-expert", Provider: "p", Model: "expert", Mode: "task", Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := a.RunAgent(ctx, sess, func(Event) {}); err != nil {
		t.Fatal(err)
	}
	saved, err := a.Store.Get(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if saved.Provider != "" || saved.Model != "" || saved.ProviderKey != "" {
		t.Fatal("fallback pinned session instead of following default")
	}
	a.Cfg.DefaultModel.Model = "next"
	target, err := a.resolveTarget(saved)
	if err != nil || target.Model != "next" {
		t.Fatalf("later resolution must follow changed default: model=%s err=%v", target.Model, err)
	}
	t.Log("fallback clears persisted reference; later resolution follows next default")
}

func TestP2L17SummaryCrossWording(t *testing.T) {
	for _, word := range []string{"跨任务", "跨压缩", "跨天数"} {
		if !strings.Contains(summarizeSectionsAssistant, word) {
			t.Errorf("summary directive missing %s", word)
		}
	}
	for _, word := range []string{"跳任务", "跳压缩", "跳天数"} {
		if strings.Contains(summarizeSectionsAssistant, word) {
			t.Errorf("summary directive still contains %s", word)
		}
	}
}
