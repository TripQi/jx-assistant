package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 仅复制组件声明；数据和组件开关都落临时目录，不启动运行时或读取真实配置。
func newComponentWriteGateApp(t *testing.T) *App {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	a := &App{Cfg: cfg}
	src, err := os.ReadFile(filepath.Join("组件", userMemoryComponentID, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	compDir := filepath.Join(dir, "components")
	dst := filepath.Join(compDir, userMemoryComponentID, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, src, 0644); err != nil {
		t.Fatal(err)
	}
	a.Components = InitComponents(compDir, filepath.Join(dir, "components.json"))
	if _, err := a.saveUserMemory(UserMemoryData{UserName: "隔离用户", Memory: "原始记忆", AIName: "隔离助理"}); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestComponentWriteGatePermissions(t *testing.T) {
	for _, tc := range []struct {
		name, permission, wantError string
		plan                        bool
	}{
		{"read_only", permReadOnly, "全电脑只读", false},
		{"workspace", permWorkspace, "", false},
		{"full", permFull, "", false},
		{"workspace_plan", permWorkspace, "计划模式下暂停", true},
		{"full_plan", permFull, "计划模式下暂停", true},
		{"read_only_plan", permReadOnly, "计划模式下暂停", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newComponentWriteGateApp(t)
			sess := &SessionMeta{Mode: "task", Permission: tc.permission, PlanMode: tc.plan}
			before, err := os.ReadFile(a.userMemoryPath())
			if err != nil {
				t.Fatal(err)
			}
			out, callErr := runMemoryCap(t, a, sess, "memory_update", `{"memory":"新记忆"}`)
			after, err := os.ReadFile(a.userMemoryPath())
			if err != nil {
				t.Fatal(err)
			}
			if tc.wantError != "" {
				if callErr == nil || !strings.Contains(callErr.Error(), tc.wantError) {
					t.Errorf("写能力应拒绝并含 %q，实际 output=%q err=%v", tc.wantError, out, callErr)
				} else {
					guidance := []string{"request_permission", "workspace-write", "justification", "权限芯片", "修改工作区"}
					if tc.plan {
						guidance = []string{"先用只读能力调研", "exit_plan_mode", "获批后"}
					}
					for _, want := range guidance {
						if !strings.Contains(callErr.Error(), want) {
							t.Errorf("拒绝文案缺少指路 %q: %v", want, callErr)
						}
					}
					t.Logf("写能力拒绝文案: %v", callErr)
				}
				if !bytes.Equal(before, after) {
					t.Error("写能力被拒后记忆文件必须逐字节不变")
				}
			} else {
				if callErr != nil || !strings.Contains(out, "已更新") {
					t.Fatalf("写能力应成功: output=%q err=%v", out, callErr)
				}
				d := parseUserMemoryDoc(string(after))
				if d.Memory != "新记忆" || d.UserName != "隔离用户" || d.AIName != "隔离助理" {
					t.Fatalf("写能力落盘内容不符: %+v", d)
				}
			}
			// 写入被拦不能连读能力一起封禁；查询所有档位（包括计划模式）。
			out, err = runMemoryCap(t, a, sess, "memory_query", "")
			if err != nil {
				t.Fatalf("读能力必须照常可用: %v", err)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatal(err)
			}
			want := "原始记忆"
			if tc.wantError == "" {
				want = "新记忆"
			}
			if got["userMemory"] != want || got["userName"] != "隔离用户" {
				t.Errorf("读能力返回错误: %s", out)
			}
			if tc.permission == permReadOnly && tc.plan {
				// 计划获批后，只读限制仍须独立生效。
				sess.PlanMode = false
				if _, err := runMemoryCap(t, a, sess, "memory_update", `{"memory":"退出计划后"}`); err == nil || !strings.Contains(err.Error(), "request_permission") {
					t.Errorf("退出计划后仍应受只读门限制: %v", err)
				}
				final, err := os.ReadFile(a.userMemoryPath())
				if err != nil || !bytes.Equal(before, final) {
					t.Errorf("退出计划不应放行只读写入: err=%v", err)
				}
			}
		})
	}
}

func TestComponentWriteGateExistingRejections(t *testing.T) {
	for _, tc := range []struct {
		name, component, capability, mode, want string
		disabled                                bool
	}{
		{"disabled", userMemoryComponentID, "memory_update", "task", "未启用", true},
		{"missing_component", "absent-component", "memory_update", "task", "没有 id", false},
		{"missing_capability", userMemoryComponentID, "absent-capability", "task", "没有声明能力", false},
		{"mode_denied", userMemoryComponentID, "memory_update", "aux", "可用模式", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := newComponentWriteGateApp(t)
			if tc.disabled {
				if err := a.Components.SetEnabled(userMemoryComponentID, false); err != nil {
					t.Fatal(err)
				}
			}
			raw, err := json.Marshal(map[string]any{"component": tc.component, "capability": tc.capability, "args": map[string]string{"memory": "不可写入"}})
			if err != nil {
				t.Fatal(err)
			}
			_, err = runComponentCall(&toolContext{ctx: t.Context(), app: a, sess: &SessionMeta{Mode: tc.mode, Permission: permReadOnly}}, raw)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("既有拒绝路径应含 %q: %v", tc.want, err)
			}
			if a.loadUserMemory().Memory != "原始记忆" {
				t.Fatal("既有拒绝路径不得落盘")
			}
		})
	}
}
