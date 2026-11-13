//go:build windows

package main

import "testing"

// navGuardAllow 是主 WebView 导航守卫的放行判定：本机服务源与内部页放行，
// 其余一律出壳。表驱动覆盖各来源形态与畸形输入。
func TestNavGuardAllow(t *testing.T) {
	cases := []struct {
		raw  string
		want bool
		note string
	}{
		{"http://127.0.0.1:9000/", true, "本机服务（默认形态）"},
		{"http://127.0.0.1:65535/?shell=desktop", true, "本机任意端口"},
		{"http://localhost:9000/", true, "localhost 别名"},
		{"http://LOCALHOST:9000/", true, "host 大小写不敏感"},
		{"http://[::1]:9000/", true, "IPv6 回环"},
		{"about:blank", true, "内部页"},
		{"data:text/html,<h1>x</h1>", true, "data 页"},
		{"blob:http://127.0.0.1:9000/uuid", true, "blob 预览"},
		{"devtools://devtools/bundled/inspector.html", true, "开发者工具"},

		{"https://jxaizz.com:3000/admin/jxharness/releases/download/x/y.exe", false, "官网直链（2026-10-31 故障现场）"},
		{"https://jxaizz.com:2227/updates/latest", false, "服务端 API 源"},
		{"http://jxaizz.com/", false, "官网 80"},
		{"http://192.168.0.210:2227/", false, "内网他机"},
		{"http://10.0.0.5:8080/", false, "其他内网段"},
		{"https://example.com/", false, "任意外网"},
		{"file:///C:/Windows/notepad.exe", false, "本地文件"},
		{"javascript:alert(1)", false, "脚本协议"},
		{"ftp://example.com/", false, "其他协议"},
		{"", false, "空串"},
		{":::", false, "畸形输入不 panic"},
	}
	for _, c := range cases {
		if got := navGuardAllow(c.raw); got != c.want {
			t.Errorf("navGuardAllow(%q) = %v, want %v（%s）", c.raw, got, c.want, c.note)
		}
	}
}
