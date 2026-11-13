/* 组件：企业技能包 / 工作流（市场）—— 浏览器半边。
 *
 * 原本体 web/app-market.js 的市场部分与 index.html 里的三块硬编码
 * （侧栏按钮 / #market-view 视图 / 上传弹层）整体迁入本组件：后端
 * （market.go 的 /api/market*）保留在主程序，入口与页面由组件掌管——
 * 不需要的人关掉开关或摘除 组件/skill-market/ 目录，侧栏入口、市场页、
 * 设置里的服务器地址一并消失，主程序其余功能不受影响。
 *
 * 机制：cx.js 注入本脚本 → CX.register 注册 → init 建界面（按钮 append 进
 * .foot-grid、视图插在 #main 顶栏后、弹层挂 body 尾），dispose 完整撤销
 * （关视图、清定时器与监听、删 DOM 与 window 接口、注销设置区）。
 * 技能/组件/插件/MCP 四 tab + 搜索 + 一键安装/更新/卸载；装/卸后调
 * CX.refresh() 热加载/热卸载组件，不刷新页面。样式在 client.css（mk-*）。 */
(function () {
  if (!window.CX) return;

  const ID = 'skill-market';

  /* 运行期状态（对应原 app-market.js 的模块级变量） */
  let mkData = null;        // GET /api/market 的最近一次结果
  let mkTab = 'skill';      // 当前 tab：skill | component | plugin | mcp
  let mkLoading = false;    // 清单在途（防抖动重复渲染）
  let mkAutoTimer = 0;      // 市场页开着时的 60s 静默复查
  let btnEl = null, btnEntry = null, viewEl = null, modalEl = null; // 本组件注入的节点（btnEntry＝插槽条目句柄）
  let legacyHost = false;  // 老前端硬编码形态（组件化之前的产品）：按钮/视图是 index.html 硬编码的，本组件整体让位
  const docKeys = [];       // document keydown 监听（dispose 逐个解绑）

  /* ---------- 小工具（$ / esc 自带，api / confirmCenter / openSettings 用主程序全局） ---------- */

  const $ = (id) => document.getElementById(id);
  const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));
  const addDocKey = (fn) => { document.addEventListener('keydown', fn); docKeys.push(fn); };

  /* ---------- DOM 注入（HTML 原样来自原 index.html，id/class 不变） ---------- */

  function injectDOM() {
    // 侧栏入口：向底部插槽申请一个条目（order 2，排在回收站 1 之上；组件注入
    // 先后由资产加载竞速决定，位置只认插槽的序号排序）。骨架、排序、电脑竖排 /
    // 手机三列网格全归插槽——见 app-core.js 的 JX.slot 与 AGENT.md「界面插槽」
    btnEntry = (window.JX && JX.slot) ? JX.slot('sidebar.foot').add({
      key: 'skill-market', id: 'btn-market', order: 2, owner: '组件 skill-market',
      title: '企业技能包 / 工作流：安装/更新 技能、组件与插件',
      label: '企业技能包 / 工作流',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 7h14l-1.2 12a1.5 1.5 0 0 1-1.5 1.4H7.7A1.5 1.5 0 0 1 6.2 19z"/><path d="M8.5 10V6a3.5 3.5 0 0 1 7 0v4"/></svg>',
    }) : null;
    if (btnEntry) {
      btnEl = btnEntry.el;
    } else {
      // 旧 engine 没有插槽（组件块先于 engine 静默换装生效的窗口期：用户还没
      // 重启）。先辨形态（与 sites-platform 同一套，细节见它那边注释）：
      // · 老前端硬编码形态——index.html 自带 btn-market 与 #market-view，再注入
      //   就是重复条目（id 撞车、入口翻倍），整体让位，重启到新 engine 自动归位；
      // · 中间形态（有 #foot-apps、无插槽）——退回旧版直接注入，入口不因时序
      //   消失（2026-11-06 客户事故的症状）。重注入先清自己上次的按钮；容器
      //   认准 #foot-apps，不兜底 .foot-grid（那是硬编码按钮的家）。
      if (document.getElementById('btn-market')) { legacyHost = true; return; }
      const grid = document.getElementById('foot-apps');
      if (!grid) return;
      grid.querySelector('[data-cx-mount="' + ID + '"]')?.remove();
      btnEl = document.createElement('button');
      btnEl.id = 'btn-market';
      btnEl.dataset.cxMount = ID;
      btnEl.className = 'foot-btn';
      btnEl.dataset.order = '10';
      btnEl.title = '企业技能包 / 工作流：安装/更新 技能、组件与插件';
      btnEl.innerHTML =
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 7h14l-1.2 12a1.5 1.5 0 0 1-1.5 1.4H7.7A1.5 1.5 0 0 1 6.2 19z"/><path d="M8.5 10V6a3.5 3.5 0 0 1 7 0v4"/></svg>' +
        '<span class="foot-btn-label">企业技能包 / 工作流</span>';
      grid.appendChild(btnEl);
      [...grid.children]
        .sort((a, b) => (parseFloat(a.dataset.order || 50) - parseFloat(b.dataset.order || 50)))
        .forEach((el) => grid.appendChild(el));
    }
    // 市场视图：#main 顶栏之后（与原 index.html 同位；.hidden 全局类控制显隐）
    const main = $('main');
    const topbar = $('topbar');
    if (main && topbar) {
      viewEl = document.createElement('div');
      viewEl.id = 'market-view';
      viewEl.className = 'hidden';
      viewEl.innerHTML =
        '<div class="mk-head">' +
        '<div class="mk-title">' +
        '<h2>企业技能包 / 工作流</h2>' +
        '<span id="mk-server" class="mk-server" title="企业技能包 / 工作流 / 小程序平台地址（设置 · 网络访问 可改）"></span>' +
        '</div>' +
        '<input id="mk-search" type="search" placeholder="搜索名称 / 描述 / 标签…" autocomplete="off" spellcheck="false">' +
        '<button id="mk-refresh" class="mk-icon-btn" title="刷新清单">' +
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M21 12a9 9 0 1 1-2.64-6.36M21 3v6h-6"/></svg>' +
        '</button>' +
        '</div>' +
        '<div class="mk-tabs">' +
        '<button data-mk-tab="skill" class="mk-tab active">工作流/技能</button>' +
        '<button data-mk-tab="component" class="mk-tab">标准组件</button>' +
        '<button data-mk-tab="plugin" class="mk-tab">兼容插件</button>' +
        '<button data-mk-tab="mcp" class="mk-tab">MCP</button>' +
        '<span class="mk-tabs-rest">' +
        '<span id="mk-count"></span>' +
        '<button id="mk-upload-btn" class="mk-act hidden" title="把一个已安装的用户技能发布到市场服务器（先装后传）">＋ 上传技能</button>' +
        '<button id="mk-ext-market" class="mk-link hidden">管理外部 Claude 市场</button>' +
        '</span>' +
        '</div>' +
        '<div id="mk-list" class="mk-list"></div>';
      main.insertBefore(viewEl, topbar.nextSibling);
    }
    // 上传弹层：modal 是 fixed 全屏遮罩，挂 body 尾即可
    modalEl = document.createElement('div');
    modalEl.id = 'mk-upload-modal';
    modalEl.className = 'modal hidden';
    modalEl.innerHTML =
      '<div class="modal-card mku-card">' +
      '<h3>上传技能到市场</h3>' +
      '<p class="mku-sub">从<b>已安装的用户技能</b>里选一个发布——技能先在本机装好、试过能用，再上市场。想传新技能，先去 设置 · 技能 安装（支持拖入 zip、.md 文件或文件夹）。</p>' +
      '<div id="mku-skills" class="mku-skills"></div>' +
      '<div class="mku-meta">' +
      '<label>技能 ID <span class="mku-req">必填</span>' +
      '<input id="mku-id" placeholder="英文短名，如 wechat-chat-ops" autocomplete="off" spellcheck="false">' +
      '</label>' +
      '<label>显示名称 <span class="mku-opt">中文为主</span>' +
      '<input id="mku-name" placeholder="市场上与技能清单里显示的名字" autocomplete="off">' +
      '</label>' +
      '<label>版本' +
      '<input id="mku-version" placeholder="如 1.0.0" autocomplete="off" spellcheck="false">' +
      '</label>' +
      '<label>标签 <span class="mku-opt">逗号分隔</span>' +
      '<input id="mku-tags" placeholder="如 办公, 自动化" autocomplete="off">' +
      '</label>' +
      '<label class="mku-wide">描述' +
      '<input id="mku-desc" placeholder="一句话说明这个技能做什么（显示在市场的卡片上，上限 1024 字）" autocomplete="off">' +
      '</label>' +
      '</div>' +
      '<div id="mku-status" class="mku-status"></div>' +
      '<div class="modal-actions">' +
      '<button id="mku-clear">清空</button>' +
      '<span class="spacer"></span>' +
      '<button id="mku-cancel">取消</button>' +
      '<button id="mku-submit" class="accent" disabled>发布到市场</button>' +
      '</div>' +
      '</div>';
    document.body.appendChild(modalEl);
  }

  /* ---------- 视图开关（body.market-mode 占领主区；与小程序平台互斥） ---------- */

  function marketOpen() {
    return document.body.classList.contains('market-mode');
  }

  function setMarketView(on) {
    // 市场与小程序平台都是主区整页，互斥（sites-platform 组件可能不在场，先探）
    if (on && typeof sitesViewOpen === 'function' && sitesViewOpen() && typeof closeSites === 'function') closeSites();
    document.body.classList.toggle('market-mode', on);
    if (viewEl) viewEl.classList.toggle('hidden', !on); // .hidden 是全局 display:none!important，必须跟着切
    if (btnEl) btnEl.classList.toggle('active', on);
    if (!on) mkuClose(); // 离开市场视图时收起上传技能弹层（上传中不关）
    if (on) {
      // 进市场页就绕过 60s 缓存拉一次最新清单（管理页下架后要尽快消失），
      // 再每 60 秒静默复查一次——市场页开着很久也不会一直显示旧条目
      loadMarket(true);
      startMarketAuto();
    } else {
      stopMarketAuto();
    }
  }

  /* 市场页开着时的自动复查（60 秒，静默：不闪「正在刷新」、不清已渲染的列表） */
  function startMarketAuto() {
    stopMarketAuto();
    mkAutoTimer = setInterval(() => {
      if (!marketOpen() || document.hidden) return;
      loadMarket(true, true);
    }, 60 * 1000);
  }
  function stopMarketAuto() {
    if (mkAutoTimer) { clearInterval(mkAutoTimer); mkAutoTimer = 0; }
  }

  function toggleMarket() {
    if (marketOpen()) closeMarket();
    else openMarket();
  }

  function openMarket() {
    if (typeof narrowClosePanels === 'function') narrowClosePanels(); // 窄屏：从侧栏浮层点进市场页即收起浮层（与进设置/选中会话同一套）
    setMarketView(true);
    const search = $('mk-search');
    if (search) setTimeout(() => search.focus(), 60);
  }

  function closeMarket() { setMarketView(false); }

  /* ---------- 数据加载与渲染 ---------- */

  async function loadMarket(force, silent) {
    const list = $('mk-list');
    if (!list) return;
    if (mkLoading) return;
    mkLoading = true;
    if (!silent) {
      if (force && mkData) list.innerHTML = '<div class="mk-hint">正在刷新清单…</div>';
      else if (!mkData) list.innerHTML = '<div class="mk-hint">正在加载…</div>';
    }
    try {
      mkData = await api('/api/market' + (force ? '?refresh=1' : ''));
    } catch (e) {
      if (silent) { mkLoading = false; return; } // 静默复查失败：保留现有列表，下一轮再试
      mkData = null;
      mkLoading = false;
      renderMarketState(esc('市场读取失败：' + e.message));
      return;
    }
    mkLoading = false;
    renderMarket();
  }

  // renderMarketState 收 HTML 片段（调用方负责转义服务端/异常文本）
  function renderMarketState(html) {
    const list = $('mk-list');
    if (list) list.innerHTML = '<div class="mk-hint">' + html + '</div>';
    const count = $('mk-count');
    if (count) count.textContent = '';
  }

  function renderMarket() {
    const ubtn = $('mk-upload-btn'); // 「＋ 上传技能」只在技能 tab 出现
    if (ubtn) ubtn.classList.toggle('hidden', mkTab !== 'skill');
    if (!mkData) return renderMarketState('清单为空');
    const server = $('mk-server');
    if (server) {
      server.textContent = mkData.configured ? (mkData.name || mkData.url || '已配置') : '未配置';
      server.classList.toggle('off', !mkData.configured);
    }
    const ext = $('mk-ext-market');
    if (ext) ext.classList.toggle('hidden', mkTab !== 'plugin');
    if (!mkData.configured) { renderUnconfigured(); return; }
    if (mkData.error) {
      renderMarketState(esc(mkData.error) + '（<span class="mk-link" id="mk-retry">重试</span> 或到 设置 · 网络访问 检查市场地址）');
      const retry = $('mk-retry');
      if (retry) retry.onclick = () => loadMarket(true);
      return;
    }
    renderTab();
  }

  function renderUnconfigured() {
    const list = $('mk-list');
    if (!list) return;
    const sug = mkData.suggested || '';
    list.innerHTML =
      '<div class="mk-hint">还没配置企业技能包 / 工作流 / 小程序平台地址。两者由同一台 jxai-server 提供（部署一次，技能/组件/插件装这一处）。' +
      (sug
        ? '<br>检测到远程接入网关，推荐直接用同一台服务器：' +
        '<div class="mk-suggest"><code>' + esc(sug) + '</code></div>' +
        '<div class="mk-suggest-acts"><button id="mk-use-suggested" class="mk-act">填入此地址</button>' +
        '<button id="mk-go-settings" class="mk-act ghost">手动设置</button></div>'
        : '<br>到 设置 · 网络访问 · 企业技能包 / 工作流 的服务器地址里填（如 https://jxaizz.com:2227）。' +
        '<div class="mk-suggest-acts"><button id="mk-go-settings" class="mk-act">去设置</button></div>') +
      '</div>';
    const use = $('mk-use-suggested');
    if (use) use.onclick = async () => {
      use.disabled = true; use.textContent = '保存中…';
      try {
        await api('/api/config', { method: 'POST', body: JSON.stringify({ marketURL: sug }) });
        mkData = null;
        await loadMarket(true);
      } catch (e) { alert('保存失败：' + e.message); use.disabled = false; use.textContent = '填入此地址'; }
    };
    const go = $('mk-go-settings');
    if (go) go.onclick = () => { if (typeof openSettings === 'function') openSettings('network'); };
  }

  function entriesOfTab(tab) {
    if (!mkData) return [];
    if (tab === 'skill') return mkData.skills || [];
    if (tab === 'component') return mkData.components || [];
    if (tab === 'mcp') return mkData.mcps || [];
    return mkData.plugins || [];
  }

  function renderTab() {
    const list = $('mk-list');
    if (!list) return;
    const q = ($('mk-search').value || '').trim().toLowerCase();
    const all = entriesOfTab(mkTab);
    const items = q
      ? all.filter((e) => {
        const hay = ((e.name || '') + ' ' + (e.id || '') + ' ' + (e.description || '') + ' ' + (e.tags || []).join(' ')).toLowerCase();
        return hay.includes(q);
      })
    : all;
    const count = $('mk-count');
    if (count) count.textContent = q ? (all.length ? `匹配 ${items.length}/${all.length} 项` : '') : (all.length ? `共 ${all.length} 项` : '');
    if (!all.length) {
      list.innerHTML = '<div class="mk-hint">' + ({
        skill: '市场里还没有技能。让服务器管理员用 jxai-server publish 发布（或检查市场地址）。',
        component: '市场里还没有组件。',
        plugin: '市场里还没有插件。也可以添加外部 Claude Code 插件市场（右上角入口）。',
        mcp: '市场里还没有 MCP 连接器。管理员在服务端管理页（/admin）或 CLI 发布 mcp.json 即可（也支持直接上传 .json 配置文件）。',
      })[mkTab] + '</div>';
      return;
    }
    if (!items.length) {
      list.innerHTML = '<div class="mk-hint">没有匹配「' + esc(q) + '」的条目</div>';
      return;
    }
    list.innerHTML = '';
    for (const e of items) list.appendChild(mkCard(e));
  }

  function mkCard(e) {
    const card = document.createElement('div');
    card.className = 'mk-card' + (e.installed ? ' installed' : '');
    const tags = (e.tags || []).slice(0, 4)
      .map((t) => `<span class="mk-tag">${esc(t)}</span>`).join('');
    card.innerHTML =
      `<div class="mk-card-head"><b title="${esc(e.id)}">${esc(e.name || e.id)}</b>` +
      `<span class="mk-ver" title="${esc(e.id)}">v${esc(e.version || '?')}</span></div>` +
      `<p class="mk-desc">${esc(e.description || '')}</p>` +
      (tags ? `<div class="mk-tags">${tags}</div>` : '') +
      `<div class="mk-card-foot"></div>`;
    const foot = card.querySelector('.mk-card-foot');

    const typeLabel = { skill: '技能', component: '组件', plugin: '插件', mcp: 'MCP 连接器' }[mkTab];
    if (e.builtin) {
      // 内置组件（随包分发）：市场不给安装/卸载——它随程序静默更新升级，
      // 要停用去 设置 → 组件 关开关。这里只提示归属，避免用户点到报错。
      const st = document.createElement('span');
      st.className = 'mk-installed';
      st.textContent = '内置（随程序更新）';
      foot.appendChild(st);
    } else if (e.installed) {
      const st = document.createElement('span');
      st.className = 'mk-installed';
      st.textContent = e.update ? `已装 v${e.installedVersion || '?'}（有新版）` : `已安装 v${e.installedVersion || e.version}`;
      foot.appendChild(st);
      if (e.update) foot.appendChild(mkAction('更新', e, 'install'));
      foot.appendChild(mkAction('卸载', e, 'uninstall'));
    } else {
      foot.appendChild(mkAction(`安装${typeLabel === '插件' ? '' : typeLabel}`, e, 'install'));
    }
    // 发布者管理（只能管自己发的）：条目 owner 与本机「发布者姓名」一致时，
    // 多出「更新发布 / 删除」——更新=同 id 发新版本，删除=从市场下架。
    const me = (mkData && mkData.me && mkData.me.name) || '';
    if (me && e.owner && e.owner === me) {
      foot.appendChild(mkPublishAction('更新发布', e, 'republish'));
      foot.appendChild(mkPublishAction('删除', e, 'unpublish'));
    }
    return card;
  }

  // mkAction 一张卡上的操作按钮（安装/更新/卸载），动作完成后整页重拉 + 组件热刷新。
  function mkAction(label, e, action) {
    const btn = document.createElement('button');
    btn.className = 'mk-act' + (action === 'uninstall' ? ' ghost' : '');
    btn.textContent = label;
    btn.onclick = async () => {
      if (action === 'uninstall') {
        const ok = await confirmCenter(`卸载${e.installed ? '「' + (e.name || e.id) + '」' : ''}？本地文件与设置将一并删除。`);
        if (!ok) return;
      }
      btn.disabled = true;
      btn.textContent = action === 'install' ? '安装中…' : '卸载中…';
      try {
        await api('/api/market/' + action, {
          method: 'POST',
          body: JSON.stringify({ type: mkTab, id: e.id }),
        });
        if (mkTab === 'component' && window.CX && CX.refresh) {
          try { await CX.refresh(); } catch (err) { console.warn('[市场] 组件热刷新失败', err); }
        }
        mkData = null;           // 让 loadMarket 走真拉取（后端清单缓存也会被绕开与否不影响正确性）
        await loadMarket(true);
      } catch (err) {
        alert((action === 'install' ? '安装失败：' : '卸载失败：') + err.message);
        btn.disabled = false;
        btn.textContent = label;
      }
    };
    return btn;
  }

  // mkPublishAction 卡片上的「更新发布 / 删除」（只对自己发布的条目渲染）：
  // 更新=同 id 发新版本（复用上传弹层、预填该条目元信息；包体取自本机已装技能）；
  // 删除=下架（进服务端回收站，管理员可恢复；归属校验在服务端做）。
  function mkPublishAction(label, e, action) {
    const btn = document.createElement('button');
    btn.className = 'mk-act' + (action === 'unpublish' ? ' ghost' : '');
    btn.textContent = label;
    btn.onclick = async () => {
      if (action === 'republish') { mkuOpenForEdit(e); return; }
      const ok = await confirmCenter('从市场下架「' + (e.name || e.id) + '」v' + (e.version || '?') +
        '？条目会进服务端回收站（管理员可恢复）；其他同事将无法再安装，已安装的不受影响。');
      if (!ok) return;
      btn.disabled = true;
      btn.textContent = '下架中…';
      try {
        await api('/api/market/unpublish', { method: 'POST', body: JSON.stringify({ type: mkTab, id: e.id, version: e.version }) });
        mkData = null;
        await loadMarket(true);
      } catch (err) {
        alert('下架失败：' + err.message);
        btn.disabled = false;
        btn.textContent = label;
      }
    };
    return btn;
  }

  /* ---------- 绑定（init 里一次） ---------- */

  function bindMarket() {
    if (btnEl) btnEl.onclick = toggleMarket;
    const search = $('mk-search');
    if (search) {
      search.oninput = () => { if (mkData && mkData.configured && !mkData.error) renderTab(); };
    }
    const refresh = $('mk-refresh');
    if (refresh) refresh.onclick = () => loadMarket(true);
    document.querySelectorAll('.mk-tab').forEach((t) => {
      t.onclick = () => {
        document.querySelectorAll('.mk-tab').forEach((x) => x.classList.toggle('active', x === t));
        mkTab = t.dataset.mkTab || 'skill';
        renderMarket();
      };
    });
    const ext = $('mk-ext-market');
    if (ext) ext.onclick = () => { if (typeof openSettings === 'function') openSettings('plugins'); };
    const uploadBtn = $('mk-upload-btn');
    if (uploadBtn) uploadBtn.onclick = mkuShow;
    mkuBind(); // 上传技能弹层（拖拽 / 选取 / Ctrl+V）
    // Esc 关闭市场视图（有任何弹窗开着时不抢键：确认框/设置自己处理 Esc；
    // 小程序平台的 Esc 由它自己的组件管）
    addDocKey((ev) => {
      if (ev.key !== 'Escape' || !marketOpen()) return;
      const anyModalOpen = [...document.querySelectorAll('.modal')].some((m) => !m.classList.contains('hidden'));
      if (anyModalOpen) return;
      closeMarket();
    });
  }

  /* ---------- 上传技能到市场（市场 · 技能 tab 的「＋ 上传技能」） ----------
     先装后传：从已装的用户技能（GET /api/skills）里选一个 → 后端从安装目录
     现场打包、走发布校验链（SKILL.md 内容体检等）→ 转发市场服务器。身份=
     本机发布者姓名（X-Jxai-User 头），归属记录与「只能动自己发的」判权都在
     服务端；下架是进服务端回收站，不是物理删除。

     元信息预填取自 GET /api/market/publish-meta?skill=<id>（后端单一事实来源：
     五字段回退链与中文→英文 ID 词表都只在 Go 一份，前端不再持有词表副本）。
     提交契约：POST /api/market/publish JSON {skillId, id, name, version,
     description, tags}，成功回 {ok:1, id, version, sha256, size}；失败非 2xx
     + {error}。 */

  let mkuSkills = [];       // 已装用户技能清单（GET /api/skills 的 user）
  let mkuSel = '';          // 选中的技能 id
  let mkuSuggested = false; // 技能 ID 是后端词表建议的（不是包内自带或用户手填的）
  let mkuBusy = false;      // 发布在途
  let mkuErr = '';          // 提交失败的提示（优先于「就绪」）
  let mkuEditOf = null;     // 「更新发布」时正在编辑的市场条目（{id,version}）；普通上传为 null

  function mkuOpen() {
    const m = $('mk-upload-modal');
    return !!m && !m.classList.contains('hidden');
  }

  async function mkuShow() {
    if (mkuOpen()) return;
    mkuReset();
    $('mk-upload-modal').classList.remove('hidden');
    await mkuLoadSkills();
  }

  function mkuClose() {
    if (!mkuOpen() || mkuBusy) return; // 发布中不关
    $('mk-upload-modal').classList.add('hidden');
  }

  // mkuOpenForEdit 「更新发布」入口：打开弹层并预填该市场条目的元信息（ID /
  // 显示名 / 描述 / 标签），**版本留空**——更新就是发一个新版本（同 id、新
  // version）。仍需选中本机已装的技能作为包体来源；没装就装一份再回来。
  async function mkuOpenForEdit(e) {
    await mkuShow();
    mkuEditOf = { id: e.id, version: e.version };
    $('mku-id').value = e.id || '';
    $('mku-name').value = e.name || '';
    $('mku-desc').value = e.description || '';
    $('mku-tags').value = (e.tags || []).join(', ');
    $('mku-version').value = '';
    mkuRender();
  }

  function mkuClearForm() {
    mkuSel = '';
    mkuSuggested = false;
    mkuErr = '';
    mkuEditOf = null;
    ['mku-id', 'mku-name', 'mku-version', 'mku-desc', 'mku-tags'].forEach((id) => { const el = $(id); if (el) el.value = ''; });
    mkuRenderSkills();
    mkuRender();
  }

  function mkuReset() {
    mkuSkills = [];
    mkuClearForm();
  }

  function mkuSetStatus(msg, kind) {
    const el = $('mku-status');
    if (!el) return;
    el.className = 'mku-status' + (kind ? ' ' + kind : '');
    el.textContent = msg || '';
  }

  function mkuSize(n) {
    if (!n) return '0 B';
    if (n < 1024) return n + ' B';
    if (n < 1048576) return (n / 1024).toFixed(1) + ' KB';
    return (n / 1048576).toFixed(1) + ' MB';
  }

  /* ---- 已装技能清单与选择 ---- */

  async function mkuLoadSkills() {
    const box = $('mku-skills');
    if (!box) return;
    box.innerHTML = '<div class="mku-skill-empty">正在读取已装技能…</div>';
    try {
      const d = await api('/api/skills');
      mkuSkills = d.user || [];
    } catch (e) {
      box.innerHTML = '<div class="mku-skill-empty">已装技能读取失败：' + esc(e.message) + '</div>';
      return;
    }
    mkuRenderSkills();
  }

  function mkuRenderSkills() {
    const box = $('mku-skills');
    if (!box) return;
    if (!mkuSkills.length) {
      box.innerHTML = '<div class="mku-skill-empty">本机还没有安装用户技能。<br>' +
        '先去 设置 · 技能 安装（支持拖入 zip、.md 文件或技能文件夹），装好、试过能用再回来发布。</div>';
      return;
    }
    box.innerHTML = '';
    for (const s of mkuSkills) {
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'mku-skill' + (s.id === mkuSel ? ' sel' : '');
      const disp = s.displayName || s.name || s.id;
      btn.innerHTML =
        '<div class="mku-skill-line"><b>' + esc(disp) + '</b>' +
        (s.version ? '<span class="mku-skill-id">v' + esc(s.version) + '</span>' : '') +
        '<span class="mku-skill-id">' + esc(s.id) + '</span></div>' +
        (s.description ? '<div class="mku-skill-desc">' + esc(s.description) + '</div>' : '');
      btn.onclick = () => mkuPick(s.id);
      box.appendChild(btn);
    }
  }

  // mkuPick 选中一个已装技能：拉后端推导的元信息预填表单（只填空字段——更新
  // 发布时条目既有元信息不被新包内容覆盖）。
  async function mkuPick(id) {
    if (mkuBusy) return;
    mkuSel = id;
    mkuErr = '';
    mkuSuggested = false;
    mkuRenderSkills();
    mkuSetStatus('正在推导元信息…');
    try {
      const meta = await api('/api/market/publish-meta?skill=' + encodeURIComponent(id));
      mkuSuggested = !!meta.idSuggested;
      const fill = (elId, v) => { const el = $(elId); if (el && v && !el.value) el.value = v; };
      fill('mku-id', meta.id);
      fill('mku-name', meta.name);
      fill('mku-version', meta.version);
      fill('mku-desc', meta.description);
      fill('mku-tags', (meta.tags || []).join(', '));
    } catch (e) {
      mkuErr = '元信息推导失败：' + e.message + '（仍可手填全部字段提交；后端会再校验一遍）';
    }
    mkuRender();
  }

  /* ---- 渲染与提交闸门 ---- */

  function mkuRender() {
    const id = ($('mku-id').value || '').trim();
    const idOk = /^[a-z0-9][a-z0-9-]*$/.test(id); // 市场端只收纯小写字母/数字/中划线
    const name = ($('mku-name').value || '').trim();
    const desc = ($('mku-desc').value || '').trim();
    const ver = ($('mku-version').value || '').trim();
    const meName = (mkData && mkData.me && mkData.me.name) || '';
    let msg = '';
    let kind = '';
    if (!mkuSel) {
      msg = '先在上面选一个已安装的用户技能。';
    } else if (!id) {
      msg = '技能 ID 还没推导出来：请手填英文短名（如 wechat-chat-ops）。';
      kind = 'warn';
    } else if (!idOk) {
      msg = '技能 ID 只能用纯小写字母/数字/中划线（市场端规则）：像 wechat-chat-ops 这样，不能带中文或空格。';
      kind = 'warn';
    } else if ([...name].length > 64) {
      msg = '显示名称超过 64 字（行业规范上限），请缩短。';
      kind = 'warn';
    } else if ([...desc].length > 1024) {
      msg = '描述超过 1024 字（行业规范上限）：请精简，详细内容写进 SKILL.md 正文。';
      kind = 'warn';
    } else {
      msg = '将发布 ' + id + (ver ? ' v' + ver : '') + '：包体取自本机已装的「' + esc(mkuSel) + '」。';
      kind = 'ok';
      if (mkuSuggested) msg += '（ID 是按中文词表建议的，可改）';
      if (!($('mku-desc').value || '').trim()) msg += '（没写描述：市场卡片上会没有说明文字，建议补一行）';
      if (!meName) msg += '（未设置发布者姓名：市场服务器记不了归属，发布会失败——去 设置 · 网络访问 · 企业技能包 / 工作流 填一个）';
    }
    if (mkuBusy) { msg = '正在发布…'; kind = ''; }
    else if (mkuErr) { msg = mkuErr; kind = 'err'; } // 提交/推导失败的提示盖过「就绪」
    else if (mkuEditOf) { msg = '【更新发布】' + mkuEditOf.id + ' v' + (mkuEditOf.version || '?') + ' → ' + msg; }
    mkuSetStatus(msg, kind);
    const sub = $('mku-submit');
    if (sub) sub.disabled = mkuBusy || !mkuSel || !id || !idOk;
  }

  /* ---- 绑定（bindMarket 里调一次） ---- */

  function mkuBind() {
    const modal = $('mk-upload-modal');
    if (!modal) return;
    ['mku-id', 'mku-name', 'mku-version', 'mku-desc', 'mku-tags'].forEach((id) => {
      const el = $(id);
      if (el) el.oninput = () => mkuRender();
    });
    $('mku-clear').onclick = mkuClearForm;
    $('mku-cancel').onclick = () => mkuClose();
    $('mku-submit').onclick = mkuSubmit;
    modal.addEventListener('click', (e) => { if (e.target === modal) mkuClose(); }); // 点遮罩=取消
    addDocKey((e) => {
      if (e.key !== 'Escape' || !mkuOpen()) return;
      const conf = $('confirm-modal');
      if (conf && !conf.classList.contains('hidden')) return; // 确认框在上层，先关它
      e.stopPropagation();
      mkuClose();
    });
  }

  /* ---- 提交：JSON → POST /api/market/publish（包由后端从安装目录打） ---- */

  async function mkuSubmit() {
    if (mkuBusy || !mkuSel) return;
    const id = ($('mku-id').value || '').trim();
    if (!id) { mkuSetStatus('请填技能 ID（英文短名，市场里唯一）', 'warn'); return; }
    const body = {
      skillId: mkuSel,
      id,
      name: ($('mku-name').value || '').trim() || id,
      version: ($('mku-version').value || '').trim(),
      description: ($('mku-desc').value || '').trim(),
      tags: ($('mku-tags').value || '').split(/[,，]/).map((s) => s.trim()).filter(Boolean),
    };
    mkuErr = '';
    mkuBusy = true;
    const btn = $('mku-submit');
    btn.disabled = true;
    btn.textContent = '发布中…';
    mkuRender();
    try {
      const r = await api('/api/market/publish', { method: 'POST', body: JSON.stringify(body) });
      mkuBusy = false;
      btn.textContent = '发布到市场';
      alert('已发布到市场：' + (r.id || id) + (r.version ? ' v' + r.version : '') +
        (r.size ? '（' + mkuSize(r.size) + '，sha256 ' + String(r.sha256 || '').slice(0, 12) + '…）' : ''));
      $('mk-upload-modal').classList.add('hidden');
      mkData = null;
      await loadMarket(true);
    } catch (e) {
      mkuBusy = false;
      btn.textContent = '发布到市场';
      mkuErr = '发布失败：' + e.message;
      mkuRender();
    }
  }

  /* ---------- 列数：最大化 4 列 / 窗口态 3 列（跨屏幕自适应，不依赖固定像素阈值） ---------- */

  // 判定「窗口是否最大化」：最大化后窗口宽度会等于本屏可用宽（screen.availWidth）。
  // 注意尺度：innerWidth/outerWidth 恒为 CSS 像素，而 screen.* 在部分宿主里报物理像素
  // （本机 1536×864@125%：availWidth=1536 物理 vs 1229 CSS）——故同时按「原值」与
  // 「除以 devicePixelRatio」两种尺度严格比对（容差 4px / 10px），任一命中即算最大化。
  // 不用固定像素阈值（如 ≥1600px）：这类屏幕最大化也只有 ≈1229px 视口，阈值必然失准。
  function updateMarketCols() {
    const iw = window.innerWidth;
    const ow = window.outerWidth || 0;
    const aw = window.screen.availWidth || 0;
    const dpr = window.devicePixelRatio || 1;
    const near = (a, b, tol) => b > 0 && Math.abs(a - b) <= tol;
    const maxed = near(iw, aw, 4) || near(ow, aw, 10) ||
      (dpr > 1 && (near(iw, aw / dpr, 4) || near(ow, aw / dpr, 10)));
    document.body.classList.toggle('mk-4col', iw >= 900 && maxed);
  }
  function onResize() { updateMarketCols(); }

  /* ---------- 设置区（设置 · 网络访问页的 #net-market-cfg 挂载段） ----------
     组件开关归「组件」页，参数归网络访问页（与其他网络配置放一起）。
     init 填充、dispose 清空并隐藏；回显 + 改动即存（单字段 PATCH /api/config，
     失败回滚为服务端值——与设置页 bindRemoteInput 同一套契约）。 */

  async function renderNetCfg() {
    const host = document.getElementById('net-market-cfg');
    if (!host) return;
    host.classList.remove('hidden');
    host.innerHTML =
      '<h4>企业技能包 / 工作流</h4>' +
      '<div class="set-field">' +
      '<div class="sf-label">企业技能包 / 工作流 / 小程序平台地址</div>' +
      '<input id="set-market-url" class="pf-input" placeholder="https://jxaizz.com:2227" autocomplete="off" spellcheck="false">' +
      '<p class="sf-desc">企业技能市场与小程序平台同一台服务端：填服务器地址即可（https://域名或IP:端口），清单与 /sites 等路径后缀由程序自动拼；改动即时生效，留空则市场页显示未配置。</p>' +
      '</div>' +
      '<div class="set-field">' +
      '<div class="sf-label">发布者姓名</div>' +
      '<input id="set-publisher-name" class="pf-input" placeholder="如 张三" autocomplete="off" spellcheck="false">' +
      '<p class="sf-desc">发布技能时随包上报的市场条目归属名：市场卡片上只有「发布者 = 这个姓名」的条目才显示「更新发布 / 删除」（即只能管自己发的）。首次接入向导中填的姓名会自动写入这里；留空则不带归属，发布后自己也删不了。</p>' +
      '</div>';
    let c = null;
    try { c = await api('/api/config'); } catch (e) { /* 配置拉取失败：字段留着空着可填，保存时再报 */ }
    const urlEl = host.querySelector('#set-market-url');
    const pubEl = host.querySelector('#set-publisher-name');
    if (c) {
      if (urlEl) urlEl.value = c.marketURL || '';
      if (pubEl) pubEl.value = c.publisherName || '';
    }
    const patch = async (el, key) => {
      const v = el.value.trim();
      try {
        const back = await api('/api/config', { method: 'POST', body: JSON.stringify({ [key]: v }) });
        if (document.activeElement !== el) el.value = back[key] || '';
      } catch (e) {
        alert('保存失败：' + e.message);
        const cur = await api('/api/config').catch(() => null);
        if (cur && document.activeElement !== el) el.value = cur[key] || '';
      }
    };
    if (urlEl) urlEl.onchange = () => patch(urlEl, 'marketURL');
    if (pubEl) pubEl.onchange = () => patch(pubEl, 'publisherName');
  }

  function clearNetCfg() {
    const host = document.getElementById('net-market-cfg');
    if (host) { host.innerHTML = ''; host.classList.add('hidden'); }
  }

  /* ---------- 注册（init 建立一切，dispose 完整撤销） ---------- */

  function init() {
    injectDOM();
    if (legacyHost) return; // 让位形态：老前端的按钮/视图/window 接口全在场，本组件不注入不覆盖
    bindMarket();
    updateMarketCols();
    window.addEventListener('resize', onResize);
    // 主程序引用点（app-chat.js 回首页/开对话收起市场页、app-hotkeys.js 让位
    // 判断）经 window 接口调用，摘除组件后这些引用自动退化为「不存在」
    window.marketOpen = marketOpen;
    window.closeMarket = closeMarket;
    renderNetCfg(); // 设置 · 网络访问页的配置段
  }

  function dispose() {
    if (legacyHost) { legacyHost = false; return; } // 让位形态什么都没装，无可撤
    if (marketOpen()) closeMarket(); // 市场正开着：先还回对话区
    stopMarketAuto();
    window.removeEventListener('resize', onResize);
    docKeys.forEach((fn) => document.removeEventListener('keydown', fn));
    docKeys.length = 0;
    document.body.classList.remove('mk-4col'); // 挂在 body 上的全局类一并撤（组件不留痕）
    if (btnEntry) { btnEntry.remove(); btnEntry = null; btnEl = null; }
    else if (btnEl) { btnEl.remove(); btnEl = null; } // 旧 engine 的 DOM 注入 fallback
    if (viewEl) { viewEl.remove(); viewEl = null; }
    if (modalEl) { modalEl.remove(); modalEl = null; }
    delete window.marketOpen;
    delete window.closeMarket;
    clearNetCfg();
    mkData = null;
    mkLoading = false;
    mkuBusy = false;
    mkuEditOf = null;
  }

  CX.register({ id: ID, init, dispose });
})();
