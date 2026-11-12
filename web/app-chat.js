/* app-chat.js —— 对话区引擎：消息渲染（折叠基元/回合分组/kind 分发注册表
   ROW_ITEM_BUILDERS，契约见《消息类型规范.md》）、发送与流式、SSE 事件流、
   审批/提问/待办卡片、SSH 远程、输入区与弹出面板、宽度拖把手。第四个加载。 */

/* 全局错误打点（桌面壳排障）：WebView2 封了远程调试端口也没有 console，
   未捕获异常在这里回传后端日志（/api/diag），否则在桌面壳里无声无息——
   顶层脚本一旦中断，后面所有 UI 绑定（菜单项/按钮）全体失效。 */
window.addEventListener('error', (ev) => {
  try {
    const where = (ev.filename || '').split('/').pop();
    const line = ev.lineno ? (':' + ev.lineno + ':' + ev.colno) : '';
    const img = new Image();
    img.src = '/api/diag?m=' + encodeURIComponent('JS错误: ' + ev.message + ' @' + where + line) + '&t=' + Date.now();
  } catch (e) { /* 最后的兜底也别再抛 */ }
});
window.addEventListener('unhandledrejection', (ev) => {
  try {
    const r = ev.reason;
    const img = new Image();
    img.src = '/api/diag?m=' + encodeURIComponent('未处理Promise拒绝: ' + (r && r.message ? r.message : String(r))) + '&t=' + Date.now();
  } catch (e) { /* 忽略 */ }
});

/* ---------- 首页（新建对话）---------- */

// 输入卡片的位置：首页时挂在页面中央的问候区里，进对话后回到底部
// 搬移的是 composer-area（工作目录胶囊 + 输入卡），两者始终在一起
function placeComposer() {
  const c = $('composer-area');
  const homeSlot = $('home-composer');
  const wrap = $('composer-wrap');
  if (!state.cur && homeSlot) {
    wrap.classList.add('hidden');
    if (c.parentElement !== homeSlot) homeSlot.appendChild(c);
  } else {
    wrap.classList.remove('hidden');
    if (c.parentElement !== wrap) wrap.appendChild(c);
  }
  syncComposerPad(mainView); // 首页/会话切换后重算悬浮垫高与按钮锚点
}

// 把输入卡挪回底部容器（msg-col 重建 innerHTML 前必须先挪出，否则会被一起销毁）
function detachComposerToWrap() {
  const c = document.getElementById('composer-area');
  if (c) $('composer-wrap').appendChild(c);
}

function resetEmpty() {
  detachComposerToWrap();
  state._pfxSid = null; // 回到首页：消息区的增量渲染缓存失效
  state._batches = []; // 分批窗口一并作废（顶条收起，重开会话时重设）
  state.older = undefined;
  cancelReleaseOlder(mainView);
  renderLoadOlderBar(mainView);
  $('msg-col').innerHTML =
    '<div class="home">' +
      '<div class="hero">' +
        '<img class="logo-img" src="/logo.png" alt="' + esc(window.brandName()) + '">' +
        '<h1>' + esc(window.brandHero()) + '</h1>' +
      '</div>' +
      '<p class="home-sub">描述你想做的事，或直接在左上角点「跟长期助理对话」。</p>' +
      '<div id="home-composer"></div>' +
    '</div>';
  $('sess-title').textContent = '数字助理';
  $('mode-badge').classList.add('hidden');
  rpClose();
  rpRestoreSession(''); // 回首页：右栏各视图的会话态统一清空（Git 仓库跟随、子对话、计划）
  placeComposer();
  // 输入框整体替换为首页草稿（跨重启保留；没有就是空，绝不带着别的对话的字）。
  // 落盘兜底在调用方（goHomeNewTask 在 cur 置空前已 flush；初始加载无输入）
  $('input').value = homeDraft.text || '';
  restoreQuoteLS(mainView); // 首页草稿里的引用也一并回来（state.cur 为空 → 取 homeDraft.quote）
  autoGrow();
  syncComposer();
}

/* ---------- 会话 ---------- */

async function openAssistant() {
  // 「跟长期助理对话」入口打开的是**当前版本**——哪一版由服务端记在库里
  // （/api/assistant/versions，见 assistant_versions.go）。这里绝不能自己从
  // 会话列表里挑：多版本下「按更新时间取第一个」会把用户刚聊过的那一版当成
  // 当前版本，切回老版本聊一句就顶掉新版本。
  let cur = '';
  try { cur = ((await api('/api/assistant/versions')).current) || ''; } catch (e) { /* 读不到版本就下面新建 */ }
  if (!cur) {
    const r = await api('/api/assistant/versions/new', { method: 'POST' }); // 还没有任何一版：建第一版
    cur = (r && r.sessionId) || '';
  }
  if (!cur) return;
  if (!(state.sessions || []).some((s) => s.id === cur)) await loadSessions(); // 列表里没有这条先补列表（乐观切换要用它当临时 meta）
  await openSession(cur);
}

// 「新工作任务」不立即建会话：留在首页聚焦输入框，发出第一条消息才真正创建
function goHomeNewTask() {
  narrowClosePanels(); // 窄屏：从左栏浮层发起，选中目标即收起浮层
  // 视图收口：市场/小程序平台已组件化（组件可能关闭或摘除，接口由组件挂 window，
  // 不在场即跳过）——回首页即关掉占领主区的整页视图
  if (typeof marketOpen === 'function' && marketOpen() && typeof closeMarket === 'function') closeMarket();
  if (typeof sitesViewOpen === 'function' && sitesViewOpen() && typeof closeSites === 'function') closeSites();
  detachStream(mainView); // 回首页只放开本地流，正在跑的那一轮继续在后台跑完
  if (state.cur) rpSnapshot(state.cur.id); // 回首页前记住这个对话的右栏状态（resetEmpty 会收起右栏）
  flushMainDraft(); // cur 置空前把防抖窗口内的输入落进旧会话草稿（resetEmpty 会整体替换输入框）
  navTok.main++;          // 作废在途的会话加载（回首页是同步完成的导航）
  navBusy.main = false;
  state._pfxSid = null;   // 消息区的增量渲染缓存随之失效
  state._loading = false; // 在途加载态一并作废（回首页是同步完成的导航）
  rememberLastSession(''); // 明确回首页：下次打开从首页开始，不再恢复上次的对话
  state.cur = null;
  // 回首页＝没有会话上下文：组件装配回到全局开关集（session:open 不会来，
  // 这里显式重置；CX 不在场（老引擎壳）时跳过）
  if (window.CX && CX.setSession) CX.setSession('');
  loadProgress();
  state.msgs = [];
  resetEmpty();
  renderPanels(); // 回到首页：待决定的浮窗整体转为「来自「某对话」」（本对话已不存在）
  loadSessions();
  refreshCtxChip();
  $('input').focus();
}

async function openSession(id, opts) {
  const stay = !!(opts && opts.stay); // 停留本页的就地翻新：保视口/展开记忆/分批窗口
  narrowClosePanels(); // 窄屏：在左栏浮层里选中会话即收起浮层
  // 市场像一张对话页：打开任何对话即关掉它——两个视图均已组件化，接口不在场即跳过
  if (typeof marketOpen === 'function' && marketOpen() && typeof closeMarket === 'function') closeMarket();
  if (typeof sitesViewOpen === 'function' && sitesViewOpen() && typeof closeSites === 'function') closeSites();
  // 正在流式时切到别的会话：只放开本地这条流（后端执行照旧，回来会自动接上），
  // 不这样做的话旧会话的增量会被写进新会话的界面
  if (state.streaming && (!state.cur || state.cur.id !== id)) detachStream(mainView);
  // 右栏跟对话走：离开旧对话前快照它的右栏状态（同对话重开/兜底刷新不动右栏）
  const oldSid = state.cur && state.cur.id;
  if (oldSid && oldSid !== id) rpSnapshot(oldSid);
  const tok = ++navTok.main;
  navBusy.main = true;
  // 乐观切换：整包会话数据（含全部历史）在远程访问下要几秒，等待期间界面一步不动，
  // 看起来就是「没切过去」，失败时更是静默留在旧会话——像「跳回」。这里先用列表项
  // （与详情 meta 同型，只可能稍旧）当临时 meta：标题/侧栏高亮/输入框立即换到目标
  // 会话，消息区先进加载态；state.cur 此刻就是目标，期间一切「刷新当前会话」的兜底
  // （refreshCurrent 等）都以目标为准，旧会话的晚到事件不可能再把界面顶回去
  const brief = (state.sessions || []).find((s) => s.id === id);
  if (brief && (!state.cur || state.cur.id !== id)) optimisticSwitch(mainView, brief);
  if (brief && brief.mode === 'assistant' && dollState === 'done') setFace('idle'); // 打开长期助理对话＝完成提醒已读：徽章清、小人回待机
  // 右栏切到目标对话自己的状态（开合/视图/各视图选择；没记忆＝默认收起+空态）。
  // 放在乐观切换之后：各视图的就位逻辑以 state.cur 为准，此刻它已是目标对话
  if (oldSid !== id) rpRestoreSession(id);
  try {
    await loadSessionInto(id, tok, { stay: stay });
  } catch (e) {
    // 加载失败：留在目标会话的壳里，给原因与「重试」，绝不静默退回旧会话
    if (tok === navTok.main) showChatLoadError(mainView, e.message, () => {
      // 重试先手动补加载反馈：state.cur 已是目标会话，openSession 的乐观分支会跳过
      mainView.state._loading = true;
      showChatLoading(mainView);
      openSession(id);
    });
  } finally {
    // 只有仍是最新一次导航才解禁（被更新的导航顶掉时，由它自己负责收尾）
    if (tok === navTok.main) navBusy.main = false;
  }
}

// 乐观切换的落地：壳（标题/徽标/输入区/草稿/侧栏高亮）立即换到目标会话，消息区置为
// 加载态；数据到达后 loadSessionInto / openSubSession 整体接管。列表项当临时 cur，
// 消息与派生数据清空，_pfxSid 失效（渲染缓存随会话作废）
function optimisticSwitch(view, brief) {
  const st = view.state;
  // 落盘必须赶在 cur 改写之前：persistSessionDraftLS / persistSubDraftLS 认的是
  // 「当前 cur」，cur 一旦换成目标对话，这次落盘就把上一个对话的字写进了目标
  // 对话的草稿键——切到哪个对话都带着同一份没发出去的字，正是这个顺序造成的
  if (view === mainView) flushMainDraft(); else flushSubDraft();
  st.cur = { ...brief };
  st.msgs = [];
  st.pendingSteer = [];
  st.pendingQueue = [];
  st.checkpoints = [];
  st.deliveries = [];
  st.streamText = '';
  st.streamThinking = '';
  st._pfxSid = null;
  st._batches = []; // 分批窗口随会话作废（顶条与 older 由 loadSessionInto 重设）
  st.older = undefined;
  cancelReleaseOlder(view);
  renderLoadOlderBar(view);
  st._loading = true; // renderMessages / reloadMessages 见此标记让路，别冲掉加载态
  applySessionChrome(view);
  showChatLoading(view);
}

// 会话壳的即时更新（本地纯渲染）：主对话换顶栏标题/模式徽标与侧栏高亮，子对话换
// 下拉头部；输入区控件与草稿、待决定浮窗跟着走。加载完成后的完整收尾（抽屉/进度/
// 计划视图/接流）仍在各加载函数里
function applySessionChrome(view) {
  const st = view.state;
  if (view === mainView) {
    applyTopbar();
    renderSessionList();
    placeComposer();
  } else {
    subRenderHeader();
    syncComposerPad(view);
  }
  syncComposer(view);
  renderPanels(); // 待决定的浮窗跟着会话归属走
  // ① 输入框整体换成目标对话的草稿；② 调用方必须已经「在 cur 改写之前」把源对话
  // 输入框里的字落过盘（optimisticSwitch / loadSessionInto 各自负责，见那里的注释）——
  // 这里不能自己 flush：此刻 st.cur 已是目标对话，flush 等于把源对话的字写进目标键
  try {
    viewEl(view, 'input').value = localStorage.getItem(
      view === mainView ? sessionDraftKey(st.cur.id) : subDraftKey(st.cur.id)
    ) || '';
  } catch (e) { viewEl(view, 'input').value = ''; }
  autoGrow(viewEl(view, 'input'));
  restoreQuoteLS(view); // 引用胶囊跟输入草稿一起换到目标会话（不残留上一个的）
}

// 「数字助理主对话」在界面上的显示名：会话标题是创建主对话时写进会话 meta 的
// 「数字助理主对话」（老会话里存的也是它），界面统一显示「跟长期助理对话」（纯
// 文本，与侧栏入口同款口径）。这里只做显示层映射——内部标识（mode=assistant）、
// 库里存的标题、以及所有按 mode/parentId 分流的功能逻辑都不动，所以旧会话不必重
// 开、不用改数据，打开即显示新名。
const ASSISTANT_MAIN_TITLE = '跟长期助理对话';
// 是否「数字助理主对话」本体（它名下的子对话有自己的标题，不套用这个名字）
function isAssistantMainSession(s) {
  return !!s && s.mode === 'assistant' && !s.parentId;
}
// 顶栏会话标题：数字助理主对话恒显示新名（纯文本，与侧栏入口同款），其余会话照
// 原样显示自己的标题。空标题 = 刚建、还没发出第一条消息的新会话
// （服务端起标题后就换成真标题）
function applySessTitle(s) {
  const el = $('sess-title');
  if (isAssistantMainSession(s)) el.textContent = ASSISTANT_MAIN_TITLE;
  else el.textContent = (s && s.title) || '新工作任务';
}

// 顶栏标题与模式徽标（主对话区；乐观阶段与数据落地共用）
function applyTopbar() {
  // 标题：数字助理主对话恒显示新名，其余会话显示自己的标题（见 applySessTitle）
  applySessTitle(state.cur);
  const badge = $('mode-badge');
  // 模式徽标；挂在主对话下的子对话再标出来历（它可能是 task/office/coding/aux 任一模式）
  const modeLabel = state.cur.mode === 'assistant' ? '数字助理'
    : state.cur.mode === 'aux' ? '辅助对话'
    : CHAT_MODES.some((m) => m.v === state.cur.mode) ? chatModeOf(state.cur.mode).label : '';
  const badgeLabel = state.cur.parentId ? (modeLabel ? modeLabel + ' · 子对话' : '子对话') : modeLabel;
  if (badgeLabel) {
    badge.textContent = badgeLabel;
    badge.classList.remove('hidden');
  } else {
    badge.classList.add('hidden');
  }
}

// 会话加载态：消息区居中转圈（样式 .chat-loading）；数据落地后 renderMessages 全量接管
function showChatLoading(view) {
  view.col.innerHTML = '<div class="chat-loading"><span class="cl-spin"></span>正在加载对话…</div>';
  view.scroll.scrollTop = 0;
}

// 加载失败：留在目标会话的壳里，给原因与「重试」（重试即重新走一次切换加载）
function showChatLoadError(view, msg, retry) {
  view.state._loading = false;
  const box = document.createElement('div');
  box.className = 'chat-loading';
  const txt = document.createElement('span');
  txt.textContent = '对话加载失败：' + (msg || '网络异常');
  const btn = document.createElement('button');
  btn.className = 'cl-retry';
  btn.type = 'button';
  btn.textContent = '重试';
  btn.onclick = retry;
  box.appendChild(txt);
  box.appendChild(btn);
  view.col.innerHTML = '';
  view.col.appendChild(box);
}

// 迟到的整包快照比界面已显示的还旧吗：只有视图正停在同一会话上才可比——会话内
// 消息 seq 单调递增，末条 seq 被反超即为旧快照：切换加载在途时用户发了消息，回显
// 与输出已随流式帧落地渲染，而这份 GET 的服务端快照取于落库之前，整包再落地会把
// 刚显示的消息顶回发送之前，直到下次整包加载才回来——正是「发消息出去界面没
// 更新，切走再切回来才正常」。navTok 只挡得住「更新的导航晚到」，挡不住「更旧的
// 数据晚到」，这里按 seq 对龄补上后半边。跨会话（切换目标的历史天然更少甚至为空）
// 绝不算旧快照，否则整个切换被拦下、界面停在旧会话上。
function snapshotStale(view, sid, incoming) {
  const st = view.state;
  if (!st.cur || st.cur.id !== sid) return false; // 视图不在该会话上：真切换，快照照常应用
  const cur = st.msgs || [];
  const inc = incoming || [];
  if (!cur.length) return false; // 界面还没有内容：任何快照都比它新
  if (!inc.length) return true;  // 同一会话界面有内容而快照为空：快照必然更旧
  return (inc[inc.length - 1].seq || 0) < (cur[cur.length - 1].seq || 0);
}

/* ---------- 分批加载（超长对话）：初始只取最近一批，上翻再取更早批次 ----------
   口径：每批 1000 条；视口上方已加载不足 500 条即预取下一批（给远程/手机网络
   留提前量，衔接无感）。DOM 只在头部前插（已渲染节点永不重建——分批的核心
   收益），配合 content-visibility 离屏跳过布局；回到底部 60 秒不上翻即释放
   上方批次（DOM 与内存回到基础窗，下次上翻按同一机制重新加载，展开记忆还在） */
const LOAD_BATCH = 1000;
const LOAD_BUFFER = 500;
const RELEASE_OLDER_MS = 60000;

// seq→已加载索引：滚动预取判定要知道「视口顶附近的消息离已加载窗口顶还有几条」
function buildSeqIndex(view) {
  const st = view.state;
  st._seqIdx = new Map();
  (st.msgs || []).forEach((m, i) => { if (m.seq != null) st._seqIdx.set(m.seq, i); });
}

// older（服务端 {count,sys,tools,envBoundary}）→ groupTurns 的分组基线（与
// 子对话继承段同一接力机制）：批次头部的 prompt 变更行据此显示「变更行」而
// 非「首见完整交代」。envBoundary 不在这里带：摘要行会混入分组输入，覆盖
// 边界由 sumAt 扫描自动推进到位
function olderBaseOf(o) {
  if (!o || (!o.sys && !o.tools)) return null;
  return {
    sawPrompt: true,
    lastSys: o.sys || '',
    lastToolsJson: o.tools ? JSON.stringify(o.tools) : '',
    lastTools: o.tools || null,
  };
}

// 顶条（#load-older）：剩余提示 / 加载中转圈 / 到底隐藏。点击立即取上一批
// （onclick 挂在 setupScrollTuck，条本身常驻 DOM、只换内容）
function renderLoadOlderBar(view) {
  const bar = viewEl(view, 'load-older');
  if (!bar) return;
  const st = view.state;
  if (st._loadingOlder) {
    bar.classList.remove('hidden');
    bar.classList.add('loading');
    bar.innerHTML = '<span class="cl-spin"></span>正在加载更早消息…';
    return;
  }
  bar.classList.remove('loading');
  if (!st.older || !(st.older.count > 0)) { bar.classList.add('hidden'); return; }
  bar.classList.remove('hidden');
  bar.textContent = '上面还有 ' + st.older.count + ' 条更早消息，上翻自动加载';
}

// 取上一批更早消息并前插渲染：网络只拉这一批（before_seq 从当前窗口首条向前），
// 已渲染的旧节点一个不动，视口按新增高度补差（用户停在原处不跳）。批次含
// inherit 标记行（已到会话最开头）时走一次全量渲染——继承段要折成「来自主
// 对话」块、已加载头部的克隆历史行要折进去，头部必须重建（只发生在翻到头，
// 此时消息已接近全量，一次性成本与旧版打开全量相当）。返回是否成功落地
// （jumpToMessage 据此决定是否继续补齐到目标消息）。
async function loadOlder(view) {
  view = view || mainView;
  const st = view.state;
  if (!st.cur || !st.older || !(st.older.count > 0) || st._loadingOlder) return false;
  if (st._loading || !st.msgs || !st.msgs.length) return false; // 整包在途：由加载方收口
  const sid = st.cur.id;
  const firstSeq = st.msgs[0].seq; // 慢网落地校验：窗口可能已被重开/重置
  st._loadingOlder = true;
  cancelReleaseOlder(view); // 上翻即取消「到顶释放」计时
  renderLoadOlderBar(view);
  try {
    const d = await api('/api/sessions/' + sid + '?before_seq=' + firstSeq + '&limit=' + LOAD_BATCH);
    if (!st.cur || st.cur.id !== sid || !st.msgs.length || st.msgs[0].seq !== firstSeq) return false;
    const batch = d.messages || [];
    if (!batch.length) { st.older = { ...st.older, count: 0 }; return false; }
    const older = d.older ? { ...d.older, count: d.older.count || 0 } : { count: 0 };
    if (batch[0].kind === 'inherit') {
      st.msgs = batch.concat(st.msgs);
      st.older = older;
      st._batches = []; // 全量重排：批记账作废（renderMessages 重建分支同样清）
      renderMessages(st.msgs, view);
      buildSeqIndex(view);
      return true;
    }
    // 普通批次：分组输入 = 新批 + 已加载的摘要/压缩账本行（这两类 builder 不产
    // 条目、只按 coversUpTo/覆盖区间归位——卡片与账本挂到新批内的真实发生位置，
    // 落库在末尾的行不会丢归位）。基线取当前 older.base（该批之前的状态）
    const relisted = st.msgs.filter((m) => displayKindOf(m) === 'summary' || isCompactNoteRow(m));
    const input = batch.concat(relisted);
    const resultOf = {};
    for (const m of input) { if (m.role === 'tool' && m.tool_call_id) resultOf[m.tool_call_id] = m; }
    reportedSubs = scanReported(batch.concat(st.msgs)); // 子任务卡完成态按全量已加载口径
    const turns = groupTurns(input, resultOf, olderBaseOf(st.older));
    const frag = document.createDocumentFragment();
    turns.forEach((turn, ti) => {
      appendTurn(frag, turn, sid + ':' + (turn.userMsg ? turn.userMsg.seq : 'head' + ti), view, false);
    });
    st.msgs = batch.concat(st.msgs);
    const nodes = frag.childNodes.length;
    // 视口补偿：按锚元素 rect 差精确归位——scrollHeight 在 content-visibility
    // 下是估算值（离屏内容按 200px/条占位），+= dh 会累积偏差；程序写 scrollTop
    // 还会抑制原生滚动锚定。锚＝视口中线命中的消息列直接子元素（与 anchorSave
    // 同法，但不用它的 250ms 活动守卫：上翻触发加载时用户往往刚滚过，不补就跳）
    let anchorEl = null, anchorTop = 0;
    if (view._pinned) {
      const vr = view.scroll.getBoundingClientRect();
      let el = document.elementFromPoint(vr.left + vr.width / 2, vr.top + vr.height / 2);
      if (el && view.col.contains(el)) {
        while (el && el.parentElement !== view.col) el = el.parentElement;
        if (el && el.parentElement === view.col) { anchorEl = el; anchorTop = el.getBoundingClientRect().top; }
      }
    }
    view._renderTs = Date.now(); // 渲染钳位豁免窗口（同 renderMessages：补差与补测余波不当用户滚动）
    view.col.insertBefore(frag, view.col.firstChild);
    if (anchorEl) {
      const dy = anchorEl.getBoundingClientRect().top - anchorTop;
      if (dy) view.scroll.scrollTop += dy; // 锚元素被内容增长推下多少就补多少，纹丝不动
    }
    st.older = older;
    st._pfxLen = (st._pfxLen || 0) + batch.length; // 前缀复用记账：分界内容未变，索引整体平移
    st._pfxNodes = (st._pfxNodes || 0) + nodes;
    (st._batches || (st._batches = [])).unshift({ msgCount: batch.length, nodeCount: nodes });
    buildSeqIndex(view);
    scheduleFolds();
    maybeLoadOlder(view); // 批到齐后缓冲仍不足（视口贴着窗顶）则继续取
    return true;
  } catch (e) {
    return false; // 取不到就保持现状（顶条还在，可再点/再滚）
  } finally {
    st._loadingOlder = false;
    renderLoadOlderBar(view);
  }
}

// 滚动预取判定（rAF 节流）：视口顶附近的消息离已加载窗口顶不足 LOAD_BUFFER
// 条 → 取上一批。视口顶条目身份靠回合碎片自带的 data-seq / data-key（回合键
// sid:seq），再经 seq 索引换算条数——按「条」而非「像素」判缓冲，消息高矮
// 不均时也稳定。
const loadOlderRAF = {};
function maybeLoadOlder(view) {
  view = view || mainView;
  if (loadOlderRAF[view.key]) return;
  loadOlderRAF[view.key] = requestAnimationFrame(() => {
    loadOlderRAF[view.key] = 0;
    const st = view.state;
    if (!st.cur || !st.older || !(st.older.count > 0) || st._loadingOlder || st._loading) return;
    const vr = view.scroll.getBoundingClientRect();
    // 采样点取视口上 1/4 线（顶部可能被更新横幅/其他浮层盖住，elementFromPoint
    // 会命中浮层；1/4 线平时落在消息列里，语义＝「视口上部那条消息距窗口顶
    // 不足 500 条即预取」，与像素阈值相比按条计数不受消息高矮影响）
    let el = document.elementFromPoint(vr.left + vr.width / 2, vr.top + Math.max(60, vr.height * 0.25));
    if (!el || !view.col.contains(el)) return;
    while (el && el.parentElement !== view.col) el = el.parentElement;
    if (!el || el.parentElement !== view.col) return;
    let seq = el.dataset ? el.dataset.seq : '';
    if (!seq && el.dataset && el.dataset.key) seq = el.dataset.key.split(':')[1];
    if (!seq || seq.startsWith('head')) return; // 截断头回合（无用户消息）不做锚
    const idx = st._seqIdx && st._seqIdx.get(+seq);
    if (idx == null || idx >= LOAD_BUFFER) return;
    loadOlder(view);
  });
}

// 「到顶释放」：回到底部（贴底跟随）后 60 秒不再上翻，就把上方已加载批次放
// 掉——超长会话翻过几批后 DOM 重新上万节点，日常底部阅读用不到；下次上翻按
// 同一机制重新加载（展开记忆在，重建后形态一致）
function armReleaseOlder(view) {
  view = view || mainView;
  const st = view.state;
  if (!st._batches || !st._batches.length || st._relTimer) return;
  st._relTimer = setTimeout(() => releaseOlder(view), RELEASE_OLDER_MS);
}
function cancelReleaseOlder(view) {
  view = view || mainView;
  clearTimeout(view.state._relTimer);
  view.state._relTimer = 0;
}
function releaseOlder(view) {
  const st = view.state;
  st._relTimer = 0;
  if (!st._batches || !st._batches.length || st._loadingOlder || st._loading) return;
  if (view._pinned) return; // 钉住阅读（哪怕停在底部附近）不释放
  const sc = view.scroll;
  if (sc.scrollHeight - sc.scrollTop - sc.clientHeight > FOLLOW_RESUME) return; // 不在底部
  let msgCount = 0, nodeCount = 0;
  st._batches.forEach((b) => { msgCount += b.msgCount; nodeCount += b.nodeCount; });
  st._batches = [];
  const h0 = sc.scrollHeight;
  const wasBottom = h0 - sc.scrollTop - sc.clientHeight <= FOLLOW_RESUME; // 释放只在贴底态发生
  view._renderTs = Date.now();
  let rm = nodeCount;
  while (rm > 0 && view.col.firstChild) { view.col.removeChild(view.col.firstChild); rm--; }
  // content-visibility 的 scrollHeight 是估算值，按 dh 补差会留缺口；贴底态
  // 释放后直接贴新底（触发条件本就要求贴底，else 只是防御）
  if (wasBottom) sc.scrollTop = sc.scrollHeight;
  else if (h0 - sc.scrollHeight > 0) sc.scrollTop = Math.max(0, sc.scrollTop - (h0 - sc.scrollHeight));
  st.msgs = st.msgs.slice(msgCount);
  st._pfxLen = Math.max(0, st._pfxLen - msgCount);
  st._pfxNodes = Math.max(0, st._pfxNodes - nodeCount);
  // 未加载条数加回释放的批次；基线恢复为基础窗的（这些消息下次上翻重新加载）
  st.older = { count: ((st.older && st.older.count) || 0) + msgCount, ...(st._baseWinBase || {}) };
  buildSeqIndex(view);
  renderLoadOlderBar(view);
}

async function loadSessionInto(id, tok, opts) {
  // stay＝停留本页的就地翻新（事件驱动 refreshCurrent）：同会话时保留已上翻
  // 加载的分批窗口与阅读状态；跨会话/首次打开照旧走全新加载
  const stay = !!(opts && opts.stay) && !!state.cur && state.cur.id === id;
  // 分批加载：只取最近 LOAD_BATCH 条（超长对话整包要几 MB，打开即卡）；上方
  // 剩余的 older 信息由顶条与上翻预取接管。旧后端不认 tail 参数（返回全量且
  // 无 older 字段）→ 安全降级为现状
  const data = await api('/api/sessions/' + id + '?tail=' + LOAD_BATCH);
  // 已被更新的导航（切换/回首页）取代：晚到的旧响应不落地，否则会把界面顶回旧会话
  if (tok !== navTok.main) return;
  state._loading = false; // 数据落地：退出加载态（必须在 renderMessages 之前）
  // 迟到的旧快照（见 snapshotStale）：界面已被流式帧推进到更新，整包不应用，
  // 只收掉加载态并按现状重绘——壳与列表由后续事件（title/done/session_updated）纠正
  if (snapshotStale(mainView, id, data.messages)) {
    // 旧快照丢的只是消息；权限/能力这类配置仍以服务端为准——否则胶囊会被
    // 这份「不应用」的快照留在旧档（切走再切回才变，正是用户报的现象之一）
    applySessionPermToViews(data.meta);
    renderMessages(state.msgs, mainView);
    return;
  }
  // 落盘必须在 cur 改写之前（见 optimisticSwitch 的注释）：列表里查不到 brief 时
  // 不走乐观切换，cur 此刻还是上一个对话——先把它输入框里的字落到它自己的草稿键
  flushMainDraft();
  // stay 的分批合并：与 reloadMessages 同款——只重取基础窗，已上翻加载的头部
  // （seq 小于新窗首条）原样保留，older 扣掉已加载部分
  if (stay && state.msgs && state.msgs.length && data.messages && data.messages.length) {
    const head = state.msgs.filter((m) => m.seq < data.messages[0].seq);
    if (head.length) {
      data.messages = head.concat(data.messages);
      if (data.older) data.older = { ...data.older, count: Math.max(0, (data.older.count || 0) - head.length) };
    }
  }
  state.cur = data.meta;
  state.msgs = data.messages || [];
  // 分批记账：本窗之上的未加载条数与基线（释放批次时恢复到它）、批栈清零
  state.older = data.older ? { ...data.older, count: data.older.count || 0 } : undefined;
  if (!stay || !state._batches || !state._batches.length) {
    // 全新加载/没有已加载批：基础窗跟新（释放批次时恢复到它的基线）
    state._baseWinBase = data.older ? { sys: data.older.sys, tools: data.older.tools } : undefined;
    state._batches = stay ? (state._batches || []) : [];
  }
  state.pendingSteer = (data.pending && data.pending.steer) || [];
  state.pendingQueue = (data.pending && data.pending.queue) || [];
  state.queuePaused = !!(data.pending && data.pending.paused);
  // 空闲但队列有保留（上轮停止/出错/暂停后留下的）：横幅给「继续」入口
  state.queueHeld = state.pendingQueue.length > 0 && !state.queuePaused &&
    !(data.running || (state.cur && state.cur.status === 'running'));
  state.checkpoints = data.checkpoints || [];
  state.deliveries = data.deliveries || [];
  if (data.error) showRunError(mainView, data.error); else hideRunError(mainView);
  // 计划/目标二选一：旧数据可能两个都开着，收敛为只留计划并落库
  if (state.cur.planMode && state.cur.goalMode) {
    state.cur.goalMode = false;
    api('/api/sessions/' + id, { method: 'PATCH', body: JSON.stringify({ goalMode: false }) }).catch(() => {});
  }
  applySessionChrome(mainView); // 壳与乐观阶段同一套：标题/徽标/侧栏高亮/输入区/草稿/浮窗
  refreshDrawer();
  refreshCtxChip();
  loadProgress();
  // 右栏各视图已随切换就位（openSession 的 rpRestoreSession，计划/子对话/文件/Git
  // 统一走那里）；这里只剩「本对话来了没自动开过的待审批计划」这一件事——
  // 它在恢复之后跑，必要时把右栏翻到计划视图（比恢复优先：审批等不起）
  maybeAutoOpenPlan();
  // stay（事件就地翻新）不 jump：钉住阅读、展开记忆与视口全保留；全新打开
  // 照旧 jump 到底并清记忆
  renderMessages(state.msgs, mainView, stay ? null : { jump: true });
  renderQueueDock(mainView);
  renderLoadOlderBar(mainView); // 分批顶条：有余量则提示「上面还有 N 条」
  // 打开即视为已读：清红点与绿点
  delete dotDone[id];
  if (state.cur && state.cur.unread) {
    state.cur.unread = false;
    api('/api/sessions/' + id, { method: 'PATCH', body: JSON.stringify({ unread: false }) }).catch(() => {});
  }
  // 会话正在执行（含刷新页面后重新打开）：接上它的流，继续实时显示
  if (!state.streaming && (data.running || state.cur.status === 'running')) attachRun(mainView);
  loadSessions();
  JX.emit('session:open', state.cur);
}

/* ---------- 消息渲染（回合分组 + 折叠体系，逻辑对齐数字助理2 的 dsh-transcript-fold） ---------- */

// 折叠阈值：流式尾部窗口行数；中间正文豁免阈值（非空行数 / 显示宽度「字」数，
// 全角记 1、半角 3 个记 1，见 cjkWidth——英文按字符数会虚高，看着短却被判长）
const TAIL_ROWS = 10;
const LONG_TEXT_LINES = 3;
const LONG_TEXT_CHARS = 100;

// 旧库兼容判定（只读旧数据用，新行一律带显式 kind，见《消息类型规范.md》§5）：
// 历史库里系统注入的通知曾是 kind=chat 的 user 行，只能按正文前缀识别。
// 前缀清单与规范 §1.4 的 origin 子类一一对应。
function isSysNote(txt) {
  return txt.startsWith('【派发回报】') || txt.startsWith('【系统中断】') || txt.startsWith('【执行中断】') ||
    txt.startsWith('【文件上传】') || txt.startsWith('【演示记录】') || txt.startsWith('【文件回退】') || txt.startsWith('【目标续跑】') ||
    txt.startsWith('【派发子对话】') || txt.startsWith('【数字助理派发】') || txt.startsWith('【数字助理转达】') ||
    txt.startsWith('【待办任务派发｜') || txt.startsWith('<goal_round>') || txt.startsWith('【交付完成】') || txt.startsWith('【交付结果】');
}

// 显示宽度（按「字」计）：CJK/全角 1 字，英文与半角符号 3 个才算 1 字——折叠
// 判定看的是视觉长度，纯字符计数会让英文多的段落「看着短却被判长」（半角在
// 等宽视角只有全角的一半，比例字体里更窄，按 1/3 折算）
function cjkWidth(t) {
  let w = 0;
  for (const ch of String(t || '')) w += ch.charCodeAt(0) >= 0x2e80 ? 1 : 1 / 3;
  return w;
}

// 中间正文够长则常显不折叠（其上下各折一条折叠条）。阈值按显示宽度而非字符数
// （见 cjkWidth），英文按 1/3 折算——同样 100「字」的视觉长度才常显。
function isLongText(t) {
  t = String(t || '').trim();
  if (!t) return false;
  return t.split('\n').filter((l) => l.trim()).length >= LONG_TEXT_LINES || cjkWidth(t) >= LONG_TEXT_CHARS;
}

function fmtCount(n) { return n >= 1000 ? (n / 1000).toFixed(1).replace(/\.0$/, '') + 'k' : String(n); }

function chev(cls) {
  return '<svg class="' + cls + '" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 9l6 6 6-6"/></svg>';
}

// 折叠容器用 until-found 而非纯 display:none：浏览器 Ctrl+F 命中折叠内容时会自动揭开并触发 beforematch
function hideEl(el) { el.setAttribute('hidden', 'until-found'); }
function showEl(el) { el.removeAttribute('hidden'); }

/* ---------- 过程行：思考行与工具行统一单行样式，点开看详情 ---------- */

function setProcOpen(row, open) {
  row.classList.toggle('open', open);
  const body = row.querySelector('.proc-body');
  if (body) { if (open) showEl(body); else hideEl(body); }
}

// 行级展开记忆的写入/删除：行键挂在 data-rk（见 renderItem），会话取所在视图的
// 当前会话。只有用户点击会走到这里（beforematch 的程序性揭开不记——Ctrl+F 是
// 临时查看，不该固化展开态）。
function procOpenMark(row, open) {
  const rk = row && row.dataset ? row.dataset.rk : '';
  if (!rk) return;
  const v = foldViewOf(row);
  const sid = v && v.state && v.state.cur ? v.state.cur.id : '';
  if (!sid) return;
  if (open) procOpenMem.set(sid + ':' + rk, '1');
  else procOpenMem.delete(sid + ':' + rk);
}

// 点击展开（过程行/折叠段/用户气泡折叠）即钉住阅读：视口停在展开处，新内容
// 继续向下渲染但不再被跟随滚动拽走——否则「展开看一段思考，底下每出一个新行
// 页面就被拽到底」根本没法读完。收起不自动恢复跟随：滚回底部或点「回到底部」
// 由现有机制自然恢复。
function pinViewForRead(el) {
  const v = foldViewOf(el);
  if (v) { v._pinned = true; v._pinTs = Date.now(); }
}

function bindProcToggle(row) {
  row.querySelector('.proc-head').onclick = () => {
    const open = !row.classList.contains('open');
    setProcOpen(row, open);
    procOpenMark(row, open);
    if (open) pinViewForRead(row);
  };
  if (!row.classList.contains('open')) hideEl(row.querySelector('.proc-body'));
}

// 单行折叠过程行（文本正文）：提示词材料各区与变更行共用
function procTextRow(label, sub, text, seq) {
  const row = document.createElement('div');
  row.className = 'proc-row';
  if (seq != null) row.dataset.seq = seq;
  row.innerHTML =
    '<div class="proc-head">' + chev('p-chev') +
    '<span class="p-name">' + label + '</span><span class="p-sub">· ' + sub + '</span></div>' +
    '<div class="proc-body"><div class="tk-text"></div></div>';
  row.querySelector('.tk-text').textContent = text;
  bindProcToggle(row);
  return row;
}

// 行级 diff（多重集合比较，保序输出）：与后端导出日志同一套口径
function lineDiffOf(prev, cur) {
  const oldL = String(prev == null ? '' : prev).split('\n');
  const curL = String(cur == null ? '' : cur).split('\n');
  const removed = [], added = [];
  const cnt = new Map();
  curL.forEach((l) => cnt.set(l, (cnt.get(l) || 0) + 1));
  oldL.forEach((l) => { const c = cnt.get(l) || 0; if (c > 0) cnt.set(l, c - 1); else removed.push(l); });
  const cnt2 = new Map();
  oldL.forEach((l) => cnt2.set(l, (cnt2.get(l) || 0) + 1));
  curL.forEach((l) => { const c = cnt2.get(l) || 0; if (c > 0) cnt2.set(l, c - 1); else added.push(l); });
  return { removed, added };
}
const DIFF_CAP = 15;
function diffBody(prev, cur) {
  const { removed, added } = lineDiffOf(prev, cur);
  const out = [];
  removed.slice(0, DIFF_CAP).forEach((l) => out.push('- ' + l));
  if (removed.length > DIFF_CAP) out.push(`-（其余 ${removed.length - DIFF_CAP} 行省略）`);
  added.slice(0, DIFF_CAP).forEach((l) => out.push('+ ' + l));
  if (added.length > DIFF_CAP) out.push(`+（其余 ${added.length - DIFF_CAP} 行省略）`);
  return out.join('\n') + (out.length ? '\n————\n' : '') + String(cur ?? '');
}

// 首轮拼装的「系统提示词」行：排在工具定义行下方、回合用户消息上方——它是
// 请求消息序列的第 1 条（messages[0]），每次请求逐字节重发，不变则命中前缀缓存
function sysHeadRow(sys, seq) {
  return procTextRow('系统提示词已拼装', fmtCount(String(sys).length) + ' 字 · 请求消息序列第 1 条（messages[0]）· 不变则命中前缀缓存', sys, seq);
}

// 工具定义行：渲染在「系统提示词」行上方、用户消息**之前**。它在请求里不是消息——
// 是请求体顶层的 tools 数组（与 messages 平级，不属于消息序列，所以没有"第几条"）。
// 排在最上面：模型侧的 KV 前缀以工具定义打头（工具目录不变是前缀缓存命中的第一
// 道前提），展示顺序与之对齐——工具定义 → 系统提示词 → 消息序列（用户消息后跟
// 时间提示，末尾那条 user 消息是运行环境快照）。
function toolsRow(tools, seq) {
  const row = document.createElement('div');
  row.className = 'proc-row';
  if (seq != null) row.dataset.seq = seq;
  row.innerHTML =
    '<div class="proc-head">' + chev('p-chev') +
    '<span class="p-name">工具定义</span><span class="p-sub">· ' + tools.length + ' 个 · 请求体顶层字段（与 messages 平级，不在消息序列里）</span></div>' +
    '<div class="proc-body"><pre></pre></div>';
  row.querySelector('pre').textContent = JSON.stringify(tools, null, 2);
  bindProcToggle(row);
  return row;
}

// 首轮拼装的尾部材料：只有「运行环境」——它是请求**消息序列里**的一条 user 快照
// （追加式：首条整份、其后按行差分；有变化才追加，且追加在当轮用户消息之后，
// 所以首轮它就是那次请求的最后一条消息），故展示在用户消息下方。
function promptTailRows(env, seq) {
  return procTextRow('运行环境', fmtCount(String(env ?? '').length) + ' 字 · 追加式环境快照（作为 user 消息进消息序列，有变化才追加新的）', env ?? '', seq);
}

// 系统提示词变更行：diff + 新全文；前缀改变意味着此前缓存的前缀将失效
function sysChangeRow(prev, sys, seq) {
  return procTextRow('系统提示词变更', '前缀改变 · 此前缓存的前缀将失效 · 现 ' + fmtCount(String(sys).length) + ' 字', diffBody(prev, sys), seq);
}

// 工具定义变更行：标题列出新增/移除的工具名，正文是新 tools JSON
function toolsChangeRow(prevTools, tools, seq) {
  const names = (arr) => (Array.isArray(arr) ? arr.map((t) => t && t.function && t.function.name).filter(Boolean) : []);
  const { removed, added } = lineDiffOf(names(prevTools).join('\n'), names(tools).join('\n'));
  const parts = [];
  if (removed.length) parts.push('移除 ' + removed.join('、'));
  if (added.length) parts.push('新增 ' + added.join('、'));
  const sub = parts.length ? parts.join(' · ') : '共 ' + (Array.isArray(tools) ? tools.length : 0) + ' 个';
  const row = document.createElement('div');
  row.className = 'proc-row';
  if (seq != null) row.dataset.seq = seq;
  row.innerHTML =
    '<div class="proc-head">' + chev('p-chev') +
    '<span class="p-name">工具定义变更</span><span class="p-sub">· ' + sub + '</span></div>' +
    '<div class="proc-body"><pre></pre></div>';
  row.querySelector('pre').textContent = tools == null ? '（无工具定义）' : JSON.stringify(tools, null, 2);
  bindProcToggle(row);
  return row;
}

// 运行环境更新行：展示这条快照在下次请求里实际发给模型的消息——正文即发送
// 原文、字数即发送量（差分时是几十~几百字，整份/完整重发时才是全文 5k 级）。
// 形态由 groupTurns 的 env 构造器按组装层口径复刻判定，见 ENV_FULL_RESEND_AFTER。
const ENV_FULL_RESEND_AFTER = 5; // 与后端 context.go envFullResendAfter 同值：连续差分到顶后完整重发一次校准

// envUpdateMessageJS / fullResendEnvJS：组装层 context.go envUpdateMessage /
// fullResendEnv 的前端镜像（行多重集差 + 中文标签清单，逐字节同输出）。
// envnote 展示的正是模型将收到的消息——改任一侧必须同步另一侧。
function envUpdateMessageJS(prev, cur) {
  prev = String(prev); cur = String(cur);
  const prevLines = prev.split('\n'), curLines = cur.split('\n');
  const prevCount = new Map(), curCount = new Map();
  prevLines.forEach((l) => prevCount.set(l, (prevCount.get(l) || 0) + 1));
  curLines.forEach((l) => curCount.set(l, (curCount.get(l) || 0) + 1));
  const added = []; // 新快照里有、旧快照里没有的行（含换值后的新值行）
  for (const l of curLines) {
    const c = prevCount.get(l) || 0;
    if (c > 0) { prevCount.set(l, c - 1); continue; }
    added.push(l);
  }
  const removed = []; // 旧快照里有、新快照里没了的 "- " 条目行（注记行等静默消失）
  for (const l of prevLines) {
    const c = curCount.get(l) || 0;
    if (c > 0) { curCount.set(l, c - 1); continue; }
    if (l.trimStart().startsWith('- ')) removed.push(l);
  }
  let b = '';
  if (removed.length) {
    b = '# 环境更新（条目有增删/变更：移除行 = 早前快照中该条目整行作废，新增行 = 最新状态，均给整行原文；连续差分积累多了系统会自动完整重发一次校准）\n';
    removed.forEach((l) => { b += '【移除】' + l + '\n'; });
    added.forEach((l) => { b += '【新增】' + l + '\n'; });
    return b.replace(/\n+$/, '');
  }
  if (!added.length) return '';
  added.sort(); // 与后端 sort.Strings 同为确定性排序（快照行为 BMP 字符，序一致）
  b = '# 环境更新（新增条目，叠加在更早的快照与更新之上；条目减少或变更时改发增删变更清单）\n';
  added.forEach((l) => { b += l + '\n'; });
  return b.replace(/\n+$/, '');
}

// 完整重发形态：正文用新快照原文，首行标题换成「完整重发」声明（镜像 fullResendEnv）
function fullResendEnvJS(cur) {
  let body = String(cur);
  const i = body.indexOf('\n');
  if (i >= 0 && body.startsWith('# ')) body = body.slice(i + 1);
  return '# 运行环境快照（完整重发，取代更早的快照与更新）\n' + body.replace(/\n+$/, '');
}

function envNoteRow(it) {
  const kindLabel = it.mode === 'full' ? '整份' : it.mode === 'resend' ? '完整重发（差分校准）' : '差分';
  return procTextRow('运行环境更新',
    '随下次请求尾部发送 · ' + fmtCount(String(it.text).length) + ' 字 · ' + kindLabel,
    String(it.text), it.seq);
}

// 时间提示词行：与「运行环境」同款折叠行——模型收到的每条时间消息都在这里
// 可见（一致原则：前端所见不少于模型所收，可折叠但绝不缺失），展开即组装原文
function timeNoteRow(text, seq) {
  const short = (text.match(/(\d{2}:\d{2})/) || [])[1] || '';
  return procTextRow('时间提示', '跟随上一条消息发送 · ' + short, text, seq);
}

// 压缩标记卡片：模型摘要压缩发生在真实位置的一枚小卡片（一两行高）——
// 标明「此处把更早的历史压成了一份摘要、覆盖到哪一条」；完整摘要默认折在
// 卡片里，点卡片头展开查看、再点收起。样式与交付/文件改动卡片同一族
//（同圆角同边框），但刻意不用强调色：压缩是系统内部动作，不该像交付成果
// 那样醒目。卡片在 streamPinned/finishedKeep 里钉住常显——它标记的就是
// 压缩这件事本身，折进过程折叠段等于没标。
function summaryRow(text, origin, covers, seq) {
  const card = document.createElement('div');
  card.className = 'compact-card';
  if (seq != null) card.dataset.seq = seq;
  const sub = (origin === 'auto' ? '自动' : '手动') + (covers ? ' · 覆盖至第 ' + covers + ' 条' : '') +
    ' · 摘要 ' + fmtCount(String(text || '').length) + ' 字 · 点开查看';
  card.innerHTML =
    '<div class="cc-head" role="button" tabindex="0" aria-expanded="false">' +
    '<svg class="cc-ico" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v5"/><path d="m9.5 5.5 2.5 2.5 2.5-2.5"/><path d="M12 21v-5"/><path d="m9.5 18.5 2.5-2.5 2.5 2.5"/><path d="M4 12h16"/></svg>' +
    '<span class="cc-title">此处发生了一次压缩</span><span class="cc-sub">' + sub + '</span>' + chev('cc-chev') +
    '</div><div class="cc-body" hidden><div class="tk-text"></div></div>';
  card.querySelector('.tk-text').textContent = String(text || '');
  const head = card.querySelector('.cc-head');
  const body = card.querySelector('.cc-body');
  const toggle = () => {
    const open = body.hidden;
    body.hidden = !open;
    card.classList.toggle('open', open);
    head.setAttribute('aria-expanded', open ? 'true' : 'false');
  };
  head.onclick = toggle;
  head.onkeydown = (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); toggle(); } };
  return card;
}

// 规则压缩折叠行：在压缩真实发生的位置（连续折叠段的末尾）标注「此处及更早
// 已折叠」。前端仍完整展示原文（所见≥所发），这行只标注发给模型的是精简表示。
// 压缩时后端落的「压缩区结束+变更账本+结论摘录」账本行（report 通道）不再按
// 落库位置画成通知块——正文挂在这里点开查看（groupTurns 按覆盖区间归到段上），
// 行本身保持一两行的紧凑高度，与思考/工具行同一族
function foldRow(rounds, seq, notes) {
  const row = document.createElement('div');
  row.className = 'proc-row';
  if (seq != null) row.dataset.seq = seq;
  const sub = '· 此前 ' + rounds +
    ' 个回合发给模型的是精简版（保留用户输入与正文、工具调用名与参数；省略思考过程、工具结果与环境快照，原文可经历史查询工具读取）';
  if (!notes || !notes.length) {
    row.innerHTML = '<div class="proc-head"><span class="p-name">已折叠</span><span class="p-sub">' + sub + '</span></div>';
    return row;
  }
  row.innerHTML =
    '<div class="proc-head">' + chev('p-chev') +
    '<span class="p-name">已折叠</span><span class="p-sub">· 此前 ' + rounds + ' 个回合已折叠，点开看压缩账本（改了哪些文件、结论摘录）</span></div>' +
    '<div class="proc-body"><div class="tk-text"></div></div>';
  row.querySelector('.tk-text').textContent = notes.join('\n\n');
  bindProcToggle(row);
  return row;
}

// 思考行：标题带字数，点开看原文
function thinkRow(text, seq, opts = {}) {
  const row = document.createElement('div');
  row.className = 'proc-row' + (opts.open ? ' open' : '');
  if (seq != null) row.dataset.seq = seq;
  row.innerHTML =
    '<div class="proc-head">' + chev('p-chev') +
    '<span class="p-name">思考</span><span class="p-sub">· ' + fmtCount(String(text).length) + ' 字</span></div>' +
    '<div class="proc-body"><div class="tk-text"></div></div>';
  row.querySelector('.tk-text').textContent = text;
  bindProcToggle(row);
  return row;
}

// 工具行：只显示工具名与运行态，点开看参数/结果（含图片信封与 4000 字截断）
function toolRow(tc, result, opts = {}) {
  const row = document.createElement('div');
  const name = (tc.function && tc.function.name) || '?';
  row.className = 'proc-row' + (result ? '' : ' running');
  if (opts.seq != null) row.dataset.seq = opts.seq;
  row.innerHTML =
    '<div class="proc-head">' + chev('p-chev') +
    '<span class="p-name">' + esc(name) + '</span>' +
    (result ? '' : '<span class="p-sub">· 运行中…</span>') + '</div>' +
    '<div class="proc-body"></div>';
  const body = row.querySelector('.proc-body');
  let argsDisp = (tc.function && tc.function.arguments) || '';
  try { argsDisp = JSON.stringify(JSON.parse(argsDisp), null, 2); } catch (e) { /* 原样显示 */ }
  const cap1 = document.createElement('div');
  cap1.className = 'pb-cap';
  cap1.textContent = '参数';
  const pre1 = document.createElement('pre');
  pre1.textContent = argsDisp || '（无）';
  body.appendChild(cap1);
  body.appendChild(pre1);
  if (result) {
    let out = String(result.content || '');
    const imgFiles = [];
    out = out.replace(/<image attachment="([^"]+)"[^>]*\/>\n?/g, (_, f) => { imgFiles.push(f); return ''; });
    if (out.length > 4000) out = out.slice(0, 4000) + '\n…';
    const cap2 = document.createElement('div');
    cap2.className = 'pb-cap';
    cap2.textContent = '结果';
    const pre2 = document.createElement('pre');
    pre2.textContent = out || '（无输出）';
    body.appendChild(cap2);
    body.appendChild(pre2);
    if (imgFiles.length) {
      const imgs = document.createElement('div');
      imgs.className = 'tc-imgs';
      for (const f of imgFiles) {
        const a = document.createElement('a');
        const attURL = '/attachments/' + encodeURIComponent(f);
        a.target = '_blank';
        a.title = '点击查看原图';
        const im = document.createElement('img');
        im.className = 'tc-img';
        im.loading = 'lazy';
        im.alt = '图片附件';
        if (window.JX_REMOTE) { // 远程模式：附件经隧道 blob 化后再上屏
          JX_REMOTE.assetURL(attURL).then((u) => { a.href = u; im.src = u; });
        } else {
          a.href = attURL;
          im.src = attURL;
        }
        a.appendChild(im);
        imgs.appendChild(a);
      }
      body.appendChild(imgs);
    }
  }
  bindProcToggle(row);
  return row;
}

/* ---------- 折叠段：折叠条 + 隐藏容器 ---------- */

  // 上下文材料条目：时间提示、首轮尾部材料、运行环境更新、系统提示词/工具定义变更。
  // 它们与思考/工具过程一起折叠（想核对「模型看到了什么」时再展开），
  // 渲染的两个路径（完成态/流式）都按此归类。
function isMatItem(it) {
  return it.k === 'time' || it.k === 'prompttail' || it.k === 'envnote' || it.k === 'syschange' || it.k === 'toolschange';
}
// 注意：fold（已折叠标注行）刻意不归入材料折叠——它是用户要看的状态标注，
// 与压缩摘要行同样常显，藏进材料分组就失去「这里压缩过」的提示意义

// 折叠条标签：折起多少行是总体计数，放最前面；后面是调用 AI 几次、执行工具几次、
// 以及上下文材料/系统通知条数。行数**永远在**（执行中与完成态同一个函数出标签，
// 不会一处有一处没有）。
// 步数口径 = 向模型发送请求的次数：一步 = 主循环一次迭代（一次模型调用，该步触发的
// 全部工具调用——含并行——都算同一步），与后端 agent.go 的 step 计步一致
// （见《最大步数计步口径排查报告.md》）。消息数据里没有现成的步数字段，用「本段
// 出现过的 assistant 行数」精确还原：think/text/tool/subtask/plancard 条目都由
// assistant 行拆出、同一行拆出的条目 seq 相同，按 seq 去重即行数（一次返回多个并行
// 工具调用只产一条 assistant 行）；该口径含无工具调用的最终答复轮，比 assistant.go
// 摘要只数带 tool_calls 的行更准。
function foldLabel(cnt) {
  const parts = ['已折叠 ' + cnt.rows + ' 行消息'];
  if (cnt.steps) parts.push('调用 AI ' + cnt.steps + ' 次');
  if (cnt.tools) parts.push(cnt.tools + ' 次工具调用');
  if (cnt.mat) parts.push(cnt.mat + ' 条上下文材料');
  if (cnt.note) parts.push(cnt.note + ' 条系统通知');
  return parts.join(' · ');
}

// seen：**回合级**共享去重集（由渲染层逐段传入）。锚点（长正文/收尾答案/计划卡/
// 通知）会把同一助手行的条目切到前后两段，各段若各建一个 Set，这一次调用就被两段
// 各计一遍（实测：主对话 seq 273 的回合折成两段，标签「调用 AI 1 次」+「调用 AI 2
// 次」= 3，而该回合真实只有 2 次调用——seq 277 在两段里都被数了）。所以整个回合
// 共用一个 Set，本段只报**本段新增**的步数，各段合计仍等于真实调用次数。
function secCount(items, seen) {
  const cnt = { steps: 0, tools: 0, mat: 0, note: 0, rows: items.length };
  const steps = seen instanceof Set ? seen : new Set();
  let added = 0; // 本段新计入的步数（前面各段已计过的不再计）
  items.forEach((it) => {
    // 工具调用次数：派发子任务也算一次工具调用（与改版前的口径一致）；条目按段
    // 划分，每条只出现在一段里，无需去重（步数要跨段去重，因为同一行的条目会被切段）
    if (it.k === 'tool' || it.k === 'subtask') cnt.tools++;
    if (it.k === 'think' || it.k === 'text' || it.k === 'tool' || it.k === 'subtask' || it.k === 'plancard') {
      // 旧数据缺 seq（null）时用条目对象本身当键：同一回合的条目对象在各段间是同一份
      // 引用，跨段照样认得出「这条已经计过」，比按段内下标（跨段会撞号）更准
      const k = it.seq != null ? 's' + it.seq : it;
      if (steps.has(k)) return;
      steps.add(k);
      added++;
    } else if (it.k === 'note') cnt.note++;
    else if (isMatItem(it)) cnt.mat++;
  });
  cnt.steps = added;
  return cnt;
}

// 折叠段 = 折叠条按钮 + until-found 隐藏容器；点击只切本段显隐，不触发整列重渲（滚动位置不跳）
// 标签一律由 foldLabel 产出——执行中与完成态长得一样，差别只在可见策略（见 renderTurnProcess）
// opts.seen：同一回合各段共用的步数去重集（见 secCount），由渲染层逐段透传
function foldSection(items, key, opts = {}) {
  const sec = document.createElement('div');
  sec.className = 'tf-sec';
  if (key) sec.dataset.key = key;
  const bar = document.createElement('button');
  bar.type = 'button';
  bar.className = 'turn-fold';
  const label = document.createElement('span');
  label.className = 'tf-label';
  label.textContent = foldLabel(secCount(items, opts.seen));
  bar.appendChild(label);
  bar.insertAdjacentHTML('beforeend', chev('tf-chev'));
  const body = document.createElement('div');
  body.className = 'tf-body';
  for (const it of items) body.appendChild(renderItem(it, { sid: opts.sid }));
  const open = opts.open === true;
  bar.classList.toggle('open', open);
  if (open) showEl(body); else hideEl(body);
  bar.onclick = () => {
    const nowOpen = body.hasAttribute('hidden');
    setSecOpen(sec, nowOpen);
    if (nowOpen) { turnOpen[key] = true; pinViewForRead(sec); } // 手动展开记忆：之后不再自动折回
    else delete turnOpen[key];        // 收起后回到自动折叠
  };
  sec.appendChild(bar);
  sec.appendChild(body);
  return sec;
}

function setSecOpen(sec, open) {
  const bar = sec.querySelector('.turn-fold');
  const body = sec.querySelector('.tf-body');
  if (!bar || !body) return;
  bar.classList.toggle('open', open);
  if (open) showEl(body); else hideEl(body);
}

/* 继承段：子对话开头的「来自主对话」历史，整段折叠成一条，点开是完整对话流。
   内部复用主线渲染（用户气泡 + 回合渲染），所以外观与正文完全一致；
   父对话的系统提示词快照行也在其中（一目了然「含系统提示词」）。
   turns 由 renderMessages 分组后传入（分组状态上的基线随 turns.base 带出，
   供正文段接着比——子对话首条 prompt 快照与父相同时不再重复显示）。 */
function inheritBlock(turns, items, sid) {
  const key = sid + ':inherit';
  const sec = document.createElement('div');
  sec.className = 'tf-sec inherit';
  sec.dataset.key = key;
  const bar = document.createElement('button');
  bar.type = 'button';
  bar.className = 'turn-fold';
  const label = document.createElement('span');
  label.className = 'tf-label';
  // 环境快照行不显示（与正文一致），计数按可见条数
  const n = items.filter((m) => m.kind !== 'env').length;
  label.textContent = '来自主对话 · ' + n + ' 条（含系统提示词）';
  bar.appendChild(label);
  bar.insertAdjacentHTML('beforeend', chev('tf-chev'));
  const body = document.createElement('div');
  body.className = 'tf-body';
  turns.forEach((turn, ti) => {
    if (turn.sysHead) body.appendChild(sysHeadRow(turn.sysHead, turn.sysHeadSeq));
    if (turn.headTools) body.appendChild(toolsRow(turn.headTools, turn.headToolsSeq));
    if (turn.userMsg) body.appendChild(userBubble(turn.userMsg, viewOf(sid)));
    renderTurnProcess(body, turn, key + ':' + ti);
  });
  const open = turnOpen[key] === true;
  bar.classList.toggle('open', open);
  if (open) showEl(body); else hideEl(body);
  bar.onclick = () => {
    const nowOpen = body.hasAttribute('hidden');
    setSecOpen(sec, nowOpen);
    if (nowOpen) { turnOpen[key] = true; pinViewForRead(sec); }
    else delete turnOpen[key];
  };
  sec.appendChild(bar);
  sec.appendChild(body);
  return sec;
}

// Ctrl+F 命中折叠内容时浏览器揭开 hidden，这里同步箭头态（过程行与折叠段都处理；
// 折叠段可以嵌套——继承段里还有回合折叠段，所以逐层往上同步）
document.addEventListener('beforematch', (e) => {
  let el = e.target;
  while (el && el.closest) {
    const row = el.closest('.proc-row');
    if (row) { setProcOpen(row, true); el = row.parentElement; continue; }
    const sec = el.closest('.tf-sec');
    if (sec) { setSecOpen(sec, true); el = sec.parentElement; continue; }
    break;
  }
});

/* ---------- 回合分组与条目渲染（类型契约的分发层，见《消息类型规范.md》） ---------- */

// 时间提示词的组装态（与后端 context.go 的 App.timeAfter 同一规则）：从消息自身
// 落库时刻派生「（发送时间：YYYY-MM-DD HH:MM）」，分钟粒度。一致原则——前端
// 所见不少于模型所收：模型收到的每条时间消息这里都要能展开看到，派生失败返回
// 空串（后端同样跳过，两侧口径一致）。
function timeNoteOf(m) {
  const raw = String(m.createdAt || '').trim();
  if (!raw) return '';
  const d = new Date(raw.replace(' ', 'T'));
  if (isNaN(d.getTime())) return '';
  const p2 = (n) => String(n).padStart(2, '0');
  return '（发送时间：' + d.getFullYear() + '-' + p2(d.getMonth() + 1) + '-' + p2(d.getDate()) +
    ' ' + p2(d.getHours()) + ':' + p2(d.getMinutes()) + '）';
}

// 用户侧消息（chat 真人 / aiuser）后跟的时间提示词：作为上下文材料条目入回合，
// 展示与运行环境同一套折叠行（见 timeNoteRow），位置与模型收到的顺序一致——
// 紧跟在消息后面、在其后的环境材料之前。
function pushTimeNote(st, m) {
  const txt = timeNoteOf(m);
  if (txt) st.cur.items.push({ k: 'time', seq: m.seq, text: txt });
}

// 规则压缩账本行：压缩完成后落库的「══════ 压缩区结束（第 N 次压缩 · 覆盖
// seq X–Y …）══════ + 变更账本 + 结论摘录」report 行（后端 appendCompactNote，
// 前缀见 context.go compactNotePrefix）。它不按落库位置画通知块——正文挂到
// 折叠段边界的「已折叠」行上点开查看（groupTurns 按覆盖区间归位）
function isCompactNoteRow(m) {
  return m.kind === 'report' && String(m.content ?? '').startsWith('══════ 压缩区结束');
}

// 行的展示类型：新行一律带显式 kind（后端保证）；旧库残留的 chat+【】前缀
// user 行按 report 兼容（规范 §5）。此后一切分发只看 kind，不猜内容。
function displayKindOf(m) {
  const k = m.kind;
  if (k === 'chat' || k === 'report' || k === 'aiuser' || k === 'summary' || k === 'env' || k === 'prompt' || k === 'inherit' || k === 'request' || k === 'audit') return k;
  if ((!k || k === 'chat') && m.role === 'user' && isSysNote(String(m.content ?? ''))) return 'report';
  return 'chat';
}

// 存储行类型 → 条目构造器（契约的注册表）。后端新增类型：在此注册一行
// 构造器即可获得标准样式；需要新样式先在规范 §3 登记样式基元。
// 构造器往当前回合塞条目；request/audit 是审计工件不展示（导出功能的数据源，
// 正文由服务端下发瘦身时置空，见 server.go trimMessagesForUI）。
const ROW_ITEM_BUILDERS = {
  // 对话内容：user 开新回合；assistant 拆 think/text/tool 条目（复合行拆分规则见规范 §3）
  chat(m, st) {
    if (m.role === 'user') {
      st.cur = { userMsg: m, items: [] };
      st.turns.push(st.cur);
      pushTimeNote(st, m);
      return;
    }
    if (m.role !== 'assistant') return; // tool 行经 resultOf 配对进工具条目，不单列
    st.ensure();
    if (m.thinking) st.cur.items.push({ k: 'think', seq: m.seq, text: m.thinking });
    if (m.content) st.cur.items.push({ k: 'text', seq: m.seq, m, text: String(m.content) });
    for (const tc of m.tool_calls || []) {
      const n = (tc.function && tc.function.name) || '';
      st.cur.items.push({
        k: (n === 'dispatch_task' || n === 'dispatch_sub' || n === 'send_to_session') ? 'subtask'
          : (n === 'exit_plan_mode' ? 'plancard' : 'tool'),
        seq: m.seq, tc, result: st.resultOf[tc.id],
      });
    }
  },
  // AI 发出的用户消息（数字助理派发的指令/派发回报/子对话派发/待办转交）：
  // 与用户消息完全同权——开新回合，上一回合随之收口（最终输出带出复制/压缩
  // 等回合按钮）；后面同样跟一条时间提示词；气泡由 userBubble 渲染为左对齐
  // 灰底的「AI 用户」变体（.msg.user.ai，正文按 Markdown 渲染），不可编辑重发
  aiuser(m, st) {
    st.cur = { userMsg: m, items: [] };
    st.turns.push(st.cur);
    pushTimeNote(st, m);
  },
  // 系统注入通知：归入当前回合常显
  report(m, st) {
    // 压缩通知（origin=compact）不再显示为通知块：压缩发生的位置已有常显的
    // 压缩卡片标注，两处都画会重复；这条通知本身仍会发给模型，只是界面不画
    if (m.origin === 'compact') return;
    // 规则压缩的「压缩区结束」账本行同样不画通知块：它落库在压缩发生时刻
    //（会话末尾），按落库位置画就是一整块直出的长通知——正文由 groupTurns
    // 挂到折叠段边界的「已折叠」行上点开查看，位置与形态才是压缩真实发生的
    if (isCompactNoteRow(m)) return;
    st.ensure();
    st.cur.items.push({ k: 'note', m });
  },
  // 压缩摘要：不按落库位置产条目——卡片由 groupTurns 的 sumAt 归位到覆盖
  // 边界（最后一条被压缩的消息之后）就地插入，位置才是压缩真实发生的地方
  summary() {},
  // 运行环境快照：按组装层（context.go envRowRender）同一口径复刻「模型实际
  // 收到什么」——首条（或摘要折叠后首条）整份、其后差分（envUpdateMessageJS
  // 行多重集差）、连续差分满 ENV_FULL_RESEND_AFTER 后完整重发校准、同文不发；
  // 已被摘要覆盖（seq≤envBoundary）或规则压缩整行省略（omit）的快照模型已
  // 看不到，不再显示。envnote 的正文与字数因此与下次请求的发送内容一致
  // （所见即所发：差分时几十~几百字，整份/完整重发才显示全文字数）。
  env(m, st) {
    if (m.seq <= st.envBoundary) return;   // 摘要已覆盖：模型看不到这条快照
    if (m.compact === 'omit') return;      // 规则压缩整行省略：同上
    const txt = String(m.content ?? '');
    let mode = null, body = '';
    if (st.lastEnv == null) {
      mode = 'full'; body = txt; st.envUpdates = 0;
    } else if (st.envUpdates >= ENV_FULL_RESEND_AFTER) {
      mode = 'resend'; body = fullResendEnvJS(txt); st.envUpdates = 0;
    } else {
      body = envUpdateMessageJS(st.lastEnv, txt);
      if (body) { mode = 'diff'; st.envUpdates++; }
    }
    st.lastEnv = txt; // 基线无条件推进（同文/注记过期静默消失也不回退，与后端一致）
    if (!mode) return;
    st.ensure();
    st.cur.items.push({ k: 'envnote', seq: m.seq, text: body, mode });
  },
  // 提示词拼装快照：变化驱动展示（规范 §4）——首轮完整交代拼装结构
  // （工具定义 + 系统提示词在用户消息上方、运行环境在请求末尾，即请求真实排布），
  // 此后仅在系统提示词/工具定义变更时出折叠行；环境变化由 env 行负责
  prompt(m, st) {
    let sys = '', env = '', tools = null;
    try {
      const j = JSON.parse(String(m.content ?? '{}'));
      sys = j.sys || ''; env = j.env || '';
      if (Array.isArray(j.tools)) tools = j.tools;
    } catch (e) { /* 忽略坏行 */ }
    st.ensure();
    const toolsJson = tools == null ? '' : JSON.stringify(tools);
    if (!st.sawPrompt) {
      // 首轮：lastEnv 同步为本轮环境，随后的同文快照行会被去重
      st.cur.sysHead = sys; st.cur.sysHeadSeq = m.seq;
      // 工具定义跟着提示词走（都在消息序列之前展示），运行环境留在尾部材料里
      st.cur.headTools = tools; st.cur.headToolsSeq = m.seq;
      st.cur.items.push({ k: 'prompttail', seq: m.seq, env });
      st.lastEnv = env;
    } else {
      // 空壳行（服务端把与上一条逐字节相同的快照瘦成 {} 后）不产生任何变更展示：
      // 有值才比较、才推进基线——sys/tools 真实“变空”不可能发生，空值只来自瘦身
      if (sys && sys !== st.lastSys) st.cur.items.push({ k: 'syschange', seq: m.seq, sys, prev: st.lastSys });
      if (toolsJson && toolsJson !== st.lastToolsJson) st.cur.items.push({ k: 'toolschange', seq: m.seq, tools, prevTools: st.lastTools });
    }
    st.sawPrompt = true;
    if (sys) st.lastSys = sys;
    if (toolsJson) { st.lastToolsJson = toolsJson; st.lastTools = tools; }
  },
  // 继承标记行由 inheritBlock 分段处理（这里不产条目）
  inherit() {},
  // 审计工件不展示
  request() {}, audit() {},
};

// 按真实用户发言切回合；系统通知（回报/中断/上传）归入当前回合且常显；
// assistant 消息按序拆成思考/正文/工具条目，tool 消息经 resultOf 配对进条目。
// 每次请求都会完整重发系统提示词（协议如此）——「不变」= 逐字节相同，
// 前缀缓存据此命中，所以上下文材料只在变化时展示。
// base（可空）：分组状态的初始基线——子对话的正文段从这里接过继承段分组后的
// 基线，自己首条 prompt 快照与父对话逐字节相同时就不再显示「工具定义/系统
// 提示词」折叠行（它们已折在「来自主对话」段里；真换了模式/工具集才显示变更行）。
function groupTurns(msgs, resultOf, base) {
  const st = {
    turns: [], cur: null, resultOf,
    sawPrompt: false, lastSys: '', lastToolsJson: '', lastTools: null, lastEnv: null,
    ...(base || {}),
  };
  // 环境快照的组装口径状态（镜像 context.go buildContext）：envUpdates 差分
  // 计数（继承段接力），envBoundary 摘要覆盖边界（边界内的快照模型已看不到）
  st.envUpdates = st.envUpdates || 0;
  st.envBoundary = 0;
  st.ensure = () => {
    if (!st.cur) { st.cur = { userMsg: null, items: [] }; st.turns.push(st.cur); }
  };
  // 规则压缩的「已折叠」边界：compact 非空的行属于折叠区（发给模型的是精简
  // 表示或整行省略，原文前端照常完整展示——所见≥所发）。连续折叠段的最后一
  // 条**可见**行后插一条折叠标注行，位置即压缩真实发生的位置（可能在好几轮
  // 之前）；多段折叠各自成行。折叠回合数按段内的回合起始行（chat-user/aiuser/report）计。
  // 注意只看可见对话行（chat/aiuser/report/env）：prompt/request/audit 等归档行
  // 本就不展示也不带 compact 标记，把它们算进连续性判断会把折叠区打碎、
  // 生成一串多余的边界行
  const foldVisible = (m) => ['chat', 'aiuser', 'report', 'env'].includes(m.kind);
  const foldEndSeq = new Map();
  let foldRounds = 0;
  for (let i = 0; i < msgs.length; i++) {
    const m = msgs[i];
    if (!foldVisible(m)) continue;
    if (!m.compact) continue;
    if (m.kind === 'report' || ((m.kind === 'chat' || m.kind === 'aiuser') && m.role === 'user')) foldRounds++;
    let nextVisible = null;
    for (let j = i + 1; j < msgs.length && !nextVisible; j++) if (foldVisible(msgs[j])) nextVisible = msgs[j];
    if (!nextVisible || !nextVisible.compact) foldEndSeq.set(m.seq, foldRounds);
  }
  // 压缩摘要按覆盖边界归位：摘要行落库在会话末尾，但卡片要显示在压缩真实
  // 发生的地方——最后一条被压缩的消息（coversUpTo）之后、保留的原文之前，
  // 几乎总是恰好落在下一条用户消息上方。先扫一遍建 seq→摘要 的映射，
  // 主循环处理完那条消息后就地插卡片（summary 构造器因此不再产条目）。
  const sumAt = new Map();
  for (const m of msgs) {
    if (displayKindOf(m) !== 'summary' || !m.coversUpTo) continue;
    sumAt.set(m.coversUpTo, m);
    if (m.coversUpTo > st.envBoundary) st.envBoundary = m.coversUpTo; // 组装层 boundary 同口径：取摘要覆盖边界最大值
  }
  // 规则压缩账本行按覆盖区间归到折叠段：账本正文写明「覆盖 seq X–Y」，
  // Y 落在哪个段的边界之内就归哪段（段边界是段内最后一条可见行，Y 有可能
  // 压在段尾的归档行上、略过边界，所以取第一个边界 ≥ Y 的段）。归好后挂在
  // fold 条目上点开查看——不在落库位置（会话末尾）画长通知块
  const compactNotes = [];
  for (const m of msgs) {
    if (!isCompactNoteRow(m)) continue;
    const hit = String(m.content ?? '').match(/覆盖 seq (\d+)–(\d+)/);
    compactNotes.push({ to: hit ? +hit[2] : 0, text: String(m.content ?? '') });
  }
  compactNotes.sort((a, b) => a.to - b.to);
  for (const m of msgs) {
    const build = ROW_ITEM_BUILDERS[displayKindOf(m)];
    if (build) build(m, st);
    if (foldEndSeq.has(m.seq)) {
      st.ensure();
      const notes = [];
      while (compactNotes.length && compactNotes[0].to <= m.seq) notes.push(compactNotes.shift().text);
      st.cur.items.push({ k: 'fold', seq: m.seq, rounds: foldEndSeq.get(m.seq), notes });
    }
    if (sumAt.has(m.seq)) {
      const s = sumAt.get(m.seq);
      st.ensure();
      st.cur.items.push({ k: 'summary', seq: s.seq, text: String(s.content ?? ''), origin: s.origin, covers: s.coversUpTo });
    }
  }
  st.base = { sawPrompt: st.sawPrompt, lastSys: st.lastSys, lastToolsJson: st.lastToolsJson, lastTools: st.lastTools, lastEnv: st.lastEnv, envUpdates: st.envUpdates };
  st.turns.base = st.base; // 挂在返回的 turns 上：renderMessages 接力给正文段的分组
  return st.turns;
}

// 消息时间戳文案：优先 18:45（同一天），跨天补日期 09-28 18:45；时间缺失返回空串。
// 消息行自带的 createdAt 由后端落库时写入（见 StoredMessage.CreatedAt）。
function msgTimeLabel(iso) {
  if (!iso) return '';
  const d = new Date(String(iso).replace(' ', 'T'));
  if (isNaN(d.getTime())) return '';
  const p2 = (n) => String(n).padStart(2, '0');
  const hm = p2(d.getHours()) + ':' + p2(d.getMinutes());
  const now = new Date();
  const sameDay = d.getFullYear() === now.getFullYear() && d.getMonth() === now.getMonth() && d.getDate() === now.getDate();
  return sameDay ? hm : p2(d.getMonth() + 1) + '-' + p2(d.getDate()) + ' ' + hm;
}

// 时间标签节点（放进操作行：用户消息在复制左侧，模型最终输出在最右）
function actTime(iso, side) {
  const el = document.createElement('span');
  el.className = 'act-time' + (side ? ' ' + side : '');
  el.textContent = msgTimeLabel(iso);
  el.title = iso ? String(iso).replace('T', ' ') : '';
  return el;
}

// 用户消息气泡：气泡上方悬停操作（时间 / 复制 / 编辑重发）。编辑只对真实用户消息
// 开放（kind=chat）；系统注入的回报/上传/目标续跑/继承标记不参与。AI 发出的
// 用户消息（kind=aiuser，数字助理派发的指令/回报）同款气泡但灰底（.ai），
// 操作行有时间与复制、没有编辑——它是 AI 说的，不能编辑重发。
// aiuser 正文按 Markdown 渲染（2026-11-05 用户在三档预览页拍板方向 A）：派发
// 回报里的「最终答复全文」是任务模型按正文习惯写的，纯文本显示时表格/井号/
// 星号全裸露；渲染器与数字助理正文同款（md()）。复制按钮仍复制原文，不受影响。
function userBubble(m, view) {
  const wrap = document.createElement('div');
  wrap.className = 'msg user' + (m.kind === 'aiuser' ? ' ai' : '');
  wrap.dataset.seq = m.seq; // 搜索命中定位用
  const b = document.createElement('div');
  b.className = 'bubble' + (m.kind === 'aiuser' ? ' md' : '');
  appendQuotedBody(b, String(m.content ?? ''), m.kind === 'aiuser'); // 带引用前缀的消息拆成灰色引用行 + 正文（aiuser 正文走 MD 渲染）
  wrap.appendChild(b);
  if ((m.kind === 'chat' || m.kind === 'aiuser') && view) {
    const acts = document.createElement('div');
    acts.className = 'msg-actions user-acts';
    acts.appendChild(actTime(m.createdAt)); // 发送时间：排在复制按钮左边
    const cp = document.createElement('button');
    cp.className = 'act-copy';
    const cpOrig = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>复制';
    cp.innerHTML = cpOrig;
    cp.onclick = () => {
      navigator.clipboard.writeText(String(m.content ?? '')).then(() => {
        cp.textContent = '已复制';
        setTimeout(() => { cp.innerHTML = cpOrig; }, 1200);
      });
    };
    acts.appendChild(cp);
    if (m.kind === 'chat') {
      const ed = document.createElement('button');
      ed.className = 'act-copy act-edit';
      ed.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>编辑';
      ed.title = '修改这条消息并重新发送；它之后的对话记录将被移除';
      ed.onclick = () => editUserMsg(m, view, wrap);
      acts.appendChild(ed);
    }
    wrap.insertBefore(acts, b); // 操作行放气泡上方：紧贴自己的消息，放下方时视觉上更像上一轮的结尾
  }
  return wrap;
}

// 后台模型摘要压缩进行中的会话（与后端 compactRunning 同源，经 SSE 维护）：
// 渲染消息操作条时据此还原「后台压缩中…」按钮态——切走对话再切回来，
// DOM 重建后状态不丢；压缩完成/失败事件到达时移除并刷新
const compactingSess = new Set();

// 压缩完成后按会话所在视图刷新（主视图或右栏子面板），不切换视图
function refreshAfterCompact(sid) {
  if (mainView.state.cur && mainView.state.cur.id === sid) {
    openSession(sid);
  } else if (typeof subState !== 'undefined' && subState.cur && subState.cur.id === sid) {
    refreshDrawer();
    openSubSession(sid);
  }
}

// 悬停操作（仅回合最终输出）：复制原文 / 手动压缩 / 分支 / 插件扩展位
function msgActions(m, opts) {
  opts = opts || {};
  const acts = document.createElement('div');
  acts.className = 'msg-actions';
  const cp = document.createElement('button');
  cp.className = 'act-copy';
  const cpOrig = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/></svg>复制';
  cp.innerHTML = cpOrig;
  cp.onclick = () => {
    navigator.clipboard.writeText(String(m.content ?? '')).then(() => {
      cp.textContent = '已复制';
      setTimeout(() => { cp.innerHTML = cpOrig; }, 1200);
    });
  };
  acts.appendChild(cp);
  const cb = document.createElement('button');
  cb.className = 'act-copy act-compact';
  const cbOrig = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v5"/><path d="m9.5 5.5 2.5 2.5 2.5-2.5"/><path d="M12 21v-5"/><path d="m9.5 18.5 2.5-2.5 2.5 2.5"/><path d="M4 12h16"/></svg>压缩';
  const cbBusy = () => compactingSess.has(m.sessionId || (state.cur && state.cur.id));
  cb.innerHTML = cbOrig;
  cb.title = '折叠这一轮及更早的对话（这一轮本身也在内）：保留用户输入与正文、工具调用名与参数，去掉思考过程与工具结果。这一段都已折叠过时再点一次，才进一步只留用户输入与结论。原文可随时用历史查询工具搜回';
  // 后台压缩进行中（本窗口点的或别处点的）：渲染恢复时还原按钮态
  if (cbBusy()) { cb.textContent = '后台压缩中…'; cb.disabled = true; }
  cb.onclick = async () => {
    if (!state.cur || state.streaming) { toast('对话正在执行，等这一轮结束再压缩'); return; }
    const sid = m.sessionId || state.cur.id;
    cb.textContent = '后台压缩中…';
    cb.disabled = true;
    try {
      const r = await api('/api/sessions/' + sid + '/compact', { method: 'POST', body: JSON.stringify({ upTo: m.seq }) });
      if (r && r.async) {
        // 模型摘要已放后台（202 即回，大历史摘要要一两分钟）：不等它——登记
        // 状态后即可切走干别的，完成/失败由 SSE session_updated 驱动刷新
        compactingSess.add(sid);
      } else {
        // 规则折叠零模型调用、同步已完成：立即刷新
        cb.textContent = '已压缩';
        refreshAfterCompact(sid);
        setTimeout(() => { cb.innerHTML = cbOrig; cb.disabled = false; }, 1500);
      }
    } catch (e) {
      toast('压缩失败：' + e.message, 6000);
      cb.innerHTML = cbOrig;
      cb.disabled = false;
    }
  };
  acts.appendChild(cb);
  if (opts.final) {
    const bk = document.createElement('button');
    bk.className = 'act-copy act-branch';
    bk.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="6" cy="5" r="2.2"/><circle cx="6" cy="19" r="2.2"/><circle cx="18" cy="9" r="2.2"/><path d="M6 7.2v9.6"/><path d="M18 11.2c0 3.4-3.2 4-7.5 4.3"/></svg>分支';
    bk.title = '从这条回答处复制出一个新会话并切换过去；当前对话保持不变';
    bk.onclick = () => forkFromAnswer(m, bk);
    acts.appendChild(bk);
  }
  // 消息引用存到行上：组件（如朗读）注册钩子晚于本行渲染时，registerMessageAction
  // 靠它对已渲染的行回放钩子（远程中继下组件加载秒级，历史消息常先于组件渲染）
  acts._jxMsg = m;
  for (const fn of JX._msgActions) { try { fn(acts, m); } catch (e) { console.warn('[JX] action', e); } }
  acts.appendChild(actTime(m.createdAt, 'act-time-end')); // 回复时间：排在最右
  return acts;
}

// 从某条回答处分支出新会话：消息按 seq 前缀复制，创建成功后切过去看。
// 会话执行中也可分支（后端只复制到该 seq，前缀稳定）。
async function forkFromAnswer(m, btn) {
  if (!m.sessionId) return;
  if (btn) { btn.disabled = true; btn.classList.add('ok'); }
  try {
    const fork = await api('/api/sessions/' + m.sessionId + '/fork', {
      method: 'POST',
      body: JSON.stringify({ upTo: m.seq }),
    });
    // 响应就是新会话 meta：先合入列表再切换，乐观切换的 brief 必命中——不等
    // loadSessions 的防抖往返（那会给「界面还停在源会话」留窗口，窗口期发的
    // 消息会落进源会话）
    upsertSessionBrief(fork);
    loadSessions(); // 后台刷新排序等最终一致，不阻塞切换
    if (mainView.state.cur && mainView.state.cur.id === m.sessionId) {
      openSession(fork.id);
    } else if (subState.cur && subState.cur.id === m.sessionId) {
      refreshDrawer();
      openSubSession(fork.id);
    }
  } catch (e) {
    alert('分支失败：' + e.message);
  }
  if (btn) { btn.disabled = false; btn.classList.remove('ok'); }
}

function renderRowItem(it, opts) {
  if (it.k === 'prompttail') return promptTailRows(it.env, it.seq);
  if (it.k === 'envnote') return envNoteRow(it);
  if (it.k === 'time') return timeNoteRow(it.text, it.seq);
  if (it.k === 'syschange') return sysChangeRow(it.prev, it.sys, it.seq);
  if (it.k === 'toolschange') return toolsChangeRow(it.prevTools, it.tools, it.seq);
  if (it.k === 'summary') return summaryRow(it.text, it.origin, it.covers, it.seq);
  if (it.k === 'fold') return foldRow(it.rounds, it.seq, it.notes);
  if (it.k === 'think') return thinkRow(it.text, it.seq);
  if (it.k === 'tool') return toolRow(it.tc, it.result, { seq: it.seq });
  if (it.k === 'plancard') return planCardRow(it.tc, it.seq, it.result);
  if (it.k === 'subtask') {
    const w = document.createElement('div');
    if (it.seq != null) w.dataset.seq = it.seq;
    w.appendChild(subTaskCard(it.tc, it.result));
    return w;
  }
  if (it.k === 'text') {
    const wrap = document.createElement('div');
    wrap.className = 'msg assistant';
    wrap.dataset.seq = it.seq; // 搜索命中定位用
    const body = document.createElement('div');
    body.className = 'md';
    body.innerHTML = md(it.text);
    wrap.appendChild(body);
    // 操作按钮只挂在回合最终输出下（正文下方）：中间正文不带，过程拼得更紧凑
    if (opts && opts.final) wrap.appendChild(msgActions(it.m, { final: true }));
    return wrap;
  }
  // 系统通知：常显独立块
  const wrap = document.createElement('div');
  wrap.className = 'msg sysnote';
  const b = document.createElement('div');
  b.className = 'bubble';
  b.textContent = String(it.m.content ?? '');
  wrap.appendChild(b);
  return wrap;
}

// 过程行渲染出口 = 原分发（renderRowItem）+ 行级展开记忆的恢复：proc-row 系行
// 挂 data-rk 行键（kind + seq；工具/子任务/计划卡用 tool_call_id——同一条
// assistant 消息的多个调用共享 seq，靠它区分），展开记忆（procOpenMem）命中时
// 直接以展开态出现。执行中尾部每个消息事件都整段重建最后一回合，行上挂的
// 展开态随 DOM 换掉——不查表恢复的话，用户点开的思考行下一批消息到达就被折回。
function renderItem(it, opts) {
  const el = renderRowItem(it, opts);
  if (!el || !el.classList || !el.classList.contains('proc-row')) return el;
  const rk = it.seq != null
    ? (it.k + ':' + ((it.tc && it.tc.id) ? it.tc.id : it.seq))
    : '';
  if (!rk) return el;
  el.dataset.rk = rk;
  const sid = (opts && opts.sid) || '';
  if (sid && procOpenMem.get(sid + ':' + rk) === '1') setProcOpen(el, true);
  return el;
}

// 【派发回报】消息中出现过的子会话 id（渲染期间有效）：子任务卡据此在刷新/
// 重新打开会话后也能正确显示「已完成」，不依赖内存里的 subDone 标记
let reportedSubs = new Set();
function scanReported(msgs) {
  const s = new Set();
  for (const m of msgs) {
    if (m.role !== 'user') continue;
    const txt = String(m.content ?? '');
    if (!txt.startsWith('【派发回报】')) continue;
    const hit = txt.match(/（([^\s（）]+)）/); // 回报格式：…「标题」（会话id）本轮结束
    if (hit) s.add(hit[1]);
  }
  return s;
}

function subActNode(sid) {
  return document.querySelector(`.subtask-card[data-sid="${sid}"] .st-act`);
}

function renderSubActs(sid) {
  const node = subActNode(sid);
  if (!node) return;
  const arr = subActs[sid] || [];
  node.textContent = arr.length ? arr.join('\n') : '正在准备…';
}

function pushSubAct(ev) {
  const arr = (subActs[ev.sessionId] = subActs[ev.sessionId] || []);
  arr.push(ev.output ? `✓ ${ev.name}` : `→ ${ev.name} ${String(ev.args || '').slice(0, 60)}`);
  if (arr.length > 6) arr.shift();
  renderSubActs(ev.sessionId);
}

function bindOrphanCard(sid) {
  // 运行中的卡片此刻还没有会话 id（id 要等工具结果才能解析出来），
  // 后端按顺序执行派发，这里按到达顺序绑定
  const orphan = Array.from(document.querySelectorAll('.subtask-card.running'))
    .find((c) => !c.dataset.sid);
  if (orphan) orphan.dataset.sid = sid;
}

function subTaskCard(tc, result) {
  let args = {};
  try { args = JSON.parse(tc.function && tc.function.arguments) || {}; } catch (e) { /* 参数原样忽略 */ }
  const out = String((result && result.content) || '');
  // 后台派发：立即返回文本里带子会话 id；旧版同步结果则以「已完成」呈现
  // （id 不能用 \S+ 贪婪匹配：后面的文案没有空格，会把整句中文都吞进 id）
  const sidMatch = out.match(/已派发后台执行：会话 ([^\s「]+)「/) || out.match(/^会话ID:\s*(\S+)$/m);
  const sid = sidMatch ? sidMatch[1] : '';
  const dispatched = out.includes('已派发后台执行');
  const done = (sid && reportedSubs.has(sid)) || subDone[sid] || (result && !dispatched);
  const running = !done;
  const title = args.title || String(args.task || '').slice(0, 30) || '子任务';
  const d = document.createElement('div');
  d.className = 'subtask-card ' + (done ? 'done' : 'running');
  if (sid) d.dataset.sid = sid;
  d.innerHTML =
    `<div class="st-head">` +
    `<span class="st-icon">${done ? '✓' : ''}</span>` +  // 运行中留空：圆环由 CSS 画（style.css .st-icon），转字符会晃
    `<span class="st-title">${esc(title)}</span>` +
    `<span class="st-status">${done ? '已完成' : (dispatched ? '后台执行中' : '派发中…')}</span>` +
    `</div>` +
    (running ? `<pre class="st-act">正在准备…</pre>` : '') +
    (sid ? `<div class="st-foot"><button class="st-open">${running ? '查看会话' : '打开会话'}</button></div>` : '') +
    (done && !dispatched && out ? `<details class="st-detail"><summary>执行摘要</summary><pre>${esc(out)}</pre></details>` : '');
  const openBtn = d.querySelector('.st-open');
  if (openBtn) openBtn.onclick = () => openSession(sid);
  if (running) renderSubActs(sid);
  return d;
}

/* ---------- 长消息折叠（作用于**所有会话、所有视图**的用户位气泡：
   主对话、工作任务会话、右栏子对话抽屉里的用户气泡，以及 AI 发出的用户消息
   气泡（.msg.user.ai，偏白）——同一套 6 行规则、同一套交互。
   **系统通知气泡（.msg.sysnote）与数字助理的正文回复（.msg.assistant 的 .md）
   都不在折叠目标里**：通知内容直接全量显示，不裁剪、不出「展开 / 收起」控件
   （见 foldTargets）；模型回复多长都完整展开显示。 ----------
   需求：渲染高度超过 6 行就默认只显示前 6 行，底部渐隐 + 「展开 / 收起」按钮；
   短消息不折叠、不出按钮。判定必须基于**实际渲染出来的高度**——对话区宽度、
   字号一变，同一段文字换行出的行数就变，所以：
     - 阈值 = 运行时读到的行高（line-height）× 6，与元素内容高度（scrollHeight 去掉
       内边距）比较；裁剪用 max-height + overflow: hidden，scrollHeight 始终是完整
       内容高度，所以一个阈值同时管折叠态与展开态，不必先解开裁剪再量（判定不受
       自己上一次裁剪的影响，也就不会自激）；
     - 会话渲染完、对话区尺寸变化（拖宽/缩窗口）、字号变化时都重算（ResizeObserver +
       window resize，合到一帧）；所以不会出现「该折叠的没折叠」或「明明很短却折叠了」；
     - 用户手动展开过的（data-fold-open=1）重算后保持展开，只是按钮变成「收起」；
       展开意图还按「会话+seq」记入内存（foldOpenMem）——流式期间尾部每个消息
       事件都会重建、用户气泡随之换新元素，展开过的气泡靠记忆恢复展开，不再
       被新出的思考/工具行反复折回（刷新页面才回到默认折叠）。
   作用域（2026-10-14 放开）：主对话与右栏子对话两个视图都生效，不再看会话
   mode——按《消息类型规范.md》§7「用户与 AI 消息同一套 6 行折叠」，任务会话、
   子对话与主对话一致；仍不折叠的只有流式输出中的那半句与编辑态气泡
   （2026-10-20 起，发送中/插话占位同样折叠，见 foldTargets）。 */
const FOLD_LINES = 6;   // 折叠阈值（行）
const FOLD_SLACK = 2;   // 容差（px）：溢出不足 2px 不算，避免取整误差误判

let foldScheduled = false;
let foldTimer = 0;
let foldRetry = 0;      // 「当时还没排好版」的重试次数（最多 5 次）

// 展开记忆（跨尾部重建存活）：流式期间每个 message 事件都会全量重建尾部
//（renderMessages：最后一回合的用户气泡随之换新元素，挂在元素上的
// data-fold-open 一起丢，于是每出一行思考/工具就重折一次——不符合「展开了
// 就保持展开」的设计意图）。这里按「会话 id + seq」记住用户手动展开过的气泡，
// 重建后的新元素在装折叠按钮时查表恢复展开。只在内存里：刷新页面自然全部
// 回到默认折叠；用户再手动收起即删记录（回到默认行为）。插话/发送占位是
// 随重建重置的临时形态（无 seq），刻意不记。
const foldOpenMem = new Map();

// 过程行（proc-row：思考/工具/计划卡/材料行…）的展开记忆，与 foldOpenMem 同款：
// 纯内存 Map，不落 localStorage——刷新页面、手机端退出重进自然全部回到默认
// 收起；切换会话回来时由 renderMessages 的 jump 分支清掉（回来即「重新加载」
// 的默认折叠态）。解决的问题是：执行中尾部每个消息事件都整段重建最后一回合，
// 行上挂的展开态随 DOM 换掉——用户点开一段思考想细读，下一批工具行到达就被
// 折回收起。键 = 会话id + ':' + 行键（行键见 renderItem 的 data-rk）。
const procOpenMem = new Map();

function foldKeyOf(el, view) {
  const seq = el.parentElement && el.parentElement.dataset.seq;
  const sid = view && view.state && view.state.cur ? view.state.cur.id : '';
  return seq && sid ? sid + ':' + seq : null;
}

// 是否需要折叠：**所有会话、所有视图一律生效**（此前只做数字助理主对话
// mode=assistant，与《消息类型规范.md》§7「用户与 AI 消息同一套 6 行折叠」不符；
// 放开后工作任务会话、右栏子对话抽屉与主对话同一套规则，保留本函数作为唯一开关）
function foldEnabled() {
  return true;
}

// 折叠目标（按视图取：主对话列与右栏子对话列各算各的）：该视图里已定稿的
// 用户气泡与 AI 用户气泡（kind=aiuser，同样可能是长任务描述）。
// **数字助理的正文（.msg.assistant > .md）刻意不在目标里**：那是模型给出的
// 正常回复，多长都完整显示，不裁剪、不出折叠控件（用户气泡的折叠效果保持不变）。
// **系统通知气泡（.msg.sysnote > .bubble）同样不在目标里**（2026-10-21 移出）：
// 通知是给用户看的提示（中断/上传/交付/转达），内容直接全量显示——超 6 行的
// 长通知也不折，框内「展开 / 收起」按钮对一两行的通知完全多余。
// 跳过流式中的那块（边流边折叠会一直遮住最新输出）与编辑态气泡（正文在输入框
// 里改着）。发送中/插话中的占位气泡（pending-steer，虚线框临时形态）**同样
// 折叠**（2026-10-20）：附件注记块让占位气泡动辄百余字，不折就是一大坨；注意
// 尾部每次重渲染都会重建占位气泡，手动展开会随之重置回默认折叠——临时形态，
// 可接受。
function foldTargets(view) {
  view = view || mainView;
  const col = view.col;
  if (!col) return [];
  const live = view.liveId ? document.getElementById(view.liveId) : null;
  const out = [];
  col.querySelectorAll('.msg.user > .bubble').forEach((el) => {
    const msg = el.parentElement;
    if (!msg || msg.classList.contains('editing')) return;
    if (live && (msg === live || live.contains(msg))) return; // 流式实时块不折
    out.push(el);
  });
  return out;
}

// 溢出判定：与「当前是否折叠」无关——裁剪用的是 max-height + overflow: hidden，
// scrollHeight 始终是完整内容高度，所以一个阈值同时管折叠态与展开态，不需要先
// 解开裁剪再量（也就不会因为切换类名反过来触发 ResizeObserver）。阈值取
// 运行时读到行高 × 6 行，行高随字号设置变化，所以不写死像素值。
function foldMeasure(el) {
  const cs = getComputedStyle(el);
  const lh = parseFloat(cs.lineHeight);
  if (!lh) return null;
  const pad = parseFloat(cs.paddingTop || 0) + parseFloat(cs.paddingBottom || 0);
  const limit = lh * FOLD_LINES;
  return { overflow: el.scrollHeight - pad > limit + FOLD_SLACK, maxH: pad + limit };
}

// 参与气泡折叠的视图：主对话列 + 右栏子对话列（两套视图共用同一套渲染函数，
// 折叠规则也随之统一）
function foldViews() { return [mainView, subView]; }

// 重算并应用折叠状态（幂等：判定只看内容高度，不受自己上一次的裁剪影响，
// 所以结果稳定、不会自激）。逐视图各算一轮；任一视图有量不准的元素就统一重试。
function applyFolds() {
  const enabled = foldEnabled();
  // 折叠应用改变气泡高度（max-height 裁剪）：视口内/上方的变化会把钉住阅读的
  // 视口顶动（上翻历史时补测折叠导致的「往上跳」即此）。渲染标记 + 视口锚定
  // 成对——滚动补测路径（scroll 200ms 防抖后）天然在滚动停顿后触发，补偿安全。
  const pinned = foldViews().filter((v) => v._pinned);
  pinned.forEach((v) => { v._renderTs = Date.now(); anchorSave(v); });
  let unmeasurable = false;
  foldViews().forEach((view) => { if (applyFoldsIn(view, enabled)) unmeasurable = true; });
  pinned.forEach((v) => anchorRestore(v));
  // 有元素当时还没排好版（含右栏收起时不参与布局）：稍后再算一次（最多 5 次）
  if (unmeasurable && foldRetry < 5) { foldRetry++; setTimeout(scheduleFolds, 200); }
  else if (!unmeasurable) foldRetry = 0;
}

// 单个视图的一轮重算；返回「本轮有没有还没排好版、量不准的元素」。
// 增量策略（配合 .msg/.tf-sec 的 content-visibility:auto，见 style.css 与
// 《前端渲染性能分析-主对话卡顿.md》）：
// - 已测过的气泡带 data-fold-done 标记，本轮跳过——旧版每轮全列重测，流式
//   期间 MutationObserver 每帧触发一次，主对话几百个用户气泡逐个
//   getBoundingClientRect + scrollHeight，DOM 脏时首次读强制整页同步布局；
// - 离屏气泡不测也不标记：content-visibility 下离屏内容的 scrollHeight 是
//   占位估算值，量了会误判（过短漏折/过长误折）；滚入视口后由滚动监听的
//   防抖补测轮再量（上下各留 200px 提前量，快滚到的先量好）；
// - 全量重测只在「每行能放多少字变了」时发生：宽度变化（拖把手/窗口 resize）
//   与字号变化，由 resetFoldMarks 清标记触发。
function applyFoldsIn(view, enabled) {
  const col = view.col;
  if (!col) return false;
  const boxes = Array.from(col.querySelectorAll('.fold-box'));
  const seen = new Set();
  let unmeasurable = false; // 还没完成布局（高度 0）的元素：本轮跳过，稍后重算
  const vr = view.scroll ? view.scroll.getBoundingClientRect() : null; // 本视图的滚动视口
  foldTargets(view).forEach((el) => {
    seen.add(el);
    if (el.dataset.foldDone === '1') return; // 已测过：增量轮跳过
    const r = el.getBoundingClientRect();
    if (r.height === 0 || r.width === 0) {
      // 元素被隐藏/还没排好版时 scrollHeight 不可信（会误判成「不折叠」），
      // 所以这里不写结论，只标记需要重试。
      unmeasurable = true;
      return;
    }
    // 占位盒与滚动视口不相交 = 离屏：不测不标记，滚入视口后补测
    if (vr && (r.bottom < vr.top - 200 || r.top > vr.bottom + 200)) return;
    const m = enabled ? foldMeasure(el) : null;
    const btn = el.querySelector(':scope > .fold-btn');
    if (!m || !m.overflow) {
      // 不超过 6 行（或已切走会话）：彻底恢复原样，不留按钮与遮罩
      el.classList.remove('folded', 'fold-box');
      el.style.removeProperty('--fold-h');
      const fade = el.querySelector(':scope > .fold-fade');
      if (fade) fade.remove();
      if (btn) btn.remove();
      el.removeAttribute('data-fold-open');
    } else {
      if (!el.classList.contains('fold-box')) {
        el.classList.add('fold-box');
        const fade = document.createElement('div');
        fade.className = 'fold-fade';
        el.appendChild(fade);
      }
      el.style.setProperty('--fold-h', Math.ceil(m.maxH) + 'px');
      if (!btn) {
        const b = document.createElement('button');
        b.type = 'button';
        b.className = 'fold-btn';
        b.textContent = '展开';
        b.onclick = () => toggleFold(el, b);
        el.appendChild(b);
        // 尾部重建会换新元素（data-fold-open 已丢）：查展开记忆，本回合展开过
        // 就直接以展开态出现（流式中每出一行思考/工具都重建一次，不恢复的话
        // 用户刚展开的气泡立刻被折回去）
        const fk = foldKeyOf(el, view);
        const open = !!fk && foldOpenMem.get(fk) === '1';
        el.dataset.foldOpen = open ? '1' : '0';
        el.classList.toggle('folded', !open); // 无记忆时默认折叠：只显示前 6 行
        b.textContent = open ? '收起' : '展开';
        b.setAttribute('aria-expanded', open ? 'true' : 'false');
      } else {
        // 已经在折叠体系里：保留用户的展开意图，只同步按钮文案
        const open = el.dataset.foldOpen === '1';
        el.classList.toggle('folded', !open);
        btn.textContent = open ? '收起' : '展开';
      }
      const want = el.dataset.foldOpen === '1' ? 'true' : 'false';
      const b2 = el.querySelector(':scope > .fold-btn');
      if (b2 && b2.getAttribute('aria-expanded') !== want) b2.setAttribute('aria-expanded', want); // 同值不写，免得触发 MutationObserver 空转
    }
    el.dataset.foldDone = '1'; // 本轮已测：之后的增量轮直接跳过
  });
  // 已经不在目标集合里的（重渲染后残留、切走会话）：清掉折叠痕迹与测量标记
  boxes.forEach((el) => {
    if (seen.has(el)) return;
    el.classList.remove('folded', 'fold-box');
    el.style.removeProperty('--fold-h');
    el.removeAttribute('data-fold-open');
    el.removeAttribute('data-fold-done');
    el.querySelectorAll(':scope > .fold-fade, :scope > .fold-btn').forEach((n) => n.remove());
  });
  return unmeasurable;
}

// 展开/收起：折叠态只放开高度上限（按钮留在底部），展开态按钮落到正文下方
function toggleFold(el, btn) {
  const folded = el.classList.toggle('folded');
  el.dataset.foldOpen = folded ? '0' : '1';
  const fk = foldKeyOf(el, foldViewOf(el));
  if (fk) { folded ? foldOpenMem.delete(fk) : foldOpenMem.set(fk, '1'); } // 只记展开；手动收起=回到默认
  btn.textContent = folded ? '展开' : '收起';
  btn.setAttribute('aria-expanded', folded ? 'false' : 'true');
  if (folded) followScroll(foldViewOf(el), false); // 收起到 6 行后按所在视图重新贴底
  else pinViewForRead(el); // 展开阅读即钉住：与过程行/折叠段同一套展开交互
}

// 气泡属于哪个视图：右栏子对话列里的归 subView，其余归主对话列
function foldViewOf(el) {
  const sc = subView.col;
  return sc && sc.contains(el) ? subView : mainView;
}

// 一帧内合并多次请求（ResizeObserver 会在同一帧连发）
function scheduleFolds() {
  if (foldScheduled) return;
  foldScheduled = true;
  requestAnimationFrame(() => {
    foldScheduled = false;
    try { applyFolds(); } catch (e) { console.warn('[JX] fold', e); }
  });
}

// 尺寸/字号变化都要重算：观察对话列本身（宽度变了每行字数变，字号变了行高变，
// 两种都会让它的盒子尺寸变化）；消息 DOM 变化（流式重绘、尾部重建、后台刷新插入）
// 同样要重算——否则「渲染顺序」会让某条长消息错过那一次重算，出现「该折没折」。
// 另挂窗口 resize 兜底，字体就绪后再算一次（行高/字宽会变）。
// 增量时代的分工（applyFoldsIn 只测新增/视口内气泡）：
// - 宽度变化 → resetFoldMarks 全量重测（每行字数变，旧判定作废）；
// - 纯高度变化（流式内容增长）→ 只跑增量轮（新气泡），不清标记；
// - 滚动 → 防抖补测：滚入视口的未测气泡（离屏时被跳过的）在停顿后量上。
function setupFolds() {
  if (setupFolds.done) return;
  setupFolds.done = true;
  // 主对话列与右栏子对话列都观察：右栏收起/切到别的视图时它不参与布局（量不准的
  // 由 applyFolds 的重试兜底），重新展开时尺寸变化会再触发一轮重算
  foldViews().forEach((view) => {
    const col = view.col;
    if (!col) return;
    if (typeof ResizeObserver === 'function') {
      let lastW = 0; // 上次观测到的列宽：区分宽度变化（判定作废）与纯高度变化
      new ResizeObserver((ents) => {
        const w = ents && ents[0] ? ents[0].contentRect.width : 0;
        if (lastW && Math.abs(w - lastW) > 1) resetFoldMarks();
        else scheduleFolds();
        lastW = w;
      }).observe(col);
    }
    if (typeof MutationObserver === 'function') {
      // 只算状态是否变，不变就不写 DOM（见 applyFolds），所以不会自激空转。
      // 短路：流式 live 气泡的内容重写（.md 每帧 innerHTML）不涉及任何折叠目标
      //（foldTargets 明确排除 live），一批变更全部落在 live 内时直接不调度——
      // 旧版流式期间每帧都白跑一轮全列调度
      const liveId = view.liveId;
      new MutationObserver((muts) => {
        if (liveId && muts.length) {
          const le = document.getElementById(liveId);
          if (le && muts.every((m) => m.target === le || le.contains(m.target))) return;
        }
        scheduleFolds();
      }).observe(col, { childList: true, subtree: true });
    }
    if (view.scroll) {
      // 滚动补测：增量轮跳过离屏气泡，上翻/下翻经过的等滚动停顿后补上（200ms 防抖）
      let scTimer = 0;
      view.scroll.addEventListener('scroll', () => {
        clearTimeout(scTimer);
        scTimer = setTimeout(scheduleFolds, 200);
      }, { passive: true });
    }
  });
  try { if (document.fonts && document.fonts.ready) document.fonts.ready.then(() => scheduleFolds()); } catch (e) {}
  window.addEventListener('resize', () => {
    clearTimeout(foldTimer);
    foldTimer = setTimeout(resetFoldMarks, 120); // 拖窗口时别每像素都量一遍；宽度可能变了，清标记全量重测
  });
}

// 全量重测入口：清掉全部增量标记——宽度/字号变化会改变「每行能放多少字」，
// 已测过的折叠判定全部作废。离屏气泡清标记后不会立刻重测（applyFoldsIn 的
// 视口过滤会跳过它们），滚入视口时由滚动补测轮接管，正好只测看得见的。
// 字号变化的挂钩在 app-settings.js 的 applyChatFont。
function resetFoldMarks() {
  foldViews().forEach((view) => {
    if (!view.col) return;
    view.col.querySelectorAll('[data-fold-done]').forEach((el) => el.removeAttribute('data-fold-done'));
  });
  scheduleFolds();
}

/* ---------- 文件改动面板与交付成果卡片（文件快照子系统的前端半边） ---------- */

// 文件回退请求（「全部撤回」「撤回」「恢复到此版本前」共用）：成功刷新视图，
// 失败按服务端口径提示——409 blocked 时列出"快照后被外部改过"的文件清单。
// payload 走 handleFileRewind 的三种口径之一：{seqs} / {seqs,path} / {checkpointId}。
async function fileRewind(view, payload, btn, what) {
  if (view.state.streaming) {
    view.hint.textContent = '本轮执行中，等结束再撤回';
    setTimeout(() => { if (view.hint.textContent === '本轮执行中，等结束再撤回') view.hint.textContent = ''; }, 2500);
    return false;
  }
  btn.disabled = true;
  try {
    const d = await api('/api/sessions/' + view.state.cur.id + '/file-rewind', {
      method: 'POST', body: JSON.stringify(payload),
    });
    const msg = '已' + what + '：恢复 ' + (d.restored || 0) + ' 个文件、撤回 ' + (d.deleted || 0) + ' 个新建文件';
    view.hint.textContent = msg;
    setTimeout(() => { if (view.hint.textContent === msg) view.hint.textContent = ''; }, 5000);
    if (view === mainView) openSession(view.state.cur.id); else openSubSession(view.state.cur.id);
    return true;
  } catch (err) {
    btn.disabled = false;
    const list = (err.unsafe || []).map((u) => u.path + '：' + (u.reason || '被外部修改')).join('\n');
    if (err.blocked && list) {
      alert('以下文件在快照后被外部修改，无法安全回退：\n' + list);
    } else {
      alert('撤回失败：' + err.message);
    }
    return false;
  }
}

// 一个文件的历史版本列表容器（点「历史版本」就地展开/收起）。数据来自会话
// 快照（checkpoint）：每条 = 该文件的一次改动及其"改动前内容"，可查看内容
// 与当前文件的差异，或「恢复到此版本前」（只影响这一个文件）。
function fileVersionBox(view, path) {
  const box = document.createElement('div');
  box.className = 'fp-vers';
  const list = document.createElement('div');
  list.className = 'fv-list';
  box.appendChild(list);
  hideEl(box);
  box._loaded = false;
  box._load = async () => {
    list.innerHTML = '<div class="fv-tip">读取历史版本…</div>';
    try {
      const d = await api('/api/sessions/' + view.state.cur.id + '/file-versions?path=' + encodeURIComponent(path));
      renderVersionList(view, path, list, d);
    } catch (err) {
      list.innerHTML = '<div class="fv-tip bad">读取失败：' + esc(err.message) + '</div>';
    }
  };
  return box;
}

// 历史版本清单：最旧在前，末条标「当前」（该版本改动后的内容 == 文件现状）。
// 每项两个动作：查看（就地展开内容/差异）与恢复到此版本前（checkpointId 口径）。
function renderVersionList(view, path, list, d) {
  const vers = d.versions || [];
  list.innerHTML = '';
  const cap = document.createElement('div');
  cap.className = 'fv-tip';
  cap.textContent = vers.length
    ? '共 ' + vers.length + ' 个历史版本（最旧在前）' + (d.exists ? ' · 当前文件 ' + fmtBytes(d.size || 0) + '' : ' · 当前文件已不存在')
    : '没有历史版本（快照可能已按保留策略清理）';
  list.appendChild(cap);
  vers.forEach((v, i) => {
    const item = document.createElement('div');
    item.className = 'fv-item';
    const when = (v.createdAt || '').replace('T', ' ').slice(5, 16);
    const what = v.existedBefore ? '改动前 ' + fmtBytes(v.beforeSize || 0) : '此版本前文件不存在';
    const head = document.createElement('div');
    head.className = 'fv-head';
    head.innerHTML = '<span class="fv-idx">v' + (i + 1) + '</span>' +
      '<span class="fv-when">' + esc(when) + ' · ' + esc(v.toolName || '') + '</span>' +
      '<span class="fv-note">' + esc(what) + ' · 改动后 ' + fmtBytes(v.afterSize || 0) + '</span>' +
      (v.current ? '<span class="fv-cur">当前</span>' : '');
    const acts = document.createElement('span');
    acts.className = 'fv-acts';
    const showBtn = document.createElement('button');
    showBtn.type = 'button';
    showBtn.className = 'fv-btn fv-view';
    showBtn.textContent = '查看内容';
    const restBtn = document.createElement('button');
    restBtn.type = 'button';
    restBtn.className = 'fv-btn fv-restore';
    restBtn.textContent = '恢复到此版本前';
    const bodyEl = document.createElement('div');
    bodyEl.className = 'fv-body';
    hideEl(bodyEl);
    showBtn.onclick = async (e) => {
      e.stopPropagation();
      if (!bodyEl.hasAttribute('hidden')) { hideEl(bodyEl); return; }
      bodyEl.innerHTML = '<div class="fv-tip">读取中…</div>';
      showEl(bodyEl);
      try {
        const t = await api('/api/sessions/' + view.state.cur.id +
          '/file-version?path=' + encodeURIComponent(path) + '&id=' + encodeURIComponent(v.id));
        renderVersionBody(view, path, bodyEl, t);
      } catch (err) {
        bodyEl.innerHTML = '<div class="fv-tip bad">读取失败：' + esc(err.message) + '</div>';
      }
    };
    restBtn.onclick = async (e) => {
      e.stopPropagation();
      if (!(await confirmCenter('把这个文件恢复到 v' + (i + 1) + '（' + when + '）这次改动之前？\n' +
        (v.existedBefore ? '将把当前内容换成那次改动前的内容。' : '将删除这个文件（那次改动前它不存在）。') +
        '\n只影响这一个文件；对话记录不变（已按快照校验，外部改过的文件会被拒绝）。'))) return;
      await fileRewind(view, { checkpointId: v.id, path }, restBtn, '恢复 ' + path);
    };
    acts.appendChild(showBtn);
    // 「当前」版本没有可回退的目标；正文缺失（且改动前该文件存在）也无从恢复。
    // 「改动前不存在」的版本（新建）即使没有正文也有意义：恢复它 = 删掉该文件。
    const canRestore = !v.current && (v.existedBefore ? v.hasBody !== false : true);
    if (canRestore) acts.appendChild(restBtn);
    head.appendChild(acts);
    item.appendChild(head);
    item.appendChild(bodyEl);
    if (showBtn) {
      // 整行可点：点条目空白处等同「查看内容」，按钮各自独立生效
      item.onclick = () => showBtn.click();
    }
    list.appendChild(item);
  });
}

// 版本正文面板：文本给"与当前内容的差异 + 该版本全文"，二进制只给说明。
// 版本正文缺失（.jxai/snapshots 里的文件被清掉）时只展示元数据。
function renderVersionBody(view, path, box, t) {
  box.innerHTML = '';
  const note = document.createElement('div');
  note.className = 'fv-tip';
  if (!t.hasBody) {
    note.classList.add('bad');
    note.textContent = '这个版本的正文不在快照库里（已按保留策略清理），只能看到元数据。';
    box.appendChild(note);
    return;
  }
  if (t.existedBefore === false) {
    note.textContent = '这个版本之前该文件不存在（本次改动是新建）。';
    box.appendChild(note);
    return;
  }
  if (t.binary) {
    note.textContent = '这个版本是二进制内容（' + fmtBytes(t.size || 0) + '），不展示文本。';
    box.appendChild(note);
    return;
  }
  if (t.truncated) note.textContent = '内容过长，仅显示前 ' + fmtBytes(t.size || 0) + '。';
  else note.textContent = '改动前内容（' + fmtBytes(t.size || 0) + '）：';
  box.appendChild(note);
  const pre = document.createElement('pre');
  const cur = t.current || {};
  // 当前内容也是文本时给"行级差异 + 版本全文"，一眼看出版本与现状差在哪
  pre.textContent = (cur.exists && !cur.binary && typeof cur.text === 'string')
    ? diffBody(t.text, cur.text)
    : String(t.text ?? '');
  box.appendChild(pre);
}

/* ---------- 文件工具：本次改动 diff 查看器（点文件改动面板的文件行打开） ----------
   口径：一行的「本次改动」= 本批（这一回合）最早一条快照的"改动前内容" vs
   文件当前内容——正好是该行「撤回」会在两个状态之间恢复的那一对。数据零新增
   接口：复用 /file-version（一次同时返回版本正文与当前内容），diff 在前端算。
   视图：整文件展开（按新文件顺序），删除行红、新增行绿、未变行不着色；行号
   两列（旧/新），未变行两列都有号，删除行只有旧号、新增行只有新号。 */

// 行切分：CRLF/LF 统一归一（比较按行文本，行尾 \r 不参与，显示也干净）
function fdiffSplitLines(text) {
  return String(text == null ? '' : text).replace(/\r\n?/g, '\n').split('\n');
}

// 行级 diff：先修剪公共前后缀，剩余段做经典 LCS DP + 回溯（Int32Array 全表）。
// 剩余段超限（约 2000×2000）时整段降级为"全删 + 全加"——正文后端已按 256KB
// 截断，正常文件远达不到阈值；降级只损失中间段的对齐精度，界面永不卡死。
function fdiffAlign(a, b) {
  let pre = 0;
  while (pre < a.length && pre < b.length && a[pre] === b[pre]) pre++;
  let suf = 0;
  while (suf < a.length - pre && suf < b.length - pre &&
         a[a.length - 1 - suf] === b[b.length - 1 - suf]) suf++;
  const am = a.slice(pre, a.length - suf), bm = b.slice(pre, b.length - suf);
  const rows = [];
  for (let k = 0; k < pre; k++) rows.push({ t: 'ctx', ao: k, bo: k, text: a[k] });
  let degraded = false;
  if (am.length || bm.length) {
    if (am.length * bm.length > 4000000 || am.length + bm.length > 30000) {
      degraded = true;
      for (let i = 0; i < am.length; i++) rows.push({ t: 'del', ao: pre + i, bo: -1, text: am[i] });
      for (let j = 0; j < bm.length; j++) rows.push({ t: 'add', ao: -1, bo: pre + j, text: bm[j] });
    } else {
      const n = am.length, m = bm.length, w = m + 1;
      const dp = new Int32Array((n + 1) * w);
      for (let i = n - 1; i >= 0; i--) {
        const row = i * w, next = row + w;
        for (let j = m - 1; j >= 0; j--) {
          dp[row + j] = am[i] === bm[j] ? dp[next + j + 1] + 1
            : Math.max(dp[next + j], dp[row + j + 1]);
        }
      }
      let i = 0, j = 0;
      while (i < n && j < m) {
        if (am[i] === bm[j]) { rows.push({ t: 'ctx', ao: pre + i, bo: pre + j, text: am[i] }); i++; j++; }
        else if (dp[(i + 1) * w + j] >= dp[i * w + j + 1]) { rows.push({ t: 'del', ao: pre + i, bo: -1, text: am[i] }); i++; }
        else { rows.push({ t: 'add', ao: -1, bo: pre + j, text: bm[j] }); j++; }
      }
      while (i < n) { rows.push({ t: 'del', ao: pre + i, bo: -1, text: am[i] }); i++; }
      while (j < m) { rows.push({ t: 'add', ao: -1, bo: pre + j, text: bm[j] }); j++; }
    }
  }
  for (let k = 0; k < suf; k++) rows.push({ t: 'ctx', ao: a.length - suf + k, bo: b.length - suf + k, text: a[a.length - suf + k] });
  return { rows, degraded };
}

// 打开右栏「文件」视图看这个文件本次改动的 diff：cp = 本批该文件最早一条快照
// （fileChangesPanel 的行数据），它的"改动前正文"是本批改动的起点；当前内容
// 随同一响应返回（t.current）。右栏页签可反复点开，数据在页签打开时拉取。
let fvwSid = '', fvwPath = '', fvwCpId = 0, fvwWrap = false;

function openFileDiff(view, path, cp) {
  fvwSid = view.state.cur.id;
  fvwPath = path;
  fvwCpId = cp.id || 0;
  rpOpen('file');
  loadFileDiff();
}

// 空态：还没在看任何文件（页签手动点开 / 数据不属于当前会话）
function showFvEmpty(text) {
  const p = $('fvw-path');
  p.textContent = ''; p.title = '';
  $('fvw-stat').textContent = '';
  const note = $('fvw-note');
  note.textContent = '';
  note.classList.add('hidden');
  note.classList.remove('bad');
  const body = $('fvw-body');
  body.innerHTML = '';
  const tip = document.createElement('div');
  tip.className = 'fvw-empty';
  tip.textContent = text;
  body.appendChild(tip);
}

function loadFileDiff() {
  if (!fvwSid || !fvwPath || !fvwCpId) {
    showFvEmpty('还没有正在查看的文件。\n到对话里的「文件改动」面板点一个文件行，这里会打开它的本次改动（整文件红绿对比）。');
    return;
  }
  const p = $('fvw-path');
  p.textContent = fvwPath;
  p.title = fvwPath;
  $('fvw-stat').textContent = '读取中…';
  const note = $('fvw-note');
  note.textContent = '';
  note.classList.add('hidden');
  note.classList.remove('bad');
  $('fvw-body').innerHTML = '<div class="fvw-empty">正在读取快照与当前内容…</div>';
  api('/api/sessions/' + fvwSid + '/file-version?path=' + encodeURIComponent(fvwPath) +
    '&id=' + encodeURIComponent(fvwCpId))
    .then(renderFileDiff)
    .catch((err) => {
      $('fvw-stat').textContent = '';
      $('fvw-body').innerHTML = '';
      const tip = document.createElement('div');
      tip.className = 'fvw-empty';
      tip.textContent = '读取失败：' + err.message;
      $('fvw-body').appendChild(tip);
    });
}

// 渲染：二进制给说明（文件工具只支持文本类文件）；新建=整篇绿、当前已删=
// 整篇红；快照正文被清理时退化为只展示当前内容（不着色并说明）；正常路径 =
// fdiffAlign 对齐后逐行铺表格。
function renderFileDiff(t) {
  const body = $('fvw-body');
  const note = $('fvw-note');
  const notes = [];
  const say = () => {
    if (notes.length) { note.textContent = notes.join('；'); note.classList.remove('hidden'); }
    else { note.textContent = ''; note.classList.add('hidden'); }
  };
  const cur = t.current || {};
  if (t.binary || (cur.exists && cur.binary)) {
    $('fvw-stat').textContent = '二进制文件 · 不支持文本对比';
    note.textContent = '这个文件的内容是二进制（改动前 ' + (t.binary ? fmtBytes(t.size || 0) : '未知') +
      '，当前 ' + (cur.exists ? fmtBytes(cur.size || 0) : '已不存在') + '）。文件工具只支持文本类文件，请点顶栏「打开方式」用系统默认程序打开。';
    note.classList.add('bad');
    note.classList.remove('hidden');
    body.innerHTML = '';
    return;
  }
  if (t.truncated) notes.push('改动前内容超长，已按快照口径截断（前 256KB），对比基于截断内容');
  if (cur.truncated) notes.push('当前内容超长，已截断（前 256KB）');
  let oldText;
  if (t.existedBefore === false) { oldText = ''; notes.push('本次改动是新建文件：整篇按新增（绿）显示'); }
  else if (t.hasBody === false) { oldText = null; }
  else oldText = String(t.text ?? '');
  const newText = cur.exists ? String(cur.text ?? '') : '';
  if (!cur.exists) notes.push('当前文件已不存在（后来被删除）：整篇按删除（红）显示');
  if (oldText === null) {
    oldText = newText; // 基线正文已被清理：两边同文 → 整篇不着色，说明见 note
    notes.push('本批改动前的快照正文已被清理，无法对比；仅展示当前内容（不着色）');
  }
  const oldL = fdiffSplitLines(oldText), newL = fdiffSplitLines(newText);
  const al = fdiffAlign(oldL, newL);
  if (al.degraded) notes.push('差异段过大，未做行级对齐：中间整段按删除（红）/新增（绿）显示');
  let addN = 0, delN = 0;
  for (const r of al.rows) { if (r.t === 'add') addN++; else if (r.t === 'del') delN++; }
  const stat = $('fvw-stat');
  stat.innerHTML = '';
  const mk = (cls, txt) => { const sp = document.createElement('span'); if (cls) sp.className = cls; sp.textContent = txt; return sp; };
  stat.appendChild(mk('', '旧 ' + oldL.length + ' 行 → 新 ' + newL.length + ' 行 · '));
  if (addN) stat.appendChild(mk('gs', '+' + addN + ' 行'));
  if (delN) stat.appendChild(mk('rs', (addN ? ' · ' : '') + '-' + delN + ' 行'));
  const rows = al.rows;
  const DOM_CAP = 15000; // DOM 行数上限：256KB 截断后正常远达不到，兜底防卡顿
  const shown = rows.length > DOM_CAP ? rows.slice(0, DOM_CAP) : rows;
  if (rows.length > DOM_CAP) notes.push('全文共 ' + rows.length + ' 行，界面只渲染前 ' + DOM_CAP + ' 行');
  const tbl = document.createElement('table');
  tbl.className = 'fdiff-table';
  tbl.classList.toggle('wrap', fvwWrap);
  const tb = document.createElement('tbody');
  for (const r of shown) {
    const tr = document.createElement('tr');
    tr.className = r.t;
    const c1 = document.createElement('td'); c1.className = 'ln lno'; c1.textContent = r.ao >= 0 ? String(r.ao + 1) : '';
    const c2 = document.createElement('td'); c2.className = 'ln lnn'; c2.textContent = r.bo >= 0 ? String(r.bo + 1) : '';
    const c3 = document.createElement('td'); c3.className = 'sg'; c3.textContent = r.t === 'add' ? '+' : r.t === 'del' ? '-' : '';
    const c4 = document.createElement('td'); c4.className = 'cd'; c4.textContent = r.text === '' ? '\u00A0' : r.text;
    tr.appendChild(c1); tr.appendChild(c2); tr.appendChild(c3); tr.appendChild(c4);
    tb.appendChild(tr);
  }
  tbl.appendChild(tb);
  body.innerHTML = '';
  if (!rows.length) {
    body.innerHTML = '<div class="fvw-empty">（两边都是空文件，没有可显示的内容）</div>';
  } else {
    body.appendChild(tbl);
    const first = tbl.querySelector('.lno'); // 第二列行号 sticky 的偏移 = 首列实际宽
    if (first) tbl.style.setProperty('--lno-w', first.offsetWidth + 'px');
  }
  say();
}

// 顶栏：折行切换（长行横向滚动 ↔ 折行）、用系统默认应用打开当前文件
// （open-path 用户手势直通端点，与浏览器页签「打开方式」同一链路）与重新读取
$('fvw-wrap').onclick = () => {
  fvwWrap = !fvwWrap;
  $('fvw-wrap').classList.toggle('on', fvwWrap);
  $('fvw-wrap').title = fvwWrap ? '当前：长行折行显示（点击改为横向滚动）' : '当前：长行横向滚动（点击改为折行显示）';
  const tbl = $('fvw-body').querySelector('.fdiff-table');
  if (tbl) tbl.classList.toggle('wrap', fvwWrap);
};
$('fvw-sysopen').onclick = () => {
  if (!fvwSid || !fvwPath) return; // 空态（没在看任何文件）：无事可做
  api('/api/sessions/' + encodeURIComponent(fvwSid) + '/open-path', { method: 'POST', body: JSON.stringify({ path: fvwPath }) })
    .catch((e) => alert('打开失败：' + e.message));
};
$('fvw-reload').onclick = () => loadFileDiff();

// 右栏页签注册：与子对话/计划同一套机制。会话态记"这个会话正在看哪个文件的
// 哪次改动"，切走再切回（面板开着时 rpOpen 的 onOpen 会重拉）能回到原文件。
rpRegister('file', {
  title: '文件', order: 2.5,
  onOpen: () => { loadFileDiff(); },
  saveSession: () => ({ sid: fvwSid, path: fvwPath, cpId: fvwCpId, wrap: fvwWrap }),
  restoreSession: (d) => {
    fvwSid = (d && d.sid) || '';
    fvwPath = (d && d.path) || '';
    fvwCpId = (d && d.cpId) || 0;
    fvwWrap = !!(d && d.wrap);
    $('fvw-wrap').classList.toggle('on', fvwWrap);
    // 数据不在这里拉：面板开着时 rpOpen('file') 的 onOpen 会 loadFileDiff；
    // 收着时等用户点开页签再拉（旧对话的正文不会被画出来）
  },
});
// 文件改动面板：一回合内动过的文件（自动快照 + 手动 snapshot_file 都算），
// 折叠条显示「修改 N / 新增 M」（点折叠条展开/收起清单）。折叠行上的
// 「全部撤回」作用于本批全部文件；清单里每行文件：整行点击 = 打开文件工具
// 看本次改动（右栏「文件」视图整文件红绿对比，见上方 openFileDiff 一节），行内另有两个互相
// 独立的按钮：「撤回」（只回退这一行文件，仍限定在本批 seqs 内）与
// 「历史版本」（就地展开该文件此前的快照版本，可查看内容、恢复到某个版本之前）。
// 字号一律走 .msg-col 继承来的 --chat-fs（设置里的「字体大小」），本面板
// 不再写死字号：内部只用 em 相对值，跟随设置一起变化。
function fileChangesPanel(cps, seqs, view) {
  // 同文件多次改动只显示一行，类型按最早一条（改动前是否存在）
  const byPath = new Map();
  for (const c of cps) {
    if (!byPath.has(c.path)) byPath.set(c.path, c);
  }
  const modified = [], added = [];
  for (const c of byPath.values()) (c.existedBefore ? modified : added).push(c);
  const sec = document.createElement('div');
  sec.className = 'proc-row file-panel';
  const head = document.createElement('div');
  head.className = 'proc-head';
  const label = '文件改动 · 修改 ' + modified.length + ' · 新增 ' + added.length;
  head.innerHTML = chev('p-chev') + '<span class="p-name">' + label + '</span>' +
    '<span class="p-sub">· 可撤回</span>';
  // 整条折叠行上的总按钮：作用于本批全部文件（按轮撤回）
  const undo = document.createElement('button');
  undo.type = 'button';
  undo.className = 'fp-undo';
  undo.textContent = '全部撤回';
  undo.title = '把这批全部文件改动恢复到改动之前（对话记录不变）';
  undo.onclick = async (e) => {
    e.stopPropagation();
    const detail = modified.map((c) => '恢复 ' + c.path).concat(added.map((c) => '删除 ' + c.path)).join('\n');
    if (!(await confirmCenter('撤回本批全部文件改动？\n' + detail + '\n（已按快照校验，外部改过的文件会被拒绝）'))) return;
    await fileRewind(view, { seqs }, undo, '全部撤回');
  };
  head.appendChild(undo);
  const body = document.createElement('div');
  body.className = 'proc-body';
  const one = (tag, c) => {
    const row = document.createElement('div');
    row.className = 'fp-file';
    row.innerHTML = '<span class="fp-tag ' + (tag === '新增' ? 'add' : 'mod') + '">' + tag + '</span>' +
      '<span class="fp-path">' + esc(c.path) + '</span>' +
      '<span class="fp-size">' + fmtBytes(c.afterSize || 0) + '</span>';
    const acts = document.createElement('span');
    acts.className = 'fp-acts';
    const rev = document.createElement('button');
    rev.type = 'button';
    rev.className = 'fp-btn fp-revert';
    rev.textContent = '撤回';
    rev.title = '只把这个文件回退到本次改动之前（本批内的改动，对话记录不变）';
    const vers = document.createElement('button');
    vers.type = 'button';
    vers.className = 'fp-btn fp-vers-btn';
    vers.textContent = '历史版本';
    vers.title = '展开/收起这个文件的历史版本列表（可查看内容、恢复到某个版本之前）';
    acts.appendChild(rev);
    acts.appendChild(vers);
    row.appendChild(acts);
    const vbox = fileVersionBox(view, c.path);
    rev.onclick = async (e) => {
      e.stopPropagation();
      if (!(await confirmCenter('撤回 ' + c.path + ' 的改动？\n' +
        (c.existedBefore ? '将恢复成这次改动前的内容。' : '将删除这个文件（改动前它不存在）。') +
        '\n只影响这一个文件；对话记录不变（已按快照校验，外部改过的文件会被拒绝）。'))) return;
      await fileRewind(view, { seqs, path: c.path }, rev, '撤回 ' + c.path);
    };
    const toggleVers = async (e) => {
      if (e) e.stopPropagation();
      const open = vbox.hasAttribute('hidden');
      if (open) {
        showEl(vbox);
        vers.classList.add('on');
        if (!vbox._loaded) { vbox._loaded = true; await vbox._load(); }
      } else {
        hideEl(vbox);
        vers.classList.remove('on');
      }
    };
    vers.onclick = toggleVers; // 只有「历史版本」按钮就地展开/收起版本列表
    // 行内其余区域点击：打开文件工具，整文件红绿视图看本次改动（openFileDiff）
    row.onclick = () => openFileDiff(view, c.path, c);
    row.title = '在右侧栏查看本次改动：整文件红绿对比（绿=新增、红=删除）；「历史版本」按钮 = 展开这个文件的历史版本列表';
    const wrap = document.createElement('div');
    wrap.appendChild(row);
    wrap.appendChild(vbox);
    return wrap;
  };
  modified.forEach((c) => body.appendChild(one('修改', c)));
  added.forEach((c) => body.appendChild(one('新增', c)));
  sec.appendChild(head);
  sec.appendChild(body);
  bindProcToggle(sec);
  hideEl(body);
  return sec;
}

function fmtBytes(n) {
  if (n >= 1048576) return (n / 1048576).toFixed(1) + ' MB';
  if (n >= 1024) return (n / 1024).toFixed(1) + ' KB';
  return n + ' B';
}

// 交付卡片的两个动作（用户主动操作，不经模型权限门控）。「打开」按文件类型
// 分两路（右栏「文件」查看页已整体移除——那是原生浏览器面板修不好时期的
// 替代品，只支持文本且观感差，不再维护）：
//   * 网页（html/htm）→ 右栏「浏览器」（桌面=原生 WebView2 面板，网页=新标签）；
//   * 其余（代码/图片/PDF/Word/Excel/压缩包/exe 等）→ 系统默认程序，双击文件
//     是什么程序这里就是什么程序（远程会话没有本机路径，后端给直白报错）。
//     **md 也在这条老路上**：系统关联的 MD 查看器（md-viewer/MD查看器.exe）以
//     独立窗口打开——「打开」= 交系统程序，与「右侧查看」是两件事，别合并。
// 「在文件夹中查看」= 资源管理器里定位并选中该文件（远程不可用，给直白提示）。

// Markdown：只有**显式**的「右侧查看」/「预览（右栏）」才进右栏渲染页（后端
// mdview.go 复用 MD 查看器的渲染网页）。默认「打开」不认它——那是系统应用的事。
const MD_PANEL_EXT = new Set(['md', 'markdown', 'mdown', 'mkd']);
// 走右栏「浏览器」的后缀：真网页（原生面板/新标签里的完整浏览体验）。
// 别把 md 加进来：加进来「打开」就又变成进右栏了（md 的右栏入口在
// openInPanelBrowser 里显式写着，只服务「右侧查看」）。
const BROWSER_PANEL_EXT = new Set(['html', 'htm']);

function fileExtOf(p) {
  const m = String(p).toLowerCase().match(/\.([a-z0-9]+)$/);
  return m ? m[1] : '';
}

function isMarkdownPath(p) { return MD_PANEL_EXT.has(fileExtOf(p)); }

// 两路路由：'panel'（html 进右栏浏览器）/ 'shell'（系统默认应用，含 md）。
function deliveryOpenRoute(view, path) {
  return BROWSER_PANEL_EXT.has(fileExtOf(path)) ? 'panel' : 'shell';
}

/* Markdown 渲染页地址（后端 mdview.go）：/api/md/<会话id>/<工作区相对路径>。
   路径形态而非 ?sid=&path=（页在 …/docs/a.md，文里 ./img/x.png 由浏览器解析成
   …/docs/img/x.png）——md 里的相对图片/链接因此天然落在同一路由上，不裂图、
   不用改写正文。前导 / 去掉：与其它工作区端点同口径，相对路径基于会话工作区。 */
function mdPanelURL(sid, path) {
  const segs = String(path).replace(/\\/g, '/').replace(/^\/+/, '').split('/');
  return '/api/md/' + encodeURIComponent(sid) + '/' + segs.map(encodeURIComponent).join('/');
}

/* Markdown 在右栏渲染打开——只给**显式**的右栏入口（「右侧查看」/右键「预览
   （右栏）」）用，默认「打开」不经这里。桌面/Electron 走原生面板（与 html
   预览同一条 navigate 通道，面板里跑的就是那张渲染页）；手机与网页形态走右栏
   iframe（bpwShowFrame，见 app-right.js）——抽屉里铺满可用宽，窄屏单列可读。 */
function openMDInPanel(sid, path) {
  const url = mdPanelURL(sid, path);
  rpOpen('browser'); // 面板未开自动开；已开只是切页签
  if (bpwDesktop() || bpwElectron()) { bpwNav({ url: location.origin + url }); return; }
  bpwShowFrame(url, String(path).replace(/\\/g, '/').split('/').pop());
}

function deliveryOpenFile(view, sid, d) {
  if (deliveryOpenRoute(view, d.path) === 'panel') { openInPanelBrowser({ session: sid, path: d.path }); return Promise.resolve(); }
  return api('/api/sessions/' + encodeURIComponent(sid) + '/delivery/open', {
    method: 'POST', body: JSON.stringify({ path: d.path }),
  }).catch((e) => alert('打开失败：' + e.message));
}

// 远程访问环境（发起访问的设备不是跑助手的那台电脑）：交付卡的「打开」换成
// 「下载」——文件只在本机上，「打开」调起的是本机的系统程序，访问端什么都
// 看不见。判据按访问位置、不看设备形态：桌面壳/Electron 的页面就跑在本机程序
// 里（bindHost 配成局域网 IP 时壳里也不是 loopback，所以先判壳）；远程接入
// （JX_REMOTE）必是别的设备；浏览器只认 loopback 地址为本机——手机壳、手机
// 浏览器、局域网里别的电脑加载的都是电脑 IP。取舍：本机浏览器手动敲自己的
// 局域网 IP 也会被当远程（下载到本机下载目录，文件仍在这台机器上，误伤很小；
// 反过来远程点「打开」把本机程序调起来才是真问题）。
function isRemoteClientEnv() {
  if (bpwDesktop() || bpwElectron()) return false;
  if (window.JX_REMOTE) return true;
  const h = (location.hostname || '').toLowerCase();
  return h !== 'localhost' && h !== '127.0.0.1' && h !== '::1' && h !== '[::1]';
}

// 手机壳（jx-link）的下载通路：WebView 存不了 blob（详见 deliveryDownloadFile），
// 壳注入了 JXLink.beginDownload/appendDownload/finishDownload 桥，由壳落盘到它
// 自己的「下载管理」（首页第三个按钮）。没有桥 = 普通浏览器，返回 null 走原路。
// 桥可能挂在本窗口，也可能在父窗口——远程外壳若以 iframe 形态嵌在配对页里，
// 壳的桥注入在顶层文档上，只查 window 会误判成「不是壳」。
function jxShellBridge() {
  try {
    if (window.JXLink && typeof window.JXLink.beginDownload === 'function') return window.JXLink;
  } catch (e) {}
  try {
    let p = window.parent;
    for (let i = 0; i < 3 && p && p !== window; i++) {
      if (p.JXLink && typeof p.JXLink.beginDownload === 'function') return p.JXLink;
      p = p.parent;
    }
  } catch (e) {}
  return null;
}

function jxBase64Of(buf) {
  const bytes = new Uint8Array(buf);
  let s = '';
  const SLICE = 0x8000; // 逐段转，避免 apply 参数过多爆栈
  for (let i = 0; i < bytes.length; i += SLICE) {
    s += String.fromCharCode.apply(null, bytes.subarray(i, i + SLICE));
  }
  return btoa(s);
}

// 把一个网址的内容存进手机壳的「下载管理」并打开。流式：响应块攒到 ~512KB
// （base64 后约 680KB）再过一次 JS↔Java 桥，单次调用不卡主线程，也免了整文件
// blob 化的二次占内存。下载完成后若壳有 openDownload 桥（20261030-1 起）立即
// 调系统查看器打开——「下载并打开」是一条用户动作；旧壳没有该桥则只保存。
// 开始下载的提示由调用方（deliveryDownloadFile）出，不等下载完成。
window.jxShellSaveURL = async function (url, name) {
  const br = jxShellBridge();
  if (!br) throw new Error('不是手机壳环境');
  let id = '';
  try {
    const r = await fetch(url, { credentials: 'same-origin' });
    if (!r.ok) throw new Error('HTTP ' + r.status);
    id = br.beginDownload(name || '下载文件', r.headers.get('content-type') || '') || '';
    if (!id) throw new Error('手机壳无法创建下载');
    const CHUNK = 512 * 1024;
    const push = (u8) => { if (!br.appendDownload(id, jxBase64Of(u8))) throw new Error('写入手机失败'); };
    if (!r.body || typeof r.body.getReader !== 'function') {
      // 老引擎没有流：整块取再按块过桥（与历史行为等价）
      const u8 = new Uint8Array(await (await r.blob()).arrayBuffer());
      for (let off = 0; off < u8.length; off += CHUNK) push(u8.subarray(off, off + CHUNK));
    } else {
      // 流式：隧道块本身 16KB 级，攒够 CHUNK 合并一次过桥（少一个数量级的桥调用）
      let parts = [], accLen = 0;
      const flush = () => {
        if (!parts.length) return;
        const merged = new Uint8Array(accLen);
        let o = 0;
        for (const p of parts) { merged.set(p, o); o += p.length; }
        push(merged);
        parts = []; accLen = 0;
      };
      const reader = r.body.getReader();
      for (;;) {
        const x = await reader.read();
        if (x.done) break;
        parts.push(x.value); accLen += x.value.length;
        if (accLen >= CHUNK) flush();
      }
      flush();
    }
    if (!br.finishDownload(id)) throw new Error('保存失败');
    if (typeof br.openDownload === 'function') {
      try { br.openDownload(id); } catch (e) { /* 打开失败不影响已保存的文件 */ }
    }
  } catch (e) {
    if (id) { try { br.abortDownload(id); } catch (_) {} }
    throw e;
  }
};

// 远程端交付卡的「下载」：纯下载，打开/查看类需求由「右侧查看」承担（图片/
// 网页/md 它本来就管）。三条通路按「引擎够不够得着家端」分流：
//   · 手机壳（jx-link）：WebView 存不了 blob，走壳下载管理（jxShellSaveURL：
//     流式分块过桥），每次下载完成后自动调系统应用打开（openDownload 桥，
//     旧壳没有则只保存）——下载并打开是一条用户动作。开始下载就提示。
//   · 远程浏览器（JX_REMOTE，页面挂在网关源上、引擎够不到家端）：经隧道流式
//     取字节，收尾 blob 交浏览器原生下载气泡落盘（不弹保存框）。
//   · 局域网直开（同源）：a[href] attachment 原生下载（浏览器自带进度条）。
function deliveryDownloadFile(sid, d) {
  const url = '/api/sessions/' + encodeURIComponent(sid) + '/delivery/download?path=' + encodeURIComponent(d.path);
  const name = String(d.path).replace(/\\/g, '/').split('/').pop() || '交付文件';
  if (jxShellBridge()) {
    // 壳自己会在 beginDownload 时 Toast「正在下载到下载管理」，完成后自动打开
    window.jxShellSaveURL(url, name).catch((e) => alert('下载失败：' + ((e && e.message) || e)));
    return;
  }
  if (window.JX_REMOTE) {
    window.JX_REMOTE.downloadURL(url, name).catch(() => { /* 失败已由提示暴露 */ });
    return;
  }
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  document.body.appendChild(a);
  a.click();
  setTimeout(() => a.remove(), 1000);
}

function deliveryRevealFile(view, sid, d) {
  const cur = view === subView ? subState.cur : state.cur;
  if (cur && cur.remote) { alert('这是远程工作区里的文件，本机没有对应路径，无法在文件夹中定位。'); return Promise.resolve(); }
  return api('/api/sessions/' + encodeURIComponent(sid) + '/delivery/reveal', {
    method: 'POST', body: JSON.stringify({ path: d.path }),
  }).catch((e) => alert('打开所在文件夹失败：' + e.message));
}

/* 交付卡行里两件事由这里判定（都只看行自身实际宽，按钮宽现场实测，不写死像素、
   不看视口——手机窄屏、右栏挤窄的对话区、桌面宽屏同一套）：
   1. 折不折：文件名区与按钮区地位对等——文件名区的最小宽就取按钮区的实际宽，
      两块宽 + 中间间距塞不进行内可用宽，就把按钮挪到下一行（上一行 = 文件名 +
      灰色说明，下一行 = 按钮）。CSS 只描述两种形态，这里切 .dc-stacked。
   2. 灰字显不显：文件名优先完整，剩下有富余才显示灰色说明（一句「这文件是
      什么」的简介）；富余连两个字都摆不下就整块不显示，免得挤出半截灰字。
   行会随尾部重画反复重建，离了 DOM 立即取消观察。 */
const dcStackRO = typeof ResizeObserver === 'function' ? new ResizeObserver((entries) => {
  for (const en of entries) {
    const row = en.target;
    const acts = row.isConnected ? row.querySelector('.dc-acts') : null;
    if (!acts) { dcStackRO.unobserve(row); continue; }
    const rcs = getComputedStyle(row);
    const avail = row.clientWidth - (parseFloat(rcs.paddingLeft) || 0) - (parseFloat(rcs.paddingRight) || 0);
    const btnGap = parseFloat(getComputedStyle(acts).columnGap) || 0;
    let actsW = 0; // 按钮区实际宽 = 各按钮宽之和 + 按钮间距
    for (let i = 0; i < acts.children.length; i++) actsW += acts.children[i].offsetWidth + (i ? btnGap : 0);
    // 两块等宽：可用宽要装得下「文件名区（= 按钮区宽）+ 间距 + 按钮区」，否则折
    row.classList.toggle('dc-stacked', avail < actsW * 2 + (parseFloat(rcs.columnGap) || 0));
    const main = row.querySelector('.dc-main');
    const note = main && main.querySelector('.dc-note');
    if (note) {
      const name = main.querySelector('.dc-name');
      const gap = parseFloat(getComputedStyle(main).columnGap) || 0;
      const spare = main.clientWidth - name.scrollWidth - gap; // 文件名完整显示后剩下的宽
      const minShow = (parseFloat(getComputedStyle(note).fontSize) || 13) * 2; // 至少摆得下两个字
      note.classList.toggle('hidden', spare < minShow); // 项目口径的隐身：.hidden = display:none !important
    }
  }
}) : null;

// 交付成果卡片：deliver_result 登记的最终交付文件。本机每行三个动作（「打开」
// 「右侧查看」「在文件夹中打开」），整行都是点击热区——点行内任何位置（含空白）
// 都等同「打开」该文件，悬停有高亮反馈。远程端（手机壳/手机浏览器/别的电脑/
// 公网远程接入，见 isRemoteClientEnv）主按钮换成「下载」：文件落到访问的这台
// 设备上才谈得上用，「打开」只会把本机那头的系统程序调起来，访问端看不见
// （手机壳下载完成后会自动调系统应用打开——jx-link 20261030-1 起）。
function deliveryCard(dvs, sid, view) {
  view = view || mainView;
  const card = document.createElement('div');
  card.className = 'delivery-card';
  const head = document.createElement('div');
  head.className = 'dc-head';
  head.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 8v13H3V8"/><path d="M1 3h22v5H1z"/><path d="M10 12h4"/></svg>' +
    '<span class="dc-title">交付成果</span><span class="dc-count">' + dvs.length + ' 个文件</span>';
  card.appendChild(head);
  dvs.forEach((d) => {
    const row = document.createElement('div');
    row.className = 'dc-file';
    row.dataset.path = d.path; // 右键菜单取路径用（app-ctxmenu.js）；title 在按钮区定下来后再按端设置
    const main = document.createElement('span'); // 文件名 + 灰色说明同属一块（.dc-main 只缩不折）
    main.className = 'dc-main';
    const nm = document.createElement('span');
    nm.className = 'dc-name';
    nm.title = d.path;
    nm.textContent = d.path.split('/').pop();
    main.appendChild(nm);
    if (d.note) {
      const note = document.createElement('span');
      note.className = 'dc-note';
      note.textContent = d.note;
      main.appendChild(note);
    }
    row.appendChild(main);
    const acts = document.createElement('span');
    acts.className = 'dc-acts';
    // 一套代码按访问位置分流（isRemoteClientEnv）：本机主按钮「打开」（系统
    // 默认程序/右栏面板）；远程端换「下载」并把「在文件夹中打开」收起（资源
    // 管理器是本机的事）——下载落盘后手机壳会自动调系统应用打开（jx-link
    // 20261030-1 起），查看类需求由「右侧查看」承担（图片/网页/md 它本来就管）。
    const remote = isRemoteClientEnv();
    const route = deliveryOpenRoute(view, d.path);
    const ov = document.createElement('button');
    ov.type = 'button';
    ov.className = 'dc-btn';
    ov.textContent = remote ? '下载' : '打开';
    ov.title = remote ? '下载到这台设备（手机上下载完自动用系统应用打开）'
      : route === 'panel' ? '在右侧「浏览器」面板中打开这个网页文件'
      : '用系统默认程序打开这个文件';
    // 在右侧查看：显式入口（与右键菜单「预览（右栏）」同一逻辑）——图片、
    // 音视频、svg 这些浏览器能渲染的交付物，点它就在右栏看；md 也一样进右栏
    // 渲染页。默认「打开」只把 html 交右栏，其余（含 md）一律系统应用。
    const pv = document.createElement('button');
    pv.type = 'button';
    pv.className = 'dc-btn';
    pv.textContent = '右侧查看';
    pv.title = '在右侧「浏览器」面板中打开（网页/图片/音视频可直接渲染）';
    const rv = document.createElement('button');
    rv.type = 'button';
    rv.className = 'dc-btn';
    rv.textContent = '在文件夹中打开';
    rv.title = '在文件资源管理器中定位这个文件';
    ov.onclick = (ev) => {
      ev.stopPropagation();
      if (remote) deliveryDownloadFile(sid, d);
      else deliveryOpenFile(view, sid, d);
    };
    pv.onclick = (ev) => { ev.stopPropagation(); openInPanelBrowser({ session: sid, path: d.path }); };
    rv.onclick = (ev) => { ev.stopPropagation(); deliveryRevealFile(view, sid, d); };
    acts.appendChild(ov);
    acts.appendChild(pv);
    if (!remote) acts.appendChild(rv);
    row.appendChild(acts);
    // 整行可点击（含行内空白）：与主按钮等效（本机=打开，远程=下载）
    row.title = d.path + (remote ? '\n点击下载这个文件（手机上下载完自动打开）' : '\n点击打开这个文件');
    row.onclick = () => {
      if (remote) deliveryDownloadFile(sid, d);
      else deliveryOpenFile(view, sid, d);
    };
    if (dcStackRO) dcStackRO.observe(row); // 折不折成上下两行，按行自身实际宽判断
    card.appendChild(row);
  });
  return card;
}

/* ---------- 手机壳里的「打开页面」：一律开在壳内浏览器页 ----------
 * 壳子（jx-link）的 WebView 没有接管新窗口（没实现 onCreateWindow），网页这边
 * window.open(url,'_blank') 要么被悄悄丢掉（点了没反应），要么把整个应用 UI 顶掉：
 * 那页上没有 jxNavBack，返回键就再也退不出来，只能杀掉 App。所以只要有壳桥
 * （window.JXLink，壳注入），就交给壳另开一层——那一层用自己的返回键与右缘左滑
 * 关闭，关完就回到对话页，与网页这套返回层级互不干扰。
 * 没有桥的场合（局域网/公网里用手机浏览器、电脑浏览器打开的网页版）保持新标签页：
 * 那是浏览器的标签，浏览器自己的返回就在，不存在退不出去的问题。 */
function jxHasShellBrowser() {
  return !!(window.JXLink && typeof window.JXLink.openPage === 'function');
}

function jxOpenPage(u) {
  let url = String(u || '').trim();
  if (!url) return;
  if (!/^[A-Za-z][A-Za-z0-9+.-]*:/.test(url)) {
    try { url = new URL(url, location.href).href; } catch (e) { return; } // 相对地址补全
  }
  if (jxHasShellBrowser()) {
    try { window.JXLink.openPage(url); return; } catch (e) { /* 桥异常退回新标签 */ }
  }
  window.open(url, '_blank');
}

// 内容预览（工作区文件 / 导出页）：内容只在这一页里取得到（远程模式走隧道，桌面外壳
// 走同源接口），所以先取成 blob、转 base64 交给壳，由壳落成缓存文件再打开。
// 5MB 上限：base64 还要膨胀 1/3，再大的字符串过一次 JS 桥不划算，直说让用户去电脑上看。
const JX_SHELL_PREVIEW_MAX = 5 * 1024 * 1024;
function jxOpenContent(url, path) {
  const name = String(path || '').split(/[\\/]/).pop() || '预览';
  fetch(url).then((r) => {
    if (!r.ok) throw new Error('HTTP ' + r.status);
    return r.blob();
  }).then((b) => {
    if (b.size > JX_SHELL_PREVIEW_MAX) {
      throw new Error('文件太大（' + Math.round(b.size / 1048576) + 'MB），请在电脑端查看');
    }
    return new Promise((res, rej) => {
      const fr = new FileReader();
      fr.onload = () => res(String(fr.result || ''));
      fr.onerror = () => rej(new Error('内容读取失败'));
      fr.readAsDataURL(b);
    });
  }).then((d) => {
    const i = d.indexOf(',');
    const mime = (d.match(/^data:([^;,]*)/) || [])[1] || '';
    if (i < 0 || !window.JXLink.openFile(name, mime, d.slice(i + 1))) {
      throw new Error('壳没能打开这个文件');
    }
  }).catch((e) => alert('打不开：' + ((e && e.message) || e)));
}

/* 聊天链接 → 右栏浏览器打开（桌面=原生 WebView2 面板，人机共看）。
 * 用户主动操作，不经模型权限门控；后端 open-view 会把内嵌形态设为工具作用
 * 目标（用户开了「操作浏览器」能力后，模型的 browser_screenshot/browser_content
 * 看到的就是这页）并注入【网页查看】回报，模型由此知道用户在看什么。
 * 网页模式没有原生面板：手机壳里开在壳内浏览器页（见下面 jxOpenPage），
 * 纯浏览器里仍是新标签页。这是**显式**的右栏入口（「右侧查看」/「预览（右栏）」/
 * 聊天链接）；传 md 时进右栏渲染页（openMDInPanel），默认「打开」不走这里——
 * md 的「打开」是系统程序（MD 查看器独立窗口）。 */
async function openInPanelBrowser(body) {
  // 显式右栏入口传 md：进右栏渲染页（右栏看到的就是 MD 查看器那一页，见
  // openMDInPanel），不走下面的「原文流式预览」——那是 text/plain，看不了 md。
  if (body.path && body.session && isMarkdownPath(body.path)) { openMDInPanel(body.session, body.path); return; }
  const desktop = document.documentElement.classList.contains('desktop-shell');
  if (!desktop) {
    if (body.url) { jxOpenPage(body.url); return; }
    if (body.path && body.session) {
      const vurl = '/api/browser/view?sid=' + encodeURIComponent(body.session) +
        '&path=' + encodeURIComponent(body.path);
      if (jxHasShellBrowser()) { jxOpenContent(vurl, body.path); return; } // 手机壳：内容交壳内页面
      if (window.JX_REMOTE) { JX_REMOTE.openURL(vurl); return; } // 远程模式：隧道取 blob 再开
      window.open(vurl, '_blank');
      return;
    }
  }
  try {
    await api('/api/browser/open-view', { method: 'POST', body: JSON.stringify(body) });
  } catch (e) {
    if (body.url) jxOpenPage(body.url); // 打不开就退回「打开页面」的常规出口
    else alert(e.message); // 后端错误已带「打开失败: 」前缀，不重复包
    return;
  }
  rpOpen('browser'); // 面板未开自动开；已开只是切页签（不重播动画）
  setTimeout(() => { if (bpwDesktop() && bpwActive()) bpwReport(true, true); }, 350); // 开合动画落定后强制补报（自愈，不吃去重）
}

// 委托接管（主对话/子对话通用）：左键点 ext-link 在右栏打开；Ctrl/中键等放行默认
document.addEventListener('click', (e) => {
  const a = e.target.closest && e.target.closest('a.ext-link');
  if (!a || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey || e.button !== 0) return;
  e.preventDefault();
  openInPanelBrowser({ url: a.getAttribute('href') });
});

// 文件路径链接（file-link，正文/反引号里的路径渲染而来）：按消息所在视图
// 解析出会话——网页文件进右栏「浏览器」，其余（含 md：系统关联的 MD 查看器
// 独立窗口）一律系统默认程序打开
// （/api/sessions/{id}/open-path：用户手势直通，工作区内外都不校验、无
// 确认弹窗；只有「路径不存在」这类自然错误才弹带完整路径的提示）。
document.addEventListener('click', (e) => {
  const a = e.target.closest && e.target.closest('a.file-link');
  if (!a || e.ctrlKey || e.metaKey || e.shiftKey || e.altKey || e.button !== 0) return;
  e.preventDefault();
  const path = a.getAttribute('data-path');
  const view = a.closest('#sub-msg-col') ? subView : mainView;
  const sid = view.state.cur && view.state.cur.id;
  if (!sid || !path) return;
  if (deliveryOpenRoute(view, path) === 'panel') { openInPanelBrowser({ session: sid, path }); return; }
  openWorkspacePath(view, sid, path);
});

/* 渲染契约（性能与滚动）：
   - 前缀复用：最后一个真实用户消息之前的所有回合（含继承段、文件改动面板、
     交付卡）只在结构变化时整体重建；流式期间的 message 事件只重建尾部
     （最后一回合 + 待生效插话），前缀的折叠状态与阅读位置不被整列重渲冲掉。
   - 跟随滚动：默认贴底跟随输出；用户上翻即「钉住」阅读位置，流式增量与
     消息事件都不再拽回，滚回底部或点「回到底部」才恢复跟随；jump=true
     （打开会话/首次渲染）强制到底。 */

// 尾部 = 从最后一个真实用户消息起的回合（AI 发出的用户消息 kind=aiuser 也是
// 回合起点——派发回报开启的回合同样要收口重建）；没有则返回 -1（全算尾部）
function lastTurnStart(msgs) {
  for (let i = msgs.length - 1; i >= 0; i--) {
    const m = msgs[i];
    if (m.role === 'user' && (m.kind === 'aiuser' || !isSysNote(String(m.content ?? '')))) return i;
  }
  return -1;
}

// 跟随滚动（钉住式）：默认贴底跟随输出；用户上翻（滚轮/触摸/拖滚动条，经
// setupScrollTuck 钉住）后 view._pinned=true，之后的跟随一律让路，直到用户
// 滚回底部（或点「回到底部」）恢复。jump=true 强制到底并解除钉住。
// 旧版只按「距底 <120px 才跟随」判断：流式输出长得快，滚轮一格约 100px，
// 一步逃不出 120px 窗口就被拽回——表现就是「往上滑不动、总被拉到最底下」。
const FOLLOW_PIN_DIST = 120; // 滚动位置离底超过此距 → 兜底钉住（拖滚动条/键盘翻页）
const FOLLOW_RESUME = 48;    // 滚回离底小于此距 → 恢复跟随
const PIN_GUARD_MS = 350;    // 手势钉住后的短窗口：其间的小距滚动不算「滚回底部」

function followScroll(view, jump) {
  const el = view.scroll;
  if (jump) { view._pinned = false; el.scrollTop = el.scrollHeight; return; }
  if (view._pinned) return; // 钉住阅读位置：不跟随
  el.scrollTop = el.scrollHeight;
}

/* 视口锚定（钉住阅读时视口零跳动）：上翻阅读期间尾部重建、折叠应用、滚动
   补测都会改变内容高度，浏览器重排后视口里正显示的内容会被顶得上下窜——
   表现就是「下面一有更新就跳、上面逐渐加载出来又往上跳」（远程/手机最明显：
   事件成簇到达、单帧变化量大）。锚定与浏览器原生 scroll anchoring 同一原理：
   变更前记住视口**中线**上的回合级元素与其视口坐标，变更后按位置差补回
   scrollTop——屏幕中间正在看的内容纹丝不动，上下怎么长都不跳。
   只在 view._pinned 时生效：贴底跟随（未钉住）由 followScroll 直接滚到底，
   天然稳定，不经这里。 */
function anchorSave(view) {
  view._anchor = null;
  if (!view._pinned) return; // 贴底跟随态不需要锚定
  const col = view.col, sc = view.scroll;
  if (!col || !sc) return;
  const vr = sc.getBoundingClientRect();
  // 中线命中的最深元素提升到消息列的直接子元素（回合级块）：elementFromPoint
  // 一次定位，不逐元素量 rect。命不中消息列（列矮于视口/中线落在输入区）则不锚
  let el = document.elementFromPoint(vr.left + vr.width / 2, vr.top + vr.height / 2);
  if (!el || el === col || !col.contains(el)) return;
  while (el && el.parentElement !== col) el = el.parentElement;
  if (!el || el.parentElement !== col) return;
  const live = view.liveId ? document.getElementById(view.liveId) : null;
  if (live && (el === live || el.contains(live))) return; // 流式气泡高度常变，不做锚
  view._anchor = {
    el, top: el.getBoundingClientRect().top,
    // 替身查找依据：折叠段带 data-key（回合键），消息行带 data-seq
    key: (el.dataset && el.dataset.key) || '',
    seq: (el.dataset && el.dataset.seq) || '',
  };
}

// DOM 变更后按锚点补差。锚元素被尾部重建换掉时按 key/seq 找重建后的替身
//（同一消息渲染出的新行，位置等价）；替身也没有则放弃本次补偿（保守，不猜）。
// 用户正在触摸/滚轮操作（含手机惯性滚动的收尾期）时不写 scrollTop——程序
// 设置会掐断惯性滚动，那 250ms 的突变留给浏览器原生 anchoring 兜底。
function anchorRestore(view) {
  const a = view._anchor;
  view._anchor = null;
  if (!a) return;
  if (Date.now() - (view._actTs || 0) < 250) return;
  let el = a.el;
  if (!el.isConnected) {
    if (a.key) el = view.col.querySelector('[data-key="' + a.key + '"]');
    else if (a.seq) el = view.col.querySelector('[data-seq="' + a.seq + '"]');
    if (!el) return;
  }
  const dy = el.getBoundingClientRect().top - a.top;
  if (dy) view.scroll.scrollTop += dy;
}

// 「发送中」占位气泡：点发送到服务端 message 事件回显之间（本机后端极快，
// 但经网关/手机网络远程访问时可达数秒）用户消息处于「输入框已清、对话区没影」
// 的空窗，看起来像没发出去。先按插话同款虚线气泡立显，回显落地即被正常气泡
// 替换；整轮失败则连内容一起退回输入框（见 send 的 catch）。
function pendingSendBubble(content) {
  const wrap = document.createElement('div');
  wrap.className = 'msg user pending-steer';
  const b = document.createElement('div');
  b.className = 'bubble';
  const badge = document.createElement('span');
  badge.className = 'q-badge q-badge-steer';
  badge.textContent = '发送中';
  b.appendChild(badge);
  appendQuotedBody(b, content); // 引用段同样拆成灰色引用行（与正式气泡一致）
  wrap.appendChild(b);
  return wrap;
}

// 当前视图是否有待回显的发送占位：带会话 id 自校验（切走后旧条自失效），
// 且服务端历史末尾已是同文用户消息时不再画（attached 模式整体重取后防重）
function pendingSendOf(st, msgs, sid) {
  const ps = st.pendingSend;
  if (!ps || ps.sid !== sid) return null;
  const lastM = msgs[msgs.length - 1];
  if (lastM && lastM.role === 'user' && String(lastM.content ?? '') === ps.content) return null;
  return ps;
}

// 渲染单个回合（用户气泡 + 过程折叠 + 完成态收尾件）；isLast 决定是否走流式尾部窗口。
// 工具定义排在系统提示词上方（模型侧 KV 前缀以工具定义打头，展示对齐真实顺序）
function appendTurn(target, turn, key, view, isLast) {
  if (turn.headTools) target.appendChild(toolsRow(turn.headTools, turn.headToolsSeq));
  if (turn.sysHead) target.appendChild(sysHeadRow(turn.sysHead, turn.sysHeadSeq));
  if (turn.userMsg) target.appendChild(userBubble(turn.userMsg, view));
  const streaming = isLast && view.state.streaming;
  renderTurnProcess(target, turn, key, { streaming: streaming, isLast: !!isLast });
  if (streaming) return; // 执行中不画收尾件（文件改动/交付卡片），随完成态一起出现
  const seqSet = new Set();
  turn.items.forEach((it) => { if (it.seq != null) seqSet.add(it.seq); });
  if (!seqSet.size) return;
  const sid = (view.state.cur && view.state.cur.id) || '';
  const cps = (view.state.checkpoints || []).filter((c) => seqSet.has(c.seq));
  if (cps.length) target.appendChild(fileChangesPanel(cps, [...seqSet], view));
  const dvs = (view.state.deliveries || []).filter((d) => seqSet.has(d.seq));
  if (dvs.length) target.appendChild(deliveryCard(dvs, sid, view));
}

function renderMessages(msgs, view, opts) {
  view = view || mainView;
  const st = view.state;
  const box = view.col;
  // 会话正在乐观加载：转圈占位不被空列表渲染冲掉（事件触发的就地翻新拿到的
  // 还是清空后的 msgs）；真实数据落地时加载方先清 _loading 再来渲染
  if (st._loading && !msgs.length) return;
  if (!msgs.length) {
    // 无会话（首页）时空列表是常态：问候区由 resetEmpty 铺好且里面挂着输入卡
    // （#home-composer），这里绝不能拿 emptyHTML 顶掉它——销毁问候区等于销毁 #input
    if (!st.cur) return;
    st._pfxSid = null;
    renderLoadOlderBar(view); // 空态没有「更早」可言：顶条收起
    box.innerHTML = view.emptyHTML;
    // 首条消息已发出、还没回显：空态（首页问候）下面也立显「发送中」占位
    const ps0 = pendingSendOf(st, msgs, (st.cur && st.cur.id) || '');
    if (ps0) box.appendChild(pendingSendBubble(ps0.content));
    view._pinned = false; // 空会话没有可钉住的阅读位置
    view.scroll.scrollTop = view.scroll.scrollHeight;
    return;
  }
  const jump = !!(opts && opts.jump);
  reportedSubs = scanReported(msgs); // 子任务卡片的完成态依据（渲染期间同步使用）
  const resultOf = {};
  for (const m of msgs) {
    if (m.role === 'tool' && m.tool_call_id) resultOf[m.tool_call_id] = m;
  }
  const sid = (st.cur && st.cur.id) || '';
  // 切换会话的全新加载（jump）：本会话的展开记忆一并下线——回来是默认折叠的
  // 「重新加载」状态（刷新页面/手机重进时内存表本就为空，同一约定）。停留本页
  // 时的就地翻新（无 jump）不清：执行中展开的思考/工具行靠记忆跨尾部重建保持。
  // _pinned 在这里提前重置：否则锚定会把上一个会话的视口锚带进新渲染。
  if (jump) {
    view._pinned = false;
    const pfx = sid + ':';
    Object.keys(turnOpen).forEach((k) => { if (k.lastIndexOf(pfx, 0) === 0) delete turnOpen[k]; });
    Array.from(foldOpenMem.keys()).forEach((k) => { if (k.lastIndexOf(pfx, 0) === 0) foldOpenMem.delete(k); });
    Array.from(procOpenMem.keys()).forEach((k) => { if (k.lastIndexOf(pfx, 0) === 0) procOpenMem.delete(k); });
    st._batches = []; // 全新加载：分批窗口重置（loadSessionInto 已设好新 older）
    cancelReleaseOlder(view);
  }
  anchorSave(view); // 任何重建（前缀全量重建/尾部重建）之前锚定视口（仅钉住阅读时生效）
  // 子对话开头的继承段（后端克隆父对话时写入的标记行 + N 条历史）：
  // 折叠成一条「来自主对话」折叠条，其余消息照常渲染。继承段先分组，
  // 正文段从它分组后的基线接着比——子对话自己的 prompt 快照与父对话相同
  // 时不再重复显示「工具定义/系统提示词」（真变了才出变更行）。
  let inherited = null, rest = msgs, off = 0, inhTurns = null;
  if (msgs[0].kind === 'inherit') {
    let n = 0;
    try { n = JSON.parse(String(msgs[0].content ?? '{}')).count || 0; } catch (e) { /* 坏行就当没有继承段 */ }
    inherited = msgs.slice(1, 1 + n);
    rest = msgs.slice(1 + n);
    off = 1 + n;
    inhTurns = groupTurns(inherited, resultOf);
  }
  const kRel = lastTurnStart(rest);
  const k = kRel < 0 ? off : off + kRel; // 前缀长度：无真实用户消息时继承段之后全算尾部
  const turns = groupTurns(rest, resultOf, inhTurns && inhTurns.base);
  // 前缀可复用：同会话、分界没动、分界前最后一条 seq 未变、前缀 DOM 还在
  const reusable = st._pfxSid === sid && st._pfxLen === k && k > 0
    && msgs[k - 1] && msgs[k - 1].seq === st._pfxSeq
    && st._pfxNodes > 0 && box.children.length >= st._pfxNodes;
  if (!reusable) {
    box.innerHTML = '';
    if (inhTurns) box.appendChild(inheritBlock(inhTurns, inherited, sid));
    turns.forEach((turn, ti) => {
      if (ti === turns.length - 1) return; // 最后一回合属尾部
      appendTurn(box, turn, sid + ':' + (turn.userMsg ? turn.userMsg.seq : 'head' + ti), view, false);
    });
    st._pfxSid = sid;
    st._pfxLen = k;
    st._pfxSeq = msgs[k - 1] ? msgs[k - 1].seq : 0;
    st._pfxNodes = box.children.length;
    st._batches = []; // 前缀重建（编辑重发/切回）：批记账对应的旧节点已不存在
  }
  // 尾部每次全量重建：最后一回合（流式中走尾部窗口）+ 待生效插话。
  // 渲染前记时间戳：移除尾部的瞬间内容变矮，浏览器会把 scrollTop 钳到临时
  // 底部并发出 dist≈0 的 scroll 事件——若当作「用户滚回底部」会误解除钉住、
  // 随后跟随把阅读位置拽到底（单格上翻场景实测翻车），scroll 监听据此忽略
  view._renderTs = Date.now();
  while (box.children.length > st._pfxNodes) box.removeChild(box.lastChild);
  const tail = document.createDocumentFragment();
  // 继承段有了、还没有任何自有消息：空态引导文案画在**尾部**（不是前缀）——
  // 首条消息到达时随尾部一起消失。画进前缀的话，前缀复用判定（分界与末条
  // seq 都没变）会让它残留，消息从空态标题下面长出来、滚动时才被「顶」出
  // 视口（曾经的「用户消息一点点往上顶、盖过空态」就是这么来的）。
  if (inherited && !rest.length) {
    const empty = document.createElement('div');
    empty.innerHTML = view.emptyHTML;
    tail.appendChild(empty);
  }
  const last = turns[turns.length - 1];
  if (last) {
    appendTurn(tail, last, sid + ':' + (last.userMsg ? last.userMsg.seq : 'head' + (turns.length - 1)), view, true);
  }
  // 已插话、等下一次调用模型生效的消息：直接落在对话最底下（与已发送的用户消息
  // 同一套气泡外观，只多一个「插话中」角标），落库后由 message 事件替换成正常气泡。
  // 数字助理派发注入的插话（kind=aiuser）按「AI 用户」气泡渲染（右对齐偏白，
  // 落库后形态一致）；report 类系统通知仍走 sysnote 那套左对齐通知。
  (st.pendingSteer || []).forEach((pm) => {
    if (pm.kind === 'report') {
      const nw = document.createElement('div');
      nw.className = 'msg sysnote';
      nw.dataset.pendingSteer = pm.id;
      const nb = document.createElement('div');
      nb.className = 'bubble';
      nb.textContent = pm.content;
      nw.appendChild(nb);
      tail.appendChild(nw);
      return;
    }
    const wrap = document.createElement('div');
    wrap.className = 'msg user' + (pm.kind === 'aiuser' ? ' ai' : '') + ' pending-steer';
    wrap.dataset.pendingSteer = pm.id;
    const b = document.createElement('div');
    b.className = 'bubble' + (pm.kind === 'aiuser' ? ' md' : '');
    const badge = document.createElement('span');
    badge.className = 'q-badge q-badge-steer';
    badge.textContent = '插话中';
    b.appendChild(badge);
    appendQuotedBody(b, pm.content, pm.kind === 'aiuser'); // 附件块/引用块与正式气泡同款拆行渲染（含折叠，长注记不再是一大坨原文）；aiuser 正文同样 MD 渲染，落库前后形态一致
    wrap.appendChild(b);
    // 悬停操作行：与正式用户气泡同一套样式，只有「编辑」（退回输入框改完重发）。
    // 只给自己发的插话（kind 空/chat）；派发注入的其他形态不参与。
    if (!pm.kind || pm.kind === 'chat') {
      const acts = document.createElement('div');
      acts.className = 'msg-actions user-acts';
      const ed = document.createElement('button');
      ed.className = 'act-copy act-edit';
      ed.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/></svg>编辑';
      ed.title = '退回输入框编辑后重新发送';
      ed.onclick = () => editPendingMsg(view, pm);
      acts.appendChild(ed);
      wrap.insertBefore(acts, b); // 与正式用户气泡一致：操作行在气泡上方
    }
    tail.appendChild(wrap);
  });
  // 刚发出、等回显的「发送中」占位（最末位，晚于插话）
  const ps = pendingSendOf(st, msgs, sid);
  if (ps) tail.appendChild(pendingSendBubble(ps.content));
  // live 气泡（正在流式输出的那团）尾部重建时被删掉又还没被 liveRender 重建：
  // 已在 DOM 里的话挪进尾部、钉在插话上方——插话/发送占位始终是最底下等待者
  const live = document.getElementById(view.liveId);
  if (live && st.streaming) {
    const anchor = tail.querySelector('[data-pending-steer], .msg.pending-send');
    if (anchor) tail.insertBefore(live, anchor); else tail.appendChild(live);
  }
  box.appendChild(tail);
  followScroll(view, jump);
  anchorRestore(view); // 钉住阅读时按锚点补差（followScroll 在钉住态不写 scrollTop，不会互相覆盖）
  scheduleFolds(); // 气泡折叠：渲染完按实际高度重算（主对话与右栏子对话列都算）
  buildSeqIndex(view); // seq→索引：滚动预取判定用（msgs 变化的主路径都走这里刷新）
}

/* ---------- 回合过程区：一条渲染路径，两套可见策略 ---------- */

// 过程区（用户气泡与收尾件之间的思考/工具/子任务卡/上下文材料/系统通知）只有这
// 一条渲染路径：常显项就地渲染，其余相邻项攒成折叠段（foldSection）。**折叠机制
// 只有一份**——折叠条构造、标签（foldLabel）、展开记忆（turnOpen）、步数去重集
// （seenSteps）都在这条路径上，执行中与完成态共用，所以「折起来的样子」永远一致；
// 两态真正的差别只在**哪些条目常显**，由下面两套策略给出：
//   · 执行中 streamKeep：尾部窗口——最近 TAIL_ROWS 个过程条目常显（看得到最新
//     动作），上方折成折叠条；系统通知/运行中子任务卡/计划卡/长正文/已折叠标注行
//     钉住常显。
//   · 完成态 finishedKeep：按内容角色钉住——收尾答案、长正文、计划卡、已折叠
//     标注行、孤立的系统通知与末回合尾部通知。
// 为什么不能并成一套：执行中按内容角色折会把最新动作折没；完成后还留尾部 10 行
// 窗口，打开历史会话时中段会露出零散过程行。上下文材料行不参与常显，位置就是它
// 在消息序列里的真实位置——两态一致，不会「流式时被搬到顶上、完成后又跑回中间」。
function renderTurnProcess(box, turn, key, opts = {}) {
  const items = turn.items;
  const streaming = opts.streaming === true;
  const plan = streaming
    ? { set: streamKeep(items), answerIdx: -1 }
    : finishedKeep(items, opts.isLast === true);
  // sid 从回合键（会话id:seq）取出，透传给行渲染：行级展开记忆（procOpenMem）
  // 按会话存取，主/子视图同屏时不串台
  const sid = key ? key.split(':')[0] : '';
  // 步数去重集按**回合**共享：同一助手行的条目可能被锚点切到前后两段，
  // 各段各建一个 Set 会把一次调用数两遍（见 secCount）
  const seenSteps = new Set();
  let seg = [];
  const flush = () => {
    if (!seg.length) return;
    box.appendChild(foldSection(seg, key, { open: turnOpen[key] === true, seen: seenSteps, sid: sid }));
    seg = [];
  };
  // 常显项就地渲染（必要时把折叠段切开），其余条目折进段里
  items.forEach((it, i) => {
    if (plan.set.has(i)) { flush(); box.appendChild(renderItem(it, { final: i === plan.answerIdx, sid: sid })); }
    else seg.push(it);
  });
  flush();
}

// 执行中的可见策略（尾部窗口）：材料行不占窗口、也不顶掉最新动作
function streamKeep(items) {
  const keep = new Set();
  const flowIdx = [];
  items.forEach((it, i) => { if (!isMatItem(it)) flowIdx.push(i); });
  const foldFrom = Math.max(0, flowIdx.length - TAIL_ROWS);
  flowIdx.forEach((idx, j) => {
    if (j >= foldFrom || streamPinned(items[idx])) keep.add(idx);
  });
  return keep;
}

// 执行中钉住常显的几类：系统通知（report 通道的虚线通知块——执行期间的中断/上传
// 提示不能被折没）、运行中的子任务卡（实时进度）、计划卡、中间长正文（与完成态
// 同一豁免：「先解释一大段、再连续十几次工具调用」的讲解段不能执行中途被折没）、
// 以及「已折叠」标注行（它标记的就是折叠这件事本身，折进段里等于没标）。
// 派发类消息（aiuser）是回合的 userMsg、永远在过程段之外，天然常显，不劳这里钉。
function streamPinned(it) {
  return it.k === 'note' || it.k === 'plancard' || it.k === 'fold' || it.k === 'summary' ||
    (it.k === 'subtask' && !it.result) || (it.k === 'text' && isLongText(it.text));
}

// 完成态的可见策略（锚点）：回合内连续的可折叠项（思考/工具/子任务卡/上下文材料/
// 系统通知）不分家，相邻就折进同一段；段内还有各自的行级小折叠（proc-row 点开看
// 原文、工具行点开看参数）。锚点四类：回合收尾答案（最后一条正文且其行无伴随工具
// 调用，见下）、长正文（isLongText，plancard 计划卡同理）、**孤立**的系统通知
// （前后都没有可折叠邻居的单条），以及**尾部通知**——最后一个回合里、下方再没有
// 任何消息（正文/计划卡）的通知保持常显：系统中断这类提示往往就是会话的最后一条，
// 重启后用户打开会话要靠它知道「发个继续」。
function finishedKeep(items, isLast) {
  // 回合的收尾答案 = 最后一条正文，且它自己所在的助手行**没有伴随工具调用**
  // （「正文+工具调用」是过程交代，不是收尾）。被打断的回合（系统中断/执行中断）
  // 停在半路，最后一条正文往往来自这种行——不能按最终输出对待：不挂操作按钮、
  // 不常显，按过程正文常规折叠（否则出现「短句带一排按钮还不折叠」的怪相）。
  let answerIdx = -1;
  for (let i = items.length - 1; i >= 0; i--) {
    if (items[i].k !== 'text') continue;
    const calls = items[i].m && items[i].m.tool_calls;
    answerIdx = (!calls || !calls.length) ? i : -1;
    break;
  }
  const anchor = new Set();
  if (answerIdx >= 0) anchor.add(answerIdx);
  items.forEach((it, i) => {
    if (it.k === 'plancard' || (it.k === 'text' && isLongText(it.text))) anchor.add(i);
  });
  // 「已折叠」标注行与压缩卡片常显：它们标记的就是压缩/折叠这件事本身，
  // 折进折叠段等于没标——用户要靠它们知道对话在哪儿被压过
  items.forEach((it, i) => { if (it.k === 'fold' || it.k === 'summary') anchor.add(i); });
  const foldable = (it) => it.k !== 'text' && it.k !== 'plancard';
  items.forEach((it, i) => {
    if (it.k !== 'note') return;
    const nb = (j) => j >= 0 && j < items.length && foldable(items[j]);
    // 尾部豁免：最后一回合里，这条通知下方没有消息（正文/计划卡）——它是
    // 最新一条消息，常显；下方一旦有消息（哪怕材料行也无所谓）按常规规则走
    const tail = isLast && !items.slice(i + 1).some((x) => x.k === 'text' || x.k === 'plancard');
    if (tail || (!nb(i - 1) && !nb(i + 1))) anchor.add(i);
  });
  return { set: anchor, answerIdx: answerIdx };
}

let rafPending = false;
// live 气泡（正在流式输出的思考/正文）的落位：插话气泡是「还没生效、排在
// 所有进行中内容之后等待」的排队者——live 永远插在它上方，插话钉在最底
// （此前的 bug：插话渲染后 live 重建被 appendChild 到末尾，插话反被顶到
// 「正在思考的输出」上面，看起来像已经插队进了上下文）。
function liveInsert(view, el) {
  const col = view.col;
  const anchor = col.querySelector('[data-pending-steer], .msg.pending-send');
  if (anchor) col.insertBefore(el, anchor);
  else col.appendChild(el);
}
// 超长流式正文的降频档：md() 是全文重 parse + innerHTML 重建，长输出随帧数
// 平方增长（每个 delta 帧都把累计全文再 parse 一遍）。超过 32KB 后每 150ms
// 才重渲染一次——短输出（绝大多数）不受影响，走原来的每帧节奏。
const LIVE_BIG = 32768;
const LIVE_BIG_MS = 150;
let liveLastPaint = 0;    // 上次实际重渲染 .md 的时刻（performance.now）
let livePendingTimer = 0; // 降频档的补渲染定时器
function liveRender(view) {
  view = view || mainView;
  if (rafPending) return;
  rafPending = true;
  requestAnimationFrame(() => {
    rafPending = false;
    // 降频档时间窗未到：本帧不重渲染，到点后补一次（期间新帧只会顺延这个定时）
    const txt = view.state.streamText || '';
    if (txt.length > LIVE_BIG) {
      const since = performance.now() - liveLastPaint;
      if (since < LIVE_BIG_MS) {
        clearTimeout(livePendingTimer);
        livePendingTimer = setTimeout(() => {
          livePendingTimer = 0;
          if (view.state.streaming) liveRender(view); // 流已结束就别渲染（removeLive 已清场）
        }, LIVE_BIG_MS - since);
        return;
      }
    }
    liveLastPaint = performance.now();
    let el = document.getElementById(view.liveId);
    if (!el) {
      el = document.createElement('div');
      el.className = 'msg assistant';
      el.id = view.liveId;
      el.innerHTML =
        '<div class="proc-row running open" id="' + view.liveId + '-think">' +
          '<div class="proc-head">' + chev('p-chev') + '<span class="p-name">思考中…</span></div>' +
          '<div class="proc-body"><div class="tk-text"></div></div>' +
        '</div><div class="md"></div>';
      liveInsert(view, el);
      bindProcToggle(el.querySelector('#' + view.liveId + '-think'));
    }
    const tk = el.querySelector('.proc-row');
    const thinking = view.state.streamThinking || '';
    if (thinking) {
      tk.style.display = '';
      const tkt = tk.querySelector('.tk-text');
      tkt.textContent = thinking;
      // 内滚贴底才跟随：展开思考块往回读推理时不被每帧拽到底（外层钉住同款语义）
      if (tkt.scrollHeight - tkt.scrollTop - tkt.clientHeight < 48) tkt.scrollTop = tkt.scrollHeight;
    } else {
      tk.style.display = 'none';
    }
    el.querySelector('.md').innerHTML = md(txt);
    followScroll(view, false); // 贴近底部才跟随：流式中上翻阅读不被拽回
  });
}

/* ---------- 全局事件流（/api/events）：后台派发进度与回报的统一入口 ---------- */

let eventStream = null;
let eventsEverOpened = false; // SSE 是否已成功连过（区分首连与重连）

function connectEvents() {
  try {
    eventStream = new EventSource('/api/events');
    eventStream.onopen = () => {
      // 重连即补拉待审批/待回答清单：广播无回放（question_required 等一次性
      // 广播），断线窗口里错过的就永远错过了——远程中继对单请求有 150s 上限，
      // SSE 被掐后 3s 重连，重连窗口里的广播必丢；20s 兜底轮询只在
      // readyState!==1 时拉，重连太快又会被跳过。计划在对话区显示出来、
      // 审批卡却不出现（2026-10-30 远程实报）即此：正文走会话自己的流（有
      // 接回机制），审批卡走全局广播（丢了就没了）。首次连接跳过——
      // app.js 启动时已经拉过，别多打一次。
      if (eventsEverOpened) { loadApprovals(); loadQuestions(); }
      eventsEverOpened = true;
    };
    eventStream.onmessage = (e) => {
      try { handleGlobalEvent(JSON.parse(e.data)); } catch (err) { /* 跳过坏帧 */ }
    };
  } catch (e) { /* 不支持 SSE 时退化为手动刷新 */ }
}

function removeLive(view) {
  clearTimeout(livePendingTimer); // 降频档的补渲染定时器随 live 清场一起作废
  livePendingTimer = 0;
  const live = document.getElementById((view || mainView).liveId);
  if (live) live.remove();
}

// 打开/切换会话的导航状态：切换在途时，旧会话事件触发的「就地翻新」要让路，
// 否则一次切换会被旧会话的刷新顶回去（表现为点了别的会话却切不动）
const navBusy = { main: false, sub: false };
// 导航令牌：每次切换/回首页自增，响应落地前对不上令牌即丢弃。
// 只靠 navBusy 挡不住「晚到的旧响应反超新导航」——完成事件触发的重载与用户
// 点击并发时，旧的大响应可能后到获胜，把界面顶回旧会话（实测的「输出完成后
// 被强切回完成的对话」即此）。令牌保证永远以最后一次导航为准。
const navTok = { main: 0, sub: 0 };

function refreshCurrent() {
  if (navBusy.main) return; // 正在切换：别插队
  // 事件驱动的就地翻新走 stay：保留视口（钉住阅读不被拽到底）、展开记忆与
  // 已上翻加载的分批窗口（旧路径 jump 全重置——子对话回报一多阅读就被打断）
  if (state.cur && !state.streaming) openSession(state.cur.id, { stay: true });
}

// 事件驱动的整包重取节流（见《前端渲染性能分析-主对话卡顿.md》）：子对话回报/
// 自动汇报轮期间 done/report_ready/message/session_updated 接连到达，每个都
// 整包 GET（主对话几 MB JSON）+ 全量渲染。尾随合并 500ms：期间新事件顺延计时，
// 最后一次事件后 500ms 才重取，状态最终一致；执行时读当下状态（cur/streaming/
// navBusy 都在执行点检查），防抖窗口内切走会话不会误刷。
let refreshCurrentTimer = 0;
function refreshCurrentSoon(delay) {
  clearTimeout(refreshCurrentTimer);
  refreshCurrentTimer = setTimeout(() => {
    refreshCurrentTimer = 0;
    refreshCurrent();
  }, delay == null ? 500 : delay);
}

// 同款节流的 reloadMessages（attachRun 跟随后台执行时，轮内每条 message 事件
// 都整包重取——汇报轮几十条消息就是几十次全量拉取）。按视图各管各的。
const reloadTimers = {};
function reloadMessagesSoon(view, delay) {
  view = view || mainView;
  clearTimeout(reloadTimers[view.key]);
  reloadTimers[view.key] = setTimeout(() => {
    reloadTimers[view.key] = 0;
    reloadMessages(view);
  }, delay == null ? 400 : delay);
}

function refreshCurrentSub() {
  if (navBusy.sub) return;
  if (subState.cur && !subState.streaming) openSubSession(subState.cur.id);
}

function handleGlobalEvent(ev) {
  const viewing = state.cur && ev.sessionId === state.cur.id;
  // 子对话视图并行接收同一事件流：右栏正看这个子对话且它没有本地流在跑（后台自动汇报）
  const subViewing = subState.cur && ev.sessionId === subState.cur.id;
  // 会话 id -> 它的父会话 id（子对话才返回；数据源是全量 state.sessions——
  // 子对话执行完时它早已在列表里，新建窗口期的缺失只影响一次提示，无害）
  const parentOf = (sid) => {
    const s = (state.sessions || []).find((x) => x.id === sid);
    return s && s.parentId ? s.parentId : null;
  };
  // 侧栏「⏸ 待确认」标记与橙点直接由 state.approvals / state.questions 推导（见
  // app-left.js 的 sessionPendingAsk）：下面的 push/remove 更新清单后 renderPanels
  // 会顺带刷新会话列表，不再单独维护事件驱动的 dotApproval 影子表。
  switch (ev.type) {
    case 'providers':
      // 模型服务商状态变化（公告同步/自动停用恢复/toggle）：刷新模型菜单，
      // 公司通道自动停用时默认模型随即切到可用服务商
      loadModels();
      break;
    case 'brand':
      // 品牌下发变化（服务端管理页「品牌」页签保存 → 公告同步应用）：重拉文字
      // 配置并刷新（LOGO 走 /logo.png 自然换装）
      loadBrandUI();
      break;
    case 'sub_start':
      subActs[ev.sessionId] = [];
      delete subDone[ev.sessionId];
      bindOrphanCard(ev.sessionId);
      renderSubActs(ev.sessionId);
      loadSessions();
      refreshDrawer();
      break;
    case 'sub_tool':
      pushSubAct(ev);
      break;
    case 'sub_end':
      subDone[ev.sessionId] = true;
      dotDone[ev.sessionId] = true;
      // 子对话完成且右栏没正看着它：所属主对话在侧栏那行也亮绿点——否则人工
      // 建的子对话干完了，用户不知道是哪个主对话名下的事（打开主对话即清）
      { const par = parentOf(ev.sessionId); if (par && !subViewing) dotDone[par] = true; }
      loadSessions();
      refreshDrawer();
      if (subViewing && !subState.streaming) refreshCurrentSub(); // 右栏子对话就地翻新
      // 主对话区正显示派发它的那个会话（子任务卡在场）才翻新卡片状态；
      // 否则不动——不冲掉别的会话/首页的界面，也不拽走阅读位置
      if (state.cur && !state.streaming && subActNode(ev.sessionId)) renderMessages(state.msgs);
      break;
    case 'message':
      // 后台落库的用户消息（todo.go 广播）与环境快照行（权限等 PATCH 后立即
      // 落库的 env 行，role=system）：正看着这个会话就整体翻新（节流合并）
      if (viewing && !state.streaming && ev.message && (ev.message.role === 'user' || ev.kind === 'env')) refreshCurrentSoon();
      // env 行是「环境条目刚变过」的信号（权限/能力/计划目标/工作目录；人工改权限
      // 走 PATCH，不发 session_updated）：顺手把权限胶囊跟上——同一个会话的另一处
      // 视图（右栏子对话）、别的窗口、手机端都靠这里，不必等下一次切会话
      if (ev.kind === 'env') {
        const sid = ev.sessionId || (ev.message && ev.message.sessionId);
        if (sid) loadSessions().then(() => applySessionPermFromList(sid));
      }
      break;
    case 'pending':
      // 插话/排队队列变化：只更新对应的那个视图（自己窗口的操作也会推回来，天然多端一致）
      if (state.cur && ev.sessionId === state.cur.id) applyPending(mainView, ev.pending);
      if (subState.cur && ev.sessionId === state.cur.id) applyPending(subView, ev.pending);
      break;
    case 'stopped':
      // 用户显式停止（runtime.go 终态广播）：不算「完成未查看」，只刷列表
      loadSessions();
      if (viewing && !state.streaming) refreshCurrentSoon();
      break;
    case 'done':
      // 一轮执行结束（含数字助理的自动汇报轮）：没正看着这个会话就点亮绿点，
      // 打开会话即清；正看着则就地翻新出最终结果。完成的是子对话且右栏没看着
      // → 所属主对话侧栏行同样亮绿点（与 sub_end 同口径，两条事件幂等互备）
      if (!viewing) {
        dotDone[ev.sessionId] = true;
        const par = parentOf(ev.sessionId);
        if (par && !subViewing) dotDone[par] = true;
      }
      loadSessions();
      refreshDrawer();
      if (viewing && !state.streaming) refreshCurrentSoon();
      if ((state.sessions.find((s) => s.id === ev.sessionId) || {}).mode === 'assistant') setFace('done', viewing ? 4000 : 0); // 没看着＝方案C常驻：举纸+微笑+徽章，点开才清；正看着＝4秒回待机
      break;
    case 'report_ready':
      // 子智能体/派发任务的回报已落库（数字助理与普通任务派发同走这里）：
      // 没正看着父会话就点亮「完成未查看」绿点；正看着则整体翻新，
      // 并自动接上服务端随回报启动的汇报轮，实时看到汇报输出
      if (!viewing) dotDone[ev.sessionId] = true;
      loadSessions();
      refreshDrawer();
      if (viewing && !state.streaming) refreshCurrentSoon();
      break;
    case 'session_updated':
      // 权限档/能力开关的变更（审批卡「始终允许」升级、别处或另一窗口改权限）
      // 优先用广播自带的新值（permission/tools）就地应用——「事件→再拉列表」的
      // 二次往返要过 loadSessions 的防抖合并，流式执行中事件密集时窗口内的调用
      // 等不到新列表，权限胶囊因此停在旧档（曾反复出现，别再退回去）。其余
      // session_updated（改名/清空等）不带权限字段，仍走列表刷新取数。
      if (ev.permission) {
        applySessionPermToViews({ id: ev.sessionId, permission: ev.permission, tools: ev.tools || [] });
        loadSessions(); // 侧栏等列表数据照常刷新，但不阻塞胶囊应用
      } else {
        loadSessions().then(() => applySessionPermFromList(ev.sessionId));
      }
      refreshDrawer();
      // 后台模型摘要压缩的状态机（手动/提前自动压缩 202 即回、goroutine 悄悄跑）：
      // started 登记并翻新出「后台压缩中…」按钮态；完成（reason=compact）或失败
      // 移除登记——多窗口/切走对话再切回都靠这张表还原状态
      const creason = ev.data && ev.data.reason;
      if (creason === 'compact_started') {
        compactingSess.add(ev.sessionId);
      } else if (creason === 'compact' || creason === 'compact_failed') {
        compactingSess.delete(ev.sessionId);
      }
      if (creason === 'compact_failed') {
        toast('后台压缩失败：' + ((ev.data && ev.data.error) || '未知原因'), 8000);
      }
      // 压缩改写了历史（reason=compact：compact 标记 UPDATE、摘要行落库，均不
      // 改变消息条数与末条 seq）——前缀缓存主动作废，否则前缀复用判定永远命中，
      // 边界处的「已折叠」行与压缩卡片在流式期间与就地翻新中出不来（要等切换
      // 会话/刷新页面才全量重建）。作废后：非流式由下方 refreshCurrentSoon 立即
      // 重建；流式中等下一条 message 事件或 done 后的翻新落地
      if (creason === 'compact') {
        if (viewing) mainView.state._pfxSid = null;
        if (subViewing) subView.state._pfxSid = null;
      }
      if ((creason === 'compact_started' || creason === 'compact_failed') && subViewing && !subState.streaming) refreshCurrentSub();
      if (viewing && !state.streaming) refreshCurrentSoon();
      break;
    case 'title':
      // 会话标题被改（AI 自动命名组件在流结束数秒后晚到落库 / 别处窗口手动改
      // 名）：翻新侧栏行、右栏子对话下拉与顶栏——侧栏只在流结束那一刻刷新一次，
      // 不跟这条广播的话晚到的改名当面永远看不见，观感就是「自动命名没生效」
      loadSessions();
      refreshDrawer();
      if (viewing) {
        state.cur.title = ev.title; // 内存 brief 一并对齐（重命名弹窗等读它）
        applySessTitle({ mode: state.cur.mode, parentId: state.cur.parentId, title: ev.title });
      }
      if (subViewing) { subState.cur.title = ev.title; subRenderHeader(); }
      break;
    case 'approval_required':
      pushApproval(ev);
      break;
    case 'approval_resolved':
      removeApproval(ev.approvalId);
      break;
    case 'question_required':
      pushQuestion(ev);
      break;
    case 'question_resolved':
      removeQuestion(ev.questionId);
      break;
    case 'question_deadline':
      // 停表广播（本窗口或其他端触发的）：就地更新倒计时显示，不重建卡片
      //（重建会丢自由输入框的焦点）
      if (ev.questionId) {
        const q = state.questions.find((x) => x.id === ev.questionId);
        if (q && !q.timerHeld) { q.timerHeld = true; q.deadlineMs = 0; }
        if (q) document.querySelectorAll('.q-card[data-qid="' + ev.questionId + '"]').forEach((card) => {
          const chip = card.querySelector('.q-count');
          if (chip) paintQCount(chip, q, Date.now());
          const stop = card.querySelector('.q-stop');
          if (stop) stop.classList.add('hidden');
        });
      }
      break;
    case 'ask_alert':
      // 久等未答的声光提醒（叮声后端已响）：全屏闪烁 + 朗读，点击跳去作答
      showAskAlert(ev);
      break;
    case 'ask_alert_dismiss':
      // 桌面全屏蒙层（后端的第二处提醒）被点掉了：主窗口这层一起收——
      // 两处提醒任意点一处、两边同退（另一半边见 hideAskAlert 里的 POST）
      hideAskAlert();
      break;
    case 'children':
      // 本会话有子进程起/停：刷新进度面板底部的「运行中的程序」
      loadProgress();
      break;
    case 'progress':
      loadProgress();
      break;
    case 'update_available':
      // 门铃事件：拉全量信息（含安装包直链）渲染更新横幅
      api('/api/status').then((st) => {
        if (st.update && st.update.available) showUpdateBanner(st.update);
      }).catch(() => {});
      break;
    case 'update_ready':
      // 主程序已静默交换：就绪横幅（立即重启 / 稍后等自然重启）
      api('/api/status').then((st) => {
        if (st.update) showUpdateBanner(st.update);
      }).catch(() => {});
      break;
    case 'update_installer': {
      // 整包静默下载的进度/结果（stage: downloading|launched|error）：
      // 更新横幅就地显示——不弹窗、不开新页，失败也只是横幅一行红字
      const d = ev.data || {};
      const bar = document.getElementById('update-banner');
      if (!bar) break;
      const text = bar.querySelector('.ub-text');
      if (!text) break;
      if (d.stage === 'launched') {
        text.innerHTML = '安装包已下载完成，安装程序已启动——按安装向导完成升级即可';
        const acts = bar.querySelector('.ub-actions');
        if (acts) acts.innerHTML = '';
      } else if (d.stage === 'downloading') {
        text.innerHTML = '正在后台下载新版本安装包，下载完成后将自动运行安装…';
      } else {
        text.innerHTML = '安装包下载失败：<span style="color:var(--danger)">' + esc(d.text || '未知原因') + '</span>' +
          '　<button class="ub-confirm" type="button" style="margin-left:8px">重试</button>';
        const acts = bar.querySelector('.ub-actions');
        if (acts) acts.innerHTML = '';
      }
      bar.classList.remove('hidden');
      break;
    }
    case 'company_resync': {
      // 新版一次性重新接入的结果（sync.go companyResyncOnce）：
      //   needWizard → 连不上公司服务端，弹带原有配置的向导让用户点一下；
      //   否则 → 已静默追平，刷新模型并轻提示。
      const cd = ev.data || {};
      if (cd.needWizard) {
        if (typeof openWizardForResync === 'function') openWizardForResync(cd);
      } else {
        if (typeof loadModels === 'function') { try { loadModels(); } catch (e) {} }
        if (typeof loadStatus === 'function') { try { loadStatus(); } catch (e) {} }
        if (typeof toast === 'function') {
          toast('公司模型配置已更新（' + (cd.models || 0) + ' 个模型）' + (cd.mode === 'announce' ? '' : ''));
        }
      }
      break;
    }
    case 'demo_record_started':
    case 'demo_record_stopped':
      // 演示记录器归用户直控（按钮）：任何窗口起录/停录，所有录制按钮同步收敛
      demoRec.active = ev.type === 'demo_record_started';
      demoRec.dir = ev.output || demoRec.dir;
      document.querySelectorAll('.rec-btn').forEach(demoRecSyncBtn);
      // 停录：【演示记录】通知落进该会话，正看着就翻新让它落屏
      if (ev.type === 'demo_record_stopped' && ev.sessionId) {
        if (state.cur && state.cur.id === ev.sessionId && !state.streaming) refreshCurrent();
        if (subState.cur && subState.cur.id === ev.sessionId && !subState.streaming) refreshCurrentSub();
      }
      break;
    case 'components_updated':
      // 组件已静默热更：前端半边对账增删（不刷新页面），轻提示不打扰
      if (window.CX && CX.refresh) {
        try { CX.refresh(); } catch (err) { console.warn('[更新] 组件热刷新失败', err); }
      }
      if (ev.title) toast('组件已更新：' + ev.title);
      if (typeof refreshUpdateBox === 'function') refreshUpdateBox();
      break;
    // 浏览器面板（桌面原生 WebView2，每会话独立实例）：browser_nav 同步工具栏
    // 地址/标题（AI 在面板未开时也会导航），browser_resize 是 AI 的 viewport 请求
    // （X/Y 复用为宽高），两个处理函数都在 app-right.js。nav 事件顺带强制
    // 重报矩形：面板曾因矩形上报失联被留在隐藏态（地址已填、舞台空白），
    // 每次导航到达都补一次上报，让面板自愈显示。带 sessionId 的事件只作用于
    // 本会话的面板——别的会话（含后台子对话的 AI）在导航，不刷本会话工具栏、
    // 不动本会话布局；不带 sessionId 的（Electron 壳/共享面板）维持全局口径。
    case 'browser_nav':
      if (ev.sessionId && (!state.cur || ev.sessionId !== state.cur.id)) break;
      bpwOnNav(ev);
      if (typeof bpwDesktop === 'function' && bpwDesktop() && typeof bpwActive === 'function' && bpwActive()) bpwReport(true, true);
      break;
    case 'browser_resize':
      if (ev.sessionId && (!state.cur || ev.sessionId !== state.cur.id)) break;
      bpwApplyViewport(ev.x);
      break;
    // 说明：delta/reasoning 不上全局总线（流式增量只走会话自己的 SSE）；
    // 真实失败也不广播——错误经会话详情的 error 字段到达
    // （session_updated → refreshCurrent → openSession 会带出报错条）
  }
}

/* ---------- 待用户决定的浮窗（审批 / 提问 / 计划审批）：两个落点 ----------
   按「这张卡属于谁」分两处落，各司其职：

   1) 本对话的卡 → 输入框上方（`#ask-bar` 主对话 / `#sub-ask-bar` 右栏子对话）：
      位置在输入卡之上、附件区之上，宽度与输入卡一致（max-width: --content-w），
      就地处理最顺手；手机端有待办时输入区不收起（否则卡跟着滑出屏）。
   2) 别的对话的卡 → 右下角角标堆（`#ask-dock`）：卡片顶部多一行「来自「某对话」」
      （点一下切到那个对话），一叠攒在角落互不打扰当前对话；顶部「—」按钮把它整块
      缩成一枚胶囊（只显示条数，点胶囊再展开），用户回头再处理。左侧栏的橙点
      照旧提示哪个会话在等。

   另外两条（两处共用）：
   · 重绘不动的字：轮询/事件重画按「卡片签名」比对——签名没变的卡片一个节点都不碰，
     内容真的变了才重建那一张；自由输入框的文字按请求 id 存草稿并回填。这正是
     「隔十几秒清空用户输入」的根治点（原来的实现每次都把容器 innerHTML 清空重画）。
   · 计划（kind=plan）不在这里铺长文：卡片只给标题 + 首段 + 三个动作，全文在右侧
     边栏的「计划」视图里按 Markdown 渲染，且那份文档与对话历史同寿命（见下节）。 */

const askCards = { main: new Map(), sub: new Map(), dock: new Map() }; // 落点 → (卡片 key → {sig, el})
const planAutoOpened = {}; // 计划提问 id → 已自动打开过右栏计划视图（用户手动关掉后不再自动弹）

// 自由输入框草稿：按请求 id 存（sessionStorage：刷新页面也还在，关标签页即清）
function askDraftKey(id) { return 'jx.ask.' + id; }
function askDraftGet(id) {
  try { return sessionStorage.getItem(askDraftKey(id)) || ''; } catch (e) { return ''; }
}
function askDraftSet(id, v) {
  try { if (v) sessionStorage.setItem(askDraftKey(id), v); else sessionStorage.removeItem(askDraftKey(id)); } catch (e) {}
}
function askDraftClear(id) { try { sessionStorage.removeItem(askDraftKey(id)); } catch (e) {} }

// 字符串指纹：卡片签名的正文部分（计划全文不进签名的明文，只进哈希）
function askHash(s) {
  let h = 5381;
  for (let i = 0; i < s.length; i++) h = ((h << 5) + h + s.charCodeAt(i)) | 0;
  return h;
}

// 会话标题：后端随请求下发的最准（会话可能已归档、不在当前清单里），再退到会话清单
function sessionTitleOf(sid, hint) {
  if (hint) return hint;
  const s = (state.sessions || []).find((x) => x.id === sid);
  if (s && s.title) return s.title;
  if (state.cur && state.cur.id === sid && state.cur.title) return state.cur.title;
  if (subState.cur && subState.cur.id === sid && subState.cur.title) return subState.cur.title;
  return '另一个对话';
}

// 「来自「某对话」」：别的对话的卡靠上位置的一行来源，点一下切到那个对话
// 样式约定（用户口径）：卡片左边条与本对话的卡同色（主题蓝），来源行里
// 「来自「…」」不加粗、右侧「点击跳转 ›」加粗——见 style.css 的 .ap-src 一段
function askSrcStrip(sid, hint) {
  const b = document.createElement('button');
  b.type = 'button';
  b.className = 'ap-src';
  b.title = '这条请求属于另一个对话：点一下切过去（不点也能就地处理）';
  b.innerHTML =
    '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
    '<path d="M21 11.5a8.4 8.4 0 0 1-8.5 8.3 9 9 0 0 1-3.4-.6L4 21l1.4-4a8.2 8.2 0 0 1-1.4-4.5A8.4 8.4 0 0 1 12.5 4 8.4 8.4 0 0 1 21 11.5z"/></svg>' +
    '<span class="ap-src-title">来自「' + esc(sessionTitleOf(sid, hint)) + '」</span>' +
    '<span class="ap-src-go">点击跳转 ›</span>';
  b.onclick = () => askJumpTo(sid);
  return b;
}

// 切到那条请求所属的对话（就在本对话时只是把焦点还给输入框）。跳过去＝用户
// 开始处理了：这条对话下待审批的计划一并登记「已在处理」（久等提醒不再响）。
function askJumpTo(sid) {
  if (!sid) return;
  notePlanHandled(sid, '');
  if (state.cur && state.cur.id === sid) { $('input').focus(); return; }
  openSession(sid);
}

// 自由输入行：值存草稿、重建后回填（卡片的「发送/修改/拒绝」都走它）
function askFreeRow(id, placeholder, btnText, onSend) {
  const row = document.createElement('div');
  row.className = 'q-free';
  row.onclick = (e) => e.stopPropagation(); // 输入区不触发整卡点击（计划卡：整张卡点开计划）
  const input = document.createElement('input');
  input.type = 'text';
  input.placeholder = placeholder;
  input.value = askDraftGet(id);
  input.dataset.draftkey = id;
  input.addEventListener('input', () => askDraftSet(id, input.value));
  const send = () => {
    const v = input.value.trim();
    askDraftSet(id, input.value);
    onSend(v);
  };
  const btn = document.createElement('button');
  btn.className = 'ghost';
  btn.textContent = btnText;
  btn.onclick = send;
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); send(); }
  });
  row.appendChild(input);
  row.appendChild(btn);
  return row;
}

/* ---------- 对话流里的计划卡（exit_plan_mode 提交过的计划） ----------
   输入区上方那张审批卡批了/驳了就收走了，但计划本身得在对话里留个常住的入口：
   这条卡跟着提交它的那一轮常显（不会被折进过程段），点一下就再打开右栏「计划」
   视图看全文——刷新、切走、关掉软件重开，它都还在（正文从会话历史里读回）。 */

// 这份计划还在等审批吗：按正文与 /api/questions 里 kind=plan 的提问配对
// （审批卡与计划卡拿的是同一份正文），配得上就给「待审批」徽章。
function planPendingIdOf(plan) {
  const head = String(plan || '').trim().replace(/…$/, '');
  if (!head) return '';
  const q = (state.questions || []).find((x) => x.kind === 'plan'
    && (x.plan === plan || String(x.plan || '').startsWith(head)));
  return q ? q.id : '';
}

// 计划卡：标题 + 首段 + 「查看计划」（打开右栏并定位到这一份）。正文取不到
// （参数是坏行）时退回普通工具行，别造一张空卡。
function planCardRow(tc, seq, result) {
  let plan = '';
  try { plan = String(JSON.parse((tc && tc.function && tc.function.arguments) || '{}').plan || '').trim(); } catch (e) { plan = ''; }
  if (!plan) return toolRow(tc, result, { seq });
  const info = planSummaryOf(plan);
  const pendingId = planPendingIdOf(plan);
  const sid = (result && result.sessionId) || (state.cur && state.cur.id) || '';
  const card = document.createElement('div');
  card.className = 'plan-card' + (pendingId ? ' pending' : '');
  if (seq != null) card.dataset.seq = seq;
  const head = document.createElement('div');
  head.className = 'pc-head';
  head.innerHTML = '<b>计划已提交</b>' +
    (pendingId ? '<span class="pc-badge">待审批</span>' : '');
  const body = document.createElement('div');
  body.className = 'pc-body';
  body.innerHTML = '<p class="pc-title">' + esc(info.title) + '</p>' +
    (info.desc ? '<p class="pc-desc">' + esc(info.desc) + '</p>' : '');
  const acts = document.createElement('div');
  acts.className = 'pc-acts';
  acts.textContent = pendingId ? '等待你的审批 · 点击这张卡看计划全文' : '点击这张卡看计划全文';
  card.appendChild(head);
  card.appendChild(body);
  card.appendChild(acts);
  card.onclick = () => openPlanView(sid, seq); // 整张卡可点：点哪儿都能打开那份计划
  card._plan = plan; // 徽章翻新用（refreshPlanCards 据此重算「待审批」）
  return card;
}

// 计划摘要（输入区那张卡只显示这一段，全文去右栏看）
function planTitleOf(md) {
  const m = /^#{1,6}\s+(\S.*)$/m.exec(String(md || '').trim());
  if (m) return m[1].trim().slice(0, 80);
  const first = String(md || '').trim().split(/\r?\n/)[0] || '';
  return first.replace(/[#*`>\s]+$/, '').slice(0, 80) || '计划';
}
function planSummaryOf(md) {
  const lines = String(md || '').split(/\r?\n/);
  let i = 0;
  while (i < lines.length && (lines[i].trim() === '' || /^#{1,6}\s/.test(lines[i]))) i++; // 跳过标题
  const para = [];
  while (i < lines.length && lines[i].trim() !== '' && !/^#{1,6}\s/.test(lines[i])) { para.push(lines[i].trim()); i++; }
  const desc = para.join(' ').replace(/\s+/g, ' ')
    .replace(/^[-*+]\s+/, '').replace(/\*\*/g, '').replace(/`/g, '').trim();
  return { title: planTitleOf(md), desc: desc.length > 180 ? desc.slice(0, 180) + '…' : desc };
}

/* ---------- 事件 → 状态 ---------- */

function pushApproval(ev) {
  if (!ev.approvalId || state.approvals.some((a) => a.id === ev.approvalId)) return;
  state.approvals.push({
    id: ev.approvalId, sessionId: ev.sessionId || '', sessionTitle: ev.title || '',
    tool: ev.name || '', summary: ev.summary || '', reason: ev.reason || '',
    createdAt: ev.createdAt || '',
  });
  renderPanels();
}

function removeApproval(id) {
  hideAskAlertIfMatched(id); // 声光提醒正为它闪着：审批已决定/超时收卡，收起并取消朗读
  state.approvals = state.approvals.filter((a) => a.id !== id);
  askDraftClear(id);
  renderPanels();
}

async function loadApprovals() {
  try {
    const d = await api('/api/approvals');
    state.approvals = (d.approvals || []).map((a) => ({
      id: a.id, sessionId: a.sessionId || '', sessionTitle: a.sessionTitle || '',
      tool: a.tool || '', summary: a.summary || '', reason: a.reason || '',
      createdAt: a.createdAt || '',
    }));
    renderPanels();
  } catch (e) { /* 服务离线时忽略 */ }
}

async function decideApproval(id, approved, always) {
  removeApproval(id); // 先乐观移除，避免双击
  try {
    await api('/api/approvals/' + id, { method: 'POST', body: JSON.stringify({ approved, always }) });
  } catch (e) {
    $('hint').textContent = '审批失败：' + e.message;
    setTimeout(() => { $('hint').textContent = ''; }, 3000);
  }
}

/* ---------- 提问 / 计划审批 ---------- */

function pushQuestion(ev) {
  if (!ev.questionId || state.questions.some((q) => q.id === ev.questionId)) return;
  state.questions.push({
    id: ev.questionId, sessionId: ev.sessionId || '', sessionTitle: ev.title || '',
    kind: ev.kind || 'ask', question: ev.summary || '', plan: ev.plan || '',
    options: ev.options || [],
    deadlineMs: ev.deadlineMs || 0, timerHeld: !!ev.held, // 倒计时（仅 ask/enter-plan 有）
    noTimeout: !!ev.noTimeout, // 模型显式不限时（timeout_sec=0）：一直等用户作答
  });
  renderPanels();
  noteFreshPlanQuestions(); // 新到达的计划：右栏切到这一份（别停在上次看的那版）
  maybeAutoOpenPlan(); // 计划：右栏「计划」视图自动打开（本对话的才自动开）
}

function removeQuestion(id) {
  const q = state.questions.find((x) => x.id === id);
  if (q && q.kind === 'plan') stashPlanDoc(q); // 提问没了，计划本身要留在右栏（持久化）
  hideAskAlertIfMatched(id); // 声光提醒正为它闪着：问题解决了就收起
  state.questions = state.questions.filter((x) => x.id !== id);
  askDraftClear(id);
  renderPanels();
}

async function loadQuestions() {
  try {
    const d = await api('/api/questions');
    state.questions = (d.questions || []).map((q) => ({
      id: q.id, sessionId: q.sessionId, sessionTitle: q.sessionTitle || '',
      kind: q.kind || 'ask', question: q.question || '', plan: q.plan || '', options: q.options || [],
      deadlineMs: q.deadlineMs || 0, timerHeld: !!q.timerHeld, noTimeout: !!q.noTimeout,
    }));
    renderPanels();
    noteFreshPlanQuestions(); // 新到达的计划（含重启后拉回的待审批）：右栏切到这一份
    maybeAutoOpenPlan();
  } catch (e) { /* 服务离线时忽略 */ }
}

async function decideQuestion(id, answer) {
  const q = state.questions.find((x) => x.id === id);
  if (q && q.kind === 'plan') stashPlanDoc(q); // 先留一份副本，批/驳之后右栏仍看得到
  // 「忽略计划」= 搁置：后端要保留这份计划（它仍在等处置），所以这里不做乐观移除
  // ——移除早了会让用户以为计划没了；批/驳才移除（后端也会确认）。
  if (answer !== '__ignore__') removeQuestion(id); // 先乐观移除，避免双击
  try {
    await api('/api/questions/' + id, { method: 'POST', body: JSON.stringify({ answer }) });
  } catch (e) {
    $('hint').textContent = '提交失败：' + e.message;
    setTimeout(() => { $('hint').textContent = ''; }, 3000);
  }
  if (q && q.kind === 'plan') loadPlanView(); // 历史里落的正文接上来（替换临时副本）
}

/* ---------- 卡片与重绘 ---------- */

// 右栏正显示着的子对话（右栏开着、且停在子对话视图）：它自己的待办卡在右栏输入区
// 上方显示，主对话区那份就不再重复；右栏一关/切走，主对话区立刻接过来显示。
function subVisibleSid() {
  const panel = $('rpanel');
  if (!panel || panel.classList.contains('hidden')) return '';
  if (rpActive !== 'subs') return '';
  return (subState.cur && subState.cur.id) || '';
}

// 本视图要显示的待办（只有「本对话自己的」）：
// · 主对话区＝当前会话的卡；右栏子对话＝它自己的卡。
// 别的对话的卡一律进右下角的角标堆（renderAskDock），不再混进输入框上方——
// 本对话的卡贴在输入区上方「就地处理」，别的对话的卡攒在角落「回头处理」。
function ownAskItems(view) {
  const own = ((view.state.cur || {}).id) || '';
  if (!own) return { own, list: [] };
  const list = [];
  for (const ap of state.approvals) if (ap.sessionId === own) list.push({ type: 'approval', id: ap.id, it: ap });
  for (const q of state.questions) if (q.sessionId === own) list.push({ type: 'question', id: q.id, it: q });
  return { own, list };
}

// 不属于任何「正开着的对话」的待办：主对话与右栏子对话都占着的那两个会话要排除，
// 免得同一张卡在两处出现。
function otherAskItems() {
  const mainSid = (state.cur && state.cur.id) || '';
  const subSid = subVisibleSid();
  const list = [];
  for (const ap of state.approvals) {
    if (ap.sessionId !== mainSid && ap.sessionId !== subSid) list.push({ type: 'approval', id: ap.id, it: ap });
  }
  for (const q of state.questions) {
    if (q.sessionId !== mainSid && q.sessionId !== subSid) list.push({ type: 'question', id: q.id, it: q });
  }
  return list;
}

function askCardKey(e) { return (e.type === 'approval' ? 'a:' : 'q:') + e.id; }

// 卡片签名：只跟「渲染出来的内容」有关。签名一致 → 原节点原样留着，输入/焦点/滚动都不动
function askSigOf(e, own) {
  const it = e.it;
  const src = it.sessionId === own ? '' : sessionTitleOf(it.sessionId, it.sessionTitle);
  return [
    e.type, e.id, src, it.kind || '', it.tool || '',
    it.summary || '', it.reason || '', it.question || '',
    it.plan ? it.plan.length + '.' + askHash(it.plan) : '',
    (it.options || []).join('\u0001'),
  ].join('\u0002');
}

function askPending(view) {
  const bar = viewEl(view, 'ask-bar');
  return !!bar && !bar.classList.contains('hidden');
}

function buildApprovalCard(e, own) {
  const ap = e.it;
  const card = document.createElement('div');
  card.className = 'ap-card' + (ap.sessionId === own ? '' : ' from-other');
  if (ap.sessionId !== own) card.appendChild(askSrcStrip(ap.sessionId, ap.sessionTitle));
  const head = document.createElement('div');
  head.className = 'ap-head';
  head.innerHTML = '<b>需要批准</b>' + (ap.tool ? '<span>' + esc(ap.tool) + '</span>' : '');
  card.appendChild(head);
  const body = document.createElement('div');
  body.className = 'ap-body';
  body.innerHTML = '<code>' + esc(ap.summary || '') + '</code>' +
    (ap.reason ? '<p>' + esc(ap.reason) + '</p>' : '');
  card.appendChild(body);
  const acts = document.createElement('div');
  acts.className = 'ap-actions';
  acts.innerHTML =
    '<button class="ok" data-act="ok">允许</button>' +
    '<button class="ok always" data-act="always" title="常设授权：命令升级/能力申请点亮对应能力开关；工作区外写入会把会话切到「完全访问」（三项能力捆绑点亮），此后不再逐次审批">始终允许</button>' +
    '<button class="deny" data-act="deny">拒绝</button>';
  acts.querySelectorAll('button').forEach((b) => {
    b.onclick = () => decideApproval(ap.id, b.dataset.act !== 'deny', b.dataset.act === 'always');
  });
  card.appendChild(acts);
  return card;
}

/* ---------- 提问卡倒计时（ask_user_question 的 timeout_sec） ----------
   后端是计时的唯一权威；前端只按 deadlineMs 跳显示，且只改 textContent——
   绝不进 askSigOf 卡片签名（签名一变整卡重建，自由输入框的焦点就没了）。
   停表是单向的：倒计时旁的停止按钮 / 鼠标经过卡片（电脑端）/ 轻触卡片
   （手机端）都触发；停了就转为不限时，选完选项即止。 */
let askTickTimer = 0;
function askEnsureTicker() {
  if (askTickTimer) return;
  askTickTimer = setInterval(() => {
    const now = Date.now();
    for (const q of state.questions) {
      if (q.kind === 'plan' || (!q.deadlineMs && !q.timerHeld && !q.noTimeout)) continue;
      document.querySelectorAll('.q-card[data-qid="' + q.id + '"] .q-count').forEach((chip) => paintQCount(chip, q, now));
    }
  }, 500);
}

function paintQCount(chip, q, now) {
  if (q.timerHeld || q.noTimeout) {
    chip.textContent = '∞ 不限时';
    chip.classList.add('held');
    chip.classList.remove('urgent');
    return;
  }
  if (!q.deadlineMs) { chip.classList.add('hidden'); return; }
  const remain = Math.max(0, Math.ceil((q.deadlineMs - now) / 1000));
  chip.classList.remove('held', 'hidden');
  chip.classList.toggle('urgent', remain > 0 && remain <= 10);
  chip.textContent = remain <= 0 ? '超时中…' : Math.floor(remain / 60) + ':' + String(remain % 60).padStart(2, '0');
}

// askHoldTimer 停表（幂等）：就地更新所有窗口里的这张卡，再通知后端；
// 后端会广播 question_deadline，让别的端同步（本地已画好，广播到达是确认）。
function askHoldTimer(id) {
  const q = state.questions.find((x) => x.id === id);
  if (!q || q.timerHeld || q.noTimeout) return; // 不限时本就没有表可停
  q.timerHeld = true;
  q.deadlineMs = 0;
  document.querySelectorAll('.q-card[data-qid="' + id + '"]').forEach((card) => {
    const chip = card.querySelector('.q-count');
    if (chip) paintQCount(chip, q, Date.now());
    const stop = card.querySelector('.q-stop');
    if (stop) stop.classList.add('hidden');
  });
  api('/api/questions/' + id, { method: 'POST', body: JSON.stringify({ stop_timer: true }) }).catch(() => {});
}

/* ---------- 询问声光提醒（设置·常规：久等未答时叮声+全屏闪烁+朗读） ----------
   时序（用户口径）：
     ① 浮窗弹出：叮声在触发那一刻由后端系统音效立即响（事件到达即已响，前端不重复）；
     ② 浮窗上显示 15 秒可见倒计时（数字 + 进度条）；
     ③ 倒计时走完才开始朗读——朗读走语音组件的 /api/voice/tts
        （组件停用时该端点 404，静默跳过：只闪不读）；
     ④ 15 秒内用户按掉/取消/关闭这层，或从任何入口进去处理（计划页/浮窗，
        见 notePlanHandled），都取消这次朗读；正在读的也一并掐断（代次作废）。
   观感（用户口径）：刚弹出只是很淡的压暗＋光晕呼吸，**从这一刻起用 15 秒缓慢缓入**
   （与倒计时同步）渐进成整屏主题色——到 15 秒才彻底变蓝，不闪入（护眼、防光敏）；
   变成主题色后**常驻、不再自动收起**，直到用户关掉这层（点背景/点卡片/Esc）或
   这条请求被处理（作答/进去处理），界面才恢复正常。 */
const ASK_ALERT_TTS_SEC = 15;   // 朗读前的倒计时（秒）；也是渐变到整屏主题色的时长
const ASK_ALERT_FADE_MS = ASK_ALERT_TTS_SEC * 1000; // 淡压暗 → 纯主题色的缓入时长
let askAlertQid = '';
let askAlertPayload = null;  // 本次提醒的内容（倒计时结束朗读时用）
let askAlertSettleTimer = 0; // 呼吸 6 秒后停成静态淡光（护眼：不一直动）
let askAlertCountTimer = 0;  // 倒计时数字/进度条刷新
let askAlertTtsTimer = 0;    // 倒计时到点 → 开始朗读
let askAlertAudio = null;    // 正在朗读的音频（取消时掐断）
let askAlertAudioURL = '';
let askAlertGen = 0;         // 代次：关掉/重开一次就 +1，作废在途的朗读请求

function showAskAlert(ev) {
  const ov = $('ask-alert');
  if (!ov) return;
  hideAskAlert(); // 上一次可能还挂着（同一条再来一次）：彻底收干净再开
  // 提问/计划审批与越权审批共用这一层提醒：kind 按来源措辞，askAlertQid 收
  // 两种 id（问题解决/审批决定时 hideAskAlertIfMatched 据此收起并取消朗读）
  askAlertQid = ev.questionId || ev.approvalId || '';
  askAlertPayload = ev;
  const kind = ev.approvalId ? '审批' : '提问';
  $('ask-alert-text').textContent =
    (ev.title ? '来自「' + ev.title + '」的' + kind + '：' : '') + (ev.summary || '有个请求需要你确认');
  ov.classList.remove('hidden', 'settle', 'solid'); // 从头来：先淡压暗，再慢慢渐变到主题色
  if (askAlertSettleTimer) clearTimeout(askAlertSettleTimer);
  askAlertSettleTimer = setTimeout(() => ov.classList.add('settle'), 6000);
  // 整屏主题色与倒计时同步：用 15 秒缓入（时长见 style.css 的 background-color
  // 过渡，由 .solid 触发）从「很淡的压暗」一路渐进到纯主题色，到 15 秒彻底变蓝
  // 并保持——不闪入（用户口径：渐变换色，怕刺眼/光敏）；这里不再排任何「自动收起」。
  const gen = ++askAlertGen;
  // 元素刚从 display:none 显出来，同帧加 .solid 不会有过渡（没有「变化前」的样式），
  // 所以先让淡压暗这一帧落定（读一次 offsetWidth 强制重排），下一帧再加。
  void ov.offsetWidth;
  requestAnimationFrame(() => { if (gen === askAlertGen) ov.classList.add('solid'); });
  // 15 秒可见倒计时：走完才朗读（点掉/进去处理连它一起取消）
  startAskAlertCount(Date.now() + ASK_ALERT_TTS_SEC * 1000);
  if (askAlertTtsTimer) clearTimeout(askAlertTtsTimer);
  askAlertTtsTimer = setTimeout(() => {
    askAlertTtsTimer = 0;
    if (gen === askAlertGen) speakAskAlert(gen);
  }, ASK_ALERT_TTS_SEC * 1000);
  // 随手一点就能关、且不打扰当前操作：点背景＝只关闭（原地不动，用户可能
  // 正在别的对话里忙）；点中央卡片＝关闭并跳去回答（跳转＝进去处理，一并
  // 停掉提醒计时）；Esc 同效关闭。三层互不冒泡，第一次点击永远只做关闭。
  ov.onclick = hideAskAlert;
  const box = ov.querySelector('.aa-box');
  if (box) box.onclick = (e) => {
    e.stopPropagation();
    notePlanHandled(ev.sessionId, ev.questionId); // 点卡片去处理：提醒计时一起停
    hideAskAlert();
    askJumpTo(ev.sessionId);
  };
  // 「此类弹窗提醒可在设置·常规里关闭或修改时长」那一行：点它打开设置·常规
  //（设置浮层 z-index 230 盖在提醒层之上，浮窗留着，关掉设置回来接着处理）。
  const setLink = $('ask-alert-settings');
  if (setLink) setLink.onclick = (e) => {
    e.stopPropagation(); // 不触发「点卡片＝关闭并跳去回答」
    if (typeof openSettings === 'function') openSettings('general');
  };
}

// 倒计时（数字 + 进度条）：只改文本/宽度，不重建节点。
function startAskAlertCount(deadline) {
  const wrap = $('ask-alert-count');
  if (!wrap) return;
  wrap.classList.remove('hidden', 'speaking');
  $('ask-alert-num').textContent = String(ASK_ALERT_TTS_SEC);
  $('ask-alert-tip').textContent = '秒后朗读提醒';
  const bar = $('ask-alert-bar');
  if (bar) bar.style.width = '0%';
  const tick = () => {
    const remain = Math.max(0, Math.ceil((deadline - Date.now()) / 1000));
    $('ask-alert-num').textContent = String(remain);
    if (bar) bar.style.width = Math.round(((ASK_ALERT_TTS_SEC - remain) / ASK_ALERT_TTS_SEC) * 100) + '%';
    if (remain <= 0 && askAlertCountTimer) { clearInterval(askAlertCountTimer); askAlertCountTimer = 0; }
  };
  tick();
  if (askAlertCountTimer) clearInterval(askAlertCountTimer);
  askAlertCountTimer = setInterval(tick, 200);
}

// 倒计时结束：开始朗读提醒内容（组件停用/播放被拦：只闪不读，界面如实标注）。
function speakAskAlert(gen) {
  const wrap = $('ask-alert-count');
  if (askAlertCountTimer) { clearInterval(askAlertCountTimer); askAlertCountTimer = 0; }
  if (wrap) {
    wrap.classList.add('speaking');
    $('ask-alert-num').textContent = '🔊';
    $('ask-alert-tip').textContent = '正在朗读提醒内容';
  }
  const ev = askAlertPayload || {};
  fetch('/api/voice/tts', {
    method: 'POST',
    headers: { 'Content-Type': 'application/json' },
    body: JSON.stringify({ text: ev.output || '有个问题需要您确认一下。', rate: 1 }),
  }).then((r) => (r.ok ? r.blob() : Promise.reject(new Error('tts ' + r.status))))
    .then((b) => {
      if (gen !== askAlertGen) return; // 期间用户关掉/进去处理了：不朗读
      const u = URL.createObjectURL(b);
      askAlertAudioURL = u;
      const el = new Audio(u);
      askAlertAudio = el;
      el.onended = () => {
        URL.revokeObjectURL(u);
        if (askAlertAudioURL === u) askAlertAudioURL = '';
        if (askAlertAudio === el) askAlertAudio = null; // 读完也不收起：提醒层常驻，等用户处置
      };
      el.play().catch(() => {
        if (gen === askAlertGen) $('ask-alert-tip').textContent = '未能朗读（浏览器拦截了自动播放）';
      });
    })
    .catch(() => {
      if (gen !== askAlertGen) return;
      if (wrap) $('ask-alert-tip').textContent = '语音组件未启用，未朗读'; // 只闪不读
    });
}

// hideAskAlert 关闭提醒层并清干净定时器与音频（点击/Esc/问题解决共用）：
// 关掉之后在途的朗读请求一律作废（代次 +1）——15 秒内取消就不会出声。
// 层真的可见时还会通知后端把**桌面全屏蒙层**（同一次提醒的第二处）一起关掉
//（qid 匹配当前轮才关：上一轮迟到的请求不许把刚弹出的新一轮蒙层误关）。
function hideAskAlert() {
  const ov = $('ask-alert');
  const wasVisible = ov && !ov.classList.contains('hidden');
  const closingQid = askAlertQid;
  if (ov) {
    ov.classList.add('hidden');
    ov.classList.remove('solid'); // 关掉即恢复原界面（下次重新从淡压暗慢慢渐变）
  }
  if (wasVisible) {
    fetch('/api/alert/screen', {
      method: 'POST', headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ qid: closingQid }),
    }).catch(() => { /* 老版本后端没这个接口：主窗口层照常收；全屏蒙层点一下即关 */ });
  }
  askAlertGen++; // 同时作废还没跑到的「加 .solid」那一帧（rAF 里比对代次）
  if (askAlertSettleTimer) { clearTimeout(askAlertSettleTimer); askAlertSettleTimer = 0; }
  if (askAlertCountTimer) { clearInterval(askAlertCountTimer); askAlertCountTimer = 0; }
  if (askAlertTtsTimer) { clearTimeout(askAlertTtsTimer); askAlertTtsTimer = 0; }
  if (askAlertAudio) { try { askAlertAudio.pause(); } catch (e) { /* 已结束/被拦：忽略 */ } askAlertAudio = null; }
  if (askAlertAudioURL) { URL.revokeObjectURL(askAlertAudioURL); askAlertAudioURL = ''; }
  const wrap = $('ask-alert-count');
  if (wrap) wrap.classList.add('hidden');
  askAlertQid = '';
  askAlertPayload = null;
}

function hideAskAlertIfMatched(id) {
  if (id && id === askAlertQid) {
    hideAskAlert();
  }
}

// Esc 关闭提醒层（只在层可见时生效，不影响别的 Esc 行为）。
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  const ov = $('ask-alert');
  if (ov && !ov.classList.contains('hidden')) hideAskAlert();
});

/* ---------- 「用户已经在处理这份计划了」的唯一登记口（久等提醒去重） ----------
   久等提醒（ask_alert）是给「没人管」的请求用的：用户已经点开看过了就不该再催。
   打开计划页/浮窗的入口有四条，全部汇到这里登记：
     · 输入区上方的待审批浮窗（buildQuestionCard 的计划卡，整张卡可点）；
     · 右下角角标堆里的那张卡（同一个 builder：点卡＝跳过去并打开计划页）；
     · 消息流里的计划卡（planCardRow，点整张卡打开右栏「计划」）；
     · 右栏「计划」页签本身（下面委托点击的那条）。
   登记动作＝对该计划提问调一次停表接口（幂等，前端 askHoldTimer）：后端据此
   把那次久等提醒作废（interact.go 的 askAlertDue），前端把正在闪的提醒层收起
   （连未开始的朗读一起取消）。「本回合新来的计划自动铺开」那一路不算用户打开
   （见 openPlanView 的 silent），提醒照旧——它本来就是叫用户回来的。 */
function notePlanHandled(sid, qid) {
  const ids = [];
  const add = (q) => { if (q && q.kind === 'plan' && ids.indexOf(q.id) < 0) ids.push(q.id); };
  add((state.questions || []).find((x) => x.id === qid));
  if (sid) for (const q of state.questions || []) if (q.sessionId === sid) add(q);
  for (const id of ids) askHoldTimer(id); // 停计时（后端据此作废那次提醒）
  if (askAlertQid && ids.indexOf(askAlertQid) >= 0) hideAskAlert(); // 正闪着它：收起并取消朗读
}

// 右栏「计划」页签被点开＝用户显式去看那份计划：同样登记。页签按钮由
// rpRenderTabs 生成（带 data-rp-id），这里委托点击比逐处挂钩子稳。
document.addEventListener('click', (e) => {
  const tab = e.target && e.target.closest ? e.target.closest('#rp-tabs .rp-tab') : null;
  if (!tab || tab.dataset.rpId !== 'plan') return;
  notePlanHandled(planSid || (state.cur && state.cur.id) || '', '');
});

function buildQuestionCard(e, own) {
  const q = e.it;
  const isPlan = q.kind === 'plan';
  const isEnter = q.kind === 'enter-plan';
  const card = document.createElement('div');
  card.className = 'ap-card q-card' + (isPlan ? ' plan' : '') + (q.sessionId === own ? '' : ' from-other');
  if (q.sessionId !== own) card.appendChild(askSrcStrip(q.sessionId, q.sessionTitle));
  const head = document.createElement('div');
  head.className = 'ap-head';
  head.innerHTML = '<b>' + (isPlan ? '计划审批' : (isEnter ? '进入计划模式？' : '助手提问')) + '</b>';
  card.appendChild(head);
  // 倒计时 + 停止按钮（计划审批与显式不限时没有表可停，只显示 ∞）：
  // data-qid 是倒计时就地刷写的定位锚；悬停/轻触卡片即停表（单向），停止按钮同效。
  if (!isPlan && (q.deadlineMs || q.timerHeld || q.noTimeout)) {
    card.dataset.qid = q.id;
    const count = document.createElement('span');
    count.className = 'q-count';
    head.appendChild(count);
    if (!q.noTimeout) { // 不限时没有停止按钮（也没必要挂悬停停表）
      const stop = document.createElement('button');
      stop.type = 'button';
      stop.className = 'q-stop' + (q.timerHeld ? ' hidden' : '');
      stop.title = '停止计时：不限时作答，选完选项即止';
      stop.innerHTML = '<svg viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="1.5"/></svg>';
      stop.onclick = (ev) => { ev.stopPropagation(); askHoldTimer(q.id); };
      head.appendChild(stop);
      card.addEventListener('mouseenter', () => askHoldTimer(q.id));
      card.addEventListener('touchstart', () => askHoldTimer(q.id), { passive: true });
    }
    askEnsureTicker();
    paintQCount(count, q, Date.now());
  }

  if (isPlan) {
    // 计划：这里只给摘要，全文在右栏「计划」视图（Markdown 渲染，可反复打开）
    // 别的对话的计划（含右下角角标堆里的卡）：点整张卡 = 跳回那个对话再打开
    // 计划全文——原地开右栏时主对话没换，这份计划看起来就像当前对话的，误导。
    const planFrom = q.sessionId || '';
    const planJump = !!planFrom && planFrom !== own;
    const info = planSummaryOf(q.plan || '');
    const body = document.createElement('div');
    body.className = 'ap-body';
    body.innerHTML = '<p class="q-plan-title">' + esc(info.title) + '</p>' +
      (info.desc ? '<p class="q-plan-desc">' + esc(info.desc) + '</p>' : '') +
      '<p class="q-plan-hint">' + (planJump
        ? '点这张卡跳回那个对话并打开计划全文（右侧边栏「计划」，Markdown 渲染）：除输入框与三个按钮外，卡片任何位置都可以点'
        : '点这张卡看计划全文（右侧边栏「计划」，Markdown 渲染）：除输入框与三个按钮外，卡片任何位置都可以点') + '</p>';
    card.appendChild(body);
    card.appendChild(askFreeRow(q.id, '反馈修改意见后点「修改」，或直接批准', '修改',
      (v) => decideQuestion(q.id, v || '（未填写具体意见）')));
    const acts = document.createElement('div');
    acts.className = 'ap-actions';
    // 三个动作：修改（带反馈文本）、忽略计划（本轮就此停下、计划仍留着）、批准执行。
    const ig = document.createElement('button');
    ig.className = 'ghost';
    ig.textContent = '忽略计划';
    ig.title = '搁置这份计划：本轮对话到此结束（等同点「停止」），计划仍留在这里，随时可以再批准或修改';
    ig.onclick = (e) => { e.stopPropagation(); decideQuestion(q.id, '__ignore__'); };
    // 整张卡可点 → 打开这份计划（输入框与按钮自己拦下冒泡，不误触）
    card.classList.add('clickable');
    card.title = planJump ? '点击跳到那个对话并查看这份计划的全文' : '点击查看这份计划的全文';
    // 跳转要等 openSession 就位再开右栏：切换对话时 rpRestoreSession 会清掉
    // 一次性定位（planWantQid 等），先设会被切对话冲掉，落不到这份待审批的计划上。
    card.onclick = async () => {
      if (planJump) await openSession(planFrom);
      openPlanView(planFrom, 0, q.id);
    };
    const ok = document.createElement('button');
    ok.className = 'ok';
    ok.textContent = '批准并开始执行';
    ok.onclick = (e) => { e.stopPropagation(); decideQuestion(q.id, '__approve__'); };
    acts.appendChild(ig);
    acts.appendChild(ok);
    card.appendChild(acts);
    return card;
  }

  const body = document.createElement('div');
  body.className = 'ap-body';
  body.innerHTML = '<p>' + esc(q.question || '') + '</p>';
  card.appendChild(body);
  const opts = q.options || [];
  if (!isEnter && opts.length) {
    const box = document.createElement('div');
    box.className = 'q-opts';
    opts.forEach((o) => {
      const b = document.createElement('button');
      b.className = 'opt';
      b.textContent = o;
      b.onclick = () => decideQuestion(q.id, o);
      box.appendChild(b);
    });
    card.appendChild(box);
  }
  card.appendChild(askFreeRow(
    q.id,
    isEnter ? '可填写不同意见，留空直接拒绝' : (opts.length ? '或输入自定义回答' : '输入你的回答'),
    isEnter ? '拒绝' : '发送',
    (v) => {
      if (isEnter) { decideQuestion(q.id, v || '（未填写具体意见）'); return; }
      if (!v) return;
      decideQuestion(q.id, v);
    },
  ));
  if (isEnter) {
    const acts = document.createElement('div');
    acts.className = 'ap-actions';
    const ok = document.createElement('button');
    ok.className = 'ok';
    ok.textContent = '同意，进入计划模式';
    ok.onclick = (e) => { e.stopPropagation(); decideQuestion(q.id, '__approve__'); };
    acts.appendChild(ok);
    card.appendChild(acts);
  }
  return card;
}

// 一个视图的浮窗渲染：增/删/改都只动该动的那张卡，其余节点一个都不碰
function renderAskBar(view) {
  const bar = viewEl(view, 'ask-bar');
  if (!bar) return;
  const { own, list } = ownAskItems(view);
  const cards = askCards[view.key];
  const keep = new Set();
  const els = [];
  for (const e of list) {
    const key = askCardKey(e);
    keep.add(key);
    const sig = askSigOf(e, own);
    let cur = cards.get(key);
    if (!cur) {
      cur = { sig, el: e.type === 'approval' ? buildApprovalCard(e, own) : buildQuestionCard(e, own) };
      cards.set(key, cur);
    } else if (cur.sig !== sig) {
      const el = e.type === 'approval' ? buildApprovalCard(e, own) : buildQuestionCard(e, own);
      el.dataset.fresh = '1'; // 重建的卡：草稿在 askFreeRow 里回填，值不会丢
      cur.el.replaceWith(el);
      cur = { sig, el };
      cards.set(key, cur);
    }
    els.push(cur.el);
  }
  for (const [key, cur] of [...cards]) {
    if (keep.has(key)) continue;
    cur.el.remove();
    cards.delete(key);
  }
  els.forEach((el, i) => { if (bar.children[i] !== el) bar.insertBefore(el, bar.children[i] || null); });
  bar.classList.toggle('hidden', !els.length);
  if (els.length) tuckComposer(view, false); // 有待办时手机端不收起输入区（否则连卡一起滑出屏）
}

/* ---------- 右下角角标堆（别的对话的待办：攒在对话区右侧的留白里） ----------
   与输入区上方那块的分工：本对话的卡贴输入区「就地处理」，别的对话的卡一律攒到这里。

   尺寸与生灭都由「对话区右侧还剩多少留白」决定（对齐进度面板 pgMetrics 的 auto 模型）：
   · 留白够宽（≥240px）且纵向够高（≥220px）→ 自动展开，宽度取 min(300, 留白)，高度取
     min(560, 可用高度)，绝不吃到正文列或右侧栏；
   · 展开后内容本身塞不下（卡片总高超过可用高度）→ 自动缩成胶囊（点胶囊仍可展开看，
     展开态在堆内滚动）；用户手动点开过就不再自动收，直到卡片增删或他再次收起；
   · 留白不够展开 → 只留胶囊；连胶囊都放不下（右侧栏占光、留白 <96px）→ 整块不显示
     （左侧栏的橙点照旧标出哪个会话在等）。
   窄屏（手机）没有「右侧留白」可用，退回窗口右下角、按视口宽度收窄。 */

const DOCK_W = 300;    // 角标堆最大宽度（比对话区浮窗窄一档）
const DOCK_MIN = 240;  // 自动展开的门槛：右侧留白至少放得下这么宽
const DOCK_HMIN = 220; // 纵向可用高度低于它也不展开（缩成胶囊）
const DOCK_HMAX = 560; // 展开态高度上限（再多就在堆内滚动）
const DOCK_GAP = 12;   // 与对话区右缘、窗口底缘的间距（展开与胶囊共用同一锚点）

const askDock = { min: false, seen: 0, force: false }; // min=用户收了；seen=上次条数；force=用户手动点开过

// 角标堆能用多少空间：按 #main 与消息列的实际矩形算，右栏开合、拖把手改宽、
// 窗口缩放都会自然反映进来（它们都会改这两个矩形）。
function dockMetrics() {
  const main = $('main'), col = document.querySelector('#messages .msg-col');
  if (!main || !col) return null;
  const mainRect = main.getBoundingClientRect();
  if (isNarrow()) {
    // 手机端左右栏是整屏浮层：浮层开着时不铺开（免得盖在浮层上抢空间）
    const overlay = document.body.classList.contains('sb-show') || document.body.classList.contains('rp-open');
    const fitW = window.innerWidth - 2 * DOCK_GAP;
    const fitH = Math.max(0, window.innerHeight - mainRect.top - 2 * DOCK_GAP);
    return { right: DOCK_GAP, fitW, fitH, width: Math.round(Math.min(DOCK_W, fitW)),
      height: Math.round(Math.min(DOCK_HMAX, fitH)),
      auto: !overlay && fitW >= DOCK_MIN && fitH >= DOCK_HMIN, pill: !overlay && fitW >= 96 && fitH >= 36 };
  }
  const room = Math.max(0, mainRect.right - col.getBoundingClientRect().right); // 对话区右侧留白
  const fitW = room - DOCK_GAP;
  const fitH = Math.max(0, window.innerHeight - mainRect.top - DOCK_GAP * 2);
  return {
    right: Math.round(window.innerWidth - mainRect.right + DOCK_GAP),
    fitW: Math.round(fitW), fitH: Math.round(fitH),
    width: Math.round(Math.min(DOCK_W, fitW)), height: Math.round(Math.min(DOCK_HMAX, fitH)),
    auto: fitW >= DOCK_MIN && fitH >= DOCK_HMIN, pill: fitW >= 96 && fitH >= 36,
  };
}

function renderAskDock(list) {
  const dock = $('ask-dock');
  if (!dock) return;
  const box = $('ask-dock-cards');
  const pill = $('ask-dock-pill');
  const cards = askCards.dock;
  const keep = new Set();
  const els = [];
  for (const e of list) {
    const key = askCardKey(e);
    keep.add(key);
    const sig = askSigOf(e, ''); // 角标堆里的卡一律带「来自「某对话」」来源行
    let cur = cards.get(key);
    if (!cur) {
      cur = { sig, el: e.type === 'approval' ? buildApprovalCard(e, '') : buildQuestionCard(e, '') };
      cards.set(key, cur);
    } else if (cur.sig !== sig) {
      const el = e.type === 'approval' ? buildApprovalCard(e, '') : buildQuestionCard(e, '');
      cur.el.replaceWith(el);
      cur = { sig, el };
      cards.set(key, cur);
    }
    els.push(cur.el);
  }
  for (const [key, cur] of [...cards]) {
    if (keep.has(key)) continue;
    cur.el.remove();
    cards.delete(key);
  }
  els.forEach((el, i) => { if (box.children[i] !== el) box.insertBefore(el, box.children[i] || null); });

  // 卡片增删（seen 变化）= 情况变了：不再沿用「用户手动点开」的意愿，重新按空间自动定
  if (askDock.seen !== els.length) askDock.force = false;

  const mt = dockMetrics() || { right: DOCK_GAP, width: DOCK_W, height: DOCK_HMAX, auto: false, pill: false };
  dock.style.right = mt.right + 'px';
  pill.style.right = mt.right + 'px';
  if (pill.style.bottom) { // 窗口变矮：胶囊别被留在视口外（位置是用户拖出来记下的）
    const b = parseFloat(pill.style.bottom) || DOCK_GAP;
    pill.style.bottom = Math.max(DOCK_GAP, Math.min(b, window.innerHeight - (pill.offsetHeight || 36) - DOCK_GAP)) + 'px';
  }
  dock.style.width = mt.width + 'px';
  dock.style.height = '';          // 交给内容撑，上限用 maxHeight
  dock.style.maxHeight = mt.height + 'px';
  // 内容本身塞不下 → 自动缩成胶囊（用户手动点开过就尊重他，展开态在堆内滚动）
  const tooTall = !askDock.force && mt.auto && box.scrollHeight > mt.height + 2;
  const noRoom = !mt.auto;                        // 留白根本放不下：任何情况下都不铺开
  const folded = els.length > 0 && (askDock.min || noRoom || tooTall);
  const showPill = els.length > 0 && folded && mt.pill;
  dock.classList.toggle('hidden', !els.length || folded);
  pill.classList.toggle('hidden', !showPill);
  $('ask-dock-count').textContent = els.length ? els.length + ' 项' : '';
  $('ask-dock-pill-num').textContent = String(els.length);
  // 缩着的时候来了新的：胶囊上点一下「有新」的提示（不自动展开，别打断当前对话）
  if (askDock.seen < els.length && folded) pill.classList.add('has-new');
  if (!folded) pill.classList.remove('has-new');
  askDock.seen = els.length;
  askDock.lastW = mt.width;
  askDock.lastAuto = mt.auto;
  askDock.lastPill = mt.pill;
}

// 收起 / 展开（收起是用户的显式选择，记在内存里；展开则标记「手动点开过」）
function setAskDockMin(min) {
  askDock.min = !!min;
  askDock.force = !min;
  renderPanels();
}

// 计划卡上的「待审批」徽章随待办翻新：审批卡批了/驳了、或重启后重新拉回待审批
// 计划时，消息流里那张卡立刻跟上（不整列重渲染，阅读位置不跳）。
function refreshPlanCards() {
  document.querySelectorAll('.plan-card').forEach((card) => {
    const pendingId = planPendingIdOf(card._plan || '');
    card.classList.toggle('pending', !!pendingId);
    const head = card.querySelector('.pc-head');
    const badge = card.querySelector('.pc-badge');
    if (pendingId && !badge && head) {
      const b = document.createElement('span');
      b.className = 'pc-badge';
      b.textContent = '待审批';
      head.appendChild(b);
    } else if (!pendingId && badge) {
      badge.remove();
    }
    const tip = card.querySelector('.pc-acts');
    if (tip) tip.textContent = pendingId ? '等待你的审批 · 点击这张卡看计划全文' : '点击这张卡看计划全文';
  });
}

// 三处一起重画（主对话区 / 右栏子对话 / 右下角角标堆，加右栏「计划」视图联动）；
// 所有入口统一走它
function renderPanels() {
  refreshPlanCards(); // 计划卡上的「待审批」徽章随待办状态翻新
  renderAskBar(mainView);
  renderAskBar(subView);
  renderAskDock(otherAskItems());
  if (rpActive === 'plan') drawPlanView();
  loadSessions(); // 待办清单变了：侧栏的「⏸ 待确认」标记/橙点跟着翻新（自身带合并去抖）
}

// 角标堆的两个交互：整块收起成胶囊 / 胶囊点开（点开＝用户显式要展开）。
// 胶囊另可整枚上下拖动（横向不动＝一直吸附右侧）：手机端默认位置压着发送
// 按钮，拖上去即可。位置记 localStorage，刷新/重开保持；拖动超过 6px 视为
// 拖拽不算点击（pointerup 后浏览器还会派发 click，用标志位拦下）。
const PILL_BOTTOM_KEY = 'jx.askPillBottom';
const askPill = $('ask-dock-pill');
function askPillClamp(b) {
  const h = askPill.offsetHeight || 36;
  return Math.max(DOCK_GAP, Math.min(b, window.innerHeight - h - DOCK_GAP));
}
function askPillApply(b) { askPill.style.bottom = askPillClamp(b) + 'px'; }
{ // 启动时恢复上次拖到的位置（没拖过就维持 CSS 默认 bottom:12px）
  const v = parseFloat(lsGet(PILL_BOTTOM_KEY, '') || '');
  if (v > 0) askPillApply(v);
}
let askPillDrag = null, askPillClickOff = false;
askPill.addEventListener('pointerdown', (e) => {
  if (e.button !== undefined && e.button !== 0) return; // 只认主键/触摸/笔
  askPillDrag = { startY: e.clientY, startBottom: parseFloat(askPill.style.bottom) || DOCK_GAP, moved: false };
  try { askPill.setPointerCapture(e.pointerId); } catch (err) { /* 拿不到捕获也照拖 */ }
});
askPill.addEventListener('pointermove', (e) => {
  if (!askPillDrag) return;
  const dy = askPillDrag.startY - e.clientY; // 向上拖为正 → bottom 增大（胶囊上移）
  if (!askPillDrag.moved) {
    if (Math.abs(dy) < 6) return; // 抖动容差：没超过它仍算点击
    askPillDrag.moved = true;
    askPill.classList.add('dragging');
  }
  askPillApply(askPillDrag.startBottom + dy);
});
const askPillDragEnd = () => {
  if (!askPillDrag) return;
  if (askPillDrag.moved) {
    lsSet(PILL_BOTTOM_KEY, String(Math.round(parseFloat(askPill.style.bottom) || DOCK_GAP)));
    askPillClickOff = true; // 紧随 pointerup 的 click 是拖拽收尾，不是「点开」
    setTimeout(() => { askPillClickOff = false; }, 80);
  }
  askPillDrag = null;
  askPill.classList.remove('dragging');
};
askPill.addEventListener('pointerup', askPillDragEnd);
askPill.addEventListener('pointercancel', askPillDragEnd);
$('ask-dock-min').onclick = () => setAskDockMin(true);
askPill.onclick = () => {
  if (askPillClickOff) { askPillClickOff = false; return; }
  askDock.seen = 0;
  setAskDockMin(false);
};

// 可展空间随「对话列宽度 / 右栏开合 / 窗口缩放」变。右栏开合是有过渡动画的，过渡
// 途中算出来的留白是中间值（会误判成「放不下」），所以这里做防抖：等布局落定再算，
// 只有真正跨过门槛（能展开 ↔ 只能给胶囊 ↔ 放不下）或宽度变了才重画。
let dockSyncTimer = 0;
function syncAskDock() {
  clearTimeout(dockSyncTimer);
  dockSyncTimer = setTimeout(() => {
    const mt = dockMetrics();
    if (!mt) return;
    if (mt.width === askDock.lastW && mt.auto === askDock.lastAuto && mt.pill === askDock.lastPill) return;
    renderAskDock(otherAskItems());
  }, 150);
}
window.addEventListener('resize', syncAskDock);
if (window.ResizeObserver) {
  // 两个信号都要：#main 的宽度＝留白的来源（右栏开合/拖把手都会改它），
  // 消息列宽度＝内容列自己的宽度（拖把手改 --content-w 时只动它）
  const obs = new ResizeObserver(syncAskDock);
  obs.observe($('main'));
  const dockCol = document.querySelector('#messages .msg-col');
  if (dockCol) obs.observe(dockCol);
}

/* ---------- 右栏「计划」视图（exit_plan_mode 的正文：Markdown + 可反复打开） ----------
   文档来源两路合一：
   · 历史：GET /api/sessions/{id}/plans（后端从会话日志里读回 exit_plan_mode 的调用
     参数——计划与对话同寿命，刷新页面、切走再回来、重启进程后都还在）；
   · 待审批：/api/questions 里该会话 kind=plan 的提问，正文在这里就地批准/修改。
   计划全文不进输入区（那边只留摘要卡），所以表格/清单/代码块都能在右栏正常展开。 */

let planSid = '';      // 视图绑定的会话（默认跟随主对话；从别的对话的卡点进来则绑那个会话）
let planDocs = [];     // 历史文档（新的在后）
let planSel = '';      // 当前展示的文档 key（渲染结果，回填下拉）
let planPinned = '';   // 用户在下拉里显式选过的那份：优先显示，直到它消失或来了新计划
let planWantSeq = 0;   // 一次性定位：消息流里的计划卡被点击（按提交序号）
let planWantQid = '';  // 一次性定位：刚到达的待审批计划（按提问 id）
let planCur = null;    // 当前展示的文档（复制按钮用）
let planFetching = false;
const planTried = {};  // 计划提问 id → 已补拉过历史（只补一次）

function planKeyOf(d) { return d.pendingId ? 'q:' + d.pendingId : 'h:' + d.seq; }

// 打开计划视图（sid 省略 = 跟随主对话；给 seq/qid 就是「直接定位到某一份」）
// opts.silent = 本回合新来的计划自动铺开：不算「用户打开」，久等提醒照旧。
function openPlanView(sid, seq, qid, opts) {
  if (sid) planSid = sid;
  if (!planSid && state.cur) planSid = state.cur.id;
  // 一次性定位：点的是哪一份就看哪一份（右栏清单加载完自动选中，配不上就退回默认）
  if (seq) planWantSeq = seq;
  if (qid) planWantQid = qid;
  if (seq || qid) planPinned = ''; // 显式定位优先于「上次在下拉里选过的那份」
  if (!(opts && opts.silent)) notePlanHandled(planSid, qid); // 用户打开＝已在处理：停提醒计时
  rpOpen('plan');
}

// 只在「新出现的」待审批计划上把右栏切过去（首次打开、重启后拉回、本轮新提交）：
// 后续重复拉取不再打断用户在右栏里翻看旧版计划的选择。
const planSeenQids = {};
function noteFreshPlanQuestions() {
  let fresh = '';
  for (const q of state.questions || []) {
    if (q.kind !== 'plan' || planSeenQids[q.id]) continue;
    planSeenQids[q.id] = true;
    fresh = q.id;
  }
  if (fresh) { planWantQid = fresh; planPinned = ''; }
}

// 计划提问到达/本对话切换时自动打开一次（用户手动关掉后不再自动弹）
function maybeAutoOpenPlan() {
  const sid = (state.cur && state.cur.id) || '';
  if (!sid) return;
  if (isNarrow()) return; // 手机端右栏是整屏浮层：不自动铺开，卡上的「查看计划」随时可点
  const q = state.questions.find((x) => x.kind === 'plan' && x.sessionId === sid);
  if (!q || planAutoOpened[q.id]) return;
  planAutoOpened[q.id] = true;
  openPlanView(sid, 0, '', { silent: true });
}

// 提问/回答离场前先留一份正文副本：历史那边还没接上时右栏不会突然空掉
function stashPlanDoc(q) {
  const plan = q.plan || '';
  if (!plan) return;
  const head = plan.replace(/…$/, '');
  if (planDocs.some((d) => d.markdown === plan || d.markdown.startsWith(head))) return;
  planDocs.push({ seq: 0, title: planTitleOf(plan), markdown: plan, createdAt: '', truncated: true });
}

async function loadPlanView() {
  const sid = planSid || (state.cur && state.cur.id) || '';
  planSid = sid;
  if (!sid) { planDocs = []; drawPlanView(); return; }
  planFetching = true;
  try {
    const d = await api('/api/sessions/' + sid + '/plans');
    planDocs = (d.plans || []).map((p) => ({
      seq: p.seq, title: p.title || planTitleOf(p.markdown),
      markdown: p.markdown, createdAt: p.createdAt || '',
    }));
  } catch (e) {
    /* 服务离线/会话不存在：保留上次的清单（空清单时下面会给提示） */
  } finally { planFetching = false; }
  drawPlanView();
}

// 历史文档 + 待审批提问（同一份按正文前缀配对，配不上就以提问自带正文兜底）
function planDocsWithPending() {
  const sid = planSid || (state.cur && state.cur.id) || '';
  const out = planDocs.map((d) => ({ ...d }));
  for (const q of state.questions) {
    if (q.kind !== 'plan' || q.sessionId !== sid) continue;
    const head = (q.plan || '').replace(/…$/, '');
    const hit = out.find((d) => d.markdown === q.plan || d.markdown.startsWith(head));
    if (hit) hit.pendingId = q.id;
    else out.push({ seq: 0, title: planTitleOf(q.plan || ''), markdown: q.plan || '', pendingId: q.id, createdAt: '', truncated: true });
  }
  return out;
}

function drawPlanView() {
  const docEl = $('plan-doc');
  if (!docEl) return;
  const list = planDocsWithPending();
  // 选中规则（优先级从高到低）：
  //   1) 一次性定位：点了某张计划卡 / 本回合新到达的待审批计划；
  //   2) 用户在下拉里显式选过的那份（它消失、或又来了新计划就作废）；
  //   3) 最新的那份「待审批」计划——新计划提交后默认露出新的，不再停在上一版；
  //   4) 上次显示的那份；5) 最新一份。有「待审批」时永远优先看它。
  let cur = null;
  if (planWantSeq) { const hit = list.find((d) => d.seq === planWantSeq); if (hit) { cur = hit; planWantSeq = 0; } }
  if (!cur && planWantQid) { const hit = list.find((d) => d.pendingId === planWantQid); if (hit) { cur = hit; planWantQid = ''; } }
  if (!cur && planPinned) cur = list.find((d) => planKeyOf(d) === planPinned) || null;
  if (!cur) {
    const pend = list.filter((d) => d.pendingId);
    cur = pend.length ? pend[pend.length - 1] : null;
  }
  if (!cur) cur = list.find((d) => planKeyOf(d) === planSel) || null;
  if (!cur) cur = list[list.length - 1] || null;
  // （旧的「有待审批就直接跳过去」兜底已并进上面的规则 3）
  planSel = cur ? planKeyOf(cur) : '';
  planCur = cur || null;

  const pick = $('plan-pick');
  pick.innerHTML = '';
  list.forEach((d) => {
    const o = document.createElement('option');
    o.value = planKeyOf(d);
    o.textContent = (d.pendingId ? '待审批 · ' : '') + d.title +
      (d.createdAt ? '（' + String(d.createdAt).slice(5, 16) + '）' : '');
    pick.appendChild(o);
  });
  pick.value = planSel;
  pick.classList.toggle('hidden', list.length < 2);

  const pendingCount = list.filter((d) => d.pendingId).length;
  $('plan-status').textContent = !list.length ? ''
    : (pendingCount ? '待审批 ' + pendingCount + ' 份 · 共 ' + list.length + ' 份' : '共 ' + list.length + ' 份');

  if (!cur) {
    docEl.innerHTML = '<div class="plan-empty">本对话还没有提交过计划。<br>' +
      '计划模式下模型完成调研、调用 exit_plan_mode 提交后，计划正文就会出现在这里；' +
      '可以随时关掉再打开，刷新页面也还在。</div>';
  } else {
    docEl.innerHTML =
      '<div class="plan-meta"><b>' + esc(cur.title) + '</b>' +
      (cur.createdAt ? '<span>' + esc(cur.createdAt) + '</span>' : '') +
      (cur.pendingId ? '<span class="plan-badge">待审批</span>' : '') +
      (cur.truncated ? '<span class="plan-badge gray">摘要</span>' : '') +
      '</div><div class="md plan-md">' + md(cur.markdown) + '</div>';
  }

  const act = $('plan-act');
  if (cur && cur.pendingId) {
    act.classList.remove('hidden');
    $('plan-feedback').value = askDraftGet(cur.pendingId);
  } else {
    act.classList.add('hidden');
  }
  // 待审批的正文在历史里还没配对上（工具调用刚落库）：拉一次清单补齐全文，
  // 每份提问只补拉一次（planTried），免得历史里确实没有时来回打转
  const missing = list.find((d) => d.pendingId && d.truncated);
  if (missing && !planFetching && !planTried[missing.pendingId]) {
    planTried[missing.pendingId] = true;
    loadPlanView();
  }
}

// 右栏「计划」视图的控件（只绑一次；展示对象从 planCur 取）
$('plan-pick').addEventListener('change', (e) => {
  planPinned = e.target.value; planWantSeq = 0; planWantQid = ''; // 用户显式选择：优先显示它
  planNoteHide(); drawPlanView();
});
$('plan-copy').onclick = () => {
  if (!planCur) return;
  navigator.clipboard.writeText(planCur.markdown).catch(() => {});
  $('plan-status').textContent = '已复制计划全文';
};
$('plan-reload').onclick = () => loadPlanView();

// 导出：把当前这份计划写成工作区里的 .md（后端不覆盖同名，自动加 -2、-3）
function planNoteShow(text, dir, bad) {
  const note = $('plan-note');
  $('plan-note-text').textContent = text;
  note.dataset.dir = dir || '';
  $('plan-note-open').classList.toggle('hidden', !dir);
  note.classList.toggle('bad', !!bad);
  note.classList.remove('hidden');
}
function planNoteHide() {
  const note = $('plan-note');
  if (!note) return;
  note.classList.add('hidden');
  note.dataset.dir = '';
}
$('plan-export').onclick = async () => {
  if (!planCur) return;
  const btn = $('plan-export');
  const sid = planSid || (state.cur && state.cur.id) || '';
  if (!sid) return;
  btn.disabled = true;
  try {
    const d = await api('/api/sessions/' + sid + '/plans/export',
      { method: 'POST', body: JSON.stringify({ seq: planCur.seq || 0 }) });
    planNoteShow('已导出：' + (d.path || d.name || ''), d.dir || '', false);
  } catch (e) {
    planNoteShow('导出失败：' + e.message, '', true);
  } finally {
    btn.disabled = false;
  }
};
$('plan-note-open').onclick = async () => {
  const dir = $('plan-note').dataset.dir || '';
  if (!dir) return;
  try {
    await api('/api/open-folder', { method: 'POST', body: JSON.stringify({ path: dir }) });
  } catch (e) {
    planNoteShow('打开文件夹失败：' + e.message, dir, true);
  }
};

$('plan-feedback').addEventListener('input', (e) => {
  if (planCur && planCur.pendingId) askDraftSet(planCur.pendingId, e.target.value);
});
$('plan-ignore').onclick = () => { if (planCur && planCur.pendingId) decideQuestion(planCur.pendingId, '__ignore__'); };
$('plan-approve').onclick = () => { if (planCur && planCur.pendingId) decideQuestion(planCur.pendingId, '__approve__'); };
$('plan-reject').onclick = () => {
  if (!planCur || !planCur.pendingId) return;
  const v = $('plan-feedback').value.trim();
  decideQuestion(planCur.pendingId, v || '（未填写具体意见）');
};
rpRegister('plan', {
  title: '计划', order: 2,
  onOpen: () => { if (!planSid && state.cur) planSid = state.cur.id; loadPlanView(); },
  // 会话态：视图绑定的会话（可能是子对话——从它的计划卡点开）+ 用户选中的那份
  saveSession: () => ({ sid: planSid, sel: planSel, pinned: planPinned }),
  restoreSession: (d) => {
    planSid = (d && d.sid) || '';
    planSel = (d && d.sel) || '';
    planPinned = (d && d.pinned) || '';
    planWantSeq = 0; planWantQid = ''; // 一次性定位是跳转意图，不跨对话
    // 数据不在这里拉：面板开着时 rpOpen('plan') 的 onOpen 会 loadPlanView；
    // 收着时等用户点开页签再拉（旧对话的正文不会被画出来）
  },
});

/* ---------- 进度面板（progress_write）：对话区右上角的一枚浮窗，可折叠 ---------- */
// 定位只有一个锚点：对话区（#main）的右上角。折叠＝一枚「进度」胶囊，展开＝从同一个
// 右上角往左下长出来的卡片——右缘与顶边都不动，所以点开是「原地展开」不跳位。
// 宽度按对话区右侧的留白算（见 pgMetrics）：留白放得下就塞进留白里，不遮正文；留白
// 不够说明是窄屏/中等窗口，那就用一张舒适宽度的卡（用户自己点开的，遮一点也能接受）。
// 用户没点过时（pgFold=null）只在留白够的时候自动展开，否则一律折叠成胶囊；点过之后
// 按用户的选择来（本次页面有效，刷新复位）。面板永远是浮层：不占对话区高度，出现与
// 折叠切换都不触发对话区重排。
const PG_W = 340;    // 卡片宽度上限（内容自适应：短清单更窄，见 #progress-panel 的 fit-content）
const PG_MIN = 220;  // 自动展开的门槛：右侧留白至少放得下这么宽
const PG_GAP = 10;   // 面板与对话区右缘的间距（胶囊与卡片共用，保证同一锚点）
const PG_PAD = 24;   // 内容列自身的内边距：留白不够时可以借它，不算遮正文

let pgItems = [];    // 最近一次取到的进度项（空 = 没有进度项）
let pgRuns = [];     // 本会话「运行中的程序」（在跑的子进程）；两者都空才整体隐藏
let pgFold = null;   // 用户显式选择：null=自动；true=展开；false=收起
let pgDrawn = null;  // 上次画出来的展开态（留白变化时只在翻转时才重画）

// 右侧留白 → 卡片宽度与「够不够自动展开」。按 #main 与消息列的实际矩形算，
// 开关左右栏、拖拽调宽对话列都会自然反映进来（它们都会改变这个留白）。
function pgMetrics() {
  const main = $('main'), col = document.querySelector('#messages .msg-col');
  if (!main || !col) return null;
  const mainRect = main.getBoundingClientRect();
  const room = Math.max(0, mainRect.right - col.getBoundingClientRect().right);
  const fit = room + PG_PAD - PG_GAP; // 塞进留白，顺手借用内容列自己的右内边距
  return {
    width: Math.round(Math.min(PG_W, fit >= PG_MIN ? fit : mainRect.width - 2 * PG_GAP)),
    auto: fit >= PG_MIN,
  };
}
function pgExpanded(mt) { return pgFold === null ? mt.auto : pgFold; }

async function loadProgress() {
  const panel = $('progress-panel');
  if (!panel) return;
  if (!state.cur) { pgItems = []; pgRuns = []; pgFold = null; drawProgress(); return; }
  try {
    const d = await api('/api/sessions/' + state.cur.id + '/progress');
    pgItems = d.progress || [];
    pgRuns = d.running || [];
  } catch (e) { /* 会话不存在/服务离线：维持上一次的样子 */ }
  drawProgress();
}

// 运行时长文案：88s / 3m20s / 1h02m
function fmtElapsed(sec) {
  sec = Math.max(0, Math.round(sec || 0));
  if (sec < 60) return sec + 's';
  const m = Math.floor(sec / 60), s = sec % 60;
  if (m < 60) return m + 'm' + (s < 10 ? '0' + s : s) + 's';
  return Math.floor(m / 60) + 'h' + (m % 60 < 10 ? '0' + (m % 60) : m % 60) + 'm';
}

// 还有在跑的程序时每 5s 刷一次时长（起/停由 SSE 的 children 事件即时刷）
let pgRunTick = null;
function scheduleRunTick() {
  if (pgRunTick) { clearTimeout(pgRunTick); pgRunTick = null; }
  // 只要有登记项（在跑，或刚结束还没被服务端清掉的）就继续刷，直到列表空掉为止
  if (!state.cur || !pgRuns.length) return;
  pgRunTick = setTimeout(() => { pgRunTick = null; loadProgress(); }, 5000);
}

// 按当前折叠状态重画面板：折叠＝胶囊，展开＝头部（整行是收起按钮）+ 清单。
// 两态共用同一个右上角锚点，宽度写进 --pg-w 交给 CSS，所以展开就是在原地长出来。
function drawProgress() {
  const panel = $('progress-panel');
  if (!panel) return;
  const items = pgItems, runs = pgRuns;
  if (!items.length && !runs.length) { panel.classList.add('hidden'); panel.innerHTML = ''; pgDrawn = null; return; }
  const mt = pgMetrics() || { width: PG_W, auto: false };
  const open = pgExpanded(mt);
  pgDrawn = open;
  panel.style.setProperty('--pg-w', mt.width + 'px');
  panel.classList.remove('hidden');
  panel.classList.toggle('pg-folded', !open);
  const running = runs.filter((r) => r.state === 'running').length;
  if (!open) {
    panel.innerHTML = '<button type="button" class="pg-pill" title="展开进度清单">进度'
      + (running ? '<span class="pg-run-dot"></span>' + running + ' 个在跑' : '')
      + chev('pg-chev') + '</button>';
    panel.querySelector('.pg-pill').onclick = () => { pgFold = true; drawProgress(); };
    return;
  }
  const done = items.filter((t) => t.status === 'completed').length;
  let html = '<button type="button" class="progress-head" title="收起进度清单">'
    + '<b>进度</b><span class="pg-count">' + done + '/' + items.length + '</span>'
    + '<svg class="pg-chev" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 15l6-6 6 6"/></svg>'
    + '</button>';
  if (items.length) {
    html += '<ul>';
    for (const t of items) {
      const mark = t.status === 'completed' ? '✔' : (t.status === 'in_progress' ? '▶' : '·');
      html += '<li class="pg-' + (t.status || 'pending') + '"><span class="pg-mark">' + mark + '</span>' + esc(t.content) + '</li>';
    }
    html += '</ul>';
  }
  if (runs.length) {
    // 横线隔开：上面是进度清单，下面专放本会话「运行中的程序」——短命令会一闪而过，
    // 一直后台跑的（后台作业、长命令）会一直留在这里，看得见它在跑多久。
    html += '<div class="pg-sep"></div>'
      + '<div class="pg-runs-head">运行中的程序<span class="pg-runs-n">' + (running ? running + ' 个在跑' : '刚结束') + '</span></div>'
      + '<ul class="pg-runs">';
    for (const r of runs) {
      const live = r.state === 'running';
      const cmd = String(r.command || '');
      // 停止＝两个红字（无框无背景）：锚在时长格右缘、行尾固定位置，悬停该行才
      // 现身，时长文字（.pg-run-elapsed）同时让位隐藏；按钮绝不向外扩，不会把
      // 列表的横向滚动条顶出来（见 .pg-run-stop 的 right:0 定位）
      html += '<li class="pg-run' + (live ? ' live' : '') + '">'
        + '<span class="pg-run-label">' + esc(r.label || '子进程') + '</span>'
        + '<span class="pg-run-cmd" title="' + esc(cmd) + '">' + esc(cmd.length > 70 ? cmd.slice(0, 70) + '…' : cmd) + '</span>'
        + '<span class="pg-run-time"><span class="pg-run-elapsed">' + (live ? '已跑 ' + fmtElapsed(r.elapsed) : esc(r.exit || '已结束')) + '</span>'
        + (live ? '<button type="button" class="pg-run-stop" data-id="' + esc(r.id) + '" title="停止这个程序（会通知 AI：这是用户停止的，不是出错）">停止</button>' : '')
        + '</span>'
        + '</li>';
    }
    html += '</ul>';
  }
  panel.innerHTML = html;
  panel.querySelector('.progress-head').onclick = () => { pgFold = false; drawProgress(); };
  // 运行中的程序：停止按钮 → 真正终止（后台作业优雅取消、子进程强杀），后端
  // 会向本会话投【用户停止程序】注记（不触发 AI 对话）；列表由 children 事件刷新，
  // 这里的 loadProgress 只是成功后的即时反馈
  for (const b of panel.querySelectorAll('.pg-run-stop')) {
    b.onclick = () => {
      b.disabled = true;
      if (!state.cur) return;
      api('/api/sessions/' + state.cur.id + '/stop-child', { method: 'POST', body: JSON.stringify({ id: b.dataset.id }) })
        .then(() => loadProgress())
        .catch((e) => { b.disabled = false; alert('停止失败：' + e.message); });
    };
  }
  scheduleRunTick();
}

// 留白变了（缩放窗口、开关左右栏、拖拽调宽对话列）：宽度跟着变；自动档下展开态翻转
// 才重画，其余情况只改宽度不动 DOM
function syncProgress() {
  if (!pgItems.length && !pgRuns.length) return;
  const mt = pgMetrics();
  if (!mt) return;
  const panel = $('progress-panel');
  if (panel && !panel.classList.contains('hidden')) panel.style.setProperty('--pg-w', mt.width + 'px');
  if (pgExpanded(mt) !== pgDrawn) drawProgress();
}
window.addEventListener('resize', syncProgress);
// 对话列宽度会单独变（拖把手、右栏开合），盯它的尺寸就够了
if (window.ResizeObserver) {
  const pgCol = document.querySelector('#messages .msg-col');
  if (pgCol) new ResizeObserver(syncProgress).observe(pgCol);
}

/* ---------- SSH 远程工作区（两步式：连接配置 → 保存 → 浏览目录 → 使用） ---------- */

let rmSavedSpec = null; // 第一步「保存」的连接配置（本轮流程内有效）
let rmCurPath = '';     // 目录浏览器当前所在路径（即选中项）
let rmHomePath = '/';   // 远程用户主目录（后端探测）

function remoteFormPrefill() {
  // 优先级：当前会话 > 首页草稿 > 本机记住的连接配置（密码不落盘）
  const saved = lsGet('jx.rmDraft', null) || {};
  const r = (state.cur && state.cur.remote) || homeDraft.remote || saved;
  rmSavedSpec = null;
  $('rm-host').value = r.host || '';
  $('rm-port').value = r.port && r.port !== 22 ? r.port : '';
  $('rm-user').value = r.user || '';
  $('rm-auth').value = r.auth || 'key';
  $('rm-key').value = r.keyPath || '';
  $('rm-pass').value = r.password || '';
  $('rm-dir').value = (state.cur && state.cur.remote && state.cur.remote.dir) || r.dir || '';
  $('rm-result').textContent = '';
  $('rm-result').className = 'remote-result';
  $('rm-ls-status').textContent = '';
  $('rm-ls-status').className = 'remote-result';
  $('rm-clear').style.display = ((state.cur && state.cur.remote) || homeDraft.remote) ? '' : 'none';
  syncRemoteFormVis();
  showRmStep(1);
}

function showRmStep(n) {
  $('rm-step1').classList.toggle('hidden', n !== 1);
  $('rm-step2').classList.toggle('hidden', n !== 2);
  $('rm-title').textContent = n === 1 ? '添加远程工作目录 · 连接配置' : '选择远程工作目录';
  // 两步内容高度不同，弹窗重新对齐锚点（只重摆位置，不做开合切换）
  if (!$('remote-pop').classList.contains('hidden')) {
    const anchor = $('remote-badge').classList.contains('hidden') ? $('btn-plus') : $('remote-badge');
    reposPopover('remote-pop', anchor);
  }
  if (n === 2) browseRemote($('rm-dir').value.trim());
}

function syncRemoteFormVis() {
  const key = $('rm-auth').value === 'key';
  $('rm-key').style.display = key ? '' : 'none';
  $('rm-pass').style.display = key ? 'none' : '';
}

function collectRemoteSpec() {
  return {
    host: $('rm-host').value.trim(),
    port: parseInt($('rm-port').value, 10) || 22,
    user: $('rm-user').value.trim(),
    auth: $('rm-auth').value,
    keyPath: $('rm-key').value.trim(),
    password: $('rm-pass').value,
    dir: $('rm-dir').value.trim(),
  };
}

function validateRemoteSpec(spec) {
  if (!spec.host) return '请先填写主机';
  if (spec.auth === 'key' && !spec.keyPath) return '私钥文件路径必填（或改用密码认证）';
  if (spec.auth === 'password' && !spec.password) return '密码必填';
  return '';
}

function showRemoteResult(ok, msg, hostKey) {
  const el = $('rm-result');
  el.className = 'remote-result ' + (ok ? 'ok' : 'bad');
  el.textContent = msg + (ok && hostKey ? '｜主机指纹 ' + hostKey : '');
}

function showRmLs(msg, bad) {
  const el = $('rm-ls-status');
  el.className = 'remote-result' + (bad ? ' bad' : '');
  el.textContent = msg;
}

// saveRemote 第一步「保存」：校验并记住连接配置，进入目录选择。
function saveRemote() {
  const spec = collectRemoteSpec();
  const bad = validateRemoteSpec(spec);
  if (bad) { showRemoteResult(false, bad); return; }
  rmSavedSpec = spec;
  lsSet('jx.rmDraft', { ...spec, password: '' }); // 本机记住连接配置（密码不落盘）
  showRemoteResult(true, '已保存连接配置，正在连接 ' + spec.host + ' …');
  showRmStep(2);
}

async function testRemote() {
  const spec = collectRemoteSpec();
  if (!spec.host) { showRemoteResult(false, '请先填写主机'); return; }
  const btn = $('rm-test');
  btn.disabled = true; btn.textContent = '测试中…';
  try {
    const d = await api('/api/workspace/test', { method: 'POST', body: JSON.stringify({ remote: spec }) });
    showRemoteResult(!!d.ok, d.message || (d.ok ? '连接成功' : '失败'), d.hostKey);
  } catch (e) {
    showRemoteResult(false, e.message);
  }
  btn.disabled = false; btn.textContent = '测试连接';
}

// browseRemote 列出远程目录；path 为空 = 回到用户主目录。
async function browseRemote(path) {
  const spec = rmSavedSpec || collectRemoteSpec();
  const bad = validateRemoteSpec(spec);
  if (bad) { showRmStep(1); showRemoteResult(false, bad); return; }
  showRmLs('连接中…');
  const list = $('rm-dir-list');
  list.innerHTML = '<div class="rm-empty">加载中…</div>';
  try {
    const d = await api('/api/workspace/ls', { method: 'POST', body: JSON.stringify({ remote: spec, path: path || '' }) });
    if (!d.ok) {
      list.innerHTML = '';
      showRmLs(d.message || '读取目录失败', true);
      return;
    }
    rmCurPath = d.path;
    rmHomePath = d.home || '/';
    $('rm-dir').value = d.path;
    showRmLs('');
    renderDirList(d.entries || []);
  } catch (e) {
    list.innerHTML = '';
    showRmLs('连接失败：' + e.message, true);
  }
}

function renderDirList(entries) {
  const list = $('rm-dir-list');
  list.innerHTML = '';
  const byName = (a, b) => a.name.localeCompare(b.name, 'zh-Hans-CN');
  const dirs = entries.filter((e) => e.isDir).sort(byName);
  const files = entries.filter((e) => !e.isDir).sort(byName);
  const folderSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>';
  const fileSvg = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M14 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V8z"/><path d="M14 2v6h6"/></svg>';
  const addRow = (e) => {
    const row = document.createElement('div');
    row.className = 'rm-entry' + (e.isDir ? '' : ' file');
    row.innerHTML = (e.isDir ? folderSvg : fileSvg) + '<span>' + esc(e.name) + (e.isDir ? '/' : '') + '</span>';
    row.dataset.path = rmCurPath === '/' ? '/' + e.name : rmCurPath + '/' + e.name;
    list.appendChild(row);
  };
  dirs.forEach(addRow);
  files.forEach(addRow);
  if (!entries.length) list.innerHTML = '<div class="rm-empty">（空目录）</div>';
  list.scrollTop = 0;
}

// applyRemote 第二步「使用」：以当前所在目录为工作区落库/入草稿，
// 并登记为可复选的远程工作区（工作目录面板里一键复挂）。
async function applyRemote() {
  const spec = rmSavedSpec || collectRemoteSpec();
  const bad = validateRemoteSpec(spec);
  if (bad) { showRmStep(1); showRemoteResult(false, bad); return; }
  const dir = ($('rm-dir').value || '').trim() || rmCurPath;
  if (!dir.startsWith('/')) { showRmLs('请先在上方选择或输入一个远程目录', true); return; }
  const full = { ...spec, dir };
  if (state.cur) {
    try {
      state.cur = await api('/api/sessions/' + state.cur.id + '/remote', { method: 'POST', body: JSON.stringify({ remote: full }) });
    } catch (e) { showRmLs('保存失败：' + e.message, true); return; }
  } else {
    homeDraft.remote = full;
  }
  registerRemoteWs(full); // 后端登记（密码 DPAPI 落库，清单不回密码）
  closePopovers();
  syncComposer();
  loadSessions();
}

// registerRemoteWs 把使用过的远程工作区登记到后端（失败静默：登记只是
// 复挂便利，不该打断「使用」成功的流程），成功后刷新面板清单缓存。
function registerRemoteWs(spec) {
  api('/api/remotes', { method: 'POST', body: JSON.stringify({ remote: spec }) })
    .then(() => loadRmList())
    .catch(() => {});
}

// loadRmList 拉取已登记的远程工作区清单（不回密码）。ws-pop 开着时顺手重渲。
function loadRmList() {
  return api('/api/remotes').then((d) => {
    state.rmList = d.remotes || [];
    if (!$('ws-pop').classList.contains('hidden')) renderWsPop();
  }).catch(() => { state.rmList = state.rmList || []; });
}

async function clearRemote() {
  if (state.cur) {
    try {
      state.cur = await api('/api/sessions/' + state.cur.id + '/remote', { method: 'POST', body: JSON.stringify({ clear: true }) });
    } catch (e) { alert('解除失败：' + e.message); return; }
  } else {
    homeDraft.remote = null;
  }
  lsSet('jx.rmDraft', null);
  closePopovers();
  syncComposer();
  loadSessions();
}

/* ---------- 发送与事件流 ---------- */

// 占位符「执行中」段的开关（setStreamUI 与 prepareSend 的提前亮起共用）：
// 追加/收回都幂等。完整占位符三段＝发送快捷键 + 语音组件的语音提示 + 本段
// （仅执行中）。前两段整体存 dataset 作存货：闲时文案可能中途被改（设置页换
// 发送键 / 语音组件刷新），updateSendKeyHint 会同步更新存货，恢复时不至于拿
// 回旧文案。
function runHintOn(view) {
  const ph = view.input;
  if (!ph) return;
  if (!ph.dataset.runPh) { ph.dataset.idlePh = ph.placeholder; ph.dataset.runPh = '1'; }
  ph.placeholder = ph.dataset.idlePh + RUN_HINT_SEGMENT;
}
function runHintOff(view) {
  const ph = view.input;
  if (!ph || !ph.dataset.runPh) return;
  ph.placeholder = ph.dataset.idlePh || ph.placeholder;
  delete ph.dataset.runPh; delete ph.dataset.idlePh;
}

function setStreamUI(on, view) {
  view = view || mainView;
  // 发送按钮保持可用：执行中发送 = 插话或排队（常规页「对话中途发送行为」）
  view.stopBtn.classList.toggle('hidden', !on);
  if (!on) view.hint.textContent = '';
  if (on) runHintOn(view); else runHintOff(view);
  // 上下文明细卡里的「压缩」跟着执行态走：执行中置灰并写明原因（后端也会 409，
  // 前端先拦更友好）。有数据才重画，别把「当前没有会话」的占位盖到已有明细上
  if (view.state.cur && lastCtx[view.key]) renderCtxTip(view);
}

function handleEvent(ev, view) {
  view = view || mainView;
  switch (ev.type) {
    case 'edited': {
      // 编辑重发的截断确认：服务端已删掉 seq 之后的记录并改写正文，
      // 本地对齐后模型续跑的输出会接着这条消息往下走；该点之后的
      // 文件快照与交付登记也已被服务端一并清掉
      view.state.msgs = view.state.msgs.filter((x) => x.seq <= ev.seq);
      view.state.msgs.forEach((x) => { if (x.seq === ev.seq) x.content = ev.delta; });
      if (view.state.checkpoints) view.state.checkpoints = view.state.checkpoints.filter((c) => c.seq <= ev.seq);
      if (view.state.deliveries) view.state.deliveries = view.state.deliveries.filter((d) => d.seq <= ev.seq);
      renderMessages(view.state.msgs, view);
      break;
    }
    case 'message': {
      // 环境快照行（运行中改权限等 PATCH 落的 env）可能在模型流式输出中途到达：
      // 只入列重渲染，不清半截正文——收尾动作只属于会关闭当前步的消息
      if (ev.kind !== 'env') {
        view.state.streamText = '';
        view.state.streamThinking = '';
        removeLive(view);
      }
      // 用户消息回显落地：「发送中」占位功成身退（助手/工具事件不会先于它到达）
      if (view.state.pendingSend && ev.message && ev.message.role === 'user') {
        view.state.pendingSend = null;
      }
      // 插话落库兜底：正式消息到达即摘掉同内容的虚线插话框。实框走本流
      // （message 事件），虚框清理靠全局总线的 pending 广播——两条通道独立，
      // 广播在断线重连窗口/订阅者跟不上被丢帧时，实框已现、虚框残留成双框
      // （插话打断模型流的瞬间事件最密集，最易撞上）。按内容本地摘除，
      // pending 广播正常到达时快照整体覆盖，两边不冲突。
      if (ev.message && ev.message.role === 'user' && typeof ev.message.content === 'string'
        && (view.state.pendingSteer || []).some((p) => p.content === ev.message.content)) {
        view.state.pendingSteer = view.state.pendingSteer.filter((p) => p.content !== ev.message.content);
      }
      if (view.state.attached) {
        // 跟随别的连接发起的执行：以服务端为准整体重取，避免与已落库内容重复
        //（节流合并：汇报轮里几十条 message 事件连发，不再每条整包拉一次）
        reloadMessagesSoon(view);
        break;
      }
      // 后端 message 事件带该步思考原文与消息 seq：
      // 思考块实时渲染不消失（工具调用落地后仍在），seq 用作回合标识与搜索定位
      if (ev.thinking) ev.message.thinking = ev.thinking;
      if (ev.seq) ev.message.seq = ev.seq;
      if (ev.kind) ev.message.kind = ev.kind; // 行类型：与存储 kind 一致（契约见《消息类型规范.md》）
      if (ev.origin) ev.message.origin = ev.origin;
      // 新回合开始（用户消息回显）：关注点回到尾部，顺手放掉上方已加载批次
      // （贴底跟随态才生效，钉住阅读不动）。赶在下一条 push 触发前缀判定之前：
      // 释放后索引同步平移，渲染仍走尾部重建快路径，批记账不会作废
      if (ev.message && ev.message.role === 'user') releaseOlder(view);
      view.state.msgs.push(ev.message);
      renderMessages(view.state.msgs, view);
      break;
    }
    case 'resume': // 接回执行流时补发的、进行中的那半句
      if (ev.delta) view.state.streamText = ev.delta;
      if (ev.thinking) view.state.streamThinking = ev.thinking;
      liveRender(view);
      break;
    case 'pending': // 插话/排队队列变化（自己发的、别的窗口发的、服务端排水的都在这里）
      applyPending(view, ev.pending);
      break;
    case 'stopped':
      view.hint.textContent = ev.error || '已停止';
      // 停止后队列保留不自动续：横幅给「继续发送」入口（队列空则无事发生）
      view.state.queueHeld = (view.state.pendingQueue || []).length > 0;
      if (view.state.attached) reloadMessages(view);
      JX.emit('chat:stream-end', { view: view.key, reason: 'stopped' });
      break;
    case 'delta':
      view.state.streamText += ev.delta;
      liveRender(view);
      if (view === mainView && state.cur && state.cur.mode === 'assistant') setFace('thinking');
      // 吐字通知（组件用：输入框下方的实时速度条。没人监听时零开销）
      JX.emit('chat:stream', { view: view.key, text: ev.delta });
      break;
    case 'reasoning': // 思考增量：实时显示在折叠块里
      view.state.streamThinking = (view.state.streamThinking || '') + ev.delta;
      liveRender(view);
      if (view === mainView && state.cur && state.cur.mode === 'assistant') setFace('thinking');
      JX.emit('chat:stream', { view: view.key, text: ev.delta });
      break;
    case 'usage':
      // 本步流尾的真实用量（后端 Event{Type:"usage"}）：输入框下方的速度条用它
      // 把估算值定格成真实值。不属于对话内容，不渲染、不落库
      JX.emit('chat:usage', { view: view.key, data: ev.data || {} });
      break;
    case 'tool_start':
      break;
    case 'tool_wait':
      view.hint.textContent = ev.output || '等待用户批准…';
      break;
    case 'tool_end':
      // deliver_result 一落库就刷新会话轻量状态：交付卡片当轮实时出现
      //（deliveries/checkpoints 只在打开会话时拉取过，不刷就永远不显示；
      // 节流合并，与 attached 的 message 重取共用队列节奏）
      if (ev.name === 'deliver_result' && !view.state.attached) reloadMessagesSoon(view);
      break;
    case 'sub_start':
      // 主对话 SSE 里的派发确认；后续进度统一走全局事件流
      subActs[ev.sessionId] = [];
      bindOrphanCard(ev.sessionId);
      $('hint').textContent = `子任务「${ev.title}」已在后台执行，完成后自动回报…`;
      loadSessions();
      break;
    case 'sub_end':
      $('hint').textContent = '';
      break;
    case 'title':
      // 首条消息自动起标题（服务端只在会话标题为空时才发）；跟随当前会话更新顶栏
      if (view === mainView && state.cur && ev.sessionId === state.cur.id) {
        applySessTitle({ mode: state.cur.mode, parentId: state.cur.parentId, title: ev.title });
      }
      break;
    case 'error':
      // 真实失败：收掉流式、把原因显示到输入框上方的报错条（不进对话区）
      view.state.streamText = '';
      view.state.streamThinking = '';
      removeLive(view);
      showRunError(view, ev.error);
      view.state.queueHeld = (view.state.pendingQueue || []).length > 0; // 出错后队列保留
      JX.emit('chat:stream-end', { view: view.key, reason: 'error' });
      break;
    case 'retry':
      // 重试即将开始（人工点重试 / 网络失败自动重试）：本步已流出的半截作废——
      // 服务端会从头重新输出，本地同步清干净并收掉报错条（observe 也已清掉
      // 重连补发的半截缓冲）
      hideRunError(view);
      view.state.streamText = '';
      view.state.streamThinking = '';
      removeLive(view);
      break;
    case 'auto_retry':
      // 自动重试提示条：output 非空=显示（几秒后第 X/5 次重试）；空=重试成功，清除
      view.hint.textContent = ev.output || '';
      break;
    case 'done':
      view.hint.textContent = '';
      // 正常收尾但队列仍有保留 = 暂停生效（排水循环被暂停门挡下）：横幅走暂停分支
      view.state.queueHeld = (view.state.pendingQueue || []).length > 0;
      if (view === mainView && state.cur && state.cur.mode === 'assistant') setFace('done', 4000);
      JX.emit('chat:stream-end', { view: view.key, reason: 'done' });
      break;
  }
}

/* ---------- 执行中的发消息：插话（贴到对话末尾，下一次调用模型生效）
   与排队（进输入框上方排队坞，本轮结束后自动作为独立一轮发送）。
   两边的队列都在服务端（runtime.go 的 PendingMsg）：刷新页面/换窗口不丢，
   前端只负责呈现与增删。 ---------- */

function pendingHint(view, text) {
  view.hint.textContent = text;
  setTimeout(() => { if (view.hint.textContent === text) view.hint.textContent = ''; }, 6000);
}

/* ---------- 报错条：模型/网络等真实失败显示在输入框上方（不属于对话区），
   带「重试」按钮（重试不再重复落库用户消息，后台直接带着当前上下文再跑一轮）。
   正常完成与用户手动停止都不产生任何内容。 ---------- */

function showRunError(view, text) {
  view = view || mainView;
  const box = viewEl(view, 'run-error');
  if (!box) return;
  if (!text) { hideRunError(view); return; }
  if (view.state.errDismissed === text) return; // 这条已被用户手动关掉
  view.state.errText = text;
  box.innerHTML = '';
  box.classList.remove('hidden');
  const icon = document.createElement('span');
  icon.className = 're-icon';
  icon.textContent = '⚠';
  const msg = document.createElement('span');
  msg.className = 're-text';
  msg.textContent = text;
  const retry = document.createElement('button');
  retry.className = 're-btn re-retry';
  retry.type = 'button';
  retry.textContent = '重试';
  retry.onclick = () => retryRun(view);
  // 反馈：把这条报错一键带进 设置→关于 的反馈表单（远程用户的主报修通道）
  const fb = document.createElement('button');
  fb.className = 're-btn re-retry';
  fb.type = 'button';
  fb.textContent = '反馈';
  fb.onclick = () => {
    const sid = (view.state.cur && view.state.cur.id) || '';
    openFeedbackForm('【运行报错】' + text + (sid ? '\n（会话 ' + sid + '）' : ''), sid);
  };
  const close = document.createElement('button');
  close.className = 're-btn re-close';
  close.type = 'button';
  close.title = '关闭';
  close.textContent = '✕';
  close.onclick = () => hideRunError(view, true);
  box.appendChild(icon);
  box.appendChild(msg);
  box.appendChild(retry);
  box.appendChild(close);
}

// hideRunError 收掉报错条；remember=true 表示「用户手动关闭」（同一条不再自动弹回）
function hideRunError(view, remember) {
  view = view || mainView;
  const box = viewEl(view, 'run-error');
  if (remember) view.state.errDismissed = view.state.errText || '';
  view.state.errText = '';
  if (!box) return;
  box.classList.add('hidden');
  box.innerHTML = '';
}

/* ---------- 更新横幅（服务端推送通知的统一出口） ---------- */

// showUpdateBanner 更新横幅（双模式，置顶常驻）：
// ① 整包路：新版本可用，「确认更新」直接下载安装包；
// ② 就绪路（u.pendingRestart）：主程序已静默交换，「立即重启」经助手接力生效，
//    「稍后」等下次自然重启。在线时由 update_available/update_ready 门铃事件
//    触发，刚打开页面时从 /api/status 的 update 恢复。force=true 为用户主动
//    唤起（左下角更新胶囊）：无视本版「稍后」记录强制弹出——胶囊是随时再入
//    的入口，不能点了没反应。
function showUpdateBanner(u, force) {
  syncUpdatePill(u);
  if (!u || !u.latest) return;
  const bar = document.getElementById('update-banner');
  if (!bar) return;
  const actions = bar.querySelector('.ub-actions');
  if (!actions) return;
  if (u.pendingRestart && u.pendingVersion) {
    // 就绪横幅：本版本点过「稍后」不再自动弹（下次新版本重新提醒）；胶囊
    // 主动唤起（force）不受此限——那是用户明确要看，横幅必须出来
    try { if (!force && localStorage.getItem('jxUbLater') === u.pendingVersion) return; } catch (e) {}
    bar.querySelector('.ub-text').innerHTML =
      '新版本 <b>' + esc(u.pendingVersion) + '</b> 已就绪（当前 ' + esc(u.current || '') +
      '），重启软件后生效' +
      (u.notes ? '　<span class="ub-notes">' + esc(String(u.notes).slice(0, 80)) + '</span>' : '');
    bar.dataset.mode = 'ready';
    bar.dataset.pendingVer = u.pendingVersion;
    actions.innerHTML =
      '<button class="ub-confirm" type="button">立即重启</button>' +
      '<button class="ub-confirm ub-later" type="button">稍后</button>';
  } else {
    bar.querySelector('.ub-text').innerHTML =
      '新版本 <b>' + esc(u.latest) + '</b> 可用（当前 ' + esc(u.current || '') + '）' +
      (u.notes ? '　<span class="ub-notes">' + esc(String(u.notes).slice(0, 80)) + '</span>' : '');
    bar.dataset.mode = 'installer';
    bar.dataset.download = u.downloadUrl || '';
    bar.dataset.url = u.url || '';
    actions.innerHTML = '<button class="ub-confirm" type="button">确认更新</button>';
  }
  bar.classList.remove('hidden');
}

// syncUpdatePill 左下角「更新」胶囊（反馈胶囊左侧）：与置顶更新横幅同源——
// 无更新隐藏；新版本可用（available，manual/整包路）或已静默换装待重启
// （pendingRestart，auto/分块路）都出现。与横幅的差异只在「常驻」：横幅点过
// 「稍后」就藏，胶囊仍是随时再入的入口（点它唤起横幅，完整动作都在横幅里）。
let curUpdateState = null;
function syncUpdatePill(u) {
  curUpdateState = u || null;
  const pill = document.getElementById('btn-update');
  if (!pill) return;
  const on = !!(u && (u.available || u.pendingRestart));
  pill.classList.toggle('hidden', !on);
  if (!on) return;
  if (u.pendingRestart && u.pendingVersion) {
    pill.title = '新版本 ' + u.pendingVersion + ' 已就绪，重启软件后生效——点击查看';
  } else if (u.latest) {
    pill.title = '发现新版本 ' + u.latest + '——点击查看';
  }
}
document.getElementById('btn-update')?.addEventListener('click', (e) => {
  e.stopPropagation(); // 拦住外层设置按钮的默认打开路径，只唤起更新横幅
  if (curUpdateState) showUpdateBanner(curUpdateState, true); // 主动唤起：无视「稍后」必弹
});
document.getElementById('btn-update')?.addEventListener('keydown', (e) => {
  if (e.key === 'Enter' || e.key === ' ') {
    e.preventDefault(); e.stopPropagation();
    if (curUpdateState) showUpdateBanner(curUpdateState, true);
  }
});

$('update-banner').addEventListener('click', (e) => {
  const bar = e.currentTarget;
  if (e.target.closest('.ub-close')) { bar.classList.add('hidden'); return; }
  if (bar.dataset.mode === 'ready') {
    if (e.target.closest('.ub-later')) {
      // 稍后：记下版本，本版不再弹（横幅可从设置页「立即检查」重新唤起）
      try { localStorage.setItem('jxUbLater', bar.dataset.pendingVer || ''); } catch (err) {}
      bar.classList.add('hidden');
      return;
    }
    if (e.target.closest('.ub-confirm')) {
      // 立即重启：后端拉起 PowerShell 助手（逃逸 Job）等本进程退出后接力新 exe
      api('/api/update/restart', { method: 'POST' }).then(() => {
        bar.querySelector('.ub-text').textContent = '正在重启…';
      }).catch((err) => {
        alert('立即重启失败：' + err.message + '\n磁盘文件已就绪，手动重启软件即可生效。');
      });
    }
    return;
  }
  if (e.target.closest('.ub-confirm')) {
    // 确认更新：后端静默下载安装包并在完成后自动运行（jxDownload）；
    // 下载期间横幅就地显示进度态，结果经 update_installer 事件更新文案
    const dl = bar.dataset.download;
    if (dl) {
      jxDownload(dl);
      bar.querySelector('.ub-text').innerHTML =
        '正在后台下载新版本安装包，下载完成后将自动运行安装…';
      const acts = bar.querySelector('.ub-actions');
      if (acts) acts.innerHTML = ''; // 防重复点击再次发起下载
    } else if (bar.dataset.url) {
      jxOpenPage(bar.dataset.url); // 没挂附件就退回发布页
      bar.classList.add('hidden');
    }
  }
});

// retryRun 重试上一轮：直接启动新一轮执行（用户消息已在库里），并跟随输出
async function retryRun(view) {
  view = view || mainView;
  const cur = view.state.cur;
  if (!cur || view.state.streaming) return;
  hideRunError(view);
  view.state.streaming = true;
  view._pinned = false; // 主动重试：恢复跟随
  setStreamUI(true, view);
  view.state.abort = new AbortController();
  view.state.streamText = '';
  view.state.streamThinking = '';
  let aborted = false;
  try {
    const res = await fetch(`/api/sessions/${cur.id}/retry`, {
      method: 'POST', signal: view.state.abort.signal,
    });
    if (!res.ok) throw new Error(res.statusText || '重试失败');
    await consumeStream(res, view, cur.id);
  } catch (e) {
    if (e.name === 'AbortError') aborted = true;
    // 慢网下请求失败可能晚于切走：报错条只落在仍停在这个会话的界面上
    else if (view.state.cur && view.state.cur.id === cur.id) showRunError(view, '重试失败：' + e.message);
  }
  await endTurn(view, aborted, { sid: cur.id, abort: view.state.abort });
}

// endTurn 一轮结束后的收尾（send / retry / edit / attach 共用）：收流式、对齐状态、刷新周边。
// stream = {sid, abort} 用于辨认「这条流还算不算当前界面的」：用户中途切到别的
// 会话（detachStream）或又接了别的流时，这条流的收尾不再动界面。
async function endTurn(view, aborted, stream) {
  if (stream && (view.state.abort !== stream.abort ||
      !view.state.cur || view.state.cur.id !== stream.sid)) return;
  removeLive(view);
  view.state.streaming = false;
  view.state.attached = false;
  view.state.pendingSend = null; // 收尾兜底：占位气泡不越过本轮（正常路径早已被回显替换）
  if (aborted) {
    await reloadMessages(view); // 本地被动断流：整体对一次
  } else {
    await refreshIfDangling(view); // 流断但后端仍在跑：接回去继续看
    // 正常收尾也重取一次：文件改动面板与交付卡片当轮即出（此前只有
    // 重开会话时才会拉到 deliveries/checkpoints，同页跑完的回合永远不显示）
    if (!view.state.streaming) await reloadMessages(view);
  }
  // 中途的 await（backendRunning / reloadMessages 都取会话详情）在慢网下可达数秒，
  // 期间用户可能已切走/回首页：收尾前复验这条流仍属于当前界面，否则整个尾巴不动。
  // 晚到的旧续体曾把空消息盖到首页问候区上、连里面的输入卡一起销毁
  if (stream && (view.state.abort !== stream.abort ||
      !view.state.cur || view.state.cur.id !== stream.sid)) return;
  renderMessages(view.state.msgs, view);
  setStreamUI(false, view);
  renderQueueDock(view); // 流结束才轮得到横幅判定（执行结束后队列保留的「继续发送」）
  if (view === mainView) {
    loadSessions();
    loadStatus();
    refreshCtxChip();
    JX.emit('message:done', { sessionId: state.cur && state.cur.id });
  } else {
    refreshDrawer();
    refreshCtxChip(subView);
  }
}

// 排队坞：输入框上方的紧凑面板，列出排队的消息（提前为插话/编辑回填/删除/
// 拖拽排序）。头部带队列级「暂停/继续」开关；暂停中、或执行结束后队列保留
// （停止/出错）时给横幅 +「继续发送」。顺序与暂停状态都以服务端为准。
function renderQueueDock(view) {
  view = view || mainView;
  const box = viewEl(view, 'queue-dock');
  if (!box) return;
  const list = view.state.pendingQueue || [];
  if (!view.state.cur || !list.length) { box.classList.add('hidden'); box.innerHTML = ''; return; }
  box.classList.remove('hidden');
  const paused = !!view.state.queuePaused;
  const held = !paused && view.state.queueHeld && !view.state.streaming;
  const expanded = box.dataset.open === '1' || list.length === 1;
  box.innerHTML = '';
  const panel = document.createElement('div');
  panel.className = 'qd-panel';
  // 头部常驻（单条也有——暂停是队列级开关，单条消息同样可能想压着不发）
  const head = document.createElement('div');
  head.className = 'qd-head';
  const tog = document.createElement('button');
  tog.className = 'qd-toggle';
  tog.type = 'button';
  tog.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 12h13M3 6h18M3 18h18"/><path d="M17 9l3 3-3 3"/></svg>' +
    '<span class="qd-count">' + list.length + ' 条排队消息</span>' +
    '<span class="qd-sub">' + (paused ? '已暂停，继续前不自动发送' : '本轮结束后依次发送') + '</span>' +
    (list.length > 1 ? '<svg class="caret" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M6 9l6 6 6-6"/></svg>' : '');
  if (list.length > 1) {
    tog.onclick = () => { box.dataset.open = expanded ? '0' : '1'; renderQueueDock(view); };
  } else {
    tog.tabIndex = -1; // 单条没有折叠语义
    tog.style.cursor = 'default';
  }
  head.appendChild(tog);
  const pause = document.createElement('button');
  pause.className = 'qd-act qd-pause';
  pause.type = 'button';
  pause.title = paused ? '继续：排队消息依次自动发送' : '暂停：本轮结束后排队消息保留不自动发送';
  pause.innerHTML = paused
    ? '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M6 4l14 8-14 8Z"/></svg>'
    : '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><path d="M9 5v14M15 5v14"/></svg>';
  pause.onclick = () => pendingAction(view, '', paused ? 'resume' : 'pause');
  head.appendChild(pause);
  panel.appendChild(head);
  if (paused || held) {
    const banner = document.createElement('div');
    banner.className = 'qd-banner';
    const bt = document.createElement('span');
    bt.className = 'qd-btext';
    bt.textContent = paused
      ? '队列已暂停：' + list.length + ' 条消息保留，不会自动发送'
      : '本轮已结束：' + list.length + ' 条排队消息保留待发';
    banner.appendChild(bt);
    const resume = document.createElement('button');
    resume.className = 'qd-resume';
    resume.type = 'button';
    resume.textContent = '继续发送';
    resume.onclick = () => pendingAction(view, '', 'resume');
    banner.appendChild(resume);
    panel.appendChild(banner);
  }
  if (!expanded) { box.appendChild(panel); return; }
  const ul = document.createElement('ul');
  ul.className = 'qd-list';
  ul.addEventListener('dragend', () => qdClearDropMarks(ul));
  list.forEach((pm) => {
    const li = document.createElement('li');
    li.className = 'qd-row';
    li.dataset.id = pm.id;
    if (list.length > 1) li.appendChild(qdGrip(pm, li));
    const txt = document.createElement('span');
    txt.className = 'qd-text';
    const qdPrev = qdPreviewText(pm.content); // 摘要跳过块开头，直接显示用户正文
    txt.textContent = qdPrev.length > 200 ? qdPrev.slice(0, 200) + '…' : qdPrev;
    txt.title = pm.content;
    li.appendChild(txt);
    li.appendChild(qdAction('steer', view, pm, '插话发送：模型输出中立即打断生效；工具执行中等下一步调用模型', '', '立即插话'));
    // 编辑只给自己发的排队项（kind 空/chat）；数字助理派发注入的不参与
    if (!pm.kind || pm.kind === 'chat') {
      li.appendChild(qdAction('edit', view, pm, '退回输入框编辑', '<path d="M12 20h9"/><path d="M16.5 3.5a2.1 2.1 0 0 1 3 3L7 19l-4 1 1-4Z"/>'));
    }
    li.appendChild(qdAction('remove', view, pm, '移出队列', '<path d="M6 6l12 12M18 6L6 18"/>'));
    qdDropTarget(view, li);
    ul.appendChild(li);
  });
  panel.appendChild(ul);
  box.appendChild(panel);
}

// 排队坞摘要用：剥离开头的「用户附件：」「用户引用：」块，直接显示用户正文
// （纯展示层——排队项内容本身不动，悬停 title 仍看全文）
function qdPreviewText(c) {
  let s = String(c || '');
  for (;;) {
    let head = -1;
    if (s.indexOf(ATTACH_HEAD) === 0 && s.charAt(ATTACH_HEAD.length) === '\n') head = ATTACH_HEAD.length;
    else if (s.indexOf(QUOTE_HEAD) === 0 && s.charAt(QUOTE_HEAD.length) === '\n') head = QUOTE_HEAD.length;
    else break;
    const after = s.slice(head + 1);
    const blank = after.indexOf('\n\n');
    s = blank >= 0 ? after.slice(blank + 2) : '';
  }
  return s;
}

// 拖拽把手（HTML5 DnD，桌面用；触屏拖拽浏览器不支持，排序走桌面端）
function qdGrip(pm, li) {
  const g = document.createElement('span');
  g.className = 'qd-grip';
  g.title = '拖拽调整发送顺序';
  g.draggable = true;
  g.innerHTML = '<svg viewBox="0 0 24 24" fill="currentColor"><circle cx="9" cy="6" r="1.7"/><circle cx="15" cy="6" r="1.7"/><circle cx="9" cy="12" r="1.7"/><circle cx="15" cy="12" r="1.7"/><circle cx="9" cy="18" r="1.7"/><circle cx="15" cy="18" r="1.7"/></svg>';
  g.addEventListener('dragstart', (e) => {
    e.dataTransfer.effectAllowed = 'move';
    e.dataTransfer.setData('text/plain', pm.id);
    li.classList.add('dragging');
  });
  g.addEventListener('dragend', () => li.classList.remove('dragging'));
  return g;
}

// 行落点：上半行=插到该行前，下半行=插到该行后（用下一行当锚点；末行下半=队尾）。
// 锚点 id 上送服务端定序（move before=锚点），顺序唯一权威在服务端。
function qdDropTarget(view, li) {
  li.addEventListener('dragover', (e) => {
    e.preventDefault();
    e.dataTransfer.dropEffect = 'move';
    const r = li.getBoundingClientRect();
    const top = (e.clientY - r.top) < r.height / 2;
    qdClearDropMarks(li.parentElement);
    li.classList.add(top ? 'drop-before' : 'drop-after');
  });
  li.addEventListener('drop', (e) => {
    e.preventDefault();
    const dragId = e.dataTransfer.getData('text/plain');
    if (!dragId || dragId === li.dataset.id) return;
    const list = view.state.pendingQueue || [];
    const idx = list.findIndex((p) => p.id === li.dataset.id);
    if (idx < 0) return; // 拖的过程中列表变了：放弃本次落点（下次渲染已对齐最新）
    const r = li.getBoundingClientRect();
    const top = (e.clientY - r.top) < r.height / 2;
    const anchor = top ? list[idx].id : (list[idx + 1] ? list[idx + 1].id : '');
    if (anchor === dragId) return; // 原位：不动
    movePendingTo(view, dragId, anchor);
  });
}

function qdClearDropMarks(ul) {
  if (!ul) return;
  ul.querySelectorAll('.drop-before,.drop-after').forEach((n) => n.classList.remove('drop-before', 'drop-after'));
}

// 拖拽排序落地：move 成功后直接应用服务端返回的 pending 快照（多窗口一致）
async function movePendingTo(view, id, before) {
  try {
    const d = await api('/api/sessions/' + view.state.cur.id + '/pending', {
      method: 'POST', body: JSON.stringify({ id, action: 'move', before: before || '' }),
    });
    applyPending(view, d.pending);
  } catch (e) {
    pendingHint(view, '排序失败：' + e.message);
  }
}

// 排队项动作钮：带 label 的渲染成文字按钮（插话是首要动作，中文直说不用猜图标），
// 不带的照旧渲染圆形图标钮（编辑/移除是次要动作，图标 + 悬停提示够了）
function qdAction(action, view, pm, title, path, label) {
  const b = document.createElement('button');
  b.className = 'qd-act' + (action === 'steer' ? ' qd-steer' : '');
  b.type = 'button';
  b.title = title;
  if (label) b.textContent = label;
  else b.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' + path + '</svg>';
  b.onclick = () => (action === 'edit' ? editPendingMsg(view, pm) : pendingAction(view, pm.id, action));
  return b;
}

async function pendingAction(view, id, action) {
  if (action === 'pause' || action === 'resume') {
    // 队列级开关（不需要消息 id）：暂停后本轮跑完保留不续发；恢复时空闲即接着发
    try {
      const d = await api('/api/sessions/' + view.state.cur.id + '/pending', {
        method: 'POST', body: JSON.stringify({ id: '', action }),
      });
      applyPending(view, d.pending);
      pendingHint(view, action === 'pause' ? '已暂停队列：本轮结束后排队消息保留不发送' : '已继续队列：排队消息将依次发送');
    } catch (e) {
      pendingHint(view, '操作失败：' + e.message);
    }
    return;
  }
  try {
    const d = await api('/api/sessions/' + view.state.cur.id + '/pending', {
      method: 'POST', body: JSON.stringify({ id, action }),
    });
    applyPending(view, d.pending);
    pendingHint(view, action === 'steer' ? '已提前为插话：模型输出中立即打断生效' : '已移出队列');
  } catch (e) {
    pendingHint(view, '操作失败：' + e.message);
  }
}

// 编辑待生效消息（排队/插话）＝「撤回式编辑」：先从服务端队列权威删除（remove），
// 成功后才把原文回填输入框当草稿——消息要么在队列、要么在输入框，不会有两份。
// 输入框里已有草稿时拒绝，避免覆盖正在打的内容；删除请求返回前切走了会话则只保留
// 删除、放弃回填（否则会把旧会话的内容写进新会话的输入框）。
async function editPendingMsg(view, pm) {
  view = view || mainView;
  if (view.input.value.trim()) {
    pendingHint(view, '请先发送或清空输入框内容，再编辑排队消息');
    return;
  }
  const sid = view.state.cur && view.state.cur.id;
  if (!sid) return;
  try {
    const d = await api('/api/sessions/' + sid + '/pending', {
      method: 'POST', body: JSON.stringify({ id: pm.id, action: 'remove' }),
    });
    if (!view.state.cur || view.state.cur.id !== sid) return;
    applyPending(view, d.pending);
    view.input.value = pm.content;
    clearQuote(view); // 退回的消息文本里已带「用户引用：」段：别在胶囊上再叠一层
    autoGrow(view.input);
    if (view === mainView) persistMainDraftLS(); else persistSubDraftLS(); // 程序化填入也要落盘，切走/重启不丢
    view.input.focus();
    pendingHint(view, '已退回输入框：编辑后重新发送');
  } catch (e) {
    if (view.state.cur && view.state.cur.id === sid) pendingHint(view, '操作失败：' + e.message);
  }
}

// 待生效消息快照入视图（服务端下发或接口返回）：只对当前会话生效
function applyPending(view, pending) {
  if (!pending) return;
  view.state.pendingSteer = pending.steer || [];
  view.state.pendingQueue = pending.queue || [];
  view.state.queuePaused = !!pending.paused;
  if (!view.state.pendingQueue.length) view.state.queueHeld = false; // 队列清空：保留横幅不再有意义
  renderQueueDock(view);
  if (view.state.cur) renderMessages(view.state.msgs, view);
}

// 执行中发出的消息：按「对话中途发送行为」插话或排队（两种模式都在服务端排队）
// 默认值=等待排队：新装/没设置过 jx.midrun 时排队；只有用户在设置里显式选过
// 「插话」（localStorage 里存了 interject）才走 steer。手动切换照旧存同一个键。
async function sendMidrun(content, viaInput, view, voiceUsed) {
  view = view || mainView;
  const mode = lsGet('jx.midrun', 'queue') === 'interject' ? 'steer' : 'queue';
  try {
    const d = await api('/api/sessions/' + view.state.cur.id + '/interject', {
      method: 'POST', body: JSON.stringify({ content, mode, voiceUsed: !!voiceUsed }),
    });
    if (!d.accepted) { pendingHint(view, '发送失败：该会话当前不接受消息'); return; }
    // 本地立即出效果（插话贴到对话末尾，排队进坞）；服务端的 pending 广播可能
    // 先于本次响应到达，按 id 去重，避免同一条显示两次
    const known = view.state.pendingSteer.concat(view.state.pendingQueue).some((p) => p.id === d.id);
    if (!known) {
      const pm = { id: d.id, content: content, createdAt: '' };
      if (d.mode === 'steer') view.state.pendingSteer.push(pm);
      else view.state.pendingQueue.push(pm);
    }
    renderQueueDock(view);
    view._pinned = false; // 插话/排队展示在对话末尾：恢复跟随看得到回执
    renderMessages(view.state.msgs, view);
    pendingHint(view, d.mode === 'steer' ? '已插话：模型输出中立即打断生效；工具执行中等下一步调用模型' : '已排队：本轮结束后自动发送');
  } catch (e) {
    pendingHint(view, '发送失败：' + e.message);
    return;
  }
  if (viaInput) {
    view.input.value = ''; clearQuote(view); autoGrow(view.input);
    delete view.input.dataset.jxVoice; // 语音标记已随本条消息上报，随之退场
    if (view === mainView) clearSessionDraftLS(view.state.cur.id); else clearSubDraftLS(view.state.cur.id);
  }
}

// 本轮 chat 是否已落库：从会话详情倒序找最后一条用户发言（role=user 且
// kind=chat/空——注入回报行不算用户发言），正文与本次一致即认定服务端已收下。
// 给 send 的断连兜底用：误判代价不对称——把已落库的消息再发一遍会重复执行一
// 轮（执行记录+排队项两条一样的），把没落库的误判成已落库只是少发一条（界面
// 随后接回流，用户看得见会话还在跑，重发即可）。
function userChatLanded(msgs, content) {
  for (let i = msgs.length - 1; i >= 0; i--) {
    const m = msgs[i];
    if (m.role !== 'user') continue;
    if (m.kind && m.kind !== 'chat') continue;
    return m.content === content;
  }
  return false;
}

// prepareSend 是 send 的准备段：发送前钩子（可等待）→ 空文案守卫 → 懒建会话 →
// 附件落盘（收集注记）→ 组装正文（附件块+引用块+正文）→ 清草稿 → 置流式态。
// 返回 null = 本次发送已处理或中止（重复触发 / 空正文 / 插话排队 / 未选子对话 /
// 附件失败），send 就此返回；返回上下文 =
// 进入流式阶段。_sendBusy 闸门防重入：准备段有多个 await 点（钩子/建会话/附件
// 落盘），快速连按回车会重入——闸门期间的重复触发直接忽略；进入流式态后由
// streaming 标志分流（改走插话/排队），闸门随即放行，任何出口都不会卡死发送。
// 一点发送就把占位符的「执行中」段提前亮起（不等钩子/懒建会话/附件落盘）——
// 用户视角：点了发送就是开始执行，听写尾段识别、建会话这些等待不该让提示迟
// 到。只动占位符，不动 state.streaming（闸门/分流语义不受影响）；真正的流式
// 态由本函数尾部的 setStreamUI(true) 接管（dataset 幂等，不会叠加）。准备段
// 中途放弃发送的出口要收回（armed 只记自己置的位：进函数时已在流式态的，段
// 属于正在跑的那轮，不动）。
async function prepareSend(forceContent, view) {
  if (view._sendBusy) {
    // 第一次发送还停在附件落盘阶段（flush 期间 _sendBusy/_attachFlush 都占着）：
    // 静默吞掉这次点击，用户以为点了没反应——给一句指路（✕ 移除上传中的附件
    // 即解锁）。普通防重入（钩子/建会话中的连按回车）依旧静默忽略。
    if (view._attachFlush) attachHint(view, '附件还在上传中，稍候再发送；点附件上的 ✕ 可移除它', 4000);
    return null;
  }
  view._sendBusy = true;
  const armed = !view.state.streaming;
  if (armed) runHintOn(view);
  try {
  // 发送前钩子（可等待，JX.awaitHooks）：组件在此刻意收尾——听写组件等尾段识别
  // 落定再放行，随说的内容随这条消息一起发出。逐个 await、异常隔离、总超时放行，
  // 组件卡死不会卡死发送。回车 / Ctrl+回车 / 发送按钮 / 子对话，所有发送路径都
  // 经过这里；没有组件注册钩子时是空操作。
  await JX.awaitHooks('send', { view });
  const input = view.input;
  const fromInput = typeof forceContent !== 'string';
  // 本条消息含语音听写转写文字（组件听写落框时在输入框上打的标记 jxVoice）：
  // 随消息上报后端置位会话粘性标记，环境快照自此每轮带同音错字提示（server.go）
  const voiceUsed = fromInput && input.dataset.jxVoice === '1';
  const staged = attachmentsOf(view);
  // 只挂了附件、没写字：不替用户编消息，提示补齐再发（附件留在输入框上方）
  if (fromInput && !input.value.trim()) {
    if (staged.length) attachHint(view, '写一句要让数字助理做什么，' + staged.length + ' 个附件会随这条消息一起上传');
    if (armed) runHintOff(view);
    return;
  }
  if (view._attachFlush) { if (armed) runHintOff(view); return; } // 附件正在落盘：忽略重复点发送（提示在入口 _sendBusy 处统一给）
  if (view.state.streaming) {
    // 执行中插话/排队：先传附件（返回注记），组装后的内容进插话/排队队列——
    // 模型在消息真正生效那一刻才看到附件块与正文，文件信息不再先于消息到达
    let midNotes = [];
    if (staged.length) {
      midNotes = await flushAttachments(view);
      if (midNotes === null) { if (armed) runHintOff(view); return; }
      clearAttachments(view); // 已上传并将并入内容：附件区退场
    }
    const content = (fromInput ? composeQuoted(view, input.value, midNotes) : forceContent).trim();
    await sendMidrun(content, fromInput, view, voiceUsed);
    return;
  }
  // 这条消息是不是从首页（新对话）发出的：决定了发送后清哪份草稿
  const fromHome = view === mainView && !view.state.cur;
  if (!view.state.cur) {
    if (view === mainView) {
      // 懒创建：发出第一条消息才真正建会话（侧栏不再出现空会话，首页不再一闪而过）
      const s = await api('/api/sessions', {
        method: 'POST',
        body: JSON.stringify({
          mode: CHAT_MODES.some((m) => m.v === homeDraft.mode) ? homeDraft.mode : 'task',
          workspace: homeDraft.workspace || '',
          remote: homeDraft.remote || null,
          permission: homeDraft.permission,
          provider: homeDraft.provider,
          providerKey: homeDraft.providerKey || '',
          model: homeDraft.model,
          thinking: homeDraft.thinking || '',
          planMode: homeDraft.planMode,
          goalMode: homeDraft.goalMode,
          goalText: homeDraft.goalText,
          tools: homeDraft.tools,
        }),
      });
      state.cur = s;
      rpRestoreSession(s.id); // 懒创建也切了会话：右栏各视图跟上（否则 Git 面板还停在首页态，显示默认工作区）
      placeComposer();
      syncComposer();
      refreshDrawer();
      loadSessions();
    } else {
      // 没选中子对话：懒创建（与主对话首页同款）——发出第一条消息才真正建
      // 子对话（继承主对话上下文）并就地打开，消息照发，不提前留空会话；
      // 无主对话可挂靠或创建失败由 newAuxChat 就地提示，输入内容保留、本轮中止
      const typedText = input.value; // 空态里打进来的这句话
      await newAuxChat();
      if (!view.state.cur) { if (armed) runHintOff(view); return; }
      // 懒创建走的是 openSubSession，它按「切换会话即整体换草稿」的铁律把输入框
      // 换成新子对话的空草稿——空态里刚打的这句话会被一并清掉，下面组装正文就
      // 只剩空串（后端以 content 不能为空拒收），右栏留下一个「话没发出去」的空
      // 子对话。这条消息是用户点发送之前就写好的，与切换会话无关：放回输入框，
      // 后面组装正文、发送失败兜底都还认输入框这一处。
      input.value = typedText;
      autoGrow(input);
      persistSubDraftLS(); // 放回的字同样落盘：万一这次发送失败，切走再回来还在
    }
  }
  // 附件先落盘：注记要拼进消息头（真实落盘文件名/uid/判重说明上传完才知道），
  // 内容在落盘之后组装；有失败就中止本轮——输入内容与附件区都留着（全量保留，
  // 重试时判重幂等，见 flushAttachments）
  let attNotes = [];
  if (attachmentsOf(view).length) {
    attNotes = await flushAttachments(view);
    if (attNotes === null) { if (armed) runHintOff(view); return; }
    clearAttachments(view); // 已上传并将并入内容：附件区退场
  }
  // 组装：附件块（注记原样）+ 引用块 + 正文；forceContent 路径（编辑重发/降级
  // 补发）内容已完整，不再叠加
  const content = (fromInput ? composeQuoted(view, input.value, attNotes) : forceContent).trim();

  if (fromInput) {
    input.value = ''; clearQuote(view); // 引用/附件已组装进这条消息：输入框与胶囊一起退场
    delete input.dataset.jxVoice; // 语音标记已随本条消息上报，随之退场
    autoGrow(input);
    // 发出即清：首页清 homeDraft 的 text，主对话清 jx.st.<id>，子对话清 jx.subst.<id>
    if (view === mainView) {
      if (fromHome) clearHomeDraftLS();
      else clearSessionDraftLS(view.state.cur.id);
    } else {
      clearSubDraftLS(view.state.cur.id);
    }
  }

  const sid = view.state.cur.id; // 这条流所属的会话：切走后它的帧与收尾都不再动界面
  view.state.streaming = true;
  view._pinned = false; // 自己发的消息：恢复跟随，输出落地即到底
  setStreamUI(true, view);
  view.state.abort = new AbortController();
  view.state.streamText = '';
  view.state.streamThinking = '';
  hideRunError(view); // 新一轮开始：上一轮的报错条收掉
  // 立显「发送中」占位：回显落地前（远程/慢网可数秒）消息不悬空；带 sid，
  // 切走自失效，message 事件的用户回显落地即换正常气泡
  view.state.pendingSend = { sid, content };
  renderMessages(view.state.msgs, view);
  if (view === mainView) JX.emit('send', { content });
    return { input, content, sid, fromInput, voiceUsed };
  } catch (e) {
    if (armed) runHintOff(view); // 准备段异常（懒建会话失败等）：收回提前亮起的提示
    throw e;
  } finally {
    view._sendBusy = false;
  }
}

async function send(forceContent, view) {
  view = view || mainView;
  const pre = await prepareSend(forceContent, view);
  if (!pre) return;
  const { input, content, sid, fromInput } = pre;

  let aborted = false;
  // 服务端是否已收下这条消息：handleChat 在返回响应头之前就完成了落库并启动
  // 执行——只要拿到 2xx 响应头，之后流再断，消息也一定在跑了，绝不能重发
  //（否则同一条消息一边执行、一边又出现在排队队列，正是远程断流时「重复排队」
  // 的根源）。响应头都没回来的断连没法靠这个标记，只能靠内容比对兜底（见
  // userChatLanded）。
  let accepted = false;
  try {
    const res = await fetch(`/api/sessions/${sid}/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ content, voiceUsed: !!pre.voiceUsed }),
      signal: view.state.abort.signal,
    });
    if (!res.ok) {
      const d = await res.json().catch(() => ({}));
      throw new Error(d.error || res.statusText);
    }
    accepted = true;
    await consumeStream(res, view, sid);
  } catch (e) {
    if (e.name === 'AbortError') {
      aborted = true; // 用户主动停止（已另行请求后端停止）：排队消息保留
    } else if (!view.state.cur || view.state.cur.id !== sid) {
      // 请求失败的信息晚于切走/回首页到达：界面归新导航管，本轮不作任何界面动作
      // （旧代码这里会拿空 msgs 盖掉首页问候区、对空 cur 取 id 还会抛错）
    } else {
      // 流断了不等于执行结束：后端执行体独立于这条连接，先对一次状态再决定提示。
      // 手机远程链路抖动时最常见的失败形态是「请求已到、消息已落库、响应流中途
      // 断」——旧逻辑探测到会话在跑就一律降级重发，把正在跑的消息又原样塞进
      // 排队队列（刷新后看到执行记录与排队项一模一样两条）。现在先确认服务端
      // 收没收下这条消息，没收下且会话在跑才降级排队。
      view.state.streamText = '';
      view.state.streamThinking = '';
      view.state.pendingSend = null; // 这轮的去向马上有定论：占位气泡退场
      renderMessages(view.state.msgs, view);
      let running = false;
      if (!accepted) {
        try {
          const d = await api('/api/sessions/' + sid);
          if (d.pending && view.state.cur && view.state.cur.id === sid) applyPending(view, d.pending);
          running = !!(d.running || (d.meta && d.meta.status === 'running'));
          accepted = userChatLanded(d.messages || [], content);
        } catch (e2) { /* 探测也失败（服务不可达）：按没收下处理，走下面的兜底 */ }
      }
      if (accepted) {
        // 消息已在服务端落库执行：不重发。endTurn → refreshIfDangling 会接回
        // 那条执行流继续看——断的只是这条连接，不是执行本身
      } else if (running) {
        // 后端刚被别的路径拉起执行（自动汇报/排队排水/别的窗口在发），这条消息
        // 被拒了且库里没有它：降级为插话/排队（在服务端排队，不丢），endTurn
        // 会接上那轮继续看
        await sendMidrun(content, false, view);
      } else {
        // 整轮没发出去（请求根本没到服务端，消息没落库）：内容退回输入框不丢字，
        // 报错条给出原因；用户改完重发即可（别用「重试」——库里没有这条消息）
        if (fromInput && !input.value) { input.value = content; autoGrow(input); }
        showRunError(view, '请求失败：' + e.message);
      }
    }
  }
  await endTurn(view, aborted, { sid, abort: view.state.abort });
}

/* ---------- 编辑历史用户消息并重发（语义对齐 ZCode 的 editUserQuery：
   同会话内截断后重发——该消息之后的记录从当前对话移除、模型回到这条消息
   重新执行。两种提交档（对齐 ZCode 的双提交按钮）：
   - 发送：仅重置对话，文件保持现状；
   - 回退文件并发送：把这条消息之后有快照的文件一并恢复到当时（服务端
     fail-closed 校验，被外部改过的文件会整单拒绝并给出逃生口）。
   想保留原路线，先用助手回答下的「分支」按钮把对话另存一份再编辑。 ---------- */

// 编辑点之后将被回退的文件清单（同一文件按最早一条快照算状态），确认框用
function rewindFileList(view, seq) {
  const byPath = new Map();
  for (const c of (view.state.checkpoints || [])) {
    if (c.seq <= seq) continue;
    if (!byPath.has(c.path)) byPath.set(c.path, c);
  }
  const restore = [], drop = [];
  for (const c of byPath.values()) (c.existedBefore ? restore : drop).push(c.path);
  return { restore, drop };
}

// 进入编辑态：气泡原位换成输入框（Esc/取消还原，Ctrl+Enter 或按钮提交）
function editUserMsg(m, view, wrap) {
  if (wrap.classList.contains('editing')) return;
  if (view.state.streaming) {
    view.hint.textContent = '本轮执行中，等结束再编辑';
    setTimeout(() => { if (view.hint.textContent === '本轮执行中，等结束再编辑') view.hint.textContent = ''; }, 2500);
    return;
  }
  wrap.classList.add('editing');
  const box = document.createElement('div');
  box.className = 'ed-box';
  const ta = document.createElement('textarea');
  ta.className = 'ed-input';
  ta.value = String(m.content ?? '');
  ta.rows = 3;
  const row = document.createElement('div');
  row.className = 'ed-row';
  const cancel = document.createElement('button');
  cancel.className = 'ed-btn';
  cancel.type = 'button';
  cancel.textContent = '取消';
  const rewind = document.createElement('button');
  rewind.className = 'ed-btn rewind';
  rewind.type = 'button';
  rewind.title = '恢复这条消息之后的文件改动（按快照校验），再重置对话并发送';
  rewind.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="9"/><path d="M12 7v5l3.5 2"/></svg>回退文件并发送';
  const ok = document.createElement('button');
  ok.className = 'ed-btn primary';
  ok.type = 'button';
  ok.textContent = '发送';
  row.appendChild(cancel);
  row.appendChild(rewind);
  row.appendChild(ok);
  const note = document.createElement('div');
  note.className = 'ed-note';
  note.textContent = '「发送」仅重置对话；「回退文件并发送」会同时把之后的文件改动恢复到当时（未做快照的文件无法回退）';
  box.appendChild(ta);
  box.appendChild(row);
  box.appendChild(note);
  wrap.appendChild(box);
  ta.focus();
  ta.setSelectionRange(ta.value.length, ta.value.length);
  const close = () => { wrap.classList.remove('editing'); box.remove(); };
  cancel.onclick = close;
  ta.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && $('confirm-modal').classList.contains('hidden')) {
      e.stopPropagation();
      close();
    }
    if (e.key === 'Enter' && (e.ctrlKey || e.metaKey)) { e.preventDefault(); ok.click(); }
  });
  const submit = async (mode) => {
    const content = ta.value.trim();
    if (!content) return false;
    const later = view.state.msgs.filter((x) => x.seq > m.seq).length;
    if (later > 0 || mode === 'rewind') {
      let msg = `这条消息之后还有 ${later} 条记录，重新发送后将全部移除（要保留原路线请先用「分支」）。`;
      if (mode === 'rewind') {
        const { restore, drop } = rewindFileList(view, m.seq);
        const parts = [];
        if (restore.length) parts.push('恢复 ' + restore.length + ' 个文件（' + restore.join('、') + '）');
        if (drop.length) parts.push('撤回 ' + drop.length + ' 个新建文件（' + drop.join('、') + '）');
        if (!parts.length) parts.push('这条消息之后没有可回退的文件快照（只重置对话）');
        msg = '将回退文件并重发：\n· ' + parts.join('\n· ') + '\n同时' + msg;
      } else {
        msg += '\n已做的文件修改不回退；要连文件一起回退请用「回退文件并发送」。';
      }
      if (!(await confirmCenter(msg))) return false;
    }
    ok.disabled = true;
    rewind.disabled = true;
    cancel.disabled = true;
    if (await resendEdit(m, content, view, mode)) { close(); return true; }
    ok.disabled = false;
    rewind.disabled = false;
    cancel.disabled = false;
    return false;
  };
  ok.onclick = () => submit('preserve');
  rewind.onclick = () => submit('rewind');
}

// 编辑重发的请求与收流：结构与 send() 的发后处理一致；成功返回 true（此时
// edited 确认帧已把本地记录截断对齐），失败/中止返回 false 保留编辑框。
// mode=rewind 时文件被外部改过会收到 blocked：就地给出「改为仅重发对话」逃生口
async function resendEdit(m, content, view, mode) {
  view = view || mainView;
  if (!view.state.cur || view.state.streaming) return false;
  const sid = view.state.cur.id; // 这条流所属的会话：切走后它的帧与收尾都不再动界面
  view.state.streaming = true;
  view._pinned = false; // 编辑重发也是自己的动作：恢复跟随
  setStreamUI(true, view);
  view.state.abort = new AbortController();
  view.state.streamText = '';
  view.state.streamThinking = '';
  if (view === mainView) JX.emit('send', { content });
  let aborted = false;
  // blocked（文件被外部改过）时给逃生口：改按 preserve 档就地重发一次
  const postEdit = async (wm) => {
    const res = await fetch(`/api/sessions/${sid}/edit`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ seq: m.seq, content, workspaceMode: wm }),
      signal: view.state.abort.signal,
    });
    if (!res.ok) {
      const d = await res.json().catch(() => ({}));
      if (d.blocked && wm === 'rewind') {
        const list = (d.unsafe || []).map((u) => u.path + '：' + (u.reason || '被外部修改')).join('\n');
        const fallback = await confirmCenter('以下文件在快照之后被外部修改，无法安全回退：\n' + list + '\n\n改为仅重发对话（不动这些文件）？');
        if (fallback) return postEdit('preserve');
      }
      throw new Error(d.error || res.statusText);
    }
    await consumeStream(res, view, sid);
    return true;
  };
  try {
    await postEdit(mode === 'rewind' ? 'rewind' : 'preserve');
  } catch (e) {
    if (e.name === 'AbortError') {
      aborted = true; // 用户主动停止：保留编辑框，可改完再试
    } else {
      view.state.streamText = '';
      view.state.streamThinking = '';
      showRunError(view, '编辑发送失败：' + e.message); // 真实失败进报错条，不插进对话区
    }
  }
  await endTurn(view, aborted, { sid, abort: view.state.abort });
  return !aborted;
}

/* ---------- 事件流读取与「接回执行中会话」 ---------- */

// consumeStream 读一条 SSE 响应直到流结束，每帧交给 handleEvent。
// sid 是这条流所属的会话：中途切到别的会话后本函数立刻收手——帧不能再写进
// 新会话的界面（后端执行不受影响，回来时重新接上即可）。
async function consumeStream(res, view, sid) {
  const reader = res.body.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { done, value } = await reader.read();
    if (done) break;
    if (sid && (!view.state.cur || view.state.cur.id !== sid)) return; // 已切走：丢弃
    buf += dec.decode(value, { stream: true });
    let idx;
    while ((idx = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, idx).trim();
      buf = buf.slice(idx + 2);
      if (!frame.startsWith('data:')) continue;
      try { handleEvent(JSON.parse(frame.slice(5)), view); } catch (e) { /* 跳过坏帧 */ }
    }
  }
}

// detachStream 放开当前这条本地流：只断开界面侧的读取，后端执行照旧跑完；
// 换会话/回首页/新建对话前必须调用，否则旧会话的增量会写进新会话的界面。
function detachStream(view) {
  view = view || mainView;
  if (!view.state.streaming) return;
  const abort = view.state.abort;
  view.state.streaming = false;
  view.state.attached = false;
  view.state.abort = null;
  view.state.pendingSend = null; // 切走即弃占位：回来时以服务端落库为准（endTurn 不会再跑）
  view.state.streamText = '';
  view.state.streamThinking = '';
  removeLive(view);
  setStreamUI(false, view);
  if (abort) { try { abort.abort(); } catch (e) { /* 已经断了 */ } }
}

async function backendRunning(sid, view) {
  const v = view || mainView;
  try {
    const d = await api('/api/sessions/' + sid);
    // 慢网下响应可能晚于导航：pending 属于被探测的会话，只对仍停在它上面的界面生效
    if (d.pending && v.state.cur && v.state.cur.id === sid) applyPending(v, d.pending);
    return !!(d.running || (d.meta && d.meta.status === 'running'));
  } catch (e) { return false; }
}

// refreshIfDangling 流断开但后端仍在执行：重新接上那条流继续看（刷新/网络抖动都不再打断对话）
async function refreshIfDangling(view) {
  const sid0 = view.state.cur && view.state.cur.id;
  if (!sid0) return;
  if (!(await backendRunning(sid0, view))) return;
  if (!view.state.cur || view.state.cur.id !== sid0) return; // 探测期间已切走：不接流
  attachRun(view);
}

// reloadMessages 只重取当前会话的消息与待生效队列（不触发接流，避免与 attachRun 互相递归）
async function reloadMessages(view) {
  view = view || mainView;
  if (!view.state.cur) return;
  if (view.state._loading) return; // 会话正在整包加载：由 loadSessionInto 收口，不并发重取
  const nav = navTok[view.key]; // 导航令牌：慢网下响应可能晚于切走/回首页，落地前必须复验
  try {
    // 分批口径：只重取基础窗（tail），已上翻加载的头部原样保留——attached 事件
    // 刷新不再把整段历史反复拉回（这正是主对话被事件轰炸时最大的流量来源）
    const d = await api('/api/sessions/' + view.state.cur.id + '?tail=' + LOAD_BATCH);
    // 已被更新的导航取代：晚到的旧会话数据不落地——否则 state.cur 被旧 meta 复活、
    // 界面被旧消息顶回旧会话，正是远程访问时「切过去又被跳回来」的根源
    if (nav !== navTok[view.key]) return;
    if (snapshotStale(view, view.state.cur.id, d.messages)) return; // 迟到的旧快照：界面已被流式帧推进，不回退
    const head = (d.messages && d.messages.length)
      ? view.state.msgs.filter((m) => m.seq < d.messages[0].seq) : [];
    if (head.length) {
      d.messages = head.concat(d.messages);
      if (d.older) d.older = { ...d.older, count: Math.max(0, (d.older.count || 0) - head.length) };
    } else if (d.older) {
      view.state._baseWinBase = { sys: d.older.sys, tools: d.older.tools }; // 基础窗边界前移（无上翻历史）：释放基线跟到最新
    }
    view.state.older = d.older ? { ...d.older, count: d.older.count || 0 } : undefined;
    renderLoadOlderBar(view);
    view.state.cur = d.meta;
    view.state.msgs = d.messages || [];
    view.state.pendingSteer = (d.pending && d.pending.steer) || [];
    view.state.pendingQueue = (d.pending && d.pending.queue) || [];
    view.state.checkpoints = d.checkpoints || [];
    view.state.deliveries = d.deliveries || [];
    // 上一轮失败过就继续显示报错条（刷新/切换回来都还在，方便接着重试）
    if (d.error) showRunError(view, d.error);
  } catch (e) { return; /* 取不到就保持现状 */ }
  renderQueueDock(view);
  renderMessages(view.state.msgs, view);
}

// attachRun 接上一个正在执行的会话的事件流（页面刷新/流断开后继续跟随输出）
async function attachRun(view) {
  view = view || mainView;
  const cur = view.state.cur;
  if (!cur || view.state.streaming) return;
  view.state.streaming = true;
  view.state.attached = true;
  const abort = new AbortController();
  view.state.abort = abort;
  view.state.streamText = '';
  view.state.streamThinking = '';
  hideRunError(view); // 执行还在跑：报错条收掉
  setStreamUI(true, view);
  await reloadMessages(view); // 先对齐到服务端现状，再接流
  try {
    const res = await fetch(`/api/sessions/${cur.id}/stream`, { signal: abort.signal });
    if (!res.ok || !res.body) throw new Error(res.statusText || '无法接上执行流');
    await consumeStream(res, view, cur.id);
  } catch (e) {
    if (e.name !== 'AbortError') console.warn('[JX] 接回执行流失败', e);
  }
  // 中途切到别的会话（detachStream）/ 又接了别的流：这条流的收尾不再动界面
  if (view.state.abort !== abort) return;
  removeLive(view);
  view.state.streaming = false;
  view.state.attached = false;
  setStreamUI(false, view);
  // 执行可能在这一段里结束了：重取一次，落库内容与待生效队列都归位
  if (view.state.cur && view.state.cur.id === cur.id) await reloadMessages(view);
  if (view === mainView) {
    loadSessions();
    refreshCtxChip();
  } else {
    refreshDrawer();
    refreshCtxChip(subView);
  }
  // 本轮可能是排队消息驱动的：它发完还有下一条，继续跟着看
  if (view.state.cur && view.state.cur.id === cur.id && view.state.abort === abort) {
    if (await backendRunning(cur.id, view)) attachRun(view);
  }
}

// stopRun 显式停止：后端执行体独立于页面，必须让服务端停——本地只是等着这条流自己收尾
// （取消会立刻作用到模型流/工具/等待，服务端写入的收尾记录也就随流到达）。
async function stopRun(view) {
  view = view || mainView;
  const cur = view.state.cur;
  if (!cur) return;
  view.hint.textContent = '正在停止…';
  try {
    const r = await api('/api/sessions/' + cur.id + '/stop', { method: 'POST' });
    if (r && r.stopped === false) { view.hint.textContent = ''; return; }
  } catch (e) {
    view.hint.textContent = '';
    return;
  }
  // 兜底：5 秒还没收尾（网络异常/执行体卡住）就断掉本地流并重新对齐一次
  const token = view.state.abort;
  setTimeout(async () => {
    if (view.state.streaming && view.state.abort === token) {
      if (view.state.abort) view.state.abort.abort();
      await reloadMessages(view);
    }
  }, 5000);
}

/* ---------- 上下文用量：圆环指示器（模型选择左侧）+ 悬停明细 ---------- */

const CTX_COLORS = { '消息': '#4d6bfe', '系统工具': '#1f9d55', '系统提示词': '#9a6700', '摘要': '#8b5cf6', '环境快照': '#0ea5a5', '其他': '#8b949e' };
const CTX_CIRC = 97.39; // 2πr，r=15.5
let lastCtx = { main: null, sub: null }; // 每个输入区视图各自的上下文用量（圆环+明细共用）
// 每个视图各记一个「压缩进行中」标记：明细卡里的按钮据此进禁用态（卡片每次重画都读它）
const ctxCompactBusy = { main: false, sub: false };
// 明细卡「压缩」按钮的图标（上下箭头夹一条中缝＝把历史往里收）
const CTX_COMPACT_ICON = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3v5"/><path d="m9.5 5.5 2.5 2.5 2.5-2.5"/><path d="M12 21v-5"/><path d="m9.5 18.5 2.5-2.5 2.5 2.5"/><path d="M4 12h16"/></svg>';

function fmtTokens(n) {
  if (n >= 10000) return (Math.round(n / 1000) / 10).toFixed(1) + '万';
  if (n >= 1000) return (Math.round(n / 100) / 10).toFixed(1) + 'k';
  return String(n);
}

async function refreshCtxChip(view) {
  view = view || mainView;
  const ring = viewEl(view, 'ctx-ring');
  if (!ring) return;
  const num = viewEl(view, 'ctx-num'), tip = viewEl(view, 'ctx-tip');
  const cur = view.state.cur;
  if (!cur) {
    lastCtx[view.key] = null;
    ring.classList.remove('warn', 'full');
    ring.querySelector('.cr-fg').style.strokeDashoffset = CTX_CIRC;
    num.textContent = '–';
    tip.innerHTML = '<div class="ctx-title">当前没有会话</div>';
    return;
  }
  try {
    lastCtx[view.key] = await api('/api/sessions/' + cur.id + '/context');
    const d = lastCtx[view.key];
    const pct = d.window ? Math.min(d.total / d.window, 1) : 0;
    ring.classList.toggle('warn', pct >= .6 && pct < .85);
    ring.classList.toggle('full', pct >= .85);
    ring.querySelector('.cr-fg').style.strokeDashoffset = CTX_CIRC * (1 - pct);
    num.textContent = Math.max(1, Math.round(pct * 100)) + '%';
    renderCtxTip(view);
  } catch (e) { /* 拿不到就保持原样 */ }
}

// 悬停明细：总量/窗口 + 占用条（整条=窗口，已用从左往右按窗口占比增长，
// 灰色轨道=未占用）+ 分类清单（share=占已用量比例）+ 缓存命中率
// 卡片顶部（标题行下面第一行）带一个「压缩」按钮：压缩**当前会话**的上下文。
function renderCtxTip(view) {
  view = view || mainView;
  const tip = viewEl(view, 'ctx-tip');
  const d = lastCtx[view.key];
  if (!d) { tip.innerHTML = '<div class="ctx-title">当前没有会话</div>'; return; }
  const pct = d.window ? Math.round(d.total * 100 / d.window) : 0;
  const running = ctxRunning(view);      // 执行中不给压：后端也 409，前端先拦更友好
  const busy = !!ctxCompactBusy[view.key];
  const off = busy || running;
  let html =
    '<div class="ctx-head">' +
      '<span class="ctx-title">上下文容量</span>' +
      `<span class="ctx-total">${fmtTokens(d.total)}/${fmtTokens(d.window)}（${pct}%）</span>` +
    '</div>' +
    // 「压缩」放卡片最上面（标题下方第一行、右上角）：压缩会改写对话历史，放顶部
    // 离用户翻开卡片时的手感落点（卡下沿/圆环）远一些，避免误操作——用户明确要求。
    '<div class="ctx-ops">' +
      (running ? '<span class="ctx-ops-hint">执行中，等这一轮结束再压缩</span>' : '') +
      '<button type="button" class="ctx-compact" data-ctx-compact="1"' + (off ? ' disabled' : '') +
        ` title="${running ? '会话正在执行，等这一轮结束再压缩' : '提前自动压缩：不等阈值到点，立即按设置的保留字数执行一次自动压缩（规则折叠或模型摘要，按当前设置的模式选路）'}">` +
        CTX_COMPACT_ICON +
        '<span class="cc-t">' + (busy ? '压缩中…' : '提前自动压缩') + '</span>' +
      '</button>' +
    '</div>' +
    '<div class="ctx-bar" title="已用 ' + pct + '%，灰色为未占用">';
  for (const c of d.categories || []) {
    if (!c.tokens || !d.window) continue;
    const w = (c.tokens * 100 / d.window).toFixed(2); // 段宽=占窗口比例，未占用留给灰色轨道
    html += `<i style="width:${w}%;background:${CTX_COLORS[c.name] || '#8b949e'}"></i>`;
  }
  html += '</div>';
  for (const c of d.categories || []) {
    html +=
      `<div class="ctx-cat"><span class="ctx-dot" style="background:${CTX_COLORS[c.name] || '#8b949e'}"></span>` +
      `<span class="cc-name">${esc(c.name)}</span><span class="cc-tok">${fmtTokens(c.tokens)}</span>` +
      `<span class="cc-pct">${c.share}%</span></div>`;
  }
  // Token 用量分组（截图样式）：命中%按输入口径；未缓存/缓存读取/输出按 token 累计
  const c = d.cache || {};
  const has = c.calls > 0;
  const tokFull = (n) => (n || 0).toLocaleString('en-US') + ' tok';
  const rate = has && c.promptTokens ? Math.round(c.cacheHit * 100 / c.promptTokens) + '%' : '—';
  html +=
    `<div class="ctx-tok-head"><span>Token 用量</span><b>${has ? tokFull((c.promptTokens || 0) + (c.output || 0)) : '—'}</b></div>` +
    `<div class="ctx-cat"><span class="cc-name">缓存命中</span><span class="cc-val">${rate}</span></div>` +
    `<div class="ctx-cat"><span class="cc-name">未缓存输入</span><span class="cc-val">${has ? tokFull(c.uncached) : '—'}</span></div>` +
    `<div class="ctx-cat"><span class="cc-name">缓存读取</span><span class="cc-val">${has ? tokFull(c.cacheHit) : '—'}</span></div>` +
    `<div class="ctx-cat"><span class="cc-name">输出</span><span class="cc-val">${has ? tokFull(c.output) : '—'}</span></div>`;
  // 输出速度（最后一次输出的吐字速度）：厂家 usage 报的 completion_tokens ÷
  // 输出窗口时长（首字到末字，不含首字前的等待）。网关不报 usage 或老会话的
  // 审计行没有窗口数据时不显示
  const sp = d.speed || {};
  if (sp.tokPerSec > 0 && sp.ms > 0) {
    html += `<div class="ctx-cat"><span class="cc-name">输出速度</span><span class="cc-val">${sp.tokPerSec.toFixed(1)} tok/s（${Number(sp.tokens).toLocaleString('en-US')} tok / ${(sp.ms / 1000).toFixed(1)}s）</span></div>`;
  }
  tip.innerHTML = html;
  // 卡里的按钮随 innerHTML 重建：每次渲染后重新挂（renderCtxTip 只在数据刷新/状态
  // 变化时调用，不会频繁重挂）
  const btn = tip.querySelector('[data-ctx-compact]');
  if (btn) btn.onclick = () => compactFromCtxPanel(view);
  placeCtxTip(view);
}

// 卡片定位兜底：卡片固定 250 宽、贴着圆环右缘往外撑，圆环太靠左（窄屏手机）时会
// 顶出屏幕左缘。这里按圆环的实时位置把它收回视口内——桌面/宽屏下算出来就是 CSS
// 里那个 right:-8px，外观不变；窄屏下往右收，顶部那行（压缩）始终完整可见。
function placeCtxTip(view) {
  const ring = viewEl(view, 'ctx-ring'), tip = viewEl(view, 'ctx-tip');
  if (!ring || !tip) return;
  const rr = ring.getBoundingClientRect();
  if (!rr.width) return; // 还没布局（页面隐藏等），别乱设
  const w = tip.offsetWidth || 250;
  const vw = document.documentElement.clientWidth || window.innerWidth;
  let rightEdge = Math.min(rr.right + 8, vw - 8);
  rightEdge = Math.max(rightEdge, Math.min(8 + w, vw - 8));
  tip.style.right = (rr.right - rightEdge) + 'px';
}

// 圆环是否该按「执行中」处理：本地有流在跑，或会话（详情 meta / 列表项）仍标着 running
function ctxRunning(view) {
  const cur = view.state.cur;
  if (!cur) return false;
  if (view.state.streaming) return true;
  if (cur.status === 'running') return true;
  const brief = (state.sessions || []).find((s) => s.id === cur.id);
  return !!(brief && brief.status === 'running');
}

// 本轮/整段的「终点」：最后一个属于对话回合的行的 seq（摘要/系统提示词/请求快照/
// 调用审计/继承标记这些归档行不属任何一轮，按它们收尾会让后端把终点算到轮外）
const CTX_ARCHIVE_KINDS = new Set(['summary', 'prompt', 'request', 'audit', 'inherit']);
function lastRoundSeq(view) {
  const msgs = (view.state && view.state.msgs) || [];
  for (let i = msgs.length - 1; i >= 0; i--) {
    const m = msgs[i];
    if (!m || !m.seq) continue;
    if (CTX_ARCHIVE_KINDS.has(m.kind)) continue;
    return m.seq;
  }
  return 0;
}

// 明细卡顶部的「提前自动压缩」：对**当前视图这个会话**手动点火一次自动压缩
// 线路（用户 2026-11-09 要求；此前它与「最后一轮正文输出下方那个压缩按钮」同款
// 走手动全量线路，现已分开——输出下方那个仍是手动全量，这里走自动线路）：
// · 接口 POST /api/sessions/{id}/autocompact（后端 autoCompactKick：与阈值触发的
//   自动压缩同一套动作——按设置的保留字数从最新一轮往前数、装得下的整轮保留，
//   更早的压缩；规则折叠还是模型摘要按当前设置的模式选路，前端不另造规则。
//   规则折叠同步完成；模型摘要 202 转后台执行，完成/失败经 session_updated 事件
//   驱动刷新，这里不等待）；
// · 先居中确认（用户明确在意误操作：位置放顶部 + 点一次确认，两道都上）；
// · 执行中不压：正在跑的一轮还在往历史里写，后端也会 409，前端先拦并说人话；
// · 完成后重拉 /api/sessions/{id}/context（refreshCtxChip）刷新这张卡，再轻提示
//   「压了多少 / 现在占用多少」；消息区一并翻新，折叠/摘要行才会出现。
async function compactFromCtxPanel(view) {
  view = view || mainView;
  const cur = view.state.cur;
  if (!cur) return;
  if (ctxCompactBusy[view.key]) return;
  if (ctxRunning(view)) {
    await confirmCenter('这个对话正在执行，等这一轮跑完再压缩。\n（正在跑的一轮会继续往历史里写，压了也会立刻被冲回来。）', true);
    return;
  }
  const upTo = lastRoundSeq(view);
  if (!upTo) {
    await confirmCenter('这个对话还没有可压缩的回合。', true);
    return;
  }
  const ok = await confirmCenter('提前执行一次自动压缩？\n按设置的保留字数保留最近几轮原文不动，更早的内容压缩掉' +
    '（规则折叠或模型摘要，按当前设置的模式选路）；原始消息仍可用历史检索工具查回。');
  if (!ok) return;
  const before = (lastCtx[view.key] && lastCtx[view.key].total) || 0;
  ctxCompactBusy[view.key] = true;
  renderCtxTip(view); // 按钮立刻进「压缩中…」禁用态
  // 收工：按钮回到可用（成功失败都走，成功时卡片已被 refreshCtxChip 重画过一遍也无妨）
  const restore = () => { ctxCompactBusy[view.key] = false; renderCtxTip(view); };
  try {
    const r = await api('/api/sessions/' + cur.id + '/autocompact', { method: 'POST' });
    if (r && r.async) {
      // 模型摘要已转后台（202 即回，大历史摘要要一两分钟）：不在这里等结果，
      // 登记状态后即可去干别的，完成/失败由 SSE 驱动对话区翻新
      compactingSess.add(cur.id);
      toast('已转入后台压缩，完成后对话区自动刷新', 5000);
      return; // finally 恢复按钮
    }
    await refreshCtxChip(view); // 重拉 /context：卡片里的数字与分类条按压缩后的状态重画
    const after = (lastCtx[view.key] && lastCtx[view.key].total) || 0;
    toast(compactResultMsg(r, before, after), 6000);
    // 消息区也要重取：折叠标记与摘要行只在重取后出现（与消息操作条上的「压缩」同款做法）
    if (mainView.state.cur && mainView.state.cur.id === cur.id) {
      if (!mainView.state.streaming) openSession(cur.id);
    } else if (subState.cur && subState.cur.id === cur.id) {
      if (!subState.streaming) openSubSession(cur.id);
    }
  } catch (e) {
    restore(); // 先恢复按钮再弹错误：别让它在错误框还开着的时候挂着「压缩中…」
    // 后端的原话照实给（如「没有可折叠的回合（保留最近约 N 字）」「该会话正在执行，稍后再压缩」）
    await confirmCenter('压缩失败：' + e.message, true);
  } finally {
    restore();
  }
}

// 压缩完成后的轻提示：压了什么 + 上下文从多少变成多少（省了多少）
function compactResultMsg(r, before, after) {
  const what = (r && r.folded)
    ? '已折叠 ' + r.folded + ' 个回合'
    : (r && r.scope === 'auto' ? '已把较早历史压缩成摘要' : '已把整个对话压缩成摘要');
  const from = before ? fmtTokens(before) : '—';
  const to = after ? fmtTokens(after) : '—';
  const saved = (before && after && before > after) ? before - after : 0;
  // 两个数按「万」折算后可能一模一样（省下的不到一千）：这时只报省了多少，
  // 别写出「2.8万 → 2.8万」这种看着像没压动的句子
  const body = from !== to ? '上下文 ' + from + ' → ' + to : '上下文 ' + to;
  return what + '：' + body + (saved ? '，省了 ' + fmtTokens(saved) : '');
}

/* ---------- 对话区选中文字浮窗：复制 / 引用到下方输入框 / 右侧辅助问答 ---------- */

let selKeepText = '';

/* ---------- 引用胶囊：对话区选文引用进输入框（多条累计、正文标记、发送完整） ----------
   「引用到输入框 / 在右侧辅助问答」不把全文塞进输入框——与用户自己打的字混在
   一起，模型分不清哪段是引用。引用挂到输入卡内的胶囊条上，并支持多条累计：
   · 每次引用在输入框光标处插入一个「“引用N”」纯文本标记，占位记下"在哪个位置
     引用了第几条"（输入框里由背衬层画成灰底胶囊，见「输入框标记胶囊」节）；胶囊
     条上 1 条显示“某某某…”截断，多条收成「引用了N条」（悬停
     title 看逐条清单），✕ 一键清空。
   · 发送时组装为「用户附件：\n<注记行>」块（附件上传返回的注记逐字原样，见
     flushAttachments）+「用户引用：\n“引用1” 全文\n“引用2” 全文」块 + 正文——
     前端看到的是胶囊与标记，模型收到的是注记与带编号的完整引用，正文里的
     “引用N”/文件标记即引用位置。
   · 对话区用户气泡：附件块与引用块逐条渲染成灰色行（截断 + title 全文），正文里
     的“引用N”标记与对得上号的文件标记渲染成灰色胶囊（title=对应全文/注记）。
   · 引用跟随输入草稿落盘（首页 jx.homeDraft / 主对话 jx.q.<id> / 子对话
     jx.subq.<id>，存 JSON 数组），切走再回来、刷新页面都不丢。 */

const QUOTE_HEAD = '用户引用：';
const ATTACH_HEAD = '用户附件：'; // 与后端 uploadpath.go 的同名常量一致
const QUOTE_SNIP = 10; // 胶囊与气泡里最多显示的字符数（按 Unicode 码点截，不劈中文）

// 按码点截断：前 N 字符 + 省略号（够短就原样返回）
function quoteClip(t) {
  const cps = Array.from(String(t || '').trim());
  if (cps.length <= QUOTE_SNIP) return cps.join('');
  return cps.slice(0, QUOTE_SNIP).join('').replace(/\s+$/, '') + '…';
}

// 引用标记：正文里占位“第 N 条引用”的短文本（输入框里是纯文本，气泡里渲染成灰胶囊）
function quoteMark(n) { return '\u201c引用' + n + '\u201d'; }

// 视图的引用列表（state.quotes：字符串数组）。老状态 state.quote 是单条字符串，
// 首次访问时就地迁移成数组
function quotesOf(view) {
  view = view || mainView;
  if (!Array.isArray(view.state.quotes)) {
    const old = String(view.state.quote || '').trim();
    view.state.quotes = old ? [old] : [];
    view.state.quote = '';
  }
  return view.state.quotes;
}

// 发送时组装：附件块在前（注记行逐字原样，来自上传接口返回——uid/判重/顺延
// 真实文件名/交付说明一字不改，随用户消息一起到达）、引用块次之、正文最后，
// 空行隔开。正文里的文件标记（“报告fina….docx”）原样留在正文里；forceContent
// 路径（编辑重发/降级补发）内容已完整，不经过本函数
function composeQuoted(view, text, attNotes) {
  const notes = (attNotes || []).filter(Boolean);
  const qs = quotesOf(view || mainView).map((q) => String(q).trim()).filter(Boolean);
  const body = String(text || '').trim();
  const parts = [];
  if (notes.length) parts.push(ATTACH_HEAD + '\n' + notes.join('\n'));
  if (qs.length) parts.push(QUOTE_HEAD + '\n' + qs.map((q, i) => quoteMark(i + 1) + ' ' + q).join('\n'));
  if (body) parts.push(body);
  return parts.join('\n\n');
}

// 反向拆解（渲染用户气泡用）：
// · 附件块 + 引用块 + 正文：「用户附件：\n<注记行>…\n\n用户引用：…\n\n正文」
//   （附件块与引用块可各自缺席，注记行不跨行）；
// · 旧版单条引用：「用户引用：“…”\n\n正文」——历史消息兼容
function splitQuoted(raw) {
  if (typeof raw !== 'string') return null;
  const out = { attNotes: [], quotes: [], rest: '' };
  let s = raw, hit = false;
  // 附件块在最先：逐行收注记，到空行为止（没有正文就整块都是注记）
  if (s.indexOf(ATTACH_HEAD) === 0 && s.charAt(ATTACH_HEAD.length) === '\n') {
    const after = s.slice(ATTACH_HEAD.length + 1);
    const cut = after.indexOf('\n\n');
    out.attNotes = (cut >= 0 ? after.slice(0, cut) : after).split('\n').filter(Boolean);
    out.rest = cut >= 0 ? after.slice(cut + 2) : '';
    hit = true;
    s = out.rest;
  }
  if (s.indexOf(QUOTE_HEAD) === 0) {
    const after = s.slice(QUOTE_HEAD.length);
    if (after.charAt(0) === '\n') {
      const cut = after.indexOf('\n\n');
      const block = cut >= 0 ? after.slice(0, cut) : after; // 没有正文：整块都是引用
      out.rest = cut >= 0 ? after.slice(cut + 2) : '';
      block.split('\n').forEach((line) => {
        const m = line.match(/^“引用\d+” /);
        if (m) out.quotes.push(line.slice(m[0].length));
        else if (out.quotes.length) out.quotes[out.quotes.length - 1] += '\n' + line; // 全文里的换行
      });
      hit = out.quotes.length > 0 || hit;
    } else {
      const cut = after.indexOf('”\n\n');
      if (cut >= 0) { out.quotes = [after.slice(1, cut)]; out.rest = after.slice(cut + 3); hit = true; }
      else if (after.slice(-1) === '”') { out.quotes = [after.slice(1, -1)]; out.rest = ''; hit = true; }
    }
  }
  return hit ? out : null;
}

// 气泡正文渲染：附件块逐行渲染灰色附件行（与引用行同款：CSS 截断 + title 全文），
// 引用块拆成灰色引用行（截断 + title 全文），正文里“引用N”与对得上号的文件标记
// 渲染成灰色胶囊
function appendQuotedBody(b, raw, mdBody) {
  const qm = splitQuoted(raw);
  // 没有任何块：正文整段落位（普通消息走标记切段兜底，mdBody 时整段 MD 渲染）
  if (!qm) { appendBodyText(b, raw, [], [], mdBody); return; }
  qm.attNotes.forEach((n) => {
    const line = document.createElement('span');
    line.className = 'msg-quote';
    line.textContent = n;
    line.title = n;
    b.appendChild(line);
  });
  qm.quotes.forEach((q, i) => {
    const line = document.createElement('span');
    line.className = 'msg-quote';
    line.textContent = quoteMark(i + 1) + ' ' + quoteClip(q);
    line.title = q;
    b.appendChild(line);
  });
  appendBodyText(b, qm.rest, qm.quotes, qm.attNotes, mdBody);
}

// 正文渲染分流：mdBody=true（aiuser：任务书/派发回报/待办转交）时整段按
// Markdown 渲染——这些消息天然带 MD 结构（模型正文习惯：列表/表格/路径），
// 渲染器与数字助理正文同款（md()，见 app-core.js）；否则走「引用N/文件标记→
// 灰胶囊」的用户消息切段（那是用户输入语法，aiuser 正文不依赖，两分支不交叉）
function appendBodyText(b, text, quotes, attNotes, mdBody) {
  if (mdBody) {
    const src = String(text == null ? '' : text);
    if (!src) return;
    const box = document.createElement('div');
    box.innerHTML = md(src);
    b.appendChild(box);
    return;
  }
  appendQuoteBodyText(b, text, quotes, attNotes);
}

// 正文按标记切段：引用标记与文件名形态的引号串（以 .扩展名 结尾，如“报告fina….docx”）
// 都渲染成灰胶囊；其余原样文本节点。两类都走“形状命中 + 对号”两步：引用对引用块
// 的第 N 条；文件对附件块里《全名》的截短（fileClipName(全名) === 标记内文）。
// 对不上号的（引用已清掉、附件块缺失、用户手打的文件名引号）一律按普通文字
// 展示，不硬猜——气泡渲染只看消息文本，这里是纯视觉层
function appendQuoteBodyText(b, text, quotes, attNotes) {
  const src = String(text == null ? '' : text);
  if (!src) return;
  const notes = attNotes || [];
  const rx = /“(引用\d+|[^”\n]*\.[A-Za-z]{1,8})”/g;
  let last = 0, m;
  while ((m = rx.exec(src))) {
    if (m.index > last) b.appendChild(document.createTextNode(src.slice(last, m.index)));
    const qm = m[1].match(/^引用(\d+)$/);
    if (qm) {
      const full = quotes[Number(qm[1]) - 1];
      if (full == null) {
        b.appendChild(document.createTextNode(m[0])); // 对不上号的引用标记：按普通文字展示
      } else {
        const chip = document.createElement('span');
        chip.className = 'quote-chip';
        chip.textContent = m[0];
        chip.title = full;
        b.appendChild(chip);
      }
    } else {
      // 文件标记：在附件块里找《全名》截短后与标记内文一致的那条注记
      const inner = m[1];
      let matched = null;
      for (const n of notes) {
        const nm = n.match(/《([^》]+)》/);
        if (nm && fileClipName(nm[1]) === inner) { matched = n; break; }
      }
      if (!matched) b.appendChild(document.createTextNode(m[0]));
      else {
        const chip = document.createElement('span');
        chip.className = 'quote-chip';
        chip.textContent = m[0];
        chip.title = matched;
        b.appendChild(chip);
      }
    }
    last = rx.lastIndex;
  }
  if (last < src.length) b.appendChild(document.createTextNode(src.slice(last)));
}

// 胶囊条渲染：有引用才出现（hidden 零占位）；1 条显示截断文，多条收成「引用了N条」。
// 悬停明细不用原生 title（浏览器黄色小框、触发慢、样式老）：胶囊条正上方挂一个同宽
// 展开面板，逐条列「引用N + 全文」，纯 CSS :hover 即时展开（style.css .q-expand）
function renderQuote(view) {
  view = view || mainView;
  const bar = viewEl(view, 'quote-bar');
  if (!bar) return;
  const qs = quotesOf(view);
  bar.classList.toggle('hidden', !qs.length);
  const ex = bar.querySelector('.q-expand');
  if (!qs.length) { if (ex) ex.textContent = ''; return; }
  const tx = bar.querySelector('.q-text');
  if (tx) tx.textContent = qs.length === 1
    ? '\u201c' + quoteClip(qs[0]) + '\u201d'
    : '引用了 ' + qs.length + ' 条';
  if (ex) {
    ex.textContent = '';
    qs.forEach((q, i) => {
      const row = document.createElement('div');
      row.className = 'qe-row';
      const mark = document.createElement('span');
      mark.className = 'qe-mark';
      mark.textContent = quoteMark(i + 1);
      const body = document.createElement('span');
      body.className = 'qe-text';
      body.textContent = q; // 面板里放全文（折行显示），看完整引用不必再点开别处
      row.appendChild(mark);
      row.appendChild(body);
      ex.appendChild(row);
    });
  }
}

// 在视图输入框光标处插入一段标记文本（“引用N”/附件截短名引号串共用）：输入框有焦点就
// 插在光标处，否则追加到末尾；插完聚焦、光标跟在标记后（接着打字即续写）。
// 落盘由调用方收口（一次插多条时只落一次）
function insertInputMark(view, mark) {
  const inp = view.input;
  const p = (document.activeElement === inp && typeof inp.selectionStart === 'number')
    ? inp.selectionStart : inp.value.length;
  inp.value = inp.value.slice(0, p) + mark + inp.value.slice(p);
  inp.focus();
  inp.setSelectionRange(p + mark.length, p + mark.length);
  autoGrow(inp);
}

// 文件名截断：主名前 6 个字符 + … + 完整扩展名（“报告final版V3.docx”→“报告fin….docx”），
// 够短就原样返回；按码点截不劈中文
function fileClipName(name) {
  const s = String(name || '');
  const i = s.lastIndexOf('.');
  const ext = (i > 0 && i >= s.length - 7) ? s.slice(i) : '';
  const cps = Array.from(ext ? s.slice(0, i) : s);
  const head = cps.length <= 6 ? cps.join('') : cps.slice(0, 6).join('') + '…';
  return head + ext;
}

// 附件的输入框标记（“报告fina….docx”，主名前 6 字 + … + 扩展名）：完整标记文本
// 存在附件对象 a.mark 上——背衬层按它精确高亮、卡片 ✕ 按它从输入框摘掉。不带
// “文件N”编号：用户自己传的文件认得名字，胶囊越短越好。标记原样随正文发出
// （【文件上传】注记带全名，模型自己对上号）
function attachMarkOf(a) {
  a.mark = '\u201c' + fileClipName(a.name) + '\u201d';
  return a.mark;
}

// 输入框标记区间：`{start, end, owner}`，owner 是认领这一段的附件对象（引用标记为 null）。
// 附件标记按“一份附件认领一处”从左到右一对一分配，**不做“按文本找全部出现”**：
// 同名文件（微信相机 wx_cam….jpg 两张最常见）在正文里就是两段一模一样的文字，逐份
// 去扫全部出现会把同一段重复认领 n 次（n 份同名 → n×n 颗胶囊），背衬层比输入框长出
// 好几行，灰底就画到正文上（手机端「胶囊撑长、盖住用户的话」就是这么来的）。
// 多出来的同形文字（用户自己打的引号）认不到 owner，自然不当胶囊。
function inputMarkRanges(value, list) {
  const v = String(value || '');
  const out = [];
  const qr = /“引用\d+”/g;
  let m;
  while ((m = qr.exec(v))) out.push({ start: m.index, end: m.index + m[0].length, owner: null });
  const want = new Map(); // 标记文本 → 还要认领它的附件队列（按入列顺序）
  for (const a of list) {
    if (!a.mark) continue;
    const q = want.get(a.mark);
    if (q) q.push(a); else want.set(a.mark, [a]);
  }
  if (want.size) {
    // 长的优先：一条标记是另一条的前缀时，先试长的那条（如 a.jpg 与 a.jpg….jpg）
    const pat = Array.from(want.keys()).sort((x, y) => y.length - x.length)
      .map((s) => s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')).join('|');
    const re = new RegExp(pat, 'g');
    while ((m = re.exec(v))) {
      const q = want.get(m[0]);
      if (!q || !q.length) continue; // 队列空了：多出来的同形文字，不算胶囊
      out.push({ start: m.index, end: m.index + m[0].length, owner: q.shift() });
    }
  }
  out.sort((x, y) => x.start - y.start);
  return out;
}

// 背衬层的落 DOM 数据：把值按标记区间切成 [{text, mark}] 段。拼起来必须与原值
// 逐字相同——背衬层只有与输入框逐字重合，灰底才是“精确垫底”，否则就画到别人身上。
function markBackdropSegments(value, ranges) {
  const v = String(value || '');
  const out = [];
  let last = 0;
  for (const r of ranges) {
    if (r.start < last) continue; // 防御：区间交错时只认先到的，宁少画不错画
    if (r.start > last) out.push({ text: v.slice(last, r.start), mark: false });
    out.push({ text: v.slice(r.start, r.end), mark: true });
    last = r.end;
  }
  if (last < v.length) out.push({ text: v.slice(last), mark: false });
  return out;
}

// 挂上引用（各引用入口统一走这里）：累计进引用列表（同文再次引用不新增条目、
// 复用原序号），在输入框光标处插入“引用N”标记，落盘并聚焦
function setQuote(view, text) {
  view = view || mainView;
  const t = String(text || '').trim();
  if (!t) return;
  const qs = quotesOf(view);
  let idx = qs.indexOf(t); // 同一段文字又引了一次：只在光标处再插一个标记，指向原条
  if (idx < 0) { qs.push(t); idx = qs.length - 1; }
  renderQuote(view);
  persistQuoteLS(view);
  insertInputMark(view, quoteMark(idx + 1));
  if (view === mainView) persistMainDraftLS(); else persistSubDraftLS(); // 程序化改值也要落盘
  syncComposerPad(view); // 胶囊出现改变输入区高度：重算悬浮垫高
}

// 移除引用（胶囊 ✕ / 发送清理）
function clearQuote(view) {
  view = view || mainView;
  if (!quotesOf(view).length) return;
  view.state.quotes = [];
  renderQuote(view);
  persistQuoteLS(view);
  syncComposerPad(view);
}

// 引用随草稿落盘：首页并入 jx.homeDraft，主对话 jx.q.<id>，子对话 jx.subq.<id>
function persistQuoteLS(view) {
  view = view || mainView;
  const qs = quotesOf(view);
  try {
    if (view === mainView && !view.state.cur) { homeDraft.quote = qs.slice(); persistHomeDraftLS(); return; }
    if (!view.state.cur) return;
    const key = view === mainView ? 'jx.q.' + view.state.cur.id : 'jx.subq.' + view.state.cur.id;
    if (qs.length) localStorage.setItem(key, JSON.stringify(qs));
    else localStorage.removeItem(key);
  } catch (e) { /* 隐私模式下忽略 */ }
}

// 落盘值归一：新版 JSON 数组 / 旧版单条字符串 → 字符串数组
function parseQuotesLS(v) {
  if (Array.isArray(v)) return v.filter((x) => typeof x === 'string' && x.trim());
  if (typeof v === 'string' && v.trim()) return [v];
  return [];
}

// 从草稿恢复引用（切换会话 / 回首页 / 刷新启动；子对话落地处也手动调）
function restoreQuoteLS(view) {
  view = view || mainView;
  let raw = null;
  try {
    if (view === mainView) {
      raw = view.state.cur ? localStorage.getItem('jx.q.' + view.state.cur.id) : (homeDraft.quote || null);
    } else if (view.state.cur) {
      raw = localStorage.getItem('jx.subq.' + view.state.cur.id);
    }
  } catch (e) {}
  if (typeof raw === 'string' && raw.charAt(0) === '[') { try { raw = JSON.parse(raw); } catch (e) {} }
  view.state.quotes = parseQuotesLS(raw);
  view.state.quote = '';
  renderQuote(view);
}

// 引用胶囊的移除按钮（主对话与子对话各一条）：清引用时把输入框里的“引用N”标记
// 一并摘掉；但退回消息回填的内容自带「用户引用：」段（标记指着消息里的引用块），
// 不动它的正文
[mainView, subView].forEach((v) => {
  const bar = viewEl(v, 'quote-bar');
  if (!bar) return;
  bar.addEventListener('click', (e) => {
    if (!e.target.closest('.q-x')) return;
    if (v.input.value.indexOf(QUOTE_HEAD) < 0) {
      v.input.value = v.input.value.replace(/“引用\d+”/g, '');
      autoGrow(v.input);
      if (v === mainView) persistMainDraftLS(); else persistSubDraftLS();
    }
    clearQuote(v);
    v.input.focus();
  });
});

/* ---------- 输入框标记胶囊：背衬层渲染 + 光标原子策略 ----------
   引用标记（“引用N”）与附件标记（“报告fina….docx”）在输入框里是纯文本（textarea
   装不下富文本），灰胶囊观感由背衬层实现（HTML/CSS 见 index.html 的
   composer-input-wrap 与 style.css）：与输入框同排版的透明层垫在输入框底下（字体/
   行高/内边距/换行规则从输入框复制，同语音灰字层的手法），标记段画灰底圆角色块
   ——黑字仍是输入框自己的、叠在最上层正常编辑，胶囊只是精确垫底的底色（两层渲染
   同一段文本，逐字重合）；pointer-events:none 永不吃点击。
   标记区间怎么认：引用标记按 /“引用\d+”/ 正则；附件标记按“一份附件认领一处”
   （inputMarkRanges 从左到右一对一分配）——不猜格式、也不按文本反查出现次数：同名
   文件的那两段文字本来就一模一样，按文本扫会重复认领，灰底就画到正文上。草稿恢复后
   没有对应附件就自然不高亮，只是普通文字。
   防误改中间：光标落进标记内部弹到标记末尾；退格贴着标记末尾 / Delete 贴着标记
   开头整颗删除（优先 execCommand 保撤销栈）；选区完整落在一颗标记内扩成整颗。
   IME 组字期间一律不干预。程序化写值（语音落字、退回回填、清空等）经 value
   存取器拦截统一刷新背衬层，不必挨个调用点补。 */
function setupInputMarks(view) {
  const ta = view.input;
  const wrap = ta.closest('.composer-input-wrap');
  const bd = wrap && wrap.querySelector('.mark-backdrop');
  if (!bd) return;

  // 排版对齐：从输入框复制 computed style（composer 字体静态，初始化与窗口缩放时同步）
  function syncMetrics() {
    const cs = getComputedStyle(ta);
    ['font-size', 'font-family', 'font-weight', 'line-height', 'letter-spacing',
      'padding', 'box-sizing', 'white-space', 'word-break', 'overflow-wrap', 'text-align']
      .forEach((k) => { bd.style[k] = cs[k]; });
  }

  // 标记区间（引用标记 + 附件标记的认领结果，升序；见 inputMarkRanges）
  function markList() { return inputMarkRanges(ta.value, attachmentsOf(view)); }

  // 渲染：按标记切段，标记段灰底胶囊、其余透明文本；没有标记就整层腾空（零开销）
  function render() {
    bd.textContent = '';
    for (const seg of markBackdropSegments(ta.value, markList())) {
      if (!seg.mark) { bd.appendChild(document.createTextNode(seg.text)); continue; }
      const sp = document.createElement('span');
      sp.className = 'mk';
      sp.textContent = seg.text;
      bd.appendChild(sp);
    }
    syncBdScroll(); // 重建会把滚动归零：按输入框当前 scrollTop 对齐（内容流位置）
  }
  let raf = 0;
  function schedule() { if (!raf) raf = requestAnimationFrame(() => { raf = 0; render(); }); }

  // value 存取器：程序化写值也触发刷新（原生键入走下方 input 事件）。拦不住的
  // 极端环境不影响功能，只是背衬层要等下一次键入才跟上
  try {
    const desc = Object.getOwnPropertyDescriptor(HTMLTextAreaElement.prototype, 'value');
    Object.defineProperty(ta, 'value', {
      get() { return desc.get.call(this); },
      set(v) { desc.set.call(this, v); schedule(); },
      configurable: true,
    });
  } catch (e) { /* 老内核：靠 input 事件兜底 */ }

  ta.addEventListener('input', schedule);
  // 内容超高时输入框自己滚：背衬层用 scrollTop 镜像跟着滚——裁剪窗（盒子本身
  // 的 overflow:hidden，钉在输入框位置）不动、只滚内容，胶囊与文字同步进出
  // 可视区。绝不能平移盒子本身（曾用 translateY：裁剪窗跟着上移逃出输入卡，
  // 超长输入时灰胶囊悬空浮在输入区上方——裁剪窗脱离了该管它的上下文）。
  ta.addEventListener('scroll', syncBdScroll);
  function syncBdScroll() { bd.scrollTop = ta.scrollTop; }
  window.addEventListener('resize', () => { syncMetrics(); render(); });
  syncMetrics();
  render();

  // —— 光标原子策略（markList 见上） ——
  let composing = false;
  ta.addEventListener('compositionstart', () => { composing = true; });
  ta.addEventListener('compositionend', () => { composing = false; schedule(); });

  // 整颗删除（保撤销栈：选中范围后插入空串；引擎不接则回落 setRangeText）
  function deleteRange(st, en) {
    ta.focus();
    ta.setSelectionRange(st, en);
    if (!document.execCommand('insertText', false, '')) {
      ta.setRangeText('', st, en, 'end');
      ta.dispatchEvent(new Event('input', { bubbles: true }));
    }
  }
  ta.addEventListener('keydown', (e) => {
    if (composing || e.isComposing) return;
    if (ta.value.indexOf('“') < 0 || ta.selectionStart !== ta.selectionEnd) return;
    const p = ta.selectionStart;
    const list = markList();
    if (e.key === 'Backspace') {
      for (const mk of list) if (mk.end === p) { e.preventDefault(); deleteRange(mk.start, mk.end); return; }
    } else if (e.key === 'Delete') {
      for (const mk of list) if (mk.start === p) { e.preventDefault(); deleteRange(mk.start, mk.end); return; }
    }
  });
  // 光标落进标记内部 → 弹到标记末尾；选区完整落在单颗内 → 扩成整颗（双击选词等）
  document.addEventListener('selectionchange', () => {
    if (document.activeElement !== ta || composing) return;
    if (ta.value.indexOf('“') < 0) return;
    const s = ta.selectionStart, e = ta.selectionEnd;
    if (s !== e) {
      for (const mk of markList()) {
        if (s >= mk.start && e <= mk.end && (s !== mk.start || e !== mk.end)) {
          ta.setSelectionRange(mk.start, mk.end); return;
        }
      }
      return;
    }
    for (const mk of markList()) {
      if (s > mk.start && s < mk.end) { ta.setSelectionRange(mk.end, mk.end); return; }
    }
  });
}

// 仅识别主对话区与右栏子对话区里的文字选区
function chatSelection() {
  const sel = window.getSelection();
  if (!sel || sel.isCollapsed || sel.rangeCount === 0) return null;
  const text = sel.toString().trim();
  if (!text) return null;
  const n = sel.anchorNode;
  const anchor = n && (n.nodeType === 1 ? n : n.parentElement);
  if (!anchor || !anchor.closest('#messages, #sub-messages')) return null;
  const rect = sel.getRangeAt(0).getBoundingClientRect();
  if (!rect.width && !rect.height) return null;
  return { text, rect };
}

function hideSelPop() { $('sel-pop').classList.add('hidden'); }

function showSelPop() {
  const s = chatSelection();
  if (!s) { hideSelPop(); return; }
  selKeepText = s.text;
  const pop = $('sel-pop');
  pop.classList.remove('hidden');
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let x = s.rect.left + s.rect.width / 2 - pw / 2;
  x = Math.max(8, Math.min(x, window.innerWidth - pw - 8));
  let y = s.rect.top - ph - 8;
  if (y < 8) y = s.rect.bottom + 8;
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
}

// 选中文字送去右侧辅助子对话：不切主对话、不自动发送（填入子对话输入框由用户发出）
async function askInAux(text) {
  if (!state.cur) return;
  rpOpen('subs');
  // 复用已有的子对话（人工建的都算，含改造前建的老 aux）
  let aux = subKids.find((s) => s.mode === 'aux' || s.origin === 'human');
  if (!aux) {
    const fid = drawerFamilyId();
    if (!fid) return;
    try {
      aux = await api('/api/sessions', {
        method: 'POST',
        body: JSON.stringify({ parent: fid, title: '子对话', inheritContext: true }),
      });
      await refreshDrawer();
    } catch (e) {
      alert('新建子对话失败：' + e.message);
      return;
    }
  }
  await openSubSession(aux.id);
  setQuote(subView, text); // 挂成右栏输入卡的引用胶囊：显示截断、发送组装完整
}

/* ---------- 模型选择（发送按钮左侧的芯片 + 弹出面板） ---------- */

async function loadModels() {
  try {
    const d = await api('/api/models');
    state.models = d.models || [];
    state.defaultProvider = d.defaultProvider || '';
    state.defaultModel = d.defaultModel || '';
    state.companyQuota = d.companyQuota || null; // 公司通道额度（服务端随公告下发）
    syncComposer();
  } catch (e) { state.models = []; }
}

// 模型菜单的 key：providerId|keyId|modelId —— 中间那段是「模型归属哪把 Key」
// （自定义供应商多 Key 后，同名模型只能靠 Key 归属区分；单 Key 供应商为空）。
function modelKeyOf(provider, providerKey, model) {
  return provider + '|' + (providerKey || '') + '|' + model;
}

function currentModelKey(view) {
  const cur = (view || mainView).state.cur;
  if (cur) {
    return cur.provider && cur.model ? modelKeyOf(cur.provider, cur.providerKey, cur.model) : '__default__';
  }
  return homeDraft.provider && homeDraft.model
    ? modelKeyOf(homeDraft.provider, homeDraft.providerKey, homeDraft.model) : '__default__';
}

function modelNameOf(key) {
  if (key === '__default__') return state.defaultModel || '默认模型';
  const [pid, kid, mid] = key.split('|');
  const hit = (state.models || []).find((m) => m.providerId === pid && (m.keyId || '') === kid && m.modelId === mid)
    || (state.models || []).find((m) => m.providerId === pid && m.modelId === mid);
  return (hit && (hit.modelName || hit.modelId)) || mid;
}

// 思考档位：全系统固定六档，与模型、渠道协议无关（任何模型都能展开同一套，
// 由用户自己选；服务端认不认、能自动降级就降级，报错由用户自己换档）。
// 内部值 = 发给模型的值，显示名按官方口径。
const THINK_LEVELS = ['none', 'low', 'medium', 'high', 'xhigh', 'max'];
// 显示名与内部值分开：内部值（发给后端、落库的）永远是 thinkingLevels 里的小写，
// 显示名按官方口径首字母大写——第五档显示 Xhigh（不是内部值的小写 xhigh，也不用旧版长名）。
const THINK_LABELS = { none: 'None', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Xhigh', max: 'Max' };
// 跟随服务端默认（thinking 为空串）时芯片与卡片大字显示的文案
const THINK_DEFAULT_LABEL = 'Default';

// 老草稿里的档位（上一代的 disabled/enabled）归一，与后端 normalizeThinkingLevel 同口径：
// disabled → none、enabled（只开关不指深浅）→ 空（跟随服务端默认）、认不出的清空。
function normThinkLevel(lv) {
  const v = String(lv || '').trim().toLowerCase();
  if (!v || v === 'enabled') return '';
  if (v === 'disabled') return 'none';
  return THINK_LEVELS.includes(v) ? v : '';
}

// 当前生效的思考档位（会话级 / 首页草稿 / 指定视图的会话）
function curThinking(view) {
  const cur = (view || mainView).state.cur;
  return cur ? (cur.thinking || '') : (homeDraft.thinking || '');
}

// pickModel 选中一个模型：会话 PATCH provider/providerKey/model，首页写进草稿。
// providerKey 是模型归属的那把 Key——重名模型靠它落到正确的密钥上。
// 只发模型三件套：换模型不动思考档位（thinking 不在 body 里）。返回是否成功。
async function pickModel(key, view) {
  view = view || mainView;
  const parts = key === '__default__' ? ['', '', ''] : key.split('|');
  const provider = parts[0] || '', providerKey = parts[1] || '', model = parts[2] || '';
  if (view.state.cur) {
    try {
      view.state.cur = await api('/api/sessions/' + view.state.cur.id, {
        method: 'PATCH',
        body: JSON.stringify({ provider, providerKey, model }),
      });
    } catch (e) { mcShowError('切换模型失败：' + e.message); return false; }
  } else if (view === mainView) {
    homeDraft.provider = provider;
    homeDraft.providerKey = providerKey;
    homeDraft.model = model;
  }
  syncComposer(view);
  return true;
}

// 设置思考档位（空串=跟随服务端默认）。卡片不收起：滑杆是连续调节控件，调一档就
// 收卡片等于让用户反复点开。只发 thinking 一个字段——改档位不动模型。
async function setThinking(level, view) {
  view = view || popView || mainView;
  level = normThinkLevel(level);
  if (level) rememberManualThink(level);
  if (view.state.cur) {
    try {
      view.state.cur = await api('/api/sessions/' + view.state.cur.id, { method: 'PATCH', body: JSON.stringify({ thinking: level }) });
    } catch (e) {
      mcShowError('设置思考档位失败：' + e.message); // 就地提示，不静默失败
      renderModelCard(view);                        // 回到真实状态，不留乐观态
      return;
    }
  } else if (view === mainView) {
    homeDraft.thinking = level;
    persistHomeDraftLS();
  }
  renderModelCard(view);
  syncComposer(view);
}

/* ---------- 模型与思考档位卡片（一卡两层，入口是输入框下方那枚芯片） ----------
   第一层：居中档位大字（主题色）+ 居中的模型行（点它翻到第二层）+ 渐变滑杆
           + 「跟随服务端默认」开关 + 右上角刷新（重新拉 GET /api/models，不是重置档位）；
   第二层：按供应商分组的模型列表（灰色小字标题 + 缩进列模型，当前模型带勾），
           选中即切会话模型并翻回第一层。
   设计要点（都是用户敲定的口径）：
   · 档位只有滑杆这一个入口，模型菜单里的档位二级菜单已删；两者互不影响——
     改档位只发 thinking，换模型只发 provider/providerKey/model；
   · 拖动只实时改大字预览，松手才 PATCH（一次）；点轨道＝跳档并立即提交；
     键盘左右各一档、立即提交；
   · 六档对所有模型都可用（后端白名单与模型无关），所以滑杆永远在，不按模型隐藏；
   · 「跟随服务端默认」= thinking 空串：滑杆隐藏、大字显示 Default，关掉恢复上次手选档位。 */
const MC_TRACK_PAD = 9;   // 滑杆轨道左右内边距（须与 style.css 的 .mc-slider padding 一致）
const MC_CHECK = '<svg class="mc-check" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.4" stroke-linecap="round" stroke-linejoin="round" aria-hidden="true"><path d="M5 12.5l4.5 4.5L19 7"/></svg>';
let mcLayer = 1;          // 卡片当前层：1=档位，2=模型列表
let mcDrag = null;        // 拖动中：{ pid, idx }——松手才提交
let mcBusy = false;       // 刷新模型列表进行中（loading 态）
// 「跟随服务端默认」关掉后要恢复的档位：用户上一次手选的（本机记忆，没有就用 Medium）
let lastManualThink = normThinkLevel(lsGet('jx.thinkLast', '')) || 'medium';

function thinkLabel(level) { return level ? (THINK_LABELS[level] || level) : THINK_DEFAULT_LABEL; }

function rememberManualThink(level) {
  if (!THINK_LEVELS.includes(level)) return;
  lastManualThink = level;
  lsSet('jx.thinkLast', level);
}

// 卡片里的错误条（刷新失败/切档失败都在这里现身；空串＝收起）
function mcShowError(msg) {
  const el = $('mc-err');
  if (!el) return;
  el.textContent = msg || '';
  el.classList.toggle('hidden', !msg);
}
function mcHideError() { mcShowError(''); }

// 打开卡片。anchor 是那枚芯片：主对话区 #model-chip、右栏子对话 #sub-model-chip，
// 两处共用同一张卡，popView 记住它属于哪个视图。
function openModelCard(view) {
  popView = view || mainView;
  mcLayer = 1;
  mcHideError();
  renderModelCard(popView);
  openPopover('model-card', viewEl(popView, 'model-chip'));
}

// 按当前会话/草稿状态重画整张卡（第一层 + 第二层列表）
function renderModelCard(view) {
  view = view || popView || mainView;
  const lv = curThinking(view);         // '' = 跟随服务端默认
  const follow = !lv;
  const shown = follow ? lastManualThink : lv;
  const idx = Math.max(0, THINK_LEVELS.indexOf(shown));
  $('mc-big').textContent = follow ? THINK_DEFAULT_LABEL : thinkLabel(lv);
  $('mc-model-name').textContent = modelNameOf(currentModelKey(view));
  $('mc-follow').checked = follow;
  $('model-card').classList.toggle('following', follow);
  $('mc-hint').textContent = follow ? '由服务端决定推理强度' : '拖动调节推理强度';
  const slider = $('mc-slider');
  slider.setAttribute('aria-disabled', follow ? 'true' : 'false');
  slider.tabIndex = follow ? -1 : 0;    // 跟随默认时滑杆不可聚焦（键盘也调不了）
  mcSetSlider(idx, follow ? THINK_DEFAULT_LABEL : thinkLabel(lv));
  mcShowLayer(mcLayer);
  mcRenderList(view);
}

// 滑杆几何：六等分，第 idx 档落在 idx/5 处；填充条与滑块圆心对齐同一位置
function mcSetSlider(idx, text) {
  const n = THINK_LEVELS.length - 1;
  const p = n > 0 ? idx / n : 0;
  $('mc-fill').style.width = (p * 100).toFixed(2) + '%';
  $('mc-thumb').style.left = 'calc(' + MC_TRACK_PAD + 'px + (100% - ' + MC_TRACK_PAD * 2 + 'px) * ' + p.toFixed(4) + ')';
  const s = $('mc-slider');
  s.setAttribute('aria-valuenow', String(idx));
  s.setAttribute('aria-valuetext', text);
}

function mcShowLayer(n) {
  mcLayer = n;
  $('mc-lv').classList.toggle('hidden', n !== 1);
  $('mc-models').classList.toggle('hidden', n !== 2);
}

// 指针横坐标 → 档位下标（夹取在轨道两端之间，六等分就近取整）
function mcLevelAt(clientX) {
  const r = $('mc-track').getBoundingClientRect();
  const n = THINK_LEVELS.length - 1;
  if (r.width <= 0) return 0;
  const x = Math.min(Math.max(clientX - r.left, 0), r.width);
  return Math.round((x / r.width) * n);
}

// 拖动中的预览：只改大字与滑杆位置，不发请求（松手才提交）
function mcPreview(idx) {
  if (!mcDrag) return;
  mcDrag.idx = idx;
  const text = thinkLabel(THINK_LEVELS[idx]);
  $('mc-big').textContent = text;
  mcSetSlider(idx, text);
}

// 卡片被关掉（Esc 等）时丢掉未松手的拖动：松手前关闭不提交中间态
function mcCancelDrag() { mcDrag = null; }

// 第二层：按供应商分组的模型列表——灰色小字分组标题 + 下面缩进列该供应商的模型。
// 因为已经分组，重名模型不再加渠道后缀；不放「默认模型」这类条目。
function mcRenderList(view) {
  const list = $('mc-list');
  const cur = currentModelKey(view || popView || mainView);
  // 老会话只存了模型名（没有 Key 归属）时按「同渠道同模型的第一项」认勾选，
  // 免得列表里一个勾都没有
  let curKey = cur;
  if (cur !== '__default__') {
    const p = cur.split('|');
    if (!p[1]) {
      const hit = (state.models || []).find((m) => m.providerId === p[0] && m.modelId === p[2]);
      if (hit) curKey = modelKeyOf(hit.providerId, hit.keyId, hit.modelId);
    }
  }
  let h = '';
  let lastProv = '';
  for (const m of state.models || []) {
    // 按供应商分组：灰色小字标题（不可点、不带勾）+ 下面缩进列该供应商的模型。
    // 顺序就是 /api/models 的返回顺序（与「设置 · 模型」里的供应商顺序一致）。
    if (m.providerId !== lastProv) {
      h += '<div class="pop-group mc-group">' + esc(m.providerName || m.providerId) + (m.providerOff ? '（已停用）' : '') + '</div>';
      lastProv = m.providerId;
    }
    const k = modelKeyOf(m.providerId, m.keyId, m.modelId);
    const on = k === curKey;
    // 已按供应商分组，不再给重名模型加渠道后缀（同名模型各在自己渠道下面）
    h += '<button type="button" class="pop-item mc-item' + (on ? ' sel' : '') + (m.providerOff ? ' off' : '') + '"'
      + ' data-key="' + esc(k) + '" title="' + esc(m.modelId) + '">'
      + '<span>' + esc(m.modelName || m.modelId) + '</span>' + (on ? MC_CHECK : '') + '</button>';
  }
  if (!h && !mcBusy) h = '<div class="mc-empty">还没有可用模型（去 设置 · 模型 添加供应商）</div>';
  const keepTop = list.scrollTop;
  list.innerHTML = (mcBusy ? '<div class="mc-empty">正在刷新模型列表…</div>' : '') + h;
  list.scrollTop = keepTop;
  list.classList.toggle('busy', mcBusy);
  list.querySelectorAll('.mc-item').forEach((b) => { b.onclick = () => mcPickModel(b.dataset.key); });
}

// 点模型：立即切会话模型（PATCH /api/sessions/{id}），档位保持不变，然后翻回第一层
async function mcPickModel(key) {
  if (!key) return;
  const ok = await pickModel(key, popView);
  if (!ok) { renderModelCard(popView); return; } // 失败留在列表里改选，错误条已给出原因
  mcShowLayer(1);
  renderModelCard(popView);
}

// 右上角刷新：重新拉 GET /api/models 并重画第二层列表；期间转圈，失败给出可见提示
async function mcRefreshModels() {
  if (mcBusy) return;
  mcBusy = true;
  mcHideError();
  const btn = $('mc-refresh');
  btn.classList.add('spin');
  btn.disabled = true;
  mcRenderList(popView); // 第二层当场显示加载态（刷新常在第一层点，但列表可能正开着）
  try {
    const d = await api('/api/models');
    state.models = d.models || [];
    state.defaultProvider = d.defaultProvider || '';
    state.defaultModel = d.defaultModel || '';
    state.companyQuota = d.companyQuota || null;
  } catch (e) {
    mcShowError('刷新模型列表失败：' + e.message); // 不静默失败
  } finally {
    mcBusy = false;
    btn.classList.remove('spin');
    btn.disabled = false;
    renderModelCard(popView);
    syncComposer(popView); // 芯片上的模型名可能已变（模型清单刷新后重名后缀/命名跟着变）
  }
}

/* ---------- 弹出面板系统：加号菜单 / 权限面板 / 模型面板 / 目录面板 ---------- */

const PERM_LABELS = { 'read-only': '全电脑只读', 'workspace-write': '修改工作区', 'full': '完全访问' };
/* 窄屏（手机）输入工具栏一行排时权限芯片的短名（chip 的 data-short，CSS 换显，
   机制同左侧栏入口按钮）：全名六七个字会把模型标签挤没，短名保住可读性 */
const PERM_SHORTS = { 'read-only': '只读', 'workspace-write': '改工作区', 'full': '完全' };
/* ---------- 模式前端定制点 ----------
   基础前端一套不变；工作模式要对界面做定向增补时，在自己 CHAT_MODES 那一项挂
   ui 对象：mount(view) 首次把控件挂进该视图（常驻 DOM，之后只切显隐），返回根
   元素；sync(view, s, el) 每次 syncComposer 跟着刷状态——在基础刷新之后执行，
   可以覆盖基础设置（如接管「计划」胶囊的显示）。applyModeUI 是唯一分发点：
   未挂 ui 的模式零影响，基础前端不认识任何具体控件，模式对主前端的改动代码
   全部收在模式自己那一项里。 */

// 编程模式定制：计划（先出方案再执行）在编码场景是高频操作，开关直接进工具栏
// ——权限芯片右侧一枚「计划」胶囊 + 小号苹果开关（.sw-mini），点按即切换计划
// 模式（复用 toggleModeFlag：与目标模式互斥、PATCH 会话或改首页草稿，失败回刷）
const codingModeUI = {
  mount(view) {
    const label = document.createElement('label');
    label.className = 'mflag mflag-toggle hidden';
    label.title = '计划模式：先出方案再执行';
    const box = document.createElement('input');
    box.type = 'checkbox';
    const sw = document.createElement('span');
    sw.className = 'sw-mini';
    sw.setAttribute('aria-hidden', 'true');
    label.append('计划', box, sw);
    box.addEventListener('change', () => toggleModeFlag('planMode', view));
    viewEl(view, 'perm-chip').after(label);
    return label;
  },
  // 开关自身已表达开/关状态：本模式下「计划✕」胶囊不再重复显示（目标胶囊不受影响）
  sync(view, s, el) {
    const box = el.querySelector('input');
    if (box) box.checked = !!s.planMode;
    viewEl(view, 'flag-plan').classList.remove('on');
    viewEl(view, 'mode-flags').classList.toggle('hidden', !viewEl(view, 'flag-goal').classList.contains('on'));
  },
};

/* ---------- 演示记录器（创建技能模式）：录制按钮，归用户直控 ----------
   模型不持有录制工具；按钮 POST /api/demo-record/* 起录/停录，材料以【演示记录】
   系统通知进对话（同文件上传机制）。状态唯一权威在后端单例，按钮只是视图：
   SSE demo_record_started/stopped 与 status 接口把所有窗口/视图收敛一致。 */
const demoRec = { supported: true, active: false, dir: '', loaded: false, busy: false };

function demoRecSyncBtn(btn) {
  if (!btn) return;
  btn.disabled = demoRec.loaded && !demoRec.supported;
  btn.classList.toggle('on', demoRec.active);
  btn.title = demoRec.active ? '停止录制并收录材料'
    : (demoRec.supported ? '演示记录：开始录制（操作时自动截图＋记事件＋录音）' : '演示记录（仅 Windows 桌面可用）');
  const play = btn.querySelector('.ico-play');
  const stop = btn.querySelector('.ico-stop');
  if (play) play.classList.toggle('hidden', demoRec.active);
  if (stop) stop.classList.toggle('hidden', !demoRec.active);
}

function demoRecLoadOnce() {
  if (demoRec.loaded) return;
  demoRec.loaded = true;
  api('/api/demo-record/status').then((st) => {
    demoRec.supported = !!st.supported;
    demoRec.active = !!st.active;
    demoRec.dir = st.dir || '';
    document.querySelectorAll('.rec-btn').forEach(demoRecSyncBtn);
  }).catch(() => { demoRec.loaded = false; }); // 失败可重试：下次挂载再拉
}

async function demoRecToggle(view) {
  if (demoRec.busy) return;
  const sess = (view.state && view.state.cur) || null; // 会话内停录：【演示记录】通知落进该会话；首页（未开会话）也能录
  const body = sess ? { session: sess.id } : {};
  demoRec.busy = true;
  try {
    if (demoRec.active) {
      const r = await api('/api/demo-record/stop', { method: 'POST', body: JSON.stringify(body) });
      demoRec.active = false;
      demoRec.dir = r.dir || demoRec.dir;
      toast(sess ? '演示已收录，回到对话让 AI 分析即可' : '已收录到 ' + (r.dir || '') + '，开个对话即可让 AI 分析');
    } else {
      const r = await api('/api/demo-record/start', { method: 'POST', body: JSON.stringify(body) });
      demoRec.active = true;
      demoRec.dir = r.dir || '';
      toast('开始录制：去电脑上正常操作并口头讲解，做完回来点停止');
    }
  } catch (e) {
    toast((demoRec.active ? '停止' : '开始') + '录制失败：' + e.message);
  }
  demoRec.busy = false;
  document.querySelectorAll('.rec-btn').forEach(demoRecSyncBtn);
}

// 创建技能模式定制：输入框工具条上一枚录制按钮（▶ 起录 / ■ 红底停止），
// 与「计划」开关同一插槽（权限芯片右侧）。首页与子对话视图都生效。
const skillCreateModeUI = {
  mount(view) {
    const btn = document.createElement('button');
    btn.type = 'button';
    btn.className = 'cbtn rec-btn';
    btn.innerHTML = '<svg class="ico-play" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><path d="M8 5v14l11-7z"/></svg>' +
      '<svg class="ico-stop hidden" viewBox="0 0 24 24" fill="currentColor" aria-hidden="true"><rect x="7" y="7" width="10" height="10" rx="1.5"/></svg>';
    btn.addEventListener('click', () => demoRecToggle(view));
    viewEl(view, 'perm-chip').after(btn);
    demoRecLoadOnce();
    demoRecSyncBtn(btn);
    return btn;
  },
  sync(view, s, el) { demoRecSyncBtn(el); },
};

// 新对话可选的工作模式（后端 systemPrompt/toolsFor 按 mode 分流）。
// **清单的唯一数据源在后端**（chatmodes.go 的 chatModes → GET /api/modes）：
// 值、中文名与一句话说明都由后端下发，启动时 loadChatModes() 重建本数组——
// 与数字助理 dispatch_task 的 mode 参数是同一份清单，加模式只改后端一处，
// 下拉与派发自然一致（与子对话专家清单同一取舍，见后端 experts.go 文件头）。
// 下面这份内置兜底只为接口不可达时页面仍能渲染（取名与后端保持一致）。
let CHAT_MODES = [
  { v: 'office', label: '标准办公', desc: '面向日常办公：文档、资料、表格、沟通文案，成果落文件' },
  { v: 'coding', label: '编程', desc: '面向软件开发：读改代码、编译构建、调试排障，成果直接落项目文件' },
  { v: 'beginner-coding', label: '新手编程', desc: '面向不懂编程的你：先问清需求再动手，全程大白话讲解，成果落项目文件' },
  { v: 'skill-create', label: '创建技能', desc: '把「怎么做」沉淀成给 AI 的教程：点按钮录演示让 AI 分析，或直接派活让 AI 探路总结；默认完全访问' },
];
// 模式前端定制点：工作模式要对界面做定向增补时，按**值**在这里挂 ui 对象
// ——mount(view) 首挂控件并返回根元素、sync(view, s, el) 随 syncComposer 刷
// 状态（应用点见 applyModeUI）。没挂 ui 的模式零影响：后端加一个模式，前端
// 一字不改就会出现在下拉里，要定制再回这里按值补。
const MODE_UI = {
  coding: codingModeUI,
  'beginner-coding': codingModeUI,
  'skill-create': skillCreateModeUI,
};
// loadChatModes 用后端下发的清单重建 CHAT_MODES（值/中文名/说明）并把前端
// 定制钩子按值挂回去；接口不可达时保留上面的内置兜底清单，页面照常可用。
async function loadChatModes() {
  try {
    const d = await api('/api/modes');
    const list = (d && d.modes) || [];
    if (!list.length) return;
    CHAT_MODES = list.map((m) => ({ v: m.value, label: m.label, desc: m.desc || '', ui: MODE_UI[m.value] }));
    renderModePop();
  } catch (e) { /* 接口不可达：留在内置兜底清单上 */ }
}
const chatModeOf = (v) => CHAT_MODES.find((m) => m.v === v) || { v: 'office', label: '标准办公', desc: '' };

// applyDraftMode：切换首页工作模式，带「创建技能」的权限联动——该模式要
// 看懂用户演示（截屏/键鼠记录）并亲自验证技能步骤，操作电脑/浏览器是刚需，
// 选中即默认完全访问（用户仍可在权限面板改回）；切去别的模式且权限还是
// 本联动自动设的完全访问时，还原为修改工作区。FULL_BUNDLED_TOOLS 的
// 勾选随权限档联动（与权限面板同一套规则）。
function applyDraftMode(v) {
  const fullByAuto = homeDraft.permission === 'full' && homeDraft._fullFromMode;
  if (v === 'skill-create' && homeDraft.permission !== 'full') {
    homeDraft._fullFromMode = true;
    homeDraft.permission = 'full';
    homeDraft.tools = [...new Set([...(homeDraft.tools || []), ...FULL_BUNDLED_TOOLS])];
  } else if (v !== 'skill-create' && fullByAuto) {
    homeDraft._fullFromMode = false;
    homeDraft.permission = 'workspace-write';
    homeDraft.tools = applyCapDefaults((homeDraft.tools || []).filter((t) => !FULL_BUNDLED_TOOLS.includes(t)), v, 'workspace-write');
  }
  homeDraft.mode = v;
}

// 模式下拉：选项列表 + 当前项打勾
function renderModePop() {
  const box = $('mode-pop-list');
  if (!box) return;
  box.innerHTML = '';
  for (const m of CHAT_MODES) {
    const b = document.createElement('button');
    b.type = 'button';
    b.className = 'pop-item' + (homeDraft.mode === m.v ? ' sel' : '');
    b.innerHTML =
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 3l1.9 5.1L19 10l-5.1 1.9L12 17l-1.9-5.1L5 10l5.1-1.9z"/></svg>' +
      '<span class="mp2-text"><b>' + esc(m.label) + '</b><i>' + esc(m.desc) + '</i></span>' +
      (homeDraft.mode === m.v ? '<span class="pop-check">✓</span>' : '');
    b.onclick = () => {
      applyDraftMode(m.v);
      syncComposer();
      closePopovers();
    };
    box.appendChild(b);
  }
}

function sessionOrDraftFor(view) {
  view = view || mainView;
  const cur = view.state.cur;
  if (cur) {
    return {
      permission: cur.permission || 'workspace-write',
      tools: cur.tools || [],
      planMode: !!cur.planMode,
      goalMode: !!cur.goalMode,
      goalText: cur.goalText || '',
    };
  }
  if (view === mainView) return homeDraft; // 主对话区没会话时 = 首页草稿
  // 右栏没选中子对话：给一份中性默认，避免串到首页草稿
  return { permission: 'workspace-write', tools: [], planMode: false, goalMode: false, goalText: '' };
}

function sessionOrDraft() { return sessionOrDraftFor(mainView); }

// 权限胶囊与能力菜单的唯一数据源是各自视图的 state.cur（permission / tools），
// 只有「重开这个会话」或「本视图自己发的 PATCH」才会重画它。于是两种真实场景
// 会看到胶囊停在旧档、菜单里却已是新值：
//   ① 审批卡「始终允许」把权限升级（或在别处/另一窗口改权限）——本窗口只收到广播，
//      原来那段同步逻辑还只在 state.streaming 为真时跑、且只同步主对话区，右栏
//      子对话的胶囊只能靠「切走再切回」才更新；
//   ② 同一个会话同时开在主对话区与右栏子对话——一边改完，另一边的胶囊不动
//      （PATCH 不发 session_updated 广播）。
// 下面两个函数把「已知的新权限」就地写进正在显示该会话的视图并重画输入区，
// 与流式状态无关，也不整包重取（胶囊是配置，不该等消息级刷新）。
function applySessionPermToViews(meta) {
  if (!meta || !meta.id) return false;
  let hit = false;
  for (const view of [mainView, subView]) {
    const cur = view.state.cur;
    if (!cur || cur.id !== meta.id) continue;
    if (meta.permission) cur.permission = meta.permission;
    if (Array.isArray(meta.tools)) cur.tools = meta.tools.slice();
    syncComposer(view); // 权限胶囊/模式徽标/能力菜单都按 state.cur 重画
    // 权限菜单正开着：勾选态与「完全访问捆绑项置灰」一并跟上，别让面板停在旧档
    if (view === popView && !$('perm-pop').classList.contains('hidden')) renderPermPop(view);
    hit = true;
  }
  return hit;
}

// 广播（session_updated / env 行）只带会话 id：从刚刷新的会话列表取权限两字段。
// 列表接口带 permission/tools（GET /api/sessions），比整包 GET /api/sessions/{id}
// 轻得多——后者会把整个消息历史再拉一遍。
function applySessionPermFromList(sessionId) {
  const m = (state.sessions || []).find((s) => s.id === sessionId);
  if (!m) return false;
  return applySessionPermToViews({ id: m.id, permission: m.permission, tools: m.tools });
}

async function patchDraftOrSession(fields, view) {
  view = view || mainView;
  const cur = view.state.cur;
  if (cur) {
    try {
      const meta = await api('/api/sessions/' + cur.id, { method: 'PATCH', body: JSON.stringify(fields) });
      view.state.cur = meta;
      // 同一会话可能同时开在主对话区与右栏子对话：另一处的胶囊也立刻跟上
      applySessionPermToViews(meta);
    } catch (e) { alert('保存失败：' + e.message); }
  } else if (view === mainView) {
    Object.assign(homeDraft, fields);
  }
  syncComposer(view);
}

// 同步 composer 芯片显示（权限/模式徽标/模型）——按视图更新对应那套控件
// 当前视图的工作模式：有会话看会话（子对话继承父会话模式），无会话只有主
// 对话区取首页草稿的选择；右栏没选子对话返回 ''（chatModeOf 回退到无定制的默认）
function viewChatMode(view) {
  const vcur = view.state.cur;
  if (vcur) return vcur.mode || '';
  return view === mainView ? (homeDraft.mode || '') : '';
}

// 模式控件离场动画：切换工作模式（如 编程 → 标准办公）时，工具栏上的模式胶囊
// （「计划」琥珀色开关、创建技能的录制按钮）不再瞬间消失，而是漏斗三段收进
// 同行最左侧的加号按钮：原位瘪成细线 → 细线滑到加号 → 在加号处立起融入
// （像被吞进去又立起来）。实现取向是「稳」：
// ① 原件立刻加 hidden——布局当场回到真实状态，快速连续切换模式绝不会出现
//    双胶囊、错位或布局抖动；飞走的只是一枚克隆的“幽灵”。
// ② 幽灵 fixed 挂 body、pointer-events:none，不参与布局、不可点；新动画开始
//    前先清掉旧幽灵（新动作取代旧动作），播完自动移除，另有兜底定时器——
//    无论怎么快速折腾页面都绝无残留、绝无堆积。
// ③ 任何异常情形（不支持 WAAPI、元素或加号不在屏上、系统开了“减弱动态
//    效果”）一律退回“瞬间隐藏”：动画纯锦上添花，永远不挡正确的最终状态。
function vanishIntoPlus(el, view) {
  if (!el || el.classList.contains('hidden')) return; // 本来就看不见：无事可做
  const plus = viewEl(view, 'btn-plus'); // 主视图 #btn-plus / 右栏 #sub-btn-plus
  const r = el.getBoundingClientRect();
  const p = plus ? plus.getBoundingClientRect() : null;
  if (!el.animate || !p || !p.width || !r.width ||
      (window.matchMedia && matchMedia('(prefers-reduced-motion: reduce)').matches)) {
    el.classList.add('hidden');
    return;
  }
  el.classList.add('hidden'); // 原件先退场（真实状态先行），幽灵接着飞
  for (const g of document.querySelectorAll('.mfx-ghost')) g.remove(); // 快速切换：旧的立即让位
  const ghost = el.cloneNode(true);
  ghost.classList.remove('hidden');
  ghost.removeAttribute('id'); // 克隆不带 id，绝不与原件撞 id
  ghost.classList.add('mfx-ghost');
  Object.assign(ghost.style, {
    position: 'fixed', left: r.left + 'px', top: r.top + 'px',
    width: r.width + 'px', height: r.height + 'px', margin: '0',
    zIndex: 'var(--z-ghost)', pointerEvents: 'none', // 压过消息流即可（锚定弹层 --z-pop、窄屏侧栏 --z-panel 均在其上）
  });
  document.body.appendChild(ghost);
  const dx = p.left + p.width / 2 - (r.left + r.width / 2);
  const dy = p.top + p.height / 2 - (r.top + r.height / 2);
  // 漏斗三段（二改：一版“整体缩小+平移”看不出进入加号，被吞的叙事要分三步讲）：
  // ① 原位骤压：宽度略收、高度压到 12% —— 胶囊瞬间瘪成一条细横线（漏斗入口）；
  // ② 细线滑行：保持细线形态平移到加号中心，宽度继续收窄（漏斗收口）；
  // ③ 立起融入：在加号处高度回弹立起（略过冲再落定），同时淡出 —— 像“吞进去又立起来”。
  // 每段自带 easing：骤压加速、滑行匀速、立起减速，一眼看出是“进去了”而非平移消失。
  const anim = ghost.animate([
    { transform: 'none', opacity: 1, offset: 0, easing: 'cubic-bezier(.5,0,.85,.55)' },
    { transform: 'scale(.85,.12)', opacity: 1, offset: .28, easing: 'linear' },
    { transform: 'translate(' + dx + 'px,' + dy + 'px) scale(.28,.12)', opacity: 1, offset: .62, easing: 'cubic-bezier(.25,.6,.35,1)' },
    { transform: 'translate(' + dx + 'px,' + dy + 'px) scale(.13,1.18)', opacity: .55, offset: .88 },
    { transform: 'translate(' + dx + 'px,' + dy + 'px) scale(.13,1)', opacity: 0, offset: 1 },
  ], { duration: 400 });
  const drop = () => ghost.remove();
  anim.onfinish = drop;
  anim.oncancel = drop;
  setTimeout(drop, 600); // 兜底：动画事件万一不来也绝不残留
}

// 模式前端定制的应用点（机制见 CHAT_MODES 上方「模式前端定制点」注释）：
// 当前模式挂了 ui 就「首挂 + 刷状态」，没挂就把本视图已挂的模式控件全部隐藏
const _modeUIMounted = new WeakMap(); // view → { [mode]: 控件根元素 }
function applyModeUI(view) {
  const mode = viewChatMode(view);
  const ui = chatModeOf(mode).ui;
  const mounted = _modeUIMounted.get(view) || {};
  if (!ui) {
    for (const el of Object.values(mounted)) vanishIntoPlus(el, view);
    return;
  }
  let el = mounted[mode];
  if (!el) {
    el = ui.mount(view);
    mounted[mode] = el;
    _modeUIMounted.set(view, mounted);
  }
  for (const [m, e] of Object.entries(mounted)) if (e && m !== mode) vanishIntoPlus(e, view);
  if (!el) return;
  el.classList.remove('hidden');
  ui.sync(view, sessionOrDraftFor(view), el);
}

function syncComposer(view) {
  view = view || mainView;
  const vcur = view.state.cur;
  const s = sessionOrDraftFor(view);
  const remote = vcur ? !!vcur.remote : (view === mainView && !!homeDraft.remote);
  viewEl(view, 'perm-chip-label').textContent = (PERM_LABELS[s.permission] || s.permission) + (remote ? ' ·远程' : '');
  viewEl(view, 'perm-chip').dataset.short = (PERM_SHORTS[s.permission] || s.permission) + (remote ? ' ·远' : '');
  // 芯片＝「模型名 · 档位」一体的入口（点开整张模型与档位卡片）；跟随服务端默认显示 Default
  viewEl(view, 'model-chip-label').textContent = modelNameOf(currentModelKey(view)) + ' · ' + thinkLabel(curThinking(view));
  viewEl(view, 'flag-plan').classList.toggle('on', s.planMode);
  viewEl(view, 'flag-goal').classList.toggle('on', s.goalMode);
  viewEl(view, 'flag-goal').title = s.goalMode ? ('目标模式：' + (s.goalText || '（未填写目标）')) : '目标模式';
  viewEl(view, 'mode-flags').classList.toggle('hidden', !s.planMode && !s.goalMode);
  applyModeUI(view); // 模式前端定制：在基础刷新之后跑、可覆盖上面的设置（两个视图都跑）
  if (vcur) viewEl(view, 'input').placeholder = sendKeyHint(); // 占位跟随发送快捷键设置
  refreshCtxChip(view);
  renderAttachments(view); // 附件区跟随视图/会话（换会话时放掉上一轮的附件）
  // 以下只属于主对话区（首页草稿 / 顶栏徽标 / 目录胶囊）
  if (view !== mainView) return;
  syncPlusChecks(view);
  // 顶栏远程徽标（挂了远程工作区的会话）。只显示固定短字「远程」：完整地址
  // （user@host:dir）放 title 悬停与点击后的 SSH 弹窗里——2026-11-04 前徽标
  // 内联整段地址，手机端两行顶栏被它挤断（标题被顶到第三行、同行胶囊被裁），
  // 桌面端也挤标题。电脑/手机同一套：短徽标 + flex:none，永不参与换行压缩。
  const rb = $('remote-badge');
  if (state.cur && state.cur.remote) {
    rb.textContent = '远程';
    rb.title = 'SSH 远程工作区 ' + remoteDisplay(state.cur.remote) + '（点击配置/解除）';
    rb.classList.remove('hidden');
  } else {
    rb.classList.add('hidden');
  }
  // 目录行/模式行只在新建对话（无会话）时出现；窄屏（手机）同样提供工作目录
  // 选择——新建对话要能换已有工作区/远程挂载，只有「添加工作目录」这步由
  // toggleWsPopAddNotice 就地告知（原生选择框弹在宿主机屏幕上手机等不到）
  const wsRow = $('ws-row');
  const modeRow = $('mode-row');
  if (!state.cur) {
    wsRow.classList.remove('hidden');
    modeRow.classList.remove('hidden');
    $('ws-row-name').textContent = homeDraft.remote
      ? remoteDisplay(homeDraft.remote)
      : (baseName(homeDraft.workspace || state.defaultWs || '') || '工作目录');
    $('mode-row-name').textContent = chatModeOf(homeDraft.mode).label;
  } else {
    wsRow.classList.add('hidden');
    modeRow.classList.add('hidden');
  }
  $('input').placeholder = sendKeyHint(); // 占位跟随发送快捷键设置（首页/会话同一提示）
  persistHomeDraftLS(); // 所有选择改动都会走到这里，统一落一次本地草稿
}

// remoteDisplay：user@host[:port]:dir
function remoteDisplay(r) {
  if (!r || !r.host) return '';
  const user = r.user || 'root';
  const port = (r.port && r.port !== 22) ? ':' + r.port : '';
  return user + '@' + r.host + port + ':' + (r.dir || '/');
}

// 加号菜单是单例：打开前按发起它的视图刷新勾选态，并记住归属（popView）
function syncPlusChecks(view) {
  const s = sessionOrDraftFor(view);
  const cur = view.state.cur;
  const remote = cur ? !!cur.remote : (view === mainView && !!homeDraft.remote);
  $('pm-plan-check').textContent = s.planMode ? '✓' : '';
  $('pm-goal-check').textContent = s.goalMode ? '✓' : '';
  $('pm-remote-check').textContent = remote ? '✓' : '';
  // 「本对话的组件与插件」只属于主对话区且要有当前会话（子对话区与首页不显示：
  // 组件装配跟主会话走，从子对话区改它反而说不清改的是谁）
  const cxItem = $('pm-cxsession');
  if (cxItem) {
    const hasSession = view === mainView && !!cur;
    cxItem.classList.toggle('hidden', !hasSession);
    $('pm-cxsession-check').textContent =
      hasSession && window.CX && CX.sessionCustom ? '✓' : '';
  }
}

function openPlusMenu(view) {
  popView = view;
  syncPlusChecks(view);
  openPopover('plus-menu', viewEl(view, 'btn-plus'));
}

// 完全访问预设捆绑的能力：切到完全访问即强制勾选、置灰不可关闭（后端权限引擎同样视为开启）
const FULL_BUNDLED_TOOLS = ['unsandboxed', 'browser', 'computer', 'device'];

// 默认能力（后端 perm.go 的 capDefaultsForMode 是同一策略的唯一权威，这里是镜像）：
// 「修改工作区」默认开启「操作浏览器」，只对真会注册 browser_* 的模式生效；
// 只读 / 完全访问不带默认项。用在「切档时把默认项补回」——完全访问 ⇄ 修改工作区、
// 创建技能模式 ⇄ 普通模式往返，都不能把默认能力丢掉（与后端 strip+merge 对齐）。
const CAP_DEFAULT_MODES = ['task', 'office', 'coding', 'beginner-coding', 'skill-create'];
function applyCapDefaults(tools, mode, perm) {
  const out = [...(tools || [])];
  if (perm === 'workspace-write' && CAP_DEFAULT_MODES.includes(mode) && !out.includes('browser')) out.push('browser');
  return out;
}

function renderPermPop(view) {
  view = view || popView || mainView;
  const s = sessionOrDraftFor(view);
  const pop = $('perm-pop');
  const full = s.permission === 'full';
  pop.querySelectorAll('input[name=perm]').forEach((r) => { r.checked = r.value === s.permission; });
  pop.querySelectorAll('input[data-tool]').forEach((c) => {
    const forced = full && FULL_BUNDLED_TOOLS.includes(c.dataset.tool);
    c.checked = forced || s.tools.includes(c.dataset.tool);
    c.disabled = forced; // 完全访问下捆绑能力不可关闭
    c.closest('.perm-opt').classList.toggle('forced', forced);
  });
}

function renderWsPop() {
  // 候选目录 = 默认工作区 + 手动添加的工作目录 + 历史会话用过的目录
  const seen = new Set();
  const list = [];
  const add = (ws) => {
    if (!ws || seen.has(ws)) return;
    seen.add(ws);
    list.push(ws);
  };
  add(state.defaultWs);
  lsGet('jx.wslist', []).forEach(add);
  (state.sessions || []).forEach((s) => add(s.workspace));
  const q = ($('ws-search').value || '').trim().toLowerCase();
  const filtered = q ? list.filter((ws) => ws.toLowerCase().includes(q)) : list;
  // 已登记的远程工作区（remotelist.go）：跟本地目录一起排进候选，
  // 选中即挂载为远程工作区（不带密码，发送建会话时服务端回填）
  const rms = (state.rmList || []).filter((r) => !q ||
    (r.display || '').toLowerCase().includes(q) || (r.host || '').toLowerCase().includes(q));
  // 挂了远程目录时不标本地选中项
  const cur = homeDraft.remote ? '' : (homeDraft.workspace || state.defaultWs || '');
  const curRm = homeDraft.remote ? remoteDisplay(homeDraft.remote) : '';
  const box = $('ws-pop-list');
  box.innerHTML = '';
  if (!filtered.length && !rms.length) {
    box.innerHTML = `<div class="ws-pop-empty">${q ? '没有匹配的工作目录' : '还没有工作目录，用下方按钮添加'}</div>`;
    return;
  }
  filtered.forEach((ws) => {
    const b = document.createElement('button');
    b.className = 'pop-item' + (ws === cur ? ' sel' : '');
    b.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linejoin="round"><path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/></svg>${esc(ws)}<span class="pop-check">${ws === cur ? '✓' : ''}</span>`;
    b.onclick = () => {
      homeDraft.remote = null; // 显式选本地目录 = 放弃远程挂载
      homeDraft.workspace = ws;
      closePopovers();
      syncComposer();
    };
    box.appendChild(b);
  });
  if (rms.length) {
    const label = document.createElement('div');
    label.className = 'pop-group ws-pop-group';
    label.textContent = '远程工作区';
    box.appendChild(label);
  }
  rms.forEach((r) => {
    const b = document.createElement('button');
    const sel = r.display === curRm;
    b.className = 'pop-item ws-remote-item' + (sel ? ' sel' : '');
    b.title = 'SSH 远程工作区 ' + r.display;
    b.innerHTML = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><rect x="3" y="4" width="18" height="7" rx="2"/><path d="M7 8h.01M11 8h.01"/><rect x="3" y="14" width="18" height="7" rx="2"/><path d="M7 18h.01M11 18h.01"/></svg>${esc(r.display || remoteDisplay(r))}<span class="ws-rm-x" title="忘记此远程工作区（不影响已有对话）">✕</span><span class="pop-check">${sel ? '✓' : ''}</span>`;
    b.onclick = () => {
      homeDraft.remote = { host: r.host, port: r.port, user: r.user, auth: r.auth, keyPath: r.keyPath || '', password: '', dir: r.dir };
      homeDraft.workspace = ''; // 挂远程 = 不选本地目录
      closePopovers();
      syncComposer();
    };
    b.querySelector('.ws-rm-x').onclick = async (e) => {
      e.stopPropagation();
      try { await api('/api/remotes/delete', { method: 'POST', body: JSON.stringify({ key: r.key || r.display }) }); } catch (err) { alert('忘记失败：' + err.message); return; }
      await loadRmList();
    };
    box.appendChild(b);
  });
}

/* ---------- 加号菜单动作（按视图：作用于发起操作的那个输入区） ---------- */

let uploadTarget = mainView; // 文件选择框是单例：记住这次选择属于哪个视图

/* ---------- 附件区（输入框上方那条"本轮带进来的文件"轨道） ----------
   选择 / 粘贴 / 拖拽进来的文件先在这里暂存，点发送才真正写进工作区（写入后由后端
   照旧注入【文件上传】通知，通知文案与触发行为都不变）。没有附件时整块带 hidden：
   不占高度、不留白条、也没有空状态提示——有且只有"真有附件"时才出现。
   交互与布局对齐数字助理2 的 ui-attachment（ComposerAttachments / AttachmentRail：
   横向卡片轨道 + 图片缩略图 + 逐项移除 + 拖拽投放）。

   两种载荷与既有上传路径一一对应：桌面壳（本地会话）采集本机真实路径，发送时走
   JSON {path}（登记 uploads 表，该路径可作 deliver_result 的 upload 交付目标）；其余（浏览器 /
   手机 / 远程 / 粘贴 / 拖拽）暂存 File 对象，发送时走 multipart。 */

// 与后端 multipart 上限一致（server.go handleUpload 的 LimitReader）：超了入列时就提示。
// 256MB：办公 PPT 常带大媒体，32MB 旧上限会把多数 pptx 挡在门外（用户以为「粘贴不了」）。
const ATTACH_MAX_BYTES = 256 * 1024 * 1024;
const ATTACH_MAX_COUNT = 20;               // 一条消息最多带 20 个附件

// 前端诊断打点：桌面壳 WebView2 封了远程调试端口、也没有 console 可看，
// 关键分支的执行结果经 1px GIF beacon 回传后端日志（GET /api/diag 落一行）。
// 排障「点了没反应 / 请求没发出」类问题用，别当普通日志刷。
function jxDiag(msg) {
  try { new Image().src = '/api/diag?m=' + encodeURIComponent(msg) + '&t=' + Date.now(); } catch (e) { /* 静默 */ }
}

let attachSeq = 0; // 附件 id 序号（本次页面生命周期内递增，DOM 定位与移除用）

// 附件挂在视图状态上：主对话与右栏子对话各一份，互不相干
function attachmentsOf(view) {
  const st = (view || mainView).state;
  if (!st.attachments) st.attachments = [];
  return st.attachments;
}

// 输入框上方的提示条（附件相关提示都走它，到点自清）
function attachHint(view, text, ms) {
  view.hint.textContent = text;
  if (view._attachHintTimer) clearTimeout(view._attachHintTimer);
  view._attachHintTimer = setTimeout(() => { view.hint.textContent = ''; }, ms || 3200);
}

// 剪贴板来的图片没有文件名，按 MIME 给一个带时间戳的默认名
function pasteImageName(f) {
  const ext = { 'image/png': '.png', 'image/jpeg': '.jpg', 'image/jpg': '.jpg', 'image/gif': '.gif', 'image/webp': '.webp', 'image/bmp': '.bmp' }[f.type] || '.png';
  const d = new Date(), p = (n) => String(n).padStart(2, '0');
  return '粘贴图片-' + d.getFullYear() + p(d.getMonth() + 1) + p(d.getDate()) + '-' +
    p(d.getHours()) + p(d.getMinutes()) + p(d.getSeconds()) + ext;
}

// 桌面壳原生选择框：本机真实路径入列（发送时走 JSON {path}，不受 32MB 限制）
function stagePaths(paths, view) {
  view = view || popView || mainView;
  const list = attachmentsOf(view);
  const added = [];
  let dropped = 0;
  for (const p of paths) {
    if (list.length >= ATTACH_MAX_COUNT) { dropped++; continue; }
    const name = String(p).split(/[\\/]/).pop() || String(p);
    const a = {
      id: 'a' + (++attachSeq), kind: 'file', name, size: null, mime: '',
      file: null, path: p, previewUrl: '', status: 'staged',
    };
    list.push(a);
    added.push(a);
  }
  renderAttachments(view);
  // 输入框里挂上“报告fina….docx”胶囊标记：附件区卡片照旧（图标/缩略图/✕），胶囊只记
  // “在正文哪个位置引用了哪个文件”，发送时原样随正文发出（【文件上传】注记带全名）
  for (const a of added) insertInputMark(view, attachMarkOf(a));
  if (added.length) {
    if (view === mainView) persistMainDraftLS(); else persistSubDraftLS();
    syncComposerPad(view);
  }
  if (dropped) attachHint(view, '一条消息最多带 ' + ATTACH_MAX_COUNT + ' 个附件，' + dropped + ' 个没有加入');
}

// 浏览器采集到的文件入列（File/Blob：发送时走 multipart）；粘贴与拖拽也走这里。
// 返回本批真正入列的附件（超出上限/超大的会被跳过，调用方要按它对齐 File 对象）。
function stageFiles(files, view) {
  view = view || uploadTarget || mainView;
  const list = attachmentsOf(view);
  const added = [];
  let dropped = 0, tooBig = 0;
  for (const f of files) {
    if (list.length >= ATTACH_MAX_COUNT) { dropped++; continue; }
    if (f.size > ATTACH_MAX_BYTES) { tooBig++; continue; }
    const isImage = /^image\//.test(f.type || '');
    const a = {
      id: 'a' + (++attachSeq), kind: isImage ? 'image' : 'file',
      name: f.name || pasteImageName(f), size: f.size, mime: f.type || '',
      file: f, path: '', previewUrl: isImage ? URL.createObjectURL(f) : '', status: 'staged',
    };
    list.push(a);
    added.push(a);
  }
  renderAttachments(view);
  // 输入框里挂上“报告fina….docx”胶囊标记（同 stagePaths：附件区卡片照旧，标记只管位置）
  for (const a of added) insertInputMark(view, attachMarkOf(a));
  if (added.length) {
    if (view === mainView) persistMainDraftLS(); else persistSubDraftLS();
    syncComposerPad(view);
  }
  if (tooBig) attachHint(view, tooBig + ' 个文件超过 ' + fmtBytes(ATTACH_MAX_BYTES) + '，没有加入；大文件请点 ＋ 用「文件上传」选（走本机路径，不受此限）', 8000);
  else if (dropped) attachHint(view, '一条消息最多带 ' + ATTACH_MAX_COUNT + ' 个附件，' + dropped + ' 个没有加入');
  return added;
}

/* ---------- 拖拽原件路径桥（仅桌面壳 WebView2） ----------
   WebView2 不给页面文件绝对路径（File.path 不存在、text/uri-list / text/plain
   皆空、webkitGetAsEntry().fullPath 只有文件名——实测见 AGENT.md 界面节），所以
   拖进来的文件过去只能按 multipart 送内容：原件路径无人登记，交付工具送不回原处。
   宿主能拿到：页面把 File 对象经 chrome.webview.postMessageWithAdditionalObjects
   发给宿主，宿主在 WebMessageReceived 的 AdditionalObjects 里读出
   CoreWebView2File.Path（真实路径，见 dropbridge_windows.go），再回调
   JX.onNativeDrop 交回来——这里把本批附件从「multipart」就地升级成「路径型」
   （发送时走既有 JSON {path} 通路，登记原件路径）。
   浏览器 / 手机 / 远程没有这个 API：函数直接返回，行为与从前完全一致。 */
let dropBatchSeq = 0;
const dropBatches = new Map(); // batch → { view, items:[{id,name}] }（等宿主回话）

// 把一批拖入的 File 对象交给宿主换真实路径（batch 用于回话配对）
function requestNativeDropPaths(files, added, view) {
  const wv = window.chrome && window.chrome.webview;
  if (!wv || typeof wv.postMessageWithAdditionalObjects !== 'function' || !added.length) return;
  const batch = ++dropBatchSeq;
  dropBatches.set(batch, { view, items: added.map((a) => ({ id: a.id, name: a.name })) });
  if (dropBatches.size > 20) { // 没人回话（老运行时）的批次不积压
    const oldest = dropBatches.keys().next().value;
    dropBatches.delete(oldest);
  }
  try {
    wv.postMessageWithAdditionalObjects(
      { kind: 'jx-drop-paths', batch, names: added.map((a) => a.name) },
      added.map((a) => a.file) // 与 added 一一对应（跳过的不进来，顺序不错位）
    );
  } catch (e) {
    dropBatches.delete(batch); // 序列化失败等：保持 multipart，拖拽本身不受影响
  }
}

// 宿主回话（宿主注入 JX.onNativeDrop({batch,names,paths})）：升级成路径型附件
function applyNativeDropPaths(reply) {
  const batch = reply && reply.batch;
  const rec = batch != null ? dropBatches.get(batch) : null;
  if (!rec) return;
  dropBatches.delete(batch);
  const list = attachmentsOf(rec.view);
  const paths = (reply.paths || []);
  let n = 0;
  rec.items.forEach((it, i) => {
    const p = paths[i];
    if (!p) return; // 这一条没取到路径（非文件对象等）：保持 multipart
    const a = list.find((x) => x.id === it.id);
    if (!a || a.path === p) return;
    a.path = p; // 发送时 flushAttachments 走 JSON {path} → 登记原件路径
    n++;
  });
  if (n) {
    renderAttachments(rec.view); // 卡片 title 会显示原件路径
    jxDiag('drop:native-paths ' + n + '/' + rec.items.length);
  }
}
JX.onNativeDrop = applyNativeDropPaths;

/* ---------- 手机壳分享入口（jx-link 的系统分享桥） ----------
   Android 壳（jx-link）收到其他应用（微信「用其他应用打开」/分享、文件管理器、
   浏览器分享文本）的 ACTION_SEND / ACTION_VIEW 后，把内容注入到这里：
   - 文件走 stageFiles（与拖拽/粘贴/SAF 选择完全同一条通路）——落进输入框
     附件区：跨会话切换保留、最终发送时才上送；
   - 文字追加进主输入框（触发 input 事件，草稿/量高照常跟上）。
   壳子注入时优先找 JX.receiveFiles（本入口），电脑端未更新时回退全局
   stageFiles / 直接写 #input，两不误。 */
JX.receiveFiles = function (files) {
  return stageFiles(Array.prototype.slice.call(files), mainView);
};
JX.receiveText = function (text) {
  const ta = viewEl(mainView, 'input');
  if (!ta) return false;
  const cur = ta.value;
  ta.value = (cur ? cur.replace(/\s+$/, '') + '\n' : '') + String(text);
  ta.dispatchEvent(new Event('input', { bubbles: true }));
  return true;
};

// 清空附件（发送完毕）：缩略图的 objectURL 一并释放。暂存区跨对话通用，切对话不清
function clearAttachments(view) {
  const list = attachmentsOf(view);
  for (const a of list) if (a.previewUrl) URL.revokeObjectURL(a.previewUrl);
  list.length = 0;
  const box = viewEl(view, 'attachments');
  if (box) { box._sig = null; box.innerHTML = ''; box.classList.add('hidden'); }
  syncComposerPad(view);
}

// 单张附件卡：图片给缩略图，其余给文件卡（图标 + 文件名 + 大小 + ✕）
function attachCard(view, a) {
  const card = document.createElement('div');
  card.className = 'attach-item' + (a.kind === 'image' ? ' image' : '') +
    (a.status === 'error' ? ' error' : (a.status === 'uploading' ? ' uploading' : ''));
  card.title = (a.path || a.name) + (a.size != null ? '（' + fmtBytes(a.size) + '）' : '') +
    (a.status === 'error' ? '\n上传失败：' + (a.error || '') : '');
  const rm = document.createElement('button');
  rm.type = 'button';
  rm.className = 'attach-x';
  rm.title = '移除（这条消息不会带上它）';
  rm.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.6" stroke-linecap="round"><path d="M6 6l12 12M18 6L6 18"/></svg>';
  rm.onclick = (e) => { e.stopPropagation(); removeAttachment(view, a.id); };
  if (a.kind === 'image' && a.previewUrl) {
    const img = document.createElement('img');
    img.className = 'attach-thumb';
    img.src = a.previewUrl;
    img.alt = a.name;
    card.appendChild(img);
    card.appendChild(rm);
    return card;
  }
  const icon = document.createElement('span');
  icon.className = 'attach-icon';
  icon.innerHTML = '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8" stroke-linecap="round" stroke-linejoin="round"><path d="M14 3H7a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V8z"/><path d="M14 3v5h5"/></svg>';
  const name = document.createElement('b');
  name.className = 'attach-name';
  name.textContent = a.name;
  const size = document.createElement('span');
  size.className = 'attach-size';
  size.textContent = a.status === 'uploading' ? '上传中…' : (a.status === 'error' ? '未上传' : (a.size != null ? fmtBytes(a.size) : ''));
  card.appendChild(icon);
  card.appendChild(name);
  card.appendChild(size);
  card.appendChild(rm);
  return card;
}

function removeAttachment(view, id) {
  const list = attachmentsOf(view);
  const i = list.findIndex((a) => a.id === id);
  if (i < 0) return;
  const a = list[i];
  // 上传中的附件：中止其挂起中的上传（flushAttachments 还 await 着它的 fetch）。
  // 不中止的话请求要等到自身失败/超时才结束——期间 _attachFlush 一直占着（点
  // 发送全被吞），结束后还会对这个已删附件再弹一次上传失败：正是「删了附件仍
  // 报同一个文件未上传」的残留链
  if (a._abort) { try { a._abort.abort(); } catch (e) { /* 已结束的上传再 abort 无害 */ } }
  if (a.previewUrl) URL.revokeObjectURL(a.previewUrl);
  // 它在输入框里的胶囊标记一并摘掉：认领要在移出列表之前算，而且只摘它自己那一处
  // （同名两份的标记文字相同，按文本 split 全删会把另一份的一起带走）
  const r = a.mark ? inputMarkRanges(view.input.value, list).find((x) => x.owner === a) : null;
  list.splice(i, 1);
  if (r) {
    const v = view.input.value;
    view.input.value = v.slice(0, r.start) + v.slice(r.end);
    autoGrow(view.input);
    if (view === mainView) persistMainDraftLS(); else persistSubDraftLS();
  }
  const box = viewEl(view, 'attachments');
  if (box) box._sig = null; // 强制重画（删到空时立刻回到零占位）
  renderAttachments(view);
}

// 附件区渲染：0 个附件 → 带 hidden（零占位、不参与布局）；有附件才上屏。
// 内容签名没变就不重建 DOM（syncComposer 会被频繁调用）。
// 附件暂存区是「所有对话通用」的：跨对话切换/回首页都不清（手机壳把文件分享到
// 新建对话页、再挑一个对话发出去，靠的就是这份暂存跟着走）；只有发送成功才清。
function renderAttachments(view) {
  view = view || mainView;
  const box = viewEl(view, 'attachments');
  if (!box) return;
  const list = attachmentsOf(view);
  const sig = list.map((a) => [a.id, a.name, a.size, a.status].join(':')).join('|');
  if (box._sig === sig) return;
  box._sig = sig;
  box.innerHTML = '';
  box.classList.toggle('hidden', !list.length);
  for (const a of list) box.appendChild(attachCard(view, a));
  syncComposerPad(view); // 出现/消失即时重算悬浮垫高（ResizeObserver 之外再兜一次，防首帧抖动）
}

/* 粘贴与拖拽投放：都只入附件区，不碰工作区。
   粘贴只吃剪贴板里的文件对象（截图、网页复制的图片），纯文本粘贴原样放行；
   从资源管理器复制文件再粘贴时 Chromium 只给路径文本、拿不到文件对象，本轮不做。 */
function setupAttachDrop(view) {
  // 投放目标是输入卡本身，不是 composer-wrap：首页时输入卡被搬进页面中央的问候区
  // （wrap 里根本没有它），绑在 wrap 上会漏掉首页那次投放
  const card = viewEl(view, 'composer');
  const input = view.input;
  if (card) {
    let depth = 0; // dragenter/leave 会在子元素上反复触发，用深度计数防闪
    const hasFiles = (e) => !!e.dataTransfer && [...(e.dataTransfer.types || [])].includes('Files');
    card.addEventListener('dragenter', (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      depth++;
      card.classList.add('drop-hot');
    });
    card.addEventListener('dragover', (e) => {
      if (!hasFiles(e)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'copy';
    });
    card.addEventListener('dragleave', () => {
      depth = Math.max(0, depth - 1);
      if (!depth) card.classList.remove('drop-hot');
    });
    card.addEventListener('drop', (e) => {
      depth = 0;
      card.classList.remove('drop-hot');
      const files = e.dataTransfer && e.dataTransfer.files ? [...e.dataTransfer.files] : [];
      if (!files.length) return;
      e.preventDefault();
      const added = stageFiles(files, view);
      // 桌面壳：顺手把 File 对象交给宿主换真实路径（取到后升级成路径型附件，
      // 交付工具才能把成品送回桌面原位置）；浏览器/手机没有这条桥，什么都不做。
      requestNativeDropPaths(files, added, view);
    });
  }
  const rail = viewEl(view, 'attachments');
  if (rail) {
    // 附件区横向排卡：竖向滚轮平移成横向（否则会去滚后面的对话区）
    rail.addEventListener('wheel', (e) => {
      if (e.deltaY === 0) return;
      e.preventDefault();
      rail.scrollLeft += (e.deltaY > 0 ? 1 : -1) * Math.min(Math.abs(e.deltaY), 60);
    }, { passive: false });
  }
  if (input) {
    input.addEventListener('paste', (e) => {
      const cd = e.clipboardData;
      if (!cd) return;
      const files = [];
      const itemKinds = [];
      for (const it of (cd.items || [])) {
        itemKinds.push(it.kind + ':' + it.type);
        if (it.kind !== 'file') continue;
        const f = it.getAsFile();
        if (f) files.push(f);
      }
      if (!files.length && cd.files) files.push(...cd.files);
      // 剪贴板带文件（或声明了 Files 类型）时打点：文件名/大小/类型全记，
      // 「ppt 粘贴不了、word 能粘」这类差异一查便知（超限被拒/类型为空/根本没送到）
      if (files.length || (cd.types || []).includes('Files')) {
        jxDiag('paste:files=[' + files.map((f) => f.name + ':' + f.size + ':' + (f.type || '?')).join(', ') +
          '] items=[' + itemKinds.slice(0, 8).join(' | ') + '] types=[' + (cd.types || []).join(',') + ']');
      }
      if (!files.length) return; // 纯文本：交给默认粘贴
      e.preventDefault();
      stageFiles(files, view);
    });
  }
}

// 上传失败时取后端那句可读的 error（writeErr 的响应体是 {"error":"…"}），
// 别把 JSON 原样甩到提示条上
async function uploadErrText(r) {
  let t = '';
  try { t = await r.text(); } catch (e) { /* 读不到就用状态码 */ }
  try {
    const d = JSON.parse(t);
    if (d && d.error) return String(d.error);
  } catch (e) { /* 不是 JSON：原样用 */ }
  return t.trim() || ('HTTP ' + r.status);
}

/* 发送时落盘：把暂存的附件逐个写进工作区，收集每个附件返回的【文件上传】注记
   文本（后端不再独立落注记行），由调用方拼进消息开头的「用户附件：」块——注记
   随用户消息一起到达（插话/排队同样，块随内容走）。有失败就中止本轮发送，返回
   null：全部附件（含已成功的）都留在附件区——重试时一起重传，判重会把重复上传
   当同一次处理（幂等），注记块因此永远完整；旧版"成功项清走"会在中止后丢掉
   它们的注记（独立注记行已不存在），不再可行。 */
async function flushAttachments(view) {
  const list = attachmentsOf(view);
  const sid = view.state.cur && view.state.cur.id;
  if (!list.length || !sid) return [];
  view._attachFlush = true; // 上传期间忽略重复点发送（见 send 的入口守卫）
  const box = viewEl(view, 'attachments');
  if (box) box.classList.add('busy');
  let failed = 0, firstErr = '';
  const notes = [];
  // 迭代用快照：上传是多轮 await，期间用户可能 ✕ 掉任何一项。直接 for..of 原数组
  // 会在删掉"当前正在传的那项"时让迭代器跳位漏传后面的附件（消息发出、附件却
  // 没带上）；快照 + includes 校验让被删项自然跳过，其余项一个不少。
  const queue = [...list];
  try {
    for (const a of queue) {
      if (!list.includes(a)) continue; // 已被 ✕ 删掉：不再上传
      a.status = 'uploading';
      a._abort = new AbortController(); // 删除附件时按它中止挂起中的上传
      renderAttachments(view);
      try {
        const url = '/api/sessions/' + sid + '/upload';
        let r;
        if (a.path) {
          r = await fetch(url, { method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ path: a.path }), signal: a._abort.signal });
        } else {
          const fd = new FormData();
          fd.append('file', a.file, a.name);
          r = await fetch(url, { method: 'POST', body: fd, signal: a._abort.signal });
        }
        if (!r.ok) throw new Error(await uploadErrText(r));
        const d = await r.json().catch(() => ({}));
        a.note = (d && d.note) || '';
        // 注记是「用户附件：」块的唯一来源：响应正常却没带回注记时若按成功放行，
        // 消息照发、文件块缺失——比报错拦下更糟的静默失败，按失败处理
        if (!a.note) throw new Error('上传响应缺少注记');
        if (!list.includes(a)) continue; // 传完了但它已被 ✕ 删掉：丢弃注记，不并入本轮
        a.status = 'staged';
        notes.push(a.note);
      } catch (e) {
        if (!list.includes(a)) continue; // 已被删掉（含 ✕ 触发的 abort）：不计失败、不提示
        a.status = 'error';
        a.error = String((e && e.message) || e);
        if (!firstErr) firstErr = a.error;
        failed++;
      }
    }
  } finally {
    view._attachFlush = false;
    if (box) box.classList.remove('busy');
  }
  if (failed) {
    const bx = viewEl(view, 'attachments');
    if (bx) bx._sig = null;
    renderAttachments(view);
    attachHint(view, failed + ' 个附件上传失败，本轮没有发送（全部留在附件区，可重试或 ✕ 移除）：' + firstErr.slice(0, 80), 8000);
    return null;
  }
  return notes; // 每份附件的注记行（拼进消息头）；全成功才非 null
}

// 加号菜单「上传文件」：只把文件加进附件区（不再立刻写工作区），落盘交给发送那一步
async function doUpload(view) {
  view = view || popView || mainView;
  closePopovers();
  // 让出本次鼠标事件链再弹选择器：原生文件选择对话框（桌面壳 /api/pick-file）与浏览器
  // 文件框若还在 mousedown/mouseup 处理链里弹出，容易抢走随后的 mouseup —— 语音快捷键
  // 引擎会因此留下"按键未松开"的残留（点上传却开听写的老问题，详见
  // 语音误触发排查（点上传文件启动听写）2026-10-10.md）。
  await new Promise((r) => setTimeout(r, 0));
  // 本轮执行中照样让选文件（选进来的随下一条消息一起上传）；旧版直接 return，
  // 用户看来就是「点了文件上传没任何反应」
  jxDiag('upload:click streaming=' + !!view.state.streaming +
    ' shell=' + document.documentElement.classList.contains('desktop-shell') +
    ' remote=' + !!(view.state.cur && view.state.cur.remote));
  if (view.state.streaming) attachHint(view, '本轮执行中：加进来的文件会随下一条消息一起上传');
  // 桌面壳（shell=desktop）的本地会话：走系统原生文件选择框采集本机真实路径
  // （它同时是 deliver_result 的 upload 交付目标来源）；用户取消则静默结束。fetch 带超时：
  // 渲染进程异常/连接问题时请求可能永远悬着，到点回退浏览器文件选择框，不再干等。
  // 手机/远程（JX_REMOTE）与局域网浏览器（非桌面壳）走 multipart 原路径。
  if (document.documentElement.classList.contains('desktop-shell') &&
      !(view.state.cur && view.state.cur.remote)) {
    try {
      jxDiag('upload:fetch pick-file');
      const opt = (window.AbortSignal && AbortSignal.timeout) ? { signal: AbortSignal.timeout(15000) } : {};
      const r = await fetch('/api/pick-file?filter=all&multi=1&title=' + encodeURIComponent('选择要加入的文件'), opt);
      jxDiag('upload:pick-file http=' + r.status);
      if (r.ok) {
        const d = await r.json();
        const paths = d.paths || (d.path ? [d.path] : []);
        jxDiag('upload:pick-file paths=' + paths.length);
        if (paths.length) stagePaths(paths, view);
        return; // 选到了就入列收工，取消就什么都不做
      }
    } catch (e) {
      jxDiag('upload:pick-file err=' + ((e && (e.name + ':' + e.message)) || e) + '，回退浏览器文件框');
    }
  }
  jxDiag('upload:回退 file-input');
  uploadTarget = view;
  $('file-input').value = '';
  $('file-input').click();
}

function doExport(format, view) {
  view = view || popView || mainView;
  closePopovers();
  const cur = view.state.cur;
  if (!cur) { alert('还没有可导出的对话'); return; }
  const q = format === 'json' ? '?format=json' : '';
  const url = '/api/sessions/' + cur.id + '/export' + q;
  if (window.JX_REMOTE) { JX_REMOTE.openURL(url); return; } // 远程模式：隧道取 blob 再开
  jxOpenPage(url);
}

async function toggleModeFlag(flag, view) {
  view = view || popView || mainView;
  const s = sessionOrDraftFor(view);
  const turningOn = !s[flag];
  const fields = {};
  fields[flag] = turningOn;
  if (turningOn && flag === 'planMode') fields.goalMode = false; // 计划/目标二选一
  if (turningOn && flag === 'goalMode') fields.planMode = false;
  await patchDraftOrSession(fields, view);
  closePopovers();
}

function setupWidthHandles() {
  const wrap = $('messages');
  if (!wrap) return;
  // 把滚动区包进一层定位容器：把手不能直接放滚动容器里（会随内容滚走）
  let zone = document.getElementById('msg-zone');
  if (!zone) {
    zone = document.createElement('div');
    zone.id = 'msg-zone';
    wrap.parentNode.insertBefore(zone, wrap);
    zone.appendChild(wrap);
  }
  // 拖拽跟随不依赖 setPointerCapture：把手只有 10px 宽，捕获一旦失效，move 事件
  // 只剩「指针正压在把手上」的窄窗才有——2026-11-08 远程实报：经网关打开的页面
  // 上捕获静默不生效，对话区把手的 ×2 增益让把手以与指针同速跑离，拖几个像素
  // 就再也跟不上（「能动很小一点，离开就不跟」）。改为拖动期间由 window 接管
  // move/up：指针下无论是什么元素，事件都冒泡到 window，捕获在不在都持续跟随；
  // setPointerCapture 保留为增强（本地可用时，指针拖出窗口外也不断流）。
  // 三处分割条（对话区/右栏/左栏）同一模式，见 app-right.js / app-left.js。
  for (const side of ['left', 'right']) {
    const h = document.createElement('div');
    h.className = 'w-handle ' + side;
    h.title = '拖动调整对话区宽度（双击恢复默认）';
    zone.appendChild(h);
    let startX = 0, startW = 860;
    h.addEventListener('pointerdown', (e) => {
      e.preventDefault();
      startX = e.clientX;
      startW = contentWidth();
      h.classList.add('dragging');
      try { h.setPointerCapture(e.pointerId); } catch (err) { /* 旧浏览器忽略 */ }
    });
    window.addEventListener('pointermove', (e) => {
      if (!h.classList.contains('dragging')) return;
      const dx = (e.clientX - startX) * 2 * (side === 'right' ? 1 : -1); // 居中对称放大
      setContentWidth(startW + dx);
    });
    const up = () => {
      if (!h.classList.contains('dragging')) return;
      h.classList.remove('dragging');
      setContentWidth(contentWidth(), true); // 松手落盘
    };
    window.addEventListener('pointerup', up);
    window.addEventListener('pointercancel', up);
    h.addEventListener('dblclick', () => {
      document.documentElement.style.removeProperty('--content-w');
      lsSet('jx.contentWidth', null);
    });
    h.addEventListener('animationend', (e) => {
      // 淡尽摘类：forwards 末帧（opacity:0）会一直压住 :hover 的亮起，必须摘掉
      if (e.animationName === 'w-handle-flash') h.classList.remove('flash');
    });
  }
  // 窗口宽度变化 → 两侧亮线像悬停一样亮起、3s 淡出（提示这里能拖宽）。拖动窗口
  // 期间 resize 连发，每帧摘类→回流→挂类重放，表现为常亮；松手后才开始淡出计时。
  // 只认宽度真变化（纯调高度不闪）。
  let flashWinW = window.innerWidth;
  window.addEventListener('resize', () => {
    if (window.innerWidth === flashWinW) return;
    flashWinW = window.innerWidth;
    for (const h of zone.querySelectorAll('.w-handle')) {
      h.classList.remove('flash');
      void h.offsetWidth; // 强制回流：打断未播完的淡出、从头重放
      h.classList.add('flash');
    }
  });
}

/* ---------- 回到底部按钮 + 手机端输入区滚动收起（主/子对话按 view 装配） ---------- */

// 回到底部按钮：滚离底部一定距离浮现于输入区上方正中（手机/桌面一致）。
// 主对话挂 #msg-zone（上面 setupWidthHandles 建的定位层），子对话现场把
// #sub-messages 包进同款 .sub-msg-zone；按钮不随内容滚动，始终贴消息区底部。
const JUMP_SHOW = 320, JUMP_HIDE = 160; // 滞回阈值：出现/收起取不同距离，防临界抖动

function makeJumpLatest(view) {
  const b = document.createElement('button');
  b.className = 'jump-latest';
  b.title = '回到底部';
  b.innerHTML = chev('p-chev');
  b.onclick = () => {
    view._pinned = false; // 用户明确要回底部：恢复跟随
    view._progUntil = Date.now() + 800; // 平滑下降途中的中间位置不当作上翻
    view.scroll.scrollTo({ top: view.scroll.scrollHeight, behavior: 'smooth' });
    tuckComposer(view, false);
  };
  return b;
}

function setupJumpLatest() {
  for (const view of [mainView, subView]) {
    let zone = null;
    if (view.key === 'main') {
      zone = document.getElementById('msg-zone'); // boot 里 setupWidthHandles 先建好
    } else {
      const inner = view.scroll; // #sub-messages：包一层定位容器（幂等）
      if (inner.parentNode && !inner.parentNode.classList.contains('sub-msg-zone')) {
        zone = document.createElement('div');
        zone.className = 'sub-msg-zone';
        inner.parentNode.insertBefore(zone, inner);
        zone.appendChild(inner);
      } else if (inner.parentNode) {
        zone = inner.parentNode;
      }
    }
    if (!zone) continue;
    view._jumpBtn = makeJumpLatest(view);
    zone.appendChild(view._jumpBtn);
  }
}

// 手机端输入区收起：往回翻历史时整体滑出屏幕底让位；向底部翻/聚焦输入框/点回
// 到底部时滑回。桌面（≥500px）不参与，断点跨越时由 app-core 的复位逻辑清理。
function composerWrapOf(view) {
  if (view.key === 'main') return $('composer-wrap');
  return document.querySelector('.sub-chat .composer-wrap');
}

// 悬浮输入区的配套：输入卡铺在消息区上方（见 style.css 输入区节），内容列要垫
// 等高内边距，最后一条消息才停在卡上方而不是被盖住；回到底部按钮的锚点高度
// --cw-h 也在此同步（收起时归零，按钮落到屏底）。输入区高度随输入/胶囊/提示条
// 变化，由 ResizeObserver 跟随，切换上下文时 placeComposer / openSubSession 补调。
function syncComposerPad(view) {
  const wrap = composerWrapOf(view);
  const zone = view.scroll && view.scroll.parentNode;
  if (!wrap || !zone || !view.col) return;
  const h = wrap.classList.contains('tucked') ? 0 : wrap.offsetHeight;
  view.col.style.paddingBottom = (h + 14) + 'px';
  zone.style.setProperty('--cw-h', h + 'px');
  view._cwH = h; // 未收起时的垫高：收起判定用它做距离阈值（见 setupScrollTuck）
}

function tuckComposer(view, on) {
  const wrap = composerWrapOf(view);
  if (!wrap) return;
  if (on) {
    if (!isNarrow() || wrap.classList.contains('tucked')) return;
    // 有待决定的浮窗（审批/提问/计划审批）时不收起：否则卡片跟着输入区一起滑出屏幕
    if (askPending(view)) return;
    wrap.style.transform = 'translateY(100%)';
    wrap.classList.add('tucked');
    syncComposerPad(view); // 收起即刻收走垫高：内容马上用上让出的高度
  } else {
    if (!wrap.classList.contains('tucked')) return;
    wrap.classList.remove('tucked');
    wrap.style.transform = '';
    // 展开相反：垫高等滑入动画结束（.25s）再恢复——瞬间撑回 189px 的话，最后
    // 一条消息与屏幕底之间会闪出一大块空背景（浅色主题下就是一条白边），卡片
    // 要再过 250ms 才盖上来。动画期间卡从下方滑入、内容暂滚到它后面（悬浮卡
    // 本来就允许）。syncComposerPad 读实时状态，期间若又收起会先按收起收口，
    // 这个迟到的一次同样幂等
    setTimeout(() => syncComposerPad(view), 270);
  }
}

function setupScrollTuck() {
  for (const view of [mainView, subView]) {
    setupAttachDrop(view); // 附件区：粘贴图片与拖拽投放（两个视图各挂一套）
    setupInputMarks(view); // 标记胶囊：背衬层渲染与光标原子策略（两个视图各挂一套）
    const lob = viewEl(view, 'load-older');
    if (lob) lob.onclick = () => loadOlder(view); // 顶条点击：立即取上一批
    let lastY = view.scroll.scrollTop;
    // 跟随钉住的手势入口：滚轮上翻/触摸上滑立即钉住（无论离底多近——否则
    // 一步滚轮增量会被下一次跟随吃掉）；拖滚动条/键盘翻页由下方 scroll 兜底。
    // 手势同时记 _actTs（视口锚定的活动守卫）：触摸/滚轮操作中及其惯性收尾期
    // 不做锚定补偿——程序写 scrollTop 会掐断惯性滚动（见 anchorRestore）
    view.scroll.addEventListener('wheel', (e) => {
      view._actTs = Date.now();
      if (e.deltaY < 0) { view._pinned = true; view._pinTs = Date.now(); }
    }, { passive: true });
    let touchY = null;
    view.scroll.addEventListener('touchstart', (e) => {
      view._actTs = Date.now();
      touchY = e.touches[0] ? e.touches[0].clientY : null;
    }, { passive: true });
    view.scroll.addEventListener('touchmove', (e) => {
      view._actTs = Date.now();
      if (touchY != null && e.touches[0] && e.touches[0].clientY - touchY > 4) {
        view._pinned = true; view._pinTs = Date.now(); // 手指下移 = 内容上翻
      }
    }, { passive: true });
    view.scroll.addEventListener('touchend', () => { view._actTs = Date.now(); }, { passive: true });
    view.scroll.addEventListener('scroll', () => {
      const el = view.scroll;
      const y = el.scrollTop;
      const dy = y - lastY;
      lastY = y;
      // 跟随钉住/恢复：内容增长不触发 scroll（scrollTop 不变），离底变远只可能
      // 是用户在滚 → 钉住；滚回贴近底部 → 恢复跟随（刚钉住的短窗口内不判恢复，
      // 小距上翻的余量不能立刻抵消钉住）。程序性贴底跟随时 dist≈0，天然维持跟随态。
      // 两类合成事件不算用户意图：渲染钳位（_renderTs 后 350ms 内——尾部重建
      // 把 scrollTop 夹到临时底部，随后折叠 rAF 与 200ms 补测的高度余波也在
      // 这个窗口里；窗口太短会把钳位误读成「用户滚回底部」→ 解除钉住 → 下一次
      // 跟随把阅读位置拽到底，正是「下面一有更新就往下跳」）与回到底部按钮的
      // 平滑下降（_progUntil 前的中间位置）
      const dist = el.scrollHeight - el.scrollTop - el.clientHeight;
      const ts = Date.now();
      const synthetic = ts - (view._renderTs || 0) < 350 || ts < (view._progUntil || 0);
      if (!synthetic && dist > FOLLOW_PIN_DIST) { view._pinned = true; view._pinTs = ts; }
      else if (!synthetic && dist < FOLLOW_RESUME && ts - (view._pinTs || 0) > PIN_GUARD_MS) view._pinned = false;
      // 分批加载：上翻接近已加载窗口顶即预取上一批（缓冲 500 条）；贴底跟随时
      // 武装「到顶释放」计时（60 秒不上翻就把上方批次放掉），一旦离底立刻取消
      maybeLoadOlder(view);
      if (dist > FOLLOW_PIN_DIST) cancelReleaseOlder(view);
      else if (!view._pinned && dist < FOLLOW_RESUME) armReleaseOlder(view);
      // 回到底部按钮：滚离底部出现，靠近底部消失（滞回）
      const was = !!view._jumpShow;
      const now = was ? dist > JUMP_HIDE : dist > JUMP_SHOW;
      if (now !== was && view._jumpBtn) {
        view._jumpShow = now;
        view._jumpBtn.classList.toggle('show', now);
      }
      // 输入区收起只作用于窄屏。收起会同步收走内容列底部垫高（syncComposerPad），
      // scrollHeight 随之变矮 pad——若收时离底不足「垫高+余量」，浏览器会把
      // scrollTop 钳到新底部并触发 scroll：近底判定翻转→立即展开→垫高长回→
      // 惯性滚动再收……一来一回就是「文字图像不断抽搐」。收起因此要求离底
      // 超过 pad+FOLLOW_RESUME：收完 dist 仍 >FOLLOW_RESUME，钳位不可能发生。
      if (!isNarrow()) return;
      const nearBottom = y + el.clientHeight >= el.scrollHeight - FOLLOW_RESUME;
      if (nearBottom) tuckComposer(view, false);
      else if (dy < -8 && y + el.clientHeight < el.scrollHeight - (view._cwH || 0) - FOLLOW_RESUME) tuckComposer(view, true); // 往回翻历史：收起
      else if (dy > 12) tuckComposer(view, false); // 向底部翻：展开
    }, { passive: true });
    view.input.addEventListener('focus', () => tuckComposer(view, false));
    // 输入区高度变化（autogrow/胶囊/提示条/排队坞）跟随同步垫高与按钮锚点
    const wrap = composerWrapOf(view);
    if (wrap && window.ResizeObserver) new ResizeObserver(() => syncComposerPad(view)).observe(wrap);
    syncComposerPad(view);
  }
}

$('btn-send').onclick = send;
$('btn-stop').onclick = () => stopRun(mainView);
$('btn-plus').onclick = () => openPlusMenu(mainView);
jxDiag('init:绑定A btn-send/stop/plus 完成');
$('sub-btn-plus').onclick = () => openPlusMenu(subView);
$('perm-chip').onclick = () => { popView = mainView; renderPermPop(popView); openPopover('perm-pop', $('perm-chip')); };
$('sub-perm-chip').onclick = () => { popView = subView; renderPermPop(popView); openPopover('perm-pop', $('sub-perm-chip')); };
$('model-chip').onclick = () => openModelCard(mainView);
$('sub-model-chip').onclick = () => openModelCard(subView);

/* ---------- 模型与思考档位卡片：滑杆/开关/刷新/两层的交互 ----------
   滑杆用 pointer 事件 + setPointerCapture：WebView2 里把滑块拖出卡片范围也不丢事件
   （触屏同样走 pointer，touch-action:none 关掉页面滚动抢手势）。
   拖动只更新大字预览，松手（pointerup）才提交一次 PATCH；pointercancel（被系统手势
   打断／卡片被收）直接回到真实状态、不提交。点轨道任意处＝按下即预览、松手提交，
   与拖动同一条路径，所以「点击＝跳档并立即提交」自然成立。 */
{
  const slider = $('mc-slider');
  slider.addEventListener('pointerdown', (e) => {
    if ($('mc-follow').checked) return;               // 跟随服务端默认时滑杆禁用
    if (e.pointerType === 'mouse' && e.button !== 0) return;
    e.preventDefault();
    mcDrag = { pid: e.pointerId, idx: mcLevelAt(e.clientX) };
    try { slider.setPointerCapture(e.pointerId); } catch (err) { /* 老内核不支持捕获，move 照样来 */ }
    mcPreview(mcDrag.idx);
    try { slider.focus({ preventScroll: true }); } catch (err) { /* 老内核无参对象 */ }
  });
  slider.addEventListener('pointermove', (e) => {
    if (!mcDrag || mcDrag.pid !== e.pointerId) return;
    e.preventDefault();
    mcPreview(mcLevelAt(e.clientX));
  });
  const endDrag = (commit) => (e) => {
    if (!mcDrag || (e && e.pointerId !== mcDrag.pid)) return;
    const idx = mcDrag.idx;
    const pid = mcDrag.pid;
    mcDrag = null;
    try { slider.releasePointerCapture(pid); } catch (err) { /* 已释放 */ }
    if (commit) setThinking(THINK_LEVELS[idx], popView); // 松手才提交：一次 PATCH
    else renderModelCard(popView);                       // 取消：回到真实档位，不留中间态
  };
  slider.addEventListener('pointerup', endDrag(true));
  slider.addEventListener('pointercancel', endDrag(false));
  // 键盘：左右（上下）各切一档并立即提交，role=slider 的标准交互
  slider.addEventListener('keydown', (e) => {
    const dir = (e.key === 'ArrowRight' || e.key === 'ArrowUp') ? 1
      : ((e.key === 'ArrowLeft' || e.key === 'ArrowDown') ? -1 : 0);
    if (!dir) return;
    const n = THINK_LEVELS.length;
    const cur = THINK_LEVELS.indexOf(curThinking(popView) || lastManualThink);
    const idx = Math.min(n - 1, Math.max(0, (cur < 0 ? THINK_LEVELS.indexOf(lastManualThink) : cur) + dir));
    e.preventDefault();
    setThinking(THINK_LEVELS[idx], popView);
  });
  // 「跟随服务端默认」：开＝thinking 空串（滑杆隐藏、大字 Default）；
  // 关＝恢复上一次手选的档位（没有历史就是 Medium）
  $('mc-follow').addEventListener('change', (e) => {
    setThinking(e.target.checked ? '' : (lastManualThink || 'medium'), popView);
  });
  // 模型行 → 第二层；返回键回第一层；右上角刷新重新拉模型列表
  $('mc-model').onclick = () => { mcShowLayer(2); mcRenderList(popView); $('mc-list').scrollTop = 0; };
  $('mc-back').onclick = () => mcShowLayer(1);
  $('mc-refresh').onclick = () => mcRefreshModels();
}
jxDiag('init:绑定B sub/chips 完成');
// 工作目录面板：每次打开重置搜索；搜索实时过滤目录列表
$('ws-row').onclick = () => {
  $('ws-search').value = '';
  $('ws-search-clear').classList.add('hidden');
  loadRmList(); // 远程工作区清单异步刷新（回来时面板开着会自动重渲）
  renderWsPop();
  $('ws-pop-notice').classList.add('hidden'); // 告知行跟随面板开合复位
  openPopover('ws-pop', $('ws-row'));
};

// 模式选择：与工作目录胶囊同款交互（未来 设计人员/编程人员 等模式照标准办公加选项）
$('mode-row').onclick = () => {
  renderModePop();
  openPopover('mode-pop', $('mode-row'));
};
$('ws-search').addEventListener('input', (e) => {
  $('ws-search-clear').classList.toggle('hidden', !e.target.value);
  renderWsPop();
});
$('ws-search-clear').onclick = () => {
  $('ws-search').value = '';
  $('ws-search-clear').classList.add('hidden');
  renderWsPop();
  $('ws-search').focus();
};
$('pm-upload').onclick = () => doUpload();
$('pm-plan').onclick = () => toggleModeFlag('planMode');
jxDiag('init:绑定C 菜单项 完成');
// 目标模式开启时要求填写目标文本；关闭时清状态（goalText 保留备查）
async function toggleGoalFlag() {
  const s = sessionOrDraftFor(popView);
  if (s.goalMode) {
    await patchDraftOrSession({ goalMode: false }, popView);
  } else {
    const text = prompt('目标模式：要达成的目标是什么？\n（模型将自主持续推进，直到达成或确认受阻）', s.goalText || '');
    if (text === null || !text.trim()) return;
    await patchDraftOrSession({ goalMode: true, planMode: false, goalText: text.trim() }, popView);
  }
  closePopovers();
}
$('pm-goal').onclick = toggleGoalFlag;
// 黄色模式药丸：点 ✕（或药丸本身）关闭该模式（两个输入区视图各一套）
for (const v of [mainView, subView]) {
  viewEl(v, 'mode-flags').addEventListener('click', (e) => {
    const pill = e.target.closest('.mflag');
    if (!pill || !pill.classList.contains('on')) return;
    // 药丸可能是主对话区那份（id=flag-plan）或右栏那份（id=sub-flag-plan）
    toggleModeFlag(pill.id.includes('goal') ? 'goalMode' : 'planMode', v);
  });
}
$('pm-export').onclick = () => doExport('md');
$('pm-export-json').onclick = () => doExport('json');
$('pm-remote').onclick = () => { remoteFormPrefill(); openPopover('remote-pop', viewEl(popView, 'btn-plus')); };
// 本对话的组件与插件：会话级覆盖浮窗（app-cxsession.js）。从加号菜单进，
// 先收掉弹层再开浮窗（modal 与 popover 互不打架）
$('pm-cxsession').onclick = () => {
  if (!(popView === mainView && state.cur)) return; // 只在主对话区且有会话时可见
  closePopovers();
  openCXSessionUI();
};
$('remote-badge').onclick = () => { remoteFormPrefill(); openPopover('remote-pop', $('remote-badge')); };
$('rm-auth').addEventListener('change', syncRemoteFormVis);
$('rm-test').onclick = testRemote;
$('rm-save').onclick = saveRemote;
$('rm-back').onclick = () => showRmStep(1);
$('rm-up').onclick = () => {
  let p = ($('rm-dir').value || '').trim() || rmCurPath;
  if (!p.startsWith('/')) p = rmCurPath;
  p = p.replace(/\/+$/, '');
  const i = p.lastIndexOf('/');
  browseRemote(i <= 0 ? '/' : p.slice(0, i));
};
$('rm-refresh').onclick = () => browseRemote(($('rm-dir').value || '').trim() || rmCurPath);
$('rm-dir').addEventListener('keydown', (e) => {
  if (e.key === 'Enter') { e.preventDefault(); browseRemote(($('rm-dir').value || '').trim()); }
});
$('rm-apply').onclick = applyRemote;
$('rm-clear').onclick = clearRemote;
// 目录列表：双击文件夹进入（文件行不可进）
$('rm-dir-list').addEventListener('dblclick', (e) => {
  const row = e.target.closest('.rm-entry');
  if (!row || row.classList.contains('file')) return;
  browseRemote(row.dataset.path);
});
$('file-input').addEventListener('change', (e) => { if (e.target.files.length) stageFiles([...e.target.files], uploadTarget); });
// 窄屏（手机远程访问）不支持新增工作目录：原生选择框弹在宿主机屏幕上，手机端
// 永远等不到——点了就地展开一行告知（文案与侧栏那颗按钮的提示卡同源，见
// app-core.js 的 NARROW_WS_ADD_NOTICE），不把请求发出去白等。
function toggleWsPopAddNotice() {
  const el = $('ws-pop-notice');
  if (el.classList.contains('hidden')) el.textContent = NARROW_WS_ADD_NOTICE;
  el.classList.toggle('hidden'); // 再点一次 = 收起，与侧栏告知卡同一套切换语义
}

// 「添加工作目录」：弹系统选择框，选中即设为新对话的工作目录并登记
$('ws-pop-add').onclick = async () => {
  if (isNarrow()) { toggleWsPopAddNotice(); return; } // 手机端：只给提示，不调后端
  const p = await pickWorkFolder();
  if (!p) return;
  homeDraft.remote = null; // 选了本地目录，放弃远程挂载
  homeDraft.workspace = p;
  rememberWorkspace(p);
  closePopovers();
  syncComposer();
};
// 「添加远程工作目录」：打开 SSH 表单（与加号菜单里的是同一个）
$('ws-pop-remote').onclick = () => { remoteFormPrefill(); openPopover('remote-pop', $('ws-row')); };
// 权限面板：单选立即生效，多选即时收集；完全访问 ⇄ 三项捆绑能力联动
// （切入强制勾上并置灰，切回「修改工作区」/「只读」自动取消勾选）
$('perm-pop').addEventListener('change', (e) => {
  const v = popView; // 面板是单例：改动作用在打开它的那个视图的会话上
  if (e.target.name === 'perm') {
    if (e.target.value === 'full') {
      const set = new Set(sessionOrDraftFor(v).tools || []);
      FULL_BUNDLED_TOOLS.forEach((t) => set.add(t));
      patchDraftOrSession({ permission: 'full', tools: [...set] }, v).then(() => renderPermPop(v));
    } else {
      const fields = { permission: e.target.value };
      if (sessionOrDraftFor(v).permission === 'full') {
        // 从完全访问切回：捆绑点亮的三项能力随之自动取消，但默认能力要补回来（修改工作区默认开启「操作浏览器」——切回时同时打开它）
        const _cur = sessionOrDraftFor(v);
        fields.tools = applyCapDefaults((_cur.tools || []).filter((t) => !FULL_BUNDLED_TOOLS.includes(t)), _cur.mode, e.target.value);
      }
      patchDraftOrSession(fields, v).then(() => renderPermPop(v));
    }
  } else if (e.target.dataset.tool) {
    const set = new Set(sessionOrDraftFor(v).tools || []);
    if (e.target.checked) set.add(e.target.dataset.tool); else set.delete(e.target.dataset.tool);
    patchDraftOrSession({ tools: [...set] }, v);
  }
});
// 点面板外收起所有弹出层。注意排除会打开弹出层的锚点按钮本身：
// 它们的 onclick 先开菜单，事件再冒泡到这里——不排除的话菜单会开完即关。
document.addEventListener('click', (e) => {
  if (!e.target.closest('.popover') && !e.target.closest('#btn-plus') &&
      !e.target.closest('#remote-badge') && // 顶栏「远程」徽标：点它打开 SSH 配置弹窗
      !e.target.closest('#perm-chip') && !e.target.closest('#model-chip') && !e.target.closest('#ws-row') &&
      !e.target.closest('#mode-row') &&
      !e.target.closest('#sub-dd') &&
      !e.target.closest('#skill-add-btn') && // 设置页「＋ 添加技能」弹层锚点
      !e.target.closest('#sp-add') && // 设置页「＋ 添加供应商」弹层锚点（搜索供应商下拉）
      !e.target.closest('.ex-model-chip') && // 设置页「子对话专家」的模型选择芯片锚点
      // 右栏子对话输入区那套同款控件也是弹层锚点
      !e.target.closest('#sub-btn-plus') && !e.target.closest('#sub-perm-chip') &&
      !e.target.closest('#sub-model-chip') &&
      !e.target.closest('.ws-tools') && !e.target.closest('.ws-acts') && !e.target.closest('.row-acts')) {
    closePopovers();
  }
});
document.addEventListener('keydown', (e) => {
  if (e.key !== 'Escape') return;
  hideSelPop();
  if (sideSearchOpen()) { closeSideSearch(); return; } // 搜索开着：优先关搜索
  mcCancelDrag(); // 模型与档位卡片里正在拖滑杆：Esc 关闭不提交中间态
  closePopovers();
});
/* ---------- 语音输入：实现已迁入组件 组件/voice/ ------------------------------------------
   原先在这里的语音输入实现（麦克风三态：空闲/连接中转圈/录音中脉冲、灰色暂定层、
   断线退避重连、时长倒计时徽标、一次性轻提示）已整体搬进「语音（朗读与听写）」
   组件：组件/voice/client.js。两个语音输入按钮的 DOM 仍在 index.html（位置、样式、
   图标全不变），但默认带 .hidden——按钮属于组件，组件加载成功才揭开并接管行为；
   组件关掉（设置 → 组件）或没加载到页面 = 按钮整组不出现。 */
/* ---------- 上下文圆环：悬停出明细卡（卡片顶部带「压缩」）----------
   交互就一套悬停：鼠标进圆环出卡片，从圆环挪到卡片上不消失（CSS 的 :hover 认整棵
   子树，圆环与卡片之间那 10px 空隙由 .ctx-tip::before 的透明块补上），离开圆环和
   卡片才收起。卡片里有按钮，所以悬停时 CSS 给卡片接上鼠标事件（平时只画不接事件）。
   不做点击/钉住：手机上点一下本来就会保持展开，桌面再叠一套点击只会多一种状态。
   主对话与右栏子对话各一枚（id 带 sub- 前缀，viewEl 解析）。 */
[mainView, subView].forEach((v) => {
  const ring = viewEl(v, 'ctx-ring');
  // 卡片固定 250 宽、贴着圆环右缘往外撑，窄屏手机上圆环太靠左会顶出屏幕左缘：
  // 悬停进圆环的这一刻按圆环实时位置把卡片收回视口（宽屏下算出来就是 CSS 的
  // right:-8px，外观不变）
  if (ring) ring.addEventListener('mouseenter', () => placeCtxTip(v));
});
// 对话区选中文字浮窗：松开鼠标出、选区消失/滚动/点外部收起；点浮窗不冲掉选区
$('sel-pop').addEventListener('mousedown', (e) => e.preventDefault());
$('sel-pop').addEventListener('click', (e) => {
  const b = e.target.closest('button');
  if (!b) return;
  const text = selKeepText;
  hideSelPop();
  if (b.dataset.act === 'copy') {
    navigator.clipboard.writeText(text).catch(() => {});
  } else if (b.dataset.act === 'compose') {
    // 挂成引用胶囊（显示截断、发送时才组装完整引用；见「引用胶囊」节）
    setQuote(mainView, text);
  } else if (b.dataset.act === 'aux') {
    askInAux(text);
  }
});
document.addEventListener('mouseup', (e) => {
  if (e.target.closest && e.target.closest('#sel-pop')) return;
  setTimeout(showSelPop, 10); // 等选区状态稳定
});
document.addEventListener('selectionchange', () => {
  const pop = $('sel-pop');
  if (pop && !pop.classList.contains('hidden') && !chatSelection()) hideSelPop();
});
['#messages', '#sub-messages'].forEach((id) => {
  const el = $(id);
  if (el) el.addEventListener('scroll', hideSelPop, { passive: true });
});
$('input').addEventListener('keydown', (e) => composerEnterKeydown(e, () => send()));
// 每键输入：量高与草稿落盘都不再同步做——autoGrow 合并到 rAF（field-sizing
// 内核下是 no-op，旧内核兜底也每帧最多一次），草稿防抖 300ms（切换上下文/
// 发送/关页前由 flushMainDraft 兜底，见 app-core.js）。旧版逐键同步做两件事，
// 在超大对话上放大逐键卡顿（见《前端渲染性能分析-主对话卡顿.md》）
let inputGrowRaf = 0;
$('input').addEventListener('input', () => {
  if (!inputGrowRaf) inputGrowRaf = requestAnimationFrame(() => { inputGrowRaf = 0; autoGrow(); });
  scheduleMainDraft();
});
window.addEventListener('beforeunload', () => { flushMainDraft(); flushSubDraft(); });
jxDiag('init:app-chat.js 顶层执行完毕');
