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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	// 在连接器工作目录里留个标记：父用例据此确认「子进程已拉起」（即握手已经
	// 开始并持有 c.mu——该锁内根本读不到 c.cmd），不必用固定 sleep 猜时序。
	_ = os.WriteFile("handshake-ready", []byte("1"), 0644)
	time.Sleep(60 * time.Second) // 父进程摘除连接器时会杀掉本进程
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
	defs := map[string]mcpServerDef{"hung": {
		Command: os.Args[0],
		Args:    []string{"-test.run=TestMCPStdioHungHelper"},
		Env:     map[string]string{"JX_MCP_HUNG_HELPER": "1"},
	}}
	if _, err := r.UpsertFromDefs(defs, "market:hung", "握手不返回的连接器"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	// 等到握手确实在进行：子进程已拉起即意味着 start 已持 c.mu 并卡在读上
	// （c.cmd 在 c.mu 内写入，这里不能拿 c.mu，否则轮询本身就阻塞）。
	waitMCP(t, 10*time.Second, func() bool {
		_, err := os.Stat(filepath.Join(r.Root(), "handshake-ready"))
		return err == nil
	}, "握手没有进入进行中状态")

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
