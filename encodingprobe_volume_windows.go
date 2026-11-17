//go:build windows

package main

// 探测子进程的盘类型闸门（Windows）。
//
// 为什么查盘类型：断连的映射网络盘上 Stat/Open 能卡很久，与设备同类。这一步
// 放在子进程里做（它可被强杀），父侧只按名字排除设备/命名空间/ADS。

import (
	"path/filepath"
	"syscall"
	"unsafe"
)

var procProbeGetDriveTypeW = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDriveTypeW")

// probeDriveRemote 是 GetDriveTypeW 的 DRIVE_REMOTE。
const probeDriveRemote = 4

// probeVolumeBlocked 报告该路径是否落在网络驱动器上（query 失败按不拦处理：
// 未知盘符的路径后面还有存在性/普通文件确认兜着）。
func probeVolumeBlocked(p string) bool {
	vol := filepath.VolumeName(p)
	if vol == "" {
		return false
	}
	vp, err := syscall.UTF16PtrFromString(vol + `\`)
	if err != nil {
		return true
	}
	typ, _, _ := procProbeGetDriveTypeW.Call(uintptr(unsafe.Pointer(vp)))
	return typ == probeDriveRemote
}
