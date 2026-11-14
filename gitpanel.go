package main

// Git 面板（组件 git-panel 的宿主半边）：识别会话工作区所在的 git 仓库，
// 为右栏面板提供只读数据与三个安全的本地写操作。
//
// 执行缝：直接 os/exec 调 git，argv 全部由服务端拼装、永不过 shell；
// 浏览器能影响的只有四样，且各自有独立校验：
//   session（经 Store 查会话拿工作区）、repo（必须在本服务自己扫描出的候选列表里）、
//   paths（safePathspecs 逐项校验后放在 -- 之后）、hash（4..40 位十六进制）。
// 写边界：只动 index 与本地历史的操作——add（暂存）/ restore --staged（取消暂存，
// 空仓库回退 rm --cached）/ rm --cached（移出跟踪变未跟踪）/ commit（提交）/
// checkout（切换本地分支）/ sync（与远程同步，细分为 pull=fetch+快进/自动合并、
// push=仅本地领先且远程无新提交、空=整套先拉后推；无法自动完成的情形一律
// 原样退回并提示交给对话）。不做
// reset/revert/discard/stash/rebase 等会改写工作区或历史的操作。
// 组件未启用时全部接口一律 404（与 todo-scheduler 同一语义）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

const gitComponentID = "git-panel"

// 单条 git 命令的超时：只读 15 秒（大仓库 status 可能接近秒级，兜底即可），
// 提交 60 秒（用户可能配了提交钩子），AI 摘要 45 秒；同步（fetch/push）走网络，
// 给到 2 分钟。
const (
	gitReadTimeout    = 15 * time.Second
	gitCommitTimeout  = 60 * time.Second
	gitSummaryTimeout = 45 * time.Second
	gitSyncTimeout    = 120 * time.Second
)

// stdout 保留上限（超过即截断并标记）；diff 给浏览器的展示上限更小；
// stderr 只留尾巴当错误原因。
const (
	gitStdoutMaxBytes = 4 << 20
	gitStderrMaxBytes = 16 << 10
	gitDiffMaxBytes   = 200 << 10
	gitAddedMaxLines  = 4000
	gitStatusMax      = 1000
	gitLogDefault     = 60
	gitLogMax         = 300
	// AI 摘要喂给模型的 diff 文本上限。
	gitSummaryMaxChars = 24000
	// 仅探远程状态（面板刷新时后台 fetch，op=fetch）：短超时，失败静默。
	gitFetchTimeout = 20 * time.Second
)

// 候选仓库扫描：预算与跳过目录对齐数字助理2 的 dsh-git-panel 插件。
const (
	gitScanBudget   = 400
	gitScanMaxFound = 12
	gitScanDepth    = 2
)

// 候选列表缓存时长：面板连续刷新时不必反复扫目录。
const gitScanTTL = 10 * time.Second

// git log / for-each-ref 的字段/记录分隔符（控制字符不会出现在提交信息里）。
const (
	gitFieldSep = "\x1f"
	gitRecSep   = "\x1e"
)

// 提交信息字符数上限（单个 -m 参数的安全边界）。
const gitMessageMaxChars = 4000

// pathspec 列表条数上限。
const gitPathspecMax = 500

var gitHashRe = regexp.MustCompile(`^[0-9a-fA-F]{4,40}$`)

// ---------- 数据结构 ----------

// gitFileEntry 是一个改动文件的状态（porcelain 字母表：M/A/D/R/C/T/U，'.' 未变）。
type gitFileEntry struct {
	Index    string `json:"index"`
	Worktree string `json:"worktree"`
	Mark     string `json:"mark"` // 未跟踪（??）时的原始状态码
	Path     string `json:"path"`
	Original string `json:"original,omitempty"` // 重命名/复制的原路径
}

// gitRepoRef 是一个候选仓库。
type gitRepoRef struct {
	Path string `json:"path"`
	Name string `json:"name"`
}

// gitRef 是图谱提交上的装饰引用（分支/远程/标签）。
type gitRef struct {
	Name    string `json:"name"`
	Kind    string `json:"kind"` // head | remote | tag
	Current bool   `json:"current,omitempty"`
}

// gitCommitInfo 是图谱里的一次提交（Parents 供前端计算泳道）。
type gitCommitInfo struct {
	Hash    string   `json:"hash"`
	Parents []string `json:"parents"`
	Author  string   `json:"author"`
	Date    string   `json:"date"`
	Subject string   `json:"subject"`
	Refs    []gitRef `json:"refs,omitempty"`
}

// gitStatusResult 是 status 端点的完整返回。
type gitStatusResult struct {
	Repos      []gitRepoRef   `json:"repos"`
	Repo       string         `json:"repo"`
	Branch     string         `json:"branch"`
	OID        string         `json:"oid"`
	Upstream   string         `json:"upstream"`
	Ahead      int            `json:"ahead"`
	Behind     int            `json:"behind"`
	Staged     []gitFileEntry `json:"staged"`
	Unstaged   []gitFileEntry `json:"unstaged"`
	Untracked  []gitFileEntry `json:"untracked"`
	Conflicted []gitFileEntry `json:"conflicted"`
	Truncated  bool           `json:"truncated"`
	Reason     string         `json:"reason"` // 没有仓库时的人话原因
}

// gitDiffResult 是单个文件/单次提交的 diff。
type gitDiffResult struct {
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
	Binary    bool   `json:"binary"`
	Empty     bool   `json:"empty"`
}

// gitBranchInfo 是一条分支（本地或远程）。
type gitBranchInfo struct {
	Full     string `json:"-"` // 完整 refname（过滤远程的符号 HEAD 用）
	Name     string `json:"name"`
	Short    string `json:"short"`
	Subject  string `json:"subject"`
	Upstream string `json:"upstream,omitempty"`
	Ahead    int    `json:"ahead"`
	Behind   int    `json:"behind"`
	Gone     bool   `json:"gone,omitempty"` // 上游已删除
	Current  bool   `json:"current,omitempty"`
}

// ---------- 执行缝 ----------

// limitedWriter 写满即停（不再吞内存）；truncated 记录是否发生截断。
type limitedWriter struct {
	buf       strings.Builder
	max       int
	truncated bool
}

func (lw *limitedWriter) Write(p []byte) (int, error) {
	if lw.truncated {
		return len(p), nil
	}
	if room := lw.max - lw.buf.Len(); len(p) > room {
		if room > 0 {
			lw.buf.Write(p[:room])
		}
		lw.truncated = true
		return len(p), nil
	}
	lw.buf.Write(p)
	return len(p), nil
}

// gitRun 跑一条 git 命令：返回 stdout 原文与是否被上限截断。
// argv 由各调用点用服务端常量拼装；错误带 git 的 stderr 尾巴，用户可读。
func (a *App) gitRun(ctx context.Context, dir string, timeout time.Duration, args ...string) (string, bool, error) {
	git, err := exec.LookPath("git")
	if err != nil {
		return "", false, errors.New("没有找到 git 命令：请安装 Git（https://git-scm.com）后再使用 Git 面板")
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, git, args...)
	cmd.Dir = dir
	// 只读环境：不弹凭据提示、不抢 index 锁；LC_ALL=C 让报错与解析稳定。
	// 保留用户配置（提交身份 user.name/email 来自这里），只压掉交互项。
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"LC_ALL=C",
	)
	gitHideWindow(cmd)
	var out, errOut limitedWriter
	out.max, errOut.max = gitStdoutMaxBytes, gitStderrMaxBytes
	cmd.Stdout, cmd.Stderr = &out, &errOut
	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return out.buf.String(), out.truncated, errors.New("git " + args[0] + " 超时未结束，已终止")
		}
		msg := strings.TrimSpace(errOut.buf.String())
		if msg == "" {
			msg = err.Error()
		}
		return out.buf.String(), out.truncated, errors.New("git " + args[0] + " 失败：" + gitTail(msg, 600))
	}
	return out.buf.String(), out.truncated, nil
}

// gitErr 把一条 git 失败包装成带上下文的人话错误。
func gitErr(what string, err error) error { return errors.New(what + "：" + err.Error()) }

// gitTail 取文本尾巴（错误原因常在末段）。
func gitTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// ---------- 仓库定位 ----------

// gitIsRepoRoot 目录是否是仓库根（worktree/submodule 的 .git 文件形式也算）。
func gitIsRepoRoot(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// gitOwnRoot 从 base 逐级向上找最近的仓库根；不在任何仓库内返回空串。
func gitOwnRoot(base string) string {
	dir := base
	for i := 0; i < 32; i++ {
		if gitIsRepoRoot(dir) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
	return ""
}

// gitSkippedDir 是仓库扫描时跳过的目录（依赖与元数据目录里不会有独立仓库）。
func gitSkippedDir(name string) bool {
	switch name {
	case "node_modules", ".git", ".cache", "dist", "lib", "target", "vendor",
		"data", "__pycache__", ".venv", "venv":
		return true
	}
	return strings.HasPrefix(name, ".")
}

// gitFindRepositories 以 base 为起点向下找仓库根（深度受限、预算封顶、跳过依赖目录）。
func gitFindRepositories(base string, maxDepth int) []string {
	found := []string{}
	budget := gitScanBudget
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if budget <= 0 || len(found) >= gitScanMaxFound {
			return
		}
		if gitIsRepoRoot(dir) {
			found = append(found, dir)
			return
		}
		if depth >= maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return // 无权限或已消失的目录直接跳过
		}
		names := make([]string, 0, len(entries))
		for _, e := range entries {
			names = append(names, e.Name())
		}
		sort.Strings(names)
		for _, name := range names {
			if budget <= 0 {
				return
			}
			budget--
			if gitSkippedDir(name) {
				continue
			}
			child := filepath.Join(dir, name)
			if fi, err := os.Stat(child); err != nil || !fi.IsDir() {
				continue
			}
			walk(child, depth+1)
		}
	}
	walk(base, 0)
	return found
}

// gitScanCache 记录每个会话基目录最近一次的候选列表。
type gitScanCache struct {
	mu    sync.Mutex
	items map[string]gitScanEntry
}

type gitScanEntry struct {
	at    time.Time
	repos []gitRepoRef
}

var gitScans = gitScanCache{items: map[string]gitScanEntry{}}

// gitCandidates 汇总候选仓库：base 所属仓库优先，其次扫描它周围（上一级目录
// 向下两层）发现的并列仓库。结果按发现顺序、路径去重。
func gitCandidates(base string) []gitRepoRef {
	gitScans.mu.Lock()
	cached, ok := gitScans.items[base]
	gitScans.mu.Unlock()
	if ok && time.Since(cached.at) < gitScanTTL {
		return cached.repos
	}
	own := gitOwnRoot(base)
	scanBase := base
	if own != "" {
		scanBase = filepath.Dir(own)
	}
	paths := []string{}
	if own != "" {
		paths = append(paths, own)
	}
	seen := map[string]bool{}
	repos := []gitRepoRef{}
	for _, p := range append(paths, gitFindRepositories(scanBase, gitScanDepth)...) {
		if p == "" {
			continue
		}
		key := strings.ToLower(filepath.Clean(p))
		if seen[key] {
			continue
		}
		seen[key] = true
		repos = append(repos, gitRepoRef{Path: p, Name: filepath.Base(p)})
	}
	gitScans.mu.Lock()
	gitScans.items[base] = gitScanEntry{at: time.Now(), repos: repos}
	gitScans.mu.Unlock()
	return repos
}

// gitResolve 解析浏览器传来的会话与仓库参数：仓库只能是候选列表里的路径
// （空串=默认第一个）；没有仓库时 root 为空串、reason 给人话说明。
func (a *App) gitResolve(ctx context.Context, session, repo string) (root string, repos []gitRepoRef, reason string) {
	base := a.Cfg.Workspace
	if session != "" {
		if m, err := a.Store.Get(session); err == nil && m != nil && m.Workspace != "" {
			base = m.Workspace
		}
	}
	if base == "" {
		return "", nil, "没有可用的工作目录：请先打开或新建一个会话"
	}
	repos = gitCandidates(base)
	if len(repos) == 0 {
		return "", repos, "当前工作目录不是 git 仓库：" + base
	}
	for _, c := range repos {
		if c.Path == repo {
			return c.Path, repos, ""
		}
	}
	return repos[0].Path, repos, ""
}

// ---------- 解析（纯函数，便于单测） ----------

// gitParseStatusV2 解析 `git status --porcelain=v2 --branch -z`。
// -z 下记录以 NUL 分隔且路径不做引号转义，空格、中文、换行路径原样保留；
// 类型 2（重命名/复制）的原路径是紧随其后的独立字段。
func gitParseStatusV2(raw string) (branch, oid, upstream string, ahead, behind int, entries []gitFileEntry) {
	entries = []gitFileEntry{}
	fields := strings.Split(raw, "\x00")
	for i := 0; i < len(fields); i++ {
		f := fields[i]
		if f == "" {
			continue
		}
		if strings.HasPrefix(f, "# ") {
			switch {
			case strings.HasPrefix(f, "# branch.oid "):
				oid = f[len("# branch.oid "):]
			case strings.HasPrefix(f, "# branch.head "):
				branch = f[len("# branch.head "):]
			case strings.HasPrefix(f, "# branch.upstream "):
				upstream = f[len("# branch.upstream "):]
			case strings.HasPrefix(f, "# branch.ab "):
				m := strings.Fields(f[len("# branch.ab "):]) // +N -M
				if len(m) >= 2 {
					ahead, _ = strconv.Atoi(strings.TrimPrefix(m[0], "+"))
					behind, _ = strconv.Atoi(strings.TrimPrefix(m[1], "-"))
				}
			}
			continue
		}
		switch f[0] {
		case '?':
			entries = append(entries, gitFileEntry{Index: ".", Worktree: ".", Mark: "??", Path: f[2:]})
		case '!':
			// 忽略文件不是改动，不进列表
		case 'u':
			// u <XY> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>
			parts := strings.Split(f, " ")
			xy := "UU"
			if len(parts) > 1 {
				xy = parts[1]
			}
			path := strings.Join(parts[10:], " ")
			entries = append(entries, gitFileEntry{Index: gitByte(xy, 0), Worktree: gitByte(xy, 1), Path: path})
		case '1', '2':
			// 1 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <path>
			// 2 <XY> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path><NUL><origPath>
			parts := strings.Split(f, " ")
			xy := ".."
			if len(parts) > 1 {
				xy = parts[1]
			}
			pathStart := 8
			orig := ""
			if f[0] == '2' {
				pathStart = 9
				if i+1 < len(fields) {
					orig = fields[i+1]
				}
				i++
			}
			path := strings.Join(parts[pathStart:], " ")
			entries = append(entries, gitFileEntry{Index: gitByte(xy, 0), Worktree: gitByte(xy, 1), Path: path, Original: orig})
		}
	}
	return branch, oid, upstream, ahead, behind, entries
}

// gitByte 取 xy 的第 i 个字符（越界给 '.'）。
func gitByte(xy string, i int) string {
	if i < len(xy) && xy[i] != 0 {
		return string(xy[i])
	}
	return "."
}

// gitGroupStatus 把改动文件分到「已暂存/未暂存/未跟踪/冲突」四组；
// 一个文件可同时在前两组（MM），与 git 语义一致。
func gitGroupStatus(entries []gitFileEntry) (staged, unstaged, untracked, conflicted []gitFileEntry) {
	staged, unstaged, untracked, conflicted = []gitFileEntry{}, []gitFileEntry{}, []gitFileEntry{}, []gitFileEntry{}
	for _, e := range entries {
		switch {
		case e.Mark == "??":
			untracked = append(untracked, e)
		case e.Index == "U" || e.Worktree == "U":
			conflicted = append(conflicted, e)
		default:
			if e.Index != "." && e.Index != " " {
				staged = append(staged, e)
			}
			if e.Worktree != "." && e.Worktree != " " {
				unstaged = append(unstaged, e)
			}
		}
	}
	return
}

// gitParseDecorations 解析 `git log` 的 %d 装饰段：" (HEAD -> go, origin/go, tag: v1)"。
func gitParseDecorations(d string) []gitRef {
	d = strings.TrimSpace(d)
	if d == "" || d == "()" {
		return nil
	}
	d = strings.TrimPrefix(d, "(")
	d = strings.TrimSuffix(d, ")")
	refs := []gitRef{}
	for _, part := range strings.Split(d, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if strings.HasPrefix(part, "HEAD -> ") {
			refs = append(refs, gitRef{Name: strings.TrimPrefix(part, "HEAD -> "), Kind: "head", Current: true})
			continue
		}
		if part == "HEAD" {
			continue // 游离 HEAD 没有名字，单独的分支 chip 由 status 提供不了就不画
		}
		if strings.HasPrefix(part, "tag: ") {
			refs = append(refs, gitRef{Name: strings.TrimPrefix(part, "tag: "), Kind: "tag"})
			continue
		}
		if strings.Contains(part, "/") && (strings.HasPrefix(part, "origin/") || strings.Contains(part, "/")) {
			refs = append(refs, gitRef{Name: part, Kind: "remote"})
			continue
		}
		refs = append(refs, gitRef{Name: part, Kind: "head"})
	}
	return refs
}

// gitParseLogGraph 解析图谱格式的 `git log` 输出：
// %H<US>%P<US>%an<US>%ad<US>%d<US>%s<RS>（--all --topo-order --date=iso-strict）。
func gitParseLogGraph(raw string) []gitCommitInfo {
	commits := []gitCommitInfo{}
	for _, rec := range strings.Split(raw, gitRecSep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		parts := strings.Split(rec, gitFieldSep)
		if len(parts) < 6 || parts[0] == "" {
			continue
		}
		parents := []string{}
		if parts[1] != "" {
			parents = strings.Fields(parts[1])
		}
		commits = append(commits, gitCommitInfo{
			Hash:    parts[0],
			Parents: parents,
			Author:  parts[2],
			Date:    parts[3],
			Refs:    gitParseDecorations(parts[4]),
			Subject: parts[5],
		})
	}
	return commits
}

// gitParseForEachRef 解析 for-each-ref 的记录（字段间 US、记录间 RS）。
// 字段序：完整 refname、短名、短哈希、上游、track、HEAD 标记、主题。
// track 形如 "[ahead 1, behind 2]" 或 "[gone]"。
func gitParseForEachRef(raw string) []gitBranchInfo {
	branches := []gitBranchInfo{}
	for _, rec := range strings.Split(raw, gitRecSep) {
		rec = strings.TrimLeft(rec, "\n")
		if rec == "" {
			continue
		}
		p := strings.Split(rec, gitFieldSep)
		if len(p) < 5 || p[1] == "" {
			continue
		}
		b := gitBranchInfo{Full: p[0], Name: p[1], Short: p[2], Upstream: p[3]}
		if len(p) >= 6 && p[5] == "*" {
			b.Current = true
		}
		if len(p) >= 7 {
			b.Subject = p[6]
		}
		track := p[4]
		if strings.Contains(track, "gone") {
			b.Gone = true
		}
		if m := regexp.MustCompile(`ahead (\d+)`).FindStringSubmatch(track); m != nil {
			b.Ahead, _ = strconv.Atoi(m[1])
		}
		if m := regexp.MustCompile(`behind (\d+)`).FindStringSubmatch(track); m != nil {
			b.Behind, _ = strconv.Atoi(m[1])
		}
		branches = append(branches, b)
	}
	return branches
}

// gitParseNameStatus 解析 `git show --name-status -z --format=`：
// 记录形如 M\0path\0，重命名为 R100\0old\0new\0。
func gitParseNameStatus(raw string) []gitFileEntry {
	fields := strings.Split(raw, "\x00")
	files := []gitFileEntry{}
	for i := 0; i < len(fields); i++ {
		st := fields[i]
		if len(st) == 0 || st[0] < 'A' || st[0] > 'Z' {
			continue
		}
		letter := string(st[0])
		if letter == "R" || letter == "C" {
			if i+2 < len(fields) {
				files = append(files, gitFileEntry{Index: letter, Worktree: ".", Path: fields[i+2], Original: fields[i+1]})
			}
			i += 2
			continue
		}
		if i+1 < len(fields) {
			files = append(files, gitFileEntry{Index: letter, Worktree: ".", Path: fields[i+1]})
		}
		i++
	}
	return files
}

// gitDiffIsBinary 判断 diff 文本是否表示二进制差异。
func gitDiffIsBinary(text string) bool {
	return strings.Contains(text, "GIT binary patch") ||
		(regexp.MustCompile(`(?m)^Binary files .* differ$`).MatchString(text))
}

// gitCapText 按 UTF-8 字节上限截断文本，保留行边界；截断点回退到完整的
// UTF-8 字符（孤儿前导字节一并去掉），不把多字节字符切成半个。
func gitCapText(text string, maxBytes int) (string, bool) {
	if len(text) <= maxBytes {
		return text, false
	}
	sliced := text[:maxBytes]
	for i := 0; i < 3; i++ {
		r, size := utf8.DecodeLastRuneInString(sliced)
		if r != utf8.RuneError || size > 1 {
			break // 末尾是完整字符：停
		}
		sliced = sliced[:len(sliced)-size] // 无效字节（半个多字节字符）：去掉再试
	}
	if lastBreak := strings.LastIndex(sliced, "\n"); lastBreak > 0 {
		sliced = sliced[:lastBreak+1]
	}
	return sliced, true
}

// gitAddedTextDiff 把整份文件内容合成「整份新增」伪 diff（未跟踪文件没有
// index 版本可比，git diff 为空，用内容合成展示）。
func gitAddedTextDiff(text, displayPath string, maxLines int) (string, bool) {
	lines := strings.Split(text, "\n")
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1] // 末尾换行切出的空串不算内容行
	}
	truncated := len(lines) > maxLines
	if truncated {
		lines = lines[:maxLines]
	}
	var b strings.Builder
	b.WriteString("--- /dev/null\n+++ b/" + displayPath + "\n")
	for _, l := range lines {
		b.WriteByte('+')
		b.WriteString(l)
		b.WriteByte('\n')
	}
	return b.String(), truncated
}

// gitSafePathspecs 过滤出一组可安全传给 git 的 pathspec（放在 -- 之后作为独立
// argv 元素）：拒绝绝对路径、.. 与 . 段、pathspec magic 前缀、目录形式，去重限数。
func gitSafePathspecs(paths []string) []string {
	seen := map[string]bool{}
	ok := []string{}
	for _, p := range paths {
		if len(ok) >= gitPathspecMax {
			break
		}
		if p == "" || strings.Contains(p, "\x00") || len(p) > 4096 {
			continue
		}
		if regexp.MustCompile(`^[a-zA-Z]:[\\/]`).MatchString(p) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "\\") {
			continue
		}
		if strings.HasPrefix(p, ":") || strings.HasPrefix(p, "!") || strings.HasPrefix(p, "^") {
			continue
		}
		segs := strings.FieldsFunc(p, func(r rune) bool { return r == '/' || r == '\\' })
		bad := false
		for _, s := range segs {
			if s == ".." || s == "." {
				bad = true
			}
		}
		if bad || p[len(p)-1] == '/' || p[len(p)-1] == '\\' {
			continue
		}
		if seen[p] {
			continue
		}
		seen[p] = true
		ok = append(ok, p)
	}
	return ok
}

// gitSanitizeMessage 净化提交信息：去控制字符（换行保留——多行信息合法）、
// 压缩多余空行、限长。
func gitSanitizeMessage(message string) string {
	var b strings.Builder
	for _, r := range message {
		if r < 0x20 && r != '\n' && r != '\t' || r == 0x7f {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.ReplaceAll(b.String(), "\r\n", "\n")
	cleaned = strings.ReplaceAll(cleaned, "\r", "\n")
	for strings.Contains(cleaned, "\n\n\n") {
		cleaned = strings.ReplaceAll(cleaned, "\n\n\n", "\n\n")
	}
	cleaned = strings.TrimSpace(cleaned)
	if len(cleaned) > gitMessageMaxChars {
		cleaned = cleaned[:gitMessageMaxChars]
	}
	return cleaned
}

// ---------- 业务读取 ----------

// gitStatus 组装当前仓库状态（分支、分组改动、候选仓库）。
func (a *App) gitStatus(ctx context.Context, session, repo string) (*gitStatusResult, error) {
	root, repos, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return &gitStatusResult{Repos: repos, Staged: []gitFileEntry{}, Unstaged: []gitFileEntry{}, Untracked: []gitFileEntry{}, Conflicted: []gitFileEntry{}, Reason: reason}, nil
	}
	raw, trunc, err := a.gitRun(ctx, root, gitReadTimeout, "status", "--porcelain=v2", "--branch", "-z")
	if err != nil {
		return nil, gitErr("读取仓库状态失败", err)
	}
	branch, oid, upstream, ahead, behind, entries := gitParseStatusV2(raw)
	staged, unstaged, untracked, conflicted := gitGroupStatus(entries)
	if branch == "(detached)" {
		branch = ""
	}
	cut := func(list []gitFileEntry) []gitFileEntry {
		if len(list) > gitStatusMax {
			return list[:gitStatusMax]
		}
		return list
	}
	return &gitStatusResult{
		Repos: repos, Repo: root, Branch: branch, OID: oid, Upstream: upstream,
		Ahead: ahead, Behind: behind,
		Staged: cut(staged), Unstaged: cut(unstaged), Untracked: cut(untracked), Conflicted: cut(conflicted),
		Truncated: trunc || len(entries) > gitStatusMax,
	}, nil
}

// gitGraph 组装图谱提交页（--all --topo-order：全部分支、拓扑序保证泳道可画）。
func (a *App) gitGraph(ctx context.Context, session, repo string, limit, skip int) (commits []gitCommitInfo, more bool, err error) {
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return []gitCommitInfo{}, false, errors.New(reason)
	}
	if limit <= 0 || limit > gitLogMax {
		limit = gitLogDefault
	}
	if skip < 0 {
		skip = 0
	}
	format := "--pretty=format:%H" + gitFieldSep + "%P" + gitFieldSep + "%an" + gitFieldSep + "%ad" + gitFieldSep + "%d" + gitFieldSep + "%s" + gitRecSep
	raw, _, err := a.gitRun(ctx, root, gitReadTimeout, "log", "--all", "--topo-order", "--date=iso-strict",
		"-n", strconv.Itoa(limit+1), "--skip", strconv.Itoa(skip), format)
	if err != nil {
		if strings.Contains(err.Error(), "does not have any commits yet") || strings.Contains(err.Error(), "unknown revision") || strings.Contains(err.Error(), "bad revision") {
			return []gitCommitInfo{}, false, nil // 空仓库不是错误
		}
		return nil, false, gitErr("读取提交图谱失败", err)
	}
	parsed := gitParseLogGraph(raw)
	more = len(parsed) > limit
	if more {
		parsed = parsed[:limit]
	}
	return parsed, more, nil
}

// gitCommitDetail 单次提交的完整信息：消息 + 文件清单 + diff。
func (a *App) gitCommitDetail(ctx context.Context, session, repo, hash string) (subject, message string, files []gitFileEntry, diff gitDiffResult, err error) {
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return "", "", nil, diff, errors.New(reason)
	}
	if !gitHashRe.MatchString(hash) {
		return "", "", nil, diff, errors.New("提交标识不合法：" + hash)
	}
	// 合并提交 git show 默认不出 diff 与文件清单；按通用惯例（GitHub/VSCode）
	// 改为对第一父提交比较（--first-parent -m）。
	mergeExtra := []string{}
	if probe, _, err := a.gitRun(ctx, root, gitReadTimeout, "rev-list", "--parents", "-n", "1", hash); err == nil {
		if len(strings.Fields(probe)) > 2 {
			mergeExtra = []string{"--first-parent", "-m"}
		}
	}
	files = []gitFileEntry{}
	msgRaw, _, err := a.gitRun(ctx, root, gitReadTimeout, "show", "-s", "--pretty=format:%s%n%n%b", hash)
	if err != nil {
		return "", "", nil, diff, gitErr("读取提交说明失败", err)
	}
	subject = strings.SplitN(strings.TrimSpace(msgRaw), "\n", 2)[0]
	message = strings.TrimSpace(msgRaw)
	namesArgs := append([]string{"show", "--name-status", "-z", "--format="}, mergeExtra...)
	namesArgs = append(namesArgs, hash)
	namesRaw, _, err := a.gitRun(ctx, root, gitReadTimeout, namesArgs...)
	if err != nil {
		return "", "", nil, diff, gitErr("读取提交文件清单失败", err)
	}
	files = gitParseNameStatus(namesRaw)
	patchArgs := append([]string{"show", "--patch", "--no-color", "--no-ext-diff", "--unified=3", "--format="}, mergeExtra...)
	patchArgs = append(patchArgs, hash)
	patchRaw, patchTrunc, err := a.gitRun(ctx, root, gitReadTimeout, patchArgs...)
	if err != nil {
		return subject, message, files, diff, gitErr("读取提交 diff 失败", err)
	}
	diff = gitCapDiff(patchRaw, patchTrunc)
	return subject, message, files, diff, nil
}

// gitCapDiff 统一做展示上限截断与二进制判定。
func gitCapDiff(text string, truncated bool) gitDiffResult {
	capped, capTrunc := gitCapText(text, gitDiffMaxBytes)
	return gitDiffResult{Text: capped, Truncated: truncated || capTrunc, Binary: gitDiffIsBinary(text), Empty: text == ""}
}

// gitFileDiff 单个文件的 diff（staged=true 读索引对 HEAD，false 读工作区对索引）；
// 未跟踪文件无 diff 时用文件内容合成「整份新增」。
func (a *App) gitFileDiff(ctx context.Context, session, repo, path string, staged bool) (gitDiffResult, error) {
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return gitDiffResult{}, errors.New(reason)
	}
	spec := gitSafePathspecs([]string{path})
	if len(spec) == 0 {
		return gitDiffResult{}, errors.New("文件路径不合法：" + path)
	}
	args := []string{"diff", "--no-color", "--no-ext-diff", "--find-renames", "--unified=3"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, "--", spec[0])
	raw, trunc, err := a.gitRun(ctx, root, gitReadTimeout, args...)
	if err != nil {
		return gitDiffResult{}, gitErr("读取文件 diff 失败", err)
	}
	if raw == "" && !staged {
		if added, trunc2, ok := a.gitAddedFileDiff(filepath.Join(root, filepath.FromSlash(path)), path); ok {
			raw, trunc = added, trunc2
		}
	}
	return gitCapDiff(raw, trunc), nil
}

// gitAddedFileDiff 读未跟踪文件内容合成伪 diff；读不到或太大时不合成（ok=false）。
func (a *App) gitAddedFileDiff(absPath, displayPath string) (text string, truncated bool, ok bool) {
	fi, err := os.Stat(absPath)
	if err != nil || !fi.Mode().IsRegular() {
		return "", false, false
	}
	if fi.Size() > gitDiffMaxBytes*8 {
		return "", true, true // 太大：给一句说明，不读内容
	}
	raw, err := os.ReadFile(absPath)
	if err != nil {
		return "", false, false
	}
	if strings.ContainsRune(string(raw), 0) {
		return "", false, true // 含 NUL：按二进制处理，不给伪 diff
	}
	text, truncated = gitAddedTextDiff(string(raw), displayPath, gitAddedMaxLines)
	return text, truncated, true
}

// gitBranches 本地与远程分支清单（含当前标记、上游与领先/落后）。
func (a *App) gitBranches(ctx context.Context, session, repo string) (locals, remotes []gitBranchInfo, err error) {
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return nil, nil, errors.New(reason)
	}
	format := "--format=%(refname)" + gitFieldSep + "%(refname:short)" + gitFieldSep + "%(objectname:short)" + gitFieldSep +
		"%(upstream:short)" + gitFieldSep + "%(upstream:track)" + gitFieldSep + "%(HEAD)" + gitFieldSep + "%(contents:subject)" + gitRecSep
	locals = []gitBranchInfo{}
	remotes = []gitBranchInfo{}
	raw, _, err := a.gitRun(ctx, root, gitReadTimeout, "for-each-ref", "refs/heads", format)
	if err != nil {
		return nil, nil, gitErr("读取分支失败", err)
	}
	locals = gitParseForEachRef(raw)
	raw, _, err = a.gitRun(ctx, root, gitReadTimeout, "for-each-ref", "refs/remotes", format)
	if err == nil {
		remotes = gitParseForEachRef(raw)
	}
	// refs/remotes 下每个远程还有一个指向默认分支的符号 HEAD（origin/HEAD），
	// 它的 shortname 不带 /HEAD 后缀，按完整 refname 过滤（origin 这类没有
	// 命名空间的记录不是真实分支）。
	filtered := remotes[:0]
	for _, b := range remotes {
		if n := strings.TrimPrefix(b.Name, "refs/remotes/"); !strings.Contains(n, "/") {
			continue
		}
		filtered = append(filtered, b)
	}
	return locals, filtered, nil
}

// ---------- 安全写操作 ----------

// gitStage 暂存一批文件（git add；未跟踪文件同样适用）。只动 index。
func (a *App) gitStage(ctx context.Context, session, repo string, paths []string) (int, error) {
	spec := gitSafePathspecs(paths)
	if len(spec) == 0 {
		return 0, errors.New("没有可暂存的路径（列表为空或全部未通过校验）")
	}
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return 0, errors.New(reason)
	}
	args := append([]string{"add", "--"}, spec...)
	if _, _, err := a.gitRun(ctx, root, gitReadTimeout, args...); err != nil {
		return 0, gitErr("暂存失败", err)
	}
	return len(spec), nil
}

// gitUnstage 取消暂存一批文件（git restore --staged；空仓库无 HEAD 时回退 rm --cached，
// 同样只动 index，工作区内容不受影响）。
func (a *App) gitUnstage(ctx context.Context, session, repo string, paths []string) (int, error) {
	spec := gitSafePathspecs(paths)
	if len(spec) == 0 {
		return 0, errors.New("没有可取消暂存的路径（列表为空或全部未通过校验）")
	}
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return 0, errors.New(reason)
	}
	args := append([]string{"restore", "--staged", "--"}, spec...)
	if _, _, err := a.gitRun(ctx, root, gitReadTimeout, args...); err != nil {
		fallback := append([]string{"rm", "--cached", "--"}, spec...)
		if _, _, err2 := a.gitRun(ctx, root, gitReadTimeout, fallback...); err2 != nil {
			return 0, gitErr("取消暂存失败", err)
		}
	}
	return len(spec), nil
}

// gitUntrack 把一批已跟踪文件移出跟踪（git rm --cached -q：只删 index 里的
// 记录，工作区文件原样保留），之后它们出现在「未跟踪」组。不写 .gitignore——
// 要不要忽略由用户自己决定（可在对话里让数字助理加）。
func (a *App) gitUntrack(ctx context.Context, session, repo string, paths []string) (int, error) {
	spec := gitSafePathspecs(paths)
	if len(spec) == 0 {
		return 0, errors.New("没有可移出跟踪的路径（列表为空或全部未通过校验）")
	}
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return 0, errors.New(reason)
	}
	args := append([]string{"rm", "--cached", "-q", "--"}, spec...)
	if _, _, err := a.gitRun(ctx, root, gitReadTimeout, args...); err != nil {
		msg := err.Error()
		if strings.Contains(msg, "did not match any files") {
			return 0, errors.New("这些文件不在跟踪里（可能已被移出），请刷新后重试")
		}
		return 0, gitErr("移出跟踪失败", err)
	}
	return len(spec), nil
}

// gitCommit 用给定信息提交已暂存的改动（git commit -m）。
func (a *App) gitCommit(ctx context.Context, session, repo, message string) (string, error) {
	text := gitSanitizeMessage(message)
	if text == "" {
		return "", errors.New("提交信息为空：请先输入内容，或用「AI 生成摘要」")
	}
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return "", errors.New(reason)
	}
	raw, _, err := a.gitRun(ctx, root, gitCommitTimeout, "commit", "-m", text)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "nothing to commit") || strings.Contains(msg, "no changes added") {
			return "", errors.New("没有已暂存的改动：请先暂存要提交的文件")
		}
		if strings.Contains(msg, "Please tell me who you are") {
			return "", errors.New("git 不知道你是谁：请先配置提交身份（git config --global user.name / user.email）")
		}
		return "", gitErr("提交失败", err)
	}
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) != "" {
			return strings.TrimSpace(line), nil // 第一行形如 [branch hash] subject
		}
	}
	return "", nil
}

// gitCheckout 切换本地分支（git checkout <branch>）。分支名必须精确匹配
// for-each-ref 列出的本地分支（不接受任意引用与路径）；未提交改动会被切换
// 覆盖时 git 自己拒绝，这里把原因转成人话。
func (a *App) gitCheckout(ctx context.Context, session, repo, branch string) (string, error) {
	branch = strings.TrimSpace(branch)
	if branch == "" {
		return "", errors.New("分支名为空")
	}
	if strings.HasPrefix(branch, "-") {
		return "", errors.New("分支名不合法：" + branch)
	}
	locals, _, err := a.gitBranches(ctx, session, repo)
	if err != nil {
		return "", err
	}
	known := false
	for _, b := range locals {
		if b.Name == branch {
			known = true
			break
		}
	}
	if !known {
		return "", errors.New("没有这个本地分支：" + branch)
	}
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return "", errors.New(reason)
	}
	raw, _, err := a.gitRun(ctx, root, gitCommitTimeout, "checkout", branch)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "would be overwritten") || strings.Contains(msg, "Please commit your changes") {
			return "", errors.New("有未提交改动会被切换覆盖：请先暂存或提交，再切换分支")
		}
		return "", gitErr("切换分支失败", err)
	}
	summary := strings.TrimSpace(raw)
	if summary == "" {
		summary = "已切换到 " + branch
	}
	return strings.SplitN(summary, "\n", 2)[0], nil
}

// ---------- 远程同步（「⟲ 同步」按钮） ----------

// gitAheadBehind 读 HEAD 相对 upstream 的领先/落后提交数（left=ahead 右=behind）。
func (a *App) gitAheadBehind(ctx context.Context, root, upstream string) (ahead, behind int) {
	raw, _, err := a.gitRun(ctx, root, gitReadTimeout, "rev-list", "--left-right", "--count", "HEAD..."+upstream)
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(raw)
	if len(fields) == 2 {
		ahead, _ = strconv.Atoi(fields[0])
		behind, _ = strconv.Atoi(fields[1])
	}
	return ahead, behind
}

// gitSync 与远程同步。op：""=整套（拉取能合并的、再推送，「⟲ 同步」按钮）、
// "pull"=只拉取（提交栏「↓ 拉取」按钮）、"push"=只推送（「↑ 推送」按钮）。
// 流程：fetch → 落后则合并（先快进；分叉时自动合并，一旦冲突立即 --abort 恢复
// 现场）→ 本地领先且远程没有新提交时推送当前分支。面板只做"机器能自动完成"的
// 部分；凡是需要人（或数字助理）决策/外部条件的情形——无上游、历史分叉有
// 冲突、未提交改动挡住合并、推送被拒、凭据缺失——一律不动手，返回
// needHelp=true，由前端引导用户去对话里派工作任务处理。
func (a *App) gitSync(ctx context.Context, session, repo, op string) (summary string, needHelp bool, err error) {
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return "", false, errors.New(reason)
	}
	raw, _, err := a.gitRun(ctx, root, gitReadTimeout, "rev-parse", "--abbrev-ref", "--symbolic-full-name", "@{upstream}")
	upstream := strings.TrimSpace(raw)
	if err != nil || upstream == "" {
		return "当前分支没有配置上游（远程跟踪分支）：需要先配置远程仓库并设置上游（git remote add / git push -u），面板不代做这一步", true, nil
	}
	remote := strings.SplitN(upstream, "/", 2)[0]

	// 1) fetch：拉取/推送都先取一次远程（推送据此判断远程是否又有新提交）；
	//    网络/凭据问题交给对话（AI 可以先诊断 remote 配置与凭据）
	//    网络/凭据问题交给对话（AI 可以先诊断 remote 配置与凭据）。
	//    仅探状态（op=fetch，前端后台调）用短超时，失败也不弹建议卡
	fetchTimeout := gitSyncTimeout
	if op == "fetch" {
		fetchTimeout = gitFetchTimeout
	}
	if _, _, err := a.gitRun(ctx, root, fetchTimeout, "fetch", "--no-tags", remote); err != nil {
		if op == "fetch" {
			return "获取远程状态失败：" + gitTail(err.Error(), 200), false, nil
		}
		return "拉取远程更新失败：" + gitTail(err.Error(), 200), true, nil
	}
	if op == "fetch" {
		return "已更新远程状态", false, nil
	}
	ahead, behind := a.gitAheadBehind(ctx, root, upstream)

	// 2) 落后则合并：快进优先（不产生提交）；分叉再试自动合并（普通 merge
	//    提交）；有任何冲突立即中止恢复，工作区保持原样
	pulled := ""
	if (op == "" || op == "pull") && behind > 0 {
		if _, _, err := a.gitRun(ctx, root, gitSyncTimeout, "merge", "--ff-only", upstream); err != nil {
			if _, _, err2 := a.gitRun(ctx, root, gitSyncTimeout, "merge", "--no-edit", upstream); err2 != nil {
				// 冲突时恢复现场；若 merge 因未提交改动被 git 直接拒绝，现场本就未动
				_, _, _ = a.gitRun(ctx, root, gitReadTimeout, "merge", "--abort")
				return "远程更新无法自动合并（历史分叉有冲突，或未提交的改动挡住了合并），已恢复原状、未做任何改动", true, nil
			}
			pulled = "已自动合并远程分支"
		} else {
			pulled = fmt.Sprintf("已快进拉取 %d 个提交", behind)
		}
	}
	if op == "pull" {
		if pulled == "" {
			if ahead == 0 {
				return "已是最新：与远程一致", false, nil
			}
			return "远程没有比本地新的提交，无需拉取", false, nil
		}
		return pulled, false, nil
	}

	// 3) 领先则推送当前分支（显式 refspec，不受 push.default 影响）。
	//    只推送时远程又有新提交则不动手，引导先走「拉取」
	if op == "push" && behind > 0 {
		return fmt.Sprintf("远程领先 %d 个提交：请先「拉取」合并，再推送", behind), false, nil
	}
	pushed := ""
	if ahead, _ := a.gitAheadBehind(ctx, root, upstream); ahead > 0 {
		if _, _, err := a.gitRun(ctx, root, gitSyncTimeout, "push", remote, "HEAD"); err != nil {
			if pulled != "" {
				return "本地已拉取到最新，但推送失败：" + gitTail(err.Error(), 200), true, nil
			}
			return "推送失败：" + gitTail(err.Error(), 200), true, nil
		}
		pushed = fmt.Sprintf("已推送 %d 个提交", ahead)
	}
	if op == "push" && pushed == "" {
		return "没有领先远程的提交可推送", false, nil
	}

	if op == "push" && pushed != "" {
		return pushed, false, nil
	}

	if pulled == "" && pushed == "" {
		return "已是最新：与远程一致", false, nil
	}
	parts := []string{}
	if pulled != "" {
		parts = append(parts, pulled)
	}
	if pushed != "" {
		parts = append(parts, pushed)
	}
	return "已同步：" + strings.Join(parts, "，"), false, nil
}

// ---------- AI 提交摘要 ----------

// gitSummarySystemPrompt 是 AI 提交摘要的系统提示词（对齐数字助理2 dsh-git-panel 的写法）。
const gitSummarySystemPrompt = `你是提交信息撰写员。根据给定的 git 已暂存改动（stat 概览 + diff 正文），写一条提交信息。
要求：
- 第一行是简洁的主题行（不超过 50 个字，中文，概括最重要的改动；必要时可用 conventional commit 前缀如 feat:/fix:/docs:/refactor:）。
- 如果改动较复杂，主题行后空一行，再写 2-4 行正文说明要点；简单改动就不用正文。
- 只输出提交信息本身：不要代码块、不要引号、不要解释、不要任何多余前言或落款。`

// gitSummarize 用当前会话配置的模型为已暂存改动生成提交信息（不提交，只生成）。
func (a *App) gitSummarize(ctx context.Context, session, repo string) (string, error) {
	root, _, reason := a.gitResolve(ctx, session, repo)
	if root == "" {
		return "", errors.New(reason)
	}
	stat, _, err := a.gitRun(ctx, root, gitReadTimeout, "diff", "--cached", "--no-color", "--stat")
	if err != nil {
		return "", gitErr("读取已暂存改动概览失败", err)
	}
	if strings.TrimSpace(stat) == "" {
		return "", errors.New("没有已暂存的改动：请先暂存再生成摘要")
	}
	patch, _, err := a.gitRun(ctx, root, gitReadTimeout, "diff", "--cached", "--no-color", "--no-ext-diff", "--unified=1")
	if err != nil {
		return "", gitErr("读取已暂存 diff 失败", err)
	}
	diffText := stat + "\n" + patch
	if len(diffText) > gitSummaryMaxChars {
		diffText = diffText[:gitSummaryMaxChars]
	}
	// 指定会话的解析失败必须原样阻断，不能把暂存源码改发到全局渠道。
	var target LLMTarget
	var terr error
	if session == "" {
		target, terr = a.DefaultTarget()
	} else {
		m, err := a.Store.Get(session)
		if err != nil {
			return "", fmt.Errorf("读取摘要会话失败：%w", err)
		}
		if m == nil {
			return "", errors.New("摘要会话不存在，请重新选择会话")
		}
		target, terr = a.resolveTarget(m)
	}
	if terr != nil {
		return "", terr
	}
	sctx, cancel := context.WithTimeout(ctx, gitSummaryTimeout)
	defer cancel()
	var sb strings.Builder
	// 用量明细（组件 usage-stats）：git 提交摘要记成「其他辅助生成」，与主对话分开
	meterCb, finishUsage := a.meterUsage(usagePurposeAssist, nil, target, "", StreamCallbacks{
		OnText: func(s string) { sb.WriteString(s) },
	})
	err = StreamChat(sctx, target, ChatRequest{
		Model: target.Model,
		Messages: []Message{
			TextMessage("system", gitSummarySystemPrompt),
			TextMessage("user", diffText),
		},
		Stream: true,
	}, meterCb)
	finishUsage(err)
	if err != nil {
		return "", errors.New("生成摘要失败：" + gitTail(err.Error(), 400))
	}
	cleaned := gitSanitizeMessage(sb.String())
	if cleaned == "" {
		return "", errors.New("模型没有返回可用的摘要：请重试或手动输入提交信息")
	}
	return cleaned, nil
}

// ---------- HTTP 处理器 ----------

// gitGuard 组件未启用时本功能的接口一律 404（与组件资产同一语义）。
func (a *App) gitGuard(w http.ResponseWriter) bool {
	if !a.Components.IsEnabled(gitComponentID) {
		writeErr(w, 404, "「Git 面板」组件未启用，请到 设置 → 内置组件 打开")
		return false
	}
	return true
}

type gitRequest struct {
	Session string   `json:"session"`
	Repo    string   `json:"repo"`
	Paths   []string `json:"paths"`
	Message string   `json:"message"`
	Branch  string   `json:"branch"`
	Op      string   `json:"op"` // sync 的细分操作：""=整套 / "pull"=只拉取 / "push"=只推送
}

// decodeGitRequest 解析 POST 请求体；查询参数兜底（GET/POST 统一取 session/repo）。
func decodeGitRequest(r *http.Request) gitRequest {
	var body gitRequest
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Session == "" {
		body.Session = r.URL.Query().Get("session")
	}
	if body.Repo == "" {
		body.Repo = r.URL.Query().Get("repo")
	}
	return body
}

func (a *App) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	q := r.URL.Query()
	res, err := a.gitStatus(r.Context(), q.Get("session"), q.Get("repo"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

func (a *App) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	q := r.URL.Query()
	res, err := a.gitFileDiff(r.Context(), q.Get("session"), q.Get("repo"), q.Get("path"), q.Get("staged") == "1")
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, res)
}

func (a *App) handleGitGraph(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	q := r.URL.Query()
	limit, _ := strconv.Atoi(q.Get("limit"))
	skip, _ := strconv.Atoi(q.Get("skip"))
	commits, more, err := a.gitGraph(r.Context(), q.Get("session"), q.Get("repo"), limit, skip)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"commits": commits, "more": more})
}

func (a *App) handleGitCommitDetail(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	q := r.URL.Query()
	subject, message, files, diff, err := a.gitCommitDetail(r.Context(), q.Get("session"), q.Get("repo"), q.Get("hash"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"subject": subject, "message": message, "files": files, "diff": diff})
}

func (a *App) handleGitBranches(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	q := r.URL.Query()
	locals, remotes, err := a.gitBranches(r.Context(), q.Get("session"), q.Get("repo"))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"locals": locals, "remotes": remotes})
}

func (a *App) handleGitStage(w http.ResponseWriter, r *http.Request) {
	a.gitWrite(w, r, a.gitStage)
}

func (a *App) handleGitUnstage(w http.ResponseWriter, r *http.Request) {
	a.gitWrite(w, r, a.gitUnstage)
}

// gitWrite 暂存/取消暂存的公共壳：解析请求 → 调操作 → 返回条数。
func (a *App) gitWrite(w http.ResponseWriter, r *http.Request, op func(context.Context, string, string, []string) (int, error)) {
	if !a.gitGuard(w) {
		return
	}
	body := decodeGitRequest(r)
	n, err := op(r.Context(), body.Session, body.Repo, body.Paths)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "count": n})
}

func (a *App) handleGitCommitMsg(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	body := decodeGitRequest(r)
	summary, err := a.gitCommit(r.Context(), body.Session, body.Repo, body.Message)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "summary": summary})
}

func (a *App) handleGitSummarize(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	body := decodeGitRequest(r)
	message, err := a.gitSummarize(r.Context(), body.Session, body.Repo)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "message": message})
}

func (a *App) handleGitCheckout(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	body := decodeGitRequest(r)
	summary, err := a.gitCheckout(r.Context(), body.Session, body.Repo, body.Branch)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "summary": summary})
}

func (a *App) handleGitUntrack(w http.ResponseWriter, r *http.Request) {
	a.gitWrite(w, r, a.gitUntrack)
}

func (a *App) handleGitSync(w http.ResponseWriter, r *http.Request) {
	if !a.gitGuard(w) {
		return
	}
	body := decodeGitRequest(r)
	if op := body.Op; op != "" && op != "pull" && op != "push" && op != "fetch" {
		writeErr(w, 400, "未知的同步操作："+op)
		return
	}
	summary, needHelp, err := a.gitSync(r.Context(), body.Session, body.Repo, body.Op)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "summary": summary, "needHelp": needHelp})
}
