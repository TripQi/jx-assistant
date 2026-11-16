package main

// 用量明细「输出速度」（t/s）的测试。
//
// 口径（与用户定的口径一致）：速度 = 输出（completion）token 数 ÷ 生成耗时（秒），
// 生成耗时取「首末输出事件之间」那段（首字延迟另有一列，生成前的排队等待不算），
// 保留 1 位小数；没有生成耗时或没有输出 token 时给 null（页面写「—」），
// 绝不写 0 或 Infinity 冒充。
//
// 本文件两层看护：
//   - 接口契约：/api/usage/records 的每条明细都带 outMs（生成耗时）与 tokensPerSec；
//   - 前端渲染：用量页明细表真有「速度」列，有值写「12.3 t/s」、空值写「—」。

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/dop251/goja"
)

// usageSpeedRecordItems 调真实明细接口并解析 items。
func usageSpeedRecordItems(t *testing.T, a *App) []map[string]any {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleUsageRecords(rec, httptest.NewRequest("GET", "/api/usage/records?limit=10&offset=0", nil))
	if rec.Code != 200 {
		t.Fatalf("明细接口应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	var body struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("明细不是合法 JSON: %v", err)
	}
	return body.Items
}

// TestUsageRecordsExposeSpeedField 明细接口必须给出速度字段：没有它页面就不可能显示，
// 也不可能按数值排序。
func TestUsageRecordsExposeSpeedField(t *testing.T) {
	a := newUsageTestApp(t)
	seedUsageRows(t, a, usageRow{
		TS: "2026-11-16 10:00:00", Model: "m1", Protocol: "chat", ProviderID: "p1",
		Input: 1000, Output: 100, OK: true, Stream: true,
	})
	items := usageSpeedRecordItems(t, a)
	if len(items) != 1 {
		t.Fatalf("应有 1 条明细，得到 %d", len(items))
	}
	it := items[0]
	if _, ok := it["outMs"]; !ok {
		t.Fatalf("明细缺少 outMs（生成耗时）字段：%v", it)
	}
	if _, ok := it["tokensPerSec"]; !ok {
		t.Fatalf("明细缺少 tokensPerSec（速度）字段：%v", it)
	}
}

// usageRecordsHarness 是客户前置条件：真实 组件/usage-stats/client.js + 最小 DOM +
// fetch 打桩（明细返回给定 JSON，其余端点给空壳），mount 设置页后即可断言明细表。
const usageRecordsHarness = `
class Element {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.style = {}; this.attrs = {};
    this.value = ''; this._text = ''; this._html = ''; this.className = '';
    this.classList = { add: function(){}, remove: function(){}, toggle: function(){} };
  }
  set textContent(v) { this._text = v == null ? '' : String(v); this.children = []; }
  get textContent() { return this._text; }
  set innerHTML(v) { this._html = v == null ? '' : String(v); if (!this._html) this.children = []; }
  get innerHTML() { return this._html; }
  appendChild(e) { this.children.push(e); return e; }
  get options() { return this.children.filter(function(c){ return c.tagName === 'option'; }); }
  setAttribute(k, v) { this.attrs[k] = v; }
  querySelectorAll() { return []; }
  querySelector() { return null; }
  remove() {}
}
var root = new Element('div');
var document = {
  createElement: function(t){ return new Element(t); },
  createElementNS: function(ns, t){ return new Element(t); },
  getElementById: function(){ return null; }
};
var window = { confirm: function(){ return true; }, addEventListener: function(){}, removeEventListener: function(){} };
var CX = { register: function(d){ d.init(); }, registerSettings: function(d){ d.mount(root); } };
var JX = { on: function(){}, off: function(){} };
var setTimeout = function(){ return 1; };
var clearTimeout = function(){};
function all(e) { return e.children.reduce(function(a, c){ return a.concat([c], all(c)); }, []); }
function text(e) { return e.textContent + e.children.map(text).join(''); }
function recTable() {
  var tables = all(root).filter(function(e){
    return e.tagName === 'table' && String(e.className).indexOf('us-records-table') >= 0;
  });
  for (var i = 0; i < tables.length; i++) {
    if (text(tables[i]).indexOf('首字延迟') >= 0) return tables[i];
  }
  throw new Error('用量明细表 missing');
}
function headers() {
  return all(recTable()).filter(function(e){ return e.tagName === 'th'; }).map(text);
}
function bodyRows() {
  var tb = recTable().children.filter(function(c){ return c.tagName === 'tbody'; })[0];
  if (!tb) throw new Error('明细表 tbody missing');
  return tb.children;
}
function rowOf(model) {
  var rows = bodyRows();
  for (var i = 0; i < rows.length; i++) {
    if (text(rows[i]).indexOf(model) >= 0) return rows[i];
  }
  throw new Error('明细行 missing: ' + model);
}
function cells(row) { return row.children.filter(function(c){ return c.tagName === 'td'; }); }
function check(b, msg) { if (!b) throw new Error(msg); }
var emptyBody = JSON.stringify({ models: [], providers: [], search: [], note: '', ruleReady: false,
  kpi: {}, daily: [], byModel: [], items: [], total: 0, byProvider: [], limit: 0, offset: 0 });
var fetch = function(path, opts) {
  var body = String(path).indexOf('/api/usage/records') === 0 ? recordsJSON : emptyBody;
  return Promise.resolve({
    ok: true, status: 200,
    text: function(){ return Promise.resolve(body); },
    json: function(){ return Promise.resolve(JSON.parse(body)); }
  });
};
`

// mountUsageRecords 用给定明细 JSON 跑真实组件前端，返回 goja 运行时（断言脚本用）。
func mountUsageRecords(t *testing.T, recordsJSON string) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	vm.Set("recordsJSON", recordsJSON)
	if _, err := vm.RunString(usageRecordsHarness); err != nil {
		t.Fatalf("装 DOM 打桩失败: %v", err)
	}
	source, err := os.ReadFile("组件/usage-stats/client.js")
	if err != nil {
		t.Fatalf("读用量页前端失败: %v", err)
	}
	if _, err := vm.RunString(string(source)); err != nil {
		t.Fatalf("跑用量页前端失败: %v", err)
	}
	return vm
}

// runJS 跑一段断言脚本（失败即 t.Fatal，错误信息带现场）。
func runJS(t *testing.T, vm *goja.Runtime, script string) {
	t.Helper()
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestUsageRecordsTableSpeedColumn 前端明细表必须有「速度」列：有值「12.3 t/s」，
// 空值「—」，且整表不出现 NaN / Infinity。
func TestUsageRecordsTableSpeedColumn(t *testing.T) {
	records := map[string]any{
		"total": 2, "limit": 50, "offset": 0,
		"items": []map[string]any{
			// 老记录：没有生成耗时也没有速度（本次改动之前的行）
			{"ts": "2026-11-16 10:05:00", "model": "m-old", "protocol": "chat",
				"input": 100, "output": 0, "cacheRead": 0, "cacheWrite": 0, "total": 100,
				"ttftMs": 0, "durationMs": 900, "ok": true, "stream": true},
			// 有速度：49 tok / 4000ms = 12.25 → 12.3 t/s
			{"ts": "2026-11-16 10:00:00", "model": "m-speed", "protocol": "chat",
				"input": 1000, "output": 49, "cacheRead": 0, "cacheWrite": 0, "total": 1049,
				"ttftMs": 380, "durationMs": 4380, "outMs": 4000, "tokensPerSec": 12.3,
				"ok": true, "stream": true},
		},
	}
	raw, err := json.Marshal(records)
	if err != nil {
		t.Fatal(err)
	}
	vm := mountUsageRecords(t, string(raw))
	runJS(t, vm, `
var hs = headers();
check(hs.indexOf('速度') > 0, '明细表缺少「速度」列：' + hs.join(' | '));
check(hs.indexOf('速度') === hs.indexOf('耗时') + 1, '速度列应紧跟「耗时」：' + hs.join(' | '));
var speedAt = hs.indexOf('速度');
var c1 = cells(rowOf('m-speed'))[speedAt];
check(c1.textContent === '12.3 t/s', '速度单元应为「12.3 t/s」，得到 ' + JSON.stringify(c1.textContent));
check(String(c1.className).indexOf('num') >= 0, '速度单元应是数值样式（num）：' + c1.className);
var c2 = cells(rowOf('m-old'))[speedAt];
check(c2.textContent === '—', '空值单元应显示「—」，得到 ' + JSON.stringify(c2.textContent));
check(!/NaN|Infinity/.test(text(recTable())), '明细出现了 NaN / Infinity');
`)
}

// ---------- 口径与数值（a/b/c/e） ----------

// withUsageClock 把记账时钟换成按序列返回的假钟：第 i 次读钟得到 base + steps[i]，
// 序列用完后一直返回最后一个值。有了它「首字延迟 / 生成耗时」是确定值，速度才能
// 逐位比对（真流式的毫秒抖动没法定期望值）。
func withUsageClock(t *testing.T, steps ...time.Duration) {
	t.Helper()
	base := time.Date(2026, 11, 16, 10, 0, 0, 0, time.Local)
	var i int
	old := usageClock
	usageClock = func() time.Time {
		d := steps[len(steps)-1]
		if i < len(steps) {
			d = steps[i]
		}
		i++
		return base.Add(d)
	}
	t.Cleanup(func() { usageClock = old })
}

// usageSpeedRecordsRaw 取明细接口的原始 JSON（看 null 字段与 NaN/Infinity 用）。
func usageSpeedRecordsRaw(t *testing.T, a *App) string {
	t.Helper()
	rec := httptest.NewRecorder()
	a.handleUsageRecords(rec, httptest.NewRequest("GET", "/api/usage/records?limit=10&offset=0", nil))
	if rec.Code != 200 {
		t.Fatalf("明细接口应 200，得到 %d（%s）", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestUsageTokensPerSecFormula 钉死算式：输出 token ÷ 生成耗时（秒），保留 1 位小数；
// 除零与没输出一律 null（不是 0、不是 NaN/Inf）。
func TestUsageTokensPerSecFormula(t *testing.T) {
	cases := []struct {
		name   string
		output int64
		outMs  int64
		want   *float64
	}{
		{"49 tok / 4000ms → 12.25 四舍五入", 49, 4000, floatPtr(12.3)},
		{"100 tok / 3000ms → 33.333… 保留 1 位", 100, 3000, floatPtr(33.3)},
		{"1 tok / 3000ms → 0.3 不抹成 0", 1, 3000, floatPtr(0.3)},
		{"250 tok / 1000ms → 250.0", 250, 1000, floatPtr(250)},
		{"没有输出 token → null", 0, 3000, nil},
		{"没有生成耗时 → null", 100, 0, nil},
		{"负的生成耗时 → null", 100, -5, nil},
	}
	for _, c := range cases {
		got := usageTokensPerSec(c.output, c.outMs)
		if c.want == nil {
			if got != nil {
				t.Fatalf("%s：应为 null，得到 %v", c.name, *got)
			}
			continue
		}
		if got == nil || *got != *c.want {
			t.Fatalf("%s：应为 %v，得到 %v", c.name, *c.want, got)
		}
	}
}

// TestUsageSpeedNullCases 除零与没输出时明细给 null（不出现 NaN / Infinity / 0）。
func TestUsageSpeedNullCases(t *testing.T) {
	a := newUsageTestApp(t)

	// 1) 有生成耗时但厂家报 completion=0：无速度
	withUsageClock(t, 0, time.Second, 3*time.Second, 3100*time.Millisecond)
	cb, finish := a.meterUsage(usagePurposeTask, nil, LLMTarget{Model: "m-no-output", ProviderID: "p1"}, "",
		StreamCallbacks{})
	cb.OnText("甲")
	cb.OnText("乙")
	cb.OnUsage(Usage{PromptTokens: 10, CompletionTokens: 0})
	finish(nil)

	// 2) 两个输出事件落在同一毫秒：窗口为 0，无速度
	withUsageClock(t, 0, 2*time.Second, 2*time.Second, 2100*time.Millisecond)
	cb2, finish2 := a.meterUsage(usagePurposeTask, nil, LLMTarget{Model: "m-no-window", ProviderID: "p1"}, "",
		StreamCallbacks{})
	cb2.OnText("甲")
	cb2.OnText("乙")
	cb2.OnUsage(Usage{PromptTokens: 10, CompletionTokens: 50})
	finish2(nil)

	waitUsageRows(t, a, 2)
	rows, _, err := a.Store.UsageRecords(usageFilter{}, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("应有 2 条明细，得到 %d", len(rows))
	}
	// 新的在前：窗口 0 的那条在前，completion=0 的在後
	if rows[0].Model != "m-no-window" || rows[1].Model != "m-no-output" {
		t.Fatalf("明细顺序/模型不对：%s / %s", rows[0].Model, rows[1].Model)
	}
	if rows[1].OutMs != 2000 || rows[1].TokensPerSec != nil {
		t.Fatalf("completion=0 时应有生成耗时 2000ms、速度为 null：outMs=%d tps=%v", rows[1].OutMs, rows[1].TokensPerSec)
	}
	if rows[0].OutMs != 0 || rows[0].TokensPerSec != nil {
		t.Fatalf("窗口为 0 时应 outMs=0、速度为 null：outMs=%d tps=%v", rows[0].OutMs, rows[0].TokensPerSec)
	}
	raw := usageSpeedRecordsRaw(t, a)
	if n := strings.Count(raw, `"tokensPerSec":null`); n != 2 {
		t.Fatalf("两条明细都应给 tokensPerSec=null（实际 %d 处）：%s", n, raw)
	}
	if strings.Contains(raw, "NaN") || strings.Contains(raw, "Infinity") {
		t.Fatalf("明细出现了 NaN / Infinity：%s", raw)
	}
}

// TestUsageSpeedThreeProtocols 三条协议各跑一次真流（含流式多帧），用假钟钉住
// 首字延迟与生成耗时，逐位比对速度——同时证明分母是「首末输出事件之间」而不是总耗时。
func TestUsageSpeedThreeProtocols(t *testing.T) {
	cases := []struct {
		name       string
		protocol   string
		sse        string
		clockMs    []time.Duration
		completion int64
		wantTTFT   int64
		wantOut    int64
		wantDur    int64
		wantTPS    float64
	}{
		{
			name: "chat 两帧正文", protocol: "chat",
			sse: "data: {\"choices\":[{\"delta\":{\"content\":\"你好\"}}]}\n\n" +
				"data: {\"choices\":[{\"delta\":{\"content\":\"，世界\"}}],\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":90,\"total_tokens\":190}}\n\n" +
				"data: [DONE]\n\n",
			// 读钟序列：start / 首帧 / 末帧 / finish
			clockMs:    []time.Duration{0, time.Second, 4 * time.Second, 4100 * time.Millisecond},
			completion: 90, wantTTFT: 1000, wantOut: 3000, wantDur: 4100, wantTPS: 30.0,
		},
		{
			name: "responses 两帧正文", protocol: "responses",
			sse: "event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"好\"}\n\n" +
				"event: response.output_text.delta\n" +
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"的\"}\n\n" +
				"event: response.completed\n" +
				"data: {\"type\":\"response.completed\",\"response\":{\"usage\":{\"input_tokens\":50,\"output_tokens\":45,\"total_tokens\":95}}}\n\n",
			clockMs:    []time.Duration{0, time.Second, 2500 * time.Millisecond, 2600 * time.Millisecond},
			completion: 45, wantTTFT: 1000, wantOut: 1500, wantDur: 2600, wantTPS: 30.0,
		},
		{
			name: "anthropic 思考+正文两帧", protocol: "anthropic",
			sse: "event: message_start\n" +
				"data: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":25,\"output_tokens\":1}}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"先想\"}}\n\n" +
				"event: content_block_delta\n" +
				"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"这就答。\"}}\n\n" +
				"event: message_delta\n" +
				"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":120}}\n\n" +
				"event: message_stop\n" +
				"data: {\"type\":\"message_stop\"}\n\n",
			clockMs:    []time.Duration{0, 800 * time.Millisecond, 3200 * time.Millisecond, 3300 * time.Millisecond},
			completion: 120, wantTTFT: 800, wantOut: 2400, wantDur: 3300, wantTPS: 50.0,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := newUsageTestApp(t)
			withUsageClock(t, c.clockMs...)
			sse := c.sse
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(sse))
			}))
			defer srv.Close()

			target := LLMTarget{BaseURL: srv.URL, APIKey: "sk-test", Model: "m-" + c.protocol,
				Protocol: c.protocol, ProviderID: "p1"}
			cb, finish := a.meterUsage(usagePurposeTask, &SessionMeta{ID: "s-speed"}, target, "", StreamCallbacks{})
			err := StreamChat(t.Context(), target, ChatRequest{
				Messages: []Message{TextMessage("user", "说句话")},
			}, cb)
			finish(err)
			if err != nil {
				t.Fatalf("StreamChat(%s): %v", c.protocol, err)
			}

			waitUsageRows(t, a, 1)
			rows, _, err := a.Store.UsageRecords(usageFilter{}, 10, 0)
			if err != nil || len(rows) != 1 {
				t.Fatalf("应有 1 条明细，得到 %d（%v）", len(rows), err)
			}
			row := rows[0]
			if row.Protocol != usageProtocolLabel(c.protocol) {
				t.Fatalf("协议标签不对：%s", row.Protocol)
			}
			if row.Output != c.completion {
				t.Fatalf("输出 token 应为 %d，得到 %d", c.completion, row.Output)
			}
			if row.TTFTMs != c.wantTTFT || row.OutMs != c.wantOut || row.DurationMs != c.wantDur {
				t.Fatalf("首字延迟/生成耗时/总耗时应为 %d/%d/%d，得到 %d/%d/%d",
					c.wantTTFT, c.wantOut, c.wantDur, row.TTFTMs, row.OutMs, row.DurationMs)
			}
			if row.TokensPerSec == nil || *row.TokensPerSec != c.wantTPS {
				t.Fatalf("速度应为 %.1f t/s，得到 %v", c.wantTPS, row.TokensPerSec)
			}
			// 同一算式现算一遍（分母就是刚才断言的生成耗时，不是总耗时）
			want := math.Round(float64(row.Output)*10000/float64(row.OutMs)) / 10
			if *row.TokensPerSec != want {
				t.Fatalf("速度应用 %d tok ÷ %d ms 算，得到 %.1f vs %.1f", row.Output, row.OutMs, *row.TokensPerSec, want)
			}
			if wrong := float64(row.Output) * 1000 / float64(row.DurationMs); math.Abs(wrong-*row.TokensPerSec) < 0.05 {
				t.Fatalf("分母没体现出「首末输出事件之间」：计成总耗时也算出同样数值（%.1f）", wrong)
			}

			// 接口同样把数值给到前端（不多算一层）
			items := usageSpeedRecordItems(t, a)
			got, ok := items[0]["tokensPerSec"].(float64)
			if !ok || got != c.wantTPS {
				t.Fatalf("接口 tokensPerSec 应为 %.1f，得到 %v", c.wantTPS, items[0]["tokensPerSec"])
			}
		})
	}
}

// TestUsageSpeedOldRowEndToEnd 升级前的老记录（没有生成耗时）→ 明细照常显示，速度是「—」。
func TestUsageSpeedOldRowEndToEnd(t *testing.T) {
	a := newUsageTestApp(t)
	// 老记录：只写了输出 token，没有 out_ms（列加之前的行就是这个样子）
	seedUsageRows(t, a, usageRow{
		TS: "2026-11-15 09:00:00", Model: "m-old", Protocol: "chat", ProviderID: "p1",
		Input: 800, Output: 300, OK: true, Stream: true, TTFTMs: 400, DurationMs: 5200,
	})
	items := usageSpeedRecordItems(t, a)
	if len(items) != 1 {
		t.Fatalf("应有 1 条明细，得到 %d", len(items))
	}
	if v, has := items[0]["tokensPerSec"]; !has || v != nil {
		t.Fatalf("老记录的 tokensPerSec 应为 null，得到 %v（has=%v）", v, has)
	}
	if got, _ := items[0]["outMs"].(float64); got != 0 {
		t.Fatalf("老记录的 outMs 应为 0，得到 %v", items[0]["outMs"])
	}
	// 前端拿到这份真实接口数据：整表照常渲染，速度列写「—」
	vm := mountUsageRecords(t, usageSpeedRecordsRaw(t, a))
	runJS(t, vm, `
var hs = headers();
var at = hs.indexOf('速度');
check(at > 0, '明细表缺少「速度」列：' + hs.join(' | '));
var cell = cells(rowOf('m-old'))[at];
check(cell.textContent === '—', '老记录的速度应显示「—」，得到 ' + JSON.stringify(cell.textContent));
check(!/NaN|Infinity/.test(text(recTable())), '老记录渲染出现 NaN / Infinity');
`)
}

// TestUsageSpeedPageEndToEnd 接口 → 前端：服务端算的 12.3 t/s 在明细表里逐字显示。
func TestUsageSpeedPageEndToEnd(t *testing.T) {
	a := newUsageTestApp(t)
	seedUsageRows(t, a, usageRow{
		TS: "2026-11-16 10:00:00", Model: "m-speed", Protocol: "chat", ProviderID: "p1",
		Input: 1000, Output: 49, OK: true, Stream: true, TTFTMs: 380, DurationMs: 4380, OutMs: 4000,
	})
	vm := mountUsageRecords(t, usageSpeedRecordsRaw(t, a))
	runJS(t, vm, `
var hs = headers();
var at = hs.indexOf('速度');
check(at > 0, '明细表缺少「速度」列：' + hs.join(' | '));
var cell = cells(rowOf('m-speed'))[at];
check(cell.textContent === '12.3 t/s', '速度单元应为「12.3 t/s」，得到 ' + JSON.stringify(cell.textContent));
check(String(cell.className).indexOf('num') >= 0, '速度单元应是数值样式（num）：' + cell.className);
check((cell.title || '').indexOf('49 tok') >= 0, '悬停说明应写清分子分母：' + cell.title);
`)
}
