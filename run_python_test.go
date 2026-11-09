package main

// run_python 的针对性测试：落盘归档（.jxai/tmp/<会话id>/）、中文/JSON 输出
// 编码、path 重跑、目录越界拒绝、非法文件名回退、超龄孤儿目录清理。
// 真实链路（权限→沙箱→powershell→便携 python）在本机跑；技能运行时未装配
// 的环境跳过主体用例（装配需联网，不属于单测职责）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newRunPyTestApp(t *testing.T) *App {
	t.Helper()
	a := &App{Cfg: &Config{DataDir: t.TempDir(), Workspace: t.TempDir(), AllowShell: true}}
	// CfgPath 必须显式给：空值会让 protectPaths 把 cwd（项目根）整目录保护，
	// 工具读项目根的文件全被「受保护路径」误拦（真实 App 的 CfgPath 恒非空）
	a.CfgPath = filepath.Join(a.Cfg.DataDir, "config.json")
	a.initRuntime()
	// 技能目录指向本仓库的 技能/（go test 的 cwd=项目根）：内置便携 Python
	// 已装配的机器上真机用例可跑；否则主体用例自动跳过
	a.Cfg.SkillsDir = "技能"
	a.initRuntime()
	return a
}

// skipIfNoPyRuntime 便携解释器未装配时跳过（真装配要联网下载，不在单测里做）。
func skipIfNoPyRuntime(t *testing.T, a *App) {
	t.Helper()
	dir, err := a.SkillsDir()
	if err != nil {
		t.Skipf("技能目录不可用: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "_runtime", "python", pyExeName())); err != nil {
		t.Skip("便携 Python 未装配（需联网），跳过 run_python 真机用例")
	}
}

func pyExeName() string {
	if isWindowsRuntime {
		return "python.exe"
	}
	return "python3"
}

func runPyCall(t *testing.T, a *App, sess *SessionMeta, args map[string]any) (string, error) {
	t.Helper()
	raw, _ := json.Marshal(args)
	tctx := &toolContext{app: a, sess: sess, emit: func(Event) {}, ctx: context.Background()}
	return runPython(tctx, raw)
}

func TestRunPythonBasicAndRerun(t *testing.T) {
	a := newRunPyTestApp(t)
	skipIfNoPyRuntime(t, a)
	sess := &SessionMeta{ID: "testpy0001abcdef", Workspace: a.Cfg.Workspace}

	// 中文 + JSON：曾经的 PowerShell 内联坑场景，脚本化后应原样返回
	code := "import json\nprint(json.dumps({\"城市\": \"杭州\", \"n\": 3}, ensure_ascii=False))"
	out, err := runPyCall(t, a, sess, map[string]any{"code": code, "name": "json_demo"})
	if err != nil {
		t.Fatalf("run_python 执行失败: %v", err)
	}
	if !strings.Contains(out, `"城市": "杭州"`) {
		t.Fatalf("中文 JSON 输出不对（编码坑未消失）:\n%s", out)
	}
	script := filepath.Join(a.Cfg.Workspace, ".jxai", "tmp", sess.ID, "json_demo.py")
	if _, serr := os.Stat(script); serr != nil {
		t.Fatalf("脚本未落盘到会话目录 %s: %v", script, serr)
	}
	if !strings.Contains(out, script) {
		t.Fatalf("返回里应带脚本路径:\n%s", out)
	}

	// path 重跑：相对路径（相对会话工作区）
	out2, err := runPyCall(t, a, sess, map[string]any{"path": ".jxai/tmp/" + sess.ID + "/json_demo.py"})
	if err != nil || !strings.Contains(out2, `"城市": "杭州"`) {
		t.Fatalf("path 重跑失败: %v\n%s", err, out2)
	}
}

func TestRunPythonPathOutsideRejected(t *testing.T) {
	a := newRunPyTestApp(t)
	sess := &SessionMeta{ID: "testpy0002abcdef", Workspace: a.Cfg.Workspace}
	outside := filepath.Join(a.Cfg.Workspace, "evil.py")
	_ = os.WriteFile(outside, []byte("print(1)"), 0o644)
	_, err := runPyCall(t, a, sess, map[string]any{"path": "evil.py"})
	if err == nil || !strings.Contains(err.Error(), "本会话脚本目录") {
		t.Fatalf("工作区根的 py 应被拒绝（只放行会话脚本目录内）, got: %v", err)
	}
}

func TestRunPythonBadNameFallsBack(t *testing.T) {
	a := newRunPyTestApp(t)
	skipIfNoPyRuntime(t, a)
	sess := &SessionMeta{ID: "testpy0003abcdef", Workspace: a.Cfg.Workspace}
	out, err := runPyCall(t, a, sess, map[string]any{"code": "print('ok')", "name": "../../evil"})
	if err != nil {
		t.Fatalf("非法 name 应回退默认名而不是失败: %v", err)
	}
	dir := filepath.Join(a.Cfg.Workspace, ".jxai", "tmp", sess.ID)
	entries, _ := os.ReadDir(dir)
	// jx.py 是工具桥客户端（bridge.go 每次 run_python 生成），不算业务脚本；
	// 业务脚本应恰好一个：默认时间戳名、不含 evil。
	scripts := []string{}
	for _, e := range entries {
		if e.Name() != "jx.py" {
			scripts = append(scripts, e.Name())
		}
	}
	if len(scripts) != 1 || !strings.HasSuffix(scripts[0], ".py") || strings.Contains(scripts[0], "evil") {
		t.Fatalf("应落一个默认名脚本, got: %v (out=%s)", scripts, out)
	}
}

func TestRunPythonSandboxBlocksOutsideWrite(t *testing.T) {
	a := newRunPyTestApp(t)
	skipIfNoPyRuntime(t, a)
	sess := &SessionMeta{ID: "testpy0004abcdef", Workspace: a.Cfg.Workspace, Permission: permWorkspace}
	// 工作区根之上（C:\ 根）：受限令牌下应被拒——脚本非零退出、输出带拒绝痕迹
	target := `C:\__jx_py_sandbox_probe.txt`
	out, err := runPyCall(t, a, sess, map[string]any{
		"code": "open(r'" + target + "', 'w').write('x')", "name": "escape_probe", "timeout_sec": 30,
	})
	_ = os.Remove(target) // 双保险：万一环境异常真写进去了，也别留垃圾
	combined := out
	if err != nil {
		combined += err.Error()
	}
	if !strings.Contains(combined, "denied") && !strings.Contains(combined, "PermissionError") {
		t.Fatalf("workspace-write 档写工作区外应被沙箱拒绝:\n%s\nerr=%v", out, err)
	}
}

func TestCleanupPySessionDirs(t *testing.T) {
	ws := t.TempDir()
	tmp := filepath.Join(ws, ".jxai", "tmp")
	old := filepath.Join(tmp, "0123456789abcdef")   // 会话 ID 形状 + 超龄 → 清
	fresh := filepath.Join(tmp, "0123456789abcdee") // 会话 ID 形状 + 新 → 留
	other := filepath.Join(tmp, "uitest")           // 非会话形状 + 超龄 → 留（别的任务暂存物）
	for _, d := range []string{old, fresh, other} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	past := time.Now().Add(-8 * 24 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(other, past, past); err != nil {
		t.Fatal(err)
	}
	cleanupPySessionDirs(ws)
	if _, err := os.Stat(old); err == nil {
		t.Fatal("超龄的会话目录应被清理")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Fatal("未超龄的会话目录不应被清理")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal("非会话形状的目录不在清理范围")
	}
}
