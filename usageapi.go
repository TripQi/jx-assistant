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
	Cost          float64 `json:"cost"`          // 已配单价部分的估算花费（元）
	Priced        bool    `json:"priced"`        // 是否全部模型都配了单价
	UnpricedCalls int64   `json:"unpricedCalls"` // 未配单价的调用数
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
}

type usagePricingResp struct {
	Models    []usagePricingModel `json:"models"`
	Providers []usageProviderView `json:"providers"`
	Note      string              `json:"note"`
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

	// 日级：先按天累加 token 与费用（费用逐渠道算），再按日期排序成趋势数据
	dayIdx := map[string]*usageDailyPoint{}
	var kpi usageKPI
	kpi.Priced = true
	for _, d := range days {
		p := dayIdx[d.Day]
		if p == nil {
			p = &usageDailyPoint{Day: d.Day, Priced: true}
			dayIdx[d.Day] = p
		}
		p.Input += d.Input
		p.Output += d.Output
		p.CacheRead += d.CacheRead
		p.CacheWrite += d.CacheWrite
		p.Calls += d.Calls
		p.Errors += d.Errors
		if cost, ok := pricing.cost(d.Model, d.ProviderID, usageTokens{Input: d.Input, Output: d.Output,
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
		if cost, ok := pricing.cost(m.Model, m.ProviderID, usageTokens{Input: m.Input, Output: m.Output,
			CacheRead: m.CacheRead, CacheWrite: m.CacheWrite}); ok {
			s.Cost += cost
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

// ---------- 单价与倍率 ----------

func (a *App) usagePricingView() usagePricingResp {
	p := a.loadUsagePricing()
	resp := usagePricingResp{
		Models:    p.modelsSorted(),
		Providers: []usageProviderView{},
		Note:      "单价单位：元 / 百万 tokens；费用 = 基础价算出的金额 × 渠道倍率。按本地单价估算，不代表账单。",
	}
	if a.Cfg == nil {
		return resp
	}
	for _, pv := range a.Cfg.Providers {
		keys := []usageProviderKeyView{}
		for _, k := range pv.KeyList() {
			keys = append(keys, usageProviderKeyView{ID: k.ID, Label: k.Label})
		}
		resp.Providers = append(resp.Providers, usageProviderView{
			ID: pv.ID, Name: pv.Name, Multiplier: p.multiplierOf(pv.ID), Keys: keys,
		})
	}
	return resp
}

func (a *App) handleUsagePricingGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, a.usagePricingView())
}

func (a *App) handleUsagePricingSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Models      []usagePricingModel `json:"models"`
		Multipliers map[string]float64  `json:"multipliers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	p := usagePricing{Models: map[string]modelPrice{}, Multipliers: map[string]float64{}}
	for _, m := range body.Models {
		id := strings.TrimSpace(m.ID)
		if id == "" {
			continue
		}
		if m.Input < 0 || m.Output < 0 || m.CacheRead < 0 || m.CacheWrite < 0 {
			writeErr(w, 400, "模型「"+id+"」的单价不能是负数")
			return
		}
		p.Models[id] = modelPrice{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheWrite: m.CacheWrite}
	}
	for id, mul := range body.Multipliers {
		if mul > 0 {
			p.Multipliers[id] = mul
		}
	}
	if err := a.saveUsagePricing(p); err != nil {
		writeErr(w, 500, "保存单价配置失败："+err.Error())
		return
	}
	writeJSON(w, 200, a.usagePricingView())
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
