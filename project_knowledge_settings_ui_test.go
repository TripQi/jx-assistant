package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// 真组件 + 最小 DOM，fetch 转发真实项目 API；所有存储由测试辅助函数隔离。
func TestProjectKnowledgeSettingsDirectoryUI(t *testing.T) {
	a := projectCapabilityTestApp(t)
	vm := goja.New()
	dir := filepath.Join(t.TempDir(), "selected")
	vm.Set("testDirectory", dir)
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
	harness := `
class Element {
 constructor(tag) { this.tagName=tag; this.children=[]; this.style={}; this.attrs={}; this.value=''; this.textContent=''; this.isConnected=true; }
 appendChild(e) { this.children.push(e); e.parent=this; return e; }
 replaceChildren() { this.children=[]; this.textContent=''; }
 setAttribute(k,v) { this.attrs[k]=v; }
 closest(s) { if(s==='#settings') return root; if(s==='#cxsession') return null; if(s==='[data-pk-readonly]') { for(var e=this;e;e=e.parent) if(e.attrs['data-pk-readonly']!==undefined) return e; } return null; }
 querySelectorAll(s) { return all(this).filter(e=>s.split(',').includes(e.tagName)); }
 focus() { this.focused=true; }
}
var root=new Element('div'), mounted;
var document={createElement:t=>new Element(t),createElementNS:(ns,t)=>new Element(t)};
var window={confirm:()=>true,addEventListener:()=>{},removeEventListener:()=>{}};
var CX={register:d=>d.init(),registerSettings:d=>{mounted=d;d.mount(root);}};
var state={defaultWs:testDirectory,sessions:[{workspace:testDirectory}]};
var lsGet=()=>[testDirectory];
var setTimeout=()=>1,clearTimeout=()=>{};
function all(e) { return e.children.reduce((a,c)=>a.concat([c],all(c)),[]); }
function text(e) { return e.textContent+e.children.map(text).join(''); }
function findButton(s) { var e=all(root).find(e=>e.tagName==='button'&&text(e)===s); if(!e) throw Error('button missing: '+s); return e; }
function click(s) { findButton(s).onclick(); }
function field(prefix) { var label=all(root).find(e=>e.tagName==='label'&&text(e).startsWith(prefix)); if(!label) throw Error('field missing: '+prefix); return label.children[1]; }
function check(b,msg) { if(!b) throw Error(msg); }
var fetch=(path,opts)=>{opts=opts||{};var r=path==='/api/remotes'?{ok:true,status:200,body:JSON.stringify({remotes:[{user:'u',host:'2001:db8::1',port:2222,dir:'/srv/app'}]})}:request(opts.method||'GET',path,opts.body||'');return Promise.resolve({ok:r.ok,status:r.status,text:()=>Promise.resolve(r.body),json:()=>Promise.resolve(JSON.parse(r.body))});};
`
	if _, err := vm.RunString(harness); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("组件/project-knowledge/client.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = vm.RunString(string(source)); err != nil {
		t.Fatal(err)
	}
	activeTest := t
	run := func(script string) {
		activeTest.Helper()
		if _, err := vm.RunString(script); err != nil {
			activeTest.Fatal(err)
		}
	}
	t.Run("IconsAndSelectedDirectoryCreateBinding", func(t *testing.T) {
		activeTest = t
		run(`var option=findButton(testDirectory);check(option.children[0].tagName==='svg','directory icon missing');var folderPath=option.children[0].children[0].attrs.d;check(findButton('请选择项目⌄').children[0].children[0].attrs.d===folderPath,'project icon differs');option.onclick();check(field('工作目录').value===testDirectory,'selection did not fill input');field('新项目名称').value='UI selected';click('新建项目');`)
		run(`check(text(root).includes('项目已新建并绑定'),'create/bind failed: '+text(root));var projectOption=all(root).find(e=>e.tagName==='button'&&text(e).startsWith('UI selected · ')&&!text(e).endsWith('⌄'));check(projectOption.children[0].tagName==='svg'&&projectOption.children[0].children[0].attrs.d===folderPath,'project option icon missing');`)
		list, err := a.Store.ListProjects(true)
		if err != nil || len(list) != 1 || len(list[0].Workspaces) != 1 || list[0].Workspaces[0].DisplayPath != dir {
			t.Fatalf("selected binding: %+v %v", list, err)
		}
		chat, err := os.ReadFile("web/app-chat.js")
		if err != nil {
			t.Fatal(err)
		}
		folder := vm.Get("folderPath").String()
		if !strings.Contains(string(chat), `d="`+folder+`"`) {
			t.Fatal("icon no longer matches workspace selector")
		}
	})
	t.Run("ManualLocalBinding", func(t *testing.T) {
		activeTest = t
		vm.Set("manualLocal", filepath.Join(t.TempDir(), "manual"))
		run(`var binding=all(root).find(e=>e.attrs['aria-label']==='绑定绝对路径');binding.value=manualLocal;binding.oninput();click('保存名称与绑定');`)
		run(`check(text(root).includes('名称与绑定已保存'),'local manual save failed');`)
		list, _ := a.Store.ListProjects(true)
		if list[0].Workspaces[0].DisplayPath != vm.Get("manualLocal").String() {
			t.Fatal("local manual path not persisted")
		}
	})
	t.Run("RemoteSelectionAndManualBindings", func(t *testing.T) {
		activeTest = t
		run(`click('u@[2001:db8::1]:2222:/srv/app');check(field('工作目录').value==='u@[2001:db8::1]:2222:/srv/app','remote selection');click('添加到当前项目绑定');click('保存名称与绑定');`)
		for _, path := range []string{"u@host:/srv/manual", "u@host:2222:/srv/manual", "u@[2001:db8::2]:/srv/manual"} {
			vm.Set("manualPath", path)
			run(`field('工作目录').value=manualPath;click('添加到当前项目绑定');click('保存名称与绑定');`)
			run(`check(text(root).includes('名称与绑定已保存'),'manual save failed: '+text(root));`)
		}
		list, _ := a.Store.ListProjects(true)
		if len(list[0].Workspaces) != 5 {
			t.Fatalf("manual bindings not saved: %+v", list)
		}
	})
	t.Run("InvalidInputRejected", func(t *testing.T) {
		activeTest = t
		run(`field('工作目录').value='u@host:bad:/srv';click('解析归属');`)
		run(`check(text(root).includes('解析失败'),'invalid resolve accepted');click('添加到当前项目绑定');click('保存名称与绑定');`)
		run(`check(text(root).includes('操作失败'),'invalid binding accepted');field('新项目名称').value='must not be created';click('新建项目');`)
		run(`check(text(root).includes('操作失败'),'invalid create accepted');`)
		list, _ := a.Store.ListProjects(true)
		if len(list) != 1 || len(list[0].Workspaces) != 5 {
			t.Fatal("invalid input changed bindings or created a project")
		}
	})
}
