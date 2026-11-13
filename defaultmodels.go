package main

// 默认模型体系：三类默认（全局默认模型 / 派发任务默认模型 / 每条待办的任务模型）
// 与「不可用就报错、绝不静默回落」的取值链。
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
//   - dispatch_task / dispatch_sub → 派发任务默认模型（未设置＝跟随主会话：
//     派发者当前的 provider/model/key）→ 派发者也没手选时跟随全局默认模型；
//   - 待办 → 待办自选 → 派发任务默认模型 →（派发设置跟随主会话时，待办没有
//     主会话可跟）→ 全局默认模型。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// ModelRef 是一次「渠道 + 模型 + Key 归属」的完整指向。
type ModelRef struct {
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
	whereDispatchDefault = "「设置 → 模型」→「默认模型」→「派发任务默认模型」"
	whereTodoCard        = "待办卡片上的「模型」（编辑弹窗里也能选）"
	whereDispatchTool    = "本次派发的 model 参数（providerId/modelId 完整格式）"
	whereSessionModel    = "该会话输入框下方的模型芯片"
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
	ref, source, err := a.todoModelChain(task.ModelMode, derefModel(task.Model))
	if err != nil {
		return "", "", "", source, fmt.Errorf("任务模型不可用（来自%s）：%w", source, err)
	}
	return ref.Provider, ref.ProviderKey, ref.Model, source, nil
}

// ---------- 界面展示 ----------

// ModelRefView 是给界面看的「当前生效」：渠道显示名 · 模型 + 可用性 + 不可用原因。
type ModelRefView struct {
	Mode        string `json:"mode,omitempty"`   // 待办三档：dispatch/global/custom（其余场景空）
	Source      string `json:"source,omitempty"` // 这个值从哪来（全局默认模型 / 派发任务默认模型 / 跟随主会话 / 待办自选模型）
	Provider    string `json:"provider,omitempty"`
	ProviderKey string `json:"providerKey,omitempty"`
	Model       string `json:"model,omitempty"`
	// Text 是一眼看懂的生效值：「DC3 · deepseek-v4.1-flash-1017」；未设置时为空。
	Text      string `json:"text,omitempty"`
	Available bool   `json:"available"`
	Error     string `json:"error,omitempty"` // 不可用原因（界面红字原样显示）
}

// refView 把一次模型指向渲染成界面展示值（判定不过不报错，原因进 Error）。
func (a *App) refView(r ModelRef, mode, source, where string) ModelRefView {
	r = r.norm()
	v := ModelRefView{Mode: mode, Source: source, Provider: r.Provider, ProviderKey: r.ProviderKey, Model: r.Model}
	if r.Empty() {
		v.Error = "未设置"
		return v
	}
	if a.providerByID(r.Provider) != nil && r.Model != "" {
		v.Text = providerLabel(a, r.Provider) + " · " + r.Model
	}
	err := a.checkModelRef(r, where)
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
		return ModelRefView{Source: "全局默认模型", Error: "未设置：请选一个渠道 + 模型"}
	}
	return a.refView(ref, "", "全局默认模型", whereGlobalDefault)
}

// dispatchSettingsView 设置页「派发任务默认模型」一行：模式 + 当前实际生效值。
// 跟随主会话时生效值＝数字助理主对话当前手选的模型（主对话也没手选就是全局
// 默认模型）——界面上直接把这个值写出来，用户不必自己推。
func (a *App) dispatchSettingsView() map[string]any {
	if ref, custom := a.dispatchDefaultRef(); custom {
		return map[string]any{"mode": "custom", "ref": a.refView(ref, "", "派发任务默认模型", whereDispatchDefault)}
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
	v := a.refView(follow, "", source, whereGlobalDefault)
	if follow.Empty() {
		v.Error = "未设置：主对话未手选模型，全局默认模型也没设"
	}
	return map[string]any{"mode": "follow", "ref": v}
}

// todoModelView 待办卡片「模型：…（生效：…）」那行的数据：三档取值链算出来
// 的当前实际生效值与可用性（不可用时原因进 Error，卡片红字原样显示）。
func (a *App) todoModelView(mode string, custom ModelRef) ModelRefView {
	m := normTodoModelMode(mode)
	ref, source, err := a.todoModelChain(m, custom)
	if err != nil {
		shown := custom.norm()
		if m != todoModelCustom {
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
	return a.refView(ref, m, source, whereTodoCard)
}

// ---------- 保存（设置页） ----------

// setDefaultModels 保存三类默认里的前两类（设置页「默认模型」那一块）：
// 只校验格式并补齐 Key 归属，不拦可用性——不可用的选择由界面红字说明，派发时
// 按「不回落」报错。dispatchFollow=true 表示派发默认模型是「跟随主会话」（存空）。
func (a *App) setDefaultModels(global ModelRef, dispatch ModelRef, dispatchFollow bool) error {
	g := a.withKey(global)
	if g.Provider != "" && g.Model == "" {
		return fmt.Errorf("全局默认模型只选了渠道、没选模型：请到%s选全", whereGlobalDefault)
	}
	if g.Model != "" && g.Provider == "" {
		return fmt.Errorf("全局默认模型只选了模型、没选渠道：请到%s选全", whereGlobalDefault)
	}
	var d ModelRef
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

// applySessionModelDefault 手动新建会话（HTTP 人工入口）的模型取值：没选（全空）
// 就用全局默认模型；选了就先判定能不能用。返回要落到会话上的三件套。
// 不可用一律报错——建会话之前挡掉，不把跑不起来的空会话建出来；跟随全局默认时
// 把那个模型钉进会话（之后改全局默认不影响已有会话）。
func (a *App) applySessionModelDefault(provider, providerKey, model string) (string, string, string, error) {
	ref := ModelRef{Provider: provider, ProviderKey: providerKey, Model: model}.norm()
	if ref.Empty() {
		g := a.globalDefaultRef()
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

// handleDefaultModelsGet 设置页「默认模型」两块的数据：全局默认模型的选中值，
// 与派发任务默认模型的模式及当前实际生效值（跟随主会话时算到具体模型）。
func (a *App) handleDefaultModelsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"global":   a.globalSettingsView(),
		"dispatch": a.dispatchSettingsView(),
	})
}

// handleDefaultModelsSet 保存这两块：只校验格式并补齐 Key 归属，不拦可用性
// （用户可能先选好再启用渠道）——不可用由界面红字与派发时的「不回落」报错说明。
func (a *App) handleDefaultModelsSet(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Global   *ModelRef `json:"global"`
		Dispatch *ModelRef `json:"dispatch"` // null 或空＝跟随主会话
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON: "+err.Error())
		return
	}
	g := ModelRef{}
	if body.Global != nil {
		g = *body.Global
	}
	d := ModelRef{}
	follow := true
	if body.Dispatch != nil && !body.Dispatch.Empty() {
		d, follow = *body.Dispatch, false
	}
	if err := a.setDefaultModels(g, d, follow); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	a.broadcast(Event{Type: "providers"}) // 默认模型变了：前端模型菜单/状态栏跟着刷新
	writeJSON(w, 200, map[string]any{
		"global":   a.globalSettingsView(),
		"dispatch": a.dispatchSettingsView(),
	})
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
