package main

// 独立 MCP 服务器注册表：不装整个 Claude Code 插件，直接添加一个 MCP
// 服务（stdio 命令或 http/sse 地址），像技能一样开关管理。协议客户端在
// mcpclient.go（与插件桥共用实现，宿主用 mcpPlainHost：仅 ${环境变量} 展开）。
// 加载机制对齐用户技能——清单先行、内容按需：环境快照只列连接器
// （PromptBlock），模型按需 mcp_read_connector 看工具清单、mcp_call_tool
// 执行；两个工具常驻目录且定义恒定，连接器增删不扰动工具目录与前缀缓存
// （此前把每个工具以 mcp__<服务器>__<工具> 全量注册，目录随连接器膨胀、
// 缓存命中大幅下降，且工具过多误导模型——已废弃）。
// 进程同样按需（懒启动）：引擎启动期不拉起任何连接器——启动零 MCP 进程，
// 首次调用（mcp_read_connector / mcp_call_tool 的调用自愈）或用户显式动作
// （设置页测试 / 开关启用 / 安装）才建立连接；连接建立后常驻复用，死亡
// 自愈重启。清单只来自状态文件，与连接状态无关（启动自检契约不受影响）。
// 信任边界：显式添加并启用 = 放行该服务器的工具调用，不再逐工具审批。
// 状态落 <dataDir>/mcp-servers.json；安装器（mcpcatalog.go）拉取的桥代码
// 放 <dataDir>/mcp-servers/<id>/，每桥独立 venv，互不污染。设置页有独立
// 的「MCP 连接器」分区（mcpservers_api.go）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// mcpServerNameRe 服务器名的合法域：字母开头，仅字母数字连字符。
// 不含下划线，避免与插件路径的 mcp__<插件>_<服务器>__<工具> 撞名。
var mcpServerNameRe = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9-]{0,63}$`)

// mcpServerEntry 是 mcp-servers.json 里的一条服务器定义（自描述，含开关）。
type mcpServerEntry struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Enabled     bool              `json:"enabled"`
	Source      string            `json:"source,omitempty"` // manual | catalog:<id> | builtin:<id>
	// RequiresCap 需要的会话能力（内置连接器用，如 computer）：能力没开时
	// 既不进环境快照的清单（软性），调用也会被 requireCapability 拦下（硬性）。
	RequiresCap string            `json:"requiresCap,omitempty"`
	AddedAt     string            `json:"addedAt,omitempty"`
	Type        string            `json:"type,omitempty"` // ""（stdio）| http | sse
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
}

// def 转成 mcpClient 用的服务器定义。
func (e *mcpServerEntry) def() mcpServerDef {
	return mcpServerDef{
		Type: e.Type, Command: e.Command, Args: e.Args,
		Env: e.Env, URL: e.URL, Headers: e.Headers,
	}
}

// mcpServersFile 是 mcp-servers.json 的整体结构。
type mcpServersFile struct {
	Version int               `json:"version"`
	Servers []*mcpServerEntry `json:"servers"`
}

// mcpServerStatus 是设置页/API 的一条服务器记录（含连通状态）。
type mcpServerStatus struct {
	Name        string            `json:"name"`
	Description string            `json:"description,omitempty"`
	Enabled     bool              `json:"enabled"`
	Type        string            `json:"type"` // stdio | http | sse
	Command     string            `json:"command,omitempty"`
	Args        []string          `json:"args,omitempty"`
	Env         map[string]string `json:"env,omitempty"`
	URL         string            `json:"url,omitempty"`
	Headers     map[string]string `json:"headers,omitempty"`
	Connected   bool              `json:"connected"`
	ToolCount   int               `json:"toolCount"`
	Tools       []string          `json:"tools,omitempty"`
	LastError   string            `json:"lastError,omitempty"`
	Source      string            `json:"source,omitempty"`
	AddedAt     string            `json:"addedAt,omitempty"`
}

// MCPServersRegistry 持有独立服务器清单与活动连接；全部方法并发安全。
type MCPServersRegistry struct {
	mu        sync.Mutex
	app       *App
	root      string // <dataDir>/mcp-servers（绝对路径）
	statePath string
	file      mcpServersFile
	clients   map[string]*mcpClient // 已建连接（键：服务器名）
	lastErr   map[string]string     // 最近一次启动/调用失败原因

	installMu sync.Mutex // 目录安装器串行化（同一时间只装一个桥）
}

// InitMCPServers 读状态文件构建清单（不连接——连接按需：模型调用自愈或
// 用户显式测试/启用触发，引擎启动期零 MCP 进程）。
func InitMCPServers(app *App, dataDir string) *MCPServersRegistry {
	root, err := filepath.Abs(filepath.Join(dataDir, "mcp-servers"))
	if err != nil {
		root = filepath.Join(dataDir, "mcp-servers")
	}
	r := &MCPServersRegistry{
		app:       app,
		root:      root,
		statePath: filepath.Join(dataDir, "mcp-servers.json"),
		file:      mcpServersFile{Version: 1},
		clients:   map[string]*mcpClient{},
		lastErr:   map[string]string{},
	}
	// root 是连接器进程的工作目录（mcpPlainHost.dir，见 connectOne）：
	// 不先建出来，cmd.Dir 指向不存在的路径，启动会报 "The directory name is invalid"。
	// 干净机器上必然踩到，所以初始化时就建。
	if err := os.MkdirAll(root, 0755); err != nil {
		log.Printf("[MCP] 创建连接器工作目录失败: %v", err)
	}
	if data, err := os.ReadFile(r.statePath); err == nil {
		_ = jsonUnmarshalLoose(data, &r.file)
	}
	n := 0
	for _, s := range r.file.Servers {
		if s.Enabled {
			n++
		}
	}
	log.Printf("[MCP] 独立服务器 %d 个（启用 %d）", len(r.file.Servers), n)
	return r
}

// jsonUnmarshalLoose JSON 解析的薄封装（与其它状态文件同样的损坏按空处理策略）。
func jsonUnmarshalLoose(data []byte, v any) error { return json.Unmarshal(data, v) }

// save 落盘状态（调用方持锁）。
func (r *MCPServersRegistry) save() error {
	data, err := json.MarshalIndent(&r.file, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(r.statePath), 0755); err != nil {
		return err
	}
	return os.WriteFile(r.statePath, data, 0644)
}

// Root 返回 mcp-servers 数据根（安装器放代码的位置）。
func (r *MCPServersRegistry) Root() string { return r.root }

// validate 校验并规范化一条定义（name 域、传输形态、必填项）。
func (e *mcpServerEntry) validate() error {
	e.Name = strings.TrimSpace(e.Name)
	e.Type = strings.TrimSpace(strings.ToLower(e.Type))
	e.Command = strings.TrimSpace(e.Command)
	e.URL = strings.TrimSpace(e.URL)
	if !mcpServerNameRe.MatchString(e.Name) {
		return fmt.Errorf("服务器名须以字母开头、仅含字母数字连字符（≤64）：%q", e.Name)
	}
	switch e.Type {
	case "", "stdio":
		e.Type = ""
		if e.Command == "" {
			return fmt.Errorf("stdio 服务器需要 command（启动命令）")
		}
	case "http", "sse":
		if !strings.HasPrefix(e.URL, "http://") && !strings.HasPrefix(e.URL, "https://") {
			return fmt.Errorf("%s 服务器需要 http(s):// 的 url", e.Type)
		}
	default:
		return fmt.Errorf("未知传输类型 %q（stdio/http/sse）", e.Type)
	}
	return nil
}

// HasName 报告注册表里是否已有同名服务器（内置连接器释放时判重，避免覆盖
// 用户改过的定义或把用户删掉的再塞回来）。
func (r *MCPServersRegistry) HasName(name string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.file.Servers {
		if s.Name == name {
			return true
		}
	}
	return false
}

// requiresCapOf 取一条服务器的能力门控（空 = 不门控）。
func (r *MCPServersRegistry) requiresCapOf(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.entryOf(name); ok {
		return e.RequiresCap
	}
	return ""
}

// addQuiet 登记一条服务器定义并落盘，但不建立连接。
// 与 Upsert 的差别只有这一点：内置连接器在启动期释放，必须保持
// 「引擎启动期零 MCP 进程」（见 InitMCPServers 注释），连接留到首次调用时懒启动。
func (r *MCPServersRegistry) addQuiet(e mcpServerEntry) error {
	if err := e.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	for _, s := range r.file.Servers {
		if s.Name == e.Name {
			r.mu.Unlock()
			return nil // 已存在，保持用户现状
		}
	}
	if e.AddedAt == "" {
		e.AddedAt = time.Now().Format(time.RFC3339)
	}
	r.file.Servers = append(r.file.Servers, &e)
	err := r.save()
	r.mu.Unlock()
	return err
}

// Upsert 新增或更新一条服务器定义并重连。同名覆盖（AddedAt/Source 原值
// 优先保留），成功后立即后台连接，不阻塞返回。
func (r *MCPServersRegistry) Upsert(e mcpServerEntry) error {
	if err := e.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	for _, s := range r.file.Servers {
		if s.Name == e.Name {
			if e.AddedAt == "" {
				e.AddedAt = s.AddedAt
			}
			if e.Source == "" {
				e.Source = s.Source
			}
			break
		}
	}
	old := r.takeClientLocked(e.Name)
	out := r.file.Servers[:0]
	for _, s := range r.file.Servers {
		if s.Name != e.Name {
			out = append(out, s)
		}
	}
	if e.AddedAt == "" {
		e.AddedAt = time.Now().Format(time.RFC3339)
	}
	out = append(out, &e)
	r.file.Servers = out
	err := r.save()
	r.mu.Unlock()
	shutdownMCPClients(old) // 锁外关闭：旧握手可能仍在进行（shutdown 会先取消它）
	if err != nil {
		return err
	}
	if e.Enabled {
		r.connectAsync(e.Name)
	}
	return nil
}

// Remove 删除一条服务器定义（连接一并关闭）。
func (r *MCPServersRegistry) Remove(name string) error {
	r.mu.Lock()
	found := false
	out := r.file.Servers[:0]
	for _, s := range r.file.Servers {
		if s.Name == name {
			found = true
			continue
		}
		out = append(out, s)
	}
	r.file.Servers = out
	old := r.takeClientLocked(name)
	delete(r.lastErr, name)
	err := r.save()
	r.mu.Unlock()
	shutdownMCPClients(old)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("MCP 服务器 %s 不存在", name)
	}
	return nil
}

// SetEnabled 开关一条服务器并按需连接/断开（统一插件页总开关的落地）。
func (r *MCPServersRegistry) SetEnabled(name string, on bool) error {
	r.mu.Lock()
	var hit *mcpServerEntry
	for _, s := range r.file.Servers {
		if s.Name == name {
			s.Enabled = on
			hit = s
		}
	}
	var old *mcpClient
	if !on {
		old = r.takeClientLocked(name)
	}
	err := r.save()
	r.mu.Unlock()
	shutdownMCPClients(old)
	if err != nil {
		return err
	}
	if hit == nil {
		return fmt.Errorf("MCP 服务器 %s 不存在", name)
	}
	if on {
		r.connectAsync(name)
	}
	return nil
}

// takeClientLocked 摘除一条连接并返回它（调用方持锁）。真正的关闭必须在锁外做：
// c.mu 可能被进行中的握手长期占用（stdio 首帧 120s、HTTP 请求 120s），在 r.mu
// 里等它会连带卡死环境快照、设置页列表与所有连接器调用。
func (r *MCPServersRegistry) takeClientLocked(name string) *mcpClient {
	c, ok := r.clients[name]
	if !ok {
		return nil
	}
	delete(r.clients, name)
	return c
}

// shutdownMCPClients 在注册表锁外关闭摘下来的连接。shutdown 会先取消进行中的
// 握手（见 mcpclient.go），因此不会把锁卡顿变成同步长等。
func shutdownMCPClients(cs ...*mcpClient) {
	for _, c := range cs {
		if c != nil {
			c.shutdown()
		}
	}
}

// clientOf 取或建一条服务器的客户端（不启动）。
func (r *MCPServersRegistry) clientOf(e *mcpServerEntry) *mcpClient {
	if c, ok := r.clients[e.Name]; ok {
		return c
	}
	c := &mcpClient{fullName: e.Name, def: e.def(), host: mcpPlainHost{dir: r.root}}
	r.clients[e.Name] = c
	return c
}

// entryOf 按名取定义。
func (r *MCPServersRegistry) entryOf(name string) (*mcpServerEntry, bool) {
	for _, s := range r.file.Servers {
		if s.Name == name {
			return s, true
		}
	}
	return nil, false
}

// connectAsync 后台连接一条服务器，结果记 lastErr（不阻塞调用方）。
func (r *MCPServersRegistry) connectAsync(name string) {
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		c, err := r.connectOne(ctx, name)
		if !r.noteConnectResult(name, c, err) {
			return // 实例已摘除或已被同名新定义替换：迟到结果不得回填当前状态
		}
		if err != nil {
			log.Printf("[MCP] %s 启动失败: %v", name, err)
		} else {
			log.Printf("[MCP] %s 已连接", name)
		}
	}()
}

// noteConnectResult 记录一次连接结果，仅在名字当前仍对应同一个连接实例时生效：
// 同名替换后旧握手的迟到结果不得污染新实例的 lastErr，已摘除的连接器也不得
// 留下幽灵错误。名字存在不等于实例相等，所以按指针比。
func (r *MCPServersRegistry) noteConnectResult(name string, c *mcpClient, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if cur, ok := r.clients[name]; !ok || cur != c {
		return false
	}
	if err != nil {
		r.lastErr[name] = err.Error()
		return true
	}
	delete(r.lastErr, name)
	return true
}

// connectOne 建立连接并完成握手（锁外执行；重入安全）。返回本次使用的实例，
// 便于调用方按实例身份判定结果归属。
func (r *MCPServersRegistry) connectOne(ctx context.Context, name string) (*mcpClient, error) {
	r.mu.Lock()
	e, ok := r.entryOf(name)
	if !ok || !e.Enabled {
		r.mu.Unlock()
		return nil, fmt.Errorf("MCP 服务器 %s 不存在或未启用", name)
	}
	c := r.clientOf(e)
	r.mu.Unlock()
	if err := c.start(ctx); err != nil {
		return c, err
	}
	return c, nil
}

// ShutdownAll 关闭全部连接（进程退出前调用；顶层 kill-on-close Job Object
// 兜底清场，本方法目前无调用方，留作优雅退出的挂点）。
func (r *MCPServersRegistry) ShutdownAll() {
	r.mu.Lock()
	cs := make([]*mcpClient, 0, len(r.clients))
	for name := range r.clients {
		cs = append(cs, r.takeClientLocked(name))
	}
	r.mu.Unlock()
	shutdownMCPClients(cs...)
}

// Test 连通性测试：连上并拉工具清单，返回工具名与描述（已连接则直接回）。
func (r *MCPServersRegistry) Test(name string) ([]mcpToolDef, error) {
	r.mu.Lock()
	e, ok := r.entryOf(name)
	if !ok {
		r.mu.Unlock()
		return nil, fmt.Errorf("MCP 服务器 %s 不存在", name)
	}
	c := r.clientOf(e)
	r.mu.Unlock()
	// 状态走快照，不拿 c.mu：握手进行中也立即返回（真开始则交由 start 串行排队）。
	if up, _ := c.connSnapshot(); !up {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := c.start(ctx); err != nil {
			r.noteConnectResult(name, c, err)
			return nil, err
		}
		r.noteConnectResult(name, c, nil)
	}
	c.mu.Lock()
	tools := append([]mcpToolDef(nil), c.tools...)
	c.mu.Unlock()
	return tools, nil
}

// List 全部服务器的状态（按名排序，含连通信息）。
func (r *MCPServersRegistry) List() []mcpServerStatus {
	type row struct {
		st mcpServerStatus
		c  *mcpClient
	}
	r.mu.Lock()
	rows := make([]row, 0, len(r.file.Servers))
	for _, s := range r.file.Servers {
		st := mcpServerStatus{
			Name: s.Name, Description: s.Description, Enabled: s.Enabled,
			Type: s.Type, Command: s.Command, Args: s.Args, Env: s.Env, URL: s.URL, Headers: s.Headers,
			Source: s.Source, AddedAt: s.AddedAt, LastError: r.lastErr[s.Name],
		}
		if st.Type == "" {
			st.Type = "stdio"
		}
		rows = append(rows, row{st: st, c: r.clients[s.Name]})
	}
	r.mu.Unlock()
	// 连通信息读连接状态快照：不碰 c.mu，握手进行中也能立即返回（握手方可能
	// 正持它到自己的 deadline）。只报真实状态：握手未完成就是未连接、工具数 0。
	out := make([]mcpServerStatus, 0, len(rows))
	for _, it := range rows {
		st, c := it.st, it.c
		if c != nil {
			if up, tools := c.connSnapshot(); up {
				st.Connected = true
				st.ToolCount = len(tools)
				for _, t := range tools {
					st.Tools = append(st.Tools, t.Name)
				}
				st.LastError = ""
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// ---------- 技能式按需加载（对齐 skill_manager 的清单先行、内容按需） ----------
//
// 一个连接器可能带几十个工具，全部注册进工具目录会撑大目录、打断前缀
// 缓存，过多的工具定义也容易误导模型。机制与用户技能一致：
//   环境快照只列连接器（名字 + 描述 + 工具数） → 模型要用时先
//   mcp_read_connector(name) 看工具清单与参数 schema → 再用
//   mcp_call_tool(connector, tool, arguments) 执行。
// 两个工具常驻目录且定义恒定——连接器增删只影响环境快照（瞬态尾部），
// 工具目录形状不变，前缀缓存完全不受扰动（与 read_skill 同一纪律）。

// mcpConnectorBodyMax read_connector 返回正文的字符上限（超长截断提示）。
const mcpConnectorBodyMax = 8000

// PromptBlock 生成环境快照的连接器清单段（无条件注入，见 agent.go envPrompt）。
//
// sess 用于能力门控：内置连接器带 RequiresCap 且该能力没开时**整条不出现**——
// 模型看不到就不会去试（软性管控）；真正的拦截在 mcp_read_connector /
// mcp_call_tool 里走 requireCapability（硬性）。sess 为 nil 时按"没有任何能力"处理。
func (r *MCPServersRegistry) PromptBlock(sess *SessionMeta) string {
	type item struct {
		e *mcpServerEntry
		c *mcpClient
	}
	r.mu.Lock()
	items := make([]item, 0, len(r.file.Servers))
	for _, s := range r.file.Servers {
		if !s.Enabled {
			continue
		}
		if s.RequiresCap != "" && (sess == nil || !hasCapability(sess, s.RequiresCap)) {
			continue
		}
		items = append(items, item{e: s, c: r.clients[s.Name]})
	}
	r.mu.Unlock()
	// 工具数读连接状态快照（c.mu 可能被握手占用）；清单顺序仍按定义顺序。
	var b strings.Builder
	for _, it := range items {
		b.WriteString("  - " + it.e.Name)
		if it.e.Description != "" {
			b.WriteString("：" + it.e.Description)
		}
		if it.c != nil {
			if up, tools := it.c.connSnapshot(); up {
				b.WriteString(fmt.Sprintf("——%d 个工具", len(tools)))
			}
		}
		b.WriteString("\n")
	}
	if b.Len() == 0 {
		return ""
	}
	return "# MCP 连接器清单（要用某连接器的工具时，先调 mcp_read_connector 读它的工具清单与参数，再用 mcp_call_tool 执行；连接器平时不运行，首次调用会自动启动、约需数秒）\n" + b.String()
}

// ReadConnector 返回一个连接器的完整工具文档（名称/描述/参数 schema）；
// 未连接时顺带懒启动（等价于设置页的「测试」）。
func (r *MCPServersRegistry) ReadConnector(ctx context.Context, name string) (string, error) {
	r.mu.Lock()
	e, ok := r.entryOf(name)
	if !ok {
		r.mu.Unlock()
		return "", fmt.Errorf("MCP 连接器 %s 不存在（清单见环境快照）", name)
	}
	if !e.Enabled {
		r.mu.Unlock()
		return "", fmt.Errorf("MCP 连接器 %s 已停用（设置 → MCP 里可开启）", name)
	}
	c := r.clientOf(e)
	r.mu.Unlock()
	// 状态走快照，不碰 c.mu：握手进行中也立刻返回（真开始则交由 start 排队）。
	if up, _ := c.connSnapshot(); !up {
		if err := c.start(ctx); err != nil {
			r.noteConnectResult(name, c, err)
			return "", fmt.Errorf("连接器 %s 连接失败: %w", name, err)
		}
		r.noteConnectResult(name, c, nil)
	}
	c.mu.Lock()
	tools := append([]mcpToolDef(nil), c.tools...)
	c.mu.Unlock()
	var b strings.Builder
	fmt.Fprintf(&b, "# MCP 连接器 %s（%s）——%d 个工具\n", e.Name, e.Description, len(tools))
	b.WriteString("调用方式：mcp_call_tool(connector=\"" + e.Name + "\", tool=<工具名>, arguments=<按下方 schema 填的对象>)\n\n")
	for _, t := range tools {
		b.WriteString("## " + t.Name + "\n")
		if d := strings.TrimSpace(t.Description); d != "" {
			b.WriteString(d + "\n")
		}
		schema, _ := json.Marshal(t.InputSchema)
		if len(schema) == 0 || string(schema) == "null" {
			schema = []byte(`{"type":"object"}`)
		}
		b.WriteString("参数 schema：" + string(schema) + "\n\n")
	}
	if body := []rune(b.String()); len(body) > mcpConnectorBodyMax {
		b.Reset()
		b.WriteString(string(body[:mcpConnectorBodyMax]))
		b.WriteString("\n…（工具过多已截断：优先使用未截到的部分可再问，或让用户在设置里查看完整清单）")
	}
	return b.String(), nil
}

// CallToolByName 按连接器名 + 工具名执行（mcp_call_tool 的落地）。
func (r *MCPServersRegistry) CallToolByName(ctx context.Context, connector, tool string, args json.RawMessage) (string, []mcpImage, error) {
	r.mu.Lock()
	e, ok := r.entryOf(connector)
	if !ok {
		r.mu.Unlock()
		return "", nil, fmt.Errorf("MCP 连接器 %s 不存在（清单见环境快照）", connector)
	}
	if !e.Enabled {
		r.mu.Unlock()
		return "", nil, fmt.Errorf("MCP 连接器 %s 已停用", connector)
	}
	c := r.clientOf(e)
	r.mu.Unlock()
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return c.callTool(ctx, tool, args)
}

// ConnectorTools 返回两个常驻的按需工具（定义恒定，不随连接器增删变化）。
func (r *MCPServersRegistry) ConnectorTools() []Tool {
	if r == nil {
		return nil
	}
	return []Tool{
		{
			Spec: toolSpec("mcp_read_connector",
				"读取一个 MCP 连接器（设计软件/服务整合包）的工具清单与用法：每个工具的名称、说明、参数 schema。环境快照的「MCP 连接器清单」只列连接器名字与描述；要用某连接器的工具之前，先调用本工具看清单。连接器平时不运行，调用本工具会自动启动它（首次约需数秒）。只读模式下不可用（会拉起连接器进程）：被拒后可用 request_permission 申请切档。",
				objSchema(map[string]any{
					"name": strProp("连接器名，来自环境快照里的 MCP 连接器清单"),
				}, "name")),
			Run: func(t *toolContext, args json.RawMessage) (string, error) {
				var p struct {
					Name string `json:"name"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return "", err
				}
				if err := requireNotReadOnly(t, "读取 MCP 连接器清单（会拉起连接器进程）"); err != nil {
					return "", err
				}
				if t.app.MCPServers == nil {
					return "", fmt.Errorf("MCP 注册表未初始化")
				}
				// 内置连接器带能力门控（如「操作电脑」）：没开就走 requireCapability
				// 弹审批卡——与 browser_*/computer_* 同一套升级通道。
				if req := t.app.MCPServers.requiresCapOf(strings.TrimSpace(p.Name)); req != "" {
					if err := requireCapability(t, req, capZh(req), args); err != nil {
						return "", err
					}
				}
				return t.app.MCPServers.ReadConnector(t.ctx, strings.TrimSpace(p.Name))
			},
		},
		{
			Spec: toolSpec("mcp_call_tool",
				"执行一个 MCP 连接器里的工具。先用 mcp_read_connector 看过该连接器的工具清单与参数 schema，再按 schema 填 arguments 调用本工具。只读模式下不可用（MCP 工具会驱动外部软件读写文件）：被拒后可用 request_permission 申请切档。",
				objSchema(map[string]any{
					"connector": strProp("连接器名，来自环境快照里的 MCP 连接器清单"),
					"tool":      strProp("工具名，来自 mcp_read_connector 的清单"),
					"arguments": map[string]any{
						"type":                 "object",
						"description":          "工具参数对象，按 mcp_read_connector 给出的 schema 填写",
						"additionalProperties": true,
					},
				}, "connector", "tool")),
			Run: func(t *toolContext, args json.RawMessage) (string, error) {
				var p struct {
					Connector string          `json:"connector"`
					Tool      string          `json:"tool"`
					Arguments json.RawMessage `json:"arguments"`
				}
				if err := json.Unmarshal(args, &p); err != nil {
					return "", err
				}
				if err := requireNotReadOnly(t, "调用 MCP 连接器工具"); err != nil {
					return "", err
				}
				if t.app.MCPServers == nil {
					return "", fmt.Errorf("MCP 注册表未初始化")
				}
				// 同 mcp_read_connector：内置连接器受能力门控，没开就拦下并弹审批卡。
				// 读清单与真正执行都要拦——只拦前者的话，知道名字就能绕过。
				if req := t.app.MCPServers.requiresCapOf(strings.TrimSpace(p.Connector)); req != "" {
					if err := requireCapability(t, req, capZh(req), args); err != nil {
						return "", err
					}
				}
				text, images, err := t.app.MCPServers.CallToolByName(t.ctx, strings.TrimSpace(p.Connector), strings.TrimSpace(p.Tool), p.Arguments)
				if err != nil {
					return "", err
				}
				// 随结果回传的图片入库为附件（信封置顶，防长文本截断），与
				// read_image 同链路进入模型上下文与前端工具卡片。
				if note := mcpImagesEnvelope(t.app, images); note != "" {
					text = note + "\n" + text
				}
				return text, nil
			},
		},
	}
}

// Count 服务器条数。
func (r *MCPServersRegistry) Count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.file.Servers)
}

// mcpToolsMode 报告某会话模式是否带 MCP 按需工具（与 toolsFor 的分支对齐：
// 执行类模式有，总控 assistant 只编排不实操、aux 只读答疑，均不带）。
// 连接器清单段（PromptBlock 注入）用同一判定——清单与调用入口同模式集，
// 不给任何模式开「看得见调不着」的空头支票。
func mcpToolsMode(mode string) bool { return mode != "assistant" && mode != "aux" }

// hasEnabled 是否存在启用的连接器（启动自检的清单段契约用）。
func (r *MCPServersRegistry) hasEnabled() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.file.Servers {
		if s.Enabled {
			return true
		}
	}
	return false
}

// HasSource 是否存在来自指定来源（如 "market:<条目id>"）的连接器（市场
// 页的已装标记用）。
func (r *MCPServersRegistry) HasSource(source string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.file.Servers {
		if s.Source == source {
			return true
		}
	}
	return false
}

// RemoveBySource 按来源注销全部连接器（市场卸载用），返回删掉的名单。
func (r *MCPServersRegistry) RemoveBySource(source string) []string {
	r.mu.Lock()
	names := []string{}
	old := []*mcpClient{}
	out := r.file.Servers[:0]
	for _, s := range r.file.Servers {
		if s.Source == source {
			names = append(names, s.Name)
			old = append(old, r.takeClientLocked(s.Name))
			delete(r.lastErr, s.Name)
			continue
		}
		out = append(out, s)
	}
	r.file.Servers = out
	err := r.save()
	r.mu.Unlock()
	shutdownMCPClients(old...) // 锁外关闭：即使某个连接正在握手也不阻塞注册表
	if err != nil {
		log.Printf("[MCP] 市场卸载落盘失败（连接器已移除）: %v", err)
	}
	return names
}

// UpsertFromDefs 批量注册（市场安装/拖拽 json 导入用）：source 标来源，
// desc 为缺省描述；返回注册成功的服务器名与首个错误。
func (r *MCPServersRegistry) UpsertFromDefs(defs map[string]mcpServerDef, source, desc string) ([]string, error) {
	names := make([]string, 0, len(defs))
	for n := range defs {
		names = append(names, n)
	}
	sort.Strings(names)
	var firstErr error
	for _, n := range names {
		d := defs[n]
		e := mcpServerEntry{
			Name: n, Description: desc, Enabled: true, Source: source,
			Type: d.Type, Command: d.Command, Args: d.Args, Env: d.Env,
			URL: d.URL, Headers: d.Headers,
		}
		if err := r.Upsert(e); err != nil && firstErr == nil {
			firstErr = err
			continue
		}
	}
	if firstErr != nil && len(names) == 0 {
		return nil, firstErr
	}
	return names, firstErr
}

// parseMCPServersDoc 解析一份 MCP 配置 JSON，容忍三种行业常见形态：
// {"mcpServers":{"名":{...}}} 包裹层（Claude Desktop/Cursor/Codex 配置）、
// {"名":{...}} 扁平、单个服务器对象本体（此时用 fallbackName 当服务器名，
// 常取导入文件名）。每个服务器须为 stdio（command）或 http/sse（url）。
// 与服务端 readMCPPackage 同语义（两端各持一份最小实现）。
func parseMCPServersDoc(data []byte, fallbackName string) (map[string]mcpServerDef, error) {
	defs := map[string]mcpServerDef{}
	var wrapped struct {
		MCPServers map[string]mcpServerDef `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil && len(wrapped.MCPServers) > 0 {
		defs = wrapped.MCPServers
	} else if flat := map[string]mcpServerDef{}; json.Unmarshal(data, &flat) == nil && mcpDefsValid(flat) {
		defs = flat
	} else {
		var one mcpServerDef
		if err := json.Unmarshal(data, &one); err != nil || (strings.TrimSpace(one.Command) == "" && one.URL == "") {
			return nil, fmt.Errorf("不是可识别的 MCP 配置（支持 {\"mcpServers\":{...}}、{\"名\":{...}} 或单服务器对象三种形态）")
		}
		if !mcpServerNameRe.MatchString(fallbackName) {
			return nil, fmt.Errorf("单服务器形态需要合法服务器名（文件名 %q 不符：字母开头、仅字母数字连字符）", fallbackName)
		}
		defs = map[string]mcpServerDef{fallbackName: one}
	}
	for n, d := range defs {
		t := strings.ToLower(strings.TrimSpace(d.Type))
		if t == "http" || t == "sse" {
			if strings.TrimSpace(d.URL) == "" {
				return nil, fmt.Errorf("服务器 %q 声明为 %s 但缺少 url", n, t)
			}
		} else if strings.TrimSpace(d.Command) == "" {
			return nil, fmt.Errorf("服务器 %q 缺少 command（stdio 型必须给启动命令）", n)
		}
	}
	if len(defs) == 0 {
		return nil, fmt.Errorf("配置里没有服务器定义")
	}
	return defs, nil
}

// mcpDefsValid 报告扁平解析结果是否真像「名→定义」而不是把单服务器对象
// 的字段名当成了键（command/url 字段非空的定义才可信）。
func mcpDefsValid(flat map[string]mcpServerDef) bool {
	ok := 0
	for _, d := range flat {
		if strings.TrimSpace(d.Command) != "" || strings.TrimSpace(d.URL) != "" {
			ok++
		}
	}
	return ok > 0 && ok == len(flat)
}
