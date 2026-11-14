package main

// 消息操作条「压缩」按钮（POST /api/sessions/{id}/compact，llm 模式）的异步
// 行为测试：模型摘要放后台 goroutine（202 立即返回、摘要行悄悄落库），前台只
// 做同步预检——点错位置、该区间已压过立即 400；压缩进行中重复点火 409。
// 用户主诉场景的对立面：压缩不再挂在 HTTP 请求上，切走对话不影响后台完成。

import (
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// postCompact 直调手动压缩处理器（body 可空字符串=不带 upTo）。
func postCompact(t *testing.T, a *App, sid, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	var rd *strings.Reader
	if body == "" {
		rd = strings.NewReader("{}")
	} else {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest("POST", "/api/sessions/"+sid+"/compact", rd)
	req.SetPathValue("id", sid)
	a.handleCompact(w, req)
	return w
}

// summaryCount 统计会话里指定 origin 的摘要行数。
func summaryCount(t *testing.T, a *App, sid, origin string) int {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, m := range all {
		if m.Kind == kindSummary && m.Origin == origin {
			n++
		}
	}
	return n
}

// 手动压缩（llm 模式）：202 async 立即返回，摘要行由后台 goroutine 落库，
// covers_up_to = 按钮所在轮的轮尾（upTo 指向第二轮的结论行，第二轮整轮入摘要）。
func TestManualCompactAsyncBackground(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	// 本仓库的 resolveTarget 只认「渠道+模型」的全局默认（裸模型名不再静默
	// 回落），不设就报「还没有设置全局默认模型」——与压缩行为无关的配置项
	a.Cfg.DefaultModel = ModelRef{Provider: "mock", Model: "mock-1"}
	seedAutoSession(t, a, "masync")
	_, _, _, final2 := seedRound(t, a, "masync", "第二轮b", true)

	w := postCompact(t, a, "masync", `{"upTo":`+strconv.FormatInt(final2, 10)+`}`)
	if w.Code != 202 {
		t.Fatalf("llm 模式手动压缩应 202（后台执行）, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"async":true`) {
		t.Fatalf("响应应带 async=true, got %s", w.Body.String())
	}
	// 后台落库（mock 摘要即时返回，短暂轮询即到）：manual 摘要一条，覆盖到
	// upTo 所在轮（第四轮「第二轮b」）的轮尾；落库与清标记之间隔着收尾广播，
	// 两者都等到才算完成
	done := false
	for i := 0; i < 100 && !done; i++ {
		all, _ := a.Store.RawMessagesAll("masync")
		for _, m := range all {
			if m.Kind == kindSummary && m.Origin == "manual" {
				if m.CoversUpTo != final2 {
					t.Fatalf("摘要应覆盖到 upTo 所在轮的轮尾(%d), got covers_up_to=%d", final2, m.CoversUpTo)
				}
				done = !a.compactBusyOf("masync")
			}
		}
		if !done {
			time.Sleep(20 * time.Millisecond)
		}
	}
	if !done {
		t.Fatal("后台摘要行未落库或压缩中标记未清")
	}

	// 同一区间再点：预检立即 400「已压缩过」，不再发起后台调用
	w = postCompact(t, a, "masync", `{"upTo":`+strconv.FormatInt(final2, 10)+`}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "没有可压缩的区间") {
		t.Fatalf("重复压同一位置应同步 400, got %d %s", w.Code, w.Body.String())
	}
	if got := summaryCount(t, a, "masync", "manual"); got != 1 {
		t.Fatalf("不该重复落摘要, got %d", got)
	}
}

// 点错位置（upTo 超出末尾）与压缩进行中重复点火：前者同步 400，后者 409。
func TestManualCompactPreflightAndBusy(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	// 本仓库的 resolveTarget 只认「渠道+模型」的全局默认（裸模型名不再静默
	// 回落），不设就报「还没有设置全局默认模型」——与压缩行为无关的配置项
	a.Cfg.DefaultModel = ModelRef{Provider: "mock", Model: "mock-1"}
	seedAutoSession(t, a, "mbusy")

	// upTo 指向不存在的行（比末条还大）：预检直接 400，历史不动
	w := postCompact(t, a, "mbusy", `{"upTo":9999}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "没有可压缩的回合") {
		t.Fatalf("超界 upTo 应同步 400, got %d %s", w.Code, w.Body.String())
	}

	// 压缩进行中（模拟：手动置位运行标记）再点：409，不并发起第二个摘要
	a.setCompactBusy("mbusy", true)
	t.Cleanup(func() { a.setCompactBusy("mbusy", false) })
	w = postCompact(t, a, "mbusy", `{"upTo":3}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "正在后台压缩") {
		t.Fatalf("压缩中重复点火应 409, got %d %s", w.Code, w.Body.String())
	}
	if got := summaryCount(t, a, "mbusy", "manual"); got != 0 {
		t.Fatalf("409 拦截下不该落摘要, got %d", got)
	}
}

// rule 模式的手动压缩保持同步（零模型调用，毫秒级完成，行为不回归）。
func TestManualCompactRuleStaysSync(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.CompactMode = "rule"
	starts := seedAutoSession(t, a, "mrule")

	w := postCompact(t, a, "mrule", `{"upTo":`+strconv.FormatInt(starts[1], 10)+`}`)
	if w.Code != 200 {
		t.Fatalf("rule 模式应同步 200, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"folded":`) {
		t.Fatalf("rule 模式响应应带 folded, got %s", w.Body.String())
	}
	if got := foldedUpto(t, a, "mrule"); got != starts[2]-1 {
		t.Fatalf("折叠边界应在第三轮起始(%d)之前一行, got %d", starts[2], got)
	}
}

// 摘要行的增量区间标注（2026-11-11 用户要求）：文本头写明「本次压缩第 X 至
// Y 条」，结构化字段 covers_from/covers_up_to 成对可检索；auto 二次压缩合并
// 旧摘要时累计起点保持（首压起点一路继承），manual 叠加存本次区间起点。
func TestSummaryCoversRangeAnnotation(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	// 本仓库的 resolveTarget 只认「渠道+模型」的全局默认（裸模型名不再静默
	// 回落），不设就报「还没有设置全局默认模型」——与压缩行为无关的配置项
	a.Cfg.DefaultModel = ModelRef{Provider: "mock", Model: "mock-1"}
	// 本仓库：resolveTarget 只认「渠道+模型」的全局默认（裸模型名不再静默回落）
	a.Cfg.DefaultModel = ModelRef{Provider: "mock", Model: "mock-1"}
	sess := &SessionMeta{ID: "range1", Title: "区间标注", Mode: "task", Workspace: ".",
		CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	a.Cfg.KeepChars = 1000 // 保留窗口只装下最后一轮，前面轮次构成可压区间
	seedAutoSession(t, a, "range1")
	seedRound(t, a, "range1", "第四轮", true)

	// 第一次 auto：压 (0, segEnd]，起点 1
	if _, err := a.doSummarize(sess, "auto", -1, 0); err != nil {
		t.Fatalf("第一次 auto 压缩: %v", err)
	}
	first := activeSummaryOf(t, a, "range1", "auto")
	if first.CoversFrom != 1 {
		t.Fatalf("首压 covers_from 应为 1, got %d", first.CoversFrom)
	}
	if !strings.Contains(first.Text(), "本次压缩第 1 至") {
		t.Fatalf("首压文本头应含「本次压缩第 1 至 …」: %s", first.Text())
	}

	// 第二次 auto：窗口推进后新区间从 boundary+1 起，累计起点继承首压的 1
	_, _, _, final5 := seedRound(t, a, "range1", "第五轮", true)
	if _, err := a.doSummarize(sess, "auto", -1, 0); err != nil {
		t.Fatalf("第二次 auto 压缩: %v", err)
	}
	second := activeSummaryOf(t, a, "range1", "auto")
	if second.CoversFrom != 1 {
		t.Fatalf("合并后累计 covers_from 应保持 1, got %d", second.CoversFrom)
	}
	if !strings.Contains(second.Text(), "合并先前摘要后累计覆盖第 1 至") {
		t.Fatalf("合并摘要文本头应含累计区间: %s", second.Text())
	}

	// manual 叠加：本次区间起点 = 当前边界+1（不继承 auto 的累计起点）
	if _, err := a.doSummarize(sess, "manual", -1, final5); err != nil {
		t.Fatalf("manual 压缩: %v", err)
	}
	m := activeSummaryOf(t, a, "range1", "manual")
	if m.CoversFrom <= second.CoversUpTo || m.CoversFrom > final5 {
		t.Fatalf("manual covers_from 应为本次区间起点(%d, %d], got %d", second.CoversUpTo, final5, m.CoversFrom)
	}
}

// activeSummaryOf 取当前活跃（未被取代）的指定 origin 摘要行；没有则 Fail。
func activeSummaryOf(t *testing.T, a *App, sid, origin string) *StoredMessage {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range all {
		if m.Kind == kindSummary && m.Origin == origin && !m.Superseded {
			return m
		}
	}
	t.Fatalf("没有活跃的 %s 摘要行", origin)
	return nil
}
