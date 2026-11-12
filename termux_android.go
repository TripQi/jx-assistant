//go:build android

package main

// 安卓：内置运行时（Termux 快照）的就绪入口与路径定位。
//
// 快照文件由 Kotlin 壳（EngineService.copyRuntimeAssets）首启从 APK assets
// 拷贝到引擎数据目录：runtime.tgz 与 runtime-MANIFEST.json；之后由 Go 侧
// termuxEnsure 解压装配到 dataDir/runtime/（见 termux.go）。

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
)

// termuxRuntimePaths 返回（快照、清单、装配目标）路径。
func termuxRuntimePaths(a *App) (tarGz, manifest, prefixRoot string) {
	base := a.Cfg.DataDir
	return filepath.Join(base, "runtime.tgz"),
		filepath.Join(base, "runtime-MANIFEST.json"),
		filepath.Join(base, "runtime")
}

// termuxEnsureFor 确保内置运行时就绪并返回 prefix（含首次解压装配）。
func termuxEnsureFor(a *App, emit func(string)) (string, error) {
	tarGz, manifest, prefixRoot := termuxRuntimePaths(a)
	if _, err := os.Stat(tarGz); err != nil {
		return "", fmt.Errorf("运行时快照缺失（壳未拷贝 assets？）: %w", err)
	}
	return termuxEnsure(manifest, tarGz, prefixRoot, emit)
}

// applyTermuxProcAttr 给内置运行时命令挂进程组（命令收尾时组杀清理残余）。
func applyTermuxProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killTermuxProcGroup 组杀清理命令进程组残余（幂等；命令已退出也安全）。
func killTermuxProcGroup(cmd *exec.Cmd) {
	if cmd.Process != nil {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}

// runTermuxBash 用内置运行时执行命令（与 runUnconfinedLocal 同语义，仅替换
// shell 与执行环境）。进程组收尾：命令结束后组杀清理残余子孙进程（对齐桌面
// 沙箱的 kill-on-close 语义；常驻进程不会被保留）。
func runTermuxBash(ctx context.Context, prefix string, r sandboxRun) (string, error) {
	// 先建环境（termuxEnv 顺带确保 home/tmp 在位）再验工作目录：首条命令
	// 时 home 尚未创建，先 Stat 会误报「工作目录不存在」
	env := termuxEnv(prefix)
	if info, serr := os.Stat(r.Dir); serr != nil || !info.IsDir() {
		return "", fmt.Errorf("工作目录不存在: %s", r.Dir)
	}
	ctx, cancel := context.WithTimeout(ctx, r.Timeout)
	defer cancel()
	bash := filepath.Join(prefix, "bin", "bash")
	wrapped := "cd " + shellQuote(r.Dir) + " && { " + r.Command + " ;}"
	cmd := exec.CommandContext(ctx, bash, "-lc", wrapped)
	cmd.Dir = r.Dir
	cmd.Env = env
	applyTermuxProcAttr(cmd)
	out, err := cmd.CombinedOutput()
	// 组杀清理：命令结束（含超时被终止）后回收该进程组残余
	killTermuxProcGroup(cmd)
	note := ""
	if ctx.Err() == context.DeadlineExceeded {
		note += "\n[命令超时被终止]"
	}
	// 先解码再截断：字节层截断会把 GBK / UTF-8 多字节序列切断，尾部变乱码
	res := clipHeadTail(strings.TrimRight(decodeCmdOutput(out), "\r\n"), cmdOutputMaxBytes, cmdTruncNote) + note
	if err != nil && ctx.Err() == nil {
		res += fmt.Sprintf("\n[进程退出异常: %v]", err)
	}
	if strings.TrimSpace(res) == "" {
		res = "（命令执行完成，无输出）"
	}
	return res, nil
}
