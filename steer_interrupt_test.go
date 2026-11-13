package main

// 插话立即打断模型调用的隔离验收测试（用户 2026-11-03 需求）：
//
//   模型调用中（流式思考/正文输出/重试等待）插话 → 立即掐断当前模型流，
//   半截输出带打断标注落库，下一步带着插话重组上下文重新调模型；
//   工具执行中插话 → 不打断，照旧等下一步调用模型前注入（现有行为，不回归）。
//
// 打断路径的完整链路走真实组件：executeRun → RunAgent → StreamChat →
// httptest 假网关（第一次请求流一段正文后挂起到客户端断开，第二次请求正常
// 收尾），插话走 pushPending(steer)（kick 在 pushPending 内统一触发，与
// handleInterject / deliverToSession 同一入口）。
//
// 验收点：
//  1. 第一次模型流被掐断：历史里有半截 assistant 消息，带「（被插话打断，
//     输出提前结束）」标注；
//  2. 插话消息在半截之后落库（顺序对，模型下一轮看到的就是这个顺序）；
//  3. 第二次模型请求（假网关收到的第 2 个 body）包含插话内容——插话真的
//     进了重组后的上下文；
//  4. 本轮正常收尾（最后一条 assistant 是第二次请求的完整回复），不是报错中断。

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestSteerInterruptsModelStream 模型流式输出中途插话 → 立即打断生效。
func TestSteerInterruptsModelStream(t *testing.T) {
	var mu sync.Mutex
	bodies := []string{}          // 假网关收到的每个请求体（按到达顺序）
	firstFlushing := make(chan struct{}) // 第一次请求已流出半截正文（模型调用进行中的信号）

	writeSSE := func(w http.ResponseWriter, txt string) {
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%q}}]}\n\n", txt)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if n == 1 {
			// 第一次请求：流出半截正文后挂起——直到客户端（stepCtx 取消）断开连接
			writeSSE(w, "半截正文：我正在慢慢")
			fl.Flush()
			close(firstFlushing)
			<-r.Context().Done() // 客户端断开（打断生效）才返回
			return
		}
		// 后续请求：正常完整收尾（模型消化插话后的新回复）
		writeSSE(w, "收到插话后的完整回复")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)

	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL,
		Models: []ModelCfg{{ID: "fake-m"}}}}
	// 默认模型体系：会话没手选时跟全局默认模型
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	a.Components = &ComponentRegistry{}
	m := &SessionMeta{ID: "steerkick", Title: "插话打断测试", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	a.appendMessage(m, TextMessage("user", "开始"))

	run, ok := a.beginRun(m.ID)
	if !ok {
		t.Fatal("beginRun 失败")
	}
	go a.executeRun(m, run, nil)

	select { // 等模型流真正开始输出（半截正文已从假网关发出）
	case <-firstFlushing:
	case <-time.After(10 * time.Second):
		t.Fatal("模型流迟迟未建立")
	}
	// 再等客户端**真正收到**那半截正文：网关 flush 与客户端读到之间是异步的，
	// 此前只看 firstFlushing 就插话，打断偶尔落在正文入缓冲之前 → 半截为空、
	// 按产品行为（一个字没出就不造空半截）不落库，测试随机红。partial 是流式
	// 正文落进 sessionRun 的唯一源头，等它含正文才是「输出进行中」的可靠信号。
	deadline := time.Now().Add(10 * time.Second)
	for {
		run.mu.Lock()
		got := strings.Contains(run.partial, "半截正文")
		run.mu.Unlock()
		if got {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("客户端迟迟没收到半截正文（partial 一直为空）")
		}
		time.Sleep(10 * time.Millisecond)
	}
	a.pushPending(m.ID, "steer", "插话：先停下，改听这句", "", "")

	select { // 等执行体收尾
	case <-run.done:
	case <-time.After(20 * time.Second):
		t.Fatal("执行体迟迟未收尾（打断没有生效？）")
	}

	all, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	var halfAI, steerUser, finalAI = -1, -1, -1
	for i, msg := range all {
		txt, _ := msg.Content.(string)
		if msg.Role == "assistant" && strings.Contains(txt, "半截正文") {
			halfAI = i
			if !strings.Contains(txt, "（被插话打断，输出提前结束）") {
				t.Errorf("半截 assistant 消息缺少打断标注: %q", txt)
			}
		}
		if msg.Role == "user" && txt == "插话：先停下，改听这句" {
			steerUser = i
		}
		if msg.Role == "assistant" && strings.Contains(txt, "收到插话后的完整回复") {
			finalAI = i
		}
	}
	if halfAI < 0 || steerUser < 0 || finalAI < 0 {
		t.Fatalf("历史不完整：半截=%d 插话=%d 完整回复=%d（应有这三条）", halfAI, steerUser, finalAI)
	}
	if !(halfAI < steerUser && steerUser < finalAI) {
		t.Errorf("落库顺序不对：半截(%d) 应在插话(%d) 之前、插话应在完整回复(%d) 之前", halfAI, steerUser, finalAI)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("假网关只收到 %d 次请求，打断后应重调模型", len(bodies))
	}
	if !strings.Contains(bodies[1], "插话：先停下，改听这句") {
		t.Errorf("第二次模型请求没有带上插话内容")
	}
	t.Logf("链路验证：首次请求被掐断 → 半截落库 → 插话注入 → 重调模型（共 %d 次请求）", len(bodies))
}

// TestSteerDuringToolRunWaitsForNextStep 工具执行中插话：不打断工具，插话在
// steer 队列里等待（服务端持有 = 前端虚线框的数据源），工具跑完才在下一步
// 调用模型前注入落库、进入新请求。这是改造刻意保留的既有行为——打断只属于
// 模型调用段，工具执行段的插话「等得起」。
func TestSteerDuringToolRunWaitsForNextStep(t *testing.T) {
	var mu sync.Mutex
	bodies := []string{}
	firstDone := make(chan struct{})

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(body))
		n := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fl, _ := w.(http.Flusher)
		if n == 1 {
			// 首请求：返回一个约 2 秒的工具调用（ping 计时），给「工具执行中插话」留窗口
			fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_t1\",\"function\":{\"name\":\"run_command\",\"arguments\":\"{\\\"command\\\":\\\"ping -n 3 127.0.0.1\\\"}\"}}]}}]}\n\n")
			fmt.Fprint(w, "data: [DONE]\n\n")
			fl.Flush()
			close(firstDone)
			return
		}
		// 后续请求（带工具结果，可能同时带插话）：正常完整收尾
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"工具跑完后的完整回复\"}}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(srv.Close)

	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL,
		Models: []ModelCfg{{ID: "fake-m"}}}}
	// 默认模型体系：会话没手选时跟全局默认模型
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	a.Components = &ComponentRegistry{}
	m := &SessionMeta{ID: "steertool", Title: "工具中插话测试", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	a.appendMessage(m, TextMessage("user", "开始"))

	run, ok := a.beginRun(m.ID)
	if !ok {
		t.Fatal("beginRun 失败")
	}
	go a.executeRun(m, run, nil)

	select {
	case <-firstDone: // 假网关已返回工具调用（模型步结束）
	case <-time.After(10 * time.Second):
		t.Fatal("首请求迟迟未到")
	}
	// 等到「工具调用已落库、工具结果还没落库」的窗口 = 工具正在执行（ping ~2 秒）
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		all, _ := a.Store.RawMessagesAll(m.ID)
		called, finished := false, false
		for _, msg := range all {
			if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
				called = true
			}
			if msg.Role == "tool" {
				finished = true
			}
		}
		if called && !finished {
			break
		}
		if finished {
			t.Fatal("没卡进工具执行窗口（工具瞬间跑完了，窗口判定失效）")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 工具正在执行：此刻插话（与 handleInterject 完全同一入口，含 kick）
	a.pushPending(m.ID, "steer", "工具执行中的插话", "", "")
	// 插话此刻必须在 steer 队列里等待（服务端持有 = 前端虚线框的数据源），
	// 而不是被丢掉或立即注入
	if st := a.pendingOf(m.ID); len(st.Steer) != 1 || st.Steer[0].Content != "工具执行中的插话" {
		t.Fatalf("插话没进 steer 等待队列： %+v", st.Steer)
	}

	select {
	case <-run.done:
	case <-time.After(30 * time.Second):
		t.Fatal("执行体迟迟未收尾")
	}

	all, err := a.Store.RawMessagesAll(m.ID)
	if err != nil {
		t.Fatalf("读历史失败: %v", err)
	}
	steerAt, toolResultAt, aborted := -1, -1, false
	for i, msg := range all {
		txt, _ := msg.Content.(string)
		if strings.Contains(txt, "（被插话打断") {
			aborted = true
		}
		if msg.Role == "tool" {
			toolResultAt = i
			if strings.Contains(txt, "TTL=") || strings.Contains(txt, "字节") || strings.Contains(txt, "bytes") {
				// ping 完整输出：工具真的跑完了
			}
		}
		if msg.Role == "user" && txt == "工具执行中的插话" {
			steerAt = i
		}
	}
	if aborted {
		t.Error("工具执行段的插话不该产生打断（历史里出现了打断标注）")
	}
	if toolResultAt < 0 {
		t.Fatal("工具结果没落库（工具未完整执行）")
	}
	if steerAt < 0 {
		t.Fatal("插话消息没落库（发不出去）")
	}
	if steerAt < toolResultAt {
		t.Errorf("插话(序 %d) 落在工具结果(序 %d) 之前：应等工具完、下一步调用模型前注入", steerAt, toolResultAt)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 2 {
		t.Fatalf("假网关只收到 %d 次请求", len(bodies))
	}
	if !strings.Contains(bodies[1], "工具执行中的插话") {
		t.Errorf("第二次模型请求没带上插话内容（注入没进上下文）")
	}
	t.Logf("工具执行中插话：等待于 steer 队列 → 工具完整跑完（结果序 %d）→ 插话注入（序 %d）→ 进入第二次请求 ✓", toolResultAt, steerAt)
}
