// websearch_search.go —— 各家搜索供应商的实际请求与响应归一。
//
// 一家一段：kind 决定端点路径、请求体字段名与响应字段名（五家的字段名差得很远：
// 智谱的地址叫 link、博查的标题叫 name、AnySearch 的结果埋在 data.results），
// 归一成 searchResponse 之后上游差异就到此为止，回落与渲染只认统一形状。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// providerSearchCall 一次搜索的入参（各家共用）。
type providerSearchCall struct {
	query  string
	count  int
	sessID string // 公司网关按对话轮询搜索引擎，需要会话标识
}

// runProviderSearch 调一家供应商做搜索：成功（含 0 条结果）返回归一后的结果，
// 失败返回 *providerError（类别决定文案，回落由调用方做）。
func (a *App) runProviderSearch(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	if providerNeedsKey(p.cfg.Kind) && strings.TrimSpace(p.cfg.APIKey) == "" {
		return searchResponse{}, newProviderError(p, 0, searchErrAuth, "未配置 API Key")
	}
	switch p.cfg.Kind {
	case spKindGateway:
		return a.searchViaGateway(ctx, p, call)
	case spKindDeepSeek:
		return a.searchViaDeepSeek(ctx, p, call)
	case spKindTavily:
		return a.searchViaTavily(ctx, p, call)
	case spKindAnySearch:
		return a.searchViaAnySearch(ctx, p, call)
	case spKindBocha:
		return a.searchViaBocha(ctx, p, call)
	case spKindZhipu:
		return a.searchViaZhipu(ctx, p, call)
	}
	return searchResponse{}, newProviderError(p, 0, searchErrConfig, "不支持的供应商类型 "+p.cfg.Kind)
}

// providerNeedsKey 该公司类型是否必须带 API Key（AnySearch 支持匿名；网关由服务端
// 持 key，本机不填）。
func providerNeedsKey(kind string) bool {
	return kind != spKindGateway && kind != spKindAnySearch
}

// clampCount 按各家上限夹取条数（超上限会被上游 400 拒，白白回落一次）。
func clampCount(kind string, n int) int {
	if n <= 0 {
		n = 8
	}
	max := 20
	switch kind {
	case spKindAnySearch:
		max = 10
	case spKindTavily:
		max = 20
	case spKindDeepSeek, spKindBocha, spKindZhipu:
		max = 50
	}
	if n > max {
		n = max
	}
	return n
}

// newSearchResponse 起一个带来源标注的响应（顺位由调用方回填）。
func newSearchResponse(kind string) searchResponse {
	return searchResponse{Engine: providerEngineLabel(kind) + "/search"}
}

// appendHit 归一一条结果（snippet 取各家的摘要字段；正文不塞进搜索结果——那是
// web_fetch 的活，塞进来会白烧上下文）。
func appendHit(out *searchResponse, title, link, snippet, site, date string) {
	link = strings.TrimSpace(link)
	if link == "" {
		return
	}
	out.Results = append(out.Results, searchHit{
		Title:   strings.TrimSpace(title),
		URL:     link,
		Snippet: strings.TrimSpace(snippet),
		Site:    strings.TrimSpace(site),
		Date:    normalizeSearchDate(date),
	})
}

// normalizeSearchDate 各家日期格式统一成 YYYY-MM-DD（认不出就留空，别瞎猜）。
func normalizeSearchDate(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	for _, layout := range []string{time.RFC3339, time.RFC3339Nano, time.RFC1123, time.RFC1123Z, "2006-01-02 15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("2006-01-02")
		}
	}
	if len(s) >= 10 && s[4] == '-' && s[7] == '-' {
		return s[:10]
	}
	return ""
}

// ---------- HTTP ----------

// postProviderJSON 带鉴权头发一次 JSON POST，返回状态码与响应体。
func postProviderJSON(ctx context.Context, p activeSearchProvider, endpoint string, payload any) (int, []byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return 0, nil, newProviderError(p, 0, searchErrConfig, "请求体无法编码: "+err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return 0, nil, newProviderError(p, 0, searchErrConfig, "Base URL 不合法: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if k := strings.TrimSpace(p.cfg.APIKey); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	status, raw, err := doProviderHTTP(req, p.cfg.timeout())
	if err != nil {
		return 0, nil, doProviderErr(p, err)
	}
	return status, raw, nil
}

// doProviderHTTP 发请求并读回响应体（限 providerBodyLimit）。
func doProviderHTTP(req *http.Request, timeout time.Duration) (int, []byte, error) {
	client := newCompanyHTTPClient(timeout)
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, providerBodyLimit))
	return resp.StatusCode, raw, nil
}

// doProviderErr 网络层错误归类：超时/连不上都是一家的失败，上层逐家回落。
func doProviderErr(p activeSearchProvider, err error) *providerError {
	return newProviderError(p, 0, searchErrNetwork, err.Error())
}

// providerErrMessage 从错误响应体里取一句人能读的原因。凭据类状态码（401/402/
// 403/429）一律不转述响应体：AnySearch 的 402 会回账号密码与 api_key（调研报告
// §4.7 明确要求不得进日志/模型上下文）。
func providerErrMessage(kind string, status int, raw []byte) string {
	if status == 401 || status == 402 || status == 403 || status == 429 {
		if kind == spKindBocha && status == 401 {
			return "API Key 无效" // 博查 401 是空 body，给一句确定的说法
		}
		return ""
	}
	var probe struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
		Detail  json.RawMessage `json:"detail"`
		Msg     string          `json:"msg"`
	}
	if json.Unmarshal(raw, &probe) == nil {
		if len(probe.Error) > 0 {
			var e struct {
				Message string `json:"message"`
			}
			if json.Unmarshal(probe.Error, &e) == nil && e.Message != "" {
				return truncateRunes(e.Message, 200)
			}
		}
		for _, m := range []string{probe.Message, probe.Msg} {
			if s := strings.TrimSpace(m); s != "" {
				return truncateRunes(s, 200)
			}
		}
		if len(probe.Detail) > 0 {
			var d struct {
				Error string `json:"error"`
			}
			if json.Unmarshal(probe.Detail, &d) == nil && d.Error != "" {
				return truncateRunes(d.Error, 200)
			}
			return truncateRunes(string(probe.Detail), 200)
		}
	}
	txt := strings.TrimSpace(string(raw))
	if txt == "" {
		return ""
	}
	return truncateRunes(txt, 200)
}

// providerHTTPError 非 2xx 的统一出口（搜索口径）。
func providerHTTPError(p activeSearchProvider, status int, raw []byte) *providerError {
	return newProviderError(p, status, classifySearchStatus(status), providerErrMessage(p.cfg.Kind, status, raw))
}

// ---------- 各家 ----------

// searchViaGateway 公司网关（老链路）：POST <base>/api/search，响应结构与
// searchResponse 一致，引擎名以服务端回的为准。
func (a *App) searchViaGateway(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	payload, _ := json.Marshal(map[string]any{"query": call.query, "count": clampCount(spKindGateway, call.count)})
	req, err := a.marketRequest(ctx, http.MethodPost, p.base+"/api/search", bytes.NewReader(payload))
	if err != nil {
		return searchResponse{}, newProviderError(p, 0, searchErrConfig, "请求构造失败: "+err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	// sess 在 authorize 处已保证非空；服务端按此头对对话轮询搜索引擎
	if call.sessID != "" {
		req.Header.Set(searchConversationHeader, call.sessID)
	}
	status, raw, err := doProviderHTTP(req, p.cfg.timeout())
	if err != nil {
		// 老文案保留（"连接服务端失败"）：排障时用户/日志一眼认得是网关不通
		return searchResponse{}, newProviderError(p, 0, searchErrNetwork,
			fmt.Sprintf("连接服务端失败（%s）: %v", p.base, err))
	}
	var out searchResponse
	if err := json.Unmarshal(raw, &out); err != nil {
		if status >= 400 {
			return searchResponse{}, newProviderError(p, status, classifySearchStatus(status),
				truncateRunes(string(raw), 300))
		}
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	if status >= 400 {
		msg := out.Error
		if msg == "" {
			msg = fmt.Sprintf("HTTP %d", status)
		}
		return searchResponse{}, newProviderError(p, status, classifySearchStatus(status), msg)
	}
	if out.Engine == "" {
		out.Engine = providerEngineLabel(spKindGateway) + "/search"
	}
	return out, nil
}

// searchViaDeepSeek POST /v1/tools/search：results[].title/url/snippet/summary/date，
// 每条自带 600–800 字 summary（snippet 为空时拿它当摘要）。
func (a *App) searchViaDeepSeek(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	status, raw, err := postProviderJSON(ctx, p, p.base+"/v1/tools/search", map[string]any{
		"query": call.query, "count": clampCount(spKindDeepSeek, call.count), "need_content": false,
	})
	if err != nil {
		return searchResponse{}, err
	}
	if status >= 400 {
		return searchResponse{}, providerHTTPError(p, status, raw)
	}
	var body struct {
		Results []struct {
			Title   string `json:"title"`
			URL     string `json:"url"`
			Snippet string `json:"snippet"`
			Summary string `json:"summary"`
			Date    string `json:"date"`
			Site    string `json:"site_name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	out := newSearchResponse(spKindDeepSeek)
	for _, r := range body.Results {
		snippet := r.Snippet
		if strings.TrimSpace(snippet) == "" {
			snippet = r.Summary
		}
		appendHit(&out, r.Title, r.URL, snippet, r.Site, r.Date)
	}
	return out, nil
}

// searchViaTavily POST /search：results[].title/url/content/score/published_date。
func (a *App) searchViaTavily(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	payload := map[string]any{
		"query": call.query, "max_results": clampCount(spKindTavily, call.count),
		"search_depth": "basic", "include_answer": false, "include_raw_content": false,
	}
	if p.cfg.Freshness != "" {
		payload["time_range"] = p.cfg.Freshness
	}
	if p.cfg.Region != "" {
		payload["country"] = p.cfg.Region
	}
	status, raw, err := postProviderJSON(ctx, p, p.base+"/search", payload)
	if err != nil {
		return searchResponse{}, err
	}
	if status >= 400 {
		return searchResponse{}, providerHTTPError(p, status, raw)
	}
	var body struct {
		Results []struct {
			Title         string `json:"title"`
			URL           string `json:"url"`
			Content       string `json:"content"`
			PublishedDate string `json:"published_date"`
		} `json:"results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	out := newSearchResponse(spKindTavily)
	for _, r := range body.Results {
		appendHit(&out, r.Title, r.URL, r.Content, "", r.PublishedDate)
	}
	return out, nil
}

// searchViaAnySearch POST /v1/search：{code,message,data:{results:[{title,url,snippet,content}]}}；
// code != 0 表示业务失败（402=额度耗尽、422=抽取失败、502=服务不可用）。
func (a *App) searchViaAnySearch(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	payload := map[string]any{"query": call.query, "max_results": clampCount(spKindAnySearch, call.count)}
	if p.cfg.Region != "" {
		payload["zone"] = p.cfg.Region
	}
	status, raw, err := postProviderJSON(ctx, p, p.base+"/v1/search", payload)
	if err != nil {
		return searchResponse{}, err
	}
	if status >= 400 {
		return searchResponse{}, providerHTTPError(p, status, raw)
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Results []struct {
				Title   string `json:"title"`
				URL     string `json:"url"`
				Snippet string `json:"snippet"`
				Content string `json:"content"`
			} `json:"results"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	if body.Code != 0 {
		return searchResponse{}, newProviderError(p, status, searchErrParse,
			fmt.Sprintf("业务码 %d: %s", body.Code, truncateRunes(body.Message, 120)))
	}
	out := newSearchResponse(spKindAnySearch)
	for _, r := range body.Data.Results {
		snippet := r.Snippet
		if strings.TrimSpace(snippet) == "" {
			snippet = r.Content
		}
		appendHit(&out, r.Title, r.URL, snippet, "", "")
	}
	return out, nil
}

// searchViaBocha POST /v1/web-search：Bing 兼容结构 webPages.value[]（name/url/
// snippet/summary）；dateLastCrawled 是「博查抓取时间」不是发布时间，不当日期用。
func (a *App) searchViaBocha(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	freshness := p.cfg.Freshness
	if freshness == "" {
		freshness = "noLimit"
	}
	payload := map[string]any{
		"query": call.query, "count": clampCount(spKindBocha, call.count),
		"summary": true, "freshness": freshness,
	}
	status, raw, err := postProviderJSON(ctx, p, p.base+"/v1/web-search", payload)
	if err != nil {
		return searchResponse{}, err
	}
	if status >= 400 {
		return searchResponse{}, providerHTTPError(p, status, raw)
	}
	var body struct {
		WebPages *struct {
			Value []struct {
				Name     string `json:"name"`
				URL      string `json:"url"`
				Snippet  string `json:"snippet"`
				Summary  string `json:"summary"`
				SiteName string `json:"siteName"`
			} `json:"value"`
		} `json:"webPages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	if body.WebPages == nil {
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应里没有 webPages 字段")
	}
	out := newSearchResponse(spKindBocha)
	for _, r := range body.WebPages.Value {
		snippet := r.Snippet
		if strings.TrimSpace(snippet) == "" {
			snippet = r.Summary
		}
		appendHit(&out, r.Name, r.URL, snippet, r.SiteName, "")
	}
	return out, nil
}

// searchViaZhipu POST /api/paas/v4/web_search：search_result[]（地址字段叫 link、
// 摘要叫 content）；业务错误码 1701 限流 / 1702 无可用引擎 / 1703 无有效数据。
func (a *App) searchViaZhipu(ctx context.Context, p activeSearchProvider, call providerSearchCall) (searchResponse, error) {
	engine := p.cfg.Engine
	if engine == "" {
		engine = "search_pro"
	}
	payload := map[string]any{
		"search_query": truncateRunes(call.query, 70), // 官方上限 70 字符：长 query 必须截断
		"search_engine": engine,
		"search_intent": false,
		"count":         clampCount(spKindZhipu, call.count),
	}
	if p.cfg.Freshness != "" {
		payload["search_recency_filter"] = p.cfg.Freshness
	}
	status, raw, err := postProviderJSON(ctx, p, p.base+"/api/paas/v4/web_search", payload)
	if err != nil {
		return searchResponse{}, err
	}
	var body struct {
		SearchResult []struct {
			Title       string `json:"title"`
			Link        string `json:"link"`
			Content     string `json:"content"`
			Media       string `json:"media"`
			PublishDate string `json:"publish_date"`
		} `json:"search_result"`
		Error struct {
			Code    any    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	// 智谱的业务错误可能配 200 也可能配 4xx，两条都要认
	if err := json.Unmarshal(raw, &body); err != nil {
		if status >= 400 {
			return searchResponse{}, providerHTTPError(p, status, raw)
		}
		return searchResponse{}, newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	if body.Error.Message != "" || body.Error.Code != nil {
		code := fmt.Sprint(body.Error.Code)
		kind := searchErrParse
		switch code {
		case "1701":
			kind = searchErrRateLimit
		case "1702":
			kind = searchErrUpstream
		}
		return searchResponse{}, newProviderError(p, status, kind,
			fmt.Sprintf("业务码 %s: %s", code, truncateRunes(body.Error.Message, 120)))
	}
	if status >= 400 {
		return searchResponse{}, providerHTTPError(p, status, raw)
	}
	out := newSearchResponse(spKindZhipu)
	for _, r := range body.SearchResult {
		appendHit(&out, r.Title, r.Link, r.Content, r.Media, r.PublishDate)
	}
	return out, nil
}
