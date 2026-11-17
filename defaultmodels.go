package main

// 默认模型体系：三类默认（全局默认模型 / 派发任务默认模型 / 每条待办的任务模型）
// 与「不可用就报错、绝不静默回落」的取值链；其中派发任务默认模型按用途分三套
//（执行/通用、方案设计、审查·审计），旧的那个键原位就是「执行/通用」。
//
// 设计意图（为什么这么做）：
//   - 一个模型只有「渠道 + 模型 + Key 归属」三者齐备才真的能调（多 Key 渠道里
//     重名模型只能靠 Key 归属区分），所以三类默认都存完整指向 ModelRef；老配置
//     里那个裸模型名（Config.Model）只留作老版本 exe 回退的镜像，不再参与解析。
//   - 之前到处「取第一个可用渠道 / 兜老配置裸名 / 已选模型被下线就换成第一个」，
//     用户看到的是模型或渠道莫名其妙变了，账单和输出都对不上。现在一律先判定再
//     建会话：不可用就点名「哪个渠道、哪个模型、什么原因、去哪个设置页改」，
//     不偷偷顶上（BaseURL 不通这类只有首轮才暴露的，就让它首轮报错留痕）。
//
// 取值链（每一环取到的值不可用就直接报错，不继续往下试）：
//   - 手动新建任务会话 → 全局默认模型；
//   - dispatch_task → 按用途：本次显式 model / 本用途配置 →（plan/review）跟随执行
//     通用 → 派发者手选的 provider/model/key → 全局默认模型；
//   - dispatch_sub 不走派发默认，模型链不变；档位继承派发者当前 Thinking；
//   - 待办 → 待办自选 → 派发任务默认模型（＝执行/通用）→（派发设置跟随主会话时，
//     待办没有主会话可跟）→ 全局默认模型。
//
// 派发用途（purpose）为什么这样设计：任务处在「出方案 / 干活 / 独立审查」哪个阶段，
// 对模型与推理强度的要求就不同，但用户不想每派一次都手调。所以三套默认各自可配，
// 方案/审查默认「跟随执行/通用」（推荐用法，也是老配置读出来的零值），要分开时
// 改成 custom。用途只决定默认模型与强度，与工作模式（mode）正交，也不落库。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
)

// ModelRef 是一次「渠道 + 模型 + Key 归属」的完整指向。
type ModelRef struct {
	Thinking    string `json:"thinking,omitempty"` // 空＝不指定，独立于模型选择
	Provider    string `json:"provider,omitempty"`
	ProviderKey string `json:"providerKey,omitempty"` // 模型归属的那把 Key（多 Key 渠道必须写准）
	Model       string `json:"model,omitempty"`
}

// norm 去空白：老配置与前端手填都可能带空格。
func (r ModelRef) norm() ModelRef {
	return ModelRef{
		Provider:    strings.TrimSpace(r.Provider),
		ProviderKey: strings.TrimSpace(r.ProviderKey),
		Model:       strings.TrimSpace(r.Model),
		Thinking:    r.Thinking,
	}
}

// Empty 一个模型都没选（未设置）。
func (r ModelRef) Empty() bool {
	r = r.norm()
	return r.Provider == "" && r.Model == ""
}

// Complete 渠道与模型都齐（Key 归属可空＝按模型名在该渠道里找回）。
func (r ModelRef) Complete() bool {
	r = r.norm()
	return r.Provider != "" && r.Model != ""
}

// Spec 完整格式 providerId/modelId（日志与错误信息用）。
func (r ModelRef) Spec() string {
	r = r.norm()
	switch {
	case r.Provider == "":
		return r.Model
	case r.Model == "":
		return r.Provider
	}
	return r.Provider + "/" + r.Model
}

// 派发用途（dispatch_task 的 purpose）固定三类：id 供工具参数与配置使用，Label 给
// 人与模型看。不设中文别名——别名会让用途变成自由文本，与「未知用途必须报错」冲突。

// dispatchPurpose 是一项派发用途的 id 与中文标签。
// 顺序即设置页三行的顺序，execute 固定第一项（它的视图就是原来那一行）。
type dispatchPurpose struct{ ID, Label string }

var dispatchPurposes = []dispatchPurpose{
	{"execute", "执行/通用"},
	{"plan", "方案设计"},
	{"review", "审查/审计"},
}

// dispatchPurposeCfg 是「方案设计 / 审查·审计」两套用途的配置。
// Mode：""/"follow"＝模型跟随「执行/通用」（推荐默认，也是老配置读出来的零值）；
// "custom"＝用下面的渠道/模型；其它值一律是**脏配置**，读写两处都必须报错，
// 不得降级成 follow（跟错了模型比报错更难查）。Thinking 与 Mode 无关：模型跟随
// 时档位仍可独立设置（空＝沿档位链继承）。
type DispatchPurposeCfg struct {
	Mode string `json:"mode,omitempty"`
	ModelRef
}

// 用途配置的 mode 取值（只认这两个 + 空）。
const (
	purposeModeFollow = "follow"
	purposeModeCustom = "custom"
)

// purposeLabel 用途的中文标签（报错与注记都用它说清是哪个用途）。
func purposeLabel(p string) string {
	for _, d := range dispatchPurposes {
		if d.ID == p {
			return d.Label
		}
	}
	return p
}

// purposeWhere 某用途在设置页里的位置：报错与界面红字都指到这里，用户才知道改哪一行。
func purposeWhere(p string) string {
	switch p {
	case "plan":
		return whereDispatchPlanDefault
	case "review":
		return whereDispatchReviewDefault
	}
	return whereDispatchDefault
}

// normPurposeMode 归一用途配置的 mode：只做大小写与空白处理，**不做同义猜测**
// （「跟随」「跟随主会话」这类中文一律按脏配置报错，不猜用户意思）。
func normPurposeMode(s string) (string, error) {
	v := strings.ToLower(strings.TrimSpace(s))
	switch v {
	case "", purposeModeFollow:
		return purposeModeFollow, nil
	case purposeModeCustom:
		return purposeModeCustom, nil
	}
	return "", fmt.Errorf("mode 值非法：%q（只能是 %s（跟随执行/通用）或 %s（指定渠道 + 模型），留空＝%s）",
		strings.TrimSpace(s), purposeModeFollow, purposeModeCustom, purposeModeFollow)
}

// 待办「任务模型」三档；空串＝跟随派发设置（默认，老数据没有该字段读出来就是空，
// 天然落在这一档，不需要额外迁移）。
const (
	todoModelFollowDispatch = ""       // 跟随派发设置
	todoModelGlobal         = "global" // 全局默认模型
	todoModelCustom         = "custom" // 指定渠道+模型
)

// 错误信息里的「去哪改」：只写一处，文案统一，用户照着点就能到。
const (
	whereGlobalDefault   = "「设置 → 模型」→「默认模型」→「全局默认模型」"
	whereDispatchDefault = "「设置 → 模型」→「默认模型」→「派发任务默认模型 · 执行/通用」"
	// 方案/审查的定位锚点：报错必须指到那个用途那一行，否则用户不知道改哪儿。
	whereDispatchPlanDefault   = "「设置 → 模型」→「默认模型」→「派发任务默认模型 · 方案设计」"
	whereDispatchReviewDefault = "「设置 → 模型」→「默认模型」→「派发任务默认模型 · 审查/审计」"
	whereTodoCard              = "待办卡片上的「模型」（编辑弹窗里也能选）"
	whereDispatchTool          = "本次派发的 model 参数（providerId/modelId 完整格式）"
	whereSessionModel          = "该会话输入框下方的模型芯片"
)

// errNoGlobalDefault 全局默认模型没设置：手动新建会话与待办派发都会撞到它。
var errNoGlobalDefault = errors.New("还没有设置全局默认模型：请到" + whereGlobalDefault + "选一个（渠道 + 模型）")

// ---------- 定位与判定 ----------

// locateModel 在渠道里找这个模型的归属那把 Key（多 Key 渠道重名模型靠它定位）。
func (p *ProviderCfg) locateModel(model string) (ProviderKeyCfg, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ProviderKeyCfg{}, false
	}
	for _, k := range p.KeyList() {
		for _, m := range k.Models {
			if m.ID == model {
				return k, true
			}
		}
	}
	return ProviderKeyCfg{}, false
}

// keyByID 按归属 id 找那把 Key；id 为空返回 false（＝没指定归属）。
func (p *ProviderCfg) keyByID(id string) (ProviderKeyCfg, bool) {
	id = strings.TrimSpace(id)
	if id == "" {
		return ProviderKeyCfg{}, false
	}
	for _, k := range p.KeyList() {
		if k.ID == id {
			return k, true
		}
	}
	return ProviderKeyCfg{}, false
}

// checkModelRef 判定一次模型指向能不能真的跑起来，可用返回 nil。不可用一律点名
// 「哪个渠道、哪个模型、什么原因、去哪个设置页改」——这是「不静默回落」的唯一
// 判据，建会话之前必须先过它。
//
// 判定项（与需求逐条对应）：渠道未配置 / 渠道被手动停用 / 渠道被自动停用 /
// 模型不在该渠道清单（含指定了 Key 归属但那把 Key 的分组里没有它）/ Key 缺失。
func (a *App) checkModelRef(r ModelRef, where string) error {
	if err := validateDefaultThinking(r.Thinking); err != nil {
		return fmt.Errorf("%s：%w", where, err)
	}
	r = r.norm()
	if r.Provider == "" && r.Model == "" {
		return fmt.Errorf("没有选模型：请到%s选一个（渠道 + 模型）", where)
	}
	if r.Provider == "" {
		return fmt.Errorf("模型「%s」没有指明渠道：请到%s选完整的渠道 + 模型（providerId/modelId）", r.Model, where)
	}
	p := a.providerByID(r.Provider)
	if p == nil {
		return fmt.Errorf("模型不可用：渠道「%s」本地没有配置（可能已被删除），模型「%s」。请到「设置 → 模型」重新添加该渠道，或到%s换一个模型",
			r.Provider, r.Model, where)
	}
	name := providerLabel(a, p.ID)
	switch {
	case p.Enabled != nil && !*p.Enabled:
		return fmt.Errorf("模型不可用：渠道「%s」（%s）已被手动停用，模型「%s」。请到「设置 → 模型」打开该渠道的开关，或到%s换一个模型",
			name, p.ID, r.Model, where)
	case p.AutoDisabled:
		return fmt.Errorf("模型不可用：渠道「%s」（%s）因连不上公司服务端被自动停用，模型「%s」。请到「设置 → 模型」手动开启该渠道，或到%s换一个模型",
			name, p.ID, r.Model, where)
	}
	if r.Model == "" {
		return fmt.Errorf("模型不可用：渠道「%s」（%s）没有指定模型。请到%s选一个模型", name, p.ID, where)
	}
	byName, hasModel := p.locateModel(r.Model)
	if !hasModel {
		return fmt.Errorf("模型不可用：该模型「%s」已不在渠道「%s」（%s）的模型清单里，请重选（可在该渠道卡片的「拉取模型」重新拉一次，或手动补上它）。请到%s重新选择",
			r.Model, name, p.ID, where)
	}
	key := byName
	if r.ProviderKey != "" {
		k, ok := p.keyByID(r.ProviderKey)
		if !ok {
			return fmt.Errorf("模型不可用：渠道「%s」（%s）里已经没有 Key 归属「%s」（那把 Key 被删了），模型「%s」。请到%s重选（会重新记下 Key 归属）",
				name, p.ID, r.ProviderKey, r.Model, where)
		}
		if !keyHasModel(k, r.Model) {
			return fmt.Errorf("模型不可用：模型「%s」不在渠道「%s」（%s）的 Key 归属「%s」这个分组里（这个分组只有别的模型）。请到%s重选，或在「设置 → 模型」给该分组拉取模型",
				r.Model, name, p.ID, r.ProviderKey, where)
		}
		key = k
	}
	if strings.TrimSpace(p.BaseURL) != "mock" && strings.TrimSpace(key.APIKey) == "" {
		return fmt.Errorf("模型不可用：渠道「%s」（%s）的 Key「%s」没有可用的 API Key（密钥为空或未填），模型「%s」。请到「设置 → 模型」该渠道的密钥栏补上，或到%s换一个模型",
			name, p.ID, providerKeyLabel(key), r.Model, where)
	}
	return nil
}

// keyHasModel 这把 Key 的分组里有没有这个模型。
func keyHasModel(k ProviderKeyCfg, model string) bool {
	for _, m := range k.Models {
		if m.ID == model {
			return true
		}
	}
	return false
}

// providerKeyLabel Key 的展示名（分组备注名，没写时用归属 id 或「默认」）。
func providerKeyLabel(k ProviderKeyCfg) string {
	if s := strings.TrimSpace(k.Label); s != "" {
		return s
	}
	if s := strings.TrimSpace(k.ID); s != "" {
		return s
	}
	return "默认"
}

// resolveModelRef 校验并解析成调用目标：不可用返回点名原因的错误，绝不换渠道。
func (a *App) resolveModelRef(r ModelRef, where string) (LLMTarget, error) {
	r = r.norm()
	if err := a.checkModelRef(r, where); err != nil {
		return LLMTarget{}, err
	}
	return a.targetOf(a.providerByID(r.Provider), r.ProviderKey, r.Model), nil
}

// withKey 补齐 Key 归属：指向里没写归属（老配置/前端手填）时按模型名在渠道里
// 找回它——存下来的默认值必须带准确的 Key 归属（多 Key 渠道的重名模型）。
func (a *App) withKey(r ModelRef) ModelRef {
	r = r.norm()
	if r.Provider == "" || r.Model == "" || r.ProviderKey != "" {
		return r
	}
	if p := a.providerByID(r.Provider); p != nil {
		if k, ok := p.locateModel(r.Model); ok {
			r.ProviderKey = k.ID
		}
	}
	return r
}

// bareModelRef 把裸模型名解析成完整指向：优先派发者自己的渠道（preferProvider），
// 否则按配置顺序找第一个（仍是可用的）提供它的渠道；一个都没有就报错，不猜渠道。
func (a *App) bareModelRef(model, preferProvider string) (ModelRef, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ModelRef{}, fmt.Errorf("没有指定模型")
	}
	if p := a.providerByID(strings.TrimSpace(preferProvider)); p != nil {
		if k, ok := p.locateModel(model); ok {
			return ModelRef{Provider: p.ID, ProviderKey: k.ID, Model: model}, nil
		}
	}
	hits := []ModelRef{}
	for i := range a.Cfg.Providers {
		p := &a.Cfg.Providers[i]
		if !p.IsSelectable() {
			continue
		}
		if k, ok := p.locateModel(model); ok {
			hits = append(hits, ModelRef{Provider: p.ID, ProviderKey: k.ID, Model: model})
		}
	}
	if len(hits) == 0 {
		if chans := a.modelChannelsOf(model); len(chans) > 0 {
			return ModelRef{}, fmt.Errorf("模型「%s」只存在于已停用的渠道里（%s）：请先在「设置 → 模型」启用它，或换一个模型",
				model, strings.Join(chans, "、"))
		}
		return ModelRef{}, fmt.Errorf("模型「%s」不在任何已配置渠道的模型清单里：请用 providerId/modelId 完整格式指明渠道，或到「设置 → 模型」给对应渠道拉取模型", model)
	}
	if len(hits) > 1 {
		log.Printf("[模型] 裸模型名「%s」有 %d 个渠道提供，取配置里第一个：%s", model, len(hits), hits[0].Spec())
	}
	return hits[0], nil
}

// validateDefaultThinking 默认配置只接受六档；旧会话 enabled/disabled 的兼容仍留在会话层。
func validateDefaultThinking(s string) error {
	if s != "" && !containsStr(thinkingLevels(), s) {
		return fmt.Errorf("推理强度无效：%s（可选 none/low/medium/high/xhigh/max 或空＝不指定）", s)
	}
	return nil
}

// defaultThinking 只继承档位，绝不借此换渠道或模型。空串一路透传到原会话默认。
func (a *App) defaultThinking(kind, own string) (string, string, error) {
	if err := validateDefaultThinking(own); err != nil {
		return "", "", err
	}
	if own != "" {
		return own, "", nil
	}
	if kind != "global" {
		d := a.Cfg.DispatchModel.Thinking
		if err := validateDefaultThinking(d); err != nil {
			return "", "", err
		}
		if d != "" {
			return d, "跟随派发设置", nil
		}
	}
	g := a.Cfg.DefaultModel.Thinking
	if err := validateDefaultThinking(g); err != nil {
		return "", "", err
	}
	if g != "" {
		return g, "跟随全局默认", nil
	}
	return "", "跟随会话默认", nil
}

func thinkingLabel(s string) string {
	if s == "" {
		return "不指定"
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func thinkingViewText(level, source string) string {
	if source != "" {
		return "（" + source + " · " + thinkingLabel(level) + "）"
	}
	return " · " + thinkingLabel(level)
}

// ---------- 三类默认 ----------

// globalDefaultRef 全局默认模型。未设置返回空引用（调用方按「未设置」报错）。
func (a *App) globalDefaultRef() ModelRef {
	if a == nil || a.Cfg == nil {
		return ModelRef{}
	}
	return a.Cfg.DefaultModel.norm()
}

// dispatchDefaultRef 派发任务默认模型：设置页选了「指定渠道+模型」时返回它；
// 空（含老配置没有这一项）＝跟随主会话，也就是这次改动之前的现状。
func (a *App) dispatchDefaultRef() (ModelRef, bool) {
	if a == nil || a.Cfg == nil {
		return ModelRef{}, false
	}
	r := a.Cfg.DispatchModel.norm()
	if !r.Complete() {
		return ModelRef{}, false
	}
	return a.withKey(r), true
}

// purposeCfg 取某用途的配置并归一 mode：execute 没有自己的 mode 键（它的配置就是
// 老的 dispatchModel：完整＝custom、空＝follow）；plan/review 读各自的配置。
// mode 非法一律返回错误（带用途定位），绝不降级成 follow。
func (a *App) purposeCfg(p string) (DispatchPurposeCfg, string, error) {
	cfg := DispatchPurposeCfg{}
	if a != nil && a.Cfg != nil {
		switch p {
		case "plan":
			cfg = a.Cfg.DispatchPlanModel
		case "review":
			cfg = a.Cfg.DispatchReviewModel
		default:
			cfg = DispatchPurposeCfg{ModelRef: a.Cfg.DispatchModel}
		}
	}
	mode, err := normPurposeMode(cfg.Mode)
	if err != nil {
		return cfg, "", fmt.Errorf("%s默认模型的 %s（来自%s）", purposeLabel(p), err.Error(), purposeWhere(p))
	}
	if p == "execute" {
		// 执行/通用的「自定义」就是它的模型指向齐全（历史语义，不加 mode 键）。
		if _, ok := a.dispatchDefaultRef(); ok {
			mode = purposeModeCustom
		} else {
			mode = purposeModeFollow
		}
	}
	cfg.Mode = mode
	return cfg, mode, nil
}

// followPurposeText 设置页里「跟随」那个选项的中文说法（两个跟随不是同一个东西）。
func followPurposeText(p string) string {
	if p == "execute" {
		return "跟随主会话"
	}
	return "跟随执行/通用"
}

// purposeRef 用途 → 具体模型指向，或空引用（空＝继续跟主会话，由调用方接
// inheritSessionModel）。跟随是**配置语义**（plan/review 出厂就是 follow），不是兜底：
// 本用途没配就显式跟执行/通用，执行/通用也没配才跟主会话。
// 返回 source/where 供界面与报错说清「这是哪个设置里的值、去哪改」。
func (a *App) purposeRef(p string) (ref ModelRef, source, where string, err error) {
	cfg, mode, cerr := a.purposeCfg(p)
	if cerr != nil {
		return ModelRef{}, "", purposeWhere(p), cerr
	}
	label := purposeLabel(p)
	where = purposeWhere(p)
	if mode == purposeModeCustom {
		ref = a.withKey(cfg.ModelRef)
		if !ref.Complete() {
			return ModelRef{}, label + "默认模型", where,
				fmt.Errorf("%s默认模型选了「指定渠道+模型」，但渠道或模型没选全：请到%s选全（或改回「%s」）",
					label, where, followPurposeText(p))
		}
		return ref, label + "默认模型", where, nil
	}
	if p == "execute" {
		return ModelRef{}, "跟随主会话", "", nil
	}
	if r, ok := a.dispatchDefaultRef(); ok {
		return r, "跟随执行/通用", whereDispatchDefault, nil
	}
	return ModelRef{}, "跟随执行/通用（再跟随主会话）", "", nil
}

// purposeThinking 派发用途的档位链：本次 thinking 参数 > 本用途 thinking >
// 执行/通用 thinking > 全局默认 thinking > 空（不指定，服务端默认）。
// own 是已经归一过的本次参数（空＝未传）；空串在整条链上历来就是「不指定」的表示。
func (a *App) purposeThinking(p, own string) (level, source string, err error) {
	if own != "" {
		return own, "本次派发指定", nil
	}
	if p != "execute" {
		cfg, _, cerr := a.purposeCfg(p)
		if cerr != nil {
			return "", "", cerr
		}
		if t := strings.TrimSpace(cfg.Thinking); t != "" {
			// 校验用原值（与全局/执行通用的配置口径一致），落会话用去空白后的值。
			if err := validateDefaultThinking(cfg.Thinking); err != nil {
				return "", "", fmt.Errorf("%s默认模型的推理强度不可用（来自%s）：%w", purposeLabel(p), purposeWhere(p), err)
			}
			return t, purposeLabel(p) + "默认模型", nil
		}
	}
	d := ""
	if a != nil && a.Cfg != nil {
		d = a.Cfg.DispatchModel.Thinking
	}
	if err := validateDefaultThinking(d); err != nil {
		return "", "", err
	}
	if d != "" {
		if p == "execute" {
			return d, "跟随派发设置", nil
		}
		return d, "跟随执行/通用", nil
	}
	g := ""
	if a != nil && a.Cfg != nil {
		g = a.Cfg.DefaultModel.Thinking
	}
	if err := validateDefaultThinking(g); err != nil {
		return "", "", err
	}
	if g != "" {
		return g, "跟随全局默认", nil
	}
	return "", "跟随会话默认", nil
}

// normTodoModelMode 归一待办三档模型取值（空与认不出的都按「跟随派发设置」）。
func normTodoModelMode(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case todoModelGlobal:
		return todoModelGlobal
	case todoModelCustom:
		return todoModelCustom
	}
	return todoModelFollowDispatch
}

// todoModelChain 待办的模型取值链：待办自选 → 派发任务默认模型 → 全局默认模型
// （派发设置是「跟随主会话」时——待办由系统调度器派出（DispatchedBy=todo-scheduler），
// 根本没有主会话可跟，所以落到全局默认模型，不静默空着也不乱猜）。
//
// 返回值：要钉到新会话上的指向（空＋err==nil＝新会话跟随全局默认，已校验可用）、
// 这一环的来源说明（卡片与报错都用它说清「哪个设置里的值」）。
func (a *App) todoModelChain(mode string, custom ModelRef) (ModelRef, string, error) {
	globalRef := func() (ModelRef, string, error) {
		g := a.globalDefaultRef()
		if g.Empty() {
			return ModelRef{}, "全局默认模型", errNoGlobalDefault
		}
		if err := a.checkModelRef(g, whereGlobalDefault); err != nil {
			return ModelRef{}, "全局默认模型", err
		}
		return ModelRef{}, "全局默认模型", nil
	}
	switch normTodoModelMode(mode) {
	case todoModelCustom:
		c := a.withKey(custom)
		if !c.Complete() {
			return ModelRef{}, "待办自选模型", fmt.Errorf("待办选了「指定渠道+模型」，但没有选全渠道与模型：请到%s选一个", whereTodoCard)
		}
		if err := a.checkModelRef(c, whereTodoCard); err != nil {
			return ModelRef{}, "待办自选模型", err
		}
		return c, "待办自选模型", nil
	case todoModelGlobal:
		return globalRef()
	default:
		if r, ok := a.dispatchDefaultRef(); ok {
			if err := a.checkModelRef(r, whereDispatchDefault); err != nil {
				return ModelRef{}, "派发任务默认模型", err
			}
			return r, "派发任务默认模型", nil
		}
		return globalRef()
	}
}

// todoDispatchModel 待办派发时该用什么模型：三档取值链解析出的结果。返回要钉到
// 新会话上的指向（都为空＝新会话跟随全局默认，已校验可用）与来源说明；不可用
// 直接把「来自哪个设置」写进错误（运行记录里用户能一眼看出该去改哪儿）。
func (a *App) todoDispatchModel(task *TaskItem) (provider, providerKey, model, source string, err error) {
	if _, _, err := a.defaultThinking("todo", task.Thinking); err != nil {
		return "", "", "", "待办推理强度", err
	}
	ref, source, err := a.todoModelChain(task.ModelMode, derefModel(task.Model))
	if err != nil {
		return "", "", "", source, fmt.Errorf("任务模型不可用（来自%s）：%w", source, err)
	}
	return ref.Provider, ref.ProviderKey, ref.Model, source, nil
}

// ---------- 界面展示 ----------

// ModelRefView 是给界面看的「当前生效」：渠道显示名 · 模型 + 可用性 + 不可用原因。
type ModelRefView struct {
	Thinking     string `json:"thinking,omitempty"` // 配置值；与展示的继承结果分开
	ThinkingText string `json:"thinkingText,omitempty"`
	Mode         string `json:"mode,omitempty"`   // 待办三档：dispatch/global/custom（其余场景空）
	Source       string `json:"source,omitempty"` // 这个值从哪来（全局默认模型 / 派发任务默认模型 / 跟随主会话 / 待办自选模型）
	Provider     string `json:"provider,omitempty"`
	ProviderKey  string `json:"providerKey,omitempty"`
	Model        string `json:"model,omitempty"`
	// Text 是一眼看懂的生效值：「DC3 · deepseek-v4.1-flash-1017」；未设置时为空。
	Text      string `json:"text,omitempty"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"` // 不可用原因（界面红字原样显示）
}

// refView 把一次模型指向渲染成界面展示值（判定不过不报错，原因进 Error）。
func (a *App) refView(r ModelRef, mode, source, where string) ModelRefView {
	r = r.norm()
	v := ModelRefView{Mode: mode, Source: source, Provider: r.Provider, ProviderKey: r.ProviderKey, Model: r.Model, Thinking: r.Thinking}
	kind := "global"
	if where == whereDispatchDefault {
		kind = "dispatch"
	}
	if where == whereTodoCard {
		kind = "todo"
	}
	level, thinkingSource, thinkingErr := a.defaultThinking(kind, r.Thinking)
	v.ThinkingText = thinkingViewText(level, thinkingSource)
	if r.Empty() {
		v.Error = "未设置"
		return v
	}
	if a.providerByID(r.Provider) != nil && r.Model != "" {
		v.Text = providerLabel(a, r.Provider) + " · " + r.Model + v.ThinkingText
	}
	err := a.checkModelRef(r, where)
	if thinkingErr != nil {
		err = thinkingErr
	}
	v.Available = err == nil
	if err != nil {
		v.Error = err.Error()
	}
	return v
}

// globalSettingsView 设置页「全局默认模型」一行：选中值 + 当前生效展示。
func (a *App) globalSettingsView() ModelRefView {
	ref := a.globalDefaultRef()
	if ref.Empty() {
		return ModelRefView{Source: "全局默认模型", Thinking: ref.Thinking, Error: "未设置：请选一个渠道 + 模型"}
	}
	return a.refView(ref, "", "全局默认模型", whereGlobalDefault)
}

// dispatchSettingsView 设置页「派发任务默认模型」一行：模式 + 当前实际生效值。
// 跟随主会话时生效值＝数字助理主对话当前手选的模型（主对话也没手选就是全局
// 默认模型）——界面上直接把这个值写出来，用户不必自己推。
func (a *App) dispatchSettingsView() map[string]any {
	if ref, custom := a.dispatchDefaultRef(); custom {
		return map[string]any{"mode": "custom", "thinking": a.Cfg.DispatchModel.Thinking, "ref": a.refView(ref, "", "派发任务默认模型", whereDispatchDefault)}
	}
	follow := ModelRef{}
	source := "跟随主会话"
	if m := a.currentAssistant(); m != nil && strings.TrimSpace(m.Provider) != "" {
		follow = ModelRef{Provider: m.Provider, ProviderKey: m.ProviderKey, Model: m.Model}
	}
	if follow.Empty() {
		follow = a.globalDefaultRef()
		source = "跟随主会话（主对话未手选 → 全局默认模型）"
	}
	follow.Thinking = a.Cfg.DispatchModel.Thinking
	v := a.refView(follow, "", source, whereDispatchDefault)
	v.Thinking = a.Cfg.DispatchModel.Thinking
	if follow.Empty() {
		v.Error = "未设置：主对话未手选模型，全局默认模型也没设"
	}
	return map[string]any{"mode": "follow", "thinking": a.Cfg.DispatchModel.Thinking, "ref": v}
}

// refText 一行看得懂的生效值：渠道显示名 · 模型 + 档位后缀（算不到就空）。
func (a *App) refText(r ModelRef, thinkingText string) string {
	r = r.norm()
	if r.Provider == "" || r.Model == "" || a.providerByID(r.Provider) == nil {
		return ""
	}
	return providerLabel(a, r.Provider) + " · " + r.Model + thinkingText
}

// purposeSettingsView 设置页「派发任务默认模型」的一行：mode / 档位配置值 /
// 当前实际生效值（模型与档位各自算到底）。execute 直接复用原来的那一行，
// 保证 GET 里 dispatch 字段与旧前端、待办组件零影响。
//
// 脏 mode：mode 原样返回配置里的字符串，ref.Error 写清「配置非法」——界面据此
// 把那行置红并提示可选值，用户能看出是配置错了而不是自己选错了。
func (a *App) purposeSettingsView(p string) map[string]any {
	v := map[string]any{"purpose": p, "label": purposeLabel(p)}
	if p == "execute" {
		for k, val := range a.dispatchSettingsView() {
			v[k] = val
		}
		return v
	}
	label, where := purposeLabel(p), purposeWhere(p)
	cfg, mode, err := a.purposeCfg(p)
	v["thinking"] = cfg.Thinking
	if err != nil {
		v["mode"] = strings.TrimSpace(cfg.Mode)
		v["ref"] = ModelRefView{Source: label + "默认模型", Error: err.Error()}
		return v
	}
	v["mode"] = mode
	level, thinkingSource, thinkingErr := a.purposeThinking(p, "")
	thinkingText := thinkingViewText(level, thinkingSource)
	var rv ModelRefView
	if mode == purposeModeCustom {
		ref := a.withKey(cfg.ModelRef)
		rv = ModelRefView{Source: label + "默认模型", Provider: ref.Provider, ProviderKey: ref.ProviderKey,
			Model: ref.Model, Thinking: cfg.Thinking, ThinkingText: thinkingText}
		if ref.Empty() {
			rv.Error = "未设置"
		} else {
			rv.Text = a.refText(ref, thinkingText)
			if cerr := a.checkModelRef(ref, where); cerr != nil {
				rv.Error = cerr.Error()
			} else {
				rv.Available = true
			}
		}
	} else {
		// 跟随执行/通用：直接拿执行/通用那一行算到底的值，只把档位换成自己的链。
		if base, ok := a.dispatchSettingsView()["ref"].(ModelRefView); ok {
			rv = base
		}
		rv.Source = "跟随执行/通用"
		rv.Thinking = cfg.Thinking
		rv.ThinkingText = thinkingText
		if rv.Provider != "" && rv.Model != "" {
			rv.Text = a.refText(ModelRef{Provider: rv.Provider, ProviderKey: rv.ProviderKey, Model: rv.Model}, thinkingText)
		}
	}
	if thinkingErr != nil {
		rv.Available = false
		rv.Error = thinkingErr.Error()
	}
	v["ref"] = rv
	return v
}

// dispatchPurposesView 三套用途的视图（顺序固定：执行/通用 / 方案设计 / 审查·审计）。
func (a *App) dispatchPurposesView() []map[string]any {
	out := make([]map[string]any, 0, len(dispatchPurposes))
	for _, d := range dispatchPurposes {
		out = append(out, a.purposeSettingsView(d.ID))
	}
	return out
}

// todoManagerModelView 只读长期助理当前实际目标，不把待办设置套进既有会话。
func (a *App) todoManagerModelView() ModelRefView {
	v := ModelRefView{Source: "由长期助理当前模型执行"}
	if a.Store == nil {
		v.Error = "长期助理尚未创建；执行时由长期助理当前模型处理"
		return v
	}
	m := a.currentAssistant()
	if m == nil {
		v.Error = "长期助理尚未创建；执行时由长期助理当前模型处理"
		return v
	}
	tgt, err := a.resolveTarget(m)
	if err != nil {
		v.Error = err.Error()
		return v
	}
	level, _ := normalizeThinkingLevel(m.Thinking)
	v.Provider, v.ProviderKey, v.Model = tgt.ProviderID, tgt.KeyID, tgt.Model
	v.Thinking, v.ThinkingText = level, " · "+thinkingLabel(level)
	v.Text = providerLabel(a, tgt.ProviderID) + " · " + tgt.Model + v.ThinkingText
	v.Available = true
	return v
}

// todoModelView 待办卡片「模型：…（生效：…）」那行的数据：三档取值链算出来
// 的当前实际生效值与可用性（不可用时原因进 Error，卡片红字原样显示）。
func (a *App) todoModelView(mode string, custom ModelRef) ModelRefView {
	m := normTodoModelMode(mode)
	ref, source, err := a.todoModelChain(m, custom)
	if err != nil {
		shown := custom.norm()
		switch source {
		case "派发任务默认模型":
			shown, _ = a.dispatchDefaultRef()
		case "全局默认模型":
			shown = a.globalDefaultRef()
		}
		v := ModelRefView{Mode: m, Source: source, Provider: shown.Provider, ProviderKey: shown.ProviderKey, Model: shown.Model}
		if a.providerByID(shown.Provider) != nil && shown.Model != "" {
			v.Text = providerLabel(a, shown.Provider) + " · " + shown.Model
		}
		v.Error = err.Error()
		return v
	}
	if ref.Empty() {
		ref = a.globalDefaultRef() // 落到全局默认：卡片显示它此刻的值
	}
	ref.Thinking = custom.Thinking // 模型来源与档位来源独立
	return a.refView(ref, m, source, whereTodoCard)
}

// ---------- 保存（设置页） ----------

// setDefaultModels 保存三类默认里的前两类（设置页「默认模型」那一块）：
// 只校验格式并补齐 Key 归属，不拦可用性——不可用的选择由界面红字说明，派发时
// 按「不回落」报错。dispatchFollow=true 表示派发默认模型是「跟随主会话」（存空）。
func (a *App) setDefaultModels(global ModelRef, dispatch ModelRef, dispatchFollow bool) error {
	if err := validateDefaultThinking(global.Thinking); err != nil {
		return fmt.Errorf("全局默认模型（来自%s）：%w", whereGlobalDefault, err)
	}
	if err := validateDefaultThinking(dispatch.Thinking); err != nil {
		return fmt.Errorf("派发任务默认模型 · 执行/通用（来自%s）：%w", whereDispatchDefault, err)
	}
	g := a.withKey(global)
	if g.Provider != "" && g.Model == "" {
		return fmt.Errorf("全局默认模型只选了渠道、没选模型：请到%s选全", whereGlobalDefault)
	}
	if g.Model != "" && g.Provider == "" {
		return fmt.Errorf("全局默认模型只选了模型、没选渠道：请到%s选全", whereGlobalDefault)
	}
	d := ModelRef{Thinking: dispatch.Thinking}
	if !dispatchFollow {
		d = a.withKey(dispatch)
		if !d.Complete() {
			return fmt.Errorf("派发任务默认模型选了「指定渠道+模型」，但渠道或模型没选全：请到%s选全（或改回「跟随主会话」）", whereDispatchDefault)
		}
	}
	a.Cfg.DefaultModel = g
	a.Cfg.DispatchModel = d
	if d.Complete() {
		log.Printf("[模型] 默认模型已更新：全局默认=%s，派发默认=%s", g.Spec(), d.Spec())
	} else {
		log.Printf("[模型] 默认模型已更新：全局默认=%s，派发默认=跟随主会话", g.Spec())
	}
	return nil
}

// setDispatchPurposes 校验并落装「方案设计 / 审查·审计」两套用途配置：
// mode 非法或 custom 不全一律报错（不落盘、不改运行值、绝不降级 follow）；
// follow 时模型三项清空——模型跟随不代表档位也要跟随（Thinking 保留）。
func (a *App) setDispatchPurposes(plan, review DispatchPurposeCfg) error {
	for _, item := range []struct {
		p   string
		cfg DispatchPurposeCfg
	}{{"plan", plan}, {"review", review}} {
		label, where := purposeLabel(item.p), purposeWhere(item.p)
		mode, err := normPurposeMode(item.cfg.Mode)
		if err != nil {
			return fmt.Errorf("%s默认模型的 %s（来自%s）", label, err.Error(), where)
		}
		ref := item.cfg.ModelRef
		if err := validateDefaultThinking(ref.Thinking); err != nil {
			return fmt.Errorf("%s默认模型：%w（来自%s）", label, err, where)
		}
		next := DispatchPurposeCfg{}
		switch mode {
		case purposeModeCustom:
			ref = a.withKey(ref)
			if !ref.Complete() {
				return fmt.Errorf("%s默认模型选了「指定渠道+模型」，但渠道或模型没选全：请到%s选全（或改回「%s」）",
					label, where, followPurposeText(item.p))
			}
			next = DispatchPurposeCfg{Mode: purposeModeCustom, ModelRef: ref}
		default:
			// 跟随：只留档位（ModelRef 的 Thinking），模型三项清空；磁盘上不写 mode 键，
			// 与老配置零值同形（回退到旧版 exe 时不会让旧版读到不认识的语义）。
			next = DispatchPurposeCfg{ModelRef: ModelRef{Thinking: ref.Thinking}}
		}
		if item.p == "plan" {
			a.Cfg.DispatchPlanModel = next
		} else {
			a.Cfg.DispatchReviewModel = next
		}
	}
	return nil
}

// purposePatch 是保存接口里一次用途配置的局部修改：键缺席＝保留，给了就替换。
type purposePatch struct {
	Mode        *string `json:"mode"`
	Provider    *string `json:"provider"`
	ProviderKey *string `json:"providerKey"`
	Model       *string `json:"model"`
	Thinking    *string `json:"thinking"`
}

// empty 没有任何字段：保存接口把它（与 null）当成「这一项整体清空」。
func (p purposePatch) empty() bool {
	return p.Mode == nil && p.Provider == nil && p.ProviderKey == nil && p.Model == nil && p.Thinking == nil
}

// mergePurposeCfg 逐字段合并一次用途配置（不校验完整度：那是 setDispatchPurposes
// 的统一关口）。语义与设计 §3.5 一致：
//   - mode 显式给成 follow/空：清空模型三项（档位保留，本次给了就用本次的）；
//   - mode 显式给成 custom：按字段替换；
//   - mode 缺席：纯粹逐字段合并——旧配置已是 custom 时可以直接换模型，不必重报 mode；
//     旧配置在跟随时只给渠道/模型会被**明确拒绝**（跟随时模型字段不生效，悄悄收下再
//     清掉就是用户输入静默丢失），要么给 mode=custom 要么先给 mode=follow。
func mergePurposeCfg(old DispatchPurposeCfg, patch purposePatch, label, where string) (DispatchPurposeCfg, error) {
	next := old
	if patch.Mode != nil {
		mode, err := normPurposeMode(*patch.Mode)
		if err != nil {
			return old, fmt.Errorf("%s默认模型的 %s（来自%s）", label, err.Error(), where)
		}
		if mode == purposeModeFollow {
			next = DispatchPurposeCfg{ModelRef: ModelRef{Thinking: old.Thinking}}
			if patch.Thinking != nil {
				next.Thinking = *patch.Thinking
			}
			return next, nil
		}
		next.Mode = purposeModeCustom
	}
	if patch.Provider != nil {
		next.Provider = *patch.Provider
	}
	if patch.ProviderKey != nil {
		next.ProviderKey = *patch.ProviderKey
	}
	if patch.Model != nil {
		next.Model = *patch.Model
	}
	if patch.Thinking != nil {
		next.Thinking = *patch.Thinking
	}
	// 换渠道或换模型时重新定位 Key 归属（旧的归属属于旧模型的，沿用它会让新模型
	// 落到不提供它的分组）；前端总是三件套一起发，这里只服务 call_api/脚本的单字段补丁。
	if (patch.Provider != nil || patch.Model != nil) && patch.ProviderKey == nil {
		next.ProviderKey = ""
	}
	if patch.Mode == nil && (patch.Provider != nil || patch.ProviderKey != nil || patch.Model != nil) {
		if mode, _ := normPurposeMode(next.Mode); mode == purposeModeFollow {
			return old, fmt.Errorf("%s默认模型现在跟随执行/通用，只提交渠道/模型不会生效：要让它生效请同时给 mode=%q（指定渠道 + 模型）；要清空就显式给 mode=%q（来自%s）",
				label, purposeModeCustom, purposeModeFollow, where)
		}
	}
	return next, nil
}

// applySessionModelDefault 手动新建会话（HTTP 人工入口）的模型取值：没选（全空）
// 就用全局默认模型；选了就先判定能不能用。返回要落到会话上的三件套。
// 不可用一律报错——建会话之前挡掉，不把跑不起来的空会话建出来；跟随全局默认时
// 把那个模型钉进会话（之后改全局默认不影响已有会话）。
func (a *App) applySessionModelDefault(provider, providerKey, model string) (string, string, string, error) {
	ref := ModelRef{Provider: provider, ProviderKey: providerKey, Model: model}.norm()
	if ref.Empty() {
		g := a.withKey(a.globalDefaultRef())
		if g.Empty() {
			return "", "", "", errNoGlobalDefault
		}
		if err := a.checkModelRef(g, whereGlobalDefault); err != nil {
			return "", "", "", err
		}
		return g.Provider, g.ProviderKey, g.Model, nil
	}
	if !ref.Complete() {
		return "", "", "", fmt.Errorf("新建会话的模型没选全（渠道与模型都要有）：请到%s选一个", whereSessionModel)
	}
	if err := a.checkModelRef(ref, whereSessionModel); err != nil {
		return "", "", "", err
	}
	t := a.targetOf(a.providerByID(ref.Provider), ref.ProviderKey, ref.Model)
	return t.ProviderID, t.KeyID, t.Model, nil
}

// ---------- HTTP ----------

// 默认设置端点串行合并、持久化后再发布，防止两个局部保存彼此覆盖。
// 只协调此端点；其他供应商/通用配置写入口仍沿用既有保存边界。
var defaultModelsSaveMu sync.Mutex

// handleDefaultModelsGet 设置页「默认模型」的数据：全局默认模型、三套派发用途
// （dispatch 键就是执行/通用那一套：结构与改动前完全一致，待办组件在读）与长期
// 助理当前实际目标；每项都带服务端算好的「当前生效值」。
func (a *App) handleDefaultModelsGet(w http.ResponseWriter, r *http.Request) {
	defaultModelsSaveMu.Lock()
	defer defaultModelsSaveMu.Unlock()
	writeJSON(w, 200, a.defaultModelsView())
}

// defaultModelsView 是 GET 与保存响应共用的视图（保存后前端直接拿它重渲染，
// 不在前端重算生效值）。
func (a *App) defaultModelsView() map[string]any {
	return map[string]any{
		"thinkingLevels":   thinkingLevels(),
		"global":           a.globalSettingsView(),
		"dispatch":         a.dispatchSettingsView(),
		"dispatchPurposes": a.dispatchPurposesView(),
		"manager":          a.todoManagerModelView(),
	}
}

// handleDefaultModelsSet 保存这两块：只校验格式并补齐 Key 归属，不拦可用性
// （用户可能先选好再启用渠道）——不可用由界面红字与派发时的「不回落」报错说明。
func (a *App) handleDefaultModelsSet(w http.ResponseWriter, r *http.Request) {
	// 省略字段＝保留；null 或 {}＝显式清空（dispatch 清空即跟随）。
	// 旧客户端同时提交两项仍兼容，但空请求对象不再隐式删除两项。
	var body map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body == nil {
		writeErr(w, 400, "请求体必须是 JSON 对象")
		return
	}
	decodeRef := func(name string, old ModelRef) (ModelRef, error) {
		raw, present := body[name]
		if !present {
			return old, nil
		}
		var ref ModelRef
		if err := json.Unmarshal(raw, &ref); err != nil {
			return ModelRef{}, fmt.Errorf("%s 不是合法模型引用: %w", name, err)
		}
		return ref, nil
	}
	defaultModelsSaveMu.Lock()
	defer defaultModelsSaveMu.Unlock()
	g, err := decodeRef("global", a.Cfg.DefaultModel)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	d, err := decodeRef("dispatch", a.Cfg.DispatchModel)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// 方案/审查两套用途：键缺席＝保留；null 与 {} 都是**整体清空**（回到跟随、档位
	// 也清，与旧的 dispatch:{} 语义统一）；只给某一项（如 {"thinking":"high"}）
	// 则逐字段合并。mode 非法或「跟随态只给模型字段」在这里就 400，不落盘。
	decodePurpose := func(name, purpose string, old DispatchPurposeCfg) (DispatchPurposeCfg, error) {
		raw, present := body[name]
		if !present {
			return old, nil
		}
		if strings.TrimSpace(string(raw)) == "null" {
			return DispatchPurposeCfg{}, nil
		}
		var patch purposePatch
		if err := json.Unmarshal(raw, &patch); err != nil {
			return old, fmt.Errorf("%s 不是合法的用途配置: %w", name, err)
		}
		if patch.empty() {
			return DispatchPurposeCfg{}, nil
		}
		return mergePurposeCfg(old, patch, purposeLabel(purpose), purposeWhere(purpose))
	}
	plan, err := decodePurpose("dispatchPlan", "plan", a.Cfg.DispatchPlanModel)
	if err != nil {
		writeErr(w, 400, "本次保存未生效（整份默认模型都未改动，磁盘也没写入）——"+err.Error())
		return
	}
	review, err := decodePurpose("dispatchReview", "review", a.Cfg.DispatchReviewModel)
	if err != nil {
		writeErr(w, 400, "本次保存未生效（整份默认模型都未改动，磁盘也没写入）——"+err.Error())
		return
	}
	// 在副本上归一/校验：一次保存是个原子整体，任一行不合格就整份不落盘（不做部分
	// 保存），错误里点名根因在哪一行，用户改完再提交一次。
	next := *a.Cfg
	candidate := &App{Cfg: &next}
	if err := candidate.setDefaultModels(g, d, d.Empty()); err != nil {
		writeErr(w, 400, "本次保存未生效（整份默认模型都未改动，磁盘也没写入）——"+err.Error())
		return
	}
	if err := candidate.setDispatchPurposes(plan, review); err != nil {
		writeErr(w, 400, "本次保存未生效（整份默认模型都未改动，磁盘也没写入）——"+err.Error())
		return
	}
	if err := next.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	a.Cfg.DefaultModel, a.Cfg.DispatchModel = next.DefaultModel, next.DispatchModel
	a.Cfg.DispatchPlanModel, a.Cfg.DispatchReviewModel = next.DispatchPlanModel, next.DispatchReviewModel
	a.broadcast(Event{Type: "providers"})
	writeJSON(w, 200, a.defaultModelsView())
}

// migrateDefaultModel 把单模型时代那个裸模型名（Config.Model，向导只写过一次）
// 折算成「渠道 + 模型 + Key 归属」的完整格式，写进 DefaultModel。
//
// 定位规则：按配置顺序扫所有渠道（含每把 Key 的清单），第一个提供它的渠道即命中
// ——多个渠道都能提供时取配置里第一个，并在日志里写明（用户要知道落在哪）。
// 一个都定位不到就留空（DefaultModel 为空串＝未设置，设置页如实显示「未设置」）。
//
// 只在配置文件里压根没有 defaultModel 这一键时执行（用户显式清空过就不再复活）。
func migrateDefaultModel(cfg *Config) (bool, string) {
	if !cfg.DefaultModel.Empty() {
		return false, ""
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return true, "老配置没有裸模型名，全局默认模型留空（设置页显示「未设置」）"
	}
	hits := []ModelRef{}
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if k, ok := p.locateModel(model); ok {
			hits = append(hits, ModelRef{Provider: p.ID, ProviderKey: k.ID, Model: model})
		}
	}
	if len(hits) == 0 {
		return true, fmt.Sprintf("裸模型名「%s」不在任何渠道的模型清单里，全局默认模型留空（设置页显示「未设置」）", model)
	}
	cfg.DefaultModel = hits[0]
	if len(hits) > 1 {
		all := make([]string, 0, len(hits))
		for _, h := range hits {
			all = append(all, h.Spec())
		}
		return true, fmt.Sprintf("裸模型名「%s」有 %d 个渠道提供（%s），取配置里第一个：%s", model, len(hits), strings.Join(all, "、"), hits[0].Spec())
	}
	return true, fmt.Sprintf("裸模型名「%s」→ %s", model, hits[0].Spec())
}
