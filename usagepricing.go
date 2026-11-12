package main

// usagepricing.go —— 用量统计的计费口径：模型基础单价 + 渠道倍率。
//
// 用户定的口径（2026-11-11）：
//   - 模型基础价按「模型 id」配置四个价：输入 / 输出 / 缓存读取 / 缓存写入，
//     单位「元 / 百万 tokens」。
//   - 每个渠道（provider）一个倍率，默认 1.0：费用 = 基础价算出的金额 × 该渠道
//     倍率。同一个模型挂在不同中转站不用逐个配价——中转站通常就是「原价 × 倍率」。
//   - 未配置单价的模型只显示 token，费用列显示「未配单价」（不给 0，免得让人
//     误以为免费）。
//   - 页面必须写明「按本地单价估算，不代表账单」。
//
// 价格表存 meta 表（globaldata:usage.pricing）：它不是凭据、也不是程序配置，
// 跟着数据走（跟待办清单同一个存放口径）；用户改完即存，聚合时现算。
//
// 计费公式（2026-11-12 修正，用户实测虚高 23 倍后定的口径）：
//
//	费用 = 未命中输入 × 输入价 + 缓存读取 × 缓存读价 + 缓存写入 × 缓存写价 + 输出 × 输出价
//
// 「未命中输入」要按协议归一到「不含缓存」的口径（usagepeak.go 的
// splitUsageTokens）：OpenAI 系/DeepSeek 的 prompt_tokens 本身就含缓存命中，
// 直接乘输入价会把命中量按最贵的价再收一遍。缓存写入（Anthropic 的
// cache_creation）是单列的，按写价计。
//
// 分时段价：厂商（DeepSeek）按调用时刻分高峰/空闲两档，空闲价 = 峰值价 × 折扣。
// 表里存峰值价 + IdleFactor（0.5＝官方空闲半价，缺省/1＝不分时段），聚合时按**每次
// 调用发生的时间戳**判定（usagepeak.go 的 usagePeakAt），历史数据重算才对。

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	usagePricingKey = "usage.pricing" // meta 表键（SetGlobalData 自动加 globaldata: 前缀）
	usdToCNY        = 7.2             // 美元价折算成元的系数（内置价里含 OpenAI/Anthropic 官方价）
)

// modelPrice 是一个模型的基础单价：元 / 百万 tokens（存的是**峰值价**）。
type modelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	// IdleFactor 空闲时段折扣：0.5＝官方空闲半价，缺省/1＝不分时段（全天同价）。
	// 用折扣而不是再存四个空闲价：官方两档就是半价关系，页面上一个数字也更好懂。
	IdleFactor float64 `json:"idleFactor,omitempty"`
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

// defaultUsagePricing 是内置的初始单价：按各家公开价格（美元价按 usdToCNY 折算
// 成元），全部可编辑。模型 id 用各渠道官方短名；没列到的一律「未配单价」，
// 由用户在页面上自己加。
func defaultUsagePricing() usagePricing {
	usd := func(input, output, cached float64) modelPrice {
		return modelPrice{
			Input:      roundCNY(input * usdToCNY),
			Output:     roundCNY(output * usdToCNY),
			CacheRead:  roundCNY(cached * usdToCNY),
			CacheWrite: roundCNY(input * usdToCNY), // 多数协议按未命中输入价计缓存写入
		}
	}
	cny := func(input, output, cacheRead, cacheWrite float64) modelPrice {
		return modelPrice{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite}
	}
	// cnyIdle 峰值价 + 空闲折扣（官方分时段价的模型用它）
	cnyIdle := func(input, output, cacheRead, cacheWrite, idle float64) modelPrice {
		return modelPrice{Input: input, Output: output, CacheRead: cacheRead, CacheWrite: cacheWrite, IdleFactor: idle}
	}
	// DeepSeek 官方现行价（元/百万 tokens，2026-11 核对
	// https://api-docs.deepseek.com/zh-cn/quick_start/pricing）：
	//   缓存命中 高峰 0.04 / 空闲 0.02、缓存未命中 高峰 2 / 空闲 1、输出 高峰 8 / 空闲 4
	// 空闲 = 高峰半价；缓存写入官方未单列（未命中价即含写入），按未命中输入价兜底。
	dsFlash := cnyIdle(2, 8, 0.04, 2, 0.5)
	// deepseek-v4-pro：命中 0.30/0.15、未命中 9/4.5、输出 27/13.5
	dsPro := cnyIdle(9, 27, 0.3, 9, 0.5)
	return usagePricing{
		Models: map[string]modelPrice{
			// DeepSeek：deepseek-flash 是现行短名；deepseek-v4-flash 是老名，仍按
			// flash 价；deepseek-chat / deepseek-reasoner 已非现行模型名，按 flash 价
			// 兜底（用户真在用它们时可在页面上单独改价）。
			"deepseek-flash":    dsFlash,
			"deepseek-v4-flash": dsFlash,
			"deepseek-v4-pro":   dsPro,
			"deepseek-chat":     dsFlash, // 非现行模型名：按 flash 价兜底
			"deepseek-reasoner": dsFlash, // 非现行模型名：按 flash 价兜底
			// OpenAI（官方美元价折元）
			"gpt-4o":       usd(2.5, 10, 1.25),
			"gpt-4o-mini":  usd(0.15, 0.6, 0.075),
			"gpt-4.1":      usd(2, 8, 0.5),
			"gpt-4.1-mini": usd(0.4, 1.6, 0.1),
			"gpt-5":        usd(1.25, 10, 0.125),
			"gpt-5-mini":   usd(0.25, 2, 0.025),
			"o3":           usd(2, 8, 0.5),
			"o4-mini":      usd(1.1, 4.4, 0.275),
			// 智谱 GLM（按量价）
			"glm-4.6":       cny(4, 16, 1, 4),
			"glm-4.5":       cny(3, 12, 0.8, 3),
			"glm-5":         cny(4, 16, 1, 4),
			"glm-5.3":       cny(4, 16, 1, 4),
			"glm-5.3-flash": cny(0.5, 2, 0.1, 0.5),
			// 通义千问（百炼按量价）
			"qwen-turbo":       cny(0.3, 0.6, 0.06, 0.3),
			"qwen-plus":        cny(0.8, 2, 0.16, 0.8),
			"qwen-max":         cny(2.4, 9.6, 0.48, 2.4),
			"qwen3-coder-plus": cny(4, 16, 0.8, 4),
			// Anthropic（官方美元价折元；缓存写入 1.25 倍输入价、缓存读取 0.1 倍）
			"claude-opus-4-1":   {Input: roundCNY(15 * usdToCNY), Output: roundCNY(75 * usdToCNY), CacheRead: roundCNY(1.5 * usdToCNY), CacheWrite: roundCNY(18.75 * usdToCNY)},
			"claude-sonnet-4-5": {Input: roundCNY(3 * usdToCNY), Output: roundCNY(15 * usdToCNY), CacheRead: roundCNY(0.3 * usdToCNY), CacheWrite: roundCNY(3.75 * usdToCNY)},
			"claude-haiku-4-5":  {Input: roundCNY(1 * usdToCNY), Output: roundCNY(5 * usdToCNY), CacheRead: roundCNY(0.1 * usdToCNY), CacheWrite: roundCNY(1.25 * usdToCNY)},
			// 月之暗面 Kimi
			"kimi-k2":         cny(4, 16, 1, 4),
			"moonshot-v1-8k":  cny(12, 12, 1.2, 12),
			"moonshot-v1-32k": cny(24, 24, 2.4, 24),
		},
		Multipliers: map[string]float64{},
	}
}

// roundCNY 保留到分（单价本身很便宜，多留几位没意义，也免得一串小数进界面）。
func roundCNY(v float64) float64 {
	return float64(int64(v*100+0.5)) / 100
}

// normalize 补齐缺失的映射并去掉非法倍率（<=0 当未配置＝1.0）。
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
		// 空闲折扣只认 (0,1)：1 与非法值都当「不分时段」（全天同一价）
		if m.IdleFactor <= 0 || m.IdleFactor >= 1 {
			m.IdleFactor = 0
		}
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

// priceOf 取模型单价；未配返回 false（页面显示「未配单价」）。
func (p usagePricing) priceOf(model string) (modelPrice, bool) {
	m, ok := p.Models[strings.TrimSpace(model)]
	return m, ok
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
// splitUsageTokens）；ts 决定该次调用命中峰值价还是空闲价。ts 传明细表
// 的时间戳（聚合时是小时桶），**不传「现在」**——否则历史重算会按今天的时段算。
func (p usagePricing) cost(model, providerID, protocol, ts string, t usageTokens) (amount float64, ok bool) {
	price, ok := p.priceOf(model)
	if !ok {
		return 0, false
	}
	if !usagePeakAt(ts) {
		price = price.idle()
	}
	spl := splitUsageTokens(protocol, t)
	sum := float64(spl.Missed)*price.Input +
		float64(spl.Output)*price.Output +
		float64(spl.CacheRead)*price.CacheRead +
		float64(spl.CacheWrite)*price.CacheWrite
	return sum / 1e6 * p.multiplierOf(providerID), true
}

// idle 空闲时段价：IdleFactor 生效时按折扣算（缺省/非法＝不分时段，原价返回）。
func (m modelPrice) idle() modelPrice {
	if m.IdleFactor <= 0 || m.IdleFactor >= 1 {
		return m
	}
	return modelPrice{
		Input:      m.Input * m.IdleFactor,
		Output:     m.Output * m.IdleFactor,
		CacheRead:  m.CacheRead * m.IdleFactor,
		CacheWrite: m.CacheWrite * m.IdleFactor,
		IdleFactor: m.IdleFactor,
	}
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

// usagePricingModel 是页面「单价与倍率」里模型价表的一行（按 id 排序后的视图）。
type usagePricingModel struct {
	ID         string  `json:"id"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
	IdleFactor float64 `json:"idleFactor"` // 空闲折扣（1＝不分时段）
}

// modelsSorted 把模型价表拍平成按 id 排序的列表（页面表格顺序稳定）。
func (p usagePricing) modelsSorted() []usagePricingModel {
	out := make([]usagePricingModel, 0, len(p.Models))
	for id, m := range p.Models {
		idle := m.IdleFactor
		if idle <= 0 || idle >= 1 {
			idle = 1 // 页面上一律显示 1＝不分时段，别让 0 这个内部哨兵露出去
		}
		out = append(out, usagePricingModel{ID: id, Input: m.Input, Output: m.Output,
			CacheRead: m.CacheRead, CacheWrite: m.CacheWrite, IdleFactor: idle})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ---------- 读写（存 meta 表；App 层带缓存） ----------

// loadUsagePricing 读全局价格配置；没存过或损坏则用内置默认值（不写库——用户
// 没动过就不该在库里留一份"看起来是用户配的"副本）。
func (a *App) loadUsagePricing() usagePricing {
	if raw := a.Store.GetGlobalData(usagePricingKey); strings.TrimSpace(raw) != "" {
		var p usagePricing
		if err := json.Unmarshal([]byte(raw), &p); err == nil {
			p.normalize()
			return p
		}
	}
	return defaultUsagePricing()
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
