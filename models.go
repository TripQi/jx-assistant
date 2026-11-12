package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 多模型接入：providers 预置清单 + 会话级模型解析。
// 渠道协议三种（ProviderCfg.Protocol）：空串＝OpenAI 兼容 chat/completions
//（各家 DeepSeek/智谱/Kimi/通义/OpenAI/Ollama 本地都有兼容端点）、anthropic＝
// Anthropic 原生 /v1/messages、responses＝OpenAI Responses /v1/responses。
// 协议实现见 llmproto.go（与 StreamChat 同接口），这里只存类型，调用层零感知。
//
// 自定义供应商支持多把 Key（ProviderCfg.Keys，见 ProviderKeyCfg）：中转站按
// 分组发 Key，一把 Key 只看得到它那个分组的模型；一个供应商卡片里收多把 Key，
// 每把各自拉模型、各自选协议（同一中转站不同分组可能挂不同协议），菜单里那些
// 模型平铺在同一个供应商分组下。模型的重名靠「Key 归属 + 模型 ID」唯一定位
//（会话存 SessionMeta.ProviderKey，见 store.go）。

// ModelCfg 是一个可选模型的定义；ContextWindow 为 0 表示未知，按默认处理。
// 思考档位不在这里：档位与模型彻底解绑，全系统一套（见 thinkingLevels）。
type ModelCfg struct {
	ID             string `json:"id"`
	Name           string `json:"name,omitempty"`
	ContextWindow  int    `json:"contextWindow,omitempty"`
	MaxOutput      int    `json:"maxOutput,omitempty"`
	SupportsImages bool   `json:"supportsImages,omitempty"`
}

// ProviderKeyCfg 是供应商下的一把 Key（对应中转站的一个分组）。
// 同一个 BaseURL 共用一个供应商卡片，每把 Key 各自拉模型、各自选协议。
type ProviderKeyCfg struct {
	// ID 是 Key 的稳定归属标识（会话/草稿里的模型引用靠它还原该用哪把密钥，
	// 也是重名模型唯一的区分依据）。空串＝老单 Key 结构合成的隐含 Key。
	ID string `json:"id,omitempty"`
	// Label 分组备注名（如「默认分组」「Claude 分组」）：只在模型重名时作为
	// 界面上最小的区分后缀，不参与调用。
	Label string `json:"label,omitempty"`
	// APIKey 密钥本身：任何接口都只回脱敏后的掩码（server.go providerView）。
	APIKey string `json:"apiKey,omitempty"`
	// Protocol 是该 Key 的协议覆盖：空串＝跟随供应商级 Protocol（Key 级优先）；
	// "chat"＝显式 OpenAI 兼容（与空串的「跟随」区分开，见 keyProtocol）。
	Protocol string `json:"protocol,omitempty"`
	// Models 是这把 Key 自己拉到/填好的模型清单（同 Key 内不重名）。
	Models []ModelCfg `json:"models"`
}

// ProviderCfg 是一个模型服务提供方。
type ProviderCfg struct {
	ID      string     `json:"id"`
	Name    string     `json:"name"`
	BaseURL string     `json:"baseURL"`
	APIKey  string     `json:"apiKey,omitempty"`
	Models  []ModelCfg `json:"models"`
	// Keys 是自定义供应商的多 Key（多分组）结构：非空时它才是密钥与模型的
	// 真实来源，供应商级 APIKey/Models 只作向下兼容的镜像（保存时由第一把
	// Key 派生，老版本 exe 回退时仍读得到）。预置提供方与公司内部通道保持
	// 供应商级单 Key 结构不变（它们由系统/服务端维护，不需要分组）。
	Keys []ProviderKeyCfg `json:"keys,omitempty"`
	// Protocol 渠道协议类型：空串＝OpenAI 兼容 chat/completions（默认，存量
	// 配置全是它）、anthropic＝/v1/messages（x-api-key 鉴权）、responses＝
	// /v1/responses。设置页渠道卡片「协议类型」选择，调用层经 LLMTarget 透传；
	// 多 Key 结构下它是「默认协议」，每把 Key 可各自覆盖（keyProtocol）。
	Protocol string `json:"protocol,omitempty"`
	// Enabled 手动启停开关：nil＝启用（旧配置没写过这一键），显式 false＝停用
	//（不参与默认选择与会话解析；卡片置灰）。手动停用不会被自动恢复打开。
	Enabled *bool `json:"enabled,omitempty"`
	// AutoDisabled 公司通道专用（sync.go）：公司服务端内网/公网都连不上时自动
	// 停用，恢复可达自动清除；手动开一下开关也能清掉。
	AutoDisabled bool `json:"autoDisabled,omitempty"`
	// PublicBaseURL 公司通道的外网地址（由服务端公告下发，服务端没配就为空）：
	// 内网地址连不上时调用层自动改走这里（llm.go StreamChat 的故障转移）。
	PublicBaseURL string `json:"publicBaseURL,omitempty"`
	// Note 是预置提供方的备注（计费方式、Key 获取方式等），仅设置页展示，
	// 不参与调用；已保存的提供方以预置模板为准实时取，不落盘。
	Note string `json:"note,omitempty"`
}

// KeyList 返回该供应商的有效 Key 清单：多 Key 结构直接用；老的供应商级单 Key
//（预置提供方、公司内部通道）合成一把没有 ID 的隐含 Key——调用链、接口、菜单
// 因此只有一套解析路径，不必到处分支「是不是多 Key」。
func (p *ProviderCfg) KeyList() []ProviderKeyCfg {
	if len(p.Keys) > 0 {
		return p.Keys
	}
	return []ProviderKeyCfg{{APIKey: p.APIKey, Models: p.Models}}
}

// keyProtocol 返回一把 Key 生效的协议：Key 级覆盖优先于供应商级默认。
// Key 级的 "chat" 是「显式 OpenAI 兼容」的哨兵——Key 的 Protocol 空串表示跟随
// 供应商，没有它就表达不出「供应商默认 anthropic、这把 Key 走 chat/completions」。
func (p *ProviderCfg) keyProtocol(k ProviderKeyCfg) string {
	switch v := strings.TrimSpace(k.Protocol); v {
	case "":
		return p.Protocol
	case "chat":
		return ""
	default:
		return v
	}
}

// resolveKey 定位模型归属的那把 Key（重名模型能用哪把密钥全靠它）：
//  1. keyID 命中现有 Key 就直接用它——会话里存下的归属是权威，即使该 Key 的
//     模型清单后来被手工改过（用户可能删了模型条目但仍想用这把 Key）；
//  2. 否则找模型清单里有这个模型的 Key（老会话只存了模型名，或用户手工填了
//     一个没在清单里的模型名之外的场景）；
//  3. 都找不到 → 第一把可用的 Key（有密钥的那把；全都没密钥就第一把）——
//     落不到具体归属时也别把调用搞挂。
func (p *ProviderCfg) resolveKey(keyID, modelID string) ProviderKeyCfg {
	keys := p.KeyList()
	if id := strings.TrimSpace(keyID); id != "" {
		for _, k := range keys {
			if k.ID == id {
				return k
			}
		}
	}
	if modelID = strings.TrimSpace(modelID); modelID != "" {
		for _, k := range keys {
			for _, m := range k.Models {
				if m.ID == modelID {
					return k
				}
			}
		}
	}
	for _, k := range keys {
		if strings.TrimSpace(k.APIKey) != "" {
			return k
		}
	}
	return keys[0]
}

// ProviderModel 是平铺后的一个模型条目：模型本身 + 它的 Key 归属。
// 模型菜单要在一个供应商分组下平铺所有 Key 的模型，重名时靠 Key 归属区分。
type ProviderModel struct {
	Model ModelCfg
	Key   ProviderKeyCfg
}

// AllModels 平铺全部 Key 的模型（按 Key 顺序，Key 内保持原序）。
// 模型菜单、启停校验、派发模型校验都看这份清单——单 Key 结构自然只有一份。
func (p *ProviderCfg) AllModels() []ProviderModel {
	keys := p.KeyList()
	out := make([]ProviderModel, 0, len(keys))
	for _, k := range keys {
		for _, m := range k.Models {
			out = append(out, ProviderModel{Model: m, Key: k})
		}
	}
	return out
}

// hasModel 该供应商（任意一把 Key）是否提供这个模型。
func (p *ProviderCfg) hasModel(id string) bool {
	for _, k := range p.KeyList() {
		for _, m := range k.Models {
			if m.ID == id {
				return true
			}
		}
	}
	return false
}

// SetPrimaryKey 写「第一把 Key 的密钥与模型」：多 Key 结构改 Keys[0]，老单 Key
// 结构改供应商级字段。公司通道（wizard.go / sync.go）这类由系统维护的通道走它，
// 不用关心结构形态。
func (p *ProviderCfg) SetPrimaryKey(apiKey string, models []ModelCfg) {
	if len(p.Keys) > 0 {
		p.Keys[0].APIKey = apiKey
		p.Keys[0].Models = models
	}
	p.APIKey = apiKey
	p.Models = models
}

// IsSelectable 该提供方当前是否参与选模型：手动启用且未被自动停用。
func (p *ProviderCfg) IsSelectable() bool {
	return (p.Enabled == nil || *p.Enabled) && !p.AutoDisabled
}

// DefaultContextWindow 是模型未声明上下文窗口时的保守默认值。
const DefaultContextWindow = 262144

// DefaultProviders 预置常用提供方：DeepSeek/小米/智谱 Coding 自带模型清单
// （其余拿到 Key 后用「获取模型列表」一键拉取），URL 均为 OpenAI 兼容端点。
// 同一厂商按计费方式拆多条（按量 / 订阅套餐）：套餐端点与按量端点地址不同，
// 填错端点会扣不到套餐额度，Name/Note 里写清楚区别。
func DefaultProviders() []ProviderCfg {
	return []ProviderCfg{
		{ID: "deepseek", Name: "DeepSeek", BaseURL: "https://api.deepseek.com/v1", Models: []ModelCfg{
			// 官方思考控制走 reasoning_effort（档位由用户自己选，见 thinkingLevels）
			{ID: "deepseek-flash", Name: "DeepSeek-V4.1-Flash", ContextWindow: 1048576},
			{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro", ContextWindow: 1048576},
		}},
		{ID: "token-plan-cn", Name: "小米 MiMo（Token Plan）", BaseURL: "https://token-plan-cn.xiaomimimo.com/v1", Models: []ModelCfg{
			{ID: "mimo-v2.6-pro", Name: "MiMo v2.6 Pro"},
			{ID: "mimo-v2.6-flash", Name: "MiMo v2.6 Flash"},
		}},
		{ID: "zhipu", Name: "智谱 GLM（按量计费）", BaseURL: "https://open.bigmodel.cn/api/paas/v4",
			Note: "按 token 计费的开放平台端点；订阅了 Coding Plan 套餐的用户请改用下方的套餐端点，否则扣不到套餐额度"},
		{ID: "zhipu-coding", Name: "智谱 GLM（Coding Plan 套餐）", BaseURL: "https://open.bigmodel.cn/api/coding/paas/v4", Models: []ModelCfg{
			{ID: "glm-5.3", Name: "GLM-5.3", ContextWindow: 1048576, MaxOutput: 131072},
			{ID: "glm-5.3-flash", Name: "GLM-5.3-Flash（多模态）", ContextWindow: 1048576, MaxOutput: 131072, SupportsImages: true},
		}, Note: "走 Coding Plan 订阅额度；Key 需在「个人编程套餐」页单独创建，与按量计费 Key 不通用。套餐优先保障编程工具，通用 Agent 高负载时段可能限流排队"},
		{ID: "doubao", Name: "火山方舟 / 豆包（按量计费）", BaseURL: "https://ark.cn-beijing.volces.com/api/v3",
			Note: "模型填模型 ID（如 doubao-seed-2.0-pro）或接入点 ID（ep- 开头）；Key 在火山引擎方舟控制台创建"},
		{ID: "doubao-coding", Name: "火山方舟 / 豆包（Coding 套餐）", BaseURL: "https://ark.cn-beijing.volces.com/api/coding/v3",
			Note: "走方舟 Coding Plan 订阅额度（doubao-seed-code 系列）；需先在火山引擎订阅套餐"},
		{ID: "moonshot", Name: "Kimi / 月之暗面", BaseURL: "https://api.moonshot.cn/v1"},
		{ID: "qwen", Name: "通义千问", BaseURL: "https://dashscope.aliyuncs.com/compatible-mode/v1",
			Note: "阿里云百炼兼容模式；Key 在百炼控制台创建（sk- 开头）"},
		{ID: "qianfan", Name: "百度千帆", BaseURL: "https://qianfan.baidubce.com/v2",
			Note: "千帆 v2 OpenAI 兼容端点（文心/DeepSeek 系等）；Key 用千帆控制台的 API Key"},
		{ID: "hunyuan", Name: "腾讯混元", BaseURL: "https://api.hunyuan.cloud.tencent.com/v1",
			Note: "Key 在腾讯云混元控制台创建"},
		{ID: "minimax", Name: "MiniMax", BaseURL: "https://api.minimaxi.com/v1"},
		{ID: "spark", Name: "讯飞星火", BaseURL: "https://spark-api-open.xf-yun.com/v1",
			Note: "密钥填讯飞开放平台的 APIPassword（控制台「API Password」页生成），不是旧版 WebSocket 鉴权的 AppID/APIKey/APISecret 组合"},
		{ID: "stepfun", Name: "阶跃星辰", BaseURL: "https://api.stepfun.com/v1"},
		{ID: "siliconflow", Name: "硅基流动", BaseURL: "https://api.siliconflow.cn/v1",
			Note: "聚合平台：一个 Key 调用多家开源模型（DeepSeek/Qwen/GLM 等），部分模型免费"},
		{ID: "modelscope", Name: "魔搭 ModelScope（免费额度）", BaseURL: "https://api-inference.modelscope.cn/v1",
			Note: "免费推理接口（每日限量、不保证稳定，不建议重要任务依赖）；密钥用魔搭账户的 SDK Token，需绑定阿里云账号"},
		{ID: "openai", Name: "OpenAI", BaseURL: "https://api.openai.com/v1"},
		// Anthropic 原生协议预置：x-api-key 鉴权 + /v1/messages（llmproto.go）；
		// baseURL 约定与 OpenAI 一致（填到 /v1）。模型别名即官方短名。
		{ID: "anthropic", Name: "Anthropic", BaseURL: "https://api.anthropic.com/v1", Protocol: "anthropic", Models: []ModelCfg{
			// 思考走官方现行口径：thinking{type:adaptive} + output_config.effort
			//（none=thinking{type:disabled}；档位由用户自己选，见 thinkingLevels）
			{ID: "claude-opus-4-1", Name: "Claude Opus 4.1", ContextWindow: 200000, MaxOutput: 32768, SupportsImages: true},
			{ID: "claude-sonnet-4-5", Name: "Claude Sonnet 4.5", ContextWindow: 200000, MaxOutput: 64000, SupportsImages: true},
			{ID: "claude-haiku-4-5", Name: "Claude Haiku 4.5", ContextWindow: 200000, MaxOutput: 64000, SupportsImages: true},
		}, Note: "密钥用 Anthropic 控制台的 API Key（sk-ant- 开头）；兼容 new-api 等中转站时地址填中转站地址"},
		// OpenAI Responses 协议预置：官方新一代 /v1/responses 端点（与上面的
		// chat/completions 预置同一 BaseURL 同一 Key，两种端点并存，按需选）。
		// 思考档位走 reasoning.effort，六档原样发（llmproto.go）。
		{ID: "openai-responses", Name: "OpenAI（Responses 协议）", BaseURL: "https://api.openai.com/v1", Protocol: "responses", Models: []ModelCfg{
			{ID: "gpt-5", Name: "GPT-5", ContextWindow: 400000, MaxOutput: 128000, SupportsImages: true},
			{ID: "gpt-5-mini", Name: "GPT-5 mini", ContextWindow: 400000, MaxOutput: 128000, SupportsImages: true},
			{ID: "o3", Name: "o3", ContextWindow: 200000, MaxOutput: 100000, SupportsImages: true},
		}, Note: "官方新一代 Responses 端点；与「OpenAI」预置（chat/completions）同一账号同一 Key，二选一即可"},
		{ID: "ollama", Name: "Ollama 本地", BaseURL: "http://127.0.0.1:11434/v1"},
	}
}

// LLMTarget 是一次模型调用解析后的全部目的地信息。
type LLMTarget struct {
	BaseURL        string
	APIKey         string
	Model          string
	ContextWindow  int
	SupportsImages bool // 该模型是否声明支持图片输入（决定 read_image 是否内联视觉内容）
	// Protocol 渠道协议（ProviderCfg.Protocol 透传）：空串＝OpenAI 兼容、
	// anthropic＝/v1/messages、responses＝/v1/responses，StreamChat 按它分发。
	Protocol string
	// MaxOutput 模型目录声明的最大输出 tokens：Anthropic 必填 max_tokens、
	// Responses 的 max_output_tokens 都以它为上限来源（未声明则协议层默认）。
	MaxOutput int
	// ProviderID 目标所属提供方 id（""=旧单模型配置）：调用层用它把连接类失败
	// 归因到提供方（公司通道不可达时自动停用，见 sync.go noteProviderConnFail）。
	ProviderID string
	// KeyID 这次调用用的是该提供方的哪把 Key（多 Key 供应商的用量要能区分是哪把
	// 分组的额度；""=老单 Key 结构合成的隐含 Key）。只用于记账与展示。
	KeyID string
	// AltBaseURL 外网备用地址（公司通道由公告下发）：内网地址连接失败时
	// StreamChat 自动改走这里重试一次。
	AltBaseURL string
}

// AuditModel 返回审计行（call/request）里记的模型标识：provider/model 形式。
// 设计意图：将来按模型拆账算成本——sessions 表只有当前 model，中途换模型/
// 换渠道的会话拆不开；provider 前缀还能区分不同渠道的同名模型。旧单模型
// 配置兜底路径没有 provider，退回裸模型名。
func (t LLMTarget) AuditModel() string {
	if t.ProviderID == "" {
		return t.Model
	}
	return t.ProviderID + "/" + t.Model
}

func (a *App) providerByID(id string) *ProviderCfg {
	for i := range a.Cfg.Providers {
		if a.Cfg.Providers[i].ID == id {
			return &a.Cfg.Providers[i]
		}
	}
	return nil
}

// firstSelectableProvider 第一个可用（手动启用且未被自动停用）的提供方；
// 全部停用时回落 providers[0]（返回 nil 当没有任何提供方）——全停是用户自己的
// 选择，仍按原顺序解析，调用报错即可，不静默换通道。
func (a *App) firstSelectableProvider() *ProviderCfg {
	for i := range a.Cfg.Providers {
		if a.Cfg.Providers[i].IsSelectable() {
			return &a.Cfg.Providers[i]
		}
	}
	if len(a.Cfg.Providers) > 0 {
		return &a.Cfg.Providers[0]
	}
	return nil
}

// DefaultTarget 返回全局默认调用目标（第一个可用 provider 的默认模型）。
func (a *App) DefaultTarget() LLMTarget {
	if len(a.Cfg.Providers) == 0 {
		return LLMTarget{BaseURL: a.Cfg.BaseURL, APIKey: a.Cfg.APIKey, Model: a.Cfg.Model, ContextWindow: DefaultContextWindow}
	}
	return a.targetOf(a.firstSelectableProvider(), "", "")
}

// resolveTarget 按会话解析调用目标：会话指定了 provider/model 就用会话的，
// 否则跟随全局默认（第一个可用 provider）；会话指定的 provider 已停用时也回落
// 默认——否则出公司后旧会话仍钉死在已停用的公司通道上。
// sess.ProviderKey 是模型归属的那把 Key：老会话没有这一项，targetOf 会按模型
// 名找回它的 Key（找不到用第一把可用 Key），所以老会话/老草稿照常可用。
func (a *App) resolveTarget(sess *SessionMeta) LLMTarget {
	p := a.providerByID(sess.Provider)
	if p != nil && !p.IsSelectable() {
		p = nil // 停用（手动或自动）的提供方不接会话调用
	}
	if p == nil {
		p = a.firstSelectableProvider()
		if p == nil {
			return LLMTarget{BaseURL: a.Cfg.BaseURL, APIKey: a.Cfg.APIKey, Model: sess.Model, ContextWindow: DefaultContextWindow}
		}
		// 回落到了别的提供方：原会话模型不在它目录里就不带过去（换用其默认模型）
		if sess.Provider != "" && p.ID != sess.Provider && !p.hasModel(sess.Model) {
			return a.targetOf(p, "", "")
		}
	}
	return a.targetOf(p, sess.ProviderKey, sess.Model)
}

// targetOf 解析「某供应商 + 某把 Key（空=按模型归属找回/第一把可用）+ 某模型」
// 的调用目标；BaseURL 是供应商级的，密钥与协议（Key 级覆盖优先）来自那把 Key。
func (a *App) targetOf(p *ProviderCfg, keyID, modelID string) LLMTarget {
	k := p.resolveKey(keyID, modelID)
	t := LLMTarget{BaseURL: p.BaseURL, APIKey: k.APIKey, ContextWindow: DefaultContextWindow,
		ProviderID: p.ID, AltBaseURL: p.PublicBaseURL, Protocol: p.keyProtocol(k), KeyID: k.ID}
	t.Model = modelID
	if t.Model == "" && len(k.Models) > 0 {
		t.Model = k.Models[0].ID
	}
	for _, m := range k.Models {
		if m.ID == t.Model {
			if m.ContextWindow > 0 {
				t.ContextWindow = m.ContextWindow
			}
			t.SupportsImages = m.SupportsImages
			t.MaxOutput = m.MaxOutput
			break
		}
	}
	if t.Model == "" {
		t.Model = a.Cfg.Model // 兜底：老配置里的裸模型名
	}
	return t
}

// PresetByID 返回内置预置提供方模板（预置方直接用系统存好的 API 地址）。
func PresetByID(id string) (ProviderCfg, bool) {
	for _, p := range DefaultProviders() {
		if p.ID == id {
			return p, true
		}
	}
	return ProviderCfg{}, false
}

// guessModelMeta 按模型 ID 猜测上下文窗口与图片能力，供「拉取模型」自动配置。
// 猜不准就返回 0（运行时按 DefaultContextWindow 处理）；图片能力只报有把握的。
func guessModelMeta(id string) (ctx int, images bool) {
	s := strings.ToLower(id)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(s, x) {
				return true
			}
		}
		return false
	}
	images = has("4o", "gpt-4-turbo", "gpt-4.1", "gpt-5", "o3", "o4-mini", "claude",
		"gemini", "glm-4v", "glm-5.3-flash", "qwen-vl", "-vl-", "vision", "llava", "pixtral", "grok-4", "vlmodel",
		// DeepSeek 仅 flash 支持图片输入（官方 /v1/models 的 input_modalities: text+image）；
		// v4-pro 与老 deepseek-chat/reasoner/coder 均为纯文本，不列。
		"deepseek-flash")
	switch {
	case has("moonshot-v1-128k"):
		return 131072, images
	case has("moonshot-v1-32k"):
		return 32768, images
	case has("moonshot-v1-8k"):
		return 8192, images
	case has("deepseek"):
		return 1048576, images // V4 起上下文 1M；老端点拉不到元数据时的兜底猜测
	case has("kimi"), has("moonshot"), has("k2"):
		return 262144, images
	case has("gemini"):
		return 1048576, images
	case has("claude"):
		return 200000, images
	case has("gpt-5"), has("gpt-4.1"):
		return 1048576, images
	case has("gpt-4o"), has("gpt-4-turbo"):
		return 131072, images
	case has("o3"), has("o4"):
		return 200000, images
	case has("gpt-3"):
		return 16384, images
	case has("glm-4v"):
		return 8192, true
	case has("glm-5.3"):
		return 1048576, images // 5.3 系上下文 1M（模型卡片），flash 为多模态
	case has("glm-4"), has("glm-5"):
		return 131072, images
	case has("qwen-long"):
		return 10000000, images
	case has("qwen3"), has("qwen-max"), has("qwen-plus"), has("qwen-turbo"), has("qwen2.5"):
		return 131072, images
	case has("qwen"):
		return 32768, images
	case has("doubao"):
		return 262144, images
	case has("hunyuan"):
		return 256000, images
	case has("llama"):
		if has("128k", "3.1", "3.2", "3.3", "llama4") {
			return 131072, images
		}
		return 8192, images
	case has("mistral-large", "mistral-medium"):
		return 131072, images
	}
	return 0, images
}

// thinkingLevels 是全系统唯一的思考档位清单。档位与模型、渠道协议彻底解绑：
// 任何模型、任何渠道都显示同一套六档，由用户自己选——模型支不支持、服务端
// 认不认都不在这里猜（能自动降级就降级，报错让用户自己换档）。
// 内部值 → 显示名（前端 THINK_LABELS）：none/low/medium/high/xhigh/max →
// None/Low/Medium/High/Xhigh/Max。
func thinkingLevels() []string {
	return []string{"none", "low", "medium", "high", "xhigh", "max"}
}

// normalizeThinkingLevel 把档位串归一成六档之一（或空串=跟随服务端默认）。
// 老会话库里存的是上一代的 enabled/disabled：disabled 归一到 none，enabled
//（只开关不指深浅）归一到空串；六档之外的串不合规，ok=false 交调用方处理。
func normalizeThinkingLevel(s string) (level string, ok bool) {
	switch lv := strings.ToLower(strings.TrimSpace(s)); lv {
	case "", "enabled":
		return "", true
	case "disabled":
		return "none", true
	default:
		if containsStr(thinkingLevels(), lv) {
			return lv, true
		}
		return "", false
	}
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// ModelTestResult 是单个模型的连通性测试结果。
type ModelTestResult struct {
	ID  string `json:"id"`
	OK  bool   `json:"ok"`
	MS  int64  `json:"ms"`
	Err string `json:"err,omitempty"`
}

// TestProviderModels 对某把 Key 目录里的每个模型发一个 1 token 的极小请求测
// 连通，限 4 路并发，单请求 30 秒超时。ids 为空时测该 Key 的整个目录；
// keyID 指定用哪把 Key（不同分组的 Key 能调的模型不同，测试必须按 Key 走）。
func TestProviderModels(p *ProviderCfg, keyID string, ids []string) []ModelTestResult {
	k := p.resolveKey(keyID, "")
	if len(ids) == 0 {
		for _, m := range k.Models {
			ids = append(ids, m.ID)
		}
	}
	results := make([]ModelTestResult, len(ids))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			res := ModelTestResult{ID: id}
			start := time.Now()
			if err := pingModel(p, k, id); err != nil {
				res.Err = truncateRunes(err.Error(), 160)
			} else {
				res.OK = true
			}
			res.MS = time.Since(start).Milliseconds()
			results[i] = res
		}(i, id)
	}
	wg.Wait()
	return results
}

// pingModel 发一次最小化的协议请求，能通即可（按该 Key 生效的协议选端点与鉴权头）。
func pingModel(p *ProviderCfg, k ProviderKeyCfg, model string) error {
	base := strings.TrimRight(p.BaseURL, "/")
	key := k.APIKey
	url := base + "/chat/completions"
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]string{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	}
	setAuth := func(req *http.Request) {
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	switch p.keyProtocol(k) {
	case "anthropic":
		url = base + "/messages"
		payload = map[string]any{
			"model":      model,
			"messages":   []map[string]string{{"role": "user", "content": "ping"}},
			"max_tokens": 1,
			"stream":     false,
		}
		setAuth = func(req *http.Request) {
			req.Header.Set("anthropic-version", anthropicVersionHeader)
			if key != "" {
				req.Header.Set("x-api-key", key)
			}
		}
	case "responses":
		url = base + "/responses"
		// Responses 的输出下限是 16（低于会 400）
		payload = map[string]any{
			"model":             model,
			"input":             "ping",
			"max_output_tokens": 16,
			"stream":            false,
		}
	}
	body, _ := json.Marshal(payload)
	req, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	setAuth(req)
	client := newCompanyHTTPClient(30 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

// FetchModels 用供应商下某把 Key 拉取可用模型清单（GET {baseURL}/models）。
// keyID 为空时按「模型归属→第一把可用 Key」解析：单 Key 结构自然只有一把。
// 新条目按 ID 猜测上下文窗口与图片能力自动填好。DeepSeek 等较新的兼容端
// 会在清单里直接给出 context_window / max_output_tokens / input_modalities——
// 这些真实字段优先；只返回 id 的老式端点回退到按 ID 猜测。服务端的
// effort.supported_levels 不采用：档位全系统固定六档（thinkingLevels），
// 未知档位透传到前端只会变成「能选、后端 400」。
func (a *App) FetchModels(p *ProviderCfg, keyID string) ([]ModelCfg, error) {
	return fetchModelsWith(p, p.resolveKey(keyID, ""))
}

// fetchModelsWith 用一把具体 Key（密钥 + 它生效的协议）拉模型：中转站的不同
// 分组能看到的模型清单不一样，必须逐把 Key 各拉一次。
func fetchModelsWith(p *ProviderCfg, k ProviderKeyCfg) ([]ModelCfg, error) {
	key := k.APIKey
	url := strings.TrimRight(p.BaseURL, "/") + "/models"
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	if p.keyProtocol(k) == "anthropic" {
		// Anthropic 的 /v1/models 用 x-api-key + anthropic-version 鉴权（官方口径，
		// 与调用链 llmproto.go 的 anthropicProtocol 同一组头，不发 Bearer）
		req.Header.Set("anthropic-version", anthropicVersionHeader)
		if key != "" {
			req.Header.Set("x-api-key", key)
		}
	} else if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := newCompanyHTTPClient(15 * time.Second)
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("连接失败: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("服务返回 %d: %s", resp.StatusCode, truncateRunes(string(body), 200))
	}
	var out struct {
		Data []struct {
			ID              string   `json:"id"`
			Name            string   `json:"name"`
			DisplayName     string   `json:"display_name"` // Anthropic /v1/models 的同义字段
			ContextWindow   int      `json:"context_window"`
			ContextLength   int      `json:"context_length"` // 部分端点的同义字段
			MaxOutputTokens int      `json:"max_output_tokens"`
			InputModalities []string `json:"input_modalities"`
			Modalities      []string `json:"modalities"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("响应不是合法的模型清单 JSON: %w", err)
	}
	models := []ModelCfg{}
	for _, m := range out.Data {
		if strings.TrimSpace(m.ID) == "" {
			continue
		}
		ctx, img := guessModelMeta(m.ID)
		// Anthropic 的显示名在 display_name（Name 为空时取它）
		if m.Name == "" {
			m.Name = m.DisplayName
		}
		// 真实字段优先：上下文长度绝不取自输出上限
		if m.ContextWindow > 0 {
			ctx = m.ContextWindow
		} else if m.ContextLength > 0 {
			ctx = m.ContextLength
		}
		modalities := m.InputModalities
		if len(modalities) == 0 {
			modalities = m.Modalities
		}
		if len(modalities) > 0 {
			img = false
			for _, mod := range modalities {
				if strings.EqualFold(strings.TrimSpace(mod), "image") {
					img = true
					break
				}
			}
		}
		models = append(models, ModelCfg{
			ID: m.ID, Name: m.Name, ContextWindow: ctx,
			MaxOutput: m.MaxOutputTokens, SupportsImages: img,
		})
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("服务未返回任何模型")
	}
	return models, nil
}
