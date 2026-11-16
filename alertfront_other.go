//go:build !windows

package main

// 非 Windows 平台：没有桌面窗口/托盘可唤起（安卓/ Linux 下提醒浮窗由前端全屏
// 呈现，用户切走的情况交给系统自身的通知机制），这里是空实现，保持接口一致。

func alertBringToFront(title, text string) {}

func alertFallbackNotify(title, text string) {}

// pendingCardAlertFlash 卡片刚出现时的轻量唤起：非 Windows 平台无此实现
// （提醒浮窗由前端全屏呈现，闪不闪跟着前端），空实现保持接口一致。
func pendingCardAlertFlash() {}
