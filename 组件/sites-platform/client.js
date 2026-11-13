/* 组件：小程序平台（jxai-server 的 /sites 网页托管平台）—— 浏览器半边。
 *
 * 原本体 web/app-market.js 的 sites* 系列与 index.html 里的两块硬编码
 * （侧栏按钮 / #sites-view 视图）整体迁入本组件：后端透明反向代理
 * （sitesproxy.go 的 /sites、/sites-assets/、/api/sites/*、/s/）保留在
 * 主程序，入口与页面由组件掌管——不需要的人关掉开关或摘除
 * 组件/sites-platform/ 目录，侧栏入口、平台页、设置区一并消失，主程序
 * 其余功能不受影响。
 *
 * 不套壳：顶栏以下整页就是平台管理页（iframe 常驻不销毁，来回切不打扰
 * 已打开的页面）。本机与局域网形态 src 用相对路径 /sites；远程外壳形态
 * （window.JX_REMOTE）下改由外壳代载（remote-boot.js 的 loadSitesFrame）。
 * dispose 撤掉按钮、视图、监听与 window 接口——组件可热关闭。 */
(function () {
  if (!window.CX) return;

  const ID = 'sites-platform';

  let btnEl = null, btnEntry = null, viewEl = null; // 本组件注入的节点（btnEntry＝插槽条目句柄）
  let legacyHost = false;    // 老前端硬编码形态（组件化之前的产品）：按钮/视图是 index.html 硬编码的，本组件整体让位
  const docKeys = [];                // document keydown 监听（dispose 逐个解绑）

  const $ = (id) => document.getElementById(id);
  const addDocKey = (fn) => { document.addEventListener('keydown', fn); docKeys.push(fn); };

  /* ---------- DOM 注入（HTML 原样来自原 index.html） ---------- */

  function injectDOM() {
    // 侧栏入口：向底部插槽申请一个条目（order 3，排在回收站 1、企业技能包 2 之上）。
    // 骨架、排序、电脑竖排 / 手机三列网格全归插槽——见 app-core.js 的 JX.slot 与
    // AGENT.md「界面插槽」。标签交给插槽包成 .foot-btn-label（裸文本节点拿不到
    // 手机端"最多两行、超出裁断"的规则，会被挤成竖排）
    btnEntry = (window.JX && JX.slot) ? JX.slot('sidebar.foot').add({
      key: 'sites-platform', id: 'btn-sites', order: 3, owner: '组件 sites-platform',
      title: '企业小程序平台：发布与管理小程序、数据文件、二维码',
      label: '小程序平台',
      icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="14" rx="2"/><path d="M3 9h18"/><path d="M8 4V2M16 4V2"/><path d="M7 13h4M7 16h7"/></svg>',
    }) : null;
    if (btnEntry) {
      btnEl = btnEntry.el;
    } else {
      // 旧 engine 没有插槽（组件块先于 engine 静默换装生效的窗口期）。先辨形态：
      // · 老前端硬编码形态（组件化之前的产品）——index.html 里自带 btn-sites 与
      //   #sites-view，老前端逻辑完整在场。再注入任何东西都是重复（按钮/视图
      //   id 全撞车、入口翻倍，2026-11-06 窗口期底部入口串成"乱码"就是重复
      //   条目在窄格里被挤成竖排的字）——整体让位，重启到新 engine 自动归位。
      if (document.getElementById('btn-sites')) { legacyHost = true; return; }
      // · 中间形态（有 #foot-apps 挂载点、还没插槽）：退回旧版直接注入，入口
      //   不因时序消失（2026-11-06 客户事故的症状）。重注入先清自己上次的
      //   按钮（热重载/静默换装重放同一脚本不落双份）；容器认准 #foot-apps，
      //   不再兜底 .foot-grid——那会命中硬编码按钮的容器（让位形态之外也可能
      //   存在更老的骨架），把按钮塞进别人家的格子里。
      const grid = document.getElementById('foot-apps');
      if (!grid) return;
      grid.querySelector('[data-cx-mount="' + ID + '"]')?.remove();
      btnEl = document.createElement('button');
      btnEl.id = 'btn-sites';
      btnEl.dataset.cxMount = ID;
      btnEl.className = 'foot-btn';
      btnEl.dataset.order = '20';
      btnEl.title = '企业小程序平台：发布与管理小程序、数据文件、二维码';
      btnEl.innerHTML =
        '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect x="3" y="4" width="18" height="14" rx="2"/><path d="M3 9h18"/><path d="M8 4V2M16 4V2"/><path d="M7 13h4M7 16h7"/></svg>' +
        '<span class="foot-btn-label">小程序平台</span>';
      grid.appendChild(btnEl);
      [...grid.children]
        .sort((a, b) => (parseFloat(a.dataset.order || 50) - parseFloat(b.dataset.order || 50)))
        .forEach((el) => grid.appendChild(el));
    }
    // 平台视图：#main 顶栏之后（与市场视图平级的主区整页；.hidden 全局类控制显隐）
    const main = $('main');
    const topbar = $('topbar');
    if (main && topbar) {
      viewEl = document.createElement('div');
      viewEl.id = 'sites-view';
      viewEl.className = 'hidden';
      viewEl.innerHTML = '<iframe id="sites-frame" class="sites-frame" title="企业小程序平台"></iframe>';
      main.insertBefore(viewEl, topbar.nextSibling);
    }
  }

  /* ---------- 视图开关（body.sites-mode 占领主区；与市场互斥） ---------- */

  // renderSitesFrame：进小程序平台页时确保 iframe 已加载。
  function renderSitesFrame() {
    const frame = $('sites-frame');
    if (!frame) return;
    if (window.JX_REMOTE && typeof window.JX_REMOTE.loadSitesFrame === 'function') {
      if (frame.dataset.jxRemoteLoaded !== '1') {
        frame.dataset.jxRemoteLoaded = '1';
        window.JX_REMOTE.loadSitesFrame(frame);
      }
      return;
    }
    if (!frame.src || frame.src === 'about:blank') frame.src = '/sites';
  }

  function sitesViewOpen() {
    return document.body.classList.contains('sites-mode');
  }

  function setSitesView(on) {
    // 市场与平台都是主区整页，互斥（marketOpen 由 skill-market 组件挂 window，
    // 组件关闭/摘除后不存在——先探再关）
    if (on && typeof marketOpen === 'function' && marketOpen() && typeof closeMarket === 'function') closeMarket();
    document.body.classList.toggle('sites-mode', on);
    if (viewEl) viewEl.classList.toggle('hidden', !on); // .hidden 是全局 display:none!important，必须跟着切
    if (btnEl) btnEl.classList.toggle('active', on);
    if (on) renderSitesFrame();
  }

  function openSites() {
    if (typeof narrowClosePanels === 'function') narrowClosePanels(); // 窄屏：从侧栏浮层点进平台页即收起浮层（与进市场/设置同一套）
    setSitesView(true);
  }

  function closeSites() { setSitesView(false); }

  // openSitesPlatform 侧栏「小程序平台」入口：代理目标从市场地址推导，没配置时
  // 引导去设置（代理本身也会回 503 提示）。
  async function openSitesPlatform() {
    let cfg = null;
    try { cfg = await api('/api/market'); } catch (e) {}
    if (!(cfg && cfg.configured)) {
      alert('还没配置企业技能包 / 工作流 / 小程序平台地址：请到 设置 · 网络访问 填服务器地址（如 https://jxaizz.com:2227），技能市场与小程序平台同一台服务端。');
      if (typeof openSettings === 'function') openSettings('network');
      return;
    }
    openSites();
  }

  /* ---------- 设置区（设置 · 网络访问页的 #net-sites-cfg 挂载段） ----------
     极简：一个「打开托管平台」入口。与市场组件共用同一台服务器地址（config
     的 marketURL）：skill-market 在场时地址归它的配置段（同在网络访问页上方），
     这里只留入口；它不在场（关闭/摘除）时这里顶上地址字段，平台仍可独立配置
     使用。市场组件热开关时经 cx:change 联动重挂。组件开关归「组件」页。 */

  async function renderNetCfg() {
    const host = document.getElementById('net-sites-cfg');
    if (!host) return;
    host.classList.remove('hidden');
    const marketLive = !!(window.CX && CX.isActive('skill-market'));
    let urlHtml = '';
    if (!marketLive) {
      urlHtml =
        '<div class="set-field">' +
        '<div class="sf-label">企业技能包 / 工作流 / 小程序平台地址</div>' +
        '<input id="set-market-url" class="pf-input" placeholder="https://jxaizz.com:2227" autocomplete="off" spellcheck="false">' +
        '<p class="sf-desc">小程序平台（与技能市场同一台服务端）：填服务器地址即可（https://域名或IP:端口），/sites 等路径后缀由程序自动拼；改动即时生效。</p>' +
        '</div>';
    }
    host.innerHTML =
      '<h4>小程序平台</h4>' +
      urlHtml +
      '<div class="set-field">' +
      '<div class="sf-label">网页托管平台</div>' +
      '<div class="sf-inline">' +
      '<button id="btn-open-sites" class="pf-add" type="button">打开托管平台</button>' +
      '<span class="sf-desc">把 HTML/文件夹拖进去即发布成网址，收集的表单数据也托管在服务端。</span>' +
      '</div>' +
      '<p class="sf-desc">助手可在对话里用 publish_webpage 工具直接把工作区里的网页发布成网址；这里打开的是网页管理端（拖拽上传 / 站点列表 / 数据下载）。</p>' +
      '</div>';
    // 地址字段（仅独立形态）：回显 + 改动即存（单字段 PATCH，失败回滚——与设置页
    // bindRemoteInput 同一套契约）
    if (!marketLive) {
      let c = null;
      try { c = await api('/api/config'); } catch (e) { /* 拉取失败：字段空着可填，保存时再报 */ }
      const urlEl = host.querySelector('#set-market-url');
      if (urlEl) {
        if (c) urlEl.value = c.marketURL || '';
        urlEl.onchange = async () => {
          const v = urlEl.value.trim();
          try {
            const back = await api('/api/config', { method: 'POST', body: JSON.stringify({ marketURL: v }) });
            if (document.activeElement !== urlEl) urlEl.value = back.marketURL || '';
          } catch (e) {
            alert('保存失败：' + e.message);
            const cur = await api('/api/config').catch(() => null);
            if (cur && document.activeElement !== urlEl) urlEl.value = cur.marketURL || '';
          }
        };
      }
    }
    const openBtn = host.querySelector('#btn-open-sites');
    if (openBtn) openBtn.onclick = () => openSitesPlatform();
  }

  function clearNetCfg() {
    const host = document.getElementById('net-sites-cfg');
    if (host) { host.innerHTML = ''; host.classList.add('hidden'); }
  }

  // 市场组件激活态变化（热开关/注册注销）→ 重挂本段：地址字段在「归市场组件」与
  // 「本段顶上」之间联动
  function onCxChange() { renderNetCfg(); }

  /* ---------- 注册（init 建立一切，dispose 完整撤销） ---------- */

  function init() {
    injectDOM();
    if (legacyHost) return; // 让位形态：老前端的按钮/视图/window 接口全在场，本组件不注入不覆盖
    if (btnEl) btnEl.onclick = () => { // 与企业技能包 / 工作流同款 toggle：开着再点关闭，关着点开
      if (sitesViewOpen()) { closeSites(); return; }
      openSitesPlatform();
    };
    // Esc 关闭平台视图（有任何弹窗开着时不抢键：确认框/设置自己处理 Esc；
    // 市场的 Esc 由 skill-market 组件自己管）
    addDocKey((ev) => {
      if (ev.key !== 'Escape' || !sitesViewOpen()) return;
      const anyModalOpen = [...document.querySelectorAll('.modal')].some((m) => !m.classList.contains('hidden'));
      if (anyModalOpen) return;
      closeSites();
    });
    // 主程序引用点（app-chat.js 回首页/开对话收起平台页）经 window 接口调用，
    // 摘除组件后这些引用自动退化为「不存在」
    window.sitesViewOpen = sitesViewOpen;
    window.closeSites = closeSites;
    renderNetCfg(); // 设置 · 网络访问页的配置段
    // 市场组件激活态变化时联动重挂（地址字段归属切换，见 renderNetCfg）
    window.addEventListener('cx:change', onCxChange);
  }

  function dispose() {
    if (legacyHost) { legacyHost = false; return; } // 让位形态什么都没装，无可撤
    if (sitesViewOpen()) closeSites(); // 平台正开着：先还回对话区
    docKeys.forEach((fn) => document.removeEventListener('keydown', fn));
    docKeys.length = 0;
    window.removeEventListener('cx:change', onCxChange);
    if (btnEntry) { btnEntry.remove(); btnEntry = null; btnEl = null; }
    else if (btnEl) { btnEl.remove(); btnEl = null; } // 旧 engine 的 DOM 注入 fallback
    if (viewEl) { viewEl.remove(); viewEl = null; }
    delete window.sitesViewOpen;
    delete window.closeSites;
    clearNetCfg();
  }

  CX.register({ id: ID, init, dispose });
})();
