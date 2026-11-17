package main

// run_command 读面编码的**父侧**部分：候选提取（纯字符串）→ 一次探测子进程 →
// 聚合出整条命令的读面编码。
//
// 不变式（encodingprobe_guard_test.go 的 AST 用例守着）：本文件不得 import
// os / syscall / os/exec / golang.org/x/sys，也不得调用任何文件系统 API。
// 理由见 encoding.go 的「探测的边界」：这段跑在启动命令之前，只要碰一次文件
// 系统，就可能被不可取消的 CreateFile/同步 Read/同步 Close 永久挂住，而那时
// runChild 的超时还没开始计时。存在性、盘类型、普通文件确认、读头部全在子进程。

import (
	"context"
	"fmt"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var (
	reWinPath  = regexp.MustCompile(`(?i)\b[A-Z]:[\\/][^"'|<>;&\r\n]*`)
	reQuoted   = regexp.MustCompile(`"([^"\r\n]+)"|'([^'\r\n]+)'`)
	reBarePath = regexp.MustCompile(`(?i)[^\s"'|<>;&]+\.(?:txt|md|sql|java|kt|go|ts|vue|js|cjs|mjs|json|ya?ml|xml|csv|bat|cmd|ps1|py|log|ini|conf|properties|html|css|scss|toml|gradle|kts|sh)`)
	// maxProbeNum 是一条命令里最多探测几个文件：够覆盖常见读法，又不让每条命令
	// 多花时间（一次子进程探测全部候选）。
	maxProbeNum = 8
)

// probeReasonText 把子进程的原因码翻成一句话。空串＝不出提示。
// "文件不存在"刻意静默：命令里提到的输出文件本来就还不存在，提示只会变噪音
// （与探测前的旧行为一致）。
func probeReasonText(code string) string {
	switch code {
	case probeReasonNameBlocked:
		return "名字像设备或特殊路径，已跳过"
	case probeReasonProtected:
		return "位于助手自身配置/数据目录，已跳过"
	case probeReasonRemote:
		return "在网络驱动器上，已跳过"
	case probeReasonNonRegular:
		return "不是普通文件（设备/目录/管道），已跳过"
	case probeReasonOpenFailed:
		return "打开失败，已跳过"
	default:
		return ""
	}
}

// extractPathCandidates 从命令串里捞出可能是文件的路径：正则 + 名称闸门 +
// 去重 + 限量，**纯字符串**、不判断存在性（因此不会被任何设备/远端卡住）。
// 被名称闸门拦下的第一条写成 skipNote，供提示说明本条为何按 UTF-8 兜底。
func extractPathCandidates(command, cwd string) (paths []string, skipNote string) {
	if strings.TrimSpace(command) == "" {
		return nil, ""
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
		// 协议按行传递，含换行的路径不接收
		if strings.ContainsAny(p, "\r\n") {
			continue
		}
		if !filepath.IsAbs(p) {
			p = filepath.Join(cwd, p)
		}
		p = filepath.Clean(p)
		if seen[p] {
			continue
		}
		// 设备名/设备命名空间/备用数据流按名字就拒（连子进程都不惊动）：
		// 命令正文（含提交说明）是模型随手写的，一个保留设备名就能让后面的
		// Stat/Open 摸到真实设备。
		if why := probeNameBlocked(p); why != "" {
			if skipNote == "" {
				skipNote = filepath.Base(p) + " " + why + "，已跳过"
			}
			continue
		}
		seen[p] = true
		out = append(out, p)
		if len(out) >= maxProbeNum {
			break
		}
	}
	return out, skipNote
}

// parseEncodingProbeOutput 解析子进程输出："<序号>\t<编码码>\t<原因码>"。
// 缺行＝没来得及回报（被杀/中途崩），由调用方按"探测未完成"处理。
func parseEncodingProbeOutput(out []byte, n int) map[int]encodingProbeResult {
	res := make(map[int]encodingProbeResult, n)
	for _, line := range strings.Split(string(out), "\n") {
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) < 2 {
			continue
		}
		i, err := strconv.Atoi(f[0])
		if err != nil || i < 0 || i >= n {
			continue
		}
		why := ""
		if len(f) > 2 {
			why = f[2]
		}
		res[i] = encodingProbeResult{enc: encodingFromProbeCode(f[1]), reason: why}
	}
	return res
}

// detectEncodingMode 决定一条 PS 5.1 命令该用哪种读面编码：
//   - 命令里出现的文本文件全是 GB 族 → gbk（含 .bat/.cmd 这类老文件）
//   - 全是 UTF-8 族 / 没有可探测文件 → utf8（默认）
//   - 混在一起 → utf8（多数资产是 UTF-8），由输出乱码检测 + 重跑兜底
//
// 总耗时上界 = 探测子进程预算（encodingProbeBudget）+ 强杀回收延迟；父进程本身
// 不做任何可能阻塞的文件系统调用。protectRoots 是助手自身配置/数据目录
// （App.protectPaths），子进程拒绝探测它们。
// hint 只在混合/未知/有候选被跳过/探测未完成时给出（正常情况为空，不制造噪音）。
func detectEncodingMode(ctx context.Context, cwd, command, shell string, protectRoots []string) (mode string, hint string) {
	if !isPS51Shell(shell) {
		return encodingModeUTF8, ""
	}
	// 会话已经取消：既不提取也不起子进程（绝不能"取消后还把进程起起来"）。
	if err := ctx.Err(); err != nil {
		return encodingModeUTF8, "会话已取消，已跳过编码探测"
	}
	paths, skipNote := extractPathCandidates(command, cwd)
	if len(paths) == 0 {
		return encodingModeUTF8, skipNote
	}
	run := spawnEncodingProbe(ctx, paths, protectRoots)
	if run.err != nil {
		return encodingModeUTF8, "无法完成编码探测（" + run.err.Error() + "）：本条按 UTF-8 读取，遇到乱码会自动换编码重跑一次"
	}
	if !run.reaped {
		return encodingModeUTF8, "探测子进程未能回收：本条按 UTF-8 读取，遇到乱码会自动换编码重跑一次"
	}
	res := parseEncodingProbeOutput(run.out, len(paths))
	var gbFiles, utf8Files, otherFiles []string
	gbEnc := EncUnknown
	note := skipNote
	for i, p := range paths {
		r, ok := res[i]
		if !ok {
			if note == "" {
				note = filepath.Base(p) + " 探测未完成（探测进程已终止），已跳过该候选"
			}
			otherFiles = append(otherFiles, p)
			continue
		}
		if r.reason != "" {
			if txt := probeReasonText(r.reason); txt != "" && note == "" {
				note = filepath.Base(p) + " " + txt
			}
			if r.reason == probeReasonMissing {
				continue // 不存在的路径既不算候选也不出提示（与旧行为一致）
			}
			otherFiles = append(otherFiles, p)
			continue
		}
		switch r.enc {
		case EncGBK, EncGB18030:
			if len(gbFiles) == 0 {
				gbEnc = r.enc
			}
			gbFiles = append(gbFiles, p)
		case EncUTF8, EncUTF8BOM, EncUTF16LE, EncUTF16BE:
			utf8Files = append(utf8Files, p)
		default:
			otherFiles = append(otherFiles, p)
		}
	}
	switch {
	case len(gbFiles) > 0 && len(utf8Files) == 0:
		mode = encodingModeGBK
	case len(gbFiles) > 0 && len(utf8Files) > 0:
		mode = encodingModeUTF8
		hint = fmt.Sprintf("命令同时涉及 UTF-8 与 GBK 文件（%s 是 %s）：本条按 UTF-8 读；读该文件请加 -Encoding Default 或改用 read 工具",
			filepath.Base(gbFiles[0]), gbEnc.String())
	case len(otherFiles) > 0 && len(utf8Files) == 0:
		mode = encodingModeUTF8
		hint = fmt.Sprintf("命令里的文件编码无法确定（%s）：按 UTF-8 读取，遇到乱码会自动换编码重跑一次", filepath.Base(otherFiles[0]))
	default:
		mode = encodingModeUTF8
	}
	if note != "" {
		if hint != "" {
			hint += "；"
		}
		hint += note
	}
	return mode, hint
}
