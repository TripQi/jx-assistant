package main

// 起探测子进程：硬期限与取消都由"杀进程 + Wait"实现。
//
// 为什么不是 goroutine 看门狗：父侧主流程阻塞在 cmd.Wait() 上，期限/取消通过
// time.AfterFunc / context.AfterFunc 调 TerminateProcess——进程一死 Wait 必返，
// 没有任何"被放弃的 goroutine"和外泄的句柄可言。这条路径在隔离验证里实测：
// 预算 1.5s 时子进程被杀的调用实际 1.5048s 返回，Wait 确认回收，goroutine 数
// 回到基线（见 encodingprobe_guard_test.go）。

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"
)

// encodingProbeBudget 是一条命令的全部候选共用的硬期限（含子进程启动开销）。
// 隔离实测：44MB 的自身 exe 冷启一次约 12~22ms，1.5s 足够 8 个候选读完头部。
const encodingProbeBudget = 1500 * time.Millisecond

// encodingProbeResult 是单个候选的探测结果（子进程回报）。
type encodingProbeResult struct {
	enc    TextEncoding
	reason string
}

// encodingProbeRun 是一次子进程探测的收尾状态。
type encodingProbeRun struct {
	out    []byte
	killed bool // 是我们按期限/取消杀的
	reaped bool // Wait 已返回、进程已回收
	err    error
}

// encodingProbeArgv 决定"用哪个可执行文件、什么参数"起探测子进程：生产是自身
// exe + 隐藏子命令；测试换成当前测试二进制 + helper 用例（绝不能真把产品当
// 测试子进程启动）。
var encodingProbeArgv = func() (string, []string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	return exe, []string{encodingProbeSubcommand}, nil
}

// spawnEncodingProbe 是父侧唯一的进程动作（测试替换点）。
var spawnEncodingProbe = spawnEncodingProbeProcess

// probePayload 拼子进程输入：首行协议头（受保护目录）+ 每行一个候选。
func probePayload(paths []string, protectRoots []string) string {
	var b strings.Builder
	b.WriteString(encodingProbeProtocolV1)
	for _, r := range protectRoots {
		if r != "" {
			b.WriteString("\t")
			b.WriteString(r)
		}
	}
	b.WriteString("\n")
	for _, p := range paths {
		b.WriteString(p)
		b.WriteString("\n")
	}
	return b.String()
}

// spawnEncodingProbeProcess 起子进程、按期限/取消强杀、等它回收。
func spawnEncodingProbeProcess(ctx context.Context, paths []string, protectRoots []string) encodingProbeRun {
	exe, args, err := encodingProbeArgv()
	if err != nil {
		return encodingProbeRun{err: err}
	}
	cmd := exec.Command(exe, args...)
	hideConsoleWindow(cmd)
	cmd.Stdin = strings.NewReader(probePayload(paths, protectRoots))
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Start(); err != nil {
		return encodingProbeRun{err: err}
	}
	var killed atomic.Bool
	kill := func() {
		killed.Store(true)
		_ = cmd.Process.Kill()
	}
	timer := time.AfterFunc(encodingProbeBudget, kill)
	defer timer.Stop()
	stopCtx := context.AfterFunc(ctx, kill)
	defer stopCtx()
	werr := cmd.Wait()
	state := cmd.ProcessState
	run := encodingProbeRun{out: out.Bytes(), killed: killed.Load(), reaped: state != nil}
	if run.killed {
		return run // 主动杀的：原因由父侧文案说明，不算启动失败
	}
	if werr != nil {
		run.err = werr
	}
	return run
}
