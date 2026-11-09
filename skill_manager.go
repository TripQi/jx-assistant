package main

// 用户技能管理：自带技能（pdf/ocr/office）只读展示、常开；
// 用户技能位于 技能/user/<目录名>/（必须含 SKILL.md），可添加/开关/删除。
// 开关状态存 data/skills.json。用户技能按「清单先行、内容按需」送达模型：
// 环境快照里只列 名字（id）与一句话描述（**所有模式一律注入**，见
// envPrompt 的固定条目与 assertSkillVisibility 自检），正文由模型按需调
// read_skill 读取；技能增减不改变工具定义与系统提示词的稳定前缀。

import (
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// 内置技能清单（顺序即设置页展示顺序）：随包分发、只读展示、常开、不可
// 删除。分块更新（skills 块）的白名单（isBuiltinSkillName）、设置页自带
// 技能清单（listSkills）、启动迁移（migrateBuiltinFromUser）共用这一份。
// 打包脚本 打包\发布静默更新.ps1 的 $builtin 与此保持一致，改动需双侧同步。
var builtinSkillOrder = []string{"pdf", "ocr", "office", "downloader", "apkinfo", "web-search-local"}

// 内置技能 id 集合（由 builtinSkillOrder 派生，查重 O(1)）。
var builtinSkillIDs = func() map[string]bool {
	m := make(map[string]bool, len(builtinSkillOrder))
	for _, id := range builtinSkillOrder {
		m[id] = true
	}
	return m
}()

// isBuiltinSkillName 判断一个目录名是不是「随包分发的内置技能」。
// 分块更新（skills 块）用它做白名单：只有名单内的目录允许整目录替换，
// user、_runtime 以及任何别的名字一律拒绝——用户技能永远不在热更范围内。
func isBuiltinSkillName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return false
	}
	return builtinSkillIDs[name]
}

const (
	userSkillBodyMax = 6000 // read_skill 返回的单个技能说明上限（字符）
	// userSkillDescPromptMax 环境快照里一句话描述的截断上限：快照每个会话都
	// 注入，超长描述会污染全部对话的上下文（行业规范本身也限 description ≤1024）
	userSkillDescPromptMax = 200
	// skillListingBudget 单个技能清单段的字符预算（字节口径，对标 ZCode skills.ts
	// 的 DEFAULT_SKILL_METADATA_BUDGET=20000：英文 20K 字符 ≈ 5K token，我们按
	// 字节算时中文 6.6K 字/英文 20K 字符 ≈ 同一 token 量级）。用户技能段与
	// 插件技能段各自计算，超预算即整段降级为仅「名字（id=…）」行——技能再
	// 多也不吃掉环境快照；名字+id 恒在，read_skill 按需取全文。
	skillListingBudget = 20000
)

// truncateRunesForPrompt 按 rune 数截断并加省略号（不破坏多字节字符）。
func truncateRunesForPrompt(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n]) + "…"
	}
	return s
}

// renderSkillListing 按预算渲染技能清单段：full 为逐条「名字（id=…）：描述」
// 行，namesOnly 为同序的仅名字行；整段超 skillListingBudget 时降级用后者
//（对齐 ZCode skills.ts 的两级形态：先拼完整版，超预算一次性降级，降级版
// 不再二次截断——名字行极短，条目数失控也膨胀有限）。
func renderSkillListing(header string, full, namesOnly []string) string {
	if len(full) == 0 {
		return ""
	}
	join := func(lines []string) string { return header + "\n" + strings.Join(lines, "\n") }
	if s := join(full); len(s) <= skillListingBudget {
		return s
	}
	return join(namesOnly)
}

type userSkillEntry struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName,omitempty"` // 显示名称（中文为主）；显示时 displayName → name → id 逐级回退
	Description string `json:"description"`
	Version     string `json:"version,omitempty"` // SKILL.md frontmatter / manifest.json 的版本（缺省空）
	Enabled     bool   `json:"enabled"`
	BuiltIn     bool   `json:"builtin"`
}

type skillsState struct {
	Disabled map[string]bool `json:"disabled"` // 用户技能默认开启，只记关闭项
}

var skillsStateMu sync.Mutex

func (a *App) skillsStatePath() string { return filepath.Join(a.Cfg.DataDir, "skills.json") }

func (a *App) loadSkillsState() skillsState {
	skillsStateMu.Lock()
	defer skillsStateMu.Unlock()
	st := skillsState{Disabled: map[string]bool{}}
	data, err := os.ReadFile(a.skillsStatePath())
	if err == nil {
		_ = json.Unmarshal(data, &st)
	}
	if st.Disabled == nil {
		st.Disabled = map[string]bool{}
	}
	return st
}

func (a *App) saveSkillsState(st skillsState) error {
	skillsStateMu.Lock()
	defer skillsStateMu.Unlock()
	data, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(a.skillsStatePath(), data, 0644)
}

// userSkillsDir 用户技能根目录：技能/user/。
func (a *App) userSkillsDir() (string, error) {
	dir, err := a.SkillsDir()
	if err != nil {
		return "", err
	}
	p := filepath.Join(dir, "user")
	if err := os.MkdirAll(p, 0755); err != nil {
		return "", err
	}
	return p, nil
}

// stripFrontmatter 去掉 SKILL.md 开头的 YAML frontmatter（--- ... ---）。
func stripFrontmatter(body string) string {
	s := strings.TrimSpace(body)
	if !strings.HasPrefix(s, "---") {
		return s
	}
	if i := strings.Index(s[3:], "\n---"); i >= 0 {
		return strings.TrimSpace(s[3+i+4:])
	}
	return s
}

// skillFM 是 SKILL.md 的解析结果（frontmatter 键 + 正文），清单/安装/发布共用。
// name 保持行业规范（Agent Skills：小写字母/数字/中划线）；displayName 是展示名
//（中文为主，没有就回退 name——VS Code 的 name/displayName 同款双名称）。
type skillFM struct {
	Name        string
	DisplayName string
	Description string
	Version     string
	Tags        []string
	Body        string // 去掉 frontmatter 后的正文
}

// skillDisplayOf 显示名的逐级回退：displayName → name → id。
func skillDisplayOf(display, name, id string) string {
	if s := strings.TrimSpace(display); s != "" {
		return s
	}
	if s := strings.TrimSpace(name); s != "" {
		return s
	}
	return id
}

// skillMeta 从目录的 SKILL.md 提取元信息（frontmatter 的 name / displayName /
// description / version / tags）；没有 frontmatter 时回退为目录名与正文首行。
func skillMeta(dir string) skillFM {
	var fm skillFM
	data, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		fm.Name = filepath.Base(dir)
		return fm
	}
	text := string(data)
	s := strings.TrimSpace(text)
	fm.Name = filepath.Base(dir)
	if strings.HasPrefix(s, "---") {
		if i := strings.Index(s[3:], "\n---"); i >= 0 {
			fm.Body = strings.TrimSpace(s[3+i+4:])
			for _, line := range strings.Split(s[3:3+i], "\n") {
				line = strings.TrimSpace(line)
				switch {
				case strings.HasPrefix(line, "name:"):
					fm.Name = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "name:")), `"'`)
				case strings.HasPrefix(line, "displayName:"):
					fm.DisplayName = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "displayName:")), `"'`)
				case strings.HasPrefix(line, "description:"):
					fm.Description = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "description:")), `"'`)
				case strings.HasPrefix(line, "version:"):
					fm.Version = strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "version:")), `"'`)
				}
			}
			return fm
		}
	}
	lines := strings.Split(text, "\n")
	for _, line := range lines {
		if t := strings.TrimSpace(line); t != "" {
			fm.Description = strings.TrimSpace(strings.TrimPrefix(t, "#"))
			break
		}
	}
	fm.Body = strings.TrimSpace(text)
	return fm
}

// SkillLists 是 GET /api/skills 的载荷：自带技能 + 用户技能。
type SkillLists struct {
	BuiltIn []userSkillEntry `json:"builtin"`
	User    []userSkillEntry `json:"user"`
}

// listSkills 汇总自带与用户技能清单。
func (a *App) listSkills() (*SkillLists, error) {
	out := &SkillLists{BuiltIn: []userSkillEntry{}, User: []userSkillEntry{}}
	dir, err := a.SkillsDir()
	if err != nil {
		return out, nil // 技能目录不可用不算错，返回空清单
	}
	st := a.loadSkillsState()
	// 自带技能：读各自 manifest
	for _, id := range builtinSkillOrder {
		m, _, err := a.readSkillManifest(id)
		if err != nil {
			continue
		}
		out.BuiltIn = append(out.BuiltIn, userSkillEntry{
			ID: m.ID, Name: m.Name, Description: m.Description, Enabled: true, BuiltIn: true,
		})
	}
	// 用户技能：扫描 技能/user/
	userDir := filepath.Join(dir, "user")
	entries, err := os.ReadDir(userDir)
	if err != nil {
		return out, nil
	}
	for _, e := range entries {
		if !e.IsDir() || builtinSkillIDs[e.Name()] {
			continue
		}
		skillDir := filepath.Join(userDir, e.Name())
		if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
			continue // 没有 SKILL.md 的目录不算技能
		}
		fm := skillMeta(skillDir)
		version := fm.Version
		if version == "" { // manifest.json（技能运行清单）里有版本时认它
			if m, _, err := a.readSkillManifest(filepath.Join("user", e.Name())); err == nil {
				version = m.Version
			}
		}
		out.User = append(out.User, userSkillEntry{
			ID: e.Name(), Name: fm.Name, DisplayName: fm.DisplayName,
			Description: fm.Description, Version: version,
			Enabled: !st.Disabled[e.Name()], BuiltIn: false,
		})
	}
	sort.Slice(out.User, func(i, j int) bool { return out.User[i].ID < out.User[j].ID })
	return out, nil
}

// skillAvailable 某个技能当前是否可用：内置技能在位即常开；用户技能要求
// 目录里有 SKILL.md 且未被停用。web_search 的兜底提示用它决定要不要指引
// 模型改走「本机网络搜索」技能——技能被删除或停用时提示随之消失，保证
// 摘除干净、不留悬空指引。
func (a *App) skillAvailable(id string) bool {
	list, err := a.listSkills()
	if err != nil {
		return false
	}
	if builtinSkillIDs[id] {
		for _, s := range list.BuiltIn {
			if s.ID == id {
				return true
			}
		}
		return false
	}
	for _, s := range list.User {
		if s.ID == id {
			return s.Enabled
		}
	}
	return false
}

// addUserSkill 安装一个用户技能到 技能/user/<id>/：srcPath 是本机技能目录
// （整目录复制）、ZIP 压缩包（解压后安装）或单个 .md 说明文件（包装成
// <文件名>/SKILL.md 再安装）。安装器本质只是帮用户把文件挪到位——手动把
// 目录拖进/拖出 技能/user/，清单扫描同样能发现。
func (a *App) addUserSkill(srcPath string) (*userSkillEntry, error) {
	src := strings.TrimSpace(srcPath)
	if src == "" {
		return nil, fmt.Errorf("路径不能为空")
	}
	info, err := os.Stat(src)
	if err != nil {
		return nil, fmt.Errorf("路径不存在: %s", src)
	}
	var root, idSrc string // root=待复制的技能根；idSrc=推导技能 id 的名字
	if info.IsDir() {
		if _, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil {
			return nil, fmt.Errorf("目录里缺少 SKILL.md（技能必须自带说明文件）")
		}
		root, idSrc = src, filepath.Base(src)
	} else {
		isZip := strings.EqualFold(filepath.Ext(src), ".zip")
		isMD := strings.EqualFold(filepath.Ext(src), ".md")
		if !isZip && !isMD {
			return nil, fmt.Errorf("只支持技能文件夹、.zip 压缩包或单个 .md 说明文件: %s", src)
		}
		var tmp string
		if isMD {
			root, tmp, err = wrapSkillMD(src)
		} else {
			root, tmp, err = extractSkillZip(src)
		}
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		if isMD || root == tmp {
			// 单 .md 包装 / SKILL.md 在压缩包根部：id 用文件名（临时目录名不能当 id）
			idSrc = strings.TrimSuffix(filepath.Base(src), filepath.Ext(src))
		} else {
			idSrc = filepath.Base(root) // 只包了一层目录：id 用内层目录名
		}
	}
	id := sanitizeSkillID(idSrc)
	if id == "" {
		return nil, fmt.Errorf("无法从名字推导技能 id，请用纯字母数字或中文命名文件夹/压缩包/.md 文件")
	}
	if builtinSkillIDs[id] {
		return nil, fmt.Errorf("%s 是自带技能，不能重名", id)
	}
	// 内容体检（与发布侧 checkSkillMDBody 同一份判定）：空 SKILL.md / 只有
	// frontmatter 的包装进来「列表没描述、read_skill 读不出」，在安装口拦下
	if raw, err := os.ReadFile(filepath.Join(root, "SKILL.md")); err != nil {
		return nil, fmt.Errorf("SKILL.md 读取失败: %w", err)
	} else if err := checkSkillMDBody("SKILL.md", raw); err != nil {
		return nil, err
	}
	userDir, err := a.userSkillsDir()
	if err != nil {
		return nil, err
	}
	dest := filepath.Join(userDir, id)
	if dirExists(dest) {
		return nil, fmt.Errorf("技能 %s 已存在", id)
	}
	if err := copyDir(root, dest); err != nil {
		return nil, fmt.Errorf("安装技能失败: %w", err)
	}
	fm := skillMeta(dest)
	return &userSkillEntry{
		ID: id, Name: fm.Name, DisplayName: fm.DisplayName,
		Description: fm.Description, Enabled: true,
	}, nil
}

// extractSkillZip 把技能 ZIP 解压到专用临时目录，返回 (技能根, 临时目录, 错误)。
// 技能根 = SKILL.md 所在目录：支持 SKILL.md 直接在压缩包根部，或只多包一层
// 目录（右键压缩文件夹的常见产物）两种打包方式。临时目录用完由调用方清理。
func extractSkillZip(zipPath string) (root, tmp string, err error) {
	tmp, err = os.MkdirTemp("", "jxskill-")
	if err != nil {
		return "", "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	if err := unzip(zipPath, tmp); err != nil {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("解压失败: %w", err)
	}
	if _, statErr := os.Stat(filepath.Join(tmp, "SKILL.md")); statErr == nil {
		return tmp, tmp, nil
	}
	entries, readErr := os.ReadDir(tmp)
	if readErr != nil {
		os.RemoveAll(tmp)
		return "", "", readErr
	}
	var subs []string
	for _, e := range entries {
		if e.IsDir() {
			subs = append(subs, e.Name())
		}
	}
	if len(subs) == 1 {
		sub := filepath.Join(tmp, subs[0])
		if _, statErr := os.Stat(filepath.Join(sub, "SKILL.md")); statErr == nil {
			return sub, tmp, nil
		}
	}
	os.RemoveAll(tmp)
	return "", "", fmt.Errorf("压缩包里找不到 SKILL.md（技能必须自带说明文件；SKILL.md 需在压缩包根部或其唯一子目录里）")
}

// wrapSkillMD 把单个 .md 说明文件包装成临时技能目录（内容落为目录根部的
// SKILL.md），返回 (技能根, 临时目录, 错误)；技能 id 由调用方从原文件名推导。
// 临时目录用完由调用方清理。
func wrapSkillMD(mdPath string) (root, tmp string, err error) {
	tmp, err = os.MkdirTemp("", "jxskmd-")
	if err != nil {
		return "", "", fmt.Errorf("创建临时目录失败: %w", err)
	}
	data, err := os.ReadFile(mdPath)
	if err != nil {
		os.RemoveAll(tmp)
		return "", "", fmt.Errorf("读取 %s 失败: %w", filepath.Base(mdPath), err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "SKILL.md"), data, 0644); err != nil {
		os.RemoveAll(tmp)
		return "", "", err
	}
	return tmp, tmp, nil
}

// toggleUserSkill 开关用户技能；自带技能不允许关闭。
func (a *App) toggleUserSkill(id string, enabled bool) error {
	if builtinSkillIDs[id] {
		return fmt.Errorf("自带技能常开，不能关闭")
	}
	userDir, err := a.userSkillsDir()
	if err != nil {
		return err
	}
	if !dirExists(filepath.Join(userDir, id)) {
		return fmt.Errorf("技能 %s 不存在", id)
	}
	st := a.loadSkillsState()
	if enabled {
		delete(st.Disabled, id)
	} else {
		st.Disabled[id] = true
	}
	return a.saveSkillsState(st)
}

// deleteUserSkill 删除用户技能（目录 + 开关状态）；自带技能拒绝。
func (a *App) deleteUserSkill(id string) error {
	if builtinSkillIDs[id] {
		return fmt.Errorf("自带技能不能删除")
	}
	userDir, err := a.userSkillsDir()
	if err != nil {
		return err
	}
	target := filepath.Join(userDir, id)
	if !dirExists(target) {
		return fmt.Errorf("技能 %s 不存在", id)
	}
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	st := a.loadSkillsState()
	delete(st.Disabled, id)
	return a.saveSkillsState(st)
}

// userSkillsPromptBlock 生成用户技能清单段（归入环境快照，变化补在对话尾部）：
// 只列 显示名（id）与一句话描述，正文不注入——模型要用哪个技能，先调用
// read_skill(id) 按需读完整说明（对齐 ZCode 的技能模式：清单先行、内容按需，
// 技能增减不动工具定义、不打断前缀缓存）。显示名中文优先（displayName →
// name）；描述超长截断（一句话描述的本意，全文在 read_skill）；整段超
// skillListingBudget 时降级为仅名字行（预算与两级形态均对齐 ZCode skills.ts）。
func (a *App) userSkillsPromptBlock() string {
	list, err := a.listSkills()
	if err != nil || len(list.User) == 0 {
		return ""
	}
	const header = "# 用户技能清单（要用某个技能时，先调用 read_skill 读它的完整说明）"
	var full, namesOnly []string
	for _, s := range list.User {
		if !s.Enabled {
			continue
		}
		disp := skillDisplayOf(s.DisplayName, s.Name, s.ID)
		namesOnly = append(namesOnly, "  - "+disp+"（id="+s.ID+"）")
		if desc := truncateRunesForPrompt(s.Description, userSkillDescPromptMax); desc != "" {
			full = append(full, "  - "+disp+"（id="+s.ID+"）："+desc)
		} else {
			full = append(full, namesOnly[len(namesOnly)-1])
		}
	}
	return renderSkillListing(header, full, namesOnly)
}

// assertSkillVisibility 启动自检（NewApp 调用）：技能可见性是全模式契约——
// 用户技能清单经环境快照注入每个模式（envPrompt 的固定条目，无模式谓词），
// read_skill 也在每个模式的工具目录里（toolsFor 出口统一追加）。历史上正是
// 「按模式手写门控」漏掉了先于技能功能存在的 office/coding——装了技能、
// 模型却收不到清单更新；把契约做成启动断言后，将来任何模式（含新增模式）
// 悄悄丢掉技能可见性都会在这里炸响，而不是静默坑用户。
// 自检会话 id 不落库，envPrompt 只读不写，无启动残留。
// 自检会话按完全访问构造：连接器清单段带能力门控（能力未开时整条隐藏是
// agent.go 环境快照明说的设计行为），自检验证的是「清单段与工具同模式注入」
// 这条契约，不是能力过滤——会话不带能力会让带门控的连接器全被过滤、清单
// 段为空，误报 panic（启用 cua/windows-mcp 后桌面版启动即崩的根因）。
func (a *App) assertSkillVisibility() {
	enabledSkills := false
	if list, err := a.listSkills(); err == nil {
		for _, s := range list.User {
			if s.Enabled {
				enabledSkills = true
				break
			}
		}
	}
	for _, mode := range sessionModes {
		hasRead := false
		for _, tl := range a.toolsFor(mode) {
			if tl.Spec.Function.Name == "read_skill" {
				hasRead = true
				break
			}
		}
		if !hasRead {
			panic(fmt.Sprintf("技能可见性自检失败：模式 %s 的工具目录里没有 read_skill（toolsFor 出口应全模式追加）", mode))
		}
		if enabledSkills {
			sess := &SessionMeta{ID: "__skill_selfcheck__", Mode: mode, Permission: permFull}
			if !strings.Contains(a.envPrompt(sess), "用户技能清单") {
				panic(fmt.Sprintf("技能可见性自检失败：模式 %s 的环境快照里没有用户技能清单（envPrompt 应为固定条目、无条件注入）", mode))
			}
		}
		// MCP 连接器可见性同款契约（mcpservers.go）：两个常驻按需工具与
		// 连接器清单段都只在执行类模式（mcpToolsMode），且两者同进同出；
		// 总控/答疑模式两者都不出现——不给「看得见调不着」的空头支票。
		if mcpToolsMode(mode) {
			hasMCPRead, hasMCPCall := false, false
			for _, tl := range a.toolsFor(mode) {
				switch tl.Spec.Function.Name {
				case "mcp_read_connector":
					hasMCPRead = true
				case "mcp_call_tool":
					hasMCPCall = true
				}
			}
			if !hasMCPRead || !hasMCPCall || (a.MCPServers == nil) {
				panic(fmt.Sprintf("MCP 可见性自检失败：模式 %s 的工具目录里 mcp_read_connector/mcp_call_tool 应随注册表常驻成对在场", mode))
			}
			if a.MCPServers.hasEnabled() {
				sess := &SessionMeta{ID: "__skill_selfcheck__", Mode: mode, Permission: permFull}
				if !strings.Contains(a.envPrompt(sess), "MCP 连接器清单") {
					panic(fmt.Sprintf("MCP 可见性自检失败：模式 %s 的环境快照里没有连接器清单（PromptBlock 应与工具同模式注入）", mode))
				}
			}
		}
	}
}

// readUserSkillTool 是用户技能的按需读取工具：清单只报名字与描述，
// 完整 SKILL.md 说明用它加载。工具常驻目录，技能增减不改变工具定义。
func readUserSkillTool() Tool {
	return Tool{
		Spec: toolSpec("read_skill", "读取一个技能的完整使用说明（SKILL.md 原文），内置技能（自带技能清单里）与用户技能都能读。环境快照里的技能清单只列名字与一句话描述；动手用某个技能之前，先调用本工具看完整说明。",
			objSchema(map[string]any{
				"id": strProp("技能 id，来自环境快照里的自带技能或用户技能清单"),
			}, "id")),
		Run: runReadUserSkill,
	}
}

func runReadUserSkill(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	id := strings.TrimSpace(p.ID)
	// 只允许读清单在列的技能：id 经名单校验，不拿用户输入拼路径。
	// 内置技能（如本机网络搜索）在 技能/<id>/ 常开；用户技能要求存在且启用。
	skillRoot, err := t.app.SkillsDir()
	if err != nil {
		return "", err
	}
	list, lerr := t.app.listSkills()
	if lerr == nil {
		for _, s := range list.BuiltIn {
			if s.ID == id {
				return readSkillBodyAt(filepath.Join(skillRoot, id), id)
			}
		}
	}
	inList := false
	if lerr == nil {
		for _, s := range list.User {
			if s.ID == id && s.Enabled {
				inList = true
				break
			}
		}
	}
	if !inList {
		// Claude Code 插件技能（ccplugin_skills.go）：带命名空间的 id（插件名:技能名）
		if t.app.CCPlugins != nil {
			if s, plugin, ok := t.app.CCPlugins.ccSkillByID(id); ok {
				body, err := plugin.ccSkillBody(&s, t.app.CCPlugins.UserConfigOf(plugin))
				if err != nil {
					return "", err
				}
				return body, nil
			}
		}
		return "", fmt.Errorf("技能 %s 不在当前清单里（不存在或已停用），请用清单里给出的 id", id)
	}
	return readSkillBodyAt(filepath.Join(skillRoot, "user", id), id)
}

// readSkillBodyAt 读技能目录里 SKILL.md 的正文（剥 frontmatter；空正文报错；
// 超长截断），read_skill 的内置与用户两个分支共用。
func readSkillBodyAt(dir, id string) (string, error) {
	body := stripFrontmatter(skillMeta(dir).Body)
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("技能 %s 没有可用的说明正文", id)
	}
	if r := []rune(body); len(r) > userSkillBodyMax {
		body = string(r[:userSkillBodyMax]) + "\n…（说明过长已截断，完整内容见技能目录）"
	}
	return body, nil
}

// registerSkillTool 是技能注册工具（创建技能模式专用）：把写好的技能目录
// 注册进用户技能库，闭合「写 SKILL.md → 注册 → read_skill 验证」的循环。
// 与设置页的 POST /api/skills/add 同走 addUserSkill；update=true 时先删
// 旧再装（addUserSkill 对已存在的 id 报错，迭代改进技能要用覆盖路径）。
func registerSkillTool() Tool {
	return Tool{
		Spec: toolSpec("register_skill",
			"把一个技能注册进用户技能库：路径可以是含 SKILL.md 的目录，或单个 .md 说明文件（自动包装成技能）。注册后技能进入各任务会话的环境快照技能清单，可用 read_skill 读回验证。技能已注册过、要更新时带 update=true（用当前路径内容覆盖旧版）。只读模式下不可用（会写用户技能库）：被拒后可用 request_permission 申请切档。",
			objSchema(map[string]any{
				"path":   strProp("技能路径：含 SKILL.md 的目录，或单个 .md 说明文件"),
				"update": boolProp("技能已存在时先删旧版再装（默认 false）"),
			}, "path")),
		Run: func(t *toolContext, args json.RawMessage) (string, error) {
			var p struct {
				Path   string `json:"path"`
				Update bool   `json:"update"`
			}
			if err := json.Unmarshal(args, &p); err != nil {
				return "", err
			}
			if err := requireNotReadOnly(t, "注册技能（会写入用户技能库）"); err != nil {
				return "", err
			}
			abs, err := t.resolve(p.Path)
			if err != nil {
				return "", err
			}
			isDir, _, serr := t.envStat(abs)
			if serr != nil {
				return "", fmt.Errorf("路径不存在: %s（%s）", abs, p.Path)
			}
			if isDir {
				if _, _, serr := t.envStat(filepath.Join(abs, "SKILL.md")); serr != nil {
					return "", fmt.Errorf("目录里缺少 SKILL.md（技能必须自带说明文件）：%s", abs)
				}
			} else if !strings.EqualFold(filepath.Ext(abs), ".md") {
				return "", fmt.Errorf("技能路径须是含 SKILL.md 的目录或单个 .md 说明文件: %s", abs)
			}
			entry, err := t.app.addUserSkill(abs)
			if err != nil {
				if !p.Update || !strings.Contains(err.Error(), "已存在") {
					return "", err
				}
				if err := t.app.deleteUserSkill(sanitizeSkillID(filepath.Base(abs))); err != nil {
					return "", err
				}
				entry, err = t.app.addUserSkill(abs)
				if err != nil {
					return "", err
				}
			}
			return fmt.Sprintf("已注册技能 %s（名称：%s%s）：各任务会话的技能清单会自动更新，"+
				"可用 read_skill(id=%s) 读回验证。", entry.ID, entry.Name,
				map[bool]string{true: "，覆盖旧版", false: ""}[p.Update], entry.ID), nil
		},
	}
}

func sanitizeSkillID(name string) string {
	name = strings.TrimSpace(name)
	name = strings.Map(func(r rune) rune {
		switch r {
		case '\\', '/', ':', '*', '?', '"', '<', '>', '|':
			return -1
		}
		return r
	}, name)
	return strings.TrimSpace(name)
}

func copyDir(src, dest string) error {
	return filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		target := filepath.Join(dest, rel)
		if info.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.Create(target)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = io.Copy(out, in)
		return err
	})
}

// ---------- HTTP 处理器 ----------

func (a *App) handleListSkills(w http.ResponseWriter, r *http.Request) {
	list, err := a.listSkills()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, list)
}

func (a *App) handleAddSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	entry, err := a.addUserSkill(body.Path)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, entry)
}

// handleUploadSkill 技能包直传（设置页拖拽安装）：multipart 的 file 字段
// 落临时文件后走与 /api/skills/add 同一条 addUserSkill 路径（解压/包装装
// 进 技能/user/）。浏览器拖拽拿不到本地路径，上传是唯一通道。
func (a *App) handleUploadSkill(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseMultipartForm(256 << 20); err != nil {
		writeErr(w, 400, "上传解析失败: "+err.Error())
		return
	}
	file, header, err := r.FormFile("file")
	if err != nil {
		writeErr(w, 400, "缺少 file 字段")
		return
	}
	defer file.Close()
	ext := strings.ToLower(filepath.Ext(header.Filename))
	if ext != ".zip" && ext != ".md" {
		writeErr(w, 400, "只支持 .zip 压缩包或单个 .md 说明文件（技能文件夹请用「添加技能」按钮选择）")
		return
	}
	// 临时文件保留原始文件名（放进专用临时目录）：addUserSkill 用文件名推导
	// 技能 id（.md 包装安装与根级 SKILL.md 的 zip 都靠它），随机临时名会把
	// id 变成 jxskill-xxxx；目录隔离同时避免重名冲突。
	tmpDir, err := os.MkdirTemp("", "jxskill-")
	if err != nil {
		writeErr(w, 500, "临时目录创建失败: "+err.Error())
		return
	}
	defer os.RemoveAll(tmpDir)
	tmpPath := filepath.Join(tmpDir, filepath.Base(header.Filename))
	tmp, err := os.Create(tmpPath)
	if err != nil {
		writeErr(w, 500, "临时文件创建失败: "+err.Error())
		return
	}
	if _, err := io.Copy(tmp, file); err != nil {
		tmp.Close()
		writeErr(w, 500, "上传写入失败: "+err.Error())
		return
	}
	tmp.Close()
	entry, err := a.addUserSkill(tmpPath)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, entry)
}

// handleInstallSkillFiles 技能文件夹拖拽安装（设置页拖入文件夹）：multipart 的
// files + paths（包内相对路径，公共顶层目录=技能文件夹名）→ 组装进临时目录 →
// 走与 /api/skills/add 同一条 addUserSkill。浏览器拖文件夹拿不到本地路径，
// webkitGetAsEntry 遍历出的文件集是唯一通道（前端 app-settings.js）。
func (a *App) handleInstallSkillFiles(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, marketPublishMaxZip)
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		writeErr(w, 400, "上传解析失败（或超出大小上限）: "+err.Error())
		return
	}
	var uploads []*multipart.FileHeader
	if r.MultipartForm != nil {
		uploads = r.MultipartForm.File["files"]
	}
	if len(uploads) == 0 {
		writeErr(w, 400, "没有收到文件（files 为空）")
		return
	}
	var paths []string
	if s := strings.TrimSpace(r.FormValue("paths")); s != "" {
		if err := json.Unmarshal([]byte(s), &paths); err != nil {
			writeErr(w, 400, "paths 不是合法 JSON 数组: "+err.Error())
			return
		}
	}
	// 公共顶层目录（=拖进来的文件夹名，兼当技能 id 的推导来源）
	tops := map[string]bool{}
	for i := range uploads {
		p := ""
		if i < len(paths) {
			p = cleanZipPath(paths[i])
		}
		if p == "" {
			p = cleanZipPath(uploads[i].Filename)
		}
		if i := strings.Index(p, "/"); i > 0 {
			tops[p[:i]] = true
		} else {
			tops[""] = true // 有文件不在任何顶层目录下：散文件形态
		}
	}
	top := ""
	for t := range tops {
		if t == "" || top != "" {
			writeErr(w, 400, "请拖入整个技能文件夹（应只有一个顶层目录，目录里含 SKILL.md），而不是散文件")
			return
		}
		top = t
	}
	if top == "" {
		writeErr(w, 400, "请拖入整个技能文件夹（应只有一个顶层目录，目录里含 SKILL.md），而不是散文件")
		return
	}
	tmp, err := os.MkdirTemp("", "jxskf-")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer os.RemoveAll(tmp)
	root := filepath.Join(tmp, top)
	for i, uh := range uploads {
		rel := ""
		if i < len(paths) {
			rel = cleanZipPath(paths[i])
		}
		if rel == "" {
			rel = cleanZipPath(uh.Filename)
		}
		rel = strings.TrimPrefix(rel, top+"/") // 剥掉顶层目录，落到 root 下
		if rel == "" {
			continue // 顶层目录自身的条目（遍历文件夹时理论上不会出现）
		}
		if rel == ".." || strings.HasPrefix(rel, "../") || strings.Contains(rel, "/../") {
			writeErr(w, 400, "文件名里有非法路径: "+rel)
			return
		}
		target := filepath.Join(root, filepath.FromSlash(rel))
		if !strings.HasPrefix(target, root+string(os.PathSeparator)) {
			writeErr(w, 400, "文件名里有非法路径: "+rel)
			return
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		in, err := uh.Open()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		out, err := os.Create(target)
		if err != nil {
			in.Close()
			writeErr(w, 500, err.Error())
			return
		}
		_, err = io.Copy(out, in)
		in.Close()
		out.Close()
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
	}
	entry, err := a.addUserSkill(root)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, entry)
}

func (a *App) handleToggleSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID      string `json:"id"`
		Enabled bool   `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeErr(w, 400, "需要 id 字段")
		return
	}
	if err := a.toggleUserSkill(body.ID, body.Enabled); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (a *App) handleDeleteSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ID == "" {
		writeErr(w, 400, "需要 id 字段")
		return
	}
	if err := a.deleteUserSkill(body.ID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}
