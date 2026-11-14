// 右键「粘贴」浏览器（非桌面壳）模式验收：桌面壳专属的自定义右键菜单在浏览器
// 模式本就不注册（app-ctxmenu.js 的注册闸门），右键粘贴由浏览器原生菜单承担
// （原生粘贴 = 全功能，无需降级逻辑）。本脚本验证改动没有波及浏览器模式：
//   W1 页面正常启动（#input 存在、无 desktop-shell 类、启动期无 JS 报错）；
//   W2 app-ctxmenu.js 在浏览器模式照常解析装载（clipboardForPaste 等函数在）；
//   W3 自定义菜单未注册：合成 contextmenu 事件不弹 #ctx-menu（原生菜单接管）。
// 手机壳（jx-link 远程模式）加载同一前端、同样不带 desktop-shell，结论同 W1–W3。
// 运行：仓库根目录 node gui-test/ctxmenu-paste-webmode.mjs
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync, accessSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const ROOT = fileURLToPath(new URL('..', import.meta.url));
const TMP = join(tmpdir(), 'jx-paste-web');
const BASE = 'http://127.0.0.1:8845';
const CDP = 'http://127.0.0.1:9226';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

mkdirSync(join(TMP, 'ws'), { recursive: true });
writeFileSync(join(TMP, 'config.json'), JSON.stringify({
  port: 8845, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
  workspace: join(TMP, 'ws'), updateURL: '',
}));
const exe = join(TMP, 'server.exe');
execSync(`go build -o "${exe}" .`, { cwd: ROOT, stdio: 'inherit' });
const server = spawn(exe, ['-config', 'config.json', '-noopen'], { cwd: TMP, stdio: 'ignore' });
let up = false;
for (let i = 0; i < 40; i++) {
  await sleep(500);
  try { const r = await fetch(BASE + '/api/status'); if (r.ok) { up = true; break; } } catch (e) { /* wait */ }
}
if (!up) { console.log('FAIL 隔离实例没起来'); process.exit(1); }

const cands = [
  'C:\\Program Files (x86)\\Microsoft\\Edge\\Application\\msedge.exe',
  'C:\\Program Files\\Microsoft\\Edge\\Application\\msedge.exe',
];
const edge = cands.find((p) => { try { accessSync(p); return true; } catch (e) { return false; } });
if (!edge) { console.log('FAIL 没找到 msedge.exe'); process.exit(1); }
const edgeProc = spawn(edge, [
  '--headless=new', '--remote-debugging-port=9226',
  '--user-data-dir=' + join(TMP, 'edge-profile'), 'about:blank',
], { stdio: 'ignore' });
let ws = null, c = null;
outer:
for (let i = 0; i < 30; i++) {
  await sleep(500);
  try {
    const list = await (await fetch(CDP + '/json/list')).json();
    const page = list.find((t) => t.type === 'page');
    if (!page) continue;
    ws = new WebSocket(page.webSocketDebuggerUrl);
    await new Promise((ok2, bad2) => { ws.onopen = ok2; ws.onerror = bad2; });
    let seq = 0; const pending = new Map();
    ws.onmessage = (ev) => {
      const m = JSON.parse(ev.data);
      if (m.id && pending.has(m.id)) {
        const { ok: resolve, bad } = pending.get(m.id);
        pending.delete(m.id);
        m.error ? bad(new Error(m.error.message)) : resolve(m.result);
      }
    };
    c = { send(method, params = {}) {
      const id = ++seq;
      ws.send(JSON.stringify({ id, method, params }));
      return new Promise((ok2, bad2) => pending.set(id, { ok: ok2, bad: bad2 }));
    } };
    await c.send('Runtime.enable', {});
    await c.send('Page.enable', {});
    await c.send('Runtime.evaluate', { expression: `window.__errs = []; window.onerror = (m) => window.__errs.push(String(m));` });
    await c.send('Page.navigate', { url: BASE + '/' });
    for (let j = 0; j < 30; j++) {
      await sleep(500);
      const r = await c.send('Runtime.evaluate', { expression: `document.readyState`, returnByValue: true });
      if (r.result.value === 'complete') break outer;
    }
  } catch (e) { /* retry attach */ }
}
const read = async (expr) => {
  const r = await c.send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300));
  return r.result.value;
};
try {
  await sleep(1500); // 等前端初始化
  const state = JSON.parse(await read(`JSON.stringify({
    hasInput: !!document.getElementById('input'),
    desktopShell: document.documentElement.classList.contains('desktop-shell'),
    clipFn: typeof clipboardForPaste,
    pasteFn: typeof pasteIntoTarget,
    stagePaths: typeof stagePaths,
    errs: (window.__errs || []),
  })`));
  ok('W1 页面正常启动（输入框在、非桌面壳形态、无 JS 报错）',
    state.hasInput && !state.desktopShell && state.errs.length === 0, JSON.stringify(state));
  ok('W2 app-ctxmenu.js 照常装载（新粘贴函数已定义）',
    state.clipFn === 'function' && state.pasteFn === 'function' && state.stagePaths === 'function',
    `clipboardForPaste=${state.clipFn} pasteIntoTarget=${state.pasteFn} stagePaths=${state.stagePaths}`);
  const menuShown = await read(`(() => {
    document.getElementById('input').dispatchEvent(
      new MouseEvent('contextmenu', { bubbles: true, cancelable: true, clientX: 8, clientY: 8 }));
    return !!document.getElementById('ctx-menu');
  })()`);
  ok('W3 浏览器模式未注册自定义菜单（右键走浏览器原生菜单，粘贴本就全功能）', menuShown === false, 'ctx-menu=' + menuShown);
} catch (e) {
  ok('浏览器模式验收异常', false, String(e && e.message || e));
}
try { execSync(`taskkill /PID ${edgeProc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ }
try { execSync(`taskkill /PID ${server.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ }
const fails = results.filter((r) => !r.pass);
console.log(`\n==== ${results.length - fails.length}/${results.length} PASS ====`);
if (fails.length) { fails.forEach((f) => console.log('FAILED: ' + f.name)); process.exit(1); }
