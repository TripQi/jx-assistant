package main

// 「询问提醒」的提示音：卡片刚出现（pendingalert.go）与久等提醒到点
// （interact.go 的 fireAskAlert）共用同一档音效设置（设置·常规·询问提醒 →
// 「提示音方案」，config.json 的 askAlert.onAppearSound / onAppearWav）。
//
// 为什么要有它：原来那条路上只有一句 user32 MessageBeep（系统默认「叮」），
// 又短又轻，程序放后台基本听不出来——用户的原话是「太短太弱」。现在默认改播
// Windows 自己的通知音 notify.wav（明显得多），同时保留「系统提示音」这一档给
// 想回到原来安静效果的人；双响 / 自定义 WAV 是再往上加的两档。
//
// 两条硬约束：
//   - **播放一律异步**：winmm PlaySound 带 SND_ASYNC 立即返回，双响的第二声用
//     timer 补——绝不在提问/审批的请求路径上 sleep（那会把一次弹卡拖成几百毫秒）。
//   - **回落不静默**：首选音效文件缺失或播放失败 → 回落系统 MessageBeep，并写
//     一条日志说明原因（「为什么不响/为什么变小声了」在日志里要有话）。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// 可选方案（存进 config.json 的 askAlert.onAppearSound）。
const (
	alertSoundNotify = "notify" // 默认：Windows 通知音 notify.wav（比系统叮明显）
	alertSoundChimes = "chimes" // 更醒目：chimes.wav
	alertSoundDouble = "double" // 默认通知音连响两次（间隔 250ms，不阻塞）
	alertSoundSystem = "system" // 安静档：回到原来的单声 MessageBeep
	alertSoundCustom = "custom" // 用户自己给的 .wav 绝对路径
)

// alertSoundModes 全部可选方案（保存校验与文档共用这一份口径；顺序即设置页
// 推荐的先后：默认在前，安静档在后）。
var alertSoundModes = []string{alertSoundNotify, alertSoundChimes, alertSoundDouble, alertSoundSystem, alertSoundCustom}

// alertSoundGap 双响的间隔（变量是为了测试里改小，不必真等 250ms）。
var alertSoundGap = 250 * time.Millisecond

// alertWavPlayer 平台实现：试着异步播一个 wav，返回错误＝没播上（调用方回落）。
// 换桩即可在测试里静音运行（不真发声、不依赖本机音频设备）。
var alertWavPlayer = playWavAsync

// alertSystemBeep 平台实现：MessageBeep 一声系统提示音（回落档／安静档）。
var alertSystemBeep = systemBeep

// 设置页「提示音方案」下拉的选项文案（后端下发，前端不另抄一份——档位增删只
// 改这里；值与设置页保存的枚举一致）。
func alertSoundOptions() []map[string]string {
	return []map[string]string{
		{"v": alertSoundNotify, "label": "Windows 通知音（默认，推荐）"},
		{"v": alertSoundChimes, "label": "更醒目的提示音（chimes）"},
		{"v": alertSoundDouble, "label": "通知音连响两次（双响）"},
		{"v": alertSoundSystem, "label": "系统提示音（原来那声叮，安静）"},
		{"v": alertSoundCustom, "label": "自定义 WAV 文件"},
	}
}

// canonicalAlertSound 把配置值规整成合法方案：空串/未知值一律落默认档 notify。
func canonicalAlertSound(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return alertSoundNotify
	}
	for _, m := range alertSoundModes {
		if s == m {
			return s
		}
	}
	return alertSoundNotify
}

// validAlertSound 是否是我们认识的方案名（保存时用来拒绝未知值——不静默改档）。
// 空串合法：表示回到默认档。
func validAlertSound(v string) bool {
	s := strings.ToLower(strings.TrimSpace(v))
	if s == "" {
		return true
	}
	for _, m := range alertSoundModes {
		if s == m {
			return true
		}
	}
	return false
}

// windowsMediaWav Windows 自带音效的绝对路径（Media 目录跟着 SystemRoot 走，
// 系统装在非 C 盘也找得到；SystemRoot 缺失时退回经典路径）。
func windowsMediaWav(name string) string {
	root := strings.TrimSpace(os.Getenv("SystemRoot"))
	if root == "" {
		root = `C:\Windows`
	}
	return filepath.Join(root, "Media", name)
}

// alertWavProblem 这个 wav 现在能不能播：返回空串＝没问题，否则是给人看的原因。
// 保存校验、试听报错、播放回落日志三处说同一句话。
func alertWavProblem(path string) string {
	p := strings.TrimSpace(path)
	if p == "" {
		return "没有填 WAV 文件路径"
	}
	if !filepath.IsAbs(p) {
		return "路径必须是绝对路径（如 D:\\声音\\提示.wav）"
	}
	if !strings.EqualFold(filepath.Ext(p), ".wav") {
		return "只支持 .wav 文件（Windows 提示音接口只认 wav）"
	}
	st, err := os.Stat(p)
	if err != nil {
		return "文件打不开（" + err.Error() + "）"
	}
	if st.IsDir() {
		return "这是个文件夹，不是 .wav 文件"
	}
	return ""
}

// playWavAsync 先确认文件在（PlaySound 对缺失文件是静默返回成功，从返回值看不
// 出差别），再交给平台异步播放。
func playWavAsync(path string) error {
	if why := alertWavProblem(path); why != "" {
		return errors.New(why)
	}
	return wavPlay(path) // 平台：winmm PlaySound（异步）/ 非 Windows 报不支持
}

// alertSoundPlan 一次提示音的播放计划（resolveAlertSound 的产物）。
type alertSoundPlan struct {
	mode    string        // 生效方案（日志与试听回执用）
	wav     string        // 要播的 wav 绝对路径（空＝直接走 MessageBeep）
	times   int           // 播放次数（双响＝2）
	gap     time.Duration // 多次播放之间的间隔
	why     string        // 非空＝必须回落 MessageBeep 的原因（写日志，不静默）
	invalid bool          // 配置本身非法（自定义路径不可用）→ 保存/试听要明确报错
}

// resolveAlertSound 把设置解析成播放计划（不播放、不动真实设备）。
func (c *Config) resolveAlertSound() alertSoundPlan {
	mode := c.askAlertSoundMode()
	switch mode {
	case alertSoundSystem: // 安静档：本来就只叮一声
		return alertSoundPlan{mode: mode, times: 1}
	case alertSoundCustom:
		p := alertSoundPlan{mode: mode, wav: c.askAlertWav(), times: 1, gap: alertSoundGap}
		if why := alertWavProblem(p.wav); why != "" {
			p.why, p.invalid = "自定义 WAV 不可用："+why, true
		}
		return p
	case alertSoundChimes:
		return systemAlertSoundPlan(mode, "chimes.wav", 1)
	case alertSoundDouble:
		return systemAlertSoundPlan(mode, "notify.wav", 2)
	default: // alertSoundNotify（含未知值归一到默认档）
		return systemAlertSoundPlan(alertSoundNotify, "notify.wav", 1)
	}
}

// systemAlertSoundPlan 系统音效那一类方案的计划（文件缺失只回落、不算配置非法：
// 那是环境问题，用户没填错东西，保存不该被拒）。
func systemAlertSoundPlan(mode, file string, times int) alertSoundPlan {
	p := alertSoundPlan{mode: mode, wav: windowsMediaWav(file), times: times, gap: alertSoundGap}
	if why := alertWavProblem(p.wav); why != "" {
		p.why = "系统音效文件不可用：" + why
	}
	return p
}

// playAlertSound 执行一份播放计划，异步返回实际走的方式：
//   - 有 wav 就交给平台异步播放器（不等播完），双响的第二声用 timer 补；
//   - 文件缺失/播放失败/配置非法 → 回落 MessageBeep，并记一条日志说明原因。
//
// 返回值：played＝"wav:<路径>" 或 "beep"；fallbackWhy＝回落原因（试听接口回给
// 设置页，正式提醒时只进日志）。
func playAlertSound(plan alertSoundPlan) (played, fallbackWhy string) {
	times, gap := plan.times, plan.gap
	if times < 1 {
		times = 1
	}
	if gap <= 0 {
		gap = alertSoundGap
	}
	if plan.why != "" {
		fallbackWhy = plan.why
	} else if plan.wav != "" {
		if err := alertWavPlayer(plan.wav); err != nil {
			fallbackWhy = err.Error()
		}
	}
	if fallbackWhy != "" {
		log.Printf("[提醒] 提示音回落系统 MessageBeep：%s（方案=%s，wav=%s）", fallbackWhy, plan.mode, plan.wav)
		beepNTimes(times, gap)
		return "beep", fallbackWhy
	}
	if plan.wav == "" { // system 档
		beepNTimes(times, gap)
		return "beep", ""
	}
	// 双响：第二声到点再补（time.AfterFunc，绝不在请求路径上 sleep）。新的
	// PlaySound 调用会顶掉上一声——两下「叮」正是这一档想要的效果。
	for i := 1; i < times; i++ {
		path, n := plan.wav, i
		time.AfterFunc(gap*time.Duration(n), func() {
			if err := alertWavPlayer(path); err != nil {
				log.Printf("[提醒] 提示音第 %d 声没播上，改用系统 MessageBeep：%v（wav=%s）", n+1, err, path)
				alertSystemBeep()
			}
		})
	}
	return "wav:" + plan.wav, ""
}

// beepNTimes 连叮 times 声系统提示音：第 2 声起用 timer 补，不阻塞调用方。
// 回落到系统叮时沿用方案的次数——双响档回落了也还是两下，别让用户以为白设了。
func beepNTimes(times int, gap time.Duration) {
	if times < 1 {
		times = 1
	}
	if gap <= 0 {
		gap = alertSoundGap
	}
	alertSystemBeep()
	for i := 1; i < times; i++ {
		time.AfterFunc(gap*time.Duration(i), alertSystemBeep)
	}
}

// handleAlertSoundPreview 设置页「试听」：按请求里的方案真播一次提示音。
//
//	POST /api/alert/sound {"sound":"notify|chimes|double|system|custom","wav":"D:\\x.wav"}
//
// 不读也不写配置：用户还没点保存、或正想换一档听听效果，都该能试。自定义路径
// 不可用一律 400 并把原因原样给设置页（不静默换成别的音）。返回 played（实际播
// 的方式）与 fallback（回落原因，设置页把它显示在路径框下方）。
func (a *App) handleAlertSoundPreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Sound string `json:"sound"`
		Wav   string `json:"wav"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	if !validAlertSound(body.Sound) {
		writeErr(w, 400, "未知的提示音方案："+strings.TrimSpace(body.Sound))
		return
	}
	// 用一份临时配置解析：只影响这一次试听，不碰用户已保存的设置
	preview := &Config{AskAlert: AskAlertCfg{OnAppearSound: body.Sound, OnAppearWav: strings.TrimSpace(body.Wav)}}
	plan := preview.resolveAlertSound()
	if plan.invalid {
		writeErr(w, 400, plan.why)
		return
	}
	played, fallback := playAlertSound(plan)
	writeJSON(w, 200, map[string]any{"played": played, "fallback": fallback, "mode": plan.mode})
}

// saveAlertSound 保存提示音方案（handleSetConfig 的 askAlert 分支调）：未知方案
// 与自定义路径不可用一律返回 error（HTTP 层转 400 原样告诉用户），不做「静默改成
// 别的音」——用户设了自定义却听到系统叮，比报错更让人困惑。
func saveAlertSound(c *Config, haveMode bool, mode string, haveWav bool, wav string) error {
	nextMode := c.askAlertSoundMode()
	if haveMode {
		if !validAlertSound(mode) {
			return fmt.Errorf("未知的提示音方案：%s（可选 %s）", strings.TrimSpace(mode), strings.Join(alertSoundModes, " / "))
		}
		nextMode = canonicalAlertSound(mode)
	}
	nextWav := c.askAlertWav()
	if haveWav {
		nextWav = strings.TrimSpace(wav)
	}
	// custom 档必须当场校验：路径非法就不许保存（试听同样会被拒，原因一致）
	if nextMode == alertSoundCustom {
		if why := alertWavProblem(nextWav); why != "" {
			return fmt.Errorf("自定义 WAV 路径不可用：%s", why)
		}
	}
	if haveMode {
		c.AskAlert.OnAppearSound = nextMode
	}
	if haveWav {
		c.AskAlert.OnAppearWav = nextWav
	}
	return nil
}
