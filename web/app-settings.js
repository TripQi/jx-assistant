/* app-settings.js —— 设置弹窗：常规页/模型页/上下文页/组件/插件/技能、
   背景色与自定义下拉。第五个加载。 */

let provDraft = [];      // 模型页编辑中的提供方列表（不直接改全局配置）
let presetsCache = [];   // 后端预置提供方模板 [{id,name,baseURL}]
let provOpenKey = null;  // 展开的卡片：provider id / 'new' / null（手风琴式单开）
let provNew = null;      // 「添加提供方」的草稿
let provModelOpen = {};  // 卡片key -> 展开编辑的模型行下标（-1=无）
let provTest = {};       // 卡片key -> { 模型id: {state:'run'|'ok'|'fail', ms, err} }

// lsGet/lsSet 使用顶部 162 行的兼容版（对象 JSON、字符串原样），此处不再重复定义。

/* ---------- 关于页：问题反馈（标题+正文+截图，服务端入库存图；只收不看） ---------- */

const FB_MAX_IMAGES = 4;
let fbKind = 'bug';       // 当前选中的反馈类型
let fbImages = [];        // 待发送的图片 File 列表

function bindFeedbackForm() {
  const wrap = document.getElementById('feedback-section');
  if (!wrap || wrap.dataset.bound) return;
  wrap.dataset.bound = '1';
  wrap.querySelectorAll('.fb-kind').forEach((b) => {
    b.onclick = () => {
      fbKind = b.dataset.kind;
      wrap.querySelectorAll('.fb-kind').forEach((x) => x.classList.toggle('cur', x === b));
    };
  });
  $('fb-img-btn').onclick = () => $('fb-img-input').click();
  $('fb-img-input').onchange = () => {
    for (const f of $('fb-img-input').files) {
      if (fbImages.length >= FB_MAX_IMAGES) { alert(`截图最多 ${FB_MAX_IMAGES} 张`); break; }
      fbImages.push(f);
    }
    $('fb-img-input').value = '';
    renderFbImages();
  };
  $('fb-send').onclick = sendFeedback;
}

// renderFbImages 缩略图预览 + 单张移除
function renderFbImages() {
  const box = $('fb-img-previews');
  box.innerHTML = '';
  $('fb-img-hint').textContent = fbImages.length ? `${fbImages.length}/${FB_MAX_IMAGES} 张` : '';
  fbImages.forEach((f, i) => {
    const cell = document.createElement('span');
    cell.className = 'fb-thumb';
    const img = document.createElement('img');
    img.src = URL.createObjectURL(f);
    img.onload = () => URL.revokeObjectURL(img.src);
    const x = document.createElement('button');
    x.type = 'button';
    x.className = 'fb-thumb-x';
    x.textContent = '✕';
    x.title = '移除这张截图';
    x.onclick = () => { fbImages.splice(i, 1); renderFbImages(); };
    cell.appendChild(img);
    cell.appendChild(x);
    box.appendChild(cell);
  });
}

async function sendFeedback() {
  const title = $('fb-title').value.trim();
  const text = $('fb-text').value.trim();
  if (!title) { alert('请填写标题'); return; }
  if (!text) { alert('请填写正文'); return; }
  const btn = $('fb-send');
  btn.disabled = true;
  btn.textContent = '发送中…';
  try {
    // FormData：远程模式下由 remote-boot 自动转 multipart 帧协议走加密中继
    const fd = new FormData();
    fd.append('kind', fbKind);
    fd.append('title', title);
    fd.append('content', text);
    fd.append('sessionId', (state.cur && state.cur.id) || '');
    fd.append('diagnostics', JSON.stringify({ ua: navigator.userAgent, page: location.href }));
    for (const f of fbImages) fd.append('images', f, f.name);
    await fetch('/api/feedback', { method: 'POST', body: fd }).then(async (res) => {
      if (!res.ok) {
        let msg = res.statusText;
        try { msg = (await res.json()).error || msg; } catch (e) { /* 保留状态文本 */ }
        throw new Error(msg);
      }
    });
    $('fb-title').value = '';
    $('fb-text').value = '';
    fbImages = [];
    renderFbImages();
    btn.textContent = '已提交 ✓';
    const hint = $('fb-img-hint');
    if (hint) hint.textContent = '已记录；连接了公司服务端时会同步一份供排查（若网络异常会稍后自动重试）';
    setTimeout(() => { btn.textContent = '发送反馈'; }, 2500);
  } catch (e) {
    alert('发送失败：' + e.message);
    btn.textContent = '发送反馈';
  }
  btn.disabled = false;
}

// openFeedbackForm 打开设置关于页并预填反馈（报错条「反馈」按钮的入口）
async function openFeedbackForm(prefill, sessionId) {
  await openSettings('about');
  const ta = $('fb-text');
  if (ta && prefill && !ta.value.trim()) {
    $('fb-title').value = prefill.split('\n')[0].slice(0, 100);
    ta.value = prefill;
  }
  if (ta) ta.focus();
}

function loadProvDraft(c) {
  return (c.providers || []).map((p) => ({
    id: p.id,
    presetId: p.preset ? p.id : null, // 预置提供方用系统地址，不出 URL 输入框
    presetURL: p.presetURL || '',
    name: p.name || '',
    baseURL: p.baseURL || '',
    protocol: p.protocol || '',       // 渠道协议：''=OpenAI 兼容 / anthropic / responses
    key: '', keyMasked: p.apiKeyMasked || '',
    enabled: p.enabled !== false,   // 手动启停开关（服务商卡片头）
    autoDisabled: !!p.autoDisabled, // 公司通道自动停用（连不上服务端）
    company: !!p.company,           // 公司内部通道（删除提示/状态文案用）
    publicBaseURL: p.publicBaseURL || '', // 外网备用地址（服务端下发，只读展示）
    via: p.via || '',               // 公司通道当前线路：lan=内网 / wan=外网
    quota: p.quota || null,         // 公司通道额度快照（服务端随公告下发）
    portalURL: p.portalURL || '',   // 门户充值入口（2026-11-13 充值组件）：卡片挂载点 dataset 带给组件判显隐
    models: (p.models || []).map((m) => ({
      id: m.id || '', name: m.name || '',
      contextWindow: m.contextWindow || 0, maxOutput: m.maxOutput || 0,
      supportsImages: !!m.supportsImages,
    })),
  }));
}

// 草稿指纹：判断卡片相对「最近一次服务端状态」是否有改动。底部通用「保存」
// 只提交动过的卡片，未动过的一律不碰（避免无谓写盘，也避免覆盖服务端已有的元信息）。
function provSig(p) {
  return JSON.stringify({
    id: p.id || '', presetId: p.presetId || '', name: p.name || '', baseURL: p.baseURL || '',
    protocol: p.protocol || '',
    models: (p.models || []).map((m) => [m.id || '', m.name || '', m.contextWindow || 0, m.maxOutput || 0, !!m.supportsImages]),
  });
}

// 重载模型页草稿并记下基线（openSettings / reloadProviders 共用）。
// p.base 只用于比对，不进入提交体。
function resetProvDraft(c) {
  provDraft = loadProvDraft(c);
  provDraft.forEach((p) => { p.base = provSig(p); });
  provOpenKey = null; provTest = {}; provModelOpen = {};
}

// pane 可选＝「地址」：打开设置并落在该分区，两端同义——桌面双栏显示该分区
// 并高亮菜单项，手机直接进二级页面（navGoto）；缺省＝未选中态（菜单无高亮，
// 二级区显示默认常规页，手机从一级菜单起步）
async function openSettings(pane) {
  const c = await api('/api/config');
  presetsCache = c.presets || [];
  provNew = null;
  resetProvDraft(c);
  renderProvCards();
  selRefs.compactMode.set(c.compactMode === 'llm' ? 'llm' : 'rule');
  $('set-keepchars').value = c.keepChars || 100000; // 10 万字 ≈ 7.5 万 token
  $('set-compact-interval').value = c.compactInterval != null ? Math.round(c.compactInterval / 1000) : 400;
  selRefs.compactUnit.set(c.compactUnit === 'tokens' ? 'tokens' : 'chars');
  $('set-compact-llmpct').value = c.compactLlmPct != null ? c.compactLlmPct : 80;
  // 常规页：文件备份（服务端配置，改动即时保存）
  $('set-snapshot-on').checked = c.snapshotEnabled !== false;
  syncSnapshotState(c.snapshotEnabled !== false);
  $('set-snapshot-days').value = c.snapshotDays != null ? c.snapshotDays : 7;
  // 常规页：会话浏览器数据保留天数（服务端配置，改动即时保存；缺省 3 天，0=不清理）
  $('set-browserprofile-days').value = c.browserProfileDays != null ? c.browserProfileDays : 3;
  // 常规页：询问提醒（服务端配置，改动即时保存；三键嵌套部分提交）
  const aa = c.askAlert || {};
  $('set-askalert-on').checked = aa.enabled !== false;
  $('set-askalert-sec').value = aa.afterSec || 240;
  // 发送页：排队消息自动续发（服务端配置，改动即时保存；缺省＝开）
  $('set-queueauto-on').checked = c.queueAutoSend !== false;
  $('set-askalert-text').value = aa.text || '';
  // 常规页：任务最大步数（服务端配置，改动即时保存；0=不限制，是新装默认）
  syncMaxSteps(c.maxSteps);
  // 常规页：关闭窗口驻留后台（服务端配置，改动即时保存，缺省＝开；仅桌面版显示）
  $('set-tray-on').checked = c.closeToTray !== false;
  // 常规页：局域网访问（服务端配置，写 bindHost，重启程序后生效）
  $('set-lan-on').checked = c.bindHost === '0.0.0.0' || c.bindHost === '::';
  syncLanState(c);
  // 常规页：远程接入（服务端配置，写 remoteAccess.enabled，即时生效）
  $('set-remote-on').checked = !!(c.remoteAccess && c.remoteAccess.enabled);
  syncRemoteState(c);
  // 网络访问页：公司接入开关（服务端配置 companyLinkOff：关＝不弹向导、不连公司服务端）
  $('set-company-on').checked = !c.companyLinkOff;
  syncCompanyState(c);
  // 关于页：软件更新分区（版本/最近检查/就绪状态；更新检查源仍在网络访问页）
  refreshUpdateBox();
  // 常规页：本机浏览器偏好（即时生效，打开时回显当前值）
  selRefs.lang.set(lsGet('jx.lang', 'zh'));
  selRefs.fontsize.set(lsGet('jx.fontpx', String(FONT_PX_DEFAULT)));
  syncThemeCards();
  selRefs.sendkey.set(lsGet('jx.sendkey', 'enter'));
  // 默认「等待排队」：未设置过（新装）时下拉选框显示等待，用户选过就按用户存的显示
  selRefs.midrun.set(lsGet('jx.midrun', 'queue'));
  renderPluginList();
  mountPluginSections();
  renderComponentList(true); // 打开设置页＝组件清单唯一的重扫时机（见 renderComponentList）
  mountComponentPanes(); // 声明了 pane 的组件（如语音）的独立设置页
  renderHotkeysPane(); // 快捷键总表（含组件经 JX.registerHotkey 登记的条目）
  renderSkillsPanel();
  bindSkillDropZone();
  renderMCPPage(); // 独立「MCP 连接器」分区（app-mcp.js）
  renderExpertsPanel();
  refreshAssistantVersions(); // 对话历史与压缩：版本下拉（当前第几版 / 各版消息数与最后更新）
  const target = pane && document.querySelector('.set-nav-item[data-pane="' + pane + '"]') ? pane : null;
  // 手机端：进设置即收起侧栏浮层（返回后落在对话区），并压一层「设置一级」
  // 底座历史——直达分区时 navGoto 在其上补第二层，返回手势得以逐级退
  narrowClosePanels();
  if (isNarrow() && setHistDepth === 0) {
    setHistDepth = 1;
    setHistPush();
  }
  setNav.open = true;
  if (target) navGoto(target); // 同一个地址两端同义：直达该分区（选中并渲染）
  else {                        // 泛开：未选中态——二级区显示默认页，手机停一级菜单
    setNav.pane = 'general';
    setNav.level = 1;
    renderNav();
  }
  bindFeedbackForm();
  // 关于页信息：只显示版本号（模型名/连接状态等展示已按需求移除）
  try {
    const st = await api('/api/status');
    $('about-version').textContent = '版本 ' + (st.version || '-');
    } catch (e) { /* 状态取不到时保留页面默认版本文本 */ }
  loadAboutShell();
}

/* ---------- 关于页：远程外壳版本 + 一键清远程端缓存 ----------
   版本 = 外壳内容哈希.代数（/api/shell-hash）。按钮 POST /api/shell-bump 把代数
   +1：旧外壳里的嵌入版本随之全部「不一致」，remote-boot 自愈（清手机端缓存并
   整页重拉）在它们的下一次加载时触发——见 remoteaccess.go handleShellBump。 */
function fmtShellVer(h) {
  // 显示带代数（哈希前 8 位 + .N）：按钮按下后版本行能看到变化
  const i = h.lastIndexOf('.');
  return i > 0 ? h.slice(0, 8) + '…' + h.slice(i) : h.slice(0, 12) + '…';
}
async function loadAboutShell() {
  const ver = $('about-shell-ver');
  try {
    const d = await api('/api/shell-hash');
    ver.textContent = d && d.hash ? '外壳版本 ' + fmtShellVer(d.hash) : '外壳版本未知';
  } catch (e) { ver.textContent = '外壳版本读取失败'; }
  const btn = $('btn-shell-bump');
  if (!btn || btn.dataset.bound) return;
  btn.dataset.bound = '1';
  btn.onclick = async () => {
    btn.disabled = true;
    const done = $('about-shell-done');
    try {
      const d = await api('/api/shell-bump', { method: 'POST' });
      if (d && d.hash) ver.textContent = '外壳版本 ' + fmtShellVer(d.hash);
      done.style.display = '';
      done.textContent = '已生效：远程端下次打开自动清缓存重载';
    } catch (e) {
      done.style.display = '';
      done.textContent = '失败：' + e.message;
    }
    btn.disabled = false;
  };
}

/* ---------- 组件清单（/api/components + CX 运行时热开关）
   两档归属同一页分两区（与「技能」页同款排版）：
   - 内置组件：随包分发、不可删除，只有开关；升级随程序静默更新；
   - 用户组件：市场/发行渠道单个安装到 组件/user/，可开关、可删除。 ---------- */

async function renderComponentList(rescan) {
  const bBox = $('component-builtin-list');
  const uBox = $('component-user-list');
  if (!bBox || !uBox) return;
  bBox.innerHTML = '加载中…';
  uBox.innerHTML = '';
  let list = [];
  // 重扫时机只有一个：打开设置页（组件页）那一次——平时清单接口是纯读，
  // 不扫目录；这里带 rescan=1 让服务端重扫 组件/ 与 组件/user/，运行中新放进
  // 或删掉的组件打开设置页即见（桌面版单实例常驻，重开窗口≠重启服务）。
  const url = rescan ? '/api/components?rescan=1' : '/api/components';
  try { list = (await api(url)).components || []; } catch (e) { bBox.textContent = '组件清单读取失败：' + e.message; return; }
  bBox.innerHTML = '';

  for (const c of list) {
    const builtin = !!c.builtin;
    const row = document.createElement('div');
    row.className = 'plugin-row';
    const info = document.createElement('div');
    info.className = 'pr-info';
    info.innerHTML =
      `<div class="pr-line"><b>${esc(c.name)}</b><span class="pr-ver">${esc(c.version || '')}</span></div>` +
      `<p>${esc(c.description || '')}</p>`;
    row.appendChild(info);
    const tg = document.createElement('label');
    tg.className = 'pr-toggle';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = !!c.enabled;
    cb.onchange = async () => {
      cb.disabled = true;
      const note = row.querySelector('.pr-note');
      try {
        const r = await CX.toggle(c.id, cb.checked);
        if (note) note.textContent = r.hot ? (cb.checked ? '已即时启用' : '已即时停用') : '该组件不支持热关闭，刷新页面后完全生效';
      } catch (e) {
        alert('切换失败：' + e.message);
        cb.checked = !cb.checked;
      }
      cb.disabled = false;
      setTimeout(() => { if (note) note.textContent = ''; }, 3000);
    };
    tg.appendChild(cb);
    tg.appendChild(document.createElement('span'));
    row.appendChild(tg);
    if (!builtin) { // 用户组件才有删除入口（内置的随包分发，只可开关）
      const del = document.createElement('button');
      del.className = 'skill-del';
      del.title = '删除该组件';
      del.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M10 11v6"/><path d="M14 11v6"/></svg>';
      del.onclick = async () => {
        if (!(await confirmCenter('删除组件「' + (c.name || c.id) + '」？组件目录（组件/user/' + c.id + '）将被一并移除。', false, true))) return;
        try {
          await api('/api/components/delete', { method: 'POST', body: JSON.stringify({ id: c.id }) });
          await CX.refresh(); // 已装载的立即热卸载
          renderComponentList();
        } catch (e) { alert('删除失败：' + e.message); }
      };
      row.appendChild(del);
    }
    const note = document.createElement('div');
    note.className = 'pr-note';
    row.appendChild(note);
    (builtin ? bBox : uBox).appendChild(row);
  }
  if (!list.some((c) => c.builtin)) bBox.innerHTML = '<div class="pr-note">没有发现内置组件（组件/ 下无 manifest.json）</div>';
  if (!list.some((c) => !c.builtin)) {
    uBox.innerHTML = '<div class="skill-empty">还没有用户组件。在市场里一键安装，或把组件目录放进 组件/user/ 即装即用。</div>';
  }
  mountComponentSections(); // 组件自己的设置区（组件经 CX.registerSettings 交出）
}

/* ---------- 组件设置区（CX.registerSettings；组件关掉时它那段随之消失） ---------- */

function mountComponentSections() {
  const box = $('component-sections');
  if (!box) return;
  box.innerHTML = '';
  for (const sec of CX.settingsSections()) {
    if (sec.pane) continue; // 声明了 pane 的组件走独立分区页（见 mountComponentPanes）
    const wrap = document.createElement('div');
    wrap.className = 'prov-section jx-section';
    const h = document.createElement('h4');
    h.textContent = sec.title;
    wrap.appendChild(h);
    const body = document.createElement('div');
    wrap.appendChild(body);
    try { sec.mount(body); } catch (e) { body.textContent = '组件设置加载失败：' + e.message; }
    box.appendChild(wrap);
  }
}

/* ---------- 组件设置页（CX.registerSettings 且带 pane 字段的组件 → 独立分区页）
   组件在注册时声明 pane:{label, icon}，设置页左侧一级菜单出现专属入口（插在
   「组件」之后）、右侧出现专属分区页；组件停用/卸载时导航项与页面一并撤掉，
   正停在该页时退回默认页。导航状态机（setNav/renderNav/navGoto）按 data-pane
   寻址，动态项与静态项同一条路径，窄屏翻页/返回手势天然兼容。 */
function syncComponentPanePresence() {
  const nav = document.querySelector('.set-nav');
  const panes = document.querySelector('.settings-body .set-panes') || document.querySelector('.settings-body');
  if (!nav || !panes) return;
  const defs = CX.settingsSections().filter((s) => s.pane && s.pane.label);
  const live = new Set(defs.map((d) => d.id));
  // 撤掉失活组件的导航项与页面
  document.querySelectorAll('[data-cx-pane]').forEach((el) => {
    const id = el.dataset.cxPane;
    if (live.has(id)) return;
    const paneName = 'component-' + id;
    if (setNav.pane === paneName) { // 正停在被撤的页：退回默认页
      setNav.pane = 'general';
      renderNav();
    }
    el.remove();
  });
  for (const d of defs) {
    const paneName = 'component-' + d.id;
    if (nav.querySelector('.set-nav-item[data-pane="' + paneName + '"]')) continue;
    const navBtn = document.createElement('button');
    navBtn.type = 'button';
    navBtn.className = 'set-nav-item';
    navBtn.dataset.pane = paneName;
    navBtn.dataset.cxPane = d.id;
    navBtn.innerHTML = (d.pane.icon || '') + esc(d.pane.label);
    navBtn.onclick = () => navGoto(paneName);
    // 插在「组件」之后；没有该入口（页面结构变动）就退到「关于」之前
    const anchor = nav.querySelector('.set-nav-item[data-pane="components"]') ||
      nav.querySelector('.set-nav-item[data-pane="about"]');
    if (anchor && anchor.nextSibling) nav.insertBefore(navBtn, anchor.nextSibling);
    else nav.appendChild(navBtn);
    const paneEl = document.createElement('div');
    paneEl.className = 'set-pane';
    paneEl.id = 'set-pane-' + paneName;
    paneEl.dataset.cxPane = d.id;
    const anchorPane = document.getElementById('set-pane-components');
    if (anchorPane && anchorPane.parentNode === panes && anchorPane.nextSibling) panes.insertBefore(paneEl, anchorPane.nextSibling);
    else panes.appendChild(paneEl);
  }
}

// 内容挂载（设置弹窗打开时与热开关时）：标题 + 组件交出的设置区
function mountComponentPanes() {
  syncComponentPanePresence();
  for (const d of CX.settingsSections()) {
    if (!d.pane || !d.pane.label) continue;
    const paneEl = document.getElementById('set-pane-component-' + d.id);
    if (!paneEl) continue;
    paneEl.innerHTML = '';
    const h = document.createElement('h3');
    h.textContent = d.title;
    paneEl.appendChild(h);
    const inner = document.createElement('div');
    paneEl.appendChild(inner);
    try { d.mount(inner); } catch (e) { inner.textContent = '组件设置加载失败：' + e.message; }
  }
}

// 组件激活状态或设置区有变（注册 / 设置页热开关）→ 重挂；设置弹窗没开就只同步
// 导航项在位情况（内容等下次打开再挂，免得后台空跑组件自己的请求）
window.addEventListener('cx:change', () => {
  const set = $('settings');
  if (set && !set.classList.contains('hidden')) {
    mountComponentSections();
    mountComponentPanes();
    renderHotkeysPane(); // 组件带上/撤下自己的快捷键（如语音）→ 快捷键页跟着变
  } else {
    syncComponentPanePresence();
  }
});

/* ---------- 快捷键总表（设置 · 快捷键）：数据与渲染都在 app-hotkeys.js 的 HK，
   这里只管挂载时机——打开设置、组件注册变动、任一处快捷键被改（hotkeys:change）
   都重挂；语音页/常规页与这张表改的是同一份数据，天然同步 ---------- */
function renderHotkeysPane() {
  const host = $('hotkeys-table');
  if (host && window.HK) HK.renderTable(host);
}
JX.on('hotkeys:change', () => {
  const set = $('settings');
  if (set && !set.classList.contains('hidden') && setNav.pane === 'hotkeys') renderHotkeysPane();
});

/* ---------- 插件清单与分区（统一插件页：/api/plugins 带 layers + JX 注册的分区） ---------- */

// 各体系行的富渲染器（按层 id 注册，app-ccplugins.js 等提供）；缺省通用行。
// opts.session（会话id）＝按对话开关浮窗的会话模式：总开关写会话级覆盖而不是全局。
const pluginRowRenderers = { claude: ccpPluginRow };

function pluginRowFor(p, opts) {
  return (pluginRowRenderers[p.layer] || simplePluginRow)(p, opts);
}

// 插件行/组件行通用的「已覆盖」徽标：按 overridden 显示或摘除（点击恢复跟随全局）
function syncPluginOverrideBadge(row, overridden) {
  let ovr = row.querySelector('.pr-ovr');
  if (!overridden) { if (ovr) ovr.remove(); return; }
  if (ovr) return;
  ovr = document.createElement('span');
  ovr.className = 'pr-ovr';
  ovr.title = '本对话覆盖了全局开关';
  ovr.textContent = '已覆盖';
  const line = row.querySelector('.pr-line');
  if (line) line.appendChild(ovr);
}

// 通用行（DSH 等纯扫描体系）：未适配原件灰显、开关禁用
function simplePluginRow(p, opts) {
  const row = document.createElement('div');
  row.className = 'plugin-row' + (p.adapted === false ? ' unadapted' : '');
  const info = document.createElement('div');
  info.className = 'pr-info';
  info.innerHTML =
    `<div class="pr-line"><b>${esc(p.name)}</b><span class="pr-ver">${esc(p.version || '')}</span>` +
    (p.adapted === false ? '<span class="pr-badge">未适配</span>' : '') +
    (p.override ? '<span class="pr-ovr" title="本对话覆盖了全局开关（点击恢复跟随全局）">已覆盖</span>' : '') + '</div>' +
    `<p>${esc(p.description || '')}</p>`;
  const tg = document.createElement('label');
  tg.className = 'pr-toggle';
  const cb = document.createElement('input');
  cb.type = 'checkbox';
  cb.checked = !!p.enabled;
  cb.disabled = p.adapted === false;
  cb.onchange = async () => {
    cb.disabled = true;
    try {
      const body = { layer: p.layer, id: p.id, enabled: cb.checked };
      if (opts && opts.session) body.session = opts.session;
      const r = await api('/api/plugins', { method: 'POST', body: JSON.stringify(body) });
      // 会话模式：把定制标记带回来（加号菜单的 ✓ 读它）
      if (opts && opts.session && window.CX && r && typeof r.sessionCustom !== 'undefined') {
        CX.sessionCustom = !!r.sessionCustom;
      }
      row.querySelector('.pr-note').textContent = opts && opts.session
        ? (cb.checked ? '本对话已启用（其他对话不受影响）' : '本对话已停用（其他对话不受影响）')
        : '已更新；界面部分刷新页面后生效';
      // 覆盖徽标跟实际状态对齐：与全局一致＝已恢复跟随（徽标摘掉）
      if (opts && opts.session) syncPluginOverrideBadge(row, cb.checked !== p.globalEnabled);
    } catch (e) {
      alert('切换失败：' + e.message);
      cb.checked = !cb.checked;
    }
    cb.disabled = p.adapted === false;
  };
  tg.appendChild(cb);
  tg.appendChild(document.createElement('span'));
  row.appendChild(info);
  row.appendChild(tg);
  const note = document.createElement('div');
  note.className = 'pr-note';
  row.appendChild(note);
  return row;
}

// 统一插件页主体：拉 layers + plugins，按来源体系分组渲染；
// 暴露了市场清单的层（当前 Claude 层）顺带挂市场浏览面板
async function renderPluginList() {
  const box = $('plugin-list');
  if (!box) return;
  box.innerHTML = '加载中…';
  let data;
  try { data = await api('/api/plugins'); } catch (e) { box.textContent = '插件清单读取失败：' + e.message; return; }
  let markets = [];
  for (const L of (data.layers || [])) {
    if (L.extra && Array.isArray(L.extra.marketplaces)) { markets = L.extra.marketplaces; break; }
  }
  renderCCMarketPanel(markets);
  const byLayer = {};
  for (const p of (data.plugins || [])) (byLayer[p.layer] = byLayer[p.layer] || []).push(p);
  box.innerHTML = '';
  for (const L of (data.layers || [])) {
    const group = byLayer[L.id] || [];
    delete byLayer[L.id];
    box.appendChild(pluginLayerSection(L, group));
  }
  // 兜底：清单里出现未注册层（不应发生）——按层名原样追加一组
  for (const [lid, group] of Object.entries(byLayer)) {
    box.appendChild(pluginLayerSection({ id: lid, name: group[0].layerName || lid }, group));
  }
}

function pluginLayerSection(L, group, opts) {
  const sec = document.createElement('div');
  sec.className = 'gen-section';
  const h = document.createElement('h4');
  h.innerHTML = `${esc(L.name)} <span class="sec-sub">${group.length ? group.length + ' 个' : '暂无'}</span>`;
  sec.appendChild(h);
  if (group.length) {
    for (const p of group) sec.appendChild(pluginRowFor(p, opts));
  } else {
    const empty = document.createElement('div');
    empty.className = 'pr-note';
    empty.textContent = '该体系下还没有插件';
    sec.appendChild(empty);
  }
  return sec;
}

function mountPluginSections() {
  const box = $('plugin-sections');
  if (!box) return;
  box.innerHTML = '';
  for (const sec of JX._settingsSections) {
    const wrap = document.createElement('div');
    wrap.className = 'prov-section jx-section';
    const h = document.createElement('h4');
    h.textContent = sec.title;
    wrap.appendChild(h);
    const body = document.createElement('div');
    wrap.appendChild(body);
    try { sec.mount(body); } catch (e) { body.textContent = '插件设置加载失败：' + e.message; }
    box.appendChild(wrap);
  }
}

/* ---------- 模型页：提供方卡片（点击展开即全量编辑，每卡单独保存） ---------- */

function presetOf(id) { return presetsCache.find((p) => p.id === id) || null; }

function fmtTok(n) {
  if (!n || n <= 0) return '';
  if (n >= 1048576) return (Math.round(n / 1048576 * 10) / 10).toString().replace(/\.0$/, '') + 'M';
  return Math.round(n / 1024) + 'K';
}

// 卡片当前生效的 API 地址（预置 = 系统地址，自定义 = 表单里的）
function effURL(p) {
  if (p.presetId) {
    const t = presetOf(p.presetId);
    return (t && t.baseURL) || p.presetURL || p.baseURL;
  }
  return p.baseURL;
}

async function reloadProviders() {
  const c = await api('/api/config');
  resetProvDraft(c);
  // 不清 provNew：「添加提供方」的草稿卡片由保存/取消按钮显式处理，避免重载时误丢输入
  renderProvCards();
  loadModels();
}

function renderProvCards() {
  const box = $('prov-list');
  if (!box) return;
  box.innerHTML = '';
  // 新装（未接公司通道、也没自己添加）时列表是空的：给一句话说清下一步，
  // 免得以为界面上漏了东西
  if (!provDraft.length && !provNew) {
    box.innerHTML = '<div class="skill-empty">还没有模型通道。点下方「添加提供方」自己接一个，或用「公司接入向导」一键接入公司通道。</div>';
  }
  provDraft.forEach((p) => box.appendChild(buildProvCard(p, p.id)));
  // 草稿卡片常驻列表（收起只是折叠）：收起/切换卡片都不丢已输入的内容
  if (provNew) box.appendChild(buildProvCard(provNew, 'new'));
  // 渲染完成通知（2026-11-13 充值组件）：卡片是全量重建的，组件往挂载点
  // （.pf-card-slot）里放的按钮每次渲染都会随之消失——发一个事件让组件重新挂。
  // 与 run:error / wizard:done 同为「主程序不认识组件、组件不碰主程序函数」的
  // 通用对接点（JX.on 同步通知，见 app-core.js）。
  if (window.JX && typeof JX.emit === 'function') JX.emit('prov:render');
}

function buildProvCard(p, key) {
  const open = provOpenKey === key;
  const card = document.createElement('div');
  card.className = 'prov-card' + (open ? ' open' : '');

  // 折叠头：状态点 + 名称 + 模型数 + 启停开关 + 编辑/收起
  // 预置提供方的名称实时取预置模板（模板改名后已保存的卡片跟着更新，
  // 不用存量用户重存一遍）
  const tp = p.presetId ? presetOf(p.presetId) : null;
  const configured = !!(p.key || p.keyMasked);
  const offTag = p.autoDisabled
    ? '<span class="pv-off-tag" title="连不上公司服务端时自动停用，恢复可达自动启用">已自动停用 · 服务端不可达</span>'
    : (p.enabled === false ? '<span class="pv-off-tag">已停用</span>' : '');
  // 公司通道的线路徽标（配了外网地址才有）：外网兜底生效时明确显示「外网通道中」，
  // 不再让用户以为通道不可用；内网期间显示「内外网」提示具备双地址。
  const viaTag = !p.autoDisabled && p.publicBaseURL
    ? (p.via === 'wan'
        ? '<span class="pv-via-tag wan" title="内网地址连不上，已自动改走外网地址；回公司内网恢复后自动切回">外网通道中</span>'
        : '<span class="pv-via-tag" title="配有外网备用地址：内网连不上时自动切换，恢复后切回">内外网</span>')
    : '';
  // 公司通道：卡片头上直接显示额度（服务端随公告下发；展开后还有明细与「刷新额度」）
  let quotaChip = '';
  if (p.company && p.quota) {
    const q = p.quota;
    const fmt = (n) => (n || 0).toLocaleString('zh-CN');
    let txt = q.unlimited ? '额度不限量' : '剩余 ' + fmt(q.remainPoints) + ' 积分';
    if (q.status === 2) txt = '额度已用完';
    let tip = q.unlimited ? '公司通道额度不限量（只记账）'
      : '公司通道剩余 ' + fmt(q.remainPoints) + ' 积分（≈' + ((q.remainPoints || 0) / 1000).toFixed(2) + ' 元）· 已用 ' + fmt(q.usedPoints) + ' 积分';
    tip += ' · 规则：' + (q.periodText || '') + (q.points ? '，每次 ' + fmt(q.points) + ' 积分' : '');
    tip += q.nextAt ? ' · 下次重置 ' + q.nextAt.replace('T', ' ').slice(0, 16) : ' · 不自动重置';
    quotaChip = '<span class="pv-quota" title="' + esc(tip) + '">' + esc(txt) + '</span>';
  }
  const head = document.createElement('div');
  head.className = 'pv-head' + (p.enabled === false || p.autoDisabled ? ' off' : '');
  head.innerHTML =
    `<span class="pv-dot ${configured ? 'ok' : 'no'}" title="${configured ? '密钥已配置' : '未配置密钥'}"></span>` +
    `<b class="pv-name">${esc((tp && tp.name) || p.name || '未命名提供方')}</b>` +
    offTag + viaTag + quotaChip +
    `<span class="pv-meta">${p.models.length} 个模型</span>` +
    (p.key ? '<span class="pv-dirty">密钥未保存</span>' : '');
  // 启停开关：停用的提供方不参与默认选择与会话解析（出了公司自动切到可用服务商
  // 就靠它）；公司通道另有「自动停用」，手动开启会一并清掉
  if (key !== 'new') {
    const tg = document.createElement('label');
    tg.className = 'pr-toggle pv-toggle';
    tg.title = p.autoDisabled ? '已因连不上公司服务端自动停用；手动开启立即生效（恢复可达也会自动启用）'
      : (p.enabled === false ? '已手动停用：不参与模型选择' : '停用后该服务商不参与模型选择');
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = p.enabled !== false && !p.autoDisabled;
    cb.onclick = (e) => e.stopPropagation();
    cb.onchange = async () => {
      const on = cb.checked;
      try {
        await api('/api/providers/toggle', { method: 'POST', body: JSON.stringify({ id: p.id, enabled: on }) });
        await reloadProviders();
      } catch (e) { alert((on ? '启用' : '停用') + '失败：' + e.message); reloadProviders(); }
    };
    tg.appendChild(cb);
    tg.appendChild(document.createElement('span'));
    tg.onclick = (e) => e.stopPropagation();
    head.appendChild(tg);
  }
  const edit = document.createElement('span');
  edit.className = 'pv-edit';
  edit.textContent = open ? '收起' : '编辑';
  head.appendChild(edit);
  head.onclick = () => {
    // 收起/展开只切换视图，不重载服务器数据：已输入的密钥、模型修改都留在草稿里，
    // 只有「取消」「保存」才丢弃或提交（修复：随便点哪，输入的内容就消失）
    if (open) {
      provOpenKey = null;
    } else {
      provOpenKey = key;
      if (provModelOpen[key] === undefined) provModelOpen[key] = -1;
    }
    renderProvCards();
  };
  card.appendChild(head);
  if (!open) return card;

  const form = document.createElement('div');
  form.className = 'pv-form';

  // 提供方选择：预置清单 + 自定义
  const lblProv = document.createElement('div');
  lblProv.className = 'pf-label';
  lblProv.textContent = '提供方';
  form.appendChild(lblProv);
  const sel = document.createElement('select');
  sel.className = 'pf-input';
  for (const t of presetsCache) {
    const o = document.createElement('option');
    o.value = t.id; o.textContent = t.name;
    sel.appendChild(o);
  }
  const oCustom = document.createElement('option');
  oCustom.value = '__custom'; oCustom.textContent = '自定义提供方';
  sel.appendChild(oCustom);
  sel.value = p.presetId || '__custom';
  sel.onchange = () => {
    if (sel.value === '__custom') {
      p.presetId = null;
    } else {
      p.presetId = sel.value;
      const t = presetOf(sel.value);
      if (t) { p.name = t.name; p.baseURL = ''; p.presetURL = t.baseURL; p.protocol = t.protocol || ''; }
    }
    renderProvCards();
  };
  form.appendChild(sel);

  // 协议类型：决定同一套 agent 走哪种渠道协议（后端 llmproto.go 的三个实现）。
  // 预置方跟随模板不可改；自定义渠道三选一，默认 OpenAI 兼容。
  const PROTOCOLS = [
    ['', 'OpenAI 兼容 · chat/completions（默认）'],
    ['anthropic', 'Anthropic 原生 · /v1/messages'],
    ['responses', 'OpenAI Responses · /v1/responses'],
  ];
  const protoLabel = (v) => { const hit = PROTOCOLS.find((x) => x[0] === v); return hit ? hit[1] : v; };
  if (p.presetId) {
    if (p.protocol) {
      const lblProto = document.createElement('div');
      lblProto.className = 'pf-label';
      lblProto.textContent = '协议类型';
      form.appendChild(lblProto);
      const protoFixed = document.createElement('div');
      protoFixed.className = 'pf-fixed';
      protoFixed.textContent = protoLabel(p.protocol) + '（预置提供方固定）';
      form.appendChild(protoFixed);
    }
  } else {
    const lblProto = document.createElement('div');
    lblProto.className = 'pf-label';
    lblProto.textContent = '协议类型';
    form.appendChild(lblProto);
    const selProto = document.createElement('select');
    selProto.className = 'pf-input';
    for (const [v, label] of PROTOCOLS) {
      const o = document.createElement('option');
      o.value = v; o.textContent = label;
      selProto.appendChild(o);
    }
    selProto.value = p.protocol || '';
    selProto.onchange = () => { p.protocol = selProto.value; renderProvCards(); };
    form.appendChild(selProto);
  }

  // 名称 + API 地址（预置方用系统地址，只展示不可改；备注展示计费方式/Key 获取方式）
  if (p.presetId) {
    const lblUrl = document.createElement('div');
    lblUrl.className = 'pf-label';
    lblUrl.textContent = 'API 地址';
    form.appendChild(lblUrl);
    const fixed = document.createElement('div');
    fixed.className = 'pf-fixed';
    fixed.textContent = '使用系统预设地址：' + effURL(p);
    form.appendChild(fixed);
    const note = (tp && tp.note) || '';
    if (note) {
      const fixedNote = document.createElement('div');
      fixedNote.className = 'pf-note';
      fixedNote.textContent = note;
      form.appendChild(fixedNote);
    }
  } else {
    const two = document.createElement('div');
    two.className = 'pf-two';
    const cellName = document.createElement('div');
    cellName.innerHTML = '<div class="pf-label">名称</div>';
    const inName = document.createElement('input');
    inName.className = 'pf-input';
    inName.placeholder = '如 公司内部网关';
    inName.value = p.name;
    inName.oninput = () => { p.name = inName.value; };
    cellName.appendChild(inName);
    const cellUrl = document.createElement('div');
    cellUrl.innerHTML = '<div class="pf-label">API 地址（' + (p.protocol === 'anthropic' ? 'Anthropic，含 /v1' : p.protocol === 'responses' ? 'Responses，含 /v1' : 'OpenAI 兼容') + '）</div>';
    const inUrl = document.createElement('input');
    inUrl.className = 'pf-input';
    inUrl.placeholder = p.protocol === 'anthropic' ? 'https://api.anthropic.com/v1' : 'https://…/v1';
    inUrl.value = p.baseURL;
    inUrl.oninput = () => { p.baseURL = inUrl.value; };
    cellUrl.appendChild(inUrl);
    two.appendChild(cellName);
    two.appendChild(cellUrl);
    form.appendChild(two);
  }

  // 公司通道的外网备用地址（服务端公告下发，只读展示；一套 key、一套模型配置，
  // 内外网自动切换——见 llm.go StreamChat 的故障转移）
  if (p.publicBaseURL) {
    const lblPub = document.createElement('div');
    lblPub.className = 'pf-label';
    lblPub.textContent = '外网地址（内外网自动切换）';
    form.appendChild(lblPub);
    const pubFixed = document.createElement('div');
    pubFixed.className = 'pf-fixed';
    pubFixed.textContent = p.publicBaseURL + (p.via === 'wan' ? '（当前走外网）' : '（当前走内网）');
    form.appendChild(pubFixed);
    const pubNote = document.createElement('div');
    pubNote.className = 'pf-note';
    pubNote.textContent = '由公司服务端统一下发，无需手动切换：内网地址连不上时自动改走此地址，回公司内网恢复后自动切回。';
    form.appendChild(pubNote);
  }

  // 公司通道的额度（服务端随公告下发，只读；刷新 = 让后端重拉一份公告）
  if (p.company && p.quota) {
    const q = p.quota;
    const lblQ = document.createElement('div');
    lblQ.className = 'pf-label';
    lblQ.textContent = '额度';
    form.appendChild(lblQ);
    const box = document.createElement('div');
    box.className = 'pf-fixed';
    const fmt = (n) => (n || 0).toLocaleString('zh-CN');
    let line = '';
    if (q.unlimited) {
      line = '不限量（只记账）';
    } else {
      line = '剩余 ' + fmt(q.remainPoints) + ' 积分（≈' + ((q.remainPoints || 0) / 1000).toFixed(2) + ' 元）';
      if (q.usedPoints) line += ' · 已用 ' + fmt(q.usedPoints) + ' 积分';
    }
    if (q.status === 2) line += ' · 令牌已被中转站停用（额度可能已用完）';
    box.textContent = line;
    form.appendChild(box);
    const qNote = document.createElement('div');
    qNote.className = 'pf-note';
    let rule = '规则：' + (q.periodText || '') + (q.points ? '，每次 ' + fmt(q.points) + ' 积分' : '');
    if (q.nextAt) rule += ' · 下次重置 ' + q.nextAt.replace('T', ' ').slice(0, 16);
    else rule += ' · 不自动重置';
    if (q.asOf) rule += ' · 更新于 ' + q.asOf.replace('T', ' ').slice(11, 16);
    qNote.textContent = rule + '（由公司服务端与中转站结算，这里只读）';
    form.appendChild(qNote);
    const refresh = document.createElement('button');
    refresh.className = 'pf-cancel';
    refresh.textContent = '刷新额度';
    refresh.onclick = async (e) => {
      e.stopPropagation();
      refresh.disabled = true; refresh.textContent = '刷新中…';
      try {
        await api('/api/company-sync', { method: 'POST' });
        await reloadProviders();
      } catch (err) {
        alert('刷新失败：' + err.message);
        refresh.disabled = false; refresh.textContent = '刷新额度';
      }
    };
    form.appendChild(refresh);
  }

  // 组件插槽（2026-11-13 充值组件）：公司通道卡片的按钮挂载点。充值组件监听
  // prov:render 事件后往 .pf-card-slot 里放「充值」按钮（挂载点 dataset 带本卡
  // portalURL，空则组件不放，空容器不占位）。这是通用机制：任何组件都能在此
  // 贡献按钮，主程序只提供挂载点与渲染完成事件，不认识具体组件。
  if (p.company) {
    const slot = document.createElement('span');
    slot.className = 'pf-card-slot';
    slot.dataset.cxSlot = 'prov-actions';
    slot.dataset.provId = p.id || '';
    slot.dataset.portalURL = p.portalURL || '';
    form.appendChild(slot);
  }

  // API 密钥：绝不回显，只标「已配置」，输入可替换
  const lblKey = document.createElement('div');
  lblKey.className = 'pf-label';
  lblKey.textContent = 'API 密钥';
  form.appendChild(lblKey);
  const keyRow = document.createElement('div');
  keyRow.className = 'key-row';
  if (p.keyMasked) {
    const chip = document.createElement('span');
    chip.className = 'key-chip';
    chip.textContent = '已配置';
    keyRow.appendChild(chip);
  }
  const inKey = document.createElement('input');
  inKey.className = 'pf-input';
  inKey.type = 'password';
  inKey.autocomplete = 'new-password';
  inKey.placeholder = p.keyMasked ? '留空保持不变，输入可替换' : (p.presetId === 'ollama' ? '本地服务可留空' : '必填，如 sk-…');
  inKey.value = p.key || ''; // 重渲染回填已输入但未保存的密钥，避免视觉上「消失」
  inKey.oninput = () => { p.key = inKey.value; };
  keyRow.appendChild(inKey);
  form.appendChild(keyRow);

  // 模型目录
  const cat = document.createElement('div');
  cat.className = 'model-cat';
  const catHead = document.createElement('div');
  catHead.className = 'mc-head';
  catHead.innerHTML = '<span class="mc-title">模型目录</span>';
  const btnTest = document.createElement('button');
  btnTest.className = 'mc-btn';
  btnTest.textContent = '测试连通';
  const btnFetch = document.createElement('button');
  btnFetch.className = 'mc-btn';
  btnFetch.textContent = '拉取模型';
  catHead.appendChild(btnTest);
  catHead.appendChild(btnFetch);
  cat.appendChild(catHead);

  p.models.forEach((m, mi) => {
    const row = document.createElement('div');
    row.className = 'model-row';
    const line = document.createElement('div');
    line.className = 'mr-line';
    const chips = [
      m.contextWindow ? '上下文 ' + fmtTok(m.contextWindow) : '上下文未知',
      m.maxOutput ? '输出 ' + fmtTok(m.maxOutput) : null,
      m.supportsImages ? '图片 ✓' : null,
    ].filter(Boolean).join(' · ');
    line.innerHTML =
      `<span class="mr-id">${esc(m.id || '(未命名模型)')}</span>` +
      `<span class="mr-test"></span>` +
      `<span class="mr-chips">${esc(chips)}</span>` +
      `<button class="mr-x" title="删除该模型">删除</button>`;
    const testEl = line.querySelector('.mr-test');
    const t = (provTest[key] || {})[m.id];
    if (t && t.state === 'run') { testEl.textContent = '测试中…'; testEl.className = 'mr-test run'; }
    else if (t && t.state === 'ok') { testEl.textContent = '✓ ' + t.ms + 'ms'; testEl.className = 'mr-test ok'; }
    else if (t && t.state === 'fail') { testEl.textContent = '✗ ' + t.err; testEl.title = t.err; testEl.className = 'mr-test fail'; }
    line.querySelector('.mr-x').onclick = (e) => {
      e.stopPropagation();
      p.models.splice(mi, 1);
      renderProvCards();
    };
    line.onclick = () => {
      provModelOpen[key] = provModelOpen[key] === mi ? -1 : mi;
      renderProvCards();
    };
    row.appendChild(line);
    if (provModelOpen[key] === mi) {
      const edit = document.createElement('div');
      edit.className = 'mr-edit';
      const mk = (labelText, inputEl, full) => {
        const cell = document.createElement('div');
        if (full) cell.className = 'full';
        const l = document.createElement('div');
        l.className = 'pf-label';
        l.textContent = labelText;
        cell.appendChild(l);
        cell.appendChild(inputEl);
        return cell;
      };
      const inMid = document.createElement('input');
      inMid.className = 'pf-input';
      inMid.placeholder = '模型 ID，如 deepseek-chat';
      inMid.value = m.id;
      inMid.oninput = () => { m.id = inMid.value; };
      edit.appendChild(mk('模型 ID', inMid));
      const inMname = document.createElement('input');
      inMname.className = 'pf-input';
      inMname.placeholder = '选填，显示名称';
      inMname.value = m.name;
      inMname.oninput = () => { m.name = inMname.value; };
      edit.appendChild(mk('显示名称', inMname));
      const inCtx = document.createElement('input');
      inCtx.className = 'pf-input';
      inCtx.type = 'number';
      inCtx.min = '0';
      inCtx.placeholder = '默认 262144';
      inCtx.value = m.contextWindow || '';
      inCtx.oninput = () => { m.contextWindow = parseInt(inCtx.value, 10) || 0; };
      edit.appendChild(mk('上下文窗口（tokens）', inCtx));
      const inMax = document.createElement('input');
      inMax.className = 'pf-input';
      inMax.type = 'number';
      inMax.min = '0';
      inMax.placeholder = '不限';
      inMax.value = m.maxOutput || '';
      inMax.oninput = () => { m.maxOutput = parseInt(inMax.value, 10) || 0; };
      edit.appendChild(mk('最大输出（tokens）', inMax));
      // 支持图片：苹果式开关
      const tg = document.createElement('label');
      tg.className = 'pr-toggle';
      const cb = document.createElement('input');
      cb.type = 'checkbox';
      cb.checked = !!m.supportsImages;
      cb.onchange = () => { m.supportsImages = cb.checked; };
      tg.appendChild(cb);
      tg.appendChild(document.createElement('span'));
      edit.appendChild(mk('是否支持图片', tg));
      const del = document.createElement('button');
      del.className = 'mr-del2';
      del.textContent = '删除该模型';
      del.onclick = (e) => { e.stopPropagation(); p.models.splice(mi, 1); renderProvCards(); };
      edit.appendChild(del);
      row.appendChild(edit);
    }
    cat.appendChild(row);
  });

  const btnAddModel = document.createElement('button');
  btnAddModel.className = 'mc-add';
  btnAddModel.textContent = '＋ 添加模型';
  btnAddModel.onclick = () => {
    p.models.push({ id: '', name: '', contextWindow: 0, maxOutput: 0, supportsImages: false });
    provModelOpen[key] = p.models.length - 1;
    renderProvCards();
    const inputs = $('prov-list').querySelectorAll('.mr-edit input');
    if (inputs.length) inputs[inputs.length - 5 >= 0 ? inputs.length - 5 : 0].focus();
  };
  cat.appendChild(btnAddModel);
  form.appendChild(cat);

  // 拉取模型：合并策略 —— 已有条目保留手工改过的元信息，新条目用后端猜的
  btnFetch.onclick = async () => {
    btnFetch.disabled = true; btnFetch.textContent = '拉取中…';
    try {
      const d = await api('/api/providers/fetch', {
        method: 'POST',
        body: JSON.stringify({ id: p.id || p.presetId || '', baseURL: effURL(p), apiKey: p.key, protocol: p.protocol || '' }),
      });
      const oldMap = new Map(p.models.map((m) => [m.id, m]));
      const merged = [];
      for (const fm of (d.models || [])) {
        const ex = oldMap.get(fm.id);
        if (ex) {
          oldMap.delete(fm.id);
          // 思考档位没有手工编辑入口：服务端报了真实 supported_levels 就以它为准
          if (fm.reasoningLevels && fm.reasoningLevels.length) ex.reasoningLevels = fm.reasoningLevels.slice();
          merged.push(ex);
        }
        else merged.push({
          id: fm.id, name: fm.name || '', contextWindow: fm.contextWindow || 0,
          maxOutput: fm.maxOutput || 0, supportsImages: !!fm.supportsImages,
          reasoningLevels: (fm.reasoningLevels && fm.reasoningLevels.length) ? fm.reasoningLevels.slice() : null,
        });
      }
      for (const ex of oldMap.values()) merged.push(ex); // 手工添加的保留在末尾
      p.models = merged;
      provModelOpen[key] = -1;
      renderProvCards();
    } catch (e) {
      alert('拉取失败：' + e.message);
      btnFetch.disabled = false; btnFetch.textContent = '拉取模型';
    }
  };

  // 测试连通：目录里的所有模型各发一个极小请求
  btnTest.onclick = async () => {
    const ids = p.models.map((m) => m.id).filter(Boolean);
    if (!ids.length) { alert('目录里还没有模型，先「拉取模型」或「添加模型」'); return; }
    provTest[key] = {};
    ids.forEach((id) => { provTest[key][id] = { state: 'run' }; });
    btnTest.disabled = true; btnTest.textContent = '测试中…';
    renderProvCards();
    try {
      const d = await api('/api/providers/test', {
        method: 'POST',
        body: JSON.stringify({ id: p.id || p.presetId || '', baseURL: effURL(p), apiKey: p.key, protocol: p.protocol || '', models: ids }),
      });
      provTest[key] = {};
      for (const r of (d.results || [])) provTest[key][r.id] = { state: r.ok ? 'ok' : 'fail', ms: r.ms, err: r.err || '失败' };
    } catch (e) {
      provTest[key] = {};
      ids.forEach((id) => { provTest[key][id] = { state: 'fail', err: e.message }; });
    }
    renderProvCards();
  };

  // 卡片操作区：删除 | 取消 | 保存
  const acts = document.createElement('div');
  acts.className = 'pf-actions';
  if (key !== 'new') {
    const delBtn = document.createElement('button');
    delBtn.className = 'pf-del';
    delBtn.textContent = '删除提供方';
    delBtn.onclick = async () => {
      const msg = p.company
        ? '删除公司通道「' + (p.name || p.id) + '」？\n重新连上公司服务端后会按你的姓名自动重建（含密钥）；长期不用请改用卡片上的停用开关。'
        : '删除提供方「' + (p.name || p.id) + '」？以后可重新添加。';
      if (!confirm(msg)) return;
      try {
        await api('/api/providers/delete', { method: 'POST', body: JSON.stringify({ id: p.id }) });
        reloadProviders();
      } catch (e) { alert('删除失败：' + e.message); }
    };
    acts.appendChild(delBtn);
  }
  const btnCancel = document.createElement('button');
  btnCancel.className = 'pf-cancel';
  btnCancel.textContent = '取消';
  btnCancel.onclick = () => {
    if (key === 'new') { provNew = null; provOpenKey = null; renderProvCards(); }
    else reloadProviders();
  };
  const btnSave = document.createElement('button');
  btnSave.className = 'pf-save';
  btnSave.textContent = '保存';
  btnSave.onclick = async () => {
    if (!p.presetId && !p.name.trim()) { alert('请填写名称'); return; }
    if (!p.presetId && !p.baseURL.trim()) { alert('请填写 API 地址'); return; }
    if (!p.key && !p.keyMasked && !confirm('尚未填写 API 密钥，确定保存？（本地服务如 Ollama 可不填）')) return;
    try {
      await putProvider(p, key);
      reloadProviders();
    } catch (e) { alert('保存失败：' + e.message); }
  };
  acts.appendChild(btnCancel);
  acts.appendChild(btnSave);
  form.appendChild(acts);

  card.appendChild(form);
  return card;
}

// putProvider 提交单张提供方卡片。卡片内的「保存」与底部通用「保存」共用这一条
// 路径（原来底部保存只写上下文页，模型页的密钥、增删模型点它不生效）。
// apiKey 传空串＝后端沿用已保存的密钥，所以没改密钥的卡片提交也无副作用。
async function putProvider(p, key) {
  await api('/api/providers', {
    method: 'POST',
    body: JSON.stringify({
      // 新卡片选了预置提供方时本地 id 还是空的：用预置 id 作为保存目标，
      // 否则后端识别不出预置（baseURL 传空）会报「必须填写 API 地址」
      id: p.id || p.presetId || '',
      name: p.name,
      baseURL: p.presetId ? '' : (p.baseURL || '').trim(),
      apiKey: p.key,
      protocol: p.presetId ? '' : (p.protocol || ''),
      models: p.models,
    }),
  });
  if (key === 'new') provNew = null; // 草稿已保存转正：清掉常驻草稿卡片
  p.base = provSig(p);               // 已落盘：基线推进，重试时不重复提交
}

// saveProviderDrafts 把模型页有改动的卡片一并落盘（底部通用「保存」先调它）。
// 校验不通过就整体不提交、如实指出是哪一张，返回 false 让调用方留在设置页。
async function saveProviderDrafts() {
  const targets = [];
  for (const p of provDraft) {
    if (p.key || provSig(p) !== p.base) targets.push([p, p.id]);
  }
  const nw = provNew;
  if (nw && (nw.name.trim() || nw.baseURL.trim() || nw.key || nw.models.length)) targets.push([nw, 'new']);
  if (!targets.length) return true; // 模型页没动过：不打扰用户

  for (const [p] of targets) {
    if (p.presetId) continue; // 预置提供方用系统地址，名称由后端补
    if (!p.name.trim()) {
      alert('有提供方还没填名称，模型页的改动这次没有保存；填好后重新点「保存」即可。');
      return false;
    }
    if (!p.baseURL.trim()) {
      alert('提供方「' + p.name.trim() + '」还没填 API 地址，模型页的改动这次没有保存；填好后重新点「保存」即可。');
      return false;
    }
  }
  const noKey = targets.filter(([p]) => !p.key && !p.keyMasked).length;
  if (noKey && !confirm('有 ' + noKey + ' 家提供方还没填 API 密钥（本地服务如 Ollama 可留空），仍要保存吗？')) return false;

  for (const [p, key] of targets) {
    try {
      await putProvider(p, key);
    } catch (e) {
      alert('模型改动保存失败：' + e.message + '\n（其余设置这次也没保存，可修正后重新点「保存」）');
      return false;
    }
  }
  return true;
}

async function saveSettings() {
  // 底部通用「保存」＝先把当前二级页（模型页）的草稿改变落盘，再走组件设置区
  // 的统一保存（CX.registerSettings 带 save 钩子的组件在此落库），最后写上下文页
  // 配置；常规页偏好本来即时生效。任一环节出问题就不关窗口，让用户就地修正。
  if (!(await saveProviderDrafts())) return;
  if (!(await CX.saveSettingsSections())) return; // 组件设置（如 AI 记忆与拟人化）
  // 间隔以 K 为单位输入（服务端存原值），×1000 提交，0=关闭规则自动压缩；
  // keepChars 的 0 是合法值（全压语义），不能用 || 兜底（会把 0 吞成默认值）
  const clampPct = (v) => Math.max(0, Math.min(100, parseInt(v, 10) || 0));
  const keepRaw = parseInt($('set-keepchars').value, 10);
  const intervalRaw = parseFloat($('set-compact-interval').value);
  const body = {
    compact: {
      mode: selRefs.compactMode.get(),
      keepChars: Number.isFinite(keepRaw) && keepRaw >= 0 ? keepRaw : 100000,
      interval: Number.isFinite(intervalRaw) && intervalRaw > 0 ? Math.round(intervalRaw * 1000) : 0,
      unit: selRefs.compactUnit.get() || 'chars',
      llmPct: clampPct($('set-compact-llmpct').value),
    },
  };
  try {
    await api('/api/config', { method: 'POST', body: JSON.stringify(body) });
  } catch (e) { alert('保存失败：' + e.message); return; }
  loadModels(); // 模型页刚可能增删了模型：对话区的模型芯片跟着刷新
  closeSettingsUI();
  loadStatus();
}

/* ---------- 常规页：本机浏览器偏好（语言/字体/发送行为，即时生效） ---------- */

// 阅读字号：只写 :root 的 --chat-fs 一处，对话区（.msg-col）与计划显示区
//（右栏「计划」视图正文 .plan-doc、计划浮窗 .q-card.plan）共用这个基准，两处内部
// 字号均为 em 相对值——改字号这两块文字同步缩放；侧栏/设置/输入控件不跟随。
const FONT_PX_DEFAULT = 14; // 默认档（「（默认）」标在它身上，档位增减量都以它为基准）

function applyChatFont(px) {
  document.documentElement.style.setProperty('--chat-fs', px + 'px');
  lsSet('jx.fontpx', String(px));
  // 字号变化改变行高与每行字数：已测过的气泡折叠判定作废（data-fold-done），
  // 清标记触发全量重测（app-chat.js 的 resetFoldMarks；脚本加载顺序异常时静默跳过）
  if (typeof resetFoldMarks === 'function') resetFoldMarks();
}

function restoreChatFont() {
  const px = parseFloat(lsGet('jx.fontpx', String(FONT_PX_DEFAULT))) || FONT_PX_DEFAULT;
  document.documentElement.style.setProperty('--chat-fs', px + 'px');
}
/* ---------- 常规页：文件备份（服务端配置，改动即时保存） ---------- */

function syncSnapshotState(on) {
  $('set-snapshot-state').textContent = on ? '已开启' : '已关闭';
}

async function saveSnapshotCfg(patch) {
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify(patch) });
    $('set-snapshot-on').checked = c.snapshotEnabled !== false;
    syncSnapshotState(c.snapshotEnabled !== false);
    if (c.snapshotDays != null) $('set-snapshot-days').value = c.snapshotDays;
  } catch (e) {
    alert('保存失败：' + e.message);
  }
}

$('set-snapshot-on').onchange = () => saveSnapshotCfg({ snapshotEnabled: $('set-snapshot-on').checked });
$('set-snapshot-days').onchange = () => {
  const v = parseInt($('set-snapshot-days').value, 10);
  if (isNaN(v) || (v < 1 && v !== -1)) { alert('天数需为 ≥1 的整数，或 -1（永不过期）'); return; }
  saveSnapshotCfg({ snapshotDays: v });
};

/* ---------- 常规页：会话浏览器数据保留天数（服务端配置，改动即时保存） ---------- */

async function saveBrowserProfileCfg(days) {
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ browserProfileDays: days }) });
    if (c.browserProfileDays != null) $('set-browserprofile-days').value = c.browserProfileDays;
  } catch (e) {
    alert('保存失败：' + e.message);
  }
}
$('set-browserprofile-days').onchange = () => {
  const v = parseInt($('set-browserprofile-days').value, 10);
  if (isNaN(v) || v < 0 || v > 3650) { alert('天数需为 0–3650 的整数（0=不清理）'); return; }
  saveBrowserProfileCfg(v);
};

/* ---------- 常规页：询问提醒（服务端配置，改动即时保存；嵌套部分提交，
   只传变的键，服务端按键合并并钳制 30-600） ---------- */
async function saveAskAlertCfg(patch) {
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ askAlert: patch }) });
    const aa = c.askAlert || {};
    $('set-askalert-on').checked = aa.enabled !== false;
    if (aa.afterSec) $('set-askalert-sec').value = aa.afterSec;
    if (aa.text !== undefined) $('set-askalert-text').value = aa.text; // 空串=默认文案，回显留空
  } catch (e) {
    alert('保存失败：' + e.message);
  }
}
$('set-askalert-on').onchange = () => saveAskAlertCfg({ enabled: $('set-askalert-on').checked });
$('set-askalert-sec').onchange = () => {
  let v = parseInt($('set-askalert-sec').value, 10);
  if (isNaN(v)) v = 240;
  v = Math.min(600, Math.max(30, v));
  $('set-askalert-sec').value = v;
  saveAskAlertCfg({ afterSec: v });
};
$('set-askalert-text').onchange = () => saveAskAlertCfg({ text: $('set-askalert-text').value.trim() });

/* ---------- 发送页：排队消息自动续发（服务端配置，改动即时保存） ----------
   默认开：本轮回复完成后，队列里的待发消息（不分来源）自动逐条发出；关掉后
   本轮结束队列保留，排队坞给「继续发送」入口（点它照发，不受开关限制）。 */

$('set-queueauto-on').onchange = async () => {
  const on = $('set-queueauto-on').checked;
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ queueAutoSend: on }) });
    $('set-queueauto-on').checked = c.queueAutoSend !== false; // 以服务端落库值为准回显
  } catch (e) {
    $('set-queueauto-on').checked = !on;
    alert('保存失败：' + e.message);
  }
};

/* ---------- 常规页：任务最大步数（服务端配置，改动即时保存） ----------
   表示法只有一种：服务端 maxSteps 0/负数 = 不限制（新装默认），> 0 = 手填的
   步数上限。设置页就一个输入框：回显多少就是多少（0 显示 0），保存原样提交
   （含 0）——不搞开关，用户填个 0 就懂，说明文字里写着。 */

// syncMaxSteps 按服务端值回显：0/负数一律显示 0（＝不限制），正数显示该值
function syncMaxSteps(n) {
  $('set-maxsteps').value = n > 0 ? n : 0;
}

$('set-maxsteps').onchange = async () => {
  const raw = $('set-maxsteps').value.trim();
  const v = Number(raw);
  if (raw === '' || !Number.isInteger(v) || v < 0) {
    alert('步数需为 ≥0 的整数（填 0 = 不限制）');
    const c = await api('/api/config').catch(() => null);
    if (c) syncMaxSteps(c.maxSteps); // 输入不合法：回到服务端实际值
    return;
  }
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ maxSteps: v }) });
    syncMaxSteps(c.maxSteps); // 以服务端落库值为准回显
  } catch (e) {
    alert('保存失败：' + e.message);
  }
};

/* ---------- 常规页：关闭窗口驻留后台（服务端配置，改动即时保存） ----------
   默认开：✕ / Alt+F4 只隐藏进托盘（会话与运行中的任务继续，双击托盘图标回
   到界面）；关掉后关闭窗口即真正退出程序。区块仅桌面版显示（网页/手机端无
   窗口关闭语义，见 initRenderMode 里的显示联动）。 */

$('set-tray-on').onchange = async () => {
  const on = $('set-tray-on').checked;
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ closeToTray: on }) });
    $('set-tray-on').checked = c.closeToTray !== false; // 以服务端落库值为准回显
  } catch (e) {
    $('set-tray-on').checked = !on;
    alert('保存失败：' + e.message);
  }
};

/* ---------- 常规页：局域网访问（bindHost 127.0.0.1 ⇄ 0.0.0.0，重启生效） ---------- */

function syncLanState(c) {
  const on = c.bindHost === '0.0.0.0' || c.bindHost === '::';
  $('set-lan-state').textContent = on ? '已开启（重启程序后对外可访问）' : '已关闭（仅本机可访问）';
  const urls = c.lanIPs || [];
  const port = location.port || '80';
  $('set-lan-urls').innerHTML = on && urls.length
    ? '手机连同一 Wi-Fi 后用浏览器打开：' + urls.map((ip) => `http://${esc(ip)}:${esc(port)}`).join('　或　')
    : '';
}

$('set-lan-on').onchange = async () => {
  const on = $('set-lan-on').checked;
  try {
    const c = await api('/api/config', {
      method: 'POST',
      body: JSON.stringify({ bindHost: on ? '0.0.0.0' : '127.0.0.1' }),
    });
    syncLanState(c);
  } catch (e) {
    alert('保存失败：' + e.message);
    $('set-lan-on').checked = !on;
  }
};

/* ---------- 网络访问页：公司接入开关（关闭＝不弹向导、后端不连公司服务端） ---------- */

function syncCompanyState(c) {
  const on = !c.companyLinkOff;
  $('set-company-state').textContent = on
    ? '已开启（首启自动弹接入向导；接入后自动跟随服务端变化）'
    : '已停用（不连接公司服务端，启动不再弹出向导）';
  $('wizard-rerun-field').classList.toggle('hidden', !on); // 停用时藏起「重新运行接入向导」
}

$('set-company-on').onchange = async () => {
  const on = $('set-company-on').checked;
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ companyLinkOff: !on }) });
    syncCompanyState(c);
  } catch (e) {
    alert('保存失败：' + e.message);
    $('set-company-on').checked = !on;
  }
};

/* ---------- 网络访问页：远程接入（开关 + 设备号/密钥/网关编辑，即时生效） ---------- */

let lastRemoteEntry = ''; // 入口链接快照：复制用 + 二维码按它增量重绘
let remotePollTimer = 0; // 连接异步成立：未连上时短轮询刷新状态文案

// 搜索服务「当前实际生效地址」：显示后端算好的 base（填了=填的，空=市场同源），
// 让用户一眼知道现在 AI 搜索打在哪个服务端；算不出来（两处都没填/地址非法）时
// 给明确原因，而不是留一行空白。
function syncSearchState(c) {
  const el = $('set-search-endpoint');
  if (!el) return;
  const custom = (c.searchURL || '').trim();
  const ep = (c.searchEndpoint || '').trim();
  if (ep) {
    el.innerHTML = '当前生效：<b>' + esc(ep) + '</b>　' + (custom ? '（用的是这里填的地址）' : '（跟随市场服务器同源）');
  } else {
    el.innerHTML = '<span style="color:var(--danger)">当前不可用：'
      + esc(c.searchEndpointErr || '尚未配置搜索服务地址') + '</span>';
  }
}

function syncRemoteState(c) {
  const ra = c.remoteAccess || {};
  $('set-remote-state').textContent = ra.connected
    ? '已连接网关'
    : (ra.enabled ? '已开启（正在连接网关：' + (ra.lastError || '稍候自动重试') + '）' : '已关闭');
  const entryEl = $('set-remote-entry');
  entryEl.innerHTML = ra.enabled && ra.entry
    ? '手机（外网可用）扫码或浏览器打开并收藏：<b>' + esc(ra.entry) + '</b>'
    : '';
  entryEl.style.cursor = ra.entry ? 'pointer' : '';
  lastRemoteEntry = ra.entry || '';
  // 二维码：入口变化才重绘（标准库 toString 是异步的），未开启时收起
  const qrBox = $('remote-qr');
  if (qrBox) {
    if (ra.enabled && ra.entry) {
      if (qrBox.dataset.entry !== ra.entry) {
        const entry = ra.entry;
        qrBox.dataset.entry = entry; // 先占位防重入；异步回来时入口又变则丢弃
        qrLib.qrSVG(entry, { size: 208 }).then((svg) => {
          if (qrBox.dataset.entry === entry) qrBox.innerHTML = svg;
        }).catch((e) => {
          if (qrBox.dataset.entry === entry) qrBox.textContent = '二维码生成失败：' + e.message;
        });
      }
      qrBox.classList.remove('hidden');
    } else {
      qrBox.classList.add('hidden');
      qrBox.dataset.entry = '';
    }
  }
  // 输入框以服务端落库值为准回显（焦点中不清扰，保存回包才刷新）
  if (document.activeElement !== $('set-remote-id')) $('set-remote-id').value = ra.myId || '';
  if (document.activeElement !== $('set-remote-pk')) $('set-remote-pk').value = ra.pairKey || '';
  if (document.activeElement !== $('set-remote-host')) $('set-remote-host').value = ra.host || '';
  if (document.activeElement !== $('set-remote-port')) $('set-remote-port').value = ra.port || 2227;
  // 市场服务器地址与发布者姓名已随市场组件化由 skill-market 组件注入本页
  // （#net-market-cfg 挂载段，组件自带回显与改动即存）。搜索服务（websearch.go
  // 的 searchEndpointBase）仍在本页：填了用填的、空则跟随市场同源；生效地址
  // 由后端算好下发（searchEndpoint）
  if (document.activeElement !== $('set-search-url')) $('set-search-url').value = c.searchURL || '';
  syncSearchState(c);
  if (document.activeElement !== $('set-registry-url')) $('set-registry-url').value = c.registryURL || '';
  if (document.activeElement !== $('set-update-url')) $('set-update-url').value = (c.updateURL === 'off' ? 'off' : (c.updateURL || ''));
  $('set-update-mode').value = (c.updateMode === 'manual' ? 'manual' : 'auto');
  // 开关一动连接就异步建立（一般一两秒）：未连上时定时重拉状态刷新文案；
  // 连上或设置页收起即停，不做长轮询
  clearTimeout(remotePollTimer);
  if (ra.enabled && !ra.connected && !$('settings').classList.contains('hidden')) {
    remotePollTimer = setTimeout(async () => {
      const cur = await api('/api/config').catch(() => null);
      if (cur) syncRemoteState(cur);
    }, 2500);
  }
}

// 入口链接点击复制（含配对密钥，复制成功给个轻提示）
$('set-remote-entry').addEventListener('click', () => {
  if (!lastRemoteEntry) return;
  navigator.clipboard.writeText(lastRemoteEntry).then(() => {
    const el = $('set-remote-entry');
    const old = el.innerHTML;
    el.innerHTML = '<b>已复制 ✓</b>';
    setTimeout(() => { el.innerHTML = old; }, 1200);
  }).catch(() => {});
});

$('set-remote-on').onchange = async () => {
  const on = $('set-remote-on').checked;
  try {
    const c = await api('/api/config', {
      method: 'POST',
      body: JSON.stringify({ remoteEnabled: on }),
    });
    syncRemoteState(c);
  } catch (e) {
    alert('保存失败：' + e.message);
    $('set-remote-on').checked = !on;
  }
};

// 设备号/密钥/网关：改动即存（单字段 PATCH），失败回滚为回包值
function bindRemoteInput(id, patchKey, parse) {
  $(id).onchange = async () => {
    const v = parse ? parse($(id).value) : $(id).value.trim();
    try {
      const c = await api('/api/config', {
        method: 'POST',
        body: JSON.stringify({ [patchKey]: v }),
      });
      syncRemoteState(c);
    } catch (e) {
      alert('保存失败：' + e.message);
      const cur = (await api('/api/config').catch(() => null));
      if (cur) syncRemoteState(cur);
    }
  };
}
bindRemoteInput('set-remote-id', 'remoteMyId');
bindRemoteInput('set-remote-host', 'remoteHost');
bindRemoteInput('set-remote-port', 'remotePort', (v) => parseInt(v, 10) || 0);
// 配对密钥：只读展示 + 「自动生成」换新（不可手填）。换新会作废旧入口/二维码——
// 已配对的手机要用新入口重连，先确认再生成；32 位十六进制与服务端 genSecret(32) 同规格。
$('btn-remote-pk-new').onclick = async () => {
  if (!await confirmCenter('换新配对密钥后，旧入口链接立即失效，已配对的手机要用新入口重新连接。确定换新吗？')) return;
  const bytes = new Uint8Array(16); // 16 字节 → 32 位十六进制
  crypto.getRandomValues(bytes);
  const key = [...bytes].map((b) => b.toString(16).padStart(2, '0')).join('');
  try {
    const c = await api('/api/config', { method: 'POST', body: JSON.stringify({ remotePairKey: key }) });
    syncRemoteState(c); // 回显新密钥；入口链接与二维码随之更新
  } catch (e) {
    alert('换新失败：' + e.message);
  }
};
// 市场服务器地址：已随市场组件化由 skill-market 组件注入网络访问页
// （#net-market-cfg 挂载段，组件内自带回显与「改动即存」），此处不再绑定；
// 后端 /api/config 的 marketURL 字段照旧。
// 搜索服务：同一套「改动即存」；留空 = 跟随市场同源。非法地址由后端 400 拦下
// （searchURL 不合法），这里 alert 出原因并回滚为服务端值
bindRemoteInput('set-search-url', 'searchURL');
// 网页托管平台（小程序平台）：已随小程序平台组件化迁入 sites-platform 组件的
// 设置区（设置 · 组件），此处不再绑定；后端 /sites 代理照旧。
bindRemoteInput('set-registry-url', 'registryURL');
bindRemoteInput('set-update-url', 'updateURL');
// 更新方式：auto=分块静默（默认）/manual=仅提醒手动整包安装；改动即存
bindRemoteInput('set-update-mode', 'updateMode');

/* ---------- 软件更新（update_blocks.go 分块静默更新） ---------- */

// refreshUpdateBox 设置页「软件更新」分区：当前/最新版本、最近检查、就绪待
// 重启等状态一屏汇总（/api/status 的 update 字段）；设置页打开时与手动检查后刷新。
async function refreshUpdateBox() {
  const el = $('update-man-state');
  const dl = $('btn-update-dl');
  if (!el) return;
  let st;
  try { st = await api('/api/status'); } catch (e) { el.textContent = '状态获取失败：' + e.message; return; }
  const u = st.update || {};
  let text = '当前版本 ' + (u.current || '-') + '　·　最新版本 ' + (u.latest || '未知');
  if (u.pendingRestart && u.pendingVersion) {
    text += '　·　<b>新版本 ' + esc(u.pendingVersion) + ' 已就绪，重启软件后生效</b>';
    dl.disabled = false;
  } else if (u.available) {
    text += '（有新版可装）';
  } else if (u.latest) {
    text += '（已是最新）';
  }
  if (u.checkedAt) text += '　·　上次检查 ' + esc(u.checkedAt);
  if (u.applied && Object.keys(u.applied).length) {
    const parts = Object.keys(u.applied).sort().map((k) => esc(k) + '→' + esc(u.applied[k]));
    text += '　·　已静默更新组件：' + parts.join('、');
  }
  if (u.lastError) text += '　·　<span style="color:var(--danger)">' + esc(u.lastError) + '</span>';
  el.innerHTML = text;
  dl.disabled = !u.downloadUrl;
  dl.title = u.downloadUrl ? '后台下载完整安装包，完成后自动运行安装' : '发布源没有可下载的安装包';
}
$('btn-update-check').onclick = async () => {
  const btn = $('btn-update-check');
  const el = $('update-man-state');
  btn.disabled = true;
  btn.textContent = '正在检查…';
  try {
    await api('/api/update/check', { method: 'POST' });
    if (el) el.textContent = '已开始检查（含静默下载应用，大组件可能要几分钟），稍后自动刷新…';
    // 检查在后台跑（可能下几十上百兆的分块）：几秒后刷状态，就绪/报错经事件流也会到
    setTimeout(refreshUpdateBox, 4000);
    setTimeout(refreshUpdateBox, 12000);
  } catch (e) {
    alert('检查失败：' + e.message);
    refreshUpdateBox();
  } finally {
    btn.disabled = false;
    btn.textContent = '立即检查更新';
  }
};
$('btn-update-dl').onclick = async () => {
  let st;
  try { st = await api('/api/status'); } catch (e) { alert('获取下载地址失败：' + e.message); return; }
  const dl = (st.update || {}).downloadUrl;
  if (!dl) { alert('发布源没有可下载的安装包'); return; }
  jxDownload(dl);
};
// 设置页打开/配置刷新时同步一次更新状态（全局函数，app-chat.js 的事件处理也会调）
window.refreshUpdateBox = refreshUpdateBox;

// 重新运行接入向导（app-wizard.js 的 openWizard）。向导弹层在 DOM 序上先于设置窗
// （层级在其下），从设置里打开会被盖住——先收起设置页再弹，向导直接可见。
$('btn-wizard-rerun').onclick = async () => {
  if (typeof openWizard !== 'function') return;
  closeSettingsUI();
  // 带原有配置预填（公司名/服务端地址/姓名）：重连只是点一下，不用重新找地址
  let prefill = {};
  try {
    const c = await api('/api/config');
    prefill = { company: c.wizardCompany || '', server: c.wizardServer || '', user: c.publisherName || '' };
  } catch (e) { /* 拿不到配置就空表单 */ }
  openWizard(prefill);
};

$('btn-settings').onclick = openSettings;
$('set-save').onclick = saveSettings;

/* ---------- 对话历史与压缩：长期助理主对话的版本 ----------
   主对话不再靠「清空」删历史来重新开始，而是「换版本」：新建一版＝从零开始的
   新数字助理，老版本的历史、文件快照、交付记录原样留在它自己名下，随时选回去
   接着聊。当前是哪一版由**服务端**记在库里（/api/assistant/versions），前端只照
   它显示与切换——绝不自己按更新时间挑，否则切回老版聊一句就把它顶成「最新」，
   压缩/定时任务转交/入口各取各的、版本当场错位。
   版本选择器是设置页这里**唯一的一枚下拉**（渲染与切换见 app-core.js 的
   refreshAssistantVersions / renderAssistantVersionPicker），这一节只放
   「新建一版 / 压缩当前版本」两个动作 + 那枚下拉，不把每一版铺成一片卡片，
   侧栏也不放任何版本控件。点下立即生效、不参与底部「保存」。 */

// confirmCenter 的层级（--z-confirm）已保证居中确认框压过设置页等一切页面
// 与弹窗，设置页里直接调用即可，无须临时抬层。

// refreshAssistantOps 动作后的界面定版：会话列表、版本下拉、正开着的主对话区、右栏子对话清单。
// 用户此刻正看着某一版（新建/切换换了版本，或压缩改了历史）：对话区跟着切过去/重载。
async function refreshAssistantOps() {
  try { await loadSessions(); } catch (e) { /* 列表拉取失败不影响操作结果 */ }
  try { await refreshAssistantVersions(); } catch (e) { /* 版本下拉拿不到就保持原样 */ }
  const showingAssist = !!(state.cur && state.cur.mode === 'assistant' && !state.cur.parentId);
  if (showingAssist) { openSession(assistantVer.current || state.cur.id); return; }
  try { refreshDrawer(); } catch (e) { /* 右栏没开时跳过 */ }
}

// assistantOpsState 写按钮下方那行状态说明（操作结果回执）。
function assistantOpsState(msg) {
  const el = $('assistant-ops-state');
  if (el) el.textContent = msg;
}

$('btn-assistant-new').onclick = async () => {
  if (!(await confirmCenter('新建一版「跟长期助理对话」？\n' +
      '新的一版是一个全新的数字助理，从零开始聊，工作目录、权限、模型等设置沿用当前这一版。\n' +
      '当前这一版的历史、文件快照与交付记录都原样保留，随时可以在上面选回来。'))) return;
  const btn = $('btn-assistant-new');
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = '新建中…';
  try {
    const r = await api('/api/assistant/versions/new', { method: 'POST' });
    assistantOpsState('已新建第 ' + r.version + ' 版，现在是全新的数字助理；上一版原样保留，可在上面的下拉里选回来。');
    await refreshAssistantOps();
  } catch (e) {
    alert('新建失败：' + e.message);
  } finally {
    btn.textContent = orig;
    btn.disabled = false;
  }
};

$('btn-assistant-compact').onclick = async () => {
  if (!(await confirmCenter('立即压缩「跟长期助理对话」的当前这一版？\n较早期的历史会被整理成一份摘要放进对话；原始消息永久保留、可随时查回。'))) return;
  const btn = $('btn-assistant-compact');
  const orig = btn.textContent;
  btn.disabled = true;
  btn.textContent = '压缩中…（要几秒）';
  try {
    const r = await api('/api/assistant/compact', { method: 'POST' });
    const sum = String(r.summary || '').replace(/\s+/g, ' ').trim();
    assistantOpsState('已压缩当前这一版。摘要开头：' +
      (sum ? sum.slice(0, 80) + (sum.length > 80 ? '…' : '') : '（摘要为空）') +
      '　原始历史仍可查回；对话区已翻新。');
    await refreshAssistantOps();
  } catch (e) {
    alert('压缩失败：' + e.message);
  } finally {
    btn.textContent = orig;
    btn.disabled = false;
  }
};

/* ---------- 设置导航：状态唯一、渲染集中 ----------
   心智模型两端同构：设置＝一级菜单＋二级分区页。桌面双栏并排框定（左一级
   右二级，level 只决定左栏高亮与否）；窄屏一屏一级或二级（pane-open 翻页，
   pushState 压栈供返回手势逐级退：二级→一级→关设置，取消/保存主动退等量
   历史不留死栈；桌面不压栈）。所有入口只改 setNav 状态，DOM 一律经 renderNav
   投影——导航类名的写点全文件仅此一处。level=1 即「未选中」空态：菜单全无
   高亮，二级区照常显示 pane 指的默认页（窄屏在后台，无渲染成本差异）。 */
const setNav = { open: false, pane: 'general', level: 1 };
let setHistDepth = 0;  // 窄屏历史栈层数：0 无 / 1 一级 / 2 二级（桌面恒 0）
let setHistPushed = 0; // 实际压栈成功的层数（与 depth 分开记账：环境不支持
                       // pushState、或历史栈被宿主页动过时两者会背离——一切
                       // 主动回退只信它，防超界跳出应用）

// 压一层设置历史。环境可能不允许 pushState（远程外壳一类的宿主会抛
// SecurityError）：失败时静默降级为纯 UI 导航——返回键直切层、不碰
// history，绝不让 back()/go() 越过自己压的栈把用户带出应用（手机端
// 二级页返回曾被带回宿主连接页的根源）。
function setHistPush() {
  try {
    history.pushState({ jxSettings: setHistDepth }, '');
    setHistPushed++;
  } catch (e) { /* 降级：只走 UI 层级 */ }
}

function settingsCard() { return document.querySelector('.settings-card'); }

// 唯一 DOM 出口：从 setNav 推导全部导航外观（菜单高亮/分区显隐/翻页/返回头标题/弹窗显隐）
function renderNav() {
  document.querySelectorAll('.set-nav-item').forEach((b) => {
    b.classList.toggle('active', setNav.level === 2 && b.dataset.pane === setNav.pane);
  });
  document.querySelectorAll('.set-pane').forEach((p) => {
    p.classList.toggle('active', p.id === 'set-pane-' + setNav.pane);
  });
  const card = settingsCard();
  if (card) card.classList.toggle('pane-open', setNav.level === 2); // 桌面 CSS 不消费，无视觉影响
  const cur = document.querySelector('.set-nav-item[data-pane="' + setNav.pane + '"]');
  $('set-back-title').textContent = setNav.level === 2 && cur ? cur.textContent.trim() : '设置';
  $('settings').classList.toggle('hidden', !setNav.open);
}

// 选中分区＝「地址」：一级菜单点击与各直达入口共用，两端同一条路径。唯一
// 平台差异＝窄屏把历史补到两层（契约：openSettings 已先压好「一级」底座）
function navGoto(pane) {
  setNav.pane = pane;
  setNav.level = 2;
  if (isNarrow() && setHistDepth < 2) {
    setHistDepth = 2;
    setHistPush();
  }
  renderNav();
}

function closeSettingsUI(fromPop) {
  setNav.open = false;
  setNav.level = 1; // 复位未选中态，下次打开从默认页起步
  renderNav();
  const n = setHistPushed; // 只退实际压成功的层数（depth 可能虚高，按它退会越界）
  setHistDepth = 0;
  setHistPushed = 0;
  if (n > 0 && !fromPop) {  // popstate 引发的关闭（栈已退穿）不再退栈
    backGuardSkipOnce = true; // 程序性退栈：返回键守卫跳过这次 popstate（左栏别跟着弹出来）
    history.go(-n);           // 退掉自己压入的栈；popstate 到达时已隐藏，无副作用
  }
  loadStatus(); // 设置里可能刚改了远程开关：左下角手机图标状态随之对齐
}

window.addEventListener('popstate', (e) => {
  const d = (e.state && e.state.jxSettings) || 0;
  if (!setNav.open) { setHistDepth = 0; setHistPushed = 0; return; } // UI 已关：只对齐
  setHistDepth = d;
  setHistPushed = d;
  if (d >= 1) { setNav.level = d; renderNav(); } // 返回手势：二级退一级（高亮熄灭）；一级再退＝下面的 d===0
  else closeSettingsUI(true);
});

$('set-cancel').onclick = () => closeSettingsUI();
$('settings').onclick = (e) => { if (e.target === $('settings')) closeSettingsUI(); };
$('set-back').onclick = () => {
  if (setNav.level === 2) {
    // UI 先直切回一级：不依赖 popstate——历史栈不可用/被宿主动过时也正确。
    // 随后若有自己压的栈再同步退一步；没压过（或压失败）就到此为止，
    // 绝不裸 back() 把用户带出应用
    setNav.level = 1;
    setHistDepth = 1;
    renderNav();
    if (setHistPushed > 0) {
      setHistPushed--;
      history.back(); // 到达 {jxSettings:1} 的 popstate 与上面的直切幂等
    }
  } else closeSettingsUI();                  // 一级：返回＝退出设置回对话区
};
document.querySelectorAll('.set-nav-item').forEach((b) => {
  b.onclick = () => navGoto(b.dataset.pane);
});
// 模型页：「添加提供方」= 插入一张展开的自定义卡片（已有未保存草稿则直接展开，不覆盖）
$('prov-add-card').onclick = () => {
  if (!provNew) {
    provNew = { id: '', presetId: null, presetURL: '', name: '', baseURL: '', protocol: '', key: '', keyMasked: '', models: [] };
  }
  provOpenKey = 'new';
  provModelOpen['new'] = -1;
  renderProvCards();
  const box = $('prov-list');
  if (box && box.lastElementChild) box.lastElementChild.scrollIntoView({ block: 'nearest' });
};
// 常规页：语言/字体/发送行为，改动即时生效并落本机
/* ---------- 自定义下拉：圆角按钮 + 浮层，设置页统一外观 ---------- */

const selRefs = {};

function closeAllSelects() {
  document.querySelectorAll('.jx-sel-pop.open').forEach((p) => p.classList.remove('open'));
}

// makeSelect(hostId, key, options, onChange)：在容器里建一套下拉，
// 返回 {set(v), get()}；onChange 在用户选中时触发。
function makeSelect(hostId, key, options, onChange) {
  const host = $(hostId);
  if (!host) return { set() {}, get() { return null; } };
  host.innerHTML = '';
  const wrap = document.createElement('div');
  wrap.className = 'jx-select';
  const btn = document.createElement('button');
  btn.type = 'button';
  btn.className = 'jx-sel-btn';
  const label = document.createElement('span');
  label.className = 'jx-sel-label';
  btn.appendChild(label);
  btn.insertAdjacentHTML('beforeend',
    '<svg class="jx-sel-chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>');
  const pop = document.createElement('div');
  pop.className = 'jx-sel-pop';
  let value = options.length ? options[0].v : null;
  const render = () => {
    const cur = options.find((o) => o.v === value);
    label.textContent = cur ? cur.label : '请选择';
    pop.innerHTML = '';
    for (const o of options) {
      const it = document.createElement('button');
      it.type = 'button';
      it.className = 'jx-sel-opt' + (o.v === value ? ' cur' : '');
      it.innerHTML = '<span></span><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6L9 17l-5-5"/></svg>';
      it.firstChild.textContent = o.label;
      it.onclick = (e) => {
        e.stopPropagation();
        value = o.v;
        render();
        closeAllSelects();
        if (onChange) onChange(o.v);
      };
      pop.appendChild(it);
    }
  };
  btn.onclick = (e) => {
    e.stopPropagation();
    const wasOpen = pop.classList.contains('open');
    closeAllSelects();
    pop.classList.toggle('open', !wasOpen);
  };
  wrap.appendChild(btn);
  wrap.appendChild(pop);
  host.appendChild(wrap);
  render();
  const ref = { set(v) { value = v; render(); }, get() { return value; } };
  selRefs[key] = ref;
  return ref;
}

/* ---------- 背景色：白 / 黑 / 跟随系统（CSS 变量整体换装） ---------- */

const themeMedia = window.matchMedia ? window.matchMedia('(prefers-color-scheme: dark)') : null;

function applyTheme() {
  const pref = lsGet('jx.theme', 'light');
  let t = pref;
  if (pref === 'auto' && themeMedia) t = themeMedia.matches ? 'dark' : 'light';
  document.documentElement.dataset.theme = t;
}
if (themeMedia && themeMedia.addEventListener) {
  themeMedia.addEventListener('change', () => { if (lsGet('jx.theme', 'light') === 'auto') applyTheme(); });
}

// 常规页各下拉：值即时落本机并生效
makeSelect('sel-lang', 'lang', [
  { v: 'zh', label: '简体中文' },
  { v: 'en', label: 'English（翻译逐步补齐）' },
], (v) => lsSet('jx.lang', v));
// 档位清单与默认值只在这里一处维护：默认档标「14px（默认）」，其余档标相对默认的
// 增减（15 (+1) / 13 (-1)）——一眼看出比默认大几号、小几号，增减量随 FONT_PX_DEFAULT 走。
const FONT_PX_OPTS = [12, 13, 14, 15, 16, 17, 18, 20];
makeSelect('sel-fontsize', 'fontsize', FONT_PX_OPTS.map((px) => {
  const d = px - FONT_PX_DEFAULT;
  return {
    v: String(px),
    label: d === 0 ? px + 'px（默认）' : px + ' (' + (d > 0 ? '+' + d : String(d)) + ')',
  };
}), (v) => applyChatFont(parseFloat(v) || FONT_PX_DEFAULT));
// 背景色：白/黑/跟随系统的预览卡片，点选即时生效
function syncThemeCards() {
  const cur = lsGet('jx.theme', 'light');
  document.querySelectorAll('#theme-cards .theme-card').forEach((b) => {
    b.classList.toggle('cur', b.dataset.v === cur);
  });
}
document.querySelectorAll('#theme-cards .theme-card').forEach((b) => {
  b.onclick = () => { lsSet('jx.theme', b.dataset.v); applyTheme(); syncThemeCards(); };
});

// 界面渲染模式（桌面版）：GPU 渲染 / 兼容模式。状态读自服务端实际运行值
// （/api/display-mode），网页/手机端 desktop=false 时整个区块隐藏。
// 点选切换 → POST 后端写/删兼容标记并前端热重启（窗口约 1 秒后原位重开，
// 本页随之前端一起重建，回来即是新模式）。
(async function initRenderMode() {
  const sec = document.getElementById('render-mode-section');
  if (!sec) return;
  let st = null;
  try { st = await api('/api/display-mode'); } catch (e) { return; }
  if (!st || !st.desktop) return;
  sec.style.display = '';
  // 「关闭窗口驻留后台」区块同属桌面版专属：复用同一次 display-mode 判定
  const traySec = document.getElementById('close-to-tray-section');
  if (traySec) traySec.style.display = '';
  const cards = document.querySelectorAll('#render-cards .theme-card');
  const stateLine = document.getElementById('render-mode-state');
  const sync = () => {
    const cur = st.software ? 'compat' : 'gpu';
    cards.forEach((b) => b.classList.toggle('cur', b.dataset.v === cur));
    stateLine.textContent = st.software
      ? '当前运行：兼容模式（软件渲染）'
      : '当前运行：GPU 渲染（默认）';
  };
  sync();
  cards.forEach((b) => {
    b.onclick = async () => {
      const want = b.dataset.v === 'compat';
      if (want === !!st.software) return;
      stateLine.textContent = '正在切换渲染模式，界面即将自动重开…';
      try {
        await api('/api/display-mode', { method: 'POST', body: JSON.stringify({ software: want }) });
        st.software = want;
        sync();
      } catch (e) {
        sync(); // 切换失败（非桌面版等）：恢复显示当前实际状态
      }
    };
  });
})();

// 主题色：内置强调色板（原 accent-theme 组件并入，8 预设 + 自定义）
const ACCENTS = [
  ['默认蓝', '#4d6bfe', '#3d5bef'],
  ['紫', '#8250df', '#6f42c1'],
  ['绿', '#1f9d55', '#188a49'],
  ['橘', '#bc4c00', '#a34400'],
  ['红', '#c22f2f', '#ab2727'],
  ['青', '#0d7ea5', '#0b6d8d'],
  ['粉', '#bf3989', '#a63075'],
  ['金', '#9a6700', '#855900'],
];

let accentStyleEl = null;

function applyAccent(color, hover, save) {
  if (!accentStyleEl) {
    accentStyleEl = document.createElement('style');
    accentStyleEl.id = 'accent-style';
    document.head.appendChild(accentStyleEl);
  }
  accentStyleEl.textContent = ':root{--accent:' + color + ';--accent-hover:' + hover + ';}';
  if (save) {
    lsSet('jx.accent', color + '|' + hover);
    // 同步写主程序配置：独立 WebView 环境读不到本页 localStorage，
    // 从 /api/config 的 accentColor 取色（fire-and-forget）
    fetch('/api/config', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ accentColor: color }),
    }).catch(() => {});
  }
  // 无论保存还是启动时应用，都把「点缀色 + 当前主题文字色」推给独立语音输入法：
  // 只放在 save 分支里的话，用户不改颜色就永远不生成 voicetype.json，输入法拿不到
  // 主程序的点缀色（首启即生效才叫联动）。
  pushVoiceTypeTheme(color);
}

// pushVoiceTypeTheme 推送点缀色 + 主题文字色。
// 文字色优先读 CSS 变量 --text 的计算值；取不到时按主程序主题档位直接给值
// （style.css 里 :root 是 #1f2328、深色是 #e6e8eb）——首次实测里 applyAccent
// 有可能在样式表生效前就跑，computed 拿到空串，此时不能把空值推过去。
function pushVoiceTypeTheme(color, retried) {
  let textColor = '';
  try { textColor = getComputedStyle(document.documentElement).getPropertyValue('--text').trim(); } catch (e) {}
  if (!textColor) {
    let dark = false;
    try {
      const t = lsGet('jx.theme', 'light');
      dark = t === 'dark' ||
        (t === 'auto' && window.matchMedia && window.matchMedia('(prefers-color-scheme: dark)').matches);
    } catch (e) { /* 读不到主题：按浅色 */ }
    textColor = dark ? '#e6e8eb' : '#1f2328';
  }
  fetch('/api/voicetype/link', {
    method: 'POST', headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ accentColor: color, textColor }),
  }).catch(() => {});
}

function buildAccentRow() {
  const row = $('accent-row');
  if (!row) return;
  const saved = lsGet('jx.accent', lsGet('cx:accent', '')); // 迁移旧组件的本地存储
  row.innerHTML = '';
  for (const [name, c, h] of ACCENTS) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'acc-swatch';
    b.style.background = c;
    b.title = name;
    if (saved.startsWith(c + '|')) b.classList.add('cur');
    b.onclick = () => {
      applyAccent(c, h, true);
      row.querySelectorAll('.acc-swatch').forEach((s) => s.classList.remove('cur'));
      b.classList.add('cur');
    };
    row.appendChild(b);
  }
  const custom = document.createElement('input');
  custom.type = 'color';
  custom.title = '自定义颜色';
  custom.oninput = () => {
    const c = custom.value;
    const d = c.replace(/^#(..)(..)(..)$/, (_, r, g, bl) => {
      const f = (x) => Math.max(0, parseInt(x, 16) - 18).toString(16).padStart(2, '0');
      return '#' + f(r) + f(g) + f(bl);
    });
    applyAccent(c, d, true);
    row.querySelectorAll('.acc-swatch').forEach((s) => s.classList.remove('cur'));
  };
  row.appendChild(custom);
  if (saved) {
    const [c, h] = saved.split('|');
    applyAccent(c, h || c, false);
  }
}
buildAccentRow();
// 发送快捷键：两枚键帽芯片（与 设置·快捷键 同一控件 HK.choiceChips、同一数据源
// jx.sendkey——chat.send 在那张表里登记，改任一处两边同步；占位提示链路
// updateSendKeyHint 不变）。selRefs.sendkey 保留：openSettings 打开时回显刷新用。
(function buildSendKeyRow() {
  const host = $('sel-sendkey');
  const def = window.HK ? HK.get('chat.send') : null;
  if (!host || !def) { selRefs.sendkey = { set() {} }; return; }
  host.innerHTML = '';
  const chips = HK.choiceChips(def);
  host.appendChild(chips);
  selRefs.sendkey = { set() { chips._hkRender(); } }; // 值的真相在 localStorage，set 只刷新外观
})();
// 默认值 = 等待排队：故「等待」放第一项（makeSelect 以第一项为初始值，与
// openSettings 里 lsGet('jx.midrun','queue') 的兜底默认一致）；用户手选后由
// onChange 的 lsSet('jx.midrun', v) 记住，选过「插话」的照旧保持插话。
makeSelect('sel-midrun', 'midrun', [
  { v: 'queue', label: '等待：本轮结束后排队逐条发送' },
  { v: 'interject', label: '插话：模型思考/输出中立即打断生效；工具执行中等下一步注入' },
], (v) => lsSet('jx.midrun', v));
makeSelect('sel-compact-mode', 'compactMode', [
  { v: 'llm', label: '模型摘要（默认）' },
  { v: 'rule', label: '规则折叠' },
], null);
makeSelect('sel-compact-unit', 'compactUnit', [
  { v: 'chars', label: '字数（默认，模型无关）' },
  { v: 'tokens', label: 'token（模型返回的实际用量）' },
], null);

/* ---------- 技能管理（/api/skills：自带只读展示，用户可加/开关/删） ---------- */

async function renderSkillsPanel() {
  const bBox = $('skill-builtin-list');
  const uBox = $('skill-user-list');
  if (!bBox || !uBox) return;
  bBox.innerHTML = '加载中…';
  uBox.innerHTML = '';
  let data;
  try { data = await api('/api/skills'); } catch (e) { bBox.textContent = '技能清单读取失败：' + e.message; return; }
  bBox.innerHTML = '';

  for (const s of (data.builtin || [])) {
    const row = document.createElement('div');
    row.className = 'plugin-row';
    row.innerHTML =
      `<div class="pr-info"><div class="pr-line"><b>${esc(s.name)}</b>${s.version ? `<span class="pr-ver">v${esc(s.version)}</span>` : ''}</div>` +
      `<p>${esc(s.description || '')}</p></div>`;
    const tag = document.createElement('span');
    tag.className = 'skill-constant';
    tag.textContent = '常开';
    row.appendChild(tag);
    bBox.appendChild(row);
  }

  for (const s of (data.user || [])) {
    const row = document.createElement('div');
    row.className = 'plugin-row';
    // 显示名中文优先（displayName → name → id），与市场卡片/环境快照同一口径
    row.innerHTML =
      `<div class="pr-info"><div class="pr-line"><b>${esc(s.displayName || s.name || s.id)}</b>` +
      `${s.version ? `<span class="pr-ver">v${esc(s.version)}</span>` : ''}<span class="pr-ver">${esc(s.id)}</span></div>` +
      `<p>${esc(s.description || '')}</p></div>`;
    const tg = document.createElement('label');
    tg.className = 'pr-toggle';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = !!s.enabled;
    cb.onchange = async () => {
      cb.disabled = true;
      const note = row.querySelector('.pr-note');
      try {
        await api('/api/skills/toggle', { method: 'POST', body: JSON.stringify({ id: s.id, enabled: cb.checked }) });
        if (note) note.textContent = cb.checked ? '已开启' : '已停用';
      } catch (e) {
        alert('切换失败：' + e.message);
        cb.checked = !cb.checked;
      }
      cb.disabled = false;
      setTimeout(() => { if (note) note.textContent = ''; }, 3000);
    };
    tg.appendChild(cb);
    tg.appendChild(document.createElement('span'));
    row.appendChild(tg);
    const del = document.createElement('button');
    del.className = 'skill-del';
    del.title = '删除该技能';
    del.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 6h18"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M10 11v6"/><path d="M14 11v6"/></svg>';
    del.onclick = async () => {
      const disp = s.displayName || s.name || s.id; // 与清单/市场卡片同口径：显示名中文优先
      if (!(await confirmCenter('删除技能「' + disp + '」？技能目录将被一并移除。', false, true))) return;
      try {
        await api('/api/skills/delete', { method: 'POST', body: JSON.stringify({ id: s.id }) });
        renderSkillsPanel();
      } catch (e) { alert('删除失败：' + e.message); }
    };
    row.appendChild(del);
    const note = document.createElement('div');
    note.className = 'pr-note';
    row.appendChild(note);
    uBox.appendChild(row);
  }

  if (!(data.user || []).length) {
    uBox.innerHTML = '<div class="skill-empty">还没有用户技能。点上方「添加技能」选择文件夹、ZIP 压缩包或 .md 文件，或把它们直接拖进本页安装。</div>';
  }
}

// 添加技能：弹操作系统原生选择框（后端代选，拿真实路径），文件夹整目录复制、
// ZIP 解压安装、单个 .md 说明文件包装安装；用户取消选择框时静默返回。
async function installSkillFromPicker(kind) {
  closePopovers();
  // 手机端（窄屏）：系统选择框由后端弹在宿主机屏幕上，点了只会白等或直接
  // 报错（安卓上「仅支持 Windows」）——与「添加工作目录」同款口径，指到电脑端
  if (isNarrow()) { alert('手机端不支持选择技能文件/文件夹：请在电脑端（宽屏）的设置页添加技能。'); return; }
  const btn = $('skill-add-btn');
  let picked;
  try {
    const url = (kind === 'file' ? '/api/pick-file' : '/api/pick-folder') +
      '?title=' + encodeURIComponent(kind === 'file' ? '选择技能文件（ZIP 压缩包 / MD 说明）' : '选择技能文件夹');
    picked = await api(url);
  } catch (e) { alert('无法打开选择框：' + e.message); return; }
  const p = (picked && picked.path) || '';
  if (!p) return; // 用户取消了选择框
  btn.disabled = true;
  btn.textContent = '安装中…';
  try {
    await api('/api/skills/add', { method: 'POST', body: JSON.stringify({ path: p }) });
    renderSkillsPanel();
  } catch (e) { alert('添加失败：' + e.message); }
  btn.disabled = false;
  btn.textContent = '＋ 添加技能';
}

$('skill-add-btn').onclick = () => openPopover('skill-add-pop', $('skill-add-btn'));
$('sa-folder').onclick = () => installSkillFromPicker('folder');
$('sa-zip').onclick = () => installSkillFromPicker('file');

/* ---------- 技能拖拽安装（拖 .zip / .md 文件或整个文件夹进技能页） ----------
   单文件（.zip 解压、.md 包装）走 POST /api/skills/upload（multipart 单文件）；
   文件夹走 webkitGetAsEntry 递归遍历 → POST /api/skills/install-files
   （multipart files+paths，后端组装成技能目录再走同一条 addUserSkill）。
   两条路最终都落 addUserSkill。 */
async function uploadSkillFile(file, note) {
  const fd = new FormData();
  fd.append('file', file, file.name);
  try {
    const res = await fetch('/api/skills/upload', { method: 'POST', body: fd });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    note.textContent = `「${file.name}」安装成功`;
    return true;
  } catch (e) {
    alert(`「${file.name}」安装失败：` + e.message);
    return false;
  }
}

async function uploadSkillFolder(entries, note) {
  // entries 必须在 drop 事件同步阶段取出来（await 后 DataTransfer 失效）
  const collect = [];
  const walk = async (entry, prefix) => {
    if (!entry) return;
    if (entry.isFile) {
      await new Promise((res) => entry.file(
        (f) => { collect.push({ file: f, path: (prefix || '') + entry.name }); res(); }, res));
      return;
    }
    if (!entry.isDirectory) return;
    const reader = entry.createReader();
    await new Promise((res) => {
      const step = () => reader.readEntries(async (batch) => {
        if (!batch.length) return res();
        for (const en of batch) await walk(en, (prefix || '') + entry.name + '/');
        step();
      }, res);
      step();
    });
  };
  for (const en of entries) await walk(en, '');
  if (!collect.length) {
    alert('文件夹读取失败（或是个空文件夹）。');
    return false;
  }
  const fd = new FormData();
  fd.append('paths', JSON.stringify(collect.map((x) => x.path)));
  for (const x of collect) fd.append('files', x.file, x.path.split('/').pop());
  note.textContent = `安装中（${collect.length} 个文件）…`;
  try {
    const res = await fetch('/api/skills/install-files', { method: 'POST', body: fd });
    const data = await res.json().catch(() => ({}));
    if (!res.ok) throw new Error(data.error || res.statusText);
    note.textContent = `「${collect[0].path.split('/')[0]}」安装成功`;
    return true;
  } catch (e) {
    alert('文件夹安装失败：' + e.message);
    return false;
  }
}

function bindSkillDropZone() {
  const pane = $('set-pane-skills');
  if (!pane || pane.dataset.dropBound) return;
  pane.dataset.dropBound = '1';
  const note = document.createElement('div');
  note.className = 'pr-note';
  pane.addEventListener('dragover', (e) => {
    if (![...(e.dataTransfer.types || [])].includes('Files')) return;
    e.preventDefault(); // 允许落点
    pane.classList.add('drop-hover');
  });
  pane.addEventListener('dragleave', (e) => {
    if (e.target === pane) pane.classList.remove('drop-hover');
  });
  pane.addEventListener('drop', async (e) => {
    e.preventDefault();
    pane.classList.remove('drop-hover');
    const dt = e.dataTransfer;
    // 文件夹：entries 里有 directory 才走文件夹通道（同步阶段取出，见上）
    const dirEntries = [];
    if (dt && dt.items && dt.items.length) {
      for (const it of dt.items) {
        if (it.kind !== 'file') continue;
        const en = it.webkitGetAsEntry ? it.webkitGetAsEntry() : null;
        if (en && en.isDirectory) dirEntries.push(en);
      }
    }
    pane.appendChild(note);
    if (dirEntries.length) {
      note.textContent = '安装中…';
      let ok = 0;
      for (const en of dirEntries) { if (await uploadSkillFolder([en], note)) ok++; }
      if (dirEntries.length > 1) {
        note.textContent = `完成：成功 ${ok} / ${dirEntries.length} 个技能文件夹`;
      }
      renderSkillsPanel();
      return;
    }
    // 散文件：只认 .zip / .md（其他散文件装不了，指路按钮）
    const files = [...(dt.files || [])];
    const packs = files.filter((f) => /\.(zip|md)$/i.test(f.name));
    if (!packs.length) {
      alert(files.length
        ? '只支持拖入 .zip 压缩包、单个 .md 文件或整个技能文件夹安装（其他散文件请先用「添加技能」按钮）'
        : '拖入的内容不是文件（技能文件夹请用「添加技能」按钮选择）');
      return;
    }
    note.textContent = `安装中（${packs.length} 个）…`;
    let ok = 0;
    for (const f of packs) { if (await uploadSkillFile(f, note)) ok++; }
    note.textContent = ok === packs.length
      ? `已安装 ${ok} 个技能${ok > 1 ? '包' : ''}，清单见下方`
      : `完成：成功 ${ok} / ${packs.length}`;
    renderSkillsPanel();
  });
}

/* ---------- 子对话专家（/api/experts：内置只读展示，自定义可增删改开关） ---------- */

// 名称即调用参数：这里展示的每个名称（内置与自定义）都与 dispatch_sub 的
// expert 参数共用同一份后端事实来源（/api/experts 的 builtin/user），前端
// 不抄写不改写——保证「所见名称 = 可填参数」100% 一致。

let expertDraft = [];     // 编辑中的专家列表（改动先落草稿，点保存才提交）
let expertOrig = [];      // 打开面板时的原始快照（取消 = 回滚到它）
let expertOpenID = null;  // 展开编辑的卡片：专家 id / '__new' / null（手风琴式单开）

// 专家预设的模型格式：''（跟随主对话）| providerId/modelId | 纯 modelId。
// 显示名从已配置模型清单（state.models）解析，解析不到原样展示。
function expertModelLabel(m) {
  m = (m || '').trim();
  if (!m) return '';
  const slash = m.indexOf('/');
  const pid = slash > 0 ? m.slice(0, slash) : '';
  const mid = slash > 0 ? m.slice(slash + 1) : m;
  const list = state.models || [];
  const hit = list.find((x) => x.providerId === pid && x.modelId === mid) ||
    (pid === '' ? list.find((x) => x.modelId === mid) : null);
  return (hit && (hit.modelName || hit.modelId)) || m;
}

function expertMeta(e) {
  const parts = [e.inheritContext ? '继承上下文' : '不继承上下文'];
  parts.push(e.model ? '默认模型 ' + expertModelLabel(e.model) : '模型随主对话');
  return parts.join(' · ');
}

// 专家表单的模型选择面板：与对话区同源（/api/models），点选写回草稿。
// 选中后 renderExpertCards 重建卡片（expertOpenID 不变，卡片保持展开）。
async function openExpertModelPop(e, anchor) {
  if (!(state.models || []).length) await loadModels(); // 设置页早于首页加载时兜底
  const pop = $('expert-model-pop');
  const cur = (e.model || '').trim();
  let html = `<button class="pop-item model-item ${cur === '' ? 'sel' : ''}" data-key="">跟随主对话（默认）</button>`;
  let lastProv = '';
  for (const m of state.models || []) {
    if (m.providerId !== lastProv) {
      html += `<div class="pop-group">${esc(m.providerName)}</div>`;
      lastProv = m.providerId;
    }
    const k = m.providerId + '/' + m.modelId;
    html += `<button class="pop-item model-item ${k === cur ? 'sel' : ''}" data-key="${esc(k)}"><span>${esc(m.modelName || m.modelId)}</span></button>`;
  }
  if (!(state.models || []).length) html = '<div class="pop-group">还没有已配置的模型，先到「模型」页添加提供方</div>';
  pop.innerHTML = html;
  pop.querySelectorAll('.model-item').forEach((b) => {
    b.onclick = () => {
      e.model = b.dataset.key;
      closePopovers();
      renderExpertCards();
    };
  });
  openPopover('expert-model-pop', anchor);
}

async function renderExpertsPanel() {
  const bBox = $('expert-builtin-list');
  const uBox = $('expert-user-list');
  if (!bBox || !uBox) return;
  bBox.innerHTML = '';
  uBox.innerHTML = '加载中…';
  let data;
  try { data = await api('/api/experts'); } catch (e) { uBox.textContent = '专家清单读取失败：' + e.message; return; }
  // 内置：直接渲染后端清单（名称=可填参数，一个字都不改）
  for (const b of (data.builtin || [])) {
    const row = document.createElement('div');
    row.className = 'plugin-row';
    row.innerHTML =
      `<div class="pr-info"><div class="pr-line"><b>${esc(b.name)}</b></div>` +
      `<p>${esc(b.description || '')}</p></div>`;
    const tag = document.createElement('span');
    tag.className = 'skill-constant';
    tag.textContent = '常开';
    row.appendChild(tag);
    bBox.appendChild(row);
  }
  const fields = (e) => ({
    id: e.id || '', name: e.name || '', description: e.description || '',
    prompt: e.prompt || '', model: e.model || '',
    inheritContext: !!e.inheritContext, enabled: !!e.enabled,
  });
  expertDraft = (data.user || []).map(fields);
  expertOrig = (data.user || []).map(fields);
  expertOpenID = null;
  renderExpertCards();
}

function renderExpertCards() {
  const box = $('expert-user-list');
  if (!box) return;
  box.innerHTML = '';
  expertDraft.forEach((e) => box.appendChild(buildExpertCard(e)));
  if (!expertDraft.length) {
    box.innerHTML = '<div class="skill-empty">还没有自定义专家。点上方「添加专家」创建——比如一个带专属审查提示词、默认用别的模型的审查员。</div>';
  }
}

// 开关即时保存：upsert 同一条记录，只翻 enabled
async function toggleExpertEnabled(e, on) {
  try {
    await api('/api/experts', { method: 'POST', body: JSON.stringify({ ...e, enabled: on }) });
    e.enabled = on;
  } catch (err) {
    alert('切换失败：' + err.message);
  }
}

function buildExpertCard(e) {
  const isNew = e.id === '__new'; // 新建草稿的临时 id（保存时才换成后端 id）
  const open = expertOpenID === e.id;
  const card = document.createElement('div');
  card.className = 'prov-card' + (open ? ' open' : '') + (e.enabled ? '' : ' off');

  // 折叠头：名称+描述（与「组件」行同款排版）+ 元信息 + 开关 + 编辑；
  // 点名称区展开，开关不冒泡
  const head = document.createElement('div');
  head.className = 'pv-head';
  const info = document.createElement('div');
  info.className = 'pr-info ex-head-info';
  info.innerHTML =
    `<div class="pr-line"><b>${esc(e.name || '未命名专家')}</b></div>` +
    `<p>${esc(e.description || '（没有描述）')}</p>`;
  head.appendChild(info);
  const meta = document.createElement('span');
  meta.className = 'ex-meta';
  meta.textContent = expertMeta(e);
  head.appendChild(meta);
  if (!isNew) { // 新卡片保存前不给开关（还没落到后端）
    const tg = document.createElement('label');
    tg.className = 'pr-toggle';
    tg.title = e.enabled ? '停用后主对话看不到该专家' : '启用后主对话可按名称派发';
    const cb = document.createElement('input');
    cb.type = 'checkbox';
    cb.checked = !!e.enabled;
    cb.onclick = (ev) => ev.stopPropagation();
    cb.onchange = async () => { await toggleExpertEnabled(e, cb.checked); renderExpertCards(); };
    tg.appendChild(cb);
    tg.appendChild(document.createElement('span'));
    tg.onclick = (ev) => ev.stopPropagation(); // 点开关只翻转，不触发展开/收起
    head.appendChild(tg);
  }
  const edit = document.createElement('span');
  edit.className = 'pv-edit';
  edit.textContent = open ? '收起' : '编辑';
  head.appendChild(edit);
  head.onclick = () => {
    if (open) {
      if (isNew) { expertDraft = expertDraft.filter((x) => x !== e); }
      else { // 收起 = 丢弃未保存的修改，回滚到打开面板时的快照
        const o = expertOrig.find((x) => x.id === e.id);
        if (o) Object.assign(e, o);
      }
      expertOpenID = null;
    } else {
      expertOpenID = e.id;
    }
    renderExpertCards();
  };
  card.appendChild(head);
  if (!open) return card;

  // 展开表单
  const form = document.createElement('div');
  form.className = 'pv-form';
  const mk = (labelText, inputEl, full) => {
    const cell = document.createElement('div');
    if (full) cell.className = 'full';
    const l = document.createElement('div');
    l.className = 'pf-label';
    l.textContent = labelText;
    cell.appendChild(l);
    cell.appendChild(inputEl);
    return cell;
  };
  const grid = document.createElement('div');
  grid.className = 'pf-two ex-form-grid';
  const inName = document.createElement('input');
  inName.className = 'pf-input';
  inName.placeholder = '专家名称';
  inName.value = e.name;
  inName.oninput = () => { e.name = inName.value; };
  grid.appendChild(mk('名称', inName));
  const btnModel = document.createElement('button');
  btnModel.type = 'button';
  btnModel.className = 'chip-btn ex-model-chip';
  btnModel.title = '选择该专家的默认模型';
  btnModel.innerHTML =
    `<span>${esc(expertModelLabel(e.model) || '跟随主对话（默认）')}</span>` +
    '<svg class="caret" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 9l6 6 6-6"/></svg>';
  btnModel.onclick = (ev) => { ev.stopPropagation(); openExpertModelPop(e, btnModel); };
  grid.appendChild(mk('默认模型', btnModel));
  form.appendChild(grid);

  const inDesc = document.createElement('input');
  inDesc.className = 'pf-input';
  inDesc.placeholder = '给主对话看的选用依据';
  inDesc.value = e.description;
  inDesc.oninput = () => { e.description = inDesc.value; };
  form.appendChild(mk('描述', inDesc, true));

  const inPrompt = document.createElement('textarea');
  inPrompt.className = 'pf-input pf-textarea';
  inPrompt.placeholder = '追加的系统提示词：拼在该子对话模式基础提示词之后，定义这位专家的立场与工作方式';
  inPrompt.value = e.prompt;
  inPrompt.oninput = () => { e.prompt = inPrompt.value; };
  form.appendChild(mk('追加提示词', inPrompt, true));

  // 继承上下文开关
  const ctxRow = document.createElement('div');
  ctxRow.className = 'ex-inline-toggle';
  const ctxTg = document.createElement('label');
  ctxTg.className = 'pr-toggle';
  const ctxCb = document.createElement('input');
  ctxCb.type = 'checkbox';
  ctxCb.checked = !!e.inheritContext;
  ctxCb.onchange = () => { e.inheritContext = ctxCb.checked; };
  ctxTg.appendChild(ctxCb);
  ctxTg.appendChild(document.createElement('span'));
  ctxRow.appendChild(ctxTg);
  const ctxDesc = document.createElement('span');
  ctxDesc.className = 'sf-desc';
  ctxDesc.textContent = '继承对话上下文（开启后子对话完整继承主对话聊天记录，任务只需写「接下来做什么」；是否继承完全由专家预设决定）';
  ctxRow.appendChild(ctxDesc);
  form.appendChild(mk('上下文', ctxRow, true));

  // 操作区：删除 | 取消 | 保存
  const acts = document.createElement('div');
  acts.className = 'pf-actions';
  if (!isNew) {
    const del = document.createElement('button');
    del.className = 'pf-del';
    del.textContent = '删除专家';
    del.onclick = async () => {
      if (!(await confirmCenter('删除专家「' + (e.name || '未命名') + '」？已派发的子对话保留，只是不再追加该提示词。', false, true))) return;
      try {
        await api('/api/experts/delete', { method: 'POST', body: JSON.stringify({ id: e.id }) });
        renderExpertsPanel();
      } catch (err) { alert('删除失败：' + err.message); }
    };
    acts.appendChild(del);
  }
  const btnCancel = document.createElement('button');
  btnCancel.className = 'pf-cancel';
  btnCancel.textContent = '取消';
  btnCancel.onclick = () => {
    if (isNew) expertDraft = expertDraft.filter((x) => x !== e);
    else { // 取消 = 丢弃未保存的修改
      const o = expertOrig.find((x) => x.id === e.id);
      if (o) Object.assign(e, o);
    }
    expertOpenID = null;
    renderExpertCards();
  };
  const btnSave = document.createElement('button');
  btnSave.className = 'pf-save';
  btnSave.textContent = '保存';
  btnSave.onclick = async () => {
    if (!e.name.trim()) { alert('请填写名称（它是主对话派发时的调用参数）'); return; }
    if (!e.prompt.trim()) { alert('请填写追加提示词'); return; }
    try {
      await api('/api/experts', {
        method: 'POST',
        body: JSON.stringify({
          id: isNew ? '' : e.id, name: e.name, description: e.description,
          prompt: e.prompt, model: e.model,
          inheritContext: !!e.inheritContext, enabled: !!e.enabled,
        }),
      });
      renderExpertsPanel();
    } catch (err) { alert('保存失败：' + err.message); }
  };
  acts.appendChild(btnCancel);
  acts.appendChild(btnSave);
  form.appendChild(acts);

  card.appendChild(form);
  return card;
}

// 「添加专家」= 插入一张展开的新卡片（enabled 默认开；__new 是临时 id）
$('expert-add-btn').onclick = () => {
  if (expertOpenID === '__new') return;
  expertDraft.push({ id: '__new', name: '', description: '', prompt: '', model: '', inheritContext: false, enabled: true });
  expertOpenID = '__new';
  renderExpertCards();
  const box = $('expert-user-list');
  if (box && box.lastElementChild) {
    box.lastElementChild.scrollIntoView({ block: 'nearest' });
    const first = box.lastElementChild.querySelector('.pf-input');
    if (first) first.focus();
  }
};
