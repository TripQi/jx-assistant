//go:build !windows

package main

// 非 Windows：既无 MessageBeep 也无 winmm。提醒的声音空实现（闪烁、蒙层与
// 朗读不受影响），与 recorder_other.go 同款跨平台桩纪律。

import "errors"

func systemBeep() {}

// wavPlay 非 Windows 不做实际播放：返回错误让上层按「播放失败」回落 systemBeep
// （同样是空实现），日志里写清是本平台不支持——空着不出声，但不静默。
func wavPlay(path string) error {
	return errors.New("当前平台不支持 wav 提示音（只有 Windows 有 winmm）")
}
