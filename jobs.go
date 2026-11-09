package main

// 会话内后台作业（对齐官方标准栈的 run_in_background + job_* 工具族）：
// run_command 带 run_in_background=true 时不等待命令结束，立即返回作业 id，
// 命令在后台继续跑（不限时）；模型用 job_output 读输出（可阻塞等待）、
// job_list 列出本会话的作业、job_kill 终止。输出进内存环形缓冲，上限
// 64KB（与前台的 run_command 同口径；曾为 512KB，一次 job_output 就能吃掉
// 大半个上下文窗口——ERP 会话实测单次 50 万字），超出截头。作业仅本地执行
//（SSH 远程会话不支持，返回明确错误）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	jobBufMax = 64 * 1024 // 单作业输出缓冲上限（与前台命令 64KB 同口径，防单次读走半个上下文）
	jobKeep   = time.Hour  // 已结束作业的保留时长（之后在新作业启动时清理）
)

// lockedBuffer 是并发安全的输出缓冲，超过上限时丢弃最早的内容。
type lockedBuffer struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	drop int // 已丢弃的字节数
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.buf.Write(p)
	if b.buf.Len() > jobBufMax {
		drop := b.buf.Len() - jobBufMax
		b.buf.Next(drop)
		b.drop += drop
	}
	return len(p), nil
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	// 控制台字节按 UTF-8/GB18030 还原（与前台 runCommand 同规则）：
	// 缓冲里是原始字节，这里是对外的唯一字符串出口，解码只此一处
	s := decodeCmdOutput(b.buf.Bytes())
	if b.drop > 0 {
		return fmt.Sprintf("…（开头 %d 字节已被截断）\n%s", b.drop, s)
	}
	return s
}

// jobRun 是一个后台作业的运行时记录。
type jobRun struct {
	ID        string
	SessionID string
	Command   string
	Cwd       string
	Policy    string // 本次作业的沙箱档位（job_output 附加拒绝/故障 marker 用）
	Origin    string // 启动来源：command（run_command）| python:<脚本路径>（run_python 后台）
	startedAt time.Time
	buf       *lockedBuffer
	cancel    context.CancelFunc
	done      chan struct{}
	mu        sync.Mutex
	exitInfo  string // 空=仍在运行；否则 "exit 0" / "exit 3" / "超时" 等
	onDone    func(j *jobRun) // 收尾回调（startJob 的调用方注入；仅 run_python 后台用于【后台脚本回报】）
}

func (j *jobRun) Status() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.exitInfo == "" {
		return "running"
	}
	return "done: " + j.exitInfo
}

func (j *jobRun) setExit(info string) {
	j.mu.Lock()
	if j.exitInfo == "" {
		j.exitInfo = info
	}
	j.mu.Unlock()
	close(j.done)
}

// startJob 启动一个后台命令（权限已由调用方 authorize 并核销凭证）。
// policy 为 full 时不进沙箱；其余档位走 OS 级沙箱后端（与前台同路径）。
// origin 标记启动来源（job_list 展示与收尾回报区分用）；onDone 在作业收尾
// （成功/失败/被终止都算）时回调，可为 nil。
func (a *App) startJob(t *toolContext, command, shell, cwd, policy, origin string, onDone func(j *jobRun)) (*jobRun, error) {
	if info, serr := os.Stat(cwd); serr != nil || !info.IsDir() {
		return nil, fmt.Errorf("工作目录不存在: %s", cwd)
	}
	a.jobsMu.Lock()
	// 顺手清理超过保留期的已结束作业
	now := time.Now()
	for id, j := range a.jobs {
		select {
		case <-j.done:
			if now.Sub(j.startedAt) > jobKeep {
				delete(a.jobs, id)
			}
		default:
		}
	}
	j := &jobRun{
		ID: "job-" + newID(), SessionID: t.sess.ID, Command: command, Cwd: cwd, Policy: policy,
		Origin: origin, startedAt: time.Now(), buf: &lockedBuffer{}, done: make(chan struct{}),
	}
	a.jobs[j.ID] = j
	a.jobsMu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	// 后台作业同样走「子进程统一入口」（child.go）：沙箱分派、压窗、输出捕获、
	// 「运行中的程序」登记都在那一处；不限时，终止只经 ctx（job_kill）。
	spec := sandboxRun{Dir: cwd, Command: command, Shell: shell, Policy: policy,
		Workspace: t.workspace(), Remote: t.sess, Label: "job:run_in_background"}
	wait, err := a.startChild(ctx, spec, j.buf)
	if err != nil {
		a.jobsMu.Lock()
		delete(a.jobs, j.ID)
		a.jobsMu.Unlock()
		cancel()
		return nil, err
	}
	go func() {
		defer cancel()
		wait()
		if ctx.Err() != nil {
			j.setExit("被终止（job_kill 或会话停止）")
			return
		}
		// 沙箱路径的 wait 不携带真实退出码（文案照旧），但补一行日志留痕：
		// 「作业启动后秒退」类异常（2026-10-17 曾发生一次、不可复现）从此可从
		// 日志对账定位，不再查无实据。运行时长是关键信号：秒退 = 命令没真跑起来。
		dur := time.Since(j.startedAt).Round(time.Second)
		j.setExit("exit 0")
		log.Printf("[作业] %s（%s）正常结束，运行 %s", j.ID, truncateRunes(j.Command, 60), dur)
		if onDone != nil {
			onDone(j) // 【后台脚本回报】等收尾动作：此时输出缓冲已齐、exitInfo 已定
		}
	}()
	return j, nil
}

// stopSessionJobs 终止某会话全部仍在运行的后台作业，返回终止数。
// 会话停止 = 「这一摊全停」（产品决策：运行逻辑清晰明确优先于作业跨轮存活），
// 由 stopRun 统一收口调用；正常轮末（finishRun）不走这里，作业照旧跨轮存活。
// stop 终止一个仍在运行的作业并（限时）等待进程树真正退出——结算纪律：
// cancel 后内核异步终止 Job 内全部进程（含孙进程），done 才代表「已死透」。
// 超时不阻塞调用方：退出/停止路径超时后继续走，顶层 Job 兜底清场。
func (j *jobRun) stop(wait time.Duration) {
	select {
	case <-j.done:
		return
	default:
	}
	j.cancel()
	if wait <= 0 {
		return
	}
	select {
	case <-j.done:
	case <-time.After(wait):
	}
}

func (a *App) stopSessionJobs(id string) int {
	a.jobsMu.Lock()
	mine := []*jobRun{}
	for _, j := range a.jobs {
		if j.SessionID == id {
			mine = append(mine, j)
		}
	}
	a.jobsMu.Unlock()
	n := 0
	for _, j := range mine {
		select {
		case <-j.done:
			continue
		default:
		}
		j.stop(3 * time.Second) // 发终止并限时等死透：结算纪律与 job_kill 同款
		n++
	}
	if n > 0 {
		log.Printf("[停止] 会话 %s 的 %d 个后台作业已一并终止", id, n)
	}
	return n
}

func (a *App) getJob(id, sessionID string) (*jobRun, error) {
	a.jobsMu.Lock()
	defer a.jobsMu.Unlock()
	j, ok := a.jobs[id]
	if !ok || j.SessionID != sessionID {
		return nil, fmt.Errorf("作业 %s 不存在（或属于其他会话）", id)
	}
	return j, nil
}

// ---------- 工具 ----------

func jobOutputTool() Tool {
	return Tool{
		Spec: toolSpec("job_output", "读取后台作业的输出。默认立即返回当前已有输出与运行状态；wait=true 时最多阻塞等待 timeout_sec 秒（默认 30，上限 120）直到作业结束。",
			objSchema(map[string]any{
				"id":          strProp("作业 id，来自 run_command(run_in_background) 的返回"),
				"wait":        boolProp("是否阻塞等待作业结束，默认 false"),
				"timeout_sec": intProp("wait=true 时的等待上限秒数，默认 30"),
			}, "id")),
		Run: runJobOutput,
	}
}

func runJobOutput(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		ID        string `json:"id"`
		Wait      bool   `json:"wait"`
		TimeoutSc int    `json:"timeout_sec"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	j, err := t.app.getJob(strings.TrimSpace(p.ID), t.sess.ID)
	if err != nil {
		return "", err
	}
	if p.Wait {
		timeout := time.Duration(30) * time.Second
		if p.TimeoutSc > 0 {
			timeout = time.Duration(min(p.TimeoutSc, 120)) * time.Second
		}
		select {
		case <-j.done:
		case <-time.After(timeout):
		case <-t.ctx.Done():
		}
	}
	// 沙箱作业的输出与前台同规则附加拒绝/执行器故障 marker（对齐官方
	// renderProcessRead 在每次读取时附加 notice），模型读到才有正确方向。
	out := j.buf.String()
	if j.Policy != "" && j.Policy != permFull {
		out += runDeniedMarker(j.Policy, out, t.isRemote())
	}
	return fmt.Sprintf("作业 %s｜状态：%s\n输出：\n%s", j.ID, j.Status(), out), nil
}

func jobListTool() Tool {
	return Tool{
		Spec: toolSpec("job_list", "列出本会话的全部后台作业（id、状态、命令、启动时间）。", objSchema(map[string]any{})),
		Run:  runJobList,
	}
}

func runJobList(t *toolContext, _ json.RawMessage) (string, error) {
	t.app.jobsMu.Lock()
	mine := []*jobRun{}
	for _, j := range t.app.jobs {
		if j.SessionID == t.sess.ID {
			mine = append(mine, j)
		}
	}
	t.app.jobsMu.Unlock()
	if len(mine) == 0 {
		return "（本会话没有后台作业）", nil
	}
	lines := []string{}
	for _, j := range mine {
		lines = append(lines, fmt.Sprintf("- %s｜%s｜启动于 %s｜%s", j.ID, j.Status(), j.startedAt.Format("15:04:05"), truncateRunes(j.Command, 80)))
	}
	return strings.Join(lines, "\n"), nil
}

func jobKillTool() Tool {
	return Tool{
		Spec: toolSpec("job_kill", "终止一个还在运行的后台作业。已结束的作业无法终止。",
			objSchema(map[string]any{
				"id": strProp("作业 id"),
			}, "id")),
		Run: runJobKill,
	}
}

func runJobKill(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	j, err := t.app.getJob(strings.TrimSpace(p.ID), t.sess.ID)
	if err != nil {
		return "", err
	}
	select {
	case <-j.done:
		return "作业 " + j.ID + " 已结束（" + j.Status() + "），无需终止。", nil
	default:
	}
	j.cancel()
	select {
	case <-j.done:
		return "作业 " + j.ID + " 已终止。", nil
	case <-time.After(5 * time.Second):
		return "终止信号已发出，作业 " + j.ID + " 尚未退出（稍后可用 job_output 确认）。", nil
	}
}
