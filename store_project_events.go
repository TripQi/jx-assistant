package main

// 项目事件的存取与事务（表结构见 store.go 的 0023_project_events）。
//
// 设计取舍（人的意图，不在实现细节里重复）：
//   - 事件域时间统一成「UTC + 固定三位毫秒」的文本，按文本排序即时间序，
//     不混用偏移与变长小数；事实时间（occurred_at）可空，空＝未明确，排序
//     回退记录时间，绝不捏造时间精度。
//   - 一切写入走同一个事务：概况/状态、新记录、来源增删、父事件 revision
//     要么全成、要么全不成；无实际变化不刷新 revision 与 updated_at。
//   - 写事务内的授权复核只用 tx 查询（Store 是单连接，持锁再调 Store 会死锁）。
//   - 来源不设外键、只存消息行 ID 与正文摘要哈希：用户删会话/编辑重发不会
//     被拦住，也不会把事实抹掉；失效来源读时如实报告状态。

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// 事件域时间精度：固定三位毫秒 UTC，文本排序即时间序。
const projectEventTimeLayout = "2006-01-02T15:04:05.000Z"

const (
	projectEventScopeActive  = "active"
	projectEventScopeHistory = "history"
	projectEventScopeAll     = "all"
)

const (
	eventStatusDiscussing = "discussing"
	eventStatusProcessing = "processing"
	eventStatusAwaiting   = "awaiting"
	eventStatusPaused     = "paused"
	eventStatusDone       = "done"
	eventStatusCancelled  = "cancelled"
)

const (
	eventKindPlan       = "plan"
	eventKindMilestone  = "milestone"
	eventKindIssue      = "issue"
	eventKindValidation = "validation"
	eventKindBlocker    = "blocker"
	eventKindAcceptance = "acceptance"
	eventKindOther      = "other"
)

const (
	eventOriginAI   = "ai"
	eventOriginUser = "user" // 设置页用户录入：origin 由宿主按入口身份确定，AI 不可指定
)

var projectEventStatusLabels = map[string]string{
	eventStatusDiscussing: "讨论中",
	eventStatusProcessing: "处理中",
	eventStatusAwaiting:   "待验收",
	eventStatusPaused:     "暂停",
	eventStatusDone:       "已完成",
	eventStatusCancelled:  "取消",
}

var projectEventKindLabels = map[string]string{
	eventKindPlan:       "方案确认",
	eventKindMilestone:  "阶段成果",
	eventKindIssue:      "关键问题",
	eventKindValidation: "验证结论",
	eventKindBlocker:    "阻碍变化",
	eventKindAcceptance: "用户验收",
	eventKindOther:      "其他关键事实",
}

var (
	ErrProjectEventNotFound          = errors.New("项目事件不存在，请重新查询当前事件列表")
	ErrProjectEventRecordNotFound    = errors.New("推进记录不存在，请重新读取该事件的时间线")
	ErrProjectEventRevisionConflict  = errors.New("项目事件版本冲突：请重读当前概况与最近记录，合并后再保存")
	ErrProjectEventRecordConflict    = errors.New("推进记录版本冲突：请重读该记录后合并再更正")
	ErrProjectEventIdempotent        = errors.New("相同幂等键已用于不同内容：请换新键，或先核对已提交结果")
	ErrProjectEventArchived          = errors.New("项目已归档，事件只读；请先由用户在设置页恢复项目")
	ErrProjectEventDeleteConfirm     = errors.New("永久删除须由用户在设置页明确确认；AI 不提供删除能力")
	ErrProjectEventSourceInvalid     = errors.New("来源会话不满足项目与调用者资格，已拒绝写入")
	ErrProjectEventRecordNeedsSource = errors.New("非讨论中状态必须同时附一条有依据的关键事实")
	ErrProjectEventSourceRequired    = errors.New("AI 关键事实必须给出实际依据会话")
)

// ---------- 数据结构 ----------

// ProjectEvent 是大事件当前概况：不含时间线，记录数/来源会话数都是派生计数。
type ProjectEvent struct {
	ID           string `json:"event_id"`
	ProjectID    string `json:"project_id"`
	Title        string `json:"title"`
	Summary      string `json:"summary"`
	Status       string `json:"status"`
	StatusLabel  string `json:"status_label"`
	Revision     int64  `json:"revision"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	RecordCount  int    `json:"record_count"`
	SessionCount int    `json:"session_count"`
}

// ProjectEventRecord 是一条关键事实；OccurredAt 为空表示事实时间未明确。
type ProjectEventRecord struct {
	ID          string `json:"record_id"`
	EventID     string `json:"event_id"`
	Kind        string `json:"kind"`
	KindLabel   string `json:"kind_label"`
	Text        string `json:"text"`
	OccurredAt  string `json:"occurred_at,omitempty"`
	Origin      string `json:"origin"`
	Revision    int64  `json:"revision"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	SourceCount int    `json:"source_count"`
}

// ProjectEventSource 是「记录 → 来源」关系；message_id/digest 只用于复核，不是正文副本。
type ProjectEventSource struct {
	ID            string `json:"source_id"`
	RecordID      string `json:"record_id"`
	SessionID     string `json:"session_id"`
	MessageID     int64  `json:"message_id"`
	MessageSeq    int64  `json:"message_seq"`
	MessageDigest string `json:"-"`
	SourceKey     string `json:"-"`
	Origin        string `json:"origin"`
	LinkedAt      string `json:"linked_at"`
}

type ProjectEventRecordInput struct {
	Kind          string
	Text          string
	OccurredAt    string // 已规范化为 UTC 毫秒文本，空＝未明确
	ClientKey     string
	RequestDigest string
	Sources       []ProjectEventSourceInput
}

// ProjectEventSourceInput 已由业务层校验过项目资格；事务内再核存在性与消息归属。
type ProjectEventSourceInput struct {
	SessionID  string
	MessageSeq int64
}

type ProjectEventCreateInput struct {
	Title        string
	Summary      string
	Status       string
	CreateKey    string
	CreateDigest string
	Record       *ProjectEventRecordInput
}

// ProjectEventPatchInput 是 PATCH 语义：nil 字段不动。
type ProjectEventPatchInput struct {
	Title   *string
	Summary *string
	Status  *string
}

type ProjectEventRecordPatchInput struct {
	Text          *string
	Kind          *string
	OccurredAt    *string
	SourcesAdd    []ProjectEventSourceInput
	SourcesRemove []string
}

// eventWriteAuth 在写事务内复核调用者资格：只能使用传入的 reader（tx），不得再调 Store。
type eventWriteAuth func(q projectReader, p Project, sess *SessionMeta) error

// ---------- 时间与 ID ----------

func projectEventNow() string { return time.Now().UTC().Format(projectEventTimeLayout) }

func projectEventTimeOf(t time.Time) string { return t.UTC().Format(projectEventTimeLayout) }

func newProjectID(prefix string) (string, error) {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b[:]), nil
}

// messageDigest 只用于「原正文是否被改」的复核；不保存正文副本。
func messageDigest(role, content string) string {
	h := sha256.New()
	h.Write([]byte(role))
	h.Write([]byte{0})
	h.Write([]byte(content))
	return hex.EncodeToString(h.Sum(nil))
}

// ---------- 游标（绑定筛选签名，不承诺并发快照） ----------

// eventCursor 用时间和稳定 ID 定位下一页；Kind/Sig 绑定查询种类与筛选条件，
// 伪造或换筛选复用旧游标一律拒绝，不允许「分页即全量一致」的错觉。
type eventCursor struct {
	Kind string `json:"k"`
	Sig  string `json:"s"`
	Time string `json:"t"`
	ID   string `json:"i"`
}

func encodeEventCursor(c eventCursor) string {
	b, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeEventCursor(raw, kind, sig string) (*eventCursor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil {
		return nil, fmt.Errorf("分页游标无效，请从第一页重新查询")
	}
	var c eventCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("分页游标无效，请从第一页重新查询")
	}
	if c.Kind != kind || c.Sig != sig || c.Time == "" || c.ID == "" {
		return nil, fmt.Errorf("分页游标与当前查询条件不匹配：请从第一页重新查询，不要把上一页游标套到别的筛选上")
	}
	return &c, nil
}

func eventFilterSignature(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// ---------- 过滤条件 ----------

// ProjectEventFilter 是大事件筛选；scope 缺省＝未结束，显式 Status 覆盖 scope 状态集合。
type ProjectEventFilter struct {
	Scope       string
	Status      string
	Keyword     string
	SessionID   string
	UpdatedFrom string
	UpdatedTo   string
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	return strings.ReplaceAll(s, `_`, `\_`)
}

func (f ProjectEventFilter) clause() ([]string, []any) {
	var parts []string
	var args []any
	scope := f.Scope
	if scope == "" {
		scope = projectEventScopeActive
	}
	if f.Status != "" {
		parts = append(parts, `e.status = ?`)
		args = append(args, f.Status)
	} else {
		switch scope {
		case projectEventScopeActive:
			parts = append(parts, `e.status IN ('discussing','processing','awaiting','paused')`)
		case projectEventScopeHistory:
			parts = append(parts, `e.status IN ('done','cancelled')`)
		}
	}
	if f.Keyword != "" {
		like := "%" + escapeLike(f.Keyword) + "%"
		parts = append(parts, `(e.title LIKE ? ESCAPE '\' OR e.summary LIKE ? ESCAPE '\')`)
		args = append(args, like, like)
	}
	if f.SessionID != "" {
		parts = append(parts, `EXISTS (SELECT 1 FROM project_event_records r0 JOIN project_event_sources s0 ON s0.record_id=r0.id WHERE r0.event_id=e.id AND s0.session_id=?)`)
		args = append(args, f.SessionID)
	}
	if f.UpdatedFrom != "" {
		parts = append(parts, `e.updated_at >= ?`)
		args = append(args, f.UpdatedFrom)
	}
	if f.UpdatedTo != "" {
		parts = append(parts, `e.updated_at < ?`)
		args = append(args, f.UpdatedTo)
	}
	return parts, args
}

// ProjectEventRecordFilter 是时间线筛选；时间筛选只看已知 occurred_at，
// 事实时间未明确的记录另报数量，不当成「这期间发生」。
type ProjectEventRecordFilter struct {
	Keyword      string
	Kind         string
	OccurredFrom string
	OccurredTo   string
}

func (f ProjectEventRecordFilter) clause() ([]string, []any) {
	var parts []string
	var args []any
	if f.Kind != "" {
		parts = append(parts, `r.kind = ?`)
		args = append(args, f.Kind)
	}
	if f.Keyword != "" {
		like := "%" + escapeLike(f.Keyword) + "%"
		parts = append(parts, `r.text LIKE ? ESCAPE '\'`)
		args = append(args, like)
	}
	if f.OccurredFrom != "" {
		parts = append(parts, `r.occurred_at >= ?`)
		args = append(args, f.OccurredFrom)
	}
	if f.OccurredTo != "" {
		parts = append(parts, `r.occurred_at < ?`)
		args = append(args, f.OccurredTo)
	}
	return parts, args
}

func (f ProjectEventRecordFilter) hasTimeRange() bool {
	return f.OccurredFrom != "" || f.OccurredTo != ""
}

func eventWhereClause(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return " AND " + strings.Join(parts, " AND ")
}

// ---------- 行读取 ----------

const projectEventCols = `id, project_id, title, summary, status, revision, created_at, updated_at`

func scanProjectEvent(sc interface{ Scan(dest ...any) error }) (ProjectEvent, error) {
	var e ProjectEvent
	err := sc.Scan(&e.ID, &e.ProjectID, &e.Title, &e.Summary, &e.Status, &e.Revision, &e.CreatedAt, &e.UpdatedAt)
	if err == nil {
		e.StatusLabel = projectEventStatusLabels[e.Status]
	}
	return e, err
}

func readProjectEventRow(q projectReader, id string) (ProjectEvent, error) {
	e, err := scanProjectEvent(q.QueryRow(`SELECT `+projectEventCols+` FROM project_events WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectEvent{}, ErrProjectEventNotFound
	}
	return e, err
}

const projectEventRecordCols = `id, event_id, kind, text, occurred_at, origin, revision, created_at, updated_at`

func scanProjectEventRecord(sc interface{ Scan(dest ...any) error }) (ProjectEventRecord, error) {
	r := ProjectEventRecord{}
	var occurred sql.NullString
	err := sc.Scan(&r.ID, &r.EventID, &r.Kind, &r.Text, &occurred, &r.Origin, &r.Revision, &r.CreatedAt, &r.UpdatedAt)
	if err != nil {
		return r, err
	}
	r.OccurredAt = occurred.String
	r.KindLabel = projectEventKindLabels[r.Kind]
	return r, nil
}

func readProjectEventRecordRow(q projectReader, id string) (ProjectEventRecord, error) {
	r, err := scanProjectEventRecord(q.QueryRow(`SELECT `+projectEventRecordCols+` FROM project_event_records WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectEventRecord{}, ErrProjectEventRecordNotFound
	}
	return r, err
}

const projectEventSourceCols = `id, record_id, session_id, message_id, message_seq, message_digest, source_key, origin, linked_at`

func scanProjectEventSource(sc interface{ Scan(dest ...any) error }) (ProjectEventSource, error) {
	var s ProjectEventSource
	err := sc.Scan(&s.ID, &s.RecordID, &s.SessionID, &s.MessageID, &s.MessageSeq, &s.MessageDigest, &s.SourceKey, &s.Origin, &s.LinkedAt)
	return s, err
}

// readSessionTx 在事务内取会话当前状态（授权复核的权威副本）。
func readSessionTx(q projectReader, id string) (*SessionMeta, error) {
	m, err := scanSession(q.QueryRow(`SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("会话 %s 不存在", id)
	}
	return m, err
}

// eventCountsOf 派生计数：推进记录数、来源会话去重数。
func eventCountsOf(q projectReader, eventID string) (int, int, error) {
	var records, sessions int
	if err := q.QueryRow(`SELECT COUNT(*) FROM project_event_records WHERE event_id=?`, eventID).Scan(&records); err != nil {
		return 0, 0, err
	}
	if err := q.QueryRow(`SELECT COUNT(DISTINCT s.session_id) FROM project_event_sources s
		JOIN project_event_records r ON r.id=s.record_id WHERE r.event_id=?`, eventID).Scan(&sessions); err != nil {
		return 0, 0, err
	}
	return records, sessions, nil
}

func attachEventCounts(q projectReader, events []ProjectEvent) ([]ProjectEvent, error) {
	for i := range events {
		records, sessions, err := eventCountsOf(q, events[i].ID)
		if err != nil {
			return nil, err
		}
		events[i].RecordCount = records
		events[i].SessionCount = sessions
	}
	return events, nil
}

// recordSourceCounts 给一页记录填来源数（每页最多 40 条，逐条计数足够便宜）。
func recordSourceCounts(q projectReader, records []ProjectEventRecord) ([]ProjectEventRecord, error) {
	for i := range records {
		var n int
		if err := q.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, records[i].ID).Scan(&n); err != nil {
			return nil, err
		}
		records[i].SourceCount = n
	}
	return records, nil
}

// ---------- 读取：项目分组 / 事件页 ----------

// ProjectEventGroupCounts 是项目组头部计数；Matched 是当前筛选的匹配数。
type ProjectEventGroupCounts struct {
	Active     int `json:"active"`
	Discussing int `json:"discussing"`
	Processing int `json:"processing"`
	Awaiting   int `json:"awaiting"`
	Paused     int `json:"paused"`
	History    int `json:"history"`
	Matched    int `json:"matched"`
}

// ProjectEventGroup 是一组项目的事件预览；预览最多 3 条，独立翻页。
type ProjectEventGroup struct {
	ProjectID string                  `json:"project_id"`
	Counts    ProjectEventGroupCounts `json:"counts"`
	Events    []ProjectEvent          `json:"events"`
}

// ProjectEventGroups 返回一组项目的计数与预览（一次锁内完成，供跨项目总览）。
func (s *Store) ProjectEventGroups(projectIDs []string, filter ProjectEventFilter, preview int) (map[string]ProjectEventGroup, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]ProjectEventGroup, len(projectIDs))
	for _, pid := range projectIDs {
		group := ProjectEventGroup{ProjectID: pid, Events: []ProjectEvent{}}
		rows, err := s.db.Query(`SELECT status, COUNT(*) FROM project_events WHERE project_id=? GROUP BY status`, pid)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var status string
			var n int
			if err := rows.Scan(&status, &n); err != nil {
				rows.Close()
				return nil, err
			}
			switch status {
			case eventStatusDiscussing:
				group.Counts.Discussing += n
			case eventStatusProcessing:
				group.Counts.Processing += n
			case eventStatusAwaiting:
				group.Counts.Awaiting += n
			case eventStatusPaused:
				group.Counts.Paused += n
			case eventStatusDone, eventStatusCancelled:
				group.Counts.History += n
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		group.Counts.Active = group.Counts.Discussing + group.Counts.Processing + group.Counts.Awaiting + group.Counts.Paused

		parts, args := filter.clause()
		base := append([]any{pid}, args...)
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_events e WHERE e.project_id=?`+eventWhereClause(parts), base...).Scan(&group.Counts.Matched); err != nil {
			return nil, err
		}
		q := `SELECT ` + projectEventCols + ` FROM project_events e WHERE e.project_id=?` + eventWhereClause(parts) + ` ORDER BY e.updated_at DESC, e.id DESC LIMIT ?`
		qargs := append(append([]any{}, base...), preview)
		prows, err := s.db.Query(q, qargs...)
		if err != nil {
			return nil, err
		}
		for prows.Next() {
			e, err := scanProjectEvent(prows)
			if err != nil {
				prows.Close()
				return nil, err
			}
			group.Events = append(group.Events, e)
		}
		err = prows.Err()
		prows.Close()
		if err != nil {
			return nil, err
		}
		if group.Events, err = attachEventCounts(s.db, group.Events); err != nil {
			return nil, err
		}
		out[pid] = group
	}
	return out, nil
}

// ProjectEventPage 返回单项目的事件页（按 updated_at DESC, id DESC 稳定排序）。
func (s *Store) ProjectEventPage(projectID string, filter ProjectEventFilter, cursor *eventCursor, limit int) ([]ProjectEvent, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	parts, args := filter.clause()
	base := append([]any{projectID}, args...)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_events e WHERE e.project_id=?`+eventWhereClause(parts), base...).Scan(&total); err != nil {
		return nil, 0, 0, err
	}
	cparts := append([]string{}, parts...)
	cargs := append([]any{}, base...)
	if cursor != nil {
		cparts = append(cparts, `(e.updated_at, e.id) < (?, ?)`)
		cargs = append(cargs, cursor.Time, cursor.ID)
	}
	q := `SELECT ` + projectEventCols + ` FROM project_events e WHERE e.project_id=?` + eventWhereClause(cparts) + ` ORDER BY e.updated_at DESC, e.id DESC LIMIT ?`
	qargs := append(append([]any{}, cargs...), limit)
	rows, err := s.db.Query(q, qargs...)
	if err != nil {
		return nil, 0, 0, err
	}
	items := []ProjectEvent{}
	for rows.Next() {
		e, err := scanProjectEvent(rows)
		if err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		items = append(items, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	if items, err = attachEventCounts(s.db, items); err != nil {
		return nil, 0, 0, err
	}
	remaining := 0
	if len(items) > 0 {
		last := items[len(items)-1]
		rparts := append(append([]string{}, parts...), `(e.updated_at, e.id) < (?, ?)`)
		rargs := append(append([]any{}, base...), last.UpdatedAt, last.ID)
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_events e WHERE e.project_id=?`+eventWhereClause(rparts), rargs...).Scan(&remaining); err != nil {
			return nil, 0, 0, err
		}
	}
	return items, total, remaining, nil
}

// GetProjectEvent 读单个事件当前概况（不自动展开时间线）。
func (s *Store) GetProjectEvent(eventID string) (ProjectEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, err := readProjectEventRow(s.db, eventID)
	if err != nil {
		return ProjectEvent{}, err
	}
	records, sessions, err := eventCountsOf(s.db, e.ID)
	if err != nil {
		return ProjectEvent{}, err
	}
	e.RecordCount, e.SessionCount = records, sessions
	return e, nil
}

func (s *Store) GetProjectEventRecord(recordID string) (ProjectEventRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readProjectEventRecordRow(s.db, recordID)
}

// ---------- 读取：时间线 / 来源 ----------

// ProjectEventRecordPageResult 是一页时间线：页内始终按事实时间正序。
type ProjectEventRecordPageResult struct {
	Items          []ProjectEventRecord
	Total          int
	OlderRemaining int
	UnknownTime    int // 时间筛选下事实时间未明确的记录数（不猜归期）
}

// ProjectEventRecordPage 分页读取关键记录。
// direction: "older"（含空串＝最新一页）向过去翻；"newer" 向现在翻。
func (s *Store) ProjectEventRecordPage(eventID string, filter ProjectEventRecordFilter, direction string, cursor *eventCursor, limit int) (ProjectEventRecordPageResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res := ProjectEventRecordPageResult{Items: []ProjectEventRecord{}}
	parts, args := filter.clause()
	base := append([]any{eventID}, args...)
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_event_records r WHERE r.event_id=?`+eventWhereClause(parts), base...).Scan(&res.Total); err != nil {
		return res, err
	}
	if filter.hasTimeRange() {
		uparts := append(append([]string{}, parts...), `r.occurred_at IS NULL`)
		uargs := append([]any{}, base...)
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_event_records r WHERE r.event_id=?`+eventWhereClause(uparts), uargs...).Scan(&res.UnknownTime); err != nil {
			return res, err
		}
	}
	cparts := append([]string{}, parts...)
	cargs := append([]any{}, base...)
	order := `DESC`
	if direction == "newer" {
		order = `ASC`
	}
	if cursor != nil {
		op := `<`
		if direction == "newer" {
			op = `>`
		}
		cparts = append(cparts, `(COALESCE(r.occurred_at, r.created_at), r.id) `+op+` (?, ?)`)
		cargs = append(cargs, cursor.Time, cursor.ID)
	}
	q := `SELECT ` + projectEventRecordCols + ` FROM project_event_records r WHERE r.event_id=?` + eventWhereClause(cparts) +
		` ORDER BY COALESCE(r.occurred_at, r.created_at) ` + order + `, r.id ` + order + ` LIMIT ?`
	qargs := append(append([]any{}, cargs...), limit)
	rows, err := s.db.Query(q, qargs...)
	if err != nil {
		return res, err
	}
	for rows.Next() {
		r, err := scanProjectEventRecord(rows)
		if err != nil {
			rows.Close()
			return res, err
		}
		res.Items = append(res.Items, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return res, err
	}
	if direction != "newer" {
		// 最新一页用 DESC 取到，页内翻回正序：时间线永远从早到晚展示。
		for i, j := 0, len(res.Items)-1; i < j; i, j = i+1, j-1 {
			res.Items[i], res.Items[j] = res.Items[j], res.Items[i]
		}
	}
	if len(res.Items) > 0 {
		first := res.Items[0]
		firstKey := first.CreatedAt
		if first.OccurredAt != "" {
			firstKey = first.OccurredAt
		}
		rparts := append(append([]string{}, parts...), `(COALESCE(r.occurred_at, r.created_at), r.id) < (?, ?)`)
		rargs := append(append([]any{}, base...), firstKey, first.ID)
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_event_records r WHERE r.event_id=?`+eventWhereClause(rparts), rargs...).Scan(&res.OlderRemaining); err != nil {
			return res, err
		}
	}
	if res.Items, err = recordSourceCounts(s.db, res.Items); err != nil {
		return res, err
	}
	return res, nil
}

// ProjectEventSourcePage 分页读取来源（按绑定时间倒序，不代表事实新旧）。
func (s *Store) ProjectEventSourcePage(recordID string, cursor *eventCursor, limit int) ([]ProjectEventSource, int, int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, recordID).Scan(&total); err != nil {
		return nil, 0, 0, err
	}
	q := `SELECT ` + projectEventSourceCols + ` FROM project_event_sources WHERE record_id=?`
	qargs := []any{recordID}
	if cursor != nil {
		q += ` AND (linked_at, id) < (?, ?)`
		qargs = append(qargs, cursor.Time, cursor.ID)
	}
	q += ` ORDER BY linked_at DESC, id DESC LIMIT ?`
	qargs = append(qargs, limit)
	rows, err := s.db.Query(q, qargs...)
	if err != nil {
		return nil, 0, 0, err
	}
	items := []ProjectEventSource{}
	for rows.Next() {
		src, err := scanProjectEventSource(rows)
		if err != nil {
			rows.Close()
			return nil, 0, 0, err
		}
		items = append(items, src)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, 0, 0, err
	}
	remaining := 0
	if len(items) > 0 {
		last := items[len(items)-1]
		if err := s.db.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=? AND (linked_at, id) < (?, ?)`, recordID, last.LinkedAt, last.ID).Scan(&remaining); err != nil {
			return nil, 0, 0, err
		}
	}
	return items, total, remaining, nil
}

// ProjectEventRecordSources 取一页记录各自的来源入口（最多每条 n 条）。
func (s *Store) ProjectEventRecordSources(recordIDs []string, n int) (map[string][]ProjectEventSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string][]ProjectEventSource, len(recordIDs))
	for _, id := range recordIDs {
		rows, err := s.db.Query(`SELECT `+projectEventSourceCols+` FROM project_event_sources WHERE record_id=? ORDER BY linked_at DESC, id DESC LIMIT ?`, id, n)
		if err != nil {
			return nil, err
		}
		list := []ProjectEventSource{}
		for rows.Next() {
			src, err := scanProjectEventSource(rows)
			if err != nil {
				rows.Close()
				return nil, err
			}
			list = append(list, src)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
		out[id] = list
	}
	return out, nil
}

func (s *Store) GetProjectEventSource(sourceID string) (ProjectEventSource, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	src, err := scanProjectEventSource(s.db.QueryRow(`SELECT `+projectEventSourceCols+` FROM project_event_sources WHERE id=?`, sourceID))
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectEventSource{}, fmt.Errorf("来源不存在")
	}
	return src, err
}

// MessageFingerprint 读一条会话消息的行 ID 与正文摘要（不返回正文）。
func (s *Store) MessageFingerprint(sessionID string, seq int64) (int64, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	var role string
	var content sql.NullString
	err := s.db.QueryRow(`SELECT id, role, content FROM messages WHERE session_id=? AND seq=?`, sessionID, seq).Scan(&id, &role, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", fmt.Errorf("来源会话 %s 没有 seq=%d 的消息", sessionID, seq)
	}
	if err != nil {
		return 0, "", err
	}
	return id, messageDigest(role, content.String), nil
}

// MessageRowExists 只判消息行是否还在（`SELECT id`，不读正文）：来源列表默认不预读
// 正文，正文指纹只对显式 source_id 定点复核一条才读。
func (s *Store) MessageRowExists(id int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var got int64
	err := s.db.QueryRow(`SELECT id FROM messages WHERE id=?`, id).Scan(&got)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// MessageRowByID 按行 ID 复核来源：行被删或 seq 复用后行 ID 会变，不能只看 seq。
func (s *Store) MessageRowByID(id int64) (int64, string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var role string
	var content sql.NullString
	err := s.db.QueryRow(`SELECT id, role, content FROM messages WHERE id=?`, id).Scan(&id, &role, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, "", false, nil
	}
	if err != nil {
		return 0, "", false, err
	}
	return id, messageDigest(role, content.String), true, nil
}

// MessageSeqOwner 返回该会话该 seq 当前对应的消息行 ID（用于识别「seq 被复用」）。
func (s *Store) MessageSeqOwner(sessionID string, seq int64) (int64, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var id int64
	err := s.db.QueryRow(`SELECT id FROM messages WHERE session_id=? AND seq=?`, sessionID, seq).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return id, true, nil
}

// ---------- 写入 ----------

// insertEventSource 在事务内复核来源存在性与消息归属，再落关系；重复来源是 no-op。
func insertEventSource(tx *sql.Tx, recordID string, src ProjectEventSourceInput, origin, now string) error {
	if strings.TrimSpace(src.SessionID) == "" {
		return fmt.Errorf("来源必须给出 session_id")
	}
	src.SessionID = strings.TrimSpace(src.SessionID)
	var exists int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sessions WHERE id=?`, src.SessionID).Scan(&exists); err != nil {
		return err
	}
	if exists == 0 {
		return fmt.Errorf("来源会话 %s 不存在；请绑定实际依据会话，不要编造来源", src.SessionID)
	}
	var msgID int64
	var digest string
	key := "session"
	if src.MessageSeq > 0 {
		var role string
		var content sql.NullString
		err := tx.QueryRow(`SELECT id, role, content FROM messages WHERE session_id=? AND seq=?`, src.SessionID, src.MessageSeq).Scan(&msgID, &role, &content)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("来源会话 %s 没有 seq=%d 的消息；只能绑定实际存在的消息位置", src.SessionID, src.MessageSeq)
		}
		if err != nil {
			return err
		}
		digest = messageDigest(role, content.String)
		key = "m" + strconv.FormatInt(msgID, 10)
	}
	id, err := newProjectID("src-")
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO project_event_sources(id, record_id, session_id, message_id, message_seq, message_digest, source_key, origin, linked_at)
		VALUES(?,?,?,?,?,?,?,?,?) ON CONFLICT(record_id, session_id, source_key) DO NOTHING`,
		id, recordID, src.SessionID, msgID, src.MessageSeq, digest, key, origin, now)
	return err
}

// eventRecordByClientKey 判幂等命中：同键同载荷返回此前提交的那一行，
// 同键异载荷是冲突（不能拿同一个键写两条不同的事实）。
func eventRecordByClientKey(tx *sql.Tx, eventID, key, digest string) (ProjectEventRecord, bool, error) {
	var id, stored string
	err := tx.QueryRow(`SELECT id, request_digest FROM project_event_records WHERE event_id=? AND client_key=?`, eventID, key).Scan(&id, &stored)
	if errors.Is(err, sql.ErrNoRows) {
		return ProjectEventRecord{}, false, nil
	}
	if err != nil {
		return ProjectEventRecord{}, false, err
	}
	if stored != digest {
		return ProjectEventRecord{}, false, ErrProjectEventIdempotent
	}
	rec, err := readProjectEventRecordRow(tx, id)
	return rec, true, err
}

// eventWithCounts 读当前事件概况并填派生计数（回执用，不展开时间线）。
func eventWithCounts(tx *sql.Tx, eventID string) (ProjectEvent, error) {
	ev, err := readProjectEventRow(tx, eventID)
	if err != nil {
		return ProjectEvent{}, err
	}
	records, sessions, err := eventCountsOf(tx, eventID)
	if err != nil {
		return ProjectEvent{}, err
	}
	ev.RecordCount, ev.SessionCount = records, sessions
	return ev, nil
}

// insertEventRecord 追加一条关键事实；client_key 命中时按请求摘要判幂等。
// created=false 表示这条事实此前已提交（重试命中），调用方不应再刷新父事件 revision。
func insertEventRecord(tx *sql.Tx, eventID string, in ProjectEventRecordInput, origin, now string) (ProjectEventRecord, bool, error) {
	if in.ClientKey != "" {
		rec, hit, err := eventRecordByClientKey(tx, eventID, in.ClientKey, in.RequestDigest)
		if err != nil {
			return ProjectEventRecord{}, false, err
		}
		if hit {
			return rec, false, nil
		}
	}
	id, err := newProjectID("rec-")
	if err != nil {
		return ProjectEventRecord{}, false, err
	}
	var occurred any
	if in.OccurredAt != "" {
		occurred = in.OccurredAt
	}
	if _, err := tx.Exec(`INSERT INTO project_event_records(id, event_id, kind, text, occurred_at, origin, revision, client_key, request_digest, created_at, updated_at)
		VALUES(?,?,?,?,?,?,1,?,?,?,?)`, id, eventID, in.Kind, in.Text, occurred, origin, in.ClientKey, in.RequestDigest, now, now); err != nil {
		return ProjectEventRecord{}, false, err
	}
	for _, src := range in.Sources {
		if err := insertEventSource(tx, id, src, origin, now); err != nil {
			return ProjectEventRecord{}, false, err
		}
	}
	rec, err := readProjectEventRecordRow(tx, id)
	return rec, true, err
}

// eventWriteCtx 描述一次写入的发起方：AI 会话（带事务内授权回调）或设置页用户（无会话）。
// 用户路径同样在事务内重查项目与归档状态；只是不需要会话授权——用户身份由
// projectSettingsRequest 在路由层卡死，AI 带调用者头一律 403。
type eventWriteCtx struct {
	sessionID string
	authorize eventWriteAuth
	origin    string
}

func (wc eventWriteCtx) check(tx *sql.Tx, p Project) error {
	if wc.authorize == nil {
		return nil
	}
	if wc.sessionID == "" {
		return ErrProjectAuthorizationRequired
	}
	sess, err := readSessionTx(tx, wc.sessionID)
	if err != nil {
		return err
	}
	return wc.authorize(tx, p, sess)
}

// CreateProjectEventForSession 新建大事件；非讨论中状态必须同事务附一条关键事实。
func (s *Store) CreateProjectEventForSession(projectID string, in ProjectEventCreateInput, sessionID string, authorize eventWriteAuth) (ProjectEvent, bool, error) {
	return s.createProjectEvent(projectID, in, eventWriteCtx{sessionID: sessionID, authorize: authorize, origin: eventOriginAI})
}

// CreateProjectEventAsUser 是设置页用户新建（无会话、origin=user，同样过归档与幂等检查）。
func (s *Store) CreateProjectEventAsUser(projectID string, in ProjectEventCreateInput) (ProjectEvent, bool, error) {
	return s.createProjectEvent(projectID, in, eventWriteCtx{origin: eventOriginUser})
}

func (s *Store) createProjectEvent(projectID string, in ProjectEventCreateInput, wc eventWriteCtx) (ProjectEvent, bool, error) {
	if wc.authorize != nil && wc.sessionID == "" {
		return ProjectEvent{}, false, ErrProjectAuthorizationRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ProjectEvent{}, false, err
	}
	defer tx.Rollback()
	p, err := readProject(tx, projectID)
	if err != nil {
		return ProjectEvent{}, false, err
	}
	if err := wc.check(tx, p); err != nil {
		return ProjectEvent{}, false, err
	}
	if p.Archived {
		return ProjectEvent{}, false, ErrProjectEventArchived
	}
	if in.CreateKey != "" {
		var id, digest string
		err := tx.QueryRow(`SELECT id, create_digest FROM project_events WHERE project_id=? AND create_key=?`, projectID, in.CreateKey).Scan(&id, &digest)
		switch {
		case err == nil:
			if digest != in.CreateDigest {
				return ProjectEvent{}, false, ErrProjectEventIdempotent
			}
			ev, err := readProjectEventRow(tx, id)
			if err != nil {
				return ProjectEvent{}, false, err
			}
			records, sessions, err := eventCountsOf(tx, ev.ID)
			if err != nil {
				return ProjectEvent{}, false, err
			}
			ev.RecordCount, ev.SessionCount = records, sessions
			return ev, false, nil
		case !errors.Is(err, sql.ErrNoRows):
			return ProjectEvent{}, false, err
		}
	}
	id, err := newProjectID("ev-")
	if err != nil {
		return ProjectEvent{}, false, err
	}
	now := projectEventNow()
	if _, err := tx.Exec(`INSERT INTO project_events(id, project_id, title, summary, status, revision, create_key, create_digest, created_at, updated_at)
		VALUES(?,?,?,?,?,1,?,?,?,?)`, id, projectID, in.Title, in.Summary, in.Status, in.CreateKey, in.CreateDigest, now, now); err != nil {
		return ProjectEvent{}, false, err
	}
	if in.Record != nil {
		if _, _, err := insertEventRecord(tx, id, *in.Record, wc.origin, now); err != nil {
			return ProjectEvent{}, false, err
		}
	}
	ev, err := readProjectEventRow(tx, id)
	if err != nil {
		return ProjectEvent{}, false, err
	}
	records, sessions, err := eventCountsOf(tx, id)
	if err != nil {
		return ProjectEvent{}, false, err
	}
	ev.RecordCount, ev.SessionCount = records, sessions
	if err := tx.Commit(); err != nil {
		return ProjectEvent{}, false, err
	}
	return ev, true, nil
}

// MutateProjectEventForSession 在一个事务里改概况/状态并可选追加一条关键事实。
// changed=false 表示没有任何有效内容变化（相同值保存、幂等重试、重复来源），
// 此时不刷新 revision 与 updated_at。
func (s *Store) MutateProjectEventForSession(eventID string, expectedRevision int64, patch ProjectEventPatchInput, rec *ProjectEventRecordInput, sessionID string, authorize eventWriteAuth) (ProjectEvent, ProjectEventRecord, bool, error) {
	return s.mutateProjectEvent(eventID, expectedRevision, patch, rec, eventWriteCtx{sessionID: sessionID, authorize: authorize, origin: eventOriginAI})
}

// MutateProjectEventAsUser 是设置页用户的概况/状态编辑（用户的显式更正不需要附事实）。
func (s *Store) MutateProjectEventAsUser(eventID string, expectedRevision int64, patch ProjectEventPatchInput, rec *ProjectEventRecordInput) (ProjectEvent, ProjectEventRecord, bool, error) {
	return s.mutateProjectEvent(eventID, expectedRevision, patch, rec, eventWriteCtx{origin: eventOriginUser})
}

// mutateProjectEvent 在一个事务里改概况/状态并可选追加一条关键事实。
// 返回的 ProjectEventRecord 就是本次提交（或幂等命中）的那一行：调用方不得在提交后
// 再按「最新一条」重读——晚到的旧事实会让「最新一条」指向别的记录（设计 §9.2）。
// changed=false 表示没有任何有效内容变化（相同值保存、幂等重试、重复来源），
// 此时不刷新 revision 与 updated_at。
func (s *Store) mutateProjectEvent(eventID string, expectedRevision int64, patch ProjectEventPatchInput, rec *ProjectEventRecordInput, wc eventWriteCtx) (ProjectEvent, ProjectEventRecord, bool, error) {
	none := ProjectEventRecord{}
	if wc.authorize != nil && wc.sessionID == "" {
		return ProjectEvent{}, none, false, ErrProjectAuthorizationRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ProjectEvent{}, none, false, err
	}
	defer tx.Rollback()
	ev, err := readProjectEventRow(tx, eventID)
	if err != nil {
		return ProjectEvent{}, none, false, err
	}
	p, err := readProject(tx, ev.ProjectID)
	if err != nil {
		return ProjectEvent{}, none, false, err
	}
	if err := wc.check(tx, p); err != nil {
		return ProjectEvent{}, none, false, err
	}
	if p.Archived {
		return ProjectEvent{}, none, false, ErrProjectEventArchived
	}
	// 幂等命中先于父 revision 校验：同键同载荷是「已提交请求的重发」，父事件后来前进
	// 不能把它误判成新请求（设计 §7.1）；授权、项目状态与幂等仍先核。
	if rec != nil && rec.ClientKey != "" {
		committed, hit, err := eventRecordByClientKey(tx, eventID, rec.ClientKey, rec.RequestDigest)
		if err != nil {
			return ProjectEvent{}, none, false, err
		}
		if hit {
			current, err := eventWithCounts(tx, eventID)
			if err != nil {
				return ProjectEvent{}, none, false, err
			}
			return current, committed, false, nil
		}
	}
	if ev.Revision != expectedRevision {
		return ProjectEvent{}, none, false, ErrProjectEventRevisionConflict
	}
	title, summary, status := ev.Title, ev.Summary, ev.Status
	if patch.Title != nil {
		title = *patch.Title
	}
	if patch.Summary != nil {
		summary = *patch.Summary
	}
	if patch.Status != nil {
		status = *patch.Status
	}
	// 状态实际变化必须同事务附一条关键事实；在事务里判，避免事务外读到旧状态。
	if status != ev.Status && rec == nil {
		return ProjectEvent{}, none, false, fmt.Errorf("%w：状态从「%s」变到「%s」必须同时提交一条有依据的关键事实",
			ErrProjectEventRecordNeedsSource, projectEventStatusLabels[ev.Status], projectEventStatusLabels[status])
	}
	changed := title != ev.Title || summary != ev.Summary || status != ev.Status
	var recRow ProjectEventRecord
	if rec != nil {
		created := false
		recRow, created, err = insertEventRecord(tx, eventID, *rec, wc.origin, projectEventNow())
		if err != nil {
			return ProjectEvent{}, none, false, err
		}
		if created {
			changed = true
		}
	}
	if !changed {
		current, err := eventWithCounts(tx, eventID)
		if err != nil {
			return ProjectEvent{}, none, false, err
		}
		return current, recRow, false, nil
	}
	now := projectEventNow()
	if _, err := tx.Exec(`UPDATE project_events SET title=?, summary=?, status=?, revision=revision+1, updated_at=? WHERE id=? AND revision=?`,
		title, summary, status, now, eventID, expectedRevision); err != nil {
		return ProjectEvent{}, none, false, err
	}
	current, err := eventWithCounts(tx, eventID)
	if err != nil {
		return ProjectEvent{}, none, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectEvent{}, none, false, err
	}
	return current, recRow, true, nil
}

// PatchProjectEventRecordForSession 更正一条记录（原位纠错，不建旧正文历史）并增删来源。
// 记录内容或来源有任何有效变化时，记录与父事件 revision 一并递增。
func (s *Store) PatchProjectEventRecordForSession(recordID string, expectedRecordRevision, expectedEventRevision int64, patch ProjectEventRecordPatchInput, sessionID string, authorize eventWriteAuth) (ProjectEvent, ProjectEventRecord, bool, error) {
	return s.patchProjectEventRecord(recordID, expectedRecordRevision, expectedEventRevision, patch, eventWriteCtx{sessionID: sessionID, authorize: authorize, origin: eventOriginAI})
}

// PatchProjectEventRecordAsUser 是设置页用户的记录更正。
func (s *Store) PatchProjectEventRecordAsUser(recordID string, expectedRecordRevision, expectedEventRevision int64, patch ProjectEventRecordPatchInput) (ProjectEvent, ProjectEventRecord, bool, error) {
	return s.patchProjectEventRecord(recordID, expectedRecordRevision, expectedEventRevision, patch, eventWriteCtx{origin: eventOriginUser})
}

func (s *Store) patchProjectEventRecord(recordID string, expectedRecordRevision, expectedEventRevision int64, patch ProjectEventRecordPatchInput, wc eventWriteCtx) (ProjectEvent, ProjectEventRecord, bool, error) {
	if wc.authorize != nil && wc.sessionID == "" {
		return ProjectEvent{}, ProjectEventRecord{}, false, ErrProjectAuthorizationRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	defer tx.Rollback()
	rec, err := readProjectEventRecordRow(tx, recordID)
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	ev, err := readProjectEventRow(tx, rec.EventID)
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	p, err := readProject(tx, ev.ProjectID)
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	if err := wc.check(tx, p); err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	if p.Archived {
		return ProjectEvent{}, ProjectEventRecord{}, false, ErrProjectEventArchived
	}
	if ev.Revision != expectedEventRevision {
		return ProjectEvent{}, ProjectEventRecord{}, false, ErrProjectEventRevisionConflict
	}
	if rec.Revision != expectedRecordRevision {
		return ProjectEvent{}, ProjectEventRecord{}, false, ErrProjectEventRecordConflict
	}
	now := projectEventNow()
	text, kind, occurred := rec.Text, rec.Kind, rec.OccurredAt
	if patch.Text != nil {
		text = *patch.Text
	}
	if patch.Kind != nil {
		kind = *patch.Kind
	}
	if patch.OccurredAt != nil {
		occurred = *patch.OccurredAt
	}
	changed := text != rec.Text || kind != rec.Kind || occurred != rec.OccurredAt
	removed := 0
	for _, sid := range patch.SourcesRemove {
		res, err := tx.Exec(`DELETE FROM project_event_sources WHERE id=? AND record_id=?`, sid, recordID)
		if err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		if n, err := res.RowsAffected(); err == nil && n > 0 {
			changed = true
			removed += int(n)
		}
	}
	for _, src := range patch.SourcesAdd {
		var before int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, recordID).Scan(&before); err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		if err := insertEventSource(tx, recordID, src, wc.origin, now); err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		var after int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, recordID).Scan(&after); err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		if after > before {
			changed = true
		}
	}
	// 设计 §4.4：显式移除来源不能把原 AI 事实变成无来源记录（用户录入的无来源事实不受此限）。
	if removed > 0 && rec.Origin == eventOriginAI {
		var left int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, recordID).Scan(&left); err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		if left == 0 {
			return ProjectEvent{}, ProjectEventRecord{}, false, fmt.Errorf("%w：AI 记录的来源不能删空；请补上相应依据，误录记录请由用户在设置页确认删除", ErrProjectEventSourceRequired)
		}
	}
	if !changed {
		ev, err = readProjectEventRow(tx, ev.ID)
		if err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		records, sessions, err := eventCountsOf(tx, ev.ID)
		if err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		ev.RecordCount, ev.SessionCount = records, sessions
		// 无变化也要回报当前来源数：调用方拿它刷新界面，不能因为没写入就报 0。
		if err := tx.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, recordID).Scan(&rec.SourceCount); err != nil {
			return ProjectEvent{}, ProjectEventRecord{}, false, err
		}
		return ev, rec, false, nil
	}
	var occurredAny any
	if occurred != "" {
		occurredAny = occurred
	}
	if _, err := tx.Exec(`UPDATE project_event_records SET text=?, kind=?, occurred_at=?, revision=revision+1, updated_at=? WHERE id=? AND revision=?`,
		text, kind, occurredAny, now, recordID, expectedRecordRevision); err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	if _, err := tx.Exec(`UPDATE project_events SET revision=revision+1, updated_at=? WHERE id=?`, now, ev.ID); err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	rec, err = readProjectEventRecordRow(tx, recordID)
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	ev, err = readProjectEventRow(tx, ev.ID)
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	records, sessions, err := eventCountsOf(tx, ev.ID)
	if err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	ev.RecordCount, ev.SessionCount = records, sessions
	if err := tx.QueryRow(`SELECT COUNT(*) FROM project_event_sources WHERE record_id=?`, recordID).Scan(&rec.SourceCount); err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return ProjectEvent{}, ProjectEventRecord{}, false, err
	}
	return ev, rec, true, nil
}

// AddProjectEventRecordForSession 追加关键事实（可同时显式 patch 概况/状态）。
func (s *Store) AddProjectEventRecordForSession(eventID string, expectedEventRevision int64, patch ProjectEventPatchInput, rec ProjectEventRecordInput, sessionID string, authorize eventWriteAuth) (ProjectEvent, ProjectEventRecord, bool, error) {
	return s.addProjectEventRecord(eventID, expectedEventRevision, patch, rec, eventWriteCtx{sessionID: sessionID, authorize: authorize, origin: eventOriginAI})
}

// AddProjectEventRecordAsUser 是设置页用户的记录追加。
func (s *Store) AddProjectEventRecordAsUser(eventID string, expectedEventRevision int64, patch ProjectEventPatchInput, rec ProjectEventRecordInput) (ProjectEvent, ProjectEventRecord, bool, error) {
	return s.addProjectEventRecord(eventID, expectedEventRevision, patch, rec, eventWriteCtx{origin: eventOriginUser})
}

func (s *Store) addProjectEventRecord(eventID string, expectedEventRevision int64, patch ProjectEventPatchInput, rec ProjectEventRecordInput, wc eventWriteCtx) (ProjectEvent, ProjectEventRecord, bool, error) {
	// 回执直接用事务内落库（或幂等命中）的那一行：提交后按「最新一条」重读会把
	// 晚到的旧事实说成别的事实，调用方拿着错的 record_id 去更正/查来源就会打空。
	return s.mutateProjectEvent(eventID, expectedEventRevision, patch, &rec, wc)
}

// DeleteProjectEvent 永久删除一个大事件（仅用户设置页确认后调用）：级联删记录与来源，
// 不删来源会话、消息、代码或工作区文件。
func (s *Store) DeleteProjectEvent(eventID string, confirmed bool) error {
	if !confirmed {
		return ErrProjectEventDeleteConfirm
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM project_events WHERE id=?`, eventID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrProjectEventNotFound
	}
	return nil
}

// DeleteProjectEventRecord 删除一条误录的推进记录（仅用户确认后调用）。
func (s *Store) DeleteProjectEventRecord(recordID string, confirmed bool) error {
	if !confirmed {
		return ErrProjectEventDeleteConfirm
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rec, err := readProjectEventRecordRow(tx, recordID)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM project_event_records WHERE id=?`, recordID); err != nil {
		return err
	}
	// 派生计数现算，不需要维护冗余列；父事件只刷新修改时间与 revision。
	if _, err := tx.Exec(`UPDATE project_events SET revision=revision+1, updated_at=? WHERE id=?`, projectEventNow(), rec.EventID); err != nil {
		return err
	}
	return tx.Commit()
}
