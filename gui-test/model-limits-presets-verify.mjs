// 模型编辑「上下文窗口 / 最大输出」预设档位验证（隔离实例 + 无头 Edge + CDP 真实点击）。
// 背景：拉回来的模型常常不带 contextWindow / maxOutput（服务端不报），用户只能手敲
// 六七位数字。要求两个字段变成「可输入的下拉」：能直接输数字（0/留空语义不变），
// 展开后是「简称（精确值）」的预设档位，点一下即填精确值并走原来的保存路径。
// 用例：
//   P1 两个字段都是组合框（数字框 + 展开按钮），各自的档位清单条数与关键行正确
//   P2 点「1M（1,048,576）」→ 输入框是原始数字 1048576（不是「1M」）；键盘 ↓↓Enter
//      选到 8K（8192）；保存后 /api/config 里两个值都对
//   P3 手输 123456 能存；已有自定义值时打开下拉不会被改成档位；Esc 只收浮层
//   P4 清空＝未声明（保存后 contextWindow / maxOutput 键都不出现，与改前一致）
//   P5 深色一张、460px 窄屏一张；浮层不越界、页面不横向溢出
// 运行：仓库根目录 node gui-test/model-limits-presets-verify.mjs
import { newTab, read, sleep, closeTab, shot } from './cdp.mjs';
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync, accessSync, readFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const BASE = 'http://127.0.0.1:8834';
const CDP_HTTP = 'http://127.0.0.1:9223';
const ROOT = fileURLToPath(new URL('..', import.meta.url));
const TMP = join(tmpdir(), 'jx-tokpreset-' + Date.now().toString(36));
const SHOTS = join(ROOT, '.jxai', 'tmp', 'model-limits-presets-shots');
mkdirSync(SHOTS, { recursive: true });
const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

// 两个组合框都在展开的模型编辑面板里，按出现顺序取（0=上下文窗口，1=最大输出）
const COMBO = (i) => `document.querySelectorAll('#prov-list .mr-edit .tok-combo')[${i}]`;
const INPUT = (i) => `${COMBO(i)}.querySelector('input')`;

let edgeProc = null;
async function ensureEdge() {
  try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e) { /* spawn */ }
  const cands = [
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  ];
  const edge = cands.find((p) => { try { accessSync(p); return true; } catch (e2) { return false; } });
  if (!edge) throw new Error('没找到 msedge.exe');
  edgeProc = spawn(edge, ['--headless=new', '--remote-debugging-port=9223',
    '--user-data-dir=' + join(TMP, 'edge'), 'about:blank'], { stdio: 'ignore' });
  for (let i = 0; i < 30; i++) {
    await sleep(500);
    try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e2) { /* wait */ }
  }
  throw new Error('Edge CDP 没起来');
}

let serverProc = null;
async function ensureServer() {
  try { const r = await fetch(BASE + '/api/status'); if (r.ok) return; } catch (e) { /* spawn */ }
  mkdirSync(join(TMP, 'ws'), { recursive: true });
  const exe = join(TMP, 'server.exe');
  execSync(`go build -o "${exe}" .`, { cwd: ROOT, stdio: 'inherit' });
  // 一家自定义提供方、一把 Key、一个「拉回来但没有上下文/输出上限」的模型
  writeFileSync(join(TMP, 'config.json'), JSON.stringify({
    port: 8834, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
    workspace: join(TMP, 'ws'), updateURL: '',
    providers: [{
      id: 'cpreset1', name: '测试渠道', baseURL: 'http://127.0.0.1:9/v1',
      keys: [{
        id: 'k1', label: '默认分组', apiKey: 'sk-test',
        models: [{ id: 'm-unknown', name: '未知模型' }],
      }],
    }],
  }, null, 2));
  serverProc = spawn(exe, ['-config', 'config.json', '-noopen'], { cwd: TMP, stdio: 'ignore' });
  for (let i = 0; i < 40; i++) {
    await sleep(500);
    try { const r = await fetch(BASE + '/api/status'); if (r.ok) return; } catch (e) { /* wait */ }
  }
  throw new Error('server 没起来');
}

async function tread(t, expr, ms = 6000) {
  return await Promise.race([read(t, expr), new Promise((r) => setTimeout(() => r(undefined), ms))]);
}
async function rectJS(t, expr) {
  return await tread(t, `(() => { const el = ${expr}; if (!el) return null;
    const b = el.getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2, l: b.left, r: b.right, t: b.top }; })()`);
}
async function clickXY(t, c) {
  for (const type of ['mousePressed', 'mouseReleased']) {
    await t.send('Input.dispatchMouseEvent', {
      type, x: Math.round(c.x), y: Math.round(c.y), button: 'left', clickCount: 1,
    });
    await sleep(40);
  }
}
async function clickJS(t, expr, scroll = true) {
  if (scroll) {
    // 卡片内容常在滚动容器里（模型编辑面板可能在视口下方）：先居中再点，
    // 否则坐标落在视口外，鼠标事件等于没点
    await tread(t, `(() => { const el = ${expr}; if (el) el.scrollIntoView({ block: 'center' }); return 1; })()`);
    await sleep(250);
  }
  const c = await rectJS(t, expr);
  if (!c) throw new Error('not found: ' + expr);
  await clickXY(t, c);
}
const VK = { ArrowDown: 40, ArrowUp: 38, Enter: 13, Escape: 27, Tab: 9, Delete: 46 };
async function key(t, name, modifiers = 0) {
  const vk = VK[name];
  if (!vk) throw new Error('unknown key ' + name);
  for (const type of ['keyDown', 'keyUp']) {
    await t.send('Input.dispatchKeyEvent', {
      type, key: name, code: name, windowsVirtualKeyCode: vk, nativeVirtualKeyCode: vk, modifiers,
    });
    await sleep(60);
  }
}
const focusCombo = (t, i) => tread(t, `(() => { ${INPUT(i)}.focus(); return 1; })()`);
async function openCombo(t, i) {
  await focusCombo(t, i);
  await clickJS(t, `${COMBO(i)}.querySelector('.tok-combo-btn')`);
  await sleep(250);
}
// 打开模型编辑面板（设置 → 模型 → 展开渠道卡片 → 点模型行）
async function openModelEditor(t) {
  await tread(t, `openSettings('models')`);
  await sleep(700);
  await clickJS(t, `document.querySelector('#prov-list .pv-head')`);
  await sleep(400);
  await clickJS(t, `document.querySelector('#prov-list .mr-line')`);
  await sleep(400);
}
const cfgModel = async () => {
  const c = await (await fetch(BASE + '/api/config')).json();
  const p = (c.providers || []).find((x) => x.id === 'cpreset1') || {};
  const k = (p.keys || [])[0] || {};
  return ((k.models || p.models || [])[0]) || {};
};
function statSize(p) { try { return readFileSync(p).length; } catch (e) { return 0; } }

await ensureEdge();
await ensureServer();

const t = await newTab(BASE + '/', 1280, 900);
try {
  await sleep(1200);
  // 首启会弹「接入向导」盖在设置页之上（隔离实例没有公司服务端）：先跳过
  await tread(t, `(() => { const b = document.getElementById('wz-skip');
    const m = b && b.closest('.modal');
    if (b && m && !m.classList.contains('hidden')) b.click();
    return 1; })()`);
  await sleep(400);

  // ---------- P1 控件形态与档位清单 ----------
  await openModelEditor(t);
  const shape = await tread(t, `(() => {
    const combos = [...document.querySelectorAll('#prov-list .mr-edit .tok-combo')];
    return {
      n: combos.length,
      isNum: combos.every((c) => c.querySelector('input.tok-combo-in[type=number]')),
      hasBtn: combos.every((c) => !!c.querySelector('.tok-combo-btn')),
      min0: combos.every((c) => c.querySelector('input').getAttribute('min') === '0'),
      labels: [...document.querySelectorAll('#prov-list .mr-edit .pf-label')].map((x) => x.textContent),
    };
  })()`);
  ok('P1 两个字段都是「数字框 + 展开按钮」的组合框',
    shape && shape.n === 2 && shape.isNum && shape.hasBtn && shape.min0, JSON.stringify(shape));
  ok('P1 两个字段的标签仍是原来的（上下文窗口 / 最大输出）',
    shape && shape.labels.some((s) => s.includes('上下文窗口')) && shape.labels.some((s) => s.includes('最大输出')),
    JSON.stringify(shape && shape.labels));

  await openCombo(t, 0);
  const ctxOpts = await tread(t, `(() => {
    const pop = document.querySelector('.tok-combo-pop');
    if (!pop) return null;
    return { n: pop.querySelectorAll('.tok-opt').length,
      texts: [...pop.querySelectorAll('.tok-opt')].map((b) => b.textContent),
      role: pop.getAttribute('role'),
      optRole: pop.querySelector('.tok-opt').getAttribute('role'),
      expanded: ${INPUT(0)}.getAttribute('aria-expanded') };
  })()`);
  ok('P1 上下文窗口 9 个档位，含「1M（1,048,576）」两段显示',
    ctxOpts && ctxOpts.n === 9 && ctxOpts.role === 'listbox' && ctxOpts.optRole === 'option'
      && ctxOpts.expanded === 'true'
      && ctxOpts.texts.includes('1M（1,048,576）') && ctxOpts.texts.includes('32K（32,768）'),
    ctxOpts ? JSON.stringify(ctxOpts.texts) : 'no popup');
  await key(t, 'Escape');
  await sleep(200);

  await openCombo(t, 1);
  const maxOpts = await tread(t, `(() => {
    const pop = document.querySelector('.tok-combo-pop');
    if (!pop) return null;
    return { n: pop.querySelectorAll('.tok-opt').length,
      texts: [...pop.querySelectorAll('.tok-opt')].map((b) => b.textContent) };
  })()`);
  ok('P1 最大输出 10 个档位，含「384K（393,216）」与「128K（128,000）」两种口径',
    maxOpts && maxOpts.n === 10 && maxOpts.texts.includes('384K（393,216）')
      && maxOpts.texts.includes('128K（128,000）') && maxOpts.texts.includes('128K（131,072）'),
    maxOpts ? JSON.stringify(maxOpts.texts) : 'no popup');
  await key(t, 'Escape');
  await sleep(200);

  // ---------- P2 点档位即填精确值 → 保存 → /api/config ----------
  await openCombo(t, 0);
  const optRect = await tread(t, `(() => {
    const b = [...document.querySelectorAll('.tok-combo-pop .tok-opt')].find((x) => x.dataset.v === '1048576');
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  })()`);
  if (!optRect) throw new Error('没找到 1048576 档位');
  await clickXY(t, optRect);
  await sleep(300);
  const picked = await tread(t, `(() => ({
    v: ${INPUT(0)}.value, pop: !!document.querySelector('.tok-combo-pop'),
    draft: provDraft[0].keys[0].models[0].contextWindow,
    focused: document.activeElement === ${INPUT(0)},
  }))()`);
  ok('P2 点「1M（1,048,576）」→ 输入框是原始数字 1048576、浮层收起、草稿即时更新',
    picked && picked.v === '1048576' && picked.pop === false && picked.draft === 1048576 && picked.focused,
    JSON.stringify(picked));

  // 键盘：↓ 打开、↓ 移动、Enter 确认（最大输出 → 8K / 8192）
  await focusCombo(t, 1);
  await key(t, 'ArrowDown');
  await sleep(250);
  const opened = await tread(t, `(() => {
    const pop = document.querySelector('.tok-combo-pop');
    return pop ? { open: true, active: [...pop.querySelectorAll('.tok-opt')].findIndex((b) => b.classList.contains('active')) } : { open: false };
  })()`);
  await key(t, 'ArrowDown');
  await sleep(150);
  const moved = await tread(t, `[...document.querySelectorAll('.tok-combo-pop .tok-opt')].findIndex((b) => b.classList.contains('active'))`);
  await key(t, 'Enter');
  await sleep(250);
  const kb = await tread(t, `(() => ({ v: ${INPUT(1)}.value, pop: !!document.querySelector('.tok-combo-pop') }))()`);
  ok('P2 键盘 ↓ 展开（高亮第一档）、↓ 移动、Enter 确认 → 8192',
    opened.open === true && opened.active === 0 && moved === 1 && kb && kb.v === '8192' && kb.pop === false,
    JSON.stringify({ opened, moved, kb }));

  await clickJS(t, `document.querySelector('#set-save')`);
  await sleep(1500);
  const m1 = await cfgModel();
  ok('P2 保存后 /api/config：contextWindow=1048576、maxOutput=8192',
    m1.contextWindow === 1048576 && m1.maxOutput === 8192, JSON.stringify(m1));

  // ---------- P3 手输自定义值；打开下拉不改写；Esc 只收浮层 ----------
  await openModelEditor(t);
  await focusCombo(t, 0);
  await tread(t, `(() => { ${INPUT(0)}.select(); return 1; })()`);
  await t.send('Input.insertText', { text: '123456' });
  await sleep(250);
  const typed = await tread(t, `${INPUT(0)}.value`);
  await openCombo(t, 0);
  const afterOpen = await tread(t, `(() => ({
    v: ${INPUT(0)}.value,
    curMarked: [...document.querySelectorAll('.tok-combo-pop .tok-opt')].filter((b) => b.classList.contains('cur')).length,
  }))()`);
  await key(t, 'Escape');
  await sleep(250);
  const afterEsc = await tread(t, `(() => ({
    pop: !!document.querySelector('.tok-combo-pop'),
    settingsOpen: !document.getElementById('settings').classList.contains('hidden'),
    v: ${INPUT(0)}.value,
  }))()`);
  ok('P3 手输 123456：打开下拉不改写、无档位被标成当前值；Esc 只收浮层不关设置',
    typed === '123456' && afterOpen.v === '123456' && afterOpen.curMarked === 0
      && afterEsc.pop === false && afterEsc.settingsOpen === true && afterEsc.v === '123456',
    JSON.stringify({ typed, afterOpen, afterEsc }));
  await clickJS(t, `document.querySelector('#set-save')`);
  await sleep(1500);
  const m2 = await cfgModel();
  ok('P3 手输的 123456 存进 contextWindow（原保存路径不变）', m2.contextWindow === 123456, JSON.stringify(m2));

  // ---------- P4 清空＝未声明 ----------
  await openModelEditor(t);
  for (const i of [0, 1]) {
    await focusCombo(t, i);
    await tread(t, `(() => { ${INPUT(i)}.select(); return 1; })()`);
    await key(t, 'Delete');
    await sleep(200);
  }
  const cleared = await tread(t, `(() => ({
    ctx: ${INPUT(0)}.value, max: ${INPUT(1)}.value,
    draft: JSON.stringify(provDraft[0].keys[0].models[0]),
  }))()`);
  await clickJS(t, `document.querySelector('#set-save')`);
  await sleep(1500);
  const m3 = await cfgModel();
  ok('P4 清空两格＝未声明（草稿归 0，保存后配置里两个键都不出现）',
    cleared.ctx === '' && cleared.max === ''
      && JSON.parse(cleared.draft).contextWindow === 0 && JSON.parse(cleared.draft).maxOutput === 0
      && m3.contextWindow === undefined && m3.maxOutput === undefined,
    JSON.stringify({ cleared, m3 }));

  // ---------- P5 浮层几何 + 深色/窄屏截图 ----------
  await openModelEditor(t);
  await openCombo(t, 1);
  const geoWide = await tread(t, `(() => {
    const pop = document.querySelector('.tok-combo-pop'); if (!pop) return null;
    const r = pop.getBoundingClientRect();
    return { l: r.left, r: r.right, t: r.top, b: r.bottom, vw: innerWidth, vh: innerHeight };
  })()`);
  await tread(t, `lsSet('jx.theme','dark'); applyTheme(); 1`);
  await sleep(500);
  const darkShot = join(SHOTS, 'presets-dark-1280.png');
  await shot(t, darkShot);

  await tread(t, `lsSet('jx.theme','light'); applyTheme(); 1`);
  await t.send('Emulation.setDeviceMetricsOverride', { width: 460, height: 900, deviceScaleFactor: 1, mobile: false });
  await sleep(600);
  await tread(t, `window.dispatchEvent(new Event('resize')); 1`); // 视口变了：让浮层重摆
  await sleep(300);
  const geoNarrow = await tread(t, `(() => {
    const pop = document.querySelector('.tok-combo-pop'); if (!pop) return null;
    const r = pop.getBoundingClientRect();
    const de = document.documentElement;
    return { l: r.left, r: r.right, w: r.width, vw: innerWidth,
      overflowX: de.scrollWidth - de.clientWidth, popOverflowX: pop.scrollWidth - pop.clientWidth };
  })()`);
  const narrowShot = join(SHOTS, 'presets-narrow-460.png');
  await shot(t, narrowShot);
  ok('P5 浮层在 1280 与 460 窄屏都不越界、不横向溢出',
    !!geoWide && geoWide.l >= 0 && geoWide.r <= geoWide.vw && geoWide.t >= 0 && geoWide.b <= geoWide.vh
      && !!geoNarrow && geoNarrow.l >= 0 && geoNarrow.r <= geoNarrow.vw
      && geoNarrow.overflowX <= 0 && geoNarrow.popOverflowX <= 0,
    JSON.stringify({ geoWide, geoNarrow }));
  ok('P5 两张截图已落盘（深色 1280 / 窄屏 460）',
    statSize(darkShot) > 1000 && statSize(narrowShot) > 1000, darkShot + ' , ' + narrowShot);

  const alive = await tread(t, `(() => ({ cards: document.querySelectorAll('#prov-list .prov-card').length,
    rows: document.querySelectorAll('#prov-list .model-row').length }))()`);
  ok('P5 交互后卡片/模型行仍完好', alive && alive.cards === 1 && alive.rows === 1, JSON.stringify(alive));
} finally {
  await closeTab(t);
  if (t) { try { t.ws.close(); } catch (e) { /* already closed */ } } // 不关 ws，node 事件循环不空、脚本跑完不退
  try { if (serverProc) execSync(`taskkill /PID ${serverProc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ }
  try { if (edgeProc) execSync(`taskkill /PID ${edgeProc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ }
}

const fails = results.filter((r) => !r.pass);
console.log(`\n==== ${results.length - fails.length}/${results.length} PASS ====`);
if (fails.length) fails.forEach((f) => console.log('FAILED: ' + f.name));
process.exit(fails.length ? 1 : 0); // 显式收尾：等事件循环自然退出会挂住（CDP 的 ws 可能仍连着一个实例）
