package main

// 越权审批卡的久等声光提醒与等待上限（perm.go）：
//   · approvalTTL 从 2 分钟改成 5 分钟（与提问卡默认时限一致）；
//   · 审批卡也挂 ask_alert 声光提醒，延时统一取「设置·常规·询问提醒」的
//     askAlertAfterSec（默认 4 分钟）——与提问/计划审批共用同一套机制、同一个
//     设置项，只是多一个应用位置（浮窗文案带审批内容摘要）。
// 时序：提醒 4 分钟 < 超时 5 分钟 —— 先提醒、后超时按拒。
// 测试里把 approvalTTL / approvalAlertDelay 换成毫秒级，验证这条时序而不是真等 4 分钟。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitApprovalRegistered 等审批请求登记进 a.approvals 并返回它的 id。
func waitApprovalRegistered(t *testing.T, a *App) string {
	t.Helper()
	for i := 0; i < 500; i++ {
		a.permMu.Lock()
		id := ""
		for k := range a.approvals {
			id = k
		}
		a.permMu.Unlock()
		if id != "" {
			return id
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatal("审批请求没有登记上")
	return ""
}

// approvalAlertSpy 换掉两个副作用入口（唤起窗口 / 叮声）并计数，测试完恢复。
type approvalAlertSpy struct {
	mu    sync.Mutex
	front []string // 每次唤起窗口带的文案
	beeps int
}

func newApprovalAlertSpy(t *testing.T) *approvalAlertSpy {
	t.Helper()
	spy := &approvalAlertSpy{}
	oldFront, oldBeep := alertFrontHook, askAlertSound
	alertFrontHook = func(title, text string) {
		spy.mu.Lock()
		spy.front = append(spy.front, text)
		spy.mu.Unlock()
	}
	askAlertSound = func(*Config) {
		spy.mu.Lock()
		spy.beeps++
		spy.mu.Unlock()
	}
	t.Cleanup(func() { alertFrontHook, askAlertSound = oldFront, oldBeep })
	return spy
}

func (s *approvalAlertSpy) counts() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.front), s.beeps
}

func (s *approvalAlertSpy) frontTexts() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string{}, s.front...)
}

// 审批挂出 → 到点先声光提醒（带审批内容摘要、广播 ask_alert 带 approvalId）
// → 等待上限到点按拒绝收卡、之后不再补提醒。
func TestApprovalAlertThenTimeoutDeny(t *testing.T) {
	oldTTL, oldDelay := approvalTTL, approvalAlertDelay
	approvalTTL = 1500 * time.Millisecond
	approvalAlertDelay = func(*App) time.Duration { return 300 * time.Millisecond }
	defer func() { approvalTTL, approvalAlertDelay = oldTTL, oldDelay }()

	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{} // 提醒默认开（Enabled=nil），延时由上面的桩决定
	sess := createPlanSession(t, st, "s1", "会话一", false)
	spy := newApprovalAlertSpy(t)

	ch, unsub := a.subscribe()
	defer unsub()

	type res struct{ approved, always, timedOut bool }
	done := make(chan res, 1)
	go func() {
		ap, al, to := a.requestApproval(context.Background(), sess, "shell",
			"rm -rf 构建临时目录", "清理构建产物")
		done <- res{ap, al, to}
	}()
	id := waitApprovalRegistered(t, a)

	// ① 提醒先到（提醒延时 300ms < 等待上限 1.5s）
	deadline := time.After(2 * time.Second)
	for {
		if n, _ := spy.counts(); n > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("审批挂出后到点没有触发声光提醒（唤起窗口）")
		case <-time.After(20 * time.Millisecond):
		}
	}
	// 卡片刚出现时还响过 1 声（pendingalert.go 的即时提示，与久等提醒共用
	// 同一个叮声通道）：到点这一次是第 2 声——次数必须确切，不放松。
	if _, beeps := spy.counts(); beeps != 2 {
		t.Fatalf("卡片出现 1 声 + 到点 1 声 = 2 声，实际 %d 次", beeps)
	}
	if texts := spy.frontTexts(); !strings.Contains(texts[0], "会话一") ||
		!strings.Contains(texts[0], "rm -rf 构建临时目录") {
		t.Fatalf("提醒文案应带会话标题 + 审批内容摘要，实际 %q", texts[0])
	}

	// 广播：ask_alert 带 approvalId（前端据此措辞「审批」并在决定/超时时收卡）
	alertDeadline := time.After(2 * time.Second)
	for sawAlert := false; !sawAlert; {
		select {
		case ev := <-ch:
			if ev.Type != "ask_alert" {
				continue
			}
			if ev.ApprovalID != id {
				t.Fatalf("ask_alert 应带 approvalId=%s，实际 %+v", id, ev)
			}
			if ev.QuestionID != "" {
				t.Fatalf("审批的 ask_alert 不该带 questionId，实际 %q", ev.QuestionID)
			}
			if !strings.Contains(ev.Summary, "rm -rf 构建临时目录") || ev.Output == "" {
				t.Fatalf("ask_alert 应带审批摘要与朗读文案，实际 %+v", ev)
			}
			sawAlert = true
		case <-alertDeadline:
			t.Fatal("到点没有广播 ask_alert（前端就不会变色/倒计时/朗读）")
		}
	}

	// ② 等待上限到点：按拒绝返回（timedOut 与明确拒绝分开）
	select {
	case r := <-done:
		if r.approved || r.always {
			t.Fatalf("超时应按拒绝处理，实际 %+v", r)
		}
		if !r.timedOut {
			t.Fatal("超时应带 timedOut 标记（模型据此如实转述是错过还是拒绝）")
		}
	case <-time.After(4 * time.Second):
		t.Fatal("审批没有在等待上限到点时结束（approvalTTL 未生效？）")
	}
	a.permMu.Lock()
	_, still := a.approvals[id]
	a.permMu.Unlock()
	if still {
		t.Fatal("超时后审批卡应从待决清单里消失")
	}
	time.Sleep(400 * time.Millisecond) // 再等一会儿：确认不会补第二次提醒
	// front=1 证明只到点提醒了 1 次；beep=2 = 卡片出现那 1 声 + 到点那 1 声，
	// 超时后不再补第 3 声。
	if n, beeps := spy.counts(); n != 1 || beeps != 2 {
		t.Fatalf("超时后不该再补提醒：front=%d beep=%d（应 1 次窗口提醒 + 2 声叮）", n, beeps)
	}
}

// 用户在提醒到点之前就点掉（允许/拒绝）：不该再响；到点后的提醒一并作废。
func TestApprovalResolvedBeforeAlertStaysQuiet(t *testing.T) {
	oldTTL, oldDelay := approvalTTL, approvalAlertDelay
	approvalTTL = 3 * time.Second
	approvalAlertDelay = func(*App) time.Duration { return 800 * time.Millisecond }
	defer func() { approvalTTL, approvalAlertDelay = oldTTL, oldDelay }()

	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{}
	sess := createPlanSession(t, st, "s1", "会话一", false)
	spy := newApprovalAlertSpy(t)

	done := make(chan bool, 1)
	go func() {
		ap, _, _ := a.requestApproval(context.Background(), sess, "run_command", "npm install", "装依赖")
		done <- ap
	}()
	id := waitApprovalRegistered(t, a)
	if !a.approvalAlertDue(id) {
		t.Fatal("刚挂出、没人点过的审批：到点该提醒")
	}
	if err := a.resolveApproval(id, true, false); err != nil {
		t.Fatalf("允许失败: %v", err)
	}
	if a.approvalAlertDue(id) {
		t.Fatal("已决定的审批不该再提醒")
	}
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("用户点允许后应按批准返回")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("用户决定后审批没有结束等待")
	}
	time.Sleep(1000 * time.Millisecond) // 越过原定 800ms 提醒点
	// 卡片出现那一刻的即时提示（pendingalert.go）仍该响 1 声；到点那次不许再有：
	// front 保持 0（没盖过屏）、beep 恰好 1 声。
	if n, beeps := spy.counts(); n != 0 || beeps != 1 {
		t.Fatalf("用户已点掉，到点不该再提醒（只剩卡片出现那 1 声）：front=%d beep=%d", n, beeps)
	}
}
