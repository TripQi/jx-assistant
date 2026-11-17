package main

// F6 回归：项目元数据的 CAS 令牌（updated_at）必须在每次有效修改后严格前进。
// 时间源停在原地（同一时钟刻度）或被回拨时也不得复用旧值——否则携带旧令牌的
// 陈旧快照仍会匹配，冲突校验放行、新元数据被旧快照覆盖（丢更新）。
//
// 既有 TestProjectMetadata* 依赖真实时钟刻度竞争是否命中，结果随时序变化（本机
// 实测连续取值的同刻最长连续可达数千次，故偶发失败）；本用例用可控时钟把
// "同刻 / 回拨 / 无变化保存" 做成确定性对照。

import (
	"errors"
	"testing"
	"time"
)

func withProjectClock(t *testing.T, at time.Time) {
	t.Helper()
	old := projectClock
	projectClock = func() time.Time { return at }
	t.Cleanup(func() { projectClock = old })
}

func TestProjectMetadataTokenStrictlyAdvances(t *testing.T) {
	withProjectClock(t, time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC))
	s := projectTestStore(t)
	p := projectTestCreate(t, s, "token")
	before := projectTestKnowledge(t, s, p.ID)

	// 同刻：时钟停在原地，连续两次有效修改也必须换令牌。
	name1 := "first"
	first, err := s.PatchProject(p.ID, &name1, nil, nil, &p.UpdatedAt)
	if err != nil {
		t.Fatalf("同刻首次改名: %v", err)
	}
	if first.UpdatedAt == p.UpdatedAt {
		t.Fatalf("同刻有效修改没有换令牌，陈旧令牌仍会匹配: %q", first.UpdatedAt)
	}
	if _, err := s.PatchProject(p.ID, &name1, nil, nil, &p.UpdatedAt); !errors.Is(err, ErrProjectMetadataConflict) {
		t.Fatalf("陈旧令牌必须冲突（会被放行成丢更新），got %v", err)
	}
	name2 := "second"
	second, err := s.PatchProject(p.ID, &name2, nil, nil, &first.UpdatedAt)
	if err != nil {
		t.Fatalf("同刻第二次改名: %v", err)
	}
	if second.UpdatedAt == first.UpdatedAt {
		t.Fatalf("同刻第二次修改复用了令牌: %q", second.UpdatedAt)
	}

	// 时钟回拨：新令牌仍须晚于上一次，回拨前的令牌照样失效。
	withProjectClock(t, time.Date(2026, 11, 17, 7, 0, 0, 0, time.UTC))
	name3 := "third"
	third, err := s.PatchProject(p.ID, &name3, nil, nil, &second.UpdatedAt)
	if err != nil {
		t.Fatalf("回拨后改名: %v", err)
	}
	prevAt, perr := time.Parse(time.RFC3339Nano, second.UpdatedAt)
	curAt, cerr := time.Parse(time.RFC3339Nano, third.UpdatedAt)
	if perr != nil || cerr != nil || !curAt.After(prevAt) {
		t.Fatalf("回拨后令牌未前进: %q -> %q (err %v/%v)", second.UpdatedAt, third.UpdatedAt, perr, cerr)
	}
	if _, err := s.PatchProject(p.ID, &name1, nil, nil, &second.UpdatedAt); !errors.Is(err, ErrProjectMetadataConflict) {
		t.Fatalf("回拨后的陈旧令牌必须冲突，got %v", err)
	}

	// 无变化保存语义不变：不换令牌、不动知识正文/版本/时间。
	noop, err := s.PatchProject(p.ID, &name3, nil, nil, &third.UpdatedAt)
	if err != nil {
		t.Fatalf("无变化保存: %v", err)
	}
	if noop.UpdatedAt != third.UpdatedAt {
		t.Fatalf("无变化保存不应刷新令牌: %q -> %q", third.UpdatedAt, noop.UpdatedAt)
	}
	if k := projectTestKnowledge(t, s, p.ID); k != before {
		t.Fatal("元数据修改改动了知识版本/正文/时间")
	}
}
