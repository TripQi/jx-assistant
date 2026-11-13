package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	neturl "net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- OpenAI chat-completions 兼容的消息与请求结构 ----------

type ToolCallFunc struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolCall struct {
	ID       string       `json:"id"`
	Type     string       `json:"type"`
	Function ToolCallFunc `json:"function"`
}

// Message 同时用于本地持久化与 LLM 请求体。Content 用 any 是因为
// 带工具调用的 assistant 消息需要 content 为 null，而其余角色是字符串。
type Message struct {
	Role    string `json:"role"`
	Content any    `json:"content"`
	// 思考模式历史回传：DeepSeek V4/flash、智谱 GLM-5.x 等要求带 tools 的请求
	// 把此前 assistant 回合的思考原文原样传回（否则 400「reasoning_content in
	// the thinking mode must be passed back」/ 智谱的保留式思考校验）。仅
	// DeepSeek/GLM 系端点读取，其余 OpenAI 兼容端点忽略消息上的未知字段。
	//
	// 用指针是必需的：nil = 这条消息没有该字段（省略）；非 nil 指向 "" = 显式
	// 空思考。二者对上游不等价——带 tools 时字段整个缺失会 400，而空串合法
	// （模型当轮确实没产生思考，也要把字段带上）。见 fillReasoningStub。
	ReasoningContent *string    `json:"reasoning_content,omitempty"`
	ToolCalls        []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID       string     `json:"tool_call_id,omitempty"`
}

func TextMessage(role, text string) Message {
	return Message{Role: role, Content: text}
}

func (m Message) Text() string {
	if s, ok := m.Content.(string); ok {
		return s
	}
	return ""
}

type ToolSpec struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolDef struct {
	Type     string   `json:"type"` // 固定 "function"
	Function ToolSpec `json:"function"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []ToolDef `json:"tools,omitempty"`
	Stream   bool      `json:"stream"`
	// 流尾统计：OpenAI 兼容端（DeepSeek/GLM 等）要求显式开启才在最后一个
	// chunk 附带 usage（缓存命中率的数据来源）
	StreamOptions *StreamOptionsCfg `json:"stream_options,omitempty"`
	// 思考控制：全系统固定六档（thinkingLevels），chat/completions 一律走
	// reasoning_effort 原样透传——不再按模型挑 thinking{type} 开关那套老口径。
	ReasoningEffort string `json:"reasoning_effort,omitempty"`
	// 以下三个参数主循环不设置（服务端默认），供协议转换与测试用：
	// Anthropic 必填 max_tokens（协议层会兜底默认值）；temperature 三协议都
	// 透传；tool_choice 按 OpenAI 语义（auto/none/required/具名工具）。
	MaxTokens   int      `json:"max_tokens,omitempty"`
	Temperature *float64 `json:"temperature,omitempty"`
	ToolChoice  string   `json:"tool_choice,omitempty"`
}

type StreamOptionsCfg struct {
	IncludeUsage bool `json:"include_usage"`
}

// ContentPart 是多模态消息的内容块（OpenAI 兼容格式）：
// 视觉输入用 {type:"text"} + {type:"image_url"} 组合放在 user 消息里。
type ContentPart struct {
	Type     string    `json:"type"` // text | image_url
	Text     string    `json:"text,omitempty"`
	ImageURL *ImageURL `json:"image_url,omitempty"`
}

type ImageURL struct {
	URL string `json:"url"`
}

// ApplyThinking 把会话思考档位落到请求。档位与模型解绑：六档一律走
// reasoning_effort 原样透传（none…max），空串=不干预（跟随服务端默认）。
// 老会话库里的 disabled 归一到 none、enabled（只开关不指深浅）归一到空串；
// 认不出的串同样按不干预处理——不替用户瞎猜，报错让用户自己换档。
func (r *ChatRequest) ApplyThinking(level string) {
	lv, ok := normalizeThinkingLevel(level)
	if !ok {
		lv = ""
	}
	r.ReasoningEffort = lv
}

// StreamCallbacks 把流式输出回调给调用方：思考增量、正文增量、完整的工具调用
// 与流尾的用量统计（DeepSeek/GLM 等兼容网关在最后一个 chunk 附带 usage，
// 含各形态的前缀缓存命中量，见 Usage.CacheHit）。
type StreamCallbacks struct {
	OnText      func(string)
	OnReasoning func(string)
	OnToolCall  func(ToolCall)
	OnUsage     func(Usage)
	// OnRequest 在请求真正发出前回调一次：url 是最终请求地址，body 是与线上
	// 完全一致的请求体字节（含 model/stream/stream_options 等最终字段）。
	// 「导出原始数据」靠它捕获最近一次发给模型的完整内容。
	OnRequest func(url string, body []byte)
	// OnBody 在模型流响应体建立后回调一次（abort=强制断开器，关响应体即断流），
	// 流结束后再回调一次（abort=nil，注销）。停止看门狗的硬终结（runtime.go
	// forceStopRun）靠它在流不响应 ctx 取消时直接断开底层连接。
	OnBody func(abort func())
}

// Usage 是流尾统计块（OpenAI 兼容）。缓存命中量各端点字段名不一，取用一律
// 走 CacheHit/CacheMiss，别直接读字段：
//   - OpenAI/智谱 GLM：prompt_tokens_details.cached_tokens（OpenAI 标准口径；
//     智谱 coding 端点实测只报这个——只认 DeepSeek 字段会让面板把明明命中的
//     调用记成 0%）
//   - DeepSeek：prompt_cache_hit_tokens / prompt_cache_miss_tokens（旧口径）
//   - 部分网关的 Anthropic 兼容层：cache_read_input_tokens
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	PromptCacheHit   int `json:"prompt_cache_hit_tokens"`
	PromptCacheMiss  int `json:"prompt_cache_miss_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
	// OpenAI 标准形态：{"prompt_tokens_details":{"cached_tokens":N}}
	PromptTokensDetails *PromptTokensDetails `json:"prompt_tokens_details,omitempty"`
	// Anthropic 兼容层形态：usage 里的缓存读取量
	CacheReadTokens int `json:"cache_read_input_tokens,omitempty"`
	// Anthropic 原生协议：本次调用**写入**前缀缓存的 token 量（prompt cache
	// creation，与读取命中是两回事——CacheHit 只认读取，不计入）
	CacheCreationTokens int `json:"cache_creation_input_tokens,omitempty"`
}

// PromptTokensDetails 是 OpenAI 标准 usage 明细块（cached_tokens=命中前缀量）。
type PromptTokensDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

// CacheHit 取本次调用命中的前缀缓存 token 数：各端点字段名归一（取最大非零）。
func (u Usage) CacheHit() int {
	hit := u.PromptCacheHit
	if u.PromptTokensDetails != nil && u.PromptTokensDetails.CachedTokens > hit {
		hit = u.PromptTokensDetails.CachedTokens
	}
	if u.CacheReadTokens > hit {
		hit = u.CacheReadTokens
	}
	return hit
}

// ReportsCache 报告本次 usage 是否带缓存统计：端点完全不报时用它区分
// 「没命中」与「没统计」（前者由 CacheMiss 推出未命中量，后者两者皆 0）。
func (u Usage) ReportsCache() bool {
	return u.PromptCacheHit > 0 || u.PromptCacheMiss > 0 ||
		u.PromptTokensDetails != nil || u.CacheReadTokens > 0 || u.CacheCreationTokens > 0
}

// CacheMiss 取未命中部分：端点明报 miss 就照用；只报命中量的端点（OpenAI
// 口径）按 prompt-hit 推出；端点压根没报缓存统计则返回 0。
func (u Usage) CacheMiss() int {
	if u.PromptCacheMiss > 0 {
		return u.PromptCacheMiss
	}
	if !u.ReportsCache() {
		return 0
	}
	if hit := u.CacheHit(); u.PromptTokens > hit {
		return u.PromptTokens - hit
	}
	return 0
}

// connFailure 连接层面的失败（拨号失败/超时/拒连，区别于模型服务返回的业务
// 错误）。调用层拿它判断要不要走备用地址（公司通道外网故障转移）以及要不要
// 触发公司通道自动停用（sync.go noteProviderConnFail）。
//
// 它同时带着这次失败的现场：一句人话 + 实际拨号目标 + 底层错误原文 + 是否经
// 代理 + 耗时。只用「连接被拒绝」一句话根本判不出是不是本机 TUN + fake-ip
// 代理栈那条路（实测 api.travel7.org 被解析成 198.18.0.13、内核回 RST），
// 事后无法追查；这些细节现在原样进报错文案（见 connErrText）。
type connFailure struct {
	err       error         // 底层错误原文（Unwrap 用；文案里也要原文）
	kind      string        // 一句人话（连接被拒绝/连接超时/域名解析/证书）
	target    string        // 实际拨号目标 host:port（取不到则空）
	proxy     string        // "未经代理" / "经代理 http://127.0.0.1:7890"
	elapsed   time.Duration // 从发起到失败的耗时（含 DNS/TCP/TLS）
	retryable bool          // 拨号瞬断（拒连/连接重置）：单次连接内值得短退避重试一次
}

func (e *connFailure) Error() string { return "无法连接模型服务：" + connErrText(e) }
func (e *connFailure) Unwrap() error { return e.err }

func isConnFailure(err error) bool {
	var cf *connFailure
	return errors.As(err, &cf)
}

// streamHTTP 流式对话专用客户端：只限拨号阶段（8 秒），不限总时长——流式响应
// 可以合法地跑很久。公司内网地址出了公司往往不可路由，靠它快速失败转外网，
// 不然每次调用都要白等系统默认的 TCP 重试超时。
var streamHTTP = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 8 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		DialTLSContext:        companyTransport.DialTLSContext, // 证书策略同 companyTransport（域名/内网 IP 免校验，公网 IP 严格）
		MaxIdleConns:          16,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 60 * time.Second,
	},
}

// altPreferred 按提供方记住「上次是靠外网地址成功的」（出公司期间每通电话先走
// 外网，免得次次先等内网拨号超时）；内网恢复后外网先失败即自动切回。
var altPreferred sync.Map // providerID → bool（true=优先外网）

// StreamChat 向解析后的 LLM 目标发起一轮流式对话。
// target.BaseURL 配为 "mock" 时走内置演示模型，便于离线验证整条链路。
// target.Protocol 选渠道协议（""＝OpenAI 兼容 chat/completions、anthropic、
// responses，实现见 llmproto.go）；配了外网备用地址（AltBaseURL，公司通道由
// 公告下发）且主地址把自己的拨号重试探梯（connRetryDelays，1/2/3/4/5s）跑完
// 仍连接失败时，自动改走备用地址（备用地址同样跑一遍阶梯），成功后粘住备用
// 地址直到它再失败——故障转移对三种协议一视同仁。
func StreamChat(ctx context.Context, target LLMTarget, req ChatRequest, cb StreamCallbacks) error {
	if target.BaseURL == "mock" {
		return mockStream(req, cb)
	}
	proto := protocolOf(target.Protocol)
	body, err := proto.marshal(target, req)
	if err != nil {
		return err
	}
	primary := strings.TrimRight(target.BaseURL, "/")
	alt := strings.TrimRight(target.AltBaseURL, "/")
	first, second := primary, alt
	altFirst := false
	if alt != "" && target.ProviderID != "" {
		if v, ok := altPreferred.Load(target.ProviderID); ok {
			altFirst, _ = v.(bool)
		}
	}
	if altFirst {
		first, second = alt, primary
	}
	err = streamProtoOnce(ctx, first, target.APIKey, body, proto, cb)
	if err == nil {
		markAltPreferred(target, first == alt && alt != "")
		return nil
	}
	if second == "" || !isConnFailure(err) || ctx.Err() != nil {
		return err
	}
	log.Printf("[模型] %s 连接失败，改走备用地址 %s", first, second)
	err2 := streamProtoOnce(ctx, second, target.APIKey, body, proto, cb)
	if err2 == nil {
		markAltPreferred(target, second == alt && alt != "")
		return nil
	}
	return err2
}

// fillReasoningStub 给历史里「当轮没产生思考」的 assistant 消息补一个空
// reasoning_content。DeepSeek（thinking 模式）与智谱 GLM-5.x（交错式/保留式
// 思考）在带 tools 的请求里硬校验这个字段：字段整个缺失直接 400（「思考内容
// 没有带上」），空串则合法。而客户端只在落库思考非空时才写该字段，模型某些
// 轮次（尤其连续机械工具轮：连着读文件、截图）本身不产生思考，历史里就留下
// 一个个"缺字段"的洞，下一轮必然 400——且此后每轮都 400，直到换模型或清历史。
//
// 只对带 tools 的请求补：无 tools 时两家都不校验，补了反而给不认识的兼容端点
// 添噪音。已带思考的消息一律原样保留——智谱要求 reasoning_content 与原始序列
// 逐字一致，不能改写、裁剪或重排。
func fillReasoningStub(req *ChatRequest) {
	if len(req.Tools) == 0 {
		return
	}
	for i := range req.Messages {
		m := &req.Messages[i]
		if m.Role == "assistant" && m.ReasoningContent == nil {
			empty := ""
			m.ReasoningContent = &empty
		}
	}
}

// altPrefNotify 「优先外网」状态翻转的通知钩子（NewApp 挂上：公司通道翻转时广播
// providers 事件，前端模型菜单的 内网/外网 徽标与设置页状态即时刷新）。
var altPrefNotify func(providerID string, preferAlt bool)

// setAltPreferred 写入提供方的「优先外网」状态，翻转即通知（状态未变不打扰）。
func setAltPreferred(providerID string, preferAlt bool) {
	if providerID == "" {
		return
	}
	if v, ok := altPreferred.Load(providerID); ok && v.(bool) == preferAlt {
		return
	}
	altPreferred.Store(providerID, preferAlt)
	if altPrefNotify != nil {
		altPrefNotify(providerID, preferAlt)
	}
}

// companyViaPublic 公司通道当前是否在走外网（模型菜单徽标与设置页状态用）：
// 「上次成功走的是外网地址」或「公告只能从公网拿到」（sync.go 出公司兜底）。
func companyViaPublic() bool {
	v, ok := altPreferred.Load(wizardProviderID)
	return ok && v.(bool)
}

// markAltPreferred 记住本次成功的地址形态；没有备用地址的提供方不参与。
func markAltPreferred(target LLMTarget, preferAlt bool) {
	if target.AltBaseURL == "" || target.ProviderID == "" {
		return
	}
	setAltPreferred(target.ProviderID, preferAlt)
}

// connRetryDelays 拨号瞬断（拒连/连接重置/连接被强制关闭/管道断开/unexpected
// eof）的重试探梯：首试失败后按 1s、2s、3s、4s、5s 依次退避重试**同一地址**，
// 最多 5 次重试——合计最多 6 次尝试、最坏约 15 秒。
//
// 早先只补一次、间隔 200ms：对「代理内核正在切节点」这类真实的瞬断太短，等同
// 直接报错（用户 2026-11-13 反馈「200ms 太短」）。阶梯只由本处集中定义，任何
// 退避数值都从这里取，别在调用点再散落魔法数。
//
// 只在拨号层退避：外层 agent.go 的整步重试（5 次 × 1/2/3/4s）**不接连接类
// 失败**（isRetryableModelErr 对「无法连接模型服务」返回 false）——两层各做满
// 会把最坏等待放大成 5×(1+2+3+4+5)=75 秒，用户会以为程序卡死。
var connRetryDelays = []time.Duration{
	1 * time.Second, 2 * time.Second, 3 * time.Second, 4 * time.Second, 5 * time.Second,
}

// sleepCtx 等 d，期间 ctx 取消（用户点停止/插话打断）立即返回 false，绝不睡满。
func sleepCtx(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return ctx.Err() == nil
	}
}

// streamProtoOnce 向单个地址发一轮流式请求（StreamChat 的主/备用共用体，
// 三种协议共用：端点路径、鉴权头与流解析都来自 proto）。
//
// 顺序固定为：**先在这个地址上把 connRetryDelays 的整条阶梯跑完（首试 +
// 1s/2s/3s/4s/5s 各重试一次，最多 6 次尝试）→ 仍失败才由 StreamChat 改走
// 备用地址（alt），alt 也按同一阶梯从首试开始跑一遍**。主地址没跑完自己的
// 阶梯，绝不去碰备用地址（内网地址只是瞬断时不该提前切外网）。
//
// 阶梯只认拨号瞬断（连接被拒/重置/强制关闭/管道断开/unexpected eof，见
// isDialTransient）；429 限流、鉴权失败、余额不足、模型不存在、上下文超限都是
// 业务错误，压根不是 connFailure，走不到这里；超时/域名解析/证书也不重试。
// 每一步等待都走 sleepCtx：ctx 取消（用户点停止/插话打断）立即放弃，不睡满。
func streamProtoOnce(ctx context.Context, baseURL, apiKey string, body []byte, proto llmProtocol, cb StreamCallbacks) error {
	url := baseURL + proto.path
	// 拨号目标：端点地址的 host:port（也就是 target.BaseURL 的 host:port）。
	// 只用于报错细节，不参与请求构造——不为此改任何签名。
	target := urlHost(url)
	if cb.OnRequest != nil {
		cb.OnRequest(url, body) // 只报一次：本次调用（含阶梯重试）是同一个请求
	}
	var err error
	for i := 0; ; i++ {
		err = streamProtoTry(ctx, url, target, apiKey, body, proto, cb)
		var cf *connFailure
		if !errors.As(err, &cf) || !cf.retryable || ctx.Err() != nil {
			return err
		}
		if i >= len(connRetryDelays) { // 阶梯跑完：交给 StreamChat 决定要不要切备用地址
			return err
		}
		delay := connRetryDelays[i]
		log.Printf("[模型] %s 拨号瞬断（%s），%s 后重试同一地址（第 %d/%d 次重试）",
			target, cf.kind, delay, i+1, len(connRetryDelays))
		if !sleepCtx(ctx, delay) {
			return err // ctx 取消：立即退出（不睡满），也不再多试
		}
	}
}

// streamProtoTry 向一个地址实发一次请求：构造请求 → 拨号/收响应头 → 状态码
// 检查 → 解流。连接失败时把现场（拨号目标、原始错误、代理形态、耗时）一并
// 塞进 connFailure，供报错文案与「要不要短退避重试」判断。
func streamProtoTry(ctx context.Context, url, target, apiKey string, body []byte, proto llmProtocol, cb StreamCallbacks) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	proto.setAuth(apiKey, httpReq.Header)
	started := time.Now()
	resp, err := streamHTTP.Do(httpReq)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return err // 调用方取消：原样返回，不算连接故障，也不重试
		}
		return newConnFailure(httpReq, target, started, err)
	}
	// 登记强制断开器（停止看门狗用）：拿到响应体后交出一个可直接关闭它的
	// abort；流结束（正常收尾/出错/ctx 取消）时回调 OnBody(nil) 注销，登记表
	// 不留旧句柄。断开一个已关闭的 body 是安全的幂等操作。
	if cb.OnBody != nil {
		cb.OnBody(func() { _ = resp.Body.Close() })
		defer cb.OnBody(nil)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(upstreamErrText(resp.StatusCode, snippet))
	}
	return proto.parse(resp.Body, cb)
}

// upstreamErrText 把网关返回的错误整理成用户看得懂的一行：优先取 JSON 里的
// error.message / message（余额不足、限流、鉴权失败这类都在这），否则回退原文摘要。
func upstreamErrText(status int, body []byte) string {
	type apiErr struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	}
	var wrap struct {
		Error   *apiErr `json:"error"`
		Msg     string  `json:"msg"`
		Message string  `json:"message"`
		Detail  string  `json:"detail"`
	}
	var text string
	if json.Unmarshal(body, &wrap) == nil {
		switch {
		case wrap.Error != nil && strings.TrimSpace(wrap.Error.Message) != "":
			text = wrap.Error.Message
		case strings.TrimSpace(wrap.Message) != "":
			text = wrap.Message
		case strings.TrimSpace(wrap.Msg) != "":
			text = wrap.Msg
		case strings.TrimSpace(wrap.Detail) != "":
			text = wrap.Detail
		}
	}
	if text == "" {
		text = strings.TrimSpace(string(body))
	}
	// 折叠成单行并限长：报错条是一行提示，不是日志
	text = strings.Join(strings.Fields(text), " ")
	if text == "" {
		text = http.StatusText(status)
	}
	return fmt.Sprintf("模型服务返回 %d：%s", status, truncateRunes(text, 300))
}

// connErrDetailMax 报错文案里「括号细节」的长度上限（字）：整条文案按界面
// 报错条约 300 字控制，人话在前不被截掉，细节部分自己让位。
// connErrRawMax 是底层错误原文在细节里的长度上限：原文一长就把「是否经代理」
// 「耗时」挤出去了，而这两项恰恰是排障时最要紧的。
const (
	connErrDetailMax = 260
	connErrRawMax    = 200
)

// connErrText 把一次连接失败整理成一行文案：一句人话在前，括号里挂底层细节
// （实际拨号目标、底层错误原文、是否经代理、耗时）。形如：
//
//	连接被拒绝，请确认 API 地址与端口可达（拨号 198.18.0.13:443 失败：dial tcp
//	198.18.0.13:443: connectex: No connection could be made because the target
//	machine actively refused it；耗时 3ms；未经代理）
//
// 界面报错条是一行提示，所以整体压平（strings.Fields）并限长，不撑成长条。
func connErrText(e *connFailure) string {
	kind := e.kind
	if kind == "" {
		kind = "连接建立失败"
	}
	var parts []string
	if e.target != "" {
		parts = append(parts, "拨号 "+e.target+" 失败")
	}
	if e.err != nil {
		if raw := strings.Join(strings.Fields(e.err.Error()), " "); raw != "" {
			parts = append(parts, truncateRunes(raw, connErrRawMax))
		}
	}
	// 耗时总要报：<1ms 也是有用信息（说明这次是秒回 RST，不是等到超时）
	if ms := e.elapsed.Milliseconds(); ms < 1 {
		parts = append(parts, "耗时 <1ms")
	} else {
		parts = append(parts, fmt.Sprintf("耗时 %dms", ms))
	}
	if e.proxy != "" {
		parts = append(parts, e.proxy)
	}
	if len(parts) == 0 {
		return kind
	}
	return kind + "（" + truncateRunes(strings.Join(parts, "；"), connErrDetailMax) + "）"
}

// newConnFailure 记一次连接失败的全部现场。诊断本机 TUN + fake-ip 代理栈瞬断
// （端点被解析成 198.18.x.x、内核直接回 RST）时，这几项缺一不可。
func newConnFailure(req *http.Request, target string, started time.Time, err error) *connFailure {
	return &connFailure{
		err:       err,
		kind:      connErrKind(err),
		target:    target,
		proxy:     proxyDesc(req),
		elapsed:   time.Since(started),
		retryable: isDialTransient(err),
	}
}

// connErrKind 把底层连接错误归类成一句人话（超时/拒连/证书各说各的，不含细节）。
func connErrKind(err error) string {
	msg := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, context.Canceled), strings.Contains(msg, "context canceled"):
		return "请求已取消"
	case errors.Is(err, context.DeadlineExceeded), strings.Contains(msg, "i/o timeout"),
		strings.Contains(msg, "timeout"):
		return "连接超时，模型服务没有在预期时间内响应"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "server misbehaving"):
		return "域名解析失败，请检查 API 地址（baseURL）是否正确"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return "TLS 证书校验失败，请检查代理或证书配置"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "actively refused"):
		return "连接被拒绝，请确认 API 地址与端口可达"
	}
	return "连接建立失败"
}

// isDialTransient 判断这次连接失败是不是「拨号瞬断」——连接被拒 / 连接被重置
// 这类马上再试一次大概率就能过的情形（本机代理内核瞬抖正是这种）。超时、域名
// 解析、证书一律不算：同一地址立刻重试结果一样，白等用户时间。
func isDialTransient(err error) bool {
	msg := strings.ToLower(err.Error())
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		strings.Contains(msg, "timeout") || strings.Contains(msg, "context canceled") {
		return false
	}
	for _, w := range []string{"connection refused", "actively refused", "connection reset",
		"forcibly closed", "broken pipe", "unexpected eof"} {
		if strings.Contains(msg, w) {
			return true
		}
	}
	return false
}

// proxyFromEnv 解析代理环境变量（与 streamHTTP 的 Transport 同一口径）。
// 抽成变量是为了单测能确定地拿到「未经代理」形态——开发机环境里真配了代理
// 也不该让文案断言飘。
var proxyFromEnv = http.ProxyFromEnvironment

// proxyDesc 说明这次拨号是否经代理（报错细节里最要紧的一项：本机 TUN +
// fake-ip 代理栈瞬断时，会话看起来就像「地址不可达」）。
func proxyDesc(req *http.Request) string {
	pu, err := proxyFromEnv(req)
	if err != nil {
		return "代理配置有误：" + truncateRunes(strings.Join(strings.Fields(err.Error()), " "), 80)
	}
	if pu == nil {
		return "未经代理"
	}
	return "经代理 " + pu.Scheme + "://" + pu.Host
}

// urlHost 取请求地址的 host:port（缺省端口按 scheme 补全），作为拨号目标的
// 兜底展示；解析失败返回空串，文案里就不写「拨号 … 失败」这一段。
func urlHost(rawurl string) string {
	u, err := neturl.Parse(rawurl)
	if err != nil || u.Host == "" {
		return ""
	}
	if u.Port() == "" {
		switch u.Scheme {
		case "https":
			return u.Host + ":443"
		case "http":
			return u.Host + ":80"
		}
	}
	return u.Host
}

// isModelUnavailableErr 判断一次模型调用失败是不是「这个模型/提供方用不了」：
// 余额或额度耗尽、鉴权失败、模型不存在、提供方连不上这类硬性不可用。
// 纯限流（rate limit）与请求被取消不算——它们是瞬时的，换个模型不是正解。
// 专家预设模型的回退兜底（RunAgent）靠它判定。
func isModelUnavailableErr(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	if strings.Contains(s, "无法连接模型服务") {
		return !strings.Contains(s, "请求已取消") // 提供方连不上：换默认提供方有意义
	}
	quotaWords := []string{"余额", "额度", "欠费", "充值", "balance", "quota", "insufficient", "arrear"}
	hardWords := append(append([]string{}, quotaWords...),
		"unauthorized", "forbidden", "invalid api key", "authentication",
		"not exist", "not found", "no such model", "permission denied")
	for _, sc := range []int{400, 401, 402, 403, 404, 429} {
		if !strings.Contains(s, fmt.Sprintf("返回 %d", sc)) {
			continue
		}
		switch sc {
		case 429: // 限流类 429 只在说余额/额度时才算不可用
			return containsAnyFold(s, quotaWords)
		case 400: // 400 多为请求本身的问题；只有点名「模型不存在」这类才算
			// （错误前缀「模型服务返回…」自带"模型"二字，不能拿它当关键词）
			return containsAnyFold(s, []string{"not exist", "no such model", "unknown model",
				"invalid model", "model not found", "模型不存在", "未知模型", "无可用模型"})
		default: // 401/402/403/404：鉴权失败、欠费、禁止、模型不存在，一律算
			return true
		}
	}
	return containsAnyFold(s, hardWords)
}

// isRetryableModelErr 判断一次模型调用失败是否值得在**整步层**自动重试（
// agent.go 的 5 次 × 1/2/3/4s）。值得的是瞬态业务类：流中途断开（parseSSE
// 抛出的原始 IO 错误）、网关 5xx 与限流 429。请求级错误（400/401/402/403/404
// ——请求非法/鉴权失败/余额不足/模型不存在）换个时机结果一样；上下文超限交给
// overflowCompact 兜底——两者都不在此重试。调用方还需自行排除 ctx 取消
// （用户点了停止，绝不重试）。
//
// 连接类失败（"无法连接模型服务"，含报「请求已取消」形态的那一种）**不在这里
// 重试**：拨号层（streamProtoOnce → connRetryDelays）已经把同一地址 1/2/3/4/5s、
// 最多 6 次尝试做完了。两层叠乘会把最坏等待放大到 5×15=75 秒（分钟级），用户
// 会以为程序卡死；「连接建立不通」这类重试同一件事由拨号层做一次就够。
// 被取消的请求本来也不该重试（与原先一样不重试，既有语义不变）。
func isRetryableModelErr(err error) bool {
	if err == nil || isOverflowError(err) {
		return false
	}
	s := err.Error()
	// 连接类失败：拨号层已把阶梯跑完，整步层不再叠乘（见函数注释里的动机）
	if strings.Contains(s, "无法连接模型服务") || isConnFailure(err) {
		return false
	}
	for _, sc := range []int{400, 401, 402, 403, 404} {
		if strings.Contains(s, fmt.Sprintf("模型服务返回 %d", sc)) {
			return false
		}
	}
	return true
}

// containsAnyFold 大小写不敏感地判断 s 是否含 words 中任一词。
func containsAnyFold(s string, words []string) bool {
	ls := strings.ToLower(s)
	for _, w := range words {
		if strings.Contains(ls, strings.ToLower(w)) {
			return true
		}
	}
	return false
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content          string `json:"content"`
			Reasoning        string `json:"reasoning"`         // OpenAI 新版/部分网关
			ReasoningContent string `json:"reasoning_content"` // DeepSeek/GLM 风格
			ToolCalls        []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason any `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage,omitempty"`
}

// parseSSE 解析 text/event-stream：聚合按 index 分片的工具调用参数增量，
// 流结束后按序回调完整的工具调用。
func parseSSE(r io.Reader, cb StreamCallbacks) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	acc := map[int]*ToolCall{}
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Content != "" && cb.OnText != nil {
				cb.OnText(ch.Delta.Content)
			}
			// 思考增量：两个字段名都收（reasoning_content / reasoning）
			if rn := ch.Delta.ReasoningContent; rn != "" && cb.OnReasoning != nil {
				cb.OnReasoning(rn)
			} else if rn := ch.Delta.Reasoning; rn != "" && cb.OnReasoning != nil {
				cb.OnReasoning(rn)
			}
			for _, tc := range ch.Delta.ToolCalls {
				cur := acc[tc.Index]
				if cur == nil {
					cur = &ToolCall{Type: "function"}
					acc[tc.Index] = cur
				}
				if tc.ID != "" {
					cur.ID = tc.ID
				}
				if tc.Function.Name != "" {
					cur.Function.Name += tc.Function.Name
				}
				cur.Function.Arguments += tc.Function.Arguments
			}
		}
		// 流尾用量块（缓存命中率的数据来源）；网关不带 usage 时静默跳过
		if chunk.Usage != nil && cb.OnUsage != nil {
			cb.OnUsage(*chunk.Usage)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	indexes := make([]int, 0, len(acc))
	for i := range acc {
		indexes = append(indexes, i)
	}
	sort.Ints(indexes)
	if cb.OnToolCall != nil {
		for i, idx := range indexes {
			tc := acc[idx]
			if tc.ID == "" {
				tc.ID = fmt.Sprintf("call_%d", i)
			}
			if tc.Function.Name != "" {
				cb.OnToolCall(*tc)
			}
		}
	}
	return nil
}

// mockStream 是内置的离线演示模型：按会话形态演示一轮完整链路。
// 助理会话（带 dispatch_task 工具）演示「派发 → 汇报」；
// 任务会话依次发起 write、run_command 两个工具调用，
// 收到足够工具结果后输出总结文本，用于无 API Key 时验证主循环与工具链路。
func mockStream(req ChatRequest, cb StreamCallbacks) error {
	toolResults := 0
	for _, m := range req.Messages {
		if m.Role == "tool" {
			toolResults++
		}
	}
	isAssistant := false
	for _, t := range req.Tools {
		if t.Function.Name == "dispatch_task" {
			isAssistant = true
		}
	}
	emitCall := func(name string, args any) {
		raw, _ := json.Marshal(args)
		if cb.OnToolCall != nil {
			cb.OnToolCall(ToolCall{
				ID:       fmt.Sprintf("call_mock_%d", toolResults+1),
				Type:     "function",
				Function: ToolCallFunc{Name: name, Arguments: string(raw)},
			})
		}
	}
	if isAssistant {
		switch toolResults {
		case 0:
			emitCall("dispatch_task", map[string]string{
				"task":  "在演示工作区创建 mock-演示.txt 并写入一行问候，然后用 shell 确认文件存在",
				"title": "演示子任务",
			})
		default:
			segs := []string{
				"演示模式（数字助理）：",
				"我已把任务派发给子会话执行，它创建并确认了「mock-演示.txt」。",
				"主对话 → 派发 → 子会话执行 → 压缩摘要回传 的完整链路均正常。",
				"在设置中填入 API Key 并把 baseURL 改回真实服务地址，即可使用真实模型。",
			}
			if cb.OnText != nil {
				for _, seg := range segs {
					cb.OnText(seg)
					time.Sleep(80 * time.Millisecond)
				}
			}
		}
		return nil
	}
	switch toolResults {
	case 0:
		emitCall("write", map[string]string{
			"path":    "mock-演示.txt",
			"content": "这是演示模式创建的测试文件，用于验证主循环与工具链路。\n",
		})
	case 1:
		emitCall("run_command", map[string]any{
			"command": "echo shell-ok",
			"shell":   "cmd",
		})
	case 2:
		// 演示交付链路：产出成果文件后用 deliver_result 登记，前端出交付卡片
		emitCall("deliver_result", map[string]any{
			"files": []any{map[string]any{
				"path": "mock-演示.txt",
				"note": "演示模式生成的成果文件",
			}},
			"summary": "本轮演示的交付成果",
		})
	default:
		segs := []string{
			"演示模式：",
			"我已调用 write 创建了「mock-演示.txt」，又用 run_command 执行了 cmd 命令，",
			"并用 deliver_result 登记了交付成果（见交付卡片）；文件快照、交付与回退链路均正常。",
			"在设置中填入 API Key 并把 baseURL 改回真实服务地址，即可使用真实模型。",
		}
		if cb.OnText != nil {
			for _, seg := range segs {
				cb.OnText(seg)
				time.Sleep(80 * time.Millisecond)
			}
		}
	}
	return nil
}
