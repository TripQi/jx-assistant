package main

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// 数字助理（assistant 模式）总控半球。设计参考 DSH 版数字助理插件：
// 总控不亲自读写文件、不执行命令，只做「看清需求 → 新建或复用任务会话 →
// 把需求交代清楚 → 等它做完 → 用压缩摘要向用户汇报」。

// ---------- 工具集 ----------

func listSessionsTool() Tool {
	return Tool{
		Spec: toolSpec("list_sessions", "列出最近的任务会话（默认 12 条，最多 40 条）：id、标题、工作区、更新时间、是否在跑、是否由数字助理派生。用来判断新需求与哪个会话相关。",
			objSchema(map[string]any{
				"limit": intProp("返回条数，默认 12，上限 40"),
			})),
		Run: runListSessions,
	}
}

func runListSessions(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Limit int `json:"limit"`
	}
	if len(args) > 0 {
		_ = json.Unmarshal(args, &p)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 12
	}
	if limit > 40 {
		limit = 40
	}
	list, err := t.app.Store.List()
	if err != nil {
		return "", err
	}
	lines := []string{}
	for _, m := range list {
		if m.Mode == "assistant" || m.ID == t.sess.ID {
			continue
		}
		if len(lines) >= limit {
			break
		}
		running := ""
		if lock := t.app.sessLock(m.ID); lock.TryLock() {
			lock.Unlock()
		} else {
			running = "｜正在运行"
		}
		ws := m.Workspace
		if ws == "" {
			ws = t.app.Cfg.Workspace
		}
		origin := ""
		if m.ParentID != "" || m.DispatchedBy != "" {
			origin = "｜我派生的"
		}
		lines = append(lines, fmt.Sprintf("- %s｜id=%s｜工作区=%s｜更新 %s%s%s",
			m.Title, m.ID, ws, m.UpdatedAt, running, origin))
	}
	if len(lines) == 0 {
		return "（除主对话外还没有别的会话）", nil
	}
	return strings.Join(lines, "\n"), nil
}

func readSummaryTool() Tool {
	return Tool{
		Spec: toolSpec("read_session_summary", "读某个会话做过什么的摘要（轮数与步数、最终答复全文、最终答复之后的结尾消息、过程要点或中断断点附近的原文、与实际落盘核对过的改动文件清单）。不含工具参数细节。",
			objSchema(map[string]any{
				"session_id": strProp("目标会话 id，来自 list_sessions"),
			}, "session_id")),
		Run: runReadSummary,
	}
}

func runReadSummary(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	sid := strings.TrimSpace(p.SessionID)
	if sid == "" {
		return "", fmt.Errorf("session_id 不能为空")
	}
	if sid == t.sess.ID {
		return "", fmt.Errorf("不能读主对话自己的摘要")
	}
	m, err := t.app.Store.Get(sid)
	if err != nil {
		return "", fmt.Errorf("会话 %s 不存在，先用 list_sessions 看看 id", sid)
	}
	rows, err := t.app.Store.MessagesWithSeq(sid)
	if err != nil {
		return "", err
	}
	return renderSummary(t.app, m, rows, sessionInterrupted(rows)), nil
}

func dispatchTaskTool() Tool {
	return Tool{
		Spec: toolSpec("dispatch_task", "新建一个工作任务会话并把需求交给它后台执行（它拥有读写文件与执行命令的全部工具）。本工具立即返回，任务完成后你会自动收到【派发回报】（含它的最终答复全文、轮数/步数、与实际落盘核对过的改动文件清单）。工作任务与我们的聊天记录完全隔离——你就像用户一样只给它发一段自足的消息：说清目标与已知背景（涉及的文件/目录给绝对路径）即可；怎么干、怎么验证交给它自己判断，不要替它列步骤清单和验收矩阵。可选参数与人工在界面新建工作任务一致：mode 选工作模式、plan_mode 让它以计划模式启动、attachments 给它带上现成文件、model 指定模型（providerId/modelId 完整格式）、permission 调它的权限档（只能同等或降权，绝不允许增权）。",
			objSchema(map[string]any{
				"task":        strProp("自足的任务描述：目标与已知背景（涉及的文件/目录给绝对路径）——工作任务看不到本对话的任何记录；不要替它列步骤清单和验收矩阵"),
				"title":       strProp("给新任务起的标题，可省略"),
				"workspace":   strProp("可选：工作任务的工作目录（绝对路径），默认沿用当前工作区；与 remote_workspace 互斥"),
				"remote_workspace": strProp("可选：工作任务使用的 SSH 远程工作区（本对话的二号工作区），取值照抄环境快照「可派发远程工作区」行里的完整地址（user@host:dir）。与 workspace 互斥；不填＝沿用当前环境（本会话挂了远程则继承同一远程）"),
				"model":       strProp("可选：任务会话使用的模型，格式 providerId/modelId（如 deepseek/deepseek-v4-pro）或纯 modelId；不填沿用默认"),
				"mode":        strProp("可选：工作模式，取值与人工新建会话的模式下拉同一份清单（" + chatModesDoc() + "）；按任务性质选——办公文档/表格/资料选 office，改代码/编译排障选 coding，用户不懂编程的编程需求选 beginner-coding，把做法沉淀成技能教程选 skill-create。不填＝通用工作任务（task，与改动前一致）"),
				"plan_mode":   boolProp("可选：true＝让新任务以计划模式启动（与人工建会话勾选「计划模式」一致）：它先只读调研、提交计划等用户批准后才动手；适合改动面大、跨多文件、有风险的任务"),
				"attachments": arrayProp("可选：附件文件路径列表（本机绝对路径，如 D:\\资料\\需求.docx）。文件会复制进新任务的工作区并登记为上传（交付时可在 deliver_result 的文件项填 upload 交回用户原处），与人工建会话上传附件完全一致；路径不存在、是目录，或新任务是远程工作区时，整个派发被拒绝并报错", strProp("本机文件绝对路径，如 D:\\资料\\需求.docx")),
				"permission":  strProp("可选：新任务的权限档，取值 read-only（全电脑只读）/ workspace-write（修改工作区）/ full（完全访问），与权限面板同一套档位。只能**同等或降权**——不得高于本会话（数字助理）当前权限（见环境快照的「权限模式」行），超出即拒绝并报错（不静默钳制）。不填＝修改工作区（本会话权限更窄时取本会话权限）。要改文件、跑命令的任务别降档；全电脑只读调研/查看可降为 read-only"),
			}, "task")),
		Run: runDispatchTask,
	}
}

// deliverSub 把派发需求投递进目标会话——走与用户发消息同一套投递（见 runtime.go
// 的 deliverToSession）：目标空闲=落库并起 runSubAgent；正在执行=一律插话
//（注入它当前这一轮，下一次调用模型就能看到——AI 发话不等排队）。
// dispatch_task / dispatch_sub 派发的是新会话、必然空闲；send_to_session 对已有
// 会话中途发话，插话正是它的语义。落库身份是 kind=aiuser（AI 发出的用户消息）：
// 系统按用户消息同等对待（开新回合、组装时后面跟一条时间提示词），前端是
// 右对齐偏白的「AI 用户」气泡。
func (t *toolContext) deliverSub(sub *SessionMeta, text, note string) string {
	d := t.app.deliverToSession(sub, text, kindAiUser, "task")
	if !d.Started {
		return subQueueNote(sub, note)
	}
	// 当前主对话的 SSE 若还连着，先给它推一条派发确认
	t.emit(Event{Type: "sub_start", SessionID: sub.ID, Title: sub.Title})
	go t.app.runSubAgent(t.sess, sub, d.Run)
	return fmt.Sprintf("已派发后台执行：会话 %s「%s」。%s本工具立即返回，不要等待；"+
		"被派发的会话完成后系统会自动收到【派发回报】并触发你处理回报。现在可以先回用户一句「已经安排」。", sub.ID, sub.Title, note)
}

// dispatchModelChoice 解析派发工具的模型参数，返回应落到新会话上的
// (provider, providerKey, model)。取值链（见 defaultmodels.go 文件头）：
// 显式 model 参数 > 派发任务默认模型（设置页）> 跟随主会话（派发者手选的
// 渠道/模型/Key；派发者自己也没手选时跟随全局默认模型）。任一环取到的值不可用
// 就直接报错，不往下试也不换渠道——废会话在建出来之前就被挡掉。
func (t *toolContext) dispatchModelChoice(model string) (provider, providerKey, mdl string, err error) {
	if m := strings.TrimSpace(model); m != "" {
		ref, rerr := t.app.resolveDispatchModelRef(m, t.sess.Provider)
		if rerr != nil {
			return "", "", "", rerr
		}
		tgt, rerr := t.app.resolveModelRef(ref, whereDispatchTool)
		if rerr != nil {
			return "", "", "", rerr
		}
		return tgt.ProviderID, tgt.KeyID, tgt.Model, nil
	}
	if ref, custom := t.app.dispatchDefaultRef(); custom {
		tgt, rerr := t.app.resolveModelRef(ref, whereDispatchDefault)
		if rerr != nil {
			return "", "", "", fmt.Errorf("派发任务默认模型不可用：%w", rerr)
		}
		return tgt.ProviderID, tgt.KeyID, tgt.Model, nil
	}
	// 派发设置是「跟随主会话」（默认）：派发者手选的渠道/模型/Key 原样继承——
	// 多 Key 供应商连「哪把 Key」一起继承，重名模型才不会换到另一把 Key 上。
	if pid := strings.TrimSpace(t.sess.Provider); pid != "" {
		ref := ModelRef{Provider: pid, ProviderKey: t.sess.ProviderKey, Model: t.sess.Model}
		if strings.TrimSpace(ref.Model) == "" {
			// 派发者只钉了渠道没钉模型：按该渠道的 Key 解析出具体模型（会话运行时
			// 就是这么算的），先落到具体模型上再校验。
			if p := t.app.providerByID(pid); p != nil {
				k := p.resolveKey(t.sess.ProviderKey, "")
				if len(k.Models) > 0 {
					ref.Model, ref.ProviderKey = k.Models[0].ID, k.ID
				}
			}
		}
		tgt, rerr := t.app.resolveModelRef(ref, whereSessionModel)
		if rerr != nil {
			return "", "", "", fmt.Errorf("派发设置为「跟随主会话」，但本会话的模型不可用：%w", rerr)
		}
		return tgt.ProviderID, tgt.KeyID, tgt.Model, nil
	}
	// 派发者自己也没手选（它跟全局默认）：子会话同样跟全局默认，先确认它真的可用。
	ref := t.app.globalDefaultRef()
	if ref.Empty() {
		return "", "", "", errNoGlobalDefault
	}
	if err := t.app.checkModelRef(ref, whereGlobalDefault); err != nil {
		return "", "", "", fmt.Errorf("派发设置为「跟随主会话」而本会话没手选模型，落到全局默认模型时不可用：%w", err)
	}
	return "", "", "", nil
}
// providerLabel 渠道的中文名（没配名字时回退 id）。
func providerLabel(a *App, id string) string {
	if a == nil || a.Cfg == nil {
		return id
	}
	if p := a.providerByID(id); p != nil && strings.TrimSpace(p.Name) != "" {
		return strings.TrimSpace(p.Name)
	}
	return id
}

// modelChannelsOf 查本地配置里哪些渠道提供这个模型名：返回「providerId/modelId
//（渠道名）」列表（按配置顺序全列——同一模型常由多个渠道提供，只列一个会误导）。
// 匹配 id 精确/大小写不敏感，也认渠道里配的中文显示名（模型可能拿显示名来问）。
func (a *App) modelChannelsOf(model string) []string {
	model = strings.TrimSpace(model)
	if model == "" || a == nil || a.Cfg == nil {
		return nil
	}
	out := []string{}
	for i := range a.Cfg.Providers {
		p := &a.Cfg.Providers[i]
		for _, mc := range p.Models {
			if mc.ID == model || strings.EqualFold(mc.ID, model) ||
				(strings.TrimSpace(mc.Name) != "" && strings.EqualFold(strings.TrimSpace(mc.Name), model)) {
				out = append(out, p.ID+"/"+mc.ID+"（"+providerLabel(a, p.ID)+"）")
				break
			}
		}
	}
	return out
}

// modelChannelHint 给「裸模型名走错了渠道」的场景拼提示：本地配置里这个模型名
// 确实存在、而本次调用的渠道不提供它时，把可用渠道全列出来并提示改用完整格式；
// 返回空串＝不提（名字没在任何渠道清单里，或本次渠道本身就提供它）。
func (a *App) modelChannelHint(model, curProviderID string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	chans := a.modelChannelsOf(model)
	if len(chans) == 0 {
		return ""
	}
	for _, c := range chans {
		if curProviderID != "" && strings.HasPrefix(c, curProviderID+"/") {
			return "" // 本次渠道就提供它：报错与渠道无关，不要误导
		}
	}
	cur := ""
	if curProviderID != "" {
		cur = "，本次调用走的是渠道「" + providerLabel(a, curProviderID) + "」（" + curProviderID + "）"
	}
	return "本地配置里模型名「" + model + "」存在于这些渠道：" + strings.Join(chans, "、") + cur +
		"；同一模型名可能在多个渠道提供，请用 providerId/modelId 完整格式指明渠道"
}

// resolveDispatchModelRef 把派发工具的 model 参数解析成完整指向（dispatch_task /
// dispatch_sub 共用）：完整格式（providerId/modelId，渠道在本地配置里）直接用它
// 并补全 Key 归属；裸模型名只认「派发者自己的渠道」与「全本地唯一提供它的渠道」
// 这两神——多个渠道都提供就拒绝并列出清单，让他改用完整格式，绝不替他猜渠道。
func (a *App) resolveDispatchModelRef(req, preferProvider string) (ModelRef, error) {
	m := strings.TrimSpace(req)
	if m == "" {
		return ModelRef{}, nil
	}
	if i := strings.Index(m, "/"); i > 0 {
		ref := ModelRef{Provider: m[:i], Model: m[i+1:]}
		if a.providerByID(ref.Provider) == nil {
			return ModelRef{}, fmt.Errorf("model=%q 里的渠道「%s」本地没有配置：请改用已配置的渠道，或到「设置 → 模型」添加它", m, ref.Provider)
		}
		return a.withKey(ref), nil
	}
	if p := a.providerByID(strings.TrimSpace(preferProvider)); p != nil {
		if k, ok := p.locateModel(m); ok {
			return ModelRef{Provider: p.ID, ProviderKey: k.ID, Model: m}, nil
		}
	}
	hits := []ModelRef{}
	for i := range a.Cfg.Providers {
		p := &a.Cfg.Providers[i]
		if !p.IsSelectable() {
			continue
		}
		if k, ok := p.locateModel(m); ok {
			hits = append(hits, ModelRef{Provider: p.ID, ProviderKey: k.ID, Model: m})
		}
	}
	switch len(hits) {
	case 0:
		if chans := a.modelChannelsOf(m); len(chans) > 0 {
			return ModelRef{}, fmt.Errorf("model=%q 是裸模型名，提供它的渠道都已停用（%s）：请先用「设置 → 模型」启用，或用 providerId/modelId 指明渠道", m, strings.Join(chans, "、"))
		}
		return ModelRef{}, fmt.Errorf("model=%q 是裸模型名，本地任何渠道的模型清单里都没有它：请用 providerId/modelId 完整格式指明渠道，并在「设置 → 模型」给该渠道拉取模型", m)
	case 1:
		return hits[0], nil
	}
	specs := make([]string, 0, len(hits))
	for _, h := range hits {
		specs = append(specs, h.Provider+"/"+h.Model)
	}
	return ModelRef{}, fmt.Errorf("model=%q 是裸模型名，而本地有 %d 个渠道都提供它：%s；同一模型名可能在多个渠道提供，请用 providerId/modelId 完整格式指明渠道", m, len(hits), strings.Join(specs, "、"))
}

// resolveDispatchMode 解析 dispatch_task 的 mode 参数：不传＝通用工作任务
// （task，与改动前的默认一致）；传入的值必须落在「人工新建会话」的模式清单里
// （chatModes，见 chatmodes.go）——前端模式下拉与它同一份数据源。清单外的值
// （含 assistant / aux 这类内部模式）一律拒绝并列出可选值，让模型改口重试。
func resolveDispatchMode(req string) (string, error) {
	req = strings.TrimSpace(req)
	if req == "" {
		return "task", nil
	}
	if m := chatModeByValue(req); m != nil {
		return m.Value, nil
	}
	return "", fmt.Errorf("mode=%q 不是可选的工作模式：只能是 %s（与人工新建会话的模式下拉同一份清单）；不传＝通用工作任务（task）", req, chatModesDoc())
}

// dispatchPermission 解析派发工具的 permission 参数（dispatch_task / dispatch_sub
// 共用），返回被派发会话应使用的权限档。规则：只能派发**不高于**本会话权限的
// 会话——可同等、可削减，**绝不允许增权**（数字助理不能借派发给自己或派出去
// 的会话提权）。超出即拒绝并给明确报错：不静默钳制——钳制会让模型以为已按请求
// 生效，报错才能让它改口，或先请用户把本会话权限调宽。
// 档位与权限面板同一套（read-only / workspace-write / full；danger-full-access
// 按别名折成 full，与 request_permission 同口径）。不传＝沿用「修改工作区」
// 这一既有默认，本会话权限更窄时取本会话权限（只降不升）。
func (t *toolContext) dispatchPermission(req string) (string, error) {
	cur := permModeOf(t.sess)
	req = strings.TrimSpace(req)
	if req == "danger-full-access" {
		req = permFull
	}
	if req == "" {
		if sandboxPolicyRank(cur) < sandboxPolicyRank(permWorkspace) {
			return cur, nil
		}
		return permWorkspace, nil
	}
	if req != permReadOnly && req != permWorkspace && req != permFull {
		return "", fmt.Errorf("permission=%q 不是有效档位：只能是 %s（全电脑只读）/ %s（修改工作区）/ %s（完全访问）", req, permReadOnly, permWorkspace, permFull)
	}
	if sandboxPolicyRank(req) > sandboxPolicyRank(cur) {
		return "", fmt.Errorf("permission=%s 超出本会话当前权限（%s）：派发只能同等或降权，绝不允许增权。请改为「%s」或更窄的档位重试；确需更宽权限时，先请用户把本会话（数字助理）的权限调宽再派发",
			req, permModeLabel(cur), permModeLabel(cur))
	}
	return req, nil
}

// normalizeDispatchAttachments 归一化 dispatch_task 的 attachments 参数：去掉
// 空白项与重复项（同一次派发里同一个文件只登记一次）。只做形态归一——路径
// 是否存在、是不是文件、能不能复制，由 registerUploadPath 在建会话时按桌面端
// 上传同一套校验与文案报错。
func normalizeDispatchAttachments(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, raw := range in {
		p := strings.TrimSpace(raw)
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		out = append(out, p)
	}
	return out
}

// dispatchTaskNote 组织这次派发回给数字助理的一句补充说明（模式、计划模式、
// 附件、权限档——只讲与默认不同的点）。工具结果里带上它，模型才知道这次派发
// 实际按什么档跑起来的；都取默认值时返回空串，不啰嗦。
func dispatchTaskNote(mode, perm, reqPerm string, planMode bool, attached int, remoteDisplay string) string {
	var parts []string
	if m := chatModeByValue(mode); m != nil {
		parts = append(parts, "模式：「"+m.Label+"」（"+mode+"）")
	}
	if planMode {
		parts = append(parts, "以计划模式启动（先调研出计划、经用户批准后再动手）")
	}
	if attached > 0 {
		parts = append(parts, fmt.Sprintf("已带上 %d 个附件（已进新任务工作区并登记为上传）", attached))
	}
	if strings.TrimSpace(reqPerm) != "" {
		parts = append(parts, "权限档：「"+permModeLabel(perm)+"」（"+perm+"）")
	} else if perm != permWorkspace {
		parts = append(parts, "权限档：「"+permModeLabel(perm)+"」（"+perm+"，随本会话权限，只降不升）")
	}
	if remoteDisplay != "" {
		parts = append(parts, "在 SSH 远程工作区 "+remoteDisplay+" 中工作（文件走 SFTP、命令经 SSH）")
	}
	if len(parts) == 0 {
		return ""
	}
	return "（" + strings.Join(parts, "；") + "）"
}

// dispatchSubTool 构造 dispatch_sub 工具。描述是**纯静态文案**：内置两种模式
// 写死在说明里，自定义专家名单不嵌进来（嵌了会让 tools 数组随设置增删变化，
// 前缀缓存从最开头全断）——名单走常驻的 list_experts 工具按需查询，返回内容
// 作为工具结果落在上下文尾部。
func (a *App) dispatchSubTool() Tool {
	desc := "新建一个子对话（挂在本对话下，右栏可见），把一块自洽的子任务交给它后台执行（它拥有读写文件与执行命令的全部工具）。本工具立即返回，子对话完成后结果自动回报到本对话并触发你继续。子对话只有一级，不能再派发。专家预设是选模式的唯一方式：不传 expert 等同内置的「标准子对话」（自足任务、不继承上下文）；要继承本对话聊天记录就填内置的「继承上下文子对话」；自定义专家还有哪些是易变信息——先调用 list_experts 查名单，expert 必须与名单里的名字逐字一致。permission 参数可指定子对话的权限档，但只能同等或降权（绝不高于本会话权限）。"
	return Tool{
		Spec: toolSpec("dispatch_sub", desc,
			objSchema(map[string]any{
				"task":       strProp("完整子任务描述。未继承上下文的专家（如「标准子对话」）看不到本对话记录，task 必须自带全部上下文（目标、验收标准、涉及目录）；继承上下文的专家（如「继承上下文子对话」）能看到此前的聊天记录，task 只用写「接下来要做什么」"),
				"expert":     strProp("子对话专家名称：不传等同「标准子对话」；内置另有「继承上下文子对话」；自定义专家的名字与用途先用 list_experts 查询，必须与返回的清单逐字一致"),
				"title":      strProp("给子对话起的标题，可省略"),
				"workspace":  strProp("可选：子对话的工作目录（绝对路径），默认沿用当前工作区；与 remote_workspace 互斥"),
				"remote_workspace": strProp("可选：子对话使用的 SSH 远程工作区（本对话的二号工作区），取值照抄环境快照「可派发远程工作区」行里的完整地址（user@host:dir）。与 workspace 互斥；不填＝沿用当前环境（本会话挂了远程则继承同一远程）"),
				"model":      strProp("可选：覆盖专家预设的模型，格式 providerId/modelId（如 deepseek/deepseek-v4-pro）或纯 modelId；不填用预设默认模型或全局默认"),
				"permission": strProp("可选：子对话的权限档，取值 read-only（全电脑只读）/ workspace-write（修改工作区）/ full（完全访问）。只能**同等或降权**——不得高于本会话（数字助理）当前权限，超出即拒绝并报错（不静默钳制）。不填＝修改工作区（本会话权限更窄时取本会话权限）"),
			}, "task")),
		Run: runDispatchSub,
	}
}

// runDispatchSub 派发子对话：被派发的是挂在当前会话下的 task 会话（右栏抽屉
// 可见），回报链就是 parent_id。主对话与工作任务（task/office）都可派；
// 子对话本身不能再派发（只有一级）。专家是唯一的模式选择路径：expert=名称
// 按预设派发（继承行为、默认模型、追加提示词都由预设决定），不传 expert
// 等同内置的「标准子对话」（自足任务、不带上下文）；model 参数可换模型。
// 子会话记下专家名（SessionMeta.Expert），提示词构建时按名解析追加。
func runDispatchSub(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Task           string `json:"task"`
		Expert         string `json:"expert"`
		Title          string `json:"title"`
		Workspace      string `json:"workspace"`
		RemoteWorkspace string `json:"remote_workspace"`
		Model          string `json:"model"`
		Permission     string `json:"permission"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Task) == "" {
		return "", fmt.Errorf("task 不能为空")
	}
	// 二号工作区参数（快速失败：互斥与登记校验都在建会话之前）
	if strings.TrimSpace(p.Workspace) != "" && strings.TrimSpace(p.RemoteWorkspace) != "" {
		return "", fmt.Errorf("workspace 与 remote_workspace 互斥：要本地目录用 workspace，要远程工作区用 remote_workspace")
	}
	dispRemote, err := t.app.resolveDispatchRemote(p.RemoteWorkspace)
	if err != nil {
		return "", err
	}
	// 工具按模式分发，但人工把会话克隆成子对话后模式照旧、工具还在手里，
	// 这里硬拦，保证子对话只有一级
	if t.sess.ParentID != "" {
		return "", fmt.Errorf("子对话不能再派发子对话（只有一级）；请把这块工作直接做掉")
	}
	// 专家解析：名字即参数，逐字匹配；不存在时把可用名单带回去让模型改
	var ex *ExpertCfg
	if name := strings.TrimSpace(p.Expert); name != "" {
		if ex = t.app.enabledExpertByName(name); ex == nil {
			names := []string{}
			for _, e := range t.app.enabledExperts() {
				names = append(names, "「"+strings.TrimSpace(e.Name)+"」")
			}
			return "", fmt.Errorf("子对话专家「%s」不存在（名称要与 list_experts 返回的清单逐字一致），可用：%s",
				name, strings.Join(names, "、"))
		}
	} else {
		// 不传 expert = 内置「标准子对话」（自足任务、不继承上下文）
		sys := systemExperts()
		ex = &sys[0]
	}
	// 权限档：与 dispatch_task 同一条规则——只能派发不高于本会话权限的子对话
	//（可同等、可降权，绝不允许增权；超出即报错）
	perm, err := t.dispatchPermission(p.Permission)
	if err != nil {
		return "", err
	}
	// 模型默认链：显式参数 > 专家预设 > 继承主会话手选 > 空（全局默认）
	model := strings.TrimSpace(p.Model)
	if model == "" {
		model = strings.TrimSpace(ex.Model)
	}
	provider, providerKey, mdl, err := t.dispatchModelChoice(model)
	if err != nil {
		return "", err
	}
	ws := t.workspace()
	if dispRemote != nil {
		ws = "" // 二号工作区：环境由 remote_workspace 决定，不占本地 ws 字段
	} else if strings.TrimSpace(p.Workspace) != "" {
		ws = p.Workspace
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = truncateRunes(p.Task, 20)
	}
	params := createSessionParams{
		Mode: "task", Title: title, Workspace: ws,
		ParentID:   t.sess.ID, // 子对话挂在当前会话下；回报链走 parent_id
		Permission: perm,      // 只能同等或降权，绝不允许增权（dispatchPermission）
		Expert:     strings.TrimSpace(ex.Name),
		Provider:   provider, Model: mdl, ProviderKey: providerKey,
		CopyHistory: ex.InheritContext, // 预设要继承时复制本会话完整历史（task 排在继承块之后）
	}
	switch {
	case dispRemote != nil:
		params.Remote = dispRemote // 显式二号工作区优先于父会话继承
	case t.sess.Remote != nil:
		r := *t.sess.Remote
		params.Remote = &r // 远程工作区继承：子对话默认同一远程环境
	}
	// 建会话收口在 createSession（sessioncreate.go）：能力默认、历史继承（失败
	// 自动回滚，不留半截子对话——旧实现在这里不回滚）与人工建会话同一处
	sub, err := t.app.createSession(params)
	if err != nil {
		return "", err
	}
	// 需求正文（【派发子对话】+ task）由 deliverSub 走统一投递落库——与用户发消息同路
	var hints []string
	if strings.TrimSpace(p.Permission) != "" {
		hints = append(hints, "权限档：「"+permModeLabel(perm)+"」（"+perm+"，只能同等或降权）")
	}
	if dispRemote != nil {
		hints = append(hints, "子对话在 SSH 远程工作区 "+dispRemote.Display()+" 中工作（文件走 SFTP、命令经 SSH）")
	}
	if strings.TrimSpace(ex.Prompt) != "" {
		hints = append(hints, fmt.Sprintf("已按专家预设「%s」派发，追加提示词已生效", sub.Expert))
	} else {
		hints = append(hints, fmt.Sprintf("已按「%s」模式派发", sub.Expert))
	}
	if ex.InheritContext {
		hints = append(hints, "已继承本会话上下文，子对话能看到此前的聊天记录")
	}
	note := ""
	if len(hints) > 0 {
		note = "（" + strings.Join(hints, "；") + "）"
	}
	return t.deliverSub(sub, "【派发子对话】\n\n"+p.Task, note), nil
}
func runDispatchTask(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Task        string   `json:"task"`
		Title       string   `json:"title"`
		Workspace   string   `json:"workspace"`
		RemoteWorkspace string `json:"remote_workspace"`
		Model       string   `json:"model"`
		Mode        string   `json:"mode"`
		PlanMode    bool     `json:"plan_mode"`
		Attachments []string `json:"attachments"`
		Permission  string   `json:"permission"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Task) == "" {
		return "", fmt.Errorf("task 不能为空")
	}
	// 子对话不能再派发子对话：编排工具只发给总控，但人工把总控克隆成子对话后，
	// 那个子会话手里也有这套工具，这里硬拦，保证只有一级子对话。
	if t.sess.ParentID != "" {
		return "", fmt.Errorf("子对话不能再派发子对话（本会话已是子对话，只有一级）；请把需求直接做掉，或让用户在主对话里派发")
	}
	// 二号工作区参数（快速失败：互斥与登记校验都在建会话之前）
	if strings.TrimSpace(p.Workspace) != "" && strings.TrimSpace(p.RemoteWorkspace) != "" {
		return "", fmt.Errorf("workspace 与 remote_workspace 互斥：要本地目录用 workspace，要远程工作区用 remote_workspace")
	}
	dispRemote, err := t.app.resolveDispatchRemote(p.RemoteWorkspace)
	if err != nil {
		return "", err
	}
	// 可选参数先全部解析校验（快速失败）：模式 / 权限 / 模型 / 附件都可能被拒，
	// 拒绝必须发生在建出半个会话之前
	mode, err := resolveDispatchMode(p.Mode)
	if err != nil {
		return "", err
	}
	perm, err := t.dispatchPermission(p.Permission)
	if err != nil {
		return "", err
	}
	provider, providerKey, mdl, err := t.dispatchModelChoice(p.Model)
	if err != nil {
		return "", err
	}
	attPaths := normalizeDispatchAttachments(p.Attachments)
	if len(attPaths) > 0 && (t.sess.Remote != nil || dispRemote != nil) {
		return "", fmt.Errorf("附件不能带给远程工作区的新任务（本会话挂了远程或 remote_workspace 指定了远程）：本机文件路径在远程无从复制。请去掉附件，或让远程会话自己取文件")
	}
	for _, src := range attPaths {
		if _, _, _, err := resolveUploadSource(src); err != nil {
			return "", fmt.Errorf("附件 %s 不可用：%w", src, err)
		}
	}
	ws := t.workspace()
	if dispRemote != nil {
		ws = "" // 二号工作区：环境由 remote_workspace 决定，不占本地 ws 字段
	} else if strings.TrimSpace(p.Workspace) != "" {
		ws = p.Workspace
	}
	title := strings.TrimSpace(p.Title)
	if title == "" {
		title = truncateRunes(p.Task, 20)
	}
	params := createSessionParams{
		Mode:         mode, // 与人工新建会话同一份模式清单（resolveDispatchMode）
		Title:        title, Workspace: ws,
		Permission:   perm,       // 只能同等或降权，绝不允许增权（dispatchPermission）
		PlanMode:     p.PlanMode, // 与人工建会话勾选「计划模式」一致
		DispatchedBy: t.sess.ID,  // 派发的是顶层工作任务（左侧列表可见）；回报链走 dispatched_by，不走 parent_id
		Provider:     provider, Model: mdl, ProviderKey: providerKey,
	}
	switch {
	case dispRemote != nil:
		params.Remote = dispRemote // 显式二号工作区优先于父会话继承
	case t.sess.Remote != nil:
		r := *t.sess.Remote
		params.Remote = &r // 远程工作区继承：新任务默认同一远程环境
	}
	// 建会话收口在 createSession（sessioncreate.go）：模式/权限/能力默认与落库
	// 和人工建会话同一处
	sub, err := t.app.createSession(params)
	if err != nil {
		return "", err
	}
	// 附件：与桌面端 /api/upload 的 JSON {path} 走同一套登记（registerUploadPath）
	// ——复制进新任务工作区 + 落 uploads 表。注记拼进任务消息开头的「用户附件：」
	// 块（与用户消息同形态），随任务描述一起到达——新任务开轮就知道有哪些现成
	// 文件、在哪、能当交付目标。
	attached := 0
	attNotes := make([]string, 0, len(attPaths))
	for _, src := range attPaths {
		_, note, err := t.app.registerUploadPath(sub, src)
		if err != nil {
			return "", fmt.Errorf("附件 %s 登记失败：%w", src, err)
		}
		attNotes = append(attNotes, note)
		attached++
	}
	// 工作任务永远不带上下文：像用户发消息一样，只收到一段自足的任务描述，
	// 与派发方的聊天记录完全隔离（要上下文的场景用 dispatch_sub 的继承模式）
	// 需求正文（【数字助理派发】+ task）由 deliverSub 走统一投递落库；带附件时
	// 正文最前是「用户附件：」块（注记逐字原样，模型收到的与用户消息同形态）
	remoteDisp := ""
	if dispRemote != nil {
		remoteDisp = dispRemote.Display()
	}
	taskMsg := "【数字助理派发】\n" + p.Task
	if len(attNotes) > 0 {
		taskMsg = ATTACH_HEAD + "\n" + strings.Join(attNotes, "\n") + "\n\n" + taskMsg
	}
	return t.deliverSub(sub, taskMsg, dispatchTaskNote(mode, perm, p.Permission, p.PlanMode, attached, remoteDisp)), nil
}
func sendToSessionTool() Tool {
	return Tool{
		Spec: toolSpec("send_to_session", "把话发给一个已有会话（新需求与该会话明显相关时优先用它，上下文连续效率最高）：对方**正在执行**时作为**插话**注入它当前这一轮——它下一次调用模型就能看到，当场生效（纠偏、答疑、补要求都直接发）；对方**空闲**时作为新需求直接开跑。本工具立即返回，它这一轮完成后你会自动收到【派发回报】；想等它干完当前活再派下一个任务的，就等回报来了再发，不要提前占位。可选 permission 指定这条需求用哪个权限档跑，只能同等或降权（绝不高于本会话权限，也不能给已有会话提权）。",
			objSchema(map[string]any{
				"session_id": strProp("目标会话 id，来自 list_sessions"),
				"task":       strProp("完整、自足的一段需求"),
				"permission": strProp("可选：这条新需求使用的权限档，取值 read-only（全电脑只读）/ workspace-write（修改工作区）/ full（完全访问）。只能**同等或降权**——不得高于本会话（数字助理）当前权限，也不得高于该会话自己的当前权限（既有会话的档位不能被这条需求抬高），超出即拒绝并报错（不静默钳制）。不填＝沿用该会话当前的权限档，不做任何改动"),
			}, "session_id", "task")),
		Run: runSendToSession,
	}
}

// runSendToSession 把话发给已有会话，默认语义是**插话**：对方执行中就注入它
// 当前这一轮（等价 call_api 调 interject mode=steer），空闲则直接作为新需求开跑
// ——中途发话一律立即生效，不排队占位。权限档与 dispatch_task / dispatch_sub
// 同一套规则（只能同等或降权）。此处比派发工具多挡一道——已有会话**自己的**权限
// 也不能被这条需求抬高（那个会话可能本就比本会话更窄，比如只读调研会话）。不传
// permission 时原样沿用该会话的档位，不改动任何配置。
func runSendToSession(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		SessionID  string `json:"session_id"`
		Task       string `json:"task"`
		Permission string `json:"permission"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	sid := strings.TrimSpace(p.SessionID)
	if sid == "" || strings.TrimSpace(p.Task) == "" {
		return "", fmt.Errorf("session_id 与 task 不能为空")
	}
	if sid == t.sess.ID {
		return "", fmt.Errorf("不能把任务发回主对话自己")
	}
	m, err := t.app.Store.Get(sid)
	if err != nil {
		return "", fmt.Errorf("会话 %s 不存在，先用 list_sessions 看看 id", sid)
	}
	if m.Mode == "assistant" {
		return "", fmt.Errorf("目标会话是另一个总控会话，不能作为派发目标")
	}
	note := ""
	if strings.TrimSpace(p.Permission) != "" {
		perm, perr := t.dispatchPermission(p.Permission)
		if perr != nil {
			return "", perr
		}
		cur := permModeOf(m)
		if sandboxPolicyRank(perm) > sandboxPolicyRank(cur) {
			return "", fmt.Errorf("不能给已有会话提权：会话 %s 当前权限是「%s」（%s），permission=%s 比它宽。这条需求只能用「%s」或更窄的档位，或先请用户在权限面板把这个会话调宽",
				m.ID, permModeLabel(cur), cur, perm, permModeLabel(cur))
		}
		if perm != cur {
			// 权限档落装与能力联动收口在 applyPermissionChange（与人工 PATCH 权限
			// 面板同一套）：降到完全访问以下时取消捆绑能力、补回默认——人工改档
			// 是什么效果，AI 传 permission 就是什么效果。
			applyPermissionChange(m, perm, false)
			if err := t.app.Store.SaveMeta(m); err != nil {
				return "", fmt.Errorf("保存会话 %s 的权限档失败：%w", m.ID, err)
			}
		}
		note = "（权限档：「" + permModeLabel(perm) + "」（" + perm + "，只能同等或降权））"
	}
	// 需求正文（【数字助理转达】+ task）由 deliverSub 走统一投递落库：投递一律
	// 插话——目标执行中就注入它当前这一轮（下一次调用模型前生效），空闲则直接
	// 落库开跑（两种情况都立即生效，不排队占位）
	// 经理首次发话即建立「经理关联」标记（mgrnotify.go）：这个会话之后有审批/
	// 提问卡挂久了没人处理，会自动通知经理代为处理。幂等，重复发话写同值。
	t.app.markManagerLinked(m)
	return t.deliverSub(m, "【数字助理转达】\n"+p.Task, note), nil
}

func stopSessionTool() Tool {
	return Tool{
		Spec: toolSpec("stop_session", "停掉一个正在执行的任务会话（等效人工点该会话上的「停止」按钮，走服务端同一套停止逻辑）。用户说「停掉 / 别查了 / 取消那个任务 / 让他停下」时用它，参数只要会话 id。停止是**分级**的：先协作式取消（模型流、工具执行与等待都会收到取消信号，当前这一步跑完即停）；若 3 秒后仍未收口（执行卡死不响应取消），停止看门狗会自动强制终结——断开模型流、终止子进程与后台作业、置为停止态，保证停止最终必然生效。调用刚返回时该会话可能还显示「正在运行」，稍候即收尾。**只在用户明确要求停止时调用**：不要自己去掐任务。停止后的【派发回报】首句会写「已由用户手动停止」（触发者标「数字助理（受用户指示）」）——那是人为叫停、不是故障，**不要再重派、不要续跑**，回用户一句已停止即可。",
			objSchema(map[string]any{
				"session_id": strProp("要停止的会话 id，来自 list_sessions（列表里带「正在运行」标记的才是在跑的）"),
			}, "session_id")),
		Run: runStopSession,
	}
}

// runStopSession 停掉一个正在执行的任务会话：调用服务端**既有**的停止逻辑
//（App.stopRunBy，与人工点会话上「停止」按钮走同一个函数：协作式取消 ctx、
// 终止该会话全部后台作业、把停止原因落库），不另造一套。差别只在触发者——
// 人工点按钮记 user，本工具记 assistant（数字助理受用户指示代停），因此停止
// 记录、重启恢复与【派发回报】的首句都分得清是谁停的（两者都算人为叫停：
// Kind 都是 stopByUser，不会被当成故障而重新派发）。
func runStopSession(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	sid := strings.TrimSpace(p.SessionID)
	if sid == "" {
		return "", fmt.Errorf("session_id 不能为空")
	}
	// 停自己不成：本会话就是数字助理（你正在说话的这个），停它等于掐掉自己
	// 这一轮，连回报都无从说起
	if sid == t.sess.ID {
		return "", fmt.Errorf("不能停本会话（数字助理主对话，就是你正在说话的这个）：那等于掐掉自己这一轮。请用 list_sessions 查用户指名的那个任务会话的 id")
	}
	m, err := t.app.Store.Get(sid)
	if err != nil {
		return "", fmt.Errorf("会话 %s 不存在（先用 list_sessions 核对 id；列表里带「正在运行」标记的才是在跑的）", sid)
	}
	if m.Mode == "assistant" {
		return "", fmt.Errorf("会话 %s 是另一个总控（数字助理）会话，不是可停的任务会话", sid)
	}
	// 先看内存注册表（与「停止」按钮同一口径）再叫停：停止请求发出后是协作式收尾，
	// 但执行体的登记就在注册表里，所以能立刻分辨「在跑」与「本来就空闲」
	running := t.app.IsRunning(sid)
	if !t.app.stopRunBy(sid, stopActorAssistant, stopTriggerNote(stopActorAssistant)) {
		return fmt.Sprintf("会话 %s「%s」当前不在运行（没有在执行的动作，也没有在跑的后台作业），无需停止。", sid, m.Title), nil
	}
	if !running {
		return fmt.Sprintf("会话 %s「%s」没有在执行的动作，但它有后台作业在跑，已一并终止（作业不随会话空闲而自己停）。", sid, m.Title), nil
	}
	return fmt.Sprintf("已发出停止请求：会话 %s「%s」会在当前这一步跑完后停下——先走协作式停止（模型流、工具执行与等待都会收到取消信号）；若 %d 秒内没有收尾（执行卡死不响应取消），停止看门狗会自动强制终结（断模型流、终止子进程与后台作业），保证停止最终必然生效。它的后台作业已一并终止；停止原因按「%s」落库，所以给本对话的【派发回报】首句会写明这是人为叫停（不是故障）。**不要重派、不要续跑**：收到回报或直接回用户一句已停止即可。",
		sid, m.Title, int(t.app.stopWatchdog/time.Second), stopTriggerNote(stopActorAssistant)), nil
}

// sessionInterrupted 判断一份（读到末尾的）会话历史是不是以中断收尾：末尾几行里
// 有中断注记（【执行中断】/【系统中断】，kind=report origin=interrupt）就算，除非
// 之后又有真人消息（那说明用户已经接续、中断翻篇了）。回报有中断标记用不着它，
// 但 read_session_summary（读别人历史）没有标记，靠它区分「正常完成」与「中断」，
// 从而决定过程内容给「要点」还是给「断点附近的原文」。
func sessionInterrupted(rows []*StoredMessage) bool {
	for i := len(rows) - 1; i >= 0 && i >= len(rows)-8; i-- {
		r := rows[i]
		if r == nil {
			continue
		}
		if r.Kind == kindReport && r.Origin == "interrupt" {
			return true
		}
		if r.Role == "user" && r.Kind == kindChat {
			return false
		}
	}
	return false
}

// ---------- 压缩摘要（设计对齐 DSH 版 summary.ts，只留本栈有的字段） ----------
// 回报摘要的字段与渲染规则见下面 reportDigest/buildReportDigest。

// changedFile 是一条「改动文件」记录：Path/Op 来自工具调用参数，其余字段是
// **核对结果**——回报里的路径必须与实际落盘对得上（曾出现回报路径不存在：
// 报告实际在 .jxai\tmp\，记录里却写成 .jxai\tmp\patch\）。
type changedFile struct {
	Path     string // 工具记录里的路径
	Op       string // 新建或覆盖 / 修改
	Display  string // 回报里展示的路径（核对后的口径）
	Rel      bool   // Display 是相对工作区的路径（回报据此标注所属工作区）
	WS       string // 所属工作区（相对路径展示用）
	Exists   bool   // 磁盘上确实存在
	Resolved string // 实际落盘路径（存在时）
	Note     string // 核对结论：已核对落盘 / 记录与实际不一致 / 未找到 / 远程未核对
}

var writeTools = map[string]bool{"write": true, "edit": true} // 改动文件的来源工具

var resultFailedPrefixes = []string{"工具执行失败", "未知工具", "（用户中断"}

// countProgress 折出会话进度：轮 = 用户侧消息数，步 = 带工具调用的助手消息数。
// 回报与中断进度共用这一份口径（见 runowner.go 的 sessionProgress），改这里两处同时变。
func countProgress(rows []*StoredMessage) (rounds, steps int) {
	for _, r := range rows {
		if r == nil {
			continue
		}
		switch r.Role {
		case "user":
			rounds++
		case "assistant":
			if len(r.ToolCalls) > 0 {
				steps++
			}
		}
	}
	return rounds, steps
}

// summarizeSession 是同一口径的 []Message 版本（会话进度展示用）。
func summarizeSession(msgs []Message) (rounds, steps int) {
	for _, m := range msgs {
		switch m.Role {
		case "user":
			rounds++
		case "assistant":
			if len(m.ToolCalls) > 0 {
				steps++
			}
		}
	}
	return rounds, steps
}

// reportDigest 是【派发回报】里那份压缩摘要的数据：只留对回报有用、且人读得懂
// 的东西——轮数与步数、完整最终答复、结尾消息、折叠后的过程、核对过的改动文件。
// **不收集工具调用统计**：run_command×12 这类数字对汇报没用，只挤占上下文。
type reportDigest struct {
	Rounds, Steps int
	Final         string // 最终答复正文（宁长勿截，reportFinalCap 只挡病态超长）
	FinalSeq      int64  // 最终答复所在的消息序号（截断时给「完整版为会话第 N 条」用）
	FinalTotal    int    // 最终答复总字数
	FinalClipped  bool   // 是否被 reportFinalCap 截断
	HasFinal      bool
	Process       []string      // 过程内容：正常完成=压成要点；中断=断点附近原文
	Tail          []string      // 最终答复之后的收尾消息（一行式）
	Interrupted   bool          // 本轮是否中断收尾（决定过程内容怎么处理）
	Files         []changedFile // 改动文件（已与实际落盘核对）
}

// 回报的可读性参数：最终答复宁长勿截（reportFinalCap 只是防病态超长的兜底，
// 真被截时明示「已截断，完整版为会话第 N 条」）。
const (
	reportFinalCap    = 20000 // 最终答复全文上限（字）
	reportTailMax     = 5     // 附带的结尾消息条数
	reportTailClip    = 400   // 结尾消息单条上限（字）
	reportProcessKeep = 2     // 过程内容保留条数（最后 1-2 条）
	reportProcessClip = 220   // 正常完成时的过程要点上限（字）
	reportBreakClip   = 600   // 中断时断点附近原文的上限（字）
)

// displayableRow 判断一条消息能不能进回报摘要：对话内容与系统通知（回报/
// 中断/上传注记）可以；上下文材料（环境快照/拼装快照/请求快照/审计行/摘要行）
// 不进——它们是组装材料，不是「这个会话做了什么」。
func displayableRow(m *StoredMessage) bool {
	switch m.Kind {
	case kindEnv, kindPrompt, kindRequest, kindAudit, kindInherit, kindSummary:
		return false
	}
	return true
}

// buildReportDigest 把会话消息流折成回报摘要数据。interrupted 由调用方给出
//（它知道本轮是正常收尾还是中断收尾）：中断时过程内容保留断点附近的原文，
// 正常完成时只留一两句要点。
func buildReportDigest(a *App, m *SessionMeta, rows []*StoredMessage, interrupted bool) reportDigest {
	dg := reportDigest{Interrupted: interrupted}
	dg.Rounds, dg.Steps = countProgress(rows)
	calls := map[string]string{} // callID → 工具名
	paths := map[string]string{} // callID → 参数里的 path
	type textAt struct {
		idx   int    // 在 rows 里的下标
		seq   int64  // 消息序号
		text  string // 正文
		plain bool   // 只有正文、没有工具调用（收尾答复的典型形态）
	}
	texts := []textAt{}
	fileMap := map[string]string{}
	fileOrder := []string{}
	for i, r := range rows {
		if r == nil {
			continue
		}
		switch r.Role {
		case "assistant":
			for _, tc := range r.ToolCalls {
				calls[tc.ID] = tc.Function.Name
				var arg struct {
					Path string `json:"path"`
				}
				if json.Unmarshal([]byte(tc.Function.Arguments), &arg) == nil && arg.Path != "" {
					paths[tc.ID] = arg.Path
				}
			}
			if s := strings.TrimSpace(r.Text()); s != "" {
				texts = append(texts, textAt{idx: i, seq: r.Seq, text: s, plain: len(r.ToolCalls) == 0})
			}
		case "tool":
			name, p := calls[r.ToolCallID], paths[r.ToolCallID]
			if name == "" || !writeTools[name] || p == "" || failedToolResult(r.Text()) {
				continue
			}
			op := "新建或覆盖"
			if name == "edit" {
				op = "修改"
			}
			if _, ok := fileMap[p]; !ok {
				fileOrder = append(fileOrder, p)
			}
			fileMap[p] = op
		}
	}
	// 最终答复：优先取「只有正文、没有工具调用」的最后一段（那才是收尾答复）；
	// 一段都没有（执行中途被打断）就退回最后一段助手正文——比原先「一律取最后
	// 一段正文」更贴近「最终答复」四个字。
	finalAt := -1
	for i := len(texts) - 1; i >= 0; i-- {
		if texts[i].plain {
			finalAt = i
			break
		}
	}
	if finalAt < 0 && len(texts) > 0 {
		finalAt = len(texts) - 1
	}
	if finalAt >= 0 {
		dg.HasFinal = true
		dg.FinalSeq = texts[finalAt].seq
		body := texts[finalAt].text
		dg.FinalTotal = len([]rune(body))
		if r := []rune(body); len(r) > reportFinalCap {
			dg.Final = string(r[:reportFinalCap])
			dg.FinalClipped = true
		} else {
			dg.Final = body
		}
		// 过程内容＝最终答复之前的助手正文：正常完成压成要点（一两句），
		// 中断则保留断点附近的原文（格式留着，一眼看出中断前在干嘛）
		prev := texts[:finalAt]
		if len(prev) > reportProcessKeep {
			prev = prev[len(prev)-reportProcessKeep:]
		}
		for _, t := range prev {
			if dg.Interrupted {
				dg.Process = append(dg.Process, clipRunes(t.text, reportBreakClip))
			} else {
				dg.Process = append(dg.Process, clipText(t.text, reportProcessClip))
			}
		}
		// 结尾消息＝最终答复之后的收尾内容（交付结果/中断/上传等系统注记都在这里）
		for _, r := range rows[texts[finalAt].idx+1:] {
			if r == nil || !displayableRow(r) {
				continue
			}
			dg.Tail = append(dg.Tail, historyLine(r, reportTailClip))
		}
		if len(dg.Tail) > reportTailMax {
			dg.Tail = dg.Tail[len(dg.Tail)-reportTailMax:]
		}
	}
	for _, p := range fileOrder {
		dg.Files = append(dg.Files, resolveChangedFile(a, m, p, fileMap[p]))
	}
	return dg
}

// failedToolResult 判断一条工具结果是不是失败结尾（失败的不算「成功写入」）。
func failedToolResult(s string) bool {
	s = strings.TrimSpace(s)
	for _, pre := range resultFailedPrefixes {
		if strings.HasPrefix(s, pre) {
			return true
		}
	}
	return false
}

// ---------- 回报渲染 ----------

// roleLabel 给一条消息一个人类可读的身份标签（结尾消息/过程内容展示用）。
func roleLabel(m *StoredMessage) string {
	base := map[string]string{"user": "用户", "assistant": "助手", "tool": "工具"}[m.Role]
	if base == "" {
		base = m.Role
	}
	switch m.Kind {
	case kindReport:
		switch m.Origin {
		case "interrupt":
			base = "系统通知（中断）"
		case "upload":
			base = "系统通知（上传）"
		default:
			base = "系统通知"
		}
	case kindAiUser:
		base = "AI 用户消息"
	}
	return base
}

// historyLine 把一条消息渲染成回报里的「结尾消息」一行（压成单行、限长）。
func historyLine(m *StoredMessage, clip int) string {
	body := strings.TrimSpace(m.Text())
	if body == "" {
		if len(m.ToolCalls) > 0 {
			names := []string{}
			for _, tc := range m.ToolCalls {
				names = append(names, tc.Function.Name)
			}
			body = "（工具调用 " + strings.Join(names, "、") + "）"
		} else {
			body = "（无文本内容）"
		}
	}
	return fmt.Sprintf("[%d] %s：%s", m.Seq, roleLabel(m), clipText(body, clip))
}

// renderChangedFiles 渲染「改动文件」清单：路径按实际落盘核对后的口径展示，
// 相对路径标注所属工作区（数字助理据此拼绝对路径做交付登记），核对结论
//（一致/不一致/找不到/远程未核对）一并写明。
func renderChangedFiles(files []changedFile) []string {
	if len(files) == 0 {
		return []string{"· 改动文件：无（未记录到成功的写文件动作）"}
	}
	out := []string{"· 改动文件："}
	for _, f := range files {
		meta := []string{f.Op}
		if f.Rel && f.WS != "" {
			meta = append(meta, "工作区 "+f.WS)
		}
		if f.Note != "" {
			meta = append(meta, f.Note)
		}
		out = append(out, "  - "+f.Display+"（"+strings.Join(meta, "；")+"）")
	}
	return out
}

// resolveChangedFile 核对一条改动记录与实际落盘：write/edit 的记录来自工具
// 参数，未必等于文件真正落地的位置（曾出现回报路径不存在：报告实际在
// .jxai\tmp\，记录里却写成 .jxai\tmp\patch\）。这里先按记录路径找一遍，找不到
// 再按文件名在工作区里找一遍，唯一命中即认定「实际落盘」并注明差异；确实
// 找不到的如实标出，不硬说文件在。
func resolveChangedFile(a *App, m *SessionMeta, path, op string) changedFile {
	ws := ""
	if m != nil {
		ws = m.Workspace
	}
	if ws == "" && a != nil && a.Cfg != nil {
		ws = a.Cfg.Workspace
	}
	cf := changedFile{Path: path, Op: op, Display: path, WS: ws}
	if m != nil && m.Remote != nil {
		cf.Note = "远程工作区（" + m.Remote.Dir + "），未在本机核对"
		return cf
	}
	if filepath.IsAbs(path) {
		if pathExists(path) {
			cf.Exists, cf.Resolved, cf.Note = true, path, "已核对落盘"
			return cf
		}
	} else {
		cf.Rel = true
		if ws != "" {
			if joined := filepath.Join(ws, path); pathExists(joined) {
				cf.Exists, cf.Resolved, cf.Note = true, joined, "已核对落盘"
				return cf
			}
		}
	}
	if ws != "" {
		if hit := findFileInWorkspace(ws, filepath.Base(path)); hit != "" {
			cf.Exists, cf.Resolved = true, hit
			if rel, err := filepath.Rel(ws, hit); err == nil && !strings.HasPrefix(rel, "..") {
				cf.Display, cf.Rel = filepath.ToSlash(rel), true
			} else {
				cf.Display, cf.Rel = hit, false
			}
			cf.Note = "已核对：工具记录为 " + path + "，实际落盘 " + cf.Display
			return cf
		}
	}
	cf.Note = "未在磁盘上找到（可能已被移动、删除，或由外部程序产生）"
	return cf
}

// findFileInWorkspace 在工作区里按文件名找文件（核对路径用）：跳过 .git /
// 数据目录 / 快照正文等噪音，唯一命中才返回（多个同名文件时不敢乱认）。
func findFileInWorkspace(ws, base string) string {
	if base == "" || base == "." || base == "/" || base == "\\" {
		return ""
	}
	skipDirs := map[string]bool{".git": true, "node_modules": true, "data": true, "__pycache__": true}
	matches := []string{}
	budget := 20000 // 遍历上限：工作区再大也不为一次回报跑遍整棵树
	_ = filepath.WalkDir(ws, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if budget <= 0 || len(matches) > 1 {
			return fs.SkipAll
		}
		budget--
		if d.IsDir() {
			name := d.Name()
			if p != ws && (skipDirs[name] || (name == "snapshots" && filepath.Base(filepath.Dir(p)) == ".jxai")) {
				return fs.SkipDir
			}
			return nil
		}
		if d.Name() == base {
			matches = append(matches, p)
		}
		return nil
	})
	if len(matches) == 1 {
		return matches[0]
	}
	return ""
}

// pathExists 判断路径存在且是文件（目录不算「文件落盘了」）。
func pathExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// clipText 压缩空白并按上限截断。
func clipText(s string, limit int) string {
	space := regexp.MustCompile(`\s+`)
	flat := strings.TrimSpace(space.ReplaceAllString(s, " "))
	r := []rune(flat)
	if len(r) <= limit {
		return flat
	}
	return string(r[:limit-1]) + "…"
}

// renderSummary 渲染【派发回报】里的压缩摘要：轮数/步数、最终答复全文、最终
// 答复之后的结尾消息、过程内容（正常完成=要点，中断=断点附近原文）、核对过的
// 改动文件清单。中断原因由调用方写在摘要之前（runtime.go 的 head），这里不重复。
func renderSummary(a *App, m *SessionMeta, rows []*StoredMessage, interrupted bool) string {
	dg := buildReportDigest(a, m, rows, interrupted)
	ws := m.Workspace
	if ws == "" && a != nil && a.Cfg != nil {
		ws = a.Cfg.Workspace
	}
	lines := []string{
		fmt.Sprintf("【会话 %s】%s —— 共 %d 轮 / %d 步", m.ID, m.Title, dg.Rounds, dg.Steps),
		"· 工作区：" + ws,
	}
	switch {
	case !dg.HasFinal:
		lines = append(lines, "· 最终答复：（本轮没有助手正文输出）")
	case dg.FinalClipped:
		lines = append(lines, fmt.Sprintf("· 最终答复（已截断：会话第 %d 条共 %d 字，此处显示前 %d 字；完整版为会话第 %d 条，用 read_session_messages(session_id=%s, seq=%d) 读全文）：\n%s",
			dg.FinalSeq, dg.FinalTotal, reportFinalCap, dg.FinalSeq, m.ID, dg.FinalSeq, dg.Final))
	default:
		lines = append(lines, fmt.Sprintf("· 最终答复全文（会话第 %d 条，共 %d 字）：\n%s", dg.FinalSeq, dg.FinalTotal, dg.Final))
	}
	if len(dg.Tail) > 0 {
		lines = append(lines, "· 结尾消息（最终答复之后的收尾内容）：\n  - "+strings.Join(dg.Tail, "\n  - "))
	}
	if len(dg.Process) > 0 {
		label := "· 过程要点（正常完成，只留要点）："
		if dg.Interrupted {
			label = "· 断点附近的过程内容（中断，保留原文）："
		}
		lines = append(lines, label+"\n  - "+strings.Join(dg.Process, "\n  - "))
	}
	lines = append(lines, renderChangedFiles(dg.Files)...)
	lines = append(lines, "（以上是压缩摘要：不含工具调用统计；需要工具参数细节可让用户点进该会话，或用 read_session_messages 翻原文。）")
	return strings.Join(lines, "\n")
}
