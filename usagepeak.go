package main

// usagepeak.go —— 计费口径里两条「厂商规则」：输入 token 的协议口径、高峰/空闲时段。
//
// 为什么单独成文件：这两条都是**官方事实**（不是本程序的配置），与价格配置
//（usagepricing.go）、采集聚合（usagestats.go）都不属于一类。集中一处便于按官方
// 文档核对与更新；改动它们会直接改历史重算的金额，所以每条都注明出处。
//
// 一、输入 token 口径（各协议不同，别一律做减法）：
//   - OpenAI 系（chat/completions、/v1/responses）与 DeepSeek 的 prompt_tokens
//     **已包含**缓存命中的部分（cached_tokens / prompt_cache_hit_tokens 只是另列
//     一份）。计费时必须把命中量从未命中输入里减掉：否则缓存 token 既按输入价
//     （最贵的价）又按缓存读价收了两遍——2026-11-12 实测这使当日估算虚高约 23 倍。
//   - Anthropic 原生 messages 的 input_tokens **不含**缓存：缓存读取/写入在
//     cache_read_input_tokens / cache_creation_input_tokens 里单列（llmproto.go
//     的 anthropicStreamAgg.finish 已按这个口径归一）。对它做减法会把未命中输入
//     算成 0，变成少收钱。
//
// 二、高峰/空闲（DeepSeek 官方规则，2026-11 核对：api-docs.deepseek.com 价格页）：
//   北京时间周一至周五 9:00–12:00、14:00–18:00 为高峰；其余时段（含周末与法定
//   节假日全天）为空闲，空闲价 = 峰值价 × 折扣（官方现行就是半价）。
//   判定按**每次调用发生的时间戳**（明细表的 ts），不按「现在」——历史数据重算才准。

import (
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

// usageTimezone 高峰/空闲按北京时间判定（用户与厂商的口径都是这个时区；
// 固定 +08:00，不依赖运行机器时区，也不受夏令时影响）。
var usageTimezone = time.FixedZone("CST", 8*3600)

// 高峰时段（北京时间，[起, 止) 的整点小时）。
const (
	usagePeakMorningFrom   = 9
	usagePeakMorningTo     = 12
	usagePeakAfternoonFrom = 14
	usagePeakAfternoonTo   = 18
)

// usageHolidayRanges 中国法定节假日的放假日（连休区间，全年全天按空闲计）。
//
// 来源：国务院办公厅《关于2026年部分节假日安排的通知》（国办发明电〔2025〕7号，
// 2025-11-04 发布）：元旦 1/1–1/3、春节 2/15–2/23、清明 4/4–4/6、劳动节 5/1–5/5、
// 端午 6/19–6/21、中秋 9/25–9/27、国庆 10/1–10/7。
//
// 更新方式：每年 11 月国务院发布次年安排后，在这里按年份追加一组区间（键是年份，
// 值是 "MM-DD" 的闭区间）。**没有配置的年份整年按周一至周五判定**——只会让那一年
// 的节假日当天金额偏高，不影响平日；周末始终算空闲，不依赖这张表。
//
// 调休上班的周末日（2026 年 1/4、2/14、2/28、5/9、9/20、10/10）按官方规则的字面
// 口径仍算周末→空闲：DeepSeek 的规则写的是「周一至周五 … 不含法定节假日」，调休
// 日既不是周一至周五的时段判定对象、也不是法定节假日。误差只落在这些当天的金额上。
var usageHolidayRanges = map[string][][2]string{
	"2026": {
		{"01-01", "01-03"}, // 元旦
		{"02-15", "02-23"}, // 春节
		{"04-04", "04-06"}, // 清明
		{"05-01", "05-05"}, // 劳动节
		{"06-19", "06-21"}, // 端午
		{"09-25", "09-27"}, // 中秋
		{"10-01", "10-07"}, // 国庆
	},
}

// usageIsHoliday 某天（'YYYY-MM-DD'）是否中国法定节假日。
func usageIsHoliday(day string) bool {
	if len(day) < 10 {
		return false
	}
	ranges := usageHolidayRanges[day[:4]]
	md := day[5:10]
	for _, r := range ranges {
		if md >= r[0] && md <= r[1] { // "MM-DD" 定宽，字典序即日期序
			return true
		}
	}
	return false
}

// usageIsPeak 判定某个时刻是否落在高峰时段。
func usageIsPeak(t time.Time) bool {
	t = t.In(usageTimezone)
	if wd := t.Weekday(); wd == time.Saturday || wd == time.Sunday {
		return false
	}
	if usageIsHoliday(t.Format("2006-01-02")) {
		return false
	}
	h := t.Hour()
	return (h >= usagePeakMorningFrom && h < usagePeakMorningTo) ||
		(h >= usagePeakAfternoonFrom && h < usagePeakAfternoonTo)
}

// usagePeakAt 按明细表的时间戳判定高峰（ts 是本地时间字符串，见 usagestats 的 now()）。
// 解析不出来（空串/老数据/脏数据）按高峰算：宁可高估也不低估——低估会让人误以为
// 还有余额，高估只是数字保守；且未配空闲折扣的模型本来就不受影响。
func usagePeakAt(ts string) bool {
	s := strings.TrimSpace(ts)
	if s == "" {
		return true
	}
	for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(layout, s, usageTimezone); err == nil {
			return usageIsPeak(t)
		}
	}
	return true
}
