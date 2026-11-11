// 更新检查：定期拉取发布源（默认 Gitea releases/latest API），tag_name 比本
// 程序版本新就经事件流给所有客户端推「新版本可用」；最新版本信息同时进
// /api/status（客户端刚打开也能看到，弥补 SSE 无回放）。检查结果落
// data/update.json——升级过版本后只提醒一次，重启不复读。
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// SelfUpdateDisabled 二开版本专用开关：屏蔽「上游发布把本程序（引擎 exe / 完整
// 安装包）顶掉」的整条路。
//
// 为什么要这个开关：本仓库是二开版本，程序本身带着我们的改动。照上游的更新检查
// 走，上游一发新版就会换来路不明的 exe、或用完整安装包把我们的版本整体覆盖，
// 二开成果当场被顶掉且不可逆。
//
// 为真时（下面各处守卫协同，逐条可读）：
//   - 启动首查与每小时轮询都不拉发布源（StartUpdateChecker）；
//   - checkUpdateOnce 只放行市场技能包那条异步任务，随即返回——一个发布源请求
//     都不发；
//   - POST /api/update/check 直接回「已禁用」，不联网；
//   - /api/status 的 update 恒为「没有可更新版本」，前端更新横幅/胶囊/待重启
//     提示因此一律不出现；
//   - 整包下载运行、engine 分块交换、待重启接力（能替换程序本体的三条路）
//     全部早退。
//
// 只关「程序自身」这一块：技能市场（marketURL）、内置组件、便携 Python 运行时、
// 组件块这些更新通道的代码原样保留（它们与引擎更新共用发布源检查，检查一停自然
// 也不跑，但没有任何一行是为这个开关而改坏的）。
//
// 想恢复上游行为：把它改回 false，其余一行都不用动。
const SelfUpdateDisabled = true

// selfUpdateBlockedMsg 二开版本拒绝「程序自身更新」动作时的统一提示（接口回包
// 与错误信息共用，用户可读）。
const selfUpdateBlockedMsg = "二开版本已禁用程序自身更新（上游发布不会覆盖本版本）"

// UpdateState 最近一次检查到的发布信息（内存态，/api/status 直接引用）。
type UpdateState struct {
	Disabled    bool   `json:"disabled,omitempty"`    // 二开版本：程序自身更新被源码级开关屏蔽（前端据此收起「软件更新」区）
	Available   bool   `json:"available"`             // 最新版本比当前新
	Latest      string `json:"latest,omitempty"`      // 最新版本号（tag_name，去 v 前缀）
	Notes       string `json:"notes,omitempty"`       // 发布说明（body 截断）
	URL         string `json:"url,omitempty"`         // 发布页（html_url）
	DownloadURL string `json:"downloadUrl,omitempty"` // 安装包直链（完整安装路：横幅「确认更新」与设置页按钮共用）
	PublishedAt string `json:"publishedAt,omitempty"`
	Current     string `json:"current"`             // 当前运行版本
	CheckedAt   string `json:"checkedAt,omitempty"` // 最近一次成功检查时间
	LastError   string `json:"lastError,omitempty"` // 最近一次检查/应用失败原因（发布源不可达等）
	// 分块静默更新（update_blocks.go）
	BlocksMode     bool              `json:"blocksMode,omitempty"`     // 发布源带分块清单（走静默分块路）
	PendingRestart bool              `json:"pendingRestart,omitempty"` // engine 已交换，重启后生效
	PendingVersion string            `json:"pendingVersion,omitempty"`
	Applied        map[string]string `json:"applied,omitempty"` // 已静默应用的组件块 id→版本
}

var (
	updateMu       sync.Mutex
	updateState    = UpdateState{Current: appVersion}
	updateStop     chan struct{}
	updateNotified string // 已广播过的版本：同一版本只推一次
	updateCheckMu  sync.Mutex
)

func updateStatePath(a *App) string { return filepath.Join(a.Cfg.DataDir, "update.json") }

// versionCompare 版本号两套体系：新的「日期+V序号」（20261008V1，同日发多版用
// V2 V3）与旧的点分号（0.1.9）。日期体系恒新于旧体系（体系切换只升不降）；同为
// 日期体系先比 8 位日期再比 V 序号（V10 > V9）；同为旧体系按点分数字段比较
// （非数字段按 0 处理，长度不齐补零——"0.2" == "0.2.0"）。
func versionCompare(a, b string) int {
	ad, an, aok := parseDateVersion(a)
	bd, bn, bok := parseDateVersion(b)
	switch {
	case aok && bok:
		if ad != bd {
			if ad > bd {
				return 1
			}
			return -1
		}
		if an != bn {
			if an > bn {
				return 1
			}
			return -1
		}
		return 0
	case aok:
		return 1 // 新体系 > 旧体系
	case bok:
		return -1
	}
	nums := func(s string) [3]int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		var out [3]int
		for i, p := range strings.Split(s, ".") {
			if i >= 3 {
				break
			}
			n, _ := strconv.Atoi(strings.TrimSpace(p))
			out[i] = n
		}
		return out
	}
	an2, bn2 := nums(a), nums(b)
	for i := 0; i < 3; i++ {
		if an2[i] > bn2[i] {
			return 1
		}
		if an2[i] < bn2[i] {
			return -1
		}
	}
	return 0
}

// parseDateVersion 解析「日期-序号」版本号（20261013-1 → 20261013, 1）；旧写法「日期+V序号」（20261008V10 → 20261008, 10）一并识别——存量已装版本报 V 格式，混比时仍走精确的日期+序号比较。
var dateVersionRe = regexp.MustCompile(`^(\d{8})(?:[Vv]|-)(\d+)$`)

func parseDateVersion(s string) (date, serial int64, ok bool) {
	m := dateVersionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, 0, false
	}
	d, err1 := strconv.ParseInt(m[1], 10, 64)
	n, err2 := strconv.ParseInt(m[2], 10, 64)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return d, n, true
}

// StartUpdateChecker 启动后台轮询：启动 30s 后首查（不拖慢启动），之后每小时一次
// （原来是 6h，用户要求「更新提示改每小时检查」：发版当天客户端一小时内就能看到横幅）。
func StartUpdateChecker(a *App) {
	if SelfUpdateDisabled {
		// 二开版本：程序自身的更新检查一次都不发（url 传空串，checkUpdateOnce
		// 见到开关即早退）。仍留一条同节奏的轮询，只为「企业技能包/工作流自动
		// 更新」——那是市场的事，不属于程序本体更新，不该被这个开关连坐停掉。
		a.startUpdatePolling("")
		return
	}
	url := a.Cfg.UpdateURL
	if url == "" || url == "off" {
		return
	}
	a.startUpdatePolling(url)
}

// startUpdatePolling 轮询主体：30s 后首跑，此后每小时一次，直到 StopUpdateChecker。
func (a *App) startUpdatePolling(url string) {
	updateStop = make(chan struct{})
	go func() {
		select {
		case <-time.After(30 * time.Second):
		case <-updateStop:
			return
		}
		a.checkUpdateOnce(url)
		t := time.NewTicker(time.Hour)
		defer t.Stop()
		for {
			select {
			case <-t.C:
				a.checkUpdateOnce(url)
			case <-updateStop:
				return
			}
		}
	}()
}

func StopUpdateChecker() {
	if updateStop != nil {
		close(updateStop)
		updateStop = nil
	}
}

// releaseInfo Gitea releases API（/releases/latest）用到的字段。
type releaseInfo struct {
	TagName     string         `json:"tag_name"`
	Name        string         `json:"name"`
	Body        string         `json:"body"`
	HTMLURL     string         `json:"html_url"`
	PublishedAt string         `json:"published_at"`
	Assets      []releaseAsset `json:"assets"`
}

// releaseAsset 发布附件（安装包等）。
type releaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

// pickInstaller 从附件里挑安装包：优先名字含「安装包」的 .exe，其次任意 .exe。
// 都没有返回空串——客户端就没有可下载物，横幅只提示不发版。
// sourceURL 是本次检查用的发布源地址：Gitea 的 browser_download_url 由服务端
// ROOT_URL 生成，若哪天被配回局域网地址，这里按发布源的同 scheme/host 改写，
// 保证手机等外网设备拿到的永远是可下载的公网地址。
func pickInstaller(assets []releaseAsset, sourceURL string) string {
	fallback := ""
	for _, a := range assets {
		n := strings.ToLower(a.Name)
		if !strings.HasSuffix(n, ".exe") {
			continue
		}
		url := publicizeURL(a.BrowserDownloadURL, sourceURL)
		if strings.Contains(a.Name, "安装包") || strings.Contains(strings.ToLower(a.Name), "setup") || strings.Contains(n, "installer") {
			return url
		}
		if fallback == "" {
			fallback = url
		}
	}
	return fallback
}

// publicizeURL 检测到下载地址指向私网/回环地址时，改用发布源的公网
// scheme+host 重组（路径与查询原样保留）；本来就是公网地址则原样返回。
func publicizeURL(raw, sourceURL string) string {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw
	}
	if !isPrivateHost(u.Hostname()) {
		return raw
	}
	b, err := url.Parse(sourceURL)
	if err != nil || b.Host == "" || isPrivateHost(b.Hostname()) {
		return raw
	}
	u.Scheme = b.Scheme
	u.Host = b.Host
	return u.String()
}

func (a *App) checkUpdateOnce(url string) {
	// 手动检查与每小时轮询可能同拍触发；已有一轮在跑就跳过（结果共享）
	if !updateCheckMu.TryLock() {
		return
	}
	defer updateCheckMu.Unlock()
	// 顺带检查企业技能包 / 工作流更新（market.go）：发布源检查与技能包更新互不依赖，
	// 异步跑——发布源不可达时技能包更新照常进行，反之亦然
	go a.autoUpdateMarketSkills()
	if SelfUpdateDisabled {
		// 二开版本：到此为止，不拉发布源、不查程序自身更新（market 那条已在上面
		// 异步起跑，市场不受影响）。整包/分块那套也就永远拿不到发布信息。
		return
	}
	// 检查更新即清理：旧的整包安装包在这时删掉（启动 30s 首查 + 每小时轮询
	// 天然就是清理时机，见 handleUpdateInstaller 的说明）。下载进行中跳过。
	go cleanupInstallerCache(installerCacheDir(a))
	client := newCompanyHTTPClient(20 * time.Second)
	resp, err := client.Get(url)
	if err != nil {
		setUpdateError(err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 404 = 发布源还没有任何 release：静默视为「无更新」，不算错误
		if resp.StatusCode == http.StatusNotFound {
			setUpdateError("")
			return
		}
		setUpdateError("发布源返回 HTTP " + strconv.Itoa(resp.StatusCode))
		return
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		setUpdateError(err.Error())
		return
	}
	var rel releaseInfo
	if err := json.Unmarshal(raw, &rel); err != nil || rel.TagName == "" {
		setUpdateError("发布源响应不是 releases/latest 格式")
		return
	}
	setUpdateError("")
	// 分块静默更新（update_blocks.go）：附件里有 blocks.json 时先取清单——
	// 清单覆盖 engine 且自动模式开启则抑制「新版本可用」整包横幅（改走静默
	// 应用 + update_ready 就绪横幅）；清单缺失/失败完全回退整包路
	blocks := fetchUpdateBlocks(rel, url)
	if blocks != nil {
		updateMu.Lock()
		updateState.BlocksMode = true
		updateMu.Unlock()
	}
	silentEngine := blocks != nil && a.silentUpdateEnabled() &&
		blocks.Engine != nil && versionCompare(blocks.Engine.Version, appVersion) > 0
	applyUpdateInfo(a, rel, url, silentEngine)
	if blocks != nil && a.silentUpdateEnabled() {
		a.applyBlocksPlan(blocks, rel, url)
	}
}

// silentUpdateEnabled 更新方式：auto（默认）=分块静默应用；manual=只提醒不动手。
func (a *App) silentUpdateEnabled() bool { return a.Cfg.UpdateMode != "manual" }

func setUpdateError(msg string) {
	updateMu.Lock()
	defer updateMu.Unlock()
	updateState.LastError = msg
	if msg == "" {
		updateState.CheckedAt = time.Now().Format("2006-01-02 15:04")
	}
}

// statusUpdateSnapshot /api/status 对外报的更新状态。二开版本恒定「没有可更新
// 版本」：前端的置顶更新横幅、左下角更新胶囊、待重启提示都只看 available /
// pendingRestart，这里全空即一律不出现，也不需要前端各自加开关。
// 内部状态（updateSnapshot）不在这里改，免得把已有的状态机搅乱。
func statusUpdateSnapshot() UpdateState {
	if SelfUpdateDisabled {
		return UpdateState{Current: appVersion, Disabled: true}
	}
	return updateSnapshot()
}

func applyUpdateInfo(a *App, rel releaseInfo, sourceURL string, suppressBanner bool) {
	latest := strings.TrimPrefix(strings.TrimSpace(rel.TagName), "v")
	notes := strings.TrimSpace(rel.Body)
	if len(notes) > 500 {
		notes = notes[:500] + "…"
	}
	download := pickInstaller(rel.Assets, sourceURL)
	updateMu.Lock()
	updateState.Latest = latest
	updateState.Notes = notes
	updateState.URL = publicizeURL(rel.HTMLURL, sourceURL)
	updateState.DownloadURL = download
	updateState.PublishedAt = rel.PublishedAt
	updateState.Available = versionCompare(latest, appVersion) > 0
	shouldNotify := updateState.Available && updateNotified != latest && !suppressBanner
	if shouldNotify {
		updateNotified = latest
	}
	updateMu.Unlock()

	// 提醒过一次就落盘记 version：重启后不重复弹（版本再次更新才会重新提醒）。
	// 事件只当「门铃」，前端收到后拉 /api/status 取含 downloadUrl 的全量信息。
	if shouldNotify && !alreadyNotifiedVersion(a, latest) {
		markNotifiedVersion(a, latest)
		log.Printf("[更新] 发现新版本 %s（当前 %s），已通知在线客户端", latest, appVersion)
		a.broadcast(Event{Type: "update_available", Title: latest})
	}
}

func notifiedVersionPath(a *App) string { return filepath.Join(a.Cfg.DataDir, "update.json") }

func alreadyNotifiedVersion(a *App, v string) bool {
	raw, err := os.ReadFile(notifiedVersionPath(a))
	if err != nil {
		return false
	}
	var m map[string]string
	if json.Unmarshal(raw, &m) != nil {
		return false
	}
	return m["notified"] == v
}

func markNotifiedVersion(a *App, v string) {
	raw, _ := json.Marshal(map[string]string{"notified": v, "at": time.Now().Format(time.RFC3339)})
	_ = os.MkdirAll(a.Cfg.DataDir, 0755)
	_ = os.WriteFile(notifiedVersionPath(a), raw, 0644)
}

/* ---------- 整包安装包：静默下载 + 运行 + 检查更新时清理 ---------- */

// 整包路（横幅「确认更新」/设置页按钮）的工作方式：后端静默下载发布附件到
// data/update-cache/，完成后直接运行安装包——用户不接触浏览器与下载目录，
// 全程只有横幅行内提示（update_installer 事件），失败绝不弹窗换页。旧安装包
// 不设保留期：checkUpdateOnce 每轮（启动 30s 首查 + 每小时）清理整个目录，
// 正在运行中的安装器文件被系统占用删不掉，跳过即可。
var installerBusy atomic.Bool

// installerCacheDir 整包安装包的落盘目录。
func installerCacheDir(a *App) string { return filepath.Join(a.Cfg.DataDir, "update-cache") }

// cleanupInstallerCache 删除整包安装包缓存（下载进行中整体跳过；单个文件
// 删除失败——如安装器正被运行——静默跳过，下一轮再删）。
func cleanupInstallerCache(dir string) {
	if installerBusy.Load() {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// handleUpdateInstaller POST /api/update/installer：静默下载当前版本的完整
// 安装包并运行。响应只是「已开始」，进度与结果经 update_installer 事件到达
// （data: {stage: downloading|launched|error, text}）。
func (a *App) handleUpdateInstaller(w http.ResponseWriter, r *http.Request) {
	if SelfUpdateDisabled {
		// 二开版本：整包安装会覆盖掉我们自己的 exe，这条路整个不开放
		writeErr(w, http.StatusBadRequest, selfUpdateBlockedMsg)
		return
	}
	updateMu.Lock()
	dl := updateState.DownloadURL
	updateMu.Unlock()
	if dl == "" {
		writeErr(w, http.StatusBadRequest, "发布源没有可下载的安装包")
		return
	}
	if !installerBusy.CompareAndSwap(false, true) {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "busy": true})
		return
	}
	go func() {
		defer installerBusy.Store(false)
		if err := a.downloadAndRunInstaller(dl); err != nil {
			log.Printf("[更新] 安装包下载失败: %v", err)
			a.broadcast(Event{Type: "update_installer", Data: map[string]any{"stage": "error", "text": err.Error()}})
		}
	}()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// downloadAndRunInstaller 下载发布附件到缓存目录（.part 落盘后改名，防半截
// 文件被运行）并交给系统默认程序运行（安装包 exe 即安装器接管）。
func (a *App) downloadAndRunInstaller(dl string) error {
	if SelfUpdateDisabled {
		// 兜底：不管上游调用方从哪进来，二开版本都不下载/不运行会覆盖本程序的包
		return fmt.Errorf("%s", selfUpdateBlockedMsg)
	}
	dir := installerCacheDir(a)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	name := installerFileName(dl)
	a.broadcast(Event{Type: "update_installer", Data: map[string]any{"stage": "downloading"}})
	client := newCompanyHTTPClient(15 * time.Minute)
	resp, err := client.Get(dl)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("发布源返回 HTTP %d", resp.StatusCode)
	}
	part := filepath.Join(dir, name+".part")
	f, err := os.Create(part)
	if err != nil {
		return err
	}
	size, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		_ = os.Remove(part)
		return copyErr
	}
	final := filepath.Join(dir, name)
	if err := os.Rename(part, final); err != nil {
		return err
	}
	log.Printf("[更新] 安装包已就绪（%s，%.1f MB），启动安装程序", final, float64(size)/1048576)
	if err := openPathWithDefaultApp(final); err != nil {
		return fmt.Errorf("安装包已下载但启动失败: %w", err)
	}
	a.broadcast(Event{Type: "update_installer", Data: map[string]any{"stage": "launched", "text": final}})
	return nil
}

// installerFileName 从发布附件直链取文件名（URL 解码；Gitea 附件路径含中文
// 文件名的百分号编码）。取不到退回固定名。
func installerFileName(dl string) string {
	if u, err := url.Parse(dl); err == nil {
		if i := strings.LastIndexByte(u.Path, '/'); i >= 0 && len(u.Path) > i+1 {
			n := u.Path[i+1:]
			if dec, err := url.PathUnescape(n); err == nil {
				return dec
			}
			return n
		}
	}
	return "installer-package.exe"
}
