// 组件 user-memory「AI 记忆与拟人化」的浏览器半边：
//   - 设置 → AI 记忆与拟人化 独立设置页（CX.registerSettings pane）：AI 对话
//     气泡颜色选择 + 用户称呼 / 用户身份与喜好 / AI 助理名字 / AI 助理身份；
//     **无自带保存按钮**——改动暂存在 DOM 草稿，随设置页右下角的全局「保存」
//     统一落库（CX.registerSettings 的 save 钩子 → CX.saveSettingsSections，
//     失败时设置窗不关、状态行亮错、完整草稿留在输入框就地修正）；
//   - 长度约束（1000 字/字段）在前端把关：输入超长计数行红字预警，保存按
//     截断版提交（服务端对超长整单报错，不截断存储）；
//   - 气泡颜色是本地生效的前端设置：读记忆文档里的 bubbleColor，往
//     <html> 注入 CSS 变量 --ai-bubble（web/style.css 的 .msg.user.ai 气泡
//     引用它，缺省回退主题灰）。保存成功才应用，组件卸载即还原默认灰。
// 设置页 UI 全部复用主站全局类，不依赖其它组件的样式（也不带 client.css）：
// 色板=主题色那套（.acc-swatch/.cur），输入框=网络访问页那套
// （.set-field/.sf-label/.pf-input），按钮 .mc-btn、说明 .sf-desc/.pf-note——
// 深浅色主题随全站 CSS 变量自动协调。
// 数据全部走主程序的 GET/POST /api/user-memory（usermemory.go，同一份
// md 文档，长期助理经 call_api 也走它；服务端对超长字段整单报错——前端
// 提交前已截断，正常不会触发）。
(function () {
  'use strict';

  var LIMIT = 1000; // 与后端 userMemoryFieldLimit 一致
  var CSS_VAR = '--ai-bubble';

  // 预设色板（浅色系与亮色主题搭；暗色主题下自定义色自担对比度）；空串=默认灰。
  // 交互与 设置→常规→主题色 同一套色板组件（style.css 的 .acc-swatch 圆点 +
  // .cur 选中描边 + 自定义取色器），仅色值清单不同：气泡色第一位是灰（跟随
  // 主题），主题色板第一位是主题蓝。
  var PRESETS = [
    { c: '', n: '默认灰（跟随主题）' },
    { c: '#dbeafe', n: '浅蓝' },
    { c: '#e0f2fe', n: '浅青' },
    { c: '#dcfce7', n: '浅绿' },
    { c: '#fef3c7', n: '浅黄' },
    { c: '#ffedd5', n: '浅橙' },
    { c: '#ede9fe', n: '浅紫' },
    { c: '#fce7f3', n: '浅粉' },
  ];

  function runeLen(s) { return Array.from(s == null ? '' : String(s)).length; }

  function clamp(s) {
    var a = Array.from(String(s == null ? '' : s));
    return a.length > LIMIT ? a.slice(0, LIMIT).join('') : a.join('');
  }

  function apiErr(res, text) {
    try {
      var j = JSON.parse(text || '');
      if (j && j.error) return j.error;
    } catch (e) { /* 非 JSON 错误体 */ }
    return 'HTTP ' + res.status;
  }

  // 气泡颜色注入/还原（记在 <html> 上，主对话/子对话/任务会话全部视图共享）
  function setColor(c) {
    var root = document.documentElement;
    c = (c || '').trim();
    if (/^#[0-9a-fA-F]{3,8}$/.test(c)) root.style.setProperty(CSS_VAR, c);
    else root.style.removeProperty(CSS_VAR);
  }

  function loadMemory(session) {
    var url = '/api/user-memory' + (session ? '?s=' + encodeURIComponent(session) : '');
    return fetch(url).then(function (res) {
      if (!res.ok) throw new Error(apiErr(res, ''));
      return res.json();
    });
  }

  function saveMemory(body, session) {
    var url = '/api/user-memory' + (session ? '?s=' + encodeURIComponent(session) : '');
    return fetch(url, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }).then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error(apiErr(res, t)); });
      return res.json();
    });
  }

  // cx:session 监听句柄（init 注册、dispose 摘除；不用 this 存——CX 卸载时
  // 裸调 dispose()，this 不绑定组件对象）
  var onSessionRef = null;

  /* ================= 设置页 ================= */

  function mountSettings(el) {
    el.innerHTML = '';

    // 会话语境：挂进「本对话的组件与插件」浮窗（#cxsession 内）＝按当前会话
    // 读写（该会话启用了独立记忆时读会话视角、保存写会话覆盖）；挂进设置
    // 浮窗＝全局。挂载时判定一次（浮窗切换会整段重挂，语境不会中途漂移）。
    var inCxs = !!(el.closest && el.closest('#cxsession'));
    var sessCtx = inCxs && typeof CX !== 'undefined' && CX.session ? CX.session() : '';

    // 说明行 / 分区：全站样式（网络访问页同款结构）
    var note = function (text) {
      var p = document.createElement('p');
      p.className = 'sf-desc';
      p.textContent = text;
      el.appendChild(p);
      return p;
    };
    var group = function (title) {
      var g = document.createElement('div');
      g.className = 'gen-section';
      var h = document.createElement('h4');
      h.textContent = title;
      g.appendChild(h);
      el.appendChild(g);
      return g;
    };

    var state = { userName: '', userMemory: '', aiName: '', aiIdentity: '', bubbleColor: '' };

    // ---- AI 对话气泡颜色（本地生效的前端设置，也存记忆文档） ----
    group('AI 对话气泡颜色');
    note('AI 发出的消息气泡当前是默认灰，可换成喜欢的颜色；只影响本地界面的显示。');

    // 即时预览块：一个左对齐「AI 消息」气泡，跟随当前选择变色
    var preview = document.createElement('div');
    preview.style.cssText = 'margin:6px 0 10px;max-width:320px;padding:10px 15px;border-radius:14px 14px 14px 4px;line-height:1.6;font-size:.95em;background:var(--ai-bubble, var(--chip));transition:background .2s;';
    preview.textContent = '小助理：这是 AI 消息气泡的颜色预览。';
    el.appendChild(preview);

    // 色板 + 自定义取色器：与 设置→常规→主题色 同一套色板组件
    // （.acc-swatch 圆点 + .cur 选中描边，style.css 全局类；容器布局照
    // #accent-row 复刻——它是按 id 选择器写的，这里不占那个 id）
    var pickerBox = document.createElement('div');
    pickerBox.style.cssText = 'display:flex;gap:8px;align-items:center;flex-wrap:wrap;padding:2px 0 4px;';
    var markSel = function () {
      pickerBox.querySelectorAll('.acc-swatch').forEach(function (b) {
        if (b.dataset.cxp === state.bubbleColor) b.classList.add('cur');
        else b.classList.remove('cur');
      });
    };
    PRESETS.forEach(function (p) {
      var b = document.createElement('button');
      b.type = 'button';
      b.className = 'acc-swatch';
      b.dataset.cxp = p.c;
      b.title = p.n;
      b.setAttribute('aria-label', '气泡颜色：' + p.n);
      b.style.background = p.c || 'var(--chip)';
      b.onclick = function () { setDraft({ bubbleColor: p.c }, true); };
      pickerBox.appendChild(b);
    });
    var custom = document.createElement('input');
    custom.type = 'color';
    custom.title = '自定义颜色';
    custom.value = /^#[0-9a-fA-F]{6}$/.test(state.bubbleColor) ? state.bubbleColor : '#dbeafe';
    // 取色器外观与主题色板那枚同款（#accent-row input[type=color] 的规则）
    custom.style.cssText = 'width:24px;height:24px;border:1px solid var(--border);border-radius:6px;background:none;padding:0;cursor:pointer;';
    custom.oninput = function () { setDraft({ bubbleColor: custom.value }, true); };
    pickerBox.appendChild(custom);
    el.appendChild(pickerBox);
    note('色板第一格「默认灰」= 跟随主题；色板外可用取色器自定义。');

    // ---- 记忆文本字段 ----
    group('用户与 AI 的长期设定');
    note('这些内容会注入每个对话的环境提示词：AI 据此称呼你、自称与定位。各栏上限 1000 字：超出的部分保存时按前 1000 字入库（计数行会提示被截断，输入框保留你的完整草稿）；AI 写入超长会直接报错。改动随设置页底部的「保存」一并生效。任务对话中 AI 可用 user_memory 工具更新「用户身份与喜好」一栏；长期助理可读写整个记忆文件。');

    // 字段行：网络访问页同款（.set-field + .sf-label + .pf-input，圆角边框、
    // 聚焦描边主题色，深浅色主题随全站变量自动协调）
    function textField(label, key, multiLine) {
      var box = document.createElement('div');
      box.className = 'set-field';
      var lab = document.createElement('div');
      lab.className = 'sf-label';
      lab.textContent = label;
      box.appendChild(lab);
      var ta;
      if (multiLine) {
        ta = document.createElement('textarea');
        ta.rows = 4;
        ta.style.cssText = 'resize:vertical;min-height:70px;line-height:1.6;font-family:inherit;';
      } else {
        ta = document.createElement('input');
        ta.type = 'text';
        ta.autocomplete = 'off';
      }
      ta.className = 'pf-input';
      ta.dataset.umKey = key; // saveDraft 按 DOM 收集草稿的定位键（无按钮后落库走全局保存）
      // dirty：用户动过此栏。loadMemory 异步回填只覆盖没动过的字段——回填
      // 与用户输入赛跑时（打开即秒填），后到的服务端数据不能冲掉草稿。
      var f = { el: ta, cnt: null, upd: null, dirty: false };
      ta.addEventListener('input', function () { f.dirty = true; updCount(); });
      box.appendChild(ta);
      var cnt = document.createElement('p');
      cnt.className = 'sf-desc';
      cnt.style.cssText = 'text-align:right;margin:4px 0 0;';
      box.appendChild(cnt);
      var updCount = function () {
        var n = runeLen(ta.value);
        cnt.textContent = n + ' / ' + LIMIT;
        // 超长红字：告知保存口径（截断入库），草稿原样保留
        if (n > LIMIT) {
          cnt.textContent = n + ' / ' + LIMIT + '（超出 ' + (n - LIMIT) + ' 字：保存时只保留前 ' + LIMIT + ' 字）';
          cnt.style.color = 'var(--danger)';
        } else {
          cnt.style.color = '';
        }
      };
      updCount();
      el.appendChild(box);
      f.cnt = cnt;
      f.upd = updCount;
      fields[key] = f;
    }

    var fields = {};
    textField('用户称呼（想 AI 怎么称呼你？）', 'userName', false);
    textField('用户身份与喜好（记忆：身份、偏好、习惯、项目背景，AI 可在对话中更新这一栏）', 'userMemory', true);
    textField('AI 助理名字（你想怎么称呼 AI）', 'aiName', false);
    textField('AI 助理身份（AI 的定位与人设）', 'aiIdentity', true);

    // ---- 草稿状态：色板与取色器即时改草稿值（预览同步），保存才落库 ----
    var colorTouched = false; // 用户点过色板/取色器后，服务端回填不再覆盖颜色草稿
    function setDraft(patch, fromUser) {
      if (fromUser) colorTouched = true;
      Object.keys(patch).forEach(function (k) { state[k] = patch[k]; });
      markSel();
      preview.style.background = state.bubbleColor
        ? state.bubbleColor
        : 'var(--chip)';
    }

    // 状态行：无保存按钮（设置页右下角的全局「保存」统一落库）；这行只承载
    // 读取/保存失败的错误提示——失败时全局不关窗，用户就地修正后可再点保存。
    var status = document.createElement('p');
    status.className = 'sf-desc';
    status.style.cssText = 'margin:10px 0 0;';
    status.dataset.umStatus = '';
    el.appendChild(status);
    el.dataset.umHost = '1'; // 供 saveDraft 按 DOM 定位（见 collectDraft）

    // 用已保存的数据回填（仅初始加载）。脏检查：用户已动过的字段（dirty /
    // colorTouched）不覆盖——loadMemory 的异步回包与用户输入赛跑时，草稿优先。
    // 全局保存成功后不回填（关窗即弃 DOM）；保存失败停在原地草稿天然保留。
    function fill(d) {
      ['userName', 'userMemory', 'aiName', 'aiIdentity'].forEach(function (k) {
        var f = fields[k];
        if (!f || f.dirty) return;
        f.el.value = d[k] || '';
        f.upd();
      });
      if (!colorTouched) {
        state.bubbleColor = d.bubbleColor || '';
        if (/^#[0-9a-fA-F]{6}$/.test(state.bubbleColor)) custom.value = state.bubbleColor;
        setDraft({});
      }
    }

    fill({ userName: '', userMemory: '', aiName: '', aiIdentity: '', bubbleColor: '' });
    loadMemory(sessCtx).then(function (d) {
      if (!el.isConnected) return; // 设置页已收起：结果作废
      fill(d);
      if (d.session) {
        // 本对话使用独立记忆：说明行亮出来（用户在浮窗里要知道自己改的是哪份）
        var tip = note('本对话使用独立的记忆设定：这里查到与保存的内容只影响本对话（全局记忆在 设置 → AI 记忆与拟人化 维护）。');
        tip.style.color = 'var(--accent)';
      }
    }).catch(function (e) {
      status.style.color = 'var(--danger)';
      status.textContent = '读取失败：' + (e && e.message ? e.message : e);
    });
  }

  // collectDraft 从挂出的 DOM 收集草稿（落库走 CX.registerSettings 的 save
  // 钩子——设置浮窗右下角「保存」或会话浮窗底部「保存」时
  // CX.saveSettingsSections 调到这里；save 与 mount 不共享闭包，故按稳定 id
  // 定位）。**哪个浮窗开着收哪个**——保存动作必然来自开着的那个浮窗的底栏；
  // 两个都开着时设置浮窗优先（与全局保存按钮同一语境）。不能只看 pane 元素
  // 在不在：设置浮窗的组件 pane 在页面 boot 时就预建**空壳**（mount 发生在
  // 打开时），空壳里没有输入框，盲收它会拿全空字段把服务端清掉（实测踩过）。
  function collectDraft() {
    var settingsOpen = !document.getElementById('settings').classList.contains('hidden');
    var cxsOpen = !document.getElementById('cxsession').classList.contains('hidden');
    var pane, sess = '';
    if (settingsOpen) {
      pane = document.querySelector('#set-pane-component-user-memory');
    } else if (cxsOpen) {
      // 会话浮窗语境：带当前会话读写（该会话独立时写覆盖，服务端按 ?s= 分流）
      pane = document.getElementById('cxs-pane-cxs-component-user-memory');
      sess = (typeof CX !== 'undefined' && CX.session) ? CX.session() : '';
    }
    if (!pane) return null; // 没有开着的浮窗（组件中途被关等）：视为无事发生
    var byKey = {};
    pane.querySelectorAll('[data-um-key]').forEach(function (t) { byKey[t.dataset.umKey] = t.value; });
    // 颜色草稿：选中色板的 data-cxp（空串=默认灰）；自定义色（无色板命中）
    // 读取色器当前值
    var cur = pane.querySelector('.acc-swatch.cur');
    var picker = pane.querySelector('input[type=color]');
    var color = cur ? cur.dataset.cxp : (picker ? picker.value : '');
    var statusEl = pane.querySelector('[data-um-status]');
    return { byKey: byKey, color: color, statusEl: statusEl, pane: pane, sess: sess };
  }

  // saveDraft 保存钩子：收集 DOM 草稿 → 前端把关长度（超长按前 1000 字提交，
  // 输入框保留完整草稿）→ POST（会话浮窗语境带 ?s=：该会话独立时服务端写
  // 会话覆盖、全局文档不动）。成功返回 true（全局流程继续，关窗）；失败在
  // 状态行亮错并返回 false（设置窗不关，修正后可再点保存）。
  function saveDraft() {
    var d = collectDraft();
    if (!d) return Promise.resolve(true);
    var body = {
      userName: d.byKey.userName || '',
      userMemory: d.byKey.userMemory || '',
      aiName: d.byKey.aiName || '',
      aiIdentity: d.byKey.aiIdentity || '',
      bubbleColor: d.color || '',
    };
    Object.keys(body).forEach(function (k) {
      if (typeof body[k] === 'string' && runeLen(body[k]) > LIMIT) {
        body[k] = clamp(body[k]);
      }
    });
    return saveMemory(body, d.sess).then(function (saved) {
      setColor(saved.bubbleColor); // 保存成功才把颜色应用到对话区气泡
      if (d.statusEl) { d.statusEl.textContent = ''; d.statusEl.style.color = ''; }
      return true;
    }).catch(function (e) {
      if (d.statusEl) {
        d.statusEl.style.color = 'var(--danger)';
        d.statusEl.textContent = '「AI 记忆与拟人化」保存失败：' + (e && e.message ? e.message : e);
      }
      return false;
    });
  }

  /* ================= 注册 ================= */

  CX.register({
    id: 'user-memory',

    init() {
      // 颜色尽早应用（对话区一打开就是所选色，而不是先灰后变色）；按当前
      // 会话视角取色（该会话独立记忆时用会话覆盖的 bubbleColor）
      var applyColor = function () {
        loadMemory(typeof CX.session === 'function' ? CX.session() : '')
          .then(function (d) { setColor(d.bubbleColor); })
          .catch(function () { /* 组件关闭等，忽略 */ });
      };
      applyColor();
      // 会话切换（cx:session）时按新会话视角重取颜色；dispose 摘干净
      onSessionRef = function () { applyColor(); };
      window.addEventListener('cx:session', onSessionRef);

      CX.registerSettings({
        id: 'user-memory',
        title: 'AI 记忆与拟人化',
        pane: {
          label: 'AI 记忆与拟人化',
          icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2"/><circle cx="12" cy="7" r="4"/><path d="M12 12v0"/></svg>',
        },
        mount: mountSettings,
        save: saveDraft, // 接入设置页右下角「保存」：改动暂存 DOM，全局保存统一落库
      });
    },

    dispose() {
      if (onSessionRef) { window.removeEventListener('cx:session', onSessionRef); onSessionRef = null; }
      CX.unregisterSettings('user-memory');
      // 还原默认灰：组件关掉，自定义色不再生效
      document.documentElement.style.removeProperty(CSS_VAR);
    },
  });
})();
