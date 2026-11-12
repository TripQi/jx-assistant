package main

// 工具结果统一封顶的测试：agent 层 clipToolResult 是最后一层兜底，任何工具的
// 单次输出超过 toolResultMaxBytes（128KB）都截断，且在 rune 边界收口（不劈开
// 多字节字符）。
//
// 截断策略是「头 2/3 + 尾 1/3」：编译错误、测试汇总、命令结论通常落在输出末尾，
// 只留头部会让模型看不到结论、只能重跑命令（一次重跑＝一次十几万 token 的全量
// 输入）。所以这里除「截断生效」外还要守住两件事：
//   1. 头尾两段都真能拿到（开头标记、结尾标记都在返回值里）；
//   2. 总长度不因头尾双保留而膨胀（头尾共用同一份 128KB 预算）。

import (
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

// 标记文案里用来切分「头段 / 标记 / 尾段」的两处锚点（与实现共用常量，
// 改文案时只需改这里）。
const (
	markerOpen  = "\n…（" + toolResultClipMarker
	markerClose = "需要完整内容请用 offset / 分页参数分段读取）\n"
)

// TestClipToolResult 基本行为：未超限原样返回；超限截断并提示续读。
func TestClipToolResult(t *testing.T) {
	// 未超限：原样返回
	small := "普通工具结果"
	if got := clipToolResult(small); got != small {
		t.Fatalf("未超限应原样返回, got %q", got)
	}
	// 超限：截断 + 续读提示，且保留部分不劈开 rune
	big := strings.Repeat("字", toolResultMaxBytes) // 每个汉字 3 字节，必然超 128KB
	got := clipToolResult(big)
	if !strings.Contains(got, toolResultClipMarker) || !strings.Contains(got, "offset") {
		t.Fatal("超限应截断并提示续读")
	}
	if !utf8.ValidString(got) {
		t.Fatal("截断后必须是合法 UTF-8（不能在多字节字符中间切）")
	}
	// 截断后主体应明显小于原串
	if len(got) >= len(big)/2 {
		t.Fatalf("截断后应显著变短: got %d bytes, orig %d bytes", len(got), len(big))
	}
}

// 头部前缀与尾部后缀都要留在返回值里——这是本次改动的核心收益。
func TestClipToolResultKeepsHeadAndTail(t *testing.T) {
	const headMark = "HEAD-开头第一行\n"
	const tailMark = "\nTAIL-最后的结论：FAIL main_test.go:42"
	big := headMark + strings.Repeat("填充行内容\n", toolResultMaxBytes/6) + tailMark
	if len(big) <= toolResultMaxBytes {
		t.Fatalf("测试数据必须超限，当前 %d 字节", len(big))
	}
	got := clipToolResult(big)
	if !strings.HasPrefix(got, headMark) {
		t.Fatal("头部前缀丢失")
	}
	if !strings.HasSuffix(got, tailMark) {
		t.Fatal("尾部后缀丢失（结论在末尾的场景，模型必须能看到）")
	}

	// 切出三段：头段 / 标记 / 尾段，验证都与原文一致
	headEnd := strings.Index(got, markerOpen)
	closeIdx := strings.Index(got, markerClose)
	if headEnd < 0 || closeIdx < 0 || closeIdx < headEnd {
		t.Fatalf("省略标记缺失或结构不符:\n%s", tailOf(got, 200))
	}
	headSeg := got[:headEnd]
	tailSeg := got[closeIdx+len(markerClose):]
	if !strings.HasPrefix(big, headSeg) {
		t.Fatal("头段不是原文的前缀（内容被改动）")
	}
	if !strings.HasSuffix(big, tailSeg) {
		t.Fatal("尾段不是原文的后缀（内容被改动）")
	}
	// 预算用满：头≈2/3、尾≈1/3（rune 回退最多各让掉 3 字节）
	headBudget := toolResultMaxBytes * toolResultHeadNum / toolResultHeadDen
	tailBudget := toolResultMaxBytes - headBudget
	if len(headSeg) < headBudget-4 || len(headSeg) > headBudget {
		t.Fatalf("头段长度应≈%d，实际 %d", headBudget, len(headSeg))
	}
	if len(tailSeg) < tailBudget-4 || len(tailSeg) > tailBudget {
		t.Fatalf("尾段长度应≈%d，实际 %d", tailBudget, len(tailSeg))
	}
	if len(headSeg) <= len(tailSeg) {
		t.Fatalf("头段应比尾段长（2/3 vs 1/3）: head=%d tail=%d", len(headSeg), len(tailSeg))
	}

	// 标记必须同时说明三件事：总字节数、保留的头部字节、保留的尾部字节
	marker := got[headEnd:closeIdx]
	for _, want := range []string{
		fmt.Sprintf("原始共 %d 字节", len(big)),
		fmt.Sprintf("已保留头部 %d 字节 + 尾部 %d 字节", len(headSeg), len(tailSeg)),
		fmt.Sprintf("中间省略 %d 字节", len(big)-len(headSeg)-len(tailSeg)),
	} {
		if !strings.Contains(marker, want) {
			t.Fatalf("标记文案缺少 %q，实际：%s", want, marker)
		}
	}
}

// 总长度 ≈ 上限 + 标记长度：头尾共用一份预算，不能因为双保留而膨胀。
func TestClipToolResultLengthBounded(t *testing.T) {
	src := strings.Repeat("abcdefgh", toolResultMaxBytes) // 1MB 纯 ASCII（无 rune 回退）
	got := clipToolResult(src)
	if len(got) < toolResultMaxBytes-8 {
		t.Fatalf("保留长度不该明显小于上限（预算没花完）: got %d, max %d", len(got), toolResultMaxBytes)
	}
	if len(got) > toolResultMaxBytes+512 {
		t.Fatalf("总长度超出「上限 + 标记」太多: got %d, max %d", len(got), toolResultMaxBytes)
	}
}

// 多字节边界：让两个切口分别落在汉字 / emoji 中间，断言结果仍是合法 UTF-8、
// 无替换字符，且首尾字符都没被劈开。
func TestClipToolResultRuneBoundary(t *testing.T) {
	cases := []struct {
		name string
		s    string
	}{
		{"汉字-切口落在字符中间", strings.Repeat("字", toolResultMaxBytes)},
		{"汉字-前缀错开1字节", "x" + strings.Repeat("字", toolResultMaxBytes)},
		{"汉字-前缀错开2字节", "xy" + strings.Repeat("字", toolResultMaxBytes)},
		{"emoji4字节", strings.Repeat("😀", toolResultMaxBytes)},
		{"emoji-前缀错开1字节", "x" + strings.Repeat("😀", toolResultMaxBytes)},
		{"emoji-前缀错开2字节", "xy" + strings.Repeat("😀", toolResultMaxBytes)},
		{"emoji-前缀错开3字节", "xyz" + strings.Repeat("😀", toolResultMaxBytes)},
		{"中英混排", strings.Repeat("a字😀", toolResultMaxBytes)},
	}
	for _, c := range cases {
		got := clipToolResult(c.s)
		if !utf8.ValidString(got) {
			t.Fatalf("%s: 截断结果不是合法 UTF-8", c.name)
		}
		if strings.Contains(got, "\uFFFD") {
			t.Fatalf("%s: 出现替换字符（劈开了多字节字符）", c.name)
		}
		// 首尾 rune 必须完好：头部从原文第一个字符开始，尾部到原文最后一个字符结束
		firstRune, _ := utf8.DecodeRuneInString(c.s)
		gotFirst, _ := utf8.DecodeRuneInString(got)
		if firstRune != gotFirst {
			t.Fatalf("%s: 开头字符被破坏: want %q got %q", c.name, firstRune, gotFirst)
		}
		lastRune, _ := utf8.DecodeLastRuneInString(c.s)
		gotLast, _ := utf8.DecodeLastRuneInString(got)
		if lastRune != gotLast {
			t.Fatalf("%s: 末尾字符被破坏: want %q got %q", c.name, lastRune, gotLast)
		}
	}
}

// 已经带过截断标记的输入（read 等工具自己裁过）不重复裁，连提示都原样保留。
func TestClipToolResultAlreadyClipped(t *testing.T) {
	inner := "首段" + strings.Repeat("字", toolResultMaxBytes)
	once := clipToolResult(inner)
	twice := clipToolResult(once)
	if twice != once {
		t.Fatalf("已含截断标记的结果不应被再裁一次（长度 %d -> %d）", len(once), len(twice))
	}
	// 「内部工具裁过 + 尾部还有一长段内容」的情形也不能动
	manual := "…（工具结果过长已截断：内部工具自己裁的）\n" + strings.Repeat("尾巴", toolResultMaxBytes)
	if got := clipToolResult(manual); got != manual {
		t.Fatal("带内部截断标记的结果应原样返回")
	}
}

// tailOf 返回字符串末尾 n 字节（失败信息用，避免打印整段）。
func tailOf(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}
