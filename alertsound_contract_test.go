package main

// 提示音方案（askAlert.onAppearSound）的**契约存在性**用例：这组断言在改动前
// 的基线（dev 2defd0a5）上全红——那一版只有单声 MessageBeep，既没有方案字段、
// 也没有自定义 wav 路径，/api/config 里当然读不到这两个键。
//
// 放在单独文件里是有意的：它只依赖「早已存在」的接口（AskAlertCfg 的反射字段、
// /api/config 的 GET/POST、beep_windows.go 源码文本），所以基线也能编译，
// 修前红是真实跑出来的，不是靠改测试制造的。修后同一份断言转绿，即为
// 「方案确实落地」的最小证据。

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
)

// getConfigAskAlert 走一遍 GET /api/config，取回 askAlert 段（沿用测试里既有的
// newWizardTestApp：只有 Cfg 与 CfgPath 的裸 App，不碰真实 data/）。
func getConfigAskAlert(t *testing.T) map[string]any {
	t.Helper()
	a := newWizardTestApp(t)
	rec := httptest.NewRecorder()
	a.handleGetConfig(rec, httptest.NewRequest("GET", "/api/config", nil))
	var out struct {
		AskAlert map[string]any `json:"askAlert"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("/api/config 响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	return out.AskAlert
}

// postAskAlert 走一遍 POST /api/config（嵌套 askAlert 部分提交），返回状态码与
// 响应体里的 AskAlert 段。
func postAskAlert(t *testing.T, body string) (int, map[string]any) {
	t.Helper()
	a := newWizardTestApp(t)
	rec := httptest.NewRecorder()
	a.handleSetConfig(rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
	var out struct {
		AskAlert map[string]any `json:"askAlert"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST /api/config 响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, out.AskAlert
}

// 1) 配置字段：方案枚举与自定义 wav 路径都挂在既有的 askAlert 段里。
func TestAskAlertSoundFieldsExist(t *testing.T) {
	typ := reflect.TypeOf(AskAlertCfg{})
	if _, ok := typ.FieldByName("OnAppearSound"); !ok {
		t.Fatal("AskAlertCfg 没有 OnAppearSound 字段：提示音无从「可选」，只有那一声 MessageBeep")
	}
	if _, ok := typ.FieldByName("OnAppearWav"); !ok {
		t.Fatal("AskAlertCfg 没有 OnAppearWav 字段：自定义 wav 路径无处落地")
	}
}

// 2) GET /api/config 把方案下发给设置页（没有它，设置页连当前档都回显不出来）。
func TestAskAlertSoundExposedByConfig(t *testing.T) {
	aa := getConfigAskAlert(t)
	if _, ok := aa["onAppearSound"]; !ok {
		t.Fatalf("GET /api/config 的 askAlert 段没有 onAppearSound 键：设置页无处回显音效方案（实际 %v）", aa)
	}
	if _, ok := aa["onAppearWav"]; !ok {
		t.Fatalf("GET /api/config 的 askAlert 段没有 onAppearWav 键（实际 %v）", aa)
	}
	// 缺省行为＝新的默认档（明显一些的 Windows 通知音），而不是旧的单声系统叮。
	// 这里写死字面量而不是包内常量：本文件要能在改动前的基线上编译，才谈得上
	// 「修前红」——基线里根本没有那些常量。
	if got, _ := aa["onAppearSound"].(string); got != "notify" {
		t.Fatalf("缺省音效方案应为新的默认档 %q，实际 %q", "notify", got)
	}
}

// 3) POST /api/config 能选中「系统提示音」这一档并读回（往返正确，不被丢弃）。
func TestAskAlertSoundRoundTrip(t *testing.T) {
	code, aa := postAskAlert(t, `{"askAlert":{"onAppearSound":"system"}}`)
	if code != 200 {
		t.Fatalf("保存音效方案应成功，实际 HTTP %d", code)
	}
	if got, _ := aa["onAppearSound"].(string); got != "system" {
		t.Fatalf("保存后应回显 system，实际 %q（%v）——说明这一档被当未知键丢掉了", got, aa)
	}
}

// 4) 播放实现不再是「只会 MessageBeep」：winmm 的 PlaySound 才是能播指定 wav
// 的那条路（异步、不阻塞请求）。基线版本 beep_windows.go 里根本没有它。
func TestAlertSoundPlaysWavNotOnlyMessageBeep(t *testing.T) {
	src, err := os.ReadFile("beep_windows.go")
	if err != nil {
		t.Fatalf("读 beep_windows.go 失败: %v", err)
	}
	if !strings.Contains(string(src), "PlaySound") {
		t.Fatal("beep_windows.go 里没有 PlaySound：播放仍只有单声 MessageBeep，无法播指定音效文件")
	}
}
