//go:build !windows

package main

// 非 Windows 平台的编译占位：运行时不会走到（spawnConfinedLocal 按
// isWindowsRuntime 分派），但 Linux 构建仍需要符号存在。

import (
	"context"
	"fmt"
	"io"
	"time"
)

// confinedChildStub 与 Windows 侧 confinedChild 同形（占位）。
type confinedChild struct {
	proc, thread uintptr
	stdoutR      io.Closer
	stderrR      io.Closer
	tempDir      string
}

// osFile 在非 Windows 平台对 *os.File 的占位别名。
type osFile = io.Closer

func spawnConfinedWindows(ctx context.Context, r sandboxRun) (string, error) {
	return "", fmt.Errorf("sandbox unavailable: Windows 沙箱后端不可用")
}

func startConfinedWindows(r sandboxRun) (*confinedChild, error) {
	return nil, fmt.Errorf("sandbox unavailable: Windows 沙箱后端不可用")
}

func (c *confinedChild) drain(w io.Writer)                                    {}
func (c *confinedChild) wait(ctx context.Context, timeout time.Duration) bool { return false }
func (c *confinedChild) exitText() string                                     { return "" }
func (c *confinedChild) cleanup()                                             {}
