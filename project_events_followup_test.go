package main

// 复审（第二轮）N1–N4 的回归测试：
//   N1 同一幂等键但同事务 event patch 不同 → 必须语义冲突，不能 200 changed=false 静默丢 patch；
//   N2 宿主共享登记恢复「已有远程目录」下拉与刷新（两个设置页共用）；
//   N3 登记视图新建/切项目前复用知识页未保存草稿确认，取消则不切项目且保留草稿；
//   N4 知识正文预算预览的 setTimeout 节流路径真正被执行。
// 一律隔离临时库 + goja 真组件 + 真 HTTP 路由，不碰真实运行库。

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestProjectEventIdempotentRetryRejectsChangedPatch 守 N1。
func TestProjectEventIdempotentRetryRejectsChangedPatch(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 1)
	ev := createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "幂等 patch", "summary": "原概况。"})

	call := func(rev int64, patch map[string]any) (map[string]any, error) {
		body := map[string]any{
			"event_id": ev.ID, "expected_event_revision": rev, "client_key": "patch-1",
			"kind": "milestone", "text": "带 patch 的同一条事实。",
			"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
		}
		if patch != nil {
			body["event"] = patch
		}
		return callEventCap(t, a, s, "project_event_record_add", body)
	}

	first, err := call(ev.Revision, map[string]any{"summary": "第一次提交的概况。"})
	if err != nil {
		t.Fatalf("首次带 patch 追加失败: %v", err)
	}
	if changed, _ := first["changed"].(bool); !changed {
		t.Fatalf("首次带 patch 追加应算变化: %v", first)
	}
	firstEv := first["event"].(map[string]any)
	if firstEv["summary"] != "第一次提交的概况。" {
		t.Fatalf("首次 patch 应落库: %v", firstEv)
	}
	firstRecID := first["record"].(map[string]any)["record_id"].(string)
	appliedRev := int64(firstEv["revision"].(float64))

	// 同键、同事实、patch 换成别的：属于「同键不同载荷」，必须冲突。
	if _, err := call(appliedRev, map[string]any{"summary": "重试时想改的概况。"}); err == nil || !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("同键不同 event patch 应冲突（不能静默丢 patch）: %v", err)
	}
	detail := mustEventCap(t, a, s, "project_event_get", map[string]any{"event_id": ev.ID})
	if got := detail["event"].(map[string]any)["summary"]; got != "第一次提交的概况。" {
		t.Fatalf("冲突后概况不应被改动: %v", got)
	}

	// 同键、同事实、同 patch，带旧 revision 重试：回执已提交结果，不重复追加、不再改。
	same, err := call(ev.Revision, map[string]any{"summary": "第一次提交的概况。"})
	if err != nil {
		t.Fatalf("同键同载荷重试不应失败: %v", err)
	}
	if changed, _ := same["changed"].(bool); changed {
		t.Fatalf("同键同载荷重试不应算变化: %v", same)
	}
	if got := same["record"].(map[string]any)["record_id"].(string); got != firstRecID {
		t.Fatalf("幂等重试应回执原记录 %s，实得 %s", firstRecID, got)
	}

	// 同键、同事实、但漏掉 patch：同样是不同语义载荷，不能当成同一请求。
	if _, err := call(appliedRev, nil); err == nil || !strings.Contains(err.Error(), "幂等键") {
		t.Fatalf("漏掉 patch 的重试应冲突: %v", err)
	}

	if recs, err := a.Store.ProjectEventRecordPage(ev.ID, ProjectEventRecordFilter{}, "", nil, 10); err != nil || recs.Total != 1 {
		t.Fatalf("冲突/重试都不应多出记录: total=%d err=%v", recs.Total, err)
	}
}

// 设置页用户路径同样守 N1（handleEventRecordAdd 的 patch 也进幂等语义）。
func TestProjectEventSettingsIdempotentRetryRejectsChangedPatch(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 1)
	w := eventAPIRequest(a, "POST", "/api/project-events", map[string]any{
		"project_id": p.ID, "title": "设置页幂等 patch", "summary": "原概况。",
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
	body := func(patch map[string]any) map[string]any {
		r := map[string]any{
			"expected_event_revision": created.Event.Revision,
			"record": map[string]any{
				"kind": "validation", "text": "用户带 patch 的一条事实。", "client_key": "ui-patch-1",
				"sources": []any{map[string]any{"session_id": s.ID}},
			},
		}
		if patch != nil {
			r["event"] = patch
		}
		return r
	}
	w = eventAPIRequest(a, "POST", "/api/project-events/"+created.Event.ID+"/records", body(map[string]any{"summary": "用户第一次的概况。"}), false)
	if w.Code != 200 {
		t.Fatalf("首次带 patch 追加失败: %d %s", w.Code, w.Body.String())
	}
	w = eventAPIRequest(a, "POST", "/api/project-events/"+created.Event.ID+"/records", body(map[string]any{"summary": "重试时想改的概况。"}), false)
	if w.Code != 409 {
		t.Fatalf("同键不同 patch 应 409: %d %s", w.Code, w.Body.String())
	}
	w = eventAPIRequest(a, "GET", "/api/project-events/"+created.Event.ID, nil, false)
	if w.Code != 200 {
		t.Fatalf("读回失败: %d %s", w.Code, w.Body.String())
	}
	var detail struct {
		Event ProjectEvent `json:"event"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil {
		t.Fatal(err)
	}
	if detail.Event.Summary != "用户第一次的概况。" {
		t.Fatalf("冲突后概况不应被改动: %q", detail.Event.Summary)
	}
}

// TestProjectRegistryRemoteDirectoryPicker 守 N2：宿主登记视图恢复「已有远程目录」下拉与刷新。
func TestProjectRegistryRemoteDirectoryPicker(t *testing.T) {
	a := projectCapabilityTestApp(t)
	_, run := projectSettingsVM(t, a, true)

	run(`clickLabel('项目登记 ▸ 展开');registryOpen=true;`)
	run(`clickLabel('新建项目');field('项目名称').value='远程项目';`)
	run(`var sel=byAria('选择已有远程目录');check(!!sel,'远程目录下拉缺失');
var opts=sel.children.map(function(o){return o.value;});
check(opts.indexOf('u@[2001:db8::1]:2222:/srv/app')>=0,'缺少 IPv6+端口的远程目录: '+opts.join(','));
check(opts.indexOf('root@10.0.0.5:/opt/x')>=0,'缺少默认端口(22 不写)的远程目录: '+opts.join(','));`)
	run(`var sel=byAria('选择已有远程目录');
sel.value='u@[2001:db8::1]:2222:/srv/app';sel.onchange();
check(byAria('新绑定路径').value==='u@[2001:db8::1]:2222:/srv/app','选中远程目录未填入路径: '+byAria('新绑定路径').value);
var kind=byAria('新绑定路径').parent.children.filter(function(c){return c.tagName==='select';})[0];
check(kind.value==='remote','选中已有远程目录应把类型切到远程: '+kind.value);`)
	run(`clickLabel('添加绑定');clickLabel('创建项目');`)
	run(`check(text(mountTarget).includes('正在编辑「远程项目」'),'远程绑定创建未进入编辑态: '+text(mountTarget).slice(0,800));`)

	list, err := a.Store.ListProjects(true)
	if err != nil || len(list) != 1 || len(list[0].Workspaces) != 1 {
		t.Fatalf("远程绑定未落库: %+v %v", list, err)
	}
	if got := list[0].Workspaces[0]; got.WorkspaceKind != "remote" || got.DisplayPath != "u@[2001:db8::1]:2222:/srv/app" {
		t.Fatalf("远程绑定内容不符: %+v", got)
	}

	// 刷新按钮真的重读 /api/remotes 并回报数量。
	run(`if(!all(mountTarget).some(e=>e.tagName==='button'&&text(e)==='刷新远程目录')) clickLabel('编辑');
clickLabel('刷新远程目录');`)
	run(`check(text(mountTarget).includes('已刷新 2 个已登记远程目录'),'刷新远程目录无回执: '+text(mountTarget).slice(0,800));`)
}

// TestProjectKnowledgeDraftGuardWhenRegistryCreatesProject 守 N3。
func TestProjectKnowledgeDraftGuardWhenRegistryCreatesProject(t *testing.T) {
	a := projectCapabilityTestApp(t)
	vm, run := projectSettingsVM(t, a, true)
	vm.Set("testDirectory2", t.TempDir())

	run(`createProject('草稿项目', testDirectory);`)
	run(`var c=all(mountTarget).find(e=>e.tagName==='textarea');check(!!c,'知识正文缺失');c.value='未保存的草稿正文';c.oninput();`)

	// 取消切换：登记视图新建另一个项目，但用户拒绝离开当前草稿。
	vm.Set("confirmAnswer", false)
	run(`createProject('另一个项目', testDirectory2);`)
	run(`var c=all(mountTarget).find(e=>e.tagName==='textarea');check(!!c&&c.value==='未保存的草稿正文','取消后草稿被丢弃: '+(c?('"'+c.value+'"'):'no textarea'));
check(text(byAria('请选择项目')).startsWith('草稿项目'),'取消后不应切换项目: '+text(byAria('请选择项目')));`)
	if list, err := a.Store.ListProjects(true); err != nil || len(list) != 2 {
		t.Fatalf("登记视图的新建应已生效（只是知识页没切走）: %+v %v", list, err)
	}

	// 确认切换：下拉切到「另一个项目」→ 换成它自己的正文；切回草稿项目 → 草稿还在。
	vm.Set("confirmAnswer", true)
	run(`var trigger=byAria('请选择项目');var panel=trigger.parent.children[1];trigger.onclick();
var item=all(panel).find(e=>e.tagName==='button'&&text(e).startsWith('另一个项目 · '));check(!!item,'下拉里找不到另一个项目');item.onclick();`)
	run(`check(text(byAria('请选择项目')).startsWith('另一个项目'),'确认后应切到新项目: '+text(byAria('请选择项目')));`)
	run(`var trigger=byAria('请选择项目');var panel=trigger.parent.children[1];trigger.onclick();
var item=all(panel).find(e=>e.tagName==='button'&&text(e).startsWith('草稿项目 · '));check(!!item,'下拉里找不到草稿项目');item.onclick();`)
	run(`var c=all(mountTarget).find(e=>e.tagName==='textarea');check(!!c&&c.value==='未保存的草稿正文','切回后草稿丢失: '+(c?c.value:'no textarea'));`)
}

// TestProjectKnowledgeBudgetPreviewThrottle 守 N4：知识正文预算预览的 setTimeout 节流真的执行。
func TestProjectKnowledgeBudgetPreviewThrottle(t *testing.T) {
	a := projectCapabilityTestApp(t)
	_, run := projectSettingsVM(t, a, true)

	run(`createProject('预算预览项目', testDirectory);`)
	run(`var c=all(mountTarget).find(e=>e.tagName==='textarea');check(!!c,'知识正文缺失');c.value='中文';c.oninput();`)
	run(`flushTimers();`)
	run(`check(text(mountTarget).includes('预算用量：4 / 4000 单元'),'知识正文预算预览未生效（节流路径未覆盖）: '+text(mountTarget).slice(0,800));`)
}
