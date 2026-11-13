package main

// 模型自动重试计时的隔离验收测试（用户反馈疑似：重试间隔没有 1s/2s/3s/4s
// 递增，好像每次只隔 1 秒、总等待仅 5 秒）。走真实 RunAgent → StreamChat
// 路径记录每次尝试的实际时刻，两个场景各自钉住一层：
//
//   - 429 限流：业务错误，走整步层 5 次 × 1s/2s/3s/4s（累计约 10s），拨号层
//     不插手（HTTP 状态码错误压根不是 connFailure）；
//   - 拒连（断网形态）：连接类失败**不进整步层**，由拨号层按
//     connRetryDelays（1/2/3/4/5s）跑 6 次尝试、最坏约 15 秒——两层叠乘会把
//     等待放大到分钟级，所以这里同时断言「整步层一次都没重试」。
//
// 计时起点是「上一次失败返回」，不是「第一次失败」。

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// runRetryTimingApp 搭好假渠道 App（全局默认模型指向它）并跑一轮 RunAgent，
// 返回这一轮的报错、auto_retry 提示出现的时刻与文案。
func runRetryTimingApp(t *testing.T, baseURL string) (runErr error, notices []time.Time, texts []string) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: baseURL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-m"}}}}
	// 默认模型体系：会话没手选模型时跟全局默认模型（空着会报「还没有设置
	// 全局默认模型」，与重试无关的失败；渠道 Key 也必须非空）
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	sess := &SessionMeta{ID: "retrytiming", Title: "重试计时", Mode: "task", Workspace: a.Cfg.Workspace,
		CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	var mu sync.Mutex
	runErr = a.RunAgent(context.Background(), sess, func(ev Event) {
		if ev.Type == "auto_retry" {
			mu.Lock()
			notices = append(notices, time.Now())
			texts = append(texts, ev.Output)
			mu.Unlock()
		}
	})
	t.Logf("RunAgent 返回: %v", runErr)
	return runErr, notices, texts
}

// assertLadder 校验相邻尝试时刻依次间隔 want[0]、want[1]…（容差 400ms），
// 并打印总跨度。
func assertLadder(t *testing.T, label string, ts []time.Time, want []time.Duration) {
	t.Helper()
	if len(ts) != len(want)+1 {
		t.Fatalf("%s：尝试 %d 次，期望 %d 次（首试 + 重试 %d 次）", label, len(ts), len(want)+1, len(want))
	}
	for i := 1; i < len(ts); i++ {
		got := ts[i].Sub(ts[i-1])
		if got < want[i-1]-400*time.Millisecond || got > want[i-1]+400*time.Millisecond {
			t.Errorf("%s：第 %d→%d 次尝试间隔 = %v，期望约 %v", label, i, i+1, got.Round(time.Millisecond), want[i-1])
		}
		t.Logf("%s：第 %d→%d 次尝试间隔 = %v（期望约 %v）", label, i, i+1, got.Round(time.Millisecond), want[i-1])
	}
	total := ts[len(ts)-1].Sub(ts[0]).Round(time.Millisecond)
	t.Logf("%s：首次到末次尝试总跨度 = %v", label, total)
}

// TestRetryTiming429 恒 429 限流：整步层可重试的主场景，5 次 × 1/2/3/4s。
func TestRetryTiming429(t *testing.T) {
	var mu sync.Mutex
	var hits []time.Time
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits = append(hits, time.Now())
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit, too many requests"}}`))
	}))
	t.Cleanup(srv.Close)

	runErr, notices, texts := runRetryTimingApp(t, srv.URL)
	if runErr == nil {
		t.Fatal("恒 429 应当以失败收场")
	}
	for _, tx := range texts {
		if tx == "" {
			continue // 重试耗尽/成功时发空 auto_retry 收掉提示条，不是文案
		}
		if !strings.Contains(tx, "秒后自动重试") {
			t.Errorf("auto_retry 提示不含重试文案: %q", tx)
		}
		t.Logf("提示条: %s", tx)
	}
	want := []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second}
	mu.Lock()
	assertLadder(t, "429 服务器侧", hits, want)
	mu.Unlock()
	assertLadder(t, "429 事件侧", notices, want)
}

// TestRetryTimingConnRefused 拒连（断网形态）：拨号层按 1/2/3/4/5s 阶梯跑满
// 6 次尝试（约 15 秒），整步层一次都不重试（不再叠乘）。
func TestRetryTimingConnRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	srv.Close() // 立即关掉：留下一个必然拒连的本地端口

	var mu sync.Mutex
	var dials []time.Time
	withStreamHTTP(t, &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			mu.Lock()
			dials = append(dials, time.Now())
			mu.Unlock()
			var d net.Dialer
			return d.DialContext(ctx, network, addr) // 真拨号：对已关闭端口必然拒连
		},
	}})

	runErr, notices, _ := runRetryTimingApp(t, srv.URL)
	if runErr == nil {
		t.Fatal("拒连应当以失败收场")
	}
	if !isConnFailure(runErr) {
		t.Errorf("报错应当是连接类失败（connFailure）：%v", runErr)
	}
	if len(notices) != 0 {
		t.Errorf("连接类失败不该进整步层重试：出现 %d 次 auto_retry 提示（拨号层已把阶梯跑完）", len(notices))
	}
	mu.Lock()
	assertLadder(t, "拒连 拨号层", dials, connRetryDelays)
	mu.Unlock()
	t.Logf("报错文案 = %s", runErr.Error())
}
