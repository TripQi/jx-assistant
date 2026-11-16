package main

// 「AI 会话不能给提问卡停表」的验收（server.go handleResolveQuestion 的
// stop_timer 分支）。
//
// 缺陷事实：stop_timer 分支在调用者身份判定之前就 return——任何 AI 会话（子对话、
// 普通任务会话、aux、认不出身份的）都能对任意会话的提问卡停表，倒计时随之冻结、
// 永不超时：卡虽然不落决定，却能一直挂在界面上。
//
// 口径：带 X-JX-Caller-Session 头的 stop_timer 一律 403（经理自己、子对话、任务
// 会话、aux 都一样，不设例外——停表是「用户正在看着这条请求」的在场信号，AI 会话
// 不能替用户表态）；无头（界面按钮、远程转发）照常成功，暂停标记、截止时刻清零与
// 广播事件与改动前完全一致。
//
// 本文件的用例全走真实 HTTP handler（与界面按钮 / call_api 直调同一条路径），
// 提问用产品同一条 requestUserAnswer 挂号（真有计时器、真有截止时刻）。
// 修复前跑本文件即红（带头的停表 200 且计时器被掐），修复后全绿。

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// postStopTimerAs 打真实停表入口：callerHeader 空串＝真用户（界面按钮 / 远程
// 转发都不带这个头），非空＝该 AI 会话经 call_api 直调（runCallAPI 注入
// X-JX-Caller-Session=它自己的会话 id）。
func postStopTimerAs(t *testing.T, a *App, callerHeader, id string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/api/questions/"+id, strings.NewReader(`{"stop_timer":true}`))
	r.SetPathValue("id", id)
	if callerHeader != "" {
		r.Header.Set("X-JX-Caller-Session", callerHeader)
	}
	w := httptest.NewRecorder()
	a.handleResolveQuestion(w, r)
	who := "真用户（无调用者头）"
	if callerHeader != "" {
		who = "AI 调用者 " + callerHeader
	}
	t.Logf("POST /api/questions/%s body={\"stop_timer\":true} 调用者=%s -> HTTP %d %s",
		id, who, w.Code, strings.TrimSpace(w.Body.String()))
	return w
}

// liveAsk 挂一张真正的限时提问（与产品路径 requestUserAnswer 同一条：有计时器、
// 有截止时刻、进等待清单）；收尾二选一：answer 拿到答案 / timedOut 到点超时。
func liveAsk(t *testing.T, a *App, sess *SessionMeta, timeoutSec int) (q *UserQuestion, answer chan string, timedOut chan bool) {
	t.Helper()
	q = &UserQuestion{Kind: askKindAsk, Question: "选哪个？", Options: []string{"甲", "乙"}, AllowFree: true, TimeoutSec: timeoutSec}
	answer, timedOut = make(chan string, 1), make(chan bool, 1)
	go func() {
		s, ok := a.requestUserAnswer(context.Background(), sess, q)
		if ok {
			answer <- s
			return
		}
		timedOut <- true
	}()
	waitRegistered(t, a, q) // 既有 helper（ask_alert_test.go）：等它真的进等待清单
	return q, answer, timedOut
}

// timerState 取卡片当前的暂停标记、截止时刻、是否仍在等、计时器是否还挂着
//（permMu 保护）。被拒的停表一个字都不许改这些值。
func timerState(a *App, id string) (held bool, deadline int64, waiting, hasTimer bool) {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	q, ok := a.questions[id]
	if !ok {
		return false, 0, false, false
	}
	return q.TimerHeld, q.DeadlineMs, true, q.timer != nil
}

// eventSink 收集全局事件流：停表广播 question_deadline 就在这条线上（前端据此
// 把各端的倒计时统一换成「已停止计时」）。
type eventSink struct {
	mu     sync.Mutex
	events []Event
	unsub  func()
}

func watchEvents(a *App) *eventSink {
	ch, unsub := a.subscribe()
	s := &eventSink{unsub: unsub}
	go func() {
		for ev := range ch {
			s.mu.Lock()
			s.events = append(s.events, ev)
			s.mu.Unlock()
		}
	}()
	return s
}

func (s *eventSink) close() { s.unsub() }

// countHeld 数这条提问的停表广播（held=true）条数。
func (s *eventSink) countHeld(qid string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, ev := range s.events {
		if ev.Type == "question_deadline" && ev.QuestionID == qid && ev.Held {
			n++
		}
	}
	return n
}

// stopTimerCallers：AI 会话侧的各种身份——子对话（parent 链落到主对话）、普通
// 任务会话、经理自己、aux、认不出的、以及空取值的调用者头（有头即 AI，不看取值）。
func stopTimerCallers(t *testing.T, a *App, mgr, task *SessionMeta) []struct{ name, header string } {
	t.Helper()
	sub := addChildSession(t, a, "sub-1", "子对话", "coding", mgr.ID)
	addChildSession(t, a, "aux-1", "辅助答疑", "aux", "")
	return []struct{ name, header string }{
		{"子对话（经理派发，parent 链落到主对话）", sub.ID},
		{"普通任务会话（经理 dispatch_task 派生）", task.ID},
		{"长期助理主对话自己（不设例外）", mgr.ID},
		{"aux 辅助答疑会话", "aux-1"},
		{"根本不存在的会话 id", "no-such-session"},
		{"空取值的调用者头（有头即 AI）", " "},
	}
}

// 带调用者头的 stop_timer 一律 403，且卡片状态一点不动：不置暂停标记、不改截止
// 时刻、不广播 question_deadline、不落任何决定（提问仍在等用户作答）。
// 修复前这里全是 200 且计时器被掐（红证见报告）。
func TestStopTimerAiCallerDenied(t *testing.T) {
	a, mgr, task, solo := newMgrNotifyTestApp(t)
	callers := stopTimerCallers(t, a, mgr, task)
	sink := watchEvents(a)
	defer sink.close()

	for _, c := range callers {
		c := c
		t.Run(c.name, func(t *testing.T) {
			q, answer, _ := liveAsk(t, a, solo, 60) // 60 秒：本子用例跑不完就到点，不会被超时打断
			held0, deadline0, waiting0, hasTimer0 := timerState(a, q.ID)
			if held0 || deadline0 == 0 || !waiting0 || !hasTimer0 {
				t.Fatalf("挂号状态不对（应是有时限、未停表）：held=%v deadline=%d waiting=%v timer=%v",
					held0, deadline0, waiting0, hasTimer0)
			}

			w := postStopTimerAs(t, a, c.header, q.ID)
			if w.Code != http.StatusForbidden {
				t.Fatalf("%s 停表必须 403，实际 %d %s", c.name, w.Code, w.Body.String())
			}
			if strings.TrimSpace(w.Body.String()) == "" {
				t.Error("403 正文应给出可读原因")
			}

			held, deadline, waiting, hasTimer := timerState(a, q.ID)
			if held {
				t.Fatalf("%s 被拒后不许置暂停标记（卡片会被无限冻结）", c.name)
			}
			if deadline != deadline0 {
				t.Fatalf("%s 被拒后不许动截止时刻：%d -> %d", c.name, deadline0, deadline)
			}
			if !waiting || !hasTimer {
				t.Fatalf("%s 被拒后提问应原样等用户（且计时器还在）：waiting=%v timer=%v", c.name, waiting, hasTimer)
			}
			if n := sink.countHeld(q.ID); n != 0 {
				t.Fatalf("%s 被拒后不许广播 question_deadline，实际 %d 条", c.name, n)
			}
			if q.decided || len(q.ch) != 0 {
				t.Fatalf("%s 被拒后不许落任何决定：decided=%v ch=%d", c.name, q.decided, len(q.ch))
			}

			// 收尾：用户作答，放行等待中的工具（也证明这张卡还能正常用）
			if err := a.resolveQuestion(q.ID, "甲"); err != nil {
				t.Fatalf("收尾作答失败: %v", err)
			}
			select {
			case s := <-answer:
				if s != "甲" {
					t.Fatalf("收尾作答应原样送达，实际 %q", s)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("收尾作答没唤醒等待中的工具")
			}
		})
	}
}

// 被拒的停表不能把倒计时冻住：原时限照旧到点超时（ok=false，视为放行）——
// 这是缺陷后果（卡无限挂着）的直接反向断言。
func TestStopTimerDeniedKeepsCountdown(t *testing.T) {
	a, mgr, _, solo := newMgrNotifyTestApp(t)
	sub := addChildSession(t, a, "sub-1", "子对话", "coding", mgr.ID)

	q, _, timedOut := liveAsk(t, a, solo, 1)
	if w := postStopTimerAs(t, a, sub.ID, q.ID); w.Code != http.StatusForbidden {
		t.Fatalf("子对话停表必须 403，实际 %d %s", w.Code, w.Body.String())
	}
	select {
	case <-timedOut:
		t.Log("被拒之后 1 秒时限照旧到点超时（ok=false）：倒计时没有被冻结")
	case <-time.After(3 * time.Second):
		t.Fatal("停表被拒后提问仍在无限等待：倒计时被冻住了")
	}
	if held, _, waiting, _ := timerState(a, q.ID); held || waiting {
		t.Fatalf("超时收尾后应退出等待且从未置停表标记，实际 held=%v waiting=%v", held, waiting)
	}
}

// 真用户（无调用者头）停表照常成功——最关键的回归：暂停标记、截止时刻清零、
// 停表广播、越过原时限继续等、随后作答唤醒，全部与改动前一致。
func TestStopTimerUserPathUnchanged(t *testing.T) {
	a, _, _, solo := newMgrNotifyTestApp(t)
	sink := watchEvents(a)
	defer sink.close()

	q, answer, timedOut := liveAsk(t, a, solo, 1) // 1 秒时限：停表后必须越过它还在等
	held0, deadline0, _, hasTimer0 := timerState(a, q.ID)
	if held0 || deadline0 == 0 || !hasTimer0 {
		t.Fatalf("限时提问挂号状态不对（改动前的基准）：held=%v deadline=%d timer=%v", held0, deadline0, hasTimer0)
	}

	// 界面上悬停/触碰卡片或点停止按钮、远程转发——都不带调用者头
	w := postStopTimerAs(t, a, "", q.ID)
	if w.Code != http.StatusOK {
		t.Fatalf("用户停表应照常成功，实际 %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"ok":"1"`) {
		t.Fatalf(`用户停表应回 {"ok":"1"}，实际 %s`, strings.TrimSpace(w.Body.String()))
	}

	held, deadline, waiting, hasTimer := timerState(a, q.ID)
	if !held {
		t.Fatal("用户停表必须置位 TimerHeld（与改动前一致）")
	}
	if deadline != 0 {
		t.Fatalf("用户停表必须把截止时刻清零（转不限时），实际 %d", deadline)
	}
	if !waiting || !hasTimer {
		t.Fatalf("停表只是停计时，等待本身照旧：waiting=%v timer=%v", waiting, hasTimer)
	}
	if n := sink.countHeld(q.ID); n != 1 {
		t.Fatalf("用户停表应广播且只广播一条 question_deadline(held=true)，实际 %d 条", n)
	}

	time.Sleep(1300 * time.Millisecond) // 越过原 1 秒时限
	if held, _, waiting, _ := timerState(a, q.ID); !held || !waiting {
		t.Fatalf("停表后越过原时限仍应无限等待：held=%v waiting=%v", held, waiting)
	}
	select {
	case <-timedOut:
		t.Fatal("停表后不该再按原时限超时")
	default:
	}

	// 停表之后的作答照常唤醒等待中的工具
	if err := a.resolveQuestion(q.ID, "乙"); err != nil {
		t.Fatalf("停表后作答失败: %v", err)
	}
	select {
	case s := <-answer:
		if s != "乙" {
			t.Fatalf("停表后作答应原样送达，实际 %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("停表后作答没唤醒等待中的工具")
	}
}
