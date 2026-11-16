package main

// 项目事件的设置页路由（用户身份）。AI 只能走 component_call：
// 一切带 X-JX-Caller-Session 的请求在这里 403，用户操作不登记 apiMeta。
//
// 项目登记/改名/归档/删除不在这里另开一套：设置页复用 /api/projects（Project 表
// 与生命周期只有一套），事件页只负责事件本身。永久删除须带 confirm 且由用户点确认。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
)

func (a *App) registerProjectEventRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/project-events/overview", a.handleEventOverview)
	mux.HandleFunc("GET /api/project-events", a.handleEventList)
	mux.HandleFunc("POST /api/project-events", a.handleEventCreate)
	mux.HandleFunc("GET /api/project-events/{id}", a.handleEventDetail)
	mux.HandleFunc("PATCH /api/project-events/{id}", a.handleEventPatch)
	mux.HandleFunc("DELETE /api/project-events/{id}", a.handleEventDelete)
	mux.HandleFunc("GET /api/project-events/{id}/records", a.handleEventRecords)
	mux.HandleFunc("POST /api/project-events/{id}/records", a.handleEventRecordAdd)
	mux.HandleFunc("PATCH /api/project-events/records/{id}", a.handleEventRecordPatch)
	mux.HandleFunc("DELETE /api/project-events/records/{id}", a.handleEventRecordDelete)
	mux.HandleFunc("GET /api/project-events/records/{id}/sources", a.handleEventRecordSources)
}

func projectEventSettingsGuard(w http.ResponseWriter, r *http.Request) bool {
	if r.Header.Get("X-JX-Caller-Session") != "" {
		writeErr(w, http.StatusForbidden, "项目事件设置仅限用户操作；AI 请使用 component_call 的项目事件能力")
		return false
	}
	return true
}

func (a *App) projectEventComponentOn(w http.ResponseWriter) bool {
	if a.Components == nil || !a.Components.IsEnabled(projectEventsComponentID) {
		writeErr(w, http.StatusForbidden, "项目事件组件未启用")
		return false
	}
	return true
}

func projectEventResponse(w http.ResponseWriter, v any, err error) {
	if err != nil {
		code := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrProjectEventRevisionConflict), errors.Is(err, ErrProjectEventRecordConflict), errors.Is(err, ErrProjectEventIdempotent):
			code = http.StatusConflict
		case errors.Is(err, ErrProjectEventNotFound), errors.Is(err, ErrProjectEventRecordNotFound), errors.Is(err, ErrProjectNotFound):
			code = http.StatusNotFound
		case errors.Is(err, ErrProjectEventArchived):
			code = http.StatusForbidden
		}
		writeErr(w, code, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func decodeEventRequest(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 4<<20)
	d := json.NewDecoder(r.Body)
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		writeErr(w, http.StatusBadRequest, "请求参数无效："+err.Error())
		return false
	}
	return true
}

func eventQueryInt(r *http.Request, name string) (*int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(name))
	if raw == "" {
		return nil, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return nil, fmt.Errorf("%s 必须是整数", name)
	}
	return &n, nil
}

func (a *App) userScope() eventScope { return eventScope{assistant: true} }

// ---------- 读 ----------

func (a *App) handleEventOverview(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	limit, err := eventQueryInt(r, "project_limit")
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	q := r.URL.Query()
	args := eventOverviewArgs{
		ProjectKeyword:  q.Get("project_keyword"),
		EventKeyword:    q.Get("event_keyword"),
		Scope:           q.Get("scope"),
		Status:          q.Get("status"),
		IncludeArchived: q.Get("include_archived") == "1" || q.Get("include_archived") == "true",
		ProjectLimit:    limit,
		ProjectCursor:   q.Get("project_cursor"),
	}
	out, err := a.eventOverview(a.userScope(), args, 12)
	projectEventResponse(w, out, err)
}

func (a *App) handleEventList(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	limit, err := eventQueryInt(r, "limit")
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	q := r.URL.Query()
	out, err := a.eventList(a.userScope(), eventListArgs{
		ProjectID:   q.Get("project_id"),
		Scope:       q.Get("scope"),
		Status:      q.Get("status"),
		Keyword:     q.Get("keyword"),
		SessionID:   q.Get("session_id"),
		UpdatedFrom: q.Get("updated_from"),
		UpdatedTo:   q.Get("updated_to"),
		Limit:       limit,
		Cursor:      q.Get("cursor"),
	}, 20)
	projectEventResponse(w, out, err)
}

func (a *App) handleEventDetail(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	out, err := a.eventDetail(a.userScope(), r.PathValue("id"))
	projectEventResponse(w, out, err)
}

func (a *App) handleEventRecords(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	limit, err := eventQueryInt(r, "limit")
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	q := r.URL.Query()
	out, err := a.eventRecords(a.userScope(), eventRecordsArgs{
		EventID:      r.PathValue("id"),
		Kind:         q.Get("kind"),
		Keyword:      q.Get("keyword"),
		OccurredFrom: q.Get("occurred_from"),
		OccurredTo:   q.Get("occurred_to"),
		Direction:    q.Get("direction"),
		Limit:        limit,
		Cursor:       q.Get("cursor"),
	}, 20)
	projectEventResponse(w, out, err)
}

func (a *App) handleEventRecordSources(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	limit, err := eventQueryInt(r, "limit")
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	out, err := a.eventSources(a.userScope(), eventSourcesArgs{
		RecordID: r.PathValue("id"),
		SourceID: r.URL.Query().Get("source_id"),
		Limit:    limit,
		Cursor:   r.URL.Query().Get("cursor"),
	}, 20)
	projectEventResponse(w, out, err)
}

// ---------- 写 ----------

type eventAPIWrite struct {
	ProjectID        string  `json:"project_id"`
	Title            *string `json:"title"`
	Summary          *string `json:"summary"`
	Status           *string `json:"status"`
	EventID          string  `json:"event_id"`
	ExpectedRevision *int64  `json:"expected_revision"`
	CreateKey        string  `json:"create_key"`
	Event            *struct {
		Title   *string `json:"title"`
		Summary *string `json:"summary"`
		Status  *string `json:"status"`
	} `json:"event"`
	Record                 *eventRecordArgs `json:"record"`
	ExpectedEventRevision  *int64           `json:"expected_event_revision"`
	ExpectedRecordRevision *int64           `json:"expected_record_revision"`
	Confirm                bool             `json:"confirm"`
}

func (a *App) handleEventCreate(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	var body eventAPIWrite
	if !decodeEventRequest(w, r, &body) {
		return
	}
	p, err := a.Store.GetProject(strings.TrimSpace(body.ProjectID))
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	if body.Title == nil || body.Summary == nil {
		projectEventResponse(w, nil, fmt.Errorf("需要标题与概况"))
		return
	}
	title, err := normalizeEventText("title", "标题", *body.Title, projectEventTitleBudget)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	summary, err := normalizeEventText("summary", "概况", *body.Summary, projectEventSummaryBudget)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	status, err := normalizeEventStatus(derefString(body.Status))
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	if status == "" {
		status = eventStatusDiscussing
	}
	in := ProjectEventCreateInput{Title: title, Summary: summary, Status: status, CreateKey: strings.TrimSpace(body.CreateKey)}
	if body.Record != nil {
		rec, err := a.prepareRecordAsUser(*body.Record)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		in.Record = &rec
	}
	if status != eventStatusDiscussing && in.Record == nil {
		projectEventResponse(w, nil, fmt.Errorf("%w：直接创建为「%s」必须同时给一条关键事实", ErrProjectEventRecordNeedsSource, projectEventStatusLabels[status]))
		return
	}
	in.CreateDigest = eventDigest(title, summary, status, in.Record)
	ev, created, err := a.Store.CreateProjectEventAsUser(p.ID, in)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	projectEventResponse(w, map[string]any{"created": created, "event": ev, "project": eventProjectView(p)}, nil)
}

// userStatusChangeGuard：设置页也要守「状态实际变化必须附一条关键事实」。
// 只改标题/概况（哪怕当前状态非讨论中）不强制；实际状态跳变则需同次提交 record。
func (a *App) userStatusChangeGuard(eventID string, status *string, hasRecord bool) error {
	if status == nil || hasRecord {
		return nil
	}
	ev, err := a.Store.GetProjectEvent(eventID)
	if err != nil {
		return err
	}
	if *status == ev.Status {
		return nil
	}
	return fmt.Errorf("%w：状态从「%s」变到「%s」必须同时附一条关键事实（record）；只有标题/概况的小幅纠错可以不附",
		ErrProjectEventRecordNeedsSource, projectEventStatusLabels[ev.Status], projectEventStatusLabels[*status])
}

func (a *App) handleEventPatch(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	var body eventAPIWrite
	if !decodeEventRequest(w, r, &body) {
		return
	}
	if body.ExpectedRevision == nil {
		projectEventResponse(w, nil, fmt.Errorf("需要 expected_revision；请先读取当前概况"))
		return
	}
	patch := ProjectEventPatchInput{}
	if body.Title != nil {
		v, err := normalizeEventText("title", "标题", *body.Title, projectEventTitleBudget)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		patch.Title = &v
	}
	if body.Summary != nil {
		v, err := normalizeEventText("summary", "概况", *body.Summary, projectEventSummaryBudget)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		patch.Summary = &v
	}
	if body.Status != nil {
		v, err := normalizeEventStatus(*body.Status)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		if v == "" {
			projectEventResponse(w, nil, fmt.Errorf("status 不能为空"))
			return
		}
		patch.Status = &v
	}
	var rec *ProjectEventRecordInput
	if body.Record != nil {
		prepared, err := a.prepareRecordAsUser(*body.Record)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		rec = &prepared
	}
	if err := a.userStatusChangeGuard(r.PathValue("id"), patch.Status, rec != nil); err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	ev, changed, err := a.Store.MutateProjectEventAsUser(r.PathValue("id"), *body.ExpectedRevision, patch, rec)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	projectEventResponse(w, map[string]any{"event": ev, "changed": changed}, nil)
}

func (a *App) handleEventDelete(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	var body eventAPIWrite
	if !decodeEventRequest(w, r, &body) {
		return
	}
	err := a.Store.DeleteProjectEvent(r.PathValue("id"), body.Confirm)
	projectEventResponse(w, map[string]any{"deleted": err == nil}, err)
}

func (a *App) handleEventRecordAdd(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	var body eventAPIWrite
	if !decodeEventRequest(w, r, &body) {
		return
	}
	if body.ExpectedEventRevision == nil {
		projectEventResponse(w, nil, fmt.Errorf("需要 expected_event_revision；请先读取当前概况"))
		return
	}
	if body.Record == nil {
		projectEventResponse(w, nil, fmt.Errorf("需要 record"))
		return
	}
	rec, err := a.prepareRecordAsUser(*body.Record)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	patch := ProjectEventPatchInput{}
	if body.Event != nil {
		if body.Event.Title != nil {
			v, err := normalizeEventText("title", "标题", *body.Event.Title, projectEventTitleBudget)
			if err != nil {
				projectEventResponse(w, nil, err)
				return
			}
			patch.Title = &v
		}
		if body.Event.Summary != nil {
			v, err := normalizeEventText("summary", "概况", *body.Event.Summary, projectEventSummaryBudget)
			if err != nil {
				projectEventResponse(w, nil, err)
				return
			}
			patch.Summary = &v
		}
		if body.Event.Status != nil {
			v, err := normalizeEventStatus(*body.Event.Status)
			if err != nil {
				projectEventResponse(w, nil, err)
				return
			}
			if v == "" {
				projectEventResponse(w, nil, fmt.Errorf("status 不能为空"))
				return
			}
			patch.Status = &v
		}
	}
	ev, added, changed, err := a.Store.AddProjectEventRecordAsUser(r.PathValue("id"), *body.ExpectedEventRevision, patch, rec)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	projectEventResponse(w, map[string]any{"event": ev, "record": added, "changed": changed}, nil)
}

func (a *App) handleEventRecordPatch(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	var body struct {
		ExpectedRecordRevision *int64           `json:"expected_record_revision"`
		ExpectedEventRevision  *int64           `json:"expected_event_revision"`
		Text                   *string          `json:"text"`
		Kind                   *string          `json:"kind"`
		OccurredAt             *string          `json:"occurred_at"`
		SourcesAdd             []eventSourceArg `json:"sources_add"`
		SourcesRemove          []string         `json:"sources_remove"`
	}
	if !decodeEventRequest(w, r, &body) {
		return
	}
	if body.ExpectedRecordRevision == nil || body.ExpectedEventRevision == nil {
		projectEventResponse(w, nil, fmt.Errorf("需要 expected_record_revision 与 expected_event_revision；请先重读记录与概况"))
		return
	}
	patch := ProjectEventRecordPatchInput{}
	if body.Text != nil {
		v, err := normalizeEventText("text", "推进记录", *body.Text, projectEventTextBudget)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		patch.Text = &v
	}
	if body.Kind != nil {
		v, err := normalizeEventKind(*body.Kind)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		patch.Kind = &v
	}
	if body.OccurredAt != nil {
		v, err := normalizeEventTime(*body.OccurredAt)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		patch.OccurredAt = &v
	}
	if len(body.SourcesAdd) > 0 {
		add, err := a.userSourceInputs(body.SourcesAdd)
		if err != nil {
			projectEventResponse(w, nil, err)
			return
		}
		patch.SourcesAdd = add
	}
	patch.SourcesRemove = body.SourcesRemove
	ev, rec, changed, err := a.Store.PatchProjectEventRecordAsUser(r.PathValue("id"), *body.ExpectedRecordRevision, *body.ExpectedEventRevision, patch)
	if err != nil {
		projectEventResponse(w, nil, err)
		return
	}
	projectEventResponse(w, map[string]any{"event": ev, "record": rec, "changed": changed}, nil)
}

func (a *App) handleEventRecordDelete(w http.ResponseWriter, r *http.Request) {
	if !projectEventSettingsGuard(w, r) || !a.projectEventComponentOn(w) {
		return
	}
	var body eventAPIWrite
	if !decodeEventRequest(w, r, &body) {
		return
	}
	err := a.Store.DeleteProjectEventRecord(r.PathValue("id"), body.Confirm)
	projectEventResponse(w, map[string]any{"deleted": err == nil}, err)
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
