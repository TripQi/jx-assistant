package main

// 分块静默更新：runtime（共享便携 Python 运行时）与 skills（内置技能）两块。
//
// 为什么单独成文件：update_blocks.go 管 engine / components 两块的老逻辑，这里
// 只管「技能区」的两块，且**必须只动两个写死的路径**——
//
//	<技能库>\_runtime        ← runtime 块：整目录替换（旧运行时才会真的消失）
//	<技能库>\<内置技能名>     ← skills 块：白名单内的内置技能整目录替换
//
// 用户技能 <技能库>\user 永远不在这份代码的路径拼接范围内：不是"跳过名单"，
// 而是压根不遍历技能库目录（skills 块只按 zip 里的目录名逐个拼绝对路径，
// 名字必须命中 isBuiltinSkillName 白名单）。这条纪律由 update_runtime_test.go
// 锁死，改代码时别绕过白名单。

import (
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

const (
	// runtimeMarkerName 运行时版本标记（放在 <技能库>\_runtime\.version）
	runtimeMarkerName = ".version"
	// skillsMarkerName 内置技能包版本标记（放在 <技能库>\.skills-version）
	skillsMarkerName = ".skills-version"
	// runtimeOldPrefix 换位后旧运行时的目录名前缀（下次启动清）
	runtimeOldPrefix = "_runtime.old-"
)

// normMarkVersion 归一化块版本号：去空格、去 "py" 前缀、小写。
func normMarkVersion(v string) string {
	v = strings.ToLower(strings.TrimSpace(v))
	return strings.TrimPrefix(v, "py")
}

// readMark 读版本标记文件（读不到返回空串）。
func readMark(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

// writeMark 原子写版本标记。
func writeMark(path, version string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return writeFileAtomic(path, []byte(strings.TrimSpace(version)+"\n"))
}

// pythonTagRE 匹配 embeddable 发行版的特征文件：python314._pth / python314.dll /
// python314.zip（python39._pth 这种两位写法也吃）。
var pythonTagRE = regexp.MustCompile(`^python3(\d+)\.(?:_pth|dll|zip)$`)

// localRuntimeTag 由 <runtimeDir>\python 下的文件反推 "3.14" 形式的大版本
// （不启进程、不读 .version，专门用来判断"本机到底装的是哪个 Python"）。
// 目录不存在/认不出返回空串。
func localRuntimeTag(runtimeDir string) string {
	entries, err := os.ReadDir(filepath.Join(runtimeDir, "python"))
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if m := pythonTagRE.FindStringSubmatch(strings.ToLower(e.Name())); m != nil {
			return "3." + m[1]
		}
	}
	return ""
}

// tagMatchesVersion 判断本地大版本（"3.14"）是否与目标版本（"3.14.8"）一致。
func tagMatchesVersion(localTag, wantVersion string) bool {
	localTag = strings.TrimSpace(localTag)
	if localTag == "" {
		return false
	}
	want := normMarkVersion(wantVersion)
	return want == localTag || strings.HasPrefix(want, localTag+".")
}

// runtimeRootDir 共享运行时的绝对路径（<技能库>\_runtime）。
func (a *App) runtimeRootDir() (string, error) {
	dir, err := a.SkillsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "_runtime"), nil
}

// applyRuntimeBlock 应用 runtime 块：版本相同直接跳过（避免每版重下几百兆）；
// 否则下载 → 校验 → 解压 → 备份换位 → 冒烟 → 记账。
//
// 忙则让路：运行时正被 python 子进程占用时 rename 会失败，本轮直接返回错误
// （调用方记日志、等下一轮轮询），绝不强杀用户正在跑的技能。
func (a *App) applyRuntimeBlock(b *updateBlock, rel releaseInfo, sourceURL string) {
	if b == nil || strings.TrimSpace(b.Version) == "" || strings.TrimSpace(b.File) == "" {
		return
	}
	rt, err := a.runtimeRootDir()
	if err != nil {
		log.Printf("[更新] runtime 块跳过（技能目录不可用）: %v", err)
		return
	}
	want := normMarkVersion(b.Version)
	marker := filepath.Join(rt, runtimeMarkerName)

	if cur := readMark(marker); cur != "" {
		if normMarkVersion(cur) == want {
			log.Printf("[更新] 运行时已是最新 %s，跳过", b.Version)
			return
		}
	} else if tag := localRuntimeTag(rt); tagMatchesVersion(tag, b.Version) {
		// 老客户端没有标记文件：大版本对得上就认为不用换，补写标记免得每轮重判
		log.Printf("[更新] 运行时版本与本机一致（%s），补写标记 %s", tag, b.Version)
		_ = writeMark(marker, b.Version)
		return
	}

	url := findReleaseAssetURL(rel.Assets, b.File, sourceURL)
	if url == "" {
		log.Printf("[更新] runtime 块在附件里找不到 %s，跳过", b.File)
		return
	}
	log.Printf("[更新] 运行时开始静默更新 %s → %s（%s）", localTagOrUnknown(rt), b.Version, b.File)
	zipPath, cleanup, err := downloadUpdateFile(url, b.SHA256)
	if err != nil {
		log.Printf("[更新] 运行时下载失败（本轮放弃，本地未改动）: %v", err)
		setUpdateError("运行时静默更新失败: " + err.Error())
		return
	}
	defer cleanup()

	tmp, err := os.MkdirTemp("", "jxrt-")
	if err != nil {
		log.Printf("[更新] 运行时临时目录创建失败: %v", err)
		return
	}
	defer os.RemoveAll(tmp)
	if err := unzip(zipPath, tmp); err != nil {
		log.Printf("[更新] 运行时包解压失败（本地未改动）: %v", err)
		setUpdateError("运行时包解压失败: " + err.Error())
		return
	}
	root, err := findRootWith(tmp, filepath.Join("python", "python.exe"))
	if err != nil {
		log.Printf("[更新] 运行时包结构不对（需含 python\\python.exe）: %v", err)
		setUpdateError("运行时包结构不对: " + err.Error())
		return
	}
	// 这里曾经有一道「包内 Python 大版本 == 块版本前缀」的校验（tagMatchesVersion），
	// 2026-11-01 按用户要求删除：块版本改复合格式（py3.14.8-20261101-1）演进时，这道
	// 校验第一时间把合法包拒之门外——纯防自己人。包的完整性已有 sha256 校验兜底
	// （downloadUpdateFile 对 b.SHA256），包结构由上面的 findRootWith 保证。

	// 语音 worker 也跑在这个运行时上：先请它退场，换完它自己会重启
	if a.Bridge != nil {
		a.Bridge.KillComponent("voice")
	}

	old := filepath.Join(filepath.Dir(rt), fmt.Sprintf("%s%d", runtimeOldPrefix, time.Now().Unix()))
	if err := os.Rename(rt, old); err != nil {
		if os.IsNotExist(err) {
			old = "" // 本地还没有运行时：直接新建
		} else {
			log.Printf("[更新] 运行时目录正被占用（多半有 python 子进程在跑），本轮让路: %v", err)
			setUpdateError("运行时正在使用中，稍后自动重试")
			return
		}
	}
	if err := copyDir(root, rt); err != nil {
		_ = os.RemoveAll(rt)
		if old != "" {
			_ = os.Rename(old, rt)
		}
		log.Printf("[更新] 铺开新运行时失败（已回滚）: %v", err)
		setUpdateError("运行时铺开失败: " + err.Error())
		return
	}
	if err := smokeTestRuntime(rt); err != nil {
		_ = os.RemoveAll(rt)
		if old != "" {
			_ = os.Rename(old, rt)
		}
		log.Printf("[更新] 新运行时冒烟校验失败（已回滚）: %v", err)
		setUpdateError("新运行时不可用: " + err.Error())
		return
	}
	_ = writeMark(marker, b.Version)
	if provisionedPy != nil {
		provisionedPy.clearModuleCache() // 换了解释器，模块可用性缓存必须失效
	}
	st := a.loadUpdateBlockState()
	st.Runtime = b.Version
	_ = a.saveUpdateBlockState(st)
	log.Printf("[更新] 运行时已静默更新到 %s（旧目录 %s 下次启动清理）", b.Version, filepath.Base(old))
	a.broadcast(Event{Type: "update_ready", Title: "运行时 " + b.Version})
}

// localTagOrUnknown 日志用：本地运行时大版本，认不出就写 unknown。
func localTagOrUnknown(rt string) string {
	if tag := localRuntimeTag(rt); tag != "" {
		return tag
	}
	return "unknown"
}

// smokeTestRuntime 新运行时能不能跑起来（起一次 python，超时/非零都算失败）。
func smokeTestRuntime(rt string) error {
	py := filepath.Join(rt, "python", "python.exe")
	if _, err := os.Stat(py); err != nil {
		return fmt.Errorf("新运行时里没有 python.exe: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, py, "-c", "import sys; print('%d.%d' % sys.version_info[:2])")
	hideConsoleWindow(cmd)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return fmt.Errorf("python 启动超时（90s）")
	}
	if err != nil {
		return fmt.Errorf("python 启动失败: %v；输出: %s", err, strings.TrimSpace(string(out)))
	}
	if tag := strings.TrimSpace(string(out)); tag != "" && !strings.Contains(tag, ".") {
		return fmt.Errorf("python 输出异常: %s", tag)
	}
	return nil
}

// applySkillsBlock 应用 skills 块：zip 里每个顶层目录都必须命中内置技能白名单，
// 命中就整目录替换（这样旧版本里被删掉的文件才会真的消失）；user / _runtime /
// 任何别的名字 → 整包拒绝，一个字节都不写。
func (a *App) applySkillsBlock(b *updateBlock, rel releaseInfo, sourceURL string) {
	if b == nil || strings.TrimSpace(b.Version) == "" || strings.TrimSpace(b.File) == "" {
		return
	}
	skillsDir, err := a.SkillsDir()
	if err != nil {
		log.Printf("[更新] skills 块跳过（技能目录不可用）: %v", err)
		return
	}
	marker := filepath.Join(skillsDir, skillsMarkerName)
	if readMark(marker) == strings.TrimSpace(b.Version) {
		log.Printf("[更新] 内置技能已是最新 %s，跳过", b.Version)
		return
	}
	url := findReleaseAssetURL(rel.Assets, b.File, sourceURL)
	if url == "" {
		log.Printf("[更新] skills 块在附件里找不到 %s，跳过", b.File)
		return
	}
	zipPath, cleanup, err := downloadUpdateFile(url, b.SHA256)
	if err != nil {
		log.Printf("[更新] 内置技能包下载失败（本地未改动）: %v", err)
		setUpdateError("内置技能更新失败: " + err.Error())
		return
	}
	defer cleanup()

	tmp, err := os.MkdirTemp("", "jxsk-")
	if err != nil {
		log.Printf("[更新] 内置技能临时目录创建失败: %v", err)
		return
	}
	defer os.RemoveAll(tmp)
	if err := unzip(zipPath, tmp); err != nil {
		log.Printf("[更新] 内置技能包解压失败（本地未改动）: %v", err)
		setUpdateError("内置技能包解压失败: " + err.Error())
		return
	}

	// 白名单校验：先全部查完再动手，避免"删了一半发现包里混了 user"
	names, err := planSkillDirs(tmp)
	if err != nil {
		log.Printf("[更新] 内置技能包被拒绝（安全校验未过，本地未改动）: %v", err)
		setUpdateError("内置技能包被拒绝: " + err.Error())
		return
	}
	applied := make([]string, 0, len(names))
	for _, name := range names {
		src := filepath.Join(tmp, name)
		if len(names) == 1 {
			// 允许发布端把技能目录再套一层（zip 根 → 单个技能目录）
			if _, statErr := os.Stat(filepath.Join(src, "SKILL.md")); statErr != nil {
				if inner := filepath.Join(src, name); dirExists(inner) {
					src = inner
				}
			}
		}
		dst := filepath.Join(skillsDir, name) // 名字已过白名单，绝不会是 user/_runtime
		if err := os.RemoveAll(dst); err != nil {
			log.Printf("[更新] 内置技能 %s 旧目录删除失败（跳过该技能）: %v", name, err)
			setUpdateError("内置技能 " + name + " 删除旧目录失败: " + err.Error())
			continue
		}
		if err := copyDir(src, dst); err != nil {
			log.Printf("[更新] 内置技能 %s 铺开失败: %v", name, err)
			setUpdateError("内置技能 " + name + " 铺开失败: " + err.Error())
			continue
		}
		applied = append(applied, name)
	}
	if len(applied) == 0 {
		return
	}
	_ = writeMark(marker, b.Version)
	st := a.loadUpdateBlockState()
	st.Skills = b.Version
	_ = a.saveUpdateBlockState(st)
	log.Printf("[更新] 内置技能已静默更新 %s → %s", strings.Join(applied, "、"), b.Version)
	a.broadcast(Event{Type: "skills_updated", Title: strings.Join(applied, "、")})
}

// planSkillDirs 校验解压目录：顶层只能是内置技能目录名，且至少一个。
// 返回要替换的技能名列表（升序）。
func planSkillDirs(root string) ([]string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := []string{}
	for _, e := range entries {
		if !e.IsDir() {
			if strings.HasPrefix(e.Name(), ".") {
				continue // .DS_Store 之类
			}
			return nil, fmt.Errorf("包内出现顶层文件 %q（应当只有内置技能目录）", e.Name())
		}
		name := e.Name()
		if !isBuiltinSkillName(name) {
			return nil, fmt.Errorf("包内含非内置技能目录 %q（白名单只允许内置技能，用户技能永不热更）", name)
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("包里没有任何技能目录")
	}
	sortStrings(names)
	return names, nil
}

// sortStrings 忽略大小写的原地插入排序（短切片足够，刻意不引 sort 包）。
// 从 sandbox_windows.go 移来：update_runtime 在全平台构建都要用它排序技能
// 目录名，留在 Windows 专属文件里会让 Linux 构建缺符号。
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && strings.ToLower(s[j]) < strings.ToLower(s[j-1]); j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

// FinalizeRuntimeState 启动时结算运行时状态（幂等，任何失败不阻断启动）：
//  1. 清掉上次换位留下的 _runtime.old-*（当前进程已不再占用）；
//  2. 兜底：本机运行的 Python 大版本与本 exe 期望的不一致（典型：从 3.12 时代
//     的旧版升级上来、又没走 runtime 块）→ 整目录删掉，下次用技能按新版本
//     联网装配。用户技能一个字节都不动。
func (a *App) FinalizeRuntimeState() {
	rt, err := a.runtimeRootDir()
	if err != nil {
		return
	}
	parent := filepath.Dir(rt)
	if entries, err := os.ReadDir(parent); err == nil {
		for _, e := range entries {
			name := e.Name()
			if !strings.HasPrefix(name, runtimeOldPrefix) && !strings.HasPrefix(name, "_runtime.stale-") {
				continue
			}
			p := filepath.Join(parent, name)
			if err := os.RemoveAll(p); err != nil {
				log.Printf("[更新] 旧运行时目录清理失败（下次启动再试）: %v", err)
			} else {
				log.Printf("[更新] 已清理换位残留的旧运行时目录 %s", name)
			}
		}
	}
	tag := localRuntimeTag(rt)
	if tag == "" || tagMatchesVersion(tag, pythonVersion) {
		return
	}
	log.Printf("[启动] 本机运行时是 Python %s，当前版本需要 %s —— 清理旧运行时，首次用技能时按新版本装配", tag, pythonVersion)
	if err := os.RemoveAll(rt); err != nil {
		log.Printf("[启动] 旧运行时清理失败（不影响使用，下次再试）: %v", err)
	}
}
