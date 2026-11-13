package main

// 批量 read / edit（唯一形态：files / edits 数组）的行为锁定。这里逐条盯住
// 任务书里的语义，任何一条被改动都会在这里炸：
//  1. 同一文件的多条按输入顺序生效（第 2 条基于第 1 条改完的内容匹配）；
//  2. 某条失败则该条零改动、其余条照常生效，且**已成功的条目不回滚**；
//  3. old_string 多次出现：没设 replace_all 就失败，设了就替换全部；
//  4. 同一文件最后只写一次（快照也只落一条）；
//  5. 编码保持：GBK 文件改完仍是 GBK；emoji 写进 GBK → 只有该条失败；
//  6. 超过 5 条、空数组都直接报错（不静默截断）；
//  7. read 整批输出按预算「填满即停」并明说第几条及之后未返回；
//  8. read 单条失败（文件不存在）不影响同批其它条，同一文件可给多个区间；
//  9. 旧单条入参仍可用（run_python 的 jx.call 桥、老脚本的内部兼容兜底）；
// 10. 文件级信息（编码保持/回读异常/整文件失败）用 ↳ 单独成行，条目行不挂它；
// 11. read 的行数与编辑器一致（末尾换行不多算一行），offset 与行号一致。
//
// 另外盯一眼折叠视图：部分条失败时不能笼统显示「编辑成功」。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// batchTestCtx 建一个批量用例的工具上下文（10 秒超时）。
func batchTestCtx(t *testing.T, a *App, sess *SessionMeta, tool string) *toolContext {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return &toolContext{ctx: ctx, app: a, sess: sess, toolName: tool}
}

// batchTestApp 建带临时库与测试工作区的 App，返回 App 与会话。
func batchTestApp(t *testing.T, dir string) (*App, *SessionMeta) {
	t.Helper()
	a := newCapTestApp(t, dir)
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-11-13 10:00:00"}
	mustCreateSession(t, a, sess)
	return a, sess
}

// runEditBatch 用批量形态执行一组修改，返回输出（结构性错误才返回 error）。
func runEditBatch(t *testing.T, tc *toolContext, items ...map[string]any) (string, error) {
	t.Helper()
	arr := make([]any, 0, len(items))
	for _, it := range items {
		arr = append(arr, it)
	}
	raw, err := json.Marshal(map[string]any{"edits": arr})
	if err != nil {
		t.Fatalf("构造参数失败: %v", err)
	}
	return runEditFile(tc, raw)
}

func mustReadFileText(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读文件失败: %v", err)
	}
	return string(b)
}

// ---------- 1. 同一文件多条按序生效，且只写一次盘 ----------

func TestEditBatchSameFileInOrder(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-order-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "order.go")
	if err := os.WriteFile(target, []byte("a\nb\nc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := batchTestCtx(t, a, sess, "edit")
	out, err := runEditBatch(t, tc,
		map[string]any{"path": target, "old_string": "a", "new_string": "a1"},
		// 第 2 条基于第 1 条改完的内容匹配（"a" 已被改成 "a1"）
		map[string]any{"path": target, "old_string": "a1", "new_string": "A"},
	)
	if err != nil {
		t.Fatalf("批量 edit 失败: %v", err)
	}
	if strings.Contains(out, editFailMark) {
		t.Fatalf("两条都该成功: %q", out)
	}
	if got := mustReadFileText(t, target); got != "A\nb\nc\n" {
		t.Fatalf("应按序生效（a→a1→A）: %q", got)
	}
	// 同一文件只写一次盘 → 只应有一条快照
	cps, cerr := a.Store.CheckpointsAfter(sess.ID, -1)
	if cerr != nil {
		t.Fatalf("读快照失败: %v", cerr)
	}
	if len(cps) != 1 {
		t.Fatalf("同一文件应只写一次盘（快照 1 条），实得 %d 条", len(cps))
	}
	// 成功行带文件:行号与替换处数
	if !strings.Contains(out, "order.go:1 已替换 1 处") {
		t.Fatalf("成功行应含「文件:行号 已替换 N 处」: %q", out)
	}
}

// ---------- 2. 某条失败其余照常，已成功的不回滚 ----------

func TestEditBatchItemFailureIsolated(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-isolated-test")
	ws := sess.Workspace
	f1 := filepath.Join(ws, "a.go")
	f2 := filepath.Join(ws, "b.go")
	os.WriteFile(f1, []byte("one\ntwo\n"), 0o644)
	os.WriteFile(f2, []byte("alpha\nbeta\ngamma\n"), 0o644)

	tc := batchTestCtx(t, a, sess, "edit")
	out, err := runEditBatch(t, tc,
		map[string]any{"path": f1, "old_string": "one", "new_string": "ONE"},
		map[string]any{"path": f1, "old_string": "nope", "new_string": "x"}, // 失败，该条零改动
		map[string]any{"path": f2, "old_string": "beta", "new_string": "BETA", "replace_all": true},
	)
	if err != nil {
		t.Fatalf("批量 edit 不该因单条失败而报错: %v", err)
	}
	// 成功的两条照常生效
	if got := mustReadFileText(t, f1); got != "ONE\ntwo\n" {
		t.Fatalf("第 1 条应生效且不被回滚: %q", got)
	}
	if got := mustReadFileText(t, f2); got != "alpha\nBETA\ngamma\n" {
		t.Fatalf("第 3 条应生效: %q", got)
	}
	// 逐条状态：一条 ❌ 两条 ✅，失败行点出同文件其他条已生效
	if n := strings.Count(out, editOKMark); n != 2 {
		t.Fatalf("应有 2 条 ✅: %q", out)
	}
	if !strings.Contains(out, "第 2 条未生效") || !strings.Contains(out, "old_string 未找到") {
		t.Fatalf("失败行应说明第几条、什么原因: %q", out)
	}
	if !strings.Contains(out, "（文件内其余 1 条已生效）") {
		t.Fatalf("失败行应提示同文件其他条已生效: %q", out)
	}
	if !strings.Contains(out, "共 3 条：2 条已生效、1 条未生效") {
		t.Fatalf("应有一句汇总: %q", out)
	}
}

// ---------- 3. old_string 多次出现 / replace_all ----------

func TestEditBatchAmbiguousAndReplaceAll(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-ambig-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "dup.txt")
	orig := "x = 1\nx = 1\nx = 1\n"
	if err := os.WriteFile(target, []byte(orig), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := batchTestCtx(t, a, sess, "edit")

	// 未设 replace_all：该条失败、文件不动
	out, err := runEditBatch(t, tc,
		map[string]any{"path": target, "old_string": "x = 1", "new_string": "x = 2"})
	if err != nil {
		t.Fatalf("不该报结构性错误: %v", err)
	}
	if !strings.Contains(out, "出现了 3 次") || !strings.Contains(out, "replace_all") {
		t.Fatalf("多次出现应给出次数与 replace_all 指引: %q", out)
	}
	if mustReadFileText(t, target) != orig {
		t.Fatal("失败的条目应零改动")
	}

	// 设了 replace_all：全部替换并标明处数
	out, err = runEditBatch(t, tc,
		map[string]any{"path": target, "old_string": "x = 1", "new_string": "x = 2", "replace_all": true})
	if err != nil {
		t.Fatalf("replace_all 不该失败: %v", err)
	}
	if !strings.Contains(out, "已替换 3 处（replace_all，行号为第一处）") {
		t.Fatalf("replace_all 多处应标明处数与「行号为第一处」: %q", out)
	}
	if got := mustReadFileText(t, target); got != "x = 2\nx = 2\nx = 2\n" {
		t.Fatalf("应全部替换: %q", got)
	}
}

// ---------- 4/5. 编码保持 + emoji 只让该条失败 ----------

func TestEditBatchKeepsGBKAndIsolatesEmoji(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-gbk-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "gbk.bat")
	orig := gbkBytes(t, "@echo off\r\necho 中文第一行\r\necho 中文第二行\r\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	tc := batchTestCtx(t, a, sess, "edit")
	out, err := runEditBatch(t, tc,
		map[string]any{"path": target, "old_string": "echo 中文第一行", "new_string": "echo 中文已改"},
		// emoji 写不进 GBK：这一条必须失败且零改动，不能拖累第 1 条
		map[string]any{"path": target, "old_string": "echo 中文第二行", "new_string": "echo 😀"},
	)
	if err != nil {
		t.Fatalf("不该报结构性错误: %v", err)
	}
	if strings.Count(out, editOKMark) != 1 || strings.Count(out, editFailMark) != 1 {
		t.Fatalf("应是一成一败: %q", out)
	}
	if !strings.Contains(out, "本条未改动") || !strings.Contains(out, "write") {
		t.Fatalf("emoji 失败应说明该条未改动并指路 write: %q", out)
	}
	raw, _ := os.ReadFile(target)
	if got := detectTextEncoding(raw); got != EncGBK {
		t.Fatalf("改完应仍是 GBK，实得 %v", got)
	}
	text, _ := decodeText(raw)
	if !strings.Contains(text, "echo 中文已改") {
		t.Fatalf("第 1 条应生效: %q", text)
	}
	if !strings.Contains(text, "echo 中文第二行") {
		t.Fatalf("第 2 条应零改动（保留原文）: %q", text)
	}
	if !strings.Contains(text, "\r\n") {
		t.Fatalf("应保持原 CRLF 行尾: %q", text)
	}
	// 编码保持的注记挂在最后一条成功行上
	if !strings.Contains(out, "保持原 GBK 编码写回") {
		t.Fatalf("应注明保持原编码: %q", out)
	}
}

// ---------- 6. 空数组 / 条数上限 ----------

func TestBatchEmptyAndTooManyItems(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-limit-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "x.txt")
	os.WriteFile(target, []byte("hello\n"), 0o644)

	editCtx := batchTestCtx(t, a, sess, "edit")
	if _, err := runEditFile(editCtx, json.RawMessage(`{"edits":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "不能为空") ||
		!strings.Contains(err.Error(), "本次未做任何改动") {
		t.Fatalf("空 edits 应报错并明确文件未动: %v", err)
	}
	many := make([]any, 0, editMaxItems+1)
	for i := 0; i < editMaxItems+1; i++ {
		many = append(many, map[string]any{"path": target, "old_string": "hello", "new_string": "hi"})
	}
	raw, _ := json.Marshal(map[string]any{"edits": many})
	if _, err := runEditFile(editCtx, raw); err == nil ||
		!strings.Contains(err.Error(), "请分批，每批最多 5 条") ||
		!strings.Contains(err.Error(), "本次未做任何改动") {
		t.Fatalf("超过 %d 条应报错并要求分批（不静默截断、也不碰文件）: %v", editMaxItems, err)
	}
	if mustReadFileText(t, target) != "hello\n" {
		t.Fatal("报错的批量不该动文件")
	}

	readCtx := batchTestCtx(t, a, sess, "read")
	if _, err := runReadFile(readCtx, json.RawMessage(`{"files":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "不能为空") ||
		!strings.Contains(err.Error(), "本次未做任何改动") {
		t.Fatalf("空 files 应报错并明确未动文件: %v", err)
	}
	files := make([]any, 0, readMaxFiles+1)
	for i := 0; i < readMaxFiles+1; i++ {
		files = append(files, map[string]any{"path": target})
	}
	raw, _ = json.Marshal(map[string]any{"files": files})
	if _, err := runReadFile(readCtx, raw); err == nil ||
		!strings.Contains(err.Error(), "请分批，每批最多 5 条") ||
		!strings.Contains(err.Error(), "本次未做任何改动") {
		t.Fatalf("超过 %d 条应报错并要求分批: %v", readMaxFiles, err)
	}
}

// ---------- 7. read 预算：填满即停 + 明确告知第几条起没返回 ----------

func TestReadBatchStopsAtBudgetAndTellsRest(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-read-budget-test")
	ws := sess.Workspace
	line := strings.Repeat("a", 29) + "\n" // 30 字节/行
	big := strings.Repeat(line, 3000)      // ≈90KB，两条就超过单次输出预算
	small := "tiny file body\n"
	var paths []string
	for _, name := range []string{"big1.txt", "big2.txt", "small.txt"} {
		p := filepath.Join(ws, name)
		body := big
		if name == "small.txt" {
			body = small
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, p)
	}
	tc := batchTestCtx(t, a, sess, "read")
	raw, _ := json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": paths[0], "limit": 4000},
		map[string]any{"path": paths[1], "limit": 4000},
		map[string]any{"path": paths[2]},
	}})
	out, err := runReadFile(tc, raw)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if len(out) > readOutputMaxBytes+4096 {
		t.Fatalf("输出不该超过预算（%d），实得 %d 字节", readOutputMaxBytes, len(out))
	}
	// 第 1 条完整返回，第 2 条被截断，第 3 条及之后未返回
	if !strings.Contains(out, "big1.txt（第 1-") {
		t.Fatalf("第 1 条段头缺失: %q", string([]rune(out)[:200]))
	}
	if !strings.Contains(out, "被截断") || !strings.Contains(out, "继续读本文件") {
		t.Fatalf("第 2 条应明确说明被截断与续读 offset: %q", batchTailOf(out, 400))
	}
	if !strings.Contains(out, "第 3 条及之后未返回，请分批再读") {
		t.Fatalf("末尾应写明第几条及之后未返回: %q", batchTailOf(out, 400))
	}
	if strings.Contains(out, "tiny file body") {
		t.Fatal("预算用尽后的条目不该被返回")
	}
}

// batchTailOf 返回末尾 n 个字符（失败信息用，避免打印整段）。
func batchTailOf(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[len(r)-n:])
}

// ---------- 8. read 单条失败不影响其它；同一文件多区间 ----------

func TestReadBatchItemFailureAndMultiRange(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-read-misc-test")
	ws := sess.Workspace
	good := filepath.Join(ws, "good.txt")
	os.WriteFile(good, []byte("l1\nl2\nl3\nl4\n"), 0o644)
	bin := filepath.Join(ws, "bin.dat")
	os.WriteFile(bin, []byte{0x00, 0x01, 0x02, 0xff}, 0o644)

	tc := batchTestCtx(t, a, sess, "read")
	raw, _ := json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": filepath.Join(ws, "missing.txt")},
		map[string]any{"path": bin},
		map[string]any{"path": good, "offset": 2, "limit": 2},
		map[string]any{"path": good, "offset": 4},
	}})
	out, err := runReadFile(tc, raw)
	if err != nil {
		t.Fatalf("单条失败不该让整批报错: %v", err)
	}
	if !strings.Contains(out, "missing.txt") || !strings.Contains(out, "读取失败") {
		t.Fatalf("缺文件应在该段给出原因: %q", out)
	}
	if !strings.Contains(out, "bin.dat") || !strings.Contains(out, "二进制") {
		t.Fatalf("二进制文件应在该段给出原因: %q", out)
	}
	if !strings.Contains(out, "good.txt（第 2-3 行，共 4 行）") || !strings.Contains(out, "l2\nl3") {
		t.Fatalf("区间读取不符: %q", out)
	}
	// 行数按编辑器口径："l1\nl2\nl3\nl4\n" 是 4 行，offset=4 就是最后一行
	if !strings.Contains(out, "good.txt（第 4-4 行，共 4 行）") || !strings.Contains(out, "l4") {
		t.Fatalf("同一文件的第二个区间应照常返回: %q", out)
	}
}

// ---------- 9. 旧单条入参仍可用（内部兼容兜底） ----------

func TestLegacySingleArgStillWorks(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-legacy-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "legacy.txt")
	os.WriteFile(target, []byte("旧内容\n"), 0o644)

	readCtx := batchTestCtx(t, a, sess, "read")
	out, err := runReadFile(readCtx, json.RawMessage(fmt.Sprintf(`{"path":%q}`, target)))
	if err != nil {
		t.Fatalf("旧单条 read 入参应仍可用: %v", err)
	}
	if !strings.Contains(out, "旧内容") {
		t.Fatalf("旧单条 read 内容不符: %q", out)
	}

	editCtx := batchTestCtx(t, a, sess, "edit")
	out, err = runEditFile(editCtx, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"旧内容","new_string":"新内容"}`, target)))
	if err != nil {
		t.Fatalf("旧单条 edit 入参应仍可用: %v", err)
	}
	if !strings.Contains(out, editOKMark) || mustReadFileText(t, target) != "新内容\n" {
		t.Fatalf("旧单条 edit 应正常生效: %q", out)
	}
}

// ---------- ① 文件级注记单独成行（↳），只在有话说时出现 ----------

func TestEditFileNoteOwnLine(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-filenote-test")
	ws := sess.Workspace
	gbkFile := filepath.Join(ws, "note.bat")
	if err := os.WriteFile(gbkFile, gbkBytes(t, "@echo off\r\necho 一\r\necho 二\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := batchTestCtx(t, a, sess, "edit")
	out, err := runEditBatch(t, tc,
		map[string]any{"path": gbkFile, "old_string": "echo 一", "new_string": "echo 甲"},
		map[string]any{"path": gbkFile, "old_string": "nope", "new_string": "x"},
		map[string]any{"path": gbkFile, "old_string": "echo 二", "new_string": "echo 乙"},
	)
	if err != nil {
		t.Fatalf("批量 edit 不该整批报错: %v", err)
	}
	lines := strings.Split(out, "\n")
	// 条目行只讲该条自己的事：不得带编码/回读这类文件级注记
	for _, ln := range lines {
		if strings.Contains(ln, "已替换") && (strings.Contains(ln, "编码") || strings.Contains(ln, "回读")) {
			t.Fatalf("条目行不该带文件级注记: %q（全文 %q）", ln, out)
		}
	}
	// 恰好一行 ↳，且内容就是文件级的那件事
	if n := strings.Count(out, editNoteMark); n != 1 {
		t.Fatalf("应恰好一行 ↳ 文件级注记，实得 %d 条: %q", n, out)
	}
	wantNote := editNoteMark + "note.bat：已写盘，保持原 GBK 编码写回"
	noteIdx, lastItemIdx, summaryIdx := -1, -1, -1
	for i, ln := range lines {
		switch {
		case strings.HasPrefix(ln, editNoteMark):
			if ln != wantNote {
				t.Fatalf("文件级注记应为 %q，实得 %q", wantNote, ln)
			}
			noteIdx = i
		case strings.HasPrefix(ln, editOKMark) || strings.HasPrefix(ln, editFailMark):
			lastItemIdx = i
		case strings.HasPrefix(ln, "（共 "):
			summaryIdx = i
		}
	}
	if noteIdx < 0 {
		t.Fatalf("缺少 ↳ 文件级注记行: %q", out)
	}
	if !(lastItemIdx < noteIdx && noteIdx < summaryIdx) {
		t.Fatalf("↳ 行应排在该文件条目之后、汇总之前（条目 %d、注记 %d、汇总 %d）: %q",
			lastItemIdx, noteIdx, summaryIdx, out)
	}

	// 正常 UTF-8 回读一致：没有任何文件级行（不刷屏）
	plain := filepath.Join(ws, "plain.txt")
	os.WriteFile(plain, []byte("aa\nbb\n"), 0o644)
	out2, err := runEditBatch(t, tc,
		map[string]any{"path": plain, "old_string": "aa", "new_string": "AA"},
		map[string]any{"path": plain, "old_string": "bb", "new_string": "BB"})
	if err != nil {
		t.Fatalf("批量 edit 失败: %v", err)
	}
	if strings.Contains(out2, editNoteMark) {
		t.Fatalf("无异常时不该出现 ↳ 行: %q", out2)
	}
}

// 整文件读写失败：仍归到该文件下（↳ + ❌），不与「第 N 条未生效」的条目行混同。
func TestEditFileLevelFailureUnderArrow(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-filefail-test")
	ws := sess.Workspace
	ok := filepath.Join(ws, "ok.txt")
	os.WriteFile(ok, []byte("hello\n"), 0o644)

	tc := batchTestCtx(t, a, sess, "edit")
	out, err := runEditBatch(t, tc,
		map[string]any{"path": filepath.Join(ws, "missing.txt"), "old_string": "x", "new_string": "y"},
		map[string]any{"path": ok, "old_string": "hello", "new_string": "HELLO"},
	)
	if err != nil {
		t.Fatalf("单文件读写失败不该让整批报错: %v", err)
	}
	if !strings.Contains(out, editNoteMark+editFailMark+"missing.txt：读取失败：") {
		t.Fatalf("整文件失败应用 ↳ + ❌ 归到该文件下: %q", out)
	}
	if !strings.Contains(out, "该文件内这 1 条均未生效") {
		t.Fatalf("应点明该文件的条目均未生效: %q", out)
	}
	if mustReadFileText(t, ok) != "HELLO\n" {
		t.Fatal("另一个文件应照常生效")
	}
}

// ---------- ② read 的行数语义与编辑器一致（末尾换行不多算一行） ----------

func TestTextLinesSemantics(t *testing.T) {
	cases := []struct {
		in   string
		n    int
		last string
	}{
		{"a\nb\nc\n", 3, "c"}, // 末尾单个换行只是行终结符
		{"a\nb\nc", 3, "c"},   // 无末尾换行也是 3 行
		{"a\nb\n\n", 3, ""},   // 中间的空行是真的一行（最后一行是空的）
		{"a\n", 1, "a"},
		{"a", 1, "a"},
		{"", 0, ""},
		{"a\nb\n\n\n", 4, ""},
	}
	for _, c := range cases {
		lines := textLines(c.in)
		if len(lines) != c.n {
			t.Fatalf("textLines(%q) 行数=%d，应为 %d", c.in, len(lines), c.n)
		}
		if c.n > 0 && lines[c.n-1] != c.last {
			t.Fatalf("textLines(%q) 末行=%q，应为 %q", c.in, lines[c.n-1], c.last)
		}
	}
}

func TestReadLineCountAndOffset(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-readlines-test")
	ws := sess.Workspace
	for name, body := range map[string]string{
		"three.txt": "a\nb\nc\n",
		"blank.txt": "a\nb\n\n",
		"one.txt":   "a",
		"empty.txt": "",
	} {
		if err := os.WriteFile(filepath.Join(ws, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	tc := batchTestCtx(t, a, sess, "read")
	raw, _ := json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": filepath.Join(ws, "three.txt")},
		map[string]any{"path": filepath.Join(ws, "blank.txt")},
		map[string]any{"path": filepath.Join(ws, "one.txt")},
		map[string]any{"path": filepath.Join(ws, "empty.txt")},
	}})
	out, err := runReadFile(tc, raw)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	// 3 行文件就是 3 行；末尾不多出一行空读
	if !strings.HasPrefix(out, "===== three.txt（第 1-3 行，共 3 行）=====\na\nb\nc\n\n===== blank.txt") {
		t.Fatalf("three.txt 的段头/正文不符（疑结尾多一行空读）: %q", out)
	}
	for _, want := range []string{
		"blank.txt（第 1-3 行，共 3 行）", // 含空行：a、b、空行
		"one.txt（第 1-1 行，共 1 行）",
		"empty.txt（空文件，共 0 行）",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("段头应含 %q: %q", want, out)
		}
	}
	if strings.Contains(out, "empty.txt") && strings.Contains(out, "读取失败") {
		t.Fatalf("空文件不该当成读取失败: %q", out)
	}

	// 续读：offset 与行号一致（offset=2 从第 2 行起，读到末行就停）
	raw, _ = json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": filepath.Join(ws, "three.txt"), "offset": 2, "limit": 2},
	}})
	out, err = runReadFile(tc, raw)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.HasPrefix(out, "===== three.txt（第 2-3 行，共 3 行）=====\nb\nc") {
		t.Fatalf("offset/limit 与行号不一致: %q", out)
	}

	// offset 越过末行：直接说超出范围，不再给一次空读
	raw, _ = json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": filepath.Join(ws, "three.txt"), "offset": 4},
	}})
	out, err = runReadFile(tc, raw)
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "起始行 4 超出文件范围，共 3 行") {
		t.Fatalf("越过末行应报「超出文件范围，共 3 行」: %q", out)
	}
}

// ---------- 折叠视图：部分失败不能说成「编辑成功」 ----------

func TestEditBatchCompactView(t *testing.T) {
	row := &StoredMessage{Message: Message{Role: "tool", Content: ""}, Seq: 7}
	text := editOKMark + "a.go:3 已替换 1 处\n" + editFailMark + "a.go 第 2 条未生效：old_string 未找到（文件内其余 1 条已生效）\n（共 2 条：1 条已生效、1 条未生效）"
	got := toolResultCompact(toolResInfo{name: "edit", text: text}, row)
	if got != "部分失败（1 条已生效、1 条未生效）" {
		t.Fatalf("部分失败应如实折叠，实得 %q", got)
	}
	allOK := editOKMark + "a.go:3 已替换 1 处\n" + editOKMark + "b.go:9 已替换 2 处\n（共 2 条：2 条已生效、0 条未生效）"
	if got := toolResultCompact(toolResInfo{name: "edit", text: allOK}, row); got != "编辑成功" {
		t.Fatalf("全成功应折叠为「编辑成功」，实得 %q", got)
	}
	// 整文件失败行（↳ ❌）仍要被认成失败：不能因为加了缩进标记就变「编辑成功」
	fileFail := editNoteMark + editFailMark + "a.go：读取失败：permission denied（该文件内这 2 条均未生效）\n（共 2 条：0 条已生效、2 条未生效）"
	if got := toolResultCompact(toolResInfo{name: "edit", text: fileFail}, row); got == "编辑成功" || !strings.Contains(got, "失败") {
		t.Fatalf("整文件失败应折叠成失败（不得报「编辑成功」），实得 %q", got)
	}
	// 动作行目标：批量形态取首项路径 + 条数
	if got := actionTarget("edit", `{"edits":[{"path":"a.go","old_string":"x","new_string":"y"},{"path":"b.go","old_string":"m","new_string":"n"}]}`); got != "a.go 等 2 处" {
		t.Fatalf("动作行目标应为首项路径 + 条数，实得 %q", got)
	}
	if got := actionTarget("read", `{"files":[{"path":"a.go"}]}`); got != "a.go" {
		t.Fatalf("单条批量目标应为首项路径，实得 %q", got)
	}
}

// ---------- ③ limit 必须 ≥ 1：显式给 0/负数报错，不静默当整份 ----------

func TestReadBatchLimitMustBePositive(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-readlimit-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "lim.txt")
	if err := os.WriteFile(target, []byte("l1\nl2\nl3\nl4\nl5\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tc := batchTestCtx(t, a, sess, "read")

	// limit=0 曾经静默当成 2000 行（整份大文件灌进上下文）——必须当场报错
	for _, lim := range []int{0, -2} {
		raw, _ := json.Marshal(map[string]any{"files": []any{
			map[string]any{"path": target, "limit": lim}}})
		_, err := runReadFile(tc, raw)
		if err == nil {
			t.Fatalf("limit=%d 应报错（不能静默按默认值读整份）", lim)
		}
		if !strings.Contains(err.Error(), fmt.Sprintf("limit 必须 ≥ 1（收到 %d）", lim)) ||
			!strings.Contains(err.Error(), "本次未做任何读取") {
			t.Fatalf("limit=%d 的报错不符: %v", lim, err)
		}
	}
	// 批量里任一条 limit<1：整批报错并点明第几条
	raw, _ := json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": target, "limit": 1},
		map[string]any{"path": target, "limit": 0}}})
	if _, err := runReadFile(tc, raw); err == nil || !strings.Contains(err.Error(), "第 2 条") {
		t.Fatalf("批量里 limit<1 应点明第几条: %v", err)
	}

	// 不传 limit → 默认读满（5 行）；给 limit=1 → 只读 1 行
	raw, _ = json.Marshal(map[string]any{"files": []any{map[string]any{"path": target}}})
	out, err := runReadFile(tc, raw)
	if err != nil || !strings.Contains(out, "lim.txt（第 1-5 行，共 5 行）") {
		t.Fatalf("不传 limit 应按默认值读满: %q / %v", out, err)
	}
	raw, _ = json.Marshal(map[string]any{"files": []any{
		map[string]any{"path": target, "limit": 1}}})
	out, err = runReadFile(tc, raw)
	if err != nil || !strings.Contains(out, "lim.txt（第 1-1 行，共 5 行）") {
		t.Fatalf("limit=1 应只给 1 行: %q / %v", out, err)
	}

	// offset < 1 或未给按 1 处理（不报错）
	for _, off := range []int{0, -5} {
		raw, _ = json.Marshal(map[string]any{"files": []any{
			map[string]any{"path": target, "offset": off, "limit": 2}}})
		out, err = runReadFile(tc, raw)
		if err != nil || !strings.Contains(out, "lim.txt（第 1-2 行，共 5 行）") {
			t.Fatalf("offset=%d 应按 1 处理: %q / %v", off, out, err)
		}
	}

	// 旧的单条入参（jx.call 桥/老脚本）把 limit 0 当「没给」，沿用默认值
	out, err = runReadFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q,"limit":0}`, target)))
	if err != nil || !strings.Contains(out, "lim.txt（第 1-5 行，共 5 行）") {
		t.Fatalf("旧单条入参 limit=0 应沿用默认值（向后兼容）: %q / %v", out, err)
	}
}

// ---------- ④ 同一文件的不同路径写法归并到同一份内容，只写一次盘 ----------

func TestEditBatchPathVariantsSingleWrite(t *testing.T) {
	a, sess := batchTestApp(t, "data-batch-pathvariant-test")
	ws := sess.Workspace
	target := filepath.Join(ws, "pathcase.txt")
	if err := os.WriteFile(target, []byte("alpha beta\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(ws, target)
	if err != nil {
		t.Fatal(err)
	}
	items := []map[string]any{
		{"path": rel, "old_string": "alpha", "new_string": "ALPHA"},   // 相对工作区
		{"path": target, "old_string": "beta", "new_string": "BETA"}, // 绝对路径
	}
	want := "ALPHA BETA\n"
	if isWindowsRuntime {
		// 大小写不同也是同一文件：这一条只能匹配到第 1 条改完的 ALPHA，
		// 归并失败就会报「old_string 未找到」并留下 ALPHA BETA
		items = append(items, map[string]any{"path": strings.ToUpper(target), "old_string": "ALPHA", "new_string": "ALPHA2"})
		want = "ALPHA2 BETA\n"
	}
	tc := batchTestCtx(t, a, sess, "edit")
	out, err := runEditBatch(t, tc, items...)
	if err != nil {
		t.Fatalf("批量 edit 失败: %v", err)
	}
	if strings.Contains(out, editFailMark) {
		t.Fatalf("同一文件的三种路径写法应全部生效: %q", out)
	}
	if got := mustReadFileText(t, target); got != want {
		t.Fatalf("内容应为 %q，实得 %q（输出 %q）", want, got, out)
	}
	// 只写一次盘 → 只落一条快照
	cps, cerr := a.Store.CheckpointsAfter(sess.ID, -1)
	if cerr != nil {
		t.Fatalf("读快照失败: %v", cerr)
	}
	if len(cps) != 1 {
		t.Fatalf("同一文件的 %d 条应只写一次盘（快照 1 条），实得 %d 条", len(items), len(cps))
	}
	// 文件级注记只出现一次（同一个文件不会重复交代）
	if n := strings.Count(out, editNoteMark); n > 1 {
		t.Fatalf("同一文件只该有一条 ↳ 文件级注记，实得 %d 条: %q", n, out)
	}
}
