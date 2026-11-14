package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 只暂存隔离仓库中的合成源码，绝不读当前工作树的 diff。
func newGitSummaryRepo(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("测试需要 git")
	}
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "--quiet"}, {"add", "--", "example.go"}} {
		if args[0] == "add" {
			if err := os.WriteFile(filepath.Join(dir, "example.go"), []byte("package example\nconst synthetic = true\n"), 0600); err != nil {
				t.Fatal(err)
			}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
		hideConsoleWindow(cmd)
		out, err := cmd.CombinedOutput()
		cancel()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return dir
}

func gitSummaryLLM(t *testing.T, hits *atomic.Int32, wantModel string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req struct {
			Model    string `json:"model"`
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		if req.Model != wantModel {
			t.Errorf("请求模型 = %q，想要 %q", req.Model, wantModel)
		}
		if len(req.Messages) < 2 || !strings.Contains(req.Messages[1].Content, "synthetic") {
			t.Errorf("未收到隔离暂存 diff: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"fix: synthetic summary\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestGitSummaryRejectsInvalidSessionWithoutFallback(t *testing.T) {
	for _, name := range []string{"disabled-provider", "deleted-provider", "empty-key", "missing-session", "store-error"} {
		t.Run(name, func(t *testing.T) {
			a := newDefaultModelsApp(t)
			a.Cfg.Workspace = newGitSummaryRepo(t)
			var globalHits atomic.Int32
			global := gitSummaryLLM(t, &globalHits, "global-model")
			a.Cfg.Providers = []ProviderCfg{
				{ID: "global", BaseURL: global.URL, APIKey: "fake", Models: []ModelCfg{{ID: "global-model"}}},
				{ID: "session", BaseURL: global.URL, APIKey: "fake", Models: []ModelCfg{{ID: "session-model"}}},
			}
			a.Cfg.DefaultModel = ModelRef{Provider: "global", Model: "global-model"}
			if err := a.Store.Create(&SessionMeta{ID: "chosen", Workspace: a.Cfg.Workspace, Provider: "session", Model: "session-model"}); err != nil {
				t.Fatal(err)
			}
			sid := "chosen"
			switch name {
			case "disabled-provider":
				a.Cfg.Providers[1].Enabled = boolPtr(false)
			case "deleted-provider":
				a.Cfg.Providers = a.Cfg.Providers[:1]
			case "empty-key":
				a.Cfg.Providers[1].APIKey = ""
			case "missing-session":
				sid = "not-present"
			case "store-error":
				_ = a.Store.Close()
			}
			text, err := a.gitSummarize(context.Background(), sid, "")
			if err == nil || text != "" {
				t.Fatalf("指定会话失效必须报错，不得生成全局摘要: text=%q err=%v", text, err)
			}
			if got := globalHits.Load(); got != 0 {
				t.Fatalf("错误路径不许外发 diff，实际请求 %d 次", got)
			}
			t.Logf("session=%s err=%v global_requests=%d", sid, err, globalHits.Load())
		})
	}
}

func TestGitSummaryUsesSessionBeforeGlobal(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.Workspace = newGitSummaryRepo(t)
	var sessionHits, globalHits atomic.Int32
	session := gitSummaryLLM(t, &sessionHits, "session-model")
	global := gitSummaryLLM(t, &globalHits, "global-model")
	a.Cfg.Providers = []ProviderCfg{
		{ID: "session", BaseURL: session.URL, APIKey: "fake", Models: []ModelCfg{{ID: "session-model"}}},
		{ID: "global", BaseURL: global.URL, APIKey: "fake", Models: []ModelCfg{{ID: "global-model"}}},
	}
	if err := a.Store.Create(&SessionMeta{ID: "chosen", Workspace: a.Cfg.Workspace, Provider: "session", Model: "session-model"}); err != nil {
		t.Fatal(err)
	}
	// 没有全局默认不能妨碍一个有效的显式会话。
	a.Cfg.DefaultModel = ModelRef{}
	text, err := a.gitSummarize(context.Background(), "chosen", "")
	if err != nil || text != "fix: synthetic summary" || sessionHits.Load() != 1 || globalHits.Load() != 0 {
		t.Fatalf("session summary=%q err=%v session=%d global=%d", text, err, sessionHits.Load(), globalHits.Load())
	}
	// 只有调用方明确不带 session 时，才使用全局默认。
	a.Cfg.DefaultModel = ModelRef{Provider: "global", Model: "global-model"}
	text, err = a.gitSummarize(context.Background(), "", "")
	if err != nil || text != "fix: synthetic summary" || sessionHits.Load() != 1 || globalHits.Load() != 1 {
		t.Fatalf("global summary=%q err=%v session=%d global=%d", text, err, sessionHits.Load(), globalHits.Load())
	}
	t.Logf("指定会话请求=%d；显式无会话的全局请求=%d", sessionHits.Load(), globalHits.Load())
}
