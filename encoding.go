package main

// 文本编码探测与判定（全项目唯一真源）。
//
// 为什么需要它：PowerShell 5.1 的 Get-Content/Select-String 默认按 ANSI(936)
// 解文件，UTF-8 无 BOM 的文本必然乱码；而"UTF-8 字节被 GBK 解码"得到的是
// 合法 UTF-8 字符串（閿€鍞…），输出侧的解码兜底看不出来。所以必须在**读之前**
// 按真实编码去解：命令里出现的文件先探测、read/edit 读文件也探测。
//
// 判定顺序固定且互斥（无歧义）：BOM → 严格 UTF-8 → GBK/GB18030 结构 →
// UTF-16 启发 → 二进制/未知。GBK 结构校验自己写而不用"解码是否报错"——
// x/text 的 GBK 解码器对非法字节静默丢字、不返回错误（实测 0xFF 0xFF → 空串），
// 拿它当探针会把任意二进制都认成 GBK。

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf16"
	"unicode/utf8"

	"golang.org/x/text/encoding/simplifiedchinese"
)

// TextEncoding 是探测出来的文件编码。零值即 UTF-8（无 BOM），
// 让"默认 UTF-8"成为结构的默认行为。
type TextEncoding int

const (
	EncUTF8    TextEncoding = iota // UTF-8 无 BOM（默认）
	EncUTF8BOM                     // UTF-8 带 BOM
	EncGBK                         // GBK/GB2312 双字节（含 ASCII）
	EncGB18030                     // 出现 GB18030 四字节序列
	EncUTF16LE                     // UTF-16 小端（带或不带 BOM）
	EncUTF16BE                     // UTF-16 大端（带或不带 BOM）
	EncBinary                      // 二进制（含 NUL 且不像 UTF-16 文本）
	EncUnknown                     // 既非合法 UTF-8 也非合法 GB 族：按 GB18030 宽松解，调用方注记
)

func (e TextEncoding) String() string {
	switch e {
	case EncUTF8:
		return "UTF-8"
	case EncUTF8BOM:
		return "UTF-8(BOM)"
	case EncGBK:
		return "GBK"
	case EncGB18030:
		return "GB18030"
	case EncUTF16LE:
		return "UTF-16LE"
	case EncUTF16BE:
		return "UTF-16BE"
	case EncBinary:
		return "二进制"
	default:
		return "未知编码"
	}
}

// needsPreserveBytes 表示 write 整体覆盖时会改变编码，需要提示用户/模型。
func (e TextEncoding) needsPreserveBytes() bool {
	return e == EncGBK || e == EncGB18030 || e == EncUTF16LE || e == EncUTF16BE || e == EncUTF8BOM
}

var (
	bomUTF8    = []byte{0xEF, 0xBB, 0xBF}
	bomUTF16LE = []byte{0xFF, 0xFE}
	bomUTF16BE = []byte{0xFE, 0xFF}
)

// detectTextEncoding 探测给定字节块的编码（调用方负责只喂文件头部）。
// 顺序：BOM → 含 NUL（UTF-16 或二进制）→ 严格 UTF-8 → GB 族结构 → 未知。
// 含 NUL 必须排在 utf8.Valid 之前：NUL 在 UTF-8 里是合法字符，但正常文本
// 文件不会带 NUL，带了就只能是 UTF-16 或二进制。
func detectTextEncoding(b []byte) TextEncoding {
	switch {
	case len(b) == 0:
		return EncUTF8
	case bytes.HasPrefix(b, bomUTF8):
		return EncUTF8BOM
	case bytes.HasPrefix(b, bomUTF16LE):
		return EncUTF16LE
	case bytes.HasPrefix(b, bomUTF16BE):
		return EncUTF16BE
	case bytes.IndexByte(b, 0) >= 0:
		if e := utf16Guess(b); e != EncUnknown {
			return e
		}
		return EncBinary
	case utf8.Valid(b):
		return EncUTF8
	}
	if ok, hasFour := gbkStructure(b); ok {
		if hasFour {
			return EncGB18030
		}
		return EncGBK
	}
	return EncUnknown
}

// gbkStructure 校验字节能否完整切分成 GBK 双字节 / GB18030 四字节序列。
// ASCII 区（0x00-0x7F）两族共用；0x80/0xFF 在 GBK 中未定义，直接判否。
func gbkStructure(b []byte) (ok bool, hasFour bool) {
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			i++
			continue
		}
		if c == 0x80 || c == 0xFF {
			return false, false
		}
		if i+3 < len(b) && b[i+1] >= 0x30 && b[i+1] <= 0x39 {
			if b[i+2] < 0x81 || b[i+2] > 0xFE || b[i+3] < 0x30 || b[i+3] > 0x39 {
				return false, false
			}
			hasFour = true
			i += 4
			continue
		}
		if i+1 >= len(b) {
			return false, false
		}
		t := b[i+1]
		if t < 0x40 || t == 0x7F || t > 0xFE {
			return false, false
		}
		i += 2
	}
	return true, hasFour
}

// utf16Guess 用「高位字节多为 0」的统计判定 UTF-16 字节序（仅无 BOM 时需要）。
func utf16Guess(b []byte) TextEncoding {
	limit := len(b)
	if limit > 4096 {
		limit = 4096
	}
	pairs := limit / 2
	if pairs < 2 {
		return EncUnknown
	}
	var even, odd int
	for i := 0; i+1 < limit; i += 2 {
		if b[i] == 0 {
			even++
		}
		if b[i+1] == 0 {
			odd++
		}
	}
	switch {
	case odd*4 > pairs*3: // 小端：偶数位是低字节、奇数位是高字节
		return EncUTF16LE
	case even*4 > pairs*3:
		return EncUTF16BE
	}
	return EncUnknown
}

// decodeText 按探测结果把字节解成 UTF-8 文本，返回文本与编码。
// 只解一次、不做二次转换：这是 read 与 edit 的共同入口。
func decodeText(b []byte) (string, TextEncoding) {
	enc := detectTextEncoding(b)
	switch enc {
	case EncUTF8BOM:
		return string(b[len(bomUTF8):]), enc
	case EncUTF16LE, EncUTF16BE:
		return decodeUTF16(b, enc), enc
	case EncGBK:
		if s, err := simplifiedchinese.GBK.NewDecoder().Bytes(b); err == nil {
			return string(s), enc
		}
		return string(b), EncUnknown
	case EncGB18030:
		if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
			return string(s), enc
		}
		return string(b), EncUnknown
	case EncUnknown:
		// 未知编码按 GB18030 宽松解（中文环境最可能的来源），失败再兜底替换
		if s, err := simplifiedchinese.GB18030.NewDecoder().Bytes(b); err == nil {
			return string(s), enc
		}
		return strings.ToValidUTF8(string(b), "\uFFFD"), enc
	default: // EncUTF8 / EncBinary
		return string(b), enc
	}
}

// decodeUTF16 解 UTF-16（带 BOM 或不带）。
func decodeUTF16(b []byte, enc TextEncoding) string {
	if enc == EncUTF16LE && bytes.HasPrefix(b, bomUTF16LE) {
		b = b[2:]
	}
	if enc == EncUTF16BE && bytes.HasPrefix(b, bomUTF16BE) {
		b = b[2:]
	}
	u := make([]uint16, 0, len(b)/2)
	for i := 0; i+1 < len(b); i += 2 {
		if enc == EncUTF16LE {
			u = append(u, uint16(b[i])|uint16(b[i+1])<<8)
		} else {
			u = append(u, uint16(b[i])<<8|uint16(b[i+1]))
		}
	}
	return string(utf16.Decode(u))
}

// encodeText 按目标编码把文本编回字节（edit 写回用）。不可表示字符直接报错，
// 绝不静默替换成 '?'——替换一次就永久丢字，比报错难查得多。
func encodeText(s string, enc TextEncoding) ([]byte, error) {
	switch enc {
	case EncUTF8BOM:
		return append(append([]byte{}, bomUTF8...), []byte(s)...), nil
	case EncUTF16LE:
		out := append([]byte{}, bomUTF16LE...)
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, byte(u), byte(u>>8))
		}
		return out, nil
	case EncUTF16BE:
		out := append([]byte{}, bomUTF16BE...)
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, byte(u>>8), byte(u))
		}
		return out, nil
	case EncGBK:
		b, err := simplifiedchinese.GBK.NewEncoder().Bytes([]byte(s))
		if err != nil {
			return nil, errors.New("内容含 GBK 无法表示的字符（如 emoji），已中止写入：请改用 write 以 UTF-8 保存，或先去掉该字符")
		}
		return b, nil
	case EncGB18030:
		b, err := simplifiedchinese.GB18030.NewEncoder().Bytes([]byte(s))
		if err != nil {
			return nil, errors.New("内容含 GB18030 无法表示的字符，已中止写入：请改用 write 以 UTF-8 保存")
		}
		return b, nil
	default:
		// UTF-8 / Unknown：按 UTF-8 落盘（Unknown 已被解码为 UTF-8 文本）
		return []byte(s), nil
	}
}

// probeTextFile 读文件头部（64KB）探测编码；文件不存在返回 EncUnknown。
func probeTextFile(path string) TextEncoding {
	f, err := os.Open(path)
	if err != nil {
		return EncUnknown
	}
	defer f.Close()
	buf := make([]byte, 64*1024)
	n, _ := f.Read(buf)
	return detectTextEncoding(buf[:n])
}

const mojibakeSample = "銆锛鈥鍜閿鑴娴鏂绋缂鍐鑻鏈宸璁鏄鍚瀹鎴缁鍒鏃鍙锟"

// looksMojibake 判断文本是否已经是"乱码态"（两种来源）：
//  1. 含 U+FFFD：某次有损解码已经把字节换成替换符，**不可逆**，必须重读；
//  2. 出现 ≥2 个 UTF-8→GBK 误码的高频字：正常中文文本里这些字几乎不出现，
//     阈值 2 是为了放过偶发的单字（如引文、人名用字）。
func looksMojibake(s string) bool {
	if strings.ContainsRune(s, utf8.RuneError) {
		return true
	}
	hits := 0
	for _, r := range mojibakeSample {
		if strings.ContainsRune(s, r) {
			hits++
			if hits >= 2 {
				return true
			}
		}
	}
	return false
}

var (
	reWinPath     = regexp.MustCompile(`(?i)\b[A-Z]:[\\/][^"'|<>;&\r\n]*`)
	reQuoted      = regexp.MustCompile(`"([^"\r\n]+)"|'([^'\r\n]+)'`)
	reBarePath    = regexp.MustCompile(`(?i)[^\s"'|<>;&]+\.(?:txt|md|sql|java|kt|go|ts|vue|js|cjs|mjs|json|ya?ml|xml|csv|bat|cmd|ps1|py|log|ini|conf|properties|html|css|scss|toml|gradle|kts|sh)`)
	reWriteRedir  = regexp.MustCompile(`[^0-9&]>`)
	reCmdSplitter = regexp.MustCompile(`[;|\r\n]+|&&|\|\|`)
	maxProbeNum   = 8 // 一条命令里最多探测几个文件：够覆盖常见读法，又不让每条命令多花时间
)

// extractPathCandidates 从命令串里捞出可能被读取的文件路径（去重、只留真实存在的
// 普通文件）。只认"命令里字面出现"的路径——程序内部自己读的文件探测不到，那条路
// 由输出乱码检测 + 重跑兜底。
func extractPathCandidates(command, cwd string) []string {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	var raw []string
	raw = append(raw, reWinPath.FindAllString(command, -1)...)
	for _, m := range reQuoted.FindAllStringSubmatch(command, -1) {
		if m[1] != "" {
			raw = append(raw, m[1])
		} else if m[2] != "" {
			raw = append(raw, m[2])
		}
	}
	raw = append(raw, reBarePath.FindAllString(command, -1)...)
	seen := make(map[string]bool, len(raw))
	out := make([]string, 0, len(raw))
	for _, p := range raw {
		p = strings.TrimSpace(strings.Trim(p, `"';,`))
		if p == "" || strings.ContainsAny(p, "*?<>|") || strings.HasPrefix(p, "-") {
			continue
		}
		// 去掉尾随的 PowerShell 参数/标点残留（如 file.txt, 或 file.txt)）
		p = strings.TrimRight(p, "):,;")
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		if fi, err := os.Stat(p); err != nil || fi.IsDir() {
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxProbeNum {
			break
		}
	}
	return out
}

// 只读命令白名单：命令被「; | && || 换行」拆段后，每段的第一个动词都必须在这里。
var readOnlyVerbs = map[string]bool{
	"get-content": true, "gc": true, "cat": true, "type": true, "more": true,
	"head": true, "tail": true, "select-string": true, "sls": true, "findstr": true,
	"grep": true, "rg": true, "dir": true, "ls": true, "get-childitem": true, "gci": true,
	"test-path": true, "get-item": true, "gi": true, "resolve-path": true,
	"measure-object": true, "sort-object": true, "select-object": true, "where-object": true,
	"format-table": true, "format-list": true, "out-string": true, "out-host": true,
	"write-output": true, "write-host": true, "echo": true, "cd": true, "set-location": true,
	"pwd": true, "get-location": true, "whoami": true, "hostname": true, "date": true,
	"git": true, "go": true, "npm": true, "node": true, "java": true, "python": true, "py": true,
	"mvn": true, "gradle": true, "which": true, "where": true, "ver": true, "uname": true,
}

// 写/副作用特征：出现即判定"非只读"，绝不自动重跑（重跑等于执行两次）。
var writeHints = []string{
	"out-file", "set-content", "add-content", "clear-content", "new-item", "remove-item",
	"copy-item", "move-item", "rename-item", "set-itemproperty", "start-process", "stop-process",
	"taskkill", "kill ", "rm ", "rmdir", "del ", "erase ", "move ", "mv ", "copy ", "cp ",
	"mkdir", "md ", "tee ", "chmod", "chown", "touch", "install", "uninstall", "update",
	"commit", "push", "pull", "fetch", "merge", "rebase", "reset", "checkout", "clean",
	"build", "package", "publish", "deploy", "start ", "restart", "stop ", "enable",
	"disable", "import-csv", "export-csv", "convertto-json ", "set-", "invoke-webrequest",
	"invoke-restmethod", "curl ", "wget ", "scp ", "ssh ", "docker ", "systemctl",
}

// isReadOnlyCommand 判定命令是否可安全重跑一次（只读、无写副作用）。
// 保守原则：拿不准就返回 false——宁可少重跑一次，也不能让命令执行两遍。
func isReadOnlyCommand(command string) bool {
	low := strings.ToLower(command)
	for _, w := range writeHints {
		if strings.Contains(low, w) {
			return false
		}
	}
	// 重定向写文件（含 >>），排除 2>&1 这类句柄复制
	if reWriteRedir.MatchString(command) {
		return false
	}
	// 拆段：每段第一个动词必须在只读白名单里
	seen := false
	for _, seg := range reCmdSplitter.Split(command, -1) {
		seg = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(seg), "&"))
		if seg == "" {
			continue
		}
		// 跳过 $env:X='y' / $v=... 之类的赋值段
		if strings.HasPrefix(seg, "$") && strings.Contains(seg, "=") {
			continue
		}
		fields := strings.Fields(seg)
		if len(fields) == 0 {
			continue
		}
		verb := strings.ToLower(filepath.Base(strings.Trim(fields[0], `"'`)))
		verb = strings.TrimSuffix(verb, ".exe")
		if !readOnlyVerbs[verb] {
			return false
		}
		seen = true
	}
	return seen
}

// 文件级"读面编码"：告诉 PS 5.1 该按 UTF-8 还是按 ANSI(936) 读文件。
const (
	encodingModeUTF8 = "utf8"
	encodingModeGBK  = "gbk"
)

// detectEncodingMode 决定一条 PS 5.1 命令该用哪种读面编码：
//   - 命令里出现的文本文件全是 GB 族 → gbk（含 .bat/.cmd 这类老文件）
//   - 全是 UTF-8 族 / 没有可探测文件 → utf8（默认）
//   - 混在一起 → utf8（多数资产是 UTF-8），由输出乱码检测 + 重跑兜底
//
// 返回的 hint 只在混合/未知时给出（正常情况为空，不制造噪音）。
func detectEncodingMode(cwd, command, shell string) (mode string, hint string) {
	if !isPS51Shell(shell) {
		return encodingModeUTF8, ""
	}
	paths := extractPathCandidates(command, cwd)
	if len(paths) == 0 {
		return encodingModeUTF8, ""
	}
	var gbFiles, utf8Files, otherFiles []string
	for _, p := range paths {
		switch probeTextFile(p) {
		case EncGBK, EncGB18030:
			gbFiles = append(gbFiles, p)
		case EncUTF8, EncUTF8BOM, EncUTF16LE, EncUTF16BE:
			utf8Files = append(utf8Files, p)
		default:
			otherFiles = append(otherFiles, p)
		}
	}
	switch {
	case len(gbFiles) > 0 && len(utf8Files) == 0:
		return encodingModeGBK, ""
	case len(gbFiles) > 0 && len(utf8Files) > 0:
		return encodingModeUTF8, "命令同时涉及 UTF-8 与 GBK 文件（" + filepath.Base(gbFiles[0]) +
			" 是 " + probeTextFile(gbFiles[0]).String() + "）：本条按 UTF-8 读；读该文件请加 -Encoding Default 或改用 read 工具"
	case len(otherFiles) > 0 && len(utf8Files) == 0:
		return encodingModeUTF8, "命令里的文件编码无法确定（" + filepath.Base(otherFiles[0]) +
			"）：按 UTF-8 读取，遇到乱码会自动换编码重跑一次"
	}
	return encodingModeUTF8, ""
}

// isPS51Shell 与三处注入点的判定口径保持一致：空 shell 与 powershell 走 PS 5.1。
func isPS51Shell(shell string) bool {
	switch strings.ToLower(strings.TrimSpace(shell)) {
	case "", "powershell":
		return true
	}
	return false
}

// alternateEncodingMode 是重跑时切换到的另一种模式。
func alternateEncodingMode(mode string) string {
	if mode == encodingModeGBK {
		return encodingModeUTF8
	}
	return encodingModeGBK
}
