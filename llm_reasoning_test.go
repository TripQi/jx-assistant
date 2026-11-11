package main

import (
	"encoding/json"
	"testing"
)

// 带 tools 的请求里，历史中「当轮没产生思考」的 assistant 消息必须带上
// reasoning_content（空串合法），否则 DeepSeek/智谱 GLM 直接 400；已有的思考
// 必须逐字保留（智谱要求与原始生成序列完全一致）。
func TestFillReasoningStubFillsAssistantWithoutThinking(t *testing.T) {
	kept := "这一步我要先看文件结构"
	req := ChatRequest{
		Tools: []ToolDef{{Type: "function", Function: ToolSpec{Name: "read"}}},
		Messages: []Message{
			TextMessage("user", "读一下 a.go"),
			{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "read"}}}},
			{Role: "tool", Content: "ok", ToolCallID: "c1"},
			{Role: "assistant", ReasoningContent: &kept, ToolCalls: []ToolCall{{ID: "c2", Type: "function", Function: ToolCallFunc{Name: "read"}}}},
			{Role: "tool", Content: "ok", ToolCallID: "c2"},
		},
	}
	fillReasoningStub(&req)

	if req.Messages[1].ReasoningContent == nil || *req.Messages[1].ReasoningContent != "" {
		t.Fatalf("缺思考的 assistant 应补成空串，得到 %v", req.Messages[1].ReasoningContent)
	}
	if req.Messages[3].ReasoningContent == nil || *req.Messages[3].ReasoningContent != kept {
		t.Fatalf("已有的思考被改动了：%v", req.Messages[3].ReasoningContent)
	}
	for _, i := range []int{0, 2, 4} {
		if req.Messages[i].ReasoningContent != nil {
			t.Fatalf("非 assistant 消息不该带 reasoning_content（idx=%d）", i)
		}
	}

	// 线上报文口径：assistant 一律出现 reasoning_content 键，其余角色不出现
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	for i, m := range wire.Messages {
		v, has := m["reasoning_content"]
		if m["role"] == "assistant" {
			if !has {
				t.Fatalf("assistant[%d] 报文里缺 reasoning_content 键：%s", i, raw)
			}
			if i == 3 && v != kept {
				t.Fatalf("assistant[3] 思考被改写：%v", v)
			}
		} else if has {
			t.Fatalf("%v 消息不该带 reasoning_content：%s", m["role"], raw)
		}
	}
}

// 不带 tools 的请求不补：无工具时两家都不校验，别给其它兼容端点添噪音。
func TestFillReasoningStubSkippedWithoutTools(t *testing.T) {
	req := ChatRequest{Messages: []Message{
		TextMessage("user", "你好"),
		{Role: "assistant", Content: "你好"},
	}}
	fillReasoningStub(&req)
	if req.Messages[1].ReasoningContent != nil {
		t.Fatalf("无 tools 时不该补 reasoning_content")
	}
	raw, _ := json.Marshal(req)
	var wire struct {
		Messages []map[string]any `json:"messages"`
	}
	_ = json.Unmarshal(raw, &wire)
	if _, has := wire.Messages[1]["reasoning_content"]; has {
		t.Fatalf("无 tools 时报文里不该出现 reasoning_content：%s", raw)
	}
}

// 重复调用幂等；已经是空串指针的消息保持空串，不会被改成 nil 或填入内容。
func TestFillReasoningStubIdempotent(t *testing.T) {
	empty := ""
	req := ChatRequest{
		Tools: []ToolDef{{Type: "function", Function: ToolSpec{Name: "read"}}},
		Messages: []Message{
			{Role: "assistant", ReasoningContent: &empty},
		},
	}
	fillReasoningStub(&req)
	fillReasoningStub(&req)
	if req.Messages[0].ReasoningContent == nil || *req.Messages[0].ReasoningContent != "" {
		t.Fatalf("空串指针应原样保留：%v", req.Messages[0].ReasoningContent)
	}
}

// 思考档位的线上报文口径：六档一律走 reasoning_effort 原样透传（none…max），
// 空串不干预；老值 disabled 归一到 none、enabled 归一到不干预，认不出的串同样
// 不干预（报错让用户自己换档，不替用户猜）。
func TestApplyThinking(t *testing.T) {
	cases := []struct {
		level    string
		wantJSON string // 附加在基础报文后的键（不含外层花括号）
	}{
		{"", ``},
		{"enabled", ``},
		{"disabled", `"reasoning_effort":"none"`},
		{"none", `"reasoning_effort":"none"`},
		{"low", `"reasoning_effort":"low"`},
		{"medium", `"reasoning_effort":"medium"`},
		{"high", `"reasoning_effort":"high"`},
		{"xhigh", `"reasoning_effort":"xhigh"`},
		{"max", `"reasoning_effort":"max"`},
		{"minimal", ``}, // 六档之外：不干预
	}
	for _, c := range cases {
		req := ChatRequest{Model: "m", Messages: []Message{}, Stream: true}
		req.ApplyThinking(c.level)
		raw, err := json.Marshal(req)
		if err != nil {
			t.Fatal(err)
		}
		want := `{"model":"m","messages":[],"stream":true`
		if c.wantJSON != "" {
			want += "," + c.wantJSON
		}
		want += `}`
		if string(raw) != want {
			t.Errorf("档位 %q 报文为 %s，期望 %s", c.level, raw, want)
		}
	}
}
