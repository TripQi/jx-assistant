// 右键「粘贴」与 Ctrl+V 链路差异·真机判定诊断（真桌面壳 WebView2 + CDP）。
// 要修的 bug：右键菜单「粘贴」只出文字，文件/图片进不了附件区。修法取向需要
// 三个事实，本脚本一次跑清：
//   Q1 execCommand('paste') 在桌面壳里是否可用（真实点击带用户激活时）——可用
//      则右键「粘贴」直接走它 = 与 Ctrl+V 同一条 paste 链路，最省事；
//   Q2 原生 paste（CDP 合成 Ctrl+V）在 WebView2 里能否带出 CF_HDROP 文件
//      （资源管理器复制的文件，app-chat.js 的 paste 处理器收不收得到 files）；
//   Q3 原生 paste 对只有 CF_DIB、没有 PNG 注册格式的剪贴板给不给 file（截图）。
// 每个场景顺带取一次 GET /api/clipboard，看现有桥对各场景返回什么（差距证据）。
// 多实例并行：JX_DESKTOP_MUTEX 覆盖互斥体名（acquireSingleInstance 的开发开关），
// 不影响正在使用的正式实例。
// 运行：仓库根目录 node gui-test/ctxmenu-paste-diag.mjs
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const TMP = join(tmpdir(), 'jx-paste-diag');
const BASE = 'http://127.0.0.1:8831';
const CDP = 'http://127.0.0.1:9224';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const out = [];

mkdirSync(join(TMP, 'ws'), { recursive: true });
writeFileSync(join(TMP, 'config.json'), JSON.stringify({
  port: 8831, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
  workspace: join(TMP, 'ws'), updateURL: '',
}));
writeFileSync(join(TMP, 'ws', 'diag-a.txt'), 'A\n');
writeFileSync(join(TMP, 'ws', 'diag-b.txt'), 'B\n');
const exe = join(TMP, 'server.exe');
execSync(`go build -o "${exe}" .`, { cwd: fileURLToPath(new URL('..', import.meta.url)), stdio: 'inherit' });
const proc = spawn(exe, ['-config', 'config.json', '-desktop'], {
  cwd: TMP, stdio: 'ignore',
  env: {
    ...process.env,
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: '--remote-debugging-port=9224',
    JX_DESKTOP_AUTOCLOSE: '300',
    JX_DESKTOP_MUTEX: 'jx-paste-diag',
  },
});

function makeClient(ws) {
  let seq = 0;
  const pending = new Map();
  ws.onmessage = (ev) => {
    const m = JSON.parse(ev.data);
    if (m.id && pending.has(m.id)) {
      const { ok: resolve, bad } = pending.get(m.id);
      pending.delete(m.id);
      m.error ? bad(new Error(m.error.message)) : resolve(m.result);
    }
  };
  return { send(method, params = {}) {
    const id = ++seq;
    ws.send(JSON.stringify({ id, method, params }));
    return new Promise((ok2, bad2) => pending.set(id, { ok: ok2, bad: bad2 }));
  }, close() { ws.close(); } };
}
async function attach() {
  for (let i = 0; i < 60; i++) {
    await sleep(1000);
    try {
      const list = await (await fetch(CDP + '/json/list')).json();
      const page = list.find((t) => t.type === 'page' && String(t.url).includes('shell=desktop'));
      if (!page) continue;
      const ws = new WebSocket(page.webSocketDebuggerUrl);
      await new Promise((ok2, bad2) => { ws.onopen = ok2; ws.onerror = bad2; });
      const c = makeClient(ws);
      await c.send('Runtime.enable', {});
      return c;
    } catch (e) { /* wait */ }
  }
  throw new Error('attach 失败');
}
async function read(c, expr) {
  const r = await c.send('Runtime.evaluate', { expression: expr, returnByValue: true, awaitPromise: true });
  if (r.exceptionDetails) throw new Error(JSON.stringify(r.exceptionDetails).slice(0, 300));
  return r.result.value;
}
async function click(c, x, y) {
  for (const type of ['mousePressed', 'mouseReleased']) {
    await c.send('Input.dispatchMouseEvent', { type, x: Math.round(x), y: Math.round(y), button: 'left', clickCount: 1 });
    await sleep(50);
  }
}
async function ctrlV(c) {
  await c.send('Input.dispatchKeyEvent', { type: 'keyDown', modifiers: 2, key: 'v', code: 'KeyV', windowsVirtualKeyCode: 86 });
  await sleep(80);
  await c.send('Input.dispatchKeyEvent', { type: 'keyUp', modifiers: 2, key: 'v', code: 'KeyV', windowsVirtualKeyCode: 86 });
  await sleep(500);
}
// PS 一律 -EncodedCommand（UTF-16LE base64）绕引号坑；剪贴板 API 需要 STA
function runPS(script) {
  const enc = Buffer.from(script, 'utf16le').toString('base64');
  try {
    return execSync(`powershell -NoProfile -STA -EncodedCommand ${enc}`, { encoding: 'utf8' }).trim();
  } catch (e) {
    return 'PS-ERR: ' + String(e.stderr || e.message).slice(0, 200);
  }
}
const clipText = () => runPS(`Set-Clipboard -Value 'diag-text-123'`);
const clipFiles = () => runPS(`Add-Type -AssemblyName System.Windows.Forms
$sc = New-Object System.Collections.Specialized.StringCollection
$sc.Add('${join(TMP, 'ws', 'diag-a.txt').replace(/\\/g, '\\\\')}')
$sc.Add('${join(TMP, 'ws', 'diag-b.txt').replace(/\\/g, '\\\\')}')
[System.Windows.Forms.Clipboard]::SetFileDropList($sc)
'done'`);
const clipImage = () => runPS(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bmp = New-Object System.Drawing.Bitmap(24,12)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.Clear([System.Drawing.Color]::FromArgb(200,10,10)); $g.Dispose()
[System.Windows.Forms.Clipboard]::SetImage($bmp)
'done'`);
async function focusWindow() {
  runPS(`$p = Get-Process -Id ${proc.pid} -ErrorAction SilentlyContinue
if ($p -and $p.MainWindowHandle -ne 0) {
  $sig = '[DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);'
  Add-Type -MemberDefinition $sig -Name W32 -Namespace U
  [U.W32]::SetForegroundWindow($p.MainWindowHandle) | Out-Null
  'fg'
} else { 'no-hwnd' }`);
  await sleep(400);
}
async function bridge() {
  try {
    const r = await fetch(BASE + '/api/clipboard');
    const j = await r.json();
    return JSON.stringify({ text: (j.text || '').slice(0, 40), pngLen: j.png ? j.png.length : 0 });
  } catch (e) { return 'ERR ' + e.message; }
}
const SNAP = `(() => {
  const box = document.getElementById('attachments');
  return JSON.stringify({
    focus: document.hasFocus(),
    pastes: (window.__pasteLog || []),
    input: document.getElementById('input').value,
    attach: box.children.length,
    attachHidden: box.classList.contains('hidden'),
  });
})()`;
async function reset(c) {
  await read(c, `(() => {
    document.getElementById('input').value = '';
    window.__pasteLog = [];
    if (typeof attachmentsOf === 'function' && typeof renderAttachments === 'function') {
      attachmentsOf(mainView).splice(0);
      renderAttachments(mainView);
    }
    return 1;
  })()`);
}
async function execPaste(c, pt) {
  await focusWindow();
  await click(c, pt.x, pt.y); // 真实点击带用户激活
  const ret = await read(c, `(() => {
    document.getElementById('input').focus();
    return document.execCommand('paste');
  })()`);
  await sleep(600);
  return ret;
}

let c;
try {
  c = await attach();
  await sleep(1500);
  // 页面探针：记下每次 paste 事件的 types/files/items（capture 阶段挂，先于应用自己的处理器）
  await read(c, `(() => {
    window.__pasteLog = [];
    document.getElementById('input').addEventListener('paste', (e) => {
      const cd = e.clipboardData || {};
      window.__pasteLog.push({
        types: [...(cd.types || [])].join(','),
        files: [...(cd.files || [])].map((f) => f.name + ':' + f.size + ':' + (f.type || '?')).join(' | '),
        items: [...(cd.items || [])].map((i) => i.kind + ':' + i.type).join(' | '),
      });
    }, true);
    return 1;
  })()`);
  const pt = await read(c, `(() => { const b = document.getElementById('input').getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);

  // ---- 文本 ----
  console.log('clip(text) =', clipText(), '| bridge =', await bridge());
  await reset(c);
  let ret = await execPaste(c, pt);
  out.push(['execCommand+text', { ret, snap: await read(c, SNAP) }]);
  await reset(c);
  await focusWindow(); await click(c, pt.x, pt.y);
  await ctrlV(c);
  out.push(['ctrlV+text', { snap: await read(c, SNAP) }]);

  // ---- 文件（CF_HDROP ×2）----
  console.log('clip(files) =', clipFiles(), '| bridge =', await bridge());
  await reset(c);
  await focusWindow(); await click(c, pt.x, pt.y);
  await ctrlV(c);
  out.push(['ctrlV+files', { snap: await read(c, SNAP) }]);
  await reset(c);
  ret = await execPaste(c, pt);
  out.push(['execCommand+files', { ret, snap: await read(c, SNAP) }]);

  // ---- 图片（CF_DIB，无 PNG 注册格式）----
  console.log('clip(image) =', clipImage(), '| bridge =', await bridge());
  await reset(c);
  await focusWindow(); await click(c, pt.x, pt.y);
  await ctrlV(c);
  out.push(['ctrlV+dib-image', { snap: await read(c, SNAP) }]);
  await reset(c);
  ret = await execPaste(c, pt);
  out.push(['execCommand+dib-image', { ret, snap: await read(c, SNAP) }]);

  // 权限态（只查询一次，供报告）
  const perm = await read(c, `navigator.permissions.query({name:'clipboard-read'}).then(p=>p.state).catch(e=>'ERR:'+e.name)`);
  console.log('clipboard-read 权限态:', perm);
  c.close();
} catch (e) {
  console.log('诊断异常:', String(e && e.message || e));
  try { c && c.close(); } catch (e2) { /* ignore */ }
}
try { execSync(`taskkill /PID ${proc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ }

console.log('\n==== 汇总 ====');
for (const [tag, v] of out) {
  console.log('\n[' + tag + '] ' + (v.ret !== undefined ? 'execCommand=' + v.ret + ' ' : '') + v.snap);
}
