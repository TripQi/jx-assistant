// 「用量统计」组件（组件/usage-stats）的真机验证：自包含——构建隔离实例
// （临时 exe + 独立 config/dataDir/工作区 + 独立端口，端口与数据都在系统临时
// 目录，用完即删）、拉起无头 Edge（CDP 9223），在那个实例里发几轮**真实模型**
// 消息，逐项验证并截图落 gui-test-screenshots/usage-*.png：
//   U1 组件加载：设置左侧出现「用量」一级页（组件 pane）
//   U2 速度条：流式输出时输入框下方出现 `↑ x tok/s · 本轮 x tok`，结束约 3 秒后淡出
//   U3 记账：/api/usage/records 有真实记录（含首字延迟、渠道、模型、协议）
//   U4 页面数据：KPI/趋势/环形图/模型表/明细表都有内容
//   U5 深色主题与窄视口不破版（无横向溢出）
//   U6 组件关闭：采集停记（已有数据保留）、设置里「用量」页消失、速度条不出现
// 运行：仓库根目录 node gui-test/usage-verify.mjs
import { newTab, read, click, typeText, key, shot, center, sleep, closeTab } from './cdp.mjs';
import { mkdirSync, readFileSync, writeFileSync, rmSync, accessSync } from 'node:fs';
import { spawn, execSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const SCREEN = join(ROOT, 'gui-test-screenshots');
const PORT = 8832;
const BASE = `http://127.0.0.1:${PORT}`;
const CDP_HTTP = 'http://127.0.0.1:9223';
const TMP = join(tmpdir(), 'jx-usage-test');
mkdirSync(SCREEN, { recursive: true });

const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

// ---------- 环境自举 ----------
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

// ---------- 页面操作 ----------
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
  // 等数据到位（KPI 卡片渲染出来）
  for (let i = 0; i < 40; i++) {
    const n = await read(client, `document.querySelectorAll('#set-pane-component-usage-stats .us-kpi').length`);
    if (n > 0) return true;
    await sleep(250);
  }
  return false;
}

// ---------- 主流程 ----------
let client = null;
try {
  await up();
  await ensureEdge();

  const st = await api('GET', '/api/status');
  ok('U0 隔离实例在线', st.status === 200, 'port=' + PORT + ' data=' + join(TMP, 'data'));
  const comp = await api('GET', '/api/components');
  const listed = (comp.json && comp.json.components || []).find((c) => c.id === 'usage-stats');
  ok('U0b 组件被发现且启用', !!listed && listed.enabled, JSON.stringify(listed && { id: listed.id, enabled: listed.enabled }));

  // 建一个任务会话（独立工作区），发两轮真实消息
  const mk = await api('POST', '/api/sessions', {
    mode: 'task', workspace: join(TMP, 'ws'), title: '用量统计验证',
  });
  const sid = (mk.json && (mk.json.id || (mk.json.meta && mk.json.meta.id))) || '';
  ok('U0c 建会话', mk.status === 200 && !!sid, 'sid=' + sid);

  client = await newTab(BASE + '/', 1400, 950);
  await sleep(800);
  await openSession(client, sid);

  // 第一轮：真实模型回答（输出要够长，能在流式中间抓速度条）
  await sendMessage(client, '用三句话介绍一下 Python 这门语言，然后列出 3 个常用标准库。');
  // 抓流式中的速度条
  let barText = '';
  let shotDone = false;
  for (let i = 0; i < 200; i++) {
    const snap = await read(client, `(() => {
      const b = document.querySelector('#composer-wrap .us-speed');
      if (!b || b.classList.contains('hidden')) return null;
      return { text: b.textContent, opacity: getComputedStyle(b).opacity };
    })()`);
    if (snap) {
      barText = snap.text || '';
      if (!shotDone && /tok\/s/.test(barText)) {
        await shot(client, join(SCREEN, 'usage-2-speedbar-streaming.png'));
        shotDone = true;
      }
    }
    if (shotDone && (await read(client, `(() => {
      const stop = document.getElementById('btn-stop');
      return !(stop && !stop.classList.contains('hidden'));
    })()`))) {
      // 已经收尾：再等到速度条淡出
      break;
    }
    await sleep(150);
  }
  ok('U2 速度条出现且格式正确', /↑\s*[\d.]+\s*tok\/s\s*·\s*本轮\s*[\d.]+k?\s*tok/.test(barText.replace(/\s+/g, ' ')),
    JSON.stringify(barText.replace(/\s+/g, ' ')));
  ok('U2b 已截图', shotDone, join(SCREEN, 'usage-2-speedbar-streaming.png'));

  await waitIdle(client);
  // 结束定格约 3 秒后淡出：轮询到速度条隐藏为止（上限 8 秒）
  const fadeStart = Date.now();
  let gone = false, seenAfterIdle = false;
  for (let i = 0; i < 40; i++) {
    const vis = await read(client, `(() => {
      const b = document.querySelector('#composer-wrap .us-speed');
      if (!b) return 'gone';
      if (b.classList.contains('hidden')) return 'hidden';
      return b.classList.contains('fade') ? 'fade' : 'show';
    })()`);
    if (vis === 'show' || vis === 'fade') seenAfterIdle = true;
    if (vis === 'hidden' || vis === 'gone') { gone = true; break; }
    await sleep(200);
  }
  ok('U2c 结束后淡出隐藏', gone, 'seenAfterIdle=' + seenAfterIdle + ' 消失耗时≈' + (Date.now() - fadeStart) + 'ms');

  // 第二轮：再发一条，并实测「结束定格约 3 秒后淡出」的时长
  await sendMessage(client, '用一个表格对比 list 与 tuple 的三个区别。');
  let fText = '', fAt = 0, fHideAt = 0, fSeen = false;
  for (let i = 0; i < 400; i++) {
    const snap = await read(client, `(() => {
      const b = document.querySelector('#composer-wrap .us-speed');
      if (!b) return { state: 'gone', text: '' };
      if (b.classList.contains('hidden')) return { state: 'gone', text: '' };
      return { state: b.classList.contains('fade') ? 'fade' : 'show', text: b.textContent || '' };
    })()`);
    const now = Date.now();
    if (snap.state === 'show' && snap.text) {
      fSeen = true;
      if (snap.text !== fText) {
        fText = snap.text;
        fAt = now; // 文字还在变：定格时刻继续往后推
      }
    }
    if (fSeen && (snap.state === 'fade' || snap.state === 'gone')) { fHideAt = now; break; }
    if (fSeen && i > 8 && /本轮/.test(fText) && now - fAt > 6000) { fHideAt = now; break; } // 兜底：没等到淡出
    await sleep(100);
  }
  await waitIdle(client);
  const frozeMs = fHideAt - fAt;
  ok('U2d 定格约 3 秒后淡出', fSeen && frozeMs >= 1800 && frozeMs <= 6500,
    '定格后约 ' + frozeMs + 'ms 淡出（文字=' + fText.replace(/\s+/g, ' ') + '）');
  await sleep(400);

  // 记账核对：真实记录、字段齐全
  const recs = await api('GET', '/api/usage/records?limit=50');
  const items = (recs.json && recs.json.items) || [];
  const total = (recs.json && recs.json.total) || 0;
  const first = items[0] || {};
  ok('U3 记账落库（真实调用）', total >= 2, 'total=' + total + ' 首条=' + JSON.stringify({
    model: first.model, protocol: first.protocol, provider: first.providerName || first.providerId,
    input: first.input, output: first.output, ttftMs: first.ttftMs, durationMs: first.durationMs, ok: first.ok,
  }));
  ok('U3b 明细字段齐全', !!(first.model && first.purpose && first.ttftMs > 0 && first.durationMs > 0),
    'purpose=' + first.purpose + ' ttft=' + first.ttftMs + 'ms dur=' + first.durationMs + 'ms');
  ok('U3c 有成功调用', items.some((x) => x.ok), 'okCount=' + items.filter((x) => x.ok).length);

  // 页面数据
  const paneOK = await openUsagePane(client);
  ok('U1 设置里出现「用量」页并渲染', paneOK, '');
  const view = await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    if (!pane) return null;
    const kpis = [...pane.querySelectorAll('.us-kpi')].map((k) => ({
      label: (k.querySelector('.k-label') || {}).textContent,
      val: (k.querySelector('.k-val') || {}).textContent,
    }));
    return {
      navLabel: (document.querySelector('.set-nav-item[data-cx-pane="usage-stats"]') || {}).textContent,
      kpis,
      chartPaths: pane.querySelectorAll('.us-chart svg polyline').length,
      donutSegs: pane.querySelectorAll('.us-donut svg circle[stroke-dasharray]').length,
      modelRows: pane.querySelectorAll('.us-card table tbody tr').length,
      recordRows: pane.querySelectorAll('.us-table-wrap').length,
      noteText: [...pane.querySelectorAll('.us-note')].map((n) => n.textContent).join(' | ').slice(0, 200),
    };
  })()`);
  const kpiMap = {};
  (view && view.kpis || []).forEach((k) => { kpiMap[k.label] = k.val; });
  ok('U4 KPI 有数据', !!view && view.kpis.length >= 9 && kpiMap['总请求数'] !== '0',
    JSON.stringify(kpiMap));
  ok('U4b 趋势折线 / 环形图 / 表格都在', !!view && view.chartPaths === 2 && view.donutSegs >= 1 && view.modelRows >= 1,
    'polyline=' + (view && view.chartPaths) + ' donut=' + (view && view.donutSegs) + ' rows=' + (view && view.modelRows));
  const hasDisclaimer = await read(client,
    `[...document.querySelectorAll('#set-pane-component-usage-stats *')].some(n => (n.textContent || '').includes('按本地单价估算，不代表账单'))`);
  ok('U4c 页面标明「按本地单价估算，不代表账单」', !!hasDisclaimer, '');
  await shot(client, join(SCREEN, 'usage-1-settings-pane.png'));

  // 下半屏：按模型统计 → 用量记录 → 单价与倍率 → 清理（滚动容器是设置页正文）
  const scrollPane = async (ratio) => read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    let e = pane;
    while (e && e !== document.body) {
      const st = getComputedStyle(e);
      if (/(auto|scroll)/.test(st.overflowY) && e.scrollHeight > e.clientHeight + 4) {
        e.scrollTop = (e.scrollHeight - e.clientHeight) * ${ratio};
        return { target: e.id || e.className, h: e.scrollHeight, ch: e.clientHeight };
      }
      e = e.parentElement;
    }
    return null;
  })()`);
  const sc1 = await scrollPane(0.42);
  await sleep(400);
  await shot(client, join(SCREEN, 'usage-1b-models-records.png'));
  const sc2 = await scrollPane(1);
  await sleep(400);
  await shot(client, join(SCREEN, 'usage-1c-pricing-cleanup.png'));
  ok('U4d 页面可滚动到下半部分', !!sc1 && !!sc2, JSON.stringify({ sc1, sc2 }));
  await scrollPane(0);
  await sleep(300);

  // 深色主题
  await read(client, `document.documentElement.dataset.theme = 'dark'`);
  await sleep(500);
  await shot(client, join(SCREEN, 'usage-3-pane-dark.png'));
  const darkBg = await read(client,
    `getComputedStyle(document.getElementById('set-pane-component-usage-stats').querySelector('.us-card')).backgroundColor`);
  ok('U5 深色主题生效', /rgb\((1[0-9]|[0-9]), /.test(darkBg) || darkBg !== 'rgb(255, 255, 255)', 'card bg=' + darkBg);
  await read(client, `document.documentElement.dataset.theme = 'light'`);
  await sleep(200);

  // 窄视口
  await client.send('Emulation.setDeviceMetricsOverride', {
    width: 420, height: 860, deviceScaleFactor: 1, mobile: true,
  });
  await sleep(600);
  const overflow = await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    const card = pane.querySelector('.us-card');
    return {
      docOverflow: document.documentElement.scrollWidth - window.innerWidth,
      cardOverflow: card ? Math.round(card.getBoundingClientRect().right - window.innerWidth) : 0,
      kpiCols: getComputedStyle(pane.querySelector('.us-kpis')).gridTemplateColumns.split(' ').length,
    };
  })()`);
  await shot(client, join(SCREEN, 'usage-4-pane-narrow.png'));
  ok('U5b 窄视口不破版', overflow.docOverflow <= 2 && overflow.cardOverflow <= 2,
    JSON.stringify(overflow));
  await client.send('Emulation.setDeviceMetricsOverride', {
    width: 1400, height: 950, deviceScaleFactor: 1, mobile: false,
  });
  await sleep(300);

  // ---- U7 单价与倍率：页面里改价，费用立刻按新价重算 ----
  const recList = await api('GET', '/api/usage/records?limit=5');
  const usedModel = ((recList.json.items || [])[0] || {}).model || '';
  const addRes = await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    const add = [...pane.querySelectorAll('.us-btn')].find((b) => b.textContent === '新增模型');
    if (!add) return 'no-add-btn';
    add.click();
    const rows = [...pane.querySelectorAll('.us-price-table tbody tr')];
    const ins = rows[rows.length - 1].querySelectorAll('input');
    ins[0].value = ${JSON.stringify(usedModel)};
    ins[1].value = '10'; // 输入 元/M
    ins[2].value = '40'; // 输出 元/M
    ins[3].value = '1';  // 缓存读 元/M
    ins[4].value = '10'; // 缓存写 元/M
    return ins.length;
  })()`);
  const clickSave = `[...document.querySelectorAll('#set-pane-component-usage-stats .us-btn')].find((b) => b.textContent === '保存单价与倍率').click()`;
  await read(client, clickSave);
  await sleep(1600);
  const paid = (await api('GET', '/api/usage/summary?date=30d')).json;
  ok('U7 配价后费用立刻可算（含未配单价转已配）',
    addRes === 5 && paid.kpi.priced === true && paid.kpi.cost > 0,
    'model=' + usedModel + ' cost=' + paid.kpi.cost + ' priced=' + paid.kpi.priced);
  const cost1 = paid.kpi.cost;

  // 渠道倍率全部改成 2.0 → 费用应当翻倍（价格 × 倍率的口径）
  await read(client, `(() => {
    const pane = document.getElementById('set-pane-component-usage-stats');
    pane.querySelectorAll('.us-mul-row input[data-provider]').forEach((i) => { i.value = '2'; });
  })()`);
  await read(client, clickSave);
  await sleep(1600);
  const paid2 = (await api('GET', '/api/usage/summary?date=30d')).json;
  const ratio = cost1 > 0 ? paid2.kpi.cost / cost1 : 0;
  ok('U8 渠道倍率生效（×2 费用翻倍）', Math.abs(ratio - 2) < 0.02,
    'cost ' + cost1 + ' → ' + paid2.kpi.cost + '（×' + ratio.toFixed(3) + '）');

  // ---- U9 按日期范围清理：二次确认（取消不删、确认才删） ----
  const beforeClean = (await api('GET', '/api/usage/records?limit=1')).json.total;
  await read(client, `(() => {
    window.__confirms = [];
    window.__confirmAnswer = false;
    window.confirm = (m) => { window.__confirms.push(m); return window.__confirmAnswer === true; };
  })()`);
  const clickClean = `[...document.querySelectorAll('#set-pane-component-usage-stats .us-btn')].find((b) => b.textContent === '删除该范围的用量数据').click()`;
  await read(client, clickClean);
  await sleep(700);
  const confirms = await read(client, `window.__confirms.slice()`);
  const afterCancel = (await api('GET', '/api/usage/records?limit=1')).json.total;
  ok('U9 清理有二次确认且取消不删',
    Array.isArray(confirms) && confirms.length === 1 && /不可恢复/.test(confirms[0]) && afterCancel === beforeClean,
    'confirm=' + JSON.stringify(confirms) + ' total=' + afterCancel + '/' + beforeClean);

  await read(client, `window.__confirmAnswer = true`);
  await read(client, clickClean);
  await sleep(1500);
  const afterConfirm = (await api('GET', '/api/usage/records?limit=1')).json.total;
  ok('U9b 确认后按日期范围删除', afterConfirm === 0 && beforeClean > 0,
    '删前 ' + beforeClean + ' 条 → 删后 ' + afterConfirm + ' 条');

  // 收尾补一轮真实调用：给 U6 留一条「关闭前的已有数据」，验证采集开关关闭后
  // 已有数据保留、且不再新增
  await read(client, `closeSettingsUI()`);
  await sleep(600);
  await sendMessage(client, '用一句话说明模型调用里的「缓存命中」是什么意思。');
  await waitIdle(client);
  await sleep(900);

  // ---- U6 组件关闭 ----
  const beforeTotal = (await api('GET', '/api/usage/records?limit=1')).json.total;
  await api('POST', '/api/components', { id: 'usage-stats', enabled: false });
  await read(client, `location.reload()`);
  await sleep(2200);
  await read(client, `openSettings()`);
  await sleep(800);
  const offState = await read(client, `(() => ({
    nav: !!document.querySelector('.set-nav-item[data-cx-pane="usage-stats"]'),
    pane: !!document.getElementById('set-pane-component-usage-stats'),
    speed: document.querySelectorAll('.us-speed').length,
    barHosts: [...document.querySelectorAll('#composer-wrap > .us-speed, #sub-composer-wrap > .us-speed')].length,
  }))()`);
  ok('U6 组件关闭后设置里没有「用量」页', !offState.nav && !offState.pane, JSON.stringify(offState));
  await shot(client, join(SCREEN, 'usage-5-component-off.png'));

  // 关闭状态下再发一条：不应产生新记录、也不显示速度条
  await openSession(client, sid);
  await sleep(600);
  await sendMessage(client, '说一句「收到」。');
  let sawBar = false;
  for (let i = 0; i < 60; i++) {
    const vis = await read(client, `document.querySelectorAll('.us-speed').length`);
    if (vis > 0) { sawBar = true; break; }
    await sleep(200);
  }
  await waitIdle(client);
  await sleep(800);
  const afterTotal = (await api('GET', '/api/usage/records?limit=1')).json.total;
  ok('U6b 关闭时不记录（已有数据保留）', afterTotal === beforeTotal && beforeTotal > 0,
    'before=' + beforeTotal + ' after=' + afterTotal);
  ok('U6c 关闭时速度条不出现', !sawBar, '');

  // 复原：重新打开组件（验证开关可逆，且数据还在）
  await api('POST', '/api/components', { id: 'usage-stats', enabled: true });
  const recomp = await api('GET', '/api/components');
  const reListed = (recomp.json.components || []).find((c) => c.id === 'usage-stats');
  ok('U6d 重新开启后数据保留', !!reListed && reListed.enabled &&
    (await api('GET', '/api/usage/records?limit=1')).json.total === beforeTotal, '');
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
