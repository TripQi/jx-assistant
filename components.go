package main

// 组件管理：本栈原生的轻量外围扩展（区别于 插件兼容层 的 DSH 适配）。
// 组件 = 组件/<名字>/ 下的 manifest.json + 纯浏览器半边 client.js（client.css 可选），
// 没有宿主半边、没有转译——由内嵌的 web/cx.js 运行时负责发现注入与热开关：
// 设置页切换开关时，前端即时加载/卸载组件（卸载依赖组件注册的 dispose），
// 不需要刷新页面，也不需要重启服务。

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 组件分两档归属（用户定调），走同一个注册表、同一个组件桥、同一个设置页：
//   - 内置组件：组件/<id>/，随包分发、**不可删除只可开关**、随静默更新的
//     组件分块热更新（update_blocks.go）；
//   - 用户组件：组件/user/<id>/，市场或手动安装、可开关可删除、**不进打包、
//     不上 git、不热更新**（更新走市场重装）。
// 与技能同一套约定（技能/user/ 放用户技能），打包与静默更新的脚本侧同步
// 排除 user 目录。
const userComponentsSubdir = "user"

// componentEntry 是一条已发现组件：清单 + 本体目录 + 归属。
type componentEntry struct {
	Manifest ComponentManifest
	Dir      string
	BuiltIn  bool
}

// ComponentWorkerDef 是组件自包含后端的声明（manifest 顶层 "workers" 数组项）：
// 主程序的组件桥（componentbridge.go）按声明拉起子进程并把端点桥到它的
// stdin/stdout——后端逻辑零主程序代码，组件可免编译安装（市场 zip 直装）。
type ComponentWorkerDef struct {
	Name      string                    `json:"name"`    // worker 标识（同组件内唯一；端点按它归属进程）
	Command   []string                  `json:"command"` // argv 模板：{python}=共享便携运行时 {dir}=组件目录，其余原样
	Endpoints []ComponentWorkerEndpoint `json:"endpoints"`
	// Hooks 可选：订阅的主程序钩子事件（白名单见 componentbridge.go，如
	// prompt.submit/run.end）。声明了钩子的 worker 启用帧协议（stdin/stdout
	// 变成带 id 的 JSON 帧双向解复用），并因此能收到事件、回决定、反向调主程序
	// API；未声明维持哑管（一行一答），存量组件零影响。
	Hooks []string `json:"hooks,omitempty"`
	// HooksTimeoutMs 可选：拦截型钩子等 worker 决定的上限（毫秒）。缺省 5000，
	// 上限 30000——超时按放行处理（fail-open），组件卡死不拖慢发送。
	HooksTimeoutMs int `json:"hooksTimeoutMs,omitempty"`
}

// ComponentWorkerEndpoint 声明一条对外端点（挂在 /api/c/<组件id>/<path>）。
type ComponentWorkerEndpoint struct {
	Method string `json:"method"` // GET|POST
	Path   string `json:"path"`   // 组件内相对路径（不含 /api/c/<id>/ 前缀）
	Type   string `json:"type"`   // call=HTTP 一行一答哑管；ws=WebSocket 双向哑转发
}

// ComponentManifest 是 组件/<name>/manifest.json 的结构。
type ComponentManifest struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Client      struct {
		JS  string `json:"js"`
		CSS string `json:"css"`
	} `json:"client"`
	// Workers 可选：自包含后端（C 形态组件）。不用 "host" 字段名——市场服务端
	// 旧校验把 host 解析为 bool，嵌套对象会被误判非法 JSON；未知字段 workers
	// 静默放行，新组件包无需服务端先升级即可发布。
	Workers []ComponentWorkerDef `json:"workers,omitempty"`
	// Capabilities 可选：组件声明「可分发能力」——模型经统一入口 component_call
	// 调用的那批能力（格式与扩展方式见 组件/README.md「可分发能力」一节，
	// 分发逻辑见 componentcall.go）。声明是**白名单**：没声明的能力宿主不给调；
	// 声明了但宿主没注册实现的能力同样不可调（报错指路）。
	// 不占工具目录：能力增删只改这里 + 环境快照的「组件能力」块，工具定义不变。
	Capabilities []ComponentCapability `json:"capabilities,omitempty"`
}

// ComponentCapability 是 manifest 顶层 capabilities 数组的一项：一条模型可调用的
// 组件能力声明。
//   - Name：能力名（组件内唯一，component_call 的 capability 参数）
//   - Description：一句中文说明（进环境快照的「组件能力」块，模型据此挑能力）
//   - Parameters：参数 JSON Schema（进环境快照的能力行说明与调用校验；缺省＝
//     无参数。刻意保持原始 JSON——组件的参数长什么样由组件自己决定，宿主不建模）
//   - Modes：可用模式白名单（空＝全部模式）。沿用原专用工具的门控口径：只读
//     答疑（aux）与长期助理（assistant）通常不声明
//   - Writes：是否会产生写入副作用（true 的能力在计划模式下暂停，与 write/edit
//     同理；模型看得见、调用时被拦）
type ComponentCapability struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Modes       []string        `json:"modes,omitempty"`
	Writes      bool            `json:"writes,omitempty"`
}

// allowedIn 该能力在某会话模式下是否可见可调（Modes 为空＝全部模式）。
func (c ComponentCapability) allowedIn(mode string) bool {
	if len(c.Modes) == 0 {
		return true
	}
	for _, m := range c.Modes {
		if m == mode {
			return true
		}
	}
	return false
}

// ComponentStatus 是设置页展示的一条组件记录。
// Enabled＝调用方视角的生效值；带会话查清单时（?s=<会话id>）为「会话覆盖
// 优先于全局」的结果，GlobalEnabled 恒为全局开关值、Override 标记该会话
// 是否显式覆盖过（按对话开关浮窗据此显示覆盖徽标）。
// BuiltIn 标记归属：设置页据此分「内置组件 / 用户组件」两区，内置区不给
// 删除入口（只有开关）。
type ComponentStatus struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description"`
	Enabled     bool   `json:"enabled"`
	BuiltIn     bool   `json:"builtin"`
	HasJS       bool   `json:"hasJs"`
	HasCSS      bool   `json:"hasCss"`
	GlobalEnabled bool `json:"globalEnabled,omitempty"`
	Override    bool   `json:"override,omitempty"`
}

// ComponentRegistry 持有扫描结果与开关状态；全部方法并发安全。
type ComponentRegistry struct {
	mu        sync.Mutex
	dir       string // 内置组件根：组件/
	statePath string // data/components.json
	items     map[string]componentEntry
	enabled   map[string]bool
	scanMu    sync.Mutex // 扫描串行化（设置页显式重扫与市场装卸的 Rescan 可能并发）
}

// InitComponents 扫描 组件/（内置）与 组件/user/（用户）并读开关状态；目录
// 不存在按空处理，绝不阻断启动。
//
// 发现时机（用户定调）：**平时不自动重扫**，清单接口是纯读；重扫只挂在「打开
// 设置页 → 组件页」这一次——前端带 rescan=1 让服务端 Rescan 一遍。这样运行中
// 放进/删掉的组件目录打开设置页即见（不必重启服务：桌面版单实例常驻，重开
// 窗口≠重启服务，正是 user-memory 曾「重启后仍不见」的根因），而平时零扫描
// 开销。市场装卸与静默更新组件块应用后另有显式 Rescan。
func InitComponents(dir, statePath string) *ComponentRegistry {
	r := &ComponentRegistry{dir: dir, statePath: statePath, items: map[string]componentEntry{}, enabled: map[string]bool{}}
	r.rescan()
	return r
}

func (r *ComponentRegistry) rescan() {
	items := map[string]componentEntry{}
	// 两档归属同一个命名空间（组件 id 即菜单键）：内置先扫、用户后扫，
	// 同名即忽略用户那份——内置组件由程序持有，用户侧不能顶替它。
	scanRoot := func(root string, builtIn bool) {
		entries, err := os.ReadDir(root)
		if err != nil {
			return
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			name := e.Name()
			if builtIn && name == userComponentsSubdir {
				continue // 用户组件区单独扫
			}
			cdir := filepath.Join(root, name)
			data, err := os.ReadFile(filepath.Join(cdir, "manifest.json"))
			if err != nil {
				continue
			}
			var m ComponentManifest
			if err := json.Unmarshal(data, &m); err != nil {
				log.Printf("[组件] %s/manifest.json 解析失败，已跳过: %v", cdir, err)
				continue
			}
			if m.ID == "" {
				m.ID = name
			}
			if m.Name == "" {
				m.Name = name
			}
			if prev, ok := items[m.ID]; ok {
				log.Printf("[组件] %s 与已发现的 %s（%s）同名，已忽略；内置优先", cdir, m.ID, prev.Dir)
				continue
			}
			items[m.ID] = componentEntry{Manifest: m, Dir: cdir, BuiltIn: builtIn}
		}
	}
	scanRoot(r.dir, true)
	scanRoot(filepath.Join(r.dir, userComponentsSubdir), false)
	enabled := map[string]bool{}
	if data, err := os.ReadFile(r.statePath); err == nil {
		_ = json.Unmarshal(data, &enabled) // 损坏按空处理（全部缺省启用）
	}
	// 残留清理：组件目录已删除（手动删/卸载）的开关态一并剔除并回写——
	// 否则重装同名组件时旧记录会把「缺省启用」短路成停用（实测踩过：
	// input-counter 删目录重装后默认关）。清掉后重装即视为新组件。
	pruned := false
	for id := range enabled {
		if _, ok := items[id]; !ok {
			delete(enabled, id)
			pruned = true
			log.Printf("[组件] 清理残留开关态：%s（组件目录已不存在）", id)
		}
	}
	r.mu.Lock()
	r.items, r.enabled = items, enabled
	r.mu.Unlock()
	if pruned {
		if data, err := json.MarshalIndent(enabled, "", "  "); err == nil {
			_ = os.MkdirAll(filepath.Dir(r.statePath), 0755)
			_ = os.WriteFile(r.statePath, data, 0644)
		}
	}
}

// IsEnabled 组件的全局生效值（未记录=缺省启用）。
func (r *ComponentRegistry) IsEnabled(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.isEnabledLocked(id)
}

func (r *ComponentRegistry) isEnabledLocked(id string) bool {
	if _, ok := r.items[id]; !ok {
		return false
	}
	on, rec := r.enabled[id]
	return !rec || on // 未记录的缺省启用
}

// HookSub 是一个钩子订阅：启用中组件的某个 worker 声明了该事件。
type HookSub struct {
	CompID string
	Def    ComponentWorkerDef
}

// HookSubscribers 返回订阅了某事件的全部启用组件 worker（组件 id 升序——拦截型
// 决定的应用顺序因此稳定）。事件是否在白名单由发射方（componentbridge.go）把关，
// 这里只做声明匹配。
func (r *ComponentRegistry) HookSubscribers(event string) []HookSub {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []HookSub{}
	for _, id := range ids {
		if !r.isEnabledLocked(id) {
			continue
		}
		for _, def := range r.items[id].Manifest.Workers {
			for _, h := range def.Hooks {
				if h == event {
					out = append(out, HookSub{CompID: id, Def: def})
					break
				}
			}
		}
	}
	return out
}

func (r *ComponentRegistry) Manifest(id string) (ComponentManifest, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.items[id]
	return e.Manifest, ok
}

// Capabilities 返回某组件声明的可分发能力（manifest 的 capabilities；没声明为空）。
func (r *ComponentRegistry) Capabilities(id string) []ComponentCapability {
	m, ok := r.Manifest(id)
	if !ok {
		return nil
	}
	return m.Capabilities
}

// Capability 按名字取一条能力声明（白名单查询：声明了才有）。
func (r *ComponentRegistry) Capability(id, name string) (ComponentCapability, bool) {
	for _, c := range r.Capabilities(id) {
		if c.Name == name {
			return c, true
		}
	}
	return ComponentCapability{}, false
}

// CompDir 返回某个组件本体的绝对可用目录（内置=组件/<id>，用户=组件/user/<id>）。
// 组件桥拉起 worker 时把 cwd 与相对脚本路径锚在这里。
func (r *ComponentRegistry) CompDir(id string) (string, bool) {
	r.mu.Lock()
	e, ok := r.items[id]
	r.mu.Unlock()
	if !ok {
		return "", false
	}
	dir := e.Dir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	return dir, true
}

// IsBuiltin 判断组件归属：内置＝随包分发（不可删除、随静默更新热更新）。
func (r *ComponentRegistry) IsBuiltin(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.items[id]
	return ok && e.BuiltIn
}

// List 按 id 排序返回全部组件（内置与用户同列，BuiltIn 供设置页分区）。
func (r *ComponentRegistry) List() []ComponentStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	ids := make([]string, 0, len(r.items))
	for id := range r.items {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	out := []ComponentStatus{}
	for _, id := range ids {
		e := r.items[id]
		on, rec := r.enabled[id]
		out = append(out, ComponentStatus{
			ID: id, Name: e.Manifest.Name, Version: e.Manifest.Version, Description: e.Manifest.Description,
			Enabled: !rec || on,
			BuiltIn: e.BuiltIn,
			HasJS:   e.Manifest.Client.JS != "", HasCSS: e.Manifest.Client.CSS != "",
		})
	}
	return out
}

// SetEnabled 切换开关并落盘（热生效由前端运行时即时应用，服务端仅记状态）。
func (r *ComponentRegistry) SetEnabled(id string, on bool) error {
	if _, ok := r.Manifest(id); !ok {
		return os.ErrNotExist
	}
	r.mu.Lock()
	r.enabled[id] = on
	r.mu.Unlock()
	data, err := json.MarshalIndent(r.enabled, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0755); err != nil {
		return err
	}
	return os.WriteFile(r.statePath, data, 0644)
}

// Remove 卸载一个**用户组件**（删目录 + 清开关态 + 重扫）；内置组件拒绝删除
// ——它们随包分发、只可开关（调用方负责先杀 worker）。
func (r *ComponentRegistry) Remove(id string) error {
	r.mu.Lock()
	e, ok := r.items[id]
	r.mu.Unlock()
	if !ok {
		return os.ErrNotExist
	}
	if e.BuiltIn {
		return fmt.Errorf("内置组件（随包分发）不可删除，不需要时关掉开关即可")
	}
	if err := os.RemoveAll(e.Dir); err != nil {
		return err
	}
	r.mu.Lock()
	delete(r.items, id)
	delete(r.enabled, id)
	state := map[string]bool{}
	for k, v := range r.enabled {
		state[k] = v
	}
	r.mu.Unlock()
	if data, err := json.MarshalIndent(state, "", "  "); err == nil {
		_ = os.MkdirAll(filepath.Dir(r.statePath), 0755)
		_ = os.WriteFile(r.statePath, data, 0644)
	}
	r.Rescan()
	return nil
}

// BuiltinDir 返回内置组件根目录（组件/）：资产兜底、静默更新分块落位用。
func (r *ComponentRegistry) BuiltinDir() string { return r.dir }

// UserDir 返回用户组件安装区（组件/user/）：市场安装、手动放入都落这里，
// 不进打包、不上 git、不参与静默热更新。
func (r *ComponentRegistry) UserDir() string { return filepath.Join(r.dir, userComponentsSubdir) }

// Rescan 重扫组件目录（市场安装/卸载后调用：新组件即时装载、删除的组件即时效）。
func (r *ComponentRegistry) Rescan() {
	r.scanMu.Lock()
	defer r.scanMu.Unlock()
	r.rescan()
}

// ---------- HTTP 处理器 ----------

func (a *App) handleListComponents(w http.ResponseWriter, r *http.Request) {
	// 纯读清单；重扫只在显式要求时做——前端打开设置页的「组件」页时带
	// rescan=1 触发一次（组件目录可以在运行中增删，单实例常驻不该靠重启服务
	// 才看见新组件；但平时不扫，零开销）。
	if q := r.URL.Query().Get("rescan"); q == "1" || q == "true" {
		a.Components.Rescan()
	}
	// ?s=<会话id>：返回该会话的生效视图（会话覆盖优先于全局开关），并带
	// sessionCustom 标记（本会话是否定制过）与 cfg（会话视角的组件配置合并层：
	// 总开关 + 每组件覆盖）——前端 cx.js 一次拉齐、装配前就位，按对话开关
	// 浮窗也照这份渲染。
	sid := strings.TrimSpace(r.URL.Query().Get("s"))
	if sid != "" {
		writeJSON(w, 200, map[string]any{
			"components":    a.cxoComponentList(sid),
			"sessionCustom": a.SessionCX.custom(sid),
			"cfg":           a.cxoComponentCfgView(sid),
		})
		return
	}
	writeJSON(w, 200, map[string]any{"components": a.Components.List()})
}

func (a *App) handleSetComponent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
		Session string `json:"session"` // 会话id：带它＝只改本会话的覆盖，全局开关不动
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeErr(w, 400, "需要 id 与 enabled")
		return
	}
	if _, ok := a.Components.Manifest(body.ID); !ok {
		writeErr(w, 404, "组件不存在")
		return
	}
	// 会话级覆盖：优先于全局开关，全局开关本身不动；worker 进程只在
	// 「全局关了且再没有任何会话要它」时才收（别的地方还在用）。
	if sid := strings.TrimSpace(body.Session); sid != "" {
		if _, err := a.Store.Get(sid); err != nil {
			writeErr(w, 404, "会话不存在")
			return
		}
		g := a.Components.IsEnabled(body.ID)
		a.cxoSetComponent(sid, body.ID, body.Enabled)
		if g && !a.cxoComponentLive(body.ID) {
			a.Bridge.KillComponent(body.ID)
		}
		// 语音组件：本会话把它打开而全局没开时，独立语音输入法立即跟上
		// （幂等；全局开过它本就在跑）。用户点了开就该能用，不等懒启。
		if body.ID == voiceComponentID && body.Enabled && !g {
			go a.spawnVoiceType()
		}
		writeJSON(w, 200, map[string]any{
			"ok": 1, "components": a.cxoComponentList(sid),
			"sessionCustom": a.SessionCX.custom(sid),
		})
		return
	}
	if err := a.Components.SetEnabled(body.ID, body.Enabled); err != nil {
		writeErr(w, 404, "组件不存在")
		return
	}
	// 组件关闭即杀它声明的全部 worker 进程（热卸载做真；重开会懒启复活）。
	// 全局关了但还有会话显式要它时不杀——那几个对话的组件仍在工作。
	if !body.Enabled && !a.cxoComponentLive(body.ID) {
		a.Bridge.KillComponent(body.ID)
	}
	// 语音组件：关闭时连独立语音输入法一起收掉（见 Bridge.onKill）；
	// 重新启用时立刻把它拉回来（懒启对"进程"没意义——用户点了开就该能用）。
	if body.ID == voiceComponentID && body.Enabled {
		go a.spawnVoiceType()
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "components": a.Components.List()})
}

// handleComponentAsset 提供组件的浏览器半边资产（仅平级文件，防目录穿越）。
// 路径 /api/components/{id}/{file}：挂 /api/ 下让远程中继白名单天然覆盖
// （前端经 fetch 拿文本注入，本地直连、远程走隧道，同一条加载路径）。
// ?s=<会话id> 时按会话生效集放行（会话把全局关着的组件打开，资产要能拿到）。
func (a *App) handleComponentAsset(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/components/")
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
	enabled := a.Components.IsEnabled(id)
	if sid := strings.TrimSpace(r.URL.Query().Get("s")); sid != "" {
		enabled = a.cxoComponentEnabled(sid, id)
	}
	if _, ok := a.Components.Manifest(id); !ok || !enabled {
		http.NotFound(w, r)
		return
	}
	dir, ok := a.Components.CompDir(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, filepath.Join(dir, file))
}

// handleDeleteComponent 卸载一个用户组件（POST /api/components/delete）。
// 内置组件不在这里：它们随包分发、只可开关，删除入口对内置区一律拒绝。
func (a *App) handleDeleteComponent(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.ID) == "" {
		writeErr(w, 400, "需要 id")
		return
	}
	id := strings.TrimSpace(body.ID)
	if _, ok := a.Components.Manifest(id); !ok {
		writeErr(w, 404, "组件不存在")
		return
	}
	if a.Components.IsBuiltin(id) {
		writeErr(w, 400, "内置组件随包分发、不可删除；不需要时关掉开关即可")
		return
	}
	// 先杀 worker：exe 形态占着目录时 RemoveAll 会失败；py 形态会继续用旧代码服务
	if a.Bridge != nil {
		a.Bridge.KillComponent(id)
	}
	if err := a.Components.Remove(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.removeMarketRec("component", id) // 市场记账一并清掉，市场里不再显示「已安装」
	writeJSON(w, 200, map[string]any{"ok": 1, "components": a.Components.List()})
}
