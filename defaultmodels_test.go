package main

// 默认模型体系（defaultmodels.go）的测试：
//   - 老配置的裸模型名迁移成「渠道 + 模型 + Key 归属」，多个渠道都能提供时取
//     配置里第一个（并在日志里写明），定位不到就留空；用户显式清空过不复活
//   - 三类默认（全局 / 派发任务 / 待办任务模型）的取值链
//   - 每一环取到的值不可用就报错，不回落（渠道禁用 / 模型不在清单 / Key 为空）
//   - 待办「任务模型」列的读写、数据版本迁移与派发时取值
//   - 空 Cfg.Model 的老配置加载不炸

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newDefaultModelsApp 建一个带临时库的 App，配两个「可用」渠道：一个单 Key
// （BaseURL=mock 免密钥），一个多 Key（Key 归属要写准）。
func newDefaultModelsApp(t *testing.T) *App {
	t.Helper()
	a := newCompactTestApp(t)
	a.initRuntime()
	a.sessLocks = map[string]*sync.Mutex{}
	a.Cfg.Workspace = t.TempDir()
	a.Cfg.Providers = []ProviderCfg{
		{ID: "zhipu", Name: "智谱", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}, {ID: "glm-5.3-flash"}}},
		{ID: "router", Name: "中转站", BaseURL: "mock", Keys: []ProviderKeyCfg{
			{ID: "k1", APIKey: "sk-a", Models: []ModelCfg{{ID: "shared"}}},
			{ID: "k2", APIKey: "sk-b", Models: []ModelCfg{{ID: "shared"}, {ID: "claude-x"}}},
		}},
	}
	return a
}

// ---------- 迁移 ----------

// 裸模型名 → 完整格式：多渠道都能提供时取配置里第一个并在日志里写明；多 Key
// 渠道要落到真正提供它的那把 Key 上；一个都定位不到就留空。
func TestMigrateDefaultModel(t *testing.T) {
	// 多 Key 渠道：只有第二把 Key 提供它 → Key 归属必须是 k2
	cfg := &Config{Model: "claude-x", Providers: []ProviderCfg{multiKeyProvider()}}
	migrated, note := migrateDefaultModel(cfg)
	if !migrated {
		t.Fatal("有裸模型名时应执行迁移")
	}
	if dm := cfg.DefaultModel; dm.Provider != "custom-1" || dm.ProviderKey != "k2" || dm.Model != "claude-x" {
		t.Fatalf("迁移结果不对: %+v", dm)
	}
	if !strings.Contains(note, "custom-1/claude-x") {
		t.Fatalf("迁移日志应写明落点: %s", note)
	}

	// 多个渠道都能提供：取配置里第一个，日志写明有几个渠道
	cfg = &Config{Model: "glm-5.3", Providers: []ProviderCfg{
		{ID: "a", Name: "A", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}}},
		{ID: "b", Name: "B", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}}},
	}}
	if _, note = migrateDefaultModel(cfg); cfg.DefaultModel.Provider != "a" {
		t.Fatalf("多渠道应取配置里第一个: %+v", cfg.DefaultModel)
	}
	if !strings.Contains(note, "2 个渠道") || !strings.Contains(note, "b/glm-5.3") {
		t.Fatalf("迁移日志应写明多个渠道: %s", note)
	}

	// 定位不到：留空（设置页据此显示「未设置」）
	cfg = &Config{Model: "no-such-model", Providers: []ProviderCfg{
		{ID: "a", BaseURL: "mock", Models: []ModelCfg{{ID: "glm-5.3"}}},
	}}
	if _, note = migrateDefaultModel(cfg); !cfg.DefaultModel.Empty() {
		t.Fatalf("定位不到应留空: %+v", cfg.DefaultModel)
	}
	if !strings.Contains(note, "留空") {
		t.Fatalf("应写明留空: %s", note)
	}

	// 空模型名（老配置没写过/用户清空过）：留空、不炸
	if m, note := migrateDefaultModel(&Config{}); !m || !strings.Contains(note, "留空") {
		t.Fatalf("空模型名应留空: %v %s", m, note)
	}
}

// LoadConfig 端到端：配置文件里没有 defaultModel 这一键就迁移并立即落盘；
// 有这一键（哪怕存的是空值＝用户显式清空）就不再复活。
func TestLoadConfigMigratesDefaultModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	old := `{"model":"gpt-x","providers":[{"id":"p1","name":"P1","baseURL":"mock","apiKey":"sk-1","models":[{"id":"gpt-x"}]}]}`
	if err := os.WriteFile(path, []byte(old), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if dm := cfg.DefaultModel; dm.Provider != "p1" || dm.Model != "gpt-x" {
		t.Fatalf("裸模型名应迁移成完整格式: %+v", dm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) != nil {
		t.Fatal("迁移后配置文件应是合法 JSON")
	}
	if _, has := probe["defaultModel"]; !has {
		t.Fatal("迁移结果应立即落盘（配置文件里要出现 defaultModel 键）")
	}

	// 用户显式清空过：再加载不复活
	cleared := `{"model":"gpt-x","defaultModel":{},"providers":[{"id":"p1","name":"P1","baseURL":"mock","apiKey":"sk-1","models":[{"id":"gpt-x"}]}]}`
	if err := os.WriteFile(path, []byte(cleared), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err = LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.DefaultModel.Empty() {
		t.Fatalf("用户清空过的默认模型不该被迁移复活: %+v", cfg.DefaultModel)
	}
}

// 空 Cfg.Model 的老配置（没跑过向导）：加载不炸，全局默认留空，应用会话默认时
// 报「还没设置」而不是拿别的渠道顶。
func TestEmptyLegacyModelDoesNotBreak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.json")
	if err := os.WriteFile(path, []byte(`{"providers":[{"id":"p1","baseURL":"mock","apiKey":"sk-1","models":[{"id":"m1"}]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("老配置加载不该失败: %v", err)
	}
	if !cfg.DefaultModel.Empty() {
		t.Fatalf("没有可定位的模型时应留空: %+v", cfg.DefaultModel)
	}
	a := &App{Cfg: cfg}
	if _, _, _, err := a.applySessionModelDefault("", "", ""); err == nil || !strings.Contains(err.Error(), "全局默认模型") {
		t.Fatalf("未设置全局默认时应报错指路: %v", err)
	}
}

// ---------- 取值链 ----------

// 手动新建会话：空 → 全局默认模型；显式指定且可用 → 原样；不可用 → 报错不回落。
func TestManualSessionModelChain(t *testing.T) {
	a := newDefaultModelsApp(t)

	// 没设置全局默认：报错并指路设置页
	if _, _, _, err := a.applySessionModelDefault("", "", ""); err == nil ||
		!strings.Contains(err.Error(), "全局默认模型") || !strings.Contains(err.Error(), "设置 → 模型") {
		t.Fatalf("未设置全局默认应报错指路: %v", err)
	}

	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	p, k, m, err := a.applySessionModelDefault("", "", "")
	if err != nil || p != "zhipu" || m != "glm-5.3" || k != "" {
		t.Fatalf("空选择应落到全局默认: %q %q %q %v", p, k, m, err)
	}

	// 显式指定可用模型：原样落到会话
	if p, _, m, err = a.applySessionModelDefault("router", "k2", "claude-x"); err != nil || p != "router" || m != "claude-x" {
		t.Fatalf("显式指定应原样: %q %q %v", p, m, err)
	}

	// 全局默认的渠道被停用：报错点名渠道与原因，不换别的渠道
	a.Cfg.Providers[0].Enabled = boolPtr(false)
	_, _, _, err = a.applySessionModelDefault("", "", "")
	if err == nil || !strings.Contains(err.Error(), "智谱") || !strings.Contains(err.Error(), "手动停用") {
		t.Fatalf("停用渠道应报错点名: %v", err)
	}
}

// 派发任务默认模型：指定 → 用它；跟随主会话（默认值/空）→ 维持原行为（继承派发者，
// 派发者也没手选就跟全局默认）；任一环不可用即报错。
func TestDispatchModelChain(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	main := &SessionMeta{ID: "main", Mode: "assistant", Provider: "zhipu", Model: "glm-5.3-flash"}
	tc := dispatchCtx(a, main)

	// 默认（没设置派发默认模型）＝跟随主会话：继承派发者手选
	p, k, m, err := tc.dispatchModelChoice("")
	if err != nil || p != "zhipu" || m != "glm-5.3-flash" || k != "" {
		t.Fatalf("空设置时应跟随主会话: %q %q %q %v", p, k, m, err)
	}

	// 设置「指定渠道+模型」：派发用它（连 Key 归属一起落）
	a.Cfg.DispatchModel = ModelRef{Provider: "router", Model: "claude-x"}
	p, k, m, err = tc.dispatchModelChoice("")
	if err != nil || p != "router" || m != "claude-x" || k != "k2" {
		t.Fatalf("派发默认模型应生效: %q %q %q %v", p, k, m, err)
	}

	// 派发默认模型的渠道被停用：报错，不回落主会话也不回落全局
	a.Cfg.Providers[1].Enabled = boolPtr(false)
	if _, _, _, err = tc.dispatchModelChoice(""); err == nil || !strings.Contains(err.Error(), "派发任务默认模型") {
		t.Fatalf("派发默认不可用应报错: %v", err)
	}
	a.Cfg.Providers[1].Enabled = nil
	a.Cfg.DispatchModel = ModelRef{}

	// 跟随主会话，而主会话自己没手选：跟全局默认（回空值，子会话跟随）
	main.Provider, main.Model = "", ""
	if p, k, m, err = tc.dispatchModelChoice(""); err != nil || p != "" || m != "" || k != "" {
		t.Fatalf("主会话未手选时应跟全局默认: %q %q %q %v", p, k, m, err)
	}
	// 全局默认也没设置：报错
	a.Cfg.DefaultModel = ModelRef{}
	if _, _, _, err = tc.dispatchModelChoice(""); err == nil || !strings.Contains(err.Error(), "全局默认模型") {
		t.Fatalf("全局默认也没有时应报错: %v", err)
	}
	// 全局默认设了但渠道没配可用 Key（非 mock 地址）：报错点名 Key
	a.Cfg.Providers[0].BaseURL = "https://zhipu/v4"
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	if _, _, _, err = tc.dispatchModelChoice(""); err == nil || !strings.Contains(err.Error(), "API Key") {
		t.Fatalf("Key 为空应报错: %v", err)
	}
}

// 待办三档取值链：待办自选 → 派发设置 → 全局默认（派发设置跟主会话时，待办
// 没有主会话可跟，落到全局默认，并在卡片上说清来自哪一环）。
func TestTodoModelChain(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}

	// 跟随派发设置 + 派发设置跟主会话 → 全局默认
	ref, source, err := a.todoModelChain(todoModelFollowDispatch, ModelRef{})
	if err != nil || !ref.Empty() || source != "全局默认模型" {
		t.Fatalf("应落到全局默认: %+v %q %v", ref, source, err)
	}
	v := a.todoModelView(todoModelFollowDispatch, ModelRef{})
	if v.Source != "全局默认模型" || v.Text != "智谱 · glm-5.3（跟随会话默认 · 不指定）" || !v.Available {
		t.Fatalf("卡片生效值不对: %+v", v)
	}

	// 派发设置改成「指定」：待办跟随它
	a.Cfg.DispatchModel = ModelRef{Provider: "router", Model: "claude-x"}
	ref, source, err = a.todoModelChain(todoModelFollowDispatch, ModelRef{})
	if err != nil || ref.Provider != "router" || ref.ProviderKey != "k2" || source != "派发任务默认模型" {
		t.Fatalf("应跟随派发默认模型: %+v %q %v", ref, source, err)
	}
	// 待办自选优先于派发设置
	ref, source, err = a.todoModelChain(todoModelCustom, ModelRef{Provider: "zhipu", Model: "glm-5.3-flash"})
	if err != nil || ref.Model != "glm-5.3-flash" || source != "待办自选模型" {
		t.Fatalf("待办自选应优先: %+v %q %v", ref, source, err)
	}
	// 待办自选的模型已不在渠道清单里：报错（不回落派发设置）
	_, _, err = a.todoModelChain(todoModelCustom, ModelRef{Provider: "zhipu", Model: "gone-model"})
	if err == nil || !strings.Contains(err.Error(), "请重选") {
		t.Fatalf("模型下线应报错: %v", err)
	}
	// 指定档却没选全：报错
	if _, _, err = a.todoModelChain(todoModelCustom, ModelRef{}); err == nil || !strings.Contains(err.Error(), "没有选全") {
		t.Fatalf("没选全应报错: %v", err)
	}
	// 指定 Key 归属但那把 Key 的分组里没有这个模型：报错
	_, _, err = a.todoModelChain(todoModelCustom, ModelRef{Provider: "router", ProviderKey: "k1", Model: "claude-x"})
	if err == nil || !strings.Contains(err.Error(), "不在渠道") {
		t.Fatalf("Key 分组不含该模型应报错: %v", err)
	}

	// 全局默认模型这一档：没设置就报错
	a.Cfg.DefaultModel = ModelRef{}
	if _, _, err = a.todoModelChain(todoModelGlobal, ModelRef{}); err == nil || !strings.Contains(err.Error(), "全局默认模型") {
		t.Fatalf("全局默认没设置应报错: %v", err)
	}
}

// ---------- 待办字段与数据迁移 ----------

// 待办「任务模型」三档列的读写：新建带自选、改档为全局、改回跟随派发设置。
func TestTodoModelFieldRoundTrip(t *testing.T) {
	_, s, _ := newTodoDispatchApp(t)
	view, err := s.create(todoRequest{
		Title: "带模型的待办", Description: "x", Executor: execAssistant, Target: targetTask,
		Schedule:  &Schedule{Kind: schedManual},
		ModelMode: todoModelCustom, Model: &ModelRef{Provider: "mock-prov", Model: "mock-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if view.ModelMode != todoModelCustom || view.Model == nil || view.Model.Model != "mock-model" {
		t.Fatalf("自选模型应落库: %+v", view.TaskItem)
	}
	if view.ModelView == nil || !view.ModelView.Available || view.ModelView.Text == "" {
		t.Fatalf("卡片应带当前生效值: %+v", view.ModelView)
	}

	// 改成「全局默认模型」：指向清空、档位留下
	upd, err := s.update(todoRequest{ID: view.ID, ModelMode: todoModelGlobal})
	if err != nil {
		t.Fatal(err)
	}
	if upd.ModelMode != todoModelGlobal || upd.Model != nil {
		t.Fatalf("改成全局默认应清空指向: %+v", upd.TaskItem)
	}

	// 改回「跟随派发设置」：档位归空（存的就是空串，老数据同形态）
	upd, err = s.update(todoRequest{ID: view.ID, ModelMode: "dispatch"})
	if err != nil {
		t.Fatal(err)
	}
	if upd.ModelMode != todoModelFollowDispatch || upd.Model != nil {
		t.Fatalf("改回跟随派发设置应归空: %+v", upd.TaskItem)
	}

	// 指定档却没给指向：拒绝
	if _, err = s.update(todoRequest{ID: view.ID, ModelMode: todoModelCustom}); err == nil {
		t.Fatal("指定档缺指向应拒绝")
	}
}

// 数据版本迁移：v1 老单据（没有 modelMode）读出来落在「跟随派发设置」；带了指向
// 却没写档位的半残数据补成 custom；版本号推到当前并落盘。
func TestTodoDataMigration(t *testing.T) {
	a, s, _ := newTodoDispatchApp(t)
	d := &TodoData{Version: 1, Tasks: []*TaskItem{
		{ID: "t1", Title: "老单据", Executor: execAssistant, Schedule: Schedule{Kind: schedManual}},
		{ID: "t2", Title: "带指向没档位", Executor: execAssistant, Schedule: Schedule{Kind: schedManual},
			Model: &ModelRef{Provider: "mock-prov", Model: "mock-model"}},
	}, Records: []*RunRecord{}}
	s.migrateData(d)
	if d.Version != todoDataVersion {
		t.Fatalf("版本号应推到 %d, got %d", todoDataVersion, d.Version)
	}
	if d.Tasks[0].ModelMode != todoModelFollowDispatch || d.Tasks[0].Model != nil {
		t.Fatalf("老单据应落在跟随派发设置: %+v", d.Tasks[0])
	}
	if d.Tasks[1].ModelMode != todoModelCustom || d.Tasks[1].Model == nil {
		t.Fatalf("带指向的应补成自选档: %+v", d.Tasks[1])
	}
	// 落盘后重新读：档位与指向都还在
	raw := a.Store.GetGlobalData(todoDataKey)
	if !strings.Contains(raw, "custom") || !strings.Contains(raw, "mock-model") {
		t.Fatalf("迁移结果应已落盘: %s", raw)
	}
	_ = s
}

// 待办派发时取模型：默认档 → 新会话跟随全局默认（不钉模型）；派发设置指定时
// 钉到派发默认模型上；不可用时报错且不建会话。
func TestTodoDispatchModelApplied(t *testing.T) {
	a, s, _ := newTodoDispatchApp(t)

	view, err := s.create(todoRequest{
		Title: "默认档", Description: "x", Executor: execAssistant, Target: targetTask,
		Schedule: &Schedule{Kind: schedManual},
	})
	if err != nil {
		t.Fatal(err)
	}
	task := findTodoTask(t, s, view.ID)
	sid, err := s.dispatch(task, "manual", 0)
	if err != nil {
		t.Fatalf("默认档派发应成功: %v", err)
	}
	sub, err := a.Store.Get(sid)
	if err != nil {
		t.Fatal(err)
	}
	if sub.Provider != "" || sub.Model != "" {
		t.Fatalf("落到全局默认时不钉模型（会话跟随全局默认）: %+v", sub)
	}

	// 派发默认模型指定成另一个可用模型：新会话钉到它上面
	a.Cfg.Providers = append(a.Cfg.Providers, ProviderCfg{ID: "alt", Name: "备用", BaseURL: "mock",
		Models: []ModelCfg{{ID: "alt-model"}}})
	a.Cfg.DispatchModel = ModelRef{Provider: "alt", Model: "alt-model"}
	view2, err := s.create(todoRequest{
		Title: "派发默认档", Description: "y", Executor: execAssistant, Target: targetTask,
		Schedule: &Schedule{Kind: schedManual},
	})
	if err != nil {
		t.Fatal(err)
	}
	task2 := findTodoTask(t, s, view2.ID)
	sid2, err := s.dispatch(task2, "manual", 0)
	if err != nil {
		t.Fatalf("派发默认模型派发应成功: %v", err)
	}
	sub2, err := a.Store.Get(sid2)
	if err != nil {
		t.Fatal(err)
	}
	if sub2.Provider != "alt" || sub2.Model != "alt-model" {
		t.Fatalf("新会话应钉到派发默认模型: %+v", sub2)
	}

	// 自选模型不可用（渠道被停用）：报错、不建会话
	a.Cfg.Providers[0].Enabled = boolPtr(false)
	view3, err := s.create(todoRequest{
		Title: "坏模型", Description: "z", Executor: execAssistant, Target: targetTask,
		Schedule:  &Schedule{Kind: schedManual},
		ModelMode: todoModelCustom, Model: &ModelRef{Provider: "alt", Model: "alt-model"},
	})
	if err != nil {
		t.Fatal(err)
	}
	a.Cfg.Providers[len(a.Cfg.Providers)-1].Enabled = boolPtr(false)
	before := len(todoAllSessions(t, a))
	task3 := findTodoTask(t, s, view3.ID)
	if _, err := s.dispatch(task3, "manual", 0); err == nil || !strings.Contains(err.Error(), "停用") {
		t.Fatalf("模型不可用应报错: %v", err)
	}
	if after := len(todoAllSessions(t, a)); after != before {
		t.Fatalf("不可用时不该建出会话: %d -> %d", before, after)
	}
	// 失败原因记进执行记录与任务状态（用户看得见）
	rec := lastTodoRecord(t, s)
	if rec.Status != "failed" || !strings.Contains(rec.Error, "停用") {
		t.Fatalf("失败原因应进执行记录: %+v", rec)
	}
}

// sync 渠道模型清单时不再改用户选择：下线只标不可用，判定仍是「模型已不在渠道
// 清单里，请重选」——这一条同时验证设置页红字的那句文案来源。
func TestSyncKeepsChoiceAndMarksUnavailable(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	// 模拟渠道清单被服务端更新、把用户选的模型下掉
	a.Cfg.Providers[0].Models = []ModelCfg{{ID: "glm-5.4"}}
	if err := a.checkModelRef(a.Cfg.DefaultModel, whereGlobalDefault); err == nil ||
		!strings.Contains(err.Error(), "请重选") || !strings.Contains(err.Error(), whereGlobalDefault) {
		t.Fatalf("下线模型应判定为不可用并指路: %v", err)
	}
	v := a.globalSettingsView()
	if v.Available || !strings.Contains(v.Error, "请重选") {
		t.Fatalf("设置页应红字提示重选: %+v", v)
	}
	if a.Cfg.DefaultModel.Model != "glm-5.3" {
		t.Fatalf("用户选择不该被改写: %+v", a.Cfg.DefaultModel)
	}
	// 保存接口不因不可用而失败（用户可能先选好再启用渠道），但界面照旧红字
	if err := a.setDefaultModels(ModelRef{Provider: "zhipu", Model: "glm-5.3"}, ModelRef{}, true); err != nil {
		t.Fatalf("保存不该因不可用失败: %v", err)
	}
}
