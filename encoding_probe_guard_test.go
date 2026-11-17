package main

// run_command 编码探测的边界测试。
//
// 背景（2026-11-17 实锤）：探测跑在 run_command 启动命令**之前**，候选路径是从
// 命令正文（含提交说明、注释）里捞出来的。旧实现只按 IsDir() 过滤候选、再直接
// os.Open+Read 探测——Windows 保留设备名带任何扩展名仍指向设备，一个落在正文里
// 的设备名就能让探测去读真实设备，Read 永不返回，命令一个子进程都没起，而
// runChild 的超时那时还没开始计时。
//
// 现在的结构：父侧只做纯字符串候选处理（AST 用例守着不碰文件系统），存在性/盘
// 类型/普通文件确认/读头部全在可强杀的探测子进程里，按 1.5s 预算杀并 Wait 回收。
//
// 夹具原则：绝不真实打开/读取设备或串口。设备名闸门用纯字符串判定验证；"子进程
// 卡住"用"测试二进制里的 helper 用例 + 长时间 Sleep"模拟，杀的是真进程，回收由
// Wait 的返回证明；普通文件的编码行为用临时目录里的真文件对照。

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// 设备名/设备命名空间/备用数据流/网络路径一律不探测（Windows 语义，纯字符串）。
func TestProbeNameBlockedWindowsDevices(t *testing.T) {
	if !isWindowsRuntime {
		t.Skip("Windows 保留设备名语义")
	}
	dir := t.TempDir()
	blocked := []string{
		filepath.Join(dir, "NUL.md"),
		filepath.Join(dir, "nul.txt"),
		filepath.Join(dir, "COM1.log"),
		filepath.Join(dir, "lpt9.bat"),
		filepath.Join(dir, "CON.md"),
		filepath.Join(dir, "PRN."),
		filepath.Join(dir, "COM¹.txt"),
		`\\.\` + "NUL",
		`\\?\` + dir + `\a.md`,
		`\\server\share\a.md`,
		filepath.Join(dir, "a.txt") + ":stream",
	}
	for _, p := range blocked {
		if why := probeNameBlocked(p); why == "" {
			t.Errorf("%q 应被拒绝探测", p)
		}
	}
	safe := []string{
		filepath.Join(dir, "a.md"),
		filepath.Join(dir, "auxiliary.md"),
		filepath.Join(dir, "COM.txt"),
		filepath.Join(dir, "中文名称.sql"),
	}
	for _, p := range safe {
		if why := probeNameBlocked(p); why != "" {
			t.Errorf("%q 不该被拒（%s）", p, why)
		}
	}
}

// 父侧候选/聚合阶段不得引用文件系统/系统调用包（AST 级不变式，不受注释干扰）。
func TestProbeParentStageIsPure(t *testing.T) {
	const file = "encodingprobe_parent.go"
	fset := token.NewFileSet()
	af, err := parser.ParseFile(fset, file, nil, parser.ParseComments)
	if err != nil {
		t.Fatalf("解析 %s: %v", file, err)
	}
	for _, imp := range af.Imports {
		path := strings.Trim(imp.Path.Value, `"`)
		if path == "os" || path == "syscall" || path == "os/exec" || strings.HasPrefix(path, "golang.org/x/sys") {
			t.Errorf("%s 引用了 %s：父侧只能做纯字符串处理（会卡在不可取消的文件系统调用上）", file, path)
		}
	}
	ast.Inspect(af, func(n ast.Node) bool {
		if se, ok := n.(*ast.SelectorExpr); ok {
			if id, ok := se.X.(*ast.Ident); ok && (id.Name == "os" || id.Name == "syscall") {
				t.Errorf("%s 调用了 %s.%s：父侧不得碰文件系统", file, id.Name, se.Sel.Name)
			}
		}
		return true
	})
}

// 候选提取：设备名候选不能进探测队列；普通文件照旧；存在性不在这里判（子进程判）。
func TestExtractPathCandidatesSkipsDeviceNames(t *testing.T) {
	if !isWindowsRuntime {
		t.Skip("Windows 保留设备名语义")
	}
	dir := t.TempDir()
	normal := filepath.Join(dir, "normal.txt")
	if err := os.WriteFile(normal, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, note := extractPathCandidates(`Get-Content "normal.txt"`, dir)
	if len(got) != 1 || got[0] != normal {
		t.Fatalf("普通文件应照旧被探测，实得 %v", got)
	}
	if note != "" {
		t.Fatalf("正常命令不该带跳过提示，实得 %q", note)
	}
	// 不存在但名字合法的路径照样进候选：存在性判定已挪到子进程（子进程会回 missing）。
	got, _ = extractPathCandidates(`Get-Content "还没生成.txt"`, dir)
	if len(got) != 1 {
		t.Fatalf("名字合法的候选应交由子进程判存在性，实得 %v", got)
	}
	for _, name := range []string{"NUL.md", "nul.txt", "COM1.log"} {
		got, note = extractPathCandidates(`Get-Content "`+name+`"`, dir)
		if len(got) != 0 {
			t.Fatalf("%s 是 Windows 设备名，不该成为探测候选，实得 %v", name, got)
		}
		if !strings.Contains(note, "已跳过") {
			t.Fatalf("%s 被跳过时应给出可诊断的提示，实得 %q", name, note)
		}
	}
}

// 子进程协议与闸门顺序：名称闸门 → 受保护路径 → 盘类型 → 存在性 → 普通文件 → 读头部。
func TestProbeChildProtocol(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "中文目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	utf8Path := filepath.Join(sub, "说明.md")
	if err := os.WriteFile(utf8Path, []byte("# 中文 UTF-8\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gbkBody, err := encodeText("echo 中文批处理\r\n", EncGBK)
	if err != nil {
		t.Fatalf("编 GBK 样本失败: %v", err)
	}
	gbkPath := filepath.Join(sub, "老脚本.bat")
	if err := os.WriteFile(gbkPath, gbkBody, 0o644); err != nil {
		t.Fatal(err)
	}
	utf16Body, err := encodeText("宽字符 UTF-16LE\r\n", EncUTF16LE)
	if err != nil {
		t.Fatalf("编 UTF-16LE 样本失败: %v", err)
	}
	utf16Path := filepath.Join(sub, "宽字符.txt")
	if err := os.WriteFile(utf16Path, utf16Body, 0o644); err != nil {
		t.Fatal(err)
	}

	// 受保护目录：助手自身配置/数据目录里的文件一律不读
	guardRoot := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(guardRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	guarded := filepath.Join(guardRoot, "config.json")
	if err := os.WriteFile(guarded, []byte(`{"apiKey":"secret"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	missingDir := filepath.Join(t.TempDir(), "no-such-dir")
	missingPath := filepath.Join(missingDir, "plain.txt")
	// 设备名候选放在一个根本不存在的父目录下：若闸门没先于文件系统调用生效，
	// 子进程会回 missing 而不是 blocked-name。
	deviceLookalike := filepath.Join(missingDir, "NUL.md")

	payload := encodingProbeProtocolV1 + "\t" + guardRoot + "\n" +
		utf8Path + "\n" + gbkPath + "\n" + utf16Path + "\n" +
		guarded + "\n" + dir + "\n" + missingPath + "\n" + deviceLookalike + "\n"
	var out strings.Builder
	if code := runEncodingProbeChild(strings.NewReader(payload), &out); code != 0 {
		t.Fatalf("子进程返回码 %d", code)
	}
	res := parseEncodingProbeOutput([]byte(out.String()), 7)
	want := []struct {
		enc    TextEncoding
		reason string
	}{
		{EncUTF8, ""},
		{EncGBK, ""},
		{EncUTF16LE, ""},
		{EncUnknown, probeReasonProtected},
		{EncUnknown, probeReasonNonRegular}, // 目录
		{EncUnknown, probeReasonMissing},
		{EncUnknown, probeReasonNameBlocked},
	}
	for i, w := range want {
		got, ok := res[i]
		if !ok {
			t.Fatalf("第 %d 个候选没有回报：%s", i, out.String())
		}
		if got.enc != w.enc || got.reason != w.reason {
			t.Fatalf("第 %d 个候选：期望 %v/%q，实得 %v/%q", i, w.enc, w.reason, got.enc, got.reason)
		}
	}
}

// 普通文件与中文路径的原编码处理必须不变（UTF-8 / GBK / UTF-16LE / 混合提示）。
func TestDetectEncodingModeKeepsNormalFiles(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "中文目录")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, body []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(sub, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("说明.md", []byte("# 销售订单（UTF-8 无 BOM）\r\n第二行\r\n"))
	gbkBody, err := encodeText("echo 中文批处理\r\n", EncGBK)
	if err != nil {
		t.Fatalf("编 GBK 样本失败: %v", err)
	}
	write("老脚本.bat", gbkBody)
	body, err := encodeText("宽字符 UTF-16LE\r\n", EncUTF16LE)
	if err != nil {
		t.Fatalf("编 UTF-16LE 样本失败: %v", err)
	}
	write("宽字符.txt", body)
	cases := []struct {
		cmd  string
		want string
	}{
		{`Get-Content "中文目录\说明.md"`, encodingModeUTF8},
		{`Get-Content "中文目录\老脚本.bat"`, encodingModeGBK},
		{`Get-Content "中文目录\宽字符.txt"`, encodingModeUTF8},
	}
	for _, c := range cases {
		if m, hint := detectEncodingMode(context.Background(), dir, c.cmd, "", nil); m != c.want || hint != "" {
			t.Fatalf("%q: 期望 %q 无提示，实得 %q / %q", c.cmd, c.want, m, hint)
		}
	}
	// 混合：GBK + UTF-8 → 按 UTF-8 并给出提示
	if m, hint := detectEncodingMode(context.Background(), dir, `Get-Content "中文目录\老脚本.bat" "中文目录\说明.md"`, "", nil); m != encodingModeUTF8 || !strings.Contains(hint, "GBK") {
		t.Fatalf("混合编码应按 UTF-8 并提示，实得 %q / %q", m, hint)
	}
	// 不存在的输出文件：既不算候选也不出提示
	if m, hint := detectEncodingMode(context.Background(), dir, `Set-Content "新文件.txt" x`, "", nil); m != encodingModeUTF8 || hint != "" {
		t.Fatalf("不存在的路径不该出提示，实得 %q / %q", m, hint)
	}
}

// 子进程卡住：父侧到点强杀、Wait 确认回收、goroutine 回到基线。
func TestDetectEncodingModeBlockedChildKilled(t *testing.T) {
	useBlockedProbeChild(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got encodingProbeRun
	old := spawnEncodingProbe
	spawnEncodingProbe = func(ctx context.Context, paths []string, roots []string) encodingProbeRun {
		got = spawnEncodingProbeProcess(ctx, paths, roots)
		return got
	}
	t.Cleanup(func() { spawnEncodingProbe = old })

	base := runtime.NumGoroutine()
	start := time.Now()
	mode, hint := detectEncodingMode(context.Background(), dir, `Get-Content "sample.txt"`, "", nil)
	elapsed := time.Since(start)

	if !got.killed || !got.reaped {
		t.Fatalf("子进程应被杀并回收：killed=%v reaped=%v err=%v", got.killed, got.reaped, got.err)
	}
	if mode != encodingModeUTF8 || hint == "" {
		t.Fatalf("应退回 UTF-8 并说明（mode=%q hint=%q）", mode, hint)
	}
	// 预算 1.5s 是唯一的上界，允许强杀/回收开销（实测 ~5ms 量级）
	if elapsed < encodingProbeBudget || elapsed > encodingProbeBudget+900*time.Millisecond {
		t.Fatalf("探测耗时不在 [预算, 预算+余量] 内：%v", elapsed)
	}
	time.Sleep(200 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > base+1 {
		t.Fatalf("goroutine 未回到基线：before=%d after=%d", base, after)
	}
	t.Logf("阻塞子进程：预算 %v，实际 %v，killed=%v reaped=%v goroutine %d→%d",
		encodingProbeBudget, elapsed, got.killed, got.reaped, base, runtime.NumGoroutine())
}

// 8 个候选只起 1 个子进程、一次强杀：总耗时≈一次预算（进程内逐候选看门狗的旧实现是 5.5s）。
func TestDetectEncodingModeEightCandidatesOneChild(t *testing.T) {
	useBlockedProbeChild(t)
	dir := t.TempDir()
	var cmd strings.Builder
	cmd.WriteString("Get-Content")
	for i := 0; i < 8; i++ {
		name := "cand" + string(rune('a'+i)) + ".txt"
		if err := os.WriteFile(filepath.Join(dir, name), []byte("hello"), 0o644); err != nil {
			t.Fatal(err)
		}
		cmd.WriteString(` "` + name + `"`)
	}
	spawns := 0
	old := spawnEncodingProbe
	spawnEncodingProbe = func(ctx context.Context, paths []string, roots []string) encodingProbeRun {
		spawns++
		return spawnEncodingProbeProcess(ctx, paths, roots)
	}
	t.Cleanup(func() { spawnEncodingProbe = old })

	start := time.Now()
	mode, hint := detectEncodingMode(context.Background(), dir, cmd.String(), "", nil)
	elapsed := time.Since(start)

	if spawns != 1 {
		t.Fatalf("应只起 1 个探测子进程，实际 %d 个", spawns)
	}
	if mode != encodingModeUTF8 || hint == "" {
		t.Fatalf("应退回 UTF-8 并说明（mode=%q hint=%q）", mode, hint)
	}
	if elapsed > encodingProbeBudget+900*time.Millisecond {
		t.Fatalf("总耗时超出上界：%v（预算 %v）", elapsed, encodingProbeBudget)
	}
	t.Logf("8 候选 + 阻塞子进程：%v（预算 %v），spawn 次数 %d", elapsed, encodingProbeBudget, spawns)
}

// 进来就取消：一次子进程都不起、耗时几微秒（取消后绝不能还起进程）。
func TestDetectEncodingModeCancelledBeforeStart(t *testing.T) {
	useBlockedProbeChild(t)
	spawns := 0
	old := spawnEncodingProbe
	spawnEncodingProbe = func(ctx context.Context, paths []string, roots []string) encodingProbeRun {
		spawns++
		return encodingProbeRun{}
	}
	t.Cleanup(func() { spawnEncodingProbe = old })

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start := time.Now()
	mode, hint := detectEncodingMode(ctx, t.TempDir(), `Get-Content "sample.txt"`, "", nil)
	elapsed := time.Since(start)

	if spawns != 0 {
		t.Fatalf("取消后不应起子进程，实际 %d 次", spawns)
	}
	if mode != encodingModeUTF8 || !strings.Contains(hint, "取消") {
		t.Fatalf("应退回 UTF-8 并说明取消（mode=%q hint=%q）", mode, hint)
	}
	if elapsed > 100*time.Millisecond {
		t.Fatalf("已取消的会话应立刻返回，实际 %v", elapsed)
	}
}

// 飞行中取消：快速杀进程并回收（远小于预算）。
func TestDetectEncodingModeCancelMidFlight(t *testing.T) {
	useBlockedProbeChild(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "sample.txt")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	var got encodingProbeRun
	old := spawnEncodingProbe
	spawnEncodingProbe = func(ctx context.Context, paths []string, roots []string) encodingProbeRun {
		got = spawnEncodingProbeProcess(ctx, paths, roots)
		return got
	}
	t.Cleanup(func() { spawnEncodingProbe = old })

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(150 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	mode, hint := detectEncodingMode(ctx, dir, `Get-Content "sample.txt"`, "", nil)
	elapsed := time.Since(start)

	if mode != encodingModeUTF8 || hint == "" {
		t.Fatalf("应退回 UTF-8 并说明（mode=%q hint=%q）", mode, hint)
	}
	if !got.killed || !got.reaped {
		t.Fatalf("取消应杀掉并回收子进程：killed=%v reaped=%v", got.killed, got.reaped)
	}
	if elapsed > 700*time.Millisecond {
		t.Fatalf("取消应收尾很快（远小于预算），实际 %v", elapsed)
	}
}

// 没有候选：不起子进程、不花时间。
func TestDetectEncodingModeNoCandidates(t *testing.T) {
	spawns := 0
	old := spawnEncodingProbe
	spawnEncodingProbe = func(ctx context.Context, paths []string, roots []string) encodingProbeRun {
		spawns++
		return encodingProbeRun{}
	}
	t.Cleanup(func() { spawnEncodingProbe = old })

	start := time.Now()
	mode, hint := detectEncodingMode(context.Background(), t.TempDir(), `Get-Process powershell`, "", nil)
	if spawns != 0 || mode != encodingModeUTF8 || hint != "" {
		t.Fatalf("无候选不该探测：spawn=%d mode=%q hint=%q", spawns, mode, hint)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("无候选应立刻返回，实际 %v", elapsed)
	}
}

// 探测子进程起不来：降级为 UTF-8 并说明（不能默默按 UTF-8）。
func TestDetectEncodingModeSpawnFailureDiagnosable(t *testing.T) {
	old := encodingProbeArgv
	encodingProbeArgv = func() (string, []string, error) {
		return filepath.Join(t.TempDir(), "no-such-probe.exe"), nil, nil
	}
	t.Cleanup(func() { encodingProbeArgv = old })
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "sample.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	mode, hint := detectEncodingMode(context.Background(), dir, `Get-Content "sample.txt"`, "", nil)
	if mode != encodingModeUTF8 || !strings.Contains(hint, "无法完成编码探测") {
		t.Fatalf("启动失败应降级并说明，实得 %q / %q", mode, hint)
	}
}

// 探测阶段被取消（用户点停止）时，run_command 绝不能再把命令起起来。
func TestRunCommandCancelledBeforeProbeStartsNothing(t *testing.T) {
	a := newCompactTestApp(t)
	a.initRuntime()
	dir := t.TempDir()
	marker := filepath.Join(dir, "started.txt")
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 从进来就已经取消
	tc := &toolContext{ctx: ctx, app: a, sess: &SessionMeta{ID: "enc-cancel", Mode: "task", Permission: permFull}}
	args, _ := json.Marshal(map[string]any{
		"command": `Set-Content "` + filepath.Base(marker) + `" x`,
		"cwd":     dir,
	})
	out, err := runCommand(tc, json.RawMessage(args))
	if err == nil {
		t.Fatalf("取消后仍执行了命令：%s", out)
	}
	if !strings.Contains(err.Error(), "会话已取消") {
		t.Fatalf("拒绝原因应是会话已取消，实得 %v", err)
	}
	time.Sleep(100 * time.Millisecond) // 给（不该出现的）子进程一点落盘时间
	if _, serr := os.Stat(marker); serr == nil {
		t.Fatal("取消后仍然启动了命令（标记文件被写出）")
	}
}
