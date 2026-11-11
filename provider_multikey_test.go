package main

// 自定义供应商多 Key（多分组）单测：
//   - 老配置（供应商级单 apiKey+models）加载即迁移成「只有一把 Key」的结构，
//     落盘后能原样回读，预置提供方与公司内部通道不掺和
//   - 模型归属解析：显式 Key > 模型清单里有它的那把 > 第一把可用 Key
//   - 协议：Key 级覆盖优先，"chat" 是显式 OpenAI 兼容（区别于空串的「跟随」）
//   - 拉取模型按 Key 走（密钥与协议都取那把 Key 的）
//   - 接口：/api/models 拍平且带 Key 归属与备注名，/api/config 每把 Key 只回掩码
//   - 会话存得住「模型归属哪把 Key」

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// multiKeyProvider 两把 Key 的典型形态：同一 BaseURL、各自的分组备注名/密钥/
// 协议，两个分组都拉到同名模型 shared（重名模型就是靠 Key 归属定位的）。
func multiKeyProvider() ProviderCfg {
	return ProviderCfg{
		ID: "custom-1", Name: "中转站", BaseURL: "https://relay.example.com/v1",
		Keys: []ProviderKeyCfg{
			{ID: "k1", Label: "默认分组", APIKey: "sk-daily-1111",
				Models: []ModelCfg{{ID: "shared"}, {ID: "only-a"}}},
			{ID: "k2", Label: "Claude 分组", APIKey: "sk-claude-2222", Protocol: "anthropic",
				Models: []ModelCfg{{ID: "shared"}, {ID: "claude-x"}}},
		},
	}
}

// 老配置里的自定义供应商（单 APIKey + Models）加载即升级成一把 Key；预置提供方
// 与公司内部通道保持单 Key 结构（它们的密钥/模型由模板与服务端维护）。迁移立即
// 落盘，再加载一次 Key 归属 id 不变（会话里的模型引用靠它，不能每次启动都换）。
func TestLoadConfigMigratesSingleKeyProvider(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{
  "providers": [
    {"id": "custom-77", "name": "中转站", "baseURL": "https://relay.example.com/v1",
     "apiKey": "sk-legacy-9999", "models": [{"id": "gpt-x"}, {"id": "gpt-y"}]},
    {"id": "deepseek", "name": "DeepSeek", "baseURL": "https://api.deepseek.com/v1",
     "apiKey": "sk-preset-1234", "models": [{"id": "deepseek-flash"}]},
    {"id": "company-internal", "name": "公司·内部通道", "baseURL": "http://lan:3000/v1",
     "apiKey": "sk-company-5678", "models": [{"id": "co-m"}]}
  ]
}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	var custom, preset, company *ProviderCfg
	for i := range cfg.Providers {
		switch cfg.Providers[i].ID {
		case "custom-77":
			custom = &cfg.Providers[i]
		case "deepseek":
			preset = &cfg.Providers[i]
		case wizardProviderID:
			company = &cfg.Providers[i]
		}
	}
	if custom == nil || len(custom.Keys) != 1 {
		t.Fatalf("自定义供应商应迁成一把 Key: %#v", custom)
	}
	k := custom.Keys[0]
	if k.ID == "" {
		t.Fatal("迁移出来的 Key 必须有稳定归属 id")
	}
	if k.APIKey != "sk-legacy-9999" || len(k.Models) != 2 || k.Models[0].ID != "gpt-x" {
		t.Fatalf("迁移丢了密钥或模型: %#v", k)
	}
	if len(preset.Keys) != 0 || len(company.Keys) != 0 {
		t.Fatalf("预置提供方/公司通道不该被迁移成多 Key: preset=%d company=%d", len(preset.Keys), len(company.Keys))
	}

	// 迁移立即落盘；再加载一次 Key 归属 id 不变（幂等）
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var raw struct {
		Providers []struct {
			ID   string `json:"id"`
			Keys []struct {
				ID string `json:"id"`
			} `json:"keys"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("落盘配置不是合法 JSON: %v", err)
	}
	firstID := ""
	for _, p := range raw.Providers {
		if p.ID == "custom-77" {
			if len(p.Keys) != 1 {
				t.Fatalf("落盘里应有一把 Key: %s", data)
			}
			firstID = p.Keys[0].ID
		}
	}
	if firstID != k.ID {
		t.Fatalf("落盘的 Key 归属 id 应为 %q，得到 %q", k.ID, firstID)
	}
	reloaded, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reloaded.Providers {
		if reloaded.Providers[i].ID != "custom-77" {
			continue
		}
		if got := reloaded.Providers[i].Keys; len(got) != 1 || got[0].ID != k.ID || got[0].APIKey != "sk-legacy-9999" {
			t.Fatalf("回读不一致: %#v", got)
		}
	}
}

// 多 Key 配置落盘再回读：每把 Key 的归属 id/备注名/协议/密钥/模型都要原样回来
//（密钥落盘是密文，回读要能透明解密回明文）。
func TestConfigSaveRoundTripsMultiKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	cfg := DefaultConfig()
	cfg.DataDir = dir
	p := multiKeyProvider()
	cfg.Providers = []ProviderCfg{p}
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	got, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Providers) != 1 || len(got.Providers[0].Keys) != 2 {
		t.Fatalf("Key 数量不对: %#v", got.Providers)
	}
	keys := got.Providers[0].Keys
	if keys[0].ID != "k1" || keys[0].Label != "默认分组" || keys[0].APIKey != "sk-daily-1111" {
		t.Fatalf("第一把 Key 回读不一致: %#v", keys[0])
	}
	if keys[1].ID != "k2" || keys[1].Protocol != "anthropic" || keys[1].APIKey != "sk-claude-2222" ||
		len(keys[1].Models) != 2 {
		t.Fatalf("第二把 Key 回读不一致: %#v", keys[1])
	}
	// 供应商级镜像：第一把 Key 的密钥与模型（老版本 exe 回退时读得到）
	if got.Providers[0].APIKey != "sk-daily-1111" || len(got.Providers[0].Models) != 2 {
		t.Fatalf("供应商级镜像未同步第一把 Key: %#v", got.Providers[0])
	}
}

// 模型归属解析：显式 Key 最优先；老会话没有 Key 归属时按模型名找回它的那把；
// 都找不到落第一把可用 Key（不能让调用直接挂掉）。
func TestResolveKeyAndTarget(t *testing.T) {
	p := multiKeyProvider()
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}, Model: "shared"}}

	// 显式 Key：密钥与协议都取那把的（Key 级协议覆盖优先于供应商级）
	p.Protocol = "responses" // 供应商级默认
	a.Cfg.Providers[0] = p
	tk := a.targetOf(a.providerByID("custom-1"), "k2", "shared")
	if tk.APIKey != "sk-claude-2222" || tk.Protocol != "anthropic" {
		t.Fatalf("应落到第二把 Key 的密钥与协议: %+v", tk)
	}
	// 会话带着 Key 归属：解析到同一把（重名模型靠它定位）
	sess := &SessionMeta{Provider: "custom-1", ProviderKey: "k2", Model: "shared"}
	if got := a.resolveTarget(sess); got.APIKey != "sk-claude-2222" {
		t.Fatalf("会话 Key 归并未生效: %+v", got)
	}
	// 老会话（没有 Key 归属）：按模型名找回那把 Key
	sess = &SessionMeta{Provider: "custom-1", Model: "claude-x"}
	if got := a.resolveTarget(sess); got.APIKey != "sk-claude-2222" {
		t.Fatalf("应按模型清单找回 Key: %+v", got)
	}
	// 模型名不在任何 Key 的清单里（手工改过清单的场景）：落第一把可用 Key
	sess = &SessionMeta{Provider: "custom-1", Model: "not-in-catalog"}
	if got := a.resolveTarget(sess); got.APIKey != "sk-daily-1111" || got.Model != "not-in-catalog" {
		t.Fatalf("找不到归属应落第一把可用 Key: %+v", got)
	}
	// 第一把 Key 没密钥时落「第一把有密钥的」
	p2 := ProviderCfg{ID: "c2", BaseURL: "https://x/v1", Keys: []ProviderKeyCfg{
		{ID: "a", Models: []ModelCfg{{ID: "m"}}},
		{ID: "b", APIKey: "sk-b", Models: []ModelCfg{{ID: "m"}}},
	}}
	if k := p2.resolveKey("", "m"); k.ID != "a" {
		t.Fatalf("按模型名找回时第一把命中就够（确定性）: %#v", k)
	}
	if k := p2.resolveKey("", ""); k.ID != "b" {
		t.Fatalf("无模型可依据时应落第一把有密钥的: %#v", k)
	}
	// 默认目标走第一把 Key 的第一个模型
	if got := a.DefaultTarget(); got.APIKey != "sk-daily-1111" || got.Model != "shared" {
		t.Fatalf("默认目标不对: %+v", got)
	}
}

// 协议：Key 级覆盖优先；"chat" 是「显式 OpenAI 兼容」的哨兵，要能压过供应商级
// 的 anthropic（空串才是「跟随供应商」）。
func TestKeyProtocolOverride(t *testing.T) {
	p := ProviderCfg{ID: "c", Protocol: "anthropic", Keys: []ProviderKeyCfg{
		{ID: "inherit"},
		{ID: "chat", Protocol: "chat"},
		{ID: "resp", Protocol: "responses"},
	}}
	cases := map[string]string{"inherit": "anthropic", "chat": "", "resp": "responses"}
	for id, want := range cases {
		if got := p.keyProtocol(p.resolveKey(id, "")); got != want {
			t.Errorf("Key %s 生效协议 = %q，期望 %q", id, got, want)
		}
	}
	// 通道校验白名单接受哨兵值（Key 级），供应商级仍只认三种
	if !providerProtocolOK("chat") || providerProtocolOK("openai") {
		t.Fatal("协议白名单不对")
	}
}

// 拉取模型按 Key 走：分组 A 的 Key 走 Bearer + /chat 兼容端点，分组 B 的 Key
// 走 x-api-key（协议覆盖成 anthropic）——中转站不同分组能看到的模型不一样。
func TestFetchModelsUsesKeyAuthAndProtocol(t *testing.T) {
	type hit struct {
		path, bearer, xkey string
	}
	var hits []hit
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, hit{path: r.URL.Path, bearer: r.Header.Get("Authorization"), xkey: r.Header.Get("x-api-key")})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"m1"}]}`))
	}))
	defer srv.Close()

	p := multiKeyProvider()
	p.BaseURL = srv.URL
	a := &App{Cfg: &Config{Providers: []ProviderCfg{p}}}
	if _, err := a.FetchModels(a.providerByID("custom-1"), "k1"); err != nil {
		t.Fatalf("拉取第一把 Key 失败: %v", err)
	}
	if _, err := a.FetchModels(a.providerByID("custom-1"), "k2"); err != nil {
		t.Fatalf("拉取第二把 Key 失败: %v", err)
	}
	if len(hits) != 2 {
		t.Fatalf("应各拉一次，实际 %d 次", len(hits))
	}
	if hits[0].bearer != "Bearer sk-daily-1111" || hits[0].xkey != "" {
		t.Fatalf("第一把 Key 应走 Bearer: %#v", hits[0])
	}
	if hits[1].xkey != "sk-claude-2222" || hits[1].bearer != "" {
		t.Fatalf("第二把 Key（anthropic 覆盖）应走 x-api-key: %#v", hits[1])
	}
	if hits[0].path != "/models" {
		t.Fatalf("拉取路径不对: %q", hits[0].path)
	}
}

// /api/models 拍平：同一供应商分组下平铺所有 Key 的模型，条目带 Key 归属与
// 分组备注名（重名模型的前端后缀就取它）；单 Key 供应商的归属为空。
func TestHandleModelsFlattensKeys(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.Providers = []ProviderCfg{multiKeyProvider(),
		{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1",
			APIKey: "sk-preset-1234", Models: []ModelCfg{{ID: "deepseek-flash"}}}}
	rec := httptest.NewRecorder()
	a.handleModels(rec, httptest.NewRequest("GET", "/api/models", nil))
	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Models []struct {
			ProviderID, ModelID, KeyID, KeyLabel string
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(out.Models) != 5 { // 中转站 4（含重名的 shared 两行）+ 预置 1
		t.Fatalf("模型条数 %d，期望 5: %s", len(out.Models), rec.Body.String())
	}
	shared := []string{}
	labels := map[string]string{}
	for _, m := range out.Models {
		if m.ProviderID != "custom-1" {
			if m.KeyID != "" || m.KeyLabel != "" {
				t.Errorf("单 Key 供应商不该带 Key 归属: %#v", m)
			}
			continue
		}
		labels[m.KeyID] = m.KeyLabel
		if m.ModelID == "shared" {
			shared = append(shared, m.KeyID)
		}
	}
	if len(shared) != 2 || shared[0] == shared[1] {
		t.Fatalf("重名模型应各占一行、归属不同: %v", shared)
	}
	if labels["k1"] != "默认分组" || labels["k2"] != "Claude 分组" {
		t.Fatalf("Key 备注名未下发: %#v", labels)
	}
	// 绝不下发密钥
	if body := rec.Body.String(); strings.Contains(body, "sk-daily-1111") || strings.Contains(body, "sk-claude-2222") {
		t.Fatalf("/api/models 泄漏了密钥: %s", body)
	}
}

// /api/config 的每把 Key 都只回掩码（口径与供应商级 apiKeyMasked 一致），
// 模型清单与分组备注名这类非敏感信息照常下发。
func TestHandleGetConfigMasksEveryKey(t *testing.T) {
	a := newCompactTestApp(t)
	a.Cfg.Providers = []ProviderCfg{multiKeyProvider()}
	rec := httptest.NewRecorder()
	a.handleGetConfig(rec, httptest.NewRequest("GET", "/api/config", nil))
	if rec.Code != 200 {
		t.Fatalf("状态码 %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, secret := range []string{"sk-daily-1111", "sk-claude-2222"} {
		if strings.Contains(body, secret) {
			t.Fatalf("/api/config 泄漏了密钥 %s: %s", secret, body)
		}
	}
	var out struct {
		Providers []struct {
			ID           string `json:"id"`
			APIKeyMasked string `json:"apiKeyMasked"`
			Keys         []struct {
				ID           string     `json:"id"`
				Label        string     `json:"label"`
				Protocol     string     `json:"protocol"`
				APIKeyMasked string     `json:"apiKeyMasked"`
				Models       []ModelCfg `json:"models"`
			} `json:"keys"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("响应不是合法 JSON: %v", err)
	}
	if len(out.Providers) != 1 || len(out.Providers[0].Keys) != 2 {
		t.Fatalf("Keys 未下发: %s", body)
	}
	if got := out.Providers[0].APIKeyMasked; got != "sk-d****1111" {
		t.Fatalf("供应商级掩码口径变了: %q", got)
	}
	k2 := out.Providers[0].Keys[1]
	if k2.ID != "k2" || k2.Label != "Claude 分组" || k2.Protocol != "anthropic" || k2.APIKeyMasked != "sk-c****2222" ||
		len(k2.Models) != 2 {
		t.Fatalf("第二把 Key 的下发内容不对: %#v", k2)
	}
}

// 保存渠道（upsert）：带 keys[] 就按多 Key 存（新 Key 分到稳定归属 id、密钥
// 留空沿用同 id 已保存的那把、空壳 Key 丢掉）；不带 keys 的普通保存不能把已有
// 的多 Key 冲成单 Key。
func TestUpsertProviderKeepsMultiKeys(t *testing.T) {
	dir := t.TempDir()
	a := &App{Cfg: &Config{DataDir: dir, Providers: []ProviderCfg{multiKeyProvider()}}}
	a.CfgPath = filepath.Join(dir, "config.json")

	post := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		a.handleUpsertProvider(rec, httptest.NewRequest("POST", "/api/providers", strings.NewReader(body)))
		return rec.Code, rec.Body.String()
	}

	// 1) 老页面式保存（只有 apiKey/models）：多 Key 结构原样保留
	if code, body := post(`{"id":"custom-1","name":"中转站","baseURL":"https://relay.example.com/v1","apiKey":"","models":[]}`); code != 200 {
		t.Fatalf("保存失败 %d: %s", code, body)
	}
	if p := a.providerByID("custom-1"); p == nil || len(p.Keys) != 2 {
		t.Fatalf("多 Key 被普通保存冲掉了: %#v", p)
	}

	// 2) 带 keys：改备注名、加一把新 Key（无 id、无密钥）、删掉一把
	body := `{"id":"custom-1","name":"中转站","baseURL":"https://relay.example.com/v1",
	  "keys":[
	    {"id":"k1","label":"日常分组","protocol":"","apiKey":"","models":[{"id":"shared"}]},
	    {"label":"备用分组","protocol":"chat","apiKey":"sk-spare-3333","models":[{"id":"shared"},{"id":"extra"}]},
	    {"label":"","protocol":"","apiKey":"","models":[]}
	  ]}`
	if code, msg := post(body); code != 200 {
		t.Fatalf("保存失败 %d: %s", code, msg)
	}
	p := a.providerByID("custom-1")
	if len(p.Keys) != 2 {
		t.Fatalf("空壳 Key 应被丢掉，实际 %d 把: %#v", len(p.Keys), p.Keys)
	}
	if p.Keys[0].APIKey != "sk-daily-1111" {
		t.Fatalf("留空应沿用已保存的密钥: %#v", p.Keys[0])
	}
	if p.Keys[0].Label != "日常分组" {
		t.Fatalf("备注名没存上: %#v", p.Keys[0])
	}
	if p.Keys[1].ID == "" || p.Keys[1].ID == "k2" {
		t.Fatalf("新 Key 应分到新的归属 id: %#v", p.Keys[1])
	}
	if p.Keys[1].Protocol != "chat" || p.Keys[1].APIKey != "sk-spare-3333" {
		t.Fatalf("新 Key 的协议/密钥没存上: %#v", p.Keys[1])
	}
	// 供应商级镜像是第一把 Key（老版本回退可读）
	if p.APIKey != "sk-daily-1111" {
		t.Fatalf("供应商级镜像应为第一把 Key: %#v", p)
	}

	// 3) 未知协议被拒
	if code, _ := post(`{"id":"custom-1","name":"中转站","baseURL":"https://x/v1","keys":[{"label":"x","protocol":"openai","apiKey":"k"}]}`); code != 400 {
		t.Fatalf("未知 Key 协议应 400，得到 %d", code)
	}
}

// 会话存得住「模型归属哪把 Key」：多 Key 重名模型要靠它还原该用哪把密钥。
func TestSessionProviderKeyPersisted(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Title: "t", Mode: "task", CreatedAt: now(), UpdatedAt: now(),
		Provider: "custom-1", ProviderKey: "k2", Model: "shared"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	got, err := a.Store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if got.ProviderKey != "k2" || got.Model != "shared" {
		t.Fatalf("Key 归属没存住: %#v", got)
	}
	// 改选另一个分组：PATCH 只换 Key 归属（模型名不变）
	got.ProviderKey = "k1"
	if err := a.Store.SaveMeta(got); err != nil {
		t.Fatal(err)
	}
	again, err := a.Store.Get("s1")
	if err != nil {
		t.Fatal(err)
	}
	if again.ProviderKey != "k1" {
		t.Fatalf("Key 归并未更新: %#v", again)
	}
}
