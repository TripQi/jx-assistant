package main

// 「AI 自动会话标题」组件的验收测试：
//  1. PATCH 钉住语义：手动改 title 即打钉 → autoTitle 改动被 409 拒 →
//     autoTitleUnpin 解钉 → autoTitle 恢复可改且不打钉（对齐老 DSH「user
//     来源标题钉住」决策；GET 详情回带 titlePinned）；
//  2. GET /api/sessions/{id}/messages 轻量文本列表：role=user 过滤 + 插话
//    （origin=interject）排除；
//  3. POST /api/llm/ask 独立单轮调用（httptest 假 LLM 的 SSE 应答）——
//     一次请求一次输出，且不往会话消息表落任何行；
//  4. 全链路集成（需共享便携 Python，缺则跳过）：真实组件 worker 的节奏
//     （第 1 轮命名、每满 3 轮修订、插话不计轮次）与手动命名钉住后停改。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeLLMServer 起一个 OpenAI 兼容的 SSE 假模型：无 tools 的请求回复 *reply
// （指向变量的指针：测试中途可切换后续回复）。sysGot 非空时记录最近一次请求
// 的 system 提示词（E2E 断言 worker 用的命名规则版式用）。
func fakeLLMServer(t *testing.T, reply *string, sysGot *string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if sysGot != nil {
			var req struct {
				Messages []struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"messages"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			for _, m := range req.Messages {
				if m.Role == "system" {
					*sysGot = m.Content
					break
				}
			}
		}
		enc, _ := json.Marshal(*reply)
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"choices\":[{\"delta\":{\"content\":%s}}]}\n\n", enc)
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newTitleTestApp 建一个接假 LLM 的最小 App（Store/DataDir 隔离在临时目录）。
func newTitleTestApp(t *testing.T, llmURL string) *App {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: llmURL, APIKey: "test",
		Models: []ModelCfg{{ID: "fake-m"}}}}
	// 默认模型体系：会话没手选模型时跟全局默认模型（空着会报「还没有设置全局
	// 默认模型」，渠道 Key 也不能为空——新语义下两者都会在发请求前就被拒）
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	a.Cfg.BaseURL = llmURL
	a.Cfg.Model = "fake-model"
	return a
}

func doReq(t *testing.T, a *App, method, path, body string) (*httptest.ResponseRecorder, map[string]any) {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	a.Routes().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

func mkTitleSession(t *testing.T, a *App, id string) {
	t.Helper()
	if err := a.Store.Create(&SessionMeta{ID: id, Title: "新会话", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
}

// ① PATCH 钉住语义。
func TestTitlePinSemantics(t *testing.T) {
	a := newTitleTestApp(t, "")
	mkTitleSession(t, a, "s-pin")

	// 用户手动命名 → 打钉
	rec, _ := doReq(t, a, "PATCH", "/api/sessions/s-pin", `{"title":"我的手工命名"}`)
	if rec.Code != 200 {
		t.Fatalf("手动命名应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	rec, d := doReq(t, a, "GET", "/api/sessions/s-pin", "")
	if rec.Code != 200 || d["titlePinned"] != true {
		t.Fatalf("手动命名后应 titlePinned=true，得到 code=%d pinned=%v", rec.Code, d["titlePinned"])
	}
	// 组件通道（autoTitle）改钉住会话 → 409 拒绝
	rec, _ = doReq(t, a, "PATCH", "/api/sessions/s-pin", `{"title":"AI 想改","autoTitle":true}`)
	if rec.Code != 409 {
		t.Fatalf("钉住会话的 autoTitle 改名应 409，得到 %d", rec.Code)
	}
	if m, _ := a.Store.Get("s-pin"); m.Title != "我的手工命名" {
		t.Fatalf("钉住标题不应被改写，得到 %q", m.Title)
	}
	// 解钉 → autoTitle 恢复可改，且成功后不打钉
	rec, _ = doReq(t, a, "PATCH", "/api/sessions/s-pin", `{"autoTitleUnpin":true}`)
	if rec.Code != 200 {
		t.Fatalf("解钉应 200，得到 %d", rec.Code)
	}
	rec, _ = doReq(t, a, "PATCH", "/api/sessions/s-pin", `{"title":"AI 修订后的标题","autoTitle":true}`)
	if rec.Code != 200 {
		t.Fatalf("解钉后 autoTitle 应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	rec, d = doReq(t, a, "GET", "/api/sessions/s-pin", "")
	if d["titlePinned"] != false {
		t.Fatalf("autoTitle 成功后不应打钉，得到 %v", d["titlePinned"])
	}
	if m, _ := a.Store.Get("s-pin"); m.Title != "AI 修订后的标题" {
		t.Fatalf("autoTitle 标题未生效: %q", m.Title)
	}
}

// ①′ 钉住标记随会话列表/详情下发（会话右键菜单「恢复自动命名」按它显隐）。
func TestSessionListCarriesTitlePinned(t *testing.T) {
	a := newTitleTestApp(t, "")
	mkTitleSession(t, a, "s-pin2")
	mkTitleSession(t, a, "s-free")

	// 初始都没钉住：列表与详情都应回 false/缺省
	inList := func(id string) map[string]any {
		_, d := doReq(t, a, "GET", "/api/sessions", "")
		sessions, _ := d["sessions"].([]any)
		for _, sm := range sessions {
			if m, ok := sm.(map[string]any); ok && m["id"] == id {
				return m
			}
		}
		return nil
	}
	if m := inList("s-pin2"); m == nil || m["titlePinned"] == true {
		t.Fatalf("未钉住会话不应带 titlePinned=true，得到 %v", m)
	}
	// 手动命名 → 列表带 true，详情 meta 也带（worker 预检读 meta.titlePinned）
	doReq(t, a, "PATCH", "/api/sessions/s-pin2", `{"title":"手工名"}`)
	if m := inList("s-pin2"); m == nil || m["titlePinned"] != true {
		t.Fatalf("钉住后列表应带 titlePinned=true，得到 %v", m)
	}
	_, d := doReq(t, a, "GET", "/api/sessions/s-pin2", "")
	meta, _ := d["meta"].(map[string]any)
	if meta == nil || meta["titlePinned"] != true {
		t.Fatalf("详情 meta 应带 titlePinned=true（worker 预检口径），得到 %v", meta)
	}
	if d["titlePinned"] != true {
		t.Fatalf("详情顶层应带 titlePinned=true，得到 %v", d["titlePinned"])
	}
	// 解钉 → 列表回落
	doReq(t, a, "PATCH", "/api/sessions/s-pin2", `{"autoTitleUnpin":true}`)
	if m := inList("s-pin2"); m == nil || m["titlePinned"] == true {
		t.Fatalf("解钉后列表不应再带 titlePinned=true，得到 %v", m)
	}
}

// ② 轻量消息列表的 role 过滤与插话排除。
func TestSessionMessagesRoleFilter(t *testing.T) {
	a := newTitleTestApp(t, "")
	mkTitleSession(t, a, "s-msg")
	for _, m := range []Message{
		TextMessage("user", "帮我做一个标题组件"),
		TextMessage("assistant", "好的，我来实现"),
		TextMessage("user", "顺便加上钉住机制"),
	} {
		if err := a.Store.Append("s-msg", m); err != nil {
			t.Fatalf("落消息失败: %v", err)
		}
	}
	// 一条插话消息（landUserSide 对 chat+origin 的落库形态）：不应进入轻量列表
	if err := a.Store.AppendRow("s-msg", &StoredMessage{
		Message: TextMessage("user", "插话：先别动文件"), Kind: kindChat, Origin: "interject", CreatedAt: now(),
	}); err != nil {
		t.Fatalf("落插话消息失败: %v", err)
	}
	rec, d := doReq(t, a, "GET", "/api/sessions/s-msg/messages?role=user", "")
	if rec.Code != 200 {
		t.Fatalf("messages 应 200，得到 %d", rec.Code)
	}
	msgs, _ := d["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("role=user 应只回 2 条（插话被排除），得到 %d（%v）", len(msgs), d)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "user" || first["text"] != "帮我做一个标题组件" {
		t.Fatalf("消息内容不符: %v", first)
	}
	// 不带 role：全部有文本的行（assistant 也算，插话仍排除）
	_, d = doReq(t, a, "GET", "/api/sessions/s-msg/messages", "")
	all, _ := d["messages"].([]any)
	if len(all) != 3 {
		t.Fatalf("不带 role 应回 3 条（插话排除），得到 %d", len(all))
	}
}

// ③ 独立单轮调用：一次请求一次输出，不落会话消息表。
func TestLLMAskSingleTurn(t *testing.T) {
	reply := "「组件标题」"
	llm := fakeLLMServer(t, &reply, nil)
	a := newTitleTestApp(t, llm.URL)
	mkTitleSession(t, a, "s-ask")

	rec, d := doReq(t, a, "POST", "/api/llm/ask",
		`{"session":"s-ask","system":"你是标题撰写员","user":"当前标题：（无）\n用户消息：[1] 做个标题组件"}`)
	if rec.Code != 200 {
		t.Fatalf("llm/ask 应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	if d["text"] != "「组件标题」" {
		t.Fatalf("应答文本不符: %v", d["text"])
	}
	// 独立路径：会话消息表不能多出任何行（不进上下文）
	msgs, err := a.Store.Messages("s-ask")
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 0 {
		t.Fatalf("独立调用不应落消息表，现有 %d 条", len(msgs))
	}
	// 空 user 拒绝
	rec, _ = doReq(t, a, "POST", "/api/llm/ask", `{"user":"  "}`)
	if rec.Code != 400 {
		t.Fatalf("空 user 应 400，得到 %d", rec.Code)
	}
}

// ④ 全链路：真实组件 worker（组件/session-title）——节奏（第 1 轮命名、
// 每满 3 轮修订）、插话不计轮次、手动命名钉住后停改。
func TestSessionTitleWorkerEndToEnd(t *testing.T) {
	testPython(t) // 共享运行时缺失时 skip（同桥钩子集成测试口径）；解释器由声明解析
	reply := "  \"订单导出与透视表\"\n"
	var sysGot string
	llm := fakeLLMServer(t, &reply, &sysGot)
	a := newTitleTestApp(t, llm.URL)
	// 临时组件根 + 组件目录 + 绝对解释器路径：真实 worker 脚本原样拷贝
	//（{python} 在 go test 的构建目录下解析不到仓库里的共享运行时）
	py := testPython(t)
	compRoot := t.TempDir()
	compDir := filepath.Join(compRoot, "session-title")
	if err := os.MkdirAll(compDir, 0755); err != nil {
		t.Fatal(err)
	}
	src, err := os.ReadFile(filepath.Join("组件", "session-title", "title_worker.py"))
	if err != nil {
		t.Skipf("组件 worker 脚本不可读: %v", err)
	}
	if err := os.WriteFile(filepath.Join(compDir, "title_worker.py"), src, 0644); err != nil {
		t.Fatal(err)
	}
	man := ComponentManifest{ID: "session-title", Name: "AI 自动会话标题", Version: "1"}
	man.Workers = []ComponentWorkerDef{{Name: "title", Command: []string{py, "title_worker.py"}, Hooks: []string{"run.end"}}}
	data, _ := json.Marshal(man)
	if err := os.WriteFile(filepath.Join(compDir, "manifest.json"), data, 0644); err != nil {
		t.Fatal(err)
	}
	a.Components = InitComponents(compRoot, filepath.Join(a.Cfg.DataDir, "components.json"))
	a.Bridge = newBridgeManager(a)
	t.Cleanup(func() { a.Bridge.KillComponent("session-title") })
	mkTitleSession(t, a, "s-e2e")

	appendMsg := func(text, origin string) {
		t.Helper()
		if origin == "" {
			if err := a.Store.Append("s-e2e", TextMessage("user", text)); err != nil {
				t.Fatal(err)
			}
			return
		}
		if err := a.Store.AppendRow("s-e2e", &StoredMessage{
			Message: TextMessage("user", text), Kind: kindChat, Origin: origin, CreatedAt: now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	runEnd := func() {
		a.emitComponentHooks("run.end", map[string]any{"sessionID": "s-e2e", "status": "done", "error": ""})
	}
	titleOf := func() string {
		m, _ := a.Store.Get("s-e2e")
		if m == nil {
			return "?"
		}
		return m.Title
	}
	waitTitle := func(want, why string) {
		t.Helper()
		deadline := time.Now().Add(20 * time.Second)
		for titleOf() != want {
			if time.Now().After(deadline) {
				t.Fatalf("%s：20 秒内未见到标题 %q（现值 %q）", why, want, titleOf())
			}
			time.Sleep(150 * time.Millisecond)
		}
	}
	stayTitle := func(want, why string, d time.Duration) {
		t.Helper()
		time.Sleep(d)
		if titleOf() != want {
			t.Fatalf("%s：标题应保持 %q，却被改成 %q", why, want, titleOf())
		}
	}

	// 第 1 轮（n=1）：命名
	appendMsg("把生产订单导出并做成透视表", "")
	runEnd()
	waitTitle("订单导出与透视表", "首轮命名")
	// 命名规则版式：核心段「最近主题|最初主题」近期在前（侧栏窄，靠前的字
	// 先被看到）——旧顺序「最初主题|最近主题」不许再出现
	if !strings.Contains(sysGot, "「最近主题|最初主题」") {
		t.Fatalf("命名提示词应含新版式「最近主题|最初主题」，实际：\n%s", sysGot)
	}
	if strings.Contains(sysGot, "「最初主题|最近主题」") {
		t.Fatalf("命名提示词仍含旧版式「最初主题|最近主题」：\n%s", sysGot)
	}

	// 第 2、3 轮（n=3）：不到改写轮，标题不动
	appendMsg("再加上按月份分组", "")
	appendMsg("透视表也导出一份", "")
	runEnd()
	stayTitle("订单导出与透视表", "n=3 未满节奏", 2500*time.Millisecond)

	// 插话（origin=interject）：不计轮次——若被误计为 n=4 会触发改写而显形
	appendMsg("插话：先别动那个文件", "interject")
	runEnd()
	stayTitle("订单导出与透视表", "插话不计轮次", 2500*time.Millisecond)

	// 第 4 轮（n=4）：满 3 轮，修订（换回复内容证明真调了模型）
	reply = "订单导出透视表＋月度汇总"
	appendMsg("补一个月度汇总页", "")
	runEnd()
	waitTitle("订单导出透视表＋月度汇总", "第 4 轮修订")

	// 手动命名 → 打钉 → 第 7 轮（n=7 满足节奏）也不再改
	rec, _ := doReq(t, a, "PATCH", "/api/sessions/s-e2e", `{"title":"我手工定的名字"}`)
	if rec.Code != 200 {
		t.Fatalf("手动命名应 200，得到 %d", rec.Code)
	}
	reply = "完全不同的新标题"
	appendMsg("第七轮的消息一", "")
	appendMsg("第七轮的消息二", "")
	appendMsg("第七轮的消息三", "")
	runEnd()
	stayTitle("我手工定的名字", "钉住后停改", 4*time.Second)
}
