package main

// 编码探测子进程的测试替身。
//
// 生产里探测子进程是「自身 exe + 隐藏子命令 __jx-probe-encoding」；测试里换成
// 「当前测试二进制 + helper 用例」——绝不能真把产品启动路径当子进程起（那会把
// 测试二进制当产品跑，递归执行整套测试）。替身与生产走同一份 spawnEncodingProbeProcess，
// 因此强杀、回收、耗时上限这些被测性质都是真的。

import (
	"os"
	"testing"
	"time"
)

const encodingProbeHelperRole = "encprobe-child"

// probeArgPresent 判断子进程参数里有没有某个角色词。
func probeArgPresent(want string) bool {
	for _, a := range os.Args {
		if a == want {
			return true
		}
	}
	return false
}

func TestMain(m *testing.M) {
	encodingProbeArgv = testProbeArgv(false)
	os.Exit(m.Run())
}

// testProbeArgv 给出替身子进程的命令行；block=true 时子进程启动后长时间不返回，
// 用来验证父侧的期限与强杀回收。
func testProbeArgv(block bool) func() (string, []string, error) {
	return func() (string, []string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", nil, err
		}
		args := []string{"-test.run=TestEncodingProbeHelperProcess", "--", encodingProbeHelperRole}
		if block {
			args = append(args, "block")
		}
		return exe, args, nil
	}
}

// useBlockedProbeChild 把探测子进程换成"起了就不返回"的版本。
func useBlockedProbeChild(t *testing.T) {
	t.Helper()
	old := encodingProbeArgv
	encodingProbeArgv = testProbeArgv(true)
	t.Cleanup(func() { encodingProbeArgv = old })
}

// TestEncodingProbeHelperProcess 是子进程本体：不是子进程就直接返回（父侧跑测试
// 时它什么也不做）。
func TestEncodingProbeHelperProcess(t *testing.T) {
	if !probeArgPresent(encodingProbeHelperRole) {
		return
	}
	if probeArgPresent("block") {
		// 不能用 select{}：单 goroutine 阻塞会触发运行时 deadlock 检测直接退出。
		time.Sleep(10 * time.Minute)
		os.Exit(9)
	}
	os.Exit(runEncodingProbeChild(os.Stdin, os.Stdout))
}
