package main

// 项目管理闭环提示词（Docs/项目管理提示词闭环-方案.md）的定向测试：
//  1. 拼装：经理的维护要求落在覆盖后仍追加的 managerCoordPrompt；执行模式的
//     共用短段只拼给五种执行模式；aux 两侧都没有（只读答疑不承担维护）。
//  2. 覆盖：data/prompts/<mode>.md 替换基础正文后，附加段仍在（含新增短段）。
//  3. 开关与角色边界：组件能力块（环境快照）按组件开关与模式白名单即时进出，
//     项目知识写的说明只在长期助理的能力块里，事件写的能力不进 aux；关掉组件
//     后对应维护说明随能力块消失，而系统提示词与工具目录不变。
//
// 这里验证的是最终提示词/能力块的拼装结果与可见范围，**不声称模型行为已在
// 真机验证**——文案是给模型的约束，行为仍取决于模型与运行时。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 两段的唯一标题：执行侧是共用的短段，经理侧在 managerCoordPrompt 里。
const (
	projectLoopExecHeading = "## 项目知识与事件"
	projectLoopMgrHeading  = "## 项目知识库与项目事件"
)

func execModeList() []string {
	return []string{"task", "office", "coding", "beginner-coding", "skill-create"}
}

func TestProjectLoopPromptWiring(t *testing.T) {
	a := newPermTestApp(t, "data-projloop-test")
	a.Cfg.DataDir = roTestWS(t) // 空目录：杜绝 data/prompts 引用文件覆盖干扰

	for _, mode := range execModeList() {
		sys := a.systemPrompt(&SessionMeta{ID: "pl-" + mode, Mode: mode})
		wantAll(t, sys, "模式 "+mode, projectLoopExecHeading,
			"项目知识会自动注入给你", "随派发回报交给经理", "知识只提建议",
			"不越级联系经理", "不要声称已通知经理",
			"没有候选就不填模板", "任务完成不等于用户验收",
			"## 找经理帮忙")
		if strings.Contains(sys, projectLoopMgrHeading) {
			t.Fatalf("模式 %s 不该拿到经理侧的「项目知识库与项目事件」段", mode)
		}
	}

	as := a.systemPrompt(&SessionMeta{ID: "pl-assistant", Mode: "assistant"})
	wantAll(t, as, "长期助理", projectLoopMgrHeading,
		"正文始终只有你能写", "晚到旧事实不倒退当前概况", "任务完成不等于用户验收",
		"不用 request_manager 重复报同一件事")
	// 经理侧与执行侧口径不混：执行侧的两条判据不该出现在经理段里
	for _, gone := range []string{"随派发回报交给经理", "知识只提建议"} {
		if strings.Contains(as, gone) {
			t.Fatalf("长期助理不该拿到执行侧的 %q", gone)
		}
	}

	aux := a.systemPrompt(&SessionMeta{ID: "pl-aux", Mode: "aux"})
	for _, gone := range []string{projectLoopExecHeading, projectLoopMgrHeading, "任务完成不等于用户验收"} {
		if strings.Contains(aux, gone) {
			t.Fatalf("aux 只读答疑不该拿到项目管理维护要求 %q", gone)
		}
	}
}

// 引用文件覆盖后附加段仍在：覆盖只换基础正文，产品硬规则照拼（含新增短段）。
func TestProjectLoopPromptSurvivesOverride(t *testing.T) {
	a := newPermTestApp(t, "data-projloop-ovr")
	pdir := filepath.Join(a.Cfg.DataDir, "prompts")
	if err := os.MkdirAll(pdir, 0755); err != nil {
		t.Fatal(err)
	}
	// 注意：不能再写 aux.md——Windows 上 AUX 是保留设备名（aux.md 仍解析到
	// AUX 设备），写会跑到设备上、读直接挂住。aux 的覆盖本就无附加段可验，
	// 它只在「拼装」用例里查一次。
	for mode, text := range map[string]string{
		"assistant": "自定义长期助理正文（引用文件覆盖）。",
		"task":      "自定义任务正文（引用文件覆盖）。",
	} {
		if err := os.WriteFile(filepath.Join(pdir, mode+".md"), []byte(text), 0644); err != nil {
			t.Fatal(err)
		}
	}

	as := a.systemPrompt(&SessionMeta{ID: "ov-assistant", Mode: "assistant"})
	wantAll(t, as, "覆盖后的长期助理", "自定义长期助理正文", projectLoopMgrHeading, "系统提示词：引用文件")

	tk := a.systemPrompt(&SessionMeta{ID: "ov-task", Mode: "task"})
	wantAll(t, tk, "覆盖后的任务", "自定义任务正文", projectLoopExecHeading,
		"## 文件修改与交付规范", "## 子对话派发", "## 找经理帮忙", "系统提示词：引用文件")
	if tk == as {
		t.Fatal("两种模式应各自读到自己的引用文件")
	}
}

// newProjectLoopCompApp 只复制两个项目组件的清单进临时目录（不启运行时、
// 不读真实配置），用来渲染组件能力块。
func newProjectLoopCompApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	a := &App{Cfg: cfg}
	compDir := filepath.Join(dir, "components")
	for _, id := range []string{"project-knowledge", "project-events"} {
		src, err := os.ReadFile(filepath.Join("组件", id, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(compDir, id, "manifest.json")
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, src, 0644); err != nil {
			t.Fatal(err)
		}
	}
	a.Components = InitComponents(compDir, filepath.Join(dir, "components.json"))
	return a
}

// 组件能力块的条件化：知识写的说明只进长期助理，事件写的能力不进 aux；组件一关，
// 对应维护说明随能力块消失，而系统提示词不变（规则条件化在能力块上，不是代码分支）。
func TestProjectCapabilityGuidanceConditional(t *testing.T) {
	a := newProjectLoopCompApp(t)

	asst := a.componentCapabilityBlock(&SessionMeta{ID: "cap-assistant", Mode: "assistant"})
	wantAll(t, asst, "长期助理能力块", "project_knowledge_update", "project_event_record_add",
		"知识正文仅长期助理可写", "只留必要背景与持续有效约束")
	// 经理不写文件：知识写的说明只出现在能力块里，不改变它的工具集

	taskBlock := a.componentCapabilityBlock(&SessionMeta{ID: "cap-task", Mode: "task"})
	wantAll(t, taskBlock, "任务能力块", "project_knowledge_get", "project_event_record_add",
		"任务完成不等于用户验收", "晚到旧事实不倒退概况")
	if strings.Contains(taskBlock, "project_knowledge_update") {
		t.Fatal("知识写能力不该出现在工作任务的能力块里（正文只有长期助理写）")
	}

	auxBlock := a.componentCapabilityBlock(&SessionMeta{ID: "cap-aux", Mode: "aux"})
	wantAll(t, auxBlock, "aux 能力块", "project_knowledge_get", "project_events_query")
	for _, gone := range []string{"project_knowledge_update", "project_event_record_add",
		"project_event_create", "project_event_update"} {
		if strings.Contains(auxBlock, gone) {
			t.Fatalf("aux 能力块不该出现写入能力 %q", gone)
		}
	}

	// 关掉知识组件：知识类说明整组消失，事件类说明仍在；系统提示词一字不变
	before := a.systemPrompt(&SessionMeta{ID: "cap-task", Mode: "task"})
	if err := a.Components.SetEnabled("project-knowledge", false); err != nil {
		t.Fatal(err)
	}
	after := a.componentCapabilityBlock(&SessionMeta{ID: "cap-task", Mode: "task"})
	wantAll(t, after, "关闭知识后的事件能力块", "project_event_record_add")
	for _, gone := range []string{"project_knowledge_get", "project_knowledge_update", "只留必要背景与持续有效约束"} {
		if strings.Contains(after, gone) {
			t.Fatalf("知识组件关闭后能力块不该再有 %q：\n%s", gone, after)
		}
	}
	if got := a.systemPrompt(&SessionMeta{ID: "cap-task", Mode: "task"}); got != before {
		t.Fatal("组件开关只改环境快照的能力块，不应改动系统提示词")
	}

	// 两个都关掉：整块不出现（不含空块标题），提示词里的条件化措辞仍在
	if err := a.Components.SetEnabled("project-events", false); err != nil {
		t.Fatal(err)
	}
	if block := a.componentCapabilityBlock(&SessionMeta{ID: "cap-task", Mode: "task"}); block != "" {
		t.Fatalf("两个组件都关闭后不该有能力块：\n%s", block)
	}
	if !strings.Contains(before, "仅当环境快照的「组件能力」块列出对应项目能力时") {
		t.Fatal("执行模式的短段应把规则条件化在能力块上")
	}
}
