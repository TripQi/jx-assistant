package main

// 项目事件（组件 project-events）：跨项目大目标索引 → 关键推进时间线 → 来源会话/消息。
//
// 设计意图（人的判断，别在实现细节里重复）：
//   - 事件只回答「目标是什么、现在怎样、哪些关键事实支撑判断、去哪里核实」；
//     协议正文、代码、日志与完整会话仍由原载体保存，事件里绝不复制。
//   - 严格分层读取：总览只给分组与概况，单事件才给时间线，来源只在被问到时
//     才给元数据，正文永远交给既有历史工具按需读取。
//   - 独立组件的写权限不等于知识库的写权限：知识库只有长期助理能写，事件允许
//     五种工作模式维护自身项目；但只读答疑（aux）、只读档、计划模式一律拒写，
//     且写事务内用权威会话复核一遍（不信任调用前的运行指针）。
//   - 事实与状态分开：状态变化、直接创建非讨论中事件必须附一条有依据的关键事实；
//     仅改标题/概况（哪怕当前状态非讨论中）不强制补事实，不制造无意义记录。

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

const projectEventsComponentID = "project-events"

// 已确认的字段预算（预算单元，非字符数、非 token）：各字段独立计数，额度不可挪用。
const (
	projectEventTitleBudget   int64 = 80
	projectEventSummaryBudget int64 = 240
	projectEventTextBudget    int64 = 240
	projectEventPageMax             = 40
	projectEventPreviewLimit        = 3
)

func init() {
	registerComponentCapability(projectEventsComponentID, "project_events_overview", runProjectEventsOverview)
	registerComponentCapability(projectEventsComponentID, "project_events_query", runProjectEventsQuery)
	registerComponentCapability(projectEventsComponentID, "project_event_get", runProjectEventGet)
	registerComponentCapability(projectEventsComponentID, "project_event_records_query", runProjectEventRecordsQuery)
	registerComponentCapability(projectEventsComponentID, "project_event_sources_query", runProjectEventSourcesQuery)
	registerComponentCapability(projectEventsComponentID, "project_event_create", runProjectEventCreate)
	registerComponentCapability(projectEventsComponentID, "project_event_update", runProjectEventUpdate)
	registerComponentCapability(projectEventsComponentID, "project_event_record_add", runProjectEventRecordAdd)
	registerComponentCapability(projectEventsComponentID, "project_event_record_update", runProjectEventRecordUpdate)
}

// projectEventBudgetError 超预算的字段回执：指明字段、已用/上限、超出量与精简方向，
// 不静默截断，也不借用知识库「删旧内容/扩容」的措辞——事件只精简本次字段。
type projectEventBudgetError struct {
	Field string
	Label string
	Used  int64
	Limit int64
}

func (e *projectEventBudgetError) Error() string {
	return fmt.Sprintf("%s超出预算：已用 %d / %d 单元，超出 %d；请精简为目标短语/一句概况/一句关键事实后重试（不得截断）。字段额度独立，不能挪用标题或其它记录的余量",
		e.Label, e.Used, e.Limit, e.Used-e.Limit)
}

func eventBudgetCheck(field, label, value string, limit int64) (string, error) {
	if used := ProjectKnowledgeBudget(value); used > limit {
		return value, &projectEventBudgetError{Field: field, Label: label, Used: used, Limit: limit}
	}
	return value, nil
}

var eventListPrefix = regexp.MustCompile(`^\s*(?:[-*•]\s|\d+[.)、]\s)`)

// normalizeEventText 做单行/一句校验：换行、列表、分号拼接流水一律拒写；
// 句末标点只允许出现在结尾（内部点号如版本号、域名、路径不机械当句界）。
func normalizeEventText(field, label, value string, limit int64) (string, error) {
	v := strings.TrimSpace(value)
	if v == "" {
		return "", fmt.Errorf("%s不能为空；请写一句可理解的关键事实", label)
	}
	if strings.ContainsAny(v, "\r\n") {
		return "", fmt.Errorf("%s只能是单行；不要写入换行、列表或执行流水", label)
	}
	if eventListPrefix.MatchString(v) {
		return "", fmt.Errorf("%s不要写成列表项；请合成一句关键事实", label)
	}
	if strings.ContainsAny(v, "；;") {
		return "", fmt.Errorf("%s不要用分号拼接多条流水；请合成一句关键事实，多条事实请分别追加记录", label)
	}
	body := strings.TrimRight(v, "。！？")
	if strings.ContainsAny(body, "。！？") {
		return "", fmt.Errorf("%s应是一句话：请把「已修复。等待验收。」这类多句合成一句（如「已完成修复，等待验收。」）", label)
	}
	return eventBudgetCheck(field, label, v, limit)
}

// normalizeEventStatus 校验六状态；空串返回空（调用方决定缺省）。
func normalizeEventStatus(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", nil
	}
	if _, ok := projectEventStatusLabels[s]; !ok {
		keys := make([]string, 0, len(projectEventStatusLabels))
		for k := range projectEventStatusLabels {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		return "", fmt.Errorf("状态 %q 无效；可用：%s（讨论中/处理中/待验收/暂停/已完成/取消）", raw, strings.Join(keys, "、"))
	}
	return s, nil
}

func normalizeEventKind(raw string) (string, error) {
	k := strings.TrimSpace(raw)
	if k == "" {
		return eventKindOther, nil
	}
	if _, ok := projectEventKindLabels[k]; !ok {
		keys := make([]string, 0, len(projectEventKindLabels))
		for key := range projectEventKindLabels {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		return "", fmt.Errorf("记录类型 %q 无效；可用：%s", raw, strings.Join(keys, "、"))
	}
	return k, nil
}

// normalizeEventTime 把带偏移的时间规范成 UTC 三位毫秒文本；空串＝事实时间未明确。
func normalizeEventTime(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", nil
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, raw); err == nil {
			return projectEventTimeOf(t), nil
		}
	}
	return "", fmt.Errorf("时间 %q 格式无效：请用带时区的 RFC3339（如 2026-11-16T09:30:00+08:00）；无法确定事实时刻就留空，不要编造精度", raw)
}

// ---------- 会话与权限交集 ----------

func (a *App) projectEventsEnabled(sess *SessionMeta) bool {
	if a.Components == nil || sess == nil {
		return false
	}
	if _, ok := a.Components.Manifest(projectEventsComponentID); !ok {
		return false
	}
	return a.cxoComponentEnabled(sess.ID, projectEventsComponentID)
}

func projectNotRegisteredForEvents() error {
	return fmt.Errorf("当前工作区尚未登记项目：项目事件需要项目身份。请到 设置 → 项目事件（或项目知识库）新建项目并绑定工作区；不要擅自登记、写进其他项目或创建收件箱")
}

// projectEventCapabilityContext 读一次权威会话（Store.Get）后再判门：不信任运行中内存副本。
func projectEventCapabilityContext(t *toolContext, write bool) error {
	if t == nil || t.app == nil || t.app.Store == nil || t.sess == nil || t.sess.ID == "" {
		return fmt.Errorf("缺少合法项目会话上下文或模式不支持")
	}
	fresh, err := t.app.Store.Get(t.sess.ID)
	if err != nil || fresh == nil {
		return fmt.Errorf("无法读取当前项目会话授权，拒绝访问")
	}
	mergeRunAuthorization(t.sess, fresh)
	return projectEventSessionContext(t, write)
}

// projectEventSessionContext 只校验传入的权威快照；写事务内可复用（不重入 Store）。
func projectEventSessionContext(t *toolContext, write bool) error {
	if t == nil || t.sess == nil {
		return fmt.Errorf("缺少会话上下文，拒绝访问（fail-closed）")
	}
	if !projectReadableMode(t.sess.Mode) {
		return fmt.Errorf("缺少合法项目会话上下文或模式不支持")
	}
	if !t.app.projectEventsEnabled(t.sess) {
		return fmt.Errorf("项目事件组件在本会话未启用；请到 设置 → 组件 或 加号菜单「本对话的组件与插件」打开")
	}
	if !write {
		return nil
	}
	if t.sess.Mode == "aux" {
		return fmt.Errorf("只读答疑（aux）只能查询项目事件，不能新建或更正；请由工作任务或长期助理维护")
	}
	if t.sess.PlanMode {
		return fmt.Errorf("计划模式下不能写入项目事件：先用只读能力调研，exit_plan_mode 获批后再写")
	}
	return requireNotReadOnly(t, "维护项目事件")
}

// eventScope 是调用者的项目视野：长期助理跨项目，其余模式只看自身工作区绑定项目。
type eventScope struct {
	assistant bool
	own       *Project
}

func (a *App) eventScopeFor(t *toolContext) (eventScope, error) {
	if t.sess.Mode == "assistant" {
		return eventScope{assistant: true}, nil
	}
	own, err := a.resolveSessionProject(t.sess)
	if err != nil {
		return eventScope{}, err
	}
	if own == nil {
		return eventScope{}, projectNotRegisteredForEvents()
	}
	return eventScope{own: own}, nil
}

// eventProject 取调用者有权访问的项目：普通会话只能是自己绑定的项目；
// 长期助理必须显式给 project_id（先用总览定位，不凭相似名认定）。
func (a *App) eventProject(sc eventScope, projectID string) (*Project, error) {
	projectID = strings.TrimSpace(projectID)
	if !sc.assistant {
		if projectID != "" && projectID != sc.own.ID {
			return nil, fmt.Errorf("普通会话只能访问自身实际工作区绑定的项目，不能按 project_id 跨项目读取")
		}
		own := *sc.own
		return &own, nil
	}
	if projectID == "" {
		return nil, fmt.Errorf("长期助理请先调 project_events_overview 定位项目，再指定 project_id；不要遍历所有项目猜测用户所指")
	}
	p, err := a.Store.GetProject(projectID)
	if errors.Is(err, ErrProjectNotFound) {
		return nil, fmt.Errorf("项目 %s 不存在；请先用 project_events_overview 或 设置 → 项目事件 核对", projectID)
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// eventByID 读事件并按项目视野校验归属（event_id / record_id 不能成为越权入口）。
func (a *App) eventByID(sc eventScope, eventID string) (ProjectEvent, Project, error) {
	ev, err := a.Store.GetProjectEvent(strings.TrimSpace(eventID))
	if err != nil {
		return ProjectEvent{}, Project{}, err
	}
	p, err := a.Store.GetProject(ev.ProjectID)
	if errors.Is(err, ErrProjectNotFound) {
		return ProjectEvent{}, Project{}, fmt.Errorf("事件所属项目已不存在，请重新查询")
	}
	if err != nil {
		return ProjectEvent{}, Project{}, err
	}
	if !sc.assistant && sc.own != nil && p.ID != sc.own.ID {
		return ProjectEvent{}, Project{}, fmt.Errorf("该事件不属于本会话绑定的项目，不能按 event_id 跨项目读取")
	}
	return ev, p, nil
}

// eventWriteAuthorize 生成本次写入的事务内授权：项目一致性 + 权威会话的
// 模式/权限/计划模式/工作区归属全部按 tx 重查。只能使用传入的 reader。
func (a *App) eventWriteAuthorize(t *toolContext, target *Project) eventWriteAuth {
	return func(q projectReader, p Project, sess *SessionMeta) error {
		if p.ID != target.ID {
			return fmt.Errorf("项目归属在提交时发生变化，拒绝写入；请重读项目后重试")
		}
		auth := *t
		auth.sess = sess
		if err := projectEventSessionContext(&auth, true); err != nil {
			return err
		}
		if sess.ID != t.sess.ID {
			return fmt.Errorf("调用会话与提交会话不一致，拒绝写入")
		}
		if t.sess.Mode != "assistant" {
			own, err := a.resolveSessionProjectIn(q, sess)
			if err != nil {
				return err
			}
			if own == nil || own.ID != p.ID {
				return fmt.Errorf("会话工作区归属已变化，无法确认该项目仍属于本会话；请重读项目状态后再写入")
			}
		}
		return nil
	}
}

// ---------- 来源资格 ----------

type eventSourceArg struct {
	SessionID  string `json:"session_id"`
	MessageSeq int64  `json:"message_seq"`
}

// eventSourceInputs 校验来源：会话必须存在；普通会话只能绑自身或同项目的会话；
// 长期助理可额外绑 assistant 会话或有可核验派发/回报关系的会话。
func (a *App) eventSourceInputs(t *toolContext, target *Project, args []eventSourceArg, callerSession string) ([]ProjectEventSourceInput, error) {
	out := make([]ProjectEventSourceInput, 0, len(args))
	seen := map[string]bool{}
	for _, raw := range args {
		sid := strings.TrimSpace(raw.SessionID)
		if sid == "" {
			return nil, fmt.Errorf("来源必须给出 session_id；可以绑定当前会话或实际执行/评审的会话")
		}
		seq := raw.MessageSeq
		if seq < 0 {
			return nil, fmt.Errorf("message_seq 必须是正整数或省略")
		}
		key := sid + "#" + strconv.FormatInt(seq, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		sess, err := a.Store.Get(sid)
		if err != nil || sess == nil {
			return nil, fmt.Errorf("来源会话 %s 不存在；只能绑定实际存在的会话，不能编造来源", sid)
		}
		if !sameEventProjectSource(a, t, target, sess, callerSession) {
			return nil, fmt.Errorf("来源会话 %s 不满足项目与调用者资格：普通会话只能绑自身或同项目的会话，跨项目协调请由长期助理绑定", sid)
		}
		if seq > 0 {
			if _, _, err := a.Store.MessageFingerprint(sid, seq); err != nil {
				return nil, fmt.Errorf("来源会话 %s 没有 seq=%d 的消息；不能把未核实的位置当作已核实原文", sid, seq)
			}
		}
		out = append(out, ProjectEventSourceInput{SessionID: sid, MessageSeq: seq})
	}
	return out, nil
}

// sameEventProjectSource 判定某个会话能否作为该项目事件的来源。
func sameEventProjectSource(a *App, t *toolContext, target *Project, sess *SessionMeta, callerSession string) bool {
	if sess.ID == callerSession {
		return true
	}
	if own, err := a.resolveSessionProject(sess); err == nil && own != nil && own.ID == target.ID {
		return true
	}
	if t.sess.Mode != "assistant" {
		return false
	}
	// 长期助理的跨项目协调：assistant 会话本身，或由 assistant / 同项目会话派出、
	// 作为其子对话的会话（有可核验的派发或回报关系）。
	if sess.Mode == "assistant" {
		return true
	}
	for _, link := range []string{sess.DispatchedBy, sess.ParentID} {
		if link == "" {
			continue
		}
		if link == t.sess.ID {
			return true
		}
		if owner, err := a.Store.Get(link); err == nil && owner != nil {
			if owner.Mode == "assistant" {
				return true
			}
			if own, err := a.resolveSessionProject(owner); err == nil && own != nil && own.ID == target.ID {
				return true
			}
		}
	}
	return false
}

// ---------- 参数与分页 ----------

func eventPageLimit(v *int, def int) (int, error) {
	if v == nil {
		return def, nil
	}
	if *v < 1 || *v > projectEventPageMax {
		return 0, fmt.Errorf("limit 必须在 1—%d 之间；不静默修正参数，请显式给合法条数", projectEventPageMax)
	}
	return *v, nil
}

func eventStatusFilter(raw string) (string, error) {
	s, err := normalizeEventStatus(raw)
	if err != nil {
		return "", err
	}
	return s, nil
}

func eventScopeFilter(raw string) (string, error) {
	switch strings.TrimSpace(raw) {
	case "", projectEventScopeActive:
		return projectEventScopeActive, nil
	case projectEventScopeHistory:
		return projectEventScopeHistory, nil
	case projectEventScopeAll:
		return projectEventScopeAll, nil
	}
	return "", fmt.Errorf("scope 只能是 active（未结束）/ history（已完成与取消）/ all（全部）")
}

func eventTimeArg(raw, label string) (string, error) {
	v, err := normalizeEventTime(raw)
	if err != nil {
		return "", fmt.Errorf("%s：%s", label, err.Error())
	}
	return v, nil
}

// ---------- 读能力 ----------

type eventOverviewArgs struct {
	ProjectKeyword  string `json:"project_keyword"`
	EventKeyword    string `json:"event_keyword"`
	Scope           string `json:"scope"`
	Status          string `json:"status"`
	IncludeArchived bool   `json:"include_archived"`
	ProjectLimit    *int   `json:"project_limit"`
	ProjectCursor   string `json:"project_cursor"`
}

func runProjectEventsOverview(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, false); err != nil {
		return "", err
	}
	var args eventOverviewArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	out, err := t.app.eventOverview(sc, args, 5)
	if err != nil {
		return "", err
	}
	return projectJSON(out)
}

// eventOverview 组合跨项目总览：项目分页独立于事件分页；每组只给概况预览，
// 事件多也不挤掉别的项目（项目来自全部符合范围的登记项目，不是从最新事件反推）。
func (a *App) eventOverview(sc eventScope, args eventOverviewArgs, defLimit int) (map[string]any, error) {
	scope, err := eventScopeFilter(args.Scope)
	if err != nil {
		return nil, err
	}
	status, err := eventStatusFilter(args.Status)
	if err != nil {
		return nil, err
	}
	limit, err := eventPageLimit(args.ProjectLimit, defLimit)
	if err != nil {
		return nil, fmt.Errorf("project_limit：%s", err.Error())
	}
	filter := ProjectEventFilter{Scope: scope, Status: status, Keyword: strings.TrimSpace(args.EventKeyword)}

	var projects []Project
	if sc.assistant {
		projects, err = a.Store.ListProjects(true)
		if err != nil {
			return nil, err
		}
	} else {
		projects = []Project{*sc.own}
	}

	pkw := strings.ToLower(strings.TrimSpace(args.ProjectKeyword))
	filtered := make([]Project, 0, len(projects))
	for _, p := range projects {
		if p.Archived && !args.IncludeArchived && !(!sc.assistant && p.ID == sc.own.ID) {
			continue
		}
		if pkw != "" {
			search := strings.ToLower(p.ID + " " + p.Name)
			for _, w := range p.Workspaces {
				search += " " + strings.ToLower(w.DisplayPath)
			}
			if !strings.Contains(search, pkw) {
				continue
			}
		}
		filtered = append(filtered, p)
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Name != filtered[j].Name {
			return filtered[i].Name < filtered[j].Name
		}
		return filtered[i].ID < filtered[j].ID
	})

	sig := eventFilterSignature("prj", scope, status, pkw, filter.Keyword, strconv.FormatBool(args.IncludeArchived))
	cursor, err := decodeEventCursor(args.ProjectCursor, "prj", sig)
	if err != nil {
		return nil, err
	}
	start := 0
	if cursor != nil {
		for i, p := range filtered {
			if p.Name > cursor.Time || (p.Name == cursor.Time && p.ID > cursor.ID) {
				start = i
				break
			}
			start = i + 1
		}
	}
	end := start + limit
	if end > len(filtered) {
		end = len(filtered)
	}
	page := filtered[start:end]

	ids := make([]string, 0, len(page))
	byID := make(map[string]Project, len(page))
	for _, p := range page {
		ids = append(ids, p.ID)
		byID[p.ID] = p
	}
	groups := map[string]ProjectEventGroup{}
	if len(ids) > 0 {
		groups, err = a.Store.ProjectEventGroups(ids, filter, projectEventPreviewLimit)
		if err != nil {
			return nil, err
		}
	}
	outGroups := make([]map[string]any, 0, len(page))
	for _, p := range page {
		g := groups[p.ID]
		if g.Events == nil {
			g.Events = []ProjectEvent{}
		}
		remaining := g.Counts.Matched - len(g.Events)
		if remaining < 0 {
			remaining = 0
		}
		outGroups = append(outGroups, map[string]any{
			"project_id":          p.ID,
			"name":                p.Name,
			"archived":            p.Archived,
			"read_only":           p.Archived,
			"workspaces":          p.Workspaces,
			"counts":              g.Counts,
			"events":              g.Events,
			"events_shown":        len(g.Events),
			"events_remaining":    remaining,
			"events_has_more":     remaining > 0,
			"events_preview_size": projectEventPreviewLimit,
		})
	}
	remaining := len(filtered) - end
	hasMore := remaining > 0
	next := ""
	if hasMore && len(page) > 0 {
		last := page[len(page)-1]
		next = encodeEventCursor(eventCursor{Kind: "prj", Sig: sig, Time: last.Name, ID: last.ID})
	}
	note := "首页不是全量：每组最多 " + strconv.Itoa(projectEventPreviewLimit) + " 个目标；更多事件请用 project_events_query 按项目翻页，不要把首页当全部"
	if len(filtered) == 0 {
		// 无结果分三种，不能笼统一句「没有事件」：没登记 / 当前工作区未绑定 / 筛选没匹配。
		switch {
		case !sc.assistant && sc.own == nil:
			note = "当前会话的工作区尚未绑定任何已登记项目：请到 设置 → 项目事件（或项目知识库）新建项目并绑定工作区；不要擅自登记、写进其他项目或创建收件箱"
		case len(projects) == 0:
			note = "尚无已登记项目：请到 设置 → 项目事件 新建项目并绑定工作区（本页即可完成，不依赖项目知识库组件）"
		default:
			note = "筛选无匹配项目：已登记 " + strconv.Itoa(len(projects)) + " 个，但当前项目关键词/范围/状态都没命中；放宽筛选后再查，勿据此认为项目不存在"
		}
	}
	return map[string]any{
		"scope":               scope,
		"status":              status,
		"event_keyword":       filter.Keyword,
		"project_keyword":     strings.TrimSpace(args.ProjectKeyword),
		"projects":            outGroups,
		"project_total":       len(filtered),
		"project_shown":       len(page),
		"project_remaining":   remaining,
		"has_more":            hasMore,
		"next_project_cursor": next,
		"note":                note,
	}, nil
}

type eventListArgs struct {
	ProjectID   string `json:"project_id"`
	Scope       string `json:"scope"`
	Status      string `json:"status"`
	Keyword     string `json:"keyword"`
	SessionID   string `json:"session_id"`
	UpdatedFrom string `json:"updated_from"`
	UpdatedTo   string `json:"updated_to"`
	Limit       *int   `json:"limit"`
	Cursor      string `json:"cursor"`
}

func runProjectEventsQuery(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, false); err != nil {
		return "", err
	}
	var args eventListArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	out, err := t.app.eventList(sc, args, 6)
	if err != nil {
		return "", err
	}
	return projectJSON(out)
}

func (a *App) eventList(sc eventScope, args eventListArgs, defLimit int) (map[string]any, error) {
	p, err := a.eventProject(sc, args.ProjectID)
	if err != nil {
		return nil, err
	}
	scope, err := eventScopeFilter(args.Scope)
	if err != nil {
		return nil, err
	}
	status, err := eventStatusFilter(args.Status)
	if err != nil {
		return nil, err
	}
	from, err := eventTimeArg(args.UpdatedFrom, "updated_from")
	if err != nil {
		return nil, err
	}
	to, err := eventTimeArg(args.UpdatedTo, "updated_to")
	if err != nil {
		return nil, err
	}
	if from != "" && to != "" && from > to {
		return nil, fmt.Errorf("时间范围无效：updated_from 必须早于 updated_to（半开区间 from <= t < to）")
	}
	limit, err := eventPageLimit(args.Limit, defLimit)
	if err != nil {
		return nil, err
	}
	filter := ProjectEventFilter{Scope: scope, Status: status, Keyword: strings.TrimSpace(args.Keyword), SessionID: strings.TrimSpace(args.SessionID), UpdatedFrom: from, UpdatedTo: to}
	sig := eventFilterSignature("ev", p.ID, scope, status, filter.Keyword, filter.SessionID, from, to)
	cursor, err := decodeEventCursor(args.Cursor, "ev", sig)
	if err != nil {
		return nil, err
	}
	items, total, remaining, err := a.Store.ProjectEventPage(p.ID, filter, cursor, limit)
	if err != nil {
		return nil, err
	}
	next := ""
	if remaining > 0 && len(items) > 0 {
		last := items[len(items)-1]
		next = encodeEventCursor(eventCursor{Kind: "ev", Sig: sig, Time: last.UpdatedAt, ID: last.ID})
	}
	return map[string]any{
		"project":     eventProjectView(*p),
		"scope":       scope,
		"status":      status,
		"events":      items,
		"total":       total,
		"shown":       len(items),
		"remaining":   remaining,
		"has_more":    remaining > 0,
		"next_cursor": next,
		"filter_note": "按 updated_at 筛选的是「当前最后修改时间在范围内的目标」，不是「这期间发生过的全部工作」",
	}, nil
}

type eventGetArgs struct {
	EventID string `json:"event_id"`
}

func runProjectEventGet(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, false); err != nil {
		return "", err
	}
	var args eventGetArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.EventID) == "" {
		return "", fmt.Errorf("需要 event_id；先用 project_events_overview / project_events_query 定位")
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	out, err := t.app.eventDetail(sc, args.EventID)
	if err != nil {
		return "", err
	}
	return projectJSON(out)
}

func (a *App) eventDetail(sc eventScope, eventID string) (map[string]any, error) {
	ev, p, err := a.eventByID(sc, eventID)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"event":   ev,
		"project": eventProjectView(p),
		"note":    "这里只给当前概况；时间线请用 project_event_records_query 分页读取，来源用 project_event_sources_query",
	}, nil
}

type eventRecordsArgs struct {
	EventID      string `json:"event_id"`
	Kind         string `json:"kind"`
	Keyword      string `json:"keyword"`
	OccurredFrom string `json:"occurred_from"`
	OccurredTo   string `json:"occurred_to"`
	Direction    string `json:"direction"`
	Limit        *int   `json:"limit"`
	Cursor       string `json:"cursor"`
}

func runProjectEventRecordsQuery(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, false); err != nil {
		return "", err
	}
	var args eventRecordsArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	out, err := t.app.eventRecords(sc, args, 5)
	if err != nil {
		return "", err
	}
	return projectJSON(out)
}

func (a *App) eventRecords(sc eventScope, args eventRecordsArgs, defLimit int) (map[string]any, error) {
	ev, p, err := a.eventByID(sc, args.EventID)
	if err != nil {
		return nil, err
	}
	kind := ""
	if strings.TrimSpace(args.Kind) != "" {
		if kind, err = normalizeEventKind(args.Kind); err != nil {
			return nil, err
		}
	}
	from, err := eventTimeArg(args.OccurredFrom, "occurred_from")
	if err != nil {
		return nil, err
	}
	to, err := eventTimeArg(args.OccurredTo, "occurred_to")
	if err != nil {
		return nil, err
	}
	if from != "" && to != "" && from > to {
		return nil, fmt.Errorf("时间范围无效：occurred_from 必须早于 occurred_to（半开区间 from <= t < to）")
	}
	limit, err := eventPageLimit(args.Limit, defLimit)
	if err != nil {
		return nil, err
	}
	direction := strings.TrimSpace(args.Direction)
	switch direction {
	case "", "latest", "older":
		direction = "older"
	case "newer":
	default:
		return nil, fmt.Errorf("direction 只能是 older（更早）或 newer（向后）")
	}
	filter := ProjectEventRecordFilter{Kind: kind, Keyword: strings.TrimSpace(args.Keyword), OccurredFrom: from, OccurredTo: to}
	sig := eventFilterSignature("rec", ev.ID, kind, filter.Keyword, from, to)
	cursor, err := decodeEventCursor(args.Cursor, "rec", sig)
	if err != nil {
		return nil, err
	}
	res, err := a.Store.ProjectEventRecordPage(ev.ID, filter, direction, cursor, limit)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(res.Items))
	for _, r := range res.Items {
		ids = append(ids, r.ID)
	}
	srcMap, err := a.Store.ProjectEventRecordSources(ids, projectEventPreviewLimit)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(res.Items))
	for _, r := range res.Items {
		views, err := a.eventSourceViews(sc, ev.ProjectID, srcMap[r.ID], "")
		if err != nil {
			return nil, err
		}
		items = append(items, map[string]any{
			"record":        r,
			"time_note":     eventTimeNote(r),
			"sources":       views,
			"sources_shown": len(views),
			"sources_total": r.SourceCount,
		})
	}
	next := ""
	if res.OlderRemaining > 0 && len(res.Items) > 0 {
		first := res.Items[0]
		key := eventRecordSortKey(first)
		next = encodeEventCursor(eventCursor{Kind: "rec", Sig: sig, Time: key, ID: first.ID})
	}
	newer := ""
	if len(res.Items) > 0 && (cursor != nil || direction == "newer") {
		last := res.Items[len(res.Items)-1]
		newer = encodeEventCursor(eventCursor{Kind: "rec", Sig: sig, Time: eventRecordSortKey(last), ID: last.ID})
	}
	return map[string]any{
		"event":           ev,
		"project":         eventProjectView(p),
		"records":         items,
		"total":           res.Total,
		"shown":           len(items),
		"remaining_older": res.OlderRemaining,
		"has_more":        res.OlderRemaining > 0,
		"next_cursor":     next,
		"newer_cursor":    newer,
		"unknown_time":    res.UnknownTime,
		"time_note":       "页内按事实发生时间正序；事实时间未明确的记录以记录时间排序，并标注不是事实时间",
	}, nil
}

func eventRecordSortKey(r ProjectEventRecord) string {
	if r.OccurredAt != "" {
		return r.OccurredAt
	}
	return r.CreatedAt
}

func eventTimeNote(r ProjectEventRecord) string {
	if r.OccurredAt == "" {
		return "记录时间（事实时间未明确）"
	}
	return ""
}

type eventSourcesArgs struct {
	RecordID string `json:"record_id"`
	SourceID string `json:"source_id"`
	Limit    *int   `json:"limit"`
	Cursor   string `json:"cursor"`
}

func runProjectEventSourcesQuery(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, false); err != nil {
		return "", err
	}
	var args eventSourcesArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	out, err := t.app.eventSources(sc, args, 5)
	if err != nil {
		return "", err
	}
	return projectJSON(out)
}

func (a *App) eventSources(sc eventScope, args eventSourcesArgs, defLimit int) (map[string]any, error) {
	rec, err := a.Store.GetProjectEventRecord(strings.TrimSpace(args.RecordID))
	if err != nil {
		return nil, err
	}
	ev, p, err := a.eventByID(sc, rec.EventID)
	if err != nil {
		return nil, err
	}
	limit, err := eventPageLimit(args.Limit, defLimit)
	if err != nil {
		return nil, err
	}
	sig := eventFilterSignature("src", rec.ID)
	cursor, err := decodeEventCursor(args.Cursor, "src", sig)
	if err != nil {
		return nil, err
	}
	sources, total, remaining, err := a.Store.ProjectEventSourcePage(rec.ID, cursor, limit)
	if err != nil {
		return nil, err
	}
	views, err := a.eventSourceViews(sc, ev.ProjectID, sources, strings.TrimSpace(args.SourceID))
	if err != nil {
		return nil, err
	}
	next := ""
	if remaining > 0 && len(sources) > 0 {
		last := sources[len(sources)-1]
		next = encodeEventCursor(eventCursor{Kind: "src", Sig: sig, Time: last.LinkedAt, ID: last.ID})
	}
	return map[string]any{
		"record":      rec,
		"event":       ev,
		"project":     eventProjectView(p),
		"sources":     views,
		"total":       total,
		"shown":       len(views),
		"remaining":   remaining,
		"has_more":    remaining > 0,
		"next_cursor": next,
		"order_note":  "按绑定时间倒序只是关系排序，不代表事实新旧；请结合所问记录与消息时间判断",
		"source_note": "来源列表只给可用元数据判定的状态；正文指纹只在你指定 source_id 复核时定点比对一条，未复核不等于原文未变",
	}, nil
}

// eventSourceViews 组装来源视图：先给元数据状态（会话缺失/归属变化/消息被替换），
// 只有显式 recheckID 才做正文指纹复核。普通会话看不到跨项目来源的标题。
func (a *App) eventSourceViews(sc eventScope, projectID string, sources []ProjectEventSource, recheckID string) ([]map[string]any, error) {
	out := make([]map[string]any, 0, len(sources))
	for _, src := range sources {
		view := map[string]any{
			"source_id":   src.ID,
			"session_id":  src.SessionID,
			"message_seq": src.MessageSeq,
			"linked_at":   src.LinkedAt,
			"origin":      src.Origin,
		}
		sess, err := a.Store.Get(src.SessionID)
		switch {
		case err != nil || sess == nil:
			view["state"] = "session_missing"
			view["note"] = "来源会话已不存在：事实保留，不编造替代来源"
		default:
			accessible := true
			if !sc.assistant {
				if own, err := a.resolveSessionProject(sess); err == nil {
					accessible = own != nil && own.ID == projectID
				} else {
					accessible = false
				}
			}
			if !accessible {
				view["state"] = "session_restricted"
				view["note"] = "来源会话归属已变化：只保留关系标识，正文请由长期助理核实"
			} else {
				view["session_title"] = sess.Title
				view["session_mode"] = sess.Mode
				state := "available"
				if src.MessageID > 0 {
					_, _, found, err := a.Store.MessageRowByID(src.MessageID)
					if err != nil {
						return nil, err
					}
					if !found {
						owner, ok, err := a.Store.MessageSeqOwner(src.SessionID, src.MessageSeq)
						if err != nil {
							return nil, err
						}
						if ok && owner != src.MessageID {
							state = "message_replaced"
						} else {
							state = "message_missing"
						}
					} else if src.ID == recheckID {
						_, digest, _, err := a.Store.MessageRowByID(src.MessageID)
						if err != nil {
							return nil, err
						}
						if digest == src.MessageDigest {
							state = "verified_unchanged"
						} else {
							state = "message_changed"
						}
					} else {
						state = "unreviewed"
					}
				}
				view["state"] = state
				switch state {
				case "message_replaced":
					view["note"] = "该位置已被新消息占用（编辑重发）：原依据不再有效，不能跳到新内容伪称同一依据"
				case "message_missing":
					view["note"] = "原消息已移除：事实保留，正文请重新核实"
				case "message_changed":
					view["note"] = "来源已修改，需复核：正文与原依据不同"
				case "verified_unchanged":
					view["note"] = "已复核：消息行与正文摘要一致"
				case "unreviewed":
					view["note"] = "正文指纹未复核；需要时用 source_id 定点复核一条"
				}
			}
		}
		out = append(out, view)
	}
	return out, nil
}

func eventProjectView(p Project) map[string]any {
	return map[string]any{
		"project_id": p.ID,
		"name":       p.Name,
		"archived":   p.Archived,
		"read_only":  p.Archived,
	}
}

// ---------- 写能力 ----------

type eventRecordArgs struct {
	Kind       string           `json:"kind"`
	Text       string           `json:"text"`
	OccurredAt string           `json:"occurred_at"`
	ClientKey  string           `json:"client_key"`
	Sources    []eventSourceArg `json:"sources"`
}

// prepareRecord 校验并规范化一条关键事实（含来源资格），返回可落库的输入与摘要。
func (a *App) prepareRecord(t *toolContext, target *Project, args eventRecordArgs, origin string) (ProjectEventRecordInput, error) {
	kind, err := normalizeEventKind(args.Kind)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	text, err := normalizeEventText("text", "推进记录", args.Text, projectEventTextBudget)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	occurred, err := normalizeEventTime(args.OccurredAt)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	var sources []ProjectEventSourceInput
	if origin == eventOriginAI {
		sources, err = a.eventSourceInputs(t, target, args.Sources, t.sess.ID)
		if err != nil {
			return ProjectEventRecordInput{}, err
		}
	} else {
		sources, err = a.userSourceInputs(args.Sources)
		if err != nil {
			return ProjectEventRecordInput{}, err
		}
	}
	return ProjectEventRecordInput{
		Kind:          kind,
		Text:          text,
		OccurredAt:    occurred,
		ClientKey:     strings.TrimSpace(args.ClientKey),
		RequestDigest: eventDigest(kind, text, occurred, sources),
		Sources:       sources,
	}, nil
}

func eventDigest(parts ...any) string {
	b, err := json.Marshal(parts)
	if err != nil {
		return ""
	}
	return messageDigest("event-request", string(b))
}

// prepareRecordAsUser 是设置页用户录入的同一套字段校验；来源只需真实存在（用户身份
// 已由路由层卡住），不按 AI 的项目资格过滤。
func (a *App) prepareRecordAsUser(args eventRecordArgs) (ProjectEventRecordInput, error) {
	kind, err := normalizeEventKind(args.Kind)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	text, err := normalizeEventText("text", "推进记录", args.Text, projectEventTextBudget)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	occurred, err := normalizeEventTime(args.OccurredAt)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	sources, err := a.userSourceInputs(args.Sources)
	if err != nil {
		return ProjectEventRecordInput{}, err
	}
	return ProjectEventRecordInput{
		Kind:          kind,
		Text:          text,
		OccurredAt:    occurred,
		ClientKey:     strings.TrimSpace(args.ClientKey),
		RequestDigest: eventDigest(kind, text, occurred, sources),
		Sources:       sources,
	}, nil
}

// userSourceInputs 只校验来源真实存在；消息位置必须真有消息，不拿 seq 当占位。
func (a *App) userSourceInputs(args []eventSourceArg) ([]ProjectEventSourceInput, error) {
	out := make([]ProjectEventSourceInput, 0, len(args))
	seen := map[string]bool{}
	for _, raw := range args {
		sid := strings.TrimSpace(raw.SessionID)
		if sid == "" {
			return nil, fmt.Errorf("来源必须给出 session_id")
		}
		if raw.MessageSeq < 0 {
			return nil, fmt.Errorf("message_seq 必须是正整数或省略")
		}
		key := sid + "#" + strconv.FormatInt(raw.MessageSeq, 10)
		if seen[key] {
			continue
		}
		seen[key] = true
		if _, err := a.Store.Get(sid); err != nil {
			return nil, fmt.Errorf("来源会话 %s 不存在；只能绑定实际存在的会话", sid)
		}
		if raw.MessageSeq > 0 {
			if _, _, err := a.Store.MessageFingerprint(sid, raw.MessageSeq); err != nil {
				return nil, fmt.Errorf("来源会话 %s 没有 seq=%d 的消息", sid, raw.MessageSeq)
			}
		}
		out = append(out, ProjectEventSourceInput{SessionID: sid, MessageSeq: raw.MessageSeq})
	}
	return out, nil
}

type eventCreateArgs struct {
	ProjectID string           `json:"project_id"`
	Title     string           `json:"title"`
	Summary   string           `json:"summary"`
	Status    string           `json:"status"`
	CreateKey string           `json:"create_key"`
	Record    *eventRecordArgs `json:"record"`
}

func runProjectEventCreate(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, true); err != nil {
		return "", err
	}
	var args eventCreateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	p, err := t.app.eventProject(sc, args.ProjectID)
	if err != nil {
		return "", err
	}
	title, err := normalizeEventText("title", "标题", args.Title, projectEventTitleBudget)
	if err != nil {
		return "", err
	}
	summary, err := normalizeEventText("summary", "概况", args.Summary, projectEventSummaryBudget)
	if err != nil {
		return "", err
	}
	status, err := normalizeEventStatus(args.Status)
	if err != nil {
		return "", err
	}
	if status == "" {
		status = eventStatusDiscussing
	}
	in := ProjectEventCreateInput{
		Title:     title,
		Summary:   summary,
		Status:    status,
		CreateKey: strings.TrimSpace(args.CreateKey),
	}
	if args.Record != nil {
		rec, err := t.app.prepareRecord(t, p, *args.Record, eventOriginAI)
		if err != nil {
			return "", err
		}
		in.Record = &rec
	}
	if status != eventStatusDiscussing && in.Record == nil {
		return "", fmt.Errorf("%w：直接创建为「%s」必须同时给出 record（有依据的关键事实）；讨论中可只登记概况", ErrProjectEventRecordNeedsSource, projectEventStatusLabels[status])
	}
	in.CreateDigest = eventDigest(title, summary, status, in.Record)
	ev, created, err := t.app.Store.CreateProjectEventForSession(p.ID, in, t.sess.ID, t.app.eventWriteAuthorize(t, p))
	if err != nil {
		return "", err
	}
	return projectJSON(map[string]any{"created": created, "event": ev, "project": eventProjectView(*p)})
}

type eventUpdateArgs struct {
	EventID          string           `json:"event_id"`
	ExpectedRevision *int64           `json:"expected_revision"`
	Title            *string          `json:"title"`
	Summary          *string          `json:"summary"`
	Status           *string          `json:"status"`
	Record           *eventRecordArgs `json:"record"`
}

func runProjectEventUpdate(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, true); err != nil {
		return "", err
	}
	var args eventUpdateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.EventID) == "" || args.ExpectedRevision == nil {
		return "", fmt.Errorf("需要 event_id 与 expected_revision；先读取当前概况，冲突时重读合并")
	}
	if args.Title == nil && args.Summary == nil && args.Status == nil && args.Record == nil {
		return "", fmt.Errorf("没有要改的字段：请给出 title / summary / status / record 之一")
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	ev, p, err := t.app.eventByID(sc, args.EventID)
	if err != nil {
		return "", err
	}
	patch := ProjectEventPatchInput{}
	if args.Title != nil {
		v, err := normalizeEventText("title", "标题", *args.Title, projectEventTitleBudget)
		if err != nil {
			return "", err
		}
		patch.Title = &v
	}
	if args.Summary != nil {
		v, err := normalizeEventText("summary", "概况", *args.Summary, projectEventSummaryBudget)
		if err != nil {
			return "", err
		}
		patch.Summary = &v
	}
	statusChanged := false
	if args.Status != nil {
		v, err := normalizeEventStatus(*args.Status)
		if err != nil {
			return "", err
		}
		if v == "" {
			return "", fmt.Errorf("status 不能为空；要清空状态请显式给六个状态之一")
		}
		statusChanged = v != ev.Status
		patch.Status = &v
	}
	var rec *ProjectEventRecordInput
	if args.Record != nil {
		prepared, err := t.app.prepareRecord(t, &p, *args.Record, eventOriginAI)
		if err != nil {
			return "", err
		}
		rec = &prepared
	}
	// 只有实际状态变化才要求附事实；仅改标题/概况（哪怕当前状态非讨论中）不强制。
	if statusChanged && rec == nil {
		return "", fmt.Errorf("%w：状态从「%s」变到「%s」必须同时提交一条有依据的关键事实", ErrProjectEventRecordNeedsSource, projectEventStatusLabels[ev.Status], projectEventStatusLabels[*patch.Status])
	}
	updated, changed, err := t.app.Store.MutateProjectEventForSession(ev.ID, *args.ExpectedRevision, patch, rec, t.sess.ID, t.app.eventWriteAuthorize(t, &p))
	if err != nil {
		return "", err
	}
	return projectJSON(map[string]any{"event": updated, "changed": changed, "project": eventProjectView(p)})
}

type eventRecordAddArgs struct {
	EventID               string           `json:"event_id"`
	ExpectedEventRevision *int64           `json:"expected_event_revision"`
	Kind                  string           `json:"kind"`
	Text                  string           `json:"text"`
	OccurredAt            string           `json:"occurred_at"`
	ClientKey             string           `json:"client_key"`
	Sources               []eventSourceArg `json:"sources"`
	Event                 *eventUpdateArgs `json:"event"`
}

func runProjectEventRecordAdd(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, true); err != nil {
		return "", err
	}
	var args eventRecordAddArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.EventID) == "" || args.ExpectedEventRevision == nil {
		return "", fmt.Errorf("需要 event_id 与 expected_event_revision；先读取当前概况")
	}
	if strings.TrimSpace(args.ClientKey) == "" {
		return "", fmt.Errorf("需要 client_key（事件内唯一幂等键）：重试时复用同一个键，不会重复追加同一条事实")
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	ev, p, err := t.app.eventByID(sc, args.EventID)
	if err != nil {
		return "", err
	}
	rec, err := t.app.prepareRecord(t, &p, eventRecordArgs{Kind: args.Kind, Text: args.Text, OccurredAt: args.OccurredAt, ClientKey: args.ClientKey, Sources: args.Sources}, eventOriginAI)
	if err != nil {
		return "", err
	}
	patch := ProjectEventPatchInput{}
	if args.Event != nil {
		if args.Event.Title != nil {
			v, err := normalizeEventText("title", "标题", *args.Event.Title, projectEventTitleBudget)
			if err != nil {
				return "", err
			}
			patch.Title = &v
		}
		if args.Event.Summary != nil {
			v, err := normalizeEventText("summary", "概况", *args.Event.Summary, projectEventSummaryBudget)
			if err != nil {
				return "", err
			}
			patch.Summary = &v
		}
		if args.Event.Status != nil {
			v, err := normalizeEventStatus(*args.Event.Status)
			if err != nil {
				return "", err
			}
			if v == "" {
				return "", fmt.Errorf("status 不能为空")
			}
			patch.Status = &v
		}
	}
	updated, added, changed, err := t.app.Store.AddProjectEventRecordForSession(ev.ID, *args.ExpectedEventRevision, patch, rec, t.sess.ID, t.app.eventWriteAuthorize(t, &p))
	if err != nil {
		return "", err
	}
	return projectJSON(map[string]any{"event": updated, "record": added, "changed": changed, "project": eventProjectView(p)})
}

type eventRecordUpdateArgs struct {
	RecordID               string           `json:"record_id"`
	ExpectedRecordRevision *int64           `json:"expected_record_revision"`
	ExpectedEventRevision  *int64           `json:"expected_event_revision"`
	Text                   *string          `json:"text"`
	Kind                   *string          `json:"kind"`
	OccurredAt             *string          `json:"occurred_at"`
	SourcesAdd             []eventSourceArg `json:"sources_add"`
	SourcesRemove          []string         `json:"sources_remove"`
}

func runProjectEventRecordUpdate(t *toolContext, raw json.RawMessage) (string, error) {
	if err := projectEventCapabilityContext(t, true); err != nil {
		return "", err
	}
	var args eventRecordUpdateArgs
	if err := json.Unmarshal(raw, &args); err != nil {
		return "", err
	}
	if strings.TrimSpace(args.RecordID) == "" || args.ExpectedRecordRevision == nil || args.ExpectedEventRevision == nil {
		return "", fmt.Errorf("需要 record_id、expected_record_revision 与 expected_event_revision；先重读该记录与事件概况")
	}
	if args.Text == nil && args.Kind == nil && args.OccurredAt == nil && len(args.SourcesAdd) == 0 && len(args.SourcesRemove) == 0 {
		return "", fmt.Errorf("没有要改的内容：请给出 text / kind / occurred_at / sources_add / sources_remove 之一")
	}
	sc, err := t.app.eventScopeFor(t)
	if err != nil {
		return "", err
	}
	rec, err := t.app.Store.GetProjectEventRecord(strings.TrimSpace(args.RecordID))
	if err != nil {
		return "", err
	}
	_, p, err := t.app.eventByID(sc, rec.EventID)
	if err != nil {
		return "", err
	}
	patch := ProjectEventRecordPatchInput{}
	if args.Text != nil {
		v, err := normalizeEventText("text", "推进记录", *args.Text, projectEventTextBudget)
		if err != nil {
			return "", err
		}
		patch.Text = &v
	}
	if args.Kind != nil {
		v, err := normalizeEventKind(*args.Kind)
		if err != nil {
			return "", err
		}
		patch.Kind = &v
	}
	if args.OccurredAt != nil {
		v, err := normalizeEventTime(*args.OccurredAt)
		if err != nil {
			return "", err
		}
		patch.OccurredAt = &v
	}
	if len(args.SourcesAdd) > 0 {
		add, err := t.app.eventSourceInputs(t, &p, args.SourcesAdd, t.sess.ID)
		if err != nil {
			return "", err
		}
		patch.SourcesAdd = add
	}
	patch.SourcesRemove = args.SourcesRemove
	ev, updated, changed, err := t.app.Store.PatchProjectEventRecordForSession(rec.ID, *args.ExpectedRecordRevision, *args.ExpectedEventRevision, patch, t.sess.ID, t.app.eventWriteAuthorize(t, &p))
	if err != nil {
		return "", err
	}
	return projectJSON(map[string]any{"event": ev, "record": updated, "changed": changed, "project": eventProjectView(p)})
}
