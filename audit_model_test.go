package main

// 模型调用审计行补记模型名的验收测试：主循环每次真实发出的模型请求，落库的
// call 审计行（type=call）与 request 行（kind=request）都要带本次实际使用的
// 模型（provider/model 形式）——将来按模型拆账算成本靠它（sessions 表只有
// 当前 model，中途换模型的会话拆不开）。历史旧行没有该字段、留空不补。
// 侧路摘要调用（scope=summarize）同一口径，见 CallSummarizer。

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCallAuditRowCarriesModel(t *testing.T) {
	// OpenAI 兼容 SSE 假模型：一行正文 + 流尾 usage + [DONE]
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"做完了\"}}]}\n\n")
		fmt.Fprint(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5}}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(srv.Close)

	a := newCompactTestApp(t)
	a.initRuntime()
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{{ID: "fakep", Name: "假渠道", BaseURL: srv.URL,
		Models: []ModelCfg{{ID: "fake-m"}}}}
	// 默认模型体系：会话没手选时跟全局默认模型（必设，不再拿第一个渠道充数）
	a.Cfg.DefaultModel = ModelRef{Provider: "fakep", Model: "fake-m"}
	sess := &SessionMeta{ID: "amodel", Title: "审计行模型", Mode: "task", Workspace: a.Cfg.Workspace,
		CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sess); err != nil {
		t.Fatalf("建会话失败: %v", err)
	}

	// 走真实发送路径：RunAgent 主循环 → StreamChat → RecordCall/RecordRequest
	if err := a.RunAgent(context.Background(), sess, func(Event) {}); err != nil {
		t.Fatalf("RunAgent 失败: %v", err)
	}

	raw, err := a.Store.RawMessagesAll(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	var call *auditRow
	for _, m := range raw {
		if m.Kind != kindAudit {
			continue
		}
		var row auditRow
		if json.Unmarshal([]byte(m.Text()), &row) == nil && row.Type == "call" {
			call = &row
		}
	}
	if call == nil {
		t.Fatal("没有找到 type=call 的审计行")
	}
	if call.Model != "fakep/fake-m" {
		t.Fatalf("call 审计行应记本次实际使用的 provider/model，得到 %q", call.Model)
	}
	if call.Usage == nil || call.Usage.Prompt != 10 || call.Usage.Completion != 5 {
		t.Fatalf("usage 应照常记录（模型名不能挤掉既有字段）: %+v", call.Usage)
	}

	// request 行（kind=request）同一口径
	recs, err := a.ctxEngine().RecentRequests(sess, keepRequests)
	if err != nil || len(recs) == 0 {
		t.Fatalf("没有找到 request 行: %v", err)
	}
	if last := recs[len(recs)-1]; last.Model != "fakep/fake-m" {
		t.Fatalf("request 行应记 provider/model，得到 %q", last.Model)
	}
}
