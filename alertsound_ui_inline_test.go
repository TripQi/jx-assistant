package main

// 设置页「提示音方案 / 自定义 WAV 路径」这块交互的回归（用户实测的 400 弹窗）。
//
// 口径（用户定调）：**切档 ≠ 已配置完成**。
//   - 切到「自定义 WAV 文件」不弹模态框，路径框下方给行内提示（还没填路径时暂用
//     系统提示音），用户可以继续填；
//   - 填了非法路径再保存/试听：原因写在路径框下方（行内），不弹模态框；
//   - 试听在路径为空时给友好的行内提示，不弹错误框；
//   - 填了合法路径后保存即生效（服务端存下路径、界面回到正常说明）。
//
// 这里不复制被测逻辑：直接用 goja 跑 web/app-settings.js 里「常规页：询问提醒」
// 那一段真实代码，DOM/api 用最小替身（和 token_combo_test.go 同一套路子）。
// 模态框＝alert()：替身把它记下来，断言「一次都没弹」。

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// askAlertUIHarness 最小 DOM + api 替身：只模拟这一区块用到的东西。
const askAlertUIHarness = `
function assert(ok, msg) { if (!ok) throw Error(msg); }
function mkNode(id) {
  return { id, value: '', checked: false, textContent: '', style: {}, onclick: null, onchange: null,
    classList: { _s: new Set(),
      toggle(c, on) { const has = this._s.has(c); const want = on === undefined ? !has : !!on;
        if (want) this._s.add(c); else this._s.delete(c); },
      contains(c) { return this._s.has(c); }, add(c) { this._s.add(c); }, remove(c) { this._s.delete(c); } } };
}
const nodes = {};
for (const id of ['set-askalert-on','set-askalert-appear-on','set-askalert-sec','set-askalert-text',
  'set-askalert-wav','set-askalert-wav-field','set-askalert-wav-state','set-askalert-sound-try',
  'set-askalert-wav-pick','sel-askalert-sound']) nodes[id] = mkNode(id);
nodes['set-askalert-wav-field'].style.display = 'none';
const $ = (id) => nodes[id] || null;
const selRefs = {};
const modalAlerts = [];           // 弹过的模态框（alert）文案
const alert = (m) => { modalAlerts.push(String(m)); };
let narrow = false;
const isNarrow = () => narrow;
let selectOnChange = null;
// makeSelect 的替身：只保留 get/set 与「用户选中」回调
function makeSelect(hostId, key, options, onChange) {
  const ref = { value: options.length ? options[0].v : null,
    set(v) { this.value = v; }, get() { return this.value; } };
  selRefs[key] = ref; selectOnChange = onChange; return ref;
}
function chooseValue(v) { selRefs.askalertSound.set(v); selectOnChange(v); }
// 服务端替身：aa＝已保存的 askAlert 段；reject＝保存被拒（模拟 400）；
// preview＝试听响应（带 error 就是 400）
const srv = { aa: { enabled: true, afterSec: 240, text: '', onAppear: true,
  onAppearSound: 'notify', onAppearWav: '' }, reject: null, preview: null };
const asks = [];
function clone(o) { return JSON.parse(JSON.stringify(o)); }
function api(path, opts) {
  if (!opts) return Promise.resolve({ askAlert: clone(srv.aa) });
  const body = JSON.parse(opts.body);
  asks.push({ path, body });
  if (path === '/api/alert/sound') {
    if (srv.preview && srv.preview.error) return Promise.reject(new Error(srv.preview.error));
    return Promise.resolve(srv.preview || { played: 'beep', mode: 'system' });
  }
  if (srv.reject) return Promise.reject(new Error(srv.reject));
  Object.assign(srv.aa, body.askAlert || {});
  return Promise.resolve({ askAlert: clone(srv.aa) });
}
const wavHint = () => nodes['set-askalert-wav-state'].textContent || '';
const wavBad = () => nodes['set-askalert-wav-state'].classList.contains('bad');
`

// runAskAlertUI 跑设置页那一段真实代码：每个 chunk 一次 RunString，之间把 goja
// 的微任务（async/await 链）跑完，模拟浏览器事件之间的让出。
func runAskAlertUI(t *testing.T, chunks ...string) {
	t.Helper()
	raw, err := os.ReadFile("web/app-settings.js")
	if err != nil {
		t.Fatalf("读 web/app-settings.js 失败: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "/* ---------- 常规页：询问提醒")
	end := strings.Index(src, "/* ---------- 发送页：排队消息自动续发")
	if start < 0 || end <= start {
		t.Fatalf("找不到「常规页：询问提醒」那段（start=%d end=%d）", start, end)
	}
	vm := goja.New()
	if _, err := vm.RunString(askAlertUIHarness); err != nil {
		t.Fatalf("harness 跑不起来: %v", err)
	}
	if _, err := vm.RunString(src[start:end]); err != nil {
		t.Fatalf("设置页询问提醒段跑不起来: %v", err)
	}
	for _, c := range chunks {
		if _, err := vm.RunString(c); err != nil {
			t.Fatal(err)
		}
		for i := 0; i < 8; i++ { // 让 async/await 与 Promise 链走完
			if _, err := vm.RunString("void 0"); err != nil {
				t.Fatal(err)
			}
		}
	}
}

// 切到「自定义 WAV 文件」：提交的是档位本身，路径框出现，且不弹模态框。
// 路径为空时路径框下方要有行内提示（用户随即就能填），不是报错。
func TestAskAlertUICustomSwitchNoModal(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]); chooseValue('custom');`,
		`assert(modalAlerts.length === 0, '切到自定义档不该弹模态框，实际弹了：' + modalAlerts.join(' | '));
		 assert(asks.length === 1 && asks[0].body.askAlert.onAppearSound === 'custom',
		   '切档应提交 onAppearSound=custom，实际 ' + JSON.stringify(asks));
		 assert(asks[0].body.askAlert.onAppearWav === undefined, '切档不该顺手提交空路径（空路径＋custom 曾被判非法）');
		 assert(nodes['set-askalert-wav-field'].style.display === '', '选自定义后路径框要显示出来，用户才好填');
		 assert(/请填写|未填写/.test(wavHint()), '路径为空时要在路径框下方给行内提示，实际：' + wavHint());
		 assert(!wavBad(), '「还没填」是正常中间态，不该用告警样式：' + wavHint());
		 assert(srv.aa.onAppearSound === 'custom', '服务端应已存下 custom');
		 assert(nodes['set-askalert-wav'].value === '', '路径框应保持空，等用户填');`)
}

// 保存真的被拒时（例如路径填了但不可用）：错误落在路径框下方，不弹模态框。
// 这就是用户看到的那个「保存失败」弹窗要消失的地方。
func TestAskAlertUISaveFailureIsInline(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]); chooseValue('custom');`,
		`srv.reject = '自定义 WAV 路径不可用：文件打不开（no such file）';
		 nodes['set-askalert-wav'].value = 'D:\\nope\\missing.wav';
		 nodes['set-askalert-wav'].onchange();`,
		`assert(modalAlerts.length === 0, '保存被拒不该弹模态框，实际弹了：' + modalAlerts.join(' | '));
		 assert(/保存失败/.test(wavHint()) && /WAV/.test(wavHint()), '原因要写在路径框下方（行内），实际：' + wavHint());
		 assert(wavBad(), '行内错误要有可见的警示样式');
		 assert(nodes['set-askalert-wav'].value === 'D:\\nope\\missing.wav',
		   '失败后别清掉用户刚填的路径（他正要改它），实际：' + nodes['set-askalert-wav'].value);
		 assert(srv.aa.onAppearWav === '', '服务端没存进非法路径，实际：' + srv.aa.onAppearWav);
		 assert(nodes['set-askalert-wav-field'].style.display === '', '报错说明必须可见（路径框不能藏起来）');`)
}

// 切档这个动作被后端拒绝（极老版本／别的校验）时同样行内说明，不弹模态框。
func TestAskAlertUISwitchRejectedInline(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]);
		 srv.reject = '自定义 WAV 路径不可用：没有填 WAV 文件路径'; chooseValue('custom');`,
		`assert(modalAlerts.length === 0, '切档被拒不该弹模态框，实际弹了：' + modalAlerts.join(' | '));
		 assert(/保存失败|不可用/.test(wavHint()), '原因要写在路径框下方，实际：' + wavHint());
		 assert(nodes['set-askalert-wav-field'].style.display === '', '提示要看得到，路径框不该藏起来');`)
}

// 试听：路径为空时给友好行内提示（"还没填路径"），不弹错误框。
func TestAskAlertUIPreviewEmptyFriendly(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]); chooseValue('custom');`,
		`srv.preview = { played: 'beep', mode: 'custom', fallback: '未填写自定义 WAV 路径，暂用系统提示音' };
		 nodes['set-askalert-wav'].value = '';
		 nodes['set-askalert-sound-try'].onclick();`,
		`assert(modalAlerts.length === 0, '路径为空时试听不该弹错误框，实际弹了：' + modalAlerts.join(' | '));
		 assert(/还没填|请填写/.test(wavHint()), '要给「还没填路径」的行内提示，实际：' + wavHint());
		 assert(!wavBad(), '「还没填」不该显示成错误，实际：' + wavHint());
		 const pv = asks.filter((a) => a.path === '/api/alert/sound').pop();
		 assert(pv && pv.body.sound === 'custom' && pv.body.wav === '',
		   '试听要用当前档位＋当前路径（空串）发请求，实际 ' + JSON.stringify(pv && pv.body));`)
}

// 试听自定义路径非法：原因行内显示，不弹模态框。
func TestAskAlertUIPreviewInvalidInline(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]); chooseValue('custom');`,
		`srv.preview = { error: '自定义 WAV 路径不可用：文件打不开（no such file）' };
		 nodes['set-askalert-wav'].value = 'D:\\nope\\missing.wav';
		 nodes['set-askalert-sound-try'].onclick();`,
		`assert(modalAlerts.length === 0, '试听报错不该弹模态框，实际弹了：' + modalAlerts.join(' | '));
		 assert(/试听失败/.test(wavHint()) && /WAV/.test(wavHint()), '原因要写在路径框下方，实际：' + wavHint());
		 assert(wavBad(), '行内错误要有可见的警示样式');`)
}

// 选了合法路径并保存：服务端存下路径、界面回到正常说明（不留错误）。
func TestAskAlertUIValidPathSaves(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]); chooseValue('custom');`,
		`nodes['set-askalert-wav'].value = 'D:\\声音\\提示.wav';
		 nodes['set-askalert-wav'].onchange();`,
		`assert(modalAlerts.length === 0, '保存合法路径不该弹框，实际：' + modalAlerts.join(' | '));
		 assert(srv.aa.onAppearWav === 'D:\\声音\\提示.wav', '服务端应存下路径，实际：' + srv.aa.onAppearWav);
		 assert(!/保存失败|错误/.test(wavHint()), '成功保存后不该留错误文案，实际：' + wavHint());
		 assert(!wavBad(), '成功保存后不该留警示样式，实际：' + wavHint());`)
}

// 切回其它档：路径框收起，不再提交任何 WAV 路径。
func TestAskAlertUISwitchAwayHidesField(t *testing.T) {
	runAskAlertUI(t,
		`ensureAskAlertSoundSelect([{v:'notify',label:'n'},{v:'custom',label:'c'}]); chooseValue('custom');`,
		`chooseValue('chimes');`,
		`assert(modalAlerts.length === 0, '切档不该弹模态框，实际：' + modalAlerts.join(' | '));
		 assert(nodes['set-askalert-wav-field'].style.display === 'none', '非自定义档应收起路径框');
		 assert(srv.aa.onAppearSound === 'chimes', '应存下 chimes，实际：' + srv.aa.onAppearSound);`)
}
