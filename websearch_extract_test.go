// websearch_extract.go / tools.go 的 web_fetch 单测：兜底触发判据（403/429、类型
// 不支持、JS 空壳、正文过短）、抽取链路与来源标注、正常页面不花抽取的钱、
// 全失败时的双原因，以及抽取结果的限长。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// 兜底触发判据：403/429、类型不支持、JS 空壳、正文过短都要触发；正常长正文不触发。
func TestFetchWebPageFallbackReason(t *testing.T) {
	longText := strings.Repeat("这是一段足够长的正文内容。", 60) // > 200 字
	cases := []struct {
		name       string
		status     int
		contentTyp string
		body       string
		wantReason bool
	}{
		{"403 反爬", 403, "text/html; charset=utf-8", "<html><body>forbidden</body></html>", true},
		{"429 限流", 429, "text/html", "<html></html>", true},
		{"类型不支持", 200, "image/png", "binary", true},
		{"JS 空壳", 200, "text/html", "<html><body><div id=app></div><script src=a.js></script></body></html>", true},
		{"正文过短", 200, "text/html", "<html><body><p>只有一句话</p></body></html>", true},
		{"正常长正文", 200, "text/html; charset=utf-8", "<html><body><p>" + longText + "</p></body></html>", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", c.contentTyp)
				w.WriteHeader(c.status)
				w.Write([]byte(c.body))
			}))
			defer srv.Close()
			res := fetchWebPage(context.Background(), srv.URL)
			if c.wantReason && res.reason == "" {
				t.Fatalf("应触发兜底（%s）", c.name)
			}
			if !c.wantReason && res.reason != "" {
				t.Fatalf("不该触发兜底（%s）：%s", c.name, res.reason)
			}
		})
	}
}

// 抽取链路：目标站 403 → 供应商抽取成功 → 结果标注来源；本地正常时不去花这笔钱。
func TestWebFetchExtractFallback(t *testing.T) {
	var extractHits int32
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/tools/fetch" {
			t.Errorf("DeepSeek 抽取应打 /v1/tools/fetch，实得 %s", r.URL.Path)
		}
		atomic.AddInt32(&extractHits, 1)
		w.Write([]byte(`{"title":"标题","content":"# 正文\n这是供应商抽出来的正文"}`))
	}))
	defer ext.Close()
	blocked := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(403)
		w.Write([]byte("<html><body>denied</body></html>"))
	}))
	defer blocked.Close()
	a := searchTestApp(testDeepSeek(ext.URL, "sk-test", 1))
	// 完全访问档：httptest 是回环地址，非完全访问会被 SSRF 拦截（安全语义照旧）
	tc := &toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "s", Permission: permFull}}

	out, err := runWebFetch(tc, json.RawMessage(`{"url":"`+blocked.URL+`"}`))
	if err != nil {
		t.Fatalf("403 页应改由供应商抽取成功: %v", err)
	}
	if !strings.Contains(out, "经 DeepSeek 抽取") || !strings.Contains(out, "供应商抽出来的正文") {
		t.Fatalf("应标注抽取来源并给出正文:\n%s", out)
	}
	if !strings.Contains(out, "HTTP 403") {
		t.Fatalf("首行应保留本地直抓状态码:\n%s", out)
	}
	if n := atomic.LoadInt32(&extractHits); n != 1 {
		t.Fatalf("抽取应被调用 1 次（实际 %d）", n)
	}

	// 正常页面：不触发抽取（抽取是按次计费的）
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Write([]byte("<html><body><p>" + strings.Repeat("正常正文内容。", 60) + "</p></body></html>"))
	}))
	defer ok.Close()
	atomic.StoreInt32(&extractHits, 0)
	out, err = runWebFetch(tc, json.RawMessage(`{"url":"`+ok.URL+`"}`))
	if err != nil {
		t.Fatalf("正常页面不该报错: %v", err)
	}
	if strings.Contains(out, "抽取") || atomic.LoadInt32(&extractHits) != 0 {
		t.Fatalf("正常页面不该走抽取:\n%s", out)
	}
	// 抽取也失败时：错误里既保留本地失败原因，也列出供应商失败原因
	deadExt := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(402)
	}))
	defer deadExt.Close()
	a2 := searchTestApp(testDeepSeek(deadExt.URL, "sk-test", 1))
	tc2 := &toolContext{ctx: context.Background(), app: a2, sess: &SessionMeta{ID: "s", Permission: permFull}}
	_, err = runWebFetch(tc2, json.RawMessage(`{"url":"`+blocked.URL+`"}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 403") || !strings.Contains(err.Error(), "额度或余额不足") {
		t.Fatalf("全失败时应同时给出本地与供应商原因: %v", err)
	}
}

// 抽取结果限长：几万字的正文不能整个塞进上下文。
func TestExtractFallbackClip(t *testing.T) {
	huge := strings.Repeat("很长的正文", 5000) // 25000 字
	ext := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := json.Marshal(map[string]string{"title": "t", "content": huge})
		w.Write(body)
	}))
	defer ext.Close()
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer page.Close()
	a := searchTestApp(testDeepSeek(ext.URL, "sk-test", 1))
	tc := &toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "s", Permission: permFull}}
	out, err := runWebFetch(tc, json.RawMessage(`{"url":"`+page.URL+`","max_chars":500}`))
	if err != nil {
		t.Fatalf("抽取应成功: %v", err)
	}
	if n := len([]rune(out)); n > 700 {
		t.Fatalf("正文应被 max_chars 限长，实得 %d 字", n)
	}
}
