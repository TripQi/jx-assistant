//go:build linux

package main

// 本地 Linux 沙箱：经主程序自重入 sandboxhelper 的 Landlock 限制器，
// 限制施加后 exec 目标命令（Go 无法 fork，自重入是最干净的路径）。

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// spawnConfinedLinuxReexec 在 Landlock 沙箱内执行一条命令。
func spawnConfinedLinuxReexec(r sandboxRun) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("sandbox unavailable: 定位自身失败: %w", err)
	}
	sh := "/bin/sh"
	if _, err := os.Stat(sh); err != nil {
		sh, err = exec.LookPath("sh")
		if err != nil {
			return "", fmt.Errorf("sandbox unavailable: 找不到 sh: %w", err)
		}
	}
	// 私有临时目录由父进程创建并在退出后清理（与 Windows 后端行为对齐），
	// 不再用 --temp auto（helper exec 后无人清理，会泄漏 /tmp 目录）
	tempDir, err := os.MkdirTemp("", "jx-sbx-")
	if err != nil {
		return "", fmt.Errorf("sandbox unavailable: 创建私有临时目录失败: %w", err)
	}
	defer os.RemoveAll(tempDir)
	wrapped := "cd " + shellQuote(r.Dir) + " && { " + r.Command + " ;}"
	args := []string{
		"__jx-sandbox-restrict", "restrict",
		"--mode", r.Policy, "--workspace", r.Workspace, "--temp", tempDir,
		"--", sh, "-c", wrapped,
	}
	cmd := exec.Command(exe, args...)
	cmd.Dir = r.Dir
	cmd.Env = sandboxEnv(false)
	out, err := cmd.CombinedOutput()
	// 先解码再截断：字节层截断会把 GBK / UTF-8 多字节序列切断，尾部变乱码
	res := clipHeadTail(strings.TrimRight(decodeCmdOutput(out), "\r\n"), cmdOutputMaxBytes, cmdTruncNote)
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			res += fmt.Sprintf("\n[exit code %d]", ee.ExitCode())
		} else {
			return "", fmt.Errorf("sandbox unavailable: 启动受限进程失败: %w", err)
		}
	}
	if strings.TrimSpace(res) == "" {
		res = "（命令执行完成，无输出）"
	}
	return res, nil
}
