package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// Exercise the real copy entry: a rejected copy must not even leave an inherit mark.
func TestB3UnregisteredHistoryInheritanceRequiresSameEffectiveWorkspace(t *testing.T) {
	for _, name := range []string{
		"same-normalized-local", "different-local", "default-local",
		"empty-default", "relative-default", "nul-default", "nul-explicit", "nil-config",
		"same-normalized-remote", "different-remote-host", "different-remote-dir", "invalid-remote",
	} {
		t.Run(name, func(t *testing.T) {
			a := newCompactTestApp(t)
			ws := t.TempDir()
			a.Cfg.Workspace = ws
			parent := &SessionMeta{ID: "b3-parent", Mode: "task", Workspace: ws}
			child := &SessionMeta{ID: "b3-child", Mode: "task", Workspace: ws}
			allow := false
			switch name {
			case "same-normalized-local":
				child.Workspace = filepath.Join(ws, "module", "..") + string(filepath.Separator)
				allow = true
			case "different-local":
				child.Workspace = t.TempDir()
			case "default-local":
				parent.Workspace = ""
				allow = true
			case "empty-default", "relative-default", "nul-default", "nil-config":
				parent.Workspace, child.Workspace = "", ""
				switch name {
				case "empty-default":
					a.Cfg.Workspace = ""
				case "relative-default":
					a.Cfg.Workspace = "relative-workspace"
				case "nul-default":
					a.Cfg.Workspace = filepath.Join(ws, "invalid\x00path")
				case "nil-config":
					a.Cfg = nil
				}
			case "nul-explicit":
				parent.Workspace = filepath.Join(ws, "invalid\x00path")
				child.Workspace = parent.Workspace
			default:
				parent.Remote = &RemoteWorkspace{User: "b3-user", Host: "B3-HOST", Dir: "/srv/b3"}
				child.Remote = &RemoteWorkspace{User: "b3-user", Host: "b3-host", Port: 22, Dir: "/srv/b3/module/.."}
				switch name {
				case "same-normalized-remote":
					allow = true
				case "different-remote-host":
					child.Remote.Host = "other-host"
				case "different-remote-dir":
					child.Remote.Dir = "/srv/other"
				case "invalid-remote":
					parent.Remote.Host, child.Remote.Host = "bad host", "bad host"
				}
			}
			for _, sess := range []*SessionMeta{parent, child} {
				if err := a.Store.Create(sess); err != nil {
					t.Fatal(err)
				}
			}
			seedRound(t, a, parent.ID, "B3 parent history", false)
			n, err := a.copyInheritedContext(parent.ID, child.ID)
			rows, readErr := a.Store.RawMessagesAll(child.ID)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if !allow {
				if err == nil || n != 0 || len(rows) != 0 {
					t.Fatalf("must reject before any row (including inherit mark): count=%d rows=%d err=%v", n, len(rows), err)
				}
				return
			}
			if err != nil || n != 2 || len(rows) != 3 || rows[0].Kind != inheritKind {
				t.Fatalf("same effective workspace must copy history and mark: count=%d rows=%d err=%v", n, len(rows), err)
			}
			if !strings.Contains(rows[1].Text(), "B3 parent history") {
				t.Fatal("allowed entry did not copy the parent history")
			}
		})
	}
}

func TestB3UnboundKnowledgeHistoryCannotReachOtherUnregisteredWorkspace(t *testing.T) {
	a, parent, project := knowledgeEnvTestApp(t)
	// Persist the actual A knowledge environment before removing its binding.
	a.maybeAppendEnvSnapshot(parent, a.envPrompt(parent), nil)
	seedRound(t, a, parent.ID, "B3 A knowledge history", false)
	before, err := a.Store.RawMessagesAll(parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	foundKnowledge := false
	for _, row := range before {
		if strings.Contains(row.Text(), project.ID) && strings.Contains(row.Text(), "整段约束，不是列表。") {
			foundKnowledge = true
		}
	}
	if !foundKnowledge {
		t.Fatal("fixture must contain persisted project A knowledge history")
	}
	project.Workspaces = nil
	if _, err := a.Store.UpdateProject(project); err != nil {
		t.Fatal(err)
	}
	child := &SessionMeta{ID: "b3-unbound-target", Mode: "task", Workspace: t.TempDir()}
	if err := a.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	for _, sess := range []*SessionMeta{parent, child} {
		p, err := a.resolveSessionProject(sess)
		if err != nil || p != nil {
			t.Fatalf("both workspaces must now be unregistered: project=%+v err=%v", p, err)
		}
	}
	if n, err := a.copyInheritedContext(parent.ID, child.ID); err == nil || n != 0 {
		t.Fatalf("unbound A history must not cross to another unregistered workspace: count=%d err=%v", n, err)
	} else if !strings.Contains(err.Error(), "自足任务书") {
		t.Fatalf("rejection must guide self-contained dispatch: %v", err)
	}
	rows, err := a.Store.RawMessagesAll(child.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected target must have zero rows, including no inherit mark: rows=%d err=%v", len(rows), err)
	}
}
