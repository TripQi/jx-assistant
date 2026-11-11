package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// 档位与模型彻底解绑：全系统一套六档，任何模型、任何渠道看到的是同一份清单。
func TestThinkingLevelsFixed(t *testing.T) {
	want := []string{"none", "low", "medium", "high", "xhigh", "max"}
	if got := thinkingLevels(); !reflect.DeepEqual(got, want) {
		t.Fatalf("thinkingLevels() = %v，期望 %v", got, want)
	}
}

// 老会话库里的档位要能兼容读：disabled → none、enabled（只开关不指深浅）→
// 空串（跟随服务端默认）；六档原样（大小写/空白归一）；其余串判非法。
func TestNormalizeThinkingLevel(t *testing.T) {
	cases := []struct {
		in, want string
		ok       bool
	}{
		{"", "", true},
		{"none", "none", true},
		{"LOW", "low", true},
		{" Medium ", "medium", true},
		{"high", "high", true},
		{"xhigh", "xhigh", true},
		{"max", "max", true},
		{"disabled", "none", true},
		{"enabled", "", true},
		{"minimal", "", false}, // 服务端旧档位名不再透传
		{"ultra", "", false},
	}
	for _, c := range cases {
		got, ok := normalizeThinkingLevel(c.in)
		if got != c.want || ok != c.ok {
			t.Errorf("normalizeThinkingLevel(%q) = (%q, %v)，期望 (%q, %v)", c.in, got, ok, c.want, c.ok)
		}
	}
}

// 预置模板只声明协议与模型目录，不再逐模型写档位（档位全系统一套）。
func TestPresetProtocols(t *testing.T) {
	want := map[string]string{"anthropic": "anthropic", "openai-responses": "responses", "deepseek": ""}
	for id, wantProto := range want {
		preset, ok := PresetByID(id)
		if !ok {
			t.Fatalf("预置清单里没有 %s", id)
		}
		if preset.Protocol != wantProto {
			t.Errorf("预置 %s 协议类型 %q，期望 %q", id, preset.Protocol, wantProto)
		}
	}
	// Anthropic / Responses 预置必须自带模型目录（无 Key 可配的开箱体验）
	for _, id := range []string{"anthropic", "openai-responses"} {
		preset, _ := PresetByID(id)
		if len(preset.Models) == 0 {
			t.Errorf("预置 %s 应自带模型目录", id)
		}
	}
}

// 「拉取模型」不采用服务端上报的 effort.supported_levels：未知档位（minimal/
// ultra 之类）透传到前端只会变成「能选、后端 400」。其余真实字段（上下文、
// 输出上限、多模态）照旧优先采用。
func TestFetchModelsIgnoresServerLevels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"deepseek-flash","display_name":"DeepSeek V4.1 Flash","context_window":1048576,
			 "max_output_tokens":65536,"input_modalities":["text","image"],
			 "effort":{"supported_levels":["minimal","low","medium","ultra"]}}
		]}`))
	}))
	defer srv.Close()
	a := &App{}
	models, err := a.FetchModels(&ProviderCfg{ID: "x", BaseURL: srv.URL})
	if err != nil {
		t.Fatalf("FetchModels: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("模型条数 %d，期望 1", len(models))
	}
	m := models[0]
	if m.ID != "deepseek-flash" || m.Name != "DeepSeek V4.1 Flash" {
		t.Errorf("id/名称解析错误：%+v", m)
	}
	if m.ContextWindow != 1048576 || m.MaxOutput != 65536 || !m.SupportsImages {
		t.Errorf("真实字段未采用：%+v", m)
	}
	// 结构里不再有档位字段：服务端上报的 minimal/ultra 不可能漏到前端
	raw, _ := json.Marshal(m)
	var obj map[string]any
	_ = json.Unmarshal(raw, &obj)
	if _, has := obj["reasoningLevels"]; has {
		t.Errorf("模型目录不该再带档位：%s", raw)
	}
}
