package main

// 默认档位独立于模型指向；所有持久化断言均用临时 Store，不启动执行体或请求 API。
import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDefaultThinkingValidation(t *testing.T) {
	for _, level := range []string{"", "none", "low", "medium", "high", "xhigh", "max"} {
		if err := validateDefaultThinking(level); err != nil {
			t.Errorf("合法默认档位 %q: %v", level, err)
		}
	}
	for _, bad := range []string{"enabled", "disabled", "garbage", "High", " high "} {
		if err := validateDefaultThinking(bad); err == nil {
			t.Errorf("默认体系必须严格拒绝 %q", bad)
		}
	}
	// 默认体系变严格不能顺手删除旧会话的兼容。
	for old, want := range map[string]string{"enabled": "", "disabled": "none"} {
		if got, ok := normalizeThinkingLevel(old); !ok || got != want {
			t.Errorf("旧会话兼容 %q = %q, %v；want %q", old, got, ok, want)
		}
	}
}

func TestDefaultThinkingPriority(t *testing.T) {
	a := newDefaultModelsApp(t)
	cases := []struct {
		name, kind, own, dispatch, global, want, source string
	}{
		{"empty-global", "global", "", "", "", "", "跟随会话默认"},
		{"empty-dispatch", "dispatch", "", "", "", "", "跟随会话默认"},
		{"empty-todo", "todo", "", "", "", "", "跟随会话默认"},
		{"manual-skips-dispatch", "global", "", "medium", "high", "high", "跟随全局默认"},
		{"manual-stays-empty", "global", "", "medium", "", "", "跟随会话默认"},
		{"dispatch-global", "dispatch", "", "", "high", "high", "跟随全局默认"},
		{"dispatch-own-setting", "dispatch", "", "medium", "high", "medium", "跟随派发设置"},
		{"todo-global", "todo", "", "", "high", "high", "跟随全局默认"},
		{"todo-dispatch", "todo", "", "medium", "high", "medium", "跟随派发设置"},
		{"todo-own", "todo", "xhigh", "medium", "high", "xhigh", ""},
		{"own-wins", "dispatch", "none", "max", "high", "none", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a.Cfg.DefaultModel.Thinking, a.Cfg.DispatchModel.Thinking = tc.global, tc.dispatch
			got, source, err := a.defaultThinking(tc.kind, tc.own)
			if err != nil || got != tc.want || source != tc.source {
				t.Fatalf("defaultThinking(%q,%q) = %q,%q,%v; want %q,%q", tc.kind, tc.own, got, source, err, tc.want, tc.source)
			}
		})
	}
	for _, level := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
		a.Cfg.DefaultModel.Thinking, a.Cfg.DispatchModel.Thinking = level, ""
		for _, kind := range []string{"global", "dispatch", "todo"} {
			if got, _, err := a.defaultThinking(kind, ""); err != nil || got != level {
				t.Errorf("%s 继承六档 %s: %q %v", kind, level, got, err)
			}
		}
	}
}

func TestDefaultThinkingInvalidNeverFallsBack(t *testing.T) {
	a := newDefaultModelsApp(t)
	for _, bad := range []string{"enabled", "disabled", "garbage"} {
		for _, kind := range []string{"global", "dispatch", "todo"} {
			a.Cfg.DefaultModel.Thinking, a.Cfg.DispatchModel.Thinking = "high", "medium"
			if _, _, err := a.defaultThinking(kind, bad); err == nil {
				t.Errorf("%s own=%q 不应静默回落", kind, bad)
			}
		}
		a.Cfg.DefaultModel.Thinking, a.Cfg.DispatchModel.Thinking = "high", bad
		for _, kind := range []string{"dispatch", "todo"} {
			if _, _, err := a.defaultThinking(kind, ""); err == nil {
				t.Errorf("%s 派发设置=%q 不应回落全局", kind, bad)
			}
		}
		// 非生效分支上的非法配置不应污染更高优先级的合法档位。
		if got, _, err := a.defaultThinking("todo", "max"); err != nil || got != "max" {
			t.Errorf("显式档位不应被低优先级阻断: %q %v", got, err)
		}
		a.Cfg.DefaultModel.Thinking, a.Cfg.DispatchModel.Thinking = bad, ""
		for _, kind := range []string{"global", "dispatch", "todo"} {
			if _, _, err := a.defaultThinking(kind, ""); err == nil {
				t.Errorf("%s 全局设置=%q 不应被吞掉", kind, bad)
			}
		}
	}
}

func TestDefaultThinkingModelRefJSON(t *testing.T) {
	base := ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	raw, err := json.Marshal(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"provider":"zhipu","model":"glm-5.3"}` {
		t.Fatalf("未配档位的 ModelRef 序列化必须逐字节保持旧格式: %s", raw)
	}
	if strings.Contains(string(raw), "thinking") {
		t.Fatalf("老配置空档位必须 omitempty: %s", raw)
	}
	for _, level := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
		ref := base
		ref.Thinking = level
		raw, err = json.Marshal(ref)
		if err != nil {
			t.Fatal(err)
		}
		var got ModelRef
		if err := json.Unmarshal(raw, &got); err != nil || got != ref {
			t.Fatalf("JSON 档位往返失败: %s %+v %v", raw, got, err)
		}
	}
	// 仅设档位不能把「跟随主会话」误判为自选模型。
	onlyThinking := ModelRef{Thinking: "medium"}
	if !onlyThinking.Empty() || onlyThinking.Complete() {
		t.Fatal("Thinking 不应改变模型 Empty/Complete 语义")
	}
	if got := (ModelRef{Provider: " zhipu ", Model: " glm-5.3 ", Thinking: " high "}).norm(); got.Thinking != " high " || got.Provider != "zhipu" {
		t.Fatalf("norm 丢失档位: %+v", got)
	}
}

func thinkingCreateHTTP(t *testing.T, a *App, body string) *SessionMeta {
	t.Helper()
	w := httptest.NewRecorder()
	a.handleCreateSession(w, httptest.NewRequest("POST", "/api/sessions", strings.NewReader(body)))
	if w.Code != 200 {
		t.Fatalf("HTTP create: %d %s", w.Code, w.Body.String())
	}
	var made SessionMeta
	if err := json.Unmarshal(w.Body.Bytes(), &made); err != nil {
		t.Fatal(err)
	}
	got, err := a.Store.Get(made.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestDefaultThinkingHTTPCreateAndExistingSession(t *testing.T) {
	a, parent := newDispatchTestApp(t)
	// 老、新会话在完全不配置档位时都保持空，不擅自补 High。
	if got, err := a.Store.Get(parent.ID); err != nil || got.Thinking != "" {
		t.Fatalf("老会话空档位改变: %+v %v", got, err)
	}
	if got := thinkingCreateHTTP(t, a, `{}`); got.Thinking != "" {
		t.Fatalf("未指定新会话应保留空: %+v", got)
	}
	a.Cfg.DispatchModel.Thinking = "medium"
	for _, level := range []string{"none", "low", "medium", "high", "xhigh", "max"} {
		a.Cfg.DefaultModel.Thinking = level
		got := thinkingCreateHTTP(t, a, `{}`)
		if got.Thinking != level {
			t.Errorf("手工 create 应取全局 %s 而不是派发档: %+v", level, got)
		}
	}
	if got, err := a.Store.Get(parent.ID); err != nil || got.Thinking != "" {
		t.Fatalf("修改默认不得回填老会话: %+v %v", got, err)
	}
	a.Cfg.DefaultModel.Thinking = "high"
	if got := thinkingCreateHTTP(t, a, `{"thinking":"none"}`); got.Thinking != "none" {
		t.Fatalf("人工显式 none 必须压过全局: %+v", got)
	}
	// 克隆按父会话档位，不按全局/派发默认。
	parent.Thinking = "low"
	if err := a.Store.SaveMeta(parent); err != nil {
		t.Fatal(err)
	}
	if got := thinkingCreateHTTP(t, a, `{"parent":"main","inheritContext":true}`); got.Thinking != "low" {
		t.Fatalf("克隆应继承父档位: %+v", got)
	}
}

func TestDefaultThinkingCreateLegacyCompatibility(t *testing.T) {
	a := newDefaultModelsApp(t)
	for old, want := range map[string]string{"": "", "enabled": "", "disabled": "none"} {
		made, err := a.createSession(createSessionParams{Thinking: old})
		if err != nil {
			t.Fatal(err)
		}
		got, err := a.Store.Get(made.ID)
		if err != nil || got.Thinking != want {
			t.Fatalf("会话兼容 %q: %+v %v", old, got, err)
		}
	}
}

func TestDefaultThinkingSettingsHTTPStrictAndPreserve(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.CfgPath = filepath.Join(t.TempDir(), "defaults.json")
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3", Thinking: "high"}
	for _, level := range []string{"", "none", "low", "medium", "high", "xhigh", "max"} {
		w := defaultPatch(a, `{"dispatch":{"thinking":"`+level+`"}}`)
		if w.Code != 200 || a.Cfg.DispatchModel.Thinking != level || a.Cfg.DefaultModel.Thinking != "high" {
			t.Fatalf("thinking-only 派发设置保存失败或误清全局: %d %s", w.Code, w.Body.String())
		}
		if _, custom := a.dispatchDefaultRef(); custom {
			t.Fatal("只设派发档位仍应跟随主会话模型")
		}
	}
	for _, field := range []string{"global", "dispatch"} {
		for _, bad := range []string{"enabled", "disabled", "garbage"} {
			g, d := a.Cfg.DefaultModel, a.Cfg.DispatchModel
			w := defaultPatch(a, `{"`+field+`":{"provider":"zhipu","model":"glm-5.3","thinking":"`+bad+`"}}`)
			if w.Code != 400 || a.Cfg.DefaultModel != g || a.Cfg.DispatchModel != d {
				t.Fatalf("非法默认档位必须 400 且不改配置: %s %s %d %s", field, bad, w.Code, w.Body.String())
			}
		}
	}
}

func TestDefaultThinkingViews(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3", Thinking: "xhigh"}
	a.Cfg.DispatchModel = ModelRef{Provider: "router", ProviderKey: "k2", Model: "claude-x"}
	assertText := func(name string, v ModelRefView, want string) {
		t.Helper()
		if !v.Available || !strings.Contains(v.Text, want) {
			t.Errorf("%s 展示应含 %q: %+v", name, want, v)
		}
	}
	assertText("global explicit", a.globalSettingsView(), " · Xhigh")
	a.Cfg.DefaultModel.Thinking = "high"
	assertText("dispatch global", a.dispatchSettingsView()["ref"].(ModelRefView), "（跟随全局默认 · High）")
	assertText("todo global", a.todoModelView(todoModelFollowDispatch, ModelRef{}), "（跟随全局默认 · High）")
	a.Cfg.DispatchModel.Thinking = "medium"
	assertText("todo dispatch", a.todoModelView(todoModelFollowDispatch, ModelRef{}), "（跟随派发设置 · Medium）")
	assertText("todo own", a.todoModelView(todoModelCustom, ModelRef{Provider: "zhipu", Model: "glm-5.3", Thinking: "xhigh"}), " · Xhigh")
	// 模型跟随但档位自选时，也要展示派发档位，不得偷带全局 Thinking。
	a.Cfg.DispatchModel = ModelRef{Thinking: "medium"}
	assertText("dispatch follow model", a.dispatchSettingsView()["ref"].(ModelRefView), " · Medium")
	a.Cfg.DefaultModel.Thinking, a.Cfg.DispatchModel.Thinking = "", ""
	assertText("global empty", a.globalSettingsView(), "（跟随会话默认 · 不指定）")
	assertText("dispatch empty", a.dispatchSettingsView()["ref"].(ModelRefView), "（跟随会话默认 · 不指定）")
	assertText("todo empty", a.todoModelView(todoModelFollowDispatch, ModelRef{}), "（跟随会话默认 · 不指定）")
}
