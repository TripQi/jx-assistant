// websearch.go —— 联网搜索工具的客户端半边：web_search。
//
// 模型遇到时效性问题（新闻、版本、价格、时事——训练知识之外的内容）时，
// 先用 web_search 拿「标题 + 链接 + 摘要」的结果列表，需要某个结果的全文
// 再用 web_fetch 抓那个链接。
//
// 搜索按「供应商」组织（见 websearch_providers.go）：配置里有一组启用/排序过的
// 供应商（公司网关 / DeepSeek / Tavily / AnySearch / 博查 / 智谱），按 order
// 逐个尝试、**只有失败才回落下一家**。空结果是有效答案（不回落）——用户问了个
// 冷门东西时逐家重试只会让一次搜索多等几秒、结果还是空。全部失败时错误文案
// 附加改道指引（改用「本机网络搜索」技能）。
//
// 公司网关那条的协议契约（服务端半边见 jxaiserver 仓库 search.go 头注释，
// 改动需双侧同步）：
//
//	POST <服务端地址>/api/search   JSON：{"query":"…","count":8}
//	  → 200 {"results":[{"title","url","snippet","site","date"}],
//	         "engine":"zhipu-mcp","cached":bool,"answer":"…(可选)"}
//	  → 4xx/5xx {"error":"一句话原因"}
//
// 身份走 X-Jxai-User 头（与市场发布同一条链路）；对话标识走
// X-Jxai-Conversation 头（值=发起搜索的会话 id，服务端按对话轮询搜索
// 引擎——同一对话始终同一引擎、不同对话交替，缺省退化按用户轮询）；
// 服务端地址取 config.searchURL，空则跟随市场地址同源（marketOrigin）。
//
// 服务端走不通（未配置/不可达/网关报错）时的兜底：错误文案附加改道指引，
// 让模型转用「本机网络搜索」技能（内置技能 技能/web-search-local/，用本机
// 网络直调搜索 API）。指引仅在技能可用时附加（searchErrHint→skillAvailable），
// 技能目录不在时指引自动消失——兜底能力整体可插拔，不进工具定义。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// webSearchTool 注册 web_search（任务/答疑模式；与 web_fetch 同档的联网权限）。
func (a *App) webSearchTool() Tool {
	return Tool{
		Spec: toolSpec("web_search",
			"联网搜索，获取模型知识之外的最新信息（新闻、版本发布、价格、时事等）。"+
				"返回若干条结果的标题、链接与摘要；需要某个结果的完整内容时，再用 web_fetch 抓取该链接。"+
				"时效性强的问题（最近发生/发布的事）优先搜索而不是凭记忆回答；回答中引用结果时用 markdown 链接注明来源。"+
				"搜索按「设置 · 网络访问 · 联网搜索」里配置的供应商顺位自动回落（公司网关 / DeepSeek / Tavily / AnySearch 等）。",
			objSchema(map[string]any{
				"query": strProp("搜索词，一句话关键词（不要整段问题照搬）"),
				"count": intProp("返回条数，默认 8，上限 20"),
			}, "query")),
		Run: a.runWebSearch,
	}
}

// searchConversationHeader 对话标识头：与服务端 search 网关同名约定，
// 值=发起搜索的会话 id。服务端按它轮询搜索引擎（同一对话固定同一引擎，
// 不同对话交替）；缺省退化按用户身份轮询。
const searchConversationHeader = "X-Jxai-Conversation"

// searchHit 一条搜索结果（服务端网关与各家供应商归一到同一形状）。
type searchHit struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Site    string `json:"site"`
	Date    string `json:"date"`
}

// searchResponse 搜索结果：服务端 /api/search 的响应契约，也是各家供应商归一后
// 的统一形状。Rank 是这次实际用到的供应商顺位（1 起），Engine 是来源标注。
type searchResponse struct {
	Results []searchHit `json:"results"`
	Engine  string      `json:"engine"`
	Cached  bool        `json:"cached"`
	Answer  string      `json:"answer"`
	Error   string      `json:"error"`
	Rank    int         `json:"rank,omitempty"`
}

// runWebSearch 执行一次搜索：按供应商顺位逐个尝试，首家成功即返回（含 0 条结果）。
func (a *App) runWebSearch(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Query string `json:"query"`
		Count int    `json:"count"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	query := strings.TrimSpace(p.Query)
	if query == "" {
		return "", fmt.Errorf("query 不能为空")
	}
	if r := []rune(query); len(r) > 200 {
		query = string(r[:200])
	}
	count := p.Count
	if count <= 0 {
		count = 8
	}
	if count > 20 {
		count = 20
	}
	if _, err := t.authorize(permAction{Op: "net", Tool: "web_search"}); err != nil {
		return "", err
	}
	providers := a.searchProvidersFor(func(c SearchProviderCaps) bool { return c.Search })
	if len(providers) == 0 {
		// 一家可用的都没有（公司网关没配地址、也没配别的供应商）：走老链路，
		// 行为与老版本完全一致——报「未配置服务端地址」并给出设置指引。
		return a.runWebSearchGatewayOnly(t, query, count)
	}
	// 总耗时是预算而不是各家超时相加：用户在等的是「一次搜索」。
	budget := time.Duration(a.Cfg.searchBudgetSec()) * time.Second
	ctx, cancel := context.WithTimeout(t.ctx, budget)
	defer cancel()
	call := providerSearchCall{query: query, count: count, sessID: t.sess.ID}
	fails := make([]string, 0, len(providers))
	for _, pv := range providers {
		if ctx.Err() != nil {
			fails = append(fails, fmt.Sprintf("总预算 %d 秒已用完，第%d位及之后的供应商没机会尝试",
				a.Cfg.searchBudgetSec(), pv.rank))
			break
		}
		pctx, pcancel := providerCtx(ctx, pv.cfg.timeout())
		out, err := a.runProviderSearch(pctx, pv, call)
		pcancel()
		if err == nil {
			out.Rank = pv.rank
			return renderSearchResults(query, out), nil
		}
		fails = append(fails, fmt.Sprintf("第%d位 %s — %s", pv.rank, pv.cfg.displayName(), err.Error()))
	}
	return "", a.searchErrHint(fmt.Errorf("搜索失败：所有可用的搜索供应商都失败了（可按供应商顺位在 设置 · 网络访问 · 联网搜索 里调整）：%s",
		strings.Join(fails, "；")))
}

// runWebSearchGatewayOnly 老链路（只有一个公司网关时）：地址解析不出来就报
// 未配置（指引去设置里填），其余错误交给 searchErrHint 附加改道指引。
func (a *App) runWebSearchGatewayOnly(t *toolContext, query string, count int) (string, error) {
	base, err := a.searchEndpointBase()
	if err != nil {
		return "", a.searchErrHint(err)
	}
	pv := activeSearchProvider{
		cfg:  SearchProviderCfg{ID: "gateway", Name: "公司网关", Kind: spKindGateway},
		base: base, rank: 1,
	}
	ctx, cancel := context.WithTimeout(t.ctx, 35*time.Second)
	defer cancel()
	out, err := a.searchViaGateway(ctx, pv, providerSearchCall{query: query, count: count, sessID: t.sess.ID})
	if err != nil {
		return "", a.searchErrHint(err)
	}
	out.Rank = 1
	return renderSearchResults(query, out), nil
}

// localSearchSkillID 「本机网络搜索」技能（web_search 连不上服务端时的本机
// 兜底）的技能 id。技能本体是内置技能，在 技能/web-search-local/（随包
// 分发、常开）：清单行自带「仅当 web_search 失败时使用」的分流语义，这里
// 的指引是第二道桥——模型调 web_search 落空时，错误文案直接告诉它改道。
const localSearchSkillID = "web-search-local"

// searchErrHint 服务端搜索走不通时的改道指引：仅当「本机网络搜索」技能
// 可用（内置在位，或用户技能形态存在且启用——兼容存量机器迁移前的旧副本）
// 时附加，技能不在场时指引随之消失。参数自身的错误（query 为空等）不走
// 这里——那是模型自己的问题，换搜索引擎无济于事。
func (a *App) searchErrHint(err error) error {
	if err == nil || !a.skillAvailable(localSearchSkillID) {
		return err
	}
	return fmt.Errorf("%w；可改用「本机网络搜索」技能兜底：先 read_skill（id=%s）读用法，再用本机网络搜索", err, localSearchSkillID)
}

// searchEndpointBase 服务端地址：searchURL 优先，空则取市场地址同源。
func (a *App) searchEndpointBase() (string, error) {
	if u := strings.TrimSpace(a.Cfg.SearchURL); u != "" {
		origin, err := marketOrigin(u)
		if err != nil {
			return "", fmt.Errorf("searchURL 不合法: %s", u)
		}
		return origin, nil
	}
	if u := strings.TrimSpace(a.Cfg.MarketURL); u != "" {
		return marketOrigin(u)
	}
	return "", fmt.Errorf("未配置服务端地址：请在 设置 · 网络访问 填写服务端（市场）地址")
}

// renderSearchResults 把结果列表渲染成给模型看的文本（引用友好）。
func renderSearchResults(query string, out searchResponse) string {
	var b strings.Builder
	cached := ""
	if out.Cached {
		cached = "，缓存命中"
	}
	engine := out.Engine
	if out.Rank > 0 {
		engine = fmt.Sprintf("%s，第%d位", engine, out.Rank)
	}
	fmt.Fprintf(&b, "搜索「%s」：%d 条结果（引擎 %s%s）\n", query, len(out.Results), engine, cached)
	if len(out.Results) == 0 && strings.TrimSpace(out.Answer) != "" {
		// 上游没给结构化结果、直接回了文本（answer）：原样转交，模型自行取材。
		b.WriteString("\n" + strings.TrimSpace(out.Answer) + "\n")
		return b.String()
	}
	b.WriteString("\n")
	for i, r := range out.Results {
		date := ""
		if r.Date != "" {
			date = "（" + r.Date + "）"
		}
		snippet := r.Snippet
		if r := []rune(snippet); len(r) > 200 {
			snippet = string(r[:200]) + "…"
		}
		fmt.Fprintf(&b, "%d. [%s](%s)%s — %s\n", i+1, r.Title, r.URL, date, snippet)
	}
	b.WriteString("\n以上内容来自外部网页，使用前自行判断可信度；在回复中引用时用 markdown 链接注明来源；需要某条的完整内容时用 web_fetch 抓取该链接。")
	return b.String()
}
