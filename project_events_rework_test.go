package main

// 项目事件 V1 返工的回归测试（对应独立审查报告 F1–F5、F7–F11 的落地缺口）。
// 这些用例先在新代码上跑绿；返工前在 773e14fa 的临时工作树上验过红（见交接说明）。
// 数据库一律用 t.TempDir 的隔离实例，不碰真实运行库。

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// TestProjectEventRecordAddReceiptIsInsertedRecord 守 F1：追加记录的回执必须是本次插入
// 的那一行，不能提交后按「最新一条」重读——晚到的旧事实会让「最新一条」指向别的记录。
func TestProjectEventRecordAddReceiptIsInsertedRecord(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 2)

	w := eventAPIRequest(a, "POST", "/api/project-events", map[string]any{
		"project_id": p.ID, "title": "回执身份", "summary": "回执必须对应本次插入的记录。",
	}, false)
	if w.Code != 200 {
		t.Fatalf("建目标失败: %d %s", w.Code, w.Body.String())
	}
	var created struct {
		Event ProjectEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	add := func(rev int64, body map[string]any) (ProjectEvent, ProjectEventRecord) {
		t.Helper()
		body["expected_event_revision"] = rev
		w := eventAPIRequest(a, "POST", "/api/project-events/"+created.Event.ID+"/records", body, false)
		if w.Code != 200 {
			t.Fatalf("追加失败: %d %s", w.Code, w.Body.String())
		}
		var out struct {
			Event  ProjectEvent       `json:"event"`
			Record ProjectEventRecord `json:"record"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out.Event, out.Record
	}

	ev, later := add(created.Event.Revision, map[string]any{
		"record": map[string]any{
			"kind": "milestone", "text": "较晚发生的事实。", "occurred_at": "2026-11-16T10:00:00Z",
			"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
		},
	})
	// 晚到的旧事实：事实时间更早，但入库更晚；回执仍必须是这一条。
	_, late := add(ev.Revision, map[string]any{
		"record": map[string]any{
			"kind": "issue", "text": "更早发生的晚到事实。", "occurred_at": "2026-11-01T08:00:00Z",
			"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 2}},
		},
	})
	if late.Text != "更早发生的晚到事实。" {
		t.Fatalf("回执指向了别的记录：期望本次插入的「更早发生的晚到事实。」，实得 %q（record_id=%s）", late.Text, late.ID)
	}
	if late.ID == later.ID {
		t.Fatalf("回执复用了上一条记录的 ID: %s", late.ID)
	}
	page, err := a.Store.ProjectEventRecordPage(created.Event.ID, ProjectEventRecordFilter{}, "", nil, 10)
	if err != nil || page.Total != 2 {
		t.Fatalf("两条事实都应落库: total=%d err=%v", page.Total, err)
	}
}

// TestProjectEventRecordAddIdempotentRetrySurvivesParentRevision 守 F4：同键同载荷重试是
// 「已提交请求的重发」，父 revision 后来前进也不能判成版本冲突（设计 §7.1）。
func TestProjectEventRecordAddIdempotentRetrySurvivesParentRevision(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 1)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "幂等重试", "summary": "父 revision 前进后重试仍应回执。"})

	args := map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "retry-1",
		"kind": "milestone", "text": "可重试的事实。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
	}
	first := mustEventCap(t, a, s, "project_event_record_add", args)
	firstRec := first["record"].(map[string]any)["record_id"].(string)

	// 父事件前进：改概况（revision+1）。
	if out := mustEventCap(t, a, s, "project_event_update", map[string]any{
		"event_id": ev.ID, "expected_revision": 2, "summary": "父事件已经前进。",
	}); !out["changed"].(bool) {
		t.Fatalf("改概况应算变化: %v", out)
	}

	// 同键同载荷重试，仍带旧的 expected_event_revision。
	out, err := callEventCap(t, a, s, "project_event_record_add", args)
	if err != nil {
		t.Fatalf("父 revision 前进后幂等重试不应失败: %v", err)
	}
	if changed, _ := out["changed"].(bool); changed {
		t.Fatalf("幂等重试不应算变化: %v", out)
	}
	if got := out["record"].(map[string]any)["record_id"].(string); got != firstRec {
		t.Fatalf("幂等重试应回执原记录 %s，实得 %s", firstRec, got)
	}
	if recs, err := a.Store.ProjectEventRecordPage(ev.ID, ProjectEventRecordFilter{}, "", nil, 10); err != nil || recs.Total != 1 {
		t.Fatalf("重试不应重复追加: total=%d err=%v", recs.Total, err)
	}

	// 同键异载荷仍是冲突，不能借幂等覆盖别的事实。
	bad := map[string]any{
		"event_id": ev.ID, "expected_event_revision": 3, "client_key": "retry-1",
		"kind": "milestone", "text": "换了内容的事实。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
	}
	if _, err := callEventCap(t, a, s, "project_event_record_add", bad); err == nil || !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("同键异载荷应冲突: %v", err)
	}
}

// TestProjectEventAIAppendRequiresRealSource 守 F3 / §4.4：AI 新增关键事实必须给出实际
// 依据会话；零来源只能来自设置页用户录入（origin=user）。
func TestProjectEventAIAppendRequiresRealSource(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 1)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "AI 来源", "summary": "零来源事实应被拒。"})

	_, err := callEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "nosrc-1",
		"kind": "milestone", "text": "没有依据会话的事实。", "sources": []any{},
	})
	if err == nil || !strings.Contains(err.Error(), "来源") {
		t.Fatalf("AI 零来源追加应被拒: %v", err)
	}
	// 直接建非讨论中状态也一样：事实必须有来源。
	_, err = callEventCap(t, a, s, "project_event_create", map[string]any{
		"project_id": p.ID, "title": "无依据直接处理中", "summary": "应被拒。", "status": "processing",
		"record": map[string]any{"kind": "plan", "text": "没有来源的计划确认。", "sources": []any{}},
	})
	if err == nil || !strings.Contains(err.Error(), "来源") {
		t.Fatalf("AI 无来源事实直接建非讨论中应被拒: %v", err)
	}
	// 真正给了依据会话才能写；设置页用户仍可录无来源事实（另一条入口，不在此放宽）。
	out := mustEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "src-1",
		"kind": "milestone", "text": "有依据会话的事实。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
	})
	if got := out["record"].(map[string]any)["origin"]; got != eventOriginAI {
		t.Fatalf("AI 录入应标 origin=ai，实得 %v", got)
	}
	w := eventAPIRequest(a, "POST", "/api/project-events/"+ev.ID+"/records", map[string]any{
		"expected_event_revision": 2,
		"record":                  map[string]any{"kind": "acceptance", "text": "用户口头确认的无来源事实。", "sources": []any{}},
	}, false)
	if w.Code != 200 {
		t.Fatalf("用户设置页应允许无来源事实: %d %s", w.Code, w.Body.String())
	}
}

// TestProjectEventAIAppendCannotDropLastSource 守 §4.4：显式移除来源不能把原 AI 事实
// 变成无来源记录。
func TestProjectEventAIAppendCannotDropLastSource(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 1)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "来源不变量", "summary": "AI 记录不能删空来源。"})
	out := mustEventCap(t, a, s, "project_event_record_add", map[string]any{
		"event_id": ev.ID, "expected_event_revision": ev.Revision, "client_key": "keep-1",
		"kind": "validation", "text": "一条有依据的验证结论。",
		"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
	})
	rec := out["record"].(map[string]any)
	recID := rec["record_id"].(string)
	recRev := int64(rec["revision"].(float64))
	evRev := int64(out["event"].(map[string]any)["revision"].(float64))

	list := mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": recID})
	sources := list["sources"].([]any)
	if len(sources) != 1 {
		t.Fatalf("来源条数不符: %v", list)
	}
	sid := sources[0].(map[string]any)["source_id"].(string)

	_, err := callEventCap(t, a, s, "project_event_record_update", map[string]any{
		"record_id": recID, "expected_record_revision": recRev, "expected_event_revision": evRev,
		"sources_remove": []any{sid},
	})
	if err == nil || !strings.Contains(err.Error(), "来源") {
		t.Fatalf("把 AI 记录来源删空应被拒: %v", err)
	}
	// 拒写后来源与记录都还在。
	after := mustEventCap(t, a, s, "project_event_sources_query", map[string]any{"record_id": recID})
	if len(after["sources"].([]any)) != 1 {
		t.Fatalf("拒写不应删掉来源: %v", after)
	}
	if _, err := callEventCap(t, a, s, "project_event_get", map[string]any{"event_id": ev.ID}); err != nil {
		t.Fatalf("拒写不应影响事实可读: %v", err)
	}
}

// TestProjectEventOverviewGroupCarriesEventCursor 守 F10：总览每组要给事件续读游标，
// 调用方不必自己重启一次查询即可接着读该组剩余目标。
func TestProjectEventOverviewGroupCarriesEventCursor(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	for i := 0; i < 4; i++ {
		createEvent(t, a, s, map[string]any{
			"project_id": p.ID, "title": fmt.Sprintf("目标%d", i), "summary": "总览每组最多预览 3 个。",
		})
	}
	over := mustEventCap(t, a, s, "project_events_overview", map[string]any{})
	groups := over["projects"].([]any)
	if len(groups) != 1 {
		t.Fatalf("应只有自身项目一组: %v", over)
	}
	group := groups[0].(map[string]any)
	if int(group["events_remaining"].(float64)) != 1 || group["events_has_more"] != true {
		t.Fatalf("预览应封顶 3 条并报剩余 1: %v", group)
	}
	cursor, _ := group["events_next_cursor"].(string)
	if cursor == "" {
		t.Fatalf("总览每组都应给事件续读游标: %v", group)
	}
	preview := map[string]bool{}
	for _, e := range group["events"].([]any) {
		preview[e.(map[string]any)["event_id"].(string)] = true
	}
	next := mustEventCap(t, a, s, "project_events_query", map[string]any{"project_id": p.ID, "cursor": cursor})
	events := next["events"].([]any)
	if len(events) != 1 {
		t.Fatalf("用组游标续读应拿到剩余 1 条: %v", next)
	}
	if id := events[0].(map[string]any)["event_id"].(string); preview[id] {
		t.Fatalf("续读不应重复预览里的目标: %s", id)
	}
}

// TestProjectEventOverviewExcludesArchivedByDefault 守 F10：默认不含归档项目，
// 显式 include_archived 才按只读列出（设计 §6.2）。
func TestProjectEventOverviewExcludesArchivedByDefault(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "会随归档隐藏", "summary": "默认不含归档项目。"})

	latest, err := a.Store.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	archived := true
	if _, err := a.Store.PatchProject(p.ID, nil, &archived, nil, &latest.UpdatedAt); err != nil {
		t.Fatal(err)
	}
	over := mustEventCap(t, a, s, "project_events_overview", map[string]any{})
	if len(over["projects"].([]any)) != 0 {
		t.Fatalf("默认总览不应列出归档项目: %v", over)
	}
	if note, _ := over["note"].(string); !strings.Contains(note, "归档") {
		t.Fatalf("全归档时应说清为何没有结果: %v", over["note"])
	}
	over = mustEventCap(t, a, s, "project_events_overview", map[string]any{"include_archived": true})
	groups := over["projects"].([]any)
	if len(groups) != 1 || groups[0].(map[string]any)["read_only"] != true {
		t.Fatalf("显式包含归档时应按只读列出: %v", over)
	}
}

// TestProjectEventSettingsPageDefaultsMatchDesign 守 F10：设置页默认条数按设计 §6.2
// 的 UI 推荐（项目 20 组、事件 12 条），不是把 AI 的小页当 UI 默认，也不互换。
func TestProjectEventSettingsPageDefaultsMatchDesign(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	for i := 0; i < 13; i++ {
		createEvent(t, a, s, map[string]any{
			"project_id": p.ID, "title": fmt.Sprintf("默认页目标%d", i), "summary": "设置页默认一页 12 条。",
		})
	}
	w := eventAPIRequest(a, "GET", "/api/project-events?project_id="+p.ID, nil, false)
	if w.Code != 200 {
		t.Fatalf("事件列表失败: %d %s", w.Code, w.Body.String())
	}
	var page struct {
		Total     int  `json:"total"`
		Shown     int  `json:"shown"`
		Remaining int  `json:"remaining"`
		HasMore   bool `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if page.Total != 13 || page.Shown != 12 || page.Remaining != 1 || !page.HasMore {
		t.Fatalf("设置页事件默认应为 12 条/页: %+v", page)
	}

	// 项目组默认 20 组：再登记 20 个项目，总览首页应显示 20 组而不是更小/更大。
	for i := 0; i < 20; i++ {
		dir := t.TempDir()
		binding, err := normalizeProjectWorkspace("local", dir)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := a.Store.CreateProject(Project{Name: fmt.Sprintf("默认页项目%02d", i), Workspaces: []ProjectWorkspace{binding}}); err != nil {
			t.Fatal(err)
		}
	}
	w = eventAPIRequest(a, "GET", "/api/project-events/overview", nil, false)
	if w.Code != 200 {
		t.Fatalf("总览失败: %d %s", w.Code, w.Body.String())
	}
	var over struct {
		ProjectTotal int  `json:"project_total"`
		ProjectShown int  `json:"project_shown"`
		HasMore      bool `json:"has_more"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &over); err != nil {
		t.Fatal(err)
	}
	if over.ProjectTotal != 21 || over.ProjectShown != 20 || !over.HasMore {
		t.Fatalf("设置页总览默认应为 20 组项目: %+v", over)
	}
}

// ---------- 设置页真实交互（goja + 真组件 + 最小 DOM） ----------

const eventPaneHarness = `
class Element {
 constructor(tag) { this.tagName=tag; this.children=[]; this.style={}; this.attrs={}; this.value=''; this.textContent=''; this.isConnected=true; this.hidden=false; this.checked=false; this.disabled=false; this.id=''; this.placeholder=''; }
 appendChild(e) { this.children.push(e); e.parent=this; return e; }
 replaceChildren() { this.children=[]; this.textContent=''; }
 remove() { if(this.parent) this.parent.children=this.parent.children.filter(c=>c!==this); this.parent=null; }
 setAttribute(k,v) { this.attrs[k]=v; }
 getAttribute(k) { return this.attrs[k]; }
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
var closeSettingsUI=()=>{},jumpToMessage=()=>{},openSession=()=>{};
var setTimeout=()=>1, clearTimeout=()=>{};
function URLSearchParams(q){ this.map={}; if(q){ q.split('&').forEach(kv=>{ var i=kv.indexOf('='); this.map[decodeURIComponent(kv.slice(0,i))]=decodeURIComponent(kv.slice(i+1)); }); } }
URLSearchParams.prototype.set=function(k,v){ this.map[k]=v; };
URLSearchParams.prototype.toString=function(){ return Object.keys(this.map).map(k=>encodeURIComponent(k)+'='+encodeURIComponent(this.map[k])).join('&'); };
function all(e) { return e.children.reduce((a,c)=>a.concat([c],all(c)),[]); }
function text(e) { return (e.textContent||'')+e.children.map(text).join(''); }
function buttons(s) { return all(root).filter(e=>e.tagName==='button'&&text(e)===s); }
function findButton(s) { var e=buttons(s)[0]; if(!e) throw Error('button missing: '+s+' :: '+text(root).slice(0,600)); return e; }
function click(s) { findButton(s).onclick(); }
function fieldExact(s) { var l=all(root).find(e=>e.tagName==='label'&&text(e)===s); if(!l) throw Error('field missing: '+s+' :: '+text(root).slice(0,600)); return l.children[1]; }
function fieldPrefix(s) { var l=all(root).find(e=>e.tagName==='label'&&text(e).startsWith(s)); if(!l) throw Error('field missing(prefix): '+s+' :: '+text(root).slice(0,600)); return l.children[1]; }
function check(b,msg) { if(!b) throw Error(msg); }
var fetch=(path,opts)=>{opts=opts||{};var r=request(opts.method||'GET',path,opts.body||'');return Promise.resolve({ok:r.ok,status:r.status,text:()=>Promise.resolve(r.body),json:()=>Promise.resolve(JSON.parse(r.body))});};
`

func eventPaneVM(t *testing.T, a *App, s *SessionMeta) (*goja.Runtime, func(string)) {
	t.Helper()
	vm := goja.New()
	vm.Set("sessionID", s.ID)
	vm.Set("request", func(method, path, body string) map[string]any {
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
	if _, err := vm.RunString(eventPaneHarness); err != nil {
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
	return vm, func(script string) {
		t.Helper()
		if _, err := vm.RunString(script); err != nil {
			t.Fatal(err)
		}
	}
}
