// websearch_extract.go —— web_fetch 的抽取兜底：本地直抓拿不到正文时，按顺位
// 找具备 extract 能力的供应商代抓，返回正文（Markdown/纯文本）并标注来源。
//
// 触发判据在 tools.go 的 fetchWebPage（403/429、类型不支持、JS 空壳、超时），
// 这里只负责「换谁去抽」：逐家试，第一家成功就用；全失败把逐家原因交回上层，
// 让用户看到的是「哪家因为什么没成」，而不是一句笼统的抓取失败。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// extractProviders 具备抽取能力且启用的供应商（按 order）。
func (a *App) extractProviders() []activeSearchProvider {
	return a.searchProvidersFor(func(c SearchProviderCaps) bool { return c.Extract })
}

// runProviderExtract 调一家供应商抽正文：返回标题与正文（Markdown/纯文本）。
func (a *App) runProviderExtract(ctx context.Context, p activeSearchProvider, pageURL string) (string, string, error) {
	if providerNeedsKey(p.cfg.Kind) && strings.TrimSpace(p.cfg.APIKey) == "" {
		return "", "", newProviderError(p, 0, searchErrAuth, "未配置 API Key")
	}
	switch p.cfg.Kind {
	case spKindDeepSeek:
		return a.extractViaDeepSeek(ctx, p, pageURL)
	case spKindTavily:
		return a.extractViaTavily(ctx, p, pageURL)
	case spKindAnySearch:
		return a.extractViaAnySearch(ctx, p, pageURL)
	}
	return "", "", newProviderError(p, 0, searchErrConfig, "该类型不支持正文抽取")
}

// extractViaDeepSeek POST /v1/tools/fetch：单 URL，正文即 Markdown。
func (a *App) extractViaDeepSeek(ctx context.Context, p activeSearchProvider, pageURL string) (string, string, error) {
	status, raw, err := postProviderJSON(ctx, p, p.base+"/v1/tools/fetch", map[string]any{"url": pageURL})
	if err != nil {
		return "", "", err
	}
	if status >= 400 {
		return "", "", newProviderError(p, status, classifyExtractStatus(status),
			providerErrMessage(p.cfg.Kind, status, raw))
	}
	var body struct {
		Title   string `json:"title"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", "", newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	if strings.TrimSpace(body.Content) == "" {
		return "", "", newProviderError(p, status, searchErrRejected, "返回的正文为空")
	}
	return body.Title, body.Content, nil
}

// extractViaTavily POST /extract：urls 数组（这里只放一个），返回 results[].raw_content。
// HTTP 200 也可能全失败（results 空、failed_results 有内容），两个数组都要看。
func (a *App) extractViaTavily(ctx context.Context, p activeSearchProvider, pageURL string) (string, string, error) {
	status, raw, err := postProviderJSON(ctx, p, p.base+"/extract", map[string]any{
		"urls": []string{pageURL}, "extract_depth": "basic", "format": "markdown", "timeout": 30,
	})
	if err != nil {
		return "", "", err
	}
	if status >= 400 {
		return "", "", newProviderError(p, status, classifyExtractStatus(status),
			providerErrMessage(p.cfg.Kind, status, raw))
	}
	var body struct {
		Results []struct {
			URL        string `json:"url"`
			RawContent string `json:"raw_content"`
		} `json:"results"`
		FailedResults []struct {
			URL   string `json:"url"`
			Error string `json:"error"`
		} `json:"failed_results"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", "", newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	for _, r := range body.Results {
		if strings.TrimSpace(r.RawContent) != "" {
			return "", r.RawContent, nil
		}
	}
	reason := "抽取结果为空"
	if len(body.FailedResults) > 0 && strings.TrimSpace(body.FailedResults[0].Error) != "" {
		reason = truncateRunes(body.FailedResults[0].Error, 160)
	}
	return "", "", newProviderError(p, status, searchErrRejected, reason)
}

// extractViaAnySearch POST /v1/extract：单 URL，正文上限 50000 字符、服务端固定 30 秒。
func (a *App) extractViaAnySearch(ctx context.Context, p activeSearchProvider, pageURL string) (string, string, error) {
	status, raw, err := postProviderJSON(ctx, p, p.base+"/v1/extract", map[string]any{"url": pageURL})
	if err != nil {
		return "", "", err
	}
	if status >= 400 {
		return "", "", newProviderError(p, status, classifyExtractStatus(status),
			providerErrMessage(p.cfg.Kind, status, raw))
	}
	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
		Data    struct {
			Title   string `json:"title"`
			Content string `json:"content"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return "", "", newProviderError(p, status, searchErrParse, "响应无法解析")
	}
	if body.Code != 0 {
		return "", "", newProviderError(p, status, searchErrRejected,
			fmt.Sprintf("业务码 %d: %s", body.Code, truncateRunes(body.Message, 120)))
	}
	if strings.TrimSpace(body.Data.Content) == "" {
		return "", "", newProviderError(p, status, searchErrRejected, "返回的正文为空")
	}
	return body.Data.Title, body.Data.Content, nil
}

// extractFallback 按顺位依次试具备抽取能力的供应商，第一家成功的就用它。
// 返回正文与「经谁抽取」的来源标注；全失败时返回逐家失败原因（给用户看）。
func (a *App) extractFallback(ctx context.Context, pageURL string) (content string, via string, errs []string) {
	ctx, cancel := context.WithTimeout(ctx, extractDefaultBudgetSec*time.Second)
	defer cancel()
	for _, p := range a.extractProviders() {
		if ctx.Err() != nil {
			break
		}
		// 抽取比搜索慢（Tavily 默认 10/30 秒、AnySearch 服务端固定 30 秒）：
		// 没单独配超时的供应商给 20 秒
		per := p.cfg.timeout()
		if p.cfg.TimeoutSec <= 0 {
			per = extractDefaultTimeoutSec * time.Second
		}
		pctx, pcancel := providerCtx(ctx, per)
		_, body, err := a.runProviderExtract(pctx, p, pageURL)
		pcancel()
		if err == nil && strings.TrimSpace(body) != "" {
			return body, p.cfg.displayName(), nil
		}
		if err == nil {
			err = newProviderError(p, 0, searchErrParse, "返回的正文为空")
		}
		errs = append(errs, fmt.Sprintf("%s：%s", p.cfg.displayName(), err.Error()))
	}
	return "", "", errs
}
