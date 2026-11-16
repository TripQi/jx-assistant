package main

// DSML 泄漏的**行为层**用例：只用公开行为断言（假 SSE 服务端 + 主循环 + 库里
// 落了什么），不引用兼容层内部符号——本文件可以原样拷到基线工作树（e6d4969a）
// 上跑，那里必然红（正文被当普通回答、工具没执行、没有任何告警），装上兼容层
// 后转绿。修前红/修后绿的对照就是用这两份证据做的。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// dsmlFakeLLM 是一台「真实 SSE 假服务端」：按第几次请求决定这一轮回什么增量，
// 并把每次请求体留档（用于断言「回喂纠正材料真的发出去了」）。
type dsmlFakeLLM struct {
	mu      sync.Mutex
	replies [][]string
	hits    int
	bodies  []string
}

func (f *dsmlFakeLLM) handler(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	f.mu.Lock()
	idx := f.hits
	f.hits++
	f.bodies = append(f.bodies, string(body))
	f.mu.Unlock()
	if idx >= len(f.replies) {
		idx = len(f.replies) - 1
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	fl, _ := w.(http.Flusher)
	for _, c := range f.replies[idx] {
		payload, _ := json.Marshal(map[string]any{
			"choices": []any{map[string]any{"delta": map[string]any{"content": c}}},
		})
		fmt.Fprintf(w, "data: %s\n\n", payload)
		if fl != nil {
			fl.Flush()
		}
	}
	fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	if fl != nil {
		fl.Flush()
	}
}

func (f *dsmlFakeLLM) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hits
}

func (f *dsmlFakeLLM) requestBodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// dsmlChunks 把一段正文切成小块（模拟 SSE 把标记切在多个 chunk 里）。
func dsmlChunks(s string, n int) []string {
	rs := []rune(s)
	var out []string
	for i := 0; i < len(rs); i += n {
		end := i + n
		if end > len(rs) {
			end = len(rs)
		}
		out = append(out, string(rs[i:end]))
	}
	return out
}

// dsmlEnv 建一套「假渠道 + 临时工作区 + 任务会话」的测试环境。
func dsmlEnv(t *testing.T, replies [][]string) (*App, *SessionMeta, *dsmlFakeLLM, string) {
	t.Helper()
	fake := &dsmlFakeLLM{replies: replies}
	srv := httptest.NewServer(http.HandlerFunc(fake.handler))
	t.Cleanup(srv.Close)
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	dir := t.TempDir()
	a.Cfg.Workspace = dir
	a.Cfg.MaxSteps = 8
	a.Cfg.Providers = []ProviderCfg{{ID: "dsmlp", Name: "假 DSML 渠道", BaseURL: srv.URL, APIKey: "synthetic",
		Models: []ModelCfg{{ID: "deepseek-flash"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "dsmlp", Model: "deepseek-flash"}
	sess := &SessionMeta{ID: "dsml-sess", Title: "DSML 兼容", Mode: "task", Workspace: dir,
		CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	return a, sess, fake, dir
}

func dsmlRunAgent(t *testing.T, a *App, sess *SessionMeta) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	return a.RunAgent(ctx, sess, func(Event) {})
}

func dsmlRows(t *testing.T, a *App, id string) []*StoredMessage {
	t.Helper()
	rows, err := a.Store.RawMessagesAll(id)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	return rows
}

// dsmlChatRows 只取对话行（审计/快照等上下文材料不算「历史正文」）。
func dsmlChatRows(rows []*StoredMessage) []*StoredMessage {
	out := []*StoredMessage{}
	for _, m := range rows {
		switch m.Kind {
		case kindAudit, kindPrompt, kindEnv, kindRequest, kindSummary, kindInherit:
			continue
		}
		out = append(out, m)
	}
	return out
}

// dsmlAuditEvents 取本会话全部 type=dsml 的审计事件。
func dsmlAuditEvents(t *testing.T, rows []*StoredMessage) []map[string]any {
	t.Helper()
	out := []map[string]any{}
	for _, m := range rows {
		if m.Kind != kindAudit {
			continue
		}
		var p map[string]any
		if json.Unmarshal([]byte(m.Text()), &p) != nil {
			continue
		}
		if p["type"] == "dsml" {
			out = append(out, p)
		}
	}
	return out
}

// dsmlAssertNoMarkup 断言历史里没有泄漏原文（审计行的 before 预览不算：
// 那是「记下了什么坏东西」，不是把标记当回答存下来了）。
func dsmlAssertNoMarkup(t *testing.T, rows []*StoredMessage) {
	t.Helper()
	for _, m := range dsmlChatRows(rows) {
		if strings.Contains(m.Text(), "DSML") {
			t.Fatalf("泄漏原文被当正文入库了（role=%s seq=%d）：%q", m.Role, m.Seq, m.Text())
		}
	}
}

// dsmlLastAssistantCalls 取最后一条带 tool_calls 的 assistant 行。
func dsmlAssistantCalls(t *testing.T, rows []*StoredMessage) ([]ToolCall, string) {
	t.Helper()
	var calls []ToolCall
	text := ""
	for _, m := range dsmlChatRows(rows) {
		if m.Role != "assistant" {
			continue
		}
		if len(m.ToolCalls) > 0 {
			calls = m.ToolCalls
		}
		text = m.Text()
	}
	return calls, text
}

// dsmlWriteBlock 拼一段 DSML 调用块（variant 决定书写形态）。
func dsmlWriteBlock(variant, path, content string) string {
	tag := func(close bool, name string) string {
		switch variant {
		case "spaced":
			if close {
				return "</ | DSML | " + name + ">"
			}
			return "< | DSML | " + name
		case "double":
			if close {
				return "</||DSML||" + name + ">"
			}
			return "<||DSML||" + name
		case "fullwidth":
			if close {
				return "</｜DSML｜" + name + ">"
			}
			return "<｜DSML｜" + name
		}
		if close {
			return "</|DSML|" + name + ">"
		}
		return "<|DSML|" + name
	}
	nl := "\n"
	if variant == "compact" {
		nl = ""
	}
	var b strings.Builder
	b.WriteString(tag(false, "calls") + ">" + nl)
	b.WriteString(tag(false, "invoke") + ` name="write">` + nl)
	b.WriteString(tag(false, "parameter") + ` name="path" string="true">` + path + tag(true, "parameter") + nl)
	b.WriteString(tag(false, "parameter") + ` name="content" string="true">` + content + tag(true, "parameter") + nl)
	b.WriteString(tag(true, "invoke") + nl)
	b.WriteString(tag(true, "calls"))
	return b.String()
}

// TestDsmlLeakExecutesAfterCompat 是修前红/修后绿的主用例：泄漏的 DSML 调用块
// 必须被解析成 tool_call 并真的执行（四种书写形态：标准换行、V4.1 带空格、
// 双管、紧凑无换行）。
func TestDsmlLeakExecutesAfterCompat(t *testing.T) {
	cases := []struct {
		variant string
		want    string // 写进 out.txt 的内容
	}{
		{"standard", "标准形态"},
		{"spaced", "带空格形态"},
		{"double", "双管形态"},
		{"compact", "紧凑形态"},
	}
	for _, c := range cases {
		t.Run(c.variant, func(t *testing.T) {
			block := dsmlWriteBlock(c.variant, "out.txt", c.want)
			a, sess, fake, dir := dsmlEnv(t, [][]string{dsmlChunks(block, 7), dsmlChunks("已完成。", 3)})
			if err := dsmlRunAgent(t, a, sess); err != nil {
				t.Fatalf("RunAgent 失败: %v", err)
			}
			// ① 工具真的执行了（文件真被写出来）
			data, ferr := os.ReadFile(filepath.Join(dir, "out.txt"))
			if ferr != nil {
				t.Fatalf("工具没执行（修前就是这个现象）：%v", ferr)
			}
			if string(data) != c.want {
				t.Fatalf("写进去的内容不对：%q，期望 %q", string(data), c.want)
			}
			// ② 结构化 tool_call 落库，arguments 与原始值一致
			rows := dsmlRows(t, a, sess.ID)
			calls, _ := dsmlAssistantCalls(t, rows)
			if len(calls) != 1 || calls[0].Function.Name != "write" {
				t.Fatalf("应落 1 条 write 的 tool_call，得到 %+v", calls)
			}
			var args struct{ Path, Content string }
			if err := json.Unmarshal([]byte(calls[0].Function.Arguments), &args); err != nil {
				t.Fatalf("arguments 不是 JSON: %v（%q）", err, calls[0].Function.Arguments)
			}
			if args.Path != "out.txt" || args.Content != c.want {
				t.Fatalf("arguments 与原始值不一致：%+v", args)
			}
			// ③ 泄漏原文绝不当正文入库，且有审计事件
			dsmlAssertNoMarkup(t, rows)
			audits := dsmlAuditEvents(t, rows)
			if len(audits) == 0 || audits[len(audits)-1]["action"] != "executed" {
				t.Fatalf("应落一条 action=executed 的 dsml 审计，得到 %+v", audits)
			}
			// ④ 第二次请求带的是真实工具结果（工具真跑完了）
			bodies := fake.requestBodies()
			if len(bodies) != 2 {
				t.Fatalf("应调用模型 2 次（泄漏调用 → 工具结果），实际 %d", len(bodies))
			}
			if !strings.Contains(bodies[1], `"role":"tool"`) || !strings.Contains(bodies[1], "out.txt") {
				t.Fatalf("第二请求里没有工具结果：%s", bodies[1][:min(len(bodies[1]), 400)])
			}
		})
	}
}

// TestDsmlMultiInvokeExecutesInOrder 看护「多调用并列 → 全部执行，顺序与位置对应」。
func TestDsmlMultiInvokeExecutesInOrder(t *testing.T) {
	block := `<|DSML|calls>
<|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">a.txt</|DSML|parameter>
<|DSML|parameter name="content" string="true">AAA</|DSML|parameter>
</|DSML|invoke>
<|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">b.txt</|DSML|parameter>
<|DSML|parameter name="content" string="true">BBB</|DSML|parameter>
</|DSML|invoke>
</|DSML|calls>`
	a, sess, _, dir := dsmlEnv(t, [][]string{dsmlChunks(block, 11), dsmlChunks("都完成了。", 4)})
	if err := dsmlRunAgent(t, a, sess); err != nil {
		t.Fatalf("RunAgent 失败: %v", err)
	}
	for name, want := range map[string]string{"a.txt": "AAA", "b.txt": "BBB"} {
		data, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil || string(data) != want {
			t.Fatalf("%s 没按位置执行：err=%v content=%q", name, err, string(data))
		}
	}
	rows := dsmlRows(t, a, sess.ID)
	calls, _ := dsmlAssistantCalls(t, rows)
	if len(calls) != 2 {
		t.Fatalf("应落 2 条 tool_call，得到 %d", len(calls))
	}
	if !strings.Contains(calls[0].Function.Arguments, "a.txt") || !strings.Contains(calls[1].Function.Arguments, "b.txt") {
		t.Fatalf("两个调用的顺序串了：%+v", calls)
	}
	dsmlAssertNoMarkup(t, rows)
}

// TestDsmlNegativeDiscussionUntouched 是**负向用例**（防误吞正常回答）：模型在
// 正文里讨论/演示 DSML 语法（含完整代码块与说明文字）时，绝不解析、绝不执行、
// 正文原样保留。
func TestDsmlNegativeDiscussionUntouched(t *testing.T) {
	discussion := "DSML 是 DeepSeek V4 起在内部序列化工具调用用的格式，服务端负责把它翻译成 tool_calls。" +
		"它长这样：\n\n```\n" + dsmlWriteBlock("standard", "out.txt", "不该被执行") + "\n```\n\n" +
		"要点有三：第一，专用 token 是 DSML；第二，结构是 XML 风格的 calls/invoke/parameter；" +
		"第三，V4.1 之后标签变成了带空格形态，老正则不认，所以整块会当普通正文透传。" +
		"如果客户端看到这种正文，正确做法是归一化后解析，或者剥掉标记并告警，绝不能直接当回答展示给用户。"
	a, sess, fake, dir := dsmlEnv(t, [][]string{dsmlChunks(discussion, 13)})
	if err := dsmlRunAgent(t, a, sess); err != nil {
		t.Fatalf("RunAgent 失败: %v", err)
	}
	// ① 一个字都不许执行
	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err == nil {
		t.Fatal("正文里演示的 DSML 被当成真调用执行了")
	}
	// ② 正文原样保留（逐字一致，包含代码块与标记）
	rows := dsmlRows(t, a, sess.ID)
	_, text := dsmlAssistantCalls(t, rows)
	if text != discussion {
		t.Fatalf("正文被改写了：\n得到 %q\n期望 %q", text, discussion)
	}
	// ③ 不该有任何 DSML 告警（它本来就不是泄漏）
	if audits := dsmlAuditEvents(t, rows); len(audits) != 0 {
		t.Fatalf("正常讨论被记成了 DSML 事件：%+v", audits)
	}
	if fake.count() != 1 {
		t.Fatalf("讨论场景只该调模型 1 次，实际 %d", fake.count())
	}
}

// TestDsmlNoiseOnlyDoesNotCrash 看护「只剩孤立闭标签」：剥掉 + 审计 + 正文照常，
// 不崩、不把标记入库。
func TestDsmlNoiseOnlyDoesNotCrash(t *testing.T) {
	a, sess, _, _ := dsmlEnv(t, [][]string{dsmlChunks("我来处理。</|DSML|calls>", 3)})
	if err := dsmlRunAgent(t, a, sess); err != nil {
		t.Fatalf("只剩孤立闭标签不该让整轮报错: %v", err)
	}
	rows := dsmlRows(t, a, sess.ID)
	_, text := dsmlAssistantCalls(t, rows)
	if text != "我来处理。" {
		t.Fatalf("残渣应被剥干净，正文得到 %q", text)
	}
	audits := dsmlAuditEvents(t, rows)
	if len(audits) != 1 || audits[0]["action"] != "noise" {
		t.Fatalf("应落一条 action=noise 的审计，得到 %+v", audits)
	}
	dsmlAssertNoMarkup(t, rows)
}

// TestDsmlBadArgsRetryThenGiveUp 看护「参数漂移/必填缺失 → 不执行，走回喂重试，
// 重试上限 2 次，仍失败给用户可见报错」。
func TestDsmlBadArgsRetryThenGiveUp(t *testing.T) {
	// 参数名漂移：schema 要 path，模型给了 content（社区实测的真实形态）
	broken := `<|DSML|calls>
<|DSML|invoke name="write">
<|DSML|parameter name="content" string="true">正文</|DSML|parameter>
</|DSML|invoke>
</|DSML|calls>`
	a, sess, fake, dir := dsmlEnv(t, [][]string{dsmlChunks(broken, 5)})
	err := dsmlRunAgent(t, a, sess)
	if err == nil {
		t.Fatal("连续三次都不合法时，必须给用户可见的报错")
	}
	if !strings.Contains(err.Error(), "不合法") || !strings.Contains(err.Error(), "已剥离") {
		t.Fatalf("报错要说清发生了什么，得到：%v", err)
	}
	// ① 不执行坏调用
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("坏调用被执行了：%v", entries)
	}
	// ② 重试上限：1 次原发 + 2 次重发
	if hits := fake.count(); hits != 3 {
		t.Fatalf("重试上限应为 2 次（合计 3 次调用），实际 %d", hits)
	}
	// ③ 重发请求里真的带了纠正材料（assistant(tool_calls) + tool 错误说明）
	bodies := fake.requestBodies()
	for _, i := range []int{1, 2} {
		if !strings.Contains(bodies[i], "工具调用语法错误") || !strings.Contains(bodies[i], `"role":"tool"`) {
			t.Fatalf("第 %d 次请求没带纠正材料：%s", i+1, bodies[i])
		}
		if !strings.Contains(bodies[i], "标准 tool_calls") {
			t.Fatalf("第 %d 次请求没告诉模型该怎么改：%s", i+1, bodies[i])
		}
	}
	// ④ 审计：2 条 retry + 1 条 giveup，字段齐全
	rows := dsmlRows(t, a, sess.ID)
	audits := dsmlAuditEvents(t, rows)
	if len(audits) != 3 {
		t.Fatalf("应有 3 条 dsml 审计（retry/retry/giveup），得到 %+v", audits)
	}
	if audits[0]["action"] != "retry" || audits[1]["action"] != "retry" || audits[2]["action"] != "giveup" {
		t.Fatalf("审计动作序列不对：%+v", audits)
	}
	for i, p := range audits {
		for _, key := range []string{"provider", "model", "finish_reason", "raw_len", "reason", "before", "after", "retry", "step"} {
			if _, ok := p[key]; !ok {
				t.Fatalf("第 %d 条审计缺字段 %s：%+v", i+1, key, p)
			}
		}
		if !strings.Contains(fmt.Sprint(p["reason"]), "path") {
			t.Fatalf("审计原因应指出缺什么（参数漂移的现场证据）：%+v", p)
		}
		if p["model"] != "dsmlp/deepseek-flash" {
			t.Fatalf("审计要记渠道/模型：%+v", p)
		}
	}
	// ⑤ 泄漏原文与合成调用都不入库
	dsmlAssertNoMarkup(t, rows)
}
