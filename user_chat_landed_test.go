package main

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// 执行生产判定和完整 send 异常分支；不使用网络、计时器或执行体。
func TestUserChatLandedRequiresRequestIdentity(t *testing.T) {
	source := "web/app-chat.js"
	if baseline := os.Getenv("JX_CHAT_TEST_SOURCE"); baseline != "" {
		source = baseline
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	vm := goja.New()
	_, err = vm.RunString(`
function assert(ok, msg) { if (!ok) throw Error(msg); }
let mainView, detail, headerOK, probes, queued, errors, ended, sent, landing, asyncError = null, completed = false;
let nonce = 0;
const crypto = {getRandomValues(a) { a.fill(++nonce); return a; }};
function reset(running, headers, landed) {
  mainView = {state:{cur:{id:'s'},abort:{signal:{}},msgs:[],pendingSend:{}},input:{value:''}};
  detail = {running, messages:[{id:71,seq:9,role:'user',kind:'chat',content:'repeat',requestId:'old-request'}]};
  headerOK = headers; landing = landed; probes = 0; queued = []; errors = []; ended = 0; sent = null;
}
function prepareSend(forceContent, view) {
  return Promise.resolve({input:view.input,content:'repeat',sid:'s',fromInput:true,voiceUsed:true});
}
function fetch(url, opts) {
  sent = JSON.parse(opts.body);
  assert(/^[0-9a-f]{32}$/.test(sent.requestId), 'POST needs generated request identity');
  assert(sent.content === 'repeat' && sent.voiceUsed === true, 'existing body preserved');
  if (landing) detail.messages.push({role:'user',kind:'chat',requestId:sent.requestId,content:landing === 'hook' ? 'rewritten by hook' : 'repeat'});
  return headerOK ? Promise.resolve({ok:true}) : Promise.reject(Error('disconnected before headers'));
}
function consumeStream() { return Promise.reject(Error('disconnected after headers')); }
function api() { probes++; return Promise.resolve(detail); }
function applyPending() {}
function renderMessages() {}
function sendMidrun(content, viaInput, view) { queued.push({content,viaInput,view}); return Promise.resolve(); }
function autoGrow() {}
function showRunError(view, message) { errors.push(message); }
function endTurn() { ended++; return Promise.resolve(); }
`)
	if err != nil {
		t.Fatal(err)
	}
	src := string(raw)
	for _, name := range []string{"userChatLanded", "send"} {
		start := strings.Index(src, "function "+name+"(")
		if start < 0 {
			t.Fatalf("missing function %s", name)
		}
		end := strings.Index(src[start:], "\n}")
		if end < 0 {
			t.Fatalf("missing function end %s", name)
		}
		prefix := ""
		if name == "send" {
			prefix = "async "
		}
		if _, err := vm.RunString(prefix + src[start:start+end+2]); err != nil {
			t.Fatal(err)
		}
	}
	_, err = vm.RunString(`
for (const messages of [
  [], [{role:'user',kind:'chat',content:'repeat',id:71,seq:9}],
  [{role:'user',content:'repeat',id:71,seq:9}],
  [{role:'user',kind:'chat',content:'repeat',pendingId:'req'}],
  [{role:'user',kind:'chat',content:'repeat',requestId:'other'}],
  [{role:'assistant',kind:'chat',requestId:'req'}],
  [{role:'user',kind:'report',requestId:'req'}]
]) assert(!userChatLanded(messages,'req'), 'unrelated row must not confirm: '+JSON.stringify(messages));
assert(!userChatLanded([{role:'user',kind:'chat'}],''), 'empty ID must not match old rows');
assert(!userChatLanded([{role:'user',kind:'chat'}],undefined), 'missing ID must not match');
assert(userChatLanded([{role:'user',kind:'chat',content:'hook rewrite',requestId:'req'}],'req'), 'identity survives rewritten body');
reset(true, false, false);
send(undefined,mainView).then(() => {
  assert(probes === 1 && queued.length === 1, 'unlanded running request keeps queue fallback');
  assert(queued[0].content === 'repeat' && queued[0].viaInput === false && queued[0].view === mainView, 'queue arguments unchanged');
  assert(errors.length === 0 && ended === 1 && mainView.state.pendingSend === null, 'cleanup unchanged');
  const previous = sent.requestId;
  reset(false, false, false);
  return send(undefined,mainView).then(() => assert(sent.requestId !== previous, 'identical bodies need distinct request IDs'));
}).then(() => {
  assert(probes === 1 && queued.length === 0, 'unlanded idle request must not queue');
  assert(mainView.input.value === 'repeat' && errors.length === 1 && ended === 1, 'idle fallback restores input/error');
  reset(true, false, true);
  return send(undefined,mainView);
}).then(() => {
  assert(probes === 1 && queued.length === 0 && errors.length === 0 && mainView.input.value === '', 'landed running request never resends');
  reset(false, false, 'hook');
  return send(undefined,mainView);
}).then(() => {
  assert(probes === 1 && queued.length === 0 && errors.length === 0 && mainView.input.value === '', 'landed rewritten idle request never restores');
  reset(true, true, false);
  return send(undefined,mainView);
}).then(() => {
  assert(probes === 0 && queued.length === 0 && errors.length === 0, '2xx headers confirm even when stream breaks');
  assert(mainView.input.value === '' && ended === 1, 'accepted request not restored or resent');
  completed = true;
}).catch(e => { asyncError = e; });
`)
	if err != nil {
		t.Fatal(err)
	}
	if v := vm.Get("asyncError"); !goja.IsNull(v) {
		t.Fatal(v.String())
	}
	if !vm.Get("completed").ToBoolean() {
		t.Fatal("send scenarios did not complete")
	}
}
