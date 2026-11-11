package main

// 沙箱统一入口：按会话策略档位（Grant.Policy）把命令送进对应的
// OS 级沙箱执行。移植自 DSH 官方的分层：
//
//	local Windows → 受限令牌 + ACL（sandbox_windows.go，对齐 sandbox-windows-acl）
//	local Linux   → Landlock（经 sandboxhelper 自限制后 exec，对齐 sandbox-local）
//	SSH remote    → 上传同一 helper 到远端执行（对齐 sandbox-ssh：
//	                「远端主机选择它安装的本地后端来执行策略」）
//	policy = full → 不受限（对齐 danger-full-access：消费者直接绕过 confine）
//
// 拒绝驱动的升级（对齐官方 bash 工具）：confined 执行的输出里检测到
// 访问被拒的标志后，附 [sandbox: …] 标记与升级提示；模型同轮带
// sandbox_permissions + justification 重试一次，由审批卡决定。

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"time"

	"jxassistant/sandboxhelper"
)

// sandboxRun 是一次沙箱内执行的全部输入。
type sandboxRun struct {
	Dir       string // 子进程工作目录（cwd 本身不是边界）
	Command   string // 完整命令
	Shell     string // powershell | cmd（本地 Windows 用；远端忽略）
	Timeout   time.Duration
	Policy    string       // read-only | workspace-write（full 不进沙箱）
	Workspace string       // 写授权目录（工作区）
	Remote    *SessionMeta // 非nil = SSH 远程执行（取 Remote 连接信息）

	// —— 「子进程统一入口」的声明面（见 child.go）：工具/技能/探测只声明跑什么、
	// 在哪跑、超时多少；压窗、沙箱分派、环境白名单、捕获与截断、运行中登记全在
	// 执行层一处做掉（以前散在各调用点，漏一处就漏一种行为差异）。
	Visible bool     // true = 用户要看得见这个窗口（不压窗）；默认 false = 压窗、后台悄悄跑
	Label   string   // 用途标签（进度面板/审计显示）：run_command｜skill:pdf｜py-probe…
	Argv    []string // argv 形态：直接 exec 不过 shell（打开文件/起浏览器这类），与 Command 二选一
	Model   bool     // true = 输出回给模型（附沙箱拒绝/升级 marker）

	// EncodingMode 决定 PS 5.1 路径的命令前置哪种编码（encoding.go）：
	// "utf8"（空值同义，默认）读面按 UTF-8；"gbk" 读面按 ANSI(936)——
	// 命令涉及 GBK 老文件（.bat/.cmd/金蝶导出 CSV）时由 run_command 探测决定。
	// 只影响 PS 5.1；pwsh/cmd/Linux/远程一律忽略。
	EncodingMode string
}

// spawnConfinedLocal 本地沙箱分派：Windows 受限令牌 / Linux Landlock。
func spawnConfinedLocal(ctx context.Context, r sandboxRun) (string, error) {
	// 工作区/命令目录失效时给出明确指引：目录被改名或移动后路径悬空，
	// 此前会以裸错误码失败，所有工具调用表现为「沙箱初始化失败」。
	if fi, err := os.Stat(r.Workspace); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("sandbox unavailable: 工作区目录不存在: %s（可能被改名或移动，请在会话设置里重新选择工作目录）", r.Workspace)
	}
	if fi, err := os.Stat(r.Dir); err != nil || !fi.IsDir() {
		return "", fmt.Errorf("sandbox unavailable: 命令工作目录不存在: %s（可能被改名或移动）", r.Dir)
	}
	switch {
	case isWindowsRuntime:
		wsAbs, err := filepath.Abs(r.Workspace)
		if err != nil {
			return "", fmt.Errorf("sandbox unavailable: %w", err)
		}
		r.Workspace = wsAbs
		return spawnConfinedWindows(ctx, r)
	case isLinuxRuntime:
		return spawnConfinedLinuxReexec(r)
	case isAndroidRuntime:
		// 安卓：内核禁用 Landlock 与非特权用户命名空间（android 内核
		// CONFIG 均未启用），OS 级命令沙箱不可用。对齐 DSH 手机版社区
		// 方案：命令直接执行，隔离由安卓应用沙箱（app uid + SELinux）
		// 承担；文件边界与审批仍由引擎上层把守。
		out, err := runUnconfinedLocal(ctx, r)
		if err != nil {
			return "", err
		}
		return out + "\n[安卓设备直接执行｜无 OS 级命令沙箱，隔离由安卓应用沙箱承担]", nil
	default:
		return "", fmt.Errorf("sandbox unavailable: 当前平台暂无沙箱后端；如确需执行，可带 sandbox_permissions=danger-full-access 与 justification 申请升级（将弹审批）")
	}
}

// startConfinedAsync 在沙箱内启动一个后台作业风格的进程：输出增量写入 w，
// ctx 取消时终止进程。返回等待函数（wait 返回即进程已死透）与退出文案函数
// （"exit 3"；取不到码时为空串，调用方按 ctx 状态自己定性）。
func startConfinedAsync(ctx context.Context, r sandboxRun, w io.Writer) (func() bool, func() string, error) {
	switch {
	case isWindowsRuntime:
		wsAbs, err := filepath.Abs(r.Workspace)
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox unavailable: %w", err)
		}
		r.Workspace = wsAbs
		child, err := startConfinedWindows(r)
		if err != nil {
			return nil, nil, err
		}
		go child.drain(w)
		waitDone := make(chan struct{}, 1)
		go func() {
			child.wait(ctx, 0) // 0 = 不限时；终止只经 ctx（job_kill）
			child.cleanup()
			close(waitDone)
		}()
		wait := func() bool { <-waitDone; return true }
		return wait, child.exitText, nil
	case isLinuxRuntime:
		exe, err := os.Executable()
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox unavailable: %w", err)
		}
		tempDir, err := os.MkdirTemp("", "jx-sbx-")
		if err != nil {
			return nil, nil, fmt.Errorf("sandbox unavailable: 创建私有临时目录失败: %w", err)
		}
		wrapped := "cd " + shellQuote(r.Dir) + " && { " + r.Command + " ;}"
		args := []string{"__jx-sandbox-restrict", "restrict", "--mode", r.Policy,
			"--workspace", r.Workspace, "--temp", tempDir, "--", "/bin/sh", "-c", wrapped}
		cmd := exec.CommandContext(ctx, exe, args...)
		cmd.Dir = r.Dir
		cmd.Env = sandboxEnv(false)
		cmd.Stdout = w
		cmd.Stderr = w
		tree := armTreeKill(cmd)
		if err := cmd.Start(); err != nil {
			tree.finish()
			os.RemoveAll(tempDir)
			return nil, nil, fmt.Errorf("sandbox unavailable: %w", err)
		}
		tree.attach(cmd)
		waitDone := make(chan struct{}, 1)
		go func() { _ = cmd.Wait(); tree.finish(); os.RemoveAll(tempDir); close(waitDone) }()
		return func() bool { <-waitDone; return true }, processExitText(cmd), nil
	case isAndroidRuntime:
		// 安卓后台作业：同前台，无 OS 级沙箱；内置运行时优先、未就绪回落系统 sh
		// （见 spawnConfinedLocal 的安卓分支注释）。
		wrapped := "cd " + shellQuote(r.Dir) + " && { " + r.Command + " ;}"
		var cmd *exec.Cmd
		if prefix := termuxPrefixOrEmpty(); prefix != "" {
			cmd = exec.CommandContext(ctx, prefix+"/bin/bash", "-lc", wrapped)
			cmd.Dir = r.Dir
			cmd.Env = termuxEnv(prefix)
			applyTermuxProcAttr(cmd)
		} else {
			cmd = exec.CommandContext(ctx, "sh", "-c", wrapped)
			cmd.Dir = r.Dir
			cmd.Env = sandboxEnv(true)
		}
		cmd.Stdout = w
		cmd.Stderr = w
		tree := armTreeKill(cmd)
		if err := cmd.Start(); err != nil {
			tree.finish()
			return nil, nil, fmt.Errorf("sandbox unavailable: %w", err)
		}
		tree.attach(cmd)
		waitDone := make(chan struct{}, 1)
		go func() { _ = cmd.Wait(); tree.finish(); close(waitDone) }()
		return func() bool { <-waitDone; return true }, processExitText(cmd), nil
	default:
		return nil, nil, fmt.Errorf("sandbox unavailable: 当前平台暂无沙箱后端")
	}
}

// processExitText 取 exec.Cmd 的退出文案（"exit 3"）；未结束或取不到时为空串。
func processExitText(cmd *exec.Cmd) func() string {
	return func() string {
		if cmd.ProcessState == nil {
			return ""
		}
		return fmt.Sprintf("exit %d", cmd.ProcessState.ExitCode())
	}
}

// winDenialSigs Windows 受限令牌/ACL 沙箱的拒绝方言（对齐官方 windows-acl
// DENIAL_SIGNATURES 三条，附中文本地化等价——本机 shell 输出是中文；「所需的
// 特权」覆盖受限令牌剥特权导致的失败如 mklink，同属沙箱所致）。刻意不收
// EPERM/EACCES/EROF 等泛 Unix 字样——非沙箱原因的普通程序错误被标成
// 「策略拒绝」会把模型引向申请升级、跟审批打架（官方同款收窄理由）。
var winDenialSigs = []string{
	"access is denied", "access to the path", "permission denied",
	"拒绝访问", "对路径", "所需的特权",
}

// posixDenialSigs Linux Landlock / SSH 远端沙箱的拒绝方言（对齐官方
// landlock + runnerCommand 列表，附中文等价）。
var posixDenialSigs = []string{
	"permission denied", "read-only file system",
	"权限不够", "只读文件系统",
}

// runnerFailureSigs 沙箱执行器自身的故障签名：Linux helper 施限失败时输出
// JX_SANDBOX_UNAVAILABLE（官方 windows-acl runner 的对等物是
// "windows-acl-run: " 前缀 + 退出码门）。故障必须与「策略拒绝」分开，
// 否则模型会把沙箱坏了当成命令被拒去申请升级。
var runnerFailureSigs = []string{"jx_sandbox_unavailable"}

// runDeniedMarker 检查 confined 输出是否携带「访问被拒」类标志，命中返回
// 官方同款拒绝 marker + 同轮升级提示（带本次够用的最窄更宽档）。执行器
// 自身故障（runnerFailureSigs）命中时改返回 runnerFailureMarker——那是
// 另一类信号，绝不附带升级提示。已知局限：不按退出码门控（官方以非零
// 退出 + 签名双门防「命令只是打印了该字样」的误标），此处输出已合并无
// 退出码可查，误标风险低（命令成功却原样回显拒绝句式的场景罕见）。
func runDeniedMarker(policy, out string, remote bool) string {
	if policy == permFull {
		return ""
	}
	lower := strings.ToLower(out)
	for _, s := range runnerFailureSigs {
		if strings.Contains(lower, s) {
			return runnerFailureMarker()
		}
	}
	sigs := posixDenialSigs
	if isWindowsRuntime && !remote {
		sigs = winDenialSigs
	}
	hit := false
	for _, s := range sigs {
		if strings.Contains(lower, s) {
			hit = true
			break
		}
	}
	if !hit {
		return ""
	}
	wider := "danger-full-access"
	if policy == permReadOnly {
		wider = "workspace-write"
	}
	return fmt.Sprintf("\n[sandbox: file access denied under %s mode]\n[sandbox: escalation available — 用 sandbox_permissions=%s（够用的最窄更宽档）+ 一句 justification 在同一轮内重试同一命令一次；审批弹窗由用户决定。被拒后该命令终局：停止并如实说明，不要绕过；其他命令不受影响]", policy, wider)
}

// runnerFailureMarker 沙箱执行器自身故障的模型面提示（对齐官方
// "[sandbox: the sandbox runner itself failed under <mode> mode — the
// command did not run; this is a sandbox problem, not a command failure]"）：
// 命令没有运行，既不是命令失败也不是策略拒绝——重试与升级都无济于事，
// 唯一正确动作是如实向用户报告。
func runnerFailureMarker() string {
	return "\n[sandbox: 沙箱执行器自身故障——命令没有运行。这是沙箱问题，不是命令失败，也不是策略拒绝：不要重试同一命令，不要申请 sandbox_permissions 升级（升级救不了沙箱故障），请如实向用户说明沙箱不可用]"
}

// ---------- SSH 远程沙箱（官方 sandbox-ssh 思路：远端用它的本地后端执行策略） ----------

// remoteSandboxInfo 缓存在 remoteClient 上：远端架构与 helper 路径。
// 定义在 remote.go 的 remoteClient 结构里（arch / helperPath 字段）。

// runRemoteConfined 把命令包进远端 helper 的沙箱执行。
// helper 首次需要时按远端架构上传（~/.jx-assistant/bin/）。
func (a *App) runRemoteConfined(ctx context.Context, rc *remoteClient, r sandboxRun) (string, error) {
	helper, err := a.ensureRemoteHelper(ctx, rc)
	if err != nil {
		return "", fmt.Errorf("sandbox unavailable: %w", err)
	}
	// helper exec /bin/sh -c 'cd dir && { cmd ;}'：cd 在沙箱内执行。
	// 外层 mktemp 建临时目录 + EXIT trap 清理：ssh 会话 shell 退出时必触发
	// （成功/失败/超时被杀都清理），堵住此前 --temp auto 的每命令泄漏。
	wrapped := "cd " + shellQuote(r.Dir) + " && { " + r.Command + " ;}"
	inner := shellQuote(helper) + " restrict --mode " + r.Policy +
		" --workspace " + shellQuote(r.Workspace) + " --temp \"$JX_TMP\" -- /bin/sh -c " + shellQuote(wrapped)
	cmdline := "JX_TMP=$(mktemp -d /tmp/jx-sbx-XXXXXX) && trap 'rm -rf \"$JX_TMP\"' EXIT && " + inner
	return rc.run(ctx, r.Workspace, cmdline, r.Timeout)
}

// ensureRemoteHelper 确保远端装有对应架构的沙箱 helper，返回其绝对路径。
func (a *App) ensureRemoteHelper(ctx context.Context, rc *remoteClient) (string, error) {
	rc.mu.Lock()
	if rc.helperPath != "" {
		defer rc.mu.Unlock()
		return rc.helperPath, nil
	}
	rc.mu.Unlock()

	// 探测远端架构（HOME 复用 remoteClient 的懒探测）
	out, err := rc.run(ctx, "/tmp", "uname -m", 15*time.Second)
	if err != nil {
		return "", fmt.Errorf("探测远端架构失败: %w", err)
	}
	goarch := map[string]string{
		"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64",
	}[strings.TrimSpace(out)]
	if goarch == "" {
		return "", fmt.Errorf("远端架构 %q 无可用沙箱 helper（支持 x86_64/aarch64）", strings.TrimSpace(out))
	}

	bin, ok := helperBinary(goarch)
	if !ok {
		return "", fmt.Errorf("内嵌 helper 缺失（linux-%s）", goarch)
	}
	dest := rc.homeDir() + "/.jx-assistant/bin/jx-sandbox-linux-" + goarch

	sc, err := rc.sftpClient()
	if err != nil {
		return "", fmt.Errorf("sandbox unavailable: %w", err)
	}
	// 远端路径必须用 POSIX 的 path.Dir：filepath.Dir 在 Windows 宿主上会把
	// 分隔符转成反斜杠，SFTP 将其当作字面文件名字符，目录永远建不到位
	if err := rc.mkdirAll(path.Dir(dest)); err != nil {
		return "", fmt.Errorf("远端创建 helper 目录失败: %w", err)
	}
	needUpload := false
	if fi, serr := sc.Stat(dest); serr != nil || fi.Size() != int64(len(bin)) {
		needUpload = true
	} else {
		// 尺寸巧合相同也要校验语义版本：不符即重传（防旧语义 helper）
		out, verr := rc.run(ctx, "/tmp", shellQuote(dest)+" version", 10*time.Second)
		if verr != nil || strings.TrimSpace(out) != sandboxhelper.Version {
			needUpload = true
		}
	}
	if needUpload {
		if err := rc.writeFile(dest, bin); err != nil {
			return "", fmt.Errorf("上传沙箱 helper 失败: %w", err)
		}
		if err := sc.Chmod(dest, 0o755); err != nil {
			return "", fmt.Errorf("设置 helper 执行位失败: %w", err)
		}
		if out, verr := rc.run(ctx, "/tmp", shellQuote(dest)+" version", 10*time.Second); verr != nil ||
			strings.TrimSpace(out) != sandboxhelper.Version {
			return "", fmt.Errorf("sandbox unavailable: 上传后版本校验失败（远端输出 %q，期望 %s）",
				strings.TrimSpace(out), sandboxhelper.Version)
		}
	}

	rc.mu.Lock()
	rc.arch = goarch
	rc.helperPath = dest
	rc.mu.Unlock()
	return dest, nil
}
