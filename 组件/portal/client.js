/* 组件：充值与个人中心（portal）—— 浏览器半边。
 *
 * 三处入口（全部由本组件掌管，开关关闭/服务端没开门户即全部消失）：
 *   1. 设置·模型 公司通道卡片：额度区旁「充值」按钮（挂载点由主程序 buildProvCard
 *      提供，.pf-card-slot；本组件监听 prov:render 事件重新挂——卡片是全量重建的）；
 *   2. 向导接入完成：主程序广播 wizard:done（portalURL 非空=公司通道+门户已开），
 *      紧接着自动弹个人中心浮窗做引导（纯本地/个人配置不弹）；
 *   3. 报错条：主程序广播 run:error，本组件按词表判定「余额/积分不足」类报错
 *      （词表与 llm.go quotaWords 同源），在「重试」旁加「去充值」。
 *
 * 浮窗内嵌 iframe 加载本地同源 /portal（portalproxy.go 反向代理到服务端门户，
 * 自签证书由程序证书策略接管——iframe 直连服务端会被浏览器证书校验静默拒绝）。
 * iframe 常驻不销毁：来回开关不打扰已登录/已打开的页面（与小程序平台同一理念）。
 *
 * 主程序只提供通用对接点（prov:render / run:error / wizard:done 三个事件与卡片
 * 挂载点），不认识本组件；本组件不改主程序函数。dispose 撤干净一切，可热关闭。 */
(function () {
  if (!window.CX) return;

  const ID = 'portal';

  /* 余额/积分不足类报错词表——与 llm.go quotaWords 保持同源（那边是模型回退
   * 判定用，这里是报错条入口判定用；改词表两处一起改）。不区分大小写匹配。 */
  const QUOTA_WORDS = ['余额', '额度', '欠费', '充值', 'balance', 'quota', 'insufficient', 'arrear'];

  let portalURL = '';          // 服务端下发的门户地址（空=没开门户，入口隐藏）
  let modalEl = null;          // 浮窗（常驻 DOM，hidden 控制）
  let frameEl = null;
  let docKey = null;           // Esc 监听（dispose 解绑）

  const $id = (i) => document.getElementById(i);

  function quotaLike(text) {
    const s = String(text || '').toLowerCase();
    return QUOTA_WORDS.some((w) => s.indexOf(w.toLowerCase()) >= 0);
  }

  /* ---------- 门户地址（缓存 + 刷新） ---------- */

  // refreshPortalURL 用主程序最新值覆盖缓存（/api/models 的 portalURL 来自
  // Cfg.PortalURL，服务端开关门户后随公告/向导变化）——**允许清空为无**：
  // 「从有到无」必须有明确的清空路径，否则报错条入口会拿旧值复活。
  async function refreshPortalURL() {
    try {
      const d = await api('/api/models'); // 主程序在该响应里带 portalURL（server.go）
      if (d && typeof d.portalURL === 'string') portalURL = d.portalURL;
    } catch (e) { /* 拉不到就用缓存（离线时入口可能已在，点击走代理的错误提示） */ }
    return portalURL;
  }

  /* ---------- 浮窗 ---------- */

  function ensureModal() {
    if (modalEl) return;
    modalEl = document.createElement('div');
    modalEl.id = 'portal-modal';
    modalEl.className = 'modal hidden';
    // 卡片 + 标题栏 + iframe；拖选文字滑出卡片落到遮罩上不算点击关闭（与向导
    // 同一道防误触：只有按下就在遮罩上才关）
    modalEl.innerHTML =
      '<div class="portal-win" role="dialog" aria-label="个人中心">' +
      '  <div class="portal-head">' +
      '    <b>个人中心</b><span class="portal-sub">注册 / 登录 / 充值</span>' +
      '    <button type="button" class="portal-close" title="关闭">✕</button>' +
      '  </div>' +
      // 注册条已移除（2026-11-13 极简化）：门户页登录卡自带「注 册 / 登 录」
      // 双按钮，本组件打开 iframe 时把向导姓名与本机设备号带在 URL 上，门户
      // 页注册自动采用（充值到设备的匹配链同源）——浮窗内不再叠一层表单。
      '  <iframe class="portal-frame" title="个人中心（注册 / 登录 / 充值）"></iframe>' +
      '</div>';
    document.body.appendChild(modalEl);
    frameEl = modalEl.querySelector('.portal-frame');
    modalEl.querySelector('.portal-close').onclick = closePortal;
    let downOnMask = false;
    modalEl.addEventListener('mousedown', (e) => { downOnMask = e.target === modalEl; });
    modalEl.addEventListener('click', (e) => {
      if (e.target === modalEl && downOnMask) closePortal();
    });
  }

  // detectLogin/doRegister 已随注册条一并移除（2026-11-13）：注册入口统一收进
  // 门户页登录卡（左「注 册」右「登 录」），本组件只负责带 name/device 参数。

  // deviceID 本机稳定设备号：取主程序的 remoteAccess.myId（/api/config，与向导
  // provision 上报的设备号同一个值——「充值到设备令牌」按 人名+设备号 匹配
  // provision 设备级记录，两边必须同源，各生成各的就永远对不上）。拉不到
  // （主程序过旧/接口失败）退回组件自生成号：只影响「充值到设备」的匹配
  // （匹配不上自动走门户钱包，不阻塞充值）。
  let devidPromise = null;
  function portalDeviceID() {
    if (!devidPromise) {
      devidPromise = api('/api/config')
        .then((c) => {
          const v = (c && c.remoteAccess && c.remoteAccess.myId) || '';
          if (v) return v;
          throw new Error('no myId');
        })
        .catch(() => legacyDeviceID());
    }
    return devidPromise;
  }

  // legacyDeviceID 组件本地生成兜底（老主程序没有 myId 时）：localStorage 持久。
  function legacyDeviceID() {
    let v = '';
    try { v = localStorage.getItem('cx:portal:devid') || ''; } catch (e) {}
    if (!v) {
      const abc = 'abcdefghjkmnpqrstuvwxyz23456789';
      v = 'jx-';
      for (let i = 0; i < 8; i++) v += abc[Math.floor(Math.random() * abc.length)];
      try { localStorage.setItem('cx:portal:devid', v); } catch (e) {}
    }
    return v;
  }

  function portalOpen() {
    return !!modalEl && !modalEl.classList.contains('hidden');
  }

  // openPortal 打开充值浮窗（三处入口共用）。没开门户（portalURL 空）时先刷新
  // 一次再判——仍空就静默返回：入口按约定不该出现，这里是不让「点了报错」的
  // 最后一道闸。首次加载 iframe 时把向导姓名（publisherName）与本机设备号带在
  // URL 上：门户页注册卡自动采用——「充值到设备」按 人名+设备号 匹配 provision
  // 设备级记录，两边必须同源。
  async function openPortal() {
    if (!portalURL) await refreshPortalURL();
    if (!portalURL) return;
    ensureModal();
    if (!frameEl.src || frameEl.src === 'about:blank') {
      const name = api('/api/config').then((c) => (c && c.publisherName) || '').catch(() => '');
      const dev = portalDeviceID().catch(() => '');
      Promise.all([name, dev]).then(([n, d]) => {
        const qs = new URLSearchParams();
        if (n) qs.set('name', n);
        if (d) qs.set('device', d);
        const q = qs.toString();
        frameEl.src = '/portal' + (q ? '?' + q : ''); // 同源代理，登录态随 cookie 走
      });
    }
    modalEl.classList.remove('hidden');
  }

  function closePortal() {
    if (modalEl) modalEl.classList.add('hidden');
  }

  /* ---------- 三处入口 ---------- */

  // 1) 模型页公司通道卡片：prov:render 后往挂载点放「充值」按钮（卡片全量重建
  //    时按钮随之消失，靠这个事件补回）。挂载点 dataset.portalURL 空（服务端没
  //    开门户）就不放——空容器不占位（client.css 的 :empty 规则）。dataset 是
  //    主程序渲染卡片时的最新值，顺带同步进缓存（含「从有到无」的清空）。
  function onProvRender() {
    let fresh = null;
    document.querySelectorAll('.pf-card-slot[data-cx-slot="prov-actions"]').forEach((slot) => {
      if (slot.dataset.provId === 'company-internal' && fresh === null) fresh = slot.dataset.portalURL || '';
      slot.querySelectorAll('[data-cx-owner="' + ID + '"]').forEach((b) => b.remove());
      if (!slot.dataset.portalURL) return;
      const btn = document.createElement('button');
      btn.type = 'button';
      btn.className = 'pp-btn';
      btn.dataset.cxOwner = ID;
      btn.textContent = '充值';
      btn.title = '打开个人中心：注册 / 登录 / 给公司通道充值';
      btn.onclick = (e) => { e.stopPropagation(); openPortal(); };
      slot.appendChild(btn);
    });
    if (fresh !== null) portalURL = fresh;
  }

  // 2) 向导接入完成：紧接着自动弹个人中心（只在公司通道 + 服务端已开门户时——
  // payload.portalURL 非空即两条件都满足；纯本地/个人配置不会带）。无论带没带
  // 都先覆盖缓存（空=门户已关，把旧值清掉——向导是服务端口径最直接的回传），
  // 再决定弹不弹。等一拍让向导浮窗先收下去，两层浮窗不打架。
  function onWizardDone(d) {
    portalURL = (d && d.portalURL) || '';
    if (!portalURL) return;
    setTimeout(() => { openPortal(); }, 350);
  }

  // 3) 报错条：余额类报错 + 门户开着 → 「重试」旁加「去充值」。box 是主程序
  //    报错条容器（重建时按钮随旧 DOM 消失，不需要追踪）；「接入公司通道」
  //    那条专属文案不含余额词，天然不冲突。每次先刷新门户地址再判——组件缓存
  //    可能是旧值（服务端中途关门户），报错条入口不能拿旧值复活。
  function onRunError(d) {
    if (!d || !d.box || !quotaLike(d.text)) return;
    refreshPortalURL().then(() => addPayBtn(d.box));
  }

  function addPayBtn(box) {
    if (!portalURL || !box || box.querySelector('.pp-btn')) return;
    const btn = document.createElement('button');
    btn.className = 're-btn re-retry pp-btn';
    btn.type = 'button';
    btn.dataset.cxOwner = ID; // dispose 按 [data-cx-owner] 清理，动态按钮必须挂同款标记
    btn.textContent = '去充值';
    btn.title = '余额不足——打开个人中心充值页';
    btn.onclick = () => openPortal();
    const close = box.querySelector('.re-close');
    if (close) box.insertBefore(btn, close); // 排在「✕」前、「重试」后
    else box.appendChild(btn);
  }

  /* ---------- 注册（init 建立一切，dispose 完整撤销） ---------- */

  function init() {
    window.JX.on('prov:render', onProvRender);
    window.JX.on('wizard:done', onWizardDone);
    window.JX.on('run:error', onRunError);
    docKey = (ev) => {
      if (ev.key !== 'Escape' || !portalOpen()) return;
      // 有别的弹窗开着时不抢键（与设置/向导的 Esc 同一口径）
      const anyModal = [...document.querySelectorAll('.modal')].some((m) => m !== modalEl && !m.classList.contains('hidden'));
      if (anyModal) return;
      ev.stopPropagation();
      closePortal();
    };
    document.addEventListener('keydown', docKey, true);
    refreshPortalURL();
    onProvRender(); // 组件热重载/设置页已开着时补挂一次
  }

  function dispose() {
    window.JX.off('prov:render', onProvRender);
    window.JX.off('wizard:done', onWizardDone);
    window.JX.off('run:error', onRunError);
    if (docKey) { document.removeEventListener('keydown', docKey, true); docKey = null; }
    document.querySelectorAll('[data-cx-owner="' + ID + '"]').forEach((b) => b.remove());
    closePortal();
    if (modalEl) { modalEl.remove(); modalEl = null; frameEl = null; regEl = null; }
  }

  CX.register({ id: ID, init, dispose });
})();
