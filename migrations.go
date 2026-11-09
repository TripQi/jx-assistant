package main

// migrations.go —— 客户端迁移机制（Flyway 式）：版本化一次性任务 + 执行账本。
//
// 每个迁移任务有唯一 id，编译进 exe 随客户端版本分发；执行成功记入账本，
// 已记账的永不再跑。要加新的一次性动作（清理误发文件、目录搬家、格式转换……），
// 往下面 migrations 清单**尾部追加**条目即可——不再写「每次启动全量扫描、
// 靠自身幂等」的散装自愈（历史上的 migrateBuiltinFromUser /
// purgeLeakedUserSkills 就是那种形态：没有账本，说不出哪些执行过、执行到哪了）。
//
// 账本作用域（2026-11-09 事故后定死）：<技能目录>/.migrations.json，**与迁移
// 改动的对象同域**（程序目录级），不再是 data/migrations.json（数据目录级）。
// 数据目录是「实例」域：开发版、隔离验证实例、并行实例各有自己的 dataDir，
// 却共用同一个程序目录的技能目录——账本放数据目录时，每个新实例首跑都会把
// 一次性清理再执行一遍。事故实证：隔离验证实例首跑重跑 purge，把市场正式
// 安装（主实例有市场记账豁免）的 4 个技能当泄漏技能删掉了。
// 数据目录里的旧账本升级后仍读（只读不删）：老账本记着哪些一次性任务早已
// 跑过，丢了就等于让它们再跑一遍。
//
// 与 Flyway 的差异（有意为之）：
//   - 失败不拒绝启动——助手是给人用的，迁移失败最多退化某项功能，不能起不来；
//   - 任务是 Go 代码不是外部脚本文件——单文件交付形态，且不让可执行文件落地；
//   - 没有 undo——迁移只做向前修复，不提供回滚。
//
// 写任务的三条纪律：id 用「日期.短名」（如 20261030.purge-leaked-skills），
// 只追加不改名不插队；任务体尽力而为（内部消化小失败，返回 nil 即视为完成
// 记账）；SkillsDir 等基础设施错误才返回 error（不记账，下次启动重试）。

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"time"
)

// migration 一次性迁移任务。
type appMigration struct {
	id   string // 唯一 id：「日期.短名」，按日期自然有序
	desc string // 人读描述（进日志，账本只存 id 与时间）
	run  func(a *App) error
}

// migrations 迁移任务清单（按序执行；新任务一律追加到尾部）。
var appMigrations = []appMigration{
	{
		id:   "20261030.builtin-skill-from-user",
		desc: "内置技能的 user/ 旧副本挪到技能根（web-search-local 用户技能时代的分发形态）",
		run: func(a *App) error {
			dir, err := a.SkillsDir()
			if err != nil {
				return err
			}
			a.migrateBuiltinFromUser(dir)
			return nil
		},
	},
	{
		id:   "20261030.purge-leaked-skills",
		desc: "删掉旧安装包误分发的内部技能（含密钥；市场正式安装的跳过，名单见 leakedInternalSkills）",
		run: func(a *App) error {
			dir, err := a.SkillsDir()
			if err != nil {
				return err
			}
			a.purgeLeakedUserSkills(dir)
			return nil
		},
	},
	{
		id:   "20261103.builtin-mcp",
		desc: "释放内置 MCP 连接器（cua / windows-mcp）：随包分发、默认可用，受「操作电脑」能力门控",
		run: func(a *App) error {
			return a.materializeBuiltinMCP()
		},
	},
}

// 账本文件名：本域账本放技能目录（点前缀，与同目录的 .skills-version 同款，
// 不会被当成技能目录扫出来）；旧账本在数据目录，名字是当年写下的 migrations.json。
const (
	migrationsFileName       = ".migrations.json"
	legacyMigrationsFileName = "migrations.json"
)

// migrationsPath 本域账本：<技能目录>/.migrations.json。技能目录解析不出来
// （极端：目录不可写且释放失败）才退回数据目录，宁可账本域收窄也不能没有账本。
func (a *App) migrationsPath() string {
	if dir, err := a.SkillsDir(); err == nil {
		return filepath.Join(dir, migrationsFileName)
	}
	return a.legacyMigrationsPath()
}

// legacyMigrationsPath 旧位置（数据目录级）账本：仅供升级机器读取。
func (a *App) legacyMigrationsPath() string {
	return filepath.Join(a.Cfg.DataDir, legacyMigrationsFileName)
}

func (a *App) loadMigrationRecsAt(path string) map[string]time.Time {
	recs := map[string]time.Time{}
	data, err := os.ReadFile(path)
	if err != nil {
		return recs
	}
	_ = json.Unmarshal(data, &recs)
	return recs
}

// loadMigrationRecs 账本合并视图：旧账本先铺底、本域账本覆盖（同 id 以本域为准）。
func (a *App) loadMigrationRecs() map[string]time.Time {
	recs := a.loadMigrationRecsAt(a.legacyMigrationsPath())
	own := a.migrationsPath()
	if own == a.legacyMigrationsPath() {
		return recs
	}
	for id, at := range a.loadMigrationRecsAt(own) {
		recs[id] = at
	}
	return recs
}

// runMigrations 启动入口（NewApp 调用）：跑正式清单。
func (a *App) runMigrations() { a.runMigrationList(appMigrations) }

// runMigrationList 执行器：已记账的跳过；成功的落账，失败的记日志不落账
// （下次启动重试）；单个失败不阻断后续任务。落盘条件：跑了新任务，或旧账本
// 里的记账还没搬进本域账本（升级首跑把老记账搬过来，避免一次性任务重跑）。
func (a *App) runMigrationList(list []appMigration) {
	path := a.migrationsPath()
	recs := a.loadMigrationRecs()
	persisted := a.loadMigrationRecsAt(path)
	changed := false
	for id := range recs {
		if _, ok := persisted[id]; !ok {
			changed = true
			break
		}
	}
	for _, m := range list {
		if _, done := recs[m.id]; done {
			continue
		}
		at := time.Now()
		if err := m.run(a); err != nil {
			log.Printf("[迁移] %s（%s）执行失败，下次启动重试: %v", m.id, m.desc, err)
			continue
		}
		recs[m.id] = at
		changed = true
		log.Printf("[迁移] %s（%s）完成", m.id, m.desc)
	}
	if !changed {
		return
	}
	out, err := json.MarshalIndent(recs, "", "  ")
	if err != nil {
		return
	}
	if err := writeFileAtomic(path, out); err != nil {
		log.Printf("[迁移] 账本落盘失败（%s；未记账的任务下次启动会重跑）: %v", path, err)
	}
}
