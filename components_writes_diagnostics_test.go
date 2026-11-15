package main

import (
	"bytes"
	"encoding/json"
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 经真正加载入口断言日志：旧版覆盖 components.go 后这些测试仍可编译，
// 会因缺少告警而变红，而非依赖新增 helper 导致编译失败。
func TestComponentManifestWritesDiagnostics(t *testing.T) {
	for _, tc := range []struct {
		name, capability, warning string
		writes                    bool
	}{
		{"legacy_read_missing", `{"name":"memory_query"}`, "未显式声明布尔 writes", false},
		{"legacy_write_missing", `{"name":"memory_update"}`, "未显式声明布尔 writes", false},
		{"null_not_explicit_bool", `{"name":"memory_query","writes":null}`, "未显式声明布尔 writes", false},
		{"suspect_false", `{"name":"memory_update","writes":false}`, "名称疑似写操作", false},
		{"suspect_hyphen", `{"name":"item-delete","writes":false}`, "名称疑似写操作", false},
		{"suspect_case", `{"name":"SAVE","writes":false}`, "名称疑似写操作", false},
		{"explicit_read", `{"name":"memory_query","writes":false}`, "", false},
		{"not_substring", `{"name":"asset_query","writes":false}`, "", false},
		{"explicit_write", `{"name":"memory_update","writes":true}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, user := range []bool{false, true} {
				root := t.TempDir()
				dir := filepath.Join(root, "fixture")
				if user {
					dir = filepath.Join(root, userComponentsSubdir, "fixture")
				}
				if err := os.MkdirAll(dir, 0755); err != nil {
					t.Fatal(err)
				}
				path := filepath.Join(dir, "manifest.json")
				original := []byte(`{"id":"fixture","capabilities":[` + tc.capability + `]}`)
				if err := os.WriteFile(path, original, 0644); err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				previous := log.Writer()
				log.SetOutput(&output)
				// 不 parallel：标准 logger 是进程级；捕获仅包围同步初始化。
				var r *ComponentRegistry
				func() {
					defer log.SetOutput(previous)
					r = InitComponents(root, filepath.Join(t.TempDir(), "components.json"))
				}()
				var declared ComponentCapability
				if err := json.Unmarshal([]byte(tc.capability), &declared); err != nil {
					t.Fatal(err)
				}
				cap, ok := r.Capability("fixture", declared.Name)
				if !ok || !r.IsEnabled("fixture") || cap.Writes != tc.writes {
					t.Fatalf("诊断不得拒绝加载或改变 Writes: loaded=%v enabled=%v capability=%+v", ok, r.IsEnabled("fixture"), cap)
				}
				got := output.String()
				if tc.warning == "" {
					if strings.Contains(got, "[writes 诊断]") {
						t.Errorf("正确声明不应告警: %s", got)
					}
				} else {
					for _, want := range []string{"[writes 诊断]", dir + "/manifest.json", `组件 "fixture"`, cap.Name, tc.warning, "未自动修改清单"} {
						if !strings.Contains(got, want) {
							t.Errorf("日志缺少 %q: %s", want, got)
						}
					}
				}
				after, err := os.ReadFile(path)
				if err != nil || !bytes.Equal(original, after) {
					t.Fatalf("加载不得修改 manifest: %v", err)
				}
			}
		})
	}
}

func TestComponentManifestWritesDiagnosticsReadPermissions(t *testing.T) {
	const id = "manifest-writes-read-fixture"
	root := t.TempDir()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	// 一条旧声明和一条显式只读声明，都不应被诊断变成写能力。
	manifest := `{"id":"` + id + `","capabilities":[{"name":"legacy_query"},{"name":"explicit_query","writes":false}]}`
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	a := &App{Cfg: DefaultConfig(), Components: InitComponents(root, filepath.Join(t.TempDir(), "components.json"))}
	for _, name := range []string{"legacy_query", "explicit_query"} {
		registerComponentCapability(id, name, func(_ *toolContext, _ json.RawMessage) (string, error) {
			return "read", nil
		})
	}
	t.Cleanup(func() {
		compCapRunsMu.Lock()
		delete(compCapRuns, id)
		compCapRunsMu.Unlock()
	})
	for _, permission := range []string{permReadOnly, permWorkspace, permFull} {
		for _, plan := range []bool{false, true} {
			for _, name := range []string{"legacy_query", "explicit_query"} {
				sess := &SessionMeta{Mode: "task", Permission: permission, PlanMode: plan}
				out, err := runComponentCall(&toolContext{ctx: t.Context(), app: a, sess: sess},
					json.RawMessage(`{"component":"`+id+`","capability":"`+name+`"}`))
				if err != nil || out != "read" {
					t.Errorf("permission=%s plan=%v capability=%s: %q %v", permission, plan, name, out, err)
				}
			}
		}
	}
}
