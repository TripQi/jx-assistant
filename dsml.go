package main

// DeepSeek DSML 工具调用兼容层（解析 + 兜底）。
//
// 为什么存在：DeepSeek V4 起在内部用 DSML 序列化工具调用（专用 token + XML
// 结构，规格见权重仓 encoding/README.md）。正常路径由**服务端**把它翻译成
// tool_calls；可一旦服务端解析器缺失/版本不匹配（V4.1 把标签改成了「带空格」
// 形态，V4 那代正则不认），或模型自己写歪（漏开标签、拼成 toolcalls、只剩
// 孤立闭标签），整块标记就会当**普通正文**透传——工具没执行、会话看起来
// 中断，泄漏原文还会被当正常回答入库。
//
// 本文件只做「解析 + 兜底」，四道闸：
//  ① 归一化：竖线两侧的空格、双管 <||DSML||…>、全角竖线 ｜、token 之间带空格、
//     漏开标签的孤儿 invoke、只剩孤立闭标签——统一成 <|DSML|…> 再解析；
//     参考解析器要求「> 后有换行、< 前有换行」，紧凑输出必须也能认。
//  ② markup-only 判定（防误吞正常回答，最关键）：只有**没有结构化 tool_calls**
//     且**剥掉标记后剩余正文 < 60 字**时才当成泄漏——模型在正文里讨论/演示
//     DSML 语法（长代码块 + 说明）时一个字都不许动。
//  ③ 严格解析 + schema 校验：块必须完整闭合、参数名不得重复、string="true"
//     取原值 / string="false" 按 JSON 解析（保字节，不重新编码）；拼成 OpenAI
//     语义的 arguments 后按该工具的 schema 校验（必填齐、无未知参数、类型对）。
//     社区踩过的坑：泄漏调用的参数名会漂移（schema 要 messages 却给了 content），
//     不校验就执行＝执行一个坏调用。
//  ④ 失败不静默：剥掉标记 + 写审计 + 把「语法错误，请用标准 tool_calls 重发」
//     回喂模型（最多 dsmlMaxRetries 次）；仍失败给用户可见的明确报错。泄漏
//     原文任何时候都不进历史库。
//
// 不改任何协议报文与思考回传逻辑（llmproto.go 的三协议回传一行未动）：这里
// 只在「正文聚合完成后」接管主循环的判断。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	// dsmlMarkupOnlyRunes 是「泄漏」判定的分界：剥掉标记后剩余正文少于它才算
	// 工具调用泄漏。放宽会误吞正常回答（模型讨论 DSML 语法时正文很长），收紧
	// 会把「说了半句就调用」的真泄漏漏掉。
	dsmlMarkupOnlyRunes = 60
	// dsmlMaxRetries 是「回喂纠正重发」的次数上限：模型连着写歪三次就不再陪它
	// 玩，给用户可见的报错。
	dsmlMaxRetries = 2
	// dsmlHoldMaxBytes 是流式 hold-back 的硬上限：扣住的内容超过它就原样放行。
	// 真实泄漏的调用块通常几 KB；真有 256 KiB 以上的「疑似标记」多半是模型在写
	// 文档/示例，扣着不吐只会让用户干等——上限保证任何情况都不会卡住整轮。
	dsmlHoldMaxBytes = 256 << 10
)

// ---------- 标记识别（全部书写变体的唯一入口） ----------

// dsmlTag 是识别出的一个 DSML 标记。
type dsmlTag struct {
	Close bool   // </…> 闭标签
	Name  string // 归一化标签名：calls / invoke / parameter
	Attrs string // 开标签属性原文（含前导空格；闭标签为空）
	End   int    // 标记结束偏移（不含，即 '>' 之后）
}

// dsmlCanonicalName 把模型写歪的标签名归一到三种规范名（tool_calls / toolcalls
// 这类写法社区里真实出现过）。
func dsmlCanonicalName(raw string) string {
	n := strings.ToLower(raw)
	n = strings.ReplaceAll(n, "_", "")
	n = strings.ReplaceAll(n, "-", "")
	switch n {
	case "calls", "toolcalls", "functioncalls":
		return "calls"
	case "invoke", "call":
		return "invoke"
	case "parameter", "param", "argument", "arguments":
		return "parameter"
	}
	return ""
}

// dsmlSkipSpaces 跳过标记内部的空白（竖线两侧、token 之间）。
func dsmlSkipSpaces(s string, p int) int {
	for p < len(s) && (s[p] == ' ' || s[p] == '\t') {
		p++
	}
	return p
}

// dsmlBars 吃掉连续的竖线（ASCII 竖线或全角竖线 ｜），返回新位置与个数。
func dsmlBars(s string, p int) (int, int) {
	n := 0
	for p < len(s) {
		if s[p] == '|' {
			p++
			n++
			continue
		}
		if r, size := utf8.DecodeRuneInString(s[p:]); r == '｜' {
			p += size
			n++
			continue
		}
		break
	}
	return p, n
}

// dsmlMatchTag 在 s[i]（必须是 '<'）尝试识别一个 DSML 标记。识别范围覆盖
// 全部已知变体：<|DSML|…>、<||DSML||…>、< | DSML | …>（V4.1 带空格形态）、
// 全角竖线 ｜、闭标签 </ | DSML | …>。识别不出返回 false（照旧当普通文本）。
func dsmlMatchTag(s string, i int) (dsmlTag, bool) {
	if i >= len(s) || s[i] != '<' {
		return dsmlTag{}, false
	}
	p := i + 1
	closeTag := false
	if p < len(s) && s[p] == '/' {
		closeTag = true
		p++
	}
	p = dsmlSkipSpaces(s, p)
	p, bars := dsmlBars(s, p)
	if bars == 0 {
		return dsmlTag{}, false
	}
	p = dsmlSkipSpaces(s, p)
	// DSML 四个字母（大小写不敏感；字母之间容忍空格——token 被拆开写的变体）
	for _, c := range []byte("DSML") {
		if p >= len(s) {
			return dsmlTag{}, false
		}
		b := s[p]
		if b >= 'a' && b <= 'z' {
			b -= 'a' - 'A'
		}
		if b != c {
			return dsmlTag{}, false
		}
		p++
		p = dsmlSkipSpaces(s, p)
	}
	if p, bars = dsmlBars(s, p); bars == 0 {
		return dsmlTag{}, false
	}
	p = dsmlSkipSpaces(s, p)
	// 竖线后面也可能再放一个 '/'（</|DSML|/calls> 这类歪写）
	if p < len(s) && s[p] == '/' {
		closeTag = true
		p++
		p = dsmlSkipSpaces(s, p)
	}
	// 标签名
	start := p
	for p < len(s) {
		b := s[p]
		if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || b == '_' || b == '-' {
			p++
			continue
		}
		break
	}
	name := dsmlCanonicalName(s[start:p])
	if name == "" {
		return dsmlTag{}, false
	}
	if closeTag {
		// 闭标签：允许属性残渣，但必须尽快收尾到 '>'
		for p < len(s) && s[p] != '>' {
			if s[p] == '<' {
				return dsmlTag{}, false
			}
			p++
		}
		if p >= len(s) {
			return dsmlTag{}, false
		}
		return dsmlTag{Close: true, Name: name, End: p + 1}, true
	}
	// 开标签：属性直到 '>'（属性值里的 '>' 由引号保护）
	attrStart := p
	inQuote := byte(0)
	for p < len(s) {
		b := s[p]
		if inQuote != 0 {
			if b == inQuote {
				inQuote = 0
			}
			p++
			continue
		}
		if b == '"' || b == '\'' {
			inQuote = b
			p++
			continue
		}
		if b == '>' {
			return dsmlTag{Name: name, Attrs: s[attrStart:p], End: p + 1}, true
		}
		if b == '<' {
			return dsmlTag{}, false
		}
		p++
	}
	return dsmlTag{}, false
}

// dsmlTagText 把一个标记写回规范形态（归一化的唯一输出形状）。
func dsmlTagText(t dsmlTag) string {
	if t.Close {
		return "</|DSML|" + t.Name + ">"
	}
	return "<|DSML|" + t.Name + t.Attrs + ">"
}

// dsmlNormalize 把正文里的全部 DSML 书写变体统一成规范形态，供解析层只认一种形状。
func dsmlNormalize(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if s[i] != '<' {
			i++
			continue
		}
		tag, ok := dsmlMatchTag(s, i)
		if !ok {
			i++
			continue
		}
		b.WriteString(s[last:i])
		b.WriteString(dsmlTagText(tag))
		last = tag.End
		i = tag.End
	}
	b.WriteString(s[last:])
	return b.String()
}

// dsmlTagPrefixPossible 判断 s（以 '<' 开头）是否可能是某个 DSML 标记的**前缀**
// （含完整标记）：只用于流式 hold-back 的「要不要先扣着」——普通正文里的
// <div>、a<b 之类必须立刻判否，否则正常分行文会被无故延迟。
func dsmlTagPrefixPossible(s string) bool {
	if s == "" || s[0] != '<' {
		return false
	}
	p := 1
	if p < len(s) && s[p] == '/' {
		p++
	}
	p = dsmlSkipSpaces(s, p)
	if p >= len(s) {
		return true // "<" / "</" / "< " 都可能长成标记
	}
	np, bars := dsmlBars(s, p)
	if bars == 0 {
		return false // "<d" 这类不是标记开头
	}
	p = dsmlSkipSpaces(s, np)
	for _, c := range []byte("DSML") {
		if p >= len(s) {
			return true
		}
		b := s[p]
		if b >= 'a' && b <= 'z' {
			b -= 'a' - 'A'
		}
		if b != c {
			return false
		}
		p++
		p = dsmlSkipSpaces(s, p)
	}
	if p >= len(s) {
		return true
	}
	if _, bars = dsmlBars(s, p); bars == 0 {
		return false
	}
	return true // 竖线都齐了：后面是标签名/属性，仍算标记开头
}

// ---------- 结构解析 ----------

// dsmlParam 是一个调用参数。Text=true（string="true"）时 Value 是原样字符串；
// 否则 Value 是原样 JSON 字节片（保字节拼进 arguments，不重新编码）。
type dsmlParam struct {
	Name  string
	Text  bool
	Value string
}

// dsmlInvoke 是一次工具调用（工具名 + 参数）。
type dsmlInvoke struct {
	Name   string
	Params []dsmlParam
}

// dsmlResult 是「把泄漏正文读成调用」的结果。
type dsmlResult struct {
	Calls  []ToolCall // 解析 + schema 校验通过的调用（Err==nil 时才有意义）
	Rest   string     // 剥掉全部标记与调用块后的剩余正文
	Noise  int        // 剥掉的孤立闭标签/游离参数标记数
	Orphan int        // 补造外层包装的孤儿 invoke 块数
	Err    error      // 失败原因（解析或校验）；Rest 已完成剥离
	Tries  []ToolCall // 尽力拼出的调用（校验失败时用于回喂说明，不执行）
}

// dsmlInterpret 是四道闸的②③合并入口：raw 是这一步模型的正文，tools 是本轮
// 工具目录（schema 校验依据），idPrefix 用于合成 tool_call id。
func dsmlInterpret(raw string, tools []ToolDef, idPrefix string) dsmlResult {
	norm := dsmlNormalize(raw)
	res := dsmlResult{Rest: dsmlStripNorm(norm)}
	invokes, noise, orphan, err := dsmlScanInvokes(norm)
	res.Noise, res.Orphan = noise, orphan
	if err != nil {
		res.Err = err
		return res
	}
	if len(invokes) == 0 {
		return res // 只有标记残渣：剥掉即可，不算失败
	}
	res.Tries = dsmlAttempts(invokes, idPrefix)
	calls, err := dsmlValidate(invokes, tools, idPrefix)
	if err != nil {
		res.Err = err
		return res
	}
	res.Calls = calls
	return res
}

// dsmlFindClose 找 from 之后第一个与 name 同名的闭标签，返回它的起止偏移。
func dsmlFindClose(s string, from int, name string) (start, end int, ok bool) {
	for i := from; i < len(s); {
		if s[i] != '<' {
			i++
			continue
		}
		tag, mok := dsmlMatchTag(s, i)
		if !mok {
			i++
			continue
		}
		if tag.Close && tag.Name == name {
			return i, tag.End, true
		}
		i = tag.End
	}
	return 0, 0, false
}

// dsmlBlockBody 取 from 之后、name 闭标签之前的块正文，以及闭标签之后的偏移。
func dsmlBlockBody(s string, from int, name string) (body string, next int, err error) {
	start, end, ok := dsmlFindClose(s, from, name)
	if !ok {
		return "", 0, fmt.Errorf("DSML %s 块没有闭合（缺 </|DSML|%s>）", name, name)
	}
	return s[from:start], end, nil
}

// dsmlScanInvokes 取出全部 invoke 块：外层 calls 块里的、以及漏了外层开标签的
// 孤儿 invoke（补造包装照样收，orphan 计数）；只剩孤立闭标签的当噪声计数。
func dsmlScanInvokes(norm string) (invokes []dsmlInvoke, noise, orphan int, err error) {
	for i := 0; i < len(norm); {
		if norm[i] != '<' {
			i++
			continue
		}
		tag, ok := dsmlMatchTag(norm, i)
		if !ok {
			i++
			continue
		}
		switch {
		case tag.Close:
			noise++ // 没被任何开标签吃掉的闭标签：残渣
			i = tag.End
		case tag.Name == "calls":
			body, next, berr := dsmlBlockBody(norm, tag.End, "calls")
			if berr != nil {
				return nil, noise, orphan, berr
			}
			invs, perr := dsmlParseInvokeList(body)
			if perr != nil {
				return nil, noise, orphan, perr
			}
			invokes = append(invokes, invs...)
			i = next
		case tag.Name == "invoke":
			// 孤儿块：外层 calls 开标签漏了（模型写歪），补造包装后照样收
			body, next, berr := dsmlBlockBody(norm, tag.End, "invoke")
			if berr != nil {
				return nil, noise, orphan, berr
			}
			inv, perr := dsmlParseInvoke(body, tag)
			if perr != nil {
				return nil, noise, orphan, perr
			}
			invokes = append(invokes, inv)
			orphan++
			i = next
		default: // 游离的 parameter 标记等：残渣
			noise++
			i = tag.End
		}
	}
	return invokes, noise, orphan, nil
}

// dsmlParseInvokeList 解析 calls 块正文里的 invoke 序列。
func dsmlParseInvokeList(body string) ([]dsmlInvoke, error) {
	out := []dsmlInvoke{}
	for i := 0; i < len(body); {
		if body[i] != '<' {
			i++
			continue
		}
		tag, ok := dsmlMatchTag(body, i)
		if !ok {
			i++
			continue
		}
		if tag.Close {
			i = tag.End // 多余的闭标签：宽容跳过
			continue
		}
		if tag.Name != "invoke" {
			return nil, fmt.Errorf("calls 块里出现了 <%s>，应为 invoke", tag.Name)
		}
		inner, next, err := dsmlBlockBody(body, tag.End, "invoke")
		if err != nil {
			return nil, err
		}
		inv, err := dsmlParseInvoke(inner, tag)
		if err != nil {
			return nil, err
		}
		out = append(out, inv)
		i = next
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("calls 块里没有 invoke（空块或结构损坏）")
	}
	return out, nil
}

// dsmlParseInvoke 解析单个 invoke 块（块正文 + 开标签属性）。
func dsmlParseInvoke(body string, open dsmlTag) (dsmlInvoke, error) {
	name := strings.TrimSpace(dsmlAttrs(open.Attrs)["name"])
	if name == "" {
		return dsmlInvoke{}, fmt.Errorf("invoke 缺 name 属性（认不出调哪个工具）")
	}
	params := []dsmlParam{}
	seen := map[string]bool{}
	for i := 0; i < len(body); {
		if body[i] != '<' {
			i++
			continue
		}
		tag, ok := dsmlMatchTag(body, i)
		if !ok {
			i++
			continue
		}
		if tag.Close {
			i = tag.End
			continue
		}
		if tag.Name != "parameter" {
			return dsmlInvoke{}, fmt.Errorf("%s 里出现了 <%s>，应为 parameter", name, tag.Name)
		}
		pbody, next, err := dsmlBlockBody(body, tag.End, "parameter")
		if err != nil {
			return dsmlInvoke{}, err
		}
		attrs := dsmlAttrs(tag.Attrs)
		pname := strings.TrimSpace(attrs["name"])
		if pname == "" {
			return dsmlInvoke{}, fmt.Errorf("%s 有 parameter 缺 name 属性", name)
		}
		if seen[pname] {
			// 参数名重复＝语义不明，宁可不执行
			return dsmlInvoke{}, fmt.Errorf("%s 的参数 %q 重复出现（同一调用里参数名不得重复）", name, pname)
		}
		seen[pname] = true
		isText := strings.EqualFold(strings.TrimSpace(attrs["string"]), "true")
		p := dsmlParam{Name: pname, Text: isText, Value: pbody}
		if !isText {
			// string="false"：值是 JSON，保字节收下（拼 arguments 时原样写入）
			p.Value = strings.TrimSpace(pbody)
			if p.Value == "" {
				return dsmlInvoke{}, fmt.Errorf("%s 的参数 %q 声明为 JSON（string=\"false\"）但内容为空", name, pname)
			}
			if !json.Valid([]byte(p.Value)) {
				return dsmlInvoke{}, fmt.Errorf("%s 的参数 %q 声明为 JSON（string=\"false\"）但内容不是合法 JSON", name, pname)
			}
		}
		params = append(params, p)
		i = next
	}
	return dsmlInvoke{Name: name, Params: params}, nil
}

// dsmlAttrs 解析标记属性（tolerate name = "x" 这种松散写法）。
func dsmlAttrs(s string) map[string]string {
	out := map[string]string{}
	i := 0
	for i < len(s) {
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\r' || s[i] == '\n') {
			i++
		}
		start := i
		for i < len(s) {
			b := s[i]
			if (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '_' || b == '-' {
				i++
				continue
			}
			break
		}
		if start == i {
			i++
			continue
		}
		key := s[start:i]
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i >= len(s) || s[i] != '=' {
			out[key] = ""
			continue
		}
		i++
		for i < len(s) && s[i] == ' ' {
			i++
		}
		if i < len(s) && (s[i] == '"' || s[i] == '\'') {
			q := s[i]
			i++
			vstart := i
			for i < len(s) && s[i] != q {
				i++
			}
			out[key] = s[vstart:i]
			if i < len(s) {
				i++
			}
			continue
		}
		vstart := i
		for i < len(s) && s[i] != ' ' {
			i++
		}
		out[key] = s[vstart:i]
	}
	return out
}

// dsmlStripNorm 剥掉规范形态里的全部标记与调用块，只留块外正文（残留标记一律
// 不留：泄漏原文任何时候都不进历史）。未闭合的块（多半是流被截断）连同其后
// 一切一并丢掉——半截调用对模型没有信息价值。
func dsmlStripNorm(s string) string {
	var b strings.Builder
	last := 0
	for i := 0; i < len(s); {
		if s[i] != '<' {
			i++
			continue
		}
		tag, ok := dsmlMatchTag(s, i)
		if !ok {
			i++
			continue
		}
		b.WriteString(s[last:i])
		if tag.Close {
			last, i = tag.End, tag.End
			continue
		}
		_, end, found := dsmlFindClose(s, tag.End, tag.Name)
		if found {
			last, i = end, end
			continue
		}
		if tag.Name == "parameter" {
			last, i = tag.End, tag.End // 游离参数：只丢标记，值留着
			continue
		}
		last, i = len(s), len(s) // 未闭合调用块：其后的内容都不可信
	}
	b.WriteString(s[last:])
	return b.String()
}

// dsmlStripOnly 是「剥掉标记」的对外入口（原始正文 → 剩余正文）。
func dsmlStripOnly(raw string) string { return dsmlStripNorm(dsmlNormalize(raw)) }

// ---------- schema 校验 ----------

// dsmlAttempts 尽力把解析出的调用拼成 ToolCall（arguments 可能不合 schema）；
// 只用于失败时向模型交代「你写了什么」，绝不执行。
func dsmlAttempts(invokes []dsmlInvoke, idPrefix string) []ToolCall {
	out := make([]ToolCall, 0, len(invokes))
	for i, inv := range invokes {
		args, err := dsmlArgsJSON(inv)
		if err != nil {
			args = "{}"
		}
		out = append(out, ToolCall{ID: fmt.Sprintf("%s_retry%d", idPrefix, i+1), Type: "function",
			Function: ToolCallFunc{Name: inv.Name, Arguments: args}})
	}
	return out
}

// dsmlArgsJSON 把参数拼成 OpenAI 语义的 arguments JSON：string="false" 的值
// 原样写入（与模型给的 JSON 逐字一致），string="true" 的值按字符串编码。
func dsmlArgsJSON(inv dsmlInvoke) (string, error) {
	var b strings.Builder
	b.WriteByte('{')
	for i, p := range inv.Params {
		if i > 0 {
			b.WriteByte(',')
		}
		k, err := json.Marshal(p.Name)
		if err != nil {
			return "", err
		}
		b.Write(k)
		b.WriteByte(':')
		if p.Text {
			v, err := json.Marshal(p.Value)
			if err != nil {
				return "", err
			}
			b.Write(v)
			continue
		}
		b.WriteString(p.Value)
	}
	b.WriteByte('}')
	raw := b.String()
	if !json.Valid([]byte(raw)) {
		return "", fmt.Errorf("拼出的 arguments 不是合法 JSON")
	}
	return raw, nil
}

// dsmlValidate 按本轮工具 schema 校验并合成最终 tool_calls。
func dsmlValidate(invokes []dsmlInvoke, tools []ToolDef, idPrefix string) ([]ToolCall, error) {
	specs := make(map[string]ToolSpec, len(tools))
	for _, t := range tools {
		specs[t.Function.Name] = t.Function
	}
	calls := make([]ToolCall, 0, len(invokes))
	for i, inv := range invokes {
		spec, ok := specs[inv.Name]
		if !ok {
			return nil, fmt.Errorf("工具 %q 不在本轮工具目录里（可用：%s）", inv.Name, dsmlToolNameList(specs))
		}
		args, err := dsmlArgsJSON(inv)
		if err != nil {
			return nil, fmt.Errorf("%s 的参数拼装失败：%v", inv.Name, err)
		}
		if err := dsmlValidateArgs(spec.Parameters, args); err != nil {
			return nil, fmt.Errorf("%s 的参数不合 schema：%v", inv.Name, err)
		}
		calls = append(calls, ToolCall{ID: fmt.Sprintf("%s_%d", idPrefix, i+1), Type: "function",
			Function: ToolCallFunc{Name: inv.Name, Arguments: args}})
	}
	if len(calls) == 0 {
		return nil, fmt.Errorf("没有可执行的调用")
	}
	return calls, nil
}

// dsmlValidateArgs 校验必填齐、无未知参数、类型对（schema 没写清楚的部分不猜）。
func dsmlValidateArgs(params map[string]any, argsJSON string) error {
	if len(params) == 0 {
		return nil // 工具没声明 schema：无从校验，交给工具自己解析
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &obj); err != nil {
		return fmt.Errorf("arguments 不是 JSON 对象：%v", err)
	}
	for _, name := range dsmlRequiredNames(params["required"]) {
		if _, ok := obj[name]; !ok {
			return fmt.Errorf("缺少必填参数 %q", name)
		}
	}
	props, _ := params["properties"].(map[string]any)
	allowExtra, hasExtraRule := params["additionalProperties"].(bool)
	for _, k := range dsmlSortedKeysOf(obj) {
		prop, known := props[k]
		if !known {
			if hasExtraRule && allowExtra {
				continue
			}
			return fmt.Errorf("未知参数 %q（schema 只认 %s）", k, dsmlSortedKeysOf(props))
		}
		ps, _ := prop.(map[string]any)
		if len(ps) == 0 {
			continue
		}
		if err := dsmlCheckType(ps["type"], obj[k]); err != nil {
			return fmt.Errorf("参数 %q %v", k, err)
		}
	}
	return nil
}

// dsmlCheckType 按 schema 的 type 声明校验 JSON 值。schema 没写类型或写了认不出
// 的类型名时一律放行（不猜，避免把合法调用判死）。
func dsmlCheckType(typ any, raw json.RawMessage) error {
	want := dsmlTypeNames(typ)
	if len(want) == 0 {
		return nil
	}
	for _, w := range want {
		if dsmlValueIsType(w, raw) {
			return nil
		}
	}
	return fmt.Errorf("类型应为 %s，实际是 %s", strings.Join(want, "|"), dsmlValueKind(raw))
}

// dsmlTypeNames 取 schema 的 type 声明（可能是字符串或字符串数组）。
func dsmlTypeNames(typ any) []string {
	var out []string
	switch v := typ.(type) {
	case string:
		if v != "" {
			out = append(out, v)
		}
	case []any:
		for _, it := range v {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
	}
	// 认不出的类型名会让「类型对」永远判不过，宁可不校验
	for _, n := range out {
		switch n {
		case "string", "number", "integer", "boolean", "array", "object", "null":
		default:
			return nil
		}
	}
	return out
}

func dsmlValueIsType(name string, raw json.RawMessage) bool {
	s := strings.TrimSpace(string(raw))
	switch name {
	case "string":
		return strings.HasPrefix(s, "\"")
	case "boolean":
		return s == "true" || s == "false"
	case "null":
		return s == "null"
	case "array":
		return strings.HasPrefix(s, "[")
	case "object":
		return strings.HasPrefix(s, "{")
	case "number":
		var f float64
		return json.Unmarshal(raw, &f) == nil
	case "integer":
		if json.Unmarshal(raw, new(float64)) != nil {
			return false
		}
		return !strings.ContainsAny(s, ".eE")
	}
	return true
}

func dsmlValueKind(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	switch {
	case strings.HasPrefix(s, "\""):
		return "string"
	case strings.HasPrefix(s, "{"):
		return "object"
	case strings.HasPrefix(s, "["):
		return "array"
	case s == "true" || s == "false":
		return "boolean"
	case s == "null":
		return "null"
	}
	return "number"
}

// dsmlRequiredNames 取 schema 的 required 清单：内置工具在内存里是 []string，
// 从 JSON 解出来的（MCP/插件工具）是 []any——两种都得认，否则必填项校验形同虚设。
func dsmlRequiredNames(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, it := range t {
			if s, ok := it.(string); ok && s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

func dsmlSortedKeysOf[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func dsmlToolNameList(specs map[string]ToolSpec) string {
	names := dsmlSortedKeysOf(specs)
	if len(names) > 12 {
		names = append(names[:12], "…")
	}
	return strings.Join(names, "、")
}

// ---------- ② markup-only 判定 ----------

// dsmlHasMarkup 判断正文里有没有可识别的 DSML 标记。
func dsmlHasMarkup(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if _, ok := dsmlMatchTag(s, i); ok {
			return true
		}
	}
	return false
}

// dsmlContainsToken 大小写不敏感地找 "DSML"（廉价前置过滤：没这个词就不必剥）。
func dsmlContainsToken(s, tok string) bool {
	for i := 0; i+len(tok) <= len(s); i++ {
		if strings.EqualFold(s[i:i+len(tok)], tok) {
			return true
		}
	}
	return false
}

// dsmlLeakLikely 是第二道闸：**没有**结构化 tool_calls，且剥掉标记后剩余正文
// 不足 dsmlMarkupOnlyRunes 字，才算「泄漏的工具调用」。这一条是防误吞的关键：
// 模型在正文里讨论/演示 DSML 语法时剩余正文很长，永远判不过（原文原样保留）。
func dsmlLeakLikely(raw string, structuredCalls int) bool {
	if structuredCalls > 0 {
		return false
	}
	if !dsmlContainsToken(raw, "DSML") || !dsmlHasMarkup(raw) {
		return false
	}
	rest := strings.TrimSpace(dsmlStripOnly(raw))
	return utf8.RuneCountInString(rest) < dsmlMarkupOnlyRunes
}

// ---------- ④ 回喂纠正材料 ----------

// dsmlCorrection 造回喂给模型的纠偏材料：把这轮失败的调用摆成一条带 tool_calls
// 的 assistant 行 + 对应 tool 结果（说清哪里错、该怎么改）。只拼进**本次请求**，
// 不落库——泄漏原文与合成调用都不进历史。
//
// 连工具名都认不出来时退回一条 user 提示（协议里 tool 结果必须挂在某个
// tool_calls 后面，凭空造一条调用反而会教坏模型）。
func dsmlCorrection(res dsmlResult, reason string) []Message {
	hint := "工具调用语法错误：" + reason + "。请改用标准 tool_calls 重新发起（不要输出 DSML/XML 标记；参数名与必填项照工具 schema）。"
	if len(res.Tries) == 0 {
		return []Message{TextMessage("user", hint)}
	}
	out := make([]Message, 0, len(res.Tries)+1)
	out = append(out, Message{Role: "assistant", ToolCalls: res.Tries})
	for _, c := range res.Tries {
		out = append(out, Message{Role: "tool", Content: hint, ToolCallID: c.ID})
	}
	return out
}

// ---------- 流式 hold-back ----------

// dsmlStreamFilter 是流式的**尾部 hold-back**：疑似 DSML 标记的开头先不吐给
// 前端，等能确认（整块收全或排除）再一次性处理——标记被切在多个 chunk 里也
// 不会让用户看到半截标记，也不会重复吐字。
//
// 策略与上限（不能因为等待卡住整轮）：
//   - 只有「可能是一个标记开头」的内容才扣（'<' 之后必须是空白/竖线/DSML 字样），
//     普通正文里的 <div>、a<b 立刻放行，正常流式不受影响；
//   - 整块标记收全后：已放行正文还不到 dsmlMarkupOnlyRunes（60 字）就继续扣住
//     ——这正是「泄漏的工具调用」形态；正文已经够长（模型在讨论/演示语法）就
//     直接放行，不牺牲正常流式；
//   - 硬上限 dsmlHoldMaxBytes（256 KiB）：扣住的量超过它立即原样放行，绝不为了
//     等一个可能永远不来的闭标签让用户干等；
//   - 流结束（Flush）一定放行：调用方拿到扣下的尾巴后再判定去留。
//     因此等待有界：最长等一个块（含上限），且只影响显示时机，不阻塞模型流本身
//     （用户点停止仍走 ctx 取消，与 hold-back 无关）。
type dsmlStreamFilter struct {
	pending string // 已扣下、尚未决定去留的尾部
	emitted int    // 已放行正文的 rune 数（「正文够长＝不是泄漏」的快速判定）
}

// Reset 清空状态（模型调用重试时重新开始）。
func (f *dsmlStreamFilter) Reset() { f.pending, f.emitted = "", 0 }

// Flush 交出仍扣着的尾部（流结束/中断时必须调，保证正文不丢）。
func (f *dsmlStreamFilter) Flush() string {
	out := f.pending
	f.pending = ""
	return out
}

// Feed 吃一个流式增量，返回可以立刻显示的正文（可能为空）。
func (f *dsmlStreamFilter) Feed(chunk string) string {
	f.pending += chunk
	var out strings.Builder
	for {
		if f.pending == "" {
			break
		}
		i := dsmlPotentialStart(f.pending)
		if i < 0 {
			// 没有潜在标记：只扣住末尾「可能长成标记」的那点前缀
			hold := dsmlTailHoldLen(f.pending)
			cut := len(f.pending) - hold
			f.release(&out, f.pending[:cut])
			f.pending = f.pending[cut:]
			break
		}
		f.release(&out, f.pending[:i])
		f.pending = f.pending[i:]
		tag, ok := dsmlMatchTag(f.pending, 0)
		if !ok {
			// 只是标记前缀，还没收全：等下一个增量（扣太多就放行）
			if f.emitted >= dsmlMarkupOnlyRunes || len(f.pending) > dsmlHoldMaxBytes {
				f.release(&out, f.pending)
				f.pending = ""
			}
			break
		}
		if tag.Close {
			// 孤立闭标签也是「标记证据」：先扣住，交给流尾判定
			if f.emitted >= dsmlMarkupOnlyRunes || len(f.pending) > dsmlHoldMaxBytes {
				f.release(&out, f.pending)
				f.pending = ""
			}
			break
		}
		_, end, found := dsmlFindClose(f.pending, tag.End, tag.Name)
		if !found {
			if f.emitted >= dsmlMarkupOnlyRunes || len(f.pending) > dsmlHoldMaxBytes {
				f.release(&out, f.pending)
				f.pending = ""
			}
			break
		}
		if f.emitted >= dsmlMarkupOnlyRunes || end > dsmlHoldMaxBytes {
			// 正文已经够长（不是泄漏形态）或块大到不合理：原样放行
			f.release(&out, f.pending[:end])
			f.pending = f.pending[end:]
			continue
		}
		break
	}
	return out.String()
}

func (f *dsmlStreamFilter) release(out *strings.Builder, s string) {
	if s == "" {
		return
	}
	f.emitted += utf8.RuneCountInString(s)
	out.WriteString(s)
}

// dsmlPotentialStart 返回 s 中最早的「可能是标记开头」的下标（-1=没有）。
func dsmlPotentialStart(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] != '<' {
			continue
		}
		if _, ok := dsmlMatchTag(s, i); ok {
			return i
		}
		if dsmlTagPrefixPossible(s[i:]) {
			return i
		}
	}
	return -1
}

// dsmlTailHoldLen 返回 s 末尾需要扣住的字节数（末尾那个 '<' 之后还可能长成标记）。
func dsmlTailHoldLen(s string) int {
	limit := len(s) - 64
	if limit < 0 {
		limit = 0
	}
	for i := len(s) - 1; i >= limit; i-- {
		if s[i] != '<' {
			continue
		}
		if dsmlTagPrefixPossible(s[i:]) {
			return len(s) - i
		}
		return 0
	}
	return 0
}
