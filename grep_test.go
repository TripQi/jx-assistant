package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf16"
)

// TestGrepDecodesGBK 回归：grep 曾把文件字节直接当 UTF-8 解释，GBK 文件里的
// 中文永远匹配不到（read/edit/run_command 都走 encoding.go 探测，只有 grep 漏了）。
// 这里用真实的中文命中验证「先探测解码、再匹配」。
func TestGrepDecodesGBK(t *testing.T) {
	a := newCapTestApp(t, "data-grep-gbk-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "grep"}

	target := filepath.Join(ws, "gbk批处理.bat")
	if err := os.WriteFile(target, gbkBytes(t, "@echo off\r\nrem 中文批处理：执行情况\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 另放一份 UTF-8 文件，确认两种编码在同一轮里都能命中
	utf8File := filepath.Join(ws, "utf8笔记.md")
	if err := os.WriteFile(utf8File, []byte("第一行\n中文批处理：执行情况\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runGrep(tc, json.RawMessage(fmt.Sprintf(`{"pattern":"中文批处理：执行情况","path":%q}`, ws)))
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "gbk批处理.bat:2:") {
		t.Fatalf("GBK 文件里的中文应命中，实得:\n%s", out)
	}
	if !strings.Contains(out, "utf8笔记.md:2:") {
		t.Fatalf("UTF-8 文件里的中文应命中，实得:\n%s", out)
	}
}

// TestGrepExclude 验证 exclude 参数（默认不忽略任何业务目录，全是显式请求）：
// 纯名字剪整棵目录、通配符按名字过滤、含 / 的按相对路径前缀剪枝。
func TestGrepExclude(t *testing.T) {
	a := newCapTestApp(t, "data-grep-exclude-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "grep"}

	files := []string{
		"data/notes.md",
		"src/main.go",
		"src/app.log",
		"gen/deep/x.go",
	}
	for _, rel := range files {
		full := filepath.Join(ws, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("命中词 在这里\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	search := func(args string) string {
		t.Helper()
		out, err := runGrep(tc, json.RawMessage(fmt.Sprintf(`{"pattern":"命中词","path":%q%s}`, ws, args)))
		if err != nil {
			t.Fatalf("grep 失败: %v", err)
		}
		return out
	}

	// 1) 不传 exclude：四个都命中（默认不排除业务目录 data/）
	all := search("")
	for _, rel := range files {
		if !strings.Contains(all, filepath.FromSlash(rel)) && !strings.Contains(all, rel) {
			t.Fatalf("默认应命中 %s，实得:\n%s", rel, all)
		}
	}

	// 2) 纯名字：data 整棵跳过
	out := search(`,"exclude":"data"`)
	if strings.Contains(out, "notes.md") || !strings.Contains(out, "main.go") || !strings.Contains(out, "x.go") {
		t.Fatalf("exclude=data 只应剪掉 data/，实得:\n%s", out)
	}

	// 3) 通配符按名字过滤：*.log
	out = search(`,"exclude":"*.log"`)
	if strings.Contains(out, "app.log") || !strings.Contains(out, "main.go") {
		t.Fatalf("exclude=*.log 只应剪掉日志，实得:\n%s", out)
	}

	// 4) 含 / 的相对路径前缀：gen/deep 连同子目录
	out = search(`,"exclude":"gen/deep"`)
	if strings.Contains(out, "x.go") || !strings.Contains(out, "notes.md") {
		t.Fatalf("exclude=gen/deep 应剪掉 gen/deep 子树，实得:\n%s", out)
	}

	// 5) 多项 + 大小写不敏感
	out = search(`,"exclude":"DATA, Gen"`)
	if strings.Contains(out, "notes.md") || strings.Contains(out, "x.go") || !strings.Contains(out, "main.go") {
		t.Fatalf("exclude=DATA, Gen 应剪掉两棵目录，实得:\n%s", out)
	}
}

// UTF-16 文本必然带 NUL、会被整份误跳；改成按探测结果判二进制后应能搜到，
// 而真正的二进制仍被跳过。
func TestGrepSkipsBinaryButReadsUTF16(t *testing.T) {
	a := newCapTestApp(t, "data-grep-utf16-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "grep"}

	// UTF-16LE 带 BOM 的文本
	u := utf16.Encode([]rune("订单编号：A-1024\r\n"))
	raw := []byte{0xFF, 0xFE}
	for _, v := range u {
		raw = append(raw, byte(v), byte(v>>8))
	}
	if err := os.WriteFile(filepath.Join(ws, "utf16导出.txt"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	// 二进制：含 NUL 且不像 UTF-16 文本，内容里刻意带上可打印串
	bin := append([]byte("MATCHME-二进制标记"), 0x00, 0x01, 0x02, 0xFF)
	if err := os.WriteFile(filepath.Join(ws, "blob.dat"), bin, 0o644); err != nil {
		t.Fatal(err)
	}

	out, err := runGrep(tc, json.RawMessage(fmt.Sprintf(`{"pattern":"订单编号","path":%q}`, ws)))
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if !strings.Contains(out, "utf16导出.txt:1:") {
		t.Fatalf("UTF-16 文本应能命中（不再被当作二进制跳过），实得:\n%s", out)
	}

	out2, err := runGrep(tc, json.RawMessage(fmt.Sprintf(`{"pattern":"MATCHME","path":%q}`, ws)))
	if err != nil {
		t.Fatalf("grep 失败: %v", err)
	}
	if strings.Contains(out2, "blob.dat") {
		t.Fatalf("二进制文件应被跳过，实得:\n%s", out2)
	}
}
