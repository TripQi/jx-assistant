package main

// 右键上下文菜单「提前自动压缩」（POST /api/sessions/{id}/autocompact）的测试：
// 它走的是自动压缩线路（autoCompactNow）——按设置的保留字数划界（KeepChars 变了
// 边界跟着变）、规则折叠/模型摘要按当前配置选路（摘要 origin=auto，与手动全量
// 压缩的 manual 区分开）、两个通道都没启用时报错且不动历史。设置页
// 「压缩数字助理主对话」固定模型摘要的行为不回归（见 assistant_ops_test.go）。

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// postAutoCompact 直接调处理器（路由变量手动注入，不经 mux 匹配）。
func postAutoCompact(t *testing.T, a *App, sid string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/api/sessions/"+sid+"/autocompact", nil)
	req.SetPathValue("id", sid)
	a.handleAutoCompact(w, req)
	return w
}

// seedAutoSession 建一个任务会话并落 3 轮历史（每轮带一次工具调用），返回每轮
// 起始行（用户消息）的 seq。
func seedAutoSession(t *testing.T, a *App, id string) [3]int64 {
	t.Helper()
	m := &SessionMeta{ID: id, Title: "自动压缩测试", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(m); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}
	var starts [3]int64
	for i, label := range []string{"第一轮", "第二轮", "第三轮"} {
		user, _, _, _ := seedRound(t, a, id, label, true)
		starts[i] = user
	}
	return starts
}

// foldedUpto 返回会话里最晚一条带折叠标记（compact 非空）的 seq，0=没有折叠。
func foldedUpto(t *testing.T, a *App, sid string) int64 {
	t.Helper()
	all, err := a.Store.RawMessagesAll(sid)
	if err != nil {
		t.Fatal(err)
	}
	upTo := int64(0)
	for _, m := range all {
		if m.Compact != "" && m.Seq > upTo {
			upTo = m.Seq
		}
	}
	return upTo
}

// 按设置字数划界：保留预算很小（KeepChars=10）时只保最近一轮，前两轮折叠，
// 折叠边界正好落在第三轮起始行之前（与自动压缩同一保留窗口口径）。
func TestAutoCompactButtonFollowsKeepChars(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.CompactInterval = 1 // 规则通道启用（不等到没到间隔，按钮就是提前点火）
	a.Cfg.CompactLlmPct = 0
	a.Cfg.KeepChars = 10
	starts := seedAutoSession(t, a, "autok")

	w := postAutoCompact(t, a, "autok")
	if w.Code != 200 {
		t.Fatalf("应 200, got %d %s", w.Code, w.Body.String())
	}
	// folded 是折叠的行数（ruleCompactAuto 口径）：这里应折掉前两轮全部 8 行
	if !strings.Contains(w.Body.String(), `"folded":8`) {
		t.Fatalf("保留预算 10 字应只保最近一轮（折前两轮 8 行）, got %s", w.Body.String())
	}
	if got := foldedUpto(t, a, "autok"); got != starts[2]-1 {
		t.Fatalf("折叠边界应在第三轮起始(%d)之前一行, got %d", starts[2], got)
	}
	// 保留段（第三轮起）原文不动：没有任何折叠标记
	all, _ := a.Store.RawMessagesAll("autok")
	for _, m := range all {
		if m.Seq >= starts[2] && m.Compact != "" {
			t.Fatalf("保留段第 %d 行不该被折叠", m.Seq)
		}
	}
}

// 设置改字数后行为跟变：保留预算放大到装下全部三轮，再点按钮就没有可折叠的
// 回合（400 透传下层文案），历史一行不动。
func TestAutoCompactKeepCharsChangeMovesBoundary(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.CompactInterval = 1
	a.Cfg.CompactLlmPct = 0
	a.Cfg.KeepChars = 100000
	seedAutoSession(t, a, "autowide")
	before := msgCount(t, a, "autowide")

	w := postAutoCompact(t, a, "autowide")
	if w.Code != 400 {
		t.Fatalf("预算内没东西可压应 400, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "没有可折叠的回合") {
		t.Fatalf("应透传下层文案, got %s", w.Body.String())
	}
	if n := msgCount(t, a, "autowide"); n != before {
		t.Fatalf("失败的压缩不该改动历史, %d -> %d", before, n)
	}
	if foldedUpto(t, a, "autowide") != 0 {
		t.Fatalf("不该产生任何折叠标记")
	}
}

// 按当前配置选路：规则通道关闭、只开模型摘要时，按钮走 doSummarize(auto)——
// 摘要行 origin=auto（自动线路），不是手动全量的 manual。
func TestAutoCompactLlmChannelAutoOrigin(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Providers = []ProviderCfg{{
		ID: "mock", Name: "离线演示", BaseURL: "mock",
		Models: []ModelCfg{{ID: "mock-1", Name: "演示模型"}},
	}}
	a.Cfg.CompactInterval = 0
	a.Cfg.CompactLlmPct = 80
	a.Cfg.KeepChars = 10
	seedAutoSession(t, a, "autollm")

	w := postAutoCompact(t, a, "autollm")
	if w.Code != 200 {
		t.Fatalf("应 200, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"scope":"auto"`) || !strings.Contains(w.Body.String(), `"summary"`) {
		t.Fatalf("摘要通路的响应应带 scope=auto 与 summary, got %s", w.Body.String())
	}
	all, _ := a.Store.RawMessagesAll("autollm")
	auto, manual := 0, 0
	for _, m := range all {
		if m.Kind != kindSummary {
			continue
		}
		if m.Origin == "auto" {
			auto++
		}
		if m.Origin == "manual" {
			manual++
		}
	}
	if auto != 1 || manual != 0 {
		t.Fatalf("应落 1 条 origin=auto 的摘要行（自动线路）, got auto=%d manual=%d", auto, manual)
	}
}

// 两个通道都没启用（间隔 0 且摘要阈值 0）时按钮报错并说明去哪开，历史不动。
func TestAutoCompactChannelsOffIsError(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.CompactInterval = 0
	a.Cfg.CompactLlmPct = 0
	seedAutoSession(t, a, "autooff")
	before := msgCount(t, a, "autooff")

	w := postAutoCompact(t, a, "autooff")
	if w.Code != 400 {
		t.Fatalf("未启用应 400, got %d %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "自动压缩未启用") {
		t.Fatalf("应说明未启用, got %s", w.Body.String())
	}
	if n := msgCount(t, a, "autooff"); n != before {
		t.Fatalf("不该改动历史, %d -> %d", before, n)
	}
}
