package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// 只加载目标生产函数，DOM/网络用确定性桩；Promise 在 RunString 返回前排空。
func interjectVM(t *testing.T) *goja.Runtime {
	t.Helper()
	raw, err := os.ReadFile("web/app-chat.js")
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	_, err = vm.RunString(`
function assert(ok, msg) { if (!ok) throw Error(msg); }
function makeView(id) { return { state: {cur:{id}, msgs:[], pendingSteer:[], pendingQueue:[]}, input:{value:'draft',dataset:{jxVoice:'1'}} }; }
const mainView = makeView('main'), subView = makeView('sub');
const state = mainView.state, subState = subView.state;
let response, hook = () => {}, request, cleared = [], asyncError = null;
function api(url, opts) { request = {url, body:JSON.parse(opts.body)}; hook(); return Promise.resolve(response); }
function lsGet() { return 'interject'; }
function renderQueueDock() {}
function renderMessages(msgs, view) { view.rendered = view.state.pendingSteer.map(p => p.id); }
function pendingHint(view, text) { view.hint = text; }
function clearQuote() {}
function autoGrow() {}
function clearSessionDraftLS(id) { cleared.push(id); }
function clearSubDraftLS(id) { cleared.push(id); }
`)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"handleGlobalEvent", "applyPending", "sendMidrun"} {
		src := string(raw)
		start := strings.Index(src, "function "+name+"(")
		if start < 0 {
			t.Fatalf("missing function %s", name)
		}
		end := strings.Index(src[start:], "\n}")
		if end < 0 {
			t.Fatalf("missing function end %s", name)
		}
		prefix := ""
		if name == "sendMidrun" {
			prefix = "async "
		}
		if _, err := vm.RunString(prefix + src[start:start+end+2]); err != nil {
			t.Fatal(err)
		}
	}
	return vm
}

func interjectJS(t *testing.T, script string) {
	t.Helper()
	vm := interjectVM(t)
	if _, err := vm.RunString(script); err != nil {
		t.Fatal(err)
	}
	if v := vm.Get("asyncError"); !goja.IsNull(v) {
		t.Fatal(v.String())
	}
}

func TestInterjectPendingRouting(t *testing.T) {
	interjectJS(t, `
handleGlobalEvent({type:'pending',sessionId:'main',pending:{steer:[{id:'m'}]}});
assert(mainView.state.pendingSteer[0].id === 'm', 'main receives own pending');
assert(subView.state.pendingSteer.length === 0, 'main pending must not leak into right pane');
handleGlobalEvent({type:'pending',sessionId:'sub',pending:{queue:[{id:'s'}]}});
assert(subView.state.pendingQueue[0].id === 's', 'right pane receives own pending');
state.cur = null;
handleGlobalEvent({type:'pending',sessionId:'sub',pending:{steer:[]}});
assert(subView.state.pendingQueue.length === 0, 'right pane works without main session');
`)
}

func TestInterjectResponseSnapshot(t *testing.T) {
	interjectJS(t, `
response = {accepted:true,id:'already-landed',mode:'steer',pending:{steer:[],queue:[],paused:true}};
hook = () => applyPending(mainView,{steer:[],queue:[]}); // 清理广播先于 HTTP 响应
sendMidrun('hello',true,mainView,true).then(() => {
  assert(mainView.state.pendingSteer.length === 0, 'empty response snapshot must not resurrect landed steer');
  assert(mainView.state.queuePaused, 'response snapshot includes paused');
  assert(mainView.rendered.length === 0, 'render contains no ghost');
  assert(request.body.voiceUsed && request.body.content === 'hello', 'voice/content preserved');
  assert(mainView.input.value === '' && !mainView.input.dataset.jxVoice, 'composer cleared');
  assert(cleared[0] === 'main' && mainView._pinned === false, 'draft cleanup and follow preserved');
}).catch(e => { asyncError = e; });
`)
}

func TestInterjectResponsePendingAndLegacy(t *testing.T) {
	interjectJS(t, `
response = {accepted:true,id:'new',mode:'steer',pending:{steer:[{id:'new',content:'same'}],queue:[{id:'other'}],paused:true}};
mainView.state.msgs = [{role:'user',content:'same',seq:1}];
sendMidrun('same',false,mainView,false).then(() => {
  assert(mainView.state.pendingSteer.length === 1, 'old identical body must not hide new pending');
  assert(mainView.state.pendingQueue[0].id === 'other' && mainView.state.queuePaused, 'nonempty response snapshot applied in full');
  response = {accepted:true,id:'legacy',mode:'queue'};
  hook = () => applyPending(subView,{queue:[{id:'legacy',content:'queue'}]});
  return sendMidrun('queue',true,subView,false);
}).then(() => {
  assert(subView.state.pendingQueue.length === 1, 'legacy backend fallback deduplicates by id');
  assert(cleared[0] === 'sub', 'sub draft cleanup preserved');
}).catch(e => { asyncError = e; });
`)
}

func TestInterjectHTTPSnapshot(t *testing.T) {
	for _, drained := range []bool{false, true} {
		name := "queued"
		if drained {
			name = "drained-before-response"
		}
		t.Run(name, func(t *testing.T) {
			a, _, task, _ := newMgrNotifyTestApp(t)
			// 夹具只登记内存执行轮，不触发开/收轮的后台交付与心跳。
			run := &sessionRun{}
			a.runners[task.ID] = run
			t.Cleanup(func() { delete(a.runners, task.ID) })
			if drained {
				// pushPending 的模型打断回调同步消费，确定性模拟响应前已排水。
				entry := a.beginModelStream(task.ID, func() {
					a.pendMu.Lock()
					delete(a.pendingSteer, task.ID)
					a.pendMu.Unlock()
				})
				t.Cleanup(entry.end)
			}
			r := httptest.NewRequest("POST", "/api/sessions/"+task.ID+"/interject", strings.NewReader(`{"content":"hello","mode":"steer","voiceUsed":true}`))
			r.SetPathValue("id", task.ID)
			w := httptest.NewRecorder()
			a.handleInterject(w, r)
			var got struct {
				Accepted bool          `json:"accepted"`
				ID       string        `json:"id"`
				Pending  *PendingState `json:"pending"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
				t.Fatal(err)
			}
			if w.Code != 200 || !got.Accepted || got.Pending == nil {
				t.Fatalf("missing response snapshot: HTTP %d %s", w.Code, w.Body.String())
			}
			want := 1
			if drained {
				want = 0
			}
			if len(got.Pending.Steer) != want {
				t.Fatalf("steer count=%d want=%d", len(got.Pending.Steer), want)
			}
			if !drained && got.Pending.Steer[0].ID != got.ID {
				t.Fatal("snapshot id mismatch")
			}
			t.Logf("HTTP %d snapshot steer=%d drained=%v", w.Code, len(got.Pending.Steer), drained)
		})
	}
}
