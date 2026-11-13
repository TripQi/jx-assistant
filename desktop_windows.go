//go:build windows

package main

// 桌面壳（Windows）：用系统自带的 WebView2 运行时开原生窗口承载本地
// 服务页面，替代「起服务 + 弹默认浏览器」的网页形态。
//
// - 运行时预检：Evergreen WebView2 未见时给安装指引（Win10/11 一般自带）；
// - 单实例：命名互斥体，二次启动唤起已有窗口后退出；
// - 窗口图标取自 exe 资源里的 RT_GROUP_ICON（id=1，见 打包/go-winres）。

import (
	"fmt"
	"log"
	"os"
	"path/filepath"
	"reflect"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	webview2 "github.com/jchv/go-webview2"
	edge "github.com/jchv/go-webview2/pkg/edge"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	desktopAppTitle  = "AI数字助理"
	desktopMutexName = `Local\JX-Digital-Assistant-GO-SingleInstance`
	// WebView2 Evergreen 的 EdgeUpdate 客户端键与官方引导器下载地址
	webView2ClientKey       = `{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}`
	webView2BootstrapperURL = "https://go.microsoft.com/fwlink/p/?LinkId=2124703"
	webView2ArgsEnv         = "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS"
)

var (
	desktopUser32          = windows.NewLazySystemDLL("user32.dll")
	procDesktopShowWindow  = desktopUser32.NewProc("ShowWindow")
	procDesktopMessageBoxW = desktopUser32.NewProc("MessageBoxW")
	desktopKernel32        = windows.NewLazySystemDLL("kernel32.dll")
	procGetStdHandle       = desktopKernel32.NewProc("GetStdHandle")
)

const (
	stdOutputHandle = ^uintptr(10) // -11
	invalidHandle   = ^uintptr(0)
)

// stdHandleValid 报告标准输出是否为有效句柄（控制台或管道）。
// -H=windowsgui 构建从资源管理器双击启动时句柄无效。
func stdHandleValid(h uintptr) bool {
	return h != 0 && h != invalidHandle
}

// guiLogRedirect：GUI 子系统构建没有控制台，双击启动时 log 输出全部
// 落空。检测到无有效标准输出句柄时把 log 改落到 data\desktop.log
// （超 5MB 轮转成 .old），诊断信息不丢；带控制台/管道启动（终端调试、
// 脚本调用）保持原样打屏。
// 脚本调用）保持原样打屏。无控制台时 panic/fatal 的堆栈同样接到这份日志。
func guiLogRedirect() {
	if h, _, _ := procGetStdHandle.Call(stdOutputHandle); stdHandleValid(h) {
		return
	}
	logPath := filepath.Join("data", "desktop.log")
	_ = os.MkdirAll(filepath.Dir(logPath), 0755)
	if st, err := os.Stat(logPath); err == nil && st.Size() > 5<<20 {
		_ = os.Rename(logPath, logPath+".old")
	}
	// 日志只保留近 7 天：轮转出的 .old 超期即删（常态下它会在下次轮转被
	// 覆盖，这里兜底「长期不重启、一直没触发轮转」的场景），日志盘占用
	// 有界：desktop.log + 至多一份 7 天内的 .old。
	if st, err := os.Stat(logPath + ".old"); err == nil && time.Since(st.ModTime()) > 7*24*time.Hour {
		_ = os.Remove(logPath + ".old")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		return
	}
	log.SetOutput(f)
	// 崩溃现场也要落盘（2026-10-14）：GUI 子系统构建没有控制台，panic / fatal
	// error 的堆栈默认写 stderr，而 stderr 句柄同样无效——「闪退」于是不留任何
	// 痕迹（事件查看器也看不到 Application Error：Go 自己接住了异常后 exit(2)）。
	// 把 stderr 接到同一份日志，并交给 runtime 的 crash 输出通道，下次闪退就能
	// 在 data\desktop.log 里直接看到堆栈。
	os.Stderr = f
	if err := debug.SetCrashOutput(f, debug.CrashOptions{}); err != nil {
		log.Printf("崩溃输出接管失败（panic 堆栈可能仍丢失）: %v", err)
	}
}

// attachParentConsole：GUI 子系统 exe 在终端里跑 CLI 子命令（如
// sandbox acl-audit）时，挂到父进程的控制台并重绑标准句柄，让
// fmt/os.Stdout 输出正常回显。已有可用句柄（管道/控制台）则不动。
func attachParentConsole() {
	if h, _, _ := procGetStdHandle.Call(stdOutputHandle); stdHandleValid(h) {
		return
	}
	procAttachConsole := desktopKernel32.NewProc("AttachConsole")
	const attachParentProcess = ^uintptr(0) // -1
	if r, _, _ := procAttachConsole.Call(attachParentProcess); r == 0 {
		return // 父进程没有控制台（双击启动），静默
	}
	if h, _, _ := procGetStdHandle.Call(stdOutputHandle); stdHandleValid(h) {
		os.Stdout = os.NewFile(h, "/dev/stdout")
	}
	if h, _, _ := procGetStdHandle.Call(^uintptr(11)); stdHandleValid(h) { // -12 stderr
		os.Stderr = os.NewFile(h, "/dev/stderr")
	}
}

// webView2RuntimeAvailable 检测系统是否装了 WebView2 运行时：
// 固定版本（环境变量指定目录）> Evergreen（HKLM/HKCU EdgeUpdate 的 pv 值）。
func webView2RuntimeAvailable() bool {
	if dir := os.Getenv("WEBVIEW2_BROWSER_EXECUTABLE_FOLDER"); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "msedgewebview2.exe")); err == nil {
			return true
		}
	}
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		for _, subkey := range []string{
			`SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\` + webView2ClientKey,
			`SOFTWARE\Microsoft\EdgeUpdate\Clients\` + webView2ClientKey,
		} {
			k, err := registry.OpenKey(root, subkey, registry.QUERY_VALUE)
			if err != nil {
				continue
			}
			pv, _, err := k.GetStringValue("pv")
			k.Close()
			if err == nil && pv != "" && pv != "0.0.0.0" {
				return true
			}
		}
	}
	return false
}

// acquireSingleInstance 创建命名互斥体；already=true 表示已有一个桌面实例
// 在跑（此时 release 为空函数，调用方应唤起旧窗口后退出）。
func acquireSingleInstance() (release func(), already bool) {
	procCreateMutexW := windows.NewLazySystemDLL("kernel32.dll").NewProc("CreateMutexW")
	name := desktopMutexName
	// JX_DESKTOP_MUTEX：覆盖互斥体名（不带 Local\ 前缀时自动补上）——开发
	// 调试需要多实例并行时用，发行形态不受影响。
	if v := strings.TrimSpace(os.Getenv("JX_DESKTOP_MUTEX")); v != "" {
		if !strings.Contains(v, `\`) {
			v = `Local\` + v
		}
		name = v
	}
	mu, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return func() {}, false
	}
	h, _, callErr := procCreateMutexW.Call(0, 1, uintptr(unsafe.Pointer(mu)))
	if errno, ok := callErr.(syscall.Errno); ok && errno == windows.ERROR_ALREADY_EXISTS {
		return func() {}, true
	}
	if h == 0 {
		// 互斥体创建失败不拦截启动，退化为允许多开
		return func() {}, false
	}
	return func() { _ = windows.CloseHandle(windows.Handle(h)) }, false
}

// activateExistingWindow 按标题找回已有桌面窗口并置前
// （FindWindowW/SetForegroundWindow 复用 pickfolder_windows.go 的声明）。
func activateExistingWindow() {
	title, err := windows.UTF16PtrFromString(desktopAppTitle)
	if err != nil {
		return
	}
	hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title)))
	if hwnd == 0 {
		return
	}
	procDesktopShowWindow.Call(hwnd, 9) // SW_RESTORE
	procSetForegroundWindow.Call(hwnd)
}

func desktopMessageBox(text, caption string) {
	t, _ := windows.UTF16PtrFromString(text)
	c, _ := windows.UTF16PtrFromString(caption)
	procDesktopMessageBoxW.Call(0, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(c)), 0x30) // MB_ICONWARNING
}

// workspaceFolderName 是默认工作区目录名：安装目录下的「默认工作区」。
// 旧版用的是「工作区」（legacyWorkspaceFolderName），启动时原地改名迁移
// （见 desktopWorkspaceDefault），用户已有的文件一份不搬不丢。
const (
	workspaceFolderName       = "默认工作区"
	legacyWorkspaceFolderName = "工作区"
)

// migrateLegacyWorkspaceDir 把旧默认工作区目录原地改名成新名（数据留在原处、
// 不改路径引用之外的东西）。返回旧目录路径（空串 = 没有旧目录要处理）与是否已
// 落到新目录（新目录本来就在，或改名成功）。旧目录在、改名失败时如实返回 false，
// 由调用方决定是回退旧目录还是继续——不在这里替它做决定。
func migrateLegacyWorkspaceDir(oldDir, newDir string) (legacy string, migrated bool) {
	fi, err := os.Stat(oldDir)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	if _, err := os.Stat(newDir); err == nil {
		return oldDir, true // 新目录已在（装包建的或上次迁移留下的）：旧目录原样保留
	}
	if rerr := os.Rename(oldDir, newDir); rerr != nil {
		log.Printf("默认工作区改名失败（%s → %s）: %v", oldDir, newDir, rerr)
		return oldDir, false
	}
	return oldDir, true
}

// desktopWorkspaceDefault 归置桌面模式的默认工作区：默认工作区不落在安装目录
// 本身，而是安装目录下的「默认工作区」子文件夹——模型的工作区域与软件本体
// （exe/config/data/技能）分离，默认目录同时跟随安装位置。只认默认形态（当前值
// 就是 exe 目录本身，或旧版默认目录「工作区」）；用户自己设过的工作目录一律不动。
// 旧版的「工作区」原地改名为「默认工作区」；改名失败则保持原样、下次启动再试
// （不把用户切到一个可能为空的新目录上）。
// 返回 (legacy, changed)：changed = 是否改写了配置（调用方负责回写，避免设置页
// 与实际不一致）；legacy = 被搬走的旧默认目录，仅 changed 为真时有效，调用方用
// 它把显式挂在旧路径上的历史会话一并改指新目录。
// 安装目录不可写时（系统级位置）回退「文档\坚祥数字助理」。
func desktopWorkspaceDefault(cfg *Config) (legacy string, changed bool) {
	exe, err := os.Executable()
	if err != nil {
		return "", false
	}
	exeDir := filepath.Clean(filepath.Dir(exe))
	ws, err := filepath.Abs(cfg.Workspace)
	if err != nil {
		return "", false
	}
	ws = filepath.Clean(ws)
	oldDir := filepath.Join(exeDir, legacyWorkspaceFolderName)
	dir := filepath.Join(exeDir, workspaceFolderName)
	if !stringsEqualFold(ws, exeDir) && !stringsEqualFold(ws, oldDir) {
		return "", false // 用户自选的工作目录：不碰
	}
	if lg, migrated := migrateLegacyWorkspaceDir(oldDir, dir); lg != "" {
		if !migrated && stringsEqualFold(ws, oldDir) {
			return "", false // 旧目录仍是当前工作区：保持原样，下次启动再试
		}
		legacy = lg
	}
	if mkErr := os.MkdirAll(dir, 0755); mkErr != nil {
		docs := filepath.Join(os.Getenv("USERPROFILE"), "Documents")
		if kf, kerr := windows.KnownFolderPath(windows.FOLDERID_Documents, 0); kerr == nil {
			docs = kf
		}
		dir = filepath.Join(docs, "坚祥数字助理")
		_ = os.MkdirAll(dir, 0755)
	}
	cfg.Workspace = dir
	return legacy, true
}

// desktopDataDir 桌面壳本地状态的落地根目录（WebView2 用户数据、兼容模式
// 标记文件）：跟随配置里的 dataDir，而不是写死 "data"——开发/测试实例用
// 隔离的数据目录时，浏览器 profile 与兼容开关也跟着隔离，不会和正式实例
// 抢同一个 user-data-folder（同目录双开会创建失败，界面偏好也会互相串）。
// 默认配置的 dataDir 就是 "data"，安装版路径与旧版完全一致。
func desktopDataDir(app *App) string {
	if app != nil && app.Cfg != nil && strings.TrimSpace(app.Cfg.DataDir) != "" {
		return app.Cfg.DataDir
	}
	return "data"
}

func stringsEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		ca, cb := a[i], b[i]
		if 'A' <= ca && ca <= 'Z' {
			ca += 'a' - 'A'
		}
		if 'A' <= cb && cb <= 'Z' {
			cb += 'a' - 'A'
		}
		if ca != cb {
			return false
		}
	}
	return true
}

// runDesktop 打开 WebView2 窗口并驻留运行，直到退出请求（托盘菜单
// 「退出」/ 自动冒烟 JX_DESKTOP_AUTOCLOSE）：用户关窗只隐藏进托盘。
// JX_DESKTOP_AUTOCLOSE 秒数>0 时到点自动关窗（自动化冒烟用）。
// 桌面壳就是主 WebView2 控件（承载 Web UI）；右栏原生浏览器面板
// （browser_panel_windows.go）作为第二个 WebView2 控件惰性创建，挂在
// 本窗口上。URL 带 shell=desktop 参数：前端据此进入桌面形态（右栏
// 浏览器视图 + 矩形上报）。
//
// 【临时护栏】整个窗口循环连同主窗口 subclass / 托盘 / 面板 COM 等全部
// Windows 回调都跑在这条 goroutine 上，必须置于 stackguard_windows.go
// 的预增长深栈内——规避 Go 运行时「回调期间栈拷贝」静默闪退缺陷（详见
// 该文件注释；Go 官方修复后随护栏一并移除）。
func runDesktop(app *App, url string) error {
	return withGuardedStackResult(func() error { return runDesktopLoop(app, url) })
}

func runDesktopLoop(app *App, url string) error {
	if !webView2RuntimeAvailable() {
		desktopMessageBox(
			"未检测到 Microsoft WebView2 运行时（Windows 10 / 11 一般自带）。\n\n"+
				"请先安装 WebView2 Runtime 再启动桌面版：\n"+webView2BootstrapperURL+"\n\n"+
				"也可以不带 -desktop 参数运行，用浏览器模式使用。", desktopAppTitle)
		return fmt.Errorf("缺少 WebView2 运行时")
	}
	release, already := acquireSingleInstance()
	defer release()
	if already {
		activateExistingWindow()
		return nil
	}
	// 冷启动直达兼容模式：设置页手动开启落过标记文件（<dataDir>/webview-compat.flag）
	// 就直接软件合成进来。参数合并与麦克风权限说明见 newDesktopFrontend
	// （每轮前端创建时都会做）。
	softwareCompatPath = filepath.Join(desktopDataDir(app), "webview-compat.flag")
	if _, err := os.Stat(softwareCompatPath); err == nil {
		webviewSoftwareRender.Store(true)
		log.Printf("兼容显示模式已启用（软件合成，标记文件 %s 存在）", softwareCompatPath)
	}
	// 前端窗口循环：窗口的生死与进程的生死解耦——用户关窗（✕/Alt+F4）在
	// subclass 里按设置分流：closeToTray 开（默认）拦截进托盘
	// （hideMainWindowToTray），Run 不返回；关则置 frontendQuitReq 放行销毁
	// ＝真正退出。热重启/托盘退出/autoclose 的 frontendQuitReq 也走同一收尾。
	// 设置页渲染切换与 WebView2 崩溃自愈（ProcessFailed）置 frontendRestartReq
	// 关窗重建，窗口意外死亡也自动重建——
	// 后端（HTTP 服务、会话、运行中任务）全在 Go 进程里，一个字节都不动。
	// 旧前端的 WebView2 控制器在收尾时 Close（浏览器进程树与 GPU 占用一并
	// 释放），新旧环境用不同数据目录（webview / webview-compat），互不抢
	// user-data-folder。
	// 托盘先挂上：后端（本进程）驻留的可见锚点，主窗口关闭/意外死亡都不消失。
	traySetup(app)
	defer trayTeardown()
	var prevRect *panelRect
	for {
		w := newDesktopFrontend(desktopDataDir(app), prevRect)
		if w == nil {
			return fmt.Errorf("创建桌面窗口失败")
		}
		subclassMainWindow(uintptr(w.Window()))
		// 当前主窗口登记（原子）：restartFrontendNow 从任意 goroutine 取
		// 「此刻活着的那扇窗」投递关窗，热重启的重建间隙里重试等待新句柄。
		mainWndHwnd.Store(uintptr(w.Window()))
		// 右栏原生浏览器面板（每会话一个实例，App.panels）：先登记本轮前端
		// 窗口的 UI 线程投递与 HWND——旧窗口的面板已随上一轮收尾释放，本轮
		// 的面板由此后的矩形上报/导航按会话惰性创建。COM 调用一律经 Dispatch
		// 投递到本窗口的 UI 线程。Z 序重申与拖动让路的钩子作用于全部面板
		//（各自内部按可见性早退，屏外面板零动作）。
		app.panelFrontendReset(w.Dispatch, uintptr(w.Window()))
		panelReassertHook.Store(func() {
			app.panelEach(func(p browserTarget) {
				if bp, ok := p.(*browserPanel); ok {
					bp.reassertTopOnUI()
				}
			})
		})
		// 主窗口被用户拖动/缩放（含最大化还原拖动）时让面板暂停落位：系统的
		// 模态移动循环里不再碰 WebView2 子窗口（详见 holdForWindowDrag）。
		panelDragHoldHook.Store(func(hold bool) {
			app.panelEach(func(p browserTarget) {
				if bp, ok := p.(*browserPanel); !ok {
					return
				} else if hold {
					bp.holdForWindowDrag()
				} else {
					bp.releaseAfterWindowDrag()
				}
			})
		})
		// 右侧栏截图（panel_screenshot 工具）的通道：shellEval 注入主界面 JS，
		// 探针把右栏矩形 POST /api/shell/panel-view 回到 panelViewCh。
		// ExecuteScript 是 COM 调用，必须发生在 UI 线程——工具侧从任意 goroutine
		// 调 shellEval，这里经 Dispatch 投递到主窗口消息循环再执行。
		app.shellEval = func(js string) { w.Dispatch(func() { w.Eval(js) }) }
		app.shellWnd = uintptr(w.Window())
		app.panelViewCh = make(chan panelViewReport, 1)
		// 导航守卫先于首次 Navigate 挂上：非本机源的顶层导航/弹窗一律拦下转
		// 系统浏览器（防外链把主界面顶成错误页，见 desktop_navguard_windows.go）
		attachNavGuard(mainWebViewOf(w))
		sep := "?"
		if strings.Contains(url, "?") {
			sep = "&"
		}
		w.Navigate(url + sep + "shell=desktop")
		// WebView2 崩溃自愈：渲染进程死亡/无响应自动刷新，浏览器进程死亡重建前端
		attachProcessFailed(mainWebViewOf(w))
		// 拖拽原件路径桥：页面拖入文件的真实路径只有宿主拿得到（见 dropbridge_windows.go），
		// 交给页面登记成「路径型附件」，交付工具才能把成品送回桌面原位置。
		attachDropBridge(mainWebViewOf(w), func(js string) { w.Dispatch(func() { w.Eval(js) }) })
		if secs, err := strconv.Atoi(os.Getenv("JX_DESKTOP_AUTOCLOSE")); err == nil && secs > 0 {
			// 每一轮前端都各自布防（热重启换了窗口句柄，只布防首轮会让
			// 新窗口永远等不到关窗指令）。
			go func() {
				time.Sleep(time.Duration(secs) * time.Second)
				// Terminate() 的 PostQuitMessage 只作用于调用线程，跨线程
				// 无效；Destroy() 走 PostMessage(WMClose)，跨线程安全。
				atomic.StoreInt32(&frontendQuitReq, 1) // 冒烟关窗 = 真退出（托盘驻留语义不影响）
				w.Destroy()
			}()
		}
		w.Run()
		recorderShutdown() // 还在录演示就停录收尾（保 audio.wav、生成 INDEX.md）
		// 收尾在 UI 线程（Run 返回处即主线程）：关面板与主控的 COM Close，
		// 让两棵浏览器进程树真正退场（GPU/句柄全释放），再拆窗口句柄。
		// 收尾前反注册崩溃事件（每轮前端换新控件，旧 handler 不能留给已销毁的环境）
		if wv := mainWebViewOf(w); wv != nil {
			detachProcessFailed(wv)
			detachDropBridge(wv)
			detachNavGuard(wv)
		}
		// Run 返回即窗口已亡：清掉句柄登记，托盘唤起/热重启投递不再指向死窗口
		mainWndHwnd.Store(0)
		releaseFrontend(app, w)
		w.Destroy()
		switch {
		case atomic.LoadInt32(&frontendQuitReq) == 1:
			// 退出请求（托盘菜单「退出」/ 自动冒烟 autoclose）：整体收尾退出
			log.Printf("退出请求（托盘/自动冒烟）：桌面壳退出")
			return nil
		case atomic.LoadInt32(&frontendRestartReq) == 1:
			// 热重启（设置页渲染模式切换 / ProcessFailed 崩溃自愈）：按当前显示模式重建
			atomic.StoreInt32(&frontendRestartReq, 0)
			prevRect = restartWndRect.Load()
			log.Printf("前端热重启：重建界面（后端与任务不受影响）")
		default:
			// 意外返回（WebView2 崩溃、窗口异常死亡）：非用户退出也非热重启请求
			// ——自动重建前端，后端不动；带风暴保护（10 分钟内 ≥5 次弹窗告知一次，
			// 避免运行时损坏时无限闪窗，进程保持驻留托盘）。
			prevRect = restartWndRect.Load()
			if !frontendCrashAllowRebuild() {
				log.Printf("前端连续意外死亡达上限：已弹窗告知，继续重建（进程驻留托盘）")
			}
			log.Printf("桌面窗口意外关闭：自动重建前端（后端与任务不受影响）")
		}
	}
}

// newDesktopFrontend 建主窗口（含附加参数合并与兼容模式分流）。兼容模式走
// 独立数据目录 <dataDir>/webview-compat：Chromium 参数在环境创建时定死、换
// 参数必须换环境，而新旧环境的 user-data-folder 不能相同（旧浏览器进程异步
// 退场，同目录创建会失败）；代价仅是兼容窗的前端本地存储（界面偏好类）从零
// 开始，业务数据都在服务端。prev 非空时新窗原位重开（热重启不跳位）。
//
// dataDir 传配置里的 dataDir（见 desktopDataDir）：WebView2 用户数据目录
// 跟着数据目录走，开发/测试实例把 dataDir 隔离后浏览器 profile 也一并隔离。
func newDesktopFrontend(dataRoot string, prev *panelRect) webview2.WebView {
	// 麦克风权限：桌面端应当「天生具备」，不能每次都弹审批。
	// 绑定库建主控件时只显式放行了剪贴板（jchv/go-webview2 的
	// SetPermission(ClipboardRead)），其余权限按「默认」交给 WebView2：桌面壳里
	// 弹不出可靠的授权气泡，语音输入就会直接以 not-allowed 失败。这里在创建主
	// 控件（＝主环境，只在这一次调用里建）之前，把 Chromium 的「自动接受媒体
	// 权限」开关并进已有的附加参数，建完立刻还原：应用一启动就带着麦克风权限，
	// 整个生命周期一次都不用问、也不用用户去点授权（下次启动同样如此）。
	// 右栏浏览器面板用的是另一个环境、且是惰性创建的，不会被带上（不给面板里
	// 的任意网站自动放行麦克风）；系统层面的麦克风开关（Windows 设置 → 隐私 →
	// 麦克风）仍照旧生效，是唯一的最终闸门。
	const autoGrantMic = "--use-fake-ui-for-media-stream"
	// 防遮挡停帧：关掉 Chromium 的原生窗口遮挡判定。电脑操作技能执行时
	// 主窗口长时间被目标程序盖住，Chromium 判定「已遮挡」停止合成，解除后
	// 偶发不再续帧＝整个客户区黑屏、执行不受影响（部分机型才复现，
	// WebView2Feedback #5171 同类）。新版运行时或已忽略该开关（无害）。
	// 防黑屏目前只靠此开关；真死了由 ProcessFailed 自愈刷新/重建兜底。
	// 已带 --disable-features 的环境把本特性并进去，避免出现第二个同名
	// 开关把用户的值顶掉。
	const noOcclusion = "CalculateNativeWinOcclusion"
	prevArgs, hadArgs := os.LookupEnv(webView2ArgsEnv)
	merged := prevArgs
	if !strings.Contains(prevArgs, autoGrantMic) {
		merged = strings.TrimSpace(merged + " " + autoGrantMic)
	}
	if !strings.Contains(prevArgs, noOcclusion) {
		const dfPrefix = "--disable-features="
		if i := strings.Index(prevArgs, dfPrefix); i >= 0 {
			merged = prevArgs[:i+len(dfPrefix)] + noOcclusion + "," + prevArgs[i+len(dfPrefix):]
		} else {
			merged = strings.TrimSpace(merged + " " + dfPrefix + noOcclusion)
		}
	}
	profileDir := filepath.Join(dataRoot, "webview")
	if webviewSoftwareRender.Load() {
		// 兼容显示模式（软件合成）：设置页手动开启（标记文件）触发，把 GPU 整个
		// 让给与我们抢资源的其他应用（聊天界面很轻，软件合成无压力）。删掉
		// <dataDir>/webview-compat.flag 即恢复默认硬件加速（热重启本会话内也会切回）。
		if !strings.Contains(merged, "--disable-gpu-compositing") {
			merged = strings.TrimSpace(merged + " --disable-gpu-compositing")
		}
		profileDir = filepath.Join(dataRoot, "webview-compat")
		log.Printf("兼容显示模式：软件合成，数据目录 %s", profileDir)
	}
	if merged != prevArgs {
		_ = os.Setenv(webView2ArgsEnv, merged)
	}
	w := webview2.NewWithOptions(webview2.WebViewOptions{
		AutoFocus: true,
		DataPath:  profileDir,
		WindowOptions: webview2.WindowOptions{
			Title:  desktopAppTitle,
			Width:  1280,
			Height: 880,
			IconId: 1, // 打包/go-winres 生成的 RT_GROUP_ICON 资源号
			Center: true,
		},
	})
	if hadArgs {
		_ = os.Setenv(webView2ArgsEnv, prevArgs)
	} else {
		_ = os.Unsetenv(webView2ArgsEnv)
	}
	if w == nil {
		return nil
	}
	if prev != nil && prev.Left > -30000 {
		procPanelSetWindowPos.Call(uintptr(w.Window()), 0,
			uintptr(int(prev.Left)), uintptr(int(prev.Top)),
			uintptr(int(prev.Right-prev.Left)), uintptr(int(prev.Bottom-prev.Top)),
			swpNoZOrder) // 触发 WM_SIZE，绑定库会跟着 PutBounds
	}
	return w
}

// releaseFrontend 前端收尾（仅 UI 线程调用）：把右栏面板与主界面的 WebView2
// 控制器 Close 掉，浏览器进程树随之退场。绑定库不暴露主控件，这里经反射取
// 其内部 *edge.Chromium（go.mod 已锁定版本；字段变动时 NewAt 抛错由 recover
// 兜住，退化为「进程退出时由 Job Object 回收」，功能不受影响）。
func releaseFrontend(app *App, w webview2.WebView) {
	// 右栏的面板集合逐个兜底释放（窗口将亡、消息泵将停；下一轮前端循环
	// 的 panelFrontendReset 会整体重建集合）
	app.panelEach(func(p browserTarget) {
		if bp, ok := p.(*browserPanel); ok {
			bp.releaseAfterMainLoopGone()
		}
	})
	func() {
		defer func() { _ = recover() }()
		rv := reflect.ValueOf(w)
		if rv.Kind() != reflect.Ptr {
			return
		}
		rv = rv.Elem()
		f := rv.FieldByName("browser")
		if !f.IsValid() || (f.Kind() == reflect.Ptr && f.IsNil()) {
			return
		}
		ch, _ := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*edge.Chromium)
		if ch == nil {
			return
		}
		if ctl := ch.GetController(); ctl != nil {
			_, _, _ = ctlVtblOf(ctl).close.Call(uintptr(unsafe.Pointer(ctl)))
		}
		// 主壳实例同款保活（retirePanelChromium 的语义在面板侧，这里是主
		// WebView2）：热重启销毁旧前端后，绑定库交给原生侧的 handler 裸指针
		// 仍指着它——旧 Chromium 若被 GC 回收，下一次原生回调就是静默闪退。
		retirePanelChromium(ch)
	}()
}

// ---------- 主窗口子类化（关窗进托盘 / 拖动让路 / 面板 Z 序） ----------
//
// 子类化只保留三件与窗口消息强相关的事（2026-10-15 撤掉 1px 渲染唤醒与
// 黑屏自检/自动降级后收敛，快速切窗时的自发挪窗不再存在）：
//   - 用户关窗（SC_CLOSE）隐藏进托盘，进程与后端驻留（见 tray_windows.go）；
//   - 吞掉最小化时的 WM_SIZE——绑定库会拿退化矩形 PutBounds 给 WebView2，
//     是坏矩形的来源之一；
//   - 拖动/缩放让路（WM_ENTERSIZEMOVE/WM_EXITSIZEMOVE，见 mainWndInSizeMove）
//     与激活/Z 序变化时的面板 Z 序重申（Chromium 聚焦会把主 WebView2 提到顶、
//     压住右栏面板——右栏白屏的根因之一）。
// 渲染层的防遮挡停帧由 newDesktopFrontend 的 CalculateNativeWinOcclusion
// 禁用开关承担；真黑屏由 WebView2 ProcessFailed 自愈与设置页手动兼容模式兜底。
const (
	wmSize             = 0x0005
	sizeMinimized      = 1
	wmActivate         = 0x0006
	wmWindowPosChanged = 0x0047
	wmSysCommand       = 0x0112 // 系统命令（wParam 低 16 位 = 命令：关闭/最大化/还原等）
	scClose            = 0xf060 // SC_CLOSE：用户关闭窗口（✕ / Alt+F4 / 任务栏右键）
	wmEnterSizeMove    = 0x0231 // 用户开始拖动/缩放窗口（标题栏拖动、最大化还原拖动都算）
	wmExitSizeMove     = 0x0232 // 系统的模态移动/缩放循环结束
	swpNoSizeFlag      = 0x0001
	swpNoMoveFlag      = 0x0002
	waInactive         = 0
	gwlpWndProc        = ^uintptr(3) // GWLP_WNDPROC(-4)：按位取反，^3 == -4
)

// windowPosMirror 镜像 Win32 WINDOWPOS（x64 布局；只读 flags 字段）。
type windowPosMirror struct {
	hwnd, after  uintptr
	x, y, cx, cy int32
	flags        uint32
}

var (
	// panelReassertHook 主窗口激活/Z 序变化时重申面板 Z 序（runDesktop 注入，
	// 见 browserPanel.reassertTopOnUI）。原子指针：subclass 回调与窗口循环并发的护栏。
	panelReassertHook atomic.Value // 存 func()：主窗口激活时重申面板 Z 序
	// panelDragHoldHook 用户拖动/缩放主窗口时通知面板层让路（runDesktop 注入，
	// 见 browserPanel.holdForWindowDrag）。存 func(bool)：true=进入拖动，
	// false=拖动结束。与 panelReassertHook 同样的原子指针护栏。
	panelDragHoldHook     atomic.Value
	procSetWindowLongPtrW = desktopUser32.NewProc("SetWindowLongPtrW")
	procCallWindowProcW   = desktopUser32.NewProc("CallWindowProcW")
	procDefWindowProcW    = desktopUser32.NewProc("DefWindowProcW")
	mainWndOrigProc       uintptr
	mainWndSubclassCb     = windows.NewCallback(mainWndSubclassProc)
	// mainWndInSizeMove = 1：UI 线程正处在系统的模态移动/缩放循环内（用户按住
	// 标题栏/边框正在拖动窗口，含最大化窗口被拖出还原）。拖动期间一律不做
	// 自发挪窗与面板 Z 序重申——拖动由系统驱动，我们在自己的回调栈上同步改窗口
	// 几何会让拖动错乱，并把 WebView2 控制器（PutBounds /
	// NotifyParentWindowPositionChanged / 面板子窗口 SetWindowPos）在同一栈上重入；
	// 2026-10-14 的「全屏拖动闪退」正出在这条路上。拖动结束
	// （WM_EXITSIZEMOVE，栈已干净）一次性补做 Z 序重申。
	mainWndInSizeMove int32
)

// subclassMainWindow 换掉主窗口原 wndproc（安装必须在 UI 线程；此后所有消息
// 先经 mainWndSubclassProc 再转回绑定库原 proc）。失败则保持原样并落日志：
// 行为退回原生消息处理（关窗直接退出而非进托盘），不影响功能。
func subclassMainWindow(hwnd uintptr) {
	if hwnd == 0 {
		return
	}
	orig, _, callErr := procSetWindowLongPtrW.Call(hwnd, gwlpWndProc, mainWndSubclassCb)
	if orig != 0 {
		mainWndOrigProc = orig
	} else {
		log.Printf("主窗口子类化失败：关窗将直接退出（进托盘失效），errno=%v", callErr)
	}
}

func mainWndSubclassProc(hwnd, msg, wp uintptr, lp unsafe.Pointer) uintptr {
	// 本过程跑在原生的 Windows 消息栈上：panic 从这里冒出去＝整个进程当场消失、
	// 日志一个字都不留（windowsgui 子系统下无 stderr）。就地兜住并落堆栈：
	// 进程保住，现场留下（返回 0＝按已处理对待，行为上等价于不做额外动作）。
	defer func() {
		if r := recover(); r != nil {
			log.Printf("主窗口过程 panic（已兜住，未终止进程）: %v\n%s", r, debug.Stack())
		}
	}()
	switch msg {
	case wmSysCommand:
		// 用户关闭窗口（✕ / Alt+F4 / 任务栏右键「关闭窗口」）：设置「关闭窗口
		// 驻留后台」（closeToTray，默认开）开着且托盘图标在位时隐藏进托盘，进程
		// 与后端（服务/会话/任务）驻留；设置关掉（或图标挂载失败）则「关窗=退出」
		// ——放行销毁前先置退出请求，否则窗口循环会把 Run 返回当成意外死亡自动
		// 重建窗口，程序永远退不出去。SC_CLOSE 比较前先掩掉 wParam 低 4 位的
		// 系统内部标志；拦截时不传原 proc 就不会走到 WM_CLOSE 的销毁路径，消息
		// 循环继续跑。程序化投递的 WM_CLOSE（热重启 / 托盘退出 / 冒烟 autoclose）
		// 不经本分支，销毁语义不变。
		if wp&0xfff0 == scClose {
			if trayResidencyOn() {
				hideMainWindowToTray(hwnd)
				return 0
			}
			atomic.StoreInt32(&frontendQuitReq, 1)
		}
	case wmSize:
		if wp == sizeMinimized {
			return 0 // 别让绑定库把最小化下的退化矩形 PutBounds 给 WebView2
		}
	case wmActivate:
		if wp&0xffff != waInactive && atomic.LoadInt32(&mainWndInSizeMove) == 0 {
			panelReassertZ()
		}
	case wmEnterSizeMove:
		// 用户开始拖动/缩放窗口（含把最大化窗口拖出还原）：系统接着就在本次
		// WM_NCLBUTTONDOWN 处理里跑它的模态移动循环，期间窗口几何全由系统驱动。
		// 置位后其余分支的面板动作一律让路（见 mainWndInSizeMove 注释）。
		atomic.StoreInt32(&mainWndInSizeMove, 1)
		panelDragHold(true)
	case wmExitSizeMove:
		// 模态循环已退出、栈已干净：把拖动期间攒下的面板落位与 Z 序重申
		// 补做一次（面板浮层的既有作用不丢）。
		atomic.StoreInt32(&mainWndInSizeMove, 0)
		panelDragHold(false)
		panelReassertZ()
	case wmWindowPosChanged:
		if p := (*windowPosMirror)(lp); p != nil &&
			p.flags&swpNoSizeFlag != 0 && p.flags&swpNoMoveFlag != 0 &&
			atomic.LoadInt32(&mainWndInSizeMove) == 0 {
			panelReassertZ()
		}
	}
	if mainWndOrigProc == 0 {
		r, _, _ := procDefWindowProcW.Call(hwnd, msg, wp, uintptr(lp))
		return r
	}
	r, _, _ := procCallWindowProcW.Call(mainWndOrigProc, hwnd, msg, wp, uintptr(lp))
	return r
}

// panelReassertZ 主窗口激活/Z 序变化：把右栏面板提回兄弟窗口顶（Chromium
// 聚焦时会把主 WebView2 重新提到顶、压住面板——右栏白屏的根因之一）。
// panelDragHold 通知面板层「用户开始/结束拖动、缩放主窗口」（见 panelDragHoldHook）。
func panelDragHold(hold bool) {
	if f, ok := panelDragHoldHook.Load().(func(bool)); ok && f != nil {
		f(hold)
	}
}

func panelReassertZ() {
	if f, ok := panelReassertHook.Load().(func()); ok && f != nil {
		f()
	}
}

// ---------- 兼容渲染模式与前端热重启 ----------

var (
	softwareCompatPath string
	// webviewSoftwareRender 本次启动（或上次热重启后）前端是否软件合成。
	// 设置页切换的 HTTP goroutine 写、UI 线程窗口循环读，一律走原子访问。
	webviewSoftwareRender atomic.Bool
	mainWndHwnd           atomic.Uintptr            // 当前主窗口句柄（窗口循环每轮登记）
	frontendRestartReq    int32                     // 热重启请求（设置页渲染切换 / ProcessFailed 自愈）：窗口循环见位即按当前模式重建前端
	restartWndRect        atomic.Pointer[panelRect] // 热重启时沿用旧窗口位置尺寸
	procPostMessageW      = desktopUser32.NewProc("PostMessageW")
)

// restartFrontendNow 请求前端热重启（设置页「界面渲染」切换与 WebView2 崩溃
// 自愈共用）：记下旧窗口位置（重开原位）→ 向「当前活着的主窗口」投递 WM_CLOSE，
// 窗口循环见 frontendRestartReq 即按当前 webviewSoftwareRender 重建前端。
// 关键护栏：热重启存在 ~几百毫秒的重建间隙，此刻拿旧句柄 PostMessage 会
// 静默失败、切换请求丢失（标记已写而前端没换 → 状态不一致）。这里改为
// 反复读 mainWndHwnd 重试投递，直到新窗口登记后送达为止（上限 3 秒兜底）。
func restartFrontendNow() {
	if hwnd := mainWndHwnd.Load(); hwnd != 0 {
		r := &panelRect{}
		if ok, _, _ := procPanelGetWindowRect.Call(hwnd, uintptr(unsafe.Pointer(r))); ok != 0 && r.Left > -30000 {
			restartWndRect.Store(r)
		}
	}
	atomic.StoreInt32(&frontendRestartReq, 1)
	for i := 0; i < 16; i++ {
		if h := mainWndHwnd.Load(); h != 0 {
			if r1, _, _ := procPostMessageW.Call(h, 0x0010, 0, 0); r1 != 0 { // WM_CLOSE
				return // 已送达（不保证窗口还活着，但请求已入队）
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	log.Printf("前端热重启请求未能投递（3 秒内无有效主窗口）")
}

// ---------- 渲染模式查询与切换（设置页「界面渲染」） ----------

// displayMode 供 GET /api/display-mode：desktop=桌面壳是否在跑；
// software=当前前端是否软件合成（兼容模式）。兼容模式只经设置页手动切换。
func (a *App) displayMode() map[string]any {
	return map[string]any{
		"desktop":  a.shellWnd != 0,
		"software": webviewSoftwareRender.Load(),
	}
}

// setDisplayMode 供 POST /api/display-mode：写/删兼容标记并前端热重启，
// 当场切到目标模式（后端不动）。标记文件存在即兼容模式（内容无语义；
// 历史版本区分 user/auto 的 reason 字段已随黑屏自检一并移除）。
func (a *App) setDisplayMode(software bool) error {
	if a.shellWnd == 0 {
		return fmt.Errorf("仅桌面版支持渲染模式切换")
	}
	if software == webviewSoftwareRender.Load() {
		return nil // 已在目标模式：不动窗口
	}
	if software {
		_ = os.MkdirAll(filepath.Dir(softwareCompatPath), 0755)
		if err := os.WriteFile(softwareCompatPath, []byte("user\n"), 0644); err != nil {
			return fmt.Errorf("兼容标记写入失败: %w", err)
		}
	} else if err := os.Remove(softwareCompatPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("兼容标记删除失败: %w", err)
	}
	webviewSoftwareRender.Store(software)
	log.Printf("渲染模式切换（设置页）：%s，前端热重启", map[bool]string{true: "兼容模式（软件合成）", false: "GPU 渲染"}[software])
	// 响应先送达前端再关窗（400ms 缓冲），用户看到界面自动重开即生效
	time.AfterFunc(400*time.Millisecond, restartFrontendNow)
	return nil
}

// ---------- 主 WebView2 崩溃自愈（ProcessFailed） ----------

// pfArgsVtbl 镜像 ICoreWebView2ProcessFailedEventArgs（COM ABI 头 3 槽固定 +
// get_ProcessFailedKind；绑定库未导出该事件，复用右栏面板的 vtbl 手法，
// 见 browser_panel_windows.go 的 evtHandler / wvRawVtbl）。
type pfArgsVtbl struct {
	queryInterface, addRef, release edge.ComProc
	getProcessFailedKind            edge.ComProc
}

// COREWEBVIEW2_PROCESS_FAILED_KIND 只列本地要处置的取值；其余（GPU 进程、
// 工具进程、崩溃保护进程等）WebView2 会自动重启对应子进程，记日志即可。
const (
	pfBrowserExited      = 0 // 浏览器主进程退出（整个 WebView2 控件已不可用）
	pfRenderExited       = 1 // 渲染进程退出（官方枚举 1=RenderProcessExited；页面死掉，reload 即可恢复）
	pfRenderUnresponsive = 2 // 渲染进程无响应（官方枚举 2=RenderProcessUnresponsive；长任务重渲染也会触发，需容忍）
)

func pfKindName(kind int32) string {
	switch kind {
	case pfBrowserExited:
		return "浏览器进程退出"
	case pfRenderUnresponsive:
		return "渲染进程无响应"
	case pfRenderExited:
		return "渲染进程退出"
	}
	return fmt.Sprintf("其他子进程事件(%d)", kind)
}

var (
	frontendPfHandler *evtHandler // COM 侧只存裸指针，Go 侧必须持引用保活（同 browserPanel）
	frontendPfToken   int64
	pfMu              sync.Mutex
	pfRenderExitN     int   // 渲染进程退出计数（时间窗内）
	pfRenderExitSince int64 // 计数窗口起点（毫秒）
	pfUnrespN         int   // 渲染无响应计数（时间窗内）
	pfUnrespSince     int64
	frontendCrashN    []int64 // 意外关窗时间戳（风暴保护窗口）
)

// mainWebViewOf 取主窗口 WebView2 的 ICoreWebView2：反射绑定库内部
// *edge.Chromium 再经控制器 vtbl 取 CoreWebView2（releaseFrontend 同款手法；
// 绑定库改版时反射失败返回 nil，自愈缺位但主流程不受影响）。
func mainWebViewOf(w webview2.WebView) (wv *edge.ICoreWebView2) {
	defer func() { _ = recover() }()
	rv := reflect.ValueOf(w)
	if rv.Kind() != reflect.Ptr {
		return nil
	}
	f := rv.Elem().FieldByName("browser")
	if !f.IsValid() || (f.Kind() == reflect.Ptr && f.IsNil()) {
		return nil
	}
	ch, _ := reflect.NewAt(f.Type(), unsafe.Pointer(f.UnsafeAddr())).Elem().Interface().(*edge.Chromium)
	if ch == nil {
		return nil
	}
	return ctlWebView(ch.GetController())
}

// attachProcessFailed 给主 WebView2 挂 ProcessFailed 事件（每轮前端创建后调用）。
// 回调发生在 WebView2 的原生事件栈上，evtHandler 内置 recover 兜底（panic 不外泄）。
func attachProcessFailed(wv *edge.ICoreWebView2) {
	if wv == nil {
		return
	}
	frontendPfHandler = newEvtHandler(func(sender, args unsafe.Pointer) {
		handleProcessFailed((*edge.ICoreWebView2)(sender), args)
	})
	var tok int64
	_, _, _ = wvVtblOf(wv).addProcessFailed.Call(
		uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(frontendPfHandler)), uintptr(unsafe.Pointer(&tok)))
	frontendPfToken = tok
}

// detachProcessFailed 反注册（前端收尾时调用，与 attach 配对）。
func detachProcessFailed(wv *edge.ICoreWebView2) {
	if wv == nil || frontendPfHandler == nil {
		return
	}
	_, _, _ = wvVtblOf(wv).removeProcessFailed.Call(
		uintptr(unsafe.Pointer(wv)), uintptr(unsafe.Pointer(frontendPfHandler)), uintptr(unsafe.Pointer(&frontendPfToken)))
	frontendPfHandler = nil
}

// handleProcessFailed 崩溃分级处置：
//   - 浏览器主进程死：控件整体不可用 → restartFrontendNow 重建前端（后端不动）；
//   - 渲染进程死（页面死）：reload 恢复；10 分钟内 3 次则升级重建前端（防崩溃循环）；
//   - 渲染无响应：容忍一次（重渲染长任务会误报），连续 2 次才刷新；
//   - 其余子进程事件：WebView2 自动重启，仅记日志。
func handleProcessFailed(sender *edge.ICoreWebView2, args unsafe.Pointer) {
	var kind int32
	if args != nil {
		_, _, _ = (*(**pfArgsVtbl)(unsafe.Pointer(args))).getProcessFailedKind.Call(
			uintptr(args), uintptr(unsafe.Pointer(&kind)))
	}
	log.Printf("[ProcessFailed] %s", pfKindName(kind))
	now := time.Now().UnixMilli()
	pfMu.Lock()
	defer pfMu.Unlock()
	switch kind {
	case pfBrowserExited:
		log.Printf("[ProcessFailed] 浏览器进程已退出：重建前端窗口（后端与任务不受影响）")
		restartFrontendNow()
	case pfRenderExited:
		if now-pfRenderExitSince > 10*60*1000 {
			pfRenderExitSince, pfRenderExitN = now, 0
		}
		pfRenderExitN++
		if pfRenderExitN >= 3 {
			pfRenderExitN = 0
			log.Printf("[ProcessFailed] 渲染进程 10 分钟内死亡 3 次：重建前端（防崩溃循环）")
			restartFrontendNow()
			return
		}
		log.Printf("[ProcessFailed] 渲染进程死亡：重新加载界面")
		reloadWebView(sender)
	case pfRenderUnresponsive:
		if now-pfUnrespSince > 5*60*1000 {
			pfUnrespSince, pfUnrespN = now, 0
		}
		pfUnrespN++
		if pfUnrespN >= 2 {
			pfUnrespN = 0
			log.Printf("[ProcessFailed] 渲染进程持续无响应：刷新界面恢复")
			reloadWebView(sender)
		}
	}
}

// reloadWebView 经 vtbl 调 ICoreWebView2::Reload（绑定库未导出；事件回调里的
// sender 即主 WebView 的 ICoreWebView2，直接可用）。
func reloadWebView(wv *edge.ICoreWebView2) {
	if wv == nil {
		return
	}
	_, _, _ = wvVtblOf(wv).reload.Call(uintptr(unsafe.Pointer(wv)))
}

// frontendCrashAllowRebuild 意外关窗的风暴保护：10 分钟窗口内意外重建不足 5 次
// 返回 true；达到上限则弹窗告知一次并重置计数（继续重建但不再静默——
// WebView2 运行时真损坏时用户至少知道发生了什么，进程保持驻留托盘）。
func frontendCrashAllowRebuild() bool {
	now := time.Now().UnixMilli()
	kept := frontendCrashN[:0]
	for _, t := range frontendCrashN {
		if now-t < 10*60*1000 {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	frontendCrashN = kept
	if len(kept) < 5 {
		return true
	}
	frontendCrashN = nil
	go desktopMessageBox(
		"界面在短时间内多次意外关闭（通常是 WebView2 运行时或显卡驱动异常）。\n\n"+
			"程序仍驻留后台（右下角托盘图标在），会话与运行中的任务不受影响；已继续尝试重开界面。\n\n"+
			"如反复出现，请把安装目录 data\\desktop.log 反馈给我们排查。", desktopAppTitle)
	return false
}
