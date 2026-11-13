package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// 上下文引擎：会话上下文的唯一组装者 + 上下文事件总线。
//
// 设计契约（对齐 pi 的显式装配管线与 Reasonix 的 cache-first loop）：
//  1. 一切影响「发给模型什么」的材料——系统提示词（可被 data/prompts/<mode>.md
//     引用文件覆盖）、环境快照、工具目录、历史/摘要视图、视觉内联——都经
//     Assemble 组装成请求材料；开轮的 prompt 展示快照行同样经引擎落库
//     （OpenRound），工具目录有增删时开轮先落一条 origin=tools 的变更公告。
//     环境快照按**追加式**进上下文：首条整份、后续按行差分为短「环境更新」，
//     已发送的前缀绝不改写（前缀缓存命中的根基）。RunAgent 只做编排，
//     不触碰任何拼装函数。
//  2. 组装是确定性的：同样的会话状态与技能/提示词文件，重复组装得到逐字节
//     相同的请求材料（这是前缀缓存命中的根基；破坏确定性等于破坏缓存，
//     有测试看护，见 contextengine_test.go）。测量口径（token 估算与前缀
//     指纹）随 Assembly 一并产出，主循环不自算。
//  3. 一切会改变下一次组装结果的会话级事实（权限/模型/思考档位/计划目标/
//     工作区/远程挂载变更）经 Notify 进总线，统一落上下文事件日志
//     （audit type=ctx 行，导出时间线按发生位置呈现）；技能是全局资源，
//     其增减不走 Notify——由环境快照 diff 在受影响会话的尾部留痕
//     （maybeAppendEnvSnapshot 的技能变更行），同一事实不重复记账。
//  4. 主循环的模型调用由 RunAgent 经手发送——引擎产包（Assembly）、循环取包；
//     需要把上下文内容发给模型的侧路调用（模型摘要）则由引擎拼装并发送
//     （CallSummarizer），不存在绕过引擎的第二条拼装路径。插件/组件的自用
//     调用（如语音润色，不带会话前缀）豁免。引擎不执行工具、不做编排；
//     组装产出的快照留痕（env 行）与请求材料同源，日志天然对得上线上。
//  5. 引擎也是记录者与导出门面：组装/调用/请求/事件统一经它落日志
//     （本文件后段的 Journal/RecordCall/RecordRequest），发给模型的请求
//     环形缓存最近 keepRequests 轮；前端两类下载（运行日志 / 原始数据）
//     统一经 ExportTimeline / ExportRawRequests 向引擎申请。
type ContextEngine struct{ app *App }

// ctxEngine 取上下文引擎：NewApp 已装配；测试等直接构造 App 的路径惰性
// 补一个（引擎无状态，重复装配无害）。
func (a *App) ctxEngine() *ContextEngine {
	if a.Ctx == nil {
		a.Ctx = &ContextEngine{app: a}
	}
	return a.Ctx
}

// Assembly 是一次组装的产物：一次模型调用请求的全部上下文材料与测量口径。
type Assembly struct {
	Sys       string    // 系统提示词（含版本与提示词来源标识）
	Env       string    // 环境快照（发给模型时作为尾部 user 消息）
	Tools     []ToolDef // 工具目录（请求体里平级发送的 JSON 数组）
	Msgs      []Message // 消息数组（压缩视图 + 环境快照尾消息 + 视觉内联）
	EstTokens int       // Msgs 的 token 估算（调用日志与请求缓存共用口径）
	SysHash   string    // 系统提示词指纹（变化=前缀缓存必断）
	EnvHash   string    // 环境快照指纹
}

// Assemble 为一步模型调用组装全部请求材料与测量口径，并完成环境快照留痕
// （内容有变化才落新快照行，见 maybeAppendEnvSnapshot；快照按追加式进上下文，
// 渲染逻辑在 buildContext）。RunAgent 每步调用一次。
func (e *ContextEngine) Assemble(sess *SessionMeta, emit func(Event)) *Assembly {
	sys, envRaw, tools := e.materials(sess)
	env := e.app.maybeAppendEnvSnapshot(sess, envRaw, emit)
	msgs := e.app.buildContext(sess, sys)
	// 图片内联跟随当前会话的模型能力；模型不可用时不内联（真正的报错在调模型时给）
	inlineTarget := LLMTarget{}
	if t, err := e.app.resolveTarget(sess); err == nil {
		inlineTarget = t
	}
	msgs = inlineImagesForRequest(e.app, msgs, inlineTarget)
	return &Assembly{Sys: sys, Env: env, Tools: toolDefsOf(tools), Msgs: msgs,
		EstTokens: estimateTokens(msgs), SysHash: promptHash(sys), EnvHash: promptHash(env)}
}

// materials 计算 sys/env/tools 三样纯材料，无任何留痕副作用——Assemble
// （请求材料 + env 留痕）与 OpenRound（开轮快照行）共用同一计算口，
// 展示快照与请求材料天然同源，不会漂移。
func (e *ContextEngine) materials(sess *SessionMeta) (sys, env string, tools []Tool) {
	return e.app.systemPrompt(sess), e.app.envPrompt(sess), e.app.toolsFor(sess.Mode)
}

// toolDefsOf 把工具目录转成请求体形态的 JSON 数组（Assemble/OpenRound/
// contextStats 共用的唯一转换口）。
func toolDefsOf(tools []Tool) []ToolDef {
	defs := make([]ToolDef, len(tools))
	for i, tl := range tools {
		defs[i] = tl.Spec
	}
	return defs
}

// OpenRound 开轮留痕：组装本轮初始材料并落 kind=prompt 展示快照行（正文
// {sys, env, tools}），主循环每轮开始调用一次，自己不再拼这条行。env 为
// 原始快照（不含技能变更补丁行）；环境快照的落库 diff 仍由首步 Assemble
// 负责。工具目录相对上一轮有增删时，先落一条 kind=report（origin=tools）
// 的变更公告——追加式上下文里目录真变化本就断前缀，公告让模型明确知道
// 增删了什么（能力开关不改编目，走环境快照公告，见 suspendedInPlan）。
func (e *ContextEngine) OpenRound(sess *SessionMeta, emit func(Event)) {
	sys, env, tools := e.materials(sess)
	defs := toolDefsOf(tools)
	if note := e.toolChangeNote(sess, defs); note != "" {
		sm := e.app.appendRow(sess, StoredMessage{Message: TextMessage("user", note), Kind: kindReport, Origin: "tools", CreatedAt: now()})
		if emit != nil {
			emit(Event{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kindReport, Origin: "tools"})
		}
	}
	if pj, err := json.Marshal(map[string]any{"sys": sys, "env": env, "tools": defs}); err == nil {
		sm := e.app.appendRow(sess, StoredMessage{Message: TextMessage("system", string(pj)), Kind: kindPrompt, CreatedAt: now()})
		if emit != nil {
			emit(Event{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kindPrompt})
		}
	}
}

// toolChangeNote 对比本轮与上一轮（最近一条 kind=prompt 行）的工具目录，
// 产出「停用某某工具、新增某某工具」式的变更公告；首轮或未变化返回空串。
func (e *ContextEngine) toolChangeNote(sess *SessionMeta, defs []ToolDef) string {
	raw, err := e.app.Store.RawMessagesAll(sess.ID)
	if err != nil {
		return ""
	}
	var prev promptRec
	found := false
	for i := len(raw) - 1; i >= 0; i-- {
		if raw[i].Kind != kindPrompt {
			continue
		}
		if err := json.Unmarshal([]byte(raw[i].Text()), &prev); err == nil {
			found = true
		}
		break
	}
	if !found {
		return ""
	}
	oldNames := map[string]bool{}
	for _, t := range prev.Tools {
		oldNames[t.Function.Name] = true
	}
	newNames := map[string]bool{}
	for _, d := range defs {
		newNames[d.Function.Name] = true
	}
	var added, removed []string
	for n := range newNames {
		if !oldNames[n] {
			added = append(added, n)
		}
	}
	for n := range oldNames {
		if !newNames[n] {
			removed = append(removed, n)
		}
	}
	if len(added) == 0 && len(removed) == 0 {
		return ""
	}
	sort.Strings(added)
	sort.Strings(removed)
	var b strings.Builder
	b.WriteString("【工具目录变更】")
	if len(added) > 0 {
		b.WriteString("\n- 新增工具：" + strings.Join(added, "、"))
	}
	if len(removed) > 0 {
		b.WriteString("\n- 移除工具（已停用）：" + strings.Join(removed, "、"))
	}
	b.WriteString("\n（工具目录已按上述变化更新，其余工具不变）")
	return b.String()
}

// ---------- 侧路模型调用：模型摘要 ----------
//
// 摘要生成要把对话历史发给模型——凡是「把上下文内容发给模型」的拼装与发送
// 都归引擎（压缩策略本身——压哪段、待压缩视图怎么渲染——仍在 context.go 的
// doSummarize，那是专门的程序）。留痕走 kind=audit（type=call、scope=summarize），
// 进导出时间线与文首调用表；不进 kind=request 环形缓存——那是主对话前缀
// 观察窗（相邻两两 diff），混入侧路调用会污染 diff 链；摘要本身另有 kind=summary
// 行可见。不计入会话用量统计（与主循环调用分列口径一致）。

// summarizerPrompt 是摘要调用的系统提示词（请求材料的常量部分，归引擎）。
const summarizerPrompt = "你是严谨的对话历史压缩助手。"

// CallSummarizer 由引擎拼装并发起摘要模型调用，流式收齐全文返回。mock 目标
// （离线演示）直接给占位摘要，不发请求也不落调用痕。
func (e *ContextEngine) CallSummarizer(sess *SessionMeta, input string) (string, error) {
	target, terr := e.app.resolveTarget(sess)
	if terr != nil {
		return "", terr // 模型不可用：如实报错，不让摘要悄悄走别的渠道
	}
	if target.BaseURL == "mock" {
		return "（离线演示摘要：本段历史已完成创建演示文件并验证，细节见原始记录。）", nil
	}
	msgs := []Message{TextMessage("system", summarizerPrompt), TextMessage("user", input)}
	var sb strings.Builder
	var usage *Usage
	start := time.Now()
	// 用量明细（组件 usage-stats）：摘要调用单独记为「上下文压缩」用途——压缩的
	// token 花销往往不小，混在主对话里看不出钱花在哪
	// 摘要调用不吃会话档位（请求不带 reasoning effort，走服务端默认）：明细里
	// 推理强度记空，与「实际使用的档位」一致
	meterCb, finishUsage := e.app.meterUsage(usagePurposeCompact, sess, target, "", StreamCallbacks{
		OnText: func(s string) { sb.WriteString(s) },
		OnUsage: func(u Usage) {
			uu := u
			usage = &uu
		},
	})
	err := StreamChat(context.Background(), target, ChatRequest{Model: target.Model, Messages: msgs}, meterCb)
	finishUsage(err)
	p := map[string]any{"type": "call", "scope": "summarize", "model": target.AuditModel(),
		"msgs": len(msgs), "est": estimateTokens(msgs),
		"ms": time.Since(start).Milliseconds(), "text": len([]rune(sb.String()))}
	if usage != nil {
		p["usage"] = auditUsage{Prompt: usage.PromptTokens, Hit: usage.CacheHit(),
			Miss: usage.CacheMiss(), Completion: usage.CompletionTokens}
	}
	if err != nil {
		p["error"] = err.Error()
	}
	e.Journal(sess, p)
	if err != nil {
		return "", fmt.Errorf("摘要调用失败: %w", err)
	}
	out := strings.TrimSpace(sb.String())
	if out == "" {
		return "", fmt.Errorf("摘要调用没有产出内容")
	}
	return out, nil
}

// 上下文事件类型（Notify 的 kind）：每类都必然改变下一次组装的结果。
const (
	ctxPerm      = "perm_changed"      // 权限/能力开关变更
	ctxModel     = "model_changed"     // 提供方/模型变更
	ctxThinking  = "thinking_changed"  // 思考档位变更
	ctxMode      = "mode_flag_changed" // 计划/目标模式开关
	ctxWorkspace = "workspace_changed" // 工作目录变更
	ctxRemote    = "remote_changed"    // 远程工作区挂载/解除
)

// Notify 是上下文事件总线的唯一入口：kind 用上面的 ctxEvent 常量，detail
// 一句人话（含前后值），原样进导出时间线。在配置落库成功后调用。
func (e *ContextEngine) Notify(sess *SessionMeta, kind, detail string) {
	if sess == nil {
		return
	}
	e.app.auditWrite(sess, map[string]any{"type": "ctx", "kind": kind, "detail": detail})
}

// promptOverride 读 data/prompts/<mode>.md 引用文件（存在即覆盖该模式的基础
// 提示词）；不存在返回空串。文件缺失/读失败按无覆盖处理，不阻断组装。
func (a *App) promptOverride(mode string) string {
	if a.Cfg == nil || a.Cfg.DataDir == "" {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(a.Cfg.DataDir, "prompts", mode+".md"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ---------- 导出门面：前端两类下载统一向引擎申请 ----------

// ExportTimeline 生成会话的运行日志 Markdown（排版实现在 export.go）。
func (e *ContextEngine) ExportTimeline(m *SessionMeta) string {
	return e.app.exportTimeline(m)
}

// ExportRawRequests 生成「导出原始数据」文档：最近 keepRequests 轮真正发给
// 模型的请求体**原样数组**（旧→新，与线上发送逐字节同源），不带任何导出侧
// 附加的元数据——用户要看的就是纯原始数据；会话信息与调用统计在运行日志
// （ExportTimeline）里。没有请求记录时返回错误（与旧版口径一致）。
func (e *ContextEngine) ExportRawRequests(m *SessionMeta) ([]byte, error) {
	recs, err := e.RecentRequests(m, keepRequests)
	if err != nil {
		return nil, err
	}
	if len(recs) == 0 {
		return nil, fmt.Errorf("该会话还没有发给模型的请求记录（发送过一条消息后可用；离线演示模式不产生请求）")
	}
	bodies := make([]json.RawMessage, 0, len(recs))
	for _, r := range recs {
		bodies = append(bodies, r.Body)
	}
	return json.MarshalIndent(bodies, "", "  ")
}

// ---------- 引擎日志与请求缓存（上下文引擎的记录半边） ----------
//
// 引擎收到什么、发出什么都从这里落库，前端两类导出（运行日志 / 原始数据）
// 统一经引擎申请。
//
// kind=audit 行（role=system，前端对话流、上下文组装、历史工具都跳过）记录
// 「不发给模型、但复盘对话演进必需」的事件——每次模型调用的请求规模/前缀
// 指纹/返回/用量（缓存命中）、运行中报错（含网关错误响应摘要）、用户停止、
// 上下文超限重试、规则压缩生效、上下文事件（Notify）；导出日志按时间线呈现。
//
// kind=request 行缓存最近 keepRequests=3 次真正发给模型的完整请求体
// （环形裁剪，Body 与线上逐字节同源），供「导出原始数据」观察相邻请求间
// 的变化与缓存影响。

// Journal 落一条引擎日志事件行；payload 至少带 type
// （call|error|stop|compact|overflow|ctx）。
func (e *ContextEngine) Journal(sess *SessionMeta, payload map[string]any) {
	if sess == nil {
		return
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return
	}
	_ = e.app.Store.AppendRow(sess.ID, &StoredMessage{
		Message: TextMessage("system", string(b)), Kind: kindAudit, CreatedAt: now(),
	})
}

// auditWrite 兼容入口：存量调用点（主循环报错/压缩/停止等）统一汇入引擎日志。
func (a *App) auditWrite(sess *SessionMeta, payload map[string]any) {
	a.ctxEngine().Journal(sess, payload)
}

// auditUsage 是一次模型调用的流尾用量（审计行里的精简形态）。
type auditUsage struct {
	Prompt     int `json:"prompt"`
	Hit        int `json:"hit"`
	Miss       int `json:"miss"`
	Completion int `json:"completion"`
}

// auditRow 是 kind=audit 行正文的解析形态；各 type 只用自己那几个字段。
type auditRow struct {
	Type    string      `json:"type"`
	Model   string      `json:"model,omitempty"` // call：本次请求实际使用的模型（provider/model；改动前的旧行留空）
	Scope   string      `json:"scope,omitempty"` // call：调用方域（summarize=引擎侧路摘要调用；空=主循环）
	Step    int         `json:"step"`            // call：本轮第几步（从 1 起）
	Msgs    int         `json:"msgs"`            // call：发送的消息条数
	Est     int         `json:"est"`             // call：发送前的 token 估算
	Ms      int64       `json:"ms"`              // call：耗时
	Text    int         `json:"text"`            // call：返回正文字数
	OutMs   int64       `json:"outMs,omitempty"` // call：输出窗口耗时（首末输出事件间，吐字速度分母）
	Tools   []string    `json:"tools"`           // call：返回的工具调用名
	Usage   *auditUsage `json:"usage"`
	Error   string      `json:"error"`             // call/error/overflow：错误原文（网关错误含响应摘要）
	SysHash string      `json:"sysHash,omitempty"` // call：本次系统提示词指纹（变化=前缀缓存必断）
	EnvHash string      `json:"envHash,omitempty"` // call：本次环境快照指纹
	Rounds  int         `json:"rounds"`            // compact：折叠的轮数
	From    int64       `json:"from"`              // compact：折叠区间起始 seq
	To      int64       `json:"to"`                // compact：折叠区间结束 seq
	Kind    string      `json:"kind"`              // ctx（上下文事件总线）：事件类别，见上文常量
	Detail  string      `json:"detail"`            // ctx：一句人话说明（含前后值）
}

// RecordCall 在一次模型调用返回后（无论成败）落引擎日志行：调用前后的关键
// 事实一次记全（含当次组装的前缀指纹——指纹变化即能解释缓存命中变化）。
// model 是本次请求实际使用的模型（provider/model，见 LLMTarget.AuditModel）——
// 按模型拆账靠它，历史旧行没有该字段、解析后为空即不补。
// outMs 是输出窗口（首末输出事件间）的时长，与厂家 usage 的 completion_tokens
// 相除即吐字速度；无输出（纯报错）的调用为 0。
func (e *ContextEngine) RecordCall(sess *SessionMeta, model string, step, msgs, est int, sysHash, envHash string, ms int64, usage *Usage, textRunes int, toolNames []string, callErr error, outMs int64) {
	p := map[string]any{
		"type": "call", "step": step, "msgs": msgs, "est": est, "ms": ms, "text": textRunes,
	}
	if model != "" {
		p["model"] = model
	}
	if outMs > 0 {
		p["outMs"] = outMs
	}
	if sysHash != "" {
		p["sysHash"] = sysHash
	}
	if envHash != "" {
		p["envHash"] = envHash
	}
	if len(toolNames) > 0 {
		p["tools"] = toolNames
	}
	if usage != nil {
		p["usage"] = auditUsage{Prompt: usage.PromptTokens, Hit: usage.CacheHit(),
			Miss: usage.CacheMiss(), Completion: usage.CompletionTokens}
	}
	if callErr != nil {
		p["error"] = callErr.Error()
	}
	e.Journal(sess, p)
}

// toolNamesOf 取一组工具调用的名字（审计行的返回摘要用）。
func toolNamesOf(calls []ToolCall) []string {
	names := make([]string, 0, len(calls))
	for _, tc := range calls {
		names = append(names, tc.Function.Name)
	}
	return names
}

// lastRequestRec 是 kind=request 行的正文：一次发给模型的请求全量记录。
// Body 与线上逐字节同源（发送前捕获）；URL/Model 只记元信息，绝不含 API Key。
// Model 为 provider/model 形式（LLMTarget.AuditModel），与 call 审计行同口径。
// Sys/Env 指纹与 audit 行同源，导出对比用。
type lastRequestRec struct {
	URL       string          `json:"url"`
	Model     string          `json:"model"`
	Step      int             `json:"step"`
	MsgCount  int             `json:"msgs"`
	EstTokens int             `json:"estTokens"`
	SysHash   string          `json:"sysHash,omitempty"`
	EnvHash   string          `json:"envHash,omitempty"`
	Body      json.RawMessage `json:"body"`
}

// RecordRequest 在请求发出前捕获并落库（环形保留最近 keepRequests 条）。
func (e *ContextEngine) RecordRequest(sess *SessionMeta, url, model string, step, msgs, est int, sysHash, envHash string, body []byte) {
	rec := lastRequestRec{URL: url, Model: model, Step: step, MsgCount: msgs, EstTokens: est,
		SysHash: sysHash, EnvHash: envHash, Body: append(json.RawMessage(nil), body...)}
	b, err := json.Marshal(rec)
	if err != nil {
		return
	}
	_ = e.app.Store.AppendRequestRow(sess.ID, &StoredMessage{
		Message: TextMessage("system", string(b)), Kind: kindRequest, CreatedAt: now(),
	})
}

// RecentRequests 取最近 n 次发给模型的请求记录（旧→新）；供原始数据导出。
func (e *ContextEngine) RecentRequests(m *SessionMeta, n int) ([]lastRequestRec, error) {
	rows, err := e.app.Store.RecentRequestRows(m.ID, n)
	if err != nil {
		return nil, err
	}
	out := make([]lastRequestRec, 0, len(rows))
	for _, r := range rows {
		var rec lastRequestRec
		if err := json.Unmarshal([]byte(r.Text()), &rec); err == nil && len(rec.Body) > 0 {
			out = append(out, rec)
		}
	}
	return out, nil
}
