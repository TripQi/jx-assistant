package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 对全部静态 AI 工具声明做穷举审计，不靠工具名猜写行为。
// 新 toolSpec 未分类即失败；新增副作用入口必须同时登记它实际使用的门。
// internal 仅指会话编排/展示，不授予用户文件或外部服务的写权限；不是写门豁免。
func TestReadOnlyWriteEntryInventory(t *testing.T) {
	policies := map[string]string{}
	add := func(policy, names string) {
		for _, n := range strings.Fields(names) {
			if _, ok := policies[n]; ok {
				t.Fatalf("重复政策 %s", n)
			}
			policies[n] = policy
		}
	}
	add("read", "read list_dir glob grep web_fetch read_image list_sessions read_session_summary list_experts search_sessions read_session_messages job_output job_list panel_screenshot read_skill todo_task_list todo_task_get todo_workspace_list browser_content browser_screenshot browser_scroll browser_viewport browser_close device_screenshot device_ui adb_screencap web_search office_skill")
	add("sandbox", "write edit run_command run_python pdf ocr")
	add("host", "mcp_read_connector mcp_call_tool register_skill git_sync device_tap device_swipe device_type device_key device_app adb_connect adb_shell todo_task_create todo_task_update todo_task_delete todo_task_run browser_click browser_type browser_evaluate")
	add("conditional", "component_call deliver_result call_api browser_open")
	add("internal", "dispatch_task dispatch_sub send_to_session stop_session request_manager ask_user_question request_permission enter_plan_mode exit_plan_mode update_goal progress_write job_kill")
	add("snapshot-exemption", "snapshot_file")
	exempt := 0
	for name, policy := range policies {
		if policy == "snapshot-exemption" {
			exempt++
			if name != "snapshot_file" {
				t.Fatalf("未经批准的写豁免: %s", name)
			}
		}
	}
	if exempt != 1 {
		t.Fatal("唯一写豁免必须是 snapshot_file")
	}
	root := "."
	if dir := os.Getenv("JX_READONLY_AUDIT_ROOT"); dir != "" {
		root = dir // 仅测试源码审计的修前对照目录，不参与运行时权限判断。
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	bodies := map[string]*ast.BlockStmt{}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(root, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			if fn, ok := d.(*ast.FuncDecl); ok && fn.Body != nil {
				bodies[fn.Name.Name] = fn.Body
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok || id.Name != "toolSpec" || len(call.Args) == 0 {
				return true
			}
			lit, ok := call.Args[0].(*ast.BasicLit)
			if !ok {
				t.Errorf("动态 toolSpec 需要显式审计: %s", name)
				return true
			}
			tool, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatal(err)
			}
			seen[tool] = true
			if policies[tool] == "" {
				t.Errorf("新 AI 入口 %q 未登记只读门控政策；必须审计副作用并加门，不能默认只读", tool)
			}
			return true
		})
	}
	for tool := range policies {
		if !seen[tool] {
			t.Errorf("过期门控清单: %s", tool)
		}
	}
	// 直接走宿主的这批写入口必须调用统一门，不能仅靠能力或审批。
	for _, fn := range []string{"runTodoTaskCreate", "runTodoTaskUpdate", "runTodoTaskDelete", "runTodoTaskRun", "browserClickTool", "browserTypeTool", "browserEvaluateTool", "runDeliverResult", "runDeliveryJob", "runComponentCall", "browserOpenTool"} {
		found := false
		if body := bodies[fn]; body != nil {
			ast.Inspect(body, func(n ast.Node) bool {
				if c, ok := n.(*ast.CallExpr); ok {
					if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "requireNotReadOnly" {
						found = true
					}
				}
				return true
			})
		}
		if !found {
			t.Errorf("写入口 %s 缺少 requireNotReadOnly", fn)
		}
	}
	body := bodies["runSnapshotFile"]
	if body == nil {
		t.Fatal("snapshot 实现丢失")
	}
	ast.Inspect(body, func(n ast.Node) bool {
		if c, ok := n.(*ast.CallExpr); ok {
			if id, ok := c.Fun.(*ast.Ident); ok && id.Name == "requireNotReadOnly" {
				t.Error("snapshot_file 是唯一显式写豁免，禁止误加只读门")
			}
		}
		return true
	})
	t.Logf("已审计 %d 个静态 AI 工具；唯一写豁免 snapshot_file", len(seen))
}
