package main

// 连接失败报错细节 + 拨号瞬断短退避重试的验收测试。
//
// 背景（2026-11-13 实测）：本机开着 TUN + fake-ip 代理栈，api.travel7.org 被
// 解析成 198.18.0.13、api.deepseek.com 成 198.18.0.6，代理内核瞬抖时 TUN 直接
// 回 RST，Go 拨号拿到 "connectex: ... actively refused it"。旧版 connErrText
// 只留一句「连接被拒绝，请确认 API 地址与端口可达」——目标 IP、端口、是否经
// 代理、耗时全丢，事后判不出是不是 fake-ip 那条路。本文件看护两件事：
//
//  1. 报错文案仍是单行（界面报错条），但人话在前、括号里带拨号目标 host:port、
//     底层错误原文、是否经代理、耗时；
//  2. 只有「拨号瞬断」（连接被拒/连接重置/强制关闭/管道断开/unexpected eof）
//     在单次连接内按 1/2/3/4/5s 的阶梯退避重试同一地址（最多 5 次重试＝6 次
//     尝试）；超时、域名解析、429 限流、鉴权失败一律不重试；ctx 取消时退避
//     立即返回，不睡满。主/备用地址次序不变：主地址跑完自己的阶梯 → 才走
//     备用地址（alt 也跑同样阶梯）。
//  3. 连接类失败不再进入整步层重试（isRetryableModelErr 返回 false）：两层
//     叠乘会把最坏等待放大到分钟级；429/5xx 仍是整步 5 次。

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
)

// connTestTarget 造一个假渠道目标（ProviderID 参与「优先外网」记忆，可传空）。
func connTestTarget(providerID, base, alt string) LLMTarget {
	return LLMTarget{ProviderID: providerID, BaseURL: base, AltBaseURL: alt, Model: "fake-m"}
}

func connTestReq() ChatRequest {
	return ChatRequest{Model: "fake-m", Messages: []Message{TextMessage("user", "hi")}, Stream: true}
}

// withStreamHTTP 临时换掉流式客户端（拨号行为由假 Transport 决定），用完还原。
func withStreamHTTP(t *testing.T, c *http.Client) {
	t.Helper()
	old := streamHTTP
	streamHTTP = c
	t.Cleanup(func() { streamHTTP = old })
}

// withConnRetryDelays 改写拨号重试探梯，免得测试白等（真值 1/2/3/4/5s）。
// 传一个值＝阶梯只有一级（只重试一次），传五个值＝完整阶梯。
func withConnRetryDelays(t *testing.T, ds ...time.Duration) {
	t.Helper()
	old := connRetryDelays
	connRetryDelays = ds
	t.Cleanup(func() { connRetryDelays = old })
}

// withNoProxy 把「是否经代理」的判定钉死为「未经代理」：开发机真配了代理环境
// 变量也不该让文案断言飘。
func withNoProxy(t *testing.T) {
	t.Helper()
	old := proxyFromEnv
	proxyFromEnv = func(*http.Request) (*neturl.URL, error) { return nil, nil }
	t.Cleanup(func() { proxyFromEnv = old })
}

// refusedTransport 返回一个「每次拨号都按 dialErr 失败」的客户端，并计数拨号次数。
func refusedTransport(t *testing.T, dialErr string) (*http.Client, *int32) {
	t.Helper()
	var dials int32
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			atomic.AddInt32(&dials, 1)
			return nil, errors.New(dialErr)
		},
	}}
	return c, &dials
}

// sseServer 起一个正常回流的假网关（收到请求计一次），用于「重试之后成功」的场景。
func sseServer(t *testing.T) (*httptest.Server, *int32) {
	t.Helper()
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv, &hits
}

// TestConnErrTextCarriesDetail 真拒连（关掉的本地端口）走一遍 StreamChat：
// 文案必须带 host:port、底层错误原文、「未经代理」与耗时，且仍是单行。
func TestConnErrTextCarriesDetail(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 10*time.Millisecond)

	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	addr := srv.URL
	srv.Close() // 关掉：留下一个必然拒连的本地端口
	hostPort := strings.TrimPrefix(addr, "http://")

	err := StreamChat(context.Background(), connTestTarget("fakep-detail", addr, ""), connTestReq(), StreamCallbacks{})
	if err == nil {
		t.Fatal("期望连接失败，却成功了")
	}
	msg := err.Error()
	t.Logf("报错文案 = %s", msg)

	for _, want := range []string{"连接被拒绝", hostPort, "refused", "未经代理", "耗时"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错文案缺少 %q：%s", want, msg)
		}
	}
	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("报错文案不是单行：%q", msg)
	}
	if n := utf8.RuneCountInString(msg); n > 300 {
		t.Errorf("报错文案 %d 字，超过界面一行能承受的约 300 字：%s", n, msg)
	}
}

// TestConnErrTextOneLineAndCapped 底层原文很长时：整条仍限长、仍单行，
// 且「是否经代理」「耗时」不会被原文挤掉（原文自己让位）。
func TestConnErrTextOneLineAndCapped(t *testing.T) {
	cf := &connFailure{
		err:       errors.New(strings.Repeat("x", 500) + "\n第二行不该出现"),
		kind:      connErrKind(errors.New("connection refused")),
		target:    "api.travel7.org:443",
		proxy:     "未经代理",
		elapsed:   3 * time.Millisecond,
		retryable: true,
	}
	msg := cf.Error()
	t.Logf("限长后的文案 = %s", msg)
	t.Logf("长度 = %d 字", utf8.RuneCountInString(msg))

	if strings.ContainsAny(msg, "\r\n") {
		t.Errorf("报错文案不是单行：%q", msg)
	}
	if n := utf8.RuneCountInString(msg); n > 300 {
		t.Errorf("报错文案 %d 字，超长", n)
	}
	for _, want := range []string{"连接被拒绝", "api.travel7.org:443", "耗时 3ms", "未经代理"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错文案缺少 %q：%s", want, msg)
		}
	}
	if !strings.HasSuffix(msg, "）") {
		t.Errorf("括号细节被截断得不完整：%s", msg)
	}
}

// TestStreamChatRetryOnceOnRefused 前一次拒连、之后成功的假服务端：
// 阶梯只一级时只重试一次（拨号 2 次、真实请求只到 1 次、正文正常收到），
// 成功后不再多试。
func TestStreamChatRetryOnceOnRefused(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 10*time.Millisecond)
	srv, hits := sseServer(t)

	var dials int32
	withStreamHTTP(t, &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if atomic.AddInt32(&dials, 1) == 1 {
				return nil, errors.New("dial tcp " + addr + ": connectex: No connection could be made because the target machine actively refused it.")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}})

	var text strings.Builder
	err := StreamChat(context.Background(), connTestTarget("fakep-retry", srv.URL, ""), connTestReq(),
		StreamCallbacks{OnText: func(s string) { text.WriteString(s) }})
	if err != nil {
		t.Fatalf("短退避后应当成功，却报错：%v", err)
	}
	if got := atomic.LoadInt32(&dials); got != 2 {
		t.Errorf("拨号次数 = %d，期望 2（首试 + 短退避重试一次；多一次就是重复重试）", got)
	}
	if got := atomic.LoadInt32(hits); got != 1 {
		t.Errorf("网关收到请求 %d 次，期望 1（拒连那次的请求没出去过）", got)
	}
	if text.String() != "ok" {
		t.Errorf("正文 = %q，期望 \"ok\"", text.String())
	}
}

// TestStreamChatNoRetryForTimeoutOrDNS 超时与域名解析不是拨号瞬断：不重试。
func TestStreamChatNoRetryForTimeoutOrDNS(t *testing.T) {
	cases := []struct {
		name    string
		dialErr string
		want    string
	}{
		{"超时", "dial tcp 198.18.0.13:443: i/o timeout", "连接超时"},
		{"域名解析", "dial tcp: lookup api.example.com: no such host", "域名解析失败"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withNoProxy(t)
			withConnRetryDelays(t, 10*time.Millisecond)
			client, dials := refusedTransport(t, c.dialErr)
			withStreamHTTP(t, client)

			err := StreamChat(context.Background(), connTestTarget("fakep-noretry", "https://api.example.com", ""), connTestReq(), StreamCallbacks{})
			if err == nil {
				t.Fatal("期望连接失败，却成功了")
			}
			if got := atomic.LoadInt32(dials); got != 1 {
				t.Errorf("%s 不该触发短退避：拨号 %d 次，期望 1", c.name, got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s 文案缺少 %q：%s", c.name, c.want, err.Error())
			}
			t.Logf("%s 文案 = %s", c.name, err.Error())
		})
	}
}

// TestStreamChatNoRetryForHTTPErrors 429 限流与 401 鉴权是业务错误，不是连接
// 失败：一次都不重试（既不走短退避，也不进 connFailure）。
func TestStreamChatNoRetryForHTTPErrors(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"429 限流", http.StatusTooManyRequests, `{"error":{"message":"rate limit, too many requests"}}`, "模型服务返回 429"},
		{"401 鉴权", http.StatusUnauthorized, `{"error":{"message":"invalid api key"}}`, "模型服务返回 401"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			withNoProxy(t)
			withConnRetryDelays(t, 10*time.Millisecond)
			var hits int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				atomic.AddInt32(&hits, 1)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(srv.Close)

			err := StreamChat(context.Background(), connTestTarget("fakep-status", srv.URL, ""), connTestReq(), StreamCallbacks{})
			if err == nil {
				t.Fatal("期望失败，却成功了")
			}
			if got := atomic.LoadInt32(&hits); got != 1 {
				t.Errorf("%s 不该重试：网关收到 %d 次请求，期望 1", c.name, got)
			}
			if isConnFailure(err) {
				t.Errorf("%s 被误判成连接故障：%v", c.name, err)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s 文案缺少 %q：%s", c.name, c.want, err.Error())
			}
		})
	}
}

// TestConnRetryBackoffHonorsCancel 退避期间 ctx 取消（用户点停止/插话打断）：
// 立即返回，绝不睡满，也不再拨第二次。
func TestConnRetryBackoffHonorsCancel(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 2*time.Second) // 真睡满会明显被抓出来
	client, dials := refusedTransport(t, "connect: connection refused")
	withStreamHTTP(t, client)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	err := StreamChat(ctx, connTestTarget("fakep-cancel", "http://127.0.0.1:1", ""), connTestReq(), StreamCallbacks{})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("期望连接失败，却成功了")
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("退避没被 ctx 取消打断：耗时 %v（退避设成了 2s）", elapsed)
	}
	if got := atomic.LoadInt32(dials); got != 1 {
		t.Errorf("取消后不该再拨号：拨号 %d 次，期望 1", got)
	}
	t.Logf("ctx 取消后 %v 返回，文案 = %s", elapsed.Round(time.Millisecond), err.Error())
}

// TestConnRetryOrderPrimaryThenAlt 次序不变：主地址先把整条阶梯跑完
// （首试 + 1/2/3/4/5s 共 6 次尝试）→ 仍失败才改走备用地址（alt 自己的首试，
// 成功即止、不多试）。
func TestConnRetryOrderPrimaryThenAlt(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)
	const providerID = "fakep-altorder"
	t.Cleanup(func() { altPreferred.Delete(providerID) })

	alt, altHits := sseServer(t)
	var primaryDials int32
	withStreamHTTP(t, &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if addr == "127.0.0.1:1" { // 主地址：恒拒连
				atomic.AddInt32(&primaryDials, 1)
				return nil, errors.New("dial tcp " + addr + ": connectex: No connection could be made because the target machine actively refused it.")
			}
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}})

	err := StreamChat(context.Background(), connTestTarget(providerID, "http://127.0.0.1:1", alt.URL), connTestReq(), StreamCallbacks{})
	if err != nil {
		t.Fatalf("备用地址应当成功，却报错：%v", err)
	}
	if got := atomic.LoadInt32(&primaryDials); got != 6 {
		t.Errorf("主地址拨号 %d 次，期望 6（首试 + 阶梯 5 次重试后才切备用）", got)
	}
	if got := atomic.LoadInt32(altHits); got != 1 {
		t.Errorf("备用地址收到 %d 次请求，期望 1", got)
	}
}

// TestConnRetryLadderExhausted：恒拒连时阶梯跑满——拨号 6 次（首试 + 5 次
// 重试），最后仍报连接失败；多一次就是重试次数失控。
func TestConnRetryLadderExhausted(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)
	client, dials := refusedTransport(t, "connect: connection refused")
	withStreamHTTP(t, client)

	err := StreamChat(context.Background(), connTestTarget("fakep-ladder", "http://127.0.0.1:1", ""), connTestReq(), StreamCallbacks{})
	if err == nil {
		t.Fatal("期望连接失败，却成功了")
	}
	if got := atomic.LoadInt32(dials); got != 6 {
		t.Errorf("拨号 %d 次，期望 6（首试 + 1/2/3/4/5s 五次重试）", got)
	}
	if !isConnFailure(err) {
		t.Errorf("仍应是 connFailure（不变成业务错误）：%v", err)
	}
	if !strings.Contains(err.Error(), "连接被拒绝") {
		t.Errorf("文案缺少拒绝说明：%s", err.Error())
	}
}

// TestConnRetryLadderAltAlsoRetries：主、备都拒连——两边各跑一遍阶梯
// （6 + 6 次拨号），报错取备用地址那次；说明阶梯不只在主地址上生效。
func TestConnRetryLadderAltAlsoRetries(t *testing.T) {
	withNoProxy(t)
	withConnRetryDelays(t, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond, 5*time.Millisecond)
	client, dials := refusedTransport(t, "connect: connection refused")
	withStreamHTTP(t, client)

	err := StreamChat(context.Background(), connTestTarget("fakep-altladder", "http://127.0.0.1:1", "http://127.0.0.1:2"), connTestReq(), StreamCallbacks{})
	if err == nil {
		t.Fatal("期望连接失败，却成功了")
	}
	if got := atomic.LoadInt32(dials); got != 12 {
		t.Errorf("拨号 %d 次，期望 12（主 6 + 备 6，两边各跑一遍阶梯）", got)
	}
}

// TestConnRetryDelaysLadder 真值固定：1/2/3/4/5s 五级（不睡，只查常量），
// 以及「首试 + 5 次重试 = 6 次尝试、最坏约 15 秒」这个口径。
func TestConnRetryDelaysLadder(t *testing.T) {
	want := []time.Duration{1 * time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second}
	if len(connRetryDelays) != len(want) {
		t.Fatalf("阶梯 %v，期望 5 级 %v", connRetryDelays, want)
	}
	var sum time.Duration
	for i, w := range want {
		if connRetryDelays[i] != w {
			t.Errorf("第 %d 级 = %v，期望 %v", i+1, connRetryDelays[i], w)
		}
		sum += w
	}
	if sum != 15*time.Second {
		t.Errorf("阶梯总长 = %v，期望 15s（最坏等待上限）", sum)
	}
}

// TestIsRetryableModelErrConnNotRetried 整步层判据：连接类失败不再重试（拨号层
// 已把 6 次尝试做完），429/5xx/流中断仍可重试，请求级错误照旧不重试。
func TestIsRetryableModelErrConnNotRetried(t *testing.T) {
	refused := &connFailure{err: errors.New("dial tcp 198.18.0.13:443: connectex: No connection could be made because the target machine actively refused it."),
		kind: "连接被拒绝，请确认 API 地址与端口可达", target: "198.18.0.13:443", proxy: "未经代理", elapsed: 3 * time.Millisecond, retryable: true}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"连接被拒绝", refused, false},
		{"连接超时（也是连接类）", &connFailure{err: errors.New("dial tcp: i/o timeout"), kind: "连接超时，模型服务没有在预期时间内响应", retryable: false}, false},
		{"请求已取消形态的连接失败", errors.New("无法连接模型服务：请求已取消（耗时 0ms）"), false},
		{"429 限流", errors.New("模型服务返回 429：rate limit, too many requests"), true},
		{"500 网关错误", errors.New("模型服务返回 500：internal error"), true},
		{"流中途断开", errors.New("unexpected EOF"), true},
		{"401 鉴权", errors.New("模型服务返回 401：invalid api key"), false},
		{"nil", nil, false},
	}
	for _, c := range cases {
		if got := isRetryableModelErr(c.err); got != c.want {
			t.Errorf("%s：isRetryableModelErr = %v，期望 %v", c.name, got, c.want)
		}
	}
}
