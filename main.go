// 坚祥数字助理（GO版）—— 单进程本地智能助手服务。
//
// 架构：Go 后端（LLM 主循环 + 工具集 + 会话存储）+ 原生 JS 前端，
// 前后端仅通过 HTTP / SSE 通信，不共享任何运行时模块。
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"jxassistant/sandboxhelper"
)

// desktopDefault 由打包脚本注入（-ldflags -X）：=1 时默认桌面窗口形态。
var desktopDefault string

// androidPathAlias 在 /data/data/<pkg>/... 与 /data/user/0/<pkg>/... 两种同一
// 目录的路径写法之间互换（应用层与内核 getcwd 各报一种，字面比较会漏判）。
func androidPathAlias(p string) string {
	if strings.HasPrefix(p, "/data/data/") {
		return "/data/user/0/" + strings.TrimPrefix(p, "/data/data/")
	}
	if strings.HasPrefix(p, "/data/user/0/") {
		return "/data/data/" + strings.TrimPrefix(p, "/data/user/0/")
	}
	return p
}

func main() {
	// 安卓 x86_64：modernc sqlite 的 lstat 会被应用 seccomp 杀死（SIGSYS），
	// 必须在首次打开数据库前替换为 newfstatat 实现（其他平台空操作）。
	patchSqliteSyscallsForAndroid()
	// 安卓：父进程（应用壳）死亡时内核自动回收引擎，杜绝孤儿占端口
	// 导致的下一次启动绑定失败死循环（其他平台空操作）。
	setupParentDeathSignal()

	// 顶层进程树兜底：自身挂入 kill-on-close Job，正常退出 / 被强杀 /
	// 崩溃时内核终止它启动过的全部后代（含沙箱外 start 出来的孙进程、
	// chromedp 浏览器、技能 python）——不留无人认领的后台残留。
	if err := setupKillOnCloseJob(); err != nil {
		log.Printf("顶层进程树兜底未启用: %v", err)
	}

	// 本地 Linux 沙箱的自重入口：主进程以隐藏参数重启自身施加 Landlock
	// 限制，随后 exec 目标命令（Go 无法 fork，这是最干净的进程级限制路径）。
	if len(os.Args) > 1 && os.Args[1] == "__jx-sandbox-restrict" {
		os.Exit(sandboxhelper.Main(os.Args[2:]))
	}
	// 沙箱常驻授权审计/清理子命令：jx-assistant.exe sandbox acl-audit
	if len(os.Args) > 1 && os.Args[1] == "sandbox" {
		attachParentConsole() // GUI 子系统构建时挂回终端，保证 CLI 回显
		os.Exit(runSandboxCLI(os.Args[2:]))
	}
	// APK 元数据查看子命令：jx-assistant.exe apkinfo <file.apk>
	if len(os.Args) > 1 && os.Args[1] == "apkinfo" {
		attachParentConsole()
		os.Exit(runApkInfoCLI(os.Args[2:]))
	}
	// 内置运行时自检子命令（安卓故障诊断）：jx runtime-selftest [dataDir]
	if len(os.Args) > 1 && os.Args[1] == "runtime-selftest" {
		attachParentConsole()
		os.Exit(runRuntimeSelftest(os.Args[2:]))
	}

	cfgPath := flag.String("config", "config.json", "配置文件路径")
	port := flag.Int("port", 0, "覆盖配置中的监听端口")
	noOpen := flag.Bool("noopen", false, "启动后不自动打开浏览器")
	webDir := flag.String("webdir", "", "开发模式：前端从该磁盘目录提供（默认用内嵌副本）")
	desktop := flag.Bool("desktop", false, "桌面模式：用系统 WebView2 开原生窗口（不开浏览器）")
	browser := flag.Bool("browser", false, "强制浏览器模式（覆盖安装版默认的桌面模式）")
	electron := flag.Bool("electron", false, "Electron 壳 sidecar 模式：不开窗口不开浏览器，由 Electron 主进程拉起（目录锚定与工作区迁移同桌面模式）")
	flag.Parse()

	// 安装包构建时注入（-ldflags "-X main.desktopDefault=1"）：发行版
	// 双击即开桌面窗口；开发版（jx-assistant.exe）不注入，保持浏览器形态。
	wantDesktop := *desktop || desktopDefault == "1"

	// Electron sidecar 与桌面模式共享环境准备（cwd 锚定 / 默认工作区迁移 /
	// 禁开浏览器），但绝不启动 WebView2 窗口——窗口由 Electron 主进程持有，
	// 本进程只当本地 HTTP 服务跑。
	anchored := (wantDesktop && !*browser) || *electron

	// 桌面/安装版：cwd 由快捷方式「起始位置」决定、不可靠，把工作目录
	// 锚到 exe 旁，config/data/技能 等相对路径全部随之落位。开发目录
	// （有 go.mod，含 go run 的临时目录场景之外）不搬迁，避免扰动调试。
	if anchored {
		if _, err := os.Stat("go.mod"); err != nil {
			if exe, err := os.Executable(); err == nil {
				if err := os.Chdir(filepath.Dir(exe)); err != nil {
					log.Printf("切换到安装目录失败（按当前目录继续）: %v", err)
				}
			}
		}
		*noOpen = true
	}

	// GUI 子系统构建（-H=windowsgui）双击启动时没有控制台，日志改落
	// data\desktop.log；放在 cwd 锚定之后，日志与数据同目录
	guiLogRedirect()

	cfg, err := LoadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("加载配置失败: %v", err)
	}
	var legacyWS string // 被迁移掉的旧默认工作区目录（空串 = 没有迁移）
	if anchored {
		// 默认工作区规整：落在安装目录下的「默认工作区」，旧版的「工作区」
		// 原地改名迁移过来；有改写就立即回写，设置页显示与实际保持一致
		var wsChanged bool
		legacyWS, wsChanged = desktopWorkspaceDefault(cfg)
		if wsChanged {
			if serr := cfg.Save(*cfgPath); serr != nil {
				log.Printf("回写默认工作区失败: %v", serr)
			}
		}
	}
	// 安卓：工作区锚定迁移——旧版默认工作区 = 引擎根目录（config/data/运行时
	// 全泡在里面）；现默认收进 workspace/ 子目录。手机端从不自选工作目录，
	// 旧值等于引擎根目录的必为默认值，配置与挂旧路径的历史会话一并改指。
	// 注意 /data/data/<pkg> 与 /data/user/0/<pkg> 是同一目录的两种写法（应用
	// 层混用），比较前按别名互换归一，否则迁移静默漏判。
	var androidLegacyWS string
	if isAndroidRuntime {
		if cwd, cerr := os.Getwd(); cerr == nil {
			if cfg.Workspace == cwd || cfg.Workspace == androidPathAlias(cwd) {
				androidLegacyWS = cfg.Workspace
				cfg.Workspace = filepath.Join(cwd, "workspace")
			}
		}
		_ = os.MkdirAll(cfg.Workspace, 0755) // 工作区在位（含全新安装）
		if androidLegacyWS != "" {
			if serr := cfg.Save(*cfgPath); serr != nil {
				log.Printf("回写安卓工作区锚定失败: %v", serr)
			}
		}
	}
	// -port 只覆盖本次监听，不写进 cfg.Port：否则运行期任何一次保存配置
	// （如设置页改专家/提供方）都会把临时端口永久落盘，改变下次启动行为
	listenPort := cfg.Port
	if *port > 0 {
		listenPort = *port
	}

	app, err := NewApp(cfg, *cfgPath, *webDir)
	if err != nil {
		log.Fatalf("初始化失败: %v", err)
	}

	// Electron 壳 sidecar：发现面板桥（环境变量由 Electron 主进程注入）即挂
	// 为内嵌浏览器目标——browser_* 工具经桥驱动壳内 <webview> 右栏面板。
	// 网页/旧桌面壳不带变量，nativePanel 维持 nil，形态不变。
	if ep := newElectronPanelFromEnv(app); ep != nil {
		app.nativePanel = ep
		log.Printf("Electron 面板桥已接入: %s", ep.base)
	}

	// 默认工作区迁移的收尾：历史会话里显式挂过旧默认目录（安装目录\工作区）
	// 的，一并改指新目录——升级后不会有会话悬空指向已改名的文件夹
	if legacyWS != "" {
		if n, merr := app.Store.RepointWorkspace(legacyWS, cfg.Workspace); merr != nil {
			log.Printf("历史会话工作目录改指失败（%s）: %v", legacyWS, merr)
		} else if n > 0 {
			log.Printf("默认工作区迁移：%d 个会话的工作目录改指 %s", n, cfg.Workspace)
		}
	}

	// 安卓工作区锚定迁移的收尾：挂旧默认目录（引擎根）的历史会话改指 workspace/
	if androidLegacyWS != "" {
		aliases := []string{androidLegacyWS, androidPathAlias(androidLegacyWS)}
		repointed := 0
		for _, old := range aliases {
			if n, merr := app.Store.RepointWorkspace(old, cfg.Workspace); merr != nil {
				log.Printf("安卓历史会话工作目录改指失败（%s）: %v", old, merr)
			} else if n > 0 {
				repointed += n
			}
		}
		if repointed > 0 {
			log.Printf("安卓工作区锚定迁移：%d 个会话的工作目录改指 %s", repointed, cfg.Workspace)
		}
	}

	// 安卓：注册内置运行时（Termux 快照）并后台预热装配（不阻塞启动/服务就绪）
	if isAndroidRuntime {
		setTermuxProvider(func(emit func(string)) (string, error) { return termuxEnsureFor(app, emit) })
		go func() {
			if p := termuxPrefixOrEmpty(); p != "" {
				log.Printf("[runtime] 内置运行时就绪: %s", p)
			}
		}()
	}

	// 历史密级沙箱残留还原：旧版给工作区打过常驻 Low 完整性标签（机制已
	// 撤除），启动时自动还原为 Medium（异步；根级签名校验防误伤他人标签）。
	go app.startupIntegrityLabelRestore(cfg.Workspace)
	go app.startupPySessionCleanup(cfg.Workspace) // run_python 会话脚本目录的按天兜底回收（run_python.go）

	host := cfg.BindHost
	if host == "" {
		host = "127.0.0.1"
	}
	// 先占端口再起服务：桌面模式端口被占（如浏览器模式实例还在跑）时
	// 自动回退到系统分配的可用端口，而不是直接退出。
	ln, err := net.Listen("tcp", fmt.Sprintf("%s:%d", host, listenPort))
	if err != nil && wantDesktop && !*browser {
		ln, err = net.Listen("tcp", host+":0")
	}
	if err != nil {
		log.Fatalf("监听端口失败: %v", err)
	}
	portNo := ln.Addr().(*net.TCPAddr).Port
	// 通配绑定（0.0.0.0 / ::，供局域网手机访问）时，本机浏览器与桌面
	// WebView2 继续用 127.0.0.1 直达；局域网地址另行枚举打印给手机用。
	dialHost := host
	wildcard := host == "0.0.0.0" || host == "::" || strings.EqualFold(host, "[::]")
	if wildcard {
		dialHost = "127.0.0.1"
	}
	url := "http://" + net.JoinHostPort(dialHost, strconv.Itoa(portNo))
	srv := &http.Server{Handler: withCORS(app.Routes())}

	// 远程接入（公网中继）：注册到 210 网关的 /hub，桥接目标就是本监听端口。
	// 连接按配置即时启动；设置页的开关/身份/网关改动同样热生效。
	app.listenPort = portNo
	app.syncRemoteAccess()
	// 更新：先结算上一次 engine 静默交换（确认版本/清 .old 残留/恢复横幅状态），
	// 再结算技能区（清换位残留的旧运行时、把与当前版本不符的旧运行时清掉），
	// 最后启动轮询
	app.FinalizeEngineSwap()
	app.FinalizeRuntimeState()
	StartUpdateChecker(app)
	startCompanyResyncOnce(app) // 一次性重新接入：把 10-16 之后没再跑过向导的机器模型配置追平
	StartRunSweepOnce(app)      // 启动 30s 后补扫一次孤儿会话（只扫一次，不做持续轮询）

	if wantDesktop && !*browser {
		// 服务跑在后台 goroutine；窗口关闭即整体退出
		go func() {
			if serr := srv.Serve(ln); serr != nil && serr != http.ErrServerClosed {
				log.Printf("HTTP 服务退出: %v", serr)
				os.Exit(1)
			}
		}()
		// 启动行带上 Go 工具链版本：现场拿 data\desktop.log 排查「闪退/诡异
		// 行为」时，能直接看出该安装包是哪个工具链构建的（回调栈缺陷等运行
		// 时 bug 与 Go 版本强相关）。
		log.Printf("AI数字助理 桌面版已启动: %s（%s）", url, runtime.Version())
		// 把独立语音输入法作为子进程拉起来（幂等；找不到/已在跑都只记日志）。
		// 放在 runDesktop 之前：窗口一出来用户就能用听写，不必再手动双击 bat。
		app.spawnVoiceType()
		defer stopVoiceType() // 主程序退出时一并收掉子进程，不留孤儿
		if derr := runDesktop(app, url); derr != nil {
			log.Fatalf("桌面模式启动失败: %v", derr)
		}
		app.shutdownJobs() // 退出主动收口：停后台作业+自检日志（内核兜底前的第一道收口）
		log.Printf("桌面窗口已关闭，进程退出")
		os.Exit(0)
	}

	log.Printf("坚祥数字助理（GO版）已启动: %s", url)
	if len(cfg.Providers) == 0 {
		log.Printf("模型: 还没有模型通道（在「设置 · 模型」添加提供方，或运行「公司接入向导」）")
	} else {
		t := app.DefaultTarget()
		log.Printf("模型: %s @ %s（baseURL 填 mock 时为离线演示模式）", t.Model, t.BaseURL)
	}
	if *webDir != "" {
		log.Printf("开发模式：前端从 %s 读取，修改后刷新浏览器即生效", *webDir)
	}
	if wildcard {
		for _, ip := range lanIPv4s() {
			log.Printf("局域网访问（手机连同一 Wi-Fi 后用浏览器打开）: http://%s",
				net.JoinHostPort(ip, strconv.Itoa(portNo)))
		}
		log.Printf("手机连不上时：在 Windows 防火墙弹窗点「允许访问」，或为 jx-assistant.exe 添加专用网络入站规则")
	}

	if !*noOpen && (dialHost == "127.0.0.1" || dialHost == "localhost") {
		go func() {
			time.Sleep(400 * time.Millisecond)
			openBrowser := exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
			hideConsoleWindow(openBrowser) // 借用系统外壳打开浏览器，别闪控制台窗
			_ = openBrowser.Start()
		}()
	}

	if err := srv.Serve(ln); err != nil {
		app.shutdownJobs() // 异常退场也先收口（Fatal 前最后一步）
		log.Fatalf("服务退出: %v", err)
	}
	app.shutdownJobs() // 正常收尾：停后台作业+自检日志（内核兜底前的第一道收口）
}

// lanIPv4s 枚举本机非回环、非链路本地（169.254.*）的 IPv4 地址——局域网内
// 其他设备（手机等）访问本服务时该填的地址。198.18.0.0/15 是代理工具
// （Clash fake-IP 等）虚拟网卡的常用段，列出来只会误导，一并滤掉。
func lanIPv4s() []string {
	out := []string{}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return out
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			ip4 := ipn.IP.To4()
			if ip4 != nil && !ip4.IsLoopback() && !ip4.IsLinkLocalUnicast() &&
				!(ip4[0] == 198 && ip4[1]&0xfe == 18) {
				out = append(out, ip4.String())
			}
		}
	}
	return out
}
