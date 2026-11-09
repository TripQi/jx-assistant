package main

// 建会话的 service 单源（先例：uploadpath.go 的附件登记、chatmodes.go 的模式
// 清单——HTTP 与 AI 工具共用同一实现，「人工建会话是什么效果，派发建出来的
// 就是什么效果」）。三个入口收敛到这里：
//  1. HTTP 人工建会话（server.go handleCreateSession，含右栏分支克隆）；
//  2. dispatch_task 派顶层工作任务（assistant.go runDispatchTask）；
//  3. dispatch_sub 派子对话（assistant.go runDispatchSub）。
//
// service 只管「把一个会话正确地建出来」：字段构造、权限默认、能力默认、
// 分支克隆、历史继承、.jxai/tmp 预建、创建技能模式的演示材料兜底。各入口
// 保留自己的**入口规则**——人工可任意选档与模式；AI 派发只能同等或降权、
// 模式取自 chatModes 清单——规则校验完再把最终值传进来，两边不复述。

import (
	"fmt"
	"strings"
)

// createSessionError 带建议 HTTP 状态码的建会话错误（客户端参数问题=400，
// 落库/继承失败=500），HTTP 处理器照码回，派发工具只取文案（同 uploadPathError）。
type createSessionError struct {
	code int
	msg  string
}

func (e *createSessionError) Error() string { return e.msg }

func createSessionErr(code int, format string, a ...any) error {
	return &createSessionError{code: code, msg: fmt.Sprintf(format, a...)}
}

// createSessionErrCode 取建会话错误建议的状态码（非本类错误一律 500）。
func createSessionErrCode(err error) int {
	if e, ok := err.(*createSessionError); ok && e.code > 0 {
		return e.code
	}
	return 500
}

// createSessionParams 三入口共用的建会话参数。权限/模式是**入口校验后的最终
// 值**（service 只兜底默认：模式未知退 task、权限空按「workspace-write +
// 创建技能/数字经理主对话 full」补——与人工口径一致；派发入口总是显式传值，
// 不走默认）。
type createSessionParams struct {
	Mode       string
	Title      string
	Workspace  string
	ParentID   string // 挂靠的父会话（子对话/分支克隆；顶层任务与普通会话为空）
	Provider   string
	Model      string
	Permission string
	Thinking   string
	PlanMode   bool
	GoalMode   bool
	GoalText   string
	Tools      []string
	Remote     *RemoteWorkspace

	Expert       string // 子对话专家名（dispatch_sub 专属；空=非专家）
	DispatchedBy string // 派发链（dispatch_task 专属：回报按它找派发方）

	CloneFromParent bool // 分支克隆：字段全部照抄父会话（人工右栏建子对话）
	CopyHistory     bool // 建成后复制父会话历史（克隆与专家继承两种形态共用）
}

// createSession 是建会话的唯一落装口（见文件头）。返回建好的会话；失败时不留
// 半截——历史继承失败会把已落库的会话回滚删除。
func (a *App) createSession(p createSessionParams) (*SessionMeta, error) {
	mode := p.Mode
	if !modeKnown(mode) {
		mode = "task"
	}
	perm := p.Permission
	if perm == "" {
		perm = permWorkspace
		// 创建技能模式默认完全访问：要看懂用户演示（截屏/键鼠记录）并亲自
		// 验证技能关键步骤，操作电脑/浏览器是刚需；入口显式传权限则尊重。
		if mode == "skill-create" {
			perm = permFull
		}
		// 数字经理主对话（新装机首启自动创建的那个）默认完全访问：派发
		// 「只能同等或降权」，总控自己先站到最高档，派出去的任务才有完整的
		// 档位空间（任务默认仍是修改工作区）；入口显式传权限则尊重。已建
		// 主对话的权限存在会话库里，不受这个默认影响。
		if mode == "assistant" {
			perm = permFull
		}
	}
	remote := p.Remote
	if remote != nil {
		a.fillRemoteCreds(remote) // 面板复挂路径：密码留在服务端登记库里
		spec, err := remote.normalize()
		if err != nil {
			return nil, createSessionErr(400, "远程工作区配置无效: %v", err)
		}
		remote = spec
	}
	// 父会话校验：只能挂在一级会话下——子对话不能再建子对话（与 dispatch
	// 工具的运行时守卫一起保证只有一级）。
	var parent *SessionMeta
	if pid := strings.TrimSpace(p.ParentID); pid != "" {
		m, err := a.Store.Get(pid)
		if err != nil {
			return nil, createSessionErr(400, "父会话不存在：%s", pid)
		}
		if m.ParentID != "" {
			return nil, createSessionErr(400, "子对话不能再建子对话：%s 已经是子对话（只允许一级）", pid)
		}
		parent = m
	}
	if p.CloneFromParent && parent == nil {
		return nil, createSessionErr(400, "inheritContext 需要同时指定 parent")
	}
	if p.CopyHistory && parent == nil {
		return nil, createSessionErr(400, "复制父对话上下文需要同时指定 parent")
	}
	m := &SessionMeta{
		ID:           newID(),
		Title:        strings.TrimSpace(p.Title),
		Mode:         mode,
		Workspace:    strings.TrimSpace(p.Workspace),
		Remote:       remote,
		ParentID:     strings.TrimSpace(p.ParentID),
		Provider:     strings.TrimSpace(p.Provider),
		Model:        strings.TrimSpace(p.Model),
		Permission:   perm,
		Thinking:     strings.TrimSpace(p.Thinking),
		PlanMode:     p.PlanMode,
		GoalMode:     p.GoalMode,
		GoalText:     truncateRunes(p.GoalText, 2000),
		Tools:        p.Tools,
		Expert:       strings.TrimSpace(p.Expert),
		DispatchedBy: strings.TrimSpace(p.DispatchedBy),
		CreatedAt:    now(),
		UpdatedAt:    now(),
	}
	if p.CloneFromParent {
		// 人工建的子对话＝父对话的分支：模式/工作区/远程/模型/权限/能力全部
		// 照抄父会话——系统提示词与工具目录与父完全一致，继承来的历史才能接
		// 在同一个前缀上命中模型的 KV 前缀缓存。计划/目标刻意重置：子对话不
		// 该替父会话自动续跑。
		m.Mode = parent.Mode
		m.Workspace = parent.Workspace
		m.Provider = parent.Provider
		m.Model = parent.Model
		m.Permission = parent.Permission
		m.Thinking = parent.Thinking
		m.Tools = parent.Tools
		if parent.Remote != nil {
			r := *parent.Remote
			m.Remote = &r
		}
		m.Origin = "human"
		m.PlanMode, m.GoalMode, m.GoalStatus = false, false, ""
		if m.Title == "" {
			m.Title = "子对话"
		}
	}
	if permModeOf(m) == permFull {
		m.Tools = bundleFullAccessCaps(m.Tools) // 完全访问：预设捆绑能力一并点亮
	} else if p.Tools == nil {
		// 请求没带能力开关（tools 字段缺席）：按默认策略补（修改工作区默认
		// 开启「操作浏览器」）。显式传空数组 = 用户主动取消勾选，尊重用户。
		m.Tools = mergeCapDefaults(m.Tools, m.Mode, permModeOf(m))
	}
	if m.GoalMode {
		m.GoalStatus = "active" // 创建即带目标模式：状态从 active 起步
	}
	if err := a.Store.Create(m); err != nil {
		return nil, err
	}
	// 选定工作目录后即建 .jxai/tmp：模型临时文件的约定去处（见系统提示词）
	if m.Remote == nil {
		(&toolContext{app: a, sess: m}).ensureJxAI()
	}
	if p.CopyHistory {
		if _, err := a.copyInheritedContext(parent.ID, m.ID); err != nil {
			// 上下文没复制成功就退回：留着半截继承的子对话比没有更糟
			_ = a.Store.Delete(m.ID)
			return nil, createSessionErr(500, "继承父对话上下文失败: %v", err)
		}
	}
	// 首页「先录后聊」的兜底入口：建创建技能会话时工作区若已有演示材料，
	// 注入一条【演示记录】清单通知（同文件上传通知机制），模型开轮即知道
	// 有哪些现成记录可分析——人工建的与 dispatch 派出的 skill-create 会话
	// 同一待遇（service 单源的意义之一：入口差异不产生行为差异）。
	if m.Mode == "skill-create" && m.Remote == nil {
		ws := m.Workspace
		if ws == "" {
			ws = a.Cfg.Workspace
		}
		if dirs := recorderRecentDirs(ws, 3); len(dirs) > 0 {
			_ = a.Store.AppendRow(m.ID, &StoredMessage{Message: TextMessage("user", demoRecordListNote(dirs)), Kind: kindReport, Origin: "demo", CreatedAt: now()})
		}
	}
	return m, nil
}

// applyPermissionChange 是「改会话权限档」的公共落装（人工 PATCH 权限面板与
// send_to_session 的 permission 参数共用同一套能力联动）：赋值 + 升到完全
// 访问捆绑点亮；从完全访问降回则取消捆绑、补回默认能力（toolsExplicit=本
// 次请求显式带了能力开关清单，显式清单优先于联动补默认）。返回能力开关是否
// 发生了变化（供调用方记上下文事件）。
func applyPermissionChange(m *SessionMeta, perm string, toolsExplicit bool) bool {
	was := permModeOf(m)
	wasTools := m.Tools
	m.Permission = strings.TrimSpace(perm)
	switch {
	case permModeOf(m) == permFull:
		m.Tools = bundleFullAccessCaps(m.Tools)
	case was == permFull && !toolsExplicit:
		// 从完全访问切回：捆绑能力随之取消，默认能力补回来（修改工作区
		// 默认开启「操作浏览器」——切回时同时打开它）
		m.Tools = mergeCapDefaults(stripFullAccessCaps(m.Tools), m.Mode, permModeOf(m))
	}
	return fmt.Sprint(wasTools) != fmt.Sprint(m.Tools)
}
