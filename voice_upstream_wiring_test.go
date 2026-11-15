package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// 主模块不能 import 独立输入法模块；只核对三条产品路径均接预算入口及用户提示。
func TestVoiceUpstreamCallsiteWiring(t *testing.T) {
	const path = "组件/voice/voicetype/main.go"
	f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"vtFinishSession": false, "vtMidLoop": false, "vtReidentify": false}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, ok := want[fn.Name.Name]; !ok {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			id, ok := call.Fun.(*ast.Ident)
			if !ok {
				return true
			}
			if id.Name == "vtTranscribe" {
				t.Errorf("%s bypasses timeout", fn.Name.Name)
			}
			if id.Name == "vtTranscribeWithTimeout" && len(call.Args) == 2 {
				budget, ok := call.Args[1].(*ast.CallExpr)
				if ok {
					id, ok := budget.Fun.(*ast.Ident)
					if ok && id.Name == "vtAsrTimeoutSec" {
						want[fn.Name.Name] = true
					}
				}
			}
			return true
		})
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("%s missing shared budget entry", name)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(raw), `"识别超时，请重试"`) != 2 {
		t.Error("final/reidentify must both show explicit timeout")
	}
}
