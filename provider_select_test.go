package main

// 模型服务商的启停与选择逻辑（不回落，只报错）：
//   - 默认/会话解析只认两个来源：设置页的「全局默认模型」与会话自己手选的
//     渠道+模型；渠道被停用（手动或公司通道自动停用）、Key 被清空、模型不在
//     渠道清单——一律报错并点名原因，不再偷偷换到别的渠道；
//   - 派发子对话/工作任务没带模型参数且派发设置为「跟随主会话」时，继承主会话
//     手选的渠道/模型/Key（原行为不变）。

import (
	"strings"
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// 选择逻辑：渠道被停用就报错（不换渠道）；全局默认没设置也报错（不猜第一个）。
func TestResolveTargetNoSilentSwitch(t *testing.T) {
	a := &App{Cfg: &Config{
		Providers: []ProviderCfg{
			{ID: "company-internal", Name: "公司", BaseURL: "mock", Models: []ModelCfg{{ID: "co-m"}}},
			{ID: "zhipu", Name: "智谱", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}, {ID: "glm-5.3-flash"}}},
		},
	}}

	// 没有全局默认模型：不拿「第一个可用渠道」充数，直接报错指路
	if _, err := a.DefaultTarget(); err == nil || !strings.Contains(err.Error(), "全局默认模型") {
		t.Fatalf("未设置全局默认模型应报错: %v", err)
	}
	// 会话也没手选：同样报错（不猜）
	if _, err := a.resolveTarget(&SessionMeta{}); err == nil || !strings.Contains(err.Error(), "全局默认模型") {
		t.Fatalf("未指定模型应报错: %v", err)
	}

	// 设置全局默认后：正常解析到它
	a.Cfg.DefaultModel = ModelRef{Provider: "company-internal", Model: "co-m"}
	if got, err := a.DefaultTarget(); err != nil || got.ProviderID != "company-internal" || got.Model != "co-m" {
		t.Fatalf("全局默认模型未生效: %+v %v", got, err)
	}

	// 公司通道自动停用（出公司）：默认目标与钉在该渠道上的会话都报错，不换渠道
	a.Cfg.Providers[0].AutoDisabled = true
	if _, err := a.DefaultTarget(); err == nil || !strings.Contains(err.Error(), "自动停用") {
		t.Fatalf("自动停用的渠道应报错: %v", err)
	}
	if _, err := a.resolveTarget(&SessionMeta{Provider: "company-internal", Model: "co-m"}); err == nil || !strings.Contains(err.Error(), "自动停用") {
		t.Fatalf("钉在自动停用渠道上的会话应报错: %v", err)
	}

	// 手动停用同理（不被「可用」判定捞回）
	a.Cfg.Providers[0].AutoDisabled = false
	a.Cfg.Providers[1].Enabled = boolPtr(false)
	if a.Cfg.Providers[1].IsSelectable() {
		t.Fatalf("手动停用应不可选")
	}
	if _, err := a.resolveTarget(&SessionMeta{Provider: "zhipu", Model: "glm-5.3"}); err == nil || !strings.Contains(err.Error(), "手动停用") {
		t.Fatalf("手动停用的渠道应报错: %v", err)
	}
	// 会话手选的是可用渠道：正常解析
	a.Cfg.Providers[1].Enabled = nil
	if got, err := a.resolveTarget(&SessionMeta{Provider: "zhipu", Model: "glm-5.3-flash"}); err != nil || got.Model != "glm-5.3-flash" {
		t.Fatalf("可用渠道应正常解析: %+v %v", got, err)
	}
	// 渠道配了但密钥为空：不调用、不换渠道，直说 Key 缺
	a.Cfg.Providers[1].BaseURL = "https://zhipu/v4"
	if _, err := a.resolveTarget(&SessionMeta{Provider: "zhipu", Model: "glm-5.3"}); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("密钥为空应报错: %v", err)
	}
}

// 派发继承：没带模型参数且派发设置是「跟随主会话」时子会话抄主会话手选；
// 带了参数时参数优先；主会话也没手选时两者一起跟全局默认（原行为不变）。
//（dispatchExtras 已随建会话收敛到 createSession 删除；模型解析的公共入口是
// dispatchModelChoice——校验在建会话之前，返回应落到新会话上的 provider/model。）
func TestDispatchExtrasInheritsParentModel(t *testing.T) {
	a, main := newDispatchTestApp(t)
	a.Cfg.Providers = []ProviderCfg{
		{ID: "company-internal", Name: "公司", BaseURL: "mock", Models: []ModelCfg{{ID: "co-m"}}},
		{ID: "zhipu", Name: "智谱", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}, {ID: "glm-5.3-flash"}}},
	}
	a.Cfg.DefaultModel = ModelRef{Provider: "company-internal", Model: "co-m"}
	main.Provider, main.Model = "zhipu", "glm-5.3"
	tc := dispatchCtx(a, main)

	// 无模型参数：继承主会话手选（多 Key 供应商连 Key 归属一起继承）
	provider, providerKey, mdl, err := tc.dispatchModelChoice("")
	if err != nil || provider != "zhipu" || mdl != "glm-5.3" {
		t.Fatalf("未指定模型应继承主会话: %v %v %v %v", provider, providerKey, mdl, err)
	}

	// 显式参数优先
	provider, providerKey, mdl, err = tc.dispatchModelChoice("zhipu/glm-5.3-flash")
	if err != nil || provider != "zhipu" || mdl != "glm-5.3-flash" {
		t.Fatalf("显式参数应优先: %v %v %v %v", provider, providerKey, mdl, err)
	}

	// 主会话跟随全局默认（未手选）：子会话同样走全局默认（回空值，由子会话跟随）
	main.Provider, main.Model = "", ""
	provider, providerKey, mdl, err = tc.dispatchModelChoice("")
	if err != nil || provider != "" || mdl != "" || providerKey != "" {
		t.Fatalf("主会话未手选时子会话应走全局默认: %v %v %v %v", provider, providerKey, mdl, err)
	}
}
