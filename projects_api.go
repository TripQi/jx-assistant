package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
)

// 设置页路由不登记 apiMeta：AI 只能走 component_call，不能借用户身份管理结构或额度。
func (a *App) registerProjectRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/projects", a.handleProjectsList)
	mux.HandleFunc("POST /api/projects", a.handleProjectCreate)
	mux.HandleFunc("PATCH /api/projects/{id}", a.handleProjectPatch)
	mux.HandleFunc("DELETE /api/projects/{id}", a.handleProjectDelete)
	mux.HandleFunc("GET /api/projects/{id}/knowledge", a.handleProjectKnowledgeGet)
	mux.HandleFunc("PUT /api/projects/{id}/knowledge", a.handleProjectKnowledgePut)
	mux.HandleFunc("POST /api/projects/budget", a.handleProjectBudget)
	mux.HandleFunc("POST /api/projects/resolve", a.handleProjectResolve)
}

func projectSettingsRequest(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-JX-Caller-Session") != "" {
		writeErr(w, http.StatusForbidden, "项目设置仅限用户操作；AI 请使用 component_call")
		return false
	}
	return true
}
func decodeProjectRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求参数无效："+err.Error())
		return false
	}
	return true
}
func projectResponse(w http.ResponseWriter, v any, err error) {
	if err != nil {
		code := http.StatusBadRequest
		if errors.Is(err, ErrProjectRevisionConflict) {
			code = http.StatusConflict
		}
		if errors.Is(err, ErrProjectNotFound) {
			code = http.StatusNotFound
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (a *App) handleProjectsList(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	p, e := a.Store.ListProjects(true)
	projectResponse(w, p, e)
}
func (a *App) handleProjectCreate(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	var p struct {
		Name string `json:"name"`
	}
	if !decodeProjectRequest(w, r, &p) {
		return
	}
	pr, e := a.Store.CreateProject(Project{Name: p.Name})
	projectResponse(w, pr, e)
}
func (a *App) handleProjectPatch(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	var p struct {
		Name       *string `json:"name"`
		Archived   *bool   `json:"archived"`
		Workspaces *[]struct {
			Kind        string `json:"workspace_kind"`
			DisplayPath string `json:"display_path"`
		} `json:"workspaces"`
	}
	if !decodeProjectRequest(w, r, &p) {
		return
	}
	var bindings *[]ProjectWorkspace
	if p.Workspaces != nil {
		ws := []ProjectWorkspace{}
		for _, v := range *p.Workspaces {
			n, e := normalizeProjectWorkspace(v.Kind, v.DisplayPath)
			if e != nil {
				projectResponse(w, nil, e)
				return
			}
			ws = append(ws, n)
		}
		bindings = &ws
	}
	pr, e := a.Store.GetProject(r.PathValue("id"))
	if e != nil {
		projectResponse(w, nil, e)
		return
	}
	if p.Name != nil {
		pr.Name = *p.Name
	}
	if p.Archived != nil {
		pr.Archived = *p.Archived
	}
	if bindings != nil {
		pr.Workspaces = *bindings
	}
	pr, e = a.Store.UpdateProject(pr)
	projectResponse(w, pr, e)
}
func (a *App) handleProjectDelete(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	var p struct {
		Confirm bool `json:"confirm"`
	}
	if !decodeProjectRequest(w, r, &p) {
		return
	}
	e := a.Store.DeleteProject(r.PathValue("id"), p.Confirm)
	projectResponse(w, map[string]bool{"deleted": e == nil}, e)
}
func (a *App) handleProjectKnowledgeGet(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	pr, e := a.Store.GetProject(r.PathValue("id"))
	if e != nil {
		projectResponse(w, nil, e)
		return
	}
	k, e := a.Store.GetProjectKnowledge(pr.ID)
	if e != nil {
		projectResponse(w, nil, e)
		return
	}
	projectResponse(w, projectKnowledgeView(pr, k), nil)
}
func (a *App) handleProjectKnowledgePut(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	if a.Components == nil || !a.Components.IsEnabled(projectKnowledgeComponentID) {
		writeErr(w, http.StatusForbidden, "项目知识库组件未启用")
		return
	}
	var p struct {
		Content     *string `json:"content"`
		BudgetLimit *int64  `json:"budget_limit"`
		Revision    *int64  `json:"expected_revision"`
	}
	if !decodeProjectRequest(w, r, &p) {
		return
	}
	if p.Content == nil || p.Revision == nil {
		projectResponse(w, nil, fmt.Errorf("需要正文与 expected_revision；请先读取当前知识"))
		return
	}
	k, e := a.Store.SaveProjectKnowledge(r.PathValue("id"), *p.Revision, *p.Content, p.BudgetLimit, func(pr Project) error {
		if a.Components == nil || !a.Components.IsEnabled(projectKnowledgeComponentID) {
			return fmt.Errorf("项目知识库组件未启用")
		}
		return nil
	})
	if e != nil {
		projectResponse(w, nil, e)
		return
	}
	pr, e := a.Store.GetProject(k.ProjectID)
	if e != nil {
		projectResponse(w, nil, e)
		return
	}
	projectResponse(w, projectKnowledgeView(pr, k), nil)
}
func (a *App) handleProjectBudget(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	var p struct {
		Content string `json:"content"`
		Limit   int64  `json:"budget_limit"`
	}
	if !decodeProjectRequest(w, r, &p) {
		return
	}
	if p.Limit <= 0 {
		projectResponse(w, nil, fmt.Errorf("预算上限须为合法正整数"))
		return
	}
	used := ProjectKnowledgeBudget(p.Content)
	projectResponse(w, map[string]any{"used": used, "warning": projectKnowledgeWarning(used, p.Limit)}, nil)
}
func (a *App) handleProjectResolve(w http.ResponseWriter, r *http.Request) {
	if !projectSettingsRequest(w, r) {
		return
	}
	var p struct {
		Kind    string `json:"workspace_kind"`
		Display string `json:"display_path"`
	}
	if !decodeProjectRequest(w, r, &p) {
		return
	}
	ws, e := normalizeProjectWorkspace(strings.TrimSpace(p.Kind), p.Display)
	if e != nil {
		projectResponse(w, nil, e)
		return
	}
	pr, e := a.resolveProjectWorkspace(ws)
	projectResponse(w, map[string]any{"project": pr}, e)
}
