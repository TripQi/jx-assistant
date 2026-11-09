package main

// 子对话继承的两处「静默失败」回归（2026-11-10 用户实锤会话 4e1a5e5dafee5176c2）：
//
//	现场：父对话 baa507e0 首轮执行中（一条完整往返都还没有）时直发建子对话 →
//	DB 里既没有 inherit 标记行（所以没有「来自主对话」折叠段），也没有任何用户
//	消息行（发送时正文被清空 → 400；用户点错误条上的「重试」→ handleRetry 按
//	「用户消息已在库里」的假设直接开跑，得到一个只有系统提示词与环境快照的裸
//	回合，模型只好自己去 read_session_messages 翻父会话，白烧两次调用）。
//
//	1. copyInheritedContext 没继承到东西时要给原因，建会话据此在子对话里落一条
//	   看得见的交代（用户不再面对「没有折叠行也没有说明」的空子对话）；
//	2. handleRetry 要拦住「库里压根没有用户消息」的会话，不许开跑裸回合。
//
// 继承正常、克隆字段等既有语义由 sessioncreate_test.go 覆盖。

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// ① 父对话完全空着：子对话里落一条交代，说清「父对话还没有任何对话内容」。
func TestCreateSubNoInheritLeavesNotice(t *testing.T) {
	a, main := newDispatchTestApp(t)
	sub, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Store.RawMessagesAll(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Kind == inheritKind {
			t.Fatalf("没有可继承内容时不该写 inherit 标记行（前端会画一个「来自主对话 · 0 条」）: %+v", r)
		}
	}
	if len(rows) != 1 {
		t.Fatalf("应有且仅有一条交代行，实得 %d 行", len(rows))
	}
	if rows[0].Kind != kindReport || rows[0].Origin != "inherit" {
		t.Fatalf("交代行应是 origin=inherit 的 report 行: %+v", rows[0])
	}
	if !strings.Contains(rows[0].Text(), "父对话还没有任何对话内容") {
		t.Fatalf("交代行要写清原因: %q", rows[0].Text())
	}
}

// ② 父对话首轮执行中（有工具调用、没有任何完整往返）：同样交代原因，
// 且不能把悬空的工具链复制进子对话（那样模型请求会不合法）。
func TestCreateSubRunningParentNoCompleteTurnNotice(t *testing.T) {
	a, main := newDispatchTestApp(t)
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("user", "父对话正在做的事"), CreatedAt: now()})
	_ = a.Store.AppendRow(main.ID, &StoredMessage{
		Message: Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Type: "function",
			Function: ToolCallFunc{Name: "run_command"}}}},
		CreatedAt: now(),
	})
	if _, ok := a.beginRun(main.ID); !ok { // 占住执行权＝父会话「正在执行」
		t.Fatal("父会话占位失败")
	}
	defer a.finishRun(main.ID)

	sub, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Store.RawMessagesAll(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.Kind == inheritKind {
			t.Fatalf("父会话这一轮没答完时不该写 inherit 标记行: %+v", r)
		}
		if r.Role == "tool" || len(r.ToolCalls) > 0 {
			t.Fatalf("不许把悬空的工具调用复制进子对话: %+v", r)
		}
	}
	if len(rows) != 1 || rows[0].Kind != kindReport || !strings.Contains(rows[0].Text(), "还没答完") {
		t.Fatalf("应落一条「这一轮还没答完」的交代行，实得 %+v", rows)
	}
}

// ③ 父对话有完整往返（未执行）：照旧继承，且不写交代行。
func TestCreateSubInheritsWithoutNotice(t *testing.T) {
	a, main := newDispatchTestApp(t)
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("user", "父对话的问题"), CreatedAt: now()})
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("assistant", "父对话的回答"), CreatedAt: now()})
	sub, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Store.RawMessagesAll(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) == 0 || rows[0].Kind != inheritKind {
		t.Fatalf("有可继承内容时首行应是 inherit 标记行: %+v", rows)
	}
	for _, r := range rows {
		if r.Kind == kindReport && r.Origin == "inherit" {
			t.Fatalf("继承成功时不该写「未继承」交代: %q", r.Text())
		}
	}
}

// ④ 重试守卫：没有任何自己的对话行（发送阶段就被拒）→ 409 说清原因，不开跑。
func TestRetryRefusedWithoutOwnTurn(t *testing.T) {
	a, main := newDispatchTestApp(t)
	// 子对话：父会话空着 → 继承不到东西，只剩一条「未继承」交代行
	sub, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	// 交代行是系统注入，不算「用户问过的那一轮」
	for _, id := range []string{main.ID, sub.ID} {
		req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/retry", strings.NewReader(""))
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		a.handleRetry(rec, req)
		if rec.Code != 409 {
			t.Fatalf("没有用户消息的会话重试应 409，实得 %d %s", rec.Code, rec.Body.String())
		}
		if !strings.Contains(rec.Body.String(), "还没有你发过的消息") {
			t.Fatalf("拒绝原因要说清（前端直接展示这条文案）: %s", rec.Body.String())
		}
	}
	// 有真人提问（即使还没答完）→ 放行
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("user", "问一句"), CreatedAt: now()})
	if !a.hasOwnTurn(main.ID) {
		t.Fatal("有用户提问的会话应允许重试")
	}
}

// ⑤ 继承段里的行不算「自己的回合」：继承来的父对话提问不是这一侧被问的问题。
func TestHasOwnTurnSkipsInheritedPrefix(t *testing.T) {
	a, main := newDispatchTestApp(t)
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("user", "父对话的问题"), CreatedAt: now()})
	_ = a.Store.AppendRow(main.ID, &StoredMessage{Message: TextMessage("assistant", "父对话的回答"), CreatedAt: now()})
	sub, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	if a.hasOwnTurn(sub.ID) {
		t.Fatal("只有继承段的子对话不该被当成「有自己的回合」（重试会开出一个没有提问的裸回合）")
	}
	_ = a.Store.AppendRow(sub.ID, &StoredMessage{Message: TextMessage("user", "这一侧真正的问题"), CreatedAt: now()})
	if !a.hasOwnTurn(sub.ID) {
		t.Fatal("继承段之后有了自己的提问，应允许重试")
	}
}
