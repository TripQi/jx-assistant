//go:build windows

package main

// 询问提醒的声音：user32 MessageBeep（系统默认「叮」）与 winmm PlaySound（播
// 指定 wav）。纯 syscall，不走语音组件（朗读才走组件，见 voiceapi.go / 前端
// ask_alert 处理）。

import (
	"errors"
	"syscall"
	"unsafe"
)

var (
	procMessageBeep = syscall.NewLazyDLL("user32.dll").NewProc("MessageBeep")
	procPlaySound   = syscall.NewLazyDLL("winmm.dll").NewProc("PlaySoundW")
)

// PlaySoundW 的标志位：按文件名播、立即返回（异步，不等播完）、找不到文件时
// 不拿系统默认音顶替（要回落就由我们自己回落，好记日志）。
const (
	sndAsync     = 0x0001
	sndNoDefault = 0x0002
	sndFilename  = 0x00020000
)

// systemBeep 播一声系统提示音（MB_OK：默认「叮」；无声卡/静音时静默失败）。
func systemBeep() { procMessageBeep.Call(0) }

// wavPlay 异步播一个 wav：SND_FILENAME|SND_ASYNC|SND_NODEFAULT，调用即返回，
// 绝不在请求路径上等播放结束。没播上返回错误，由上层回落 MessageBeep 并记日志。
func wavPlay(path string) error {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return err
	}
	flags := uintptr(sndFilename | sndAsync | sndNoDefault)
	// 第三个参数（hmod）必须显式传 0：PlaySound 是 (pszSound, hmod, fdwSound)
	// 三个参数，少传一个会把标志位挤到 hmod 的位置上——现象就是“调用不报错、
	// 但永远返回失败”，声音怎么都出不来。
	ok, _, callErr := procPlaySound.Call(uintptr(unsafe.Pointer(p)), 0, flags)
	if ok == 0 {
		if callErr != nil && callErr != syscall.Errno(0) {
			return callErr
		}
		return errors.New("PlaySound 返回失败")
	}
	return nil
}
