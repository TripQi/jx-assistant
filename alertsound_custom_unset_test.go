package main

// 「切到自定义 WAV 档、路径还没填」的可用性回归（用户实测的 bug）。
//
// 复现：设置·常规·询问提醒 →「提示音方案」下拉选「自定义 WAV 文件」，change
// 事件立刻整段保存 askAlert；旧实现把「custom + 空路径」判成「自定义 WAV 路径
// 不可用」→ 400 → 设置页弹「保存失败」模态框。用户还没机会填路径就被挡住了。
//
// 本文件的口径：**未填 ≠ 填错**。
//
//	未填（custom + 空/空白路径）＝「已选自定义、尚未填路径」：保存必须成功，
//	  播放回落系统提示音并记一条说明日志；
//	填错（非 .wav / 文件不存在）＝真的不可用：仍然 400、原配置不被改写。
//
// 这里的断言只用改动前就存在的接口（/api/config、/api/alert/sound、
// resolveAlertSound/playAlertSound、alertWavProblem），所以在基线 ed43ce30 上
// 能编译、且是真跑出来的红；改动后同一份断言转绿，就是修复的最小证据。

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// askAlertPost 走一遍 POST /api/config（嵌套 askAlert 部分提交）。
func askAlertPost(t *testing.T, a *App, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleSetConfig(rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
	var out struct {
		AskAlert map[string]any `json:"askAlert"`
		Error    string         `json:"error"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("POST /api/config 响应不是 JSON: %v (%s)", err, rec.Body.String())
	}
	return rec.Code, map[string]any{"askAlert": out.AskAlert, "error": out.Error}
}

// askAlertGet 走一遍 GET /api/config，取回 askAlert 段（服务端实际值）。
func askAlertGet(t *testing.T, a *App) map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleGetConfig(rec, httptest.NewRequest("GET", "/api/config", nil))
	var out struct {
		AskAlert map[string]any `json:"askAlert"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET /api/config 不是 JSON: %v", err)
	}
	return out.AskAlert
}

// makeTempWav 造一个测试用 wav 文件（内容不重要：播放原语在测试里是桩）。
func makeTempWav(t *testing.T, name string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatalf("造测试 wav 失败: %v", err)
	}
	return p
}

// a) custom + 空路径 → 保存必须成功（200），回读仍是 custom、路径为空。
//
// 这就是用户那次报错的原始场景：下拉切档只提交 onAppearSound，路径此刻必然是空的。
func TestAlertSoundCustomUnsetSaves(t *testing.T) {
	a := newWizardTestApp(t)

	// 只提交档位（正是设置页切档时发的那个请求体）
	code, body := askAlertPost(t, a, `{"askAlert":{"onAppearSound":"custom"}}`)
	if code != 200 {
		t.Fatalf("切到自定义档（路径还没填）应保存成功，实际 HTTP %d（%v）——这正是用户遇到的 400 弹窗", code, body)
	}
	aa := askAlertGet(t, a)
	if got, _ := aa["onAppearSound"].(string); got != alertSoundCustom {
		t.Fatalf("回读的档位应为 custom，实际 %q（%v）", got, aa)
	}
	if got, _ := aa["onAppearWav"].(string); got != "" {
		t.Fatalf("还没填路径时回读应为空串，实际 %q", got)
	}
	t.Logf("切档（空路径）：POST=%d 回读=%v", code, aa)

	// 显式提交空白路径（前端可能把用户敲的空格 trim 后发上来）同样算「未填」
	if code, body = askAlertPost(t, a, `{"askAlert":{"onAppearWav":"   "}}`); code != 200 {
		t.Fatalf("空白路径应视为未填、允许保存，实际 HTTP %d（%v）", code, body)
	}
	if got, _ := askAlertGet(t, a)["onAppearWav"].(string); got != "" {
		t.Fatalf("空白路径保存后应存为空串，实际 %q", got)
	}
}

// b) custom + 空路径时播放回落 MessageBeep，并记一条说明「未填路径」的日志。
func TestAlertSoundCustomUnsetFallsBackWithLog(t *testing.T) {
	spy := newSoundSpy(t)
	cfg := &Config{AskAlert: AskAlertCfg{OnAppearSound: alertSoundCustom, OnAppearWav: ""}}
	plan := cfg.resolveAlertSound()
	if plan.invalid {
		t.Fatalf("「还没填路径」不该被判成配置非法（否则保存/试听都会 400，用户没法填）：%+v", plan)
	}
	var played, why string
	hits := captureLog(t, func() {
		played, why = playAlertSound(plan)
	}, "提示音回落系统 MessageBeep")
	wavs, beeps := spy.counts()
	t.Logf("未填路径：played=%s fallback=%q wav=%v beep=%d 日志=%v", played, why, wavs, beeps, hits)
	if played != "beep" || beeps != 1 || len(wavs) != 0 {
		t.Fatalf("未填路径应回落叮 1 声、不碰 wav，实际 played=%s beep=%d wav=%v", played, beeps, wavs)
	}
	if len(hits) != 1 {
		t.Fatalf("应记 1 条说明回落的日志，实际 %v", hits)
	}
	if !strings.Contains(hits[0], "未填写自定义 WAV 路径") {
		t.Fatalf("日志要说明「未填写自定义 WAV 路径」，实际 %q", hits[0])
	}
	if !strings.Contains(why, "未填写自定义 WAV 路径") {
		t.Fatalf("回落原因要能直接给设置页显示（说明还没填路径），实际 %q", why)
	}
}

// c) custom + 非空非法路径 → 仍然 400，且原配置（档位 + 路径）不被改写。
//
// 「填了但填错」这条不许放水：用户设了自定义却听到系统叮，比报错更让人困惑。
func TestAlertSoundCustomInvalidStillRejected(t *testing.T) {
	a := newWizardTestApp(t)
	// 先把状态设成「已选 custom、路径未填」
	if code, body := askAlertPost(t, a, `{"askAlert":{"onAppearSound":"custom"}}`); code != 200 {
		t.Fatalf("前置：切到 custom 应成功，实际 %d（%v）", code, body)
	}
	dir := t.TempDir()
	txt := filepath.Join(dir, "不是音频.txt")
	if err := os.WriteFile(txt, []byte("hello"), 0o644); err != nil {
		t.Fatalf("造测试文件失败: %v", err)
	}
	missing := filepath.Join(dir, "已经删掉的.wav")
	for _, c := range []struct {
		name string
		path string
		want string
	}{
		{"非 wav 文件", txt, "只支持 .wav"},
		{"文件不存在", missing, "文件打不开"},
		{"相对路径", `声音\提示.wav`, "绝对路径"},
	} {
		t.Run(c.name, func(t *testing.T) {
			body := `{"askAlert":{"onAppearWav":"` + strings.ReplaceAll(c.path, `\`, `\\`) + `"}}`
			code, out := askAlertPost(t, a, body)
			t.Logf("%s：POST=%d out=%v", c.name, code, out)
			if code != 400 {
				t.Fatalf("%s 应 400（不许把「填错了」放水），实际 %d（%v）", c.name, code, out)
			}
			if errText, _ := out["error"].(string); !strings.Contains(errText, c.want) {
				t.Fatalf("400 说明应含 %q，实际 %q", c.want, errText)
			}
			aa := askAlertGet(t, a)
			if got, _ := aa["onAppearSound"].(string); got != alertSoundCustom {
				t.Fatalf("保存失败不该改动档位，实际 %q", got)
			}
			if got, _ := aa["onAppearWav"].(string); got != "" {
				t.Fatalf("保存失败不该写入路径，实际 %q", got)
			}
		})
	}

	// 已经存了合法路径的 custom 档上填错：400 且原路径保留（沿用既有语义）
	wav := makeTempWav(t, "好了.wav")
	if code, body := askAlertPost(t, a, `{"askAlert":{"onAppearWav":"`+strings.ReplaceAll(wav, `\`, `\\`)+`"}}`); code != 200 {
		t.Fatalf("合法路径应能保存，实际 %d（%v）", code, body)
	}
	if code, out := askAlertPost(t, a, `{"askAlert":{"onAppearWav":"D:\\nope\\missing.wav"}}`); code != 400 {
		t.Fatalf("既已填了合法路径，改成不存在的路径应 400，实际 %d（%v）", code, out)
	}
	if got, _ := askAlertGet(t, a)["onAppearWav"].(string); got != wav {
		t.Fatalf("保存失败后原路径应保持 %q，实际 %q", wav, got)
	}
}

// d) custom + 合法 .wav → 保存成功，播放的就是这个文件（不走回落）。
func TestAlertSoundCustomValidPlaysFile(t *testing.T) {
	wav := makeTempWav(t, "我的提示音.wav")
	a := newWizardTestApp(t)
	code, body := askAlertPost(t, a, `{"askAlert":{"onAppearSound":"custom","onAppearWav":"`+strings.ReplaceAll(wav, `\`, `\\`)+`"}}`)
	if code != 200 {
		t.Fatalf("合法自定义路径应保存成功，实际 %d（%v）", code, body)
	}
	aa := askAlertGet(t, a)
	if aa["onAppearSound"] != alertSoundCustom || aa["onAppearWav"] != wav {
		t.Fatalf("回读应为 custom + %q，实际 %v", wav, aa)
	}

	spy := newSoundSpy(t)
	plan := a.Cfg.resolveAlertSound()
	if plan.invalid || plan.why != "" {
		t.Fatalf("合法路径不该有回落原因：%+v", plan)
	}
	played, why := playAlertSound(plan)
	wavs, beeps := spy.counts()
	t.Logf("合法路径：played=%s fallback=%q wav=%v beep=%d", played, why, wavs, beeps)
	if played != "wav:"+wav || len(wavs) != 1 || wavs[0] != wav || beeps != 0 {
		t.Fatalf("应只播用户那个文件，实际 played=%s wav=%v beep=%d", played, wavs, beeps)
	}
}

// e) 试听接口：路径为空给**可读提示**（200 + 明确说明走了系统音），
// 既不是 500、也不是「静默成功让人以为自定义生效了」；非法路径仍是 400 可读原因。
func TestAlertSoundPreviewReadableWhenUnset(t *testing.T) {
	spy := newSoundSpy(t)
	a := newWizardTestApp(t)
	preview := func(body string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleAlertSoundPreview(rec, httptest.NewRequest("POST", "/api/alert/sound", strings.NewReader(body)))
		out := map[string]any{}
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("试听响应不是 JSON: %v (%s)", err, rec.Body.String())
		}
		return rec.Code, out
	}

	code, out := preview(`{"sound":"custom","wav":""}`)
	t.Logf("空路径试听：%d %v", code, out)
	if code != 200 {
		t.Fatalf("路径还没填时试听不该报错（400/500 都会在设置页变成弹窗），实际 %d（%v）", code, out)
	}
	fallback, _ := out["fallback"].(string)
	if !strings.Contains(fallback, "未填写自定义 WAV 路径") {
		t.Fatalf("要把「还没填路径」当可读提示回给设置页，实际 fallback=%q（%v）", fallback, out)
	}
	if out["played"] != "beep" {
		t.Fatalf("未填路径时试听应回落系统提示音（played=beep），实际 %v", out["played"])
	}
	if _, beeps := spy.counts(); beeps != 1 {
		t.Fatalf("未填路径的试听该真的叮一声（让用户听见回落效果），实际 %d 声", beeps)
	}
	// 试听不写配置
	if a.Cfg.AskAlert.OnAppearSound != "" || a.Cfg.AskAlert.OnAppearWav != "" {
		t.Fatalf("试听不该写配置，实际 %+v", a.Cfg.AskAlert)
	}

	// 非法路径：400 + 可读原因（这条不放宽）
	code, out = preview(`{"sound":"custom","wav":"D:\\nope\\missing.wav"}`)
	if code != 400 {
		t.Fatalf("非法自定义路径试听应 400，实际 %d（%v）", code, out)
	}
	if errText, _ := out["error"].(string); !strings.Contains(errText, "WAV") {
		t.Fatalf("400 要给可读原因，实际 %q", errText)
	}
}

// f) 其余四档（notify/chimes/double/system）的保存语义与播放行为不变。
func TestAlertSoundOtherModesUnchanged(t *testing.T) {
	requireWindowsSounds(t)
	a := newWizardTestApp(t)
	cases := []struct {
		mode     string
		wantFile string // 空＝只走 MessageBeep
		wantBeep int
	}{
		{alertSoundNotify, "notify.wav", 0},
		{alertSoundChimes, "chimes.wav", 0},
		{alertSoundDouble, "notify.wav", 0},
		{alertSoundSystem, "", 1},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			if code, body := askAlertPost(t, a, `{"askAlert":{"onAppearSound":"`+c.mode+`"}}`); code != 200 {
				t.Fatalf("保存 %s 应成功，实际 %d（%v）", c.mode, code, body)
			}
			if got, _ := askAlertGet(t, a)["onAppearSound"].(string); got != c.mode {
				t.Fatalf("回读应为 %s，实际 %q", c.mode, got)
			}
			spy := newSoundSpy(t)
			plan := a.Cfg.resolveAlertSound()
			played, why := playAlertSound(plan)
			if c.mode == alertSoundDouble {
				// 双响首发：这里只断言第一声；第二声由 timer 补（异步），等它响完再走——
				// 这条 timer 漏到后面的用例里会让它们的 wav 计数多一声。
				wavs, _ := spy.counts()
				if played != "wav:"+windowsMediaWav("notify.wav") || len(wavs) != 1 {
					t.Fatalf("双响首发应为 notify.wav，实际 played=%s wav=%v", played, wavs)
				}
				time.Sleep(3 * alertSoundGap)
				return
			}
			wavs, beeps := spy.counts()
			if c.wantFile == "" {
				if played != "beep" || beeps != c.wantBeep || len(wavs) != 0 || why != "" {
					t.Fatalf("%s 档只该叮一声，实际 played=%s beep=%d wav=%v why=%q", c.mode, played, beeps, wavs, why)
				}
				return
			}
			if played != "wav:"+windowsMediaWav(c.wantFile) || len(wavs) != 1 || beeps != 0 {
				t.Fatalf("%s 档该播 %s，实际 played=%s wav=%v beep=%d", c.mode, c.wantFile, played, wavs, beeps)
			}
		})
	}

	// notify → custom(空) → notify 往返：档位与路径各归各位，没有残留
	if code, body := askAlertPost(t, a, `{"askAlert":{"onAppearSound":"custom"}}`); code != 200 {
		t.Fatalf("切到 custom 应成功，实际 %d（%v）", code, body)
	}
	if code, body := askAlertPost(t, a, `{"askAlert":{"onAppearSound":"notify"}}`); code != 200 {
		t.Fatalf("切回 notify 应成功，实际 %d（%v）", code, body)
	}
	aa := askAlertGet(t, a)
	if aa["onAppearSound"] != alertSoundNotify || aa["onAppearWav"] != "" {
		t.Fatalf("切回 notify 后应为 notify + 空路径，实际 %v", aa)
	}
}
