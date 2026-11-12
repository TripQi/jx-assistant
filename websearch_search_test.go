// websearch_search.go / websearch.go 的单测：顺位回落（成功/失败/空结果不回落）、
// 全部失败的原因汇总与改道指引、总耗时预算、凭据类响应体不外传、无供应商时的老行为。
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
	"time"
)

func testDeepSeek(base, key string, order int) SearchProviderCfg {
	return SearchProviderCfg{
		ID: "ds" + fmt.Sprint(order), Name: "DeepSeek", Kind: spKindDeepSeek, BaseURL: base, APIKey: key,
		Capabilities: SearchProviderCaps{Search: true, Extract: true}, Order: order,
	}
}

func runSearchOnce(t *testing.T, a *App, query string) (string, error) {
	t.Helper()
	return a.runWebSearch(&toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "sess-search"}},
		json.RawMessage(`{"query":"`+query+`"}`))
}

// 顺位回落：第 1 家成功就用它；第 1 家失败才换第 2 家；**空结果不回落**。
func TestWebSearchFallbackOrder(t *testing.T) {
	t.Run("第1家成功不试第2家", func(t *testing.T) {
		var secondHits int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"results":[{"title":"一","url":"https://a.com/1","snippet":"摘要一"}]}`))
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&secondHits, 1)
			w.Write([]byte(`{"results":[{"title":"二","url":"https://b.com/2"}]}`))
		}))
		defer second.Close()
		a := searchTestApp(testDeepSeek(first.URL, "sk-test", 1), testDeepSeek(second.URL, "sk-test", 2))
		out, err := runSearchOnce(t, a, "顺位")
		if err != nil {
			t.Fatalf("第 1 家成功不该报错: %v", err)
		}
		if !strings.Contains(out, "第1位") || !strings.Contains(out, "一") {
			t.Fatalf("应标明第 1 位并给出第 1 家结果:\n%s", out)
		}
		if n := atomic.LoadInt32(&secondHits); n != 0 {
			t.Fatalf("第 1 家已成功，第 2 家不该被调用（实际 %d 次）", n)
		}
	})

	t.Run("第1家失败回落第2家", func(t *testing.T) {
		var secondHits int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(500)
			w.Write([]byte(`{"error":{"message":"boom"}}`))
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&secondHits, 1)
			w.Write([]byte(`{"results":[{"title":"二","url":"https://b.com/2","snippet":"s"}]}`))
		}))
		defer second.Close()
		a := searchTestApp(testDeepSeek(first.URL, "sk-test", 1), testDeepSeek(second.URL, "sk-test", 2))
		out, err := runSearchOnce(t, a, "回落")
		if err != nil {
			t.Fatalf("第 2 家成功应返回结果: %v", err)
		}
		if !strings.Contains(out, "第2位") || !strings.Contains(out, "https://b.com/2") {
			t.Fatalf("应标明回落到了第 2 位:\n%s", out)
		}
		if n := atomic.LoadInt32(&secondHits); n != 1 {
			t.Fatalf("第 2 家应被调用 1 次（实际 %d）", n)
		}
	})

	t.Run("空结果不回落", func(t *testing.T) {
		var secondHits int32
		first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Write([]byte(`{"results":[]}`))
		}))
		defer first.Close()
		second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			atomic.AddInt32(&secondHits, 1)
			w.Write([]byte(`{"results":[{"title":"二","url":"https://b.com/2"}]}`))
		}))
		defer second.Close()
		a := searchTestApp(testDeepSeek(first.URL, "sk-test", 1), testDeepSeek(second.URL, "sk-test", 2))
		out, err := runSearchOnce(t, a, "冷门词")
		if err != nil {
			t.Fatalf("0 条结果是有效答案，不该报错: %v", err)
		}
		if !strings.Contains(out, "0 条结果") {
			t.Fatalf("应如实报 0 条结果:\n%s", out)
		}
		if n := atomic.LoadInt32(&secondHits); n != 0 {
			t.Fatalf("空结果不该触发回落（第 2 家被调用 %d 次）", n)
		}
	})
}

// 全部失败：逐家原因都列出来，并附加「本机网络搜索」改道指引。
func TestWebSearchAllProvidersFailedHint(t *testing.T) {
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(401)
	}))
	defer bad.Close()
	a := mkSkillTestApp(t, true) // 带内置「本机网络搜索」技能 → 指引应附加
	a.Cfg.SearchProviders = []SearchProviderCfg{testDeepSeek(bad.URL, "sk-bad", 1)}
	_, err := runSearchOnce(t, a, "全挂")
	if err == nil {
		t.Fatal("全部失败应报错")
	}
	for _, want := range []string{"Key 无效或无权限", "第1位", "本机网络搜索"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("错误应含 %q: %v", want, err)
		}
	}
}

// 总耗时预算：是预算不是各家超时相加——预算用完后不再试下一家。
func TestWebSearchTotalBudget(t *testing.T) {
	var secondHits int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(500 * time.Millisecond)
		w.Write([]byte(`{"results":[{"title":"慢","url":"https://a.com/1"}]}`))
	}))
	defer slow.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&secondHits, 1)
		w.Write([]byte(`{"results":[{"title":"二","url":"https://b.com/2"}]}`))
	}))
	defer second.Close()
	a := searchTestApp(testDeepSeek(slow.URL, "sk-test", 1), testDeepSeek(second.URL, "sk-test", 2))
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond) // 模拟总预算到点
	defer cancel()
	started := time.Now()
	_, err := a.runWebSearch(&toolContext{ctx: ctx, app: a, sess: &SessionMeta{ID: "s"}},
		json.RawMessage(`{"query":"预算"}`))
	if err == nil {
		t.Fatal("预算用尽应报错")
	}
	if !strings.Contains(err.Error(), "总预算") {
		t.Fatalf("应说明是总耗时预算用完: %v", err)
	}
	if d := time.Since(started); d > 3*time.Second {
		t.Fatalf("不该把每家超时叠加（耗时 %s）", d)
	}
	if n := atomic.LoadInt32(&secondHits); n != 0 {
		t.Fatalf("预算用尽后不该再试下一家（第 2 家被调用 %d 次）", n)
	}
}

// 凭据类错误绝不转述响应体（AnySearch 的 402 会回账号密码与 api_key）。
func TestProviderErrMessageNoCredentialLeak(t *testing.T) {
	leaky := []byte(`{"code":402,"message":"quota exhausted","data":{"username":"u","password":"p","api_key":"as_sk_secret"}}`)
	if msg := providerErrMessage(spKindAnySearch, 402, leaky); msg != "" {
		t.Fatalf("402 不该转述响应体: %q", msg)
	}
	if msg := providerErrMessage(spKindTavily, 401, leaky); msg != "" {
		t.Fatalf("401 不该转述响应体: %q", msg)
	}
	// 非凭据类（500/422）才转述一句人能读的原因
	if msg := providerErrMessage(spKindTavily, 500, []byte(`{"detail":{"error":"Internal server error"}}`)); !strings.Contains(msg, "Internal") {
		t.Fatalf("500 应转述原因，实得 %q", msg)
	}
}

// 无供应商时的老行为：配置里一条都没有 + 服务端地址也没配 → 报「网络访问」并指引本机兜底。
func TestWebSearchLegacyWhenNoProviders(t *testing.T) {
	a := mkSkillTestApp(t, true) // Cfg 里没有任何 searchProviders
	_, err := runSearchOnce(t, a, "老行为")
	if err == nil || !strings.Contains(err.Error(), "网络访问") {
		t.Fatalf("应报未配置服务端地址: %v", err)
	}
	if !strings.Contains(err.Error(), "本机网络搜索") {
		t.Fatalf("技能可用时应附加改道指引: %v", err)
	}
	// 配了 searchURL（老配置）但没有 searchProviders：仍走网关老链路
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/search" {
			t.Errorf("老链路应打 /api/search，实得 %s", r.URL.Path)
		}
		w.Write([]byte(`{"results":[{"title":"网关结果","url":"https://a.com/1","snippet":"s"}],"engine":"zhipu-mcp"}`))
	}))
	defer srv.Close()
	a.Cfg.SearchURL = srv.URL
	out, err := runSearchOnce(t, a, "老链路")
	if err != nil {
		t.Fatalf("老链路应能出结果: %v", err)
	}
	if !strings.Contains(out, "网关结果") || !strings.Contains(out, "zhipu-mcp") {
		t.Fatalf("老链路结果/引擎名不对:\n%s", out)
	}
}
