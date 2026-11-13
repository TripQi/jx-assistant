package main

// 摘要请求「前缀缓存命中」改造（用户 2026-11-11 的设计：压缩请求复用主对话
// 的前缀——系统提示词、工具目录、截止到压缩位置的历史全部与主循环逐字节
// 同源，只在尾部追加一条压缩指令；模型试图调用工具就拒绝并重申，连续两次
// 报错。DSH 社区同思路实测命中 11.8%→99.7%）的验收测试：
//  1. buildContextUpTo 截止版是全量组装的严格前缀（逐条 JSON 逐字节一致）；
//  2. doSummarize 发出的请求：前缀=截止版组装、末条=压缩指令（任务说明/
//     小节/禁工具令），tools 与主循环同款；
//  3. CallSummarizer 纠偏跳：第一跳模型调用工具 → 合成拒绝响应+重申再跳，
//     第二跳出正文；第二跳请求只 append 不改前缀；连续两跳调工具则报错。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// msgsJSON 把消息序列折成逐条 JSON（比对前缀逐字节一致用）。
func msgsJSON(msgs []Message) []string {
	out := make([]string, len(msgs))
	for i, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			out[i] = fmt.Sprintf("MARSHAL-ERR:%v", err)
			continue
		}
		out[i] = string(b)
	}
	return out
}

// 截止组装是全量组装的严格前缀：含工具调用轮、思考回传、时间提示词的
// 历史在轮边界截止，截止点之前逐条逐字节一致——前缀缓存命中的结构性前提。
func TestBuildContextUpToIsStrictPrefix(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	sess := &SessionMeta{ID: "prefix1", Title: "前缀", Mode: "task", Workspace: ".",
		CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "prefix1", "第一轮", true)
	_, _, _, final1 := seedRound(t, a, "prefix1", "第二轮", true)
	seedRound(t, a, "prefix1", "第三轮", true) // 截止点之后的轮（不进摘要请求）

	full := a.buildContext(sess, "SYS", 0)
	cut := a.buildContext(sess, "SYS", final1) // 截止在第二轮轮尾（轮边界）
	if len(cut) >= len(full) {
		t.Fatalf("截止版(%d)应短于全量(%d)", len(cut), len(full))
	}
	fj, cj := msgsJSON(full), msgsJSON(cut)
	for i := range cj {
		if fj[i] != cj[i] {
			t.Fatalf("第 %d 条分叉:\n全量: %s\n截止: %s", i, fj[i], cj[i])
		}
	}
}

// doSummarize 发出的请求（假网关捕获）：消息前缀=截止版组装 + 末条压缩指令
// （含任务说明、踩坑/成功经验要求、禁工具令），tools 与主循环同款传递。
func TestDoSummarizeRequestShape(t *testing.T) {
	var mu sync.Mutex
	var hops [][]Message
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Messages []Message `json:"messages"`
			Tools    []ToolDef `json:"tools"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("请求体不是合法 ChatRequest: %v", err)
		}
		// 回读成 Message 再比对：map 解码会按字母序重排键，结构体marshal
		// 才与真实发送字节同形
		mu.Lock()
		hops = append(hops, req.Messages)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("摘", 60) + "\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()

	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL,
		Models: []ModelCfg{{ID: "fake-m"}}}}
	sess := &SessionMeta{ID: "shape1", Title: "请求形态", Mode: "task", Workspace: ".",
		CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "shape1", "第一轮", true)
	_, _, _, final2 := seedRound(t, a, "shape1", "第二轮", true)

	// 期望前缀必须在压缩落库**之前**取（落库后 boundary=final2，组装只剩摘要行）
	wantPrefix := msgsJSON(a.buildContext(sess, a.systemPrompt(sess), final2))

	if _, err := a.doSummarize(sess, "manual", -1, final2); err != nil {
		t.Fatalf("doSummarize: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(hops) != 1 || len(hops[0]) < 3 {
		t.Fatalf("应恰好一跳、消息数≥3, got %d 跳", len(hops))
	}
	msgs := hops[0]
	// 末条 = 压缩指令（user）
	last := msgs[len(msgs)-1]
	if last.Role != "user" {
		t.Fatalf("末条应为压缩指令(user), got %v", last.Role)
	}
	for _, want := range []string{"压缩任务", "不允许调用任何工具", "错误与教训", "踩过的坑", "成功经验"} {
		if !strings.Contains(last.Text(), want) {
			t.Errorf("压缩指令缺 %q: %s", want, last.Text())
		}
	}
	// 六节结构（2026-11-11 拍板）：四节新结构点名 + 不设「下一步」节（保留
	// 窗口原文盖住最近动向，摘要里写下一步只会过时）
	for _, want := range []string{"已完成的任务与意图", "关键决定", "用户指令与约束摘录", "进行中与未完成"} {
		if !strings.Contains(last.Text(), want) {
			t.Errorf("压缩指令缺小节 %q", want)
		}
	}
	// 指令正文有一句「不要预告或规划下一步」的说明，但小节列表本身不得含
	// 「下一步」节（列表形如「… / 进行中与未完成」，用分隔形态判定）
	if strings.Contains(last.Text(), "/ 下一步") {
		t.Errorf("压缩指令不该再设「下一步」节（保留窗口原文已盖住）: %s", last.Text())
	}
	// 前缀 = 截止版组装（手动压缩到第二轮轮尾；取值在落库前，见上方）
	if len(msgs)-1 != len(wantPrefix) {
		t.Fatalf("前缀条数不符: 请求 %d vs 组装 %d", len(msgs)-1, len(wantPrefix))
	}
	for i, wm := range wantPrefix {
		got, err := json.Marshal(msgs[i])
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != wm {
			t.Fatalf("前缀第 %d 条分叉:\n请求: %s\n组装: %s", i, got, wm)
		}
	}
}

// CallSummarizer 纠偏跳：第一跳模型返回工具调用 → 合成拒绝响应并重申指令；
// 第二跳正文返回。第二跳请求的前缀与第一跳逐字节一致（只 append 不改写——
// 缓存依然全命中）。连续两跳都调工具则报错。
func TestCallSummarizerToolCallRetry(t *testing.T) {
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\ndata: [DONE]\n\n"
	textChunk := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("摘", 60) + "\"}}]}\n\ndata: [DONE]\n\n"
	summaryText := strings.Repeat("摘", 60)

	run := func(t *testing.T, replySeq []string) (summary string, err error, hops [][]map[string]any) {
		t.Helper()
		var mu sync.Mutex
		n := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Messages []map[string]any `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			idx := n
			n++
			hops = append(hops, body.Messages)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			if idx < len(replySeq) {
				_, _ = w.Write([]byte(replySeq[idx]))
				return
			}
			_, _ = w.Write([]byte(textChunk))
		}))
		defer srv.Close()
		a := newCompactTestApp(t)
		a.initRuntime()
		a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL,
			Models: []ModelCfg{{ID: "fake-m"}}}}
		sess := &SessionMeta{ID: "retry1", Title: "纠偏", Mode: "task", Workspace: ".",
			CreatedAt: now(), UpdatedAt: now()}
		if err := a.Store.Create(sess); err != nil {
			t.Fatal(err)
		}
		s, e := a.ctxEngine().CallSummarizer(sess,
			[]Message{TextMessage("user", "前缀消息"), TextMessage("user", "【压缩任务】……")},
			[]ToolDef{{Type: "function", Function: ToolSpec{Name: "read", Description: "读"}}})
		return s, e, hops
	}

	t.Run("第一跳工具第二跳正文", func(t *testing.T) {
		summary, err, hops := run(t, []string{toolChunk})
		if err != nil {
			t.Fatalf("CallSummarizer: %v", err)
		}
		if summary != summaryText {
			t.Fatalf("摘要正文异常: %q", summary)
		}
		if len(hops) != 2 {
			t.Fatalf("应两跳, got %d", len(hops))
		}
		h1, h2 := hops[0], hops[1]
		if len(h2) != len(h1)+3 { // assistant(调用) + tool(拒绝) + user(重申)
			t.Fatalf("第二跳应只多 3 条纠偏消息: %d → %d", len(h1), len(h2))
		}
		for i := range h1 { // 前缀逐字节一致（缓存命中前提）
			g1, _ := json.Marshal(h1[i])
			g2, _ := json.Marshal(h2[i])
			if string(g1) != string(g2) {
				t.Fatalf("第二跳改写了前缀第 %d 条:\n% s\n%s", i, g1, g2)
			}
		}
		tail := h2[len(h2)-3:]
		if tail[0]["role"] != "assistant" || tail[1]["role"] != "tool" || tail[2]["role"] != "user" {
			t.Fatalf("纠偏三连形态异常: %v %v %v", tail[0]["role"], tail[1]["role"], tail[2]["role"])
		}
		if tc, _ := json.Marshal(tail[1]["content"]); !strings.Contains(string(tc), "不允许调用工具") {
			t.Errorf("合成 tool 响应应说明拒绝原因: %s", tc)
		}
	})
	t.Run("连续两跳调工具则报错", func(t *testing.T) {
		_, err, hops := run(t, []string{toolChunk, toolChunk})
		if err == nil || !strings.Contains(err.Error(), "连续两次调用工具") {
			t.Fatalf("应报「连续两次调用工具」, got %v", err)
		}
		if len(hops) != 2 {
			t.Fatalf("不该有第三跳, got %d", len(hops))
		}
	})
	t.Run("第一跳就出正文则单跳", func(t *testing.T) {
		summary, err, hops := run(t, nil)
		if err != nil || summary != summaryText {
			t.Fatalf("单跳应直接成功: %q %v", summary, err)
		}
		if len(hops) != 1 {
			t.Fatalf("应单跳, got %d", len(hops))
		}
	})
}
