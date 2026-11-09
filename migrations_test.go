package main

// 迁移机制单测（migrations.go）：执行记账后不再执行、失败不记账下次重试、
// 按序执行互不影响；正式清单在「已跑过旧版清理的存量机器」上重跑安全（回归）。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestRunMigrationListRecordsAndSkips 账本语义：成功记账、已记账跳过、失败
// 不记账（下次重试）且不阻断后续任务。
func TestRunMigrationListRecordsAndSkips(t *testing.T) {
	a := newSkillTestApp(t)
	ran := map[string]int{}
	list := []appMigration{
		{id: "t1.ok", desc: "成功任务", run: func(a *App) error { ran["t1.ok"]++; return nil }},
		{id: "t2.fail", desc: "失败任务", run: func(a *App) error { ran["t2.fail"]++; return os.ErrPermission }},
		{id: "t3.ok", desc: "失败后的任务", run: func(a *App) error { ran["t3.ok"]++; return nil }},
	}
	a.runMigrationList(list)
	if ran["t1.ok"] != 1 || ran["t3.ok"] != 1 || ran["t2.fail"] != 1 {
		t.Fatalf("执行次数不对（失败任务不应阻断后续）: %#v", ran)
	}
	recs := a.loadMigrationRecs()
	if _, ok := recs["t1.ok"]; !ok {
		t.Fatal("成功任务应记账")
	}
	if _, ok := recs["t2.fail"]; ok {
		t.Fatal("失败任务不应记账（下次要重试）")
	}

	// 第二轮：已记账的不再执行；上轮失败的这轮成功了才记账
	list[1].run = func(a *App) error { ran["t2.fail"]++; return nil }
	a.runMigrationList(list)
	if ran["t1.ok"] != 1 || ran["t3.ok"] != 1 {
		t.Fatalf("已记账任务不应重复执行: %#v", ran)
	}
	if ran["t2.fail"] != 2 {
		t.Fatalf("失败任务应重试: %#v", ran)
	}
	if _, ok := a.loadMigrationRecs()["t2.fail"]; !ok {
		t.Fatal("重试成功后应记账")
	}
}

// TestMigrationsIdempotentOnMigratedMachines 正式清单的回归：模拟「已跑过
// 20261030-5 旧版每次启动清理的机器」（目录已删）再跑迁移清单——安全无事，
// 且已清理过的泄漏技能不会再出现。
func TestMigrationsIdempotentOnMigratedMachines(t *testing.T) {
	a := newSkillTestApp(t)
	userDir, _ := a.userSkillsDir()
	os.MkdirAll(filepath.Join(userDir, "jxmes-master"), 0755)
	os.WriteFile(filepath.Join(userDir, "jxmes-master", "SKILL.md"),
		[]byte("---\nname: x\ndescription: d\n---\n正文"), 0644)

	a.runMigrations() // 第一次：清掉泄漏技能，两个迁移记账
	if dirExists(filepath.Join(userDir, "jxmes-master")) {
		t.Fatal("泄漏技能应被清理")
	}
	recs := a.loadMigrationRecs()
	for _, id := range []string{"20261030.builtin-skill-from-user", "20261030.purge-leaked-skills"} {
		if _, ok := recs[id]; !ok {
			t.Fatalf("迁移 %s 应记账: %#v", id, recs)
		}
	}

	// 第二次启动（新会话场景）：手工再放一个泄漏目录模拟「拷回误操作」，
	// 已记账的迁移不执行 → 目录保留（账本语义优先于重复清理）
	os.MkdirAll(filepath.Join(userDir, "jxmes-master"), 0755)
	a2 := &App{Cfg: a.Cfg} // 同一 DataDir：账本在；新实例：SkillsDir 缓存清零
	a2.runMigrations()
	if !dirExists(filepath.Join(userDir, "jxmes-master")) {
		t.Fatal("已记账的迁移不应重复执行（账本语义）")
	}
}

// TestMigrationRecsRoundTrip 账本时间可序列化（time.Time ↔ JSON RFC3339），重启读回不丢。
func TestMigrationRecsRoundTrip(t *testing.T) {
	a := newSkillTestApp(t)
	at := time.Date(2026, 10, 30, 12, 0, 0, 0, time.Local)
	out, err := json.Marshal(map[string]time.Time{"x": at})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(a.migrationsPath(), out); err != nil {
		t.Fatal(err)
	}
	recs := a.loadMigrationRecs()
	got, ok := recs["x"]
	if !ok || got.Format("2006-01-02 15:04") != at.Format("2006-01-02 15:04") {
		t.Fatalf("账本往返不对: %#v", recs)
	}
}

// plantLeakedSkill 在用户技能目录放一个名单内的泄漏技能（清理任务的目标）。
func plantLeakedSkill(t *testing.T, a *App) string {
	t.Helper()
	userDir, err := a.userSkillsDir()
	if err != nil {
		t.Fatalf("用户技能目录: %v", err)
	}
	target := filepath.Join(userDir, "jxmes-master")
	if err := os.MkdirAll(target, 0755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(target, "SKILL.md"), []byte("---\nname: x\ndescription: d\n---\n正文"), 0644)
	return target
}

// TestMigrationLedgerScopedToSkillsDir 账本域回归（2026-11-09 事故）：开发版 /
// 隔离验证实例各有自己的 dataDir，却共用同一个程序目录的技能目录。账本放数据
// 目录时，每个新实例首跑都会把一次性清理再执行一遍——事故里隔离实例把市场正式
// 安装（主实例有市场记账豁免）的 4 个技能当泄漏技能删掉了。账本改跟技能目录
// 同域后，换 dataDir 的实例读到同一份记账，一个都不会再删。
func TestMigrationLedgerScopedToSkillsDir(t *testing.T) {
	a := newSkillTestApp(t)
	target := plantLeakedSkill(t, a)
	a.runMigrations()
	if dirExists(target) {
		t.Fatal("泄漏技能应被清理")
	}
	if !exists(filepath.Join(a.Cfg.SkillsDir, migrationsFileName)) {
		t.Fatal("账本应落在技能目录（程序目录域）")
	}
	if exists(a.legacyMigrationsPath()) {
		t.Fatal("新版本不该再往数据目录写账本")
	}

	// 另一个实例：独立 dataDir（自己的市场记账与账本），同一个技能目录
	a2 := &App{Cfg: &Config{DataDir: t.TempDir(), SkillsDir: a.Cfg.SkillsDir}}
	target2 := plantLeakedSkill(t, a2)
	a2.runMigrations()
	if !dirExists(target2) {
		t.Fatal("换了 dataDir 的实例不该重跑一次性清理（账本应与技能目录同域）")
	}
}

// TestMigrationLegacyLedgerHonored 升级路径：老版本账本在数据目录
// （data/migrations.json，没有点前缀——当年就是这么写的，别拿新账本的文件名去
// 找它），新版本账本在技能目录。首跑必须认老账本、把老记账搬进本域账本，绝不
// 能让一次性任务重跑一遍。
func TestMigrationLegacyLedgerHonored(t *testing.T) {
	a := newSkillTestApp(t)
	old := map[string]time.Time{}
	for _, m := range appMigrations {
		old[m.id] = time.Now()
	}
	out, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	// 按历史真实路径写（不复用 legacyMigrationsPath：那条路径本身就是要测的）
	legacyPath := filepath.Join(a.Cfg.DataDir, "migrations.json")
	if err := writeFileAtomic(legacyPath, out); err != nil {
		t.Fatal(err)
	}
	target := plantLeakedSkill(t, a)
	a.runMigrations()
	if !dirExists(target) {
		t.Fatal("老账本记过的迁移不应重跑")
	}
	own := a.loadMigrationRecsAt(a.migrationsPath())
	for _, m := range appMigrations {
		if _, ok := own[m.id]; !ok {
			t.Fatalf("老记账 %s 应搬进技能目录账本: %#v", m.id, own)
		}
	}
}
