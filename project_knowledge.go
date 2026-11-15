package main

import (
	"encoding/json"
	"fmt"
	"strings"
)

func init() {
	registerComponentCapability(projectKnowledgeComponentID, "project_list", runProjectList)
	registerComponentCapability(projectKnowledgeComponentID, "project_knowledge_get", runProjectKnowledgeGet)
	registerComponentCapability(projectKnowledgeComponentID, "project_knowledge_update", runProjectKnowledgeUpdate)
}

// 能力实现再次核验门控，避免宿主直调绕过 component_call 的公共门。
func projectCapabilityContext(t *toolContext, write bool) error {
	if t == nil || t.app == nil || t.sess == nil || t.sess.ID == "" || !projectReadableMode(t.sess.Mode) {
		return fmt.Errorf("缺少合法项目会话上下文或模式不支持")
	}
	if !t.app.projectKnowledgeEnabled(t.sess) {
		return fmt.Errorf("项目知识库组件在本会话未启用")
	}
	if write {
		if t.sess.Mode != "assistant" {
			return fmt.Errorf("只有长期助理 assistant 可维护项目知识；工作会话和 aux 只能提出修订建议，请在回执或当前对话向长期助理/用户说明项目、版本、建议与事实依据")
		}
		if t.sess.PlanMode {
			return fmt.Errorf("计划模式下不能写入项目知识，请先完成只读调研并获批退出计划模式")
		}
		if err := requireNotReadOnly(t, "维护项目知识"); err != nil {
			return err
		}
	}
	return nil
}

func projectNotRegistered() error {
	return fmt.Errorf("当前工作区尚未登记项目；任务可以继续，请引导用户到 设置 → 项目知识库 新建项目并绑定工作区；不要擅自登记、写进其他项目或创建收件箱")
}

func projectJSON(v any) (string, error) { b, e := json.Marshal(v); return string(b), e }

func runProjectList(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectCapabilityContext(t, false); err != nil {
		return "", err
	}
	var p struct {
		Query           string `json:"query"`
		IncludeArchived bool   `json:"include_archived"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	all, err := t.app.Store.ListProjects(true)
	if err != nil {
		return "", err
	}
	own := ""
	if t.sess.Mode != "assistant" {
		pr, e := t.app.resolveSessionProject(t.sess)
		if e != nil {
			return "", e
		}
		if pr == nil {
			return "", projectNotRegistered()
		}
		own = pr.ID
	}
	out := []Project{}
	query := strings.ToLower(strings.TrimSpace(p.Query))
	for _, pr := range all {
		if own != "" && pr.ID != own {
			continue
		}
		if pr.Archived && !p.IncludeArchived {
			continue
		}
		search := pr.ID + " " + pr.Name
		for _, w := range pr.Workspaces {
			search += " " + w.DisplayPath
		}
		if query != "" && !strings.Contains(strings.ToLower(search), query) {
			continue
		}
		out = append(out, pr)
	}
	if len(all) == 0 {
		return "", projectNotRegistered()
	}
	return projectJSON(out)
}

func projectForCapability(t *toolContext, id, workspace string) (*Project, error) {
	var target *Project
	if strings.TrimSpace(workspace) != "" {
		kind := "local"
		if strings.Contains(workspace, ":/") && strings.Contains(workspace, "@") {
			kind = "remote"
		}
		w, e := normalizeProjectWorkspace(kind, workspace)
		if e != nil {
			return nil, e
		}
		pr, e := t.app.resolveProjectWorkspace(w)
		if e != nil {
			return nil, e
		}
		if pr == nil {
			return nil, projectNotRegistered()
		}
		target = pr
		if id != "" && id != pr.ID {
			return nil, fmt.Errorf("project_id 与 workspace 的实际项目不一致")
		}
		id = pr.ID
	}
	if t.sess.Mode != "assistant" {
		own, e := t.app.resolveSessionProject(t.sess)
		if e != nil {
			return nil, e
		}
		if own == nil {
			return nil, projectNotRegistered()
		}
		if id != "" && id != own.ID {
			return nil, fmt.Errorf("普通会话只能访问自身实际工作区绑定的项目")
		}
		return own, nil
	}
	if id == "" {
		return nil, fmt.Errorf("长期助理请先用 project_list 定位项目，再指定 project_id 或 workspace")
	}
	if target != nil {
		return target, nil
	}
	pr, e := t.app.Store.GetProject(id)
	return &pr, e
}

func runProjectKnowledgeGet(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectCapabilityContext(t, false); err != nil {
		return "", err
	}
	var p struct {
		ProjectID string `json:"project_id"`
		Workspace string `json:"workspace"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	pr, err := projectForCapability(t, p.ProjectID, p.Workspace)
	if err != nil {
		return "", err
	}
	k, err := t.app.Store.GetProjectKnowledge(pr.ID)
	if err != nil {
		return "", err
	}
	return projectJSON(projectKnowledgeView(*pr, k))
}

func runProjectKnowledgeUpdate(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectCapabilityContext(t, true); err != nil {
		return "", err
	}
	var p struct {
		ProjectID        string  `json:"project_id"`
		ExpectedRevision *int64  `json:"expected_revision"`
		Content          *string `json:"content"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return "", err
	}
	if p.ProjectID == "" || p.ExpectedRevision == nil || p.Content == nil {
		return "", fmt.Errorf("需要 project_id、expected_revision 与完整 content；先读取当前版本")
	}
	k, err := t.app.Store.SaveProjectKnowledge(p.ProjectID, *p.ExpectedRevision, *p.Content, nil, func(pr Project) error { return projectCapabilityContext(t, true) })
	if err != nil {
		return "", err
	}
	pr, err := t.app.Store.GetProject(p.ProjectID)
	if err != nil {
		return "", err
	}
	return projectJSON(projectKnowledgeView(pr, k))
}

func projectKnowledgeWarning(used, limit int64) string {
	if used > limit {
		return fmt.Sprintf("用量 %d / %d，超出 %d 单元：删除过期、重复约束与进度流水，合并相同规则并精简背景后重试；不得截断。确需扩容请由用户在设置页调整", used, limit, used-limit)
	}
	if used == limit {
		return "已达到 100% 硬阈值，请立即重构知识库"
	}
	if used >= limit-limit/5 {
		return "已达到容量 80%，请删除或压缩过期约束"
	}
	return ""
}

func projectKnowledgeView(pr Project, k ProjectKnowledge) map[string]any {
	used := ProjectKnowledgeBudget(k.Content)
	return map[string]any{"project_id": k.ProjectID, "content": k.Content, "revision": k.Revision, "updated_at": k.UpdatedAt, "budget_limit": k.BudgetLimit, "used": used, "warning": projectKnowledgeWarning(used, k.BudgetLimit), "project_name": pr.Name, "archived": pr.Archived}
}
