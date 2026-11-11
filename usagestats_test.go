package main

// 用量统计（usagestats.go / usagepricing.go / usageapi.go）的自检测试：
//   - 记账落库：一次成功调用与一次失败调用的明细字段（用途/渠道/Key/协议/
//     首字延迟/token/错误摘要）
//   - 组件关闭时不记录（开关是采集的唯一门闩）
//   - 按天 / 按模型聚合（多渠道合并、费用逐渠道算）
//   - 计费口径：基础价 × 渠道倍率、未配单价不给 0
//   - 保留策略（超 90 天 / 超 20 万条删最旧）与按日期范围清理
//   - 明细分页（新的在前、总数正确）

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newUsageTestApp 建一个带临时库与组件注册表的 App：把仓库里的 usage-stats
// manifest 复制进临时组件目录（组件开关是采集的门闩，注册表里没有这个组件就
// 等于关闭——测试必须拿真实清单，才能顺带看护组件 id 没写错）。
func newUsageTestApp(t *testing.T) *App {
	t.Helper()
	a := newCompactTestApp(t)
	compDir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("组件", usageComponentID, "manifest.json"))
	if err != nil {
		t.Fatalf("读用量统计组件清单失败: %v", err)
	}
	dst := filepath.Join(compDir, usageComponentID, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, src, 0644); err != nil {
		t.Fatal(err)
	}
	a.Components = InitComponents(compDir, filepath.Join(t.TempDir(), "components.json"))
	if !a.usageEnabled() {
		t.Fatal("装上组件清单后 usageEnabled 应为 true")
	}
	return a
}

// waitUsageRows 等异步落库协程把队列写进库（最多 3 秒）。
func waitUsageRows(t *testing.T, a *App, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if a.Store.UsageCount() >= want {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等落库超时：期望至少 %d 条，实际 %d 条", want, a.Store.UsageCount())
}

// jsonBody 把 map 编成 httptest 请求体（测试里构造 POST body 用）。
func jsonBody(v any) *strings.Reader {
	raw, _ := json.Marshal(v)
	return strings.NewReader(string(raw))
}

// seedUsageRows 直接写明细（聚合/分页/清理类测试用——不经过异步队列）。
func seedUsageRows(t *testing.T, a *App, rows ...usageRow) {
	t.Helper()
	if err := a.Store.AppendUsageRows(rows); err != nil {
		t.Fatalf("写明细失败: %v", err)
	}
}

// TestUsageRecordPersist 看护记账字段口径：埋点包装收首字延迟与流尾 usage，
// 成功与失败都落一条明细。
func TestUsageRecordPersist(t *testing.T) {
	a := newUsageTestApp(t)
	sess := &SessionMeta{ID: "s-usage-1", Mode: "task"}
	target := LLMTarget{ProviderID: "p1", KeyID: "k1", Model: "gpt-4o", Protocol: "anthropic"}

	cb, finish := a.meterUsage(usagePurposeTask, sess, target, StreamCallbacks{
		OnText: func(string) {}, OnUsage: func(Usage) {},
	})
	cb.OnText("你好")
	cb.OnUsage(Usage{PromptTokens: 1000, CompletionTokens: 50,
		CacheReadTokens: 200, CacheCreationTokens: 30})
	finish(nil)

	// 失败调用：没有 usage，也要留一条（成功率要算得出来）
	_, finish2 := a.meterUsage(usagePurposeCompact, nil, target, StreamCallbacks{})
	finish2(errors.New("模型服务返回 401：鉴权失败"))

	waitUsageRows(t, a, 2)
	rows, total, err := a.Store.UsageRecords(usageFilter{}, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("明细条数不对: total=%d err=%v", total, err)
	}
	// 新的在前：失败那条是后写的
	fail, ok := rows[0], rows[1]
	if fail.OK || fail.Purpose != usagePurposeCompact {
		t.Fatalf("失败调用应记 ok=false / 用途=compact，得到 %+v", fail)
	}
	if fail.Error == "" || fail.Model != "gpt-4o" {
		t.Fatalf("失败调用应带错误摘要与模型名，得到 %+v", fail)
	}
	if !ok.OK || ok.Purpose != usagePurposeTask || ok.SessionID != "s-usage-1" {
		t.Fatalf("成功调用字段不对，得到 %+v", ok)
	}
	if ok.Input != 1000 || ok.Output != 50 || ok.CacheRead != 200 || ok.CacheWrite != 30 || ok.Total != 1050 {
		t.Fatalf("token 口径不对，得到 %+v", ok)
	}
	if ok.ProviderID != "p1" || ok.KeyID != "k1" || ok.Protocol != "anthropic" || !ok.Stream {
		t.Fatalf("渠道/Key/协议/流式口径不对，得到 %+v", ok)
	}
	if ok.TTFTMs < 0 || ok.DurationMs < 0 {
		t.Fatalf("耗时不能是负数: %+v", ok)
	}
}

// TestUsageRecordDisabled 组件关掉就停记（已有数据保留，不删）。
func TestUsageRecordDisabled(t *testing.T) {
	a := newUsageTestApp(t)
	if err := a.Components.SetEnabled(usageComponentID, false); err != nil {
		t.Fatal(err)
	}
	if a.usageEnabled() {
		t.Fatal("组件关闭后 usageEnabled 应为 false")
	}
	cb, finish := a.meterUsage(usagePurposeTask, nil, LLMTarget{Model: "m"}, StreamCallbacks{})
	cb.OnText("x")
	finish(nil)
	time.Sleep(500 * time.Millisecond) // 给可能的误落库留出时间窗口
	if n := a.Store.UsageCount(); n != 0 {
		t.Fatalf("组件关闭时不该记录，实际落了 %d 条", n)
	}
	if err := a.Components.SetEnabled(usageComponentID, true); err != nil {
		t.Fatal(err)
	}
	if !a.usageEnabled() {
		t.Fatal("重新打开后 usageEnabled 应为 true")
	}
}

// TestUsageSummaryAggregation 看护按天/按模型聚合与费用口径：同模型多渠道合并
// token、费用按各自渠道倍率算完相加，未配单价的模型标记 priced=false。
func TestUsageSummaryAggregation(t *testing.T) {
	a := newUsageTestApp(t)
	// 自配价表：m1 输入 10 元/M、输出 20 元/M；p2 渠道倍率 2.0
	if err := a.saveUsagePricing(usagePricing{
		Models:      map[string]modelPrice{"m1": {Input: 10, Output: 20, CacheRead: 1, CacheWrite: 2}},
		Multipliers: map[string]float64{"p2": 2},
	}); err != nil {
		t.Fatal(err)
	}
	seedUsageRows(t, a,
		// 11-01：m1 @ p1（倍率 1.0）：输入 1M → 10 元，输出 1M → 20 元
		usageRow{TS: "2026-11-01 10:00:00", Model: "m1", Protocol: "chat", ProviderID: "p1",
			Input: 1000000, Output: 1000000, OK: true},
		// 11-01：m1 @ p2（倍率 2.0）：输入 1M → 20 元，输出 1M → 40 元
		usageRow{TS: "2026-11-01 11:00:00", Model: "m1", Protocol: "chat", ProviderID: "p2",
			Input: 1000000, Output: 1000000, OK: false},
		// 11-02：未配单价的 m2
		usageRow{TS: "2026-11-02 09:00:00", Model: "m2", Protocol: "responses", ProviderID: "p1",
			Input: 500, Output: 500, OK: true},
	)
	rec := httptest.NewRecorder()
	a.handleUsageSummary(rec, httptest.NewRequest("GET", "/api/usage/summary?from=2026-11-01&to=2026-11-02", nil))
	if rec.Code != 200 {
		t.Fatalf("summary 应为 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var resp usageSummaryResp
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.KPI.Calls != 3 || resp.KPI.Errors != 1 {
		t.Fatalf("请求数/错误数不对: %+v", resp.KPI)
	}
	if resp.KPI.Input != 2000500 || resp.KPI.Output != 2000500 || resp.KPI.Total != 4001000 {
		t.Fatalf("总 token 不对: %+v", resp.KPI)
	}
	if got, want := resp.KPI.Cost, 90.0; got < want-0.001 || got > want+0.001 {
		t.Fatalf("估算花费应为 %.2f（10+20+20+40），得到 %.4f", want, got)
	}
	if resp.KPI.Priced {
		t.Fatal("有未配单价的模型时 KPI.Priced 应为 false")
	}
	if resp.KPI.UnpricedCalls != 1 {
		t.Fatalf("未配单价的调用数应为 1，得到 %d", resp.KPI.UnpricedCalls)
	}
	if len(resp.Daily) != 2 {
		t.Fatalf("应有 2 天的趋势数据，得到 %d: %+v", len(resp.Daily), resp.Daily)
	}
	if d := resp.Daily[0]; d.Day != "2026-11-01" || d.Cost < 89.999 || d.Cost > 90.001 {
		t.Fatalf("11-01 费用应为 90 元（渠道倍率 2 生效），得到 %+v", d)
	}
	if resp.Daily[1].Priced {
		t.Fatal("11-02 只有未配单价的模型，Priced 应为 false")
	}
	// 按模型：m1 两条（不同渠道）合并成一行
	var m1 *usageModelStat
	for i := range resp.ByModel {
		if resp.ByModel[i].Model == "m1" {
			m1 = &resp.ByModel[i]
		}
	}
	if m1 == nil {
		t.Fatalf("按模型统计里没有 m1: %+v", resp.ByModel)
	}
	if m1.Input != 2000000 || m1.Calls != 2 || m1.Total != 4000000 {
		t.Fatalf("m1 的两条记录应合并：%+v", *m1)
	}
	if m1.Cost < 89.999 || m1.Cost > 90.001 || !m1.Priced {
		t.Fatalf("m1 费用应为 90 元且已配价：%+v", *m1)
	}
}

// TestUsagePricingCost 看护计费公式与「未配单价」口径。
func TestUsagePricingCost(t *testing.T) {
	p := usagePricing{
		Models:      map[string]modelPrice{"m": {Input: 2, Output: 8, CacheRead: 0.2, CacheWrite: 4}},
		Multipliers: map[string]float64{"relay": 1.5},
	}
	tk := usageTokens{Input: 1000000, Output: 1000000, CacheRead: 1000000, CacheWrite: 1000000}
	// (2+8+0.2+4) = 14.2；直连倍率 1.0
	if got, ok := p.cost("m", "direct", tk); !ok || got < 14.199 || got > 14.201 {
		t.Fatalf("直连费用应为 14.2，得到 %.4f ok=%v", got, ok)
	}
	// 中转站倍率 1.5
	if got, ok := p.cost("m", "relay", tk); !ok || got < 21.299 || got > 21.301 {
		t.Fatalf("倍率 1.5 后应为 21.3，得到 %.4f ok=%v", got, ok)
	}
	// 未配单价：不给 0，返回 ok=false
	if got, ok := p.cost("unknown-model", "direct", tk); ok || got != 0 {
		t.Fatalf("未配单价应返回 ok=false / 0，得到 %.4f ok=%v", got, ok)
	}
	// 倍率缺省 1.0、非法倍率按缺省
	p.Multipliers["zero"] = 0
	if got := p.multiplierOf("zero"); got != 1.0 {
		t.Fatalf("非法倍率应按 1.0，得到 %v", got)
	}
}

// TestUsagePrune 看护保留策略：超龄删、超条数删最旧，保留最新。
func TestUsagePrune(t *testing.T) {
	a := newUsageTestApp(t)
	today := time.Now().Format("2006-01-02")
	old := time.Now().AddDate(0, 0, -100).Format("2006-01-02")
	seedUsageRows(t, a,
		usageRow{TS: old + " 08:00:00", Model: "old", OK: true},
		usageRow{TS: today + " 09:00:00", Model: "r1", OK: true},
		usageRow{TS: today + " 10:00:00", Model: "r2", OK: true},
		usageRow{TS: today + " 11:00:00", Model: "r3", OK: true},
	)
	// 90 天超龄：删掉 100 天前那条
	cutoff := time.Now().AddDate(0, 0, -usageRetentionDays).Format("2006-01-02")
	if n, err := a.Store.UsagePrune(cutoff, 0); err != nil || n != 1 {
		t.Fatalf("超龄清理应删 1 条，得到 n=%d err=%v", n, err)
	}
	// 条数上限 2：再删最旧的 1 条
	if n, err := a.Store.UsagePrune("", 2); err != nil || n != 1 {
		t.Fatalf("超量清理应删 1 条，得到 n=%d err=%v", n, err)
	}
	rows, total, err := a.Store.UsageRecords(usageFilter{}, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("清理后应剩 2 条，得到 total=%d err=%v", total, err)
	}
	// 保留的是最新的两条（新的在前）
	if rows[0].Model != "r3" || rows[1].Model != "r2" {
		t.Fatalf("应保留最新两条（r3/r2），得到 %s/%s", rows[0].Model, rows[1].Model)
	}
	// 空库再跑一次不炸也不删
	if n, err := a.Store.UsagePrune("", 200000); err != nil || n != 0 {
		t.Fatalf("空跑不应删任何东西，得到 n=%d err=%v", n, err)
	}
}

// TestUsageCleanupRange 看护「按日期范围清理」的边界（含首尾、倒序入参）。
func TestUsageCleanupRange(t *testing.T) {
	a := newUsageTestApp(t)
	seedUsageRows(t, a,
		usageRow{TS: "2026-10-01 10:00:00", Model: "a", OK: true},
		usageRow{TS: "2026-10-05 10:00:00", Model: "b", OK: true},
		usageRow{TS: "2026-10-09 10:00:00", Model: "c", OK: true},
	)
	rec := httptest.NewRecorder()
	a.handleUsageCleanup(rec, httptest.NewRequest("POST", "/api/usage/cleanup",
		jsonBody(map[string]any{"from": "2026-10-09", "to": "2026-10-05"})))
	if rec.Code != 200 {
		t.Fatalf("清理应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var out struct{ Deleted int64 }
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.Deleted != 2 {
		t.Fatalf("倒序入参也要删 2 条（10-05 与 10-09），得到 %d", out.Deleted)
	}
	if n := a.Store.UsageCount(); n != 1 {
		t.Fatalf("应剩 1 条，得到 %d", n)
	}
	// 空范围必须被挡住（不能删全表）
	rec = httptest.NewRecorder()
	a.handleUsageCleanup(rec, httptest.NewRequest("POST", "/api/usage/cleanup",
		jsonBody(map[string]any{"from": "", "to": ""})))
	if rec.Code != 400 {
		t.Fatalf("空范围应 400，得到 %d", rec.Code)
	}
	if n := a.Store.UsageCount(); n != 1 {
		t.Fatalf("空范围不能删数据，剩 %d 条", n)
	}
}

// TestUsageRecordsPagination 看护明细分页：新的在前、总数与页大小。
func TestUsageRecordsPagination(t *testing.T) {
	a := newUsageTestApp(t)
	rows := make([]usageRow, 0, 120)
	for i := 0; i < 120; i++ {
		rows = append(rows, usageRow{
			TS: fmt.Sprintf("2026-11-01 %02d:%02d:00", i/60, i%60), Model: fmt.Sprintf("m%03d", i), OK: true,
		})
	}
	seedUsageRows(t, a, rows...)

	page, total, err := a.Store.UsageRecords(usageFilter{}, 50, 0)
	if err != nil || total != 120 || len(page) != 50 {
		t.Fatalf("第一页应 50 条 / 总 120，得到 %d/%d err=%v", len(page), total, err)
	}
	if page[0].Model != "m119" || page[1].Model != "m118" {
		t.Fatalf("明细应按时间倒序（最新在前），得到 %s/%s", page[0].Model, page[1].Model)
	}
	last, _, err := a.Store.UsageRecords(usageFilter{}, 50, 100)
	if err != nil || len(last) != 20 {
		t.Fatalf("第三页应 20 条，得到 %d err=%v", len(last), err)
	}
	if last[len(last)-1].Model != "m000" {
		t.Fatalf("最后一页末尾应是最旧的 m000，得到 %s", last[len(last)-1].Model)
	}
	// 超大 limit 被钳到单页上限（防一次拉爆）：给足 600 条再看第一页长度
	more := make([]usageRow, 0, 600)
	for i := 0; i < 600; i++ {
		more = append(more, usageRow{TS: "2026-11-02 00:00:00", Model: fmt.Sprintf("x%03d", i), OK: true})
	}
	seedUsageRows(t, a, more...)
	big, bigTotal, err := a.Store.UsageRecords(usageFilter{}, 100000, 0)
	if err != nil || bigTotal != 720 || len(big) != usageRecordPageMax {
		t.Fatalf("limit 应钳到 %d，得到 %d（总 %d）err=%v", usageRecordPageMax, len(big), bigTotal, err)
	}
}
