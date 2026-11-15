package main

import (
	"fmt"
	"strings"
)

const projectKnowledgeEnvStart = "<!-- project-knowledge:begin -->"
const projectKnowledgeEnvEnd = "<!-- project-knowledge:end -->"

func projectKnowledgeAutoMode(mode string) bool {
	switch mode {
	case "task", "office", "coding", "beginner-coding", "skill-create", "aux":
		return true
	}
	return false
}

// 即使当前无有效正文，也明确否定摘要或旧快照恢复旧项目约束的效力。
func (a *App) projectKnowledgeEnv(sess *SessionMeta) (string, error) {
	if sess == nil || !projectKnowledgeAutoMode(sess.Mode) {
		return "", nil
	}
	inactive := "# 项目知识库\n- 当前无有效自动注入的项目知识；早前项目知识块及摘要中的项目约束均不得据此恢复效力。\n"
	body := inactive
	if a.projectKnowledgeEnabled(sess) {
		p, err := a.resolveSessionProject(sess)
		if err != nil {
			return "", fmt.Errorf("解析项目知识归属失败：%w", err)
		}
		if p != nil && !p.Archived {
			k, err := a.Store.GetProjectKnowledge(p.ID)
			if err != nil {
				return "", fmt.Errorf("读取项目知识失败：%w", err)
			}
			ws := a.workspaceOf(sess)
			if sess.Remote != nil {
				ws = sess.Remote.Display()
			}
			used := ProjectKnowledgeBudget(k.Content)
			percent := float64(0)
			if k.BudgetLimit > 0 {
				percent = float64(used) / float64(k.BudgetLimit) * 100
			}
			body = fmt.Sprintf("# 项目知识库（仅适用于下述项目）\n- 项目：%s / %s\n- 当前工作区：%s\n- 知识版本：v%d\n- 更新时间：%s\n- 预算：%d / %d 单元（%.1f%%）\n- 有效性：本块整体替代早前项目知识，以当前版本为准；历史快照或摘要中的旧约束可能已失效，冲突须核实。\n- 修订建议：工作会话和 aux 不可直接修改正文；仅在必要时向长期助理的任务回执或当前用户对话说明项目、依据版本、遗漏/冲突点、建议及事实依据；无须填写空模板。\n\n%s\n", p.Name, p.ID, ws, k.Revision, k.UpdatedAt, used, k.BudgetLimit, percent, k.Content)
		} else if p == nil {
			body += "- 当前工作区未登记项目；需要项目知识时请用户到设置页登记，当前任务可继续。\n"
		}
	}
	return projectKnowledgeEnvStart + "\n" + body + projectKnowledgeEnvEnd, nil
}

func projectKnowledgeEnvPart(env string) string {
	start := strings.Index(env, projectKnowledgeEnvStart)
	if start < 0 {
		return ""
	}
	end := strings.LastIndex(env, projectKnowledgeEnvEnd)
	if end < start {
		return env[start:]
	}
	return env[start : end+len(projectKnowledgeEnvEnd)]
}

// 继承完整历史无法可靠过滤项目内容；解析失败或未登记都不能视为同项目。
func (a *App) checkProjectHistoryInheritance(parent, child *SessionMeta) error {
	if a.Store == nil {
		return nil
	}
	p, err := a.resolveSessionProject(parent)
	if err != nil {
		return fmt.Errorf("无法核实父会话项目，拒绝继承历史：%w", err)
	}
	c, err := a.resolveSessionProject(child)
	if err != nil {
		return fmt.Errorf("无法核实目标项目，拒绝继承历史：%w", err)
	}
	// 未登记不代表历史没有项目内容（绑定可能已解除）；只有可证明同一工作区才放行。
	if p == nil && c == nil {
		var workspaces [2]ProjectWorkspace
		for i, sess := range []*SessionMeta{parent, child} {
			if sess == nil || (sess.Remote == nil && sess.Workspace == "" && a.Cfg == nil) {
				return fmt.Errorf("无法核实有效工作区，拒绝继承历史；请使用不继承上下文的专家和自足任务书")
			}
			w, err := a.sessionProjectWorkspace(sess)
			if err != nil || w.WorkspaceKey == "" || strings.ContainsRune(w.DisplayPath, 0) {
				return fmt.Errorf("无法核实有效工作区，拒绝继承历史；请使用不继承上下文的专家和自足任务书")
			}
			workspaces[i] = w
		}
		if workspaces[0].WorkspaceKind == workspaces[1].WorkspaceKind && workspaces[0].WorkspaceKey == workspaces[1].WorkspaceKey {
			return nil
		}
	}
	if p == nil || c == nil || p.ID != c.ID {
		return fmt.Errorf("跨项目或未登记项目不能继承父会话完整历史；请使用不继承上下文的专家和自足任务书，目标工作区须自行加载项目知识")
	}
	return nil
}

func (a *App) validateProjectKnowledgeContext(sess *SessionMeta, target LLMTarget) error {
	block, err := a.projectKnowledgeEnv(sess)
	if err != nil {
		return err
	}
	if block == "" {
		return nil
	}
	if target.ContextWindow > 0 && estimateTokens([]Message{TextMessage("system", a.systemPrompt(sess)), TextMessage("user", block)}) >= target.ContextWindow {
		return fmt.Errorf("项目知识正文与系统提示已超出当前模型上下文能力；未截断正文、未切换模型或渠道。请整理知识内容或由用户调整模型上下文配置")
	}
	return nil
}
