package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestP1F13ReadMaxInt(t *testing.T) {
	a, m := batchTestApp(t, "p1-f13")
	p := filepath.Join(m.Workspace, "three.txt")
	if err := os.WriteFile(p, []byte("a\nb\nc"), 0600); err != nil {
		t.Fatal(err)
	}
	tc := batchTestCtx(t, a, m, "read")
	max := int(^uint(0) >> 1)
	for _, off := range []int{3, 4, max, 1} {
		t.Run(fmt.Sprintf("offset_%d", off), func(t *testing.T) {
			raw, _ := json.Marshal(map[string]any{"files": []any{map[string]any{"path": p, "offset": off, "limit": max}}})
			out, _ := runReadFile(tc, raw)
			if off > 3 {
				if !strings.Contains(out, "超出文件范围") {
					t.Fatal(out)
				}
				return
			}
			if !strings.Contains(out, "c") {
				t.Fatal(out)
			}
			t.Logf("offset=%d limit=MaxInt: %s", off, out)
		})
	}
}
