package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 用线上真实的组件包验证解压链路（默认跳过，发布前手动跑一次）：
//
//	$env:JX_VOICE_ZIP = ".jxai\tmp\voicezip\voice-1.3.8.zip"; go test -run TestUnzipRealVoiceArtifact -v .
//
// 2026-11-13 事故的包就是这种形状（PS 5.1 Compress-Archive：条目名全是反斜杠、
// 目录条目以反斜杠结尾），修复前必然死在 voicetype\go-webview2\.github。
func TestUnzipRealVoiceArtifact(t *testing.T) {
	zipPath := os.Getenv("JX_VOICE_ZIP")
	if zipPath == "" {
		t.Skip("未设置 JX_VOICE_ZIP，跳过真实组件包解压验证")
	}
	if _, err := os.Stat(zipPath); err != nil {
		t.Fatalf("组件包不存在: %v", err)
	}
	dest := t.TempDir()
	if err := unzip(zipPath, dest); err != nil {
		t.Fatalf("解压失败: %v", err)
	}
	// 包根就是组件目录内容：manifest.json 必须落在根上
	root, err := findRootWith(dest, "manifest.json")
	if err != nil {
		t.Fatalf("找不到 manifest.json: %v", err)
	}
	if root != dest {
		t.Fatalf("组件包结构不对，manifest.json 在 %s", root)
	}
	// 曾经的失败点：.github 必须是目录，里面的 workflow 文件必须落盘
	gh := filepath.Join(dest, "voicetype", "go-webview2", ".github")
	st, err := os.Stat(gh)
	if err != nil {
		t.Fatalf(".github 不存在: %v", err)
	}
	if !st.IsDir() {
		t.Fatalf(".github 被解成了文件")
	}
	wf := filepath.Join(gh, "workflows", "go.yml")
	if wfst, err := os.Stat(wf); err != nil || wfst.IsDir() {
		t.Fatalf(".github/workflows/go.yml 没落盘: %v", err)
	}
	// 顺带确认大文件也真的写出来了（模型是组件的主内容）
	if modelSt, err := os.Stat(filepath.Join(dest, "models", "xasr", "encoder-epoch-99-avg-1.int8.onnx")); err != nil || modelSt.Size() < 100<<20 {
		t.Fatalf("模型文件不完整: %v", err)
	}
}
