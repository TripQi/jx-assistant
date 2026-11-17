package main

// 设置页「派发任务默认模型（按用途）」三行的前端回归：直接跑 web/app-settings.js
// 的默认模型片段（不复制被测逻辑），验证
//   - 三行按 dispatchPurposes 回填（模式 / 模型 / 档位分开）；
//   - 脏 mode 显示成「配置非法：<原值>」并置红，且不会被当成 follow 提交；
//   - 方案/审查保存时显式带 mode，执行/通用沿用旧请求形状（只带档位＝跟随）。

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

func TestDefaultModelsJSPurposeRows(t *testing.T) {
	// 与 token_combo_test 同一套做法：允许环境变量指向副本，mutation 验证用（不改工作树）。
	path := "web/app-settings.js"
	if p := os.Getenv("JX_DM_SETTINGS_SOURCE"); p != "" {
		path = p
	}
	raw, err := os.ReadFile(path)
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
 let v = '', html = '', className = '';
 return { options:[], style:{}, disabled:false, textContent:'',
  classList:{ toggle(c,on){ const s=new Set(className.split(' ')); if(on)s.add(c);else s.delete(c); className=[...s].join(' '); } },
  get className(){return className}, set className(x){className=x},
  get value(){return v}, set value(x){v=this.options.some(o=>o.value===x)?x:''},
  get innerHTML(){return html}, set innerHTML(x){html=x;this.options=[];for(const m of x.matchAll(/<option value="([^"]*)"[^>]*>([^<]*)</g))this.options.push({value:m[1],textContent:m[2]});v=''},
  appendChild(o){this.options.push(o)} };
}
const $=id=>nodes[id]||(nodes[id]=select());
const state={models:[{providerId:'p',keyId:'k',modelId:'P'},{providerId:'p',keyId:'k',modelId:'G'},{providerId:'p',keyId:'k',modelId:'R'}]};
const modelKeyOf=(p,k,m)=>[p,k||'',m].join('|');
const esc=s=>String(s);
const document={createElement:()=>({})};
const THINK_LEVELS=['none','low','medium','high','xhigh','max'];
const THINK_LABELS={none:'None',low:'Low',medium:'Medium',high:'High',xhigh:'Xhigh',max:'Max'};
const normThinkLevel=s=>s||'';
const requests=[];
const api=(path,opts)=>{ if(!opts) return Promise.resolve(dmCache); return new Promise((resolve)=>{requests.push({body:JSON.parse(opts.body),resolve})}); };
const loadModels=()=>Promise.resolve();
const alert=()=>{};
function expect(v,s){if(!v)throw new Error(s)}
const P={provider:'p',providerKey:'k',model:'P'}, R={provider:'p',providerKey:'k',model:'R'}, G={provider:'p',providerKey:'k',model:'G'};
const DM={global:G,dispatch:{mode:'custom',ref:P},dispatchPurposes:[
 {purpose:'execute',label:'执行/通用',mode:'custom',thinking:'low',ref:P},
 {purpose:'plan',label:'方案设计',mode:'follow',thinking:'',ref:P},
 {purpose:'review',label:'审查/审计',mode:'custom',thinking:'xhigh',ref:R},
]};
`
	if _, err := vm.RunString(harness + "\n" + src[start:end]); err != nil {
		t.Fatal(err)
	}
	run := func(s string) {
		t.Helper()
		if _, err := vm.RunString(s); err != nil {
			t.Fatal(err)
		}
	}

	run(`dmCache=DM;dmReady=true;renderDefaultModels();`)
	// 三行按用途回填：模式、模型（跟随态也回填生效模型，切 custom 就不用手选）、档位各自独立。
	run(`expect($('set-dispatch-execute-mode').value==='custom','execute mode');expect($('set-dispatch-execute-model').value==='p|k|P','execute model');expect($('set-dispatch-execute-thinking').value==='low','execute thinking');`)
	run(`expect($('set-dispatch-plan-mode').value==='follow','plan mode');expect($('set-dispatch-plan-model').value==='p|k|P','plan 跟随时回填生效模型');expect($('set-dispatch-plan-thinking').value==='','plan thinking 应空');`)
	run(`expect($('set-dispatch-review-mode').value==='custom','review mode');expect($('set-dispatch-review-model').value==='p|k|R','review model');expect($('set-dispatch-review-thinking').value==='xhigh','review thinking 应独立');`)
	// 方案设计行切到 custom 后，模型下拉仍停在生效模型上（用户不必重选）
	run(`expect($('set-dispatch-plan-model').style.display==='none','plan 跟随时应隐藏模型下拉');$('set-dispatch-review-mode').value='follow';`)

	// 脏 mode：原值显示成「配置非法：跟随」并置红；此时保存不发请求。
	run(`dmCache=JSON.parse(JSON.stringify(DM));dmCache.dispatchPurposes[1].mode='跟随';dmCache.dispatchPurposes[1].ref={};renderDefaultModels();`)
	run(`expect($('set-dispatch-plan-mode').value==='__bad__','脏 mode 应停在非法项');expect($('set-dispatch-plan-mode').className.includes('bad'),'脏 mode 应置红');expect($('set-dispatch-plan-mode').options.some(o=>o.textContent==='配置非法：跟随'),'应显示原值');`)
	run(`saveDefaultModels('dispatchPlan');expect(requests.length===0,'脏 mode 不该提交');`)

	// 方案/审查保存：显式带 mode；执行/通用沿用旧形状（follow 只带档位）。
	run(`dmReady=true;saveDefaultModels('dispatchReview', true);expect(requests.length===1,'review 请求缺失');expect(requests[0].body.dispatchReview.mode==='custom' && requests[0].body.dispatchReview.model==='R' && requests[0].body.dispatchReview.thinking==='xhigh','review patch 不对');`)
	run(`requests[0].resolve(DM);`)
	run(`$('set-dispatch-plan-mode').value='follow';saveDefaultModels('dispatchPlan');expect(requests.length===2,'plan 请求缺失');expect(requests[1].body.dispatchPlan.mode==='follow' && !requests[1].body.dispatchPlan.provider,'plan follow 不该带模型');`)
	run(`requests[1].resolve(DM);`)
	run(`$('set-dispatch-execute-mode').value='follow';saveDefaultModels('dispatch');expect(requests.length===3,'execute 请求缺失');expect(!('mode' in requests[2].body.dispatch) && !('provider' in requests[2].body.dispatch),'执行/通用应沿用旧请求形状');`)

	// P8：接口没有返回用途配置（或缺某一项）时不静默回落「跟随」，而是明确报错并禁用该行；
	// 配置未加载时也不允许把该行当 follow 提交。
	run(`dmCache={global:G,dispatch:{mode:'follow',ref:G}};dmReady=true;renderDefaultModels();expect($('set-dispatch-plan-mode').value==='__missing__','缺用途配置应明确报错');expect($('set-dispatch-plan-mode').className.includes('bad'),'应置红');expect($('set-dispatch-plan-mode').options.some(o=>o.textContent==='配置未加载'),'应显示可读原因');expect($('set-dispatch-plan-mode').disabled && $('set-dispatch-plan-model').disabled && $('set-dispatch-plan-thinking').disabled,'该行所有控件应禁用');saveDefaultModels('dispatchPlan');expect(requests.length===3,'配置未加载不该提交');`)
	// 即使程序化把下拉改成一个可提交的值，也不允许把它（实际已存在于服务端的真实配置）
	// 清成 follow（N4）。
	run(`const m=$('set-dispatch-plan-mode');m.options.push({value:'follow',textContent:'跟随执行/通用'});m.value='follow';dmSaving=false;saveDefaultModels('dispatchPlan');expect(requests.length===3,'配置未加载时程序化改值也不该提交');`)
	run(`dmCache={global:G,dispatch:{mode:'follow',ref:G},dispatchPurposes:[{purpose:'execute',label:'执行/通用',mode:'follow',thinking:'',ref:G}]};renderDefaultModels();expect($('set-dispatch-execute-mode').value==='follow','execute 正常回填');expect($('set-dispatch-review-mode').value==='__missing__','缺该项应明确报错');`)
}
