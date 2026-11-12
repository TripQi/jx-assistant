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
//   I 点模型名翻到第二层：平铺（无分组头/无「默认模型」条目）、当前模型带勾、重名带渠道后缀
//   J 点某一项切模型：档位保持不变，回到第一层
//   K 刷新图标：重新 GET /api/models（loading 转圈），失败给可见提示
//   L 触摸拖动（pointer 事件）同样可用
//   M 深色主题 / 460px 窄屏：卡片不越界、滑杆不横向溢出
//   N Esc 与点外部关闭
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
      providers: [{
        id: 'mock', name: 'Mock 服务', baseURL: 'mock', apiKey: '',
        // 多 Key 结构：同渠道内两个 Key 拉到同名模型 → 列表里应带渠道后缀区分
        keys: [
          { id: 'k1', label: '默认分组', models: [{ id: 'glm-5.3', name: 'GLM-5.3' }, { id: 'deepseek-flash', name: 'deepseek-flash' }] },
          { id: 'k2', label: 'SnowAI', models: [{ id: 'gpt-6.1-terra', name: '6.1 Terra' }, { id: 'deepseek-flash', name: 'deepseek-flash' }] },
        ],
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
      thinkItems: document.body.innerHTML.includes('Extra High'),
    }))()`);
    ok('A2 旧档位二级菜单已删除（#model-pop/#model-sub-pop/.mp-lv/.lv-hit 都不存在）',
      !oldUi.modelPop && !oldUi.subPop && !oldUi.mpLv && !oldUi.lvHit, JSON.stringify(oldUi));
    ok('A3 界面文案里不再出现「Extra High」', !oldUi.thinkItems, '');

    // ---------- B 打开卡片（初始 thinking 为空＝跟随服务端默认） ----------
    await clickEl(t, `document.getElementById('model-chip')`);
    let b = await bigBox(t);
    ok('B1 点芯片打开卡片，第一层可见、第二层收起', b.card && b.layer1 && !b.layer2, JSON.stringify(b));
    ok('B2 空档位：大字显示 Default', (await bigText(t)) === 'Default', await bigText(t));
    ok('B3 空档位：滑杆隐藏、开关打开', b.sliderDisplay === 'none' && b.follow, JSON.stringify({ d: b.sliderDisplay, f: b.follow }));
    ok('B4 滑杆 aria：role=slider、valuemin/max=0/5、valuetext',
      (await read(t, `(() => { const s = document.getElementById('mc-slider'); return [s.getAttribute('role'), s.getAttribute('aria-valuemin'), s.getAttribute('aria-valuemax'), s.getAttribute('aria-label')].join('|'); })()`)) === 'slider|0|5|思考档位', '');

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
      const items = [...card.querySelectorAll('.mc-item')];
      const cur = items.find((x) => x.classList.contains('sel'));
      return {
        layer1: !document.getElementById('mc-lv').classList.contains('hidden'),
        layer2: !document.getElementById('mc-models').classList.contains('hidden'),
        title: (card.querySelector('.mc-models-head b') || {}).textContent || '',
        labels: items.map((x) => x.textContent.trim()),
        groups: card.querySelectorAll('#mc-models .pop-group').length,
        curText: cur ? cur.textContent.trim() : '',
        curCheck: cur ? !!cur.querySelector('.mc-check') : false,
        hasDefault: items.some((x) => /^默认/.test(x.textContent.trim())),
      };
    })()`);
    ok('I1 点模型名翻到第二层（第一层收起）', !l2.layer1 && l2.layer2, JSON.stringify({ l1: l2.layer1, l2: l2.layer2 }));
    ok('I2 标题为「选择模型」', l2.title === '选择模型', l2.title);
    ok('I3 列表平铺：无渠道分组头', l2.groups === 0, String(l2.groups));
    ok('I4 不放「默认模型」条目', !l2.hasDefault, JSON.stringify(l2.labels));
    ok('I5 当前模型（GLM-5.3）带勾选标记', l2.curText === 'GLM-5.3' && l2.curCheck, JSON.stringify({ cur: l2.curText, check: l2.curCheck }));
    ok('I6 重名模型带渠道后缀、不重名的保持干净',
      l2.labels.includes('deepseek-flash（SnowAI）') && l2.labels.includes('deepseek-flash（默认分组）')
      && l2.labels.includes('6.1 Terra') && l2.labels.includes('GLM-5.3'),
      JSON.stringify(l2.labels));
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
  } finally {
    await closeTab(t);
  }

  const fail = results.filter((r) => !r.pass).length;
  console.log(fail ? `\n${fail} 项失败` : `\n全部通过（${results.length} 项）`);
  down();
  process.exit(fail ? 1 : 0);
}

main().catch((e) => { console.error('ERROR', e); down(); process.exit(2); });
