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
//  9. 旧单条入参仍可用（run_python 的 jx.call 桥、老脚本的内部兼容兜底）。
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
	if !strings.Contains(out, "已替换 3 处（replace_all）") {
		t.Fatalf("replace_all 成功行应带标记: %q", out)
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
		!strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空 edits 应报错: %v", err)
	}
	many := make([]any, 0, editMaxItems+1)
	for i := 0; i < editMaxItems+1; i++ {
		many = append(many, map[string]any{"path": target, "old_string": "hello", "new_string": "hi"})
	}
	raw, _ := json.Marshal(map[string]any{"edits": many})
	if _, err := runEditFile(editCtx, raw); err == nil ||
		!strings.Contains(err.Error(), "请分批，每批最多 5 条") {
		t.Fatalf("超过 %d 条应报错并要求分批（不静默截断）: %v", editMaxItems, err)
	}
	if mustReadFileText(t, target) != "hello\n" {
		t.Fatal("报错的批量不该动文件")
	}

	readCtx := batchTestCtx(t, a, sess, "read")
	if _, err := runReadFile(readCtx, json.RawMessage(`{"files":[]}`)); err == nil ||
		!strings.Contains(err.Error(), "不能为空") {
		t.Fatalf("空 files 应报错: %v", err)
	}
	files := make([]any, 0, readMaxFiles+1)
	for i := 0; i < readMaxFiles+1; i++ {
		files = append(files, map[string]any{"path": target})
	}
	raw, _ = json.Marshal(map[string]any{"files": files})
	if _, err := runReadFile(readCtx, raw); err == nil ||
		!strings.Contains(err.Error(), "请分批，每批最多 5 条") {
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
	if !strings.Contains(out, "good.txt（第 2-3 行，共 5 行）") || !strings.Contains(out, "l2\nl3") {
		t.Fatalf("区间读取不符: %q", out)
	}
	if !strings.Contains(out, "good.txt（第 4-5 行，共 5 行）") || !strings.Contains(out, "l4") {
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
	// 动作行目标：批量形态取首项路径 + 条数
	if got := actionTarget("edit", `{"edits":[{"path":"a.go","old_string":"x","new_string":"y"},{"path":"b.go","old_string":"m","new_string":"n"}]}`); got != "a.go 等 2 处" {
		t.Fatalf("动作行目标应为首项路径 + 条数，实得 %q", got)
	}
	if got := actionTarget("read", `{"files":[{"path":"a.go"}]}`); got != "a.go" {
		t.Fatalf("单条批量目标应为首项路径，实得 %q", got)
	}
}
