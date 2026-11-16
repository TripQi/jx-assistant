package main

// DSML 兼容层的内部用例：归一化、严格解析、schema 校验、markup-only 判定、
// 回喂材料、流式 hold-back。全部离线（不发行请求）。

import (
	"encoding/json"
	"strings"
	"testing"
)

// dsmlStdSample 是规范形态（> 后换行、< 前换行），编辑一份 config.go。
const dsmlStdSample = `<|DSML|calls>
<|DSML|invoke name="edit">
<|DSML|parameter name="edits" string="false">[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]</|DSML|parameter>
</|DSML|invoke>
</|DSML|calls>`

// dsmlSpacedSample 是 V4.1 起的「带空格」形态——用户实测抓到的样子。
const dsmlSpacedSample = `< | DSML | calls>
< | DSML | invoke name="edit">
< | DSML | parameter name="edits" string="false">[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]</ | DSML | parameter>
</ | DSML | invoke>
</ | DSML | calls>`

const dsmlEditArgs = `{"edits":[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]}`

func dsmlTestTools() []ToolDef {
	return []ToolDef{
		editFileTool().Spec,
		writeFileTool().Spec,
		readFileTool().Spec,
	}
}

// TestDsmlNormalizeVariants 看护第一道闸：双管、全角竖线、带空格 token、
// 紧凑无换行——全部归一成同一份规范形态后解析出同一个调用。
//
// 参考解析器要求「> 后有换行、< 前有换行」，模型紧凑输出时就整块泄漏；
// 这里逐变体验证我们不吃那个脆点。
func TestDsmlNormalizeVariants(t *testing.T) {
	inner := `<|DSML|invoke name="edit">
<|DSML|parameter name="edits" string="false">[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]</|DSML|parameter>
</|DSML|invoke>`
	compactInner := `<|DSML|invoke name="edit"><|DSML|parameter name="edits" string="false">[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]</|DSML|parameter></|DSML|invoke>`
	cases := []struct {
		name string
		raw  string
	}{
		{"标准形态", dsmlStdSample},
		{"带空格形态（V4.1）", dsmlSpacedSample},
		{"紧凑无换行", "<|DSML|calls>" + compactInner + "</|DSML|calls>"},
		{"双管", "<||DSML||calls><||DSML||invoke name=\"edit\"><||DSML||parameter name=\"edits\" string=\"false\">" +
			`[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]` +
			"</||DSML||parameter></||DSML||invoke></||DSML||calls>"},
		{"全角竖线", "<｜DSML｜calls><｜DSML｜invoke name=\"edit\"><｜DSML｜parameter name=\"edits\" string=\"false\">" +
			`[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]` +
			"</｜DSML｜parameter></｜DSML｜ invoke></｜DSML｜ calls>"},
		{"竖线与 token 之间多空格", "<  |  DSML  |  calls><|DSML|invoke name=\"edit\"><|DSML|parameter name=\"edits\" string=\"false\">" +
			`[{"new_string":"新值","old_string":"旧值","path":"D:\\go\\config.go"}]` +
			"</|DSML|parameter></|DSML|invoke></  |  DSML  |  calls>"},
		{"闭标签写成 /|DSML|/invoke", "<|DSML|calls>" + strings.ReplaceAll(inner, "</|DSML|invoke>", "</|DSML|/invoke>") + "</|DSML|calls>"},
		{"标签名写成 tool_calls", strings.ReplaceAll(dsmlStdSample, "|calls>", "|tool_calls>")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			norm := dsmlNormalize(c.raw)
			if !strings.Contains(norm, "<|DSML|calls>") || !strings.Contains(norm, "</|DSML|calls>") {
				t.Fatalf("归一化后不是规范形态：%q", norm)
			}
			res := dsmlInterpret(c.raw, dsmlTestTools(), "call_t")
			if res.Err != nil {
				t.Fatalf("解析失败：%v（归一化后 %q）", res.Err, norm)
			}
			if len(res.Calls) != 1 || res.Calls[0].Function.Name != "edit" {
				t.Fatalf("应解析出 1 个 edit 调用，得到 %+v", res.Calls)
			}
			if res.Calls[0].Function.Arguments != dsmlEditArgs {
				t.Fatalf("arguments 与原 JSON 值不逐字一致：\n得到 %s\n期望 %s", res.Calls[0].Function.Arguments, dsmlEditArgs)
			}
			if strings.TrimSpace(res.Rest) != "" {
				t.Fatalf("纯调用不应留下正文，得到 %q", res.Rest)
			}
		})
	}
}

// TestDsmlParseMultiInvokeOrder 看护「多调用并列 → 2 个 tool_call，顺序与
// 位置对应」：DSML 的结果按位置对应，顺序换了就等于执行错了调用。
func TestDsmlParseMultiInvokeOrder(t *testing.T) {
	raw := `<|DSML|calls>
<|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">a.txt</|DSML|parameter>
<|DSML|parameter name="content" string="true">AAA</|DSML|parameter>
</|DSML|invoke>
<|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">b.txt</|DSML|parameter>
<|DSML|parameter name="content" string="true">BBB</|DSML|parameter>
</|DSML|invoke>
</|DSML|calls>`
	res := dsmlInterpret(raw, dsmlTestTools(), "call_t")
	if res.Err != nil {
		t.Fatalf("解析失败：%v", res.Err)
	}
	if len(res.Calls) != 2 {
		t.Fatalf("应解析出 2 个调用，得到 %d", len(res.Calls))
	}
	for i, want := range []struct{ path, content string }{{"a.txt", "AAA"}, {"b.txt", "BBB"}} {
		var got struct{ Path, Content string }
		if err := json.Unmarshal([]byte(res.Calls[i].Function.Arguments), &got); err != nil {
			t.Fatalf("第 %d 个调用的 arguments 不是 JSON：%v", i+1, err)
		}
		if got.Path != want.path || got.Content != want.content {
			t.Fatalf("第 %d 个调用串了位置：得到 %+v，期望 %+v", i+1, got, want)
		}
		if res.Calls[i].ID == "" {
			t.Fatalf("第 %d 个调用缺 id", i+1)
		}
	}
	if res.Calls[0].ID == res.Calls[1].ID {
		t.Fatalf("两个调用的 id 重复：%q", res.Calls[0].ID)
	}
}

// TestDsmlParseOrphanAndNoise 看护「漏开标签的孤儿 invoke 照样恢复」与
// 「只剩孤立闭标签 → 剥掉 + 计数，不报错」。
func TestDsmlParseOrphanAndNoise(t *testing.T) {
	orphan := `<|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">a.txt</|DSML|parameter>
<|DSML|parameter name="content" string="true">AAA</|DSML|parameter>
</|DSML|invoke>`
	res := dsmlInterpret(orphan, dsmlTestTools(), "call_t")
	if res.Err != nil {
		t.Fatalf("孤儿 invoke 应能恢复：%v", res.Err)
	}
	if len(res.Calls) != 1 || res.Orphan != 1 {
		t.Fatalf("应补造外层包装恢复 1 个调用（orphan=1），得到 calls=%d orphan=%d", len(res.Calls), res.Orphan)
	}

	// 只有孤立闭标签：不报错、不崩，剥干净并记噪声
	res = dsmlInterpret("原文</|DSML|calls>\n尾巴", dsmlTestTools(), "call_t")
	if res.Err != nil {
		t.Fatalf("只剩孤立闭标签不该报错：%v", res.Err)
	}
	if len(res.Calls) != 0 || res.Noise == 0 {
		t.Fatalf("应识别为标记残渣（noise>0），得到 calls=%d noise=%d", len(res.Calls), res.Noise)
	}
	if strings.Contains(res.Rest, "DSML") {
		t.Fatalf("残渣没剥干净：%q", res.Rest)
	}
	if res.Rest != "原文\n尾巴" {
		t.Fatalf("剥标记时不该动正文，得到 %q", res.Rest)
	}

	// 紧凑形态里同时出现孤立闭标签与真调用：闭标签记噪声，调用照样解析
	mixed := "</|DSML|invoke>" + orphan
	res = dsmlInterpret(mixed, dsmlTestTools(), "call_t")
	if res.Err != nil || len(res.Calls) != 1 || res.Noise != 1 {
		t.Fatalf("混合残渣 + 真调用：err=%v calls=%d noise=%d", res.Err, len(res.Calls), res.Noise)
	}
}

// TestDsmlStrictParseFailures 看护第三道闸的「严格」：块必须闭合、参数名不得
// 重复、string="false" 必须是合法 JSON、结构不得错位。
func TestDsmlStrictParseFailures(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"块未闭合", `<|DSML|calls>
<|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">a.txt</|DSML|parameter>
</|DSML|invoke>`, "没有闭合"},
		{"参数名重复", `<|DSML|calls><|DSML|invoke name="write">
<|DSML|parameter name="path" string="true">a.txt</|DSML|parameter>
<|DSML|parameter name="path" string="true">b.txt</|DSML|parameter>
</|DSML|invoke></|DSML|calls>`, "重复出现"},
		{"JSON 参数不合法", `<|DSML|calls><|DSML|invoke name="edit">
<|DSML|parameter name="edits" string="false">[{bad json</|DSML|parameter>
</|DSML|invoke></|DSML|calls>`, "不是合法 JSON"},
		{"calls 里混进别的标记", `<|DSML|calls><|DSML|parameter name="path" string="true">a</|DSML|parameter></|DSML|calls>`, "应为 invoke"},
		{"invoke 缺 name", `<|DSML|calls><|DSML|invoke>
<|DSML|parameter name="path" string="true">a</|DSML|parameter>
</|DSML|invoke></|DSML|calls>`, "缺 name"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := dsmlInterpret(c.raw, dsmlTestTools(), "call_t")
			if res.Err == nil {
				t.Fatalf("这种写法必须解析失败（arg 拼接=%+v）", res.Calls)
			}
			if !strings.Contains(res.Err.Error(), c.want) {
				t.Fatalf("错误原因应含 %q，得到 %v", c.want, res.Err)
			}
			if strings.Contains(res.Rest, "DSML") {
				t.Fatalf("失败路径也必须剥干净标记，得到 %q", res.Rest)
			}
		})
	}
}

// TestDsmlSchemaValidation 看护第三道闸的 schema 校验：社区踩过的坑是泄漏
// 调用的**参数名漂移**（schema 要 path 却给了 content），不校验就会执行一个坏调用。
func TestDsmlSchemaValidation(t *testing.T) {
	mk := func(name string, params ...string) string {
		var b strings.Builder
		b.WriteString(`<|DSML|calls><|DSML|invoke name="` + name + `">`)
		for i := 0; i+1 < len(params); i += 2 {
			b.WriteString(`<|DSML|parameter name="` + params[i] + `" string="true">` + params[i+1] + `</|DSML|parameter>`)
		}
		b.WriteString(`</|DSML|invoke></|DSML|calls>`)
		return b.String()
	}
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"参数名漂移（要 path 给了 content）", mk("write", "content", "正文"), []string{"缺少必填参数", `"path"`}},
		{"未知参数", mk("write", "path", "a.txt", "content", "x", "message", "y"), []string{"未知参数", `"message"`}},
		{"未知工具", mk("no_such_tool", "path", "a.txt"), []string{"不在本轮工具目录里"}},
		{"类型不对", `<|DSML|calls><|DSML|invoke name="write">
<|DSML|parameter name="path" string="false">123</|DSML|parameter>
<|DSML|parameter name="content" string="true">x</|DSML|parameter>
</|DSML|invoke></|DSML|calls>`, []string{"类型应为 string"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := dsmlInterpret(c.raw, dsmlTestTools(), "call_t")
			if res.Err == nil {
				t.Fatalf("必须校验不通过，却解析出了调用：%+v", res.Calls)
			}
			for _, w := range c.want {
				if !strings.Contains(res.Err.Error(), w) {
					t.Fatalf("错误原因应含 %q，得到 %v", w, res.Err)
				}
			}
			if len(res.Calls) != 0 {
				t.Fatalf("校验失败绝不能产出可执行调用，得到 %+v", res.Calls)
			}
			if len(res.Tries) == 0 {
				t.Fatal("失败时要留下「模型写了什么」用于回喂说明")
			}
		})
	}

	// 正例对照：必填齐、类型对 → 通过
	ok := dsmlInterpret(mk("write", "path", "a.txt", "content", "正文"), dsmlTestTools(), "call_t")
	if ok.Err != nil || len(ok.Calls) != 1 {
		t.Fatalf("合法调用必须通过：err=%v calls=%d", ok.Err, len(ok.Calls))
	}
}

// TestDsmlLeakGate 看护第二道闸（防误吞正常回答）：没有结构化调用 + 剥标记后
// 剩余正文 < 60 字才算泄漏；正文里讨论/演示 DSML 语法的一律不算。
func TestDsmlLeakGate(t *testing.T) {
	if !dsmlLeakLikely(dsmlStdSample, 0) {
		t.Fatal("纯调用块（无正文）必须判为泄漏")
	}
	if dsmlLeakLikely(dsmlStdSample, 1) {
		t.Fatal("有结构化 tool_calls 时绝不判泄漏（交给既有链路）")
	}
	discussion := "DSML 是 DeepSeek 内部使用的工具调用格式。真正的请求里它会被服务端翻译成 tool_calls，" +
		"但服务端解析器缺失时整块会当普通正文透传。它的结构大致是：\n\n" +
		dsmlStdSample + "\n\n要点：带空格形态是 V4.1 之后的新写法，" +
		"我们归一化之后才能解析；同时正文里必须有足够多的说明文字，才不会被当成泄漏吞掉。"
	if dsmlLeakLikely(discussion, 0) {
		t.Fatal("正文讨论 DSML 语法（残余正文远大于 60 字）绝不能判为泄漏")
	}
	// 只有很短的前后文时仍算泄漏（真实现场：说一句「我来改」就吐标记）
	short := "我来改这个文件。" + dsmlStdSample
	if !dsmlLeakLikely(short, 0) {
		t.Fatal("短正文 + 完整标记应判为泄漏")
	}
	// 没有任何标记的普通正文永不判泄漏
	if dsmlLeakLikely("普通回答里提到 DSML 这个词但没有任何标记", 0) {
		t.Fatal("没有标记时不该判泄漏")
	}
}

// TestDsmlCorrectionFeedback 看护第四道闸的回喂材料：能拼出调用时用
// assistant(tool_calls)+tool 结果（让协议自洽），拼不出时退回一条 user 提示。
func TestDsmlCorrectionFeedback(t *testing.T) {
	bad := `<|DSML|calls><|DSML|invoke name="write">
<|DSML|parameter name="content" string="true">正文</|DSML|parameter>
</|DSML|invoke></|DSML|calls>`
	res := dsmlInterpret(bad, dsmlTestTools(), "call_t")
	if res.Err == nil {
		t.Fatal("前置条件：这条必须是失败用例")
	}
	msgs := dsmlCorrection(res, res.Err.Error())
	if len(msgs) < 2 || msgs[0].Role != "assistant" || len(msgs[0].ToolCalls) == 0 {
		t.Fatalf("回喂应以 assistant(tool_calls) 开头，得到 %+v", msgs)
	}
	if msgs[1].Role != "tool" || msgs[1].ToolCallID != msgs[0].ToolCalls[0].ID {
		t.Fatalf("tool 结果必须挂在对应调用后：%+v", msgs[1])
	}
	if !strings.Contains(msgs[1].Text(), "标准 tool_calls") || !strings.Contains(msgs[1].Text(), "不要输出 DSML") {
		t.Fatalf("提示词要说清怎么改，得到 %q", msgs[1].Text())
	}
	if strings.Contains(msgs[1].Text(), "<|DSML|") {
		t.Fatalf("回喂的提示里不能再示范标记原文，得到 %q", msgs[1].Text())
	}

	// 连工具名都认不出来：退回 user 提示（不能凭空造一条 tool 结果）
	bare := dsmlInterpret("</|DSML|calls>", dsmlTestTools(), "call_t")
	msgs = dsmlCorrection(bare, "只有标记残渣")
	if len(msgs) != 1 || msgs[0].Role != "user" {
		t.Fatalf("认不出调用时退回一条 user 提示，得到 %+v", msgs)
	}
}

// TestDsmlStreamFilterHoldBack 看护流式 hold-back：标记被切在多个 chunk 里
// 时既不能漏显半截标记，也不能重复/丢正文。
func TestDsmlStreamFilterHoldBack(t *testing.T) {
	raw := "我来改这个文件：" + dsmlStdSample + "改完了"
	f := &dsmlStreamFilter{}
	var shown strings.Builder
	for _, c := range dsmlSplitRunes(raw, 5) {
		shown.WriteString(f.Feed(c))
		if strings.Contains(shown.String(), "DSML") {
			t.Fatalf("hold-back 漏出了标记碎片：%q", shown.String())
		}
	}
	tail := f.Flush()
	if got := shown.String() + tail; got != raw {
		t.Fatalf("正文有丢字或重复：\n得到 %q\n原始 %q", got, raw)
	}
	if shown.String() != "我来改这个文件：" {
		t.Fatalf("标记之前的小段正文应正常流出，得到 %q", shown.String())
	}

	// 正文已经足够长（模型在讨论语法）：整块立刻放行，不牺牲正常流式
	long := strings.Repeat("这是一段很长的说明文字。", 8)
	f.Reset()
	var shown2 strings.Builder
	for _, c := range dsmlSplitRunes(long+dsmlStdSample+"尾巴", 9) {
		shown2.WriteString(f.Feed(c))
	}
	if !strings.Contains(shown2.String(), "DSML") {
		t.Fatal("正文够长时不该继续扣住整块（那是正常回答里的示例，不是泄漏）")
	}
	if got := shown2.String() + f.Flush(); got != long+dsmlStdSample+"尾巴" {
		t.Fatalf("长正文场景丢字/重复：%q", got)
	}

	// 没有标记的普通正文：一个字都不留
	f.Reset()
	var shown3 strings.Builder
	for _, c := range dsmlSplitRunes("普通正文，没有标记，结尾带个 < 符号", 3) {
		shown3.WriteString(f.Feed(c))
	}
	if got := shown3.String() + f.Flush(); got != "普通正文，没有标记，结尾带个 < 符号" {
		t.Fatalf("普通正文被改了：%q", got)
	}
}

// dsmlSplitRunes 按 rune 数把串切成小块（模拟 SSE 把标记切碎）。
func dsmlSplitRunes(s string, n int) []string {
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
