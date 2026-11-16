package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// 设置页导航图标 / 项目下拉的真实 DOM 断言：跑真组件 client.js + 最小 DOM。
// 导航图标的规格直接拿 web/index.html 里的静态导航项做基线比对，不另立标准。
const projectSettingsHarness = `
class Element {
 constructor(tag) { this.tagName=tag; this.children=[]; this.style={}; this.attrs={}; this.value=''; this.textContent=''; this.isConnected=true; this.hidden=undefined; this.disabled=false; }
 appendChild(e) { this.children.push(e); e.parent=this; return e; }
 replaceChildren() { this.children=[]; this.textContent=''; }
 remove() {}
 setAttribute(k,v) { this.attrs[k]=v; }
 getAttribute(k) { return this.attrs[k]; }
 closest(s) { if(s==='#settings') return mountTarget; if(s==='#cxsession') return null; if(s==='[data-pk-readonly]') { for(var e=this;e;e=e.parent) if(e.attrs['data-pk-readonly']!==undefined) return e; } return null; }
 querySelectorAll(s) { return all(this).filter(e=>s.split(',').includes(e.tagName)); }
 focus() { this.focused=true; }
}
function all(e) { return e.children.reduce((a,c)=>a.concat([c],all(c)),[]); }
function text(e) { return (e.textContent||'')+e.children.map(text).join(''); }
// 极简片段解析：只为把导航图标的 <svg>/<path> 还原成节点树（app-settings.js 用 innerHTML 拼导航项）
function parseInto(host, html) {
 var stack=[host], re=/<(\/?)([a-zA-Z]+)((?:\s+[\w:-]+="[^"]*")*)\s*(\/?)>|([^<>]+)/g, m;
 while((m=re.exec(html))) {
  if(m[5]!==undefined) { var t=m[5].trim(); if(t) { var tn=new Element('#text'); tn.textContent=t; stack[stack.length-1].appendChild(tn); } continue; }
  if(m[1]) { if(stack.length>1) stack.pop(); continue; }
  var el=new Element(m[2].toLowerCase());
  m[3].replace(/([\w:-]+)="([^"]*)"/g, function(_,k,v){ el.attrs[k]=v; });
  stack[stack.length-1].appendChild(el);
  if(!m[4]) stack.push(el);
 }
}
var mountTarget=new Element('div'), captured=null;
var document={createElement:t=>new Element(t),createElementNS:(ns,t)=>new Element(t),body:new Element('body')};
var window={confirm:()=>true,prompt:()=>'',addEventListener:()=>{},removeEventListener:()=>{}};
var CX={register:d=>d.init(),registerSettings:d=>{captured=d;if(__MOUNT__) d.mount(mountTarget);}};
var JX={}; window.JX=JX;
var registryMountCount=0;
var registryOpen=false;
// 项目登记归宿主视图（web/project-registry.js）：从这里建项目，验证两页共用同一套登记。
function addBinding(path, kind) {
 var box=all(mountTarget).find(e=>e.tagName==='input'&&e.attrs['aria-label']==='新绑定路径');
 if(!box) throw Error('binding input missing: '+text(mountTarget).slice(0,400));
 var line=box.parent, sel=line.children.filter(c=>c.tagName==='select')[0];
 if(sel&&kind) sel.value=kind;
 box.value=path; clickLabel('添加绑定');
}
function createProject(name, path) {
 if(!registryOpen) { clickLabel('项目登记 ▸ 展开'); registryOpen=true; }
 clickLabel('新建项目');
 field('项目名称').value=name;
 addBinding(path, 'local');
 clickLabel('创建项目');
}
var state={defaultWs:testDirectory,sessions:[]};
var lsGet=()=>[];
var setTimeout=()=>1, clearTimeout=()=>{};
function check(b,msg) { if(!b) throw Error(msg); }
function buttons() { return all(mountTarget).filter(e=>e.tagName==='button'); }
function captionOf(e) { return text(e).replace(/⌄/g,'').trim(); }
function triggers(label) { return buttons().filter(e=>captionOf(e)===label); }
function byAria(label) { return all(mountTarget).find(e=>e.attrs['aria-label']===label); }
function field(prefix) { var l=all(mountTarget).find(e=>e.tagName==='label'&&text(e).startsWith(prefix)); if(!l) throw Error('field missing: '+prefix); return l.children[1]; }
function clickLabel(s) { var b=buttons().find(e=>text(e)===s); if(!b) throw Error('button missing: '+s); b.onclick(); }
var fetch=(path,opts)=>{opts=opts||{};var r=path==='/api/remotes'?{ok:true,status:200,body:JSON.stringify({remotes:[]})}:request(opts.method||'GET',path,opts.body||'');return Promise.resolve({ok:r.ok,status:r.status,text:()=>Promise.resolve(r.body),json:()=>Promise.resolve(JSON.parse(r.body))});};
`

func projectSettingsVM(t *testing.T, a *App, mount bool) (*goja.Runtime, func(string)) {
	t.Helper()
	vm := goja.New()
	vm.Set("testDirectory", filepath.Join(t.TempDir(), "ws"))
	vm.Set("request", func(method, path, body string) map[string]any {
		var b any
		if body != "" {
			if err := json.Unmarshal([]byte(body), &b); err != nil {
				t.Fatal(err)
			}
		}
		w := projectAPIRequest(a, method, path, b, false)
		return map[string]any{"ok": w.Code >= 200 && w.Code < 300, "status": w.Code, "body": w.Body.String()}
	})
	flag := "false"
	if mount {
		flag = "true"
	}
	if _, err := vm.RunString(strings.Replace(projectSettingsHarness, "__MOUNT__", flag, 1)); err != nil {
		t.Fatal(err)
	}
	// 两个设置页共用的宿主登记视图先加载，再加载知识库页（它会调用 JX.projectRegistry）。
	registry, err := os.ReadFile(filepath.Join("web", "project-registry.js"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(string(registry)); err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(`JX.projectRegistry.mount=(function(real){return function(el,opts){registryMountCount++;return real(el,opts);};})(JX.projectRegistry.mount);`); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("组件/project-knowledge/client.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vm.RunString(string(source)); err != nil {
		t.Fatal(err)
	}
	return vm, func(script string) {
		t.Helper()
		if _, err := vm.RunString(script); err != nil {
			t.Fatal(err)
		}
	}
}

// 设置页静态导航项（web/index.html）的图标属性，作为「同一套视觉规格」的基线。
func staticNavIconAttrs(t *testing.T) []map[string]string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("web", "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	html := string(src)
	start := strings.Index(html, `<nav class="set-nav">`)
	if start < 0 {
		t.Fatal("设置页导航 .set-nav 未找到")
	}
	rest := html[start:]
	end := strings.Index(rest, "</nav>")
	if end < 0 {
		t.Fatal("设置页导航未闭合")
	}
	svgOpen := regexp.MustCompile(`<svg\s+([^>]*)>`)
	attrRe := regexp.MustCompile(`([\w:-]+)="([^"]*)"`)
	var out []map[string]string
	for _, m := range svgOpen.FindAllStringSubmatch(rest[:end], -1) {
		attrs := map[string]string{}
		for _, a := range attrRe.FindAllStringSubmatch(m[1], -1) {
			attrs[a[1]] = a[2]
		}
		out = append(out, attrs)
	}
	return out
}

// 组件声明的 pane.icon 经 app-settings.js 的拼法进入导航项后，必须与相邻静态导航项同规格。
func TestProjectKnowledgeNavPaneIconMatchesSettingsNav(t *testing.T) {
	a := projectCapabilityTestApp(t)
	vm, run := projectSettingsVM(t, a, false)

	// 与 app-settings.js syncComponentPanePresence 同一拼法：innerHTML = (pane.icon||'') + esc(label)
	run(`check(!!captured&&!!captured.pane,'组件未注册设置分区');check(captured.pane.label==='项目知识库','分区文案异常: '+captured.pane.label);
var navItem=new Element('button');navItem.className='set-nav-item';
parseInto(navItem,(captured.pane.icon||'')+captured.pane.label);
var navSvg=navItem.children[0];
check(!!navSvg&&navSvg.tagName==='svg','导航项缺少图标节点：仍是纯文字');
check(text(navItem)==='项目知识库','图标与文案顺序异常: '+text(navItem));
check(navSvg.children.length>0,'图标没有图形节点');`)

	raw, err := vm.RunString(`JSON.stringify({attrs:navSvg.attrs,children:navSvg.children.map(function(c){return {tag:c.tagName,attrs:c.attrs};})})`)
	if err != nil {
		t.Fatal(err)
	}
	var icon struct {
		Attrs    map[string]string `json:"attrs"`
		Children []struct {
			Tag   string
			Attrs map[string]string
		}
	}
	if err := json.Unmarshal([]byte(raw.String()), &icon); err != nil {
		t.Fatal(err)
	}

	statics := staticNavIconAttrs(t)
	if len(statics) < 10 {
		t.Fatalf("静态导航图标数量异常: %d", len(statics))
	}
	// viewBox / fill / stroke / stroke-width：静态项全部一致，组件项必须逐字一致
	for _, key := range []string{"viewBox", "fill", "stroke", "stroke-width"} {
		want, seen := "", map[string]bool{}
		for _, s := range statics {
			if v, ok := s[key]; ok {
				seen[v] = true
				want = v
			}
		}
		if len(seen) != 1 {
			t.Fatalf("静态导航图标 %s 不唯一，无法作为基线: %v", key, seen)
		}
		if icon.Attrs[key] != want {
			t.Fatalf("图标 %s = %q，相邻导航项为 %q", key, icon.Attrs[key], want)
		}
	}
	// 圆角端点/连接：静态项里出现即统一，组件项同样采用
	for _, key := range []string{"stroke-linecap", "stroke-linejoin"} {
		want, seen := "", map[string]bool{}
		for _, s := range statics {
			if v, ok := s[key]; ok {
				seen[v] = true
				want = v
			}
		}
		if len(seen) != 1 {
			t.Fatalf("静态导航图标 %s 不一致: %v", key, seen)
		}
		if icon.Attrs[key] != want {
			t.Fatalf("图标 %s = %q，相邻导航项为 %q", key, icon.Attrs[key], want)
		}
	}
	// 尺寸/颜色交给 .set-nav-item svg，图标自身不得带内联尺寸、颜色或灰底
	allowed := map[string]bool{"viewBox": true, "fill": true, "stroke": true, "stroke-width": true, "stroke-linecap": true, "stroke-linejoin": true}
	for k, v := range icon.Attrs {
		if !allowed[k] {
			t.Fatalf("图标多出不受导航样式统一控制的属性 %s=%q", k, v)
		}
		if v == "currentColor" && k == "stroke" {
			continue
		}
	}
	// 纯描边线框：只有 path，且 path 不自带填充/描边颜色
	if len(icon.Children) == 0 {
		t.Fatal("图标没有 path")
	}
	for _, c := range icon.Children {
		if c.Tag != "path" {
			t.Fatalf("图标含非线框节点 %s", c.Tag)
		}
		if strings.TrimSpace(c.Attrs["d"]) == "" {
			t.Fatal("图标 path 缺少 d")
		}
		for k := range c.Attrs {
			if k != "d" {
				t.Fatalf("path 自带属性 %s，会破坏统一描边规格", k)
			}
		}
	}
	// 尺寸/颜色的统一来源：动态项与静态项共用 .set-nav-item，其 svg 规则由 style.css 给出
	css, err := os.ReadFile(filepath.Join("web", "style.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, need := range []string{".set-nav-item svg { width: 16px; height: 16px; color: var(--text-3); flex-shrink: 0; }", ".set-nav-item.active svg { color: var(--accent); }"} {
		if !strings.Contains(string(css), need) {
			t.Fatalf("导航项样式契约缺失: %s", need)
		}
	}
}

// 未建项目时下拉面板不得再渲染一个同名触发器，而要给出空状态；有项目时正常列出并可选。
func TestProjectKnowledgeProjectPickerEmptyStateAndSelection(t *testing.T) {
	a := projectCapabilityTestApp(t)
	vm, run := projectSettingsVM(t, a, true)

	t.Run("SingleTriggerAndEmptyHintWithoutProjects", func(t *testing.T) {
		run(`check(triggers('请选择项目').length===1,'空列表时出现 '+triggers('请选择项目').length+' 个「请选择项目」');
var trigger=byAria('请选择项目');check(!!trigger,'项目下拉触发器缺失');
var panel=trigger.parent.children[1];check(!!panel,'下拉面板节点缺失');
check(panel.hidden===true,'下拉面板应默认收起');
trigger.onclick();check(panel.hidden===false,'点击下拉后面板未展开');
check(trigger.attrs['aria-expanded']==='true','展开后 aria-expanded 未同步');
check(panel.children.length===1,'空面板条目数应为 1（空状态提示），实际 '+panel.children.length);
check(panel.children[0].tagName!=='button','空状态被渲染成可点条目：仍是同名触发器');
check(text(panel.children[0]).includes('还没有项目'),'空状态提示缺失: '+text(panel.children[0]));
check(triggers('请选择项目').length===1,'展开后面板里又冒出第二个「请选择项目」');
check(all(panel).filter(e=>e.tagName==='button').length===0,'空面板里不应有任何可点条目');
trigger.onclick();check(panel.hidden===true,'再次点击应收起面板');`)
	})

	t.Run("ListsAndSelectsProjectAfterCreate", func(t *testing.T) {
		vm.Set("testDirectory2", filepath.Join(t.TempDir(), "ws2"))
		run(`createProject('知识项目', testDirectory);`)
		run(`var trigger=byAria('请选择项目');check(!!trigger,'项目下拉触发器缺失');
check(text(trigger).startsWith('知识项目'),'新建后触发器未显示项目名: '+text(trigger));
check(triggers('请选择项目').length===0,'已有项目时触发器不应再是占位文案');
var panel=trigger.parent.children[1];check(panel.hidden===true,'新建后下拉面板应保持收起');
trigger.onclick();check(panel.hidden===false,'点击下拉后面板未展开');
check(panel.children.length===1,'面板应列出 1 个项目项，实际 '+panel.children.length);
var item=panel.children[0];
check(item.tagName==='button','项目项应为可点按钮');
check(!!item.children[0]&&item.children[0].tagName==='svg','项目项缺少图标');
check(text(item).startsWith('知识项目 · '),'项目项文案异常: '+text(item));
item.onclick();
check(panel.hidden===true,'选中后面板应收起');
check(text(mountTarget).includes('项目 ID：'),'选中后没有渲染项目编辑器');`)
		// 两个项目时才能验证「从面板切到另一个」：面板只列项目项，不再混入占位条目。
		run(`createProject('知识项目二', testDirectory2);`)
		if list, err := a.Store.ListProjects(true); err != nil || len(list) != 2 {
			t.Fatalf("项目未通过 UI 落到存储: %+v %v", list, err)
		} else {
			for _, p := range list {
				if p.Name == "知识项目" {
					vm.Set("idA", p.ID)
				} else {
					vm.Set("idB", p.ID)
				}
			}
		}
		run(`check(text(byAria('请选择项目')).startsWith('知识项目二'),'新建第二个项目后未自动选中: '+text(byAria('请选择项目')));
check(text(byAria('请选择项目')).includes(idB),'触发器未登记第二个项目: '+text(byAria('请选择项目')));
var trigger=byAria('请选择项目');var panel=trigger.parent.children[1];trigger.onclick();
check(panel.children.length===2,'面板应列出 2 个项目项，实际 '+panel.children.length);
check(all(panel).filter(e=>e.tagName==='button'&&captionOf(e)==='请选择项目').length===0,'面板里混入了占位条目');
var target=all(panel).find(e=>e.tagName==='button'&&text(e).startsWith('知识项目 · '));
check(!!target,'面板里找不到第一个项目');
target.onclick();`)
		run(`check(text(trigger).startsWith('知识项目 · '),'切换项目后触发器未更新: '+text(trigger));
check(text(trigger).includes(idA),'切换后触发器未指向所选项目: '+text(trigger));
check(text(mountTarget).includes('项目 ID：'+idA),'切换后编辑器不是所选项目');`)
	})
}
