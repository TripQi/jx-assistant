package main

import (
	"os"
	"strings"
	"testing"
)

func newThinkingContinueUIRun() *sessionRun {
	return &sessionRun{subs: map[int]chan Event{}}
}

func assertNoThinkingContinueReplay(t *testing.T, r *sessionRun) {
	t.Helper()
	ch, unsub := r.subscribe()
	defer unsub()
	select {
	case ev := <-ch:
		t.Fatalf("不应补发已撤下的提示: %#v", ev)
	default:
	}
}

func TestThinkingContinueRuntimeReplayPreservesThinking(t *testing.T) {
	r := newThinkingContinueUIRun()
	r.observe(Event{Type: "reasoning", Delta: "已流出的思考"})
	r.observe(Event{Type: "delta", Delta: "半句正文"})
	for _, output := range []string{"纯思考续接 1/2", "纯思考续接 2/2"} {
		r.observe(Event{Type: "thinking_continue", SessionID: "s", Output: output})
	}
	for i := 0; i < 2; i++ { // 刷新与再次接回都只补当前提示，不重放旧计数。
		ch, unsub := r.subscribe()
		select {
		case ev := <-ch:
			if ev.Type != "thinking_continue" || ev.SessionID != "s" || ev.Output != "纯思考续接 2/2" {
				t.Fatalf("续接补发不符: %#v", ev)
			}
		default:
			t.Fatal("订阅时未补发续接提示")
		}
		select {
		case ev := <-ch:
			t.Fatalf("不应重放旧计数: %#v", ev)
		default:
		}
		unsub()
	}
	if text, thinking := r.snapshot(); text != "半句正文" || thinking != "已流出的思考" {
		t.Fatalf("续接不能按 retry 丢弃思考/正文: %q / %q", text, thinking)
	}
	r.observe(Event{Type: "thinking_continue", SessionID: "s"})
	assertNoThinkingContinueReplay(t, r)
	if text, thinking := r.snapshot(); text != "半句正文" || thinking != "已流出的思考" {
		t.Fatalf("空续接事件只应清提示: %q / %q", text, thinking)
	}
}

func TestThinkingContinueRuntimeLifecycle(t *testing.T) {
	for _, typ := range []string{"delta", "reasoning", "tool_start", "message", "stopped", "error", "done", "retry"} {
		t.Run(typ, func(t *testing.T) {
			r := newThinkingContinueUIRun()
			r.observe(Event{Type: "thinking_continue", SessionID: "s", Output: "纯思考续接 1/2"})
			r.observe(Event{Type: typ, Delta: "后续输出"})
			assertNoThinkingContinueReplay(t, r)
		})
	}
	for _, ev := range []Event{{Type: "usage"}, {Type: "tool_wait"}, {Type: "auto_retry"}, {Type: "message", Kind: "env"}} {
		t.Run("unrelated_"+ev.Type+ev.Kind, func(t *testing.T) {
			r := newThinkingContinueUIRun()
			r.observe(Event{Type: "thinking_continue", SessionID: "s", Output: "纯思考续接 1/2"})
			r.observe(ev)
			ch, unsub := r.subscribe()
			defer unsub()
			select {
			case got := <-ch:
				if got.Type != "thinking_continue" || got.Output == "" {
					t.Fatalf("无关事件不应替换续接提示: %#v", got)
				}
			default:
				t.Fatal("无关事件误清了续接提示")
			}
		})
	}
}

func TestThinkingContinueRuntimeInterjectClearsImmediately(t *testing.T) {
	r := newThinkingContinueUIRun()
	r.observe(Event{Type: "reasoning", Delta: "保留的思考"})
	r.observe(Event{Type: "thinking_continue", SessionID: "s", Output: "纯思考续接 1/2"})
	ch, unsub := r.subscribe()
	defer unsub()
	select {
	case <-ch: // 先取订阅补发；无帧时立即失败，不死等通道。
	default:
		t.Fatal("缺少订阅补发事件")
	}
	cancelled := false
	r.modelAbort = &modelAbortEntry{r: r, cancel: func() { cancelled = true }}
	a := &App{runners: map[string]*sessionRun{"s": r}}
	a.kickModelStream("s")
	if !cancelled {
		t.Fatal("插话仍应打断当前模型流")
	}
	select {
	case ev := <-ch:
		if ev.Type != "thinking_continue" || ev.SessionID != "s" || ev.Output != "" {
			t.Fatalf("插话须立即发布空续接事件: %#v", ev)
		}
	default:
		t.Fatal("插话未立即撤下续接提示")
	}
	assertNoThinkingContinueReplay(t, r)
	if _, thinking := r.snapshot(); thinking != "保留的思考" {
		t.Fatalf("插话清提示不能丢思考: %q", thinking)
	}
	// observe 与 publish 之间恰好发生插话，旧的非空帧不得再把提示点亮。
	r.publish(Event{Type: "thinking_continue", SessionID: "s", Output: "纯思考续接 1/2"})
	select {
	case ev := <-ch:
		if ev.Output != "" {
			t.Fatalf("已被插话撤销的旧提示不得迟到重亮: %#v", ev)
		}
	default:
		t.Fatal("缺少迟到帧清理事件")
	}
	// 不在模型调用窗口时也应清提示，而不是依赖 cancel 回调。
	r.modelAbort = nil
	r.observe(Event{Type: "thinking_continue", SessionID: "s", Output: "纯思考续接 2/2"})
	a.kickModelStream("s")
	assertNoThinkingContinueReplay(t, r)
}

func TestThinkingContinueRuntimeStopRejectsLateHint(t *testing.T) {
	for _, killed := range []bool{false, true} {
		r := newThinkingContinueUIRun()
		r.stopped, r.killed = !killed, killed
		ch, unsub := r.subscribe()
		ev := Event{Type: "thinking_continue", SessionID: "s", Output: "纯思考续接 1/2"}
		r.observe(ev)
		r.publish(ev)
		select {
		case got := <-ch:
			if got.Type != "thinking_continue" || got.Output != "" {
				t.Fatalf("停止后迟到的提示不能重新亮起: %#v", got)
			}
		default:
			t.Fatal("停止后的迟到事件应变为空清理事件")
		}
		unsub()
		assertNoThinkingContinueReplay(t, r)
	}
}

func thinkingContinueSourceSection(t *testing.T, src, start, end string) string {
	t.Helper()
	i := strings.Index(src, start)
	if i < 0 {
		t.Fatalf("找不到契约起点 %q", start)
	}
	part := src[i:]
	j := strings.Index(part[len(start):], end)
	if j < 0 {
		t.Fatalf("找不到契约终点 %q", end)
	}
	return part[:len(start)+j]
}

// 前端无构建链：这里只锁定 UI/runtime 的事件与隔离清理契约，不启动 JS/浏览器。
func TestThinkingContinueUIStaticContract(t *testing.T) {
	b, err := os.ReadFile("web/app-chat.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	hint := thinkingContinueSourceSection(t, src, "function setThinkingContinueHint(", "function setStreamUI(")
	for _, want := range []string{"hint.remove()", "hint.textContent = text", "view.hint.insertAdjacentElement('afterend', hint)"} {
		if !strings.Contains(hint, want) {
			t.Errorf("缺少独立提示节点契约 %q", want)
		}
	}
	for _, forbidden := range []string{"view.hint.textContent", "streamText", "streamThinking", "removeLive", "hideRunError"} {
		if strings.Contains(hint, forbidden) {
			t.Errorf("清续接提示不能改其他提示/内容: %q", forbidden)
		}
	}
	branch := thinkingContinueSourceSection(t, src, "case 'thinking_continue':", "case 'auto_retry':")
	if !strings.Contains(branch, "setThinkingContinueHint(view, ev.output || '')") {
		t.Fatal("thinking_continue 应按 output 非空显示、空清理")
	}
	for _, forbidden := range []string{"streamText =", "streamThinking =", "removeLive(", "hideRunError(", "view.hint.textContent"} {
		if strings.Contains(branch, forbidden) {
			t.Errorf("续接事件不能冒充 retry 或覆盖其他等待提示: %q", forbidden)
		}
	}
	handler := thinkingContinueSourceSection(t, src, "function handleEvent(", "switch (ev.type)")
	for _, typ := range []string{"delta", "reasoning", "tool_start", "stopped", "error", "done", "retry"} {
		if !strings.Contains(handler, "'"+typ+"'") {
			t.Errorf("缺少 %s 的提示清理契约", typ)
		}
	}
	if !strings.Contains(handler, "setThinkingContinueHint(view, '')") {
		t.Fatal("正常输出/终态应只清独立续接提示")
	}
	for _, section := range []struct{ start, end string }{
		{"function setStreamUI(", "function handleEvent("},
		{"function applyPending(", "async function sendMidrun("},
		{"async function stopRun(", "const CTX_COLORS"},
	} {
		part := thinkingContinueSourceSection(t, src, section.start, section.end)
		if !strings.Contains(part, "setThinkingContinueHint(view, '')") {
			t.Errorf("%s 缺少结束/插话/停止时的清理", section.start)
		}
	}
}

func TestThinkingContinueRuntimeSSESubscribeContract(t *testing.T) {
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	i := strings.Index(src, "func (a *App) serveRun(")
	if i < 0 {
		t.Fatal("缺少执行流 SSE 入口")
	}
	part := src[i:]
	subscribe := strings.Index(part, "ch, unsub := run.subscribe()")
	snapshot := strings.Index(part, "run.snapshot()")
	if subscribe < 0 || snapshot < subscribe || !strings.Contains(part, "case ev, ok := <-ch:") {
		t.Fatal("SSE 接流必须先订阅，再补文本快照，并转发订阅通道中的续接提示")
	}
}

func TestThinkingContinueRuntimeStopAndCloseStaticContract(t *testing.T) {
	b, err := os.ReadFile("runtime.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, section := range []struct{ start, end string }{
		{"func (a *App) stopRunBy(", "// stopWatchdogDefault"},
		{"func (a *App) closeRun(", "// runOf"},
	} {
		part := thinkingContinueSourceSection(t, src, section.start, section.end)
		if !strings.Contains(part, "r.clearThinkingContinue(id)") {
			t.Errorf("%s 缺少立即清提示契约", section.start)
		}
	}
}
