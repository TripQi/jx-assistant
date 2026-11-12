package main

// usagestats.go —— 「用量统计」组件的数据层。
//
// 背景：sessions 表只有会话级累计（usage_prompt/usage_cache_hit/usage_output/
// usage_calls），看不出「哪天、哪个模型、哪个渠道、哪一次调用花了多少」，也就
// 算不出按天趋势、模型占比与估算花费。这里新增一张逐次调用明细表
// usage_events，每次模型调用（成功与失败都算）落一条。
//
// 设计意图（为什么这么做）：
//   - 采集走「统一出口 + 异步落库」：埋点挂在 StreamChat 的调用处（agent.go 的
//     主循环、摘要、/api/llm/ask、git 摘要、CC mods），流式输出每一帧都在热路径
//     上，写库必须异步批量做，绝不阻塞吐字。
//   - 表只存「事实」（token 数、耗时、渠道、Key、模型、协议、成败），价格不进表：
//     单价与渠道倍率是用户随时可改的配置（usagepricing.go），聚合时再算钱——
//     改了单价，历史记录的花费跟着变，不用回填全表。
//   - 保留策略在启动与每天定时各跑一次（超 90 天或超 20 万条删最旧），页面另有
//     「按日期范围清理」，数据不能无限长。
//   - 采集由组件开关控制：组件（id 见 usageComponentID）关掉就停记，已有数据保留
//     （重新打开还能看到历史），界面与输入框下方的速度条一起消失。

import (
	"log"
	"strings"
	"time"
)

const (
	// usageComponentID 是用量统计组件的 id：组件开关是采集的唯一门闩
	//（components.go 的 IsEnabled；前端页面/速度条也随同一个开关出现与消失）。
	usageComponentID = "usage-stats"

	usageRetentionDays = 90                     // 明细保留天数（超龄删最旧）
	usageMaxRows       = 200000                 // 明细条数上限（超量删最旧）
	usageWriteBatch    = 32                     // 落库批量：攒够就提交
	usageFlushEvery    = 300 * time.Millisecond // 或最多等这么久就提交
	usageErrorMaxRunes = 300                    // 错误摘要限长，明细表不做日志
	usageRecordPageMax = 500                    // 明细分页单页上限（防御超大 limit）
)

// 用途：一次模型调用是「谁」发起的。存英文键，界面按它显示中文。
const (
	usagePurposeMain    = "main"    // 主对话（数字助理主对话，mode=assistant）
	usagePurposeTask    = "task"    // 普通任务会话
	usagePurposeSub     = "sub"     // 子任务会话 / 子对话（parentId 或 dispatchedBy 非空）
	usagePurposeAux     = "aux"     // 辅助答疑对话（mode=aux）
	usagePurposeTitle   = "title"   // AI 自动会话标题（组件 session-title 经 /api/llm/ask）
	usagePurposeCompact = "compact" // 上下文压缩摘要（contextengine 的 CallSummarizer）
	usagePurposeAssist  = "assist"  // 其他辅助生成（git 提交摘要、插件 mods 等）
	usagePurposeSearch  = "search"  // 联网搜索/抽取的供应商调用（按次计费，见 usagesearch.go）
)

// 调用类型（usage_events.kind）：模型调用与搜索/抽取调用共用一张明细表，但计费
// 维度完全不同（token 按量 vs 按次），页面分开展示、各自合计——kind 是两者的分界。
// 模型调用落空串（这列加之前的老数据也是空串，按 llm 对待，不回填）。
const (
	usageKindLLM     = ""        // 模型调用（token 计费）
	usageKindSearch  = "search"  // 搜索供应商调用（按次计费）
	usageKindExtract = "extract" // 抽取供应商调用（按次计费）
)

// usageKindSearchOf 搜索/抽取类调用的 kind（其余一律 llm）。
func usageKindIsSearchCall(kind string) bool {
	return kind == usageKindSearch || kind == usageKindExtract
}

// usageRow 是一条逐次调用明细（表 usage_events 的一行）。
type usageRow struct {
	TS         string
	SessionID  string
	Purpose    string
	ProviderID string
	KeyID      string
	Model      string
	Protocol   string // chat | responses | anthropic（搜索/抽取调用为空）
	Input      int64  // 输入 tokens（含缓存读取部分，上游 prompt_tokens 口径）
	CacheRead  int64  // 缓存读取 tokens（命中）
	CacheWrite int64  // 缓存写入 tokens（Anthropic 的 cache_creation）
	Output     int64  // 输出 tokens
	Thinking   string // 本次调用实际使用的思考档位；空串＝没指定（跟随服务端默认）
	TTFTMs     int64  // 首字延迟（请求发出 → 首个正文/思考增量）
	DurationMs int64  // 总耗时
	OK         bool
	Error      string
	Stream     bool
	// ---- 搜索/抽取调用专用（llm 调用这四项为空）----
	Kind    string // 调用类型：""（llm）/ search / extract
	Detail  string // 目标摘要：搜索词或目标 URL（截断，便于排查，不存全文）
	ErrKind string // 失败类别（连不上/Key 无效/额度/限流/解析/上游/拒绝/配置）
	Rank    int    // 供应商顺位（1 起；>1 表示前面几家失败过、这次是回落来的）
}

// usagePurposeOf 按会话形态判定用途（主对话/子任务会话/辅助对话/任务会话）。
func usagePurposeOf(sess *SessionMeta) string {
	if sess == nil {
		return usagePurposeAssist
	}
	switch {
	case sess.Mode == "assistant":
		return usagePurposeMain
	case sess.Mode == "aux":
		return usagePurposeAux
	case sess.ParentID != "" || sess.DispatchedBy != "":
		return usagePurposeSub
	default:
		return usagePurposeTask
	}
}

// usageProtocolLabel 把 LLMTarget.Protocol 归一到界面上的「类型」：空串是
// OpenAI 兼容 chat/completions（项目里绝大多数渠道），另两种是原生协议。
func usageProtocolLabel(p string) string {
	switch strings.TrimSpace(strings.ToLower(p)) {
	case "":
		return "chat"
	case "chat", "responses", "anthropic":
		return strings.TrimSpace(strings.ToLower(p))
	default:
		return strings.TrimSpace(strings.ToLower(p))
	}
}

// ---------- 采集：埋点包装 + 异步队列 ----------

// usageEnabled 组件是否开启（全局开关；采集只看全局——会话级覆盖是界面视角的
// 事，不该让同一台机器上的记录时有时无）。
func (a *App) usageEnabled() bool {
	return a != nil && a.Components != nil && a.Components.IsEnabled(usageComponentID)
}

// startUsageWriter 启动落库协程（NewApp 调一次）。
func (a *App) startUsageWriter() {
	if a.usageCh != nil {
		return
	}
	a.usageCh = make(chan usageRow, 256)
	go func(ch chan usageRow) {
		batch := make([]usageRow, 0, usageWriteBatch)
		flush := func() {
			if len(batch) == 0 {
				return
			}
			rows := batch
			batch = make([]usageRow, 0, usageWriteBatch)
			if err := a.Store.AppendUsageRows(rows); err != nil {
				log.Printf("[用量] 明细落库失败（%d 条）: %v", len(rows), err)
			}
		}
		tick := time.NewTicker(usageFlushEvery)
		defer tick.Stop()
		for {
			select {
			case r := <-ch:
				batch = append(batch, r)
				if len(batch) >= usageWriteBatch {
					flush()
				}
			case <-tick.C:
				flush()
			}
		}
	}(a.usageCh)
}

// recordUsage 把一条明细放进异步队列。组件关闭时直接丢弃（不落库）。
// 队列满＝写库比模型调用还慢，丢这条并记日志——统计功能绝不能拖累对话。
func (a *App) recordUsage(r usageRow) {
	if !a.usageEnabled() {
		return
	}
	a.startUsageWriter() // 测试里可能没走 NewApp
	select {
	case a.usageCh <- r:
	default:
		log.Printf("[用量] 落库队列已满，丢弃一条明细（模型=%s）", r.Model)
	}
}

// usageCall 是一次模型调用的采集器：包住调用方原有的 StreamCallbacks，只额外
// 记录首字延迟、流尾 usage 与成败，流结束调一次 finish 落库。
type usageCall struct {
	app      *App
	purpose  string
	sessID   string
	target   LLMTarget
	thinking string // 本次调用实际落到请求上的档位（空串＝跟随服务端默认）
	start    time.Time
	ttft     time.Duration
	usage    *Usage
	done     bool
}

// meterUsage 埋点包装：调用方把自己的回调交进来，拿到包过一层的回调与收尾函数
// （流结束/失败时调一次，传 StreamChat 的返回错误）。purpose 见上面的用途常量。
// thinking 必须是**这次调用真正发给模型的档位**：主对话传请求上的 reasoning
// effort，不吃档位的辅助调用（标题/摘要/插件 mods）传空——记会话的档位会把
// 没生效的档位算到辅助调用头上，明细就不可信了。
func (a *App) meterUsage(purpose string, sess *SessionMeta, target LLMTarget, thinking string, cb StreamCallbacks) (StreamCallbacks, func(error)) {
	m := &usageCall{app: a, purpose: purpose, target: target, start: time.Now()}
	if lv, ok := normalizeThinkingLevel(thinking); ok {
		m.thinking = lv
	}
	if sess != nil {
		m.sessID = sess.ID
	}
	mark := func() {
		if m.ttft == 0 {
			m.ttft = time.Since(m.start)
		}
	}
	wrapped := StreamCallbacks{
		OnBody:     cb.OnBody,
		OnRequest:  cb.OnRequest,
		OnToolCall: cb.OnToolCall,
		OnText: func(s string) {
			mark()
			if cb.OnText != nil {
				cb.OnText(s)
			}
		},
		OnReasoning: func(s string) {
			mark()
			if cb.OnReasoning != nil {
				cb.OnReasoning(s)
			}
		},
		OnUsage: func(u Usage) {
			uu := u
			m.usage = &uu
			if cb.OnUsage != nil {
				cb.OnUsage(u)
			}
		},
	}
	return wrapped, func(err error) { m.finish(err) }
}

// finish 把这次调用整理成一行明细送进异步队列（只调一次）。
func (m *usageCall) finish(err error) {
	if m == nil || m.done {
		return
	}
	m.done = true
	row := usageRow{
		TS:         now(),
		SessionID:  m.sessID,
		Purpose:    m.purpose,
		ProviderID: m.target.ProviderID,
		KeyID:      m.target.KeyID,
		Model:      m.target.Model,
		Protocol:   usageProtocolLabel(m.target.Protocol),
		Thinking:   m.thinking,
		TTFTMs:     m.ttft.Milliseconds(),
		DurationMs: time.Since(m.start).Milliseconds(),
		OK:         err == nil,
		Stream:     true,
		Kind:       usageKindLLM,
	}
	if u := m.usage; u != nil {
		row.Input = int64(u.PromptTokens)
		row.CacheRead = int64(u.CacheHit())
		row.CacheWrite = int64(u.CacheCreationTokens)
		row.Output = int64(u.CompletionTokens)
	}
	if err != nil {
		row.Error = truncateRunes(strings.Join(strings.Fields(err.Error()), " "), usageErrorMaxRunes)
	}
	m.app.recordUsage(row)
}

// ---------- 存储：明细表读写 ----------

// AppendUsageRows 批量写入明细（落库协程专用；单条写入也走它）。
func (s *Store) AppendUsageRows(rows []usageRow) error {
	if len(rows) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	stmt, err := tx.Prepare(`INSERT INTO usage_events
		(ts, session_id, purpose, provider_id, key_id, model, protocol,
		 input_tokens, cache_read, cache_write, output_tokens, thinking, ttft_ms, duration_ms, ok, error, stream,
		 kind, detail, err_kind, provider_rank)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return err
	}
	defer stmt.Close()
	for _, r := range rows {
		if _, err := stmt.Exec(r.TS, r.SessionID, r.Purpose, r.ProviderID, r.KeyID, r.Model, r.Protocol,
			r.Input, r.CacheRead, r.CacheWrite, r.Output, r.Thinking, r.TTFTMs, r.DurationMs, boolInt(r.OK), r.Error, boolInt(r.Stream),
			r.Kind, r.Detail, r.ErrKind, r.Rank); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// usageFilter 是页面的筛选条件：日期范围（含首尾，'YYYY-MM-DD'，空=不限）+
// 渠道 + 该渠道下的一把 Key（多 Key 供应商的「渠道 · Key」下拉）+ 推理强度。
// 推理强度取值为六档之一，或 usageThinkingDefault（「默认」＝当时没指定档位，
// 老数据与辅助调用都在这一档）。
type usageFilter struct {
	From       string
	To         string
	ProviderID string
	KeyID      string
	Thinking   string
}

// usageThinkingDefault 是筛选里「默认（未指定档位）」的哨兵值：库里存的是空串，
// 直接用空串当筛选值会与「不筛选」撞车（空串表示全部）。
const usageThinkingDefault = "default"

// usageThinkingFilter 把筛选值归一："" 不筛、"default" 筛空串、六档原样。
func usageThinkingFilter(s string) string {
	lv := strings.ToLower(strings.TrimSpace(s))
	if lv == "" {
		return ""
	}
	if lv == usageThinkingDefault {
		return usageThinkingDefault
	}
	if ok := containsStr(thinkingLevels(), lv); ok {
		return lv
	}
	return ""
}

// baseConds 基础筛选条件（日期/渠道/Key/推理强度），**不含调用类型**：
// token 类聚合与搜索类聚合都从它出发，各自再叠自己的 kind 条件。
// 日期用 substr(ts,1,10) 比较——ts 是本地时间字符串，按天切开就是日期，不必让
// SQLite 解析时间（也吃得到 ts 上的索引）。
func (f usageFilter) baseConds() ([]string, []any) {
	var conds []string
	var args []any
	if d := strings.TrimSpace(f.From); d != "" {
		conds = append(conds, "substr(ts,1,10) >= ?")
		args = append(args, d)
	}
	if d := strings.TrimSpace(f.To); d != "" {
		conds = append(conds, "substr(ts,1,10) <= ?")
		args = append(args, d)
	}
	if p := strings.TrimSpace(f.ProviderID); p != "" {
		conds = append(conds, "provider_id = ?")
		args = append(args, p)
	}
	if k := strings.TrimSpace(f.KeyID); k != "" {
		conds = append(conds, "key_id = ?")
		args = append(args, k)
	}
	switch usageThinkingFilter(f.Thinking) {
	case "":
		// 不筛
	case usageThinkingDefault:
		conds = append(conds, "thinking = ''")
	default:
		conds = append(conds, "thinking = ?")
		args = append(args, usageThinkingFilter(f.Thinking))
	}
	return conds, args
}

// whereClause 拼 WHERE 与参数（无条件时返回空串——调用方必须挡住全表操作）。
func whereClause(conds []string, args []any) (string, []any) {
	if len(conds) == 0 {
		return "", nil
	}
	return " WHERE " + strings.Join(conds, " AND "), args
}

// where 全部明细的条件（清理用；不带调用类型条件）。
func (f usageFilter) where() (string, []any) {
	conds, args := f.baseConds()
	return whereClause(conds, args)
}

// whereTokens 只取模型调用：token 合计与模型占比不能被搜索/抽取明细污染
//（它们没有 token，只按次计费）。
func (f usageFilter) whereTokens() (string, []any) {
	conds, args := f.baseConds()
	conds = append(conds, "kind IN ('', 'llm')")
	return whereClause(conds, args)
}

// whereKind 只取某一类搜索/抽取调用。
func (f usageFilter) whereKind(kind string) (string, []any) {
	conds, args := f.baseConds()
	conds = append(conds, "kind = ?")
	return whereClause(conds, append(args, kind))
}

// usageDayGroupRow 是「每日用量趋势」的原始聚合：按 (小时, 模型, 协议, 渠道) 分组
// —— 天级费用必须逐渠道按各自倍率、逐时段按峰值/空闲价算完再合并，SQL 里算不了
//（价格、节假日表都是 Go 侧的配置）。
//
// 为什么精确到小时而不是天：官方的高峰/空闲时段边界都落在整点，按小时分桶与逐条
// 按 ts 判定完全等价；而费用必须按**调用发生的时段**算，不能按「现在」——否则
// 历史数据重算出来的钱是错的。
type usageDayGroupRow struct {
	HourTS     string `json:"hourTs"` // 'YYYY-MM-DD HH'（时段判定的最小粒度）
	Model      string `json:"model"`
	Protocol   string `json:"protocol"`
	ProviderID string `json:"providerId"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	Calls      int64  `json:"calls"`
	Errors     int64  `json:"errors"`
}

// usageModelRow 是「按模型消耗统计」的一行原始聚合：按 (小时, 模型, 协议, 渠道)
// 分组——渠道必须单独出来（费用按各自渠道倍率算），小时也必须单独出来（费用按
// 各自时段价算）。
type usageModelRow struct {
	HourTS     string `json:"hourTs"`
	Model      string `json:"model"`
	Protocol   string `json:"protocol"`
	ProviderID string `json:"providerId"`
	Input      int64  `json:"input"`
	Output     int64  `json:"output"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	Calls      int64  `json:"calls"`
	Errors     int64  `json:"errors"`
}

// usageDayOf 从小时桶取日期（'YYYY-MM-DD HH' → 'YYYY-MM-DD'）。
func usageDayOf(hourTS string) string {
	if len(hourTS) >= 10 {
		return hourTS[:10]
	}
	return hourTS
}

// UsageDailyGroups 按 (小时, 模型, 协议, 渠道) 聚合（升序）。调用方在 Go 侧合并成
// 天级数据，并逐条按时段价、逐渠道按倍率算钱。
func (s *Store) UsageDailyGroups(f usageFilter) ([]usageDayGroupRow, error) {
	where, args := f.whereTokens()
	q := `SELECT substr(ts,1,13) AS hour_ts, model, protocol, provider_id,
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
			COUNT(*), COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0)
		FROM usage_events` + where + ` GROUP BY hour_ts, model, protocol, provider_id ORDER BY hour_ts`
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageDayGroupRow{}
	for rows.Next() {
		var r usageDayGroupRow
		if err := rows.Scan(&r.HourTS, &r.Model, &r.Protocol, &r.ProviderID, &r.Input, &r.Output,
			&r.CacheRead, &r.CacheWrite, &r.Calls, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageByModel 按 (小时, 模型, 协议, 渠道) 聚合。
func (s *Store) UsageByModel(f usageFilter) ([]usageModelRow, error) {
	where, args := f.whereTokens()
	q := `SELECT substr(ts,1,13) AS hour_ts, model, protocol, provider_id,
			COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0),
			COALESCE(SUM(cache_read),0), COALESCE(SUM(cache_write),0),
			COUNT(*), COALESCE(SUM(CASE WHEN ok=0 THEN 1 ELSE 0 END),0)
		FROM usage_events` + where + `
		GROUP BY hour_ts, model, protocol, provider_id ORDER BY COUNT(*) DESC`
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageModelRow{}
	for rows.Next() {
		var r usageModelRow
		if err := rows.Scan(&r.HourTS, &r.Model, &r.Protocol, &r.ProviderID, &r.Input, &r.Output,
			&r.CacheRead, &r.CacheWrite, &r.Calls, &r.Errors); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// whereSearch 只取搜索/抽取调用（按次计费的明细，没有 token）。
func (f usageFilter) whereSearch() (string, []any) {
	conds, args := f.baseConds()
	conds = append(conds, "kind IN ('search', 'extract')")
	return whereClause(conds, args)
}

// usageRecord 是「用量记录」明细表的一行（对外 JSON 口径）。
type usageRecord struct {
	TS         string `json:"ts"`
	SessionID  string `json:"sessionId"`
	Purpose    string `json:"purpose"`
	ProviderID string `json:"providerId"`
	KeyID      string `json:"keyId"`
	Model      string `json:"model"`
	Protocol   string `json:"protocol"`
	Input      int64  `json:"input"`
	CacheRead  int64  `json:"cacheRead"`
	CacheWrite int64  `json:"cacheWrite"`
	Output     int64  `json:"output"`
	Thinking   string `json:"thinking,omitempty"` // 本次调用实际使用的思考档位，空＝跟随服务端默认
	Total      int64  `json:"total"`
	TTFTMs     int64  `json:"ttftMs"`
	DurationMs int64  `json:"durationMs"`
	OK         bool   `json:"ok"`
	Error      string `json:"error,omitempty"`
	Stream     bool   `json:"stream"`
	// Peak 这次调用落在高峰（按峰值价计费）还是空闲（按空闲价）；页面 hover 显示。
	Peak bool `json:"peak"`
	// ---- 搜索/抽取调用专用 ----
	Kind    string `json:"kind,omitempty"`    // search | extract（模型调用为空）
	Detail  string `json:"detail,omitempty"`  // 搜索词/目标 URL 的截断摘要
	ErrKind string `json:"errKind,omitempty"` // 失败类别
	Rank    int    `json:"rank,omitempty"`    // 供应商顺位（1 起）
}

// UsageRecords 模型调用的明细分页（新的在前）与总条数。搜索/抽取明细走
// UsageSearchRecords（各自合计，别混在一张表里）。
func (s *Store) UsageRecords(f usageFilter, limit, offset int) ([]usageRecord, int, error) {
	where, args := f.whereTokens()
	return s.scanUsageRecords(where, args, limit, offset)
}

// UsageSearchRecords 搜索/抽取调用的明细分页（新的在前）与总条数。
func (s *Store) UsageSearchRecords(f usageFilter, limit, offset int) ([]usageRecord, int, error) {
	where, args := f.whereSearch()
	return s.scanUsageRecords(where, args, limit, offset)
}

// scanUsageRecords 明细分页的公共实现（token 类与搜索类共用的列与口径）。
func (s *Store) scanUsageRecords(where string, args []any, limit, offset int) ([]usageRecord, int, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > usageRecordPageMax {
		limit = usageRecordPageMax
	}
	if offset < 0 {
		offset = 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM usage_events`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT ts, session_id, purpose, provider_id, key_id, model, protocol,
			input_tokens, cache_read, cache_write, output_tokens, thinking, ttft_ms, duration_ms, ok, error, stream,
			kind, detail, err_kind, provider_rank
		FROM usage_events` + where + ` ORDER BY id DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []usageRecord{}
	for rows.Next() {
		var r usageRecord
		var ok, stream int
		if err := rows.Scan(&r.TS, &r.SessionID, &r.Purpose, &r.ProviderID, &r.KeyID, &r.Model, &r.Protocol,
			&r.Input, &r.CacheRead, &r.CacheWrite, &r.Output, &r.Thinking, &r.TTFTMs, &r.DurationMs, &ok, &r.Error, &stream,
			&r.Kind, &r.Detail, &r.ErrKind, &r.Rank); err != nil {
			return nil, 0, err
		}
		r.OK = ok != 0
		r.Stream = stream != 0
		r.Total = r.Input + r.Output
		r.Peak = usagePeakAt(r.TS)
		out = append(out, r)
	}
	return out, total, rows.Err()
}

// usageSearchGroupRow 是「搜索调用」按 (天, 供应商, 类型) 的原始聚合。
// 搜索按次计费：次数、成功次数（计费依据）、回落次数、总耗时（求平均）。
type usageSearchGroupRow struct {
	Day        string `json:"day"`
	ProviderID string `json:"providerId"`
	Kind       string `json:"kind"` // search | extract
	Calls      int64  `json:"calls"`
	Success    int64  `json:"success"`
	Fallback   int64  `json:"fallback"` // 顺位 >1 的次数：前面几家失败过
	DurationMs int64  `json:"durationMs"`
}

// UsageSearchGroups 按 (天, 供应商, 类型) 聚合搜索/抽取调用。
func (s *Store) UsageSearchGroups(f usageFilter) ([]usageSearchGroupRow, error) {
	where, args := f.whereSearch()
	q := `SELECT substr(ts,1,10) AS day, provider_id, kind,
			COUNT(*), COALESCE(SUM(CASE WHEN ok=1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(CASE WHEN provider_rank > 1 THEN 1 ELSE 0 END),0),
			COALESCE(SUM(duration_ms),0)
		FROM usage_events` + where + ` GROUP BY day, provider_id, kind ORDER BY day`
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []usageSearchGroupRow{}
	for rows.Next() {
		var r usageSearchGroupRow
		if err := rows.Scan(&r.Day, &r.ProviderID, &r.Kind, &r.Calls, &r.Success, &r.Fallback, &r.DurationMs); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UsageCleanupBetween 删除日期范围内（含首尾）的明细，返回删除条数。
func (s *Store) UsageCleanupBetween(fromDay, toDay string) (int64, error) {
	where, args := usageFilter{From: fromDay, To: toDay}.where()
	if where == "" {
		return 0, nil // 空条件会删全表，这里必须挡住
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM usage_events`+where, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// UsagePrune 保留策略：先删超龄（ts 早于 cutoffDay 的整天），再按条数上限删最旧。
// 返回总删除条数。
func (s *Store) UsagePrune(cutoffDay string, maxRows int) (int64, error) {
	var deleted int64
	s.mu.Lock()
	defer s.mu.Unlock()
	if strings.TrimSpace(cutoffDay) != "" {
		res, err := s.db.Exec(`DELETE FROM usage_events WHERE substr(ts,1,10) < ?`, cutoffDay)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	if maxRows > 0 {
		// 一次子查询定位「第 maxRows 新那条」的 id，比 NOT IN 全表排序省得多
		res, err := s.db.Exec(`DELETE FROM usage_events WHERE id <= (
			SELECT id FROM usage_events ORDER BY id DESC LIMIT 1 OFFSET ?)`, maxRows)
		if err != nil {
			return deleted, err
		}
		n, _ := res.RowsAffected()
		deleted += n
	}
	return deleted, nil
}

// UsageCount 明细总条数（启动清理与页面提示用）。
func (s *Store) UsageCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM usage_events`).Scan(&n)
	return n
}

// ---------- 保留策略：启动清一次 + 每天定时清一次 ----------

// pruneUsage 跑一遍保留策略（超 90 天 / 超 20 万条删最旧）。
func (a *App) pruneUsage() int64 {
	cutoff := time.Now().AddDate(0, 0, -usageRetentionDays).Format("2006-01-02")
	n, err := a.Store.UsagePrune(cutoff, usageMaxRows)
	if err != nil {
		log.Printf("[用量] 保留策略清理失败: %v", err)
		return 0
	}
	if n > 0 {
		log.Printf("[用量] 保留策略清理 %d 条（%d 天 / %d 条上限）", n, usageRetentionDays, usageMaxRows)
	}
	return n
}

// usageJanitor 启动先跑一轮，之后每天跑一轮（对齐 snapshot.go 的快照清理）。
func (a *App) usageJanitor() {
	a.pruneUsage()
	t := time.NewTicker(24 * time.Hour)
	defer t.Stop()
	for range t.C {
		a.pruneUsage()
	}
}
