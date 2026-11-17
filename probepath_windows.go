//go:build windows

package main

// 编码探测候选的**名称**闸门（Windows，纯字符串，不碰文件系统）。
//
// 为什么按名字先判：候选是从命令正文（提交说明、注释、模型随手写的说明）里
// 捞出来的，而 DOS 保留设备名带任何扩展名都仍指向设备（AUX.md 就是 AUX 设备），
// 设备命名空间前缀与备用数据流同理。这类路径 Stat/Open/Read 会摸到真实设备并
// 可能永久阻塞，且发生在 run_command 起命令之前——所以连 Stat 都不做，直接在
// 父侧（纯字符串阶段）与子进程的第一道闸门拒掉。
// 盘类型（网络映射盘）检查不在本文件：它要查系统、必须放在可强杀的探测子进程
// 里（见 encodingprobe_volume_windows.go）。

import (
	"path/filepath"
	"strings"
)

// dosDeviceNames 是 DOS 保留设备名：不区分大小写，带扩展名或尾随点也算设备。
var dosDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CLOCK$": true, "CONIN$": true, "CONOUT$": true,
}

// probeNameBlocked 返回该候选"按名字就不能探测"的原因（空串＝名字层面放行）。
// 纯字符串判定：不 Stat、不查盘类型。
func probeNameBlocked(p string) string {
	if p == "" {
		return ""
	}
	// \\?\ 与 \\.\ 是设备命名空间（直达设备对象），\\server\share 是网络路径
	if strings.HasPrefix(p, `\\`) {
		return "设备命名空间或网络路径"
	}
	// 备用数据流（D:\a\b.txt:stream）：冒号只允许出现在盘符位置
	if rest := strings.TrimPrefix(p, filepath.VolumeName(p)); strings.ContainsRune(rest, ':') {
		return "备用数据流路径"
	}
	// Windows 忽略文件名尾部空格与点，且只看扩展名之前的主干判断设备名
	stem := filepath.Base(p)
	if i := strings.IndexByte(stem, '.'); i >= 0 {
		stem = stem[:i]
	}
	stem = strings.ToUpper(strings.TrimRight(stem, " ."))
	if dosDeviceNames[stem] || isComLptDevice(stem) {
		return "Windows 保留设备名"
	}
	return ""
}

// isComLptDevice 判 COM1-9 / LPT1-9（含 Windows 认可的 ¹²³ 上标写法）。
func isComLptDevice(stem string) bool {
	if !strings.HasPrefix(stem, "COM") && !strings.HasPrefix(stem, "LPT") {
		return false
	}
	rest := stem[3:]
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if !strings.ContainsRune("0123456789¹²³", r) {
			return false
		}
	}
	return true
}
