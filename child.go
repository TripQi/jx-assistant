package main

// 子进程统一入口：引擎里所有「拉起一个子进程」的动作都经这里，调用点只声明
// 「跑什么、在哪跑、超时多少」，压窗/沙箱分派/环境白名单/超时/输出捕获与截断/
// 解码/沙箱拒绝 marker/「运行中的程序」登记全在这一层做掉。
//
// 为什么要收这一层：这些套路以前散在每个调用点各写一遍（run_command、后台作业、
// 技能脚本、python 探测、环境探测、git、打开文件…），漏一处就漏一种行为差异
// ——最直观的就是桌面形态（-windowsgui，自己没有控制台可复用）下，桌面上一个
// 个往外冒控制台窗口。
//
// 两个入口，按「谁跑」分：
//   - `a.runChild(ctx, sandboxRun)` / `a.startChild(...)`：会话里的命令（有会话、
//     有权限档位、要进沙箱、要登记进进度面板）。
//   - `runToolChild(...)`：产品自己拉起的小工具（技能运行时装配、python 探测），
//     不进沙箱、不登记，只要压窗 + 超时 + 捕获。
//
// 窗口纪律（默认后台跑，只有显式要求才给窗口）：
//   - Visible=false（默认）：压掉控制台窗口（CREATE_NO_WINDOW）；
//   - Visible=true：什么都不加——用户/AI 明确要看见这个窗口（run_command 的
//     `visible:true`）。
//   - 「本来就要窗口」的子进程（有头浏览器）走各自启动路径，绝不要顺手套压窗
//     标记：标记里的 HideWindow 会把 GUI 程序的窗口一起藏掉。
//   - 受控例外：`gitpanel.go` 的 gitRun 要 stdout/stderr 分离 + 专属上限 +
//     完整环境，保留自己的 argv 形态；压窗仍走同一个 hideConsoleWindow。

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ---------- 「运行中的程序」登记表（进度面板的数据源） ----------

const childKeep = 30 * time.Second // 已结束的登记项保留多久（让一闪而过的短命令也来得及显示）

// 进程内唯一的子进程登记表：一个进程一个 App，所以不挂在 App 字段上（这样
// server.go 的 App 结构一行都不用动）。要挪到 App 上时把这两行换成字段即可。
var (
	childRegMu sync.Mutex
	childReg   = map[string]*childRun{}
)

// childRun 是登记表里的一条子进程记录。
type childRun struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	SessionID string    `json:"sessionId"`
	Command   string    `json:"command"`
	Dir       string    `json:"dir,omitempty"`
	Policy    string    `json:"policy,omitempty"`
	Visible   bool      `json:"visible,omitempty"`
	PID       int       `json:"pid,omitempty"`
	StartedAt time.Time `json:"startedAt"`

	mu   sync.Mutex
	exit string // 空 = 还在跑；否则 "exit 0" / "超时" / "被终止"…
}

// Exit 取退出文案（空 = 还在跑）。
func (c *childRun) Exit() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.exit
}

func (c *childRun) setExit(info string) {
	c.mu.Lock()
	if c.exit == "" {
		c.exit = info
	}
	c.mu.Unlock()
}

// childJSON 给前端的形态：附带已运行秒数（前端不必自己算时钟差）。
func (c *childRun) childJSON() map[string]any {
	item := map[string]any{
		"id": c.ID, "label": c.Label, "command": c.Command, "dir": c.Dir,
		"policy": c.Policy, "visible": c.Visible, "pid": c.PID,
		"startedAt": c.StartedAt.Format("15:04:05"),
		"elapsed":   int(time.Since(c.StartedAt).Seconds()),
		"state":     "running",
	}
	if e := c.Exit(); e != "" {
		item["state"] = "exited"
		item["exit"] = e
	}
	return item
}

func (a *App) registerChild(r sandboxRun, pid int) *childRun {
	c := &childRun{
		ID: "child-" + newID(), Label: r.Label, Command: r.childCommandLine(),
		Dir: r.Dir, Policy: r.Policy, Visible: r.Visible, PID: pid, StartedAt: time.Now(),
	}
	if r.Remote != nil {
		c.SessionID = r.Remote.ID
	}
	childRegMu.Lock()
	for id, old := range childReg { // 顺手清掉过期的已结束项
		if old.Exit() != "" && time.Since(old.StartedAt) > childKeep {
			delete(childReg, id)
		}
	}
	childReg[c.ID] = c
	childRegMu.Unlock()
	if c.SessionID != "" {
		a.broadcast(Event{Type: "children", SessionID: c.SessionID}) // 前端据此刷进度面板
	}
	return c
}

func (a *App) finishChild(c *childRun, info string) {
	if c == nil {
		return
	}
	c.setExit(info)
	if c.SessionID != "" {
		a.broadcast(Event{Type: "children", SessionID: c.SessionID})
	}
}

// runningChildrenOf 取某会话的子进程（含刚结束 30s 内的，便于看见一闪而过的命令）。
// sessionID 为空时返回全部。
func runningChildrenOf(a *App, sessionID string) []map[string]any {
	childRegMu.Lock()
	list := make([]*childRun, 0, len(childReg))
	for _, c := range childReg {
		if sessionID != "" && c.SessionID != sessionID {
			continue
		}
		list = append(list, c)
	}
	childRegMu.Unlock()
	out := []map[string]any{}
	for _, c := range list {
		if c.Exit() != "" && time.Since(c.StartedAt) > childKeep {
			continue
		}
		out = append(out, c.childJSON())
	}
	return out
}

// childCommandLine 是登记表里展示的命令行（argv 形态拼回一行，便于阅读）。
func (r sandboxRun) childCommandLine() string {
	if r.Argv != nil {
		return strings.Join(r.Argv, " ")
	}
	return r.Command
}

// childDefaults 补齐缺省：工作目录、沙箱档位、写授权目录、shell、标签。
// 会话命令只声明「跑什么、在哪跑、超时多少」，其余从这里补。
func (a *App) childDefaults(r sandboxRun) sandboxRun {
	if r.Remote != nil {
		if r.Workspace == "" {
			r.Workspace = a.workspaceOf(r.Remote)
		}
		if r.Policy == "" {
			r.Policy = permOf(r.Remote)
		}
	}
	if r.Policy == "" {
		r.Policy = permWorkspace
	}
	if r.Workspace == "" {
		r.Workspace = a.Cfg.Workspace
	}
	if r.Dir == "" {
		r.Dir = r.Workspace
	}
	if r.Shell == "" && r.Argv == nil {
		if isWindowsRuntime {
			r.Shell = "powershell"
		} else {
			r.Shell = "sh"
		}
	}
	if r.Label == "" {
		r.Label = "child"
	}
	return r
}

// workspaceOf 会话的工作区根目录（远程会话 = 远程目录）；口径与
// toolContext.workspace 一致，供没有工具上下文的调用点用。
func (a *App) workspaceOf(sess *SessionMeta) string {
	if sess == nil {
		return a.Cfg.Workspace
	}
	if sess.Remote != nil {
		return sess.Remote.Dir
	}
	if sess.Workspace != "" {
		return sess.Workspace
	}
	return a.Cfg.Workspace
}

// ---------- 会话命令入口 ----------

// runChild 同步执行一个会话命令并返回输出（等待结束、捕获、截断、解码、压窗、
// 沙箱拒绝 marker、登记都在这一层）。Remote 非空 = 该会话是 SSH 远程会话。
func (a *App) runChild(ctx context.Context, r sandboxRun) (string, error) {
	r = a.childDefaults(r)
	entry := a.registerChild(r, 0)
	if r.Argv != nil {
		out, err := runArgv(ctx, r)
		a.finishChild(entry, childExitInfo(err, ctx))
		return out, err
	}
	// 远程会话：远端用它的本地后端执行策略（与前台同路）
	if r.Remote != nil && r.Remote.Remote != nil {
		rc, err := a.getRemoteClient(r.Remote)
		if err != nil {
			a.finishChild(entry, "失败")
			return "", fmt.Errorf("sandbox unavailable: %w", err)
		}
		display := r.Remote.Remote.Display()
		if r.Policy == permFull {
			out, rerr := rc.run(ctx, r.Dir, r.Command, r.Timeout)
			a.finishChild(entry, childExitInfo(rerr, ctx))
			if rerr != nil {
				return "", rerr
			}
			return out + "\n[远程执行于 " + display + "｜不受限]", nil
		}
		out, rerr := a.runRemoteConfined(ctx, rc, r)
		a.finishChild(entry, childExitInfo(rerr, ctx))
		if rerr != nil {
			return "", rerr
		}
		out += "\n[远程执行于 " + display + "｜沙箱 " + r.Policy + "]"
		if r.Model {
			out += runDeniedMarker(r.Policy, out, true)
		}
		return out, nil
	}
	if r.Policy == permFull {
		out, err := runUnconfinedLocal(ctx, r) // 内部已解码/截断/附超时注记
		a.finishChild(entry, childExitInfo(err, ctx))
		return out, err
	}
	out, err := spawnConfinedLocal(ctx, r)
	a.finishChild(entry, childExitInfo(err, ctx))
	if err != nil {
		return "", err
	}
	if r.Model {
		out += runDeniedMarker(r.Policy, out, false)
	}
	return out, nil
}

// runArgv 是 argv 形态的本地执行（不过 shell）：git、打开文件这类用它。
func runArgv(ctx context.Context, r sandboxRun) (string, error) {
	if len(r.Argv) == 0 {
		return "", fmt.Errorf("childSpec 既没有 Command 也没有 Argv")
	}
	if info, err := os.Stat(r.Dir); err != nil || !info.IsDir() {
		return "", fmt.Errorf("工作目录不存在: %s", r.Dir)
	}
	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, r.Argv[0], r.Argv[1:]...)
	cmd.Dir = r.Dir
	cmd.Env = sandboxEnv(true)
	if !r.Visible {
		hideConsoleWindow(cmd) // 默认压窗（GUI 程序要看得见就设 Visible=true）
	}
	tree := armTreeKill(cmd)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if serr := cmd.Start(); serr != nil {
		tree.finish()
		return "", serr
	}
	tree.attach(cmd)
	err := cmd.Wait()
	tree.finish()
	out := buf.Bytes()
	note := ""
	if ctx.Err() == context.DeadlineExceeded {
		note = "[命令超时被终止]"
	}
	return strings.TrimRight(decodeCmdOutput(out), "\r\n") + note, err
}

// startChild 异步启动一个会话命令（后台作业）：输出增量写入 w，返回等待函数。
// 与同步路径共用沙箱分派与登记（进度面板能看见它一直在跑）。
func (a *App) startChild(ctx context.Context, r sandboxRun, w io.Writer) (func() bool, error) {
	r = a.childDefaults(r)
	if r.Argv != nil {
		r.Timeout = 0 // 异步：不限时，终止只经 ctx
		cmd, tree := argvCmd(ctx, r, w)
		if err := cmd.Start(); err != nil {
			tree.finish()
			return nil, err
		}
		tree.attach(cmd)
		entry := a.registerChild(r, cmd.Process.Pid)
		done := make(chan struct{}, 1)
		go func() {
			err := cmd.Wait()
			tree.finish() // 作业结束：job 句柄关闭，残留孙进程清场
			entry.setExit(childExitInfo(err, ctx))
			close(done)
		}()
		return func() bool { <-done; return true }, nil
	}
	if r.Policy == permFull {
		cmd, tree := shellCmd(ctx, r, w)
		if err := cmd.Start(); err != nil {
			tree.finish()
			return nil, err
		}
		tree.attach(cmd)
		entry := a.registerChild(r, cmd.Process.Pid)
		done := make(chan struct{}, 1)
		go func() {
			err := cmd.Wait()
			tree.finish() // 同上：整树纪律对后台 shell 作业一致
			entry.setExit(childExitInfo(err, ctx))
			close(done)
		}()
		return func() bool { <-done; return true }, nil
	}
	wait, err := startConfinedAsync(ctx, r, w)
	if err != nil {
		return nil, err
	}
	entry := a.registerChild(r, 0)
	go func() {
		wait()
		entry.setExit(childExitInfo(nil, ctx))
	}()
	return wait, nil
}

// shellCmd 起一个「输出进 w」的 shell 子进程（不受限档位的异步路径）。
// 返回的 procTree 由调用方在 Start 成功后 attach、Wait 返回后 finish。
// PS 5.1（含空 shell 缺省）前置编码设置并注入环境变量，同 runUnconfinedLocal。
func shellCmd(ctx context.Context, r sandboxRun, w io.Writer) (*exec.Cmd, *procTree) {
	var cmd *exec.Cmd
	ps51 := false
	if strings.ToLower(strings.TrimSpace(r.Shell)) == "cmd" {
		cmd = exec.CommandContext(ctx, "cmd", "/C", r.Command)
	} else {
		command := r.Command
		if strings.ToLower(strings.TrimSpace(r.Shell)) != "pwsh" {
			ps51 = true
			command = ps51Prologue(r.EncodingMode) + command
		}
		cmd = exec.CommandContext(ctx, "powershell", "-NoProfile", "-NonInteractive", "-Command", command)
	}
	cmd.Dir = r.Dir
	cmd.Env = sandboxEnv(true)
	if ps51 {
		cmd.Env = appendEnvUnique(cmd.Env, ps51Utf8Env()...)
	}
	if !r.Visible {
		hideConsoleWindow(cmd)
	}
	cmd.Stdout, cmd.Stderr = w, w
	return cmd, armTreeKill(cmd)
}

func argvCmd(ctx context.Context, r sandboxRun, w io.Writer) (*exec.Cmd, *procTree) {
	cmd := exec.CommandContext(ctx, r.Argv[0], r.Argv[1:]...)
	cmd.Dir = r.Dir
	cmd.Env = sandboxEnv(true)
	if !r.Visible {
		hideConsoleWindow(cmd)
	}
	cmd.Stdout, cmd.Stderr = w, w
	return cmd, armTreeKill(cmd)
}

// ---------- 产品内部小工具入口 ----------

// runToolChild 是产品自己拉起的小工具子进程的统一入口（技能运行时装配、python
// 模块探测这类）：不属于会话命令、不进沙箱、不登记，统一做压窗 + 超时 + 捕获 + 解码。
// timeout<=0 表示不限时（由 ctx 管）。
func runToolChild(ctx context.Context, exe string, args []string, dir string, timeout time.Duration) (string, error) {
	return runToolChildEnv(ctx, exe, args, dir, timeout, nil)
}

// runToolChildEnv 是 runToolChild 的带环境版：env 非空时替换默认继承环境
// （安卓内置运行时的 python3/pip 必须带 Termux 前缀环境——LD_LIBRARY_PATH
// 指向 prefix/lib，缺了动态链接器找不到 libpython）。
func runToolChildEnv(ctx context.Context, exe string, args []string, dir string, timeout time.Duration, env []string) (string, error) {
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, exe, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if env != nil {
		cmd.Env = env
	}
	hideConsoleWindow(cmd) // 内部小工具同样悄悄跑
	tree := armTreeKill(cmd)
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	if serr := cmd.Start(); serr != nil {
		tree.finish()
		return "", serr
	}
	tree.attach(cmd)
	err := cmd.Wait()
	tree.finish()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("超时（%v）", timeout)
	}
	return strings.TrimRight(decodeCmdOutput(buf.Bytes()), "\r\n"), err
}

// childExitInfo 把 Wait 的结果翻译成登记表里的退出文案。
func childExitInfo(err error, ctx context.Context) string {
	if ctx != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "超时"
		}
		if ctx.Err() == context.Canceled {
			return "被终止"
		}
	}
	if errors.Is(err, exec.ErrWaitDelay) {
		return "已收口（残留后台子进程一并终止）"
	}
	if err != nil {
		return fmt.Sprintf("exit %v", err)
	}
	return "exit 0"
}

// killSessionChildren 强制终止该会话登记在册、仍在运行的子进程（停止看门狗
// 硬终结的补刀，见 runtime.go 的 forceStopRun）：正常路径这些进程挂在
// run.ctx / 后台作业的 ctx 上，取消即死（CommandContext Kill + Job Object
// 整树终止 + WaitDelay 收口）；这里按 PID 补刀，兜的是「ctx 已取消、进程却
// 还活着」的极端卡死。没登记 PID 的（同步前台命令、沙箱 helper）没有补刀
// 句柄，仍靠那三层取消机制收场。返回补刀的进程数。
func killSessionChildren(id string) int {
	childRegMu.Lock()
	mine := []*childRun{}
	for _, c := range childReg {
		if c.SessionID == id && c.Exit() == "" && c.PID > 0 {
			mine = append(mine, c)
		}
	}
	childRegMu.Unlock()
	for _, c := range mine {
		killChildTree(c.PID)
		c.setExit("被硬终结（停止兜底）")
	}
	return len(mine)
}
