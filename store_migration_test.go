package main

// 库结构迁移的两条 0016 共存看护（上游同步·压缩块）：上游的
// 0016_message_covers_from 与本仓库的 0016_thinking_six_levels 同号不同串，
// 迁移账本（schema_migration）按**全串 id** 记账，所以两条都必须留在清单里、
// 按编号邻着排：删任一条，老库就少一列（covers_from 少了摘要区间检索失效）。

import "testing"

// 两条 0016 都在清单里（谁把哪条合并/改名了，这里立刻红）。
func TestBoth0016MigrationsCoexist(t *testing.T) {
	ids := map[string]bool{}
	for _, m := range storeMigrations {
		ids[m.id] = true
	}
	for _, want := range []string{"0016_message_covers_from", "0016_thinking_six_levels"} {
		if !ids[want] {
			t.Fatalf("迁移清单缺 %s（同号两条都必须保留）", want)
		}
	}
}

// 老库升级路径：模拟「早就跑过 0016_thinking_six_levels、covers_from 还没加」
// 的库（撤列 + 撤那条记账），重开时只补跑缺的那条——列补回来、两条记账都在，
// 已应用的迁移不重跑（重跑 ALTER 会因列已存在报错，正好也是这条的探针）。
func TestOldDBGetsCoversFromOnUpgrade(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir)
	if err != nil {
		t.Fatalf("建库失败: %v", err)
	}
	if _, err := s.db.Exec(`ALTER TABLE messages DROP COLUMN covers_from`); err != nil {
		t.Fatalf("模拟老库失败（DROP COLUMN）: %v", err)
	}
	if _, err := s.db.Exec(`DELETE FROM schema_migration WHERE id='0016_message_covers_from'`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s2, err := NewStore(dir)
	if err != nil {
		t.Fatalf("老库升级重开失败: %v", err)
	}
	defer s2.Close()
	if _, err := s2.db.Exec(`SELECT covers_from FROM messages LIMIT 1`); err != nil {
		t.Fatalf("covers_from 列没补回来: %v", err)
	}
	var cnt int
	if err := s2.db.QueryRow(`SELECT COUNT(*) FROM schema_migration
		WHERE id IN ('0016_message_covers_from','0016_thinking_six_levels')`).Scan(&cnt); err != nil {
		t.Fatal(err)
	}
	if cnt != 2 {
		t.Fatalf("两条 0016 都应记账, got %d", cnt)
	}
}
