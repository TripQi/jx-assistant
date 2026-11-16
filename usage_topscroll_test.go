package main

// 用量明细「表格上方的横向滚动条」的测试。
//
// 为什么要有：明细表 14 列，屏窄时靠 .us-table-wrap 自己横向滚动，而那条滚动条
// 长在表格底部——用户必须先滚到表格最下面才拖得到，很别扭。所以表格正上方也放
// 一条窄的横向滚动条（内部占位元素撑到表格内容宽），两条 scrollLeft 双向同步。
//
// 本文件用真实的 组件/usage-stats/client.js + 最小 DOM（scrollLeft /
// scrollWidth / clientWidth 都是可写的桩，写入 scrollLeft 会同步派发 onscroll，
// 比真实浏览器的异步派发更苛刻）+ fetch 打桩，覆盖：
//
//	a. 需要横向滚动时顶部条存在、位置在表格容器正上方、占位宽 = 表格内容宽；
//	b. 拖顶部条（写 scrollLeft）→ 表格容器跟随；
//	c. 滚表格容器 → 顶部条跟随；
//	d. 无回环：一次操作最多各写一次，值稳定，不会来回抖；
//	e. 不需要横向滚动时隐藏（不占位），resize 变窄/变宽后状态跟着变。
//
// 修前的红：「表格上方没有顶部横向滚动条（.us-hscroll 未渲染）」。

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// usageTopScrollHarness 最小 DOM + 滚动桩 + fetch 打桩（明细返回 recordsJSON）。
const usageTopScrollHarness = `
class Element {
  constructor(tag) {
    this.tagName = tag; this.children = []; this.parentNode = null;
    this.style = {}; this.attrs = {}; this.value = '';
    this._text = ''; this._html = ''; this.className = '';
    this.scrollWidth = 0; this.clientWidth = 0; this._sl = 0; this._writes = [];
    this.classList = {
      add: function () {}, remove: function () {}, toggle: function () {},
      contains: function () { return false; }
    };
  }
  set textContent(v) { this._text = v == null ? '' : String(v); this.children = []; }
  get textContent() { return this._text; }
  set innerHTML(v) { this._html = v == null ? '' : String(v); if (!this._html) this.children = []; }
  get innerHTML() { return this._html; }
  appendChild(e) { e.parentNode = this; this.children.push(e); return e; }
  get options() { return this.children.filter(function (c) { return c.tagName === 'option'; }); }
  setAttribute(k, v) { this.attrs[k] = v; }
  querySelectorAll() { return []; }
  querySelector() { return null; }
  remove() {
    if (!this.parentNode) return;
    var i = this.parentNode.children.indexOf(this);
    if (i >= 0) this.parentNode.children.splice(i, 1);
    this.parentNode = null;
  }
}
// scrollLeft 桩：写入即记账；值真的变了才派发 onscroll（与浏览器一致，只是
// 同步派发——异步派发能过的，同步派发一定也能过）。
Object.defineProperty(Element.prototype, 'scrollLeft', {
  get: function () { return this._sl; },
  set: function (v) {
    var x = Number(v) || 0;
    this._writes.push(x);
    if (x === this._sl) return;
    this._sl = x;
    if (typeof this.onscroll === 'function') this.onscroll();
  },
  configurable: true
});
var root = new Element('div');
var document = {
  createElement: function (t) { return new Element(t); },
  createElementNS: function (ns, t) { return new Element(t); },
  getElementById: function () { return null; }
};
var resizeHandlers = [];
var window = {
  confirm: function () { return true; },
  addEventListener: function (t, fn) { if (t === 'resize') resizeHandlers.push(fn); },
  removeEventListener: function (t, fn) {
    var i = resizeHandlers.indexOf(fn); if (i >= 0) resizeHandlers.splice(i, 1);
  }
};
function fireResize() { resizeHandlers.slice().forEach(function (fn) { fn(); }); }
// ResizeObserver 桩：真机里分区页从 display:none 切成块时正好由它触发
var observers = [];
function ResizeObserver(fn) { this.fn = fn; this.targets = []; observers.push(this); }
ResizeObserver.prototype.observe = function (t) { this.targets.push(t); };
ResizeObserver.prototype.unobserve = function (t) {
  var i = this.targets.indexOf(t); if (i >= 0) this.targets.splice(i, 1);
};
ResizeObserver.prototype.disconnect = function () { this.targets = []; };
function fireObserve() {
  observers.slice().forEach(function (ro) { if (ro.targets.length) ro.fn([]); });
}
var CX = { register: function (d) { d.init(); }, registerSettings: function (d) { d.mount(root); } };
var JX = { on: function () {}, off: function () {} };
var setTimeout = function () { return 1; };
var clearTimeout = function () {};
function all(e) { return e.children.reduce(function (a, c) { return a.concat([c], all(c)); }, []); }
function text(e) { return e.textContent + e.children.map(text).join(''); }
function clsOf(e) { return String((e && e.className) || ''); }
function hasClass(e, c) { return clsOf(e).split(/\s+/).indexOf(c) >= 0; }
function check(b, msg) { if (!b) throw new Error(msg); }
function recTable() {
  var tables = all(root).filter(function (e) {
    return e.tagName === 'table' && clsOf(e).indexOf('us-records-table') >= 0;
  });
  for (var i = 0; i < tables.length; i++) {
    if (text(tables[i]).indexOf('首字延迟') >= 0) return tables[i];
  }
  throw new Error('用量明细表 missing');
}
function wrapOf(t) {
  var n = t.parentNode;
  while (n && !hasClass(n, 'us-table-wrap')) n = n.parentNode;
  if (!n) throw new Error('明细表不在 .us-table-wrap 容器里');
  return n;
}
// topScrollEl 找表格容器所在卡片里的顶部横向滚动条（找不到＝修前红）
function topScrollEl() {
  var wrap = wrapOf(recTable());
  var sibs = wrap.parentNode.children;
  for (var i = 0; i < sibs.length; i++) {
    if (hasClass(sibs[i], 'us-hscroll')) return sibs[i];
  }
  throw new Error('表格上方没有顶部横向滚动条（.us-hscroll 未渲染）');
}
function topInner(top) {
  check(top.children.length >= 1, '顶部滚动条内部没有宽度占位元素');
  return top.children[0];
}
// needScroll 设定「需要 / 不需要横向滚动」的尺寸桩并触发 resize 重算
function needScroll(contentW, viewW) {
  var wrap = wrapOf(recTable());
  wrap.scrollWidth = contentW;
  wrap.clientWidth = viewW;
  fireResize();
}
function headers() {
  return all(recTable()).filter(function (e) { return e.tagName === 'th'; }).map(text);
}
function bodyRows() {
  var tb = recTable().children.filter(function (c) { return c.tagName === 'tbody'; })[0];
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
function cells(row) { return row.children.filter(function (c) { return c.tagName === 'td'; }); }
var emptyBody = JSON.stringify({ models: [], providers: [], search: [], note: '', ruleReady: false,
  kpi: {}, daily: [], byModel: [], items: [], total: 0, byProvider: [], limit: 0, offset: 0 });
var fetch = function (path) {
  var body = String(path).indexOf('/api/usage/records') === 0 ? recordsJSON : emptyBody;
  return Promise.resolve({
    ok: true, status: 200,
    text: function () { return Promise.resolve(body); },
    json: function () { return Promise.resolve(JSON.parse(body)); }
  });
};
`

// usageTopScrollRecordsJSON 两条明细（一条有速度、一条老记录），明细表照常渲染。
const usageTopScrollRecordsJSON = `{"total":2,"limit":50,"offset":0,"items":[
 {"ts":"2026-11-16 10:05:00","model":"m-old","protocol":"chat","input":100,"output":0,
  "cacheRead":0,"cacheWrite":0,"total":100,"ttftMs":0,"durationMs":900,"ok":true,"stream":true},
 {"ts":"2026-11-16 10:00:00","model":"m-speed","protocol":"chat","input":1000,"output":49,
  "cacheRead":0,"cacheWrite":0,"total":1049,"ttftMs":380,"durationMs":4380,"outMs":4000,
  "tokensPerSec":12.3,"ok":true,"stream":true}]}`

// mountUsageTopScroll 用真实组件前端挂载设置页，返回 goja 运行时（断言脚本用）。
func mountUsageTopScroll(t *testing.T) *goja.Runtime {
	t.Helper()
	vm := goja.New()
	vm.Set("recordsJSON", usageTopScrollRecordsJSON)
	if _, err := vm.RunString(usageTopScrollHarness); err != nil {
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

func runTopScrollJS(t *testing.T, vm *goja.Runtime, script string) {
	t.Helper()
	if _, err := vm.RunString(script); err != nil {
		t.Fatalf("%v", err)
	}
}

// TestUsageRecordsTopScrollExists 需要横向滚动时，表格正上方有一条顶部滚动条，
// 占位宽与表格内容宽一致；既有列（速度列）不受影响。
func TestUsageRecordsTopScrollExists(t *testing.T) {
	vm := mountUsageTopScroll(t)
	runTopScrollJS(t, vm, `
var hs = headers();
check(hs.indexOf('速度') > 0, '明细表缺少「速度」列：' + hs.join(' | '));
var wrap = wrapOf(recTable());
var top = topScrollEl();
check(top.parentNode === wrap.parentNode, '顶部滚动条应与表格容器同在一张卡片里');
var idxTop = wrap.parentNode.children.indexOf(top);
var idxWrap = wrap.parentNode.children.indexOf(wrap);
check(idxTop === idxWrap - 1, '顶部滚动条应紧贴在表格容器正上方（idx ' + idxTop + ' vs ' + idxWrap + '）');
needScroll(1200, 600);
check(top.style.display === 'block', '需要横向滚动时顶部条应显示，得到 display=' + JSON.stringify(top.style.display));
var inner = topInner(top);
check(inner.style.width === '1200px', '占位宽应等于表格内容宽 1200px，得到 ' + JSON.stringify(inner.style.width));
`)
}

// TestUsageRecordsTopScrollStyles 样式文件里得有这条滚动条的类（薄、透明轨道、
// 默认不显示——不需要横向滚动时绝不占位）。
func TestUsageRecordsTopScrollStyles(t *testing.T) {
	raw, err := os.ReadFile("组件/usage-stats/client.css")
	if err != nil {
		t.Fatalf("读用量页样式失败: %v", err)
	}
	css := string(raw)
	for _, want := range []string{".us-hscroll", ".us-hscroll-in", ".us-hscroll::-webkit-scrollbar"} {
		if !strings.Contains(css, want) {
			t.Fatalf("client.css 缺少 %s 样式（顶部横向滚动条）", want)
		}
	}
	// 默认隐藏：只有 JS 判定需要横向滚动才 display:block
	i := strings.Index(css, ".us-hscroll {")
	if i < 0 {
		t.Fatalf("client.css 没有 .us-hscroll 规则块")
	}
	block := css[i:]
	if end := strings.Index(block, "}"); end > 0 {
		block = block[:end]
	}
	if !strings.Contains(block, "display: none") {
		t.Fatalf(".us-hscroll 应默认 display:none（不需要横向滚动时不占位），实际：%s", block)
	}
}

// TestUsageRecordsTopScrollSync 双向同步：拖上面 → 表格跟；滚表格 → 上面跟。
func TestUsageRecordsTopScrollSync(t *testing.T) {
	vm := mountUsageTopScroll(t)
	runTopScrollJS(t, vm, `
var wrap = wrapOf(recTable());
var top = topScrollEl();
needScroll(1200, 600);

// b. 拖顶部条 → 表格容器 scrollLeft 跟随
top.scrollLeft = 120;
check(wrap.scrollLeft === 120, '拖顶部条后表格容器 scrollLeft 应为 120，得到 ' + wrap.scrollLeft);

// c. 滚表格容器（触控板横滑 / Shift+滚轮 / 拖底部条都会走这条）→ 顶部条跟随
wrap.scrollLeft = 200;
check(top.scrollLeft === 200, '滚表格后顶部条 scrollLeft 应为 200，得到 ' + top.scrollLeft);
`)
}

// TestUsageRecordsTopScrollNoLoop 无回环：一次操作只向对方写一次，值到 200/120
// 就稳定；再反复触发两边 scroll 事件也不再写（否则就是来回抖）。
func TestUsageRecordsTopScrollNoLoop(t *testing.T) {
	vm := mountUsageTopScroll(t)
	runTopScrollJS(t, vm, `
var wrap = wrapOf(recTable());
var top = topScrollEl();
needScroll(1200, 600);

// 拖顶部条：只有用户那一次写顶部，加上同步给表格的一次
top._writes = []; wrap._writes = [];
top.scrollLeft = 120;
check(String(top._writes) === '120', '顶部只该有用户那一次 120，得到 [' + top._writes + ']');
check(String(wrap._writes) === '120', '表格应只被写一次 120（多写＝回环），得到 [' + wrap._writes + ']');
check(top.scrollLeft === 120 && wrap.scrollLeft === 120, '同步后两边应都是 120');

// 滚表格：反过来同样只写一次
top._writes = []; wrap._writes = [];
wrap.scrollLeft = 200;
check(String(wrap._writes) === '200', '表格只该有用户那一次 200，得到 [' + wrap._writes + ']');
check(String(top._writes) === '200', '顶部应只被写一次 200（多写＝回环），得到 [' + top._writes + ']');
check(top.scrollLeft === 200 && wrap.scrollLeft === 200, '同步后两边应都是 200');

// 稳定：值已一致时反复触发 scroll 事件，一次都不该再写
top._writes = []; wrap._writes = [];
for (var i = 0; i < 5; i++) {
  if (top.onscroll) top.onscroll();
  if (wrap.onscroll) wrap.onscroll();
}
check(top._writes.length === 0 && wrap._writes.length === 0,
  '值已一致时不应再写 scrollLeft（回环抖动）：top=[' + top._writes + '] wrap=[' + wrap._writes + ']');
check(top.scrollLeft === 200 && wrap.scrollLeft === 200,
  '反复触发后值应稳定在 200，得到 ' + top.scrollLeft + '/' + wrap.scrollLeft);
`)
}

// TestUsageRecordsTopScrollVisibility 按需显示 + resize 重算：内容挤不下才露出，
// 窗口变宽后收起，再变窄后重新出现且占位宽跟着内容宽走。
func TestUsageRecordsTopScrollVisibility(t *testing.T) {
	vm := mountUsageTopScroll(t)
	runTopScrollJS(t, vm, `
var wrap = wrapOf(recTable());
var top = topScrollEl();

// 装得下：不显示（不占位）
needScroll(700, 700);
check(top.style.display === 'none', '内容挤得下时顶部条应隐藏，得到 display=' + JSON.stringify(top.style.display));
check(topInner(top).style.width !== '700px', '隐藏时不该留着占位宽度');

// 窗口变窄：14 列挤不下 → 出现，占位宽 = 内容宽
needScroll(1400, 700);
check(top.style.display === 'block', '变窄后顶部条应出现，得到 display=' + JSON.stringify(top.style.display));
check(topInner(top).style.width === '1400px', '占位宽应为 1400px，得到 ' + JSON.stringify(topInner(top).style.width));

// 再变宽：挤得下 → 又收起
needScroll(700, 1000);
check(top.style.display === 'none', '变宽后顶部条应收起，得到 display=' + JSON.stringify(top.style.display));

// 再变窄：重新出现，占位宽按新的内容宽重算
needScroll(1500, 800);
check(top.style.display === 'block', '再次变窄后顶部条应出现，得到 display=' + JSON.stringify(top.style.display));
check(topInner(top).style.width === '1500px', '占位宽应为 1500px，得到 ' + JSON.stringify(topInner(top).style.width));

// 显示状态下两条的滚动位置保持一致（重排后先对齐，不出现错位）
top.scrollLeft = 0;
wrap.scrollLeft = 90;
check(top.scrollLeft === 90, '表格位置的 90 应同步到顶部条，得到 ' + top.scrollLeft);
`)
}

// TestUsageRecordsTopScrollPaneSwitch 挂载时分区还没显示（量出来宽高都是 0）→ 先
// 不占位；切到「用量」页（表格量出真实宽度）→ ResizeObserver 触发，顶部条才出现。
// 这一步只靠 window resize 是监测不到的。
func TestUsageRecordsTopScrollPaneSwitch(t *testing.T) {
	vm := mountUsageTopScroll(t)
	runTopScrollJS(t, vm, `
var wrap = wrapOf(recTable());
var top = topScrollEl();
check(observers.length === 1, '应在表格容器上装一个 ResizeObserver，得到 ' + observers.length);
check(observers[0].targets[0] === wrap, 'ResizeObserver 应观察表格容器（.us-table-wrap）');

// 挂载时分区 display:none：宽高均是 0，不显示
wrap.scrollWidth = 0; wrap.clientWidth = 0;
fireResize();
check(top.style.display === 'none', '分区未显示时顶部条不该占位，得到 ' + JSON.stringify(top.style.display));

// 切到「用量」页：容器量出真实宽度 → ResizeObserver 回调 → 顶部条出现
wrap.scrollWidth = 1300; wrap.clientWidth = 640;
fireObserve();
check(top.style.display === 'block', '切到用量页后顶部条应出现，得到 ' + JSON.stringify(top.style.display));
check(topInner(top).style.width === '1300px', '占位宽应为 1300px，得到 ' + JSON.stringify(topInner(top).style.width));
`)
}

// TestUsageRecordsTopScrollKeepsTable 加了顶部条之后，明细表本身照旧：14 列齐全、
// 速度列有值写「12.3 t/s」、老记录写「—」、悬停说明与行 hover 用的 title 都在。
func TestUsageRecordsTopScrollKeepsTable(t *testing.T) {
	vm := mountUsageTopScroll(t)
	runTopScrollJS(t, vm, `
var hs = headers();
check(hs.length === 14, '明细表应有 14 列，得到 ' + hs.length + '：' + hs.join(' | '));
var at = hs.indexOf('速度');
check(at > 0 && at === hs.indexOf('耗时') + 1, '速度列应紧跟「耗时」：' + hs.join(' | '));
var sp = cells(rowOf('m-speed'))[at];
check(sp.textContent === '12.3 t/s', '速度单元应为「12.3 t/s」，得到 ' + JSON.stringify(sp.textContent));
check((sp.title || '').indexOf('49 tok') >= 0, '速度单元的悬停说明应写清分子分母：' + sp.title);
var old = cells(rowOf('m-old'))[at];
check(old.textContent === '—', '老记录速度应为「—」，得到 ' + JSON.stringify(old.textContent));
var row = rowOf('m-speed');
check((row.title || '').indexOf('调用时段') >= 0, '行悬停说明（高峰/空闲）应保留：' + row.title);
check(!/NaN|Infinity/.test(text(recTable())), '明细出现了 NaN / Infinity');
`)
}
