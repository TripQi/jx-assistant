package main

// 「三条路径共用同一个函数」的验收（第 e 项）。两个角度一起看：
//
//  1. 结构：全包非测试源码里 normalizeHistoryToolCalls 只有**一份定义、一个调用点**
//     （context.go 的 buildContext 出口 = 一切请求的唯一组装口）。有人想在模式
//     切换、子对话继承、组件开关三条路上各写一套近似逻辑，这条先红。
//  2. 运行：三条路径各跑一次真实组装，归一化的降级计数各 +1——说明三条路径走的
//     确实是那一份实现，而不是某条路径悄悄绕过去了。
//
// 这条用例只存在于修复分支（基线没有归一化函数），修前红由行为层用例
// （history_tool_ref_test.go，可原样拷到基线跑）承担。

import (
	"os"
	"strings"
	"testing"
)

func TestHistoryToolNormalizationIsOneSharedFunction(t *testing.T) {
	const fn = "normalizeHistoryToolCalls"
	const decl = "func " + fn + "("
	defs, calls := 0, 0
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("读 %s 失败: %v", name, err)
		}
		src := string(b)
		n := strings.Count(src, fn+"(")
		d := strings.Count(src, decl)
		defs += d
		calls += n - d
	}
	if defs != 1 {
		t.Fatalf("%s 必须只有一份定义（实际 %d 份）——三条路径不许各写一套", fn, defs)
	}
	if calls != 1 {
		t.Fatalf("%s 必须只有一个调用点（实际 %d 处）——唯一出口是 buildContext", fn, calls)
	}

	// 运行期：三条路径各降级一次（每场景只丢一个工具），计数各 +1
	run := func(stage string, build func(t *testing.T) (*App, *SessionMeta)) {
		t.Helper()
		a, sess := build(t)
		historyToolRefCount.Store(0)
		a.ctxEngine().Assemble(sess, nil)
		if got := historyToolRefCount.Load(); got != 1 {
			t.Fatalf("%s 应经同一归一化函数降级 1 条调用，实际 %d", stage, got)
		}
	}
	run("模式切换", func(t *testing.T) (*App, *SessionMeta) { return histScenarioModeSwitch(t) })
	run("子对话继承", func(t *testing.T) (*App, *SessionMeta) {
		a, s, _ := histScenarioInherit(t)
		return a, s
	})
	run("组件开关", func(t *testing.T) (*App, *SessionMeta) { return histScenarioComponent(t) })
}
