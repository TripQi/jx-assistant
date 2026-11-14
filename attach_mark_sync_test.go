package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// Run the production composer functions with a small DOM, including native-value
// writes (user input), the value setter (draft restoration), and queued RAF work.
func newAttachMarkVM(t *testing.T) *goja.Runtime {
	t.Helper()
	raw, err := os.ReadFile("web/app-chat.js")
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	_, err = vm.RunString(`
const revoked = [], frames = [];
let attachSeq = 0;
const ATTACH_MAX_COUNT = 10, ATTACH_MAX_BYTES = 32000000;
function assert(ok, message) { if (!ok) throw Error(message); }
class Node {
  constructor() {
    this.children = []; this.style = {}; this.events = {}; this.scrollTop = 0;
    this.classes = new Set();
    this.classList = {
      add: (c) => this.classes.add(c),
      contains: (c) => this.classes.has(c),
      toggle: (c, on) => on ? this.classes.add(c) : this.classes.delete(c)
    };
  }
  set textContent(v) { this.text = v; this.children = []; }
  get textContent() { return this.text || ''; }
  set innerHTML(v) { this.html = v; this.children = []; }
  appendChild(n) { this.children.push(n); }
  addEventListener(name, fn) { (this.events[name] || (this.events[name] = [])).push(fn); }
  dispatchEvent(e) { for (const fn of this.events[e.type] || []) fn(e); }
}
class HTMLTextAreaElement extends Node {
  constructor() { super(); this._value = ''; this.selectionStart = this.selectionEnd = 0; }
  get value() { return this._value; }
  set value(v) { this._value = String(v); }
  closest() { return this.wrap; }
  focus() { document.activeElement = this; }
  setSelectionRange(s, e) { this.selectionStart = s; this.selectionEnd = e; }
  setRangeText(v, s, e) { this._value = this._value.slice(0, s) + v + this._value.slice(e); this.setSelectionRange(s + v.length, s + v.length); }
}
class Event { constructor(type, options) { this.type = type; Object.assign(this, options); } }
const document = {
  activeElement: null, createElement: () => new Node(),
  createTextNode: (s) => ({ textContent: s }), addEventListener() {}, execCommand: () => false
};
const window = { addEventListener() {} };
const URL = { createObjectURL: () => 'blob:' + (attachSeq + 1), revokeObjectURL: (url) => revoked.push(url) };
function requestAnimationFrame(fn) { frames.push(fn); return frames.length; }
function drain() { while (frames.length) frames.shift()(); }
function getComputedStyle() { return {}; }
function autoGrow() {}
function syncComposerPad() {}
function persistMainDraftLS() {}
function persistSubDraftLS() {}
function fmtBytes(n) { return String(n); }
function viewEl(view, name) { return view[name]; }
function makeView() {
  const input = new HTMLTextAreaElement(), backdrop = new Node();
  input.wrap = { querySelector: () => backdrop };
  return { input, backdrop, state: {}, attachments: new Node(), hint: new Node() };
}
const mainView = makeView(), subView = makeView();
function image(name) { return { name: name || 'photo.png', size: 100, type: 'image/png' }; }
function editValue(view, value, options) {
  // Browsers update the native textarea value before dispatching input.
  Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value').set.call(view.input, value);
  view.input.dispatchEvent(new Event('input', options));
  drain();
}
`)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, name := range []string{
		"insertInputMark", "fileClipName", "attachMarkOf", "inputMarkRanges", "markBackdropSegments",
		"setupInputMarks", "attachmentsOf", "pasteImageName", "stagePaths", "stageFiles",
		"clearAttachments", "attachCard", "removeAttachment", "renderAttachments",
	} {
		start := strings.Index(src, "function "+name+"(")
		if start < 0 {
			t.Fatalf("missing function %s", name)
		}
		end := strings.Index(src[start:], "\n}")
		if end < 0 {
			t.Fatalf("missing function end %s", name)
		}
		if _, err := vm.RunString(src[start : start+end+2]); err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
	}
	if _, err := vm.RunString(`setupInputMarks(mainView); setupInputMarks(subView);`); err != nil {
		t.Fatal(err)
	}
	return vm
}

func runAttachMark(t *testing.T, script string) {
	t.Helper()
	if _, err := newAttachMarkVM(t).RunString(script); err != nil {
		t.Fatal(err)
	}
}

func TestAttachMarkSyncImageRemoval(t *testing.T) {
	runAttachMark(t, `
for (const view of [mainView, subView]) {
  const a = stageFiles([image()], view)[0]; drain();
  assert(view.input.value === a.mark, 'staged image inserts its mark');
  assert(view.attachments.children.length === 1, 'staged image renders one DOM card');
  const url = a.previewUrl;
  editValue(view, '');
  assert(attachmentsOf(view).length === 0, 'deleting the mark empties the attachment list');
  assert(view.attachments.children.length === 0, 'deleting the mark removes the DOM card');
  assert(view.attachments.classList.contains('hidden'), 'empty attachment track is hidden');
  assert(revoked.filter(x => x === url).length === 1, 'thumbnail objectURL is revoked exactly once');
}
`)
}

func TestAttachMarkSyncSameName(t *testing.T) {
	runAttachMark(t, `
const [a, b] = stageFiles([image('wx_camera.jpg'), image('wx_camera.jpg')], mainView); drain();
assert(mainView.input.value === a.mark + b.mark, 'same-name images insert two identical marks');
editValue(mainView, b.mark);
assert(attachmentsOf(mainView).length === 1 && attachmentsOf(mainView)[0] === a, 'one remaining mark keeps exactly one same-name attachment');
assert(mainView.attachments.children.length === 1, 'one remaining same-name attachment has one DOM card');
assert(revoked.includes(b.previewUrl) && !revoked.includes(a.previewUrl), 'only the unclaimed same-name thumbnail is revoked');
`)
}

func TestAttachMarkSyncTypedQuotes(t *testing.T) {
	runAttachMark(t, `
const a = stageFiles([image()], mainView)[0]; drain();
// A pre-existing unmarked attachment must never be interpreted as a deleted mark.
const legacy = { id: 'legacy', name: 'old.txt', status: 'staged' };
attachmentsOf(mainView).push(legacy);
editValue(mainView, a.mark + '“ordinary hand-typed text”');
editValue(mainView, a.mark);
assert(attachmentsOf(mainView).length === 2, 'adding and removing ordinary typed quotes removes no attachments');
assert(attachmentsOf(mainView).includes(legacy), 'an attachment that never inserted a mark is not removed');
assert(revoked.length === 0, 'ordinary typed quotes revoke no thumbnail URLs');
`)
}

func TestComposerAttachMarkSyncBoundaries(t *testing.T) {
	runAttachMark(t, `
const a = stageFiles([image()], mainView)[0];
const b = stageFiles([image('other.png')], subView)[0]; drain();
let aborted = 0;
a.status = 'uploading'; a._abort = { abort() { aborted++; } };
mainView.input.dispatchEvent(new Event('compositionstart'));
editValue(mainView, '');
assert(attachmentsOf(mainView).length === 1 && aborted === 0, 'IME composition does not remove or abort attachments');
mainView.input.dispatchEvent(new Event('compositionend')); drain();
assert(attachmentsOf(mainView).length === 0 && aborted === 1, 'compositionend removes missing mark and aborts its upload');
assert(attachmentsOf(subView)[0] === b, 'main composer edits do not remove sub-composer attachments');
editValue(mainView, a.mark);
assert(attachmentsOf(mainView).length === 0 && mainView.attachments.children.length === 0, 'undo-restored text creates no ghost attachment');
// Programmatic draft switches refresh the backdrop, not attachment ownership.
subView.input.value = ''; drain();
assert(attachmentsOf(subView)[0] === b, 'programmatic draft restoration preserves staged attachments');
subView.input.value = b.mark; drain();
const c = stageFiles([image('third.png')], subView)[0]; drain();
editValue(subView, '');
assert(attachmentsOf(subView).length === 0 && subView.attachments.classList.contains('hidden'), 'select-all deletion removes all marked attachments and hides the track');
// Atomic Backspace/Delete both go through the input-event synchronization.
for (const key of ['Backspace', 'Delete']) {
  const item = stageFiles([image()], mainView)[0]; drain();
  mainView.input.value = item.mark; drain();
  const p = key === 'Backspace' ? item.mark.length : 0;
  mainView.input.setSelectionRange(p, p);
  mainView.input.dispatchEvent({ type: 'keydown', key, preventDefault() {} }); drain();
  assert(attachmentsOf(mainView).length === 0, 'atomic ' + key + ' removes its attachment');
}
stagePaths(['D:/report.txt'], mainView); drain();
editValue(mainView, '');
assert(attachmentsOf(mainView).length === 0, 'path attachments also synchronize on mark removal');
const final = stageFiles([image()], mainView)[0]; drain();
const mark = final.mark;
clearAttachments(mainView); drain();
assert(mainView.input.value === mark && attachmentsOf(mainView).length === 0, 'send-success clearAttachments behavior remains unchanged');
`)
}
