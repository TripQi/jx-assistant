//go:build windows

package main

// 摘除连接器要把在握手中的子进程真的收掉（不只是返回得快）：否则只是把一次
// 锁卡顿换成后台进程残留。Windows 上拿句柄 + WaitForSingleObject(0) 判退出。

import (
	"syscall"
	"testing"
	"time"
)

const mcpWaitObject0 = 0 // WAIT_OBJECT_0

func mcpProcessExited(pid int) bool {
	h, err := syscall.OpenProcess(syscall.PROCESS_QUERY_INFORMATION|syscall.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return true // 句柄打不开：进程已不在
	}
	defer syscall.CloseHandle(h)
	st, err := syscall.WaitForSingleObject(h, 0)
	return err == nil && st == mcpWaitObject0
}

func mcpProcessExitedWithin(pid int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if mcpProcessExited(pid) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestMCPHungChildKilledOnRemove(t *testing.T) {
	r := newTestMCPR(t)
	if _, err := r.UpsertFromDefs(hungStdioDef(), "market:hung", "握手不返回的连接器"); err != nil {
		t.Fatalf("注册失败: %v", err)
	}
	var pid int
	waitMCP(t, 10*time.Second, func() bool { pid = hungMarker(r); return pid != 0 }, "握手没有进入进行中状态")
	if mcpProcessExited(pid) {
		t.Fatalf("握手进行中子进程不该已退出: pid=%d", pid)
	}
	if removed := r.RemoveBySource("market:hung"); len(removed) != 1 {
		t.Fatalf("摘除失败: %v", removed)
	}
	if !mcpProcessExitedWithin(pid, 3*time.Second) {
		t.Fatalf("摘除后握手子进程仍在运行: pid=%d", pid)
	}
}
