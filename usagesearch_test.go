package main

// 搜索/抽取调用记账的自检测试（2026-11-12 新增的需求）：
//   - 一次回落尝试各记一行（第 1 家失败一行、第 2 家成功一行），顺位与失败类别可查；
//   - 查询词/URL 只存截断摘要，不存全文、不记 Key；
//   - token 类明细与合计不被搜索行污染（搜索行 token 恒 0）；
//   - 失败计数不计费、成功按供应商单价计费；
//   - 搜索费用与模型费用能正确相加（顶部总花费 = token 费用 + 搜索费用）。

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newSearchUsageApp 装好两家搜索供应商（DeepSeek 第 1、Tavily 第 2）的测试 App。
func newSearchUsageApp(t *testing.T) (*App, activeSearchProvider, activeSearchProvider) {
	t.Helper()
	a := newUsageTestApp(t)
	a.Cfg.SearchProviders = []SearchProviderCfg{
		{ID: "ds", Name: "DeepSeek", Kind: spKindDeepSeek, Order: 1},
		{ID: "tv", Name: "Tavily", Kind: spKindTavily, Order: 2},
	}
	first := activeSearchProvider{cfg: a.Cfg.SearchProviders[0], rank: 1}
	second := activeSearchProvider{cfg: a.Cfg.SearchProviders[1], rank: 2}
	return a, first, second
}

// TestUsageSearchFallbackRows 看护「每次回落尝试各一行」与明细字段。
func TestUsageSearchFallbackRows(t *testing.T) {
	a, first, second := newSearchUsageApp(t)
	longQuery := strings.Repeat("长", usageDetailMaxRunes+120) // 超长查询词要被截断
	// 第 1 家被限流 → 回落；第 2 家成功
	a.recordSearchCall(usageKindSearch, "sess-1", first, false,
		newProviderError(first, 429, searchErrRateLimit, "rate limited"), longQuery, time.Now())
	a.recordSearchCall(usageKindSearch, "sess-1", second, true, nil, longQuery, time.Now())
	// 抽取也记（能力位分开，便于分辨搜索与抽取的花销）
	a.recordSearchCall(usageKindExtract, "", first, true, nil, "https://example.com/a?token=secret", time.Now())
	waitUsageRows(t, a, 3)

	rows, total, err := a.Store.UsageSearchRecords(usageFilter{}, 10, 0)
	if err != nil || total != 3 {
		t.Fatalf("应有 3 条搜索/抽取明细，得到 total=%d err=%v", total, err)
	}
	// 新的在前：抽取、成功回落、失败
	ext, ok2, fail := rows[0], rows[1], rows[2]
	if ext.Kind != usageKindExtract || !ext.OK || ext.ProviderID != "ds" {
		t.Fatalf("抽取行字段不对：%+v", ext)
	}
	if ext.Detail != usageDetailSummary("https://example.com/a?token=secret") {
		t.Fatalf("抽取行应记 URL 摘要，得到 %q", ext.Detail)
	}
	if ok2.Kind != usageKindSearch || !ok2.OK || ok2.Rank != 2 || ok2.ProviderID != "tv" {
		t.Fatalf("回落到第 2 家成功的那行字段不对：%+v", ok2)
	}
	if fail.Kind != usageKindSearch || fail.OK || fail.Rank != 1 || fail.ErrKind != searchErrRateLimit.label() {
		t.Fatalf("第 1 家失败那行字段不对：%+v", fail)
	}
	// 查询词只存截断摘要（截断后带省略号，共 max+1 个字符）
	if got := []rune(fail.Detail); len(got) != usageDetailMaxRunes+1 || got[len(got)-1] != '…' {
		t.Fatalf("超长查询词应截断到 %d 字 + 省略号，得到 %d 字", usageDetailMaxRunes, len(got))
	}
	// 搜索行不许带 token，也不许进 token 明细
	for _, r := range rows {
		if r.Input != 0 || r.Output != 0 || r.CacheRead != 0 || r.CacheWrite != 0 || r.Total != 0 {
			t.Fatalf("搜索/抽取明细的 token 必须为 0：%+v", r)
		}
		if r.SessionID != "" && r.SessionID != "sess-1" {
			t.Fatalf("会话标识不该被改写：%+v", r)
		}
	}
	if _, n, err := a.Store.UsageRecords(usageFilter{}, 10, 0); err != nil || n != 0 {
		t.Fatalf("token 明细表不该含搜索行，得到 total=%d err=%v", n, err)
	}
	// 失败类别与错误摘要都能回查
	if fail.Error == "" || !strings.Contains(fail.Error, "被限流") {
		t.Fatalf("失败行应带错误摘要，得到 %q", fail.Error)
	}
}

// TestUsageSearchCostAndTotal 看护按次计费与「总花费 = 模型 + 搜索」的相加口径。
func TestUsageSearchCostAndTotal(t *testing.T) {
	a, first, second := newSearchUsageApp(t)
	// 模型价：m 输入 2 元/M、输出 8 元/M（未配空闲折扣）；搜索单价用各家默认价
	if err := a.saveUsagePricing(usagePricing{
		Models: map[string]modelPrice{"m": {Input: 2, Output: 8}},
	}); err != nil {
		t.Fatal(err)
	}
	// 搜索：DeepSeek 成功 2 次、失败 1 次；Tavily 回落到第 2 家后成功 1 次
	a.recordSearchCall(usageKindSearch, "s", first, true, nil, "q1", time.Now())
	a.recordSearchCall(usageKindSearch, "s", first, true, nil, "q2", time.Now())
	a.recordSearchCall(usageKindSearch, "s", first, false,
		newProviderError(first, 500, searchErrUpstream, "boom"), "q3", time.Now())
	a.recordSearchCall(usageKindSearch, "s", second, true, nil, "q3", time.Now())
	waitUsageRows(t, a, 4)
	// 一条模型调用：输入 1M（未命中，chat 协议无缓存字段）→ 2 元
	seedUsageRows(t, a, usageRow{TS: now(), Model: "m", Protocol: "chat", ProviderID: "p1",
		Input: 1000000, OK: true})

	// ---- 搜索分区接口 ----
	rec := httptest.NewRecorder()
	a.handleUsageSearch(rec, httptest.NewRequest("GET", "/api/usage/search?limit=10", nil))
	if rec.Code != 200 {
		t.Fatalf("搜索统计应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var sr usageSearchResp
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.KPI.Calls != 4 || sr.KPI.Success != 3 || sr.KPI.Errors != 1 {
		t.Fatalf("搜索 KPI 次数不对：%+v", sr.KPI)
	}
	if sr.KPI.FallbackCalls != 1 || sr.KPI.FallbackRate < 24.99 || sr.KPI.FallbackRate > 25.01 {
		t.Fatalf("回落次数/回落率不对（1/4 = 25%%）：%+v", sr.KPI)
	}
	// 只对成功次数计费：DeepSeek 2 次 × 0.005 = 0.01；Tavily 默认 0（免费额度内）
	if sr.KPI.Cost < 0.0099 || sr.KPI.Cost > 0.0101 {
		t.Fatalf("搜索费用应为 0.01 元，得到 %.4f", sr.KPI.Cost)
	}
	if len(sr.ByProvider) != 2 {
		t.Fatalf("按供应商应有 2 行，得到 %d：%+v", len(sr.ByProvider), sr.ByProvider)
	}
	var ds, tv *usageSearchProviderStat
	for i := range sr.ByProvider {
		switch sr.ByProvider[i].ProviderID {
		case "ds":
			ds = &sr.ByProvider[i]
		case "tv":
			tv = &sr.ByProvider[i]
		}
	}
	if ds == nil || tv == nil {
		t.Fatalf("按供应商缺少 ds/tv：%+v", sr.ByProvider)
	}
	if ds.Calls != 3 || ds.Success != 2 || ds.Errors != 1 || ds.Fallback != 0 ||
		ds.UnitPrice != 0.005 || ds.Cost < 0.0099 || ds.Cost > 0.0101 {
		t.Fatalf("DeepSeek 行不对：%+v", *ds)
	}
	if tv.Calls != 1 || tv.Success != 1 || tv.Fallback != 1 || tv.Cost != 0 {
		t.Fatalf("Tavily 行不对（应有 1 次回落、免费额度内 0 元）：%+v", *tv)
	}
	if len(sr.Items) != 4 || sr.Total != 4 {
		t.Fatalf("搜索明细应有 4 条，得到 %d/%d", len(sr.Items), sr.Total)
	}

	// ---- 总览：模型费用 + 搜索费用 = 总花费，token 合计不含搜索 ----
	rec = httptest.NewRecorder()
	a.handleUsageSummary(rec, httptest.NewRequest("GET", "/api/usage/summary", nil))
	if rec.Code != 200 {
		t.Fatalf("summary 应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var sum usageSummaryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &sum); err != nil {
		t.Fatal(err)
	}
	if sum.KPI.Calls != 1 || sum.KPI.Input != 1000000 || sum.KPI.Total != 1000000 {
		t.Fatalf("token 类合计被搜索行污染了：%+v", sum.KPI)
	}
	if sum.KPI.Cost < 1.9999 || sum.KPI.Cost > 2.0001 {
		t.Fatalf("模型费用应为 2 元，得到 %.4f", sum.KPI.Cost)
	}
	if sum.KPI.SearchCalls != 4 || sum.KPI.SearchCost < 0.0099 || sum.KPI.SearchCost > 0.0101 {
		t.Fatalf("搜索费用汇总不对：%+v", sum.KPI)
	}
	if sum.KPI.TotalCost < 2.0099 || sum.KPI.TotalCost > 2.0101 {
		t.Fatalf("总花费应为 2 + 0.01 = 2.01 元，得到 %.4f", sum.KPI.TotalCost)
	}

	// ---- 自定义搜索单价：按供应商 id 覆盖，历史重算跟着变 ----
	if err := a.saveUsagePricing(usagePricing{
		Models:       map[string]modelPrice{"m": {Input: 2, Output: 8}},
		SearchPrices: map[string]float64{"tv": 0.02},
	}); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	a.handleUsageSearch(rec, httptest.NewRequest("GET", "/api/usage/search?limit=1", nil))
	if err := json.Unmarshal(rec.Body.Bytes(), &sr); err != nil {
		t.Fatal(err)
	}
	if sr.KPI.Cost < 0.0299 || sr.KPI.Cost > 0.0301 {
		t.Fatalf("改价后费用应为 0.01 + 0.02 = 0.03 元，得到 %.4f", sr.KPI.Cost)
	}
}

// TestUsageSearchComponentOff 组件关闭时不记搜索调用（与模型调用同一门闩）。
func TestUsageSearchComponentOff(t *testing.T) {
	a, first, _ := newSearchUsageApp(t)
	if err := a.Components.SetEnabled(usageComponentID, false); err != nil {
		t.Fatal(err)
	}
	a.recordSearchCall(usageKindSearch, "", first, true, nil, "q", time.Now())
	time.Sleep(500 * time.Millisecond)
	if n := a.Store.UsageCount(); n != 0 {
		t.Fatalf("组件关闭时不该记搜索调用，实际落了 %d 条", n)
	}
}
