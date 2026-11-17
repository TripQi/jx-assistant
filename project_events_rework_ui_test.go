package main

// 设置页真实交互回归（goja + 真组件 + 最小 DOM）：对应独立审查报告 F2 / F5 / F8，
// 以及 F7「知识库页复用宿主项目登记」的结构断言。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

// TestProjectEventSettingsCreateWithStatusAndBudgetPreview 守 F2 / F5：
//   - 新建表单选非讨论中状态时能在同一次提交里附一条关键事实（旧界面只发
//     project_id/title/summary/status，提示还让用户「下一步」补，服务端却要求同次，
//     必然失败）；
//   - 输入框旁有字段级「已用 / 上限 单元」预览，数值来自宿主预算函数，不是 JS 长度。
func TestProjectEventSettingsCreateWithStatusAndBudgetPreview(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	_, run := eventPaneVM(t, a, s)

	run(`var t=fieldExact('标题（一句目标短语）');t.value='汉';if(t.oninput)t.oninput();`)
	run(`check(text(root).includes('已用 2 / 80 单元'),'标题预算预览缺失: '+text(root).slice(0,600));`)

	run(`fieldExact('标题（一句目标短语）').value='界面建成处理中';
fieldExact('概况（一句当前状态）').value='带事实直接建。';
fieldPrefix('新建状态').value='processing';
fieldExact('关键事实').value='用户确认方案后直接记为处理中。';
click('新建目标');`)
	run(`check(text(root).includes('界面建成处理中'),'非讨论中新建未成功: '+text(root));`)

	events, _, _, err := a.Store.ProjectEventPage(p.ID, ProjectEventFilter{Scope: projectEventScopeAll}, nil, 40)
	if err != nil || len(events) != 1 || events[0].Status != eventStatusProcessing || events[0].RecordCount != 1 {
		t.Fatalf("设置页应能带事实直接建非讨论中目标: %+v %v", events, err)
	}
}

// TestProjectEventSettingsRecordAddShowsBudgetForFact 守 F5：追加事实的事实字段也有预算预览。
func TestProjectEventSettingsRecordAddShowsBudgetForFact(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	createEvent(t, a, s, map[string]any{"project_id": p.ID, "title": "预算预览", "summary": "看事实字段的已用/上限。"})
	_, run := eventPaneVM(t, a, s)
	run(`click('查看时间线');`)
	run(`var f=fieldExact('事实');f.value='汉';if(f.oninput)f.oninput();`)
	run(`check(text(root).includes('已用 2 / 240 单元'),'事实预算预览缺失: '+text(root).slice(0,600));`)
}

// TestProjectEventSettingsSourceRecheckShowsClickedSource 守 F8：「复核这条」显示的是
// 点的那一条来源的结论，而不是返回数组的第一条。
func TestProjectEventSettingsSourceRecheckShowsClickedSource(t *testing.T) {
	a, s, p := projectEventsTestApp(t)
	seedEventMessages(t, a, s.ID, 2)
	w := eventAPIRequest(a, "POST", "/api/project-events", map[string]any{
		"project_id": p.ID, "title": "来源复核", "summary": "点第二条应显示第二条的结论。",
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
	// 两步落两条来源，让会话级来源（后绑）固定排在前，确保「点的那条」不是返回数组的第一条：
	// 旧代码取 sources[0] 的 note，显示的是会话级那条的结论而不是消息级这条的。
	w = eventAPIRequest(a, "POST", "/api/project-events/"+created.Event.ID+"/records", map[string]any{
		"expected_event_revision": created.Event.Revision,
		"record": map[string]any{
			"kind": "validation", "text": "两条来源的验证结论。",
			"sources": []any{map[string]any{"session_id": s.ID, "message_seq": 1}},
		},
	}, false)
	if w.Code != 200 {
		t.Fatalf("追加失败: %d %s", w.Code, w.Body.String())
	}
	var added struct {
		Event  ProjectEvent       `json:"event"`
		Record ProjectEventRecord `json:"record"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &added); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	w = eventAPIRequest(a, "PATCH", "/api/project-events/records/"+added.Record.ID, map[string]any{
		"expected_record_revision": added.Record.Revision,
		"expected_event_revision":  added.Event.Revision,
		"sources_add":              []any{map[string]any{"session_id": s.ID}},
	}, false)
	if w.Code != 200 {
		t.Fatalf("补会话级来源失败: %d %s", w.Code, w.Body.String())
	}

	_, run := eventPaneVM(t, a, s)
	run(`click('查看时间线');`)
	run(`click('来源（2）');`)
	run(`var rows=all(root).filter(function(e){return e.children.some(function(c){return c.tagName==='button'&&text(c)==='复核这条';});});
var row=rows.find(function(r){return text(r).indexOf('seq 1')>=0;});
check(!!row,'找不到消息级来源行: '+text(root).slice(0,800));
row.children.find(function(c){return c.tagName==='button'&&text(c)==='复核这条';}).onclick();`)
	run(`check(text(root).includes('已复核'),'复核结论不是所点那条来源: '+text(root).slice(0,800));`)
}

// TestProjectKnowledgePaneMountsHostRegistry 守 F7（真实挂载）：知识库页展开「项目登记」时
// 挂的是宿主同一份 JX.projectRegistry（全局只一个定义），且不再另渲染一套新建项目表单。
func TestProjectKnowledgePaneMountsHostRegistry(t *testing.T) {
	a := projectCapabilityTestApp(t)
	_, run := projectSettingsVM(t, a, true)

	run(`check(registryMountCount===0,'登记视图不应在未展开时就挂载');`)
	run(`clickLabel('项目登记 ▸ 展开');`)
	run(`check(registryMountCount===1,'知识库页没挂宿主登记视图（挂载次数 '+registryMountCount+'）');`)
	run(`check(text(mountTarget).includes('项目身份与工作区绑定是全实例唯一的一套'),'共享登记说明缺失');`)
	run(`check(text(mountTarget).includes('新建项目'),'共享登记视图未渲染新建入口');`)
	run(`check(all(mountTarget).filter(e=>e.tagName==='button'&&text(e)==='新建项目').length===1,'出现了第二套「新建项目」入口');`)
}

// TestProjectKnowledgePageReusesSharedRegistry 守 F7（结构断言）：知识库页不再自带一套
// 项目登记，登记表单 / 路径解析 / 归档 / 删除都由宿主 JX.projectRegistry 承担。
func TestProjectKnowledgePageReusesSharedRegistry(t *testing.T) {
	src, err := os.ReadFile("组件/project-knowledge/client.js")
	if err != nil {
		t.Fatal(err)
	}
	text := string(src)
	for _, want := range []string{"JX.projectRegistry", "项目登记"} {
		if !strings.Contains(text, want) {
			t.Fatalf("知识库页未复用宿主登记（缺 %q）", want)
		}
	}
	for _, bad := range []string{"新建项目", "保存名称与绑定", "永久删除项目", "归档项目", "'/resolve'", "api('', 'POST'"} {
		if strings.Contains(text, bad) {
			t.Fatalf("知识库页仍在自建登记：%q", bad)
		}
	}
}
