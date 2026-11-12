// 「用量统计 · 搜索调用 + 分时段计费」的真机验证：自包含——构建隔离实例（临时
// exe + 独立 config/dataDir/工作区 + 独立端口，用完即删）、拉起无头 Edge（CDP 9223），
// 在那个实例里发一条**必须联网搜索**的真实模型消息，逐项断言：
//   S1 搜索明细：第 1 家（被故意改坏 Key 的供应商）失败 1 行 + 第 2 家成功 1 行——
//      回落要能在明细里看见（顺位 1/2、失败类别、查询摘要）
//   S2 搜索 KPI：次数 / 成功 / 失败 / 回落率 / 估算费用（失败不计费）
//   S3 token 明细不被搜索行污染，顶部总花费 = 模型费用 + 搜索费用
//   S4 页面：用量页有「搜索调用」分区、单价表有「空闲折扣」列（截图落
//      gui-test-screenshots/usage-search-*.png）
// 运行：仓库根目录 node gui-test/usage-search-verify.mjs
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
const TMP = join(tmpdir(), 'jx-usage-search-test');
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
  // 独立配置：复制用户日常配置里的渠道/搜索 Key（DPAPI 密文本机同用户可解），
  // 只改端口、数据目录、工作区，并**故意把第 1 位搜索供应商的 Key 改坏**——
  // 这样一次真实搜索必然回落，正好验证「失败一行 + 成功一行」的记账。
  const cfg = JSON.parse(readFileSync(join(ROOT, 'config.json'), 'utf8'));
  cfg.port = PORT;
  cfg.bindHost = '127.0.0.1';
  cfg.dataDir = join(TMP, 'data');
  cfg.workspace = join(TMP, 'ws');
  cfg.updateURL = '';
  cfg.maxSteps = 0;
  cfg.searchURL = '';
  const provs = (cfg.searchProviders || []).filter((p) => p.kind !== 'gateway');
  if (provs.length < 2) throw new Error('隔离验证需要至少两家搜索供应商（配置里只有 ' + provs.length + ' 家）');
  provs[0].apiKey = 'invalid-key-for-usage-test'; // 明文：读取时原样使用 → 必失败
  provs[0].enabled = true;
  provs.forEach((p, i) => { p.order = i + 1; p.enabled = i < 2; });
  cfg.searchProviders = provs.slice(0, 2);
  writeFileSync(join(TMP, 'config.json'), JSON.stringify(cfg, null, 2));

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

async function waitIdle(client, timeoutMs = 180000) {
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
  ok('S0 隔离实例在线（迁移 0020 没炸库）', st.status === 200, 'port=' + PORT + ' data=' + join(TMP, 'data'));

  const mk = await api('POST', '/api/sessions', {
    mode: 'task', workspace: join(TMP, 'ws'), title: '搜索用量验证',
  });
  const sid = (mk.json && (mk.json.id || (mk.json.meta && mk.json.meta.id))) || '';
  ok('S0b 建会话', mk.status === 200 && !!sid, 'sid=' + sid);

  client = await newTab(BASE + '/', 1400, 950);
  await sleep(800);
  await openSession(client, sid);

  // 一次必须联网搜索的真实请求（Key 被改坏的那家必然失败 → 回落到第 2 家）
  await sendMessage(client, '这是测试：请务必调用 web_search 工具搜索「DeepSeek 官方 API 价格 2026」，' +
    '把前两条结果的标题和链接原样贴出来，不要凭记忆回答。');
  await waitIdle(client, 240000);
  await sleep(1500);

  // ---- S1/S2 搜索明细与 KPI ----
  const sr = (await api('GET', '/api/usage/search?limit=20')).json;
  const items = (sr && sr.items) || [];
  const kpi = (sr && sr.kpi) || {};
  ok('S1 搜索调用已记账', items.length >= 1, 'total=' + (sr && sr.total) + ' kpi=' + JSON.stringify(kpi));

  const fail = items.find((x) => x.rank === 1 && !x.ok);
  const succ = items.find((x) => x.rank === 2 && x.ok);
  ok('S1a 第 1 家失败一行（Key 被改坏）', !!fail,
    fail ? ('provider=' + fail.providerName + ' errKind=' + fail.errKind + ' detail=' + fail.detail) : '没找到 rank=1 的失败行');
  ok('S1b 第 2 家成功一行（回落可见）', !!succ,
    succ ? ('provider=' + succ.providerName + ' rank=' + succ.rank + ' 耗时=' + succ.durationMs + 'ms') : '没找到 rank=2 的成功行');
  ok('S1c 查询摘要落库且截断', !!items.find((x) => (x.detail || '').includes('DeepSeek')),
    JSON.stringify((items[0] || {}).detail));
  ok('S2 KPI 次数/成功/失败/回落率', kpi.calls >= 2 && kpi.success >= 1 && kpi.errors >= 1 && kpi.fallbackCalls >= 1,
    JSON.stringify(kpi));
  // 失败不计费：只有成功那次计费（第 2 家若为 0 元则合计 0）
  const expectCost = (sr.byProvider || []).reduce((s, p) => s + (Number(p.success) || 0) * (Number(p.unitPrice) || 0), 0);
  ok('S2b 失败不计费（费用 = Σ成功×单价）', Math.abs((Number(kpi.cost) || 0) - expectCost) < 1e-6,
    'kpi.cost=' + kpi.cost + ' 期望=' + expectCost);

  // ---- S3 token 合计与总花费 ----
  const sum = (await api('GET', '/api/usage/summary')).json;
  const recs = (await api('GET', '/api/usage/records?limit=50')).json;
  ok('S3 token 明细不含搜索行', ((recs && recs.items) || []).every((x) => !x.kind),
    'tokenRows=' + ((recs && recs.items) || []).length);
  const k = (sum && sum.kpi) || {};
  ok('S3b token 合计正常（有模型的 token）', (k.input || 0) > 0, 'input=' + k.input + ' output=' + k.output);
  ok('S3c 总花费 = 模型 + 搜索', Math.abs((Number(k.totalCost) || 0) - ((Number(k.cost) || 0) + (Number(k.searchCost) || 0))) < 1e-6,
    'total=' + k.totalCost + ' 模型=' + k.cost + ' 搜索=' + k.searchCost + ' 搜索次数=' + k.searchCalls);

  // ---- S4 页面渲染 ----
  const pane = await openUsagePane(client);
  ok('S4 用量页打开', pane, '');
  await sleep(1500);
  const ui = await read(client, `(() => {
    const root = document.getElementById('set-pane-component-usage-stats');
    if (!root) return {};
    const h4 = Array.from(root.querySelectorAll('h4')).map((h) => h.textContent);
    const th = Array.from(root.querySelectorAll('.us-price-table th')).map((t) => t.textContent);
    const kpis = Array.from(root.querySelectorAll('.us-kpi')).map((k) => (k.querySelector('.k-label') || {}).textContent || '');
    return {
      hasSearch: h4.some((x) => x.includes('搜索调用')),
      hasIdleCol: th.some((x) => x.includes('空闲折扣')),
      kpis,
      searchKpis: kpis.filter((x) => /搜索次数|回落率|估算搜索费用/.test(x)),
      totalKpi: kpis.includes('总花费'),
      priceRows: root.querySelectorAll('.us-price-table tbody tr').length,
    };
  })()`);
  ok('S4b 页面有「搜索调用」分区', !!ui.hasSearch, JSON.stringify(ui.searchKpis));
  ok('S4c 单价表有「空闲折扣」列', !!ui.hasIdleCol && ui.priceRows > 0, 'rows=' + ui.priceRows);
  ok('S4d KPI 有总花费卡', !!ui.totalKpi, JSON.stringify(ui.kpis));
  const text = await read(client, `(() => {
    const root = document.getElementById('set-pane-component-usage-stats');
    return root ? root.textContent.replace(/\\s+/g, ' ') : '';
  })()`);
  ok('S4e 页面显示搜索明细与失败类别', /搜索调用/.test(text) && (/(Key 无效|连不上|被限流|配置不完整|响应无法解析|上游服务故障)/.test(text)),
    String(text).slice(0, 160));
  await shot(client, join(SCREEN, 'usage-search-1-page.png'));
  ok('S4f 已截图', true, join(SCREEN, 'usage-search-1-page.png'));

  // 空闲折扣的 hover 说明与明细行的时段标注（token 明细行 title）
  const hasPeakTitle = await read(client, `(() => {
    const rows = Array.from(document.querySelectorAll('#set-pane-component-usage-stats .us-records-table tbody tr'));
    return rows.some((r) => /调用时段：(高峰|空闲)/.test(r.title || ''));
  })()`);
  ok('S4g 明细行标注高峰/空闲时段', !!hasPeakTitle, '');
} catch (e) {
  ok('异常', false, e && e.stack ? e.stack : String(e));
} finally {
  try { if (client) await closeTab(client); } catch (e) { /* ignore */ }
  down();
}

const failed = results.filter((r) => !r.pass);
console.log('\n==== ' + (results.length - failed.length) + '/' + results.length + ' 项通过 ====');
if (failed.length) {
  failed.forEach((f) => console.log('FAILED: ' + f.name + ' | ' + f.detail));
  process.exit(1);
}
