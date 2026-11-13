package main

// 市场客户端单测：httptest 假市场服务器（清单 + zip）跑通三类型的
// 安装 → 已装/可更新判定 → 更新 → 卸载全链路，外加未配置态与哈希校验失败。

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// zipDirForTest 把目录打成 zip 字节（与服务端仓库 jxai-server 的 zipTree 等价的最小实现）。
func zipDirForTest(t *testing.T, src string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	err := filepath.Walk(src, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, _ := filepath.Rel(src, path)
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(w, in)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// fakeMarket 起一个假市场服务器：按注册表下发清单与 zip。
type fakeMarket struct {
	srv     *httptest.Server
	entries map[string]map[string]marketEntryIn // typ → id → 清单条目
	bodies  map[string][]byte                   // "typ/id" → zip 字节
}

func newFakeMarket(t *testing.T) *fakeMarket {
	fm := &fakeMarket{
		entries: map[string]map[string]marketEntryIn{"skill": {}, "component": {}, "plugin": {}},
		bodies:  map[string][]byte{},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /market/manifest.json", func(w http.ResponseWriter, r *http.Request) {
		m := map[string]any{"name": "测试市场", "updatedAt": "2026-10-07T00:00:00Z"}
		for _, typ := range []string{"skill", "component", "plugin"} {
			list := []marketEntryIn{}
			for _, e := range fm.entries[typ] {
				list = append(list, e)
			}
			m[typ+"s"] = list
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m)
	})
	mux.HandleFunc("GET /market/items/", func(w http.ResponseWriter, r *http.Request) {
		p := strings.Split(strings.TrimPrefix(r.URL.Path, "/market/items/"), "/")
		if len(p) != 3 {
			http.NotFound(w, r)
			return
		}
		e, ok := fm.entries[p[0]][p[1]]
		if !ok || !strings.HasSuffix(r.URL.Path, e.ID+"-"+e.Version+".zip") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/zip")
		_, _ = w.Write(fm.bodies[p[0]+"/"+p[1]])
	})
	fm.srv = httptest.NewServer(mux)
	t.Cleanup(fm.srv.Close)
	return fm
}

func newMarketTestApp(t *testing.T, marketURL string) *App {
	t.Helper()
	dataDir := t.TempDir()
	skillsDir := t.TempDir()
	compDir := filepath.Join(t.TempDir(), "组件")
	os.MkdirAll(compDir, 0755)
	a := &App{
		Cfg: &Config{
			DataDir:   dataDir,
			SkillsDir: skillsDir,
			MarketURL: marketURL,
		},
		Components: InitComponents(compDir, filepath.Join(dataDir, "components.json")),
		CCPlugins:  InitCCPlugins(nil, dataDir),
	}
	return a
}

func callHandler(t *testing.T, h func(w http.ResponseWriter, r *http.Request), method, path, body string) (int, map[string]any) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rd)
	rec := httptest.NewRecorder()
	h(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// publish 把一个源目录登进假市场（打 zip、算哈希、登记清单条目）。
func (fm *fakeMarket) publish(t *testing.T, typ, id, version string, srcDir string, corruptSHA bool) {
	t.Helper()
	body := zipDirForTest(t, srcDir)
	sum := sha256.Sum256(body)
	sha := hex.EncodeToString(sum[:])
	if corruptSHA {
		sha = strings.Repeat("0", 64)
	}
	fm.entries[typ][id] = marketEntryIn{
		ID: id, Name: id, Version: version, Description: "测试条目",
		Zip:    "/market/items/" + typ + "/" + id + "/" + id + "-" + version + ".zip",
		SHA256: sha, Size: int64(len(body)),
	}
	fm.bodies[typ+"/"+id] = body
}

func writeTestSkillSrc(t *testing.T) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: 市场技能\ndescription: 来自市场\n---\n正文"), 0644)
	return dir
}

func writeTestComponentSrc(t *testing.T) string {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "manifest.json"),
		[]byte(`{"id":"mk-comp","name":"市场组件","version":"0.1.0","description":"测试","client":{"js":"client.js"}}`), 0644)
	os.WriteFile(filepath.Join(dir, "client.js"), []byte("CX.register({id:'mk-comp',init(){},dispose(){}})"), 0644)
	return dir
}

func writeTestPluginSrc(t *testing.T) string {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, ".claude-plugin"), 0755)
	os.WriteFile(filepath.Join(dir, ".claude-plugin", "plugin.json"),
		[]byte(`{"name":"mk-plug","version":"2.0.0","description":"市场插件"}`), 0644)
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("插件"), 0644)
	return dir
}

func TestMarketViewUnconfigured(t *testing.T) {
	a := newMarketTestApp(t, "")
	a.Cfg.RemoteAccess = RemoteAccessCfg{Host: "jxaizz.com", Port: 2228}
	code, out := callHandler(t, a.handleMarketView, "GET", "/api/market", "")
	if code != 200 {
		t.Fatalf("期待 200，收到 %d", code)
	}
	if out["configured"] != false {
		t.Fatalf("应未配置: %v", out)
	}
	// 建议值只给服务器地址（2026-11-03 口径）：清单后缀由程序自动拼
	if out["suggested"].(string) != "https://jxaizz.com:2228" {
		t.Fatalf("建议值不对: %v", out["suggested"])
	}
}

func TestMarketSkillLifecycle(t *testing.T) {
	fm := newFakeMarket(t)
	fm.publish(t, "skill", "mk-skill", "1.0.0", writeTestSkillSrc(t), false)
	a := newMarketTestApp(t, fm.srv.URL+"/market/manifest.json")

	// 初始：可见、未安装
	_, out := callHandler(t, a.handleMarketView, "GET", "/api/market", "")
	skills := out["skills"].([]any)
	if len(skills) != 1 || skills[0].(map[string]any)["installed"] != false {
		t.Fatalf("清单态不对: %#v", skills)
	}
	// 安装
	code, resp := callHandler(t, a.handleMarketInstall, "POST", "/api/market/install", `{"type":"skill","id":"mk-skill"}`)
	if code != 200 {
		t.Fatalf("安装失败: %d %v", code, resp)
	}
	userDir, err := a.userSkillsDir()
	if err != nil {
		t.Fatal(err)
	}
	if !dirExists(filepath.Join(userDir, "mk-skill")) {
		t.Fatal("技能目录未落位")
	}
	// 已装态 + 新版本可更新
	fm.publish(t, "skill", "mk-skill", "1.1.0", writeTestSkillSrc(t), false)
	a.marketCache = marketCacheEntry{} // 清缓存（安装路径会强刷，这里视图手动清）
	_, out = callHandler(t, a.handleMarketView, "GET", "/api/market", "")
	e := out["skills"].([]any)[0].(map[string]any)
	if e["installed"] != true || e["update"] != true {
		t.Fatalf("已装/可更新判定不对: %#v", e)
	}
	// 更新（先删再装）
	code, resp = callHandler(t, a.handleMarketInstall, "POST", "/api/market/install", `{"type":"skill","id":"mk-skill"}`)
	if code != 200 {
		t.Fatalf("更新失败: %d %v", code, resp)
	}
	recs := a.loadMarketRecs()
	if recs["skill:mk-skill"].Version != "1.1.0" {
		t.Fatalf("记账版本不对: %#v", recs)
	}
	// 卸载
	code, resp = callHandler(t, a.handleMarketUninstall, "POST", "/api/market/uninstall", `{"type":"skill","id":"mk-skill"}`)
	if code != 200 {
		t.Fatalf("卸载失败: %d %v", code, resp)
	}
	if dirExists(filepath.Join(userDir, "mk-skill")) {
		t.Fatal("卸载后目录应删除")
	}
}

func TestMarketComponentLifecycle(t *testing.T) {
	fm := newFakeMarket(t)
	fm.publish(t, "component", "mk-comp", "0.1.0", writeTestComponentSrc(t), false)
	a := newMarketTestApp(t, fm.srv.URL+"/market/manifest.json")

	code, resp := callHandler(t, a.handleMarketInstall, "POST", "/api/market/install", `{"type":"component","id":"mk-comp"}`)
	if code != 200 {
		t.Fatalf("安装失败: %d %v", code, resp)
	}
	if _, ok := a.Components.Manifest("mk-comp"); !ok {
		t.Fatal("组件安装后应被识别")
	}
	// 两档归属：市场装的是**用户组件**，落在 组件/user/<id>/（不进打包、不热更新）
	if a.Components.IsBuiltin("mk-comp") {
		t.Fatal("市场安装的组件不应被当成内置组件")
	}
	if want := filepath.Join(a.Components.UserDir(), "mk-comp"); !dirExists(want) {
		t.Fatalf("市场组件应装进用户组件区 %s", want)
	}
	if !a.Components.IsEnabled("mk-comp") {
		t.Fatal("市场组件缺省应启用")
	}
	// 同名内置组件不允许被市场覆盖（内置随包分发、随程序静默更新升级）
	writeComp(t, a.Components.BuiltinDir(), "voice", "语音（内置）", "1.0.0")
	a.Components.Rescan()
	srcForVoice := t.TempDir()
	writeComp(t, srcForVoice, "voice", "语音（市场版）", "9.9.9")
	if _, err := a.installMarketComponent(marketEntryIn{ID: "voice", Version: "9.9.9"}, filepath.Join(srcForVoice, "voice")); err == nil {
		t.Fatal("同名内置组件应拒绝市场安装")
	}
	// 内置组件无记账：市场卸载应拒绝（防误删随程序发的组件）
	code, resp = callHandler(t, a.handleMarketUninstall, "POST", "/api/market/uninstall", `{"type":"component","id":"mk-comp2"}`)
	if code == 200 {
		t.Fatalf("无记账的组件不应可卸载: %v", resp)
	}
	// 正常卸载
	code, resp = callHandler(t, a.handleMarketUninstall, "POST", "/api/market/uninstall", `{"type":"component","id":"mk-comp"}`)
	if code != 200 {
		t.Fatalf("卸载失败: %d %v", code, resp)
	}
	if _, ok := a.Components.Manifest("mk-comp"); ok {
		t.Fatal("卸载后组件应消失")
	}
}

func TestMarketPluginLifecycle(t *testing.T) {
	fm := newFakeMarket(t)
	fm.publish(t, "plugin", "mk-plug", "2.0.0", writeTestPluginSrc(t), false)
	a := newMarketTestApp(t, fm.srv.URL+"/market/manifest.json")

	code, resp := callHandler(t, a.handleMarketInstall, "POST", "/api/market/install", `{"type":"plugin","id":"mk-plug"}`)
	if code != 200 {
		t.Fatalf("安装失败: %d %v", code, resp)
	}
	st, ok := a.CCPlugins.stateOf("mk-plug@" + marketPluginHome)
	if !ok || st.Version != "2.0.0" {
		t.Fatalf("插件安装记录不对: %#v", st)
	}
	// 视图应显示已装且版本一致（无更新）
	_, out := callHandler(t, a.handleMarketView, "GET", "/api/market", "")
	e := out["plugins"].([]any)[0].(map[string]any)
	if e["installed"] != true || e["update"] != false {
		t.Fatalf("插件已装态不对: %#v", e)
	}
	// 卸载
	code, resp = callHandler(t, a.handleMarketUninstall, "POST", "/api/market/uninstall", `{"type":"plugin","id":"mk-plug"}`)
	if code != 200 {
		t.Fatalf("卸载失败: %d %v", code, resp)
	}
	if _, ok := a.CCPlugins.stateOf("mk-plug@" + marketPluginHome); ok {
		t.Fatal("卸载后安装记录应消失")
	}
}

func TestMarketShaMismatch(t *testing.T) {
	fm := newFakeMarket(t)
	fm.publish(t, "skill", "bad-skill", "1.0.0", writeTestSkillSrc(t), true) // 清单哈希故意写错
	a := newMarketTestApp(t, fm.srv.URL+"/market/manifest.json")
	code, resp := callHandler(t, a.handleMarketInstall, "POST", "/api/market/install", `{"type":"skill","id":"bad-skill"}`)
	if code == 200 || !strings.Contains(fmt.Sprint(resp["error"]), "校验失败") {
		t.Fatalf("哈希不符应被拒: %d %v", code, resp)
	}
}

// writeVersionedSkillSrc 带版本标记的技能源（正文含版本号，断言更新落位用）。
func writeVersionedSkillSrc(t *testing.T, ver string) string {
	t.Helper()
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: 市场技能\ndescription: 来自市场\n---\n正文 "+ver), 0644)
	return dir
}

// TestMarketSkillAutoUpdate 企业技能包 / 工作流自动更新（checkUpdateOnce 顺带跑的那条路）：
// 市场发新版 → 已装技能静默更新到新版并改记账；本地自建技能（无记账）不被触碰。
func TestMarketSkillAutoUpdate(t *testing.T) {
	fm := newFakeMarket(t)
	fm.publish(t, "skill", "mk-skill", "1.0.0", writeVersionedSkillSrc(t, "1.0.0"), false)
	a := newMarketTestApp(t, fm.srv.URL+"/market/manifest.json")
	if code, resp := callHandler(t, a.handleMarketInstall, "POST", "/api/market/install", `{"type":"skill","id":"mk-skill"}`); code != 200 {
		t.Fatalf("首装失败: %d %v", code, resp)
	}
	userDir, err := a.userSkillsDir()
	if err != nil {
		t.Fatal(err)
	}
	// 本地自建技能（无市场记账）：自动更新不得动它
	local := filepath.Join(userDir, "my-local")
	os.MkdirAll(local, 0755)
	os.WriteFile(filepath.Join(local, "SKILL.md"), []byte("---\nname: 本地技能\ndescription: 自建\n---\n本地正文"), 0644)

	fm.publish(t, "skill", "mk-skill", "1.1.0", writeVersionedSkillSrc(t, "1.1.0"), false)
	a.autoUpdateMarketSkills()

	raw, _ := os.ReadFile(filepath.Join(userDir, "mk-skill", "SKILL.md"))
	if !strings.Contains(string(raw), "1.1.0") {
		t.Fatalf("技能应已自动更新到 1.1.0: %s", raw)
	}
	if recs := a.loadMarketRecs(); recs["skill:mk-skill"].Version != "1.1.0" {
		t.Fatalf("记账版本应为 1.1.0: %#v", recs["skill:mk-skill"])
	}
	if _, err := os.Stat(filepath.Join(userDir, ".mk-skill.bak")); !os.IsNotExist(err) {
		t.Fatal("更新备份残留应清理")
	}
	if lraw, lerr := os.ReadFile(filepath.Join(local, "SKILL.md")); lerr != nil || !strings.Contains(string(lraw), "本地正文") {
		t.Fatalf("本地自建技能被误动: %v %s", lerr, lraw)
	}
	// 清单没有再发新版时重跑：内容与记账保持不变（幂等）
	a.autoUpdateMarketSkills()
	if recs := a.loadMarketRecs(); recs["skill:mk-skill"].Version != "1.1.0" {
		t.Fatalf("无新版时记账不应变化: %#v", recs["skill:mk-skill"])
	}
	if raw2, _ := os.ReadFile(filepath.Join(userDir, "mk-skill", "SKILL.md")); !strings.Contains(string(raw2), "1.1.0") {
		t.Fatalf("无新版时技能内容不应变化: %s", raw2)
	}
}
