package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 明文停用迁移（2026-10-25）：自家地址 http→https 收敛到 https://jxaizz.com:2227，
// 第三方/https/空值一律不动。幂等（迁过即 https，不再匹配）。
func TestMigrateLegacyPlainURL(t *testing.T) {
	cases := []struct {
		in    string
		want  string
		moved bool
	}{
		{"http://192.168.0.210:2227/announce", "https://jxaizz.com:2227/announce", true},
		{"http://jxaizz.com:2227/market/manifest.json", "https://jxaizz.com:2227/market/manifest.json", true},
		{"http://jxaizz.cn:2227", "https://jxaizz.com:2227", true},
		{"http://jxkj.mycloudnas.com:2227", "https://jxaizz.com:2227", true},
		{"http://192.168.0.210:3000/v1", "https://jxaizz.com:2227/v1", true}, // new-api 明文直连的历史下发
		// 不动：https（Gitea 默认源）、非自家地址（用户的 Ollama/第三方明文）、空、非 URL
		{"https://jxaizz.com:3000/api/v1/repos/x/y/releases/latest", "https://jxaizz.com:3000/api/v1/repos/x/y/releases/latest", false},
		{"http://192.168.1.5:11434/v1", "http://192.168.1.5:11434/v1", false},
		{"http://api.example.com/v1", "http://api.example.com/v1", false},
		{"", "", false},
		{"mock", "mock", false},
	}
	for _, c := range cases {
		got, moved := migrateLegacyPlainURL(c.in)
		if got != c.want || moved != c.moved {
			t.Errorf("migrateLegacyPlainURL(%q) = %q,%v；期望 %q,%v", c.in, got, moved, c.want, c.moved)
		}
	}
}

// 整链迁移：老配置（明文自家地址 + 公司通道明文 baseURL + 第三方明文提供方）
// 经 LoadConfig 后自家地址全部 https 化、第三方原样保留，并立即回写落盘。
func TestLoadConfigMigratesPlainSelfURLs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{
  "wizardServer": "http://192.168.0.210:2227",
  "marketURL": "http://192.168.0.210:2227/market/manifest.json",
  "updateURL": "http://jxaizz.com:2227/updates/latest",
  "providers": [
    {"id": "company-internal", "name": "公司·内部通道", "baseURL": "http://192.168.0.210:3000/v1", "apiKey": "sk-x",
     "models": [{"id": "glm-5.3"}]},
    {"id": "ollama", "name": "本地", "baseURL": "http://192.168.1.5:11434/v1", "apiKey": "",
     "models": [{"id": "qwen"}]}
  ]
}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.WizardServer != "https://jxaizz.com:2227" {
		t.Fatalf("WizardServer 应迁移，得到 %q", cfg.WizardServer)
	}
	if cfg.MarketURL != "https://jxaizz.com:2227/market/manifest.json" {
		t.Fatalf("MarketURL 应迁移，得到 %q", cfg.MarketURL)
	}
	if cfg.UpdateURL != "https://jxaizz.com:2227/updates/latest" {
		t.Fatalf("UpdateURL 应迁移，得到 %q", cfg.UpdateURL)
	}
	var wiz, oll *ProviderCfg
	for i := range cfg.Providers {
		switch cfg.Providers[i].ID {
		case wizardProviderID:
			wiz = &cfg.Providers[i]
		case "ollama":
			oll = &cfg.Providers[i]
		}
	}
	if wiz == nil || wiz.BaseURL != "https://jxaizz.com:2227/v1" {
		t.Fatalf("公司通道 baseURL 应迁移到服务端 /v1 反代，得到 %#v", wiz)
	}
	if oll == nil || oll.BaseURL != "http://192.168.1.5:11434/v1" {
		t.Fatalf("第三方明文提供方不应被动，得到 %#v", oll)
	}
	// 迁移应立即落盘（重启后不再回到明文）
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(saved), "http://192.168.0.210") || strings.Contains(string(saved), "http://jxaizz.com") {
		t.Fatalf("落盘的配置仍含自家明文地址: %s", saved)
	}
	if !strings.Contains(string(saved), "http://192.168.1.5:11434/v1") {
		t.Fatalf("第三方提供方应在落盘文件中原样保留: %s", saved)
	}
}
