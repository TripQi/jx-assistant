// 分块静默更新：发布源 release 附件里带 blocks.json 时，不再走「下载完整
// 安装包手动装」的整包路，而是按块（engine=主程序 exe、components/<id>=组件
// 目录）下载校验后静默应用，用户无感知。
//
//	engine：Windows 允许改名正在运行的 exe（但不能删/覆盖）。趁旧进程还活着
//	  做两次原子改名：旧 exe → xxx.exe.old、新 exe → xxx.exe。此后磁盘上的
//	  正牌文件已是新版，内存里仍跑旧版——用户下次自然重启即生效，无需任何
//	  常驻助手。新版启动时自清 .old（FinalizeEngineSwap）。想立即生效走
//	  /api/update/restart：临时生成 PowerShell 助手逃出 kill-on-close Job
//	  （processguard 加了 BREAKAWAY_OK，仅此一处用），等旧进程退出后拉起新 exe。
//	组件：整目录替换（先杀 worker，修市场链路同款缺口），Rescan 后前端
//	  CX.refresh() 热生效，不打扰用户。
//
// blocks.json 是「快照式清单」：记录截至本次发布每块应是哪个版本（不是变更
// 单）。落后多版的客户端一步对齐到每块最新——V3 只改主程序时清单里 voice 仍
// 写 V2 发的版本，V1 用户一次拿到 engine@V3 + voice@V2，V2 用户只下主程序。
//
// 兼容：发布源没有 blocks.json 附件时完全不进本文件的路，整包横幅行为不变；
// 任何分块失败记 LastError 并回退整包横幅。完整安装包始终是并行存在的兜底路
// （设置页「下载完整安装包」）。
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// updateBlocksFile 分块清单在 release 附件里的固定名字。
const updateBlocksFile = "blocks.json"

// updateBlocks 分块清单（快照式：每块当前应是什么版本）。
type updateBlocks struct {
	Engine     *updateBlock           `json:"engine,omitempty"`
	Components map[string]updateBlock `json:"components,omitempty"`
	// Runtime 共享便携 Python 运行时（技能\_runtime 整目录替换）；
	// Skills 内置技能包（白名单内技能整目录替换）。两者都只动技能区里
	// 写死的路径，用户技能（技能\user）永不参与——见 update_runtime.go。
	Runtime *updateBlock `json:"runtime,omitempty"`
	Skills  *updateBlock `json:"skills,omitempty"`
	// Builtin 内置组件（Cua 驱动 / Windows-MCP 依赖）：{app}\内置组件 整目录替换。
	// 与 components 不同——那些是程序内的默认组件，这块只服务内置 MCP 连接器。
	Builtin *updateBlock `json:"builtin,omitempty"`
}

// updateBlock 单个块：file 是 release 附件名（按名在 assets 里查下载直链）。
type updateBlock struct {
	Version string `json:"version"`
	File    string `json:"file"`
	SHA256  string `json:"sha256,omitempty"`
}

// updateBlockState 分块更新的落盘记账（data/updates/state.json）。
type updateBlockState struct {
	EnginePending string            `json:"enginePending,omitempty"` // 已交换待重启生效的 engine 版本
	EngineArgv    []string          `json:"engineArgv,omitempty"`    // 交换时的启动参数（立即重启时沿用）
	SwappedAt     string            `json:"swappedAt,omitempty"`
	Applied       map[string]string `json:"applied,omitempty"` // 已应用的组件块：id → 版本
	Runtime       string            `json:"runtime,omitempty"` // 已应用的运行时块版本（py3.14.8）
	Skills        string            `json:"skills,omitempty"`  // 已应用的内置技能包版本
	Builtin       string            `json:"builtin,omitempty"` // 已应用的内置组件版本（Cua / Windows-MCP 依赖）
}

var (
	updateApplyMu  sync.Mutex // 串行化分块应用（轮询与手动检查可能同拍触发）
	updateApplying bool
	// 大块下载（voice 模型 zip 几百兆）：30 分钟上限、1GB 封顶
	updateBlockDownload = newCompanyHTTPClient(30 * time.Minute)
)

func (a *App) updatesStatePath() string {
	return filepath.Join(a.Cfg.DataDir, "updates", "state.json")
}

func (a *App) loadUpdateBlockState() updateBlockState {
	var st updateBlockState
	if data, err := os.ReadFile(a.updatesStatePath()); err == nil {
		_ = json.Unmarshal(data, &st)
	}
	return st
}

func (a *App) saveUpdateBlockState(st updateBlockState) error {
	out, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(a.updatesStatePath()), 0755); err != nil {
		return err
	}
	return writeFileAtomic(a.updatesStatePath(), out)
}

// currentExePath 本程序 exe 真身路径（解析软链；安装版锚它做原地交换）。
func currentExePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if rp, err := filepath.EvalSymlinks(p); err == nil {
		return rp
	}
	if ap, err := filepath.Abs(p); err == nil {
		return ap
	}
	return p
}

// runningFromOldExe 判断当前进程是不是从交换残名（xxx.exe.old 或换名让路产生的
// xxx.exe.old-<unix>）启动的——这是「内存版本旧于磁盘正位文件」唯一合法场景；
// 否则说明交换被外部覆盖，pending 已悬空（FinalizeEngineSwap 据此清账）。
func runningFromOldExe(exePath string) bool {
	base := filepath.Base(exePath)
	if i := strings.Index(base, ".old"); i > 0 {
		rest := base[i+len(".old"):]
		if rest == "" || (strings.HasPrefix(rest, "-") && strings.Trim(rest[1:], "0123456789") == "") {
			return true
		}
	}
	return false
}

// findReleaseAssetURL 按附件名找下载直链（找不到返回空串）。
func findReleaseAssetURL(assets []releaseAsset, name, sourceURL string) string {
	for _, a := range assets {
		if a.Name == name {
			return publicizeURL(a.BrowserDownloadURL, sourceURL)
		}
	}
	return ""
}

// fetchUpdateBlocks 下载并解析分块清单；附件里没有 blocks.json 返回 nil（调用
// 方据此走原整包路），下载/解析失败记日志并同样返回 nil（回退整包横幅）。
func fetchUpdateBlocks(rel releaseInfo, sourceURL string) *updateBlocks {
	url := findReleaseAssetURL(rel.Assets, updateBlocksFile, sourceURL)
	if url == "" {
		return nil
	}
	client := newCompanyHTTPClient(20 * time.Second)
	resp, err := client.Get(url)
	if err != nil {
		log.Printf("[更新] 分块清单下载失败（回退整包路）: %v", err)
		return nil
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[更新] 分块清单返回 HTTP %d（回退整包路）", resp.StatusCode)
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("[更新] 分块清单读取失败: %v", err)
		return nil
	}
	var b updateBlocks
	if err := json.Unmarshal(raw, &b); err != nil {
		log.Printf("[更新] 分块清单不是合法 JSON: %v", err)
		return nil
	}
	return &b
}

// downloadUpdateFile 下载一个更新块 zip 到临时文件并校验 sha256（空 sha 跳过
// 校验——自家服务端发布时都算）；调用方用 cleanup 清理。
func downloadUpdateFile(url, sha string) (zipPath string, cleanup func(), err error) {
	resp, err := updateBlockDownload.Get(url)
	if err != nil {
		return "", nil, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return "", nil, fmt.Errorf("下载失败: HTTP %d", resp.StatusCode)
	}
	tmp, err := os.MkdirTemp("", "jxupd-")
	if err != nil {
		return "", nil, err
	}
	fail := func(e error) (string, func(), error) {
		os.RemoveAll(tmp)
		return "", nil, e
	}
	zipPath = filepath.Join(tmp, "block.zip")
	f, err := os.Create(zipPath)
	if err != nil {
		return fail(err)
	}
	ok := false
	defer func() {
		if !ok {
			f.Close()
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, 1<<30)); err != nil {
		f.Close()
		return fail(fmt.Errorf("下载写入失败: %w", err))
	}
	f.Close()
	if sha != "" && !strings.EqualFold(hex.EncodeToString(h.Sum(nil)), sha) {
		return fail(fmt.Errorf("包校验失败（sha256 不符，下载可能不完整或被篡改）"))
	}
	ok = true
	return zipPath, func() { os.RemoveAll(tmp) }, nil
}

// applyBlocksPlan 应用分块更新计划（调用方已确认 silent 模式）。engine 与各
// 组件互相独立：单块失败记LastError继续其余；engine 失败补发 update_available
// 让整包横幅兜底。
func (a *App) applyBlocksPlan(blocks *updateBlocks, rel releaseInfo, sourceURL string) {
	if updateApplying {
		return // 上一轮还在下载大块：跳过本轮（下个轮询周期再看）
	}
	updateApplyMu.Lock()
	updateApplying = true
	defer func() { updateApplyMu.Unlock(); updateApplying = false }()

	// 技能区两块先行：runtime（共享运行时）与 skills（内置技能）。
	// 只动 <技能库>\_runtime 与白名单内置技能目录，用户技能永不参与。
	a.applyRuntimeBlock(blocks.Runtime, rel, sourceURL)
	a.applySkillsBlock(blocks.Skills, rel, sourceURL)
	// 内置组件块：操作电脑的二进制（Cua / Windows-MCP 依赖）
	a.applyBuiltinBlock(blocks.Builtin, rel, sourceURL)
	// 组件块：各自独立，失败不影响 engine
	if len(blocks.Components) > 0 {
		a.applyComponentBlocks(blocks.Components, rel, sourceURL)
	}
	// engine 块：二开版本绝不替换本程序 exe（其余块照旧处理）
	if e := blocks.Engine; e != nil && !SelfUpdateDisabled && e.Version != "" && versionCompare(e.Version, appVersion) > 0 {
		st := a.loadUpdateBlockState()
		if st.EnginePending != "" && versionCompare(e.Version, st.EnginePending) <= 0 {
			a.syncPendingToState() // 已交换过（等重启），只补横幅状态
			return
		}
		if err := a.applyEngineBlock(*e, rel, sourceURL); err != nil {
			log.Printf("[更新] 主程序分块更新失败（回退整包横幅）: %v", err)
			setUpdateError("主程序静默更新失败: " + err.Error())
			a.broadcast(Event{Type: "update_available", Title: e.Version})
			return
		}
		log.Printf("[更新] 主程序已静默更新到 %s（当前进程仍为 %s，重启后生效）", e.Version, appVersion)
		a.syncPendingToState()
		a.broadcast(Event{Type: "update_ready", Title: e.Version})
	}
}

// applyEngineBlock 下载 engine zip → 校验 → 解出 exe 暂存 exe.new → 原地交换。
func (a *App) applyEngineBlock(b updateBlock, rel releaseInfo, sourceURL string) error {
	if SelfUpdateDisabled {
		// 二开版本的最后一道闸：任何路径走到「换掉本程序」都在这里被拒
		return fmt.Errorf("%s", selfUpdateBlockedMsg)
	}
	url := findReleaseAssetURL(rel.Assets, b.File, sourceURL)
	if url == "" {
		return fmt.Errorf("附件里找不到 %s", b.File)
	}
	zipPath, cleanup, err := downloadUpdateFile(url, b.SHA256)
	if err != nil {
		return err
	}
	defer cleanup()
	tmp, err := os.MkdirTemp("", "jxexe-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := unzip(zipPath, tmp); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	exeSrc, err := findEngineExe(tmp)
	if err != nil {
		return err
	}
	exePath := currentExePath()
	if exePath == "" {
		return fmt.Errorf("取不到本程序路径")
	}
	// 暂存到 exe 旁（保证同卷可改名）：.new 在交换前只是普通文件，随时可清理
	staged := exePath + ".new"
	if err := copyFileAtomic(staged, exeSrc); err != nil {
		return fmt.Errorf("暂存新程序失败: %w", err)
	}
	// 原地交换：旧 → .old（运行中可改名），.new → 正名。第二步失败回滚第一步。
	old := exePath + ".old"
	if err := os.Remove(old); err != nil && !os.IsNotExist(err) {
		// 上一个 .old 还被占（古董实例仍从它跑着）：换名让路，不挡本次交换
		old = fmt.Sprintf("%s.old-%d", exePath, time.Now().Unix())
	}
	if err := os.Rename(exePath, old); err != nil {
		return fmt.Errorf("旧程序改名失败: %w", err)
	}
	if err := os.Rename(staged, exePath); err != nil {
		_ = os.Rename(old, exePath)
		return fmt.Errorf("新程序就位失败: %w", err)
	}
	st := a.loadUpdateBlockState()
	st.EnginePending = b.Version
	st.EngineArgv = os.Args
	st.SwappedAt = time.Now().Format(time.RFC3339)
	return a.saveUpdateBlockState(st)
}

// findEngineExe 在解压目录里找唯一的一个 .exe（发布脚本保证 zip 内只有一个）。
func findEngineExe(dir string) (string, error) {
	var found []string
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() && strings.EqualFold(filepath.Ext(p), ".exe") {
			found = append(found, p)
		}
		return nil
	})
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("engine 包里没有 exe")
	default:
		return "", fmt.Errorf("engine 包里有多个 exe（应有且仅有一个）")
	}
}

// FinalizeEngineSwap 新版启动时结算上一次 engine 交换：确认版本、清残留、同步
// 横幅状态。幂等，任何失败不阻断启动（下次启动再来一遍）。
func (a *App) FinalizeEngineSwap() {
	exePath := currentExePath()
	if exePath != "" {
		_ = os.Remove(exePath + ".new") // 上次下载了但没换成的残留
	}
	st := a.loadUpdateBlockState()
	if st.EnginePending != "" {
		switch {
		case st.EnginePending == appVersion:
			log.Printf("[更新] 静默更新已生效：当前版本 %s（%s 完成交换）", appVersion, st.SwappedAt)
		case versionCompare(st.EnginePending, appVersion) < 0:
			// 本机已比 pending 新（完整包覆盖安装等）：清账即可
		case runningFromOldExe(exePath):
			// 磁盘已换、内存仍旧（从 .old 等异常路径启动）：保留 pending，
			// 重启后自然生效
			a.syncPendingToState()
			return
		default:
			// 正位 exe 在跑却比 pending 旧：交换后被外部覆盖了（典型：静默换好
			// 新版后又跑了旧版完整安装包，2026-10-15 案例——安装包盖掉已换新的
			// exe，pending 却还在，横幅永远提示重启到新版本、点了也只会拉起旧
			// 版）。清掉悬空 pending 并立即重新检查更新，让静默更新重新换一次。
			log.Printf("[更新] 检测到待生效版本 %s 的交换已被覆盖（当前正位程序为 %s），清除悬空状态并重新检查更新", st.EnginePending, appVersion)
			if url := a.Cfg.UpdateURL; url != "" && url != "off" {
				go a.checkUpdateOnce(url)
			}
		}
		st.EnginePending = ""
		st.EngineArgv = nil
		st.SwappedAt = ""
		_ = a.saveUpdateBlockState(st)
	}
	if exePath != "" {
		_ = os.Remove(exePath + ".old") // 旧进程已不在，此刻删得动
		// .old-<unix> 是交换时 .old 被仍在运行的旧实例占着、换名让路产生的
		// 残留（连发两版静默更新必产生一个），此前自清只认精确的 .old，
		// 带时间戳的一直没人清（实测 20261014-1 换 -2 后留下 .old-1791972527）。
		// 按前缀一并扫掉；仍被古董实例占用的删不动就留着，下次启动再试。
		if dir := filepath.Dir(exePath); dir != "" {
			prefix := filepath.Base(exePath) + ".old"
			if entries, err := os.ReadDir(dir); err == nil {
				for _, e := range entries {
					if !e.IsDir() && strings.HasPrefix(e.Name(), prefix) {
						_ = os.Remove(filepath.Join(dir, e.Name()))
					}
				}
			}
		}
	}
	a.syncPendingToState()
}

// syncPendingToState 把落盘 pending 同步进 /api/status 的横幅状态。
func (a *App) syncPendingToState() {
	st := a.loadUpdateBlockState()
	pending := st.EnginePending != "" && versionCompare(st.EnginePending, appVersion) > 0
	updateMu.Lock()
	updateState.PendingRestart = pending
	if pending {
		updateState.PendingVersion = st.EnginePending
	} else {
		updateState.PendingVersion = ""
	}
	updateState.Applied = st.Applied
	updateMu.Unlock()
}

// planComponentBlocks 算需要静默更新的组件块（id 升序）。两类都要：
//   - 本机已装且版本落后的：升级（本地比清单新则不动，用户手动装的更新版不回退）；
//   - 本机没有的：补装。清单带块＝发布版本应持有该组件——与安装包 stage\组件
//     整目录随包装回是同一语义。2026-11-06 客户事故的教训：内置功能组件化后
//     （skill-market / sites-platform 的入口全靠组件注入），老客户升级 engine
//     却拿不到新组件目录，入口随升级消失、连市场入口也没了（想再装组件得先有
//     市场入口，死锁）。想彻底不要某组件用**开关关闭**——关掉不丢数据、记账
//     保持关、补装也不会把开关打开；手动删目录只在两次组件发版之间有效
//     （发新版清单带块就会装回，与安装包升级同款行为）。
func (a *App) planComponentBlocks(blocks map[string]updateBlock) []string {
	ids := make([]string, 0, len(blocks))
	for id, b := range blocks {
		if b.Version == "" {
			continue // 防呆：没版本的块不碰
		}
		// 用户组件（组件/user/）不参与热更新：它由用户从市场/发行渠道自己
		// 装、自己卸，版本归用户；清单里的同名条目即使有块也不去覆盖它。
		if _, ok := a.Components.Manifest(id); ok && !a.Components.IsBuiltin(id) {
			log.Printf("[更新] 组件 %s 在本机是用户组件（组件/user/），不参与内置热更新", id)
			continue
		}
		if m, ok := a.Components.Manifest(id); ok && versionCompare(b.Version, m.Version) <= 0 {
			continue // 本地已是清单版本或更新
		}
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// applyComponentBlocks 逐个应用组件块：成功后广播 components_updated 让前端
// CX.refresh() 热生效。
func (a *App) applyComponentBlocks(blocks map[string]updateBlock, rel releaseInfo, sourceURL string) {
	ids := a.planComponentBlocks(blocks)
	applied := []string{}
	for _, id := range ids {
		b := blocks[id]
		m, _ := a.Components.Manifest(id)
		if err := a.applyComponentBlock(id, b, rel, sourceURL); err != nil {
			log.Printf("[更新] 组件 %s 分块更新失败: %v", id, err)
			setUpdateError("组件 " + id + " 静默更新失败: " + err.Error())
			continue
		}
		log.Printf("[更新] 组件 %s 已静默更新 %s → %s", id, m.Version, b.Version)
		applied = append(applied, id+"→"+b.Version)
	}
	if len(applied) == 0 {
		return
	}
	// 记账：state.Applied 供状态页展示（只记真正换成功的）；市场记账同步版本
	// （有记录才更新，不给内置组件凭空造「市场可卸载」资格）
	st := a.loadUpdateBlockState()
	if st.Applied == nil {
		st.Applied = map[string]string{}
	}
	appliedIDs := make([]string, 0, len(applied))
	for _, item := range applied {
		if i := strings.Index(item, "→"); i > 0 {
			appliedIDs = append(appliedIDs, item[:i])
			st.Applied[item[:i]] = blocks[item[:i]].Version
		}
	}
	_ = a.saveUpdateBlockState(st)
	recs := a.loadMarketRecs()
	for _, id := range appliedIDs {
		if _, has := recs["component:"+id]; has {
			_ = a.saveMarketRec("component", id, marketInstallRec{
				Version: blocks[id].Version, Key: id,
				InstalledAt: time.Now().Format(time.RFC3339),
			})
		}
	}
	a.broadcast(Event{Type: "components_updated", Title: strings.Join(applied, "、")})
}

// applyComponentBlock 下载组件 zip → 校验 → 解压找根 → 杀 worker → 整目录替换
// → Rescan。
func (a *App) applyComponentBlock(id string, b updateBlock, rel releaseInfo, sourceURL string) error {
	if strings.ContainsAny(id, `\/:*?"<>|`) || id == "." || id == ".." || id == userComponentsSubdir {
		return fmt.Errorf("组件 id 不能当目录名: %s", id)
	}
	url := findReleaseAssetURL(rel.Assets, b.File, sourceURL)
	if url == "" {
		return fmt.Errorf("附件里找不到 %s", b.File)
	}
	zipPath, cleanup, err := downloadUpdateFile(url, b.SHA256)
	if err != nil {
		return err
	}
	defer cleanup()
	tmp, err := os.MkdirTemp("", "jxcomp-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := unzip(zipPath, tmp); err != nil {
		return fmt.Errorf("解压失败: %w", err)
	}
	root, err := findRootWith(tmp, "manifest.json")
	if err != nil {
		return err
	}
	var m ComponentManifest
	if data, err := os.ReadFile(filepath.Join(root, "manifest.json")); err != nil || json.Unmarshal(data, &m) != nil {
		return fmt.Errorf("包里没有合法的 manifest.json")
	}
	if strings.TrimSpace(m.ID) != id {
		return fmt.Errorf("包内组件 id（%s）与清单条目（%s）不一致", m.ID, id)
	}
	// 先杀 worker：exe 形态的 worker 占着目录时删不掉，py 形态会继续用旧代码服务
	if a.Bridge != nil {
		a.Bridge.KillComponent(id)
	}
	dst := filepath.Join(a.Components.BuiltinDir(), id)
	clearReadonlyTree(dst)
	aside := ""
	if err := os.RemoveAll(dst); err != nil {
		// 删不掉（voice 的 voicetype.exe 这类常驻 exe 还占着目录、文件带只读位、
		// 杀软锁着）：改名让路，先让新版本落位，别让整块更新永远卡死。Windows 的
		// 目录改名只动目录项，里面有打开的句柄也能成（runtime 块用的是同一招）。
		aside = fmt.Sprintf("%s.old-%d", dst, time.Now().Unix())
		if rerr := os.Rename(dst, aside); rerr != nil {
			return fmt.Errorf("旧组件目录既删不掉也挪不开（多半有进程占用）: %v", err)
		}
		log.Printf("[更新] 组件 %s 旧目录删不掉（%v），已改名让路到 %s，稍后清理", id, err, aside)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return err
	}
	if err := copyDir(root, dst); err != nil {
		if aside != "" {
			_ = os.RemoveAll(dst)
			_ = os.Rename(aside, dst) // 回滚：老版本重新就位，别让组件凭空消失
		}
		return fmt.Errorf("复制组件文件失败: %w", err)
	}
	if aside != "" {
		_ = os.RemoveAll(aside) // 仍被占用就留着，下次更新或启动时再清
	}
	a.Components.Rescan()
	if _, ok := a.Components.Manifest(id); !ok {
		return fmt.Errorf("组件更新后未被识别（manifest.json 异常）")
	}
	return nil
}

// clearReadonlyTree 递归去掉只读位：Windows 上带只读属性的文件删不掉，
// os.RemoveAll 会整块失败（同步工具、解压器、杀软都可能给组件文件打上只读）。
func clearReadonlyTree(dir string) {
	_ = filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		if info.Mode().Perm()&0200 == 0 {
			_ = os.Chmod(p, 0644)
		}
		return nil
	})
}

/* ---------- HTTP：手动入口 ---------- */

// handleUpdateCheck POST /api/update/check：立即触发一次更新检查（不等每小时
// 轮询）。检查在后台跑（含大块下载），响应只是「已开始」；结果经事件流与
// /api/status 到达。
func (a *App) handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	if SelfUpdateDisabled {
		// 二开版本：手动「立即检查更新」也不联网，直接说明已禁用
		writeErr(w, http.StatusBadRequest, selfUpdateBlockedMsg)
		return
	}
	url := strings.TrimSpace(a.Cfg.UpdateURL)
	if url == "" || url == "off" {
		writeErr(w, http.StatusBadRequest, "更新检查已关闭（更新检查源为 off）")
		return
	}
	go a.checkUpdateOnce(url)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "checking": true})
}

// handleUpdateRestart POST /api/update/restart：engine 已交换待生效时，拉起
// 重启助手并整体退出（助手等本进程退出后按原启动参数拉起新 exe）。
func (a *App) handleUpdateRestart(w http.ResponseWriter, r *http.Request) {
	if SelfUpdateDisabled {
		// 二开版本：没有「待重启生效的更新」这种事，重启接力一并关闭
		writeErr(w, http.StatusBadRequest, selfUpdateBlockedMsg)
		return
	}
	st := a.loadUpdateBlockState()
	if st.EnginePending == "" || versionCompare(st.EnginePending, appVersion) <= 0 {
		writeErr(w, http.StatusBadRequest, "当前没有待重启生效的更新")
		return
	}
	if err := spawnRestartHelper(a, st); err != nil {
		writeErr(w, http.StatusInternalServerError, "重启助手启动失败: "+err.Error())
		return
	}
	log.Printf("[更新] 立即重启以应用 %s（旧进程即将退出，助手接力）", st.EnginePending)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	// 给响应留出送达时间再退（桌面窗口关闭同款路径：会话在 SQLite，孤儿有补扫兜底）
	go func() {
		time.Sleep(800 * time.Millisecond)
		os.Exit(0)
	}()
}
