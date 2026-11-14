//go:build !windows

package main

import "errors"

// 非 Windows 桌面壳不存在（runDesktop 直接报错），/api/clipboard 只服务桌面
// 壳的右键「粘贴」；其余平台给出明确错误，前端会静默（无操作）。
var errClipboardBusy = errors.New("当前平台不支持读取系统剪贴板")

func readClipboardWindows() (text string, png []byte, files []string, err error) {
	return "", nil, nil, errClipboardBusy
}
