package main

// 模型服务商的启停与选择逻辑：
//   - 停用（手动 Enabled=false 或公司通道 AutoDisabled）的提供方不参与默认选择，
//     会话钉死在停用提供方上也回落到第一个可用提供方（出公司场景的核心行为）
//   - 派发子对话/工作任务没带模型参数时继承主会话手选的服务商/模型
//   - 回落到的提供方没有原会话模型时用它的默认模型，不把不存在的模型名带过去

import (
	"testing"
)

func boolPtr(b bool) *bool { return &b }

// 选择逻辑：默认/会话解析都跳过停用提供方；模型不在回落方目录时用其默认模型。
func TestResolveTargetSkipsDisabled(t *testing.T) {
	a := &App{Cfg: &Config{Providers: []ProviderCfg{
		{ID: "company-internal", Name: "公司", BaseURL: "http://co:3000/v1", Models: []ModelCfg{{ID: "co-m"}}},
		{ID: "zhipu", Name: "智谱", BaseURL: "https://zhipu/v4", Models: []ModelCfg{{ID: "glm-5.3"}, {ID: "glm-5.3-flash"}}},
	}}}

	// 基线：默认=第一个提供方（公司）
	if got := a.DefaultTarget(); got.ProviderID != "company-internal" || got.Model != "co-m" {
		t.Fatalf("基线默认不对: %+v", got)
	}

	// 公司通道自动停用（出公司）：默认落到智谱
	a.Cfg.Providers[0].AutoDisabled = true
	if got := a.DefaultTarget(); got.ProviderID != "zhipu" || got.Model != "glm-5.3" {
		t.Fatalf("自动停用后默认应切到智谱: %+v", got)
	}

	// 会话钉死在公司通道：回落智谱；模型 co-m 不在智谱目录 → 用智谱默认
	sess := &SessionMeta{Provider: "company-internal", Model: "co-m"}
	if got := a.resolveTarget(sess); got.ProviderID != "zhipu" || got.Model != "glm-5.3" {
		t.Fatalf("停用提供方上的会话应回落: %+v", got)
	}

	// 会话模型恰好也在回落方目录：模型保留
	sess = &SessionMeta{Provider: "company-internal", Model: "glm-5.3-flash"}
	if got := a.resolveTarget(sess); got.Model != "glm-5.3-flash" {
		t.Fatalf("回落方有的模型应保留: %+v", got)
	}

	// 手动停用同理，且手动停用不被「可用」判定捞回
	a.Cfg.Providers[0].AutoDisabled = false
	a.Cfg.Providers[1].Enabled = boolPtr(false)
	if a.Cfg.Providers[1].IsSelectable() {
		t.Fatalf("手动停用应不可选")
	}
	// 全停时回落 providers[0]（用户自己的选择，如实报错而非静默换通道）
	a.Cfg.Providers[0].Enabled = boolPtr(false)
	if got := a.DefaultTarget(); got.ProviderID != "company-internal" {
		t.Fatalf("全停应回落 providers[0]: %+v", got)
	}
}

// 派发继承：没带模型参数时子会话抄主会话手选；带了参数时参数优先。
//（dispatchExtras 已随建会话收敛到 createSession 删除；模型解析的公共入口是
// dispatchModelChoice——校验在建会话之前，返回应落到新会话上的 provider/model。）
func TestDispatchExtrasInheritsParentModel(t *testing.T) {
	a, main := newDispatchTestApp(t)
	a.Cfg.Providers = []ProviderCfg{
		{ID: "company-internal", Name: "公司", BaseURL: "http://co:3000/v1", Models: []ModelCfg{{ID: "co-m"}}},
		{ID: "zhipu", Name: "智谱", BaseURL: "https://zhipu/v4", Models: []ModelCfg{{ID: "glm-5.3"}}},
	}
	main.Provider, main.Model = "zhipu", "glm-5.3"
	tc := dispatchCtx(a, main)

	// 无模型参数：继承主会话手选（多 Key 供应商连 Key 归属一起继承）
	provider, providerKey, mdl, err := tc.dispatchModelChoice("")
	if err != nil || provider != "zhipu" || mdl != "glm-5.3" || providerKey != "" {
		t.Fatalf("未指定模型应继承主会话: %v %v %v %v", provider, providerKey, mdl, err)
	}

	// 显式参数优先（不带 Key 归属：由 resolveKey 按模型名找回）
	provider, providerKey, mdl, err = tc.dispatchModelChoice("zhipu/glm-5.3-flash")
	if err != nil || provider != "zhipu" || mdl != "glm-5.3-flash" || providerKey != "" {
		t.Fatalf("显式参数应优先: %v %v %v %v", provider, providerKey, mdl, err)
	}

	// 主会话跟随全局默认（未手选）：子会话同样走全局默认（空）
	main.Provider, main.Model = "", ""
	provider, providerKey, mdl, err = tc.dispatchModelChoice("")
	if err != nil || provider != "" || mdl != "" || providerKey != "" {
		t.Fatalf("主会话未手选时子会话应走全局默认: %v %v %v %v", provider, providerKey, mdl, err)
	}
}
