package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func projectCapabilityTestApp(t *testing.T) *App {
	t.Helper()
	s := projectTestStore(t)
	root := t.TempDir()
	src, e := os.ReadFile(filepath.Join("组件", projectKnowledgeComponentID, "manifest.json"))
	if e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(root, projectKnowledgeComponentID)
	if e = os.MkdirAll(dir, 0755); e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(dir, "manifest.json"), src, 0644); e != nil {
		t.Fatal(e)
	}
	return &App{Store: s, Components: InitComponents(root, filepath.Join(t.TempDir(), "components.json"))}
}
func projectBindTest(t *testing.T, a *App, id, dir string) Project {
	t.Helper()
	w, e := normalizeProjectWorkspace("local", dir)
	if e != nil {
		t.Fatal(e)
	}
	p, e := a.Store.CreateProject(Project{ID: id, Name: id, Workspaces: []ProjectWorkspace{w}})
	if e != nil {
		t.Fatal(e)
	}
	return p
}
func callProjectTest(a *App, s *SessionMeta, cap string, args any) (string, error) {
	raw, _ := json.Marshal(map[string]any{"component": projectKnowledgeComponentID, "capability": cap, "args": args})
	return runComponentCall(&toolContext{app: a, sess: s, ctx: context.Background()}, raw)
}
func TestProjectPathBoundaryLongestAndNamespaces(t *testing.T) {
	a := projectCapabilityTestApp(t)
	base := t.TempDir()
	root := filepath.Join(base, "app")
	nested := filepath.Join(root, "module")
	outer := projectBindTest(t, a, "outer", root)
	inner := projectBindTest(t, a, "inner", nested)
	cases := []struct{ dir, want string }{{root, outer.ID}, {filepath.Join(root, "src"), outer.ID}, {root + "-old", ""}, {nested, inner.ID}, {filepath.Join(nested, "deep"), inner.ID}, {base, ""}}
	for _, tc := range cases {
		p, e := a.resolveSessionProject(&SessionMeta{Workspace: tc.dir})
		if e != nil {
			t.Fatal(e)
		}
		got := ""
		if p != nil {
			got = p.ID
		}
		if got != tc.want {
			t.Fatalf("%s => %s want %s", tc.dir, got, tc.want)
		}
	}
	inner.Archived = true
	if _, e := a.Store.UpdateProject(inner); e != nil {
		t.Fatal(e)
	}
	p, e := a.resolveSessionProject(&SessionMeta{Workspace: nested})
	if e != nil || p == nil || p.ID != inner.ID || !p.Archived {
		t.Fatalf("archived fallback: %+v %v", p, e)
	}
	w1, e := normalizeProjectWorkspace("remote", "root@EXAMPLE.COM:/srv/app/../app/")
	if e != nil {
		t.Fatal(e)
	}
	w2, e := normalizeProjectWorkspace("remote", "root@example.com:22:/srv/app")
	if e != nil {
		t.Fatal(e)
	}
	if w1.WorkspaceKey != w2.WorkspaceKey {
		t.Fatalf("same target differs: %+v %+v", w1, w2)
	}
	w3, _ := normalizeProjectWorkspace("remote", "root@other:/srv/app")
	w4, _ := normalizeProjectWorkspace("remote", "other@example.com:/srv/app")
	if projectWorkspaceContains(w1, w3) || projectWorkspaceContains(w1, w4) || projectWorkspaceContains(ProjectWorkspace{WorkspaceKind: "local", WorkspaceKey: w1.WorkspaceKey}, w1) {
		t.Fatal("namespace collision")
	}
	if _, e = a.Store.CreateProject(Project{Name: "remote", Workspaces: []ProjectWorkspace{w1}}); e != nil {
		t.Fatal(e)
	}
	if _, e = a.Store.CreateProject(Project{Name: "duplicate", Workspaces: []ProjectWorkspace{w2}}); e == nil {
		t.Fatal("normalized UNIQUE missing")
	}
	if isWindowsRuntime {
		x, _ := normalizeProjectWorkspace("local", strings.ToUpper(root)+`\`)
		y, _ := normalizeProjectWorkspace("local", root)
		if x.WorkspaceKey != y.WorkspaceKey {
			t.Fatal("windows casing/separator")
		}
	}
	if p, e = a.resolveSessionProject(&SessionMeta{}); p != nil || e != nil {
		t.Fatalf("empty must not fallback: %+v %v", p, e)
	}
}
func TestProjectCapabilityPermissionMatrix(t *testing.T) {
	for _, mode := range []string{"assistant", "task", "office", "coding", "beginner-coding", "skill-create", "aux"} {
		for _, permission := range []string{permReadOnly, permWorkspace, permFull} {
			for _, plan := range []bool{false, true} {
				name := mode + "/" + permission
				if plan {
					name += "/plan"
				}
				t.Run(name, func(t *testing.T) {
					a := projectCapabilityTestApp(t)
					root := t.TempDir()
					p := projectBindTest(t, a, "own", root)
					other := projectBindTest(t, a, "other", t.TempDir())
					sess := &SessionMeta{ID: "session-test", Mode: mode, Workspace: root, Permission: permission, PlanMode: plan}
					if err := a.Store.Create(sess); err != nil {
						t.Fatal(err)
					}
					before := projectTestKnowledge(t, a.Store, p.ID)
					out, e := callProjectTest(a, sess, "project_knowledge_get", map[string]any{"project_id": p.ID})
					if e != nil || !strings.Contains(out, p.ID) {
						t.Fatalf("read blocked %s %v", out, e)
					}
					_, e = callProjectTest(a, sess, "project_knowledge_get", map[string]any{"project_id": other.ID})
					if (e == nil) != (mode == "assistant") {
						t.Fatalf("cross project read: %v", e)
					}
					out, e = callProjectTest(a, sess, "project_knowledge_update", map[string]any{"project_id": p.ID, "expected_revision": before.Revision, "content": "new"})
					allowed := mode == "assistant" && permission != permReadOnly && !plan
					if (e == nil) != allowed {
						t.Fatalf("write allowed=%v out=%s err=%v", allowed, out, e)
					}
					after := projectTestKnowledge(t, a.Store, p.ID)
					if allowed {
						if after.Content != "new" || after.Revision != before.Revision+1 {
							t.Fatal("write not persisted")
						}
					} else if after != before {
						t.Fatal("denied write mutated state")
					}
				})
			}
		}
	}
}
func TestProjectCapabilitiesOwnershipArchiveSwitchAndThreshold(t *testing.T) {
	a := projectCapabilityTestApp(t)
	root := t.TempDir()
	p := projectBindTest(t, a, "own", root)
	other := projectBindTest(t, a, "other", t.TempDir())
	s := &SessionMeta{ID: "s", Mode: "coding", Workspace: root, Permission: permFull}
	if err := a.Store.Create(s); err != nil {
		t.Fatal(err)
	}
	for _, args := range []any{map[string]any{"project_id": other.ID}, map[string]any{"workspace": other.Workspaces[0].DisplayPath}} {
		if _, e := callProjectTest(a, s, "project_knowledge_get", args); e == nil {
			t.Fatal("spoofed ownership accepted")
		}
	}
	out, e := callProjectTest(a, s, "project_list", map[string]any{})
	if e != nil || strings.Contains(out, other.ID) || !strings.Contains(out, p.ID) {
		t.Fatalf("list ownership: %s %v", out, e)
	}
	s.Mode = "assistant"
	if err := a.Store.SaveMeta(s); err != nil {
		t.Fatal(err)
	}
	p.Archived = true
	if _, e = a.Store.UpdateProject(p); e != nil {
		t.Fatal(e)
	}
	out, e = callProjectTest(a, s, "project_knowledge_get", map[string]any{"project_id": p.ID})
	if e != nil || !strings.Contains(out, `"archived":true`) {
		t.Fatalf("archive explicit: %s %v", out, e)
	}
	if _, e = callProjectTest(a, s, "project_knowledge_update", map[string]any{"project_id": p.ID, "expected_revision": 1, "content": "no"}); e == nil {
		t.Fatal("archived write accepted")
	}
	if e = a.Components.SetEnabled(projectKnowledgeComponentID, false); e != nil {
		t.Fatal(e)
	}
	if _, e = callProjectTest(a, s, "project_list", map[string]any{}); e == nil {
		t.Fatal("disabled call accepted")
	}
	if resolved, e := a.resolveSessionProject(s); e != nil || resolved == nil {
		t.Fatal("component off destroyed shared resolver")
	}
	if _, e = runProjectKnowledgeGet(&toolContext{app: a}, json.RawMessage(`{}`)); e == nil {
		t.Fatal("missing session accepted")
	}
	if projectKnowledgeWarning(79, 100) != "" || !strings.Contains(projectKnowledgeWarning(80, 100), "80%") || !strings.Contains(projectKnowledgeWarning(100, 100), "100%") || !strings.Contains(projectKnowledgeWarning(101, 100), "超出 1") {
		t.Fatal("threshold comparisons")
	}
	if !strings.Contains(projectKnowledgeWarning(999, 1000), "80%") {
		t.Fatal("rounding incorrectly used for 100%")
	}
}
