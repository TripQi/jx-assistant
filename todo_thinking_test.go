package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func todoThinkingPtr(s string) *string { return &s }

func TestTodoThinkingStrictValues(t *testing.T) {
	for _, level := range append([]string{""}, thinkingLevels()...) {
		if err := validateTodoThinking(level); err != nil {
			t.Errorf("%q: %v", level, err)
		}
	}
	for _, level := range []string{"enabled", "disabled", "HIGH", " high", "high ", "unknown"} {
		if err := validateTodoThinking(level); err == nil || !strings.Contains(err.Error(), "thinking") {
			t.Errorf("应明确拒绝 %q: %v", level, err)
		}
	}
}

func TestTodoThinkingCreateUpdateAndClear(t *testing.T) {
	_, s := newDoneTestApp(t)
	for _, mode := range []string{"dispatch", "global", "custom"} {
		req := todoRequest{Title: "思考档位", Description: "x", Schedule: &Schedule{Kind: schedManual}, ModelMode: mode, Thinking: todoThinkingPtr("high")}
		if mode == "custom" {
			req.Model = &ModelRef{Provider: "test", Model: "test"}
		}
		v, err := s.create(req)
		if err != nil {
			t.Fatal(err)
		}
		if v.Thinking != "high" {
			t.Fatalf("%s 未保存档位: %+v", mode, v)
		}
		v, err = s.update(todoRequest{ID: v.ID, Title: "只改标题"})
		if err != nil || v.Thinking != "high" {
			t.Fatalf("省略应保留: %+v %v", v, err)
		}
		if _, err = s.update(todoRequest{ID: v.ID, Thinking: todoThinkingPtr("HIGH")}); err == nil {
			t.Fatal("非法值应失败")
		}
		if got := findTodoTask(t, s, v.ID); got.Thinking != "high" {
			t.Fatal("失败更新不应修改任务")
		}
		var clear todoRequest
		if err := json.Unmarshal([]byte(`{"id":"`+v.ID+`","thinking":""}`), &clear); err != nil {
			t.Fatal(err)
		}
		v, err = s.update(clear)
		if err != nil || v.Thinking != "" {
			t.Fatalf("空串应清除: %+v %v", v, err)
		}
	}
	if _, err := s.create(todoRequest{Title: "非法", Description: "x", Schedule: &Schedule{Kind: schedManual}, Thinking: todoThinkingPtr("enabled")}); err == nil {
		t.Fatal("新建也必须严格校验")
	}
}

func TestTodoThinkingMigrationKeepsOldEmpty(t *testing.T) {
	_, s := newDoneTestApp(t)
	var d TodoData
	if err := json.Unmarshal([]byte(`{"version":2,"tasks":[{"id":"old","modelMode":"global"}]}`), &d); err != nil {
		t.Fatal(err)
	}
	s.migrateData(&d)
	if d.Version != todoDataVersion || d.Tasks[0].Thinking != "" {
		t.Fatalf("旧任务应保持空档位: %+v", d)
	}
	raw := s.app.Store.GetGlobalData(todoDataKey)
	if !strings.Contains(raw, `"version":3`) || strings.Contains(raw, `"thinking"`) {
		t.Fatalf("迁移落盘不符: %s", raw)
	}
}

func TestTodoThinkingToolCreateUpdate(t *testing.T) {
	a, s := newDoneTestApp(t)
	a.Todos = s
	ctx := &toolContext{app: a}
	out, err := runTodoTaskCreate(ctx, json.RawMessage(`{"title":"工具档位","description":"x","schedule":{"kind":"manual"},"model_mode":"global","thinking":"max"}`))
	if err != nil || !strings.Contains(out, "已新建") {
		t.Fatalf("新建失败: %s %v", out, err)
	}
	if len(s.data.Tasks) != 1 || s.data.Tasks[0].Thinking != "max" {
		t.Fatal("工具未传递档位")
	}
	id := s.data.Tasks[0].ID
	out, err = runTodoTaskUpdate(ctx, json.RawMessage(`{"task_id":"`+id+`","thinking":""}`))
	if err != nil || !strings.Contains(out, "已更新") || s.data.Tasks[0].Thinking != "" {
		t.Fatalf("工具清除失败: %s %v", out, err)
	}
	out, err = runTodoTaskUpdate(ctx, json.RawMessage(`{"task_id":"`+id+`","thinking":"enabled"}`))
	if err != nil || !strings.Contains(out, "更新失败") {
		t.Fatalf("工具应明确拒绝非法档位: %s %v", out, err)
	}
}

func TestTodoThinkingToolSchemas(t *testing.T) {
	for _, tool := range []Tool{todoTaskCreateTool(), todoTaskUpdateTool()} {
		b, err := json.Marshal(tool.Spec)
		if err != nil {
			t.Fatal(err)
		}
		for _, field := range []string{`"thinking"`, `"model_mode"`, `"model"`} {
			if !strings.Contains(string(b), field) {
				t.Errorf("工具缺字段 %s: %s", field, b)
			}
		}
		for _, level := range thinkingLevels() {
			if !strings.Contains(string(b), `"`+level+`"`) {
				t.Errorf("缺档位 %s", level)
			}
		}
	}
}
