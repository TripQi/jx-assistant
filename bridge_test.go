package main

// run_python 工具桥的核心路径测试（不联网、不依赖便携 Python）：
//  1. token 生命周期：伪造 token 403；release 后真 token 也 403
//  2. 白名单：非白名单工具（browser_open）被拒且错误可读；白名单 read 成功
//  3. 回报归集：bridgeInterceptReport 命中收集表时吃掉回报（父会话零消息）、
//     jx.wait 语义（取走即清）；bridgeRelease 把未取走的转交父会话
//
// 端到端（真脚本 import jx、后台作业、子任务编排）走开发实例人工验收
//（见会话记录：编译并行开发版 → 浏览器发消息实测 P0/P1/P2）。

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newBridgeTestApp(t *testing.T) (*App, *SessionMeta, *httptest.Server) {
	t.Helper()
	a, m := newQueueTestApp(t)
	srv := httptest.NewServer(http.HandlerFunc(a.handleBridgeTool))
	t.Cleanup(srv.Close)
	// listenPort 只影响 jx.py 里写的地址；桥测试直接打 httptest 服务器
	return a, m, srv
}

func bridgePost(t *testing.T, srv *httptest.Server, token, tool string, args map[string]any) (int, map[string]any) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"token": token, "tool": tool, "args": args})
	resp, err := srv.Client().Post(srv.URL, "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("桥请求失败: %v", err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// 桥 token 生命周期 + 白名单 + 审批参数剥除。
func TestBridgeTokenAndWhitelist(t *testing.T) {
	a, m, srv := newBridgeTestApp(t)
	// 工作区放一个文件供 read 读
	ws := a.Cfg.Workspace
	target := filepath.Join(ws, "hello.txt")
	if err := os.WriteFile(target, []byte("桥测试正文"), 0o644); err != nil {
		t.Fatalf("准备文件失败: %v", err)
	}

	tc := &toolContext{app: a, sess: m}
	a.listenPort = 0 // 不影响本测试（jx.py 地址不由这里校验）
	key, cleanup, err := a.bridgePrepare(tc, "") // 测试不依赖运行时就绪
	if err != nil {
		t.Fatalf("桥装配失败: %v", err)
	}
	if key == "" || cleanup == nil {
		t.Fatal("bridgePrepare 应返回 key 与收尾函数")
	}
	// jx.py 落在会话脚本目录且带端点路径
	py, rerr := os.ReadFile(filepath.Join(pySessionDir(tc), "jx.py"))
	if rerr != nil || !strings.Contains(string(py), "/api/bridge/tool") {
		t.Fatalf("jx.py 未正确落盘: err=%v len=%d", rerr, len(py))
	}

	// 伪造 token → 403
	if code, _ := bridgePost(t, srv, "forged-token", "read", map[string]any{"path": ws + "\\hello.txt"}); code != http.StatusForbidden {
		t.Fatalf("伪造 token 应 403，得到 %d", code)
	}

	// 拿真 token：从 bridgeTokens 表里取（bridgePrepare 不直接返回 token）
	a.bridgeMu.Lock()
	var token string
	for tk, bt := range a.bridgeTokens {
		if bt.key == key {
			token = tk
		}
	}
	a.bridgeMu.Unlock()
	if token == "" {
		t.Fatal("桥表里找不到刚装配的 token")
	}

	// 白名单外：browser_open → ok:false，错误信息指路
	_, out := bridgePost(t, srv, token, "browser_open", map[string]any{"url": "http://x"})
	if ok, _ := out["ok"].(bool); ok {
		t.Fatal("browser_open 不在白名单，应被拒")
	}
	if s, _ := out["error"].(string); !strings.Contains(s, "不在桥白名单") {
		t.Fatalf("拒绝文案应说明白名单：%v", out["error"])
	}

	// 白名单内：read 成功读到文件（批量形态 files 数组）
	_, out = bridgePost(t, srv, token, "read", map[string]any{"files": []any{map[string]any{"path": ws + "\\hello.txt"}}})
	if ok, _ := out["ok"].(bool); !ok {
		t.Fatalf("read 应成功：%v", out)
	}
	if s, _ := out["result"].(string); !strings.Contains(s, "桥测试正文") {
		t.Fatalf("read 结果应含文件正文：%q", s)
	}

	// release 后再调 → 403
	cleanup()
	if code, _ := bridgePost(t, srv, token, "read", map[string]any{"path": ws + "\\hello.txt"}); code != http.StatusForbidden {
		t.Fatalf("release 后 token 应失效（403），得到 %d", code)
	}
	// 幂等：再 release 不炸
	cleanup()
}

// 回报归集与释放转交：拦截吃掉回报 → wait 取走即清；未取走的由 release 转交。
func TestBridgeInterceptAndRelease(t *testing.T) {
	a, m, _ := newBridgeTestApp(t)
	tc := &toolContext{app: a, sess: m}
	key, cleanup, err := a.bridgePrepare(tc, "")
	if err != nil {
		t.Fatalf("桥装配失败: %v", err)
	}
	a.bridgeMu.Lock()
	var tok *bridgeToken
	for _, bt := range a.bridgeTokens {
		if bt.key == key {
			tok = bt
		}
	}
	a.bridgeMu.Unlock()

	sub := &SessionMeta{ID: "subbridge1", Title: "桥派测试子任务", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	bw := &bridgeWait{subID: sub.ID, title: sub.Title, key: key, tok: tok,
		ready: make(chan struct{}), started: time.Now()}
	a.bridgeWaits[sub.ID] = bw

	before := countUserRows(t, a, m.ID)
	reportText := "【派发回报】任务会话「桥派测试子任务」本轮结束。\n…摘要…"
	if !a.bridgeInterceptReport(sub, reportText) {
		t.Fatal("收集表里的子任务应被拦截")
	}
	// 拦截吃掉回报：父会话不该有任何新消息
	if got := countUserRows(t, a, m.ID); got != before {
		t.Fatalf("拦截后父会话不应有新消息：before=%d after=%d", before, got)
	}
	// 取走即清（模拟 jx.wait 的取走语义：表项删除）；同会话再次拦截不再命中
	a.bridgeMu.Lock()
	delete(a.bridgeWaits, sub.ID)
	a.bridgeMu.Unlock()
	if a.bridgeInterceptReport(sub, reportText) {
		t.Fatal("wait 取走后表项应已清除，二次拦截不应命中")
	}

	// 未取走的：登记第二个子任务并拦截，release 后转交父会话
	sub2 := &SessionMeta{ID: "subbridge2", Title: "第二个子任务", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	a.bridgeWaits[sub2.ID] = &bridgeWait{subID: sub2.ID, title: sub2.Title, key: key, tok: tok,
		ready: make(chan struct{}), started: time.Now()}
	if !a.bridgeInterceptReport(sub2, "第二份回报正文") {
		t.Fatal("第二个子任务应被拦截")
	}
	cleanup() // release：未取走回报转交（异步投递）
	waitRowsContain(t, a, m.ID, "第二份回报正文", 5*time.Second)
}

// ---------- 辅助 ----------

func countUserRows(t *testing.T, a *App, sid string) int {
	t.Helper()
	rows, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	n := 0
	for _, r := range rows {
		if r.Role == "user" {
			n++
		}
	}
	return n
}

func waitRowsContain(t *testing.T, a *App, sid, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		rows, err := a.Store.RawMessagesAll(sid)
		if err == nil {
			for _, r := range rows {
				if s, ok := r.Content.(string); ok && strings.Contains(s, want) {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("等不到包含 %q 的消息行", want)
}
