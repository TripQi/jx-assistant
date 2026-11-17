//go:build !windows

package main

// 编码探测候选的路径闸门（非 Windows）。
//
// 与 Windows 的保留设备名同类：Linux/Android 上的字符设备（/dev/tty、
// /dev/random 等）一读就可能阻塞或永远读不完，内核伪文件同样不适合拿来当
// "文本文件头部"探测。这类路径先排除，安全普通文件才允许 Stat/Open
// （见 encoding.go 的三道闸门）。

import "strings"

// probePathBlocked 返回该候选不能探测的原因（空串=可以安全探测）。
func probePathBlocked(p string) string {
	for _, root := range []string{"/dev/", "/proc/", "/sys/"} {
		if strings.HasPrefix(p, root) {
			return "设备或内核伪文件系统路径"
		}
	}
	return ""
}
