//go:build windows

package main

// 久等提醒（ask_alert）的 Windows 半边：把提醒顶到用户眼前。
//
// 用户口径：提醒浮窗是应用内 UI，软件不在前台/最小化时完全看不见（全屏变蓝也
// 白变），所以提醒触发时必须主动把窗口拉回来：
//  1. 最小化 → ShowWindow(SW_RESTORE) 还原；收进托盘/隐藏 → SW_SHOW 显示；
//  2. 在后台 → 抢前台（SetForegroundWindow）；Windows 有「前台锁」，用户正在
//     别的程序里操作时这一句常常直接失败，故先用经典的 AttachThreadInput 附着
//     到当前前台窗口的输入队列再置前，成功率高得多；
//  3. 仍失败（前台锁/别的程序全屏独占）→ 退化为任务栏按钮闪烁（FlashWindowEx，
//     一直闪到用户切回来）+ 托盘气泡通知（Shell_NotifyIcon NIF_INFO）。
//
// 只做「唤起注意力」，不改窗口的 topmost/尺寸：用户正在全屏工作时不会被一个
// 置顶窗口长期压住（切回来即恢复正常）。

import (
	"log"
	"os"
	"syscall"
	"unsafe"
)

var (
	alertUser32           = syscall.NewLazyDLL("user32.dll")
	procAttachThreadInput = alertUser32.NewProc("AttachThreadInput")
	procFlashWindowEx     = alertUser32.NewProc("FlashWindowEx")
	procAllowSetForegnd   = alertUser32.NewProc("AllowSetForegroundWindow")
	procIsWindowVisible   = alertUser32.NewProc("IsWindowVisible")
)

const (
	alertSwRestore = 9 // SW_RESTORE
	alertSwShow    = 5 // SW_SHOW
	// FLASHW_ALL（标题栏 + 任务栏按钮一起闪）| FLASHW_TIMERNOFG（一直闪，
	// 直到窗口被切到前台为止——比固定次数更可靠：人在别的程序里没留神时
	// 闪两下就停了，等于没提醒）
	alertFlashwAll        = 0x00000003
	alertFlashwTimerNoFG  = 0x0000000C
	alertAsfwAny         = ^uintptr(0) // ASFW_ANY（DWORD 0xFFFFFFFF）
	alertFlashCount      = 5           // TIMERNOFG 下这个次数不生效，留个合适值
)

// flashWindowInfo 镜像 FLASHWINFO（cbSize + hwnd + dwFlags + uCount + dwTimeout）。
type flashWindowInfo struct {
	cbSize    uint32
	hwnd      uintptr
	dwFlags   uint32
	uCount    uint32
	dwTimeout uint32
}

// alertMainHwnd 当前主窗口句柄：优先取窗口循环登记的（desktop_windows.go），
// 重建间隙里 HWND 还没登记时按标题找一次（与 activateExistingWindow 同源）。
func alertMainHwnd() uintptr {
	if hwnd := mainWndHwnd.Load(); hwnd != 0 {
		return hwnd
	}
	if title, err := syscall.UTF16PtrFromString(desktopAppTitle); err == nil {
		if hwnd, _, _ := procFindWindowW.Call(0, uintptr(unsafe.Pointer(title))); hwnd != 0 {
			return hwnd
		}
	}
	return 0
}

// alertForegroundIs hwnd 是不是当前前台窗口。
func alertForegroundIs(hwnd uintptr) bool {
	fg, _, _ := procGetForegroundWindow.Call()
	return fg != 0 && fg == hwnd
}

// alertOwnProcessInForeground 前台窗口是否本来就属于本进程（主窗口、模态对话框、
// 语音浮窗等）：是的话就不必抢前台，免得把用户正在用的窗口盖掉。
func alertOwnProcessInForeground() bool {
	fg, _, _ := procGetForegroundWindow.Call()
	if fg == 0 {
		return false
	}
	var pid uint32
	procGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&pid)))
	return pid != 0 && pid == uint32(os.Getpid())
}

// alertForceForeground 抢前台：先放宽本进程的前台权限，再附着到当前前台窗口的
// 输入队列（绕开前台锁），SetForegroundWindow + BringWindowToTop 后解附着。
// 返回是否真的成了前台（以 GetForegroundWindow 为准，不看调用返回值）。
func alertForceForeground(hwnd uintptr) bool {
	procAllowSetForegnd.Call(alertAsfwAny)
	fg, _, _ := procGetForegroundWindow.Call()
	var fgTid uintptr
	if fg != 0 {
		procGetWindowThreadProcessId.Call(fg, uintptr(unsafe.Pointer(&fgTid)))
	}
	cur, _, _ := procGetCurrentThreadID.Call()
	attached := false
	if fgTid != 0 && fgTid != cur {
		if r, _, _ := procAttachThreadInput.Call(fgTid, cur, 1); r != 0 {
			attached = true
		}
	}
	procSetForegroundWindow.Call(hwnd)
	procBringWindowToTop.Call(hwnd)
	if attached {
		procAttachThreadInput.Call(fgTid, cur, 0)
	}
	return alertForegroundIs(hwnd)
}

// alertFlashTaskbar 任务栏按钮闪烁（用户切到别的程序时唯一一定看得见的软提示）。
func alertFlashTaskbar(hwnd uintptr) {
	fi := flashWindowInfo{
		cbSize:  uint32(unsafe.Sizeof(flashWindowInfo{})),
		hwnd:    hwnd,
		dwFlags: alertFlashwAll | alertFlashwTimerNoFG,
		uCount:  alertFlashCount,
	}
	procFlashWindowEx.Call(uintptr(unsafe.Pointer(&fi)))
}

// alertFallbackNotify 置前做不到时的兜底：任务栏闪烁 + 托盘气泡通知。
func alertFallbackNotify(title, text string) {
	if hwnd := alertMainHwnd(); hwnd != 0 {
		alertFlashTaskbar(hwnd)
	}
	trayBalloon(title, text) // 托盘图标没挂上时它自己安静跳过
}

// pendingCardAlertFlash 卡片刚出现时的轻量唤起：只闪本进程登记的主窗口的任务栏
// 按钮，用户已经就在本程序前台时什么都不做。
//
// 与 alertBringToFront 的区别是「不打扰」：不抢前台、不还原窗口、不盖全屏蒙层、
// 不弹气泡——那套留给「等满 N 分钟仍无人处理」的久等提醒，卡片刚冒出来就抢窗口
// 会直接打断用户正在做的事。只用本进程登记的 HWND（不按标题跨进程找窗口）：
// 取不到就当没信号，叮声已经响过。
func pendingCardAlertFlash() {
	if alertOwnProcessInForeground() {
		return
	}
	if hwnd := mainWndHwnd.Load(); hwnd != 0 {
		alertFlashTaskbar(hwnd)
	}
}

// alertBringToFront 把提醒顶到用户眼前：还原/显示 → 抢前台 → 抢不到就闪+通知。
func alertBringToFront(title, text string) {
	hwnd := alertMainHwnd()
	if hwnd == 0 {
		// 桌面窗口还没建好（或非桌面形态，如浏览器模式）：只剩系统通知这一条
		log.Printf("[提醒] 取不到主窗口句柄：改用托盘通知兜底")
		trayBalloon(title, text)
		return
	}
	if r, _, _ := procIsIconic.Call(hwnd); r != 0 {
		procDesktopShowWindow.Call(hwnd, alertSwRestore) // 最小化 → 还原
	} else if r, _, _ := procIsWindowVisible.Call(hwnd); r == 0 {
		procDesktopShowWindow.Call(hwnd, alertSwShow) // 收进托盘 / 被隐藏 → 显示
	}
	if alertOwnProcessInForeground() || alertForegroundIs(hwnd) {
		return // 用户本来就在软件里：窗口已还原，不必再抢
	}
	if alertForceForeground(hwnd) {
		log.Printf("[提醒] 主窗口已置前（%s）", title)
		return
	}
	log.Printf("[提醒] 置前被系统前台锁拦下：改走任务栏闪烁 + 托盘通知")
	alertFallbackNotify(title, text)
}
