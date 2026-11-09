package main

// 权限与授权引擎：移植 DSH（DeepSeek 官方 Agent）的沙箱与权限模型，
// 与官方同构的三层结构：
//
//  1. 三档沙箱策略（会话粒度，权限芯片可选）：read-only（沙箱内执行、
//     无任何写授权）/ workspace-write（沙箱内执行、授权工作区+私有临时
//     目录）/ full（不受限，且预设捆绑点亮「允许沙箱外执行/操作浏览器/
//     操作电脑」能力，勾选置灰不可单独关闭）。强制力来自操作系统：
//     Windows 受限令牌+ACL（sandbox_windows.go）、Linux Landlock（经
//     sandboxhelper 自限制）、SSH 远程经上传到远端的同一 helper。
//     文件工具以路径边界为沙箱。
//  2. 拒绝驱动的升级（对齐官方 bash 工具）：不做威胁命令黑名单、不做
//     执行前预检——命令先按会话策略进沙箱真实执行；写操作被操作系统
//     拒绝时结果带 [sandbox: write denied under <mode> mode] 标记，模型
//     同轮可带 sandbox_permissions（够用的最宽档）+ justification 重试
//     一次，该重试弹审批卡由用户决定，批准后以更宽策略执行这一次。
//     「始终允许」= 常设授权：命令升级点亮对应能力开关；工作区外写入
//     把会话切到完全访问（捆绑能力一并点亮，不再审批）。
//     能力门控（browser_* / computer_* / device_*）有同款升级参数：被拦
//     后同轮带 request_capability=true + justification 重试即弹审批卡，
//     「允许」放行本次、「始终允许」点亮对应能力开关。
//     被拒绝的升级对该命令终局。
//  3. 一次性授权凭证（Grant）：每次写文件/执行命令签发短时效凭证，
//     执行器核销后才动手；过期或重复核销即拒绝（fail closed）。
//  4. 受保护路径：助手自身的配置与数据目录（内含 API Key 与全部会话
//     记录）任何模式下工具都禁止访问——确定性的路径边界检查。
//
// 远程工作区（SSH）会话走同一套引擎：工作区边界换为远程目录，命令经
// 远端 helper 在远端沙箱内执行（官方 sandbox-ssh 同思路）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	permReadOnly    = "read-only"       // 全电脑只读（沙箱内执行但无任何写授权）
	permWorkspace   = "workspace-write" // 修改工作区（默认）
	permFull        = "full"            // 完全访问（不受限）
	capUnsandboxed  = "unsandboxed"     // 能力：允许沙箱外（不受限）执行命令
	capSandboxWrite = "sandbox-write"   // 能力：允许命令在沙箱内写入（全电脑只读档位下）
	capBrowser      = "browser"         // 能力：允许操作浏览器（browser_* 工具）
	capComputer     = "computer"        // 能力：允许操作电脑（门控内置 MCP 连接器 cua / windows-mcp）
	capDevice       = "device"          // 能力：允许操作手机（device_* / adb_* 工具，仅安卓）
	grantTTL        = 5 * time.Minute   // 授权凭证有效期
)

// approvalTTL 审批等待上限（超时按拒绝处理）：与提问卡的默认时限一致（5 分钟），
// 而久等提醒默认 4 分钟——先提醒、后超时，时序顺。做成包级变量只为测试能把
// 它换短来验证「提醒 → 超时按拒」的时序（perm_alert_test.go），产品路径永远是这个值。
var approvalTTL = 5 * time.Minute

// fullAccessCaps 是「完全访问」预设捆绑的能力：会话切到完全访问时，
// 这些项在权限面板强制勾选并置灰（不可单独关闭），权限引擎同样视为开启；
// 切离完全访问时随之自动取消（stripFullAccessCaps）。capDevice（操作手机）
// 仅安卓有意义，桌面端权限面板按平台隐藏该芯片。
var fullAccessCaps = []string{capUnsandboxed, capBrowser, capComputer, capDevice}

// escalationCapability 返回某类升级请求对应的能力开关 id：
// 「始终允许」点亮它，之后该类动作不再逐次审批（常设授权）。
func escalationCapability(req string) string {
	switch req {
	case permFull:
		return capUnsandboxed
	case permWorkspace:
		return capSandboxWrite
	}
	return ""
}

// hasCapability 判断会话是否开启了某个能力开关（权限面板右侧多选）。
// 完全访问模式下捆绑能力（沙箱外执行/操作浏览器/操作电脑）一律视为
// 开启——与前端联动一致：切到完全访问，这三项强制点亮且不可关闭。
func hasCapability(s *SessionMeta, cap string) bool {
	if permModeOf(s) == permFull {
		for _, c := range fullAccessCaps {
			if c == cap {
				return true
			}
		}
	}
	for _, c := range s.Tools {
		if c == cap {
			return true
		}
	}
	return false
}

// addCapability 给会话开启能力开关并持久化（「始终允许」用）。
func (a *App) addCapability(s *SessionMeta, cap string) {
	if hasCapability(s, cap) {
		return
	}
	s.Tools = append(s.Tools, cap)
	_ = a.Store.SaveMeta(s)
	a.broadcast(Event{Type: "session_updated", SessionID: s.ID, Permission: permModeOf(s), Tools: s.Tools})
}

// requireCapability 是能力门控（browser_* / computer_* / device_* 工具族）的
// 统一入口：会话未开启对应能力时 fail closed，不降级、不猜测用户意图。
// 拒绝报错与工具描述都要两头读得懂：对模型明说「别绕路」——能力关着时命令
// 沙箱里也做不到（受限令牌），改用 run_command 只会白费一轮；以及「正确出路」
// ——同轮带 request_capability=true + justification 重试（弹审批卡向用户申请
// 开启本能力），或在回复里请用户点权限芯片勾选。用户在工具卡片里同样会看到
// 这句报错，措辞要能直接照做。
func requireCapability(t *toolContext, capID, title string, args json.RawMessage) error {
	if hasCapability(t.sess, capID) {
		return nil
	}
	// 升级重试：模型被拦后带 request_capability + justification 向用户申请
	// 开启（sandbox_permissions 的能力版，语义同款）：「允许」= 本次放行
	// （不点亮开关，下次仍需申请）；「始终允许」= addCapability 点亮权限
	// 芯片对应开关（常设授权），之后该能力不再逐次审批。
	var p struct {
		RequestCapability bool   `json:"request_capability"`
		Justification     string `json:"justification"`
	}
	_ = json.Unmarshal(args, &p) // 解析失败按未携带处理（schema 校验兜底）
	if p.RequestCapability {
		just := strings.TrimSpace(p.Justification)
		if just == "" {
			return fmt.Errorf("request_capability 必须搭配一句 justification（给用户看的理由），本次缺少")
		}
		summary := fmt.Sprintf("开启「%s」能力以使用 %s｜理由：%s", title, t.toolName, truncateRunes(just, 120))
		if t.emit != nil {
			t.emit(Event{Type: "tool_wait", Name: t.toolName, Output: "等待用户审批（申请开启「" + title + "」能力）…"})
		}
		approved, always, timedOut := t.app.requestApproval(t.ctx, t.sess, t.toolName, summary,
			fmt.Sprintf("当前会话未开启「%s」能力，%s 已被能力门控拦下。「允许」=本次放行这一次调用；「始终允许」=在权限芯片点亮「允许%s」（常设授权，之后不再逐次审批）", title, t.toolName, title))
		if !approved {
			if timedOut {
				return fmt.Errorf("等待审批超时（上限 %s），按拒绝处理：「%s」能力未开启。可请用户在权限芯片勾选「允许%s」，下次被拦时再重新申请", approvalTTL, title, title)
			}
			return fmt.Errorf("用户未批准开启「%s」能力，本次未执行。本轮终局：停止并如实说明，不要换法绕过；命令行替代不了这条能力", title)
		}
		if always {
			wasMode, wasTools := permModeOf(t.sess), t.sess.Tools
			t.app.addCapability(t.sess, capID)
			// 「权限变更」即时落地：注记随新环境快照落库（env 通道）
			t.app.landPermChange(t.sess, permSrcApproval, wasMode, wasTools, t.emit)
		}
		return nil
	}
	var road string // 该能力的"别绕路"那句：各能力能绕的路不同，分开说
	switch capID {
	case capBrowser:
		road = "不要改用 run_command 启动浏览器：命令在 OS 沙箱里执行，沙箱里启动浏览器一定失败（受限令牌，access denied），只会白费一轮。"
	case capComputer:
		road = "不要改用 run_command 模拟键鼠或截屏：命令在 OS 沙箱里执行，做不到这些，只会白费一轮。"
	case capDevice:
		road = "不要改用 adb_* 或 shell 命令绕行：它们同属这条能力，关着时一律被拦。"
	}
	return fmt.Errorf("需要「%s」能力：本会话未开启，该工具已被能力门控拦下。%s如果确实需要，可在重试时带 request_capability=true 与一句 justification 向用户申请开启（弹审批卡由用户决定），或请用户点权限芯片 → 勾选「允许%s」。", title, road, title)
}

// requireNotReadOnly 是只读档的统一门：MCP 连接器调用（mcp_call_tool /
// mcp_read_connector / 插件 mcp__*）、register_skill 这类「宿主直连」通道
// 既不经命令沙箱、也不走 write/exec 的路径围栏，只读档一律拒绝——只读的
// 语义是「不做任何写入/执行」，不能留绕开沙箱的旁门。报错统一指路：
// request_permission 申请切档（弹审批），或用户手动切权限芯片。
func requireNotReadOnly(t *toolContext, what string) error {
	if permModeOf(t.sess) != permReadOnly {
		return nil
	}
	return fmt.Errorf("全电脑只读模式下 %s 不可用：本会话禁止一切写入/执行通道。可用 request_permission(target=workspace-write, justification=…) 向用户申请切换权限档（弹审批卡，批准后立即生效）；或请用户把权限芯片切到「修改工作区」后再试", what)
}

// bundleFullAccessCaps 把完全访问捆绑的能力并入 tools（纯内存合并，不落库；
// 会话创建/改权限时用，保证存储状态与面板勾选一致）。
func bundleFullAccessCaps(tools []string) []string {
	have := make(map[string]bool, len(tools))
	for _, c := range tools {
		have[c] = true
	}
	out := tools
	for _, c := range fullAccessCaps {
		if !have[c] {
			out = append(out, c)
		}
	}
	return out
}

// stripFullAccessCaps 从 tools 中移除完全访问捆绑的能力（纯内存过滤，不落库；
// 从完全访问切回其他档位时用：捆绑点亮的能力随之自动取消勾选）。
func stripFullAccessCaps(tools []string) []string {
	bundled := make(map[string]bool, len(fullAccessCaps))
	for _, c := range fullAccessCaps {
		bundled[c] = true
	}
	out := make([]string, 0, len(tools))
	for _, c := range tools {
		if !bundled[c] {
			out = append(out, c)
		}
	}
	return out
}

// capDefaultsForMode 返回某模式/权限档位下新会话的**默认能力开关**（唯一权威，
// 前端 web/app-chat.js 的 applyCapDefaults 与它同一策略）。现行策略只有一条：
// 「修改工作区」默认开启「操作浏览器」——模型看网页是常态动作，默认给上；
// 用户随时可在权限芯片里取消，显式取消不被覆盖（见各调用点的 tools 是否为 nil）。
// 只对真会注册 browser_* 的模式生效（toolsFor：task/office/coding/
// beginner-coding/skill-create）；只读、完全访问、aux/assistant 不在默认之列——
// 完全访问走 bundleFullAccessCaps 捆绑点亮，语义不同。
func capDefaultsForMode(mode, perm string) []string {
	if perm != permWorkspace {
		return nil
	}
	switch mode {
	case "task", "office", "coding", "beginner-coding", "skill-create":
		return []string{capBrowser}
	}
	return nil
}

// mergeCapDefaults 把默认能力并入 tools（纯内存合并，不落库）：会话创建、以及
// 「从完全访问切回修改工作区」（捆绑项被 strip 掉之后，默认项照旧要在）。
func mergeCapDefaults(tools []string, mode, perm string) []string {
	def := capDefaultsForMode(mode, perm)
	if len(def) == 0 {
		return tools
	}
	have := make(map[string]bool, len(tools))
	for _, c := range tools {
		have[c] = true
	}
	out := tools
	for _, c := range def {
		if !have[c] {
			out = append(out, c)
		}
	}
	return out
}

// syncRunPerms 把库里最新的权限模式与能力开关合并进运行指针：用户在流式
// 执行中改权限芯片（PATCH 处理器写的是自己的副本并落库），运行中的会话每步
// 从这里取新值——下一步的 authorize、环境快照、【权限变更】公告三者即时一致。
// 只合并权限两字段：用量计数等运行态在指针上持续累加，不能用库里的旧值覆盖。
// 运行指针由执行体单协程持有（PATCH/审批写的是各自副本），无数据竞态。
func (a *App) syncRunPerms(sess *SessionMeta) {
	fresh, err := a.Store.Get(sess.ID)
	if err != nil || fresh == nil {
		return // 读失败保持现权（维持现状，不阻断执行）
	}
	sess.Permission = fresh.Permission
	sess.Tools = fresh.Tools
}

// switchToFullAccess 把会话权限切到完全访问并点亮捆绑能力（工作区外写入
// 「始终允许」用）：持久化并广播，前端权限面板随之刷新。
func (a *App) switchToFullAccess(s *SessionMeta) {
	a.switchSessionPerm(s, permFull)
}

// switchSessionPerm 把会话切到指定权限档并广播（request_permission 审批
// 「允许」、写升级「始终允许」到 workspace-write 用）：切到完全访问照点
// 捆绑能力；切到其他档不动能力开关（用户显式勾选的不撤）。广播自带新值
// （permission/tools），前端权限面板与胶囊就地应用，不等列表刷新。
func (a *App) switchSessionPerm(s *SessionMeta, mode string) {
	s.Permission = mode
	if mode == permFull {
		s.Tools = bundleFullAccessCaps(s.Tools)
	}
	_ = a.Store.SaveMeta(s)
	a.broadcast(Event{Type: "session_updated", SessionID: s.ID, Permission: permModeOf(s), Tools: s.Tools})
}

// permModeOf 返回会话的权限模式（未设置时默认修改工作区）。
func permModeOf(s *SessionMeta) string {
	switch s.Permission {
	case permReadOnly, permFull, permWorkspace:
		return s.Permission
	case "danger-full-access": // 官方风格别名：等价 full（与 sandbox_permissions 的接受范围一致）
		return permFull
	default:
		return permWorkspace
	}
}

// permModeLabel 返回权限模式的中文展示名。
func permModeLabel(mode string) string {
	switch mode {
	case permReadOnly:
		return "全电脑只读"
	case permFull:
		return "完全访问"
	default:
		return "修改工作区"
	}
}

// permSrcPanel / permSrcApproval 是【权限变更】公告的两处来源文案。
const (
	permSrcPanel    = "用户在权限面板调整"
	permSrcApproval = "用户在审批卡上点「始终允许」"
)

// modeGrantDesc 返回某权限模式「允许什么」的一句话描述（新增/取消授权行的
// 正文用——降档时同一句话即「被取消的东西」）。
func modeGrantDesc(mode string) string {
	switch mode {
	case permReadOnly:
		return "可读文件、检索内容、抓取网页，禁止任何写入"
	case permFull:
		return "文件读写与命令执行不再受沙箱限制（助手自身配置/数据仍受保护）"
	default:
		return "文件写入仅限工作区，命令在 OS 级沙箱内执行（只能写工作区与沙箱临时目录，联网不限）"
	}
}

// capZh 返回能力开关的中文展示名（权限面板右侧多选同款文案）。
func capZh(cap string) string {
	switch cap {
	case capUnsandboxed:
		return "允许工作区外执行命令"
	case capSandboxWrite:
		return "全电脑只读档下允许沙箱内写入"
	case capBrowser:
		return "操作浏览器"
	case capComputer:
		return "操作电脑"
	case capDevice:
		return "操作手机"
	}
	return cap
}

// capEffectDesc 返回能力开关后的效果说明（开=on）。
func capEffectDesc(cap string, on bool) string {
	onDesc := map[string]string{
		capUnsandboxed:  "沙箱外执行命令不再逐次审批（常设授权）",
		capSandboxWrite: "全电脑只读档下命令可在沙箱内写入",
		capBrowser:      "browser_* 工具现在可用",
		capComputer:     "内置连接器 cua / windows-mcp 现在可用（截屏、点击、键入、切换窗口）",
		capDevice:       "device_* / adb_* 工具现在可用",
	}
	offDesc := map[string]string{
		capUnsandboxed:  "命令升级恢复逐次审批",
		capSandboxWrite: "全电脑只读档下命令恢复禁止写入",
		capBrowser:      "browser_* 工具调用将被拒绝",
		capComputer:     "内置连接器 cua / windows-mcp 的调用将被拒绝",
		capDevice:       "device_* / adb_* 工具调用将被拒绝",
	}
	if on {
		return onDesc[cap]
	}
	return offDesc[cap]
}

// capCanonicalOrder 是能力开关的公告排序（输出确定性）。
var capCanonicalOrder = []string{capUnsandboxed, capSandboxWrite, capBrowser, capComputer, capDevice}

// effCaps 返回会话的生效能力集合（完全访问档捆绑能力视为开启，与
// hasCapability 同口径）。
func effCaps(mode string, tools []string) map[string]bool {
	out := map[string]bool{}
	for _, c := range tools {
		out[c] = true
	}
	if mode == permFull {
		for _, c := range fullAccessCaps {
			out[c] = true
		}
	}
	return out
}

// permChangeNote 生成「权限变更」注记（单行，追加在新环境快照尾部，形如
// 「（权限变更：…）」——与技能变更注记同通道）：告诉模型权限发生了什么变化、
// 现在允许什么、取消了什么。wasMode/wasTools 是变更前的权限模式与能力表，
// s 是变更后的会话；src 是来源文案（面板/审批）。纯函数，归一化后无实际变化
// 时返回空串。
func permChangeNote(src, wasMode string, wasTools []string, s *SessionMeta) string {
	nowMode := permModeOf(s)
	wasCaps, nowCaps := effCaps(wasMode, wasTools), effCaps(nowMode, s.Tools)
	var added, removed []string
	for _, c := range capCanonicalOrder {
		if nowCaps[c] && !wasCaps[c] {
			added = append(added, c)
		}
		if wasCaps[c] && !nowCaps[c] {
			removed = append(removed, c)
		}
	}
	if wasMode == nowMode && len(added) == 0 && len(removed) == 0 {
		return ""
	}
	var parts []string
	if wasMode != nowMode {
		parts = append(parts,
			"权限模式："+permModeLabel(wasMode)+" → "+permModeLabel(nowMode),
			"新增授权："+permModeLabel(nowMode)+"——"+modeGrantDesc(nowMode),
			"取消授权："+permModeLabel(wasMode)+"——「"+modeGrantDesc(wasMode)+"」的旧规则不再适用")
	} else {
		parts = append(parts, "权限模式不变（"+permModeLabel(nowMode)+"）")
	}
	// 档位切换带来的捆绑能力增删已在授权行说明，这里只公告档位之外的手动增删
	if nowMode == permFull || wasMode == permFull {
		added, removed = filterBundled(added), filterBundled(removed)
	}
	if len(added) > 0 {
		parts = append(parts, "新增能力："+joinCapEffects(added, true))
	}
	if len(removed) > 0 {
		parts = append(parts, "取消能力："+joinCapEffects(removed, false))
	}
	parts = append(parts, "来源："+src+"，立即生效，后续工具调用按新权限执行")
	return "权限变更：" + strings.Join(parts, "；")
}

// joinCapEffects 把能力增删清单拼成「名字（效果）、名字（效果）」。
func joinCapEffects(caps []string, on bool) string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, "「"+capZh(c)+"」"+capEffectDesc(c, on))
	}
	return strings.Join(out, "、")
}

// capZhList 批量取能力中文名。
func capZhList(caps []string) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, capZh(c))
	}
	return out
}

// filterBundled 从能力增删清单里剔掉完全访问捆绑项（档位切换已说明，不重复公告）。
func filterBundled(caps []string) []string {
	bundled := map[string]bool{}
	for _, c := range fullAccessCaps {
		bundled[c] = true
	}
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		if !bundled[c] {
			out = append(out, c)
		}
	}
	return out
}

// stashPermNote / takePermNote 暂存与取走「权限变更」注记：权限变化时
// （面板 PATCH / 审批「始终允许」）写入，maybeAppendEnvSnapshot 组装下一次
// 环境快照时取走、追加为新快照尾部的「（权限变更：…）」行——与技能变更
// 注记同走 env 差分通道，同一事实不重复记账。取走即清：注记只随变化后的
// 第一份快照落一次。permMu 保护；App 可能被测试直接构造，map 惰性建。
func (a *App) stashPermNote(sessionID, note string) {
	if note == "" {
		return
	}
	a.permMu.Lock()
	if a.permNotes == nil {
		a.permNotes = map[string]string{}
	}
	a.permNotes[sessionID] = note
	a.permMu.Unlock()
}

func (a *App) takePermNote(sessionID string) string {
	a.permMu.Lock()
	note := a.permNotes[sessionID]
	delete(a.permNotes, sessionID)
	a.permMu.Unlock()
	return note
}

// landPermChange 权限变更的即时落地：算出变更后的环境快照并立刻经
// maybeAppendEnvSnapshot 落库（注记随这份快照携带）。env「重发只由真实变化
// 驱动」约束的是发给模型的差分重发，不约束落库时机——变更瞬间即进时间线与
// 下一次组装的上下文，不等下一次模型调用；下一次组装做等值比较，不会重复落。
// 审批「始终允许」发生在工具执行中间时用这条路径（emit 传运行通道）。
func (a *App) landPermChange(sess *SessionMeta, src, wasMode string, wasTools []string, emit func(Event)) {
	note := permChangeNote(src, wasMode, wasTools, sess)
	if note == "" {
		return
	}
	a.stashPermNote(sess.ID, note)
	a.maybeAppendEnvSnapshot(sess, a.envPrompt(sess), emit)
}

// ---------- 授权凭证 ----------

// Grant 是一次敏感操作的执行凭证：短时效、一次性、与动作绑定。
// 策略评估通过（或用户审批通过）才签发；执行器核销后才执行。
// Policy 是本次执行使用的沙箱策略（read-only / workspace-write / full），
// 执行器据此选择受限启动或普通启动。
type Grant struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Tool      string `json:"tool"`
	Op        string `json:"op"`      // write | exec
	Summary   string `json:"summary"` // 人类可读的动作摘要
	Policy    string `json:"policy"`  // 本次执行的沙箱策略档位
	issuedAt  time.Time
	expiresAt time.Time
	used      bool
}

// issueGrant 签发一张授权凭证（不做事前评估——调用方必须已经过 authorize）。
func (a *App) issueGrant(sessionID, tool, op, summary, policy string) *Grant {
	g := &Grant{
		ID: "g-" + newID(), SessionID: sessionID, Tool: tool, Op: op, Summary: summary,
		Policy:   policy,
		issuedAt: time.Now(), expiresAt: time.Now().Add(grantTTL),
	}
	a.permMu.Lock()
	a.grants[g.ID] = g
	a.permMu.Unlock()
	return g
}

// consumeGrant 核销凭证：必须未核销且未过期，否则拒绝执行（fail closed）。
func (a *App) consumeGrant(g *Grant) error {
	if g == nil {
		return fmt.Errorf("缺少授权凭证，拒绝执行")
	}
	a.permMu.Lock()
	defer a.permMu.Unlock()
	if g.used {
		return fmt.Errorf("授权凭证 %s 已核销（一次性），需重新申请", g.ID)
	}
	if time.Now().After(g.expiresAt) {
		return fmt.Errorf("授权凭证 %s 已过期，需重新申请", g.ID)
	}
	g.used = true
	return nil
}

// ---------- 审批（ask 路径） ----------

type approvalResult struct {
	approved bool
	always   bool
}

// Approval 是一次等待用户决定的越权请求。工具执行同步等待结果；
// 前端经全局事件流收到 approval_required 后弹审批卡，决定经
// POST /api/approvals/{id} 回传。
type Approval struct {
	ID        string `json:"id"`
	SessionID string `json:"sessionId"`
	Tool      string `json:"tool"`
	Summary   string `json:"summary"`
	Reason    string `json:"reason"`
	CreatedAt string `json:"createdAt"`
	// SessionTitle 是发起这条请求的会话标题：前端在别的对话里也弹这张卡
	// （卡片顶部标「来自「某对话」」并可点击跳过去），标题随请求下发，前端不必再查。
	SessionTitle string `json:"sessionTitle,omitempty"`
	expiresAt    time.Time
	decided      bool
	ch           chan approvalResult
	// 经理代理通知（mgrnotify.go）：notified=已通知过数字经理（决议据此回告）；
	// byManager=决议由经理经 call_api 代选（回告标注来源）。permMu 保护。
	notified  bool
	byManager bool
}

// requestApproval 广播审批请求并阻塞等待用户决定（超时/取消视为拒绝）。
// 第三个返回值 timedOut 区分「等到超时都没人点」与「用户明确点了拒绝」：
// 都按拒绝处理，但报错文案分开——模型能向用户如实转述是错过还是拒绝。
func (a *App) requestApproval(ctx context.Context, sess *SessionMeta, tool, summary, reason string) (approved, always, timedOut bool) {
	ap := &Approval{
		ID: "ap-" + newID(), SessionID: sess.ID, Tool: tool,
		Summary: summary, Reason: reason, CreatedAt: now(),
		SessionTitle: sess.Title,
		expiresAt:    time.Now().Add(approvalTTL), ch: make(chan approvalResult, 1),
	}
	a.permMu.Lock()
	a.approvals[ap.ID] = ap
	a.permMu.Unlock()
	a.broadcast(Event{Type: "approval_required", ApprovalID: ap.ID, SessionID: sess.ID, Title: sess.Title,
		Name: tool, Summary: summary, Reason: reason})
	// 久等声光提醒（与提问卡/计划审批共用同一套机制与同一个设置项——设置·常规
	// ·询问提醒的 askAlertAfterSec，默认 4 分钟）：到点还没人决定就唤起窗口 +
	// 叮一声 + 广播 ask_alert（浮窗文案带审批内容摘要）。提醒赶在超时之前
	//（提醒 4 分钟 < 等待上限 5 分钟）；已决定/已超时收走的卡到点不再响。
	var alertTimer *time.Timer
	if a.Cfg.askAlertOn() {
		alertTimer = time.AfterFunc(approvalAlertDelay(a), func() {
			if !a.approvalAlertDue(ap.ID) {
				return
			}
			a.fireAskAlert("", ap.ID, sess.ID, sess.Title, summary, a.Cfg.askAlertText())
		})
	}
	// 经理代理通知（mgrnotify.go）：经理触达过的会话，卡挂了设置时长（默认
	// 30 秒）仍无人处理就通知数字经理——与上面的用户声光提醒并行，各管各。
	var mgrNotifyTimer *time.Timer
	if a.Cfg.mgrNotifyOn() {
		mgrNotifyTimer = time.AfterFunc(mgrNotifyDelay(a), func() {
			a.notifyManagerOfApproval(ap.ID)
		})
	}
	defer func() {
		if alertTimer != nil {
			alertTimer.Stop()
		}
		if mgrNotifyTimer != nil {
			mgrNotifyTimer.Stop()
		}
		a.permMu.Lock()
		delete(a.approvals, ap.ID)
		a.permMu.Unlock()
		a.broadcast(Event{Type: "approval_resolved", ApprovalID: ap.ID, SessionID: sess.ID})
	}()
	timer := time.NewTimer(approvalTTL)
	defer timer.Stop()
	select {
	case r := <-ap.ch:
		a.reportApprovalOutcome(ap, r.approved, r.always, false)
		return r.approved, r.always, false
	case <-timer.C:
		a.reportApprovalOutcome(ap, false, false, true)
		return false, false, true
	case <-ctx.Done():
		return false, false, false
	}
}

// resolveApproval 登记用户的决定并唤醒等待中的工具执行。
// always=true 表示用户点了「始终允许」（常设授权）：命令升级点亮对应
// 能力开关、工作区外写入把会话切到完全访问（权限面板同步刷新），
// 本次放行，之后的同类动作不再审批。
func (a *App) resolveApproval(id string, approved, always bool) error {
	a.permMu.Lock()
	ap, ok := a.approvals[id]
	if ok && !ap.decided {
		ap.decided = true
		ap.ch <- approvalResult{approved: approved, always: always}
	}
	a.permMu.Unlock()
	if !ok {
		return fmt.Errorf("审批请求不存在或已处理（可能已超时）")
	}
	// 注意：always 的能力点亮在 authorize 里按升级类别进行（那里知道
	// 请求的是哪一类动作），这里只负责唤醒等待中的工具执行。
	return nil
}

// pendingApprovals 返回全部待决定审批（前端轮询兜底用；每张卡带会话标题）。
func (a *App) pendingApprovals() []*Approval {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	out := []*Approval{}
	for _, ap := range a.approvals {
		out = append(out, ap)
	}
	return out
}

// approvalAlertDelay 久等提醒的触发延时：统一取「设置·常规·询问提醒」的
// askAlertAfterSec（钳 30-600，默认 240=4 分钟），与提问卡/计划审批共用同一个
// 设置项，不另立一套。做成包级函数变量只为测试能换短延时来验证「先提醒、
// 后超时」的时序（perm_alert_test.go），产品路径永远是设置值。
var approvalAlertDelay = func(a *App) time.Duration {
	return time.Duration(a.Cfg.askAlertAfterSec()) * time.Second
}

// approvalAlertDue 审批的久等提醒到点那一刻是否还该响：这条审批还在等、
// 没人点过（允许/拒绝/始终允许都会收卡；超时也会收走）。拆出来是为了可测
// ——判定条件只有这一处。
func (a *App) approvalAlertDue(id string) bool {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	ap, pending := a.approvals[id]
	return pending && !ap.decided
}

// ---------- 受保护路径 ----------

// isProtectedPath 判断路径是否落在助手自身的配置 / 数据目录内。
// 这些位置存着 API Key 与全部会话记录，任何模式下工具都禁止访问。
// 这是确定性的路径边界检查（同官方 fs-sandbox 的路径判断），
// 不是威胁命令猜测。除原始路径外，canonical 化后的目标一并检查
// （工作区内放指向数据目录的符号链接也不放行）。
func (a *App) isProtectedPath(p string, remote bool) bool {
	if remote || p == "" {
		return false
	}
	for _, q := range []string{p, canonicalFor(p)} {
		lower := strings.ToLower(filepath.Clean(q))
		for _, pp := range a.protectPaths {
			if lower == pp || strings.HasPrefix(lower, pp+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// ---------- 沙箱内浏览器预拦（确定性失败路径的快速失败） ----------

// browserLaunchRes 浏览器启动特征：浏览器可执行名（词边界匹配，.exe 变体
// 与全路径自然覆盖）+ microsoft-edge: 协议形式。chromedriver 等不含纯词
// 边界的不在此列——那是另一个进程，预拦没有收益。见 browserLaunchHit。
var browserLaunchRes = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(msedgewebview2|msedge|chrome|chromium|firefox|iexplore)\b`),
	regexp.MustCompile(`(?i)\bmicrosoft-edge:`),
}

// browserLaunchHit 命令试图启动浏览器时返回命中的特征词（供报错展示），
// 空串表示放行进沙箱。与危险命令护栏（安全边界）不同，这层拦的是
// 「注定失败的白跑」：OS 沙箱内浏览器进程起不来（要写 AppData 等系统
// 位置），且失败多为静默秒退、无任何输出——模型对着空输出只会反复重试
// 打扰用户。因此在执行前拦下并把正确出路（browser_*，宿主直驱不经沙箱）
// 说清楚，不做组件开关。极端误拦（echo/写文件里出现浏览器名）代价低：
// 报错自带出路，模型改写即可。
func browserLaunchHit(cmd string) string {
	for _, re := range browserLaunchRes {
		if m := re.FindString(cmd); m != "" {
			return m
		}
	}
	return ""
}

// ---------- 危险命令护栏（可开关组件） ----------

// destructiveCmds 危险命令拦截表：不可逆破坏在任何模式下都拒绝。
// 官方没有这层（官方靠 OS 沙箱兜底）；这是本产品自己的加固，做成
// 可开关组件（组件/dangerous-cmd-guard，缺省启用），用户可关。
var destructiveCmds = []struct {
	re  *regexp.Regexp
	why string
}{
	{regexp.MustCompile(`(?i)\bformat\s+[a-z]:`), "格式化磁盘"},
	{regexp.MustCompile(`(?i)\b(shutdown|logoff|restart-computer)\b`), "关机/重启"},
	{regexp.MustCompile(`(?i)\bmkfs(\.\w+)?\b`), "格式化文件系统"},
	{regexp.MustCompile(`(?i)\bdd\s+[^|]*of=/dev/`), "直写块设备"},
	{regexp.MustCompile(`(?i)\brm\s+(-[a-z]*r[a-z]*f*|-rf)\s+(/|~|\$HOME)(\s|/|$|\*)`), "递归删除根目录或用户主目录"},
	{regexp.MustCompile(`(?i)\b(rd|rmdir)\b(\s+/[a-z0-9]+)*[\s"']*([a-z]):\\+["'\s]*$`), "递归删除盘根目录"},
	{regexp.MustCompile(`(?i)\bdel\s+(/f\s+|/s\s+|/q\s+)+.*[a-z]:\\?\s*$`), "递归强删盘根目录"},
	{regexp.MustCompile(`(?i)\b(bcdedit|diskpart|vssadmin\s+delete|wbadmin\s+delete|cipher\s+/w)\b`), "系统级破坏工具"},
	{regexp.MustCompile(`:\(\)\s*\{`), "fork 炸弹"},
}

// destructiveCommandHit 命中危险命令时返回拦截原因，空串表示放行。
func destructiveCommandHit(cmd string) string {
	if why := registryNukeHit(cmd); why != "" {
		return why
	}
	for _, d := range destructiveCmds {
		if d.re.MatchString(cmd) {
			return d.why
		}
	}
	return ""
}

// regDeleteRe 捕获 reg delete 的键名参数（带引号的整段或单个无空白词）。
var regDeleteRe = regexp.MustCompile(`(?i)\breg(?:\.exe)?\s+delete\s+("[^"]*"|\S+)`)

// isSystemHive 键名是否指向系统级 hive（HKLM/HKCR，含 \\远程机\HKLM\... 形态）。
// HKCU 是用户态（删坏可重建用户配置解决），不算系统级、不拦。
func isSystemHive(key string) bool {
	k := strings.ToLower(strings.Trim(key, `"`))
	return strings.HasPrefix(k, "hklm") || strings.HasPrefix(k, "hkcr") ||
		strings.Contains(k, `\hklm\`) || strings.Contains(k, `\hkcr\`) ||
		strings.HasSuffix(k, `\hklm`) || strings.HasSuffix(k, `\hkcr`)
}

// registryNukeHit 注册表灾难形态：整键递归删除系统 hive（HKLM/HKCR）。
// 拦截原则：只拦「删整棵子树」——注册表没有回收站，删错 HKLM\SYSTEM /
// HKCR 分支是重装量级且不可逆；单值级写删可逆、有会话记录可回溯，交给
// 沙箱/审批层。两种形态（Go regexp 是 RE2、无 (?!) 前瞻，例外只能用代码判断）：
//   形态 1 reg delete：键名指向 HKLM/HKCR 且不带 /v|/ve（带 /v|/ve 是删
//     单值，放行；/va 删键下全部值仍属大范围，照拦）。按 |&; 分段后逐段
//     匹配，/v 的判断才不被同串其他命令污染。
//   形态 2 PowerShell：Remove-Item/ri 作用于 HKLM:/HKCR: 且带 -rec* 开关。
//     覆盖管道写法 Get-ChildItem HKLM:\... | Remove-Item -Recurse（路径与
//     动词分居管道两侧），故按整条命令判断。已知代价：同一条命令里
//     「删普通目录 -Recurse」又引用 HKLM:/HKCR: 路径的罕见组合会误拦，
//     拆成两条命令即可，报错自带出路。del/rm/rd 等别名误伤面大（cmd 下
//     是文件操作，盘根另有专门规则），不纳入。
func registryNukeHit(cmd string) string {
	low := strings.ToLower(cmd)
	hasRemove, hasRecurse := false, false
	for _, tok := range strings.Fields(low) {
		if tok == "remove-item" || tok == "ri" {
			hasRemove = true
		}
		if strings.HasPrefix(tok, "-rec") {
			hasRecurse = true
		}
	}
	if hasRemove && hasRecurse && (strings.Contains(low, "hklm:") || strings.Contains(low, "hkcr:")) {
		return "整键递归删除系统注册表（HKLM/HKCR）"
	}
	for _, seg := range strings.FieldsFunc(low, func(r rune) bool { return r == '|' || r == '&' || r == ';' }) {
		m := regDeleteRe.FindStringSubmatch(seg)
		if m == nil {
			continue
		}
		singleValue := false
		for _, tok := range strings.Fields(seg) {
			if tok == "/v" || tok == "/ve" {
				singleValue = true
			}
		}
		if !singleValue && isSystemHive(m[1]) {
			return "整键递归删除系统注册表（HKLM/HKCR）"
		}
	}
	return ""
}

// guardRejectError 危险命令护栏的统一拒绝文案：指明是谁拦的、为什么、
// 人工出路，并明说「勿换写法绕过」——注册表规则的 PowerShell 等价写法
// 已一并被拦，提前掐掉模型换写法或转写脚本文件的念头。
func guardRejectError(why string) error {
	return fmt.Errorf("权限拒绝：该命令被「危险命令拦截」护栏拦截（%s）。如确实需要，请让用户在设置→组件中评估该开关后手动执行。请勿尝试换写法（如 PowerShell 注册表路径）或生成脚本文件绕过", why)
}

// guardEnabled 危险命令护栏是否开启：组件缺省启用，组件缺失视为开启。
func (a *App) guardEnabled() bool {
	if a.Components == nil {
		return true
	}
	if _, ok := a.Components.Manifest("dangerous-cmd-guard"); !ok {
		return true
	}
	return a.Components.IsEnabled("dangerous-cmd-guard")
}

// ---------- 工作区边界（本地 Windows / 远程 POSIX 双规则） ----------

// isRemote 定义在 tools.go（执行环境抽象）

// posixRel 是 POSIX 风格的相对路径计算（path 包没有 Rel，按其语义实现）。
func posixRel(base, target string) (string, error) {
	base = path.Clean("/" + base)
	target = path.Clean("/" + target)
	if base == target {
		return ".", nil
	}
	if !strings.HasPrefix(target, base+"/") {
		return "", fmt.Errorf("不能求相对路径：%s 相对 %s", target, base)
	}
	return target[len(base)+1:], nil
}

// inWorkspace 判断绝对路径 p 是否落在会话工作区内。
// 远程会话按 POSIX 路径；本地 Windows 按大小写不敏感比较
// （工作区可能是配置里的相对路径，两侧都先绝对化再比）。
func (t *toolContext) inWorkspace(p string) bool {
	ws := t.workspace()
	if t.isRemote() {
		rel, err := posixRel(ws, p)
		if err != nil {
			return false
		}
		return rel != ".." && !strings.HasPrefix(rel, "../")
	}
	return containsPathLexical(ws, p)
}

// containsPathLexical 词法包含判断：p 是否落在 root 内（Windows 大小写
// 不敏感，其余平台区分大小写；两侧都先绝对化再比）。
func containsPathLexical(root, p string) bool {
	r, err1 := filepath.Abs(root)
	q, err2 := filepath.Abs(p)
	if err1 != nil || err2 != nil {
		return false
	}
	if isWindowsRuntime {
		r, q = strings.ToLower(r), strings.ToLower(q)
	}
	rel, err := filepath.Rel(r, q)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// canonicalFor 归一化路径用于围栏判断：Windows 优先走原生
// GetFinalPathNameByHandle（junction/符号链接/subst 全解析——Go 的
// EvalSymlinks 不解析 junction；对齐 DSH fs-sandbox 的 realpath 规范化）；
// 其余平台用 EvalSymlinks。末段不存在（新建文件）时归一化父目录再拼回。
// 失败时回退 Clean（词法判断兜底）。
func canonicalFor(p string) string {
	if c, ok := canonicalNative(p); ok {
		return c
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	dir := filepath.Dir(p)
	if rp, err := filepath.EvalSymlinks(dir); err == nil {
		return filepath.Join(rp, filepath.Base(p))
	}
	return filepath.Clean(p)
}

// sameFileUnder 身份比对回退：词法不匹配时逐级向上找与 root 同一文件
// （dev/ino，os.SameFile）的祖先——识别 8.3 短名/大小写别名（对齐 DSH
// fs-sandbox containment 的 identity 回退）。
func sameFileUnder(p, root string) bool {
	ri, err := os.Stat(root)
	if err != nil {
		return false
	}
	cur := p
	for {
		if ci, err := os.Stat(cur); err == nil && os.SameFile(ri, ci) {
			return true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return false
		}
		cur = parent
	}
}

// writeFenceAllows 判断写目标是否落在允许的可写根内（对齐 DSH fs-sandbox
// 的 checkedTarget：先 canonicalize 再围栏，词法 + 身份双规则）。本地会话
// 的可写根 = 工作区 + 系统临时目录（DSH writableRoots 同款——临时目录与
// 命令沙箱的授权一致，写临时文件不该弹审批）；远程会话仅工作区（SFTP
// 直写，POSIX 词法边界）。
func (t *toolContext) writeFenceAllows(p string) bool {
	if t.isRemote() {
		return t.inWorkspace(p)
	}
	c := canonicalFor(p)
	for _, root := range []string{t.workspace(), os.TempDir()} {
		r := canonicalFor(root)
		if containsPathLexical(r, c) || sameFileUnder(c, r) {
			return true
		}
	}
	return false
}

// ---------- 观察策略（对齐 DSH fs-observation-policy 的写意图） ----------

// observedKey 观察集的键：本地 Windows 路径大小写不敏感折叠；远程与
// 大小写敏感平台保持原样。
func (t *toolContext) observedKey(p string) string {
	if isWindowsRuntime && !t.isRemote() {
		return strings.ToLower(p)
	}
	return p
}

// markObserved 把路径记入会话的已观察集（read 成功 / write 或 edit 落盘 /
// snapshot_file 打快照后调用）。
func (t *toolContext) markObserved(p string) {
	t.app.observedFiles.Store(t.sess.ID+"\x00"+t.observedKey(p), true)
}

// isObserved 报告路径是否已被本会话观察过。
func (t *toolContext) isObserved(p string) bool {
	_, ok := t.app.observedFiles.Load(t.sess.ID + "\x00" + t.observedKey(p))
	return ok
}

// inJxAITmp 报告路径是否位于工作区 .jxai/tmp/ 之下——观察策略对此豁免：
// tmp 是模型专属暂存区，旧文件跨会话永久残留，而观察集每会话清零、目录
// 检索又不扫 .jxai，模型无从得知旧文件存在，观察策略在这里只会卡住模型
// 自己（实测撞过墙：覆盖 tmp 里的残留脚本被拒）。tmp 里没有用户文件，
// 盲覆盖无风险。远程会话恒 false：.jxai 是本机系统目录，远程工作区没有。
func (t *toolContext) inJxAITmp(p string) bool {
	if t.isRemote() {
		return false
	}
	fold := filepath.Clean
	if isWindowsRuntime {
		fold = func(s string) string { return strings.ToLower(filepath.Clean(s)) }
	}
	rel, err := filepath.Rel(fold(filepath.Join(t.workspace(), jxAIName, "tmp")), fold(p))
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

// ---------- 策略评估入口 ----------

// permAction 是一次待授权的动作。
type permAction struct {
	Op      string // read | write | exec
	Tool    string
	Path    string // write：目标绝对路径
	Command string // exec：完整命令
	Cwd     string // exec：工作目录（空 = 会话工作区；仅决定进程 cwd，不再做边界判断）

	EscalateTo    string // exec：模型请求的更宽沙箱档位（官方 sandbox_permissions）
	Justification string // exec：模型给出的升级理由（官方 justification）
}

// sandboxPolicyForMode 把会话权限模式映射为本次命令执行的沙箱策略档位。
// 这就是「预设捆绑」：模式即策略，完全访问 = 不受限。
func sandboxPolicyForMode(mode string) string {
	switch mode {
	case permReadOnly:
		return permReadOnly
	case permFull:
		return permFull
	default:
		return permWorkspace
	}
}

// sandboxPolicyRank 返回策略档位的宽窄序（窄 < 宽），用于升级方向校验。
func sandboxPolicyRank(p string) int {
	switch p {
	case permReadOnly:
		return 0
	case permWorkspace:
		return 1
	case permFull:
		return 2
	}
	return -1
}

// authorize 是所有工具的权限入口。exec 不再预检命令内容：命令先以会话
// 策略档进沙箱真实执行，被拒绝后模型带 sandbox_permissions+justification
// 重试（act.EscalateTo），该重试在这里弹审批、由用户决定后以更宽策略
// 放行这一次。拒绝驱动，不做威胁命令猜测。
func (t *toolContext) authorize(act permAction) (*Grant, error) {
	mode := permModeOf(t.sess)
	remote := t.isRemote()

	if act.Path != "" && t.app.isProtectedPath(act.Path, remote) {
		return nil, fmt.Errorf("权限拒绝：%s 是助手自身的受保护路径（配置/数据），任何模式下都不允许访问", act.Path)
	}

	switch act.Op {
	case "read", "info", "net":
		// 只读与联网放行（受保护路径除外；web_fetch 的内网目标
		// 限制在工具内自行判断）。
		return nil, nil

	case "write":
		// 文件工具的沙箱：路径边界是确定性的路径判断（fs-sandbox 等价物，
		// canonicalize + 词法 + 身份比对，见 writeFenceAllows）。
		blocked := ""
		switch mode {
		case permReadOnly:
			blocked = "read-only"
		case permWorkspace:
			if !t.writeFenceAllows(act.Path) {
				blocked = "workspace-write"
			}
		}
		if blocked == "" {
			return t.app.issueGrant(t.sess.ID, act.Tool, "write", "写文件 "+act.Path, sandboxPolicyForMode(mode)), nil
		}
		// 升级申请（拒绝驱动）：只读档按目标给「够用的最宽档」——写围栏内的
		// 目标申请 workspace-write 就够（「始终允许」= 会话切到修改工作区，
		// 不再一跳到顶）；围栏外目标才要 full。「允许」= 本次按申请档执行。
		needFull := blocked == "workspace-write" || (blocked == "read-only" && !t.writeFenceAllows(act.Path))
		req := strings.TrimSpace(act.EscalateTo)
		if req == "danger-full-access" {
			req = permFull // 官方词汇表的 full 档别名
		}
		want := ""
		switch req {
		case permFull:
			want = permFull
		case permWorkspace:
			if !needFull {
				want = permWorkspace
			}
		}
		if want != "" {
			just := strings.TrimSpace(act.Justification)
			if just == "" {
				return nil, fmt.Errorf("sandbox_permissions 必须搭配一句 justification（给用户看的理由），本次缺少")
			}
			scope := "写入工作区外"
			if want == permWorkspace {
				scope = "写入（当前会话为全电脑只读）"
			}
			summary := fmt.Sprintf("提升为「%s」以%s：%s｜理由：%s", permModeLabel(want), scope, truncateRunes(act.Path, 160), truncateRunes(just, 120))
			if t.emit != nil {
				t.emit(Event{Type: "tool_wait", Name: act.Tool, Output: "等待用户审批（提升为「" + permModeLabel(want) + "」）…"})
			}
			approved, always, timedOut := t.app.requestApproval(t.ctx, t.sess, act.Tool, summary,
				fmt.Sprintf("当前会话为「%s」，模型请求%s（%s）。「允许」=本次放行；「始终允许」=把会话切到「%s」（此后该档内不再逐次审批）", permModeLabel(mode), scope, act.Path, permModeLabel(want)))
			if !approved {
				if timedOut {
					return nil, fmt.Errorf("等待审批超时（上限 %s），按拒绝处理：该写入未执行。可请用户把权限芯片切档，或稍后重新发起（再次被拒后可重新申请）", approvalTTL)
				}
				return nil, fmt.Errorf("用户未批准该写入，未执行。请停止并如实说明，不要绕过")
			}
			if always {
				wasMode, wasTools := permModeOf(t.sess), t.sess.Tools
				t.app.switchSessionPerm(t.sess, want)
				// 「权限变更」即时落地：注记随新环境快照落库（env 通道），
				// 下一步组装的差分自然带上
				t.app.landPermChange(t.sess, permSrcApproval, wasMode, wasTools, t.emit)
			}
			return t.app.issueGrant(t.sess.ID, act.Tool, "write", "写文件 "+act.Path, want), nil
		}
		// 未带/带错升级参数：按目标给「够用的最宽档」提示
		if blocked == "read-only" {
			wider := permFull
			if !needFull {
				wider = permWorkspace
			}
			return nil, fmt.Errorf("[sandbox: file access denied under read-only mode]\n目标 %s（当前会话为全电脑只读）。可在重试时带 sandbox_permissions=%s 与一句 justification 申请放行（弹审批由用户决定）；也可用 request_permission 直接申请切档，或让用户把权限芯片切到「修改工作区」", act.Path, wider)
		}
		return nil, fmt.Errorf("[sandbox: file access denied under workspace-write mode]\n目标 %s 不在工作区 %s 与系统临时目录内。可在重试时带 sandbox_permissions=full 与一句 justification 申请把会话提升为完全访问（弹审批由用户决定）", act.Path, t.workspace())

	case "exec":
		if !t.app.Cfg.AllowShell {
			return nil, fmt.Errorf("shell 执行已在设置中禁用（allowShell=false）")
		}
		// 危险命令护栏（组件开关，缺省开）：不可逆破坏直接拒绝，
		// 不进入"先执行后升级"的流程——这类命令不该有被放行的机会。
		if t.app.guardEnabled() {
			if why := destructiveCommandHit(act.Command); why != "" {
				return nil, guardRejectError(why)
			}
		}
		policy := sandboxPolicyForMode(mode)
		// 用户常设授权「允许工作区外执行命令」= 关闭命令沙箱：勾选即默认
		// 沙箱外执行（run_command / run_python / 后台作业同一把锁），不再是
		// 「升级时免审批」。设计意图：这一档是修改工作区与完全访问之间的
		// 中间档——命令通道全开（可写任意位置），但 browser_* / computer_* /
		// device_* 仍按各自能力开关把守、不捆绑点亮，因此不叫完全访问。
		// 危险命令护栏不受影响（它是独立于沙箱的一层，上面已检查）。
		if hasCapability(t.sess, capUnsandboxed) {
			policy = permFull
		}

		// 升级重试：模型因真实拒绝请求更宽档位（官方 sandbox_permissions 语义）。
		// 已开启「允许工作区外执行命令」的会话 policy 已是 full（上方改写），
		// 任何升级请求都会被 rank 检查以「无需升级」退回；未开启的会话每次
		// 升级弹审批，用户点「始终允许」点亮能力——此后命令默认沙箱外（上
		// 方改写生效），这正是该常设授权的语义。
		if req := strings.TrimSpace(act.EscalateTo); req != "" {
			if req == "danger-full-access" {
				req = permFull // 官方词汇表的 full 档别名（拒绝 marker 按官方用此名）
			}
			if sandboxPolicyRank(req) <= sandboxPolicyRank(policy) {
				return nil, fmt.Errorf("sandbox_permissions=%s 无效：当前策略已是 %s，无需升级或请求了更窄的档位。请直接重跑命令", req, policy)
			}
			if req != permWorkspace && req != permFull {
				return nil, fmt.Errorf("sandbox_permissions 仅支持 workspace-write 或 danger-full-access（当前支持的最宽档位），收到 %s", req)
			}
			capID := escalationCapability(req)
			if !hasCapability(t.sess, capID) {
				just := strings.TrimSpace(act.Justification)
				if just == "" {
					return nil, fmt.Errorf("sandbox_permissions 必须搭配一句 justification（给用户看的理由），本次缺少")
				}
				summary := fmt.Sprintf("沙箱升级到 %s：%s｜理由：%s", req, truncateRunes(act.Command, 160), truncateRunes(just, 120))
				if t.emit != nil {
					t.emit(Event{Type: "tool_wait", Name: act.Tool, Output: "等待用户审批沙箱升级…"})
				}
				approved, always, timedOut := t.app.requestApproval(t.ctx, t.sess, act.Tool, summary,
					fmt.Sprintf("模型请求把沙箱档位从 %s 升级到 %s 来重跑被拒绝的命令", policy, req))
				if !approved {
					if timedOut {
						return nil, fmt.Errorf("等待审批超时（上限 %s），按拒绝处理：沙箱升级（%s）未获批，该命令本轮终局。可请用户留意待审批卡片并稍后重试", approvalTTL, req)
					}
					return nil, fmt.Errorf("用户未批准沙箱升级（%s），该命令本轮终局：停止并如实说明，不要换着法绕过；其他命令仍可正常执行", req)
				}
				if always {
					// 「始终允许」= 点亮这类升级对应的能力开关（权限面板
					// 可见），之后该类动作不再逐次审批（常设授权）。
					wasMode, wasTools := permModeOf(t.sess), t.sess.Tools
					t.app.addCapability(t.sess, capID)
					// 「权限变更」即时落地：注记随新环境快照落库（env 通道）
					t.app.landPermChange(t.sess, permSrcApproval, wasMode, wasTools, t.emit)
				}
			}
			policy = req
		}
		// 沙箱内浏览器预拦（升级处理之后按最终档位判定）：非 full 档命中
		// browserLaunchHit 直接拒绝、不执行——沙箱内起浏览器必败且多为静默
		// 秒退，让命令白跑只会换来空输出与模型反复重试。full（用户知情
		// 批准的沙箱外执行）不拦：浏览器在沙箱外可以正常启动。
		if policy != permFull {
			if name := browserLaunchHit(act.Command); name != "" {
				return nil, fmt.Errorf("[sandbox: browser launch blocked under %s mode]\n"+
					"命令试图启动浏览器（命中特征：%s），已被预拦、没有执行：OS 沙箱内浏览器进程起不来（写不了 AppData/用户配置等系统位置，且多为静默秒退、无任何报错输出），跑了也只是白跑。"+
					"要看网页/操作浏览器，一律用 browser_* 工具（browser_open 打开、browser_content 读正文、browser_click/browser_type 交互、browser_screenshot 截图）——"+
					"它们由助手进程直接驱动本机 Edge/Chrome，不经命令沙箱，当前权限档即可用；若「操作浏览器」能力未开启，重试 browser_* 时带 request_capability=true 与一句 justification 向用户申请。"+
					"不要改试其他浏览器命令或参数（沙箱内同样必败）；确有「必须在命令行里跑浏览器」的罕见场景，才带 sandbox_permissions=full 申请沙箱外执行（弹审批由用户决定）。",
					policy, name)
			}
		}
		return t.app.issueGrant(t.sess.ID, act.Tool, "exec", truncateRunes(act.Command, 200), policy), nil
	}
	return nil, fmt.Errorf("权限引擎：未知操作类型 %s", act.Op)
}
