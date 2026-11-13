package main

// edit 行尾等价回归测试（2026-10-17 会话反馈的实际故障）：
// read 给模型的内容已把 CRLF 归一为 LF，模型按所见写的 old_string 自然是
// LF——edit 若在原始字节上逐字匹配，跨行片段因每个换行差一个 \r 而永远
// 对不上，Windows 项目上必炸。本文件锁定五个行为：
//  1. LF 原文可匹配 CRLF 文件（跨多行），写回保持 CRLF；
//  2. LF 文件编辑后仍是 LF（不被翻成 CRLF）；
//  3. 模型给了带 \r\n 的 new_string 时不会产生 \r\r\n 双重化；
//  4. detectFileLineEndings 按多数派判主导风格；
//  5. CRLF 文件上仍不匹配时，报错带行尾等价提示（给模型排障线索）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// editTestCtx 建一个 10 秒超时的工具上下文（批量 edit 的用例共用）。
func editTestCtx(t *testing.T, a *App, sess *SessionMeta) *toolContext {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return &toolContext{ctx: ctx, app: a, sess: sess, toolName: "edit"}
}

// runEditArgs 用**批量形态**（edits 数组）执行一条修改，返回工具输出。
// 逐条失败不再让整次调用报错（该条零改动、其余照常生效），所以失败要看输出里的 ❌。
func runEditArgs(t *testing.T, tc *toolContext, args map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"edits": []any{args}})
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	out, err := runEditFile(tc, raw)
	if err != nil {
		t.Fatalf("批量 edit 不该因单条失败而报错: %v", err)
	}
	return out
}

// editSucceeded 判断一条修改是否生效（输出里是 ✅ 行而不是 ❌ 行）。
func editSucceeded(out string) bool {
	return strings.Contains(out, editOKMark) && !strings.Contains(out, editFailMark)
}

func TestEditLFQueryMatchesCRLFFile(t *testing.T) {
	a := newCapTestApp(t, "data-edit-crlf-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-10-17 23:00:00"}
	mustCreateSession(t, a, sess)

	target := filepath.Join(ws, "demo.cs")
	if err := os.WriteFile(target, []byte("line1\r\nold A\r\nold B\r\nline4\r\n"), 0o644); err != nil {
		t.Fatalf("预置 CRLF 文件失败: %v", err)
	}

	tc := editTestCtx(t, a, sess)
	// 模型按 read 所见写 LF 跨行原文——旧实现在这里必报「不存在」
	if out := runEditArgs(t, tc, map[string]any{
		"path": target, "old_string": "old A\nold B", "new_string": "new A\nnew B",
	}); !editSucceeded(out) {
		t.Fatalf("LF 原文应能匹配 CRLF 文件: %q", out)
	}
	got, _ := os.ReadFile(target)
	want := "line1\r\nnew A\r\nnew B\r\nline4\r\n"
	if string(got) != want {
		t.Fatalf("写回应保持原 CRLF 风格:\n got=%q\nwant=%q", got, want)
	}
}

func TestEditLFFileStaysLF(t *testing.T) {
	a := newCapTestApp(t, "data-edit-lf-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-10-17 23:00:00"}
	mustCreateSession(t, a, sess)

	target := filepath.Join(ws, "demo.go")
	if err := os.WriteFile(target, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatalf("预置 LF 文件失败: %v", err)
	}

	tc := editTestCtx(t, a, sess)
	if out := runEditArgs(t, tc, map[string]any{
		"path": target, "old_string": "b\nc", "new_string": "B\nC",
	}); !editSucceeded(out) {
		t.Fatalf("LF 文件编辑失败: %q", out)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "a\nB\nC\n" {
		t.Fatalf("LF 文件不应被翻成 CRLF: got=%q", got)
	}
}

func TestEditCRLFNewStringNoDoubleCR(t *testing.T) {
	a := newCapTestApp(t, "data-edit-crlf2-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-10-17 23:00:00"}
	mustCreateSession(t, a, sess)

	target := filepath.Join(ws, "demo2.cs")
	if err := os.WriteFile(target, []byte("a\r\nb\r\n"), 0o644); err != nil {
		t.Fatalf("预置 CRLF 文件失败: %v", err)
	}

	tc := editTestCtx(t, a, sess)
	// 模型给的 new_string 自己带了 \r\n（少见但合法），写回不得产生 \r\r\n
	if out := runEditArgs(t, tc, map[string]any{
		"path": target, "old_string": "a\nb", "new_string": "X\r\nY",
	}); !editSucceeded(out) {
		t.Fatalf("编辑失败: %q", out)
	}
	got, _ := os.ReadFile(target)
	if string(got) != "X\r\nY\r\n" || strings.Contains(string(got), "\r\r\n") {
		t.Fatalf("写回应为 CRLF 且无双重化: got=%q", got)
	}
}

func TestDetectFileLineEndings(t *testing.T) {
	cases := []struct{ in, want string }{
		{"a\r\nb\r\n", "\r\n"},
		{"a\nb\n", "\n"},
		{"a\nb\nc\r\nd\n", "\n"},       // 混合：LF 占多数
		{"a\r\nb\r\nc\nd\r\n", "\r\n"}, // 混合：CRLF 占多数
		{"", "\n"},
	}
	for _, c := range cases {
		if got := detectFileLineEndings(c.in); got != c.want {
			t.Fatalf("detectFileLineEndings(%q)=%q, want %q", c.in, got, c.want)
		}
	}
}

func TestEditCRLFNotFoundHint(t *testing.T) {
	a := newCapTestApp(t, "data-edit-hint-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-10-17 23:00:00"}
	mustCreateSession(t, a, sess)

	crlfFile := filepath.Join(ws, "a.cs")
	os.WriteFile(crlfFile, []byte("x\r\ny\r\n"), 0o644)
	lfFile := filepath.Join(ws, "b.go")
	os.WriteFile(lfFile, []byte("x\ny\n"), 0o644)

	tc := editTestCtx(t, a, sess)
	// CRLF 文件：失败原因应带行尾等价提示，帮模型排除行尾因素
	out := runEditArgs(t, tc, map[string]any{
		"path": crlfFile, "old_string": "nope", "new_string": "z",
	})
	if !strings.Contains(out, editFailMark) || !strings.Contains(out, "行尾等价") {
		t.Fatalf("CRLF 文件不匹配应带行尾等价提示: %q", out)
	}
	// LF 文件：保持原失败文案，不加噪音
	out = runEditArgs(t, tc, map[string]any{
		"path": lfFile, "old_string": "nope", "new_string": "z",
	})
	if !strings.Contains(out, "old_string 未找到") || strings.Contains(out, "行尾等价") {
		t.Fatalf("LF 文件不匹配应保持原失败文案: %q", out)
	}
}
