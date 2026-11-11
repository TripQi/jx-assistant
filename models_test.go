package main

import (
	"reflect"
	"testing"
)

// 思考档位推断的协议维度：同一模型 ID 在三种渠道协议下档位不同
//（Anthropic 预算档 / Responses effort 档 / 兼容端点既有口径）。
func TestGuessReasoningLevelsByProtocol(t *testing.T) {
	cases := []struct {
		name, proto, id string
		want            []string
	}{
		{
			name: "anthropic 渠道 claude 全档含 max", proto: "anthropic", id: "claude-sonnet-4-5",
			want: []string{"enabled", "disabled", "low", "medium", "high", "max"},
		},
		{
			name: "anthropic 渠道非 claude 无档位", proto: "anthropic", id: "glm-5.3",
			want: nil,
		},
		{
			name: "responses 渠道 gpt-5", proto: "responses", id: "gpt-5.2",
			want: []string{"disabled", "low", "medium", "high"},
		},
		{
			name: "responses 渠道 o3", proto: "responses", id: "o3",
			want: []string{"disabled", "low", "medium", "high"},
		},
		{
			name: "responses 渠道 o4-mini", proto: "responses", id: "o4-mini",
			want: []string{"disabled", "low", "medium", "high"},
		},
		{
			name: "responses 渠道非推理模型无档位", proto: "responses", id: "gpt-4o",
			want: nil,
		},
		{
			// OpenAI 官方 chat/completions 对 gpt-5/o 系支持 reasoning_effort
			//（low/medium/high；档位口径既有行为不变）
			name: "兼容渠道 gpt-5", proto: "", id: "gpt-5",
			want: []string{"low", "medium", "high"},
		},
		{
			// 兼容网关上的 claude（new-api/OpenRouter 等）：维持既有档位，
			// 不带 anthropic 协议特有的 max（reasoning_effort 透传 max 多数
			// 网关不认识，选了不生效不如不列）
			name: "兼容渠道 claude 维持旧档", proto: "", id: "claude-opus-4-1",
			want: []string{"enabled", "disabled", "low", "medium", "high"},
		},
		{
			name: "兼容渠道 deepseek 照旧", proto: "", id: "deepseek-flash",
			want: []string{"enabled", "disabled", "low", "high", "max"},
		},
	}
	for _, c := range cases {
		got := guessReasoningLevels(c.proto, c.id)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s：guessReasoningLevels(%q, %q) = %v，期望 %v", c.name, c.proto, c.id, got, c.want)
		}
	}
}

// 思考档位的最终口径：存储值是当前推断的子集 → 视为旧版自动推断写死的，
// 跟随新版本刷新；拉取到的真实 supported_levels（含推断之外的档位）原样保留。
func TestEffectiveReasoningLevels(t *testing.T) {
	cases := []struct {
		name, proto, id string
		stored          []string
		want            []string
	}{
		{
			// 存量 DeepSeek 配置：旧版 guess 写死的开/关两档，本版本补了强度档
			name: "deepseek 旧推断自动升级", proto: "", id: "deepseek-flash",
			stored: []string{"enabled", "disabled"},
			want:   []string{"enabled", "disabled", "low", "high", "max"},
		},
		{
			// 存量 Anthropic 渠道：旧版写死的五档（无 max），本版本补 max
			name: "anthropic 存量补 max", proto: "anthropic", id: "claude-sonnet-4-5",
			stored: []string{"enabled", "disabled", "low", "medium", "high"},
			want:   []string{"enabled", "disabled", "low", "medium", "high", "max"},
		},
		{
			// 存量 Responses 渠道：兼容协议时期写死的三档，本版本补 disabled
			name: "responses 存量补 disabled", proto: "responses", id: "gpt-5",
			stored: []string{"low", "medium", "high"},
			want:   []string{"disabled", "low", "medium", "high"},
		},
		{
			// 协议切换后旧档位对不上新模式（非子集）：原样保留，重新拉取换新
			name: "协议切换后旧档位保留", proto: "responses", id: "gpt-5",
			stored: []string{"enabled", "low"},
			want:   []string{"enabled", "low"},
		},
		{
			// 拉取到的真实档位里有推断不认识的档：以存储值为准
			name: "真实 supported_levels 不被推断覆盖", proto: "", id: "deepseek-v4-pro",
			stored: []string{"enabled", "low", "high", "max", "xhigh"},
			want:   []string{"enabled", "low", "high", "max", "xhigh"},
		},
		{
			name: "存储为空回退推断", proto: "", id: "glm-5.3",
			stored: nil,
			want:   []string{"enabled", "low", "high", "max"},
		},
		{
			// 按命名规则无思考控制的模型：存过值说明端点报过真实档位，原样保留
			name: "推断为 nil 时存储值原样", proto: "", id: "my-custom-model",
			stored: []string{"low", "high"},
			want:   []string{"low", "high"},
		},
		{
			name: "推断为 nil 且存储为空", proto: "", id: "llama-3",
			stored: nil,
			want:   nil,
		},
	}
	for _, c := range cases {
		got := effectiveReasoningLevels(c.proto, c.id, c.stored)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s：effectiveReasoningLevels(%q, %q, %v) = %v，期望 %v", c.name, c.proto, c.id, c.stored, got, c.want)
		}
	}
}

// 预置 DeepSeek 模板必须自带强度档：新用户开箱即有思考强度可选。
func TestDeepSeekPresetHasEffortLevels(t *testing.T) {
	preset, ok := PresetByID("deepseek")
	if !ok {
		t.Fatal("预置清单里没有 deepseek")
	}
	for _, m := range preset.Models {
		for _, lv := range []string{"enabled", "disabled", "low", "high", "max"} {
			if !containsStr(m.ReasoningLevels, lv) {
				t.Errorf("预置模型 %s 缺思考档位 %q（现有 %v）", m.ID, lv, m.ReasoningLevels)
			}
		}
	}
}

// 预置 Anthropic 模板开箱即有全档（含 max=预算 24576），与 llmproto 的
// anthropicThinkingBudget 一一对应。
func TestAnthropicPresetHasAllLevels(t *testing.T) {
	preset, ok := PresetByID("anthropic")
	if !ok {
		t.Fatal("预置清单里没有 anthropic")
	}
	if preset.Protocol != "anthropic" {
		t.Fatalf("anthropic 预置协议类型错误：%q", preset.Protocol)
	}
	want := []string{"enabled", "disabled", "low", "medium", "high", "max"}
	for _, m := range preset.Models {
		if !reflect.DeepEqual(m.ReasoningLevels, want) {
			t.Errorf("预置模型 %s 档位 %v，期望 %v", m.ID, m.ReasoningLevels, want)
		}
		for _, lv := range want {
			if lv != "disabled" && anthropicThinkingBudget(lv) == 0 {
				t.Errorf("预置档位 %q 在 anthropicThinkingBudget 里没有预算映射", lv)
			}
		}
	}
}

// 预置 Responses 模板：协议 responses、模型档位 disabled/low/medium/high
//（Responses 无 max 档；effort 值在 responsesEffort 里都有映射）。
func TestResponsesPresetLevels(t *testing.T) {
	preset, ok := PresetByID("openai-responses")
	if !ok {
		t.Fatal("预置清单里没有 openai-responses")
	}
	if preset.Protocol != "responses" {
		t.Fatalf("openai-responses 预置协议类型错误：%q", preset.Protocol)
	}
	if len(preset.Models) == 0 {
		t.Fatal("openai-responses 预置应自带模型目录（无 Key 可配的开箱体验）")
	}
	want := []string{"disabled", "low", "medium", "high"}
	for _, m := range preset.Models {
		if !reflect.DeepEqual(m.ReasoningLevels, want) {
			t.Errorf("预置模型 %s 档位 %v，期望 %v", m.ID, m.ReasoningLevels, want)
		}
		for _, lv := range []string{"low", "medium", "high"} {
			if responsesEffort(lv) == "" {
				t.Errorf("预置档位 %q 在 responsesEffort 里没有 effort 映射", lv)
			}
		}
	}
}
