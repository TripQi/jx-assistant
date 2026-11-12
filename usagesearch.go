package main

// usagesearch.go —— 联网搜索/抽取调用的记账与「搜索调用」视图。
//
// 为什么与 token 类用量分开记：搜索不是按 token 计费，而是**按次**（各家公开价
// 元/千次或免费额度），塞进 token 模型里算出来的钱没有意义、还会把 token 合计算脏。
// 所以明细表用 kind 列区分调用类型（search / extract / 空串=模型调用），token 列
// 对搜索行恒为 0；页面分两块展示、各自合计，顶部「总花费」再把两边相加并注明构成。
//
// 记什么：一次**回落的尝试**记一行（第 1 家失败一行、第 2 家成功一行）——只有这样
// 「失败率」「回落率」才有意义。字段：供应商、能力位（kind）、目标摘要（搜索词或
// URL 截断）、成功/失败、失败类别、耗时、顺位（第几家）。绝不记 Key，也不存全文
// 查询词（只留截断摘要，便于排查又不把敏感内容原样落库）。
//
// 计费：费用 = 成功次数 × 该供应商单价；失败调用计数但不计费（用户定的口径）。
// 单价按供应商 id 可配、按类型有默认值（usagepricing.go 的 searchPriceOf）。

import (
	"errors"
	"strings"
	"time"
)

const usageDetailMaxRunes = 200 // 搜索词/URL 摘要的截断长度

// usageDetailSummary 目标摘要：压掉换行与多余空白、截断到固定长度。
func usageDetailSummary(s string) string {
	return truncateRunes(strings.Join(strings.Fields(s), " "), usageDetailMaxRunes)
}

// usageSearchErrKind 失败类别（与 websearch_providers.go 的分类同一套，页面按它
// 分组看「哪家总在限流、哪家 Key 不对」）。不是 providerError 的（总预算耗尽、
// 上下文取消）统一记「连不上」。
func usageSearchErrKind(err error) string {
	var pe *providerError
	if errors.As(err, &pe) {
		return pe.KindText
	}
	return searchErrNetwork.label()
}

// recordSearchCall 记一次搜索/抽取的供应商尝试（回落时每家各记一行）。
// 只入队不写库，与模型调用共用同一套异步队列——搜索本身也要快，统计绝不能拖慢它；
// 组件关闭时 recordUsage 直接丢弃（与已有行为一致）。
func (a *App) recordSearchCall(kind, sessID string, p activeSearchProvider, ok bool, err error, detail string, start time.Time) {
	if p.cfg.ID == "" {
		return
	}
	row := usageRow{
		TS:         now(),
		SessionID:  sessID,
		Purpose:    usagePurposeSearch,
		ProviderID: p.cfg.ID,
		Kind:       kind,
		Detail:     usageDetailSummary(detail),
		Rank:       p.rank,
		DurationMs: time.Since(start).Milliseconds(),
		OK:         ok,
		Stream:     false,
	}
	if err != nil {
		row.Error = truncateRunes(strings.Join(strings.Fields(err.Error()), " "), usageErrorMaxRunes)
		row.ErrKind = usageSearchErrKind(err)
	}
	a.recordUsage(row)
}

// ---------- 视图 ----------

// usageSearchKPI 是「搜索调用」分区的 KPI。
type usageSearchKPI struct {
	Calls         int64   `json:"calls"`         // 供应商尝试总次数（含失败与回落）
	Success       int64   `json:"success"`       // 成功次数（计费依据）
	Errors        int64   `json:"errors"`        // 失败次数
	FallbackCalls int64   `json:"fallbackCalls"` // 顺位 >1 的尝试次数（前面几家失败过）
	FallbackRate  float64 `json:"fallbackRate"`  // 回落次数 / 总次数，0-100
	AvgDurationMs float64 `json:"avgDurationMs"` // 平均耗时
	Cost          float64 `json:"cost"`          // 估算搜索费用（元）
}

// usageSearchProviderStat 是「按供应商」的一行（同供应商的搜索与抽取分开合计）。
type usageSearchProviderStat struct {
	ProviderID    string  `json:"providerId"`
	Name          string  `json:"name"`
	Kind          string  `json:"kind"` // search | extract
	Calls         int64   `json:"calls"`
	Success       int64   `json:"success"`
	Errors        int64   `json:"errors"`
	Fallback      int64   `json:"fallback"`
	AvgDurationMs float64 `json:"avgDurationMs"`
	UnitPrice     float64 `json:"unitPrice"` // 元/次（成功才计）
	Cost          float64 `json:"cost"`
	Note          string  `json:"note,omitempty"` // 「免费额度内」「公司统一计费」等
}

type usageSearchResp struct {
	KPI        usageSearchKPI            `json:"kpi"`
	ByProvider []usageSearchProviderStat `json:"byProvider"`
	Items      []map[string]any          `json:"items"`
	Total      int                       `json:"total"`
	Limit      int                       `json:"limit"`
	Offset     int                       `json:"offset"`
	Note       string                    `json:"note"`
}

// searchKindOf 搜索供应商 id → 类型 kind（供应商被删掉后返回空串：单价退回默认 0）。
func (a *App) searchKindOf(providerID string) string {
	if a == nil || a.Cfg == nil {
		return ""
	}
	for _, p := range a.Cfg.SearchProviders {
		if p.ID == providerID {
			return p.Kind
		}
	}
	return ""
}

// searchProviderLabel 搜索供应商 id → 显示名（页面不用再拿配置对一遍）。
func (a *App) searchProviderLabel(providerID string) string {
	if a == nil || a.Cfg == nil {
		return providerID
	}
	for _, p := range a.Cfg.SearchProviders {
		if p.ID == providerID {
			return p.displayName()
		}
	}
	return providerID
}

// searchUnitPriceNote 单价的说明文字：0 元的那几家要写清「为什么是 0」，免得
// 被当成「没配价」。
func searchUnitPriceNote(kind string) string {
	switch kind {
	case spKindTavily:
		return "免费 1000 credits/月，额度内不计费"
	case spKindAnySearch:
		return "免费 1000 次/天，额度内不计费"
	case spKindGateway:
		return "公司统一计费，客户端不单独算"
	}
	return ""
}

// searchKindLabelOf 能力位的中文名（搜索 / 抽取）。
func usageCallKindLabel(kind string) string {
	if kind == usageKindExtract {
		return "抽取"
	}
	return "搜索"
}
