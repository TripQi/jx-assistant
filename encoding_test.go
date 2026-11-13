package main

// 编码探测 / 写回语义 / 只读判定 / 乱码检测的测试。
//
// 纯函数用例零环境敏感；工具级用例只碰临时文件（不起子进程、不碰真实沙箱）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/text/encoding/simplifiedchinese"
)

func gbkBytes(t *testing.T, s string) []byte {
	t.Helper()
	b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
	if err != nil {
		t.Fatalf("构造 GBK 样本失败: %v", err)
	}
	return b
}

func TestDetectTextEncoding(t *testing.T) {
	utf16le := []byte{0xFF, 0xFE, 'a', 0x00, 'b', 0x00}
	cases := []struct {
		name string
		b    []byte
		want TextEncoding
	}{
		{"空文件按 UTF-8", nil, EncUTF8},
		{"纯 ASCII", []byte("hello\n"), EncUTF8},
		{"UTF-8 中文", []byte("中文\n第二行"), EncUTF8},
		{"UTF-8 带 BOM", append([]byte{0xEF, 0xBB, 0xBF}, []byte("中文")...), EncUTF8BOM},
		{"GBK 中文", gbkBytes(t, "中文测试\n"), EncGBK},
		{"UTF-16LE 带 BOM", utf16le, EncUTF16LE},
		{"二进制含 NUL", []byte{0x00, 0x01, 0x02, 0x03}, EncBinary},
		{"非法字节", []byte{0xFF, 0xFF, 0xFE, 0x01}, EncUnknown},
	}
	for _, c := range cases {
		if got := detectTextEncoding(c.b); got != c.want {
			t.Fatalf("%s: 期望 %v，实得 %v", c.name, c.want, got)
		}
	}
}

func TestDecodeEncodeRoundTrip(t *testing.T) {
	for _, s := range []string{"第一行中文\n第二行 English 混排：；！", "只有 ASCII"} {
		raw := gbkBytes(t, s)
		text, enc := decodeText(raw)
		if enc != EncGBK {
			t.Fatalf("应判为 GBK，实得 %v", enc)
		}
		if text != s {
			t.Fatalf("解码不一致: %q != %q", text, s)
		}
		back, err := encodeText(text, enc)
		if err != nil {
			t.Fatalf("GBK 编回失败: %v", err)
		}
		if string(back) != string(raw) {
			t.Fatalf("编回字节与原文不一致（写回会改动原文件）")
		}
	}
}

// 关键语义：GBK 编不出的字符必须报错，绝不静默替换成 '?'——那是有损丢字。
func TestEncodeGBKRejectsEmoji(t *testing.T) {
	if _, err := encodeText("中文😀", EncGBK); err == nil {
		t.Fatal("emoji 写 GBK 应报错")
	}
	if _, err := encodeText("中文😀", EncUTF8); err != nil {
		t.Fatalf("UTF-8 不该拒绝 emoji: %v", err)
	}
}

// CP936 的单字节 0x80 是欧元符号 €：必须认成 GBK 并原样写回，否则含 € 的老
// GBK 文件会被判「编码未知」，edit 时静默变成 UTF-8（Windows 存盘的 GBK
// 报价单/批处理里就有这种字节，实测可复现）。
func TestGBKDecodeEncodeCP936Euro(t *testing.T) {
	raw := append(append(gbkBytes(t, "价格："), 0x80), gbkBytes(t, "100 元\n")...)
	if got := detectTextEncoding(raw); got != EncGBK {
		t.Fatalf("含 0x80(€) 的 GBK 文件应判为 GBK，实得 %v", got)
	}
	text, enc := decodeText(raw)
	if !strings.Contains(text, "价格：€100 元") {
		t.Fatalf("0x80 应解成 €，实得 %q", text)
	}
	back, err := encodeText(text, enc)
	if err != nil {
		t.Fatalf("含 € 的文本应按 GBK 编回: %v", err)
	}
	if !bytes.Equal(back, raw) {
		t.Fatalf("GBK 写回字节应与原文一致\n原: %x\n回: %x", raw, back)
	}
}

// 64KB 探测窗口可能正好切开一个汉字：必须退尾字节重试认出 GBK，否则
// run_command 的读面会按 UTF-8 走——只读命令靠重跑兜底，写类命令不重跑，
// GBK 内容会被按 UTF-8 误解后写出去。
func TestProbeTextFileSplitsMultibyteAtProbeWindow(t *testing.T) {
	dir := t.TempDir()
	line := gbkBytes(t, "这是一行用于测试大文件的中文内容，长度固定。\r\n")
	// 前置一个 ASCII 字节，让第 64KB 边界落在某个汉字的两个字节之间
	body := append([]byte("x"), bytes.Repeat(line, 66000/len(line)+1)...)
	if got := detectTextEncoding(body[:64*1024]); got == EncGBK {
		t.Fatal("样本构造有误：64KB 截断处应正好切开一个汉字")
	}
	p := filepath.Join(dir, "big_gbk.txt")
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if got := probeTextFile(p); got != EncGBK {
		t.Fatalf("大 GBK 文件应探测为 GBK，实得 %v", got)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content big_gbk.txt`, ""); m != encodingModeGBK {
		t.Fatalf("大 GBK 文件应走 gbk 读面，实得 %q", m)
	}
}

func TestLooksMojibake(t *testing.T) {
	cases := []struct {
		s    string
		want bool
	}{
		{"正常中文：销售订单与采购订单的执行情况", false},
		{"混排 English 与数字 123，标点：；！", false},
		{"閿€鍞崟鍒楄〃", true},            // 典型 UTF-8→GBK 误码
		{"hello \uFFFD world", true}, // 有损解码的替换符
		{"这里有一个字：銆", false},          // 单个特征字不误伤
		{"銆锛", true},                 // 两个特征字才判乱码
		{"中文 English 混排，偶尔出现鏄字", false},
	}
	for _, c := range cases {
		if got := looksMojibake(c.s); got != c.want {
			t.Fatalf("%q: 期望 %v，实得 %v", c.s, c.want, got)
		}
	}
}

func TestExtractPathCandidates(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(a, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	gbkFile := filepath.Join(dir, "旧脚本.bat")
	if err := os.WriteFile(gbkFile, gbkBytes(t, "echo 中文\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range []string{
		`Get-Content "a.txt"`,
		`Select-String -Pattern 'x' -Path 'a.txt'`,
		`type a.txt`,
		`Get-Content ` + a,
		"Get-Content '旧脚本.bat'",
	} {
		got := extractPathCandidates(cmd, dir)
		if len(got) == 0 {
			t.Fatalf("%q: 应抓到至少一个候选路径", cmd)
		}
		for _, p := range got {
			if !filepath.IsAbs(p) {
				t.Fatalf("%q: 候选应是绝对路径，实得 %q", cmd, p)
			}
		}
	}
	// 不存在的文件、通配符、纯参数都不该被抓
	if got := extractPathCandidates(`Get-Content -Raw 不存在.txt *.log`, dir); len(got) != 0 {
		t.Fatalf("应抓不到候选，实得 %v", got)
	}
}

func TestIsReadOnlyCommand(t *testing.T) {
	readOnly := []string{
		`Get-Content a.txt`,
		`Select-String -Pattern 中文 -Path a.txt`,
		`git log --oneline -5`,
		`git status; git diff`,
		`dir; Get-ChildItem`,
		`type 旧脚本.bat`,
		`Get-Content a.txt | Select-Object -First 10`,
		`Get-Content a.txt 2>&1`, // 句柄复制不是写文件
	}
	for _, c := range readOnly {
		if !isReadOnlyCommand(c) {
			t.Fatalf("%q 应判为只读", c)
		}
	}
	notReadOnly := []string{
		`Set-Content b.txt "x"`,
		`Get-Content a.txt; Set-Content b.txt x`,
		`Get-Content a.txt > b.txt`,
		`git commit -m "x"`,
		`git push origin main`,
		`Remove-Item a.txt`,
		`go build ./...`,
		`npm install`,
		`Start-Process notepad`,
		`echo hi >> log.txt`,
	}
	for _, c := range notReadOnly {
		if isReadOnlyCommand(c) {
			t.Fatalf("%q 不该判为只读（重跑会执行两遍）", c)
		}
	}
}

func TestPs51PrologueModes(t *testing.T) {
	utf8 := ps51Prologue("")
	for _, want := range []string{
		"$OutputEncoding", "[Console]::OutputEncoding",
		"$PSDefaultParameterValues['Get-Content:Encoding']='utf8'",
		"$PSDefaultParameterValues['Select-String:Encoding']='utf8'",
		"$PSDefaultParameterValues['Import-Csv:Encoding']='utf8'",
	} {
		if !strings.Contains(utf8, want) {
			t.Fatalf("UTF-8 前置块应含 %q:\n%s", want, utf8)
		}
	}
	gbk := ps51Prologue(encodingModeGBK)
	for _, want := range []string{
		"GetEncoding(936)",
		"$PSDefaultParameterValues['Get-Content:Encoding']='Default'",
		"$PSDefaultParameterValues['Select-String:Encoding']='Default'",
	} {
		if !strings.Contains(gbk, want) {
			t.Fatalf("GBK 前置块应含 %q:\n%s", want, gbk)
		}
	}
	if strings.Contains(utf8, "Out-File:Encoding") || strings.Contains(gbk, "Out-File:Encoding") {
		t.Fatal("写面默认编码不该注入（5.1 的 -Encoding utf8 会写 BOM，写坏 .bat/.cmd）")
	}
}

func TestDetectEncodingMode(t *testing.T) {
	dir := t.TempDir()
	utf8File := filepath.Join(dir, "u.sql")
	gbkFile := filepath.Join(dir, "g.bat")
	if err := os.WriteFile(utf8File, []byte("select 中文 from t"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gbkFile, gbkBytes(t, "echo 中文\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content "u.sql"`, ""); m != encodingModeUTF8 {
		t.Fatalf("UTF-8 文件应走 utf8，实得 %q", m)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content "g.bat"`, ""); m != encodingModeGBK {
		t.Fatalf("GBK 文件应走 gbk，实得 %q", m)
	}
	if m, hint := detectEncodingMode(dir, `Get-Content "u.sql" ; Get-Content "g.bat"`, ""); m != encodingModeUTF8 || hint == "" {
		t.Fatalf("混合编码应按 utf8 并给出提示，实得 %q / %q", m, hint)
	}
	if m, _ := detectEncodingMode(dir, `Get-Content "u.sql"`, "cmd"); m != encodingModeUTF8 {
		t.Fatalf("cmd 路径不做注入，应返回 utf8，实得 %q", m)
	}
}

// TestPs51PrologueLive 是唯一真跑 PowerShell 的用例：验证新注入块在
// -Command 环境下确实生效（读面默认 UTF-8 / GBK 模式对照）。
// 默认跳过（环境敏感），显式设置 JX_ENC_LIVE=1 才执行。
func TestPs51PrologueLive(t *testing.T) {
	if os.Getenv("JX_ENC_LIVE") != "1" || !isWindowsRuntime {
		t.Skip("设置 JX_ENC_LIVE=1 且在 Windows 上才跑真实 PowerShell 编码用例")
	}
	dir := t.TempDir()
	u8 := filepath.Join(dir, "u8.txt")
	gb := filepath.Join(dir, "g.bat")
	if err := os.WriteFile(u8, []byte("第一行中文（UTF-8 无 BOM）\r\n第二行：销售订单与采购订单\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(gb, gbkBytes(t, "@echo off\r\necho 中文批处理：执行情况\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runPS := func(mode, cmd string) string {
		t.Helper()
		c := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps51Prologue(mode)+cmd)
		c.Env = appendEnvUnique(os.Environ(), ps51Utf8Env()...)
		out, err := c.CombinedOutput()
		if err != nil {
			t.Fatalf("powershell 执行失败（mode=%s）: %v\n%s", mode, err, decodeCmdOutput(out))
		}
		return decodeCmdOutput(out)
	}
	// 1) 注入块本身生效：读面默认值确实被设上
	if s := runPS("", `$PSDefaultParameterValues['Get-Content:Encoding']`); !strings.Contains(s, "utf8") {
		t.Fatalf("读面默认编码未生效，实际: %q", s)
	}
	// 2) UTF-8 模式读 UTF-8 无 BOM 文件：正确、无乱码
	if s := runPS("", `Get-Content -LiteralPath '`+u8+`'`); !strings.Contains(s, "第二行：销售订单与采购订单") || looksMojibake(s) {
		t.Fatalf("UTF-8 模式读 UTF-8 文件应正确，实际:\n%s", s)
	}
	// 3) GBK 模式读 GBK 文件：正确
	if s := runPS(encodingModeGBK, `Get-Content -LiteralPath '`+gb+`'`); !strings.Contains(s, "中文批处理：执行情况") {
		t.Fatalf("GBK 模式读 GBK 文件应正确，实际:\n%s", s)
	}
	// 4) 对照：UTF-8 模式读 GBK 文件必须解不对（证明第 2/3 步不是"碰巧都对"）
	if s := runPS("", `Get-Content -LiteralPath '`+gb+`'`); strings.Contains(s, "中文批处理：执行情况") {
		t.Fatalf("对照组不应正确解出中文，实际:\n%s", s)
	}
}

// ---------- 工具级：read 解码 + edit 保持原编码 ----------

func TestReadFileDecodesGBK(t *testing.T) {
	a := newCapTestApp(t, "data-enc-read-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "read"}

	target := filepath.Join(ws, "gbk读取验证.txt")
	if err := os.WriteFile(target, gbkBytes(t, "第一行中文\n第二行内容\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q}`, target)))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if !strings.Contains(out, "第一行中文") || !strings.Contains(out, "第二行内容") {
		t.Fatalf("GBK 文件应解码为正确中文，实得: %q", out)
	}
	if !strings.Contains(out, "GBK") || !strings.Contains(out, "原编码写回") {
		t.Fatalf("应注明原编码与写回语义，实得: %q", out)
	}
}

func TestEditPreservesGBKEncoding(t *testing.T) {
	a := newCapTestApp(t, "data-enc-edit-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "edit"}

	target := filepath.Join(ws, "gbk编辑验证.bat")
	orig := gbkBytes(t, "@echo off\r\necho 中文第一行\r\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := runEditFile(tc, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"echo 中文第一行","new_string":"echo 中文已改过"}`, target)))
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	if !strings.Contains(out, "保持原 GBK 编码写回") {
		t.Fatalf("成功文案应注明保持原编码，实得: %q", out)
	}
	raw, rerr := os.ReadFile(target)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got := detectTextEncoding(raw); got != EncGBK {
		t.Fatalf("写回后应仍是 GBK，实得 %v", got)
	}
	text, _ := decodeText(raw)
	if !strings.Contains(text, "echo 中文已改过") || !strings.Contains(text, "@echo off") {
		t.Fatalf("写回内容不正确: %q", text)
	}
	// 连续第二次编辑：验证幂等，不会因转码叠加而变乱
	if _, err := runEditFile(tc, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"echo 中文已改过","new_string":"echo 中文再改"}`, target))); err != nil {
		t.Fatalf("第二次编辑失败: %v", err)
	}
	raw2, _ := os.ReadFile(target)
	if got := detectTextEncoding(raw2); got != EncGBK {
		t.Fatalf("第二次写回后应仍是 GBK，实得 %v", got)
	}
	if text2, _ := decodeText(raw2); !strings.Contains(text2, "echo 中文再改") {
		t.Fatalf("第二次写回内容不正确: %q", text2)
	}
}

func TestEditGBKRejectsUnencodable(t *testing.T) {
	a := newCapTestApp(t, "data-enc-edit-reject-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "edit"}

	target := filepath.Join(ws, "gbk不可写验证.bat")
	orig := gbkBytes(t, "@echo off\r\necho 中文\r\n")
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := runEditFile(tc, json.RawMessage(fmt.Sprintf(
		`{"path":%q,"old_string":"echo 中文","new_string":"echo 😀"}`, target)))
	if err == nil {
		t.Fatal("emoji 写入 GBK 文件应报错")
	}
	raw, _ := os.ReadFile(target)
	if string(raw) != string(orig) {
		t.Fatal("报错时文件不应被改动")
	}
}

func TestWriteWarnsWhenOverwritingGBK(t *testing.T) {
	a := newCapTestApp(t, "data-enc-write-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)
	tc := &toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "write"}

	target := filepath.Join(ws, "覆盖提示验证.bat")
	if err := os.WriteFile(target, gbkBytes(t, "echo 旧\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runReadFile(&toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "read"},
		json.RawMessage(fmt.Sprintf(`{"path":%q}`, target))); err != nil {
		t.Fatalf("先读观察失败: %v", err)
	}
	out, err := runWriteFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"echo new\n"}`, target)))
	if err != nil {
		t.Fatalf("写入失败: %v", err)
	}
	if !strings.Contains(out, "原为 GBK 编码") || !strings.Contains(out, "UTF-8") {
		t.Fatalf("覆盖 GBK 文件应提示编码变化，实得: %q", out)
	}
	raw, _ := os.ReadFile(target)
	if got := detectTextEncoding(raw); got != EncUTF8 {
		t.Fatalf("write 后应为 UTF-8，实得 %v", got)
	}
}

// 含 €（CP936 单字节 0x80）的 GBK 文件：read 不该报「编码无法确定」，
// edit 必须仍写 GBK——修复前这里会被静默改写成 UTF-8（真实 Windows
// 存盘的 GBK 报价单就是这种字节）。
func TestEditKeepsGBKWithCP936Euro(t *testing.T) {
	a := newCapTestApp(t, "data-enc-edit-euro-test")
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws}
	mustCreateSession(t, a, sess)

	target := filepath.Join(ws, "gbk欧元验证.txt")
	orig := append(append(gbkBytes(t, "价格："), 0x80), gbkBytes(t, "100 元\n")...)
	if err := os.WriteFile(target, orig, 0o644); err != nil {
		t.Fatal(err)
	}
	readOut, err := runReadFile(&toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "read"},
		json.RawMessage(fmt.Sprintf(`{"path":%q}`, target)))
	if err != nil {
		t.Fatalf("读取失败: %v", err)
	}
	if strings.Contains(readOut, "编码无法确定") || !strings.Contains(readOut, "GBK 编码") {
		t.Fatalf("含 € 的 GBK 文件应判为 GBK，实得: %q", readOut)
	}
	editOut, err := runEditFile(&toolContext{ctx: context.Background(), app: a, sess: sess, toolName: "edit"},
		json.RawMessage(fmt.Sprintf(`{"path":%q,"old_string":"价格：€100 元","new_string":"价格：€200 元"}`, target)))
	if err != nil {
		t.Fatalf("编辑失败: %v", err)
	}
	if !strings.Contains(editOut, "保持原 GBK 编码写回") {
		t.Fatalf("成功文案应注明保持原编码，实得: %q", editOut)
	}
	raw, _ := os.ReadFile(target)
	if got := detectTextEncoding(raw); got != EncGBK {
		t.Fatalf("写回后应仍是 GBK，实得 %v", got)
	}
	if !bytes.Contains(raw, []byte{0x80}) {
		t.Fatalf("€ 应仍以单字节 0x80 存放，实得字节: %x", raw)
	}
	if text, _ := decodeText(raw); !strings.Contains(text, "价格：€200 元") {
		t.Fatalf("写回内容不正确: %q", text)
	}
}
