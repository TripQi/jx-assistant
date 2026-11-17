package main

// 注册表锁的针对性回归：摘除/替换连接器不得被进行中的握手套牢。
//
// 原缺陷：removeClientLocked 在 r.mu 内调用 c.shutdown()，而 shutdown 要拿 c.mu
// ——start 在整段握手期间持有 c.mu，stdio 的读与 HTTP 请求又各有 120s 上限且
// 不看外层 ctx，于是一次卡住的握手就会把注册表锁占满，环境快照（PromptBlock）、
// 设置页列表（List）、按来源移除/同名替换（RemoveBySource / Upsert）全部跟着卡。
//
// 这里不依赖某台机器上有没有 python、也不依赖外网 DNS，用可控的子进程与本地
// HTTP 服务器把「握手卡住」做成确定性现场。
//
// TestMCPStdioHungHelper 只在被当作连接器子进程拉起时干活：握手永不返回。

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestMCPStdioHungHelper(t *testing.T) {
	if os.Getenv("JX_MCP_HUNG_HELPER") != "1" {
		t.Skip("仅作 MCP 握手卡住的子进程用")
	}
	// 在连接器工作目录里留下自己的 pid：父用例据此确认「子进程已拉起」（即握手
	// 已经开始并持有 c.mu——该锁内根本读不到 c.cmd），并可在摘除后核对进程已退出，
	// 不必用固定 sleep 猜时序。
	_ = os.WriteFile("handshake-ready", []byte(strconv.Itoa(os.Getpid())), 0644)
	time.Sleep(60 * time.Second) // 父进程摘除连接器时会杀掉本进程
}

// hungStdioDef 是「握手永不返回」的 stdio 连接器定义（子进程即本测试二进制）。
func hungStdioDef() map[string]mcpServerDef {
	return map[string]mcpServerDef{"hung": {
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioHungHelper"},
		Env:     map[string]string{"JX_MCP_HUNG_HELPER": "1"},
	}}
}

// hungMarker 读子进程留下的 pid 标记（0 = 还没写出来）。
func hungMarker(r *MCPServersRegistry) int {
	data, err := os.ReadFile(filepath.Join(r.Root(), "handshake-ready"))
	if err != nil {
		return 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		return 0
	}
	return pid
}

// waitMCP 有界轮询一个条件，避免用固定 sleep 猜时序。
func waitMCP(t *testing.T, limit time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("等待超时：%s", what)
}

func mcpStatusOf(list []mcpServerStatus, name string) mcpServerStatus {
	for _, st := range list {
		if st.Name == name {
			return st
		}
	}
	return mcpServerStatus{}
}

// 摘除正在握手的 stdio 连接器必须立即返回：stdio 的读只有随握手被中断才可能
// 退出（进程杀灭之外没有别的路径），所以这条同时验证取消打到了实际 I/O。
func TestMCPRemoveNotBlockedByHungStdioHandshake(t *testing.T) {
	r := newTestMCPR(t)
	if _, err := r.UpsertFromDefs(hungStdioDef(), "market:hung", "握手不返回的连接器"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 等到握手确实在进行：子进程已拉起即意味着 start 已持 c.mu 并卡在读上
	// （c.cmd 在 c.mu 内写入，这里不能拿 c.mu，否则轮询本身就阻塞）。
	waitMCP(t, 10*time.Second, func() bool { return hungMarker(r) != 0 }, "握手没有进入进行中状态")

	done := make(chan []string, 1)
	go func() { done <- r.RemoveBySource("market:hung") }()
	select {
	case removed := <-done:
		if len(removed) != 1 || removed[0] != "hung" {
			t.Fatalf("按来源移除应删 hung: %v", removed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("按来源移除被进行中的 stdio 握手套牢（注册表锁内等 c.mu）")
	}
	if r.Count() != 0 || r.HasSource("market:hung") {
		t.Fatalf("移除后应清空: count=%d", r.Count())
	}
	if got := r.PromptBlock(nil); got != "" {
		t.Fatalf("移除后环境快照不该再列该连接器: %q", got)
	}
}

// 同名替换正在握手的 HTTP 连接器必须立即返回，且旧握手的 HTTP 请求要被真正
// 中断（服务端观测到请求 ctx 结束），不是只取消外层 ctx 放任 I/O。
func TestMCPReplaceNotBlockedByHungHTTPHandshake(t *testing.T) {
	r := newTestMCPR(t)
	var hits int32
	var once sync.Once
	interrupted := make(chan struct{})
	gate := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		atomic.AddInt32(&hits, 1)
		// 先把请求体读尽：否则服务器不会起后台读，客户端断开也就观测不到
		// req.Context() 结束。
		_, _ = io.Copy(io.Discard, req.Body)
		select {
		case <-gate:
		case <-req.Context().Done():
			once.Do(func() { close(interrupted) })
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[]}}`))
	}))
	t.Cleanup(func() {
		select {
		case <-gate:
		default:
			close(gate)
		}
		srv.Close()
	})
	if err := r.Upsert(mcpServerEntry{Name: "swap", Enabled: true, Type: "http", URL: srv.URL}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	waitMCP(t, 10*time.Second, func() bool { return atomic.LoadInt32(&hits) > 0 }, "握手没有打到本地服务器")

	done := make(chan error, 1)
	go func() {
		// 同名替换：旧定义的握手此时仍卡在服务器上
		done <- r.Upsert(mcpServerEntry{Name: "swap", Enabled: true, Type: "http", URL: "http://127.0.0.1:1/mcp"})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("同名替换失败: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("同名替换被进行中的 HTTP 握手套牢（注册表锁内等 c.mu）")
	}
	select {
	case <-interrupted:
	case <-time.After(2 * time.Second):
		t.Fatal("旧握手的 HTTP 请求没有被中断（取消只到外层 ctx，没到实际 I/O）")
	}
	if r.Count() != 1 {
		t.Fatalf("替换后仍应只有一条定义: %d", r.Count())
	}
}

// 迟到的握手结果必须按实例身份丢弃：名字还在不代表还是同一个连接实例。
func TestMCPStaleConnectResultIgnored(t *testing.T) {
	r := newTestMCPR(t)
	if err := r.addQuiet(mcpServerEntry{Name: "swap", Enabled: true, Command: "cmd.exe"}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	r.mu.Lock()
	e, ok := r.entryOf("swap")
	var cur *mcpClient
	if ok {
		cur = r.clientOf(e)
	}
	r.mu.Unlock()
	if cur == nil {
		t.Fatal("未能取到当前连接实例")
	}

	stale := &mcpClient{fullName: "swap"}
	if r.noteConnectResult("swap", stale, errors.New("旧实例握手失败")) {
		t.Fatal("被替换掉的实例的迟到结果不该被采纳")
	}
	if st := mcpStatusOf(r.List(), "swap"); st.LastError != "" {
		t.Fatalf("迟到结果污染了当前实例的 lastErr: %q", st.LastError)
	}

	if !r.noteConnectResult("swap", cur, errors.New("当前实例握手失败")) {
		t.Fatal("当前实例的结果应被采纳")
	}
	if st := mcpStatusOf(r.List(), "swap"); !strings.Contains(st.LastError, "当前实例握手失败") {
		t.Fatalf("当前实例的失败应可见: %+v", st)
	}

	if err := r.Remove("swap"); err != nil {
		t.Fatalf("移除失败: %v", err)
	}
	if r.noteConnectResult("swap", cur, errors.New("摘除之后迟到")) {
		t.Fatal("已摘除连接器的迟到结果不该被采纳")
	}
	r.mu.Lock()
	_, ghost := r.lastErr["swap"]
	r.mu.Unlock()
	if ghost {
		t.Fatal("已摘除的连接器不得留下幽灵 lastErr")
	}
}

// 环境组装与设置页列表不能挂在握手的 deadline 上：握手进行中（c.mu 被对手
// 握全程持有）也必须立即返回真实状态——未连接、无工具数，而不是等握手结束。
// 修前实测阻塞 ≈ 握手方 ctx（connectAsync 30s，懒启动最长 120s）。
func TestMCPPromptBlockAndListNotBlockedByHandshake(t *testing.T) {
	r := newTestMCPR(t)
	if _, err := r.UpsertFromDefs(hungStdioDef(), "market:hung", "握手不返回的连接器"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	waitMCP(t, 10*time.Second, func() bool { return hungMarker(r) != 0 }, "握手没有进入进行中状态")

	pb := make(chan string, 1)
	go func() { pb <- r.PromptBlock(nil) }()
	select {
	case got := <-pb:
		if !strings.Contains(got, "hung") {
			t.Fatalf("连接器清单应列出尚未连上的连接器: %q", got)
		}
		if strings.Contains(got, "个工具") {
			t.Fatalf("握手未完成却报出工具数（不许伪装成已连接）: %q", got)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("PromptBlock 被进行中的握手挡住：环境组装不能依赖握手长锁")
	}

	ls := make(chan []mcpServerStatus, 1)
	go func() { ls <- r.List() }()
	select {
	case list := <-ls:
		if st := mcpStatusOf(list, "hung"); st.Connected || st.ToolCount != 0 || len(st.Tools) != 0 {
			t.Fatalf("握手未完成的状态应如实报未连接: %+v", st)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("List 被进行中的握手挡住：设置页列表不能依赖握手长锁")
	}
	if removed := r.RemoveBySource("market:hung"); len(removed) != 1 {
		t.Fatalf("摘除失败: %v", removed)
	}
}

// 快照的另一半：真连上之后 PromptBlock/List 必须如实报出工具数与清单
// （不许因为改读快照而永远显示"未连接"）。
func TestMCPPromptBlockAndListShowConnectedState(t *testing.T) {
	r := newTestMCPR(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var frame struct {
			ID int64 `json:"id"`
		}
		_ = json.Unmarshal(body, &frame)
		w.Header().Set("Content-Type", "application/json")
		if frame.ID == 0 { // notification：无响应体
			w.WriteHeader(http.StatusAccepted)
			return
		}
		fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%d,"result":{"tools":[{"name":"ping","description":"p"}]}}`, frame.ID)
	}))
	t.Cleanup(srv.Close)
	if err := r.Upsert(mcpServerEntry{Name: "live", Enabled: true, Type: "http", URL: srv.URL}); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	waitMCP(t, 10*time.Second, func() bool { return mcpStatusOf(r.List(), "live").Connected }, "连接器没有连上")

	st := mcpStatusOf(r.List(), "live")
	if st.ToolCount != 1 || len(st.Tools) != 1 || st.Tools[0] != "ping" {
		t.Fatalf("已连接状态报错: %+v", st)
	}
	if got := r.PromptBlock(nil); !strings.Contains(got, "1 个工具") {
		t.Fatalf("环境快照没有如实报出工具数: %q", got)
	}
}

// 反复「起握手 → 摘除」不得留下常驻读行/连接 goroutine：本次引入 readLoop
// 后必须有正式回归守着，而不是只在开发探针里看过。
func TestMCPHandshakeGoroutinesReclaimed(t *testing.T) {
	r := newTestMCPR(t)
	base := runtime.NumGoroutine()
	for i := 0; i < 3; i++ {
		_ = os.Remove(filepath.Join(r.Root(), "handshake-ready"))
		if _, err := r.UpsertFromDefs(hungStdioDef(), "market:hung", "握手不返回的连接器"); err != nil {
			t.Fatalf("第 %d 轮注册失败: %v", i+1, err)
		}
		waitMCP(t, 10*time.Second, func() bool { return hungMarker(r) != 0 }, "握手没有进入进行中状态")
		if removed := r.RemoveBySource("market:hung"); len(removed) != 1 {
			t.Fatalf("第 %d 轮摘除失败: %v", i+1, removed)
		}
		_ = os.Remove(filepath.Join(r.Root(), "handshake-ready"))
	}
	limit := base + 2 // 允许运行时/调度器自身的少量抖动
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && runtime.NumGoroutine() > limit {
		time.Sleep(20 * time.Millisecond)
	}
	if now := runtime.NumGoroutine(); now > limit {
		t.Fatalf("反复摘除后 goroutine 未回收（可能有常驻读行循环）: base=%d now=%d", base, now)
	}
	if r.Count() != 0 {
		t.Fatalf("注册表应清空: %d", r.Count())
	}
}

// TestMCPStdioEchoHelper 是可应答的 stdio MCP 子进程：握手与工具调用都能正常完成，
// 工具结果回显调用时给的工具名，便于核对响应归属。
func TestMCPStdioEchoHelper(t *testing.T) {
	if os.Getenv("JX_MCP_ECHO_HELPER") != "1" {
		t.Skip("仅作 MCP stdio 子进程用")
	}
	sc := bufio.NewScanner(os.Stdin)
	sc.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for sc.Scan() {
		var frame struct {
			ID     int64  `json:"id"`
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(sc.Bytes(), &frame); err != nil || frame.ID == 0 {
			continue
		}
		res := "{}"
		switch frame.Method {
		case "tools/list":
			res = `{"tools":[{"name":"ping","description":"p"}]}`
		case "tools/call":
			res = fmt.Sprintf(`{"content":[{"type":"text","text":%q}]}`, frame.Params.Name)
		}
		fmt.Fprintf(os.Stdout, `{"jsonrpc":"2.0","id":%d,"result":%s}`+"\n", frame.ID, res)
	}
}

// echoMCPClient 起一个应答正常的 stdio 连接（工具结果回显工具名）。
func echoMCPClient(t *testing.T) *mcpClient {
	t.Helper()
	c := &mcpClient{fullName: "echo", def: mcpServerDef{
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioEchoHelper"},
		Env:     map[string]string{"JX_MCP_ECHO_HELPER": "1"},
	}, host: mcpPlainHost{dir: t.TempDir()}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := c.start(ctx); err != nil {
		t.Fatalf("echo 连接器启动失败: %v", err)
	}
	t.Cleanup(c.shutdown)
	return c
}

// 并发调用与关闭相邻时的保护（trMu 的定向验证）：不许写空管道 panic、不许数据
// 竞争（-race 下验）、不许无界阻塞；关闭后再发请求必须报错。
//
// 注意：stdio 连接上的**并发工具调用本身**不在本用例范围——响应只按 id 过滤、
// 由先读到的一方拿走，串不串属既有设计（改成按 id 分发是架构改动）。这里只断言
// 并发不崩、不死等，且喂完之后串行调用仍然路由正确。
func TestMCPConcurrentCallsAndRpcAfterShutdown(t *testing.T) {
	c := echoMCPClient(t)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		name := fmt.Sprintf("tool-%d", i)
		wg.Add(1)
		go func(name string) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// 成功或报超时都算正常收束；panic / 死等 / 数据竞争才算问题。
			_, _, _ = c.callTool(ctx, name, nil)
		}(name)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("并发工具调用没有在 20 秒内收束")
	}
	// 并发风波过后，串行调用仍必须拿到自己的结果（没串台 / 没卡死）
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	text, _, err := c.callTool(ctx, "after", nil)
	if err != nil {
		t.Fatalf("并发后的串行调用失败: %v", err)
	}
	if text != "after" {
		t.Fatalf("并发后的串行调用串台: %q", text)
	}
	// 关闭后再发请求：必须是错误，而不是写空管道
	c.shutdown()
	if _, err := c.rpc(context.Background(), "tools/list", map[string]any{}); err == nil {
		t.Fatal("连接已关闭时 rpc 应返回错误（不许写空管道）")
	}
}
