package main

// 规则压缩重构的测试：折叠语义（丢结果/丢思考/丢环境快照、保留用户输入与
// 正文、调用留名+参数）、手动按钮按 upTo 划范围、自动阈值触发的保留窗口、
// 溢出兜底、以及历史查询工具的三列搜索与正向窗口预算。

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newCompactTestApp 建一个带临时库的 App（测试用完即删，只落本目录）。
// DataDir 一并指向临时目录：DefaultConfig 的 "data" 是相对路径，不覆盖的话
// 经 DataDir 的东西（如远程工作区登记库）会读写仓库真实 data/ 目录。
func newCompactTestApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	cfg := DefaultConfig()
	cfg.DataDir = dir
	return &App{Cfg: cfg, Store: s}
}

// seedRound 落一轮对话：用户输入 → 带工具调用的助手行（带思考）→ 工具结果 →
// 助手结论（带思考）。返回关键行 seq（user/call/tool/final）。
func seedRound(t *testing.T, a *App, sid, label string, withTool bool) (user, call, tool, final int64) {
	t.Helper()
	next := func() int64 {
		all, err := a.Store.RawMessagesAll(sid)
		if err != nil {
			t.Fatalf("读历史失败: %v", err)
		}
		return int64(len(all) + 1)
	}
	user = next()
	if err := a.Store.AppendRow(sid, &StoredMessage{
		Message: TextMessage("user", label+"的问题"), Kind: kindChat, CreatedAt: "2026-01-02 10:00:00",
	}); err != nil {
		t.Fatal(err)
	}
	if withTool {
		args, _ := json.Marshal(map[string]any{"path": label + ".txt", "content": strings.Repeat("字", 800)})
		call = next()
		if err := a.Store.AppendRow(sid, &StoredMessage{
			Message: Message{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "c-" + label, Type: "function", Function: ToolCallFunc{Name: "write", Arguments: string(args)},
			}}}, Thinking: label + "的思考", Kind: kindChat, CreatedAt: "2026-01-02 10:00:01",
		}); err != nil {
			t.Fatal(err)
		}
		tool = next()
		if err := a.Store.AppendRow(sid, &StoredMessage{
			Message: Message{Role: "tool", Content: label + "写入成功1024字节", ToolCallID: "c-" + label},
			Kind:    kindChat, CreatedAt: "2026-01-02 10:00:02",
		}); err != nil {
			t.Fatal(err)
		}
	}
	final = next()
	if err := a.Store.AppendRow(sid, &StoredMessage{
		Message: TextMessage("assistant", label+"的结论"), Thinking: label + "的结论思考",
		Kind: kindChat, CreatedAt: "2026-01-02 10:00:03",
	}); err != nil {
		t.Fatal(err)
	}
	return
}

func rowBySeq(t *testing.T, a *App, sid string, seq int64) *StoredMessage {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	for _, m := range all {
		if m.Seq == seq {
			return m
		}
	}
	t.Fatalf("第 %d 行不存在", seq)
	return nil
}

// 折叠语义：用户输入与正文保留原文；调用行=正文+调用名+截断参数（无结果段）；
// 工具结果与环境快照整行 omit。
func TestRuleCompactFoldSemantics(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	user, call, tool, final := seedRound(t, a, "s1", "一轮", true)
	// 对照组：非写入类调用（run_command）参数照常截断到 500 字
	rcArgs, _ := json.Marshal(map[string]any{"command": strings.Repeat("命", 600)})
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c-cmd", Type: "function", Function: ToolCallFunc{Name: "run_command", Arguments: string(rcArgs)}}}},
		Kind:    kindChat, CreatedAt: "2026-01-02 10:00:04",
	}); err != nil {
		t.Fatal(err)
	}
	rcall := final + 1
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("system", "- 环境快照内容行"), Kind: kindEnv, CreatedAt: "2026-01-02 10:00:05",
	}); err != nil {
		t.Fatal(err)
	}
	// 第二轮（让第一轮可折叠）
	seedRound(t, a, "s1", "二轮", false)

	n, err := a.ruleCompactTo(sess, final)
	if err != nil || n != 1 {
		t.Fatalf("ruleCompactTo 想折 1 轮, got n=%d err=%v", n, err)
	}
	if got := rowBySeq(t, a, "s1", user).Compact; got != "一轮的问题" {
		t.Fatalf("用户输入折叠视图应=原文, got %q", got)
	}
	cmpt := rowBySeq(t, a, "s1", call).Compact
	// 动作行：[seq] write 路径（seedRound 的调用行无正文，意图缺省）
	if !strings.Contains(cmpt, "] write 一轮.txt") {
		t.Fatalf("调用行折叠应为动作行 [seq] write 路径: %q", cmpt)
	}
	// write 参数 ≤2000 字原样保留（seedRound 的 write 参数 800 字）
	if !strings.Contains(cmpt, strings.Repeat("字", 800)) {
		t.Fatalf("write 参数应原样保留(800字), got %q", clipPreview(cmpt))
	}
	if strings.Contains(cmpt, "结果=") || strings.Contains(cmpt, "写入成功") {
		t.Fatalf("调用行折叠不应保留结果: %q", cmpt)
	}
	// 对照：run_command 命令 600 字 > 500 → 头 200 +（中间已裁 N 字）+ 尾 100
	cmptCmd := rowBySeq(t, a, "s1", rcall).Compact
	if !strings.Contains(cmptCmd, strings.Repeat("命", cmdKeepHead)) ||
		!strings.Contains(cmptCmd, strings.Repeat("命", cmdKeepTail)) ||
		strings.Contains(cmptCmd, strings.Repeat("命", cmdKeepHead+cmdKeepTail+10)) {
		t.Fatalf("超长命令应头200+尾100裁切, got %q", clipPreview(cmptCmd))
	}
	if !strings.Contains(cmptCmd, "中间已裁") {
		t.Fatalf("超长命令应带裁切标注: %q", clipPreview(cmptCmd))
	}
	// write 成功结果折叠为一句话（不再是 omit——成败本身是有效信息）
	if got := rowBySeq(t, a, "s1", tool).Compact; got != "已写入" {
		t.Fatalf("write 成功结果应折叠为 已写入, got %q", got)
	}
	if got := rowBySeq(t, a, "s1", final).Compact; got != "一轮的结论" {
		t.Fatalf("助手正文折叠视图应=原文, got %q", got)
	}
	envSeq := rcall + 1
	if got := rowBySeq(t, a, "s1", envSeq).Compact; got != compactOmit {
		t.Fatalf("环境快照行应 omit, got %q", got)
	}
}

// clipPreview 截断失败信息里的超长折叠文本，避免 t.Fatalf 打印几万字
func clipPreview(s string) string {
	r := []rune(s)
	if len(r) > 200 {
		return string(r[:200]) + fmt.Sprintf("…(共%d字)", len(r))
	}
	return s
}

// 组装视图：折叠区的工具结果与环境快照不进上下文，用户输入/正文/调用名在，
// 思考不回传；折叠的用户行时间提示词照常派生。
func TestBuildContextAfterFold(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	_, call, tool, final := seedRound(t, a, "s1", "一轮", true)
	seedRound(t, a, "s1", "二轮", false)

	if _, err := a.ruleCompactTo(sess, final); err != nil {
		t.Fatal(err)
	}
	_ = call
	_ = tool
	msgs := a.buildContext(sess, "SYS", 0)
	joined := ""
	for _, m := range msgs {
		if s, ok := m.Content.(string); ok {
			joined += s + "\n"
		}
	}
	for _, want := range []string{"一轮的问题", "] write 一轮.txt", "一轮的结论", "二轮的问题", "二轮的结论"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("折叠后视图缺 %q:\n%s", want, joined)
		}
	}
	for _, ban := range []string{"写入成功1024字节", "环境快照内容行", "结论思考"} {
		if strings.Contains(joined, ban) {
			t.Fatalf("折叠后视图不应再含 %q:\n%s", ban, joined)
		}
	}
	// 时间提示词：两条用户消息各一条（折叠的用户行也照常派生）
	if n := strings.Count(joined, "（发送时间："); n != 2 {
		t.Fatalf("时间提示词应 2 条, got %d:\n%s", n, joined)
	}
	if msgs[0].Role != "system" || msgs[0].Content.(string) != "SYS" {
		t.Fatalf("首条应是系统提示词, got %+v", msgs[0])
	}
}

// 手动按钮的范围与幂等：upTo 所在轮（含）及更早折叠，之后不动；重复点报错；
// 自动入口按保留窗口折叠。
func TestRuleCompactScopeAndIdempotent(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	_, _, _, final1 := seedRound(t, a, "s1", "一轮", false)
	_, _, _, final2 := seedRound(t, a, "s1", "二轮", false)
	_, _, _, final3 := seedRound(t, a, "s1", "三轮", false)

	// 按钮在第二轮结论上：折一、二轮（新轮，一级），第三轮不动
	n, err := a.ruleCompactTo(sess, final2)
	if err != nil || n != 2 {
		t.Fatalf("upTo=二轮结论 应折 2 轮, got n=%d err=%v", n, err)
	}
	if got := rowBySeq(t, a, "s1", final3).Compact; got != "" {
		t.Fatalf("第三轮不应被折叠, got compact=%q", got)
	}
	if got := rowBySeq(t, a, "s1", final1).Compact; got == "" {
		t.Fatal("第一轮应已折叠")
	}
	// 同一位置再点：三轮种子没有工具调用行，一级视图已是最简（用户输入+
	// 最终输出），二级无可再压 → 报已折叠（带工具的轮才有二级可升）
	if _, err := a.ruleCompactTo(sess, final2); err == nil || !strings.Contains(err.Error(), "已折叠") {
		t.Fatalf("最简后重复点应报已折叠, got err=%v", err)
	}
	// 再点第三轮：折第三轮（新轮走一级，不带动已折叠的旧轮）
	if n, err := a.ruleCompactTo(sess, final3); err != nil || n != 1 {
		t.Fatalf("upTo=三轮 应折 1 轮, got n=%d err=%v", n, err)
	}
	// 自动入口（保留 2 轮）此刻没有可折的
	if n := a.ruleCompactAuto(sess, 2); n != 0 {
		t.Fatalf("全部折完后 auto 应 0, got %d", n)
	}
}

// 自动间隔触发（rule 模式零模型调用）与溢出兜底只留最近一轮。
// 保留窗口走字数预算 + 按轮对齐：每个带工具的种子轮约 870 字，预算 2000 时
// 命中点落在二轮的调用行 → 二轮整轮保留，只折一轮（四轮、三轮全在窗口内）。
// 间隔触发线 = 间隔(1000) + 保留(2000) = 3000 字：四轮 ≈ 3500 → 触发。
func TestAutoCompactThresholdAndOverflow(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.KeepChars = 2000
	a.Cfg.CompactInterval = 1000
	a.Cfg.CompactLlmPct = 0 // 专测规则路径：模型阈值关闭（默认 80% 会抢先把请求引去摘要）
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	_, _, _, final1 := seedRound(t, a, "s1", "一轮", true)
	r2user, r2call, r2tool, _ := seedRound(t, a, "s1", "二轮", true)
	_, _, _, final3 := seedRound(t, a, "s1", "三轮", true)
	seedRound(t, a, "s1", "四轮", true)

	// 未折叠 ≈ 3500 字 ≥ 间隔1000+保留2000 → 触发规则折叠（msgs 不再参与规则触发）
	cp, err := a.maybeAutoCompact(sess, []Message{TextMessage("user", strings.Repeat("长", 10000))}, 5000, false)
	if err != nil || !cp {
		t.Fatalf("间隔到线应触发 rule 折叠, got cp=%v err=%v", cp, err)
	}
	if got := rowBySeq(t, a, "s1", final1).Compact; got == "" {
		t.Fatal("窗口外的一轮应已折叠")
	}
	// 命中轮（二轮）整轮保留：开轮的用户行、调用行、工具结果都原样在窗口里
	for _, seq := range []int64{r2user, r2call, r2tool} {
		if got := rowBySeq(t, a, "s1", seq).Compact; got != "" {
			t.Fatalf("命中轮应整轮保留, seq=%d got compact=%q", seq, got)
		}
	}
	if got := rowBySeq(t, a, "s1", final3).Compact; got != "" {
		t.Fatalf("保留窗口内不应折叠, got %q", got)
	}
	// 再次触发：压缩后增量归零（未折叠 ≈ 2630 < 3000）→ 间隔天然节流，不触发
	cp, err = a.maybeAutoCompact(sess, []Message{TextMessage("user", strings.Repeat("长", 10000))}, 5000, false)
	if err != nil || cp {
		t.Fatalf("压缩后间隔未到不应再触发, got cp=%v err=%v", cp, err)
	}
	// 溢出兜底：只留最近 1 轮 → 三轮也被折叠
	if err := a.overflowCompact(sess); err != nil {
		t.Fatalf("溢出兜底失败: %v", err)
	}
	if got := rowBySeq(t, a, "s1", final3).Compact; got == "" {
		t.Fatal("溢出兜底应折叠三轮（只留最近1轮）")
	}
}

// 自动压缩保留窗口按轮对齐的另一半：轮首可以是数字助理转发的消息（aiuser/
// report）——主对话里大部分回合由转发消息开启，若只认用户亲发的消息开轮，
// 几十个回合会被算成一个超长轮、窗口形同虚设；命中轮整轮保留的语义下，
// 边界落在转发消息上即可。
func TestAutoCompactRoundStartsAtForwardedMessage(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.KeepChars = 2000
	sess := &SessionMeta{ID: "s1", Mode: "assistant"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	// 每回合 = 开轮行 + 调用行(思考+800字参数) + 工具结果 + 结论，约 840 字
	round := func(label, openKind string) (open, call, tool, final int64) {
		next := func() int64 {
			all, err := a.Store.RawMessagesAll("s1")
			if err != nil {
				t.Fatalf("读历史失败: %v", err)
			}
			return int64(len(all) + 1)
		}
		args, _ := json.Marshal(map[string]any{"content": strings.Repeat("字", 800)})
		open = next()
		if err := a.Store.AppendRow("s1", &StoredMessage{
			Message: TextMessage("user", label+"的指令"), Kind: openKind, CreatedAt: "2026-01-02 10:00:00",
		}); err != nil {
			t.Fatal(err)
		}
		call = next()
		if err := a.Store.AppendRow("s1", &StoredMessage{
			Message: Message{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "c-" + label, Type: "function", Function: ToolCallFunc{Name: "write", Arguments: string(args)},
			}}}, Thinking: label + "的思考", Kind: kindChat, CreatedAt: "2026-01-02 10:00:01",
		}); err != nil {
			t.Fatal(err)
		}
		tool = next()
		if err := a.Store.AppendRow("s1", &StoredMessage{
			Message: Message{Role: "tool", Content: label + "写入成功", ToolCallID: "c-" + label},
			Kind:    kindChat, CreatedAt: "2026-01-02 10:00:02",
		}); err != nil {
			t.Fatal(err)
		}
		final = next()
		if err := a.Store.AppendRow("s1", &StoredMessage{
			Message: TextMessage("assistant", label+"的结论"), Thinking: label + "的结论思考",
			Kind: kindChat, CreatedAt: "2026-01-02 10:00:03",
		}); err != nil {
			t.Fatal(err)
		}
		return
	}
	// 只有第一回合是真人用户消息开轮，其余都由数字助理的转发消息开启
	_, _, _, final1 := round("甲", kindChat)
	r2open, r2call, r2tool, r2final := round("乙", kindAiUser)
	r3open, _, _, _ := round("丙", kindReport)
	round("丁", kindAiUser)

	raw, err := a.Store.RawMessagesAll("s1")
	if err != nil {
		t.Fatal(err)
	}
	// 预算 2000：丁丙两轮(约1680)装得下，命中点落在乙轮的调用行 → 乙轮整轮
	// 保留，保留区起点 = 乙轮的 aiuser 开轮行（转发消息即可作边界）
	if got := a.keepStartSeq(raw); got != r2open {
		t.Fatalf("保留区起点应=乙轮的转发消息行(%d), got %d", r2open, got)
	}
	if n := a.ruleCompactAuto(sess, -1); n == 0 {
		t.Fatal("自动压缩应折掉甲轮")
	}
	if got := rowBySeq(t, a, "s1", final1).Compact; got == "" {
		t.Fatal("窗口外的甲轮应已折叠")
	}
	for _, seq := range []int64{r2open, r2call, r2tool, r2final} {
		if got := rowBySeq(t, a, "s1", seq).Compact; got != "" {
			t.Fatalf("命中轮(乙)应整轮保留, seq=%d got compact=%q", seq, got)
		}
	}
	if got := rowBySeq(t, a, "s1", r2open).Kind; got != kindAiUser {
		t.Fatalf("边界行应是 aiuser 转发消息, got kind=%q", got)
	}
	if got := rowBySeq(t, a, "s1", r3open).Kind; got != kindReport {
		t.Fatalf("丙轮开轮行应是 report, got kind=%q", got)
	}
}

// 三列搜索：正文、工具调用参数、思考过程都能命中，片段取自命中列。
func TestSearchMessagesThreeColumns(t *testing.T) {
	a := newCompactTestApp(t)
	if err := a.Store.Create(&SessionMeta{ID: "s1", Mode: "task", Title: "搜索源"}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("user", "关于部署脚本的讨论"), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	args, _ := json.Marshal(map[string]any{"cmd": "deploy --key=deploy_key_773"})
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "run_command", Arguments: string(args)}}}},
		Kind:    kindChat,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("assistant", "部署完成"), Thinking: "我在考虑uniqueThought配置优先级",
		Kind: kindChat,
	}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		query string
		want  string
	}{
		{"deploy_key_773", "run_command"},
		{"uniqueThought", "配置优先级"},
		{"部署脚本", "讨论"},
	} {
		hits, err := a.Store.SearchMessages(tc.query, 5, 0)
		if err != nil {
			t.Fatalf("搜 %s 失败: %v", tc.query, err)
		}
		if len(hits) != 1 {
			t.Fatalf("搜 %s 应 1 命中, got %d", tc.query, len(hits))
		}
		if !strings.Contains(hits[0].Snippet, tc.want) {
			t.Fatalf("搜 %s 的片段应含 %q, got %q", tc.query, tc.want, hits[0].Snippet)
		}
	}
	// 上下文窗口参数放大片段半径
	hits, _ := a.Store.SearchMessages("uniqueThought", 5, 2000)
	if len(hits) != 1 || len([]rune(hits[0].Snippet)) < 20 {
		t.Fatalf("大半径片段应更长, got %q", hits[0].Snippet)
	}
}

// 正向窗口读取：from_seq 起从旧到新、思考过程可见、字数预算用尽即停并给续读起点。
func TestReadSessionMessagesForwardBudget(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("user", "第一问"), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("assistant", "答A"), Thinking: "思考A", Kind: kindChat,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("user", strings.Repeat("巨", 900)), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("assistant", "最后的结论"), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("system", "- 环境行不该出现"), Kind: kindEnv}); err != nil {
		t.Fatal(err)
	}

	tk := &toolContext{app: a, sess: sess}
	// 预算充足：全部对话行可见（含思考），环境快照被滤掉
	out, err := runReadSessionMessages(tk, json.RawMessage(`{"session_id":"s1","from_seq":1,"chars":6000}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"【思考】思考A", "第一问", "最后的结论"} {
		if !strings.Contains(out, want) {
			t.Fatalf("正向读取缺 %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "环境行") {
		t.Fatalf("环境快照不应出现在历史读取:\n%s", out)
	}
	// 预算收紧：读到巨型消息前停止并给出续读起点
	out, err = runReadSessionMessages(tk, json.RawMessage(`{"session_id":"s1","from_seq":1,"chars":100}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "最后的结论") {
		t.Fatalf("小预算不应读到末尾:\n%s", out)
	}
	if !strings.Contains(out, "继续读用 from_seq=") {
		t.Fatalf("预算截断应给续读起点:\n%s", out)
	}
}

// 分支继承：从已折叠的会话 fork，压缩标记与思考过程必须随分支复制过去——
// 否则分支的上下文视图会瞬间膨胀回未压缩状态（E2E 实测抓出的存量 bug）。
func TestForkPreservesCompactAndThinking(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	_, _, _, final := seedRound(t, a, "s1", "一轮", true)
	seedRound(t, a, "s1", "二轮", false)
	if _, err := a.ruleCompactTo(sess, final); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/api/sessions/s1/fork", strings.NewReader(
		fmt.Sprintf(`{"upTo":%d}`, final)))
	r.SetPathValue("id", "s1")
	a.handleFork(w, r)
	if w.Code != 200 {
		t.Fatalf("fork 失败: %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil || resp.ID == "" {
		t.Fatalf("fork 响应异常: %s", w.Body.String())
	}
	fk, err := a.Store.RawMessagesAll(resp.ID)
	if err != nil {
		t.Fatal(err)
	}
	sawToolFold, sawFold, sawThinking := false, false, false
	for _, m := range fk {
		if m.Role == "tool" && m.Compact == "已写入" {
			sawToolFold = true
		}
		if strings.Contains(m.Compact, "] write 一轮.txt") {
			sawFold = true
		}
		if m.Role == "assistant" && m.Thinking != "" {
			sawThinking = true
		}
	}
	if !sawToolFold || !sawFold || !sawThinking {
		t.Fatalf("分支继承不完整: toolFold=%v fold=%v thinking=%v", sawToolFold, sawFold, sawThinking)
	}
}

// 双阈值选路：估算 token 在两阈值之间走规则折叠（零模型调用）；达到模型阈值
// 间隔制时序：间隔到线先走规则折叠（零模型调用，mock 提供方不产生摘要行），
// 折后增量归零但估算仍超模型线（CompactLlmPct 对窗口百分比）才摘要兜底；
// 间隔 0 即关闭规则路。
func TestMaybeAutoCompactIntervalThenSummary(t *testing.T) {
	a := newCompactTestApp(t)
	// 显式固定间隔/模型线，锁住「间隔到线先折、折后仍超走摘要」的时序语义
	a.Cfg.CompactInterval = 1000
	a.Cfg.CompactLlmPct = 80
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	a.Cfg.KeepChars = 1000 // 保留窗口收紧，压掉一轮之外的早期轮
	s1 := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(s1); err != nil {
		t.Fatal(err)
	}
	s2 := &SessionMeta{ID: "s2", Mode: "task"}
	if err := a.Store.Create(s2); err != nil {
		t.Fatal(err)
	}
	_, _, _, final1 := seedRound(t, a, "s1", "一轮", true)
	seedRound(t, a, "s1", "二轮", true)
	seedRound(t, a, "s1", "三轮", true)
	// 未折叠 ≈ 2600 字 ≥ 间隔1000+保留1000 → 规则折叠；est 7500 token < 8000（1万窗口×80%）不摘要
	cp, err := a.maybeAutoCompact(s1, []Message{TextMessage("user", strings.Repeat("长", 10000))}, 10000, false)
	if err != nil || !cp {
		t.Fatalf("间隔到线应走规则折叠, got cp=%v err=%v", cp, err)
	}
	if got := rowBySeq(t, a, "s1", final1).Compact; got == "" {
		t.Fatal("规则路径应折掉一轮")
	}
	if raw, _ := a.Store.RawMessagesAll("s1"); countKind(raw, kindSummary) != 0 {
		t.Fatal("间隔触发不该生成摘要")
	}
	// 折后增量归零（未折叠 < 2000 不再折），但 est 11000 字 ≈ 8250 ≥ 8000 → 摘要兜底
	for _, label := range []string{"甲", "乙", "丙"} {
		seedRound(t, a, "s2", label, true)
	}
	cp, err = a.maybeAutoCompact(s2, []Message{TextMessage("user", strings.Repeat("长", 11000))}, 10000, false)
	if err != nil || !cp {
		t.Fatalf("间隔与模型线同超时第一遍应先折叠, got cp=%v err=%v", cp, err)
	}
	if raw, _ := a.Store.RawMessagesAll("s2"); countKind(raw, kindSummary) != 0 {
		t.Fatal("第一遍只折叠，不该生成摘要")
	}
	cp, err = a.maybeAutoCompact(s2, []Message{TextMessage("user", strings.Repeat("长", 11000))}, 10000, false)
	if err != nil || !cp {
		t.Fatalf("第二遍（间隔未到、仍超模型线）应走摘要, got cp=%v err=%v", cp, err)
	}
	raw, _ := a.Store.RawMessagesAll("s2")
	if countKind(raw, kindSummary) != 1 {
		t.Fatalf("应落一条自动摘要, got %d", countKind(raw, kindSummary))
	}
	// 间隔 0 = 规则路关闭：未达模型线就完全不压缩
	s3 := &SessionMeta{ID: "s3", Mode: "task"}
	if err := a.Store.Create(s3); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "s3", "一轮", true)
	seedRound(t, a, "s3", "二轮", true)
	a.Cfg.CompactInterval = 0
	cp, err = a.maybeAutoCompact(s3, []Message{TextMessage("user", strings.Repeat("长", 10000))}, 10000, false)
	if err != nil || cp {
		t.Fatalf("间隔 0 且未达模型线应不压缩, got cp=%v err=%v", cp, err)
	}
	// 只开模型线（间隔 0）：达线直接走摘要，折叠标记一律不动
	s4 := &SessionMeta{ID: "s4", Mode: "task"}
	if err := a.Store.Create(s4); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "s4", "一轮", true)
	seedRound(t, a, "s4", "二轮", true)
	seedRound(t, a, "s4", "三轮", true)
	a.Cfg.CompactLlmPct = 60
	cp, err = a.maybeAutoCompact(s4, []Message{TextMessage("user", strings.Repeat("长", 8700))}, 10000, false)
	if err != nil || !cp {
		t.Fatalf("达模型线应走摘要, got cp=%v err=%v", cp, err)
	}
	raw, _ = a.Store.RawMessagesAll("s4")
	if countKind(raw, kindSummary) != 1 {
		t.Fatalf("应落一条自动摘要, got %d", countKind(raw, kindSummary))
	}
	for _, m := range raw {
		if m.Compact != "" {
			t.Fatalf("规则路关闭时不该动折叠标记, seq=%d got %q", m.Seq, m.Compact)
		}
	}
}

// countKind 数历史里某 kind 的行数（测试断言用）。
func countKind(msgs []*StoredMessage, kind string) int {
	n := 0
	for _, m := range msgs {
		if m.Kind == kind {
			n++
		}
	}
	return n
}

// 间隔制计量：chars 单位按未折叠可见字数（触发线=间隔+保留）；tokens 单位用
// usage 真值差值——首个 usage 即基线，压缩后 pendingBase 等新 usage 刷新基线，
// 换模型时 tokenizer 不可比、同样重建基线。
func TestCompactIntervalUnits(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.CompactInterval = 1000
	a.Cfg.KeepChars = 500
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "s1", "一轮", true) // 未折叠 ≈ 870 字
	if a.intervalReached(sess) {
		t.Fatal("870 字 < 间隔1000+保留500，不应触发")
	}
	seedRound(t, a, "s1", "二轮", true) // ≈ 1740
	if !a.intervalReached(sess) {
		t.Fatal("1740 字 ≥ 1500，应触发")
	}
	// tokens 单位：没有 usage 记录不触发；差值到线触发；压缩后等基线刷新
	a.Cfg.CompactUnit = "tokens"
	if a.intervalReached(sess) {
		t.Fatal("tokens 单位下没有 usage 记录不应触发")
	}
	a.noteCompactUsage("s1", "model-a", 1000) // 首个 usage 即基线
	if a.intervalReached(sess) {
		t.Fatal("差值 0 不应触发")
	}
	a.noteCompactUsage("s1", "model-a", 1500)
	if a.intervalReached(sess) {
		t.Fatal("差值 500 < 1000 不应触发")
	}
	a.noteCompactUsage("s1", "model-a", 2000)
	if !a.intervalReached(sess) {
		t.Fatal("差值 1000 ≥ 1000 应触发")
	}
	a.markCompactBase("s1") // 模拟压缩完成
	if a.intervalReached(sess) {
		t.Fatal("压缩后基线未刷新（pendingBase）不应触发")
	}
	a.noteCompactUsage("s1", "model-a", 1200) // 压缩后首个 usage = 新基线
	if a.intervalReached(sess) {
		t.Fatal("新基线差值 0 不应触发")
	}
	a.noteCompactUsage("s1", "model-b", 9999) // 换模型：tokenizer 不可比，重建基线
	if a.intervalReached(sess) {
		t.Fatal("换模型后应以新模型 usage 为基线，差值 0 不应触发")
	}
	a.noteCompactUsage("s1", "model-b", 9999+1000)
	if !a.intervalReached(sess) {
		t.Fatal("新模型差值 1000 应触发")
	}
}

// 间隔制迁移：老配置（百分比阈值+冷却的字段已删除）读入时补默认 间隔40万/
// chars/模型线80；旧 off 保持自动压缩全关（间隔 0）并把模式归一为当前默认
// （手动按钮的默认行为总要有一个）；新键齐全的配置原样保留（含显式 0 与非法
// 单位回落）。旧字段 compactMode/compactThreshold 原样保留，供旧版 exe 回滚。
func TestConfigCompactIntervalMigration(t *testing.T) {
	run := func(content string) *Config {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(path)
		if err != nil {
			t.Fatalf("LoadConfig 失败: %v", err)
		}
		return cfg
	}
	cfg := run(`{"compactMode":"llm","compactThreshold":0.9}`)
	if cfg.CompactInterval != 400000 || cfg.CompactUnit != "chars" || cfg.CompactLlmPct != 80 {
		t.Fatalf("老配置应迁移为 400000/chars/80, got %d/%s/%d", cfg.CompactInterval, cfg.CompactUnit, cfg.CompactLlmPct)
	}
	if cfg.CompactMode != "llm" || cfg.CompactThreshold != 0.9 {
		t.Fatalf("旧字段应原样保留, got mode=%q threshold=%v", cfg.CompactMode, cfg.CompactThreshold)
	}
	cfg = run(`{"compactMode":"off","compactThreshold":0.8}`)
	if cfg.CompactInterval != 0 || cfg.CompactLlmPct != 0 {
		t.Fatalf("旧 off 应迁移为自动压缩全关(间隔0), got %d/%d", cfg.CompactInterval, cfg.CompactLlmPct)
	}
	if cfg.CompactMode != "llm" {
		t.Fatalf("off 应归一为当前默认 llm, got %q", cfg.CompactMode)
	}
	cfg = run(`{"compactMode":"rule","compactInterval":50000,"compactUnit":"tokens","compactLlmPct":0}`)
	if cfg.CompactInterval != 50000 || cfg.CompactUnit != "tokens" || cfg.CompactLlmPct != 0 {
		t.Fatalf("显式新键应原样保留, got %d/%s/%d", cfg.CompactInterval, cfg.CompactUnit, cfg.CompactLlmPct)
	}
	// 旧配置显式 compactRulePct:0 = 用户明确关闭规则通道：语义延续为间隔 0，
	// 不因机制切换被重新打开
	cfg = run(`{"compactMode":"llm","compactRulePct":0,"compactLlmPct":80}`)
	if cfg.CompactInterval != 0 {
		t.Fatalf("旧 rulePct=0 应迁移为 interval=0（规则通道保持关闭）, got %d", cfg.CompactInterval)
	}
	// keepChars：显式 0 合法（全压语义，不回默认）；无键的老配置补默认
	cfg = run(`{"compactMode":"llm","keepChars":0,"compactInterval":400000}`)
	if cfg.KeepChars != 0 {
		t.Fatalf("显式 keepChars:0 应原样保留（全压语义）, got %d", cfg.KeepChars)
	}
	cfg = run(`{"compactMode":"llm"}`)
	if cfg.KeepChars != 100000 {
		t.Fatalf("无 keepChars 键应补默认 100000, got %d", cfg.KeepChars)
	}
	cfg = run(`{"compactMode":"rule","compactInterval":50000,"compactUnit":"bogus"}`)
	if cfg.CompactUnit != "chars" {
		t.Fatalf("非法单位应回落 chars, got %q", cfg.CompactUnit)
	}
}

// 设置页压缩参数「改不动、保存不生效」回归：POST /api/config 保存（响应即
// GET 同一视图）后间隔/单位/模型线三键必须原样回传，且落盘重载不失真。
func TestConfigAPICompactKeysRoundTrip(t *testing.T) {
	a := newCompactTestApp(t)
	a.CfgPath = filepath.Join(t.TempDir(), "config.json")
	// 设置页保存体：间隔 300000 / 单位 tokens / 模型线 0（不启用）
	body := strings.NewReader(`{"compact":{"mode":"llm","interval":300000,"unit":"tokens","llmPct":0}}`)
	w := httptest.NewRecorder()
	a.handleSetConfig(w, httptest.NewRequest("POST", "/api/config", body))
	if w.Code != 200 {
		t.Fatalf("保存应成功, got %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if got["compactInterval"] != float64(300000) || got["compactUnit"] != "tokens" || got["compactLlmPct"] != float64(0) {
		t.Fatalf("压缩三键应原样回传, got interval=%v unit=%v llm=%v",
			got["compactInterval"], got["compactUnit"], got["compactLlmPct"])
	}
	if got["compactMode"] != "llm" {
		t.Fatalf("compactMode 应为 llm, got %v", got["compactMode"])
	}
	// 回显口径要活过重启：落盘文件重载后三个值原样
	cfg2, err := LoadConfig(a.CfgPath)
	if err != nil {
		t.Fatalf("重载配置失败: %v", err)
	}
	if cfg2.CompactInterval != 300000 || cfg2.CompactUnit != "tokens" || cfg2.CompactLlmPct != 0 {
		t.Fatalf("落盘值应保持 300000/tokens/0, got %d/%s/%d", cfg2.CompactInterval, cfg2.CompactUnit, cfg2.CompactLlmPct)
	}
}

// 字数预算保留算法：从最新一条往前累计模型可见字数，预算耗尽处（命中点）所在
// 的轮整轮保留——保留区起点永远对齐到轮首，压缩边界只在轮与轮之间，绝不切轮
// （命中轮自身超预算也一样：整轮保留）。
func TestKeepStartSeq(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	// 三个小轮（无工具）：每轮 = 用户输入(4字) + 结论(4字+6字思考) = 14 字，两行
	//（user 4 字 + assistant 10 字）
	seedRound(t, a, "s1", "甲", false)
	seedRound(t, a, "s1", "乙", false)
	seedRound(t, a, "s1", "丙", false)
	raw, err := a.Store.RawMessagesAll("s1")
	if err != nil {
		t.Fatal(err)
	}
	// 预算 30：丙乙两轮(28)装得下，命中点落在甲轮的结论行——甲轮(14字)自身
	// 不超预算，整轮保留 → 保留区从第 1 条起（宁可略超预算，也不留半轮）
	a.Cfg.KeepChars = 30
	if got := a.keepStartSeq(raw); got != 1 {
		t.Fatalf("预算 30 的保留区应整轮保到第 1 条, got %d", got)
	}
	// 预算大到装下全部 → 从第一条起
	a.Cfg.KeepChars = 1000
	if got := a.keepStartSeq(raw); got != 1 {
		t.Fatalf("预算 1000 的保留区应从第 1 条起, got %d", got)
	}
	// 命中轮整轮保留的硬口径：这个 5 万字的末轮超预算，命中点落在它内部——
	// 整轮都不压，压缩边界只在轮与轮之间（早前行级回退口径会把一轮切成两半，
	// 已砍掉：实测普通任务轮也被判成巨轮、压缩落在轮中间）
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("user", "巨轮问题"), Kind: kindChat,
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("assistant", strings.Repeat("巨", 50000)), Kind: kindChat,
	}); err != nil {
		t.Fatal(err)
	}
	raw, _ = a.Store.RawMessagesAll("s1")
	a.Cfg.KeepChars = 10000
	a.Cfg.CompactInterval = 5000 // 触发线 15000：未折叠 ≈ 5 万字，必然到线
	a.Cfg.CompactLlmPct = 0      // 专测规则路径
	if got := a.keepStartSeq(raw); got != 7 {
		t.Fatalf("命中轮应整轮保留（起点=轮首 7）, got %d", got)
	}
	// 端到端：间隔触发后只折命中轮之前的旧轮，命中轮（7-8）整轮原文保留
	cp, err := a.maybeAutoCompact(sess, []Message{TextMessage("user", strings.Repeat("长", 10000))}, 5000, false)
	if err != nil || !cp {
		t.Fatalf("阈值应触发折叠, cp=%v err=%v", cp, err)
	}
	for _, seq := range []int64{7, 8} {
		if got := rowBySeq(t, a, "s1", seq).Compact; got != "" {
			t.Fatalf("命中轮内的行不应被折叠（整轮保留）, seq=%d got %q", seq, got)
		}
	}
	if got := rowBySeq(t, a, "s1", 1).Compact; got == "" {
		t.Fatal("预算外的旧轮应已折叠")
	}
}

// 命中轮整轮保留 + 溢出兜底两条线：按预算的压缩（手动/自动）不切轮——命中
// 轮再大也整轮保原文；真遇到按预算压不动的超长轮时，由溢出兜底（keep=0，
// 撞模型窗口的最后防线）按轮整轮强压，巨轮的工具结果整行 omit。
// 早年的「百万级单轮压不动」根因是派发消息没当作轮起点（几十回合算一个超长
// 轮），splitRounds 已修正；随之砍掉了会把一轮切成两半的行级回退口径。
func TestHugeRoundKeptWholeThenOverflowCompacts(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.KeepChars = 10000
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	// 一个普通带工具的小轮（预算压缩时会被折掉）
	seedRound(t, a, "s1", "旧轮", true)
	// 一个超预算的轮：用户输入 + 带调用的助手行 + 巨大工具结果（omit 的对象）
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("user", "巨轮问题"), Kind: kindChat, CreatedAt: "2026-01-02 10:10:00",
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "gc", Type: "function", Function: ToolCallFunc{Name: "run_command", Arguments: `{"command":"cat huge.log"}`}}}},
		Kind:    kindChat, CreatedAt: "2026-01-02 10:10:01",
	}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: Message{Role: "tool", Content: strings.Repeat("巨", 200000), ToolCallID: "gc"},
		Kind:    kindChat, CreatedAt: "2026-01-02 10:10:02",
	}); err != nil {
		t.Fatal(err)
	}
	all, _ := a.Store.RawMessagesAll("s1")
	giantStart := int64(len(all) - 2) // 巨轮用户行 seq（旧轮 1-4 → 巨轮 5-8）
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("assistant", "巨轮结论"), Kind: kindChat, CreatedAt: "2026-01-02 10:10:03",
	}); err != nil {
		t.Fatal(err)
	}

	// 预算压缩（手动空 body 与自动同一路径）：命中轮是巨轮 → 整轮保留，
	// 只折旧轮；绝不从巨轮中间切（旧口径会折它的前半段）
	if n := a.ruleCompactAuto(sess, -1); n == 0 {
		t.Fatal("预算压缩应折旧轮，而不是报没有可折叠的回合")
	}
	if got := rowBySeq(t, a, "s1", 1).Compact; got == "" {
		t.Fatal("预算外的旧轮应已折叠")
	}
	for _, seq := range []int64{giantStart, giantStart + 1, giantStart + 2, giantStart + 3} {
		if got := rowBySeq(t, a, "s1", seq).Compact; got != "" {
			t.Fatalf("命中轮（巨轮）应整轮保留原文, seq=%d got %q", seq, got)
		}
	}

	// 溢出兜底（上下文撞模型窗口的最后防线）：按轮整轮强压，巨轮工具结果被折叠
	if n := a.ruleCompactAuto(sess, 0); n == 0 {
		t.Fatal("溢出兜底应能强压巨轮")
	}
	if got := rowBySeq(t, a, "s1", giantStart+2).Compact; got == "" {
		t.Fatal("溢出兜底后巨大工具结果应已被折叠")
	}
}

// 模式切换不影响历史压缩的应用：压缩发生了就是发生了——规则折叠的历史在
// llm（AI 摘要）模式下仍按折叠视图组装，摘要历史在 rule 模式下仍照常注入。
// 设置里的压缩模式只决定未来的压缩行为。回归测试：此前组装与 Cfg.CompactMode
// 耦合，切到摘要模式后历史折叠标记被无视、按原始消息整包重组，主对话上下文
// 从 20 多万 token 飙回 70 多万（压缩等于失效）。
func TestCompactHistoryIndependentOfMode(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	_, _, _, final := seedRound(t, a, "s1", "一轮", true)
	seedRound(t, a, "s1", "二轮", false)
	if _, err := a.ruleCompactTo(sess, final); err != nil {
		t.Fatal(err)
	}

	join := func(mode string) string {
		a.Cfg.CompactMode = mode
		var b strings.Builder
		for _, m := range a.buildContext(sess, "SYS", 0) {
			if s, ok := m.Content.(string); ok {
				b.WriteString(s + "\n")
			}
		}
		return b.String()
	}

	// 规则压缩历史 + 摘要模式：折叠视图照常生效（工具结果/思考不回流）
	llm := join("llm")
	for _, want := range []string{"一轮的问题", "] write 一轮.txt", "一轮的结论", "二轮的问题"} {
		if !strings.Contains(llm, want) {
			t.Fatalf("llm 模式下折叠历史视图缺 %q:\n%s", want, llm)
		}
	}
	for _, ban := range []string{"写入成功1024字节", "结论思考"} {
		if strings.Contains(llm, ban) {
			t.Fatalf("llm 模式下折叠历史不应回流 %q:\n%s", ban, llm)
		}
	}
	if n := strings.Count(llm, "以上为压缩后的历史"); n != 1 {
		t.Fatalf("llm 模式下折叠边界提示应 1 条, got %d:\n%s", n, llm)
	}
	if strings.Index(llm, "以上为压缩后的历史") > strings.Index(llm, "二轮的问题") {
		t.Fatalf("llm 模式下边界提示应在未折叠原文之前:\n%s", llm)
	}
	// 上下文面板口径：同一段历史在两种模式下 token 总量必须一致
	join("rule")
	ruleTotal := a.contextStats(sess).Total
	join("llm")
	llmTotal := a.contextStats(sess).Total
	if ruleTotal != llmTotal {
		t.Fatalf("切换模式不应改变历史组装口径: rule=%d llm=%d", ruleTotal, llmTotal)
	}

	// 对称场景：摘要历史 + 规则模式 → 摘要照常注入、边界内行不进上下文
	s2 := &SessionMeta{ID: "s2", Mode: "task"}
	if err := a.Store.Create(s2); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "s2", "旧一轮", true)  // seq 1-4，被摘要覆盖
	seedRound(t, a, "s2", "旧二轮", false) // seq 5-6，被摘要覆盖
	if err := a.Store.AppendRow("s2", &StoredMessage{
		Message: TextMessage("system", "【手动历史摘要（覆盖至第 6 条）】先前对话的摘要正文。"),
		Kind:    kindSummary, Origin: "manual", CoversUpTo: 6, CreatedAt: "2026-01-02 11:00:00",
	}); err != nil {
		t.Fatal(err)
	}
	seedRound(t, a, "s2", "新一轮", false) // seq 8-9，边界后原文
	a.Cfg.CompactMode = "rule"
	var b strings.Builder
	for _, m := range a.buildContext(s2, "SYS", 0) {
		if s, ok := m.Content.(string); ok {
			b.WriteString(s + "\n")
		}
	}
	rule := b.String()
	for _, want := range []string{"先前对话的摘要正文", "新一轮的问题", "新一轮的结论"} {
		if !strings.Contains(rule, want) {
			t.Fatalf("rule 模式下摘要历史视图缺 %q:\n%s", want, rule)
		}
	}
	for _, ban := range []string{"旧一轮的问题", "旧二轮", "写入成功1024字节"} {
		if strings.Contains(rule, ban) {
			t.Fatalf("rule 模式下已摘要覆盖的原文不应回流 %q:\n%s", ban, rule)
		}
	}
	if n := strings.Count(rule, "以上为压缩后的历史"); n != 0 {
		t.Fatalf("无折叠标记时不应注入折叠边界提示, got %d:\n%s", n, rule)
	}
}

// 二级压缩与边界提示标签：新轮优先一级折叠；同一范围内已没有新轮可折时再点，
// 才升级二级（只留用户输入与模型最终输出）；组装时在折叠边界注入提示（告知
// 模型以上已压缩、细节可查历史原文），全折时提示缀尾。
func TestSecondLevelFoldAndBoundaryNote(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	user1, call1, _, final1 := seedRound(t, a, "s1", "一轮", true)
	_, _, _, final2 := seedRound(t, a, "s1", "二轮", true)
	_, _, _, final3 := seedRound(t, a, "s1", "三轮", false)

	// 一级：压到二轮末（一、二轮首次折叠，调用行=正文+[调用 …]）
	if n, err := a.ruleCompactTo(sess, final2); err != nil || n != 2 {
		t.Fatalf("一级应折 2 轮, n=%d err=%v", n, err)
	}
	if got := rowBySeq(t, a, "s1", call1).Compact; !strings.Contains(got, "] write 一轮.txt") {
		t.Fatalf("一级后调用行应为动作行: %q", got)
	}
	// 组装：折叠边界注入提示，位于折叠内容之后、三轮原文之前
	msgs := a.buildContext(sess, "SYS", 0)
	joined := ""
	for _, m := range msgs {
		if s, ok := m.Content.(string); ok {
			joined += s + "\n"
		}
	}
	if n := strings.Count(joined, "以上为压缩后的历史"); n != 1 {
		t.Fatalf("折叠边界提示应恰好 1 条, got %d:\n%s", n, joined)
	}
	if !strings.Contains(joined, "read_session_messages") {
		t.Fatalf("提示应告知回查工具:\n%s", joined)
	}
	if strings.Index(joined, "以上为压缩后的历史") > strings.Index(joined, "三轮的问题") {
		t.Fatalf("提示应在三轮原文之前:\n%s", joined)
	}

	// 新轮优先一级：压到三轮末 → 只折第三轮（新轮），一、二轮保持一级不跳级
	if n, err := a.ruleCompactTo(sess, final3); err != nil || n != 1 {
		t.Fatalf("有新轮应只折新轮(一级), n=%d err=%v", n, err)
	}
	if got := rowBySeq(t, a, "s1", call1).Compact; !strings.Contains(got, "] write 一轮.txt") {
		t.Fatalf("折新轮不应顺带升二级, 调用行: %q", got)
	}
	// 同一位置再点（范围内已无新轮）：升级二级（只留用户输入与最终输出）。
	// 一、二轮带工具调用有实质变化；三轮没有工具行，一级已是最简，升不了
	if n, err := a.ruleCompactTo(sess, final3); err != nil || n != 2 {
		t.Fatalf("无新轮时应升级二级 2 轮(三轮已最简), n=%d err=%v", n, err)
	}
	if got := rowBySeq(t, a, "s1", call1).Compact; got != compactOmit {
		t.Fatalf("二级后调用行应 omit, got %q", got)
	}
	if got := rowBySeq(t, a, "s1", user1).Compact; got != "一轮的问题" {
		t.Fatalf("二级后用户输入应保留原文, got %q", got)
	}
	if got := rowBySeq(t, a, "s1", final1).Compact; got != "一轮的结论" {
		t.Fatalf("二级后最终输出应保留原文, got %q", got)
	}
	// 再点一次：已二级幂等无变化，报已折叠
	if _, err := a.ruleCompactTo(sess, final3); err == nil || !strings.Contains(err.Error(), "已折叠") {
		t.Fatalf("已二级后重复点应报已折叠, got err=%v", err)
	}
	// 全折（三轮也折了）：提示缀在尾部，仍恰好 1 条
	msgs = a.buildContext(sess, "SYS", 0)
	joined = ""
	for _, m := range msgs {
		if s, ok := m.Content.(string); ok {
			joined += s + "\n"
		}
	}
	if n := strings.Count(joined, "以上为压缩后的历史"); n != 1 {
		t.Fatalf("全折时提示应缀尾 1 条, got %d:\n%s", n, joined)
	}
	if strings.Index(joined, "三轮的结论") >= strings.Index(joined, "以上为压缩后的历史") {
		t.Fatalf("全折时提示应在最后:\n%s", joined)
	}
}

// 压缩后完整重发环境快照（resendEnvAfterCompact）：环境快照对模型是差分
// 机制（首条整份、后续只发变化行），折叠/摘要会把差分所依附的完整基线埋掉
// ——之后模型手里只剩孤立差分行，或缺掉整段环境信息。压缩路径无条件落一条
// 当前完整快照：基线被埋时它成为保留区首条可见快照，组装整份渲染；环境没变
// 时重发行与保留区可见快照同文，差分为空不重复占上下文。
func TestCompactResendsFullEnvSnapshot(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.Providers = nil
	a.Cfg.BaseURL = "mock" // 摘要走 mock 目标，不发真请求
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	// 旧基线快照落在第一轮之前（并入第一轮，折叠时会被 omit 埋掉）
	if err := a.Store.AppendRow("s1", &StoredMessage{
		Message: TextMessage("system", "# 运行环境快照（旧基线）\n- 操作系统：Windows\n"), Kind: kindEnv, CreatedAt: "2026-01-02 10:00:00",
	}); err != nil {
		t.Fatal(err)
	}
	_, _, _, final1 := seedRound(t, a, "s1", "一轮", true)
	_, _, _, final2 := seedRound(t, a, "s1", "二轮", false)
	seedRound(t, a, "s1", "三轮", false)
	joinCtx := func() string {
		joined := ""
		for _, m := range a.buildContext(sess, "SYS", 0) {
			if s, ok := m.Content.(string); ok {
				joined += s + "\n"
			}
		}
		return joined
	}

	// 规则折叠埋掉基线：库里应补落一条未折叠的完整快照行（内容=envPrompt
	// 当前口径），组装输出里整份在场
	if _, err := a.ruleCompactTo(sess, final1); err != nil {
		t.Fatal(err)
	}
	full := a.envPrompt(sess)
	var resent *StoredMessage
	envCount := 0
	raw, err := a.Store.RawMessagesAll("s1")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range raw {
		if m.Kind == kindEnv {
			envCount++
			resent = m
		}
	}
	if envCount != 2 || resent == nil || resent.Compact != "" || resent.Text() != full {
		t.Fatalf("压缩应补落 1 条未折叠的完整快照行(共 %d 条 env), 末条 %+v", envCount, resent)
	}
	if joined := joinCtx(); !strings.Contains(joined, full) {
		t.Fatal("折叠后组装输出应包含完整环境快照（保留区首条可见快照整份渲染）")
	}

	// 环境没变再折一轮：重发行同文于保留区可见快照，差分为空——完整快照
	// 在上下文里只出现一次，不随重发次数累积
	if _, err := a.ruleCompactTo(sess, final2); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(joinCtx(), "# 运行环境快照"); n != 1 {
		t.Fatalf("同文重发应差分为空，完整快照只出现 1 次, got %d", n)
	}

	// 摘要压缩同样重发：mock 目标返回占位摘要，摘要边界埋掉的环境信息由
	// 重发行补回
	if _, err := a.doSummarize(sess, "manual", 1, 0); err != nil {
		t.Fatal(err)
	}
	if joined := joinCtx(); !strings.Contains(joined, full) {
		t.Fatal("摘要压缩后组装输出也应包含完整环境快照")
	}
}

// 保留预算 0 的全压语义：keepStartSeq 返回最后一条 seq+1（保留区为空，连
// 最后一轮也不保留）；间隔触发线随预算收窄（=间隔本身），触发即全量折叠。
func TestKeepCharsZeroFoldsEverything(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.KeepChars = 0
	a.Cfg.CompactInterval = 1000
	a.Cfg.CompactLlmPct = 0 // 专测规则路径
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	u1, _, _, f1 := seedRound(t, a, "s1", "一轮", true)
	_, _, _, f2 := seedRound(t, a, "s1", "二轮", true)
	raw, _ := a.Store.RawMessagesAll("s1")
	last := raw[len(raw)-1].Seq
	if got := a.keepStartSeq(raw); got != last+1 {
		t.Fatalf("KeepChars=0 应返回 last+1（保留区为空）, got %d want %d", got, last+1)
	}
	// 两轮 ≈1740 ≥ 间隔1000+0 → 触发；全压：最后一轮也折（正文按保留规则存视图）
	cp, err := a.maybeAutoCompact(sess, []Message{}, 5000, false)
	if err != nil || !cp {
		t.Fatalf("间隔到线应触发全压, cp=%v err=%v", cp, err)
	}
	for _, seq := range []int64{u1, f1, f2} {
		if got := rowBySeq(t, a, "s1", seq).Compact; got == "" {
			t.Fatalf("全压语义下 seq=%d 也应被折叠（Compact 非空）", seq)
		}
	}
	// API：keepChars=0 提交应被接受并回传（不再 >0 才收）
	a.CfgPath = filepath.Join(t.TempDir(), "config.json")
	body := strings.NewReader(`{"compact":{"mode":"llm","keepChars":0,"interval":0,"llmPct":0}}`)
	w := httptest.NewRecorder()
	a.handleSetConfig(w, httptest.NewRequest("POST", "/api/config", body))
	if w.Code != 200 {
		t.Fatalf("保存应成功, got %d %s", w.Code, w.Body.String())
	}
	var got map[string]any
	_ = json.Unmarshal(w.Body.Bytes(), &got)
	if got["keepChars"] != float64(0) || got["compactInterval"] != float64(0) {
		t.Fatalf("keepChars/interval 的 0 应原样回传, got %v/%v", got["keepChars"], got["compactInterval"])
	}
}

// 输出三段式、结果分派与图片信封卸载的单元测试（逐分支直接覆盖）。
func TestClipOutLinesAndToolResultCompact(t *testing.T) {
	mk := func(n int) string { // 行01…行n（两位零填充避免子串误命中）
		lines := make([]string, n)
		for i := 1; i <= n; i++ {
			lines[i-1] = fmt.Sprintf("行%02d", i)
		}
		return strings.Join(lines, "\n")
	}
	// 成功：头 2 +（中间已裁 12 行）+ 尾 6；中间行（行07）不可见
	out := clipOutLines(mk(20), false, 7)
	if !strings.Contains(out, "行01") || !strings.Contains(out, "行20") || !strings.Contains(out, "中间已裁 12 行") {
		t.Fatalf("三段式头部/尾部/标注不符: %q", out)
	}
	if strings.Contains(out, "行07") {
		t.Fatalf("中间行应被裁掉: %q", out)
	}
	// 失败（exit status）：尾 10 → 中间只裁 8 行，且错误行保留在尾部
	out = clipOutLines(mk(20)+"\nexit status 1", true, 7)
	if !strings.Contains(out, "中间已裁 9 行") || !strings.Contains(out, "exit status 1") {
		t.Fatalf("失败输出应留尾10行含错误: %q", out)
	}
	// 单行巨块（无换行）：行内裁切 150+50，不再原样透传
	out = clipOutLines(strings.Repeat("巨", 5000), false, 7)
	if !strings.Contains(out, "本行已裁") || strings.Contains(out, strings.Repeat("巨", 400)) {
		t.Fatalf("单行超长应行内裁切: %q", clipPreview(out))
	}
	// 短输出原样
	if got := clipOutLines("ok\nPASS", false, 7); got != "ok\nPASS" {
		t.Fatalf("短输出应原样: %q", got)
	}

	row := func() *StoredMessage { return &StoredMessage{Message: Message{Role: "tool", Content: ""}, Seq: 7} }
	// grep 零命中必留（"不存在"的证据）
	if got := toolResultCompact(toolResInfo{name: "grep", text: ""}, row()); got != "→ 无命中" {
		t.Fatalf("grep 零命中应保留, got %q", got)
	}
	// 可再生类省略
	if got := toolResultCompact(toolResInfo{name: "read", text: "文件全文…"}, row()); got != compactOmit {
		t.Fatalf("read 结果应 omit, got %q", got)
	}
	// edit 成败一句话
	if got := toolResultCompact(toolResInfo{name: "edit", text: "已编辑 a.go（1 处替换成功）"}, row()); got != "编辑成功" {
		t.Fatalf("edit 成功应为四字结论, got %q", got)
	}
	if got := toolResultCompact(toolResInfo{name: "edit", text: "工具执行失败: old_string 未匹配"}, row()); !strings.HasPrefix(got, "失败：") {
		t.Fatalf("edit 失败应保留原因, got %q", got)
	}
	// 图片信封 → 卸载标注（附件号+源文件路径+回看通道），信封本体不残留
	env := `<image attachment="att/abc123.png" mediaType="image/png" width="1280" height="800" bytes="123456" path="tmp/shot.png" />`
	got := toolResultCompact(toolResInfo{name: "read_image", text: env + "\n图片已展示给用户"}, row())
	if !strings.Contains(got, "图片已卸载") || !strings.Contains(got, "att/abc123.png") || !strings.Contains(got, "tmp/shot.png") {
		t.Fatalf("信封应替换为卸载标注: %q", got)
	}
	if strings.Contains(got, "<image attachment=") {
		t.Fatalf("信封本体不应残留: %q", got)
	}
	// firstLineOf 零上限防护（防切片下溢 panic）
	if got := firstLineOf("abc", 0); got != "" {
		t.Fatalf("limit 0 应返回空, got %q", got)
	}
}
