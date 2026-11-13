package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// applyAnnounce 差异应用：公司名、模型清单（服务端元数据优先+兜底猜测）、
// 市场/更新/服务端地址；已选模型被下线时回落默认；空模型清单不动模型。
func TestSyncApplyAnnounce(t *testing.T) {
	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "旧公司"
	a.Cfg.WizardServer = "http://old:PORT"
	a.Cfg.MarketURL = "http://old:PORT/market/manifest.json"
	a.Cfg.UpdateURL = "http://old:PORT/updates/latest"
	a.Cfg.Model = "removed-model"
	a.Cfg.DefaultModel = ModelRef{Provider: wizardProviderID, Model: "removed-model"}
	a.Cfg.Providers = []ProviderCfg{
		{ID: wizardProviderID, Name: "旧公司·内部通道", BaseURL: "http://old:3000/v1", APIKey: "sk-x",
			Models: []ModelCfg{{ID: "removed-model"}}},
	}

	ann := &announcePayload{
		Jxai: 1, Company: "新公司", Server: "http://new:PORT", Rev: "abc123def456",
		MarketURL: "http://new:PORT/market/manifest.json", UpdateURL: "http://new:PORT/updates/latest",
		PublicServer: "https://example.com:PORT",
		API: &struct {
			BaseURL       string            `json:"baseURL"`
			PublicBaseURL string            `json:"publicBaseURL,omitempty"`
			Models        []wizardModelInfo `json:"models"`
			DefaultModel  string            `json:"defaultModel"`
		}{BaseURL: "http://new:3000/v1", PublicBaseURL: "https://example.com:PORT/v1", Models: []wizardModelInfo{{ID: "deepseek-flash"}}, DefaultModel: "deepseek-flash"},
	}
	applyAnnounce(a, ann, "http://new:PORT", false)

	p := a.Cfg.Providers[0]
	if a.Cfg.WizardCompany != "新公司" || p.Name != "新公司·内部通道" {
		t.Fatalf("公司名未跟随: %q %q", a.Cfg.WizardCompany, p.Name)
	}
	if p.BaseURL != "http://new:3000/v1" || len(p.Models) != 1 || p.Models[0].ID != "deepseek-flash" ||
		p.Models[0].ContextWindow != 1048576 || !p.Models[0].SupportsImages {
		t.Fatalf("模型清单未应用: %#v", p.Models)
	}
	if p.PublicBaseURL != "https://example.com:PORT/v1" || a.Cfg.WizardPublicServer != "https://example.com:PORT" {
		t.Fatalf("外网通道/公网服务端未应用: %q %q", p.PublicBaseURL, a.Cfg.WizardPublicServer)
	}
	if p.APIKey != "sk-x" {
		t.Fatalf("key 不应被动: %q", p.APIKey)
	}
	// 已选模型被下线：不再自动改成该渠道第一个模型——用户的选择原样保留，
	// 由 checkModelRef 判定「不可用」在界面上红字提示重选。（Cfg.Model 只是老版本
	// exe 回退的镜像，随渠道清单走，不代表用户的选择。）
	if dm := a.Cfg.DefaultModel; dm.Provider != wizardProviderID || dm.Model != "removed-model" {
		t.Fatalf("全局默认模型应保留用户选择（只标记不可用，不换成清单里第一个）: %+v", dm)
	}
	if a.Cfg.MarketURL != ann.MarketURL || a.Cfg.UpdateURL != ann.UpdateURL || a.Cfg.WizardServer != "http://new:PORT" {
		t.Fatalf("地址未跟随: %v %v %v", a.Cfg.MarketURL, a.Cfg.UpdateURL, a.Cfg.WizardServer)
	}

	// 空模型清单（服务端拉取失败的防御）：模型不被清空
	annEmpty := *ann
	annEmpty.API = &struct {
		BaseURL       string            `json:"baseURL"`
		PublicBaseURL string            `json:"publicBaseURL,omitempty"`
		Models        []wizardModelInfo `json:"models"`
		DefaultModel  string            `json:"defaultModel"`
	}{BaseURL: "http://new:3000/v1"}
	applyAnnounce(a, &annEmpty, "http://new:PORT", false)
	if len(a.Cfg.Providers[0].Models) != 1 {
		t.Fatalf("空清单不应清空模型: %#v", a.Cfg.Providers[0].Models)
	}
}

// 公网兜底：内网失联时改拉公网公告——公司通道的 baseURL/内网服务端与市场地址
// 不迁移，只更新模型清单与外网通道；公网可达不算失联（不触发自动停用）。
func TestSyncPublicFallback(t *testing.T) {
	pub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/announce" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"jxai":1,"company":"测试公司","server":"http://lan:PORT",
			"publicServer":"` + "http://" + r.Host + `",
			"api":{"baseURL":"http://lan:3000/v1","publicBaseURL":"http://` + r.Host + `/v1",
			       "models":[{"id":"deepseek-flash"}],"defaultModel":"deepseek-flash"}}`))
	}))
	t.Cleanup(pub.Close)

	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "测试公司"
	a.Cfg.WizardServer = "http://dead-lan:PORT"
	a.Cfg.WizardPublicServer = pub.URL
	a.Cfg.Providers = []ProviderCfg{{ID: wizardProviderID, Name: "测试公司·内部通道",
		BaseURL: "http://lan:3000/v1", APIKey: "sk-x", Models: []ModelCfg{{ID: "old-m"}}}}

	companySyncOnce(a, "")

	p := a.Cfg.Providers[0]
	if a.Cfg.WizardServer != "http://dead-lan:PORT" {
		t.Fatalf("公网兜底不应迁移内网服务端地址: %q", a.Cfg.WizardServer)
	}
	if p.BaseURL != "http://lan:3000/v1" {
		t.Fatalf("公网公告不应覆盖内网 baseURL: %q", p.BaseURL)
	}
	if p.PublicBaseURL != pub.URL+"/v1" {
		t.Fatalf("外网通道未应用: %q", p.PublicBaseURL)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "deepseek-flash" {
		t.Fatalf("模型清单未应用: %#v", p.Models)
	}
	if p.AutoDisabled {
		t.Fatalf("公网可达不应自动停用")
	}
}

// 自动停用与恢复：全源失联连续 2 轮自动停用（第 1 轮不动）；恢复可达自动启用；
// 手动停用（Enabled=false）不被自动恢复打开。
func TestSyncAutoDisableAndRecover(t *testing.T) {
	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "测试公司"
	a.Cfg.WizardServer = "http://dead-lan:PORT"
	a.Cfg.Providers = []ProviderCfg{{ID: wizardProviderID, Name: "测试公司·内部通道",
		BaseURL: "http://lan:3000/v1", Models: []ModelCfg{{ID: "m-1"}}}}
	oldDiscover := udpDiscoverFn
	udpDiscoverFn = func(string) []wizardServer { return nil }
	t.Cleanup(func() { udpDiscoverFn = oldDiscover })

	companySyncOnce(a, "")
	if a.Cfg.Providers[0].AutoDisabled {
		t.Fatalf("第 1 轮失联不应立即停用")
	}
	companySyncOnce(a, "")
	if !a.Cfg.Providers[0].AutoDisabled {
		t.Fatalf("连续 2 轮失联应自动停用")
	}

	// 恢复可达（内网候选复活）：自动重新启用
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"jxai":1,"company":"测试公司","api":{"baseURL":"http://lan:3000/v1",
			"models":[{"id":"m-1"}],"defaultModel":"m-1"}}`))
	}))
	t.Cleanup(ok.Close)
	a.Cfg.WizardServer = ok.URL
	companySyncOnce(a, "")
	if a.Cfg.Providers[0].AutoDisabled {
		t.Fatalf("恢复可达应自动启用")
	}

	// 手动停用不被自动恢复打开
	f := false
	a.Cfg.Providers[0].Enabled = &f
	noteCompanySyncOK(a)
	if a.Cfg.Providers[0].IsSelectable() {
		t.Fatalf("手动停用不应被自动恢复打开")
	}
}

// 公司通道被删后自动重建：公告可达 + 本地没有 company-internal → 按原姓名走
// /provision 重领配置包，通道置顶重建（含 key 与模型清单）。
func TestSyncRebuildDeletedProvider(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/announce":
			_, _ = w.Write([]byte(`{"jxai":1,"company":"测试公司","api":{"baseURL":"http://127.0.0.1:3000/v1",
				"models":[{"id":"deepseek-flash"}],"defaultModel":"deepseek-flash"}}`))
		case "/provision":
			_, _ = w.Write([]byte(`{"company":"测试公司","user":"张三","server":"http://x",
				"publicServer":"https://example.com:PORT",
				"api":{"baseURL":"http://127.0.0.1:3000/v1","publicBaseURL":"https://example.com:PORT/v1",
					"key":"sk-new","models":[{"id":"deepseek-flash"}],"defaultModel":"deepseek-flash"},
				"marketURL":"http://x/market/manifest.json","updateURL":"http://x/updates/latest"}`))
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)

	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "测试公司"
	a.Cfg.WizardServer = srv.URL
	a.Cfg.PublisherName = "张三"

	companySyncOnce(a, "")

	p := wizardProvider(a)
	if p == nil {
		t.Fatalf("公司通道应被自动重建")
	}
	if p.APIKey != "sk-new" || p.BaseURL != "http://127.0.0.1:3000/v1" {
		t.Fatalf("重建通道应带 key 与 baseURL: %#v", p)
	}
	if p.PublicBaseURL != "https://example.com:PORT/v1" || a.Cfg.WizardPublicServer != "https://example.com:PORT" {
		t.Fatalf("外网信息应随重建应用: %q %q", p.PublicBaseURL, a.Cfg.WizardPublicServer)
	}
	if len(p.Models) != 1 || p.Models[0].ID != "deepseek-flash" {
		t.Fatalf("模型清单未应用: %#v", p.Models)
	}
	if &a.Cfg.Providers[0] != p {
		t.Fatalf("重建通道应置顶: %#v", a.Cfg.Providers)
	}
}

// 失联迁移：当前服务端不可达 → 按公司名重新发现新地址 → 拉公告应用，
// 市场/更新地址随服务端地址整体迁移。
func TestSyncMigrateOnFailover(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close() // 当前服务端已下线

	newSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/announce" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"jxai":1,"company":"测试公司","server":"` + "http://" + r.Host + `",
			"rev":"f1e2d3c4b5a6","marketURL":"http://` + r.Host + `/market/manifest.json",
			"updateURL":"http://` + r.Host + `/updates/latest",
			"api":{"baseURL":"http://127.0.0.1:3000/v1","models":[{"id":"deepseek-flash","contextWindow":1048576}],
			       "defaultModel":"deepseek-flash"}}`))
	}))
	t.Cleanup(newSrv.Close)

	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "测试公司"
	a.Cfg.WizardServer = deadURL
	a.Cfg.MarketURL = deadURL + "/market/manifest.json"
	a.Cfg.UpdateURL = deadURL + "/updates/latest"
	a.Cfg.Providers = []ProviderCfg{{ID: wizardProviderID, Name: "测试公司·内部通道",
		BaseURL: "http://old:3000/v1", APIKey: "sk-x", Models: []ModelCfg{{ID: "old-m"}}}}

	// 发现函数打桩：按公司名发现到新服务端
	oldDiscover := udpDiscoverFn
	udpDiscoverFn = func(company string) []wizardServer {
		if company != "测试公司" {
			return nil
		}
		return []wizardServer{{Server: newSrv.URL, Company: company}}
	}
	t.Cleanup(func() { udpDiscoverFn = oldDiscover })

	companySyncOnce(a, "")

	if a.Cfg.WizardServer != newSrv.URL {
		t.Fatalf("服务端地址应迁移到新发现地址: %q", a.Cfg.WizardServer)
	}
	if !strings.HasPrefix(a.Cfg.MarketURL, newSrv.URL+"/") || !strings.HasPrefix(a.Cfg.UpdateURL, newSrv.URL+"/") {
		t.Fatalf("市场/更新地址应随服务端迁移: %v %v", a.Cfg.MarketURL, a.Cfg.UpdateURL)
	}
	if ms := a.Cfg.Providers[0].Models; len(ms) != 1 || ms[0].ID != "deepseek-flash" {
		t.Fatalf("模型清单未应用: %#v", ms)
	}
}
