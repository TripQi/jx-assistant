// websearch_providers.go —— 网络搜索供应商的配置模型与「哪几家此刻能用」的解析。
//
// 设计意图（为什么这么拆）：
//   - web_search 原来只有一个来源：公司网关 /api/search（服务端持上游 key）。
//     用户要自己配第三方搜索/抽取服务（DeepSeek、Tavily、AnySearch、博查、
//     智谱），于是把「一家服务」抽象成一条供应商：kind 决定请求怎么拼、响应
//     怎么归一（见 websearch_search.go），order 决定谁先用。公司网关不再特殊
//     对待——它就是 kind=gateway 的一条，老配置的 searchURL 字段原样保留
//     （零迁移），网关地址仍在调用时解析（searchURL 优先、空则跟随市场同源）。
//   - 只有「失败」才回落下一家：空结果是有效答案（DeepSeek 文档明确空结果不计费，
//     用户问了个冷门东西时也确实没有结果），逐家再试一遍只会让一次搜索多等几秒。
//   - 总耗时是预算，不是每家各自超时相加：用户在等的是「一次搜索」而不是
//     「五家各 10 秒」，所以默认总预算 20 秒，逐家分到的超时也从剩余预算里出。
//   - key 只在内存里用、落盘走 DPAPI（config.go 的 Save），对外（/api/config）
//     只回掩码；错误文案里绝不带响应体（AnySearch 的 402 响应体含账号密码与
//     api_key，见调研报告 §4.7）。
package main

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// 供应商类型（kind）：kind 决定端点路径、请求体与响应字段的映射关系。
const (
	spKindGateway   = "gateway"   // 公司网关：程序自带的 /api/search（服务端持上游 key）
	spKindDeepSeek  = "deepseek"  // DeepSeek Search & Fetch API（/v1/tools/search、/v1/tools/fetch）
	spKindTavily    = "tavily"    // Tavily（/search、/extract，抽取支持 1–20 个 URL）
	spKindAnySearch = "anysearch" // AnySearch（/v1/search、/v1/extract）
	spKindBocha     = "bocha"     // 博查（/v1/web-search，只有搜索）
	spKindZhipu     = "zhipu"     // 智谱 GLM（/api/paas/v4/web_search，只有搜索）
)

const (
	// 一次 web_search 的总耗时预算（秒，可经 config.searchTimeoutSec 覆盖）
	searchDefaultBudgetSec = 20
	// 每家供应商的单次超时（秒）：没配 timeoutSec 时用它
	searchDefaultTimeoutSec = 10
	// 抽取兜底的单家超时与总预算（秒）：抽正文比搜索慢得多（Tavily 默认 10/30 秒、
	// AnySearch 服务端固定 30 秒），预算单算，不挤占搜索那条链路
	extractDefaultTimeoutSec = 20
	extractDefaultBudgetSec  = 35
	// 单家响应体读取上限（防上游回个几 MB 把内存吃掉）
	providerBodyLimit = 4 << 20
)

// SearchProviderCaps 一条供应商具备的能力：search=能做搜索、extract=能抽正文。
// 能力位是「这条服务本身支不支持」，不是「这次要不要用」——后者靠 enabled。
type SearchProviderCaps struct {
	Search  bool `json:"search"`
	Extract bool `json:"extract"`
}

// SearchProviderCfg 一条搜索供应商的配置。baseURL 必须可配：博查正从
// api.bochaai.com 迁到 api.bocha.cn（两个域现在都活），写死会在旧域下线时挂掉。
// 空 baseURL = 用该 kind 的内置默认地址。
type SearchProviderCfg struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`                // 显示名（设置页与结果里的来源标注）
	Kind         string             `json:"kind"`                // 见 spKind* 常量
	BaseURL      string             `json:"baseURL,omitempty"`   // 空 = 内置默认
	APIKey       string             `json:"apiKey,omitempty"`    // 明文只在内存；落盘走 DPAPI
	Capabilities SearchProviderCaps `json:"capabilities"`        //
	Enabled      *bool              `json:"enabled,omitempty"`   // nil = 启用（与 providers 同口径）
	Order        int                `json:"order"`               // 小 = 先用
	Engine       string             `json:"engine,omitempty"`    // 智谱 search_engine（search_std/search_pro/…）
	Region       string             `json:"region,omitempty"`    // 地区加权（不支持的家静默忽略）
	Freshness    string             `json:"freshness,omitempty"` // 新鲜度（博查 freshness / Tavily time_range）
	Count        int                `json:"count,omitempty"`     // 该家默认条数（0 = 用工具默认）
	TimeoutSec   int                `json:"timeoutSec,omitempty"` // 该家单次超时（0 = 默认 10 秒）
}

func (p SearchProviderCfg) isEnabled() bool { return p.Enabled == nil || *p.Enabled }

// timeout 该家的单次超时（夹在 2–60 秒之间）。
func (p SearchProviderCfg) timeout() time.Duration {
	sec := p.TimeoutSec
	if sec <= 0 {
		sec = searchDefaultTimeoutSec
	}
	if sec < 2 {
		sec = 2
	}
	if sec > 60 {
		sec = 60
	}
	return time.Duration(sec) * time.Second
}

// base 该家真正要打的 base：配置里填了就用填的，空则内置默认。
func (p SearchProviderCfg) base() string {
	u := strings.TrimSpace(p.BaseURL)
	if u == "" {
		u = defaultSearchBaseURL(p.Kind)
	}
	return strings.TrimRight(u, "/")
}

func (p SearchProviderCfg) displayName() string {
	if n := strings.TrimSpace(p.Name); n != "" {
		return n
	}
	return searchKindLabel(p.Kind)
}

// defaultSearchBaseURL 各家的官方 API 域（博查用新域 api.bocha.cn：官方文档正在
// 从 api.bochaai.com 迁过来，旧域仍可用、可在设置里改回去）。
func defaultSearchBaseURL(kind string) string {
	switch kind {
	case spKindDeepSeek:
		return "https://api.deepseek.com"
	case spKindTavily:
		return "https://api.tavily.com"
	case spKindAnySearch:
		return "https://api.anysearch.com"
	case spKindBocha:
		return "https://api.bocha.cn"
	case spKindZhipu:
		return "https://open.bigmodel.cn"
	}
	return ""
}

// searchKindLabel 类型的中文名（设置页类型下拉与结果标注共用）。
func searchKindLabel(kind string) string {
	switch kind {
	case spKindGateway:
		return "公司网关"
	case spKindDeepSeek:
		return "DeepSeek"
	case spKindTavily:
		return "Tavily"
	case spKindAnySearch:
		return "AnySearch"
	case spKindBocha:
		return "博查"
	case spKindZhipu:
		return "智谱"
	}
	return kind
}

// providerEngineLabel 结果里「引擎」字段的来源标注前缀（网关的引擎名以服务端
// 回的为准，其余按 kind 固定）。
func providerEngineLabel(kind string) string {
	switch kind {
	case spKindDeepSeek:
		return "deepseek-tools"
	case spKindTavily:
		return "tavily"
	case spKindAnySearch:
		return "anysearch"
	case spKindBocha:
		return "bocha"
	case spKindZhipu:
		return "zhipu"
	}
	return "gateway"
}

// searchKindMeta 设置页用的类型清单（前端不硬编码各家默认地址与能力位）。
type searchKindMeta struct {
	Kind        string `json:"kind"`
	Label       string `json:"label"`
	DefaultBase string `json:"defaultBase"`
	Search      bool   `json:"search"`  // 该类型支持搜索
	Extract     bool   `json:"extract"` // 该类型支持抽取
	Note        string `json:"note,omitempty"`
}

func searchKinds() []searchKindMeta {
	return []searchKindMeta{
		{Kind: spKindGateway, Label: "公司网关", Search: true, Note: "程序自带的 /api/search 网关（服务端持上游 key，本机不填 Key）；地址取上面的「搜索服务地址」，留空则跟随市场服务器同源"},
		{Kind: spKindDeepSeek, Label: "DeepSeek", DefaultBase: defaultSearchBaseURL(spKindDeepSeek), Search: true, Extract: true, Note: "/v1/tools/search 搜索、/v1/tools/fetch 抽正文（Markdown）；按成功请求计费，需账户有余额"},
		{Kind: spKindTavily, Label: "Tavily", DefaultBase: defaultSearchBaseURL(spKindTavily), Search: true, Extract: true, Note: "/search 搜索、/extract 抽正文（支持一次 1–20 个 URL）；免费 1000 credits/月"},
		{Kind: spKindAnySearch, Label: "AnySearch", DefaultBase: defaultSearchBaseURL(spKindAnySearch), Search: true, Extract: true, Note: "/v1/search 搜索、/v1/extract 抽正文；免费 1000 次/天，不填 Key 也能匿名试"},
		{Kind: spKindBocha, Label: "博查", DefaultBase: defaultSearchBaseURL(spKindBocha), Search: true, Note: "/v1/web-search 搜索（没有独立的正文抽取端点）；新域 api.bocha.cn，旧域 api.bochaai.com 可改回去"},
		{Kind: spKindZhipu, Label: "智谱", DefaultBase: defaultSearchBaseURL(spKindZhipu), Search: true, Note: "web_search 搜索（没有 REST 抽取端点）；search_pro / search_std 按次计费"},
	}
}

// defaultSearchProviders 默认起手配置：公司网关排第 1 位（未配置地址时自动跳过）、
// 然后 DeepSeek → Tavily → AnySearch。API Key 由用户自己填（本仓库不存任何凭据）：
// 结构在配置加载时补齐，key 通过设置页或 /api/config 写入，落盘即 DPAPI 密文。
func defaultSearchProviders() []SearchProviderCfg {
	return []SearchProviderCfg{
		{ID: "gateway", Name: "公司网关", Kind: spKindGateway, Capabilities: SearchProviderCaps{Search: true}, Order: 1},
		{ID: "deepseek", Name: "DeepSeek", Kind: spKindDeepSeek, BaseURL: defaultSearchBaseURL(spKindDeepSeek),
			Capabilities: SearchProviderCaps{Search: true, Extract: true}, Order: 2},
		{ID: "tavily", Name: "Tavily", Kind: spKindTavily, BaseURL: defaultSearchBaseURL(spKindTavily),
			Capabilities: SearchProviderCaps{Search: true, Extract: true}, Order: 3},
		{ID: "anysearch", Name: "AnySearch", Kind: spKindAnySearch, BaseURL: defaultSearchBaseURL(spKindAnySearch),
			Capabilities: SearchProviderCaps{Search: true, Extract: true}, Order: 4},
	}
}

// normalizeSearchProviders 加载后的口径补齐：id/name 缺省按 kind 填，order 为 0
// 的按原顺序排在后面。kind 不认识的不丢弃（运行时跳过并给出可读原因）——用户手改
// 配置文件写错时要能看见，而不是静默消失。
func normalizeSearchProviders(list []SearchProviderCfg) []SearchProviderCfg {
	out := make([]SearchProviderCfg, 0, len(list))
	seen := map[string]bool{}
	for i, p := range list {
		p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
		if p.Kind == "" {
			continue // 空 kind 没有任何含义（前端删行后可能留下空壳）
		}
		if p.ID = strings.TrimSpace(p.ID); p.ID == "" {
			p.ID = p.Kind
		}
		for seen[p.ID] {
			p.ID += "-2"
		}
		seen[p.ID] = true
		p.Name = strings.TrimSpace(p.Name)
		if p.Name == "" {
			p.Name = searchKindLabel(p.Kind)
		}
		if p.Order <= 0 {
			p.Order = 100 + i
		}
		p.BaseURL = strings.TrimSpace(p.BaseURL)
		p.APIKey = strings.TrimSpace(p.APIKey)
		p.Engine = strings.TrimSpace(p.Engine)
		p.Region = strings.TrimSpace(p.Region)
		p.Freshness = strings.TrimSpace(p.Freshness)
		out = append(out, p)
	}
	return out
}

// searchProviderView 设置页看到的一条供应商（Key 只有掩码）。
type searchProviderView struct {
	ID           string             `json:"id"`
	Name         string             `json:"name"`
	Kind         string             `json:"kind"`
	KindLabel    string             `json:"kindLabel"`
	BaseURL      string             `json:"baseURL"`
	DefaultBase  string             `json:"defaultBase"`
	APIKeyMasked string             `json:"apiKeyMasked"`
	Capabilities SearchProviderCaps `json:"capabilities"`
	Enabled      bool               `json:"enabled"`
	Order        int                `json:"order"`
	Engine       string             `json:"engine,omitempty"`
	Region       string             `json:"region,omitempty"`
	Freshness    string             `json:"freshness,omitempty"`
	Count        int                `json:"count,omitempty"`
	TimeoutSec   int                `json:"timeoutSec,omitempty"`
	// Available 此刻能不能用（公司网关没配地址时为 false），设置页据此标灰并说明
	Available bool `json:"available"`
	// UnavailableHint 不能用的原因（能用时为空）
	UnavailableHint string `json:"unavailableHint,omitempty"`
}

// searchProviderViews 生成设置页回显（顺带给出运行期可用性：公司网关的地址是
// 调用时解析的，用户看不到「现在到底通不通」，这里替他算出来）。
func (a *App) searchProviderViews() []searchProviderView {
	gatewayBase, gatewayErr := a.searchEndpointBase()
	out := make([]searchProviderView, 0, len(a.Cfg.SearchProviders))
	for _, p := range a.Cfg.SearchProviders {
		v := searchProviderView{
			ID: p.ID, Name: p.displayName(), Kind: p.Kind, KindLabel: searchKindLabel(p.Kind),
			BaseURL: p.BaseURL, DefaultBase: defaultSearchBaseURL(p.Kind),
			APIKeyMasked: maskKey(p.APIKey), Capabilities: p.Capabilities,
			Enabled: p.isEnabled(), Order: p.Order, Engine: p.Engine,
			Region: p.Region, Freshness: p.Freshness, Count: p.Count, TimeoutSec: p.TimeoutSec,
			Available: true,
		}
		switch p.Kind {
		case spKindGateway:
			if gatewayErr != nil {
				v.Available = false
				v.UnavailableHint = gatewayErr.Error()
			} else {
				v.BaseURL = gatewayBase
			}
		default:
			if defaultSearchBaseURL(p.Kind) == "" {
				v.Available = false
				v.UnavailableHint = "不支持的供应商类型：" + p.Kind
			} else if !v.Available {
				v.UnavailableHint = ""
			}
		}
		out = append(out, v)
	}
	return out
}

// validProviderBase 只接受 http(s) 地址（与 searchURL 同口径）。
func validProviderBase(u string) bool {
	parsed, err := url.Parse(u)
	if err != nil {
		return false
	}
	return (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}

// applySearchProviders 把设置页提交的供应商列表落到配置上：空 Key = 沿用同 ID 已
// 保存的 key（前端只拿到掩码，不可能回传明文），其余字段整体替换。
func (a *App) applySearchProviders(in []SearchProviderCfg) error {
	prev := make(map[string]SearchProviderCfg, len(a.Cfg.SearchProviders))
	for _, p := range a.Cfg.SearchProviders {
		prev[p.ID] = p
	}
	list := make([]SearchProviderCfg, 0, len(in))
	for _, p := range in {
		p.Kind = strings.ToLower(strings.TrimSpace(p.Kind))
		if p.Kind == "" {
			continue
		}
		p.ID = strings.TrimSpace(p.ID)
		if p.ID == "" {
			p.ID = p.Kind
		}
		p.BaseURL = strings.TrimSpace(p.BaseURL)
		if p.Kind != spKindGateway && p.BaseURL != "" && !validProviderBase(p.BaseURL) {
			return fmt.Errorf("%s 的 Base URL 不合法：%s（需为 http(s) 地址）", p.displayName(), p.BaseURL)
		}
		if strings.TrimSpace(p.APIKey) == "" {
			if old, ok := prev[p.ID]; ok {
				p.APIKey = old.APIKey
			}
		}
		list = append(list, p)
	}
	a.Cfg.SearchProviders = normalizeSearchProviders(list)
	return nil
}

// ---------- 运行时：启用 + 能力过滤 + 按 order 排序 ----------

// activeSearchProvider 一次调用里「此刻能用」的供应商：配置 + 解析后的 base +
// 生效顺位（1 起，回落到第几家就是第几位，结果里要标出来）。
type activeSearchProvider struct {
	cfg  SearchProviderCfg
	base string
	rank int
}

// searchProvidersFor 返回启用、具备指定能力、地址能解析出来的供应商，按 order 升序。
// 公司网关的地址在调用时解析（searchURL 优先、空则跟随市场同源）：解析不出来
// 就是「还没配服务端」，自动跳过而不报错——剩下的第三方供应商照常工作。
func (a *App) searchProvidersFor(want func(SearchProviderCaps) bool) []activeSearchProvider {
	list := make([]SearchProviderCfg, 0, len(a.Cfg.SearchProviders))
	for _, p := range a.Cfg.SearchProviders {
		if p.isEnabled() && want(p.Capabilities) {
			list = append(list, p)
		}
	}
	sort.SliceStable(list, func(i, j int) bool { return list[i].Order < list[j].Order })
	out := make([]activeSearchProvider, 0, len(list))
	for _, p := range list {
		var base string
		if p.Kind == spKindGateway {
			b, err := a.searchEndpointBase()
			if err != nil {
				continue // 未配置/地址非法：这条视为不可用
			}
			base = b
		} else if defaultSearchBaseURL(p.Kind) != "" {
			base = p.base()
		} else {
			continue // 不认识的 kind
		}
		if base == "" {
			continue
		}
		out = append(out, activeSearchProvider{cfg: p, base: strings.TrimRight(base, "/"), rank: len(out) + 1})
	}
	return out
}

// providerCtx 给一家供应商切一块超时：不超过它自己的 timeout，也不越过整次
// 调用剩余的总预算（总预算是硬约束，单家超时只是上限）。
func providerCtx(ctx context.Context, per time.Duration) (context.Context, context.CancelFunc) {
	if per <= 0 {
		per = searchDefaultTimeoutSec * time.Second
	}
	if dl, ok := ctx.Deadline(); ok {
		if left := time.Until(dl); left < per {
			if left <= 0 {
				left = time.Millisecond
			}
			return context.WithTimeout(ctx, left)
		}
	}
	return context.WithTimeout(ctx, per)
}

// searchBudgetSec 一次 web_search 的总耗时预算（秒）。
func (c *Config) searchBudgetSec() int {
	v := searchDefaultBudgetSec
	if c != nil && c.SearchTimeoutSec >= 5 && c.SearchTimeoutSec <= 120 {
		v = c.SearchTimeoutSec
	}
	return v
}

// ---------- 失败分类（回落的依据） ----------

// searchErrKind 失败类别：全部类别都回落（只有成功才算成功），分类的意义在
// 文案与排障——Key 无效、额度用尽、被限流各自该改什么，用户看一眼就知道。
type searchErrKind int

const (
	searchErrNetwork searchErrKind = iota + 1 // DNS/TLS/超时/连接被拒
	searchErrAuth                             // key 缺失/无效/过期/无权限
	searchErrQuota                            // 额度耗尽 / 余额不足
	searchErrRateLimit                        // 被限流
	searchErrParse                            // 响应结构不符 / 业务码非 0 / 参数被拒
	searchErrUpstream                         // 5xx / 服务不可用
	searchErrRejected                         // 目标站策略拒绝（抽取 403/404/422）
	searchErrConfig                           // 配置不完整（如没填 Key、类型不支持该操作）
)

func (k searchErrKind) label() string {
	switch k {
	case searchErrNetwork:
		return "连不上"
	case searchErrAuth:
		return "Key 无效或无权限"
	case searchErrQuota:
		return "额度或余额不足"
	case searchErrRateLimit:
		return "被限流"
	case searchErrParse:
		return "响应无法解析"
	case searchErrUpstream:
		return "上游服务故障"
	case searchErrRejected:
		return "目标站拒绝抓取"
	case searchErrConfig:
		return "配置不完整"
	}
	return "未知错误"
}

// providerError 一家供应商的一次失败：带类别、HTTP 状态与（已脱敏的）原因。
type providerError struct {
	Provider string        `json:"provider"`
	Kind     searchErrKind `json:"kind"`
	KindText string        `json:"kindText"`
	Status   int           `json:"status,omitempty"`
	Msg      string        `json:"msg"`
}

func (e *providerError) Error() string {
	if e.Status > 0 {
		if e.Msg != "" {
			return fmt.Sprintf("%s（HTTP %d：%s）", e.Kind.label(), e.Status, e.Msg)
		}
		return fmt.Sprintf("%s（HTTP %d）", e.Kind.label(), e.Status)
	}
	if e.Msg != "" {
		return fmt.Sprintf("%s（%s）", e.Kind.label(), e.Msg)
	}
	return e.Kind.label()
}

func newProviderError(p activeSearchProvider, status int, kind searchErrKind, msg string) *providerError {
	return &providerError{Provider: p.cfg.displayName(), Kind: kind, KindText: kind.label(), Status: status, Msg: msg}
}

// classifySearchStatus HTTP 状态码 → 失败类别（搜索口径）。
func classifySearchStatus(status int) searchErrKind {
	switch {
	case status == 401 || status == 403:
		return searchErrAuth
	case status == 402 || status == 432 || status == 433:
		return searchErrQuota
	case status == 429:
		return searchErrRateLimit
	case status == 408:
		return searchErrNetwork
	case status >= 500:
		return searchErrUpstream
	case status == 404:
		return searchErrUpstream
	}
	return searchErrParse // 400/405/422：请求本身被拒，换一家也没用但照样回落
}

// classifyExtractStatus 抽取口径：403/404/422 在抽取里是「目标站不让抓」
//（DeepSeek Fetch 的 403=安全策略拒绝、404=取不到可用内容；AnySearch 422=extract_failed）。
func classifyExtractStatus(status int) searchErrKind {
	switch status {
	case 403, 404, 422:
		return searchErrRejected
	}
	return classifySearchStatus(status)
}
