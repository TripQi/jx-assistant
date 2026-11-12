// 模型与思考档位卡片（滑杆）真机验证：web/app-chat.js 的「模型与思考档位卡片」节 +
// index.html 的 #model-card + style.css 的 .model-card 一节。
// 自包含——需要时自动构建隔离实例（mock 模型、独立 data/工作区，落 .jxai/tmp）
// 并拉起无头 Edge（CDP 9223），沿用 cdp.mjs 约定。断言与截图落 gui-test-screenshots/thinkslider-*。
//
// 覆盖（对应本次需求逐条）：
//   A 输入框下方只有一枚芯片，显示「模型名 · 档位」；旧的档位二级菜单（#model-pop/
//     #model-sub-pop/.mp-lv/.lv-hit）已从 DOM 彻底消失
//   B 点芯片打开卡片：第一层＝大字档位 + 模型行 + 滑杆 + 跟随服务端默认；初始空档位＝Default
//   C 关掉「跟随服务端默认」：恢复上次手选档位（无历史→Medium），并落库
//   D 拖到每一档：拖动过程中零 PATCH、松手恰好一次 PATCH，且会话 thinking 落库正确
//     （六档 None/Low/Medium/High/Xhigh/Max，第五档显示 Xhigh、内部值 xhigh）
//   E 点轨道任意位置＝跳档并立即提交
//   F 键盘左右键各切一档并提交；role=slider + aria-valuemin/max/now/text
//   G 跟随服务端默认：thinking 为空串、滑杆隐藏、大字与芯片显示 Default
//   H 再关开关：恢复上一次手选档位（不是 Medium 兜底）
//   I 点模型名翻到第二层：按供应商分组（灰字标题不可点、模型缩进、顺序跟接口一致）、
//     同名模型不再带渠道后缀、当前模型带勾、无「默认模型」条目
//   J 点某一项切模型：档位保持不变，回到第一层
//   K 刷新图标：重新 GET /api/models（loading 转圈），失败给可见提示
//   L 触摸拖动（pointer 事件）同样可用
//   M 深色主题 / 460px 窄屏：卡片不越界、滑杆不横向溢出
//   N Esc 与点外部关闭
//   O 右栏子对话那枚芯片点开同一张卡
// 视觉专项（B5–B8/C4）：档位大字居中、20–22px、主题蓝、标题区无灰底带、刷新图标仍在右上角。
// 运行：仓库根目录 node gui-test/think-slider-verify.mjs
import { newTab, read, click, shot, sleep, closeTab } from './cdp.mjs';
import { mkdirSync, writeFileSync, rmSync, accessSync } from 'node:fs';
import { spawn, execSync } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const SCREEN = join(ROOT, 'gui-test-screenshots');
const PORT = 8836;
const BASE = 'http://127.0.0.1:' + PORT;
const CDP_HTTP = 'http://127.0.0.1:9223';
const TMP = join(ROOT, '.jxai', 'tmp', 'jx-thinkslider-test');
mkdirSync(SCREEN, { recursive: true });
const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

// 档位：内部值（发给后端/落库）与显示文案（UI 一律首字母大写：第五档是 Xhigh）
const LV = ['none', 'low', 'medium', 'high', 'xhigh', 'max'];
const LV_LABEL = ['None', 'Low', 'Medium', 'High', 'Xhigh', 'Max'];

// ---------- 环境自举：隔离实例 + 无头 Edge（同 model-subpop-verify 模式） ----------
let serverProc = null, edgeWasOurs = false, edgePid = 0, sessionId = '';
async function up() {
  let alive = false;
  try { const r = await fetch(BASE + '/api/status'); alive = r.ok; } catch (e) { /* down */ }
  if (!alive) {
    rmSync(TMP, { recursive: true, force: true });
    mkdirSync(join(TMP, 'ws'), { recursive: true });
    const exe = join(TMP, 'server.exe');
    execSync(`go build -o "${exe}" .`, { cwd: ROOT, stdio: 'inherit' });
    writeFileSync(join(TMP, 'config.json'), JSON.stringify({
      port: PORT, bindHost: '127.0.0.1',
      baseURL: 'mock', apiKey: '', model: 'glm-5.3',
      wizardDone: true, // 首启接入向导不弹（弹了会挡住输入区）
      // 两个供应商（顺序＝「设置 · 模型」里的顺序）：卡片第二层按供应商分组的顺序要跟着它
      providers: [{
        id: 'mock', name: 'Mock 服务', baseURL: 'mock', apiKey: '',
        // 同一供应商内两把 Key 拉到同名模型（deepseek-flash）：分组后不再加渠道后缀
        keys: [
          { id: 'k1', label: '默认分组', models: [{ id: 'glm-5.3', name: 'GLM-5.3' }, { id: 'deepseek-flash', name: 'deepseek-flash' }] },
          { id: 'k2', label: 'SnowAI', models: [{ id: 'gpt-6.1-terra', name: '6.1 Terra' }, { id: 'deepseek-flash', name: 'deepseek-flash' }] },
        ],
      }, {
        id: 'mock-alt', name: '备用渠道', baseURL: 'mock', apiKey: '',
        models: [{ id: 'glm-5.3', name: 'GLM-5.3' }],
      }],
      workspace: join(TMP, 'ws'), updateURL: '',
    }));
    serverProc = spawn(exe, ['-config', 'config.json', '-noopen'], { cwd: TMP, stdio: 'ignore' });
    for (let i = 0; i < 40; i++) {
      await sleep(500);
      try { const r = await fetch(BASE + '/api/status'); if (r.ok) { alive = true; break; } } catch (e) { /* wait */ }
    }
  }
  if (!alive) throw new Error('隔离实例没起来（端口 ' + PORT + '）');
  // 建一个会话：档位要 PATCH 到会话上才能验「落库正确」
  const r = await fetch(BASE + '/api/sessions', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ title: '档位滑杆验证', workspace: join(TMP, 'ws'), mode: 'office', provider: 'mock', providerKey: 'k1', model: 'glm-5.3', thinking: '' }),
  });
  const sess = await r.json();
  sessionId = sess.id;
  if (!sessionId) throw new Error('建会话失败：' + JSON.stringify(sess).slice(0, 200));
  try { await fetch(CDP_HTTP + '/json/version'); } catch (e) {
    const edge = [
      'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
      'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
    ].find((p) => { try { accessSync(p); return true; } catch (e2) { return false; } });
    if (!edge) throw new Error('没找到 msedge.exe');
    const edgeProc = spawn(edge, [
      '--headless=new', '--remote-debugging-port=9223',
      '--user-data-dir=' + join(TMP, 'edge-profile'), 'about:blank',
    ], { stdio: 'ignore' });
    edgeWasOurs = true; edgePid = edgeProc.pid;
    for (let i = 0; i < 30; i++) {
      await sleep(500);
      try { await fetch(CDP_HTTP + '/json/version'); break; } catch (e2) { /* wait */ }
    }
  }
}
function down() {
  if (serverProc) { try { execSync(`taskkill /PID ${serverProc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* already gone */ } }
  if (edgeWasOurs && edgePid) { try { execSync(`taskkill /PID ${edgePid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* already gone */ } }
}

// ---------- 工具 ----------
// 会话详情实际返回 {meta, messages}：档位/模型都在 meta 上
const sess = async () => {
  const d = await (await fetch(BASE + '/api/sessions/' + sessionId)).json();
  return d.meta || {};
};
const chipText = (t) => read(t, `document.getElementById('model-chip-label').textContent`);
const bigText = (t) => read(t, `document.getElementById('mc-big').textContent`);
const bigBox = (t) => read(t, `(() => ({
  card: !document.getElementById('model-card').classList.contains('hidden'),
  layer1: !document.getElementById('mc-lv').classList.contains('hidden'),
  layer2: !document.getElementById('mc-models').classList.contains('hidden'),
  sliderDisplay: getComputedStyle(document.getElementById('mc-slider')).display,
  now: document.getElementById('mc-slider').getAttribute('aria-valuenow'),
  text: document.getElementById('mc-slider').getAttribute('aria-valuetext'),
  follow: document.getElementById('mc-follow').checked,
  err: (document.getElementById('mc-err').classList.contains('hidden') ? '' : document.getElementById('mc-err').textContent),
}))()`);
const trackGeo = (t) => read(t, `(() => {
  const r = document.getElementById('mc-track').getBoundingClientRect();
  return { left: r.left, right: r.right, width: r.width, y: r.top + r.height / 2 };
})()`);
// 第一层标题区的几何/配色：档位大字与模型行是否与卡片水平中心对齐（贴字实测，不是盒子对齐）、
// 字号是否落在 20–22px、颜色是否主题强调色（蓝）、标题区与大字都没有底色（去掉灰底带）、
// 刷新图标是否仍然钉在右上角
const headerGeo = (t) => read(t, `(() => {
  const card = document.getElementById('model-card');
  const c = card.getBoundingClientRect();
  const big = document.getElementById('mc-big');
  const rg = document.createRange(); rg.selectNodeContents(big);
  const tb = rg.getBoundingClientRect();
  const nameEl = document.getElementById('mc-model-name');
  const nr = document.createRange(); nr.selectNodeContents(nameEl);
  const nb = nr.getBoundingClientRect();
  const go = document.querySelector('#mc-model .mc-go').getBoundingClientRect();
  const cs = getComputedStyle(big);
  const top = card.querySelector('.mc-top');
  const rf = document.getElementById('mc-refresh').getBoundingClientRect();
  const rgb = (cs.color.match(/\\d+/g) || []).map(Number);
  const transparent = (v) => v === 'rgba(0, 0, 0, 0)' || v === 'transparent' || v === 'rgba(0, 0, 0, 0) none';
  return {
    cardCx: c.left + c.width / 2, cardTop: c.top, cardRight: c.right,
    bigTextCx: tb.left + tb.width / 2, bigText: big.textContent,
    rowCx: (Math.min(nb.left, go.left) + Math.max(nb.right, go.right)) / 2,
    fontSize: parseFloat(cs.fontSize), fontWeight: cs.fontWeight, color: cs.color, rgb: rgb,
    bigBg: cs.backgroundColor, topBg: getComputedStyle(top).backgroundColor,
    noBand: transparent(cs.backgroundColor) && transparent(getComputedStyle(top).backgroundColor),
    refreshRight: rf.right, refreshTop: rf.top, refreshW: rf.width,
  };
})()`);
const lvX = (g, i) => g.left + (g.width * i) / 5;

async function clickEl(t, expr) {
  const b = await read(t, `(() => { const el = ${expr}; if (!el) return null; el.scrollIntoView({ block: 'center' }); const r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
  if (!b) throw new Error('not found: ' + expr);
  await click(t, b.x, b.y);
  await sleep(280);
  return b;
}
const mouse = (t, type, x, y, extra = {}) => t.send('Input.dispatchMouseEvent', Object.assign({ type, x: Math.round(x), y: Math.round(y) }, extra));
const pressKey = async (t, key) => {
  const spec = { Escape: 27, ArrowLeft: 37, ArrowRight: 39 }[key];
  for (const type of ['keyDown', 'keyUp']) {
    await t.send('Input.dispatchKeyEvent', {
      type, key, code: key, windowsVirtualKeyCode: spec, nativeVirtualKeyCode: spec,
    });
    await sleep(50);
  }
};

// 隔离实例没配密钥，首启接入向导会在启动后一两秒自动弹出、盖住中间区域；
// 截图要干净：轮询几秒，看到就点「跳过，手动配置」（真点），关掉后不会再弹
async function dismissWizard(t) {
  for (let i = 0; i < 10; i++) {
    const vis = await read(t, `(() => { const m = document.getElementById('welcome-modal'); return m && !m.classList.contains('hidden') ? 1 : 0; })()`);
    if (vis) {
      await clickEl(t, `document.getElementById('wz-skip')`);
      await sleep(400);
      return true;
    }
    await sleep(300);
  }
  return false;
}
const wizardOpen = (t) => read(t, `(() => { const m = document.getElementById('welcome-modal'); return m && !m.classList.contains('hidden') ? 1 : 0; })()`);

// 页内记录：PATCH /api/sessions/* 与 GET /api/models 的调用（判「松手才提交」「刷新真的重拉」）
async function hookFetch(t) {
  await read(t, `(() => {
    if (window.__patchHooked) { window.__patches.length = 0; window.__modelsGets = 0; window.__modelsDelay = 0; window.__modelsFail = false; return 1; }
    window.__patchHooked = true;
    window.__patches = []; window.__modelsGets = 0; window.__modelsDelay = 0; window.__modelsFail = false;
    const of = window.fetch;
    window.fetch = function (u, o) {
      const url = String(u);
      const opt = o || {};
      if (url.indexOf('/api/sessions/') >= 0 && opt.method === 'PATCH') window.__patches.push({ url: url, body: opt.body || '' });
      if (url.indexOf('/api/models') >= 0) {
        window.__modelsGets++;
        if (window.__modelsFail) return Promise.reject(new Error('模拟网络故障'));
        const d = window.__modelsDelay;
        if (d > 0) return new Promise((r) => setTimeout(r, d)).then(() => of.apply(window, [u, o]));
      }
      return of.apply(window, arguments);
    };
    return 1;
  })()`);
}
const patches = (t) => read(t, 'window.__patches.slice()');

// 一次拖动：按下 → 中间移动 → 目标位置 → 松手；返回拖动期间/松手后的 PATCH 记录
async function dragSlider(t, fromIdx, toIdx) {
  const g = await trackGeo(t);
  const y = g.y;
  await read(t, 'window.__patches.length = 0');
  await mouse(t, 'mousePressed', lvX(g, fromIdx), y, { button: 'left', clickCount: 1, buttons: 1 });
  await sleep(70);
  await mouse(t, 'mouseMoved', lvX(g, (fromIdx + toIdx) / 2), y, { button: 'left', buttons: 1 });
  await sleep(70);
  await mouse(t, 'mouseMoved', lvX(g, toIdx), y, { button: 'left', buttons: 1 });
  await sleep(90);
  const during = await patches(t);
  const bigDuring = await bigText(t);
  await mouse(t, 'mouseReleased', lvX(g, toIdx), y, { button: 'left', clickCount: 1, buttons: 0 });
  await sleep(420);
  return { during, after: await patches(t), bigDuring };
}

async function main() {
  await up();
  const t = await newTab(BASE + '/', 1280, 900);
  try {
    await sleep(1400);
    await hookFetch(t);
    await dismissWizard(t); // 首启接入向导（隔离实例没配密钥）：关掉，别挡住截图

    // 打开会话（真点侧栏）——档位要落在会话上才有「落库」可验
    await clickEl(t, `[...document.querySelectorAll('.session-item')].find((i) => ((i.querySelector('.s-title') || {}).textContent || '').includes('档位滑杆验证'))`);
    await sleep(900);
    await dismissWizard(t); // 向导也可能是打开会话后才弹出来的，收尾再确认一次
    ok('预备：接入向导已关闭（不挡截图与断言）', (await wizardOpen(t)) === 0, '');

    // ---------- A 芯片合并显示 + 旧二级菜单已消失 ----------
    await hookFetch(t);
    const chip0 = await chipText(t);
    ok('A1 芯片显示「模型名 · 档位」', chip0 === 'GLM-5.3 · Default', chip0);
    const oldUi = await read(t, `(() => ({
      modelPop: !!document.getElementById('model-pop'),
      subPop: !!document.getElementById('model-sub-pop'),
      mpLv: !!document.querySelector('.mp-lv'),
      lvHit: !!document.querySelector('.lv-hit'),
      hasExtra: /Extra/.test(document.body.innerHTML),
    }))()`);
    ok('A2 旧档位二级菜单已删除（#model-pop/#model-sub-pop/.mp-lv/.lv-hit 都不存在）',
      !oldUi.modelPop && !oldUi.subPop && !oldUi.mpLv && !oldUi.lvHit, JSON.stringify(oldUi));
    // 第五档全库统一显示 Xhigh（组件里的用量统计也一并对齐），页面不该再有旧写法残留
    ok('A3 页面文案里不再有旧档位写法（无「Extra」字样）', !oldUi.hasExtra, '');

    // ---------- B 打开卡片（初始 thinking 为空＝跟随服务端默认） ----------
    await clickEl(t, `document.getElementById('model-chip')`);
    let b = await bigBox(t);
    ok('B1 点芯片打开卡片，第一层可见、第二层收起', b.card && b.layer1 && !b.layer2, JSON.stringify(b));
    ok('B2 空档位：大字显示 Default', (await bigText(t)) === 'Default', await bigText(t));
    ok('B3 空档位：滑杆隐藏、开关打开', b.sliderDisplay === 'none' && b.follow, JSON.stringify({ d: b.sliderDisplay, f: b.follow }));
    ok('B4 滑杆 aria：role=slider、valuemin/max=0/5、valuetext',
      (await read(t, `(() => { const s = document.getElementById('mc-slider'); return [s.getAttribute('role'), s.getAttribute('aria-valuemin'), s.getAttribute('aria-valuemax'), s.getAttribute('aria-label')].join('|'); })()`)) === 'slider|0|5|思考档位', '');
    // 视觉：Default 态同样居中、同字号、主题色、无灰底带（与非默认态一套规则）
    const bGeoD = await headerGeo(t);
    ok('B5 Default 态：大字与模型行都水平居中（与卡片中心差 <2px）',
      Math.abs(bGeoD.bigTextCx - bGeoD.cardCx) < 2 && Math.abs(bGeoD.rowCx - bGeoD.cardCx) < 2,
      JSON.stringify({ big: bGeoD.bigTextCx, row: bGeoD.rowCx, center: bGeoD.cardCx }));
    ok('B6 标题区没有灰色背景带（大字与标题行都透明）', bGeoD.noBand,
      JSON.stringify({ bigBg: bGeoD.bigBg, topBg: bGeoD.topBg }));
    ok('B7 档位字号 20–22px、颜色为主题强调蓝（不是纯黑加粗）',
      bGeoD.fontSize >= 20 && bGeoD.fontSize <= 22 && Number(bGeoD.fontWeight) < 700 && bGeoD.rgb[2] > 150 && bGeoD.rgb[2] > bGeoD.rgb[0],
      JSON.stringify({ fontSize: bGeoD.fontSize, weight: bGeoD.fontWeight, color: bGeoD.color }));
    ok('B8 刷新图标仍在右上角',
      bGeoD.refreshRight > bGeoD.cardRight - 30 && bGeoD.refreshRight <= bGeoD.cardRight && bGeoD.refreshTop < bGeoD.cardTop + 20,
      JSON.stringify({ rr: bGeoD.refreshRight, cr: bGeoD.cardRight, rt: bGeoD.refreshTop, ct: bGeoD.cardTop }));

    // ---------- C 关掉「跟随服务端默认」→ 恢复上次手选档位（无历史＝Medium） ----------
    await clickEl(t, `document.getElementById('mc-follow')`);
    await sleep(400);
    b = await bigBox(t);
    let s = await sess();
    ok('C1 关开关：滑杆重新出现', b.sliderDisplay !== 'none', b.sliderDisplay);
    ok('C2 关开关：无历史时回到 Medium（大字+滑杆+芯片）',
      (await bigText(t)) === 'Medium' && b.now === '2' && b.text === 'Medium' && (await chipText(t)) === 'GLM-5.3 · Medium',
      JSON.stringify({ big: await bigText(t), now: b.now, chip: await chipText(t) }));
    ok('C3 关开关：会话 thinking=medium', s.thinking === 'medium', s.thinking);
    // 非默认态与 Default 态必须是同一套视觉（禁止一个居中一个左对齐）
    const bGeoM = await headerGeo(t);
    ok('C4 非默认态（Medium）与 Default 态视觉一致：同样居中、同字号、同色、无灰底',
      Math.abs(bGeoM.bigTextCx - bGeoM.cardCx) < 2 && Math.abs(bGeoM.rowCx - bGeoM.cardCx) < 2
      && bGeoM.fontSize === bGeoD.fontSize && bGeoM.color === bGeoD.color && bGeoM.noBand,
      JSON.stringify({ big: bGeoM.bigTextCx, row: bGeoM.rowCx, center: bGeoM.cardCx, size: bGeoM.fontSize, color: bGeoM.color }));

    // ---------- D 拖到每一档：过程中零 PATCH、松手一次 PATCH、落库正确 ----------
    for (let i = 0; i < 6; i++) {
      const d = await dragSlider(t, i === 0 ? 2 : i - 1, i);
      s = await sess();
      const big = await bigText(t);
      ok(`D${i + 1} 拖到第 ${i + 1} 档 ${LV_LABEL[i]}：拖动中零提交`, d.during.length === 0, JSON.stringify(d.during));
      ok(`D${i + 1}b 松手恰好一次 PATCH，会话 thinking=${LV[i]}`, d.after.length === 1 && s.thinking === LV[i] && big === LV_LABEL[i],
        JSON.stringify({ patches: d.after.length, body: (d.after[0] || {}).body, thinking: s.thinking, big }));
      if (i === 2) await shot(t, join(SCREEN, 'thinkslider-1-layer1-medium.png'));
    }

    // ---------- E 点轨道任意位置＝跳档并立即提交 ----------
    let g = await trackGeo(t);
    await read(t, 'window.__patches.length = 0');
    await mouse(t, 'mousePressed', lvX(g, 0), g.y, { button: 'left', clickCount: 1, buttons: 1 });
    await sleep(60);
    await mouse(t, 'mouseReleased', lvX(g, 0), g.y, { button: 'left', clickCount: 1, buttons: 0 });
    await sleep(420);
    s = await sess();
    ok('E1 点最左端＝None 并立即提交', s.thinking === 'none' && (await bigText(t)) === 'None' && (await patches(t)).length === 1,
      JSON.stringify({ thinking: s.thinking, big: await bigText(t), n: (await patches(t)).length }));

      // ---------- F 键盘左右键各切一档 ----------
    await read(t, `document.getElementById('mc-slider').focus(), 1`); // 聚焦滑杆（不点它，点了会跳档）
    await read(t, 'window.__patches.length = 0');
    await pressKey(t, 'ArrowRight');
    await sleep(400);
    s = await sess();
    ok('F1 右方向键切到 Low 并提交', s.thinking === 'low' && (await bigText(t)) === 'Low', s.thinking);
    await read(t, 'window.__patches.length = 0');
    await pressKey(t, 'ArrowLeft');
    await sleep(400);
    s = await sess();
    ok('F2 左方向键退回 None 并提交', s.thinking === 'none' && (await bigText(t)) === 'None' && (await patches(t)).length === 1, s.thinking);
    // 再往右切到 xhigh 验显示文案 Xhigh（内部值仍是 xhigh）
    for (let k = 0; k < 4; k++) { await pressKey(t, 'ArrowRight'); await sleep(320); }
    s = await sess();
    ok('F3 键盘切到第五档：显示 Xhigh、内部值 xhigh',
      (await bigText(t)) === 'Xhigh' && s.thinking === 'xhigh' && (await chipText(t)) === 'GLM-5.3 · Xhigh',
      JSON.stringify({ big: await bigText(t), thinking: s.thinking, chip: await chipText(t) }));

    // ---------- G 跟随服务端默认：thinking 空串、滑杆隐藏、Default ----------
    await clickEl(t, `document.getElementById('mc-follow')`);
    await sleep(420);
    b = await bigBox(t);
    s = await sess();
    ok('G1 开开关：会话 thinking 为空串（走服务端默认；空值在会话 JSON 里省略）',
      (s.thinking || '') === '', JSON.stringify({ thinking: s.thinking }));
    ok('G2 开开关：大字 Default、滑杆隐藏、芯片 Default',
      (await bigText(t)) === 'Default' && b.sliderDisplay === 'none' && (await chipText(t)) === 'GLM-5.3 · Default',
      JSON.stringify({ big: await bigText(t), d: b.sliderDisplay, chip: await chipText(t) }));
    ok('G3 开开关：滑杆 aria-disabled=true', b.now !== null && (await read(t, `document.getElementById('mc-slider').getAttribute('aria-disabled')`)) === 'true', '');
    await shot(t, join(SCREEN, 'thinkslider-2-follow-default.png'));

    // ---------- H 再关开关：恢复上一次手选档位（xhigh），不是 Medium ----------
    await clickEl(t, `document.getElementById('mc-follow')`);
    await sleep(420);
    s = await sess();
    ok('H1 关开关：恢复上一次手选档位 Xhigh（不是 Medium 兜底）',
      s.thinking === 'xhigh' && (await bigText(t)) === 'Xhigh', JSON.stringify({ thinking: s.thinking, big: await bigText(t) }));

    // ---------- I 第二层模型列表 ----------
    await clickEl(t, `document.getElementById('mc-model')`);
    await sleep(350);
    const l2 = await read(t, `(() => {
      const card = document.getElementById('model-card');
      const list = document.getElementById('mc-list');
      const items = [...list.querySelectorAll('.mc-item')];
      const cur = items.find((x) => x.classList.contains('sel'));
      const groupEls = [...list.querySelectorAll('.pop-group')];
      const kids = [...list.children];
      // 结构：每个分组标题后面紧跟它自己的模型行（分组标题 → 若干 .mc-item → 下一个标题）
      const struct = kids.filter((k) => k.classList.contains('mc-item') || k.classList.contains('pop-group'))
        .map((k) => (k.classList.contains('pop-group') ? 'G:' + k.textContent.trim() : 'M:' + k.textContent.trim()));
      return {
        layer1: !document.getElementById('mc-lv').classList.contains('hidden'),
        layer2: !document.getElementById('mc-models').classList.contains('hidden'),
        title: (card.querySelector('.mc-models-head b') || {}).textContent || '',
        labels: items.map((x) => x.textContent.trim()),
        groups: groupEls.map((g) => g.textContent.trim()),
        struct: struct,
        groupClickable: groupEls.some((g) => g.tagName === 'BUTTON' || !!g.onclick || !!g.closest('button')),
        groupHasCheck: groupEls.some((g) => !!g.querySelector('.mc-check') || !!g.querySelector('svg')),
        groupFontSize: groupEls.length ? parseFloat(getComputedStyle(groupEls[0]).fontSize) : 0,
        groupColor: groupEls.length ? getComputedStyle(groupEls[0]).color : '',
        groupPad: groupEls.length ? parseFloat(getComputedStyle(groupEls[0]).paddingLeft) : 0,
        itemPad: items.length ? parseFloat(getComputedStyle(items[0]).paddingLeft) : 0,
        scrollable: getComputedStyle(list).overflowY,
        curText: cur ? cur.textContent.trim() : '',
        curCheck: cur ? !!cur.querySelector('.mc-check') : false,
        hasDefault: items.some((x) => /^默认/.test(x.textContent.trim())),
      };
    })()`);
    ok('I1 点模型名翻到第二层（第一层收起）', !l2.layer1 && l2.layer2, JSON.stringify({ l1: l2.layer1, l2: l2.layer2 }));
    ok('I2 标题为「选择模型」', l2.title === '选择模型', l2.title);
    // 分组顺序＝/api/models 的返回顺序（与「设置 · 模型」的供应商顺序一致）
    const modelsApi = await (await fetch(BASE + '/api/models')).json();
    const provs = [];
    for (const m of modelsApi.models || []) if (!provs.includes(m.providerName)) provs.push(m.providerName);
    ok('I3 按供应商分组：分组标题数量与顺序跟接口一致', l2.groups.length === provs.length && l2.groups.join('|') === provs.join('|'),
      JSON.stringify({ groups: l2.groups, apiOrder: provs }));
    ok('I4 分组标题是小写灰字、不可点、无勾选', !l2.groupClickable && !l2.groupHasCheck && l2.groupFontSize < 12 && /rgb/.test(l2.groupColor),
      JSON.stringify({ clickable: l2.groupClickable, check: l2.groupHasCheck, fontSize: l2.groupFontSize, color: l2.groupColor }));
    ok('I5 模型缩进排在各自分组标题下（结构：G:供应商 → M:模型…）',
      l2.struct.filter((x) => x.startsWith('G:')).length === provs.length
      && l2.struct[0].startsWith('G:') && l2.struct.some((x) => x.startsWith('M:'))
      && l2.itemPad > l2.groupPad,
      JSON.stringify({ struct: l2.struct, itemPad: l2.itemPad, groupPad: l2.groupPad }));
    ok('I6 不放「默认模型」条目', !l2.hasDefault, JSON.stringify(l2.labels));
    ok('I7 当前模型（GLM-5.3）带勾选且只有一个',
      l2.curText === 'GLM-5.3' && l2.curCheck && (await read(t, `document.querySelectorAll('#mc-list .mc-item.sel').length`)) === 1,
      JSON.stringify({ cur: l2.curText, check: l2.curCheck }));
    ok('I8 已分组：同名模型不再带渠道后缀',
      l2.labels.filter((x) => x === 'deepseek-flash').length === 2
      && !l2.labels.some((x) => x.includes('（') || x.includes('(SnowAI)'))
      && l2.labels.includes('6.1 Terra') && l2.labels.includes('GLM-5.3'),
      JSON.stringify(l2.labels));
    ok('I9 列表可滚动', l2.scrollable === 'auto' || l2.scrollable === 'scroll', l2.scrollable);
    await shot(t, join(SCREEN, 'thinkslider-3-models.png'));

    // ---------- J 点某一项切模型：档位不变、回第一层 ----------
    await read(t, 'window.__patches.length = 0');
    await clickEl(t, `[...document.querySelectorAll('#model-card .mc-item')].find((x) => x.textContent.trim() === '6.1 Terra')`);
    await sleep(500);
    b = await bigBox(t);
    s = await sess();
    ok('J1 换模型后回到第一层', b.layer1 && !b.layer2, JSON.stringify({ l1: b.layer1, l2: b.layer2 }));
    ok('J2 会话模型已切到 6.1 Terra', s.model === 'gpt-6.1-terra', s.model);
    ok('J3 换模型不重置档位（仍 Xhigh）',
      s.thinking === 'xhigh' && (await bigText(t)) === 'Xhigh' && (await chipText(t)) === '6.1 Terra · Xhigh',
      JSON.stringify({ thinking: s.thinking, big: await bigText(t), chip: await chipText(t) }));

    // ---------- K 刷新图标：重新拉模型列表（loading 态 + 失败可见） ----------
    await read(t, `(window.__modelsGets = 0, window.__modelsDelay = 700, 1)`);
    await clickEl(t, `document.getElementById('mc-refresh')`);
    await sleep(120);
    const spin = await read(t, `(() => ({ spin: document.getElementById('mc-refresh').classList.contains('spin'), disabled: document.getElementById('mc-refresh').disabled }))()`);
    ok('K1 刷新期间有 loading 态（图标转圈 + 禁用）', spin.spin && spin.disabled, JSON.stringify(spin));
    await sleep(1100);
    ok('K2 刷新真的重新 GET 了一次 /api/models', (await read(t, 'window.__modelsGets')) === 1, String(await read(t, 'window.__modelsGets')));
    const afterSpin = await read(t, `document.getElementById('mc-refresh').classList.contains('spin')`);
    ok('K3 刷新结束回到常态（不再转圈）', !afterSpin, '');
    // 失败可见：让 /api/models 直接 reject
    await read(t, `(window.__modelsDelay = 0, window.__modelsFail = true, 1)`);
    await clickEl(t, `document.getElementById('mc-refresh')`);
    await sleep(500);
    const errShown = await read(t, `(() => { const e = document.getElementById('mc-err'); return { hidden: e.classList.contains('hidden'), text: e.textContent }; })()`);
    ok('K4 刷新失败给出可见提示（不静默失败）', !errShown.hidden && errShown.text.includes('刷新模型列表失败'), JSON.stringify(errShown));
    await shot(t, join(SCREEN, 'thinkslider-4-refresh-error.png'));
    await read(t, `(window.__modelsFail = false, 1)`);
    await clickEl(t, `document.getElementById('mc-refresh')`);
    await sleep(600);
    ok('K5 恢复后再刷新：错误条自动收起', (await read(t, `document.getElementById('mc-err').classList.contains('hidden')`)) === true, '');

    // ---------- L 触摸拖动（pointer 事件，触屏同样可用） ----------
    g = await trackGeo(t);
    await read(t, 'window.__patches.length = 0');
    await t.send('Input.dispatchTouchEvent', { type: 'touchStart', touchPoints: [{ x: Math.round(lvX(g, 4)), y: Math.round(g.y) }] });
    await sleep(80);
    await t.send('Input.dispatchTouchEvent', { type: 'touchMove', touchPoints: [{ x: Math.round(lvX(g, 1)), y: Math.round(g.y) }] });
    await sleep(100);
    const touchDuring = await patches(t);
    await t.send('Input.dispatchTouchEvent', { type: 'touchEnd', touchPoints: [] });
    await sleep(450);
    s = await sess();
    ok('L1 触摸拖动：拖动中零提交', touchDuring.length === 0, JSON.stringify(touchDuring));
    ok('L2 触摸拖动松手提交到 Low', s.thinking === 'low' && (await bigText(t)) === 'Low', JSON.stringify({ thinking: s.thinking, big: await bigText(t) }));

    // ---------- M 深色主题 / 460px 窄屏 ----------
    await read(t, `(document.documentElement.setAttribute('data-theme', 'dark'), 1)`);
    await sleep(150);
    const dark = await read(t, `(() => {
      const c = document.getElementById('model-card').getBoundingClientRect();
      const tr = document.getElementById('mc-track').getBoundingClientRect();
      return { open: !document.getElementById('model-card').classList.contains('hidden'),
        l: c.left, r: c.right, t: c.top, b: c.bottom, vw: window.innerWidth, vh: window.innerHeight,
        trackR: tr.right, cardR: c.right, bg: getComputedStyle(document.getElementById('model-card')).backgroundColor };
    })()`);
    ok('M1 深色主题下卡片正常显示且在视口内', dark.open && dark.l >= 0 && dark.r <= dark.vw + 1, JSON.stringify(dark));
    ok('M2 深色主题卡片底色为深色（非白）', dark.bg !== 'rgb(255, 255, 255)', dark.bg);
    await shot(t, join(SCREEN, 'thinkslider-5-dark.png'));
    // 窄屏 460px
    await t.send('Emulation.setDeviceMetricsOverride', { width: 460, height: 800, deviceScaleFactor: 1, mobile: false });
    await sleep(900); // 等窄屏断点切换与布局稳定（输入区/胶囊可能重排）
    await read(t, `(typeof closePopovers === 'function' ? (closePopovers(), 1) : (document.getElementById('model-card').classList.add('hidden'), 1))`);
    await sleep(150);
    // 真实点芯片打开卡片；窄屏下第一次点击偶尔被上一段异步收尾吃掉，补点一次
    const chipN = await clickEl(t, `document.getElementById('model-chip')`);
    await sleep(350);
    let openN = await read(t, `!document.getElementById('model-card').classList.contains('hidden')`);
    if (!openN) {
      await clickEl(t, `document.getElementById('model-chip')`);
      await sleep(350);
      openN = await read(t, `!document.getElementById('model-card').classList.contains('hidden')`);
    }
    const narrow = await read(t, `(() => {
      const c = document.getElementById('model-card').getBoundingClientRect();
      const sl = document.getElementById('mc-slider').getBoundingClientRect();
      const tr = document.getElementById('mc-track').getBoundingClientRect();
      const ch = document.getElementById('model-chip').getBoundingClientRect();
      return { open: !document.getElementById('model-card').classList.contains('hidden'),
        l: c.left, r: c.right, t: c.top, b: c.bottom, vw: window.innerWidth, vh: window.innerHeight,
        sliderR: sl.right, trackR: tr.right, cardR: c.right,
        chip: { x: ch.left + ch.width / 2, y: ch.top + ch.height / 2, w: ch.width, h: ch.height },
        overflowX: document.documentElement.scrollWidth - window.innerWidth };
    })()`);
    ok('M3 460px 窄屏：点芯片能打开卡片', openN && narrow.open, JSON.stringify({ chipN, chip: narrow.chip, open: narrow.open }));
    ok('M4 460px 窄屏：卡片完整在视口内',
      narrow.l >= 0 && narrow.r <= narrow.vw + 1 && narrow.t >= 0 && narrow.b <= narrow.vh + 1,
      JSON.stringify({ l: narrow.l, r: narrow.r, vw: narrow.vw, t: narrow.t, b: narrow.b, vh: narrow.vh }));
    ok('M5 460px 窄屏：滑杆不横向溢出（轨道右缘不超出卡片）', narrow.sliderR <= narrow.cardR + 0.5 && narrow.trackR <= narrow.cardR + 0.5,
      JSON.stringify({ sliderR: narrow.sliderR, trackR: narrow.trackR, cardR: narrow.cardR }));
    ok('M6 460px 窄屏：页面无横向滚动', narrow.overflowX <= 0, String(narrow.overflowX));
    await shot(t, join(SCREEN, 'thinkslider-6-narrow460.png'));
    await t.send('Emulation.setDeviceMetricsOverride', { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false });
    await sleep(400);
    await read(t, `(document.documentElement.setAttribute('data-theme', 'light'), 1)`);

    // ---------- N Esc 与点外部关闭 ----------
    await sleep(200);
    if ((await bigBox(t)).card) { await pressKey(t, 'Escape'); await sleep(300); } // 收掉上一段遗留
    await clickEl(t, `document.getElementById('model-chip')`);
    ok('N1 重新打开卡片（Esc 用例前置）', (await bigBox(t)).card, '');
    await pressKey(t, 'Escape');
    await sleep(320);
    ok('N2 Esc 关闭卡片', !(await bigBox(t)).card, '');
    await clickEl(t, `document.getElementById('model-chip')`);
    ok('N3 再打开卡片（点外部用例前置）', (await bigBox(t)).card, '');
    const spot = await read(t, `(() => { const r = document.getElementById('messages').getBoundingClientRect(); return { x: r.right - 6, y: r.top + 16 }; })()`);
    await click(t, spot.x, spot.y);
    await sleep(320);
    ok('N4 点卡片外部关闭', !(await bigBox(t)).card, '');
    await shot(t, join(SCREEN, 'thinkslider-7-closed.png'));

    // ---------- O 右栏子对话输入区那枚芯片：共用同一张卡，锚在它上面 ----------
    const subRes = await fetch(BASE + '/api/sessions', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ parent: sessionId, title: '子对话', inheritContext: true }),
    });
    const subSess = await subRes.json();
    if (!subSess.id) throw new Error('建子对话失败：' + JSON.stringify(subSess).slice(0, 200));
    await read(t, `(rpOpen('subs'), 1)`); // 打开右栏「子对话」视图（与选中文字→子对话同一条路径）
    await sleep(500);
    await read(t, `(openSubSession(${JSON.stringify(subSess.id)}), 1)`);
    await sleep(1400);
    // 真实点右栏那枚芯片；面板动画/首点被吃时补点一次
    let subChipBox = await clickEl(t, `document.getElementById('sub-model-chip')`);
    await sleep(350);
    let subOpen = await read(t, `!document.getElementById('model-card').classList.contains('hidden')`);
    let hitEl = await read(t, `(() => { const e = document.elementFromPoint(${Math.round(subChipBox.x)}, ${Math.round(subChipBox.y)}); return e ? (e.id || e.className || e.tagName) : ''; })()`);
    if (!subOpen) {
      await clickEl(t, `document.getElementById('sub-model-chip')`);
      await sleep(350);
      subOpen = await read(t, `!document.getElementById('model-card').classList.contains('hidden')`);
    }
    const subView = await read(t, `(() => {
      const c = document.getElementById('model-card').getBoundingClientRect();
      const ch = document.getElementById('sub-model-chip').getBoundingClientRect();
      return { open: !document.getElementById('model-card').classList.contains('hidden'),
        l1: !document.getElementById('mc-lv').classList.contains('hidden'),
        l: c.left, r: c.right, t: c.top, b: c.bottom, vw: window.innerWidth, vh: window.innerHeight,
        chipTop: ch.top, big: document.getElementById('mc-big').textContent,
        subChip: document.getElementById('sub-model-chip-label').textContent };
    })()`);
    ok('O1 子对话芯片点开同一张卡（默认第一层）', subView.open && subView.l1, JSON.stringify({ open: subView.open, l1: subView.l1, hit: hitEl, chip: subChipBox }));
    ok('O2 子对话芯片也显示「模型名 · 档位」', /\s·\s/.test(subView.subChip), subView.subChip);
    ok('O3 卡片贴子对话芯片、仍在视口内',
      subView.l >= 0 && subView.r <= subView.vw + 1 && subView.t >= 0 && subView.b <= subView.vh + 1,
      JSON.stringify({ l: subView.l, r: subView.r, t: subView.t, b: subView.b, vw: subView.vw, vh: subView.vh }));
    await shot(t, join(SCREEN, 'thinkslider-8-subview.png'));
    await pressKey(t, 'Escape');
    await sleep(300);
    ok('O4 Esc 关闭子对话卡片', !(await bigBox(t)).card, '');

    // ---------- P 长列表（≥20 个模型）与 460×600 窄屏：卡片/列表都不越出视口 ----------
    // 造一个大供应商（真接口 POST /api/providers），再用卡片右上角刷新把模型拉进列表
    const bulkModels = Array.from({ length: 22 }, (_, i) => ({ id: 'bulk-' + (i + 1), name: '5.5 系列模型 ' + (i + 1) }));
    const addRes = await fetch(BASE + '/api/providers', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ id: 'mock-bulk', name: '大模型池', baseURL: 'mock', apiKey: '', models: bulkModels }),
    });
    ok('P0 预备：加一个 22 个模型的供应商', addRes.ok, 'HTTP ' + addRes.status);
    // 真点：开卡片 → 刷新（拉 /api/models）→ 进第二层
    await clickEl(t, `document.getElementById('model-chip')`);
    await clickEl(t, `document.getElementById('mc-refresh')`);
    await sleep(800);
    await clickEl(t, `document.getElementById('mc-model')`);
    await sleep(400);
    // 语义化的「最后一项不越界」：把列表滚到底，量最后一项的底边
    const listGeo = () => read(t, `(() => {
      const vh = window.innerHeight;
      const card = document.getElementById('model-card');
      const list = document.getElementById('mc-list');
      list.scrollTop = list.scrollHeight; // 滚到底：量「最下面那一项」的真实位置
      const items = [...list.querySelectorAll('.mc-item')];
      const last = items[items.length - 1];
      const c = card.getBoundingClientRect();
      const lb = last ? last.getBoundingClientRect() : { top: 0, bottom: 0 };
      const lr = list.getBoundingClientRect();
      const cs = getComputedStyle(card);
      return { vh, count: items.length, cardTop: c.top, cardBottom: c.bottom, cardH: c.height,
        lastTop: lb.top, lastBottom: lb.bottom, listBottom: lr.bottom,
        overflowY: cs.overflowY, overflow: cs.overflow, clipped: cs.overflow === 'hidden' || cs.overflowY === 'hidden',
        listScroll: list.scrollHeight > list.clientHeight + 1, listClient: list.clientHeight, listScrollH: list.scrollHeight,
        listPadBottom: parseFloat(getComputedStyle(list).paddingBottom) };
    })()`);
    const big = await listGeo();
    ok('P1 模型 ≥20 项已进列表', big.count >= 22, 'items=' + big.count);
    ok('P2 卡片整体在视口内（底边留 ≥8px、顶边不越界）',
      big.cardBottom <= big.vh - 8 + 0.5 && big.cardTop >= 7.5,
      JSON.stringify({ top: big.cardTop, bottom: big.cardBottom, vh: big.vh }));
    ok('P3 滚到底时最后一项底边 ≤ 视口高 − 8（贴底不被裁）',
      big.lastBottom <= big.vh - 8 + 0.5 && big.lastBottom > 0,
      JSON.stringify({ lastBottom: big.lastBottom, vh: big.vh }));
    ok('P4 卡片 overflow:hidden（内容不会画到圆角外）', big.clipped, JSON.stringify({ overflow: big.overflow, overflowY: big.overflowY }));
    ok('P5 列表内容超高时仍内部滚动（scrollHeight > clientHeight）',
      big.listScroll && big.listScrollH > big.listClient,
      JSON.stringify({ sh: big.listScrollH, ch: big.listClient }));
    ok('P6 列表底部留 12px 内边距', big.listPadBottom >= 12, String(big.listPadBottom));
    await shot(t, join(SCREEN, 'thinkslider-9-longlist.png'));
    // 切模型后回第一层要复位（高度收回去、底边仍在视口内）
    await clickEl(t, `document.querySelector('#mc-list .mc-item.sel')`);
    await sleep(500);
    const backGeo = await read(t, `(() => {
      const c = document.getElementById('model-card').getBoundingClientRect();
      return { vh: window.innerHeight, top: c.top, bottom: c.bottom, h: c.height,
        layer1: !document.getElementById('mc-lv').classList.contains('hidden') };
    })()`);
    ok('P7 回第一层复位：层切回、卡片收短且底边仍在视口内',
      backGeo.layer1 && backGeo.h < big.cardH && backGeo.bottom <= backGeo.vh - 8 + 0.5,
      JSON.stringify(backGeo));

    // 460×600 窄视口（旋屏/小窗）：卡片开着不动，只改视口 —— 验 resize 时动态重算
    await t.send('Emulation.setDeviceMetricsOverride', { width: 460, height: 600, deviceScaleFactor: 1, mobile: false });
    await sleep(900);
    if ((await bigBox(t)).layer1) await clickEl(t, `document.getElementById('mc-model')`);
    await sleep(450);
    const narrow2 = await listGeo();
    ok('P8 460×600 窄屏：卡片开着、改视口后仍停在列表层', narrow2.count >= 22 && narrow2.vh === 600, JSON.stringify({ n: narrow2.count, vh: narrow2.vh }));
    ok('P9 460×600 窄屏（resize 动态重算）：卡片底边 ≤ 视口高 − 8、顶边不越界',
      narrow2.cardBottom <= narrow2.vh - 8 + 0.5 && narrow2.cardTop >= 7.5,
      JSON.stringify({ top: narrow2.cardTop, bottom: narrow2.cardBottom, vh: narrow2.vh }));
    ok('P10 460×600 窄屏：滚到底时最后一项底边 ≤ 视口高 − 8',
      narrow2.lastBottom <= narrow2.vh - 8 + 0.5 && narrow2.lastBottom > 0,
      JSON.stringify({ lastBottom: narrow2.lastBottom, vh: narrow2.vh }));
    ok('P11 460×600 窄屏：列表内部滚动 + 卡片 overflow:hidden',
      narrow2.listScroll && narrow2.clipped, JSON.stringify({ scroll: narrow2.listScroll, clipped: narrow2.clipped }));
    await shot(t, join(SCREEN, 'thinkslider-10-narrow600-longlist.png'));
    // 窄屏下重开一次（关闭→再点芯片），确认从零定位也不越界
    await pressKey(t, 'Escape');
    await sleep(250);
    await clickEl(t, `document.getElementById('model-chip')`);
    await sleep(250);
    if ((await bigBox(t)).layer1) await clickEl(t, `document.getElementById('mc-model')`);
    await sleep(400);
    const narrow3 = await listGeo();
    ok('P12 460×600 窄屏重开：卡片与最后一项都不越界',
      narrow3.cardBottom <= narrow3.vh - 8 + 0.5 && narrow3.cardTop >= 7.5 && narrow3.lastBottom <= narrow3.vh - 8 + 0.5,
      JSON.stringify({ top: narrow3.cardTop, bottom: narrow3.cardBottom, last: narrow3.lastBottom, vh: narrow3.vh }));
    // 复原视口与配置，避免影响后续
    await t.send('Emulation.setDeviceMetricsOverride', { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false });
    await sleep(500);
    await pressKey(t, 'Escape');
    await sleep(250);
    await fetch(BASE + '/api/providers/delete', { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ id: 'mock-bulk' }) });
    ok('P13 收尾：大供应商已删除、卡片已关闭', !(await bigBox(t)).card, '');
  } finally {
    await closeTab(t);
  }

  const fail = results.filter((r) => !r.pass).length;
  console.log(fail ? `\n${fail} 项失败` : `\n全部通过（${results.length} 项）`);
  down();
  process.exit(fail ? 1 : 0);
}

main().catch((e) => { console.error('ERROR', e); down(); process.exit(2); });
