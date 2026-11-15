package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// 用确定性时钟与最小 DOM 执行生产预览代码，不复制待测逻辑。
func runImageZoom(t *testing.T, script string) {
	t.Helper()
	raw, err := os.ReadFile("web/app-chat.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "const ZOOM_SEL =")
	if start < 0 {
		t.Fatal("missing image zoom region start")
	}
	end := strings.Index(src[start:], "\n$('btn-send').onclick")
	if end < 0 {
		t.Fatal("missing image zoom region end")
	}
	vm := goja.New()
	_, err = vm.RunString(`
function assert(ok, message) { if (!ok) throw Error(message); }
let now = 0, seq = 0;
const timers = new Map();
function setTimeout(fn, ms) { const id = ++seq; timers.set(id, {fn, at: now + ms}); return id; }
function clearTimeout(id) { timers.delete(id); }
function tick(ms) {
 const until = now + ms;
 for (;;) {
  let id = 0, at = Infinity;
  for (const [k, v] of timers) if (v.at < at) { id = k; at = v.at; }
  if (!id || at > until) break;
  now = at; const task = timers.get(id); timers.delete(id); task.fn();
 }
 now = until;
}
class Node {
 constructor() {
  this.events = {}; this.children = []; this.style = {}; this.isConnected = true;
  this.complete = false; this.naturalWidth = 0; this.offsetWidth = 120; this.offsetHeight = 100;
  this.classes = new Set();
  this.classList = {add: c => this.classes.add(c), remove: c => this.classes.delete(c)};
 }
 addEventListener(name, fn) { (this.events[name] || (this.events[name] = [])).push(fn); }
 removeEventListener(name, fn) { this.events[name] = (this.events[name] || []).filter(x => x !== fn); }
 dispatchEvent(e) {
  for (const fn of (this.events[e.type] || []).slice()) fn(e);
  if (this['on' + e.type]) this['on' + e.type](e);
 }
 appendChild(n) { this.children.push(n); n.parentNode = this; }
 replaceChild(n, old) { this.children[this.children.indexOf(old)] = n; n.parentNode = this; old.parentNode = null; }
 removeAttribute(k) { if (k === 'src') this.src = ''; }
 getAttribute(k) { return this[k] || null; }
 closest() { return this.image ? this : null; }
 contains(n) { return n === this || this.children.some(c => c.contains(n)); }
 matches() { return !!this.hover; }
 getBoundingClientRect() { return {left: 20, top: 20, right: 60, bottom: 60, width: 40, height: 40}; }
}
const document = new Node(); document.body = new Node(); document.documentElement = {};
document.createElement = () => new Node();
const window = new Node(); window.innerWidth = 1000; window.innerHeight = 800;
function image(src) { const n = new Node(); n.image = true; n.src = src || 'photo.png'; return n; }
function emit(type, target, extra) {
 const e = Object.assign({type, target, pointerType: 'mouse', pointerId: 1, clientX: 20, clientY: 20,
  preventDefault() { this.prevented = true; }, stopPropagation() { this.stopped = true; }}, extra || {});
 document.dispatchEvent(e); return e;
}
function hover(img) { img.hover = true; emit('pointerover', img); tick(340); }
function loaded(pic) { pic.complete = true; pic.naturalWidth = 400; pic.dispatchEvent({type: 'load'}); }
function down(img, id) { return emit('pointerdown', img, {pointerType: 'touch', pointerId: id || 7}); }
`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vm.RunString(src[start:start+end] + "\nsetupImageZoom();\n" + script); err != nil {
		t.Fatal(err)
	}
}

func TestImageZoomL04LoadingCancellation(t *testing.T) {
	for _, action := range []string{"leave", "scroll", "resize", "remove", "blur", "escape", "hidden"} {
		t.Run(action, func(t *testing.T) {
			runImageZoom(t, `
const img = image(); hover(img); const pending = zoomPic;
assert(!zoomShown, 'fixture must still be loading');
const action = '`+action+`';
if (action === 'leave') { img.hover = false; emit('pointerout', img); tick(140); }
if (action === 'scroll') emit('scroll', document);
if (action === 'resize') window.dispatchEvent({type: 'resize'});
if (action === 'remove') img.isConnected = false;
if (action === 'blur') window.dispatchEvent({type: 'blur'});
if (action === 'escape') emit('keydown', document, {key: 'Escape'});
if (action === 'hidden') { document.hidden = true; emit('visibilitychange', document); }
loaded(pending);
assert(!zoomShown, action + ': cancelled loading must not reveal');
assert(zoomAnchor === null, action + ': cancelled anchor must be cleared');
`)
		})
	}
}

func TestImageZoomL04StaleRequest(t *testing.T) {
	runImageZoom(t, `
const a = image('a.png'), b = image('b.png');
hover(a); const oldPic = zoomPic;
const staleLoad = (oldPic.events.load || []).slice(); const staleOnload = oldPic.onload;
a.hover = false; emit('pointerout', a); hover(b);
oldPic.complete = true; oldPic.naturalWidth = 400;
for (const fn of staleLoad) fn(); if (staleOnload) staleOnload();
assert(!zoomShown, 'expired request must not reveal the new anchor');
loaded(zoomPic);
assert(zoomShown && zoomAnchor === b, 'current request must still reveal');
`)
}

func TestImageZoomL05GestureClickGuard(t *testing.T) {
	t.Run("other-button", func(t *testing.T) {
		runImageZoom(t, `
const img = image(), button = new Node(); down(img); tick(450);
emit('pointerup', img, {pointerType:'touch', pointerId:7});
const click = emit('click', button, {pointerId:9});
assert(!click.prevented && !click.stopped, 'unrelated button first click must pass within 700ms');
`)
	})
	t.Run("long-hold", func(t *testing.T) {
		runImageZoom(t, `
const img = image(); down(img); tick(2000);
emit('pointerup', img, {pointerType:'touch', pointerId:7});
const click = emit('click', img, {pointerId:7});
assert(click.prevented && click.stopped, 'original long-hold compatibility click must be suppressed');
assert(!emit('click', img, {pointerId:7}).prevented, 'guard must consume only one click');
`)
	})
	t.Run("legacy-click-and-pointer-identity", func(t *testing.T) {
		runImageZoom(t, `
const img = image(); down(img); tick(450);
emit('pointerup', img, {pointerType:'touch', pointerId:7});
assert(!emit('click', img, {pointerId:8}).prevented, 'different pointer must pass');
assert(emit('click', img, {pointerId:undefined}).prevented, 'legacy MouseEvent from original gesture must be suppressed');
`)
	})
	t.Run("new-gesture-and-short-tap", func(t *testing.T) {
		runImageZoom(t, `
const img = image(); down(img); tick(450);
emit('pointerup', img, {pointerType:'touch', pointerId:7});
down(img); tick(100); emit('pointerup', img, {pointerType:'touch', pointerId:7});
assert(!emit('click', img, {pointerId:7}).prevented, 'new short gesture on same image must pass');
`)
	})
}

func TestImageZoomL06PressCancellation(t *testing.T) {
	for _, action := range []string{"scroll", "resize", "blur", "cancel", "move"} {
		t.Run(action, func(t *testing.T) {
			runImageZoom(t, `
const img = image(); down(img); tick(200);
const action = '`+action+`';
if (action === 'scroll') emit('scroll', document);
if (action === 'resize' || action === 'blur') window.dispatchEvent({type:action});
if (action === 'cancel') emit('pointercancel', img, {pointerType:'touch', pointerId:7});
if (action === 'move') emit('pointermove', img, {pointerType:'touch', pointerId:7, clientX:40});
assert(zoomPress === null, action + ': must clear active press immediately');
tick(1000);
assert(!zoomAnchor && !zoomShown, action + ': cancelled press must not start preview');
assert(!emit('click', img, {pointerId:7}).prevented, action + ': cancelled press must not arm click guard');
`)
		})
	}
}

func TestImageZoomCancelledLongPress(t *testing.T) {
	runImageZoom(t, `
for (const action of ['scroll', 'resize', 'cancel']) {
 const img = image(); down(img); tick(450); const pending = zoomPic;
 if (action === 'scroll') emit('scroll', document);
 if (action === 'resize') window.dispatchEvent({type:'resize'});
 if (action === 'cancel') emit('pointercancel', img, {pointerType:'touch', pointerId:7});
 loaded(pending);
 assert(!zoomShown && !zoomPress, action + ': fired long press must be cancelled too');
 assert(!emit('click', img, {pointerId:7}).prevented, action + ': cancellation clears gesture guard');
}
`)
}

func TestImageZoomNormalLifecycle(t *testing.T) {
	runImageZoom(t, `
const img = image(); hover(img); loaded(zoomPic);
assert(zoomShown, 'hover and completed load must show');
img.hover = false; emit('pointerout', img); tick(100);
zoomBox.hover = true; zoomBox.dispatchEvent({type:'pointerenter'}); tick(200);
assert(zoomShown, 'moving into preview must preserve it');
zoomBox.hover = false; zoomBox.dispatchEvent({type:'pointerleave'}); tick(140);
assert(!zoomShown, 'leaving preview must hide');
down(img); tick(450); loaded(zoomPic);
assert(zoomShown, 'long press must still show');
emit('pointerup', img, {pointerType:'touch', pointerId:7});
assert(zoomShown, 'releasing long press must leave preview open');
`)
}
