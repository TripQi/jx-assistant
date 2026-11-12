// websearch_providers.go 的单测（配置与存储这一层）：Key 掩码脱敏、DPAPI 加密
// 落盘与透明解密、老配置补默认供应商、启用/能力/地址的筛选与顺位重排、失败分类。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// searchTestApp 只有搜索供应商配置的 App（配置层用例不需要数据库与数据目录）。
func searchTestApp(providers ...SearchProviderCfg) *App {
	return &App{Cfg: &Config{SearchProviders: providers}}
}

// 失败分类映射：状态码 → 类别（回落时给用户看的说法）。
func TestSearchErrorClassify(t *testing.T) {
	cases := []struct {
		status int
		want   searchErrKind
	}{
		{401, searchErrAuth}, {403, searchErrAuth}, {402, searchErrQuota},
		{432, searchErrQuota}, {433, searchErrQuota}, {429, searchErrRateLimit},
		{408, searchErrNetwork}, {500, searchErrUpstream}, {502, searchErrUpstream},
		{404, searchErrUpstream}, {422, searchErrParse}, {400, searchErrParse},
	}
	for _, c := range cases {
		if got := classifySearchStatus(c.status); got != c.want {
			t.Fatalf("HTTP %d 分类错误：%v 应为 %v", c.status, got, c.want)
		}
	}
	// 抽取口径：403/404/422 是「目标站不让抓」而不是 Key 问题
	for _, s := range []int{403, 404, 422} {
		if got := classifyExtractStatus(s); got != searchErrRejected {
			t.Fatalf("抽取 HTTP %d 应为目标站拒绝，实得 %v", s, got)
		}
	}
	if got := classifyExtractStatus(401); got != searchErrAuth {
		t.Fatalf("抽取 401 仍是鉴权问题，实得 %v", got)
	}
	// 类别中文说法：设置页「测试」按钮直接展示这个
	if searchErrAuth.label() != "Key 无效或无权限" || searchErrQuota.label() != "额度或余额不足" {
		t.Fatalf("失败类别文案不对: %s / %s", searchErrAuth.label(), searchErrQuota.label())
	}
}

// Key 掩码：设置页回显只有前 4 后 4，明文绝不出现在 /api/config 的 JSON 里。
func TestSearchProviderKeyMasked(t *testing.T) {
	// 测试用假 Key（形状与真实一致即可）：仓库里不放任何真实凭据
	plain := "sk-0123456789abcdef0123456789abcdef"
	a := searchTestApp(SearchProviderCfg{ID: "deepseek", Kind: spKindDeepSeek, APIKey: plain,
		Capabilities: SearchProviderCaps{Search: true, Extract: true}, Order: 1})
	views := a.searchProviderViews()
	if len(views) != 1 {
		t.Fatalf("应回 1 条供应商: %+v", views)
	}
	if views[0].APIKeyMasked != "sk-0****cdef" {
		t.Fatalf("掩码不对: %q", views[0].APIKeyMasked)
	}
	raw, _ := json.Marshal(views)
	if strings.Contains(string(raw), plain) {
		t.Fatalf("回显 JSON 里出现了明文 Key: %s", raw)
	}
}

// 落盘加密：Save 后配置里只有 dpapi 密文，重新加载能透明解密回明文。
func TestSearchProviderKeyEncryptedAtRest(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	plain := "sk-encrypt-me-1234567890"
	cfg := &Config{SearchProviders: []SearchProviderCfg{
		{ID: "deepseek", Name: "DeepSeek", Kind: spKindDeepSeek, BaseURL: "https://api.deepseek.com",
			APIKey: plain, Capabilities: SearchProviderCaps{Search: true, Extract: true}, Order: 1},
	}}
	if err := cfg.Save(path); err != nil {
		t.Fatalf("保存失败: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), plain) {
		t.Fatalf("配置文件里不该出现明文 Key:\n%s", raw)
	}
	if !strings.Contains(string(raw), dpapiPrefix) {
		t.Fatalf("Key 应以 dpapi 密文落盘:\n%s", raw)
	}
	loaded, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(loaded.SearchProviders) != 1 || loaded.SearchProviders[0].APIKey != plain {
		t.Fatalf("加载应透明解密回明文: %+v", loaded.SearchProviders)
	}
}

// 老配置（没有 searchProviders 键）加载时补默认四条；写了空数组则不补。
func TestSearchProvidersSeedDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"port":8899}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(cfg.SearchProviders) != 4 {
		t.Fatalf("老配置应补默认 4 条供应商: %+v", cfg.SearchProviders)
	}
	if cfg.SearchProviders[0].Kind != spKindGateway || cfg.SearchProviders[0].Order != 1 {
		t.Fatalf("公司网关应排第 1 位: %+v", cfg.SearchProviders[0])
	}
	if cfg.SearchProviders[0].APIKey != "" {
		t.Fatal("默认供应商不该带任何 Key")
	}
	// 显式空数组 = 用户只要老网关，不再补种
	path2 := filepath.Join(dir, "config2.json")
	if err := os.WriteFile(path2, []byte(`{"searchProviders":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfig(path2)
	if err != nil {
		t.Fatalf("加载失败: %v", err)
	}
	if len(cfg2.SearchProviders) != 0 {
		t.Fatalf("显式空数组不该补种: %+v", cfg2.SearchProviders)
	}
}

// 供应商筛选：停用的、缺能力的、网关没地址的都跳过；顺位按 order 重排。
func TestSearchProvidersFiltering(t *testing.T) {
	off := false
	a := &App{Cfg: &Config{SearchProviders: []SearchProviderCfg{
		{ID: "gateway", Kind: spKindGateway, Capabilities: SearchProviderCaps{Search: true}, Order: 1},
		{ID: "off", Kind: spKindDeepSeek, APIKey: "k", Enabled: &off, Capabilities: SearchProviderCaps{Search: true}, Order: 2},
		{ID: "bocha", Kind: spKindBocha, APIKey: "k", Capabilities: SearchProviderCaps{Search: true}, Order: 3},
	}}}
	searchCap := func(c SearchProviderCaps) bool { return c.Search }
	got := a.searchProvidersFor(searchCap)
	if len(got) != 1 || got[0].cfg.ID != "bocha" || got[0].rank != 1 {
		t.Fatalf("应只剩启用且有地址的博查（顺位 1）: %+v", got)
	}
	if len(a.searchProvidersFor(func(c SearchProviderCaps) bool { return c.Extract })) != 0 {
		t.Fatal("博查没有抽取能力，不该进抽取链路")
	}
	// 配了市场地址后公司网关参与，且按 order 排在第 1 位
	a.Cfg.MarketURL = "https://market.example.com/market/manifest.json"
	got = a.searchProvidersFor(searchCap)
	if len(got) != 2 || got[0].cfg.ID != "gateway" || got[0].rank != 1 || got[1].rank != 2 {
		t.Fatalf("网关应排在博查之前: %+v", got)
	}
	if got[0].base != "https://market.example.com" {
		t.Fatalf("网关地址应取市场同源: %s", got[0].base)
	}
}
