package main

// AI 任务两种执行目标的到点分发测试（直接调 dispatch，不经 HTTP 与调度拍）：
//   - manager（长期助理）：现有路径不回归——描述进数字助理主对话，不新建会话；
//   - task（新工作任务）：走 createSession 单源建工作任务会话（mode=task、顶层、
//     dispatched_by=待办调度器系统派生标记），描述投进新会话，主对话不被打扰，
//     执行记录与任务上的 lastSessionId 指向新会话。
// 另覆盖 target 参数的归一化（空/未知回落长期助理、程序任务清空）与模型工具
// 的参数同步（「人工有啥 AI 有啥」）。

import (
	"strings"
	"sync"
	"testing"
	"time"
)

// newTodoDispatchApp 建一个带临时库的 App 与一个手动构造的调度器（不起调度
// 循环，测试直接调 dispatch）。刻意不配模型提供方：被投递的消息不会被真的
// 跑起来（goroutine 里的执行体会立刻失败收尾，不影响断言）。
func newTodoDispatchApp(t *testing.T) (*App, *todoScheduler, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	// 待办派发要走三档模型取值链（默认档落到全局默认模型），这里配一个 mock 渠道：
	// BaseURL=mock 不要密钥，投递后的执行也不会真的请求网络。
	a.Cfg.Providers = []ProviderCfg{{ID: "mock-prov", Name: "演示", BaseURL: "mock", Models: []ModelCfg{{ID: "mock-model"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "mock-prov", Model: "mock-model"}
	s := &todoScheduler{app: a, busy: map[string]bool{}, tombs: map[string]bool{},
		data: &TodoData{Version: todoDataVersion, Tasks: []*TaskItem{}, Records: []*RunRecord{}}}
	// 长期助理主对话：真实环境由 assistantSession 找/建，这里手动放一个
	mgr := &SessionMeta{ID: "mgr-main", Title: "数字助理主对话", Mode: "assistant",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(mgr); err != nil {
		t.Fatalf("建长期助理主对话失败: %v", err)
	}
	return a, s, mgr
}

// todoAllSessions 返回库里全部会话。
func todoAllSessions(t *testing.T, a *App) []*SessionMeta {
	t.Helper()
	list, err := a.Store.List()
	if err != nil {
		t.Fatalf("读会话列表失败: %v", err)
	}
	return list
}

// findTodoTask 取回一条任务的副本（持锁读）。
func findTodoTask(t *testing.T, s *todoScheduler, id string) *TaskItem {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	task, ok := s.find(id)
	if !ok {
		t.Fatalf("找不到任务 %s", id)
	}
	cp := *task
	return &cp
}

// lastTodoRecord 取最近一条执行记录（持锁读）。
func lastTodoRecord(t *testing.T, s *todoScheduler) *RunRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.data.Records) == 0 {
		t.Fatal("还没有执行记录")
	}
	return s.data.Records[len(s.data.Records)-1]
}

// msgHas 在会话历史里找包含全部关键词的一条消息。
func msgHas(t *testing.T, a *App, sid string, wants ...string) bool {
	t.Helper()
	msgs, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatalf("读会话 %s 历史失败: %v", sid, err)
	}
	for _, m := range msgs {
		hit := true
		for _, w := range wants {
			if !strings.Contains(m.Text(), w) {
				hit = false
				break
			}
		}
		if hit {
			return true
		}
	}
	return false
}

// 长期助理路径不回归：描述进主对话、不新建任何会话、记录与 lastSessionId
// 都指向主对话（与加执行目标之前的既有行为一致）。
func TestTodoDispatchManagerTargetKeepsLegacyPath(t *testing.T) {
	a, s, mgr := newTodoDispatchApp(t)
	view, err := s.create(todoRequest{
		Title: "日报提醒", Description: "整理今天的工作日报",
		Executor: execAssistant, Target: targetManager,
		Schedule: &Schedule{Kind: schedManual},
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if view.Target != targetManager {
		t.Fatalf("显式 manager 应原样保存, got %q", view.Target)
	}
	task := findTodoTask(t, s, view.ID)
	sid, err := s.dispatch(task, "schedule", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("长期助理路径执行失败: %v", err)
	}
	if sid != mgr.ID {
		t.Fatalf("长期助理路径应把任务交给主对话 %s, got %q", mgr.ID, sid)
	}
	if got := len(todoAllSessions(t, a)); got != 1 {
		t.Fatalf("长期助理路径不该新建会话, got %d 个", got)
	}
	if !msgHas(t, a, mgr.ID, "【待办任务派发｜定时触发】", "整理今天的工作日报") {
		t.Fatal("主对话应收到【待办任务派发】任务描述")
	}
	rec := lastTodoRecord(t, s)
	if rec.Status != "ok" || rec.SessionID != mgr.ID {
		t.Fatalf("执行记录应为 ok 且指向主对话: %+v", rec)
	}
	after := findTodoTask(t, s, view.ID)
	if after.LastSessionID != mgr.ID || after.LastStatus != "ok" {
		t.Fatalf("任务状态应记主对话: last=%q status=%q", after.LastSessionID, after.LastStatus)
	}
}

// 新工作任务路径：createSession 单源建会话（mode=task、顶层、工作区取默认、
// dispatched_by=待办调度器系统派生标记）、描述投进新会话、主对话不被打扰、
// 执行记录与 lastSessionId 指向新会话。
func TestTodoDispatchTaskTargetCreatesWorkSession(t *testing.T) {
	a, s, mgr := newTodoDispatchApp(t)
	view, err := s.create(todoRequest{
		Title: "清理临时目录", Description: "把下载目录里的临时文件归档",
		Executor: execAssistant, Target: targetTask,
		Schedule: &Schedule{Kind: schedManual},
	})
	if err != nil {
		t.Fatalf("建任务失败: %v", err)
	}
	if view.Target != targetTask {
		t.Fatalf("task 目标应原样保存, got %q", view.Target)
	}
	task := findTodoTask(t, s, view.ID)
	sid, err := s.dispatch(task, "schedule", time.Now().UnixMilli())
	if err != nil {
		t.Fatalf("新工作任务路径执行失败: %v", err)
	}
	if sid == "" || sid == mgr.ID {
		t.Fatalf("应新建工作任务会话并返回其 id, got %q", sid)
	}
	sub, err := a.Store.Get(sid)
	if err != nil {
		t.Fatalf("新会话 %s 应已落库: %v", sid, err)
	}
	if sub.Mode != "task" {
		t.Fatalf("新会话模式应为 task, got %q", sub.Mode)
	}
	if sub.ParentID != "" {
		t.Fatalf("定时派生的工作任务是顶层会话, got parent=%q", sub.ParentID)
	}
	if sub.DispatchedBy != todoComponentID {
		t.Fatalf("派生标记应为待办调度器（系统派生）%q, got %q", todoComponentID, sub.DispatchedBy)
	}
	if sub.Workspace != a.Cfg.Workspace {
		t.Fatalf("新会话工作区应为默认工作区 %q, got %q", a.Cfg.Workspace, sub.Workspace)
	}
	if !msgHas(t, a, sid, "【待办任务派发｜定时触发】", "把下载目录里的临时文件归档") {
		t.Fatal("新会话应收到【待办任务派发】任务描述")
	}
	// 长期助理不被打扰：主对话里一条消息都不该多
	if msgs, err := a.Store.RawMessagesAll(mgr.ID); err == nil && len(msgs) > 0 {
		t.Fatalf("长期助理主对话不该收到消息, got %d 条", len(msgs))
	}
	rec := lastTodoRecord(t, s)
	if rec.Status != "ok" || rec.SessionID != sid {
		t.Fatalf("执行记录应为 ok 且指向新会话: %+v", rec)
	}
	if after := findTodoTask(t, s, view.ID); after.LastSessionID != sid {
		t.Fatalf("lastSessionId 应指向新会话: %q", after.LastSessionID)
	}
}

// 执行目标归一：空与未知值回落长期助理；update 能改目标；换成程序任务时清空。
func TestTodoTargetNormalization(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	s := &todoScheduler{app: a, busy: map[string]bool{}, tombs: map[string]bool{},
		data: &TodoData{Version: todoDataVersion, Tasks: []*TaskItem{}, Records: []*RunRecord{}}}

	view, err := s.create(todoRequest{
		Title: "空目标", Description: "x", Executor: execAssistant,
		Schedule: &Schedule{Kind: schedManual},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.Target != targetManager {
		t.Fatalf("空目标应回落长期助理, got %q", view.Target)
	}
	upd, err := s.update(todoRequest{ID: view.ID, Target: targetTask})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Target != targetTask {
		t.Fatalf("update 应能改执行目标, got %q", upd.Target)
	}
	upd, err = s.update(todoRequest{ID: view.ID, Target: "乱写"})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Target != targetManager {
		t.Fatalf("未知目标应回落长期助理, got %q", upd.Target)
	}
	// 换成程序任务：目标清空（需带 command/dir，normalize 校验通过）
	dir := a.Cfg.Workspace
	upd, err = s.update(todoRequest{ID: view.ID, Executor: execCommand, Command: "echo hi", Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	if upd.Target != "" {
		t.Fatalf("程序任务不该带执行目标, got %q", upd.Target)
	}
}

// 模型工具参数同步：todo_task_create / todo_task_update 都有 target 参数（取值
// 与界面一致），「人工有啥 AI 有啥」。
func TestTodoToolsHaveTargetParam(t *testing.T) {
	for _, tl := range []Tool{todoTaskCreateTool(), todoTaskUpdateTool()} {
		props, _ := tl.Spec.Function.Parameters["properties"].(map[string]any)
		spec, ok := props["target"].(map[string]any)
		if !ok {
			t.Fatalf("%s 缺 target 参数", tl.Spec.Function.Name)
		}
		enum, _ := spec["enum"].([]string)
		if len(enum) != 2 || enum[0] != targetManager || enum[1] != targetTask {
			t.Fatalf("%s 的 target 取值应为 [manager task], got %v", tl.Spec.Function.Name, enum)
		}
	}
	if describeTarget(&TaskItem{Executor: execAssistant, Target: targetTask}) != "新的工作任务" {
		t.Fatal("task 目标的中文称呼不对")
	}
	if describeTarget(&TaskItem{Executor: execAssistant}) != "长期助理" {
		t.Fatal("默认目标的中文称呼不对")
	}
}
