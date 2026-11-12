package main

// usageapi.go —— 用量统计的 HTTP 接口（/api/usage/*）。
//
// 页面（组件 usage-stats 的设置页）用四个端点：
//   GET  /api/usage/summary   总览 + 每日趋势 + 按模型统计（日期/渠道筛选）
//   GET  /api/usage/records   明细表分页
//   GET  /api/usage/pricing   单价表 + 渠道倍率 + 渠道/Key 清单（筛选下拉也用）
//   POST /api/usage/pricing   保存单价表与倍率（整体替换）
//   POST /api/usage/cleanup   按日期范围删明细（不可恢复，前端二次确认）
//
// 费用一律在 Go 侧算：SQL 只聚合 token，价格与倍率是配置（usagepricing.go），
// 改价后历史数据的估算花费跟着变。未配单价的模型不给 0，而是标记 priced=false
// 让页面写「未配单价」——0 会被误读成免费。

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------- 视图结构 ----------

// usageKPI 是顶部 KPI 卡片行的数据。
type usageKPI struct {
	Calls         int64   `json:"calls"`
	Errors        int64   `json:"errors"`
	SuccessRate   float64 `json:"successRate"` // 0-100；无调用为 0
	Input         int64   `json:"input"`
	Output        int64   `json:"output"`
	CacheRead     int64   `json:"cacheRead"`
	CacheWrite    int64   `json:"cacheWrite"`
	Total         int64   `json:"total"`         // 输入 + 输出（页面「总 tokens」）
	CacheHitRate  float64 `json:"cacheHitRate"`  // 缓存读取 / 输入，0-100
	Cost          float64 `json:"cost"`          // 模型调用的估算花费（元，按调用时段计价）
	SearchCost    float64 `json:"searchCost"`    // 搜索/抽取的估算花费（元，按次）
	TotalCost     float64 `json:"totalCost"`     // 两者合计（页面「总花费」）
	SearchCalls   int64   `json:"searchCalls"`   // 搜索/抽取的供应商尝试次数
	Priced        bool    `json:"priced"`        // 是否全部模型都配了单价
	UnpricedCalls int64   `json:"unpricedCalls"` // 未配单价的调用数
	// 分时段分解（页面「高峰 X 次 ¥A + 空闲 Y 次 ¥B」）：调用按各自时段价的档位累计，
	// 用来核对时段规则是否真的生效——只看总额看不出这件事。
	PeakCalls int64   `json:"peakCalls"`
	PeakCost  float64 `json:"peakCost"`
	IdleCalls int64   `json:"idleCalls"`
	IdleCost  float64 `json:"idleCost"`
	RuleReady bool    `json:"ruleReady"` // 是否至少有一个模型配了时段规则（否则「未配置时段规则」）
}

// usageDailyPoint 是趋势图的一个点（天）。
type usageDailyPoint struct {
	Day        string  `json:"day"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Calls      int64   `json:"calls"`
	Errors     int64   `json:"errors"`
	Cost       float64 `json:"cost"`
	Priced     bool    `json:"priced"`
}

// usageModelStat 是「按模型消耗统计」的一行（同模型的多个渠道已合并，费用按各自
// 渠道倍率算完相加）。
type usageModelStat struct {
	Model      string  `json:"model"`
	Protocol   string  `json:"protocol"`
	Calls      int64   `json:"calls"`
	Errors     int64   `json:"errors"`
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Total      int64   `json:"total"`
	Cost       float64 `json:"cost"`
	Priced     bool    `json:"priced"`
}

type usageSummaryResp struct {
	Enabled bool              `json:"enabled"` // 组件是否开启（关闭时页面不可见，这里只作自检）
	From    string            `json:"from"`
	To      string            `json:"to"`
	KPI     usageKPI          `json:"kpi"`
	Daily   []usageDailyPoint `json:"daily"`
	ByModel []usageModelStat  `json:"byModel"`
}

// usageProviderKeyView 是渠道下拉里的一把 Key（多 Key 供应商才有多项）。
type usageProviderKeyView struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// usageProviderView 是「单价与倍率」里的渠道行，同时供筛选下拉使用。
type usageProviderView struct {
	ID         string                 `json:"id"`
	Name       string                 `json:"name"`
	Multiplier float64                `json:"multiplier"`
	Keys       []usageProviderKeyView `json:"keys"`
	// Models 该渠道已配的模型 id（去重）：页面「从渠道模型添加空行」按它加行，
	// 只加空行不填价（用户口径：程序不内置任何单价）。
	Models []string `json:"models"`
}

type usagePricingResp struct {
	Models    []usagePricingModel    `json:"models"`
	Providers []usageProviderView    `json:"providers"`
	Search    []usageSearchPriceView `json:"search"` // 搜索/抽取供应商的按次价
	Note      string                 `json:"note"`
	RuleReady bool                   `json:"ruleReady"` // 是否至少一个模型配了时段规则
}

// usageSearchPriceView 是「搜索单价」配置里的一行：一个搜索供应商 + 它的元/次价。
type usageSearchPriceView struct {
	ID      string  `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`    // 供应商类型（deepseek/tavily/…），决定默认价
	Price   float64 `json:"price"`   // 当前生效单价（元/次，成功才计费）
	Default float64 `json:"default"` // 该类型的公开默认价
	Note    string  `json:"note,omitempty"`
}

// ---------- 请求解析 ----------

// usageQueryFilter 从查询串取筛选条件（from/to/date 二选一，provider/key）。
func usageQueryFilter(r *http.Request) usageFilter {
	q := r.URL.Query()
	f := usageFilter{
		From:       strings.TrimSpace(q.Get("from")),
		To:         strings.TrimSpace(q.Get("to")),
		ProviderID: strings.TrimSpace(q.Get("provider")),
		KeyID:      strings.TrimSpace(q.Get("key")),
		Thinking:   strings.TrimSpace(q.Get("thinking")),
	}
	// 快捷区间由前端算好传 from/to；服务端只做兜底（date=today|7d|30d）
	if f.From == "" || f.To == "" {
		switch q.Get("date") {
		case "today":
			f.From, f.To = time.Now().Format("2006-01-02"), time.Now().Format("2006-01-02")
		case "7d":
			f.From, f.To = time.Now().AddDate(0, 0, -6).Format("2006-01-02"), time.Now().Format("2006-01-02")
		case "30d":
			f.From, f.To = time.Now().AddDate(0, 0, -29).Format("2006-01-02"), time.Now().Format("2006-01-02")
		}
	}
	return f
}

// ---------- 汇总 ----------

func (a *App) handleUsageSummary(w http.ResponseWriter, r *http.Request) {
	f := usageQueryFilter(r)
	pricing := a.loadUsagePricing()
	days, err := a.Store.UsageDailyGroups(f)
	if err != nil {
		writeErr(w, 500, "读取每日用量失败："+err.Error())
		return
	}
	models, err := a.Store.UsageByModel(f)
	if err != nil {
		writeErr(w, 500, "读取模型用量失败："+err.Error())
		return
	}

	resp := usageSummaryResp{Enabled: a.usageEnabled(), From: f.From, To: f.To,
		Daily: []usageDailyPoint{}, ByModel: []usageModelStat{}}

	// 日级：先按天累加 token 与费用（费用逐**分钟桶**按该模型的时段价、逐渠道按
	// 倍率算），再按日期排序成趋势数据
	dayIdx := map[string]*usageDailyPoint{}
	var kpi usageKPI
	kpi.Priced = true
	kpi.RuleReady = pricing.anyRuleConfigured()
	for _, d := range days {
		day := usageDayOf(d.SlotTS)
		p := dayIdx[day]
		if p == nil {
			p = &usageDailyPoint{Day: day, Priced: true}
			dayIdx[day] = p
		}
		p.Input += d.Input
		p.Output += d.Output
		p.CacheRead += d.CacheRead
		p.CacheWrite += d.CacheWrite
		p.Calls += d.Calls
		p.Errors += d.Errors
		if cost, _, ok := pricing.cost(d.Model, d.ProviderID, d.Protocol, d.SlotTS, usageTokens{Input: d.Input, Output: d.Output,
			CacheRead: d.CacheRead, CacheWrite: d.CacheWrite}); ok {
			p.Cost += cost
		} else {
			p.Priced = false
		}
		kpi.Input += d.Input
		kpi.Output += d.Output
		kpi.CacheRead += d.CacheRead
		kpi.CacheWrite += d.CacheWrite
		kpi.Calls += d.Calls
		kpi.Errors += d.Errors
	}
	dayKeys := make([]string, 0, len(dayIdx))
	for day := range dayIdx {
		dayKeys = append(dayKeys, day)
	}
	sort.Strings(dayKeys)
	for _, day := range dayKeys {
		resp.Daily = append(resp.Daily, *dayIdx[day])
	}

	// 模型级：同 (模型, 协议) 的多渠道合并，费用逐渠道算完相加
	type modelKey struct{ model, protocol string }
	mIdx := map[modelKey]*usageModelStat{}
	for _, m := range models {
		k := modelKey{m.Model, m.Protocol}
		s := mIdx[k]
		if s == nil {
			s = &usageModelStat{Model: m.Model, Protocol: m.Protocol, Priced: true}
			mIdx[k] = s
		}
		s.Input += m.Input
		s.Output += m.Output
		s.CacheRead += m.CacheRead
		s.CacheWrite += m.CacheWrite
		s.Calls += m.Calls
		s.Errors += m.Errors
		// 分时段分解按桶累计：一个桶（同一分钟）里的调用必然同档，次数与金额都对得上
		if cost, peak, ok := pricing.cost(m.Model, m.ProviderID, m.Protocol, m.SlotTS,
			usageTokens{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite}); ok {
			s.Cost += cost
			if peak {
				kpi.PeakCalls += m.Calls
				kpi.PeakCost += cost
			} else {
				kpi.IdleCalls += m.Calls
				kpi.IdleCost += cost
			}
		} else {
			s.Priced = false
			kpi.UnpricedCalls += m.Calls
		}
	}
	for _, s := range mIdx {
		s.Total = s.Input + s.Output
		kpi.Cost += s.Cost
		if !s.Priced {
			kpi.Priced = false
		}
		resp.ByModel = append(resp.ByModel, *s)
	}
	sort.Slice(resp.ByModel, func(i, j int) bool {
		if resp.ByModel[i].Total != resp.ByModel[j].Total {
			return resp.ByModel[i].Total > resp.ByModel[j].Total
		}
		return resp.ByModel[i].Model < resp.ByModel[j].Model
	})
	// 搜索/抽取：按次计费，与 token 类各自合计；顶部「总花费」把两边相加
	searches, err := a.Store.UsageSearchGroups(f)
	if err != nil {
		writeErr(w, 500, "读取搜索用量失败："+err.Error())
		return
	}
	for _, g := range searches {
		kpi.SearchCalls += g.Calls
		kpi.SearchCost += float64(g.Success) * pricing.searchPriceOf(g.ProviderID, a.searchKindOf(g.ProviderID))
	}
	kpi.TotalCost = kpi.Cost + kpi.SearchCost

	if kpi.Calls == 0 {
		kpi.Priced = true
	}
	kpi.Total = kpi.Input + kpi.Output
	if kpi.Calls > 0 {
		kpi.SuccessRate = float64(kpi.Calls-kpi.Errors) / float64(kpi.Calls) * 100
	}
	if kpi.Input > 0 {
		kpi.CacheHitRate = float64(kpi.CacheRead) / float64(kpi.Input) * 100
	}
	resp.KPI = kpi
	writeJSON(w, 200, resp)
}

// ---------- 明细分页 ----------

func (a *App) handleUsageRecords(w http.ResponseWriter, r *http.Request) {
	f := usageQueryFilter(r)
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	rows, total, err := a.Store.UsageRecords(f, limit, offset)
	if err != nil {
		writeErr(w, 500, "读取用量记录失败："+err.Error())
		return
	}
	// 渠道名与 Key 备注由后端补上（前端不必再拿配置对一遍）
	names, labels := a.usageChannelNames()
	// 这次调用落在高峰还是空闲：按**该模型自己的规则**判定（规则改过，历史明细的
	// 标记跟着变，与重算出来的金额保持一致）
	pricing := a.loadUsagePricing()
	items := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		items = append(items, map[string]any{
			"ts": r.TS, "sessionId": r.SessionID, "purpose": r.Purpose,
			"providerId": r.ProviderID, "providerName": names[r.ProviderID],
			"keyId": r.KeyID, "keyLabel": labels[r.ProviderID+"\x00"+r.KeyID],
			"model": r.Model, "protocol": r.Protocol, "thinking": r.Thinking,
			"input": r.Input, "output": r.Output, "cacheRead": r.CacheRead, "cacheWrite": r.CacheWrite,
			"total": r.Total, "ttftMs": r.TTFTMs, "durationMs": r.DurationMs,
			"ok": r.OK, "error": r.Error, "stream": r.Stream,
			// 这次调用命中峰值价还是空闲价（按明细 ts 判定），页面 hover 显示
			"peak": pricing.peakAt(r.Model, r.TS),
		})
	}
	writeJSON(w, 200, map[string]any{"total": total, "limit": limit, "offset": offset, "items": items})
}

// usageChannelNames 返回 providerID → 显示名，以及 providerID+KeyID → Key 备注。
func (a *App) usageChannelNames() (map[string]string, map[string]string) {
	names := map[string]string{}
	labels := map[string]string{}
	if a.Cfg == nil {
		return names, labels
	}
	for _, p := range a.Cfg.Providers {
		names[p.ID] = p.Name
		for _, k := range p.KeyList() {
			if k.ID != "" {
				labels[p.ID+"\x00"+k.ID] = k.Label
			}
		}
	}
	return names, labels
}

// ---------- 单价与规则 ----------

func (a *App) usagePricingView() usagePricingResp {
	p := a.loadUsagePricing()
	resp := usagePricingResp{
		Models:    p.modelsSorted(),
		Providers: []usageProviderView{},
		Search:    []usageSearchPriceView{},
		RuleReady: p.anyRuleConfigured(),
		Note: "单价单位：元 / 百万 tokens，由用户自行填写（程序不内置任何模型价，未填的行按「未配单价」处理）；" +
			"费用 = 未命中输入×输入价 + 缓存读取×缓存读价 + 缓存写入×缓存写价 + 输出×输出价，再乘渠道倍率。" +
			"缓存命中的输入不再按输入价重复计费；时段规则按调用发生的时刻判定，未配时段规则＝不分时段（全按高峰价）。" +
			"按本地单价估算，不代表账单。",
	}
	if a.Cfg == nil {
		return resp
	}
	for _, pv := range a.Cfg.Providers {
		keys := []usageProviderKeyView{}
		for _, k := range pv.KeyList() {
			keys = append(keys, usageProviderKeyView{ID: k.ID, Label: k.Label})
		}
		// 已配的模型 id（多 Key 时会重名，去重后给页面「添加空行」用）
		seen := map[string]bool{}
		ids := []string{}
		for _, pm := range pv.AllModels() {
			id := strings.TrimSpace(pm.Model.ID)
			if id == "" || seen[id] {
				continue
			}
			seen[id] = true
			ids = append(ids, id)
		}
		resp.Providers = append(resp.Providers, usageProviderView{
			ID: pv.ID, Name: pv.Name, Multiplier: p.multiplierOf(pv.ID), Keys: keys, Models: ids,
		})
	}
	for _, sv := range a.Cfg.SearchProviders {
		resp.Search = append(resp.Search, usageSearchPriceView{
			ID: sv.ID, Name: sv.displayName(), Kind: sv.Kind,
			Price:   p.searchPriceOf(sv.ID, sv.Kind),
			Default: defaultSearchPrice(sv.Kind),
			Note:    searchUnitPriceNote(sv.Kind),
		})
	}
	return resp
}

func (a *App) handleUsagePricingGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.usagePricingView())
}

func (a *App) handleUsagePricingSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Models       []usagePricingModel `json:"models"`
		Multipliers  map[string]float64  `json:"multipliers"`
		SearchPrices map[string]float64  `json:"searchPrices"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	p := usagePricing{Models: map[string]modelPrice{}, Multipliers: map[string]float64{},
		SearchPrices: map[string]float64{}}
	for _, m := range body.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if m.Input < 0 || m.Output < 0 || m.CacheRead < 0 || m.CacheWrite < 0 {
			writeErr(w, 400, "模型「"+id+"」的单价不能是负数")
			return
		}
		// 时段规则的脏值在 normalize 里统一丢掉（非法时段/日期不会把计费算歪）；
		// 这里先按页面交上来的填，折扣的 (0,1) 归一也在 normalize 里做
		p.Models[id] = modelPrice{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead,
			CacheWrite: m.CacheWrite,
			usagePeakRule: usagePeakRule{
				PeakWindows: m.PeakWindows,
				PeakDays:    m.PeakDays,
				Holidays:    m.Holidays,
				IdleFactor:  m.IdleFactor,
			}}
	}
	for id, mul := range body.Multipliers {
		if mul > 0 {
			p.Multipliers[id] = mul
		}
	}
	for id, price := range body.SearchPrices {
		if id = strings.TrimSpace(id); id != "" && price >= 0 {
			p.SearchPrices[id] = price
		}
	}
	if err := a.saveUsagePricing(p); err != nil {
		writeErr(w, 500, "保存单价配置失败："+err.Error())
		return
	}
	writeJSON(w, 200, a.usagePricingView())
}

// handleUsageSearch 是「搜索调用」分区的数据：KPI + 按供应商 + 明细分页。
// 搜索按次计费，与 token 类用量各自合计（顶部总花费 = 模型费用 + 搜索费用）。
func (a *App) handleUsageSearch(w http.ResponseWriter, r *http.Request) {
	f := usageQueryFilter(r)
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	offset, _ := strconv.Atoi(q.Get("offset"))
	pricing := a.loadUsagePricing()
	groups, err := a.Store.UsageSearchGroups(f)
	if err != nil {
		writeErr(w, 500, "读取搜索用量失败："+err.Error())
		return
	}
	resp := usageSearchResp{
		ByProvider: []usageSearchProviderStat{},
		Items:      []map[string]any{},
		Limit:      limit, Offset: offset,
		Note: "搜索/抽取按「次」计费，与模型调用的 token 花费各自合计；失败调用计数但不计费。" +
			"明细自本次升级起统计（此前的搜索调用没有记录）。",
	}
	type groupKey struct{ id, kind string }
	type acc struct {
		stat usageSearchProviderStat
		dur  int64
	}
	idx := map[groupKey]*acc{}
	var durSum int64
	for _, g := range groups {
		k := groupKey{g.ProviderID, g.Kind}
		e := idx[k]
		if e == nil {
			e = &acc{stat: usageSearchProviderStat{ProviderID: g.ProviderID, Kind: g.Kind,
				Name: a.searchProviderLabel(g.ProviderID)}}
			idx[k] = e
		}
		e.stat.Calls += g.Calls
		e.stat.Success += g.Success
		e.stat.Errors += g.Calls - g.Success
		e.stat.Fallback += g.Fallback
		e.dur += g.DurationMs
		// 单价按供应商 id 配，缺省用类型的公开价；只对成功次数计费
		e.stat.UnitPrice = pricing.searchPriceOf(g.ProviderID, a.searchKindOf(g.ProviderID))
		e.stat.Note = searchUnitPriceNote(a.searchKindOf(g.ProviderID))
		e.stat.Cost += float64(g.Success) * e.stat.UnitPrice

		resp.KPI.Calls += g.Calls
		resp.KPI.Success += g.Success
		resp.KPI.FallbackCalls += g.Fallback
		resp.KPI.Cost += float64(g.Success) * e.stat.UnitPrice
		durSum += g.DurationMs
	}
	for _, e := range idx {
		if e.stat.Calls > 0 {
			e.stat.AvgDurationMs = float64(e.dur) / float64(e.stat.Calls)
		}
		resp.ByProvider = append(resp.ByProvider, e.stat)
	}
	if resp.KPI.Calls > 0 {
		resp.KPI.Errors = resp.KPI.Calls - resp.KPI.Success
		resp.KPI.FallbackRate = float64(resp.KPI.FallbackCalls) / float64(resp.KPI.Calls) * 100
		resp.KPI.AvgDurationMs = float64(durSum) / float64(resp.KPI.Calls)
	}
	sort.Slice(resp.ByProvider, func(i, j int) bool {
		if resp.ByProvider[i].Calls != resp.ByProvider[j].Calls {
			return resp.ByProvider[i].Calls > resp.ByProvider[j].Calls
		}
		return resp.ByProvider[i].ProviderID < resp.ByProvider[j].ProviderID
	})

	rows, total, err := a.Store.UsageSearchRecords(f, limit, offset)
	if err != nil {
		writeErr(w, 500, "读取搜索明细失败："+err.Error())
		return
	}
	for _, r := range rows {
		resp.Items = append(resp.Items, map[string]any{
			"ts": r.TS, "sessionId": r.SessionID,
			"providerId": r.ProviderID, "providerName": a.searchProviderLabel(r.ProviderID),
			"kind": r.Kind, "kindLabel": usageCallKindLabel(r.Kind),
			"detail": r.Detail, "errKind": r.ErrKind,
			"ok": r.OK, "error": r.Error, "durationMs": r.DurationMs, "rank": r.Rank,
		})
	}
	resp.Total = total
	writeJSON(w, 200, resp)
}

// ---------- 清理 ----------

func (a *App) handleUsageCleanup(w http.ResponseWriter, r *http.Request) {
	var body struct {
		From string `json:"from"`
		To   string `json:"to"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	from, to := strings.TrimSpace(body.From), strings.TrimSpace(body.To)
	if from == "" || to == "" {
		writeErr(w, 400, "需要给完整的起止日期（YYYY-MM-DD）")
		return
	}
	if from > to {
		from, to = to, from
	}
	n, err := a.Store.UsageCleanupBetween(from, to)
	if err != nil {
		writeErr(w, 500, "清理用量数据失败："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"deleted": n, "from": from, "to": to})
}
