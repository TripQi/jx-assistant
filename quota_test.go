package main

// 公司通道额度显示（服务端随公告/provision 下发的 quota 快照）：
//   - applyAnnounce / applyProvisionPayload 把它存进 Cfg.CompanyQuota（有变化才落盘）；
//   - /api/models 带 companyQuota（模型菜单顶部显示）；
//   - /api/config 的公司通道卡片带 quota；/api/company-sync 复用公司同步器刷新。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func sampleQuota() *CompanyQuotaCfg {
	return &CompanyQuotaCfg{
		Points: 100000, RemainPoints: 72340, UsedPoints: 27660,
		Period: "monthly", PeriodText: "每月 1 号", NextAt: "2026-11-01T00:00:00+08:00",
		AsOf: "2026-10-26T13:20:00+08:00",
	}
}

// 公告里带 quota → 存进配置；同样的快照再来一轮不重复写盘（不产生"额度"变更）。
func TestSyncAppliesQuota(t *testing.T) {
	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "测试公司"
	a.Cfg.WizardServer = "http://lan:2228"
	a.Cfg.Providers = []ProviderCfg{{ID: wizardProviderID, Name: "测试公司·内部通道",
		BaseURL: "http://lan:3000/v1", APIKey: "sk-x", Models: []ModelCfg{{ID: "m1"}}}}

	ann := &announcePayload{Jxai: 1, Company: "测试公司", Server: "http://lan:2228",
		MarketURL: "http://lan:2228/market/manifest.json", UpdateURL: "http://lan:2228/updates/latest",
		Quota: sampleQuota(),
		API: &struct {
			BaseURL       string            `json:"baseURL"`
			PublicBaseURL string            `json:"publicBaseURL,omitempty"`
			Models        []wizardModelInfo `json:"models"`
			DefaultModel  string            `json:"defaultModel"`
		}{BaseURL: "http://lan:3000/v1", Models: []wizardModelInfo{{ID: "m1"}}, DefaultModel: "m1"},
	}
	applyAnnounce(a, ann, "http://lan:2228", false)
	q := a.Cfg.CompanyQuota
	if q == nil || q.RemainPoints != 72340 || q.PeriodText != "每月 1 号" || q.Points != 100000 {
		t.Fatalf("额度快照未应用: %#v", q)
	}

	// 服务端没下发 quota（老服务端）：保留旧值，不清空
	ann2 := *ann
	ann2.Quota = nil
	applyAnnounce(a, &ann2, "http://lan:2228", false)
	if a.Cfg.CompanyQuota == nil || a.Cfg.CompanyQuota.RemainPoints != 72340 {
		t.Fatal("没有新快照时不该清空旧额度")
	}
}

// 配置包（provision）也带 quota：装完向导就有额度可显示。
func TestProvisionPayloadAppliesQuota(t *testing.T) {
	a := newWizardTestApp(t)
	payload := &provisionPayload{
		Company: "测试公司", User: "张三", Server: "http://lan:2228",
		MarketURL: "http://lan:2228/market/manifest.json", UpdateURL: "http://lan:2228/updates/latest",
		Quota: sampleQuota(),
	}
	payload.API = &struct {
		BaseURL            string            `json:"baseURL"`
		PublicBaseURL      string            `json:"publicBaseURL,omitempty"`
		Key                string            `json:"key"`
		Models             []wizardModelInfo `json:"models"`
		DefaultModel       string            `json:"defaultModel"`
		MonthlyQuotaPoints int64             `json:"monthlyQuotaPoints,omitempty"`
	}{BaseURL: "http://lan:3000/v1", Key: "sk-x", Models: []wizardModelInfo{{ID: "m1"}}, DefaultModel: "m1", MonthlyQuotaPoints: 100000}

	models := a.applyProvisionPayload(payload)
	if len(models) != 1 {
		t.Fatalf("模型未应用: %#v", models)
	}
	if a.Cfg.CompanyQuota == nil || a.Cfg.CompanyQuota.RemainPoints != 72340 {
		t.Fatalf("额度快照未应用: %#v", a.Cfg.CompanyQuota)
	}
}

// /api/models 与 /api/config 都把额度带给前端（模型菜单 + 设置页公司通道卡片）。
func TestModelsAndConfigExposeQuota(t *testing.T) {
	a := newWizardTestApp(t)
	a.Cfg.CompanyQuota = sampleQuota()
	a.Cfg.Providers = []ProviderCfg{
		{ID: wizardProviderID, Name: "测试公司·内部通道", BaseURL: "http://lan:3000/v1", APIKey: "sk-x",
			Models: []ModelCfg{{ID: "m1"}}},
		{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", APIKey: "sk-y",
			Models: []ModelCfg{{ID: "deepseek-flash"}}},
	}

	// /api/models → companyQuota
	rec := httptest.NewRecorder()
	a.handleModels(rec, httptest.NewRequest("GET", "/api/models", nil))
	var m struct {
		Models       []map[string]any `json:"models"`
		CompanyQuota *CompanyQuotaCfg `json:"companyQuota"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	if m.CompanyQuota == nil || m.CompanyQuota.RemainPoints != 72340 {
		t.Fatalf("/api/models 应带 companyQuota: %s", rec.Body.String())
	}

	// /api/config → 公司通道 provider 的 quota（私人通道不带）
	rec2 := httptest.NewRecorder()
	a.handleGetConfig(rec2, httptest.NewRequest("GET", "/api/config", nil))
	var c struct {
		Providers []struct {
			ID      string           `json:"id"`
			Company bool             `json:"company"`
			Quota   *CompanyQuotaCfg `json:"quota"`
		} `json:"providers"`
	}
	if err := json.Unmarshal(rec2.Body.Bytes(), &c); err != nil {
		t.Fatalf("响应不是 JSON: %v", err)
	}
	var company, other *CompanyQuotaCfg
	for _, p := range c.Providers {
		if p.Company {
			company = p.Quota
		} else if p.ID == "deepseek" {
			other = p.Quota
		}
	}
	if company == nil || company.UsedPoints != 27660 {
		t.Fatalf("公司通道应带额度: %s", rec2.Body.String())
	}
	if other != nil {
		t.Fatal("私人通道不该带公司额度")
	}
}

// /api/company-sync 复用公司同步器：拉一轮公告后把新额度写进配置并回给前端。
func TestCompanySyncEndpoint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/announce" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte(`{"jxai":1,"company":"测试公司","server":"` + srvURL(r) + `",
			"marketURL":"` + srvURL(r) + `/market/manifest.json","updateURL":"` + srvURL(r) + `/updates/latest",
			"quota":{"points":5000,"remainPoints":4321,"period":"never","periodText":"不自动重置","usedPoints":679},
			"api":{"baseURL":"` + srvURL(r) + `/v1","models":[{"id":"m1"}],"defaultModel":"m1"}}`))
	}))
	t.Cleanup(srv.Close)

	a := newWizardTestApp(t)
	a.Cfg.WizardCompany = "测试公司"
	a.Cfg.WizardServer = srv.URL
	a.Cfg.Providers = []ProviderCfg{{ID: wizardProviderID, Name: "测试公司·内部通道",
		BaseURL: srv.URL + "/v1", APIKey: "sk-x", Models: []ModelCfg{{ID: "m1"}}}}

	rec := httptest.NewRecorder()
	a.handleCompanySyncNow(rec, httptest.NewRequest("POST", "/api/company-sync", strings.NewReader("")))
	if rec.Code != 200 {
		t.Fatalf("刷新应 200，得到 %d：%s", rec.Code, rec.Body.String())
	}
	var out struct {
		OK    bool             `json:"ok"`
		Quota *CompanyQuotaCfg `json:"quota"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if !out.OK || out.Quota == nil || out.Quota.RemainPoints != 4321 {
		t.Fatalf("刷新后应返回新额度: %s", rec.Body.String())
	}
	if a.Cfg.CompanyQuota == nil || a.Cfg.CompanyQuota.PeriodText != "不自动重置" {
		t.Fatalf("额度未落盘: %#v", a.Cfg.CompanyQuota)
	}

	// 没接入公司：明确报错
	b := newWizardTestApp(t)
	rec2 := httptest.NewRecorder()
	b.handleCompanySyncNow(rec2, httptest.NewRequest("POST", "/api/company-sync", strings.NewReader("")))
	if rec2.Code != 400 {
		t.Fatalf("未接入应 400，得到 %d", rec2.Code)
	}
}

// srvURL 当前请求的原始地址（测试里拼公告用；sync_resync_test.go 里已有同名同实现）
