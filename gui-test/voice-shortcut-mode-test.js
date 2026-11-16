/* 语音组件「触发方式」默认值与存量归一的断言（纯 Node，无浏览器、不起服务）。
 *
 * 背景（2026-11-14 上游 J2）：触发方式默认应为**点按切换**（按一下开始、再按一下
 * 结束），长按输入是可选；但早期版本没记「用户显式选过」，机器上留着一份从没主动
 * 选过的 hold（旧版默认或验证脚本遗留），表现为「默认竟然是长按」。
 *
 * 本文件不复制逻辑：用正则从 组件/voice/client.js 原样抽出 voiceShortcutMode /
 * migrateShortcutModeDefault 两个真函数，配一个 localStorage 同语义的 pref 桩跑用例；
 * 再对 client.js 做几条结构断言（init 里归一的时机、设置页记标记、默认选项文案）。
 * 运行：仓库根目录 node gui-test/voice-shortcut-mode-test.js
 */
'use strict';

const { readFileSync } = require('fs');
const pathMod = require('path');

const src = readFileSync(pathMod.join(__dirname, '..', '组件', 'voice', 'client.js'), 'utf8');

let pass = 0, fail = 0;
const ok = (cond, name, extra) => {
  if (cond) { pass++; return; }
  fail++;
  console.log('  ✗ ' + name + (extra !== undefined ? ' => ' + JSON.stringify(extra) : ''));
};

const grab = (re, what) => {
  const m = src.match(re);
  if (!m) throw new Error('抽不到：' + what + '（' + re + '）');
  return m[0];
};

/* ---------- 1. 纯函数：真函数 + pref 桩 ---------- */
const fnSrc = grab(/function voiceShortcutMode\(\) \{[\s\S]*?\n  \}/, 'voiceShortcutMode') + '\n' +
  grab(/function migrateShortcutModeDefault\(\) \{[\s\S]*?\n  \}/, 'migrateShortcutModeDefault');

// pref 桩：与 client.js 的 pref 同语义——localStorage 存 JSON 值，取不到给默认值
//（键不存在 → 默认值；存的是 JSON null → null，不回落默认）。
const mkPref = (init) => {
  const m = new Map(Object.entries(init || {}));
  return {
    get: (k, d) => (m.has(k) ? JSON.parse(JSON.stringify(m.get(k))) : d),
    set: (k, v) => m.set(k, JSON.parse(JSON.stringify(v))),
    has: (k) => m.has(k),
  };
};
const build = (pref) => new Function('pref', fnSrc + '\nreturn { voiceShortcutMode, migrateShortcutModeDefault };')(pref);

// 每条用例：[说明, 存储初值, 归一后期望的存储（undefined=不该出现该键）, 归一后模式]
const cases = [
  ['新机器：从没设置过 → 点按，且归一不动存储',
    {}, undefined, 'tap'],
  ['存量旧值 hold（没记过「显式选过」）→ 归一回点按',
    { shortcutMode: 'hold' }, 'tap', 'tap'],
  ['存量旧值 hold 但用户显式选过（有标记）→ 保持长按',
    { shortcutMode: 'hold', shortcutModeSet: true }, 'hold', 'hold'],
  ['存量 tap（无标记）→ 恒为点按，不需要改存储',
    { shortcutMode: 'tap' }, 'tap', 'tap'],
  ['存量脏值（旧版写坏的字符串）→ 判作点按，存储不改它',
    { shortcutMode: 'Hold' }, 'Hold', 'tap'],
  ['存量 JSON null → 判作点按（null 不是 hold）',
    { shortcutMode: null }, null, 'tap'],
  ['显式选过长按后又显式选回点按 → 点按',
    { shortcutMode: 'tap', shortcutModeSet: true }, 'tap', 'tap'],
];
for (const [name, init, wantStore, wantMode] of cases) {
  const pref = mkPref(init);
  const api = build(pref);
  const before = api.voiceShortcutMode(); // 归一前的读数（只做记录，不做断言）
  api.migrateShortcutModeDefault();
  const gotMode = api.voiceShortcutMode();
  const gotStore = pref.has('shortcutMode') ? pref.get('shortcutMode', undefined) : undefined;
  ok(gotMode === wantMode, name + '：模式', { before, got: gotMode, want: wantMode });
  ok(JSON.stringify(gotStore) === JSON.stringify(wantStore), name + '：归一后的存储值',
    { got: gotStore, want: wantStore });
  // 归一必须是幂等的（每次 init 都会跑一遍）
  api.migrateShortcutModeDefault();
  ok(api.voiceShortcutMode() === gotMode, name + '：归一幂等');
}

/* ---------- 2. 结构断言：真代码里那几处接得上 ---------- */
// 2.1 默认值就是 'tap'（不是靠别处兜）
ok(/pref\.get\('shortcutMode', 'tap'\)/.test(src), "voiceShortcutMode 的默认值应为 'tap'");

// 2.2 init() 里先归一再注册快捷键（顺序反了会先把老的 hold 推进 voicetype.json）
const initBody = src.slice(src.indexOf('    init() {'), src.indexOf('    dispose() {'));
ok(initBody.length > 0, '能切出 init() 函数体');
const iMig = initBody.indexOf('migrateShortcutModeDefault();');
const iEngine = initBody.indexOf('installShortcutEngine();');
const iHK = initBody.indexOf('JX.registerHotkey(');
ok(iMig >= 0, 'init() 里要调用 migrateShortcutModeDefault()');
ok(iMig >= 0 && iEngine > iMig, '归必须在 installShortcutEngine() 之前', { iMig, iEngine });
ok(iMig >= 0 && iHK > iMig, '归必须在 JX.registerHotkey( 之前', { iMig, iHK });

// 2.3 设置页：用户显式选择要打标记（否则下次 init 会把它当存量归一回点按）
ok(/pref\.set\('shortcutModeSet', true\)/.test(src), '设置页改触发方式时必须写 shortcutModeSet 标记');
const onchangeIdx = src.indexOf("pref.set('shortcutMode', modeSel.value === 'hold' ? 'hold' : 'tap');");
ok(onchangeIdx > 0, '设置页要写入所选触发方式');
ok(src.indexOf("pref.set('shortcutModeSet', true)", onchangeIdx) > onchangeIdx,
  'shortcutModeSet 应在同一次 onchange 里、紧跟写入所选方式之后');

// 2.4 设置页文案把「默认」说清楚（默认项 = 点按）
ok(/m1\.value = 'tap'; m1\.textContent = '点按切换（默认/.test(src), '默认选项文案应标明「默认」');

console.log((fail === 0 ? 'PASS' : 'FAIL') + ' ' + pass + ' passed, ' + fail + ' failed');
process.exit(fail === 0 ? 0 : 1);
