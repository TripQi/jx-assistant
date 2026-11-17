package main

// 读面编码的探测子进程（`__jx-probe-encoding`，由 run_command 起自身调用）。
//
// 为什么要有这个进程：探测跑在 run_command 启动命令**之前**，而 Windows 上
// CreateFile / 同步 Read / 同步 Close 都不可取消（Close 还要等未完成的引用），
// 进程内的"超时 + 放弃 worker"只能让调用方先返回，收不回句柄与线程。放进独立
// 进程后，任何一步卡死都是"杀一个进程"的事：TerminateProcess 之后内核回收它的
// 全部句柄，父进程 Wait 即可确认回收（见 encodingprobe_spawn.go）。
//
// 协议（stdin/stdout 都是管道）：
//   父 → 子：首行 "v1\t<受保护目录1>\t<受保护目录2>…"，随后每行一个候选路径
//   子 → 父：每行 "<序号>\t<编码码>\t<原因码>"（原因码空＝探测成功）
// 子进程内部同样是三道闸门：名称闸门 → 受保护路径 → 盘类型/普通文件确认。

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// encodingProbeSubcommand 是隐藏子命令名（main.go 顶部先于 flag/配置/单实例分派）。
const encodingProbeSubcommand = "__jx-probe-encoding"

// encodingProbeProtocolV1 是子进程首行的版本标记。
const encodingProbeProtocolV1 = "v1"

// 原因码（编码码见 TextEncoding.probeCode）。父侧 probeReasonText 翻成文案。
const (
	probeReasonNameBlocked = "blocked-name"
	probeReasonProtected   = "protected"
	probeReasonRemote      = "remote"
	probeReasonMissing     = "missing"
	probeReasonNonRegular  = "nonregular"
	probeReasonOpenFailed  = "io"
)

// probeCode 是跨进程传递编码用的稳定短码（不传人类可读文案）。
func (e TextEncoding) probeCode() string {
	switch e {
	case EncUTF8:
		return "utf8"
	case EncUTF8BOM:
		return "utf8bom"
	case EncGBK:
		return "gbk"
	case EncGB18030:
		return "gb18030"
	case EncUTF16LE:
		return "utf16le"
	case EncUTF16BE:
		return "utf16be"
	case EncBinary:
		return "binary"
	default:
		return "unknown"
	}
}

// encodingFromProbeCode 是 probeCode 的反向映射；认不出的码按"未知"处理。
func encodingFromProbeCode(code string) TextEncoding {
	switch code {
	case "utf8":
		return EncUTF8
	case "utf8bom":
		return EncUTF8BOM
	case "gbk":
		return EncGBK
	case "gb18030":
		return EncGB18030
	case "utf16le":
		return EncUTF16LE
	case "utf16be":
		return EncUTF16BE
	case "binary":
		return EncBinary
	default:
		return EncUnknown
	}
}

// probeProtectedTarget 判断候选是否落在助手自身配置/数据目录内（与
// App.isProtectedPath 同一口径：原始路径与归一化路径都查）。这些位置存着
// API Key 与会话记录，探测子进程也不该去读它们的头部。
func probeProtectedTarget(p string, roots []string) bool {
	if p == "" || len(roots) == 0 {
		return false
	}
	for _, q := range []string{p, canonicalFor(p)} {
		lower := strings.ToLower(filepath.Clean(q))
		for _, pp := range roots {
			if lower == pp || strings.HasPrefix(lower, pp+string(filepath.Separator)) {
				return true
			}
		}
	}
	return false
}

// probePathInChild 探测一个候选：返回编码与原因码（原因码非空＝没探成）。
// 顺序固定：名称闸门 → 受保护路径 → 网络盘 → 存在性 → 普通文件 → 读头部。
func probePathInChild(p string, protectRoots []string) (TextEncoding, string) {
	if why := probeNameBlocked(p); why != "" {
		return EncUnknown, probeReasonNameBlocked
	}
	if probeProtectedTarget(p, protectRoots) {
		return EncUnknown, probeReasonProtected
	}
	if probeVolumeBlocked(p) {
		return EncUnknown, probeReasonRemote
	}
	fi, err := os.Stat(p)
	if err != nil {
		return EncUnknown, probeReasonMissing
	}
	if !fi.Mode().IsRegular() {
		return EncUnknown, probeReasonNonRegular
	}
	f, err := os.Open(p)
	if err != nil {
		return EncUnknown, probeReasonOpenFailed
	}
	defer f.Close()
	// 打开后再确认一次类型：盖住 Stat 与 Open 之间被换成设备的空档。
	if fi2, serr := f.Stat(); serr != nil || !fi2.Mode().IsRegular() {
		return EncUnknown, probeReasonNonRegular
	}
	return probeFileHead(f), ""
}

// parseProbeHeader 解析首行：返回受保护目录与"这一行是不是头"。
// 认不出协议头时按候选处理（向前兼容：老父进程不带头也不会喂错数据）。
func parseProbeHeader(line string) (roots []string, isHeader bool) {
	if line == encodingProbeProtocolV1 {
		return nil, true
	}
	if !strings.HasPrefix(line, encodingProbeProtocolV1+"\t") {
		return nil, false
	}
	rest := strings.TrimPrefix(line, encodingProbeProtocolV1+"\t")
	for _, r := range strings.Split(rest, "\t") {
		if r = strings.TrimSpace(r); r != "" {
			roots = append(roots, strings.ToLower(filepath.Clean(r)))
		}
	}
	return roots, true
}

// runEncodingProbeChild 是子进程入口：读协议、逐候选探测、按行回报。
// 每行都 flush：父进程按预算杀它时，已经探完的结果仍然可读（可诊断）。
func runEncodingProbeChild(in io.Reader, out io.Writer) int {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	w := bufio.NewWriter(out)
	defer w.Flush()
	var protectRoots []string
	idx := 0
	first := true
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			continue
		}
		if first {
			first = false
			if roots, isHeader := parseProbeHeader(line); isHeader {
				protectRoots = roots
				continue
			}
		}
		enc, why := probePathInChild(line, protectRoots)
		fmt.Fprintf(w, "%d\t%s\t%s\n", idx, enc.probeCode(), why)
		w.Flush()
		idx++
	}
	return 0
}
