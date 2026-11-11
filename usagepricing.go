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

import (
	"encoding/json"
	"sort"
	"strings"
)

const (
	usagePricingKey = "usage.pricing" // meta 表键（SetGlobalData 自动加 globaldata: 前缀）
	usdToCNY        = 7.2             // 美元价折算成元的系数（内置价里含 OpenAI/Anthropic 官方价）
)

// modelPrice 是一个模型的基础单价：元 / 百万 tokens。
type modelPrice struct {
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// usagePricing 是整份计费配置：模型价表 + 渠道倍率。
type usagePricing struct {
	Models      map[string]modelPrice `json:"models"`
	Multipliers map[string]float64    `json:"multipliers"` // providerID → 倍率（缺省 1.0）
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
	return usagePricing{
		Models: map[string]modelPrice{
			// DeepSeek（官方按量价：输入缓存未命中 2 元/M、命中 0.2 元/M、输出 8 元/M）
			"deepseek-chat":     cny(2, 8, 0.2, 2),
			"deepseek-reasoner": cny(2, 8, 0.2, 2),
			"deepseek-v4-pro":   cny(2, 8, 0.2, 2),
			"deepseek-v4-flash": cny(1, 4, 0.1, 1),
			"deepseek-flash":    cny(1, 4, 0.1, 1),
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
			p.Models[id] = m
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
func (p usagePricing) cost(model, providerID string, t usageTokens) (amount float64, ok bool) {
	price, ok := p.priceOf(model)
	if !ok {
		return 0, false
	}
	sum := float64(t.Input)*price.Input +
		float64(t.Output)*price.Output +
		float64(t.CacheRead)*price.CacheRead +
		float64(t.CacheWrite)*price.CacheWrite
	return sum / 1e6 * p.multiplierOf(providerID), true
}

// usagePricingModel 是页面「单价与倍率」里模型价表的一行（按 id 排序后的视图）。
type usagePricingModel struct {
	ID         string  `json:"id"`
	Input      float64 `json:"input"`
	Output     float64 `json:"output"`
	CacheRead  float64 `json:"cacheRead"`
	CacheWrite float64 `json:"cacheWrite"`
}

// modelsSorted 把模型价表拍平成按 id 排序的列表（页面表格顺序稳定）。
func (p usagePricing) modelsSorted() []usagePricingModel {
	out := make([]usagePricingModel, 0, len(p.Models))
	for id, m := range p.Models {
		out = append(out, usagePricingModel{ID: id, Input: m.Input, Output: m.Output,
			CacheRead: m.CacheRead, CacheWrite: m.CacheWrite})
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
