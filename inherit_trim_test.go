package main

// 子对话继承 / 分支对话的「尾巴」处理（2026-11-10 用户实测 871ce3a3 现场）：
//
//	库里有的都是「已完成的小步」（模型正在流式输出的那一步不落库），所以整份复制
//	即可，不再判断"整轮"。唯一不能带走的是尾部**未闭合的工具调用组**：assistant
//	那一行（含 tool_calls）在模型这一步输出完就落库，工具随后才逐个执行、逐个落
//	结果——中间那段空窗（实测 91 秒）里库里就是「有 tool_calls、结果没齐」的半开
//	组，这种前缀发给模型不合法。
//
//	本文件就是这两件事的验收：trimOpenToolGroup 的判定、子对话继承、以及两个
//	分支入口（会话级 / 消息级 upTo）都不会把半开组带出去、也不会误删已完成内容。

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rowAsst 造一条 assistant 行（calls 为空＝纯正文答复）。
func rowAsst(text string, calls ...string) *StoredMessage {
	m := Message{Role: "assistant"}
	if text != "" {
		m.Content = text
	}
	for _, id := range calls {
		m.ToolCalls = append(m.ToolCalls, ToolCall{ID: id, Type: "function",
			Function: ToolCallFunc{Name: "run_command"}})
	}
	return &StoredMessage{Message: m, CreatedAt: now()}
}

// rowTool 造一条工具结果行。
func rowTool(callID, out string) *StoredMessage {
	return &StoredMessage{Message: Message{Role: "tool", Content: out, ToolCallID: callID}, CreatedAt: now()}
}

func rowUser(text string) *StoredMessage {
	return &StoredMessage{Message: TextMessage("user", text), CreatedAt: now()}
}

// assertPrefixLegal 断言这段前缀对模型合法：每个 tool_call 都有对应结果、没有
// 孤立的结果行（tool 行只能紧跟它所属的 assistant 之后）。
func assertPrefixLegal(t *testing.T, rows []*StoredMessage) {
	t.Helper()
	for i, x := range rows {
		switch x.Role {
		case "assistant":
			if len(x.ToolCalls) == 0 {
				continue
			}
			for _, tc := range x.ToolCalls {
				found := false
				for _, y := range rows[i+1:] {
					if y.Role == "tool" && y.ToolCallID == tc.ID {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("前缀不合法：第 %d 行的 tool_call %s 没有对应结果行", i, tc.ID)
				}
			}
		case "tool":
			if i == 0 {
				t.Fatalf("前缀不合法：第 0 行就是孤立的结果行")
			}
		}
	}
}

// ① trimOpenToolGroup 的判定（表驱动）。
func TestTrimOpenToolGroup(t *testing.T) {
	cases := []struct {
		name string
		rows []*StoredMessage
		want int // 期望留下来的行数
	}{
		{"尾巴是纯正文：不动", []*StoredMessage{rowUser("问"), rowAsst("答")}, 2},
		{"尾巴是闭合工具组：连结果一起留",
			[]*StoredMessage{rowUser("问"), rowAsst("第一轮完成", "c1"), rowTool("c1", "结果")}, 3},
		{"三条结果一条都没回：整组剥掉",
			[]*StoredMessage{rowUser("问"), rowAsst("答"), rowAsst("布置", "c1", "c2", "c3")}, 2},
		{"三个调用只回一个：整组剥掉（含已回的那条）",
			[]*StoredMessage{rowUser("问"), rowAsst("答"), rowAsst("布置", "c1", "c2", "c3"), rowTool("c1", "a")}, 2},
		{"剥掉后露出上一组：上一组闭合就停在它",
			[]*StoredMessage{rowUser("问"), rowAsst("第一轮", "c1"), rowTool("c1", "ok"),
				rowAsst("第二轮", "c2")}, 3},
		{"只有半开组、前面没有完整内容：全剥",
			[]*StoredMessage{rowAsst("布置", "c1")}, 0},
		{"空表", nil, 0},
	}
	for _, c := range cases {
		got := trimOpenToolGroup(c.rows)
		if len(got) != c.want {
			t.Fatalf("%s：应留 %d 行，实留 %d 行", c.name, c.want, len(got))
		}
		assertPrefixLegal(t, got)
	}
}

// seedParent 造一个「模型刚输出完一步、工具结果还没回来」的父会话（用户实测现场）。
func seedParent(t *testing.T, a *App, parentID string) {
	t.Helper()
	appendRowOf(t, a, parentID, TextMessage("user", "父对话的问题"), kindChat, "")
	appendRowOf(t, a, parentID, Message{Role: "assistant", Content: "好的，明白你的测试意图…第一轮",
		ToolCalls: []ToolCall{{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "run_command"}}}}, kindChat, "")
	appendRowOf(t, a, parentID, Message{Role: "tool", Content: "2026-11-10 01:26:30.645", ToolCallID: "c1"}, kindChat, "")
	appendRowOf(t, a, parentID, Message{Role: "assistant", Content: "第一轮完成。第二轮：执行——开始等待",
		ToolCalls: []ToolCall{{ID: "c2", Type: "function", Function: ToolCallFunc{Name: "run_command"}}}}, kindChat, "")
	// c2 的结果还没落库（工具还在跑）——正是 01:26:44 建子对话时库里的状态
}

// seedFinishedParent 造一个**已完成两轮**的父会话（每步都有结果，末尾是收尾答案）。
func seedFinishedParent(t *testing.T, a *App, parentID string) {
	t.Helper()
	seedParent(t, a, parentID)
	appendRowOf(t, a, parentID, Message{Role: "tool", Content: "等待结束", ToolCallID: "c2"}, kindChat, "")
	appendRowOf(t, a, parentID, TextMessage("assistant", "两轮测试完成，如实汇报时间线…"), kindChat, "")
}

// ② 子对话继承：父会话执行中 + 尾巴是半开组 → 继承到上一组为止，半开组不带。
func TestCreateSubInheritsUpToLastClosedStep(t *testing.T) {
	a, main := newDispatchTestApp(t)
	seedParent(t, a, main.ID)
	if _, ok := a.beginRun(main.ID); !ok {
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
	if len(rows) == 0 || rows[0].Kind != inheritKind {
		t.Fatalf("首行应是 inherit 标记行: %+v", rows)
	}
	var im inheritMark
	if err := json.Unmarshal([]byte(rows[0].Text()), &im); err != nil {
		t.Fatal(err)
	}
	body := rows[1:]
	if im.Count != len(body) {
		t.Fatalf("标记行 count=%d 与实际复制行数 %d 不一致（前端靠它切分继承段）", im.Count, len(body))
	}
	// 期望：user + 第一轮(assistant+结果) = 3 行；半开的第二轮整组不在
	if len(body) != 3 {
		t.Fatalf("应继承到上一组为止（3 行），实得 %d 行: %+v", len(body), body)
	}
	for _, x := range body {
		if x.Role == "assistant" && len(x.ToolCalls) > 0 {
			if x.ToolCalls[0].ID != "c1" {
				t.Fatalf("半开组的 assistant 不该被继承: %+v", x)
			}
		}
	}
	assertPrefixLegal(t, body)

	// 模型真正看到的序列（buildContext 输出）也必须合法：不能有裸 tool_calls
	msgs := a.buildContext(sub, "系统提示词")
	seen := map[string]bool{}
	for _, m := range msgs {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				seen[tc.ID] = false
			}
		}
	}
	for _, m := range msgs {
		if m.Role == "tool" {
			if _, ok := seen[m.ToolCallID]; ok {
				seen[m.ToolCallID] = true
			}
		}
	}
	for id, closed := range seen {
		if !closed {
			t.Fatalf("模型请求里有裸 tool_call（无结果）: %s", id)
		}
	}
}

// ③ 父会话空闲时同样整份复制（含工具步骤），不再有"只认纯正文"的截断。
func TestCreateSubIdleParentCopiesEverything(t *testing.T) {
	a, main := newDispatchTestApp(t)
	seedFinishedParent(t, a, main.ID)
	sub, err := a.createSession(createSessionParams{ParentID: main.ID, CloneFromParent: true, CopyHistory: true})
	if err != nil {
		t.Fatal(err)
	}
	rows, err := a.Store.RawMessagesAll(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows)-1 != 6 {
		t.Fatalf("空闲态应整份复制（6 行：提问 + 两轮工具组 + 收尾答案），实得 %d 行", len(rows)-1)
	}
	assertPrefixLegal(t, rows[1:])
}

// ④ 会话级分支（左栏/右栏右键，无 upTo）：同样只剥半开组。
func TestForkSessionLevelTrimsOpenGroup(t *testing.T) {
	a, main := newDispatchTestApp(t)
	seedParent(t, a, main.ID)
	if _, ok := a.beginRun(main.ID); !ok {
		t.Fatal("占位失败")
	}
	defer a.finishRun(main.ID)

	fork := doFork(t, a, main.ID, nil)
	rows, err := a.Store.RawMessagesAll(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 3 {
		t.Fatalf("分支应复制到上一组为止（3 行），实得 %d 行: %+v", len(rows), rows)
	}
	assertPrefixLegal(t, rows)
}

// ⑤ 消息级分支（每条回答下的按钮，upTo=那条回答的 seq）：不误删，也不带半开组。
func TestForkMessageLevelUpToKeepsAnswer(t *testing.T) {
	a, main := newDispatchTestApp(t)
	seedFinishedParent(t, a, main.ID)
	rows, _ := a.Store.RawMessagesAll(main.ID)
	var answerSeq int64
	for _, x := range rows {
		if x.Role == "assistant" && len(x.ToolCalls) == 0 {
			answerSeq = x.Seq
		}
	}
	fork := doFork(t, a, main.ID, map[string]any{"upTo": answerSeq})
	got, err := a.Store.RawMessagesAll(fork.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(rows) {
		t.Fatalf("upTo 覆盖全部行时应一行不少（%d），实得 %d", len(rows), len(got))
	}
	assertPrefixLegal(t, got)
}

// doFork 调一次 POST /api/sessions/{id}/fork，返回新会话。
func doFork(t *testing.T, a *App, id string, body map[string]any) *SessionMeta {
	t.Helper()
	payload := ""
	if body != nil {
		b, _ := json.Marshal(body)
		payload = string(b)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/sessions/"+id+"/fork", strings.NewReader(payload))
	req.SetPathValue("id", id)
	rec := httptest.NewRecorder()
	a.handleFork(rec, req)
	if rec.Code != 200 {
		t.Fatalf("分支失败: %d %s", rec.Code, rec.Body.String())
	}
	var m SessionMeta
	if err := json.Unmarshal(rec.Body.Bytes(), &m); err != nil {
		t.Fatalf("分支响应解析失败: %v %s", err, rec.Body.String())
	}
	return &m
}
