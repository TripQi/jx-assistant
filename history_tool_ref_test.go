package main

// 「历史工具调用 ↔ 本轮工具目录」归一化的**行为层**用例。
//
// 断言只用公开行为：组装出来的请求（Assemble 的 Msgs/Tools）与真实 RunAgent
// 发给假 SSE 服务端的请求体、以及库里的行。**不引用归一化函数的内部符号**，
// 所以本文件可以原样拷到基线（7e867065）上跑——那里三条路径都原样保留历史
// tool_calls、没有任何降级，用例 ①②③ 必红；④（目录里有该工具时必须一动不动）
// 在基线本来就绿，它是防止误降级的绊线。
//
// 三条路径（同族风险的三个入口）：
//   ① 模式切换：assistant / aux 目录里没有 write / edit / run_command 等；
//   ② 子对话继承：父会话的 dispatch_task / call_api 在子会话目录里不存在；
//   ③ 组件开关：关掉组件后 git_sync（以及 mcp__* 这类插件/MCP 工具）没了定义。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// histRefMarker 是降级引用材料的抬头（行为层唯一契约标记：只认「有一条可读的
// 历史工具引用」这件事，不认具体措辞）。
const histRefMarker = "【历史工具调用"

// histToolTestApp 建一个带临时库与临时工作区的 App（不碰真实 data / config）。
func histToolTestApp(t *testing.T) *App {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{} // NewApp 才有的运行态（sessLock 用）
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.MaxSteps = 6
	a.Cfg.CompactInterval = 0 // 别让压缩在用例中途改写视图
	a.Cfg.CompactLlmPct = 0
	return a
}

// histFakeChannel 给 App 配一条假渠道（RunAgent 用；同时收下发出的请求体）。
func histFakeChannel(t *testing.T, a *App, replies [][]string) *dsmlFakeLLM {
	t.Helper()
	fake := &dsmlFakeLLM{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	a.Cfg.Providers = []ProviderCfg{{ID: "htr", Name: "假渠道", BaseURL: srv.URL, APIKey: "synthetic",
		Models: []ModelCfg{{ID: "htr-m"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "htr", Model: "htr-m"}
	return fake
}

func histSeedUser(t *testing.T, a *App, sid, text, at string) {
	t.Helper()
	if err := a.Store.AppendRow(sid, &StoredMessage{Message: TextMessage("user", text), Kind: kindChat, CreatedAt: at}); err != nil {
		t.Fatal(err)
	}
}

// histSeedStep 落一步历史：assistant 行（一个工具调用 + 思考，没有正文）→ 工具结果行。
func histSeedStep(t *testing.T, a *App, sid, callID, tool, args, result, thinking, at string) {
	t.Helper()
	if err := a.Store.AppendRow(sid, &StoredMessage{
		Message: Message{Role: "assistant", ToolCalls: []ToolCall{{
			ID: callID, Type: "function", Function: ToolCallFunc{Name: tool, Arguments: args},
		}}},
		Thinking: thinking, Kind: kindChat, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow(sid, &StoredMessage{
		Message: Message{Role: "tool", Content: result, ToolCallID: callID},
		Kind:    kindChat, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

func histSeedFinal(t *testing.T, a *App, sid, text, thinking, at string) {
	t.Helper()
	if err := a.Store.AppendRow(sid, &StoredMessage{
		Message: TextMessage("assistant", text), Thinking: thinking, Kind: kindChat, CreatedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
}

// histFingerprint 把「对话行」拼成可比较指纹：任何回写（改正文/改思考/删调用/
// 改参数）都会变。上下文材料行（env / prompt / request / audit）是组装期的正常
// 留痕，不算历史被改写。
func histFingerprint(t *testing.T, a *App, sid string) []string {
	t.Helper()
	rows, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		switch r.Kind {
		case kindEnv, kindPrompt, kindRequest, kindAudit:
			continue
		}
		out = append(out, fmt.Sprintf("%d|%s|%s|%s|%s|%s", r.Seq, r.Kind, r.Role, r.Text(), r.Thinking, histCallDump(r.ToolCalls)))
	}
	return out
}

// histAssertHistoryKept 断言历史没有被回写：修前的行必须逐条逐字节还在。
func histAssertHistoryKept(t *testing.T, before, after []string, stage string) {
	t.Helper()
	if len(after) < len(before) {
		t.Fatalf("%s：库里的行变少了（%d → %d）", stage, len(before), len(after))
	}
	for i := range before {
		if before[i] != after[i] {
			t.Fatalf("%s：历史被回写，第 %d 行不同\n修前：%s\n修后：%s", stage, i+1, before[i], after[i])
		}
	}
}

func histCallDump(calls []ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.ID+"/"+c.Function.Name+"/"+c.Function.Arguments)
	}
	return strings.Join(parts, ";")
}

// histCallsOf 数一数请求里还带着几个某名字的协议级 tool_calls。
func histCallsOf(msgs []Message, name string) int {
	n := 0
	for _, m := range msgs {
		for _, c := range m.ToolCalls {
			if c.Function.Name == name {
				n++
			}
		}
	}
	return n
}

// histRefOf 取出降级引用材料的原文（只取引用那一条，避免与别的正文混淆）。
func histRefOf(msgs []Message) string {
	for _, m := range msgs {
		if t := m.Text(); strings.Contains(t, histRefMarker) {
			return t
		}
	}
	return ""
}

// histJoinedText 把请求里所有文本（正文 + 思考）拼起来，供「内容没丢」的断言。
func histJoinedText(msgs []Message) string {
	var b strings.Builder
	for _, m := range msgs {
		b.WriteString(m.Text())
		b.WriteString("\n")
		if m.ReasoningContent != nil {
			b.WriteString(*m.ReasoningContent)
			b.WriteString("\n")
		}
	}
	return b.String()
}

// histAssertWireLegal 校验报文协议合法：每个 tool_calls 必须紧跟自己的结果，
// tool 结果不能是孤儿（OpenAI 兼容网关硬校验这一条）。
func histAssertWireLegal(t *testing.T, msgs []Message) {
	t.Helper()
	pending := map[string]bool{}
	for i, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			if len(pending) > 0 {
				t.Fatalf("第 %d 条之前的调用还没有结果（协议不合法）", i+1)
			}
			for _, c := range m.ToolCalls {
				pending[c.ID] = true
			}
		case m.Role == "tool":
			if !pending[m.ToolCallID] {
				t.Fatalf("第 %d 条是孤立的 tool 结果（id=%s），协议不合法", i+1, m.ToolCallID)
			}
			delete(pending, m.ToolCallID)
		default:
			if len(pending) > 0 {
				t.Fatalf("第 %d 条插在未配对的调用与结果之间（协议不合法）", i+1)
			}
		}
	}
	if len(pending) > 0 {
		t.Fatalf("仍有 %d 个调用没有结果，协议不合法", len(pending))
	}
}

func histRunAgent(t *testing.T, a *App, sess *SessionMeta) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.RunAgent(ctx, sess, func(Event) {})
}

// ---------- 场景搭建（行为用例与「同一函数」结构断言共用） ----------

// histScenarioModeSwitch：① 模式切换。历史里有 write 调用，会话切到 assistant
// 模式（目录里没有 write）。
func histScenarioModeSwitch(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	a := histToolTestApp(t)
	sess := &SessionMeta{ID: "htr-mode", Mode: "assistant", Workspace: a.Cfg.Workspace}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	histSeedUser(t, a, sess.ID, "把答案写进 notes/答案.md", "2026-01-02 10:00:00")
	histSeedStep(t, a, sess.ID, "call-w1", "write",
		`{"path":"notes/答案.md","content":"`+strings.Repeat("答案正文", 40)+`"}`,
		"已写入 notes/答案.md（320 字节）", "先落盘，再回话", "2026-01-02 10:00:01")
	histSeedFinal(t, a, sess.ID, "写好了：notes/答案.md", "结论思考原文", "2026-01-02 10:00:03")
	return a, sess
}

// histScenarioInherit：② 子对话继承。父会话（assistant 模式）用 dispatch_task
// 派过任务，子会话是 task 模式（目录里没有 dispatch_task）。
func histScenarioInherit(t *testing.T) (*App, *SessionMeta, *dsmlFakeLLM) {
	t.Helper()
	a := histToolTestApp(t)
	fake := histFakeChannel(t, a, [][]string{{"子会话照常收尾"}})
	ws := a.Cfg.Workspace
	parent := &SessionMeta{ID: "htr-parent", Mode: "assistant", Workspace: ws}
	child := &SessionMeta{ID: "htr-child", Mode: "task", Workspace: ws}
	for _, s := range []*SessionMeta{parent, child} {
		if err := a.Store.Create(s); err != nil {
			t.Fatal(err)
		}
	}
	histSeedUser(t, a, parent.ID, "派个任务去核对 dev 分支", "2026-01-02 09:00:00")
	histSeedStep(t, a, parent.ID, "call-d1", "dispatch_task",
		`{"title":"核对 dev","workspace":"D:\\proj\\repo","description":"请核对 dev 与 origin/dev 是否同哈希"}`,
		"已派出工作任务（id=task-1，后台执行中）", "先派任务，等回报", "2026-01-02 09:00:01")
	histSeedFinal(t, a, parent.ID, "已派出去，等它回报", "派发后不空转", "2026-01-02 09:00:03")
	if n, err := a.copyInheritedContext(parent.ID, child.ID); err != nil || n == 0 {
		t.Fatalf("继承应该成功：count=%d err=%v", n, err)
	}
	return a, child, fake
}

// histScenarioComponent：③ 组件开关。历史里有 git_sync（组件 git-panel 提供）
// 与一个 MCP 工具调用（mcp__demo__render，任何目录里都没有定义）。
func histScenarioComponent(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	a := histToolTestApp(t)
	root := t.TempDir()
	writeComp(t, root, gitComponentID, "Git 面板", "1.0.0")
	a.Components = InitComponents(root, filepath.Join(t.TempDir(), "components.json"))
	a.Components.Rescan()
	sess := &SessionMeta{ID: "htr-comp", Mode: "task", Workspace: a.Cfg.Workspace}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	histSeedUser(t, a, sess.ID, "同步一下 dev，再问一下设计稿", "2026-01-03 10:00:00")
	histSeedStep(t, a, sess.ID, "call-g1", "git_sync", `{"op":"fetch"}`,
		"已拉取远程更新（无冲突）", "先拉一下看看", "2026-01-03 10:00:01")
	histSeedStep(t, a, sess.ID, "call-m1", "mcp__demo__render", `{"scene":"login"}`,
		"已生成登录页设计稿（12 个图层）", "顺手让设计插件出一版", "2026-01-02 10:00:05")
	histSeedFinal(t, a, sess.ID, "拉完了，设计稿也出来了", "两件事都办完", "2026-01-03 10:00:09")
	return a, sess
}

// ---------- ① 模式切换 ----------

func TestModeSwitchDegradesHistoryToolCalls(t *testing.T) {
	a, sess := histScenarioModeSwitch(t)
	before := histFingerprint(t, a, sess.ID)

	pack := a.ctxEngine().Assemble(sess, nil)
	// a. 发出的请求里没有那条 tool_calls（也没有它孤立的 tool 结果）
	if n := histCallsOf(pack.Msgs, "write"); n != 0 {
		t.Fatalf("模式切换后请求里仍有 write 的 tool_calls：%d 处", n)
	}
	for _, m := range pack.Msgs {
		if m.Role == "tool" {
			t.Fatalf("降级后不许留下孤立的 tool 结果行：%+v", m)
		}
	}
	histAssertWireLegal(t, pack.Msgs)
	// b. 但有一条可读引用：工具名 + 关键参数 + 结果摘要
	ref := histRefOf(pack.Msgs)
	if ref == "" {
		t.Fatalf("缺少降级引用材料：模型不知道之前做过什么\n请求：%s", histJoinedText(pack.Msgs))
	}
	for _, want := range []string{"write", "notes/答案.md", "已写入"} {
		if !strings.Contains(ref, want) {
			t.Fatalf("引用材料缺 %q：\n%s", want, ref)
		}
	}
	if n := len([]rune(ref)); n > 1500 {
		t.Fatalf("引用材料过长（参数应按既有折叠口径截断）：%d 字\n%s", n, ref)
	}
	// c. 正文与思考原样保留（用户输入、结论正文、结论思考、被降级那步的思考）
	joined := histJoinedText(pack.Msgs)
	for _, want := range []string{"把答案写进 notes/答案.md", "写好了：notes/答案.md", "结论思考原文", "先落盘，再回话"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("降级丢了内容 %q：\n%s", want, joined)
		}
	}
	// d. 落库历史一个字没改
	histAssertHistoryKept(t, before, histFingerprint(t, a, sess.ID), "模式切换降级")
	t.Logf("模式切换后的降级引用：\n%s", ref)
}

// ---------- ② 子对话继承 ----------

func TestInheritedContextDegradesParentToolCalls(t *testing.T) {
	a, child, fake := histScenarioInherit(t)
	before := histFingerprint(t, a, child.ID)

	pack := a.ctxEngine().Assemble(child, nil)
	if n := histCallsOf(pack.Msgs, "dispatch_task"); n != 0 {
		t.Fatalf("子会话（task 目录）里 dispatch_task 必须降级，仍有 %d 处", n)
	}
	histAssertWireLegal(t, pack.Msgs)
	ref := histRefOf(pack.Msgs)
	for _, want := range []string{"dispatch_task", "核对 dev", "已派出工作任务"} {
		if !strings.Contains(ref, want) {
			t.Fatalf("继承降级引用缺 %q：\n%s", want, ref)
		}
	}
	if !strings.Contains(histJoinedText(pack.Msgs), "派个任务去核对 dev 分支") {
		t.Fatal("继承来的用户正文丢了")
	}

	// 子会话能正常跑完一轮，且真正发出去的请求体里也没有那个调用
	if err := histRunAgent(t, a, child); err != nil {
		t.Fatalf("子会话必须能正常跑完一轮：%v", err)
	}
	bodies := fake.requestBodies()
	if len(bodies) == 0 || !strings.Contains(bodies[0], histRefMarker) {
		t.Fatalf("请求体里应有降级引用材料：%v", bodies)
	}
	if strings.Contains(bodies[0], `"name":"dispatch_task"`) {
		t.Fatalf("继承来的 dispatch_task 调用仍进了请求体：%s", clipPreview(bodies[0]))
	}
	histAssertHistoryKept(t, before, histFingerprint(t, a, child.ID), "子对话继承降级")
}

// ---------- ③ 组件开关 ----------

func TestComponentToggleDegradesMCPHistoryCalls(t *testing.T) {
	a, sess := histScenarioComponent(t)
	before := histFingerprint(t, a, sess.ID)

	// ① 组件开着：git_sync 在目录里 → 原样保留；mcp__demo__render 无定义 → 降级
	pack := a.ctxEngine().Assemble(sess, nil)
	if n := histCallsOf(pack.Msgs, "git_sync"); n != 1 {
		t.Fatalf("组件开着时 git_sync 必须原样保留，实际 %d 处", n)
	}
	if n := histCallsOf(pack.Msgs, "mcp__demo__render"); n != 0 {
		t.Fatalf("目录里没有的 MCP 工具必须降级，仍有 %d 处", n)
	}
	histAssertWireLegal(t, pack.Msgs)
	if ref := histRefOf(pack.Msgs); !strings.Contains(ref, "mcp__demo__render") || !strings.Contains(ref, "登录页设计稿") {
		t.Fatalf("MCP 调用的降级引用缺工具名或结果摘要：\n%s", ref)
	}

	// ② 关掉组件：git_sync 也没有定义了 → 一并降级
	if err := a.Components.SetEnabled(gitComponentID, false); err != nil {
		t.Fatal(err)
	}
	pack2 := a.ctxEngine().Assemble(sess, nil)
	if n := histCallsOf(pack2.Msgs, "git_sync"); n != 0 {
		t.Fatalf("关掉组件后 git_sync 必须降级，仍有 %d 处", n)
	}
	ref2 := histRefOf(pack2.Msgs)
	for _, want := range []string{"git_sync", "fetch", "已拉取远程更新"} {
		if !strings.Contains(ref2, want) {
			t.Fatalf("组件关闭后的降级引用缺 %q：\n%s", want, ref2)
		}
	}
	histAssertWireLegal(t, pack2.Msgs)

	// ③ 重新打开：老历史立刻恢复成协议级调用（降级从来没写进库）
	if err := a.Components.SetEnabled(gitComponentID, true); err != nil {
		t.Fatal(err)
	}
	pack3 := a.ctxEngine().Assemble(sess, nil)
	if n := histCallsOf(pack3.Msgs, "git_sync"); n != 1 {
		t.Fatalf("组件重新打开后 git_sync 必须原样恢复，实际 %d 处", n)
	}
	if ref3 := histRefOf(pack3.Msgs); strings.Contains(ref3, "git_sync") {
		t.Fatalf("重新打开后不该再出现 git_sync 的降级引用：\n%s", ref3)
	}
	histAssertHistoryKept(t, before, histFingerprint(t, a, sess.ID), "组件开关降级")
	t.Logf("组件关闭后的降级引用：\n%s", ref2)
}

// ---------- ④ 反向保护：目录里有该工具时一动不动 ----------

func TestHistoryToolCallsKeptWhenToolExists(t *testing.T) {
	a := histToolTestApp(t)
	sess := &SessionMeta{ID: "htr-keep", Mode: "task", Workspace: a.Cfg.Workspace}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	args := `{"path":"a.txt","content":"多行\n内容 with \"quotes\" 与 emoji 🚀"}`
	histSeedUser(t, a, sess.ID, "写个文件", "2026-01-04 10:00:00")
	histSeedStep(t, a, sess.ID, "call-k1", "write", args, "已写入 a.txt（18 字节）", "先写文件", "2026-01-04 10:00:01")
	histSeedFinal(t, a, sess.ID, "写好了", "收尾", "2026-01-04 10:00:02")

	pack := a.ctxEngine().Assemble(sess, nil)
	if ref := histRefOf(pack.Msgs); ref != "" {
		t.Fatalf("工具在当前目录里时不许降级（会丢协议级调用与配对）：\n%s", ref)
	}
	if n := histCallsOf(pack.Msgs, "write"); n != 1 {
		t.Fatalf("write 调用应原样保留，实际 %d 处", n)
	}
	// 逐字节原样：ID / 工具名 / 参数一字不差（前缀缓存也依赖这个稳定性）
	want := "call-k1/write/" + args
	if got := histCallDumpOf(pack.Msgs); got != want {
		t.Fatalf("调用被改动过\nwant: %s\ngot:  %s", want, got)
	}
	histAssertWireLegal(t, pack.Msgs)
	t.Logf("目录里有该工具时原样保留：%s", want)
}

// histCallDumpOf 把请求里全部 tool_calls 拼成一串（只用于「一字不差」的断言）。
func histCallDumpOf(msgs []Message) string {
	var parts []string
	for _, m := range msgs {
		parts = append(parts, histCallDump(m.ToolCalls))
	}
	out := strings.Join(parts, ";")
	return strings.Trim(out, ";")
}
