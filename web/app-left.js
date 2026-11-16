/* app-left.js —— 左侧栏 + 顶栏逻辑：会话列表/搜索/回收站/工作区目录、侧栏开合
   （jx.sb 记忆）、顶栏状态胶囊。第二个加载。 */

/* ---------- 状态与侧栏 ---------- */

// 顶栏状态区：异常态（缺 Key / 离线）才显示告警胶囊；正常态收起胶囊，
// 换成手机图标——远程访问的快捷入口，点击直达设置的网络访问分区看扫码。
async function loadStatus() {
  const el = $('status-text');
  const ph = $('status-phone');
  try {
    const st = await api('/api/status');
    state.defaultWs = st.workspace || '';
    if (!st.hasKey) {
      el.textContent = '未配置 Key'; el.className = 'pill warn'; el.classList.remove('hidden');
      ph.classList.add('hidden');
    } else {
      el.classList.add('hidden');
      ph.classList.remove('hidden');
      const ra = st.remote || {};
      ph.className = 'foot-phone' + (ra.enabled ? (ra.connected ? '' : ' half') : ' off');
      ph.title = ra.enabled
        ? (ra.connected ? '手机远程访问已就绪——点击扫码接入' : '远程访问已开启，网关未连上——点击查看')
        : '开启手机远程访问——点击设置';
    }
    // 更新横幅：SSE 无回放，刚打开页面从 /api/status 恢复——有新版本（整包路）
    // 或主程序已静默交换待重启（分块路）都置顶提示
    if (st.update && (st.update.available || st.update.pendingRestart)) showUpdateBanner(st.update);
    // 「操作手机」能力芯片只在安卓端引擎上出现（桌面没有 device_*/adb_* 工具）
    if (!st.android) document.getElementById('perm-opt-device')?.classList.add('hidden');
    syncComposer();
  } catch (e) {
    el.textContent = '服务离线'; el.className = 'pill gray'; el.classList.remove('hidden');
    ph.classList.add('hidden');
  }
}

$('status-phone').addEventListener('click', (e) => {
  e.stopPropagation(); // 拦住外层设置按钮的默认打开路径，改直达网络访问分区
  openSettings('network');
});

// 反馈胶囊：同样拦住外层设置按钮，改直达设置面板「关于」页——问题反馈表单就挂在
// 这个分区里（#set-pane-about > #feedback-section），省去用户自己翻菜单找路径。
// 打开后把表单滚进视野（关于页比弹窗长时不用再往下找）；Enter / Space 等价点击。
async function openFeedbackPane(e) {
  e.preventDefault();
  e.stopPropagation();
  await openSettings('about');
  const sec = document.getElementById('feedback-section');
  if (sec) sec.scrollIntoView({ block: 'nearest' });
}
$('btn-feedback').addEventListener('click', openFeedbackPane);
$('btn-feedback').addEventListener('keydown', (e) => {
  if (e.key === 'Enter' || e.key === ' ') openFeedbackPane(e);
});


/* ---------- 侧栏视图存量迁移（20261105.sidebar-view-time）：按工作区 → 时间线 ----------
 * 老版本侧栏默认「按工作区分组」，动过排序菜单的浏览器会把整份设置（含
 * group:'ws'）存进 localStorage；新版默认已改时间线，但存量旧值不会自己变——
 * 升级到本版后首次加载时统一转成时间线。账本 jx.mig 记账后永不再跑：转换后
 * 用户再手选「按工作区」是新的决定，不被覆盖；新装机没有 jx.view，无事可做
 * 只记账。id 与语义对齐 Go 侧 migrations.go 的版本化一次性迁移——jx.view 是
 * 各浏览器各自的存储，Go 账本（data/migrations.json）物理上够不着，故账本落
 * 浏览器本地。 */
(function () {
  const ID = '20261105.sidebar-view-time';
  let done = lsGet('jx.mig', {});
  if (!done || typeof done !== 'object' || Array.isArray(done)) done = {}; // 账本损坏从零重建（转换本身幂等）
  if (done[ID]) return;
  const v = lsGet('jx.view', null);
  if (v && typeof v === 'object' && v.group === 'ws') { v.group = 'time'; lsSet('jx.view', v); }
  done[ID] = 1;
  lsSet('jx.mig', done);
})();

// 侧栏视图设置：分组方式（time=时间线 ws=按工作区 flat=单列表）与排序方式；默认时间线，手动选过则 localStorage 覆盖
const viewCfg = Object.assign({ group: 'time', sort: 'updated' }, lsGet('jx.view', {}));
const wsCollapsed = lsGet('jx.wscol', {});   // 工作区分组折叠状态


/* ---------- 侧栏标题行工具：搜索 / 排序选项 / 添加工作目录 ---------- */

/* ---------- 侧栏搜索：双路逐字搜索（标题 + 聊天记录），微信式分屏结果 ---------- */

// 搜索状态：abortController 中止在途请求；token 防串台；revealTimer 控制逐条渐现
let searchAbort = null;
let searchToken = 0;
let searchRevealTimer = null;
let sideSearchDebounce = null;

function sideSearchOpen() {
  return !$('side-search').classList.contains('hidden');
}

function toggleSideSearch() {
  if (sideSearchOpen()) {
    closeSideSearch();
    return;
  }
  $('side-search').classList.remove('hidden');
  const inp = document.getElementById('side-search-input');
  inp.value = '';
  inp.focus();
}

// 关闭搜索：中止两路请求与渐现动画，还原侧栏
function closeSideSearch() {
  if (searchAbort) { searchAbort.abort(); searchAbort = null; }
  if (searchRevealTimer) { clearInterval(searchRevealTimer); searchRevealTimer = null; }
  if (sideSearchDebounce) { clearTimeout(sideSearchDebounce); sideSearchDebounce = null; }
  searchToken++; // 让在途回调全部失效
  $('side-search-input').value = '';
  $('side-search-close').classList.add('hidden');
  $('side-search').classList.add('hidden');
  $('side-search-results').classList.add('hidden');
  $('ws-section').classList.remove('hidden');
  $('session-list').classList.remove('hidden');
  loadSessions();
}

// 逐字高亮：大小写不敏感地标出所有命中（返回带 <mark> 的 HTML）
function hi(text, q) {
  if (!q) return esc(text);
  const low = String(text).toLowerCase();
  const qi = q.toLowerCase();
  let out = '', i = 0;
  for (;;) {
    const at = low.indexOf(qi, i);
    if (at < 0) { out += esc(String(text).slice(i)); break; }
    out += esc(String(text).slice(i, at)) + '<mark>' + esc(String(text).slice(at, at + q.length)) + '</mark>';
    i = at + q.length;
  }
  return out;
}

// 搜索范围跟随侧栏视图：正常工作区只搜正常内容，回收站只搜回收站内容。
// ws = 回收目录列表（默认工作区存空串，被回收时把 "" 一并带上）；
// keep = 已单独恢复的会话（目录还回收着，但对话已回到正常区）。
function searchScopeQS() {
  const recycled = lsGet('jx.wsrecycled', []);
  const wss = (state.defaultWs && recycled.includes(state.defaultWs)) ? [...recycled, ''] : recycled;
  const keep = lsGet('jx.wskeep', []);
  return '&bin=' + (binView ? 1 : 0)
    + '&ws=' + encodeURIComponent(JSON.stringify(wss))
    + '&keep=' + encodeURIComponent(JSON.stringify(keep));
}

// 触发搜索（输入防抖 250ms）：标题路先行渲染，聊天记录路返回后逐条渐现
function runSideSearch(qRaw) {
  const q = qRaw.trim();
  if (searchAbort) { searchAbort.abort(); searchAbort = null; }
  if (searchRevealTimer) { clearInterval(searchRevealTimer); searchRevealTimer = null; }
  const token = ++searchToken;
  $('side-search-close').classList.toggle('hidden', !$('side-search-input').value);
  if (!q) {
    // 清空输入：退出占领态，恢复普通列表
    $('side-search-results').classList.add('hidden');
    $('ws-section').classList.remove('hidden');
    $('session-list').classList.remove('hidden');
    loadSessions();
    return;
  }
  // 搜索结果占领侧栏（标题行隐藏）
  $('side-search-results').classList.remove('hidden');
  $('ws-section').classList.add('hidden');
  $('session-list').classList.add('hidden');

  // 标题路：先占位“搜索中”，结果到达即渲染
  $('ssr-title').innerHTML = '<div class="ssr-empty">搜索中…</div>';
  $('ssr-title-count').textContent = '';
  // 聊天记录路：占位
  $('ssr-content').innerHTML = '<div class="ssr-empty">搜索中…</div>';
  $('ssr-content-count').textContent = '';

  searchAbort = new AbortController();
  const signal = searchAbort.signal;
  const eq = encodeURIComponent(q);

  const scopeQS = searchScopeQS();
  api('/api/search/title?q=' + eq + scopeQS, { signal }).then((d) => {
    if (token !== searchToken) return;
    renderTitleHits(d.hits || [], q);
  }).catch((e) => {
    if (e.name === 'AbortError' || token !== searchToken) return;
    $('ssr-title').innerHTML = '<div class="ssr-empty">搜索失败：' + esc(e.message) + '</div>';
  });

  api('/api/search/content?q=' + eq + scopeQS, { signal }).then((d) => {
    if (token !== searchToken) return;
    revealContentHits(d.hits || [], q, token);
  }).catch((e) => {
    if (e.name === 'AbortError' || token !== searchToken) return;
    $('ssr-content').innerHTML = '<div class="ssr-empty">搜索失败：' + esc(e.message) + '</div>';
  });
}

function renderTitleHits(hits, q) {
  const box = $('ssr-title');
  box.innerHTML = '';
  $('ssr-title-count').textContent = String(hits.length);
  if (!hits.length) {
    box.innerHTML = '<div class="ssr-empty">没有匹配的工作任务</div>';
    return;
  }
  hits.forEach((h) => {
    const b = document.createElement('button');
    b.className = 'ssr-item';
    b.innerHTML =
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>' +
      '<span class="ssr-main">' + hi(h.title || '(未命名)', q) + '</span>';
    b.onclick = () => { closeSideSearch(); openSession(h.sessionId); };
    box.appendChild(b);
  });
}

// 聊天记录命中逐条渐现（约 70ms 一条），关闭/换词即停
function revealContentHits(hits, q, token) {
  const box = $('ssr-content');
  box.innerHTML = '';
  if (!hits.length) {
    $('ssr-content-count').textContent = '0';
    box.innerHTML = '<div class="ssr-empty">没有匹配的聊天记录</div>';
    return;
  }
  let i = 0;
  searchRevealTimer = setInterval(() => {
    if (token !== searchToken) { clearInterval(searchRevealTimer); searchRevealTimer = null; return; }
    if (i >= hits.length) {
      clearInterval(searchRevealTimer);
      searchRevealTimer = null;
      $('ssr-content-count').textContent = String(hits.length);
      return;
    }
    box.appendChild(contentHitItem(hits[i++], q));
  }, 70);
}

function contentHitItem(h, q) {
  const b = document.createElement('button');
  b.className = 'ssr-item ssr-msg';
  b.innerHTML =
    '<span class="ssr-session">' + esc(h.title || '(未命名)') + ' · ' + (h.role === 'user' ? '用户' : '助手') + '</span>' +
    '<span class="ssr-main">' + hi(h.snippet || '', q) + '</span>';
  b.onclick = () => jumpToMessage(h.sessionId, h.seq);
  return b;
}

// 跳到会话并定位到命中消息：滚动到视野中心并闪烁高亮
async function jumpToMessage(sessionId, seq) {
  closeSideSearch();
  if (!state.cur || state.cur.id !== sessionId) {
    await openSession(sessionId);
  }
  // 分批加载：命中可能落在未加载的更早批次——先一批批补齐到覆盖它（或到头）
  let guard = 0;
  while (state.older && state.older.count > 0 && !document.querySelector('[data-seq="' + seq + '"]')) {
    if (!(await loadOlder(mainView)) || ++guard > 20) break; // 失败/到头/保险上限即停
  }
  // 用 setTimeout 而非 rAF：页面在后台时 rAF 会被暂停，定位就丢了
  setTimeout(() => {
    const el = document.querySelector('[data-seq="' + seq + '"]');
    if (!el) return;
    // 目标在折叠段/收起的过程行里：先展开再定位
    const sec = el.closest ? el.closest('.tf-sec') : null;
    if (sec) { setSecOpen(sec, true); if (sec.dataset.key) turnOpen[sec.dataset.key] = true; }
    const row = el.closest ? el.closest('.proc-row') : null;
    if (row) setProcOpen(row, true);
    el.scrollIntoView({ block: 'center' });
    el.classList.add('search-flash');
    setTimeout(() => el.classList.remove('search-flash'), 2400);
  }, 60);
}

// 排序选项菜单：上半区「分组」（按工作区/时间线/单列表），下半区「排序」。
// 选中项左侧亮对勾，仿系统设置的分区单选菜单。
// 再点一次排序按钮 = 收起（.ws-tools 在外点关闭的豁免名单里，按钮点击不会
// 触发文档级收起，切换在这里自管）；其他弹出卡开着则照常替换成本菜单。
function openViewMenu(anchor) {
  const m = ensureCtxMenu();
  if (!m.classList.contains('hidden') && m.querySelector('.vo-item')) {
    closePopovers();
    return;
  }
  const opt = (key, val, label) => ({ key, val, label, on: viewCfg[key] === val });
  const groups = [
    { title: '分组', opts: [
      opt('group', 'time', '时间线'),
      opt('group', 'ws', '按工作区'),
      opt('group', 'flat', '单列表'),
    ] },
    { title: '排序', opts: [
      opt('sort', 'updated', '最近更新'),
      opt('sort', 'created', '最新创建'),
    ] },
  ];
  const check = '<svg class="vo-check" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round"><path d="M4 12l5 5L20 7"/></svg>';
  let html = '';
  groups.forEach((g) => {
    html += `<div class="pop-group">${g.title}</div>`;
    g.opts.forEach((o) => {
      html += `<button class="pop-item vo-item${o.on ? ' on' : ''}" data-k="${o.key}" data-v="${o.val}">${o.on ? check : '<span class="vo-check"></span>'}${o.label}</button>`;
    });
  });
  m.innerHTML = html;
  closePopovers();
  m.classList.remove('hidden');
  const r = anchor.getBoundingClientRect();
  m.style.left = Math.max(8, Math.min(r.right - 170, window.innerWidth - m.offsetWidth - 8)) + 'px';
  m.style.top = Math.min(r.bottom + 6, window.innerHeight - m.offsetHeight - 8) + 'px';
  m.querySelectorAll('.vo-item').forEach((b) => {
    b.onclick = (e) => {
      e.stopPropagation();
      viewCfg[b.dataset.k] = b.dataset.v;
      lsSet('jx.view', viewCfg);
      closePopovers();
      loadSessions();
    };
  });
}

// 调后端弹出系统原生「选择文件夹」对话框，返回所选目录绝对路径；取消返回空串。
// 浏览器自身的目录选择 API 不回传绝对路径，而本服务就跑在用户本机上，
// 由后端代为弹框既能拿到真实路径，体验也与系统/浏览器一致。
// 窄屏（手机远程访问）不支持：原生对话框弹在宿主机屏幕上，手机端永远等不到——
// 点了只弹轻提示告知去电脑上操作（守卫在 addWorkFolder）。
let pickingFolder = false;
async function pickWorkFolder() {
  if (pickingFolder) return '';
  pickingFolder = true;
  try {
    const start = state.defaultWs ? '?start=' + encodeURIComponent(state.defaultWs) : '';
    const res = await api('/api/pick-folder' + start);
    return (res && res.path) || '';
  } catch (e) {
    alert('无法打开文件夹选择框：' + (e.message || e));
    return '';
  } finally {
    pickingFolder = false;
  }
}

// 把目录登记为已知工作区（localStorage），侧栏分组与选择面板都会出现它
function rememberWorkspace(ws) {
  if (!ws) return;
  lsSet('jx.wslist', [...new Set([...lsGet('jx.wslist', []), ws])]);
  const h = lsGet('jx.wshidden', []);
  if (h.includes(ws)) lsSet('jx.wshidden', h.filter((x) => x !== ws)); // 手动重新添加 = 取消拉黑
}

// 侧栏「添加工作目录」：弹选择框并登记。
// 窄屏不支持新增（原生选择框在宿主机上弹，手机端派不上用场）：
// 与排序工具同款——从按钮向下弹出告知卡，再点一下工具或点其他区域即收起
async function addWorkFolder() {
  if (isNarrow()) {
    toggleWsAddNotice($('wst-add'));
    return;
  }
  const p = await pickWorkFolder();
  if (!p) return;
  rememberWorkspace(p);
  loadSessions();
}

// 窄屏「添加工作目录」的告知卡：复用排序菜单同一张弹出卡（#ctx-menu）。
// 收起的两条路：① 按钮二连击在这里自己收（.ws-tools 在外点关闭的豁免名单里，
// 按钮点击不会触发文档级收起）；② 点卡片外其他区域/点卡片本身由既有的
// 文档级 closePopovers 与下方 onclick 负责。
function toggleWsAddNotice(anchor) {
  const m = ensureCtxMenu();
  if (!m.classList.contains('hidden') && m.querySelector('.pop-notice')) {
    closePopovers(); // 已开着这张告知卡：再点一下工具 = 收起
    return;
  }
  m.innerHTML = '<div class="pop-notice">' + NARROW_WS_ADD_NOTICE + '</div>';
  m.querySelector('.pop-notice').onclick = () => closePopovers();
  closePopovers();
  m.classList.remove('hidden');
  const r = anchor.getBoundingClientRect();
  m.style.left = Math.max(8, Math.min(r.left, window.innerWidth - m.offsetWidth - 8)) + 'px';
  m.style.top = Math.min(r.bottom + 6, window.innerHeight - m.offsetHeight - 8) + 'px';
}

// 右键/三个点共用的上下文菜单（动态 popover）
function ensureCtxMenu() {
  let m = document.getElementById('ctx-menu');
  if (!m) {
    m = document.createElement('div');
    m.id = 'ctx-menu';
    m.className = 'popover ctx-menu hidden';
    document.body.appendChild(m);
  }
  return m;
}
function openCtxMenu(items, x, y) {
  const m = ensureCtxMenu();
  m.innerHTML = '';
  items.forEach((it) => {
    // {sep:true} 画分隔线；{disabled:true} 置灰不可点（桌面壳右键菜单用）
    if (it.sep) {
      const s = document.createElement('div');
      s.className = 'ctx-sep';
      m.appendChild(s);
      return;
    }
    const b = document.createElement('button');
    b.className = 'pop-item' + (it.danger ? ' danger' : '');
    b.textContent = it.label;
    b.disabled = !!it.disabled;
    b.onclick = (e) => { e.stopPropagation(); closePopovers(); it.fn(); };
    m.appendChild(b);
  });
  closePopovers();
  m.classList.remove('hidden');
  const mw = m.offsetWidth, mh = m.offsetHeight;
  m.style.left = Math.max(8, Math.min(x, window.innerWidth - mw - 8)) + 'px';
  m.style.top = Math.max(8, Math.min(y, window.innerHeight - mh - 8)) + 'px';
}

async function deleteSession(s) {
  if (!(await confirmCenter('删除该会话？此操作不可恢复。'))) return;
  await api('/api/sessions/' + s.id, { method: 'DELETE' });
  removeSessionLocalData(s.id); // 彻底删除连带清掉它的本地草稿/排队缓存
  if (state.cur && state.cur.id === s.id) {
    state.cur = null;
    state.msgs = [];
    resetEmpty();
  }
  loadSessions();
}

// 会话项的朗读口径：标题 + 所在工作区名（左栏这一行能看到的就这两样；时间与
// 操作按钮不念）。itemEl 若真有行内选区，仍以选区为准（speechSelectionIn）。
function sessionSpeechText(s) {
  const ws = wsOf(s);
  const name = lsGet('jx.wsalias', {})[ws] || baseName(ws) || '默认工作区';
  return (s.title || '(未命名)') + '，工作区 ' + name;
}

// 工作区分组头的朗读口径：显示名 + 目录路径（头里只显示名字，路径在 tooltip 上；
// 用户念它是想知道这组是哪个目录）
function workspaceSpeechText(ws) {
  const name = lsGet('jx.wsalias', {})[ws] || baseName(ws) || '默认工作区';
  return '工作区 ' + name + (ws ? '，路径 ' + ws : '');
}

function openSessionMenu(s, rect, itemEl) {
  const speak = ctxSpeakItems({ kind: 'session', text: speechSelectionIn(itemEl) || sessionSpeechText(s) });
  // 回收站视图里的会话菜单：恢复 / 重命名 / 彻底删除
  if (binView) {
    openCtxMenu([
      { label: '恢复', fn: () => restoreSession(s) },
      { label: '重命名', fn: async () => {
        const t = prompt('重命名会话', s.title || '');
        if (t === null || !t.trim()) return;
        await api('/api/sessions/' + s.id, { method: 'PATCH', body: JSON.stringify({ title: t.trim() }) });
        loadSessions();
      } },
      { label: '彻底删除', danger: true, fn: () => deleteSession(s) },
      ...speak,
    ], rect.right - 8, rect.bottom + 4);
    return;
  }
  openCtxMenu([
    { label: '重命名', fn: async () => {
      const t = prompt('重命名会话', s.title || '');
      if (t === null || !t.trim()) return;
      await api('/api/sessions/' + s.id, { method: 'PATCH', body: JSON.stringify({ title: t.trim() }) });
      loadSessions();
    } },
    // 手动改过名（钉住）的会话恢复 AI 自动命名：入口贴着使用场景放会话菜单，
    // 且仅在该组件启用中且确实钉住时出现（组件关掉=整套自动命名停转，恢复无意义）
    ...((window.CX && CX.isActive('session-title') && s.titlePinned) ? [{
      label: '恢复自动命名', fn: async () => {
        await api('/api/sessions/' + s.id, { method: 'PATCH', body: JSON.stringify({ autoTitleUnpin: true }) });
        loadSessions();
      } }] : []),
    { label: s.unread ? '取消未读' : '标为未读', fn: async () => {
      await api('/api/sessions/' + s.id, { method: 'PATCH', body: JSON.stringify({ unread: !s.unread }) });
      loadSessions();
    } },
    { label: '分支对话', fn: async () => {
      try {
        const fork = await api('/api/sessions/' + s.id + '/fork', { method: 'POST' });
        upsertSessionBrief(fork); // 先合入再切换：不等防抖往返，乐观切换必命中（见 app-core.js）
        loadSessions();
        openSession(fork.id);
      } catch (e) { alert('分支失败：' + e.message); }
    } },
    { label: '移入回收站', danger: true, fn: async () => {
      await api('/api/sessions/' + s.id, { method: 'PATCH', body: JSON.stringify({ archived: true }) });
      lsSet('jx.wskeep', lsGet('jx.wskeep', []).filter((x) => x !== s.id)); // 归档即离开"单独保留"，保证恢复往返一致
      if (state.cur && state.cur.id === s.id) { state.cur = null; state.msgs = []; resetEmpty(); }
      loadSessions();
    } },
    ...speak,
  ], rect.right - 8, rect.bottom + 4);
}

// 「进行态」标记的完整定义：待确认、运行中与已中断共用同一个结构、同一套 class
// 与 title 写法，只有字符、颜色类与悬停文案不同——所以几个标记天然同位同款，
// 改一个是改全部。已中断的类型码由服务端给（Store.SetLastStop），话术留在前端；
// 打开会话不清（「最近一轮没跑完」这个事实没变），重跑一轮或重试成功即自动消失。
const STATUS_CELLS = {
  wait: { cls: 'wait', text: '⏸ 待确认', tip: '这轮停在等你确认（审批／提问／计划），点开对话处理即继续' },
  running: { cls: 'running', text: '⟳ 运行中', tip: '正在执行（切走不影响，切回来会接上）' },
  // 主对话自己没跑、名下有子对话在跑：同一格同一款转圈，悬停说明是谁在执行
  subrun: { cls: 'running', text: '⟳ 运行中', tip: '名下有子对话正在执行（打开对话在右栏查看；切走不影响）' },
  step: { cls: 'stop', text: '⚠ 已中断', tip: '已达最大步数自动停止（可在设置里调大「最大步数」），可点开重试' },
  error: { cls: 'stop', text: '⚠ 已中断', tip: '本轮执行出错中断（模型／网络／网关等），点开可看原因并重试' },
  restart: { cls: 'stop', text: '⚠ 已中断', tip: '服务重启导致本轮中断，可发「继续」接续' },
};

// 这个会话是否挂着等用户操作的卡（越权审批／提问／计划审批）。权威清单就是
// state.approvals / state.questions——启动时拉取、事件流实时更新、含重启后仍在
// 等的待审批计划——直接查它们，不另维护事件驱动的影子表（那种表刷新后会丢、
// 且提问/计划本来就不覆盖）。
function sessionPendingAsk(id) {
  if (!id) return false;
  return (state.approvals || []).some((a) => a.sessionId === id)
    || (state.questions || []).some((q) => q.sessionId === id);
}

// 会话状态标记（行末时间那一格）：待确认 > 运行中都从这一个函数取，
// 没有标记则返回空串、由调用方回落相对时间。这里只出字符与颜色类——位置、字号、
// 悬停行为全部交给 .s-time 那套公共规则，别给某个状态单独写位置规则（写歪了几个
// 状态就对不齐）。「子对话运行中」：顶层会话自己空闲但名下有子对话在跑——用户
// 在侧栏一眼看出那个主对话有动静（数据源全量 state.sessions，some 短路够快）。
function sessionStatusCell(s) {
  // 名下有子代理在跑：展开时用「⟳ 运行中」这一格，收起时改由标题后的 ⟳N 计数胶囊
  // 表达（subRunChip）——同一行只留一处运行指示，不叠两套
  const kidRun = !s.parentId && subTreeOf(s, state.sessions, subOpen).runN > 0;
  const chipShown = kidRun && subOpen.has(s.id);
  const key = sessionPendingAsk(s.id) ? 'wait' : s.status === 'running' ? 'running' : (kidRun && !chipShown) ? 'subrun' : s.lastStop;
  const st = STATUS_CELLS[key];
  return st ? `<span class="s-time ${st.cls}" title="${esc(st.tip)}">${st.text}</span>` : '';
}

function sessionItem(s) {
  const item = document.createElement('div');
  item.className = 'session-item' + (state.cur && state.cur.id === s.id ? ' active' : '');
  item.dataset.sid = s.id; // 方向键快捷键按「此刻列表 DOM」切换对话时取 id（app-hotkeys.js）
  // 圆点：平时不显示；橙=待确认（审批/提问/计划），绿=完成未查看，红=标为未读
  let dot = '';
  if (sessionPendingAsk(s.id)) dot = ' ap';
  else if (dotDone[s.id]) dot = ' done';
  else if (s.unread) dot = ' unread';
  // 有卡等用户操作的会话：时间位换成橙色「⏸ 待确认」，优先级最高——用户不
  // 处理这轮就一直停着，得让人一眼看出是哪个对话在等（含重启后遗留的待审批
  // 计划：会话不在跑也一样标）
  // 正在执行的会话（含切走后仍在后台跑的那一轮）：时间位换成「⟳ 运行中」，
  // 让用户知道切走不等于停掉
  // 异常中断的会话（步数上限／执行失败／服务重启）：同一格换成「⚠ 已中断」——
  // 三个标记都出自 sessionStatusCell，字符与颜色不同、位置与悬停行为完全一致
  //（已中断的打开会话不清，重跑一轮即消失）
  const statusCell = sessionStatusCell(s);
  const timeCell = statusCell || `<span class="s-time">${esc(relTime(s.updatedAt))}</span>`;
  // 时间线视图：条目脱离了工作区分组的语境，行首补一枚工作目录小胶囊
  //（取目录名开头两三个字，塞不下的从右缘直接裁掉——不用省略号，固定宽一列对齐）
  let wsChip = '';
  if (viewCfg.group === 'time' && !binView) {
    const ws = wsOf(s);
    const name = lsGet('jx.wsalias', {})[ws] || baseName(ws) || '默认工作区';
    wsChip = `<span class="s-ws" title="${esc(ws ? name + '（' + ws + '）' : '默认工作区')}">${esc(name)}</span>`;
  }
  // 分支标记（A2）：标题以「（分支）」结尾时拆出主标题、把后缀渲染成不收缩的
  // 小胶囊——.s-title 单行省略会把后缀吞掉，源会话与分支在列表里几乎不可分
  //（切回时点错行、消息发错会话的温床）。胶囊 flex:none，永远可见。
  const rawTitle = String(s.title || '(未命名)');
  const isFork = rawTitle.endsWith('（分支）');
  const mainTitle = isFork ? rawTitle.slice(0, -4) : rawTitle;
  // 子对话折叠（见上方「会话行内子对话折叠」）：有子会话的行出箭头，没有的放
  // 同宽占位（visibility:hidden），父/非父行的标题左缘才对得齐一列；箭头只切
  // 展开态，跟会话本身无关
  const tree = subTreeOf(s, state.sessions, subOpen);
  const chev = tree.has
    ? `<button class="s-chev${tree.open ? ' open' : ''}" title="${tree.open ? '收起子对话' : '展开子对话'}"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M9 6l6 6-6 6"/></svg></button>`
    : `<span class="s-chev none"></span>`;
  // 收起态的运行中子代理数量（展开后明细就在下面，subRunChip 自己收回）
  const kidChip = subRunChip(tree)
    ? `<span class="s-kidrun" title="名下 ${tree.runN} 个子代理正在执行（点标题左侧箭头展开看明细）">${subRunChip(tree)}</span>`
    : '';
  item.innerHTML =
    wsChip +
    `<span class="dot${dot}"></span>` +
    chev +
    `<span class="s-title" title="${esc(rawTitle)}">${esc(mainTitle)}</span>` +
    (isFork ? `<span class="s-fork" title="从「${esc(mainTitle)}」分支出">分支</span>` : '') +
    kidChip +
    timeCell +
    `<span class="row-acts">` +
      (binView ? `<button class="ra-restore" title="恢复"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v5h5"/></svg></button>` : '') +
      `<button class="ra-copy" title="复制标题"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg></button>` +
      `<button class="ra-more" title="更多"><svg viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/></svg></button>` +
    `</span>`;
  item.onclick = (e) => {
    if (e.target.closest('.row-acts')) return;
    openSession(s.id);
  };
  // 箭头单独接 click 并阻止冒泡：展开/收起绝不顺手把当前对话切走
  const chevBtn = item.querySelector('button.s-chev');
  if (chevBtn) chevBtn.onclick = (e) => { e.stopPropagation(); toggleSubOpen(s.id); };
  item.querySelector('.ra-copy').onclick = (e) => {
    e.stopPropagation();
    navigator.clipboard.writeText(s.title || '(未命名)');
    const b = e.currentTarget;
    b.classList.add('ok'); setTimeout(() => b.classList.remove('ok'), 900);
  };
  const raRestore = item.querySelector('.ra-restore');
  if (raRestore) raRestore.onclick = (e) => { e.stopPropagation(); restoreSession(s); };
  item.querySelector('.ra-more').onclick = (e) => {
    e.stopPropagation();
    openSessionMenu(s, item.getBoundingClientRect(), item);
  };
  item.oncontextmenu = (e) => { e.preventDefault(); openSessionMenu(s, item.getBoundingClientRect(), item); };
  return item;
}

/* ---------- 左侧栏：会话行内子对话折叠 ----------
   问题：子会话（parent_id 非空）不进顶层列表（renderSessionList 的 visible 过滤），
   用户不点开右栏就看不出「哪个主对话名下还有子代理在跑、跑到哪」。
   做法：父行标题左侧一枚折叠箭头，只切展开态（不切当前对话），展开后在父行下
   缩进列出该父名下全部未归档子会话（不只运行中：干完的也留着，方便回看），
   运行中的子行带 ⟳ 标记；收起时父行标题后给「运行中的子代理数量」⟳N，展开后
   明细替代计数——同一行永远只有一处运行指示，不叠两套展示。
   数据全部来自 state.sessions（GET /api/sessions 已返回含 parentId/status 的全量
   会话），不新增接口；展开态存 subOpen（内存），列表刷新后保留，刷新页面回默认收起。 */

const subOpen = new Set(); // 已展开的父会话 id（内存态：默认收起，刷新页面即回默认）
const SUB_MAX_DEPTH = 3;   // 缩进视觉上限：多级子会话按 parent 链平铺，再深也按 3 级缩

// 直系子会话：未归档（进了回收站的那些各自在回收站视图）、按更新时间倒序
function subKidsOf(sid, sessions) {
  return (sessions || [])
    .filter((k) => k.parentId === sid && !k.archived)
    .sort((a, b) => String(b.updatedAt || '').localeCompare(String(a.updatedAt || '')));
}

// 名下所有层级的子会话（平铺，带 depth 供缩进）：今天派发工具只允许一级，这里
// 仍按 parent 链递归——将来放开多级，展示不用另写一套
function subDescendants(sid, sessions, depth) {
  const out = [];
  const d = depth || 1;
  if (d > SUB_MAX_DEPTH + 2) return out; // 防脏数据成环
  for (const k of subKidsOf(sid, sessions)) {
    out.push({ id: k.id, title: k.title || '(未命名)', updatedAt: k.updatedAt || '',
      parentId: sid, depth: Math.min(d, SUB_MAX_DEPTH), running: k.status === 'running' });
    for (const g of subDescendants(k.id, sessions, d + 1)) out.push(g);
  }
  return out;
}

// 折叠模型：该不该出箭头、是否展开、运行中的子代理条数、展开时的子行清单
function subTreeOf(s, sessions, open) {
  if (!s) return { has: false, open: false, runN: 0, rows: [] };
  const kids = subKidsOf(s.id, sessions);
  const all = kids.length ? subDescendants(s.id, sessions, 1) : [];
  const isOpen = !!(open && open.has(s.id));
  return { has: kids.length > 0, open: isOpen,
    runN: all.filter((r) => r.running).length,
    rows: isOpen ? all : [] };
}

// 收起态才给计数胶囊：展开时明细就在下面，两处同时出现＝同一事实显示两遍
function subRunChip(tree) {
  return (!tree.open && tree.runN > 0) ? '⟳' + tree.runN : '';
}

// 箭头点击：只切展开态并就地重绘（父行的点击＝打开对话，语义保持不变）
function toggleSubOpen(sid) {
  if (subOpen.has(sid)) subOpen.delete(sid); else subOpen.add(sid);
  renderSessionList();
}

// 子行内容（纯字符串拼装：与其它行的 innerHTML 拼法一致，标题一律转义）
function subRowHTML(row) {
  const run = row.running ? '<span class="s-run" title="正在执行（⟳ 运行中）">⟳</span>' : '';
  return `<span class="s-title">${esc(row.title)}</span>` + run +
    `<span class="s-time">${esc(relTime(row.updatedAt))}</span>`;
}

function subRowItem(row) {
  const el = document.createElement('div');
  el.className = 'session-item s-child' + (row.running ? ' running' : '');
  el.dataset.sid = row.id;
  el.dataset.depth = String(row.depth);
  el.innerHTML = subRowHTML(row);
  el.onclick = () => openSubRow(row.id);
  return el;
}

// 子行点击：先落到直系父会话（已经在那儿就不重复加载），再让右栏定位到这个子会话
function openSubRow(id) {
  const s = (state.sessions || []).find((x) => x.id === id);
  if (!s) return;
  const pid = s.parentId || '';
  if (pid && (!state.cur || state.cur.id !== pid)) openSession(pid);
  rpOpen('subs');
  openSubSession(id);
}

// 一条会话块＝父行 +（展开时）紧跟其后的缩进子行：平铺/时间线/分组三个视图都
// 经它出条目，展开态在三种视图下表现一致
function sessionBlock(s) {
  const frag = document.createDocumentFragment();
  frag.appendChild(sessionItem(s));
  for (const row of subTreeOf(s, state.sessions, subOpen).rows) frag.appendChild(subRowItem(row));
  return frag;
}

/* ---------- 回收站视图：点底部「回收站」，整个工作区列表切成回收站 ----------
   回收站内容 = 已移入回收站的工作目录（jx.wsrecycled）+ 已归档会话（archived=1）
   以及挂在回收目录下的对话（它们在正常视图里被整组隐藏）。 */

let binView = false;
let binAnimTimer = 0; // 入场动画类的摘除定时器（见 setBinView）

function setBinView(on) {
  binView = !!on;
  if (sideSearchOpen()) closeSideSearch(); // 切换视图前先还原搜索占领态（结果范围已随视图变化）
  $('btn-recycle').classList.toggle('active', binView);
  document.body.classList.toggle('bin-mode', binView);
  // 边框「从下往上」入场动画：0.22s，只有缓入没有缓出（.6,0,1,1：
  // 前半段放缓蓄力、一路加速窜上、到顶即停），
  // 进入时挂 0.35s 动画类，随后摘除、回落到静态红边
  // （动画正常则无缝，时钟冻结也不至于红边一直不可见）
  clearTimeout(binAnimTimer);
  if (binView) {
    document.body.classList.add('bin-anim');
    binAnimTimer = setTimeout(() => document.body.classList.remove('bin-anim'), 350);
  } else {
    document.body.classList.remove('bin-anim');
  }
  $('ws-section-title').textContent = binView ? '回收站' : '工作任务';
  $('side-search-input').placeholder = binView ? '搜索回收站' : '搜索工作任务';
  $('wst-search').title = binView ? '搜索回收站' : '搜索工作任务';
  loadSessions();
}

function restoreWorkspace(ws) {
  lsSet('jx.wsrecycled', lsGet('jx.wsrecycled', []).filter((x) => x !== ws));
  // 目录整体恢复时清掉该目录下会话的"单独保留"名单，下次回收从干净状态开始
  const ids = new Set((state.sessions || []).filter((s) => wsOf(s) === ws).map((s) => s.id));
  if (ids.size) lsSet('jx.wskeep', lsGet('jx.wskeep', []).filter((x) => !ids.has(x)));
  loadSessions();
}

// 回收站里恢复单个对话：已归档的取消归档；随目录回收的加入保留名单（正常视图单独可见）
async function restoreSession(s) {
  const wsRecycled = lsGet('jx.wsrecycled', []).includes(wsOf(s));
  if (s.archived) await api('/api/sessions/' + s.id, { method: 'PATCH', body: JSON.stringify({ archived: false }) });
  if (wsRecycled) lsSet('jx.wskeep', [...new Set([...lsGet('jx.wskeep', []), s.id])]);
  loadSessions();
  refreshDrawer(); // 恢复的可能是子对话：右栏下拉清单立刻回来（无父可挂时自短路）
}

// 彻底删除工作目录：删除其名下全部会话记录、注销登记并拉黑（不碰磁盘文件夹）
async function deleteWorkspace(ws) {
  const name = lsGet('jx.wsalias', {})[ws] || baseName(ws) || '默认工作区';
  const targets = (state.sessions || []).filter((s) => s.mode !== 'assistant' && wsOf(s) === ws);
  if (!(await confirmCenter(`彻底删除工作目录「${name}」及其 ${targets.length} 个对话？\n只从应用中移除记录，不会删除磁盘上的文件夹。默认工作区路径来自配置，删除后也不会再显示空目录，直到你重新添加它。\n此操作不可恢复。`))) return;
  if (!(await confirmCenter(`再次确认：目录「${name}」及其全部对话将被彻底删除、无法恢复。\n确定要继续吗？`, false, true))) return;
  for (const s of targets) {
    try { await api('/api/sessions/' + s.id, { method: 'DELETE' }); } catch (e) { /* 已不存在则跳过 */ }
    removeSessionLocalData(s.id);
  }
  unregisterWorkspace(ws);
  hideWorkspace(ws);
  lsSet('jx.wskeep', lsGet('jx.wskeep', []).filter((x) => !targets.some((t) => t.id === x)));
  if (state.cur && targets.some((t) => t.id === state.cur.id)) { state.cur = null; state.msgs = []; resetEmpty(); }
  loadSessions();
}

// 拉黑一个目录：即使它是默认工作区（配置无法注销）也不再出现在列表，
// 直到被手动重新添加、或名下重新有了会话（会话建组不查黑名单，天然复活）
function hideWorkspace(ws) {
  lsSet('jx.wshidden', [...new Set([...lsGet('jx.wshidden', []), ws])]);
}

// 注销一个工作目录的全部本地登记（列表/回收站/别名/折叠状态）
function unregisterWorkspace(ws) {
  lsSet('jx.wslist', lsGet('jx.wslist', []).filter((x) => x !== ws));
  lsSet('jx.wsrecycled', lsGet('jx.wsrecycled', []).filter((x) => x !== ws));
  const am = lsGet('jx.wsalias', {}); delete am[ws]; lsSet('jx.wsalias', am);
  const cm = lsGet('jx.wscol', {}); delete cm[ws]; lsSet('jx.wscol', cm);
}

// 清空回收站的目标集合：回收站内的全部顶层对话 + 它们的子会话，以及单独
// 归档的子对话（父对话还在正常区、只有子对话进了回收站——renderBin 同口径）
// （keep 保留的、正常区的都不算；目录由回收列表单独注销）
function collectEmptyTargets() {
  const keep = new Set(lsGet('jx.wskeep', []));
  const recycledWs = lsGet('jx.wsrecycled', []);
  const tops = (state.sessions || []).filter((s) =>
    s.mode !== 'assistant' && !s.parentId && !keep.has(s.id) &&
    (s.archived || recycledWs.includes(wsOf(s))));
  const topIds = new Set(tops.map((s) => s.id));
  const targets = (state.sessions || []).filter((s) =>
    topIds.has(s.id) || (s.parentId && topIds.has(s.parentId))
    || (s.parentId && s.archived && !topIds.has(s.parentId)));
  return { tops, targets, recycledWs };
}

// 清空回收站：删除全部回收内容并注销全部回收目录。
// 双级确认——比单条「彻底删除对话」多一道确认。
async function emptyRecycleBin() {
  const { tops, targets, recycledWs } = collectEmptyTargets();
  if (!tops.length && !recycledWs.length) { await confirmCenter('回收站是空的', true); return; }
  if (!(await confirmCenter(`清空回收站？将永久删除 ${recycledWs.length} 个工作目录登记和 ${tops.length} 条对话。\n只从应用中移除记录，不会删除磁盘上的文件夹。`))) return;
  if (!(await confirmCenter('再次确认：整个回收站将被清空，所有内容彻底删除、无法恢复。\n确定要继续吗？', false, true))) return;
  for (const s of targets) {
    try { await api('/api/sessions/' + s.id, { method: 'DELETE' }); } catch (e) { /* 已不存在则跳过 */ }
    removeSessionLocalData(s.id);
  }
  for (const ws of recycledWs) { unregisterWorkspace(ws); hideWorkspace(ws); }
  lsSet('jx.wskeep', []); // 目录登记都注销了，保留名单随之失效
  if (state.cur && targets.some((t) => t.id === state.cur.id)) { state.cur = null; state.msgs = []; resetEmpty(); }
  loadSessions();
}

function renderBin(box) {
  const aliasMap = lsGet('jx.wsalias', {});
  const recycledWs = lsGet('jx.wsrecycled', []);
  const sortIt = (arr) => viewCfg.sort === 'created'
    ? [...arr].sort((a, b) => String(b.createdAt || '').localeCompare(String(a.createdAt || '')))
    : arr;
  // 回收站里的对话：已归档的、或挂在回收目录下且未被单独恢复的。
  // 单独归档的子对话（父对话还在正常区）同样进回收站；父也归档的不单独列，
  // 随父整组恢复与清空（collectEmptyTargets 的连坐删除同口径）
  const keep = new Set(lsGet('jx.wskeep', []));
  const archivedIds = new Set((state.sessions || []).filter((x) => x.archived).map((x) => x.id));
  const all = (state.sessions || []).filter((s) =>
    s.mode !== 'assistant' && (!s.parentId || (s.archived && !archivedIds.has(s.parentId))));
  const inBin = all.filter((s) => !keep.has(s.id) && (s.archived || recycledWs.includes(wsOf(s))));
  if (!recycledWs.length && !inBin.length) {
    box.innerHTML = '<div class="ws-empty">回收站是空的</div>';
    return;
  }
  // 回收站里的子对话条目：标题前加「子对话」来源标记（复用时间线视图的工作区
  // 小胶囊样式），其余与顶层会话完全同款（恢复/重命名/彻底删除同一套菜单）
  const binItem = (s) => {
    const el = sessionItem(s);
    if (s.parentId) {
      const tag = document.createElement('span');
      tag.className = 's-ws';
      tag.textContent = '子对话';
      tag.title = '挂在别的对话名下的子会话；恢复后回到原对话右栏的子对话列表';
      const title = el.querySelector('.s-title');
      if (title) title.parentNode.insertBefore(tag, title);
    }
    return el;
  };
  if (recycledWs.length) {
    const label = document.createElement('div');
    label.className = 'bin-label';
    label.textContent = '工作目录';
    box.appendChild(label);
  }
  for (const ws of recycledWs) {
    const items = sortIt(inBin.filter((s) => wsOf(s) === ws));
    const group = document.createElement('div');
    group.className = 'ws-group';
    const collapsed = !!wsCollapsed[ws];
    const head = document.createElement('div');
    head.className = 'ws-group-head bin' + (collapsed ? ' collapsed' : '');
    head.title = ws || '（未设置，使用默认工作区）';
    head.innerHTML =
      `<svg class="chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M9 6l6 6-6 6"/></svg>` +
      `<svg class="folder" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>` +
      `<span class="ws-name">${esc(aliasMap[ws] || baseName(ws) || '默认工作区')}</span>` +
      `<span class="ws-count">${items.length}</span>` +
      `<span class="ws-acts">` +
        `<button class="wa-restore" title="恢复到工作区"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12a9 9 0 1 0 3-6.7"/><path d="M3 4v5h5"/></svg></button>` +
        `<button class="wa-more" title="工作区操作"><svg viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/></svg></button>` +
      `</span>`;
    head.onclick = (e) => {
      if (e.target.closest('.ws-acts')) return;
      wsCollapsed[ws] = !collapsed;
      lsSet('jx.wscol', wsCollapsed);
      loadSessions();
    };
    head.querySelector('.wa-restore').onclick = (e) => {
      e.stopPropagation();
      restoreWorkspace(ws);
    };
    head.querySelector('.wa-more').onclick = (e) => {
      e.stopPropagation();
      const r = e.currentTarget.getBoundingClientRect();
      openCtxMenu([
        { label: '复制路径', fn: () => navigator.clipboard.writeText(ws) },
        { label: '彻底删除目录', danger: true, fn: () => deleteWorkspace(ws) },
        ...ctxSpeakItems({ kind: 'workspace', text: speechSelectionIn(head) || workspaceSpeechText(ws) }),
      ], r.left, r.bottom + 4);
    };
    head.oncontextmenu = (e) => {
      e.preventDefault();
      head.querySelector('.wa-more').click();
    };
    group.appendChild(head);
    if (!collapsed) {
      const list = document.createElement('div');
      list.className = 'ws-group-list';
      for (const s of items) list.appendChild(binItem(s));
      if (!items.length) list.innerHTML = '<div class="ws-empty">（无对话）</div>';
      group.appendChild(list);
    }
    box.appendChild(group);
  }
  // 单独回收的对话：已归档、但其目录没被回收的
  const flat = sortIt(inBin.filter((s) => !recycledWs.includes(wsOf(s))));
  if (flat.length) {
    const label = document.createElement('div');
    label.className = 'bin-label';
    label.textContent = '对话';
    box.appendChild(label);
    for (const s of flat) box.appendChild(binItem(s));
  }
}

async function loadSessionsNow() {
  const data = await api('/api/sessions');
  state.sessions = data.sessions || [];
  // 工作区分组顺序：本次启动只在这里算一次（快照语义见下方 wsOrderSnapshot 注释）
  if (!wsOrderSnapshot) wsOrderSnapshot = snapshotWsOrder();
  renderSessionList();
}

/* ---------- 工作区分组顺序：本次启动只算一次，运行期不重排 ----------
   分组（工作目录）之间的顺序按「该目录里最新的那一个会话的时间」降序——哪个目录里
   有最新的会话，它就排得更上面。
   只在软件启动时算一次：第一份全量会话列表（/api/sessions 一次返回全部会话）到达时
   算出顺序快照 wsOrderSnapshot 存起来；此后 renderSessionList 每次只是照着这份快照
   摆位——新建会话 / 发消息 / 归档 / 重跑等任何数据变化都不会再算，顺序在本次启动期间
   固定不动；下次启动（重新加载界面）才会有新快照。 */

// 目录排序键所看的会话时间：updatedAt（最后活动时间），缺失或异常时退回 createdAt，
// 两者都不可用记 0。与左侧栏默认排序（后端 /api/sessions 按 updated_at DESC 出）和
// 「时间线」视图分带用的是同一个字段——目录顺序与用户看到的「最新对话」同一把尺子。
function sessionSortStamp(s) {
  for (const raw of [s.updatedAt, s.createdAt]) {
    const t = new Date(String(raw || '').replace(' ', 'T')).getTime();
    if (!isNaN(t)) return t;
  }
  return 0;
}

// 计入排序键的会话：属于该目录的全部会话，含子对话 / 派生子任务——子会话创建时
// 继承父会话的工作目录（assistant.go 的 dispatch_sub：ws := t.workspace()），它就是
// 那个目录里真实发生过的活动，所以计。两类不计：数字助理主对话（全局总控会话，
// workspace 为空会折进默认工作区，它不隶属任何分组，计它等于让默认工作区凭一个列表里
// 看不见的会话长期占顶）；已归档会话（用户已把它移出视线，同理不计）。
function sessionCountsForWsOrder(s) {
  return s.mode !== 'assistant' && !s.archived;
}

let wsOrderSnapshot = null; // 目录顺序快照（ws 数组，最新会话降序）；null = 本次启动还没算出来

function snapshotWsOrder() {
  const newest = new Map(); // ws -> 该目录最新会话的时间戳（毫秒），0 = 这个目录还没有会话
  // 先把已知目录按既有顺序铺一遍（0 键），再用会话的最新时间覆盖：没有会话的目录、
  // 以及时间戳缺失/异常的目录都停在 0 这一档，它们彼此之间的相对顺序就严格等于
  // 改动前的分组顺序（Array#sort 稳定，不会因为 NaN 或新目录把别的目录挤乱）。
  for (const ws of knownWorkspaces()) newest.set(ws, 0);
  (state.sessions || []).forEach((s) => {
    if (!sessionCountsForWsOrder(s)) return;
    const ws = wsOf(s);
    const t = sessionSortStamp(s);
    if (!newest.has(ws) || t > newest.get(ws)) newest.set(ws, t);
  });
  return [...newest.keys()].sort((a, b) => newest.get(b) - newest.get(a));
}

// renderSessionList：侧栏纯本地重渲（不 发请求）。loadSessionsNow 拉到列表后调；
// 切换会话的乐观阶段（app-chat.js 的 applySessionChrome）也直接调它——高亮跟手，
// 不用等（也不该为一次高亮去打一轮列表请求）
function renderSessionList() {
  const box = $('session-list');
  box.innerHTML = '';
  if (binView) { renderBin(box); return; } // 回收站视图：整个列表换成回收站内容
  // 左栏只放顶层未归档会话；助理主对话、子会话/辅助对话不占列表
  // （标题/内容的搜索在搜索面板里走 /api/search/*，这里不再本地过滤）
  let visible = state.sessions.filter((s) => s.mode !== 'assistant' && !s.parentId && !s.archived);
  if (viewCfg.sort === 'created') {
    visible.sort((a, b) => String(b.createdAt || '').localeCompare(String(a.createdAt || '')));
  }
  const recycled = new Set(lsGet('jx.wsrecycled', []));
  const keep = new Set(lsGet('jx.wskeep', [])); // 已单独恢复的对话：目录回收着也照常显示
  const aliasMap = lsGet('jx.wsalias', {});
  if (viewCfg.group === 'flat') {
    box.classList.add('flat');
    let n = 0;
    for (const s of visible) {
      if (!recycled.has(wsOf(s)) || keep.has(s.id)) { box.appendChild(sessionBlock(s)); n++; }
    }
    if (!n) box.innerHTML = '<div class="ws-empty">没有会话</div>';
    return;
  }
  if (viewCfg.group === 'time') {
    // 时间线分组：按最后活动时间（updatedAt，缺省 createdAt）分时间带
    box.classList.add('flat');
    const day0 = (d) => new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime();
    const today0 = day0(new Date());
    const bucketOf = (s) => {
      const raw = String(s.updatedAt || s.createdAt || '').replace(' ', 'T');
      const t = new Date(raw).getTime();
      if (isNaN(t)) return 4; // 无时间信息的落「更早」
      const days = Math.floor((today0 - day0(new Date(t))) / 86400000);
      if (days <= 0) return 0;
      if (days === 1) return 1;
      if (days <= 3) return 2;
      if (days <= 7) return 3;
      return 4;
    };
    const bands = ['今天', '昨天', '3天前', '1周前', '更早'].map((label) => ({ label, items: [] }));
    visible.forEach((s) => { if (!recycled.has(wsOf(s)) || keep.has(s.id)) bands[bucketOf(s)].items.push(s); });
    bands.forEach((b) => {
      if (!b.items.length) return;
      const head = document.createElement('div');
      head.className = 'time-band';
      head.textContent = b.label;
      box.appendChild(head);
      b.items.forEach((s) => box.appendChild(sessionBlock(s)));
    });
    if (!visible.length) box.innerHTML = '<div class="ws-empty">没有会话</div>';
    return;
  }
  box.classList.remove('flat');
  // 本次启动的目录顺序快照还没算出来（第一份会话列表未到）：先什么都不画，
  // 免得先按改动前的顺序摆一遍空分组，等数据到了再跳变。
  if (!wsOrderSnapshot) return;
  // 按工作区分组；手动添加过的工作区即使没有会话也显示
  const groups = new Map();
  for (const ws of knownWorkspaces()) groups.set(ws, []);
  for (const s of visible) {
    const ws = wsOf(s);
    if (!groups.has(ws)) groups.set(ws, []);
    groups.get(ws).push(s);
  }
  // 分组顺序 = 启动快照（该目录最新会话时间降序）：只照快照摆位，不重算。
  // 快照里没有的目录（运行期间手动加进来的、会话落在新目录的）一律排在最后，
  // 且保持它们本来的发现顺序（稳定），绝不动已有目录的先后。
  const rankOf = new Map(wsOrderSnapshot.map((ws, i) => [ws, i]));
  let tail = wsOrderSnapshot.length;
  const rank = new Map();
  for (const ws of groups.keys()) rank.set(ws, rankOf.has(ws) ? rankOf.get(ws) : tail++);
  for (const [ws, raw] of [...groups].sort((a, b) => rank.get(a[0]) - rank.get(b[0]))) {
    let items = raw;
    if (recycled.has(ws)) {
      items = items.filter((s) => keep.has(s.id)); // 目录回收了：只显示被单独恢复的对话
      if (!items.length) continue;
    }
    const group = document.createElement('div');
    group.className = 'ws-group';
    const collapsed = !!wsCollapsed[ws];
    const head = document.createElement('div');
    head.className = 'ws-group-head' + (collapsed ? ' collapsed' : '');
    head.title = ws || '（未设置，使用默认工作区）';
    head.innerHTML =
      `<svg class="chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M9 6l6 6-6 6"/></svg>` +
      `<svg class="folder" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>` +
      `<span class="ws-name">${esc(aliasMap[ws] || baseName(ws) || '默认工作区')}</span>` +
      `<span class="ws-count">${items.length}</span>` +
      `<span class="ws-acts">` +
        `<button class="wa-add" title="在此目录新建会话"><svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M12 5v14M5 12h14"/></svg></button>` +
        `<button class="wa-more" title="工作区操作"><svg viewBox="0 0 24 24" fill="currentColor"><circle cx="5" cy="12" r="1.6"/><circle cx="12" cy="12" r="1.6"/><circle cx="19" cy="12" r="1.6"/></svg></button>` +
      `</span>`;
    head.onclick = (e) => {
      if (e.target.closest('.ws-acts')) return;
      wsCollapsed[ws] = !collapsed;
      lsSet('jx.wscol', wsCollapsed);
      loadSessions();
    };
    head.querySelector('.wa-add').onclick = (e) => {
      e.stopPropagation();
      homeDraft.workspace = ws;
      persistHomeDraftLS();
      goHomeNewTask();
    };
    head.querySelector('.wa-more').onclick = (e) => {
      e.stopPropagation();
      const r = e.currentTarget.getBoundingClientRect();
      const alias = lsGet('jx.wsalias', {})[ws] || '';
      const acts = [
        { label: '重命名', fn: () => {
          const t = prompt('重命名工作区（仅显示名）', alias || baseName(ws));
          if (t === null) return;
          const m = lsGet('jx.wsalias', {});
          if (t.trim()) m[ws] = t.trim(); else delete m[ws];
          lsSet('jx.wsalias', m);
          loadSessions();
        } },
        { label: '复制路径', fn: () => navigator.clipboard.writeText(ws) },
        { label: '在资源管理器中打开', fn: () => api('/api/open-folder', { method: 'POST', body: JSON.stringify({ path: ws }) }).catch((err) => alert('打开失败：' + err.message)) },
        { label: '移入回收站', danger: true, fn: () => {
          lsSet('jx.wsrecycled', [...new Set([...lsGet('jx.wsrecycled', []), ws])]);
          loadSessions();
        } },
        ...ctxSpeakItems({ kind: 'workspace', text: speechSelectionIn(head) || workspaceSpeechText(ws) }),
      ];
      openCtxMenu(acts, r.left, r.bottom + 4);
    };
    group.appendChild(head);
    if (!collapsed) {
      const list = document.createElement('div');
      list.className = 'ws-group-list';
      for (const s of items) list.appendChild(sessionBlock(s));
      if (!items.length) list.innerHTML = '<div class="ws-empty">（空目录）</div>';
      group.appendChild(list);
    }
    box.appendChild(group);
  }
}

/* ---------- 侧栏收起/展开（顶栏最左按钮，状态落本地） ---------- */

function setSidebarBtn(collapsed) {
  const label = collapsed ? '展开左侧栏' : '收起左侧栏';
  const btn = document.getElementById('btn-sidebar');
  if (btn) btn.title = label;
  const txt = document.getElementById('sb-btn-text');
  if (txt) txt.textContent = label;
}

function applySidebarState() {
  const collapsed = lsGet('jx.sb', '') === '1';
  document.body.classList.toggle('sb-collapsed', collapsed);
  setSidebarBtn(collapsed);
  rpApply(); // 左侧栏折叠状态影响右栏可占的剩余宽
}


function toggleSidebar() {
  // 窄屏：整屏浮层只切显隐类——不写 jx.sb、不跑宽度动画（CSS transform 过渡）、
  // 不动右栏推导，桌面的收起记忆不被手机污染
  if (isNarrow()) {
    const show = document.body.classList.toggle('sb-show');
    setSidebarBtn(!show);
    if (!show) onLeftPanelClosed(); // 收起：补回历史哨兵，下一次返回重新呼出左栏
    return;
  }
  const sb = $('sidebar');
  const collapsed = document.body.classList.toggle('sb-collapsed');
  setSidebarBtn(collapsed);
  lsSet('jx.sb', collapsed ? '1' : '0');
  rpApply();
  slidePane(sb, collapsed ? sbWidth() : 0, collapsed ? 0 : sbWidth()); // 收起 w→0，展开 0→w
}

$('btn-new-task').onclick = () => { if (binView) setBinView(false); goHomeNewTask(); };
$('btn-sidebar').onclick = toggleSidebar;
$('wst-search').onclick = toggleSideSearch;
$('wst-view').onclick = (e) => openViewMenu(e.currentTarget);
$('wst-add').onclick = addWorkFolder;
$('wst-empty').onclick = emptyRecycleBin;
// 侧栏搜索输入：防抖后触发双路搜索；✕/Escape 关闭并中止
document.getElementById('side-search-input').addEventListener('input', (e) => {
  if (sideSearchDebounce) clearTimeout(sideSearchDebounce);
  const v = e.target.value;
  sideSearchDebounce = setTimeout(() => { sideSearchDebounce = null; runSideSearch(v); }, 250);
});
document.getElementById('side-search-input').addEventListener('keydown', (e) => {
  if (e.key === 'Escape') { e.stopPropagation(); closeSideSearch(); }
});
$('side-search-close').onclick = closeSideSearch;
$('btn-new-assistant').onclick = () => {
  if (binView) setBinView(false);
  setFace('thinking', 1600); // 进入时压眼扫视
  openAssistant();
};
/* ---------- 底部插槽的第一位使用者：内置「回收站」 ----------
   条目本身（骨架、排序、电脑竖排/手机三列网格、标签行数）全归插槽，这里只交
   图标、名字、序号与点击。序号 1 ＝ 最下面（紧贴设置行），组件入口（企业技能包 /
   工作流 2、小程序平台 3 …）从 2 起往上叠——契约见 app-core.js 的 JX.slot 与
   AGENT.md「界面插槽」。id 保持 btn-recycle：回收站视图的选中红边与
   setBinView 都按它取（见 style.css 的 body.bin-mode 段）。 */
JX.slot('sidebar.foot').add({
  key: 'recycle', id: 'btn-recycle', order: 1, owner: '内置',
  label: '回收站', title: '回收站',
  icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M3 6h18"/><path d="M8 6V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/><path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6"/><path d="M10 11v6M14 11v6"/></svg>',
  onClick: () => setBinView(!binView),
});

/* ---------- 左上角入口按钮：图标 hover 随机轻扭（三个按钮共用一套） ----------
   鼠标进入任一入口按钮（新工作任务 / 数字助理主对话 / 待办与定时任务）时，图标朝随机
   一侧轻扭一下再回正（幅度 5°~7°，末尾带一点反向回弹），动画结束自动回到直立；再次
   hover 能再扭一次，方向和幅度都重新随机。动画本身在 style.css 的 side-btn-wig。
   用事件委托挂在 .actions 上而不是逐个按钮绑：第三个按钮「待办与定时任务」是组件
   （组件/todo-scheduler/client.js）注册时才插进来的，静态绑会漏掉它；委托也顺带覆盖
   以后往这一组里加的新入口。 */
const WIG_DEG = 10, WIG_JITTER = 3; // 扭动幅度 10° + 随机 0~3°（加号/时钟用；小人改点头两下，不走扭动）：明显但不夸张
function wiggleSideBtn(btn) {
  const dir = Math.random() < .5 ? -1 : 1; // 方向每次独立随机（左/右）
  const deg = dir * (WIG_DEG + Math.random() * WIG_JITTER);
  btn.style.setProperty('--wig-deg', deg.toFixed(2) + 'deg');
  btn.classList.remove('wig');
  void btn.offsetWidth; // 强制重排：连续 hover 时重新挂 .wig，动画才会从头再播一遍
  btn.classList.add('wig');
}
function bindSideBtnWiggle() {
  const acts = document.querySelector('#sidebar .actions');
  if (!acts) return;
  acts.addEventListener('mouseover', (e) => {
    const btn = e.target.closest('.side-btn');
    if (!btn || !acts.contains(btn)) return;
    // 在同一个按钮内部从图标挪到文字（relatedTarget 还在按钮里）不算重新进入，不重播
    if (e.relatedTarget && btn.contains(e.relatedTarget)) return;
    wiggleSideBtn(btn);
  });
  // 播完摘掉 .wig，图标停在直立态（prefers-reduced-motion 下没有动画、摘不掉也无妨：
  // 下次 hover 照旧「先 remove、再强制重排、再 add」）
  acts.addEventListener('animationend', (e) => {
    if (e.animationName !== 'side-btn-wig' && e.animationName !== 'side-btn-nod') return; // 小人点头（nod）与扭动（wig）播完都要摘
    if (e.target.parentElement) e.target.parentElement.classList.remove('wig');
  });
}
bindSideBtnWiggle();

/* ---------- 左侧栏拖拽调宽：272~560，记忆落本机（jx.sbw，经 --sb-w 生效） ----------
   桌面专属：窄屏浮层恒为整屏宽（把手 CSS 禁指针 + JS 双保险，不写不落盘防污染
   桌面记忆）。拖拽中改 --sb-w，#sidebar 与 .sb-inner 一起变、列表实时回流；
   开合动画照旧走 slidePane（只动 #sidebar 内联宽，--sb-w 不参与）。 */
const SB_MIN = 272, SB_MAX = 560; // 窄到默认即止（再窄列表挤不出信息），宽过手机断点一档

function sbWidth() {
  const v = parseFloat(document.documentElement.style.getPropertyValue('--sb-w'));
  return isNaN(v) ? 272 : v;
}

function applySidebarWidth(px, save) {
  if (isNarrow()) return NaN;
  // 夹取 272~560；窗口太窄时上限收到「窗口宽-320」，给对话区留条活路
  const upper = Math.max(SB_MIN, Math.min(SB_MAX, window.innerWidth - 320));
  px = Math.round(Math.min(Math.max(px, SB_MIN), upper));
  document.documentElement.style.setProperty('--sb-w', px + 'px');
  if (save) lsSet('jx.sbw', String(px));
  rpApply(); // 右栏可瓜分的宽随左栏变（开着时重推导）
  return px;
}

function restoreSidebarWidth() {
  const px = parseFloat(lsGet('jx.sbw', ''));
  if (px >= SB_MIN) applySidebarWidth(px);
}

function setupSidebarResize() {
  const sb = $('sidebar');
  const h = document.createElement('div');
  h.className = 'sb-resize';
  h.title = '拖动调整左侧栏宽度（双击恢复默认）';
  sb.appendChild(h);
  let startX = 0, startW = SB_MIN, moved = false;
  h.addEventListener('pointerdown', (e) => {
    if (isNarrow()) return;
    e.preventDefault();
    sb._slideTok = ++slideSeq; // 作废进行中的开合动画，宽度交还给 --sb-w
    sb.classList.remove('anim');
    sb.style.width = sb.style.minWidth = '';
    startX = e.clientX;
    startW = sbWidth();
    moved = false;
    h.classList.add('dragging');
    try { h.setPointerCapture(e.pointerId); } catch (err) { /* 旧浏览器忽略 */ }
  });
  // 拖动跟随不依赖 setPointerCapture（远程页面上捕获会静默失效，10px 把手之外
  // 收不到 move）：move/up 由 window 接管，事件从指针下元素冒泡到 window——
  // 与对话区把手（app-chat.js）、右栏左缘（app-right.js）同一模式。
  window.addEventListener('pointermove', (e) => {
    if (!h.classList.contains('dragging')) return;
    if (applySidebarWidth(startW + (e.clientX - startX)) !== startW) moved = true;
  });
  const up = () => {
    if (!h.classList.contains('dragging')) return;
    h.classList.remove('dragging');
    if (moved) applySidebarWidth(sbWidth(), true); // 松手落盘
  };
  window.addEventListener('pointerup', up);
  window.addEventListener('pointercancel', up);
  h.addEventListener('dblclick', () => {
    lsSet('jx.sbw', null);
    applySidebarWidth(272);
  });
  // 窗口缩窄时把过宽的栏夹回可用范围（只改当前宽，不动记忆）
  window.addEventListener('resize', () => { if (!isNarrow()) applySidebarWidth(sbWidth()); });
}
setupSidebarResize();

/* ---------- 窄屏返回层级：网页里唯一的一套导航逻辑 ----------
   用户定义（网页与手机 App 完全同一套，两端不许各写一套）：
     · 右栏开着 → 关右栏（关完停在哪，由下一次返回照常决定）；
     · 左栏开着 → 收起左栏（回对话页）；
     · 都收着（对话页）→ 打开左栏；
     · 3 秒内连按两次 → 退出（浏览器＝离开网页，App＝壳结束本页）。
   三条通路走的是同一批函数：系统返回键（壳注入调用 jxNavBack）、浏览器后退
   （popstate → jxNavBack）、左右滑手势（jxPanelSet 同一批开合原语）。
   设置页自带两级返回（{jxSettings:depth} 哨兵），优先级最高——本函数把返回
   让给它，不插队。

   历史哨兵：窄屏页面加载即压一条 {jxGuard:1}。浏览器/系统手势的后退会先吃掉
   它再派发 popstate——守卫按到达点校准 guardOn（当前位置是否站在哨兵上），
   需要继续拦截时补压一条。App 里壳直接调 jxNavBack（不碰 history），此时
   guardOn 恒为真、补压是幂等空操作。
   桌面宽屏不压哨兵、不拦截——浏览器后退行为完全不变。 */

const BACK_EXIT_MS = 3000;      // 连按两次的窗口：与手机 App 原来的 3 秒一致
let lastBackAt = 0;             // 上一次返回的时刻（连按窗口用）
let guardOn = false;            // 当前位置是否在我方哨兵条目上（popstate 按到达点校准）
let exiting = false;            // 已决定退出：不再拦截，直到页面重载
let backGuardSkipOnce = false;  // 设置页主动退栈（history.go）的一次性豁免，见 app-settings.js
let guardReadyAt = 0;           // 加载瞬间的杂散 popstate 忽略窗口

// 左右栏的开合态（都取逻辑态：窄屏收起有滑出过渡，DOM 的 hidden 会滞后）
function jxLeftOpen() { return document.body.classList.contains('sb-show'); }
function jxRightOpen() { return document.body.classList.contains('rp-open'); }

// 左/右栏开合的唯一原语：返回键与左右滑手势都走它，不再有第二套开关逻辑
// （右栏永远经 rpOpen/rpClose，展开回到上次看的视图）
function jxPanelSet(side, open) {
  if (side === 'left') {
    if (jxLeftOpen() !== open) toggleSidebar(); // 窄屏分支：切 sb-show + 按钮文案 + 哨兵
    return;
  }
  if (jxRightOpen() !== open) toggleRpanel();
}

function backGuardArm() {
  if (guardOn) return; // 幂等：栈顶已有哨兵，不再堆叠
  try {
    history.pushState({ jxGuard: 1 }, '');
  } catch (e) {
    return; // 环境不允许 pushState（远程外壳一类宿主）：守卫降级为不生效，
            // 返回键交给浏览器——别让异常沿着 onLeftPanelClosed 把
            // openSettings 的后续步骤（开 UI/压栈）整个打断
  }
  guardOn = true;
}

// 左栏被收起后的统一钩子：补回哨兵，让下一次返回键重新从「呼出左栏」开始。
// 宽屏/已放行退出时自动跳过（幂等，可被多处重复调用）。
function onLeftPanelClosed() {
  if (!isNarrow() || exiting) return;
  backGuardArm();
}

/* 唯一的返回入口：按当前层级决定下一步动作，返回做了什么（手机壳只认 'exit'）。
   e 传 popstate 事件时表示这次后退是浏览器/系统手势送进来的（历史栈已退一步，
   需要时由这里的 backGuardArm 补回哨兵）；不传＝手机壳注入调用，历史栈没动。 */
function jxNavBack(e) {
  const fromPop = !!(e && e.type === 'popstate');
  if (exiting) return 'none';
  // 设置页：两级返回全归它自己（{jxSettings:depth} 哨兵 + 自己的 popstate 监听）
  if (!$('settings').classList.contains('hidden')) {
    // popstate 路径不做动作：设置页的监听器正在同一事件里退栈，插手会多退一级
    if (!fromPop) $('set-back').click(); // 壳直接调：等效点设置页的返回按钮
    return 'settings';
  }
  // 连按两次＝退出（与手机 App 原来的 3 秒窗口一致）。放在其余层级之前：这是
  // 「用户要立刻出去」的逃生口，不能被某一层开着挡住（全屏视频除外，归壳）
  const now = Date.now();
  const doubleBack = now - lastBackAt <= BACK_EXIT_MS;
  lastBackAt = now;
  if (doubleBack) {
    exiting = true;
    // 浏览器路径：哨兵已被这次后退吃掉，再退一步就是离开网页（无更早页面时由
    // 浏览器自己收尾）；App 路径：壳收到 'exit' 后结束本页（网页做不到这件事）
    if (fromPop) history.back();
    return 'exit';
  }
  if (!isNarrow()) return 'none'; // 桌面宽屏：左右栏是常驻布局，没有层级可退
  // 弹出菜单开着：先关菜单（返回键逐层收敛），下一次返回才轮到左右栏/对话页
  if (document.querySelector('.popover:not(.hidden)')) {
    closePopovers();
    backGuardArm();
    return 'popover';
  }
  // 居中层（确认框等 modal）开着：本次让给弹层——不动作也不补哨兵，关掉弹层后
  // 的下一次返回照常走层级
  if ([...document.querySelectorAll('.modal')].some((m) => !m.classList.contains('hidden'))) return 'modal';
  // 右栏开着：先关右栏，本次返回到此为止（判定用 body.rp-open 这个逻辑开合态：
  // 窄屏收起有 220ms 滑出过渡，panel 的 hidden 要等过渡结束才加，用它会让动画
  // 期间连按的第二次返回被「右栏还开着」吞掉，左栏永远轮不到）
  if (jxRightOpen()) {
    jxPanelSet('right', false);
    backGuardArm();
    return 'right';
  }
  // 左栏开着：收起它＝回对话页（收起时 toggleSidebar 已补回哨兵）
  if (jxLeftOpen()) {
    jxPanelSet('left', false);
    return 'left-close';
  }
  // 都收着（对话页）：打开左栏
  jxPanelSet('left', true);
  backGuardArm();
  return 'left-open';
}

function backGuardPop(e) {
  const wasOnGuard = guardOn;               // 弹出前站的那条是不是哨兵
  guardOn = !!(e.state && e.state.jxGuard); // 先按到达点校准
  if (Date.now() < guardReadyAt) return;     // 加载瞬间：浏览器可能补发杂散 popstate
  if (backGuardSkipOnce) { backGuardSkipOnce = false; return; } // 设置页程序性退栈
  // 落点仍是哨兵、弹出前也在哨兵上：栈里有重复哨兵（WebView 连历史记录一起
  // 恢复页面时可能出现）。这条只是多出来的，吃掉、不派发动作也不再补压——
  // 下一次返回落到真页面条目，层级照常生效（只会白按一次，不会失灵）
  if (wasOnGuard && guardOn) return;
  jxNavBack(e);
}

(function initJxBackGuard() {
  if (!isNarrow()) return; // 桌面/宽屏：不参与返回键语义
  guardReadyAt = Date.now() + 600;
  // 自检：页面可能是 WebView 按 saved state 恢复出来的，历史栈连同哨兵条目
  // 一起回来了——当前条目若已站在哨兵上，就不能再压一条（多压会让第一次
  // 返回像「按了没反应」）。以真实的 history.state 为准校准 guardOn
  guardOn = !!(history.state && history.state.jxGuard);
  window.addEventListener('popstate', backGuardPop);
  backGuardArm(); // 加载即压哨兵：第一次按返回先给左栏，而不是直接离开
})();
