package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 真组件 + 最小 DOM，fetch 转发真实项目 API；项目登记由宿主视图（web/project-registry.js）承担，
// 知识库页只负责项目选择、知识正文与预算。所有存储由测试辅助函数隔离。
//
// 这里是一条顺序流程（建项目 → 改绑定 → 加远程绑定 → 拒绝非法路径 → 仍能保存知识正文），
// 刻意不拆子测试：goja 里的失败通过 t.Fatal 上抛，必须落在同一个 *testing.T 上才看得清。
func TestProjectKnowledgeSettingsDirectoryUI(t *testing.T) {
	a := projectCapabilityTestApp(t)
	vm, run := projectSettingsVM(t, a, true)

	// 1) 通过共享登记视图（宿主 JX.projectRegistry）新建项目并绑定目录。
	run(`createProject('UI selected', testDirectory);`)
	list, err := a.Store.ListProjects(true)
	if err != nil || len(list) != 1 || len(list[0].Workspaces) != 1 || list[0].Workspaces[0].DisplayPath != vm.Get("testDirectory").String() {
		t.Fatalf("共享登记新建项目/绑定目录未落库: %+v %v", list, err)
	}
	run(`var cap=byAria('请选择项目');check(text(cap).startsWith('UI selected'),'知识库页未跟上共享登记的新项目: '+text(cap));`)

	// 图标一致性：项目下拉项的文件夹图标与 app-chat 工作目录图标同一路径（同一套视觉规格）。
	run(`var opt=all(mountTarget).find(e=>e.tagName==='button'&&text(e).startsWith('UI selected · '));
check(!!opt,'项目下拉项缺失: '+text(mountTarget).slice(0,600));
check(opt.children[0].tagName==='svg','项目项缺少图标');
var folderPath=opt.children[0].children[0].attrs.d;`)
	chat, err := os.ReadFile("web/app-chat.js")
	if err != nil {
		t.Fatal(err)
	}
	if folder := vm.Get("folderPath").String(); folder == "" || !strings.Contains(string(chat), `d="`+folder+`"`) {
		t.Fatal("icon no longer matches workspace selector")
	}

	// 2) 手工本地绑定：仍在共享登记视图里改，知识库页不再自带绑定编辑。
	vm.Set("manualLocal", filepath.Join(t.TempDir(), "manual"))
	run(`var b=all(mountTarget).find(e=>e.tagName==='input'&&e.attrs['aria-label']==='绑定绝对路径');
check(!!b,'绑定输入框缺失: '+text(mountTarget).slice(0,600));
b.value=manualLocal;b.oninput();clickLabel('保存名称与绑定');`)
	run(`check(text(mountTarget).includes('已保存'),'本地绑定保存失败: '+text(mountTarget).slice(0,600));`)
	list, err = a.Store.ListProjects(true)
	if err != nil || len(list) != 1 || len(list[0].Workspaces) != 1 {
		t.Fatalf("手工本地绑定: %+v %v", list, err)
	}
	if list[0].Workspaces[0].DisplayPath != vm.Get("manualLocal").String() {
		t.Fatal("local manual path not persisted")
	}

	// 3) 远程绑定（含带端口与 IPv6）：同一份共享登记视图，服务端 /resolve 规范化。
	for i, path := range []string{"u@host:/srv/manual", "u@host:2222:/srv/manual", "u@[2001:db8::2]:/srv/manual"} {
		vm.Set("manualPath", path)
		run(`if(!all(mountTarget).some(e=>e.tagName==='button'&&text(e)==='保存名称与绑定')) clickLabel('编辑');
addBinding(manualPath, 'remote');
clickLabel('保存名称与绑定');`)
		run(`check(text(mountTarget).includes('已保存'),'远程绑定保存失败('+manualPath+'): '+text(mountTarget).slice(0,600));`)
		list, err = a.Store.ListProjects(true)
		if err != nil {
			t.Fatal(err)
		}
		if len(list) != 1 || len(list[0].Workspaces) != 2+i {
			t.Fatalf("第 %d 条远程绑定未保存(%s): %+v", i+1, path, list)
		}
	}
	list, err = a.Store.ListProjects(true)
	if err != nil || len(list) != 1 || len(list[0].Workspaces) != 4 {
		t.Fatalf("远程绑定未全部保存: %+v %v", list, err)
	}

	// 4) 非法远程路径：共享登记在写库前用 /resolve 拦下（旧视图才有的服务端规范化，
	//    现在两页共用同一条），既不建项目也不改已有绑定。
	vm.Set("badPath", "u@host:bad:/srv")
	run(`clickLabel('新建项目');
field('项目名称').value='不应创建';
addBinding(badPath, 'remote');
clickLabel('创建项目');`)
	run(`check(text(mountTarget).includes('保存失败'),'非法远程路径未被拒: '+text(mountTarget).slice(0,600));`)
	list, err = a.Store.ListProjects(true)
	if err != nil || len(list) != 1 || len(list[0].Workspaces) != 4 {
		t.Fatal("invalid input changed projects or bindings")
	}

	// 5) 知识正文编辑没被登记重构带坏：选中项目后仍能保存正文。
	run(`var c=all(mountTarget).find(e=>e.tagName==='textarea');check(!!c,'知识正文输入框缺失: '+text(mountTarget).slice(0,600));c.value='正文：当前有效约束一';c.oninput();`)
	run(`clickLabel('保存知识正文与预算');`)
	run(`check(text(mountTarget).includes('知识已保存'),'知识正文保存失败: '+text(mountTarget).slice(0,600));`)
}
