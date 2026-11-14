package main

import (
	"archive/zip"
	"io"
	"os"
	"path/filepath"
	"testing"
)

// 现场事故（2026-11-13）：线上 voice-1.3.8.zip 由 PS 5.1 Compress-Archive 打包，
// 它的条目名用反斜杠当分隔符、目录条目以反斜杠结尾、external attrs 为 0
// （.NET Framework 不做 ZIP 规范要求的 "\"→"/" 转换）。客户端 unzip 只认
// 结尾是 "/" 的目录条目，于是把 `voicetype\go-webview2\.github\` 解成一个名叫
// `.github` 的文件；下一条 `...\.github\workflows\go.yml` 再要它当目录，整包解压失败：
//
//	组件 voice 静默更新失败: 解压失败: mkdir ...\voicetype\go-webview2\.github:
//	The system cannot find the path specified.
//
// 这个测试用同样的条目形状把契约钉死：反斜杠分隔符与反斜杠结尾的目录条目都要被
// 正确还原成目录树。
// oldUnzip 是 2026-11-13 修复前的 unzip 原样逻辑（只认 "/" 结尾的目录条目，
// 不做条目名规范化）。它代表用户机器上已经装着的旧客户端。
func oldUnzip(zipPath, dest string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer r.Close()
	for _, f := range r.File {
		target := filepath.Join(dest, filepath.FromSlash(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		src, err := f.Open()
		if err != nil {
			return err
		}
		dst, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
		if err != nil {
			src.Close()
			return err
		}
		_, err = io.Copy(dst, src)
		src.Close()
		dst.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

// 重打的组件包必须能被**旧客户端**直接解开——这是已经在用户机器上的安装版
// 不用等新 engine 就能自愈的前提（发布端不产反斜杠目录条目）。
//
//	$env:JX_REPACKED_ZIP = "打包\静默更新\<版本>\voice-<版本>.zip"; go test -run TestOldClientCanReadRepackedZip -v .
func TestOldClientCanReadRepackedZip(t *testing.T) {
	zipPath := os.Getenv("JX_REPACKED_ZIP")
	if zipPath == "" {
		t.Skip("未设置 JX_REPACKED_ZIP，跳过旧客户端兼容验证")
	}
	dest := t.TempDir()
	if err := oldUnzip(zipPath, dest); err != nil {
		t.Fatalf("旧客户端逻辑解不开重打的包（老机器就自愈不了）: %v", err)
	}
	if _, err := findRootWith(dest, "manifest.json"); err != nil {
		t.Fatalf("解压后找不到 manifest.json: %v", err)
	}
}

func TestUnzipBackslashSeparators(t *testing.T) {
	for _, tc := range []struct {
		name string
		// path 用 "/" 表示期望的解压结果；在 zip 里按原样（含反斜杠）写入
		entries [][2]string
		want    []string
	}{
		{
			name: "反斜杠目录条目 + 反斜杠文件条目",
			entries: [][2]string{
				{`voicetype\go-webview2\.github\`, ""},
				{`voicetype\go-webview2\.github\workflows\go.yml`, "name: CI\n"},
				{`voicetype\go-webview2\cmd\`, ""},
				{`voicetype\go-webview2\cmd\demo\main.go`, "package main\n"},
				{`asr_worker.py`, "print(1)\n"},
			},
			want: []string{
				"asr_worker.py",
				"voicetype/go-webview2/.github/workflows/go.yml",
				"voicetype/go-webview2/cmd/demo/main.go",
			},
		},
		{
			name: "标准正斜杠包不受影响",
			entries: [][2]string{
				{`voicetype/go-webview2/.github/`, ""},
				{`voicetype/go-webview2/.github/workflows/go.yml`, "name: CI\n"},
			},
			want: []string{"voicetype/go-webview2/.github/workflows/go.yml"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			zipPath := filepath.Join(dir, "block.zip")
			f, err := os.Create(zipPath)
			if err != nil {
				t.Fatal(err)
			}
			zw := zip.NewWriter(f)
			for _, e := range tc.entries {
				h := &zip.FileHeader{Name: e[0], Method: zip.Store}
				w, err := zw.CreateHeader(h)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := w.Write([]byte(e[1])); err != nil {
					t.Fatal(err)
				}
			}
			if err := zw.Close(); err != nil {
				t.Fatal(err)
			}
			f.Close()

			dest := filepath.Join(dir, "out")
			if err := unzip(zipPath, dest); err != nil {
				t.Fatalf("解压失败: %v", err)
			}
			for _, rel := range tc.want {
				p := filepath.Join(dest, filepath.FromSlash(rel))
				st, err := os.Stat(p)
				if err != nil {
					t.Fatalf("期望文件不存在 %s: %v", rel, err)
				}
				if st.IsDir() {
					t.Fatalf("期望是文件却是目录: %s", rel)
				}
			}
			// 曾经的症状：目录条目被解成一个同名文件，挡住后面的子项
			if st, err := os.Stat(filepath.Join(dest, "voicetype", "go-webview2", ".github")); err == nil && !st.IsDir() {
				t.Fatalf(".github 被解成了文件而不是目录")
			}
		})
	}
}
