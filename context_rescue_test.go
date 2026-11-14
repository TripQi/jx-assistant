package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func rescueTestApp(t *testing.T, url string, window int) (*App, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.CompactInterval = 0
	a.Cfg.CompactLlmPct = 80
	a.Cfg.Providers = []ProviderCfg{{ID: "rescue", BaseURL: url, APIKey: "fake", Models: []ModelCfg{{ID: "rescue-m", ContextWindow: window}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "rescue", Model: "rescue-m"}
	s := &SessionMeta{ID: "rescue-s", Mode: "task", Workspace: t.TempDir()}
	if err := a.Store.Create(s); err != nil {
		t.Fatal(err)
	}
	return a, s
}

func TestOverflowRescueBudgetSuccessPreservesRaw(t *testing.T) {
	var hits atomic.Int32
	huge := "HEAD-constraint-" + strings.Repeat("大上下文", 100000) + "-TAIL-result"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		var req ChatRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
		}
		target := LLMTarget{Model: "rescue-m"}
		n, err := rescueRequestSize(target, req)
		if n+rescueOutputCap+rescueEnvelopeReserve > rescueWireCap {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"maximum context length exceeded"}}`)
			return
		}
		if err != nil || n+rescueOutputCap+rescueEnvelopeReserve > rescueWireCap {
			t.Errorf("request outside rescue budget: bytes=%d err=%v", n, err)
		}
		b, _ := json.Marshal(req)
		if strings.Contains(string(b), huge) {
			t.Error("rescuer resent entire overflowing history")
		}
		if req.ToolChoice != "none" || req.MaxTokens != rescueOutputCap {
			t.Errorf("missing output/tool bound: %+v", req)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\ndata: [DONE]\n\n", strings.Repeat("真实救援摘要", 20))
	}))
	defer srv.Close()
	a, s := rescueTestApp(t, srv.URL, 262144)
	if err := a.Store.AppendRow(s.ID, &StoredMessage{Message: TextMessage("user", huge), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.doSummarize(s, "manual", 0, 0); !isOverflowError(err) {
		t.Fatalf("expected real overflow response before rescue: %v", err)
	}
	hits.Store(0)
	if err := a.overflowCompact(s); err != nil {
		t.Fatalf("overflow rescue: %v", err)
	}
	raw, err := a.Store.RawMessagesAll(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	summary, audit := false, false
	for _, m := range raw {
		if m.Seq == 1 && m.Text() != huge {
			t.Fatal("raw history modified")
		}
		if m.Kind == kindSummary {
			summary = true
			if !strings.Contains(m.Text(), "有损头尾") {
				t.Error("summary hides input loss")
			}
		}
		if m.Kind == kindAudit && strings.Contains(m.Text(), "overflow救援") {
			audit = true
		}
	}
	if !summary || !audit || hits.Load() != 1 {
		t.Fatalf("summary=%v audit=%v requests=%d", summary, audit, hits.Load())
	}
	t.Logf("超限→预算裁剪→成功落库；原文%d字节保留；请求%d次", len(huge), hits.Load())
}

func TestRescueFixedMaterialsTooLargeNoRequest(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(400) }))
	defer srv.Close()
	a, s := rescueTestApp(t, srv.URL, 4096)
	original := "原文约束必须保留"
	if err := a.Store.AppendRow(s.ID, &StoredMessage{Message: TextMessage("user", original), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	_, err := a.ctxEngine().CallRescueSummarizer(s, []Message{TextMessage("system", strings.Repeat("不可裁的系统约束", 1000)), TextMessage("user", original), TextMessage("user", "摘要")}, nil)
	if err == nil || !strings.Contains(err.Error(), "原文已保留") {
		t.Fatalf("expected clear budget error: %v", err)
	}
	// 同样从真正overflow入口调用，固定系统/工具与指令也装不下时不能再发给供应商试错。
	err = a.overflowCompact(s)
	if err == nil || !strings.Contains(err.Error(), "原文已保留") || !strings.Contains(err.Error(), "更大") {
		t.Fatalf("overflow error must be actionable: %v", err)
	}
	if hits.Load() != 0 {
		t.Fatalf("must reject before network: %d", hits.Load())
	}
	raw, _ := a.Store.RawMessagesAll(s.ID)
	for _, m := range raw {
		if m.Kind == kindSummary {
			t.Fatal("failed rescue wrote summary")
		}
	}
	if raw[0].Text() != original {
		t.Fatal("original lost")
	}
	t.Logf("救援也超预算→0请求、无摘要、原文不变：%v", err)
}

func TestRunAgentOverflowRescueFailureIsActionable(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(400)
		fmt.Fprint(w, `{"error":{"message":"maximum context length exceeded"}}`)
	}))
	defer srv.Close()
	a, s := rescueTestApp(t, srv.URL, 4096)
	// 自动摘要无有效区间，因此不发模型请求；主请求被供应商拒绝后，独立救援在预检被挡。
	a.Cfg.CompactLlmPct = 100
	original := "真实主循环失败后必须保留的用户原文"
	if err := a.Store.AppendRow(s.ID, &StoredMessage{Message: TextMessage("user", original), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	err := a.RunAgent(context.Background(), s, func(Event) {})
	if err == nil || !strings.Contains(err.Error(), "原文已保留") || !strings.Contains(err.Error(), "手动") || !strings.Contains(err.Error(), "更大") {
		t.Fatalf("主循环丢失救援可行动作: %v", err)
	}
	if hits.Load() != 1 {
		t.Fatalf("仅主请求可发出，救援须本地拒绝: %d", hits.Load())
	}
	raw, err := a.Store.RawMessagesAll(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw[0].Text() != original || raw[0].Compact != "" {
		t.Fatal("主循环失败丢失原文")
	}
	for _, m := range raw {
		if m.Kind == kindSummary {
			t.Fatal("failed rescue wrote summary")
		}
	}
	t.Logf("RunAgent真实用户错误保留救援原因与动作；主请求%d次，救援0次", hits.Load())
}

func TestRescueModelRejectPreservesRawAndCanRetry(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			w.WriteHeader(400)
			fmt.Fprint(w, `{"error":{"message":"maximum context length exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\ndata: [DONE]\n\n", strings.Repeat("稍后重试完整救援摘要", 12))
	}))
	defer srv.Close()
	a, s := rescueTestApp(t, srv.URL, 262144)
	original := strings.Repeat("必须保留的原始限制与操作过程", 10000)
	if err := a.Store.AppendRow(s.ID, &StoredMessage{Message: TextMessage("user", original), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	if err := a.overflowCompact(s); err == nil || !strings.Contains(err.Error(), "原文已保留") || !strings.Contains(err.Error(), "更大") {
		t.Fatalf("actionable rescue failure required: %v", err)
	}
	raw, err := a.Store.RawMessagesAll(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range raw {
		if m.Kind == kindSummary {
			t.Fatal("failed model call must not write summary")
		}
	}
	if raw[0].Text() != original || raw[0].Compact != "" || hits.Load() != 1 {
		t.Fatal("failed rescue modified original or retried invisibly")
	}
	if err := a.overflowCompact(s); err != nil {
		t.Fatalf("failure must allow later retry: %v", err)
	}
	raw, _ = a.Store.RawMessagesAll(s.ID)
	summary := false
	for _, m := range raw {
		if m.Kind == kindSummary {
			summary = true
		}
	}
	if !summary || raw[0].Text() != original || hits.Load() != 2 {
		t.Fatal("later retry did not succeed without losing original")
	}
}

func TestRescueMultimodalAndReasoningIncludedInBudget(t *testing.T) {
	a, s := rescueTestApp(t, "mock", 262144)
	thought := strings.Repeat("思考", 100000)
	msgs := []Message{TextMessage("system", "必须遵守系统约束"), {Role: "assistant", Content: "正文", ReasoningContent: &thought}, {Role: "user", Content: []ContentPart{{Type: "text", Text: "图片如下"}, {Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64," + strings.Repeat("A", 500000)}}}}, TextMessage("user", "请摘要")}
	out, err := a.ctxEngine().CallRescueSummarizer(s, msgs, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "有损头尾") {
		t.Fatal("must disclose loss of multimodal/original reasoning")
	}
	raw, _ := a.Store.RawMessagesAll(s.ID)
	found := false
	for _, m := range raw {
		if m.Kind == kindAudit && strings.Contains(m.Text(), `"clipped":true`) {
			found = true
		}
	}
	if !found {
		t.Fatal("no rescue loss audit")
	}
}
