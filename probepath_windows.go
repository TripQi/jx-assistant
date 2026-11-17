//go:build windows

package main

// 编码探测候选的路径闸门（Windows）。
//
// 为什么按名字先判：探测会真正打开并读取候选文件，而候选是从命令正文里捞出来的
// ——提交说明、注释、模型随手写的说明都可能出现"看着像文件"的名字。DOS 保留
// 设备名带任何扩展名都仍指向设备（AUX.md 就是 AUX 设备），设备命名空间前缀与
// 备用数据流同理；这类路径 Stat/Open/Read 会摸到真实设备并可能永久阻塞，且发生
// 在 run_command 起子进程之前。所以这类名字连 Stat 都不做（见 encoding.go 的
// 三道闸门）。

import (
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

var procGetDriveTypeW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

// driveRemote 是 GetDriveTypeW 的 DRIVE_REMOTE：网络卷上的 Stat/Open 可能长时间
// 不返回（断连的映射盘），与设备同类——探测不碰。
const driveRemote = 4

// dosDeviceNames 是 DOS 保留设备名：不区分大小写，带扩展名或尾随点也算设备。
var dosDeviceNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"CLOCK$": true, "CONIN$": true, "CONOUT$": true,
}

// probePathBlocked 返回该候选不能探测的原因（空串=可以安全探测）。
func probePathBlocked(p string) string {
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
	// 映射网络盘：断连时 Stat/Open 能卡很久
	if vol := filepath.VolumeName(p); vol != "" {
		if vp, err := syscall.UTF16PtrFromString(vol + `\`); err == nil {
			if typ, _, _ := procGetDriveTypeW.Call(uintptr(unsafe.Pointer(vp))); typ == driveRemote {
				return "网络驱动器上的文件"
			}
		}
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
