// websearch_api.go —— 搜索供应商的「测试这一家」端点（设置页每行的「测试」按钮）。
//
// 为什么单独一个端点：用户在设置页最需要知道的不是「保存成功」，而是「这把 Key
// 到底能不能用」。直接打一次最小请求（count=1）并把失败类别翻成人话（Key 无效 /
// 额度用尽 / 被限流 / 连不上），不用等到真的搜索时才踩坑。配置回显与保存的视图
// 转换在 websearch_providers.go。
package main

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleTestSearchProvider 设置页「测试」按钮：对某一家人最小的一次搜索请求，
// 回 HTTP 状态、耗时、条数与首条标题；失败给出可读的类别（Key 无效 / 额度用尽…）。
func (a *App) handleTestSearchProvider(w http.ResponseWriter, r *http.Request) {
	var in struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || strings.TrimSpace(in.ID) == "" {
		writeErr(w, 400, "需要供应商 id")
		return
	}
	id := strings.TrimSpace(in.ID)
	var found *SearchProviderCfg
	for i := range a.Cfg.SearchProviders {
		if a.Cfg.SearchProviders[i].ID == id {
			found = &a.Cfg.SearchProviders[i]
			break
		}
	}
	if found == nil {
		writeErr(w, 404, "供应商不存在")
		return
	}
	// 用一个临时的 activeSearchProvider 走真实请求路径：测试与实际搜索共用同一段
	// 代码，测试通过就说明真搜也能通（不会出现「测试绿、实际红」）
	pv := activeSearchProvider{cfg: *found, rank: 1}
	if found.Kind == spKindGateway {
		base, err := a.searchEndpointBase()
		if err != nil {
			writeJSON(w, 200, map[string]any{"ok": false, "kindText": searchErrConfig.label(), "error": err.Error()})
			return
		}
		pv.base = strings.TrimRight(base, "/")
	} else if defaultSearchBaseURL(found.Kind) == "" {
		writeJSON(w, 200, map[string]any{"ok": false, "kindText": searchErrConfig.label(),
			"error": "不支持的供应商类型：" + found.Kind})
		return
	} else {
		pv.base = found.base()
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	pctx, pcancel := providerCtx(ctx, pv.cfg.timeout())
	defer pcancel()
	started := time.Now()
	out, err := a.runProviderSearch(pctx, pv, providerSearchCall{query: "OpenAI", count: 1, sessID: ""})
	ms := time.Since(started).Milliseconds()
	if err != nil {
		resp := map[string]any{"ok": false, "ms": ms, "error": err.Error(), "kindText": ""}
		if pe, ok := err.(*providerError); ok {
			// 只回具体原因（没有就留空）：类别由 kindText 单独展示，前端拼成
			// 「✗ Key 无效或无权限（HTTP 401）」，避免同一句话出现两遍
			resp["kind"] = int(pe.Kind)
			resp["kindText"] = pe.KindText
			resp["status"] = pe.Status
			resp["error"] = pe.Msg
		}
		writeJSON(w, 200, resp)
		return
	}
	first := map[string]string{}
	if len(out.Results) > 0 {
		first["title"] = out.Results[0].Title
		first["url"] = out.Results[0].URL
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "ms": ms, "count": len(out.Results), "engine": out.Engine, "first": first,
	})
}
