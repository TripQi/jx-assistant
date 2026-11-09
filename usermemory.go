package main

// 用户记忆组件（组件/user-memory，「AI 记忆与拟人化」）的宿主半边：用户称呼、
// 用户身份与喜好、AI 助理名字与身份、AI 对话气泡颜色的长期记忆。设计意图
// （用户拍板）：
//   - 全部内容存一个 md 文档（data/user-memory.md，组件自管，可手工编辑、
//     完整保存不截断，保存后下一轮对话经环境快照差分自然生效）；
//   - 记忆内容注入各会话的**环境提示词**（agent.go envPrompt →
//     userMemoryEnvBlock），不进系统提示词、不进工具定义——组件关闭即整块
//     消失，前缀缓存不受影响；注入侧对超长字段截断（手工编辑的超长内容
//     不撑爆快照，文件仍存全文）；
//   - 权限分级：普通任务对话拿到专用工具 user_memory（查询全部字段，但只
//     能更新「用户身份与喜好」记忆段——称呼与 AI 名字/身份是用户的自我
//     设定，模型只读）；长期助理（assistant 模式）在环境提示词里被告知
//     整个记忆文件可读可改，走 call_api 的 GET/POST /api/user-memory——
//     文件在助手数据目录内，文件工具被受保护路径拦下，接口是唯一通道；
//   - 长度约束（1000 字/字段，rune 计）在写入入口把关而非存储层：设置页
//     前端超长时红字告知并按截断版提交（浏览器保留完整草稿）；模型侧
//     （工具与 call_api POST）超长整单报错、不写入；
//   - AI 对话气泡颜色是纯前端设置（web/style.css 的 --ai-bubble 变量，
//     组件 client.js 注入，本地生效即可），不进任何提示词。

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"
)

const userMemoryComponentID = "user-memory"

// 各文本字段上限（rune 计，中文友好）：保存时统一硬截断（前端 input 也限，
// 但后端是权威——自查项「1000 字截断」以这里为准）。
const userMemoryFieldLimit = 1000

// UserMemoryData 是记忆文档的全部字段。
type UserMemoryData struct {
	UserName    string `json:"userName"`    // 用户称呼
	Memory      string `json:"userMemory"`  // 用户身份与喜好（记忆段——AI 可经 user_memory 工具更新）
	AIName      string `json:"aiName"`      // AI 助理名字
	AIIdentity  string `json:"aiIdentity"`  // AI 助理身份
	BubbleColor string `json:"bubbleColor"` // AI 对话气泡颜色（#rrggbb 或空=默认灰，前端展示用）
}

// md 文档的段标题（解析与渲染同一份，顺序即文档顺序）。
var userMemorySectionTitles = []struct {
	field string // 字段名（仅用于注释可读性，解析按下标对应）
	title string
}{
	{"userName", "## 用户称呼"},
	{"userMemory", "## 用户身份与喜好"},
	{"aiName", "## AI 助理名字"},
	{"aiIdentity", "## AI 助理身份"},
	{"bubbleColor", "## AI 对话框颜色"},
}

const userMemoryDocHeader = `# 用户记忆

> 本文件由「AI 记忆与拟人化」组件管理（设置 → AI 记忆与拟人化）；也可手工编辑，保存后下一轮对话自动生效。
> 写入入口有 1000 字/字段的上限（设置页超长保存按截断版入库，AI 调用超长直接报错）；手工编辑不受此限，文件内容完整保存。
> 段标题与顺序请保持不变，便于程序解析。
`

// userMemoryEnabled 组件门控（todoEnabled 同款）：组件缺失或关闭视为未启用。
func (a *App) userMemoryEnabled() bool {
	if a.Components == nil {
		return false
	}
	if _, ok := a.Components.Manifest(userMemoryComponentID); !ok {
		return false
	}
	return a.Components.IsEnabled(userMemoryComponentID)
}

// ---------- 文档读写 ----------

var userMemoryMu sync.Mutex // 读改写全走这里：并发保存（设置页/工具/经理）不互相踩

func (a *App) userMemoryPath() string {
	return filepath.Join(a.Cfg.DataDir, "user-memory.md")
}

// loadUserMemory 读记忆文档；文件不存在或段缺失按空字段处理（损坏不抛错——
// 空字段顶上，原文档还在磁盘上没被动过）。
func (a *App) loadUserMemory() UserMemoryData {
	data, err := os.ReadFile(a.userMemoryPath())
	if err != nil {
		return UserMemoryData{}
	}
	return parseUserMemoryDoc(string(data))
}

// userMemorySessionView 某会话视角的记忆值（会话级合并，参照 sessioncx.go 的
// 会话视角配置合并层）：总开关 CfgScope 开且 user-memory 未标跟随全局时，
// 四字段+颜色逐项「会话覆盖 > 全局值」；second 返回值＝是否走了会话合并。
// 会话覆盖的键＝API 字段名（userName/userMemory/aiName/aiIdentity/bubbleColor），
// 值＝字段字符串——Cfg 的存储哲学本就是「组件配置的原始 KV」，这里对
// user-memory 而言原始形态就是字段串。覆盖没提的键自动回落全局值。
func (a *App) userMemorySessionView(sess *SessionMeta) (UserMemoryData, bool) {
	d := a.loadUserMemory()
	if sess == nil || !a.cxoComponentCfgOn(sess.ID, userMemoryComponentID) {
		return d, false
	}
	o := a.SessionCX.of(sess.ID)
	if o == nil {
		return d, false
	}
	cfg := o.Cfg[userMemoryComponentID]
	if cfg == nil {
		return d, true // 独立但还没有任何覆盖：值全来自全局
	}
	if v, ok := cfg["userName"]; ok {
		d.UserName = v
	}
	if v, ok := cfg["userMemory"]; ok {
		d.Memory = v
	}
	if v, ok := cfg["aiName"]; ok {
		d.AIName = v
	}
	if v, ok := cfg["aiIdentity"]; ok {
		d.AIIdentity = v
	}
	if v, ok := cfg["bubbleColor"]; ok {
		d.BubbleColor = v
	}
	return d, true
}

// userMemorySessionEnabled 某会话里 user-memory 是否生效：会话开关覆盖优先于
// 全局开关（cxoComponentEnabled；sess 无 ID / 注册表未初始化时回落全局）。
// 环境注入与工具调用时核的口径；工具**目录**的挂载仍按全局开关（工具目录
// 未会话化是既有约定，见 AGENT.md 组件章节），关掉只影响注入与调用。
func (a *App) userMemorySessionEnabled(sess *SessionMeta) bool {
	if sess == nil || sess.ID == "" {
		return a.userMemoryEnabled()
	}
	return a.cxoComponentEnabled(sess.ID, userMemoryComponentID)
}

// saveUserMemorySession 会话独立的写入路径（POST ?s= 带会话时的落点）：
// 逐字段写覆盖，与全局值相同的键删记录（sessioncx 的「只记显式覆盖」哲学，
// 恢复跟随不需要清理入口）；该组件的覆盖全空时整段摘除。
func (a *App) saveUserMemorySession(sid string, d UserMemoryData) {
	global := a.loadUserMemory()
	a.SessionCX.set(a.Store, sid, func(o *SessionOverrides) {
		if o.Cfg == nil {
			o.Cfg = map[string]map[string]string{}
		}
		inner := o.Cfg[userMemoryComponentID]
		if inner == nil {
			inner = map[string]string{}
		}
		vals := map[string]string{
			"userName": d.UserName, "userMemory": d.Memory, "aiName": d.AIName,
			"aiIdentity": d.AIIdentity, "bubbleColor": strings.TrimSpace(d.BubbleColor),
		}
		globals := map[string]string{
			"userName": global.UserName, "userMemory": global.Memory, "aiName": global.AIName,
			"aiIdentity": global.AIIdentity, "bubbleColor": strings.TrimSpace(global.BubbleColor),
		}
		for k, v := range vals {
			if v == globals[k] {
				delete(inner, k) // 与全局一致＝跟随，不留记录
			} else {
				inner[k] = v
			}
		}
		if len(inner) == 0 {
			delete(o.Cfg, userMemoryComponentID)
		} else {
			o.Cfg[userMemoryComponentID] = inner
		}
	})
}

// saveUserMemory 整份重写记忆文档。**不做截断**（用户拍板：md 文件存完整
// 内容——手工编辑可以写任意长），长度约束在两个写入入口把关：设置页前端
// 截断后提交、模型侧超限直接报错（handleUserMemorySet / userMemoryTool）。
func (a *App) saveUserMemory(d UserMemoryData) (UserMemoryData, error) {
	err := writeFileAtomic(a.userMemoryPath(), []byte(renderUserMemoryDoc(d)))
	return d, err
}

// overLimitFields 返回超长字段的中文名（rune 计）。设置页前端提交前已截断，
// 走到这里的是模型侧（call_api POST / user_memory 工具）——超限一律整单拒绝，
// 不静默截断（模型必须知道写入失败，改短再试）。
func (d UserMemoryData) overLimitFields() []string {
	var over []string
	for name, s := range map[string]string{
		"userName": d.UserName, "userMemory": d.Memory,
		"aiName": d.AIName, "aiIdentity": d.AIIdentity,
	} {
		if utf8.RuneCountInString(s) > userMemoryFieldLimit {
			over = append(over, name)
		}
	}
	sort.Strings(over)
	return over
}

// parseUserMemoryDoc 解析 md 文档：按「## 标题」分段，段内容为其后连续正文
// （跳过空行与引用行）。未知段落忽略——手工添加的额外内容不参与解析，
// 但会在下次程序化保存时被重建的文档覆盖。
func parseUserMemoryDoc(text string) UserMemoryData {
	var d UserMemoryData
	fields := []*string{&d.UserName, &d.Memory, &d.AIName, &d.AIIdentity, &d.BubbleColor}
	// 标题 → 字段指针的索引
	idx := map[string]int{}
	for i, s := range userMemorySectionTitles {
		idx[s.title] = i
	}
	cur := -1
	var buf []string
	flush := func() {
		if cur >= 0 {
			*fields[cur] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}
	for _, line := range strings.Split(text, "\n") {
		t := strings.TrimRight(line, "\r")
		if strings.HasPrefix(t, "## ") {
			flush()
			if i, ok := idx[strings.TrimSpace(t)]; ok {
				cur = i
			} else {
				cur = -1
			}
			continue
		}
		if cur < 0 {
			continue // 文件头与引用说明行
		}
		if strings.HasPrefix(t, ">") { // 段内的引用/注释行不算正文
			continue
		}
		buf = append(buf, t)
	}
	flush()
	return d
}

// renderUserMemoryDoc 渲染 md 文档（固定段序；空段保留标题，用户填了就有）。
func renderUserMemoryDoc(d UserMemoryData) string {
	var b strings.Builder
	b.WriteString(userMemoryDocHeader)
	vals := []string{d.UserName, d.Memory, d.AIName, d.AIIdentity, d.BubbleColor}
	for i, s := range userMemorySectionTitles {
		b.WriteString("\n" + s.title + "\n\n")
		if v := strings.TrimSpace(vals[i]); v != "" {
			b.WriteString(v + "\n")
		}
	}
	return b.String()
}

// ---------- 环境提示词块（agent.go envPrompt 调用；不进系统提示词/工具定义） ----------

// userMemoryEnvBlock 渲染注入各会话环境快照的记忆块：组件开启且任一文本字段
// 有内容时出现；内容变化（设置页/工具/经理改动）经环境快照差分送达全部会话。
// 颜色是前端设置不进提示词。多行字段压成单行（快照条目行语义，差分按行精确）；
// md 文件存完整内容（写入入口把关长度），注入侧对超长字段截断到 1000 字加
// 尾注——手工编辑写了超长内容也不会撑爆环境快照。
func (a *App) userMemoryEnvBlock(sess *SessionMeta) string {
	// 会话视角：开关被本会话覆盖关闭 → 整块不注入（与开关覆盖语义一致）；
	// 值按「会话覆盖 > 全局值」合并（CfgScope 开且未标跟随时）。envPrompt 每
	// 轮现算（无缓存），覆盖变化经环境快照差分自然送达该会话。
	if !a.userMemorySessionEnabled(sess) {
		return ""
	}
	d, sessionScoped := a.userMemorySessionView(sess)
	// 四行措辞是用户拍板的最终版（第二人称视角），照抄不润色；冒号后的
	// 括号说明随标签走。存储侧（md 段标题/字段名）是另一套稳定口径，不动。
	var b strings.Builder
	if v := envLine(d.UserName); v != "" {
		b.WriteString("- 用户的名字：" + v + "\n")
	}
	if v := envLine(d.Memory); v != "" {
		b.WriteString("- 用户身份与习惯、喜好：" + v + "\n")
	}
	if v := envLine(d.AIName); v != "" {
		b.WriteString("- 你的名字（用户会用这个名字叫你，你也可以用这个名字自称）：" + v + "\n")
	}
	if v := envLine(d.AIIdentity); v != "" {
		b.WriteString("- 用户定义的你的身份：" + v + "\n")
	}
	if b.Len() == 0 {
		return "" // 四个字段全空：整块不出现
	}
	switch sess.Mode {
	case "assistant":
		b.WriteString("- 记忆维护（长期助理专属）：整个记忆文件可读可改——call_api 调 GET /api/user-memory 读取、POST /api/user-memory 按字段保存（userName/userMemory/aiName/aiIdentity/bubbleColor，各上限 1000 字，超长会整单报错、不会静默截断；未给出的字段保持不变）" + userMemoryScopeNote(sessionScoped) + "。文件位于 " + filepath.ToSlash(a.userMemoryPath()) + "（助手数据目录内，文件工具被受保护路径拦截，读写一律走 call_api）。改动自下一轮起注入全部会话的环境提示词，请保持字段本意、谨慎修改\n")
	case "aux":
		b.WriteString("- 记忆维护：以上内容由用户在 设置 → AI 记忆与拟人化 维护（本会话为只读答疑，没有修改工具）\n")
	default:
		b.WriteString("- 记忆维护：user_memory 工具可查询并更新「用户身份与喜好」段（对应上面「用户身份与习惯、喜好」一行；了解到用户新的身份/偏好/习惯/项目背景时主动更新；整体替换式写入，先查再改；单次写入上限 1000 字，超长会报错，需精简后重试" + userMemoryScopeNote(sessionScoped) + "）；用户的名字与你的名字/身份由用户在 设置 → AI 记忆与拟人化 维护，不要改动\n")
	}
	return "# 用户记忆（用户与助手的长期设定，全部会话生效）\n" + b.String()
}

// userMemoryScopeNote 会话独立人格时的附加说明（给模型交代读写的落点差异）：
// 独立——本工具/接口的改动只影响本对话；跟随（默认）——影响全部会话。
func userMemoryScopeNote(sessionScoped bool) string {
	if !sessionScoped {
		return ""
	}
	return "；本对话启用了独立的记忆设定，这里查到与改动的内容只影响本对话（全局记忆不受影响）"
}

// envLine 字段单行化 + 注入侧截断：oneLine 后超 1000 字取前 1000 字加尾注
// （文件仍存完整内容，这里只管快照不膨胀）。
func envLine(s string) string {
	v := oneLine(s)
	if utf8.RuneCountInString(v) <= userMemoryFieldLimit {
		return v
	}
	return string([]rune(v)[:userMemoryFieldLimit]) + "…（超长，已截断）"
}

// oneLine 把多行字段压成单行（换行→全角分号），保住快照条目行的差分语义。
func oneLine(s string) string {
	return strings.TrimSpace(strings.NewReplacer("\r\n", "；", "\n", "；", "\r", "").Replace(s))
}

// ---------- HTTP（前端设置页 + 长期助理 call_api 共用） ----------

// userMemoryReqSession 请求的会话语境：?s=<会话id> 带 s 时返回（会话视角的
// 读写）；不带 s＝全局（设置浮窗、经理 call_api）。空 ID 安全回落全局。
func userMemoryReqSession(r *http.Request) *SessionMeta {
	sid := strings.TrimSpace(r.URL.Query().Get("s"))
	if sid == "" {
		return nil
	}
	return &SessionMeta{ID: sid}
}

// handleUserMemoryGet：不带 s＝全局文档；带 s＝该会话视角（CfgScope 开且
// 未标跟随时逐项「覆盖 > 全局」，session 字段标注是否走了会话合并——前端
// 据此提示「本对话使用独立设定」）。门控按「活着」口径（全局开或任何会话
// 要它，与组件资产端点同款——会话覆盖打开的对话里组件活着、请求应放行）。
func (a *App) handleUserMemoryGet(w http.ResponseWriter, r *http.Request) {
	if !a.cxoComponentLive(userMemoryComponentID) {
		writeErr(w, 404, "「AI 记忆与拟人化」组件未启用")
		return
	}
	userMemoryMu.Lock()
	d, sessionScoped := a.userMemorySessionView(userMemoryReqSession(r))
	userMemoryMu.Unlock()
	writeJSON(w, 200, map[string]any{
		"userName": d.UserName, "userMemory": d.Memory,
		"aiName": d.AIName, "aiIdentity": d.AIIdentity, "bubbleColor": d.BubbleColor,
		"session": sessionScoped,
		"path":    filepath.ToSlash(a.userMemoryPath()),
	})
}

// handleUserMemorySet 按字段保存：body 里出现的字段替换、未出现的保持不变
// （设置页全量提交——前端已截断；经理可只改一个字段）。**超长整单拒绝**
// （各字段上限 1000 字，rune 计）——存储不截断，模型侧必须拿到明确错误。
// 带 ?s=<会话id> 且该会话组件配置独立（CfgScope 开、未标跟随）时＝会话覆盖
// 路径：逐字段写覆盖、全局文档不动（「一个对话一套人格」的写入口）；
// 其余（不带 s、或该会话未独立）＝全局文档。门控同 GET 的「活着」口径。
func (a *App) handleUserMemorySet(w http.ResponseWriter, r *http.Request) {
	if !a.cxoComponentLive(userMemoryComponentID) {
		writeErr(w, 404, "「AI 记忆与拟人化」组件未启用")
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体应为 JSON 对象")
		return
	}
	str := func(k string) (string, bool) {
		v, ok := body[k]
		if !ok {
			return "", false
		}
		s, _ := v.(string)
		return s, true
	}
	sess := userMemoryReqSession(r)
	userMemoryMu.Lock()
	defer userMemoryMu.Unlock()
	// 字段基线：会话独立时以「该会话视角的现值」为基线（未提交字段保持
	// 会话视角不变＝覆盖不动或全局值兜底），全局时以全局文档为基线。
	d, sessionScoped := a.userMemorySessionView(sess)
	for k, p := range map[string]*string{
		"userName": &d.UserName, "userMemory": &d.Memory,
		"aiName": &d.AIName, "aiIdentity": &d.AIIdentity, "bubbleColor": &d.BubbleColor,
	} {
		if v, ok := str(k); ok {
			*p = v
		}
	}
	if over := d.overLimitFields(); len(over) > 0 {
		writeErr(w, 400, fmt.Sprintf("字段 %s 超过 %d 字上限（rune 计），未写入任何内容；请精简后重试", strings.Join(over, "、"), userMemoryFieldLimit))
		return
	}
	if sessionScoped {
		a.saveUserMemorySession(sess.ID, d)
		view, _ := a.userMemorySessionView(sess)
		writeJSON(w, 200, map[string]any{
			"ok": 1, "session": true,
			"userName": view.UserName, "userMemory": view.Memory,
			"aiName": view.AIName, "aiIdentity": view.AIIdentity, "bubbleColor": view.BubbleColor,
			"path": filepath.ToSlash(a.userMemoryPath()),
		})
		return
	}
	saved, err := a.saveUserMemory(d)
	if err != nil {
		writeErr(w, 500, "保存失败："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": 1, "session": false,
		"userName": saved.UserName, "userMemory": saved.Memory,
		"aiName": saved.AIName, "aiIdentity": saved.AIIdentity, "bubbleColor": saved.BubbleColor,
		"path": filepath.ToSlash(a.userMemoryPath()),
	})
}

// ---------- 专用工具（普通任务对话：查询全部字段 + 只更新记忆段） ----------

// userMemoryTools 组件 user-memory 启用时返回 user_memory 工具（todoTools 同款
// 组件门控）。挂各执行模式：aux 只读答疑不挂；assistant 不挂（经理的通道是
// call_api 整文件读写，环境提示词里有说明——权限分级，工具是「弱通道」）。
func (a *App) userMemoryTools() []Tool {
	if !a.userMemoryEnabled() {
		return nil
	}
	return []Tool{a.userMemoryTool()}
}

func (a *App) userMemoryTool() Tool {
	return Tool{
		Spec: toolSpec("user_memory", "查询/更新用户记忆（用户的身份、相关信息与喜好的长期记忆，全部会话共享、每轮注入环境提示词；若本对话启用了独立记忆设定，则查询与更新只影响本对话）。不带参数 = 查询当前全部字段；带 memory 参数 = 把「用户身份与喜好」记忆段（注入为「用户身份与习惯、喜好」一行）整体替换为给定内容（先查询、在结果基础上改写，避免丢掉已有记忆；上限 1000 字，超长会报错、本次不写入，需精简后重试）。其余字段（用户的名字、你的名字与身份）是用户的自我设定，本工具只读不写。当你在对话中了解到值得长期记住的用户信息（身份、偏好、习惯、项目背景等）时，主动调用本工具更新记忆。",
			objSchema(map[string]any{
				"memory": strProp("新的「用户身份与喜好」记忆段全文（整体替换式写入，上限 1000 字）；不传 = 仅查询"),
			})),
		Run: func(t *toolContext, args json.RawMessage) (string, error) {
			// 组件可能被会话中途关掉（工具目录在本轮开轮时定型）——调用时再核
			// 一次，按**本会话**视角（会话开关覆盖优先于全局）
			if !t.app.userMemorySessionEnabled(t.sess) {
				return "", fmt.Errorf("「AI 记忆与拟人化」组件在本会话未启用，user_memory 不可用；请到 设置 → 内置组件 或 加号菜单「本对话的组件与插件」打开")
			}
			var p struct {
				Memory *string `json:"memory"` // 指针区分「未传=查询」与「传空串=清空」
			}
			if len(args) > 0 {
				if err := json.Unmarshal(args, &p); err != nil {
					return "", err
				}
			}
			userMemoryMu.Lock()
			defer userMemoryMu.Unlock()
			// 会话视角读写：本对话启用独立记忆时，查询与更新都落在会话覆盖
			// （全局文档不动）；否则全局。「一个对话一套人格」的模型侧入口。
			d, sessionScoped := t.app.userMemorySessionView(t.sess)
			if p.Memory == nil {
				out, _ := json.Marshal(map[string]string{
					"userName": d.UserName, "userMemory": d.Memory,
					"aiName": d.AIName, "aiIdentity": d.AIIdentity,
				})
				if sessionScoped {
					return string(out) + "\n（本对话使用独立的记忆设定，改动只影响本对话）", nil
				}
				return string(out), nil
			}
			// 超长整单拒绝（用户拍板：写入入口报错，不静默截断、不写入）
			if n := utf8.RuneCountInString(*p.Memory); n > userMemoryFieldLimit {
				return "", fmt.Errorf("记忆段内容 %d 字超过 %d 字上限，本次未写入；请精简后重试", n, userMemoryFieldLimit)
			}
			d.Memory = *p.Memory
			if sessionScoped {
				t.app.saveUserMemorySession(t.sess.ID, d)
				view, _ := t.app.userMemorySessionView(t.sess)
				return "已更新「用户身份与喜好」记忆段（本对话独立记忆）。当前内容：\n" + view.Memory, nil
			}
			saved, err := t.app.saveUserMemory(d)
			if err != nil {
				return "", err
			}
			return "已更新「用户身份与喜好」记忆段。当前内容：\n" + saved.Memory, nil
		},
	}
}
