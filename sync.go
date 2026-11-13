// 公司接入自动同步：向导接入后，客户端持续跟随服务端的变化，员工零操作。
//
// 服务端（jxaiserver announce.go）两级通知：
//  1. GET /announce 公告（公司名、模型清单含能力元数据、市场/更新地址、内容哈希
//     rev、可选的公网服务端地址 publicServer 与外网模型通道 publicBaseURL）——
//     本文件每 5 分钟拉一次（启动 20 秒后首轮，带 ?user= 按人取模型清单），
//     差异驱动应用；
//  2. 配置一变就向局域网 UDP 2230 广播——客户端开着一个 UDP 监听（端口被占则
//     静默退化为纯轮询），收到本公司名的广播立即触发一轮同步（秒级跟上）。
//
// 服务端搬家（换端口/换机器，如 2228→2227）：当前地址失联时按保存的公司名重新
// 局域网发现（discovery 应答自带新地址）自动迁移；广播消息里的 server 地址也作为
// 迁移候选优先一试（旧地址已死、新地址广播可达的场景）。
//
// 公网兜底与公司通道的自动停用/恢复：内网全源失联时改拉服务端公网地址
// （wizardPublicServer，仅由服务端下发）上的公告——公网可达就不算失联（模型调用
// 走外网通道故障转移，见 llm.go StreamChat）。内网公网都失联连续 2 轮（约 10 分钟）
// 自动停用公司通道（AutoDisabled，选模型自动落到私人通道）；实际调用连接失败也
// 立即触发（agent.go 的 noteProviderConnFail）。任一来源恢复可达即自动重新启用
// （手动停用 Enabled=false 不受影响）。
//
// 公司通道被删后的自动重建：公告拉取成功但本地没有 company-internal 时，按原
// 姓名自动 POST /provision 重新领一份配置包（公告里没有 key，必须走 provision），
// 通道置顶重建——员工删掉公司通道，回到公司连上局域网几分钟内自动恢复。
//
// 应用范围：公司名（内部提供方改名）、模型清单与元数据（增删/上下文/图片能力）、
// new-api 地址（baseURL）、外网通道地址（publicBaseURL）、市场/更新地址、服务端
// 地址本身。API key 不在此轮换（key 由 provision 按人发放；失效重跑向导即可）。
package main

import (
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
	syncInterval    = 5 * time.Minute  // 常规轮询间隔
	syncFirstDelay  = 20 * time.Second // 启动后首轮
	syncUDPDebounce = 15 * time.Second // 广播触发的去抖窗口
	syncAnnounceUDP = 2230             // 服务端变更广播端口（jxaiserver announce.go）
	syncFailLimit   = 2                // 连续失联多少轮后自动停用公司通道（2×5min≈10min）
)

// udpDiscoverFn 发现函数的可测试替换点。
var udpDiscoverFn = udpDiscoverServers

// announcePayload GET /announce 的载荷（与 jxaiserver announce.go 对齐）。
type announcePayload struct {
	Jxai      int    `json:"jxai"`
	Company   string `json:"company"`
	Server    string `json:"server"`
	Rev       string `json:"rev"`
	MarketURL string `json:"marketURL"`
	UpdateURL string `json:"updateURL"`
	// PublicServer 服务端公网地址（管理页配置，空=未启用）：存 WizardPublicServer。
	PublicServer string `json:"publicServer,omitempty"`
	// RegistryURL 总服务器地址（服务端「模型」页签配置，空=不下发）：收到即覆盖
	// 本地设置里的「总服务器地址」——公司换总服务器不用逐台改客户端（2026-10-29）。
	RegistryURL string `json:"registryURL,omitempty"`
	// RelayHost / RelayPort 远程接入（公网中继）的网关：服务端下发。客户端只在"当前值
	// 还是出厂默认 example.com"时才跟随，手改过的不动（2026-10-29）。
	RelayHost string `json:"relayHost,omitempty"`
	RelayPort int    `json:"relayPort,omitempty"`
	// Quota 该员工的额度快照（按人下发：剩余/已用/重置规则/下次重置，见服务端
	// quota.go）。只读缓存，客户端只存着显示，不参与调用。
	Quota *CompanyQuotaCfg `json:"quota,omitempty"`
	// Brand 品牌下发（服务端管理页「品牌」页签，brand_sync.go）：LOGO 与界面
	// 文字统一配置。nil=老服务端未下发（保持本地现值）。LOGO 按 logoHash 比对，
	// 不变不下载（一张图只在该换时传一次）。
	Brand *brandAnnounce `json:"brand,omitempty"`
	API   *struct {
		BaseURL       string            `json:"baseURL"`
		PublicBaseURL string            `json:"publicBaseURL,omitempty"` // 外网模型通道（对所有人开放或该用户被授权才下发）
		Models        []wizardModelInfo `json:"models"`
		DefaultModel  string            `json:"defaultModel"`
	} `json:"api"`
}

// startCompanySyncer 后台同步循环 + UDP 广播监听（NewApp 起一次）。
// 向导完成前（WizardServer 空）各轮空转，接入完成后无需重启即开始同步；
// 公司接入被停用（设置·网络访问的开关，CompanyLinkOff）时整体不连服务端。
func startCompanySyncer(a *App) {
	go func() {
		time.Sleep(syncFirstDelay)
		for {
			if a.Cfg.WizardServer != "" && !a.Cfg.CompanyLinkOff {
				companySyncOnce(a, "")
			}
			time.Sleep(syncInterval)
		}
	}()
	go listenAnnounceUDP(a)
}

// companySyncOnce 一轮同步：hint 非空时优先试广播带来的地址（服务端搬家、旧地址
// 已死）；全部失联再按公司名重新局域网发现；仍失联改拉服务端公网地址兜底
// （出公司场景：公网可达就不算失联）。拿到公告后差异驱动应用。
func companySyncOnce(a *App, hintServer string) {
	if a.Cfg.CompanyLinkOff { // 接入停用：同步器与广播触发的入口都收在这
		return
	}
	server := strings.TrimRight(a.Cfg.WizardServer, "/")
	if server == "" {
		return
	}
	user := strings.TrimSpace(a.Cfg.PublisherName) // 按人身份：服务端用它给该人返回模型子集/外网授权
	candidates := []string{}
	if h := strings.TrimRight(strings.TrimSpace(hintServer), "/"); h != "" && h != server {
		candidates = append(candidates, h)
	}
	candidates = append(candidates, server)

	var ann *announcePayload
	for _, c := range candidates {
		if p := fetchAnnounce(c, user); p != nil {
			ann = p
			if c != server { // 广播地址即是新家：市场/更新/服务端地址整体迁移
				migrateServerBase(a, server, c)
				log.Printf("[公司同步] 服务端已迁移：%s → %s", server, c)
				server = c
			}
			break
		}
	}
	if ann == nil && a.Cfg.WizardCompany != "" { // 失联：按公司名重新发现（换端口/换机器）
		for _, ws := range udpDiscoverFn(a.Cfg.WizardCompany) {
			if c := strings.TrimRight(ws.Server, "/"); c != "" && c != server {
				if p := fetchAnnounce(c, user); p != nil {
					ann = p
					migrateServerBase(a, server, c)
					log.Printf("[公司同步] 失联后重新发现，服务端迁移：%s → %s", server, c)
					server = c
					break
				}
			}
		}
	}
	// 公网兜底：内网全源失联时改拉服务端公网地址（仅由服务端下发，本地不写死）。
	// 公网公告只更新公司通道/模型/外网地址，不迁移内网服务端与市场/更新地址——
	// 回到公司内网候选恢复可达，一切自动切回。
	public := strings.TrimRight(a.Cfg.WizardPublicServer, "/")
	fromPublic := false
	if ann == nil && public != "" && public != server {
		if p := fetchAnnounce(public, user); p != nil {
			ann = p
			fromPublic = true
		}
	}
	if ann == nil {
		noteCompanySyncFailed(a)
		return
	}
	noteCompanySyncOK(a)
	// 内网/外网线路状态（llm.go setAltPreferred，翻转即广播 providers 事件刷新
	// 模型菜单徽标）：公告只能从公网拿到 = 出公司——直接置「优先外网」，下一通
	// 电话不再先白等内网拨号超时；内网候选可达则回到内网优先。仅配了外网地址
	// 的公司通道参与。
	if p := wizardProvider(a); p != nil && p.PublicBaseURL != "" {
		setAltPreferred(wizardProviderID, fromPublic)
	}
	// 公司通道被删：按原姓名重新领配置包重建（公告里没有 key，必须走 provision）
	if wizardProvider(a) == nil {
		src := server
		if fromPublic {
			src = public
		}
		reprovisionCompany(a, src, user)
		return
	}
	applyAnnounce(a, ann, server, fromPublic)
}

// fetchAnnounce 拉一份公告；user 非空时带 ?user=（服务端按人返回模型子集与外网
// 通道授权）。任何失败都返回 nil（同步器把 nil 当失联处理）。
func fetchAnnounce(server, user string) *announcePayload {
	u := strings.TrimRight(server, "/") + "/announce"
	if user != "" {
		u += "?user=" + url.QueryEscape(user)
	}
	client := http.Client{Timeout: 10 * time.Second, Transport: companyTransport}
	resp, err := client.Get(u)
	if err != nil {
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	var p announcePayload
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p) != nil || p.Jxai != 1 {
		return nil
	}
	return &p
}

// migrateServerBase 服务端地址整体迁移：指向旧地址的市场/更新地址换到新地址。
func migrateServerBase(a *App, oldBase, newBase string) {
	if oldBase == "" || oldBase == newBase {
		return
	}
	swap := func(u string) string {
		if u == oldBase || strings.HasPrefix(u, oldBase+"/") {
			return newBase + strings.TrimPrefix(u, oldBase)
		}
		return u
	}
	a.Cfg.MarketURL = swap(a.Cfg.MarketURL)
	a.Cfg.UpdateURL = swap(a.Cfg.UpdateURL)
	a.Cfg.WizardServer = newBase
}

// applyAnnounce 差异驱动应用公告：有变化才写盘；空模型清单不应用（服务端拉取失败
// 的防御，避免误清员工模型）。fromPublic=本份公告来自公网兜底：只更新公司通道/
// 模型/外网地址与公司名，不动内网服务端与市场/更新地址（回公司内网候选恢复可达
// 后整体切回，公网期间公司通道的 baseURL 仍是内网地址，靠 llm.go 的外网故障转移）。
func applyAnnounce(a *App, ann *announcePayload, server string, fromPublic bool) {
	changed := []string{}
	if ann.Company != "" && ann.Company != a.Cfg.WizardCompany {
		a.Cfg.WizardCompany = ann.Company
		changed = append(changed, "公司名→"+ann.Company)
	}
	// 小程序平台地址不再随公告下发（2026-11-03 用户口径）：一律从市场地址推导，
	// 远程接入网关：出厂默认（example.com）才跟随服务端下发，手改过的保留
	if ann.RelayHost != "" && a.Cfg.RemoteAccess.Host == defaultRemoteHost {
		if a.Cfg.RemoteAccess.Host != ann.RelayHost {
			a.Cfg.RemoteAccess.Host = ann.RelayHost
			changed = append(changed, "中继网关→"+ann.RelayHost)
		}
		if ann.RelayPort > 0 && a.Cfg.RemoteAccess.Port != ann.RelayPort {
			a.Cfg.RemoteAccess.Port = ann.RelayPort
			changed = append(changed, "中继端口→"+strconv.Itoa(ann.RelayPort))
		}
	}
	if ann.RegistryURL != "" && ann.RegistryURL != a.Cfg.RegistryURL {
		a.Cfg.RegistryURL = strings.TrimRight(ann.RegistryURL, "/")
		changed = append(changed, "总服务器→"+a.Cfg.RegistryURL)
	}
	if ann.PublicServer != "" && ann.PublicServer != a.Cfg.WizardPublicServer {
		a.Cfg.WizardPublicServer = strings.TrimRight(ann.PublicServer, "/")
		changed = append(changed, "公网服务端→"+a.Cfg.WizardPublicServer)
	}
	// 额度快照：服务端下发了就覆盖（含剩余/已用/规则/下次重置）；没下发不动旧值
	if ann.Quota != nil && !quotaEqual(a.Cfg.CompanyQuota, ann.Quota) {
		a.Cfg.CompanyQuota = ann.Quota
		changed = append(changed, "额度")
	}
	// 品牌下发（服务端管理页「品牌」页签，brand_sync.go）：缓存于 data/brand/，
	// LOGO 按 hash 比对只在该换时下载一次，渲染全程读本地；nil（老服务端）不动
	if c := a.applyBrand(ann.Brand, server); c != "" {
		changed = append(changed, c)
	}
	if p := wizardProvider(a); p != nil {
		if ann.Company != "" {
			p.Name = ann.Company + "·内部通道"
		}
		if ann.API != nil && len(ann.API.Models) > 0 {
			if ann.API.BaseURL != "" && !fromPublic {
				p.BaseURL = ann.API.BaseURL // 公网公告的 baseURL 按公网推导，不覆盖内网地址
			}
			p.PublicBaseURL = strings.TrimRight(ann.API.PublicBaseURL, "/") // 空=未授权/关闭：同步清除
			// 公司通道的密钥+模型由服务端下发：统一走 SetPrimaryKey（多 Key 结构
			// 改第一把，单 Key 结构改供应商级），别的地方不用关心结构形态
			models := buildWizardModels(ann.API.Models)
			p.SetPrimaryKey(p.APIKey, models)
			changed = append(changed, fmt.Sprintf("模型×%d", len(ann.API.Models)))
			// 用户选中的模型被下线时不再自动改成该渠道第一个模型（旧行为：
			// a.Cfg.Model = firstModelID(models)）——选择是用户的，只标记为不可用：
			// 全局默认模型保留原值，设置页与相关卡片按 checkModelRef 的判定红字
			// 提示「该模型已不在渠道清单里，请重选」，绝不静默换成别的模型。
			if ref := a.Cfg.DefaultModel.norm(); ref.Provider == p.ID && ref.Model != "" && !providerHasModel(p, ref.Model) {
				log.Printf("[公司同步] 全局默认模型「%s」已不在渠道「%s」的清单里：保留用户选择不自动改，请到设置 → 模型 → 默认模型重选", ref.Model, p.ID)
			}
		}
	}
	if !fromPublic {
		if ann.MarketURL != "" && ann.MarketURL != a.Cfg.MarketURL {
			a.Cfg.MarketURL = ann.MarketURL
			a.marketMu.Lock()
			a.marketCache = marketCacheEntry{} // 市场缓存作废
			a.marketMu.Unlock()
			changed = append(changed, "市场地址")
		}
		if ann.UpdateURL != "" && ann.UpdateURL != a.Cfg.UpdateURL {
			a.Cfg.UpdateURL = ann.UpdateURL
			changed = append(changed, "更新源")
		}
		if server != "" && server != a.Cfg.WizardServer {
			a.Cfg.WizardServer = server
			changed = append(changed, "服务端→"+server)
		}
	}
	if len(changed) == 0 {
		return
	}
	a.Cfg.WizardRev = ann.Rev
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		log.Printf("[公司同步] 配置保存失败：%v", err)
		return
	}
	log.Printf("[公司同步] 已跟随服务端变化：%s", strings.Join(changed, "、"))
	a.broadcast(Event{Type: "providers"}) // 模型清单变了：前端刷新模型菜单
}

// quotaEqual 两份额度快照是否一样（决定要不要落盘 + 广播刷新界面）。
func quotaEqual(a, b *CompanyQuotaCfg) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Points == b.Points && a.RemainPoints == b.RemainPoints && a.UsedPoints == b.UsedPoints &&
		a.Period == b.Period && a.PeriodText == b.PeriodText && a.NextAt == b.NextAt &&
		a.Unlimited == b.Unlimited && a.Status == b.Status
}

// handleCompanySyncNow POST /api/company-sync：设置页「刷新额度」按钮用——
// 直接复用公司同步器的一轮（companySyncOnce）：重拉一份公告，额度/模型/地址
// 一起跟上。不新开额度查询口子（额度就挂在公告里，服务端只读内存缓存、毫秒级）。
func (a *App) handleCompanySyncNow(w http.ResponseWriter, r *http.Request) {
	if a.Cfg.CompanyLinkOff {
		writeErr(w, 400, "公司接入已停用：到 设置 · 网络访问 打开「公司接入」开关后再试")
		return
	}
	if strings.TrimSpace(a.Cfg.WizardServer) == "" {
		writeErr(w, 400, "还没接入公司服务端（先跑一次接入向导）")
		return
	}
	companySyncOnce(a, "")
	writeJSON(w, 200, map[string]any{"ok": true, "quota": a.Cfg.CompanyQuota})
}

/* ---------- 公司通道的自动停用/恢复与自动重建 ---------- */ // noteCompanySyncOK 一轮同步拿到公告：清零失联计数；公司通道曾被自动停用的恢复。
func noteCompanySyncOK(a *App) {
	a.syncFailMu.Lock()
	a.syncFails = 0
	a.syncFailMu.Unlock()
	if p := wizardProvider(a); p != nil && p.AutoDisabled {
		setCompanyAutoDisabled(a, false, "")
	}
}

// noteCompanySyncFailed 一轮同步全源失联：累计计数，达到 syncFailLimit 轮自动
// 停用公司通道（约 10 分钟，防瞬时网络抖动误停）。
func noteCompanySyncFailed(a *App) {
	a.syncFailMu.Lock()
	a.syncFails++
	n := a.syncFails
	a.syncFailMu.Unlock()
	if n < syncFailLimit {
		return
	}
	if p := wizardProvider(a); p != nil && p.IsSelectable() {
		setCompanyAutoDisabled(a, true, "公司服务端连续 "+strconv.Itoa(n)+" 轮不可达")
	}
}

// noteProviderConnFail 实际调用中连接失败（StreamChat 内外网都试过仍连不上）且
// 目标是公司通道：立即自动停用，不等 5 分钟轮询——出公司的第一条消息就能切到
// 私人通道。返回是否本次真的执行了停用（调用方据此决定要不要切换重试）。
func (a *App) noteProviderConnFail(target LLMTarget) bool {
	if target.ProviderID != wizardProviderID {
		return false
	}
	p := wizardProvider(a)
	if p == nil || p.AutoDisabled || !p.IsSelectable() {
		return false
	}
	setCompanyAutoDisabled(a, true, "模型调用连接失败")
	return true
}

// setCompanyAutoDisabled 切换公司通道的自动停用状态并落盘广播（手动停用
// Enabled=false 不受影响：自动恢复只清 AutoDisabled，不动 Enabled）。
func setCompanyAutoDisabled(a *App, disabled bool, reason string) {
	p := wizardProvider(a)
	if p == nil || p.AutoDisabled == disabled {
		return
	}
	p.AutoDisabled = disabled
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		log.Printf("[公司同步] 保存配置失败：%v", err)
	}
	if disabled {
		log.Printf("[公司同步] 公司通道已自动停用（%s）；恢复可达后自动启用", reason)
	} else {
		log.Printf("[公司同步] 公司服务端恢复可达，公司通道已自动启用")
	}
	a.broadcast(Event{Type: "providers"})
}

// reprovisionCompany 公司通道被删后的自动重建：按原姓名重新领一份配置包整体应用
// （含 key 与模型清单）。失败静默返回，下一轮同步再试。
func reprovisionCompany(a *App, server, user string) {
	if user == "" {
		return
	}
	payload, err := fetchProvision(server, user)
	if err != nil {
		log.Printf("[公司同步] 公司通道缺失，自动重建失败：%v", err)
		return
	}
	if a.applyProvisionPayload(&payload) == nil {
		return // 服务端未配模型通道：等管理员配置，下一轮再来
	}
	a.Cfg.WizardPublicServer = strings.TrimRight(payload.PublicServer, "/")
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		log.Printf("[公司同步] 自动重建保存失败：%v", err)
		return
	}
	log.Printf("[公司同步] 公司通道已自动重建（用户「%s」，模型×%d）", user, len(wizardProvider(a).Models))
	a.broadcast(Event{Type: "providers"})
}

func wizardProvider(a *App) *ProviderCfg {
	for i := range a.Cfg.Providers {
		if a.Cfg.Providers[i].ID == wizardProviderID {
			return &a.Cfg.Providers[i]
		}
	}
	return nil
}

// providerHasModel 该提供方（多 Key 时任意一把 Key）的目录里有没有这个模型。
func providerHasModel(p *ProviderCfg, id string) bool {
	return p.hasModel(id)
}

/* ---------- 一次性重新接入（发新版本时把老配置追平） ---------- */

// companyResyncTarget 本轮「一次性重新接入」的目标标记版本：提高它 = 所有已接
// 入公司、但 companyResync 低于它的客户端，在下次启动时自动重跑一次公司接入。
// 20261020：10-16 更新后没再手动跑过向导的机器，模型清单还停在旧配置（服务端
// 早已改过模型，但那批客户端的自动同步器从没跑起来过——wizardServer 空、或版本
// 太老还没有 sync.go），直接追平比让人手点向导靠谱。
const companyResyncTarget = 20261020

// startCompanyResyncOnce 启动后异步做一次重新接入；不需要/没接入过公司的直接跳过。
func startCompanyResyncOnce(a *App) {
	if a.Cfg.CompanyResync >= companyResyncTarget {
		return
	}
	if a.Cfg.CompanyLinkOff || !a.Cfg.WizardDone {
		return // 没接入过公司（或已停用）：不打扰，首启向导照常负责
	}
	server := resyncServerBase(a)
	if server == "" {
		return // 连服务端地址都没有：交给首启向导/设置页
	}
	go func() {
		time.Sleep(8 * time.Second) // 先让窗口/HTTP 就绪，再谈网络
		a.companyResyncOnce(server)
	}()
}

// resyncServerBase 重新接入用的服务端地址：优先 wizardServer；缺了就从市场/更新
// 地址反推 origin（老配置的典型形态：市场与更新指向公司服务端，但 wizardServer
// 是空的，于是自动同步器从来没跑起来过）。
func resyncServerBase(a *App) string {
	if s := strings.TrimRight(strings.TrimSpace(a.Cfg.WizardServer), "/"); s != "" {
		return s
	}
	for _, u := range []string{a.Cfg.UpdateURL, a.Cfg.MarketURL} {
		if base := originOfURL(u); base != "" {
			return base
		}
	}
	return ""
}

// originOfURL 取 http(s) 地址的 scheme://host[:port]（认不出返回空）。
func originOfURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return ""
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// companyResyncOnce 一次重新接入，按可靠性递降三级：
//  1. 按人重领 provision（最完整：模型清单 + key + 市场/更新地址）；
//  2. 只拉公告 /announce（不动 key，够把模型清单/元数据追平）；
//  3. 都不成 → 弹一次「带原有配置」的向导（companyResyncPrompted 防重复打扰）。
func (a *App) companyResyncOnce(server string) {
	user := strings.TrimSpace(a.Cfg.PublisherName)
	finish := func(mode string, models int) {
		a.Cfg.CompanyResync = companyResyncTarget
		a.Cfg.CompanyResyncPrompted = false
		a.Cfg.syncLegacyFields()
		if err := a.Cfg.Save(a.CfgPath); err != nil {
			log.Printf("[重新接入] 配置保存失败：%v", err)
		}
		log.Printf("[重新接入] 已按服务端配置追平（%s，模型×%d，用户「%s」）", mode, models, user)
		a.broadcast(Event{Type: "company_resync", Title: "公司模型配置已更新",
			Data: map[string]any{
				"ok": true, "mode": mode, "models": models,
				"server": server, "user": user, "company": a.Cfg.WizardCompany,
			}})
	}

	// ① 按人重领配置包
	if user != "" {
		if payload, err := fetchProvision(server, user); err != nil {
			log.Printf("[重新接入] 按人领配置失败（改试公告）：%v", err)
		} else {
			models := a.applyProvisionPayload(&payload)
			a.Cfg.WizardDone = true
			if payload.Company != "" {
				a.Cfg.WizardCompany = payload.Company
			}
			a.Cfg.WizardServer = server
			if payload.PublicServer != "" {
				a.Cfg.WizardPublicServer = strings.TrimRight(payload.PublicServer, "/")
			}
			finish("provision", len(models))
			return
		}
	}

	// ② 只拉公告（公司通道还在才谈得上应用；不在了必须有姓名才能重建，走 ① 已失败）
	if p := wizardProvider(a); p != nil {
		if ann := fetchAnnounce(server, user); ann != nil {
			applyAnnounce(a, ann, server, false)
			if n := len(wizardProvider(a).Models); n > 0 {
				finish("announce", n)
				return
			}
		}
	}

	// ③ 都失败：弹一次带原有配置的向导
	log.Printf("[重新接入] 公司服务端 %s 不可达（用户「%s」），模型配置未能自动更新", server, user)
	if a.Cfg.CompanyResyncPrompted {
		return
	}
	a.Cfg.CompanyResyncPrompted = true
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		log.Printf("[重新接入] 配置保存失败：%v", err)
	}
	a.broadcast(Event{Type: "company_resync", Title: "公司配置需要重新连接",
		Data: map[string]any{
			"needWizard": true, "server": server, "user": user,
			"company": a.Cfg.WizardCompany,
			"reason":  "连不上公司服务端，模型配置没能自动更新",
		}})
}

// listenAnnounceUDP best-effort 监听服务端变更广播（UDP 2230）：端口被占（如多开
// 实例）时记一行日志退化为纯轮询。收到本公司名的变更广播即触发即时同步。
func listenAnnounceUDP(a *App) {
	pc, err := net.ListenPacket("udp4", ":"+strconv.Itoa(syncAnnounceUDP))
	if err != nil {
		log.Printf("[公司同步] UDP %d 监听失败（退化为纯轮询）：%v", syncAnnounceUDP, err)
		return
	}
	defer pc.Close()
	log.Printf("[公司同步] 监听 UDP %d（服务端变更广播即时生效）", syncAnnounceUDP)
	var lastTrigger time.Time
	buf := make([]byte, 1024)
	for {
		n, _, err := pc.ReadFrom(buf)
		if err != nil {
			return
		}
		var m struct {
			Jxai    int    `json:"jxai"`
			Type    string `json:"type"`
			Company string `json:"company"`
			Server  string `json:"server"`
		}
		if json.Unmarshal(buf[:n], &m) != nil || m.Jxai != 1 || m.Type != "change" {
			continue
		}
		if a.Cfg.WizardServer == "" || a.Cfg.WizardCompany == "" || m.Company != a.Cfg.WizardCompany {
			continue
		}
		if time.Since(lastTrigger) < syncUDPDebounce {
			continue
		}
		lastTrigger = time.Now()
		go companySyncOnce(a, m.Server)
	}
}
