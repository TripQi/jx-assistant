package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Empty session workspace is a persisted state, not an instruction to lose the
// configured workspace's project identity. Exercise consumers, not just a helper.
func TestB1EffectiveWorkspaceDefaultAcrossToolQueryAndEnvironment(t *testing.T) {
	a, sess, p := knowledgeEnvTestApp(t)
	a.Components = projectCapabilityTestApp(t).Components
	ws := a.Cfg.Workspace
	sess.Workspace = ""
	if err := a.Store.SaveMeta(sess); err != nil {
		t.Fatal(err)
	}
	tc := &toolContext{app: a, sess: sess, ctx: context.Background()}
	if got := tc.workspace(); got != ws {
		t.Fatalf("tool workspace = %q, want %q", got, ws)
	}
	if got := wsOr(a, sess); got != ws {
		t.Fatalf("environment workspace = %q, want %q", got, ws)
	}
	if err := os.WriteFile(filepath.Join(ws, "b1-probe.txt"), []byte("b1-default-tool-probe"), 0600); err != nil {
		t.Fatal(err)
	}
	out, err := runReadFile(tc, json.RawMessage(`{"files":[{"path":"b1-probe.txt"}]}`))
	if err != nil || !strings.Contains(out, "b1-default-tool-probe") {
		t.Fatalf("actual relative read missed default workspace: %s %v", out, err)
	}
	resolved, err := a.resolveSessionProject(sess)
	if err != nil || resolved == nil || resolved.ID != p.ID {
		t.Fatalf("default project = %+v, err=%v", resolved, err)
	}
	out, err = callProjectTest(a, sess, "project_knowledge_get", map[string]any{})
	if err != nil || !strings.Contains(out, p.ID) || !strings.Contains(out, "整段约束") {
		t.Fatalf("default query: %s %v", out, err)
	}
	out, err = callProjectTest(a, sess, "project_list", map[string]any{})
	if err != nil || !strings.Contains(out, p.ID) {
		t.Fatalf("default project list: %s %v", out, err)
	}
	block, err := a.projectKnowledgeEnv(sess)
	if err != nil {
		t.Fatal(err)
	}
	requireKnowledgeText(t, block, p.ID, "- 当前工作区："+ws+"\n", "整段约束")
	requireKnowledgeText(t, a.envPrompt(sess), "- 工作区目录："+ws+"\n", p.ID)
}

func TestB1EffectiveWorkspaceDefaultChangeAndExplicitPrecedence(t *testing.T) {
	a, sess, original := knowledgeEnvTestApp(t)
	a.Components = projectCapabilityTestApp(t).Components
	originalWS := sess.Workspace
	sess.Workspace = ""
	if err := a.Store.SaveMeta(sess); err != nil {
		t.Fatal(err)
	}
	before := a.envPrompt(sess)
	otherWS := t.TempDir()
	other := projectBindTest(t, a, "b1-other", otherWS)
	saveKnowledgeEnvTest(t, a, other.ID, "B1 changed default knowledge")
	a.Cfg.Workspace = otherWS
	out, err := callProjectTest(a, sess, "project_knowledge_get", map[string]any{})
	if err != nil || !strings.Contains(out, other.ID) || strings.Contains(out, original.ID) {
		t.Fatalf("query did not follow changed default: %s %v", out, err)
	}
	delta := envUpdateMessage(before, a.envPrompt(sess))
	requireKnowledgeText(t, delta, other.ID, "B1 changed default knowledge", "整体替换或撤销")
	if strings.Contains(delta, "整段约束，不是列表。") {
		t.Fatal("changed default retained old project body")
	}
	sess.Workspace = originalWS
	resolved, err := a.resolveSessionProject(sess)
	if err != nil || resolved == nil || resolved.ID != original.ID {
		t.Fatalf("explicit workspace must override default: %+v %v", resolved, err)
	}
	tc := &toolContext{app: a, sess: sess}
	if tc.workspace() != originalWS || wsOr(a, sess) != originalWS {
		t.Fatal("tool/environment ignored explicit workspace")
	}
	sess.Remote = &RemoteWorkspace{User: "b1-user", Host: "b1-host", Dir: "/srv/b1"}
	remoteBinding, err := normalizeProjectWorkspace("remote", sess.Remote.Display())
	if err != nil {
		t.Fatal(err)
	}
	remote, err := a.Store.CreateProject(Project{Name: "B1 remote", Workspaces: []ProjectWorkspace{remoteBinding}})
	if err != nil {
		t.Fatal(err)
	}
	resolved, err = a.resolveSessionProject(sess)
	if err != nil || resolved == nil || resolved.ID != remote.ID || tc.workspace() != "/srv/b1" || wsOr(a, sess) != "/srv/b1" {
		t.Fatalf("remote must override local and default: %+v %v", resolved, err)
	}
}

func TestB1InvalidDefaultWorkspaceDoesNotGuessProject(t *testing.T) {
	a, sess, _ := knowledgeEnvTestApp(t)
	a.Components = projectCapabilityTestApp(t).Components
	sess.Workspace = ""
	if err := a.Store.SaveMeta(sess); err != nil {
		t.Fatal(err)
	}
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// A binding at cwd catches accidental filepath.Abs("") / relative fallback.
	projectBindTest(t, a, "b1-cwd-trap", cwd)
	for _, def := range []string{"", " ", ".", "relative-project", "\x00", filepath.Join(cwd, "invalid\x00path")} {
		t.Run(def, func(t *testing.T) {
			a.Cfg.Workspace = def
			p, err := a.resolveSessionProject(sess)
			if err != nil || p != nil {
				t.Fatalf("invalid default guessed a project: %+v %v", p, err)
			}
			if _, err := callProjectTest(a, sess, "project_knowledge_get", map[string]any{}); err == nil {
				t.Fatal("invalid default authorized knowledge query")
			}
			block, err := a.projectKnowledgeEnv(sess)
			if err != nil {
				t.Fatal(err)
			}
			requireKnowledgeText(t, block, "未登记项目", "不得据此恢复效力")
			if strings.Contains(block, "整段约束") {
				t.Fatal("invalid default injected knowledge")
			}
		})
	}
}

func TestB1DefaultWorkspaceHistoryInheritanceUsesEffectiveProject(t *testing.T) {
	a, parent, _ := knowledgeEnvTestApp(t)
	seedRound(t, a, parent.ID, "B1 default parent", false)
	child := &SessionMeta{ID: "b1-default-child", Mode: "task", Permission: permWorkspace}
	if err := a.Store.Create(child); err != nil {
		t.Fatal(err)
	}
	if n, err := a.copyInheritedContext(parent.ID, child.ID); err != nil || n == 0 {
		t.Fatalf("same effective project should inherit: %d %v", n, err)
	}
	otherWS := t.TempDir()
	projectBindTest(t, a, "b1-inherit-other", otherWS)
	a.Cfg.Workspace = otherWS
	denied := &SessionMeta{ID: "b1-default-denied", Mode: "task", Permission: permWorkspace}
	if err := a.Store.Create(denied); err != nil {
		t.Fatal(err)
	}
	if n, err := a.copyInheritedContext(parent.ID, denied.ID); err == nil || n != 0 {
		t.Fatalf("different effective project should reject before copy: %d %v", n, err)
	}
	rows, err := a.Store.RawMessagesAll(denied.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("rejected inheritance copied rows: %d %v", len(rows), err)
	}
}
