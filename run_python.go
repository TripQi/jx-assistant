package main

// run_python 工具：把「写一段 Python 跑一下」做成一步——模型把脚本全文交
// 给工具，工具落盘到本会话的脚本目录（.jxai/tmp/<会话id>/，会话删除时随
// handleDeleteSession 一并清理）、用内置便携 Python 执行、原样返回输出。
//
// 动机：JSON / 引号 / 中文转义 / 多步逻辑放进 shell 命令行，在 Windows
// PowerShell 5.1 上是高频翻车点（参数引号被吃、内联 JSON 残缺、转义地狱）。
// 脚本落盘后这些坑整体消失（write 落 UTF-8 无 BOM，Python 3 源码默认 UTF-8，
// 正好绝配）。提示词按「建议」引导模型优先用它，但 run_command 一样可用——
// 工具层不做硬限制，两条路都能走。
//
// 执行链路完全复用 run_command 那一套（child.go 统一入口）：同一把会话权限
// 锁（permAction exec）、同一个 OS 级沙箱（受限令牌/Landlock）、同一份
// PowerShell 5.1 编码注入（PYTHONUTF8=1 等对 python.exe 同样生效）、同一套
// 输出捕获/截断/超时与「运行中的程序」登记。命令形态是 Shell 而非 Argv：
// Argv 直跑形态不经受限沙箱（runArgv 是给 git 等内部工具的通道），用它会让
// workspace-write 档形同虚设——Shell 形态多一层 powershell 启动开销，但边界
// 语义与 run_command 完全一致。路径两端都是受控字符（技能目录 + 会话 ID /
// 白名单文件名），双引号包裹即可安全转义。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// pyNameOK 脚本文件名的合法字符集（字母数字开头，允许 -_ ；不含点与路径
// 分隔符，杜绝 ../ 与扩展名技巧）。不合法的 name 一律忽略、改用时间戳名。
var pyNameOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// pySessionDir 本会话的脚本目录：工作区 .jxai/tmp/<会话id>/。按会话分目录
// 让多会话并行互不覆盖，删除会话时也能整目录回收。
func pySessionDir(t *toolContext) string {
	sid := "shared"
	if t.sess != nil && t.sess.ID != "" {
		sid = t.sess.ID
	}
	return filepath.Join(t.workspace(), ".jxai", "tmp", sid)
}

func runPythonTool() Tool {
	return Tool{
		Spec: toolSpec("run_python",
			"执行一段 Python 脚本并返回输出（与 run_command 同一套权限与 OS 级沙箱）。"+
				"建议用于带 JSON、引号、中文转义、正则或多步逻辑的操作：把完整脚本写在 code 里，"+
				"工具自动落盘到本会话脚本目录（.jxai/tmp/<会话id>/）并用内置便携 Python 执行——"+
				"shell 命令行内联这些内容在 Windows PowerShell 5.1 上容易踩引号与编码坑；"+
				"需要改动重跑时用 path 指向上次返回的脚本路径（配合 edit 改脚本再跑更顺手）。"+
				"单一工具链调用（go build / git status 等）直接 run_command 即可。"+
				"只用标准库与运行时已内置的包（office/pdf/ocr 技能的库也可用）；"+
				"首次使用自动联网装配便携运行时（几分钟），之后离线可用；不要去找系统的 python/py。"+
				"沙箱与升级机制同 run_command：被拒后可带 sandbox_permissions 与 justification 重试一次。"+
				"——工具桥（编排原语）：脚本里 import jx 可直接调本会话工具（jx.call(\"read\", {...}) 等，"+
				"白名单：read/glob/grep/list_dir/write/edit/snapshot_file/run_command/web_fetch/web_search/read_skill），"+
				"把「N 步碎 tool call」合成一个脚本回合：循环、汇总、中间筛选都在脚本里做，"+
				"只有最终结论进上下文；需要弹审批的操作（如沙箱升级）桥上会被拒，回前台做。"+
				"还能 jx.dispatch(task) 派后台子任务（自足描述）+ jx.wait(session_id) 等回报——"+
				"一个脚本并行编排多个子代理并汇总。"+
				"——后台执行：run_in_background=true 时立即返回作业 id、脚本不限时继续跑，"+
				"跑完系统自动给你【后台脚本回报】并唤醒你继续（长任务/并行推进用它，timeout_sec 后台忽略）。",
			objSchema(map[string]any{
				"code":                strProp("Python 脚本全文（与 path 二选一）"),
				"name":                strProp("脚本文件名，可选：字母数字与 -_（如 http_test）；缺省按时间戳命名"),
				"path":                strProp("重跑本会话已落盘的脚本（.jxai/tmp/<会话id>/ 下的相对或绝对路径；与 code 二选一）"),
				"timeout_sec":         intProp("超时秒数，默认 60，上限 600；后台作业忽略"),
				"run_in_background":   boolProp("true 时后台执行并立即返回作业 id（仅本地）：不限时、可继续做别的事，结束后自动收到【后台脚本回报】并被唤醒继续；可 job_output 中途查看、job_kill 终止"),
				"sandbox_permissions": strProp("仅用于被沙箱拒绝后的同轮重试：请求的更宽档位（workspace-write 或 danger-full-access），必须搭配 justification"),
				"justification":       strProp("与 sandbox_permissions 搭配：一句话向用户解释为什么需要更宽权限"),
			})),
		Run: runPython,
	}
}

func runPython(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Code               string `json:"code"`
		Name               string `json:"name"`
		Path               string `json:"path"`
		TimeoutSec         int    `json:"timeout_sec"`
		RunInBackground    bool   `json:"run_in_background"`
		SandboxPermissions string `json:"sandbox_permissions"`
		Justification      string `json:"justification"`
		Com                bool   `json:"com"` // 隐藏参数：不进 schema，COM 专用（见 runPython 内通道注释）
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	if t.isRemote() {
		return "", fmt.Errorf("远程工作区会话暂不支持 run_python（脚本落盘在本机）：请改用 run_command 在远端执行")
	}

	// 定位脚本：path 重跑本会话已有脚本；code 落盘新脚本。二者至少给一个。
	dir := pySessionDir(t)
	var scriptPath string
	if sp := strings.TrimSpace(p.Path); sp != "" {
		abs, err := t.resolve(sp)
		if err != nil {
			return "", err
		}
		// 只放行本会话脚本目录之内（防把工作区里任意 py 拿进来跑，语义混淆）
		if rel, rerr := filepath.Rel(dir, abs); rerr != nil || rel == ".." ||
			strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
			return "", fmt.Errorf("path 必须位于本会话脚本目录内：%s（上次执行的返回里带有完整路径）", dir)
		}
		if _, serr := os.Stat(abs); serr != nil {
			return "", fmt.Errorf("脚本不存在: %s", abs)
		}
		scriptPath = abs
	} else if strings.TrimSpace(p.Code) != "" {
		if len(p.Code) > 512*1024 {
			return "", fmt.Errorf("脚本过大（%d 字节，上限 512KB）：请拆分或落盘后用 path 执行", len(p.Code))
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", fmt.Errorf("创建脚本目录失败: %w", err)
		}
		name := strings.TrimSpace(p.Name)
		if !pyNameOK.MatchString(name) {
			name = "py-" + time.Now().Format("150405.000")
		}
		scriptPath = filepath.Join(dir, name+".py")
		if err := os.WriteFile(scriptPath, []byte(p.Code), 0o644); err != nil {
			return "", fmt.Errorf("脚本落盘失败: %w", err)
		}
	} else {
		return "", fmt.Errorf("code 与 path 至少提供一个")
	}

	// 内置便携运行时（ensureSkillRuntime 的缓存条件已按 modules 核对，
	// 这里取基础运行时不会短路 office 等技能后续的依赖补装）
	//
	// 隐藏参数 com=true：Office/WPS COM 自动化专用（老 .doc/.xls/.ppt、WPS 格式
	// ——受限令牌沙箱内 COM 客户端挂起或报 429，跑不了）。通道设计（产品决策）：
	//   - 刻意不进 schema：工具定义对模型隐身，引擎入口无 schema 校验、参数
	//     原样透传（json.Unmarshal 忽略未知字段），唯一告知点是 office 技能
	//     com 分册（office_skill kind=com）——不相干会话不知道这条路的存在；
	//   - 不做内容预检：假设模型听话，靠会话记录（脚本全文落库）审计兜底；
	//   - 只读档仍拒绝（COM 自动化必然伴随打开/保存文件），报错指路切档；
	//   - 生效时直接签发 full 策略凭证：runChild 对 permFull 走宿主直跑
	//     （runUnconfinedLocal），其余命令通道不受影响、照旧进沙箱。
	// 检查放在装配运行时之前：越权先拒，不为只读会话白付装配等待。
	if p.Com {
		if err := requireNotReadOnly(t, "run_python 的 COM 自动化通道（com）"); err != nil {
			return "", err
		}
	}
	py, err := t.app.ensureSkillRuntime(nil, nil)
	if err != nil {
		return "", fmt.Errorf("内置 Python 运行时不可用（%v）。装配需联网（首次约几分钟）：稍后重试，或先调用 pdf / ocr / office_skill 之一触发装配；不要改用系统的 python/py", err)
	}

	// 与 run_command 同一把权限锁（exec：先按会话档执行，拒绝驱动升级）。
	// 命令按平台构造：Windows 走 powershell -Command，`"exe" "script"` 在其
	// 语法里是两个相邻字符串（非法），必须用调用操作符 & 引导；sh 形态无此问题。
	// PYTHONPATH 指向会话脚本目录：Python 3.14 起不再把脚本目录自动加入
	// sys.path，且便携运行时是 ._pth 隔离形态（PYTHONPATH 会被忽略）——所以
	// 实际注入走 sitecustomize（bridge.go 已写进 site-packages），这里设置
	// 的 JXPY_SESSION_DIR 是它读的环境变量。在命令内设置（$env: 前缀 /
	// 前置 VAR=），不依赖沙箱环境白名单传递。
	// 授权分叉：com=true 走隐藏的宿主直跑通道（full 凭证，见上方通道注释），
	// 其余命令照旧 authorize 进沙箱。
	quote := func(s string) string { return `"` + s + `"` }
	display := quote(py.Exe) + " " + quote(scriptPath)
	if isWindowsRuntime {
		display = "$env:JXPY_SESSION_DIR=" + quote(dir) + "; & " + display
	} else {
		display = "JXPY_SESSION_DIR=" + quote(dir) + " " + display
	}
	label := "run_python"
	var grant *Grant
	if p.Com {
		label = "run_python(com)"
		grant = t.app.issueGrant(t.sess.ID, "run_python", "exec",
			"COM 自动化（宿主直跑）："+truncateRunes(scriptPath, 160), permFull)
	} else {
		grant, err = t.authorize(permAction{
			Op: "exec", Tool: "run_python", Command: display, Cwd: t.workspace(),
			EscalateTo: p.SandboxPermissions, Justification: p.Justification,
		})
		if err != nil {
			return "", err
		}
	}
	if err := t.app.consumeGrant(grant); err != nil {
		return "", err
	}

	// 工具桥装配（前台与后台共用）：生成 jx.py（含一次性 token）+ 确保运行时
	// sitecustomize 就位，脚本内 import jx 即可调白名单工具/派子任务。装配放
	// 在权限确认之后——被拒不白装配。
	bridgeKey, bridgeDone, berr := t.app.bridgePrepare(t, py.Exe)
	if berr != nil {
		return "", fmt.Errorf("工具桥装配失败（脚本仍可跑，只是没有 jx 模块）：%w", berr)
	}

	// 后台作业：立即返回，不限时；跑完由 onDone 投【后台脚本回报】并唤醒本会话。
	if p.RunInBackground {
		j, jerr := t.app.startJob(t, display, "", t.workspace(), grant.Policy,
			"python:"+scriptPath, func(j *jobRun) { t.app.reportPythonJob(t, j, scriptPath, bridgeKey, bridgeDone) })
		if jerr != nil {
			bridgeDone()
			return "", sandboxErrHint(jerr)
		}
		return fmt.Sprintf("后台脚本已启动：%s（沙箱 %s，不限时）。脚本：%s\n"+
			"- 跑完系统自动给你【后台脚本回报】并唤醒你继续（脚本内 jx.dispatch 派的子任务回报会一并带上来）\n"+
			"- job_output 可中途查看输出；job_kill 终止\n现在可以继续做别的事。", j.ID, grant.Policy, scriptPath), nil
	}

	timeout := time.Duration(60) * time.Second
	if p.TimeoutSec > 0 {
		timeout = time.Duration(min(p.TimeoutSec, 600)) * time.Second
	}
	out, err := t.app.runChild(t.ctx, sandboxRun{
		Dir: t.workspace(), Command: display,
		Timeout: timeout, Policy: grant.Policy, Workspace: t.workspace(),
		Remote: t.sess, Label: label, Model: true,
	})
	bridgeDone() // 前台脚本结束：注销桥 token；未取走的子任务回报转交常规投递
	if err != nil {
		return "", sandboxErrHint(err)
	}
	return "脚本：" + scriptPath + "\n" + out, nil
}

// reportPythonJob 是后台脚本作业的收尾回调（jobs.go startJob 的 onDone）：
// 先释放工具桥（脚本没 wait 的子任务回报转交常规投递，不丢），再给本会话投
// 【后台脚本回报】。用户/系统终止的作业改走 NoKick 投递（对齐派发回报的叫停
// 纪律：用户刚停的东西不被自动叫醒）。
func (a *App) reportPythonJob(t *toolContext, j *jobRun, scriptPath, bridgeKey string, bridgeDone func()) {
	bridgeDone()
	out := j.buf.String()
	if len(out) > jobBufMax {
		out = out[:jobBufMax]
	}
	text := fmt.Sprintf("【后台脚本回报】作业 %s 已结束（%s，运行 %s）。\n脚本：%s\n\n输出：\n%s\n"+
		"（run_python 后台启动；job_output 可再查。脚本若派了 jx.dispatch 子任务，它们的回报在上面输出里或已单独转交。）",
		j.ID, j.Status(), time.Since(j.startedAt).Round(time.Second), scriptPath, out)
	m := t.sess
	killed := strings.Contains(j.Status(), "被终止")
	if killed {
		a.deliverToSessionNoKick(m, text, kindAiUser, "pyjob")
		return
	}
	a.startReportRound(m, text, "pyjob")
}

// startupPySessionCleanup 启动期异步清理各工作区 .jxai/tmp 下超龄的会话
// 脚本目录（收集口径对齐 startupIntegrityLabelRestore：配置工作区 + 全部
// 会话各自的工作区）。失败静默——只是兜底回收，不影响启动。
func (a *App) startupPySessionCleanup(primary string) {
	roots := map[string]bool{}
	if p := strings.TrimSpace(primary); p != "" {
		roots[p] = true
	}
	if a.Store != nil {
		if ss, err := a.Store.List(); err == nil {
			for _, s := range ss {
				if p := strings.TrimSpace(s.Workspace); p != "" {
					roots[p] = true
				}
			}
		}
	}
	for ws := range roots {
		cleanupPySessionDirs(ws)
	}
}

// cleanupPySessionDirs 启动时清理 .jxai/tmp 下形如会话 ID、超龄的孤儿脚本
// 目录（对齐快照按天过期的习惯）：会话删除时 handleDeleteSession 已即时
// 清过一轮，这里兜底「会话库已没了但目录残留」的历史遗留与异常退出。
// 只动会话 ID 形状的目录——tmp 下其他任务的暂存物保持既有「跨会话残留」
// 约定，不在清理范围。
func cleanupPySessionDirs(ws string) {
	tmp := filepath.Join(ws, ".jxai", "tmp")
	entries, err := os.ReadDir(tmp)
	if err != nil {
		return // 没建过 .jxai/tmp 是常态
	}
	sessionIDShape := regexp.MustCompile(`^[0-9a-f]{12,24}$`)
	const keep = 7 * 24 * time.Hour
	for _, e := range entries {
		if !e.IsDir() || !sessionIDShape.MatchString(e.Name()) {
			continue
		}
		info, serr := e.Info()
		if serr != nil {
			continue
		}
		if time.Since(info.ModTime()) > keep {
			_ = os.RemoveAll(filepath.Join(tmp, e.Name()))
		}
	}
}
