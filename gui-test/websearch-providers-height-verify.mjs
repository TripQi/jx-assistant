// 「设置 → 网络访问 → 联网搜索」供应商列表「收起态每行等高」的真机验证：
// 自包含——构建隔离实例（临时 exe + 独立 config/data/工作区 + 独立端口，全在
// 系统临时目录，用完即删）、拉起无头 Edge（CDP 9223），在隔离实例里逐项验证并
// 截图到 gui-test-screenshots/websearch-providers-height-*.png：
//   H1 收起态四行等高（行1/行2/行3 三行固定槽位）
//   H2 操作按钮位置四行固定（右上角 x/y 一致）
//   H3 超长 baseURL 单行省略、行高不变
//   H4 顺序上移/下移几次，仍等高
//   H5 「测试」成功与失败出结果后行高不变（结果写进预留的状态槽位）
//   H6 公司网关「不可用」原因不额外占行、行高不变
//   H7 展开编辑变高（用户主动操作，允许）、收起后恢复等高
//   H8 深色 + 460px 窄屏：仍等高且无横向溢出
// 运行：仓库根目录 node gui-test/websearch-providers-height-verify.mjs
import { newTab, read, click, shot, center, sleep, closeTab } from './cdp.mjs';
import { mkdirSync, readFileSync, writeFileSync, rmSync, accessSync } from 'node:fs';
import { spawn, execSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const SCREEN = join(ROOT, 'gui-test-screenshots');
const PORT = 18473;
const BASE = `http://127.0.0.1:${PORT}`;
const CDP_HTTP = 'http://127.0.0.1:9223';
const TMP = join(tmpdir(), 'jx-sp-height');
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
  // 独立配置：**复制**用户日常配置（含 DPAPI 密文 Key，本机同用户可解），只改端口、
  // 数据目录与工作区——绝不写仓库里的 config.json 与 data/
  const cfg = JSON.parse(readFileSync(join(ROOT, 'config.json'), 'utf8'));
  cfg.port = PORT;
  cfg.bindHost = '127.0.0.1';
  cfg.dataDir = join(TMP, 'data');
  cfg.workspace = join(TMP, 'ws');
  cfg.updateURL = '';
  cfg.maxSteps = 0;
  cfg.searchURL = ''; // 公司网关刻意不配地址＝不可用，用来验「不可用原因不占额外一行」
  delete cfg.marketURL; // 别让网关从市场同源取到地址
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

async function getCfg() {
  const r = await fetch(BASE + '/api/config');
  return r.json();
}

async function postCfg(patch) {
  const r = await fetch(BASE + '/api/config', {
    method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify(patch),
  });
  return r.json();
}

// 视图 → 提交体（apiKey 空串＝沿用已保存的 Key；Key 只回掩码，不回传）
function cfgPayload(views) {
  return views.map((p, i) => ({
    id: p.id, name: p.name, kind: p.kind, baseURL: p.baseURL || '', apiKey: p.apiKey || '',
    capabilities: { search: !!(p.capabilities && p.capabilities.search), extract: !!(p.capabilities && p.capabilities.extract) },
    enabled: p.enabled !== false, order: i + 1,
    engine: p.engine || '', region: p.region || '', freshness: p.freshness || '',
    count: p.count || 0, timeoutSec: p.timeoutSec || 0,
  }));
}

// ---------- 页面操作 ----------
async function openPane(client) {
  await read(client, `openSettings('network')`);
  for (let i = 0; i < 40; i++) {
    await sleep(200);
    const n = await read(client, `document.querySelectorAll('#sp-list .sp-item').length`);
    if (n >= 4) break;
  }
  // 把供应商列表滚到视口内（设置页自己滚动）
  await read(client, `(() => { const el = document.querySelector('#sp-list'); if (el) el.scrollIntoView({ block: 'center' }); return true; })()`);
  await sleep(400);
}

// 从后端重读配置并重画列表（等价于保存后的一次回显）
async function refreshList(client) {
  await read(client, `(async () => { renderSearchProviders(await api('/api/config')); return true; })()`);
  await sleep(300);
}

// 量高：每行总高、三行槽位高、按钮区位置、地址/Key 是否被省略、状态槽内容
async function measure(client) {
  return read(client, `(() => {
    const rows = [...document.querySelectorAll('#sp-list .sp-item')];
    return rows.map((r) => {
      const g = (sel) => r.querySelector(sel);
      const box = (el) => { const b = el.getBoundingClientRect(); return b; };
      const b = box(r);
      const btns = box(g('.sp-btns'));
      const base = g('.sp-base');
      const kl = g('.sp-key');
      const res = g('.sp-result');
      const hint = g('.sp-hint');
      return {
        id: r.dataset.id, open: r.classList.contains('open'),
        h: +b.height.toFixed(2),
        head: +box(g('.sp-head')).height.toFixed(2),
        meta: +box(g('.sp-meta')).height.toFixed(2),
        status: +box(g('.sp-status')).height.toFixed(2),
        btnX: +btns.left.toFixed(2), btnY: +(btns.top - b.top).toFixed(2), btnW: +btns.width.toFixed(2),
        baseEllipsis: base ? base.scrollWidth > base.clientWidth + 1 : false,
        baseH: base ? +box(base).height.toFixed(2) : 0,
        keyEllipsis: kl ? kl.scrollWidth > kl.clientWidth + 1 : false,
        keyH: kl ? +box(kl).height.toFixed(2) : 0,
        hint: hint ? hint.textContent.trim() : '',
        result: res ? res.textContent.trim() : '',
        rowOverflowX: r.scrollWidth > r.clientWidth + 1,
        offenders: (() => {
          const rb = r.getBoundingClientRect().right - 9.5;
          const out = [];
          const walk = (el) => {
            for (const ch of el.children) {
              const b = ch.getBoundingClientRect();
              if (b.right > rb + 0.6) out.push(ch.className + '@' + b.right.toFixed(0));
              walk(ch);
            }
          };
          walk(r);
          return out;
        })(),
      };
    });
  })()`);
}

function heights(rows) {
  return rows.filter((r) => !r.open).map((r) => r.h);
}

function eqHeights(rows) {
  const hs = heights(rows);
  return { same: hs.length > 1 && hs.every((h) => Math.abs(h - hs[0]) < 0.5), hs };
}

async function clickAct(client, id, act) {
  const c = await center(client, `.sp-item[data-id="${id}"] [data-act="${act}"]`);
  await click(client, c.x, c.y);
  await sleep(500);
}

// 点「测试」并等结果落进状态槽（最多 35 秒——真实网络请求）
async function runTest(client, id) {
  const c = await center(client, `.sp-item[data-id="${id}"] [data-act="test"]`);
  await click(client, c.x, c.y);
  const started = Date.now();
  while (Date.now() - started < 35000) {
    await sleep(400);
    const t = await read(client, `(() => { const el = document.querySelector('.sp-item[data-id="${id}"] .sp-result'); return el ? el.textContent.trim() : ''; })()`);
    if (t && !t.includes('正在打一次')) return t;
  }
  return '(测试超时未回)';
}

async function main() {
  try {
    await up();
    await ensureEdge();
    const client = await newTab(BASE, 1280, 900);
    try {
      await openPane(client);

      // ---- H1/H2/H6：初始收起态（含公司网关不可用）----
      let rows = await measure(client);
      let eq = eqHeights(rows);
      ok('H1 收起态四行等高（初始）', eq.same, `行高 ${eq.hs.join(' / ')}，槽位 ${rows.map((r) => r.head + '+' + r.meta + '+' + r.status).join(' | ')}`);
      const bx = rows.map((r) => r.btnX), by = rows.map((r) => r.btnY);
      ok('H2 操作按钮四行位置固定', new Set(bx).size === 1 && new Set(by).size === 1,
        `left ${[...new Set(bx)].join(',')} / top-offset ${[...new Set(by)].join(',')}`);
      const gw = rows.find((r) => r.id === 'gateway');
      ok('H6 网关不可用原因在状态槽（不额外占行）', !!gw && gw.hint.includes('不可用') && Math.abs(gw.h - eq.hs[0]) < 0.5,
        `hint「${gw ? gw.hint : '(缺)'}」，行高 ${gw ? gw.h : '?'}`);
      await shot(client, join(SCREEN, 'websearch-providers-height-1-normal.png'));

      // ---- H3：超长 baseURL 单行省略、行高不变 ----
      let cfg = await getCfg();
      let views = cfg.searchProviders.map((p) => ({ ...p }));
      const longURL = 'https://api.example-very-long-host-name-for-ellipsis-check.example.com/v1/search/with/a/quite/long/path/segment?x=1&y=2';
      const tav = views.find((p) => p.id === 'tavily');
      if (tav) tav.baseURL = longURL;
      await postCfg({ searchProviders: cfgPayload(views) });
      await refreshList(client);
      rows = await measure(client);
      eq = eqHeights(rows);
      const tv = rows.find((r) => r.id === 'tavily');
      ok('H3 超长 baseURL 单行省略不撑高', eq.same && tv && tv.baseEllipsis && tv.baseH <= 17,
        `行高 ${eq.hs.join(' / ')}，tavily 地址省略=${tv && tv.baseEllipsis} 地址行高=${tv && tv.baseH}`);

      // ---- H4：顺序上下移几次 ----
      await clickAct(client, 'gateway', 'up'); // 公司网关上移到第 3
      rows = await measure(client);
      const eqA = eqHeights(rows);
      await clickAct(client, 'gateway', 'up'); // 再上移到第 2
      await clickAct(client, 'tavily', 'down'); // 再换一次
      rows = await measure(client);
      const eqB = eqHeights(rows);
      const order = rows.map((r) => r.id).join(' > ');
      ok('H4 上下移几次仍等高', eqA.same && eqB.same, `顺序 ${order}；行高 ${eqB.hs.join(' / ')}`);

      // ---- H7：展开编辑允许变高，收起后恢复（用 tavily：不带测试结果，免得重渲染冲掉别行的结果）----
      await clickAct(client, 'tavily', 'edit');
      const openRows = await measure(client);
      const openRow = openRows.find((r) => r.id === 'tavily');
      const eqOpen = eqHeights(openRows); // 其余三行仍应等高
      await shot(client, join(SCREEN, 'websearch-providers-height-4-edit-open.png'));
      await clickAct(client, 'tavily', 'edit');
      const backRows = await measure(client);
      const eqBack = eqHeights(backRows);
      ok('H7 展开编辑变高、收起后恢复等高',
        !!openRow && openRow.open && openRow.h > eqBack.hs[0] + 20 && eqOpen.same && eqBack.same,
        `展开高 ${openRow && openRow.h}（收起 ${eqBack.hs[0]}），展开时其余三行 ${eqOpen.hs.join(' / ')}，收起后 ${eqBack.hs.join(' / ')}`);

      // ---- H5：测试成功 + 测试失败（同一屏里两行都有结果，行高必须不变）----
      // 失败：把 deepseek 的 Key 改成假的（只写隔离实例的临时配置）
      cfg = await getCfg();
      views = cfg.searchProviders.map((p) => ({ ...p }));
      const ds = views.find((p) => p.id === 'deepseek');
      if (ds) ds.apiKey = 'sk-height-verify-bogus-key';
      await postCfg({ searchProviders: cfgPayload(views) });
      await refreshList(client);
      const dsRes = await runTest(client, 'deepseek');
      // 成功：任一真实可用的第三方（AnySearch 的 Key 来自用户配置副本）
      const anyRes = await runTest(client, 'anysearch');
      rows = await measure(client);
      eq = eqHeights(rows);
      const anyRow = rows.find((r) => r.id === 'anysearch');
      const dsRow = rows.find((r) => r.id === 'deepseek');
      ok('H5a 测试成功出结果，行高不变', /^✓|✓/.test(anyRes) && eq.same,
        `AnySearch 结果「${anyRes.slice(0, 90)}」；行高 ${eq.hs.join(' / ')}`);
      ok('H5b 测试失败出结果，行高不变', /✗/.test(dsRes) && eq.same && dsRow.result.includes('✗'),
        `DeepSeek 结果「${dsRes.slice(0, 90)}」；行高 ${eq.hs.join(' / ')}`);
      ok('H5c 结果在预留槽位内（状态行高仍固定）',
        [anyRow, dsRow].every((r) => Math.abs(r.status - rows[0].status) < 0.5),
        `状态行高 ${rows.map((r) => r.status).join(' / ')}`);
      await shot(client, join(SCREEN, 'websearch-providers-height-2-test-result.png'));

      // ---- H8：深色 + 460px 窄屏 ----
      await read(client, `(() => { localStorage.setItem('jx.theme', 'dark'); applyTheme(); return document.documentElement.dataset.theme; })()`);
      await client.send('Emulation.setDeviceMetricsOverride', { width: 460, height: 1000, deviceScaleFactor: 1, mobile: false });
      await sleep(500);
      await read(client, `(() => { const el = document.querySelector('#sp-list'); if (el) el.scrollIntoView({ block: 'center' }); return true; })()`);
      await sleep(400);
      rows = await measure(client);
      eq = eqHeights(rows);
      const docOverflow = await read(client, `document.documentElement.scrollWidth - document.documentElement.clientWidth`);
      ok('H8a 深色 460px 仍等高', eq.same, `行高 ${eq.hs.join(' / ')}，主题=${await read(client, `document.documentElement.dataset.theme`)}`);
      ok('H8b 窄屏无横向溢出', docOverflow <= 0 && !rows.some((r) => r.rowOverflowX),
        `页面溢出 ${docOverflow}px，行内溢出 ${rows.filter((r) => r.rowOverflowX).map((r) => r.id + '[' + r.offenders.join('/') + ']').join(',') || '无'}`);
      await shot(client, join(SCREEN, 'websearch-providers-height-3-dark-narrow.png'));
      await client.send('Emulation.setDeviceMetricsOverride', { width: 1280, height: 900, deviceScaleFactor: 1, mobile: false });
      await read(client, `(() => { localStorage.setItem('jx.theme', 'light'); applyTheme(); return true; })()`);
    } finally {
      await closeTab(client);
    }
  } catch (e) {
    ok('脚本异常', false, e && e.message ? e.message : String(e));
  } finally {
    down();
    try { rmSync(TMP, { recursive: true, force: true }); } catch (e) { /* Edge profile 句柄未释放，忽略 */ }
  }
  const bad = results.filter((r) => !r.pass);
  console.log('\n=== ' + (results.length - bad.length) + '/' + results.length + ' 通过 ===');
  if (bad.length) process.exitCode = 1;
}

await main();
