package main

// 「数据助理编排体系」改造的验收测试：
//   A 回报重构（完整最终答复 + 结尾消息 + 无工具统计 + 路径核对 + 过程折叠两态）
//   B 数字助理工具清单去掉 dispatch_sub / list_experts，提示词规则同步
//   C send_to_session 的 permission（同等或降权、不给已有会话提权）与默认插话语义
//   D 环境快照「可用模型清单」+ 裸模型名走错渠道的提示兜底
//   E 步数默认不限制（0）；历次默认 30/100/200/1000 随升级迁到不限制，用户自己设的值不动
//   F read_session_messages 单条长消息完整读取（窗口 + seq + from_char 续读）

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// ---------- A. 回报格式 ----------

// appendRowOf 落一条测试消息并返回它（seq 由库分配）。
func appendRowOf(t *testing.T, a *App, sid string, m Message, kind, origin string) *StoredMessage {
	t.Helper()
	sm := &StoredMessage{Message: m, Kind: kind, Origin: origin, CreatedAt: now()}
	if err := a.Store.AppendRow(sid, sm); err != nil {
		t.Fatalf("落库失败: %v", err)
	}
	return sm
}

// 新回报体例：完整最终答复（不截断）、结尾消息、无工具调用统计、改动文件路径
// 与实际落盘核对一致（含「记录错了、实际在别处」的纠正）、相对路径标注工作区。
func TestReportDigestFullAnswerAndVerifiedFiles(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	ws := a.Cfg.Workspace
	sub := &SessionMeta{ID: "subA", Title: "演示任务", Mode: "task", Workspace: ws, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(sub); err != nil {
		t.Fatal(err)
	}
	// 真实落盘的文件：报告实际在 .jxai\tmp\ 下（工具参数里却会写成 .jxai\tmp\patch\）
	if err := os.MkdirAll(filepath.Join(ws, ".jxai", "tmp"), 0o755); err != nil {
		t.Fatal(err)
	}
	realRel := filepath.Join(".jxai", "tmp", "报告.md")
	if err := os.WriteFile(filepath.Join(ws, realRel), []byte("报告内容"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 另一个真实文件（正常路径，用于「已核对落盘」）
	okRel := filepath.Join(".jxai", "tmp", "明细.csv")
	if err := os.WriteFile(filepath.Join(ws, okRel), []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	appendRowOf(t, a, sub.ID, TextMessage("user", "写一份报告，顺便出个明细"), kindChat, "")
	// 过程正文：正常完成时回报里只留要点；这一条足够长，验证要点被压缩（不是原文全给）
	appendRowOf(t, a, sub.ID, TextMessage("assistant", strings.Repeat("过程旁白。", 200)), kindChat, "")
	writeCall := appendRowOf(t, a, sub.ID, Message{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "c1", Type: "function", Function: ToolCallFunc{Name: "write", Arguments: `{"path":` + strconv.Quote(filepath.Join(".jxai", "tmp", "patch", "报告.md")) + `,"content":"x"}`}},
		{ID: "c2", Type: "function", Function: ToolCallFunc{Name: "write", Arguments: `{"path":` + strconv.Quote(okRel) + `,"content":"y"}`}},
	}}, kindChat, "")
	appendRowOf(t, a, sub.ID, Message{Role: "tool", Content: "已写入", ToolCallID: writeCall.ToolCalls[0].ID}, kindChat, "")
	appendRowOf(t, a, sub.ID, Message{Role: "tool", Content: "已写入", ToolCallID: writeCall.ToolCalls[1].ID}, kindChat, "")
	// 一个没落盘的文件（记录里写了、磁盘上没有）——不得硬说文件在
	missCall := appendRowOf(t, a, sub.ID, Message{Role: "assistant", ToolCalls: []ToolCall{
		{ID: "c3", Type: "function", Function: ToolCallFunc{Name: "write", Arguments: `{"path":"不存在的目录/没这个文件.md","content":"z"}`}},
	}}, kindChat, "")
	appendRowOf(t, a, sub.ID, Message{Role: "tool", Content: "已写入", ToolCallID: missCall.ToolCalls[0].ID}, kindChat, "")
	// 最终答复：4000 字（旧实现截到 900 字）
	final := strings.Repeat("结论：本轮把所有改动落盘并核对完毕。", 200)
	appendRowOf(t, a, sub.ID, TextMessage("assistant", final), kindChat, "")
	// 结尾消息：最终答复之后的收尾内容
	appendRowOf(t, a, sub.ID, TextMessage("user", "【交付结果】已把 .jxai\\tmp\\报告.md 交付到上传目录"), kindReport, "deliver")

	rows, err := a.Store.MessagesWithSeq(sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	out := renderSummary(a, sub, rows, false)

	if !strings.Contains(out, "共 2 轮 / 2 步") {
		t.Fatalf("回报应保留轮数/步数与会话 ID：\n%s", out)
	}
	// 把这份真实渲染的回报落一份到 .jxai/tmp 供人工比对格式（不影响断言）
	_ = os.WriteFile(filepath.Join(".jxai", "tmp", "回报样例-新格式.txt"), []byte(out), 0o644)
	if !strings.Contains(out, "· 工作区："+ws) {
		t.Fatalf("回报应带工作区：\n%s", out)
	}
	if !strings.Contains(out, final) {
		t.Fatalf("回报必须附完整最终答复正文（%d 字，一个不漏）", len([]rune(final)))
	}
	if strings.Contains(out, "工具调用：") || strings.Contains(out, "×1") {
		t.Fatalf("回报不该再有工具调用统计：\n%s", out)
	}
	if !strings.Contains(out, "· 结尾消息（最终答复之后的收尾内容）") || !strings.Contains(out, "【交付结果】") {
		t.Fatalf("回报应附最终答复之后的结尾消息：\n%s", out)
	}
	if !strings.Contains(out, "· 过程要点（正常完成，只留要点）") {
		t.Fatalf("正常完成的过程内容应压成要点：\n%s", out)
	}
	if strings.Contains(out, strings.Repeat("过程旁白。", 100)) {
		t.Fatalf("过程旁白不该整段罗列：\n%s", out)
	}
	// 改动文件：纠正过的实际路径 + 工作区标注 + 未找到的那条如实标注
	if !strings.Contains(out, "已核对：工具记录为") || !strings.Contains(out, ".jxai/tmp/报告.md") {
		t.Fatalf("记录与实际不一致的路径应按实际落盘纠正：\n%s", out)
	}
	if !strings.Contains(out, "工作区 "+ws) {
		t.Fatalf("相对路径应标注所属工作区：\n%s", out)
	}
	if !strings.Contains(out, "未在磁盘上找到") {
		t.Fatalf("没落盘的文件应如实标注：\n%s", out)
	}

	// 中断收尾：过程内容改成保留断点附近的原文（原文形态，不压成一句）
	midReport := renderSummary(a, sub, rows, true)
	if !strings.Contains(midReport, "· 断点附近的过程内容（中断，保留原文）") {
		t.Fatalf("中断时过程内容应保留原文：\n%s", midReport)
	}
	if !strings.Contains(midReport, "过程旁白。过程旁白。") {
		t.Fatalf("中断时断点附近正文应保留原文格式：\n%s", midReport)
	}
}

// 极长最终答复（超 reportFinalCap）才截断，且必须写明「完整版为会话第 N 条」与
// 读取入口——不再是无出路的硬截断。
func TestReportDigestTruncationPointsToSeq(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	sub := &SessionMeta{ID: "subT", Title: "超长答复", Mode: "task", Workspace: a.Cfg.Workspace}
	if err := a.Store.Create(sub); err != nil {
		t.Fatal(err)
	}
	appendRowOf(t, a, sub.ID, TextMessage("user", "写个超长总结"), kindChat, "")
	long := strings.Repeat("很长的一段结论。", reportFinalCap) // 远超上限
	sm := appendRowOf(t, a, sub.ID, TextMessage("assistant", long), kindChat, "")
	rows, _ := a.Store.MessagesWithSeq(sub.ID)
	out := renderSummary(a, sub, rows, false)
	if !strings.Contains(out, "已截断") || !strings.Contains(out, strconv.FormatInt(sm.Seq, 10)) {
		t.Fatalf("超长答复应明示截断并给出会话第 N 条：\n%.400s", out)
	}
	if !strings.Contains(out, "read_session_messages") {
		t.Fatalf("截断时应给完整读取方式：\n%.400s", out)
	}
}

// ---------- B. 数字助理不再持有子对话派发工具 ----------

func toolNames(t *testing.T, a *App, mode string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, tl := range a.toolsFor(mode) {
		out[tl.Spec.Function.Name] = true
	}
	return out
}

func TestAssistantLosesSubDispatchTools(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	as := toolNames(t, a, "assistant")
	for _, gone := range []string{"dispatch_sub", "list_experts"} {
		if as[gone] {
			t.Fatalf("数字助理不该再有 %s（收走子对话派发与专家名单）", gone)
		}
	}
	for _, keep := range []string{"dispatch_task", "send_to_session", "list_sessions", "read_session_summary"} {
		if !as[keep] {
			t.Fatalf("数字助理应保留 %s", keep)
		}
	}
	// 工作任务自己派子对话的能力不受影响
	tk := toolNames(t, a, "task")
	for _, keep := range []string{"dispatch_sub", "list_experts"} {
		if !tk[keep] {
			t.Fatalf("任务会话应保留 %s", keep)
		}
	}
	// 系统提示词：子对话选型/继承上下文/list_experts 的指引全删，改成「一律用 dispatch_task」
	sys := a.systemPrompt(&SessionMeta{Mode: "assistant"})
	for _, gone := range []string{"list_experts", "继承上下文子对话", "dispatch_sub", "expert="} {
		if strings.Contains(sys, gone) {
			t.Fatalf("数字助理提示词里不该再有子对话指引 %q", gone)
		}
	}
	if !strings.Contains(sys, "建会话一律用 dispatch_task") {
		t.Fatalf("数字助理提示词应写明一律用 dispatch_task 建会话：\n%s", sys)
	}
	// 任务的「子对话派发」准则不受影响
	if tkSys := a.systemPrompt(&SessionMeta{Mode: "task"}); !strings.Contains(tkSys, "## 子对话派发") {
		t.Fatal("任务会话的子对话派发准则应保持")
	}
}

// ---------- B2. 数字助理的只读四件套与 web_fetch ----------

func TestAssistantReadOnlyTools(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	as := toolNames(t, a, "assistant")
	// 六个只读工具注册进总控（小问题自己看文件答，不必派任务；
	// read_image 让总控自己看懂用户上传的图片再做编排）
	for _, keep := range []string{"read", "list_dir", "glob", "grep", "web_fetch", "read_image"} {
		if !as[keep] {
			t.Fatalf("数字助理应有只读工具 %s", keep)
		}
	}
	// 红线：写、执行、交互、协调类工具绝不进场（只读定位）
	for _, gone := range []string{"write", "edit", "run_command", "run_python", "snapshot_file",
		"deliver_file", "dispatch_sub", "browser_open", "mcp_call_tool", "request_manager"} {
		if as[gone] {
			t.Fatalf("数字助理不该有 %s（只读红线）", gone)
		}
	}
	// 提示词带使用纪律：小事自己看、只读不写不执行、大批量仍派任务、读完就答
	sys := a.systemPrompt(&SessionMeta{Mode: "assistant"})
	for _, want := range []string{"只读自查的纪律", "十步", "只读不写不执行", "批量分析", "读完就答"} {
		if !strings.Contains(sys, want) {
			t.Fatalf("数字助理提示词缺只读纪律要点 %q", want)
		}
	}
	// 任务会话的文件工具不受影响
	tk := toolNames(t, a, "task")
	for _, keep := range []string{"read", "list_dir", "glob", "grep", "web_fetch", "write", "edit"} {
		if !tk[keep] {
			t.Fatalf("任务会话应保留 %s", keep)
		}
	}
}

// ---------- C. send_to_session 的权限参数 ----------

func TestSendToSessionPermission(t *testing.T) {
	a, main := newDispatchTestApp(t)
	a.Cfg.BaseURL = "mock" // 被派发的会话走内置演示模型，测试不联网
	mk := func(id, perm string) {
		m := &SessionMeta{ID: id, Title: "目标任务 " + id, Mode: "task", Workspace: a.Cfg.Workspace,
			Permission: perm, CreatedAt: now(), UpdatedAt: now()}
		if err := a.Store.Create(m); err != nil {
			t.Fatal(err)
		}
	}
	mk("t-narrow", permReadOnly)
	mk("t-open", permWorkspace)
	ctx := dispatchCtx(a, main)

	// ① 高于本会话（数字助理）权限：拒绝并报错
	if _, err := runSendToSession(ctx, mustArgs(t, map[string]any{
		"session_id": "t-open", "task": "越权试试", "permission": "full",
	})); err == nil || !strings.Contains(err.Error(), "增权") {
		t.Fatalf("send_to_session 增权应被拒绝, got %v", err)
	}
	// ② 高于目标会话自己的权限：拒绝（不能借这条需求给已有会话提权）
	if _, err := runSendToSession(ctx, mustArgs(t, map[string]any{
		"session_id": "t-narrow", "task": "提权试试", "permission": "workspace-write",
	})); err == nil || !strings.Contains(err.Error(), "不能给已有会话提权") {
		t.Fatalf("不能给已有会话提权, got %v", err)
	}
	// ③ 同等或降权：通过，且落到目标会话上（落库）
	out, err := runSendToSession(ctx, mustArgs(t, map[string]any{
		"session_id": "t-open", "task": "只读看一遍", "permission": "read-only",
	}))
	if err != nil {
		t.Fatalf("降权应有通过：%v", err)
	}
	if !strings.Contains(out, "已派发后台执行") || !strings.Contains(out, "只读") {
		t.Fatalf("派发结果应说明权限档：%s", out)
	}
	if m, err := a.Store.Get("t-open"); err != nil || permModeOf(m) != permReadOnly {
		t.Fatalf("目标会话权限应落到只读, got %v err=%v", m, err)
	}
	// ④ 不传 permission：沿用目标会话当前档位，不做改动
	if _, err := runSendToSession(ctx, mustArgs(t, map[string]any{
		"session_id": "t-narrow", "task": "接着看",
	})); err != nil {
		t.Fatalf("不传 permission 应照常派发：%v", err)
	}
	if m, _ := a.Store.Get("t-narrow"); permModeOf(m) != permReadOnly {
		t.Fatalf("不传 permission 不该改动目标会话权限, got %s", permModeOf(m))
	}
}

// C2：send_to_session 默认语义＝插话——目标会话正在执行时，话进**插话**队列
// （下一次调用模型前注入），不是排队（AI 投递一律插话，已没有按偏好排队的
// 路径）。空闲目标直接开跑的路径已在 C 的 ③④ 覆盖。
func TestSendToSessionSteersRunningSession(t *testing.T) {
	a, main := newDispatchTestApp(t)
	target := &SessionMeta{ID: "t-busy", Title: "正在干活的任务", Mode: "task",
		Workspace: a.Cfg.Workspace, CreatedAt: now(), UpdatedAt: now()}
	if err := a.Store.Create(target); err != nil {
		t.Fatal(err)
	}
	if _, ok := a.beginRun(target.ID); !ok { // 占住执行权＝目标「正在执行」
		t.Fatal("占住目标会话执行权失败")
	}
	defer a.finishRun(target.ID)

	out, err := runSendToSession(dispatchCtx(a, main), mustArgs(t, map[string]any{
		"session_id": "t-busy", "task": "先别动，等 B 完事",
	}))
	if err != nil {
		t.Fatalf("向执行中的会话发话应成功：%v", err)
	}
	if !strings.Contains(out, "已插话") {
		t.Fatalf("工具返回应说明已插话：%s", out)
	}
	p := a.pendingOf(target.ID)
	if len(p.Steer) != 1 || len(p.Queue) != 0 {
		t.Fatalf("话应进插话队列（Steer=1、Queue=0），got Steer=%d Queue=%d", len(p.Steer), len(p.Queue))
	}
	if pm := p.Steer[0]; pm.Kind != kindAiUser || pm.Origin != "task" ||
		!strings.HasPrefix(pm.Content, "【数字助理转达】") {
		t.Fatalf("插话身份不对：kind=%q origin=%q content=%.20q", pm.Kind, pm.Origin, pm.Content)
	}
	// 执行中的目标没有被这条话抢占：执行权仍在原一轮手里
	if !a.IsRunning(target.ID) {
		t.Fatal("插话不该打断目标正在执行的这一轮")
	}
}

// ---------- D. 模型清单与渠道提示 ----------

func catalogTestProviders() []ProviderCfg {
	off := false
	return []ProviderCfg{
		{ID: "deepseek", Name: "DeepSeek", BaseURL: "mock", Models: []ModelCfg{{ID: "deepseek-v4-pro", Name: "DeepSeek-V4-Pro"}}},
		{ID: "zhipu-coding", Name: "智谱 GLM（Coding Plan 套餐）", BaseURL: "mock",
			Models: []ModelCfg{{ID: "glm-5.3", Name: "GLM-5.3"}}},
		{ID: "siliconflow", Name: "硅基流动", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}}},
		{ID: "ollama", Name: "Ollama 本地", BaseURL: "mock", Enabled: &off},
	}
}

func TestEnvSnapshotModelCatalog(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	a.Cfg.Providers = catalogTestProviders()
	env := a.envPrompt(&SessionMeta{Mode: "assistant"})
	if !strings.Contains(env, "- 可用模型清单") {
		t.Fatalf("环境快照应有可用模型清单：\n%s", env)
	}
	for _, want := range []string{"deepseek/deepseek-v4-pro", "zhipu-coding/glm-5.3", "GLM-5.3", "siliconflow/glm-5.3"} {
		if !strings.Contains(env, want) {
			t.Fatalf("模型清单缺 %q：\n%s", want, env)
		}
	}
	if !strings.Contains(env, "ollama/*") || !strings.Contains(env, "已停用") {
		t.Fatalf("没配模型清单的渠道要如实列出，停用的要标注：\n%s", env)
	}
}

func TestDispatchModelChannelGuard(t *testing.T) {
	a, main := newDispatchTestApp(t)
	a.Cfg.Providers = catalogTestProviders()
	main.Provider = "deepseek"
	ctx := dispatchCtx(a, main)

	// 裸模型名走错渠道（本次渠道 deepseek 不提供 glm-5.3）：拒绝 + 列全部渠道 + 提示完整格式
	_, err := runDispatchTask(ctx, mustArgs(t, map[string]any{"task": "用 glm-5.3 做点事", "model": "glm-5.3"}))
	if err == nil {
		t.Fatal("裸模型名走错渠道应被拒绝")
	}
	for _, want := range []string{"zhipu-coding/glm-5.3", "siliconflow/glm-5.3", "providerId/modelId"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("渠道提示缺 %q：%v", want, err)
		}
	}
	if subs := otherSessions(t, a); len(subs) != 0 {
		t.Fatalf("被拒绝的派发不该建出会话, got %d", len(subs))
	}
	// 完整格式（渠道+模型）：放行，并落到会话上
	if _, err := runDispatchTask(ctx, mustArgs(t, map[string]any{
		"task": "写个总结", "title": "跑一下", "model": "zhipu-coding/glm-5.3",
	})); err != nil {
		t.Fatalf("完整格式应放行：%v", err)
	}
	subs := otherSessions(t, a)
	if len(subs) != 1 || subs[0].Provider != "zhipu-coding" || subs[0].Model != "glm-5.3" {
		t.Fatalf("渠道+模型应落到被派发的会话：%+v", subs)
	}
	// 裸模型名恰好是本会话渠道提供的：放行
	main.Provider = "zhipu-coding"
	if _, err := runDispatchTask(ctx, mustArgs(t, map[string]any{"task": "再写一份", "model": "glm-5.3"})); err != nil {
		t.Fatalf("本渠道提供该模型时不该拦：%v", err)
	}
	// 本地清单里查不到的模型名（自建接入点等）：放行，不擅自裁决
	if _, err := runDispatchTask(ctx, mustArgs(t, map[string]any{"task": "自建接入点", "model": "ep-2026abc"})); err != nil {
		t.Fatalf("清单外的模型名不该拦：%v", err)
	}
}

// 运行期兜底：模型不可用类报错（如「无可用渠道」503）补上渠道线索。
func TestModelErrChannelHint(t *testing.T) {
	a, _ := newDispatchTestApp(t)
	a.Cfg.Providers = catalogTestProviders()
	raw := errors.New("模型服务返回 503：无可用渠道")
	got := a.modelErrWithChannelHint(LLMTarget{ProviderID: "deepseek", Model: "glm-5.3"}, raw)
	if !strings.Contains(got.Error(), "zhipu-coding/glm-5.3") || !strings.Contains(got.Error(), "providerId/modelId") {
		t.Fatalf("报错应补渠道线索：%v", got)
	}
	// 本次渠道就提供它：原样返回，不乱加提示
	if got := a.modelErrWithChannelHint(LLMTarget{ProviderID: "zhipu-coding", Model: "glm-5.3"}, raw); got.Error() != raw.Error() {
		t.Fatalf("渠道本身提供该模型时不该加提示：%v", got)
	}
	// 与模型无关的报错：原样返回
	if got := a.modelErrWithChannelHint(LLMTarget{ProviderID: "deepseek", Model: "glm-5.3"}, errors.New("请求已取消")); got.Error() != "请求已取消" {
		t.Fatalf("无关报错不该加提示：%v", got)
	}
}

// ---------- E. 步数默认不限制（0） ----------

func TestMaxStepsDefaultAndUpgrade(t *testing.T) {
	if maxStepsDefault != 0 {
		t.Fatalf("默认步数应为 0（不限制）, got %d", maxStepsDefault)
	}
	if got := DefaultConfig().MaxSteps; got != 0 {
		t.Fatalf("DefaultConfig().MaxSteps 应为 0（不限制）, got %d", got)
	}
	load := func(t *testing.T, raw string) (*Config, string) {
		t.Helper()
		p := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(p, []byte(raw), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := LoadConfig(p)
		if err != nil {
			t.Fatalf("读配置失败: %v", err)
		}
		saved, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return cfg, string(saved)
	}
	// 历次默认（30/100/200/1000＝旧输入上限，填它就是想把步数拉满）＝没人手动
	// 改过：迁到不限制，并立即落盘（设置页读数、配置文件、运行时三者一致）
	for _, legacy := range []int{30, 100, 200, 1000} {
		cfg, saved := load(t, fmt.Sprintf(`{"maxSteps":%d}`, legacy))
		if cfg.MaxSteps != 0 {
			t.Fatalf("旧默认 %d 应迁到不限制(0), got %d", legacy, cfg.MaxSteps)
		}
		if !strings.Contains(saved, `"maxSteps": 0`) {
			t.Fatalf("旧默认 %d 迁移后应立即落盘 maxSteps=0，实际文件: %s", legacy, saved)
		}
	}
	// 用户自己设过的上限不受影响
	for _, user := range []int{50, 350, 5000} {
		if cfg, _ := load(t, fmt.Sprintf(`{"maxSteps":%d}`, user)); cfg.MaxSteps != user {
			t.Fatalf("用户设定的 %d 不该被改, got %d", user, cfg.MaxSteps)
		}
	}
	// 缺项 / 0 / 负数：都按不限制
	for _, raw := range []string{`{}`, `{"maxSteps":0}`, `{"maxSteps":-5}`} {
		if cfg, _ := load(t, raw); cfg.MaxSteps != 0 {
			t.Fatalf("%s 应按不限制(0), got %d", raw, cfg.MaxSteps)
		}
	}
}

// ---------- F. 单条长消息完整读取 ----------

func TestReadSessionMessagesFullLongMessage(t *testing.T) {
	a := newCompactTestApp(t)
	sess := &SessionMeta{ID: "s1", Mode: "task"}
	if err := a.Store.Create(sess); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("长报告内容。", 1600) // 8000 字
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("assistant", long), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	if err := a.Store.AppendRow("s1", &StoredMessage{Message: TextMessage("user", "看完了"), Kind: kindChat}); err != nil {
		t.Fatal(err)
	}
	tk := &toolContext{app: a, sess: sess}
	head := "[1] assistant："
	total := len([]rune(head + long))

	// seq 单条读取：整条完整返回（旧实现固定截 3000 字）
	out, err := runReadSessionMessages(tk, json.RawMessage(`{"session_id":"s1","seq":1}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, long) {
		t.Fatalf("seq 读取应含整条正文（共 %d 字）", total)
	}
	if !strings.Contains(out, "本条共 "+strconv.Itoa(total)+" 字") {
		t.Fatalf("应写明本条共多少字：\n%.200s", out)
	}
	// 窗口读取：预算够时不截断
	win, err := runReadSessionMessages(tk, json.RawMessage(`{"session_id":"s1","from_seq":1,"chars":20000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(win, long) {
		t.Fatal("预算充足时窗口读取应给完整正文（加大 chars 必须有效）")
	}
	// 窗口读取：预算不足时填满预算并明示「本条共 X 字，已显示前 Y 字」+ 续读方式
	small, err := runReadSessionMessages(tk, json.RawMessage(`{"session_id":"s1","from_seq":1,"chars":1000}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(small, "本条共 "+strconv.Itoa(total)+" 字，已显示前") ||
		!strings.Contains(small, "from_char=") || !strings.Contains(small, "seq=1") {
		t.Fatalf("预算不足时应写明本条字数、已显示字数与续读方式：\n%s", small)
	}
	// from_char 续读：接着上次的位置读剩余部分
	tailOut, err := runReadSessionMessages(tk, json.RawMessage(`{"session_id":"s1","seq":1,"from_char":8000}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(strings.TrimSpace(tailOut), head+long[:20]) || !strings.Contains(tailOut, "本次显示第 8001–"+strconv.Itoa(total)+" 字") {
		t.Fatalf("from_char 应从中途往后续读：\n%.200s", tailOut)
	}
	if !strings.Contains(tailOut, string([]rune(long)[len([]rune(long))-10:])) {
		t.Fatalf("续读应包含本条末尾内容：\n%.200s", tailOut)
	}
}
