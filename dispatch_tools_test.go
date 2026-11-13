package main

// 派发工具的参数与权限规则测试：
//   dispatch_task 的四个新参数（mode 模式 / plan_mode 计划模式 / attachments 附件 /
//   permission 权限）与 dispatch_sub 的 permission 参数——schema 有这几个格子、
//   模式取值与「人工新建会话」清单同源、权限「只能同等或降权」超出即拒绝、
//   附件路径在建会话之前预检（坏路径不留半个空任务）。

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newDispatchTestApp 建一个带临时库与临时工作区的 App。刻意不配模型提供方：
// 被派发的会话不会被真的跑起来（测试只关心派发这个动作本身落下的东西）。
func newDispatchTestApp(t *testing.T) (*App, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t) // Store 与 DataDir 都在临时目录（见其注释）
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{} // NewApp 才有的一份运行态（sessLock 用），测试里补上
	a.Cfg.Workspace = t.TempDir()
	// 模型：派发要过「建会话之前」的可用性判定（不回落），这里配一个 mock 渠道
	// 当全局默认模型（BaseURL=mock 不要密钥，也不会真的发请求）。
	a.Cfg.Providers = []ProviderCfg{{ID: "mock-prov", Name: "演示", BaseURL: "mock", Models: []ModelCfg{{ID: "mock-model"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "mock-prov", Model: "mock-model"}
	// 防回归闸门：登记库经 DataDir 落盘，DataDir 没隔离就会污染仓库真实
	// data/remote_workspaces.json——直接炸出来，不静默写脏用户数据
	if !filepath.IsAbs(a.Cfg.DataDir) {
		t.Fatalf("测试 App 的 DataDir 未隔离（%q），会污染真实登记库", a.Cfg.DataDir)
	}
	main := &SessionMeta{ID: "main", Title: "数字助理主对话", Mode: "assistant",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(main); err != nil {
		t.Fatalf("建主对话失败: %v", err)
	}
	return a, main
}

// dispatchCtx 造一个派发用的 toolContext（emit 必须有值：deliverSub 会推派发确认）。
func dispatchCtx(a *App, sess *SessionMeta) *toolContext {
	return &toolContext{app: a, sess: sess, emit: func(Event) {}}
}

func mustArgs(t *testing.T, m map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("序列化参数失败: %v", err)
	}
	return raw
}

// otherSessions 返回除主对话外的全部会话（刷新后从库里读，看派发真落下了什么）。
func otherSessions(t *testing.T, a *App) []*SessionMeta {
	t.Helper()
	list, err := a.Store.List()
	if err != nil {
		t.Fatalf("读会话列表失败: %v", err)
	}
	out := []*SessionMeta{}
	for _, m := range list {
		if m.ID != "main" {
			out = append(out, m)
		}
	}
	return out
}

func paramTypes(t *testing.T, tl Tool) map[string]any {
	t.Helper()
	return tl.Spec.Function.Parameters["properties"].(map[string]any)
}

// 两个派发工具的 schema 都要有新增的参数格子（含类型）。
func TestDispatchToolsSchemaHasNewParams(t *testing.T) {
	taskProps := paramTypes(t, dispatchTaskTool())
	for _, k := range []string{"task", "title", "workspace", "model", "mode", "plan_mode", "attachments", "permission"} {
		if _, ok := taskProps[k]; !ok {
			t.Fatalf("dispatch_task 缺参数 %s", k)
		}
	}
	if got := taskProps["plan_mode"].(map[string]any)["type"]; got != "boolean" {
		t.Fatalf("plan_mode 应是 boolean, got %v", got)
	}
	if got := taskProps["attachments"].(map[string]any)["type"]; got != "array" {
		t.Fatalf("attachments 应是 array, got %v", got)
	}
	// 模式清单直接写进 mode 参数的说明（值来自 chatModes），模型不问就知道有哪些
	modeDesc, _ := taskProps["mode"].(map[string]any)["description"].(string)
	for _, m := range chatModes {
		if !strings.Contains(modeDesc, m.Value) {
			t.Fatalf("dispatch_task 的 mode 说明里没有列出模式 %s：%s", m.Value, modeDesc)
		}
	}

	subProps := paramTypes(t, (&App{}).dispatchSubTool())
	for _, k := range []string{"task", "expert", "title", "workspace", "model", "permission"} {
		if _, ok := subProps[k]; !ok {
			t.Fatalf("dispatch_sub 缺参数 %s", k)
		}
	}
	if !strings.Contains((&App{}).dispatchSubTool().Spec.Function.Description, "permission") {
		t.Fatalf("dispatch_sub 的说明里应提到 permission 参数")
	}
}

// 模式清单与「人工新建会话」同一份数据源：清单不空、每个值都是已知会话模式
// （建会话校验用的 sessionModes），中文名与说明齐备，自检不炸。
func TestChatModeListIsSingleSourceOfTruth(t *testing.T) {
	assertChatModes() // 不 panic 即通过（不一致时启动就会拒绝）
	if len(chatModes) == 0 {
		t.Fatal("模式清单为空")
	}
	doc := chatModesDoc()
	for _, m := range chatModes {
		if !modeKnown(m.Value) {
			t.Fatalf("模式 %s 不在 sessionModes 里（建会话校验会拒）", m.Value)
		}
		if m.Label == "" || m.Desc == "" {
			t.Fatalf("模式 %s 缺中文名或说明", m.Value)
		}
		if !strings.Contains(doc, m.Value) || !strings.Contains(doc, m.Label) {
			t.Fatalf("清单文案缺 %s：%s", m.Value, doc)
		}
		if chatModeByValue(m.Value) == nil {
			t.Fatalf("按值取不到模式 %s", m.Value)
		}
	}
	if chatModeByValue("task") != nil {
		t.Fatal("task 是内部通用模式，不该进「人工新建会话」清单")
	}
}

// mode 参数：不传＝通用工作任务（task，保持改动前的默认）；传入必须是清单里的
// 值；清单外的值与内部模式（assistant/aux/task）一律拒绝并列出可选值。
func TestResolveDispatchMode(t *testing.T) {
	if got, err := resolveDispatchMode(""); err != nil || got != "task" {
		t.Fatalf("不传 mode 应默认 task, got %q err=%v", got, err)
	}
	for _, m := range chatModes {
		if got, err := resolveDispatchMode(" " + m.Value + " "); err != nil || got != m.Value {
			t.Fatalf("mode=%s 应通过, got %q err=%v", m.Value, got, err)
		}
	}
	for _, bad := range []string{"task", "assistant", "aux", "Office", "coding2"} {
		_, err := resolveDispatchMode(bad)
		if err == nil {
			t.Fatalf("mode=%q 应被拒绝", bad)
		}
		if !strings.Contains(err.Error(), chatModesDoc()) {
			t.Fatalf("拒绝报错应列出可用清单, got %v", err)
		}
	}
}

// 权限拧钮：只能同等或降权，绝不允许增权；超出即拒绝（不静默钳制）。
// 不传＝修改工作区，但本会话权限更窄时取本会话权限（只降不升）。
func TestDispatchPermissionNeverEscalates(t *testing.T) {
	cases := []struct {
		self, req   string
		want        string
		wantErr     bool
		errMustHave string
	}{
		{permWorkspace, "", permWorkspace, false, ""},
		{permWorkspace, permWorkspace, permWorkspace, false, ""},
		{permWorkspace, permReadOnly, permReadOnly, false, ""},
		{permWorkspace, permFull, "", true, "增权"},
		{permWorkspace, "danger-full-access", "", true, "增权"},
		{permWorkspace, "write", "", true, "不是有效档位"},
		{permReadOnly, "", permReadOnly, false, ""},
		{permReadOnly, permReadOnly, permReadOnly, false, ""},
		{permReadOnly, permWorkspace, "", true, "增权"},
		{permReadOnly, permFull, "", true, "增权"},
		{permFull, "", permWorkspace, false, ""}, // 默认不跟着升到完全访问
		{permFull, permWorkspace, permWorkspace, false, ""},
		{permFull, permReadOnly, permReadOnly, false, ""},
		{permFull, permFull, permFull, false, ""}, // 同等允许
	}
	for _, tc := range cases {
		a, main := newDispatchTestApp(t)
		main.Permission = tc.self
		got, err := dispatchCtx(a, main).dispatchPermission(tc.req)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("自身 %s 请求 %q 应被拒绝，却通过了（得到 %s）", tc.self, tc.req, got)
			}
			if !strings.Contains(err.Error(), tc.errMustHave) {
				t.Fatalf("自身 %s 请求 %q 的报错不对：%v", tc.self, tc.req, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("自身 %s 请求 %q 应通过：%v", tc.self, tc.req, err)
		}
		if got != tc.want {
			t.Fatalf("自身 %s 请求 %q：期望 %s，得到 %s", tc.self, tc.req, tc.want, got)
		}
	}
}

// dispatch_task 四个新参数真的落到派发出来的任务上：模式、计划模式、权限档、
// 附件（文件进工作区 + 上传登记 + 【文件上传】注记）。
func TestRunDispatchTaskAppliesNewParams(t *testing.T) {
	a, main := newDispatchTestApp(t)
	// 现成文件（模拟用户已经在本机有这份资料）
	src := filepath.Join(t.TempDir(), "需求.txt")
	if err := os.WriteFile(src, []byte("需求正文"), 0644); err != nil {
		t.Fatal(err)
	}
	out, err := runDispatchTask(dispatchCtx(a, main), mustArgs(t, map[string]any{
		"task":        "把需求整理成一份文档",
		"title":       "整理需求",
		"mode":        "office",
		"plan_mode":   true,
		"permission":  "read-only",
		"attachments": []string{src},
	}))
	if err != nil {
		t.Fatalf("派发失败: %v", err)
	}
	if !strings.Contains(out, "已派发后台执行") {
		t.Fatalf("派发结果文案不对：%s", out)
	}
	subs := otherSessions(t, a)
	if len(subs) != 1 {
		t.Fatalf("应恰好派发出 1 个任务会话, got %d", len(subs))
	}
	sub := subs[0]
	if sub.Mode != "office" {
		t.Fatalf("模式应为 office, got %s", sub.Mode)
	}
	if !sub.PlanMode {
		t.Fatal("plan_mode=true 应让新任务以计划模式启动")
	}
	if permModeOf(sub) != permReadOnly {
		t.Fatalf("权限应为只读, got %s", permModeOf(sub))
	}
	if sub.DispatchedBy != main.ID {
		t.Fatalf("回报链应走 dispatched_by, got %q", sub.DispatchedBy)
	}
	// 附件：登记进 uploads 表 + 复制进新任务工作区
	ups, err := a.Store.UploadsFor(sub.ID)
	if err != nil || len(ups) != 1 {
		t.Fatalf("应有 1 条上传登记, got %d err=%v", len(ups), err)
	}
	if ups[0].Name != "需求.txt" || ups[0].OrigPath != src {
		t.Fatalf("上传登记不对: %+v", ups[0])
	}
	if _, err := os.Stat(ups[0].CopyPath); err != nil {
		t.Fatalf("附件应已复制进工作区：%v", err)
	}
	// 注记拼进任务消息开头的「用户附件：」块（不再落独立注记行）——同一消息
	// 同时带块与任务正文，顺序天然一致
	msgs, err := a.Store.RawMessagesAll(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	taskMsg := ""
	standaloneNote := false
	for _, m := range msgs {
		if m.Kind == kindReport && m.Origin == "upload" {
			standaloneNote = true
		}
		if strings.Contains(m.Text(), "【数字助理派发】") {
			taskMsg = m.Text()
		}
	}
	if standaloneNote {
		t.Fatal("不应再有独立的【文件上传】注记行（已并入用户附件块）")
	}
	if taskMsg == "" {
		t.Fatal("新会话里应有【数字助理派发】正文")
	}
	if !strings.HasPrefix(taskMsg, ATTACH_HEAD) || !strings.Contains(taskMsg, "【文件上传】") {
		t.Fatalf("任务消息开头应是「用户附件：」块且含注记文本: %.80s", taskMsg)
	}
	if !strings.Contains(taskMsg, "【数字助理派发】") {
		t.Fatal("任务消息应含任务正文")
	}
}

// 权限增权请求与坏参数：一律拒绝，且**不留下半个空任务**。
func TestRunDispatchTaskRefusesBeforeCreatingSession(t *testing.T) {
	a, main := newDispatchTestApp(t)
	missing := filepath.Join(t.TempDir(), "不存在.txt")
	cases := []struct {
		name string
		args map[string]any
		want string
	}{
		{"权限增权", map[string]any{"task": "x", "permission": "full"}, "增权"},
		{"权限档非法", map[string]any{"task": "x", "permission": "admin"}, "不是有效档位"},
		{"模式非法", map[string]any{"task": "x", "mode": "assistant"}, "不是可选的工作模式"},
		{"附件路径不存在", map[string]any{"task": "x", "attachments": []string{missing}}, "不可用"},
		{"附件是目录", map[string]any{"task": "x", "attachments": []string{a.Cfg.Workspace}}, "是目录"},
	}
	for _, tc := range cases {
		_, err := runDispatchTask(dispatchCtx(a, main), mustArgs(t, tc.args))
		if err == nil {
			t.Fatalf("%s：应被拒绝", tc.name)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s：报错不含 %q：%v", tc.name, tc.want, err)
		}
		if subs := otherSessions(t, a); len(subs) != 0 {
			t.Fatalf("%s：被拒绝的派发不该建出会话, got %d 个", tc.name, len(subs))
		}
	}
}

// dispatch_sub：permission 落到子对话上；增权请求同样被拒绝且不建会话；
// 专家的模式体系不变（不传 expert ＝ 标准子对话）。
func TestRunDispatchSubPermission(t *testing.T) {
	a, main := newDispatchTestApp(t)
	out, err := runDispatchSub(dispatchCtx(a, main), mustArgs(t, map[string]any{
		"task": "复盘这次改动", "permission": "read-only",
	}))
	if err != nil {
		t.Fatalf("派发子对话失败: %v", err)
	}
	if !strings.Contains(out, "已派发后台执行") {
		t.Fatalf("派发结果文案不对：%s", out)
	}
	subs := otherSessions(t, a)
	if len(subs) != 1 {
		t.Fatalf("应派发 1 个子对话, got %d", len(subs))
	}
	sub := subs[0]
	if sub.ParentID != main.ID || sub.Expert != "标准子对话" {
		t.Fatalf("子对话的挂载与专家预设不该变: parent=%q expert=%q", sub.ParentID, sub.Expert)
	}
	if permModeOf(sub) != permReadOnly {
		t.Fatalf("子对话权限应为只读, got %s", permModeOf(sub))
	}
	if !strings.Contains(out, "只读") {
		t.Fatalf("派发结果应说明权限档：%s", out)
	}
	// 增权：拒绝且不建会话
	if _, err := runDispatchSub(dispatchCtx(a, main), mustArgs(t, map[string]any{
		"task": "越权试试", "permission": "full",
	})); err == nil || !strings.Contains(err.Error(), "增权") {
		t.Fatalf("子对话派发的增权请求应被拒绝, got %v", err)
	}
	if subs := otherSessions(t, a); len(subs) != 1 {
		t.Fatalf("被拒绝的子对话派发不该建会话, got %d 个", len(subs))
	}
}

// 附件参数归一：去空白、去重，保持顺序。
func TestNormalizeDispatchAttachments(t *testing.T) {
	got := normalizeDispatchAttachments([]string{" a.txt ", "", "b.txt", "a.txt", "  "})
	if len(got) != 2 || got[0] != "a.txt" || got[1] != "b.txt" {
		t.Fatalf("归一结果不对: %v", got)
	}
	if normalizeDispatchAttachments(nil) != nil {
		t.Fatal("空输入应返回 nil")
	}
}

// GET /api/modes 下发的就是 chatModes——前端模式下拉与 dispatch_task 的 mode
// 参数同源的那一份（同一数据源）。
func TestChatModesEndpointServesCanonicalList(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	w := httptest.NewRecorder()
	a.handleChatModes(w, httptest.NewRequest("GET", "/api/modes", nil))
	if w.Code != 200 {
		t.Fatalf("状态码应 200, got %d %s", w.Code, w.Body.String())
	}
	var resp struct {
		Modes []chatModeDef `json:"modes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("响应不是预期 JSON: %v (%s)", err, w.Body.String())
	}
	if len(resp.Modes) != len(chatModes) {
		t.Fatalf("模式条数 %d != 清单 %d", len(resp.Modes), len(chatModes))
	}
	for i, m := range resp.Modes {
		if m != chatModes[i] {
			t.Fatalf("第 %d 项与清单不一致: %+v vs %+v", i, m, chatModes[i])
		}
	}
}
