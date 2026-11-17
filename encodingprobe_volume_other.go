//go:build !windows

package main

// 探测子进程的盘类型闸门（非 Windows）：没有"映射网络盘"这种盘符语义，
// 网络挂载点由名称闸门（/dev、/proc、/sys）与普通文件确认兜住，这里不额外拦。
func probeVolumeBlocked(p string) bool { return false }
