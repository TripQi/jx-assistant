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
//
// 本仓库（二开）增量的三条，都在文件末尾：
//  4. 每一跳都记一条用量明细（组件 usage-stats，purpose=compact）——压缩开销
//     不许漏账；
//  5. 摘要过短 fail-closed：不落库、上下文不被破坏；
//  6. 改造前后「被重复计入的输入 token」实测对比（结构测算，非厂家真值）。

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
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-m"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
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
		a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL, APIKey: "test",
			Models: []ModelCfg{{ID: "fake-m"}}}}
		a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
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

// ---------- 二开增量 ----------

// sseUsage 组一帧流尾 usage（OpenAI 形状：prompt_tokens_details.cached_tokens
// 就是缓存命中量，记账明细的 cache_read 取它）。
func sseUsage(prompt, cached, completion int) string {
	return fmt.Sprintf("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d,"+
		"\"prompt_tokens_details\":{\"cached_tokens\":%d}}}\n\ndata: [DONE]\n\n",
		prompt, completion, prompt+completion, cached)
}

// 每一跳都是一次真实模型调用，就各记一条 usage_events（purpose=compact）：
// 两跳漏记一条，压缩开销就漏账。顺带看护审计行的 hop 标记。
func TestSummarizerHopsMeterUsage(t *testing.T) {
	toolChunk := "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n" +
		sseUsage(1000, 900, 12)
	textChunk := "data: {\"choices\":[{\"delta\":{\"content\":\"" + strings.Repeat("摘", 60) + "\"}}]}\n\n" +
		sseUsage(1200, 1150, 80)
	var mu sync.Mutex
	n := 0
	var bodies []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		idx := n
		n++
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			_, _ = w.Write([]byte(toolChunk))
			return
		}
		_, _ = w.Write([]byte(textChunk))
	}))
	defer srv.Close()

	a := newUsageTestApp(t)
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-m"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	// 会话档位跟着摘要请求走（与主循环同款信封）：明细里也如实记这一档
	sess := &SessionMeta{ID: "hop1", Mode: "task", Thinking: "high", CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ctxEngine().CallSummarizer(sess,
		[]Message{TextMessage("user", "前缀消息"), TextMessage("user", "【压缩任务】……")},
		[]ToolDef{{Type: "function", Function: ToolSpec{Name: "read", Description: "读"}}}); err != nil {
		t.Fatalf("两跳应成功: %v", err)
	}
	waitUsageRows(t, a, 2)
	rows, total, err := a.Store.UsageRecords(usageFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 {
		t.Fatalf("两跳应记两条明细, got %d", total)
	}
	// 新的在前：后写的第二跳在 rows[0]
	for _, r := range rows {
		if r.Purpose != usagePurposeCompact {
			t.Fatalf("用途应为 %s, got %+v", usagePurposeCompact, r)
		}
		if r.Model != "fake-m" || r.ProviderID != "fakep" || !r.OK {
			t.Fatalf("明细字段不对: %+v", r)
		}
	}
	hop2, hop1 := rows[0], rows[1]
	if hop1.Input != 1000 || hop1.CacheRead != 900 || hop2.Input != 1200 || hop2.CacheRead != 1150 {
		t.Fatalf("两跳 token 口径不对: 跳1=%+v 跳2=%+v", hop1, hop2)
	}
	for i, r := range []usageRecord{hop1, hop2} {
		t.Logf("usage_events[%d]：purpose=%s model=%s provider=%s input=%d cache_read=%d output=%d thinking=%q ok=%v",
			i+1, r.Purpose, r.Model, r.ProviderID, r.Input, r.CacheRead, r.Output, r.Thinking, r.OK)
		if r.Thinking != "high" {
			t.Fatalf("明细应记本次请求真正发出去的档位 high, got %q", r.Thinking)
		}
	}
	// 两跳请求体都应带 reasoning_effort=high（摘要调用自本改造起吃会话档位）
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("应捕获两跳请求体, got %d", len(bodies))
	}
	for i, b := range bodies {
		if !strings.Contains(b, "\"reasoning_effort\":\"high\"") {
			t.Fatalf("第 %d 跳请求体未带会话档位: %s", i+1, b)
		}
	}
	// 审计行：两条 scope=summarize，第二跳带 hop=1
	raw, _ := a.Store.RawMessagesAll("hop1")
	audits := 0
	hopMarked := false
	for _, m := range raw {
		if m.Kind != kindAudit {
			continue
		}
		txt := m.Text()
		if !strings.Contains(txt, "\"scope\":\"summarize\"") {
			continue
		}
		audits++
		if strings.Contains(txt, "\"hop\":1") {
			hopMarked = true
		}
	}
	if audits != 2 || !hopMarked {
		t.Fatalf("审计行应两条且第二跳带 hop 标记: audits=%d hopMarked=%v", audits, hopMarked)
	}
}

// 摘要过短 fail-closed（DSH 同款）：半截摘要比没有更危险，宁可报错不落库，
// 会话上下文保持原样可重试。
func TestSummarizeFailClosedShort(t *testing.T) {
	shortChunk := "data: {\"choices\":[{\"delta\":{\"content\":\"记了几笔。\"}}]}\n\n" + sseUsage(800, 0, 5)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(shortChunk))
	}))
	defer srv.Close()

	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-m"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	sess := &SessionMeta{ID: "short1", Mode: "task", CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "short1", "第一轮", true)
	_, _, _, final2 := seedRound(t, a, "short1", "第二轮", true)

	before := len(a.buildContext(sess, "SYS", 0))
	_, err := a.doSummarize(sess, "manual", -1, final2)
	if err == nil || !strings.Contains(err.Error(), "摘要过短") {
		t.Fatalf("短摘要应报错放弃落库, got %v", err)
	}
	raw, _ := a.Store.RawMessagesAll("short1")
	if n := countKind(raw, kindSummary); n != 0 {
		t.Fatalf("不该落摘要行, got %d", n)
	}
	if after := len(a.buildContext(sess, "SYS", 0)); after != before {
		t.Fatalf("上下文不该被破坏: %d → %d", before, after)
	}
}

// legacySummarizeInput 复刻**改造前**的口径（专用系统提示词 + 逐行「用户：…/
// 助手：…」转述文本），只用于下面的 token 测算对比，不参与任何业务路径。
func legacySummarizeInput(raw []*StoredMessage, boundary, segEnd int64) string {
	var b strings.Builder
	b.WriteString("【待压缩的对话历史】\n")
	for _, m := range raw {
		if m.Seq <= boundary || m.Seq > segEnd || !summarizable(m) {
			continue
		}
		role := map[string]string{"user": "用户", "assistant": "助手", "tool": "工具结果"}[m.Role]
		if role == "" {
			role = m.Role
		}
		text := m.Text()
		for _, tc := range m.ToolCalls {
			text += "\n[调用 " + tc.Function.Name + " " + clipText(tc.Function.Arguments, argKeepLimit) + "]"
		}
		b.WriteString(fmt.Sprintf("%s：%s\n", role, clipText(strings.TrimSpace(text), 4000)))
	}
	return b.String()
}

// 前缀改造的收益（结构测算）：同一段历史、同一压缩区间，
//   - 改造前：system=专用摘要提示词 + user=指令+转述文本，与主循环请求毫无
//     共同前缀 → 这些输入 token **全额重新 prefill**（命中≈0）；
//   - 改造后：前缀=截止版组装（与主循环请求逐字节同源，直接命中），只有
//     尾部压缩指令是新增 → 重新 prefill 的只有指令那几百字。
//
// 这里算的是 estimateTokens 口径的结构量（被重复计入的输入 token），**不是
// 厂家返回的 cache_read 真值**——真值要真机跑一次压缩后看 audit 行的
// usage.hit/miss 或 usage_events.cache_read。
func TestCompactPrefixTokenShare(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	sess := &SessionMeta{ID: "share1", Mode: "task", Workspace: ".", CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	var segEnd int64
	for i := 0; i < 6; i++ {
		_, _, _, fin := seedRound(t, a, "share1", fmt.Sprintf("第 %d 轮", i+1), i%2 == 0)
		segEnd = fin
	}
	raw, err := a.Store.RawMessagesAll("share1")
	if err != nil {
		t.Fatal(err)
	}
	sys, _, _ := a.ctxEngine().materials(sess)
	prefix := a.buildContextUpTo(sess, sys, segEnd)
	directive := a.summarizeDirective(sess, "manual", 0, segEnd, nil)

	prefixTok := estimateTokens(prefix)
	directiveMsgs := []Message{TextMessage("user", directive)}
	directiveTok := estimateTokens(directiveMsgs)
	newTotal := prefixTok + directiveTok

	legacy := []Message{
		TextMessage("system", "你是严谨的对话历史压缩助手。"),
		TextMessage("user", directive+"\n\n"+legacySummarizeInput(raw, 0, segEnd)),
	}
	oldTotal := estimateTokens(legacy)

	// 无缓存渠道上的最坏情况（很有必要说清楚）：前缀命不中时，新方案的输入
	// 比旧方案大（多了系统提示词与工具目录），这是「命中率换绝对量」的代价
	worstPct := float64(newTotal-oldTotal) * 100 / float64(oldTotal)
	hitPct := float64(prefixTok) * 100 / float64(newTotal)
	savePct := float64(oldTotal-directiveTok) * 100 / float64(oldTotal)
	t.Logf("输入 token（estimateTokens 口径，同一段 %d 条历史）：\n"+
		"  改造前：请求共 %d token，其中重新 prefill（不计命中）≈ %d token，缓存命中 ≈ 0%%\n"+
		"  改造后：请求共 %d token = 前缀 %d（与主循环逐字节同源，命中）+ 压缩指令 %d（新增）\n"+
		"  → 结构性命中占比 %.1f%%；被重复计入的输入 token 从 %d 降到 %d（降 %.1f%%）\n"+
		"  → 最坏情况（渠道不出缓存）：输入量 %d → %d，增 %.0f%%",
		len(raw), oldTotal, oldTotal, newTotal, prefixTok, directiveTok, hitPct, oldTotal, directiveTok, savePct,
		oldTotal, newTotal, worstPct)

	// 结构性底线：指令只能是请求的零头（前缀占绝对多数才叫「命中」）
	if prefixTok <= directiveTok*4 {
		t.Fatalf("前缀应远大于压缩指令: 前缀=%d 指令=%d", prefixTok, directiveTok)
	}
	if hitPct < 80 {
		t.Fatalf("前缀占比过低（%.1f%%），改造没达到目的", hitPct)
	}
}

// 压缩调用失败也得留一条明细（ok=false + 错误摘要）：漏写就是漏账，压缩
// 失败率与失败花销都算不出来。
func TestSummarizerFailureMetered(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"鉴权失败"}}`))
	}))
	defer srv.Close()

	a := newUsageTestApp(t)
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-m"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	sess := &SessionMeta{ID: "fail1", Mode: "task", CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	if _, err := a.ctxEngine().CallSummarizer(sess,
		[]Message{TextMessage("user", "【压缩任务】……")}, nil); err == nil {
		t.Fatal("渠道鉴权失败时应报错，不能当成功")
	}
	waitUsageRows(t, a, 1)
	rows, total, err := a.Store.UsageRecords(usageFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || rows[0].OK || rows[0].Purpose != usagePurposeCompact || rows[0].Error == "" {
		t.Fatalf("失败调用也应留一条 compact 明细（ok=false + 错误摘要）: total=%d %+v", total, rows)
	}
	t.Logf("usage_events：purpose=%s model=%s ok=%v error=%q", rows[0].Purpose, rows[0].Model, rows[0].OK, rows[0].Error)
}
