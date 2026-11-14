package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// 原始行复制后，摘要的下界必须独立向上取整，上界仍向下取整；
// 历史工具读取的是原文而不是 Compact，因此折叠不能破坏回查入口。
func TestInheritSummaryRangeHistoryLookup(t *testing.T) {
	for _, tc := range []struct {
		name      string
		from      int64
		wantFrom  int64
		firstText string
		folded    bool
		openTail  bool
	}{
		{"nonzero", 1, 2, "FIRST-原始用户目标", false, false},
		{"audit-start", 2, 3, "MIDDLE-原始详细约束", false, false},
		{"request-start", 3, 3, "MIDDLE-原始详细约束", false, false},
		{"folded-start", 4, 3, "MIDDLE-原始详细约束", true, false},
		{"legacy-zero", 0, 2, "FIRST-原始用户目标", false, false},
		{"beyond-upper-bound", 99, 4, "LAST-原始完成结论", false, false},
		{"trimmed-open-tool-start", 7, 4, "LAST-原始完成结论", false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newCompactTestApp(t)
			for _, id := range []string{"parent-range", "child-range"} {
				if err := a.Store.Create(&SessionMeta{ID: id, Mode: "task", Title: id}); err != nil {
					t.Fatal(err)
				}
			}
			middle := &StoredMessage{Message: TextMessage("user", "MIDDLE-原始详细约束"), Kind: kindChat}
			if tc.folded {
				middle.Compact = "MIDDLE-折叠视图"
			}
			rows := []*StoredMessage{
				{Message: TextMessage("user", "FIRST-原始用户目标"), Kind: kindChat},
				{Message: TextMessage("system", "AUDIT-NOT-INHERITED"), Kind: kindAudit},
				{Message: TextMessage("system", "REQUEST-NOT-INHERITED"), Kind: kindRequest},
				middle,
				{Message: TextMessage("assistant", "LAST-原始完成结论"), Kind: kindChat},
				{Message: TextMessage("system", "SUMMARY-范围测试"), Kind: kindSummary, Origin: "auto", CoversFrom: tc.from, CoversUpTo: 5},
			}
			if tc.openTail {
				rows = append(rows, &StoredMessage{Message: Message{Role: "assistant", Content: "OPEN-不应复制", ToolCalls: []ToolCall{{ID: "not-finished", Type: "function", Function: ToolCallFunc{Name: "read", Arguments: `{}`}}}}, Kind: kindChat})
			}
			for _, row := range rows {
				if err := a.Store.AppendRow("parent-range", row); err != nil {
					t.Fatal(err)
				}
			}
			count, err := a.copyInheritedContext("parent-range", "child-range")
			if err != nil || count != 4 {
				t.Fatalf("copy count=%d err=%v", count, err)
			}
			child, err := a.Store.RawMessagesAll("child-range")
			if err != nil {
				t.Fatal(err)
			}
			var summary *StoredMessage
			for _, row := range child {
				if row.Kind == kindSummary {
					summary = row
				}
				if strings.Contains(row.Text(), "NOT-INHERITED") || strings.Contains(row.Text(), "OPEN-") {
					t.Fatalf("不应复制诊断/半开工具行: %+v", row)
				}
			}
			if summary == nil || summary.CoversFrom != tc.wantFrom || summary.CoversUpTo != 4 {
				t.Fatalf("区间应为[%d,4]，实际摘要=%+v", tc.wantFrom, summary)
			}
			read := func(params map[string]any) string {
				t.Helper()
				params["session_id"] = "child-range"
				args, err := json.Marshal(params)
				if err != nil {
					t.Fatal(err)
				}
				out, err := runReadSessionMessages(&toolContext{app: a}, args)
				if err != nil {
					t.Fatal(err)
				}
				return out
			}
			window := read(map[string]any{"from_seq": summary.CoversFrom, "limit": int(summary.CoversUpTo - summary.CoversFrom + 1), "chars": 20000})
			if !strings.Contains(window, tc.firstText) || !strings.Contains(window, "LAST-原始完成结论") || strings.Contains(window, "MIDDLE-折叠视图") {
				t.Fatalf("按继承区间不能正确回查首尾原文: %s", window)
			}
			for _, endpoint := range []struct {
				seq  int64
				text string
			}{{summary.CoversFrom, tc.firstText}, {summary.CoversUpTo, "LAST-原始完成结论"}} {
				out := read(map[string]any{"seq": endpoint.seq})
				if !strings.Contains(out, endpoint.text) {
					t.Fatalf("单条回查[%d]失败: %s", endpoint.seq, out)
				}
			}
			t.Logf("parent from=%d → child [%d,%d]; history first=%s last=LAST-原始完成结论", tc.from, summary.CoversFrom, summary.CoversUpTo, tc.firstText)
		})
	}
}

// 子会话已有原文时，下界不能错指向子会话旧消息或继承标记。
func TestInheritSummaryRangeWithExistingChild(t *testing.T) {
	a := newCompactTestApp(t)
	for _, id := range []string{"range-parent", "range-child"} {
		if err := a.Store.Create(&SessionMeta{ID: id, Mode: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	appendRow := func(id, role, text, kind string, from, upTo int64) {
		t.Helper()
		if err := a.Store.AppendRow(id, &StoredMessage{Message: TextMessage(role, text), Kind: kind, CoversFrom: from, CoversUpTo: upTo}); err != nil {
			t.Fatal(err)
		}
	}
	appendRow("range-child", "user", "CHILD-原有消息", kindChat, 0, 0)
	appendRow("range-parent", "user", "PARENT-起点", kindChat, 0, 0)
	appendRow("range-parent", "assistant", "PARENT-终点", kindChat, 0, 0)
	appendRow("range-parent", "system", "摘要", kindSummary, 1, 2)
	if _, err := a.copyInheritedContext("range-parent", "range-child"); err != nil {
		t.Fatal(err)
	}
	rows, err := a.Store.RawMessagesAll("range-child")
	if err != nil {
		t.Fatal(err)
	}
	var summary *StoredMessage
	for _, row := range rows {
		if row.Kind == kindSummary {
			summary = row
		}
	}
	if summary == nil || summary.CoversFrom != 3 || summary.CoversUpTo != 4 {
		t.Fatalf("已有child消息的区间应为[3,4]: %+v", summary)
	}
	args := json.RawMessage(fmt.Sprintf(`{"session_id":"range-child","from_seq":%d,"limit":2,"chars":20000}`, summary.CoversFrom))
	out, err := runReadSessionMessages(&toolContext{app: a}, args)
	if err != nil || !strings.Contains(out, "PARENT-起点") || !strings.Contains(out, "PARENT-终点") || strings.Contains(out, "CHILD-原有消息") {
		t.Fatalf("回查错位: %s err=%v", out, err)
	}
}
