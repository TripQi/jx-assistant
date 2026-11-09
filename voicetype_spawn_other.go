//go:build !windows

package main

// voicetype_spawn_other.go —— 独立语音输入法（VoiceTypeGo）自动拉起的非 Windows 桩。
//
// 输入法本体是 Windows 程序（全局键盘钩子 + WebView2 悬浮胶囊），Linux/Android
// 上不存在也不该拉：spawnVoiceType 记一行日志即返回；历史记录（voicetype_history.go）
// 因 voiceTypeCandidates 为空而自然返回空列表，无需特判。

import "log"

// voiceTypeCandidates 非 Windows 恒无候选（输入法 exe 只随 Windows 安装包分发）。
func voiceTypeCandidates() []string { return nil }

// voiceTypeRunning 非 Windows 恒为未运行。
func voiceTypeRunning() bool { return false }

// spawnVoiceType 非 Windows 不拉起（组件开关照常生效，只是没有可拉的东西）。
func (a *App) spawnVoiceType() {
	log.Printf("[voicetype] 独立语音输入法仅支持 Windows，本平台跳过自动拉起")
}

// stopVoiceTypeByComponent 非 Windows 无子进程可收。
func stopVoiceTypeByComponent() {}

// stopVoiceType 非 Windows 无子进程可收。
func stopVoiceType() {}
