package main

import (
	"strings"
	"testing"
)

func TestP1F14GiantLine(t *testing.T) {
	var b strings.Builder
	_, whole := appendReadSeg(&b, readSeg{header: "H", lines: []string{strings.Repeat("x", 1000)}, startLine: 1}, 500)
	t.Log(b.String())
	if whole || !strings.Contains(b.String(), "offset=1") || !strings.Contains(b.String(), "byte_offset=") {
		t.Fatal("missing exact same-line resume")
	}
}
func TestP1L01Notes(t *testing.T) {
	var b strings.Builder
	_, whole := appendReadSeg(&b, readSeg{header: "H", lines: []string{strings.Repeat("x", 490)}, notes: "NOTE" + strings.Repeat("n", 100), startLine: 1}, 500)
	t.Log(b.String())
	if whole || !strings.Contains(b.String(), "不完整") {
		t.Fatal("notes silently lost")
	}
}
