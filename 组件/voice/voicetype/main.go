//go:build windows

package main

// VoiceTypeGo —— 独立语音输入法（Go + WebView2，零主程序依赖）。
//
// 链路：热键按住（默认 Alt，hotkeys.json 可配）→ waveIn 采集 + WebView2 胶囊波形
//       → 松开/点击胶囊 → 自带 python X-ASR 本地识别（asr_server.py，随本目录分发）
//       → 识别完成、注入前一刻检测前台可编辑（caret/类名/UIA）
//       → 可编辑=剪贴板+Ctrl+V 注入，不可编辑=仅进剪贴板。
//       历史保留最近 3 条（原始语音+文字+结果，失败的也在，可重识别）。
//
// 与主程序组件版的差异（独立化要点）：
//   · ASR 不走主程序组件桥——启动时自拉 python asr_server.py（127.0.0.1:8320），
//     multipart WAV → {"text":...}，与旧 Electron 独立版同一套已验证服务
//   · 无 stdin 组件协议、无主程序端口依赖、无前台分流（任何窗口一律响应，
//     包括数字助理自己的窗口——独立版阶段先求全局一律可用）
//
// 用法：双击「启动输入法.bat」（或直接运行 voicetype_go.exe）；
//       退出：任务管理器结束 voicetype_go.exe（taskkill /f /im voicetype_go.exe）。
import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jchv/go-webview2"
	"syscall"
)

const (
	vtHistoryKeep = 3 // 最近语音记录条数（设置页展示用）：原始语音+文字+结果
	asrHost       = "127.0.0.1"
	asrPort       = 8320
)

var (
	vtDir       string
	vtWV        webview2.WebView
	vtPageReady atomic.Bool // overlay.html 加载完成，此后 Eval 才生效
	vtStopClick = make(chan struct{}, 1)
	vtStateMu   sync.Mutex
	vtRecording bool
	vtCapCur    *vtCapture
	vtStartAt   time.Time
)

func vtLog(format string, a ...any) { log.Printf("[vtgo] "+format, a...) }

// ---------- ASR：自带 python 服务（asr_server.py，X-ASR + silero VAD） ----------

var (
	asrProc  *exec.Cmd
	asrReady atomic.Bool
)

// findPython 优先本目录 runtime，再 exe 上三级的共享运行时，最后历史候选与 PATH。
func findPython() string {
	exe, err := os.Executable()
	if err != nil {
		vtLog("无法定位输入法 exe：%v", err)
		return ""
	}
	return findPythonAt(filepath.Dir(exe))
}

// findPythonAt 按 exe 落位自举；不让启动者的工作目录或 PATH 抢走共享运行时。
func findPythonAt(exeDir string) string {
	cands := []string{
		filepath.Join(exeDir, "runtime", "python", "python.exe"),
		filepath.Clean(filepath.Join(exeDir, "..", "..", "..", "技能", "_runtime", "python", "python.exe")),
		`D:\123\坚祥数字助理GO\技能\_runtime\python\python.exe`,
		`D:\Desktop\童浩\智能体工作目录\坚祥数字助理GO\技能\_runtime\python\python.exe`,
	}
	for _, p := range cands {
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p
		}
	}
	if p, err := exec.LookPath("python"); err == nil {
		return p
	}
	vtLog("python 探测失败（尝试过：%v + PATH），识别服务起不来——检查共享运行时 技能\\_runtime\\python 是否存在", cands)
	return ""
}

// vtAsrWatch 识别服务保活：每 5 秒探一次 /health，连续不可用就重新拉起。
//
// 为什么必须有它：识别服务是**可能变成孤儿的独立 python**——输入法被 taskkill、
// 或服务自己崩掉时，进程不会被一起收走/重启；此前只有启动时拉一次，于是"哪天
// python 没了，说话就永远识别失败，只能重启输入法"。这里做自愈：
//
//	· 判定用"连续 2 次失败"（单次抖动/正在加载模型时不误判）
//	· 重启有 20 秒冷却，避免端口被僵死进程占住时反复拉、日志刷屏
//	· 只在状态变化时打日志（可用↔不可用），不刷 vt.log
func vtAsrWatch() {
	fails := 0
	var lastDown bool
	var lastTry time.Time
	for {
		time.Sleep(5 * time.Second)
		healthy := false
		if resp, err := http.Get(fmt.Sprintf("http://%s:%d/health", asrHost, asrPort)); err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
			healthy = resp.StatusCode == 200
		}
		if healthy {
			if lastDown {
				vtLog("识别服务恢复可用")
				lastDown = false
			}
			fails = 0
			continue
		}
		fails++
		if fails < 2 {
			continue
		}
		if !lastDown {
			vtLog("识别服务不可用（连续 %d 次探测失败），准备重新拉起", fails)
			lastDown = true
		}
		if time.Since(lastTry) < 20*time.Second {
			continue
		}
		lastTry = time.Now()
		asrReady.Store(false)
		startAsr()
	}
}

// startAsr 拉起识别服务并等它就绪（模型加载十几秒，健康检查轮询；不就绪也
// 不拦启动——悬浮窗/录音可用，识别时 vtTranscribe 再等/报错）。
func startAsr() {
	// 端口上已经有服务（多半是上次没退干净留下的孤儿 python：主程序被 taskkill
	// 时子进程不会被连带杀掉）→ 直接复用，别再拉一个。两头 python 各自加载一份
	// 模型要 500MB 上下，而且新拉的那个绑不上端口会自己退出，日志里看着像"就绪"，
	// 其实用的是旧进程——排查时极易误判。
	if resp, err := http.Get(fmt.Sprintf("http://%s:%d/health", asrHost, asrPort)); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode == 200 {
			asrReady.Store(true)
			vtLog("端口 %d 上已有识别服务在跑，直接复用（不再拉起新的）", asrPort)
			return
		}
	}
	py := findPython()
	if py == "" {
		vtLog("未找到 python 解释器：识别不可用（放一份到 runtime\\python\\ 或装好数字助理后重试）")
		return
	}
	logPath := filepath.Join(vtDir, "asr.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		lf = nil
	}
	asrProc = exec.Command(py, filepath.Join(vtDir, "asr_server.py"))
	asrProc.Dir = vtDir
	// 别弹黑窗口：识别服务现在属于"组件的一部分"，用户不该看到一个 Python 控制台。
	// CREATE_NO_WINDOW(0x08000000)：不创建控制台（python.exe 是控制台程序，不指定就会
	// 凭空冒出一个黑框）；HideWindow 双保险。输出已重定向到 asr.log，不需要控制台。
	asrProc.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000}
	if lf != nil {
		asrProc.Stdout = lf
		asrProc.Stderr = lf
	}
	if err := asrProc.Start(); err != nil {
		vtLog("拉起 ASR 服务失败: %v", err)
		return
	}
	vtLog("ASR 服务拉起（python=%s pid=%d），等模型加载…", py, asrProc.Process.Pid)
	go func() {
		deadline := time.Now().Add(3 * time.Minute)
		for time.Now().Before(deadline) {
			if asrProc.ProcessState != nil {
				vtLog("ASR 服务进程退出: %v", asrProc.ProcessState)
				return
			}
			resp, err := http.Get(fmt.Sprintf("http://%s:%d/health", asrHost, asrPort))
			if err == nil {
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				if resp.StatusCode == 200 {
					asrReady.Store(true)
					vtLog("ASR 服务就绪 http://%s:%d", asrHost, asrPort)
					return
				}
			}
			time.Sleep(2 * time.Second)
		}
		vtLog("ASR 服务 3 分钟未就绪（看 asr.log 排查）")
	}()
}

// vtTranscribe 16k PCM → WAV → 自带服务 /v1/audio/transcriptions → 文本。
// deadline 是本轮识别的总时限（含等服务就绪与 HTTP 重试），到点必须返回——
// 保证调用方 UI 永远有结果（成功/失败/超时），不再出现"识别中"无期限悬挂。
func vtTranscribe(pcm []byte, deadline time.Time) (string, error) {
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()
	for !asrReady.Load() {
		select {
		case <-ctx.Done():
			return "", fmt.Errorf("%w（等待识别服务就绪）", vtErrTimeout)
		case <-time.After(50 * time.Millisecond):
		}
	}
	wav := vtPcmToWav(pcm, vtSampleRate)
	boundary := fmt.Sprintf("----vtgo%d%d", time.Now().UnixMilli(), rand.Int31())
	head := []byte("--" + boundary + "\r\n" +
		"Content-Disposition: form-data; name=\"file\"; filename=\"audio.wav\"\r\n" +
		"Content-Type: audio/wav\r\n\r\n")
	tail := []byte("\r\n--" + boundary + "--\r\n")
	body := append(append(append([]byte{}, head...), wav...), tail...)
	req, _ := http.NewRequest("POST",
		fmt.Sprintf("http://%s:%d/v1/audio/transcriptions", asrHost, asrPort),
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
	req.Header.Set("Content-Length", strconv.Itoa(len(body)))
	req = req.WithContext(ctx)
	// 服务端偶尔 5xx（历史坑：VAD 实例被并发共用会崩）时重试一次：
	// 一次识别失败就要用户重说一遍，代价远大于多等 300ms。
	for attempt := 0; ; attempt++ {
		// 单次请求的超时 = 剩余 deadline（不设别的封顶）：超长语音的推理本身
		// 可能超过两分钟（慢 CPU 上 RTF≈0.5，300s 语音要 ~150s），再叠一层
		// 120s 封顶会把长语音提前误杀；总时限由 vtAsrTimeoutSec 线性给出，
		// 这里只负责守死它。
		remain := time.Until(deadline)
		if remain <= 0 {
			return "", fmt.Errorf("%w（等待服务/重试耗尽 %.0fs）", vtErrTimeout, remain.Seconds()*-1)
		}
		client := &http.Client{Timeout: remain}
		resp, err := client.Do(req)
		if err != nil {
			if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) || time.Until(deadline) <= 0 {
				return "", fmt.Errorf("%w（HTTP %.0fs 无应答）", vtErrTimeout, remain.Seconds())
			}
			return "", err
		}
		data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if err != nil {
			if ctx.Err() != nil || os.IsTimeout(err) {
				return "", fmt.Errorf("%w（读取识别应答）", vtErrTimeout)
			}
			return "", err
		}
		if !time.Now().Before(deadline) {
			return "", vtErrTimeout
		}
		if resp.StatusCode >= 500 && attempt == 0 && time.Until(deadline) > 2*time.Second {
			vtLog("识别服务 %d，300ms 后重试一次：%s", resp.StatusCode, strings.TrimSpace(string(data)))
			select {
			case <-ctx.Done():
				return "", vtErrTimeout
			case <-time.After(300 * time.Millisecond):
			}
			req, _ = http.NewRequest("POST", fmt.Sprintf("http://%s:%d/v1/audio/transcriptions", asrHost, asrPort),
				bytes.NewReader(body))
			req.Header.Set("Content-Type", "multipart/form-data; boundary="+boundary)
			req.Header.Set("Content-Length", strconv.Itoa(len(body)))
			req = req.WithContext(ctx)
			continue
		}
		if resp.StatusCode != 200 {
			return "", fmt.Errorf("ASR HTTP %d: %s", resp.StatusCode, string(data))
		}
		var out struct {
			Text  string `json:"text"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(data, &out); err != nil {
			return "", fmt.Errorf("应答非 JSON: %s", string(data))
		}
		if out.Error != nil {
			return "", fmt.Errorf("识别失败: %s", out.Error.Message)
		}
		return out.Text, nil
	}
}

// vtTranscribeWithTimeout 带总超时的识别入口：到点必返回（成功/失败/超时三态），
// UI 永远收得到结果。内部 vtTranscribe 已按 deadline 收口，这里的 select 是
// 双保险（防内部任何一处意外长等）；迟到的结果进缓冲通道后直接被丢弃，
// 不会在超时提示之后突然注入文字吓到用户。
func vtTranscribeWithTimeout(pcm []byte, timeoutSec int) (string, error) {
	return vtTranscribeUntil(pcm, time.Now().Add(time.Duration(timeoutSec)*time.Second), vtTranscribe)
}

// vtTranscribeUntil 的工作函数只返回数据，不能注入；超时后缓冲通道中的迟到结果直接丢弃。
// 注入 deadline/工作函数供毫秒级回归验证，不改产品预算。
func vtTranscribeUntil(pcm []byte, deadline time.Time, transcribe func([]byte, time.Time) (string, error)) (string, error) {
	if !time.Now().Before(deadline) {
		return "", vtErrTimeout
	}
	type asrRes struct {
		text string
		err  error
	}
	ch := make(chan asrRes, 1)
	go func() {
		t, e := transcribe(pcm, deadline)
		ch <- asrRes{t, e}
	}()
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case r := <-ch:
		if !time.Now().Before(deadline) {
			return "", vtErrTimeout
		}
		return r.text, r.err
	case <-timer.C:
		return "", vtErrTimeout
	}
}

// vtPcmToWav 16k/mono/16bit PCM → WAV 容器。
func vtPcmToWav(pcm []byte, rate int) []byte {
	hdr := make([]byte, 44)
	copy(hdr[0:], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:], uint32(36+len(pcm)))
	copy(hdr[8:], "WAVE")
	copy(hdr[12:], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:], 16)
	binary.LittleEndian.PutUint16(hdr[20:], 1)
	binary.LittleEndian.PutUint16(hdr[22:], 1)
	binary.LittleEndian.PutUint32(hdr[24:], uint32(rate))
	binary.LittleEndian.PutUint32(hdr[28:], uint32(rate*2))
	binary.LittleEndian.PutUint16(hdr[32:], 2)
	binary.LittleEndian.PutUint16(hdr[34:], 16)
	copy(hdr[36:], "data")
	binary.LittleEndian.PutUint32(hdr[40:], uint32(len(pcm)))
	return append(hdr, pcm...)
}

// ---------- 历史：滚动保留最近 vtHistoryKeep 条（原始语音 + 文字 + 结果） ----------
//
// 为什么连失败的轮次也要留：识别失败/没听清的音频是"重新识别"的唯一原料，
// 只复制/注入失败的文字是"手动再粘贴"的兜底——记录在，东西就没丢过。
// status：injected=已输入；copied=已复制；asr_failed=识别失败；empty=没听清。

type vtHistoryItem struct {
	Time   string `json:"time"`            // 2026-11-05 12:30:45
	Text   string `json:"text"`            // 识别文字（失败为空）
	Status string `json:"status"`          // injected/copied/asr_failed/empty
	Audio  string `json:"audio,omitempty"` // wav 相对路径（history/xxx.wav）
}

func vtReadHistory() []vtHistoryItem {
	var arr []vtHistoryItem
	if raw, err := os.ReadFile(filepath.Join(vtDir, "history.json")); err == nil {
		json.Unmarshal(raw, &arr)
	}
	return arr
}

// vtSaveHistory 落一条记录：pcm 存成 wav（nil 则不带音频），滚动裁掉最老的
// 并删除其音频文件。旧格式（text/injected 字段）读进来直接被新结构替换。
func vtSaveHistory(pcm []byte, text, status string) {
	arr := vtReadHistory()
	item := vtHistoryItem{Time: time.Now().Format("2006-01-02 15:04:05"), Text: text, Status: status}
	if len(pcm) > 0 {
		os.MkdirAll(filepath.Join(vtDir, "history"), 0755)
		name := "history/" + time.Now().Format("20060102-150405.000") + ".wav"
		if err := os.WriteFile(filepath.Join(vtDir, filepath.FromSlash(name)), vtPCMToWav(pcm), 0644); err != nil {
			vtLog("历史音频写入失败: %v", err)
		} else {
			item.Audio = name
		}
	}
	arr = append(arr, item)
	if len(arr) > vtHistoryKeep {
		for _, old := range arr[:len(arr)-vtHistoryKeep] {
			if old.Audio != "" {
				_ = os.Remove(filepath.Join(vtDir, filepath.FromSlash(old.Audio)))
			}
		}
		arr = arr[len(arr)-vtHistoryKeep:]
	}
	os.WriteFile(filepath.Join(vtDir, "history.json"), mustJSON(arr), 0644)
}

// vtPCMToWav 16kHz/16bit/单声道 PCM 加 44 字节 WAV 头（历史音频自给自足，
// 播放器与识别服务都吃标准 WAV）。
func vtPCMToWav(pcm []byte) []byte {
	const hdr = 44
	w := make([]byte, hdr+len(pcm))
	dataLen := len(pcm)
	copy(w[0:], "RIFF")
	le32(w[4:], uint32(hdr+dataLen-8))
	copy(w[8:], "WAVE")
	copy(w[12:], "fmt ")
	le32(w[16:], 16)          // fmt chunk 长度
	le16(w[20:], 1)           // PCM
	le16(w[22:], 1)           // 单声道
	le32(w[24:], vtSampleRate)
	le32(w[28:], vtSampleRate*2) // 字节率 = 采样率×声道×位深/8
	le16(w[32:], 2)           // 块对齐
	le16(w[34:], 16)          // 位深
	copy(w[36:], "data")
	le32(w[40:], uint32(dataLen))
	copy(w[hdr:], pcm)
	return w
}

// vtWavToPCM 剥掉 WAV 头取 PCM（找 data 块；非 WAV 原样返回，容错 reid）。
func vtWavToPCM(wav []byte) []byte {
	if len(wav) < 44 || string(wav[0:4]) != "RIFF" {
		return wav
	}
	for i := 12; i+8 <= len(wav); {
		chunk := string(wav[i : i+4])
		size := int(le32at(wav, i+4))
		if chunk == "data" {
			if i+8+size <= len(wav) {
				return wav[i+8 : i+8+size]
			}
			return wav[i+8:]
		}
		i += 8 + size + (size & 1) // chunk 按 2 字节对齐
	}
	return wav[44:]
}

func le16(b []byte, v uint16)  { b[0] = byte(v); b[1] = byte(v >> 8) }
func le32(b []byte, v uint32)  { b[0] = byte(v); b[1] = byte(v >> 8); b[2] = byte(v >> 16); b[3] = byte(v >> 24) }
func le32at(b []byte, i int) uint32 {
	return uint32(b[i]) | uint32(b[i+1])<<8 | uint32(b[i+2])<<16 | uint32(b[i+3])<<24
}

func mustJSON(v any) []byte { b, _ := json.MarshalIndent(v, "", "  "); return b }

// ---------- 状态机 ----------

// vtEval 唯一的 Eval 入口：必须经 Dispatch 投递到 WebView 主线程执行
// （jchv 的 Eval 是裸 COM 调用，跨线程直调会间歇失败/崩——STA 纪律）。
func vtEval(js string) {
	vtStateMu.Lock()
	wv := vtWV
	ready := vtPageReady.Load()
	vtStateMu.Unlock()
	if wv == nil || !ready {
		return
	}
	wv.Dispatch(func() { wv.Eval(js) })
}

func vtSetState(state, text string) {
	vtEval(fmt.Sprintf(`vt.state(%q, %q)`, state, text))
}

func vtSetState3(state, text string, arg int) {
	vtEval(fmt.Sprintf(`vt.state(%q, %q, %d)`, state, text, arg))
}

// vtErrTimeout 总超时哨兵：vtTranscribeWithTimeout 到点后返回它，
// 提示层据此区分「识别超时」与「识别失败」。
var vtErrTimeout = errors.New("识别超时")

// vtAsrTimeoutSec 一轮识别的总超时：语音时长 1:1 + 8s，下限 15s，不设上限。
// 沿用上游预算；冷启动与慢机器长语音的实际充足性仍须真机验证。
func vtAsrTimeoutSec(audioSec int) int {
	t := audioSec + 8
	if t < 15 {
		t = 15
	}
	return t
}

// vtEstimateSec 识别耗时预估（照搬 SayIt calculateThinkingDuration）。
func vtEstimateSec(recSec int) int {
	switch {
	case recSec <= 5:
		return 2
	case recSec <= 15:
		return 3
	case recSec <= 30:
		return 4
	case recSec <= 60:
		return 5
	case recSec <= 120:
		return 7
	case recSec <= 180:
		return 9
	case recSec <= 240:
		return 11
	default:
		return 13
	}
}

func vtOverlayTimerLoop(stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			vtStateMu.Lock()
			start, rec := vtStartAt, vtRecording
			vtStateMu.Unlock()
			if !rec {
				continue
			}
			total := int(time.Since(start).Seconds())
			vtEval(fmt.Sprintf(`vt.timer(%q)`, fmt.Sprintf("%d:%02d", total/60, total%60)))
		}
	}
}

// ---------- 会话代数（每轮语音一支代数） ----------
//
// 为什么需要它：收尾全是异步的——识别要几百毫秒到数秒，结果提示要停留 700ms，
// 流字气泡要 1.4s 才收起。这些延时如果落到**下一轮**上，就会把新一轮的悬浮窗关掉、
// 或把状态改回上一轮的"已输入"，用户看到的就是"两次语音之间像有冷却期、第二个
// 窗口呼不出来"。每轮开始录音时自增代数，所有延时回调先校验代数，过期直接丢弃——
// 与 VoiceType(Electron) 的 GEN.id 同一招，只是把校验铺到所有异步路径上。
// 提示本身该停留多久仍停留多久（体验不变），只是它再也不会越权管下一轮。
var vtSessionGen atomic.Int32

func vtGenOK(gen int32) bool { return gen == vtSessionGen.Load() }

// vtSetStateIf 只在代数仍是最新时改悬浮窗。
func vtSetStateIf(gen int32, state, text string) {
	if !vtGenOK(gen) {
		vtLog("丢弃过期状态更新：代数=%d 当前=%d（状态 %q 属于上一轮）", gen, vtSessionGen.Load(), state)
		return
	}
	vtSetState(state, text)
}

// vtSetState3If 带预估耗时的变体（识别中）。
func vtSetState3If(gen int32, state, text string, arg int) {
	if !vtGenOK(gen) {
		return
	}
	vtSetState3(state, text, arg)
}

// vtOverlayHideIf 只在代数仍是最新时收起悬浮窗——"连按像有冷却期"的根就在这：
// 上一轮的 700ms 延时隐藏如果照常执行，就会把**下一轮**刚弹出来的胶囊关掉。
func vtOverlayHideIf(gen int32) {
	if !vtGenOK(gen) {
		vtLog("丢弃过期收起：代数=%d 当前=%d（上一轮的延时收起不许关这一轮的窗）", gen, vtSessionGen.Load())
		return
	}
	vtOverlayHide()
}

// vtIsRecording 当前是否在录音（读锁内取，回调/事件循环共用）。
func vtIsRecording() bool {
	vtStateMu.Lock()
	defer vtStateMu.Unlock()
	return vtRecording
}

// ---------- 伪流式：中途段文本累积与流字展示 ----------

var (
	vtMidMu     sync.Mutex
	vtMidTexts  []string // 本会话各中途段文本（按序）
	vtMidFailed bool     // 任一中途识别失败 → 结束时回退全量识别
)

// vtMidSeg 一段中途音频 + 它属于哪一轮（过期段直接丢，别污染新一轮气泡）。
type vtMidSeg struct {
	Gen int32
	PCM []byte
}

// vtMidLoop 中途识别：串行消费切段器产出的段，只做气泡展示与日志。
func vtMidLoop() {
	for seg := range vtMidChGlobal {
		// 中途段是展示性的，超时按段时长 1:1 走同一个公式（见 vtAsrTimeoutSec）；
		// 超时/失败只丢这一段的气泡，不影响结束后的全量识别。
		audioSec := len(seg.PCM) / 2 / vtSampleRate
		text, err := vtTranscribeWithTimeout(seg.PCM, vtAsrTimeoutSec(audioSec))
		vtPending.Add(-1)
		if !vtGenOK(seg.Gen) {
			continue // 上一轮残留的段
		}
		if err != nil {
			continue // 展示性识别失败无所谓，不影响最终全量识别
		}
		if t := strings.TrimSpace(text); t != "" {
			vtStreamShow(seg.Gen, t)
			vtLog("中途段(仅展示): %q", t)
		}
	}
}

var vtStreamGen atomic.Int32

// vtStreamShow 流字：气泡模式（窗口 region 并集扩为胶囊+气泡）；1.4s 无新流字收起。
// 收起延迟保留（用户要求别动体验），但代数是这一轮的才允许收起/清理。
func vtStreamShow(sessGen int32, text string) {
	if !vtGenOK(sessGen) {
		return
	}
	gen := vtStreamGen.Add(1)
	vtOverlayWiden(1)
	vtEval(fmt.Sprintf(`vt.stream(%q)`, text))
	time.AfterFunc(1400*time.Millisecond, func() {
		if vtStreamGen.Load() != gen || !vtGenOK(sessGen) {
			return // 有更新的流字，或已经进入下一轮：由新一轮负责
		}
		vtEval(`vt.stream('')`)
		vtOverlayWiden(0)
	})
}

// vtPickTarget 选判定用的现场：以注入前一刻采集的 now 为准；now 拿不到
// 外部前台（悬浮窗抢到前台/没有前台）→ 用录音结束时（悬浮窗显示前采集的）
// 外部现场 end 兜底。
func vtPickTarget(end, now vtTarget) vtTarget {
	if now.Hwnd != 0 && now.Pid != uint32(os.Getpid()) {
		return now
	}
	return end
}

// vtDeliverAfterAsr 识别完成后的完整收尾：准备输入 → 检测此刻焦点 →
// 在输入框就注入、不在（含焦点落在菜单上）就只进剪贴板 → 落历史记录。
//
// 检测时机 = 准备要输入的此刻：识别要几百毫秒到数秒，录音开始/结束时焦点
// 都可能已变（历史 bug：同一窗口沿用录音开始的结论注入，Ctrl+V 落空还显示
// "已输入"）。绝不替用户按任何多余的键——焦点不在输入框就老老实实只复制。
// pcm 是本轮原始语音：无论注入/复制成败都进历史（可试听、可复制、可重识别）。
func vtDeliverAfterAsr(gen int32, text string, pcm []byte, end vtTarget) {
	now := vtCaptureTarget(true, "注入前")
	tgt := vtPickTarget(end, now)
	gate := vtEditabilityGate(tgt)
	vtLog("目标判定: gate=%s 可输入=%v | %s",
		gate, gate.editable(), tgt.describe())
	status := vtDeliver(gen, text, gate)
	vtSaveHistory(pcm, text, status)
	time.Sleep(700 * time.Millisecond)
	vtOverlayHideIf(gen)
}

// vtFinishSession 一段完整录音会话的收尾：检测 → 识别 → 注入/剪贴板。
//
// gen 是本轮代数：中途若有新一轮开始录音，gen 就过期了，本函数所有对悬浮窗的
// 操作都要作废（见 vtSessionGen 的说明），识别与写剪贴板照做不受影响。
func vtFinishSession(gen int32, sess *vtCapture) {
	// 兜底：识别/注入/剪贴板这条链上任何一处 panic 都不能让进程死掉。
	// 教训：`vtClipboardSeq` 曾把 user32 的函数写进 kernel32，LazyProc.Call 找不到
	// 过程直接 panic —— 表现是"一往输入框注入就整个程序消失，悬浮窗再也起不来"。
	defer func() {
		if r := recover(); r != nil {
			vtLog("会话收尾发生 panic（已拦截，进程继续运行）：%v", r)
		}
	}()
	pcm := sess.stop()
	// 录音结束的外部现场在显示悬浮窗之前取：悬浮窗一旦出现，前台有可能变成
	// 我们自己，识别期间真被抢了前台就靠这份现场兜底（历史 bug 是拿"悬浮窗
	// 自己"当目标判成可输入）。
	end := vtCaptureTarget(false, "录音结束")

	if vtGenOK(gen) {
		vtOverlayShow()
		vtSetState3If(gen, "processing", "识别中", vtEstimateSec(len(pcm)/2/vtSampleRate))
	}

	// 最终输出用【全量识别】（python VAD 精确切分，准确率优先）
	text := ""
	if len(pcm) > vtSampleRate/8 { // <125ms 视为误触
		var err error
		audioSec := len(pcm) / 2 / vtSampleRate
		if text, err = vtTranscribeWithTimeout(pcm, vtAsrTimeoutSec(audioSec)); err != nil {
			vtLog("识别失败: %v", err)
			// 失败也留档：音频是"重新识别"的原料，不能这轮白说
			vtSaveHistory(pcm, "", "asr_failed")
			if vtGenOK(gen) {
				// 超时单独成词（用户要求看得见的明确结束）：停留 1.5s 比"识别失败"的
				// 1.1s 略长，让用户看清"这是超时、可以立刻重试"，绝不是永久"识别中"。
				if errors.Is(err, vtErrTimeout) {
					vtSetStateIf(gen, "error", "识别超时，请重试")
					time.Sleep(1500 * time.Millisecond)
				} else {
					vtSetStateIf(gen, "error", "识别失败，请重试")
					time.Sleep(1100 * time.Millisecond)
				}
				vtOverlayHideIf(gen)
			}
			return
		}
	}
	text = strings.TrimSpace(text)
	vtLog("会话完成: 代数=%d 音频=%.1fs 文本=%q",
		gen, float64(len(pcm))/2/float64(vtSampleRate), text)
	if text == "" {
		if len(pcm) > vtSampleRate/8 {
			// 识别正常但没听清：音频留档，设置页可重新识别
			vtSaveHistory(pcm, "", "empty")
		}
		if vtGenOK(gen) {
			vtSetStateIf(gen, "empty", "没听清，再试一次")
			time.Sleep(1500 * time.Millisecond) // 停久一点：让用户看清提示
			vtOverlayHideIf(gen)
		}
		return
	}
	// 判定与输出收拢在 vtDeliverAfterAsr：识别完的此刻检测焦点、立刻分发，
	// 不加任何人为延时（代码自身的耗时就是全部延迟）。
	vtDeliverAfterAsr(gen, text, pcm, end)
}

// vtDeliver 输出一段文本：判定为可输入 → 剪贴板+Ctrl+V 注入；否则只进剪贴板。
// 返回结果状态（injected/copied），历史记录由调用方统一落。
//
// 两条路径都先把文本写进剪贴板——注入失败（误判、目标程序吞掉按键）时用户
// 手上至少还有可粘贴的内容，这是最差情况的兜底。
func vtDeliver(gen int32, text string, gate vtGate) string {
	editable := gate.editable()
	if editable {
		// 注入路径：临时占用剪贴板 → Ctrl+V → 恢复原剪贴板
		orig, had := vtClipboardGetText()
		wrote := vtClipboardSetText(text)
		// 写入后回读校验：写失败（剪贴板被别的程序占着）时千万别发 Ctrl+V，
		// 否则粘出来的是**旧内容**，用户会以为"粘错了/粘不出来"。
		if got, ok := vtClipboardGetText(); !wrote || !ok || got != text {
			vtLog("剪贴板写入未生效（wrote=%v 回读匹配=%v），本轮改为只提示、不注入", wrote, ok && got == text)
			vtSetStateIf(gen, "copied", "已复制")
			return "copied"
		}
		seq := vtClipboardSeq() // 记下"我们刚写入后"的序号
		// 注入前记下物理状态：注入后用它判断"是不是我们留下的残留"
		preCtrl := vtAsyncKeyDown(0xA2) || vtAsyncKeyDown(0xA3) || vtAsyncKeyDown(0x11)
		preV := vtAsyncKeyDown(0x56)
		time.Sleep(60 * time.Millisecond)
		vtSendCtrlV()
		vtInjectFence(preCtrl, preV) // 保证不残留（见函数注释）
		vtSetStateIf(gen, "done", "已输入")
		go func() {
			time.Sleep(250 * time.Millisecond)
			// 恢复前先看剪贴板有没有被别人动过（用户自己复制、系统剪贴板面板等）：
			// 动过就**绝不覆盖**——否则会把用户刚复制的内容冲掉，还会把 Win+V 面板
			// 顶掉（实测：面板刚打开就被一次写入关掉，表现为"闪一下又消失"）。
			if vtClipboardSeq() != seq {
				vtLog("剪贴板已被其它程序/用户改动，跳过恢复（不覆盖别人的内容）")
				return
			}
			if had {
				vtClipboardSetText(orig)
			} else {
				// 原来剪贴板里**没有文本**（可能是图片/文件/本来就是空的）：
				// 绝不 EmptyClipboard！空剪贴板会把用户原来的图片/文件抹掉，
				// 之后所有"粘贴"都变成粘不出东西（实测就是这个把用户剪贴板搞坏的）。
				// 那就把识别结果留在剪贴板里——它本来就是"最差情况的兜底"。
				vtLog("原剪贴板无文本：保留识别结果在剪贴板里（不清空，避免抹掉用户的图片/文件）")
			}
		}()
	} else {
		vtClipboardSetText(text)
		vtSetStateIf(gen, "copied", "已复制")
	}
	vtLog("输出: %s 可输入=%v 文本=%q", gate, editable, text)
	if editable {
		return "injected"
	}
	return "copied"
}

// vtDeliver 输出一段文本：判定为可输入 → 剪贴板+Ctrl+V 注入；否则只进剪贴板。
//
// 两条路径都先把文本写进剪贴板——注入失败（误判、目标程序吞掉按键）时用户
// 手上至少还有可粘贴的内容，这是最差情况的兜底。
// vtStartSession 开始一轮录音。
//
// 立刻自增代数：这是"上一轮的提示/气泡不许再管这一轮"的唯一开关，也是连按
// 不再像有冷却期的关键（上一轮的 700ms 延时隐藏、1.4s 气泡收起全部作废）。
func vtStartSession(reason string) {
	// 与原先一致：前台是宿主就把听写让给它自己的按钮
	if vtAvoidHostEnabled() && vtForegroundIsHost() {
		vtLog("前台是宿主程序，本轮热键让路（交给宿主的按钮听写）")
		return
	}
	gen := vtSessionGen.Add(1)
	cap, err := vtStartCapture()
	if err != nil {
		vtLog("开麦失败: %v", err)
		return
	}
	cap.gen = gen
	// 这里不做任何目标现场采集/判定：判定在识别完成后、注入前一刻
	// （vtDeliverAfterAsr），开麦只管开麦。
	vtMidMu.Lock()
	vtMidTexts, vtMidFailed = nil, false
	vtMidMu.Unlock()
	vtStateMu.Lock()
	vtRecording, vtCapCur, vtStartAt = true, cap, time.Now()
	vtStateMu.Unlock()
	vtRecordingOn.Store(true)
	// 注意：**不要**在这里用 vtHotkeysVKFirst() 覆盖 vtArmVK。
	// 事件循环在"按下"那一刻已经存了**实际按下的那一侧**（左 Alt=0xA4 / 右 Alt=0xA5），
	// 而 VKFirst 永远给配置里的第一枚（固定一侧）。覆盖错了侧，后面补发 down 会发成
	// 另一侧的键，真实抬起对不上账 → 系统认为原侧一直按着（Alt+Tab 切换器松手不关，
	// 得多按一次 Alt 才消失——用户实测就是这个问题）。
	vtOverlayShow()
	vtSetStateIf(gen, "recording", "")
	vtLog("录音开始（%s，代数=%d）", reason, gen)
	// 兜底硬截止：万一抬起事件丢了（钩子被摘、驱动异常），也不能一直录下去。
	// 5 分钟与 SayIt 的 HARD_RELEASE_AFTER_SECS 同量级。
	go func() {
		time.Sleep(5 * time.Minute)
		if vtGenOK(gen) && vtIsRecording() {
			vtStopSession("5 分钟硬截止")
		}
	}()
}

// vtStopSession 结束当前录音（松手/再按一次/点胶囊）。
func vtStopSession(reason string) {
	vtStateMu.Lock()
	cap, rec := vtCapCur, vtRecording
	vtRecording, vtCapCur = false, nil
	vtStateMu.Unlock()
	vtRecordingOn.Store(false)
	if !rec || cap == nil {
		return
	}
	vtLog("录音结束（%s，代数=%d）", reason, cap.gen)
	go vtFinishSession(cap.gen, cap)
}

// vtAbortSession 取消当前录音：停止采集、丢弃音频、**不识别不注入**。
//
// 用在哪：录音期间用户按了别的键（按住 Alt 又按 Tab 切窗口这种）——说明他压根不在
// 说话，是在用组合快捷键。这时既不能把语音灌进他正在操作的窗口，也不能让那半截
// 组合键漏到前台，所以本轮直接作废（钩子层已把 Alt 还给前台）。
func vtAbortSession(reason string) {
	vtStateMu.Lock()
	cap, rec := vtCapCur, vtRecording
	vtRecording, vtCapCur = false, nil
	vtStateMu.Unlock()
	vtRecordingOn.Store(false)
	if !rec || cap == nil {
		return
	}
	gen := cap.gen
	vtLog("取消本轮录音（%s）：音频丢弃、不注入；代数=%d", reason, gen)
	go func() {
		cap.stop() // 释放 waveIn；音频直接丢掉，不进识别
		if vtGenOK(gen) {
			vtSetStateIf(gen, "cancelled", "已取消")
			time.Sleep(500 * time.Millisecond)
			vtOverlayHideIf(gen)
		}
	}()
}

// vtEventLoop 热键状态机：按下 → 等长按阈值 → 开始录音；松开 → 结束或按点按处理。
//
// 300ms 长按阈值是本次新增的防误触：热键现在会吞键，点一下也开麦会很烦。
//
//	· 按住 ≥ holdMs：真正开始录音（长按=按住说话），松手结束
//	· 按住 < holdMs：算点按 —— hold 模式默认把这次按键原样回放给前台程序
//	  （点一下右 Alt 的原生行为不至于失效），tap 模式则切换录音
//
// 计时器只往 vtHoldFired 投递序号，**不直接改状态**：armed/armSeq 归本 goroutine
// 独占。之前计时器回调里直接读写它们，会和"松开"事件抢同一份状态 → 出现"漏 up"
// 误报、甚至点按与长按同时成立。
func vtEventLoop() {
	var (
		armed     bool
		armedVK   uint32
		armSeq    int
		synthetic bool
		tapMode   bool      // 本轮按的是 tap 模式（点按切换）
		pressAt   time.Time // 按下时刻（tap 模式用它判断"点按/按住较久"）
	)
	for {
		select {
		case ev := <-vtHKEvents:
			if ev.Down {
				if vtIsRecording() {
					// 已在使用中又按一次：tap 模式=结束；hold 模式=重复按下，忽略
					if vtHotkeys().Mode == "tap" {
						vtStopSession("再按一次热键（免提切换）")
					}
					continue
				}
				if armed {
					continue // 按住不放产生的 repeat
				}
				armed, armedVK, synthetic = true, ev.VK, ev.Synthetic
				armSeq++
				pressAt = time.Now()
				tapMode = vtHotkeys().Mode == "tap"
				if !synthetic {
					// 告诉钩子层"正在判定窗口里"：这期间按了别的键就要作废本轮
					// vtArmVK 已由钩子回调同步写入（真实 vk），这里不再覆盖（见 vt_windows.go 注释）
					vtArming.Store(true)
				}
				// 两种模式都起阈值计时——这是**按住也能用**的关键：
				// tap 模式如果只在"松开"时判断，用户按住说话时录音会在松手后才开始
				// （那时话已经说完了，录到的是空音频，实测就是这个现象）。
				// 现在：按住到阈值就开麦（按住说话照常）；阈值内松开算"点一下"（点按切换）。
				seq := armSeq
				threshold := vtHoldThreshold()
				time.AfterFunc(threshold, func() {
					select {
					case vtHoldFired <- seq:
					default:
					}
				})
				continue
			}
			// 松开
			if armed {
				armed = false
				armSeq++ // 作废还没触发的阈值计时
				vtArming.Store(false)
				held := time.Since(pressAt)
				if tapMode {
					// tap 模式 + 阈值内松开 = 点一下 → 切换（开/关各一次）
					if held <= vtTapThreshold() {
						vtToggleSession("点按切换")
					} else {
						vtLog("按住 %dms（> tapMs %dms）：仍按点按切换处理",
							held.Milliseconds(), vtTapThreshold().Milliseconds())
						vtToggleSession("按住较久的点按切换")
					}
					continue
				}
				vtOnShortTap(armedVK, synthetic)
				continue
			}
			// 已经过阈值、正在录音：松手要不要结束，由**触发方式**决定
			// · hold：按住说话，松开结束
			// · tap：免提切换，松开继续录（再按一次才结束）——与参考实现一致
			if vtIsRecording() {
				if vtHotkeys().Mode == "hold" {
					vtStopSession("松开热键")
				} else {
					vtLog("松开热键（tap 免提模式）：继续录音，再按一次结束")
				}
			}
		case other := <-vtArmCancel:
			// 按住热键期间按了别的键：当组合快捷键处理 —— 不录音/取消本轮，
			// 也不吐出半个组合（钩子层已经把被吞的那个按键还给前台了，
			// Alt+Tab 这类照常生效；继续按 Tab 也照常循环切换窗口）。
			if armed {
				armed = false
				armSeq++
				keyDesc := vtKeyName(armedVK)
				if keyDesc == "" {
					keyDesc = strings.Join(vtHotkeys().Main, "+")
				}
				vtLog("判定期内按下其它键 %q：按组合快捷键处理，本轮不录音（%s 已还给前台）",
					other, keyDesc)
				continue
			}
			if vtIsRecording() {
				vtAbortSession("录音期间按下其它键 " + other)
			}
		case seq := <-vtHoldFired:
			if !armed || seq != armSeq {
				continue // 这一轮已经结束（点按/组合键介入）或已被下一轮取代
			}
			armed = false
			vtArming.Store(false)
			// 只用我们自己记的账判断"热键还按着吗"：GetAsyncKeyState 对被低级钩子
			// 吞掉的键恒为"没按下"，用它复核会把每一次长按都误判成漏 up。
			if !synthetic && armedVK != 0 && vtHotVKDown.Load() != armedVK {
				vtLog("长按阈值到点但热键已松开（已收到抬起），本轮不录音")
				continue
			}
			if tapMode {
				vtStartSession("按住（tap 免提开始）")
			} else {
				vtStartSession("长按")
			}
		case <-vtStopClick: // 点击胶囊 = 结束当前录音
			vtStopSession("点击胶囊")
		}
	}
}

// vtHoldFired 长按阈值到点（计时器 → 事件循环；序号用来判别是否已被取代）。
var vtHoldFired = make(chan int, 4)

// vtToggleSession 点按切换：没在录 → 开始；正在录 → 结束。tap 模式专用。
//
// 为什么 tap 要"点一下"而不是"按住过阈值"：点按语义就是"点一下开、再点一下关"，
// 用户按下到松开通常 60~150ms；如果也要求 300ms 才算数，点按就永远不触发
// （实测反馈："点按没反应，只有长按有用"）。
func vtToggleSession(reason string) {
	if vtIsRecording() {
		vtStopSession(reason + "（结束）")
		return
	}
	vtStartSession(reason + "（开始）")
}

// vtOnShortTap 短按（按下到松开短于长按阈值）。
//
// 两条闸门都在"切换/长按"之前（与参考的开源输入法同一口径）：
//
//	① **不到 300ms 一律不算数**：不开始、不切换，把这次按键**原样回放**给前台
//	   ——所以轻点 Alt 照常弹菜单、轻点鼠标侧键照常后退，任何模式都一样；
//	② **组合键不算数**（见 vtArmCancel）：判定期内按了别的键，整轮作废并把
//	   被吞的键还给前台，Alt+Tab / Ctrl+C 照常。
//
// 只有"按住 ≥ 阈值"才进入触发，那时再按 mode 决定松手是否结束（hold 结束 / tap 切换）。
//
// 历史弯路：曾给裸修饰键加过"轻点例外"，其实不需要——只要守住①，所有键的轻点
// 都不受影响，特例反而让 tap 模式对其他键变得各说各话。
func vtOnShortTap(vk uint32, synthetic bool) {
	n := vtHoldThreshold().Milliseconds()
	if synthetic {
		vtLog("短按（后门模拟，未达阈值 %dms）：不录音", n)
		return
	}
	if vtReplayShortTapEnabled() && vk != 0 {
		vtSendKeyTap(vk)
		vtLog("短按（未达阈值 %dms）：不录音，按键已回放给前台（vk=%d）", n, vk)
		return
	}
	vtLog("短按（未达阈值 %dms）：不录音（回放已关闭）", n)
}
func procEmptyClipboardSafe() {
	if r, _, _ := procOpenClipboard.Call(0); r != 0 {
		procEmptyClipboard.Call()
		procCloseClipboard.Call()
	}
}

// vtPumpBars 把采集的波形包推给 WebView（10Hz 足够顺滑）。
func vtPumpBars(ch chan []float32, stop <-chan struct{}) {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case bars := <-ch:
			parts := make([]string, len(bars))
			for i, v := range bars {
				parts[i] = strconv.FormatFloat(float64(v), 'f', 3, 32)
			}
			vtEval("vt.bars([" + strings.Join(parts, ",") + "])")
		}
	}
}

// vtTriggerLoop 测试/自动化后门：监听本目录 trigger.txt，处理完即删。
// 物理热键外的受控触发入口（自测/回归用），命令见 README：
//
//	start / stop       直接开始/结束一轮录音（跳过 300ms 长按判定）
//	tap                模拟一次点按（按下立即松开，验证 <holdMs 的点按分支）
//	hold               模拟一次长按（按下 → 等阈值 → 松开，验证长按分支）
//	probe              把当前前台的完整现场与判定结论写进 vt.log
//	dry 要输出的文字    走正式流程同一个判定 + 注入/复制实现（不录音不识别）
func vtTriggerLoop() {
	for {
		time.Sleep(300 * time.Millisecond)
		p := filepath.Join(vtDir, "trigger.txt")
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		_ = os.Remove(p)
		cmd := strings.TrimSpace(string(b))
		vtLog("触发后门: %q", cmd)
		switch {
		case cmd == "start":
			vtStartSession("后门 start")
		case cmd == "stop":
			vtStopSession("后门 stop")
		case cmd == "tap":
			// 点按：按下后立刻松开，走 vtEventLoop 的 <holdMs 分支
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: true, VK: vtHotkeysVKFirst(), Synthetic: true}:
			default:
			}
			time.Sleep(60 * time.Millisecond)
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: false, VK: vtHotkeysVKFirst(), Synthetic: true}:
			default:
			}
		case cmd == "hold":
			// 长按：按下 → 等过阈值 → 松开
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: true, VK: vtHotkeysVKFirst(), Synthetic: true}:
			default:
			}
			time.Sleep(vtHoldThreshold() + 250*time.Millisecond)
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: false, VK: vtHotkeysVKFirst(), Synthetic: true}:
			default:
			}
		case cmd == "combo":
			// 模拟「按住热键的判定期内按了别的键」（如按住 Alt 又按 Tab）：
			// 走与钩子相同的通知路径，验证本轮作废、不录音
			select {
			case vtHKEvents <- vtHotkeyEvent{Down: true, VK: vtHotkeysVKFirst(), Synthetic: true}:
			default:
			}
			time.Sleep(50 * time.Millisecond)
			select {
			case vtArmCancel <- "tab":
			default:
			}
		case cmd == "combrec":
			// 模拟「已经在录音时按了别的键」：本轮应被取消（音频丢弃、不注入）
			vtStartSession("后门 combrec")
			time.Sleep(200 * time.Millisecond)
			select {
			case vtArmCancel <- "tab":
			default:
			}
		case cmd == "already":
			// 第二个实例发现已有实例在跑时留下的通知：让人看见"它其实在运行"
			gen := vtSessionGen.Add(1)
			vtOverlayShow()
			vtSetStateIf(gen, "already", "已在运行")
			go func() {
				time.Sleep(1800 * time.Millisecond)
				if !vtIsRecording() { // 同样无条件收，别让提示卡住
					vtOverlayHide()
				}
			}()
		case cmd == "state":
			// 状态快照：复现"Alt 被占住"时，先放一次 state，再复现一次，两份对比。
			vtModifierSnapshot()
		case cmd == "probe":
			// 判定探针：把当前前台的完整现场和结论写进 vt.log。
			// 排查「该复制却注入了 / 该注入却只复制」时不用改代码，写 trigger.txt=probe 即可。
			t := vtCaptureTarget(true, "探针")
			g := vtEditabilityGate(t)
			vtLog("探针判定: gate=%s 可输入=%v | %s", g, g.editable(), t.describe())
		case strings.HasPrefix(cmd, "reid"):
			// 重新识别历史第 n 条（0 起，从旧到新）：读 wav → 重跑识别 →
			// 更新该条记录并走完整分发（此刻焦点在输入框就直接打进去）。
			// 设置页「最近语音记录」的"重新识别"按钮将来也走这里。
			n := 0
			if v, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(cmd, "reid"))); err == nil {
				n = v
			}
			go vtReidentify(n)
		case strings.HasPrefix(cmd, "dry"):
			// 输出路径自测（不录音、不识别）：trigger.txt 写 "dry 要输出的文字"，
			// 走与正式流程同一个判定 + 注入/复制实现。做回归时不用真的对着麦克风说话。
			text := strings.TrimSpace(strings.TrimPrefix(cmd, "dry"))
			if text == "" {
				text = "语音输入法自测文本"
			}
			t := vtCaptureTarget(true, "自测")
			// 与正式流程同一个判定口径：此刻现场 → vtEditabilityGate → vtDeliver。
			g := vtEditabilityGate(t)
			gen := vtSessionGen.Add(1)
			vtLog("自测判定: gate=%s 可输入=%v | %s", g, g.editable(), t.describe())
			vtOverlayShow()
			go func() {
				vtDeliver(gen, text, g)
				time.Sleep(700 * time.Millisecond)
				vtOverlayHideIf(gen)
			}()
		}
	}
}

// vtReidentify 重新识别历史第 n 条：音频重跑 ASR → 更新记录 → 走完整分发
// （此刻焦点在输入框就直接注入，否则只复制——与正常一轮完全同一套规则）。
func vtReidentify(n int) {
	defer func() {
		if r := recover(); r != nil {
			vtLog("重新识别发生 panic（已拦截）：%v", r)
		}
	}()
	arr := vtReadHistory()
	if n < 0 || n >= len(arr) {
		vtLog("重新识别：没有第 %d 条（共 %d 条）", n, len(arr))
		return
	}
	item := arr[n]
	if item.Audio == "" {
		vtLog("重新识别：第 %d 条没有音频", n)
		return
	}
	wav, err := os.ReadFile(filepath.Join(vtDir, filepath.FromSlash(item.Audio)))
	if err != nil {
		vtLog("重新识别：音频读不出（%v）", err)
		return
	}
	pcm := vtWavToPCM(wav)
	gen := vtSessionGen.Add(1)
	vtOverlayShow()
	vtSetState3If(gen, "processing", "识别中", vtEstimateSec(len(pcm)/2/vtSampleRate))
	text, err := vtTranscribeWithTimeout(pcm, vtAsrTimeoutSec(len(pcm)/2/vtSampleRate))
	if err != nil {
		vtLog("重新识别失败: %v", err)
		if vtGenOK(gen) {
			if errors.Is(err, vtErrTimeout) {
				vtSetStateIf(gen, "error", "识别超时，请重试")
				time.Sleep(1500 * time.Millisecond)
			} else {
				vtSetStateIf(gen, "error", "识别失败，请重试")
				time.Sleep(1100 * time.Millisecond)
			}
			vtOverlayHideIf(gen)
		}
		return
	}
	text = strings.TrimSpace(text)
	vtLog("重新识别完成: 第 %d 条 文本=%q", n, text)
	if text == "" {
		if vtGenOK(gen) {
			vtSetStateIf(gen, "empty", "没听清，再试一次")
			time.Sleep(1500 * time.Millisecond)
			vtOverlayHideIf(gen)
		}
		return
	}
	// 更新该条的识别结果（原音频保留，状态由分发结果决定——用替换重写整份）
	arr[n].Text = text
	os.WriteFile(filepath.Join(vtDir, "history.json"), mustJSON(arr), 0644)
	// 分发：此刻焦点判定照走（end 兜底也现采一份，reid 没有录音结束时刻）
	end := vtCaptureTarget(false, "录音结束")
	vtDeliverAfterAsr(gen, text, pcm, end)
}

// vtHotkeysVKFirst 主热键第一枚键的 vk（后门模拟按下时用）。
func vtHotkeysVKFirst() uint32 {
	hk := vtHotkeys()
	if len(hk.Main) == 0 {
		return 0
	}
	return vtVKByName(hk.Main[0])
}

func main() {
	var err error
	vtDir, err = os.Getwd()
	if err != nil {
		log.Fatal(err)
	}
	// 日志落文件：GUI 进程无控制台，stderr 会丢
	logFile, err := os.OpenFile(filepath.Join(vtDir, "vt.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(logFile)
		defer logFile.Close()
	}
	log.SetFlags(log.Ltime)

	// 单实例：两个实例会装两套键盘钩子、出两个悬浮窗、抢同一个 ASR 端口
	if !vtSingleInstanceGuard() {
		// 关键：这里必须留一条**用户看得见**的反馈。以前只是静默退出，
		// GUI 程序没有控制台 → 用户双击后什么都没发生，看起来就是"启动不了"。
		// 让已经在跑的那个实例闪一下胶囊说"已在运行"（trigger.txt 后门通道）。
		vtLog("已有实例在运行，本次退出；已通知在跑的实例提示用户")
		_ = os.WriteFile(filepath.Join(vtDir, "trigger.txt"), []byte("already"), 0644)
		return
	}

	vtLoadHotkeys() // hotkeys.json（无文件用默认：Alt 按住说话）

	// 子进程会继承父进程的环境变量。如果主程序设置了 WEBVIEW2_*（自定义用户数据目录、
	// 附加浏览器参数、指定运行时目录），我们就会和它抢同一个 WebView2 环境/浏览器进程，
	// 结果是**窗口可见但什么都不渲染**（抓屏验证过：胶囊区域显示的是主程序内容）。
	// 这里：① 打印继承到的 WEBVIEW2_* 便于排查；② 强制用本目录下独立的用户数据目录。
	for _, k := range []string{"WEBVIEW2_USER_DATA_FOLDER", "WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS",
		"WEBVIEW2_BROWSER_EXECUTABLE_FOLDER", "WEBVIEW2_CHANNEL_SEARCH_KIND"} {
		if v := os.Getenv(k); v != "" {
			vtLog("继承到环境变量 %s=%s（将强制改用本目录独立配置）", k, v)
		}
	}
	os.Setenv("WEBVIEW2_USER_DATA_FOLDER", filepath.Join(vtDir, "webview2-profile"))
	os.Unsetenv("WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS")

	wv := webview2.NewWindow(false, nil)
	if wv == nil {
		log.Fatal("WebView2 初始化失败（需要 WebView2 Runtime）")
	}
	vtStateMu.Lock()
	vtWV = wv
	vtStateMu.Unlock()

	// 库自建窗口改造成胶囊：先库正规路径设尺寸，再改样式（去边框/不抢焦点/隐藏）
	wv.SetSize(vtWinW, vtWinH, webview2.HintNone)
	vtRestyleOverlay(uintptr(wv.Window()))
	vtLog("悬浮窗改造完成 hwnd=%d 窗口=%dx%d(恒定) 胶囊132x36", uintptr(wv.Window()), vtWinW, vtWinH)

	// 初始化后复验：Chromium controller 异步回调可能覆盖几何
	go func() {
		time.Sleep(1500 * time.Millisecond)
		if vtOverlayHwnd != 0 {
			vtForceOverlayGeom(vtOverlayHwnd, false)
			vtLog("复验窗口矩形: %s", vtWindowRectStr(vtOverlayHwnd))
		}
	}()

	// 页面点击结束 & 页面加载完成通知（Eval 在 ready 前会丢失）
	wv.Bind("vtStop", func() {
		select {
		case vtStopClick <- struct{}{}:
		default:
		}
	})
	// 页面加载完成通知（Eval 在 ready 前会丢失）
	wv.Bind("vtReady", func() {
		vtPageReady.Store(true)
		vtLog("overlay 页面就绪")
		vtApplyTheme(true) // 页面刚就绪：把主程序的主题色推上去（点缀色+文字色）
	})
	wv.Bind("vtSilent", func(on bool) {
		if on {
			vtOverlayWiden(1)
		} else {
			vtOverlayWiden(0)
		}
	})

	html, err := os.ReadFile(filepath.Join(vtDir, "overlay.html"))
	if err != nil {
		log.Fatalf("读 overlay.html 失败: %v", err)
	}
	wv.SetHtml(string(html))

	go vtHookLoop()
	go vtEventLoop()
	go vtMidLoop()
	go vtHotkeysWatch()
	go vtHostWatch() // 前台是不是宿主（钩子回调只读这个原子量，不查进程名）
	go vtTriggerLoop()
	// 钩子健康检查已停用（用户要求）：它曾每 3 秒注入一个按键，会把 Win+V 剪贴板面板踢掉。
	// 需要时再把 vtHookHealth 打开。
	// 启动可见反馈：GUI 子系统没有控制台、也没有托盘图标，双击后"什么都没发生"
	// 很容易被当成没启动成功。就绪后闪 1.2 秒胶囊说一句"已就绪"。
	go func() {
		for i := 0; i < 50 && !vtPageReady.Load(); i++ {
			time.Sleep(100 * time.Millisecond)
		}
		if vtIsRecording() {
			return
		}
		gen := vtSessionGen.Add(1)
		vtOverlayShow()
		vtSetStateIf(gen, "ready", "已就绪")
		time.Sleep(1500 * time.Millisecond)
		// 无条件收起（1.5 秒）：这里**不能**用代数校验，否则任何一个动作把代数推进
		// 都会让"已就绪"永久留在屏幕上（实测踩过：日志 `丢弃过期收起：代数=1 当前=2`）。
		if !vtIsRecording() {
			vtOverlayHide()
		}
	}()
	barsStop := make(chan struct{})
	go vtPumpBars(vtBarsChGlobal, barsStop)
	go vtOverlayTimerLoop(barsStop)
	// 置顶保活 + 几何保活
	go func() {
		t := time.NewTicker(2 * time.Second)
		defer t.Stop()
		for range t.C {
			if vtOverlayHwnd != 0 {
				if visible, _, _ := procIsWindowVisible.Call(vtOverlayHwnd); visible != 0 {
					procSetWindowPos.Call(vtOverlayHwnd, ^uintptr(0), /*HWND_TOPMOST*/
						0, 0, 0, 0, swpNoActivate|0x0001 /*NOSIZE*/ |0x0002 /*NOMOVE*/)
				}
			}
		}
	}()

	startAsr()
	go vtAsrWatch() // 识别服务保活：孤儿/崩溃自动自愈（见 vtAsrWatch 注释）

	// 启动自检：录 350ms 报一次麦克风电平与设备列表。
	// 为什么值得做：设备抓成虚拟设备/麦克风被静音时，热键、识别框架全都"正常"，
	// 但结果永远是 0 字（实测踩过：日志里全是 音频=x.xs 文本=""）——这行日志能一眼看出来。
	go func() {
		time.Sleep(1200 * time.Millisecond)
		vtMicSelftest()
		vtLog("采集设备列表：%s", vtMicNames(vtMicList()))
	}()

	// 父进程看护：由主程序拉起时（VT_PARENT_PID 注入），父进程一消失就自己退出。
	// 这样即使主程序崩溃/被强杀也不会留下孤儿，又不需要 Job 对象
	//（Job 会让 WebView2 渲染不出来、剪贴板受限——实测踩过）。
	if pp := os.Getenv("VT_PARENT_PID"); pp != "" {
		go func(ppid string) {
			var pid uint32
			fmt.Sscanf(ppid, "%d", &pid)
			if pid == 0 {
				return
			}
			for {
				time.Sleep(2 * time.Second)
				h, _, _ := procOpenProcess.Call(0x1000 /*PROCESS_QUERY_LIMITED_INFORMATION*/, 0, uintptr(pid))
				if h == 0 {
					vtLog("父进程 %d 已退出，输入法随之退出（不留孤儿）", pid)
					os.Exit(0)
				}
				procCloseHandle.Call(h)
			}
		}(pp)
	}

	hk := vtHotkeys()
	if hk.Mode == "tap" {
		vtLog("独立版就绪：热键 main=%v sub=%v，**点一下切换**（≤%dms 算点按；再点一次结束）",
			hk.Main, hk.Sub, vtTapThreshold().Milliseconds())
	} else {
		vtLog("独立版就绪：热键 main=%v sub=%v，**按住 ≥%dms 才开麦**，松开结束（点一下不录音）",
			hk.Main, hk.Sub, vtHoldThreshold().Milliseconds())
	}
	wv.Run() // WebView 消息循环（阻塞）
}

// vtBarsChGlobal 波形通道：采集 loop 投递、vtPumpBars 消费（会话间复用）。
var vtBarsChGlobal = make(chan []float32, 4)
