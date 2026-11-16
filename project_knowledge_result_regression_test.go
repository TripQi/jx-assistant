package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 模拟模型真实发起 component_call，结果经过 RunAgent 全局 cap 后落库，
// 再由第二次模型请求读回；不能以直调能力或 clipToolResult 代替这条链路。
func projectKnowledgeResultRunAgent(t *testing.T, capability string, args map[string]any, initial string) (*App, *SessionMeta, ProjectKnowledge, string) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	root := t.TempDir()
	dir := filepath.Join(root, projectKnowledgeComponentID)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	manifest, err := os.ReadFile(filepath.Join("组件", projectKnowledgeComponentID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0644); err != nil {
		t.Fatal(err)
	}
	a.Components = InitComponents(root, filepath.Join(t.TempDir(), "components.json"))
	a.SessionCX = loadSessionCX(a.Store)
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.MaxSteps = 3
	pr, err := a.Store.CreateProject(Project{ID: "result-project", Name: "回执测试"})
	if err != nil {
		t.Fatal(err)
	}
	k, err := a.Store.GetProjectKnowledge(pr.ID)
	if err != nil {
		t.Fatal(err)
	}
	limit := int64(40000)
	k, err = a.Store.SaveProjectKnowledge(pr.ID, k.Revision, initial, &limit, projectTestAllow)
	if err != nil {
		t.Fatal(err)
	}
	args["project_id"] = pr.ID
	if capability == "project_knowledge_update" {
		args["expected_revision"] = k.Revision
	}
	callArgs, err := json.Marshal(map[string]any{"component": projectKnowledgeComponentID, "capability": capability, "args": args})
	if err != nil {
		t.Fatal(err)
	}
	var requests atomic.Int32
	modelResult := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if requests.Add(1) == 1 {
			chunk := map[string]any{"choices": []any{map[string]any{"delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "knowledge-result-call", "type": "function", "function": map[string]any{"name": "component_call", "arguments": string(callArgs)}}}}}}}
			data, _ := json.Marshal(chunk)
			fmt.Fprintf(w, "data: %s\n\ndata: [DONE]\n\n", data)
			return
		}
		var req struct {
			Messages []Message `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("解析真实第二跳模型请求: %v", err)
		}
		for _, m := range req.Messages {
			if m.Role == "tool" && m.ToolCallID == "knowledge-result-call" {
				select {
				case modelResult <- m.Text():
				default:
				}
			}
		}
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"已检查回执\"}}]}\n\ndata: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)
	a.Cfg.Providers = []ProviderCfg{{ID: "result-fake", BaseURL: srv.URL, APIKey: "test", Models: []ModelCfg{{ID: "result-model"}}}}
	a.Cfg.DefaultModel = ModelRef{Provider: "result-fake", Model: "result-model"}
	sess := &SessionMeta{ID: "knowledge-result-agent", Mode: "assistant", Workspace: a.Cfg.Workspace, Permission: permFull, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := a.RunAgent(ctx, sess, func(Event) {}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	rows, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var result string
	var count int
	for _, row := range rows {
		if row.Role == "tool" && row.ToolCallID == "knowledge-result-call" {
			result = row.Text()
			count++
		}
	}
	if count != 1 || requests.Load() != 2 {
		t.Fatalf("必须真实执行一次工具并进入第二跳: results=%d requests=%d", count, requests.Load())
	}
	select {
	case got := <-modelResult:
		if got != result {
			t.Fatal("第二跳模型所见与持久化工具结果不一致")
		}
	default:
		t.Fatal("第二跳模型未收到工具结果")
	}
	if len(result) > toolResultMaxBytes || strings.Contains(result, toolResultClipMarker) {
		t.Fatalf("知识结果不应触发全局裁切: bytes=%d", len(result))
	}
	return a, sess, k, result
}

func TestProjectKnowledgeRunAgentOversizedGetRejectsBeforeGlobalCap(t *testing.T) {
	for _, body := range []struct {
		name string
		text string
	}{
		{"140000-ASCII", strings.Repeat("a", 140000)},
		// 原文不足 128KiB，但 JSON 的 HTML 转义会超限，必须按序列化后检测。
		{"JSON-escaping", strings.Repeat("<", 30000)},
	} {
		t.Run(body.name, func(t *testing.T) {
			a, _, before, result := projectKnowledgeResultRunAgent(t, "project_knowledge_get", map[string]any{}, body.text)
			for _, want := range []string{"工具执行失败", "超过单次工具结果容量", "未返回正文", "未截断", "设置 → 项目知识库", "不支持 offset 或分页"} {
				if !strings.Contains(result, want) {
					t.Fatalf("缺少可执行拒绝 %q: %.1000s", want, result)
				}
			}
			if strings.Contains(result, strings.Repeat(body.text[:1], 100)) {
				t.Fatal("拒绝不应泄出部分正文")
			}
			after, err := a.Store.GetProjectKnowledge(before.ProjectID)
			if err != nil || after != before {
				t.Fatalf("获取拒绝修改了知识: %v", err)
			}
		})
	}
}

func TestProjectKnowledgeRunAgentGetKeepsCompleteJSON(t *testing.T) {
	body := "完整开头\n" + strings.Repeat("a", 100000) + "\n完整结尾"
	_, _, before, result := projectKnowledgeResultRunAgent(t, "project_knowledge_get", map[string]any{}, body)
	var got ProjectKnowledge
	if err := json.Unmarshal([]byte(result), &got); err != nil {
		t.Fatalf("获取必须是完整 JSON: %v", err)
	}
	if got.Content != body || got.Revision != before.Revision || got.BudgetLimit != 40000 {
		t.Fatal("可容纳正文不能被裁剪或遗漏元数据")
	}
}

func TestProjectKnowledgeRunAgentUpdateReturnsSmallCommittedReceipt(t *testing.T) {
	body := strings.Repeat("z", 140000)
	a, _, before, result := projectKnowledgeResultRunAgent(t, "project_knowledge_update", map[string]any{"content": body}, "old")
	var receipt map[string]json.RawMessage
	if err := json.Unmarshal([]byte(result), &receipt); err != nil {
		t.Fatalf("写成功回执必须保持合法 JSON: %v", err)
	}
	if _, exists := receipt["content"]; exists {
		t.Fatal("update 回执不得包含正文")
	}
	for _, key := range []string{"project_id", "revision", "updated_at", "budget_limit", "used", "warning", "saved"} {
		if _, ok := receipt[key]; !ok {
			t.Fatalf("回执缺少 %s", key)
		}
	}
	after, err := a.Store.GetProjectKnowledge(before.ProjectID)
	if err != nil || after.Content != body || after.Revision != before.Revision+1 {
		t.Fatalf("必须完整提交且只递增一次: %v", err)
	}
	var revision, used, limit int64
	var saved bool
	_ = json.Unmarshal(receipt["revision"], &revision)
	_ = json.Unmarshal(receipt["used"], &used)
	_ = json.Unmarshal(receipt["budget_limit"], &limit)
	_ = json.Unmarshal(receipt["saved"], &saved)
	if !saved || revision != after.Revision || used != 35000 || limit != 40000 || len(result) > 2000 {
		t.Fatalf("回执与真实提交不一致: %s", result)
	}
}
