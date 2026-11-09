package main

// 统一组件能力分发（模型工具 component_call）。设计意图（用户拍板，方案 A）：
// **工具定义只装基础工具，组件的动态能力不进工具目录**——组件的能力改由
// 「环境提示词声明 + 一个统一入口执行」承载。这样每加一个组件能力都不再改
// 工具定义：工具目录恒定（请求形状与前缀缓存稳定），组件开关只影响环境快照
// 的「组件能力」块。
//
// 分工（两半，缺一不可）：
//   - 声明：组件 manifest.json 顶层 "capabilities" 数组（名称/说明/参数 schema/
//     可用模式/是否写入）——**白名单的权威在这里**，环境快照的能力块也由它渲染；
//   - 实现：宿主注册表（registerComponentCapability，组件自己的 .go 文件在
//     init() 里登记，见 usermemory.go 的 memory_query/memory_update）——组件
//     后端代码在主程序里运行，声明只描述「能调什么」，怎么干归宿主实现。
//
// 调用链：component_call(component, capability, args) → 组件存在？→ 本会话
// 启用？→ 能力已声明？→ 本模式可用？→ 宿主有实现？→ 交给实现跑。
// 权限语义沿用各组件原来的专用工具：会话级组件开关优先（本对话关掉即不可调，
// 不必重启），模式白名单由声明里的 modes 决定。
//
// 扩展方式（下一个组件怎么接）：①manifest 的 capabilities 里加一条声明；
// ②宿主 init() 里 registerComponentCapability(组件id, 能力名, 函数) 写实现；
// ③在环境快照确认能力块出现（envPrompt → componentCapabilityBlock）。不改
// toolsFor、不改任何工具定义。

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// ComponentCapabilityRun 是一条能力的宿主实现。args 是模型给的参数 JSON（可能
// 为 nil＝没传参），语义完全由组件自己定；返回值是给模型看的文本回执。
type ComponentCapabilityRun func(t *toolContext, args json.RawMessage) (string, error)

var (
	compCapRunsMu sync.RWMutex
	compCapRuns   = map[string]map[string]ComponentCapabilityRun{} // 组件 id → 能力名 → 实现
)

// registerComponentCapability 登记一条能力的宿主实现（各组件在自己的 .go 文件
// init() 里调一次；重复登记同一能力后者覆盖，热更新组件清单不影响它）。
func registerComponentCapability(compID, name string, run ComponentCapabilityRun) {
	if compID == "" || name == "" || run == nil {
		return
	}
	compCapRunsMu.Lock()
	defer compCapRunsMu.Unlock()
	m := compCapRuns[compID]
	if m == nil {
		m = map[string]ComponentCapabilityRun{}
		compCapRuns[compID] = m
	}
	m[name] = run
}

// componentCapabilityRun 查一条已登记的实现。
func componentCapabilityRun(compID, name string) (ComponentCapabilityRun, bool) {
	compCapRunsMu.RLock()
	defer compCapRunsMu.RUnlock()
	run, ok := compCapRuns[compID][name]
	return run, ok && run != nil
}

// componentCallTool 是基础工具 component_call（全模式注册，目录恒定）：组件的
// 全部能力都从这个口子进，具体能调什么由环境快照的「组件能力」块告知。
func componentCallTool() Tool {
	return Tool{
		Spec: toolSpec("component_call",
			"组件能力统一入口：调用某个已启用组件声明的能力（查询/更新用户记忆等）。**能力清单不在这里，看环境快照的「组件能力」块**——那里逐行列着当前可用的组件 id、能力名与参数说明；本工具只是统一入口，别凭记忆猜能力名（猜错会报错并给出该组件已声明的能力名）。参数 args 按清单里该能力的参数说明填。",
			objSchema(map[string]any{
				"component":  strProp("组件 id（环境快照「组件能力」块每行列出的组件 id，如 user-memory）"),
				"capability": strProp("能力名（同一行里列出的能力名，如 memory_query / memory_update）"),
				"args": map[string]any{
					"type":        "object",
					"description": "能力参数对象（按清单里该能力的参数说明填；无参数的能力可省略）",
				},
			}, "component", "capability")),
		Run: runComponentCall,
	}
}

// runComponentCall 是 component_call 的分发体：白名单、模式、会话开关逐层核完
// 再交给实现——报错都指路（列出可用能力/可用模式），模型不必反复试错。
func runComponentCall(t *toolContext, raw json.RawMessage) (string, error) {
	var p struct {
		Component  string          `json:"component"`
		Capability string          `json:"capability"`
		Args       json.RawMessage `json:"args"`
	}
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &p); err != nil {
			return "", err
		}
	}
	comp := strings.TrimSpace(p.Component)
	capName := strings.TrimSpace(p.Capability)
	if comp == "" || capName == "" {
		return "", fmt.Errorf("需要 component 与 capability 两个参数；可用能力见环境快照的「组件能力」块")
	}
	if t.app.Components == nil {
		return "", fmt.Errorf("组件机制未初始化，component_call 不可用")
	}
	manifest, ok := t.app.Components.Manifest(comp)
	if !ok {
		return "", fmt.Errorf("没有 id 为 %q 的组件；当前声明了能力的组件：%s", comp, strings.Join(t.app.componentCapabilityIDs(), "、"))
	}
	// 组件开关：本会话生效值为准（会话级覆盖优先于全局）——组件关掉的能力不可调
	sid := ""
	if t.sess != nil {
		sid = t.sess.ID
	}
	if !t.app.cxoComponentEnabled(sid, comp) {
		return "", fmt.Errorf("组件「%s」在本会话未启用，它的能力不可调用；请到 设置 → 组件 或 加号菜单「本对话的组件与插件」打开", manifest.Name)
	}
	capDef, ok := t.app.Components.Capability(comp, capName)
	if !ok {
		return "", fmt.Errorf("组件「%s」没有声明能力 %q；它已声明：%s", manifest.Name, capName, strings.Join(capabilityNames(manifest.Capabilities), "、"))
	}
	mode := ""
	if t.sess != nil {
		mode = t.sess.Mode
	}
	if !capDef.allowedIn(mode) {
		return "", fmt.Errorf("能力「%s」不在当前模式（%s）的可用范围；可用模式：%s", capName, modeLabel(mode), strings.Join(capDef.Modes, "、"))
	}
	if capDef.Writes && t.sess != nil && t.sess.PlanMode {
		return "", fmt.Errorf("计划模式下暂停写类组件能力（「%s」声明为写入型）；先用只读能力调研，exit_plan_mode 获批后再调", capName)
	}
	run, ok := componentCapabilityRun(comp, capName)
	if !ok {
		return "", fmt.Errorf("能力「%s」已在组件清单里声明，但宿主没有安装它的实现（程序版本可能偏旧）；请更新程序或改用其它能力", capName)
	}
	args := p.Args
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	return run(t, args)
}

// capabilityNames 取一组能力的能力名（报错与日志用）。
func capabilityNames(caps []ComponentCapability) []string {
	out := make([]string, 0, len(caps))
	for _, c := range caps {
		out = append(out, c.Name)
	}
	if len(out) == 0 {
		return []string{"（无）"}
	}
	return out
}

// componentCapabilityIDs 列出声明了能力的组件 id（报错指路用，按 id 排序）。
func (a *App) componentCapabilityIDs() []string {
	if a.Components == nil {
		return nil
	}
	out := []string{}
	for _, st := range a.Components.List() {
		if len(a.Components.Capabilities(st.ID)) > 0 {
			out = append(out, st.ID)
		}
	}
	sort.Strings(out)
	return out
}

// modeLabel 模式的界面叫法（报错文案用）。
func modeLabel(mode string) string {
	if l := map[string]string{
		"task": "工作任务", "office": "办公", "coding": "编程模式",
		"beginner-coding": "新手编程", "skill-create": "创建技能",
		"assistant": "长期助理", "aux": "只读答疑",
	}[mode]; l != "" {
		return l
	}
	return mode
}

// ---------- 环境提示词块（envPrompt 调用；不进系统提示词/工具定义） ----------

// componentCapabilityBlock 渲染环境快照的「组件能力」块：逐个启用组件（本会话
// 视角）列出它在当前模式下可调的能力——组件开关、清单声明、模式白名单任一不
// 满足即整行消失（工具定义恒定，开关只影响这一块，见方案 A 的自查项）。
// 没有可调能力时返回空串（不出现空块）。
func (a *App) componentCapabilityBlock(sess *SessionMeta) string {
	if a.Components == nil {
		return ""
	}
	sid, mode := "", ""
	planMode := false
	if sess != nil {
		sid, mode, planMode = sess.ID, sess.Mode, sess.PlanMode
	}
	var b strings.Builder
	for _, st := range a.Components.List() { // List 按 id 排序，块序稳定（差分友好）
		if !a.cxoComponentEnabled(sid, st.ID) {
			continue
		}
		var lines []string
		for _, c := range a.Components.Capabilities(st.ID) {
			if !c.allowedIn(mode) {
				continue
			}
			desc := c.Description
			if s := capabilityParamsHint(c); s != "" {
				desc += "；" + s
			}
			if c.Writes {
				desc += "｜写入型" + map[bool]string{true: "（计划模式下暂停）", false: ""}[planMode]
			}
			lines = append(lines, "  - "+c.Name+"："+desc)
		}
		if len(lines) == 0 {
			continue
		}
		if b.Len() == 0 {
			b.WriteString("# 组件能力（组件声明的可调用能力，统一经 component_call 调用）\n")
			b.WriteString("- 这些能力**不在工具目录里**：调用 component_call(component=组件id, capability=能力名, args={参数})；参数按行内说明填，无参数可省 args。列表随组件开关与当前模式变化（开关即变，不必重启）\n")
		}
		b.WriteString("- 组件 " + st.ID + "（" + st.Name + "）：\n")
		for _, ln := range lines {
			b.WriteString(ln + "\n")
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

// capabilityParamsHint 把能力声明的参数 schema 压成一行中文提示（进能力行）。
// 只读 properties 的键与它的 description——服务端对复杂 schema 不建模，参数
// 真正怎么解释归组件实现；这里只让模型知道该填哪些键。
func capabilityParamsHint(c ComponentCapability) string {
	if len(c.Parameters) == 0 {
		return ""
	}
	var sch struct {
		Properties map[string]struct {
			Type        string `json:"type"`
			Description string `json:"description"`
		} `json:"properties"`
		Required []string `json:"required"`
	}
	if err := json.Unmarshal(c.Parameters, &sch); err != nil || len(sch.Properties) == 0 {
		return ""
	}
	keys := make([]string, 0, len(sch.Properties))
	for k := range sch.Properties {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		p := sch.Properties[k]
		d := k
		if p.Description != "" {
			d += "＝" + p.Description
		}
		parts = append(parts, d)
	}
	return "参数：" + strings.Join(parts, "；")
}
