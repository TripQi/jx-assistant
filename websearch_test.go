// websearch.go 单测：服务端地址解析的回退链（searchURL 优先 → 市场同源 →
// 未配置报错）与结果渲染（列表形态、空结果走 answer、引用提醒在场）。
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSearchEndpointBase(t *testing.T) {
	a := &App{Cfg: &Config{}}
	// 两者都没配：明确报错。
	if _, err := a.searchEndpointBase(); err == nil || !strings.Contains(err.Error(), "网络访问") {
		t.Fatalf("未配置应报错: %v", err)
	}
	// 只配市场地址：取同源。
	a.Cfg.MarketURL = "http://192.168.1.10:2228/market/manifest.json"
	base, err := a.searchEndpointBase()
	if err != nil || base != "http://192.168.1.10:2228" {
		t.Fatalf("市场同源不对: %s %v", base, err)
	}
	// 配了 searchURL：优先于市场。
	a.Cfg.SearchURL = "https://search.example.com:8443/任意路径"
	base, err = a.searchEndpointBase()
	if err != nil || base != "https://search.example.com:8443" {
		t.Fatalf("searchURL 优先不对: %s %v", base, err)
	}
	// searchURL 非法：报 searchURL 而不是回落市场。
	a.Cfg.SearchURL = "不是地址"
	if _, err := a.searchEndpointBase(); err == nil || !strings.Contains(err.Error(), "searchURL") {
		t.Fatalf("非法 searchURL 应报错: %v", err)
	}
}

func TestRenderSearchResults(t *testing.T) {
	out := searchResponse{Engine: "zhipu-mcp"}
	out.Results = []searchHit{
		{Title: "结果一", URL: "https://a.com/1", Snippet: "摘要一", Site: "A站", Date: "2026-10-01"},
		{Title: "结果二", URL: "https://b.com/2", Snippet: strings.Repeat("长", 300), Site: "B站"},
	}
	text := renderSearchResults("测试", out)
	for _, want := range []string{"搜索「测试」：2 条结果", "[结果一](https://a.com/1)（2026-10-01）", "…", "web_fetch"} {
		if !strings.Contains(text, want) {
			t.Fatalf("渲染缺 %q:\n%s", want, text)
		}
	}
	// 空结果但有 answer：转交文本。
	out2 := searchResponse{Engine: "zhipu-mcp", Answer: "直接回答文本"}
	if text := renderSearchResults("q", out2); !strings.Contains(text, "直接回答文本") {
		t.Fatalf("answer 应转交:\n%s", text)
	}
}

// 请求头契约：搜索请求带 X-Jxai-Conversation（值=会话 id，服务端按它对
// 对话轮询搜索引擎；工具执行时 sess 恒非空——authorize 更早就依赖它）。
func TestWebSearchConversationHeader(t *testing.T) {
	var conv string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conv = r.Header.Get(searchConversationHeader)
		w.Write([]byte(`{"results":[{"title":"t","url":"https://a","snippet":"s"}],"engine":"zhipu-mcp"}`))
	}))
	defer srv.Close()
	a := &App{Cfg: &Config{SearchURL: srv.URL}}
	if _, err := a.runWebSearch(&toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "sess-conv-1"}},
		json.RawMessage(`{"query":"测试"}`)); err != nil {
		t.Fatalf("runWebSearch 出错: %v", err)
	}
	if conv != "sess-conv-1" {
		t.Fatalf("应带对话标识头 X-Jxai-Conversation=sess-conv-1，实得 %q", conv)
	}
}

// mkSkillTestApp 建一个带临时技能目录/数据目录的 App：enableLocal 决定是否
// 放置 web-search-local 技能（内置形态：技能根/<id>/）。
func mkSkillTestApp(t *testing.T, enableLocal bool) *App {
	t.Helper()
	dir := t.TempDir()
	if enableLocal {
		skill := filepath.Join(dir, localSearchSkillID)
		if err := os.MkdirAll(skill, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skill, "SKILL.md"), []byte("---\nname: web-search-local\ndisplayName: 本机网络搜索\ndescription: 兜底\n---\n\n# 本机网络搜索\n正文"), 0o644); err != nil {
			t.Fatal(err)
		}
		// manifest.json 是内置技能清单的识别依据（listSkills 的 BuiltIn 段），
		// 与生产布局对齐（内置技能目录必有 manifest）。
		if err := os.WriteFile(filepath.Join(skill, "manifest.json"), []byte(`{"id":"web-search-local","name":"本机网络搜索","version":"1.0.0","description":"兜底"}`), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return &App{Cfg: &Config{SkillsDir: dir, DataDir: t.TempDir()}}
}

// searchErrHint 两态：技能不在场→不指引；内置在位→附加改道指引。服务端
// 搜索失败的每种错误都要带上这道桥（searchErrHint 的挂点在 runWebSearch
// 的全部错误出口），技能目录不在时指引必须干净消失。
func TestSearchErrHint(t *testing.T) {
	// 未放置技能：无指引。
	a := mkSkillTestApp(t, false)
	if a.skillAvailable(localSearchSkillID) {
		t.Fatal("未放置技能不应可用")
	}
	err := a.searchErrHint(fmt.Errorf("连接服务端失败: x"))
	if err == nil || strings.Contains(err.Error(), "本机网络搜索") {
		t.Fatalf("技能不存在时不应附加指引: %v", err)
	}
	// 内置在位（技能根下）：skillAvailable 成立，指引附加。
	a = mkSkillTestApp(t, true)
	if !a.skillAvailable(localSearchSkillID) {
		t.Fatal("内置技能在位应可用")
	}
	err = a.searchErrHint(fmt.Errorf("连接服务端失败: x"))
	if err == nil || !strings.Contains(err.Error(), "本机网络搜索") || !strings.Contains(err.Error(), "read_skill") {
		t.Fatalf("应附加本机搜索指引: %v", err)
	}
	// read_skill 要能读到内置技能的正文（改道指引的下一步）。
	body, rerr := runReadUserSkill(&toolContext{app: a}, json.RawMessage(`{"id":"web-search-local"}`))
	if rerr != nil || !strings.Contains(body, "正文") {
		t.Fatalf("read_skill 应能读内置技能: %v %q", rerr, body)
	}
}

// 未配置服务端地址 + 技能可用：runWebSearch 的错误出口带上兜底指引。
func TestWebSearchUnconfiguredHint(t *testing.T) {
	a := mkSkillTestApp(t, true)
	_, err := a.runWebSearch(&toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "s1"}},
		json.RawMessage(`{"query":"测试"}`))
	if err == nil || !strings.Contains(err.Error(), "网络访问") {
		t.Fatalf("未配置应报错: %v", err)
	}
	if !strings.Contains(err.Error(), "本机网络搜索") {
		t.Fatalf("未配置且技能可用时应指引本机兜底: %v", err)
	}
}

// 连不上服务端（关掉的 httptest 地址）+ 技能可用：连接失败错误也带指引。
func TestWebSearchUnreachableHint(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	base := srv.URL
	srv.Close() // 立即关闭：地址必连不通
	a := mkSkillTestApp(t, true)
	a.Cfg.SearchURL = base
	_, err := a.runWebSearch(&toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "s2"}},
		json.RawMessage(`{"query":"测试"}`))
	if err == nil || !strings.Contains(err.Error(), "连接服务端失败") {
		t.Fatalf("应报连接失败: %v", err)
	}
	if !strings.Contains(err.Error(), "本机网络搜索") {
		t.Fatalf("连接失败且技能可用时应指引本机兜底: %v", err)
	}
}

// web-search-local 是内置技能：要进自带技能清单（listSkills 的 BuiltIn 段，
// 常开），不再出现在用户技能段（存量机器的 user/ 旧副本由启动迁移收走）。
func TestLocalSearchSkillListed(t *testing.T) {
	a := mkSkillTestApp(t, true)
	list, err := a.listSkills()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range list.BuiltIn {
		if s.ID == localSearchSkillID {
			found = s.Enabled && s.BuiltIn
		}
	}
	if !found {
		t.Fatalf("自带技能清单应含本机网络搜索: %+v", list.BuiltIn)
	}
	for _, s := range list.User {
		if s.ID == localSearchSkillID {
			t.Fatalf("内置技能不应出现在用户技能段: %+v", list.User)
		}
	}
}

// 存量机器迁移：user/ 下的 web-search-local 旧副本（含用户配的 config.json
// 搜索 key）在 SkillsDir 首次解析时挪到 技能根/，key 一并带走；根下已有
// 新版（静默更新先到）时只补 key、不覆盖技能本体。
func TestMigrateBuiltinFromUser(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "user", localSearchSkillID)
	os.MkdirAll(filepath.Join(old, "scripts"), 0o755)
	os.WriteFile(filepath.Join(old, "SKILL.md"), []byte("---\nname: web-search-local\n---\n旧版正文"), 0o644)
	os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"zhipu_key":"k"}`), 0o644)

	a := &App{Cfg: &Config{SkillsDir: dir, DataDir: t.TempDir()}}
	if _, err := a.SkillsDir(); err != nil {
		t.Fatal(err)
	}
	a.runMigrations() // 搬家由迁移机制执行（20261030.builtin-skill-from-user）
	if dirExists(old) {
		t.Fatal("user 下旧副本应被挪走")
	}
	if got, err := os.ReadFile(filepath.Join(dir, localSearchSkillID, "config.json")); err != nil || !strings.Contains(string(got), "zhipu_key") {
		t.Fatalf("config.json 应随迁移带到根下: %v %s", err, got)
	}
	// 根下已有新版时：技能本体不被旧副本覆盖，缺失的 key 会补上。
	os.MkdirAll(filepath.Join(dir, localSearchSkillID, "scripts"), 0o755)
	os.WriteFile(filepath.Join(dir, localSearchSkillID, "SKILL.md"), []byte("---\nname: web-search-local\n---\n新版正文"), 0o644)
	os.Remove(filepath.Join(dir, localSearchSkillID, "config.json"))
	os.MkdirAll(old, 0o755)
	os.WriteFile(filepath.Join(old, "SKILL.md"), []byte("旧版"), 0o644)
	os.WriteFile(filepath.Join(old, "config.json"), []byte(`{"bocha_key":"b"}`), 0o644)
	a2 := &App{Cfg: &Config{SkillsDir: dir, DataDir: t.TempDir()}}
	if _, err := a2.SkillsDir(); err != nil {
		t.Fatal(err)
	}
	// 直接调函数：这段测的是「根下已有新版时只补 key 不覆盖」的行为本身
	// （走 runMigrations 会因 a 已记账而跳过，故不复用那条路）
	a2.migrateBuiltinFromUser(dir)
	if got, _ := os.ReadFile(filepath.Join(dir, localSearchSkillID, "SKILL.md")); !strings.Contains(string(got), "新版正文") {
		t.Fatalf("新版技能本体不应被旧副本覆盖: %s", got)
	}
	if got, err := os.ReadFile(filepath.Join(dir, localSearchSkillID, "config.json")); err != nil || !strings.Contains(string(got), "bocha_key") {
		t.Fatalf("缺失的 key 应从旧副本补上: %v %s", err, got)
	}
	if dirExists(old) {
		t.Fatal("user 下旧副本应清理")
	}
}
