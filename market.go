package main

// 技能/组件/插件市场（客户端半边）：从市场服务器（jxai-server，独立仓库 D:\智能体工作区\jxaiserver）拉
// 清单，一键安装/更新/卸载，按类型路由到三套既有体系：
//
//	skill     → addUserSkill / deleteUserSkill（技能/user/<id>/；更新=先删再装）
//	component → 组件/<id>/ 落盘 + Components.Rescan（前端随后 CX.refresh 热加载）
//	plugin    → ccplugin 体系的 ccInstallFromPath / Uninstall（市场归属名 jx-market）
//
// 安装记账 data/market_installed.json：技能与组件本体没有可靠的版本来源，
// 安装时记账是「已装版本 / 可更新」判定的唯一事实；插件的版本以 ccplugin
// 安装记录为准。市场地址在 config.marketURL（设置页「网络」分区填写），
// 未配置时返回建议值（与中继网关同 host 时市场就在同一台 jxai-server 上）。
//
// 与服务端的协议契约（两侧文件头注释为准，改动需同步）：
//   - 清单 GET /market/manifest.json（匿名只读，60s 缓存）；
//   - 下载 GET /market/items/<type>/<id>/<ver>.zip；安装成功后 POST /api/market/report-install；
//   - 全部请求带 X-Jxai-User 头（值=发布者姓名，没设就不带）——服务端用它记
//     下载/安装明细（market_events.jsonl）与条目归属 owner；
//   - 发布/下架见 market_publish.go（用户端点 /api/market/publish、/api/market/unpublish，
//     这两个端点除 X-Jxai-User 外还带 X-Jxai-Device 设备头——服务端三要素审计
//     「用户名+设备名+IP」，缺设备头 403；归属校验在服务端；下架=进服务端回收站）。

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// marketPluginHome 市场安装的 Claude 插件归属的市场名（安装键 name@jx-market；
// 服务端发布插件时条目 id 即 ccSanitizeName(plugin 名)，两端键对齐）。
const marketPluginHome = "jx-market"

type marketEntryIn struct { // 服务器清单条目
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description"`
	Tags        []string `json:"tags"`
	Zip         string   `json:"zip"`
	SHA256      string   `json:"sha256"`
	Size        int64    `json:"size"`
	// Owner 上传者（jxai-server 从 X-Jxai-User 头记录并回传）。归属校验在
	// 服务端（发布/下架都由它判「只能动自己发的」）；客户端拿它与本机发布者
	// 姓名比对只是为了决定显不显示「更新发布 / 删除」按钮（纯 UX，不是安全边界）。
	// 老条目为空 → 谁的卡片上都没有管理按钮。
	Owner string `json:"owner,omitempty"`
}

type marketManifestIn struct {
	Name       string          `json:"name"`
	UpdatedAt  string          `json:"updatedAt"`
	Skills     []marketEntryIn `json:"skills"`
	Components []marketEntryIn `json:"components"`
	Plugins    []marketEntryIn `json:"plugins"`
	Mcps       []marketEntryIn `json:"mcps"`
}

// marketEntryOut 清单条目 + 本机安装态（前端渲染用）。
type marketEntryOut struct {
	marketEntryIn
	Installed        bool   `json:"installed"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	Update           bool   `json:"update"`
	// BuiltIn 只对组件有意义：该 id 在本机是内置组件（随包分发、随静默更新
	// 升级）。前端据此不给「安装/卸载」（市场不该覆盖内置组件），只提示去
	// 设置 → 组件 关开关。
	BuiltIn bool `json:"builtin,omitempty"`
}

// marketMe 本机发布者身份（用于前端显隐「更新发布 / 删除」按钮的 UX 比对；
// 真正的归属校验在服务端，见 X-Jxai-User 头与 market_publish.go）。
type marketMe struct {
	Name string `json:"name"`
}

type marketView struct {
	Configured bool             `json:"configured"`
	URL        string           `json:"url,omitempty"`
	Name       string           `json:"name,omitempty"`
	Suggested  string           `json:"suggested,omitempty"` // 未配置时的建议值（中继网关同 host）
	Me         *marketMe        `json:"me,omitempty"`        // 本机发布者身份（空=还没设置姓名，前端不显示发布者管理按钮）
	Error      string           `json:"error,omitempty"`
	UpdatedAt  string           `json:"updatedAt,omitempty"`
	Skills     []marketEntryOut `json:"skills"`
	Components []marketEntryOut `json:"components"`
	Plugins    []marketEntryOut `json:"plugins"`
	Mcps       []marketEntryOut `json:"mcps"`
}

type marketInstallRec struct {
	Version     string `json:"version"`
	Key         string `json:"key,omitempty"` // plugin: name@market；component: 组件目录名；skill: 技能 id
	InstalledAt string `json:"installedAt"`
}

type marketCacheEntry struct {
	url string
	at  time.Time
	man *marketManifestIn
}

var (
	marketHTTPClient     = newCompanyHTTPClient(15 * time.Second)
	marketDownloadClient = newCompanyHTTPClient(5 * time.Minute)
)

func (a *App) marketRecsPath() string { return filepath.Join(a.Cfg.DataDir, "market_installed.json") }

func (a *App) loadMarketRecs() map[string]marketInstallRec {
	a.marketMu.Lock()
	defer a.marketMu.Unlock()
	recs := map[string]marketInstallRec{}
	data, err := os.ReadFile(a.marketRecsPath())
	if err == nil {
		_ = json.Unmarshal(data, &recs)
	}
	return recs
}

func (a *App) saveMarketRec(typ, id string, rec marketInstallRec) error {
	a.marketMu.Lock()
	defer a.marketMu.Unlock()
	recs := map[string]marketInstallRec{}
	data, err := os.ReadFile(a.marketRecsPath())
	if err == nil {
		_ = json.Unmarshal(data, &recs)
	}
	recs[typ+":"+id] = rec
	out, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(a.Cfg.DataDir, 0755); err != nil {
		return err
	}
	return writeFileAtomic(a.marketRecsPath(), out)
}

func (a *App) removeMarketRec(typ, id string) {
	a.marketMu.Lock()
	defer a.marketMu.Unlock()
	recs := map[string]marketInstallRec{}
	data, err := os.ReadFile(a.marketRecsPath())
	if err == nil {
		_ = json.Unmarshal(data, &recs)
	}
	if _, ok := recs[typ+":"+id]; !ok {
		return
	}
	delete(recs, typ+":"+id)
	if out, err := json.MarshalIndent(recs, "", "  "); err == nil {
		_ = writeFileAtomic(a.marketRecsPath(), out)
	}
}

// marketManifestURL 市场地址规范化（2026-11-03 用户口径）：设置里只填服务器地址
// （域名/IP + 端口，如 https://jxaizz.com:2227），路径后缀由程序自动拼——
// /market/manifest.json；已带后缀或反代子路径（如 /jxai）的旧值原样兼容。
func marketManifestURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	path := strings.TrimRight(u.Path, "/")
	for _, suf := range []string{"/market/manifest.json", "/market"} {
		if strings.HasSuffix(path, suf) { // 只剥一层：基地址上不该叠两层后缀
			path = strings.TrimRight(strings.TrimSuffix(path, suf), "/")
			break
		}
	}
	return u.Scheme + "://" + u.Host + path + "/market/manifest.json"
}

// fetchMarketManifest 拉市场清单（60s 缓存；force 用于安装前取最新）。带上
// 身份头（设置了发布者姓名时）——服务端把它记进下载/安装明细（events.go）。
func (a *App) fetchMarketManifest(rawURL string, force bool) (*marketManifestIn, error) {
	rawURL = marketManifestURL(rawURL)
	a.marketMu.Lock()
	if !force && a.marketCache.man != nil && a.marketCache.url == rawURL &&
		time.Since(a.marketCache.at) < time.Minute {
		m := a.marketCache.man
		a.marketMu.Unlock()
		return m, nil
	}
	a.marketMu.Unlock()
	req, err := a.marketRequest(context.Background(), http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := marketHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, 10<<20))
	if err != nil {
		return nil, err
	}
	var m marketManifestIn
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("清单不是合法 JSON: %w", err)
	}
	a.marketMu.Lock()
	a.marketCache = marketCacheEntry{url: rawURL, at: time.Now(), man: &m}
	a.marketMu.Unlock()
	return &m, nil
}

// marketZipURL 清单里的 zip 路径是绝对路径（/market/items/...），拼上清单 URL
// 的 origin；也兼容清单直接写完整 URL 的情况。
func marketZipURL(baseURL, zipPath string) (string, error) {
	if strings.HasPrefix(zipPath, "http://") || strings.HasPrefix(zipPath, "https://") {
		return zipPath, nil
	}
	u, err := url.Parse(baseURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("市场地址不合法: %s", baseURL)
	}
	return u.Scheme + "://" + u.Host + zipPath, nil
}

func marketEntriesOf(m *marketManifestIn, typ string) []marketEntryIn {
	switch typ {
	case "skill":
		return m.Skills
	case "component":
		return m.Components
	case "plugin":
		return m.Plugins
	case "mcp":
		return m.Mcps
	}
	return nil
}

func marketTypeOK(t string) bool {
	return t == "skill" || t == "component" || t == "plugin" || t == "mcp"
}

/* ---------- 视图：清单 + 本机安装态 ---------- */

// marketMe 本机发布者身份（姓名为空 = 还没设置：前端不显示发布者管理按钮，
// 发布请求也会在本地被拦下先提示补姓名；重名视同同一人——真凭证上线前过渡）。
func (a *App) marketMe() *marketMe {
	name := strings.TrimSpace(a.Cfg.PublisherName)
	if name == "" {
		return nil
	}
	return &marketMe{Name: name}
}

func (a *App) buildMarketView() marketView {
	v := marketView{Skills: []marketEntryOut{}, Components: []marketEntryOut{}, Plugins: []marketEntryOut{}, Mcps: []marketEntryOut{}, Me: a.marketMe()}
	raw := strings.TrimSpace(a.Cfg.MarketURL)
	if raw == "" {
		if a.Cfg.RemoteAccess.Host != "" && a.Cfg.RemoteAccess.Port > 0 {
			v.Suggested = fmt.Sprintf("https://%s:%d",
				a.Cfg.RemoteAccess.Host, a.Cfg.RemoteAccess.Port)
		}
		return v
	}
	v.Configured = true
	v.URL = raw
	man, err := a.fetchMarketManifest(raw, false)
	if err != nil {
		v.Error = fmt.Sprintf("市场清单获取失败: %v", err)
		return v
	}
	v.Name, v.UpdatedAt = man.Name, man.UpdatedAt
	recs := a.loadMarketRecs()
	v.Skills = a.decorateEntries("skill", man.Skills, recs)
	v.Components = a.decorateEntries("component", man.Components, recs)
	v.Plugins = a.decorateEntries("plugin", man.Plugins, recs)
	v.Mcps = a.decorateEntries("mcp", man.Mcps, recs)
	return v
}

func (a *App) decorateEntries(typ string, in []marketEntryIn, recs map[string]marketInstallRec) []marketEntryOut {
	out := make([]marketEntryOut, 0, len(in))
	for _, e := range in {
		o := marketEntryOut{marketEntryIn: e}
		rec, hasRec := recs[typ+":"+e.ID]
		switch typ {
		case "skill":
			if userDir, err := a.userSkillsDir(); err == nil {
				o.Installed = dirExists(filepath.Join(userDir, sanitizeSkillID(e.ID)))
			}
			if o.Installed {
				o.InstalledVersion = rec.Version
			}
		case "component":
			_, ok := a.Components.Manifest(e.ID)
			if !ok && hasRec {
				_, ok = a.Components.Manifest(rec.Key) // 发布方 -id 与组件清单 id 不一致时按记账找
			}
			o.Installed = ok
			o.BuiltIn = a.Components.IsBuiltin(e.ID) || (rec.Key != "" && a.Components.IsBuiltin(rec.Key))
			if o.Installed {
				o.InstalledVersion = rec.Version
			}
		case "plugin":
			st, ok := a.CCPlugins.stateOf(e.ID + "@" + marketPluginHome)
			if !ok && hasRec {
				st, ok = a.CCPlugins.stateOf(rec.Key)
			}
			o.Installed = ok && st != nil && st.Version != ""
			if o.Installed && st != nil {
				o.InstalledVersion = st.Version
				if !hasRec { // 手动装到 jx-market 名下的边角：没有记账时以实际版本兜底
					rec.Version = st.Version
				}
			}
		case "mcp":
			// 已装标记按来源（市场条目 id → source 标记）：条目里的全部连接器
			// 都注册自 market:<id> 即视为已装
			if a.MCPServers != nil {
				o.Installed = a.MCPServers.HasSource("market:" + e.ID)
			}
			if o.Installed {
				o.InstalledVersion = rec.Version
			}
		}
		o.Update = o.Installed && hasRec && rec.Version != "" && rec.Version != e.Version
		out = append(out, o)
	}
	return out
}

/* ---------- 安装 ---------- */

// marketDownloadZip 下载包体到临时文件并校验 sha256（带身份头，服务端记下载
// 明细）；调用方用 cleanup 清理。
func (a *App) marketDownloadZip(zipURL string, e marketEntryIn) (zipPath string, cleanup func(), err error) {
	req, rerr := a.marketRequest(context.Background(), http.MethodGet, zipURL, nil)
	if rerr != nil {
		return "", nil, rerr
	}
	resp, err := marketDownloadClient.Do(req)
	if err != nil {
		return "", nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.MkdirTemp("", "jxmkd-")
	if err != nil {
		return "", nil, err
	}
	fail := func(e error) (string, func(), error) {
		os.RemoveAll(tmp)
		return "", nil, e
	}
	zipPath = filepath.Join(tmp, "pkg.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return fail(err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 500<<20))
	f.Close()
	if err != nil {
		return fail(fmt.Errorf("下载写入失败: %w", err))
	}
	if e.SHA256 != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), e.SHA256) {
		return fail(fmt.Errorf("包校验失败（sha256 不符，下载可能不完整或被篡改）"))
	}
	return zipPath, func() { os.RemoveAll(tmp) }, nil
}

// findRootWith 在解压目录里找包含目标文件的根（根上或唯一子目录）。
func findRootWith(dir, file string) (string, error) {
	if _, err := os.Stat(filepath.Join(dir, file)); err == nil {
		return dir, nil
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	subs := []string{}
	for _, en := range entries {
		if en.IsDir() && !strings.HasPrefix(en.Name(), ".") {
			subs = append(subs, en.Name())
		}
	}
	if len(subs) == 1 {
		sub := filepath.Join(dir, subs[0])
		if _, err := os.Stat(filepath.Join(sub, file)); err == nil {
			return sub, nil
		}
	}
	return "", fmt.Errorf("压缩包里找不到 %s（需在根部或其唯一子目录里）", file)
}

// installMarketSkillZip 装一个市场技能 zip 到 技能/user/<id>/（手动安装与
// autoUpdateMarketSkills 共用；zip 由调用方经 marketDownloadZip 过 sha256 校验）。
// 已有旧版先挪到同目录隐藏备份，新版装失败把备份还原回去——更新失败不掉技能，
// 无人值守的自动更新尤其不能让技能凭空消失。
func (a *App) installMarketSkillZip(entry marketEntryIn, zipPath string) (*userSkillEntry, error) {
	skillID := sanitizeSkillID(entry.ID)
	userDir, err := a.userSkillsDir()
	if err != nil {
		return nil, err
	}
	dst := filepath.Join(userDir, skillID)
	bak := ""
	if dirExists(dst) { // 更新 = 旧版挪走暂存，装失败还原
		bak = filepath.Join(userDir, "."+skillID+".bak")
		_ = os.RemoveAll(bak)
		if err := os.Rename(dst, bak); err != nil {
			return nil, fmt.Errorf("挪开旧版本失败: %w", err)
		}
	}
	restore := func() {
		if bak != "" {
			_ = os.RemoveAll(dst)
			_ = os.Rename(bak, dst)
		}
	}
	root, tmp, err := extractSkillZip(zipPath)
	if err != nil {
		restore()
		return nil, err
	}
	defer os.RemoveAll(tmp)
	// addUserSkill 按目录名定技能 id：先包一层以条目 id 命名的目录
	wrap, err := os.MkdirTemp("", "jxmks-")
	if err != nil {
		restore()
		return nil, err
	}
	defer os.RemoveAll(wrap)
	if err := copyDir(root, filepath.Join(wrap, skillID)); err != nil {
		restore()
		return nil, err
	}
	ent, err := a.addUserSkill(filepath.Join(wrap, skillID))
	if err != nil {
		restore()
		return nil, err
	}
	_ = os.RemoveAll(bak)
	return ent, nil
}

// installMarketComponent 把市场组件装进**用户组件区** 组件/user/<id>/ 并重扫；
// id 以组件自己的 manifest.id 为准（缺省用条目 id）。
// 两档分明：内置组件（组件/<id>）由程序携带、随静默更新热更新，市场不得覆盖
// ——同名一律拒绝，要停用去设置 → 组件 关开关；用户组件与主程序升级无关，
// 不进打包、不上 git、不参与热更新，装/卸/开关都在设置 → 组件 里。
func (a *App) installMarketComponent(e marketEntryIn, root string) (marketInstallRec, error) {
	id := ""
	var m ComponentManifest
	if data, err := os.ReadFile(filepath.Join(root, "manifest.json")); err == nil && json.Unmarshal(data, &m) == nil {
		id = strings.TrimSpace(m.ID)
	}
	if id == "" {
		id = e.ID
	}
	if strings.ContainsAny(id, `\/:*?"<>|`) || id == "." || id == ".." || id == userComponentsSubdir {
		return marketInstallRec{}, fmt.Errorf("组件 id 不能当目录名: %s", id)
	}
	if a.Components.IsBuiltin(id) {
		return marketInstallRec{}, fmt.Errorf("「%s」是内置组件（随包分发、随程序静默更新升级），不能从市场安装覆盖；不需要时在 设置 → 组件 里关掉开关", id)
	}
	// 先杀该组件的 worker：exe 形态的 worker 占着目录时 RemoveAll 直接失败，
	// py 形态会继续用旧代码服务到进程死亡——两种形态都先杀干净再换目录
	if a.Bridge != nil {
		a.Bridge.KillComponent(id)
	}
	dst := filepath.Join(a.Components.UserDir(), id)
	if err := os.RemoveAll(dst); err != nil {
		return marketInstallRec{}, err
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return marketInstallRec{}, err
	}
	if err := copyDir(root, dst); err != nil {
		return marketInstallRec{}, fmt.Errorf("复制组件文件失败: %w", err)
	}
	a.Components.Rescan()
	if _, ok := a.Components.Manifest(id); !ok {
		return marketInstallRec{}, fmt.Errorf("组件安装后未被识别（manifest.json 异常）")
	}
	return marketInstallRec{Version: e.Version, Key: id, InstalledAt: time.Now().Format(time.RFC3339)}, nil
}

// uninstallMarketComponent 卸载用户组件区里的组件；内置组件不动（市场装不了
// 也就无从卸起，兜底再拒一次）。
func (a *App) uninstallMarketComponent(e marketEntryIn, rec marketInstallRec) error {
	id := rec.Key
	if id == "" {
		id = e.ID
	}
	if a.Components.IsBuiltin(id) {
		return fmt.Errorf("「%s」是内置组件，不能卸载；不需要时在 设置 → 组件 里关掉开关", id)
	}
	if _, ok := a.Components.Manifest(id); !ok {
		return nil // 已不在（手动删过）：直接收尾
	}
	if a.Bridge != nil {
		a.Bridge.KillComponent(id)
	}
	if err := a.Components.Remove(id); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

/* ---------- HTTP ---------- */

func (a *App) handleMarketView(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("refresh") == "1" && strings.TrimSpace(a.Cfg.MarketURL) != "" {
		_, _ = a.fetchMarketManifest(strings.TrimSpace(a.Cfg.MarketURL), true) // 市场页「刷新」：绕过 60s 缓存
	}
	writeJSON(w, 200, a.buildMarketView())
}

func (a *App) handleMarketInstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !marketTypeOK(body.Type) || strings.TrimSpace(body.ID) == "" {
		writeErr(w, 400, "需要 type（skill/component/plugin）与 id")
		return
	}
	raw := strings.TrimSpace(a.Cfg.MarketURL)
	if raw == "" {
		writeErr(w, 400, "未配置市场服务器（设置 · 网络访问 · 市场服务器）")
		return
	}
	man, err := a.fetchMarketManifest(raw, true) // 安装前取最新清单
	if err != nil {
		writeErr(w, 502, "市场清单获取失败: "+err.Error())
		return
	}
	var entry marketEntryIn
	found := false
	for _, e := range marketEntriesOf(man, body.Type) {
		if e.ID == body.ID {
			entry, found = e, true
			break
		}
	}
	if !found {
		writeErr(w, 404, fmt.Sprintf("市场里没有 %s %s", body.Type, body.ID))
		return
	}
	zipURL, err := marketZipURL(raw, entry.Zip)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	zipPath, cleanup, err := a.marketDownloadZip(zipURL, entry)
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	defer cleanup()

	var rec marketInstallRec
	resp := map[string]any{"ok": 1, "type": body.Type, "id": entry.ID, "name": entry.Name, "version": entry.Version}
	switch body.Type {
	case "skill":
		ent, err := a.installMarketSkillZip(entry, zipPath)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		rec = marketInstallRec{Version: entry.Version, Key: ent.ID, InstalledAt: time.Now().Format(time.RFC3339)}
		resp["skill"] = ent
	case "component":
		tmp, err := os.MkdirTemp("", "jxmkc-")
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer os.RemoveAll(tmp)
		if err := unzip(zipPath, tmp); err != nil {
			writeErr(w, 400, "解压失败: "+err.Error())
			return
		}
		root, err := findRootWith(tmp, "manifest.json")
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		rec, err = a.installMarketComponent(entry, root)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		resp["components"] = a.Components.List()
	case "plugin":
		key, err := a.CCPlugins.ccInstallFromPath(zipPath, marketPluginHome) // 安装器直接吃 zip
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		rec = marketInstallRec{Version: entry.Version, Key: key, InstalledAt: time.Now().Format(time.RFC3339)}
		resp["plugin"] = key
	case "mcp":
		// 解包读 mcp.json → 按行业三形态解析 → 注册为独立连接器（来源标
		// market:<条目id>，卸载按来源整组注销）
		tmp, err := os.MkdirTemp("", "jxmkm-")
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		defer os.RemoveAll(tmp)
		if err := unzip(zipPath, tmp); err != nil {
			writeErr(w, 400, "解压失败: "+err.Error())
			return
		}
		root, err := findRootWith(tmp, "mcp.json")
		if err != nil {
			writeErr(w, 400, "包里缺少 mcp.json（不是有效的 MCP 连接器包）")
			return
		}
		data, err := os.ReadFile(filepath.Join(root, "mcp.json"))
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		defs, err := parseMCPServersDoc(data, entry.ID)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		names, err := a.MCPServers.UpsertFromDefs(defs, "market:"+entry.ID, entry.Name)
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		rec = marketInstallRec{Version: entry.Version, Key: strings.Join(names, ","), InstalledAt: time.Now().Format(time.RFC3339)}
		resp["servers"] = names
	}
	if err := a.saveMarketRec(body.Type, entry.ID, rec); err != nil {
		log.Printf("[市场] 安装记账落盘失败（安装本身已成功）: %v", err)
	}
	a.reportMarketInstall(raw, body.Type, entry) // 安装成功上报（失败静默，不影响安装结果）
	writeJSON(w, 200, resp)
}

// reportMarketInstall 向市场服务器上报一次成功安装（POST /api/market/report-install，
// 身份头带发布者姓名）——服务端记 install 明细，与管理页的下载/安装统计对上。
// 上报失败只落日志：明细是旁路数据，不拖垮安装主链路。
func (a *App) reportMarketInstall(marketRawURL, typ string, e marketEntryIn) {
	if a.marketUser() == "" {
		return // 没设发布者姓名：服务端记不到人，省一次空跑
	}
	origin, err := marketOrigin(marketRawURL)
	if err != nil {
		return
	}
	payload, _ := json.Marshal(map[string]string{"type": typ, "id": e.ID, "version": e.Version})
	req, err := a.marketRequest(context.Background(), http.MethodPost, origin+"/api/market/report-install", bytes.NewReader(payload))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	go func() {
		resp, err := marketHTTPClient.Do(req)
		if err != nil {
			log.Printf("[市场] 安装上报失败（忽略）: %v", err)
			return
		}
		resp.Body.Close()
	}()
}

func (a *App) handleMarketUninstall(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
		ID   string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !marketTypeOK(body.Type) || strings.TrimSpace(body.ID) == "" {
		writeErr(w, 400, "需要 type（skill/component/plugin）与 id")
		return
	}
	recs := a.loadMarketRecs()
	rec, hasRec := recs[body.Type+":"+body.ID]
	resp := map[string]any{"ok": 1}
	switch body.Type {
	case "skill":
		if err := a.deleteUserSkill(sanitizeSkillID(body.ID)); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	case "component":
		if !hasRec {
			writeErr(w, 400, "该组件不是市场安装的：内置组件只能开关（设置 → 组件），手动放入 组件/user/ 的组件在同一页删除")
			return
		}
		if err := a.uninstallMarketComponent(marketEntryIn{ID: body.ID}, rec); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		resp["components"] = a.Components.List()
	case "plugin":
		key := rec.Key
		if key == "" {
			key = body.ID + "@" + marketPluginHome
		}
		if err := a.CCPlugins.Uninstall(key); err != nil && !strings.Contains(err.Error(), "未安装") {
			writeErr(w, 400, err.Error())
			return
		}
	case "mcp":
		// 按来源整组注销（条目装进来的全部连接器一次撤下）
		names := a.MCPServers.RemoveBySource("market:" + body.ID)
		resp["removed"] = names
	}
	a.removeMarketRec(body.Type, body.ID)
	writeJSON(w, 200, resp)
}

/* ---------- 企业技能包 / 工作流自动更新 ---------- */

// marketAutoMu 自动更新防重入：手动「立即检查更新」与每小时轮询同拍触发时
// 共享一轮（TryLock 跳过），避免同一技能被并发下载安装。
var marketAutoMu sync.Mutex

// autoUpdateMarketSkills 企业技能包 / 工作流自动更新：checkUpdateOnce 每次检查客户端
// 更新时顺带跑（每小时轮询、手动「立即检查」同一条路）。对比市场最新清单与
// 安装记账（market_installed.json），已装且记账版本 ≠ 市场版本的技能自动下载
// 新版覆盖安装——与手动点「更新」同一条链路（sha256 校验、装失败还原旧版、
// 记账、上报服务端）。逐个更新、失败只记日志：单个技能失败不拖垮其他，下一
// 轮检查自然重试；本地自建技能没有市场记账，天然不被触碰；市场不可达（如
// 公司外用笔记本）也只是静默跳过，不影响客户端更新检查本身。
func (a *App) autoUpdateMarketSkills() {
	if !marketAutoMu.TryLock() {
		return
	}
	defer marketAutoMu.Unlock()
	raw := strings.TrimSpace(a.Cfg.MarketURL)
	if raw == "" {
		return
	}
	man, err := a.fetchMarketManifest(raw, true)
	if err != nil {
		log.Printf("[市场] 技能包更新检查失败（忽略，下轮再试）: %v", err)
		return
	}
	recs := a.loadMarketRecs()
	userDir, uerr := a.userSkillsDir()
	for _, e := range man.Skills {
		rec, hasRec := recs["skill:"+e.ID]
		if !hasRec || rec.Version == "" || rec.Version == e.Version {
			continue
		}
		if uerr != nil || !dirExists(filepath.Join(userDir, sanitizeSkillID(e.ID))) {
			continue // 记账在、目录没了（手动删过）：不自动重装，等用户自己决定
		}
		zipURL, err := marketZipURL(raw, e.Zip)
		if err != nil {
			log.Printf("[市场] 技能 %s 新版 %s 地址不合法（忽略）: %v", e.ID, e.Version, err)
			continue
		}
		zipPath, cleanup, err := a.marketDownloadZip(zipURL, e)
		if err != nil {
			log.Printf("[市场] 技能 %s 新版 %s 下载失败（忽略，下轮再试）: %v", e.ID, e.Version, err)
			continue
		}
		ent, err := a.installMarketSkillZip(e, zipPath)
		cleanup()
		if err != nil {
			log.Printf("[市场] 技能 %s 自动更新失败，已保留旧版 %s（下轮再试）: %v", e.ID, rec.Version, err)
			continue
		}
		if err := a.saveMarketRec("skill", e.ID, marketInstallRec{Version: e.Version, Key: ent.ID, InstalledAt: time.Now().Format(time.RFC3339)}); err != nil {
			log.Printf("[市场] 技能 %s 更新记账落盘失败（更新本身已成功）: %v", e.ID, err)
		}
		a.reportMarketInstall(raw, "skill", e)
		log.Printf("[市场] 技能 %s 已自动更新 %s → %s", e.Name, rec.Version, e.Version)
	}
}
