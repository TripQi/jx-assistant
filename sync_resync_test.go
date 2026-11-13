package main

// 「一次性重新接入」（sync.go companyResyncOnce）的测试：把 10-16 之后没再跑过
// 向导、模型清单停在旧配置的机器追平。三级递降都要覆盖：
//   ① /provision 按人重领 → 模型/key/市场/更新全部追平
//   ② /provision 失败 → 退到 /announce 只追模型清单（不动 key）
//   ③ 全失败 → 弹一次带原有配置的向导（只弹一次，且不写完成标记）

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// resyncTestApp 造一个「已接入公司」的 App（配置落临时目录）。
func resyncTestApp(t *testing.T, providers ...ProviderCfg) *App {
	t.Helper()
	dir := t.TempDir()
	a := &App{
		Cfg: &Config{
			WizardDone:    true,
			WizardCompany: "深圳坚祥科技有限公司",
			WizardServer:  "http://old-server",
			PublisherName: "张三",
			Providers:     providers,
		},
		CfgPath:     filepath.Join(dir, "config.json"),
		subscribers: map[chan Event]bool{},
	}
	return a
}

// resyncChan 订阅事件流；调用 drain() 把已发生的事件一次性取走（broadcast 是
// 同步投递到带缓冲的订阅通道，所以调用同步的 companyResyncOnce 之后 drain 必能取到）。
func resyncChan(a *App) func() []Event {
	ch, cancel := a.subscribe()
	return func() []Event {
		out := []Event{}
		for {
			select {
			case ev := <-ch:
				if ev.Type == "company_resync" {
					out = append(out, ev)
				}
			default:
				cancel()
				return out
			}
		}
	}
}

func companyProvider(models ...string) ProviderCfg {
	p := ProviderCfg{ID: wizardProviderID, Name: "旧·内部通道", BaseURL: "http://old-api", APIKey: "old-key"}
	for _, m := range models {
		p.Models = append(p.Models, ModelCfg{ID: m, Name: m})
	}
	return p
}

// provisionServer 一个既认 /provision 也认 /announce 的假服务端。
func provisionServer(t *testing.T, models []string, announceOK, provisionOK bool) (*httptest.Server, *int32, *int32) {
	t.Helper()
	var pHits, aHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/provision":
			atomic.AddInt32(&pHits, 1)
			if !provisionOK {
				http.Error(w, "not configured", 500)
				return
			}
			list := make([]map[string]any, 0, len(models))
			for _, m := range models {
				list = append(list, map[string]any{"id": m, "name": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"company": "深圳坚祥科技有限公司",
				"api": map[string]any{
					"baseURL": "http://gw:3000", "key": "new-key",
					"models": list, "defaultModel": models[0],
				},
				"marketURL": srvURL(r) + "/market", "updateURL": srvURL(r) + "/updates/latest",
			})
		case "/announce":
			atomic.AddInt32(&aHits, 1)
			if !announceOK {
				http.Error(w, "down", 500)
				return
			}
			list := make([]map[string]any, 0, len(models))
			for _, m := range models {
				list = append(list, map[string]any{"id": m, "name": m})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"jxai": 1, "company": "深圳坚祥科技有限公司", "rev": "rev-1",
				"server": srvURL(r),
				"api":    map[string]any{"baseURL": "http://gw:3000", "models": list, "defaultModel": models[0]},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv, &pHits, &aHits
}

func srvURL(r *http.Request) string { return "http://" + r.Host }

func TestCompanyResyncProvisionPath(t *testing.T) {
	srv, pHits, _ := provisionServer(t, []string{"glm-5.3", "deepseek-v4-pro", "qwen3"}, true, true)
	a := resyncTestApp(t, companyProvider("old-model-1"), ProviderCfg{ID: "anthropic", Name: "私人", BaseURL: "http://x"})
	drain := resyncChan(a)

	a.companyResyncOnce(srv.URL)

	if n := atomic.LoadInt32(pHits); n != 1 {
		t.Fatalf("应当按人领一次 provision，实际 %d 次", n)
	}
	p := wizardProvider(a)
	if p == nil || len(p.Models) != 3 || p.Models[0].ID != "glm-5.3" {
		t.Fatalf("公司通道模型没追平: %+v", p)
	}
	if p.APIKey != "new-key" {
		t.Fatalf("provision 应更新 key，实际 %q", p.APIKey)
	}
	if len(a.Cfg.Providers) != 2 || a.Cfg.Providers[1].ID != "anthropic" {
		t.Fatalf("其它提供方必须保留: %+v", a.Cfg.Providers)
	}
	if a.Cfg.CompanyResync != companyResyncTarget {
		t.Fatalf("完成标记未写: %d", a.Cfg.CompanyResync)
	}
	if a.Cfg.WizardServer != srv.URL {
		t.Fatalf("服务端地址未更新: %q", a.Cfg.WizardServer)
	}
	if _, err := os.Stat(a.CfgPath); err != nil {
		t.Fatalf("配置没落盘: %v", err)
	}
	raw, _ := os.ReadFile(a.CfgPath)
	if !strings.Contains(string(raw), `"companyResync"`) || !strings.Contains(string(raw), "glm-5.3") {
		t.Fatal("落盘配置里应有完成标记与新模型")
	}
	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("应广播一条 company_resync，实际 %d", len(evs))
	}
	if ok, _ := evs[0].Data["ok"].(bool); !ok {
		t.Fatalf("事件应表示成功: %+v", evs[0].Data)
	}
}

func TestCompanyResyncFallsBackToAnnounce(t *testing.T) {
	srv, _, aHits := provisionServer(t, []string{"glm-5.3", "deepseek-v4-pro"}, true, false)
	a := resyncTestApp(t, companyProvider("old-model-1"))
	drain := resyncChan(a)

	a.companyResyncOnce(srv.URL)

	if n := atomic.LoadInt32(aHits); n != 1 {
		t.Fatalf("provision 失败后应改拉一次公告，实际 %d 次", n)
	}
	p := wizardProvider(a)
	if p == nil || len(p.Models) != 2 {
		t.Fatalf("公告也应把模型清单追平: %+v", p)
	}
	if p.APIKey != "old-key" {
		t.Fatalf("公告路不该动 key，实际 %q", p.APIKey)
	}
	if a.Cfg.CompanyResync != companyResyncTarget {
		t.Fatal("公告追平也算完成")
	}
	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("应广播一条成功事件，实际 %d", len(evs))
	}
	if mode, _ := evs[0].Data["mode"].(string); mode != "announce" {
		t.Fatalf("事件应标明 announce 路，实际 %q", mode)
	}
}

func TestCompanyResyncPromptsWizardOnceThenStaysQuiet(t *testing.T) {
	a := resyncTestApp(t, ProviderCfg{ID: "anthropic", Name: "私人", BaseURL: "http://x"}) // 公司通道已丢
	drain := resyncChan(a)

	a.companyResyncOnce("http://127.0.0.1:1") // 必然连不上

	if a.Cfg.CompanyResync != 0 {
		t.Fatal("失败不该写完成标记（下次启动还要再试）")
	}
	if !a.Cfg.CompanyResyncPrompted {
		t.Fatal("失败应记「已提醒」，避免每次启动都打扰")
	}
	evs := drain()
	if len(evs) != 1 {
		t.Fatalf("应广播一条需要向导的事件，实际 %d", len(evs))
	}
	d := evs[0].Data
	if need, _ := d["needWizard"].(bool); !need {
		t.Fatalf("事件应要求弹向导: %+v", d)
	}
	if d["server"] != "http://127.0.0.1:1" || d["user"] != "张三" {
		t.Fatalf("事件里要带上原有配置（服务端/姓名）: %+v", d)
	}

	// 再来一轮：已经提醒过，不再重复播
	drain2 := resyncChan(a)
	a.companyResyncOnce("http://127.0.0.1:1")
	if again := drain2(); len(again) != 0 {
		t.Fatalf("已提醒过就不该再弹，实际 %d 条", len(again))
	}
}

func TestStartCompanyResyncOnceGates(t *testing.T) {
	// ① 已完成 → 不跑
	a := resyncTestApp(t)
	a.Cfg.CompanyResync = companyResyncTarget
	if s := resyncServerBase(a); s == "" {
		t.Fatal("wizardServer 有值时应能取到地址")
	}
	// ② 没接入过公司 → 不跑（WizardDone=false）
	b := resyncTestApp(t)
	b.Cfg.WizardDone = false
	if b.Cfg.CompanyResync != 0 {
		t.Fatal("前置条件不满足时不该写标记")
	}
	// ③ 老配置没有 wizardServer：从更新/市场地址反推 origin
	c := resyncTestApp(t)
	c.Cfg.WizardServer = ""
	c.Cfg.UpdateURL = "http://192.168.0.210:2227/updates/latest"
	if got := resyncServerBase(c); got != "http://192.168.0.210:2227" {
		t.Fatalf("应从更新地址反推服务端 origin，实际 %q", got)
	}
	c.Cfg.UpdateURL = ""
	c.Cfg.MarketURL = "https://jxaizz.com:3000/api/market"
	if got := resyncServerBase(c); got != "https://jxaizz.com:3000" {
		t.Fatalf("应从市场地址反推 origin，实际 %q", got)
	}
	if got := originOfURL("not-a-url"); got != "" {
		t.Fatalf("非法地址应返回空，实际 %q", got)
	}
}
