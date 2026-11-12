// 本轮两处改动的真机验证（自包含）：构建隔离实例（临时 exe + 独立 config/dataDir/
// 工作区 + 独立端口，端口与数据都在系统临时目录，用完即删）→ 拉起无头 Edge（CDP
// 9223）→ 在那个实例里发几轮**真实模型**消息 → 逐项断言并截图落
// gui-test-screenshots/thinking-resize-*.png：
//   T1 采集：主对话按会话档位记（high），不吃档位的辅助调用/老数据记空串
//   T2 明细 API 带 thinking 字段；按档位筛选生效（含「默认」档）
//   T3 「用量记录」表出现「推理强度」列，显示名 None/Low/.../Max，空值显示「默认」
//      （截图：浅色 + 深色）
//   T4 筛选区「推理强度」下拉可用（选 High 后表里只剩 High）
//   T5 设置窗口右下角把手：拖大后宽高同比例变、内容不破版（截图）
//   T6 双击把手复位到默认尺寸；调整结果被记住（刷新后打开设置仍是大窗口）
//   T7 窄视口：设置回到整屏自适应，用量页不破版（截图），无横向溢出
// 运行：仓库根目录 node gui-test/thinking-resize-verify.mjs
import { newTab, read, click, typeText, key, shot, center, sleep, closeTab } from './cdp.mjs';
import { mkdirSync, readFileSync, writeFileSync, rmSync, accessSync } from 'node:fs';
import { spawn, execSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const SCREEN = join(ROOT, 'gui-test-screenshots');
const PORT = 8833;
const BASE = `http://127.0.0.1:${PORT}`;
const CDP_HTTP = 'http://127.0.0.1:9223';
const TMP = join(tmpdir(), 'jx-thinking-resize-test');
mkdirSync(SCREEN, { recursive: true });

const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

let serverProc = null, edgePid = 0, edgeWasOurs = false;

async function up() {
  rmSync(TMP, { recursive: true, force: true });
  mkdirSync(join(TMP, 'ws'), { recursive: true });
  const exe = join(TMP, 'server.exe');
  execSync(`go build -o "${exe}" .`, { cwd: ROOT, stdio: 'inherit' });
  // 独立配置：**复制**用户日常配置里的渠道/密钥（DPAPI 密文本机同用户可解），
  // 只改端口、数据目录与工作区——绝不写仓库里的 config.json 与 data/
  const cfg = JSON.parse(readFileSync(join(ROOT, 'config.json'), 'utf8'));
  cfg.port = PORT;
  cfg.bindHost = '127.0.0.1';
  cfg.dataDir = join(TMP, 'data');
  cfg.workspace = join(TMP, 'ws');
  cfg.updateURL = '';
  cfg.maxSteps = 0;
  writeFileSync(join(TMP, 'config.json'), JSON.stringify(cfg, null, 2));

  // cwd 用仓库根：组件与技能按相对路径解析（就是仓库里这份组件）
  serverProc = spawn(exe, ['-config', join(TMP, 'config.json'), '-noopen'], { cwd: ROOT, stdio: 'ignore' });
  for (let i = 0; i < 60; i++) {
    await sleep(500);
    try { const r = await fetch(BASE + '/api/status'); if (r.ok) return; } catch (e) { /* wait */ }
  }
  throw new Error('隔离实例没起来（端口 ' + PORT + '）');
}

async function ensureEdge() {
  try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e) { /* 起一个 */ }
  const cands = [
    'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
    'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
  ];
  const edge = cands.find((p) => { try { accessSync(p); return true; } catch (e) { return false; } });
  if (!edge) throw new Error('没找到 msedge.exe');
  const proc = spawn(edge, [
    '--headless=new', '--remote-debugging-port=9223',
    '--user-data-dir=' + join(TMP, 'edge-profile'), 'about:blank',
  ], { stdio: 'ignore' });
  edgeWasOurs = true;
  edgePid = proc.pid;
  for (let i = 0; i < 30; i++) {
    await sleep(500);
    try { await fetch(CDP_HTTP + '/json/version'); return; } catch (e) { /* wait */ }
  }
  throw new Error('无头 Edge 没起来');
}

function down() {
  if (serverProc) { try { execSync(`taskkill /PID ${serverProc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ } }
  if (edgeWasOurs && edgePid) { try { execSync(`taskkill /PID ${edgePid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ } }
}

async function api(method, path, body) {
  const r = await fetch(BASE + path, {
    method,
    headers: { 'Content-Type': 'application/json' },
    body: body ? JSON.stringify(body) : undefined,
  });
  const text = await r.text();
  let json = null;
  try { json = JSON.parse(text); } catch (e) { /* 非 JSON */ }
  return { status: r.status, json, text };
}

async function openSession(client, sid) {
  await read(client, `openSession(${JSON.stringify(sid)})`);
  await sleep(900);
}

async function sendMessage(client, text) {
  const box = await center(client, '#input');
  await click(client, box.x, box.y);
  await typeText(client, text);
  await sleep(120);
  await key(client, 'Enter');
}

async function waitIdle(client, timeoutMs = 120000) {
  const started = Date.now();
  while (Date.now() - started < timeoutMs) {
    const busy = await read(client, `(() => {
      const stop = document.getElementById('btn-stop');
      return !!(stop && !stop.classList.contains('hidden'));
    })()`);
    if (!busy) return true;
    await sleep(300);
  }
  return false;
}

async function openUsagePane(client) {
  await read(client, `openSettings('component-usage-stats')`);
  await sleep(1200);
  for (let i = 0; i < 40; i++) {
    const n = await read(client, `document.querySelectorAll('#set-pane-component-usage-stats .us-records-table tbody tr').length`);
    if (n > 0) return true;
    await sleep(250);
  }
  return false;
}

// 真实鼠标拖动（pointer 事件由真实鼠标事件派生，和 WebView2 里一样）
async function dragBy(client, x, y, dx, dy, steps = 12) {
  await client.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: Math.round(x), y: Math.round(y), button: 'left', buttons: 1, clickCount: 1 });
  for (let i = 1; i <= steps; i++) {
    await client.send('Input.dispatchMouseEvent', {
      type: 'mouseMoved', x: Math.round(x + (dx * i) / steps), y: Math.round(y + (dy * i) / steps),
      button: 'left', buttons: 1,
    });
    await sleep(20);
  }
  await client.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: Math.round(x + dx), y: Math.round(y + dy), button: 'left', buttons: 0, clickCount: 1 });
  await sleep(200);
}

async function doubleClick(client, x, y) {
  for (const count of [1, 2]) {
    await client.send('Input.dispatchMouseEvent', { type: 'mousePressed', x: Math.round(x), y: Math.round(y), button: 'left', buttons: 1, clickCount: count });
    await client.send('Input.dispatchMouseEvent', { type: 'mouseReleased', x: Math.round(x), y: Math.round(y), button: 'left', buttons: 0, clickCount: count });
    await sleep(40);
  }
  await sleep(300);
}

const cardRect = (client) => read(client, `(() => {
  const c = document.querySelector('#settings .settings-card');
  if (!c) return null;
  const r = c.getBoundingClientRect();
  return { x: Math.round(r.left), y: Math.round(r.top), w: Math.round(r.width), h: Math.round(r.height),
    inlineW: c.style.width, inlineH: c.style.height, pos: c.style.position };
})()`);

const footerInside = (client) => read(client, `(() => {
  const c = document.querySelector('#settings .settings-card');
  const f = c.querySelector('.set-footer');
  const cr = c.getBoundingClientRect(), fr = f.getBoundingClientRect();
  return { insideBottom: fr.bottom <= cr.bottom + 1, insideRight: fr.right <= cr.right + 1, h: Math.round(fr.height) };
})()`);

let client = null;
try {
  await up();
  await ensureEdge();

  const st = await api('GET', '/api/status');
  ok('T0 隔离实例在线', st.status === 200, 'port=' + PORT + ' data=' + join(TMP, 'data'));

  // ---- 两个会话：一个 high 档，一个默认档（不指定） ----
  const mkHigh = await api('POST', '/api/sessions', {
    mode: 'task', workspace: join(TMP, 'ws'), title: '推理强度 high', thinking: 'high',
  });
  const sidHigh = (mkHigh.json && (mkHigh.json.id || (mkHigh.json.meta && mkHigh.json.meta.id))) || '';
  const mkDef = await api('POST', '/api/sessions', {
    mode: 'task', workspace: join(TMP, 'ws'), title: '默认档',
  });
  const sidDef = (mkDef.json && (mkDef.json.id || (mkDef.json.meta && mkDef.json.meta.id))) || '';
  ok('T0b 建两个会话（high / 默认）', !!sidHigh && !!sidDef, 'high=' + sidHigh + ' default=' + sidDef);

  client = await newTab(BASE + '/', 1400, 950);
  await sleep(800);
  await openSession(client, sidHigh);
  await sendMessage(client, '用两句话说说 Go 语言的 goroutine 是什么。');
  await waitIdle(client);
  await sleep(600);
  await openSession(client, sidDef);
  await sendMessage(client, '用一句话说明什么是 HTTP 的缓存命中。');
  await waitIdle(client);
  await sleep(900);

  // ---- T1/T2：落库字段与筛选 ----
  const recs = await api('GET', '/api/usage/records?limit=50');
  const items = (recs.json && recs.json.items) || [];
  // 同一会话可能有多条（主对话 + 标题生成等辅助调用）：看用途/档位，别只看第一条
  const highRows = items.filter((x) => x.sessionId === sidHigh)
    .map((x) => ({ purpose: x.purpose, thinking: x.thinking, ok: x.ok }));
  const defRows = items.filter((x) => x.sessionId === sidDef)
    .map((x) => ({ purpose: x.purpose, thinking: x.thinking, ok: x.ok }));
  // 会话里「真正对话」的那次调用（任务会话的用途是 task，辅助对话是 aux，主对话是 main）
  const chatPurposes = ['main', 'task', 'sub', 'aux'];
  const mainHigh = highRows.filter((x) => chatPurposes.includes(x.purpose));
  ok('T1 明细带 thinking 字段（会话档位 high 落库）',
    mainHigh.length >= 1 && mainHigh.every((x) => x.thinking === 'high'),
    'high 会话记录=' + JSON.stringify(highRows));
  // 不吃档位的辅助调用（标题生成/摘要/工具摘要）记空串——不能拿会话档位冒充
  const auxHigh = highRows.filter((x) => !chatPurposes.includes(x.purpose));
  const defChat = defRows.filter((x) => chatPurposes.includes(x.purpose));
  ok('T1b 辅助调用与默认档会话记空串',
    auxHigh.every((x) => !x.thinking) && defChat.length >= 1 && defChat.every((x) => !x.thinking),
    'high 会话辅助=' + JSON.stringify(auxHigh) + ' 默认会话=' + JSON.stringify(defRows));
  const onlyHigh = await api('GET', '/api/usage/records?limit=50&thinking=high');
  const highList = (onlyHigh.json && onlyHigh.json.items) || [];
  ok('T2 按档位筛选（thinking=high）只剩 high', highList.length >= 1 && highList.every((x) => x.thinking === 'high'),
    'total=' + (onlyHigh.json && onlyHigh.json.total));
  const onlyDef = await api('GET', '/api/usage/records?limit=50&thinking=default');
  const defList = (onlyDef.json && onlyDef.json.items) || [];
  ok('T2b 「默认」档筛选兜住空值', defList.length >= 1 && defList.every((x) => !x.thinking),
    'total=' + (onlyDef.json && onlyDef.json.total));

  // ---- T3：用量页面里看这一列 ----
  const paneOK = await openUsagePane(client);
  ok('T3 用量页渲染出明细表', paneOK, '');
  const table = await read(client, `(() => {
    const t = document.querySelector('#set-pane-component-usage-stats .us-records-table');
    if (!t) return null;
    const heads = [...t.querySelectorAll('thead th')].map((th) => th.textContent.trim());
    const idx = heads.indexOf('推理强度');
    const rows = [...t.querySelectorAll('tbody tr')].slice(0, 6).map((tr) => {
      const tds = [...tr.querySelectorAll('td')];
      return { model: (tds[1] || {}).textContent, thinking: (tds[idx] || {}).textContent };
    });
    return { heads, idx, rows, wrapScroll: t.parentElement.scrollWidth > t.parentElement.clientWidth + 1 };
  })()`);
  ok('T3b 明细表有「推理强度」列', !!table && table.idx > 0,
    '表头=' + JSON.stringify(table && table.heads));
  ok('T3c 列里显示档位显示名（High）', !!table && table.rows.some((r) => r.thinking === 'High'),
    JSON.stringify(table && table.rows));
  ok('T3d 空值显示「默认」', !!table && table.rows.some((r) => r.thinking === '默认'),
    JSON.stringify(table && table.rows));

  // 滚到明细表并截图（浅色）
  await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    const t = pane.querySelector('.us-records-table');
    if (t) t.scrollIntoView({ block: 'center' });
  })()`);
  await sleep(400);
  await shot(client, join(SCREEN, 'thinking-resize-1-records.png'));

  // ---- T4：筛选下拉 ----
  const filt = await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    const lab = [...pane.querySelectorAll('.us-filters label')].find((l) => l.textContent.includes('推理强度'));
    if (!lab) return null;
    const sel = lab.querySelector('select');
    return { opts: [...sel.options].map((o) => o.textContent), val: sel.value };
  })()`);
  ok('T4 筛选区有「推理强度」下拉（全部/默认/六档）',
    !!filt && filt.opts.length === 8 && filt.opts[0] === '全部推理强度' && filt.opts[1] === '默认' &&
      filt.opts.slice(2).join(',') === 'None,Low,Medium,High,Xhigh,Max',
    JSON.stringify(filt && filt.opts));
  if (filt) {
    // 真选一次 High：表里只剩 High
    await read(client, `(() => {
      const pane = document.getElementById('set-pane-component-usage-stats');
      const lab = [...pane.querySelectorAll('.us-filters label')].find((l) => l.textContent.includes('推理强度'));
      const sel = lab.querySelector('select');
      sel.value = 'high';
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    })()`);
    await sleep(1200);
    const filtered = await read(client, `(() => {
      const t = document.querySelector('#set-pane-component-usage-stats .us-records-table');
      if (!t) return null;
      const heads = [...t.querySelectorAll('thead th')].map((th) => th.textContent.trim());
      const idx = heads.indexOf('推理强度');
      return [...t.querySelectorAll('tbody tr')].map((tr) => (tr.querySelectorAll('td')[idx] || {}).textContent);
    })()`);
    ok('T4b 选 High 后表里只剩 High', !!filtered && filtered.length >= 1 && filtered.every((v) => v === 'High'),
      JSON.stringify(filtered));
    await shot(client, join(SCREEN, 'thinking-resize-2-filter.png'));
    await read(client, `(() => {
      const pane = document.getElementById('set-pane-component-usage-stats');
      const lab = [...pane.querySelectorAll('.us-filters label')].find((l) => l.textContent.includes('推理强度'));
      const sel = lab.querySelector('select');
      sel.value = '';
      sel.dispatchEvent(new Event('change', { bubbles: true }));
    })()`);
    await sleep(900);
  }

  // ---- T5：设置窗口等比拉伸 ----
  const before = await cardRect(client);
  const handle = await center(client, '#set-resize');
  await dragBy(client, handle.x, handle.y, 150, 110);
  const after = await cardRect(client);
  const ratioBefore = before.w / before.h;
  const ratioAfter = after.w / after.h;
  ok('T5 拖大后宽高都变大', after.w > before.w + 50 && after.h > before.h + 50,
    before.w + 'x' + before.h + ' → ' + after.w + 'x' + after.h);
  ok('T5b 宽高比保持（等比缩放）', Math.abs(ratioAfter - ratioBefore) < 0.05,
    '比例 ' + ratioBefore.toFixed(3) + ' → ' + ratioAfter.toFixed(3));
  const within = after.x >= -1 && after.y >= -1 && after.x + after.w <= 1401 && after.y + after.h <= 951;
  ok('T5c 拖大后不超出可视区域', within, JSON.stringify(after));
  const fit = await footerInside(client);
  ok('T5d 底栏仍在卡片内（没被裁掉）', fit.insideBottom && fit.insideRight && fit.h > 20, JSON.stringify(fit));
  const layout = await read(client, `(() => {
    const c = document.querySelector('#settings .settings-card');
    const nav = c.querySelector('.set-nav');
    const pane = c.querySelector('.set-pane.active');
    const nr = nav.getBoundingClientRect();
    return {
      navW: Math.round(nr.width),
      navVisible: nr.width > 0 && nr.height > 0,
      paneScrollable: !!pane && pane.scrollHeight >= pane.clientHeight,
      paneW: pane ? Math.round(pane.getBoundingClientRect().width) : 0,
      kpiCols: (() => {
        const k = c.querySelector('.us-kpis');
        return k ? getComputedStyle(k).gridTemplateColumns.split(' ').length : 0;
      })(),
    };
  })()`);
  ok('T5e 缩放后布局正常（左导航 188px、内容区有宽、KPI 栅格多列）',
    layout.navVisible && layout.navW >= 180 && layout.navW <= 200 && layout.paneW > 600 && layout.kpiCols >= 4,
    JSON.stringify(layout));
  await shot(client, join(SCREEN, 'thinking-resize-3-settings-large.png'));

  // ---- T6：记住尺寸（刷新后仍是大窗口）+ 双击复位 ----
  const draggedW = after.w;
  await read(client, `location.reload()`);
  await sleep(2200);
  await read(client, `openSettings('component-usage-stats')`);
  await sleep(1500);
  const reopened = await cardRect(client);
  ok('T6 刷新后打开设置沿用上次尺寸', Math.abs(reopened.w - draggedW) <= 3,
    '记住 ' + draggedW + ' → 重开 ' + reopened.w + '（h=' + reopened.h + '）');
  const handle2 = await center(client, '#set-resize');
  await doubleClick(client, handle2.x, handle2.y);
  const reset = await cardRect(client);
  ok('T6b 双击把手复位到默认尺寸', Math.abs(reset.w - before.w) <= 3 && Math.abs(reset.h - before.h) <= 3,
    reset.w + 'x' + reset.h + '（默认 ' + before.w + 'x' + before.h + '）');
  // 保险：前面任何一步若误把设置关掉了，这里重新打开（后面的深色/窄屏截图要有设置窗）
  await read(client, `(() => {
    if (document.getElementById('settings').classList.contains('hidden')) openSettings('component-usage-stats');
  })()`);
  await sleep(1200);

  // ---- 深色主题截图（用量记录表 + 大窗口） ----
  await read(client, `document.documentElement.dataset.theme = 'dark'`);
  await sleep(500);
  await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    const t = pane.querySelector('.us-records-table');
    if (t) t.scrollIntoView({ block: 'center' });
  })()`);
  await sleep(400);
  await shot(client, join(SCREEN, 'thinking-resize-4-dark.png'));
  const darkCol = await read(client, `(() => {
    const t = document.querySelector('#set-pane-component-usage-stats .us-records-table');
    const td = t.querySelector('tbody tr td');
    return { cellColor: getComputedStyle(td).color, cardBg: getComputedStyle(t.closest('.us-card')).backgroundColor };
  })()`);
  ok('T7 深色主题下表格可读（卡片深底、文字非纯黑）',
    !/rgb\(0, 0, 0\)/.test(darkCol.cellColor) && darkCol.cardBg !== 'rgb(255, 255, 255)',
    JSON.stringify(darkCol));
  await read(client, `document.documentElement.dataset.theme = 'light'`);
  await sleep(200);

  // ---- T7：窄视口（设置回到整屏、用量页不破版） ----
  await client.send('Emulation.setDeviceMetricsOverride', { width: 420, height: 860, deviceScaleFactor: 1, mobile: true });
  await sleep(900);
  const narrowCard = await cardRect(client);
  ok('T7b 窄屏设置退回整屏（清掉内联尺寸）', narrowCard.w >= 419 && narrowCard.h >= 859 && !narrowCard.inlineW,
    JSON.stringify(narrowCard));
  const overflow = await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    const t = pane.querySelector('.us-records-table');
    const wrap = t.parentElement;
    const card = document.querySelector('#settings .settings-card');
    const cr = card.getBoundingClientRect();
    return {
      docOverflow: document.documentElement.scrollWidth - window.innerWidth,
      cardOverflow: Math.round(cr.right - window.innerWidth),
      tableScrollsInside: wrap.scrollWidth > wrap.clientWidth,
      wrapWithinCard: wrap.getBoundingClientRect().right <= cr.right + 1,
      handleHidden: getComputedStyle(document.getElementById('set-resize')).display === 'none',
    };
  })()`);
  await shot(client, join(SCREEN, 'thinking-resize-5-narrow.png'));
  ok('T7c 窄屏不破版（文档无横向溢出、表格在卡片内横向滚动、把手隐藏）',
    overflow.docOverflow <= 2 && overflow.cardOverflow <= 2 && overflow.wrapWithinCard && overflow.handleHidden,
    JSON.stringify(overflow));
  await client.send('Emulation.setDeviceMetricsOverride', { width: 1400, height: 950, deviceScaleFactor: 1, mobile: false });
  await sleep(400);
} catch (e) {
  ok('运行异常', false, e && e.message ? e.message : String(e));
  console.error(e);
} finally {
  try { if (client) await closeTab(client); } catch (e) { /* 忽略 */ }
  down();
  rmSync(TMP, { recursive: true, force: true });
  const failed = results.filter((r) => !r.pass);
  console.log('\n===== 汇总：' + (results.length - failed.length) + '/' + results.length + ' 通过 =====');
  failed.forEach((f) => console.log('  FAIL ' + f.name + '  ' + f.detail));
  console.log('截图目录：' + SCREEN);
  process.exit(failed.length ? 1 : 0);
}
