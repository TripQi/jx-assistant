package main

import (
	"context"
	"strings"
	"testing"
)

// 显式归属是计费身份，不得当成没有归属的老会话重新猜测。
func TestExplicitKeyDeletedDoesNotFallback(t *testing.T) {
	p := multiKeyProvider()
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}, DefaultModel: ModelRef{
		Provider: p.ID, ProviderKey: "k1", Model: "shared",
	}}}
	for _, key := range []string{"deleted-key", " deleted-key "} {
		sess := &SessionMeta{Mode: "aux", Provider: p.ID, ProviderKey: key, Model: "shared"}
		got, err := a.resolveTarget(sess)
		if err == nil || !strings.Contains(err.Error(), "Key") || !strings.Contains(err.Error(), "deleted-key") {
			t.Fatalf("已删显式Key必须点名报错，不能换Key: target=%+v err=%v", got, err)
		}
		if got != (LLMTarget{}) {
			t.Fatalf("拒绝时不得暴露可调用的替代目标: %+v", got)
		}
		// RunAgent 在装配上下文及调用模型之前即解析目标；空 Store 会暴露任何
		// 继续执行的错误，因此这里也验证没有进入模型或历史读写路径。
		if err := a.RunAgent(context.Background(), sess, func(Event) {
			t.Error("失效Key不应开始模型或工具事件")
		}); err == nil || !strings.Contains(err.Error(), "deleted-key") {
			t.Fatalf("执行入口必须原样失败，不尝试全局默认/另一Key: %v", err)
		}
		t.Logf("explicit %q: target rejected; RunAgent stopped before model call: %v", key, err)
	}
}

func TestExplicitKeyValidKeepsIdentity(t *testing.T) {
	p := multiKeyProvider()
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}}}
	for _, model := range []string{"shared", "only-a", "not-in-catalog"} {
		// only-a只在k1目录里，但存量会话显式钉k2：目录不是运行权限裁决。
		got, err := a.resolveTarget(&SessionMeta{Provider: p.ID, ProviderKey: " k2 ", Model: model})
		if err != nil || got.KeyID != "k2" || got.APIKey != p.Keys[1].APIKey || got.Protocol != "anthropic" || got.Model != model {
			t.Fatalf("现存Key归属必须保持，不能因目录变化换身份: model=%q got=%+v err=%v", model, got, err)
		}
	}
	// 新建严格检查仍拒绝跨Key目录模型：只保留已存在会话的手填兼容。
	if err := a.checkModelRef(ModelRef{Provider: p.ID, ProviderKey: "k2", Model: "only-a"}, whereSessionModel); err == nil {
		t.Fatal("不能放宽新建模型的Key归属检查")
	}
	p.Keys[1].APIKey = ""
	a.Cfg.Providers[0] = p
	if _, err := a.resolveTarget(&SessionMeta{Provider: p.ID, ProviderKey: "k2", Model: "shared"}); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("显式Key密钥清空仍应报错，不能用k1: %v", err)
	}
}

func TestLegacyMissingKeyStillResolves(t *testing.T) {
	p := multiKeyProvider()
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}}}
	for _, tc := range []struct {
		provider, model, key string
	}{
		{p.ID, "claude-x", "k2"},
		{p.ID, "not-in-catalog", "k1"},
		{"", "claude-x", "k2"},
	} {
		got, err := a.resolveTarget(&SessionMeta{Provider: tc.provider, Model: tc.model})
		if err != nil || got.KeyID != tc.key || got.Model != tc.model {
			t.Fatalf("无Key老引用恢复应兼容: case=%+v got=%+v err=%v", tc, got, err)
		}
	}
}

func TestExplicitKeyWithoutProviderIsRejected(t *testing.T) {
	p := multiKeyProvider()
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}, DefaultModel: ModelRef{
		Provider: p.ID, ProviderKey: "k1", Model: "shared",
	}}}
	for _, model := range []string{"", "shared"} {
		if got, err := a.resolveTarget(&SessionMeta{ProviderKey: "k2", Model: model}); err == nil || got != (LLMTarget{}) {
			t.Fatalf("有Key无渠道不能丢掉显式身份后猜默认: got=%+v err=%v", got, err)
		}
	}
}

func TestDefaultExplicitKeyDeletedDoesNotFallback(t *testing.T) {
	p := multiKeyProvider()
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}, DefaultModel: ModelRef{
		Provider: p.ID, ProviderKey: "deleted-key", Model: "shared",
	}}}
	if got, err := a.DefaultTarget(); err == nil || got != (LLMTarget{}) || !strings.Contains(err.Error(), "deleted-key") {
		t.Fatalf("全局显式Key删除也必须报错: got=%+v err=%v", got, err)
	}
	if got, err := a.resolveTarget(&SessionMeta{}); err == nil || got != (LLMTarget{}) || !strings.Contains(err.Error(), "deleted-key") {
		t.Fatalf("跟随全局会话不能恢复到其他Key: got=%+v err=%v", got, err)
	}
}
