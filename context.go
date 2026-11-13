package main

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 上下文管理：组装管线（系统提示词 + 活跃摘要 + 规则压缩视图 + 近期原文）
// 与两种压缩机制——规则压缩（间隔制，零模型调用）与模型摘要（自动阈值 + 手动按钮）。
// 设计对齐 DSH 的哲学：原始历史 append-only 永久保留，折叠只发生在
// 「模型可见表面」上；模型随时可用历史查询工具读到未压缩原文。

const compactOmit = "omit" // 规则压缩的特殊标记：组装时整行跳过

// argKeepLimit 是模型摘要（doSummarize）拼装待压素材时工具参数的截断上限。
const argKeepLimit = 500

// ---------- 规则折叠 v2（间隔制，2026-11 规格：动作行 + 诚实裁剪标注 + seq 锚点） ----------
//
// 调用行折叠为「动作行」：[seq] 工具 目标 —— 意图。seq 是数据库主键，
// read_session_messages(seq=N) 一步取回完整原文；工具结果按类型分派
// （命令输出三段式、写入类成败一句话、grep 命中摘要、图片卸载标注），
// 每处裁剪都标注「（中间已裁 N 字 · 全文取 seq N）」——模型既能判断
// 要不要回查，也知道回查入口。
const (
	argKeepLong   = 2000 // edit/write 参数原样保留上限（rune），超过才头尾保留
	argKeepHead   = 1600 // 超限时的头部保留量
	argKeepTail   = 400  // 超限时的尾部保留量
	cmdKeepLimit  = 500  // run_command 命令文本原样保留上限
	cmdKeepHead   = 200  // 命令超限时的头部保留量
	cmdKeepTail   = 100  // 命令超限时的尾部保留量
	outHeadLines  = 2    // 命令/脚本输出的头部保留行数
	outTailLines  = 6    // 尾部保留行数（成功）
	outTailFail   = 10   // 尾部保留行数（失败——错误堆栈在尾部）
	digestLimit   = 60   // 动作行「目标」字段的截断上限
	intentLimit   = 40   // 动作行「意图」字段的截断上限
	resLineLimit  = 80   // 一行结论类结果的截断上限
	lineKeepLimit = 300  // 输出单行的原样保留上限（无换行巨块/压缩 JSON 行）
	lineKeepHead  = 150  // 行内超长时的首部保留量
	lineKeepTail  = 50   // 行内超长时的尾部保留量
)

// foldBoundaryNote 是折叠边界的提示标签：组装时注入在「已折叠区 → 原文区」
// 的交界处（全折时缀在尾部），告知模型上方是压缩视图、动作行里的编号就是
// 数据库 seq、未列出编号的段落是已删除的思考块。文案是常量——同一段历史的
// 组装逐字节稳定，前缀缓存不受扰动。
const foldBoundaryNote = "（以上为压缩后的历史：保留用户输入与模型结论原文；工具调用简化为动作行，方括号里的编号是数据库 seq，可用 read_session_messages(seq=N) 取回该行完整原文（参数与结果在相邻行）；凡未列出编号的段落均为已删除的思考块，不留条目。）"

// compactNotePrefix 是每次规则压缩完成时落库的「压缩区结束+账本」report 行
// 前缀，用于识别历史账本行以推算压缩次数。
const compactNotePrefix = "══════ 压缩区结束（第 "

// round 是一轮对话：一条用户消息（chat/aiuser/report）到下一条用户消息之前的全部。
type round struct {
	startSeq, endSeq int64
}

// splitRounds 按用户消息分轮；开头若无用户消息（不应发生），并入第一轮。
// 继承标记行（inherit）不是对话内容，跳过——否则它会自成伪轮，
// 让子对话的压缩时机比同样长度的普通会话早一轮。
// aiuser（AI 发出的用户消息）与真人用户消息同权开轮：派发指令/派发回报
// 落地即开启一个新回合（前端同样按回合收口、给上一回合带出操作按钮）。
func splitRounds(msgs []*StoredMessage) []round {
	rounds := []round{}
	cur := round{}
	started := false
	for _, m := range msgs {
		if m.Kind == inheritKind {
			continue
		}
		if m.Kind == "chat" && m.Role == "user" || m.Kind == "report" || m.Kind == kindAiUser {
			if started {
				cur.endSeq = m.Seq - 1
				if cur.endSeq < cur.startSeq {
					cur.endSeq = cur.startSeq
				}
				rounds = append(rounds, cur)
			}
			cur = round{startSeq: m.Seq}
			started = true
			continue
		}
		if !started {
			cur = round{startSeq: m.Seq}
			started = true
		}
	}
	if started {
		if len(msgs) > 0 {
			cur.endSeq = msgs[len(msgs)-1].Seq
		}
		rounds = append(rounds, cur)
	}
	return rounds
}

// activeSummaries 取未过期的摘要：手动摘要全部保留（叠加、永不重压），
// 自动摘要只取最新一条（DSH 式：新自动摘要合并旧自动摘要）。
func activeSummaries(msgs []*StoredMessage) []*StoredMessage {
	manual := []*StoredMessage{}
	var auto *StoredMessage
	for _, m := range msgs {
		if m.Kind != "summary" || m.Superseded {
			continue
		}
		if m.Origin == "manual" {
			manual = append(manual, m)
		} else if auto == nil || m.Seq > auto.Seq {
			auto = m
		}
	}
	out := manual
	if auto != nil {
		out = append(out, auto)
	}
	// 按覆盖边界升序，保证注入顺序与时间线一致
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].CoversUpTo < out[j-1].CoversUpTo; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// buildContext 是组装管线：系统提示词 + 活跃摘要 + 边界后消息
// （规则压缩视图生效，最近轮保持原文），并修剪结尾悬空的工具调用。
// 组装只认压缩的事实——行上的 Compact 标记与摘要覆盖边界无条件应用，
// 与设置里的压缩模式无关：压缩发生了就是发生了，永久生效；模式只决定
// 未来压缩走哪条路（maybeAutoCompact/overflowCompact/handleCompact 选路），
// 切换模式绝不能让已压缩的历史按原始消息整包重组。
// 环境快照行（kind=env）按落位进上下文，**追加式**（已发送的前缀绝不改写）：
// 可见的首条快照整份呈现（摘要折叠后以边界后首条重新作为完整基线），
// 其后相邻快照按差分形状分流——纯新增渲染为短的「环境更新」补充，条目有减
// 或同 key 换值渲染为【移除】/【新增】变更清单，差分连续数达到阈值后完整
// 重发一次校准（见 envUpdateMessage / envRowRender）——变化永远以新消息
// 出现在尾部，系统提示词与历史前缀逐字节稳定，前缀缓存命中最大化。
// buildContextUpTo 与 buildContext 同一管线，唯一差别：upTo>0 时行循环在
// upTo 处截止（Seq 超过的行整段跳过——消息与环境差分同步截止，组装出的
// 消息序列与主循环请求逐字节同源）。doSummarize 的摘要请求用它取「压缩
// 终点为止」的消息序列：前缀缓存直接命中整段压缩区间，摘要调用只有尾部
// 压缩指令是新内容（DSH 社区同思路实测命中可到 99%+）。
func (a *App) buildContextUpTo(sess *SessionMeta, sys string, upTo int64) []Message {
	return a.buildContext(sess, sys, upTo)
}

func (a *App) buildContext(sess *SessionMeta, sys string, upTo int64) []Message {
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return []Message{TextMessage("system", sys)}
	}
	if upTo > 0 {
		// 截止到 upTo（摘要请求的压缩终点）：对话行与环境差分行照裁——它们
		// 在「upTo 时刻的主循环请求」里本来就只到 upTo 为止，裁完的序列与
		// 那次请求逐字节同源，前缀缓存命中的正是那一段。摘要行豁免：它只是
		// 落库位置靠后的元数据（covers_up_to 才是它的逻辑位置），主循环请求
		// 里始终整段在前缀头部，裁掉反而与主循环分叉。
		kept := make([]*StoredMessage, 0, len(raw))
		for _, m := range raw {
			if m.Seq > upTo && m.Kind != "summary" {
				continue
			}
			kept = append(kept, m)
		}
		raw = kept
	}
	summaries := activeSummaries(raw)
	boundary := int64(0)
	for _, s := range summaries {
		if s.CoversUpTo > boundary {
			boundary = s.CoversUpTo
		}
	}
	out := make([]Message, 0, len(raw)+2)
	out = append(out, TextMessage("system", sys))
	for _, s := range summaries {
		out = append(out, TextMessage("user", s.Text()))
	}
	prevEnv := ""     // 上一条可见快照；空 = 下一条整份呈现（首轮，或摘要折叠后的首条）
	envUpdates := 0   // 自上一条整份/全量快照以来的连续差分数（自愈全量的计数）
	foldSeen := false // 已见过折叠行：遇到首条未折叠可见行时在交界注入提示标签
	for _, m := range raw {
		if m.Seq <= boundary || m.Kind == "summary" || m.Kind == "prompt" || m.Kind == "inherit" || m.Kind == "request" || m.Kind == "audit" {
			continue
		}
		if m.Compact == "" && foldSeen {
			out = append(out, TextMessage("user", foldBoundaryNote))
			foldSeen = false
		}
		if m.Compact != "" {
			foldSeen = true
		}
		// 折叠丢弃的行先跳过——含压缩区内的环境快照（不再走差分渲染，
		// 保留区首个可见快照自然成为整份基线）
		if m.Compact == compactOmit {
			continue
		}
		if m.Kind == "env" {
			env := m.Text()
			if s, n := envRowRender(prevEnv, env, envUpdates); s != "" {
				// 首条整份 / 差分（纯新增或【移除】【新增】变更清单）/ 计数到顶完整重发
				out = append(out, TextMessage("user", s))
				envUpdates = n
			}
			prevEnv = env
			continue
		}
		if m.Compact != "" {
			// 压缩表示：纯文本消息，不带 tool_calls（协议要求配对完整）。
			// 用户侧输入的折叠视图与原文一致，时间提示词照常派生
			out = append(out, Message{Role: m.Role, Content: m.Compact})
			out = append(out, a.timeAfter(m)...)
			continue
		}
		// 空正文且无工具调用（例如只留下思考过程的行）：对模型没有信息，
		// 直接跳过，避免发出空 content 被网关拒绝
		if m.Role == "assistant" && strings.TrimSpace(m.Text()) == "" && len(m.ToolCalls) == 0 {
			continue
		}
		msg := m.Message
		// 思考模式历史回传：DeepSeek V4/flash 带 tools 的请求要求此前回合的
		// reasoning_content 原样传回（见 Message 结构注释），落库的 thinking 正是它
		if m.Role == "assistant" && m.Thinking != "" {
			t := m.Thinking
			msg.ReasoningContent = &t
		}
		out = append(out, msg)
		// 时间提示词（独立时间通道）：每条用户侧对话消息——真人用户（chat）与
		// AI 发出的用户消息（aiuser）——后面跟一条只含发送时刻的短 user 消息。
		// 组装期从该消息自身的落库时刻派生，**不落库**（历史查询工具与前端消息
		// 流都看不到噪音行）；同一份存储重复组装逐字节相同，前缀缓存不受影响。
		// 环境快照因此不再含时间行，用户发消息不再连带触发一条「环境更新」。
		out = append(out, a.timeAfter(m)...)
	}
	// 整段可见历史都已折叠（按钮把最后一轮也折了）：提示标签缀在尾部
	if foldSeen {
		out = append(out, TextMessage("user", foldBoundaryNote))
	}
	// 全序列修复工具调用配对（协议硬要求；残缺可能出现在任何位置，见函数注释）
	return repairToolPairing(out)
}

// timeAfter 是时间提示词通道的组装形态：用户侧对话消息（chat 真人发言与
// aiuser 派发指令/回报）后面跟的那条时间消息。返回单条或空——时刻解析不出
// （理论上不发生，所有行落库都带 now() 时间戳）时宁缺毋滥。
// 内容取该消息自己的落库时刻（用户在哪一刻说的话就记哪一刻），分钟粒度与
// 旧环境快照时间行一致；不是挂钟，模型自己执行期间不会凭空多出新的时间消息。
func (a *App) timeAfter(m *StoredMessage) []Message {
	if m.Role != "user" || (m.Kind != kindChat && m.Kind != kindAiUser) {
		return nil
	}
	t, err := time.ParseInLocation("2006-01-02 15:04:05", strings.TrimSpace(m.CreatedAt), time.Local)
	if err != nil {
		return nil
	}
	return []Message{TextMessage("user", "（发送时间："+t.Format("2006-01-02 15:04")+"）")}
}

// repairToolPairing 修复工具调用配对。OpenAI 兼容网关硬校验「assistant 的每个
// tool_calls 调用都必须紧跟一条对应的 tool 响应，且 tool 响应必须有对应调用」，
// 违反一律 400（"must be followed by tool messages"）。残缺配对不只出现在尾部：
// 客户端中断在中间轮、进程崩溃/服务重启的恢复路径（repairDanglingToolCalls 补的
// 合成结尾落在历史尾部、而调用在中段）、规则压缩边界都可能留下悬空调用或孤儿
// 响应。这里做全序列视图修复（消费式重排）：
//   - 已答复的调用：响应重排为**紧跟**其 assistant 消息之后（按 calls 顺序）。
//     历史里响应本就紧随调用时，重排与原顺序逐字节等价（KV 前缀缓存不受影响）；
//     只有兜底补的合成行落在尾部这种错位场景才真正移动位置；
//   - 无响应的 tool_call 从 assistant 上剔除；清空后若有正文保留为普通消息，
//     纯调用且无正文则整条剔除；
//   - 孤儿 tool 响应（无对应调用或缺少 id）整条丢弃。
//
// 不改落库数据，未配对的原文仍可经历史查询工具读到。
func repairToolPairing(msgs []Message) []Message {
	replies := map[string]Message{} // 调用 id → 响应（有响应才进表）
	for _, m := range msgs {
		if m.Role == "tool" && m.ToolCallID != "" {
			replies[m.ToolCallID] = m
		}
	}
	fixed := make([]Message, 0, len(msgs))
	for _, m := range msgs {
		switch {
		case m.Role == "assistant" && len(m.ToolCalls) > 0:
			var kept []ToolCall
			for _, tc := range m.ToolCalls {
				if _, ok := replies[tc.ID]; !ok {
					continue
				}
				kept = append(kept, tc)
			}
			if len(kept) == 0 && strings.TrimSpace(m.Text()) == "" {
				continue // 纯调用且全部无响应：整条剔除
			}
			m.ToolCalls = kept // 全无响应但有正文 → nil，退化为普通文本消息
			fixed = append(fixed, m)
			// 响应紧跟（按 calls 顺序）——原位置的响应行在后面遇到时跳过
			for _, tc := range kept {
				fixed = append(fixed, replies[tc.ID])
			}
		case m.Role == "tool":
			continue // 响应已随其调用带出；走到这里的只剩已消费/孤儿/缺 id 的响应
		default:
			fixed = append(fixed, m)
		}
	}
	return fixed
}

// envFullResendAfter 差分自愈阈值：自上一条整份/全量快照以来已连续发出这么多条
// 差分更新后，下一次快照有变化时改发完整重发（fullResendEnv）重新校准——防止
// 长会话里差分层层叠加（每条都要求模型自行合并进旧状态），合并误差越滚越大。
const envFullResendAfter = 5

// envUpdateMessage 把相邻两份环境快照渲染成发给模型的消息，按差分形状分流：
//   - 纯新增条目 → 短「环境更新」只列新增行（叠加语义，旧快照仍然有效，
//     前缀缓存友好）；
//   - 条目有减（被移除，含同 key 换值的旧值行）→ 「变更清单」：中文标签
//     【移除】/【新增】成对列出（换值条目 = 同 key 一移一增，两行都给整行
//     原文，模型按原文精确匹配作废/并入即可，不做「旧→新」内联压缩）。
//     不用 +/- 记号：快照条目行本身以 "- "（markdown 列表符）开头，diff 记号
//     会与条目行首撞车——"- 权限模式：…"分不清是新增条目还是删除标记；
//     中文标签每行多两三个字符，语义无歧义。差分连续数达到阈值时由组装层
//     （envRowRender）改发完整重发，本函数不感知计数。
//
// 确定性纯函数（存储文本唯一决定输出），同文快照的标题行会被精确匹配自然滤掉。
// 纯注记行（非 "- " 条目，如技能变更说明）过期后静默消失不进变更清单，
// 新出现的注记行按新增进补充消息。
func envUpdateMessage(prev, cur string) string {
	prevCount := map[string]int{}
	for _, l := range strings.Split(prev, "\n") {
		prevCount[l]++
	}
	curCount := map[string]int{}
	for _, l := range strings.Split(cur, "\n") {
		curCount[l]++
	}
	added := []string{} // 新快照里有、旧快照里没有的行（含换值后的新值行）
	for _, l := range strings.Split(cur, "\n") {
		if prevCount[l] > 0 {
			prevCount[l]--
			continue // 整行未变
		}
		added = append(added, l)
	}
	removed := []string{} // 旧快照里有、新快照里没了的 "- " 条目行（含被换值的旧值）
	for _, l := range strings.Split(prev, "\n") {
		if curCount[l] > 0 {
			curCount[l]--
			continue // 整行未变（对侧消费计数）
		}
		if strings.HasPrefix(strings.TrimSpace(l), "- ") {
			removed = append(removed, l) // 注记行等非条目行静默消失
		}
	}
	if len(removed) > 0 {
		// 变更清单：【移除】在早前快照中整行作废、【新增】为最新状态
		var b strings.Builder
		b.WriteString("# 环境更新（条目有增删/变更：移除行 = 早前快照中该条目整行作废，新增行 = 最新状态，均给整行原文；连续差分积累多了系统会自动完整重发一次校准）\n")
		for _, l := range removed {
			b.WriteString("【移除】" + l + "\n")
		}
		for _, l := range added {
			b.WriteString("【新增】" + l + "\n")
		}
		return strings.TrimRight(b.String(), "\n")
	}
	if len(added) == 0 {
		return "" // 完全同文（防御：落库层等值比较已保证不会走到这）
	}
	sort.Strings(added) // 无序收集，排序保证确定性
	var b strings.Builder
	b.WriteString("# 环境更新（新增条目，叠加在更早的快照与更新之上；条目减少或变更时改发增删变更清单）\n")
	for _, l := range added {
		b.WriteString(l + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// envRowRender 组装层渲染一条 kind=env 行的统一口径（buildContext 与 ctxStats
// 共用）：prevEnv 为空 = 整份基线（首轮，或摘要折叠后保留区的首条可见快照），
// 差分计数清零；差分连续数达到 envFullResendAfter = 完整重发重新校准，计数
// 清零；否则走 envUpdateMessage 差分，计数 +1。返回消息文本（空 = 不发，同文
// 防御）与新的差分计数。
func envRowRender(prevEnv, env string, updates int) (string, int) {
	if prevEnv == "" {
		return env, 0
	}
	if updates >= envFullResendAfter {
		return fullResendEnv(env), 0
	}
	if s := envUpdateMessage(prevEnv, env); s != "" {
		return s, updates + 1
	}
	return "", updates
}

// fullResendEnv 把新快照渲染成一次完整重发：正文用新快照原文，首行标题换成
// 「完整重发」声明，取代此前所有快照与差分更新（模型无需再回头合并旧文）。
func fullResendEnv(cur string) string {
	body := cur
	if i := strings.IndexByte(cur, '\n'); i >= 0 && strings.HasPrefix(cur, "# ") {
		body = cur[i+1:]
	}
	return "# 运行环境快照（完整重发，取代更早的快照与更新）\n" + strings.TrimRight(body, "\n")
}

// envLineKey 取环境快照行的条目键："- key：value" 形式取 key（同条目的新旧
// 取值归并），其余行整行作键；空行返回空串。
func envLineKey(l string) string {
	s := strings.TrimSpace(l)
	if !strings.HasPrefix(s, "- ") {
		return s
	}
	rest := s[len("- "):]
	if i := strings.Index(rest, "："); i > 0 {
		return rest[:i]
	}
	return s
}

// ---------- 子对话的上下文继承（分支克隆） ----------

// inheritMark 是继承标记行的正文（前端只需 count 来切分继承段与自有消息）。
type inheritMark struct {
	Parent string `json:"parent"`
	Count  int    `json:"count"`
}

// trimOpenToolGroup 剥掉尾部未闭合的工具调用组，返回可安全复制的前缀。
//
// 为什么只需要这一处截断：落库时机是「这一步结束」——模型正在流式输出的那一步
// 根本不落库（被打断才写半截带中断标注），工具结果也只在工具跑完时落。但
// **assistant 那一行（含 tool_calls）在模型这一步输出完就落库了**，工具随后才逐个
// 执行、逐个落结果。于是从「模型输出完成」到「最后一个工具结果回来」之间，库里
// 存在「有 tool_calls、对应 tool 结果还没齐」的半开组（实测：父会话 01:26:38 落了
// 带 tool_call 的 assistant，它的结果 01:28:09 才落，中间 91 秒正是用户建子对话的
// 时刻）。这种前缀发给模型不合法（每个 tool_call 必须紧跟自己的 tool 结果），
// 所以整组剥掉、退到上一组为止；其余已落库的行原样保留。
//
// 不看运行状态：进程被强杀在工具中途时，空闲会话同样可能留下半开组。
// 子对话继承（copyInheritedContext）与两处分支对话（handleFork）共用本函数。
func trimOpenToolGroup(rows []*StoredMessage) []*StoredMessage {
	for len(rows) > 0 {
		ai := -1
		for i := len(rows) - 1; i >= 0; i-- {
			if rows[i].Role == "assistant" {
				ai = i
				break
			}
		}
		if ai < 0 || len(rows[ai].ToolCalls) == 0 {
			return rows // 尾巴没有未闭合的工具组
		}
		got := make(map[string]bool, len(rows)-ai)
		for _, x := range rows[ai+1:] {
			if x.Role == "tool" && x.ToolCallID != "" {
				got[x.ToolCallID] = true
			}
		}
		closed := true
		for _, tc := range rows[ai].ToolCalls {
			if !got[tc.ID] {
				closed = false
				break
			}
		}
		if closed {
			return rows
		}
		rows = rows[:ai] // 整组剥掉（那条 assistant 与它的部分结果），再看前一组
	}
	return rows
}

// copyInheritedContext 把父会话的完整消息历史复制进子会话，作为子对话继承的上下文。
// 复制的是**原始行**（含摘要与压缩标记，逐行保序、seq 重排），不是渲染成的一段文本——
// 这样复制后子会话的「模型可见序列」与父会话此前的请求逐条一致（同一套摘要边界、
// 同一套规则压缩视图），DeepSeek 的前缀缓存才能命中整段父上下文。
// 首行写一条 kind=inherit 标记行（前端折叠显示、组装时跳过），返回复制的行数。
func (a *App) copyInheritedContext(parentID, childID string) (int, error) {
	all, err := a.Store.RawMessagesAll(parentID)
	if err != nil {
		return 0, err
	}
	rows := make([]*StoredMessage, 0, len(all))
	for _, x := range all {
		if x.Kind == "request" || x.Kind == "audit" {
			continue // 模型输入快照与审计行是本会话的诊断工件，不随上下文继承
		}
		rows = append(rows, x)
	}
	// 落库即完成：库里有的都是「已完成的小步」（模型正在流式输出的那一步不落库），
	// 所以整份复制即可，不判断"整轮"、也不区分父会话是否正在执行。唯一不能带走的
	// 是尾部未闭合的工具调用组——见 trimOpenToolGroup。
	rows = trimOpenToolGroup(rows)
	if len(rows) == 0 {
		return 0, nil
	}

	// 先算好整张新旧 seq 映射再落库：摘要的覆盖边界要按新 seq 重写，
	// 否则子会话的组装边界会对不上（摘要按旧 seq 覆盖、新行全是新 seq）。
	last, err := a.Store.LastSeq(childID)
	if err != nil {
		return 0, err
	}
	newOfOld := make(map[int64]int64, len(rows))
	seq := last + 1 // 标记行占一个 seq
	for _, x := range rows {
		seq++
		newOfOld[x.Seq] = seq
	}
	// 覆盖边界重映射：取「最后一个 oldSeq ≤ 边界」的已复制行。
	// 摘要的 covers_up_to 必定 ≤ 它自己的 seq（摘要只覆盖排在它之前的行），
	// 因此被复制的摘要总能找到对应行。
	remap := func(oldCover int64) int64 {
		var out int64
		for _, x := range rows {
			if x.Seq > oldCover {
				break
			}
			if n, ok := newOfOld[x.Seq]; ok {
				out = n
			}
		}
		return out
	}

	mark, _ := json.Marshal(inheritMark{Parent: parentID, Count: len(rows)})
	if err := a.Store.AppendRow(childID, &StoredMessage{
		Message: TextMessage("user", string(mark)), Kind: inheritKind, CreatedAt: now(),
	}); err != nil {
		return 0, err
	}
	for _, x := range rows {
		cp := &StoredMessage{
			Message:    x.Message,
			Kind:       x.Kind,
			Origin:     x.Origin,
			Compact:    x.Compact,
			Superseded: x.Superseded,
			Thinking:   x.Thinking,
			CreatedAt:  x.CreatedAt,
			Seq:        newOfOld[x.Seq],
		}
		if x.Kind == "summary" {
			cp.CoversUpTo = remap(x.CoversUpTo)
		}
		if err := a.Store.AppendRow(childID, cp); err != nil {
			return 0, err
		}
	}
	// 上传登记也随上下文继承（uid 原样保留）：继承来的注记里若引用了
	// u 编号，子会话里的 deliver_result（upload 参数）仍能解析到原件路径。失败只记日志。
	if err := a.Store.CopyUploadsOnBranch(parentID, childID); err != nil {
		log.Printf("[上传] 继承 %s → %s 复制上传登记失败（子对话交付目标将不可解析）: %v", parentID, childID, err)
	}
	return len(rows), nil
}

// estimateTokens 粗估一组消息的 token 量（中文为主的启发式）。
func estimateTokens(msgs []Message) int {
	total := 0
	for _, m := range msgs {
		if s, ok := m.Content.(string); ok {
			total += len([]rune(s)) * 3 / 4
		}
		for _, tc := range m.ToolCalls {
			total += len([]rune(tc.Function.Arguments)) * 3 / 4
		}
		total += 8
	}
	return total
}

// ---------- 上下文占用统计（前端「上下文」面板的数据源） ----------

type ctxCategory struct {
	Name   string  `json:"name"`
	Tokens int     `json:"tokens"`
	Share  float64 `json:"share"` // 占总上下文的百分比（0-100）
}

type ctxStats struct {
	Window     int           `json:"window"` // 模型上下文窗口（token）
	Total      int           `json:"total"`  // 下一次请求的估算总量
	Categories []ctxCategory `json:"categories"`
	Cache      struct {
		PromptTokens int64   `json:"promptTokens"` // 历次调用 prompt_tokens 总和（输入）
		CacheHit     int64   `json:"cacheHit"`     // 历次调用缓存命中总和（缓存读取）
		Uncached     int64   `json:"uncached"`     // 未缓存输入 = prompt − hit
		Output       int64   `json:"output"`       // 历次调用 completion_tokens 总和（输出）
		Rate         float64 `json:"rate"`         // 缓存命中率（hit/prompt；无数据为 0）
		Calls        int64   `json:"calls"`        // 模型调用次数
	} `json:"cache"`
	// 输出速度（吐字速度）：最近一次带 token 统计的成功输出。分子=厂家 usage
	// 报的 completion_tokens，分母=输出窗口时长（首个输出事件到末个输出事件，
	// 不含首字前的排队等待）。
	Speed struct {
		TokPerSec float64 `json:"tokPerSec"`
		Tokens    int     `json:"tokens"` // 最后一次输出的 token 数（厂家口径）
		Ms        int64   `json:"ms"`     // 最后一次输出的窗口时长
	} `json:"speed"`
}

// contextStats 按与 buildContext 完全相同的组装规则，把下一次请求的上下文
// 拆成分类 token 估算（同一启发式）。注意：buildContext 的组装规则改动时需同步此处。
func (a *App) contextStats(sess *SessionMeta) ctxStats {
	cats := map[string]int{}
	add := func(name string, msgs ...Message) {
		for _, m := range msgs {
			cats[name] += estimateTokens([]Message{m})
		}
	}

	sys := a.systemPrompt(sess)
	add("系统提示词", TextMessage("system", sys))

	// 工具定义按序列化后的 JSON 体积估算（与消息同一 rune*3/4 启发式）
	if tools := a.toolsFor(sess.Mode); len(tools) > 0 {
		if raw, err := json.Marshal(toolDefsOf(tools)); err == nil {
			cats["系统工具"] += len([]rune(string(raw))) * 3 / 4
		}
	}

	var out ctxStats
	if raw, err := a.Store.RawMessagesAll(sess.ID); err == nil {
		summaries := activeSummaries(raw)
		boundary := int64(0)
		for _, s := range summaries {
			if s.CoversUpTo > boundary {
				boundary = s.CoversUpTo
			}
		}
		for _, s := range summaries {
			add("摘要", TextMessage("user", s.Text()))
		}
		prevEnv := ""     // 与 buildContext 同一套环境快照渲染口径（首条整份、后续差分）
		envUpdates := 0   // 与 buildContext 同一差分计数口径（自愈全量）
		foldSeen := false // 与 buildContext 同一折叠边界口径：交界处注入提示标签
		for _, m := range raw {
			if m.Seq <= boundary || m.Kind == "summary" || m.Kind == "prompt" || m.Kind == inheritKind || m.Kind == "request" || m.Kind == "audit" {
				continue
			}
			if m.Compact == "" && foldSeen {
				add("消息", TextMessage("user", foldBoundaryNote))
				foldSeen = false
			}
			if m.Compact != "" {
				foldSeen = true
			}
			// 与 buildContext 同一折叠口径：omit 行（含压缩区环境快照）不进上下文
			if m.Compact == compactOmit {
				continue
			}
			if m.Kind == "env" {
				env := m.Text()
				if s, n := envRowRender(prevEnv, env, envUpdates); s != "" {
					add("环境快照", TextMessage("user", s))
					envUpdates = n
				}
				prevEnv = env
				continue
			}
			if m.Compact != "" {
				add("消息", Message{Role: m.Role, Content: m.Compact})
				// 与 buildContext 同一派生口径：折叠的用户侧行同样跟时间消息
				for _, tm := range a.timeAfter(m) {
					add("消息", tm)
				}
				continue
			}
			add("消息", m.Message)
			// 时间提示词与 buildContext 同一派生口径（用户侧对话消息后跟一条
			// 时间消息），计入「消息」类——它确实随请求发出、占 token
			for _, tm := range a.timeAfter(m) {
				add("消息", tm)
			}
		}
		// 与 buildContext 同一口径：整段可见历史都已折叠时提示标签缀尾
		if foldSeen {
			add("消息", TextMessage("user", foldBoundaryNote))
		}
		// 输出速度：倒序找最近一次有效的成功输出——audit 行带输出窗口 outMs 且
		// 厂家报了 completion_tokens。窗口过短（<100ms，瞬发完成）与纯工具调用
		// 步没有速度意义，跳过取更早一次的真实文本输出
		for i := len(raw) - 1; i >= 0; i-- {
			if raw[i].Kind != kindAudit {
				continue
			}
			var row auditRow
			if json.Unmarshal([]byte(raw[i].Text()), &row) != nil ||
				row.Type != "call" || row.Scope != "" || row.Error != "" ||
				row.OutMs < 100 || row.Usage == nil || row.Usage.Completion <= 0 {
				continue
			}
			out.Speed.Tokens = row.Usage.Completion
			out.Speed.Ms = row.OutMs
			out.Speed.TokPerSec = float64(row.Usage.Completion) * 1000 / float64(row.OutMs)
			break
		}
	}

	order := []string{"消息", "系统工具", "系统提示词", "摘要", "环境快照", "其他"}
	total := 0
	for _, v := range cats {
		total += v
	}
	out.Window = a.resolveTarget(sess).ContextWindow
	out.Total = total
	for _, name := range order {
		tok := cats[name]
		share := 0.0
		if total > 0 {
			share = float64(tok) * 100 / float64(total)
		}
		out.Categories = append(out.Categories, ctxCategory{Name: name, Tokens: tok, Share: math.Round(share*10) / 10})
	}
	out.Cache.PromptTokens = sess.UsagePrompt
	out.Cache.CacheHit = sess.UsageCacheHit
	out.Cache.Output = sess.UsageOutput
	out.Cache.Uncached = sess.UsagePrompt - sess.UsageCacheHit
	if out.Cache.Uncached < 0 {
		out.Cache.Uncached = 0
	}
	out.Cache.Calls = sess.UsageCalls
	if sess.UsagePrompt > 0 {
		out.Cache.Rate = float64(sess.UsageCacheHit) / float64(sess.UsagePrompt)
	}
	return out
}

// ---------- 规则压缩（零模型调用） ----------

// 规则压缩不随轮次自动跑（每轮改写历史前缀会持续打断 KV 缓存），只在两种
// 时机执行：上下文估算达到阈值时（maybeAutoCompact）与手动压缩时
// （handleCompact 的 upTo）。折叠语义分两级（见 compactRound）：一级保留用户
// 输入与正文、调用名+参数（write/edit 写入类参数全文保留不截断，见
// compactKeepArgs），丢弃思考/工具结果/环境快照，可反复叠加（每次只折
// 新轮）；二级只留用户输入与模型最终输出，仅在压缩范围内已没有新轮可折时才
// 升级（见 foldRounds——一级压得动就绝不跳二级）。组装时在折叠边界注入
// foldBoundaryNote 提示模型「以上已压缩、细节可查历史」；以轮为单位整体折叠，
// 绝不拆散调用/结果配对。
// visibleSize 是一行消息在「模型可见表面」上的字数（rune）：折叠过的行按
// 折叠视图计（omit=0），原文行计正文+思考+工具参数（这些都会随请求发出）；
// 视图外的归档行（summary/prompt/request/audit/inherit）不计。
func visibleSize(m *StoredMessage) int {
	switch {
	case m.Kind == "summary" || m.Kind == "prompt" || m.Kind == "request" || m.Kind == "audit" || m.Kind == inheritKind:
		return 0
	case m.Compact == compactOmit:
		return 0
	case m.Compact != "":
		return len([]rune(m.Compact))
	}
	n := len([]rune(m.Text())) + len([]rune(m.Thinking))
	for _, tc := range m.ToolCalls {
		n += len([]rune(tc.Function.Arguments))
	}
	return n
}

// keepStartSeq 按保留预算（KeepChars，字）算保留区的起点 seq：从最新一条往前
// 累计模型可见字数，预算耗尽处（命中点）所在的轮**整轮保留**——起点扩到该轮
// 的起始行，压缩边界永远落在轮与轮之间，绝不从一轮中间切（用户口径：命中点
// 所在的那一整轮不进行压缩）。轮首不必是用户亲发的消息：数字助理转发消息
// （report/aiuser）同权开轮（见 splitRounds），主对话里大部分回合由转发消息
// 开启——早年轮次不认派发消息，几十个回合被算成一个超长轮，才有过「巨轮回退
// 行级窗口」的例外；那个例外已砍掉（它会把一轮切成两半，压缩边界落在轮中间，
// 实测普通任务轮也被判成巨轮）。正常任务轮不会超长；真遇到按预算压不动的
// 超长轮，由溢出兜底（overflowCompact 按轮强压）处理——那是撞模型窗口的最后
// 防线，不走这条预算路径。
func (a *App) keepStartSeq(raw []*StoredMessage) int64 {
	// 用户语义：保留预算 0 = 什么都不保留——连最后一轮也压（返回最后一条
	// seq+1，让调用方的「折到保留起点-1」覆盖全部行）。与「命中轮整轮保留」
	// 的常规路径相反，属显式全压开关。
	if a.Cfg.KeepChars <= 0 {
		if len(raw) == 0 {
			return 0
		}
		return raw[len(raw)-1].Seq + 1
	}
	total := 0
	for i := len(raw) - 1; i >= 0; i-- {
		total += visibleSize(raw[i])
		if total > a.Cfg.KeepChars {
			// 命中点：这一行装不下了——它所在的轮整轮保留（含本轮内更早的行）
			return a.roundStartOf(raw, raw[i].Seq)
		}
	}
	// 全部装得下：保留区从第一条起（不压缩）。调用方按「start <= 1 即无可压」
	// 判断，返回首条 seq 与 0 等价，这里取可读性更好的首条 seq
	if len(raw) > 0 {
		return raw[0].Seq
	}
	return 0
}

// roundStartOf 返回 seq 所在轮（见 splitRounds）的起始 seq。整轮保留是硬口径：
// 压缩边界只在轮与轮之间，与这一轮多大无关。找不到所属轮时返回该行自身
// （防御：正常不会走到——每行都归属于某个轮）。
func (a *App) roundStartOf(raw []*StoredMessage, seq int64) int64 {
	for _, r := range splitRounds(raw) {
		if seq >= r.startSeq && seq <= r.endSeq {
			return r.startSeq
		}
	}
	return seq
}

// ruleCompactAuto 阈值触发/溢出兜底的自动入口：把保留窗口之外、尚未折叠的
// 行折叠掉。keep<0 表示自动触发——按 KeepChars 字数预算保留最近的完整轮
// （命中轮整轮保留、边界只在轮之间，见 keepStartSeq）；keep=0 表示全部
// 折叠（溢出兜底）；keep>0 表示只留最近 keep 轮（溢出兜底的两级：先只留 1 轮，
// 再全部折叠）。返回折叠的行数（0 = 没有可折叠的）。
func (a *App) ruleCompactAuto(sess *SessionMeta, keep int) int {
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return 0
	}
	if len(raw) == 0 {
		return 0
	}
	if keep < 0 {
		// 自动：按 KeepChars 预算保留最近的完整轮（命中轮整轮保留），其余折叠
		start := a.keepStartSeq(raw)
		if start <= 1 {
			return 0
		}
		return a.foldUpTo(sess, raw, start-1)
	}
	rounds := splitRounds(raw)
	if len(rounds) == 0 {
		return 0
	}
	var endSeq int64
	if keep == 0 {
		endSeq = rounds[len(rounds)-1].endSeq // 全部折叠（含最近一轮）
	} else if keep < len(rounds) {
		endSeq = rounds[len(rounds)-keep].startSeq - 1
	} else {
		return 0
	}
	return a.foldRounds(sess, raw, rounds, endSeq)
}

// ruleCompactTo 手动压缩：折叠 upTo 所在轮（含）及更早、尚未折叠的全部轮，
// upTo 之后的轮保持原文。范围内已全折叠过时改为把已折叠轮升级为二级（新轮
// 优先一级、一级压不动了才二级，见 foldRounds）。返回实际变化的轮数。
func (a *App) ruleCompactTo(sess *SessionMeta, upTo int64) (int, error) {
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return 0, err
	}
	rounds := splitRounds(raw)
	// 找 upTo 所在轮：该轮整体折叠（一轮是不可分割的压缩单位，拆半轮会让
	// 正文与调用分家）；upTo 落在轮外的行（inherit 标记等）时并给下一轮
	end := int64(0)
	for _, r := range rounds {
		if upTo >= r.startSeq && upTo <= r.endSeq {
			end = r.endSeq
			break
		}
	}
	if end == 0 {
		for _, r := range rounds {
			if r.startSeq > upTo {
				end = r.endSeq
				break
			}
		}
	}
	if end == 0 {
		return 0, fmt.Errorf("第 %d 条之前没有可折叠的回合", upTo)
	}
	if n := a.foldRounds(sess, raw, rounds, end); n > 0 {
		return n, nil
	}
	return 0, fmt.Errorf("第 %d 条之前的回合都已折叠过了", upTo)
}

// foldRounds 按轮折叠到 endSeq（含）。范围内还有未折叠的轮就只做一级折叠、
// 到此为止；范围内已没有新轮可折时，才把已折叠的轮升级为二级（只留用户输入
// 与模型最终输出）。用户语义：一级可以无限次叠加（每次只折新轮），二级是
// 「实在没得压了」的最后一步——绝不能这次折了新轮，顺带把旧折叠区跳级到二级。
// 返回本次实际变化的轮数（新折轮数或升级轮数；0 = 无变化，重复点击报错用）。
// 已折叠边界 = 带任意 compact 标记的行（含 omit 的工具结果与环境快照）的最大
// seq——compactRound 对每轮至少标一行（用户输入也标），边界因此总能推进。
// 升级二级要求整轮都在边界之内（r.endSeq ≤ 边界）：边界只在轮之间，整轮要么
// 全在折叠区、要么全在保留区，这个条件正常恒成立——保留它作防御：旧库历史里
// 可能留有早前行级回退口径切开的半轮，半折轮不能升级。
func (a *App) foldRounds(sess *SessionMeta, raw []*StoredMessage, rounds []round, endSeq int64) int {
	ruleSeq := int64(0)
	for _, m := range raw {
		if m.Compact != "" && m.Seq > ruleSeq {
			ruleSeq = m.Seq
		}
	}
	// 先一级折范围内的新轮：折到了就返回，不动任何旧折叠行
	res := buildToolResIndex(raw)
	folded, from := 0, int64(0)
	for _, r := range rounds {
		if r.startSeq <= ruleSeq || r.endSeq > endSeq {
			continue
		}
		a.compactRound(sess.ID, r, raw, res)
		if folded == 0 {
			from = r.startSeq
		}
		folded++
	}
	if folded > 0 {
		// 审计留痕：规则压缩改写模型可见前缀，是缓存命中率突降的常见原因
		a.auditWrite(sess, map[string]any{"type": "compact", "rounds": folded, "from": from, "to": endSeq,
			"unit": a.Cfg.CompactUnit, "interval": a.Cfg.CompactInterval})
		// 压缩区结束标记+变更账本+结论摘录（report 行：用户可见、折叠时原样保留）
		a.appendCompactNote(sess, raw, from, endSeq)
		// token 单位：压缩改变了上下文，等下一个 usage 作为新基线
		a.markCompactBase(sess.ID)
		// 通知前端重取：compact 标记变了，对话区要在真实位置翻新「已折叠」行
		//（reason=compact 让前端作废前缀缓存全量重建——标记 UPDATE 不改 seq，
		// 不作废前缀复用判定永远命中，边界行出不来）
		a.broadcast(Event{Type: "session_updated", SessionID: sess.ID, Data: map[string]any{"reason": "compact"}})
		// 折叠可能埋掉最后一条环境快照行：埋了就完整重发一条
		a.resendEnvAfterCompact(sess)
		return folded
	}
	// 范围内全是已折叠轮：这次才升级二级（只留用户输入与最终输出）
	upgraded := 0
	for _, r := range rounds {
		if r.startSeq > ruleSeq || r.endSeq > ruleSeq || r.endSeq > endSeq {
			continue
		}
		if !a.compactRoundAgain(sess.ID, r, raw) {
			continue // 已二级的轮幂等跳过，不计数
		}
		if upgraded == 0 {
			from = r.startSeq
		}
		upgraded++
	}
	if upgraded > 0 {
		a.auditWrite(sess, map[string]any{"type": "compact", "upgraded": upgraded, "from": from, "to": endSeq})
		// 二级升级同样大幅改写了上下文：token 基线等下一个 usage 刷新
		a.markCompactBase(sess.ID)
		a.broadcast(Event{Type: "session_updated", SessionID: sess.ID, Data: map[string]any{"reason": "compact"}})
		// 二级升级不改变快照行的可见性（已 omit 的仍 omit），但若此前从未
		// 重发过（跨进程重启等边缘态），这里兜底再查一次，幂等
		a.resendEnvAfterCompact(sess)
	}
	return upgraded
}

// foldUpTo 行级折叠：把 endSeq 及更早、尚未折叠的行按一级规则折叠掉（自动预算
// 压缩用）。endSeq 由 keepStartSeq 决定，边界永远对齐到命中轮的轮首-1——这里
// 折的就是整轮整轮的历史，绝不从一轮中间切（命中轮整轮保留）。行级只是折叠
// 的实施粒度（逐行 compactOne），不是边界口径。
// 与 foldRounds 的差别只在粒度：行级更细，所以这里不做轮级那套「二级升级」
// （也避免每次自动压缩改写旧折叠行、持续打断 KV 缓存）；反复触发仍压不动时由
// overflowCompact 的按轮全折兜底。
// 返回新折叠的行数（0 = 没有可折叠的）。
func (a *App) foldUpTo(sess *SessionMeta, raw []*StoredMessage, endSeq int64) int {
	res := buildToolResIndex(raw)
	folded, from := 0, int64(0)
	for _, m := range raw {
		if m.Seq > endSeq || m.Compact != "" {
			continue
		}
		if !a.compactOne(sess.ID, m, res) {
			continue // 视图外的归档行 / 空正文行：不算折叠，边界不动
		}
		if folded == 0 {
			from = m.Seq
		}
		folded++
	}
	if folded > 0 {
		// 审计留痕：规则压缩改写模型可见前缀，是缓存命中率突降的常见原因
		a.auditWrite(sess, map[string]any{"type": "compact", "rows": folded, "from": from, "to": endSeq,
			"unit": a.Cfg.CompactUnit, "interval": a.Cfg.CompactInterval})
		// 压缩区结束标记+变更账本+结论摘录（report 行：用户可见、折叠时原样保留）
		a.appendCompactNote(sess, raw, from, endSeq)
		// token 单位：压缩改变了上下文，等下一个 usage 作为新基线
		a.markCompactBase(sess.ID)
		// 通知前端重取：compact 标记变了，对话区要在真实位置翻新「已折叠」行
		//（reason=compact 让前端作废前缀缓存全量重建，口径同 foldRounds）
		a.broadcast(Event{Type: "session_updated", SessionID: sess.ID, Data: map[string]any{"reason": "compact"}})
		// 折叠可能埋掉最后一条环境快照行：埋了就完整重发一条
		a.resendEnvAfterCompact(sess)
	}
	return folded
}

// appendCompactNote 压缩完成后落一条「压缩区结束 + 变更账本 + 结论摘录」的
// report 行：用户在对话流里可见压缩发生了什么；report 属用户侧消息，后续
// 压缩按正文原样保留——账本因此随历史永存，不参与二次折叠损耗。
// 账本内容全部是规则的字符串提取：变更账本取区间内 edit/write 的 path 参数；
// 结论摘录取正文里以「结论：/根因：/原因：」起头的原文行（模型说过的话的
// 子集，不是新生成的总结）。
func (a *App) appendCompactNote(sess *SessionMeta, raw []*StoredMessage, from, to int64) {
	n := 1
	for _, m := range raw {
		if m.Kind == "report" && strings.HasPrefix(m.Text(), compactNotePrefix) {
			n++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s%d 次压缩 · 覆盖 seq %d–%d · 此后为未压缩原文）══════\n", compactNotePrefix, n, from, to)
	var ledger, concl []string
	for _, m := range raw {
		if m.Seq < from || m.Seq > to {
			continue
		}
		for _, tc := range m.ToolCalls {
			if tc.Function.Name == "edit" || tc.Function.Name == "write" {
				if p := argOf(tc.Function.Arguments, "path"); p != "" {
					line := "· " + p + " · [" + strconv.FormatInt(m.Seq, 10) + "]"
					if it := firstLineOf(m.Text(), intentLimit); it != "" && it != p {
						line += " · " + it
					}
					ledger = append(ledger, line)
				}
			}
		}
		if m.Role == "assistant" && len(m.ToolCalls) == 0 {
			for _, ln := range strings.Split(m.Text(), "\n") {
				ln = strings.TrimSpace(ln)
				for _, kw := range []string{"结论：", "根因：", "原因："} {
					if strings.HasPrefix(ln, kw) {
						concl = append(concl, "· "+firstLineOf(ln, 120)+" · ["+strconv.FormatInt(m.Seq, 10)+"]")
						break
					}
				}
			}
		}
	}
	if len(ledger) > 0 {
		b.WriteString("\n变更账本（规则生成）:\n" + strings.Join(ledger, "\n") + "\n")
	}
	if len(concl) > 0 {
		b.WriteString("\n结论摘录（正文原文子集）:\n" + strings.Join(concl, "\n") + "\n")
	}
	_ = a.Store.AppendRow(sess.ID, &StoredMessage{
		Message:   TextMessage("user", strings.TrimRight(b.String(), "\n")),
		SessionID: sess.ID,
		Kind:      "report",
		CreatedAt: now(),
	})
}

// markCompactBase 压缩成功后调用：token 单位时标记「等下一个 usage 刷新基线」
// ——压缩大幅改变了上下文，旧基线不可比；chars 单位无事可做。
func (a *App) markCompactBase(sessID string) {
	if a.Cfg.CompactUnit != "tokens" {
		return
	}
	a.compactMu.Lock()
	defer a.compactMu.Unlock()
	if a.compactUsage == nil {
		a.compactUsage = map[string]*compactUsageMark{}
	}
	if m := a.compactUsage[sessID]; m != nil {
		m.pendingBase = true
	}
}

// toolResInfo 是结果行折叠时的关联信息：对应哪个工具、结果原文。
type toolResInfo struct {
	name string
	text string
}

// buildToolResIndex 建立调用→结果索引（ToolCallID → 工具名+结果文本）：
// 结果行自身不带工具名，从同库的调用行补齐；供 compactOne 按工具分派。
func buildToolResIndex(raw []*StoredMessage) map[string]toolResInfo {
	idx := map[string]toolResInfo{}
	for _, m := range raw {
		if m.Role == "tool" && m.ToolCallID != "" {
			idx[m.ToolCallID] = toolResInfo{text: m.Text()}
		}
	}
	for _, m := range raw {
		for _, tc := range m.ToolCalls {
			if info, ok := idx[tc.ID]; ok && info.name == "" {
				info.name = tc.Function.Name
				idx[tc.ID] = info
			}
		}
	}
	return idx
}

// argOf 从 JSON 参数串取字符串键值；解析失败或类型不符返回空。
func argOf(args, key string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil {
		return ""
	}
	if v, ok := m[key].(string); ok {
		return v
	}
	return ""
}

// argFirst 取参数里字典序第一个非空字符串值（动作行「目标」的兜底）。
func argFirst(args string) string {
	var m map[string]any
	if json.Unmarshal([]byte(args), &m) != nil || len(m) == 0 {
		return ""
	}
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v, ok := m[k].(string); ok && strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

// clipKeepRunes 头尾保留、中间标注（rune 口径）。note 形如 " · 全文取 seq 7"。
func clipKeepRunes(s string, head, tail int, note string) string {
	r := []rune(s)
	if len(r) <= head+tail {
		return s
	}
	cut := len(r) - head - tail
	return string(r[:head]) + "（中间已裁 " + strconv.Itoa(cut) + " 字" + note + "）" + string(r[len(r)-tail:])
}

// firstLineOf 取文本首行并按上限截断（limit≤0 视为 0，返回空——防调用方
// 传 0 导致切片下溢 panic）。
func firstLineOf(s string, limit int) string {
	if limit <= 0 {
		return ""
	}
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	r := []rune(s)
	if len(r) > limit {
		return string(r[:limit-1]) + "…"
	}
	return s
}

// outFail 启发式判定结果是否失败：错误关键词命中即算（宁可多留几行，不可漏掉错误）。
func outFail(s string) bool {
	l := strings.ToLower(s)
	for _, kw := range []string{"exit status", "工具执行失败", "失败:", "失败：", "error:", "panic:", "fatal"} {
		if strings.Contains(l, kw) {
			return true
		}
	}
	return false
}

// clipOutLines 输出三段式：先逐行做超长兜底（单行巨块如无换明输出/minified
// JSON：行内 >300 字裁为首 150 + 标注 + 尾 50），再按行数三段裁切——头 2 行 +
// （中间已裁 N 行 · 全文取 seq）+ 尾 6/10 行。
func clipOutLines(text string, fail bool, seq int64) string {
	text = strings.TrimRight(text, "\r\n \t")
	if text == "" {
		return ""
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		r := []rune(ln)
		if len(r) <= lineKeepLimit {
			continue
		}
		lines[i] = string(r[:lineKeepHead]) + "…（本行已裁 " + strconv.Itoa(len(r)-lineKeepHead-lineKeepTail) +
			" 字 · 全文取 read_session_messages(seq=" + strconv.FormatInt(seq, 10) + ")）…" + string(r[len(r)-lineKeepTail:])
	}
	tail := outTailLines
	if fail {
		tail = outTailFail
	}
	if len(lines) <= outHeadLines+tail {
		return strings.Join(lines, "\n")
	}
	cut := len(lines) - outHeadLines - tail
	head := strings.Join(lines[:outHeadLines], "\n")
	tl := strings.Join(lines[len(lines)-tail:], "\n")
	return head + "\n（中间已裁 " + strconv.Itoa(cut) + " 行 · 全文取 read_session_messages(seq=" +
		strconv.FormatInt(seq, 10) + ")）\n" + tl
}

// pythonScriptPath 从 run_python 结果首行（"脚本：<路径>"）提取已落盘的脚本路径。
func pythonScriptPath(resText string) string {
	first := firstLineOf(resText, 300)
	for _, p := range []string{"脚本：", "脚本:"} {
		if strings.HasPrefix(first, p) {
			return strings.TrimSpace(first[len(p):])
		}
	}
	return ""
}

// contentImageInfo 分解多模态 content：返回文本部分与图片块数。兼容两种形态：
// 内存构造的 []ContentPart 与库读回的 []any(map)。
func contentImageInfo(c any) (string, int) {
	var texts []string
	images := 0
	switch parts := c.(type) {
	case []ContentPart:
		for _, p := range parts {
			switch p.Type {
			case "text":
				if p.Text != "" {
					texts = append(texts, p.Text)
				}
			case "image_url":
				images++
			}
		}
	case []any:
		for _, it := range parts {
			mm, ok := it.(map[string]any)
			if !ok {
				continue
			}
			switch mm["type"] {
			case "text":
				if s, ok := mm["text"].(string); ok && s != "" {
					texts = append(texts, s)
				}
			case "image_url":
				images++
			}
		}
	}
	return strings.Join(texts, "\n"), images
}

// actionTarget 按工具生成动作行的「目标」字段（首要参数）。
func actionTarget(name, args string) string {
	clip := func(s string) string {
		r := []rune(s)
		if len(r) > digestLimit {
			return string(r[:digestLimit-1]) + "…"
		}
		return s
	}
	key := ""
	switch name {
	case "read", "edit", "write", "snapshot_file", "read_image", "ocr", "pdf", "office_skill":
		key = "path"
	case "grep", "glob":
		if p := argOf(args, "pattern"); p != "" {
			s := `"` + p + `"`
			if d := argOf(args, "path"); d != "" {
				s += " " + d
			}
			return clip(s)
		}
	case "browser_open", "web_fetch":
		key = "url"
	case "web_search", "search_sessions":
		key = "query"
	case "todo_task_create":
		key = "title"
	case "run_command":
		return "" // 命令单独成行保留，动作行不带目标
	case "run_python":
		return "" // 目的走意图，脚本路径从结果提取
	}
	if key != "" {
		if v := argOf(args, key); v != "" {
			return clip(v)
		}
	}
	return clip(argFirst(args))
}

// writeArgBlock 生成 edit/write 的参数保留块：≤2000 字原样；超限头 1600 +
// 中间标注 + 尾 400（代码头部是签名与意图、尾部是收尾，两头价值最高）。
func writeArgBlock(seq int64, args string) string {
	r := []rune(args)
	if len(r) <= argKeepLong {
		return "参数: " + args + "\n"
	}
	note := " · 全文取 read_session_messages(seq=" + strconv.FormatInt(seq, 10) + ")"
	return "参数（原 " + strconv.Itoa(len(r)) + " 字 → 头 " + strconv.Itoa(argKeepHead) +
		" + 尾 " + strconv.Itoa(argKeepTail) + "）:\n" +
		string(r[:argKeepHead]) + "\n（中间已裁 " + strconv.Itoa(len(r)-argKeepHead-argKeepTail) +
		" 字" + note + "）\n" + string(r[len(r)-argKeepTail:]) + "\n"
}

// stripImageEnvelopes 把工具结果文本里的图片信封行（attachment.go 的
// `<image attachment="…" … path="…"/>`）替换为卸载标注：图片本体不进折叠
// 视图（信息价值由看图后的正文结论承载），标注保留附件号与源文件路径作
// 回看通道。信封在结果文本的头部（mcpImagesEnvelope 约定拼在最前），替换
// 后的一行自然成为该结果的保留首行。
var imgEnvAttRe = regexp.MustCompile(`attachment="([^"]*)"`)
var imgEnvPathRe = regexp.MustCompile(`path="([^"]*)"`)

func stripImageEnvelopes(text string, seq int64) string {
	if !strings.Contains(text, "<image attachment=") {
		return text
	}
	lines := strings.Split(text, "\n")
	for i, ln := range lines {
		t := strings.TrimSpace(ln)
		if !strings.HasPrefix(t, "<image attachment=") || !strings.HasSuffix(t, "/>") {
			continue
		}
		note := "（图片已卸载 · 附件 " + firstSegment(imgEnvAttRe, t)
		if p := firstSegment(imgEnvPathRe, t); p != "" {
			note += " · 源文件 " + p + " · 重看：read_image " + p
		}
		note += "，或 read_session_messages(seq=" + strconv.FormatInt(seq, 10) + ")）"
		lines[i] = note
	}
	return strings.Join(lines, "\n")
}

// firstSegment 取正则第一个捕获组（无匹配返回空）。
func firstSegment(re *regexp.Regexp, s string) string {
	if m := re.FindStringSubmatch(s); len(m) > 1 {
		return m[1]
	}
	return ""
}

// toolResultCompact 按工具分派结果行的折叠视图；返回 compactOmit 表示整行省略。
func toolResultCompact(info toolResInfo, m *StoredMessage) string {
	text := strings.TrimSpace(stripImageEnvelopes(info.text, m.Seq))
	switch info.name {
	case "run_command", "run_python":
		if text == "" {
			return compactOmit
		}
		if out := clipOutLines(text, outFail(text), m.Seq); out != "" {
			return out
		}
		return compactOmit
	case "edit", "write":
		if text == "" {
			return compactOmit
		}
		if outFail(text) {
			return "失败：" + firstLineOf(text, 100)
		}
		if info.name == "edit" {
			return "编辑成功"
		}
		return "已写入"
	case "read", "glob", "list_dir", "search_sessions", "read_session_messages", "office_skill", "read_skill":
		return compactOmit // 内容可再生：文件可重读、回查可重跑、指南可重取
	case "grep":
		if text == "" {
			return "→ 无命中" // 零命中是「不存在」的证据，必须保留
		}
		return "→ " + firstLineOf(text, resLineLimit)
	default:
		if text == "" {
			return compactOmit
		}
		return "→ " + firstLineOf(text, resLineLimit) // 一行结论（含截图/浏览器/派发/待办等）
	}
}

// compactOne 把单行**首次**折叠为轻量表示并落库（幂等，不改原文）；res 是
// 调用→结果索引（工具分派用）。返回是否真的标了这一行（视图外的归档行与
// 空正文行不标，调用方据此计数）：
//   - 用户侧输入（chat/aiuser/report）：Compact=原文（视图逐字节不变，标记只为推进边界）
//   - 助手正文行：Compact=原文（思考过程丢弃——折叠视图不带 ReasoningContent）
//   - 多模态消息（content 为内容块数组）：文本部分保留 + 图片卸载标注（图片
//     的信息价值由看图后的正文结论承载，卸载处留 seq/路径回看通道）
//   - 带工具调用的助手行：正文 + 动作行（[seq] 工具 目标 —— 意图；edit/write
//     参数默认原样保留、超 2000 字才头尾裁；run_command 命令文本保留）
//   - 工具结果行：按工具分派（命令/脚本输出三段式、写入类成败一句话、grep
//     命中摘要、可再生类省略、其余一行结论）
//   - 环境快照行：omit（组装时整行跳过；保留区首个可见快照重作基线）
func (a *App) compactOne(sid string, m *StoredMessage, res map[string]toolResInfo) bool {
	switch {
	case m.Kind == "summary" || m.Kind == "prompt" || m.Kind == inheritKind || m.Kind == "request" || m.Kind == "audit":
		// 视图外的归档行不标（组装本来就跳过）
		return false
	case len(m.ToolCalls) > 0:
		var b strings.Builder
		if s := m.Text(); strings.TrimSpace(s) != "" {
			b.WriteString(s + "\n")
		}
		intent := firstLineOf(m.Text(), intentLimit)
		for _, tc := range m.ToolCalls {
			name := tc.Function.Name
			info := res[tc.ID]
			line := fmt.Sprintf("[%d] %s", m.Seq, name)
			if target := actionTarget(name, tc.Function.Arguments); target != "" {
				line += " " + target
			}
			switch name {
			case "edit", "write":
				if intent != "" {
					line += " —— " + intent
				}
			case "run_python":
				if p := pythonScriptPath(info.text); p != "" {
					line += "（脚本已落盘 " + p + "）"
				} else if intent != "" {
					line += " —— " + intent
				}
			}
			b.WriteString(line + "\n")
			switch name {
			case "edit", "write":
				b.WriteString(writeArgBlock(m.Seq, tc.Function.Arguments))
			case "run_command":
				if cmd := argOf(tc.Function.Arguments, "command"); cmd != "" {
					if len([]rune(cmd)) <= cmdKeepLimit {
						b.WriteString("命令: " + cmd + "\n")
					} else {
						b.WriteString("命令: " + clipKeepRunes(cmd, cmdKeepHead, cmdKeepTail,
							" · 全文取 read_session_messages(seq="+strconv.FormatInt(m.Seq, 10)+")") + "\n")
					}
				}
			}
		}
		_ = a.Store.SetCompact(sid, m.Seq, strings.TrimRight(b.String(), "\n"))
		return true
	case m.Role == "tool" || m.Kind == "env":
		_ = a.Store.SetCompact(sid, m.Seq, toolResultCompact(res[m.ToolCallID], m))
		return true
	case m.Role == "assistant" || m.Role == "user":
		// 多模态消息：图片本体卸载（base64 是上下文里最隐形的体积源），文本保留
		if txt, n := contentImageInfo(m.Content); n > 0 {
			note := txt
			if strings.TrimSpace(note) == "" {
				note = "（图片消息）"
			}
			note += fmt.Sprintf("\n（图片×%d 已卸载 · 重看：read_image 原路径，或 read_session_messages(seq=%d)）", n, m.Seq)
			_ = a.Store.SetCompact(sid, m.Seq, note)
			return true
		}
		if s := m.Text(); strings.TrimSpace(s) != "" {
			_ = a.Store.SetCompact(sid, m.Seq, s)
			return true
		}
	}
	return false
}

// compactRound 把一整轮首次折叠为一级表示（逐行调 compactOne）。供按轮折叠的
// 手动入口（ruleCompactTo）与溢出兜底用；自动预算压缩走行级的 foldUpTo。
func (a *App) compactRound(sid string, r round, raw []*StoredMessage, res map[string]toolResInfo) {
	for _, m := range raw {
		if m.Seq < r.startSeq || m.Seq > r.endSeq {
			continue
		}
		a.compactOne(sid, m, res)
	}
}

// compactRoundAgain 是二级压缩：对已折叠过的轮再压，只留用户输入与模型最终
// 输出（= 该轮最后一条纯正文助手行），其余整行 omit。只写有实际变化的行，
// 返回该轮是否发生了变化——已二级的轮再跑一遍幂等无变化，调用方据此计数。
func (a *App) compactRoundAgain(sid string, r round, raw []*StoredMessage) bool {
	finalSeq := int64(0)
	for _, m := range raw {
		if m.Seq < r.startSeq || m.Seq > r.endSeq {
			continue
		}
		if m.Role == "assistant" && len(m.ToolCalls) == 0 && strings.TrimSpace(m.Text()) != "" {
			finalSeq = m.Seq
		}
	}
	changed := false
	for _, m := range raw {
		if m.Seq < r.startSeq || m.Seq > r.endSeq {
			continue
		}
		var want string
		switch {
		case m.Kind == "summary" || m.Kind == "prompt" || m.Kind == inheritKind || m.Kind == "request" || m.Kind == "audit":
			continue // 视图外的归档行不标
		case m.Role == "user":
			// 用户输入保留原文（一级时已标，幂等；只补漏）
			if m.Compact == "" && strings.TrimSpace(m.Text()) != "" {
				want = m.Text()
			} else {
				continue
			}
		case m.Seq == finalSeq:
			// 模型最终输出保留原文
			want = m.Text()
		default:
			// 中间过程全部丢弃：调用行、过程性正文、工具结果、环境快照
			want = compactOmit
		}
		if m.Compact != want {
			_ = a.Store.SetCompact(sid, m.Seq, want)
			changed = true
		}
	}
	return changed
}

// resendEnvAfterCompact 压缩后完整重发一条环境快照。环境快照对模型是差分
// 机制（首条整份、后续只发变化行），而压缩恰恰会把差分所依附的完整基线埋
// 掉：规则折叠 omit 压缩区内的快照行、模型摘要整段跳过覆盖边界之前的一切
// 行——之后模型看到的可能只剩几条孤立的差分行（或缺掉整段环境信息），
// 权限/能力/技能/模型清单/工作区都凑不齐。不做"基线是否真的被埋"的判断：
// 差分行与完整行的可靠特征很难找（字数阈值等都是猜），而压缩本身是低频
// 事件，无条件落一条当前完整快照最简单可靠。安全性与代价：
//   - 组装层（envRowRender）对与上一条可见快照同文的行差分为空、不发消息
//     ——环境没变时重发行只留在库里，上下文零冗余；
//   - 基线被埋时，重发行成为保留区首条可见快照，prevEnv 空 → 整份渲染，
//     环境信息即时完整回来；
//   - 落库内容与 maybeAppendEnvSnapshot 的等值基准相同，下一步组装比较
//     相等即不再落行，不会连环追加。
func (a *App) resendEnvAfterCompact(sess *SessionMeta) {
	a.appendRow(sess, StoredMessage{Message: TextMessage("system", a.envPrompt(sess)), Kind: kindEnv, CreatedAt: now()})
}

// ---------- 模型摘要压缩 ----------

// summarizeSections 通用会话的摘要小节要求（压缩指令的中段，2026-11-11 按
// 六节拍板）。设计取舍：
//   - 不设「下一步」节——保留窗口内的最近对话原文随请求原样保留，「接下来
//     做什么」就活在原文里，摘要里再写一份只会过时（用户明确要求去掉）；
//   - 「用户指令与约束摘录」独立成节且安全条款逐字保留（Claude Code 实践）：
//     「不许做 XX」类禁令被压缩掉后模型可能再犯，原话引用保真；
//   - 「关键决定」跨压缩保留（Codex/Cortex 实践）：合并旧摘要时仍成立的
//     决定必须原样带入，防多代压缩后早期决策漂移；
//   - 踩坑与成功经验单独点名：压缩最有长期价值的部分（用户口径：错误与
//     教训 = 踩过的坑 + 验证过的成功做法）。
const summarizeSections = `要求：
1. 输出以下小节（没有内容的写「无」，不得删节）：已完成的任务与意图 / 关键决定 / 文件与改动 / 错误与教训 / 用户指令与约束摘录 / 进行中与未完成。
2. 「已完成的任务与意图」写用户原始与演进的目标里已经落地的部分，关键表述逐字引用。
3. 「关键决定」写方案与架构取舍及其理由——这些是跨压缩保留的核心：如果上面附有先前的自动摘要，仍成立的决定必须原样带入新摘要。
4. 「文件与改动」写精确路径、为何重要、关键改动，代码片段只在必要时引用。
5. 「错误与教训」重点写踩过的坑（失败的尝试及其原因、正确做法）与验证过的成功经验（哪个方案有效、关键参数与命令）。
6. 「用户指令与约束摘录」逐条摘录用户的关键原话，含纠正与变更的意图；「不许做 XX」类约束与安全要求（敏感文件、禁做操作、凭据规则）必须逐字保留。
7. 「进行中与未完成」写正在做什么与明确待办。摘要区间之后的最近对话原文会原样保留，不要在摘要中预告或规划下一步。
8. 保留精确的路径、命令、错误信息、数字与名称，不得改写。
9. 只输出摘要本身，不要评论、不要提及压缩这件事。`

// doSummarize 把 [最后摘要边界, 压缩终点] 的区间压成一条摘要。
// origin=auto 时合并旧自动摘要（DSH 式单一检查点）；origin=manual 时独立叠加、永不重压。
// upTo>0（手动按钮）时压缩终点=upTo 所在轮（含），之后的轮保持原文；
// 否则按保留窗口从尾部倒数——keepTail<0 表示自动触发，与规则压缩共用同一
// 字数预算（keepStartSeq：命中轮整轮保留，边界只在轮之间，绝不切轮）；
// keepTail=0 表示摘要覆盖全部历史（溢出兜底用到）；溢出兜底显式传 1（只留最近一轮）。
// summarizePlan 从原始历史算出本次模型摘要的覆盖终点：upTo 定位按钮所在轮，
// keepTail 决定保留窗口（见 doSummarize 各分支）。抽成独立函数供两处使用：
// doSummarize 本体，与手动压缩入口在把模型调用放去后台**之前**的同步校验
// （点错位置、该区间已压过，立即把错误报给前端，不等后台模型调用失败才发现）。
// summarizable 判断一行是否进入摘要的待压缩视图（doSummarize 的渲染过滤与
// summarizePlan 的区间有效性判定共用同一口径：元数据行不参与渲染，也不算进
// 压缩区间的有效内容）。
func summarizable(m *StoredMessage) bool {
	switch m.Kind {
	case "summary", "env", "prompt", inheritKind, "request", "audit":
		return false
	}
	return true
}

func (a *App) summarizePlan(raw []*StoredMessage, keepTail int, upTo int64) (segEnd, boundary int64, prevAuto *StoredMessage, err error) {
	summaries := activeSummaries(raw)
	for _, s := range summaries {
		if s.CoversUpTo > boundary {
			boundary = s.CoversUpTo
		}
		if s.Origin == "auto" {
			prevAuto = s
		}
	}
	rounds := splitRounds(raw)
	switch {
	case upTo > 0:
		for _, r := range rounds {
			if upTo >= r.startSeq && upTo <= r.endSeq {
				segEnd = r.endSeq
				break
			}
		}
		if segEnd == 0 {
			for _, r := range rounds {
				if r.startSeq > upTo {
					segEnd = r.endSeq
					break
				}
			}
		}
		if segEnd == 0 {
			return 0, 0, nil, fmt.Errorf("第 %d 条之前没有可压缩的回合", upTo)
		}
	case keepTail < 0:
		// 自动触发：与规则压缩同一口径——按 KeepChars 预算保留最近的完整轮
		//（命中轮整轮保留，边界只在轮之间），摘要覆盖到保留区起点之前那一条
		if st := a.keepStartSeq(raw); st > 1 {
			segEnd = st - 1
		}
	case keepTail == 0:
		// 连最近一行都超预算：摘要覆盖到最后一轮（全部历史），确保超长上下文必然削减
		if len(rounds) > 0 {
			segEnd = rounds[len(rounds)-1].endSeq
		}
	case len(rounds) > keepTail:
		segEnd = rounds[len(rounds)-keepTail].startSeq - 1
	}
	if segEnd <= boundary {
		return 0, 0, nil, fmt.Errorf("没有可压缩的区间（该范围之前的内容已压缩过）")
	}
	// 轮的 endSeq 会把摘要/审计等元数据行并进轮尾（splitRounds 只按用户行
	// 分轮）：真实对话内容已全部压过、区间里只剩元数据行时（典型：摘要行
	// 落库后重复点同一位置，末轮 endSeq 被它自己撑大了一号），按「已压过」
	// 报错——否则会再发起一次模型调用、落一条空的重复摘要。
	content := 0
	for _, m := range raw {
		if m.Seq > boundary && m.Seq <= segEnd && summarizable(m) {
			content++
		}
	}
	if content == 0 {
		return 0, 0, nil, fmt.Errorf("没有可压缩的区间（该范围之前的内容已压缩过）")
	}
	return segEnd, boundary, prevAuto, nil
}

func (a *App) doSummarize(sess *SessionMeta, origin string, keepTail int, upTo int64) (string, error) {
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return "", err
	}
	segEnd, boundary, prevAuto, err := a.summarizePlan(raw, keepTail, upTo)
	if err != nil {
		return "", err
	}
	// 摘要请求 = 与主循环同源的消息序列（截止到压缩终点）+ 尾部一条压缩指令。
	// 系统提示词、工具目录、历史（含差分环境快照、时间提示词、折叠视图）与
	// 主循环请求逐字节同源，前缀缓存直接命中整段已有内容——首次压缩命中
	// 整个压缩区间，模型只需要为尾部的指令付增量 token。旧做法把区间渲染成
	// 一段「用户：…/助手：…」转述文本另起炉灶全量重发：零命中、慢、且模型
	// 读的是二手转写而非原生对话格式。
	sys, _, tools := a.ctxEngine().materials(sess)
	msgs := a.buildContextUpTo(sess, sys, segEnd)
	msgs = inlineImagesForRequest(a, msgs, a.resolveTarget(sess))
	msgs = append(msgs, TextMessage("user", a.summarizeDirective(sess, origin, boundary, segEnd, prevAuto)))

	summary, err := a.ctxEngine().CallSummarizer(sess, msgs, toolDefsOf(tools))
	if err != nil {
		return "", err
	}
	if origin == "auto" {
		_ = a.Store.SupersedeAutoSummaries(sess.ID)
	}
	label := "手动"
	if origin == "auto" {
		label = "自动"
	}
	// 覆盖区间（增量式标注，2026-11-11 用户要求）：本次压的是 (boundary, segEnd]
	// 这一段——seq 即数据库行号，写进摘要文本头与结构化字段，未来按区间检索
	// 原文直接可用。auto 合并存**累计起点**（合并链头部：首压时 boundary+1，
	// 之后继承 prevAuto 的累计起点；旧数据无起点字段时按 1=对话开始兜底）；
	// manual 叠加天然增量，本次区间即全部覆盖。
	coversFrom := boundary + 1
	if origin == "auto" {
		switch {
		case prevAuto != nil && prevAuto.CoversFrom > 0:
			coversFrom = prevAuto.CoversFrom
		case prevAuto != nil:
			coversFrom = 1
		}
	}
	rangeNote := fmt.Sprintf("本次压缩第 %d 至 %d 条", boundary+1, segEnd)
	if origin == "auto" && coversFrom != boundary+1 {
		rangeNote += fmt.Sprintf("，合并先前摘要后累计覆盖第 %d 至 %d 条", coversFrom, segEnd)
	}
	// 落库正文头（DSH checkpoint preamble 同款作用）：既标记这是压缩检查点
	//（含可检索的覆盖区间），也给模型行为指令——当作既定背景、直接从后续
	// 原文继续，防止压缩后模型复述/确认摘要浪费一整个回合
	content := fmt.Sprintf("【%s历史摘要（%s，原文可用历史查询工具按区间读取）——以下是既定背景，直接基于后续原文继续工作，不要复述或确认本摘要】\n%s",
		label, rangeNote, summary)
	_ = a.Store.AppendRow(sess.ID, &StoredMessage{
		Message: TextMessage("system", content), Kind: kindSummary, // 上下文材料，非用户发言；发送时现场构造 user 视图
		Origin: origin, CoversUpTo: segEnd, CoversFrom: coversFrom, CreatedAt: now(),
	})
	// 通知前端：摘要行已落库，对话区在覆盖边界处翻新压缩卡片（reason=compact
	// 同时作废前缀缓存——摘要行 append 在会话末尾、不改末条 seq，前缀复用
	// 判定感知不到，卡片会一直不出现在正打开的会话里直到全量重建）
	a.broadcast(Event{Type: "session_updated", SessionID: sess.ID, Data: map[string]any{"reason": "compact"}})
	// 摘要边界埋掉最后一条环境快照行时完整重发一条（与规则折叠同一兜底）
	a.resendEnvAfterCompact(sess)
	return summary, nil
}

// summarizeSectionsAssistant 是数字助理主对话（mode=assistant）专属的摘要
// 小节要求。设计意图（为什么要有这一份）：其他会话是一次任务一个对话，压掉
// 过程细节无妨，按模型自己的压缩习惯就好；数字助理主对话不同——它是用户
// 与常驻助理的长期对话，跨任务、跨天数，历史里沉淀着用户的偏好、习惯与
// 定下的约定，这些一旦被摘要压掉，助理就「不认识这个用户」了。所以它的
// 摘要侧重点要先记人再记事：「用户偏好与习惯」「长期有效的约定」两节置顶
// （后者即通用版「用户指令与约束摘录」的长期形态，不再重复设节），其余
// 小节与通用版 summarizeSections 同构（含关键决定跨压缩保留、踩坑与成功
// 经验点名、无「下一步」节——保留窗口原文盖住最近动向）。
const summarizeSectionsAssistant = `要求：
1. 输出以下小节（没有内容的写「无」，不得删节）：用户偏好与习惯 / 长期有效的约定 / 已完成的任务与结果 / 关键决定 / 涉及的文件与改动 / 错误与教训 / 进行中与未完成。
2. 「用户偏好与习惯」写：称呼与身份、沟通偏好（详略/语言/格式）、常用工具与工作流、常用路径/设备/软件线索、明确表达过的喜欢与不喜欢。
3. 「长期有效的约定」逐条保留用户定下的做事方式、禁忌与必须遵守的口径，保留原意、不概括掉。
4. 「已完成的任务与结果」写跨任务已完成的事与结果，关键表述逐字引用。
5. 「关键决定」写方案取舍及其理由——跨压缩保留：上面附有先前的自动摘要时，仍成立的决定必须原样带入新摘要。
6. 「涉及过的文件与改动」写精确路径、为何重要、关键改动。
7. 「错误与教训」重点写踩过的坑（失败的尝试及其原因、正确做法）与验证过的成功经验（哪个方案有效、关键参数与命令）。
8. 「进行中与未完成」写正在做什么与明确待办。摘要区间之后的最近对话原文会原样保留，不要在摘要中预告或规划下一步。
9. 保留精确的路径、命令、错误信息、数字与名称，不得改写。
10. 只输出摘要本身，不要评论、不要提及压缩这件事。
这是用户与常驻数字助理的长期对话，跨任务、跨天数——摘要首先要让助理"还认识这个用户、还记得这些事"。`

// summarizeSectionsFor 按会话模式选摘要小节要求（指令中段）：数字助理主
// 对话用上面的专属版，其余会话用通用版 summarizeSections。只选「发给模型
// 的提示词文字」，不改变压缩方式/路径/保留窗口（设置里的压缩模式照旧）。
func summarizeSectionsFor(sess *SessionMeta) string {
	if sess != nil && sess.Mode == "assistant" {
		return summarizeSectionsAssistant
	}
	return summarizeSections
}

// summarizeDirective 生成摘要请求尾部的压缩指令——整个请求里唯一的新内容
// （前缀与主循环请求逐字节同源，见 doSummarize），所以任务说明、格式要求、
// 禁工具令全部集中在这条消息里。任务说明用 seq 明确圈定压缩区间：模型看到
// 的消息序列截止在 segEnd，「以上内容」就是待压缩区间，不存在转述歧义。
func (a *App) summarizeDirective(sess *SessionMeta, origin string, boundary, segEnd int64, prevAuto *StoredMessage) string {
	var b strings.Builder
	if boundary > 0 {
		fmt.Fprintf(&b, "【压缩任务】这是一次增量压缩：只总结第 %d 至 %d 条这段消息——即上一份压缩摘要（覆盖到第 %d 条）之后、到本条指令之前的全部内容。先前的摘要已涵盖更早的历史，不要重新总结它，只按「关键决定」的要求把仍成立的决定带入；本条指令之后没有别的消息。\n", boundary+1, segEnd, boundary)
	} else {
		fmt.Fprintf(&b, "【压缩任务】这是一次增量压缩：只总结第 1 至 %d 条这段消息——即从对话最开始、到本条指令之前的全部内容；本条指令之后没有别的消息。\n", segEnd)
	}
	if origin == "auto" && prevAuto != nil {
		b.WriteString("\n【先前的自动摘要（请合并进新摘要，保留仍成立的事实、丢弃过时的）】\n" + prevAuto.Text() + "\n")
	}
	b.WriteString("\n" + summarizeSectionsFor(sess))
	b.WriteString("\n\n这是一次性压缩任务：不允许调用任何工具。思考后直接输出摘要正文，不要输出其他任何内容。")
	return b.String()
}

// callSummarizer 已并入上下文引擎（ContextEngine.CallSummarizer）：摘要调用
// 要把对话历史发给模型，拼装与发送归引擎统一口径，此处不再保留旁路。

// maybeAutoCompact 在每次请求组装后检查自动压缩——间隔制规则通道优先：
//   - 自上次压缩边界起新增未压缩内容 ≥ CompactInterval（0=关闭）→ 先做零
//     成本规则折叠（chars 单位按可见字数累计；tokens 单位用模型返回的
//     usage 真值差值，见 noteCompactUsage）。折出东西就交回调用方重查——
//     压缩后增量归零，间隔天然节流，不需要冷却参数。
//   - 估算 token ≥ 模型阈值（CompactLlmPct>0）→ 生成模型摘要兜底。规则关闭
//     （0）、规则压不动（折无可折）、间隔未到，都自然落到这条通道——它是
//     最后防线。
//
// 间隔与保留共用同一计量单位（CompactUnit：chars 默认 / tokens 真值），不
// 做估算、不混用口径；模型阈值仍按窗口百分比（硬约束须对窗口比较）。
// 返回是否发生了压缩：true 时调用方应重新组装请求（并再查一次，最多两遍：
// 先规则、后模型），避免带着超限前的旧消息发出。
func (a *App) maybeAutoCompact(sess *SessionMeta, msgs []Message, ctxWindow int, roundEnd bool) (bool, error) {
	if ctxWindow <= 0 {
		return false, nil
	}
	// 规则通道：间隔制——新增未压缩量达到间隔即折（零成本；roundEnd 不再
	// 参与：间隔触发与轮边界无关，压完增量归零即天然节流）
	if a.Cfg.CompactInterval > 0 && a.intervalReached(sess) {
		if a.ruleCompactAuto(sess, -1) > 0 {
			return true, nil
		}
	}
	est := estimateTokens(msgs)
	if a.Cfg.CompactLlmPct > 0 && est >= windowPctTokens(ctxWindow, a.Cfg.CompactLlmPct) {
		_, err := a.doSummarize(sess, "auto", -1, 0)
		return err == nil, err
	}
	return false, nil
}

// windowPctTokens 算「窗口 × 百分比阈值」的 token 数。
func windowPctTokens(ctxWindow, pct int) int {
	return int(float64(ctxWindow) * float64(pct) / 100)
}

// compactUsageMark 是 tokens 单位下的会话级用量基线（内存态，重启后以重启
// 后首个 usage 重建基线——间隔是粗粒度节奏参数，重启重算无实际影响）：
// lastPrompt 记最近一次请求的 usage.prompt_tokens（真值，不估算）；
// basePrompt 记压缩后的基线；baseModel 记基线所属模型——换模型时 tokenizer
// 不可比，pendingBase 置位、以下一个 usage 重建基线。
type compactUsageMark struct {
	lastPrompt  int
	basePrompt  int
	baseModel   string
	pendingBase bool // 压缩已完成，等下一个 usage 作为新基线
}

// noteCompactUsage 由主循环的 OnUsage 调（tokens 单位时记录）：间隔 =
// 最近 prompt_tokens − 压缩后基线。压缩成功（foldUpTo/foldRounds）经
// markCompactBase 置 pendingBase，下一个 usage 刷新基线。
func (a *App) noteCompactUsage(sessID, model string, promptTokens int) {
	if a.Cfg.CompactUnit != "tokens" {
		return
	}
	a.compactMu.Lock()
	defer a.compactMu.Unlock()
	if a.compactUsage == nil {
		a.compactUsage = map[string]*compactUsageMark{}
	}
	m := a.compactUsage[sessID]
	if m == nil {
		a.compactUsage[sessID] = &compactUsageMark{lastPrompt: promptTokens, basePrompt: promptTokens, baseModel: model}
		return
	}
	if m.pendingBase || model != m.baseModel {
		m.basePrompt = promptTokens
		m.baseModel = model
		m.pendingBase = false
	}
	m.lastPrompt = promptTokens
}

// intervalReached 间隔判定：chars 单位 = 摘要边界之后未折叠行的可见字数总量
// ≥ 间隔 + 保留预算（压缩后未折叠量自然回落到约保留预算，触发时新增即约
// 间隔，锯齿在 [保留, 间隔+保留] 之间）；tokens 单位 = 最近 usage − 基线
// ≥ 间隔（pendingBase 期间不算——基线还没刷新）。
func (a *App) intervalReached(sess *SessionMeta) bool {
	if a.Cfg.CompactInterval <= 0 {
		return false
	}
	if a.Cfg.CompactUnit == "tokens" {
		a.compactMu.Lock()
		m := a.compactUsage[sess.ID]
		a.compactMu.Unlock()
		return m != nil && !m.pendingBase && m.lastPrompt-m.basePrompt >= a.Cfg.CompactInterval
	}
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return false
	}
	boundary := int64(0)
	for _, s := range activeSummaries(raw) {
		if s.CoversUpTo > boundary {
			boundary = s.CoversUpTo
		}
	}
	total := 0
	for _, m := range raw {
		if m.Seq <= boundary || m.Compact != "" {
			continue
		}
		// 排除压缩机制的副产物：环境快照（压缩后重发的大块）与压缩账本行
		//（report 原文保留）——它们不是对话增量，计入会把下一次间隔顶到线、
		// 造成「压完立刻又触发」的循环
		if m.Kind == "env" || (m.Kind == "report" && strings.HasPrefix(m.Text(), compactNotePrefix)) {
			continue
		}
		total += visibleSize(m)
	}
	return total >= a.Cfg.CompactInterval+a.Cfg.KeepChars
}

// overflowCompact 是上下文超限的兜底：最大化压缩一次，按配置里实际启用的路
// 走（真超限时能用的都用），不再按 CompactMode 二选一——规则的先折（零模型
// 调用、立即见效）：折到只留最近 1 轮，压不动（最近一轮自身超预算，前面又都
// 已折叠）就全部折叠，否则百万级单轮永远压不下去；仍压不动且模型阈值启用时
// 生成摘要（先摘要到只留 1 轮，再摘要全部历史）。
func (a *App) overflowCompact(sess *SessionMeta) error {
	if a.Cfg.CompactInterval > 0 {
		if a.ruleCompactAuto(sess, 1) > 0 || a.ruleCompactAuto(sess, 0) > 0 {
			return nil
		}
	}
	if a.Cfg.CompactLlmPct > 0 {
		if _, err := a.doSummarize(sess, "manual", 1, 0); err == nil {
			return nil
		}
		_, err := a.doSummarize(sess, "manual", 0, 0)
		return err
	}
	if a.Cfg.CompactInterval > 0 {
		return fmt.Errorf("没有可折叠的回合（上下文超限可能来自系统提示词或工具目录本身）")
	}
	return fmt.Errorf("上下文超限且规则/模型自动压缩均未启用，请开启其一或手动精简会话")
}

// isOverflowError 判断模型错误是否为上下文超限。
func isOverflowError(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "context length") || strings.Contains(s, "maximum context") ||
		strings.Contains(s, "too long") || strings.Contains(s, "context window")
}
