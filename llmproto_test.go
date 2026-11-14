package main

// llmproto_test.go —— 渠道协议扩展的看护测试：Anthropic /v1/messages 与
// OpenAI Responses /v1/responses 的请求转换、流式事件解析、工具调用往返，
// 以及两协议经 StreamChat 全链路的 mock 网关回放；另有一条 chat/completions
// 报文回归（既有渠道不受协议扩展影响）。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ---------- Anthropic 请求转换 ----------

// 消息形态全覆盖：system 提取、多模态图片、assistant 的 tool_use、连续 tool
// 消息合并成一条 user(tool_result)、工具 schema、tool_choice、temperature。
func TestMarshalAnthropicRequestConversion(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			TextMessage("system", "你是数字助理"),
			TextMessage("system", "工具优先"),
			TextMessage("user", "深圳天气"),
			{Role: "user", Content: []ContentPart{
				{Type: "text", Text: "看图"},
				{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/png;base64,QUJD"}},
			}},
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_1", Type: "function",
				Function: ToolCallFunc{Name: "get_weather", Arguments: `{"city":"深圳"}`},
			}}},
			{Role: "tool", Content: "晴 26℃", ToolCallID: "call_1"},
			{Role: "tool", Content: "湿度 60%", ToolCallID: "call_1b"},
		},
		Tools: []ToolDef{{Type: "function", Function: ToolSpec{
			Name: "get_weather", Description: "查天气",
			Parameters: map[string]any{"type": "object", "properties": map[string]any{"city": map[string]any{"type": "string"}}},
		}}},
		ToolChoice:  "auto",
		Temperature: floatPtr(0.3),
	}
	body, err := marshalAnthropic(LLMTarget{Model: "claude-sonnet-4-5", MaxOutput: 64000}, req)
	if err != nil {
		t.Fatalf("marshalAnthropic: %v", err)
	}
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("报文不是合法 JSON: %v\n%s", err, body)
	}
	if raw["system"] != "你是数字助理\n\n工具优先" {
		t.Errorf("system 提取错误: %v", raw["system"])
	}
	if raw["model"] != "claude-sonnet-4-5" || raw["stream"] != true {
		t.Errorf("model/stream 错误: %v / %v", raw["model"], raw["stream"])
	}
	if n, ok := raw["max_tokens"].(float64); !ok || int(n) != 64000 {
		t.Errorf("max_tokens 应取模型目录上限 64000，得到 %v", raw["max_tokens"])
	}
	if raw["temperature"] != 0.3 {
		t.Errorf("temperature 应透传，得到 %v", raw["temperature"])
	}
	if _, has := raw["messages"]; !has {
		t.Fatal("缺 messages")
	}
	msgs := raw["messages"].([]any)
	// user(文本) → user(多模态) → assistant(tool_use) → user(两条 tool_result 合并)
	if len(msgs) != 4 {
		t.Fatalf("messages 应为 4 条（连续 tool 合并），得到 %d: %s", len(msgs), body)
	}
	imgMsg := msgs[1].(map[string]any)
	blocks := imgMsg["content"].([]any)
	imgBlock := blocks[1].(map[string]any)
	if imgBlock["type"] != "image" {
		t.Fatalf("第二个 block 应为 image: %s", body)
	}
	src := imgBlock["source"].(map[string]any)
	if src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "QUJD" {
		t.Errorf("图片 base64 source 转换错误: %v", src)
	}
	am := msgs[2].(map[string]any)
	if am["role"] != "assistant" {
		t.Fatalf("第 3 条应为 assistant: %s", body)
	}
	toolUse := am["content"].([]any)[0].(map[string]any)
	if toolUse["type"] != "tool_use" || toolUse["id"] != "call_1" || toolUse["name"] != "get_weather" {
		t.Errorf("tool_use block 错误: %v", toolUse)
	}
	if s, _ := json.Marshal(toolUse["input"]); string(s) != `{"city":"深圳"}` {
		t.Errorf("tool_use.input 应为 JSON 对象，得到 %v", toolUse["input"])
	}
	tr := msgs[3].(map[string]any)
	if tr["role"] != "user" {
		t.Fatalf("tool 消息应转为 user: %s", body)
	}
	results := tr["content"].([]any)
	if len(results) != 2 {
		t.Fatalf("连续两条 tool 消息应合并为一个 user 的两个 tool_result: %s", body)
	}
	r0 := results[0].(map[string]any)
	if r0["type"] != "tool_result" || r0["tool_use_id"] != "call_1" || r0["content"] != "晴 26℃" {
		t.Errorf("tool_result block 错误: %v", r0)
	}
	tools := raw["tools"].([]any)
	tl := tools[0].(map[string]any)
	if tl["name"] != "get_weather" || tl["input_schema"] == nil {
		t.Errorf("工具 schema 应为 name+input_schema 平铺: %v", tl)
	}
	if _, has := tl["function"]; has {
		t.Errorf("Anthropic 工具不该嵌 function 层: %s", body)
	}
	tc := raw["tool_choice"].(map[string]any)
	if tc["type"] != "auto" {
		t.Errorf("tool_choice auto 映射错误: %v", tc)
	}
}

// 思考档位 → Anthropic 官方现行口径：none → thinking{type:disabled}，其余五档
// → thinking{type:adaptive} + output_config.effort 原样发；不再有 budget_tokens
// 映射，也不再为思考抬升 max_tokens。
func TestMarshalAnthropicThinking(t *testing.T) {
	cases := []struct {
		level      string
		wantThink  string // "" = 不带 thinking
		wantEffort string // "" = 不带 output_config
		maxTok     int    // 期望 max_tokens（未声明 MaxOutput 时默认 32768）
		maxOut     int    // LLMTarget.MaxOutput（0 = 未声明）
	}{
		{"", "", "", 32768, 0},
		{"enabled", "", "", 32768, 0}, // 老值：跟随服务端默认
		{"none", "disabled", "", 32768, 0},
		{"disabled", "disabled", "", 32768, 0}, // 老值兼容
		{"low", "adaptive", "low", 32768, 0},
		{"medium", "adaptive", "medium", 32768, 0},
		{"high", "adaptive", "high", 32768, 0},
		{"xhigh", "adaptive", "xhigh", 32768, 0},
		{"max", "adaptive", "max", 32768, 32768}, // 档位再大也不抬 max_tokens
		{"max", "adaptive", "max", 32768, 0},     // 未声明最大输出时仍保留 max 档映射
		{"max", "adaptive", "max", 64000, 64000}, // 已声明最大输出优先于兜底
	}
	for _, c := range cases {
		req := ChatRequest{Messages: []Message{TextMessage("user", "hi")}}
		req.ApplyThinking(c.level)
		body, err := marshalAnthropic(LLMTarget{Model: "claude-sonnet-4-5", MaxOutput: c.maxOut}, req)
		if err != nil {
			t.Fatalf("档位 %q: %v", c.level, err)
		}
		var raw struct {
			MaxTokens int `json:"max_tokens"`
			Thinking  *struct {
				Type         string `json:"type"`
				BudgetTokens int    `json:"budget_tokens"`
			} `json:"thinking"`
			OutputConfig *struct {
				Effort string `json:"effort"`
			} `json:"output_config"`
			Temperature *float64 `json:"temperature"`
		}
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("档位 %q 报文非法: %v", c.level, err)
		}
		if raw.MaxTokens != c.maxTok {
			t.Errorf("档位 %q: max_tokens=%d，期望 %d", c.level, raw.MaxTokens, c.maxTok)
		}
		if c.wantThink == "" {
			if raw.Thinking != nil {
				t.Errorf("档位 %q 不该带 thinking: %s", c.level, body)
			}
		} else if raw.Thinking == nil || raw.Thinking.Type != c.wantThink {
			t.Errorf("档位 %q thinking.type 错误: %v（报文 %s）", c.level, raw.Thinking, body)
		}
		if raw.Thinking != nil && raw.Thinking.BudgetTokens != 0 {
			t.Errorf("档位 %q 不该再发 budget_tokens: %s", c.level, body)
		}
		if c.wantEffort == "" {
			if raw.OutputConfig != nil {
				t.Errorf("档位 %q 不该带 output_config: %s", c.level, body)
			}
		} else if raw.OutputConfig == nil || raw.OutputConfig.Effort != c.wantEffort {
			t.Errorf("档位 %q output_config.effort 错误: %v（报文 %s）", c.level, raw.OutputConfig, body)
		}
		// adaptive（思考开着）时 temperature 会 400，必须去掉；disabled/不干预保留
		if c.wantThink == "adaptive" && raw.Temperature != nil {
			t.Errorf("档位 %q 思考开着时不应携带 temperature: %s", c.level, body)
		}
	}
}

// 显式请求与模型目录的最大输出优先于兜底，不按思考档位静默改小。
func TestAnthropicMaxTokensPriority(t *testing.T) {
	for _, c := range []struct {
		name      string
		requested int
		maxOutput int
		want      int
	}{
		{"fallback", 0, 0, 32768},
		{"catalog", 0, 64000, 64000},
		{"explicit", 4096, 64000, 4096},
	} {
		t.Run(c.name, func(t *testing.T) {
			req := ChatRequest{Messages: []Message{TextMessage("user", "hi")}, MaxTokens: c.requested}
			req.ApplyThinking("max")
			body, err := marshalAnthropic(LLMTarget{Model: "claude-sonnet-4-5", MaxOutput: c.maxOutput}, req)
			if err != nil {
				t.Fatal(err)
			}
			var raw anthropicRequest
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			if raw.MaxTokens != c.want {
				t.Errorf("max_tokens=%d，期望 %d", raw.MaxTokens, c.want)
			}
			if raw.Thinking == nil || raw.Thinking.Type != "adaptive" || raw.OutputConfig == nil || raw.OutputConfig.Effort != "max" {
				t.Errorf("max 档映射被改变: %s", body)
			}
		})
	}
}

// tool_choice 变体：required→any、具名→tool、none→none、空串省略。
func TestAnthropicToolChoiceMapping(t *testing.T) {
	for choice, want := range map[string]string{"required": "any", "none": "none"} {
		req := ChatRequest{ToolChoice: choice, Messages: []Message{TextMessage("user", "hi")}}
		body, _ := marshalAnthropic(LLMTarget{Model: "m"}, req)
		var raw struct {
			ToolChoice *struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"tool_choice"`
		}
		_ = json.Unmarshal(body, &raw)
		if raw.ToolChoice == nil || raw.ToolChoice.Type != want {
			t.Errorf("tool_choice %q → %v", choice, raw.ToolChoice)
		}
	}
	req := ChatRequest{ToolChoice: "get_weather", Messages: []Message{TextMessage("user", "hi")}}
	body, _ := marshalAnthropic(LLMTarget{Model: "m"}, req)
	var raw struct {
		ToolChoice *struct {
			Type string `json:"type"`
			Name string `json:"name"`
		} `json:"tool_choice"`
	}
	_ = json.Unmarshal(body, &raw)
	if raw.ToolChoice == nil || raw.ToolChoice.Type != "tool" || raw.ToolChoice.Name != "get_weather" {
		t.Errorf("具名 tool_choice 映射错误: %v", raw.ToolChoice)
	}
	req = ChatRequest{Messages: []Message{TextMessage("user", "hi")}}
	body, _ = marshalAnthropic(LLMTarget{Model: "m"}, req)
	if strings.Contains(string(body), "tool_choice") {
		t.Errorf("空串 tool_choice 不该出现在报文: %s", body)
	}
}

// 工具调用往返：解析器吐出的 ToolCall → 回填 Message{role:tool} →
// tool_result block 的 tool_use_id 对得上原调用 id。
func TestAnthropicToolResultRoundtrip(t *testing.T) {
	tc := ToolCall{ID: "toolu_9f", Type: "function", Function: ToolCallFunc{Name: "read", Arguments: `{"path":"a.go"}`}}
	m := Message{Role: "tool", Content: "文件内容", ToolCallID: tc.ID}
	_, msgs, err := toAnthropicMessages([]Message{
		TextMessage("user", "读 a.go"),
		{Role: "assistant", ToolCalls: []ToolCall{tc}},
		m,
	})
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, am := range msgs {
		blocks, _ := am.Content.([]anthropicBlock)
		for _, b := range blocks {
			if b.Type == "tool_use" && b.ID != tc.ID {
				t.Fatalf("tool_use id 断链: %s", b.ID)
			}
			if b.Type == "tool_result" && b.ToolUseID == tc.ID && b.Content == "文件内容" {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("没有找到与 tool_use id 对应的 tool_result block")
	}
}

// ---------- Anthropic 流式事件解析 ----------

// 完整事件体系回放：text/thinking 增量、tool_use 参数聚合、usage 含
// cache_read 与 cache_creation。
func TestParseAnthropicSSE(t *testing.T) {
	const sse = "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"cache_creation_input_tokens":10,"cache_read_input_tokens":100,"output_tokens":1}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先想"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":0}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":1,"content_block":{"type":"text"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"深圳"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"晴"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":1}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_01A","name":"get_weather"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"city\":"}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"\"深圳\"}"}}` + "\n\n" +
		"event: content_block_stop\n" +
		`data: {"type":"content_block_stop","index":2}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	var text, reasoning strings.Builder
	var calls []ToolCall
	var usage *Usage
	err := parseAnthropicSSE(strings.NewReader(sse), StreamCallbacks{
		OnText:      func(s string) { text.WriteString(s) },
		OnReasoning: func(s string) { reasoning.WriteString(s) },
		OnToolCall:  func(tc ToolCall) { calls = append(calls, tc) },
		OnUsage:     func(u Usage) { uu := u; usage = &uu },
	})
	if err != nil {
		t.Fatalf("parseAnthropicSSE: %v", err)
	}
	if text.String() != "深圳晴" || reasoning.String() != "先想" {
		t.Errorf("text=%q reasoning=%q", text.String(), reasoning.String())
	}
	if len(calls) != 1 || calls[0].ID != "toolu_01A" || calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"深圳"}` {
		t.Errorf("工具调用聚合错误: %+v", calls)
	}
	if usage == nil {
		t.Fatal("没有收到 usage")
	}
	if usage.PromptTokens != 25 || usage.CompletionTokens != 42 || usage.TotalTokens != 67 {
		t.Errorf("usage 主体错误: %+v", usage)
	}
	if usage.CacheReadTokens != 100 || usage.CacheHit() != 100 || usage.CacheCreationTokens != 10 || !usage.ReportsCache() {
		t.Errorf("缓存字段归一错误: %+v", usage)
	}
}

// 流中 error 事件要中断并报错（余额/鉴权类失败以这种形态出现）。
func TestParseAnthropicSSEErrorEvent(t *testing.T) {
	const sse = "event: error\n" +
		`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"
	err := parseAnthropicSSE(strings.NewReader(sse), StreamCallbacks{})
	if err == nil || !strings.Contains(err.Error(), "Overloaded") {
		t.Fatalf("error 事件应中断: %v", err)
	}
}

// ---------- Responses 请求转换 ----------

func TestMarshalResponsesRequestConversion(t *testing.T) {
	req := ChatRequest{
		Messages: []Message{
			TextMessage("system", "你是数字助理"),
			TextMessage("user", "深圳天气"),
			{Role: "user", Content: []ContentPart{
				{Type: "text", Text: "看图"},
				{Type: "image_url", ImageURL: &ImageURL{URL: "data:image/jpeg;base64,REVG"}},
			}},
			TextMessage("assistant", "我查一下"),
			{Role: "assistant", ToolCalls: []ToolCall{{
				ID: "call_1", Type: "function",
				Function: ToolCallFunc{Name: "get_weather", Arguments: `{"city":"深圳"}`},
			}}},
			{Role: "tool", Content: "晴 26℃", ToolCallID: "call_1"},
		},
		Tools: []ToolDef{{Type: "function", Function: ToolSpec{
			Name: "get_weather", Description: "查天气",
			Parameters: map[string]any{"type": "object"},
		}}},
		ToolChoice: "required",
	}
	req.ApplyThinking("high")
	body, err := marshalResponses(LLMTarget{Model: "gpt-5.2", MaxOutput: 16384}, req)
	if err != nil {
		t.Fatalf("marshalResponses: %v", err)
	}
	var raw struct {
		Model           string            `json:"model"`
		Instructions    string            `json:"instructions"`
		Input           []json.RawMessage `json:"input"`
		Tools           []struct {
			Type       string         `json:"type"`
			Name       string         `json:"name"`
			Parameters map[string]any `json:"parameters"`
			Function   *struct{}      `json:"function"`
		} `json:"tools"`
		ToolChoice      string `json:"tool_choice"`
		Reasoning       *struct {
			Effort string `json:"effort"`
		} `json:"reasoning"`
		MaxOutputTokens int      `json:"max_output_tokens"`
		Stream          bool     `json:"stream"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("报文非法: %v\n%s", err, body)
	}
	if raw.Model != "gpt-5.2" || !raw.Stream {
		t.Errorf("model/stream 错误: %v %v", raw.Model, raw.Stream)
	}
	if raw.Instructions != "你是数字助理" {
		t.Errorf("system 应合并为 instructions: %q", raw.Instructions)
	}
	if len(raw.Input) != 5 {
		t.Fatalf("input 应为 5 项（文本/多模态/assistant文本/function_call/function_call_output），得到 %d: %s", len(raw.Input), body)
	}
	var imgItem map[string]any
	_ = json.Unmarshal(raw.Input[1], &imgItem)
	parts := imgItem["content"].([]any)
	img := parts[1].(map[string]any)
	if img["type"] != "input_image" || img["image_url"] != "data:image/jpeg;base64,REVG" {
		t.Errorf("input_image 转换错误: %v", img)
	}
	var fc map[string]any
	_ = json.Unmarshal(raw.Input[3], &fc)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" || fc["arguments"] != `{"city":"深圳"}` {
		t.Errorf("function_call item 错误: %v", fc)
	}
	var fo map[string]any
	_ = json.Unmarshal(raw.Input[4], &fo)
	if fo["type"] != "function_call_output" || fo["call_id"] != "call_1" || fo["output"] != "晴 26℃" {
		t.Errorf("function_call_output item 错误: %v", fo)
	}
	if len(raw.Tools) != 1 || raw.Tools[0].Type != "function" || raw.Tools[0].Name != "get_weather" || raw.Tools[0].Function != nil {
		t.Errorf("Responses 工具应平铺不嵌 function: %+v", raw.Tools)
	}
	if raw.ToolChoice != "required" {
		t.Errorf("tool_choice required 映射错误: %v", raw.ToolChoice)
	}
	if raw.Reasoning == nil || raw.Reasoning.Effort != "high" {
		t.Errorf("reasoning effort 映射错误: %+v", raw.Reasoning)
	}
	if raw.MaxOutputTokens != 16384 {
		t.Errorf("max_output_tokens 应取模型上限: %d", raw.MaxOutputTokens)
	}
}

// 思考档位 → reasoning.effort：六档原样发（none…max），空串省略；
// 老值 disabled → none、enabled → 省略。
func TestResponsesEffortMapping(t *testing.T) {
	cases := map[string]string{
		"": "", "enabled": "",
		"none": "none", "disabled": "none",
		"low": "low", "medium": "medium", "high": "high", "xhigh": "xhigh", "max": "max",
	}
	for level, want := range cases {
		req := ChatRequest{Messages: []Message{TextMessage("user", "hi")}}
		req.ApplyThinking(level)
		body, err := marshalResponses(LLMTarget{Model: "gpt-5.2"}, req)
		if err != nil {
			t.Fatalf("档位 %q: %v", level, err)
		}
		var raw struct {
			Reasoning *struct {
				Effort string `json:"effort"`
			} `json:"reasoning"`
		}
		_ = json.Unmarshal(body, &raw)
		if want == "" {
			if raw.Reasoning != nil {
				t.Errorf("档位 %q 不该带 reasoning: %s", level, body)
			}
		} else if raw.Reasoning == nil || raw.Reasoning.Effort != want {
			t.Errorf("档位 %q → %v（期望 %s）", level, raw.Reasoning, want)
		}
	}
}

// ---------- Responses 流式事件解析 ----------

func TestParseResponsesSSE(t *testing.T) {
	const sse = "event: response.created\n" +
		`data: {"type":"response.created","response":{"id":"resp_1"}}` + "\n\n" +
		"event: response.reasoning_summary_text.delta\n" +
		`data: {"type":"response.reasoning_summary_text.delta","item_id":"rs_1","delta":"思考中"}` + "\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"深圳"` + "}\n\n" +
		"event: response.output_text.delta\n" +
		`data: {"type":"response.output_text.delta","item_id":"msg_1","output_index":0,"delta":"晴"}` + "\n\n" +
		"event: response.output_item.added\n" +
		`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":""}}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"{\"city\":"}` + "\n\n" +
		"event: response.function_call_arguments.delta\n" +
		`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","output_index":1,"delta":"\"深圳\"}"}` + "\n\n" +
		"event: response.output_item.done\n" +
		`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"深圳\"}"}}` + "\n\n" +
		"event: response.completed\n" +
		`data: {"type":"response.completed","response":{"id":"resp_1","usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":64},"output_tokens":20,"total_tokens":120}}}` + "\n\n"
	var text, reasoning strings.Builder
	var calls []ToolCall
	var usage *Usage
	err := parseResponsesSSE(strings.NewReader(sse), StreamCallbacks{
		OnText:      func(s string) { text.WriteString(s) },
		OnReasoning: func(s string) { reasoning.WriteString(s) },
		OnToolCall:  func(tc ToolCall) { calls = append(calls, tc) },
		OnUsage:     func(u Usage) { uu := u; usage = &uu },
	})
	if err != nil {
		t.Fatalf("parseResponsesSSE: %v", err)
	}
	if text.String() != "深圳晴" || reasoning.String() != "思考中" {
		t.Errorf("text=%q reasoning=%q", text.String(), reasoning.String())
	}
	if len(calls) != 1 || calls[0].ID != "call_1" || calls[0].Function.Name != "get_weather" || calls[0].Function.Arguments != `{"city":"深圳"}` {
		t.Errorf("function_call 聚合错误: %+v", calls)
	}
	if usage == nil {
		t.Fatal("没有收到 usage")
	}
	if usage.PromptTokens != 100 || usage.CompletionTokens != 20 || usage.TotalTokens != 120 {
		t.Errorf("usage 主体错误: %+v", usage)
	}
	if usage.CacheHit() != 64 || usage.CacheMiss() != 36 {
		t.Errorf("缓存归一错误: hit=%d miss=%d", usage.CacheHit(), usage.CacheMiss())
	}
}

// response.failed / response.incomplete / 裸 error 事件都要按错误收流。
func TestParseResponsesSSEFailures(t *testing.T) {
	cases := map[string]string{
		"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"quota exceeded\"}}}\n\n":          "quota exceeded",
		"event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n": "max_output_tokens",
		"event: error\ndata: {\"type\":\"error\",\"message\":\"stream disconnected\"}\n\n":                                                 "stream disconnected",
	}
	for sse, want := range cases {
		if err := parseResponsesSSE(strings.NewReader(sse), StreamCallbacks{}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("事件 %s 应报错含 %q，得到 %v", sse, want, err)
		}
	}
}

// ---------- mock 网关端到端（StreamChat 全链路） ----------

// Anthropic 协议回放：网关校验端点/鉴权头/请求体，回一条含思考、正文、工具
// 调用与缓存统计的完整事件流；StreamChat 侧断言回调与 OnRequest 捕获。
func TestStreamChatAnthropicEndToEnd(t *testing.T) {
	var gotPath, gotKey, gotVersion string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		if auth := r.Header.Get("Authorization"); auth != "" {
			t.Errorf("Anthropic 渠道不该发 Bearer 头: %s", auth)
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		const sse = "event: message_start\n" +
			`data: {"type":"message_start","message":{"usage":{"input_tokens":25,"cache_creation_input_tokens":8,"cache_read_input_tokens":100,"output_tokens":1}}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"先查"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"这就查。"}}` + "\n\n" +
			"event: content_block_start\n" +
			`data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_E2E","name":"get_weather"}}` + "\n\n" +
			"event: content_block_delta\n" +
			`data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"深圳\"}"}}` + "\n\n" +
			"event: content_block_stop\n" +
			`data: {"type":"content_block_stop","index":1}` + "\n\n" +
			"event: message_delta\n" +
			`data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":42}}` + "\n\n" +
			"event: message_stop\n" +
			`data: {"type":"message_stop"}` + "\n\n"
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()
	var text strings.Builder
	var calls []ToolCall
	var usage *Usage
	var reqURL string
	var reqBody []byte
	err := StreamChat(t.Context(), LLMTarget{
		BaseURL: srv.URL + "/v1", APIKey: "sk-ant-test", Model: "claude-sonnet-4-5", Protocol: "anthropic",
	}, ChatRequest{
		Messages: []Message{TextMessage("system", "你是助理"), TextMessage("user", "深圳天气")},
		Tools:    []ToolDef{{Type: "function", Function: ToolSpec{Name: "get_weather", Parameters: map[string]any{"type": "object"}}}},
	}, StreamCallbacks{
		OnText:     func(s string) { text.WriteString(s) },
		OnToolCall: func(tc ToolCall) { calls = append(calls, tc) },
		OnUsage:    func(u Usage) { uu := u; usage = &uu },
		OnRequest:  func(u string, b []byte) { reqURL, reqBody = u, b },
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if gotPath != "/v1/messages" {
		t.Errorf("端点应为 /v1/messages，得到 %s", gotPath)
	}
	if gotKey != "sk-ant-test" || gotVersion != anthropicVersionHeader {
		t.Errorf("鉴权头错误: x-api-key=%q anthropic-version=%q", gotKey, gotVersion)
	}
	if gotBody["system"] != "你是助理" || gotBody["model"] != "claude-sonnet-4-5" {
		t.Errorf("请求体转换错误: %s", mustJSON(gotBody))
	}
	if !strings.HasSuffix(reqURL, "/v1/messages") || !strings.Contains(string(reqBody), `"max_tokens"`) {
		t.Errorf("OnRequest 捕获异常: %s %s", reqURL, reqBody)
	}
	if text.String() != "这就查。" || len(calls) != 1 || calls[0].Function.Name != "get_weather" {
		t.Errorf("回调异常: text=%q calls=%+v", text.String(), calls)
	}
	if usage == nil || usage.CacheHit() != 100 || usage.CacheCreationTokens != 8 || usage.PromptTokens != 25 {
		t.Errorf("usage 异常: %+v", usage)
	}
}

// Responses 协议回放：网关校验端点与 Bearer 头，回 output_text.delta /
// function_call 增量 / response.completed(usage) 事件。
func TestStreamChatResponsesEndToEnd(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "text/event-stream")
		const sse = "event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","item_id":"msg_1","delta":"好的"}` + "\n\n" +
			"event: response.output_item.added\n" +
			`data: {"type":"response.output_item.added","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_r1","name":"run_command"}}` + "\n\n" +
			"event: response.function_call_arguments.delta\n" +
			`data: {"type":"response.function_call_arguments.delta","item_id":"fc_1","delta":"{\"command\":\"echo ok\"}"}` + "\n\n" +
			"event: response.output_item.done\n" +
			`data: {"type":"response.output_item.done","output_index":1,"item":{"type":"function_call","id":"fc_1","call_id":"call_r1","name":"run_command","arguments":"{\"command\":\"echo ok\"}"}}` + "\n\n" +
			"event: response.completed\n" +
			`data: {"type":"response.completed","response":{"usage":{"input_tokens":50,"input_tokens_details":{"cached_tokens":32},"output_tokens":10,"total_tokens":60}}}` + "\n\n"
		_, _ = w.Write([]byte(sse))
	}))
	defer srv.Close()
	var text strings.Builder
	var calls []ToolCall
	var usage *Usage
	err := StreamChat(t.Context(), LLMTarget{
		BaseURL: srv.URL + "/v1", APIKey: "sk-test", Model: "gpt-5.2", Protocol: "responses",
	}, ChatRequest{
		Messages: []Message{TextMessage("system", "你是助理"), TextMessage("user", "跑个命令")},
	}, StreamCallbacks{
		OnText:     func(s string) { text.WriteString(s) },
		OnToolCall: func(tc ToolCall) { calls = append(calls, tc) },
		OnUsage:    func(u Usage) { uu := u; usage = &uu },
	})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if gotPath != "/v1/responses" {
		t.Errorf("端点应为 /v1/responses，得到 %s", gotPath)
	}
	if gotAuth != "Bearer sk-test" {
		t.Errorf("Responses 鉴权应为 Bearer: %q", gotAuth)
	}
	if gotBody["instructions"] != "你是助理" || gotBody["model"] != "gpt-5.2" {
		t.Errorf("请求体转换错误: %s", mustJSON(gotBody))
	}
	if text.String() != "好的" {
		t.Errorf("正文异常: %q", text.String())
	}
	if len(calls) != 1 || calls[0].ID != "call_r1" || calls[0].Function.Arguments != `{"command":"echo ok"}` {
		t.Errorf("工具调用异常: %+v", calls)
	}
	if usage == nil || usage.CacheHit() != 32 {
		t.Errorf("usage 异常: %+v", usage)
	}
}

// ---------- 既有渠道回归：chat/completions 报文与重构前一致 ----------

// 协议扩展不得改变 chat/completions 的任何线上行为：端点、Bearer 头、
// stream_options.include_usage、fillReasoningStub 都保持原样，协议新参数
//（max_tokens/temperature/tool_choice）未设置时不出现。
func TestStreamChatOpenAIRegression(t *testing.T) {
	var gotPath, gotAuth string
	var raw map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	}))
	defer srv.Close()
	var text strings.Builder
	err := StreamChat(t.Context(), LLMTarget{BaseURL: srv.URL, APIKey: "sk-k", Model: "deepseek-flash"},
		ChatRequest{Messages: []Message{TextMessage("user", "hi")}},
		StreamCallbacks{OnText: func(s string) { text.WriteString(s) }})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if text.String() != "hi" {
		t.Fatalf("正文异常: %q", text.String())
	}
	if gotPath != "/chat/completions" || gotAuth != "Bearer sk-k" {
		t.Errorf("端点/鉴权头被改动: %s %q", gotPath, gotAuth)
	}
	if raw["model"] != "deepseek-flash" || raw["stream"] != true {
		t.Errorf("model/stream 被改动: %s", mustJSON(raw))
	}
	so, _ := raw["stream_options"].(map[string]any)
	if so == nil || so["include_usage"] != true {
		t.Errorf("stream_options.include_usage 丢失: %s", mustJSON(raw))
	}
	for _, k := range []string{"max_tokens", "temperature", "tool_choice"} {
		if _, has := raw[k]; has {
			t.Errorf("未设置的 %s 不该出现在报文: %s", k, mustJSON(raw))
		}
	}
}

// 带 tools 的请求仍走 fillReasoningStub（DeepSeek/GLM 的 400 防线）。
func TestStreamChatOpenAIRegressionReasoningStub(t *testing.T) {
	var raw struct {
		Messages []map[string]any `json:"messages"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&raw)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()
	err := StreamChat(t.Context(), LLMTarget{BaseURL: srv.URL, Model: "deepseek-flash"},
		ChatRequest{
			Messages: []Message{
				TextMessage("user", "读文件"),
				{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "read"}}}},
			},
			Tools: []ToolDef{{Type: "function", Function: ToolSpec{Name: "read"}}},
		}, StreamCallbacks{})
	if err != nil {
		t.Fatalf("StreamChat: %v", err)
	}
	if v, has := raw.Messages[1]["reasoning_content"]; !has || v != "" {
		t.Errorf("fillReasoningStub 行为被改动: %s", mustJSON(raw.Messages[1]))
	}
}

// ---------- usage 归一补充：cache_creation 字段 ----------

// Anthropic 原生协议只报 cache_creation（没有读取命中）时也要算「报了缓存统计」。
func TestUsageCacheCreationReportsCache(t *testing.T) {
	var u Usage
	if err := json.Unmarshal([]byte(`{"prompt_tokens":10,"cache_creation_input_tokens":4}`), &u); err != nil {
		t.Fatal(err)
	}
	if !u.ReportsCache() {
		t.Error("只报 cache_creation 时 ReportsCache 应为 true")
	}
	if u.CacheHit() != 0 {
		t.Errorf("写入量不该计入命中: %d", u.CacheHit())
	}
	if u.CacheMiss() != 10 {
		t.Errorf("未命中应= prompt-0: %d", u.CacheMiss())
	}
}

func floatPtr(f float64) *float64 { return &f }
