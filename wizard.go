package main

// 首次接入向导（后端半边）：浏览器发不了 UDP——局域网发现与总服务器查询都在这里做，
// 前端（web/app-wizard.js）只负责表单与展示。
//
//	POST /api/wizard/discover {company}            → UDP 广播（2s，公司名=口令）+
//	  registryURL 查询兜底 → 候选服务端列表（去重，lan 来源优先）
//	POST /api/wizard/connect  {server, user}       → 验证 healthz → POST /provision
//	  → 直接写运行配置：内部提供方（company-internal）插到 providers[0] 即默认
//	  （模型清单 models[0]=公司默认模型）、marketURL/updateURL 一键配好、wizardDone 置位
//
// 重跑向导（设置·网络访问·重新运行）会替换 company-internal 并保留其它提供方；
// 服务端未配模型通道时 api 为空：市场/更新照配，前端给出提示。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	wizardDiscoverTimeout = 2 * time.Second
	wizardProviderID      = "company-internal" // 内部提供方的固定 id（重跑即替换）
)

// wizardUDPProbes 发现探测的约定 UDP 端口：向广播地址的这些端口各发一份探测。
// UDP 包必须指定端口（没有"全端口广播"）——2026-10-25 起服务端 UDP 发现跟随其
// TCP 主口（生产=2227），历史约定是 2228；两个都发，新老服务端与本机测试实例
// （:PORT）都能被发现，应答内容自带服务端实际地址（去重见 udpDiscoverServers）。
var wizardUDPProbes = []int{2227, 2228}

// wizardServer 一个候选服务端。
type wizardServer struct {
	Server  string `json:"server"`         // 如 https://example.com:PORT（发现应答/登记查询给的完整地址）
	Name    string `json:"name,omitempty"` // 服务端市场名
	Company string `json:"company"`        // 应答身份（与探测口令一致）
	Src     string `json:"src"`            // lan=局域网发现 / registry=总服务器查询
}

// provisionPayload /provision 的响应结构（服务端 jxai-server 的 /provision 下发，独立仓库）。
// wizardModelInfo 服务端下发的模型条目（provision 与 announce 公告共用）。
type wizardModelInfo struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	ContextWindow  int    `json:"contextWindow,omitempty"`
	MaxOutput      int    `json:"maxOutput,omitempty"`
	SupportsImages bool   `json:"supportsImages,omitempty"`
}

// buildWizardModels 由服务端模型清单构造 ModelCfg：服务端权威元数据优先，
// 缺省按模型名兜底猜测（guessModelMeta）。向导接入与公告同步（sync.go）共用。
func buildWizardModels(list []wizardModelInfo) []ModelCfg {
	models := make([]ModelCfg, 0, len(list))
	for _, m := range list {
		if m.ID == "" {
			continue
		}
		cw, mo, imgs := m.ContextWindow, m.MaxOutput, m.SupportsImages
		if cw == 0 {
			cw, imgs = guessModelMeta(m.ID)
		}
		models = append(models, ModelCfg{ID: m.ID, Name: m.Name,
			ContextWindow: cw, MaxOutput: mo, SupportsImages: imgs})
	}
	return models
}

type provisionPayload struct {
	Company string `json:"company"`
	User    string `json:"user"`
	Server  string `json:"server"`
	// PublicServer 服务端自己的公网地址（管理页手动配置，空=未启用外网下发）：
	// 存进 WizardPublicServer，出公司后作为公告拉取的兜底来源。
	PublicServer string `json:"publicServer,omitempty"`
	// PortalURL 门户充值入口（2026-11-12）：服务端 portal 启用才下发（=服务端
	// 根地址）。存 Cfg.PortalURL——向导一完成「去充值」入口即可用，不等下一轮
	// 公告（10 分钟）。
	PortalURL string `json:"portalURL,omitempty"`
	API          *struct {
		BaseURL      string `json:"baseURL"`
		PublicBaseURL string `json:"publicBaseURL,omitempty"` // 外网模型通道（被授权才下发）
		Key          string `json:"key"`
		Models       []wizardModelInfo `json:"models"`
		DefaultModel string            `json:"defaultModel"`
		// 每月额度（积分，1 积分 = 0.001 元；仅展示用，扣减在 new-api 侧）
		MonthlyQuotaPoints int64 `json:"monthlyQuotaPoints,omitempty"`
	} `json:"api"`
	APIError  string `json:"apiError,omitempty"`
	MarketURL string `json:"marketURL"`
	UpdateURL string `json:"updateURL"`
	// RegistryURL 总服务器地址（服务端下发；空=不变）
	RegistryURL string `json:"registryURL,omitempty"`
	// RelayHost / RelayPort 远程接入网关（服务端下发；空=不变，且只跟随出厂默认）
	RelayHost string `json:"relayHost,omitempty"`
	RelayPort int    `json:"relayPort,omitempty"`
	// Quota 额度快照（服务端下发；老服务端没有该字段 = 不显示额度）
	Quota *CompanyQuotaCfg `json:"quota,omitempty"`
}

var wizardHTTP = newCompanyHTTPClient(15 * time.Second)

// fetchProvision POST /provision 按人领配置包（向导接入与 sync.go 的公司通道
// 自动重建共用）。任何失败都返回 error，由调用方决定提示方式。
func fetchProvision(server, user string) (provisionPayload, error) {
	var payload provisionPayload
	pd, err := json.Marshal(map[string]string{"user": user})
	if err != nil {
		return payload, err
	}
	resp, err := wizardHTTP.Post(strings.TrimRight(server, "/")+"/provision", "application/json", bytes.NewReader(pd))
	if err != nil {
		return payload, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != 200 {
		return payload, fmt.Errorf("服务端拒绝下发：%s", truncateRunes(string(raw), 300))
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		return payload, fmt.Errorf("配置包格式不对：%w", err)
	}
	return payload, nil
}

// applyProvisionPayload 把配置包应用到运行配置：内部提供方置顶重建（即默认，
// models[0]=公司默认模型），市场/更新一键配好。重跑向导与公司通道被删后的
// 自动重建（sync.go）共用；返回应用的模型目录（未配模型通道时为 nil）。
func (a *App) applyProvisionPayload(payload *provisionPayload) []ModelCfg {
	var models []ModelCfg
	if payload.API != nil && len(payload.API.Models) > 0 {
		models = buildWizardModels(payload.API.Models)
		if dm := payload.API.DefaultModel; dm != "" { // 默认模型置顶（models[0] 即默认）
			for i := range models {
				if models[i].ID == dm {
					models = append([]ModelCfg{models[i]}, append(models[:i:i], models[i+1:]...)...)
					break
				}
			}
		}
		name := payload.Company
		if name == "" {
			name = "公司"
		}
		internal := ProviderCfg{
			ID: wizardProviderID, Name: name + "·内部通道",
			BaseURL: payload.API.BaseURL, APIKey: payload.API.Key, Models: models,
			PublicBaseURL: payload.API.PublicBaseURL,
		}
		providers := []ProviderCfg{internal}
		for _, p := range a.Cfg.Providers { // 重跑/重建：替换旧的内部提供方，保留其它（含各自启停状态）
			if p.ID != wizardProviderID {
				providers = append(providers, p)
			}
		}
		a.Cfg.Providers = providers
	}
	if payload.MarketURL != "" {
		a.Cfg.MarketURL = payload.MarketURL
		a.marketMu.Lock()
		a.marketCache = marketCacheEntry{} // 市场缓存作废
		a.marketMu.Unlock()
	}
	if payload.UpdateURL != "" {
		a.Cfg.UpdateURL = payload.UpdateURL
	}
	if payload.RegistryURL != "" {
		a.Cfg.RegistryURL = strings.TrimRight(payload.RegistryURL, "/")
	}
	if payload.RelayHost != "" && a.Cfg.RemoteAccess.Host == defaultRemoteHost {
		a.Cfg.RemoteAccess.Host = payload.RelayHost
		if payload.RelayPort > 0 {
			a.Cfg.RemoteAccess.Port = payload.RelayPort
		}
	}
	// 额度快照：服务端下发就存下来（空 = 服务端没配额度/没下发，保持原值不动）
	if payload.Quota != nil {
		a.Cfg.CompanyQuota = payload.Quota
	}
	return models
}

/* ---------- 发现 ---------- */

func (a *App) handleWizardDiscover(w http.ResponseWriter, r *http.Request) {
	if a.Cfg.CompanyLinkOff {
		writeErr(w, 400, "公司接入已停用：到 设置 · 网络访问 打开「公司接入」开关后再运行向导")
		return
	}
	var body struct {
		Company string `json:"company"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	company := strings.TrimSpace(body.Company)
	if company == "" || len([]rune(company)) > 64 {
		writeErr(w, 400, "公司名称需为 1~64 字")
		return
	}
	found := []wizardServer{}
	seen := map[string]bool{}
	add := func(s wizardServer) {
		s.Server = strings.TrimRight(s.Server, "/")
		if s.Server == "" || seen[s.Server] {
			return
		}
		seen[s.Server] = true
		found = append(found, s)
	}
	for _, s := range udpDiscoverServers(company) {
		add(s)
	}
	// 总服务器查询兜底（串行：内网广播收满 2s 超时窗口后才问）——外网客户端
	// 的主路径：按公司名查总服务器拿该公司的对外地址（registry server 字段）。
	registryMiss := false
	if len(found) == 0 {
		if reg := strings.TrimSpace(a.Cfg.RegistryURL); reg != "" {
			servers, err := wizardRegistryLookup(reg, company)
			if err == nil {
				for _, s := range servers {
					add(s)
				}
				registryMiss = len(servers) == 0 // 查到了但 404 未登记：给用户准确的文案
			}
		}
	}
	writeJSON(w, 200, map[string]any{"servers": found, "registryMiss": registryMiss})
}

// udpDiscoverServers 向 255.255.255.255 与各接口子网广播地址发探测，收满超时窗口。
func udpDiscoverServers(company string) []wizardServer {
	pc, err := net.ListenPacket("udp4", "0.0.0.0:0")
	if err != nil {
		return nil
	}
	defer pc.Close()
	deadline := time.Now().Add(wizardDiscoverTimeout)
	probe, _ := json.Marshal(map[string]any{"jxai": 1, "company": company})
	for _, t := range broadcastTargets() {
		_, _ = pc.WriteTo(probe, t)
	}
	out := []wizardServer{}
	buf := make([]byte, 1024)
	for {
		remain := time.Until(deadline)
		if remain <= 0 {
			break
		}
		_ = pc.SetReadDeadline(deadline)
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			break // 超时窗口到
		}
		var m struct {
			Jxai    int    `json:"jxai"`
			Company string `json:"company"`
			Server  string `json:"server"`
			Name    string `json:"name"`
		}
		if json.Unmarshal(buf[:n], &m) == nil && m.Jxai == 1 && m.Company == company && m.Server != "" {
			out = append(out, wizardServer{Server: m.Server, Name: m.Name, Company: m.Company, Src: "lan"})
		}
	}
	return out
}

// broadcastTargets 广播目标：255.255.255.255 + 各接口的子网定向广播地址
// （后者在部分路由器/AP 屏蔽全网广播时仍可达），每个地址 × 每个约定端口。
func broadcastTargets() []*net.UDPAddr {
	targets := []*net.UDPAddr{}
	seen := map[string]bool{}
	appendAddr := func(ip string) {
		for _, port := range wizardUDPProbes {
			key := net.JoinHostPort(ip, strconv.Itoa(port))
			if seen[key] {
				continue
			}
			seen[key] = true
			if addr, err := net.ResolveUDPAddr("udp4", key); err == nil {
				targets = append(targets, addr)
			}
		}
	}
	appendAddr("255.255.255.255")
	ifaces, err := net.Interfaces()
	if err != nil {
		return targets
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			ipNet, ok := a.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil || ipNet.IP.IsLoopback() {
				continue
			}
			broadcast := make(net.IP, 4)
			for i := 0; i < 4; i++ {
				broadcast[i] = ipNet.IP[i] | ^ipNet.Mask[i]
			}
			if !broadcast.Equal(ipNet.IP) { // /32 与点对点网段没有广播地址
				appendAddr(broadcast.String())
			}
		}
	}
	return targets
}

// wizardRegistryLookup 按公司名向总服务器查登记的服务端地址（404=未登记，返回空）。
// 响应的 server 完整地址字段优先（外网接入关键：登记的 publicServer 域名地址），
// 老服务端没有该字段时退回按 addrs 拼内网地址。
func wizardRegistryLookup(registryURL, company string) ([]wizardServer, error) {
	if registryURL == "off" {
		return nil, nil // 显式禁用总服务器查询（设置·网络访问）
	}
	u := strings.TrimRight(registryURL, "/") + "/registry/lookup?company=" + url.QueryEscape(company)
	resp, err := wizardHTTP.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("总服务器返回 HTTP %d", resp.StatusCode)
	}
	var rec struct {
		Company string   `json:"company"`
		Server  string   `json:"server"` // 完整地址（新服务端下发；外网接入用）
		Addrs   []string `json:"addrs"`
		Port    int      `json:"port"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rec); err != nil {
		return nil, err
	}
	out := []wizardServer{}
	if s := strings.TrimRight(strings.TrimSpace(rec.Server), "/"); s != "" {
		out = append(out, wizardServer{Server: s, Company: rec.Company, Src: "registry"})
		return out, nil
	}
	for _, addr := range rec.Addrs {
		out = append(out, wizardServer{
			// 服务端业务只走 TLS（2026-10-25 明文停用），登记地址按 https 拼
			Server:  fmt.Sprintf("https://%s:%d", strings.Trim(addr, "[]"), rec.Port),
			Company: rec.Company, Src: "registry",
		})
	}
	return out, nil
}

/* ---------- 连接与配置应用 ---------- */

func (a *App) handleWizardConnect(w http.ResponseWriter, r *http.Request) {
	if a.Cfg.CompanyLinkOff {
		writeErr(w, 400, "公司接入已停用：到 设置 · 网络访问 打开「公司接入」开关后再运行向导")
		return
	}
	var body struct {
		Server string `json:"server"`
		User   string `json:"user"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	server := strings.TrimRight(strings.TrimSpace(body.Server), "/")
	user := strings.TrimSpace(body.User)
	if server == "" || user == "" || len([]rune(user)) > 32 {
		writeErr(w, 400, "需要服务器地址与用户名（1~32 字）")
		return
	}
	if !strings.HasPrefix(server, "http://") && !strings.HasPrefix(server, "https://") {
		server = "https://" + server // 容错：裸地址按 https 补协议（明文已停用）
	}

	// ① 验证是 jxai-server（healthz）
	resp, err := wizardHTTP.Get(server + "/healthz")
	if err != nil {
		writeErr(w, 400, "连不上 "+server+"："+err.Error())
		return
	}
	var hz struct {
		Service string `json:"service"`
	}
	_ = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&hz)
	resp.Body.Close()
	if resp.StatusCode != 200 || hz.Service != "jxai-server" {
		writeErr(w, 400, server+" 不是 jxai-server（地址或端口对吗？）")
		return
	}

	// ② 取配置包
	payload, err := fetchProvision(server, user)
	if err != nil {
		writeErr(w, 502, "取配置失败："+err.Error())
		return
	}

	// ③ 应用到运行配置：内部提供方置顶（即默认），市场/更新一键配好
	respBody := map[string]any{
		"ok": 1, "company": payload.Company, "server": server, "user": user,
		"api":       payload.API != nil,
		"apiDetail": payload.API, // 明细（含 monthlyQuotaUSD）供向导成功页展示
		"apiError":  payload.APIError,
		"marketURL": payload.MarketURL, "updateURL": payload.UpdateURL,
		// 门户充值入口（2026-11-12）：portal 启用的服务端才非空；成功页据此附
		//「充值/查余额」链接。
		"portalURL": strings.TrimRight(payload.PortalURL, "/"),
	}
	if models := a.applyProvisionPayload(&payload); models != nil {
		respBody["defaultModel"] = firstModelID(models)
		respBody["modelCount"] = len(models)
	}
	a.Cfg.WizardDone = true
	// 公司名/服务端地址留给自动同步器（sync.go）：跟随服务端变化、失联时按公司名重新发现
	a.Cfg.WizardCompany = payload.Company
	a.Cfg.WizardServer = server
	a.Cfg.WizardPublicServer = payload.PublicServer // 空=服务端没配公网地址，出公司即自动停用公司通道
	a.Cfg.PortalURL = strings.TrimRight(payload.PortalURL, "/") // 门户充值入口（2026-11-12）：portal 启用才非空
	// 向导里填的姓名即默认「发布者姓名」（市场条目的归属名 owner）：发布技能时
	// 随包上报，市场卡片据此判断哪些条目能「更新发布 / 删除」。重跑向导按新填的
	// 姓名覆盖（向导填的就是"我是谁"）；只想改名去 设置 · 网络访问。
	a.Cfg.PublisherName = user
	// 手动跑过向导 = 配置已是最新：清掉「一次性重新接入」的待办与已提醒标记
	// （sync.go companyResyncTarget），免得下次启动又自动重跑/再弹一次。
	a.Cfg.CompanyResync = companyResyncTarget
	a.Cfg.CompanyResyncPrompted = false
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "配置保存失败："+err.Error())
		return
	}
	log.Printf("[向导] 已接入 %s（%s，用户「%s」；模型通道=%v）", server, payload.Company, user, payload.API != nil)
	writeJSON(w, 200, respBody)
}

func firstModelID(models []ModelCfg) string {
	if len(models) == 0 {
		return ""
	}
	return models[0].ID
}
