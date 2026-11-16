package main

// 项目事件的行为测试：分层读取、状态/事实规则、幂等与 revision、权限矩阵、
// 来源状态、跨项目隔离、设置页路由与「知识库关闭也能登记/使用」。
// 数据库一律用 t.TempDir 的隔离实例；不碰真实运行库。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// projectEventsTestApp 只安装「项目事件」组件（不装项目知识库），这正是要守的场景：
// 知识库关闭时事件组件照样可用，项目登记走宿主视图/同一套 /api/projects。
func projectEventsTestApp(t *testing.T) (*App, *SessionMeta, Project) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	root := t.TempDir()
	src, err := os.ReadFile(filepath.Join("组件", projectEventsComponentID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, projectEventsComponentID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), src, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "client.js"), []byte("// events"), 0644); err != nil {
		t.Fatal(err)
	}
	a.Components = InitComponents(root, filepath.Join(t.TempDir(), "components.json"))
	if err := a.Components.SetEnabled(projectEventsComponentID, true); err != nil {
		t.Fatal(err)
	}
	a.SessionCX = loadSessionCX(a.Store)
	ws := t.TempDir()
	a.Cfg.Workspace = ws
	binding, err := normalizeProjectWorkspace("local", ws)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Store.CreateProject(Project{Name: "事件项目", Workspaces: []ProjectWorkspace{binding}})
	if err != nil {
		t.Fatal(err)
	}
	s := &SessionMeta{ID: "pe-task", Mode: "task", Workspace: ws, Permission: permWorkspace}
	if err := a.Store.Create(s); err != nil {
		t.Fatal(err)
	}
	return a, s, p
}

func seedEventMessages(t *testing.T, a *App, sid string, n int) {
	t.Helper()
	for i := 1; i <= n; i++ {
		if err := a.Store.AppendRow(sid, &StoredMessage{
			Message:   TextMessage("user", "第"+string(rune('0'+i))+"条消息"),
			Kind:      kindChat,
			CreatedAt: "2026-11-16 10:00:0" + string(rune('0'+i)),
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func callEventCap(t *testing.T, a *App, s *SessionMeta, cap string, args any) (map[string]any, error) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"component": projectEventsComponentID, "capability": cap, "args": args})
	out, err := runComponentCall(&toolContext{app: a, sess: s, ctx: context.Background()}, raw)
	if err != nil {
		return nil, err
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("回执不是 JSON：%v\n%s", err, out)
	}
	return m, nil
}

func mustEventCap(t *testing.T, a *App, s *SessionMeta, cap string, args any) map[string]any {
	t.Helper()
	m, err := callEventCap(t, a, s, cap, args)
	if err != nil {
		t.Fatalf("%s 失败: %v", cap, err)
	}
	return m
}

func createEvent(t *testing.T, a *App, s *SessionMeta, args map[string]any) ProjectEvent {
	t.Helper()
	m := mustEventCap(t, a, s, "project_event_create", args)
	raw, _ := json.Marshal(m["event"])
	var ev ProjectEvent
	if err := json.Unmarshal(raw, &ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

func TestProjectEventLayeredReadCreateAndBudgets(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 3)

	ev := createEvent(t, a, s, map[string]any{
		"project_id": p.ID,
		"title":      "让项目事件可用",
		"summary":    "已完成存取层，时间线与来源待验证。",
		"status":     "discussing",
		"record": map[string]any{
			"kind":    "milestone",
			"text":    "项目事件表结构与存取层已落地，等待端到端验证。",
			"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 2}},
		},
	})
	if ev.ID == "" || ev.Status != eventStatusDiscussing || ev.Revision != 1 || ev.RecordCount != 1 || ev.SessionCount != 1 {
		t.Fatalf("新建回执不符: %+v", ev)
	}

	// L1 总览：只有计数与概况预览，没有时间线细节。
	over := mustEventCap(t, a, s, "project_events_overview", map[string]any{})
	projects, _ := over["projects"].([]any)
	if len(projects) != 1 {
		t.Fatalf("总览应只含自身项目: %v", over)
	}
	group := projects[0].(map[string]any)
	counts := group["counts"].(map[string]any)
	if int(counts["active"].(float64)) != 1 || int(counts["discussing"].(float64)) != 1 {
		t.Fatalf("计数不符: %v", counts)
	}
	preview := group["events"].([]any)[0].(map[string]any)
	if _, has := preview["records"]; has {
		t.Fatal("总览不应下发时间线")
	}

	// L2 时间线：单条记录 + 来源入口。
	recs := mustEventCap(t, a, s, "project_event_records_query", map[string]any{"event_id": ev.ID})
	if int(recs["total"].(float64)) != 1 {
		t.Fatalf("时间线总数不符: %v", recs)
	}
	item := recs["records"].([]any)[0].(map[string]any)
	rec := item["record"].(map[string]any)
	if rec["kind"] != "milestone" || !strings.Contains(rec["text"].(string), "结构") {
		t.Fatalf("记录内容不符: %v", rec)
	}
	if int(item["sources_total"].(float64)) != 1 {
		t.Fatalf("来源数不符: %v", item)
	}

	// L3 来源：元数据 + 未复核状态，且不返回正文。
	src := mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": rec["record_id"].(string)})
	sources := src["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("来源条数不符: %v", src)
	}
	one := sources[0].(map[string]any)
	if one["state"] != "unreviewed" || one["message_seq"].(float64) != 2 {
		t.Fatalf("来源状态不符: %v", one)
	}
	if _, has := one["content"]; has {
		t.Fatal("来源不应携带消息正文")
	}

	// 预算：各字段独立，超限指出已用/上限，不静默截断。
	longTitle := strings.Repeat("目", 41) // 41 汉字 = 82 单元 > 80
	_, err := callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": longTitle, "summary": "一句话概况。",
	})
	if err == nil || !strings.Contains(err.Error(), "标题超出预算") || !strings.Contains(err.Error(), "/ 80") {
		t.Fatalf("标题超预算回执不符: %v", err)
	}
	_, err = callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": "短标题", "summary": strings.Repeat("况", 121), // 242 单元 > 240
	})
	if err == nil || !strings.Contains(err.Error(), "概况超出预算") {
		t.Fatalf("概况超预算回执不符: %v", err)
	}
}

func TestProjectEventStatusRequiresFactOnlyOnRealChange(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	// 直接建成非讨论中：必须同事务附事实。
	_, err := callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": "直接处理中", "summary": "没有事实的建立。", "status": "processing",
	})
	if err == nil || !strings.Contains(err.Error(), "必须同时给出 record") {
		t.Fatalf("非讨论中直接创建应要求事实: %v", err)
	}
	ev := createEvent(t, a, s, map[string]any{
		"project_id": p.ID, "title": "带事实的处理中", "summary": "已有事实支撑。", "status": "processing",
		"record": map[string]any{"kind": "plan", "text": "方案已确认，进入实现。", "sources": []any{}},
	})

	// 仅改标题/概况：当前状态非讨论中也不强制补事实（本轮收敛的口径）。
	out := mustEventCap(t, a, s, "project_event_update", map[string]any{
		"event_id": ev.ID, "expected_revision": ev.Revision, "title": "带事实的处理中（改名）",
	})
	if changed, _ := out["changed"].(bool); !changed {
		t.Fatalf("改名应算变化: %v", out)
	}
	ev2raw, _ := json.Marshal(out["event"])
	var ev2 ProjectEvent
	_ = json.Unmarshal(ev2raw, &ev2)
	if ev2.Title != "带事实的处理中（改名）" || ev2.Status != eventStatusProcessing || ev2.RecordCount != 1 {
		t.Fatalf("仅改标题不应新增记录: %+v", ev2)
	}

	// 状态实际变化：必须附事实。
	_, err = callEventCap(t, a, s, "project_event_update", map[string]any{
		"event_id": ev.ID, "expected_revision": ev2.Revision, "status": "awaiting",
	})
	if err == nil || !strings.Contains(err.Error(), "必须同时提交一条有依据的关键事实") {
		t.Fatalf("状态变化未要求事实: %v", err)
	}
	out = mustEventCap(t, a, s, "project_event_update", map[string]any{
		"event_id": ev.ID, "expected_revision": ev2.Revision, "status": "awaiting",
		"record": map[string]any{"kind": "acceptance", "text": "实现完成，提交待验收。", "sources": []any{}},
	})
	if changed, _ := out["changed"].(bool); !changed {
		t.Fatalf("状态变化应算变化: %v", out)
	}

	// 相同值保存不刷新 revision。
	out = mustEventCap(t, a, s, "project_event_update", map[string]any{
		"event_id": ev.ID, "expected_revision": 3, "title": "带事实的处理中（改名）",
	})
	if changed, _ := out["changed"].(bool); changed {
		t.Fatalf("相同值保存不应算变化: %v", out)
	}
}

func TestProjectEventIdempotencyRevisionAndSingleLine(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	args := map[string]any{
		"project_id": p.ID, "title": "幂等目标", "summary": "同键重试不应建两个。",
		"create_key": "k-1",
	}
	first := createEvent(t, a, s, args)
	m := mustEventCap(t, a, s, "project_event_create", args)
	if created, _ := m["created"].(bool); created {
		t.Fatal("同 create_key 重试不应新建")
	}
	raw, _ := json.Marshal(m["event"])
	var again ProjectEvent
	_ = json.Unmarshal(raw, &again)
	if again.ID != first.ID {
		t.Fatalf("幂等应返回同一事件: %s vs %s", first.ID, again.ID)
	}
	// 同键不同内容 → 冲突。
	_, err := callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": "换了标题", "summary": "同键不同内容。", "create_key": "k-1",
	})
	if err == nil || !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("同键不同内容应冲突: %v", err)
	}

	// 记录幂等：同 client_key 同正文不重复追加，revision 不动。
	add := map[string]any{
		"event_id": first.ID, "expected_event_revision": 1, "client_key": "r-1",
		"kind": "milestone", "text": "第一条可重试的事实。", "sources": []any{},
	}
	out := mustEventCap(t, a, s, "project_event_record_add", add)
	evRaw, _ := json.Marshal(out["event"])
	var ev ProjectEvent
	_ = json.Unmarshal(evRaw, &ev)
	if ev.Revision != 2 || ev.RecordCount != 1 {
		t.Fatalf("首次追加后 revision/记录数不符: %+v", ev)
	}
	add2 := map[string]any{
		"event_id": first.ID, "expected_event_revision": 2, "client_key": "r-1",
		"kind": "milestone", "text": "第一条可重试的事实。", "sources": []any{},
	}
	out = mustEventCap(t, a, s, "project_event_record_add", add2)
	if changed, _ := out["changed"].(bool); changed {
		t.Fatalf("幂等重试不应算变化: %v", out)
	}

	// 旧 revision 保存 → 冲突。
	_, err = callEventCap(t, a, s, "project_event_update", map[string]any{
		"event_id": first.ID, "expected_revision": 1, "title": "过期 revision",
	})
	if err == nil || !strings.Contains(err.Error(), "版本冲突") {
		t.Fatalf("旧 revision 应冲突: %v", err)
	}

	// 多句/分号/列表一律拒写。
	multi := map[string]any{"event_id": first.ID, "expected_event_revision": 2, "client_key": "r-x", "kind": "issue", "text": "已修复。等待验收。"}
	if _, err := callEventCap(t, a, s, "project_event_record_add", multi); err == nil || !strings.Contains(err.Error(), "一句话") {
		t.Fatalf("多句应拒写: %v", err)
	}
	semi := map[string]any{"event_id": first.ID, "expected_event_revision": 2, "client_key": "r-y", "kind": "issue", "text": "改完代码；跑了测试"}
	if _, err := callEventCap(t, a, s, "project_event_record_add", semi); err == nil || !strings.Contains(err.Error(), "分号") {
		t.Fatalf("分号应拒写: %v", err)
	}
	listy := map[string]any{"event_id": first.ID, "expected_event_revision": 2, "client_key": "r-z", "kind": "issue", "text": "- 第一项"}
	if _, err := callEventCap(t, a, s, "project_event_record_add", listy); err == nil || !strings.Contains(err.Error(), "列表") {
		t.Fatalf("列表项应拒写: %v", err)
	}
}

func TestProjectEventWritePermissionMatrix(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 1)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "权限目标", "summary": "切入各种门。"})
	writeArgs := map[string]any{"project_id": p.ID, "title": "试图写入", "summary": "应被拒。"}

	// 只读答疑 aux：可读不可写。
	aux := &SessionMeta{ID: "pe-aux", Mode: "aux", Workspace: s.Workspace, Permission: permWorkspace}
	if err := a.Store.Create(aux); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, aux, "project_events_overview", map[string]any{}); err != nil {
		t.Fatalf("aux 应能读: %v", err)
	}
	if _, err := callEventCap(t, a, aux, "project_event_create", writeArgs); err == nil {
		t.Fatal("aux 不应能写")
	}

	// 只读档。改档要同步改传给工具的内存副本（t.sess）与库内权威行。
	s.Permission = permReadOnly
	if err := a.Store.SaveMeta(s); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, s, "project_event_create", writeArgs); err == nil || !strings.Contains(err.Error(), "只读") {
		t.Fatalf("只读档应被拒: %v", err)
	}

	// 计划模式。
	s.Permission = permWorkspace
	s.PlanMode = true
	if err := a.Store.SaveMeta(s); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, s, "project_event_create", writeArgs); err == nil || !strings.Contains(err.Error(), "计划模式") {
		t.Fatalf("计划模式应被拒: %v", err)
	}

	// 恢复正常后可用；组件关闭则读写都拒。
	s.PlanMode = false
	if err := a.Store.SaveMeta(s); err != nil {
		t.Fatal(err)
	}
	if err := a.Components.SetEnabled(projectEventsComponentID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, s, "project_events_overview", map[string]any{}); err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("组件关闭应拒读: %v", err)
	}
	if err := a.Components.SetEnabled(projectEventsComponentID, true); err != nil {
		t.Fatal(err)
	}

	// 归档项目只读。
	latest, err := a.Store.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := a.Store.PatchProject(p.ID, nil, &archived, nil, &latest.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, s, "project_event_create", writeArgs); err == nil || !strings.Contains(err.Error(), "已归档") {
		t.Fatalf("归档项目应拒写: %v", err)
	}
	if _, err := callEventCap(t, a, s, "project_event_get", map[string]any{"event_id": ev.ID}); err != nil {
		t.Fatalf("归档项目应可读: %v", err)
	}
}

// TestProjectEventWriteRechecksOwnershipInTransaction 守着两件事：①会话工作区迁移后
// 不能继续写原项目；②真正落库时授权复核用的是事务内重读的权威会话，失败必须整笔回滚。
func TestProjectEventWriteRechecksOwnershipInTransaction(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	otherWs := t.TempDir()
	binding, err := normalizeProjectWorkspace("local", otherWs)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.CreateProject(Project{Name: "另一个项目", Workspaces: []ProjectWorkspace{binding}}); err != nil {
		t.Fatal(err)
	}
	// 会话迁到另一个项目的工作区：内存里的旧副本还指着原项目，库里已是新归属。
	s.Workspace = otherWs
	if err := a.Store.SaveMeta(s); err != nil {
		t.Fatal(err)
	}
	_, err = callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": "越界写入", "summary": "应被拒。",
	})
	if err == nil || !strings.Contains(err.Error(), "绑定") {
		t.Fatalf("工作区已迁移时不应写入原项目: %v", err)
	}

	// 直接把一笔写入交给存取层：授权回调必须拿到库里当前会话（不是调用前副本），
	// 且拒绝时什么都不落库。
	seen := ""
	_, _, err = a.Store.CreateProjectEventForSession(p.ID, ProjectEventCreateInput{
		Title: "事务内复核", Summary: "授权失败应整笔回滚。", Status: eventStatusDiscussing,
	}, s.ID, func(q projectReader, pr Project, sess *SessionMeta) error {
		seen = sess.Workspace
		return ErrProjectEventSourceInvalid
	})
	if err == nil || !errors.Is(err, ErrProjectEventSourceInvalid) {
		t.Fatalf("授权拒绝应透出: %v", err)
	}
	if seen != otherWs {
		t.Fatalf("授权复核应拿到事务内重读的会话，拿到的是 %q", seen)
	}
	events, _, _, err := a.Store.ProjectEventPage(p.ID, ProjectEventFilter{Scope: projectEventScopeAll}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.Title == "事务内复核" {
			t.Fatal("授权拒绝后不应留下半笔事件，事务需整笔回滚")
		}
	}
}

func TestProjectEventScopeIsolationAndSourceQualification(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 2)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "范围目标", "summary": "看谁能读。"})

	// 另一个项目的会话：不能按 project_id 越权，也不能把别的项目的会话当来源。
	ws2 := t.TempDir()
	binding, err := normalizeProjectWorkspace("local", ws2)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := a.Store.CreateProject(Project{Name: "别的项目", Workspaces: []ProjectWorkspace{binding}})
	if err != nil {
		t.Fatal(err)
	}
	outsider := &SessionMeta{ID: "pe-out", Mode: "coding", Workspace: ws2, Permission: permWorkspace}
	if err := a.Store.Create(outsider); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, outsider, "project_events_query", map[string]any{"project_id": p.ID}); err == nil || !strings.Contains(err.Error(), "自身实际工作区绑定") {
		t.Fatalf("跨项目查询应被拒: %v", err)
	}
	// 自身项目：不传 project_id 也能读。
	over := mustEventCap(t, a, outsider, "project_events_overview", map[string]any{})
	groups := over["projects"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["project_id"] != p2.ID {
		t.Fatalf("普通会话应只看到自身项目: %v", over)
	}
	if _, err := callEventCap(t, a, outsider, "project_event_get", map[string]any{"event_id": ev.ID}); err == nil || !strings.Contains(err.Error(), "跨项目") {
		t.Fatalf("跨项目按 event_id 读应被拒: %v", err)
	}

	// 长期助理：跨项目可读，且能用 project_id 定向。
	assist := &SessionMeta{ID: "pe-assist", Mode: "assistant", Workspace: s.Workspace, Permission: permFull}
	if err := a.Store.Create(assist); err != nil {
		t.Fatal(err)
	}
	if _, err := callEventCap(t, a, assist, "project_event_get", map[string]any{"event_id": ev.ID}); err != nil {
		t.Fatalf("长期助理应能跨项目读: %v", err)
	}
	if _, err := callEventCap(t, a, assist, "project_event_get", map[string]any{}); err == nil || !strings.Contains(err.Error(), "project_events_overview") {
		t.Fatalf("长期助理缺 event_id 时应指路: %v", err)
	}

	// 来源资格：别的项目的会话不能绑进本项目（非助理）。先在本项目建一个目标，
	// 再拿项目 1 的会话当来源——应当被资格检查拦住。
	evOut := createEvent(t, a, outsider, map[string]any{"project_id": p2.ID, "title": "本项目的目标", "summary": "只能绑本项目来源。"})
	_, err = callEventCap(t, a, outsider, "project_event_record_add", map[string]any{
		"event_id": evOut.ID, "expected_event_revision": evOut.Revision, "client_key": "o-1",
		"kind": "issue", "text": "越界来源。", "sources": []any{map[string]any{"session_id": s.ID}},
	})
	if err == nil || !strings.Contains(err.Error(), "资格") {
		t.Fatalf("跨项目来源应被拒: %v", err)
	}
	// 不存在的 seq 不能当来源。
	_, err = callEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "s-9",
		"kind": "issue", "text": "无效位置。", "sources": []any{map[string]any{"session_id": s.ID, "message_seq": 99}},
	})
	if err == nil || !strings.Contains(err.Error(), "没有 seq=99") {
		t.Fatalf("不存在的消息位置应被拒: %v", err)
	}
	// 助理可绑定同项目会话与自身。
	if _, err := callEventCap(t, a, assist, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "a-1",
		"kind": "milestone", "text": "长期助理确认了跨项目协调。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}, map[string]any{"session_id": assist.ID}},
	}); err != nil {
		t.Fatalf("助理绑定同项目与自身来源应允许: %v", err)
	}
}

func TestProjectEventSourceStatesAndFingerprint(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 4)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "来源状态", "summary": "看失效与复核。"})
	out := mustEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "src-1",
		"kind": "validation", "text": "四条消息里第二条是依据。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 2}},
	})
	recRaw, _ := json.Marshal(out["record"])
	var rec ProjectEventRecord
	_ = json.Unmarshal(recRaw, &rec)

	list := mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": rec.ID})
	one := list["sources"].([]any)[0].(map[string]any)
	if one["state"] != "unreviewed" {
		t.Fatalf("未指定 source_id 时应为未复核: %v", one)
	}
	// 定点复核：同一行同一摘要 → verified_unchanged。
	list = mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": rec.ID, "source_id": one["source_id"]})
	if got := list["sources"].([]any)[0].(map[string]any)["state"]; got != "verified_unchanged" {
		t.Fatalf("复核应一致: %v", got)
	}
	// 正文被改（行 ID 不变）→ message_changed。
	if _, err := a.Store.EditUserMessage(s.ID, 2, "改写后的正文"); err != nil {
		t.Fatal(err)
	}
	list = mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": rec.ID, "source_id": one["source_id"]})
	if got := list["sources"].([]any)[0].(map[string]any)["state"]; got != "message_changed" {
		t.Fatalf("正文改动应识别为 changed: %v", got)
	}

	// seq 被复用（截断后重发）→ message_replaced。
	// 上一步 EditUserMessage 已截掉 seq>2，先补回两条，再拿 seq 4 当依据。
	seedEventMessages(t, a, s.ID, 2)
	rec2 := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "复用位置", "summary": "看 seq 复用。"})
	out = mustEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": rec2.ID, "expected_event_revision": rec2.Revision, "client_key": "src-2",
		"kind": "issue", "text": "第四条消息是依据。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 4}},
	})
	recRaw2, _ := json.Marshal(out["record"])
	var recB ProjectEventRecord
	_ = json.Unmarshal(recRaw2, &recB)
	if _, err := a.Store.EditUserMessage(s.ID, 3, "第三条被改写"); err != nil {
		t.Fatal(err)
	}
	// 截断后追加新消息，重新占用 seq 4。
	seedEventMessages(t, a, s.ID, 1)
	list = mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": recB.ID})
	if got := list["sources"].([]any)[0].(map[string]any)["state"]; got != "message_replaced" {
		t.Fatalf("seq 复用应识别为 replaced: %v", got)
	}

	// 来源会话被删 → session_missing，但记录还在。
	other := &SessionMeta{ID: "pe-gone", Mode: "task", Workspace: s.Workspace, Permission: permWorkspace}
	if err := a.Store.Create(other); err != nil {
		t.Fatal(err)
	}
	ev3 := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "会话删除", "summary": "事实保留。"})
	out = mustEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev3.ID, "expected_event_revision": ev3.Revision, "client_key": "src-3",
		"kind": "issue", "text": "依据来自另一个会话。",
		"sources": []any{map[string]any{"session_id": other.ID}},
	})
	recRaw3, _ := json.Marshal(out["record"])
	var recC ProjectEventRecord
	_ = json.Unmarshal(recRaw3, &recC)
	if err := a.Store.Delete(other.ID); err != nil {
		t.Fatal(err)
	}
	list = mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": recC.ID})
	if got := list["sources"].([]any)[0].(map[string]any)["state"]; got != "session_missing" {
		t.Fatalf("会话删除应识别为 missing: %v", got)
	}
	if _, err := callEventCap(t, a, s, "project_event_records_query", map[string]any{"event_id": ev3.ID}); err != nil {
		t.Fatalf("来源失效不应影响事实可读: %v", err)
	}
}

func TestProjectEventOverviewIndependenceAndCursorBinding(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	// 项目 1 放 5 个目标；项目 2 放 1 个——项目 1 不许挤掉项目 2。
	for i := 0; i < 5; i++ {
		createEvent(t, a, s, map[string]any{
			"project_id": p.ID,
			"title":      "目标" + string(rune('A'+i)),
			"summary":    "多目标项目的一条概况。",
		})
	}
	ws2 := t.TempDir()
	binding, err := normalizeProjectWorkspace("local", ws2)
	if err != nil {
		t.Fatal(err)
	}
	p2, err := a.Store.CreateProject(Project{Name: "小项目", Workspaces: []ProjectWorkspace{binding}})
	if err != nil {
		t.Fatal(err)
	}
	other := &SessionMeta{ID: "pe-small", Mode: "task", Workspace: ws2, Permission: permWorkspace}
	if err := a.Store.Create(other); err != nil {
		t.Fatal(err)
	}
	createEvent(t, a, other, map[string]any{"project_id": p2.ID, "title": "唯一目标", "summary": "小项目只有这一条。"})

	assist := &SessionMeta{ID: "pe-assist2", Mode: "assistant", Workspace: s.Workspace, Permission: permFull}
	if err := a.Store.Create(assist); err != nil {
		t.Fatal(err)
	}
	over := mustEventCap(t, a, assist, "project_events_overview", map[string]any{})
	groups := over["projects"].([]any)
	if len(groups) != 2 {
		t.Fatalf("两个项目都应出现在总览: %v", over)
	}
	for _, g := range groups {
		group := g.(map[string]any)
		if group["project_id"] == p.ID {
			if int(group["events_shown"].(float64)) != 3 || int(group["events_remaining"].(float64)) != 2 {
				t.Fatalf("大项目预览应封顶 3 条并报剩余: %v", group)
			}
		} else if int(group["events_remaining"].(float64)) != 0 {
			t.Fatalf("小项目不应被挤掉: %v", group)
		}
	}

	// 游标与筛选绑定：换筛选复用旧游标必须拒绝。
	page := mustEventCap(t, a, assist, "project_events_query", map[string]any{"project_id": p.ID, "limit": 2})
	cursor, _ := page["next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("应有下一页游标: %v", page)
	}
	_, err = callEventCap(t, a, assist, "project_events_query", map[string]any{"project_id": p.ID, "limit": 2, "keyword": "目标", "cursor": cursor})
	if err == nil || !strings.Contains(err.Error(), "游标") {
		t.Fatalf("换筛选复用游标应拒绝: %v", err)
	}
	// 原筛选继续翻页可用。
	if _, err := callEventCap(t, a, assist, "project_events_query", map[string]any{"project_id": p.ID, "limit": 2, "cursor": cursor}); err != nil {
		t.Fatalf("原筛选翻页应可用: %v", err)
	}
}

func eventAPIRequest(a *App, method, url string, body any, ai bool) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, url, bytes.NewReader(b))
	if ai {
		r.Header.Set("X-JX-Caller-Session", "pe-task")
	}
	mux := http.NewServeMux()
	a.registerProjectEventRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}

func TestProjectEventSettingsAPIAndUserWrites(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 2)

	// AI 带调用者头：一律 403（AI 只能走 component_call）。
	if w := eventAPIRequest(a, "GET", "/api/project-events/overview", nil, true); w.Code != http.StatusForbidden {
		t.Fatalf("AI 不应能调设置页接口: %d %s", w.Code, w.Body.String())
	}

	w := eventAPIRequest(a, "POST", "/api/project-events", map[string]any{
		"project_id": p.ID, "title": "设置页建的目标", "summary": "用户在本页登记。",
	}, false)
	if w.Code != 200 {
		t.Fatalf("用户新建失败: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event ProjectEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	ev := created.Event
	if ev.Status != eventStatusDiscussing {
		t.Fatalf("缺省状态应为讨论中: %+v", ev)
	}

	// 非讨论中直接创建也要事实；用户从本页补一条即可。
	w = eventAPIRequest(a, "POST", "/api/project-events", map[string]any{
		"project_id": p.ID, "title": "用户直接处理中", "summary": "带事实直接建。", "status": "processing",
		"record": map[string]any{"kind": "plan", "text": "用户确认方案后直接记为处理中。", "sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}}},
	}, false)
	if w.Code != 200 {
		t.Fatalf("用户带事实直接建非讨论中失败: %d %s", w.Code, w.Body.String())
	}

	// 追加记录（用户 origin）、改概况、读回。
	w = eventAPIRequest(a, "POST", "/api/project-events/"+ev.ID+"/records", map[string]any{
		"expected_event_revision": ev.Revision,
		"record":                  map[string]any{"kind": "validation", "text": "用户在设置页补了一条验证结论。", "sources": []any{map[string]any{"session_id": s.ID}}},
	}, false)
	if w.Code != 200 {
		t.Fatalf("用户追加记录失败: %d %s", w.Code, w.Body.String())
	}
	var added struct {
		Event  ProjectEvent       `json:"event"`
		Record ProjectEventRecord `json:"record"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	if added.Record.Origin != eventOriginUser || added.Event.RecordCount != 1 {
		t.Fatalf("用户录入应标 user: %+v", added)
	}
	w = eventAPIRequest(a, "PATCH", "/api/project-events/"+ev.ID, map[string]any{
		"expected_revision": added.Event.Revision, "summary": "用户改了概况，不强制补事实。",
	}, false)
	if w.Code != 200 {
		t.Fatalf("用户改概况失败: %d %s", w.Code, w.Body.String())
	}
	w = eventAPIRequest(a, "PATCH", "/api/project-events/"+ev.ID, map[string]any{
		"expected_revision": 1, "summary": "过期 revision",
	}, false)
	if w.Code != http.StatusConflict {
		t.Fatalf("旧 revision 应 409: %d %s", w.Code, w.Body.String())
	}

	// 列表 / 详情 / 时间线 / 来源都可读。
	for _, url := range []string{
		"/api/project-events/overview",
		"/api/project-events?project_id=" + p.ID,
		"/api/project-events/" + ev.ID,
		"/api/project-events/" + ev.ID + "/records",
		"/api/project-events/records/" + added.Record.ID + "/sources",
	} {
		if w := eventAPIRequest(a, "GET", url, nil, false); w.Code != 200 {
			t.Fatalf("GET %s 失败: %d %s", url, w.Code, w.Body.String())
		}
	}

	// 设置页也守「实际状态变化必须附一条关键事实」；仅改概况不强制。
	// 先读回当前 revision（上面刚有一条被 409 拒的请求，不能拿它的响应体当基线）。
	w = eventAPIRequest(a, "GET", "/api/project-events/"+ev.ID, nil, false)
	if w.Code != 200 {
		t.Fatalf("读回目标失败: %d %s", w.Code, w.Body.String())
	}
	var summarySaved struct {
		Event ProjectEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &summarySaved); err != nil {
		t.Fatal(err)
	}
	w = eventAPIRequest(a, "PATCH", "/api/project-events/"+ev.ID, map[string]any{
		"expected_revision": summarySaved.Event.Revision, "status": "awaiting",
	}, false)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("用户改状态不附事实应被拒: %d %s", w.Code, w.Body.String())
	}
	w = eventAPIRequest(a, "PATCH", "/api/project-events/"+ev.ID, map[string]any{
		"expected_revision": summarySaved.Event.Revision, "status": "awaiting",
		"record": map[string]any{"kind": "acceptance", "text": "设置页补的事实，随状态一起提交。", "sources": []any{}},
	}, false)
	if w.Code != 200 {
		t.Fatalf("用户改状态并附事实应允许: %d %s", w.Code, w.Body.String())
	}

	// 删除必须显式确认。
	if w := eventAPIRequest(a, "DELETE", "/api/project-events/"+ev.ID, map[string]any{"confirm": false}, false); w.Code == 200 {
		t.Fatalf("未确认不应删除: %s", w.Body.String())
	}
	if w := eventAPIRequest(a, "DELETE", "/api/project-events/records/"+added.Record.ID, map[string]any{"confirm": true}, false); w.Code != 200 {
		t.Fatalf("确认删除记录失败: %d %s", w.Code, w.Body.String())
	}
	if w := eventAPIRequest(a, "DELETE", "/api/project-events/"+ev.ID, map[string]any{"confirm": true}, false); w.Code != 200 {
		t.Fatalf("确认删除目标失败: %d %s", w.Code, w.Body.String())
	}
	if _, err := a.Store.GetProjectEvent(ev.ID); err == nil {
		t.Fatal("删除后应读不到目标")
	}

	// 组件关闭：设置页接口也拒。
	if err := a.Components.SetEnabled(projectEventsComponentID, false); err != nil {
		t.Fatal(err)
	}
	if w := eventAPIRequest(a, "GET", "/api/project-events/overview", nil, false); w.Code != http.StatusForbidden {
		t.Fatalf("组件关闭应拒: %d %s", w.Code, w.Body.String())
	}
}

// TestProjectEventWorksWithoutKnowledgeComponent 守本轮修正的第二点：知识库关闭时，
// 事件页仍能读事件，并用同一套宿主登记服务（/api/projects）登记项目。
func TestProjectEventWorksWithoutKnowledgeComponent(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	if a.Components.IsEnabled(projectKnowledgeComponentID) {
		t.Fatal("本夹具不应启用项目知识库组件")
	}
	if _, err := callEventCap(t, a, s, "project_events_overview", map[string]any{}); err != nil {
		t.Fatalf("知识库关闭时事件总览应可用: %v", err)
	}
	// 登记仍走 /api/projects（同一套服务，不是第二套表）。
	w := projectAPIRequest(a, "POST", "/api/projects", map[string]any{"name": "事件页新登记"}, false)
	if w.Code != 200 {
		t.Fatalf("知识库关闭时也应能登记项目: %d %s", w.Code, w.Body.String())
	}
	var np Project
	if err := json.Unmarshal(w.Body.Bytes(), &np); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	w = projectAPIRequest(a, "PATCH", "/api/projects/"+np.ID, map[string]any{
		"expected_updated_at": np.UpdatedAt,
		"workspaces":          []any{map[string]any{"workspace_kind": "local", "display_path": dir}},
	}, false)
	if w.Code != 200 {
		t.Fatalf("绑定工作区失败: %d %s", w.Code, w.Body.String())
	}
	// 新项目里能建事件。
	s2 := &SessionMeta{ID: "pe-new", Mode: "coding", Workspace: dir, Permission: permWorkspace}
	if err := a.Store.Create(s2); err != nil {
		t.Fatal(err)
	}
	ev := createEvent(t, a, s2, map[string]any{"project_id": np.ID, "title": "新项目首个目标", "summary": "登记后即可用。"})
	if ev.ProjectID != np.ID || ev.ProjectID == p.ID {
		t.Fatalf("事件应落在新登记项目: %+v", ev)
	}
}

// TestProjectEventSettingsWiring 是结构断言：设置页必须挂宿主登记视图（不是第二套登记），
// 且清单声明了全部九条能力、宿主脚本已随页面加载。
func TestProjectEventSettingsWiring(t *testing.T) {
	client, err := os.ReadFile(filepath.Join("组件", projectEventsComponentID, "client.js"))
	if err != nil {
		t.Fatal(err)
	}
	text := string(client)
	for _, want := range []string{"CX.registerSettings", "JX.projectRegistry", "/api/project-events", "项目登记"} {
		if !strings.Contains(text, want) {
			t.Fatalf("项目事件设置页缺少 %q", want)
		}
	}
	if strings.Contains(text, "/api/projects/") {
		t.Fatal("项目事件页不应自己调项目登记写接口：登记走宿主视图，不复制第二套体系")
	}
	registry, err := os.ReadFile(filepath.Join("web", "project-registry.js"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"JX.projectRegistry", "/api/projects", "confirm"} {
		if !strings.Contains(string(registry), want) {
			t.Fatalf("宿主登记视图缺少 %q", want)
		}
	}
	index, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(index), "/project-registry.js") {
		t.Fatal("index.html 未加载宿主登记视图脚本")
	}

	manifestRaw, err := os.ReadFile(filepath.Join("组件", projectEventsComponentID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest ComponentManifest
	if err := json.Unmarshal(manifestRaw, &manifest); err != nil {
		t.Fatalf("manifest 解析失败: %v", err)
	}
	want := []string{
		"project_events_overview", "project_events_query", "project_event_get",
		"project_event_records_query", "project_event_sources_query",
		"project_event_create", "project_event_update",
		"project_event_record_add", "project_event_record_update",
	}
	got := map[string]bool{}
	for _, c := range manifest.Capabilities {
		got[c.Name] = true
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("manifest 缺少能力 %s", name)
		}
		if _, ok := componentCapabilityRun(projectEventsComponentID, name); !ok {
			t.Fatalf("宿主未注册能力实现 %s", name)
		}
	}
}

// TestProjectEventBudgetBoundaries 按设计 §5.4 的样例守边界：恰好上限允许、超一单元
// 拒写；非 CJK 只整字段取整一次；任一字段超限整次拒写且不留半条内容。
func TestProjectEventBudgetBoundaries(t *testing.T) {
	a, s, p := projectEventsTestApp(t)

	// 复用同一预算函数（标题 80 / 概况与记录 240 单元）。
	cases := []struct {
		in   string
		want int64
	}{
		{"汉", 2}, {"汉abcd", 3}, {"汉abcde", 4}, {"abcde", 2}, {"𠀀", 2},
		{"汉あア한", 8},
	}
	for _, tc := range cases {
		if got := ProjectKnowledgeBudget(tc.in); got != tc.want {
			t.Fatalf("预算样例 %q 期望 %d 实得 %d", tc.in, tc.want, got)
		}
	}

	okTitle := strings.Repeat("汉", 40) // 80 单元，恰好上限
	if got := ProjectKnowledgeBudget(okTitle); got != 80 {
		t.Fatalf("40 个汉字应为 80 单元，实得 %d", got)
	}
	if _, err := callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": okTitle, "summary": "恰好上限的标题应允许。",
	}); err != nil {
		t.Fatalf("恰好 80 单元的标题应允许: %v", err)
	}

	// 非 CJK：320 个 a = 80 允许；321 个 a = 81 拒写（不另加字符硬限）。
	if _, err := callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": strings.Repeat("a", 320), "summary": "非 CJK 恰好上限。",
	}); err != nil {
		t.Fatalf("320 个 a 应允许: %v", err)
	}
	_, err := callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": strings.Repeat("a", 321), "summary": "超一个单元。",
	})
	if err == nil || !strings.Contains(err.Error(), "81 / 80") {
		t.Fatalf("321 个 a 应报 81 / 80: %v", err)
	}

	// 概况与记录：960 个 a 允许 / 961 拒写。
	ev := createEvent(t, a, s, map[string]any{
		"project_id": p.ID, "title": "预算边界", "summary": strings.Repeat("a", 960),
	})
	_, err = callEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "b-1",
		"kind": "milestone", "text": strings.Repeat("a", 961),
	})
	if err == nil || !strings.Contains(err.Error(), "240") {
		t.Fatalf("961 个 a 的记录应超限: %v", err)
	}

	// 任一字段超限：整次拒写，目标总数与内容都不变。
	before := mustEventCap(t, a, s, "project_events_query", map[string]any{"project_id": p.ID})
	totalBefore := int(before["total"].(float64))
	_, err = callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": "记录超限的那次提交", "summary": "概况正常。",
		"record": map[string]any{"kind": "issue", "text": strings.Repeat("a", 961)},
	})
	if err == nil {
		t.Fatal("记录超限应整次拒写")
	}
	after := mustEventCap(t, a, s, "project_events_query", map[string]any{"project_id": p.ID})
	if int(after["total"].(float64)) != totalBefore {
		t.Fatalf("拒写不应留下半条事件: %d -> %d", totalBefore, int(after["total"].(float64)))
	}

	// 事件不设时间线累计总预算：多条各自合规的记录不会因累计而拒写。
	for i := 0; i < 3; i++ {
		cur := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "累计预算" + strings.Repeat("汉", i+1), "summary": "看累计。"})
		_ = cur
	}
	big := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "多条记录", "summary": "每条都合规。"})
	rev := big.Revision
	for i := 0; i < 3; i++ {
		out := mustEventCap(t, a, s, "project_event_record_add", map[string]any{
			"event_id": big.ID, "expected_event_revision": rev, "client_key": "many-" + strconv.Itoa(i),
			"kind": "milestone", "text": strings.Repeat("a", 960),
		})
		rawEv, _ := json.Marshal(out["event"])
		var next ProjectEvent
		_ = json.Unmarshal(rawEv, &next)
		rev = next.Revision
	}
	detail := mustEventCap(t, a, s, "project_event_get", map[string]any{"event_id": big.ID})
	evRaw, _ := json.Marshal(detail["event"])
	var final ProjectEvent
	_ = json.Unmarshal(evRaw, &final)
	if final.RecordCount != 3 {
		t.Fatalf("多条合规记录应全部保留: %+v", final)
	}
}

// TestProjectEventDuplicateSourceKeepsCount 守「重复关系不增计数」与来源增删的 revision 语义。
func TestProjectEventDuplicateSourceKeepsCount(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 2)
	ev := createEvent(t, a, s, map[string]any{
		"project_id": p.ID, "title": "重复来源", "summary": "同一关系不重复计数。",
		"record": map[string]any{
			"kind": "milestone", "text": "一条有来源的事实。",
			"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
		},
	})
	recs := mustEventCap(t, a, s, "project_event_records_query", map[string]any{"event_id": ev.ID})
	rec := recs["records"].([]any)[0].(map[string]any)["record"].(map[string]any)
	recID := rec["record_id"].(string)
	recRev := int64(rec["revision"].(float64))

	// 再加一遍同一来源：不增计数、不算变化、不动 revision。
	out := mustEventCap(t, a, s, "project_event_record_update", map[string]any{
		"record_id": recID, "expected_record_revision": recRev, "expected_event_revision": ev.Revision,
		"sources_add": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
	})
	if changed, _ := out["changed"].(bool); changed {
		t.Fatalf("重复来源不应算变化: %v", out)
	}
	recOut, _ := json.Marshal(out["record"])
	var recAfter ProjectEventRecord
	_ = json.Unmarshal(recOut, &recAfter)
	if recAfter.SourceCount != 1 || recAfter.Revision != recRev {
		t.Fatalf("重复来源不应增计数或刷 revision: %+v", recAfter)
	}

	// 换一条消息位置：算新关系，计数与 revision 都增。
	out = mustEventCap(t, a, s, "project_event_record_update", map[string]any{
		"record_id": recID, "expected_record_revision": recRev, "expected_event_revision": ev.Revision,
		"sources_add": []any{map[string]any{"session_id": s.ID, "message_seq": 2}},
	})
	if changed, _ := out["changed"].(bool); !changed {
		t.Fatalf("新来源应算变化: %v", out)
	}
	recOut, _ = json.Marshal(out["record"])
	_ = json.Unmarshal(recOut, &recAfter)
	if recAfter.SourceCount != 2 || recAfter.Revision != recRev+1 {
		t.Fatalf("新来源应增计数与 revision: %+v", recAfter)
	}
}

// TestProjectEventSettingsPaneSmoke 用真组件 + 最小 DOM（goja）跑一遍真实交互：
// 总览 → 新建目标 → 时间线 → 追加事实，并验证「项目登记」用的是宿主视图
// （web/project-registry.js + /api/projects），组件自己不发项目写请求。
func TestProjectEventSettingsPaneSmoke(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	vm := goja.New()
	var paths []string
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("请求轨迹: %v", paths)
		}
	})
	vm.Set("request", func(method, path, body string) map[string]any {
		paths = append(paths, method+" "+path)
		var b any
		if body != "" {
			if err := json.Unmarshal([]byte(body), &b); err != nil {
				t.Fatal(err)
			}
		}
		var w *httptest.ResponseRecorder
		if strings.HasPrefix(path, "/api/projects") {
			w = projectAPIRequest(a, method, path, b, false)
		} else {
			w = eventAPIRequest(a, method, path, b, false)
		}
		return map[string]any{"ok": w.Code >= 200 && w.Code < 300, "status": w.Code, "body": w.Body.String()}
	})
	vm.Set("sessionID", s.ID)
	harness := `
class Element {
 constructor(tag) { this.tagName=tag; this.children=[]; this.style={}; this.attrs={}; this.value=''; this.textContent=''; this.isConnected=true; this.hidden=false; this.checked=false; this.disabled=false; this.id=''; this.placeholder=''; }
 appendChild(e) { this.children.push(e); e.parent=this; return e; }
 replaceChildren() { this.children=[]; this.textContent=''; }
 remove() { if(this.parent) this.parent.children=this.parent.children.filter(c=>c!==this); this.parent=null; }
 setAttribute(k,v) { this.attrs[k]=v; }
 closest(sel) { if(sel==='#settings') return root; if(sel==='#cxsession') return null; return null; }
 querySelector(sel) { if(sel[0]==='#') return all(this).find(e=>e.id===sel.slice(1))||null; return null; }
 querySelectorAll(sel) { return all(this).filter(e=>sel.split(',').includes(e.tagName)); }
 focus() {}
}
var root=new Element('div'), mounted;
var document={createElement:t=>new Element(t),createElementNS:(ns,t)=>new Element(t)};
var window={confirm:()=>true,prompt:()=>'',addEventListener:()=>{},removeEventListener:()=>{}};
var JX={}; window.JX=JX;
var CX={register:d=>d.init(),unregisterSettings:()=>{},registerSettings:d=>{mounted=d;d.mount(root);}};
var state={cur:{id:sessionID},sessions:[]};
var localStorage={};
var closeSettingsUI=()=>{},jumpToMessage=()=>{},openSession=()=>{};
var setTimeout=()=>1, clearTimeout=()=>{};
function URLSearchParams(q){ this.map={}; if(q){ q.split('&').forEach(kv=>{ var i=kv.indexOf('='); this.map[decodeURIComponent(kv.slice(0,i))]=decodeURIComponent(kv.slice(i+1)); }); } }
URLSearchParams.prototype.set=function(k,v){ this.map[k]=v; };
URLSearchParams.prototype.toString=function(){ return Object.keys(this.map).map(k=>encodeURIComponent(k)+'='+encodeURIComponent(this.map[k])).join('&'); };
function all(e) { return e.children.reduce((a,c)=>a.concat([c],all(c)),[]); }
function text(e) { return (e.textContent||'')+e.children.map(text).join(''); }
function findButton(s) { var e=all(root).find(e=>e.tagName==='button'&&text(e)===s); if(!e) throw Error('button missing: '+s); return e; }
function click(s) { findButton(s).onclick(); }
function fieldExact(s) { var l=all(root).find(e=>e.tagName==='label'&&text(e)===s); if(!l) throw Error('field missing: '+s); return l.children[1]; }
function check(b,msg) { if(!b) throw Error(msg); }
var fetch=(path,opts)=>{opts=opts||{};var r=request(opts.method||'GET',path,opts.body||'');return Promise.resolve({ok:r.ok,status:r.status,text:()=>Promise.resolve(r.body),json:()=>Promise.resolve(JSON.parse(r.body))});};
`
	if _, err := vm.RunString(harness); err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"web/project-registry.js", "组件/project-events/client.js"} {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := vm.RunString(string(src)); err != nil {
			t.Fatalf("%s 执行失败: %v", file, err)
		}
	}
	run := func(script string) {
		t.Helper()
		if _, err := vm.RunString(script); err != nil {
			t.Fatal(err)
		}
	}

	run(`check(text(root).includes('事件项目'),'总览未显示项目: '+text(root));check(all(root).some(e=>e.tagName==='strong'&&text(e)==='新建目标'||text(e)==='新建目标'),'新建表单缺失');`)
	run(`fieldExact('标题（一句目标短语）').value='界面建的目标';fieldExact('概况（一句当前状态）').value='通过设置页新建。';click('新建目标');`)
	run(`check(text(root).includes('界面建的目标'),'新建后总览未刷新: '+text(root));`)
	events, _, _, err := a.Store.ProjectEventPage(p.ID, ProjectEventFilter{Scope: projectEventScopeAll}, nil, 40)
	if err != nil || len(events) != 1 || events[0].Title != "界面建的目标" {
		t.Fatalf("设置页新建未落库: %+v %v", events, err)
	}
	run(`click('查看时间线');`)
	run(`check(text(root).includes('还没有推进记录'),'时间线空态缺失: '+text(root));fieldExact('事实').value='设置页追加的一条事实。';click('追加事实');`)
	run(`check(text(root).includes('设置页追加的一条事实。'),'时间线未刷新: '+text(root));check(text(root).includes('时间线：共 1 条记录'),'记录计数未刷新');`)
	evID := events[0].ID
	recs, err := a.Store.ProjectEventRecordPage(evID, ProjectEventRecordFilter{}, "", nil, 10)
	if err != nil || recs.Total != 1 {
		t.Fatalf("设置页追加未落库: %+v %v", recs, err)
	}
	// 项目登记：宿主视图，不用知识库组件（夹具本就没装它）。
	run(`click('项目登记 ▸ 展开');`)
	run(`check(text(root).includes('编辑'),'登记视图未挂载: '+text(root));check(text(root).includes('事件项目'),'登记视图未列出项目');`)
	run(`check(!!JX.projectRegistry&&typeof JX.projectRegistry.mount==='function','宿主登记入口缺失');`)
	if s.ID == "" {
		t.Fatal("夹具会话缺失")
	}
}
