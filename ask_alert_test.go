package main

// 久等声光提醒（ask_alert）的两条判定（interact.go）：
//   · askAlertDue：Timer 到点那一刻，只有「还在等 + 用户没作答 + 没停表/没进去
//     处理」才响；
//   · holdQuestion 也必须受理**没有计时器**的请求（计划审批 kind=plan 无时限）——
//     用户从任何入口打开计划页/浮窗就是「已在处理」，此时没有表可停，但仍要置位
//     TimerHeld 把那次提醒作废（前端 notePlanHandled 正是这么调的）。
// 另外确认：对计划审批置位不会动等待本身——它照样等在那儿，之后批准/修改照常。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// waitRegistered 等这条请求真的登记进 a.questions（requestUserAnswer 的登记
// 在其 goroutine 里完成，q.ID 更早就赋了值，不能拿它当登记完成的信号）。
func waitRegistered(t *testing.T, a *App, q *UserQuestion) {
	t.Helper()
	for i := 0; i < 500; i++ {
		a.permMu.Lock()
		_, ok := a.questions[q.ID]
		a.permMu.Unlock()
		if ok {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("等待提问登记超时（id=%q）", q.ID)
}

// holdQuestion 对无时限的计划审批同样生效：置位 + 广播停表，且不打断等待
func TestHoldQuestionCoversTimelessPlan(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	sess := createPlanSession(t, st, "s1", "会话一", true)

	ch, unsub := a.subscribe()
	defer unsub()
	var mu sync.Mutex
	var events []Event
	go func() {
		for ev := range ch {
			mu.Lock()
			events = append(events, ev)
			mu.Unlock()
		}
	}()

	q := &UserQuestion{Kind: askKindPlan, Question: "请审批以下执行计划", Plan: "# 计划", AllowFree: true}
	done := make(chan string, 1)
	go func() { s, ok := a.requestUserAnswer(context.Background(), sess, q); if ok { done <- s } }()
	waitRegistered(t, a, q)
	if q.timer != nil {
		t.Fatal("计划审批本就不该有计时器（无时限）")
	}
	if !a.askAlertDue(q.ID) {
		t.Fatal("刚挂出来的计划审批、没人碰过：久等提醒到点应当要响")
	}

	// 用户从右下角/上方浮窗/计划页签打开这份计划：登记「已在处理」
	a.holdQuestion(q.ID)
	if a.askAlertDue(q.ID) {
		t.Fatal("已登记「已在处理」之后，久等提醒到点不该再响")
	}
	a.permMu.Lock()
	held := q.TimerHeld
	_, waiting := a.questions[q.ID]
	a.permMu.Unlock()
	if !held {
		t.Fatal("无时限的计划审批也应置位 TimerHeld（没有表可停，但标记要有）")
	}
	if !waiting {
		t.Fatal("这只是停提醒，等待本身不该结束——计划仍该等用户批准")
	}
	time.Sleep(50 * time.Millisecond) // 放行广播
	mu.Lock()
	sawHeld := false
	for _, ev := range events {
		if ev.Type == "question_deadline" && ev.QuestionID == q.ID && ev.Held {
			sawHeld = true
		}
	}
	mu.Unlock()
	if !sawHeld {
		t.Fatal("登记后应广播 question_deadline（held=true），别的端好同步显示")
	}

	// 停提醒不影响后续处置：批准照常唤醒等待中的工具
	if err := a.resolveQuestion(q.ID, "__approve__"); err != nil {
		t.Fatalf("批准失败: %v", err)
	}
	select {
	case ans := <-done:
		if ans != "__approve__" {
			t.Fatalf("等待中的计划工具应拿到批准答案，实际 %q", ans)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("登记之后计划仍该能批准（提醒去重不该影响等待）")
	}
}

// askAlertDue：还在等且没人碰过才响；作答（从清单里消失）与未知 id 都不响
func TestAskAlertDueGate(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	sess := createPlanSession(t, st, "s1", "会话一", false)

	q := &UserQuestion{Kind: askKindAsk, Question: "选哪个？", Options: []string{"甲", "乙"}, AllowFree: true}
	go func() { _, _ = a.requestUserAnswer(context.Background(), sess, q) }()
	waitRegistered(t, a, q)
	if !a.askAlertDue(q.ID) {
		t.Fatal("挂出来且没人碰过的提问：到点该响")
	}
	if a.askAlertDue("q-不存在") {
		t.Fatal("清单里没有的 id 不该响（提问已结束/已作答）")
	}

	a.holdQuestion(q.ID) // 悬停/触碰卡片：停表
	if a.askAlertDue(q.ID) {
		t.Fatal("停表之后到点不该响")
	}
	if err := a.resolveQuestion(q.ID, "甲"); err != nil {
		t.Fatalf("回答失败: %v", err)
	}
	if a.askAlertDue(q.ID) {
		t.Fatal("已作答（从等待清单里消失）之后不该响")
	}
}

// 到点提醒要「把提醒顶到用户眼前」：fireAskAlert 必须唤窗口 + 叮声 + 广播
// ask_alert（前端据此变色/倒计时/朗读）。两个副作用入口换成桩：断言调用与文案，
// 不动用户真实窗口、也不真响。
func TestAskAlertWakesWindowAndBeeps(t *testing.T) {
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)

	type frontCall struct{ title, text string }
	front := make(chan frontCall, 4)
	oldFront, oldBeep, oldScreen := alertFrontHook, askAlertSound, alertScreenHook
	alertFrontHook = func(title, text string) { front <- frontCall{title, text} }
	beeps := 0
	askAlertSound = func(*Config) { beeps++ }
	// 全屏蒙层也换桩：跑测试不真弹原生窗口，只断言「到点确实去盖屏了、qid 正确」
	type screenCall struct {
		title, text, qid string
	}
	screen := make(chan screenCall, 4)
	alertScreenHook = func(title, text, qid string, onDismiss func()) {
		screen <- screenCall{title, text, qid}
	}
	defer func() { alertFrontHook, askAlertSound, alertScreenHook = oldFront, oldBeep, oldScreen }()

	ch, unsub := a.subscribe()
	defer unsub()

	q := &UserQuestion{ID: "q-wake", SessionID: "s1", SessionTitle: "会话一",
		Kind: askKindPlan, Question: "请审批以下执行计划"}
	a.fireAskAlert(q.ID, "", q.SessionID, q.SessionTitle, q.Question, "领导，在吗？有个问题需要您审批确认一下。")

	select {
	case got := <-front:
		if !strings.Contains(got.title, "坚祥数字助理") {
			t.Fatalf("唤起窗口的标题不对: %q", got.title)
		}
		if !strings.Contains(got.text, "会话一") || !strings.Contains(got.text, "请审批") {
			t.Fatalf("唤起窗口/通知的文案应含会话标题与问题摘要，实际 %q", got.text)
		}
	case <-time.After(time.Second):
		t.Fatal("提醒到点没有去唤起窗口（用户最小化/在后台时就是「看不见」的那个 bug）")
	}
	if beeps != 1 {
		t.Fatalf("到点应叮一声，实际 %d 次", beeps)
	}

	// 桌面全屏蒙层（第二处提醒）也在同一触发点盖屏：qid 必须是这轮的问题 id
	//（前端收层时按它 POST 关闭，qid 错了关不掉）
	select {
	case got := <-screen:
		if !strings.Contains(got.title, "坚祥数字助理") {
			t.Fatalf("全屏蒙层标题不对: %q", got.title)
		}
		if got.qid != "q-wake" {
			t.Fatalf("全屏蒙层 qid 应为这轮问题 id，实际 %q", got.qid)
		}
	case <-time.After(time.Second):
		t.Fatal("提醒到点没有盖全屏蒙层（主窗口被埋住时用户就看不见了）")
	}

	deadline := time.After(time.Second)
	for {
		select {
		case ev := <-ch:
			if ev.Type == "ask_alert" {
				if ev.QuestionID != "q-wake" || ev.SessionID != "s1" {
					t.Fatalf("ask_alert 事件字段不对: %+v", ev)
				}
				if ev.Output == "" {
					t.Fatal("ask_alert 该带朗读文案")
				}
				return
			}
		case <-deadline:
			t.Fatal("到点没有广播 ask_alert（前端就不会变色/倒计时/朗读）")
		}
	}
}
