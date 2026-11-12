package main

// 远程输出采集层 capWriter 的测试：它必须在 256KB 预算内同时保住
//   - 输出的开头（头部窗口 3/4 = 192KB）；
//   - 输出的**真正结尾**（滚动尾部窗口 1/4 = 64KB，只留最新字节）；
// 并把「被滚掉多少字节」如实记下来（dropped），供下游文案报实数。

import (
	"bytes"
	"fmt"
	"math/rand"
	"strings"
	"testing"
	"unicode/utf8"
)

const testCap = 256 * 1024

func newTestCapWriter() *capWriter { return &capWriter{cap: testCap} }

// 小输出：原样返回，不丢字节。
func TestCapWriterSmall(t *testing.T) {
	w := newTestCapWriter()
	src := []byte("$ ls\nmain.go\nREADME.md\n")
	if _, err := w.Write(src); err != nil {
		t.Fatal(err)
	}
	if got := w.Bytes(); !bytes.Equal(got, src) {
		t.Fatalf("小输出应原样返回: got %q", got)
	}
	if w.Dropped() != 0 || w.Captured() != len(src) {
		t.Fatalf("不该有丢弃: dropped=%d captured=%d", w.Dropped(), w.Captured())
	}
	if w.String() != string(src) {
		t.Fatal("String 应与 Bytes 一致")
	}
}

// 恰好等于上限：完整保留，一个字节都不丢。
func TestCapWriterExactlyAtCap(t *testing.T) {
	w := newTestCapWriter()
	src := bytes.Repeat([]byte("a"), testCap)
	if n, err := w.Write(src); n != len(src) || err != nil {
		t.Fatalf("Write 应吃掉全部字节: n=%d err=%v", n, err)
	}
	if got := w.Bytes(); !bytes.Equal(got, src) {
		t.Fatalf("恰好等于上限应完整保留: got %d bytes", len(got))
	}
	if w.Dropped() != 0 {
		t.Fatalf("恰好等于上限不该丢字节: dropped=%d", w.Dropped())
	}
}

// 超过上限：开头在、真正的结尾也在、丢弃计数正确。
func TestCapWriterKeepsHeadAndRealTail(t *testing.T) {
	const headMark = "HEAD-最早的一行\n"
	const tailMark = "TAIL-最后的结论：exit status 1\n"
	// 1MB：头标记 + 26 万行填充 + 尾标记（分多个 chunk 写，模拟 ssh 分片）
	var b bytes.Buffer
	b.WriteString(headMark)
	for b.Len() < 1024*1024 {
		b.WriteString("filler line for the remote command output\n")
	}
	b.WriteString(tailMark)
	src := b.Bytes()

	w := newTestCapWriter()
	for p := src; len(p) > 0; {
		n := 7 * 1024
		if n > len(p) {
			n = len(p)
		}
		if _, err := w.Write(p[:n]); err != nil {
			t.Fatal(err)
		}
		p = p[n:]
	}
	got := w.Bytes()
	if !strings.HasPrefix(string(got), headMark) {
		t.Fatal("开头丢失")
	}
	if !strings.HasSuffix(string(got), tailMark) {
		t.Fatalf("真正的结尾丢失（应该是滚动窗口保留最新字节）: 末尾 %q", tailOf(string(got), 80))
	}
	// 采集到的东西 = 头窗口 + 尾窗口 = 上限；丢弃 = 原长 - 上限
	if len(got) != testCap {
		t.Fatalf("采集长度应为上限 %d，实际 %d", testCap, len(got))
	}
	wantDropped := int64(len(src) - testCap)
	if w.Dropped() != wantDropped {
		t.Fatalf("丢弃计数不对: want %d got %d", wantDropped, w.Dropped())
	}
	if w.Captured()+int(w.Dropped()) != len(src) {
		t.Fatalf("采集 + 丢弃应等于原始长度: %d+%d != %d", w.Captured(), w.Dropped(), len(src))
	}
	// 尾部窗口只保留最新：倒数第二段的可辨识文本应来自输出结尾附近
	tailWindow := got[len(got)-w.tailLimit():]
	if !strings.Contains(string(tailWindow), "最后的结论") {
		t.Fatal("尾部窗口里应是输出结尾的内容")
	}
}

// 一次写入就超过上限：只留最新的 64KB 尾部 + 占满的头部。
func TestCapWriterSingleHugeWrite(t *testing.T) {
	w := newTestCapWriter()
	src := bytes.Repeat([]byte("x"), 2*testCap)
	src = append(src, []byte("\nTHE-END\n")...)
	if _, err := w.Write(src); err != nil {
		t.Fatal(err)
	}
	got := w.Bytes()
	if len(got) != testCap {
		t.Fatalf("长度应为上限 %d，实际 %d", testCap, len(got))
	}
	if !strings.HasSuffix(string(got), "\nTHE-END\n") {
		t.Fatal("大块写入后也应保留真正结尾")
	}
	if w.Dropped() != int64(len(src)-testCap) {
		t.Fatalf("丢弃计数不对: want %d got %d", len(src)-testCap, w.Dropped())
	}
}

// 多字节边界：按 1~4 字节的碎块写入，接缝必然落在字符中间；取用时必须收口。
func TestCapWriterRuneBoundary(t *testing.T) {
	for _, unit := range []string{"字", "😀", "a字😀"} {
		for _, chunk := range []int{1, 2, 3, 5, 7, 1024} {
			w := newTestCapWriter()
			var b strings.Builder
			for b.Len() < 3*testCap {
				b.WriteString(unit)
			}
			src := b.String()
			for p := src; len(p) > 0; {
				n := chunk
				if n > len(p) {
					n = len(p)
				}
				if _, err := w.Write([]byte(p[:n])); err != nil {
					t.Fatal(err)
				}
				p = p[n:]
			}
			got := w.Bytes()
			if !utf8.Valid(got) {
				t.Fatalf("unit=%q chunk=%d: 接缝处收口失败，结果不是合法 UTF-8", unit, chunk)
			}
			if strings.Contains(string(got), "\uFFFD") {
				t.Fatalf("unit=%q chunk=%d: 出现替换字符", unit, chunk)
			}
			// 头 / 尾各自也应是完整字符
			last, _ := utf8.DecodeLastRune(got)
			first, _ := utf8.DecodeRune(got)
			if last == utf8.RuneError || first == utf8.RuneError {
				t.Fatalf("unit=%q chunk=%d: 首尾字符被劈开", unit, chunk)
			}
		}
	}
}

// 随机分片 + 随机字符：稳住「接缝任意位置都不出非法 UTF-8」。
func TestCapWriterRuneBoundaryRandomChunks(t *testing.T) {
	rng := rand.New(rand.NewSource(20261112))
	units := []string{"a", "中", "文", "é", "😀", "🙂", "\n", " "}
	for round := 0; round < 30; round++ {
		var b strings.Builder
		for b.Len() < 300*1024 {
			b.WriteString(units[rng.Intn(len(units))])
		}
		src := b.String()
		w := newTestCapWriter()
		for p := src; len(p) > 0; {
			n := 1 + rng.Intn(4096)
			if n > len(p) {
				n = len(p)
			}
			if _, err := w.Write([]byte(p[:n])); err != nil {
				t.Fatal(err)
			}
			p = p[n:]
		}
		got := w.Bytes()
		if !utf8.Valid(got) {
			t.Fatalf("round=%d: 结果不是合法 UTF-8", round)
		}
		// 头部应与原文开头一致（ASCII 起点，接缝不会影响开头）
		if !strings.HasPrefix(string(got), string(src[:100])) {
			t.Fatalf("round=%d: 头部内容被改动", round)
		}
	}
}

// 端到端：capWriter（256KB 头尾）→ decodeCmdOutput → clipHeadTail（64KB 头尾），
// 两层叠加后模型应看到「开头 + 真正的结尾」，且标记里的数字与丢弃量对得上。
func TestCapWriterThenClipEndToEnd(t *testing.T) {
	const headMark = "HEAD-构建开始\n"
	const tailMark = "TAIL-构建失败：undefined: Widget.Do\n"
	var b strings.Builder
	b.WriteString(headMark)
	for b.Len() < 1024*1024 {
		b.WriteString("warning: looks slightly off\n")
	}
	b.WriteString(tailMark)
	src := b.String()

	w := &capWriter{cap: 256 * 1024}
	for p := src; len(p) > 0; {
		n := 16 * 1024
		if n > len(p) {
			n = len(p)
		}
		_, _ = w.Write([]byte(p[:n]))
		p = p[n:]
	}
	dropped := int(w.Dropped())
	// remote.go 的真实顺序：decodeCmdOutput → TrimRight("\r\n") → clipHeadTail
	text := strings.TrimRight(decodeCmdOutput(w.Bytes()), "\r\n")
	res := clipHeadTail(text, cmdOutputMaxBytes, func(total, head, tail int) string {
		return cmdTruncNote(total+dropped, head, tail)
	})
	// TrimRight 会去掉结尾换行，所以期望的尾标记也按同样方式收尾
	wantTail := strings.TrimRight(tailMark, "\r\n")

	if !strings.HasPrefix(res, headMark) {
		t.Fatal("模型应看到输出开头")
	}
	if !strings.HasSuffix(res, wantTail) {
		t.Fatalf("模型应看到真正的结尾（1MB 输出的结论不再丢）: 末尾 %q", tailOf(res, 80))
	}
	// 标记里的「原始共」= 采集到的 + 采集阶段丢弃的 = 真实总量
	wantTotal := fmt.Sprintf("原始共 %d 字节", len(text)+dropped)
	if !strings.Contains(res, wantTotal) {
		t.Fatalf("标记里的原始总量应含采集阶段丢弃的字节（采集 %d + 丢弃 %d）", len(text), dropped)
	}
	if !strings.Contains(res, "中间省略") {
		t.Fatal("标记应说明省略量")
	}
	if !strings.Contains(res, "重定向到文件后用 read 的 offset 分段读取，不要重复执行同一命令") {
		t.Fatal("标记缺少下一步指引")
	}
	if len(res) > cmdOutputMaxBytes+512 || len(res) < cmdOutputMaxBytes-8 {
		t.Fatalf("最终长度应≈64KB: %d", len(res))
	}
}
