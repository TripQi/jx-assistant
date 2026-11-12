package main

// 计费口径的自检测试（2026-11-12 两次修正后重写）：
//   - 输入口径：OpenAI 系输入已含缓存命中（未命中 = 输入 − 缓存读取），Anthropic 不做减法；
//   - 时段：高峰/空闲**分别计价再相加**、分钟级边界（8:59/9:00…17:59/18:00）、
//     周末与节假日全天空闲、跨天时段、未配规则＝不分时段；
//   - 价格：未配单价（空表、或一行四个价全 0）不产生费用，页面才有「未配单价」；
//   - 时间戳：明细（秒）、分钟桶（到分）、老版本小时桶都能按对的时刻判定
//     ——2026-11-12 「942 次调用全按高峰价」的根因就是小时桶时间戳解析失败后
//     走了「按高峰兜底」的分支。

import (
	"testing"
	"time"
)

// dsRule 是 DeepSeek 官方的时段规则（周一至周五 9:00–12:00、14:00–18:00，空闲半价）。
// 测试用它是因为它就是用户实际要配的那条规则。
func dsRule() usagePeakRule {
	return usagePeakRule{
		PeakWindows: []string{"09:00-12:00", "14:00-18:00"},
		PeakDays:    []int{1, 2, 3, 4, 5}, // 周一到周五
		IdleFactor:  0.5,
	}
}

// dsPricing 一份「用户自己配的」DeepSeek flash 价（峰值价 元/百万 tokens）。
// 程序已不内置任何单价，测试里也照用户的填法写。
func dsPricing(rule usagePeakRule) usagePricing {
	return usagePricing{Models: map[string]modelPrice{
		"deepseek-flash": {Input: 2, Output: 8, CacheRead: 0.04, CacheWrite: 2, usagePeakRule: rule},
	}}
}

// TestUsagePricingInputNoDoubleCharge 看护主因修复：OpenAI 系（chat/responses）与
// DeepSeek 的 prompt_tokens 已包含缓存命中，未命中只能算 Input−CacheRead。
func TestUsagePricingInputNoDoubleCharge(t *testing.T) {
	p := dsPricing(usagePeakRule{}) // 不分时段，专看输入口径
	tk := usageTokens{Input: 10000000, CacheRead: 9999000, Output: 500000}
	got, _, ok := p.cost("deepseek-flash", "deepseek", "chat", "2026-11-12 10:00:00", tk)
	if !ok {
		t.Fatal("应能算出费用")
	}
	// 未命中 1000 × 2/1M = 0.002；命中 9,999,000 × 0.04/1M = 0.39996；输出 500,000 × 8/1M = 4
	want := 0.002 + 0.39996 + 4
	if diff := got - want; diff > 1e-9 || diff < -1e-9 {
		t.Fatalf("chat 协议费用应为 %.5f 元（未命中只算 1000 token），得到 %.5f", want, got)
	}
	// 旧口径（输入全额 × 输入价再叠缓存价）会是 20 + 0.4 + 4 = 24.4 元
	if got > 5 {
		t.Fatalf("费用 %.2f 元虚高：缓存命中的输入被重复计费了", got)
	}
	// 缓存读取比输入还大（脏数据）时不许出负数
	if _, _, ok := p.cost("deepseek-flash", "deepseek", "chat", "2026-11-12 10:00:00",
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
	tk := usageTokens{Input: 1000000, CacheRead: 1000000}
	got, _, ok := p.cost("claude-x", "p", "anthropic", "2026-11-12 10:00:00", tk)
	if !ok {
		t.Fatal("应能算出费用")
	}
	// 未命中 1M × 2 = 2；缓存读取 1M × 0.2 = 0.2 → 2.2 元（chat 口径会是 0.2 元）
	if got < 2.1999 || got > 2.2001 {
		t.Fatalf("Anthropic 费用应为 2.2 元，得到 %.4f", got)
	}
	if spl := splitUsageTokens("anthropic", tk); spl.Missed != 1000000 {
		t.Fatalf("Anthropic 未命中应为输入全量 1M，得到 %d", spl.Missed)
	}
	if usageInputIncludesCache("") != true || usageInputIncludesCache("chat") != true ||
		usageInputIncludesCache("responses") != true || usageInputIncludesCache("anthropic") != false {
		t.Fatal("协议口径判定不对")
	}
	// 缓存写入按写价单独计（Anthropic 的 cache_creation）
	got2, _, _ := p.cost("claude-x", "p", "anthropic", "2026-11-12 10:00:00",
		usageTokens{CacheWrite: 1000000})
	if got2 < 3.9999 || got2 > 4.0001 {
		t.Fatalf("缓存写入 1M × 4 元应为 4 元，得到 %.4f", got2)
	}
}

// TestUsagePricingPeakIdleSum 要求①：高峰与空闲**各自计价再相加**，不是拿总额乘一次。
func TestUsagePricingPeakIdleSum(t *testing.T) {
	p := dsPricing(dsRule())
	tk := usageTokens{Input: 1000000, Output: 1000000} // 峰值 2 + 8 = 10 元；空闲 5 元
	peakCost, peak, ok := p.cost("deepseek-flash", "c", "chat", "2026-11-12 10:30:00", tk)
	if !ok || !peak {
		t.Fatalf("周四上午应判为高峰，peak=%v ok=%v", peak, ok)
	}
	if peakCost < 9.9999 || peakCost > 10.0001 {
		t.Fatalf("高峰应为 10 元，得到 %.4f", peakCost)
	}
	idleCost, peak2, _ := p.cost("deepseek-flash", "c", "chat", "2026-11-12 20:00:00", tk)
	if peak2 {
		t.Fatal("周四 20:00 应判为空闲")
	}
	if idleCost < 4.9999 || idleCost > 5.0001 {
		t.Fatalf("空闲应为 5 元（半价），得到 %.4f", idleCost)
	}
	// 聚合口径：两个桶各自算完相加 = 15 元（若只在总额上判一次时段，会得到 20 或 10）
	if sum := peakCost + idleCost; sum < 14.9999 || sum > 15.0001 {
		t.Fatalf("高峰+空闲应各算各的再相加＝15 元，得到 %.4f", sum)
	}
	// 同样的 token 放在同一个高峰桶里则是 20 元：三桶相加能区分「逐桶判时段」
	if twoPeak := peakCost * 2; twoPeak < 19.9999 || twoPeak > 20.0001 {
		t.Fatalf("两个高峰桶应为 20 元，得到 %.4f", twoPeak)
	}
}

// TestUsagePricingBoundaries 要求②：分钟级时段边界（含 8:59/9:00、11:59/12:00、
// 13:59/14:00、17:59/18:00）各自落在对的价档上。
func TestUsagePricingBoundaries(t *testing.T) {
	p := dsPricing(dsRule())
	tk := usageTokens{Input: 1000000} // 高峰 2 元 / 空闲 1 元，金额即价档
	cases := map[string]bool{         // ts → 是否高峰
		"2026-11-12 00:00:00": false,
		"2026-11-12 08:59:00": false, // 8:59 还没进高峰
		"2026-11-12 09:00:00": true,
		"2026-11-12 11:59:00": true,
		"2026-11-12 12:00:00": false,
		"2026-11-12 13:59:00": false,
		"2026-11-12 14:00:00": true,
		"2026-11-12 17:59:00": true,
		"2026-11-12 18:00:00": false,
		"2026-11-12 23:59:00": false,
	}
	for ts, wantPeak := range cases {
		amount, peak, ok := p.cost("deepseek-flash", "c", "chat", ts, tk)
		if !ok {
			t.Fatalf("%s 应能算出费用", ts)
		}
		if peak != wantPeak {
			t.Fatalf("%s 高峰判定应为 %v，得到 %v", ts, wantPeak, peak)
		}
		want := 1.0
		if wantPeak {
			want = 2.0
		}
		if amount < want-1e-9 || amount > want+1e-9 {
			t.Fatalf("%s 金额应为 %.1f 元，得到 %.4f", ts, want, amount)
		}
	}
	// 分钟桶（聚合用的 'YYYY-MM-DD HH:MM'）与逐条明细判定必须一致——
	// 这条就是 2026-11-12 的根因回归：桶时间戳解析不了会被当成「全天高峰」
	for ts, wantPeak := range cases {
		bucket := ts[:16] // 去掉秒
		if got := p.peakAt("deepseek-flash", bucket); got != wantPeak {
			t.Fatalf("分钟桶 %s 判定应为 %v，得到 %v", bucket, wantPeak, got)
		}
	}
}

// TestUsagePricingWeekendHoliday 要求③：周末与规则里的节假日全天空闲。
func TestUsagePricingWeekendHoliday(t *testing.T) {
	rule := dsRule()
	rule.Holidays = []string{"10-01", "2026-11-12"} // 每年重复 + 只此一次
	p := dsPricing(rule)
	tk := usageTokens{Input: 1000000}
	// 2026-11-14 周六、11-15 周日：任何时刻都空闲
	for _, ts := range []string{"2026-11-14 10:00:00", "2026-11-15 15:00:00", "2026-11-14 09:30:00"} {
		if amount, peak, _ := p.cost("deepseek-flash", "c", "chat", ts, tk); peak || amount > 1.0001 {
			t.Fatalf("%s（周末）应全天按空闲 1 元，得到 peak=%v 金额 %.4f", ts, peak, amount)
		}
	}
	// 节假日：具体日期（把今天 2026-11-12 周四也当假日）整天按空闲
	if amount, peak, _ := p.cost("deepseek-flash", "c", "chat", "2026-11-12 10:00:00", tk); peak || amount > 1.0001 {
		t.Fatalf("规则里的节假日应全天按空闲，得到 peak=%v 金额 %.4f", peak, amount)
	}
	// "MM-DD" 每年重复：2027-10-01（周五）同样按空闲
	if peak := p.peakAt("deepseek-flash", "2027-10-01 10:00:00"); peak {
		t.Fatal("每年重复的节假日 10-01 在 2027 年也该生效")
	}
	// 普通工作日不受影响（把规则里的 2026-11-12 去掉后）
	p2 := dsPricing(dsRule())
	if peak := p2.peakAt("deepseek-flash", "2026-11-12 10:00:00"); !peak {
		t.Fatal("普通工作日 10:00 应为高峰")
	}
	if peak := p2.peakAt("deepseek-flash", "2026-11-13 10:00:00"); !peak {
		t.Fatal("周五 10:00 应为高峰")
	}
}

// TestUsagePricingNoRule 要求④：规则缺失＝不分时段（全按高峰价），且页面能看出没配。
func TestUsagePricingNoRule(t *testing.T) {
	p := dsPricing(usagePeakRule{IdleFactor: 0.5}) // 只填了折扣、没配时段
	tk := usageTokens{Input: 1000000}
	a, peakA, _ := p.cost("deepseek-flash", "c", "chat", "2026-11-12 10:00:00", tk)
	b, peakB, _ := p.cost("deepseek-flash", "c", "chat", "2026-11-12 03:00:00", tk)
	if !peakA || !peakB {
		t.Fatal("未配时段规则时应一律判为高峰")
	}
	if a != b {
		t.Fatalf("未配时段规则时全天同价：10:00 %.4f vs 03:00 %.4f", a, b)
	}
	if a < 1.9999 || a > 2.0001 {
		t.Fatalf("应按峰值价 2 元计价，得到 %.4f", a)
	}
	if p.anyRuleConfigured() {
		t.Fatal("一条时段都没配时 anyRuleConfigured 应为 false（页面据此提示「未配置时段规则」）")
	}
	// 配了时段后，折扣才生效
	p2 := dsPricing(dsRule())
	if !p2.anyRuleConfigured() {
		t.Fatal("配了时段后 anyRuleConfigured 应为 true")
	}
	if v, _, _ := p2.cost("deepseek-flash", "c", "chat", "2026-11-12 03:00:00", tk); v > 1.0001 {
		t.Fatalf("配了时段后空闲价应为 1 元，得到 %.4f", v)
	}
}

// TestUsagePricingUnpriced 要求⑤：未配单价不产生费用（页面显示「未配单价」）。
func TestUsagePricingUnpriced(t *testing.T) {
	empty := emptyUsagePricing()
	if len(empty.Models) != 0 {
		t.Fatal("新装环境的价表必须是空的（程序不内置任何模型价）")
	}
	tk := usageTokens{Input: 1000000, Output: 1000000}
	if _, _, ok := empty.cost("deepseek-flash", "c", "chat", "2026-11-12 10:00:00", tk); ok {
		t.Fatal("空价表下不该算出费用")
	}
	if _, ok := empty.priceOf("deepseek-flash"); ok {
		t.Fatal("空价表下 priceOf 应为未配")
	}
	// 「从渠道模型添加空行」加出来的行：四个价全 0 ＝ 还没填，也算未配单价
	blank := usagePricing{Models: map[string]modelPrice{"m": {}}}
	if _, ok := blank.priceOf("m"); ok {
		t.Fatal("四个价全 0 的行应算「未配单价」，不能显示成免费的 ¥0")
	}
	if _, _, ok := blank.cost("m", "c", "chat", "2026-11-12 10:00:00", tk); ok {
		t.Fatal("四个价全 0 的行不该得出金额")
	}
	// 填了任意一个价就算配过（用户可能只想按输出计费）
	partial := usagePricing{Models: map[string]modelPrice{"m": {Output: 8}}}
	if v, _, ok := partial.cost("m", "c", "chat", "2026-11-12 10:00:00", usageTokens{Output: 1000000}); !ok || v != 8 {
		t.Fatalf("只填输出价也应按它计费：ok=%v v=%.4f", ok, v)
	}
}

// TestUsagePeakRuleCrossMidnight 时段可跨天（"22:00-06:00"），语义写进注释与页面：
// 按调用当天的时刻判定、星期看调用当天。
func TestUsagePeakRuleCrossMidnight(t *testing.T) {
	r := usagePeakRule{PeakWindows: []string{"22:00-06:00"}, IdleFactor: 0.5}
	cases := map[string]bool{
		"2026-11-12 21:59:00": false,
		"2026-11-12 22:00:00": true,
		"2026-11-12 23:59:00": true,
		"2026-11-12 00:00:00": true,
		"2026-11-12 05:59:00": true,
		"2026-11-12 06:00:00": false,
		"2026-11-12 12:00:00": false,
	}
	for ts, want := range cases {
		if got := r.peakAt(ts); got != want {
			t.Fatalf("跨天时段 %s 判定应为 %v，得到 %v", ts, want, got)
		}
	}
	// 星期看调用当天：只勾周五 → 周六凌晨 2:00 不算高峰（不回溯到周五）
	r2 := usagePeakRule{PeakWindows: []string{"22:00-06:00"}, PeakDays: []int{5}}
	if !r2.peakAt("2026-11-13 23:00:00") { // 周五夜里
		t.Fatal("周五 23:00 应为高峰")
	}
	if r2.peakAt("2026-11-14 02:00:00") { // 周六凌晨：按调用当天（周六）判定
		t.Fatal("周六凌晨按「调用当天的星期」判定，不该算高峰")
	}
}

// TestUsagePeakRuleNormalize 脏配置不许把计费算歪：非法时段/日期/星期被丢掉，
// 全选七天归一成「不限定星期」，折扣只认 (0,1)。
func TestUsagePeakRuleNormalize(t *testing.T) {
	r := usagePeakRule{
		PeakWindows: []string{"9:00-12:00", "09:00-12:00", " 14:00 - 18:00 ", "abc", "25:00-26:00", "10:00-10:00"},
		PeakDays:    []int{1, 2, 3, 4, 5, 6, 0, 7, -1, 1},
		Holidays:    []string{"10-01", "2026/10/02", "2026.10.03", "13-01", "坏数据", "10-01", "2026-10-04"},
		IdleFactor:  1.5,
	}
	r.normalize()
	if len(r.PeakWindows) != 2 || r.PeakWindows[0] != "09:00-12:00" || r.PeakWindows[1] != "14:00-18:00" {
		t.Fatalf("时段规整不对：%v", r.PeakWindows)
	}
	if len(r.PeakDays) != 0 {
		t.Fatalf("全选七天应归一成「不限定星期」，得到 %v", r.PeakDays)
	}
	wantHols := []string{"10-01", "2026-10-02", "2026-10-03", "2026-10-04"}
	if len(r.Holidays) != len(wantHols) {
		t.Fatalf("节假日规整不对：%v", r.Holidays)
	}
	for i, h := range wantHols {
		if r.Holidays[i] != h {
			t.Fatalf("节假日规整不对：%v（期望 %v）", r.Holidays, wantHols)
		}
	}
	if r.IdleFactor != 0 {
		t.Fatalf("折扣 1.5 应归一为「不分时段」，得到 %v", r.IdleFactor)
	}
	if r.idleDiscount() != 1 {
		t.Fatal("归一后 idleDiscount 应为 1")
	}
	// 空规则（什么都没有）＝未配置
	if (usagePeakRule{}).configured() {
		t.Fatal("空规则应算未配置")
	}
	// 非法/空的 ts 按高峰兜底（宁可高估不低估）
	if !dsRule().peakAt("") || !dsRule().peakAt("坏数据") {
		t.Fatal("空/脏时间戳应按高峰兜底")
	}
	// 老版本的小时桶（'YYYY-MM-DD HH'，13 位）也要能解析——2026-11-12 的根因
	if t0, ok := usageTimeOf("2026-11-12 08"); !ok || t0.Hour() != 8 {
		t.Fatalf("小时桶应解析成 08:00，得到 %v ok=%v", t0, ok)
	}
}

// TestUsagePricingHistoryRecompute 改了规则，历史重算结果要跟着变、且与「按当时
// 时刻逐条判定」一致——费用是按明细时间戳算的，不按「现在」。
func TestUsagePricingHistoryRecompute(t *testing.T) {
	tk := usageTokens{Input: 1000000}
	noRule := dsPricing(usagePeakRule{})
	withRule := dsPricing(dsRule())
	const ts = "2026-11-12 20:00:00" // 历史时刻（周四晚，空闲）
	before, _, _ := noRule.cost("deepseek-flash", "c", "chat", ts, tk)
	after, _, _ := withRule.cost("deepseek-flash", "c", "chat", ts, tk)
	if before < 1.9999 || before > 2.0001 {
		t.Fatalf("未配规则时应按峰值 2 元，得到 %.4f", before)
	}
	if after < 0.9999 || after > 1.0001 {
		t.Fatalf("配上规则后同一条历史记录应变成空闲 1 元，得到 %.4f", after)
	}
	// 判定用的是明细时刻，不是运行时的「现在」：把本地时钟换掉不影响结果
	saved := time.Local
	defer func() { time.Local = saved }()
	time.Local = time.UTC
	if v, _, _ := withRule.cost("deepseek-flash", "c", "chat", ts, tk); v < 0.9999 || v > 1.0001 {
		t.Fatalf("判定不许依赖运行时时钟，得到 %.4f", v)
	}
}
