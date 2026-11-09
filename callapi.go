package main

// call_api：通用直通工具——「前端给用户什么接口，AI 就拥有相同的接口」。
// 接口目录不是手写清单，而是路由注册的副产品：server.go 的 registerRoutes 用
// a.apiRoute(...) 注册的每条路由同时带一份元数据（说明 / 参数要点 / 最低权限
// 档 / 危险标记）。元数据缺席（Perm 为空）的路由照常对前端开放，但**不进 AI
// 目录**——默认关闭、显式声明才开放，新增路由漏配元数据不会意外暴露给模型。
//
// 执行是**进程内直调**：构造 http.Request（RemoteAddr=回环语义）打到缓存的
// mux 上，与前端走同一个 handler、同一套校验与行为，不开网络、不占端口
// （同 componentbridge.go 组件桥的直调模式）。
//
// 三道闸：
//  1. 目录闸：接口必须在注册表里带元数据（未开放=调不着，报错并指路）；
//  2. 权限闸：元数据 Perm 是该接口要求的最低会话权限档，会话档位低于即拒绝
//     （不静默钳制，与派发工具同一口径）；完整目录（每条标最低权限档与 ⚠）
//     拼在 call_api 工具说明里、静态随请求前缀，不按会话档位过滤——档位
//     差异在调用时由本闸校验并提示需要的档位；
//  3. 危险闸：Danger 非空（删除/清空/切换分支/立即执行类）的接口调用前走与
//     文件越权相同的审批卡（App.requestApproval），用户批准才执行；
//     always=本会话后续免审（与文件授权同一语义）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
)

// apiMeta 是一条路由对 AI 开放所需的元数据（server.go registerRoutes 里随
// apiRoute 一起登记，路由与目录同源同址，不存在第二份清单）。
type apiMeta struct {
	Method  string
	Path    string // 含 {参数} 占位符，目录按注册原文展示
	Summary string // 一句话说明（目录展示）
	Params  string // 参数要点（目录展示）
	Perm    string // 最低权限档：permReadOnly / permWorkspace / permFull；空=不对 AI 开放
	Danger  string // 非空=危险说明，调用前走审批卡
}

// apiRoute 注册一条 HTTP 路由并（当 meta.Perm 非空时）把它收进 AI 可调目录。
// 幂等：同一 pattern 重复注册只保留第一份（Routes() 与 apiMuxOf() 各自触发
// 一轮注册时不会翻倍）。
func (a *App) apiRoute(mux *http.ServeMux, pattern string, h http.HandlerFunc, meta apiMeta) {
	mux.HandleFunc(pattern, h)
	if meta.Perm == "" {
		return
	}
	method, path, ok := strings.Cut(pattern, " ")
	if !ok { // 无方法前缀的老式注册不会走到这：开放目录全部带方法前缀
		return
	}
	meta.Method, meta.Path = method, path
	a.apiMetaMu.Lock()
	defer a.apiMetaMu.Unlock()
	if a.apiMetaByPattern == nil {
		a.apiMetaByPattern = map[string]apiMeta{}
	}
	if _, dup := a.apiMetaByPattern[pattern]; dup {
		return
	}
	a.apiMetaByPattern[pattern] = meta
	a.apiMetas = append(a.apiMetas, meta)
}

// apiMuxOf 返回（并缓存）一份完整注册的 mux，供 call_api 进程内直调。
// 与前端看到的是同一套路由注册（registerRoutes），行为完全一致。
// 注册在锁外进行（apiRoute 各自拿锁登记元数据），缓存写入用双检——并发的
// 首次调用可能各建一份 mux，元数据登记幂等、两份 mux 内容相同，取先到者。
func (a *App) apiMuxOf() *http.ServeMux {
	a.apiMetaMu.Lock()
	cached := a.apiMux
	a.apiMetaMu.Unlock()
	if cached != nil {
		return cached
	}
	mux := http.NewServeMux()
	a.registerRoutes(mux)
	a.apiMetaMu.Lock()
	if a.apiMux == nil {
		a.apiMux = mux
	}
	a.apiMetaMu.Unlock()
	return a.apiMux
}

// apiCatalogText 渲染完整接口目录（拼进 call_api 工具说明）：每条标注最低
// 权限档与 ⚠ 危险标记，**不按会话档位过滤**——目录是安装级稳定事实（接口
// 增删只随版本变化），静态进请求前缀稳定命中 KV 缓存，会话档位差异由
// runCallAPI 的权限闸在调用时校验（拒绝并提示需要的档位）。顺序即注册
// 顺序（server.go registerRoutes 的书写序），稳定可读。首次调用会先确保
// 路由注册完成（懒初始化：测试等没有走过 Routes() 的进程也能拿到完整
// 目录），渲染结果进程内缓存（注册表启动后不再变化）。
func (a *App) apiCatalogText() string {
	a.apiMetaMu.Lock()
	if cached := a.apiCatalogCache; cached != "" {
		a.apiMetaMu.Unlock()
		return cached
	}
	a.apiMetaMu.Unlock()
	a.apiMuxOf()
	a.apiMetaMu.Lock()
	metas := append([]apiMeta(nil), a.apiMetas...)
	a.apiMetaMu.Unlock()
	if len(metas) == 0 {
		return ""
	}
	var b strings.Builder
	for _, m := range metas {
		mark := "  "
		if m.Danger != "" {
			mark = "⚠ "
		}
		line := mark + m.Method + " " + m.Path + " [" + permModeLabel(m.Perm) + "] — " + m.Summary
		if m.Params != "" {
			line += "（" + m.Params + "）"
		}
		b.WriteString(line + "\n")
	}
	out := strings.TrimRight(b.String(), "\n")
	a.apiMetaMu.Lock()
	if a.apiCatalogCache == "" {
		a.apiCatalogCache = out
	}
	a.apiMetaMu.Unlock()
	return out
}

// apiMetaFor 用 mux 的同款匹配规则（含 {参数} 通配与方法优先级）查一条请求
// 对应的元数据：mux.Handler 返回命中的注册 pattern，再查表。匹配不到（404/
// 405/未开放）一律返回零值。
func (a *App) apiMetaFor(r *http.Request) (apiMeta, bool) {
	_, pattern := a.apiMuxOf().Handler(r)
	a.apiMetaMu.Lock()
	meta, ok := a.apiMetaByPattern[pattern]
	a.apiMetaMu.Unlock()
	return meta, ok
}

// callAPIResultMax 是 call_api 返回给模型的正文上限（字符）：接口响应可能很
// 大（会话列表、diff、清单），超限截断并说明——需要更多时用带查询参数的
// 窄化请求或专用工具（读会话历史有 read_session_messages 等）。
const callAPIResultMax = 20000

// callAPITool 构造 call_api 工具（仅 assistant 模式注入，见 tools.go）。完整
// 接口目录拼在工具说明里：工具目录与系统提示词同为请求稳定前缀（对 KV 缓存
// 等价），但目录正是 path/method/params 的取值文档，放在工具自己身上模型
// 选参时零跳转，也不占其他模式的提示词正文。
func (a *App) callAPITool() Tool {
	return Tool{
		Spec: toolSpec("call_api", "调用本产品的一个 HTTP 接口——与前端界面调用的完全相同（进程内直调同一处理器，不走网络）。完整接口目录如下（[档位]=该接口要求的最低会话权限档，本会话档位不足时调用会被拒绝并提示需要的档位；⚠=危险接口，调用前会弹审批卡请用户确认）：\n"+a.apiCatalogText()+"\n\npath 写完整路径（可带查询串，路径参数直接填实际值，如 /api/sessions/abc123/chat）；GET 之外的请求把 JSON 参数放 params。响应原样返回（状态码+正文，正文超长会截断）。适合做没有专用工具的界面级操作（改会话设置、排队管理、回答提问、待办维护、Git 面板只读查询等）。",
			objSchema(map[string]any{
				"path":   strProp("接口路径，以 / 开头，可带查询串；路径参数填实际值，如 /api/sessions/{id}/chat 的 id 换成目标会话 id"),
				"method": strProp("HTTP 方法：GET（默认）/ POST / PATCH / DELETE，与上方目录里标的方法一致"),
				// params 必须声明为 object：声明成 string 时按 schema 行事的模型
				// 会把请求体对象先序列化成字符串再传（双重编码），服务端解码
				// struct 必失败。形状随接口各异，不枚举 properties，开放对象。
				"params": map[string]any{"type": "object", "description": "请求体 JSON 对象本身（不要序列化成字符串；GET 忽略；POST/PATCH/DELETE 按各接口的参数说明填）"},
			}, "path")),
		Run: runCallAPI,
	}
}

// runCallAPI 是 call_api 的执行体：三道闸（目录/权限/危险审批）→ 进程内直调
// mux（与前端同一 handler）→ 状态码与正文回给模型。
func runCallAPI(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Path   string          `json:"path"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	path := strings.TrimSpace(p.Path)
	if path == "" || !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("path 不能为空且必须以 / 开头（如 /api/sessions）")
	}
	method := strings.ToUpper(strings.TrimSpace(p.Method))
	if method == "" {
		method = http.MethodGet
	}
	// 直调的请求地址是「回环假地址」：mux 匹配只看 path；RemoteAddr=回环与真实
	// 前端请求同语义（部分接口仅回环可达，如剪贴板读取）
	req, err := http.NewRequestWithContext(t.ctx, method, "http://127.0.0.1"+path, nil)
	if err != nil {
		return "", fmt.Errorf("请求构造失败：%v", err)
	}
	req.RemoteAddr = "127.0.0.1:0"
	// 调用者身份随请求带上：审批/提问的决议据此标注「经理代选」（mgrnotify.go
	// 的 managerResolveCaller 读它）。这不是鉴权（call_api 已过下面的三道闸），
	// 只是来源标注——前端按钮与远程转发都不带这个头，一律算用户。
	req.Header.Set("X-JX-Caller-Session", t.sess.ID)

	// 闸一：目录（mux 同款匹配含方法校验——方法不对会匹配到 405 的空 pattern）
	meta, ok := t.app.apiMetaFor(req)
	if !ok {
		return "", fmt.Errorf("接口 %s %s 不在可调目录里（未对 AI 开放、方法不对或路径不存在）。可用接口以 call_api 工具说明里的目录为准；没有对应接口的操作请用专用工具完成", method, path)
	}
	// 闸二：权限（会话档位低于接口要求即拒绝，不静默钳制）
	if sandboxPolicyRank(permModeOf(t.sess)) < sandboxPolicyRank(meta.Perm) {
		return "", fmt.Errorf("接口 %s %s 需要权限档「%s」，本会话当前是「%s」（%s）——不能越档调用。如确有需要，请让用户在权限面板调宽本会话权限，或用 request_permission 申请",
			method, meta.Path, permModeLabel(meta.Perm), permModeLabel(permModeOf(t.sess)), permModeOf(t.sess))
	}
	// 闸三：危险接口走审批卡（与文件越权同一张卡；always=本会话后续免审）
	if meta.Danger != "" {
		approved, _, timedOut := t.app.requestApproval(t.ctx, t.sess, "call_api",
			fmt.Sprintf("%s %s", method, path), meta.Danger)
		if !approved {
			if timedOut {
				return "", fmt.Errorf("接口 %s %s 是危险操作（%s），审批等待超时未获用户批准，已取消。需要时请用户直接在界面上操作", method, path, meta.Danger)
			}
			return "", fmt.Errorf("接口 %s %s 是危险操作（%s），用户未批准，已取消", method, path, meta.Danger)
		}
	}
	// 带上请求体（GET/HEAD 不带）。params 声明为 object，但旧 schema 曾声明
	// string、个别模型仍会按字符串习惯把对象序列化后再传（双重编码）——字符串
	// 形态剥掉外壳再用，保证到达处理器的一律是 JSON 对象本体。
	if len(p.Params) > 0 && method != http.MethodGet && method != http.MethodHead {
		body := unwrapJSONString(p.Params)
		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
	}
	// 进程内直调：同一 mux、同一 handler，行为与前端完全一致
	rec := httptest.NewRecorder()
	t.app.apiMuxOf().ServeHTTP(rec, req)
	body := rec.Body.String()
	note := ""
	if truncated, clipped := clipBody(body, callAPIResultMax); clipped {
		body, note = truncated, fmt.Sprintf("\n\n（响应共超出上限，此处显示前 %d 字；需要剩余部分请用更窄的查询参数或专用工具）", callAPIResultMax)
	}
	return fmt.Sprintf("HTTP %d %s\n%s%s", rec.Code, http.StatusText(rec.Code), body, note), nil
}

// unwrapJSONString 把字符串形态的 params 剥成它包着的 JSON 文本：模型按旧
// schema（params 声明为 string）或字符串习惯传参时，请求体对象会先被序列化
// 成字符串（如 "{\"content\":…}"），不剥的话处理器拿到的是 JSON 字符串而非
// 对象，解码 struct 必失败（"cannot unmarshal string into Go value of type
// struct"）。只剥字符串外壳（最多三层，防病态多重编码），对象/数组等其余
// 形态原样通过；剥出的内容不是 JSON 对象/数组/字符串开头时也原样通过（如
// 普通文本，交给接口按自己的校验报错）。
func unwrapJSONString(raw json.RawMessage) json.RawMessage {
	for i := 0; i < 3; i++ {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			break
		}
		t := strings.TrimSpace(s)
		if t == "" || (t[0] != '{' && t[0] != '[' && t[0] != '"') {
			break
		}
		raw = json.RawMessage(t)
	}
	return raw
}

// clipBody 把响应正文截到 limit 个字符（rune 计数，中文安全），返回截断后的
// 正文与是否发生了截断。
func clipBody(s string, limit int) (string, bool) {
	runes := []rune(s)
	if len(runes) <= limit {
		return s, false
	}
	return string(runes[:limit]), true
}
