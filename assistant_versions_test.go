package main

// 长期助理主对话「版本」机制的测试（assistant_versions.go）：
//  1. 升级兼容：老库里那个 assistant 主会话就是第一版，历史/快照/交付原样留着，
//     第一次列版本时自动成为「当前版本」。
//  2. 新建一版：老版本数据一条不动，新版本空历史、继承配置、编号递增且稳定。
//  3. 切换：只动指针，落库持久化（换一个 App 实例读同一个库也还是那一版）；
//     切回老版本后接着聊也不会把它顶成「最新」。
//  4. 压缩/清空这两个动作只作用于当前版本（不再按「最近更新的那个」猜）。
//  5. 还没有任何一版时，定时任务/任务求助那条路（assistantSession）建出第一版。

import (
	"testing"
)

// assistantVersionFixture 造一个「升级前」的库：只有一个数字助理主会话，
// 里面有一轮对话、一条快照登记、一条交付登记，且没有任何版本号与当前指针。
func assistantVersionFixture(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Workspace = t.TempDir() // 建会话时会预建 .jxai/tmp，别落到仓库里
	legacy := &SessionMeta{
		ID: "legacy", Title: "数字助理主对话", Mode: "assistant",
		Workspace: a.Cfg.Workspace, Permission: permFull, Provider: "p1", Model: "m1",
		CreatedAt: "2026-01-01 09:00:00", UpdatedAt: "2026-01-05 10:00:00",
	}
	if err := a.Store.Create(legacy); err != nil {
		t.Fatalf("建老主对话失败: %v", err)
	}
	seedRound(t, a, legacy.ID, "旧对话", true)
	if _, err := a.Store.AppendCheckpoint(&Checkpoint{
		SessionID: legacy.ID, Seq: 2, ToolName: "write", Path: "old.txt",
		ExistedBefore: true, BeforeSize: 5, CreatedAt: now(),
	}); err != nil {
		t.Fatalf("落快照失败: %v", err)
	}
	if err := a.Store.AddDelivery(&Delivery{
		SessionID: legacy.ID, Seq: 4, Path: "old-out.txt", Note: "旧成果", CreatedAt: now(),
	}); err != nil {
		t.Fatalf("落交付失败: %v", err)
	}
	return a, legacy
}

// versionOf 取清单里某一版的条目（没有返回 nil）。
func versionOf(t *testing.T, a *App, id string) *AssistantVersion {
	t.Helper()
	list, err := a.assistantVersions()
	if err != nil {
		t.Fatalf("列版本失败: %v", err)
	}
	for _, v := range list {
		if v.SessionID == id {
			return v
		}
	}
	return nil
}

// 升级兼容：老会话 = 第一版，数据原样，且自动成为当前版本。
func TestAssistantVersionsLegacySessionIsFirstVersion(t *testing.T) {
	a, legacy := assistantVersionFixture(t)

	v := versionOf(t, a, legacy.ID)
	if v == nil {
		t.Fatal("老主对话应出现在版本清单里")
	}
	if v.Version != 1 {
		t.Fatalf("老主对话应补成第一版, got %d", v.Version)
	}
	if !v.Current {
		t.Fatal("库里只有这一版时，它应当就是当前版本")
	}
	if v.Messages != 4 {
		t.Fatalf("版本清单的消息条数应等于真实历史条数(4), got %d", v.Messages)
	}
	if cur := a.currentAssistant(); cur == nil || cur.ID != legacy.ID {
		t.Fatalf("当前版本应是老主对话, got %+v", cur)
	}
	// 老数据一条不动
	if n, _ := a.Store.MessageCount(legacy.ID); n != 4 {
		t.Fatalf("老历史不该被动, got %d 条", n)
	}
	if cps, _ := a.Store.CheckpointsAfter(legacy.ID, 0); len(cps) != 1 {
		t.Fatalf("老快照登记不该被动, got %d", len(cps))
	}
	if dvs, _ := a.Store.DeliveriesFor(legacy.ID); len(dvs) != 1 {
		t.Fatalf("老交付登记不该被动, got %d", len(dvs))
	}
	// 版本号落库：再列一次不会换号
	if n := versionOf(t, a, legacy.ID).Version; n != 1 {
		t.Fatalf("版本号应稳定为 1, got %d", n)
	}
}

// 新建一版：老版本不动、新版本空历史并继承配置、编号递增、指针指向新版本。
func TestAssistantVersionNewStartsFreshAndKeepsOld(t *testing.T) {
	a, legacy := assistantVersionFixture(t)
	a.currentAssistant() // 先让老版本落成第一版与当前版本

	v, err := a.createAssistantVersion()
	if err != nil {
		t.Fatalf("新建一版失败: %v", err)
	}
	if v.Version != 2 {
		t.Fatalf("新版本应是第二版, got %d", v.Version)
	}
	if !v.Current {
		t.Fatal("新建后当前版本应是新版本")
	}
	m, err := a.Store.Get(v.SessionID)
	if err != nil {
		t.Fatalf("读新版本会话失败: %v", err)
	}
	if m.Mode != "assistant" || m.ParentID != "" {
		t.Fatalf("每一版都是顶层 assistant 会话, got mode=%q parent=%q", m.Mode, m.ParentID)
	}
	if n, _ := a.Store.MessageCount(m.ID); n != 0 {
		t.Fatalf("新版本应从零开始（没有消息）, got %d", n)
	}
	// 配置沿用当前版本（换版本只为重新开始聊，不该让用户重设一遍）
	if m.Workspace != legacy.Workspace || m.Permission != legacy.Permission ||
		m.Provider != legacy.Provider || m.Model != legacy.Model {
		t.Fatalf("新版本应继承当前版本的配置: %+v（老版本 %+v）", m, legacy)
	}
	// 老版本一条不动
	if n, _ := a.Store.MessageCount(legacy.ID); n != 4 {
		t.Fatalf("老版本历史不该被动, got %d 条", n)
	}
	if cps, _ := a.Store.CheckpointsAfter(legacy.ID, 0); len(cps) != 1 {
		t.Fatalf("老版本快照登记不该被动, got %d", len(cps))
	}
	if dvs, _ := a.Store.DeliveriesFor(legacy.ID); len(dvs) != 1 {
		t.Fatalf("老版本交付登记不该被动, got %d", len(dvs))
	}
	// 编号：1、2，且各自稳定
	if got := versionOf(t, a, legacy.ID); got == nil || got.Version != 1 || got.Current {
		t.Fatalf("老版本应是「第 1 版、非当前」, got %+v", got)
	}
	if list, _ := a.assistantVersions(); len(list) != 2 || list[0].Version != 1 || list[1].Version != 2 {
		t.Fatalf("版本清单应是 1、2 两版, got %+v", list)
	}
}

// 切换：只动指针；落库持久化；切回老版本接着聊不会把它顶成「最新那一版」。
func TestAssistantVersionSwitchPersistsAndStays(t *testing.T) {
	a, legacy := assistantVersionFixture(t)
	a.currentAssistant()
	v2, err := a.createAssistantVersion()
	if err != nil {
		t.Fatalf("新建一版失败: %v", err)
	}

	back, err := a.switchAssistantVersion(legacy.ID)
	if err != nil {
		t.Fatalf("切回第一版失败: %v", err)
	}
	if back.Version != 1 || !back.Current {
		t.Fatalf("切换响应应是「第 1 版、当前」, got %+v", back)
	}
	if cur := a.currentAssistant(); cur == nil || cur.ID != legacy.ID {
		t.Fatalf("当前版本应已切回第一版, got %+v", cur)
	}
	// 两边的历史都不动
	if n, _ := a.Store.MessageCount(legacy.ID); n != 4 {
		t.Fatalf("第一版历史不该被动, got %d", n)
	}
	if n, _ := a.Store.MessageCount(v2.SessionID); n != 0 {
		t.Fatalf("第二版历史不该被动, got %d", n)
	}
	// 落库持久化：换一个 App 实例读同一个库（相当于重启软件）仍停在这一版
	restarted := &App{Cfg: a.Cfg, Store: a.Store}
	if cur := restarted.currentAssistant(); cur == nil || cur.ID != legacy.ID {
		t.Fatalf("重启后应仍停在用户上次选中的版本, got %+v", cur)
	}
	// 切回老版本后又聊了一轮（更新时间变成最新）——当前版本仍是它，且不能因此
	// 变成「第二版」或把新版顶掉（这正是老实现按更新时间取第一个会错的地方）
	seedRound(t, a, legacy.ID, "新的一轮", false)
	list, err := a.assistantVersions()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].SessionID != legacy.ID || list[0].Version != 1 || !list[0].Current {
		t.Fatalf("接着聊之后仍应是「第 1 版、当前」, got %+v", list)
	}
	if list[1].Version != 2 || list[1].Current {
		t.Fatalf("第二版仍是第 2 版且非当前, got %+v", list[1])
	}
}

// 切换只接受数字助理主对话的某一版：别的会话、不存在的 id 一律拒绝且不改指针。
func TestAssistantVersionSwitchRejectsForeignSession(t *testing.T) {
	a, legacy := assistantVersionFixture(t)
	a.currentAssistant()
	task := &SessionMeta{ID: "task1", Title: "工作任务", Mode: "task",
		Workspace: a.Cfg.Workspace, Permission: permWorkspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(task); err != nil {
		t.Fatal(err)
	}
	if _, err := a.switchAssistantVersion(task.ID); err == nil {
		t.Fatal("不该能切到一个普通任务会话")
	}
	if _, err := a.switchAssistantVersion("no-such-id"); err == nil {
		t.Fatal("不该能切到不存在的会话")
	}
	if a.currentAssistantID() != legacy.ID {
		t.Fatalf("被拒绝的切换不该改动当前版本指针, got %q", a.currentAssistantID())
	}
}

// 压缩/清空只作用于当前版本：切到第二版后清空，第一版历史原样。
func TestAssistantOpsActOnCurrentVersionOnly(t *testing.T) {
	a, legacy := assistantVersionFixture(t)
	a.currentAssistant()
	v2, err := a.createAssistantVersion()
	if err != nil {
		t.Fatalf("新建一版失败: %v", err)
	}
	seedRound(t, a, v2.SessionID, "第二版的一轮", false)

	w := post(t, a.handleAssistantClear, "/api/assistant/clear")
	if w.Code != 200 {
		t.Fatalf("清空当前版本应成功, got %d %s", w.Code, w.Body.String())
	}
	if n, _ := a.Store.MessageCount(v2.SessionID); n != 0 {
		t.Fatalf("当前版本（第二版）应被清空, got %d 条", n)
	}
	if n, _ := a.Store.MessageCount(legacy.ID); n != 4 {
		t.Fatalf("非当前版本（第一版）不该被动, got %d 条", n)
	}
	// 清空不影响版本号与指针
	if v := versionOf(t, a, v2.SessionID); v == nil || v.Version != 2 || !v.Current {
		t.Fatalf("清空后仍是「第 2 版、当前」, got %+v", v)
	}
}

// 还没有任何一版时：定时任务转交/任务求助那条路建出第一版，并落当前指针。
func TestAssistantSessionCreatesFirstVersion(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Workspace = t.TempDir()

	m, err := a.assistantSession()
	if err != nil {
		t.Fatalf("取/建数字助理主对话失败: %v", err)
	}
	if m.Mode != "assistant" || m.ParentID != "" || m.Workspace != a.Cfg.Workspace {
		t.Fatalf("新建的主对话字段不对: %+v", m)
	}
	v := versionOf(t, a, m.ID)
	if v == nil || v.Version != 1 || !v.Current {
		t.Fatalf("应是「第 1 版、当前」, got %+v", v)
	}
	// 再取一次还是同一版（不重复建）
	m2, err := a.assistantSession()
	if err != nil || m2.ID != m.ID {
		t.Fatalf("应复用同一版, got %v err=%v", m2, err)
	}
	if list, _ := a.assistantVersions(); len(list) != 1 {
		t.Fatalf("不该重复建版本, got %d 版", len(list))
	}
}
