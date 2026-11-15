package main

import (
	"github.com/dop251/goja"
	"os"
	"strings"
	"testing"
)

func runTokenComboRegression(t *testing.T, script string) {
	t.Helper()
	path := "web/app-settings.js"
	if p := os.Getenv("JX_P2_SETTINGS_SOURCE"); p != "" {
		path = p
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start := strings.Index(src, "const TOKEN_PRESETS =")
	end := strings.Index(src, "// Key 草稿")
	if start < 0 || end <= start {
		t.Fatal("missing token combo region")
	}
	vm := goja.New()
	_, err = vm.RunString(`
Number.prototype.toLocaleString = function() { return String(this); }; // Goja lacks browser Intl formatting.
function assert(ok, message) { if (!ok) throw Error(message); }
class Node {
 constructor() {
  this.children=[]; this.events={}; this.style={}; this.dataset={}; this.attrs={};
  this.value=''; this.isConnected=true; this.offsetHeight=180;
  this.classList={contains:c=>this.className.split(' ').includes(c),toggle:(c,on)=>{
   const s=new Set(this.className.split(' ')); if(on)s.add(c);else s.delete(c);this.className=[...s].join(' ');
  }};
 }
 appendChild(n) { this.children.push(n); n.parent=this; return n; }
 remove() { this.parent.children=this.parent.children.filter(n=>n!==this); }
 setAttribute(k,v) { this.attrs[k]=v; }
 addEventListener(k,fn) { (this.events[k]||(this.events[k]=[])).push(fn); }
 removeEventListener(k,fn) { this.events[k]=(this.events[k]||[]).filter(f=>f!==fn); }
 contains(n) { return n===this || this.children.some(c=>c.contains(n)); }
 focus() { document.activeElement=this; }
 scrollIntoView() {}
 getBoundingClientRect() { return {left:20,top:20,bottom:50,width:200}; }
}
const document=new Node(); document.body=new Node(); document.documentElement={clientWidth:1000,clientHeight:800};
document.createElement=()=>new Node(); document.activeElement=document.body;
const window=new Node();
function event(key) { return {key,preventDefault(){},stopPropagation(){}}; }
function press(key) { for(const fn of document.activeElement.events.keydown||[]) fn(event(key)); }
function fixture(value) {
 const changes=[]; const wrap=tokenCombo('maxOutput',value,v=>changes.push(v)); document.body.appendChild(wrap);
 return {wrap,input:wrap.children[0],button:wrap.children[1],changes};
}
function clickArrow(f) { f.button.onmousedown(event()); f.button.onclick(event()); }
let setNav={open:true,pane:'models',level:2},setHistDepth=2, renders=0;
function isNarrow(){return false;} function setHistPush(){} function renderNav(){renders++;}
`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vm.RunString(src[start:end]); err != nil {
		t.Fatal(err)
	}
	ns := strings.Index(src, "function navGoto(")
	ne := strings.Index(src[ns:], "\n}")
	if _, err = vm.RunString(src[ns:ns+ne+2] + "\n" + script); err != nil {
		t.Fatal(err)
	}
}

func TestTokenComboL07MouseKeyboard(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"focus", `const f=fixture(32768);clickArrow(f);assert(document.activeElement===f.input,'mouse open must focus input');`},
		{"selected-enter", `const f=fixture(32768);clickArrow(f);press('Enter');assert(f.changes[0]==='32768','Enter must commit current preset after mouse open');`},
		{"custom-first-enter", `const f=fixture(12345);clickArrow(f);press('Enter');assert(f.changes[0]==='4096','custom value must activate first preset without rewriting until Enter');`},
		{"selected-down", `const f=fixture(32768);clickArrow(f);press('ArrowDown');press('Enter');assert(f.changes[0]==='65536','ArrowDown must advance from selected preset');`},
		{"active-on-open", `const f=fixture(32768);clickArrow(f);const pop=document.body.children[1];assert(pop.children[3].classList.contains('active'),'current preset must be active on mouse open');assert(f.changes.length===0,'opening must not commit');`},
		{"keyboard-up", `const f=fixture(12345);f.input.focus();press('ArrowUp');press('Enter');assert(f.changes[0]==='512000','keyboard ArrowUp must retain last-item opening');`},
		{"escape-tab-toggle", `const f=fixture(0);clickArrow(f);press('Escape');assert(tokenComboOpen===null,'Escape closes');clickArrow(f);press('Tab');assert(tokenComboOpen===null,'Tab closes');clickArrow(f);clickArrow(f);assert(tokenComboOpen===null,'arrow toggles closed');`},
	} {
		t.Run(tc.name, func(t *testing.T) { runTokenComboRegression(t, tc.script) })
	}
}

func TestTokenComboL08ProgrammaticNavigation(t *testing.T) {
	runTokenComboRegression(t, `
const f=fixture(32768);clickArrow(f);assert(tokenComboOpen!==null,'fixture must open');
navGoto('about');assert(tokenComboOpen===null,'programmatic navGoto must close body popup');
assert(document.body.children.length===1,'navigation must remove body popup');
assert((document.events.mousedown||[]).length===0,'navigation must detach document listener');
assert((window.events.resize||[]).length===0 && (window.events.scroll||[]).length===0,'navigation must detach placement listeners');
assert(setNav.pane==='about' && renders===1,'navigation must still render destination');
`)
}
