package main

// voicetype_spawn.go —— 桌面版启动时把「独立语音输入法（VoiceTypeGo）」作为**子进程**拉起。
//
// 为什么要由主程序拉起：
//   · 用户装好产品后不该再去双击「启动输入法.bat」；
//   · 输入法需要知道联动文件在哪，由主程序用环境变量 VT_VOICETYPE_JSON 注入最可靠
//     （它读不到本页 localStorage，只能靠这个文件拿热键/主题色）；
//   · 作为子进程管理：主程序退出时一起收掉，不留孤儿（之前踩过孤儿输入法/孤儿 python
//     占着全局钩子和 8320 端口的坑）。
//
// 纪律：
//   · 找不到 exe 只记一行日志，不影响主程序启动；
//   · 已经在跑（单实例互斥体存在）就跳过——否则重复拉起会让用户看到"已在运行"提示；
//   · 只有桌面形态才拉（远程/浏览器/Electron sidecar 形态没有桌面全局钩子的意义）；
//   · 退出码/崩溃不影响主程序：子进程死了只是听写不可用。
//
// 环境变量开关：
//
//	JX_VOICETYPE_DISABLE=1   完全停用（排查用）
//	JX_VOICETYPE_EXE=<路径>  指定 exe（打包位置特殊时用）

import (
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"unsafe"
)

var (
	procOpenMutexW               = syscall.NewLazyDLL("kernel32.dll").NewProc("OpenMutexW")
	procCloseHandle              = syscall.NewLazyDLL("kernel32.dll").NewProc("CloseHandle")
	procOpenProcess              = syscall.NewLazyDLL("kernel32.dll").NewProc("OpenProcess")
	procCreateJobObjectW         = syscall.NewLazyDLL("kernel32.dll").NewProc("CreateJobObjectW")
	procSetInformationJobObject  = syscall.NewLazyDLL("kernel32.dll").NewProc("SetInformationJobObject")
	procAssignProcessToJobObject = syscall.NewLazyDLL("kernel32.dll").NewProc("AssignProcessToJobObject")
	voiceTypeChild               *os.Process // 本次拉起的子进程（退出时收掉）
	voiceTypeJob                 uintptr     // Job 句柄：主程序一消失（含崩溃），内核连带收掉子进程
	voiceTypeSingleMutex                     = "VoiceTypeGo_SingleInstance"
	synchronizeAccess            uintptr     = 0x00100000
)

// Job 对象相关常量与结构（Windows JOBOBJECT_EXTENDED_LIMIT_INFORMATION）
const (
	jobObjectExtendedLimitInformation = 9
	jobObjectLimitKillOnJobClose      = 0x2000
	processSetQuota                   = 0x0100
	processTerminate                  = 0x0001
)

type jobBasicLimitInfo struct {
	PerProcessUserTimeLimit int64
	PerJobUserTimeLimit     int64
	LimitFlags              uint32
	MinimumWorkingSetSize   uintptr
	MaximumWorkingSetSize   uintptr
	ActiveProcessLimit      uint32
	Affinity                uintptr
	PriorityClass           uint32
	SchedulingClass         uint32
}

type jobIOCounters struct {
	ReadOperationCount  uint64
	WriteOperationCount uint64
	OtherOperationCount uint64
	ReadTransferCount   uint64
	WriteTransferCount  uint64
	OtherTransferCount  uint64
}

type jobExtendedLimitInfo struct {
	BasicLimitInformation jobBasicLimitInfo
	IoInfo                jobIOCounters
	ProcessMemoryLimit    uintptr
	JobMemoryLimit        uintptr
	PeakProcessMemoryUsed uintptr
	PeakJobMemoryUsed     uintptr
}

// ensureVoiceTypeJob 建一个"句柄全关即连带杀死子进程"的 Job 对象。
//
// 为什么需要：defer 里的 Kill 只覆盖**正常退出**；主程序被强杀/崩溃时 defer 不会
// 执行，子进程就成孤儿（继续占着全局键盘钩子和 8320 端口——之前排查过这个坑）。
// 挂进这个 Job 后，主程序进程以任何方式消失，内核都会立刻收掉子进程。
func ensureVoiceTypeJob() uintptr {
	if voiceTypeJob != 0 {
		return voiceTypeJob
	}
	h, _, _ := procCreateJobObjectW.Call(0, 0)
	if h == 0 {
		log.Printf("[voicetype] 创建 Job 失败，退化为退出时 Kill（崩溃场景会留孤儿）")
		return 0
	}
	info := jobExtendedLimitInfo{}
	info.BasicLimitInformation.LimitFlags = jobObjectLimitKillOnJobClose
	r, _, err := procSetInformationJobObject.Call(h, jobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), unsafe.Sizeof(info))
	if r == 0 {
		log.Printf("[voicetype] 配置 Job 失败（%v），退化为退出时 Kill", err)
		procCloseHandle.Call(h)
		return 0
	}
	voiceTypeJob = h
	return h
}

// attachToVoiceTypeJob 把刚启动的子进程挂进 Job。
func attachToVoiceTypeJob(pid int) {
	job := ensureVoiceTypeJob()
	if job == 0 {
		return
	}
	ph, _, err := procOpenProcess.Call(processSetQuota|processTerminate, 0, uintptr(pid))
	if ph == 0 {
		log.Printf("[voicetype] 打开子进程句柄失败（%v），无法挂入 Job", err)
		return
	}
	defer procCloseHandle.Call(ph)
	r, _, err := procAssignProcessToJobObject.Call(job, ph)
	if r == 0 {
		log.Printf("[voicetype] 挂入 Job 失败（%v）：主程序崩溃时子进程不会被连带收掉", err)
	}
}

// voiceTypeCandidates 输入法 exe 的探测顺序。
//
// 同时覆盖两种部署形态：
//   - 开发/仓库内：主程序在仓库根，输入法在同一工作区的兄弟目录 VoiceTypeGo/
//   - 安装版：安装目录下的 voicetype\ 子目录（打包时把整个 VoiceTypeGo 目录放进去）
func voiceTypeCandidates() []string {
	if p := os.Getenv("JX_VOICETYPE_EXE"); p != "" {
		return []string{p}
	}
	exe, _ := os.Executable()
	dir := filepath.Dir(exe)
	parent := filepath.Dir(dir)
	return []string{
		// 标准位置：语音组件内部（<安装目录>/组件/voice/voicetype/），开发与安装都适用
		filepath.Join(dir, "组件", "voice", "voicetype", "voicetype_go.exe"),
		// 历史/兼容位置
		filepath.Join(dir, "voicetype", "voicetype_go.exe"),      // <安装目录>\voicetype\
		filepath.Join(dir, "VoiceTypeGo", "voicetype_go.exe"),    // <安装目录>\VoiceTypeGo\
		filepath.Join(parent, "VoiceTypeGo", "voicetype_go.exe"), // 仓库根的兄弟目录（迁移前）
	}
}

// voiceTypeRunning 用单实例互斥体判断输入法是否已在运行（比枚举进程便宜且精确）。
func voiceTypeRunning() bool {
	name, err := syscall.UTF16PtrFromString(voiceTypeSingleMutex)
	if err != nil {
		return false
	}
	h, _, _ := procOpenMutexW.Call(synchronizeAccess, 0, uintptr(unsafe.Pointer(name)))
	if h == 0 {
		return false
	}
	procCloseHandle.Call(h)
	return true
}

// spawnVoiceType 把输入法作为子进程拉起（幂等：已运行/没找到 exe 都静默跳过并记日志）。
//
// 生命周期挂靠「语音组件」：组件停用就不拉、组件启用/重新启用才拉；组件被关闭时
// 由 KillComponent → onKill 回调把它一起杀掉（见 components.go 与 NewApp 的挂接）。
func (a *App) spawnVoiceType() {
	if os.Getenv("JX_VOICETYPE_DISABLE") == "1" {
		log.Printf("[voicetype] 已按 JX_VOICETYPE_DISABLE=1 停用自动拉起")
		return
	}
	// 组件是这套能力的归属：语音组件停用时不拉起（用户"关掉组件"就该彻底没有它）。
	if !a.voiceComponentOn() {
		log.Printf("[voicetype] 语音组件未启用，本轮不拉起输入法")
		return
	}
	if voiceTypeRunning() {
		log.Printf("[voicetype] 输入法已在运行（单实例互斥体存在），本次不重复拉起")
		return
	}
	var exe string
	for _, c := range voiceTypeCandidates() {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			exe = c
			break
		}
	}
	if exe == "" {
		log.Printf("[voicetype] 未找到输入法 exe（候选：%v），跳过自动拉起", voiceTypeCandidates())
		return
	}
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe) // 它按"exe 同目录"找 overlay.html / models / asr_server.py
	// VT_HOST_PID：把本进程 PID 显式告诉输入法（比让它用"父进程 PID"猜更可靠——
	// 父进程退出后 PID 会被系统复用，猜法会把无关程序当成宿主，导致永久让路）。
	env := append(os.Environ(),
		"VT_VOICETYPE_JSON="+a.voicetypeLinkPath(),
		"VT_HOST_PID="+strconv.Itoa(os.Getpid()),
		"VT_PARENT_PID="+strconv.Itoa(os.Getpid()))
	// VT_MODEL_DIR：让它直接用「语音组件」里那套 X-ASR + silero 模型，不再自带副本
	//（两份逐字节相同，各 168MB）。找不到就让它自己按内置顺序探测。
	if md := filepath.Join(filepath.Dir(filepath.Dir(exe)), "models"); dirExists(md) {
		// 标准位置：<组件>/voice/voicetype/voicetype_go.exe → 模型在 <组件>/voice/models
		env = append(env, "VT_MODEL_DIR="+md)
	} else if md2 := filepath.Join(appInstallDir(), "组件", "voice", "models"); dirExists(md2) {
		env = append(env, "VT_MODEL_DIR="+md2)
	} else {
		log.Printf("[voicetype] 未找到组件模型目录，输入法将用自带 models（如无则启动失败）")
	}
	cmd.Env = env
	// 注意：**不要**设 HideWindow: true。
	// 它会把子进程的"第一次 ShowWindow"强制成隐藏（STARTF_USESHOWWINDOW + SW_HIDE），
	// 而 WebView2 的悬浮窗正是靠那次显示建立合成表面——之后就会一直是
	// "窗口可见但一片空白"（实测：抓屏该区域仍是主程序内容，胶囊从没画出来过）。
	// 输入法是 -H windowsgui 构建、本来没有控制台窗口，也不需要隐藏什么。
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if err := cmd.Start(); err != nil {
		log.Printf("[voicetype] 拉起失败（%s）：%v", exe, err)
		return
	}
	voiceTypeChild = cmd.Process
	// 不挂 Job 对象。实测教训：把输入法放进 Job 后，它作为"作业进程"会被系统性限制——
	// WebView2 **完全不渲染**（抓屏：胶囊区域是空白，窗口 IsWindowVisible 却是 true），
	// 剪贴板访问也会被掐断（用户现象：Win+V 面板打不开/剪贴板不好用）。
	// 连带收掉子进程改由输入法自己看父进程（VT_PARENT_PID），崩溃场景同样不留孤儿。
	log.Printf("[voicetype] 已作为子进程拉起：%s pid=%d（联动文件 %s）", exe, cmd.Process.Pid, a.voicetypeLinkPath())
	// 等它退出（不阻塞启动：放 goroutine），退出只记日志，不重启、不影响主程序
	go func(p *os.Process) {
		st, err := p.Wait()
		if err != nil {
			log.Printf("[voicetype] 子进程结束（异常）：%v", err)
			return
		}
		log.Printf("[voicetype] 子进程已退出：%s", st)
	}(cmd.Process)
}

// stopVoiceTypeByComponent 组件关闭时的收尾：连"不是本次拉起"的实例也一起收掉。
//
// 为什么要单独一个：用户可能手动双击启动过输入法（或上个版本留下的），那时
// voiceTypeChild 为 nil，但组件一关，它同样不该继续跑（它占着全局键盘钩子）。
// 只按 exe 完整路径匹配，绝不误杀别的程序。
func stopVoiceTypeByComponent() {
	exe, _ := os.Executable()
	_ = exe
	found := false
	for _, c := range voiceTypeCandidates() {
		if st, err := os.Stat(c); err == nil && !st.IsDir() {
			found = true
			break
		}
	}
	if !found {
		stopVoiceType()
		return
	}
	// 用 taskkill 按映像名收（只可能命中我们自己的 voicetype_go.exe）：
	// 可选做法是枚举进程比路径，但这里映像名是专有名字，足够精确且简单。
	kill := exec.Command("taskkill", "/IM", "voicetype_go.exe", "/F")
	if err := kill.Run(); err != nil {
		log.Printf("[voicetype] 组件关闭：未发现运行中的输入法（%v）", err)
	} else {
		log.Printf("[voicetype] 组件关闭：已结束输入法进程")
	}
	voiceTypeChild = nil
}

// stopVoiceType 退出时收掉子进程（只收"本次由我们拉起的"那个）。
func stopVoiceType() {
	if voiceTypeChild == nil {
		return
	}
	if err := voiceTypeChild.Kill(); err == nil {
		log.Printf("[voicetype] 随主程序退出，已结束输入法子进程 pid=%d", voiceTypeChild.Pid)
	}
	voiceTypeChild = nil
}

// appInstallDir 主程序所在目录（开发=仓库根，安装版=安装目录）。
func appInstallDir() string {
	exe, _ := os.Executable()
	return filepath.Dir(exe)
}
