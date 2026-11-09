package main

// 新装机实测三坑的回归测试：
//  1. .jxai/tmp 观察策略豁免——tmp 残留文件跨会话存在，而观察集每会话清零、
//     目录检索又不扫 .jxai，覆盖不得再被观察策略拦下；工作区普通既有文件
//     的观察策略保持原样。
//  2. winEncodingNotes——无 BOM .ps1 的坑与全部出路必须写清；shell=pwsh
//     出路只在探测到 pwsh 7 时给。
//  3. skillRuntimePromptText——便携 Python 绝对路径与「不要用系统
//     python/py」必须在场。
//  4. systemPrompt 装配——引擎定位句全模式在场；运行时段只进本地执行
//     模式（远程会话命令在远程主机执行，assistant 不带技能工具）。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriteJxAITmpObservationExemption(t *testing.T) {
	a := newCapTestApp(t, "data-jxai-tmp-test") // 带 grants/approvals 初始化：write 要签发授权凭证
	ws, _ := filepath.Abs(roTestWS(t))
	sess := &SessionMeta{ID: "s1", Mode: "task", Permission: permFull, Workspace: ws,
		CreatedAt: "2026-10-14 09:00:00"}
	mustCreateSession(t, a, sess)

	// tmp 下预置跨会话残留旧文件 + 工作区根的普通既有文件（本会话都没 read 过）
	stale := filepath.Join(ws, ".jxai", "tmp", "dump.py")
	if err := os.MkdirAll(filepath.Dir(stale), 0o755); err != nil {
		t.Fatalf("建 tmp 失败: %v", err)
	}
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatalf("预置残留文件失败: %v", err)
	}
	note := filepath.Join(ws, "说明.txt")
	if err := os.WriteFile(note, []byte("old"), 0o644); err != nil {
		t.Fatalf("预置普通文件失败: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	tc := &toolContext{ctx: ctx, app: a, sess: sess, toolName: "write"}

	// tmp 残留文件：未 read 直接整体覆盖应成功（豁免）
	if _, err := runWriteFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"new"}`, stale))); err != nil {
		t.Fatalf("tmp 残留文件覆盖被观察策略拦截: %v", err)
	}
	// 工作区普通既有文件：未 read 覆盖仍被拒（观察策略回归）
	if _, err := runWriteFile(tc, json.RawMessage(fmt.Sprintf(`{"path":%q,"content":"new"}`, note))); err == nil || !strings.Contains(err.Error(), "观察策略") {
		t.Fatalf("普通既有文件的观察策略应保持: err=%v", err)
	}
}

func TestInJxAITmp(t *testing.T) {
	a := newPermTestApp(t, "data-jxai-loc-test")
	ws, _ := filepath.Abs(roTestWS(t))
	tc := &toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "s1", Mode: "task", Workspace: ws}}

	yes := []string{
		filepath.Join(ws, ".jxai", "tmp", "dump.py"),
		filepath.Join(ws, ".jxai", "tmp", "sub", "x.py"),
	}
	if isWindowsRuntime { // 大小写折叠与模型给的路径写法差异
		yes = append(yes, strings.ToLower(filepath.Join(ws, ".jxai", "tmp", "dump.py")),
			filepath.Join(strings.ToLower(ws), ".jxai", "tmp", "dump.py"))
	}
	for _, p := range yes {
		if !tc.inJxAITmp(p) {
			t.Fatalf("%s 应视为 tmp 之下", p)
		}
	}
	no := []string{
		filepath.Join(ws, ".jxai", "snapshots", "s1", "1.bin"),
		filepath.Join(ws, ".jxai", "other", "x"),
		filepath.Join(ws, "doc.txt"),
		ws + string(filepath.Separator) + ".." + string(filepath.Separator) + "elsewhere.py",
	}
	for _, p := range no {
		if tc.inJxAITmp(p) {
			t.Fatalf("%s 不应视为 tmp 之下", p)
		}
	}
	// 远程会话恒 false：.jxai 是本机系统目录
	r := &toolContext{ctx: context.Background(), app: a, sess: &SessionMeta{ID: "s2", Mode: "task",
		Remote: &RemoteWorkspace{Host: "h", Dir: "/srv"}}}
	if r.inJxAITmp("/srv/.jxai/tmp/x.py") {
		t.Fatal("远程会话不应豁免")
	}
}

func TestWinEncodingNotes(t *testing.T) {
	base := winEncodingNotes(false)
	// 文案口径（2026-11-10 编码自适应改造后）：输出与读文件都钉 UTF-8、
	// 命令里文件自动探测切 GBK、乱码只读命令重跑一次、edit 保持原编码。
	wantAll(t, base, "win 编码规则", "输出与读文件", "PYTHONUTF8=1", "GetEncoding(936)",
		"UTF-8 无 BOM", "ANSI/GBK", "转存带 BOM", ".cmd/.bat", "Out-File / Set-Content",
		"探测编码", "重跑一次", "保持原文件编码")
	if strings.Contains(base, "chcp") {
		t.Fatalf("cmd 路径不注入，不应再让模型手动 chcp:\n%s", base)
	}
	if strings.Contains(base, "shell=pwsh") {
		t.Fatalf("未装 pwsh 7 不应给 shell=pwsh 出路:\n%s", base)
	}
	if with7 := winEncodingNotes(true); !strings.Contains(with7, "shell=pwsh") {
		t.Fatalf("装有 pwsh 7 应给 shell=pwsh 出路:\n%s", with7)
	}
}

func TestSkillRuntimePromptText(t *testing.T) {
	s := skillRuntimePromptText(`C:\Apps\坚祥数字助理GO\技能\_runtime\python\python.exe`)
	wantAll(t, s, "运行时段", "C:/Apps/坚祥数字助理GO/技能/_runtime/python/python.exe",
		"无需用户安装环境", "python/py", "office_skill", "自动联网装配",
		"涉及中文、引号、JSON 的处理建议用 run_python 提交 Python 脚本",
		"run_command 与 run_python 都可用",
		"PowerShell 主要用于 Windows 原生交互")
}

func TestSystemPromptSelfLoc(t *testing.T) {
	a := newPermTestApp(t, "data-selfloc-test")
	a.Cfg.DataDir = roTestWS(t) // 空目录：杜绝 data/prompts 引用文件覆盖干扰
	ws, _ := filepath.Abs(roTestWS(t))

	sp := a.systemPrompt(&SessionMeta{ID: "s1", Mode: "task", Workspace: ws})
	wantAll(t, sp, "task 系统提示词", "本助手引擎程序：", "## 内置技能运行时", "_runtime/python/python.exe")

	// assistant 模式：定位句在场（全模式），运行时段不在（没有技能工具）
	asst := a.systemPrompt(&SessionMeta{ID: "s2", Mode: "assistant", Workspace: ws})
	if !strings.Contains(asst, "本助手引擎程序：") || strings.Contains(asst, "## 内置技能运行时") {
		t.Fatal("assistant 模式的自我定位段不对：定位句应在、运行时段不应在")
	}
	// 远程会话：命令在远程主机执行，不给本机运行时段
	remote := a.systemPrompt(&SessionMeta{ID: "s3", Mode: "task", Workspace: ws,
		Remote: &RemoteWorkspace{Host: "h", Dir: "/srv"}})
	if strings.Contains(remote, "## 内置技能运行时") {
		t.Fatal("远程会话不应给本机运行时段")
	}
	if !strings.Contains(remote, "本助手引擎程序：") {
		t.Fatal("远程会话仍应有引擎定位句（引擎在本机，模型需自知所在）")
	}
}
