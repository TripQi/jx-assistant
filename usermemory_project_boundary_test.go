package main

// 「项目背景归项目知识库」口径的定向回归（AI 记忆与拟人化组件）：
//   - 设置页字段标签不再把「项目背景」当记忆内容收，改指路 设置 → 项目知识库；
//   - 长期助理条目的注入补上信息边界与迁移流程（先列出 → 确认绑定 → 写入验证
//     → 经用户确认后移除），task 模式沿用已有的边界行与「跨项目偏好」口径；
//   - 组件清单的两处描述同步声明信息边界。
// 纯文案断言：不改读写机制，也不放宽任何既有断言。

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/dop251/goja"
)

// 三处文案的「新口径必须全、旧口径必须绝迹」逐字对照。
func TestUserMemoryEnvPromptProjectBoundary(t *testing.T) {
	a, _, _ := newUserMemoryTestApp(t)

	// 长期助理：call_api 通道照旧，另给项目边界与迁移流程
	mgrEnv := a.envPrompt(&SessionMeta{Mode: "assistant", Permission: permWorkspace, Workspace: a.Cfg.Workspace})
	for _, want := range []string{
		"整个记忆文件可读可改",
		"信息边界：项目背景与有效约束写项目知识库、不要写进用户记忆",
		"记忆里发现项目专属片段时，按「先列出 → 确认绑定 → 写入项目知识库并验证 → 经用户确认后移除」处理",
	} {
		if !strings.Contains(mgrEnv, want) {
			t.Fatalf("长期助理条目应含 %q:\n%s", want, mgrEnv)
		}
	}

	// task 模式：只记身份/习惯/喜好/跨项目偏好，项目背景不外溢
	taskEnv := a.envPrompt(&SessionMeta{Mode: "task", Permission: permWorkspace, Workspace: a.Cfg.Workspace})
	for _, want := range []string{
		"了解到用户新的身份/偏好/习惯或跨项目偏好时主动更新",
		"项目背景与项目约束归项目知识库，不写入用户记忆",
		"- 信息边界：这里只记用户身份、习惯、喜好和跨项目偏好；项目背景与有效约束归项目知识库。",
	} {
		if !strings.Contains(taskEnv, want) {
			t.Fatalf("task 模式记忆维护条目应含 %q:\n%s", want, taskEnv)
		}
	}
	// 旧口径：把项目背景当成记忆内容来更新
	if strings.Contains(taskEnv, "习惯/项目背景时主动更新") || strings.Contains(mgrEnv, "习惯/项目背景时主动更新") {
		t.Fatalf("旧口径（项目背景进记忆）不得再出现在注入里:\n%s", taskEnv)
	}
}

// 设置页标签：真组件源码 + 最小 DOM（与 project_knowledge_settings_ui_test 同款
// 手法），断言新文案在、旧枚举（把项目背景列为记忆内容）不在。
func TestUserMemorySettingsLabelProjectBoundary(t *testing.T) {
	vm := goja.New()
	harness := `
class Elem {
 constructor(tag){ this.tagName=tag; this.children=[]; this.attrs={}; this.dataset={}; this.value=''; this.textContent=''; this.isConnected=true;
   this.style={cssText:'',setProperty:function(){},removeProperty:function(){}};
   this.classList={add:function(){},remove:function(){},contains:function(){return false;}}; }
 appendChild(e){ this.children.push(e); e.parent=this; return e; }
 setAttribute(k,v){ this.attrs[k]=v; }
 addEventListener(){}
 closest(){ return null; }
 focus(){}
 querySelectorAll(sel){ return matches(this,sel); }
 querySelector(sel){ return matches(this,sel)[0]||null; }
 set innerHTML(v){ this.children=[]; this._html=v; }
 get innerHTML(){ return this._html||''; }
}
function all(e){ return e.children.reduce(function(a,c){ return a.concat([c],all(c)); }, []); }
function matches(e,sel){
 var cls=sel.charAt(0)==='.', attr=sel.charAt(0)==='[';
 return all(e).filter(function(c){
   if(cls) return String(c.className||'').split(' ').indexOf(sel.slice(1))>=0;
   if(attr){ var k=sel.slice(1,-1); return k==='data-um-key' ? c.dataset.umKey!==undefined : c.attrs[k]!==undefined; }
   return c.tagName===sel;
 });
}
var root=new Elem('div'), mounted;
var document={createElement:function(t){return new Elem(t);},documentElement:new Elem('html'),
  getElementById:function(){return new Elem('div');},querySelector:function(){return null;}};
var window={addEventListener:function(){},removeEventListener:function(){}};
var CX={session:function(){return '';},register:function(d){ d.init(); },unregisterSettings:function(){},
  registerSettings:function(d){ mounted=d; d.mount(root); }};
var fetch=function(){ return Promise.resolve({ok:true,status:200,
  json:function(){ return Promise.resolve({userName:'',userMemory:'',aiName:'',aiIdentity:'',bubbleColor:''}); },
  text:function(){ return Promise.resolve(''); }}); };
function check(b,msg){ if(!b) throw Error(msg); }
`
	if _, err := vm.RunString(harness); err != nil {
		t.Fatal(err)
	}
	source, err := os.ReadFile("组件/user-memory/client.js")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.RunString(string(source)); err != nil {
		t.Fatal(err)
	}
	// 取所有 .sf-label 的文本做断言（挂载即渲染，不需要 fetch 回包）
	result, err := vm.RunString(`
(function(){
  var labels = all(root).filter(function(e){ return e.className==='sf-label'; }).map(function(e){ return e.textContent; });
  return labels;
})()
`)
	if err != nil {
		t.Fatal(err)
	}
	var labels []string
	if err := vm.ExportTo(result, &labels); err != nil {
		t.Fatal(err)
	}
	if len(labels) != 4 {
		t.Fatalf("设置页应有 4 个字段标签，实际 %d: %v", len(labels), labels)
	}
	memLabel := ""
	for _, s := range labels {
		if strings.HasPrefix(s, "用户身份与喜好（记忆：") {
			memLabel = s
		}
	}
	if memLabel == "" {
		t.Fatalf("找不到「用户身份与喜好」字段标签: %v", labels)
	}
	for _, want := range []string{"跨项目偏好", "项目背景与项目约束请到 设置 → 项目知识库"} {
		if !strings.Contains(memLabel, want) {
			t.Fatalf("标签应含 %q: %s", want, memLabel)
		}
	}
	// 旧文案：项目背景被列为记忆内容、以及「AI 可在对话中更新这一栏」的尾巴
	for _, bad := range []string{"记忆：身份、偏好、习惯、项目背景", "习惯、项目背景", "AI 可在对话中更新这一栏"} {
		if strings.Contains(memLabel, bad) {
			t.Fatalf("标签不应再含旧文案 %q: %s", bad, memLabel)
		}
	}
	// 窄屏不挤压：单行标签，长度与其它字段标签同一量级（不超过 56 字）
	if n := utf8.RuneCountInString(memLabel); n > 56 {
		t.Fatalf("标签过长（%d 字），窄屏会挤压: %s", n, memLabel)
	}
}

// 组件清单：组件级描述与 memory_update 能力都声明信息边界；核心语义与能力
// 列表不变（版本、modes、writes、memory_query 一字不改）。
func TestUserMemoryManifestProjectBoundary(t *testing.T) {
	raw, err := os.ReadFile("组件/user-memory/manifest.json")
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		ID           string `json:"id"`
		Version      string `json:"version"`
		Description  string `json:"description"`
		Capabilities []struct {
			Name        string   `json:"name"`
			Description string   `json:"description"`
			Writes      bool     `json:"writes"`
			Modes       []string `json:"modes"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != userMemoryComponentID {
		t.Fatalf("组件 id 不符: %q", m.ID)
	}
	if m.Version != "1.2.0" {
		t.Fatalf("本次只改文案，版本号不应变动: %q", m.Version)
	}
	boundary := "项目背景与项目约束归项目知识库"
	if !strings.Contains(m.Description, boundary) || !strings.Contains(m.Description, "只记用户身份、习惯、喜好与跨项目偏好") {
		t.Fatalf("组件描述应声明信息边界: %s", m.Description)
	}
	if len(m.Capabilities) != 2 {
		t.Fatalf("能力列表应保持两条: %+v", m.Capabilities)
	}
	for _, c := range m.Capabilities {
		switch c.Name {
		case "memory_query":
			if c.Description != "查询用户记忆的全部字段（用户称呼、用户身份与喜好、AI 助理名字与身份）" {
				t.Fatalf("memory_query 描述不应改动: %s", c.Description)
			}
		case "memory_update":
			if !strings.Contains(c.Description, "整体替换为给定内容") {
				t.Fatalf("memory_update 的核心语义应保留: %s", c.Description)
			}
			if !strings.Contains(c.Description, boundary) || !strings.Contains(c.Description, "不要写进记忆") {
				t.Fatalf("memory_update 描述应声明信息边界: %s", c.Description)
			}
			if !c.Writes {
				t.Fatal("memory_update 仍应为写入型（writes=true）")
			}
			if len(c.Modes) != 5 {
				t.Fatalf("memory_update 的 modes 不应改动: %v", c.Modes)
			}
		default:
			t.Fatalf("未预期的能力: %s", c.Name)
		}
	}
}
