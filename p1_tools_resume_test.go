package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

func TestP1F14ResumeRoundTrip(t *testing.T) {
	a, m := batchTestApp(t, "p1-resume")
	tc := batchTestCtx(t, a, m, "read")
	text := strings.Repeat("甲😀z", 100000)
	p := filepath.Join(m.Workspace, "giant.txt")
	if err := os.WriteFile(p, []byte(text+"\nlast"), 0600); err != nil {
		t.Fatal(err)
	}
	off := 0
	var got strings.Builder
	for count := 0; count < 20; count++ {
		raw, _ := json.Marshal(map[string]any{"files": []any{map[string]any{"path": p, "offset": 1, "byte_offset": off, "limit": 1}}})
		out, err := runReadFile(tc, raw)
		if err != nil {
			t.Fatal(err)
		}
		if len(out) > readOutputMaxBytes {
			t.Fatal("budget exceeded")
		}
		body := out[strings.Index(out, "\n")+1:]
		end := strings.Index(body, "\n…（")
		if end < 0 {
			t.Fatal("missing note")
		}
		got.WriteString(body[:end])
		match := regexp.MustCompile(`byte_offset=(\d+)`).FindStringSubmatch(out)
		if len(match) == 0 {
			break
		}
		next, _ := strconv.Atoi(match[1])
		if next <= off {
			t.Fatal("cursor did not advance")
		}
		off = next
	}
	if got.String() != text {
		t.Fatalf("roundtrip got %d want %d", got.Len(), len(text))
	}
	t.Logf("roundtrip restored all %d UTF-8 bytes", got.Len())
	raw := []byte(fmt.Sprintf(`{"files":[{"path":%q,"byte_offset":1}]}`, filepath.ToSlash(p)))
	out, _ := runReadFile(tc, raw)
	if !strings.Contains(out, "字符边界") {
		t.Fatal(out)
	}
}
