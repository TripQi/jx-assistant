package main

// 派发用途（purpose）与本次档位覆盖（thinking）的定向回归：
//   - 用途只决定默认模型与推理强度，不传＝执行/通用；未知用途报错且不建会话；
//   - 档位链：本次参数 > 用途 > 执行/通用 > 全局 > 未指定；none 是有效值不是「未传」；
//   - 用途 mode 脏值 / 自定义不全一律报错，不降级 follow、不建半会话；
//   - 待办、子对话与 GET 的 dispatch 键（老结构）不受影响；老配置等价于执行/通用。

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// addMockProvider 给测试 App 追加一个 mock 渠道（免密钥、不会真的发请求）。
func addMockProvider(a *App, id, model string) {
	a.Cfg.Providers = append(a.Cfg.Providers, ProviderCfg{
		ID: id, Name: id, BaseURL: "mock", Models: []ModelCfg{{ID: model}},
	})
}

// dispatchPurposeCase 一次派发并返回落库后的会话（失败由调用方判断）。
func dispatchPurposeCase(t *testing.T, a *App, main *SessionMeta, args map[string]any) (*SessionMeta, error) {
	t.Helper()
	if _, err := runDispatchTask(dispatchCtx(a, main), mustArgs(t, args)); err != nil {
		return nil, err
	}
	subs := otherSessions(t, a)
	if len(subs) != 1 {
		t.Fatalf("应恰好建出一个任务会话，实际 %d 个", len(subs))
	}
	m, err := a.Store.Get(subs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	stopThinkingTestSession(t, a, subs[0].ID)
	return m, nil
}

// 用途只决定用哪一套默认模型：执行/通用自定义 > （plan/review）跟随它 > 用途自定义；
// 显式 model 参数永远最优先。
func TestDispatchPurposeModelChain(t *testing.T) {
	executeCustom := func(a *App, _ *SessionMeta) {
		addMockProvider(a, "exec", "exec-model")
		a.Cfg.DispatchModel = ModelRef{Provider: "exec", Model: "exec-model"}
	}
	cases := []struct {
		name      string
		setup     func(a *App, main *SessionMeta)
		args      map[string]any
		wantProv  string
		wantModel string
	}{
		{
			name:     "不传 purpose＝执行/通用自定义",
			setup:    executeCustom,
			args:     map[string]any{"task": "x"},
			wantProv: "exec", wantModel: "exec-model",
		},
		{
			name: "执行/通用跟随主会话",
			setup: func(a *App, main *SessionMeta) {
				addMockProvider(a, "owner", "owner-model")
				main.Provider, main.Model = "owner", "owner-model"
			},
			args:     map[string]any{"task": "x"},
			wantProv: "owner", wantModel: "owner-model",
		},
		{
			name:     "执行/通用跟随主会话且主会话未手选→跟全局默认（空＝新会话跟随）",
			setup:    func(a *App, _ *SessionMeta) {},
			args:     map[string]any{"task": "x"},
			wantProv: "", wantModel: "",
		},
		{
			name:     "plan 出厂跟随执行/通用",
			setup:    executeCustom,
			args:     map[string]any{"task": "x", "purpose": "plan"},
			wantProv: "exec", wantModel: "exec-model",
		},
		{
			name: "plan 自定义配置独立生效",
			setup: func(a *App, _ *SessionMeta) {
				executeCustom(a, nil)
				addMockProvider(a, "planner", "planner-model")
				a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: purposeModeCustom,
					ModelRef: ModelRef{Provider: "planner", Model: "planner-model"}}
			},
			args:     map[string]any{"task": "x", "purpose": " PLan "},
			wantProv: "planner", wantModel: "planner-model",
		},
		{
			name: "review 自定义配置独立生效",
			setup: func(a *App, _ *SessionMeta) {
				executeCustom(a, nil)
				addMockProvider(a, "auditor", "auditor-model")
				a.Cfg.DispatchReviewModel = DispatchPurposeCfg{Mode: purposeModeCustom,
					ModelRef: ModelRef{Provider: "auditor", Model: "auditor-model"}}
			},
			args:     map[string]any{"task": "x", "purpose": "review"},
			wantProv: "auditor", wantModel: "auditor-model",
		},
		{
			name: "显式 model 参数压过用途配置",
			setup: func(a *App, _ *SessionMeta) {
				addMockProvider(a, "explicit", "explicit-model")
				a.Cfg.DispatchReviewModel = DispatchPurposeCfg{Mode: purposeModeCustom,
					ModelRef: ModelRef{Provider: "explicit", Model: "explicit-model"}}
				addMockProvider(a, "other", "other-model")
			},
			args:     map[string]any{"task": "x", "purpose": "review", "model": "other/other-model"},
			wantProv: "other", wantModel: "other-model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, main := newDispatchTestApp(t)
			tc.setup(a, main)
			got, err := dispatchPurposeCase(t, a, main, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.Provider != tc.wantProv || got.Model != tc.wantModel {
				t.Fatalf("落库模型应为 %q/%q，实际 %q/%q", tc.wantProv, tc.wantModel, got.Provider, got.Model)
			}
		})
	}
}

// 档位链：本次参数 > 本用途 > 执行/通用 > 全局 > 空；none 是有效档位。
func TestDispatchPurposeThinkingChain(t *testing.T) {
	cases := []struct {
		name       string
		planThink  string
		execThink  string
		globalThnk string
		args       map[string]any
		want       string
	}{
		{"本次参数最优先", "xhigh", "medium", "high", map[string]any{"task": "x", "thinking": "none"}, "none"},
		{"大小写空白归一", "xhigh", "medium", "high", map[string]any{"task": "x", "thinking": " High "}, "high"},
		{"空串沿链到用途档位", "max", "medium", "high", map[string]any{"task": "x", "purpose": "plan", "thinking": ""}, "max"},
		{"用途档位空则跟执行/通用", "", "medium", "high", map[string]any{"task": "x", "purpose": "review"}, "medium"},
		{"执行/通用档位空则跟全局", "", "", "high", map[string]any{"task": "x", "purpose": "plan"}, "high"},
		{"整条链都空＝不指定", "", "", "", map[string]any{"task": "x", "purpose": "plan"}, ""},
		{"不传 purpose 走执行/通用（等价旧链）", "", "low", "high", map[string]any{"task": "x"}, "low"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, main := newDispatchTestApp(t)
			a.Cfg.DefaultModel.Thinking = tc.globalThnk
			a.Cfg.DispatchModel.Thinking = tc.execThink
			a.Cfg.DispatchPlanModel = DispatchPurposeCfg{ModelRef: ModelRef{Thinking: tc.planThink}}
			got, err := dispatchPurposeCase(t, a, main, tc.args)
			if err != nil {
				t.Fatal(err)
			}
			if got.Thinking != tc.want {
				t.Fatalf("落库档位应为 %q，实际 %q", tc.want, got.Thinking)
			}
			// 本次覆盖/用途配置只影响新会话，不改任何默认设置。
			if a.Cfg.DispatchModel.Thinking != tc.execThink || a.Cfg.DefaultModel.Thinking != tc.globalThnk ||
				a.Cfg.DispatchPlanModel.Thinking != tc.planThink {
				t.Fatalf("派发改动了默认设置：%+v %+v %+v", a.Cfg.DefaultModel, a.Cfg.DispatchModel, a.Cfg.DispatchPlanModel)
			}
		})
	}
}

// 非法 purpose / thinking、脏 mode、自定义不全：全部在建会话之前拒绝，不留半会话。
func TestDispatchPurposeErrorsBeforeCreate(t *testing.T) {
	cases := []struct {
		name    string
		setup   func(a *App)
		args    map[string]any
		errMust []string
	}{
		{
			name:    "未知用途 planning",
			args:    map[string]any{"task": "x", "purpose": "planning"},
			errMust: []string{"purpose=\"planning\"", "plan"},
		},
		{
			name:    "中文别名不认",
			args:    map[string]any{"task": "x", "purpose": "方案"},
			errMust: []string{"purpose=\"方案\""},
		},
		{
			name:    "用途大小写归一到非法值仍拒绝",
			args:    map[string]any{"task": "x", "purpose": "AUDIT"},
			errMust: []string{"purpose=\"AUDIT\""},
		},
		{
			name:    "档位 enabled 不是六档",
			args:    map[string]any{"task": "x", "thinking": "enabled"},
			errMust: []string{"thinking=\"enabled\"", "none/low/medium/high/xhigh/max"},
		},
		{
			name:    "档位 ultra 非法",
			args:    map[string]any{"task": "x", "thinking": "ultra"},
			errMust: []string{"thinking=\"ultra\""},
		},
		{
			name: "用途 mode 脏值",
			setup: func(a *App) {
				a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: "跟随"}
			},
			args:    map[string]any{"task": "x", "purpose": "plan"},
			errMust: []string{"方案设计", "mode", "跟随", whereDispatchPlanDefault},
		},
		{
			name: "用途自定义但没选全",
			setup: func(a *App) {
				a.Cfg.DispatchReviewModel = DispatchPurposeCfg{Mode: purposeModeCustom,
					ModelRef: ModelRef{Provider: "mock-prov"}}
			},
			args:    map[string]any{"task": "x", "purpose": "review"},
			errMust: []string{"审查/审计", "没选全"},
		},
		{
			name: "用途配置渠道不可用",
			setup: func(a *App) {
				addMockProvider(a, "gone", "gone-model")
				a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: purposeModeCustom,
					ModelRef: ModelRef{Provider: "gone", Model: "gone-model"}}
				a.Cfg.Providers[len(a.Cfg.Providers)-1].Enabled = boolPtr(false)
			},
			args:    map[string]any{"task": "x", "purpose": "plan"},
			errMust: []string{"方案设计默认模型不可用", "手动停用"},
		},
		{
			// 用户拍板：所选用途的 mode 结构即使本次给了显式 model/thinking 覆盖也
			// 必须派发前报错（配置坏了不能静默放过）。
			name: "脏 mode 即使有显式 model 与 thinking 也拒绝",
			setup: func(a *App) {
				addMockProvider(a, "okp", "okm")
				a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: "跟随"}
			},
			args:    map[string]any{"task": "x", "purpose": "plan", "model": "okp/okm", "thinking": "none"},
			errMust: []string{"方案设计", "mode", "本次派发未创建任务"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, main := newDispatchTestApp(t)
			if tc.setup != nil {
				tc.setup(a)
			}
			_, err := runDispatchTask(dispatchCtx(a, main), mustArgs(t, tc.args))
			if err == nil {
				t.Fatal("应报错")
			}
			msg := err.Error()
			for _, sub := range tc.errMust {
				if !strings.Contains(msg, sub) {
					t.Fatalf("报错应含 %q：%v", sub, msg)
				}
			}
			if n := len(otherSessions(t, a)); n != 0 {
				t.Fatalf("拒绝必须发生在建会话之前，实际建出 %d 个", n)
			}
		})
	}
}

// 老配置（两个新键都是零值）与改动前完全等价：plan/review 与执行/通用同一条链。
func TestOldConfigPurposesFollowExecute(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	a.Cfg.DispatchModel = ModelRef{Provider: "router", Model: "claude-x"}
	ex, exSource, _, err := a.purposeRef("execute")
	if err != nil || ex.Provider != "router" || exSource != "执行/通用默认模型" {
		t.Fatalf("执行/通用应为自定义配置：%+v %q %v", ex, exSource, err)
	}
	for _, p := range []string{"plan", "review"} {
		ref, source, _, err := a.purposeRef(p)
		if err != nil || ref != ex || source != "跟随执行/通用" {
			t.Fatalf("%s 出厂应跟随执行/通用：%+v %q %v", p, ref, source, err)
		}
		level, _, err := a.purposeThinking(p, "")
		if err != nil || level != a.Cfg.DispatchModel.Thinking {
			t.Fatalf("%s 档位应跟执行/通用：%q %v", p, level, err)
		}
	}
	// 执行/通用也跟随主会话时，两个用途同样落到「再跟随主会话」（空引用）。
	a.Cfg.DispatchModel = ModelRef{}
	for _, p := range []string{"execute", "plan", "review"} {
		ref, _, where, err := a.purposeRef(p)
		if err != nil || !ref.Empty() || where != "" {
			t.Fatalf("%s 应跟随主会话：%+v %q %v", p, ref, where, err)
		}
	}
}

// 保存接口：mode 合法值接受、脏值 400 且不落盘不改运行值、null 清空、
// follow 清模型但保留档位、custom 不全 400。
func TestPurposeModeSaveValidation(t *testing.T) {
	a := defaultPatchApp(t)
	a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: purposeModeCustom,
		ModelRef: ModelRef{Provider: "zhipu", Model: "glm-5.3", Thinking: "medium"}}
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		t.Fatal(err)
	}
	diskPlan := func() DispatchPurposeCfg {
		raw, err := os.ReadFile(a.CfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var cfg struct {
			Plan DispatchPurposeCfg `json:"dispatchPlanModel"`
		}
		if err := json.Unmarshal(raw, &cfg); err != nil {
			t.Fatal(err)
		}
		return cfg.Plan
	}

	// mode 脏值：400、不落盘、不改运行值、不降级 follow
	before := a.Cfg.DispatchPlanModel
	beforeDisk := diskPlan()
	w := defaultPatch(a, `{"dispatchPlan":{"mode":"跟随"}}`)
	if w.Code != 400 || !strings.Contains(w.Body.String(), "方案设计") {
		t.Fatalf("脏 mode 应 400 并点名用途：%d %s", w.Code, w.Body.String())
	}
	if a.Cfg.DispatchPlanModel != before || diskPlan() != beforeDisk {
		t.Fatalf("脏 mode 不该改动配置：%+v %+v", a.Cfg.DispatchPlanModel, diskPlan())
	}

	// custom 不全：400（先清空再只给渠道，合并后模型缺失）
	if w = defaultPatch(a, `{"dispatchPlan":null}`); w.Code != 200 {
		t.Fatalf("清空应接受：%d %s", w.Code, w.Body.String())
	}
	if w = defaultPatch(a, `{"dispatchPlan":{"mode":"custom","provider":"router"}}`); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "没选全") {
		t.Fatalf("custom 不全应 400：%d %s", w.Code, w.Body.String())
	}
	if a.Cfg.DispatchPlanModel != (DispatchPurposeCfg{}) {
		t.Fatal("400 不该改动运行值")
	}

	// 完整 custom：接受，Key 归属补齐后落盘
	if w = defaultPatch(a, `{"dispatchPlan":{"mode":"custom","provider":"router","model":"claude-x","thinking":"high"}}`); w.Code != 200 {
		t.Fatalf("完整 custom 应接受：%d %s", w.Code, w.Body.String())
	}
	got := a.Cfg.DispatchPlanModel
	if got.Mode != purposeModeCustom || got.ProviderKey != "k2" || got.Thinking != "high" {
		t.Fatalf("自定义配置不对：%+v", got)
	}
	if diskPlan() != got {
		t.Fatalf("落盘与运行值不一致：%+v vs %+v", diskPlan(), got)
	}

	// 切回 follow：清模型三项、保留档位（档位与模型来源解耦）
	if w = defaultPatch(a, `{"dispatchPlan":{"mode":"follow"}}`); w.Code != 200 {
		t.Fatalf("follow 应接受：%d %s", w.Code, w.Body.String())
	}
	got = a.Cfg.DispatchPlanModel
	if got.Mode != "" || got.Provider != "" || got.Model != "" || got.Thinking != "high" {
		t.Fatalf("follow 应清模型留档位：%+v", got)
	}

	// null：整体清空（含档位）
	if w = defaultPatch(a, `{"dispatchPlan":null}`); w.Code != 200 {
		t.Fatalf("null 应清空：%d %s", w.Code, w.Body.String())
	}
	if got = a.Cfg.DispatchPlanModel; got.Mode != "" || got.Thinking != "" || got.Provider != "" {
		t.Fatalf("null 应整体清空：%+v", got)
	}
	if diskPlan() != got {
		t.Fatalf("清空结果应落盘：%+v vs %+v", diskPlan(), got)
	}

	// 键缺席：不动（同时证明只动提交的那一项）
	review := DispatchPurposeCfg{Mode: purposeModeCustom, ModelRef: ModelRef{Provider: "zhipu", Model: "glm-5.3-flash"}}
	a.Cfg.DispatchReviewModel = review
	if w = defaultPatch(a, `{"dispatchPlan":null}`); w.Code != 200 || a.Cfg.DispatchReviewModel != review {
		t.Fatalf("缺席的用途不该被改动：%d %+v", w.Code, a.Cfg.DispatchReviewModel)
	}
}

// GET 视图：dispatchPurposes 三条、dispatch 老结构三键不变、脏 mode 原样回并给原因。
func TestDefaultModelsPurposeView(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	a.Cfg.DispatchModel = ModelRef{Provider: "router", Model: "claude-x"}
	get := func() (map[string]json.RawMessage, []struct {
		Purpose string       `json:"purpose"`
		Label   string       `json:"label"`
		Mode    string       `json:"mode"`
		Ref     ModelRefView `json:"ref"`
	}) {
		t.Helper()
		w := httptest.NewRecorder()
		a.handleDefaultModelsGet(w, httptest.NewRequest("GET", "/api/default-models", nil))
		if w.Code != 200 {
			t.Fatalf("GET 状态 %d", w.Code)
		}
		var body struct {
			Dispatch         map[string]json.RawMessage `json:"dispatch"`
			DispatchPurposes []struct {
				Purpose string       `json:"purpose"`
				Label   string       `json:"label"`
				Mode    string       `json:"mode"`
				Ref     ModelRefView `json:"ref"`
			} `json:"dispatchPurposes"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body.Dispatch, body.DispatchPurposes
	}

	dispatch, purposes := get()
	// 待办组件在读 dispatch：形状必须是 mode/thinking/ref 三键，不多不少。
	if len(dispatch) != 3 {
		t.Fatalf("dispatch 视图结构变了：%v", dispatch)
	}
	for _, k := range []string{"mode", "thinking", "ref"} {
		if _, ok := dispatch[k]; !ok {
			t.Fatalf("dispatch 缺键 %s：%v", k, dispatch)
		}
	}
	if len(purposes) != 3 || purposes[0].Purpose != "execute" || purposes[1].Purpose != "plan" || purposes[2].Purpose != "review" {
		t.Fatalf("dispatchPurposes 应固定三条：%+v", purposes)
	}
	if purposes[1].Mode != "follow" || purposes[1].Ref.Source != "跟随执行/通用" ||
		purposes[1].Ref.Provider != "router" || purposes[1].Ref.Model != "claude-x" {
		t.Fatalf("plan 跟随态生效值算错了：%+v", purposes[1])
	}
	if purposes[2].Label != "审查/审计" || purposes[2].Mode != "follow" {
		t.Fatalf("review 行不对：%+v", purposes[2])
	}

	// 方案设计的独立档位：跟随时也要显示自己的档位链（P7：去掉这行文本就红）
	a.Cfg.DispatchModel.Thinking = "low"
	a.Cfg.DispatchPlanModel = DispatchPurposeCfg{ModelRef: ModelRef{Thinking: "xhigh"}}
	_, purposes = get()
	if !strings.Contains(purposes[1].Ref.ThinkingText, "方案设计默认模型 · Xhigh") {
		t.Fatalf("方案设计行的档位文本应来自自己的链：%+v", purposes[1].Ref)
	}
	if !strings.Contains(purposes[1].Ref.Text, "Xhigh") {
		t.Fatalf("方案设计行的生效值旧该带自己的档位：%+v", purposes[1].Ref)
	}
	// 自己没配档位就跟执行/通用（不是直接跟全局）
	a.Cfg.DispatchPlanModel = DispatchPurposeCfg{}
	_, purposes = get()
	if !strings.Contains(purposes[1].Ref.ThinkingText, "跟随执行/通用 · Low") {
		t.Fatalf("方案设计行档位应跟随执行/通用：%+v", purposes[1].Ref)
	}

	// 脏 mode：原样返回配置值 + ref 说明非法（界面据此置红）
	a.Cfg.DispatchReviewModel = DispatchPurposeCfg{Mode: "Custom2"}
	_, purposes = get()
	if purposes[2].Mode != "Custom2" {
		t.Fatalf("脏 mode 应原样返回：%+v", purposes[2])
	}
	if purposes[2].Ref.Error == "" || !strings.Contains(purposes[2].Ref.Error, "mode 值非法") {
		t.Fatalf("脏 mode 应给出原因：%+v", purposes[2].Ref)
	}
	// 脏配置不因读取被改写
	if a.Cfg.DispatchReviewModel.Mode != "Custom2" {
		t.Fatal("读取不该改写配置")
	}
}

// 用途配置不渗漏：待办仍按执行/通用三档链，子对话仍继承派发者（模型与档位都不读用途）。
func TestPurposeConfigDoesNotLeak(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	a.Cfg.DispatchModel = ModelRef{Provider: "router", Model: "claude-x"}
	a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: purposeModeCustom,
		ModelRef: ModelRef{Provider: "zhipu", Model: "glm-5.3-flash", Thinking: "xhigh"}}
	a.Cfg.DispatchReviewModel = DispatchPurposeCfg{Mode: purposeModeCustom,
		ModelRef: ModelRef{Provider: "zhipu", Model: "glm-5.3", Thinking: "max"}}

	ref, source, err := a.todoModelChain(todoModelFollowDispatch, ModelRef{})
	if err != nil || ref.Provider != "router" || ref.Model != "claude-x" || source != "派发任务默认模型" {
		t.Fatalf("待办应仍取执行/通用：%+v %q %v", ref, source, err)
	}
	if level, _, err := a.defaultThinking("todo", ""); err != nil || level != "" {
		t.Fatalf("待办档位不该被用途档位影响：%q %v", level, err)
	}

	// 子对话（真派发）：模型与档位都随派发者，plan/review 的自定义配置一边不动。
	a2, main := newDispatchTestApp(t)
	main.Provider, main.Model, main.Thinking = "mock-prov", "mock-model", "high"
	addMockProvider(a2, "planner", "planner-model")
	a2.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: purposeModeCustom,
		ModelRef: ModelRef{Provider: "planner", Model: "planner-model", Thinking: "xhigh"}}
	if _, err := runDispatchSub(dispatchCtx(a2, main), mustArgs(t, map[string]any{"task": "child"})); err != nil {
		t.Fatal(err)
	}
	subs := otherSessions(t, a2)
	if len(subs) != 1 {
		t.Fatalf("子对话数 %d", len(subs))
	}
	stored, err := a2.Store.Get(subs[0].ID)
	stopThinkingTestSession(t, a2, subs[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Thinking != "high" {
		t.Fatalf("子对话档位应继承派发者：%q", stored.Thinking)
	}
	if stored.Provider != "mock-prov" || stored.Model != "mock-model" {
		t.Fatalf("子对话模型应继承派发者：%q/%q", stored.Provider, stored.Model)
	}
}

// 保存响应里也要带上 dispatchPurposes（前端保存后直接拿它重渲染）。
func TestDefaultModelsSetReturnsPurposeView(t *testing.T) {
	a := defaultPatchApp(t)
	w := defaultPatch(a, `{"dispatchReview":{"mode":"follow","thinking":"high"}}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var body struct {
		Purposes []struct {
			Purpose  string `json:"purpose"`
			Thinking string `json:"thinking"`
			Mode     string `json:"mode"`
		} `json:"dispatchPurposes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Purposes) != 3 || body.Purposes[2].Thinking != "high" || body.Purposes[2].Mode != "follow" {
		t.Fatalf("保存响应应带三套用途视图：%+v", body.Purposes)
	}
}

// 保存接口的逐字段合并语义（P1/P2）：
//   - mode 缺席时跟随态只给模型字段＝400（不静默丢弃），custom 态可按字段换模型；
//   - {} 与 null 都是整体清空，thinking 单字段补丁可独立使用；
//   - 失败响应说明整份未保存（原子保存）。
func TestPurposeMergeSemantics(t *testing.T) {
	a := defaultPatchApp(t)
	a.Cfg.DispatchPlanModel = DispatchPurposeCfg{} // 起点：跟随、无档位
	if w := defaultPatch(a, `{"dispatchPlan":{"provider":"router","model":"claude-x"}}`); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "跟随执行/通用") ||
		!strings.Contains(w.Body.String(), "本次保存未生效") {
		t.Fatalf("跟随态只给模型字段应 400 并说明整份未保存：%d %s", w.Code, w.Body.String())
	}
	if a.Cfg.DispatchPlanModel != (DispatchPurposeCfg{}) {
		t.Fatalf("拒绝不该改动配置：%+v", a.Cfg.DispatchPlanModel)
	}
	// 跟随态只给档位：可以（档位与模型来源解耦）
	if w := defaultPatch(a, `{"dispatchPlan":{"thinking":"xhigh"}}`); w.Code != 200 {
		t.Fatalf("跟随时只给档位应接受：%d %s", w.Code, w.Body.String())
	}
	if got := a.Cfg.DispatchPlanModel; got.Mode != "" || got.Thinking != "xhigh" {
		t.Fatalf("跟随时档位应独立生效：%+v", got)
	}
	// custom 后 mode 缺席只换模型：按字段合并（不必重报 mode）
	if w := defaultPatch(a, `{"dispatchPlan":{"mode":"custom","provider":"router","model":"claude-x"}}`); w.Code != 200 {
		t.Fatalf("完整 custom 应接受：%d %s", w.Code, w.Body.String())
	}
	if w := defaultPatch(a, `{"dispatchPlan":{"model":"shared"}}`); w.Code != 200 {
		t.Fatalf("custom 态只换模型应接受：%d %s", w.Code, w.Body.String())
	}
	if got := a.Cfg.DispatchPlanModel; got.Mode != purposeModeCustom || got.Model != "shared" || got.ProviderKey != "k1" || got.Thinking != "xhigh" {
		t.Fatalf("custom 态换模型应保留其余字段：%+v", got)
	}
	// thinking 单字段补丁不动模型
	if w := defaultPatch(a, `{"dispatchPlan":{"thinking":"low"}}`); w.Code != 200 {
		t.Fatalf("档位补丁应接受：%d %s", w.Code, w.Body.String())
	}
	if got := a.Cfg.DispatchPlanModel; got.Model != "shared" || got.Thinking != "low" {
		t.Fatalf("档位补丁不该动模型：%+v", got)
	}
	// {} 与 null 都整体清空（与旧的 dispatch:{} 语义统一）
	if w := defaultPatch(a, `{"dispatchPlan":{}}`); w.Code != 200 {
		t.Fatalf("{} 应整体清空：%d %s", w.Code, w.Body.String())
	}
	if got := a.Cfg.DispatchPlanModel; got != (DispatchPurposeCfg{}) {
		t.Fatalf("{} 应清空模型与档位：%+v", got)
	}
	a.Cfg.DispatchPlanModel = DispatchPurposeCfg{Mode: purposeModeCustom, ModelRef: ModelRef{Provider: "router", Model: "claude-x", Thinking: "high"}}
	if w := defaultPatch(a, `{"dispatchPlan":null}`); w.Code != 200 {
		t.Fatalf("null 应整体清空：%d %s", w.Code, w.Body.String())
	}
	if got := a.Cfg.DispatchPlanModel; got != (DispatchPurposeCfg{}) {
		t.Fatalf("null 应清空：%+v", got)
	}
	// 顶层键缺席：其它项不动
	review := DispatchPurposeCfg{Mode: purposeModeCustom, ModelRef: ModelRef{Provider: "zhipu", Model: "glm-5.3"}}
	a.Cfg.DispatchReviewModel = review
	if w := defaultPatch(a, `{"dispatchPlan":{}}`); w.Code != 200 || a.Cfg.DispatchReviewModel != review {
		t.Fatalf("缺席的用途不该被改动：%d %+v", w.Code, a.Cfg.DispatchReviewModel)
	}
}

// 显式覆盖不受「本次用不到的默认模型不可用」拖累（P5 的另一半）：执行/通用指向
// 停用渠道、审查配置也坏着，显式 model+thinking 的 plan 派发照样成功。
func TestExplicitOverrideBypassesUnusedDefaults(t *testing.T) {
	a, main := newDispatchTestApp(t)
	addMockProvider(a, "okp", "okm")
	a.Cfg.Providers[0].Enabled = boolPtr(false) // 全局默认渠道停用
	a.Cfg.DispatchModel = ModelRef{Provider: "mock-prov", Model: "mock-model"}
	a.Cfg.DispatchReviewModel = DispatchPurposeCfg{Mode: purposeModeCustom,
		ModelRef: ModelRef{Provider: "mock-prov", Model: "mock-model"}}
	got, err := dispatchPurposeCase(t, a, main, map[string]any{"task": "x", "purpose": "plan", "model": "okp/okm", "thinking": "none"})
	if err != nil {
		t.Fatalf("显式覆盖应不受未使用默认拖累：%v", err)
	}
	if got.Provider != "okp" || got.Model != "okm" || got.Thinking != "none" {
		t.Fatalf("显式覆盖落库不对：%+v", got)
	}
}

// 派发注记：用途与本次档位只在非默认时出现（P6）：直接断言这两项，注记丢了就红。
func TestDispatchTaskNotePurposeAndThinking(t *testing.T) {
	cases := []struct {
		name, purpose, thinking string
		want, notWant           string
	}{
		{"都默认＝空串", "execute", "", "", "用途"},
		{"方案设计", "plan", "", "用途：「方案设计」（plan）", ""},
		{"审查+指定档位", "review", "none", "档位：「本次指定 None」", ""},
		{"转录+高档位", "plan", "max", "档位：「本次指定 Max」", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := dispatchTaskNote("task", permWorkspace, "", false, 0, "", tc.purpose, tc.thinking)
			if tc.want != "" && !strings.Contains(got, tc.want) {
				t.Fatalf("注记应含 %q：%q", tc.want, got)
			}
			if tc.notWant != "" && strings.Contains(got, tc.notWant) {
				t.Fatalf("注记不该含 %q：%q", tc.notWant, got)
			}
			if tc.want == "" && got != "" {
				t.Fatalf("全默认时应返回空串：%q", got)
			}
		})
	}
	// execute + 指定档位：只出现档位，不出现用途
	got := dispatchTaskNote("task", permWorkspace, "", false, 0, "", "execute", "high")
	if !strings.Contains(got, "档位：「本次指定 High」") || strings.Contains(got, "用途：") {
		t.Fatalf("execute 指定档位时注记不对：%q", got)
	}
}
