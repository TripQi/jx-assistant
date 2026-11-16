package main

// 卡片刚出现时的即时提示（pendingalert.go）的验收：
//
//	用户口径：凡必须由他决定的卡，一出现就要有 Windows 提示音（复用久等提醒那套
//	叮声 + 任务栏闪烁），程序放后台也知道有卡在等他；但长期助理（经理）此刻可代批
//	的卡（managerResolveAllowed 为真、主对话在）不许打扰用户——那一轮自己会处置；
//	等该轮收口/用户接管、代批资格没了，才转成通知用户，且同一张卡只响一次。
//
// 修复前（基线 fc8cacb4）：卡片产生那一刻没有任何提示，只有「等满 4 分钟」才响，
// 本文件的 a/b 两个用例在基线上就是 0 次（红），见交付简报里的修前红原文。
//
// 覆盖 a–f：a 提问卡（含普通任务会话 / aux 两种发起方）、b 审批卡、c 经理轮内可
// 代批 → 0 次、d 轮次收口与用户接管后转为通知（只响一次）、e 重复事件/幂等重放
// 不重复响、f 设置开关关闭不响。

import (
	"context"
	"sync"
	"testing"
	"time"
)

// cardAlertSpy 换掉两个副作用入口（叮声 / 任务栏闪烁）并计数：跑测试不真响、
// 不动用户真实窗口。提示音用的是久等提醒同一个 askAlertSound（即时提示复用它）。
type cardAlertSpy struct {
	mu      sync.Mutex
	beeps   int
	flashes int
}

func newCardAlertSpy(t *testing.T) *cardAlertSpy {
	t.Helper()
	oldBeep, oldFlash := askAlertSound, pendingCardFlashHook
	s := &cardAlertSpy{}
	askAlertSound = func(*Config) { s.mu.Lock(); s.beeps++; s.mu.Unlock() }
	pendingCardFlashHook = func() { s.mu.Lock(); s.flashes++; s.mu.Unlock() }
	t.Cleanup(func() { askAlertSound, pendingCardFlashHook = oldBeep, oldFlash })
	return s
}

func (s *cardAlertSpy) counts() (beeps, flashes int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beeps, s.flashes
}

// settle 给调度留点余量（卡片登记与提示之间只隔一次函数调用，200ms 足够），
// 再把确切次数当断言——不用「等到出现为止」把多次误报盖过去。
func settle() { time.Sleep(200 * time.Millisecond) }

// openQuestion 在 sess 里挂一张提问卡，返回它的 id（等待中的 goroutine 由调用方
// 用 resolveQuestion 收尾）。
func openQuestion(t *testing.T, a *App, sess *SessionMeta, kind string) string {
	t.Helper()
	q := &UserQuestion{Kind: kind, Question: "选哪个？", Options: []string{"甲", "乙"}, AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), sess, q) }()
	waitRegistered(t, a, q)
	return q.ID
}

// openApproval 在 sess 里挂一张越权审批卡，返回它的 id。
func openApproval(t *testing.T, a *App, sess *SessionMeta) string {
	t.Helper()
	go func() {
		_, _, _ = a.requestApproval(context.Background(), sess, "run_command", "npm install", "装依赖")
	}()
	return waitApprovalRegistered(t, a)
}

// 场景 a：用户决定型的提问卡（普通任务会话 / aux）——一出现就应响 1 次。
func TestPendingCardAlertQuestionBeepsOnAppear(t *testing.T) {
	for _, mode := range []string{"task", "aux"} {
		t.Run(mode, func(t *testing.T) {
			st := testPlanStore(t)
			a := newPlanTestApp(t, st)
			a.Cfg = &Config{} // 提醒默认开（AskAlert 三个键都没写过）
			sess := &SessionMeta{ID: "s-" + mode, Title: "会话-" + mode, Mode: mode}
			if err := st.Create(sess); err != nil {
				t.Fatalf("建会话失败: %v", err)
			}
			spy := newCardAlertSpy(t)

			id := openQuestion(t, a, sess, askKindAsk)
			t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") }) // 放掉等待中的 goroutine

			settle()
			beeps, flashes := spy.counts()
			t.Logf("场景 a（%s 会话的提问卡）：叮声 %d 次，任务栏闪烁 %d 次", mode, beeps, flashes)
			if beeps != 1 || flashes != 1 {
				t.Fatalf("提问卡刚出现就该提示 1 次（叮声+闪烁各 1），实际 叮声=%d 闪烁=%d", beeps, flashes)
			}
		})
	}
}

// 场景 b：审批卡同样——一出现就应响 1 次。
func TestPendingCardAlertApprovalBeepsOnAppear(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{}
	sess := createPlanSession(t, st, "s1", "会话一", false)
	spy := newCardAlertSpy(t)

	id := openApproval(t, a, sess)
	t.Cleanup(func() { _ = a.resolveApproval(id, false, false) }) // 放掉等待中的 goroutine

	settle()
	beeps, flashes := spy.counts()
	t.Logf("场景 b（越权审批卡）：叮声 %d 次，任务栏闪烁 %d 次", beeps, flashes)
	if beeps != 1 || flashes != 1 {
		t.Fatalf("审批卡刚出现就该提示 1 次（叮声+闪烁各 1），实际 叮声=%d 闪烁=%d", beeps, flashes)
	}
}

// 场景 c：经理投递开启的那一轮里的卡（managerResolveAllowed 为真）——不打扰用户，0 次。
func TestPendingCardAlertManagerCoveredStaysQuiet(t *testing.T) {
	for _, kind := range []string{"question", "approval"} {
		t.Run(kind, func(t *testing.T) {
			a, _, task, _ := newMgrNotifyTestApp(t)
			grantTestTurn(t, a, task.ID) // 经理派发开出的那一轮正在执行
			spy := newCardAlertSpy(t)

			var id string
			if kind == "approval" {
				id = openApproval(t, a, task)
				t.Cleanup(func() { _ = a.resolveApproval(id, false, false) })
			} else {
				id = openQuestion(t, a, task, askKindAsk)
				t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") })
			}
			// 证据：这张卡此刻确实「可由长期助理代批」（同一个判定函数）
			if !a.managerResolveAllowed(kind == "approval", id) {
				t.Fatal("夹具应当让这张卡落在经理可代批的窗口里")
			}

			settle()
			beeps, flashes := spy.counts()
			t.Logf("场景 c（经理轮内的 %s 卡，managerResolveAllowed=true）：叮声 %d 次，闪烁 %d 次", kind, beeps, flashes)
			if beeps != 0 || flashes != 0 {
				t.Fatalf("经理可代批的卡不该打扰用户，实际 叮声=%d 闪烁=%d", beeps, flashes)
			}
		})
	}
}

// 场景 d：经理那一轮随后收口 / 用户接管 → 那张卡转为通知用户，且只响一次。
func TestPendingCardAlertManagerTurnEndedThenNotifies(t *testing.T) {
	for _, how := range []string{"revoke", "close"} {
		for _, kind := range []string{"question", "approval"} {
			t.Run(how+"/"+kind, func(t *testing.T) {
				a, _, task, _ := newMgrNotifyTestApp(t)
				run := grantTestTurn(t, a, task.ID)
				spy := newCardAlertSpy(t)

				var id string
				isApproval := kind == "approval"
				if isApproval {
					id = openApproval(t, a, task)
					t.Cleanup(func() { _ = a.resolveApproval(id, false, false) })
				} else {
					id = openQuestion(t, a, task, askKindAsk)
					t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") })
				}
				settle()
				if beeps, _ := spy.counts(); beeps != 0 {
					t.Fatalf("先决条件：经理可代批时不该响，实际叮声 %d 次", beeps)
				}

				// 代批资格没了：用户亲发消息接管（revoke）或那一轮收口（drop）
				if how == "revoke" {
					a.revokeManagerTurn(task.ID)
				} else {
					a.dropManagerTurnOf(task.ID, run)
				}
				if a.managerResolveAllowed(isApproval, id) {
					t.Fatal("资格已撤，这张卡不该再判为可代批")
				}
				settle()
				beeps, flashes := spy.counts()
				t.Logf("场景 d（%s 后转通知，%s 卡）：叮声 %d 次，闪烁 %d 次", how, kind, beeps, flashes)
				if beeps != 1 || flashes != 1 {
					t.Fatalf("代批资格没了就该转成通知 1 次，实际 叮声=%d 闪烁=%d", beeps, flashes)
				}

				// 再撤一次 / 再收口一次：不能补第二声
				a.revokeManagerTurn(task.ID)
				a.dropManagerTurnOf(task.ID, run)
				settle()
				if beeps, _ := spy.counts(); beeps != 1 {
					t.Fatalf("同一张卡只该响一次，重复撤销后实际叮声 %d 次", beeps)
				}
			})
		}
	}
}

// 场景 e：重复事件 / 重连重放（同一个卡片 id 再来一遍）不得重复响。
func TestPendingCardAlertReplayStaysQuiet(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{}
	sess := createPlanSession(t, st, "s1", "会话一", false)
	spy := newCardAlertSpy(t)

	id := openQuestion(t, a, sess, askKindAsk)
	t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") })
	settle()
	if beeps, _ := spy.counts(); beeps != 1 {
		t.Fatalf("首次出现应响 1 次，实际 %d 次", beeps)
	}

	// 重连后重放：同一个 id 的卡片事件再来两遍
	a.alertPendingCardOnAppear(id)
	a.alertPendingCardOnAppear(id)
	settle()
	beeps, flashes := spy.counts()
	t.Logf("场景 e（同一 id 重放 2 次）：叮声 %d 次，闪烁 %d 次", beeps, flashes)
	if beeps != 1 || flashes != 1 {
		t.Fatalf("重放不该重复响：应仍是 叮声=1 闪烁=1，实际 叮声=%d 闪烁=%d", beeps, flashes)
	}
}

// 场景 f：设置里关掉「卡片出现时提示音」→ 不响（久等提醒的语义不受影响）。
func TestPendingCardAlertSettingOffStaysQuiet(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	off := false
	a.Cfg = &Config{AskAlert: AskAlertCfg{OnAppear: &off}}
	if a.Cfg.askAlertOnAppear() {
		t.Fatal("夹具前提：设置应已关闭")
	}
	sess := createPlanSession(t, st, "s1", "会话一", false)
	spy := newCardAlertSpy(t)

	for _, kind := range []string{"question", "approval"} {
		if kind == "approval" {
			id := openApproval(t, a, sess)
			t.Cleanup(func() { _ = a.resolveApproval(id, false, false) })
		} else {
			id := openQuestion(t, a, sess, askKindAsk)
			t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") })
		}
	}
	settle()
	beeps, flashes := spy.counts()
	t.Logf("场景 f（开关关闭）：叮声 %d 次，闪烁 %d 次", beeps, flashes)
	if beeps != 0 || flashes != 0 {
		t.Fatalf("开关关闭后不该即时响，实际 叮声=%d 闪烁=%d", beeps, flashes)
	}
	// 关闭只关「出现即提示」，久等提醒的开关语义不变
	if !a.Cfg.askAlertOn() {
		t.Fatal("关掉「出现即提示」不应连带关掉久等提醒")
	}
}
