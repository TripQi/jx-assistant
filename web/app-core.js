/* app-core.js —— 共享地基：全局状态、视图抽象（mainView/subView）、通用工具、
   弹层开合、跨区共享字典与对话区宽度读写。六个脚本中第一个加载。
   文件拆分约定见《AGENT.md》开发规矩一节：原生 JS、无构建链，跨文件仅靠全局名引用。 */
/* 坚祥数字助理（GO版）前端逻辑：原生 JS，无框架、无构建步骤。风格对齐 DSH。 */

const $ = (id) => document.getElementById(id);

/* ---------- 品牌下发（服务端管理页「品牌」页签 → /announce → data/brand/ 本地缓存） ----------
 * 客户端无配置入口，只消费：文字经 /api/brand 取（未下发=空值用内置默认），
 * LOGO 走 /logo.png（Go 侧优先服务端下发的缓存图）。brand 事件（sync 应用公告
 * 后广播）到达时 loadBrandUI() 重拉刷新；resetEmpty 渲染首页 hero 时用
 * brandName()/brandHero() 动态取值。 */
window.BRAND = { name: '', heroTitle: '' };
window.brandName = () => (window.BRAND && window.BRAND.name) || '坚祥数字助理';
window.brandHero = () => (window.BRAND && window.BRAND.heroTitle) || '更懂制造业的数字助理';
function applyBrandTexts() {
  document.title = window.brandName();
  const sb = document.querySelector('.brand b'); if (sb) sb.textContent = window.brandName();
  const wz = $('wz-title'); if (wz) wz.textContent = '欢迎使用' + window.brandName();
  const ab = document.querySelector('#set-pane-about .about-block b'); if (ab) ab.textContent = window.brandName(); // 关于页只显示品牌名（「（GO版）」技术栈后缀 2026-10-30 起不再展示）
  const hh = document.querySelector('.home .hero h1'); if (hh) hh.textContent = window.brandHero();
  const ha = document.querySelector('.home .hero img'); if (ha) ha.alt = window.brandName();
}
async function loadBrandUI() {
  try {
    const r = await fetch('/api/brand', { cache: 'no-store' });
    if (!r.ok) return;
    const d = await r.json();
    window.BRAND = { name: d.name || '', heroTitle: d.heroTitle || '' };
    applyBrandTexts();
  } catch { /* 本地接口失败保持默认 */ }
}
loadBrandUI();

/* 桌面形态标记：桌面壳（runDesktop）导航的 URL 带 shell=desktop——右栏浏览器
 * 视图启用原生 WebView2 面板（矩形上报 + 工具栏），链接在右栏打开；网页模式
 * 一切照旧，链接新标签页打开。 */
if (new URLSearchParams(location.search).get('shell') === 'desktop') {
  document.documentElement.classList.add('desktop-shell');
}

/* Electron 壳标记（Electron 主进程加载本页时追加 &electron=1）：右栏浏览器
 * 面板改用页面内 <webview> 元素（DOM 布局，无矩形上报）；语音听写按钮保持
 * 隐藏——Web Speech API 在 Electron 没有免费识别通道，等本地 ASR 落地。 */
if (new URLSearchParams(location.search).get('electron') === '1') {
  document.documentElement.classList.add('electron-shell');
  window.JX_ELECTRON = true;
}

/* ---------- JX：插件接口（插件兼容层注入的浏览器半边经此接入原生界面） ---------- */
const JX = {
  _bus: {},
  _msgActions: [],       // fn(actsRow, msg)：向消息悬停操作行追加按钮
  _settingsSections: [], // {title, mount(el)}：设置弹窗分区
  _hooks: {},           // ev -> [fn(ctx)]：可等待钩子（区别于 _bus 的同步通知）
  on(ev, fn) { (this._bus[ev] = this._bus[ev] || []).push(fn); },
  // 退订事件（组件热卸载用）
  off(ev, fn) {
    const a = this._bus[ev];
    if (!a) return;
    const i = a.indexOf(fn);
    if (i >= 0) a.splice(i, 1);
  },
  emit(ev, payload) { (this._bus[ev] || []).forEach((f) => { try { f(payload); } catch (e) { console.warn('[JX]', ev, e); } }); },
  // 注册可等待钩子（组件 init 里调，dispose 里 unhook 对称注销）。钩子与 emit
  // 通知的区别：钩子是流程的一环——发射方 await 全部钩子落定后才继续（如 send
  // 在钩子返回前不组装消息正文），通知只求送到、不等结果。
  hook(ev, fn) { (this._hooks[ev] = this._hooks[ev] || []).push(fn); },
  unhook(ev, fn) {
    const a = this._hooks[ev];
    if (!a) return;
    const i = a.indexOf(fn);
    if (i >= 0) a.splice(i, 1);
  },
  // 顺序 await 全部钩子：单个钩子异常只告警不阻断；总超时（10 秒）到点放行并
  // 跳过剩余——组件卡死不能把发送卡死。超时后钩子自身仍会跑完，注册方要自兜
  // 迟到的副作用（听写组件的收尾即按此设计：自身等待上限 8 秒，小于这里）。
  async awaitHooks(ev, ctx) {
    const fns = (this._hooks[ev] || []).slice();
    if (!fns.length) return;
    const deadline = Date.now() + 10000;
    for (const f of fns) {
      const left = deadline - Date.now();
      if (left <= 0) { console.warn('[JX] 钩子', ev, '总超时，跳过剩余钩子'); return; }
      try { await Promise.race([Promise.resolve(f(ctx)), new Promise((r) => setTimeout(r, left))]); }
      catch (e) { console.warn('[JX] 钩子', ev, e); }
    }
  },
  registerMessageAction(fn) {
    this._msgActions.push(fn);
    // 补挂：本地组件毫秒级加载总赶在渲染前，远程走中继秒级——历史消息的操作行
    // 常在本钩子注册前就已渲染完毕（渲染时遍历不到它，按钮丢失）。对已渲染的行
    // 回放一次本钩子（行上的消息引用见 msgActions 的 acts._jxMsg）；时间戳先摘出
    // 再放回，保证补挂的按钮与正常渲染一样排在时间戳之前。行的渲染必然早于本次
    // 注册（push 在扫描前完成），不会重复挂载；组件热卸载由其 dispose 自清按钮。
    document.querySelectorAll('.msg-actions').forEach((row) => {
      if (!row._jxMsg) return;
      const t = row.querySelector('.act-time-end');
      if (t) t.remove();
      try { fn(row, row._jxMsg); } catch (e) { console.warn('[JX] action replay', e); }
      if (t) row.appendChild(t);
    });
  },
  // 注销消息操作行钩子（组件热卸载用：关掉后新渲染的操作行不该再带上它的按钮）
  unregisterMessageAction(fn) {
    const i = this._msgActions.indexOf(fn);
    if (i >= 0) this._msgActions.splice(i, 1);
  },
  registerSettingsSection(sec) { this._settingsSections.push(sec); },
  // 组件右键菜单贡献（app-ctxmenu 组装菜单时调用）：fn(ctx) 返回 [{label, fn,
  // disabled?}]，ctx = {kind:'selection'|'bubble', text, msgEl}。贡献项排在
  // 内置项之后，链路实现完全归组件——组件关掉必须 unregister，条目随组件消失。
  _ctxMenu: [],
  registerCtxMenu(fn) { this._ctxMenu.push(fn); },
  unregisterCtxMenu(fn) {
    const i = this._ctxMenu.indexOf(fn);
    if (i >= 0) this._ctxMenu.splice(i, 1);
  },
  // 汇总全部组件贡献的右键菜单项（单个组件出错只告警，不拦菜单）
  ctxMenuItems(ctx) {
    const out = [];
    for (const f of this._ctxMenu.slice()) {
      try {
        const items = f(ctx);
        if (Array.isArray(items)) out.push(...items.filter(Boolean));
      } catch (e) { console.warn('[JX] ctxmenu', e); }
    }
    return out;
  },
  // 插件向输入区左侧挂控件（按钮等），注册即生效；右栏子对话输入区镜像一份
  // （点击转发给原件执行，原件的 class 变化同步过来，两边外观/行为一致）
  addComposerControl(el) {
    const c = document.getElementById('plugin-composer-controls');
    if (c) c.appendChild(el);
    const sub = document.getElementById('sub-plugin-composer-controls');
    if (sub && el && el.nodeType === 1) {
      const copy = el.cloneNode(true);
      copy.addEventListener('click', (e) => { e.preventDefault(); e.stopPropagation(); el.click(); });
      new MutationObserver(() => { copy.className = el.className; copy.title = el.title; })
        .observe(el, { attributes: true, attributeFilter: ['class', 'title'] });
      sub.appendChild(copy);
    }
  },
};

/* ---------- 界面插槽（标准定位格式）：组件只「申请一个格子」，排版归插槽 ----------
   规矩：组件不拼条目 DOM、不写条目的布局样式，只交「图标 + 名字 + 序号 + 点击」。
   骨架、排序、以及同一份条目在电脑/手机两套形态下的排版都归插槽（CSS 在
   style.css 的插槽段，形态由插槽按屏幕决定，组件两套都不用管）。
   第一个插槽 sidebar.foot ＝ 左侧栏底部入口格：
   · 序号 order 小的排在最下面（紧贴设置行），大的往上叠；同序号先注册的更靠下。
     实现＝DOM 按 order 降序排——桌面竖排从上往下读就是序号大的在上，手机三列
     网格行优先铺格，序号小的自然落最后一行；
   · 内置「回收站」是第一位使用者（order 1，app-left.js 注册），组件从 2 起往上叠。
   加新插槽＝在 SLOT_HOSTS 里登记容器与条目类名 + 在 style.css 写它的两套形态。 */
const SLOT_HOSTS = {
  'sidebar.foot': {
    host: () => $('foot-apps') || document.querySelector('.sidebar-foot .foot-grid'),
    itemClass: 'foot-btn',
    labelClass: 'foot-btn-label',
  },
};
const slotReg = {};   // 插槽名 -> Map(条目 key -> {def, el, seq})
let slotSeq = 0;      // 全局注册序号：同 order 时先注册的排更下（DOM 更靠后）

// 插槽排序（唯一实现，组件不再各写一份）：order 降序 → 注册序号降序
function slotSort(host) {
  [...host.children].sort((a, b) => {
    const oa = parseFloat(a.dataset.order || '50');
    const ob = parseFloat(b.dataset.order || '50');
    if (oa !== ob) return ob - oa;
    return parseFloat(b.dataset.slotSeq || '0') - parseFloat(a.dataset.slotSeq || '0');
  }).forEach((el) => host.appendChild(el));
}

JX.slot = function (name) {
  const def0 = SLOT_HOSTS[name];
  if (!def0) throw new Error('未知界面插槽：' + name);
  const reg = slotReg[name] || (slotReg[name] = new Map());
  return {
    /* add：申请一个条目 → {el, setActive(on), remove()}。
       图标传内联 svg 字符串，标签是纯文本（插槽自己包成 .foot-btn-label——
       裸文本节点拿不到"最多两行、超出裁断"的规则）；owner 只用于自检展示。
       同 key 重挂会先撤旧的（组件热开关/热重载会走到这条路）。 */
    add(def) {
      const host = def0.host();
      if (!host) return null; // 容器缺席（异常加载序）：静默跳过，不炸组件
      const key = String(def.key);
      if (reg.has(key)) reg.get(key).el.remove();
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = def0.itemClass;
      if (def.id) btn.id = def.id;
      if (def.title) btn.title = def.title;
      btn.dataset.order = String(def.order == null ? 50 : def.order);
      const seq = ++slotSeq;
      btn.dataset.slotSeq = String(seq);
      btn.innerHTML = (def.icon || '') + '<span class="' + def0.labelClass + '"></span>';
      btn.querySelector('.' + def0.labelClass).textContent = def.label || '';
      if (typeof def.onClick === 'function') btn.onclick = def.onClick;
      host.appendChild(btn);
      slotSort(host);
      reg.set(key, { def, el: btn, seq });
      return {
        el: btn,
        setActive(on) { btn.classList.toggle('active', !!on); },
        remove() {
          btn.remove();
          const cur = reg.get(key);
          if (cur && cur.el === btn) reg.delete(key);
        },
      };
    },
    /* list：插槽现状，按显示顺序（序号小的在前＝更靠下）列出 */
    list() {
      return [...reg.entries()]
        .map(([key, v]) => ({
          key,
          order: parseFloat(v.el.dataset.order),
          seq: v.seq,
          label: v.el.querySelector('.' + def0.labelClass).textContent,
          owner: v.def.owner || '',
          active: v.el.classList.contains('active'),
        }))
        .sort((a, b) => a.order - b.order || a.seq - b.seq);
    },
  };
};

/* 自检：控制台敲 JX.slots() 看"每个插槽里有哪些条目、谁注册的、序号多少"
   （排查"界面上看着在那儿、背后是谁注册的"用） */
JX.slots = function () {
  const out = {};
  Object.keys(slotReg).forEach((name) => { out[name] = JX.slot(name).list(); });
  return out;
};

window.JX = JX;

const state = {
  sessions: [],
  cur: null,        // 当前会话 meta
  msgs: [],         // 当前会话已确认的消息
  streamText: '',   // 正在流式输出的助手文本
  streamThinking: '', // 正在流式输出的思考过程
  streaming: false,
  abort: null,
  pendingSteer: [], // 执行中已插话的消息（服务端为准）：模型调用中被立即打断生效；工具执行中等下一步调用模型注入
  pendingQueue: [], // 执行中排队的消息：本轮结束后自动作为独立一轮发送
  queuePaused: false, // 排队是否暂停（服务端为准，随 pending 快照下发）
  queueHeld: false,   // 执行结束后队列仍有保留（停止/出错/暂停完一轮）：横幅给「继续」入口
  defaultWs: '',    // 默认工作区（文件夹），来自 /api/status
  models: [],       // 可选模型清单，来自 /api/models
  defaultProvider: '',
  companyQuota: null, // 公司通道额度快照（/api/models 的 companyQuota，服务端下发才有）
  defaultModel: '',
  approvals: [],    // 待用户决定的越权审批（右下角卡片）
  questions: [],    // 待回答的助手提问 / 计划审批（右下角卡片）
};

async function api(path, opts = {}) {
  // 429 兜底重试：远程中继有「每 IP 并发挂起数」上限（jxaiserver relay.go，默认 8 路），
  // 切换对话时主对话整包＋右栏子对话并发拉取，慢链路下额度被挤爆会 429「中继并发过多」
  // （中继隧道层把它抛成带 status 的异常，见 remote-boot.js）。429 在中继转发前就被拒、
  // 业务侧未收到请求，重发安全（POST 也不会重复执行）；等 2 秒让占额度的慢请求释放后
  // 再试，最多自动重试 2 次，仍失败把最初错误抛给调用方——由既有错误 UI（如
  // 「对话加载失败＋重试」按钮）走手动。其余异常（网络断开等）不在兜底范围，照抛
  const send = () => fetch(path, {
    headers: { 'Content-Type': 'application/json' },
    ...opts,
  });
  let res;
  try {
    res = await send();
  } catch (e) {
    if (!e || e.status !== 429) throw e;
    for (let retry = 0; retry < 2 && !res; retry++) {
      await new Promise((r) => setTimeout(r, 2000));
      try { res = await send(); }
      catch (e2) { if (!e2 || e2.status !== 429) throw e2; }
    }
    if (!res) throw e;
  }
  const data = await res.json().catch(() => ({}));
  if (!res.ok) {
    // 错误体里的结构字段挂到 Error 上：文件回退的 409 带 blocked/unsafe
    // （哪些文件被外部改过、哪些可退）——前端要据此给出可操作的提示，
    // 只保留 message 会把这些信息丢掉
    const err = new Error(data.error || res.statusText);
    err.status = res.status;
    err.blocked = data.blocked;
    err.unsafe = data.unsafe;
    err.safe = data.safe;
    throw err;
  }
  return data;
}

function esc(s) {
  return String(s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
}

/* ---------- 安装包下载：后端静默下载 + 自动运行，界面零打扰 ----------
 * 「确认更新 / 下载完整安装包」统一走 POST /api/update/installer：后端把发布
 * 附件下载到 data/update-cache/，完成后自动运行安装包；旧安装包在每轮检查
 * 更新时清理。全程只有横幅行内提示（update_installer 事件），不弹窗、不开
 * 浏览器、不换页——旧实现（a[download].click() 顶层导航）在官网不可达时会把
 * 整个主界面变成 Edge 错误页（2026-10-31 客户故障，桌面壳另有导航守卫兜底）。 */
function jxDownload(u) {
  api('/api/update/installer', { method: 'POST', body: JSON.stringify({ url: String(u || '').trim() }) })
    .catch((e) => { toast('启动下载失败：' + e.message); });
}

/* ---------- 右键菜单的「朗读」取值（发音归语音组件，这里只管念什么） ----------
   「朗读」条目本身由语音组件经 JX.registerCtxMenu 贡献（组件关掉即整条消失，
   不会留下点了没反应的菜单项），应用本体不发音。这里只负责各菜单取文本的口径：
   **选区优先，其次被右键那条的可见文字**——用户选中一段就念那段（与对话区选区
   菜单一致），没选区就念整个条目/气泡；代码块、脚本、按钮标签一律跳过（念代码
   和按钮文字只是噪音，markdown 语法剥壳由组件的 cleanForSpeech 收尾）。
   放 app-core.js 是因为所有菜单（桌面壳 app-ctxmenu、左栏、右栏）都要用。 */

// 只念正文：这些标签里的东西不该念（代码块/行内代码/键盘提示/按钮与控件）
const SPEECH_SKIP_TAGS = ['PRE', 'CODE', 'KBD', 'SAMP', 'SCRIPT', 'STYLE', 'SVG', 'BUTTON', 'INPUT', 'TEXTAREA', 'SELECT', 'IMG'];

// 取节点可见文字（按文档顺序、跳过 SPEECH_SKIP_TAGS 与 .hidden 子树）
function speechTextOf(node) {
  const out = [];
  const walk = (n) => {
    if (!n) return;
    if (n.nodeType === 3) {
      const t = String(n.nodeValue || '').replace(/\s+/g, ' ').trim();
      if (t) out.push(t);
      return;
    }
    if (n.nodeType !== 1) return;
    if (SPEECH_SKIP_TAGS.indexOf(String(n.tagName || '').toUpperCase()) >= 0) return;
    if (n.classList && n.classList.contains('hidden')) return;
    const kids = n.childNodes || [];
    for (let i = 0; i < kids.length; i++) walk(kids[i]);
  };
  walk(node);
  return out.join(' ').replace(/\s{2,}/g, ' ').trim();
}

// 落在 node 内部的文字选区（选区跨元素时 anchor/focus 任一在里面即算）。
// 不认页面别处的旧选区：右键文件项时不该因为刚才在对话里划过一句就念那句。
function speechSelectionIn(node) {
  const sel = window.getSelection ? window.getSelection() : null;
  if (!sel || sel.isCollapsed || !sel.rangeCount) return '';
  const text = String(sel.toString() || '').trim();
  if (!text) return '';
  const host = (n) => {
    const el = n && (n.nodeType === 1 ? n : n.parentElement);
    return !!(el && node && (el === node || node.contains(el)));
  };
  return (host(sel.anchorNode) || host(sel.focusNode)) ? text : '';
}

// 菜单项末尾的「朗读 / 停止朗读」：问组件要（贡献项为空时连分隔线一起省掉）
function ctxSpeakItems(ctx) {
  const extra = (window.JX && JX.ctxMenuItems) ? JX.ctxMenuItems(ctx) : [];
  return extra.length ? [{ sep: true }, ...extra] : [];
}

// 路径取文件名（\ 与 / 都算分隔符；末尾分隔符忽略）
function speechFileName(p) {
  const s = String(p || '').trim().replace(/[\\/]+$/, '');
  return s ? s.split(/[\\/]/).pop() : '';
}

// 文件类目标的朗读口径：文件名 + 完整路径（路径末段就是文件名时只说一次）
function speechPathText(p) {
  const base = speechFileName(p);
  return (base && base !== p) ? ('文件 ' + base + '，路径 ' + p) : ('文件 ' + p);
}

/* ---------- 窄屏断点（CSS 宽 <500px）：浮层布局的总开关 ----------
   窄屏下左/右栏是整屏浮层（样式见 style.css 末节），各宽度引擎冻结：
   不推导 --rp-w、不写 --content-w，避免手机宽度污染桌面记忆
   （jx.sb / jx.contentWidth）。断点跨越回宽屏时恢复桌面推导。 */
const narrowMq = window.matchMedia ? window.matchMedia('(max-width: 499.98px)') : null;
// 窄屏「添加工作目录」的统一文案：原生选择框由后端弹在宿主机屏幕上，手机端点了
// 只会白等——侧栏工具按钮（app-left.js）与新对话页目录面板（app-chat.js）共用这句。
const NARROW_WS_ADD_NOTICE = '手机端不支持新增工作目录，请到电脑宽屏上操作';
function isNarrow() { return !!(narrowMq && narrowMq.matches); }

// 窄屏下收起左栏浮层：选中/新建会话等入口调用（右栏浮层保持用户手开的状态）。
// 走返回键同一个开合原语 jxPanelSet，别在别处直接动 sb-show 类
function narrowClosePanels() {
  if (isNarrow()) jxPanelSet('left', false);
  onLeftPanelClosed(); // 返回键守卫：收起后补历史哨兵（宽屏/已放行退出时内部自动跳过）
}

// 断点跨越（拖拽窗口跨 500px）：窄→宽清浮层类并恢复桌面记忆与右栏推导；
// 宽→窄不用处理——CSS 默认即收起左栏，右栏若开着自动变为浮层。
// mq 的 change 事件在部分嵌入浏览器里不随视口变化派发，resize 兜底同一清理
// （幂等：宽屏下重复执行只是重放 jx.sb 记忆与右栏推导）
function leaveNarrowIfNeeded() {
  if (isNarrow()) return;
  document.body.classList.remove('sb-show');
  // 手机端滚动收起的输入区复位（展开逻辑在 app-chat.js 的 tuckComposer）
  document.querySelectorAll('.composer-wrap.tucked').forEach((w) => {
    w.classList.remove('tucked');
    w.style.transform = '';
    w.style.marginBottom = '';
  });
  if (typeof syncComposerPad === 'function') { // 悬浮输入区：复位后重算垫高与按钮锚点
    syncComposerPad(mainView);
    syncComposerPad(subView);
  }
  applySidebarState(); // 恢复 jx.sb 记忆的左栏状态（内部会重算 rpApply）
  rpApply();
}
if (narrowMq) {
  const onNarrowMq = () => { if (!isNarrow()) leaveNarrowIfNeeded(); };
  if (narrowMq.addEventListener) narrowMq.addEventListener('change', onNarrowMq);
  else if (narrowMq.addListener) narrowMq.addListener(onNarrowMq); // 旧 Safari
}

/* ---------- 侧栏滑动手势（仅手机端／窄屏：方向×状态感知，触发即等效点击开合按钮） ----------
   只在手机端页面（CSS 宽 <500px 的浮层布局）里检测手势——那正是它存在的场景：
   手指滑动呼出/收起整屏浮层。宽屏（桌面布局）一律不检测：栏开在桌面上时，
   「在右栏里用鼠标横向拖拽选择文字」本身就是一次满足条件的横向拖拽，会被
   误判成收起手势把栏收掉（拖滚动条、别的横向拖拽同理）；宽屏开合走按钮/把手。
   定义（拉出两栏一致；收起左右两栏机制分开——按栏内操作性质各定，2026-11-04 用户裁决）：
   * 栏收着：从该侧屏幕缘往中间滑 → 拉出该侧栏（左缘右滑拉左栏、
     右缘左滑拉右栏；两栏都收着才拉得开，绝不把两栏同时打开）。
   * 左栏开着：栏身上任意位置向屏幕外（左）滑 → 收起。窄屏浮层整屏宽，
     等于屏幕任意处往左滑即收——左栏里基本只有点击类操作、没有横向滑动类
     操作，不会误触，任意滑最顺手（10-31 曾把两栏一起收进边缘热区，左栏
     因此变得不顺手，才有了左右分开这一次）。
   * 右栏开着：只认对侧屏幕缘热区（左缘右滑）＋返回键两条路——右栏要
     选文字、长按拖选，栏身上/页面其他位置的滑动不构成手势（把它们误判
     成收起正是 10-31 改边缘热区要治的病，右栏保持不动）。
   触发仍是单次位移 ≥44px 且横向主导，动作走 jxPanelSet——返回键、按钮、手势
   共用同一套开合原语（状态机、记忆、文案只有一处）。
   触摸走非被动 Touch 事件（横向确立即 preventDefault，阻断浏览器收编成滚动
   ——真机拉不出来的根因）；窄窗口里的鼠标/笔走 Pointer 事件。轻微上下漂移容忍。
   手势起点的目标豁免：拖宽把手（#rp-resize 等）、输入框——它们有自己的交互。 */
(function () {
  const EDGE = 24;    // 屏幕左右缘热区（px）：拉出手势与右栏收起手势的起点（左栏收起不限起点）
  const TRIGGER = 44; // 单向滑过此距离即触发一次
  const CLAIM = 10;   // 横向位移超过此值且未明显纵向 → 接管手势
  let g = null;

  const modalOpen = () => [...document.querySelectorAll('.modal')].some((m) => !m.classList.contains('hidden'));
  const leftOpen = () => document.body.classList.contains('sb-show'); // 仅窄屏语义：整屏浮层开着
  const rightOpen = () => !document.getElementById('rpanel').classList.contains('hidden');
  const verticalIntent = (dx, dy) => Math.abs(dy) > Math.abs(dx) * 1.5 + 12;
  // 手势与返回键是同一套逻辑：开合都调 jxPanelSet（app-left.js 的统一导航原语），
  // 手势这里只负责「往哪个方向＝要开还是要关」
  const fire = (side, close) => jxPanelSet(side, !close);

  // 起点判定（左右两栏机制分开）：左栏开＝栏身上任意位置武装收起、向屏幕外
  // （左）滑触发；其余只认屏幕缘热区——左缘右滑＝拉左栏/收开着的右栏，
  // 右缘左滑＝拉右栏/收开着的左栏；宽屏在 armAt 入口一票否决
  function armAt(target, x) {
    if (!isNarrow()) return null; // 宽屏（桌面布局）：不检测任何滑动，指针交互全归浏览器
    if (target && target.closest) {
      if (target.closest('#rp-resize, [class*="handle"], [class*="resize"], [class*="drag"]')) return null;
      if (target.closest('input, textarea, select, [contenteditable]')) return null;
      // 左栏收起：栏身上任意位置起手（浮层整屏宽＝屏幕任意处），向屏幕外滑收；
      // #rpanel 故意不配对——右栏收起只认边缘热区，栏身上滑动留给选字等交互
      if (target.closest('#sidebar') && leftOpen()) return { side: 'left', close: true };
    }
    const vw = window.innerWidth;
    if (x <= EDGE) { // 左缘热区：向右滑
      if (rightOpen()) return { side: 'right', close: true };
      if (!leftOpen() && !rightOpen()) return { side: 'left', close: false };
    }
    if (x >= vw - EDGE) { // 右缘热区：向左滑
      // 左栏开时浮层盖住顶栏以下全域，这里只兜 #sidebar 外的窄缝（顶栏右角等）
      if (leftOpen()) return { side: 'left', close: true };
      if (!leftOpen() && !rightOpen()) return { side: 'right', close: false };
    }
    return null;
  }

  // toward：位移在该栏「开向」上的分量（左栏向右为正、右栏向左为正）。
  // 拉出：≥TRIGGER（从该侧缘往中间滑入）触发；收起：≤-TRIGGER（右栏＝从
  // 对侧缘往中间滑到位，左栏＝栏身上向屏幕外滑）触发；反方向滑回则放弃
  const towardOf = (g2, x) => (x - g2.x0) * (g2.side === 'left' ? 1 : -1);
  const hit = (g2, toward) => g2.close ? toward <= -TRIGGER : toward >= TRIGGER;
  const quit = (g2, toward) => g2.close ? toward >= TRIGGER : toward <= -TRIGGER;

  document.addEventListener('touchstart', (e) => {
    if (g || e.touches.length !== 1 || modalOpen()) return;
    const t = e.touches[0];
    const arm = armAt(e.target, t.clientX);
    if (!arm) return;
    g = { ...arm, x0: t.clientX, y0: t.clientY, claimed: false, done: false, touch: true };
  }, { passive: true });
  document.addEventListener('touchmove', (e) => {
    if (!g || !g.touch || e.touches.length !== 1) return;
    const t = e.touches[0];
    const dx = t.clientX - g.x0, dy = t.clientY - g.y0;
    if (!g.claimed) {
      if (verticalIntent(dx, dy)) { g = null; return; } // 纵向：让位给滚动
      if (Math.abs(dx) < CLAIM) return;
      g.claimed = true;
      e.preventDefault(); // 锁定：浏览器不再把本次触摸收编为滚动
    } else {
      e.preventDefault();
    }
    const toward = towardOf(g, t.clientX);
    if (!g.done && hit(g, toward)) { g.done = true; fire(g.side, g.close); } // 一次手势只触发一次
    else if (quit(g, toward)) g = null;
  }, { passive: false });
  const touchEnd = () => { if (g && g.touch) g = null; };
  document.addEventListener('touchend', touchEnd);
  document.addEventListener('touchcancel', touchEnd);

  document.addEventListener('pointerdown', (e) => {
    if (g || e.pointerType === 'touch' || e.button !== 0 || modalOpen()) return;
    const arm = armAt(e.target, e.clientX);
    if (!arm) return;
    g = { ...arm, x0: e.clientX, y0: e.clientY, claimed: false, done: false, touch: false };
  });
  document.addEventListener('pointermove', (e) => {
    if (!g || g.touch) return;
    const dx = e.clientX - g.x0, dy = e.clientY - g.y0;
    if (!g.claimed) {
      if (verticalIntent(dx, dy)) { g = null; return; }
      if (Math.abs(dx) < CLAIM) return;
      g.claimed = true;
      e.preventDefault(); // 鼠标：横向确立后抑制拖动选中
    }
    const toward = towardOf(g, e.clientX);
    if (!g.done && hit(g, toward)) { g.done = true; fire(g.side, g.close); }
    else if (quit(g, toward)) g = null;
  });
  const pointerEnd = () => { if (g && !g.touch) g = null; };
  document.addEventListener('pointerup', pointerEnd);
  document.addEventListener('pointercancel', pointerEnd);
})();


window.addEventListener('resize', leaveNarrowIfNeeded);

/* ---------- Markdown 渲染（围栏代码块、标题、列表、表格、行内样式、链接） ---------- */

// 行内格式：代码 span → 粗体 → 链接（[文字](url) 与裸 http(s) URL）→ 文件路径。
// 链接带 ext-link 类，点击由 app-chat.js 的委托处理器接管——在内嵌浏览器
// （右栏面板）打开而非整页跳转；Ctrl/中键放行浏览器默认行为。
// 转义次序：代码 span 先摘出占位（其中 URL 不做链接化），其余文本逐段 esc
// 后拼链接，保证不破坏现有 XSS 防线。

// 文件路径识别（对话中途输出的文件地址 → file-link，点击右栏预览）：
// 盘符绝对路径或带分隔符的相对路径，且以字母开头的扩展名结尾（分隔符是
// 硬条件——不然 "Node.js" 这类词全会被误判）；空白与中西文标点断开。
// 含空格的路径在正文里无法安全判界（会吞掉后面的文字），只在反引号里认。
const FILE_PATH_RE = /(?:[A-Za-z]:)?(?:[^\s`'"<>|:,，。；（）【】《》「」]*[\\/])+[^\s`'"<>|:,，。；（）【】《》「」]*[A-Za-z0-9\u4e00-\u9fff_]\.[A-Za-z][A-Za-z0-9]{0,7}(?![\w.])/g;

// 反引号内容整体是否一个文件路径（模型习惯把产物路径用反引号包起来）：
// 有分隔符 → 允许空格（反引号就是边界），只要以扩展名结尾且无引号/竖线；
// 无分隔符 → 单文件名（config.json / My File.txt），最多一个空格。
function codeSpanIsPath(c) {
  if (/[<>|"']/.test(c)) return false;
  if (c.includes('\\') || c.includes('/')) {
    const body = c.replace(/^[A-Za-z]:/, '');
    if (body.includes(':')) return false;
    return /\.[A-Za-z][A-Za-z0-9]{0,7}$/.test(c);
  }
  if ((c.match(/ /g) || []).length > 1) return false;
  return /^[\w\u4e00-\u9fff][\w\u4e00-\u9fff .+-]*\.[A-Za-z][A-Za-z0-9]{0,7}$/.test(c);
}

// 纯文本段里的路径 → file-link（前后片段照常转义）
function pathLinks(seg) {
  if (!seg) return '';
  let out = '', last = 0, m;
  FILE_PATH_RE.lastIndex = 0;
  while ((m = FILE_PATH_RE.exec(seg))) {
    out += esc(seg.slice(last, m.index));
    out += '<a class="file-link" data-path="' + esc(m[0]) + '">' + esc(m[0]) + '</a>';
    last = m.index + m[0].length;
  }
  return out + esc(seg.slice(last));
}

function inlineFmt(s) {
  const codes = [];
  let t = s.replace(/`([^`\n]+)`/g, (_, c) => {
    codes.push(c);
    return '\x00' + (codes.length - 1) + '\x00';
  });
  // 裸 URL 只认 ASCII URL 字符：中文与全角标点（如句号逗号）处截断，避免把后面的正文吞进链接
  const linkRe = /\[([^\]\n]+)\]\((https?:\/\/[^\s)]+)\)|(https?:\/\/[A-Za-z0-9\-._~:\/?#\[\]@!$&'*+,;=%]+)/g;
  let out = '', last = 0, m;
  while ((m = linkRe.exec(t))) {
    out += pathLinks(t.slice(last, m.index));
    const href = m[1] !== undefined ? m[2] : m[0];
    const text = m[1] !== undefined ? m[1] : m[0];
    out += '<a class="ext-link" href="' + esc(href) + '" rel="noopener">' + esc(text) + '</a>';
    last = m.index + m[0].length;
  }
  out += pathLinks(t.slice(last));
  out = out
    .replace(/\x00(\d+)\x00/g, (_, i) => {
      const c = codes[+i];
      return codeSpanIsPath(c)
        ? '<code><a class="file-link" data-path="' + esc(c) + '">' + esc(c) + '</a></code>'
        : '<code>' + esc(c) + '</code>';
    })
    .replace(/\*\*([^*\n]+)\*\*/g, '<b>$1</b>');
  return out;
}

function renderBlocks(seg) {
  const lines = seg.split(/\r?\n/);
  const out = [];
  let i = 0;
  const isTableSep = (l) => /^\s*\|[\s:|-]+\|\s*$/.test(l);
  const isUl = (l) => /^\s*[-*]\s+\S/.test(l);
  const isOl = (l) => /^\s*\d+[.、]\s+\S/.test(l);
  const parseRow = (l) => l.trim().replace(/^\||\|$/g, '').split('|').map((c) => c.trim());

  while (i < lines.length) {
    const line = lines[i];
    if (!line.trim()) { i++; continue; }

    const h = line.match(/^(#{1,6})\s+(.*)$/);
    if (h) {
      const lv = Math.min(h[1].length + 1, 5); // 页面里 h2 起步
      out.push(`<h${lv}>${inlineFmt(h[2])}</h${lv}>`);
      i++; continue;
    }

    if (/^\s*\|.+\|\s*$/.test(line) && i + 1 < lines.length && isTableSep(lines[i + 1])) {
      const head = parseRow(line);
      i += 2;
      const rows = [];
      while (i < lines.length && /^\s*\|.+\|\s*$/.test(lines[i])) rows.push(parseRow(lines[i++]));
      let t = '<table><thead><tr>' + head.map((c) => `<th>${inlineFmt(c)}</th>`).join('') + '</tr></thead><tbody>';
      for (const r of rows) t += '<tr>' + r.map((c) => `<td>${inlineFmt(c)}</td>`).join('') + '</tr>';
      out.push(t + '</tbody></table>');
      continue;
    }

    if (isUl(line) || isOl(line)) {
      const ol = isOl(line) && !isUl(line);
      const items = [];
      while (i < lines.length && (ol ? isOl(lines[i]) : isUl(lines[i]))) {
        items.push(lines[i].replace(ol ? /^\s*\d+[.、]\s+/ : /^\s*[-*]\s+/, ''));
        i++;
      }
      out.push((ol ? '<ol>' : '<ul>') + items.map((it) => `<li>${inlineFmt(it)}</li>`).join('') + (ol ? '</ol>' : '</ul>'));
      continue;
    }

    const para = [];
    while (i < lines.length && lines[i].trim() &&
      !/^#{1,6}\s/.test(lines[i]) && !/^\s*\|/.test(lines[i]) && !isUl(lines[i]) && !isOl(lines[i])) {
      para.push(lines[i]); i++;
    }
    if (para.length) out.push(`<p>${inlineFmt(para.join('\n')).replace(/\n/g, '<br>')}</p>`);
    else { out.push(`<p>${inlineFmt(line)}</p>`); i++; } // 兜底：流式输出中的半成品表格行走到这里，必须强制前进，否则外层循环死循环卡死页面
  }
  return out.join('');
}

function md(text) {
  const parts = String(text ?? '').split('```');
  let html = '';
  parts.forEach((part, i) => {
    if (i % 2 === 1) {
      const code = part.replace(/^[a-zA-Z0-9_#+-]*\r?\n/, '');
      html += `<pre><code>${esc(code)}</code></pre>`;
    } else if (part.trim()) {
      html += renderBlocks(part);
    }
  });
  return html;
}

function relTime(s) {
  if (!s) return '';
  const t = new Date(String(s).replace(' ', 'T'));
  const diff = (Date.now() - t.getTime()) / 1000;
  if (isNaN(diff) || diff < 0) return '';
  if (diff < 60) return '刚刚';
  if (diff < 3600) return Math.floor(diff / 60) + '分钟';
  if (diff < 86400) return Math.floor(diff / 3600) + '小时';
  return Math.floor(diff / 86400) + '天';
}

function baseName(p) {
  if (!p) return '默认工作区';
  const seg = String(p).split(/[\\/]/).filter(Boolean);
  return seg.length ? seg[seg.length - 1] : p;
}
// 会话所属的工作区（文件夹）：会话能写哪个目录由它挂载的工作区决定，
// 未单独设置的挂在默认工作区下。
// lsGet/lsSet 兼容两种用法：对象/数组经 JSON 存取；纯字符串原样存取
// （读取时仅 { 或 [ 开头的值才做 JSON 解析，避免 '1'/'zh' 这类值被误转）。
function lsGet(k, dft) {
  try {
    const v = localStorage.getItem(k);
    if (v === null) return dft;
    const t = v.trim();
    if (t === 'null') return null;
    if (t.startsWith('{') || t.startsWith('[')) {
      try { return JSON.parse(t); } catch (e) { return v; }
    }
    return v;
  } catch (e) { return dft; }
}
function lsSet(k, v) {
  try { localStorage.setItem(k, typeof v === 'string' ? v : JSON.stringify(v)); } catch (e) {}
}
// 已知工作区 = 手动添加 ∪ 会话用过 ∪ 默认，再剔除被「彻底删除/清空」拉黑的目录。
// 默认工作区路径来自配置、没法注销，只能靠这份黑名单才能从列表里消失；
// 名下一旦又有会话，loadSessions 会按会话重新建组（不查黑名单），不会藏掉对话。
function knownWorkspaces() {
  const set = new Set(lsGet('jx.wslist', []));
  if (state.defaultWs) set.add(state.defaultWs);
  (state.sessions || []).forEach((x) => { if (x.workspace) set.add(x.workspace); });
  const hidden = lsGet('jx.wshidden', []);
  return [...set].filter((ws) => !hidden.includes(ws));
}

function wsOf(s) {
  return s.workspace || state.defaultWs || '';
}

/* ---------- 居中确认弹窗：破坏性操作（清空回收站/彻底删除）统一用它 ----------
   Promise 化的 confirm，卡片出现在屏幕正中；Esc/点遮罩=取消，Enter=确定。
   okOnly=true 时只留一个「确定」按钮，当提示框用。 */
let confirmResolve = null;
let confirmOkOnly = false;
function confirmCenter(msg, okOnly, up) {
  return new Promise((resolve) => {
    confirmResolve = resolve;
    confirmOkOnly = !!okOnly;
    $('confirm-msg').textContent = msg;
    $('confirm-cancel').style.display = confirmOkOnly ? 'none' : '';
    document.querySelector('#confirm-modal .confirm-card').classList.toggle('confirm-up', !!up);
    $('confirm-modal').classList.remove('hidden');
    $('confirm-ok').focus();
  });
}
function closeConfirm(v) {
  if ($('confirm-modal').classList.contains('hidden')) return;
  $('confirm-modal').classList.add('hidden');
  const r = confirmResolve; confirmResolve = null;
  if (r) r(v);
}
$('confirm-cancel').onclick = () => closeConfirm(false);
$('confirm-ok').onclick = () => closeConfirm(true);
$('confirm-modal').onclick = (e) => { if (e.target === $('confirm-modal')) closeConfirm(false); };
document.addEventListener('keydown', (e) => {
  if ($('confirm-modal').classList.contains('hidden')) return;
  if (e.key === 'Escape') { e.stopPropagation(); closeConfirm(false); }
  else if (e.key === 'Enter') { e.stopPropagation(); closeConfirm(true); }
});
/* 侧栏开合动画（左右侧栏共用同一个函数）：宽度从 from 过渡到 to，
   结束回调里做状态定版（如隐藏元素）。拖拽调宽时不受影响（不带 .anim 即时生效）。 */
let slideSeq = 0;
function slidePane(el, from, to, onEnd) {
  const tok = ++slideSeq;
  el._slideTok = tok; // 新的开合会顶掉旧动画的定版回调
  // 起始宽必须先于 .anim 提交：否则过渡起点是"开/合之前的旧宽度"（会先闪一下再滑）
  el.classList.remove('anim');
  el.style.width = el.style.minWidth = from + 'px';
  void el.offsetWidth; // 提交起始宽（此刻无过渡，直接落位）
  el.classList.add('anim');
  el.style.width = el.style.minWidth = to + 'px'; // 目标宽 → 触发 from→to 过渡
  let done = false;
  const h = (e) => {
    if (e.target !== el || e.propertyName !== 'width') return;
    fin();
  };
  const fin = () => {
    if (done || el._slideTok !== tok) return; // 已被更新的开合/拖拽取代
    done = true;
    el.removeEventListener('transitionend', h);
    el.classList.remove('anim');
    el.style.width = el.style.minWidth = '';
    if (onEnd) onEnd();
  };
  el.addEventListener('transitionend', h);
  setTimeout(fin, 460); // 兜底：transitionend 不来（嵌入浏览器节流）也定版
}

/* ---------- 数字助理按钮的小人动画（方案三：双循环随机轮播） ----------
   data-state 仍由 setFace 驱动（idle/thinking/done），触发点与旧版完全一致；
   这里在状态切换上挂一套动作调度（节奏照搬《干活小人方案三预览.html》）：
   - thinking（干活中）：循环一「低头读纸」/ 循环二「拿纸核对」随机轮播，
     循环间随机歇 1.2–2.8 秒；眨眼独立随机 2.6–6.8 秒；
   - idle（待机）：只眨眼；
   - done（完成微笑，约 4 秒后回 idle）：不跑循环，微笑交给 CSS（da-squish/da-pop）。
   动作通过给 #da-face 挂 is-* class 驱动（样式见 style.css 同名注释块）。 */

let faceTimer = null;
let dollGen = 0;      // 动画代际：setFace 换状态时自增，旧异步循环发现代际不符即退场
let dollState = null; // 已启动的状态。幂等：delta/reasoning 事件会高频调
                      // setFace('thinking')，状态没变就不重启循环——否则第一循环
                      // 永远被顶掉，动画僵在开头

const daRand = (a, b) => a + Math.random() * (b - a);
const daWait = (ms) => new Promise((r) => setTimeout(r, ms));

// 完成未读徽章（方案 C）：常驻 done（没看着对话）每来一次 +1；任何非 done 状态清零。
// 计数放 setFace（dollStart 有幂等，连续多次完成时数字要继续涨、弹跳重播）。
let daDoneUnread = 0;
function daBadgeSet(n) {
  const b = document.getElementById('da-badge');
  if (!b) return;
  if (n > 0) {
    b.textContent = n > 1 ? String(n) : '1';
    b.classList.remove('hidden', 'pop'); void b.offsetWidth;
    b.classList.add('pop');
  } else {
    daDoneUnread = 0;
    b.classList.add('hidden'); b.classList.remove('pop');
  }
}

// 循环一：低头读纸——纸在正中出现，眼睛慢慢转下去盯着它（盯久一点），再慢慢抬回，纸收起
async function daLoopStare(face, gen) {
  face.classList.add('is-read', 'is-stare');
  await daWait(900); if (gen !== dollGen) return;   // 慢慢转下去
  await daWait(2600); if (gen !== dollGen) return;  // 低头盯着中间那张纸，盯久一点
  face.classList.remove('is-stare');
  await daWait(900); if (gen !== dollGen) return;   // 慢慢抬回来
  face.classList.remove('is-read');
}
// 循环二：拿纸核对——纸出现随 1.1s 赶工起伏，中途飘起对一对，再落回收个尾
async function daLoopWork(face, gen) {
  face.classList.add('is-work');
  await daWait(800); if (gen !== dollGen) return;
  face.classList.add('is-float');
  await daWait(1400); if (gen !== dollGen) return;
  face.classList.remove('is-float');
  await daWait(700); if (gen !== dollGen) return;
  face.classList.remove('is-work');
  await daWait(1000);
}
// 干活主循环：每回掷硬币选循环一/二，循环间随机歇一口气
async function daWorkLoop(face, gen) {
  while (gen === dollGen) {
    if (Math.random() < 0.5) await daLoopStare(face, gen);
    else await daLoopWork(face, gen);
    await daWait(daRand(1200, 2800));
  }
}
// 眨眼独立随机：待机/干活都眨（随机 2.6–6.8 秒一次，一次 130ms）
async function daBlinkLoop(face, gen) {
  while (gen === dollGen) {
    await daWait(daRand(2600, 6800)); if (gen !== dollGen) return;
    face.classList.add('is-blink');
    setTimeout(() => { if (gen === dollGen) face.classList.remove('is-blink'); }, 130);
  }
}

// 状态切换：清场（代际自增让旧循环退场）后按新状态起新循环
function dollStart(st) {
  if (dollState === st) return;
  dollState = st;
  dollGen++;
  const face = document.getElementById('da-face');
  if (!face) return;
  ['is-read', 'is-stare', 'is-work', 'is-float', 'is-blink', 'is-cheer'].forEach((c) => face.classList.remove(c));
  if (st === 'thinking') { daWorkLoop(face, dollGen); daBlinkLoop(face, dollGen); daBadgeSet(0); }
  else if (st === 'idle') { daBlinkLoop(face, dollGen); daBadgeSet(0); }
  // 'done'（方案 C）：先把手里那张纸举过头顶晃两下（1.3s 后摘 class），随后微笑交给
  // CSS 常驻，不跑循环（眨眼也停——挤眼动画期间别再插一眨）；徽章计数在 setFace 里做
  else if (st === 'done') {
    face.classList.add('is-cheer');
    setTimeout(() => { if (dollState === 'done') face.classList.remove('is-cheer'); }, 1300);
  }
}

function setFace(st, revertMs) {
  const f = document.getElementById('da-face');
  if (!f) return;
  f.setAttribute('data-state', st);
  if (st === 'done' && !revertMs) daBadgeSet(++daDoneUnread); // 无回退＝常驻提醒（没看着对话）：算一条未读
  dollStart(st);
  if (faceTimer) { clearTimeout(faceTimer); faceTimer = null; }
  if (revertMs) faceTimer = setTimeout(() => {
    f.setAttribute('data-state', 'idle');
    dollStart('idle');
  }, revertMs);
}
dollStart('idle'); // 脚本在 body 末尾加载、da-face 已就绪：直接进待机眨眼

/* ---------- 子对话视图（右侧边栏首个视图：顶上下拉选/新建，下方内嵌完整对话） ---------- */

/* 对话视图描述：主对话与右栏子对话共用同一套渲染/发送函数（renderMessages /
   liveRender / handleEvent / send 全部按 view 参数化），只差 DOM 挂点与状态。 */
const mainView = {  key: 'main',
  get col() { return $('msg-col'); },
  get scroll() { return $('messages'); },
  get input() { return $('input'); },
  get hint() { return $('hint'); },
  get stopBtn() { return $('btn-stop'); },
  liveId: 'live-bubble',
  emptyHTML:
    '<div class="empty"><h2>开始新的对话</h2><p>在下方输入框发起任务；数字助理会自动派发子任务执行。<br>' +
    'API 地址填 <code>mock</code> 可离线体验完整链路。</p></div>',
  state, // 主对话的全局 state
};
const subState = {
  cur: null, msgs: [], streaming: false, abort: null,
  streamText: '', streamThinking: '', pendingSteer: [], pendingQueue: [],
  queuePaused: false, queueHeld: false,
};
const subView = {
  key: 'sub',
  get col() { return $('sub-msg-col'); },
  get scroll() { return $('sub-messages'); },
  get input() { return $('sub-input'); },
  get hint() { return $('sub-hint'); },
  get stopBtn() { return $('sub-stop'); },
  liveId: 'sub-live-bubble',
  emptyHTML: '<div class="empty"><h2>子对话</h2><p>在下方输入框向这个子对话发消息；它的工作目录与权限跟随主对话。</p></div>',
  state: subState,
};

function drawerFamilyId() {
  if (!state.cur) return null;
  return state.cur.parentId || state.cur.id; // 锚定在主对话（父）上
}

// 会话 id 反查它正展示在哪个视图（主列 or 右栏子对话）；都不在看则返回 null，
// 消息操作条据此决定编辑/分支作用于哪个会话、成功后切到哪个视图
function viewOf(sid) {
  if (state.cur && state.cur.id === sid) return mainView;
  if (subState.cur && subState.cur.id === sid) return subView;
  return null;
}

/* 输入区控件的视图参数化：两个视图各有一套底部控件（id 在右栏加 sub- 前缀），
   样式与行为完全共用；单例弹层（权限/模型/加号菜单）记住是谁打开的（popView），
   处理时就作用于那个视图的会话。 */
let popView = mainView; // 弹层归属视图（打开弹层时设置）
function viewEl(view, id) { return $((view || mainView).key === 'main' ? id : 'sub-' + id); }

// loadSessions 合并短时间内的多次触发：执行中会有成串的会话更新事件，
// 不合并就会打出一串同样的列表请求（也拖慢页面）。合并的是「请求」不是
// 「等待」：防抖窗口内的调用共享本轮刷新的同一个 promise——早先这里对窗口内
// 的调用立即假 resolve，await 拿到的是未刷新的旧列表，session_updated / env 行
// 触发的权限胶囊同步因此反复拿旧值停在旧档（审批「始终允许」升级权限的胶囊
// 滞后即源于此）。刷新完成后清空引用，下一次调用开启新一轮防抖周期。
let loadSessionsTimer = null, loadSessionsFlush = null;
function loadSessions() {
  if (loadSessionsFlush) return loadSessionsFlush;
  loadSessionsFlush = new Promise((resolve) => {
    loadSessionsTimer = setTimeout(() => {
      loadSessionsTimer = null;
      loadSessionsNow().then(
        () => { loadSessionsFlush = null; resolve(); },
        () => { loadSessionsFlush = null; resolve(); }, // 失败不抛：调用方按现状继续
      );
    }, 120);
  });
  return loadSessionsFlush;
}

// upsertSessionBrief 把刚创建的会话 meta（POST /fork、POST /api/sessions 的响应，
// 与列表项同型）就地合入 state.sessions。创建→切换的链路（分支/新建数字助理会话）
// 不能靠 await loadSessions 保证列表里有新会话：防抖窗口（120ms）内再次调用会立即
// 假 resolve，await 拿到的还是旧列表——openSession/openSubSession 找不到 brief、
// 乐观切换被跳过，界面停在旧会话上直到整包 GET 返回（分支瞬间闪现源会话内容、
// 窗口期里发的消息落进旧会话都源于此）。先合入再切换，brief 必命中；随后的真实
// 刷新整包覆盖，顺带校正排序。
function upsertSessionBrief(s) {
  if (!s || !s.id) return;
  const arr = state.sessions || (state.sessions = []);
  const i = arr.findIndex((x) => x.id === s.id);
  if (i >= 0) arr[i] = { ...arr[i], ...s };
  else arr.unshift(s); // 新会话此刻最活跃，置顶与列表的 updated_at DESC 排序一致
  renderSessionList();
}

/* ---------- 长期助理的版本（下拉选择，只在设置页一处） ----------
   主对话现在有多个「版本」（见 assistant_versions.go）：每一版都是独立的数字助理，
   老版本的历史 / 快照 / 交付原样留着，切回去接着聊。**入口只有一个**——版本不铺开成
   一串同样的条目，也不往侧栏塞控件：只在设置页「跟长期助理对话」那一节用一个下拉选
   当前用哪一版。当前是哪一版由服务端记在库里（/api/assistant/versions 的 current），
   前端只照它显示与切换。 */
const assistantVer = { current: '', versions: [] };

// refreshAssistantVersions 拉一次版本清单并重画下拉（进设置页、切换/新建之后走它）。
async function refreshAssistantVersions() {
  try {
    const d = await api('/api/assistant/versions');
    assistantVer.current = (d && d.current) || '';
    assistantVer.versions = (d && d.versions) || [];
  } catch (e) { assistantVer.current = ''; assistantVer.versions = []; }
  renderAssistantVersionPicker();
}

function assistantVersionName(v) { return v ? '第 ' + v.version + ' 版' : '第 1 版'; }

// 画设置页那枚下拉：按钮上写当前第几版，菜单里每项带消息数与最后更新时间，当前版打勾。
function renderAssistantVersionPicker() {
  const cur = assistantVer.versions.find((v) => v.current) || null;
  const labelEl = $('set-da-ver-label');
  if (labelEl) labelEl.textContent = cur ? assistantVersionName(cur) : '数字助理';
  const menuEl = $('da-set-ver-menu');
  if (!menuEl) return;
  const list = assistantVer.versions;
  if (!list.length) {
    menuEl.innerHTML = '<div class="da-ver-empty">还没有版本：点上面的「新建一版」</div>';
    return;
  }
  menuEl.innerHTML = list.map((v) => (
    '<button class="da-ver-item' + (v.current ? ' cur' : '') + '" type="button" data-id="' + esc(v.sessionId) + '">' +
      '<span class="da-ver-name">' + assistantVersionName(v) + (v.running ? '　运行中' : '') + '</span>' +
      '<span class="da-ver-meta">' + (v.messages || 0) + ' 条消息 · ' + esc(v.updatedAt || '-') + '</span>' +
    '</button>'
  )).join('');
  menuEl.querySelectorAll('.da-ver-item[data-id]').forEach((b) => {
    b.onclick = async () => {
      closeAssistantVersionMenu();
      if (b.classList.contains('cur')) return; // 已经是当前这一版：不做无谓的切换
      b.disabled = true;
      try {
        const r = await switchAssistantVersionTo(b.dataset.id);
        if (typeof assistantOpsState === 'function') {
          const no = r && r.version ? ('第 ' + r.version + ' 版') : '所选版本';
          assistantOpsState('已切到' + no + '：主对话区显示的就是这一版的历史，之后的对话也记在这一版里。');
        }
      } catch (e) {
        alert('切换版本失败：' + e.message);
      }
    };
  });
}

function closeAssistantVersionMenu() {
  const el = $('da-set-ver-menu');
  if (el) el.classList.add('hidden');
}

// 下拉按钮：点一下展开/收起（内容由上面的渲染填好；点别处自动收起）。
{
  const btn = $('btn-set-da-ver');
  const menu = $('da-set-ver-menu');
  if (btn && menu) {
    btn.onclick = (e) => {
      e.stopPropagation();
      const willOpen = menu.classList.contains('hidden');
      closeAssistantVersionMenu();
      if (willOpen) menu.classList.remove('hidden');
    };
  }
}

// 点下拉自身以外的地方收起菜单（菜单项自己点完也会收起）。
document.addEventListener('click', (e) => {
  const t = e.target;
  if (t && t.closest && t.closest('.da-ver-row')) return;
  closeAssistantVersionMenu();
});

// switchAssistantVersionTo 切到某一版：只改服务端「当前版本」指针，两边的会话与
// 历史一件不动。切完正开着某一版就把对话区换成目标版；其余地方只刷新列表与下拉。
async function switchAssistantVersionTo(id) {
  const r = await api('/api/assistant/versions/switch', { method: 'POST', body: JSON.stringify({ sessionId: id }) });
  try { await loadSessions(); } catch (e) { /* 列表刷新失败不影响切换结果 */ }
  if (state.cur && state.cur.mode === 'assistant' && !state.cur.parentId) openSession(id);
  await refreshAssistantVersions(); // 下拉按新状态重画（状态行由调用方自己写）
  return r;
}

/* ---------- 本地草稿持久化：刷新不丢输入与选择 ----------
   缓存模型：输入框内容永远只属于「当前上下文」——
   首页（新对话）一份 jx.homeDraft（含选择项 + text，跨重启保留），
   每个主对话一份 jx.st.<id>，每个子对话一份 jx.subst.<id>。
   三条铁律：
   1) 输入即时落盘（input 事件与程序化写入都走 persist*）；
   2) 切换上下文时输入框整体替换为目标上下文的草稿（没有就是空，绝不残留上一个的字）；
   3) 发送成功即清对应上下文的草稿（首页发送清 homeDraft.text，会话/子对话清各自 key）。 */

function sessionDraftKey(id) { return 'jx.st.' + id; }
function subDraftKey(id) { return 'jx.subst.' + id; }

function persistHomeDraftLS() {
  if (state.cur) return; // 只在首页上下文保存
  try {
    homeDraft.text = $('input').value;
    localStorage.setItem('jx.homeDraft', JSON.stringify({ ...homeDraft, text: homeDraft.text }));
  } catch (e) { /* 隐私模式下忽略 */ }
}

function persistSessionDraftLS() {
  if (!state.cur) return;
  try {
    const v = $('input').value;
    if (v) localStorage.setItem(sessionDraftKey(state.cur.id), v);
    else localStorage.removeItem(sessionDraftKey(state.cur.id));
  } catch (e) {}
}

function persistSubDraftLS() {
  if (!subState.cur) return;
  try {
    const v = $('sub-input').value;
    if (v) localStorage.setItem(subDraftKey(subState.cur.id), v);
    else localStorage.removeItem(subDraftKey(subState.cur.id));
  } catch (e) {}
}

// 主输入框的统一落盘入口：首页与会话草稿按当前上下文各写各的
function persistMainDraftLS() {
  persistHomeDraftLS();
  persistSessionDraftLS();
}

// 草稿落盘的防抖包装：input 事件逐键同步写 localStorage 在输入快/存储慢时
// 白费（上午排查报告路径三）；300ms 尾随合并。执行时读当下输入框与上下文，
// 不捕获参数——期间发生的清空/切换会话会让晚到的落盘写空值或新上下文的值，
// 都幂等无害。
// 切换对话时必须「在 cur 改写之前」flush（optimisticSwitch / loadSessionInto /
// openSubSession / goHomeNewTask / subClearCurrent 各自负责）：落盘按当前 cur 认
// 目标对话，cur 换成目标之后才 flush 会把上一个对话的字写进目标对话的草稿键。
// 覆盖输入框（applySessionChrome / resetEmpty）与关页（beforeunload）前也要 flush，
// 保证最后 300ms 内的输入不丢。
let mainDraftTimer = 0, subDraftTimer = 0;
function scheduleMainDraft() {
  clearTimeout(mainDraftTimer);
  mainDraftTimer = setTimeout(persistMainDraftLS, 300);
}
function flushMainDraft() {
  clearTimeout(mainDraftTimer);
  mainDraftTimer = 0;
  persistMainDraftLS();
}
function scheduleSubDraft() {
  clearTimeout(subDraftTimer);
  subDraftTimer = setTimeout(persistSubDraftLS, 300);
}
function flushSubDraft() {
  clearTimeout(subDraftTimer);
  subDraftTimer = 0;
  persistSubDraftLS();
}

function clearSessionDraftLS(id) {
  try { localStorage.removeItem(sessionDraftKey(id)); localStorage.removeItem('jx.q.' + id); } catch (e) {}
}

// 首页草稿只清 text，保留工作目录/模式等选择项
function clearHomeDraftLS() {
  homeDraft.text = '';
  homeDraft.quote = ''; // 发出即清：引用随消息走了
  try { localStorage.setItem('jx.homeDraft', JSON.stringify({ ...homeDraft, text: '' })); } catch (e) {}
}

function clearSubDraftLS(id) {
  try { localStorage.removeItem(subDraftKey(id)); localStorage.removeItem('jx.subq.' + id); } catch (e) {}
}

// 会话彻底删除时连带清掉它的本地痕迹（主草稿/子草稿与内存里的过程缓存）
function removeSessionLocalData(id) {
  try {
    localStorage.removeItem(sessionDraftKey(id));
    localStorage.removeItem(subDraftKey(id));
    localStorage.removeItem('jx.q.' + id); // 会话删除：引用草稿的本地痕迹一并清掉
    localStorage.removeItem('jx.subq.' + id);
  } catch (e) {}
  delete rpSess[id]; // 右栏的会话作用域状态（app-right.js）：会话没了，状态跟着作废
  delete subActs[id];
  delete subDone[id];
  delete dotDone[id];
  Object.keys(turnOpen).forEach((k) => { if (k.indexOf(id + ':') === 0) delete turnOpen[k]; });
}

function restoreHomeDraftLS() {
  try {
    const d = JSON.parse(localStorage.getItem('jx.homeDraft') || 'null');
    if (!d) return;
    homeDraft.workspace = d.workspace || '';
    homeDraft.mode = CHAT_MODES.some((m) => m.v === d.mode) ? d.mode : 'office';
    homeDraft.permission = d.permission || 'workspace-write';
    // 「创建技能」模式的权限联动标记（app-chat.js 的 applyDraftMode 写入）：
    // 恢复它，切换模式时才能识别「完全访问是联动自动设的」并随切换还原
    homeDraft._fullFromMode = !!d._fullFromMode;
    homeDraft.provider = d.provider || '';
    homeDraft.providerKey = d.providerKey || ''; // 老草稿没有这一项：空＝按模型名找回 Key
    homeDraft.model = d.model || '';
    homeDraft.thinking = normThinkLevel(d.thinking); // 老草稿的 enabled/disabled 等旧值归一（见 app-chat.js）
    homeDraft.tools = Array.isArray(d.tools) ? d.tools : [];
    homeDraft.planMode = !!d.planMode;
    homeDraft.goalMode = !!d.goalMode;
    homeDraft.goalText = d.goalText || '';
    // 计划/目标二选一：旧数据可能两个都存着，收敛为只留计划
    if (homeDraft.planMode && homeDraft.goalMode) homeDraft.goalMode = false;
    homeDraft.remote = (d.remote && d.remote.host) ? d.remote : null;
    homeDraft.text = typeof d.text === 'string' ? d.text : '';
    homeDraft.quote = d.quote || ''; // 引用暂存：新版字符串数组（多条）/ 旧版单条字符串，用前经 parseQuotesLS 归一
  } catch (e) {}
}

/* ---------- 跨区共享字典：左栏会话徽点/搜索定位、对话渲染、全局事件流共用 ---------- */
// 回合展开记忆：key = 会话id:回合首条seq。手动展开过的回合不再被自动折回——
// 流式期间新来的思考/工具行、回合收尾重渲都保持展开（数字助理2 修过的同类 bug）
const turnOpen = {};
/* 数字助理派发/转达的子任务卡片：后台执行期间显示实时工具活动，完成后标已完成 */
const subActs = {}; // 子会话id -> 最近几条工具活动
const subDone = {}; // 子会话id -> 是否已完成（内存态；跨刷新由回报消息补判，见 reportedSubs）
const dotDone = {};     // 会话id -> 完成未查看（绿点）

/* ---------- 首页草稿与弹层开合（弹出面板系统的共享半边） ---------- */
// tools 默认带 'browser'：权限档位＝修改工作区时默认开启「操作浏览器」（后端
// perm.go 的 capDefaultsForMode 是同一策略的唯一权威）。芯片上默认就是勾选态，
// 用户取消后前端显式传空数组，后端尊重用户、不再覆盖。
// providerKey：模型归属的那把 Key（自定义供应商多 Key 后同名模型靠它定位；
// 单 Key 供应商为空）。
const homeDraft = { mode: 'office', permission: 'workspace-write', provider: '', providerKey: '', model: '', thinking: '', tools: ['browser'], planMode: false, goalMode: false, goalText: '', remote: null, quote: '' };

// 模型菜单里展开思考档位浮窗的模型 key（app-chat.js 的模型菜单在用）。
// 放这半边是因为它属于弹层开合状态：任何关闭路径（Esc / 点外面 / 切别的
// 弹层 / 选中收起）都要把它清零，重开菜单才不会把上次的二级浮窗又弹出来。
let modelPopExpand = '';

function closePopovers() {
  document.querySelectorAll('.popover').forEach((p) => p.classList.add('hidden'));
  modelPopExpand = '';
  closeAllSelects();
}

function openPopover(popId, anchor) {
  const pop = $(popId);
  const reopening = pop.classList.contains('hidden');
  closePopovers();
  if (!reopening) return; // 点同一个按钮 = 收起
  pop.classList.remove('hidden');
  reposPopover(popId, anchor);
}

// reposPopover 把一个已打开的弹窗重新对齐锚点（内容高度变化后重摆位置）。
// 不做开合切换——openPopover 的 toggle 语义（已开着就收起）不适合这种场景：
// 历史上 showRmStep 误用它「重对齐」，结果远程工作区弹窗在第一步→第二步时
// 自己把自己关掉了，看起来就像什么都没保存。
function reposPopover(popId, anchor) {
  const pop = $(popId);
  if (pop.classList.contains('hidden') || !anchor) return;
  const r = anchor.getBoundingClientRect();
  const pw = pop.offsetWidth, ph = pop.offsetHeight;
  let x = Math.min(r.left, window.innerWidth - pw - 12);
  if (r.left + pw > window.innerWidth - 12) x = Math.max(12, r.right - pw);
  let y = r.top - ph - 8; // 输入区在底部，面板向上弹出
  if (y < 12) y = Math.min(r.bottom + 8, window.innerHeight - ph - 12);
  pop.style.left = x + 'px';
  pop.style.top = y + 'px';
}

/* ---------- 对话区宽度拖拽：左右把手对称调宽，偏好记入本机 ---------- */

function contentWidth() {
  return parseFloat(getComputedStyle(document.documentElement).getPropertyValue('--content-w')) || 860;
}

function setContentWidth(px, save) {
  if (isNarrow()) return; // 窄屏浮层下对话区恒为全宽，不写宽度也不落盘（防污染桌面记忆）
  // 夹取 640px ~ 滚动区可用宽-176（每侧留 88px 边缘预算）
  const max = Math.max(640, (($('messages') && $('messages').clientWidth) || window.innerWidth) - 176);
  px = Math.round(Math.min(Math.max(px, 640), max));
  document.documentElement.style.setProperty('--content-w', px + 'px');
  if (save) lsSet('jx.contentWidth', String(px));
}

function restoreContentWidth() {
  const px = parseFloat(lsGet('jx.contentWidth', ''));
  if (px >= 640) setContentWidth(px);
}

/* ---------- 输入框自动增高（主/子输入区与语音共用） ---------- */
// 支持 field-sizing:content 的内核（WebView2 / Chromium≥123）下高度由 CSS 原生
// 自适应，这里只清掉旧版本可能写下的内联高度（内联 height 会盖住样式表），
// 量高路径完全不跑——旧路径「写 auto→读 scrollHeight→写回」每次按键都强制
// 同步布局，且 min-height:72px 决定了 1~3 行输入高度根本不变，绝大多数按键
// 是纯浪费；在几百条消息常驻 DOM 的主对话上就是逐键卡顿的主凶（见根目录
// 《前端渲染性能分析-主对话卡顿.md》）。旧内核（iOS Safari 等）兜底走原量高。
function autoGrow(t) {
  t = t || $('input');
  if (typeof CSS !== 'undefined' && CSS.supports && CSS.supports('field-sizing', 'content')) {
    if (t.style.height) t.style.height = '';
    return;
  }
  t.style.height = 'auto';
  t.style.height = Math.min(t.scrollHeight, 200) + 'px';
}

/* ---------- 输入框回车键发送/换行统一处理（主/子输入区共用） ----------
   发送快捷键两档（jx.sendkey，设置页可切）：
   · enter 档（默认）：回车发送；Shift+回车、Ctrl/Cmd+回车都是换行；
   · ctrl 档：Ctrl/Cmd+回车发送；回车、Shift+回车都是换行。
   两种组合键都支持换行（Ctrl+回车在浏览器里没有任何保留绑定，不冲突）；
   换行优先 execCommand 插入（保留原生撤销），引擎不接则回落浏览器默认插入。
   输入法组字中的回车是「确认候选」，必须放行给 IME，不能当成发送。
   发送前停语音听写的钩子在 send() 执行点（app-chat.js），本函数不用管。 */
function composerEnterKeydown(e, doSend) {
  if (e.key !== 'Enter' || e.isComposing) return;
  if (lsGet('jx.sendkey', 'enter') === 'ctrl') {
    if (e.ctrlKey || e.metaKey) { e.preventDefault(); doSend(); }
    return;
  }
  if (!e.shiftKey && !e.ctrlKey && !e.metaKey) { e.preventDefault(); doSend(); return; }
  try { if (document.execCommand('insertLineBreak')) autoGrow(e.target); } catch (err) { /* 走默认 */ }
}

/* ---------- 输入框占位文案跟随发送快捷键设置（jx.sendkey） ---------- */
// sendKeyHint：快捷键提示短语（渲染路径每次刷新占位符时统一取这里，别写死）。
// 语音组件（组件/voice）经 window.JXVoiceHint 交来它的快捷键提示片段，追加在
// 发送提示之后（组件卸载时置空）。
function sendKeyHint() {
  const base = lsGet('jx.sendkey', 'enter') === 'ctrl'
    ? 'Ctrl+回车键发送，回车键 或 Shift+回车键换行，可到设置里更改快捷键'
    : '回车键发送，Shift+回车键 或 Ctrl+回车键换行，可到设置里更改快捷键';
  return base + (window.JXVoiceHint || '');
}
function updateSendKeyHint() {
  const el = $('input');
  if (!el) return;
  // 执行中占位符带着插话/排队提示段（setStreamUI 追加的）：这里只更新存货，
  // 流结束由 setStreamUI 拼上新文案还原，别把执行中的提示段顶掉
  if (el.dataset.runPh) { el.dataset.idlePh = sendKeyHint(); return; }
  el.placeholder = sendKeyHint();
}

// 执行中追加在输入框占位符末尾的插话/排队提示段（灰字）。完整占位符＝
// 发送快捷键提示 ＋ 语音组件的语音提示（window.JXVoiceHint）＋ 本段（仅执行中）。
// setStreamUI（app-chat.js）负责换上/还原；语音组件刷新子输入框占位时也带上它
// （组件/voice/client.js 的 refreshShortcutPlaceholders）。
const RUN_HINT_SEGMENT = '；执行中：输入内容可插话或排队发送';

/* ---------- 轻提示（toast） ---------- */
// toast：右下角短暂浮出的非阻塞通知（静默组件更新完成等「不打扰但留痕」的
// 场景；与 alert 的区别是不抢焦点）。同一时刻只保留一条，新的顶掉旧的。
let __toastTimer = 0;
function toast(msg, ms) {
  const el = $('app-toast');
  if (!el) return;
  el.textContent = msg;
  el.classList.remove('hidden');
  clearTimeout(__toastTimer);
  __toastTimer = setTimeout(() => el.classList.add('hidden'), ms || 5000);
}
