package main

import (
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func projectTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func projectTestCreate(t *testing.T, s *Store, id string) Project {
	t.Helper()
	p, err := s.CreateProject(Project{ID: id, Name: id, Workspaces: []ProjectWorkspace{{WorkspaceKind: "local", WorkspaceKey: "/" + id, DisplayPath: "/" + id}}})
	if err != nil {
		t.Fatal(err)
	}
	return p
}
func projectTestAllow(Project) error { return nil }
func projectTestKnowledge(t *testing.T, s *Store, id string) ProjectKnowledge {
	t.Helper()
	k, err := s.GetProjectKnowledge(id)
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func TestProjectKnowledgeBudgetScripts(t *testing.T) {
	tests := []struct {
		text string
		want int64
	}{
		{"", 0}, {"abcd", 1}, {"abcde", 2}, {"a\nb\nc", 2}, {"汉あア한", 8}, {"𠀀", 2},
		{"，。　!?\n", 2}, {"a\u0301😀", 1}, {"汉あア한𠀀abcde", 12},
	}
	for _, tt := range tests {
		if got := ProjectKnowledgeBudget(tt.text); got != tt.want {
			t.Errorf("%q budget=%d want=%d", tt.text, got, tt.want)
		}
	}
}

func TestProjectsLifecycleAndAtomicBindings(t *testing.T) {
	s := projectTestStore(t)
	p := projectTestCreate(t, s, "one")
	other := projectTestCreate(t, s, "two")
	initial := projectTestKnowledge(t, s, p.ID)
	if initial.Content != "" || initial.Revision != 1 || initial.BudgetLimit != 4000 {
		t.Fatalf("bad default: %+v", initial)
	}
	unchanged, err := s.UpdateProject(p)
	if err != nil || !reflect.DeepEqual(p, unchanged) {
		t.Fatalf("metadata no-op changed: %+v %v", unchanged, err)
	}
	conflicting := p
	conflicting.Name = "bad rename"
	conflicting.Workspaces = other.Workspaces
	if _, err := s.UpdateProject(conflicting); err == nil {
		t.Fatal("conflicting replacement accepted")
	}
	after, err := s.GetProject(p.ID)
	if err != nil || !reflect.DeepEqual(p, after) {
		t.Fatalf("replacement not atomic: %+v %v", after, err)
	}
	if _, err := s.CreateProject(Project{ID: "failed", Name: "failed", Workspaces: other.Workspaces}); err == nil {
		t.Fatal("duplicate create accepted")
	}
	if _, err := s.GetProject("failed"); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("partial project: %v", err)
	}
	p.Name = "renamed"
	p.Workspaces = []ProjectWorkspace{{WorkspaceKind: "remote", WorkspaceKey: "host:/new", DisplayPath: "host:/new"}}
	p, err = s.UpdateProject(p)
	if err != nil {
		t.Fatal(err)
	}
	if p.Name != "renamed" || len(p.Workspaces) != 1 || p.Workspaces[0].ProjectID != p.ID {
		t.Fatalf("update not applied: %+v", p)
	}
	p.Archived = true
	p, err = s.UpdateProject(p)
	if err != nil {
		t.Fatal(err)
	}
	list, err := s.ListProjects(false)
	if err != nil || len(list) != 1 || list[0].ID != other.ID {
		t.Fatalf("active list: %+v %v", list, err)
	}
	all, err := s.ListProjects(true)
	if err != nil || len(all) != 2 {
		t.Fatalf("all list: %+v %v", all, err)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, initial.Revision, "blocked", nil, projectTestAllow); !errors.Is(err, ErrProjectArchived) {
		t.Fatalf("archive write: %v", err)
	}
	if got := projectTestKnowledge(t, s, p.ID); got != initial {
		t.Fatalf("archive changed knowledge: %+v", got)
	}
	p.Archived = false
	if _, err := s.UpdateProject(p); err != nil {
		t.Fatal(err)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, initial.Revision, "restored", nil, projectTestAllow); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO sessions(id,title) VALUES('source-session','retain me')`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO messages(session_id,seq,role,content) VALUES('source-session',1,'user','retain message')`); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(p.ID, false); !errors.Is(err, ErrProjectDeleteConfirmation) {
		t.Fatalf("unconfirmed delete: %v", err)
	}
	if _, err := s.GetProject(p.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteProject(p.ID, true); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"projects", "project_workspaces", "project_knowledge"} {
		column := "project_id"
		if table == "projects" {
			column = "id"
		}
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM `+table+` WHERE `+column+`=?`, p.ID).Scan(&n); err != nil || n != 0 {
			t.Fatalf("delete %s n=%d err=%v", table, n, err)
		}
	}
	for _, table := range []string{"sessions", "messages"} {
		var n int
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil || n != 1 {
			t.Fatalf("source lost %s n=%d err=%v", table, n, err)
		}
	}
	if _, err := s.GetProjectKnowledge(p.ID); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("deleted get: %v", err)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, 1, "", nil, projectTestAllow); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("deleted save: %v", err)
	}
	if err := s.DeleteProject(p.ID, true); !errors.Is(err, ErrProjectNotFound) {
		t.Fatalf("deleted delete: %v", err)
	}
}

func TestProjectKnowledgeRevisionBudgetAndAuthorization(t *testing.T) {
	s := projectTestStore(t)
	p := projectTestCreate(t, s, "knowledge")
	k := projectTestKnowledge(t, s, p.ID)
	limit := int64(10)
	var err error
	k, err = s.SaveProjectKnowledge(p.ID, k.Revision, strings.Repeat("a", 32), &limit, projectTestAllow)
	if err != nil || k.Revision != 2 || ProjectKnowledgeBudget(k.Content) != 8 {
		t.Fatalf("80%%: %+v %v", k, err)
	}
	same, err := s.SaveProjectKnowledge(p.ID, k.Revision, k.Content, nil, projectTestAllow)
	if err != nil || same != k {
		t.Fatalf("no-op: %+v %v", same, err)
	}
	k, err = s.SaveProjectKnowledge(p.ID, k.Revision, strings.Repeat("a", 40), nil, projectTestAllow)
	if err != nil || k.Revision != 3 || ProjectKnowledgeBudget(k.Content) != 10 {
		t.Fatalf("100%%: %+v %v", k, err)
	}
	denied := errors.New("unauthorized actual session")
	called := false
	if _, err := s.SaveProjectKnowledge(p.ID, k.Revision, "no", nil, func(got Project) error {
		called = true
		if got.ID != p.ID || len(got.Workspaces) != 1 {
			t.Error("authorization missing actual project")
		}
		return denied
	}); !errors.Is(err, denied) || !called {
		t.Fatalf("authorization: %v called=%v", err, called)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, k.Revision, "no", nil, nil); !errors.Is(err, ErrProjectAuthorizationRequired) {
		t.Fatalf("nil auth: %v", err)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, k.Revision-1, k.Content, nil, projectTestAllow); !errors.Is(err, ErrProjectRevisionConflict) {
		t.Fatalf("stale no-op: %v", err)
	}
	_, err = s.SaveProjectKnowledge(p.ID, k.Revision, strings.Repeat("a", 41), nil, projectTestAllow)
	var budgetErr *ProjectKnowledgeBudgetError
	if !errors.As(err, &budgetErr) || budgetErr.Used != 11 || budgetErr.Limit != 10 {
		t.Fatalf("overflow: %v", err)
	}
	for _, n := range []int64{0, -1, 9} {
		if _, err := s.SaveProjectKnowledge(p.ID, k.Revision, k.Content, &n, projectTestAllow); err == nil {
			t.Fatalf("bad/lowered limit %d accepted", n)
		}
	}
	if got := projectTestKnowledge(t, s, p.ID); got != k {
		t.Fatalf("failed save mutated: %+v want %+v", got, k)
	}
	limit = 11
	next, err := s.SaveProjectKnowledge(p.ID, k.Revision, k.Content, &limit, projectTestAllow)
	if err != nil || next.Revision != k.Revision+1 || next.Content != k.Content || next.BudgetLimit != 11 {
		t.Fatalf("budget-only: %+v %v", next, err)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, k.Revision, "stale body", nil, projectTestAllow); !errors.Is(err, ErrProjectRevisionConflict) {
		t.Fatalf("budget change did not conflict: %v", err)
	}
	if _, err := s.SaveProjectKnowledge(p.ID, next.Revision, "", nil, projectTestAllow); err != nil {
		t.Fatalf("empty/shorten must be allowed: %v", err)
	}
}

func TestProjectKnowledgeConcurrentRevision(t *testing.T) {
	s := projectTestStore(t)
	p := projectTestCreate(t, s, "concurrent")
	k := projectTestKnowledge(t, s, p.ID)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, body := range []string{"first", "second"} {
		wg.Add(1)
		go func(body string) {
			defer wg.Done()
			_, err := s.SaveProjectKnowledge(p.ID, k.Revision, body, nil, projectTestAllow)
			results <- err
		}(body)
	}
	wg.Wait()
	close(results)
	successes, conflicts := 0, 0
	for err := range results {
		if err == nil {
			successes++
		} else if errors.Is(err, ErrProjectRevisionConflict) {
			conflicts++
		} else {
			t.Fatal(err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("success=%d conflicts=%d", successes, conflicts)
	}
	if got := projectTestKnowledge(t, s, p.ID); got.Revision != k.Revision+1 {
		t.Fatalf("revision %+v", got)
	}
}

// 用临时旧库真实应用历史迁移并记账，再由当前入口追加迁移；不碰真实数据。
func TestProjectMigrationAppendAndChecksum(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "jx.db")
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=foreign_keys(1)")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE schema_migration(id TEXT PRIMARY KEY,checksum TEXT NOT NULL,applied_at TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	last := storeMigrations[len(storeMigrations)-1]
	if last.id != "0021_project_knowledge" {
		t.Fatalf("project migration must append after old history, got %s", last.id)
	}
	oldSums := map[string]string{}
	for _, m := range storeMigrations[:len(storeMigrations)-1] {
		for _, q := range m.sql {
			if _, err := db.Exec(q); err != nil {
				t.Fatalf("old migration %s: %v", m.id, err)
			}
		}
		oldSums[m.id] = m.checksum()
		if _, err := db.Exec(`INSERT INTO schema_migration VALUES(?,?,?)`, m.id, m.checksum(), "old-time"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO sessions(id,title) VALUES('old-session','old title')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for id, want := range oldSums {
		var sum, ts string
		if err := s.db.QueryRow(`SELECT checksum,applied_at FROM schema_migration WHERE id=?`, id).Scan(&sum, &ts); err != nil || sum != want || ts != "old-time" {
			t.Fatalf("old ledger changed %s: %s %s %v", id, sum, ts, err)
		}
	}
	var sum string
	if err := s.db.QueryRow(`SELECT checksum FROM schema_migration WHERE id=?`, last.id).Scan(&sum); err != nil || sum != last.checksum() {
		t.Fatalf("new ledger %s %v", sum, err)
	}
	var title string
	if err := s.db.QueryRow(`SELECT title FROM sessions WHERE id='old-session'`).Scan(&title); err != nil || title != "old title" {
		t.Fatalf("old sessions changed %q %v", title, err)
	}
	if err := s.createSchema(); err != nil {
		t.Fatalf("migration not idempotent: %v", err)
	}
	projectTestCreate(t, s, "upgrade")
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name IN ('project_events','project_knowledge_history')`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("unexpected event/history table %d %v", count, err)
	}
	if _, err := s.db.Exec(`UPDATE schema_migration SET checksum='tampered' WHERE id=?`, storeMigrations[0].id); err != nil {
		t.Fatal(err)
	}
	if err := s.createSchema(); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Fatalf("checksum mismatch not rejected: %v", err)
	}
}
