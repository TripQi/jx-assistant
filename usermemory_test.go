package main

// 用户记忆组件（usermemory.go）的自检测试：
//   - md 文档解析/渲染往返（含手工编辑容错）
//   - 长度策略（用户拍板）：存储不截断（md 存全文）、模型侧入口（POST/工具）
//     超长整单报错不写入、注入侧对超长字段截断防快照膨胀
//   - 组件开关：envPrompt 注入生效/关闭即移除、工具挂载/卸载
//   - 专用工具只动「用户身份与喜好」记忆段（称呼/AI 名字/身份不被改）
//   - 经理整文件通道：GET/POST /api/user-memory 在 call_api 目录里、
//     按字段保存语义正确、组件关闭时 404

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"
)

// newUserMemoryTestApp 建带临时库的 App，并挂上真实组件清单（把仓库里的
// user-memory manifest 复制进临时组件目录——todo_done_test 同款手法）。
// 带真实 Store 与 SessionCX（会话级合并测试用），并预建 A/B 两个会话。
func newUserMemoryTestApp(t *testing.T) (*App, *SessionMeta, *SessionMeta) {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	store, err := NewStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	a.Store = store
	compDir := t.TempDir()
	src, err := os.ReadFile(filepath.Join("组件", userMemoryComponentID, "manifest.json"))
	if err != nil {
		t.Fatalf("读组件清单失败: %v", err)
	}
	dst := filepath.Join(compDir, userMemoryComponentID, "manifest.json")
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, src, 0644); err != nil {
		t.Fatal(err)
	}
	a.Components = InitComponents(compDir, filepath.Join(t.TempDir(), "components.json"))
	a.SessionCX = loadSessionCX(store)
	mk := func(id string) *SessionMeta {
		m := &SessionMeta{ID: id, Mode: "task"}
		if err := store.Create(m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	return a, mk("um-sess-a"), mk("um-sess-b")
}

func TestUserMemoryDocRoundTrip(t *testing.T) {
	d := UserMemoryData{
		UserName: "张总", Memory: "做制造业；喜欢简洁汇报\n- 周报用表格",
		AIName: "小坚", AIIdentity: "务实的工程助理", BubbleColor: "#dbeafe",
	}
	got := parseUserMemoryDoc(renderUserMemoryDoc(d))
	if got != d {
		t.Fatalf("文档往返不一致:\n%+v\n%+v", d, got)
	}
	// 手工编辑容错：段序打乱、有多余段落与引用行，已知段仍能读出
	manual := "# 用户记忆\n\n> 引用说明\n\n## AI 助理名字\n\n小坚\n\n## 未知段\n\n忽略我\n\n## 用户称呼\n\n张总\n"
	got = parseUserMemoryDoc(manual)
	if got.AIName != "小坚" || got.UserName != "张总" || got.Memory != "" {
		t.Fatalf("手工文档解析不符: %+v", got)
	}
	// 空字段渲染后再读仍为空（空段保留标题）
	got = parseUserMemoryDoc(renderUserMemoryDoc(UserMemoryData{UserName: "x"}))
	if got.Memory != "" || got.UserName != "x" {
		t.Fatalf("空字段往返不符: %+v", got)
	}
}

func TestUserMemoryLengthPolicy(t *testing.T) {
	a, _, _ := newUserMemoryTestApp(t)

	// 存储层不截断（用户拍板：md 文件存完整内容；约束在写入入口把关）
	long := strings.Repeat("记", 1200) + "🚀尾"
	saved, err := a.saveUserMemory(UserMemoryData{Memory: long})
	if err != nil {
		t.Fatal(err)
	}
	if saved.Memory != long || a.loadUserMemory().Memory != long {
		t.Fatal("saveUserMemory 不应截断（存储完整内容）")
	}

	// 注入侧截断：超长字段进环境快照时截到 1000 字加尾注（快照不膨胀）
	env := a.envPrompt(&SessionMeta{Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace})
	if !strings.Contains(env, "超长，已截断") {
		t.Fatalf("超长字段的注入行应带截断尾注:\n%s", env)
	}
	for _, ln := range strings.Split(env, "\n") {
		if strings.HasPrefix(ln, "- 用户身份与喜好：") && utf8.RuneCountInString(ln) > userMemoryFieldLimit+40 {
			t.Fatalf("注入行应截到 1000 字+尾注以内，实际 %d 字", utf8.RuneCountInString(ln))
		}
	}

	// 写入入口（模型侧）超长整单报错：call_api 的 POST
	w := httptest.NewRecorder()
	a.handleUserMemorySet(w, httptest.NewRequest(http.MethodPost, "/api/user-memory",
		strings.NewReader(`{"userMemory":"`+strings.Repeat("忆", 1100)+`"}`)))
	if w.Code != 400 || !strings.Contains(w.Body.String(), "上限") {
		t.Fatalf("POST 超长应 400 并说明上限: %d %s", w.Code, w.Body.String())
	}
	if n := len([]rune(a.loadUserMemory().Memory)); n != len([]rune(long)) {
		t.Fatalf("POST 超长报错后不应写入（应保持原值），实际 %d 字", n)
	}

	// 写入入口（工具）超长报错不写入
	tool := a.userMemoryTool()
	tk := &toolContext{ctx: t.Context(), app: a, sess: &SessionMeta{Mode: "task"}}
	if _, err := tool.Run(tk, json.RawMessage(`{"memory":"`+strings.Repeat("忆", 1001)+`"}`)); err == nil || !strings.Contains(err.Error(), "上限") {
		t.Fatalf("工具超长应报错: %v", err)
	}
	if a.loadUserMemory().Memory != long {
		t.Fatal("工具超长报错后不应写入")
	}
	// 合长（≤1000）写入正常
	if _, err := tool.Run(tk, json.RawMessage(`{"memory":"新记忆"}`)); err != nil {
		t.Fatal(err)
	}
	if a.loadUserMemory().Memory != "新记忆" {
		t.Fatal("合长写入应成功")
	}
}

func TestUserMemoryEnvPromptToggle(t *testing.T) {
	a, _, _ := newUserMemoryTestApp(t)
	sess := &SessionMeta{Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace}
	if _, err := a.saveUserMemory(UserMemoryData{UserName: "张总", Memory: "做制造业", AIName: "小坚", AIIdentity: "工程助理"}); err != nil {
		t.Fatal(err)
	}
	// 开启：注入记忆块，且不进系统提示词、不进工具定义
	env := a.envPrompt(sess)
	for _, want := range []string{"用户记忆", "张总", "做制造业", "小坚", "工程助理", "user_memory"} {
		if !strings.Contains(env, want) {
			t.Fatalf("组件开启时环境快照应含 %q:\n%s", want, env)
		}
	}
	// 四行标签是用户拍板的最终措辞（第二人称视角，照抄不润色）——钉住防回归
	for _, want := range []string{
		"- 用户的名字：张总",
		"- 用户身份与习惯、喜好：做制造业",
		"- 你的名字（用户会用这个名字叫你，你也可以用这个名字自称）：小坚",
		"- 用户定义的你的身份：工程助理",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("环境快照记忆块的标签应逐字等于用户拍板措辞，缺 %q:\n%s", want, env)
		}
	}
	if strings.Contains(a.systemPrompt(sess), "张总") {
		t.Fatal("用户记忆不得进系统提示词（设计：只进环境提示词）")
	}
	// 工具挂载：执行模式有、aux/assistant 没有（经理走 call_api 通道）
	hasTool := func(mode string) bool {
		for _, tl := range a.toolsFor(mode) {
			if tl.Spec.Function.Name == "user_memory" {
				return true
			}
		}
		return false
	}
	if !hasTool("task") || !hasTool("coding") || !hasTool("office") {
		t.Fatal("执行模式应挂 user_memory 工具")
	}
	if hasTool("aux") || hasTool("assistant") {
		t.Fatal("aux（只读答疑）与 assistant（call_api 整文件通道）不应挂 user_memory 工具")
	}
	// 经理模式的环境快照说明整文件通道与文件路径
	mgrEnv := a.envPrompt(&SessionMeta{Mode: "assistant", Permission: permWorkspace, Workspace: a.Cfg.Workspace})
	for _, want := range []string{"长期助理专属", "GET /api/user-memory", "POST /api/user-memory", "user-memory.md"} {
		if !strings.Contains(mgrEnv, want) {
			t.Fatalf("经理环境快照应含 %q:\n%s", want, mgrEnv)
		}
	}
	// 关闭：注入移除、工具卸载（开关即点即生效——envPrompt/toolsFor 每轮现算）
	if err := a.Components.SetEnabled(userMemoryComponentID, false); err != nil {
		t.Fatal(err)
	}
	if env2 := a.envPrompt(sess); strings.Contains(env2, "张总") || strings.Contains(env2, "用户记忆（") {
		t.Fatalf("组件关闭后环境快照不应再有记忆块:\n%s", env2)
	}
	if hasTool("task") {
		t.Fatal("组件关闭后 user_memory 工具应卸载")
	}
	// 全空记忆：开启也不注入（没内容就没有块）
	if err := a.Components.SetEnabled(userMemoryComponentID, true); err != nil {
		t.Fatal(err)
	}
	if _, err := a.saveUserMemory(UserMemoryData{}); err != nil {
		t.Fatal(err)
	}
	if env3 := a.envPrompt(sess); strings.Contains(env3, "# 用户记忆") {
		t.Fatalf("记忆全空时环境快照不应有记忆块:\n%s", env3)
	}
}

func TestUserMemoryToolOnlyMemorySegment(t *testing.T) {
	a, _, _ := newUserMemoryTestApp(t)
	if _, err := a.saveUserMemory(UserMemoryData{UserName: "张总", Memory: "旧记忆", AIName: "小坚", AIIdentity: "工程助理"}); err != nil {
		t.Fatal(err)
	}
	tool := a.userMemoryTool()
	tk := &toolContext{ctx: t.Context(), app: a, sess: &SessionMeta{Mode: "task"}}

	// 查询：返回全部字段
	out, err := tool.Run(tk, json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"张总", "旧记忆", "小坚", "工程助理"} {
		if !strings.Contains(out, want) {
			t.Fatalf("查询应返回字段 %q: %s", want, out)
		}
	}

	// 更新记忆段：只动 userMemory，其余字段原样
	if _, err := tool.Run(tk, json.RawMessage(`{"memory":"新记忆：喜欢表格汇报"}`)); err != nil {
		t.Fatal(err)
	}
	d := a.loadUserMemory()
	if d.Memory != "新记忆：喜欢表格汇报" {
		t.Fatalf("记忆段应已更新: %+v", d)
	}
	if d.UserName != "张总" || d.AIName != "小坚" || d.AIIdentity != "工程助理" {
		t.Fatalf("专用工具不得改动记忆段以外的字段: %+v", d)
	}

	// 超长内容经工具写入：整单报错不写入（长度约束在入口，见 TestUserMemoryLengthPolicy）
	if _, err := tool.Run(tk, json.RawMessage(`{"memory":"`+strings.Repeat("忆", 1100)+`"}`)); err == nil {
		t.Fatal("工具写入超长内容应报错（不静默截断）")
	}
	if a.loadUserMemory().Memory != "新记忆：喜欢表格汇报" {
		t.Fatalf("超长报错后记忆段应保持原值: %+v", a.loadUserMemory())
	}

	// 组件中途关闭：工具调用时报错指路（目录在本轮开轮定型，调用时再核）
	if err := a.Components.SetEnabled(userMemoryComponentID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Run(tk, json.RawMessage(`{}`)); err == nil || !strings.Contains(err.Error(), "未启用") {
		t.Fatalf("组件关闭后工具应报未启用: %v", err)
	}
}

func TestUserMemoryAPIAndCatalog(t *testing.T) {
	a, _, _ := newUserMemoryTestApp(t)
	// 经理通道在 call_api 目录里（apiMuxOf 触发注册）
	_ = a.apiMuxOf()
	found := map[string]bool{}
	a.apiMetaMu.Lock()
	for _, m := range a.apiMetas {
		if m.Path == "/api/user-memory" {
			found[m.Method+" "+m.Path] = true
		}
	}
	a.apiMetaMu.Unlock()
	if !found["GET /api/user-memory"] || !found["POST /api/user-memory"] {
		t.Fatalf("call_api 目录应含 GET/POST /api/user-memory: %v", found)
	}

	// GET：空文档返回空字段
	w := httptest.NewRecorder()
	a.handleUserMemoryGet(w, httptest.NewRequest(http.MethodGet, "/api/user-memory", nil))
	if w.Code != 200 {
		t.Fatalf("GET 应 200: %d %s", w.Code, w.Body.String())
	}
	// POST：按字段保存（只给两个字段，未给的保持）+ 全量读回
	w = httptest.NewRecorder()
	a.handleUserMemorySet(w, httptest.NewRequest(http.MethodPost, "/api/user-memory",
		strings.NewReader(`{"userName":"张总","userMemory":"做制造业"}`)))
	if w.Code != 200 {
		t.Fatalf("POST 应 200: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.handleUserMemorySet(w, httptest.NewRequest(http.MethodPost, "/api/user-memory",
		strings.NewReader(`{"aiName":"小坚"}`)))
	if w.Code != 200 {
		t.Fatalf("POST 二段应 200: %d %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	a.handleUserMemoryGet(w, httptest.NewRequest(http.MethodGet, "/api/user-memory", nil))
	var got UserMemoryData
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.UserName != "张总" || got.Memory != "做制造业" || got.AIName != "小坚" {
		t.Fatalf("按字段保存语义不符: %+v", got)
	}
	// 组件关闭：两个接口都 404（功能整体随开关停）
	if err := a.Components.SetEnabled(userMemoryComponentID, false); err != nil {
		t.Fatal(err)
	}
	for _, m := range []string{http.MethodGet, http.MethodPost} {
		w = httptest.NewRecorder()
		if m == http.MethodGet {
			a.handleUserMemoryGet(w, httptest.NewRequest(m, "/api/user-memory", nil))
		} else {
			a.handleUserMemorySet(w, httptest.NewRequest(m, "/api/user-memory", strings.NewReader(`{}`)))
		}
		if w.Code != 404 {
			t.Fatalf("组件关闭后 %s 应 404: %d", m, w.Code)
		}
	}
}

// 会话级记忆合并（2026-11-08 关键补缺）：环境注入按会话视角——A 会话独立
// 人格（覆盖生效）、B 会话跟随（全局值）、开关覆盖关（整块不注入）三态；
// POST ?s= 与 user_memory 工具在独立会话里写覆盖、全局文档不动、与全局同值
// 归一删记录。
func TestUserMemorySessionMerge(t *testing.T) {
	a, sessA, sessB := newUserMemoryTestApp(t)

	// 全局基线
	if _, err := a.saveUserMemory(UserMemoryData{
		UserName: "张总", Memory: "做制造业", AIName: "小坚", AIIdentity: "工程助理", BubbleColor: "#dbeafe",
	}); err != nil {
		t.Fatal(err)
	}

	// A 会话：总开关开 + 独立人格覆盖（名字/称呼换人，其余字段回落全局）
	a.SessionCX.set(a.Store, sessA.ID, func(o *SessionOverrides) {
		o.CfgScope = true
		if o.Cfg == nil {
			o.Cfg = map[string]map[string]string{}
		}
		o.Cfg[userMemoryComponentID] = map[string]string{
			"userName": "李工", "aiName": "小美",
		}
	})

	// A：覆盖生效、未覆盖字段回落全局、带独立说明
	envA := a.envPrompt(&SessionMeta{ID: sessA.ID, Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace})
	for _, want := range []string{"- 用户的名字：李工", "- 用户身份与习惯、喜好：做制造业", "- 你的名字（用户会用这个名字叫你，你也可以用这个名字自称）：小美", "独立的记忆设定"} {
		if !strings.Contains(envA, want) {
			t.Fatalf("A 会话独立人格的注入应含 %q:\n%s", want, envA)
		}
	}
	if strings.Contains(envA, "张总") || strings.Contains(envA, "小坚") {
		t.Fatalf("A 会话被覆盖的字段不应再出现全局值:\n%s", envA)
	}

	// B：跟随全局
	envB := a.envPrompt(&SessionMeta{ID: sessB.ID, Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace})
	for _, want := range []string{"- 用户的名字：张总", "小坚"} {
		if !strings.Contains(envB, want) {
			t.Fatalf("B 会话应注入全局值（含 %q）:\n%s", want, envB)
		}
	}
	if strings.Contains(envB, "李工") {
		t.Fatalf("B 会话不应看到 A 的覆盖:\n%s", envB)
	}

	// A 开关覆盖关：整块不注入（与开关覆盖语义一致）
	a.cxoSetComponent(sessA.ID, userMemoryComponentID, false)
	if env := a.envPrompt(&SessionMeta{ID: sessA.ID, Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace}); strings.Contains(env, "用户记忆（") {
		t.Fatalf("A 会话关掉组件后不应注入记忆块:\n%s", env)
	}
	a.cxoSetComponent(sessA.ID, userMemoryComponentID, true) // 恢复，继续下面的写路径测试

	// POST ?s=A：写会话覆盖（全局文档不动），与全局同值归一删记录
	w := httptest.NewRecorder()
	a.handleUserMemorySet(w, httptest.NewRequest(http.MethodPost, "/api/user-memory?s="+sessA.ID,
		strings.NewReader(`{"userName":"王工","userMemory":"会话独立记忆"}`)))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"session":true`) {
		t.Fatalf("A 会话 POST 应走覆盖路径: %d %s", w.Code, w.Body.String())
	}
	if g := a.loadUserMemory(); g.UserName != "张总" || g.Memory != "做制造业" {
		t.Fatalf("会话写入不应动全局文档: %+v", g)
	}
	view, scoped := a.userMemorySessionView(&SessionMeta{ID: sessA.ID})
	if !scoped || view.UserName != "王工" || view.Memory != "会话独立记忆" || view.AIName != "小美" {
		t.Fatalf("A 会话视角不符: scoped=%v %+v", scoped, view)
	}
	// 归一：把 userName 改回与全局一致（张总）→ 覆盖记录删除、回落全局
	w = httptest.NewRecorder()
	a.handleUserMemorySet(w, httptest.NewRequest(http.MethodPost, "/api/user-memory?s="+sessA.ID,
		strings.NewReader(`{"userName":"张总"}`)))
	if w.Code != 200 {
		t.Fatalf("归一写入应 200: %d %s", w.Code, w.Body.String())
	}
	if o := a.SessionCX.of(sessA.ID); o != nil {
		if _, ok := o.Cfg[userMemoryComponentID]["userName"]; ok {
			t.Fatal("与全局同值的字段应删覆盖记录（只记显式覆盖）")
		}
	}

	// 工具：独立会话里查询/写入都按会话视角（全局不动）
	tool := a.userMemoryTool()
	tk := &toolContext{ctx: t.Context(), app: a, sess: &SessionMeta{ID: sessA.ID, Mode: "task"}}
	out, err := tool.Run(tk, json.RawMessage(`{"memory":"工具写的会话记忆"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "本对话独立记忆") || !strings.Contains(out, "工具写的会话记忆") {
		t.Fatalf("独立会话的工具写入应走覆盖并说明: %s", out)
	}
	if g := a.loadUserMemory(); g.Memory != "做制造业" {
		t.Fatalf("工具的会话写入不应动全局文档: %+v", g)
	}
	// B 会话（跟随）工具写入走全局文档
	tkB := &toolContext{ctx: t.Context(), app: a, sess: &SessionMeta{ID: sessB.ID, Mode: "task"}}
	if _, err := tool.Run(tkB, json.RawMessage(`{"memory":"B 会话写全局"}`)); err != nil {
		t.Fatal(err)
	}
	if g := a.loadUserMemory(); g.Memory != "B 会话写全局" {
		t.Fatalf("B 会话（跟随全局）的工具写入应落全局文档: %+v", g)
	}

	// 持久化：覆盖随会话附属数据落库，重启（重新 loadSessionCX）仍在
	reg := loadSessionCX(a.Store)
	o := reg.of(sessA.ID)
	if o == nil || o.Cfg[userMemoryComponentID]["userMemory"] != "工具写的会话记忆" {
		t.Fatalf("会话覆盖应持久化: %+v", o)
	}
}
