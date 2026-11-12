package main

// usagepeak.go —— 计费口径里两条「厂商规则」：输入 token 的协议口径、高峰/空闲时段。
//
// 为什么单独成文件：这两条都直接影响金额，又与价格配置（usagepricing.go）、采集
// 聚合（usagestats.go）都不属于一类。集中一处便于核对；改动它们会改变历史重算
// 的结果，所以每条都注明口径与出处。
//
// 一、输入 token 口径（各协议不同，别一律做减法）：
//   - OpenAI 系（chat/completions、/v1/responses）与 DeepSeek 的 prompt_tokens
//     **已包含**缓存命中的部分（cached_tokens / prompt_cache_hit_tokens 只是另列
//     一份）。计费时必须把命中量从未命中输入里减掉：否则缓存 token 既按输入价
//     （最贵的价）又按缓存读价收了两遍——2026-11-12 实测这使当日估算虚高约 23 倍。
//   - Anthropic 原生 messages 的 input_tokens **不含**缓存：缓存读取/写入在
//     cache_read_input_tokens / cache_creation_input_tokens 里单列（llmproto.go
//     anthropicStreamAgg.finish 已按这个口径归一）。对它做减法会把未命中输入
//     算成 0，变成少收钱。
//
// 二、高峰/空闲时段（2026-11-12 改为「每个模型一条可配规则」，见 usagePeakRule）：
//   厂商的时段规则各不相同（甚至同一家会调整），节假日安排每年变，写死在代码里
//   既不准也不可改——用户实测「942 次调用全按高峰价」后定的口径是：规则由用户按
//   模型配置，程序只负责按**调用发生的时刻**判定。规则缺省＝不分时段（全按高峰价），
//   页面上明确提示「未配置时段规则」。
//
//   判定一律按明细表里的时间戳（usagestats 的 now()，本地时间字符串），**不按
//   「现在」**——否则历史数据重算出来的钱会随今天是工作日/周末乱跳。

import (
	"fmt"
	"strings"
	"time"
)

// usageInputIncludesCache 该协议的输入 token 是否已含缓存命中量（见文件头第①条）。
func usageInputIncludesCache(protocol string) bool {
	switch strings.ToLower(strings.TrimSpace(protocol)) {
	case "anthropic":
		return false
	default: // chat / responses / 空串 / 未知（OpenAI 兼容是绝大多数渠道）
		return true
	}
}

// usageSplit 是一次调用的计费拆分：未命中输入 / 缓存读取 / 缓存写入 / 输出。
type usageSplit struct {
	Missed     int64
	CacheRead  int64
	CacheWrite int64
	Output     int64
}

// splitUsageTokens 按协议把采集到的四个 token 量归一到计费口径。
func splitUsageTokens(protocol string, t usageTokens) usageSplit {
	s := usageSplit{CacheRead: t.CacheRead, CacheWrite: t.CacheWrite, Output: t.Output}
	if usageInputIncludesCache(protocol) {
		s.Missed = t.Input - t.CacheRead
		if s.Missed < 0 {
			s.Missed = 0
		}
	} else {
		s.Missed = t.Input
	}
	return s
}

// ---------- 高峰/空闲：每个模型一条规则 ----------

// usagePeakRule 是一个模型的时段规则（2026-11-12 由用户配置，不再写死在代码里）：
//
//	PeakWindows  高峰时间段，可多个，"HH:MM-HH:MM"（24 小时制）。**规则是否配置只看
//	             它**：一个时段都没有＝不分时段（全按高峰价）。
//	PeakDays     高峰发生在星期几，0=周日 … 6=周六（页面是星期多选）。空＝七天都算，
//	             与「全选七天」等价——用户只想设时段时不必再点七下。
//	Holidays     节假日（全天按空闲计），两种写法混用：
//	             "MM-DD"     每年重复（节假日安排年年相近，写一次不用年年改）
//	             "YYYY-MM-DD" 只此一年（某年的调休/特殊安排）
//	IdleFactor   空闲折扣：0.5＝空闲时段半价；(0,1) 之外的值＝不分时段；1＝不分时段。
//
// 跨天时段（"22:00-06:00"）的语义：**按调用当天的时刻判定，星期看调用当天**。
// 即「22:00 起至次日 06:00」，凌晨 0:00–5:59 落在窗口里但星期取调用当天的星期，
// 不回溯到前一天。理由：实现只有一处比较、不会有差一天的错误；页面文案照这个口径写。
type usagePeakRule struct {
	PeakWindows []string `json:"peakWindows,omitempty"`
	PeakDays    []int    `json:"peakDays,omitempty"`
	Holidays    []string `json:"holidays,omitempty"`
	IdleFactor  float64  `json:"idleFactor,omitempty"`
}

// usagePeakWindow 是解析后的一个高峰时段（分钟数，左闭右开）。
type usagePeakWindow struct {
	From int // 当天第几分钟（0–1439）
	To   int // 结束分钟；<= From 表示跨天
	Raw  string
}

// configured 规则是否配置：只看有没有高峰时段（见 usagePeakRule 的说明）。
func (r usagePeakRule) configured() bool { return len(r.PeakWindows) > 0 }

// idleDiscount 生效的空闲折扣（(0,1) 之外一律 1＝不分时段）。
func (r usagePeakRule) idleDiscount() float64 {
	if r.IdleFactor <= 0 || r.IdleFactor >= 1 {
		return 1
	}
	return r.IdleFactor
}

// windows 解析时段（格式在 normalize 里已校验过，这里对脏数据保持宽容：跳过）。
func (r usagePeakRule) windows() []usagePeakWindow {
	out := make([]usagePeakWindow, 0, len(r.PeakWindows))
	for _, raw := range r.PeakWindows {
		if w, ok := parseUsagePeakWindow(raw); ok {
			out = append(out, w)
		}
	}
	return out
}

// parseUsagePeakWindow 解析 "HH:MM-HH:MM"（也接受 "H:MM"、"HH:MM~HH:MM"）。
func parseUsagePeakWindow(raw string) (usagePeakWindow, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "~", "-"))
	s = strings.ReplaceAll(s, "－", "-")
	parts := strings.SplitN(s, "-", 2)
	if len(parts) != 2 {
		return usagePeakWindow{}, false
	}
	from, ok1 := usageParseHHMM(parts[0])
	to, ok2 := usageParseHHMM(parts[1])
	if !ok1 || !ok2 || from == to {
		return usagePeakWindow{From: from, To: to, Raw: s}, false
	}
	return usagePeakWindow{From: from, To: to, Raw: s}, true
}

// usageParseHHMM 把 "9:00" / "09:00" 解析成当天分钟数。
func usageParseHHMM(s string) (int, bool) {
	s = strings.TrimSpace(s)
	hm := strings.SplitN(s, ":", 2)
	if len(hm) != 2 {
		return 0, false
	}
	h, ok1 := usageAtoi(hm[0])
	m, ok2 := usageAtoi(hm[1])
	if !ok1 || !ok2 || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// usageAtoi 解析 1–2 位十进制数（时段分量的定宽格式，不接受空格与符号）。
func usageAtoi(s string) (int, bool) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 2 {
		return 0, false
	}
	n := 0
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	return n, true
}

// contains 该窗口是否包含当天的第 mins 分钟（跨天窗口：>= From 或 < To）。
func (w usagePeakWindow) contains(mins int) bool {
	if w.To > w.From { // 当天内：[From, To)
		return mins >= w.From && mins < w.To
	}
	// 跨天（To <= From，如 22:00-06:00）：当天 >= From，或 次日 < To
	return mins >= w.From || mins < w.To
}

// isHoliday 该天是否在规则配置的节假日里（"MM-DD" 每年重复，或 "YYYY-MM-DD" 指定）。
func (r usagePeakRule) isHoliday(t time.Time) bool {
	ymd := t.Format("2006-01-02")
	md := ymd[5:]
	for _, h := range r.Holidays {
		h = strings.TrimSpace(h)
		if len(h) == 10 { // 具体年月日
			if h == ymd {
				return true
			}
			continue
		}
		if h == md { // 每年重复
			return true
		}
	}
	return false
}

// isPeakAt 判定某时刻是否落在高峰（规则未配置＝不分时段，一律高峰）。
func (r usagePeakRule) isPeakAt(t time.Time) bool {
	if !r.configured() {
		return true
	}
	if r.isHoliday(t) {
		return false
	}
	if len(r.PeakDays) > 0 {
		wd := int(t.Weekday())
		found := false
		for _, d := range r.PeakDays {
			if d == wd {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	mins := t.Hour()*60 + t.Minute()
	for _, w := range r.windows() {
		if w.contains(mins) {
			return true
		}
	}
	return false
}

// usageTimeOf 解析明细表的时间戳（本地时间字符串，见 usagestats 的 now()）。
// 兼容三种写法：'YYYY-MM-DD HH:MM:SS'（逐条明细）、'YYYY-MM-DD HH:MM'（分钟桶）、
// 'YYYY-MM-DD HH'（老版本的小时桶，保留兼容）。
//
// 用 time.Local 而不是固定 +08:00：时间戳本来就是按本机时钟写的，按本机时钟读回来
// 才对得上（国内机器即北京时间）。
func usageTimeOf(ts string) (time.Time, bool) {
	s := strings.TrimSpace(ts)
	if s == "" {
		return time.Time{}, false
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15",
		"2006-01-02",
	} {
		if t, err := time.ParseInLocation(layout, s, time.Local); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// usagePeakAt 按规则判定某个时间戳是高峰还是空闲。解析不出时间戳（空串/脏数据）
// 按高峰算：宁可高估也不低估——低估会让人误以为还有余额，高估只是数字保守。
func (r usagePeakRule) peakAt(ts string) bool {
	t, ok := usageTimeOf(ts)
	if !ok {
		return true
	}
	return r.isPeakAt(t)
}

// normalize 校验并规整规则（脏配置不许把计费算歪）：
// 星期只留 0–6 且去重排序；时段只留能解析的并统一成 "HH:MM-HH:MM"；节假日只留
// "MM-DD" / "YYYY-MM-DD" 形式的合法日期；空闲折扣只留 (0,1)，其余归一为 0（不分时段）。
func (r *usagePeakRule) normalize() {
	if len(r.PeakDays) > 0 {
		seen := make(map[int]bool, 7)
		days := make([]int, 0, len(r.PeakDays))
		for _, d := range r.PeakDays {
			if d < 0 || d > 6 || seen[d] {
				continue
			}
			seen[d] = true
			days = append(days, d)
		}
		if len(days) == 7 { // 全选七天＝不限定星期，省得配置里多一串没意义的数字
			days = nil
		}
		r.PeakDays = days
	}
	if len(r.PeakWindows) > 0 {
		wins := make([]string, 0, len(r.PeakWindows))
		seen := map[string]bool{}
		for _, raw := range r.PeakWindows {
			w, ok := parseUsagePeakWindow(raw)
			if !ok {
				continue
			}
			key := usageFormatHHMM(w.From) + "-" + usageFormatHHMM(w.To)
			if seen[key] {
				continue
			}
			seen[key] = true
			wins = append(wins, key)
		}
		r.PeakWindows = wins
	}
	if len(r.Holidays) > 0 {
		hols := make([]string, 0, len(r.Holidays))
		seen := map[string]bool{}
		for _, raw := range r.Holidays {
			h, ok := usageNormalizeHoliday(raw)
			if !ok || seen[h] {
				continue
			}
			seen[h] = true
			hols = append(hols, h)
		}
		r.Holidays = hols
	}
	if r.IdleFactor <= 0 || r.IdleFactor >= 1 {
		r.IdleFactor = 0
	}
}

// usageFormatHHMM 分钟数 → "HH:MM"（24:00 也照原样写，表示当天末尾）。
func usageFormatHHMM(mins int) string {
	if mins < 0 {
		mins = 0
	}
	return fmt.Sprintf("%02d:%02d", mins/60, mins%60)
}

// usageNormalizeHoliday 校验节假日写法："MM-DD"（每年重复）或 "YYYY-MM-DD"（一次性）。
// 只做格式与月份/日期范围校验，不查日历（2月30日这种写错由用户负责，不必挡）。
func usageNormalizeHoliday(raw string) (string, bool) {
	s := strings.TrimSpace(strings.ReplaceAll(raw, "/", "-"))
	s = strings.ReplaceAll(s, ".", "-")
	switch len(s) {
	case 5: // MM-DD
		m, ok1 := usageAtoi(s[:2])
		d, ok2 := usageAtoi(s[3:])
		if s[2] != '-' || !ok1 || !ok2 || m < 1 || m > 12 || d < 1 || d > 31 {
			return "", false
		}
		return s, true
	case 10: // YYYY-MM-DD
		m, ok1 := usageAtoi(s[5:7])
		d, ok2 := usageAtoi(s[8:])
		if s[4] != '-' || s[7] != '-' || !ok1 || !ok2 || m < 1 || m > 12 || d < 1 || d > 31 {
			return "", false
		}
		return s, true
	}
	return "", false
}
