package main

// usagepricing.go —— 用量统计的计费口径：模型单价 + 时段规则 + 渠道倍率。
//
// 用户定的口径（2026-11-12 用户实测后彻底改过一版，"不再猜价"）：
//
//  1. **程序不内置任何模型单价**。各家价格随时调整、中转站还各有倍率，写死在代码里
//     只会给出错数——价表完全由用户填，新装环境是空表。空表（或某模型一行全 0）
//     时页面费用列写「未配单价」，**不给任何兜底价**：宁可显示不出来，也不要给错数。
//  2. 每个模型四个价：输入 / 输出 / 缓存读取 / 缓存写入，单位「元 / 百万 tokens」，
//     填的是**峰值价**；空闲时段按该模型的空闲折扣折算。
//  3. 每个渠道（provider）一个倍率，默认 1.0：费用 = 基础价算出的金额 × 该渠道倍率。
//     同一个模型挂在不同中转站不用逐个配价——中转站通常就是「原价 × 倍率」。
//  4. 时段规则（usagepeak.go 的 usagePeakRule）按模型配：高峰星期、高峰时段、
//     节假日、空闲折扣。规则缺失＝不分时段（全按高峰价），页面提示「未配置时段规则」。
//
// 价格表存 meta 表（globaldata:usage.pricing）：它不是凭据、也不是程序配置，跟着
// 数据走（跟待办清单同一个存放口径）；用户改完即存，聚合时现算——改价或改规则后
// 历史记录的花费跟着变，不用回填全表。
//
// 计费公式（2026-11-12 修正虚高 23 倍后定的口径）：
//
//	费用 = 未命中输入 × 输入价 + 缓存读取 × 缓存读价 + 缓存写入 × 缓存写价 + 输出 × 输出价
//
// 「未命中输入」要按协议归一到「不含缓存」的口径（usagepeak.go 的 splitUsageTokens）：
// OpenAI 系/DeepSeek 的 prompt_tokens 本身就含缓存命中，直接乘输入价会把命中量按
// 最贵的价再收一遍。缓存写入（Anthropic 的 cache_creation）是单列的，按写价计。
//
// 时段判定按**调用发生的时刻**（明细的 ts），不按「现在」——历史重算才对。

import (
	"encoding/json"
	"sort"
	"strings"
)

const usagePricingKey = "usage.pricing" // meta 表键（SetGlobalData 自动加 globaldata: 前缀）

// modelPrice 是一个模型的单价（元 / 百万 tokens，存的是**峰值价**）+ 时段规则。
//
// 四个价全 0 ＝ 还没填（等价「未配单价」）：页面「从渠道模型一键添加空行」加出来的
// 就是这种行，逐行填价即可，不会因为「价表里有一行 0」而把费用显示成 ¥0。
type modelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	// 时段规则平铺在这里（不用嵌套对象）：老配置里就有 idleFactor 这一键，平铺
	// 才能原地兼容——嵌套会让老配置的折扣读不出来，静默变成「不分时段」。
	usagePeakRule
}

// priced 这一行是否真的配了单价（四个价全 0 ＝ 未配，见上面的说明）。
func (m modelPrice) priced() bool {
	return m.Input > 0 || m.Output > 0 || m.CacheRead > 0 || m.CacheWrite > 0
}

// usagePricing 是整份计费配置：模型价表 + 渠道倍率 + 搜索按次价。
type usagePricing struct {
	Models      map[string]modelPrice `json:"models"`
	Multipliers map[string]float64    `json:"multipliers"` // providerID → 倍率（缺省 1.0）
	// SearchPrices 搜索/抽取的单价（元 / 次，只对成功调用计费）：键可以是供应商
	// id（精确覆盖），也可以是类型 kind（deepseek/tavily/…）——用户自定义 id 时
	// 仍能吃到该类型的默认价。
	SearchPrices map[string]float64 `json:"searchPrices,omitempty"`
}

// emptyUsagePricing 是「新装环境」的价表：**空**（用户口径：不内置任何模型价）。
func emptyUsagePricing() usagePricing {
	return usagePricing{
		Models:       map[string]modelPrice{},
		Multipliers:  map[string]float64{},
		SearchPrices: map[string]float64{},
	}
}

// normalize 补齐缺失的映射并去掉非法配置。
func (p *usagePricing) normalize() {
	if p.Models == nil {
		p.Models = map[string]modelPrice{}
	}
	if p.Multipliers == nil {
		p.Multipliers = map[string]float64{}
	}
	for id, m := range p.Models {
		if strings.TrimSpace(id) == "" {
			delete(p.Models, id)
			continue
		}
		if m.Input < 0 || m.Output < 0 || m.CacheRead < 0 || m.CacheWrite < 0 {
			m.Input, m.Output, m.CacheRead, m.CacheWrite = 0, 0, 0, 0
		}
		m.usagePeakRule.normalize()
		p.Models[id] = m
	}
	if p.SearchPrices == nil {
		p.SearchPrices = map[string]float64{}
	}
	for k, v := range p.SearchPrices {
		if strings.TrimSpace(k) == "" || v < 0 {
			delete(p.SearchPrices, k)
		}
	}
	for id, mul := range p.Multipliers {
		if mul <= 0 {
			delete(p.Multipliers, id)
		}
	}
}

// priceOf 取模型单价；未配（模型不在表里，或这一行四个价全 0）返回 false，
// 页面据此显示「未配单价」。
func (p usagePricing) priceOf(model string) (modelPrice, bool) {
	m, ok := p.Models[strings.TrimSpace(model)]
	if !ok || !m.priced() {
		return modelPrice{}, false
	}
	return m, true
}

// ruleOf 取模型的时段规则（未配单价的模型也可能有规则，页面要显示它）。
func (p usagePricing) ruleOf(model string) usagePeakRule {
	return p.Models[strings.TrimSpace(model)].usagePeakRule
}

// peakAt 按该模型的规则判定某时间戳是高峰还是空闲（明细表 hover 标记也用它）。
func (p usagePricing) peakAt(model, ts string) bool {
	return p.ruleOf(model).peakAt(ts)
}

// anyRuleConfigured 是否至少有一个模型配了时段规则（页面提示「未配置时段规则」用）。
func (p usagePricing) anyRuleConfigured() bool {
	for _, m := range p.Models {
		if m.usagePeakRule.configured() {
			return true
		}
	}
	return false
}

// multiplierOf 取渠道倍率（未配＝1.0）。
func (p usagePricing) multiplierOf(providerID string) float64 {
	if m, ok := p.Multipliers[strings.TrimSpace(providerID)]; ok && m > 0 {
		return m
	}
	return 1.0
}

// usageTokens 是一次（或一批）调用的四个 token 量。
type usageTokens struct {
	Input      int64
	Output     int64
	CacheRead  int64
	CacheWrite int64
}

// cost 按基础价算金额再乘渠道倍率；模型未配单价返回 ok=false。
// protocol 决定输入口径（OpenAI 系输入含缓存命中，Anthropic 单列——见
// splitUsageTokens）；ts 决定该次调用命中峰值价还是空闲价。ts 传明细表的时间戳
// （聚合时是分钟桶），**不传「现在」**——否则历史重算会按今天的时段算。
// peak 回传这次判定结果：页面要按「高峰 X 次 / 空闲 Y 次」分解费用。
func (p usagePricing) cost(model, providerID, protocol, ts string, t usageTokens) (amount float64, peak, ok bool) {
	price, ok := p.priceOf(model)
	if !ok {
		return 0, false, false
	}
	peak = price.peakAt(ts)
	if !peak {
		price = price.idle()
	}
	spl := splitUsageTokens(protocol, t)
	sum := float64(spl.Missed)*price.Input +
		float64(spl.Output)*price.Output +
		float64(spl.CacheRead)*price.CacheRead +
		float64(spl.CacheWrite)*price.CacheWrite
	return sum / 1e6 * p.multiplierOf(providerID), peak, true
}

// idle 空闲时段价：空闲折扣生效时按折扣算（未配折扣＝不分时段，原价返回）。
func (m modelPrice) idle() modelPrice {
	f := m.idleDiscount()
	if f >= 1 {
		return m
	}
	m.Input *= f
	m.Output *= f
	m.CacheRead *= f
	m.CacheWrite *= f
	return m
}

// defaultSearchPrice 搜索/抽取的公开按次价（元 / 次，只对成功调用计费）。
// 有免费额度的先记 0（页面标「免费额度内」），以后要精确算直接改配置即可。
func defaultSearchPrice(kind string) float64 {
	switch kind {
	case spKindDeepSeek:
		return 0.005 // ¥5 / 1000 次成功请求
	case spKindGateway:
		return 0 // 公司网关统一计费，客户端不单独计
	}
	return 0 // Tavily / AnySearch 免费额度内；博查、智谱未配单价
}

// searchPriceOf 搜索单价（元 / 次）：先按供应商 id 精确覆盖，再按类型 kind，
// 都没有就用该类型的公开默认价。用户自定义 id 时也能吃到类型的官方价。
func (p usagePricing) searchPriceOf(providerID, kind string) float64 {
	if v, ok := p.SearchPrices[strings.TrimSpace(providerID)]; ok && v >= 0 {
		return v
	}
	if v, ok := p.SearchPrices[strings.TrimSpace(kind)]; ok && v >= 0 {
		return v
	}
	return defaultSearchPrice(kind)
}

// usagePricingModel 是页面「单价与规则」里模型价表的一行（按 id 排序后的视图）。
type usagePricingModel struct {
	ID         string  `json:"id"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	// 是否真的配了价（四个价全 0＝只加了行还没填）
	Priced bool `json:"priced"`
	// 时段规则（与 usagePeakRule 同口径，页面展开编辑）
	PeakWindows []string `json:"peakWindows"`
	PeakDays    []int    `json:"peakDays"`
	Holidays    []string `json:"holidays"`
	IdleFactor  float64  `json:"idleFactor"` // 空闲折扣（1＝不分时段）
}

// modelsSorted 把模型价表拍平成按 id 排序的列表（页面表格顺序稳定）。
func (p usagePricing) modelsSorted() []usagePricingModel {
	out := make([]usagePricingModel, 0, len(p.Models))
	for id, m := range p.Models {
		out = append(out, m.viewOf(id))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// viewOf 一个模型的对外视图：折扣缺省显示 1（别让 0 这个内部哨兵露出去），
// 切片字段一律给空数组（前端不用判 null）。
func (m modelPrice) viewOf(id string) usagePricingModel {
	idle := m.idleDiscount()
	wins := m.PeakWindows
	if wins == nil {
		wins = []string{}
	}
	days := m.PeakDays
	if days == nil {
		days = []int{}
	}
	hols := m.Holidays
	if hols == nil {
		hols = []string{}
	}
	return usagePricingModel{ID: id, Input: m.Input, Output: m.Output,
		CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, Priced: m.priced(),
		PeakWindows: wins, PeakDays: days, Holidays: hols, IdleFactor: idle}
}

// ---------- 读写（存 meta 表；App 层带缓存） ----------

// loadUsagePricing 读全局价格配置；没存过或损坏则用**空价表**（不内置任何单价，
// 也不写库——用户没动过就不该在库里留一份"看起来是用户配的"东西）。
//
// 迁移口径：已经存过的自定义价表**原样保留**（老版本的 idleFactor 仍在原键位上，
// 见 modelPrice 的平铺说明），不会被这次改动清空或覆盖。
func (a *App) loadUsagePricing() usagePricing {
	if raw := a.Store.GetGlobalData(usagePricingKey); strings.TrimSpace(raw) != "" {
		var p usagePricing
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			p.normalize()
			return p
		}
	}
	return emptyUsagePricing()
}

// saveUsagePricing 落库（整体替换式保存；页面把整份配置交上来）。
func (a *App) saveUsagePricing(p usagePricing) error {
	p.normalize()
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return a.Store.SetGlobalData(usagePricingKey, string(raw))
}
