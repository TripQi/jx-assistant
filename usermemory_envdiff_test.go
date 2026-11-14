package main

// 「组件开关 → 环境快照差分」闭环测试（2026-11-09 排障：关掉 user-memory 组件后
// 长期助理主对话没收到任何环境提示词变更通知）。
//
// 设计意图：记忆块的「记忆维护」条目（组件提供的读写入口：call_api / 记忆能力）
// 与四个内容字段无关——**组件启用就注入，记忆内容填没填都一样**。只有这样
// 「关组件」在快照里才表现为条目移除，差分引擎才有东西可减（发【移除】变更清单）。
// 此前记忆块以「四字段有内容」为出现条件，全局记忆全空时快照里根本没有这一块，
// 关组件后快照逐字节不变（envHash 不变）→ 不落新 env 行 → 下轮注入什么都没有，
// 用户只看到了工具目录变更、看不到环境变化。
// 三步闭环：开（空记忆也要有维护条目）→ 关（下轮注入收到移除清单）→ 再开（新增回来）。

import (
	"strings"
	"testing"
)

// envDiffsOnNextAssemble 走一遍真实链路（envPrompt → 落库 diff → 组装渲染），
// 返回本轮组装出的全部环境更新消息（历史追加式：老差分仍在，末条是最新的）。
func envDiffsOnNextAssemble(a *App, sess *SessionMeta) []string {
	a.maybeAppendEnvSnapshot(sess, a.envPrompt(sess), nil)
	out := []string{}
	for _, m := range a.buildContext(sess, a.systemPrompt(sess), 0) {
		if strings.HasPrefix(m.Text(), "# 环境更新") {
			out = append(out, m.Text())
		}
	}
	return out
}

// envRowCount 数会话里已落库的环境快照行（kind=env）。
func envRowCount(t *testing.T, a *App, sess *SessionMeta) int {
	t.Helper()
	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range raw {
		if m.Kind == "env" {
			n++
		}
	}
	return n
}

// memoryBlockLine 取环境快照里以 prefix 开头的那条记忆块条目（整行原文）——
// 断言用它，措辞改动不会连带改测试。
func memoryBlockLine(t *testing.T, env, prefix string) string {
	t.Helper()
	for _, ln := range strings.Split(env, "\n") {
		if strings.HasPrefix(ln, prefix) {
			return ln
		}
	}
	t.Fatalf("环境快照里找不到 %q 开头的记忆块条目:\n%s", prefix, env)
	return ""
}

func TestUserMemoryEnvDiffToggleLoop(t *testing.T) {
	cases := []struct {
		name   string
		mem    UserMemoryData
		prefix string // 该场景下用于断言差分的记忆块条目前缀
	}{
		// 空记忆：块靠「记忆维护」条目存在（内容字段一条都不注入）
		{"记忆全空（故障现场）", UserMemoryData{}, "- 记忆维护"},
		// 有内容：内容条目在，差分以内容条目断言
		{"记忆有内容", UserMemoryData{UserName: "张总", Memory: "做制造业"}, "- 用户的名字："},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a, sessA, _ := newUserMemoryTestApp(t)
			sess := &SessionMeta{ID: sessA.ID, Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace}
			if _, err := a.saveUserMemory(c.mem); err != nil {
				t.Fatal(err)
			}

			// 基线：组件关闭状态下先落一条无记忆块的快照，下面的「开」才有差分可比
			if err := a.Components.SetEnabled(userMemoryComponentID, false); err != nil {
				t.Fatal(err)
			}
			if env := a.envPrompt(sess); strings.Contains(env, "# 用户记忆") {
				t.Fatalf("组件关闭时环境快照不应有记忆块:\n%s", env)
			}
			if d := envDiffsOnNextAssemble(a, sess); len(d) != 0 {
				t.Fatalf("首条快照是整份基线，不该有差分消息: %v", d)
			}
			baseRows := envRowCount(t, a, sess)
			if baseRows != 1 {
				t.Fatalf("基线应恰好落一条 env 行，实际 %d", baseRows)
			}

			// ① 开：快照里出现记忆块——空记忆也有块（维护条目与内容无关），
			//    但不注入任何内容条目；落新快照行，下轮注入收到新增
			if err := a.Components.SetEnabled(userMemoryComponentID, true); err != nil {
				t.Fatal(err)
			}
			envOn := a.envPrompt(sess)
			mark := memoryBlockLine(t, envOn, c.prefix)
			if c.mem == (UserMemoryData{}) {
				for _, bad := range []string{"- 用户的名字：", "- 你的名字（", "- 用户定义的你的身份：", "尚未填写"} {
					if strings.Contains(envOn, bad) {
						t.Fatalf("空记忆不该注入内容条目 %q:\n%s", bad, envOn)
					}
				}
			}
			onDiffs := envDiffsOnNextAssemble(a, sess)
			if len(onDiffs) != 1 || !strings.Contains(onDiffs[0], mark) {
				t.Fatalf("开启组件后下轮注入应收到含 %q 的环境更新:\n%v", mark, onDiffs)
			}
			if strings.Contains(onDiffs[0], "【移除】") {
				t.Fatalf("开启组件属纯新增，不该有【移除】段:\n%s", onDiffs[0])
			}
			onRows := envRowCount(t, a, sess)
			if onRows != baseRows+1 {
				t.Fatalf("开启组件应落一条新快照行，实际 %d → %d", baseRows, onRows)
			}

			// ② 关：记忆块整块消失，差分引擎发出【移除】变更清单（本次故障的核心断言）
			if err := a.Components.SetEnabled(userMemoryComponentID, false); err != nil {
				t.Fatal(err)
			}
			offDiffs := envDiffsOnNextAssemble(a, sess)
			if len(offDiffs) != 2 {
				t.Fatalf("关闭组件后应多出一条环境更新，实际 %d 条", len(offDiffs))
			}
			last := offDiffs[len(offDiffs)-1]
			if !strings.HasPrefix(last, "# 环境更新（条目有增删/变更") {
				t.Fatalf("关闭组件后下轮注入应是增删变更清单:\n%s", last)
			}
			if !strings.Contains(last, "【移除】"+mark) {
				t.Fatalf("变更清单应【移除】记忆块条目 %q:\n%s", mark, last)
			}
			offRows := envRowCount(t, a, sess)
			if offRows != onRows+1 {
				t.Fatalf("关闭组件应落一条新快照行，实际 %d → %d", onRows, offRows)
			}

			// 关着再组装一轮：状态没再变 → 不落新行、也不多出新的差分（等值比较兜底）
			if again := envDiffsOnNextAssemble(a, sess); len(again) != len(offDiffs) {
				t.Fatalf("状态未再变化时不该多出环境更新，实际 %d → %d", len(offDiffs), len(again))
			}
			if n := envRowCount(t, a, sess); n != offRows {
				t.Fatalf("状态未再变化时不该落新快照行，实际 %d → %d", offRows, n)
			}

			// ③ 再开：条目重新回到快照（闭环），落新行
			if err := a.Components.SetEnabled(userMemoryComponentID, true); err != nil {
				t.Fatal(err)
			}
			backDiffs := envDiffsOnNextAssemble(a, sess)
			if len(backDiffs) != len(offDiffs)+1 {
				t.Fatalf("再开组件后应多出一条环境更新，实际 %d 条", len(backDiffs))
			}
			back := backDiffs[len(backDiffs)-1]
			if !strings.Contains(back, mark) {
				t.Fatalf("再开组件后下轮注入应收到含 %q 的环境更新:\n%s", mark, back)
			}
			if strings.Contains(back, "【移除】") {
				t.Fatalf("再开属纯新增，不该有【移除】段:\n%s", back)
			}
			if n := envRowCount(t, a, sess); n != offRows+1 {
				t.Fatalf("再开组件应落一条新快照行，实际 %d → %d", offRows, n)
			}
		})
	}
}

// 会话级开关覆盖同款闭环：本会话把组件关掉（全局仍开）——该会话下轮注入收到
// 移除清单，别的会话不受影响。开关覆盖是「一个对话一套配置」的主要入口，
// 不能只测全局档。空记忆起步（维护条目即块的存在依据）。
func TestUserMemoryEnvDiffSessionOverride(t *testing.T) {
	a, sessA, sessB := newUserMemoryTestApp(t)
	viewA := &SessionMeta{ID: sessA.ID, Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace}
	viewB := &SessionMeta{ID: sessB.ID, Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace}
	// 基线（全局开、记忆全空）：两个会话各落一条含维护条目的快照
	envDiffsOnNextAssemble(a, viewA)
	envDiffsOnNextAssemble(a, viewB)
	mark := memoryBlockLine(t, a.envPrompt(viewA), "- 记忆维护")

	// A 会话关掉组件：A 收到移除清单，B 没有任何环境更新
	a.cxoSetComponent(sessA.ID, userMemoryComponentID, false)
	diffA := envDiffsOnNextAssemble(a, viewA)
	if len(diffA) != 1 || !strings.Contains(diffA[0], "【移除】"+mark) {
		t.Fatalf("A 会话关掉组件后应收到的移除清单缺记忆维护条目:\n%v", diffA)
	}
	if diffB := envDiffsOnNextAssemble(a, viewB); len(diffB) != 0 {
		t.Fatalf("B 会话没动开关，不该收到环境更新:\n%v", diffB)
	}
	// 恢复跟随：A 重新收到记忆条目
	a.cxoSetComponent(sessA.ID, userMemoryComponentID, true)
	back := envDiffsOnNextAssemble(a, viewA)
	if len(back) != 2 || !strings.Contains(back[1], mark) || strings.Contains(back[1], "【移除】") {
		t.Fatalf("A 会话恢复组件后应以新增回到快照:\n%v", back)
	}
}
