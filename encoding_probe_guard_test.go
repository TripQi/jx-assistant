package main

// run_command 编码探测的边界测试。
//
// 背景（2026-11-17 实锤）：探测跑在 run_command 启动子进程**之前**，候选路径是从
// 命令正文（含提交说明）里捞出来的。旧实现只按 `IsDir()` 过滤候选、再直接
// os.Open+Read 探测——Windows 保留设备名带任何扩展名仍指向设备，一个落在正文里的
// 设备名就能让探测去读真实设备（串口等），Read 永不返回，命令一个子进程都没起，
// runChild 的超时那时还没开始计时。
//
// 夹具原则：绝不真实打开/读取设备或串口。设备名闸门用纯字符串判定验证；"读阻塞"
// 用 os.Pipe（写端不写）模拟，并由"句柄已关闭 + 读线程已退出"证明超时收尾没有留下
// 永久阻塞的读；普通文件的编码行为用临时目录里的真文件对照。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// 设备名/设备命名空间/备用数据流/网络路径一律不探测（Windows 语义）。
func TestProbePathBlockedWindowsDevices(t *testing.T) {
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
		if why := probePathBlocked(p); why == "" {
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
		if why := probePathBlocked(p); why != "" {
			t.Errorf("%q 不该被拒（%s）", p, why)
		}
	}
}

// 候选提取：设备名候选绝不能进探测队列（旧实现只用 IsDir 过滤，设备能通过），
// 普通文件必须照旧被抓到。
func TestExtractPathCandidatesSkipsDeviceNames(t *testing.T) {
	if !isWindowsRuntime {
		t.Skip("Windows 保留设备名语义")
	}
	dir := t.TempDir()
	normal := filepath.Join(dir, "normal.txt")
	if err := os.WriteFile(normal, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	got := extractPathCandidates(`Get-Content "normal.txt"`, dir)
	if len(got) != 1 || got[0] != normal {
		t.Fatalf("普通文件应照旧被探测，实得 %v", got)
	}
	for _, name := range []string{"NUL.md", "nul.txt"} {
		if cands := extractPathCandidates(`Get-Content "`+name+`"`, dir); len(cands) != 0 {
			t.Fatalf("%s 是 Windows 设备名，不该成为探测候选，实得 %v", name, cands)
		}
	}
}

// 读阻塞的候选：取消/到点必须"关句柄 → 等读线程退出"，不能只返回、把读线程和
// 句柄丢在那儿。
func TestProbeBoundedBlockedRead(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "blocked.txt")
	if err := os.WriteFile(p, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name    string
		prepare func(context.Context) (context.Context, context.CancelFunc)
		minWait time.Duration // 期望至少等这么久：证明确实走到了看门狗，而不是提前返回
	}{
		{"取消即收尾", func(parent context.Context) (context.Context, context.CancelFunc) {
			ctx, cancel := context.WithCancel(parent)
			go func() {
				time.Sleep(60 * time.Millisecond)
				cancel()
			}()
			return ctx, cancel
		}, 0},
		{"到点自行收尾", func(parent context.Context) (context.Context, context.CancelFunc) {
			return context.WithTimeout(parent, probeFileBudget+probeCancelGrace+3*time.Second)
		}, probeFileBudget},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pr, pw, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer pw.Close() // 写端一直不写：读端永远等不到数据
			restore := probeJobFn
			probeJobFn = func(_ string, register func(*os.File) bool) (TextEncoding, string) {
				if !register(pr) {
					return EncUnknown, ""
				}
				pr.Read(make([]byte, 1)) // 阻塞到看门狗把句柄关掉
				return EncUnknown, ""
			}
			defer func() { probeJobFn = restore }()
			ctx, cancel := c.prepare(context.Background())
			defer cancel()
			start := time.Now()
			enc, note := probeTextFileBounded(ctx, p)
			elapsed := time.Since(start)
			if enc != EncUnknown || note == "" {
				t.Fatalf("阻塞候选应判未知并注明原因，实得 %v / %q", enc, note)
			}
			if strings.Contains(note, "未退出") {
				t.Fatalf("超时收尾留下了永久阻塞的读线程：%q", note)
			}
			if elapsed < c.minWait {
				t.Fatalf("过早返回（%v），没走到看门狗路径", elapsed)
			}
			if elapsed > probeFileBudget+probeCancelGrace+800*time.Millisecond {
				t.Fatalf("有界返回失败：%v", elapsed)
			}
			// 句柄确实关了：已关闭的 os.File 再 Close 会报错
			if cerr := pr.Close(); cerr == nil {
				t.Fatal("读端句柄没关（泄漏）")
			}
		})
	}
}

// 设备/目录/管道不读：拿到句柄后先确认普通文件（用目录做安全对照）。
func TestProbeBoundedRejectsNonRegular(t *testing.T) {
	dir := t.TempDir()
	enc, note := probeTextFileBounded(context.Background(), dir)
	if enc != EncUnknown || !strings.Contains(note, "不是普通文件") {
		t.Fatalf("目录不该被探测，实得 %v / %q", enc, note)
	}
}

// 普通文件与中文路径的原编码处理必须不变（UTF-8 / GBK / UTF-16LE 各取一条）。
func TestDetectEncodingModeCtxKeepsNormalFiles(t *testing.T) {
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
	write("老脚本.bat", gbkBytes(t, "echo 中文批处理\r\n"))
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
		if m, hint := detectEncodingMode(context.Background(), dir, c.cmd, ""); m != c.want || hint != "" {
			t.Fatalf("%q: 期望 %q 无提示，实得 %q / %q", c.cmd, c.want, m, hint)
		}
	}
}

// 探测阶段被取消（用户点停止）时，run_command 绝不能再把子进程起起来。
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
