// 右键「粘贴」与 Ctrl+V 一致性·真机验收（四场景 × 两链路，真桌面壳 CDP）。
// 背景：右键「粘贴」只出文字、文件/图片进不了附件区的 bug 已修——右键粘贴改走
// /api/clipboard 全量桥（文件 CF_HDROP 真实路径 → stagePaths；图片 PNG/DIB →
// stageFiles；纯文本 → 输入框），本脚本按四种剪贴板状态逐场景对照「右键菜单粘贴」
// 与「Ctrl+V 原生 paste」的结果（都应进附件区 / 都应进输入框）。
// 场景：①1 个文件 ②2 个文件 ③图片（只有 CF_DIB，无 PNG 注册格式——截图常态）
// ④纯文字；附带 ⑤PNG 注册格式 + DIB 同放（截图工具也常见的形态，回归既有分支）。
// 多实例并行：JX_DESKTOP_MUTEX 覆盖互斥体，不影响正在使用的正式实例。
// 运行：仓库根目录 node gui-test/ctxmenu-paste-verify.mjs
import { spawn, execSync } from 'node:child_process';
import { writeFileSync, mkdirSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileURLToPath } from 'node:url';

const TMP = join(tmpdir(), 'jx-paste-verify');
const BASE = 'http://127.0.0.1:8832';
const CDP = 'http://127.0.0.1:9225';
const sleep = (ms) => new Promise((r) => setTimeout(r, ms));
const results = [];
const ok = (name, pass, detail) => {
  results.push({ name, pass, detail });
  console.log((pass ? 'PASS ' : 'FAIL ') + name + (detail ? '  | ' + detail : ''));
};

mkdirSync(join(TMP, 'ws'), { recursive: true });
writeFileSync(join(TMP, 'config.json'), JSON.stringify({
  port: 8832, bindHost: '127.0.0.1', baseURL: 'mock', apiKey: '', model: 'mock-model',
  workspace: join(TMP, 'ws'), updateURL: '',
}));
writeFileSync(join(TMP, 'ws', '验收-A.txt'), 'A\n');
writeFileSync(join(TMP, 'ws', '验收-B.md'), '# B\n');
const exe = join(TMP, 'server.exe');
execSync(`go build -o "${exe}" .`, { cwd: fileURLToPath(new URL('..', import.meta.url)), stdio: 'inherit' });
const proc = spawn(exe, ['-config', 'config.json', '-desktop'], {
  cwd: TMP, stdio: 'ignore',
  env: {
    ...process.env,
    WEBVIEW2_ADDITIONAL_BROWSER_ARGUMENTS: '--remote-debugging-port=9225',
    JX_DESKTOP_AUTOCLOSE: '600',
    JX_DESKTOP_MUTEX: 'jx-paste-verify',
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
async function mouse(c, x, y, button) {
  for (const type of ['mousePressed', 'mouseReleased']) {
    await c.send('Input.dispatchMouseEvent', { type, x: Math.round(x), y: Math.round(y), button, clickCount: 1 });
    await sleep(50);
  }
}
const leftClick = (c, x, y) => mouse(c, x, y, 'left');
const rightClick = (c, x, y) => mouse(c, x, y, 'right');
async function ctrlV(c) {
  await c.send('Input.dispatchKeyEvent', { type: 'keyDown', modifiers: 2, key: 'v', code: 'KeyV', windowsVirtualKeyCode: 86 });
  await sleep(80);
  await c.send('Input.dispatchKeyEvent', { type: 'keyUp', modifiers: 2, key: 'v', code: 'KeyV', windowsVirtualKeyCode: 86 });
  await sleep(500);
}
// PS 一律 -EncodedCommand（UTF-16LE base64）绕引号坑；剪贴板 API 需要 STA。
// 剪贴板是全机共享资源：写入会偶发「did not succeed」（重试可解），更糟的是
// 偶发【无限阻塞】（剪贴板所有者延迟渲染，STA 线程挂死）——所以每次调用都给
// 硬超时，超时按失败处理进重试，绝不许测试脚本吊死。
function runPSOnce(script) {
  const enc = Buffer.from(script, 'utf16le').toString('base64');
  try {
    return execSync(`powershell -NoProfile -STA -EncodedCommand ${enc}`, { encoding: 'utf8', timeout: 15000 }).trim();
  } catch (e) { return 'PS-ERR'; }
}
async function runPS(script, tries = 3) {
  for (let i = 0; i < tries; i++) {
    const r = runPSOnce(script);
    if (r !== 'PS-ERR' && !/ExternalException|did not succeed/.test(r)) return r;
    await sleep(400);
  }
  return 'PS-ERR';
}
const esc = (s) => s.replace(/\\/g, '\\\\').replace(/'/g, "''");
const clipText = () => runPS(`Set-Clipboard -Value '验收文本ABC'
if ((Get-Clipboard) -ne '验收文本ABC') { throw 'clipboard-not-set' }
'done'`);
const clipFiles = (names) => runPS(`Add-Type -AssemblyName System.Windows.Forms
$sc = New-Object System.Collections.Specialized.StringCollection
${names.map((n) => `$sc.Add('${esc(join(TMP, 'ws', n))}') | Out-Null`).join('\n')}
[System.Windows.Forms.Clipboard]::SetFileDropList($sc)
'done'`);
const clipImageDIB = () => runPS(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bmp = New-Object System.Drawing.Bitmap(24,12)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.Clear([System.Drawing.Color]::FromArgb(10,120,200)); $g.Dispose()
[System.Windows.Forms.Clipboard]::SetImage($bmp)
'done'`);
const clipImagePNGfmt = () => runPS(`Add-Type -AssemblyName System.Windows.Forms
Add-Type -AssemblyName System.Drawing
$bmp = New-Object System.Drawing.Bitmap(24,12)
$g = [System.Drawing.Graphics]::FromImage($bmp)
$g.Clear([System.Drawing.Color]::FromArgb(200,10,10)); $g.Dispose()
$ms = New-Object System.IO.MemoryStream
$bmp.Save($ms, [System.Drawing.Imaging.ImageFormat]::Png)
$do = New-Object System.Windows.Forms.DataObject
$do.SetImage($bmp)
$do.SetData('PNG', $ms)
[System.Windows.Forms.Clipboard]::SetDataObject($do, $true)
'done'`);
async function focusWindow() {
  runPS(`$p = Get-Process -Id ${proc.pid} -ErrorAction SilentlyContinue
if ($p -and $p.MainWindowHandle -ne 0) {
  $sig = '[DllImport("user32.dll")] public static extern bool SetForegroundWindow(IntPtr h);'
  Add-Type -MemberDefinition $sig -Name W32 -Namespace U
  [U.W32]::SetForegroundWindow($p.MainWindowHandle) | Out-Null
}`);
  await sleep(400);
}
async function bridge() {
  try {
    const j = await (await fetch(BASE + '/api/clipboard')).json();
    return { text: (j.text || '').slice(0, 20), png: j.png ? j.png.length : 0, files: (j.files || []).map((f) => f.split(/[\\/]/).pop()) };
  } catch (e) { return 'ERR'; }
}
const STATE = `(() => {
  const box = document.getElementById('attachments');
  return JSON.stringify({
    input: document.getElementById('input').value,
    attach: (typeof attachmentsOf === 'function' ? attachmentsOf(mainView) : []).map((a) => a.name + '/' + a.kind + (a.path ? '/路径' : '')),
    railOpen: !box.classList.contains('hidden') && box.children.length > 0,
  });
})()`;
async function reset(c) {
  await read(c, `(() => {
    document.getElementById('input').value = '';
    if (typeof attachmentsOf === 'function' && typeof renderAttachments === 'function') {
      attachmentsOf(mainView).splice(0);
      renderAttachments(mainView);
    }
    return 1;
  })()`);
}
async function viaCtrlV(c, pt) {
  await reset(c);
  await focusWindow();
  // 点在输入框上并确认焦点真的落进去（首次点击偶发赶在布局稳定前，落空会让
  // Ctrl+V 的 paste 事件丢在 body 上）；没落进去就再点一次
  for (let i = 0; i < 3 && await read(c, `document.activeElement && document.activeElement.id`) !== 'input'; i++) {
    await leftClick(c, pt.x, pt.y);
    await sleep(150);
  }
  await ctrlV(c);
  return JSON.parse(await read(c, STATE));
}
async function viaMenu(c, pt) {
  await reset(c);
  // CDP 事件自带激活语义，菜单链路不需要抢 OS 焦点（既有冒烟 D2 同此做法）
  await rightClick(c, pt.x + 20, pt.y);
  const pb = await read(c, `(() => {
    const b = [...document.querySelectorAll('#ctx-menu .pop-item')].find((x) => x.textContent === '粘贴');
    if (!b) return null;
    const r = b.getBoundingClientRect();
    return { x: r.left + r.width / 2, y: r.top + r.height / 2 };
  })()`);
  if (!pb) return { error: '菜单里找不到「粘贴」项' };
  await leftClick(c, pb.x, pb.y);
  await sleep(900); // /api/clipboard 往返 + 附件渲染
  return JSON.parse(await read(c, STATE));
}
const names = (s) => (s.attach || []).map((x) => x.split('/')[0]);
const kinds = (s) => (s.attach || []).map((x) => x.split('/')[1]);

let c;
try {
  c = await attach();
  await sleep(2500); // 等前端初始化与首屏布局稳定（首场景点击要落在输入框上）
  const pt = await read(c, `(() => { const b = document.getElementById('input').getBoundingClientRect(); return { x: b.left + b.width / 2, y: b.top + b.height / 2 }; })()`);

  // ① 单文件
  console.log('clip(1 file) =', await clipFiles(['验收-A.txt']), '| bridge =', JSON.stringify(await bridge()));
  const s1a = await viaCtrlV(c, pt), s1b = await viaMenu(c, pt);
  ok('①单文件 Ctrl+V → 入附件区', s1a.attach.length === 1 && names(s1a)[0] === '验收-A.txt', JSON.stringify(s1a));
  ok('①单文件 右键粘贴 → 入附件区（同一入口，路径型）', s1b.attach.length === 1 && names(s1b)[0] === '验收-A.txt' && s1b.attach[0].includes('路径'), JSON.stringify(s1b));

  // ② 多文件
  console.log('clip(2 files) =', await clipFiles(['验收-A.txt', '验收-B.md']), '| bridge =', JSON.stringify(await bridge()));
  const s2a = await viaCtrlV(c, pt), s2b = await viaMenu(c, pt);
  ok('②多文件 Ctrl+V → 两张卡', s2a.attach.length === 2 && names(s2a).join(',') === '验收-A.txt,验收-B.md', JSON.stringify(s2a));
  ok('②多文件 右键粘贴 → 两张卡', s2b.attach.length === 2 && names(s2b).join(',') === '验收-A.txt,验收-B.md', JSON.stringify(s2b));

  // ③ 图片（只有 CF_DIB）
  console.log('clip(DIB image) =', await clipImageDIB(), '| bridge =', JSON.stringify(await bridge()));
  const s3a = await viaCtrlV(c, pt), s3b = await viaMenu(c, pt);
  ok('③图片(CF_DIB) Ctrl+V → 图片附件', s3a.attach.length === 1 && kinds(s3a)[0] === 'image', JSON.stringify(s3a));
  ok('③图片(CF_DIB) 右键粘贴 → 图片附件', s3b.attach.length === 1 && kinds(s3b)[0] === 'image', JSON.stringify(s3b));

  // ④ 纯文字
  console.log('clip(text) =', await clipText(), '| bridge =', JSON.stringify(await bridge()));
  const s4a = await viaCtrlV(c, pt), s4b = await viaMenu(c, pt);
  ok('④纯文字 Ctrl+V → 进输入框', s4a.input === '验收文本ABC' && !s4a.attach.length, JSON.stringify(s4a));
  ok('④纯文字 右键粘贴 → 进输入框', s4b.input === '验收文本ABC' && !s4b.attach.length, JSON.stringify(s4b));

  // ⑤ PNG 注册格式 + DIB 同放（截图工具常见形态，回归既有分支）
  console.log('clip(PNG+DIB) =', await clipImagePNGfmt(), '| bridge =', JSON.stringify(await bridge()));
  const s5a = await viaCtrlV(c, pt), s5b = await viaMenu(c, pt);
  ok('⑤图片(PNG格式) Ctrl+V → 图片附件', s5a.attach.length === 1 && kinds(s5a)[0] === 'image', JSON.stringify(s5a));
  ok('⑤图片(PNG格式) 右键粘贴 → 图片附件', s5b.attach.length === 1 && kinds(s5b)[0] === 'image', JSON.stringify(s5b));

  c.close();
} catch (e) {
  ok('验收主流程异常', false, String(e && e.message || e));
  try { c && c.close(); } catch (e2) { /* ignore */ }
}
try { execSync(`taskkill /PID ${proc.pid} /T /F`, { stdio: 'ignore' }); } catch (e) { /* gone */ }
const fails = results.filter((r) => !r.pass);
console.log(`\n==== ${results.length - fails.length}/${results.length} PASS ====`);
if (fails.length) { fails.forEach((f) => console.log('FAILED: ' + f.name)); process.exit(1); }
