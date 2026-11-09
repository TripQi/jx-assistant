package main

// Claude Code 插件组件桥：skills。
// 插件的 skills/<名>/SKILL.md 注册为带命名空间的技能（id = <插件名>:<技能名>），
// 与本引擎「清单先行、内容按需」的技能模式同构：清单进环境快照尾部，
// 正文经 read_skill 按需读取。读取时替换 ${CLAUDE_PLUGIN_ROOT}、
// ${CLAUDE_SKILL_DIR} 等路径变量（对齐 Claude 的技能正文约定）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// ccSkillEntry 是插件携带的一个技能。
type ccSkillEntry struct {
	PluginKey   string
	PluginName  string
	ID          string // read_skill 用的 id：<插件名>:<技能名>
	Name        string
	Description string
	Dir         string // SKILL.md 所在目录
}

// loadSkills 收集插件的全部技能。对齐 Claude 规则：skills/ 与 manifest.skills
// 声明目录（追加语义，"."=插件根）下每个含 SKILL.md 的子目录是一个技能，
// frontmatter name 覆盖目录名；没有任何 skills 目录且 manifest 未声明时，
// 插件根的 SKILL.md 作为单一技能加载。
func (p *CCPlugin) loadSkills() {
	p.Skills = nil
	manifestDeclared := len(p.Manifest.Skills) > 0
	seen := map[string]bool{}
	for _, d := range ccStringList(p.Manifest.Skills, "skills") {
		dir := p.Root
		if d != "." {
			dir = filepath.Join(p.Root, filepath.FromSlash(d))
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // 目录不存在：跳过（默认 skills/ 常见缺失）
		}
		for _, e := range entries {
			if !e.IsDir() || strings.HasPrefix(e.Name(), ".") {
				continue
			}
			skillDir := filepath.Join(dir, e.Name())
			if _, err := os.Stat(filepath.Join(skillDir, "SKILL.md")); err != nil {
				continue
			}
			p.addSkillFrom(skillDir, seen)
		}
	}
	// 特例：无 skills 目录且无 manifest 声明、插件根有 SKILL.md → 单一技能
	if len(p.Skills) == 0 && !manifestDeclared {
		if _, err := os.Stat(filepath.Join(p.Root, "SKILL.md")); err == nil {
			p.addSkillFrom(p.Root, seen)
		}
	}
}

// addSkillFrom 从一个含 SKILL.md 的目录登记技能（frontmatter name 覆盖目录名；
// description 与 when_to_use 拼接后截断，对齐 Claude 的合成规则）。
func (p *CCPlugin) addSkillFrom(dir string, seen map[string]bool) {
	fm, _ := parseCCFrontmatter(readFileOr(filepath.Join(dir, "SKILL.md")))
	name := filepath.Base(filepath.Clean(dir))
	if v, ok := fm["name"].(string); ok && strings.TrimSpace(v) != "" {
		name = strings.TrimSpace(v)
	}
	if seen[name] {
		return // 同名去重（首个来源优先）
	}
	seen[name] = true
	desc, _ := fm["description"].(string)
	if when, ok := fm["when_to_use"].(string); ok && strings.TrimSpace(when) != "" {
		desc = strings.TrimSpace(desc + " " + when)
	}
	if r := []rune(desc); len(r) > 500 {
		desc = string(r[:500]) + "…"
	}
	p.Skills = append(p.Skills, ccSkillEntry{
		PluginKey: p.Key, PluginName: p.Name,
		ID: p.Name + ":" + name, Name: name, Description: desc, Dir: dir,
	})
}

// readFileOr 读文件，失败返回空串（frontmatter 解析容忍空文件）。
func readFileOr(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return string(data)
}

// ccSkills 返回全部启用插件中组件放行的技能（按 id 排序）。
func (r *CCPluginRegistry) ccSkills() []ccSkillEntry {
	out := []ccSkillEntry{}
	for _, p := range r.enabled() {
		if !p.componentEnabled("skills") {
			continue
		}
		out = append(out, p.Skills...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ccSkillByID 按 id 查一个插件技能（id 经清单校验，不拿用户输入拼路径）。
func (r *CCPluginRegistry) ccSkillByID(id string) (ccSkillEntry, *CCPlugin, bool) {
	for _, p := range r.enabled() {
		if !p.componentEnabled("skills") {
			continue
		}
		for _, s := range p.Skills {
			if s.ID == id {
				return s, p, true
			}
		}
	}
	return ccSkillEntry{}, nil, false
}

// ccSkillsPromptBlock 生成插件技能清单段（归入环境快照，追加在用户技能清单后）。
// 与用户技能段同一纪律：描述单条截断（userSkillDescPromptMax）、整段超
// skillListingBudget 降级为仅名字行（renderSkillListing，对齐 ZCode skills.ts）。
func (r *CCPluginRegistry) ccSkillsPromptBlock() string {
	skills := r.ccSkills()
	if len(skills) == 0 {
		return ""
	}
	const header = "# 插件技能清单（来自 Claude Code 插件；要用某个技能时，先调用 read_skill 读它的完整说明）"
	var full, namesOnly []string
	for _, s := range skills {
		namesOnly = append(namesOnly, "  - "+s.Name+"（id="+s.ID+"）")
		if desc := truncateRunesForPrompt(s.Description, userSkillDescPromptMax); desc != "" {
			full = append(full, "  - "+s.Name+"（id="+s.ID+"）："+desc)
		} else {
			full = append(full, namesOnly[len(namesOnly)-1])
		}
	}
	return renderSkillListing(header, full, namesOnly)
}

// ccSkillBody 读取技能正文（read_skill 的插件分支）：剥 frontmatter、
// 替换路径变量、截断到与用户技能同一上限。
func (p *CCPlugin) ccSkillBody(s *ccSkillEntry, userCfg map[string]any) (string, error) {
	_, body := parseCCFrontmatter(readFileOr(filepath.Join(s.Dir, "SKILL.md")))
	if strings.TrimSpace(body) == "" {
		return "", fmt.Errorf("技能 %s 没有可用的说明正文", s.ID)
	}
	body = p.expandVars(body, userCfg)
	body = strings.ReplaceAll(body, "${CLAUDE_SKILL_DIR}", s.Dir)
	if r := []rune(body); len(r) > userSkillBodyMax {
		body = string(r[:userSkillBodyMax]) + "\n…（说明过长已截断，完整内容见插件目录）"
	}
	return body, nil
}

// expandVars 替换 Claude 插件规范里的路径与配置变量：
// ${CLAUDE_PLUGIN_ROOT}、${CLAUDE_PLUGIN_DATA}、${user_config.<键>}。
// ${CLAUDE_PROJECT_DIR}（会话工作区）由 hook 执行器按会话补。
func (p *CCPlugin) expandVars(s string, userCfg map[string]any) string {
	s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_ROOT}", p.Root)
	s = strings.ReplaceAll(s, "${ZCODE_PLUGIN_ROOT}", p.Root)
	s = strings.ReplaceAll(s, "${CLAUDE_PLUGIN_DATA}", p.DataDir)
	s = strings.ReplaceAll(s, "${ZCODE_PLUGIN_DATA}", p.DataDir)
	return replaceUserConfigVars(s, userCfg)
}

// replaceUserConfigVars 替换 ${user_config.<键>}（未配置的键替换为空串）。
func replaceUserConfigVars(s string, userCfg map[string]any) string {
	if !strings.Contains(s, "${user_config.") {
		return s
	}
	var b strings.Builder
	for {
		i := strings.Index(s, "${user_config.")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], "}")
		if j < 0 {
			b.WriteString(s)
			return b.String()
		}
		b.WriteString(s[:i])
		key := s[i+len("${user_config.") : i+j]
		if v, ok := userCfg[key]; ok {
			b.WriteString(fmt.Sprintf("%v", v))
		}
		s = s[i+j+1:]
	}
}

// ---------- 轻量 YAML frontmatter 解析 ----------

// parseCCFrontmatter 解析 Markdown 的 YAML frontmatter（首行 --- 到下一个
// 行首 ---）。支持子集：标量（剥引号）、行内列表 [a, b]、块列表（- item）、
// bool 多态（true/yes/on/1）、一层嵌套 map（缩进）。解析失败或缺失时返回
// 空 map + 全文为正文（对齐 Claude「字段全空仍加载」的行为）。
func parseCCFrontmatter(text string) (map[string]any, string) {
	fm := map[string]any{}
	lines := strings.Split(text, "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != "---" {
		return fm, text
	}
	end := -1
	for i := 1; i < len(lines); i++ {
		t := strings.TrimSpace(lines[i])
		if t == "---" || t == "..." {
			end = i
			break
		}
	}
	if end < 0 {
		return fm, text
	}
	parseSimpleYAML(strings.Join(lines[1:end], "\n"), fm)
	return fm, strings.TrimSpace(strings.Join(lines[end+1:], "\n"))
}

// parseSimpleYAML 逐行解析轻量 YAML（子集见 parseCCFrontmatter）。
func parseSimpleYAML(front string, fm map[string]any) {
	lines := strings.Split(front, "\n")
	i := 0
	for i < len(lines) {
		line := lines[i]
		i++
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		ci := strings.Index(trimmed, ":")
		if ci <= 0 {
			continue
		}
		key := strings.TrimSpace(trimmed[:ci])
		val := strings.TrimSpace(trimmed[ci+1:])
		if val == "" {
			// 块列表：吃掉后续 "- " 开头的行
			if i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "- ") {
				var items []any
				for i < len(lines) {
					t := strings.TrimSpace(lines[i])
					if !strings.HasPrefix(t, "- ") {
						break
					}
					items = append(items, parseYAMLScalar(strings.TrimSpace(t[2:])))
					i++
				}
				fm[key] = items
				continue
			}
			// 一层嵌套 map：吃掉后续更深缩进的 "k: v" 行
			nested := map[string]any{}
			for i < len(lines) {
				nl := lines[i]
				nt := strings.TrimSpace(nl)
				if nt == "" {
					i++
					continue
				}
				nIndent := len(nl) - len(strings.TrimLeft(nl, " \t"))
				if nIndent <= indent {
					break
				}
				if nc := strings.Index(nt, ":"); nc > 0 && !strings.HasPrefix(nt, "- ") {
					nested[strings.TrimSpace(nt[:nc])] = parseYAMLScalar(strings.TrimSpace(nt[nc+1:]))
				}
				i++
			}
			if len(nested) > 0 {
				fm[key] = nested
			}
			continue
		}
		fm[key] = parseYAMLScalar(val)
	}
}

// parseYAMLScalar 解析一个标量：行内列表、剥引号、bool 多态、数字。
func parseYAMLScalar(v string) any {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		inner := strings.TrimSpace(v[1 : len(v)-1])
		if inner == "" {
			return []any{}
		}
		parts := strings.Split(inner, ",")
		out := make([]any, 0, len(parts))
		for _, p := range parts {
			out = append(out, parseYAMLScalar(p))
		}
		return out
	}
	if len(v) >= 2 {
		if (v[0] == '"' && v[len(v)-1] == '"') || (v[0] == '\'' && v[len(v)-1] == '\'') {
			return v[1 : len(v)-1]
		}
	}
	switch strings.ToLower(v) {
	case "true", "yes", "on":
		return true
	case "false", "no", "off":
		return false
	case "null", "~":
		return nil
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f
	}
	return v
}

// ccStringList 解释 manifest 的组件路径键：返回 目录/文件 清单。
// 形态：缺省（用 def）| "path" | ["p1","p2"]；对象形态返回空（内联命令由
// 命令桥单独处理）。
func ccStringList(raw json.RawMessage, def string) []string {
	if len(raw) == 0 {
		if def == "" {
			return nil
		}
		return []string{def}
	}
	var one string
	if err := json.Unmarshal(raw, &one); err == nil {
		return []string{one}
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err == nil {
		return many
	}
	return nil
}
