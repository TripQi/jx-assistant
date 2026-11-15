package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 第二个组件使用隔离宿主计数器，防止门控退化成只针对 user-memory 的特例。
func TestComponentWriteGateAnyComponent(t *testing.T) {
	a := newComponentWriteGateApp(t)
	const id = "write-gate-fixture"
	manifest := `{"id":"write-gate-fixture","name":"隔离写门组件","capabilities":[{"name":"write","writes":true},{"name":"read"},{"name":"uninstalled"}]}`
	path := filepath.Join(a.Components.dir, id, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(manifest), 0644); err != nil {
		t.Fatal(err)
	}
	a.Components.Rescan()
	writes := 0
	registerComponentCapability(id, "write", func(_ *toolContext, _ json.RawMessage) (string, error) {
		writes++
		return "written", nil
	})
	registerComponentCapability(id, "read", func(_ *toolContext, _ json.RawMessage) (string, error) {
		return "read", nil
	})
	t.Cleanup(func() {
		compCapRunsMu.Lock()
		delete(compCapRuns, id)
		compCapRunsMu.Unlock()
	})
	sess := &SessionMeta{Mode: "task", Permission: permReadOnly}
	call := func(cap string) (string, error) {
		return runComponentCall(&toolContext{ctx: t.Context(), app: a, sess: sess},
			json.RawMessage(`{"component":"`+id+`","capability":"`+cap+`"}`))
	}
	if _, err := call("write"); err == nil || !strings.Contains(err.Error(), "request_permission") {
		t.Errorf("任意组件写能力都应受统一只读门约束: %v", err)
	}
	if writes != 0 {
		t.Errorf("拒绝必须发生在宿主实现之前，实际调用 %d 次", writes)
	}
	if out, err := call("read"); err != nil || out != "read" {
		t.Errorf("任意组件读能力应正常: %q %v", out, err)
	}
	if _, err := call("uninstalled"); err == nil || !strings.Contains(err.Error(), "宿主没有安装") {
		t.Errorf("未安装实现仍应走既有拒绝路径: %v", err)
	}
	for _, perm := range []string{permWorkspace, permFull} {
		sess.Permission = perm
		if out, err := call("write"); err != nil || out != "written" {
			t.Errorf("%s 写能力应可用: %q %v", perm, out, err)
		}
	}
	if writes != 2 {
		t.Errorf("仅两次非只读调用可到达实现，实际 %d 次", writes)
	}
}
