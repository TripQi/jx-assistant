package main

import (
	"os"
	"path/filepath"
	"testing"
)

// 组件目录替换前要清只读位：Windows 上只读文件删不掉，os.RemoveAll 会整块失败，
// 组件更新就永远卡在「旧目录删不掉」。
func TestClearReadonlyTree(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "voicetype", "voicetypego.exe")
	if err := os.MkdirAll(filepath.Dir(sub), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sub, []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(sub, 0444); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(sub); st.Mode().Perm()&0200 != 0 {
		t.Skip("本平台不支持只读位，跳过")
	}
	clearReadonlyTree(dir)
	st, err := os.Stat(sub)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm()&0200 == 0 {
		t.Fatalf("只读位没被清掉: %v", st.Mode())
	}
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("清完只读位仍删不掉: %v", err)
	}
}
