// 模型菜单思考档位二级浮窗验证（web/app-chat.js renderModelPop + syncModelSubPop）：
// 自包含——需要时自动构建隔离实例（mock 模型、独立 data/工作区，落 .jxai/tmp）
// 并拉起无头 Edge（CDP 9223，沿用 cdp.mjs 约定）。断言与截图落
// gui-test-screenshots/modelsub-*。交互模型（2026-10 改版）：
//   档位全系统固定六档（None/Low/Medium/High/Extra High/Max），任何模型行都能展开；
//   桌面悬停整行 = 右侧浮出这套六档；
//   点行任意处 = 选中该模型并整体收起（不再顺带弹二级菜单——旧版点击会 toggle，
//   连点两下互相抵消出现「关不掉」）；点行尾箭头(.lv-hit) = 手动开合档位浮窗；
//   在未选中的模型行上点档位 = 一并选中该模型再定档。
// 用例：
//   A 打开模型菜单：面板可见、无内嵌档位残留、二级浮窗默认收起
//   B 悬停模型行：二级浮窗显示、贴行右侧、行标 exp、内容=默认+固定六档；
//     悬停另一行 → 浮窗跟着切到那一行
//   C 点档位「Low」：双面板都收起、芯片文案 = 模型名 + Low
//   D 重开菜单：不幽灵弹出上次的浮窗；点行主体 = 选中并整体收起（切到 DeepSeek）
//   E 点箭头展开；移开指针 250ms 自动收起；在未选中行(GLM)点「High」→ 一并选中并收起
//   F 点另一行箭头浮窗切档；桌面已展开行再点箭头保持展开；点行主体=选中并收起
//     （触屏 hover:none 分支无法在无头桌面模拟：本机 Edge setEmulatedMedia 不支持
//      hover/pointer 特性，点行收起与箭头展开路径已由 D/E/F 覆盖，真机另验收）
//   G 窄视口（420px）：浮窗仍完整落在视口内（右侧放不下翻到菜单左缘外）
// 运行：仓库根目录 node gui-test/model-subpop-verify.mjs
import { newTab, read, click, shot, sleep, closeTab } from './cdp.mjs';
import { mkdirSync, writeFileSync, rmSync, accessSync } from 'node:fs';
import { spawn, execSync } from 'node:child_process';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const SCREEN = join(ROOT, 'gui-test-screenshots');
const BASE = 'http://127.0.0.1:8831';
const CDP_HTTP = 'http://127.0.0.1:9223';
const PORT = 8831;
const TMP = join(ROOT, '.jxai', 'tmp', 'jx-modelsub-test');
mkdirSync(SCREEN, { recursive: true });
const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

// ---------- 环境自举：隔离实例 + 无头 Edge（同 ctxmenu-verify 模式） ----------
let serverProc = null, edgeWasOurs = false, edgePid = 0;
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
      wizardDone: true, // 首启接入向导不弹（弹了会挡住输入区，点不到模型芯片）
      providers: [{
        id: 'mock', name: 'Mock 服务', baseURL: 'mock', apiKey: '',
        models: [
          { id: 'glm-5.3', name: 'GLM-5.3' },
          { id: 'plain-model', name: '另一个模型' },
          { id: 'deepseek-v4-pro', name: 'DeepSeek-V4-Pro' },
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
async function clickEl(c, expr) {
  await read(c, `(() => { const el = ${expr}; if (el) el.scrollIntoView({ block: 'center' }); return 1; })()`);
  await sleep(150);
  const b = await read(c, `(() => { const el = ${expr}; if (!el) return null; const r = el.getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
  if (!b) throw new Error('not found: ' + expr);
  await click(c, b.x, b.y);
  return b;
}
// 真实移动鼠标（两步：先跳远点再移到目标，保证产生 hover 进入事件）
async function moveTo(c, x, y) {
  await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: 8, y: 8 });
  await sleep(60);
  await c.send('Input.dispatchMouseEvent', { type: 'mouseMoved', x: Math.round(x), y: Math.round(y) });
  await sleep(200);
}
// 打开模型菜单：点芯片；首点偶尔被上一段的异步收尾吃掉（无头 CDP 下的时序），补点一次
async function openModelMenu(c) {
  await clickEl(c, `document.getElementById('model-chip')`);
  await sleep(350);
  if (!(await state(c)).mainOpen) {
    await clickEl(c, `document.getElementById('model-chip')`);
    await sleep(350);
  }
  return state(c);
}

const state = (c) => read(c, `(() => ({
  mainOpen: !document.getElementById('model-pop').classList.contains('hidden'),
  subOpen: !document.getElementById('model-sub-pop').classList.contains('hidden'),
  inlineLv: !!document.querySelector('#model-pop .mp-levels, #model-pop .mp-lv'),
}))()`);
const flyout = (c, key) => read(c, `(() => {
  const sub = document.getElementById('model-sub-pop');
  const pop = document.getElementById('model-pop');
  const row = document.querySelector('#model-pop .model-item[data-key="${key}"]');
  const sr = sub.getBoundingClientRect(), rr = row.getBoundingClientRect(), pr = pop.getBoundingClientRect();
  return { open: !sub.classList.contains('hidden'), rowExp: row.classList.contains('exp'),
    items: [...sub.querySelectorAll('.mp-lv')].map((x) => x.textContent.trim()),
    onItem: (sub.querySelector('.mp-lv.on') || {}).textContent || '',
    gap: sr.left - rr.right, subR: sr.right, subTop: sr.top, rowTop: rr.top, menuL: pr.left, menuR: pr.right,
    overlapY: sr.top <= rr.bottom && sr.bottom >= rr.top,
    coversMenu: sr.left < pr.right - 2 && sr.right > pr.left + 2 };
})()`);

async function main() {
  await up();
  const models = (await (await fetch(BASE + '/api/models')).json()).models || [];
  // 档位与模型解绑：随便两个模型都该展开同一套六档
  const target = models[0];
  const plain = models[1];
  if (!target || !plain) throw new Error('mock 模型清单不足两个模型');
  const key = target.providerId + '|' + target.modelId;
  const plainKey = plain.providerId + '|' + plain.modelId;
  const rowExpr = `document.querySelector('#model-pop .model-item[data-key="${key}"]')`;
  const LV_LABEL = { none: 'None', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Extra High', max: 'Max' };
  const WANT_LV = ['None', 'Low', 'Medium', 'High', 'Extra High', 'Max'];

  const t = await newTab(BASE + '/', 1280, 900);
  try {
    await sleep(1200);

    // ---------- A 打开菜单 ----------
    await clickEl(t, `document.getElementById('model-chip')`);
    await sleep(350);
    const a = await state(t);
    ok('A1 模型面板打开', a.mainOpen, '');
    ok('A2 主列表里没有内嵌档位（.mp-levels/.mp-lv）', !a.inlineLv, '');
    ok('A3 打开时二级浮窗默认收起', !a.subOpen, '');

    // ---------- B 悬停整行 → 浮窗弹出；悬停无档位行 → 收起 ----------
    const rb = await read(t, `(() => { const r = document.querySelector('#model-pop .model-item[data-key="${key}"]').getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
    await moveTo(t, rb.x, rb.y);
    const b = await flyout(t, key);
    ok('B1 悬停整行二级浮窗弹出', b.open, '');
    ok('B2 浮窗贴行右侧或菜单左缘外侧（不压菜单）',
      (b.gap >= 0 && b.gap <= 6) || (b.gap < 0 && b.subR <= b.menuL + 6),
      `gap=${b.gap.toFixed(1)} subR=${b.subR.toFixed(0)} menuL=${b.menuL.toFixed(0)}`);
    ok('B2b 浮窗不覆盖主面板任何一行', !b.coversMenu, '');
    ok('B3 浮窗与行垂直方向有交集', b.overlapY, `subTop=${b.subTop.toFixed(0)} rowTop=${b.rowTop.toFixed(0)}`);
    ok('B4 行带 exp 高亮', b.rowExp, '');
    const want = ['默认（跟随服务）', ...WANT_LV];
    ok('B5 内容=默认 + ' + WANT_LV.join('/'),
      b.items.length === want.length && want.every((x, i) => b.items[i] === x), JSON.stringify(b.items));
    await shot(t, join(SCREEN, 'modelsub-1-hover.png'));
    const rp = await read(t, `(() => { const r = document.querySelector('#model-pop .model-item[data-key="${plainKey}"]').getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
    await moveTo(t, rp.x, rp.y);
    const b6 = await flyout(t, plainKey);
    ok('B6 悬停另一模型行：浮窗跟着切过去且仍是同一套六档',
      b6.open && b6.rowExp && b6.items.length === want.length, JSON.stringify(b6.items));

    // ---------- C 点「Low」：一并选中该模型并定档，双面板收起 ----------
    await moveTo(t, rb.x, rb.y);
    const lowLabel = LV_LABEL.low;
    await clickEl(t, `[...document.querySelectorAll('#model-sub-pop .mp-lv')].find((x) => x.textContent.trim() === ${JSON.stringify(lowLabel)})`);
    await sleep(400);
    const c1 = await state(t);
    ok('C1 定档后主面板收起', !c1.mainOpen, '');
    ok('C2 定档后二级浮窗收起', !c1.subOpen, '');
    const chip = await read(t, `document.getElementById('model-chip-label').textContent`);
    ok('C3 芯片文案=模型名+「' + lowLabel + '」', chip.includes(lowLabel) && chip.includes(target.modelName || target.modelId), chip);

    // ---------- D 重开：不幽灵弹出；点行主体 = 选中并整体收起 ----------
    await moveTo(t, 8, 8);
    await clickEl(t, `document.getElementById('model-chip')`);
    await sleep(400);
    const d0 = await state(t);
    ok('D1 重开菜单不幽灵弹出上次浮窗', d0.mainOpen && !d0.subOpen, '');
    const dsKey = models.find((m) => m.modelId === 'deepseek-v4-pro');
    if (dsKey) {
      const chip0 = await read(t, `document.getElementById('model-chip-label').textContent`);
      await clickEl(t, `document.querySelector('#model-pop .model-item[data-key="${dsKey.providerId}|${dsKey.modelId}"]')`);
      await sleep(350);
      const d1 = await state(t);
      ok('D2 点行主体=选中并整体收起（主面板）', !d1.mainOpen, '');
      ok('D3 点行主体不弹二级浮窗', !d1.subOpen, '');
      const chip1 = await read(t, `document.getElementById('model-chip-label').textContent`);
      ok('D4 芯片切到 DeepSeek-V4-Pro', chip1.includes('DeepSeek-V4-Pro') && !chip1.includes(chip0), chip1);
    }

    // ---------- E 点箭头展开；移开指针自动收起；未选中行上定档=一并选中 ----------
    await moveTo(t, 8, 8); // 先把指针挪远：菜单打开位置不压指针，避免合成悬停误弹
    await sleep(400); // 等指针离开面板的收起计时走完，否则它会在刚打开菜单时把面板关掉
    await openModelMenu(t);
    if (!(await state(t)).mainOpen) {
      await clickEl(t, `document.getElementById('model-chip')`);
      await sleep(350);
    }
    await clickEl(t, `document.querySelector('#model-pop .model-item[data-key="${key}"] .lv-hit')`);
    await sleep(300);
    const e1 = await flyout(t, key);
    ok('E1 点箭头二级浮窗弹出（行 exp）', e1.open && e1.rowExp, JSON.stringify(e1.items));
    await moveTo(t, 8, 8); // 指针离开面板 → 延迟收起
    await sleep(450);
    ok('E2 指针移开后浮窗自动收起（250ms 计时）', !(await state(t)).subOpen, '');
    // E1 的 clickE 里 scrollIntoView 可能把菜单滚过，行坐标一律现取现用（rb 只在 B/C 段内有效）
    const rowCenter = (k) => read(t, `(() => { const r = document.querySelector('#model-pop .model-item[data-key="${k}"]').getBoundingClientRect(); return { x: r.left + r.width / 2, y: r.top + r.height / 2 }; })()`);
    const rc = await rowCenter(key);
    await moveTo(t, rc.x, rc.y); // 悬停重新展开，直接在未选中的行上定档
    await sleep(250);
    const high = LV_LABEL.high;
    await clickEl(t, `[...document.querySelectorAll('#model-sub-pop .mp-lv')].find((x) => x.textContent.trim() === ${JSON.stringify(high)})`);
    await sleep(400);
    const e2 = await state(t);
    const chip2 = await read(t, `document.getElementById('model-chip-label').textContent`);
    ok('E3 未选中行定档=一并选中并收起', !e2.mainOpen && !e2.subOpen && chip2.includes(target.modelName || target.modelId) && chip2.includes(high), chip2);

    // ---------- F 箭头切换与桌面语义 ----------
    // 注：触屏分支（hover:none 下箭头=开合开关、悬停不弹）无法在无头桌面验证——
    // 本机 Edge 的 Emulation.setEmulatedMedia 不支持 hover/pointer 特性（已实测）。
    // 点行=选中并收起与设备无关（D 段已测），箭头展开路径与触屏共用（下面覆盖切换分支）。
    await moveTo(t, 8, 8);
    await clickEl(t, `document.getElementById('model-chip')`);
    await sleep(300);
    const dsExpr = `document.querySelector('#model-pop .model-item[data-key="mock|deepseek-v4-pro"] .lv-hit')`;
    await clickEl(t, dsExpr); // GLM 展开着（悬停）时点 DeepSeek 的箭头 → 浮窗切到 DeepSeek
    await sleep(300);
    const f1 = await read(t, `(() => ({
      subOpen: !document.getElementById('model-sub-pop').classList.contains('hidden'),
      dsExp: document.querySelector('#model-pop .model-item[data-key="mock|deepseek-v4-pro"]').classList.contains('exp'),
      items: [...document.querySelectorAll('#model-sub-pop .mp-lv')].map((x) => x.textContent.trim()),
    }))()`);
    ok('F1 点另一行箭头：浮窗切到该行（行 exp，内容仍是六档）',
      f1.subOpen && f1.dsExp && f1.items.length === WANT_LV.length + 1, JSON.stringify(f1.items));
    await clickEl(t, dsExpr); // 桌面分支：已展开的行再点箭头 = 保持展开（不误收）
    await sleep(300);
    ok('F2 桌面上已展开行再点箭头保持展开', (await state(t)).subOpen, '');
    await shot(t, join(SCREEN, 'modelsub-2-caret-switch.png'));
    await clickEl(t, `document.querySelector('#model-pop .model-item[data-key="mock|deepseek-v4-pro"]')`); // 点行主体收起
    await sleep(350);
    const f3 = await state(t);
    ok('F3 点行主体=选中并整体收起', !f3.mainOpen && !f3.subOpen, '');

    // ---------- G 窄视口：浮窗完整落在视口内 ----------
    await t.send('Emulation.setDeviceMetricsOverride', { width: 420, height: 800, deviceScaleFactor: 1, mobile: false });
    await sleep(300);
    await moveTo(t, 8, 8);
    await clickEl(t, `document.getElementById('model-chip')`);
    await sleep(300);
    await clickEl(t, `document.querySelector('#model-pop .model-item[data-key="${key}"] .lv-hit')`);
    await sleep(350);
    const g = await read(t, `(() => {
      const sub = document.getElementById('model-sub-pop');
      const sr = sub.getBoundingClientRect();
      return { open: !sub.classList.contains('hidden'), l: sr.left, r: sr.right, t: sr.top, b: sr.bottom,
        vw: window.innerWidth, vh: window.innerHeight };
    })()`);
    ok('G1 窄视口浮窗仍显示', g.open, '');
    ok('G2 浮窗完整在视口内', g.l >= 0 && g.r <= g.vw && g.t >= 0 && g.b <= g.vh,
      `l=${g.l.toFixed(0)} r=${g.r.toFixed(0)} vw=${g.vw}`);
    await shot(t, join(SCREEN, 'modelsub-3-narrow.png'));
  } finally {
    await closeTab(t);
  }

  const fail = results.filter((r) => !r.pass).length;
  console.log(fail ? `\n${fail} 项失败` : '\n全部通过');
  down();
  process.exit(fail ? 1 : 0);
}

main().catch((e) => { console.error('ERROR', e); down(); process.exit(2); });
