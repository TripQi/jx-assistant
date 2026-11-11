package main

import (
	"bytes"
	"context"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var appVersion = "20261013-1" // 版本号体系：日期-序号（同日发多版用 -2 -3；旧写法 20261010V2 由 update.go 兼容识别）。打包脚本经 -X main.appVersion 注入实际版本，保证与安装包一致

// App 持有配置与会话存储，是所有 HTTP 处理器的宿主。
type App struct {
	Cfg              *Config
	CfgPath          string
	Store            *Store
	Ctx              *ContextEngine       // 上下文引擎：组装请求材料 + 上下文事件总线（contextengine.go）
	Plugins          *PluginRegistry      // DSH 插件兼容层实现（adapters.go：插件兼容层/ + adapter_*.go）
	Layers           *PluginLayerRegistry // 插件兼容层注册表：主体发现的全部插件体系（pluginlayers.go）
	Components       *ComponentRegistry
	SessionCX        *sessionCXRegistry  // 会话级组件/插件开关覆盖（sessioncx.go：优先于全局开关，按会话存）
	CCPlugins        *CCPluginRegistry   // Claude Code 插件兼容层实现（ccplugin*.go）
	MCPServers       *MCPServersRegistry // 独立 MCP 服务器注册表（mcpservers*.go）：用户直连 MCP 服务
	Todos            *todoScheduler      // 待办与定时任务：组件 组件/todo-scheduler 的宿主半边（todo.go）
	Bridge           *bridgeManager      // 声明式组件桥（componentbridge.go）：manifest workers 的通用宿主
	static           fs.FS               // 前端静态资源：发布用内嵌副本，开发（-webdir）指向磁盘目录
	apiMux           *http.ServeMux      // call_api 进程内直调用的完整路由缓存（callapi.go，懒建）
	apiMetas         []apiMeta           // 对 AI 开放的接口目录（注册顺序，callapi.go）
	apiMetaByPattern map[string]apiMeta  // pattern→ 元数据（callapi.go，与 apiMetas 同源）
	apiCatalogCache  string              // 完整目录文本的渲染缓存（注册表启动后不变，callapi.go）
	apiMetaMu        sync.Mutex          // 上述字段的锁（registerRoutes 期间不与 a.mu 互斥）

	mu                  sync.Mutex
	sessLocks           map[string]*sync.Mutex
	compactMu           sync.Mutex                   // 规则自动压缩的 token 用量基线锁（context.go）
	compactUsage        map[string]*compactUsageMark // 会话id → 最近 usage 与压缩基线（间隔制 token 模式的触发依据，context.go）
	compactRunMu        sync.Mutex                   // 手动模型摘要压缩的会话级互斥锁（server.go）
	compactRunning      map[string]bool              // 会话id → 后台模型摘要进行中（拦截重复点火；前端「后台压缩中」状态同源）
	runMu               sync.Mutex
	runners             map[string]*sessionRun // 正在执行的会话（含后台派发）
	permMu              sync.Mutex
	grants              map[string]*Grant        // 授权凭证（perm.go）
	approvals           map[string]*Approval     // 等待用户决定的越权审批（perm.go）
	permNotes           map[string]string        // 待落快照的「权限变更」注记（perm.go，env 通道）
	observedFiles       sync.Map                 // 会话内已观察文件集（观察策略，perm.go）
	skillGuideOnce      sync.Map                 // 会话内已随工具结果附带过 SKILL.md 指南的技能（key: 会话id\x00技能id，tool_docs.go 的 appendSkillGuide）
	questions           map[string]*UserQuestion // 等待用户回答的提问/计划审批（interact.go）
	jobsMu              sync.Mutex
	jobs                map[string]*jobRun // 会话内后台作业（jobs.go）
	bridgeMu            sync.Mutex
	bridgeTokens        map[string]*bridgeToken // run_python 工具桥的一次性凭证（bridge.go）
	bridgeWaits         map[string]*bridgeWait  // 桥派子任务的回报收集表（bridge.go）
	protectPaths        []string                // 助手自身配置/数据目录，任何模式禁访问
	skillsMu            sync.Mutex
	skillsReady         bool   // 技能母本是否已释放（SkillsDir 首次解析后置位）
	skillsDirOurs       string // 已解析的技能目录（App 级缓存，避免跨实例污染）
	marketMu            sync.Mutex
	marketCache         marketCacheEntry // 市场清单拉取缓存（60s，见 market.go）
	remoteMu            sync.Mutex
	remotes             map[string]*remoteClient // SSH 远程连接池（remote.go）
	listenPort          int                      // 本机监听端口（远程中继的桥接目标，热重启连接用）
	remoteCtlMu         sync.Mutex
	remoteCtlCancel     context.CancelFunc // 中继连接存活句柄（nil=未运行，remoteaccess.go）
	shellEpochMu        sync.Mutex
	shellEpoch          int64 // 远程外壳版本代数：「关于」页一键清缓存按钮递增（remoteaccess.go，落盘 data/shell-epoch.json）
	evtMu               sync.Mutex
	subscribers         map[chan Event]bool // /api/events 的订阅者
	syncFailMu          sync.Mutex
	syncFails           int // 公司服务端连续失联轮数（sync.go：连续 2 轮自动停用公司通道）
	pendMu              sync.Mutex
	pendingSteer        map[string][]*PendingMsg // 执行中会话的插话（下一次调用模型前生效）
	pendingQueue        map[string][]*PendingMsg // 执行中会话的排队（本轮结束后作为独立一轮发送）
	queuePaused         map[string]bool          // 会话队列是否暂停（暂停后本轮结束不自动续发；恢复时空闲即接着发）
	errMu               sync.Mutex
	runErrors           map[string]string // 会话最近一次失败原因（前端输入框上方报错条 + 重试）
	stopWatchdog        time.Duration     // 停止看门狗：协作取消后等多久仍未收口就强制终结（runtime.go watchStop）
	streamKillMu        sync.Mutex
	streamKills         map[string]func() // 会话 → 在跑模型流的强制断开器（llm.go 注册，停止看门狗硬终结时调用）
	usageCh             chan usageRow     // 用量明细的异步落库队列（usagestats.go：流式热路径只入队，写库在后台协程）
	osOnce              sync.Once
	osDesc              string // 环境快照用的系统描述（Windows 版本 + PowerShell 版本，探测一次缓存）
	selfOnce            sync.Once
	selfExe             string // 引擎程序绝对路径（os.Executable，探测一次缓存；系统提示词用）
	skillRTPrompt       string // 内置技能运行时段（技能/_runtime/python 绝对路径，探测一次缓存；系统提示词用）
	browserMu           sync.Mutex
	browsersEmbedded    map[string]*BrowserHost  // 无头 chromedp 宿主：网页形态的内嵌目标，按会话隔离（""=共享；browser_host.go，惰性创建）
	browsersHeaded      map[string]*BrowserHost  // 有头独立窗口宿主：按会话隔离（""=共享；browser_host.go，惰性创建）
	nativePanel         browserTarget            // Electron 壳的右栏 <webview> 面板（单例，browser_panel_electron.go）；原生桌面壳/网页形态为 nil
	browserActive       map[string]browserTarget // 工具作用目标：每会话最近一次 browser_open 选定的实例（键=browserProfileKey）
	browserUserClosedAt map[string]time.Time     // 用户在右栏 ✕ 关闭浏览器页面的时刻，按会话记账（键=browserProfileKey，browserMu 保护）：browser_* 报「未在运行」时据此区分为用户主动关闭（非崩溃）；browserFor 重新打开时清零

	// 原生桌面壳（desktop_windows.go）的会话面板集合：右栏浏览器每个会话
	// 一个独立 WebView2 实例与 profile（登录态/页面互不串台，与有头独立
	// 窗口的会话隔离同款约定）。panelDispatch/panelMainWnd 是当前前端窗口
	// 的 UI 线程投递与 HWND（每轮前端热重启时整体换新）；panelVisible 记
	// 当前显示中的面板键——同一时刻只显示一个会话的面板，其余收屏外。
	// 非桌面形态（网页/Electron）这些字段全为零值，不参与路由。
	panelMu       sync.Mutex
	panelDispatch func(f func())
	panelMainWnd  uintptr
	panels        map[string]browserTarget // 键=browserProfileKey（""=共享面板，兼容无会话上下文的入口）
	panelVisible  string

	// 远程帧镜像缓存（handleBrowserFrame）：右栏浏览器画面按会话缓存最后一帧
	// （内容指纹去重 + 最小间隔节流），远程端轮询命中缓存就不再截图。
	frameMu    sync.Mutex
	frameCache map[string]*browserFrameEntry // 键=browserProfileKey

	// 桌面壳（runDesktop）注入的右侧栏截图通道（panelshot_windows.go）：
	// shellEval 往主界面注入 JS、panelViewCh 收回右栏矩形回传，shellWnd 是
	// 主窗口 HWND（客户区原点换算屏幕坐标用）。非桌面形态全为零值。
	shellEval   func(string)
	shellWnd    uintptr
	panelViewCh chan panelViewReport
}

func NewApp(cfg *Config, cfgPath, webDir string) (*App, error) {
	store, err := NewStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	static, err := fs.Sub(webFS, "web")
	if err != nil {
		return nil, err
	}
	if webDir != "" {
		if info, err := os.Stat(webDir); err != nil || !info.IsDir() {
			return nil, fmt.Errorf("webdir 不可用（需为存在的目录）: %s", webDir)
		}
		static = os.DirFS(webDir)
	}
	plugins := InitPlugins("插件兼容层", "插件", filepath.Join(cfg.DataDir, "plugins.json"))
	status := plugins.List()
	adapted := 0
	for _, s := range status {
		if s.Adapted {
			adapted++
		}
	}
	log.Printf("[插件] 适配器 %d 个（启用 %d），未适配原件 %d 个", adapted, len(plugins.Assets()), len(status)-adapted)
	components := InitComponents("组件", filepath.Join(cfg.DataDir, "components.json"))
	nComp := 0
	for _, c := range components.List() {
		if c.Enabled {
			nComp++
		}
	}
	log.Printf("[组件] 发现 %d 个（启用 %d）", len(components.List()), nComp)
	a := &App{Cfg: cfg, CfgPath: cfgPath, Store: store, Plugins: plugins, Components: components, static: static,
		sessLocks: map[string]*sync.Mutex{},
		grants:    map[string]*Grant{}, approvals: map[string]*Approval{},
		permNotes: map[string]string{},
		questions: map[string]*UserQuestion{}, jobs: map[string]*jobRun{},
		remotes: map[string]*remoteClient{}}
	// 会话级组件/插件覆盖（sessioncx.go）：从各会话的附属数据载入
	a.SessionCX = loadSessionCX(store)
	a.Ctx = &ContextEngine{app: a} // 上下文引擎：所有会话经它组装与记事件
	// Claude Code 插件兼容层：加载已安装插件，后台连接 MCP 服务器、装载 mods
	// （失败记日志不阻断）
	a.CCPlugins = InitCCPlugins(a, cfg.DataDir)
	a.CCPlugins.MCPStartAll()
	a.CCPlugins.ModsStartAll()
	// 独立 MCP 服务器注册表（mcpservers*.go）：只载清单不连接——连接器
	// 懒启动，模型首次调用或用户显式测试/启用时才拉起进程（启动期零
	// MCP 进程；与技能同位而非与插件同位）
	a.MCPServers = InitMCPServers(a, cfg.DataDir)
	// 主体发现插件兼容层：各层（DSH、Claude Code…）经 registerPluginLayer
	// 自注册，主体只按 PluginLayer 契约调用——统一清单与总开关走 /api/plugins，
	// 主体不认识任何具体插件格式
	a.Layers = discoverPluginLayers(a)
	layerNames := []string{}
	for _, l := range a.Layers.Layers() {
		layerNames = append(layerNames, l.Info().Name)
	}
	log.Printf("[插件] 主体发现 %d 个插件兼容层：%s", len(layerNames), strings.Join(layerNames, "、"))
	// 受保护路径与各运行时注册表的初始化统一在 initRuntime（见 runtime.go）
	a.initRuntime()
	// 技能可见性全模式契约自检：任何模式丢了技能清单注入或 read_skill，
	// 直接拒绝启动（见 skill_manager.go 的 assertSkillVisibility）
	a.assertSkillVisibility()
	// 工作模式清单自检：chatModes（人工新建会话的可选模式）里每个值都必须
	// 是已知会话模式——前端下拉与 dispatch_task 的 mode 取值都照这份清单，
	// 出现建不出来的模式即拒绝启动（见 chatmodes.go 的 assertChatModes）
	assertChatModes()
	// 求助通道自检：request_manager 只挂执行模式，数字助理/aux 不得有（见
	// requestmgr.go 的 assertManagerChannel）
	a.assertManagerChannel()
	// 迁移任务（Flyway 式账本）：一次性清理/搬家在这里跑，执行过记账不再跑
	a.runMigrations()
	a.recoverInterrupted()
	// 交付队列崩溃残留兜底：上一进程若死在执行中（running），复位回 pending
	// 并统一重放——登记过的交付不能因为重启就丢
	if n := a.Store.ResetStuckJobs(); n > 0 {
		log.Printf("[交付] 启动恢复：%d 条中断的交付作业已复位待执行", n)
	}
	go a.flushDeliveries("")
	// 待办与定时任务：载入清单并起调度循环（组件关掉时整拍跳过，见 todo.go）
	a.Todos = newTodoScheduler(a)
	// 声明式组件桥（componentbridge.go）：manifest 声明 workers 的组件后端通用宿主
	a.Bridge = newBridgeManager(a)
	// 演示记录器随录随转：把语音组件的本地识别 worker 接给录制器（voiceasr.go；
	// 组件停用时调用报错、优雅降级为纯录音）
	recorderASR = a.transcribeWavBytes
	// 文件快照保留策略：按天超龄清理 + 按会话限量驱逐（见 snapshot.go）
	go a.snapshotJanitor()
	// 用量统计的明细落库协程 + 保留策略（见 usagestats.go）：组件关掉时不记录，
	// 队列照常空转，已有数据不动
	a.startUsageWriter()
	go a.usageJanitor()
	// 会话浏览器 profile 的按天回收（见 browser_cleanup.go）：启动先跑一轮，之后每天
	go a.browserProfileJanitor()
	// 公司接入自动同步（sync.go）：跟随服务端变化（模型/公司名/地址迁移）
	go startCompanySyncer(a)
	// 公司通道 内网/外网 状态翻转的通知（llm.go setAltPreferred）：广播 providers
	// 事件，模型菜单徽标与设置页状态即时刷新（不挂则要等前端下次拉取才跟上）。
	altPrefNotify = func(providerID string, preferAlt bool) {
		if providerID != wizardProviderID {
			return
		}
		a.broadcast(Event{Type: "providers"})
	}
	// 远程外壳版本代数恢复（一键清缓存的计数，remoteaccess.go）
	a.loadShellEpoch()
	return a, nil
}

// sessionModes 是会话模式全集（单一事实源）：建会话校验（handleCreateSession）
// 与启动自检（assertSkillVisibility）共用。新增模式必须加进本列表——自检会
// 逐模式强制「技能清单注入 + read_skill 在场」的全模式契约，漏加即启动炸响。
var sessionModes = []string{"assistant", "aux", "office", "coding", "beginner-coding", "task", "skill-create"}

// modeKnown 判断模式是否在 sessionModes 全集里。
func modeKnown(m string) bool {
	for _, x := range sessionModes {
		if x == m {
			return true
		}
	}
	return false
}

// sessLock 保证同一会话同一时刻只有一轮主循环在运行；
// 不同会话（含数字助理派发的子会话）互不阻塞。
func (a *App) sessLock(id string) *sync.Mutex {
	a.mu.Lock()
	defer a.mu.Unlock()
	l, ok := a.sessLocks[id]
	if !ok {
		l = &sync.Mutex{}
		a.sessLocks[id] = l
	}
	return l
}

func (a *App) Routes() http.Handler {
	mux := http.NewServeMux() // 前端每次新建；call_api 直调走 apiMuxOf 的缓存
	a.registerRoutes(mux)
	return mux
}

// registerRoutes 注册全部 HTTP 路由。前端（Routes，每次新建）与 call_api 的
// 进程内直调（apiMuxOf，缓存复用）共用同一份注册，行为完全一致；对 AI 开放
// 的接口用 a.apiRoute 带元数据注册（目录即注册的副产品，见 callapi.go）。
func (a *App) registerRoutes(mux *http.ServeMux) {
	// mux 由调用方传入：前端 Routes() 与 call_api 的 apiMuxOf() 共用本注册
	a.apiRoute(mux, "GET /api/status", a.handleStatus, apiMeta{Summary: "服务状态（版本、运行形态、远程接入、更新等）", Perm: permReadOnly})
	mux.HandleFunc("GET /api/display-mode", a.handleGetDisplayMode)
	mux.HandleFunc("POST /api/display-mode", a.handleSetDisplayMode)
	a.apiRoute(mux, "GET /api/search/title", a.handleSearchTitle, apiMeta{Summary: "按标题搜会话", Params: "q=关键词；scope 可选范围", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/search/content", a.handleSearchContent, apiMeta{Summary: "按内容搜会话", Params: "q=关键词", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/config", a.handleGetConfig, apiMeta{Summary: "读全局配置（Key 已掩码）", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/config", a.handleSetConfig, apiMeta{Summary: "改全局配置", Params: "要改的字段（providers 之外的一般项）", Perm: permFull, Danger: "修改全局配置，影响所有会话"})
	// 语音输入法联动（voicetype_link.go）：把听写快捷键与主题色写进 voicetype.json，
	// 独立语音输入法（VoiceTypeGo）读它——它读不到本页 localStorage，也不该读 config.json。
	a.apiRoute(mux, "POST /api/voicetype/link", a.handleVoicetypeLink, apiMeta{Summary: "同步听写快捷键/主题色给独立语音输入法", Params: "main、sub（键名数组）、accentColor、textColor，均可选", Perm: permReadOnly})
	// 最近语音记录（voicetype_history.go）：输入法落档的原始语音+文字+结果，
	// 设置页「语音」底下展示，支持试听/复制/重新识别。
	a.apiRoute(mux, "GET /api/voicetype/history", a.handleVoicetypeHistory, apiMeta{Summary: "最近语音记录（独立输入法 history.json）", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/voicetype/history/wav", a.handleVoicetypeHistoryWav, apiMeta{Summary: "第 i 条记录的原始语音（wav，试听）", Params: "i=序号", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/voicetype/history/reidentify", a.handleVoicetypeHistoryReidentify, apiMeta{Summary: "重新识别第 i 条记录", Params: "i=序号；输入法在跑则交它做（走完整分发）", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/models", a.handleModels, apiMeta{Summary: "模型清单（全部渠道）", Perm: permReadOnly})
	// 用户记忆（组件 user-memory，usermemory.go）：设置页与长期助理（call_api
	// 整文件通道）共用；未启用时 404。
	a.apiRoute(mux, "GET /api/user-memory", a.handleUserMemoryGet, apiMeta{Summary: "读用户记忆（用户称呼/身份与喜好/AI 名字/AI 身份/对话气泡颜色）", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/user-memory", a.handleUserMemorySet, apiMeta{Summary: "按字段保存用户记忆（未给出的字段保持不变，各字段上限 1000 字自动截断）", Params: "userName、userMemory、aiName、aiIdentity、bubbleColor 均可选", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/llm/ask", a.handleLLMAsk, apiMeta{Summary: "独立单轮模型调用（一次请求一次输出，不进会话上下文）", Params: "system、user、session、purpose 可选（跟随该会话的模型；purpose=title 记为标题生成）", Perm: permWorkspace})
	// 用量统计（组件 usage-stats，usageapi.go）：设置 → 用量 页的数据源。
	// 读类与设置页同为只读档；改单价与清理数据落到修改工作区档（清理不可恢复）。
	a.apiRoute(mux, "GET /api/usage/summary", a.handleUsageSummary, apiMeta{Summary: "用量总览（KPI + 每日趋势 + 按模型统计）", Params: "from、to（YYYY-MM-DD）或 date=today|7d|30d；provider、key 可选", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/usage/records", a.handleUsageRecords, apiMeta{Summary: "用量明细（分页，新的在前）", Params: "同上 + limit、offset", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/usage/pricing", a.handleUsagePricingGet, apiMeta{Summary: "模型单价表与渠道倍率（含渠道/Key 清单）", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/usage/pricing", a.handleUsagePricingSet, apiMeta{Summary: "保存模型单价表与渠道倍率（整体替换）", Params: "models、multipliers", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/usage/cleanup", a.handleUsageCleanup, apiMeta{Summary: "按日期范围删除用量明细", Params: "from、to", Perm: permWorkspace, Danger: "删除该日期范围内的用量明细，不可恢复"})
	a.apiRoute(mux, "GET /api/modes", a.handleChatModes, apiMeta{Summary: "工作模式清单（新建会话可选的模式）", Perm: permReadOnly}) // 工作模式清单（chatmodes.go）：模式下拉与 dispatch_task 的 mode 参数同一数据源
	mux.HandleFunc("POST /api/providers", a.handleUpsertProvider)
	mux.HandleFunc("POST /api/providers/toggle", a.handleToggleProvider)
	mux.HandleFunc("POST /api/providers/delete", a.handleDeleteProvider)
	mux.HandleFunc("POST /api/providers/test", a.handleTestProvider)
	mux.HandleFunc("POST /api/providers/fetch", a.handleFetchModels)
	a.apiRoute(mux, "GET /api/sessions", a.handleListSessions, apiMeta{Summary: "会话列表", Params: "parent=按父会话查子对话", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/sessions", a.handleCreateSession, apiMeta{Summary: "新建会话", Params: "mode/workspace/title/provider/model/permission/planMode/parent/inheritContext 等", Perm: permWorkspace})
	a.apiRoute(mux, "GET /api/sessions/{id}", a.handleGetSession, apiMeta{Summary: "会话详情（含运行态/排队/最近错误）", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/sessions/{id}/messages", a.handleSessionMessages, apiMeta{Summary: "会话消息轻量文本列表", Params: "role 过滤（如 user）、limit 从最新往回收", Perm: permReadOnly})
	a.apiRoute(mux, "PATCH /api/sessions/{id}", a.handlePatchSession, apiMeta{Summary: "改会话设置", Params: "title/model/provider/permission/thinking/archived/unread 等", Perm: permWorkspace})
	a.apiRoute(mux, "DELETE /api/sessions/{id}", a.handleDeleteSession, apiMeta{Summary: "删除会话（历史不可恢复）", Perm: permWorkspace, Danger: "删除会话及其全部历史，不可恢复"})
	a.apiRoute(mux, "POST /api/sessions/{id}/chat", a.handleChat, apiMeta{Summary: "向会话发一条消息（异步执行）", Params: "content=正文", Perm: permWorkspace})
	mux.HandleFunc("GET /api/sessions/{id}/stream", a.handleSessionStream)
	a.apiRoute(mux, "POST /api/sessions/{id}/stop", a.handleStop, apiMeta{Summary: "停止会话当前执行（先协作取消，3 秒未收口自动强制终结）", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/sessions/{id}/retry", a.handleRetry, apiMeta{Summary: "失败后带着上下文重试一轮", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/sessions/{id}/interject", a.handleInterject, apiMeta{Summary: "执行中发消息（插话/排队）", Params: "content、mode=steer|queue", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/sessions/{id}/pending", a.handlePending, apiMeta{Summary: "排队坞操作", Params: "action=remove|steer|move|pause|resume", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/sessions/{id}/compact", a.handleCompact, apiMeta{Summary: "手动压缩会话历史为摘要（模型摘要后台执行，立即返回）", Params: "upTo=压缩到哪条", Perm: permWorkspace})
	// 右键上下文菜单「提前自动压缩」：对当前会话手动点火一次自动压缩线路
	//（autoCompactKick），与上面的手动全量压缩是并存的两条线路。
	a.apiRoute(mux, "POST /api/sessions/{id}/autocompact", a.handleAutoCompact, apiMeta{Summary: "提前自动压缩：不等阈值到点，按设置的保留字数手动点火一次自动压缩线路", Perm: permWorkspace})
	// 设置 · 对话历史与压缩：数字助理主对话按「版本」管理（见 assistant_versions.go）。
	// 当前是哪一版由库里的指针决定，前端只照版本清单显示与切换；压缩固定走
	// 模型摘要；清空只作用于当前这一版（设置页已换成「新建一版」，接口留着）。
	a.apiRoute(mux, "GET /api/assistant/versions", a.handleAssistantVersions, apiMeta{Summary: "数字助理主对话的版本清单（当前版本/消息数/最后更新）", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/assistant/versions/new", a.handleAssistantVersionNew, apiMeta{Summary: "新建一版数字助理主对话（从零开始，老版本保留）", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/assistant/versions/switch", a.handleAssistantVersionSwitch, apiMeta{Summary: "切换数字助理主对话的当前版本", Params: "sessionId", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/assistant/compact", a.handleAssistantCompact, apiMeta{Summary: "压缩数字助理主对话当前版本的历史", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/assistant/clear", a.handleAssistantClear, apiMeta{Summary: "清空数字助理主对话当前版本的历史", Perm: permFull, Danger: "清空当前版本总控主对话的全部历史，不可恢复"})
	a.apiRoute(mux, "GET /api/sessions/{id}/progress", a.handleGetProgress, apiMeta{Summary: "会话进度清单", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/sessions/{id}/stop-child", a.handleStopChild, apiMeta{Summary: "停止会话的一个运行中程序（进度面板停止按钮）", Params: "id=程序 id（job-*/child-*）", Perm: permWorkspace})
	a.apiRoute(mux, "GET /api/sessions/{id}/plans", a.handleListPlans, apiMeta{Summary: "会话提交过的计划清单", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/sessions/{id}/plans/export", a.handleExportPlan, apiMeta{Summary: "把计划导出为工作区 md 文件", Params: "seq 可选", Perm: permWorkspace})
	a.apiRoute(mux, "GET /api/sessions/{id}/context", a.handleContextStats, apiMeta{Summary: "上下文用量统计", Perm: permReadOnly})
	mux.HandleFunc("GET /api/sessions/{id}/export", a.handleExport)
	mux.HandleFunc("POST /api/sessions/{id}/upload", a.handleUpload)
	mux.HandleFunc("GET /api/events", a.handleEvents)
	mux.HandleFunc("GET /api/p2p-shell", a.handleP2PShell)
	mux.HandleFunc("GET /api/shell-hash", a.handleShellHash)
	mux.HandleFunc("POST /api/shell-bump", a.handleShellBump)     // 「关于」页一键清远程端缓存：外壳代数 +1
	mux.HandleFunc("POST /api/webrtc-offer", a.handleWebRTCOffer) // P2P 直连信令：SDP 换 answer（p2pdirect.go，DataChannel 鉴权另过 auth 帧）
	mux.HandleFunc("POST /api/feedback", a.handleFeedbackCreate)
	mux.HandleFunc("POST /api/open-folder", a.handleOpenFolder)
	// 把主窗口顶到用户眼前（提醒触发时后端已直接调用同一实现；这个入口给
	// 前端/自测用，flashOnly=true 只走任务栏闪烁 + 托盘通知那条兜底路径）
	mux.HandleFunc("POST /api/desktop/front", a.handleDesktopFront)
	mux.HandleFunc("POST /api/alert/screen", a.handleAlertScreenHide)
	mux.HandleFunc("GET /api/pick-folder", a.handlePickFolder)
	mux.HandleFunc("GET /api/pick-file", a.handlePickFile)
	a.apiRoute(mux, "POST /api/sessions/{id}/fork", a.handleFork, apiMeta{Summary: "从某条消息分支新会话", Params: "seq=分支点", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/sessions/{id}/edit", a.handleEdit, apiMeta{Summary: "改写某条用户消息并重跑", Params: "seq、content", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/sessions/{id}/file-rewind", a.handleFileRewind, apiMeta{Summary: "把文件回退到某个历史版本", Params: "path、seq", Perm: permWorkspace})
	a.apiRoute(mux, "GET /api/sessions/{id}/file-versions", a.handleFileVersions, apiMeta{Summary: "某文件的历史版本清单", Params: "path", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/sessions/{id}/file-version", a.handleFileVersion, apiMeta{Summary: "取某文件某个历史版本正文", Params: "path、seq", Perm: permReadOnly})
	mux.HandleFunc("POST /api/sessions/{id}/delivery/open", a.handleDeliveryOpenFile)
	mux.HandleFunc("POST /api/sessions/{id}/delivery/reveal", a.handleDeliveryReveal)
	mux.HandleFunc("GET /api/sessions/{id}/delivery/download", a.handleDeliveryDownload)
	// 桌面壳右键菜单的「在系统中打开/在文件夹中定位/在系统浏览器中打开」：
	// 用户手势链路一律直通（无工作区校验、无确认弹窗——这些路由不在
	// call_api 目录里，AI 调不到），仅保留「路径不存在」自然错误；
	// open-url 只放行 http(s)
	mux.HandleFunc("POST /api/open-url", a.handleOpenURL)
	mux.HandleFunc("POST /api/sessions/{id}/open-path", a.handleSessionOpenPath)
	mux.HandleFunc("POST /api/sessions/{id}/reveal-path", a.handleSessionRevealPath)
	// 读系统剪贴板（右键「粘贴」在桌面壳里的兜底路径：WebView2 里
	// readText 会永久挂起）。剪贴板是敏感数据：仅回环可达
	mux.HandleFunc("GET /api/clipboard", a.handleClipboardRead)
	// 前端诊断打点（见 handleDiag）：只写日志不回数据
	mux.HandleFunc("GET /api/diag", a.handleDiag)
	a.apiRoute(mux, "GET /api/approvals", a.handleListApprovals, apiMeta{Summary: "待决定的越权审批清单", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/approvals/{id}", a.handleResolveApproval, apiMeta{Summary: "审批决定", Params: "approved、always", Perm: permWorkspace})
	a.apiRoute(mux, "GET /api/questions", a.handleListQuestions, apiMeta{Summary: "待回答的提问/计划审批清单", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/questions/{id}", a.handleResolveQuestion, apiMeta{Summary: "回答提问/计划审批", Params: "answer（计划批准=__approve__、忽略=__ignore__）", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/workspace/test", a.handleTestWorkspace, apiMeta{Summary: "测试 SSH 远程工作区连通性", Params: "remote 配置", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/workspace/ls", a.handleWorkspaceLs, apiMeta{Summary: "浏览远程工作区目录", Params: "remote、path", Perm: permReadOnly})
	// 已登记远程工作区（remotelist.go）：工作目录面板的远程条目
	a.apiRoute(mux, "GET /api/remotes", a.handleListRemotes, apiMeta{Summary: "已登记的 SSH 远程工作区清单（不含密码）", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/remotes", a.handleRegisterRemote, apiMeta{Summary: "登记/刷新一个 SSH 远程工作区", Params: "remote 配置", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/remotes/delete", a.handleDeleteRemote, apiMeta{Summary: "忘记一个已登记的远程工作区", Params: "key", Perm: permWorkspace})
	mux.HandleFunc("GET /attachments/", a.handleAttachment)
	mux.HandleFunc("GET /api/skills", a.handleListSkills)
	mux.HandleFunc("POST /api/skills/add", a.handleAddSkill)
	mux.HandleFunc("POST /api/skills/upload", a.handleUploadSkill)
	mux.HandleFunc("POST /api/skills/install-files", a.handleInstallSkillFiles) // 文件夹拖拽安装（multipart files+paths）
	mux.HandleFunc("POST /api/skills/toggle", a.handleToggleSkill)
	mux.HandleFunc("POST /api/skills/delete", a.handleDeleteSkill)
	a.apiRoute(mux, "GET /api/experts", a.handleListExperts, apiMeta{Summary: "子对话专家清单", Perm: permReadOnly})
	mux.HandleFunc("POST /api/experts", a.handleUpsertExpert)
	mux.HandleFunc("POST /api/experts/delete", a.handleDeleteExpert)
	a.apiRoute(mux, "POST /api/sessions/{id}/remote", a.handleSetSessionRemote, apiMeta{Summary: "设置/解除 SSH 远程工作区", Params: "remote 配置或 clear=true", Perm: permWorkspace})
	// 插件兼容层统一清单与总开关（pluginlayers.go 的各层注册）：与组件同口径
	// 开放给 call_api（?s= / session 的会话级覆盖一并可用）。
	a.apiRoute(mux, "GET /api/plugins", a.handleListPlugins, apiMeta{
		Summary: "插件清单（各兼容层的插件 + 层信息）；带 s=会话id 返回该会话的生效视图",
		Params:  "s 可选（会话id＝会话级生效视图）",
		Perm:    permReadOnly,
	})
	a.apiRoute(mux, "POST /api/plugins", a.handleSetPlugin, apiMeta{
		Summary: "开关插件（带 session＝只改该会话的覆盖，全局开关不动）",
		Params:  "id、enabled；layer 可选（层 id，缺省按 id 在全部层里唯一匹配）、session 可选（会话id＝会话级覆盖）",
		Perm:    permWorkspace,
	})
	mux.Handle("GET /_plugins/", http.HandlerFunc(a.handlePluginAsset))
	// Claude Code 插件兼容层（ccplugin*.go）：安装/开关/市场/命令清单
	mux.HandleFunc("GET /api/cc-plugins", a.handleListCCPlugins)
	mux.HandleFunc("POST /api/cc-plugins/toggle", a.handleSetCCPlugin)
	mux.HandleFunc("POST /api/cc-plugins/install", a.handleInstallCCPlugin)
	mux.HandleFunc("POST /api/cc-plugins/uninstall", a.handleUninstallCCPlugin)
	mux.HandleFunc("POST /api/cc-plugins/marketplace/add", a.handleCCMarketplaceAdd)
	mux.HandleFunc("GET /api/cc-plugins/marketplace/{id}", a.handleCCMarketplaceBrowse)
	mux.HandleFunc("GET /api/cc-plugins/commands", a.handleCCPluginCommands)
	// 独立 MCP 服务器（mcpservers*.go）：注册表 + 设计软件桥目录一键安装
	mux.HandleFunc("GET /api/mcp", a.handleListMCPServers)
	mux.HandleFunc("POST /api/mcp", a.handleUpsertMCPServer)
	mux.HandleFunc("POST /api/mcp/delete", a.handleDeleteMCPServer)
	mux.HandleFunc("POST /api/mcp/test", a.handleTestMCPServer)
	mux.HandleFunc("POST /api/mcp/upload", a.handleUploadMCPServers)
	mux.HandleFunc("GET /api/mcp/catalog", a.handleMCPCatalog)
	mux.HandleFunc("POST /api/mcp/catalog/install", a.handleMCPCatalogInstall)
	// 组件两档（内置 组件/ 与用户 组件/user/）同一套接口：清单、开关（含会话级
	// 覆盖 ?s= / session）、用户组件卸载。开放给 call_api 让长期助理能查能开关。
	a.apiRoute(mux, "GET /api/components", a.handleListComponents, apiMeta{
		Summary: "组件清单（内置组件 + 用户组件，builtin 标记归属）；带 s=会话id 返回该会话的生效视图",
		Params:  "s 可选（会话id＝会话级生效视图）；rescan=1 可选（先重扫组件目录：平时清单是纯读，重扫只挂在打开设置页）",
		Perm:    permReadOnly,
	})
	a.apiRoute(mux, "POST /api/components", a.handleSetComponent, apiMeta{
		Summary: "开关组件（带 session＝只改该会话的覆盖，全局开关不动）",
		Params:  "id、enabled；可选 session（会话id＝会话级覆盖）",
		Perm:    permWorkspace,
	})
	a.apiRoute(mux, "POST /api/components/delete", a.handleDeleteComponent, apiMeta{
		Summary: "卸载一个用户组件（组件/user/<id> 目录删除；内置组件拒绝，只能关开关）",
		Params:  "id",
		Perm:    permWorkspace,
		Danger:  "删除用户组件目录与设置，不可恢复（重装需走市场或手动放回组件/user/）",
	})
	// 会话级组件配置（合并层，见 sessioncx.go 文件头）：会话独立时组件的存储
	// 读写落在会话覆盖上，读取按「会话覆盖 > 全局值」合并——组件零改动。
	a.apiRoute(mux, "GET /api/components/cfg", a.handleComponentCfgGet, apiMeta{
		Summary: "某会话的组件配置视图（总开关 + 每组件「会话独立/跟随全局」与覆盖值）",
		Params:  "s=会话id（必填）",
		Perm:    permReadOnly,
	})
	a.apiRoute(mux, "POST /api/components/cfg", a.handleComponentCfgSet, apiMeta{
		Summary: "写/删一条会话级组件配置覆盖（组件需在该会话处于「会话独立」）",
		Params:  "session、id、key、value（缺省或 null＝删这条，回落到全局值）",
		Perm:    permWorkspace,
	})
	a.apiRoute(mux, "POST /api/components/cfg/mode", a.handleComponentCfgMode, apiMeta{
		Summary: "切换某组件在该会话是「会话独立」还是「跟随全局」（跟随＝删它的会话覆盖）",
		Params:  "session、id、on（true=会话独立 / false=跟随全局）",
		Perm:    permWorkspace,
	})
	a.apiRoute(mux, "POST /api/components/cfg/scope", a.handleComponentCfgScope, apiMeta{
		Summary: "会话级组件设置总开关（一键切全场）：开＝本对话独立，关＝清空覆盖回落全局",
		Params:  "session、on",
		Perm:    permWorkspace,
		Danger:  "关掉会清空该会话的全部组件配置覆盖（回落全局值），不可恢复",
	})
	// 组件浏览器半边资产挂在 /api/ 下（而非独立 /_components/ 前缀）：前端 cx.js
	// 统一用 fetch 拿资产，远程模式下 /api/ 前缀经 remote-boot 的 fetch 劫持自动
	// 走中继隧道——旧 /_components/ 标签相对网关 origin 解析必 404，远程组件全体消失
	mux.Handle("GET /api/components/", http.HandlerFunc(a.handleComponentAsset))
	// 市场（技能/组件/插件，见 market.go）：清单视图 + 一键安装/卸载；
	// 同样挂 /api/ 下让远程中继天然覆盖
	mux.HandleFunc("GET /api/market", a.handleMarketView)
	mux.HandleFunc("POST /api/market/install", a.handleMarketInstall)
	mux.HandleFunc("POST /api/market/uninstall", a.handleMarketUninstall)
	// 更新（update.go / update_blocks.go）：手动立即检查（不等每小时轮询）与
	// engine 交换后的立即重启（PowerShell 助手接力）；同样挂 /api/ 下远程中继天然覆盖
	mux.HandleFunc("POST /api/update/check", a.handleUpdateCheck)
	mux.HandleFunc("POST /api/update/restart", a.handleUpdateRestart)
	mux.HandleFunc("POST /api/update/installer", a.handleUpdateInstaller)
	mux.HandleFunc("GET /api/market/publish-meta", a.handleMarketPublishMeta) // 已装技能的发布元信息推导（弹层预填）
	mux.HandleFunc("POST /api/market/publish", a.handleMarketPublish)         // 发布已装技能到市场服务器：先装后传（market_publish.go）
	mux.HandleFunc("POST /api/market/unpublish", a.handleMarketUnpublish)     // 下架自己发布的市场条目（归属校验在服务端）
	// 小程序平台内嵌代理（sitesproxy.go）：路径与服务端平台页依赖一一对应，前端
	// iframe 加载本地同源 /sites——证书校验由 companyTransport 接管，浏览器/WebView
	// 自己的证书校验不再挡路（同企业技能市场的"前端不直连服务端"经验）。
	// 必须带方法限定注册：仓库里已有 "GET /"（静态前端），不带方法的模式会与其
	// 冲突（ServeMux 视为对 GET 请求的歧义匹配而 panic）；平台页用到 GET 与 POST。
	for _, m := range []string{"GET", "POST"} {
		mux.HandleFunc(m+" /sites", a.handleSitesProxy)
		mux.HandleFunc(m+" /sites-assets/", a.handleSitesProxy)
		mux.HandleFunc(m+" /s/", a.handleSitesProxy)
		mux.HandleFunc(m+" /api/sites/", a.handleSitesProxy)
		mux.HandleFunc(m+" /api/whoami", a.handleSitesProxy)
	}
	// 公司接入向导（wizard.go）：局域网发现 + 总服务器查询 + 一键应用配置
	mux.HandleFunc("POST /api/wizard/discover", a.handleWizardDiscover)
	mux.HandleFunc("POST /api/wizard/connect", a.handleWizardConnect)
	// 公司通道额度刷新（sync.go 的 companySyncOnce 复用）：设置页「刷新」按钮调它，
	// 重拉一次公告 → 额度/模型/地址一起跟上，不新开查询口子
	mux.HandleFunc("POST /api/company-sync", a.handleCompanySyncNow)
	// 语音组件（voice）宿主半边（voiceapi.go）：朗读走微软 Edge「大声朗读」
	// 免密钥通道；组件关掉时一并 404；同样挂 /api/ 下让远程中继天然覆盖
	mux.HandleFunc("GET /api/voice/voices", a.handleVoiceVoices)
	mux.HandleFunc("POST /api/voice/tts", a.handleVoiceTTS)
	// 演示记录器（创建技能模式）：录制归用户直控——输入框旁的按钮 POST 这里
	// 起录/停录，模型不持有录制工具，材料以【演示记录】系统通知进对话（recorder.go）
	mux.HandleFunc("POST /api/demo-record/start", a.handleDemoRecordStart)
	mux.HandleFunc("POST /api/demo-record/stop", a.handleDemoRecordStop)
	mux.HandleFunc("GET /api/demo-record/status", a.handleDemoRecordStatus)
	// 声明式组件桥（componentbridge.go）：组件自包含后端的统一端点前缀——
	// /api/c/<组件id>/<path> 按 manifest 的 workers 声明分发（call=HTTP 一行一答、
	// ws=WebSocket 哑转发）；组件未声明/未启用 404。挂 /api/ 下远程中继天然覆盖
	mux.HandleFunc("GET /api/c/{id}/{rest...}", a.handleComponentBridge)
	mux.HandleFunc("POST /api/c/{id}/{rest...}", a.handleComponentBridge)
	// 设备桥（device_bridge.go）：Kotlin 壳长轮询拿命令/回传结果；自检端点供
	// 本机 curl 一键诊断三件套链路。三端点仅回环可达（loopbackOnly 闸）
	mux.HandleFunc("GET /api/device/poll", a.handleDevicePoll)
	mux.HandleFunc("POST /api/device/result", a.handleDeviceResult)
	mux.HandleFunc("GET /api/device/selftest", a.handleDeviceSelftest)

	// run_python 工具桥（bridge.go）：脚本以本会话身份调白名单工具/派子任务。
	// 与设备桥同款回环闸（仅 127.0.0.1 可达，LAN/远程 403）。
	mux.HandleFunc("POST /api/bridge/tool", a.handleBridgeTool)
	// 浏览器（browser_host.go / browser_panel_windows.go）：桌面右栏是原生
	// WebView2 面板（前端经 panel-rect 上报占位区矩形），navigate/viewport/
	// state 面向内嵌形态目标；open-view/view 是聊天链接与 html 交付文件点开
	// 的入口（远程工作区的 html 经 view 流式渲染）。旧 MJPEG 帧流与输入镜像
	// 端点已随镜像方案移除
	mux.HandleFunc("POST /api/browser/panel-rect", a.handleBrowserPanelRect)
	mux.HandleFunc("POST /api/browser/navigate", a.handleBrowserNavigate)
	mux.HandleFunc("POST /api/browser/viewport", a.handleBrowserViewport)
	mux.HandleFunc("GET /api/browser/state", a.handleBrowserState)
	mux.HandleFunc("GET /api/browser/frame", a.handleBrowserFrame)
	mux.HandleFunc("POST /api/browser/close", a.handleBrowserClose)
	mux.HandleFunc("POST /api/browser/open-view", a.handleBrowserOpenView)
	mux.HandleFunc("GET /api/browser/view", a.handleBrowserView)
	// 右栏 Markdown 渲染页（mdview.go）：复用 md-viewer 的渲染网页，路径形态的
	// 地址让 md 里的相对图片/链接天然解析到同一路由（用户手势链路，AI 调不到）
	mux.HandleFunc("GET /api/md/{sid}/{path...}", a.handleMDView)
	// Electron 壳面板桥的导航回报（panel-bridge.js 在 did-navigate 推送）：
	// 更新内嵌目标状态并广播 browser_nav（旧壳由 onNavCompleted 触发同一事件）
	mux.HandleFunc("POST /api/browser/electron-report", a.handleBrowserElectronReport)
	// 右侧栏截图（panelshot_windows.go）：panel_screenshot 工具注入主界面的
	// 探针 JS 把右栏矩形回传到这里
	mux.HandleFunc("POST /api/shell/panel-view", a.handleShellPanelView)
	// 待办与定时任务（组件 todo-scheduler 的宿主半边）：组件未启用时这些接口一律 404
	a.apiRoute(mux, "GET /api/todos", a.handleTodoList, apiMeta{Summary: "待办/定时任务清单", Perm: permReadOnly})
	a.apiRoute(mux, "POST /api/todos", a.handleTodoCreate, apiMeta{Summary: "新建待办/定时任务", Params: "title/description/schedule/executor 等", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/todos/update", a.handleTodoUpdate, apiMeta{Summary: "修改待办任务", Params: "id 与要改的字段", Perm: permWorkspace})
	a.apiRoute(mux, "POST /api/todos/delete", a.handleTodoDelete, apiMeta{Summary: "删除待办任务", Params: "id", Perm: permWorkspace, Danger: "删除待办任务，不可恢复"})
	a.apiRoute(mux, "POST /api/todos/run", a.handleTodoRun, apiMeta{Summary: "立即执行一条待办任务", Params: "id", Perm: permWorkspace, Danger: "立即触发任务执行（会真实运行命令或派发 AI）"})
	a.apiRoute(mux, "POST /api/todos/missed", a.handleTodoMissed, apiMeta{Summary: "处理错过的定时槽点", Params: "id、action=run|dismiss", Perm: permWorkspace})
	a.apiRoute(mux, "GET /api/todos/workspaces", a.handleTodoWorkspaces, apiMeta{Summary: "程序任务可选的工作目录", Perm: permReadOnly})
	// Git 面板（组件 git-panel 的宿主半边）：组件未启用时这些接口一律 404。
	// 读 status/diff/graph/commit/branches，写有 stage/unstage/untrack（移出
	// 跟踪变未跟踪）/commit/summarize（只生成提交信息不提交）/checkout（切
	// 本地分支）/sync（op=pull 拉取：fetch+快进或自动合并；op=push 推送：仅本地
	// 领先且远程无新提交；空=整套先拉后推；任何
	// 需要决策的情形原样退回）。不做 reset/revert/discard/stash/rebase。
	a.apiRoute(mux, "GET /api/git/status", a.handleGitStatus, apiMeta{Summary: "Git 工作区状态（porcelain）", Params: "repo 可选", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/git/diff", a.handleGitDiff, apiMeta{Summary: "文件 diff", Params: "repo、path 等", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/git/graph", a.handleGitGraph, apiMeta{Summary: "提交图谱数据", Params: "repo 可选", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/git/commit", a.handleGitCommitDetail, apiMeta{Summary: "提交详情", Params: "repo、hash", Perm: permReadOnly})
	a.apiRoute(mux, "GET /api/git/branches", a.handleGitBranches, apiMeta{Summary: "分支清单", Params: "repo 可选", Perm: permReadOnly})
	mux.HandleFunc("POST /api/git/stage", a.handleGitStage)
	mux.HandleFunc("POST /api/git/unstage", a.handleGitUnstage)
	mux.HandleFunc("POST /api/git/untrack", a.handleGitUntrack)
	mux.HandleFunc("POST /api/git/commit", a.handleGitCommitMsg)
	mux.HandleFunc("POST /api/git/summarize", a.handleGitSummarize)
	mux.HandleFunc("POST /api/git/checkout", a.handleGitCheckout)
	mux.HandleFunc("POST /api/git/sync", a.handleGitSync)
	a.Plugins.MountRoutes(a, mux)
	// 品牌下发（brand_sync.go）：前端启动拉一次文字配置，brand 事件到达时重拉
	mux.HandleFunc("GET /api/brand", a.handleBrand)
	mux.HandleFunc("GET /", a.handleStatic)
}

// handleStatic 提供前端静态资源；入口页在返回前把已启用插件的
// 浏览器半边资产（css/js）注入 </body> 前。
func (a *App) handleStatic(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/")
	if p == "" || p == "index.html" {
		data, err := fs.ReadFile(a.static, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		html := string(data)
		if a.Plugins != nil {
			assets := a.Plugins.Assets()
			tags := []string{}
			for _, pl := range assets {
				if pl.CSS != "" {
					tags = append(tags, `<link rel="stylesheet" href="/_plugins/`+pl.ID+`/`+pl.CSS+`">`)
				}
			}
			for _, pl := range assets {
				if pl.JS != "" {
					tags = append(tags, `<script src="/_plugins/`+pl.ID+`/`+pl.JS+`" defer></script>`)
				}
			}
			if len(tags) > 0 && strings.Contains(html, "</body>") {
				html = strings.Replace(html, "</body>", strings.Join(tags, "\n")+"</body>", 1)
			}
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write([]byte(html))
		return
	}
	// 品牌下发（brand_sync.go）：服务端下发的 LOGO 缓存优先于 embed 默认图
	if r.URL.Path == "/logo.png" && a.serveBrandLogo(w, r) {
		return
	}
	// 静态子资源（app.js/style.css 等）一律 no-cache：embed FS 不带修改时间戳，
	// 浏览器会对无验证器的响应做启发式缓存，出现"前端改了、刷新不生效"；
	// 强制每次回源校验，本地服务开销可忽略
	w.Header().Set("Cache-Control", "no-cache")
	http.FileServer(http.FS(a.static)).ServeHTTP(w, r)
}

// ---------- 插件兼容层：统一清单与总开关（各层的注册表见 pluginlayers.go） ----------

func (a *App) handleListPlugins(w http.ResponseWriter, r *http.Request) {
	if a.Layers == nil {
		writeJSON(w, 200, map[string]any{"layers": []LayerInfo{}, "plugins": []LayerPlugin{}})
		return
	}
	// ?s=<会话id>：该会话的生效视图（会话覆盖优先于全局开关）+ sessionCustom 标记
	if sid := strings.TrimSpace(r.URL.Query().Get("s")); sid != "" {
		writeJSON(w, 200, map[string]any{
			"layers":        a.Layers.Infos(),
			"plugins":       a.cxoPluginList(sid),
			"sessionCustom": a.SessionCX.custom(sid),
		})
		return
	}
	writeJSON(w, 200, map[string]any{"layers": a.Layers.Infos(), "plugins": a.Layers.AllPlugins()})
}

func (a *App) handleSetPlugin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Layer   string `json:"layer"` // 来源体系（层 id；缺省=全部层里按 id 唯一匹配）
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
		Session string `json:"session"` // 会话id：带它＝只改本会话的覆盖，全局开关不动
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeErr(w, 400, "需要 id 与 enabled")
		return
	}
	if a.Layers == nil {
		writeErr(w, 500, "插件兼容层未初始化")
		return
	}
	// 会话级覆盖：写入前先按同款规则定位插件（不存在＝404），再落覆盖记录
	if sid := strings.TrimSpace(body.Session); sid != "" {
		if _, err := a.Store.Get(sid); err != nil {
			writeErr(w, 404, "会话不存在")
			return
		}
		if !a.pluginGlobalKnown(body.Layer, body.ID) {
			writeErr(w, 404, errPluginNotFound.Error())
			return
		}
		a.cxoSetPlugin(sid, body.Layer, body.ID, body.Enabled)
		writeJSON(w, 200, map[string]any{
			"ok": 1, "plugins": a.cxoPluginList(sid),
			"sessionCustom": a.SessionCX.custom(sid),
		})
		return
	}
	if err := a.Layers.SetEnabled(body.Layer, body.ID, body.Enabled); err != nil {
		if errors.Is(err, errLayerNotFound) || errors.Is(err, errPluginNotFound) {
			writeErr(w, 404, err.Error())
			return
		}
		writeErr(w, 500, "保存开关失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "refreshNeeded": true, "plugins": a.Layers.AllPlugins()})
}

// handlePluginAsset 提供适配器浏览器半边资产（仅平级文件，防目录穿越）。
func (a *App) handlePluginAsset(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/_plugins/")
	parts := strings.SplitN(rest, "/", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		http.NotFound(w, r)
		return
	}
	id, file := parts[0], parts[1]
	if strings.Contains(file, "..") || strings.ContainsAny(file, `\/`) {
		http.NotFound(w, r)
		return
	}
	m, ok := a.Plugins.Manifest(id)
	if !ok || !a.Plugins.IsEnabled(id) {
		http.NotFound(w, r)
		return
	}
	full := filepath.Join(a.Plugins.Dir(), id, file)
	if full != filepath.Join(filepath.Clean(full)) {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, full)
	_ = m
}

// withCORS 同源校验 + 简要访问日志（P1-9）：去掉 Access-Control-Allow-Origin: *，
// Origin 与 Host 不符一律 403——堵死本机恶意网页跨域调用 /api/approvals（替用户
// 点「允许」）与 PATCH 会话权限（切 full 绕过整个沙箱体系）的 CSRF 面。
// 浏览器同源请求（本地 GUI）与无 Origin 的非浏览器客户端（curl/测试）不受影响。
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("%s %s from %s (%s)", r.Method, r.URL.Path, r.RemoteAddr, r.Header.Get("User-Agent"))
		origin := r.Header.Get("Origin")
		if origin != "" && !sameOrigin(origin, r.Host) {
			writeErr(w, 403, "跨域请求被拒绝（同源策略）")
			return
		}
		w.Header().Set("Vary", "Origin")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PATCH, DELETE, OPTIONS")
		if origin != "" {
			w.Header().Set("Access-Control-Allow-Origin", origin)
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// sameOrigin 判断 Origin 头（scheme://host[:port]）与请求 Host 是否同源。
// Origin 缺省端口（http 80 / https 443）与显式端口等价。
func sameOrigin(origin, host string) bool {
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	o := strings.ToLower(u.Host)
	h := strings.ToLower(host)
	if o == h {
		return true
	}
	// 端口缺省等价：http://x ≡ http://x:80，https://x ≡ https://x:443
	oh, hp := splitHostPort(o)
	hh, hpc := splitHostPort(h)
	if oh != hh {
		return false
	}
	if hp == "" {
		if u.Scheme == "https" {
			hp = "443"
		} else {
			hp = "80"
		}
	}
	if hpc == "" {
		hpc = hp
	}
	return hp == hpc
}

// splitHostPort 拆 host[:port]；IPv6 字面量 [::1]:8788 也覆盖。
func splitHostPort(hp string) (string, string) {
	if strings.HasPrefix(hp, "[") {
		if i := strings.LastIndex(hp, "]:"); i >= 0 {
			return hp[:i+1], hp[i+2:]
		}
		return hp, ""
	}
	if i := strings.LastIndex(hp, ":"); i >= 0 {
		return hp[:i], hp[i+1:]
	}
	return hp, ""
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func maskKey(k string) string {
	if k == "" {
		return ""
	}
	if len(k) <= 8 {
		return "****"
	}
	return k[:4] + "****" + k[len(k)-4:]
}

// likePattern 把用户输入转成逐字（字面）匹配的 LIKE 模式：
// % _ \ 全部转义，保证搜索词不会被当成通配符。
func likePattern(q string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + r.Replace(q) + "%"
}

// 可搜索的会话范围基座 = 侧栏可见范围：非助理主对话、非子会话。
// 归档与目录过滤由 searchScopeWhere 按 bin/ws 参数装配（都缺省时等价旧行为 archived = 0）。
const searchableSessionsBaseSQL = `mode != 'assistant' AND parent_id = ''`

// searchScopeWhere 按请求参数组装会话范围条件（pfx 为列名前缀，如 "s."）：
//
//	bin=1 → 回收站：已归档，或其工作目录在回收列表 ws 里；
//	否则  → 正常工作区：未归档，且工作目录不在回收列表 ws 里。
//
// ws 为回收目录列表（JSON 数组，空串代表默认工作区）；keep 为已单独恢复的
// 会话 id 列表：正常侧豁免目录回收（照常可搜），回收站侧则排除（已不属于
// 回收站）。参数缺省或为空时按旧行为处理，两边搜索互不越界。
func searchScopeWhere(r *http.Request, pfx string) (string, []any) {
	bin := r.URL.Query().Get("bin") == "1"
	cond := pfx + "archived = 0"
	if bin {
		cond = pfx + "archived = 1"
	}
	var args []any
	readList := func(key string) []string {
		raw := r.URL.Query().Get(key)
		if raw == "" {
			return nil
		}
		var out []string
		if json.Unmarshal([]byte(raw), &out) != nil {
			return nil
		}
		return out
	}
	placeholders := func(vals []string) string {
		ph := make([]string, len(vals))
		for i, v := range vals {
			ph[i] = "?"
			args = append(args, v)
		}
		return strings.Join(ph, ",")
	}
	wss, keeps := readList("ws"), readList("keep")
	if bin {
		if len(wss) > 0 {
			cond = "(" + pfx + "archived = 1 OR " + pfx + "workspace IN (" + placeholders(wss) + "))"
		}
		if len(keeps) > 0 {
			cond += " AND " + pfx + "id NOT IN (" + placeholders(keeps) + ")"
		}
	} else if len(wss) > 0 {
		inner := pfx + "workspace NOT IN (" + placeholders(wss) + ")"
		if len(keeps) > 0 {
			inner += " OR " + pfx + "id IN (" + placeholders(keeps) + ")"
		}
		cond += " AND (" + inner + ")"
	}
	return cond, args
}

// handleSearchTitle 按标题逐字搜索工作任务。
func (a *App) handleSearchTitle(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, map[string]any{"hits": []any{}})
		return
	}
	scope, scopeArgs := searchScopeWhere(r, "")
	rows, err := a.Store.db.Query(
		`SELECT id, title, updated_at FROM sessions
		 WHERE `+searchableSessionsBaseSQL+` AND `+scope+` AND title LIKE ? ESCAPE '\'
		 ORDER BY updated_at DESC LIMIT 50`,
		append(scopeArgs, likePattern(q))...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	hits := []map[string]any{}
	for rows.Next() {
		var id, title, updated string
		if err := rows.Scan(&id, &title, &updated); err != nil {
			continue
		}
		hits = append(hits, map[string]any{"sessionId": id, "title": title, "updatedAt": updated})
	}
	writeJSON(w, 200, map[string]any{"hits": hits})
}

// handleSearchContent 按聊天记录逐字搜索：只搜用户消息与模型最终输出
// （无工具调用的 assistant 消息），忽略思考（本就不入库）、工具调用与
// 系统注入行。每条命中返回定位用 seq 与命中点附近的片段。
func (a *App) handleSearchContent(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		writeJSON(w, 200, map[string]any{"hits": []any{}})
		return
	}
	scope, scopeArgs := searchScopeWhere(r, "s.")
	rows, err := a.Store.db.Query(
		`SELECT m.session_id, s.title, m.seq, m.role, m.content, m.created_at
		 FROM messages m JOIN sessions s ON s.id = m.session_id
		 WHERE s.`+searchableSessionsBaseSQL+` AND `+scope+`
		   AND m.kind = 'chat' AND m.content IS NOT NULL
		   AND (m.role = 'user'
		        OR (m.role = 'assistant' AND (m.tool_calls IS NULL OR m.tool_calls = '')))
		   AND m.content LIKE ? ESCAPE '\'
		 ORDER BY s.updated_at DESC, m.seq ASC LIMIT 200`,
		append(scopeArgs, likePattern(q))...)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	hits := []map[string]any{}
	for rows.Next() {
		var sid, title, role, content, created string
		var seq int64
		if err := rows.Scan(&sid, &title, &seq, &role, &content, &created); err != nil {
			continue
		}
		hits = append(hits, map[string]any{
			"sessionId": sid, "title": title, "seq": seq,
			"role": role, "snippet": searchSnippet(content, q), "createdAt": created,
		})
	}
	writeJSON(w, 200, map[string]any{"hits": hits})
}

// searchSnippet 截取首个命中点前后各约 40 字的片段（前端负责高亮）。
func searchSnippet(content, q string) string {
	const radius = 40
	lowC, lowQ := strings.ToLower(content), strings.ToLower(q)
	at := strings.Index(lowC, lowQ)
	if at < 0 {
		at = 0
	}
	runes := []rune(content)
	atRune := len([]rune(content[:at]))
	lo := atRune - radius
	if lo < 0 {
		lo = 0
	}
	hi := atRune + len([]rune(q)) + radius
	if hi > len(runes) {
		hi = len(runes)
	}
	pre, suf := "", ""
	if lo > 0 {
		pre = "…"
	}
	if hi < len(runes) {
		suf = "…"
	}
	return pre + string(runes[lo:hi]) + suf
}

// handleGetDisplayMode 设置页「界面渲染」区块的数据源：当前渲染模式与来由
// （desktop_windows.go 实现；网页/手机模式 desktop=false，前端隐藏区块）。
func (a *App) handleGetDisplayMode(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, a.displayMode())
}

// handleSetDisplayMode 切换 GPU 渲染 / 兼容模式：写删兼容标记 + 前端热重启
// （后端不动，窗口约 1 秒后原位重开）。
func (a *App) handleSetDisplayMode(w http.ResponseWriter, r *http.Request) {
	var p struct {
		Software bool `json:"software"`
	}
	if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
		writeErr(w, http.StatusBadRequest, "参数解析失败: "+err.Error())
		return
	}
	if err := a.setDisplayMode(p.Software); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (a *App) handleStatus(w http.ResponseWriter, r *http.Request) {
	t := a.DefaultTarget()
	ra := a.Cfg.RemoteAccess
	writeJSON(w, 200, map[string]any{
		"name":       "坚祥数字助理",
		"version":    appVersion,
		"android":    isAndroidRuntime, // 前端按平台隐藏「操作手机」权限芯片
		"model":      t.Model,
		"hasKey":     t.APIKey != "",
		"wizardDone": a.Cfg.WizardDone || a.Cfg.CompanyLinkOff, // 首启向导（app-wizard.js 据此自动弹出）；公司接入停用也视为"不用弹"
		"mock":       t.BaseURL == "mock",
		"workspace":  a.Cfg.Workspace,
		"allowShell": a.Cfg.AllowShell,
		// 左下角手机图标的状态来源：正常态显示「可远程访问」，异常态回退告警胶囊
		"remote": map[string]any{
			"enabled":   ra.Enabled,
			"connected": remoteConnected.Load(),
		},
		// 更新横幅的数据源：客户端打开时先查这里（SSE 无回放，在线推送靠事件流）。
		// 二开版本走 statusUpdateSnapshot：available/pendingRestart 恒空，前端横幅、
		// 胶囊、待重启提示自然不出现。
		"update": statusUpdateSnapshot(),
	})
}

// updateSnapshot 当前更新检查状态（线程安全读）。
func updateSnapshot() UpdateState {
	updateMu.Lock()
	defer updateMu.Unlock()
	return updateState
}

// DefaultProviderID 返回全局默认提供方 id（第一个可用提供方；全停时回落
// providers[0]，与 firstSelectableProvider 同口径）。
func (a *App) DefaultProviderID() string {
	if p := a.firstSelectableProvider(); p != nil {
		return p.ID
	}
	return ""
}

// handleChatModes 返回「人工新建会话」可选的工作模式清单（chatmodes.go 的
// chatModes，单一事实源）：前端模式下拉启动时拉取它重建选项，值与
// dispatch_task 的 mode 参数同源——加模式只改后端一处，下拉与派发自然一致。
func (a *App) handleChatModes(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"modes": chatModes})
}

// handleModels 返回模型选择器用的扁平清单（含各提供方启停状态：停用的置灰展示）。
// 多 Key 供应商的模型在这里就拍平：同一个供应商分组下平铺它所有 Key 的模型，
// 条目带 keyId/keyLabel——前端只对「同供应商内重名」的模型加 keyLabel 后缀区分，
// 不重名的不加任何标签。
func (a *App) handleModels(w http.ResponseWriter, r *http.Request) {
	type modelItem struct {
		ProviderID    string `json:"providerId"`
		ProviderName  string `json:"providerName"`
		ModelID       string `json:"modelId"`
		ModelName     string `json:"modelName,omitempty"`
		ContextWindow int    `json:"contextWindow,omitempty"`
		ProviderOff   bool   `json:"providerOff,omitempty"` // 所属提供方已停用（手动或自动）
		ProviderVia   string `json:"providerVia,omitempty"` // 公司通道当前线路：lan=内网 / wan=外网（配了外网地址才有值）
		KeyID         string `json:"keyId,omitempty"`       // 模型归属哪把 Key（选择时随会话保存，重名模型靠它定位）
		KeyLabel      string `json:"keyLabel,omitempty"`    // 该 Key 的分组备注名（重名时前端拿它当区分后缀）
	}
	items := []modelItem{}
	for _, p := range a.Cfg.Providers {
		via := ""
		if p.ID == wizardProviderID && p.PublicBaseURL != "" {
			if companyViaPublic() {
				via = "wan"
			} else {
				via = "lan"
			}
		}
		keys := p.KeyList()
		for ki, k := range keys {
			label := strings.TrimSpace(k.Label)
			if label == "" && len(keys) > 1 {
				// 没写分组备注名：多 Key 的供应商给个保底称呼，重名时至少能分辨
				label = fmt.Sprintf("Key %d", ki+1)
			}
			for _, m := range k.Models {
				// 思考档位不随模型下发：全系统固定六档，前端任何模型都能展开同一套
				items = append(items, modelItem{p.ID, p.Name, m.ID, m.Name, m.ContextWindow, !p.IsSelectable(), via, k.ID, label})
			}
		}
	}
	t := a.DefaultTarget()
	writeJSON(w, 200, map[string]any{
		"models":          items,
		"defaultProvider": a.DefaultProviderID(),
		"defaultModel":    t.Model,
		// 公司通道额度快照（模型菜单顶部显示；服务端下发才有，私人通道场景为空）
		"companyQuota": a.Cfg.CompanyQuota,
	})
}

// resolveProviderDraft 把请求里的 id/baseURL/apiKey/protocol/keyID 折算成一个
// 可用的临时提供方：没存过的 id（或新增卡片）直接用请求里带来的地址与密钥，
// 便于未保存先拉取/测试；预置模板的协议（如 Anthropic）在 id 命中预置时自动带上。
// 结果是「只含一把 Key」的提供方（多 Key 供应商的拉取/测试都按单把 Key 走）：
// keyID 命中已保存的 Key 就用它的密钥与协议，请求里带了 apiKey/protocol 的覆盖它
// （前端不回传已有密钥，留空＝沿用）。
func (a *App) resolveProviderDraft(id, baseURL, apiKey, protocol, keyID string) (*ProviderCfg, error) {
	eff := &ProviderCfg{}
	if p := a.providerByID(strings.TrimSpace(id)); p != nil {
		*eff = *p
	} else if preset, ok := PresetByID(strings.TrimSpace(id)); ok {
		*eff = preset
	}
	if u := strings.TrimSpace(baseURL); u != "" {
		eff.BaseURL = u
	}
	if strings.TrimSpace(eff.BaseURL) == "" {
		return nil, fmt.Errorf("缺少 API 地址")
	}
	k := eff.resolveKey(keyID, "")
	if v := strings.TrimSpace(protocol); v != "" {
		k.Protocol = v
	}
	if s := strings.TrimSpace(apiKey); s != "" {
		k.APIKey = s
	}
	eff.Keys = []ProviderKeyCfg{k} // 只留这一把：拉取/测试都只针对它
	eff.APIKey, eff.Models = "", nil
	return eff, nil
}

// handleFetchModels 从提供方服务拉取可用模型清单（服务端代理，避开浏览器跨域）。
// keyID 指明用哪把 Key：中转站按分组发 Key，不同分组能看到的模型不一样。
func (a *App) handleFetchModels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID       string `json:"id"`
		BaseURL  string `json:"baseURL"`
		APIKey   string `json:"apiKey"`
		Protocol string `json:"protocol"`
		KeyID    string `json:"keyId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	p, err := a.resolveProviderDraft(body.ID, body.BaseURL, body.APIKey, body.Protocol, body.KeyID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	models, err := a.FetchModels(p, body.KeyID)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"models": models})
}

// sanitizeModels 规整一份模型清单：去空白、按 id 去重（空 id 的丢掉）。
func sanitizeModels(in []ModelCfg) []ModelCfg {
	out := []ModelCfg{}
	seen := map[string]bool{}
	for _, m := range in {
		mid := strings.TrimSpace(m.ID)
		if mid == "" || seen[mid] {
			continue
		}
		seen[mid] = true
		m.ID = mid
		out = append(out, m)
	}
	return out
}

// providerProtocolOK 渠道协议白名单：空串＝OpenAI 兼容（默认）、anthropic、
// responses——与 llmproto.go 的三个实现一一对应。额外允许 Key 级的 "chat"
// 哨兵（显式 OpenAI 兼容，与空串的「跟随供应商」区分，见 models.go keyProtocol）。
func providerProtocolOK(v string) bool {
	switch v {
	case "", "chat", "anthropic", "responses":
		return true
	}
	return false
}

// normalizeProviderKeys 校验并规整请求带来的多 Key 清单：
//   - 归属 id 空（前端刚加的那把）就生成一个稳定的新 id，已保存的 id 原样保留
//     ——会话/草稿里的模型引用靠它定位，id 一旦发出去就不能再变；
//   - 协议必须是白名单值（空＝跟随供应商级）；
//   - 密钥留空＝沿用同 id 已保存的密钥（前端不回传密钥，与供应商级同口径）；
//   - 同一把 Key 内模型按 id 去重；
//   - 彻底空的 Key（没密钥、没备注名、没模型）直接丢掉，不留空壳。
func normalizeProviderKeys(in []ProviderKeyCfg, old *ProviderCfg) ([]ProviderKeyCfg, error) {
	oldKeys := map[string]ProviderKeyCfg{}
	used := map[string]bool{}
	if old != nil {
		for _, k := range old.Keys {
			oldKeys[k.ID] = k
			used[k.ID] = true
		}
	}
	newKeyID := func() string {
		for i := 1; ; i++ {
			id := fmt.Sprintf("k%d", i)
			if !used[id] {
				used[id] = true
				return id
			}
		}
	}
	out := make([]ProviderKeyCfg, 0, len(in))
	claimed := map[string]bool{}
	for _, k := range in {
		k.ID = strings.TrimSpace(k.ID)
		k.Label = strings.TrimSpace(k.Label)
		k.APIKey = strings.TrimSpace(k.APIKey)
		k.Protocol = strings.TrimSpace(k.Protocol)
		if !providerProtocolOK(k.Protocol) {
			return nil, fmt.Errorf("Key「%s」的协议类型未知：%s", keyLabelForErr(k), k.Protocol)
		}
		if k.APIKey == "" {
			if o, ok := oldKeys[k.ID]; ok {
				k.APIKey = o.APIKey // 留空＝沿用已保存的密钥
			}
		}
		k.Models = sanitizeModels(k.Models)
		if k.APIKey == "" && k.Label == "" && len(k.Models) == 0 {
			continue
		}
		if k.ID == "" || claimed[k.ID] {
			k.ID = newKeyID()
		}
		claimed[k.ID] = true
		used[k.ID] = true
		out = append(out, k)
	}
	return out, nil
}

// keyLabelForErr 报错时用来指认是哪把 Key（备注名 > 归属 id）。
func keyLabelForErr(k ProviderKeyCfg) string {
	if k.Label != "" {
		return k.Label
	}
	if k.ID != "" {
		return k.ID
	}
	return "未命名"
}

// handleUpsertProvider 新增或更新单个提供方（模型页卡片「保存」）。
func (a *App) handleUpsertProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string     `json:"id"`
		Name     string     `json:"name"`
		BaseURL  string     `json:"baseURL"`
		APIKey   string     `json:"apiKey"` // 空串 = 沿用已保存的 key
		Protocol string     `json:"protocol"`
		Models   []ModelCfg `json:"models"`
		// Keys 自定义供应商的多 Key（多分组）：带了就按它保存（每把 Key 各自
		// 的备注名/密钥/协议/模型）；预置提供方忽略它，保持单 Key 结构。
		Keys []ProviderKeyCfg `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	id := strings.TrimSpace(in.ID)
	preset, isPreset := PresetByID(id)
	// 协议类型校验：空串＝OpenAI 兼容（默认），预置方跟随模板（如 Anthropic）
	protocol := strings.TrimSpace(in.Protocol)
	switch protocol {
	case "", "anthropic", "responses":
	default:
		writeErr(w, 400, "未知的渠道协议类型："+protocol)
		return
	}
	if protocol == "" && isPreset {
		protocol = preset.Protocol
	}
	baseURL := strings.TrimSpace(in.BaseURL)
	if baseURL == "" {
		if !isPreset {
			writeErr(w, 400, "自定义提供方必须填写 API 地址")
			return
		}
		baseURL = preset.BaseURL
	}
	if id == "" {
		id = fmt.Sprintf("custom-%d", time.Now().UnixMilli())
	}
	name := strings.TrimSpace(in.Name)
	if name == "" {
		if isPreset {
			name = preset.Name
		} else {
			writeErr(w, 400, "名称不能为空")
			return
		}
	}
	models := sanitizeModels(in.Models)
	// 预置提供方未带模型目录时回落到模板清单：套餐端点（智谱 Coding 等）
	// 不一定支持 /models 拉取，模板自带 glm-5.3 系才能开箱即用
	if isPreset && len(models) == 0 && len(preset.Models) > 0 {
		models = append(models, preset.Models...)
	}
	old := a.providerByID(id)
	// 多 Key：只有自定义供应商走多 Key 结构。请求没带 keys（老页面/接口调用只
	// 提交 apiKey+models）时保留已保存的多 Key 结构——否则一次普通保存就会把
	// 用户配好的多分组冲成单 Key。
	var keys []ProviderKeyCfg
	if isPreset {
		// 预置提供方保持单 Key：密钥与模型由模板/系统维护，没有分组概念
	} else if len(in.Keys) > 0 {
		var err error
		if keys, err = normalizeProviderKeys(in.Keys, old); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	} else if old != nil && len(old.Keys) > 0 {
		keys = old.Keys
	}
	pc := ProviderCfg{ID: id, Name: name, BaseURL: baseURL, Protocol: protocol}
	if len(keys) > 0 {
		pc.Keys = keys
		pc.APIKey, pc.Models = keys[0].APIKey, keys[0].Models // 供应商级镜像第一把（老版本可读）
	} else {
		pc.Models = models
		if key := strings.TrimSpace(in.APIKey); key != "" {
			pc.APIKey = key
		} else if old != nil {
			pc.APIKey = old.APIKey
		}
	}
	// 启停状态与外网通道不在编辑表单里：替换保存时沿用旧值（toggle 端点与公告同步才是它们的写入口）
	if old != nil {
		pc.Enabled = old.Enabled
		pc.AutoDisabled = old.AutoDisabled
		pc.PublicBaseURL = old.PublicBaseURL
	}
	replaced := false
	for i := range a.Cfg.Providers {
		if a.Cfg.Providers[i].ID == id {
			a.Cfg.Providers[i] = pc
			replaced = true
			break
		}
	}
	if !replaced {
		a.Cfg.Providers = append(a.Cfg.Providers, pc)
	}
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "id": id})
}

// handleToggleProvider 手动启停单个提供方（设置页卡片开关）。手动启用顺带清除
// 自动停用状态（自动恢复不动手动停用，但手动开启优先于一切）。
func (a *App) handleToggleProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.ID) == "" {
		writeErr(w, 400, "需要 provider id 与 enabled")
		return
	}
	p := a.providerByID(strings.TrimSpace(in.ID))
	if p == nil {
		writeErr(w, 404, "提供方不存在")
		return
	}
	e := in.Enabled
	p.Enabled = &e
	if in.Enabled {
		p.AutoDisabled = false
	}
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	a.broadcast(Event{Type: "providers"}) // 默认提供方可能变了：前端刷新模型菜单
	writeJSON(w, 200, map[string]any{"ok": 1})
}

// handleDeleteProvider 删除单个提供方（模型页卡片「删除提供方」）。
func (a *App) handleDeleteProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.ID) == "" {
		writeErr(w, 400, "需要 provider id")
		return
	}
	id := strings.TrimSpace(in.ID)
	kept := make([]ProviderCfg, 0, len(a.Cfg.Providers))
	for _, p := range a.Cfg.Providers {
		if p.ID != id {
			kept = append(kept, p)
		}
	}
	a.Cfg.Providers = kept
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1})
}

// handleTestProvider 对提供方目录里的所有模型做连通性测试（极小请求，限并发）。
func (a *App) handleTestProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID       string   `json:"id"`
		BaseURL  string   `json:"baseURL"`
		APIKey   string   `json:"apiKey"`
		Protocol string   `json:"protocol"`
		KeyID    string   `json:"keyId"` // 用哪把 Key 测（不同分组的分组能调的模型不同）
		Models   []string `json:"models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	p, err := a.resolveProviderDraft(in.ID, in.BaseURL, in.APIKey, in.Protocol, in.KeyID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"results": TestProviderModels(p, in.KeyID, in.Models)})
}

func (a *App) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	// keyView 是下发给前端的一把 Key：密钥一律只回掩码，模型清单与分组备注名
	// 这类非敏感信息才下发（设置页据此渲染多 Key 卡片）。
	type keyView struct {
		ID           string     `json:"id"`
		Label        string     `json:"label,omitempty"`
		Protocol     string     `json:"protocol,omitempty"` // Key 级协议覆盖：空＝跟随供应商级
		APIKeyMasked string     `json:"apiKeyMasked"`
		Models       []ModelCfg `json:"models"`
	}
	type providerView struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		BaseURL  string `json:"baseURL"`
		Protocol string `json:"protocol,omitempty"` // 渠道协议：空＝OpenAI 兼容、anthropic、responses
		// Keys 多 Key 供应商的每把 Key（自定义渠道卡片逐把渲染）；预置提供方与
		// 公司通道没有多 Key 结构，这一项为空、前端退回单 Key 表单。
		Keys          []keyView  `json:"keys,omitempty"`
		APIKeyMasked  string     `json:"apiKeyMasked"`
		Preset        bool       `json:"preset"`
		PresetURL     string     `json:"presetURL,omitempty"`
		Models        []ModelCfg `json:"models"`
		Enabled       bool       `json:"enabled"`                 // 手动启停（解析后：nil=开）
		AutoDisabled  bool       `json:"autoDisabled,omitempty"`  // 公司通道自动停用（连不上服务端）
		Company       bool       `json:"company,omitempty"`       // 公司内部通道（wizardProviderID）
		PublicBaseURL string     `json:"publicBaseURL,omitempty"` // 外网备用地址（公司通道由服务端下发，只读展示）
		Via           string     `json:"via,omitempty"`           // 公司通道当前线路：lan/wan（配了外网地址才有值）
		// Quota 公司通道的额度快照（服务端随公告下发；老服务端/私人通道为空）
		Quota *CompanyQuotaCfg `json:"quota,omitempty"`
	}
	providers := []providerView{}
	for _, p := range a.Cfg.Providers {
		pv := providerView{
			ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, Protocol: p.Protocol,
			// 供应商级掩码取第一把 Key（多 Key 结构下供应商级 apiKey 只是镜像，
			// 内存里可能还没同步过来）
			APIKeyMasked: maskKey(p.KeyList()[0].APIKey), Models: p.Models,
			Enabled: p.Enabled == nil || *p.Enabled, AutoDisabled: p.AutoDisabled,
			Company: p.ID == wizardProviderID, PublicBaseURL: p.PublicBaseURL,
		}
		for _, k := range p.Keys {
			pv.Keys = append(pv.Keys, keyView{ID: k.ID, Label: k.Label, Protocol: k.Protocol,
				APIKeyMasked: maskKey(k.APIKey), Models: k.Models})
		}
		if pv.Company && p.PublicBaseURL != "" {
			if companyViaPublic() {
				pv.Via = "wan"
			} else {
				pv.Via = "lan"
			}
		}
		if pv.Company {
			pv.Quota = a.Cfg.CompanyQuota // 额度快照只在公司通道上展示
		}
		if tpl, ok := PresetByID(p.ID); ok {
			pv.Preset = true
			pv.PresetURL = tpl.BaseURL
		}
		providers = append(providers, pv)
	}
	type presetView struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		BaseURL  string `json:"baseURL"`
		Protocol string `json:"protocol,omitempty"`
		Note     string `json:"note,omitempty"`
	}
	presets := []presetView{}
	for _, p := range DefaultProviders() {
		presets = append(presets, presetView{ID: p.ID, Name: p.Name, BaseURL: p.BaseURL, Protocol: p.Protocol, Note: p.Note})
	}
	t := a.DefaultTarget()
	// 搜索服务的「当前生效地址」：交给前端原样显示（空/报错即「还打不出去」）
	searchBase, searchBaseErr := a.searchEndpointBase()
	searchBaseErrText := ""
	if searchBaseErr != nil {
		searchBaseErrText = searchBaseErr.Error()
	}
	writeJSON(w, 200, map[string]any{
		"baseURL":       t.BaseURL,
		"apiKeyMasked":  maskKey(t.APIKey),
		"model":         t.Model,
		"providers":     providers,
		"presets":       presets,
		"workspace":     a.Cfg.Workspace,
		"allowShell":    a.Cfg.AllowShell,
		"bindHost":      a.Cfg.BindHost,
		"lanIPs":        lanIPv4s(),
		"remoteAccess":  remoteStatus(a.Cfg.RemoteAccess),
		"marketURL":     a.Cfg.MarketURL,
		"publisherName": a.Cfg.PublisherName,
		// 搜索服务（web_search）：searchURL 填了就用它，空则跟随市场同源。
		// endpoint 是后端此刻真正会打的那个 base（含上面的回退结果），设置页
		// 直接把它显示给用户——「现在搜索走哪」一眼可见，不用自己推。
		"searchURL":         a.Cfg.SearchURL,
		"searchEndpoint":    searchBase,
		"searchEndpointErr": searchBaseErrText,
		// 公司接入的「身份三件套」：只用于向导表单预填（重跑接入向导、以及新版
		// 一次性重新接入失败时弹出的向导），不含任何密钥
		"wizardCompany":   a.Cfg.WizardCompany,
		"wizardServer":    a.Cfg.WizardServer,
		"wizardDone":      a.Cfg.WizardDone,
		"companyLinkOff":  a.Cfg.CompanyLinkOff, // 公司接入停用（设置·网络访问开关）：true=不弹向导、不连公司服务端
		"registryURL":     a.Cfg.RegistryURL,
		"updateURL":       a.Cfg.UpdateURL,
		"maxSteps":        a.Cfg.MaxSteps,
		"compactMode":     a.Cfg.CompactMode,
		"keepChars":       a.Cfg.KeepChars,
		"compactInterval": a.Cfg.CompactInterval,
		"compactUnit":     a.Cfg.CompactUnit,
		"compactLlmPct":   a.Cfg.CompactLlmPct,
		"snapshotEnabled": a.snapshotsEnabled(),
		"snapshotDays":    a.Cfg.SnapshotDays,
		// 会话浏览器 profile 保留天数（下发解析后的生效值：nil=默认 3）
		"browserProfileDays": a.browserProfileRetentionDays(),
		"compactThreshold":   a.Cfg.CompactThreshold,
		"accentColor":        a.Cfg.AccentColor,
		// 询问提醒（设置·常规）：下发解析后的生效值（enabled 已按 nil=开解析）
		"askAlert": map[string]any{
			"enabled":  a.Cfg.askAlertOn(),
			"afterSec": a.Cfg.askAlertAfterSec(),
			"text":     a.Cfg.askAlertText(),
		},
		// 排队消息自动续发（设置·常规）：下发解析后的生效值（nil=开）
		"queueAutoSend": a.Cfg.queueAutoSendOn(),
		// 关闭窗口驻留托盘（设置·常规，仅桌面版消费）：下发解析后的生效值（nil=开）
		"closeToTray": a.Cfg.closeToTrayOn(),
	})
}

func (a *App) handleSetConfig(w http.ResponseWriter, r *http.Request) {
	var body struct {
		BaseURL            *string `json:"baseURL"`
		APIKey             *string `json:"apiKey"` // 空串表示保持不变
		Model              *string `json:"model"`
		Workspace          *string `json:"workspace"`
		BindHost           *string `json:"bindHost"`
		RemoteEnabled      *bool   `json:"remoteEnabled"`  // 远程接入（公网中继）总开关，即时生效（保存即热启停）
		RemoteMyID         *string `json:"remoteMyId"`     // 远程接入设备号（手机入口 /a/<设备号>）
		RemotePairKey      *string `json:"remotePairKey"`  // 远程接入配对密钥（端到端加密密钥的种子）
		RemoteHost         *string `json:"remoteHost"`     // 中继网关地址（域名或 IP，不带协议与端口）
		RemotePort         *int    `json:"remotePort"`     // 中继网关端口
		MarketURL          *string `json:"marketURL"`      // 市场（技能/组件/插件）服务器清单地址
		SearchURL          *string `json:"searchURL"`      // 搜索服务地址（web_search 的 /api/search 网关；空=跟随市场同源）
		PublisherName      *string `json:"publisherName"`  // 发布者姓名（市场条目归属名；发布/删除自己条目时用）
		WizardDone         *bool   `json:"wizardDone"`     // 首启向导：完成/跳过（不再自动弹）
		CompanyLinkOff     *bool   `json:"companyLinkOff"` // 公司接入停用（设置·网络访问开关）：不弹向导、向导接口拒绝、同步器停转
		RegistryURL        *string `json:"registryURL"`    // 总服务器地址（向导发现不到公司时的查询兜底）
		UpdateURL          *string `json:"updateURL"`      // 更新检查发布源（"off"/空串关闭）
		UpdateMode         *string `json:"updateMode"`     // 更新方式：auto=分块静默（默认）/manual=仅提醒
		AllowShell         *bool   `json:"allowShell"`
		MaxSteps           *int    `json:"maxSteps"`
		SnapshotEnabled    *bool   `json:"snapshotEnabled"`    // 文件备份总开关（常规页）
		SnapshotDays       *int    `json:"snapshotDays"`       // 快照自动清理天数（-1=永不过期）
		BrowserProfileDays *int    `json:"browserProfileDays"` // 会话浏览器 profile 保留天数（0=不清理；nil=不改）
		Compact            *struct {
			Mode      string  `json:"mode"`
			KeepChars *int    `json:"keepChars"`
			Threshold float64 `json:"threshold"` // 旧字段（0.8/0.9）：兼容旧页面提交仍接受，新逻辑不读
			Interval  *int    `json:"interval"`  // 规则自动压缩间隔（0=关闭）
			Unit      string  `json:"unit"`      // 间隔计量单位：chars | tokens
			LlmPct    *int    `json:"llmPct"`    // 模型自动压缩阈值（0-100，0=关闭）
		} `json:"compact"`
		AccentColor *string `json:"accentColor"` // 主题色（#rrggbb）：设置页换点缀色时同步写进来
		AskAlert    *struct {
			Enabled  *bool   `json:"enabled"`  // 询问提醒开关（nil=不改；存 nil 时表示默认开）
			AfterSec *int    `json:"afterSec"` // 触发秒数（30-600，钳制后存）
			Text     *string `json:"text"`     // 朗读文案（空串=恢复默认）
		} `json:"askAlert"` // 询问提醒三键，嵌套部分合并：只传变的键
		QueueAutoSend *bool `json:"queueAutoSend"` // 排队消息自动续发（nil=不改；false=本轮结束后不自动发队列，排队坞给「继续发送」）
		CloseToTray   *bool `json:"closeToTray"`   // 关闭窗口驻留托盘（nil=不改；false=关窗即退出。仅桌面版消费，见 desktop_windows.go 关窗分支）
		Providers     *[]struct {
			ID      string     `json:"id"`
			Name    string     `json:"name"`
			BaseURL string     `json:"baseURL"`
			APIKey  string     `json:"apiKey"` // 空串表示沿用已保存的 key
			Models  []ModelCfg `json:"models"`
			// Keys 多 Key 供应商的每把 Key：带了就按它写（留空/不传则沿用已保存的
			// 多 Key 结构，普通保存不会把多分组冲成单 Key）
			Keys []ProviderKeyCfg `json:"keys"`
		} `json:"providers"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	if body.BaseURL != nil {
		a.Cfg.BaseURL = strings.TrimSpace(*body.BaseURL)
	}
	if body.APIKey != nil && *body.APIKey != "" {
		a.Cfg.APIKey = strings.TrimSpace(*body.APIKey)
	}
	if body.Model != nil {
		a.Cfg.Model = strings.TrimSpace(*body.Model)
	}
	if body.Providers != nil {
		merged := []ProviderCfg{}
		for _, in := range *body.Providers {
			pc := ProviderCfg{
				ID:      strings.TrimSpace(in.ID),
				Name:    strings.TrimSpace(in.Name),
				BaseURL: strings.TrimSpace(in.BaseURL),
				Models:  in.Models,
			}
			if pc.ID == "" || pc.BaseURL == "" {
				continue
			}
			old := a.providerByID(in.ID)
			if len(in.Keys) > 0 {
				keys, err := normalizeProviderKeys(in.Keys, old)
				if err != nil {
					writeErr(w, 400, err.Error())
					return
				}
				pc.Keys = keys
				if len(keys) > 0 {
					pc.APIKey, pc.Models = keys[0].APIKey, keys[0].Models
				}
			} else if old != nil && len(old.Keys) > 0 {
				pc.Keys, pc.APIKey, pc.Models = old.Keys, old.APIKey, old.Models // 没带多 Key：原样保留
			}
			if len(pc.Keys) == 0 {
				if in.APIKey != "" {
					pc.APIKey = strings.TrimSpace(in.APIKey)
				} else if old != nil {
					pc.APIKey = old.APIKey // 前端回传空 key = 保持不变
				}
			}
			merged = append(merged, pc)
		}
		if len(merged) > 0 {
			a.Cfg.Providers = merged
		}
	}
	if body.Workspace != nil {
		a.Cfg.Workspace = strings.TrimSpace(*body.Workspace)
	}
	if body.BindHost != nil {
		bh := strings.TrimSpace(*body.BindHost)
		// 只收 localhost 或 IP 字面量（含 0.0.0.0 / :: 通配），防设置页
		// 误传垃圾值把下次启动的监听地址写坏。
		if bh != "localhost" && bh != "0.0.0.0" && bh != "::" && net.ParseIP(bh) == nil {
			writeErr(w, 400, "bindHost 只能是 localhost 或 IP 地址")
			return
		}
		a.Cfg.BindHost = bh
	}
	oldRA := a.Cfg.RemoteAccess // 保存后比对：远程接入字段有变则热生效
	if body.RemoteEnabled != nil {
		a.Cfg.RemoteAccess.Enabled = *body.RemoteEnabled
	}
	// 远程访问身份（设备名/配对密钥）：网关确认制——中继开启且网关在线时，先在
	// 当前已注册连接上凭旧凭据完成网关侧改名/改密（防本地先改导致登记簿凭据
	// 脱节把自己锁死），成功才落盘；中继未启用时本地直改（无网关身份要同步）。
	var nextID, nextPK string
	var wantChange bool
	if v := body.RemoteMyID; v != nil {
		id := strings.TrimSpace(*v)
		if !validRemoteID(id) {
			writeErr(w, 400, "设备号需为 2~32 位字母/数字/中划线/下划线")
			return
		}
		nextID = id
		wantChange = true
	}
	if v := body.RemotePairKey; v != nil {
		pk := strings.TrimSpace(*v)
		if len(pk) < 8 || len(pk) > 64 || strings.ContainsFunc(pk, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			writeErr(w, 400, "配对密钥需为 8~64 位可打印字符")
			return
		}
		nextPK = pk
		wantChange = true
	}
	if wantChange {
		cur := a.Cfg.RemoteAccess
		if nextID == "" {
			nextID = cur.MyID
		}
		if nextPK == "" {
			nextPK = cur.PairKey
		}
		if (nextID != cur.MyID || nextPK != cur.PairKey) && cur.Enabled && cur.Host != "" {
			if err := a.requestHubUpdate(nextID, nextPK); err != nil {
				writeErr(w, 409, "网关未确认身份修改："+err.Error())
				return
			}
		}
		a.Cfg.RemoteAccess.MyID = nextID
		a.Cfg.RemoteAccess.PairKey = nextPK
	}
	if v := body.RemoteHost; v != nil {
		h := strings.TrimSpace(*v)
		if h != "" && !validRemoteHost(h) {
			writeErr(w, 400, "网关地址需为域名或 IP（不带 http:// 前缀与端口）")
			return
		}
		a.Cfg.RemoteAccess.Host = h
	}
	if v := body.RemotePort; v != nil {
		if *v < 1 || *v > 65535 {
			writeErr(w, 400, "网关端口需在 1~65535 之间")
			return
		}
		a.Cfg.RemoteAccess.Port = *v
	}
	if v := body.MarketURL; v != nil {
		u := strings.TrimSpace(*v)
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeErr(w, 400, "企业技能包 / 工作流 / 小程序平台地址需为 http(s) 的服务器地址（如 https://example.com:2227，清单路径后缀自动拼）")
			return
		}
		a.Cfg.MarketURL = u
		a.marketMu.Lock()
		a.marketCache = marketCacheEntry{} // 换市场源：缓存作废
		a.marketMu.Unlock()
	}
	// 搜索服务地址：留空 = 跟随市场同源（searchEndpointBase 的回退链不动），
	// 填了必须能解析出 http(s) 的 origin——保存时就拦住，不让它拖到用户搜
	// 索时才报错。错误口径与 web_search 运行期一致（searchURL 不合法）。
	if v := body.SearchURL; v != nil {
		u := strings.TrimSpace(*v)
		if u != "" {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				writeErr(w, 400, "searchURL 不合法: "+u+"（需为 http(s) 地址，如 https://search.example.com；留空则跟随市场服务器同源）")
				return
			}
			if _, err := marketOrigin(u); err != nil {
				writeErr(w, 400, "searchURL 不合法: "+u)
				return
			}
		}
		a.Cfg.SearchURL = u
	}
	if v := body.PublisherName; v != nil {
		name := strings.TrimSpace(*v)
		if len([]rune(name)) > 32 {
			writeErr(w, 400, "发布者姓名最长 32 字")
			return
		}
		a.Cfg.PublisherName = name
	}
	if v := body.WizardDone; v != nil {
		a.Cfg.WizardDone = *v
	}
	if v := body.CompanyLinkOff; v != nil {
		a.Cfg.CompanyLinkOff = *v
	}
	if v := body.RegistryURL; v != nil {
		u := strings.TrimSpace(*v)
		if u != "" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeErr(w, 400, "总服务器地址需为 http(s)（jxai-server 的地址）")
			return
		}
		a.Cfg.RegistryURL = u
	}
	if v := body.UpdateURL; v != nil {
		u := strings.TrimSpace(*v)
		if u != "" && u != "off" && !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			writeErr(w, 400, "发布源需为 http(s) 地址，或 off 关闭检查")
			return
		}
		a.Cfg.UpdateURL = u // "off" 落盘成字面量，加载时转成关闭
	}
	if v := body.UpdateMode; v != nil {
		m := strings.TrimSpace(*v)
		if m == "" {
			m = "auto"
		}
		if m != "auto" && m != "manual" {
			writeErr(w, 400, "更新方式只支持 auto（分块静默）或 manual（仅提醒）")
			return
		}
		a.Cfg.UpdateMode = m
	}
	if body.AllowShell != nil {
		a.Cfg.AllowShell = *body.AllowShell
	}
	// 步数上限：<=0 = 不限制（设置页「不限制」勾上时前端就发 0）；>0 = 手填的上限
	if body.MaxSteps != nil {
		if *body.MaxSteps > 0 {
			a.Cfg.MaxSteps = *body.MaxSteps
		} else {
			a.Cfg.MaxSteps = 0
		}
	}
	if body.SnapshotEnabled != nil {
		a.Cfg.SnapshotEnabled = body.SnapshotEnabled
	}
	if body.AccentColor != nil {
		c := strings.TrimSpace(*body.AccentColor)
		if c != "" && !strings.HasPrefix(c, "#") && len(c) == 7 {
			c = "#" + c // 容错：色板值一般带 #，裸值补上
		}
		a.Cfg.AccentColor = c
	}
	if body.SnapshotDays != nil && (*body.SnapshotDays > 0 || *body.SnapshotDays == -1) {
		a.Cfg.SnapshotDays = *body.SnapshotDays
	}
	if body.BrowserProfileDays != nil && *body.BrowserProfileDays >= 0 {
		a.Cfg.BrowserProfileDays = body.BrowserProfileDays
	}
	if body.Compact != nil {
		if body.Compact.Mode == "rule" || body.Compact.Mode == "llm" {
			a.Cfg.CompactMode = body.Compact.Mode // off 不再是合法值（自动压缩开关由两个阈值表达）
		}
		if body.Compact.KeepChars != nil && *body.Compact.KeepChars >= 0 {
			a.Cfg.KeepChars = *body.Compact.KeepChars // 0 合法：连最近一轮也不保留的全压语义
		}
		if body.Compact.Threshold > 0.5 && body.Compact.Threshold < 1 {
			a.Cfg.CompactThreshold = body.Compact.Threshold
		}
		clampPct := func(v int) int {
			if v < 0 {
				return 0
			}
			if v > 100 {
				return 100
			}
			return v
		}
		if body.Compact.Interval != nil {
			v := *body.Compact.Interval
			if v < 0 {
				v = 0
			}
			a.Cfg.CompactInterval = v
		}
		if body.Compact.Unit == "chars" || body.Compact.Unit == "tokens" {
			a.Cfg.CompactUnit = body.Compact.Unit
		}
		if body.Compact.LlmPct != nil {
			a.Cfg.CompactLlmPct = clampPct(*body.Compact.LlmPct)
		}
	}
	// 询问提醒三键合并（只传变的键）：afterSec 钳 30-600；text 去空白，空串
	// 存空（askAlertText 读取时回默认）。开关原样存（nil=默认开的语义在读取侧）。
	if body.AskAlert != nil {
		if body.AskAlert.Enabled != nil {
			a.Cfg.AskAlert.Enabled = body.AskAlert.Enabled
		}
		if body.AskAlert.AfterSec != nil {
			v := *body.AskAlert.AfterSec
			if v < 30 {
				v = 30
			}
			if v > 600 {
				v = 600
			}
			a.Cfg.AskAlert.AfterSec = v
		}
		if body.AskAlert.Text != nil {
			a.Cfg.AskAlert.Text = strings.TrimSpace(*body.AskAlert.Text)
		}
	}
	// 排队消息自动续发开关：原样存（读侧按「nil=默认开」解析，见 queueAutoSendOn）。
	// 关掉后本轮结束不自动发队列，排队坞给「继续发送」入口。
	if body.QueueAutoSend != nil {
		v := *body.QueueAutoSend
		a.Cfg.QueueAutoSend = &v
	}
	// 关闭窗口驻留托盘开关：原样存（读侧按「nil=默认开」解析，见 closeToTrayOn）。
	// 关掉后桌面版关窗即真正退出；改动即时生效，无需重启。
	if body.CloseToTray != nil {
		v := *body.CloseToTray
		a.Cfg.CloseToTray = &v
	}
	a.Cfg.syncLegacyFields()
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		writeErr(w, 500, "保存配置失败: "+err.Error())
		return
	}
	// 点缀色有变：顺手同步给语音输入法的联动文件（输入法胶囊底色跟着主题走）
	if body.AccentColor != nil {
		if err := a.writeVoicetypeLink(nil, nil, "", a.Cfg.AccentColor, ""); err != nil {
			log.Printf("[voicetype-link] 同步点缀色失败: %v", err)
		}
	}
	// 远程接入字段有变（开关/设备号/密钥/网关）：立即热启停，无需重启进程
	if a.Cfg.RemoteAccess != oldRA {
		a.syncRemoteAccess()
	}
	a.handleGetConfig(w, r)
}

// ---------- 演示记录器端点（用户直控：输入框旁的录制按钮，见 recorder.go） ----------

// demoRecordBody 起录/停录请求体：session 可选——在会话里点按钮时带上（停录后
// 【演示记录】通知落进该会话）；首页（还没开会话）录制时不带，材料照常落全局
// 默认工作区，建会话时由清单通知兜底。workspace 仅首页起录的兜底路径用。
type demoRecordBody struct {
	Session   string `json:"session"`
	Workspace string `json:"workspace"`
}

// demoRecordSession 解析可选的 session 参数并做守卫（存在、本地、创建技能模式）；
// 没带 session 时返回 (nil, nil) 表示首页起录。
func (a *App) demoRecordSession(id string) (*SessionMeta, error) {
	if id == "" {
		return nil, nil
	}
	m, err := a.Store.Get(id)
	if err != nil {
		return nil, fmt.Errorf("会话不存在：%s", id)
	}
	if m.Remote != nil {
		return nil, fmt.Errorf("演示记录器仅支持本地会话（当前挂载的是 SSH 远程工作区）")
	}
	if m.Mode != "skill-create" {
		return nil, fmt.Errorf("演示录制按钮只在「创建技能」模式的会话里可用")
	}
	return m, nil
}

// handleDemoRecordStart 用户点录制按钮起录。用户主动操作与文件上传同哲学，
// 不走模型权限审批；首页也允许（没开会话时材料落全局默认工作区）。
func (a *App) handleDemoRecordStart(w http.ResponseWriter, r *http.Request) {
	var body demoRecordBody
	_ = json.NewDecoder(r.Body).Decode(&body) // 允许空请求体
	if !recorderSupported() {
		writeErr(w, 400, "演示记录器仅支持 Windows 桌面")
		return
	}
	sess, err := a.demoRecordSession(body.Session)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	ws := ""
	if sess != nil {
		ws = sess.Workspace
	}
	if ws == "" {
		ws = body.Workspace
	}
	if ws == "" {
		ws = a.Cfg.Workspace
	}
	dir := filepath.Join(ws, recorderBaseDir, recorderDirPrefix+time.Now().Format("20060102-150405"))
	if err := os.MkdirAll(dir, 0755); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if err := recorderStart(dir); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.broadcast(Event{Type: "demo_record_started", Output: filepath.ToSlash(dir)})
	writeJSON(w, 200, map[string]any{"ok": 1, "dir": filepath.ToSlash(dir)})
}

// handleDemoRecordStop 停录并收录材料（停钩子、存音频、生成 INDEX.md）；带
// session 时向该会话追加【演示记录】系统通知（同文件上传通知机制），模型下一轮
// 即可 read INDEX.md 开始分析。
func (a *App) handleDemoRecordStop(w http.ResponseWriter, r *http.Request) {
	var body demoRecordBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	sess, err := a.demoRecordSession(body.Session)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	stats, err := recorderStop()
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := buildRecorderIndex(stats); err != nil {
		log.Printf("演示记录 INDEX.md 生成失败：%v（%s 的 events.jsonl 仍可直接读）", err, stats.Dir)
	}
	if sess != nil {
		note := demoRecordStopNote(stats)
		_ = a.Store.AppendRow(sess.ID, &StoredMessage{Message: TextMessage("user", note), Kind: kindReport, Origin: "demo", CreatedAt: now()})
		a.broadcast(Event{Type: "session_updated", SessionID: sess.ID})
	}
	ev := Event{Type: "demo_record_stopped", Output: filepath.ToSlash(stats.Dir)}
	if sess != nil {
		ev.SessionID = sess.ID
	}
	a.broadcast(ev)
	writeJSON(w, 200, map[string]any{"ok": 1, "dir": filepath.ToSlash(stats.Dir),
		"ms": stats.MS, "events": stats.Events, "shots": stats.Shots, "audio": stats.AudioOK})
}

// handleDemoRecordStatus 录制按钮的初始态/恢复态查询：supported 决定按钮是否
// 可用，active+dir 决定显示 ▶（起录）还是 ■（停止收录）。
func (a *App) handleDemoRecordStatus(w http.ResponseWriter, r *http.Request) {
	active, dir := recorderActiveInfo()
	writeJSON(w, 200, map[string]any{"supported": recorderSupported(), "active": active, "dir": dir, "text": recorderStatusText()})
}

func (a *App) handleListSessions(w http.ResponseWriter, r *http.Request) {
	if parent := r.URL.Query().Get("parent"); parent != "" {
		list, err := a.Store.Children(parent)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		stops := a.Store.LastStops() // 异常中断标记（空=正常收口，见 Store.SetLastStop）
		pins := a.Store.PinnedTitles()
		for _, m := range list {
			m.LastStop = stops[m.ID]
			m.TitlePinned = pins[m.ID]
			if a.IsRunning(m.ID) {
				m.Status = "running"
			}
		}
		writeJSON(w, 200, map[string]any{"sessions": list})
		return
	}
	list, err := a.Store.List()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 执行状态以内存注册表为准：数据库那列只用于服务重启后的中断恢复；
	// 异常中断标记（侧栏「⚠ 已中断」）同理不读会话表，一次取齐后按 id 分发
	stops := a.Store.LastStops()
	pins := a.Store.PinnedTitles() // 标题钉住标记（右键「恢复自动命名」按它显隐）
	for _, m := range list {
		m.LastStop = stops[m.ID]
		m.TitlePinned = pins[m.ID]
		if m.Status != "running" && a.IsRunning(m.ID) {
			m.Status = "running"
		}
	}
	writeJSON(w, 200, map[string]any{"sessions": list})
}

func (a *App) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode           string           `json:"mode"`
		Workspace      string           `json:"workspace"`
		Title          string           `json:"title"`
		ParentID       string           `json:"parent"`
		Provider       string           `json:"provider"`
		Model          string           `json:"model"`
		ProviderKey    string           `json:"providerKey"` // 模型归属的那把 Key（多 Key 供应商）
		Permission     string           `json:"permission"`
		Thinking       string           `json:"thinking"`
		PlanMode       bool             `json:"planMode"`
		GoalMode       bool             `json:"goalMode"`
		GoalText       string           `json:"goalText"`
		Tools          []string         `json:"tools"`
		Remote         *RemoteWorkspace `json:"remote"`
		InheritContext bool             `json:"inheritContext"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // 允许空请求体
	// 建会话全部收口在 createSession（sessioncreate.go）：字段构造、权限默认、
	// 能力默认、分支克隆、历史继承、演示材料兜底——HTTP 人工建会话与
	// dispatch_task / dispatch_sub 派发建会话走同一处，「人工是什么效果，
	// 派发就是什么效果」。
	m, err := a.createSession(createSessionParams{
		Mode: body.Mode, Workspace: body.Workspace, Title: body.Title,
		ParentID: body.ParentID, Provider: body.Provider, Model: body.Model,
		ProviderKey: body.ProviderKey,
		Permission:  body.Permission, Thinking: body.Thinking,
		PlanMode: body.PlanMode, GoalMode: body.GoalMode, GoalText: body.GoalText,
		Tools: body.Tools, Remote: body.Remote,
		CloneFromParent: body.InheritContext, CopyHistory: body.InheritContext,
	})
	if err != nil {
		writeErr(w, createSessionErrCode(err), err.Error())
		return
	}
	writeJSON(w, 200, m)
}
func (a *App) sessionFromPath(w http.ResponseWriter, r *http.Request) *SessionMeta {
	m, err := a.Store.Get(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, "会话不存在")
		return nil
	}
	return m
}

// promptBrief 下发瘦身的比对基元：prompt 快照里只取与缓存相关的 sys/tools。
// env 逐轮必变（时间/环境），且其变化由独立的 env 行驱动前端展示，比较时忽略。
type promptBrief struct {
	Sys   string          `json:"sys"`
	Tools json.RawMessage `json:"tools"`
}

// trimMessagesForUI 对下发前端的消息副本做瘦身（只改本次响应的内存对象，库内原样）：
//   - request/audit 是审计工件：前端不展示（导出走 /api/sessions/{id}/export
//     服务端直读），正文不下发——与 checkpoint 的 Before 不下发同一思路；
//   - prompt 拼装快照与上一条的 sys+tools 相同 → 只发空壳 {}，前端跳过空壳、
//     只见变化行（规范 §4「只讲变化」的对应瘦身）；有变化才完整下发。
//
// 比对基于原始正文（上一条 prompt 的原始 sys/tools），空壳只是替换下发副本、
// 不参与比较，否则「A→A→B」会因中间行被掏空而误判为变化。
func trimMessagesForUI(msgs []*StoredMessage) {
	var last *promptBrief
	for _, m := range msgs {
		switch m.Kind {
		case kindRequest, kindAudit:
			m.Content = ""
		case kindPrompt:
			var cur promptBrief
			if json.Unmarshal([]byte(m.Text()), &cur) != nil {
				continue // 坏行原样下发（前端同样忽略坏行）
			}
			if last != nil && cur.Sys == last.Sys && bytes.Equal(cur.Tools, last.Tools) {
				m.Content = "{}"
				continue
			}
			cur.Tools = append(json.RawMessage(nil), cur.Tools...) // 自持副本，别引用解码缓冲
			last = &cur
		}
	}
}

// olderInfo 是分批加载时「切点之前还有多少没下发」+ 前端 groupTurns 的接力
// 基线：sys/tools 取切点前最后一条有效 prompt 快照（已被 trim 掏成 {} 的空壳
// 跳过——它的基线本就是更早那条）——窗口内首条真变更的 prompt 据此显示
// 「变更行」而非「首见」形态；envBoundary 是压缩摘要覆盖边界（模型已看不到
// 的 env 快照，前端同样不显示，与切点无关、按全量取最大值）。
type olderInfo struct {
	Count       int             `json:"count"`
	Sys         string          `json:"sys,omitempty"`
	Tools       json.RawMessage `json:"tools,omitempty"`
	EnvBoundary int64           `json:"envBoundary,omitempty"`
}

// isTurnStartMsg 判断某行是否回合起点（真实用户发言 / AI 发出的用户消息），
// 口径与前端 lastTurnStart 一致。旧库残留的注入通知曾是 kind 空的 user 行，
// 只能按正文前缀识别（规范 §5）——【 开头或 <goal_round> 的按通知处理，
// 不作为回合边界。
func isTurnStartMsg(m *StoredMessage) bool {
	if m.Role != "user" {
		return false
	}
	if m.Kind == "aiuser" {
		return true
	}
	if m.Kind != "" && m.Kind != "chat" {
		return false
	}
	txt := m.Text()
	return !strings.HasPrefix(txt, "【") && !strings.HasPrefix(txt, "<goal_round>")
}

// sliceMsgWindow 在全量消息（已 trim、已并入摘要行）上切出 [lo,hi) 的分批加载
// 窗口：lo 先按条数定，再**向前**对齐到回合起点（窗口扩大到含完整回合——若向
// 右缩，起点之前的残段会被后续批次的对齐再次跳过，永远不被渲染）；切点停在
// inherit 标记行之后时把标记行并入本批（继承段与其克隆历史不拆开）。
func sliceMsgWindow(msgs []*StoredMessage, lo, hi int) ([]*StoredMessage, olderInfo) {
	if lo < 0 {
		lo = 0
	}
	if hi > len(msgs) {
		hi = len(msgs)
	}
	if hi <= lo {
		return nil, olderInfo{Count: len(msgs)}
	}
	for i := lo; i >= 0; i-- {
		if isTurnStartMsg(msgs[i]) {
			lo = i
			break
		}
	}
	if lo == 1 && msgs[0].Kind == "inherit" {
		lo = 0
	}
	info := olderInfo{Count: lo}
	for i := 0; i < lo; i++ {
		if msgs[i].Kind != kindPrompt || msgs[i].Text() == "{}" {
			continue
		}
		var b promptBrief
		if json.Unmarshal([]byte(msgs[i].Text()), &b) == nil {
			info.Sys = b.Sys
			info.Tools = append(json.RawMessage(nil), b.Tools...) // 自持副本（同 trim 的习惯）
		}
	}
	for _, m := range msgs {
		if m.Kind == "summary" && m.CoversUpTo > info.EnvBoundary {
			info.EnvBoundary = m.CoversUpTo
		}
	}
	return msgs[lo:hi], info
}

func (a *App) handleGetSession(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	msgs, err := a.Store.MessagesWithSeq(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	trimMessagesForUI(msgs) // 下发瘦身：库内原样，只削响应副本（见函数注释）
	// 压缩摘要行并入下发副本（按 seq 归位到发生位置）：前端把每次模型摘要压缩
	// 显示为一枚常显卡片（覆盖到哪一条 + 点开看完整摘要）。MessagesWithSeq 的
	// 过滤口径不动（历史读取工具等仍拿不到摘要行），只补界面这一份。
	if sums, serr := a.Store.SummariesOf(m.ID); serr == nil && len(sums) > 0 {
		msgs = append(msgs, sums...)
		sort.Slice(msgs, func(i, j int) bool { return msgs[i].Seq < msgs[j].Seq })
	}
	trimMessagesForUI(msgs) // 下发瘦身：库内原样，只削响应副本（见函数注释）
	// 分批加载（超长对话）：不带参数 = 全量下发（旧调用方零影响）。带 tail /
	// before_seq 时切出最近一批（回合对齐，见 sliceMsgWindow），older 携带切点前
	// 剩余条数与分组基线——前端据 older 显示「上面还有 N 条」并按需再取上一批
	var olderPtr *olderInfo
	if n, terr := strconv.Atoi(r.URL.Query().Get("tail")); terr == nil && n > 0 {
		win, info := sliceMsgWindow(msgs, len(msgs)-n, len(msgs))
		msgs, olderPtr = win, &info
	} else if bs, berr := strconv.ParseInt(r.URL.Query().Get("before_seq"), 10, 64); berr == nil && bs > 0 {
		lim := 0
		if v, lerr := strconv.Atoi(r.URL.Query().Get("limit")); lerr == nil && v > 0 {
			lim = v
		}
		hi := sort.Search(len(msgs), func(i int) bool { return msgs[i].Seq >= bs })
		win, info := sliceMsgWindow(msgs, hi-lim, hi)
		msgs, olderPtr = win, &info
	}
	// running 与 pending 以内存运行时为准：刷新页面后据此自动接上执行流、
	// 恢复「已插话/已排队」的显示，不依赖刷新前的页面状态
	cps, err := a.Store.CheckpointsAfter(m.ID, 0) // seq 从 1 起，>0 即全部
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, cp := range cps {
		cp.Before = nil // 正文不下发，前端只做清单展示
	}
	dvs, err := a.Store.DeliveriesFor(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// meta 一并带上钉住标记：title_worker 的预检读 meta.titlePinned——此前只有
	// 顶层字段，预检永远落空，钉住会话每个节奏轮都白调一次模型再吃 409
	m.TitlePinned = a.titlePinned(m.ID)
	resp := map[string]any{
		"meta": m, "messages": msgs,
		"running": a.IsRunning(m.ID), "pending": a.pendingOf(m.ID),
		"checkpoints": cps, "deliveries": dvs,
		"error":       a.runError(m.ID), // 最近一次失败原因（输入框上方报错条 + 重试）
		"titlePinned": m.TitlePinned,    // 手动命名钉住：AI 自动标题（组件 session-title）停改
	}
	if olderPtr != nil {
		resp["older"] = *olderPtr // 分批加载：上面未下发的条数 + 分组基线（无此字段 = 全量）
	}
	writeJSON(w, 200, resp)
}

// messageTextOf 把消息正文文本化（轻量列表用）：字符串直取；分块数组拼 text 块；
// 其余罕见形态按 JSON 短打。完整原文仍以 GET /api/sessions/{id} 的渲染口径为准。
func messageTextOf(m Message) string {
	switch c := m.Content.(type) {
	case string:
		return c
	case []any:
		var sb strings.Builder
		for _, blk := range c {
			bm, _ := blk.(map[string]any)
			if t, _ := bm["text"].(string); t != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(t)
			}
		}
		return sb.String()
	default:
		raw, _ := json.Marshal(m.Content)
		return string(raw)
	}
}

// handleSessionMessages 会话消息的轻量文本列表（组件/辅助生成场景）：只回
// seq/role/text/origin，支持 role 过滤（如 ?role=user——prompt/request 等系统
// 行天然排除）与 limit（从最新往回收）。插话消息（origin=interject，执行中
// 插入的用户发言）默认排除——辅助场景按「正常轮次」计数时不应把插话算进去。
// 完整渲染口径仍是 GET /api/sessions/{id}，导出仍是 /export。
func (a *App) handleSessionMessages(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	role := strings.TrimSpace(r.URL.Query().Get("role"))
	limit := 0
	if v, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && v > 0 {
		limit = v
	}
	msgs, err := a.Store.MessagesWithSeq(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	out := []map[string]any{}
	for _, sm := range msgs {
		if role != "" && sm.Role != role {
			continue
		}
		if sm.Origin == "interject" {
			continue // 插话不计入轮次类场景（AI 自动标题的口径）
		}
		text := messageTextOf(sm.Message)
		if strings.TrimSpace(text) == "" {
			continue // 附件注记/控制行等无文本行不占列表
		}
		out = append(out, map[string]any{"seq": sm.Seq, "role": sm.Role, "text": text, "origin": sm.Origin})
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	writeJSON(w, 200, map[string]any{"messages": out})
}

func (a *App) handlePatchSession(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Title       *string   `json:"title"`
		Workspace   *string   `json:"workspace"`
		Provider    *string   `json:"provider"`
		Model       *string   `json:"model"`
		ProviderKey *string   `json:"providerKey"` // 模型归属的那把 Key（多 Key 供应商）
		Permission  *string   `json:"permission"`
		Thinking    *string   `json:"thinking"`
		PlanMode    *bool     `json:"planMode"`
		GoalMode    *bool     `json:"goalMode"`
		GoalText    *string   `json:"goalText"`
		Tools       *[]string `json:"tools"`
		Archived    *bool     `json:"archived"`
		Unread      *bool     `json:"unread"`
		// AI 自动标题（组件 session-title）的通道语义：autoTitle=true 是组件改
		// 标题——会话被手动命名（钉住）时 409 拒绝，成功也不打钉；autoTitleUnpin
		// =true 解除钉住（恢复自动改名）。不带这两项的普通 title 修改视为用户
		// 手动命名，改完即钉住（对齐老 DSH「user 来源标题钉住」的决策）。
		AutoTitle      *bool `json:"autoTitle"`
		AutoTitleUnpin *bool `json:"autoTitleUnpin"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	// 上下文事件收集：字段有实际变化才记，落库成功后经总线 Notify（导出时间线呈现）
	type ctxNote struct{ kind, detail string }
	var notes []ctxNote
	titleWas := m.Title // 标题是否真的被改：落库后广播 title 事件（前端侧栏/顶栏跟进）
	if body.AutoTitleUnpin != nil && *body.AutoTitleUnpin {
		_ = a.Store.SetSessionData(m.ID, titlePinKey, "")
	}
	if body.Title != nil {
		// 60 字：标题结构是「核心段（8-12 字）：完整描述」，描述几十字方便
		// 搜索与悬停阅读（侧栏列表本身按宽度省略号截断显示，长标题不影响列表）。
		title := truncateRunes(*body.Title, 60)
		if body.AutoTitle != nil && *body.AutoTitle {
			if a.titlePinned(m.ID) {
				writeErr(w, 409, "标题已被用户手动命名（钉住），自动改名停用；可右键该会话选「恢复自动命名」")
				return
			}
			m.Title = title
		} else {
			m.Title = title
			if strings.TrimSpace(title) != "" {
				_ = a.Store.SetSessionData(m.ID, titlePinKey, "1") // 手动命名即钉住
			}
		}
	}
	if body.Workspace != nil {
		oldWS := m.Workspace
		m.Workspace = strings.TrimSpace(*body.Workspace)
		if oldWS != m.Workspace {
			notes = append(notes, ctxNote{ctxWorkspace, "工作目录：" + oldWS + " → " + m.Workspace})
			defer a.scheduleResidualCleanup(oldWS) // 工作区切换：清旧工作区残留
		}
	}
	if body.Provider != nil {
		old := m.Provider
		m.Provider = strings.TrimSpace(*body.Provider)
		if old != m.Provider {
			// 换供应商：Key 归属跟着换（除非本次同时显式给了新的归属），
			// 否则新供应商下会拿旧供应商的 Key id 去解析密钥
			if body.ProviderKey == nil {
				m.ProviderKey = ""
			}
			notes = append(notes, ctxNote{ctxModel, "提供方：" + old + " → " + m.Provider})
		}
	}
	if body.ProviderKey != nil {
		// 模型归属的 Key（多 Key 供应商重名模型的唯一定位依据）：不记上下文
		// 事件——用户选的还是同一个模型名，只是落到了另一把 Key 上
		m.ProviderKey = strings.TrimSpace(*body.ProviderKey)
	}
	if body.Model != nil {
		old := m.Model
		m.Model = strings.TrimSpace(*body.Model)
		if old != m.Model {
			notes = append(notes, ctxNote{ctxModel, "模型：" + old + " → " + m.Model})
		}
	}
	permWas := permModeOf(m)                          // 权限旧值：SaveMeta 后生成【权限变更】注入用
	permWasTools := append([]string(nil), m.Tools...) //（须在 Permission/Tools 两个分支改动之前捕获）
	if body.Permission != nil {
		was := permModeOf(m)
		wasTools := m.Tools
		// 权限档落装与能力联动收口在 applyPermissionChange（sessioncreate.go），
		// 与 send_to_session 的 permission 参数同一套；这里只补 HTTP 侧的
		// 上下文事件记录。
		capsChanged := applyPermissionChange(m, *body.Permission, body.Tools != nil)
		if was != permModeOf(m) {
			notes = append(notes, ctxNote{ctxPerm, "权限模式：" + was + " → " + permModeOf(m)})
		} else if capsChanged && body.Tools == nil {
			notes = append(notes, ctxNote{ctxPerm, "能力开关随权限联动：" + toolIDsOf(wasTools) + " → " + toolIDsOf(m.Tools)})
		}
	}
	if body.Thinking != nil {
		// 档位全系统固定六档（thinkingLevels），与模型/协议无关，只挡非法值；
		// 老值兼容：disabled → none、enabled → 空（跟随服务端默认）。
		lv, ok := normalizeThinkingLevel(*body.Thinking)
		if !ok {
			writeErr(w, 400, "thinking 档位必须是 none/low/medium/high/xhigh/max 或空")
			return
		}
		old := m.Thinking
		m.Thinking = lv
		if old != lv {
			notes = append(notes, ctxNote{ctxThinking, "思考档位：" + old + " → " + lv})
		}
	}
	if body.PlanMode != nil {
		old := m.PlanMode
		m.PlanMode = *body.PlanMode
		if old != m.PlanMode {
			notes = append(notes, ctxNote{ctxMode, "计划模式：" + boolZh(old) + " → " + boolZh(m.PlanMode)})
		}
	}
	if body.GoalText != nil {
		m.GoalText = truncateRunes(*body.GoalText, 2000)
	}
	if body.GoalMode != nil {
		old := m.GoalMode
		m.GoalMode = *body.GoalMode
		if old != m.GoalMode {
			notes = append(notes, ctxNote{ctxMode, "目标模式：" + boolZh(old) + " → " + boolZh(m.GoalMode)})
		}
		// 开启/重开目标模式：状态归位 active；关闭时清状态（目标文本保留备查）
		if m.GoalMode {
			m.GoalStatus = "active"
		} else {
			m.GoalStatus = ""
		}
	}
	if body.Archived != nil {
		m.Archived = *body.Archived
	}
	if body.Unread != nil {
		m.Unread = *body.Unread
	}
	if body.Tools != nil {
		old := m.Tools
		m.Tools = *body.Tools
		if fmt.Sprint(old) != fmt.Sprint(m.Tools) {
			notes = append(notes, ctxNote{ctxPerm, "能力开关：" + toolIDsOf(old) + " → " + toolIDsOf(m.Tools)})
		}
	}
	if err := a.Store.SaveMeta(m); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	for _, n := range notes {
		a.ctxEngine().Notify(m, n.kind, n.detail)
	}
	// 标题被改（AI 自动命名组件晚到落库 / 别处窗口手动改名）：广播给所有在线
	// 前端翻新侧栏与顶栏。不加这条的话，组件的改名常在流结束数秒后才落库，而
	// 侧栏只在流结束那一刻刷新一次——用户当面永远看不到标题变化，误以为自动
	// 命名从未生效（10-21 实测排查的根因）。
	if m.Title != titleWas {
		a.broadcast(Event{Type: "title", SessionID: m.ID, Title: m.Title})
	}
	// 环境快照正管的条目（权限/能力/计划目标/工作目录）在 PATCH 落库后立即
	// 重算并按需落新快照——变更瞬间即进时间线与下一次组装的上下文，不等下一
	// 次模型调用；下一次组装做等值比较，不会重复落。env「变化驱动」约束的是
	// 发给模型的差分重发，不约束落库时机。权限变更注记随这份快照落库；
	// emit 广播带 SessionID：空闲会话的前端也能实时翻新「运行环境更新」。
	envTouched, permTouched := false, false
	for _, n := range notes {
		if n.kind == ctxPerm || n.kind == ctxMode || n.kind == ctxWorkspace {
			envTouched = true
		}
		if n.kind == ctxPerm {
			permTouched = true
		}
	}
	if permTouched {
		// 权限档/能力开关变了：补广播一条自带新值的 session_updated。此前 PATCH
		// 只有 env 行一条通道，前端要靠「事件→再拉列表」取数，而列表请求有防抖
		// 合并，流式执行中事件密集时别处窗口拿到旧列表——权限胶囊停在旧档
		// （多窗口权限互不同步的根因）。
		a.broadcast(Event{Type: "session_updated", SessionID: m.ID, Permission: permModeOf(m), Tools: m.Tools})
	}
	if envTouched {
		if note := permChangeNote(permSrcPanel, permWas, permWasTools, m); note != "" {
			a.stashPermNote(m.ID, note)
		}
		// 环境快照行同时进「全局广播」与「本会话执行流」：空闲会话靠前者翻新，
		// 运行中的会话靠后者把「运行环境更新」行实时推进流式视图——只广播的话
		// 流式中的前端会跳过刷新（msgs 数组里没有这行），权限变更提示就看不见
		emit := func(ev Event) {
			ev.SessionID = m.ID
			if r := a.runOf(m.ID); r != nil {
				r.observe(ev)
				r.publish(ev)
			}
			a.broadcast(ev)
		}
		a.maybeAppendEnvSnapshot(m, a.envPrompt(m), emit)
	}
	writeJSON(w, 200, m)
}

// toolIDsOf / boolZh 是上下文事件说明的小格式化辅助（人读一句话进时间线）。
func toolIDsOf(ids []string) string {
	if len(ids) == 0 {
		return "（无）"
	}
	return strings.Join(ids, ",")
}
func boolZh(b bool) string {
	if b {
		return "开"
	}
	return "关"
}

func (a *App) handleDeleteSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	oldWS := ""
	if m, err := a.Store.Get(id); err == nil {
		oldWS = m.Workspace
	}
	a.closeRemoteSession(id) // 断开该会话的 SSH 连接
	if err := a.Store.Delete(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.SessionCX.forget(id) // 会话没了：它的组件/插件覆盖记录一并摘除
	// 上传登记与待执行交付一并清掉：用户删会话即放弃这些（done/failed 的
	// 历史作业行留着无妨，量小且可追溯）
	_ = a.Store.DeleteUploadsForSession(id)
	_ = a.Store.DeletePendingJobsForSession(id)
	// 本会话的 run_python 脚本目录一并回收（.jxai/tmp/<会话id>/，见 run_python.go；
	// 会话没专属工作区时脚本落在全局配置工作区）
	pyWs := oldWS
	if strings.TrimSpace(pyWs) == "" {
		pyWs = a.Cfg.Workspace
	}
	_ = os.RemoveAll(filepath.Join(pyWs, ".jxai", "tmp", id))
	a.scheduleResidualCleanup(oldWS) // 会话结束：清本产品的常驻授权残留
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// scheduleResidualCleanup 异步清理旧工作区上本产品的沙箱常驻授权残留
// （S-1-80-* 合成 ACE 与 Low 标签）。仅当没有任何会话仍引用该工作区时执行
// （常驻授权是跨会话复用缓存，有人用就留着）；DSH 官方 S-1-4-* 旧残留属
// 用户确认范畴（sandbox acl-audit --revoke）。失败不影响主流程。
func (a *App) scheduleResidualCleanup(workspace string) {
	ws := strings.TrimSpace(workspace)
	if ws == "" {
		return
	}
	go func() {
		sessions, err := a.Store.List()
		if err != nil {
			return
		}
		for _, s := range sessions {
			if s.Workspace == ws {
				return // 仍有会话引用，保留常驻授权
			}
		}
		if n, err := cleanupResidualGrants([]string{ws}); err == nil && n > 0 {
			log.Printf("已清理工作区 %s 的 %d 项沙箱残留授权", ws, n)
		}
	}()
}

// startupIntegrityLabelRestore 启动期把历史密级沙箱（机制已撤除）打在
// 工作区上的常驻 Low 完整性标签还原为 Medium。收集配置工作区与全部会话
// 工作区，异步执行、失败只记日志（WO 引导三步见 restoreStaleIntegrityLabels）。
func (a *App) startupIntegrityLabelRestore(primary string) {
	roots := map[string]bool{}
	if p := strings.TrimSpace(primary); p != "" {
		roots[p] = true
	}
	if a.Store != nil {
		if ss, err := a.Store.List(); err == nil {
			for _, s := range ss {
				if p := strings.TrimSpace(s.Workspace); p != "" {
					roots[p] = true
				}
			}
		}
	}
	if len(roots) == 0 {
		return
	}
	list := make([]string, 0, len(roots))
	for p := range roots {
		list = append(list, p)
	}
	n, err := restoreStaleIntegrityLabels(list)
	switch {
	case err != nil:
		log.Printf("还原历史 Low 完整性标签：完成 %d 个工作区后部分失败: %v", n, err)
	case n > 0:
		log.Printf("已还原历史密级沙箱残留的 Low 完整性标签（%d 个工作区: %v）", n, list)
	}
}

// handleCompact 手动压缩：按按钮位置（body.upTo）压缩该回合及更早的内容。
// CompactMode（设置页「手动压缩按钮的默认行为」）决定走哪条路：rule 即时规则
// 折叠（零模型调用，毫秒级）；llm 生成覆盖到该轮的摘要。空请求体时按自动压缩
// 的同一字数预算划保留范围。已有摘要永不重压，多次叠加。
func (a *App) handleCompact(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	if a.IsRunning(m.ID) {
		writeErr(w, 409, "该会话正在执行，稍后再压缩")
		return
	}
	// Claude Code 插件：PreCompact hook（阻断即取消本次压缩）
	if a.CCPlugins != nil && a.CCPlugins.hasAnyHooks() {
		if oc := a.ccEmitHookEvent(r.Context(), m, "PreCompact", "", nil, nil); oc.Block {
			writeErr(w, 403, "压缩被插件 hook 拒绝："+oc.Reason)
			return
		}
	}
	var body struct {
		UpTo int64 `json:"upTo"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if a.Cfg.CompactMode == "rule" {
		folded, err := 0, error(nil)
		if body.UpTo > 0 {
			folded, err = a.ruleCompactTo(m, body.UpTo)
		} else if folded = a.ruleCompactAuto(m, -1); folded == 0 {
			err = fmt.Errorf("没有可折叠的回合（保留最近约 %d 字）", a.Cfg.KeepChars)
		}
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
		writeJSON(w, 200, map[string]any{"ok": 1, "folded": folded})
		return
	}
	// 模型摘要走后台：大历史的摘要要等模型流式输出一两分钟，挂在 HTTP 请求上
	// 会把按钮吊在「压缩中…」（切走对话回来状态全丢，只能再点一次——又撞上
	// 「该范围已压缩过」）。这里同步做完预检（点错位置/已压过立即报错）就返回
	// 202，模型调用放 goroutine 悄悄跑，完成与失败都经 session_updated 广播，
	// 前端不等待、切走对话不影响（CallSummarizer 本就用独立 context）。
	if a.compactBusyOf(m.ID) {
		writeErr(w, 409, "该会话正在后台压缩，完成或失败会自动通知")
		return
	}
	raw, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if _, _, _, err := a.summarizePlan(raw, -1, body.UpTo); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.setCompactBusy(m.ID, true)
	a.summarizeAsync(m, "manual", -1, body.UpTo)
	writeJSON(w, 202, map[string]any{"ok": 1, "async": true})
}

// compactBusyOf / setCompactBusy 手动模型摘要压缩的会话级进行中标记：摘要走
// 后台 goroutine，这里挡住同一会话的重复点火——连点两次不会并发跑两个摘要
// 调用（第二个请求直接 409「正在后台压缩」）。
func (a *App) compactBusyOf(id string) bool {
	a.compactRunMu.Lock()
	defer a.compactRunMu.Unlock()
	return a.compactRunning[id]
}

func (a *App) setCompactBusy(id string, on bool) {
	a.compactRunMu.Lock()
	defer a.compactRunMu.Unlock()
	if a.compactRunning == nil {
		a.compactRunning = map[string]bool{}
	}
	if on {
		a.compactRunning[id] = true
	} else {
		delete(a.compactRunning, id)
	}
}

// summarizeAsync 把一次模型摘要放到后台执行（手动压缩的「悄悄压缩」形态）：
// 先广播 compact_started 让前端进入「后台压缩中」状态，goroutine 里完成摘要
// 与落库（doSummarize 成功时自带 reason=compact 广播）；失败广播 compact_failed。
// 调用方必须已置位 compactRunning（先 set 再调，防两个请求同时穿过忙检查）。
func (a *App) summarizeAsync(sess *SessionMeta, origin string, keepTail int, upTo int64) {
	a.broadcast(Event{Type: "session_updated", SessionID: sess.ID, Data: map[string]any{"reason": "compact_started"}})
	go func() {
		defer a.setCompactBusy(sess.ID, false)
		if _, err := a.doSummarize(sess, origin, keepTail, upTo); err != nil {
			a.broadcast(Event{Type: "session_updated", SessionID: sess.ID, Data: map[string]any{"reason": "compact_failed", "error": err.Error()}})
		}
	}()
}

// handleAutoCompact 右键上下文菜单「提前自动压缩」：对当前会话手动点火一次
// **自动压缩线路**（autoCompactKick）——按设置的保留字数从最新一轮往前数、
// 装得下的整轮保留，更早的压缩；规则折叠还是模型摘要按当前配置选路。与
// 消息操作条上的「压缩」（handleCompact 手动全量：从指定轮往上全压）和设置页
// 「压缩数字助理主对话」（固定模型摘要）是三个并存、各自明确的入口。
// 执行中拒绝（409）；PreCompact 插件 hook 与其他压缩入口同一口径（可被阻断）。
func (a *App) handleAutoCompact(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	if a.IsRunning(m.ID) {
		writeErr(w, 409, "该会话正在执行，稍后再压缩")
		return
	}
	if a.CCPlugins != nil && a.CCPlugins.hasAnyHooks() {
		if oc := a.ccEmitHookEvent(r.Context(), m, "PreCompact", "", nil, nil); oc.Block {
			writeErr(w, 403, "压缩被插件 hook 拒绝："+oc.Reason)
			return
		}
	}
	folded, async, err := a.autoCompactKick(m)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if async { // 模型摘要放后台（同 handleCompact：立即返回，事件驱动刷新）
		writeJSON(w, 202, map[string]any{"ok": 1, "scope": "auto", "async": true})
		return
	}
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	writeJSON(w, 200, map[string]any{"ok": 1, "scope": "auto", "folded": folded})
}

// autoCompactKick 提前点火一次自动压缩线路（通道选择、先后顺序、调用参数与
// 阈值触发一字不差，只是不做「到没到阈值」的判定）：规则通道零模型调用、
// 同步折完即返回；模型摘要通道返回 async=true 由调用方放后台（同一会话级
// 互斥与事件通知，见 summarizeAsync）。规则通道折到内容即止（与自动线路
// 同样提前返回，不再试摘要）；两通道都没启用时报错，通道启用但预算内没东西
// 可压的报错由下层透传（没有可折叠的回合 / 没有可压缩的区间）。
func (a *App) autoCompactKick(m *SessionMeta) (folded int, async bool, err error) {
	if a.Cfg.CompactInterval > 0 {
		if folded = a.ruleCompactAuto(m, -1); folded > 0 {
			return folded, false, nil
		}
		if a.Cfg.CompactLlmPct <= 0 {
			return 0, false, fmt.Errorf("没有可折叠的回合（保留最近约 %d 字）", a.Cfg.KeepChars)
		}
	}
	if a.Cfg.CompactLlmPct <= 0 {
		return 0, false, fmt.Errorf("自动压缩未启用：在 设置 · 对话历史与压缩 里把「规则压缩间隔」或「模型摘要阈值」设为大于 0")
	}
	if a.compactBusyOf(m.ID) {
		return 0, false, fmt.Errorf("该会话正在后台压缩，完成或失败会自动通知")
	}
	raw, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		return 0, false, err
	}
	if _, _, _, err := a.summarizePlan(raw, -1, 0); err != nil {
		return 0, false, err
	}
	a.setCompactBusy(m.ID, true)
	a.summarizeAsync(m, "auto", -1, 0)
	return 0, true, nil
}

// 数字助理主对话现在有多个「版本」（设置 · 对话历史与压缩），当前是哪一版
// 数字助理主对话现在有多个「版本」（设置 · 对话历史与压缩），当前是哪一版
// 由库里的指针说了算——a.currentAssistant()，见 assistant_versions.go。
// 以前这里是「会话列表按更新时间倒序取第一个」，多版本下会错位（切回老版本
// 聊一句，它就成了「最近更新的那个」），已彻底收敛到那个唯一口径。

// lastManualSummaryCover 返回会话里「手动」摘要覆盖到的最远 seq（没有则 0）。
// 设置页的压缩按钮要在对话区留一条系统通知，文案里要有「这次压到哪一条」——
// 摘要行本身只带 covers_up_to（见 doSummarize），这里读出来供注记使用。
func (a *App) lastManualSummaryCover(sessionID string) int64 {
	raw, err := a.Store.RawMessagesAll(sessionID)
	if err != nil {
		return 0
	}
	cover := int64(0)
	for _, m := range raw {
		if m.Kind == kindSummary && m.Origin == "manual" && m.CoversUpTo > cover {
			cover = m.CoversUpTo
		}
	}
	return cover
}

// handleAssistantClear 清空数字助理主对话**当前这一版**的历史（接口保留，
// 设置页已不再提供按钮——那里换成了「新建一版」，见 assistant_versions.go）。
// 服务端只做真活：消息与随消息产生的工件（文件快照登记与正文文件、交付登记、
// 上传登记、待执行交付作业）全部清掉、会话运行态归零（status=idle/未读/用量），
// 并丢掉待生效的插话与排队——清完这一版就是一个全新的同一个对话（会话 id、
// 标题、工作目录、权限、模型、版本号不动，子会话与待办记录也不受影响，别的
// 版本更是一点不碰）。执行中一律拒绝（409）：正在跑的一轮会继续往历史里写，
// 清了也会立刻被冲回来。
func (a *App) handleAssistantClear(w http.ResponseWriter, r *http.Request) {
	m := a.currentAssistant()
	if m == nil {
		writeErr(w, 404, "还没有数字助理主对话，无需清空")
		return
	}
	if a.IsRunning(m.ID) {
		writeErr(w, 409, "数字助理主对话正在执行，等这一轮跑完再清空")
		return
	}
	// 快照正文文件要按 id 删（行删掉后就查不到 id 了），先取清单
	cps, _ := a.Store.CheckpointsAfter(m.ID, 0)
	ids := make([]int64, 0, len(cps))
	for _, cp := range cps {
		ids = append(ids, cp.ID)
	}
	n, err := a.Store.ClearSession(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if len(ids) > 0 { // .jxai/snapshots/<会话id>/ 下的正文文件随行删除，不留孤儿
		(&toolContext{app: a, sess: m}).deleteSnapshotFiles(ids)
	}
	a.dropPending(m.ID)   // 待生效的插话/排队一并丢弃（界面上的排队坞随之清空）
	a.clearRunError(m.ID) // 输入框上方的报错条也随历史一起消失
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	log.Printf("[设置] 已清空数字助理主对话 %s（%d 条消息）", m.ID, n)
	writeJSON(w, 200, map[string]any{"ok": 1, "sessionId": m.ID, "cleared": n})
}

// handleAssistantCompact 立即压缩数字助理主对话**当前这一版**（设置 · 对话历史
// 与压缩 · 压缩按钮）。固定走上下文压缩里「AI 模型压缩」那条路（doSummarize）：无视
// Cfg.CompactMode（规则/关闭都不看），保留窗口与区间划分原样沿用现有实现
// （keepRoundsByBudget：从最近一轮往上数，保留「保留字数」预算内装得下的
// 完整轮；最近一轮自身超预算时摘要覆盖全部历史，不硬保这一轮）。
// 设计目的：数字助理主对话平时也会自动压缩，默认走规则引擎（规则折叠）；等
// 规则引擎也压不动、上下文仍然过长时，用户才会来点这个按钮——所以这里到点
// 一定是模型压缩。origin=manual 与对话里消息级「压缩」按钮的 llm 分支同款
// （独立叠加、永不重压）；没东西可压时透传现有错误文案，不另造话术。
// 执行中拒绝（409）；PreCompact 插件 hook 与手动压缩同一口径（可被阻断）。
// 压完在对话区补一条【已压缩】系统通知：摘要行只是上下文材料、不进普通
// 消息流，用户在对话里见不到它（通知同时是模型可见的 user 消息，下一轮模型
// 自然知道历史已被压缩，与消息级「已折叠」边界提示同一口径）。
func (a *App) handleAssistantCompact(w http.ResponseWriter, r *http.Request) {
	m := a.currentAssistant()
	if m == nil {
		writeErr(w, 404, "还没有数字助理主对话，没有可压缩的历史")
		return
	}
	if a.IsRunning(m.ID) {
		writeErr(w, 409, "数字助理主对话正在执行，等这一轮跑完再压缩")
		return
	}
	if a.CCPlugins != nil && a.CCPlugins.hasAnyHooks() {
		if oc := a.ccEmitHookEvent(r.Context(), m, "PreCompact", "", nil, nil); oc.Block {
			writeErr(w, 403, "压缩被插件 hook 拒绝："+oc.Reason)
			return
		}
	}
	summary, err := a.doSummarize(m, "manual", -1, 0) // 与 llm 模式自动/手动压缩同一函数、同一保留窗口
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.landUserSide(m, fmt.Sprintf(
		"【已压缩】数字助理主对话已用模型摘要压缩：覆盖至第 %d 条历史，摘要 %d 字。摘要开头：%s",
		a.lastManualSummaryCover(m.ID), len([]rune(summary)), clipText(summary, 200)),
		kindReport, "compact", nil)
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	log.Printf("[设置] 已压缩数字助理主对话 %s（模型摘要 %d 字）", m.ID, len([]rune(summary)))
	writeJSON(w, 200, map[string]any{"ok": 1, "sessionId": m.ID, "strategy": "llm",
		"summary": truncateRunes(summary, 300)})
}

// handleExport 导出两类审计材料，统一向上下文引擎申请（引擎是记录者）：
//
//	?format=md（默认）——运行日志：从会话创建起的完整时间线（上下文组装、
//	  环境快照更新、用户消息、每次模型调用的前缀指纹/用量/缓存命中、模型逐步
//	  输出、报错/停止/压缩/上下文事件等未发给模型的事件）；
//	?format=json ——原始数据：引擎缓存的最近三轮真正发给模型的完整请求体
//	  （与线上发送逐字节同源）+ 相邻两两对比（消息条数/token 估算/工具目录
//	  指纹/首条变化消息位置），用于核对到底给模型发了什么、缓存为何变化。
//
// 两者都取自 messages 表原始历史，只导出当前会话（子会话各自单独导出）。
func (a *App) handleExport(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	stamp := time.Now().Format("20060102-150405")
	base := strings.ReplaceAll(truncateRunes(m.Title, 30), " ", "_") + "_" + stamp
	if r.URL.Query().Get("format") == "json" {
		data, err := a.ctxEngine().ExportRawRequests(m)
		if err != nil {
			writeErr(w, 404, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(base+"_模型请求.json"))
		_, _ = w.Write(data)
		return
	}
	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(base+"_运行日志.md"))
	_, _ = w.Write([]byte(a.ctxEngine().ExportTimeline(m)))
}

func wsOr(a *App, m *SessionMeta) string {
	if m.Workspace != "" {
		return m.Workspace
	}
	return a.Cfg.Workspace
}

// handleUpload 接收文件上传：保存进会话工作区，注记文本随响应返回——由调用方
// 拼进用户消息开头的「用户附件：」块（ATTACH_HEAD），随用户消息一起被模型看到。
// 两种载荷：multipart（浏览器/手机，只复制字节，原件路径拿不到）与
// JSON {path}（桌面端经 /api/pick-file 采集到的本机真实路径，会登记
// uploads 表并告知模型该路径可用作交付目标，见 handleUploadByPath）。
func (a *App) handleUpload(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	if strings.HasPrefix(strings.ToLower(r.Header.Get("Content-Type")), "application/json") {
		a.handleUploadByPath(w, r, m)
		return
	}
	// 256MB：办公 PPT 常带大媒体轻松超 32MB，旧上限会把大文件静默截断成坏文件
	// （LimitReader 截到 32MB 照写不报错）。回环/局域网传 256MB 是用户自己的选择。
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeErr(w, 400, "上传解析失败: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少 file 字段")
		return
	}
	defer file.Close()
	name := filepath.Base(header.Filename)
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `\/`) {
		writeErr(w, 400, "非法文件名")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	data, err := io.ReadAll(io.LimitReader(file, 256<<20))
	if err != nil {
		writeErr(w, 500, "读取上传内容失败: "+err.Error())
		return
	}
	// 判重与落位和桌面端路径上传（registerUploadPath）同一套规矩：内容一致的
	// 重传当成功处理、不重复落盘；同名不同内容顺延 -2/-3；统一存 .jxai/uploads
	// （不占工作区根目录），永不覆盖任何现有文件。
	uploadsDir, err := tctx.uploadsDirOf()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	dup := tctx.findUploadDuplicate(uploadsDir, fingerprintBytes(data))
	storedName := name
	if dup == "" {
		storedName = tctx.allocUploadName(uploadsDir, name)
		// 用户主动上传不走模型权限审批，直接写（本地或远程 SFTP）；目标永远是
		// 空闲的新名字，覆盖动作不存在
		if err := tctx.envWriteFile(tctx.envPathJoin(uploadsDir, storedName), data); err != nil {
			writeErr(w, 500, "写入工作区失败: "+err.Error())
			return
		}
	}
	dst := dup
	if dst == "" {
		dst = tctx.envPathJoin(uploadsDir, storedName)
	}
	// multipart 也登记 uploads 表（orig_path 留空——交付工具只认登记了原件
	// 路径的记录，无副作用），会话内上传台账与注记口径统一
	u := &Upload{SessionID: m.ID, OrigPath: "", CopyPath: dst, Name: name, Size: int64(len(data)), CreatedAt: now()}
	if err := a.Store.AddUpload(u); err != nil {
		writeErr(w, 500, "上传登记落库失败: "+err.Error())
		return
	}
	var lead string
	switch {
	case dup != "":
		ref := ""
		if uid := a.uploadDupRef(m, dup); uid != "" {
			ref = "（即 " + uid + " 那次上传的文件）"
		}
		lead = fmt.Sprintf("【文件上传】《%s》（%d 字节）：与工作区已有文件 %s 内容完全一致（大小与逐字节哈希比对通过）%s，当上传成功处理，没有重复复制。",
			name, len(data), filepath.ToSlash(dup), ref)
	case storedName != name:
		lead = fmt.Sprintf("【文件上传】《%s》（%d 字节）：工作区里已有同名的其他内容，这份存成了 %s（原有文件未被动过）；已保存到工作区 %s（用户上传的文件统一存放在 .jxai/uploads，不占工作区根目录）。",
			name, len(data), storedName, filepath.ToSlash(dst))
	default:
		lead = fmt.Sprintf("【文件上传】用户上传了文件 %s（%d 字节），已保存到工作区（用户上传的文件统一存放在 .jxai/uploads，不占工作区根目录）；需要了解内容时可用只读工具查看。",
			filepath.ToSlash(dst), len(data))
	}
	note := lead +
		"这次上传没有登记原件路径（浏览器、手机、远程、粘贴只送文件内容，拿不到本机路径），交付工具（deliver_file）无法把成品送回原处；" +
		"确实要交回原位置时，请让用户在桌面端把原文件直接拖进输入框、或用「＋→文件上传」选一次（那条路会登记原件位置）。"
	// 注记不独立落库：随响应交回，由前端拼进用户消息的「用户附件：」块
	writeJSON(w, 200, map[string]any{"ok": 1, "path": filepath.ToSlash(dst), "size": len(data), "note": note})
}

// handleUploadByPath 桌面端路径上传：前端经 /api/pick-file 拿到本机真实
// 路径后送到这里。与 multipart 不同的是原件路径被登记进 uploads 表——
// 它是 deliver_file 的交付目标来源，注记里会向模型说明这一点。
// 原件就在工作区内时不做复制（避免自己覆盖自己），只登记。
// 具体动作在 registerUploadPath（uploadpath.go）：数字助理派发的附件
// （dispatch_task 的 attachments）复用同一份实现，人工上传与派发附件效果
// 完全一致。
func (a *App) handleUploadByPath(w http.ResponseWriter, r *http.Request, m *SessionMeta) {
	var p struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&p); err != nil {
		writeErr(w, 400, "请求体解析失败: "+err.Error())
		return
	}
	u, note, err := a.registerUploadPath(m, p.Path)
	if err != nil {
		writeErr(w, uploadErrCode(err), err.Error())
		return
	}
	// 注记不独立落库：随响应交回，由前端拼进用户消息的「用户附件：」块
	writeJSON(w, 200, map[string]any{"ok": 1, "path": filepath.ToSlash(u.CopyPath), "orig": filepath.ToSlash(u.OrigPath), "uid": u.UID, "size": u.Size, "note": note})
}

// handleOpenFolder 在系统资源管理器中打开一个目录（工作区行的快捷入口）。
func (a *App) handleOpenFolder(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	path := strings.TrimSpace(body.Path)
	if path == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	if info, err := os.Stat(path); err != nil || !info.IsDir() {
		writeErr(w, 400, "目录不存在: "+path)
		return
	}
	if err := exec.Command("explorer", path).Start(); err != nil {
		writeErr(w, 500, "打开失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handlePickFolder 弹出系统原生“选择文件夹”对话框（与浏览器目录选择器同款），
// 阻塞至用户关闭后返回所选目录绝对路径；用户取消时 path 为空串。
// 浏览器端拿不到绝对路径，而本服务运行在用户本机，由后端弹框是唯一可靠途径。
func (a *App) handlePickFolder(w http.ResponseWriter, r *http.Request) {
	start := strings.TrimSpace(r.URL.Query().Get("start"))
	if start == "" {
		start = a.Cfg.Workspace
	}
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		title = "选择工作目录" // 兼容旧调用方；同时也是找对话框窗口用的标题
	}
	path, err := pickFolderSerialized(start, title)
	if err != nil {
		writeErr(w, 500, "无法打开文件夹选择框: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"path": path})
}

// handlePickFile 同款系统原生对话框，但选择文件。默认过滤器限定 *.zip 与
// *.md（技能安装文件，设置页「添加技能」在用）；query 加 filter=all 时不设
// 过滤器（任意文件，聊天上传采集原件真实路径用），multi=1 允许多选。用户
// 取消时 paths 为空数组；返回体保留 path 字段（首个）兼容旧调用方。
func (a *App) handlePickFile(w http.ResponseWriter, r *http.Request) {
	start := strings.TrimSpace(r.URL.Query().Get("start"))
	if start == "" {
		start = a.Cfg.Workspace
	}
	title := strings.TrimSpace(r.URL.Query().Get("title"))
	if title == "" {
		title = "选择文件"
	}
	allFilter := r.URL.Query().Get("filter") == "all"
	multi := r.URL.Query().Get("multi") == "1" || r.URL.Query().Get("multi") == "true"
	log.Printf("[pick-file] 打开原生文件选择框（filter=all:%v multi:%v，阻塞至用户关闭）", allFilter, multi)
	paths, err := pickFileSerialized(start, title, allFilter, multi)
	if err != nil {
		log.Printf("[pick-file] 失败: %v", err)
		writeErr(w, 500, "无法打开文件选择框: "+err.Error())
		return
	}
	log.Printf("[pick-file] 返回 %d 个路径", len(paths))
	first := ""
	if len(paths) > 0 {
		first = paths[0]
	}
	writeJSON(w, 200, map[string]any{"path": first, "paths": paths})
}

// handleInterject 对话中途发消息。
// mode=steer（插话）：排进插话队列；模型调用中（思考/正文输出）会立即打断
// 当前模型流、下一步带着插话重新调模型（kickModelStream），工具执行中则等
// 主循环下一次调用模型前注入生效；
// mode=queue（排队）：排进排队队列，本轮结束后作为独立一轮自动发送。
// 会话没在执行时一律按排队处理并顺手把执行拉起来，消息不会卡在队列里。
func (a *App) handleInterject(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Content   string `json:"content"`
		Mode      string `json:"mode"`
		VoiceUsed bool   `json:"voiceUsed"` // 本条消息含语音听写转写文字：置位会话粘性标记
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
		writeErr(w, 400, "content 不能为空")
		return
	}
	// 语音听写粘性标记：用过一次即置位且永不撤下（环境快照据此注入同音错字提示）
	if body.VoiceUsed && !m.VoiceUsed {
		m.VoiceUsed = true
		_ = a.Store.MarkVoiceUsed(m.ID)
	}
	mode := "queue"
	if body.Mode == "steer" && a.IsRunning(m.ID) {
		mode = "steer"
	}
	origin := ""
	if mode == "steer" {
		origin = "interject" // 插话来源标记：随消息落库，AI 自动标题等场景据此把插话与正常轮次区分开
	}
	pm := a.pushPending(m.ID, mode, strings.TrimSpace(body.Content), kindChat, origin)
	if !a.IsRunning(m.ID) && !a.queuePausedOf(m.ID) {
		a.kickQueue(m) // 本轮已经结束：排队消息自己开一轮，不依赖用户再点一次（暂停时只排队不拉起）
	}
	writeJSON(w, 200, map[string]any{"accepted": true, "id": pm.ID, "mode": mode})
}

// handlePending 处理排队坞上的操作：删除（remove）、提前为插话（steer）、
// 拖拽排序（move，before=锚点项 id，空=队尾）与暂停/恢复队列（pause/resume，
// 这两个是会话级开关，不需要 id）。恢复时若会话空闲且有排队消息，顺手拉起执行。
func (a *App) handlePending(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		ID     string `json:"id"`
		Action string `json:"action"`
		Before string `json:"before"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体格式错误")
		return
	}
	switch body.Action {
	case "pause":
		a.setQueuePaused(m.ID, true)
	case "resume":
		a.setQueuePaused(m.ID, false)
		if !a.IsRunning(m.ID) && len(a.pendingOf(m.ID).Queue) > 0 {
			a.kickQueue(m) // 空闲且有保留的排队消息：接着把它们发完
		}
	case "move":
		if body.ID == "" {
			writeErr(w, 400, "id 不能为空")
			return
		}
		if !a.movePending(m.ID, body.ID, body.Before) {
			writeErr(w, 404, "这条消息已经不在队列里了")
			return
		}
	default: // remove / steer
		if body.ID == "" {
			writeErr(w, 400, "id 不能为空")
			return
		}
		action := "remove"
		if body.Action == "steer" {
			action = "steer"
		}
		if !a.updatePending(m.ID, body.ID, action) {
			writeErr(w, 404, "这条消息已经不在队列里了")
			return
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "pending": a.pendingOf(m.ID)})
}

// kickQueue 会话空闲且有排队消息时，起一轮把它发出去（排队消息不依赖前端
// 定时器）：点「继续发送」、空闲时新到的消息、收口兜底续发都走这里。
func (a *App) kickQueue(m *SessionMeta) {
	run, ok := a.beginRun(m.ID)
	if !ok {
		return
	}
	a.serveQueue(m, run)
}

// serveQueue 在后台把排队消息发起来（无请求订阅者时也要跑）。被派发的会话
// （有派发方）走同一条派发链：跑完照旧回报派发方——不能因为「这一轮是队列
// 拉起的」就少了【派发回报】。
func (a *App) serveQueue(m *SessionMeta, run *sessionRun) {
	if owner := a.dispatchOwner(m); owner != nil {
		go a.runSubAgent(owner, m, run)
		return
	}
	go a.executeRun(m, run, nil)
}

// handleFork 分支对话：复制出一个新会话。body.upTo 指定分支点时只复制到
// 该 seq（含）为止——消息级分支按钮用；未指定时复制全部，目标正在执行则只
// 复制到最近一轮完成的位置（最后一条带正文且无工具调用的助手消息）。
func (a *App) handleFork(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		UpTo int64 `json:"upTo"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body) // 允许空请求体（整会话分支）
	all, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	msgs := []*StoredMessage{}
	for _, x := range all {
		if x.Kind != "summary" && x.Kind != "request" && x.Kind != "audit" && (body.UpTo <= 0 || x.Seq <= body.UpTo) {
			msgs = append(msgs, x)
		}
	}
	if len(msgs) == 0 {
		writeErr(w, 400, "分支点之前没有可复制的历史")
		return
	}
	// 落库即完成：库里有的都是已完成的小步，整份复制即可（不判断"整轮"）。
	// 唯一不能带走的是尾部未闭合的工具调用组（模型输出完就落库、工具结果还没回来
	// 的那一组，对模型请求不合法）——与子对话继承同一个函数，两个分支入口都过一遍。
	msgs = trimOpenToolGroup(msgs)
	// 源会话若带继承标记行，分支复制后条目数可能被上面的剥尾巴改小，
	// 重写标记里的计数——子对话前端靠它切分继承段，数字不对会错位
	if len(msgs) > 0 && msgs[0].Kind == inheritKind {
		var im inheritMark
		if json.Unmarshal([]byte(msgs[0].Text()), &im) == nil {
			im.Count = len(msgs) - 1
			if b, err := json.Marshal(im); err == nil {
				msgs[0].Message = TextMessage("user", string(b))
			}
		}
	}
	fork := &SessionMeta{
		ID: newID(), Title: m.Title + "（分支）", Mode: m.Mode, Workspace: m.Workspace,
		ParentID: m.ParentID, Provider: m.Provider, Model: m.Model, Permission: m.Permission,
		PlanMode: m.PlanMode, GoalMode: m.GoalMode, Tools: m.Tools,
		CreatedAt: now(), UpdatedAt: now(),
	}
	if permModeOf(fork) == permFull {
		fork.Tools = bundleFullAccessCaps(fork.Tools) // 分支照抄父会话；完全访问照捆绑点亮
	} else {
		// 分支也是"新建的会话"：按默认策略补回默认能力（与创建路径同一函数）
		fork.Tools = mergeCapDefaults(fork.Tools, fork.Mode, permModeOf(fork))
	}
	if err := a.Store.Create(fork); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	seqMap := map[int64]int64{} // 源 seq → 新会话 seq（AppendRow 顺序分配），供快照/交付按图复制
	for _, x := range msgs {
		// Compact/Thinking/Origin/Superseded 一并照搬：分支必须继承源会话的
		// 规则压缩视图（否则折叠状态丢失、上下文瞬间膨胀回去）与思考过程
		//（DeepSeek 思考模式的历史回传依赖它）
		row := &StoredMessage{Message: x.Message, Kind: x.Kind, Origin: x.Origin,
			Compact: x.Compact, Superseded: x.Superseded, Thinking: x.Thinking, CreatedAt: fork.CreatedAt}
		if err := a.Store.AppendRow(fork.ID, row); err == nil {
			seqMap[x.Seq] = row.Seq
		}
	}
	// 文件快照与交付登记跟着消息一起搬过去：分支里同样能回退文件、看交付
	if pairs, err := a.Store.CopySnapshots(m.ID, fork.ID, seqMap); err != nil {
		log.Printf("[快照] 分支 %s 复制快照失败（分支仍可用，只是缺文件历史）: %v", fork.ID, err)
	} else {
		a.copySnapshotFiles(m, fork, pairs)
	}
	// 上传登记同迁（uid 原样保留）：分支历史注记里的 u 编号继续可解析
	if err := a.Store.CopyUploadsOnBranch(m.ID, fork.ID); err != nil {
		log.Printf("[上传] 分支 %s 复制上传登记失败: %v", fork.ID, err)
	}
	(&toolContext{app: a, sess: fork}).ensureJxAI()
	a.broadcast(Event{Type: "session_updated", SessionID: fork.ID})
	writeJSON(w, 200, fork)
}

// handleEdit 编辑历史用户消息并重发：截断该消息之后的全部记录、改写正文、
// 立即续跑一轮。语义对齐 ZCode 的 editUserQuery——同会话内截断重发；文件
// 回退是可选档（workspaceMode=rewind，对齐其"对话+文件重置"提交按钮）：
// 先做安全预检（fail-closed：有外部修改过的文件就整体拒绝并回清单），
// 文件全部恢复成功才截断对话——文件与对话要么都退、要么都不退。
// 仅空闲会话可编辑；系统注入行（回报/上传/目标续跑/继承标记）不可编辑。
func (a *App) handleEdit(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Seq           int64  `json:"seq"`
		Content       string `json:"content"`
		WorkspaceMode string `json:"workspaceMode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Seq <= 0 || strings.TrimSpace(body.Content) == "" {
		writeErr(w, 400, "需要 seq 与非空 content")
		return
	}
	if body.WorkspaceMode != "preserve" && body.WorkspaceMode != "" && body.WorkspaceMode != "rewind" {
		writeErr(w, 400, "workspaceMode 只能是 preserve 或 rewind")
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeErr(w, 500, "当前连接不支持流式响应")
		return
	}
	lock := a.sessLock(m.ID)
	lock.Lock()
	if a.IsRunning(m.ID) {
		lock.Unlock()
		writeErr(w, 409, "该会话正在执行中，等本轮结束后再编辑")
		return
	}
	rows, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		lock.Unlock()
		writeErr(w, 500, err.Error())
		return
	}
	valid := false
	for _, x := range rows {
		if x.Seq == body.Seq {
			valid = x.Role == "user" && x.Kind == "chat"
			break
		}
	}
	if !valid {
		lock.Unlock()
		writeErr(w, 400, "只有真实的用户消息可以编辑")
		return
	}
	// 文件回退：预检不安全就整体拒绝（对话不裁剪），把清单回给前端选"仅重发对话"
	var restored []*rewindPlan
	if body.WorkspaceMode == "rewind" {
		tctx := &toolContext{app: a, sess: m}
		safe, unsafe, _, perr := a.planRewind(tctx, m.ID, body.Seq, nil, "")
		if perr != nil {
			lock.Unlock()
			writeErr(w, 500, "文件回退预检失败: "+perr.Error())
			return
		}
		if len(unsafe) > 0 {
			lock.Unlock()
			writeJSON(w, 409, map[string]any{
				"error":   "有文件在快照之后被外部修改，无法安全回退；可改为仅重发对话（不动文件）",
				"blocked": true, "unsafe": unsafe, "safe": safe,
			})
			return
		}
		restored = safe
	}
	content := strings.TrimSpace(body.Content)
	doomed, _ := a.Store.CheckpointsAfter(m.ID, body.Seq) // 截断将删掉的快照（拿 id 删正文文件）
	if _, err := a.Store.EditUserMessage(m.ID, body.Seq, content); err != nil {
		lock.Unlock()
		writeErr(w, 500, "编辑落库失败: "+err.Error())
		return
	}
	if len(doomed) > 0 {
		ids := make([]int64, 0, len(doomed))
		for _, cp := range doomed {
			ids = append(ids, cp.ID)
		}
		(&toolContext{app: a, sess: m}).deleteSnapshotFiles(ids)
	}
	if restored != nil {
		tctx := &toolContext{app: a, sess: m}
		if aerr := a.applyRewind(tctx, restored); aerr != nil {
			lock.Unlock()
			log.Printf("[快照] 会话 %s 编辑回退文件失败（已尝试补偿）: %v", m.ID, aerr)
			writeErr(w, 500, "文件回退失败（已自动恢复原状）: "+aerr.Error())
			return
		}
	}
	run, ok := a.beginRun(m.ID)
	if !ok {
		lock.Unlock()
		writeErr(w, 409, "该会话正在执行中，等本轮结束后再编辑")
		return
	}
	lock.Unlock()
	// opening 先发 edited 确认帧：前端据此把本地记录截断对齐，再跟随续跑输出
	opening := []Event{{Type: "edited", Seq: body.Seq, Delta: content}}
	a.serveRun(w, r, m, run, opening)
}

// handleFileRewind 撤销文件改动：不裁剪对话，只把范围内的文件改动恢复到
// 该范围之前（对齐 ZCode 文件摘要面板上的「撤销」）。三种取数口径：
//   - seqs：按轮撤销（文件改动面板的「全部撤回」）；
//   - path：只撤销这一个文件（折叠行内每行文件的「撤回」），仍限定在 seqs 内；
//   - checkpointId：只恢复到某一条快照之前（历史版本列表里的「恢复到此版本前」）。
//
// 同样 fail-closed：有不安全文件就整体拒绝；成功后落一条可见交代，并只清掉
// 真正用掉的这批快照（其余文件的快照继续留在库里，仍能再次撤回）。
func (a *App) handleFileRewind(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Seqs         []int64 `json:"seqs"`
		Path         string  `json:"path"`
		CheckpointID int64   `json:"checkpointId"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil ||
		(len(body.Seqs) == 0 && body.CheckpointID == 0) {
		writeErr(w, 400, "需要非空 seqs 或 checkpointId")
		return
	}
	if a.IsRunning(m.ID) {
		writeErr(w, 409, "该会话正在执行中，等本轮结束后再撤销")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	path := strings.TrimSpace(body.Path)
	var cps []*Checkpoint
	var err error
	if body.CheckpointID != 0 {
		// 单条快照（历史版本）：只回退该文件到这次改动之前，不看消息序号
		cps, err = a.Store.CheckpointsByIDs(m.ID, []int64{body.CheckpointID})
		if err == nil && len(cps) == 0 {
			writeErr(w, 404, "快照不存在（可能已被清理）")
			return
		}
		if path == "" && len(cps) > 0 {
			path = cps[0].Path
		}
	} else {
		cps, err = a.Store.CheckpointsIn(m.ID, body.Seqs)
		if err == nil && path != "" {
			kept := cps[:0:0]
			for _, cp := range cps {
				if cp.Path == path {
					kept = append(kept, cp)
				}
			}
			cps = kept
		}
	}
	if err != nil {
		writeErr(w, 500, "取快照失败: "+err.Error())
		return
	}
	safe, unsafe, consumed := a.planRewindFor(tctx, cps)
	if len(unsafe) > 0 {
		writeJSON(w, 409, map[string]any{
			"error":   "有文件在快照之后被外部修改，无法安全回退",
			"blocked": true, "unsafe": unsafe, "safe": safe,
		})
		return
	}
	if len(safe) == 0 {
		// 没有可执行项（如"撤回新建"但文件已不在）：目标状态已达成，照样清快照
		if len(consumed) > 0 {
			_ = a.Store.DeleteCheckpoints(m.ID, consumed)
			tctx.deleteSnapshotFiles(consumed)
			a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
		}
		writeJSON(w, 200, map[string]any{"restored": 0, "deleted": 0})
		return
	}
	if err := a.applyRewind(tctx, safe); err != nil {
		log.Printf("[快照] 会话 %s 撤销文件改动失败（已尝试补偿）: %v", m.ID, err)
		writeErr(w, 500, "文件回退失败（已自动恢复原状）: "+err.Error())
		return
	}
	// 这批快照已完成使命：状态已还原，留着会让下次回退拿到错误基线
	if len(consumed) > 0 {
		_ = a.Store.DeleteCheckpoints(m.ID, consumed)
		tctx.deleteSnapshotFiles(consumed)
	}
	names := []string{}
	restored, deleted := 0, 0
	for _, p := range safe {
		if p.Action == "delete" {
			deleted++
		} else {
			restored++
		}
		names = append(names, p.Path)
	}
	note := fmt.Sprintf("【文件回退】用户撤销了文件改动：恢复 %d 个文件、撤回 %d 个新建文件（%s）。",
		restored, deleted, strings.Join(names, "、"))
	_ = a.Store.AppendRow(m.ID, &StoredMessage{Message: TextMessage("user", note), Kind: kindReport, Origin: "rollback", CreatedAt: now()})
	m.UpdatedAt = now()
	_ = a.Store.SaveMeta(m)
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	writeJSON(w, 200, map[string]any{"restored": restored, "deleted": deleted, "paths": names})
}

// handleFileVersions 列出某文件的历史版本（快照）清单——文件改动面板每行
// 的「历史版本」按钮取数。path 用工作区相对路径（与快照同一口径）。同时回
// 当前文件是否存在与体积，供前端标注"当前"与"已删除"。
func (a *App) handleFileVersions(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	if path == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	vers, err := a.fileVersionList(tctx, path)
	if err != nil {
		writeErr(w, 500, "取历史版本失败: "+err.Error())
		return
	}
	payload := map[string]any{"path": path, "versions": vers, "exists": false, "size": 0}
	if data, exists, _ := tctx.workspaceFileBody(path); exists {
		payload["exists"] = true
		payload["size"] = len(data)
	}
	writeJSON(w, 200, payload)
}

// handleFileVersion 取一个历史版本的正文，并附带文件当前内容（供前端就地
// 对比）。版本正文缺失（.jxai/snapshots 正文文件被清掉）时 hasBody=false，
// 前端只展示元数据、不给"恢复"入口。
func (a *App) handleFileVersion(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	id := int64(0)
	if s := strings.TrimSpace(r.URL.Query().Get("id")); s != "" {
		if _, err := fmt.Sscanf(s, "%d", &id); err != nil || id == 0 {
			writeErr(w, 400, "id 不是合法快照 id")
			return
		}
	}
	if path == "" || id == 0 {
		writeErr(w, 400, "需要 path 与 id")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	cps, err := a.Store.CheckpointsByIDs(m.ID, []int64{id})
	if err != nil {
		writeErr(w, 500, "取快照失败: "+err.Error())
		return
	}
	if len(cps) == 0 || cps[0].Path != path {
		writeErr(w, 404, "该文件的这条历史版本不存在（可能已被清理）")
		return
	}
	cp := cps[0]
	data, truncated, ok := tctx.fileVersionBody(cp)
	payload := map[string]any{
		"path": cp.Path, "id": cp.ID, "seq": cp.Seq, "toolName": cp.ToolName,
		"existedBefore": cp.ExistedBefore, "afterSize": cp.AfterSize,
		"createdAt": cp.CreatedAt, "manual": cp.ToolName == manualSnapshotTool,
		"hasBody": ok,
	}
	if ok {
		appendVersionBody(payload, data, truncated)
	}
	cur, exists, curTrunc := tctx.workspaceFileBody(path)
	curPayload := map[string]any{"exists": exists}
	if exists {
		appendVersionBody(curPayload, cur, curTrunc)
	}
	payload["current"] = curPayload
	writeJSON(w, 200, payload)
}

// handleDeliveryOpenFile 用系统默认程序打开一个交付文件（交付卡片整行点击/
// 「打开」按钮）。与 open-path 同口径：用户手势链路不做边界拦截（无交付
// 登记、无工作区校验——这些路由不在 call_api 目录里，AI 调不到），仅保留
// 「路径不存在」这类自然错误。
func (a *App) handleDeliveryOpenFile(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	want := strings.TrimSpace(body.Path)
	if want == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	abs, err := tctx.resolve(want)
	if err != nil {
		writeErr(w, 400, "路径无法解析: "+want)
		return
	}
	if isDir, _, serr := tctx.envStat(abs); serr != nil || isDir {
		writeErr(w, 404, "交付文件不存在: "+abs)
		return
	}
	if m.Remote != nil {
		writeErr(w, 400, "这是 SSH 远程工作区里的文件，本机没有对应路径，无法用系统程序打开（html 网页文件仍可在右侧浏览器中打开）")
		return
	}
	if err := openPathWithDefaultApp(abs); err != nil {
		writeErr(w, 500, "打开失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleDeliveryReveal 在系统文件资源管理器里定位一个交付文件（交付卡片的
// 「在文件夹中查看」）。与 open 同口径：用户手势直通，无登记/工作区校验；
// 远程会话没有本机路径，明确拒绝。
func (a *App) handleDeliveryReveal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	want := strings.TrimSpace(body.Path)
	if want == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	if m.Remote != nil {
		writeErr(w, 400, "这是 SSH 远程工作区里的文件，本机没有对应路径，无法在资源管理器中定位")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	abs, err := tctx.resolve(want)
	if err != nil {
		writeErr(w, 400, "路径无法解析: "+want)
		return
	}
	if isDir, _, serr := tctx.envStat(abs); serr != nil || isDir {
		writeErr(w, 404, "交付文件不存在: "+abs)
		return
	}
	if err := revealInExplorer(abs); err != nil {
		writeErr(w, 500, "打开所在文件夹失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleOpenURL 用系统默认浏览器打开一个 http(s) 链接（桌面壳右键菜单的
// 「在系统浏览器中打开」）。只放行 http/https——file://、javascript: 等
// 一律拒绝；rundll32 FileProtocolHandler 对 URL 的处理即交给默认浏览器
// （非 Windows 平台 xdg-open/open 同理）。用户主动操作，不经模型权限门控。
func (a *App) handleOpenURL(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	raw := strings.TrimSpace(body.URL)
	if raw == "" {
		writeErr(w, 400, "需要 url")
		return
	}
	if len(raw) > 2048 {
		writeErr(w, 400, "url 过长")
		return
	}
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		writeErr(w, 400, "仅支持打开 http/https 链接")
		return
	}
	if err := openPathWithDefaultApp(raw); err != nil {
		writeErr(w, 500, "打开失败: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleSessionOpenPath 用系统默认程序打开一个文件或目录（右键菜单「在系统
// 中打开」与聊天文件链接的左键点击：文件走默认程序，目录等于在资源管理器里
// 打开）。用户手势链路不做任何边界拦截——调用方只有前端用户操作（call_api
// 目录闸挡着 AI，见 callapi.go），对用户自己的点击做工作区校验/确认弹窗
// 违背设计意图；仅保留「路径不存在」这类自然错误。
func (a *App) handleSessionOpenPath(w http.ResponseWriter, r *http.Request) {
	a.handleSessionOpenOrReveal(w, r, true)
}

// handleSessionRevealPath 在资源管理器里定位并选中一个文件或目录（右键菜单
// 「在文件夹中定位」）。与 open-path 同口径：用户手势直通，无边界拦截。
func (a *App) handleSessionRevealPath(w http.ResponseWriter, r *http.Request) {
	a.handleSessionOpenOrReveal(w, r, false)
}

func (a *App) handleSessionOpenOrReveal(w http.ResponseWriter, r *http.Request, open bool) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	want := strings.TrimSpace(body.Path)
	if want == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	if m.Remote != nil {
		writeErr(w, 400, "这是 SSH 远程工作区里的路径，本机没有对应文件，无法用系统程序打开")
		return
	}
	tctx := &toolContext{app: a, sess: m}
	abs, err := tctx.resolve(want)
	if err != nil {
		writeErr(w, 400, "路径无法解析: "+want)
		return
	}
	// 拒绝一律走 writeErr 且文案带完整解析路径（不裸 404）：文件真不存在时
	// 前端能弹出明确提示，用户也能照着路径手动前往
	if _, _, serr := tctx.envStat(abs); serr != nil {
		writeErr(w, 404, "路径不存在: "+abs)
		return
	}
	var opErr error
	if open {
		opErr = openPathWithDefaultApp(abs)
	} else {
		opErr = revealInExplorer(abs)
	}
	if opErr != nil {
		writeErr(w, 500, "打开失败: "+opErr.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleClipboardRead 读系统剪贴板（桌面壳右键「粘贴」的兜底：WebView2 里
// navigator.clipboard.readText 会永久挂起，权限态停在 prompt 且没有授权 UI，
// 见 clipboard_windows.go 头注）。返回 {text} 与可选 {png}（截图，base64）。
// 剪贴板属敏感数据：仅回环可达（loopbackOnly，与设备桥同闸），LAN/远程一律 403。
func (a *App) handleClipboardRead(w http.ResponseWriter, r *http.Request) {
	if !loopbackOnly(w, r) {
		return
	}
	text, png, err := readClipboardWindows()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	resp := map[string]string{"text": text}
	if len(png) > 0 {
		resp["png"] = base64.StdEncoding.EncodeToString(png)
	}
	writeJSON(w, 200, resp)
}

// handleDiag 前端诊断打点：页面探针（web/app-chat.js 的 jxDiag）把关键分支的
// 执行结果经 1px GIF beacon 送到这里落一行日志。桌面壳 WebView2 封了远程调试
// 端口、也没有 console 可看，「点了没反应 / 请求没发出去」这类前端问题只能靠
// 它取证。只写日志不回数据（GIF 空体即可），消息截断防刷屏；走 withCORS 的
// 同源校验与访问日志，不留额外攻击面。
func (a *App) handleDiag(w http.ResponseWriter, r *http.Request) {
	m := r.URL.Query().Get("m")
	if len(m) > 500 {
		m = m[:500] + "…"
	}
	log.Printf("[diag] %s", m)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "image/gif")
	// 1x1 透明 GIF：Image beacon 只需要一个能解码的图
	_, _ = w.Write([]byte("GIF89a\x01\x00\x01\x00\x80\x00\x00\x00\x00\x00\xff\xff\xff!\xf9\x04\x01\x00\x00\x00\x00,\x00\x00\x00\x00\x01\x00\x01\x00\x00\x02\x02D\x01\x00;"))
}

// handleDeliveryDownload 提供交付文件的下载。只放行已登记进 deliveries 的
// 路径（用户主动下载，不走模型权限审批；未登记的任意路径一律 404，防越权读）。
func (a *App) handleDeliveryDownload(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	want := strings.TrimSpace(r.URL.Query().Get("path"))
	if want == "" {
		writeErr(w, 400, "需要 path")
		return
	}
	dvs, err := a.Store.DeliveriesFor(m.ID)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	listed := false
	for _, d := range dvs {
		if d.Path == want {
			listed = true
			break
		}
	}
	if !listed {
		http.NotFound(w, r)
		return
	}
	tctx := &toolContext{app: a, sess: m}
	abs, err := tctx.resolve(want)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	data, err := tctx.envReadFile(abs)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	name := path.Base(filepath.ToSlash(want))
	// Content-Length 显式报准：本地/局域网浏览器的原生下载进度条、以及远程
	// 隧道 head 帧透传的总大小（remoteaccess.go bridgeCore）都取自这里。
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(data)
}

// handleListApprovals 返回待决定的审批（前端轮询兜底）。
func (a *App) handleListApprovals(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"approvals": a.pendingApprovals()})
}

// handleResolveApproval 接收用户对越权请求的决定。
func (a *App) handleResolveApproval(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Approved bool `json:"approved"`
		Always   bool `json:"always"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	// 经理代理（mgrnotify.go）：经理自己经 call_api 代批的请求带调用者会话头，
	// 先标注来源再登记决定——等待方醒来读到的就是已标注的值。
	if managerResolveCaller(a, r) {
		a.markApprovalByManager(r.PathValue("id"))
	}
	if err := a.resolveApproval(r.PathValue("id"), body.Approved, body.Always); err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleListQuestions 返回待回答的提问/计划审批（前端轮询兜底）。
func (a *App) handleListQuestions(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{"questions": a.pendingQuestions()})
}

// handleResolveQuestion 接收用户对提问的回答（计划批准用 __approve__）；
// stop_timer=true 则是「停止倒计时」（悬停/触碰提问卡或点停止按钮）：该提问
// 转为无限等待，直到作答或本轮被停止。
func (a *App) handleResolveQuestion(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Answer    string `json:"answer"`
		StopTimer bool   `json:"stop_timer"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.StopTimer {
		a.holdQuestion(r.PathValue("id"))
		writeJSON(w, 200, map[string]string{"ok": "1"})
		return
	}
	// 经理代理（mgrnotify.go）：经理自己经 call_api 代答的请求带调用者会话头，
	// 先标注来源再登记决定（口径同审批）。
	if managerResolveCaller(a, r) {
		a.markQuestionByManager(r.PathValue("id"))
	}
	if err := a.resolveQuestion(r.PathValue("id"), body.Answer); err != nil {
		writeErr(w, 404, err.Error())
		return
	}
	writeJSON(w, 200, map[string]string{"ok": "1"})
}

// handleGetProgress 返回会话的进度清单（progress_write 工具的持久化结果）。
func (a *App) handleGetProgress(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	writeJSON(w, 200, map[string]any{
		"progress": progressListOf(a, m.ID),
		"running":  runningChildrenOf(a, m.ID), // 「运行中的程序」：本会话在跑的子进程（child.go）
	})
}

// handleContextStats 返回会话下一次请求的上下文占用分解与缓存命中率
// （前端输入区「上下文」按钮面板的数据源）。
func (a *App) handleContextStats(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	writeJSON(w, 200, a.contextStats(m))
}

// handleTestWorkspace 测试一个 SSH 远程工作区配置：拨号 → 远程命令 →
// SFTP → 检查目录，返回主机指纹（前端展示，保存时绑定到会话）。
func (a *App) handleTestWorkspace(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Remote *RemoteWorkspace `json:"remote"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Remote == nil {
		writeErr(w, 400, "需要 remote 字段")
		return
	}
	a.fillRemoteCreds(body.Remote) // 面板复挂路径：密码留在服务端登记库里
	fp, err := a.testRemoteWorkspace(body.Remote)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "hostKey": fp, "message": "连接成功，命令通道与 SFTP 可用，目录存在"})
}

// handleWorkspaceLs 连接远程主机列出一个目录（前端「选择远程工作目录」用）。
// 不依赖会话；连接按规格复用（连续导航不重复拨号）。path 为空时返回
// 远程用户主目录。目录不存在等业务失败以 ok:false + message 回传。
func (a *App) handleWorkspaceLs(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Remote *RemoteWorkspace `json:"remote"`
		Path   string           `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Remote == nil {
		writeErr(w, 400, "需要 remote 字段")
		return
	}
	a.fillRemoteCreds(body.Remote) // 面板复挂路径：密码留在服务端登记库里
	spec, err := body.Remote.normalize()
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	rc, err := a.getBrowseClient(spec)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "message": err.Error()})
		return
	}
	home := rc.homeDir()
	p := strings.TrimSpace(body.Path)
	if p == "" {
		p = home
	}
	p = path.Clean(p)
	type dirEntry struct {
		Name  string `json:"name"`
		IsDir bool   `json:"isDir"`
		Size  int64  `json:"size,omitempty"`
	}
	entries, err := rc.listDir(p)
	if err != nil {
		writeJSON(w, 200, map[string]any{"ok": false, "path": p, "message": err.Error()})
		return
	}
	out := make([]dirEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, dirEntry{Name: e.Name, IsDir: e.IsDir, Size: e.Size})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "path": p, "home": home, "entries": out})
}

// handleAttachment 提供附件库里的图片（read_image 工具的产出），
// 供前端工具卡片直接渲染原图。
func (a *App) handleAttachment(w http.ResponseWriter, r *http.Request) {
	rel := strings.TrimPrefix(r.URL.Path, "/attachments/")
	if rel == "" {
		http.NotFound(w, r)
		return
	}
	data, mediaType, err := a.loadAttachment(rel)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	_, _ = w.Write(data)
}

// handleSetSessionRemote 给会话设置 / 解除 SSH 远程工作区。
// body.remote 为空对象或省略 + clear=true 时解除（回到本地工作区）。
func (a *App) handleSetSessionRemote(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Remote *RemoteWorkspace `json:"remote"`
		Clear  bool             `json:"clear"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	if body.Clear || body.Remote == nil {
		m.Remote = nil
		a.closeRemoteSession(m.ID)
	} else {
		a.fillRemoteCreds(body.Remote) // 面板复挂路径：密码留在服务端登记库里
		spec, err := body.Remote.normalize()
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		m.Remote = spec
		a.closeRemoteSession(m.ID) // 换目标后旧连接立即回收
	}
	if err := a.Store.SaveMeta(m); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if m.Remote != nil {
		a.ctxEngine().Notify(m, ctxRemote, "挂载远程工作区："+m.Remote.Display())
	} else {
		a.ctxEngine().Notify(m, ctxRemote, "解除远程工作区，回到本地工作区")
	}
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	writeJSON(w, 200, m)
}

// handleEvents 是全局事件流（SSE）：子任务进度、回报就绪、会话更新。
// 前端常驻订阅，后台派发的进度从这里进（与聊天 SSE 相互独立）。
func (a *App) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "当前连接不支持流式响应")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	ch, unsub := a.subscribe()
	defer unsub()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	_, _ = w.Write([]byte(": connected\n\n"))
	flusher.Flush()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev := <-ch:
			data, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			_, _ = w.Write(append([]byte("data: "), append(data, '\n', '\n')...))
			flusher.Flush()
		}
	}
}

// handleChat 接收用户消息：落库 → 启动会话执行 → 以 SSE 跟随输出。
// 执行体独立于本请求（见 runtime.go 的 executeRun）：浏览器刷新/关页只断开
// 这条流，执行照旧跑完并落库；重新打开页面用 GET /api/sessions/{id}/stream 再接上。
func (a *App) handleChat(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	var body struct {
		Content   string `json:"content"`
		VoiceUsed bool   `json:"voiceUsed"` // 本条消息含语音听写转写文字（组件听写落框时打的标记）
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Content) == "" {
		writeErr(w, 400, "content 不能为空")
		return
	}
	// 语音听写粘性标记：会话内用过一次即置位且永不撤下——环境快照（agent.go）
	// 据此注入同音错字提示；内存对象同步置位，本轮组装快照时即已生效。
	if body.VoiceUsed && !m.VoiceUsed {
		m.VoiceUsed = true
		_ = a.Store.MarkVoiceUsed(m.ID)
	}
	// Claude Code 插件：斜杠命令展开（插件命令正文或 mod 命令执行；未命中原样
	// 发送，不影响普通输入）
	if a.CCPlugins != nil {
		body.Content = a.ccExpandSlashMessage(r.Context(), m, body.Content)
	}
	// 声明式组件钩子（componentbridge.go）：prompt.submit 拦截型——本栈组件
	// 优先于 CC 兼容层。任一组件可拒绝（403+reason）或改写正文；附加上下文
	// 在用户消息后落系统注入行（origin=component-hook），先于 CC hook 注入
	var compExtraContext string
	if oc := a.emitComponentHooks("prompt.submit", map[string]any{"sessionID": m.ID, "content": body.Content}); oc.Block {
		writeErr(w, 403, "消息被组件钩子拒绝："+oc.Reason)
		return
	} else {
		if oc.Content != "" {
			body.Content = oc.Content
		}
		compExtraContext = oc.AdditionalContext
	}
	// Claude Code 插件：UserPromptSubmit hook（可拒绝消息或附加上下文）
	var ccExtraContext string
	if a.CCPlugins != nil && a.CCPlugins.hasAnyHooks() {
		outcome := a.ccEmitHookEvent(r.Context(), m, "UserPromptSubmit", "", nil,
			map[string]any{"prompt": body.Content})
		if outcome.Block {
			writeErr(w, 403, "消息被插件 hook 拒绝："+outcome.Reason)
			return
		}
		if outcome.SystemMsg != "" {
			a.broadcast(Event{Type: "report", SessionID: m.ID, Output: outcome.SystemMsg})
		}
		ccExtraContext = outcome.ExtraContext
	}
	// Claude Code 插件 mods：prompt.submit（可改写文本或丢弃）
	if a.CCPlugins != nil && a.CCPlugins.hasMods() {
		oc := a.ccEmitModEvent(r.Context(), m, "prompt.submit", map[string]any{"text": body.Content})
		if oc.Answer != nil {
			if reason, ok := oc.Answer["drop"].(string); ok {
				writeErr(w, 403, "消息被 mod 丢弃："+reason)
				return
			}
		}
		if t, ok := oc.Rewrite["text"].(string); ok && strings.TrimSpace(t) != "" {
			body.Content = t
		}
	}
	if _, ok := w.(http.Flusher); !ok {
		writeErr(w, 500, "当前连接不支持流式响应")
		return
	}
	// 落库用户消息与登记执行放在同一临界区：并发重复发送只会有一条成功
	lock := a.sessLock(m.ID)
	lock.Lock()
	if a.IsRunning(m.ID) {
		lock.Unlock()
		writeErr(w, 409, "该会话正在执行中，请稍候再发")
		return
	}
	run, ok := a.beginRun(m.ID)
	if !ok {
		lock.Unlock()
		writeErr(w, 409, "该会话正在执行中，请稍候再发")
		return
	}
	um := TextMessage("user", body.Content)
	sm := a.appendMessage(m, um)
	opening := []Event{{Type: "message", Message: &sm.Message, Seq: sm.Seq, Kind: kindChat}}
	// 组件钩子的 additionalContext：用户消息后落一条系统注入行（先于 CC hook）
	if compExtraContext != "" {
		rm := a.landUserSide(m, compExtraContext, kindReport, "component-hook", func(ev Event) {
			opening = append(opening, ev)
		})
		_ = rm
	}
	// UserPromptSubmit 的 additionalContext：用户消息后落一条系统注入行（发给模型）
	if ccExtraContext != "" {
		rm := a.landUserSide(m, ccExtraContext, kindReport, "plugin-hook", func(ev Event) {
			opening = append(opening, ev)
		})
		_ = rm
	}
	if m.Title == "" {
		m.Title = truncateRunes(body.Content, 24)
		_ = a.Store.SaveMeta(m)
		opening = append(opening, Event{Type: "title", SessionID: m.ID, Title: m.Title})
	}
	lock.Unlock()
	a.serveRun(w, r, m, run, opening)
}

// handleSessionStream 接上正在执行的会话流（刷新页面/换窗口后继续看实时输出）。
// 未在执行时立刻回一帧 done：前端据此收尾，不会一直挂着。
func (a *App) handleSessionStream(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeErr(w, 500, "当前连接不支持流式响应")
		return
	}
	run := a.runOf(m.ID)
	if run == nil {
		sseHeaders(w)
		_, _ = w.Write([]byte("data: {\"type\":\"done\"}\n\n"))
		w.(http.Flusher).Flush()
		return
	}
	a.serveRun(w, r, m, run, nil)
}

// handleStop 显式停止会话执行（SSE 断开不再等于停止）。
func (a *App) handleStop(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	if !a.stopRun(m.ID) {
		writeJSON(w, 200, map[string]any{"stopped": false, "reason": "该会话当前没有在执行"})
		return
	}
	writeJSON(w, 200, map[string]any{"stopped": true})
}

// handleRetry 失败后重试：不重复落库用户消息，直接带着当前上下文再跑一轮
// （上一轮报错时用户消息已经在库里，重发只会多出一条重复的用户消息）。
func (a *App) handleRetry(w http.ResponseWriter, r *http.Request) {
	m := a.sessionFromPath(w, r)
	if m == nil {
		return
	}
	lock := a.sessLock(m.ID)
	lock.Lock()
	if a.IsRunning(m.ID) {
		lock.Unlock()
		writeJSON(w, 200, map[string]any{"started": false, "reason": "该会话正在执行中"})
		return
	}
	run, ok := a.beginRun(m.ID)
	lock.Unlock()
	if !ok {
		writeJSON(w, 200, map[string]any{"started": false, "reason": "该会话正在执行中"})
		return
	}
	a.clearRunError(m.ID)
	a.serveRun(w, r, m, run, []Event{{Type: "retry"}})
}

// sseHeaders 切换响应为 SSE。
func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
}

// serveRun 把执行事件以 SSE 写给客户端：先补当前进行中的流式文本（重连不丢半句），
// 再持续转发。函数因客户端断开返回时执行不受影响；opening 为空表示纯重连。
// opening 事件由执行体统一发布（这里只订阅），否则同一条用户消息会发两遍。
func (a *App) serveRun(w http.ResponseWriter, r *http.Request, m *SessionMeta, run *sessionRun, opening []Event) {
	sseHeaders(w)
	flusher := w.(http.Flusher)
	ch, unsub := run.subscribe()
	defer unsub()
	if len(opening) > 0 {
		go a.executeRun(m, run, opening) // 启动执行；事件走订阅通道下发
	} else if text, thinking := run.snapshot(); text != "" || thinking != "" {
		// 纯重连：补上正在进行中的那半句，避免接上时丢内容
		if b, err := json.Marshal(Event{Type: "resume", SessionID: m.ID, Delta: text, Thinking: thinking}); err == nil {
			_, _ = w.Write(append([]byte("data: "), append(b, '\n', '\n')...))
		}
	}
	flusher.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return // 客户端断开（刷新/关页/主动取消）：执行继续，可随时再接上
		case <-ping.C:
			_, _ = w.Write([]byte(": ping\n\n"))
			flusher.Flush()
		case ev, ok := <-ch:
			if !ok {
				return // 执行结束，订阅通道关闭
			}
			b, err := json.Marshal(ev)
			if err != nil {
				continue
			}
			if _, err := w.Write(append([]byte("data: "), append(b, '\n', '\n')...)); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
