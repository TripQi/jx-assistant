package main

// llmproto.go —— 渠道协议扩展：Anthropic 原生 /v1/messages 与 OpenAI
// Responses /v1/responses 两个协议实现。与 llm.go 的 chat/completions 实现
// 同接口（llmProtocol：端点路径 + 鉴权头 + 请求体转换 + SSE 流式解析），
// StreamChat 主流程（mock、主备地址故障转移、断开器登记、错误整理）三协议
// 完全共用——agent/上下文/工具循环对协议零感知，仍是统一的 Message/ToolCall
// 形态，转换只发生在「发出前的最后一步」。
//
// 选型入口：设置·模型 → 渠道卡片「协议类型」（ProviderCfg.Protocol，空串 =
// OpenAI 兼容 chat/completions，存量配置不受影响）。baseURL 约定与 OpenAI
// 一致：填到版本前缀（如 https://api.anthropic.com/v1），实现按协议拼端点。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// anthropicVersionHeader Anthropic 要求的协议版本头（鉴权三件套之一）。
const anthropicVersionHeader = "2023-06-01"

// ---------- llmProtocol：协议实现的统一形状 ----------

// llmProtocol 是一种渠道协议的全部差异点。StreamChat 持有一个实现跑完整个
// 调用流程，主备地址故障转移对三种协议一视同仁。
type llmProtocol struct {
	path    string                                                  // 拼在 baseURL 后的端点路径
	setAuth func(apiKey string, h http.Header)                      // 鉴权头（Bearer / x-api-key+version）
	marshal func(target LLMTarget, req ChatRequest) ([]byte, error) // 统一消息 → 协议请求体
	parse   func(r io.Reader, cb StreamCallbacks) error             // 协议 SSE 流 → 统一回调
}

// protocolOf 按渠道协议类型取实现；未知值一律回落 OpenAI 兼容（老配置只有空串）。
func protocolOf(kind string) llmProtocol {
	switch kind {
	case "anthropic":
		return anthropicProtocol()
	case "responses":
		return responsesProtocol()
	default:
		return openAIProtocol()
	}
}

// openAIProtocol 既有 chat/completions 渠道：报文与重构前逐字节一致
// （fillReasoningStub / stream_options 等原有行为原样保留，见 llmproto_test
// 的回归测试）。
func openAIProtocol() llmProtocol {
	return llmProtocol{
		path: "/chat/completions",
		setAuth: func(apiKey string, h http.Header) {
			if apiKey != "" {
				h.Set("Authorization", "Bearer "+apiKey)
			}
		},
		marshal: func(target LLMTarget, req ChatRequest) ([]byte, error) {
			req.Model = target.Model
			req.Stream = true
			req.StreamOptions = &StreamOptionsCfg{IncludeUsage: true}
			fillReasoningStub(&req)
			return json.Marshal(req)
		},
		parse: parseSSE,
	}
}

// scanSSE 通用 SSE 分帧：按空行切事件、多条 data: 行按规范拼 \n，event:/注释
// 行忽略。Anthropic 与 Responses 的事件 JSON 自带 type 字段，event: 行不依赖。
func scanSSE(r io.Reader, fn func(data string) error) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	var data []string
	flush := func() error {
		if len(data) == 0 {
			return nil
		}
		payload := strings.Join(data, "\n")
		data = data[:0]
		return fn(payload)
	}
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == "":
			if err := flush(); err != nil {
				return err
			}
		case strings.HasPrefix(line, "data:"):
			line = strings.TrimPrefix(line, "data:")
			line = strings.TrimPrefix(line, " ") // SSE 规范只去一个前导空格
			data = append(data, line)
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	return flush()
}

// ---------- Anthropic 原生协议（/v1/messages） ----------

// anthropicProtocol 鉴权头：x-api-key + anthropic-version（官方口径；多数
// new-api 类中转也认这组头）。
func anthropicProtocol() llmProtocol {
	return llmProtocol{
		path: "/messages",
		setAuth: func(apiKey string, h http.Header) {
			h.Set("anthropic-version", anthropicVersionHeader)
			if apiKey != "" {
				h.Set("x-api-key", apiKey)
			}
		},
		marshal: marshalAnthropic,
		parse:   parseAnthropicSSE,
	}
}

// anthropicRequest /v1/messages 请求体。max_tokens 是必填项（与 OpenAI 可省
// 不同），由 marshalAnthropic 保证非零。
type anthropicRequest struct {
	Model        string                `json:"model"`
	System       string                `json:"system,omitempty"`
	Messages     []anthropicMessage    `json:"messages"`
	MaxTokens    int                   `json:"max_tokens"`
	Temperature  *float64              `json:"temperature,omitempty"`
	Tools        []anthropicTool       `json:"tools,omitempty"`
	ToolChoice   *anthropicToolChoice  `json:"tool_choice,omitempty"`
	Thinking     *anthropicThinkingCfg `json:"thinking,omitempty"`
	OutputConfig *anthropicOutputCfg   `json:"output_config,omitempty"`
	Stream       bool                  `json:"stream"`
}

// anthropicMessage 消息体：role 只剩 user/assistant（system 提到顶层参数），
// content 是 string 或 block 数组。
type anthropicMessage struct {
	Role    string `json:"role"`
	Content any    `json:"content"` // string | []anthropicBlock
}

// anthropicBlock content block：text/image/tool_use/tool_result 四种形态共用
// 一个结构（omitempty 收掉不相干字段）。
type anthropicBlock struct {
	Thinking  string `json:"thinking,omitempty"`
	Signature string `json:"signature,omitempty"`
	Data      string `json:"data,omitempty"`
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	// image 形态
	Source *anthropicSource `json:"source,omitempty"`
	// tool_use 形态
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result 形态
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
}

// MarshalJSON 签名思考即使正文为空也必须保留 thinking 字段，不能由 omitempty 丢掉。
func (b anthropicBlock) MarshalJSON() ([]byte, error) {
	type plain anthropicBlock
	if b.Type == "thinking" {
		return json.Marshal(struct {
			Type      string `json:"type"`
			Thinking  string `json:"thinking"`
			Signature string `json:"signature"`
		}{b.Type, b.Thinking, b.Signature})
	}
	return json.Marshal(plain(b))
}

// anthropicSource 图片来源：内联 base64（本地视觉内容）或 URL。
type anthropicSource struct {
	Type      string `json:"type"` // base64 | url
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

// anthropicTool 工具定义：name/description/input_schema（对应 OpenAI 的
// function.parameters）。
type anthropicTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

// anthropicToolChoice tool_choice：auto/any/tool/none。
type anthropicToolChoice struct {
	Type string `json:"type"` // auto | any | tool | none
	Name string `json:"name,omitempty"`
}

// anthropicThinkingCfg 思考配置：官方现行口径是 adaptive（让模型自己决定
// 思考多深），深浅由顶层 output_config.effort 控制；none 档用 disabled。
type anthropicThinkingCfg struct {
	Type string `json:"type"` // adaptive | disabled
}

// anthropicOutputCfg 思考深浅：官方现行口径（与 thinking{type:adaptive} 配套）。
type anthropicOutputCfg struct {
	Effort string `json:"effort"` // low | medium | high | xhigh | max
}

// marshalAnthropic 统一消息 → /v1/messages 请求体。
func marshalAnthropic(target LLMTarget, req ChatRequest) ([]byte, error) {
	sys, msgs, err := toAnthropicMessages(req.Messages)
	if err != nil {
		return nil, err
	}
	out := anthropicRequest{
		Model:       target.Model,
		System:      sys,
		Messages:    msgs,
		Temperature: req.Temperature,
		Stream:      true,
	}
	// max_tokens：请求显式值 > 模型目录的最大输出 > 保守默认 8192
	out.MaxTokens = req.MaxTokens
	if out.MaxTokens <= 0 {
		out.MaxTokens = target.MaxOutput
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = 8192
	}
	// 思考档位：none → thinking{type:disabled}；其余五档 → adaptive +
	// output_config.effort 原样发（官方推荐口径，不再折成 budget_tokens）。
	switch lv := req.ReasoningEffort; lv {
	case "":
		// 不干预（跟随服务端默认）
	case "none":
		out.Thinking = &anthropicThinkingCfg{Type: "disabled"}
	default:
		out.Thinking = &anthropicThinkingCfg{Type: "adaptive"}
		out.OutputConfig = &anthropicOutputCfg{Effort: lv}
		// 思考模式下 temperature 只接受 1：显式带了非 1 值会 400，去掉交给服务端默认
		out.Temperature = nil
	}
	for _, t := range req.Tools {
		schema := t.Function.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object"}
		}
		out.Tools = append(out.Tools, anthropicTool{
			Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema,
		})
	}
	if tc := toAnthropicToolChoice(req.ToolChoice); tc != nil {
		out.ToolChoice = tc
	}
	return json.Marshal(out)
}

// toAnthropicToolChoice OpenAI 语义的 tool_choice（auto/none/required/具名）
// → Anthropic 形态；空串不干预（默认 auto）。
func toAnthropicToolChoice(choice string) *anthropicToolChoice {
	switch choice {
	case "":
		return nil
	case "auto":
		return &anthropicToolChoice{Type: "auto"}
	case "none":
		return &anthropicToolChoice{Type: "none"}
	case "required":
		return &anthropicToolChoice{Type: "any"}
	default:
		return &anthropicToolChoice{Type: "tool", Name: choice}
	}
}

// toAnthropicMessages 统一消息视图 → Anthropic messages + 顶层 system。
// 规则：
//   - system 消息合并为顶层 system 参数（多段以空行相连）；
//   - assistant 的 tool_calls → tool_use block；原生 thinking/signature 原样回传；
//   - tool 消息 → user 消息里的 tool_result block，连续多条 tool 合并进
//     同一条 user 消息（Anthropic 的标准形态）；
//   - 图片：data: URL 内联 base64，http(s) URL 走 url source。
func toAnthropicMessages(msgs []Message) (string, []anthropicMessage, error) {
	var sysParts []string
	out := make([]anthropicMessage, 0, len(msgs))
	appendUser := func(blocks []anthropicBlock) {
		if len(blocks) > 0 {
			out = append(out, anthropicMessage{Role: "user", Content: blocks})
		}
	}
	var pendingResults []anthropicBlock // 连续 tool 消息攒批，遇到下一条非 tool 消息或收尾时落成 user
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s := m.Text(); s != "" {
				sysParts = append(sysParts, s)
			}
		case "user":
			appendUser(pendingResults)
			pendingResults = nil
			blocks, err := anthropicUserBlocks(m)
			if err != nil {
				return "", nil, err
			}
			appendUser(blocks)
		case "assistant":
			appendUser(pendingResults)
			pendingResults = nil
			var blocks []anthropicBlock
			for _, raw := range m.AnthropicThinking {
				var block anthropicBlock
				if err := json.Unmarshal(raw, &block); err != nil {
					return "", nil, err
				}
				blocks = append(blocks, block)
			}
			if s := m.Text(); s != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: s})
			}
			for _, tc := range m.ToolCalls {
				input := json.RawMessage(tc.Function.Arguments)
				if !json.Valid(input) {
					// 参数不是合法 JSON（模型半截输出被中断的历史）：包一层原样带回，
					// 别让整份请求 marshal 失败
					input = json.RawMessage(`{"_raw":` + string(mustJSON(tc.Function.Arguments)) + `}`)
				}
				blocks = append(blocks, anthropicBlock{
					Type: "tool_use", ID: tc.ID, Name: tc.Function.Name, Input: input,
				})
			}
			if len(blocks) > 0 { // 空内容消息 Anthropic 直接 400，跳过
				out = append(out, anthropicMessage{Role: "assistant", Content: blocks})
			}
		case "tool":
			content := m.Text()
			if content == "" && m.Content != nil {
				content = fmt.Sprint(m.Content)
			}
			pendingResults = append(pendingResults, anthropicBlock{
				Type: "tool_result", ToolUseID: m.ToolCallID, Content: content,
			})
		}
	}
	appendUser(pendingResults)
	return strings.Join(sysParts, "\n\n"), out, nil
}

// anthropicUserBlocks user 消息内容（string 或多模态 parts）→ content blocks。
func anthropicUserBlocks(m Message) ([]anthropicBlock, error) {
	if s, ok := m.Content.(string); ok {
		if s == "" {
			return nil, nil
		}
		return []anthropicBlock{{Type: "text", Text: s}}, nil
	}
	parts, ok := m.Content.([]ContentPart)
	if !ok {
		return nil, nil
	}
	var blocks []anthropicBlock
	for _, p := range parts {
		switch p.Type {
		case "text":
			if p.Text != "" {
				blocks = append(blocks, anthropicBlock{Type: "text", Text: p.Text})
			}
		case "image_url":
			if p.ImageURL == nil || p.ImageURL.URL == "" {
				continue
			}
			src, err := anthropicImageSource(p.ImageURL.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, anthropicBlock{Type: "image", Source: src})
		}
	}
	return blocks, nil
}

// anthropicImageSource data: URL → base64 source；http(s) → url source。
// data URL 缺 media type 时按 png 处理（客户端内联视觉内容都带全类型）。
func anthropicImageSource(url string) (*anthropicSource, error) {
	if strings.HasPrefix(url, "data:") {
		rest := strings.TrimPrefix(url, "data:")
		media, payload, ok := strings.Cut(rest, ",")
		if !ok {
			return nil, fmt.Errorf("图片 data URL 格式非法")
		}
		mediaType := strings.TrimSpace(strings.Split(media, ";")[0])
		if mediaType == "" {
			mediaType = "image/png"
		}
		return &anthropicSource{Type: "base64", MediaType: mediaType, Data: payload}, nil
	}
	return &anthropicSource{Type: "url", URL: url}, nil
}

// anthropicUsageEvent /v1/messages 的 usage 形态（message_start 与
// message_delta 各出现一次，字段取并集/后者覆盖 output）。
type anthropicUsageEvent struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

// anthropicStreamEvent 流事件的可辨识联合：按 type 分发，只声明用得到的字段。
type anthropicStreamEvent struct {
	Type    string `json:"type"`
	Message struct {
		Usage *anthropicUsageEvent `json:"usage"`
	} `json:"message"`
	Index        int             `json:"index"`
	ContentBlock *anthropicBlock `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"` // text_delta | thinking_delta | input_json_delta
		Text        string `json:"text"`
		Thinking    string `json:"thinking"`
		Signature   string `json:"signature"`
		StopReason  string `json:"stop_reason"`
		PartialJSON string `json:"partial_json"`
	} `json:"delta"`
	Usage *anthropicUsageEvent `json:"usage"` // message_delta 携带
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// parseAnthropicSSE 聚合 /v1/messages 的流式事件体系（message_start /
// content_block_start / content_block_delta[text_delta|thinking_delta|
// input_json_delta] / content_block_stop / message_delta / message_stop），
// 映射成与 chat/completions 一致的回调：text_delta→OnText、thinking_delta→
// OnReasoning、tool_use 块聚合 →OnToolCall、usage（含 cache_read/cache_creation）
// →OnUsage。
func parseAnthropicSSE(r io.Reader, cb StreamCallbacks) error {
	agg := &anthropicStreamAgg{cb: cb}
	err := scanSSE(r, func(payload string) error {
		var ev anthropicStreamEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return nil // 非 JSON 载荷（注释/心跳）跳过
		}
		return agg.event(ev)
	})
	if err != nil {
		return err
	}
	agg.finish()
	if cb.OnCompletion != nil {
		cb.OnCompletion(agg.completion)
	}
	return agg.err
}

// anthropicStreamAgg 单条流的聚合状态。
type anthropicStreamAgg struct {
	thinking map[int]*anthropicBlock
	cb       StreamCallbacks
	usage    anthropicUsageEvent
	hasUsage bool
	blocks   map[int]*anthropicToolAgg // content_block index → 聚合中的 tool_use
	order    []int                     // 工具块出现的先后（回调按序）
	err      error

	completion StreamCompletion
}

type anthropicToolAgg struct {
	id   string
	name string
	args strings.Builder
}

func (a *anthropicStreamAgg) event(ev anthropicStreamEvent) error {
	switch ev.Type {
	case "message_start":
		if ev.Message.Usage != nil {
			a.usage = *ev.Message.Usage
			a.hasUsage = true
		}
	case "content_block_start":
		if ev.ContentBlock != nil && (ev.ContentBlock.Type == "thinking" || ev.ContentBlock.Type == "redacted_thinking") {
			if a.thinking == nil {
				a.thinking = map[int]*anthropicBlock{}
			}
			block := *ev.ContentBlock
			a.thinking[ev.Index] = &block
		}
		if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
			if a.blocks == nil {
				a.blocks = map[int]*anthropicToolAgg{}
			}
			a.blocks[ev.Index] = &anthropicToolAgg{id: ev.ContentBlock.ID, name: ev.ContentBlock.Name}
			a.order = append(a.order, ev.Index)
		}
	case "content_block_delta":
		switch ev.Delta.Type {
		case "text_delta":
			if ev.Delta.Text != "" && a.cb.OnText != nil {
				a.cb.OnText(ev.Delta.Text)
			}
		case "signature_delta":
			if block := a.thinking[ev.Index]; block != nil {
				block.Signature += ev.Delta.Signature
			}
		case "thinking_delta":
			if block := a.thinking[ev.Index]; block != nil {
				block.Thinking += ev.Delta.Thinking
			}
			if ev.Delta.Thinking != "" && a.cb.OnReasoning != nil {
				a.cb.OnReasoning(ev.Delta.Thinking)
			}
		case "input_json_delta":
			if blk := a.blocks[ev.Index]; blk != nil {
				blk.args.WriteString(ev.Delta.PartialJSON)
			}
		}
	case "content_block_stop":
		if block := a.thinking[ev.Index]; block != nil {
			if a.cb.OnOutputItem != nil {
				a.cb.OnOutputItem("anthropic", mustJSON(block))
			}
			delete(a.thinking, ev.Index)
		}
		a.flushTool(ev.Index)
	case "message_delta":
		if ev.Delta.StopReason != "" {
			a.completion.Reason = ev.Delta.StopReason
		}
		// 流尾 usage：output_tokens 为最终值；缓存字段 message_start 已带，取最大
		if ev.Usage != nil {
			a.hasUsage = true
			if ev.Usage.OutputTokens > a.usage.OutputTokens {
				a.usage.OutputTokens = ev.Usage.OutputTokens
			}
			if ev.Usage.InputTokens > a.usage.InputTokens {
				a.usage.InputTokens = ev.Usage.InputTokens
			}
			if ev.Usage.CacheReadInputTokens > a.usage.CacheReadInputTokens {
				a.usage.CacheReadInputTokens = ev.Usage.CacheReadInputTokens
			}
			if ev.Usage.CacheCreationInputTokens > a.usage.CacheCreationInputTokens {
				a.usage.CacheCreationInputTokens = ev.Usage.CacheCreationInputTokens
			}
		}
	case "message_stop":
		a.completion.Ended = true
		// 收尾在 finish 统一做（流可能缺 message_stop，也能把工具块扫出来）
	case "error":
		if ev.Error != nil && ev.Error.Message != "" {
			a.err = fmt.Errorf("模型服务流中报错：%s", ev.Error.Message)
			return a.err
		}
	}
	return nil
}

// flushTool 某个 tool_use 块结束：参数聚合完整，立即按序回调。
func (a *anthropicStreamAgg) flushTool(index int) {
	blk := a.blocks[index]
	if blk == nil {
		return
	}
	delete(a.blocks, index)
	if a.cb.OnToolCall == nil {
		return
	}
	args := blk.args.String()
	if args == "" {
		args = "{}"
	}
	a.cb.OnToolCall(ToolCall{ID: blk.id, Type: "function", Function: ToolCallFunc{Name: blk.name, Arguments: args}})
}

// finish 流结束：没等到 content_block_stop 的工具块（异常断流）也按序吐出；
// usage 归一成统一 Usage（缓存读取/写入各占独立字段，CacheHit 只认读取）。
func (a *anthropicStreamAgg) finish() {
	for _, idx := range a.order {
		a.flushTool(idx)
	}
	if a.hasUsage && a.cb.OnUsage != nil {
		a.cb.OnUsage(Usage{
			PromptTokens:        a.usage.InputTokens,
			CompletionTokens:    a.usage.OutputTokens,
			TotalTokens:         a.usage.InputTokens + a.usage.OutputTokens,
			CacheReadTokens:     a.usage.CacheReadInputTokens,
			CacheCreationTokens: a.usage.CacheCreationInputTokens,
		})
	}
}

// ---------- OpenAI Responses 协议（/v1/responses） ----------

// responsesProtocol 鉴权仍是 Bearer（Responses 是 OpenAI 自家 API）。
func responsesProtocol() llmProtocol {
	return llmProtocol{
		path: "/responses",
		setAuth: func(apiKey string, h http.Header) {
			if apiKey != "" {
				h.Set("Authorization", "Bearer "+apiKey)
			}
		},
		marshal: marshalResponses,
		parse:   parseResponsesSSE,
	}
}

// responsesRequest /v1/responses 请求体。
type responsesRequest struct {
	Model           string               `json:"model"`
	Instructions    string               `json:"instructions,omitempty"`
	Input           []responsesInputItem `json:"input"`
	Tools           []responsesTool      `json:"tools,omitempty"`
	ToolChoice      any                  `json:"tool_choice,omitempty"`
	Reasoning       *responsesReasoning  `json:"reasoning,omitempty"`
	MaxOutputTokens int                  `json:"max_output_tokens,omitempty"`
	Temperature     *float64             `json:"temperature,omitempty"`
	Stream          bool                 `json:"stream"`
}

// responsesInputItem input 数组元素：消息形态（role+content）或工具往返形态
// （function_call / function_call_output，type 必填）。
type responsesInputItem struct {
	Raw       json.RawMessage `json:"-"`
	Type      string          `json:"type,omitempty"` // function_call | function_call_output
	Role      string          `json:"role,omitempty"` // user | assistant
	Content   any             `json:"content,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Arguments string          `json:"arguments,omitempty"`
	Output    string          `json:"output,omitempty"`
}

// MarshalJSON 对 reasoning 仅发送 summary 文本，其它 output item 保留配对字段。
func (i responsesInputItem) MarshalJSON() ([]byte, error) {
	if i.Raw != nil {
		var item struct {
			Type    string `json:"type"`
			Summary []struct {
				Text string `json:"text"`
			} `json:"summary"`
		}
		if err := json.Unmarshal(i.Raw, &item); err != nil {
			return nil, err
		}
		if item.Type == "reasoning" {
			summary := make([]map[string]string, 0, len(item.Summary))
			for _, part := range item.Summary {
				summary = append(summary, map[string]string{"type": "summary_text", "text": part.Text})
			}
			return json.Marshal(struct {
				Type    string              `json:"type"`
				Summary []map[string]string `json:"summary"`
			}{"reasoning", summary})
		}
		return i.Raw, nil
	}
	type plain responsesInputItem
	return json.Marshal(plain(i))
}

// responsesInputContent 消息形态的 content part（input_text / input_image；
// assistant 历史正文用裸字符串 content，不走这结构）。
type responsesInputContent struct {
	Type     string `json:"type"` // input_text | input_image
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
}

// responsesTool Responses 的 function 工具：name/description/parameters 平铺
// （不嵌 function 层——与 chat/completions 的主要差异之一）。
type responsesTool struct {
	Type        string         `json:"type"` // function
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

// responsesReasoning 思考控制：effort 六档原样发（none…max）。
type responsesReasoning struct {
	Effort string `json:"effort"` // none | low | medium | high | xhigh | max
}

// marshalResponses 统一消息 → /v1/responses 请求体。
func marshalResponses(target LLMTarget, req ChatRequest) ([]byte, error) {
	instructions, items, err := toResponsesInput(req.Messages)
	if err != nil {
		return nil, err
	}
	// 兼容渠道使用 summary-only 回传，不请求加密推理或依赖服务端存储。
	out := responsesRequest{
		Model:        target.Model,
		Instructions: instructions,
		Input:        items,
		Temperature:  req.Temperature,
		Stream:       true,
	}
	// 思考档位六档原样发（none…max），空串=不干预
	if effort := req.ReasoningEffort; effort != "" {
		out.Reasoning = &responsesReasoning{Effort: effort}
	}
	if n := req.MaxTokens; n > 0 {
		out.MaxOutputTokens = n
	} else if target.MaxOutput > 0 {
		out.MaxOutputTokens = target.MaxOutput
	}
	for _, t := range req.Tools {
		out.Tools = append(out.Tools, responsesTool{
			Type: "function", Name: t.Function.Name,
			Description: t.Function.Description, Parameters: t.Function.Parameters,
		})
	}
	switch tc := req.ToolChoice; tc {
	case "", "auto":
		// 默认 auto，省略
	case "required":
		out.ToolChoice = "required"
	case "none":
		out.ToolChoice = "none"
	default:
		out.ToolChoice = map[string]any{"type": "function", "name": tc}
	}
	return json.Marshal(out)
}

// toResponsesInput 统一消息 → instructions + input 数组。
// system 消息合并进 instructions；assistant 的 tool_calls 每个调用一条
// function_call item；tool 消息一条 function_call_output item。
func toResponsesInput(msgs []Message) (string, []responsesInputItem, error) {
	var sysParts []string
	items := make([]responsesInputItem, 0, len(msgs))
	for _, m := range msgs {
		switch m.Role {
		case "system":
			if s := m.Text(); s != "" {
				sysParts = append(sysParts, s)
			}
		case "user":
			if s, ok := m.Content.(string); ok {
				items = append(items, responsesInputItem{Role: "user", Content: s})
				continue
			}
			parts, ok := m.Content.([]ContentPart)
			if !ok {
				continue
			}
			var content []responsesInputContent
			for _, p := range parts {
				switch p.Type {
				case "text":
					content = append(content, responsesInputContent{Type: "input_text", Text: p.Text})
				case "image_url":
					if p.ImageURL != nil && p.ImageURL.URL != "" {
						content = append(content, responsesInputContent{Type: "input_image", ImageURL: p.ImageURL.URL})
					}
				}
			}
			if len(content) > 0 {
				items = append(items, responsesInputItem{Role: "user", Content: content})
			}
		case "assistant":
			if len(m.ResponsesItems) > 0 {
				for _, raw := range m.ResponsesItems {
					items = append(items, responsesInputItem{Raw: raw})
				}
				continue // 原生 output 已含正文和工具，不能重复生成
			}
			if s := m.Text(); s != "" {
				items = append(items, responsesInputItem{Role: "assistant", Content: s})
			}
			for _, tc := range m.ToolCalls {
				items = append(items, responsesInputItem{
					Type: "function_call", CallID: tc.ID,
					Name: tc.Function.Name, Arguments: tc.Function.Arguments,
				})
			}
		case "tool":
			content := m.Text()
			if content == "" && m.Content != nil {
				content = fmt.Sprint(m.Content)
			}
			items = append(items, responsesInputItem{
				Type: "function_call_output", CallID: m.ToolCallID, Output: content,
			})
		}
	}
	return strings.Join(sysParts, "\n\n"), items, nil
}

// responsesUsageEvent /v1/responses 的 usage 形态（response.completed 携带）。
type responsesUsageEvent struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

// responsesStreamEvent 流事件的可辨识联合：按 type 前缀分发。
type responsesStreamEvent struct {
	Type        string `json:"type"`
	Delta       string `json:"delta"`   // output_text.delta / reasoning delta 的增量
	Message     string `json:"message"` // 裸 error 事件的顶层 message
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Item        *struct {
		Type      string `json:"type"` // function_call | message | reasoning
		ID        string `json:"id"`   // fc_…
		CallID    string `json:"call_id"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"item"`
	Response *struct {
		Usage *responsesUsageEvent `json:"usage"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
		IncompleteDetails *struct {
			Reason string `json:"reason"`
		} `json:"incomplete_details"`
	} `json:"response"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// responsesReplayError 保留上游错误，不自动切换 summary-only 策略。
func responsesReplayError(err error) error {
	if err == nil {
		return nil
	}
	text := strings.ToLower(err.Error())
	if strings.Contains(text, "encrypted_content") || strings.Contains(text, "encrypted reasoning") {
		return fmt.Errorf("%w（该渠道要求加密推理回传；当前 Responses 使用 summary-only 策略，不自动改为 encrypted_content，请检查渠道协议要求）", err)
	}
	return err
}

// parseResponsesSSE 聚合 /v1/responses 的事件体系：response.output_text.delta →
// OnText；response.reasoning_summary_text.delta（及 reasoning_text.delta）→
// OnReasoning；response.output_item.added(function_call) +
// response.function_call_arguments.delta 聚合 → OnToolCall；
// response.completed 的 usage → OnUsage；failed/incomplete → 错误。
func parseResponsesSSE(r io.Reader, cb StreamCallbacks) error {
	agg := &responsesStreamAgg{cb: cb, byItem: map[string]*responsesToolAgg{}, byIndex: map[int]*responsesToolAgg{}}
	err := scanSSE(r, func(payload string) error {
		var ev responsesStreamEvent
		if json.Unmarshal([]byte(payload), &ev) != nil {
			return nil
		}
		var raw struct {
			Item json.RawMessage `json:"item"`
		}
		if ev.Type == "response.output_item.done" && cb.OnOutputItem != nil {
			if json.Unmarshal([]byte(payload), &raw) == nil && len(raw.Item) > 0 && string(raw.Item) != "null" {
				cb.OnOutputItem("responses", raw.Item)
			}
		}
		return agg.event(ev)
	})
	if err != nil {
		return responsesReplayError(err)
	}
	agg.finish()
	return responsesReplayError(agg.err)
}

type responsesStreamAgg struct {
	cb      StreamCallbacks
	byItem  map[string]*responsesToolAgg // item_id → 聚合中的 function_call
	byIndex map[int]*responsesToolAgg    // output_index → 同上（done 帧不带 item_id 的兜底）
	order   []*responsesToolAgg
	usage   *Usage
	err     error
}

type responsesToolAgg struct {
	callID string
	name   string
	args   strings.Builder
	done   bool
}

// toolFor 按 item_id（优先）或 output_index 找到聚合中的工具调用；命中后把
// 两个键都指向同一实例（done/delta 帧可能只带其中一种标识，双向登记保证
// 殊途同归，不会聚合出两份）。
func (a *responsesStreamAgg) toolFor(itemID string, index int) *responsesToolAgg {
	var t *responsesToolAgg
	if itemID != "" {
		t = a.byItem[itemID]
	}
	if t == nil {
		t = a.byIndex[index]
	}
	if t == nil {
		t = &responsesToolAgg{}
		a.order = append(a.order, t)
	}
	if itemID != "" {
		a.byItem[itemID] = t
	}
	a.byIndex[index] = t
	return t
}

func (a *responsesStreamAgg) event(ev responsesStreamEvent) error {
	switch {
	case ev.Type == "response.output_text.delta":
		if ev.Delta != "" && a.cb.OnText != nil {
			a.cb.OnText(ev.Delta)
		}
	case ev.Type == "response.reasoning_summary_text.delta",
		ev.Type == "response.reasoning_summary.part.delta",
		ev.Type == "response.reasoning_text.delta":
		if ev.Delta != "" && a.cb.OnReasoning != nil {
			a.cb.OnReasoning(ev.Delta)
		}
	case ev.Type == "response.output_item.added":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			// added 帧的 item_id 藏在 item.id 里（顶层通常没有），两个都试
			id := ev.ItemID
			if id == "" {
				id = ev.Item.ID
			}
			t := a.toolFor(id, ev.OutputIndex)
			if t.callID == "" {
				t.callID = ev.Item.CallID
				if t.callID == "" {
					t.callID = ev.Item.ID
				}
			}
			t.name = ev.Item.Name
		}
	case ev.Type == "response.function_call_arguments.delta":
		t := a.toolFor(ev.ItemID, ev.OutputIndex)
		t.args.WriteString(ev.Delta)
	case ev.Type == "response.output_item.done":
		if ev.Item != nil && ev.Item.Type == "function_call" {
			// done 帧的 item 带全量标识与参数：以它为准收口
			t := a.toolFor(ev.Item.ID, ev.OutputIndex)
			if t.callID == "" {
				t.callID = ev.Item.CallID
			}
			if t.name == "" {
				t.name = ev.Item.Name
			}
			if ev.Item.Arguments != "" {
				t.args.Reset()
				t.args.WriteString(ev.Item.Arguments)
			}
			a.flushTool(t)
		}
	case ev.Type == "response.completed":
		if a.cb.OnCompletion != nil {
			a.cb.OnCompletion(StreamCompletion{Reason: "completed", Ended: true})
		}
		if ev.Response != nil && ev.Response.Usage != nil {
			u := *ev.Response.Usage
			a.usage = &Usage{
				PromptTokens:        u.InputTokens,
				CompletionTokens:    u.OutputTokens,
				TotalTokens:         u.TotalTokens,
				PromptTokensDetails: &PromptTokensDetails{CachedTokens: u.InputTokensDetails.CachedTokens},
			}
		}
	case ev.Type == "response.failed":
		if a.cb.OnCompletion != nil {
			a.cb.OnCompletion(StreamCompletion{Reason: "failed", Ended: true})
		}
		msg := "响应失败"
		if ev.Response != nil && ev.Response.Error != nil && ev.Response.Error.Message != "" {
			msg = ev.Response.Error.Message
		} else if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		} else if ev.Message != "" {
			msg = ev.Message
		}
		a.err = fmt.Errorf("模型服务返回错误：%s", msg)
		return a.err
	case ev.Type == "response.incomplete":
		reason := "unknown"
		if ev.Response != nil && ev.Response.IncompleteDetails != nil && ev.Response.IncompleteDetails.Reason != "" {
			reason = ev.Response.IncompleteDetails.Reason
		}
		a.err = fmt.Errorf("模型输出不完整（%s），可重试或调大最大输出", reason)
		if a.cb.OnCompletion != nil {
			a.cb.OnCompletion(StreamCompletion{Reason: reason, Ended: true})
		}
		return a.err
	case ev.Type == "error":
		msg := "流中报错"
		if ev.Error != nil && ev.Error.Message != "" {
			msg = ev.Error.Message
		} else if ev.Message != "" {
			msg = ev.Message
		}
		a.err = fmt.Errorf("模型服务流中报错：%s", msg)
		return a.err
	}
	return nil
}

func (a *responsesStreamAgg) flushTool(t *responsesToolAgg) {
	if t.done || a.cb.OnToolCall == nil {
		t.done = true
		return
	}
	t.done = true
	args := t.args.String()
	if args == "" {
		args = "{}"
	}
	a.cb.OnToolCall(ToolCall{ID: t.callID, Type: "function", Function: ToolCallFunc{Name: t.name, Arguments: args}})
}

func (a *responsesStreamAgg) finish() {
	for _, t := range a.order {
		a.flushTool(t)
	}
	if a.usage != nil && a.cb.OnUsage != nil {
		a.cb.OnUsage(*a.usage)
	}
}

// mustJSON 序列化失败只有一种可能（值里有不可编码项，如 chan），这里只用于
// 字符串，不会失败。
func mustJSON(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
