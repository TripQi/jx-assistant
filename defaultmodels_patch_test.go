package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/dop251/goja"
)

func defaultPatchApp(t *testing.T) *App {
	t.Helper()
	a := newDefaultModelsApp(t)
	a.CfgPath = filepath.Join(t.TempDir(), "defaults.json")
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	a.Cfg.DispatchModel = ModelRef{Provider: "router", ProviderKey: "k2", Model: "claude-x"}
	if err := a.Cfg.Save(a.CfgPath); err != nil {
		t.Fatal(err)
	}
	return a
}

func defaultPatch(a *App, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	a.handleDefaultModelsSet(w, httptest.NewRequest("POST", "/api/default-models", strings.NewReader(body)))
	return w
}

func readDefaultPatchDisk(t *testing.T, a *App) (ModelRef, ModelRef) {
	t.Helper()
	raw, err := os.ReadFile(a.CfgPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Global   ModelRef `json:"defaultModel"`
		Dispatch ModelRef `json:"dispatchModel"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	keys, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("disk critical JSON: %s", keys)
	return cfg.Global, cfg.Dispatch
}

func TestDefaultModelsPatchPreservesUnedited(t *testing.T) {
	a := defaultPatchApp(t)
	g0, d0 := readDefaultPatchDisk(t, a)
	w := defaultPatch(a, `{"global":{"provider":"zhipu","model":"glm-5.3-flash"}}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	g, d := readDefaultPatchDisk(t, a)
	if g.Model != "glm-5.3-flash" || d != d0 {
		t.Fatalf("global-only lost other field: %+v %+v", g, d)
	}
	w = defaultPatch(a, `{"dispatch":{"provider":"router","providerKey":"k1","model":"shared"}}`)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	g2, d2 := readDefaultPatchDisk(t, a)
	if g2 != g || d2.ProviderKey != "k1" {
		t.Fatal("dispatch-only changed global")
	}
	// 空对象不再隐式清两项；显式 null 和 {} 继续支持原本的清空设计。
	if w = defaultPatch(a, `{}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if g3, d3 := readDefaultPatchDisk(t, a); g3 != g2 || d3 != d2 {
		t.Fatal("empty patch changed config")
	}
	if w = defaultPatch(a, `{"global":null}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if g3, d3 := readDefaultPatchDisk(t, a); !g3.Empty() || d3 != d2 {
		t.Fatal("explicit global clear affected dispatch")
	}
	if w = defaultPatch(a, `{"global":{"provider":"zhipu","model":"glm-5.3"},"dispatch":{}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if g3, d3 := readDefaultPatchDisk(t, a); g3 != a.withKey(g0) || !d3.Empty() {
		t.Fatal("legacy full request not supported")
	}
}

func TestDefaultModelsPatchConcurrentMerge(t *testing.T) {
	a := defaultPatchApp(t)
	readDefaultPatchDisk(t, a)
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make(chan *httptest.ResponseRecorder, 2)
	for _, body := range []string{`{"global":{"provider":"zhipu","model":"glm-5.3-flash"}}`, `{"dispatch":{"provider":"router","providerKey":"k1","model":"shared"}}`} {
		wg.Add(1)
		go func(body string) { defer wg.Done(); <-start; results <- defaultPatch(a, body) }(body)
	}
	close(start)
	wg.Wait()
	close(results)
	for w := range results {
		if w.Code != 200 {
			t.Fatalf("%d %s", w.Code, w.Body.String())
		}
	}
	g, d := readDefaultPatchDisk(t, a)
	if g.Model != "glm-5.3-flash" || d.ProviderKey != "k1" || d.Model != "shared" {
		t.Fatal("concurrent patches lost a field")
	}
	// 同字段按服务端串行处理顺序最后写入者胜；前端操作顺序由队列测试覆盖。
	if w := defaultPatch(a, `{"global":{"provider":"zhipu","model":"glm-5.3"}}`); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	g2, d2 := readDefaultPatchDisk(t, a)
	if g2.Model != "glm-5.3" || d2 != d {
		t.Fatal("last global patch failed or overwrote dispatch")
	}
}

func TestDefaultModelsPatchSaveFailureUnchanged(t *testing.T) {
	a := defaultPatchApp(t)
	g, d := a.Cfg.DefaultModel, a.Cfg.DispatchModel
	a.CfgPath = filepath.Join(t.TempDir(), "missing", "defaults.json")
	w := defaultPatch(a, `{"global":{"provider":"zhipu","model":"glm-5.3-flash"}}`)
	if w.Code != 500 || a.Cfg.DefaultModel != g || a.Cfg.DispatchModel != d {
		t.Fatalf("failed save changed memory: %d %s", w.Code, w.Body.String())
	}
}

// 执行真实设置页片段，不复制被测队列算法；最小 DOM 仅模拟本区块用到的 select 行为。
func TestDefaultModelsJSQueueAndLoadFailure(t *testing.T) {
	raw, err := os.ReadFile("web/app-settings.js")
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	start, end := strings.Index(src, "let dmCache = null;"), strings.Index(src, "function renderProvCards()")
	if start < 0 || end <= start {
		t.Fatal("default settings section missing")
	}
	vm := goja.New()
	harness := `
const nodes = {};
function select() {
 let v = '', html = '';
 return { options:[], style:{}, disabled:false, classList:{toggle(){}}, textContent:'',
 get value(){return v}, set value(x){v=this.options.some(o=>o.value===x)?x:''},
 get innerHTML(){return html}, set innerHTML(x){html=x;this.options=[];for(const m of x.matchAll(/<option value="([^"]*)"/g))this.options.push({value:m[1]});v=''},
 appendChild(o){this.options.push(o)} };
}
for(const id of ['set-default-model','set-default-thinking','set-dispatch-thinking','set-dispatch-mode','set-dispatch-model','set-default-model-eff','set-dispatch-model-eff'])nodes[id]=select();
nodes['set-dispatch-mode'].innerHTML='<option value="follow"></option><option value="custom"></option>';
const $=id=>nodes[id];
const state={models:[{providerId:'p',keyId:'k',modelId:'A'},{providerId:'p',keyId:'k',modelId:'C'},{providerId:'p',keyId:'k',modelId:'D'}]};
const modelKeyOf=(p,k,m)=>[p,k||'',m].join('|');
const esc=s=>String(s);
const document={createElement:()=>({})};
const THINK_LEVELS=['none','low','medium','high','xhigh','max'];
const THINK_LABELS={none:'None',low:'Low',medium:'Medium',high:'High',xhigh:'Xhigh',max:'Max'};
const normThinkLevel=s=>s||'';
const requests=[], gets=[];
let getFail=false, holdGet=false;
const api=(path,opts)=>{if(!opts){if(holdGet)return new Promise(resolve=>gets.push(resolve));return getFail?Promise.reject(new Error('offline')):Promise.resolve(dmCache)}return new Promise((resolve,reject)=>requests.push({body:JSON.parse(opts.body),resolve,reject}))};
const loadModels=()=>Promise.resolve();
const alert=()=>{};
function expect(v,s){if(!v)throw new Error(s)}
function view(g,d){return {global:g,dispatch:{mode:d?'custom':'follow',ref:d||g}}}
const A={provider:'p',providerKey:'k',model:'A'}, B={provider:'gone',providerKey:'lost',model:'B'}, C={provider:'p',providerKey:'k',model:'C'}, D={provider:'p',providerKey:'k',model:'D'};
`
	if _, err = vm.RunString(harness + "\n" + src[start:end]); err != nil {
		t.Fatal(err)
	}
	run := func(s string) {
		t.Helper()
		if _, err := vm.RunString(s); err != nil {
			t.Fatal(err)
		}
	}
	run(`dmCache=view(A,B);dmReady=true;renderDefaultModels();expect($('set-dispatch-model').value==='gone|lost|B','missing model ref was cleared');holdGet=true;loadDefaultModels();expect(gets.length===1,'delayed GET missing');`)
	run(`$('set-default-model').value='p|k|C';saveDefaultModels('global');expect(requests.length===1,'first request missing');expect(!('dispatch' in requests[0].body),'global request was full replacement');loadDefaultModels();expect(gets.length===1,'GET was started while saving');`)
	run(`$('set-dispatch-mode').value='follow';saveDefaultModels('dispatch');$('set-default-model').value='p|k|D';saveDefaultModels('global');expect(requests.length===1,'requests were not serialized');requests[0].resolve(view(C,B));`)
	run(`expect(requests.length===2,'pending patch not sent');expect(requests[1].body.global.model==='D' && requests[1].body.dispatch.thinking==='' && !requests[1].body.dispatch.provider,'latest intent lost');expect($('set-default-model').value==='p|k|D','old response reset latest UI');requests[1].resolve(view(D,null));`)
	run(`expect(!dmSaving,'queue did not finish');expect($('set-default-model').value==='p|k|D','last operation did not win');gets[0](view(A,B));`)
	run(`expect($('set-default-model').value==='p|k|D','delayed old GET rolled back saved choice');holdGet=false;getFail=true;loadDefaultModels();`)
	run(`expect(!dmReady && $('set-default-model').disabled && $('set-dispatch-mode').disabled,'GET failure left controls editable');saveDefaultModels('global');expect(requests.length===2,'GET failure allowed save');`)
}
