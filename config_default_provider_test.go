package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 出厂不预置模型通道（2026-11-13 用户口径：新装打开就该彻底是空的，通道只
// 来自公司接入或用户自己添加）。这组用例守三条线：
//   1. 全新安装 providers 为空，且落盘的 config.json 里不出现那个出厂占位；
//   2. 老配置只写了出厂默认三元组（空 Key）时不再折算成通道，填过 Key 的照旧折算
//     （老用户不能丢配置）；
//   3. 存量机器上旧版已经种下的出厂占位通道，加载时被删掉，用户自己配过的通道
//     一个不动。

func loadCfgFrom(t *testing.T, body string) (*Config, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	return cfg, path
}

func TestFreshInstallHasNoProvider(t *testing.T) {
	cfg, path := loadCfgFrom(t, "")
	if len(cfg.Providers) != 0 {
		t.Fatalf("全新安装不该有任何通道，实际 %d 个：%+v", len(cfg.Providers), cfg.Providers)
	}
	if cfg.BaseURL != "" || cfg.Model != "" {
		t.Fatalf("出厂三元组应为空，实际 baseURL=%q model=%q", cfg.BaseURL, cfg.Model)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), legacyDefaultModel) || strings.Contains(string(data), legacyDefaultBaseURL) {
		t.Fatalf("落盘的出厂配置里不该出现 deepseek 占位：\n%s", data)
	}
	// 没有通道时解析出的调用目标也是空的：上层据此给出「先添加通道」的指引
	if tgt := (&App{Cfg: cfg}).DefaultTarget(); strings.TrimSpace(tgt.BaseURL) != "" || tgt.Model != "" {
		t.Fatalf("无通道时默认目标应为空，实际 %+v", tgt)
	}
}

func TestLegacyDefaultTripleNotFolded(t *testing.T) {
	// 旧版出厂写进 config.json 的就是这一组（空 Key）：用户没配过，不该变成通道
	body := `{
  "baseURL": "https://api.deepseek.com/v1",
  "apiKey": "",
  "model": "deepseek-chat",
  "port": 8788
}`
	cfg, _ := loadCfgFrom(t, body)
	if len(cfg.Providers) != 0 {
		t.Fatalf("出厂默认三元组不该折算成通道，实际：%+v", cfg.Providers)
	}
}

func TestLegacyTripleWithKeyStillFolded(t *testing.T) {
	// 老用户填过 Key：照旧折算成第一个通道（升级不能把人家配好的通道弄丢）
	body := `{
  "baseURL": "https://api.example.com/v1",
  "apiKey": "sk-legacy",
  "model": "m-1"
}`
	cfg, _ := loadCfgFrom(t, body)
	if len(cfg.Providers) != 1 {
		t.Fatalf("填过 Key 的老三元组应折算成 1 个通道，实际 %d 个：%+v", len(cfg.Providers), cfg.Providers)
	}
	p := cfg.Providers[0]
	if p.BaseURL != "https://api.example.com/v1" || p.APIKey != "sk-legacy" {
		t.Fatalf("折算出的通道地址/密钥不对：%+v", p)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "m-1" {
		t.Fatalf("折算出的模型清单不对：%+v", p.Models)
	}
}

func TestDropDefaultPlaceholderProvider(t *testing.T) {
	// 存量机器：旧版把出厂三元组折成的占位通道 + 用户自己配的通道混在一起
	body := `{
  "providers": [
    {"id": "company-internal", "name": "公司·内部通道", "baseURL": "https://jx.example.com/v1", "apiKey": "sk-c",
     "models": [{"id": "glm-5.3"}]},
    {"id": "deepseek", "name": "DeepSeek", "baseURL": "https://api.deepseek.com/v1", "apiKey": "",
     "models": [{"id": "deepseek-chat", "contextWindow": 1048576}]},
    {"id": "ollama", "name": "本地", "baseURL": "http://127.0.0.1:11434/v1", "apiKey": "",
     "models": [{"id": "qwen3"}]}
  ]
}`
	cfg, path := loadCfgFrom(t, body)
	if len(cfg.Providers) != 2 {
		t.Fatalf("出厂占位应被删掉、其余保留，实际 %d 个：%+v", len(cfg.Providers), cfg.Providers)
	}
	for _, p := range cfg.Providers {
		if p.ID == "deepseek" {
			t.Fatalf("出厂占位通道没被删掉：%+v", p)
		}
	}
	// 清理要落盘：重启（再读一次文件）不该又冒出来
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), legacyDefaultModel) {
		t.Fatalf("清理结果没落盘，文件里还留着占位：\n%s", data)
	}
}

func TestKeepUserConfiguredDeepSeekProvider(t *testing.T) {
	// 用户自己加的 deepseek 通道（填了 Key / 模型清单是预置那套）不能被误删
	body := `{
  "providers": [
    {"id": "deepseek", "name": "DeepSeek", "baseURL": "https://api.deepseek.com/v1", "apiKey": "sk-user",
     "models": [{"id": "deepseek-chat"}]},
    {"id": "deepseek-keyless", "name": "DeepSeek2", "baseURL": "https://api.deepseek.com/v1", "apiKey": "",
     "models": [{"id": "deepseek-flash"}, {"id": "deepseek-v4-pro"}]}
  ]
}`
	cfg, _ := loadCfgFrom(t, body)
	if len(cfg.Providers) != 2 {
		t.Fatalf("配过 Key 或模型清单不同的通道都不该删，实际 %d 个：%+v", len(cfg.Providers), cfg.Providers)
	}
}
