package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/big"
	"net/url"
	"os"
	"strings"
)

// genDeviceID 生成远程接入设备号：jx- + 6 位易抄写的字符（去掉 0/o/1/i
// 这类易混字符）。手机入口路径 /a/<设备号> 用它，网关上全网唯一。
func genDeviceID() string {
	const alphabet = "abcdefghjkmnpqrstuvwxyz23456789"
	out := make([]byte, 6)
	for i := range out {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(alphabet))))
		out[i] = alphabet[n.Int64()]
	}
	return "jx-" + string(out)
}

// genSecret 生成 n 个十六进制字符的随机密钥（配对密钥等）。
func genSecret(n int) string {
	b := make([]byte, (n+1)/2)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)[:n]
}

// ensureRemoteIdentity 远程接入的身份补全：设备号/配对密钥为空时随机生成，
// 并立即回写配置——不落盘的话每次启动都会换身份，手机存的入口就失效了。
// defaultRemoteHost 出厂默认的中继网关域名：服务端下发 relayHost 时，只有当前值仍等于
// 它才自动跟随（用户手改过就尊重用户的设置）。
const defaultRemoteHost = "example.com"

func ensureRemoteIdentity(cfg *Config, path string) {
	generated := false
	if cfg.RemoteAccess.MyID == "" {
		cfg.RemoteAccess.MyID = genDeviceID()
		generated = true
	}
	if cfg.RemoteAccess.PairKey == "" {
		cfg.RemoteAccess.PairKey = genSecret(32)
		generated = true
	}
	if generated {
		if err := cfg.Save(path); err != nil {
			log.Printf("远程接入设备号/密钥生成后回写失败: %v", err)
		}
	}
}

// Config 为全局运行配置，落盘为 JSON，可在前端设置页在线修改。
// BaseURL/APIKey/Model 是单模型时代的旧字段：为兼容老配置保留，
// 加载时若 Providers 为空会折算成第一个 provider，保存时反向同步。
type Config struct {
	Port     int    `json:"port"`
	BindHost string `json:"bindHost"`
	BaseURL  string `json:"baseURL"`
	APIKey   string `json:"apiKey"`
	Model    string `json:"model"`
	// DefaultModel 全局默认模型（完整格式：渠道 + 模型 + Key 归属）。手动新建任务
	// 会话（首页输入框默认选中的模型）用它；待办在派发设置为「跟随主会话」时也落到
	// 它（待办没有主会话可跟）。老配置里那个裸模型名（Model）是单模型时代的遗留，
	// 加载时折算到这里（见 defaultmodels.go 的 migrateDefaultModel），之后不再参与解析。
	DefaultModel ModelRef `json:"defaultModel"`
	// DispatchModel 派发任务默认模型：空＝跟随主会话（派发者当前的渠道/模型/Key，
	// 也就是这个设置出现之前的现状）；非空＝dispatch_task 派出的顶层任务会话用它。
	DispatchModel ModelRef      `json:"dispatchModel,omitempty"`
	Providers     []ProviderCfg `json:"providers"`
	Workspace     string        `json:"workspace"`
	MaxSteps      int           `json:"maxSteps"` // 步数上限（一条消息触发的任务里连续步数）：0/负数 = 不限制（默认）
	AllowShell    bool          `json:"allowShell"`
	DataDir       string        `json:"dataDir"`
	SkillsDir     string        `json:"skillsDir"` // 技能目录；空 = 可执行文件旁的 技能/（不可写时落数据目录）
	// 上下文压缩设置。规则自动压缩为**间隔制**：自上次压缩边界起新增未压缩
	// 内容达到 CompactInterval（随 CompactUnit 取字数或 token）就触发一次零
	// 成本规则折叠；模型摘要阈值（CompactLlmPct，默认 80%）仍按窗口百分比，
	// 作为更晚的兜底（先折后摘）。CompactMode 只决定手动「压缩」按钮的默认
	// 行为，不参与自动选路；CompactInterval=0 表示关闭规则自动压缩。
	CompactMode      string  `json:"compactMode"`      // 手动压缩按钮的默认行为：llm=模型摘要(默认) rule=规则折叠；旧值 off 读入时归一为当前默认
	KeepChars        int     `json:"keepChars"`        // 自动压缩的保留预算（字）：从最新往前累计模型可见字数，预算耗尽处（命中点）所在的轮整轮保留——轮首可以是用户消息，也可以是数字助理转发消息；命中轮自身超预算（巨轮）时回退行级窗口，至少保最近一条原文
	CompactInterval  int     `json:"compactInterval"`  // 规则自动压缩间隔：自上次压缩边界起新增未压缩内容达到该值触发一次规则折叠（0=关闭）。单位随 CompactUnit，默认 400000 字
	CompactUnit      string  `json:"compactUnit"`      // 间隔计量单位：chars=字数（默认，模型无关）| tokens=token（用模型返回的 usage 真值，不估算）
	CompactThreshold float64 `json:"compactThreshold"` // 旧版单阈值（0.8/0.9）：新逻辑不读它，仅保留字段让旧版 exe 回滚时还能用
	CompactLlmPct    int     `json:"compactLlmPct"`    // 模型自动压缩阈值（%）：0=不启用（默认 80）——默认压缩方式，到线即摘要
	// 文件快照保留策略：按天超龄清理是主力（旧配置缺省回落默认值；-1 = 该项关闭）。
	// 自动池管 write/edit 的小快照；手动池（snapshot_file，往往是
	// Office 文档等大文件）默认不限量——清理靠 7 天过期；确有磁盘压力时再配
	// 500/1024 这一级别的大预算。SnapshotEnabled 是设置页「文件备份」总开关
	//（指针类型：配置文件里没写这一项 = 开启）。
	SnapshotEnabled     *bool `json:"snapshotEnabled,omitempty"`
	SnapshotDays        int   `json:"snapshotDays"`        // 快照保留天数，默认 7；-1=永不过期
	SnapshotMaxMB       int   `json:"snapshotMaxMB"`       // 每会话自动快照总量上限（MB），默认 32；-1=不限量
	SnapshotManualMaxMB int   `json:"snapshotManualMaxMB"` // 每会话手动快照总量上限（MB），默认 -1 不限量；需要时设 500/1024
	// 会话浏览器 profile 的保留天数（browser_cleanup.go 的按天回收）：每会话
	// 独立的浏览器配置目录（browser-profiles/-headed、webview-browsers）超过
	// 该天数未活跃即整体删除，下次使用时重建（登录态丢失，已确认可接受）。
	// 指针类型：没写过这一键 = 默认 3；显式 0 = 不清理；负数按 0 处理。
	BrowserProfileDays *int `json:"browserProfileDays,omitempty"`
	// 子对话专家（专家团）预设：dispatch_sub 按名称选用的命名预设（追加提示词/
	// 默认模型/上下文继承/启用开关）。配置文件没写过这一键时补种演示专家。
	Experts []ExpertCfg `json:"experts"`
	// 远程接入（公网中继，RustDesk 式）：出站长连注册到自建 210 网关的 /hub，
	// 手机等外部设备经网关的加密中继访问本服务；端到端 AES-256-GCM（密钥由
	// pairKey 派生），网关只见密文。与聊天服务的 p2p.js 同协议，共用同一网关。
	RemoteAccess RemoteAccessCfg `json:"remoteAccess"`
	// 企业技能包 / 工作流 / 小程序平台服务器地址（域名/IP + 端口，如 https://example.com:2227）：
	// 技能市场与小程序平台同一台 jxai-server，清单（/market/manifest.json）与 /sites
	// 等后缀由程序自动拼（market.go 的 marketManifestURL），已带后缀的旧值同样兼容；
	// 空 = 未配置（市场页提示并给出与中继网关同 host 的建议值）。
	MarketURL string `json:"marketURL"`
	// 联网搜索服务端地址（web_search 工具的上游网关，jxai-server 的 /api/search，
	// 见 websearch.go）：空 = 跟随市场地址同源——市场与搜索本就同一台服务端。
	SearchURL string `json:"searchURL,omitempty"`
	// 搜索供应商（websearch_providers.go）：公司网关 + 用户自配的第三方搜索/抽取
	// 服务，按 order 顺序回落、只有失败才换下一家。SearchURL 与它并存——公司网关
	// 只是 kind=gateway 的一条，地址仍在调用时从 SearchURL 解析（老配置零迁移）。
	// 加载时没有这一键的配置自动补齐默认四条（key 为空，由用户自己填）。
	SearchProviders []SearchProviderCfg `json:"searchProviders,omitempty"`
	// 一次 web_search 的总耗时预算（秒，5–120，默认 20）：是预算不是每家超时相加。
	SearchTimeoutSec int `json:"searchTimeoutSec,omitempty"`
	// 发布者姓名（市场条目的归属名 owner）：发布技能时随包上报，市场卡片拿它
	// 与条目 owner 比对——只显示自己那些条目的「更新发布 / 删除」（见
	// market_publish.go 的 unpublish）。默认由首启向导里填的姓名写入（wizard.go）；
	// 没跑过向导的机器在 设置 · 网络访问 手动填。空 = 未设置。
	PublisherName string `json:"publisherName"`
	// 公司接入（向导，wizard.go）：wizardDone=已完成/跳过首启向导（不再自动弹）；
	// registryURL=总服务器地址（公司名发现不到时的查询兜底，空=只走局域网广播）；
	// wizardCompany/wizardServer 留给自动同步器（sync.go）：定期拉服务端 /announce
	// 跟随变化（模型清单/元数据、公司名、市场/更新/服务端地址），失联时按公司名
	// 重新局域网发现自动迁移（服务端换端口/换机器）。
	// companyLinkOff=公司接入整体停用（设置·网络访问的开关）：永不在公司使用的
	// 机器关掉后不再连服务端，首启向导也不弹；开启的一切（含已配好的内部模型
	// 通道）原样保留，重新打开即恢复同步与向导。
	WizardDone  bool   `json:"wizardDone"`
	RegistryURL string `json:"registryURL"`
	// 小程序平台不再有独立地址（2026-11-03 用户口径）：一律从市场地址（marketURL）
	// 推导——内嵌走本端透明反向代理 /sites（sitesproxy.go），"在浏览器中打开"由
	// 前端按市场 origin 拼 /sites。服务端也不再随公告/配置包下发 sitesURL。
	WizardCompany      string `json:"wizardCompany,omitempty"`
	WizardServer       string `json:"wizardServer,omitempty"`
	WizardPublicServer string `json:"wizardPublicServer,omitempty"` // 服务端公网地址（仅由服务端公告/provision 下发，本地永不写死）：内网失联时改从它拉公告——公司通道配了外网地址就出公司也能用，否则自动停用
	WizardRev          string `json:"wizardRev,omitempty"`          // 最近应用的服务端公告哈希（仅记录）
	// CompanyQuota 公司内部通道的额度快照（服务端 /announce 与 /provision 下发，
	// 客户端只存不算；回显在设置页公司通道卡片与模型菜单）。nil = 服务端没下发
	// （老服务端或未配模型通道）——此时不显示额度。
	CompanyQuota   *CompanyQuotaCfg `json:"companyQuota,omitempty"`
	CompanyLinkOff bool             `json:"companyLinkOff,omitempty"` // 公司接入停用（设置·网络访问的开关）：给永不在公司、连不上服务端的机器——首启向导不弹（status 接口把它并进 wizardDone）、向导 discover/connect 拒绝、公司同步器（sync.go）停转；再开即恢复
	// companyResync=已完成「一次性重新接入」的标记版本（0=从未做过）。发新版时
	// 把 companyResyncTarget 提高，老客户端启动后自动重跑一次公司接入（按人
	// 重领 provision / 至少拉一次公告），把 10-16 之后没再跑过向导、模型清单
	// 还停在旧配置的机器追平；失败则弹一次带原有配置的向导（companyResyncPrompted
	// 记已弹过，避免每次启动都打扰）。
	CompanyResync         int  `json:"companyResync,omitempty"`
	CompanyResyncPrompted bool `json:"companyResyncPrompted,omitempty"`
	// 更新检查：定期 GET 该地址（默认 Gitea releases/latest API），响应里的
	// tag_name 比本程序版本新就经事件流给所有客户端推「新版本可用」。空串=关闭。
	UpdateURL string `json:"updateURL"`
	// 更新方式：auto（默认）=发布源支持时走分块静默更新（update_blocks.go）；
	// manual=只弹横幅提醒，由用户手动下载完整安装包更新。
	UpdateMode string `json:"updateMode,omitempty"`
	// 语音转写服务配置（演示记录器的讲解音频 → SRT 字幕，预留字段）：
	// provider（如 microsoft）/ endpoint / apiKey / language。当前版本尚未
	// 接入任何服务商（语音组件的朗读走 Edge 免密钥通道、听写走浏览器
	// SpeechRecognition，都不读这份配置）；接入后 recorder.go 的
	// activeSpeechTranscriber 读这里。
	SpeechAPI map[string]string `json:"speechApi,omitempty"`
	// 主题色（#rrggbb）：设置页换点缀色时同步写进来，独立 WebView 环境
	//（读不到主界面 localStorage）可从 /api/config 取它上色。
	AccentColor string `json:"accentColor,omitempty"`
	// 询问提醒（设置·常规）：提问/审批卡等待达到 AfterSec 仍未答（且用户未
	// 停表）时声光提醒——系统叮一声 + 全屏颜色闪烁 + 语音组件朗读 Text
	//（组件停用时只叮不读）。Enabled=nil 表示未写过该键＝默认开启。
	AskAlert AskAlertCfg `json:"askAlert,omitempty"`
	// 经理代理通知（mgrnotify.go）：经理触达过的会话（dispatch_task 派生 /
	// send_to_session 发过话）里，审批/提问卡挂出 AfterSec 秒（默认 30）仍
	// 无人处理时，自动给长期助理主对话投一条通知（含 call_api 代批路径），
	// 决议随后回告并标注来源。与上面的询问提醒并行，各管各：一个叫醒用户、
	// 一个叫醒经理。Enabled=nil 表示未写过该键＝默认开启。
	MgrNotify MgrNotifyCfg `json:"mgrNotify,omitempty"`
	// 「执行结束后自动续发排队消息」（设置·常规）：本轮回复完成后，排队队列里
	// 的待发消息——真人消息 / 派发回报 / 派发需求 / 系统注入，**不分来源**——
	// 自动逐条作为独立一轮发出。nil 表示未写过该键＝默认开启；显式 false 才关闭：
	// 关掉后本轮结束队列保留，排队坞给「继续发送」入口（点它照发）。
	QueueAutoSend *bool `json:"queueAutoSend,omitempty"`
	// 「关闭窗口驻留托盘」（设置·常规，仅桌面版）：✕ / Alt+F4 关窗时只隐藏窗口、
	// 进程与后端（会话/任务）驻留，双击托盘图标回到界面。nil 表示未写过该键＝
	// 默认开启；显式 false 才关闭：关窗即真正退出程序。网页/手机形态无此语义。
	CloseToTray *bool `json:"closeToTray,omitempty"`
}

// CompanyQuotaCfg 公司内部通道的额度快照（服务端 quota.go 的 quotaPayload 结构，
// 随 /announce 与 /provision 下发）。客户端只存不算：
//
//	Points       每次重置为多少积分（0 = 不限量，只记账）
//	RemainPoints 剩余（积分；快照时的值，最多滞后几分钟）
//	UsedPoints   已用（积分，new-api 按令牌记账）
//	Period/PeriodText  重置周期与给人看的描述（"每月 1 号"/"每 7 天"/"不自动重置"）
//	NextAt       下次重置时间（RFC3339，空 = 不自动重置）
//	AsOf         快照时间（显示"更新于 xx"，避免用户以为是实时值）
//	Status       1=启用 2=被中转站禁用（额度耗尽会被自动停用）
type CompanyQuotaCfg struct {
	Points       int64  `json:"points,omitempty"`
	RemainPoints int64  `json:"remainPoints,omitempty"`
	UsedPoints   int64  `json:"usedPoints,omitempty"`
	Period       string `json:"period,omitempty"`
	PeriodText   string `json:"periodText,omitempty"`
	NextAt       string `json:"nextAt,omitempty"`
	AsOf         string `json:"asOf,omitempty"`
	Unlimited    bool   `json:"unlimited,omitempty"`
	Status       int    `json:"status,omitempty"`
}

// AskAlertCfg 询问提醒配置段：Enabled=nil＝默认开启；AfterSec 触发秒数
// （默认 240=4 分钟，钳 30-600）；Text 朗读文案（空＝默认那句）。
type AskAlertCfg struct {
	Enabled  *bool  `json:"enabled,omitempty"`
	AfterSec int    `json:"afterSec,omitempty"`
	Text     string `json:"text,omitempty"`
}

// MgrNotifyCfg 经理代理通知配置段：Enabled=nil＝默认开启（false=关闭）；
// AfterSec 触发秒数（默认 30，钳 10-600，0 会在加载时归一回默认——想关用
// Enabled，不留「0=关闭」这第二个开关语义）。
type MgrNotifyCfg struct {
	Enabled  *bool `json:"enabled,omitempty"`
	AfterSec int   `json:"afterSec,omitempty"`
}

// 经理代理通知的默认触发秒数（用户定调 30 秒：比声光提醒早得多——经理先看
// 到多半就轮不到打扰用户）。
const mgrNotifyDefaultSec = 30

// mgrNotifyOn 经理代理通知是否开启（nil 接收器＝关：测试的裸 App 没有 Cfg）。
func (c *Config) mgrNotifyOn() bool {
	if c == nil {
		return false
	}
	return c.MgrNotify.Enabled == nil || *c.MgrNotify.Enabled
}

// mgrNotifyAfterSec 通知触发秒数（解析后的生效值，10-600，默认 30）。
func (c *Config) mgrNotifyAfterSec() int {
	v := mgrNotifyDefaultSec
	if c != nil && c.MgrNotify.AfterSec >= 10 && c.MgrNotify.AfterSec <= 600 {
		v = c.MgrNotify.AfterSec
	}
	return v
}

// 询问提醒的默认朗读文案与触发秒数（含访问器的兜底，测试里 Cfg 为 nil 也安全）。
const (
	askAlertDefaultSec  = 240
	askAlertDefaultText = "领导，在吗？有个问题需要您审批确认一下。"
)

// askAlertOn 询问提醒是否开启（nil 接收器＝关：测试的裸 App 没有 Cfg）。
func (c *Config) askAlertOn() bool {
	if c == nil {
		return false
	}
	return c.AskAlert.Enabled == nil || *c.AskAlert.Enabled
}

// askAlertAfterSec 触发秒数（解析后的生效值，30-600）。
func (c *Config) askAlertAfterSec() int {
	v := askAlertDefaultSec
	if c != nil && c.AskAlert.AfterSec >= 30 && c.AskAlert.AfterSec <= 600 {
		v = c.AskAlert.AfterSec
	}
	return v
}

// askAlertText 朗读文案（空回填默认）。
func (c *Config) askAlertText() string {
	if c != nil {
		if s := strings.TrimSpace(c.AskAlert.Text); s != "" {
			return s
		}
	}
	return askAlertDefaultText
}

// askAlertDurText 提醒秒数的人类读法（240 →「4 分钟」、90 →「90 秒」），
// envPrompt 的提问等待公告用。
func askAlertDurText(sec int) string {
	if sec%60 == 0 {
		return fmt.Sprintf("%d 分钟", sec/60)
	}
	return fmt.Sprintf("%d 秒", sec)
}

// queueAutoSendOn 执行结束后是否自动续发排队消息（nil 键＝默认开；nil 接收器
// ＝开：测试里的裸 App 没有 Cfg）。
func (c *Config) queueAutoSendOn() bool {
	if c == nil {
		return true
	}
	return c.QueueAutoSend == nil || *c.QueueAutoSend
}

// closeToTrayOn 关闭窗口时是否驻留托盘（nil 键＝默认开；nil 接收器＝开：
// 测试里的裸 App 没有 Cfg）。仅桌面形态消费；见 desktop_windows.go 关窗分支。
func (c *Config) closeToTrayOn() bool {
	if c == nil {
		return true
	}
	return c.CloseToTray == nil || *c.CloseToTray
}

// RemoteAccessCfg 的 Host/Port 默认指向已跑通的 210 网关；MyID 与聊天服务的
// ds-main 错开（网关按设备号路由，各应用互不干扰）；RegKey 是网关信令准入 key。
type RemoteAccessCfg struct {
	Enabled bool   `json:"enabled"`
	Host    string `json:"host"`
	Port    int    `json:"port"`
	MyID    string `json:"myId"`
	PairKey string `json:"pairKey"`
	RegKey  string `json:"regKey"` // legacy 全局口令：仅旧网关兼容用；准入实际凭据=PairKey 派生（hubAuthKey）
}

// 步数上限（一条消息触发的任务里，模型调用＋工具执行的连续步数）：0 或负数 =
// 不限制——任务跑到模型自己收手（不再发起工具调用）或用户点停止为止。
// 以前是「给个数字，跑满自动停」：30 → 100 → 200 一路往上抬，可复杂任务照样
// 撞墙（撞墙即前功尽弃，只能重试），抬数字永远猜不到够不够。所以默认改成
// 不限制：真正该决定何时停的是模型与用户，不是一个猜出来的整数。
// 想给任务兜个上限的，设置页里关掉「不限制」再填步数即可（手动值一律保留）。
const maxStepsDefault = 0

// maxStepsLegacyDefaults 是历次默认值：配置里的值正好等于其中之一，说明用户
// 从没手动改过（手动改过的不会是这些整数默认）——升级时随新默认（不限制）走，
// 用户自己设的值（如 300）不受影响。30=首版默认，100=上一次提升后的默认，
// 200=上一次提升后的默认，1000=旧设置页的输入上限（填它的人就是想拉满，
// 意图等同于不限制）。
var maxStepsLegacyDefaults = []int{30, 100, 200, 1000}

// isLegacyMaxSteps 该值是不是历次旧默认之一（升级提值的判定，见上面注释）。
func isLegacyMaxSteps(n int) bool {
	for _, v := range maxStepsLegacyDefaults {
		if n == v {
			return true
		}
	}
	return false
}

func DefaultConfig() *Config {
	wd, _ := os.Getwd()
	// 工作区默认目录：桌面 = 当前目录（安装形态锚到 exe 旁，见 main.go）；
	// 安卓壳经 JX_WORKSPACE_DEFAULT 注入 engineRoot/workspace——应用私有目录
	// 里单独的工作区子目录，引擎配置/数据/运行时不再泡在工作区里（边界清晰）。
	ws := wd
	if v := os.Getenv("JX_WORKSPACE_DEFAULT"); v != "" {
		ws = v
	}
	return &Config{
		Port:                8788,
		BindHost:            "127.0.0.1",
		BaseURL:             "https://api.deepseek.com/v1",
		APIKey:              "",
		Model:               "deepseek-chat",
		Workspace:           ws,
		MaxSteps:            maxStepsDefault,
		AllowShell:          true,
		DataDir:             "data",
		CompactMode:         "llm",
		KeepChars:           100000, // 10 万字 ≈ 7.5 万 token 的近期原文（各工具压缩后常见余量）
		CompactInterval:     400000, // 40 万字（单次压缩约 30 万 + 保留 10 万）；token 单位时为 40 万 token
		CompactUnit:         "chars",
		CompactThreshold:    0.8,
		CompactLlmPct:       80,
		SnapshotDays:        7,
		SnapshotMaxMB:       32,
		SnapshotManualMaxMB: -1,
		SnapshotEnabled:     nil, // 缺省开启；nil 表示"未配置"，按开处理
		Experts:             defaultExperts(),
		RemoteAccess: RemoteAccessCfg{
			Enabled: false,
			Host:    "example.com",
			Port:    2227,
			MyID:    "",            // 留空：首次加载时为每台安装生成唯一设备号（多人安装互不冲突）
			RegKey:  "chat_p2p_v1", // legacy 全局口令：仅兼容未升级的旧网关；注册凭据实际走 PairKey 派生的 auth（remoteaccess.go hubAuthKey）
		},
		RegistryURL: "https://example.com:PORT", // 总服务器（公司名查询的入口）：内网发现超时后按公司名查它拿接入地址；"off"=禁用
		UpdateURL:   "https://example.com:3000/api/v1/repos/your-org/your-repo/releases/latest",
		UpdateMode:  "auto",
		AskAlert:    AskAlertCfg{AfterSec: askAlertDefaultSec, Text: askAlertDefaultText},
		MgrNotify:   MgrNotifyCfg{AfterSec: mgrNotifyDefaultSec},
	}
}

// LoadConfig 读取配置文件；文件不存在时写入一份默认配置再返回。
func LoadConfig(path string) (*Config, error) {
	cfg := DefaultConfig()
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		ensureRemoteIdentity(cfg, path)
		if err := cfg.Save(path); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	def := DefaultConfig()
	if cfg.Port == 0 {
		cfg.Port = def.Port
	}
	if cfg.BaseURL == "" {
		cfg.BaseURL = def.BaseURL
	}
	if cfg.Model == "" {
		cfg.Model = def.Model
	}
	if cfg.Workspace == "" {
		cfg.Workspace = def.Workspace
	}
	// 步数上限：老配置里还是历次默认（30 / 100 / 200 / 1000）的（说明没人手动
	// 改过）随新默认改成不限制（0），并在下面落盘；手动设过其他值的配置原样
	// 保留。缺这一项也是 0 = 不限制，天然对齐默认。负数一律归一成 0。
	maxStepsBumped := false
	if isLegacyMaxSteps(cfg.MaxSteps) {
		cfg.MaxSteps = maxStepsDefault
		maxStepsBumped = true
	}
	if cfg.MaxSteps < 0 {
		cfg.MaxSteps = 0
	}
	if cfg.DataDir == "" {
		cfg.DataDir = def.DataDir
	}
	if cfg.BindHost == "" {
		cfg.BindHost = def.BindHost
	}
	if cfg.SnapshotDays == 0 {
		cfg.SnapshotDays = def.SnapshotDays
	}
	if cfg.SnapshotMaxMB == 0 {
		cfg.SnapshotMaxMB = def.SnapshotMaxMB
	}
	if cfg.SnapshotManualMaxMB == 0 {
		cfg.SnapshotManualMaxMB = def.SnapshotManualMaxMB
	}
	if cfg.CompactMode != "rule" && cfg.CompactMode != "llm" && cfg.CompactMode != "off" {
		cfg.CompactMode = def.CompactMode
	}
	// KeepChars 允许 0（= 连最近一轮也不保留的全压语义，用户显式设置）；
	// 仅负数视为非法回默认。无键补默认在下方 compactKeys 迁移块处理。
	if cfg.KeepChars < 0 {
		cfg.KeepChars = def.KeepChars
	}
	if cfg.CompactThreshold <= 0.5 || cfg.CompactThreshold >= 1 {
		cfg.CompactThreshold = def.CompactThreshold
	}
	// 间隔制迁移（旧版为规则阈值百分比+冷却，已废）：老配置没有 compactInterval
	// 键时补默认 400000；旧 CompactMode=off 的语义是「自动压缩全关」，迁移为
	// interval=0（规则通道关）——llm 阈值仍按旧迁移保持。迁移过就立即落盘，
	// 配置文件与运行时口径一致；旧配置里遗留的 compactRulePct/compactCooldown
	// 键在结构体上已无对应字段，读入时被忽略、下次保存自然消失。
	compactMigrated := false
	var compactKeys map[string]json.RawMessage
	if json.Unmarshal(data, &compactKeys) == nil {
		// keepChars 无键补默认（零值与显式 0 不可区分，无键必是老存量而非
		// 用户置零——0 是合法的全压语义，不能被默认值覆盖）
		if _, hasKeep := compactKeys["keepChars"]; !hasKeep {
			cfg.KeepChars = def.KeepChars
			compactMigrated = true
		}
		_, hasInterval := compactKeys["compactInterval"]
		if !hasInterval {
			cfg.CompactInterval = def.CompactInterval
			if cfg.CompactMode == "off" {
				cfg.CompactInterval = 0
			}
			// 旧配置显式 compactRulePct:0 = 用户明确关闭规则通道（0=不启用是
			// 旧字段的既定语义）：迁移后语义延续为间隔 0，不因换机制被重新打开
			if rawRule, ok := compactKeys["compactRulePct"]; ok {
				var oldRule int
				if json.Unmarshal(rawRule, &oldRule) == nil && oldRule == 0 {
					cfg.CompactInterval = 0
				}
			}
			compactMigrated = true
		}
		_, hasUnit := compactKeys["compactUnit"]
		if !hasUnit {
			cfg.CompactUnit = def.CompactUnit
			compactMigrated = true
		}
		_, hasLlm := compactKeys["compactLlmPct"]
		if !hasLlm {
			cfg.CompactLlmPct = def.CompactLlmPct
			if cfg.CompactMode == "off" {
				cfg.CompactLlmPct = 0
			}
			compactMigrated = true
		}
	}
	if cfg.CompactInterval < 0 {
		cfg.CompactInterval = def.CompactInterval
	}
	if cfg.CompactUnit != "chars" && cfg.CompactUnit != "tokens" {
		cfg.CompactUnit = def.CompactUnit
	}
	if cfg.CompactLlmPct < 0 || cfg.CompactLlmPct > 100 {
		cfg.CompactLlmPct = def.CompactLlmPct
	}
	if cfg.CompactMode == "off" {
		cfg.CompactMode = def.CompactMode
		compactMigrated = true
	}
	// 远程接入：老配置没有这一键时补默认网关地址；设备号与配对密钥留空则
	// 为本机随机生成并立即落盘——每台安装天然拿到互不冲突的设备号和独立
	// 密钥，多台电脑共用一个网关也不会互相顶掉（网关按设备号路由注册）。
	if cfg.RemoteAccess.Host == "" {
		cfg.RemoteAccess.Host = def.RemoteAccess.Host
	}
	// 一次性迁移（2026-10）：老部署的远程网关填的是 QNAP DDNS 域名，而 2227 上
	// 的证书只覆盖 example.com/.cn——老域名生成的配对二维码/入口链接手机端必报
	// 证书错误。启动时静默迁移到自有域名（仅精确匹配本家老地址，不动其他公司的网关）。
	remoteHostMigrated := false
	if cfg.RemoteAccess.Host == "nas.example.org" {
		cfg.RemoteAccess.Host = "example.com"
		remoteHostMigrated = true
	}
	if cfg.RemoteAccess.Port == 0 {
		cfg.RemoteAccess.Port = def.RemoteAccess.Port
	}
	if cfg.RemoteAccess.RegKey == "" {
		cfg.RemoteAccess.RegKey = def.RemoteAccess.RegKey
	}
	ensureRemoteIdentity(cfg, path)
	// 一次性迁移（2026-10-25 明文停用）：存量配置里指向自家服务端的明文 http://
	// 地址（向导/市场/搜索/更新/公司通道 baseURL）统一收敛到
	// https://example.com:2227——TLS 证书只覆盖域名，IP 直连的 https 证书校验必
	// 失败；服务端明文口只剩 308 重定向。用户自配的第三方明文地址一律不碰。
	plainSelfMigrated := false
	swapPlain := func(u string) string {
		nu, ok := migrateLegacyPlainURL(u)
		if ok {
			plainSelfMigrated = true
		}
		return nu
	}
	cfg.WizardServer = swapPlain(cfg.WizardServer)
	cfg.WizardPublicServer = swapPlain(cfg.WizardPublicServer)
	cfg.MarketURL = swapPlain(cfg.MarketURL)
	cfg.SearchURL = swapPlain(cfg.SearchURL)
	cfg.UpdateURL = swapPlain(cfg.UpdateURL)
	for i := range cfg.Providers {
		if cfg.Providers[i].ID != wizardProviderID {
			continue // 只迁公司通道；私人提供方（本地 Ollama 等）的明文地址不动
		}
		cfg.Providers[i].BaseURL = swapPlain(cfg.Providers[i].BaseURL)
		cfg.Providers[i].PublicBaseURL = swapPlain(cfg.Providers[i].PublicBaseURL)
	}
	// 总服务器（公司名查询入口）：老配置没有这一键时补默认——外网客户端按
	// 公司名查它拿接入地址（内网发现超时后走）；显式 "off" 原样保留（禁用查询）。
	if cfg.RegistryURL == "" {
		cfg.RegistryURL = def.RegistryURL
	}
	// 更新检查：老配置没有这一键时补默认发布源；显式 "off" 原样保留
	// （转成空的话，下一次保存配置又会被补回默认，用户就关不掉了）
	if cfg.UpdateURL == "" {
		cfg.UpdateURL = def.UpdateURL
	}
	// 更新方式：老配置没有这一键时补默认 auto
	if cfg.UpdateMode == "" {
		cfg.UpdateMode = def.UpdateMode
	}
	// 询问提醒：老配置没有这一键时补默认（触发秒数与朗读文案）；AfterSec
	// 越界的也拉回范围（保存侧已钳，这里兜升级前手改配置文件的情况）。
	if cfg.AskAlert.AfterSec <= 0 {
		cfg.AskAlert.AfterSec = askAlertDefaultSec
	}
	if cfg.AskAlert.AfterSec < 30 {
		cfg.AskAlert.AfterSec = 30
	}
	if cfg.AskAlert.AfterSec > 600 {
		cfg.AskAlert.AfterSec = 600
	}
	if strings.TrimSpace(cfg.AskAlert.Text) == "" {
		cfg.AskAlert.Text = askAlertDefaultText
	}
	// 经理代理通知：老配置没有这一键时补默认 30 秒；越界的拉回范围（口径同
	// 询问提醒，兜升级前手改配置文件的情况）。
	if cfg.MgrNotify.AfterSec <= 0 {
		cfg.MgrNotify.AfterSec = mgrNotifyDefaultSec
	}
	if cfg.MgrNotify.AfterSec < 10 {
		cfg.MgrNotify.AfterSec = 10
	}
	if cfg.MgrNotify.AfterSec > 600 {
		cfg.MgrNotify.AfterSec = 600
	}
	// 敏感字段透明解密（dpapi:v1: 密文 → 明文；旧版明文原样）。
	// 首启迁移：明文照常读入，下一次 Save 自动转密文。
	cfg.APIKey = unprotectSecret(cfg.APIKey)
	for i := range cfg.Providers {
		cfg.Providers[i].APIKey = unprotectSecret(cfg.Providers[i].APIKey)
		for j := range cfg.Providers[i].Keys {
			cfg.Providers[i].Keys[j].APIKey = unprotectSecret(cfg.Providers[i].Keys[j].APIKey)
		}
	}
	// 搜索供应商（websearch_providers.go）：敏感字段同一套透明解密；老配置没有
	// 这一键时补默认四条（公司网关 + DeepSeek/Tavily/AnySearch，key 为空，由用户
	// 自己在设置页填）。用户写成空数组 = 明确的「我只要老网关」，不再补种。
	var searchKeys map[string]json.RawMessage
	if json.Unmarshal(data, &searchKeys) == nil {
		if _, has := searchKeys["searchProviders"]; !has {
			cfg.SearchProviders = defaultSearchProviders()
		}
	}
	for i := range cfg.SearchProviders {
		cfg.SearchProviders[i].APIKey = unprotectSecret(cfg.SearchProviders[i].APIKey)
	}
	cfg.SearchProviders = normalizeSearchProviders(cfg.SearchProviders)
	// 老配置兼容：没有 Providers 时把旧的三元组折算成第一个 provider
	if len(cfg.Providers) == 0 {
		providers := DefaultProviders()
		p := providers[0]
		p.BaseURL = cfg.BaseURL
		p.APIKey = cfg.APIKey
		p.Models = nil
		if cfg.Model != "" {
			p.Models = []ModelCfg{{ID: cfg.Model, ContextWindow: 1048576}}
		}
		cfg.Providers = []ProviderCfg{p}
	}
	// 多 Key（多分组）迁移：自定义供应商原来的供应商级单 Key（apiKey + models）
	// 折算成「只有一把 Key」的多 Key 结构——老配置加载即升级，用户不用重填；
	// 保存时 Keys 回读一致（config_migrate_test 有往返用例）。
	// 预置提供方与公司内部通道不迁：它们是系统/服务端维护的单通道（同步器直接
	// 改供应商级字段），保持单 Key 结构，运行时由 KeyList 合成隐含 Key 走同一套
	// 解析路径。
	keysMigrated := false
	for i := range cfg.Providers {
		p := &cfg.Providers[i]
		if len(p.Keys) > 0 || p.ID == wizardProviderID {
			continue
		}
		if _, isPreset := PresetByID(p.ID); isPreset {
			continue
		}
		if p.APIKey == "" && len(p.Models) == 0 {
			continue // 空卡片（新建未填）：没有可迁移的内容
		}
		p.Keys = []ProviderKeyCfg{{ID: newProviderKeyID(p), APIKey: p.APIKey, Models: p.Models}}
		keysMigrated = true
	}
	// 默认模型迁移：单模型时代的裸模型名（Config.Model）折算成完整格式写进
	// DefaultModel。只在配置文件里压根没有 defaultModel 这一键时执行——用户显式
	// 清空过（存了空值）就不再复活他的选择。迁完在日志里写明落在哪个渠道。
	defaultModelMigrated := false
	var dmKeys map[string]json.RawMessage
	if json.Unmarshal(data, &dmKeys) == nil {
		if _, has := dmKeys["defaultModel"]; !has {
			var note string
			defaultModelMigrated, note = migrateDefaultModel(cfg)
			if note != "" {
				log.Printf("[模型] 全局默认模型迁移：%s", note)
			}
		}
	}
	// 步数上限从历次默认迁移过（旧默认 → 不限制）、压缩从百分比阈值迁移到
	// 间隔制：立即落盘，让设置页读数、配置文件与运行时三者一致（免得用户看到
	// 配置文件里还是旧键，以为没生效；其他补全都留给下一次保存）
	if maxStepsBumped || remoteHostMigrated || compactMigrated || plainSelfMigrated || keysMigrated || defaultModelMigrated {
		if err := cfg.Save(path); err != nil {
			log.Printf("回写配置迁移失败: %v", err)
		}
	}
	cfg.syncLegacyFields()
	return cfg, nil
}

// newProviderKeyID 给一把 Key 生成供应商内唯一的稳定归属 id。
// 归属 id 一旦发给前端就会被会话/草稿记住，因此只在新 Key 落库时生成一次，
// 之后永不改写（改了等于把老会话的模型引用指向别的 Key）。
func newProviderKeyID(p *ProviderCfg) string {
	used := map[string]bool{}
	for _, k := range p.Keys {
		used[k.ID] = true
	}
	for i := 1; ; i++ {
		id := fmt.Sprintf("k%d", i)
		if !used[id] {
			return id
		}
	}
}

// legacyPlainHosts 自家服务端的已知地址——存量配置里可能以明文 http:// 记录
// （历史网关域名、内网 IP、外网域名），明文停用后统一收敛到正式域名。
var legacyPlainHosts = []string{"example.com", "jxaizz.cn", "nas.example.org", "192.0.2.210"}

// migrateLegacyPlainURL 把「http://<自家host>[:任意端口]<路径…>」迁到
// https://example.com:2227<路径…>（端口无关：2227/2228 是服务端口、3000/3001 是
// 内网明文直连 new-api 的历史下发，一律收敛到服务端 TLS 口；provider baseURL
// 的 /v1 路径随地址完整保留）。host 不在自家清单（用户自配的第三方明文服务）
// 原样返回。幂等：迁过即 https，不再匹配。
func migrateLegacyPlainURL(u string) (string, bool) {
	if !strings.HasPrefix(u, "http://") {
		return u, false
	}
	pu, err := url.Parse(u)
	if err != nil || pu.Hostname() == "" {
		return u, false
	}
	for _, x := range legacyPlainHosts {
		if pu.Hostname() == x {
			pu.Scheme = "https"
			pu.Host = "example.com:PORT"
			return pu.String(), true
		}
	}
	return u, false
}

// syncLegacyFields 让旧字段与第一个 provider 保持一致，老版本 exe 回退时仍能读。
// 多 Key 供应商按第一把 Key 折算（那把 Key 就是镜像到供应商级字段的那把）。
func (c *Config) syncLegacyFields() {
	if len(c.Providers) == 0 {
		return
	}
	p := c.Providers[0]
	c.BaseURL = p.BaseURL
	c.Model = ""
	// KeyList 至少一项：多 Key 取第一把，老的供应商级单 Key 就是合成的那一把
	first := p.KeyList()[0]
	c.APIKey = first.APIKey
	if len(first.Models) > 0 {
		c.Model = first.Models[0].ID
	}
}

func (c *Config) Save(path string) error {
	// 敏感字段落盘前 DPAPI 加密（副本上做，内存对象保持明文供运行时使用）：
	// 沙箱读不受限（官方同款语义），config.json 被命令直接读到的只是密文。
	out := *c
	out.Providers = make([]ProviderCfg, len(c.Providers))
	copy(out.Providers, c.Providers)
	if enc, err := protectSecret(out.APIKey); err == nil {
		out.APIKey = enc
	}
	for i := range out.Providers {
		// 多 Key 供应商：每把 Key 的密钥各自加密；供应商级 apiKey/models 作为
		// 第一把 Key 的镜像一并写盘（老版本 exe 回退时仍读得到唯一那把 Key，
		// 新版本加载时以 Keys 为准、镜像只是派生结果，不会成为第二个事实来源）。
		if n := len(out.Providers[i].Keys); n > 0 {
			keys := make([]ProviderKeyCfg, n)
			copy(keys, out.Providers[i].Keys)
			for j := range keys {
				if enc, err := protectSecret(keys[j].APIKey); err == nil {
					keys[j].APIKey = enc
				}
			}
			out.Providers[i].Keys = keys
			out.Providers[i].APIKey = keys[0].APIKey
			out.Providers[i].Models = keys[0].Models
		} else if enc, err := protectSecret(out.Providers[i].APIKey); err == nil {
			out.Providers[i].APIKey = enc
		}
	}
	// 搜索供应商的 API Key 同一套加密（副本上做，内存对象保持明文供运行时使用）。
	// 用户在设置页粘贴的明文 key 只在内存里存在，落盘的永远是 dpapi:v1: 密文。
	if n := len(c.SearchProviders); n > 0 {
		out.SearchProviders = make([]SearchProviderCfg, n)
		copy(out.SearchProviders, c.SearchProviders)
		for i := range out.SearchProviders {
			if enc, err := protectSecret(out.SearchProviders[i].APIKey); err == nil {
				out.SearchProviders[i].APIKey = enc
			}
		}
	}
	data, err := json.MarshalIndent(&out, "", "  ")
	if err != nil {
		return err
	}
	// 原子写：先写同目录临时文件再改名覆盖。非原子写曾因并发保存
	// （多实例/诊断脚本与正在运行的实例交错写同一文件）把 config.json
	// 写成交错残片——密文尾部截断后永久无法解密，设置保存表现为随机
	// 失败。改名覆盖是原子的：读者要么看到旧文件、要么看到新文件。
	// 0600：Windows 上密钥已有 DPAPI 加密、权限位无意义；安卓/Linux
	// 上密钥为明文落盘（无系统级密钥服务），配置文件只归当前用户所有。
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0600); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
