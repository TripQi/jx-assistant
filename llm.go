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
	// 思考控制：DeepSeek/智谱等用 thinking{type}，OpenAI o/gpt-5 系用 reasoning_effort
	Thinking        *ThinkingCfg `json:"thinking,omitempty"`
	ReasoningEffort string       `json:"reasoning_effort,omitempty"`
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

// ThinkingCfg 是 DeepSeek/GLM 风格的思考开关参数。
type ThinkingCfg struct {
	Type string `json:"type"` // enabled | disabled
}

// ApplyThinking 按档位设置请求的思考参数；空串=不干预（用服务端默认）。
// enabled/disabled → thinking{type}；其余非空档位（low/medium/high/max 等，
// 以服务端 supported_levels 为准）→ reasoning_effort 原样透传。
func (r *ChatRequest) ApplyThinking(level string) {
	switch level {
	case "enabled", "disabled":
		r.Thinking = &ThinkingCfg{Type: level}
		r.ReasoningEffort = ""
	case "":
		// 不干预
	default:
		r.Thinking = nil
		r.ReasoningEffort = level
	}
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
type connFailure struct{ err error }

func (e *connFailure) Error() string { return e.err.Error() }
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

// ErrNoModelChannelText 没有任何可用模型通道时的报错文案。出厂不预置通道
// （见 config.go DefaultConfig），新装未接公司通道时发送必然命中它。前端按
// 这段文字识别并挂「接入公司通道」按钮（web/app-chat.js 的 NO_CHANNEL_HINT），
// 改文案要两边一起改。
const ErrNoModelChannelText = "还没有可用的模型通道：点「接入公司通道」接入公司通道，或到「设置 · 模型」自己添加一个提供方"

// errNoModelChannel 上面文案的哨兵错误：isRetryableModelErr 据此判定「配置类
// 错误、重试无用」（不再白跑 5 次自动重试）。
var errNoModelChannel = errors.New(ErrNoModelChannelText)

// StreamChat 向解析后的 LLM 目标发起一轮流式对话。
// target.BaseURL 配为 "mock" 时走内置演示模型，便于离线验证整条链路。
// target.Protocol 选渠道协议（""＝OpenAI 兼容 chat/completions、anthropic、
// responses，实现见 llmproto.go）；配了外网备用地址（AltBaseURL，公司通道由
// 公告下发）且主地址连接失败时，自动改走备用地址重试一次，成功后粘住备用
// 地址直到它再失败——故障转移对三种协议一视同仁。
func StreamChat(ctx context.Context, target LLMTarget, req ChatRequest, cb StreamCallbacks) error {
	if target.BaseURL == "mock" {
		return mockStream(req, cb)
	}
	// 还没有任何模型通道（新装未接公司通道、也没自己添加）：给出可执行的指引，
	// 而不是去请求一个空地址报底层 URL 错误。前端按 ErrNoModelChannelText 的
	// 文案特征在报错条里挂「接入公司通道」按钮（web/app-chat.js showRunError），
	// 两处文案要保持一致。
	if strings.TrimSpace(target.BaseURL) == "" {
		return errNoModelChannel
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

// streamProtoOnce 向单个地址发一轮流式请求（StreamChat 的主/备用共用体，
// 三种协议共用：端点路径、鉴权头与流解析都来自 proto）。
func streamProtoOnce(ctx context.Context, baseURL, apiKey string, body []byte, proto llmProtocol, cb StreamCallbacks) error {
	url := baseURL + proto.path
	if cb.OnRequest != nil {
		cb.OnRequest(url, body)
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	proto.setAuth(apiKey, httpReq.Header)
	resp, err := streamHTTP.Do(httpReq)
	if err != nil {
		if resp != nil {
			_ = resp.Body.Close()
		}
		if ctx.Err() != nil {
			return err // 调用方取消：原样返回，不算连接故障
		}
		return &connFailure{err: fmt.Errorf("无法连接模型服务：%s", connErrText(err))}
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

// connErrText 把底层连接错误整理成一句人话（超时/拒连/证书各说各的）。
func connErrText(err error) string {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "请求超时，模型服务没有在预期时间内响应"
	case strings.Contains(msg, "context canceled"):
		return "请求已取消"
	case strings.Contains(msg, "no such host"):
		return "域名解析失败，请检查 API 地址（baseURL）是否正确"
	case strings.Contains(msg, "certificate"), strings.Contains(msg, "x509"):
		return "TLS 证书校验失败，请检查代理或证书配置"
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "actively refused"):
		return "连接被拒绝，请确认 API 地址与端口可达"
	}
	return truncateRunes(strings.Join(strings.Fields(msg), " "), 300)
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

// isRetryableModelErr 判断一次模型调用失败是否值得自动重试。值得的是瞬态
// 网络类：连接失败/超时（"无法连接模型服务"）、流中途断开（parseSSE 抛出的
// 原始 IO 错误）、网关 5xx 与限流 429。请求级错误（400/401/402/403/404——
// 请求非法/鉴权失败/余额不足/模型不存在）换个时机结果一样；上下文超限交给
// overflowCompact 兜底——两者都不在此重试。调用方还需自行排除 ctx 取消
// （用户点了停止，绝不重试）。
func isRetryableModelErr(err error) bool {
	if err == nil || isOverflowError(err) {
		return false
	}
	// 配置类错误（没有任何模型通道）重试一万次也一样：直接报错，让前端把
	// 「接入公司通道」按钮给出来，别让用户干等 5 次自动重试
	if errors.Is(err, errNoModelChannel) {
		return false
	}
	s := err.Error()
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
