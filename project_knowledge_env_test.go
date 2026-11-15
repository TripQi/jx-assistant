package main

import (
	"path/filepath"
	"strings"
	"testing"
)

func knowledgeEnvTestApp(t *testing.T) (*App, *SessionMeta, Project) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	root := t.TempDir()
	writeComp(t, root, projectKnowledgeComponentID, "项目知识库", "1.0.0")
	a.Components = InitComponents(root, filepath.Join(t.TempDir(), "components.json"))
	a.SessionCX = loadSessionCX(a.Store)
	ws := t.TempDir()
	a.Cfg.Workspace = ws
	binding, err := normalizeProjectWorkspace("local", ws)
	if err != nil {
		t.Fatal(err)
	}
	p, err := a.Store.CreateProject(Project{Name: "环境项目", Workspaces: []ProjectWorkspace{binding}})
	if err != nil {
		t.Fatal(err)
	}
	s := &SessionMeta{ID: "knowledge-env", Mode: "task", Workspace: ws, Permission: permWorkspace}
	if err := a.Store.Create(s); err != nil {
		t.Fatal(err)
	}
	saveKnowledgeEnvTest(t, a, p.ID, "# 背景\n整段约束，不是列表。\n- 保留行")
	return a, s, p
}

func saveKnowledgeEnvTest(t *testing.T, a *App, id, content string) ProjectKnowledge {
	t.Helper()
	k, err := a.Store.GetProjectKnowledge(id)
	if err != nil {
		t.Fatal(err)
	}
	k, err = a.Store.SaveProjectKnowledge(id, k.Revision, content, nil, func(Project) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func requireKnowledgeText(t *testing.T, text string, parts ...string) {
	t.Helper()
	for _, part := range parts {
		if !strings.Contains(text, part) {
			t.Fatalf("缺少 %q:\n%s", part, text)
		}
	}
}

func TestProjectKnowledgeEnvironmentModesAndWholeReplacement(t *testing.T) {
	a, s, p := knowledgeEnvTestApp(t)
	for _, mode := range []string{"task", "office", "coding", "beginner-coding", "skill-create", "aux"} {
		s.Mode = mode
		block, err := a.projectKnowledgeEnv(s)
		if err != nil {
			t.Fatal(err)
		}
		requireKnowledgeText(t, block, p.ID, "知识版本：v2", "更新时间：", "预算：", "整段约束，不是列表。")
	}
	s.Mode = "assistant"
	block, err := a.projectKnowledgeEnv(s)
	if err != nil || block != "" {
		t.Fatalf("assistant 不应自动正文: %q %v", block, err)
	}
	s.Mode = "task"
	before := a.envPrompt(s)
	saveKnowledgeEnvTest(t, a, p.ID, "# 新背景\n全新约束。\n- 保留行")
	after := a.envPrompt(s)
	delta := envUpdateMessage(before, after)
	requireKnowledgeText(t, delta, "完整重发", "整体替换或撤销", "知识版本：v3", "全新约束。")
	if strings.Contains(delta, "整段约束，不是列表。") {
		t.Fatal("整体替换不应重发已失效正文")
	}
	if n := strings.Count(delta, projectKnowledgeEnvStart); n != 1 {
		t.Fatalf("同项目应只有一个当前知识块: %d", n)
	}
}

func TestProjectKnowledgeRevocationBeforeAndAfterCompression(t *testing.T) {
	for _, summary := range []bool{false, true} {
		for _, change := range []string{"archive", "unbind", "switch", "disable", "delete"} {
			t.Run(change+map[bool]string{false: "-rule", true: "-summary"}[summary], func(t *testing.T) {
				a, s, p := knowledgeEnvTestApp(t)
				a.maybeAppendEnvSnapshot(s, a.envPrompt(s), nil)
				raw, err := a.Store.RawMessagesAll(s.ID)
				if err != nil {
					t.Fatal(err)
				}
				oldSeq := raw[len(raw)-1].Seq
				saveKnowledgeEnvTest(t, a, p.ID, "更新后的完整约束。")
				a.maybeAppendEnvSnapshot(s, a.envPrompt(s), nil)
				before := a.envPrompt(s)
				switch change {
				case "archive":
					p.Archived = true
					if _, err := a.Store.UpdateProject(p); err != nil {
						t.Fatal(err)
					}
				case "unbind":
					p.Workspaces = nil
					if _, err := a.Store.UpdateProject(p); err != nil {
						t.Fatal(err)
					}
				case "switch":
					s.Workspace = t.TempDir()
				case "disable":
					if err := a.Components.SetEnabled(projectKnowledgeComponentID, false); err != nil {
						t.Fatal(err)
					}
				case "delete":
					if err := a.Store.DeleteProject(p.ID, true); err != nil {
						t.Fatal(err)
					}
				}
				after := a.envPrompt(s)
				delta := envUpdateMessage(before, after)
				requireKnowledgeText(t, delta, "整体替换或撤销", "当前无有效自动注入", "不得据此恢复效力")
				a.maybeAppendEnvSnapshot(s, after, nil)
				rows, err := a.Store.RawMessagesAll(s.ID)
				if err != nil {
					t.Fatal(err)
				}
				end := rows[len(rows)-1].Seq
				if summary {
					if err := a.Store.AppendSummary(s.ID, &StoredMessage{Message: TextMessage("system", "旧摘要：更新后的完整约束。"), Kind: kindSummary, Origin: "manual", CoversFrom: oldSeq, CoversUpTo: end, CreatedAt: now()}); err != nil {
						t.Fatal(err)
					}
				} else {
					for _, row := range rows {
						if err := a.Store.SetCompact(s.ID, row.Seq, compactOmit); err != nil {
							t.Fatal(err)
						}
					}
				}
				a.resendEnvAfterCompact(s)
				msgs := a.buildContext(s, "system", 0)
				if len(msgs) < 2 {
					t.Fatal("压缩后缺少当前环境baseline")
				}
				last := msgs[len(msgs)-1].Text()
				requireKnowledgeText(t, last, "当前无有效自动注入", "不得据此恢复效力")
				if strings.Contains(last, "更新后的完整约束。") || strings.Contains(last, "整段约束，不是列表。") {
					t.Fatal("压缩后的基线恢复了旧正文")
				}
				// 历史原文仍保留，撤销不是物理擦除。
				original := rowBySeq(t, a, s.ID, oldSeq)
				requireKnowledgeText(t, original.Text(), "整段约束，不是列表。")
			})
		}
	}
}

func TestProjectKnowledgeCompressionBaselineUsesLatestVersion(t *testing.T) {
	a, s, p := knowledgeEnvTestApp(t)
	a.maybeAppendEnvSnapshot(s, a.envPrompt(s), nil)
	raw, err := a.Store.RawMessagesAll(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range raw {
		if err := a.Store.SetCompact(s.ID, row.Seq, compactOmit); err != nil {
			t.Fatal(err)
		}
	}
	saveKnowledgeEnvTest(t, a, p.ID, "最新压缩基线正文。")
	a.resendEnvAfterCompact(s)
	msgs := a.buildContext(s, "sys", 0)
	last := msgs[len(msgs)-1].Text()
	requireKnowledgeText(t, last, "知识版本：v3", "最新压缩基线正文。")
	if strings.Contains(last, "整段约束，不是列表。") {
		t.Fatal("新baseline混入旧知识")
	}
}

func TestProjectKnowledgeInheritedHistoryIsolation(t *testing.T) {
	a, parent, _ := knowledgeEnvTestApp(t)
	seedRound(t, a, parent.ID, "父项目秘密", false)
	same := &SessionMeta{ID: "same-project", Mode: "task", Workspace: filepath.Join(parent.Workspace, "module")}
	if err := a.Store.Create(same); err != nil {
		t.Fatal(err)
	}
	if n, err := a.copyInheritedContext(parent.ID, same.ID); err != nil || n == 0 {
		t.Fatalf("同项目应能继承: %d %v", n, err)
	}
	otherWS := t.TempDir()
	binding, err := normalizeProjectWorkspace("local", otherWS)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Store.CreateProject(Project{Name: "别的项目", Workspaces: []ProjectWorkspace{binding}}); err != nil {
		t.Fatal(err)
	}
	// Empty follows the configured default, which here belongs to the parent.
	defaultChild := &SessionMeta{ID: "default-workspace", Mode: "task"}
	if err := a.Store.Create(defaultChild); err != nil {
		t.Fatal(err)
	}
	if n, err := a.copyInheritedContext(parent.ID, defaultChild.ID); err != nil || n == 0 {
		t.Fatalf("默认工作区绑定同项目应允许继承: %d %v", n, err)
	}
	for i, ws := range []string{otherWS, t.TempDir()} {
		child := &SessionMeta{ID: []string{"other-project", "unregistered"}[i], Mode: "task", Workspace: ws}
		if err := a.Store.Create(child); err != nil {
			t.Fatal(err)
		}
		if _, err := a.copyInheritedContext(parent.ID, child.ID); err == nil {
			t.Fatal("跨项目/目标未登记不能继承")
		}
		rows, err := a.Store.RawMessagesAll(child.ID)
		if err != nil || len(rows) != 0 {
			t.Fatalf("拒绝前不能复制任何父历史: %d %v", len(rows), err)
		}
	}
}

func TestProjectKnowledgeContextOverflowIsExplicit(t *testing.T) {
	a, s, _ := knowledgeEnvTestApp(t)
	target := LLMTarget{ContextWindow: 1, Model: "unchanged-model"}
	err := a.validateProjectKnowledgeContext(s, target)
	if err == nil {
		t.Fatal("知识放不进模型窗口必须明确失败")
	}
	requireKnowledgeText(t, err.Error(), "未截断正文", "未切换模型或渠道")
	if target.Model != "unchanged-model" {
		t.Fatal("不能偷偷切换模型")
	}
}
