package main

// exitguard（退出收口 + 结算纪律）的验收测试：
//  1. jobRun.stop：真后台作业（长跑命令）stop 后 done 在时限内关闭——结算纪律
//     的含义就是「终止信号发出后等到进程树死透才返回」（旧实现只发 cancel 不等，
//     done 可能仍开着）；stop 已结束的作业应立即返回（幂等）；
//  2. shutdownJobs：台账上有在跑作业时逐一停止并结算；没有作业时安静返回；
//  3. logTopLevelJobRemaining：顶层 Job 未建立（测试进程不跑 setup）时为无操作，
//     不 panic——保证任何调用路径安全。

import (
	"runtime"
	"testing"
	"time"
)

func newJobsTestApp(t *testing.T) *App {
	t.Helper()
	a := &App{Cfg: &Config{DataDir: t.TempDir(), Workspace: t.TempDir()}}
	a.jobs = map[string]*jobRun{}
	return a
}

// longRunCmd 选一条「能跑很久、经 shell 起也立刻活着」的命令：结算纪律关心
// 的是进程树终止时序，不需要真实负载。
func longRunCmd() string {
	if runtime.GOOS == "windows" {
		return "ping -n 9999 127.0.0.1"
	}
	return "sleep 300"
}

func startLongJob(t *testing.T, a *App) *jobRun {
	t.Helper()
	tctx := &toolContext{app: a, sess: &SessionMeta{ID: "smoke-sess"}}
	j, err := a.startJob(tctx, longRunCmd(), "", a.Cfg.Workspace, permFull, "command", nil)
	if err != nil {
		t.Fatalf("startJob 失败: %v", err)
	}
	t.Cleanup(func() { j.stop(5 * time.Second) })
	return j
}

func TestJobStopSettles(t *testing.T) {
	a := newJobsTestApp(t)
	j := startLongJob(t, a)

	j.stop(10 * time.Second) // 结算纪律：返回时必须已死透

	select {
	case <-j.done:
	default:
		t.Fatalf("stop 返回后 done 仍开着：终止信号已发但未等到进程树退出（结算纪律失效）")
	}
	if st := j.Status(); st == "running" {
		t.Fatalf("stop 后状态仍是 running: %s", st)
	}
	// 幂等：已结束的作业再 stop 立即返回（不阻塞、不 panic）
	done := make(chan struct{})
	go func() { j.stop(10 * time.Second); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("对已结束作业的第二次 stop 未立即返回")
	}
}

func TestShutdownJobsStopsAll(t *testing.T) {
	a := newJobsTestApp(t)
	j1 := startLongJob(t, a)
	j2 := startLongJob(t, a)

	a.shutdownJobs() // 退出收口：全部停止并结算（+ 自检日志，顶层 Job 未建时应安静）

	for i, j := range []*jobRun{j1, j2} {
		select {
		case <-j.done:
		default:
			t.Fatalf("shutdownJobs 后作业 %d 的 done 仍开着", i+1)
		}
	}
}

func TestShutdownJobsIdleQuiet(t *testing.T) {
	a := newJobsTestApp(t)
	done := make(chan struct{})
	go func() { a.shutdownJobs(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("空台账的 shutdownJobs 未及时返回")
	}
}
