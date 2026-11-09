package main

// run_python 工具桥：让脚本（前台同步或后台作业）以本会话身份调用白名单
// 工具、派发子任务并等待其回报——「模型写程序、程序调度工具/子代理」的编排
// 原语（对齐 DSH PTC 的压碎步回合与 ZCode workflow 的程序化门控；权限语义
// 按我们自己的管线走）。动机见 run_python.go 头注：碎步 tool call 每步一次
// 模型回合，脚本内循环调用把 N 步合成 1 步，中间结果不进模型上下文。
//
// 链路（全程本机回环，不出网、与远程服务端无关）：
//   脚本 import jx → jx.call("read", {...}) → POST 127.0.0.1:<listenPort>/api/bridge/tool
//   → handleBridgeTool：loopbackOnly → token 校验（绑会话+脚本运行期）
//   → 以该会话 toolContext 走既有工具管线（权限/沙箱/审计照常生效）
//
// 安全设计（三层）：
//   1. token 一次性：前台脚本随 runChild 返回注销（bridgeRelease），后台作业随
//      收尾回调注销。token 注入会话私有目录的 jx.py（.jxai/tmp/<会话id>/），
//      该目录本就只有本会话可见；泄露面 = 会话自身，与脚本本身同权，不放大。
//   2. 白名单 + 剥审批参数：只读/文件/命令/联网检索类可调；sandbox_permissions/
//      justification / run_in_background / visible 一律剥除——桥上没有交互上下文，
//      绝不允许后台脚本弹审批卡或起嵌套后台作业（对齐 ZCode「命令集提交时固定」
//      的可审批性哲学：要审批的动作，回到模型前台走）。
//   3. 限额防失控：单脚本调用总量 200 次、并发 4、同时在跑的桥派子任务 4 个
//      （对齐 DSH PTC 的调用额度设计——脚本死循环刷不动工具）。
//
// 子任务回报路由（P2 关键）：桥派子任务完成时的【派发回报】不走常规「投父会话
// +起汇报轮」（runtime.go 的 bridgeInterceptReport 拦截），而是存进收集条目，
// 由脚本 jx.wait 取走；脚本退出时未取走的由 bridgeRelease 转交常规投递，保证
// 主对话只被【后台脚本回报】打扰一次、且任何回报都不丢。
//
// 审计：每次桥调用写日志（不进模型上下文——脚本只拿它要的结果，过程留在日志
// 里可事后对账）。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	bridgeCallMax   = 200 // 单脚本（一次 run_python 调用）的桥调用总量上限
	bridgeFlightMax = 4   // 单脚本同时在跑的桥工具调用上限
	bridgeDispMax   = 4   // 单脚本同时在跑的桥派子任务上限
	bridgeBodyMax   = 1 << 20
	bridgeReportMax = 8 * 1024 // wait 返回给脚本的回报正文截断（字节）
)

// bridgeToken 是一次 run_python 调用的桥凭证。
type bridgeToken struct {
	token     string
	sessionID string
	key       string // 归集键：前台 = call:<id>；后台 = job:<id>。释放与回报归集都用它
	calls     int
	inFlight  int
	dispAlive int // 已派发、回报未取走的子任务数
}

// bridgeWait 是一个桥派子任务的回报收集条目。
type bridgeWait struct {
	subID   string
	title   string
	key     string
	tok     *bridgeToken
	report  string
	done    bool
	ready   chan struct{}
	started time.Time
}

// bridgeState 懒初始化（App 在测试里常是零值构造，不能依赖启动期 init）。
func (a *App) bridgeState() {
	if a.bridgeTokens == nil {
		a.bridgeTokens = map[string]*bridgeToken{}
	}
	if a.bridgeWaits == nil {
		a.bridgeWaits = map[string]*bridgeWait{}
	}
}

// bridgeAllow 是桥上可调用的工具白名单（按 toolsFor 各模式目录里的 Spec.Name）。
// assistant 模式的工具目录本就没有写/执行类，桥在 assistant 会话调 write 会在
// 「工具不在本会话目录」处被拒——权限语义与模型前台完全一致。
var bridgeAllow = map[string]bool{
	"read": true, "glob": true, "grep": true, "list_dir": true,
	"write": true, "edit": true, "snapshot_file": true,
	"run_command": true, "web_fetch": true, "web_search": true, "read_skill": true,
}

// bridgeClientPy 是落盘给脚本的 jx.py 模板（端口与 token 两处注入）。模板内
// 刻意不用 %s/%d 以外的百分号，Sprintf 安全。纯标准库，便携运行时可直接跑。
const bridgeClientPy = `# jx —— 坚祥数字助理工具桥客户端（run_python 自动生成；token 一次性，仅本次脚本运行期有效）
import json as _json
import urllib.request as _rq

_URL = "http://127.0.0.1:%d/api/bridge/tool"
_TOKEN = "%s"

def call(tool, args=None, timeout=600):
    """调用本会话白名单内的工具（read/glob/grep/list_dir/write/edit/snapshot_file/
    run_command/web_fetch/web_search/read_skill）。返回 dict：
    {"ok": true, "result": "<工具输出文本>"} 或 {"ok": false, "error": "<原因>"}。
    需要用户审批的操作（如沙箱升级）会被拒绝——把这类动作留给模型前台执行。"""
    body = _json.dumps({"token": _TOKEN, "tool": tool, "args": args or {}}).encode("utf-8")
    req = _rq.Request(_URL, data=body, headers={"Content-Type": "application/json"})
    with _rq.urlopen(req, timeout=timeout) as resp:
        return _json.loads(resp.read().decode("utf-8"))

def dispatch(task, title="", model="", permission=""):
    """派发一个后台子任务（与 dispatch_task 同一管线：自足任务描述，看不到本对话
    记录），立即返回 {"ok": true, "result": {"session_id": "...", "title": "..."}}。
    用 wait() 等它的回报；脚本退出时没 wait 的回报会自动转交主对话，不丢。"""
    a = {"task": task}
    if title:
        a["title"] = title
    if model:
        a["model"] = model
    if permission:
        a["permission"] = permission
    return call("_dispatch", a, timeout=60)

def wait(session_id, timeout_sec=600):
    """等待子任务完成，返回 {"ok": true, "result": {"done": ..., "title": ...,
    "report": "<回报摘要>"}}；超时返回 done=false 的当前状态（可再次 wait）。"""
    return call("_wait", {"session_id": session_id, "timeout_sec": timeout_sec},
                timeout=timeout_sec + 30)

def sessions():
    """列出本脚本派发的全部子任务及状态。"""
    return call("_sessions", {}, timeout=30)
`

// bridgeSitecustomize 是写进便携运行时 site-packages 的启动钩子：运行时是
// ._pth 隔离形态（Python 3.14 + python314._pth：PYTHONPATH 与脚本目录都不进
// sys.path），而 ._pth 已解锁 import site + Lib/site-packages——sitecustomize
// 在解释器启动时自动执行，从 JXPY_SESSION_DIR 读会话脚本目录注进 sys.path。
// 每次桥装配幂等重写（运行时被更新机制整体替换后，下次 run_python 自动补上，
// 无需迁移逻辑）。
const bridgeSitecustomize = `# 坚祥数字助理 run_python 工具桥：把会话脚本目录注进 sys.path（._pth 隔离形态下
# PYTHONPATH 与脚本目录默认都不进 sys.path，import jx 会失败）。由 bridge.go 幂等维护。
import os, sys
_d = os.environ.get("JXPY_SESSION_DIR")
if _d and _d not in sys.path:
    sys.path.insert(0, _d)
`

// bridgePrepare 为一次 run_python 调用装配桥：生成 jx.py（注入端口与新 token）、
// 确保 sitecustomize 就位，返回归集 key 与收尾函数。收尾函数（bridgeRelease）
// 注销 token、把未取走的子任务回报转交常规投递——前台在 runChild 返回后调，
// 后台在作业收尾回调里调，都调且幂等。pyExe 是便携 Python 解释器路径。
func (a *App) bridgePrepare(t *toolContext, pyExe string) (string, func(), error) {
	dir := pySessionDir(t)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, fmt.Errorf("创建脚本目录失败: %w", err)
	}
	if pyExe != "" { // 运行时就绪才写（未就绪时 jx.py 也写——前台报错路径同样无害）
		sp := filepath.Join(filepath.Dir(pyExe), "Lib", "site-packages")
		if err := os.MkdirAll(sp, 0o755); err == nil {
			_ = os.WriteFile(filepath.Join(sp, "sitecustomize.py"), []byte(bridgeSitecustomize), 0o644)
		}
	}
	a.bridgeMu.Lock()
	a.bridgeState()
	tok := &bridgeToken{token: newID(), sessionID: t.sess.ID, key: "call:" + newID()}
	a.bridgeTokens[tok.token] = tok
	a.bridgeMu.Unlock()
	code := fmt.Sprintf(bridgeClientPy, a.listenPort, tok.token)
	if err := os.WriteFile(filepath.Join(dir, "jx.py"), []byte(code), 0o644); err != nil {
		a.bridgeDropToken(tok)
		return "", nil, fmt.Errorf("写入 jx.py 失败: %w", err)
	}
	log.Printf("[桥] 会话 %s 装配脚本桥 key=%s", t.sess.ID, tok.key)
	return tok.key, func() { a.bridgeRelease(tok.key) }, nil
}

// bridgeDropToken 只注销 token（不动收集条目）。
func (a *App) bridgeDropToken(tok *bridgeToken) {
	a.bridgeMu.Lock()
	delete(a.bridgeTokens, tok.token)
	a.bridgeMu.Unlock()
}

// bridgeRelease 是一次脚本运行的桥收尾：注销 token；未取走的子任务回报转交常规
// 投递（已完成的立即投，仍在跑的删除收集条目——它完成时拦截不命中，自然走
// runtime.go 的原生回报路径）。幂等：第二个调用者（前台返回 + 后台收尾重叠的
// 极端时序）只会遇到空表。
func (a *App) bridgeRelease(key string) {
	a.bridgeMu.Lock()
	a.bridgeState()
	var tok *bridgeToken
	for _, t := range a.bridgeTokens {
		if t.key == key {
			tok = t
			break
		}
	}
	if tok != nil {
		delete(a.bridgeTokens, tok.token)
	}
	var finished []*bridgeWait
	for sid, bw := range a.bridgeWaits {
		if bw.key != key {
			continue
		}
		delete(a.bridgeWaits, sid)
		if bw.done {
			finished = append(finished, bw)
		}
	}
	a.bridgeMu.Unlock()
	if tok == nil && len(finished) == 0 {
		return // 幂等：已释放过
	}
	if tok != nil {
		log.Printf("[桥] 释放 key=%s（调用 %d 次）", key, tok.calls)
	}
	// 已完成但脚本没 wait 的：回报不能丢，转交常规投递（父会话忙=插话，空闲=起轮）
	for _, bw := range finished {
		parent, err := a.Store.Get(bw.tok.sessionID)
		if err != nil {
			log.Printf("[桥] 转交回报失败：会话 %s 不存在", bw.tok.sessionID)
			continue
		}
		text := "【桥派子任务转交】脚本已退出，未 wait 的子任务「" + bw.title +
			"」（" + bw.subID + "）回报如下（下次可直接在脚本里 jx.wait 取用）：\n\n" + bw.report
		a.startReportRound(parent, text, "bridge")
	}
}

// startReportRound 把一段回报文本按「用户侧消息」投进会话并接上汇报轮
// （空闲=落库并起轮，忙=插话注入正在执行的那一轮——与派发回报同一套收口，
// 模板照 runtime.go）。reportPythonJob 与 bridgeRelease 转交共用。
func (a *App) startReportRound(m *SessionMeta, text, origin string) {
	d := a.deliverToSession(m, text, kindAiUser, origin)
	if !d.Started {
		return // 插话模式：对方正在跑的一轮会带上它，无需起轮
	}
	go func() {
		prun := d.Run
		defer a.finishRunOf(m.ID, prun)
		prun.publish(Event{Type: "run_start"})
		err := a.runRoundsWithQueue(m, prun, func(ev Event) {
			ev.SessionID = m.ID
			prun.observe(ev)
			prun.publish(ev)
		}, true, a.queueAutoSendOn())
		if err == nil {
			prun.markCleanEnd()
		}
		a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
		a.broadcast(Event{Type: "done", SessionID: m.ID})
	}()
}

// bridgeInterceptReport 在 runtime.go 的派发回报投递点调用：命中收集表 = 该子任务
// 是桥派的，回报定向存给脚本 wait，不再向父会话投递、不起汇报轮。
func (a *App) bridgeInterceptReport(sub *SessionMeta, reportText string) bool {
	a.bridgeMu.Lock()
	a.bridgeState()
	bw, ok := a.bridgeWaits[sub.ID]
	if !ok {
		a.bridgeMu.Unlock()
		return false
	}
	if bw.done { // 防重：同一子任务只归集一次（重复回报只留首份）
		a.bridgeMu.Unlock()
		return true
	}
	bw.report = reportText
	bw.done = true
	if bw.tok != nil && bw.tok.dispAlive > 0 {
		bw.tok.dispAlive--
	}
	close(bw.ready)
	a.bridgeMu.Unlock()
	log.Printf("[桥] 子任务 %s「%s」回报已归集（key=%s，等待脚本 wait）", sub.ID, sub.Title, bw.key)
	return true
}

// ---------- HTTP 端点 ----------

// handleBridgeTool 桥端点：仅本机回环；伪工具（_dispatch/_wait/_sessions）与
// 白名单真工具统一从这里进。
func (a *App) handleBridgeTool(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}
	body := http.MaxBytesReader(w, r.Body, bridgeBodyMax)
	var req struct {
		Token string         `json:"token"`
		Tool  string         `json:"tool"`
		Args  map[string]any `json:"args"`
	}
	if err := json.NewDecoder(body).Decode(&req); err != nil {
		http.Error(w, "请求体不是合法 JSON: "+err.Error(), http.StatusBadRequest)
		return
	}
	a.bridgeMu.Lock()
	a.bridgeState()
	tok := a.bridgeTokens[strings.TrimSpace(req.Token)]
	if tok == nil {
		a.bridgeMu.Unlock()
		http.Error(w, "token 无效或已过期（仅本次脚本运行期有效）", http.StatusForbidden)
		return
	}
	if tok.calls >= bridgeCallMax {
		a.bridgeMu.Unlock()
		writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false,
			"error": fmt.Sprintf("桥调用总量已达上限 %d 次（防失控）：把余下工作交给子任务或结束脚本", bridgeCallMax)})
		return
	}
	tool := strings.TrimSpace(req.Tool)
	pseudo := strings.HasPrefix(tool, "_")
	if !pseudo { // 真工具占并发槽；伪工具（wait 会阻塞）不占
		if tok.inFlight >= bridgeFlightMax {
			a.bridgeMu.Unlock()
			writeJSON(w, http.StatusTooManyRequests, map[string]any{"ok": false,
				"error": fmt.Sprintf("并发桥调用已达上限 %d 个：先等前面的调用返回", bridgeFlightMax)})
			return
		}
		tok.inFlight++
	}
	tok.calls++
	a.bridgeMu.Unlock()
	if !pseudo {
		defer func() {
			a.bridgeMu.Lock()
			tok.inFlight--
			a.bridgeMu.Unlock()
		}()
	}

	m, err := a.Store.Get(tok.sessionID)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]any{"ok": false, "error": "会话不存在"})
		return
	}
	switch tool {
	case "_dispatch":
		writeJSON(w, http.StatusOK, a.bridgeDispatch(r, m, tok, req.Args))
	case "_wait":
		writeJSON(w, http.StatusOK, a.bridgeWait(r, req.Args))
	case "_sessions":
		writeJSON(w, http.StatusOK, a.bridgeSessions(tok.key))
	case "":
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "error": "tool 不能为空"})
	default:
		writeJSON(w, http.StatusOK, a.bridgeRunTool(r, m, tok, tool, req.Args))
	}
}

// bridgeRunTool 以会话身份执行一个白名单真工具。
func (a *App) bridgeRunTool(r *http.Request, m *SessionMeta, tok *bridgeToken, tool string, args map[string]any) map[string]any {
	if !bridgeAllow[tool] {
		return map[string]any{"ok": false, "error": "工具 " + tool + " 不在桥白名单内。可用：" +
			"read/glob/grep/list_dir/write/edit/snapshot_file/run_command/web_fetch/web_search/read_skill" +
			"；派子任务用 jx.dispatch。需要审批、浏览器/桌面/连接器类的操作不在桥上——回到模型前台调。"}
	}
	// 剥审批与嵌套后台参数：桥上没有交互上下文，绝不允许弹卡或起嵌套作业
	for _, k := range []string{"sandbox_permissions", "justification", "run_in_background", "visible"} {
		delete(args, k)
	}
	argsJSON, err := json.Marshal(args)
	if err != nil {
		return map[string]any{"ok": false, "error": "参数序列化失败: " + err.Error()}
	}
	var run *Tool
	for _, tl := range a.toolsFor(m.Mode) {
		if tl.Spec.Function.Name == tool {
			t := tl
			run = &t
			break
		}
	}
	if run == nil {
		return map[string]any{"ok": false, "error": "工具 " + tool + " 不在当前会话（模式 " + m.Mode + "）的工具目录里"}
	}
	tc := &toolContext{ctx: r.Context(), app: a, sess: m, toolName: tool, callID: "bridge", emit: func(Event) {}}
	started := time.Now()
	out, err := run.Run(tc, argsJSON)
	dur := time.Since(started).Round(time.Millisecond)
	if err != nil {
		log.Printf("[桥] 会话 %s 调 %s 失败（%v，%s）", m.ID, tool, err, dur)
		return map[string]any{"ok": false, "error": err.Error()}
	}
	log.Printf("[桥] 会话 %s 调 %s 完成（%s）", m.ID, tool, dur)
	return map[string]any{"ok": true, "result": out}
}

// bridgeDispatch 派发子任务：复用 dispatch_task 的会话创建与投递管线，差别只在
// 回报路由——登记进收集表，由 jx.wait 取走（见 bridgeInterceptReport）。
func (a *App) bridgeDispatch(r *http.Request, m *SessionMeta, tok *bridgeToken, args map[string]any) map[string]any {
	str := func(k string) string { s, _ := args[k].(string); return strings.TrimSpace(s) }
	task := str("task")
	if task == "" {
		return map[string]any{"ok": false, "error": "task 不能为空（自足的任务描述：子任务看不到本对话任何记录）"}
	}
	if m.ParentID != "" {
		return map[string]any{"ok": false, "error": "子对话不能再派发子对话（一级限制）；把需求直接做掉"}
	}
	a.bridgeMu.Lock()
	if tok.dispAlive >= bridgeDispMax {
		a.bridgeMu.Unlock()
		return map[string]any{"ok": false, "error": fmt.Sprintf("在跑的桥派子任务已达上限 %d 个：先 jx.wait 收几个", bridgeDispMax)}
	}
	tok.dispAlive++
	a.bridgeMu.Unlock()

	tc := &toolContext{ctx: r.Context(), app: a, sess: m, toolName: "run_python(bridge)", emit: func(Event) {}}
	perm, err := tc.dispatchPermission(str("permission"))
	if err != nil {
		a.bridgeMu.Lock()
		tok.dispAlive--
		a.bridgeMu.Unlock()
		return map[string]any{"ok": false, "error": err.Error()}
	}
	provider, mdl, err := tc.dispatchModelChoice(str("model"))
	if err != nil {
		a.bridgeMu.Lock()
		tok.dispAlive--
		a.bridgeMu.Unlock()
		return map[string]any{"ok": false, "error": err.Error()}
	}
	title := str("title")
	if title == "" {
		title = truncateRunes(task, 20)
	}
	params := createSessionParams{
		Mode: "task", Title: title, Workspace: m.Workspace,
		Permission: perm, DispatchedBy: m.ID,
		Provider: provider, Model: mdl,
	}
	if m.Remote != nil {
		rr := *m.Remote
		params.Remote = &rr // 与 dispatch_task 同款继承：桥派子任务默认同一远程环境
	}
	sub, err := a.createSession(params)
	if err != nil {
		a.bridgeMu.Lock()
		tok.dispAlive--
		a.bridgeMu.Unlock()
		return map[string]any{"ok": false, "error": err.Error()}
	}
	// 先登记收集表再投递（防秒完竞态：回报先于登记到达就漏拦截了）
	a.bridgeMu.Lock()
	a.bridgeWaits[sub.ID] = &bridgeWait{subID: sub.ID, title: sub.Title, key: tok.key,
		tok: tok, ready: make(chan struct{}), started: time.Now()}
	a.bridgeMu.Unlock()

	d := a.deliverToSession(sub, "【数字助理派发】\n"+task, kindAiUser, "task")
	if !d.Started {
		// 新建会话必空闲；理论竞态兜底——插话路径下子任务也会跑起来，回报照常归集
		return map[string]any{"ok": true, "result": map[string]any{
			"session_id": sub.ID, "title": sub.Title, "note": "已投递（插话模式）"}}
	}
	go a.runSubAgent(m, sub, d.Run)
	log.Printf("[桥] 会话 %s 派子任务 %s「%s」（key=%s）", m.ID, sub.ID, sub.Title, tok.key)
	return map[string]any{"ok": true, "result": map[string]any{"session_id": sub.ID, "title": sub.Title}}
}

// bridgeWait 等一个桥派子任务的回报（阻塞 HTTP 请求直至超时；不占并发槽）。
func (a *App) bridgeWait(r *http.Request, args map[string]any) map[string]any {
	sid, _ := args["session_id"].(string)
	sid = strings.TrimSpace(sid)
	if sid == "" {
		return map[string]any{"ok": false, "error": "session_id 不能为空"}
	}
	timeout := 600
	if v, ok := args["timeout_sec"].(float64); ok && v > 0 {
		timeout = int(min(v, 900))
	}
	a.bridgeMu.Lock()
	bw := a.bridgeWaits[sid]
	a.bridgeMu.Unlock()
	if bw == nil {
		return map[string]any{"ok": false, "error": "子任务 " + sid + " 不是本脚本派发的（或已转交主对话）"}
	}
	if !bw.done {
		timer := time.NewTimer(time.Duration(timeout) * time.Second)
		defer timer.Stop()
		select {
		case <-bw.ready:
		case <-timer.C:
		case <-r.Context().Done():
		}
	}
	a.bridgeMu.Lock()
	done, title, report := bw.done, bw.title, bw.report
	key := bw.key
	if done {
		delete(a.bridgeWaits, sid) // 取走即清：重复 wait 同一会话返回错误（防脚本误解语义）
	}
	a.bridgeMu.Unlock()
	if !done {
		return map[string]any{"ok": true, "result": map[string]any{"done": false,
			"session_id": sid, "title": title, "note": "仍在运行（key=" + key + "），可再次 wait"}}
	}
	return map[string]any{"ok": true, "result": map[string]any{"done": true,
		"session_id": sid, "title": title, "report": truncateRunes(report, bridgeReportMax*2)}}
}

// bridgeSessions 列出本脚本派发的全部子任务状态。
func (a *App) bridgeSessions(key string) map[string]any {
	a.bridgeMu.Lock()
	defer a.bridgeMu.Unlock()
	out := []map[string]any{}
	for _, bw := range a.bridgeWaits {
		if bw.key != key {
			continue
		}
		out = append(out, map[string]any{"session_id": bw.subID, "title": bw.title,
			"done": bw.done, "started": bw.started.Format("15:04:05")})
	}
	return map[string]any{"ok": true, "result": out}
}

// writeJSON 统一 JSON 回包（server.go 已有同名函数，这里不再重复定义——
// bridge.go 全部直接用现有的 writeJSON）。
