package main

// 用户技能管理单测（skill_manager.go）：displayName 中文名体系、环境快照的
// 显示名优先与描述截断、安装口的内容体检（空 SKILL.md 拦截）、文件夹拖拽
// 安装端点（/api/skills/install-files）、单个 .md 说明文件的包装安装
//（addUserSkill 路径安装与 /api/skills/upload 拖拽上传两条通道）。

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newSkillTestApp(t *testing.T) *App {
	t.Helper()
	return newMarketTestApp(t, "") // 只用它的 DataDir/SkillsDir 骨架
}

// 内嵌技能母本必须只含白名单内置技能目录：user/ 子树是泄漏渠道（开发机的
// 内部用户技能含密钥，一旦 embed 指令改回整目录就会随 exe 分发），在这里
// 炸响防回归。白名单目录漏嵌（新内置技能忘了加 embed 行）同样炸。
func TestEmbeddedSkillsTreeIsWhitelistOnly(t *testing.T) {
	fs.WalkDir(skillsFS, "技能", func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(p, "技能/user/") {
			t.Fatalf("内嵌技能树混入了 user/ 子树（泄漏渠道，检查 go:embed 指令）: %s", p)
		}
		return nil
	})
	for _, id := range builtinSkillOrder {
		if _, err := skillsFS.ReadDir("技能/" + id); err != nil {
			t.Fatalf("内置技能 %s 未内嵌（新内置技能要同步加 embed 行）: %v", id, err)
		}
	}
}

// 泄漏清理：旧安装包误分发的内部技能启动即删；市场正式安装（有记账）与
// 名单外的用户技能不动。
func TestPurgeLeakedUserSkills(t *testing.T) {
	a := newSkillTestApp(t)
	userDir, _ := a.userSkillsDir()
	mk := func(id string) {
		os.MkdirAll(filepath.Join(userDir, id), 0755)
		os.WriteFile(filepath.Join(userDir, id, "SKILL.md"), []byte("---\nname: x\ndescription: d\n---\n正文"), 0644)
	}
	mk("jxmes-master") // 名单内、无记账：删
	mk("微信操作")     // 名单内（中文目录名）：删
	mk("SKILL")        // 名单内（gitea 技能的目录名就叫 SKILL）：删
	mk("my-own")       // 名单外（用户自建）：保留
	if err := a.saveMarketRec("skill", "server-publish-update", marketInstallRec{Version: "1.0.0"}); err != nil {
		t.Fatal(err)
	}
	mk("server-publish-update") // 名单内、市场记账在：保留

	a.purgeLeakedUserSkills(filepath.Dir(userDir))
	for _, gone := range []string{"jxmes-master", "微信操作", "SKILL"} {
		if dirExists(filepath.Join(userDir, gone)) {
			t.Fatalf("泄漏技能 %s 应被清理", gone)
		}
	}
	for _, keep := range []string{"my-own", "server-publish-update"} {
		if !dirExists(filepath.Join(userDir, keep)) {
			t.Fatalf("%s 不应被清理", keep)
		}
	}
}

func TestSkillMetaParsesDisplayName(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"),
		[]byte("---\nname: wechat-chat-ops\ndisplayName: 微信操作\ndescription: 驱动微信\nversion: 1.2.0\n---\n\n正文"), 0644)
	fm := skillMeta(dir)
	if fm.Name != "wechat-chat-ops" || fm.DisplayName != "微信操作" || fm.Description != "驱动微信" || fm.Version != "1.2.0" {
		t.Fatalf("frontmatter 解析不对: %#v", fm)
	}
	if fm.Body != "正文" {
		t.Fatalf("正文不对: %q", fm.Body)
	}
	// 显示名回退：displayName → name → id
	if got := skillDisplayOf("", "fallback-name", "id"); got != "fallback-name" {
		t.Fatalf("回退到 name 不对: %q", got)
	}
	if got := skillDisplayOf(" ", "", "the-id"); got != "the-id" {
		t.Fatalf("回退到 id 不对: %q", got)
	}
}

func TestListSkillsCarriesDisplayNameAndVersion(t *testing.T) {
	a := newSkillTestApp(t)
	userDir, _ := a.userSkillsDir()
	skill := filepath.Join(userDir, "wechat-chat-ops")
	os.MkdirAll(skill, 0755)
	os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: wechat-chat-ops\ndisplayName: 微信操作\ndescription: 驱动微信\nversion: 2.0.0\n---\n\n正文"), 0644)
	list, err := a.listSkills()
	if err != nil {
		t.Fatal(err)
	}
	if len(list.User) != 1 {
		t.Fatalf("应识别 1 个用户技能: %#v", list.User)
	}
	s := list.User[0]
	if s.DisplayName != "微信操作" || s.Version != "2.0.0" || s.ID != "wechat-chat-ops" {
		t.Fatalf("清单字段不对: %#v", s)
	}
}

func TestUserSkillsPromptBlockDisplayNameAndTruncation(t *testing.T) {
	a := newSkillTestApp(t)
	userDir, _ := a.userSkillsDir()
	skill := filepath.Join(userDir, "wx")
	os.MkdirAll(skill, 0755)
	long := strings.Repeat("长", 260)
	os.WriteFile(filepath.Join(skill, "SKILL.md"),
		[]byte("---\nname: wx\ndisplayName: 微信操作\ndescription: "+long+"\n---\n\n正文"), 0644)
	block := a.userSkillsPromptBlock()
	if !strings.Contains(block, "微信操作（id=wx）") {
		t.Fatalf("快照应优先 displayName: %q", block)
	}
	if strings.Contains(block, long) {
		t.Fatal("超长描述应被截断，不应整段进环境快照")
	}
	if !strings.Contains(block, "…") {
		t.Fatal("截断应带省略号")
	}
	if r := []rune(strings.TrimSpace(block)); len(r) > 400 { // 快照本身不该膨胀
		t.Fatalf("快照过长: %d runes", len(r))
	}
}

// renderSkillListing 预算降级（对齐 ZCode skills.ts 的两级形态）：完整版超
// skillListingBudget 时整段降级为仅名字行，名字+id 全保留、描述不出现；
// 预算内走完整版。
func TestRenderSkillListingBudgetDowngrade(t *testing.T) {
	full, names := make([]string, 100), make([]string, 100)
	for i := range full {
		id := fmt.Sprintf("s%d", i)
		full[i] = "  - 技能" + id + "（id=" + id + "）：" + strings.Repeat("述", 300)
		names[i] = "  - 技能" + id + "（id=" + id + "）"
	}
	out := renderSkillListing("# 头", full, names)
	if len(out) > skillListingBudget {
		t.Fatalf("超预算应降级: %d > %d", len(out), skillListingBudget)
	}
	if !strings.Contains(out, "（id=s0）") || !strings.Contains(out, "（id=s99）") {
		t.Fatal("降级版应保留全部名字+id")
	}
	if strings.Contains(out, "述述") {
		t.Fatal("降级后不应出现描述正文")
	}
	if !strings.Contains(out, "# 头") {
		t.Fatal("段头应保留")
	}
	// 预算内：完整版原样输出
	out2 := renderSkillListing("# 头", names, names)
	if len(out2) > skillListingBudget || !strings.Contains(out2, "（id=s0）") {
		t.Fatalf("预算内应走完整版: %q", out2[:60])
	}
	// 空清单返回空串
	if got := renderSkillListing("# 头", nil, nil); got != "" {
		t.Fatalf("空清单应返回空串: %q", got)
	}
}

// 用户技能清单段的集成降级：装一堆长描述技能，环境快照段应降级在预算内，
// 名字+id 全在、描述不进快照（read_skill 按需取全文的契约不受影响）。
func TestUserSkillsPromptBlockBudgetDowngrade(t *testing.T) {
	a := newSkillTestApp(t)
	userDir, _ := a.userSkillsDir()
	for i := 0; i < 90; i++ { // 90 × 300 字描述（截断后 600 字节/条）≈ 5.4 万字节 > 2 万预算
		id := fmt.Sprintf("sk%02d", i)
		skill := filepath.Join(userDir, id)
		os.MkdirAll(skill, 0755)
		os.WriteFile(filepath.Join(skill, "SKILL.md"),
			[]byte(fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\n正文", id, strings.Repeat("述", 300))), 0644)
	}
	block := a.userSkillsPromptBlock()
	if len(block) > skillListingBudget {
		t.Fatalf("清单段应降级在预算内: %d > %d", len(block), skillListingBudget)
	}
	if !strings.Contains(block, "（id=sk00）") || !strings.Contains(block, "（id=sk89）") {
		t.Fatal("降级版应保留全部名字+id")
	}
	if strings.Contains(block, "述述") {
		t.Fatal("降级后描述正文不应进环境快照")
	}
	if !strings.Contains(block, "用户技能清单") {
		t.Fatal("段头应保留")
	}
}

func TestAddUserSkillRejectsEmptyBody(t *testing.T) {
	a := newSkillTestApp(t)
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("---\nname: empty\n---\n"), 0644)
	if _, err := a.addUserSkill(dir); err == nil || !strings.Contains(err.Error(), "正文") {
		t.Fatalf("只有 frontmatter 的 SKILL.md 应在安装口被拦: %v", err)
	}
	// zip 同理（市场装下来的空包也进不来）
	zipPath := filepath.Join(dir, "..", "empty.zip")
	if err := os.WriteFile(zipPath, zipBytesForTest(t, map[string]string{"SKILL.md": ""}), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := a.addUserSkill(zipPath); err == nil || !strings.Contains(err.Error(), "空文件") {
		t.Fatalf("空 SKILL.md 的 zip 应被拦: %v", err)
	}
}

func TestAddUserSkillFromMDFile(t *testing.T) {
	a := newSkillTestApp(t)
	dir := t.TempDir()
	// 单个 .md 说明文件：id 取文件名，frontmatter 提供名称与描述
	md := filepath.Join(dir, "gitea-helper.md")
	os.WriteFile(md, []byte("---\nname: gitea-helper\ndisplayName: Gitea 助手\ndescription: 调 Gitea API\n---\n\n使用说明正文"), 0644)
	entry, err := a.addUserSkill(md)
	if err != nil {
		t.Fatal(err)
	}
	if entry.ID != "gitea-helper" || entry.DisplayName != "Gitea 助手" || entry.Description != "调 Gitea API" {
		t.Fatalf("安装结果不对: %#v", entry)
	}
	// 内容应落为 技能/user/<文件名>/SKILL.md
	installed := filepath.Join(mustUserDir(t, a), "gitea-helper", "SKILL.md")
	data, err := os.ReadFile(installed)
	if err != nil || !strings.Contains(string(data), "使用说明正文") {
		t.Fatalf(".md 应包装成 SKILL.md 安装: %v", err)
	}
	// 清单扫描同样能发现
	list, _ := a.listSkills()
	if len(list.User) != 1 || list.User[0].ID != "gitea-helper" {
		t.Fatalf("清单应含包装安装的技能: %#v", list.User)
	}

	// 空正文的 .md 走同一条内容体检被拦
	emptyMD := filepath.Join(dir, "empty-skill.md")
	os.WriteFile(emptyMD, []byte("---\nname: empty-skill\n---\n"), 0644)
	if _, err := a.addUserSkill(emptyMD); err == nil || !strings.Contains(err.Error(), "正文") {
		t.Fatalf("空正文的 .md 应被拦: %v", err)
	}

	// 不认识的扩展名明确报错
	txt := filepath.Join(dir, "note.txt")
	os.WriteFile(txt, []byte("x"), 0644)
	if _, err := a.addUserSkill(txt); err == nil || !strings.Contains(err.Error(), ".md") {
		t.Fatalf("非 zip/md 文件应报支持范围: %v", err)
	}
}

func TestUploadSkillMDEndpoint(t *testing.T) {
	a := newSkillTestApp(t)
	// 拖拽安装通道（multipart 单文件）：.md 与 .zip 同走 /api/skills/upload
	var body = &strings.Builder{}
	mw := multipart.NewWriter(body)
	fw, _ := mw.CreateFormFile("file", "日历操作.md")
	fw.Write([]byte("---\nname: calendar\ndisplayName: 日历操作\ndescription: 读写日程\n---\n\n正文"))
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/skills/upload", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleUploadSkill(rec, req)
	if rec.Code != 200 {
		t.Fatalf("期待 200，收到 %d：%s", rec.Code, rec.Body.String())
	}
	out := userSkillEntry{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "日历操作" || out.DisplayName != "日历操作" {
		t.Fatalf("上传安装结果不对: %#v", out)
	}
	if _, err := os.Stat(filepath.Join(mustUserDir(t, a), "日历操作", "SKILL.md")); err != nil {
		t.Fatal(".md 上传应包装安装")
	}

	// 其他扩展名拒收并提示支持范围
	var body2 = &strings.Builder{}
	mw2 := multipart.NewWriter(body2)
	fw2, _ := mw2.CreateFormFile("file", "run.py")
	fw2.Write([]byte("print(1)"))
	mw2.Close()
	req2 := httptest.NewRequest(http.MethodPost, "/api/skills/upload", strings.NewReader(body2.String()))
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	rec2 := httptest.NewRecorder()
	a.handleUploadSkill(rec2, req2)
	if rec2.Code != 400 || !strings.Contains(rec2.Body.String(), ".md") {
		t.Fatalf("非 zip/md 上传应 400，实得 %d：%s", rec2.Code, rec2.Body.String())
	}
}

func TestInstallSkillFilesEndpoint(t *testing.T) {
	a := newSkillTestApp(t)
	// multipart：一个顶层目录（=拖入的技能文件夹）+ SKILL.md/脚本。
	// files 用有序切片：multipart 的 filename 只剩 basename，与 paths 按下标配对，
	// 用 map 迭代会随机错位（真实前端按 paths 顺序 append）。
	var body = &strings.Builder{}
	mw := multipart.NewWriter(body)
	type tf struct{ path, content string }
	files := []tf{
		{"微信操作/SKILL.md", "---\nname: wx\ndisplayName: 微信操作\ndescription: 测试\n---\n\n正文"},
		{"微信操作/scripts/run.py", "print(1)"},
	}
	paths := make([]string, 0, len(files))
	for _, f := range files {
		paths = append(paths, f.path)
	}
	pb, _ := json.Marshal(paths)
	_ = mw.WriteField("paths", string(pb))
	for _, f := range files {
		fw, _ := mw.CreateFormFile("files", filepath.Base(f.path))
		fw.Write([]byte(f.content))
	}
	mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/api/skills/install-files", strings.NewReader(body.String()))
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	a.handleInstallSkillFiles(rec, req)
	if rec.Code != 200 {
		t.Fatalf("期待 200，收到 %d：%s", rec.Code, rec.Body.String())
	}
	out := userSkillEntry{}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out.ID != "微信操作" || out.DisplayName != "微信操作" {
		t.Fatalf("安装结果不对: %#v", out)
	}
	// 装到 技能/user/ 了
	if _, err := os.Stat(filepath.Join(mustUserDir(t, a), "微信操作", "scripts", "run.py")); err != nil {
		t.Fatal("文件应落进技能目录")
	}

	// 散文件（无公共顶层目录）拒收
	var body2 = &strings.Builder{}
	mw2 := multipart.NewWriter(body2)
	pb2, _ := json.Marshal([]string{"SKILL.md", "run.py"})
	_ = mw2.WriteField("paths", string(pb2))
	for _, p := range []string{"SKILL.md", "run.py"} {
		fw, _ := mw2.CreateFormFile("files", p)
		fw.Write([]byte("x"))
	}
	mw2.Close()
	req2 := httptest.NewRequest(http.MethodPost, "/api/skills/install-files", strings.NewReader(body2.String()))
	req2.Header.Set("Content-Type", mw2.FormDataContentType())
	rec2 := httptest.NewRecorder()
	a.handleInstallSkillFiles(rec2, req2)
	if rec2.Code != 400 || !strings.Contains(rec2.Body.String(), "整个技能文件夹") {
		t.Fatalf("散文件应 400，实得 %d：%s", rec2.Code, rec2.Body.String())
	}

	// 路径穿越拒收
	var body3 = &strings.Builder{}
	mw3 := multipart.NewWriter(body3)
	pb3, _ := json.Marshal([]string{"pkg/SKILL.md", "pkg/../../evil.py"})
	_ = mw3.WriteField("paths", string(pb3))
	for _, p := range []string{"SKILL.md", "evil.py"} {
		fw, _ := mw3.CreateFormFile("files", p)
		fw.Write([]byte("x"))
	}
	mw3.Close()
	req3 := httptest.NewRequest(http.MethodPost, "/api/skills/install-files", strings.NewReader(body3.String()))
	req3.Header.Set("Content-Type", mw3.FormDataContentType())
	rec3 := httptest.NewRecorder()
	a.handleInstallSkillFiles(rec3, req3)
	if rec3.Code != 400 {
		t.Fatalf("路径穿越应 400，实得 %d：%s", rec3.Code, rec3.Body.String())
	}
}

func mustUserDir(t *testing.T, a *App) string {
	t.Helper()
	d, err := a.userSkillsDir()
	if err != nil {
		t.Fatal(err)
	}
	return d
}
