package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestProjectMigrationFailureRollsBackDDLAndLedger(t *testing.T) {
	s := projectTestStore(t)
	// 临时库退到追加迁移前，并故意造一个同名表使迁移第二条DDL失败。
	for _, q := range []string{`DROP TABLE project_knowledge`, `DROP TABLE project_workspaces`, `DROP TABLE projects`, `DELETE FROM schema_migration WHERE id='0021_project_knowledge'`, `CREATE TABLE project_workspaces(sentinel TEXT)`} {
		if _, e := s.db.Exec(q); e != nil {
			t.Fatal(e)
		}
	}
	if e := s.createSchema(); e == nil || !strings.Contains(e.Error(), "0021_project_knowledge") {
		t.Fatalf("expected migration failure: %v", e)
	}
	var n int
	if e := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE type='table' AND name='projects'`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("partial DDL persisted: %d %v", n, e)
	}
	if e := s.db.QueryRow(`SELECT count(*) FROM schema_migration WHERE id='0021_project_knowledge'`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("failed migration recorded: %d %v", n, e)
	}
	if _, e := s.db.Exec(`DROP TABLE project_workspaces`); e != nil {
		t.Fatal(e)
	}
	if e := s.createSchema(); e != nil {
		t.Fatalf("retry after repair: %v", e)
	}
	if _, e := s.CreateProject(Project{Name: "retry"}); e != nil {
		t.Fatal(e)
	}
}

func TestProjectKnowledgeSessionSwitchOverrideAndActiveProjectChange(t *testing.T) {
	a, s, p := knowledgeEnvTestApp(t)
	otherWS := t.TempDir()
	binding, e := normalizeProjectWorkspace("local", otherWS)
	if e != nil {
		t.Fatal(e)
	}
	other, e := a.Store.CreateProject(Project{Name: "另外项目", Workspaces: []ProjectWorkspace{binding}})
	if e != nil {
		t.Fatal(e)
	}
	saveKnowledgeEnvTest(t, a, other.ID, "另一项目独有约束")
	before := a.envPrompt(s)
	s.Workspace = filepath.Join(otherWS, "module")
	after := a.envPrompt(s)
	delta := envUpdateMessage(before, after)
	if !strings.Contains(delta, other.ID) || !strings.Contains(delta, "另一项目独有约束") || strings.Contains(delta, "整段约束，不是列表。") {
		t.Fatal("project switch did not replace full block")
	}
	a.SessionCX.set(a.Store, s.ID, func(o *SessionOverrides) {
		if o.Components == nil {
			o.Components = map[string]bool{}
		}
		o.Components[projectKnowledgeComponentID] = false
	})
	block, e := a.projectKnowledgeEnv(s)
	if e != nil || strings.Contains(block, "另一项目独有约束") || !strings.Contains(block, "不得据此恢复效力") {
		t.Fatalf("session off: %s %v", block, e)
	}
	if _, e = a.Store.GetProject(p.ID); e != nil {
		t.Fatal("session off removed shared project")
	}
}
