package main

// 计费口径的自检测试（2026-11-12 修正虚高后新增）：
//   - OpenAI 系输入已含缓存命中：未命中输入 = 输入 − 缓存读取，不得重复计费；
//   - Anthropic 原生输入不含缓存读写：不许做减法；
//   - 缓存写入按「缓存写价」计；
//   - 峰值价 / 空闲价两档各自正确（空闲 = 峰值 × 折扣）；
//   - 周末与法定节假日全天按空闲，时段边界（9/12/14/18 点）按官方口径；
//   - 不分时段的模型不受时段影响。

import "testing"

// TestUsagePricingInputNoDoubleCharge 看护主因修复：OpenAI 系（chat/responses）与
// DeepSeek 的 prompt_tokens 已包含缓存命中，未命中只能算 Input−CacheRead。
// 用今天实测的量级（输入 1000 万、命中 999.9 万、输出 50 万）看金额是否落在合理区间。
func TestUsagePricingInputNoDoubleCharge(t *testing.T) {
	p := usagePricing{Models: map[string]modelPrice{
		"deepseek-flash": {Input: 2, Output: 8, CacheRead: 0.04, CacheWrite: 2},
	}}
	tk := usageTokens{Input: 10000000, CacheRead: 9999000, Output: 500000}
	peak := "2026-11-12 10:00:00" // 周四上午（模型未配空闲折扣，时段不影响价）
	got, ok := p.cost("deepseek-flash", "deepseek", "chat", peak, tk)
	if !ok {
		t.Fatal("应能算出费用")
	}
	// 未命中 1000 × 2/1M = 0.002；命中 9,999,000 × 0.04/1M = 0.39996；输出 500,000 × 8/1M = 4
	want := 0.002 + 0.39996 + 4
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("chat 协议费用应为 %.5f 元（未命中只算 1000 token），得到 %.5f", want, got)
	}
	// 旧口径（输入全额 × 输入价再叠加缓存价）会是 20 + 0.4 + 4 = 24.4 元：这里再确认没走那条路
	if got > 5 {
		t.Fatalf("费用 %.2f 元虚高：缓存命中的输入被重复计费了", got)
	}
	// 缓存读取比输入还大（脏数据）时不许出负数
	if _, ok := p.cost("deepseek-flash", "deepseek", "chat", peak,
		usageTokens{Input: 100, CacheRead: 500}); !ok {
		t.Fatal("脏数据也要给个金额（未命中钳到 0）")
	}
	if spl := splitUsageTokens("responses", usageTokens{Input: 100, CacheRead: 500}); spl.Missed != 0 {
		t.Fatalf("未命中不能为负，得到 %d", spl.Missed)
	}
}

// TestUsagePricingAnthropicCacheSeparate 看护协议差异：Anthropic 的 input_tokens
// 不含缓存读写（单列在 cache_read/cache_creation），做减法会变成少收钱。
func TestUsagePricingAnthropicCacheSeparate(t *testing.T) {
	p := usagePricing{Models: map[string]modelPrice{
		"claude-x": {Input: 2, Output: 8, CacheRead: 0.2, CacheWrite: 4},
	}}
	tk := usageTokens{Input: 1000000, CacheRead: 1000000, Output: 0}
	got, ok := p.cost("claude-x", "p", "anthropic", "2026-11-12 10:00:00", tk)
	if !ok {
		t.Fatal("应能算出费用")
	}
	// 未命中 1M × 2 = 2；缓存读取 1M × 0.2 = 0.2 → 2.2 元（chat 口径会是 0.2 元）
	if got < 2.1999 || got > 2.2001 {
		t.Fatalf("Anthropic 费用应为 2.2 元（输入 1M 全部算未命中 + 缓存读取），得到 %.4f", got)
	}
	if spl := splitUsageTokens("anthropic", tk); spl.Missed != 1000000 {
		t.Fatalf("Anthropic 未命中应为输入全量 1M，得到 %d", spl.Missed)
	}
	// 空串协议（老数据/未标协议）按 OpenAI 兼容口径（含缓存）
	if usageInputIncludesCache("") != true || usageInputIncludesCache("chat") != true ||
		usageInputIncludesCache("responses") != true || usageInputIncludesCache("anthropic") != false {
		t.Fatal("协议口径判定不对")
	}
}

// TestUsagePricingCacheWrite 看护缓存写入按「缓存写价」计（Anthropic 的
// cache_creation 是单列量，既不是输入也不是读取）。
func TestUsagePricingCacheWrite(t *testing.T) {
	p := usagePricing{Models: map[string]modelPrice{
		"claude-x": {Input: 2, Output: 8, CacheRead: 0.2, CacheWrite: 4},
	}}
	got, ok := p.cost("claude-x", "p", "anthropic", "2026-11-12 10:00:00",
		usageTokens{CacheWrite: 1000000})
	if !ok || got < 3.9999 || got > 4.0001 {
		t.Fatalf("缓存写入 1M × 4 元应为 4 元，得到 %.4f ok=%v", got, ok)
	}
	// chat 协议下同样按写价计（与输入口径无关）
	got2, _ := p.cost("claude-x", "p", "chat", "2026-11-12 10:00:00",
		usageTokens{CacheWrite: 1000000})
	if got2 < 3.9999 || got2 > 4.0001 {
		t.Fatalf("chat 协议下缓存写入也应 4 元，得到 %.4f", got2)
	}
}

// TestUsagePricingPeakIdle 看护两档价：空闲 = 峰值 × 折扣，且按调用时刻取价。
func TestUsagePricingPeakIdle(t *testing.T) {
	p := usagePricing{Models: map[string]modelPrice{
		// DeepSeek flash 官方峰值价 + 空闲半价
		"deepseek-flash": {Input: 2, Output: 8, CacheRead: 0.04, CacheWrite: 2, IdleFactor: 0.5},
	}}
	// 输入 1M 未命中 + 输出 1M：峰值 = 2 + 8 = 10 元；空闲 = 5 元
	tk := usageTokens{Input: 1000000, Output: 1000000}
	cost := func(ts string) float64 {
		v, ok := p.cost("deepseek-flash", "deepseek", "chat", ts, tk)
		if !ok {
			t.Fatalf("%s 应能算出费用", ts)
		}
		return v
	}
	peak := cost("2026-11-12 10:30:00") // 周四上午
	if peak < 9.9999 || peak > 10.0001 {
		t.Fatalf("高峰应为 10 元，得到 %.4f", peak)
	}
	idle := cost("2026-11-12 08:30:00") // 周四早上（9 点前）
	if idle < 4.9999 || idle > 5.0001 {
		t.Fatalf("空闲应为 5 元（半价），得到 %.4f", idle)
	}
	// 边界：12:00–14:00 午休、18:00 起都算空闲；9/14 点整进高峰
	cases := map[string]float64{
		"2026-11-12 08:59:59": 5,
		"2026-11-12 09:00:00": 10,
		"2026-11-12 11:59:59": 10,
		"2026-11-12 12:00:00": 5,
		"2026-11-12 13:59:59": 5,
		"2026-11-12 14:00:00": 10,
		"2026-11-12 17:59:59": 10,
		"2026-11-12 18:00:00": 5,
		"2026-11-12 23:30:00": 5,
	}
	for ts, want := range cases {
		if got := cost(ts); got < want-0.0001 || got > want+0.0001 {
			t.Fatalf("%s 应为 %.1f 元，得到 %.4f", ts, want, got)
		}
	}
	// 不分时段的模型（IdleFactor 缺省）全天同价
	p2 := usagePricing{Models: map[string]modelPrice{"m": {Input: 2, Output: 8}}}
	a, _ := p2.cost("m", "p", "chat", "2026-11-12 10:00:00", tk)
	b, _ := p2.cost("m", "p", "chat", "2026-11-12 03:00:00", tk)
	if a != b {
		t.Fatalf("未配空闲折扣的模型不该分时段：高峰 %.4f vs 空闲 %.4f", a, b)
	}
	// 折扣非法（>1 / 负数）按「不分时段」处理
	p3 := usagePricing{Models: map[string]modelPrice{"m": {Input: 2, IdleFactor: 1.5}}}
	p3.normalize()
	if p3.Models["m"].IdleFactor != 0 {
		t.Fatalf("折扣 >1 应归一为「不分时段」，得到 %v", p3.Models["m"].IdleFactor)
	}
}

// TestUsagePeakWeekendAndHoliday 看护时段判定的日历部分：周末与法定节假日全天空闲。
func TestUsagePeakWeekendAndHoliday(t *testing.T) {
	peak := map[string]bool{
		"2026-11-12 10:00:00": true,  // 周四上午
		"2026-11-12 15:00:00": true,  // 周四下午
		"2026-11-14 10:00:00": false, // 周六
		"2026-11-15 15:00:00": false, // 周日
		"2026-10-02 10:00:00": false, // 国庆假期（周五）
		"2026-05-01 10:00:00": false, // 劳动节（周五）
		"2026-09-25 10:00:00": false, // 中秋（周五）
		"2026-02-17 10:00:00": false, // 春节（周二）
		"2026-01-02 10:00:00": false, // 元旦假期（周五）
		"2026-10-08 10:00:00": true,  // 国庆后第一个工作日（周四）
		"2026-11-13 10:00:00": true,  // 周五
	}
	for ts, want := range peak {
		if got := usagePeakAt(ts); got != want {
			t.Fatalf("%s 高峰判定应为 %v，得到 %v", ts, want, got)
		}
	}
	// 解析不出来的时间戳按高峰（宁可高估不低估）
	if !usagePeakAt("") || !usagePeakAt("坏数据") {
		t.Fatal("空/脏时间戳应按高峰兜底")
	}
	// 节假日表按区间存：2026 春节首尾都要命中
	if !usageIsHoliday("2026-02-15") || !usageIsHoliday("2026-02-23") || usageIsHoliday("2026-02-24") {
		t.Fatal("春节区间边界判定不对")
	}
	// 表里没有的年份整年按周一至周五：2027-01-01（周五）会被当高峰（已知可接受的偏差）
	if usageIsHoliday("2027-01-01") {
		t.Fatal("未配置年份不应命中午节假日表")
	}
}
