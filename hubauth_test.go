package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// 注册凭据派生：确定性、与中继端到端密钥（relayKey=SHA256(PairKey)）域独立
// ——网关侧拿到 auth 也推不出中继密钥。
func TestHubAuthKeyDerivation(t *testing.T) {
	a1, a2 := hubAuthKey("pk-甲"), hubAuthKey("pk-甲")
	if a1 != a2 || len(a1) != 64 {
		t.Fatalf("同 PairKey 应派生相同凭据: %q vs %q", a1, a2)
	}
	if hubAuthKey("pk-乙") == a1 {
		t.Fatal("不同 PairKey 应派生不同凭据")
	}
	relay := sha256.Sum256([]byte("pk-甲"))
	if a1 == hex.EncodeToString(relay[:]) {
		t.Fatal("auth 凭据不得与 relayKey 同值（派生域必须不同）")
	}
}

// 总服务器 lookup：server 完整地址字段优先；老服务端无该字段退回拼 addrs；
// "off" 不发查询。
func TestWizardRegistryLookupServerField(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("company") == "新式公司" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"company": "新式公司", "server": "https://jxaizz.com:2227",
				"addrs": []string{"192.168.0.210"}, "port": 2227,
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"company": "老式公司", "addrs": []string{"192.168.1.10", "192.168.1.11"}, "port": 2228,
		})
	}))
	t.Cleanup(srv.Close)

	got, err := wizardRegistryLookup(srv.URL, "新式公司")
	if err != nil || len(got) != 1 || got[0].Server != "https://jxaizz.com:2227" {
		t.Fatalf("server 字段应优先: %v %v", got, err)
	}
	got2, err := wizardRegistryLookup(srv.URL, "老式公司")
	if err != nil || len(got2) != 2 || got2[0].Server != "https://192.168.1.10:2228" {
		t.Fatalf("无 server 字段应按 addrs 拼 https: %v %v", got2, err)
	}
	// off：不查询（服务器关了也不报错）
	got3, err := wizardRegistryLookup("off", "任意公司")
	if err != nil || got3 != nil {
		t.Fatalf("off 应直接返回空: %v %v", got3, err)
	}
}

// RegistryURL 默认值：老配置空键补内置总服务器；显式 "off" 不被覆盖。
func TestLoadConfigRegistryURLDefault(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/config.json"
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RegistryURL != "https://jxaizz.com:2227" {
		t.Fatalf("空键应补默认总服务器，得到 %q", cfg.RegistryURL)
	}
	// 显式 off 保留
	cfg.RegistryURL = "off"
	if err := cfg.Save(path); err != nil {
		t.Fatal(err)
	}
	cfg2, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg2.RegistryURL != "off" {
		t.Fatalf("显式 off 不应被默认覆盖，得到 %q", cfg2.RegistryURL)
	}
}
