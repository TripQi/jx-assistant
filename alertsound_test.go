package main

// 提示音方案（设置·常规·询问提醒 →「提示音方案」）的行为验收：
//
//	a. 各档确实走对应实现：默认档播系统通知音 notify.wav、chimes 档播 chimes.wav、
//	   系统档只走 MessageBeep、双响档播两次且留间隔、自定义档用用户填的路径；
//	b. 文件不存在 / 播放失败 → 回落 MessageBeep，并产生一条说明原因**日志**（不静默）；
//	c. onAppear=false → 完全不播（连 wav 与系统叮都不碰）；
//	d. 同一张卡只响一次（既有的去重不被新的播放路径破坏）；
//	e. 设置项 POST/GET 往返正确，缺省值＝新默认档；自定义路径非法/未知方案 400 且
//	   不静默改成别的音。
//
// 播放走可注入函数（alertWavPlayer / alertSystemBeep）：这里全是计数桩，跑测试
// 不真发声、不依赖本机音频设备。

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// soundSpy 换掉两个播放原语并记录细节（播了哪些 wav、各在何时、叮了几声）。
type soundSpy struct {
	mu    sync.Mutex
	wavs  []string
	at    []time.Time
	beeps int
	wavOK bool // false = 桩每次播放都报错（模拟播放失败）
}

func newSoundSpy(t *testing.T) *soundSpy {
	t.Helper()
	s := &soundSpy{wavOK: true}
	oldWav, oldBeep, oldGap := alertWavPlayer, alertSystemBeep, alertSoundGap
	alertWavPlayer = func(path string) error {
		s.mu.Lock()
		defer s.mu.Unlock()
		if !s.wavOK {
			return os.ErrInvalid
		}
		s.wavs = append(s.wavs, path)
		s.at = append(s.at, time.Now())
		return nil
	}
	alertSystemBeep = func() { s.mu.Lock(); s.beeps++; s.mu.Unlock() }
	alertSoundGap = 20 * time.Millisecond // 双响不必真等 250ms
	t.Cleanup(func() { alertWavPlayer, alertSystemBeep, alertSoundGap = oldWav, oldBeep, oldGap })
	return s
}

func (s *soundSpy) counts() (wavs []string, beeps int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.wavs...), s.beeps
}

// requireWindowsSounds 本机必须有 Windows 自带的两条音效（Windows 一直自带）；
// 缺了就是机器的系统文件不全，直接说清楚，别让后面的断言含糊过去。
func requireWindowsSounds(t *testing.T) {
	t.Helper()
	for _, n := range []string{"notify.wav", "chimes.wav"} {
		if why := alertWavProblem(windowsMediaWav(n)); why != "" {
			t.Fatalf("本机缺系统音效 %s：%s（用例前提不成立）", windowsMediaWav(n), why)
		}
	}
}

// 场景 a-1/a-2/a-3：默认档、chimes 档、系统档
func TestAlertSoundModeRunsExpectedImpl(t *testing.T) {
	requireWindowsSounds(t)
	cases := []struct {
		mode     string
		wantFile string // 空＝只该走 MessageBeep
		wantBeep int
	}{
		{"", "notify.wav", 0}, // 空＝缺省，落默认档
		{"notify", "notify.wav", 0},
		{"chimes", "chimes.wav", 0},
		{"system", "", 1},
	}
	for _, c := range cases {
		t.Run("mode="+c.mode, func(t *testing.T) {
			spy := newSoundSpy(t)
			cfg := &Config{AskAlert: AskAlertCfg{OnAppearSound: c.mode}}
			plan := cfg.resolveAlertSound()
			played, why := playAlertSound(plan)
			wavs, beeps := spy.counts()
			t.Logf("方案 %q → mode=%s played=%s fallback=%q wav=%v beep=%d", c.mode, plan.mode, played, why, wavs, beeps)
			if c.wantFile == "" {
				if len(wavs) != 0 || beeps != c.wantBeep || played != "beep" {
					t.Fatalf("系统提示音档只该叮 %d 声、不碰 wav，实际 wav=%v beep=%d played=%s", c.wantBeep, wavs, beeps, played)
				}
				return
			}
			want := windowsMediaWav(c.wantFile)
			if len(wavs) != 1 || wavs[0] != want {
				t.Fatalf("该播 %s 一次，实际 %v", want, wavs)
			}
			if beeps != 0 {
				t.Fatalf("wav 播上了就不该再叮系统音，实际 %d 声", beeps)
			}
			if played != "wav:"+want {
				t.Fatalf("回执应为 wav:%s，实际 %s", want, played)
			}
		})
	}
}

// 场景 a-4：双响——同一个音效连响两次，两次之间有间隔（不叠成一坨）。
func TestAlertSoundDoublePlaysTwiceWithGap(t *testing.T) {
	requireWindowsSounds(t)
	spy := newSoundSpy(t)
	cfg := &Config{AskAlert: AskAlertCfg{OnAppearSound: alertSoundDouble}}
	played, why := playAlertSound(cfg.resolveAlertSound())
	if played != "wav:"+windowsMediaWav("notify.wav") || why != "" {
		t.Fatalf("双响档应播 notify.wav（played=%s fallback=%q）", played, why)
	}
	// 第 2 声由 timer 补（异步），等它响完再断言确切次数
	time.Sleep(150 * time.Millisecond)
	spy.mu.Lock()
	wavs, at := append([]string(nil), spy.wavs...), append([]time.Time(nil), spy.at...)
	spy.mu.Unlock()
	t.Logf("双响：%d 声 %v", len(wavs), wavs)
	if len(wavs) != 2 {
		t.Fatalf("双响该响两次，实际 %d 次", len(wavs))
	}
	if wavs[0] != wavs[1] || wavs[0] != windowsMediaWav("notify.wav") {
		t.Fatalf("双响两下应是同一条音效，实际 %v", wavs)
	}
	if d := at[1].Sub(at[0]); d < 15*time.Millisecond {
		t.Fatalf("两响之间应有间隔（约 %v），实际只隔了 %v——会叠成一坨噪音", alertSoundGap, d)
	}
}

// 场景 a-5：自定义档用用户填的路径（真的那个文件，不是系统音效）。
func TestAlertSoundCustomUsesUserWav(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "我的提示音.wav")
	if err := os.WriteFile(wav, []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatalf("造测试 wav 失败: %v", err)
	}
	spy := newSoundSpy(t)
	cfg := &Config{AskAlert: AskAlertCfg{OnAppearSound: alertSoundCustom, OnAppearWav: wav}}
	plan := cfg.resolveAlertSound()
	if plan.invalid || plan.why != "" {
		t.Fatalf("合法自定义路径不该被判非法: %+v", plan)
	}
	played, _ := playAlertSound(plan)
	wavs, beeps := spy.counts()
	t.Logf("自定义档：played=%s wav=%v beep=%d", played, wavs, beeps)
	if len(wavs) != 1 || wavs[0] != wav || beeps != 0 {
		t.Fatalf("自定义档该只播用户那个文件，实际 wav=%v beep=%d", wavs, beeps)
	}
}

// captureLog 收集这一段时间内的日志行（只取含 needle 的），用于断言「回落有话说」。
func captureLog(t *testing.T, fn func(), needle string) []string {
	t.Helper()
	var buf bytes.Buffer
	old := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(old)
	fn()
	var hits []string
	for _, line := range strings.Split(buf.String(), "\n") {
		if strings.Contains(line, needle) {
			hits = append(hits, line)
		}
	}
	return hits
}

// 场景 b-1：自定义文件不存在 → 回落 MessageBeep + 一条说明日志 + 标记「配置非法」
// （保存/试听会据此 400，不静默改成别的音）。
func TestAlertSoundMissingFileFallsBackWithLog(t *testing.T) {
	spy := newSoundSpy(t)
	missing := filepath.Join(t.TempDir(), "已经删掉的.wav")
	cfg := &Config{AskAlert: AskAlertCfg{OnAppearSound: alertSoundCustom, OnAppearWav: missing}}
	plan := cfg.resolveAlertSound()
	if !plan.invalid {
		t.Fatal("自定义文件不存在应标记为配置非法（保存/试听要明确报错）")
	}
	var played, why string
	hits := captureLog(t, func() {
		played, why = playAlertSound(plan)
	}, "提示音回落系统 MessageBeep")
	wavs, beeps := spy.counts()
	t.Logf("文件缺失：played=%s fallback=%q wav=%v beep=%d 日志=%v", played, why, wavs, beeps, hits)
	if played != "beep" || beeps != 1 || len(wavs) != 0 {
		t.Fatalf("文件缺失应回落叮 1 声、不碰 wav，实际 played=%s beep=%d wav=%v", played, beeps, wavs)
	}
	if len(hits) != 1 || !strings.Contains(hits[0], "已经删掉的.wav") {
		t.Fatalf("应记一条说明回落原因的日志（含路径），实际 %v", hits)
	}
}

// 场景 b-2：文件在但播放失败（音效被占用/格式坏）→ 同样回落并记日志。
func TestAlertSoundPlayFailureFallsBackWithLog(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "坏文件.wav")
	if err := os.WriteFile(wav, []byte("not a real wav"), 0o644); err != nil {
		t.Fatalf("造测试 wav 失败: %v", err)
	}
	spy := newSoundSpy(t)
	spy.wavOK = false // 播放原语失败
	cfg := &Config{AskAlert: AskAlertCfg{OnAppearSound: alertSoundCustom, OnAppearWav: wav}}
	var played, why string
	hits := captureLog(t, func() {
		played, why = playAlertSound(cfg.resolveAlertSound())
	}, "提示音回落系统 MessageBeep")
	_, beeps := spy.counts()
	t.Logf("播放失败：played=%s fallback=%q beep=%d 日志=%v", played, why, beeps, hits)
	if played != "beep" || beeps != 1 || len(hits) != 1 {
		t.Fatalf("播放失败应回落叮 1 声并记 1 条日志，实际 played=%s beep=%d 日志=%v", played, beeps, hits)
	}
}

// 场景 c：onAppear=false → 一张卡都不许响（wav 与系统叮都不碰）。
// 这里不换 askAlertSound 那道桩，走真实现（只把两个播放原语换成计数桩）。
func TestAlertSoundOffPlaysNothing(t *testing.T) {
	spy := newSoundSpy(t)
	off := false
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{AskAlert: AskAlertCfg{OnAppear: &off}}
	sess := createPlanSession(t, st, "s1", "会话一", false)

	id := openQuestion(t, a, sess, askKindAsk)
	t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") })
	settle()
	wavs, beeps := spy.counts()
	t.Logf("开关关闭：wav=%v beep=%d", wavs, beeps)
	if len(wavs) != 0 || beeps != 0 {
		t.Fatalf("关掉「卡片出现时提示音」后不该有任何声音，实际 wav=%v beep=%d", wavs, beeps)
	}
}

// 场景 d：同一张卡只响一次——重放/重连再来一遍也不补第二声（走真实现）。
func TestAlertSoundOncePerCard(t *testing.T) {
	requireWindowsSounds(t)
	spy := newSoundSpy(t)
	st := testPlanStore(t)
	a := newPlanTestApp(t, st)
	a.Cfg = &Config{} // 默认档（notify.wav 一次）
	sess := createPlanSession(t, st, "s1", "会话一", false)

	id := openQuestion(t, a, sess, askKindAsk)
	t.Cleanup(func() { _ = a.resolveQuestion(id, "甲") })
	settle()
	if wavs, _ := spy.counts(); len(wavs) != 1 {
		t.Fatalf("首次出现应响 1 次，实际 %d 次", len(wavs))
	}
	a.alertPendingCardOnAppear(id)
	a.alertPendingCardOnAppear(id)
	time.Sleep(150 * time.Millisecond) // 给可能的异步第二声留窗口
	wavs, beeps := spy.counts()
	t.Logf("同一张卡重放 2 次后：wav=%v beep=%d", wavs, beeps)
	if len(wavs) != 1 || beeps != 0 {
		t.Fatalf("同一张卡只该响一次，实际 wav=%v beep=%d", wavs, beeps)
	}
}

// 场景 e：设置项往返 + 缺省值 + 非法输入明确报错（不静默改音）。
func TestAlertSoundConfigRoundTripAndValidation(t *testing.T) {
	dir := t.TempDir()
	wav := filepath.Join(dir, "提示.wav")
	if err := os.WriteFile(wav, []byte("RIFF....WAVE"), 0o644); err != nil {
		t.Fatalf("造测试 wav 失败: %v", err)
	}
	post := func(a *App, body string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleSetConfig(rec, httptest.NewRequest("POST", "/api/config", strings.NewReader(body)))
		var out struct {
			AskAlert map[string]any `json:"askAlert"`
			Error    string         `json:"error"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, map[string]any{"askAlert": out.AskAlert, "error": out.Error}
	}
	askAlertOf := func(a *App) map[string]any {
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

	// 缺省（新装/没写过这些键）＝新默认档 notify，久等提醒的旧键语义不变
	a := newWizardTestApp(t)
	aa := askAlertOf(a)
	if got, _ := aa["onAppearSound"].(string); got != alertSoundNotify {
		t.Fatalf("缺省音效方案应为 %q，实际 %q", alertSoundNotify, got)
	}
	if got, _ := aa["onAppear"].(bool); !got {
		t.Fatal("缺省的「卡片出现时提示音」应仍为开（旧语义一字不改）")
	}
	if got, _ := aa["afterSec"].(float64); got != 240 {
		t.Fatalf("缺省触发时长应仍为 240，实际 %v", got)
	}

	// 存「双响」→ 读回双响
	code, body := post(a, `{"askAlert":{"onAppearSound":"double"}}`)
	if code != 200 {
		t.Fatalf("保存 double 应成功，实际 %d（%v）", code, body)
	}
	if got := askAlertOf(a)["onAppearSound"]; got != alertSoundDouble {
		t.Fatalf("往返后应为 double，实际 %v", got)
	}

	// 存自定义（合法路径）→ 读回 custom + 路径
	if code, body := post(a, `{"askAlert":{"onAppearSound":"custom","onAppearWav":"`+strings.ReplaceAll(wav, `\`, `\\`)+`"}}`); code != 200 {
		t.Fatalf("合法自定义路径应能保存，实际 %d（%v）", code, body)
	}
	aa = askAlertOf(a)
	if aa["onAppearSound"] != alertSoundCustom || aa["onAppearWav"] != wav {
		t.Fatalf("往返后应为 custom + 路径 %q，实际 %v", wav, aa)
	}

	// 存一个不存在的自定义路径 → 400，且原值不被改掉（不静默换成别的音）
	code, body = post(a, `{"askAlert":{"onAppearWav":"D:\\nope\\missing.wav"}}`)
	if code != 400 {
		t.Fatalf("自定义路径不存在应 400，实际 %d（%v）", code, body)
	}
	if errText, _ := body["error"].(string); !strings.Contains(errText, "WAV") {
		t.Fatalf("400 的说明应指明 WAV 路径问题，实际 %q", errText)
	}
	if got := askAlertOf(a)["onAppearWav"]; got != wav {
		t.Fatalf("保存失败后原路径应保持 %q，实际 %v", wav, got)
	}

	// 未知方案 → 400（不静默回落到某个档）
	code, body = post(a, `{"askAlert":{"onAppearSound":"极大声"}}`)
	if code != 400 {
		t.Fatalf("未知方案应 400，实际 %d（%v）", code, body)
	}
	if errText, _ := body["error"].(string); !strings.Contains(errText, "提示音方案") {
		t.Fatalf("400 的说明应指明提示音方案问题，实际 %q", errText)
	}
	if got := askAlertOf(a)["onAppearSound"]; got != alertSoundCustom {
		t.Fatalf("非法保存不该改动原方案，实际 %v", got)
	}
}

// 试听接口：按请求里的方案真播一次（不动已保存配置）；自定义路径非法给 400。
func TestAlertSoundPreviewEndpoint(t *testing.T) {
	spy := newSoundSpy(t)
	a := newWizardTestApp(t)
	preview := func(body string) (int, map[string]any) {
		t.Helper()
		rec := httptest.NewRecorder()
		a.handleAlertSoundPreview(rec, httptest.NewRequest("POST", "/api/alert/sound", strings.NewReader(body)))
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}

	// 系统档试听：只叮一声，不碰 wav，也不写配置
	code, out := preview(`{"sound":"system"}`)
	if code != 200 || out["played"] != "beep" {
		t.Fatalf("试听系统档应 200 且 played=beep，实际 %d %v", code, out)
	}
	if a.Cfg.AskAlert.OnAppearSound != "" {
		t.Fatalf("试听不该写配置，实际被写成 %q", a.Cfg.AskAlert.OnAppearSound)
	}

	// 自定义路径不可用：400 + 原因（不静默播别的音）
	code, out = preview(`{"sound":"custom","wav":"D:\\nope\\missing.wav"}`)
	if code != 400 {
		t.Fatalf("自定义路径不可用应 400，实际 %d %v", code, out)
	}
	if _, beeps := spy.counts(); beeps != 1 {
		t.Fatalf("非法试听不该发声（beeps 应仍是 1），实际 %d", beeps)
	}

	// 未知方案：400
	if code, out = preview(`{"sound":"nope"}`); code != 400 {
		t.Fatalf("未知方案试听应 400，实际 %d %v", code, out)
	}
}
