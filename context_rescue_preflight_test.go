package main

// 第 4 项验收：主循环「组装即超窗」的预检分支（项目知识模式那条早退）不许吞掉
// 超限救援的可行动作。
//
// 基线（7e867065）行为：组装完就已越过窗口时直接 return 一句「请整理内容或由
// 用户调整配置」——估算/窗口之后什么都没有，用户不知道该删什么、该换什么模型；
// 而正常超限路径（context_rescue.go 的 overflowCompact）本来就把这些话说得很
// 清楚，信息在早退分支被掐掉了。本用例只断言用户可见的那条错误（RunAgent 返回
// 值，前端报错条直接显示它），因此可以原样拷到基线上跑：那里必红。

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestOverWindowPreflightKeepsActionableRescue(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		// 供应商口径的超限拒绝（唯一权威：本地估算只是启发式）
		w.WriteHeader(http.StatusBadRequest)
		fmt.Fprint(w, `{"error":{"message":"maximum context length exceeded"}}`)
	}))
	defer srv.Close()

	a, s := rescueTestApp(t, srv.URL, 4096)
	a.Cfg.CompactLlmPct = 100 // 规则压缩关闭、模型救援启用（救援会在预算预检被挡）
	original := "预检就超窗时，用户原文必须保留"
	if err := a.Store.AppendRow(s.ID, &StoredMessage{Message: TextMessage("user", original), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}

	err := a.RunAgent(context.Background(), s, func(Event) {})
	if err == nil {
		t.Fatal("超窗且救援失败必须报错，不能静默继续")
	}
	msg := err.Error()
	// 可行动作三件套：超了多少（估算 / 当前窗口）、怎么办（手动精简 / 换更大窗口）、
	// 以及原文没丢（救援失败的既有承诺）。
	for _, want := range []string{"估算", "4096", "手动", "更大", "原文已保留"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("可行动作缺 %q：%s", want, msg)
		}
	}
	// 预检不许抢在请求前硬退：本地估算只是启发式，供应商答复才权威。
	if hits.Load() != 1 {
		t.Fatalf("应只发主请求一次（救援在预算预检本地拒绝），实际请求 %d 次", hits.Load())
	}
	raw, err := a.Store.RawMessagesAll(s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if raw[0].Text() != original || raw[0].Compact != "" {
		t.Fatal("救援失败不许改写原文")
	}
	for _, m := range raw {
		if m.Kind == kindSummary {
			t.Fatal("失败的救援不许落摘要")
		}
	}
	t.Logf("预检超窗 + 救援失败的可行动作：%s", msg)
}
