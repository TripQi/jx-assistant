package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// decodeCmdOutput 把控制台输出字节还原成合法 UTF-8 文本：优先按 UTF-8，
// 失败按 GB18030（Windows 中文控制台的默认代码页），仍不行用替换符兜底。
// 不做这一步，GBK 输出会以非法字节入库，界面与导出都会出现乱码。
func decodeCmdOutput(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
		return string(s)
	}
	return strings.ToValidUTF8(string(b), "\uFFFD")
}

// 工具集：每个工具 = JSON Schema 定义 + Go 实现。
// 所有会改动外界（写文件 / 执行命令）的工具都先过权限引擎 authorize()
// （见 perm.go）：评估 → 签发授权凭证 → 执行器核销凭证 → 执行；
// 只读与检索类工具做受保护路径检查后直接放行。
// 会话挂载 SSH 远程工作区时，文件操作走 SFTP、命令走远程 shell，
// 工具的参数与行为保持一致（执行环境抽象 env* 方法分发本地/远程）。

// Tool 是模型可调用的一个工具：JSON Schema 定义 + Go 实现。
type Tool struct {
	Spec ToolDef
	Run  func(t *toolContext, args json.RawMessage) (string, error)
}

// toolContext 是一次工具执行的上下文：请求级取消信号、应用引用、
// 当前会话（决定工作区范围与权限模式）与事件上报函数。seq/callID/toolName
// 标出本次调用所在的助手步骤，文件快照据此锚定回退位置。
type toolContext struct {
	ctx      context.Context
	app      *App
	sess     *SessionMeta
	emit     func(Event)
	seq      int64
	callID   string
	toolName string
	// planApproved 只在 RunAgent 主循环构造的 toolContext 里非 nil：exit_plan_mode
	// 获批时置 true，主循环据此把步数预算从头计（计划阶段的探索不挤占执行阶段）。
	planApproved *bool
}

// isRemote 判断会话是否挂载了 SSH 远程工作区。
func (t *toolContext) isRemote() bool {
	return t.sess != nil && t.sess.Remote != nil
}

// workspace 返回会话的工作区根目录（远程会话 = 远程目录）。
func (t *toolContext) workspace() string {
	if t.isRemote() {
		return t.sess.Remote.Dir
	}
	if t.sess.Workspace != "" {
		return t.sess.Workspace
	}
	return t.app.Cfg.Workspace
}

// resolve 把（可能相对的）路径解析为绝对路径；相对路径基于会话工作区。
// 远程会话按 POSIX 规则，本地按 OS 规则。
func (t *toolContext) resolve(p string) (string, error) {
	p = strings.TrimSpace(p)
	if t.isRemote() {
		if p == "" {
			return path.Clean(t.workspace()), nil
		}
		if !strings.HasPrefix(p, "/") {
			p = path.Join(t.workspace(), p)
		}
		return path.Clean(p), nil
	}
	if p == "" {
		return filepath.Abs(t.workspace())
	}
	if !filepath.IsAbs(p) {
		p = filepath.Join(t.workspace(), p)
	}
	return filepath.Abs(p)
}

// toolsFor 按会话模式返回可用工具。数字助理总控拥有编排类工具 + 只读
// 四件套与 web_fetch（答小问题用——十步内的查看自己看，写文件、执行
// 命令仍然没有，使用纪律见 agent.go 的 assistantPrompt；deliver_result
// 是唯一例外——核对路径登记交付卡片，必要时连搬运回上传原件位置也走它）；任务会话拥有全部
// 实操工具 + 子对话派发；辅助对话只有只读工具（定位是答疑，不修改任何东西）。
// 注意：计划/目标模式【不】在这里裁剪工具——目录跨模式保持不变以稳定
// 请求形状（缓存友好），约束靠环境快照公告 + RunAgent 调用时的守卫硬拦
// （见 agent.go 的 suspendedInPlan）；enter/exit_plan_mode / update_goal 因此常驻。
// read_skill 在函数出口统一追加：技能清单经环境快照对**所有模式**注入
// （envPrompt 固定条目），读取入口必须在每个模式在场，否则清单对
// assistant/aux 是「看得见读不着」的空头支票。集中在一处追加（而非散进
// 各分支），新增模式不可能漏带；启动自检 assertSkillVisibility 逐模式校验。
func (a *App) toolsFor(mode string) []Tool {
	history := []Tool{searchSessionsTool(), readSessionMessagesTool()}
	// 待办与定时任务工具（组件 todo-scheduler）：除辅助对话（定位是只答疑）外
	// 各模式都有；组件关掉时为空。
	todos := a.todoTools()
	// Git 远程同步工具（组件 git-panel）：同一组件门控模式；只挂实操模式
	// （沙箱内远程 git 全灭，git_sync 是修改工作区档下唯一的远程通道，
	// 见 gittools.go 文件头）。组件关掉时为空。
	gits := a.gitTools()
	// 组件的模型能力不再各占一个工具定义（用户拍板方案 A）：统一走基础工具
	// component_call（componentcall.go），能调什么由环境快照的「组件能力」块
	// 告知——组件增删能力不动工具目录，请求形状与前缀缓存稳定。
	var tools []Tool
	if mode == "assistant" {
		// 数字助理总控只有「派发工作任务」这一条派发路径：dispatch_sub（派子对话）
		// 与 list_experts（子对话专家名单）不在这里——子对话归任务会话（见 toolsFor
		// 的另一个分支），总控的编排手段是 dispatch_task / send_to_session。
		// 只读四件套 + web_fetch：小问题（十步内的查看）总控自己看文件回答，
		// 不必为「这个文件写了什么」专门派个任务；写与执行类工具绝不进场。
		tools = append([]Tool{listSessionsTool(), readSummaryTool(), dispatchTaskTool(), sendToSessionTool(),
			stopSessionTool(), a.callAPITool(), componentCallTool(),
			// read_image 是纯只读动作（看图不需要写真东西），总控自己就能看清
			// 用户上传的图片再做编排，不必为一个「这张图说了什么」派任务；
			// ocr 仍留给任务会话（识别是加工动作，不是看）。
			readFileTool(), listDirTool(), globTool(), grepTool(), webFetchTool(), readImageTool(),
			deliverResultTool(),
			enterPlanModeTool(), exitPlanModeTool(), updateGoalTool()}, append(todos, history...)...)
	} else if mode == "aux" {
		// enter_plan_mode 不配：aux 只读答疑，没有可暂停的执行动作，计划无意义
		// （exit 保留，用户手动给 aux 会话开计划模式时仍可退出）。
		tools = append([]Tool{readFileTool(), listDirTool(), globTool(), grepTool(), webFetchTool(), a.webSearchTool(), readImageTool(),
			componentCallTool(), exitPlanModeTool(), updateGoalTool()}, history...)
	} else {
		// 创建技能模式专属工具：register_skill 注册闭环只挂在该模式——其他模式的
		// 工具目录保持原形状（缓存与提示词都不受影响）。演示记录器 demo_record_*
		// 已从模型工具集摘除：录制归用户直控（界面按钮经 /api/demo-record/* 端点，
		// 见 recorder.go / server.go），材料以【演示记录】系统通知进对话。
		skillCreate := []Tool{}
		if mode == "skill-create" {
			skillCreate = append(skillCreate, registerSkillTool())
		}
		tools = append(append([]Tool{
			readFileTool(), writeFileTool(), editFileTool(), listDirTool(),
			globTool(), grepTool(), runCommandTool(), runPythonTool(), webFetchTool(), a.webSearchTool(), readImageTool(),
			pdfTool(), ocrTool(), officeSkillTool(),
			snapshotFileTool(), deliverResultTool(), a.dispatchSubTool(), a.listExpertsTool(),
			// 找经理协调（requestmgr.go）：任务侧唯一能触到长期助理的通道——发一条
			// 求助消息给经理会话，立即返回、不等答复。子对话拿到也调不通（调用时
			// 硬拦），assistant / aux 分支不注册（它是被找的那个，不是发起方）。
			requestManagerTool(),
			// call_api 已收窄到 assistant 模式：界面级接口操作归总控编排，
			// 任务会话专注实操（需要时由总控代调）。
			askUserQuestionTool(), requestPermissionTool(), enterPlanModeTool(), exitPlanModeTool(), updateGoalTool(), progressWriteTool(),
			jobOutputTool(), jobListTool(), jobKillTool(),
		}, skillCreate...), append(append(append(append(append(append(todos, gits...), componentCallTool()), append(a.pluginAndMCPTools(), panelScreenshotTool())...), browserTools()...), deviceTools()...), history...)...)
	}
	return append(tools, readUserSkillTool())
}

// pluginAndMCPTools 返回 MCP 相关的全部工具：CC 插件执行型组件的动态
// 工具（mcp__<插件>_<服务器>__<工具> / mods 的 mcp__<插件名>__<工具>，
// 只在重载边界变化）加上独立 MCP 连接器的两个常驻按需工具
// （mcp_read_connector / mcp_call_tool——清单先行、内容按需，连接器增删
// 不改变工具目录形状，见 mcpservers.go）。插件工具集只在重载边界变化，
// 会话中保持稳定（前缀缓存契约）。
func (a *App) pluginAndMCPTools() []Tool {
	var out []Tool
	if a.CCPlugins != nil {
		out = append(out, a.CCPlugins.MCPTools()...)
		out = append(out, a.CCPlugins.ModTools()...)
	}
	out = append(out, a.MCPServers.ConnectorTools()...)
	return out
}

// ---------- Schema 辅助 ----------

func objSchema(props map[string]any, required ...string) map[string]any {
	// required 为空时必须整体省略：序列化成 null 会被服务端按 schema 校验拒绝。
	if len(required) == 0 {
		return map[string]any{"type": "object", "properties": props}
	}
	return map[string]any{"type": "object", "properties": props, "required": required}
}

func strProp(desc string) map[string]any {
	return map[string]any{"type": "string", "description": desc}
}

func intProp(desc string) map[string]any {
	return map[string]any{"type": "integer", "description": desc}
}

func boolProp(desc string) map[string]any {
	return map[string]any{"type": "boolean", "description": desc}
}

func toolSpec(name, desc string, params map[string]any) ToolDef {
	return ToolDef{Type: "function", Function: ToolSpec{Name: name, Description: desc, Parameters: params}}
}

// capRequestProps 能力门控工具（browser_* / computer_* / device_*）的公共升级
// 参数：能力未开启被拦后，模型重试时携带它们向用户申请开启该能力
// （requireCapability 弹审批卡，见 perm.go）。与 sandbox_permissions 同一套
// 拒绝驱动语义：未经真实拒绝不要携带。
func capRequestProps() map[string]any {
	return map[string]any{
		"request_capability": boolProp("仅在被能力门控拦下后的重试中使用：true=向用户申请开启本工具所需的能力（弹审批卡由用户决定）"),
		"justification":      strProp("与 request_capability 搭配：一句话向用户说明为什么需要这个能力"),
	}
}

// withCapRequest 把能力升级参数并入工具参数表（就地合并，返回同一 map）。
func withCapRequest(props map[string]any) map[string]any {
	for k, v := range capRequestProps() {
		props[k] = v
	}
	return props
}

// ---------- 执行环境抽象（本地 / SSH 远程统一文件与目录操作） ----------

// envEntry 是与本地/远程无关的目录项信息。
type envEntry struct {
	Name  string
	IsDir bool
	Size  int64
}

func (t *toolContext) envReadFile(p string) ([]byte, error) {
	if t.isRemote() {
		rc, err := t.app.getRemoteClient(t.sess)
		if err != nil {
			return nil, err
		}
		return rc.readFile(p)
	}
	return os.ReadFile(p)
}

// envWriteFile 写文件（不核销凭证；带凭证的写入口是 envWriteFileWithGrant）。
func (t *toolContext) envWriteFile(p string, data []byte) error {
	if t.isRemote() {
		rc, err := t.app.getRemoteClient(t.sess)
		if err != nil {
			return err
		}
		if err := rc.mkdirAll(path.Dir(p)); err != nil {
			return err
		}
		return rc.writeFile(p, data)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
		return err
	}
	return writeFileAtomic(p, data)
}

// envWriteFileWithGrant 先核销授权凭证再写文件（模型写文件的唯一入口）。
func (t *toolContext) envWriteFileWithGrant(p string, data []byte, g *Grant) error {
	if err := t.app.consumeGrant(g); err != nil {
		return err
	}
	return t.envWriteFile(p, data)
}

// envDeleteFile 删除文件（文件快照回退里"撤回新建文件"用；本地与远程一致）。
func (t *toolContext) envDeleteFile(p string) error {
	if t.isRemote() {
		rc, err := t.app.getRemoteClient(t.sess)
		if err != nil {
			return err
		}
		return rc.remove(p)
	}
	if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// envMkdirAll 递归建目录（本地与远程一致），.jxai 目录体系用。
func (t *toolContext) envMkdirAll(p string) error {
	if t.isRemote() {
		rc, err := t.app.getRemoteClient(t.sess)
		if err != nil {
			return err
		}
		return rc.mkdirAll(p)
	}
	return os.MkdirAll(p, 0755)
}

func (t *toolContext) envListDir(p string) ([]envEntry, error) {
	if t.isRemote() {
		rc, err := t.app.getRemoteClient(t.sess)
		if err != nil {
			return nil, err
		}
		return rc.listDir(p)
	}
	entries, err := os.ReadDir(p)
	if err != nil {
		return nil, err
	}
	out := []envEntry{}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil {
			out = append(out, envEntry{Name: e.Name(), IsDir: e.IsDir()})
			continue
		}
		out = append(out, envEntry{Name: e.Name(), IsDir: e.IsDir(), Size: info.Size()})
	}
	return out, nil
}

func (t *toolContext) envStat(p string) (isDir bool, size int64, err error) {
	if t.isRemote() {
		rc, rerr := t.app.getRemoteClient(t.sess)
		if rerr != nil {
			return false, 0, rerr
		}
		return rc.stat(p)
	}
	info, err := os.Stat(p)
	if err != nil {
		return false, 0, err
	}
	return info.IsDir(), info.Size(), nil
}

// jxaiHiddenRel 报告 rel（相对工作区根的斜杠路径）是否是 .jxai 的系统子目录：
// snapshots（快照正文）、tmp（模型暂存，旧文件跨会话残留）、delivery-backups
// （交付备份）都是内部数据，不进目录检索；.jxai 本体与其下的 uploads（用户
// 上传文件的统一存放处）照常可检索——模型要能看见用户传进来的文件。
func jxaiHiddenRel(rel string) bool {
	for _, sub := range []string{"snapshots", "tmp", "delivery-backups"} {
		if rel == jxAIName+"/"+sub || strings.HasPrefix(rel, jxAIName+"/"+sub+"/") {
			return true
		}
	}
	return false
}

// envWalk 遍历目录树（跳过 .git、node_modules 与 .jxai 的系统子目录），fn 收到
// 相对 root 的斜杠路径；fn 返回 filepath.SkipAll 时整体停止。远程经 SFTP 递归。
// .jxai 的 snapshots/tmp/delivery-backups 是系统内部数据，不进目录检索；
// .jxai/uploads（用户上传文件的统一存放处）与 .jxai 其余内容照常可检索；
// 按路径直接 read 访问不受影响。
func (t *toolContext) envWalk(root string, fn func(rel string, e envEntry) error) error {
	return t.envWalkSkip(root, nil, fn)
}

// envWalkSkip 是 envWalk 的可剪枝版本：skip 收到相对路径、名字与是否目录，返回
// true 的目录整棵跳过、文件直接略过；内置的 .git/node_modules/.jxai 规则照旧生效
// （任一命中即跳过）。只给需要按调用方条件剪枝的工具用（grep 的 exclude 参数），
// list_dir/glob 等其余遍历保持 envWalk 原样——排除是显式请求，不是默认行为。
func (t *toolContext) envWalkSkip(root string, skip func(rel, name string, isDir bool) bool, fn func(rel string, e envEntry) error) error {
	if t.isRemote() {
		rc, err := t.app.getRemoteClient(t.sess)
		if err != nil {
			return err
		}
		visited := 0
		var rec func(dir, rel string) error
		rec = func(dir, rel string) error {
			if visited > 4000 {
				return filepath.SkipAll
			}
			entries, err := rc.listDir(dir)
			if err != nil {
				return nil // 单个目录读不了就跳过，不中断整个遍历
			}
			for _, e := range entries {
				visited++
				r := e.Name
				if rel != "" {
					r = rel + "/" + e.Name
				}
				if skip != nil && skip(r, e.Name, e.IsDir) {
					continue
				}
				if e.IsDir {
					if e.Name == ".git" || e.Name == "node_modules" {
						continue
					}
					if e.Name == jxAIName && r != jxAIName {
						continue // 工作区根以外的 .jxai 目录照旧整棵跳过
					}
					if jxaiHiddenRel(r) {
						continue // .jxai 的系统子目录：内部数据不外露
					}
					if err := rec(dir+"/"+e.Name, r); err != nil {
						return err
					}
					continue
				}
				if err := fn(r, e); err != nil {
					return err
				}
			}
			return nil
		}
		err = rec(root, "")
		if err == filepath.SkipAll {
			return nil
		}
		return err
	}
	return filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil
		}
		if info.IsDir() {
			name := info.Name()
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return nil
			}
			r := filepath.ToSlash(rel)
			if skip != nil && skip(r, name, true) {
				return filepath.SkipDir
			}
			if name == ".git" || (name == "node_modules" && p != root) {
				return filepath.SkipDir
			}
			if name == jxAIName && r != jxAIName {
				return filepath.SkipDir // 工作区根以外的 .jxai 目录照旧整棵跳过
			}
			if jxaiHiddenRel(r) {
				return filepath.SkipDir // .jxai 的系统子目录：内部数据不外露
			}
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if skip != nil && skip(filepath.ToSlash(rel), info.Name(), false) {
			return nil
		}
		return fn(filepath.ToSlash(rel), envEntry{Name: info.Name(), Size: info.Size()})
	})
}

// ---------- 文件类工具 ----------

func readFileTool() Tool {
	return Tool{
		Spec: toolSpec("read", "读取文本文件内容返回。用本工具——而不是 cat 等命令行——查看文本文件；大文件用 offset/limit 分段继续读取。path 支持相对工作区的路径。非 UTF-8 文件（GBK/GB18030/UTF-16 等）会按探测到的编码自动解码后返回（文末注明原编码），不会出现乱码；二进制文件拒绝读取。",
			objSchema(map[string]any{
				"path":   strProp("文件路径（相对或绝对）"),
				"offset": intProp("起始行号，从 1 开始，可选"),
				"limit":  intProp("最多读取行数，可选，默认 2000"),
			}, "path")),
		Run: runReadFile,
	}
}

func runReadFile(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	path, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if _, err := t.authorize(permAction{Op: "read", Tool: "read", Path: path}); err != nil {
		return "", err
	}
	data, err := t.envReadFile(path)
	if err != nil {
		return "", err
	}
	t.markObserved(path) // 观察策略：read 过的文件之后才能被 write 覆盖
	// 编码探测：GBK / UTF-16 等非 UTF-8 文件在这里一次性解成 UTF-8 文本，
	// 模型看到的是正确中文而不是替换符；编码写进文末注记（edit 会按原编码写回）。
	text, enc := decodeText(data)
	if enc == EncBinary {
		return "", fmt.Errorf("%s 是二进制文件，无法按文本读取", path)
	}
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start := p.Offset
	if start < 1 {
		start = 1
	}
	if start > len(lines) {
		return "（起始行超出文件范围）", nil
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 2000
	}
	end := start + limit
	if end > len(lines) {
		end = len(lines)
	}
	out := strings.Join(lines[start-1:end], "\n")
	// 与 agent 层工具结果统一上限（toolResultMaxBytes=128KB）同口径、同一提示：
	// 读取工具自己说「给了 256KB」而 agent 层只让模型看到 128KB 会造成误解。
	out = clipToolResult(out)
	suffix := ""
	if end < len(lines) {
		suffix = fmt.Sprintf("\n…（共 %d 行，可调大 limit 或用 offset 继续读取）", len(lines))
	}
	switch {
	case enc.needsPreserveBytes():
		suffix += fmt.Sprintf("\n（该文件是 %s 编码，已自动解码显示；用 edit 修改会按原编码写回）", enc)
	case enc == EncUnknown:
		suffix += "\n（该文件编码无法确定，已按 GB18030 宽松解码显示；写入前请确认内容）"
	}
	return out + suffix, nil
}

func writeFileTool() Tool {
	return Tool{
		Spec: toolSpec("write", "创建文件或整体替换文件内容（UTF-8 无 BOM 文本）。content 按你提供的原样落盘（不做行尾/编码转换）——覆盖既有 GBK 等非 UTF-8 文件时会把它重写成 UTF-8 并在返回里提示（想保留原编码改用 edit）。既有文件本会话 read 过才能覆盖（观察策略；.jxai/tmp 下跨会话残留的旧文件除外）；局部修改优先用 edit。按会话权限模式的文件沙箱执行：修改工作区档只允许写工作区与系统临时目录内；只读档写入被拒后，工作区内目标带 sandbox_permissions=workspace-write、工作区外目标带 sandbox_permissions=full 与 justification 重试一次即可申请放行（弹审批）。",
			objSchema(map[string]any{
				"path":                strProp("目标文件路径（相对或绝对）"),
				"content":             strProp("要写入的完整内容"),
				"sandbox_permissions": strProp("仅在被拒绝后的重试中使用：full，申请把会话提升为完全访问以写入工作区之外（弹审批）"),
				"justification":       strProp("与 sandbox_permissions 搭配：一句话向用户说明理由"),
			}, "path", "content")),
		Run: runWriteFile,
	}
}

func runWriteFile(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path               string `json:"path"`
		Content            string `json:"content"`
		SandboxPermissions string `json:"sandbox_permissions"`
		Justification      string `json:"justification"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	path, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	// 观察策略（对齐 DSH fs-observation-policy 的写意图）：既有文件本会话
	// read 过才能整体覆盖——防基于臆测/过期记忆的盲覆盖；本会话刚创建或
	// 刚 write/edit 过的文件视为已观察。新建文件不受限。.jxai/tmp/ 下的
	// 跨会话残留文件豁免（模型专属暂存区，模型无从得知它们存在，见
	// inJxAITmp）。
	if _, _, serr := t.envStat(path); serr == nil && !t.isObserved(path) && !t.inJxAITmp(path) {
		return "", fmt.Errorf("%s 已存在但本会话尚未读取过（观察策略）：先用 read 读取该文件再重试覆盖，或改用 edit 做局部修改", filepath.ToSlash(path))
	}
	grant, err := t.authorize(permAction{Op: "write", Tool: "write", Path: path,
		EscalateTo: p.SandboxPermissions, Justification: p.Justification})
	if err != nil {
		return "", err
	}
	// 覆盖既有非 UTF-8 文件会改变它的编码（write 语义固定 UTF-8 无 BOM），
	// 这点必须让模型看见：想保留原编码请改用 edit。写前探测，写后提示。
	prevEnc := EncUTF8
	if !t.isRemote() {
		if _, _, serr := t.envStat(path); serr == nil {
			prevEnc = probeTextFile(path)
		}
	}
	// 带快照写入：改动前内容自动落 checkpoint，编辑历史消息时可回退；
	// 回读注记让模型第一轮就能发现"写进去的不是想要的"（乱码/被外部改写）
	readBack, verified, err := t.snapshotWrite(path, grant, []byte(p.Content))
	if err != nil {
		return "", err
	}
	t.markObserved(path)
	encNote := ""
	if prevEnc.needsPreserveBytes() {
		encNote = fmt.Sprintf("（注意：该文件原为 %s 编码，已按 UTF-8 无 BOM 重写；想保留原编码请改用 edit）", prevEnc)
	}
	return fmt.Sprintf("已写入 %s（%d 字节）%s%s", filepath.ToSlash(path), len(p.Content),
		readBackNote([]byte(p.Content), readBack, verified, true), encNote), nil
}

func editFileTool() Tool {
	return Tool{
		Spec: toolSpec("edit", "对既有文本文件做局部修改：把 old_string 字面量替换为 new_string（匹配时 CRLF/LF 行尾视为等价，写回自动保持文件原行尾风格）。非 UTF-8 文件（GBK/GB18030/UTF-16）按原编码解码后匹配、并按**原编码写回**（GBK 文件仍是 GBK、BOM 保留）；new_string 含原编码无法表示的字符（如 emoji 写进 GBK 文件）时直接报错、文件不改——改用 write 以 UTF-8 保存；编码无法确定（既非合法 UTF-8 也非合法 GB 族）的文件按 UTF-8 无 BOM 写回，并在结果里注明。默认要求 old_string 在文件中恰好出现一次（多次出现时给出更长的片段或设 replace_all）。只读档被拒后，工作区内目标带 sandbox_permissions=workspace-write、工作区外目标带 sandbox_permissions=full 与 justification 重试一次即可申请放行（弹审批）。",
			objSchema(map[string]any{
				"path":                strProp("文件路径"),
				"old_string":          strProp("要被替换的原文（需保证唯一）"),
				"new_string":          strProp("替换后的新文本"),
				"replace_all":         boolProp("为 true 时替换全部出现，可选"),
				"sandbox_permissions": strProp("仅在被拒绝后的重试中使用：full，申请把会话提升为完全访问以写入工作区之外（弹审批）"),
				"justification":       strProp("与 sandbox_permissions 搭配：一句话向用户说明理由"),
			}, "path", "old_string", "new_string")),
		Run: runEditFile,
	}
}

func runEditFile(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path               string `json:"path"`
		OldString          string `json:"old_string"`
		NewString          string `json:"new_string"`
		ReplaceAll         bool   `json:"replace_all"`
		SandboxPermissions string `json:"sandbox_permissions"`
		Justification      string `json:"justification"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if p.OldString == "" {
		return "", fmt.Errorf("old_string 不能为空")
	}
	path, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if _, err := t.authorize(permAction{Op: "read", Tool: "edit", Path: path}); err != nil {
		return "", err
	}
	data, err := t.envReadFile(path)
	if err != nil {
		return "", err
	}
	// 编码：按探测到的原编码解成文本做匹配，改完再按同一编码编回去——全程只有
	// 一次转换（Unicode ↔ 原编码），不存在"GBK 当 UTF-8 再转 GBK"的叠加。
	content, enc := decodeText(data)
	if enc == EncBinary {
		return "", fmt.Errorf("%s 是二进制文件，无法按文本编辑", path)
	}
	// 行尾等价匹配：read 给模型的内容已把 CRLF 归一为 LF（runReadFile），
	// 模型按所见写的 old_string/new_string 自然是 LF；若在原始字节上逐字
	// 匹配，跨行片段会因每个换行差一个 \r 而永远对不上（Windows 项目上
	// 必炸，2026-10-17 会话实际踩中）。匹配与替换都在归一域进行，写回前
	// 再按原文件主导行尾风格恢复——与 DSH fs-local / zcode edit 的
	// 「归一匹配 + 原样写回」同构。
	norm := strings.ReplaceAll(content, "\r\n", "\n")
	oldNorm := strings.ReplaceAll(p.OldString, "\r\n", "\n")
	newNorm := strings.ReplaceAll(p.NewString, "\r\n", "\n")
	count := strings.Count(norm, oldNorm)
	if count == 0 {
		if strings.Contains(content, "\r\n") {
			return "", fmt.Errorf("old_string 在文件中不存在（已按 CRLF/LF 行尾等价匹配，请对照 read 输出核对原文）")
		}
		return "", fmt.Errorf("old_string 在文件中不存在")
	}
	if count > 1 && !p.ReplaceAll {
		return "", fmt.Errorf("old_string 出现了 %d 次，请提供更长的片段保证唯一，或设置 replace_all", count)
	}
	grant, err := t.authorize(permAction{Op: "write", Tool: "edit", Path: path,
		EscalateTo: p.SandboxPermissions, Justification: p.Justification})
	if err != nil {
		return "", err
	}
	var updated string
	replaced := 1
	if p.ReplaceAll {
		updated = strings.ReplaceAll(norm, oldNorm, newNorm)
		replaced = count
	} else {
		updated = strings.Replace(norm, oldNorm, newNorm, 1)
	}
	updated = restoreLineEndings(updated, detectFileLineEndings(content))
	// 按原编码编回：GBK 文件写回仍是 GBK（字节稳定），不可表示字符（emoji）
	// 直接报错、文件不动——绝不用 '?' 静默替换（那是有损且难查的丢字）。
	outBytes, encErr := encodeText(updated, enc)
	if encErr != nil {
		return "", fmt.Errorf("%s：%w（文件未改动）", filepath.ToSlash(path), encErr)
	}
	// 带快照写入：改动前内容自动落 checkpoint（上面 envReadFile 读到的即旧内容）
	readBack, verified, err := t.snapshotWrite(path, grant, outBytes)
	if err != nil {
		return "", err
	}
	// edit 的唯一匹配本身就是更强的 CAS（old_string 必须与当前内容精确
	// 匹配，盲改不可能通过），故不设 read 前置；落盘后记为已观察，后续
	// write 覆盖随之放行（对齐 DSH「刚编辑过视为已观察」）。
	t.markObserved(path)
	keepNote := ""
	switch {
	case enc.needsPreserveBytes():
		keepNote = fmt.Sprintf("（保持原 %s 编码写回）", enc)
	case enc == EncUnknown:
		// 编码无法确定的文件只能按 UTF-8 落盘；写回时明确说出来，不能让编码
		// 在模型不知情的情况下悄悄变掉。
		keepNote = "（原文件编码无法确定，已按 UTF-8 无 BOM 写回）"
	}
	return fmt.Sprintf("已编辑 %s（替换 %d 处）%s%s", filepath.ToSlash(path), replaced, keepNote,
		readBackNote(outBytes, readBack, verified, false)), nil
}

// detectFileLineEndings 按多数派判定文件的主导行尾风格：CRLF 行数多于
// 裸 LF 才算 CRLF，「99% LF + 偶发 CRLF」的混合文件不会被整体翻成 CRLF。
func detectFileLineEndings(content string) string {
	crlf := strings.Count(content, "\r\n")
	if crlf > strings.Count(content, "\n")-crlf {
		return "\r\n"
	}
	return "\n"
}

// restoreLineEndings 把归一域（LF）内容按目标行尾风格写回；目标为 CRLF
// 时内容已无 \r 残留（new_string 同样归一过），不会产生 \r\r\n 双重化。
func restoreLineEndings(norm, eol string) string {
	if eol == "\n" {
		return norm
	}
	return strings.ReplaceAll(norm, "\n", "\r\n")
}

// readBackNote 生成写入后的回读注记（write/edit 成功文案的尾巴）。
// 防御的目的不是不出错，是出错时第一轮就看见：远程 SFTP 写入、外部程序
// 干预、编码转换都能在这里被模型立刻察觉，而不是执行到第三步才发现。
// withFirstLine=true（write）附首行摘要；edit 只报一致/不一致，控制噪音。
func readBackNote(wrote, readBack []byte, verified bool, withFirstLine bool) string {
	if !verified {
		return "（未回读校验）"
	}
	if !bytes.Equal(wrote, readBack) {
		note := fmt.Sprintf("；⚠ 回读与写入不一致（写 %d 读 %d 字节），可能被外部程序改写或转了编码", len(wrote), len(readBack))
		if bytes.HasPrefix(readBack, []byte{0xEF, 0xBB, 0xBF}) {
			note += "，且回读带 UTF-8 BOM"
		}
		return note
	}
	note := "；回读一致"
	if withFirstLine {
		note += "，首行：" + firstLineSnippet(wrote, 60)
	}
	return note
}

// firstLineSnippet 取首行并截断到 max 字符（宽字符按 rune 数）；换行归一后
// 整文件只有一行也照截。空文件返回占位符。
func firstLineSnippet(b []byte, max int) string {
	s := strings.ReplaceAll(string(b), "\r\n", "\n")
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	runes := []rune(s)
	if len(runes) > max {
		runes = append(runes[:max], []rune("…")...)
	}
	if len(runes) == 0 {
		return "（空文件）"
	}
	return strings.TrimSpace(string(runes))
}

// ---------- 文件快照与交付登记（不可追踪修改的配套工具） ----------

func arrayProp(desc string, items map[string]any) map[string]any {
	return map[string]any{"type": "array", "description": desc, "items": items}
}

func snapshotFileTool() Tool {
	return Tool{
		Spec: toolSpec("snapshot_file",
			"为工作区文件打一条历史快照（记录当前内容），供编辑历史消息时回退文件。write/edit 的修改会自动记录快照，无需本工具；凡是准备通过 run_command、外部程序、Office 文档运行库等无法自动追踪的方式修改一个已存在的重要文件，必须先调用本工具，否则该文件将无法回退。",
			objSchema(map[string]any{
				"path": strProp("目标文件路径（相对或绝对）"),
			}, "path")),
		Run: runSnapshotFile,
	}
}

func runSnapshotFile(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if !t.app.snapshotsEnabled() {
		return "文件备份已在设置中关闭：本次未记录快照。若任务需要可回退，请提示用户在设置-常规里打开「文件备份」。", nil
	}
	abs, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if _, err := t.authorize(permAction{Op: "read", Tool: "snapshot_file", Path: abs}); err != nil {
		return "", err
	}
	var before []byte
	existed := true
	isDir, size, serr := t.envStat(abs)
	switch {
	case serr != nil && os.IsNotExist(serr):
		existed = false
	case serr != nil:
		return "", fmt.Errorf("无法读取文件状态: %w", serr)
	case isDir:
		return "", fmt.Errorf("目标是一个目录，快照只支持文件")
	case size > snapshotManualMaxFileBytes:
		return "", fmt.Errorf("文件超过手动快照单文件上限（%d MB）", snapshotManualMaxFileBytes>>20)
	default:
		if before, err = t.envReadFile(abs); err != nil {
			return "", err
		}
	}
	t.markObserved(abs) // 手动快照读过内容，观察策略随之放行后续覆盖
	cp := &Checkpoint{
		SessionID: t.sess.ID, Seq: t.seq, ToolCallID: t.callID, ToolName: t.toolName,
		Path: t.displayPath(abs), ExistedBefore: existed, BeforeSize: int64(len(before)),
		AfterHash: hashBytes(before), AfterSize: size, CreatedAt: now(),
	}
	id, err := t.app.Store.AppendCheckpoint(cp)
	if err != nil {
		return "", fmt.Errorf("快照落库失败: %w", err)
	}
	if existed {
		if err := t.writeSnapshotBody(id, before); err != nil {
			_ = t.app.Store.DeleteCheckpoints(t.sess.ID, []int64{id})
			return "", fmt.Errorf("快照正文写入 .jxai 失败: %w", err)
		}
	}
	if existed {
		return fmt.Sprintf("已记录 %s 的快照（%d 字节，存于 .jxai/snapshots）。之后可以用不可追踪的方式修改它；用户编辑历史消息时可回退到此刻的内容。", cp.Path, len(before)), nil
	}
	return fmt.Sprintf("%s 当前不存在，已记录\"新增文件\"状态：之后创建它，用户编辑历史消息时会把它作为本轮新增撤回。", cp.Path), nil
}

func listDirTool() Tool {
	return Tool{
		Spec: toolSpec("list_dir", "列出目录内容。", objSchema(map[string]any{
			"path": strProp("目录路径，默认为工作区根目录"),
		})),
		Run: runListDir,
	}
}

func runListDir(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if len(args) > 0 {
		if err := json.Unmarshal(args, &p); err != nil {
			return "", err
		}
	}
	path, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if _, err := t.authorize(permAction{Op: "info", Tool: "list_dir", Path: path}); err != nil {
		return "", err
	}
	entries, err := t.envListDir(path)
	if err != nil {
		return "", err
	}
	lines := []string{}
	for _, e := range entries {
		if e.IsDir {
			lines = append(lines, e.Name+"/")
			continue
		}
		lines = append(lines, fmt.Sprintf("%s\t%d B", e.Name, e.Size))
	}
	if len(lines) == 0 {
		return "（空目录）", nil
	}
	return strings.Join(lines, "\n"), nil
}

func globTool() Tool {
	return Tool{
		Spec: toolSpec("glob", "按通配符模式查找文件，如 *.go 或 docs/*.md。返回相对 root 的路径。",
			objSchema(map[string]any{
				"pattern": strProp("通配符模式，如 *.go"),
				"path":    strProp("起始目录，默认工作区根目录"),
			}, "pattern")),
		Run: runGlob,
	}
}

func runGlob(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	root, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if isDir, _, serr := t.envStat(root); serr != nil || !isDir {
		return "", fmt.Errorf("目录不存在: %s", root)
	}
	if _, err := t.authorize(permAction{Op: "info", Tool: "glob", Path: root}); err != nil {
		return "", err
	}
	matches := []string{}
	_ = t.envWalk(root, func(rel string, e envEntry) error {
		if len(matches) >= 500 {
			return filepath.SkipAll
		}
		ok := false
		if strings.Contains(p.Pattern, "/") || strings.Contains(p.Pattern, "\\") {
			ok, _ = path.Match(filepath.ToSlash(p.Pattern), rel)
		} else {
			ok, _ = path.Match(p.Pattern, e.Name)
		}
		if ok {
			matches = append(matches, rel)
		}
		return nil
	})
	if len(matches) == 0 {
		return "（无匹配文件）", nil
	}
	sort.Strings(matches)
	return strings.Join(matches, "\n"), nil
}

func grepTool() Tool {
	return Tool{
		Spec: toolSpec("grep", "在目录内的文本文件中搜索内容，pattern 为正则表达式（非法时按字面量匹配）。返回 文件:行号: 内容。可选 exclude 忽略目录/文件（逗号分隔），路径一律写**相对 path 这个搜索根**的相对路径，不是绝对路径：纯名字（data、*.log）匹配任意层级的目录名或文件名并整棵跳过；含斜杠的从任意层级起算（src/gen 也会命中 deep/src/gen/x.go），前导 / 只锚定搜索根（/data 不影响 src/data）；支持通配符（*/testdata、src/*/*.tmp），不分大小写。文件与文件夹一视同仁。默认不忽略任何业务目录（.git 与 node_modules 本就跳过）。",
			objSchema(map[string]any{
				"pattern": strProp("搜索模式（正则表达式）"),
				"path":    strProp("起始目录，默认工作区根目录"),
				"exclude": strProp("可选：忽略的目录/文件模式（相对 path 搜索根），逗号分隔，如 data,*.log,dist/generated"),
			}, "pattern")),
		Run: runGrep,
	}
}

// splitExcludeList 把 exclude 参数拆成模式列表：逗号（中英文）/分号/空白都当分隔，
// 反斜杠统一成斜杠、去掉前导 ./ 与结尾 /。默认空 = 不忽略任何东西。
func splitExcludeList(s string) []string {
	var out []string
	f := func(r rune) bool {
		return r == ',' || r == '，' || r == ';' || r == '；' || r == ' ' || r == '\t' || r == '\r' || r == '\n'
	}
	for _, raw := range strings.FieldsFunc(s, f) {
		pat := strings.TrimPrefix(strings.TrimSuffix(strings.ReplaceAll(raw, "\\", "/"), "/"), "./")
		if pat != "" {
			out = append(out, pat)
		}
	}
	return out
}

// excludedBy 判断相对路径 rel（及它这一级名字 name）是否被 exclude 模式命中。
// 语义按 gitignore 的习惯（模型最熟的那套），文件与文件夹一视同仁——
// 命中目录即整棵剪枝，命中文件即略过：
//   - 无斜杠的名字模式（data、*.log）：匹配**任意层级**的目录名/文件名；
//   - 含斜杠、无前导 /（src/gen、*/testdata）：从**任意层级**起算，即
//     src/gen 也会命中 deep/src/gen/x.go；
//   - 前导 /（/data）：只锚定搜索根，src/data 不受影响。
//
// 路径一律相对 path 指定的搜索根；不分大小写——搜索时大小写不一致几乎总是
// 笔误而非意图。
func excludedBy(pats []string, rel, name string) bool {
	if len(pats) == 0 {
		return false
	}
	lrel, lname := strings.ToLower(rel), strings.ToLower(name)
	for _, pat := range pats {
		p := strings.ToLower(pat)
		anchored := strings.HasPrefix(p, "/")
		if anchored {
			// 前导 / 锚定搜索根：先于"含不含斜杠"判断，否则剥掉斜杠后会退化成
			// 名字模式、把 src/data 也一起剪掉。
			p = strings.TrimPrefix(p, "/")
			if lrel == p || strings.HasPrefix(lrel, p+"/") {
				return true
			}
			if ok, _ := path.Match(p, lrel); ok {
				return true
			}
			continue
		}
		if !strings.Contains(p, "/") {
			// 名字模式：任意层级的目录名或文件名
			if strings.ContainsAny(p, "*?[") {
				if ok, _ := path.Match(p, lname); ok {
					return true
				}
			} else if lname == p {
				return true
			}
			continue
		}
		// 无前导 / 的路径模式：任意层级起算（深度不限，二级三级照命中）
		if !strings.ContainsAny(p, "*?[") {
			if lrel == p || strings.HasPrefix(lrel, p+"/") ||
				strings.Contains(lrel, "/"+p+"/") || strings.HasSuffix(lrel, "/"+p) {
				return true
			}
			continue
		}
		segs := strings.Split(lrel, "/")
		for i := range segs {
			if ok, _ := path.Match(p, strings.Join(segs[i:], "/")); ok {
				return true
			}
		}
	}
	return false
}

func runGrep(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Exclude string `json:"exclude"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Pattern) == "" {
		return "", fmt.Errorf("pattern 不能为空")
	}
	re, literalErr := regexp.Compile(p.Pattern)
	literal := literalErr != nil
	root, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	if _, err := t.authorize(permAction{Op: "info", Tool: "grep", Path: root}); err != nil {
		return "", err
	}
	excl := splitExcludeList(p.Exclude)
	// 宽容一步：模型偶尔会把绝对路径写进 exclude（约定是相对搜索根），剥掉搜索
	// 根前缀当相对路径用，免得静默失效；剥不掉的就按原样参与匹配。
	if len(excl) > 0 {
		rootSlash := strings.ToLower(filepath.ToSlash(root))
		for i, e := range excl {
			le := strings.ToLower(e)
			if strings.HasPrefix(le, rootSlash+"/") {
				// 剥成锚定形式：绝对路径本身就是确定位置，不该退化成"任意层级
				// 同名目录都剪"
				excl[i] = "/" + e[len(rootSlash)+1:]
			}
		}
	}
	var skip func(rel, name string, isDir bool) bool
	if len(excl) > 0 {
		skip = func(rel, name string, isDir bool) bool { return excludedBy(excl, rel, name) }
	}
	found := []string{}
	_ = t.envWalkSkip(root, skip, func(rel string, e envEntry) error {
		if len(found) >= 200 {
			return filepath.SkipAll
		}
		if e.Size > 2*1024*1024 {
			return nil
		}
		full := root
		if !t.isRemote() {
			full = filepath.Join(root, filepath.FromSlash(rel))
		} else {
			full = path.Join(root, rel)
		}
		data, rerr := t.envReadFile(full)
		if rerr != nil {
			return nil
		}
		// 编码一律走 encoding.go 的探测（全项目唯一真源）：GBK/GB18030/UTF-16
		// 文件先解成 UTF-8 再匹配，否则这些文件里的中文永远搜不到——read/edit/
		// run_command 早就这么做了，只有 grep 漏了。顺带把「含 NUL 就当二进制
		// 跳过」换成「探测结果为二进制才跳过」：UTF-16 文本带 NUL，原来会被
		// 误当二进制整份跳过。
		text, enc := decodeText(data)
		if enc == EncBinary {
			return nil
		}
		for i, ln := range strings.Split(text, "\n") {
			hit := literal && strings.Contains(ln, p.Pattern)
			if !literal && re != nil {
				hit = re.MatchString(ln)
			}
			if hit {
				found = append(found, fmt.Sprintf("%s:%d: %s",
					filepath.ToSlash(full), i+1, truncateRunes(strings.TrimSpace(ln), 200)))
				if len(found) >= 200 {
					break
				}
			}
		}
		return nil
	})
	if len(found) == 0 {
		return "（无匹配）", nil
	}
	return strings.Join(found, "\n"), nil
}

// ---------- shell 工具（本地 Windows：powershell / cmd；远程：POSIX shell） ----------

func runCommandTool() Tool {
	return Tool{
		Spec: toolSpec("run_command", "执行一条 shell 命令并返回输出。命令按会话权限模式在 OS 级沙箱内执行：workspace-write 下只能写工作区与私有临时目录（Windows 受限令牌 / Linux Landlock / 远程沙箱 helper），read-only 下不能写任何位置，联网不受限。尝试沙箱可能拒绝的命令是安全且符合预期的：直接运行并读取标记。若命令因沙箱拒绝失败且更宽档位能让它成功，在同一轮内带 sandbox_permissions（够用的最宽档）与一句 justification 重试同一命令一次——该重试会弹审批由用户批准；未经真实拒绝不要请求升级；被拒后该命令终局，停止并说明，不要绕过。sandbox_permissions 只放宽命令沙箱，开不了 browser_*/computer_* 的能力开关。编码：PS 5.1 路径的引擎已把**输出与读文件**默认都钉在 UTF-8（Get-Content/Select-String/Import-Csv 默认 utf8）；命令里字面出现的文件会先探测编码——全是 GBK 老文件（.bat/.cmd/金蝶导出 CSV）时整条自动切 ANSI(936) 读；输出疑似乱码时只读命令会自动换编码重跑一次（两次都脏会如实说明），写/安装/提交类命令不重跑（重跑=执行两遍）。需要固定某种读法时在命令里显式 -Encoding。看网页/操作浏览器永远不要用命令行启动浏览器（msedge/chrome/firefox/microsoft-edge: 等一律同下场）：非完全访问档下这类命令会被直接预拦、不会执行，报错会指路——一律改用 browser_* 工具（助手进程直接驱动本机 Edge/Chrome，不经命令沙箱；若「操作浏览器」能力未开启，调用会被拦，可在重试该工具时带 request_capability=true 与 justification 向用户申请开启）。",
			objSchema(map[string]any{
				"command":             strProp("要执行的命令"),
				"shell":               strProp("本地执行时的 shell：Windows 默认 powershell（或 cmd），其他平台默认 sh；远程执行忽略"),
				"cwd":                 strProp("工作目录（可选，默认会话工作区；是否可写由沙箱决定，cwd 本身不受限）"),
				"timeout_sec":         intProp("超时秒数，默认 60，上限 600；后台作业忽略"),
				"run_in_background":   boolProp("true 时后台执行并立即返回作业 id（仅本地）。长驻服务（开发服务器、监控程序等）也用它：command 直接写服务本体（如 node server.js、python -m http.server 8000），它会一直跑到你 job_kill、会话停止或软件退出（关软件时连其子进程一并清场）；不要套 Start-Process——shell 退出即作业连坐清理；作业会显示在进度面板「运行中的程序」，用户可手动停止"),
				"visible":             boolProp("一般别用：只有确实需要用户盯着这个命令窗口时才置 true（默认 false = 后台悄悄跑，不弹任何窗口）"),
				"sandbox_permissions": strProp("仅用于被沙箱拒绝后的同轮重试：请求的更宽档位（workspace-write 或 danger-full-access），必须搭配 justification；会弹审批由用户决定"),
				"justification":       strProp("与 sandbox_permissions 搭配：一句话向用户解释为什么这条命令需要更宽权限"),
			}, "command")),
		Run: runCommand,
	}
}

func runCommand(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Command            string `json:"command"`
		Shell              string `json:"shell"`
		Cwd                string `json:"cwd"`
		TimeoutSec         int    `json:"timeout_sec"`
		RunInBackground    bool   `json:"run_in_background"`
		Visible            bool   `json:"visible"`
		SandboxPermissions string `json:"sandbox_permissions"`
		Justification      string `json:"justification"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if strings.TrimSpace(p.Command) == "" {
		return "", fmt.Errorf("command 不能为空")
	}
	cwd := p.Cwd
	if r, err := t.resolve(cwd); err == nil {
		cwd = r // 统一解析为绝对路径（空 = 会话工作区；远程为 POSIX 绝对路径）
	}
	// 拒绝驱动的升级在这里发生：EscalateTo 非空时弹审批，批准后按更宽档执行这一次
	grant, err := t.authorize(permAction{
		Op: "exec", Tool: "run_command", Command: p.Command, Cwd: cwd,
		EscalateTo: p.SandboxPermissions, Justification: p.Justification,
	})
	if err != nil {
		return "", err
	}
	if err := t.app.consumeGrant(grant); err != nil {
		return "", err
	}
	policy := grant.Policy

	// 后台作业：立即返回，不限时；与前台同一条策略与沙箱路径。
	if p.RunInBackground {
		if t.isRemote() {
			return "", fmt.Errorf("后台作业暂不支持远程工作区会话；请去掉 run_in_background 直接执行")
		}
		j, jerr := t.app.startJob(t, p.Command, p.Shell, cwd, policy, "command", nil)
		if jerr != nil {
			return "", sandboxErrHint(jerr)
		}
		return fmt.Sprintf("后台作业已启动：%s（沙箱 %s，不限时）。\n- job_output 查看输出（wait=true 可等它结束）\n- job_list 列出全部作业\n- job_kill 终止\n现在可以继续做别的事。", j.ID, policy), nil
	}

	timeout := time.Duration(60) * time.Second
	if p.TimeoutSec > 0 {
		timeout = time.Duration(min(p.TimeoutSec, 600)) * time.Second
	}

	// 读面编码：命令里出现的文件先探测（encoding.go），据此决定这条 PS 5.1
	// 命令按 UTF-8 还是按 ANSI(936) 读文件。探测不到（程序内部自己读文件的
	// 情形：python 脚本、findstr 之类）就靠下面的输出乱码检测 + 重跑兜底。
	encMode, encHint := detectEncodingMode(cwd, p.Command, p.Shell)
	runOnce := func(mode string) (string, error) {
		return t.app.runChild(t.ctx, sandboxRun{
			Dir: cwd, Command: p.Command, Shell: strings.ToLower(strings.TrimSpace(p.Shell)),
			Timeout: timeout, Policy: policy, Workspace: t.workspace(),
			Remote: t.sess, Visible: p.Visible, Label: "run_command", Model: true,
			EncodingMode: mode,
		})
	}
	out, err := runOnce(encMode)
	if err != nil {
		return "", sandboxErrHint(err)
	}
	// 乱码重跑：只有只读命令才有"执行两次"的豁免（写/安装/提交/启服务重跑
	// 等于执行两遍，副作用不可接受）；远程 Linux、cmd、后台作业不在 PS 5.1
	// 注入范围内，直接跳过。最多重跑一次，两次都脏就如实报告。
	if looksMojibake(out) {
		if !t.isRemote() && isPS51Shell(p.Shell) && isReadOnlyCommand(p.Command) {
			alt := alternateEncodingMode(encMode)
			if out2, err2 := runOnce(alt); err2 == nil {
				mark := fmt.Sprintf("[编码：首次按 %s 读取出现乱码，已按 %s 重跑一次并采用本次结果]", encMode, alt)
				if looksMojibake(out2) {
					mark = fmt.Sprintf("[编码：已按 %s 与 %s 各试一次仍疑似乱码——文件可能既非 UTF-8 也非 GBK，或内容本身异常]", encMode, alt)
				}
				out = out2 + "\n" + mark
			}
		} else {
			out += "\n[编码：输出疑似乱码，但该命令不是只读命令（重跑会执行两遍，故未自动重试）；读文件请显式加 -Encoding Default，或改用 read 工具]"
		}
	}
	if encHint != "" {
		out += "\n[编码：" + encHint + "]"
	}
	return out, nil
}

// sandboxErrHint 沙箱不可用（执行器自身故障）的错误加注：命令没有运行，
// 这是沙箱问题不是命令失败、也不是策略拒绝——不引导升级（升级救不了
// 故障，只会让模型跟审批打架），让模型如实向用户报告（对齐官方
// runnerFailed 提示语义）。非沙箱故障的错误原样返回。
func sandboxErrHint(err error) error {
	if strings.Contains(err.Error(), "sandbox unavailable") {
		return fmt.Errorf("%w。命令没有运行：这是沙箱自身故障，不是命令失败、也不是策略拒绝——不要重试或申请 sandbox_permissions 升级，请如实向用户说明沙箱不可用", err)
	}
	return err
}

// runUnconfinedLocal 不受限本地执行（full 策略）。
func runUnconfinedLocal(ctx context.Context, r sandboxRun) (string, error) {
	if info, serr := os.Stat(r.Dir); serr != nil || !info.IsDir() {
		return "", fmt.Errorf("工作目录不存在: %s", r.Dir)
	}
	// 安卓：内置 Termux 运行时优先（数据目录内的完整 Linux 环境；未就绪回落系统 sh）
	if isAndroidRuntime {
		if prefix := termuxPrefixOrEmpty(); prefix != "" {
			return runTermuxBash(ctx, prefix, r)
		}
	}
	shell := strings.TrimSpace(r.Shell)
	if shell == "" {
		if isWindowsRuntime {
			shell = "powershell"
		} else {
			shell = "sh"
		}
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	var cmd *exec.Cmd
	ps51 := false // PS 5.1 路径：前置 UTF-8 设置 + 注入 Python 环境变量（cmd/pwsh/sh 不注入）
	switch strings.ToLower(shell) {
	case "cmd":
		cmd = exec.CommandContext(ctx, "cmd", "/C", r.Command)
	case "sh", "bash":
		cmd = exec.CommandContext(ctx, "sh", "-c", r.Command)
	case "pwsh", "powershell":
		exe, command := "powershell", ps51Prologue(r.EncodingMode)+r.Command
		if strings.ToLower(shell) == "pwsh" {
			exe, command = "pwsh", r.Command
		} else {
			ps51 = true
		}
		cmd = exec.CommandContext(ctx, exe, "-NoProfile", "-NonInteractive", "-Command", command)
	default:
		return "", fmt.Errorf("不支持的 shell: %s（可选 powershell / cmd / sh）", r.Shell)
	}
	cmd.Dir = r.Dir
	if !r.Visible {
		hideConsoleWindow(cmd) // 默认压窗：桌面形态没有控制台可复用，不压就是一个个黑窗
	}
	cmd.Env = sandboxEnv(true)
	if ps51 {
		cmd.Env = appendEnvUnique(cmd.Env, ps51Utf8Env()...)
	}
	// 整树终止 + 强制收口（proctree_windows.go）：CombinedOutput 内部把
	// Start+Wait 一体化，没机会在启动后挂 job，这里等价展开（同 buffer 合并
	// stdout/stderr），供 attach 介入
	tree := armTreeKill(cmd)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if serr := cmd.Start(); serr != nil {
		tree.finish()
		return "", serr
	}
	tree.attach(cmd)
	err := cmd.Wait()
	tree.finish() // 残留孙进程（adb server 这类守护进程）随 job 句柄关闭清场
	out := buf.Bytes()
	note := ""
	if ctx.Err() == context.DeadlineExceeded {
		note += "\n[命令超时被终止]"
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		note += "\n[命令进程已退出，但残留的后台子进程占着输出管道，已强制收口并终止其进程树。如需进程在本命令结束后继续运行：改用 run_in_background=true，且 command 直接写服务本体（如 node server.js / python -m http.server 8000）；不要用 Start-Process 把进程脱离 shell——脱离的进程会随本命令结束被连坐清理]"
	}
	// 先解码再截断：字节层截断会把 GBK / UTF-8 的多字节序列从中间切断，尾部
	// 变成乱码（解码是按整段判断的，切过的半个字符救不回来）。
	res := strings.TrimRight(decodeCmdOutput(out), "\r\n")
	res = clipHeadTail(res, cmdOutputMaxBytes, cmdTruncNote) + note
	if err != nil && ctx.Err() == nil && !errors.Is(err, exec.ErrWaitDelay) {
		res += fmt.Sprintf("\n[进程退出异常: %v]", err)
	}
	if strings.TrimSpace(res) == "" {
		res = "（命令执行完成，无输出）"
	}
	return res, nil
}

// sandboxEnvWhitelist 是沙箱内执行保留的环境变量白名单：只留运行常规
// 工具链必需的变量，其余（含宿主环境里可能的密钥类变量）全部剔除。
var sandboxEnvWhitelist = []string{
	"PATH", "PATHEXT", "SystemRoot", "windir", "COMSPEC", "TEMP", "TMP",
	"USERPROFILE", "HOMEDRIVE", "HOMEPATH", "APPDATA", "LOCALAPPDATA",
	"PROGRAMFILES", "PROGRAMFILES(X86)", "PROGRAMW6432", "PROGRAMDATA",
	"ALLUSERSPROFILE", "OS", "COMPUTERNAME", "NUMBER_OF_PROCESSORS",
	"PROCESSOR_ARCHITECTURE", "USERNAME", "LANG", "LC_ALL", "TERM", "HOME",
}

// sandboxEnv 返回执行环境变量：沙箱内取白名单子集，沙箱外/完全访问取全量。
func sandboxEnv(full bool) []string {
	if full {
		return os.Environ()
	}
	env := os.Environ()
	out := make([]string, 0, len(env))
	for _, kv := range env {
		i := strings.IndexByte(kv, '=')
		if i <= 0 {
			continue
		}
		for _, w := range sandboxEnvWhitelist {
			if strings.EqualFold(kv[:i], w) {
				out = append(out, kv)
				break
			}
		}
	}
	return out
}

// ---------- PowerShell 5.1 编码注入（仅此路径；cmd/pwsh/sh 不注入） ----------
//
// PS 5.1 的默认有三个坑：$OutputEncoding 默认 ASCII（PS→原生程序管道传中文
// 丢字不可逆）、[Console]::OutputEncoding 默认 OEM 代码页、**Get-Content /
// Select-String / Import-Csv 读文件默认 ANSI(936)**——UTF-8 无 BOM 的文件被
// 解成乱码，而乱码本身是合法 UTF-8 字符串，输出侧的解码兜底救不回来（这是
// 2026-11-09 实际踩到的坑）。所以这里一次把输出与读面都钉死到 UTF-8，并在
// 命令涉及 GBK 文件时（encoding.go detectEncodingMode）整条切成 GBK 模式。
//
// 写面（Out-File/Set-Content）故意不设默认值：5.1 的 -Encoding utf8 会写
// BOM，落到 .bat/.cmd/.ps1 上就是 cmd 解析失败——写文件仍由 write 工具与
// [IO.File]::WriteAllText 负责。注入都是单条命令内可覆盖的机制：命令里显式
// -Encoding 优先，模型读 GBK 老文件仍可写 -Encoding Default。

// ps51Prologue 返回给 PS 5.1 命令的前置语句。mode=gbk 时输出与读面一起切
// 936：文件按 ANSI 解、输出成 GBK 字节，正好被 decodeCmdOutput 的 GB18030
// 兜底接住。try/catch 防"没有控制台可设"的场景把整条命令炸掉。
func ps51Prologue(mode string) string {
	if mode == encodingModeGBK {
		return `try{$e=[Text.Encoding]::GetEncoding(936);$OutputEncoding=$e;[Console]::OutputEncoding=$e;` +
			`$PSDefaultParameterValues['Get-Content:Encoding']='Default';` +
			`$PSDefaultParameterValues['Select-String:Encoding']='Default';` +
			`$PSDefaultParameterValues['Import-Csv:Encoding']='Default'}catch{}; `
	}
	return `try{$OutputEncoding=[Console]::OutputEncoding=[Text.Encoding]::UTF8;` +
		`$PSDefaultParameterValues['Get-Content:Encoding']='utf8';` +
		`$PSDefaultParameterValues['Select-String:Encoding']='utf8';` +
		`$PSDefaultParameterValues['Import-Csv:Encoding']='utf8'}catch{}; `
}

// ps51Utf8Env 是随命令注入的环境变量（Python 3 旧的发行版默认跟随 ANSI 代码页）。
func ps51Utf8Env() []string {
	return []string{"PYTHONUTF8=1", "PYTHONIOENCODING=utf-8"}
}

// appendEnvUnique 往 env 追加 adds，先摘除同名旧值（Windows 环境块与多数
// 运行时对重复变量"先见为准"，直接追加会被宿主已有值静默吞掉）。
func appendEnvUnique(env []string, adds ...string) []string {
	if len(adds) == 0 {
		return env
	}
	drop := make(map[string]bool, len(adds))
	for _, kv := range adds {
		if i := strings.IndexByte(kv, '='); i > 0 {
			drop[strings.ToUpper(kv[:i])] = true
		}
	}
	out := env[:0]
	for _, kv := range env {
		if i := strings.IndexByte(kv, '='); i > 0 && drop[strings.ToUpper(kv[:i])] {
			continue
		}
		out = append(out, kv)
	}
	return append(out, adds...)
}

// ---------- 联网工具 ----------

func webFetchTool() Tool {
	return Tool{
		Spec: toolSpec("web_fetch", "抓取一个 http(s) 地址并返回正文文本；HTML 自动剥离标签。用于查阅文档、API 说明、教程等。内网/本机地址在非「完全访问」模式下会被拒绝。",
			objSchema(map[string]any{
				"url":       strProp("要抓取的 http(s) 地址"),
				"max_chars": intProp("最多返回的字符数，默认 20000，上限 100000"),
			}, "url")),
		Run: runWebFetch,
	}
}

func runWebFetch(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		URL      string `json:"url"`
		MaxChars int    `json:"max_chars"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	u := strings.TrimSpace(p.URL)
	if u == "" {
		return "", fmt.Errorf("url 不能为空")
	}
	if _, err := t.authorize(permAction{Op: "net", Tool: "web_fetch"}); err != nil {
		return "", err
	}
	parsed, err := url.Parse(u)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return "", fmt.Errorf("仅支持 http/https 地址")
	}
	// 防 SSRF：非「完全访问」模式下拒绝内网 / 本机 / 链路本地目标。下面的
	// 供应商抽取兜底同样受这一层约束（供应商也是代为访问，不能绕过）。
	if permModeOf(t.sess) != permFull && isPrivateHost(parsed.Hostname()) {
		return "", fmt.Errorf("权限拒绝：%s 是内网/本机地址，非「完全访问」模式下不允许抓取", parsed.Hostname())
	}
	maxChars := p.MaxChars
	if maxChars <= 0 {
		maxChars = 20000
	}
	if maxChars > 100000 {
		maxChars = 100000
	}
	// 先本地直抓（不花钱、不依赖第三方）；拿不到可用正文才走供应商抽取兜底。
	local := fetchWebPage(t.ctx, u)
	if local.reason == "" {
		return renderWebFetch(u, local.status, "", local.text, maxChars), nil
	}
	var fails []string
	if t.app != nil {
		content, via, errs := t.app.extractFallback(t.ctx, u)
		fails = errs
		if via != "" {
			return renderWebFetch(u, local.status, via, content, maxChars), nil
		}
	}
	if local.err != nil {
		if len(fails) > 0 {
			return "", fmt.Errorf("%w（供应商抽取也没成功：%s）", local.err, strings.Join(fails, "；"))
		}
		return "", local.err
	}
	// 登录墙/付费墙不是「抓取失败」：点明原因，模型才知道该换公开来源/摘要页，
	// 而不是把同一个地址反复重试。
	hint := ""
	if local.loginWall {
		hint = "；这是登录墙/付费墙（正文需要登录或订阅才能看到），换公开来源或让用户自己登录后再看"
	}
	// 本地抓到了但不可用（登录墙/JS 空壳/正文过短）：如实说清楚，别把壳页当正文喂给模型
	if len(fails) > 0 {
		return "", fmt.Errorf("页面没有可用的正文（%s）%s，供应商抽取也没成功：%s", local.reason, hint, strings.Join(fails, "；"))
	}
	return "", fmt.Errorf("页面没有可用的正文（%s）%s；可在 设置 · 网络访问 · 联网搜索 里配置具备「抽取」能力的供应商兜底", local.reason, hint)
}

// loginWallMaxChars 登录墙判定的字数门槛：正文短于此值才可能是「登录壳页」。
// 实测知乎首页 498 字、x.com 443 字都是扫码/注册页——过了「正文过短」的 200
// 门槛、也没有 JS 空壳标记，老判据于是把它们当正常正文喂给了模型。
const loginWallMaxChars = 1200

// loginWallMarkers 登录墙/付费墙特征词（小写比较，中英混放，以后要补就往这里加）。
// 只收「单看就能定性」的组合词，不收「登录」「Sign in」这类单词词根：正常文章
// 的评论区/页脚出现一句"请登录后评论"是常态，真正把关的是上面的字数门槛。
var loginWallMarkers = []string{
	// 中文站（知乎等）
	"扫码登录", "扫码登陆", "验证码登录", "密码登录", "手机号登录", "微信登录",
	"其他方式登录", "登录/注册", "登录或注册", "登录后查看", "登录后阅读",
	"登录以继续", "登录以查看", "登录查看更多", "请先登录", "请登录后", "立即登录", "去登录",
	// 付费墙
	"订阅后查看", "付费后查看", "会员专享", "购买后查看",
	// 英文站（x.com / Medium / 各类订阅站）
	"sign in to continue", "log in to continue", "sign up to continue",
	"sign in to view", "sign in to read", "log in to view",
	"please log in", "please sign in", "login required", "sign in required",
	"already have an account", "continue with google", "continue with apple",
	"enable javascript and cookies to continue",
	"subscribe to continue", "subscribe to read", "this content is for subscribers",
}

// looksLikeLoginWall 判断正文是不是「要先登录/订阅才能看」的壳页。命中特征词
// **且** 正文很短（< loginWallMaxChars）才算——保守是刻意的：一篇正常长文里
// 出现"登录"二字再常见不过，正文够长就一律不下这个判断。
func looksLikeLoginWall(text string) (marker string, hit bool) {
	n := len([]rune(text))
	if n == 0 || n >= loginWallMaxChars {
		return "", false
	}
	l := strings.ToLower(text)
	for _, m := range loginWallMarkers {
		if strings.Contains(l, m) {
			return m, true
		}
	}
	return "", false
}

// webFetchLocal 本地直抓的结果。reason 非空表示「需要供应商抽取兜底」：
// err 是老版本那条错误（直抓失败时的原样文案），text 是能拿到的部分正文。
type webFetchLocal struct {
	status int
	text   string
	err    error
	reason string
	// loginWall 判定为登录墙/付费墙：兜底也失败时要单独点明原因（不是普通抓取失败）
	loginWall bool
}

// fetchWebPage 本地直抓一个页面并按「能不能直接给模型用」判定。需要兜底的情形
// （与调研报告 §8.6 一致）：非 2xx（403/429 常见于反爬）、响应类型不支持、
// 登录墙/付费墙壳页、HTML 剥完正文过短（JS 空壳）、正文短且带 JS 壳特征。
func fetchWebPage(ctx context.Context, pageURL string) webFetchLocal {
	client := &http.Client{Timeout: 20 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, pageURL, nil)
	if err != nil {
		return webFetchLocal{err: err, reason: "请求构造失败"}
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (compatible; JXAssistant/0.1)")
	resp, err := client.Do(req)
	if err != nil {
		return webFetchLocal{err: fmt.Errorf("请求失败: %w", err), reason: "连接失败"}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	raw := string(body)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return webFetchLocal{
			status: resp.StatusCode,
			err:    fmt.Errorf("HTTP %d：%s", resp.StatusCode, truncateRunes(raw, 300)),
			reason: fmt.Sprintf("HTTP %d", resp.StatusCode),
		}
	}
	ct := strings.ToLower(resp.Header.Get("Content-Type"))
	isHTML := strings.Contains(ct, "html") || looksLikeHTML(raw)
	text := raw
	if isHTML {
		text = stripHTML(raw)
	} else if !strings.Contains(ct, "text") && !strings.Contains(ct, "json") &&
		!strings.Contains(ct, "xml") && !strings.Contains(ct, "javascript") && ct != "" {
		return webFetchLocal{
			status: resp.StatusCode,
			err:    fmt.Errorf("不支持的响应类型：%s（仅支持文本类内容）", ct),
			reason: "响应类型不支持（" + ct + "）",
		}
	}
	text = strings.TrimSpace(text)
	if isHTML {
		n := len([]rune(text))
		// 登录墙先判：它的字数常常正好落在 200~500（知乎 498 / x.com 443），
		// 既过不了「正文过短」也配不上 JS 空壳标记，必须单独认出来。
		if marker, wall := looksLikeLoginWall(text); wall {
			return webFetchLocal{status: resp.StatusCode, text: text, loginWall: true,
				reason: fmt.Sprintf("疑似登录墙/付费墙（命中「%s」，正文仅 %d 字）", marker, n)}
		}
		if n < 200 {
			return webFetchLocal{status: resp.StatusCode, text: text,
				reason: fmt.Sprintf("正文只有 %d 字，疑似 JS 空壳页", n)}
		}
		if n < 800 && hasJSShellMarker(raw) {
			return webFetchLocal{status: resp.StatusCode, text: text, reason: "疑似 JS 空壳页"}
		}
	}
	return webFetchLocal{status: resp.StatusCode, text: text}
}

// hasJSShellMarker 正文很短时再看原始 HTML 有没有「要浏览器渲染才有内容」的特征。
// 只在短正文上做判断，避免正常页面里一句 inline 脚本就误触发（抽取是要花钱的）。
func hasJSShellMarker(rawHTML string) bool {
	l := strings.ToLower(rawHTML)
	if len(l) > 65536 {
		l = l[:65536]
	}
	for _, m := range []string{
		"please enable javascript", "enable javascript", "javascript is disabled",
		"cf-browser-verification", "checking your browser", "正在加载", "请开启 javascript",
	} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// renderWebFetch 给模型看的正文：保持老格式「HTTP 状态｜地址」，抽取兜底的结果
// 在后面注明经谁抽取，便于用户判断内容来自哪条链路。
func renderWebFetch(pageURL string, status int, via, text string, maxChars int) string {
	hdr := fmt.Sprintf("HTTP %d｜%s", status, pageURL)
	if status <= 0 {
		hdr = "本地直抓失败｜" + pageURL
	}
	if via != "" {
		hdr += "（经 " + via + " 抽取）"
	}
	r := []rune(strings.TrimSpace(text))
	if len(r) > maxChars {
		r = r[:maxChars]
	}
	return hdr + "\n\n" + string(r)
}

// looksLikeHTML 粗判正文是不是 HTML（Content-Type 缺失时的兜底）。
func looksLikeHTML(s string) bool {
	head := s
	if len(head) > 2048 {
		head = head[:2048]
	}
	l := strings.ToLower(head)
	return strings.HasPrefix(strings.TrimSpace(l), "<!doctype html") ||
		strings.Contains(l, "<html") || (strings.Contains(l, "<body") && strings.Contains(l, "<"))
}

var (
	htmlBlockRe   = regexp.MustCompile(`(?is)<(script|style|noscript)\b[^>]*>.*?</(script|style|noscript)\s*>`)
	htmlCommentRe = regexp.MustCompile(`(?s)<!--.*?-->`)
	htmlTagRe     = regexp.MustCompile(`(?s)<[^>]*>`)
	htmlSpaceRe   = regexp.MustCompile(`\n{3,}`)
)

// stripHTML 剥离 HTML 标签并解码常见实体，尽量保留可读的正文文本。
func stripHTML(s string) string {
	s = htmlBlockRe.ReplaceAllString(s, "")
	s = htmlCommentRe.ReplaceAllString(s, "")
	s = htmlTagRe.ReplaceAllString(s, "\n")
	replacer := strings.NewReplacer(
		"&amp;", "&", "&lt;", "<", "&gt;", ">", "&quot;", `"`,
		"&#39;", "'", "&#x27;", "'", "&nbsp;", " ", "&mdash;", "—", "&hellip;", "…",
	)
	s = replacer.Replace(s)
	lines := []string{}
	for _, ln := range strings.Split(s, "\n") {
		ln = strings.TrimRight(ln, " \t\r")
		if strings.TrimSpace(ln) != "" {
			lines = append(lines, ln)
		}
	}
	out := strings.Join(lines, "\n")
	out = htmlSpaceRe.ReplaceAllString(out, "\n\n")
	return out
}

func isPrivateHost(host string) bool {
	h := strings.TrimSpace(strings.ToLower(host))
	if h == "localhost" || strings.HasSuffix(h, ".localhost") || strings.HasSuffix(h, ".local") {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false // 域名交由 DNS；仅拦截字面内网地址
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
		ip.IsLinkLocalMulticast() || ip.IsUnspecified()
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// ---------- read_image：图片查看 ----------

// read_image：读取工作区里的图片文件，入库为附件并返回文本信封。
// 前端在工具卡片里直接渲染原图；支持视觉的模型在请求组装时由
// inlineImagesForRequest 自动内联看到图片内容（见 attachment.go）。
// DSH 的 read_image 与 ZCode 的 Read 工具都是这个定位，本实现为本栈新写。

func readImageTool() Tool {
	return Tool{
		Spec: toolSpec("read_image",
			"查看一张图片（png / jpg / gif / webp）。图片会展示给用户；若当前模型支持视觉，你将直接看到图片内容。path 支持相对工作区的路径。",
			objSchema(map[string]any{
				"path": strProp("图片文件路径（相对或绝对）"),
			}, "path")),
		Run: runReadImage,
	}
}

func runReadImage(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	path, err := t.resolve(p.Path)
	if err != nil {
		return "", err
	}
	// 只读动作：经权限引擎统一做受保护路径检查后放行。
	if _, err := t.authorize(permAction{Op: "read", Tool: "read_image", Path: path}); err != nil {
		return "", err
	}
	raw, err := t.envReadFile(path)
	if err != nil {
		return "", err
	}
	mediaType := sniffImageMediaType(raw)
	if mediaType == "" {
		return "", fmt.Errorf("%s 不是受支持的图片格式（支持 png / jpg / gif / webp）", path)
	}
	att, err := t.app.storeAttachment(raw, mediaType)
	if err != nil {
		return "", err
	}
	res := imageEnvelope(att, path)
	if !t.app.resolveTarget(t.sess).SupportsImages {
		res += "\n当前模型不支持图片，可使用OCR工具"
	}
	return fmt.Sprintf("%s\n来源文件：%s\n格式：%s，尺寸 %dx%d，%d 字节",
		res, path, att.MediaType, att.Width, att.Height, att.Bytes), nil
}

// ---------- progress_write：会话进度清单 ----------

// progress_write（对齐官方标准栈的 todo 类工具；曾名 todo_write，为与定时
// 任务系统 todo.go / todotools.go 的 todo_task_* 工具划清界限而更名）：模型
// 维护当前任务的会话进度清单，前端在会话顶部渲染进度。清单整体替换写入
// （每次都提交全量列表），持久化在 meta 表（会话附属数据），经
// /api/sessions/{id}/progress 读取。

// ProgressItem 是清单中的一项。
type ProgressItem struct {
	Content string `json:"content"`
	Status  string `json:"status"` // pending | in_progress | completed
}

func progressWriteTool() Tool {
	return Tool{
		Spec: toolSpec("progress_write", "维护当前任务的会话进度清单（整体替换：每次提交完整列表）。用于多步骤任务：开始前把工作拆成进度项；开工一项置 in_progress（同时只能有一项）；做完置 completed 再开下一项。简单到一两步的事不必用。",
			objSchema(map[string]any{
				"progress": arrayProp("进度项数组（全量清单，整体替换提交）", map[string]any{
					"type": "object",
					"properties": map[string]any{
						"content": strProp("进度项内容，一句话"),
						"status":  strProp("pending | in_progress | completed"),
					},
				}),
			}, "progress")),
		Run: runProgressWrite,
	}
}

// progressItemsFromArgs 解析 progress_write 的参数。progress 在 schema 里声明为
// 数组，但单对象仍兼容：旧 schema 曾把它写成单个 {content,status} 对象，照旧
// 说明书的形态发来的单进度项包成单项列表即可，不必让模型重来一轮。
func progressItemsFromArgs(args json.RawMessage) ([]ProgressItem, error) {
	var p struct {
		Progress json.RawMessage `json:"progress"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(string(p.Progress))
	if raw == "" {
		return nil, nil
	}
	if raw[0] == '{' {
		raw = "[" + raw + "]"
	}
	var items []ProgressItem
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, fmt.Errorf("progress 需为进度项数组 [{\"content\":\"…\",\"status\":\"…\"},…]：%v", err)
	}
	return items, nil
}

func runProgressWrite(t *toolContext, args json.RawMessage) (string, error) {
	items, err := progressItemsFromArgs(args)
	if err != nil {
		return "", err
	}
	if len(items) == 0 {
		// 空清单 = 清空（任务取消/不再需要时可用）
		_ = t.app.Store.SetSessionData(t.sess.ID, "progress", "")
		t.app.broadcast(Event{Type: "progress", SessionID: t.sess.ID})
		return "进度清单已清空。", nil
	}
	inProgress := 0
	for i := range items {
		td := &items[i]
		td.Content = strings.TrimSpace(td.Content)
		if td.Content == "" {
			return "", fmt.Errorf("第 %d 项 content 为空", i+1)
		}
		switch td.Status {
		case "pending", "":
			td.Status = "pending"
		case "in_progress":
			inProgress++
		case "completed":
		default:
			return "", fmt.Errorf("第 %d 项状态非法：%s（可选 pending / in_progress / completed）", i+1, td.Status)
		}
		if len(td.Content) > 500 {
			td.Content = truncateRunes(td.Content, 500)
		}
	}
	if inProgress > 1 {
		return "", fmt.Errorf("同时只能有一项 in_progress（当前 %d 项），请把其余改为 pending", inProgress)
	}
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	if err := t.app.Store.SetSessionData(t.sess.ID, "progress", string(data)); err != nil {
		return "", err
	}
	t.app.broadcast(Event{Type: "progress", SessionID: t.sess.ID})
	done := 0
	for _, td := range items {
		if td.Status == "completed" {
			done++
		}
	}
	return fmt.Sprintf("进度清单已更新（%d 项，完成 %d）。", len(items), done), nil
}

// progressListOf 读取会话的进度清单（前端 API 用）；空返回 nil。
func progressListOf(a *App, sessionID string) []ProgressItem {
	raw := a.Store.GetSessionData(sessionID, "progress")
	if raw == "" {
		return nil
	}
	out := []ProgressItem{}
	if json.Unmarshal([]byte(raw), &out) != nil {
		return nil
	}
	return out
}
