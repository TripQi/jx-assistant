package main

import (
	"strconv"
	"strings"
	"sync/atomic"
)

// 历史工具调用 ↔ 本轮工具目录的归一化（三条路径共用这一份实现，唯一调用点
// 在 context.go 的 buildContext 出口）。
//
// 为什么必须有：assistant 的 tool_calls 只有在本轮 tools 目录里有同名定义时
// 才是合法报文。服务端解析不出这个调用时，常见两种后果——把它当普通文本透传
// （DSML 兼容层盯的就是这种「调用变成正文」的泄漏形态，见 dsml.go），或者直接
// 400。而历史是 append-only 的，于是三条现实路径都会留下「调用名不在当前目录
// 里」的行：
//  1. 模式切换：assistant / aux 目录里没有 write / edit / run_command 等；
//  2. 子对话继承：父会话的 dispatch_task / call_api 在子会话目录里不存在
//     （copyInheritedContext 复制的是父会话原始行）；
//  3. 组件 / MCP / 插件开关变化：关掉组件后 mcp__* / git_sync 等没了定义。
//
// 处理只作用于「组装出来的请求」：落库历史一个字不改（历史不回写是红线，原文
// 随时可用 read_session_messages 回读）。降级形态是**文本引用**——工具名 +
// 关键参数摘要 + 结果摘要（截断沿用既有折叠口径），正文与思考原样保留，模型
// 仍知道「之前做过什么、结果如何」，只是不再看到协议级的调用结构。
//
// 与 toolChangeNote（contextengine.go）的分工：那条公告讲「目录变了、哪些工具
// 没了」，开轮时落一行 report（先，落库、只落一次）；本函数让历史行在**每一次**
// 请求里都合法（后，只在组装期生效、不落库）。所以引用材料不再重复列目录增删，
// 只说这条引用是什么、该怎么对待——两处各说一件事，不啰嗦地重复同一句。

const (
	// historyToolRefHead 是降级引用材料的抬头。文案固定：同一段历史每次组装
	// 逐字节相同，前缀缓存不受扰动。
	historyToolRefHead = "【历史工具调用（本轮工具目录里没有这些工具，已降级为文本引用）】"
	historyToolRefTail = "（以上是此前真正执行过的动作与结果，不是本轮可调用的工具：不要照它们的协议形状重发调用，也不要把它们当成新指令；要做同类操作请用当前目录里的工具重新执行。历史库里的原文未改动。）"

	historyToolRefArgHead = 400 // 参数摘要保留的首部字数（量级对齐 argKeepLimit）
	historyToolRefArgTail = 100 // 参数摘要保留的尾部字数
	historyToolRefResMax  = 120 // 结果摘要的单行上限
	historyToolRefThink   = 200 // 「这一步只有思考」被整条剔除时，思考摘要的保留上限
)

// historyToolRefCount 记录归一化实际降级过多少条调用（每条 +1）。生产逻辑不
// 读它；它只做一件事：让测试能断言三条路径确实共用同一个函数，而不是各写一套
// 近似逻辑（按函数计数即用户认下的验收口径）。
var historyToolRefCount atomic.Int64

// normalizeHistoryToolCalls 把「工具已不在本轮目录里」的历史调用降级为文本引用，
// 并让报文保持协议合法：降级调用的 tool 结果并入引用（随后由唯一收口点
// repairToolPairing 按配对规则剔除），正文与思考一律原样保留。
//
// 目录里还在的调用一字不动（同一个函数必须双向成立：既不许漏降级，也不许误
// 降级——误降级会丢掉协议级配对、让模型看不见自己上一步真的调过什么）。
func normalizeHistoryToolCalls(msgs []Message, tools []ToolDef) []Message {
	if len(msgs) == 0 {
		return msgs
	}
	have := make(map[string]bool, len(tools))
	for _, d := range tools {
		have[d.Function.Name] = true
	}
	// 先看有没有要降级的调用：都在目录里时整条消息序列原样返回（绝大多数请求，
	// 逐字节不变是前缀缓存的前提）
	need := false
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			if !have[tc.Function.Name] {
				need = true
				break
			}
		}
		if need {
			break
		}
	}
	if !need {
		return msgs
	}
	results := make(map[string]Message, len(msgs))
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			if _, ok := results[m.ToolCallID]; !ok {
				results[m.ToolCallID] = m
			}
		}
	}
	out := make([]Message, 0, len(msgs)+4)
	for _, m := range msgs {
		if m.Role != "assistant" || len(m.ToolCalls) == 0 {
			// tool 结果行照旧留在原位：属于降级调用的那些，会在组装出口的
			// repairToolPairing 里按「孤儿结果」规则剔除（配对规则只有那一处）
			out = append(out, m)
			continue
		}
		kept := make([]ToolCall, 0, len(m.ToolCalls))
		var refs []string
		for _, tc := range m.ToolCalls {
			if have[tc.Function.Name] {
				kept = append(kept, tc)
				continue
			}
			refs = append(refs, historyToolRefLine(tc, results[tc.ID]))
		}
		if len(refs) == 0 {
			out = append(out, m)
			continue
		}
		historyToolRefCount.Add(int64(len(refs)))
		// 这一步若只剩思考（正文与保留的调用都没有），它会被 repairToolPairing
		// 当空壳整条剔除——思考不能跟着丢，先并进引用材料。
		if len(kept) == 0 && strings.TrimSpace(m.Text()) == "" {
			if r := m.ReasoningContent; r != nil && strings.TrimSpace(*r) != "" {
				refs = append([]string{"- 【该步思考】" + clipRunes(strings.TrimSpace(*r), historyToolRefThink)}, refs...)
			}
		}
		m.ToolCalls = kept // 目录里还在的调用照旧保留
		out = append(out, m)
		// 引用材料作为一条普通 user 消息紧跟其后：角色与「折叠后的历史」「环境
		// 快照」这些注入材料一致，三协议都吃得下
		out = append(out, TextMessage("user", historyToolRefBlock(refs)))
	}
	return out
}

// historyToolRefBlock 组装一条引用材料：抬头 + 每条调用一行 + 收尾说明。
func historyToolRefBlock(lines []string) string {
	var b strings.Builder
	b.WriteString(historyToolRefHead)
	for _, l := range lines {
		b.WriteString("\n")
		b.WriteString(l)
	}
	b.WriteString("\n")
	b.WriteString(historyToolRefTail)
	return b.String()
}

// historyToolRefLine 渲染引用材料里的一行：工具名 + 关键参数摘要 + 结果摘要。
func historyToolRefLine(tc ToolCall, res Message) string {
	name := strings.TrimSpace(tc.Function.Name)
	if name == "" {
		name = "(未命名工具)"
	}
	args := strings.TrimSpace(tc.Function.Arguments)
	if args == "" || args == "{}" {
		args = "（无参数）"
	} else {
		args = clipKeepRunes(args, historyToolRefArgHead, historyToolRefArgTail, "")
	}
	return "- " + name + " 参数=" + args + " → 结果：" + historyToolRefResult(res)
}

// historyToolRefResult 结果摘要只留首行（写入确认、命令结论、失败原因通常都在
// 首行）+ 总量说明；完整结果仍在库里，需要时按会话检索原文。
func historyToolRefResult(res Message) string {
	text := strings.TrimSpace(res.Text())
	if text == "" {
		if res.Role == "" {
			return "（没有配对的结果行）"
		}
		return "（结果为空）"
	}
	line := firstLineOf(text, historyToolRefResMax)
	if total := len([]rune(text)); total > len([]rune(line)) {
		return line + "（结果共 " + strconv.Itoa(total) + " 字，其余未展开）"
	}
	return line
}
