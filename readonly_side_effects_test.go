package main

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func assertReadOnlyGuidance(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("只读写入口未拒绝")
	}
	for _, s := range []string{"全电脑只读", "request_permission", "workspace-write", "justification", "权限芯片"} {
		if !strings.Contains(err.Error(), s) {
			t.Errorf("拒绝缺少指路 %q: %v", s, err)
		}
	}
}

func TestReadOnlyTodoSideEffects(t *testing.T) {
	for _, permission := range []string{permReadOnly, permWorkspace, permFull} {
		t.Run(permission, func(t *testing.T) {
			a, s := newDoneTestApp(t)
			a.Todos = s
			tc := &toolContext{ctx: t.Context(), app: a, sess: &SessionMeta{ID: "todo-gate", Mode: "task", Permission: permission}}
			out, err := runTodoTaskCreate(tc, json.RawMessage(`{"title":"fixture","description":"fixture","schedule":{"kind":"manual"}}`))
			if permission == permReadOnly {
				assertReadOnlyGuidance(t, err)
				if len(s.data.Tasks) != 0 {
					t.Fatal("拒绝后创建了任务")
				}
			} else if err != nil || !strings.Contains(out, "已新建") {
				t.Fatalf("创建: %s %v", out, err)
			}
			if len(s.data.Tasks) == 0 {
				_, err = s.create(todoRequest{Title: "fixture", Description: "fixture", Schedule: &Schedule{Kind: schedManual}})
				if err != nil {
					t.Fatal(err)
				}
			}
			id := s.data.Tasks[0].ID
			for _, tool := range []Tool{todoTaskListTool(), todoTaskGetTool()} {
				raw, _ := json.Marshal(map[string]string{"task_id": id})
				out, err := tool.Run(tc, raw)
				if err != nil || !strings.Contains(out, "fixture") {
					t.Fatalf("读待办失败: %s %v", out, err)
				}
			}
			raw, _ := json.Marshal(map[string]string{"task_id": id, "title": "changed"})
			out, err = runTodoTaskUpdate(tc, raw)
			if permission == permReadOnly {
				assertReadOnlyGuidance(t, err)
				if s.data.Tasks[0].Title != "fixture" {
					t.Fatal("拒绝后任务被改写")
				}
			} else if err != nil || !strings.Contains(out, "已更新") {
				t.Fatalf("更新: %s %v", out, err)
			}
			// 已取消的独立 ctx：允许档仍到达原审批链并拒绝，绝不运行真实任务。
			ctx, cancel := context.WithCancel(t.Context())
			cancel()
			tc.ctx = ctx
			a.approvals = map[string]*Approval{}
			for _, tool := range []Tool{todoTaskDeleteTool(), todoTaskRunTool()} {
				out, err = tool.Run(tc, raw)
				if permission == permReadOnly {
					assertReadOnlyGuidance(t, err)
				} else if err != nil || !strings.Contains(out, "用户没有批准") {
					t.Fatalf("审批仍应有效: %s %v", out, err)
				}
				if len(s.data.Tasks) != 1 {
					t.Fatal("未批准时任务改变")
				}
			}
			t.Log("待办读可用；写门与操作审批独立")
		})
	}
}

type sideEffectBrowser struct {
	fakeBrowserTarget
	writes, reads int
}

func (f *sideEffectBrowser) Click(string) error                  { f.writes++; return nil }
func (f *sideEffectBrowser) TypeInto(string, string, bool) error { f.writes++; return nil }
func (f *sideEffectBrowser) Evaluate(string) (any, error)        { f.writes++; return "ok", nil }
func (f *sideEffectBrowser) Content() (string, string, string, error) {
	f.reads++
	return "https://fixture.invalid", "fixture", "readable", nil
}
func (f *sideEffectBrowser) Navigate(string) (string, string, error) {
	f.reads++
	return "https://fixture.invalid", "fixture", nil
}
func (f *sideEffectBrowser) Screenshot() ([]byte, error) {
	f.reads++
	var b bytes.Buffer
	err := png.Encode(&b, image.NewRGBA(image.Rect(0, 0, 1, 1)))
	return b.Bytes(), err
}

func TestReadOnlyBrowserSideEffects(t *testing.T) {
	for _, permission := range []string{permReadOnly, permWorkspace, permFull} {
		t.Run(permission, func(t *testing.T) {
			a := newCompactTestApp(t)
			sess := &SessionMeta{ID: "browser-gate", Mode: "task", Permission: permission, Tools: []string{capBrowser}}
			f := &sideEffectBrowser{fakeBrowserTarget: fakeBrowserTarget{started: true}}
			key := browserProfileKey(sess.ID)
			a.browserActive = map[string]browserTarget{key: f}
			a.panels = map[string]browserTarget{key: f}
			a.panelDispatch = func(fn func()) { fn() }
			tc := &toolContext{ctx: t.Context(), app: a, sess: sess}
			for _, tool := range []Tool{browserClickTool(), browserTypeTool(), browserEvaluateTool()} {
				_, err := tool.Run(tc, json.RawMessage(`{"selector":"input","text":"x","js":"fetch('/write')","request_capability":true,"justification":"fixture"}`))
				if permission == permReadOnly {
					assertReadOnlyGuidance(t, err)
				} else if err != nil {
					t.Fatal(err)
				}
			}
			if permission == permReadOnly && f.writes != 0 {
				t.Fatal("只读写动作已触达浏览器")
			}
			out, err := browserContentTool().Run(tc, json.RawMessage(`{}`))
			if err != nil || !strings.Contains(out, "readable") {
				t.Fatalf("读网页: %s %v", out, err)
			}
			_, err = browserOpenTool().Run(tc, json.RawMessage(`{"url":"https://fixture.invalid"}`))
			if err != nil {
				t.Fatal(err)
			}
			_, err = browserScreenshotTool().Run(tc, json.RawMessage(`{}`))
			if err != nil {
				t.Fatalf("只读截图不可用: %v", err)
			}
			if permission == permReadOnly {
				_, err = browserOpenTool().Run(tc, json.RawMessage(`{"url":"https://fixture.invalid","grants":["notifications@https://fixture.invalid"]}`))
				assertReadOnlyGuidance(t, err)
			}
			if f.reads != 3 {
				t.Fatalf("读/导航未到达假浏览器: %d", f.reads)
			}
			t.Logf("浏览器 writes=%d reads=%d", f.writes, f.reads)
		})
	}
	// 能力关闭也不能靠申请能力绕过只读门（不触发审批）。
	a := newCompactTestApp(t)
	a.approvals = map[string]*Approval{}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	for _, tool := range []Tool{browserClickTool(), browserTypeTool(), browserEvaluateTool()} {
		_, err := tool.Run(&toolContext{app: a, ctx: ctx, sess: &SessionMeta{Permission: permReadOnly}}, json.RawMessage(`{"request_capability":true,"justification":"fixture"}`))
		assertReadOnlyGuidance(t, err)
	}
}

func TestReadOnlyDeliverySideEffects(t *testing.T) {
	for _, permission := range []string{permReadOnly, permWorkspace, permFull} {
		t.Run(permission, func(t *testing.T) {
			a, tc, sess, ws := deliverFixture(t)
			sess.Permission = permission
			if err := a.Store.SaveMeta(sess); err != nil {
				t.Fatal(err)
			}
			up := seedUpload(t, a, sess, ws, "original.txt", "original")
			source := filepath.Join(ws, "result.txt")
			if err := os.WriteFile(source, []byte("result"), 0644); err != nil {
				t.Fatal(err)
			}
			deliverCall(t, tc, map[string]any{"path": source})
			if len(deliveryPaths(t, a, sess.ID)) != 1 {
				t.Fatal("只登记卡片应可用")
			}
			raw, _ := json.Marshal(map[string]any{"files": []map[string]any{{"path": source, "upload": "u1", "filename": "original.txt"}}})
			_, err := runDeliverResult(tc, raw)
			if permission == permReadOnly {
				assertReadOnlyGuidance(t, err)
				if len(deliveryPaths(t, a, sess.ID)) != 1 {
					t.Fatal("拒绝后仍登记搬运卡片")
				}
			} else if err != nil {
				t.Fatal(err)
			}
			// 执行旧作业也必须查当前权限；覆盖启动恢复入口，目标含旧版文件。
			j := &DeliveryJob{SessionID: sess.ID, SourcePath: source, TargetDir: filepath.Dir(up.OrigPath), TargetName: "original.txt", Status: "pending", CreatedAt: now()}
			a.runDeliveryJob(j)
			b, err := os.ReadFile(up.OrigPath)
			if err != nil {
				t.Fatal(err)
			}
			if permission == permReadOnly {
				if j.Status != "failed" || !strings.Contains(j.Error, "request_permission") || string(b) != "original" {
					t.Fatalf("只读作业产生写入: %+v %s", j, b)
				}
				if _, err := os.Stat(source); err != nil {
					t.Fatal("拒绝后源被删")
				}
			} else if j.Status != "done" || string(b) != "result" {
				t.Fatalf("写档搬运应照常: %+v %s", j, b)
			}
			t.Logf("卡片可用；搬运终态=%s", j.Status)
		})
	}
	t.Run("downgrade_before_flush", func(t *testing.T) {
		a, tc, sess, ws := deliverFixture(t)
		sess.Permission = permWorkspace
		seedUpload(t, a, sess, ws, "original.txt", "original")
		p := filepath.Join(ws, "result.txt")
		if err := os.WriteFile(p, []byte("result"), 0644); err != nil {
			t.Fatal(err)
		}
		deliverCall(t, tc, map[string]any{"path": p, "upload": "u1", "filename": "original.txt"})
		sess.Permission = permReadOnly
		if err := a.Store.SaveMeta(sess); err != nil {
			t.Fatal(err)
		}
		a.flushDeliveries(sess.ID)
		if _, err := os.Stat(p); err != nil {
			t.Fatal("切回只读后仍搬运")
		}
		if !hasReportNote(t, a, sess.ID, "全电脑只读") {
			t.Fatal("缺少执行期拒绝注记")
		}
	})
}

func TestReadOnlyMissingSessionFailsClosed(t *testing.T) {
	if got := permModeOf(nil); got != permReadOnly {
		t.Fatalf("缺失会话必须按只读处理: %q", got)
	}
	for _, tc := range []*toolContext{nil, {}} {
		if err := requireNotReadOnly(tc, "测试写入"); err == nil || !strings.Contains(err.Error(), "缺少会话权限上下文") {
			t.Fatalf("缺失上下文必须可读拒绝: %v", err)
		}
	}
	_, err := runTodoTaskCreate(&toolContext{}, json.RawMessage(`{}`))
	if err == nil || !strings.Contains(err.Error(), "缺少会话权限上下文") {
		t.Fatalf("待办缺失会话必须在副作用前拒绝: %v", err)
	}
}

func TestReadOnlySnapshotExemption(t *testing.T) {
	for _, permission := range []string{permReadOnly, permWorkspace, permFull} {
		t.Run(permission, func(t *testing.T) {
			_, tc, sess, ws := deliverFixture(t)
			sess.Permission = permission
			tc.ctx = t.Context()
			tc.toolName = "snapshot_file"
			p := filepath.Join(ws, "fixture.txt")
			if err := os.WriteFile(p, []byte("untouched"), 0644); err != nil {
				t.Fatal(err)
			}
			out, err := readFileTool().Run(tc, json.RawMessage(`{"files":[{"path":"fixture.txt"}]}`))
			if err != nil || !strings.Contains(out, "untouched") {
				t.Fatalf("读文件: %s %v", out, err)
			}
			out, err = runSnapshotFile(tc, json.RawMessage(`{"path":"fixture.txt"}`))
			if err != nil || !strings.Contains(out, "已记录") {
				t.Fatalf("快照豁免: %s %v", out, err)
			}
			b, err := os.ReadFile(p)
			if err != nil || string(b) != "untouched" {
				t.Fatal("快照改变用户文件")
			}
			t.Log("读文件与内部快照均可用，用户文件不变")
		})
	}
}
