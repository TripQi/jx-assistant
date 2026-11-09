/* app-hotkeys.js —— 全局快捷键统一登记（HK）。第二个加载（app-core 之后）。
 *
 * 设计意图（人的为什么）：
 * - 快捷键原先散落在各页（常规页发送键、语音页听写键）各自为政，用户找不到、
 *   也改不齐。这里给一个**唯一登记口**：界面内置快捷键与组件（经
 *   JX.registerHotkey，如语音）交来的快捷键进同一张表，设置 · 快捷键 页统一
 *   展示 / 录制 / 恢复默认；原页面保留同款键帽控件，两边改的是同一份数据，
 *   经 hotkeys:change 事件互相同步。
 * - 槽位语义沿用语音组件已验证的模型：槽值 = null（跟默认）| 数组（自定义组合）
 *   | 空数组（停用）。存储归各注册方：界面内置存 jx.hotkeys（本机），
 *   语音仍存自己的 cx:voice:*，老用户设置零迁移。
 * - 方向键导航只在「鼠标最后一次点在左侧栏 / 对话区（或从未点击）」时生效；
 *   点进右侧栏、弹窗、输入框后按键归还给那里——浏览器和输入框的按键不被抢。
 */

(function () {
  if (window.HK) return;

  /* ---------- 键名体系（与语音组件同一套，补方向键与 Enter 的显示名） ---------- */
  const KEY_NAMES = {
    ctrl: 'Ctrl', alt: 'Alt', shift: 'Shift', meta: 'Win', space: '空格', enter: 'Enter',
    arrowup: '↑ 上', arrowdown: '↓ 下', arrowleft: '← 左', arrowright: '→ 右',
    'mouse-side': '鼠标侧键', 'mouse-side-1': '鼠标侧键1', 'mouse-side-2': '鼠标侧键2',
    'mouse-left': '鼠标左键', 'mouse-right': '鼠标右键',
  };
  const MOD_ORDER = { ctrl: 0, alt: 1, shift: 2, meta: 3 };
  const isModKey = (k) => k === 'ctrl' || k === 'alt' || k === 'shift' || k === 'meta';

  // 事件 → 规范化键名（录制器与引擎共用）。Enter 不收：它属于发送键的领地，
  // 不该被录成别的组合（发送键是 choice 型，在 choices 里写字面量）；Esc 是
  // 录制器的取消键，同样不收。
  function normKey(e) {
    const k = e.key;
    if (k === 'Control') return 'ctrl';
    if (k === 'Alt') return 'alt';
    if (k === 'Shift') return 'shift';
    if (k === 'Meta') return 'meta';
    if (k === 'ArrowUp') return 'arrowup';
    if (k === 'ArrowDown') return 'arrowdown';
    if (k === 'ArrowLeft') return 'arrowleft';
    if (k === 'ArrowRight') return 'arrowright';
    const lk = String(k).toLowerCase();
    if (/^[a-z0-9]$/.test(lk)) return lk;
    if (/^f([1-9]|1[0-2])$/.test(lk)) return lk;
    if (lk === ' ') return 'space';
    return null;
  }
  function normMouse(e) {
    // button 3=XBUTTON1（侧键1·后退位）4=XBUTTON2（侧键2·前进位）：
    // 录制器录出具体键名，用户才能"只绑某一枚侧键"；旧配置的 'mouse-side'
    // （两枚都算）仍由各引擎自行兼容
    return { 0: 'mouse-left', 2: 'mouse-right', 3: 'mouse-side-1', 4: 'mouse-side-2' }[e.button] || null;
  }
  // 组合的规范形：修饰键在前（固定序）+ 其余字典序，加号连接——引擎匹配与冲突
  // 查重都用它比较
  function canonical(keys) {
    const mods = [], rest = [];
    for (const k of keys || []) (isModKey(k) ? mods : rest).push(k);
    mods.sort((a, b) => MOD_ORDER[a] - MOD_ORDER[b]);
    rest.sort();
    return [...mods, ...rest].join('+');
  }
  function keyLabel(k) { return KEY_NAMES[k] || String(k).toUpperCase(); }
  function comboText(keys) { return (keys || []).map(keyLabel).join('+'); }

  /* ---------- 界面内置快捷键的槽位存储（jx.hotkeys：id -> {main,sub}） ---------- */
  function builtinGet(id, slot) {
    const m = lsGet('jx.hotkeys', {});
    const v = m && m[id] ? m[id][slot] : undefined;
    return v === undefined ? null : v;
  }
  function builtinSet(id, slot, v) {
    const m = lsGet('jx.hotkeys', {}) || {};
    const cur = m[id] || {};
    if (v === null) delete cur[slot]; else cur[slot] = v;
    if (cur.main || cur.sub) m[id] = cur; else delete m[id];
    lsSet('jx.hotkeys', m);
  }

  /* ---------- 注册表 ---------- */
  const HK = {
    _defs: [],       // 注册顺序即表格顺序（界面内置先注册在前）
    _boxes: [],      // 活着的键帽框/芯片组（数据变更时统一重渲染）：{box, def, slot}
    _recCount: 0,    // 进行中的录制会话数（>0 = 各快捷键引擎暂停触发）
    _index: new Map(), // canonical 组合 -> {def, slot}：引擎匹配与冲突查重共用

    isRecording() { return this._recCount > 0; },
    list() { return this._defs.slice(); },
    get(id) { return this._defs.find((d) => d.id === id) || null; },

    register(def) {
      if (!def || !def.id || !def.name) return null;
      const d = Object.assign(
        { kind: 'keys', source: '', mouseKeys: false, defaults: { main: [], sub: [] } },
        def
      );
      const i = this._defs.findIndex((x) => x.id === d.id);
      if (i >= 0) this._defs[i] = d; else this._defs.push(d); // 组件重注入：原位刷新
      this._changed(d.id, d);
      return d;
    },
    unregister(id) {
      const i = this._defs.findIndex((x) => x.id === id);
      if (i < 0) return;
      this._defs.splice(i, 1);
      this._changed(id, null);
    },

    // 槽位有效组合：数组=自定义（空=停用），null=跟默认（取 defaults）
    effectiveKeys(def, slot) {
      if (def.kind === 'choice') {
        const c = this.effectiveChoice(def);
        return (c && c.keys) || [];
      }
      let v = null;
      try { v = def.get(slot); } catch (e) { /* 组件存储读取失败按默认 */ }
      if (Array.isArray(v)) return v;
      return (def.defaults && def.defaults[slot]) || [];
    },
    effectiveChoice(def) {
      let v = null;
      try { v = def.getChoice(); } catch (e) { /* 同上 */ }
      return (def.choices || []).find((c) => c.v === v) || (def.choices || [])[0];
    },
    rawSlot(def, slot) {
      try { return def.get(slot); } catch (e) { return null; }
    },

    setSlot(def, slot, v) { def.set(slot, v); this._changed(def.id, def); },
    setChoice(def, v) { def.setChoice(v); this._changed(def.id, def); },
    // 清空＝主副两槽一起停用（行级；单槽停用是键帽框里的 ✕）
    clearDef(def) {
      if (def.kind === 'choice') return;
      def.set('main', []);
      def.set('sub', []);
      this._changed(def.id, def);
    },
    restoreDef(def) {
      if (def.kind === 'choice') {
        if (def.choices && def.choices.length) def.setChoice(def.choices[0].v);
      } else {
        def.set('main', null);
        def.set('sub', null);
      }
      this._changed(def.id, def);
    },
    restoreAll() { for (const d of this._defs.slice()) this.restoreDef(d); },

    // 冲突查重：返回占用者或 null。except={defId,slot} 允许同槽重录同值
    comboInUse(keys, except) {
      const c = canonical(keys);
      for (const [k, owner] of this._index) {
        if (k !== c) continue;
        if (except && owner.def.id === except.defId && owner.slot === except.slot) continue;
        return owner;
      }
      return null;
    },

    _changed(defId, def) {
      rebuildIndex();
      renderAllBoxes();
      if (def && typeof def.onChanged === 'function') {
        try { def.onChanged(); } catch (e) { console.warn('[HK] onChanged', defId, e); }
      }
      try { JX.emit('hotkeys:change', defId); } catch (e) { /* JX 缺席（极端加载序）忽略 */ }
      // 语音听写快捷键/触发方式变了 → 同步给独立语音输入法（VoiceTypeGo）：
      // 输入法是独立进程，读不到本页 localStorage，所以由主程序落一份
      // voicetype.json（只含热键/触发方式/颜色，见 voicetype_link.go）。
      if (defId === 'voice.dictation' && def) {
        try {
          const main = HK.effectiveKeys(def, 'main');
          const sub = HK.effectiveKeys(def, 'sub');
          // 触发方式由「设置·语音」决定，组件存在 cx:voice:shortcutMode（JSON 字符串）
          let mode = '';
          try {
            const raw = localStorage.getItem('cx:voice:shortcutMode');
            const v = raw ? JSON.parse(raw) : 'tap';
            mode = v === 'hold' ? 'hold' : 'tap';
          } catch (e) { /* 读不到就不带 mode，输入法沿用自己配置 */ }
          // 颜色也一起带上：这是**启动时必然会发生的一次推送**（组件注册即触发），
          // 不能只靠设置页的 applyAccent——用户不打开外观设置就永远拿不到颜色。
          let accent = '';
          try {
            const a = lsGet('jx.accent', '');
            if (a) accent = String(a).split('|')[0];
          } catch (e) { /* 没存过点缀色：不带，输入法用默认 */ }
          // 文字色无条件带上（与 accent 是否存过无关）：主程序的主题文字色。
          let textColor = '';
          try {
            textColor = getComputedStyle(document.documentElement).getPropertyValue('--text').trim();
          } catch (e) { /* 样式表可能还没算：下面按主题档位给值 */ }
          if (!textColor) {
            let dark = false;
            try {
              const t = lsGet('jx.theme', 'light');
              dark = t === 'dark' || (t === 'auto' && window.matchMedia &&
                window.matchMedia('(prefers-color-scheme: dark)').matches);
            } catch (e) { /* 读不到主题：按浅色 */ }
            textColor = dark ? '#e6e8eb' : '#1f2328'; // 与 style.css 的 --text 一致
          }
          fetch('/api/voicetype/link', {
            method: 'POST', headers: { 'Content-Type': 'application/json' },
            body: JSON.stringify({ main, sub, mode, accentColor: accent || undefined, textColor: textColor || undefined }),
          }).catch(() => {});
        } catch (e) { /* 组件未就绪：忽略，下一次变更再同步 */ }
      }
    },

    keyCapBox, choiceChips, renderTable, keyLabel, comboText,
  };
  window.HK = HK;
  // 组件通道：与 JX 其它注册口同一风格（组件 init 里注册、dispose 里注销）
  JX.registerHotkey = (def) => HK.register(def);
  JX.unregisterHotkey = (id) => HK.unregister(id);

  // 组合占用索引 + 冲突清单：按表格顺序展开全部生效组合，同一组合**先到者生效**
  //（后到的进冲突清单、不进引擎索引——「从上往下按第一个处理」）。choice 的
  // 各档键位也参与占位与冲突检测（它两档互斥但都该被保护）。
  const SLOT_LABEL = { main: '主快捷键', sub: '副快捷键', choice: '固定键位' };
  function rebuildIndex() {
    HK._index = new Map();
    HK._conflicts = [];
    const seen = new Map(); // canonical -> 第一个占用项
    const entries = [];
    for (const def of HK._defs) {
      if (def.kind === 'choice') {
        for (const c of def.choices || []) {
          if (c.keys && c.keys.length) entries.push({ def, slot: 'choice', keys: c.keys });
        }
        continue;
      }
      for (const slot of ['main', 'sub']) {
        const ks = HK.effectiveKeys(def, slot);
        if (ks.length) entries.push({ def, slot, keys: ks });
      }
    }
    for (const e of entries) {
      const c = canonical(e.keys);
      const first = seen.get(c);
      if (first) {
        HK._conflicts.push({ keys: e.keys, a: first, b: e });
        continue; // 引擎只认第一个
      }
      seen.set(c, e);
      if (e.def.kind === 'keys') HK._index.set(c, { def: e.def, slot: e.slot });
    }
  }
  function renderAllBoxes() {
    HK._boxes = HK._boxes.filter((b) => b.box.isConnected);
    for (const b of HK._boxes) {
      try { b.box._hkRender(); } catch (e) { console.warn('[HK] 重渲染键帽框失败', e); }
    }
  }

  // 组合合法性（keys 型）。注册方可自带 validate(keys)（返回错误文案或 false）；
  // 默认规则：必须含键盘键（引擎只听键盘），且不能只用修饰键（单按 Ctrl/Alt 之类
  // 太容易误触）。语音组件允许单修饰键（点按 Alt 触发是它的默认副键），走自带
  // validate + mouseKeys。
  function validateKeys(def, keys) {
    if (typeof def.validate === 'function') {
      let r = null;
      try { r = def.validate(keys); } catch (e) { r = null; }
      if (typeof r === 'string') return r;
      if (r === false) return '该组合不被允许';
      return null;
    }
    if (keys.some((k) => k.startsWith('mouse-')) && !def.mouseKeys) return '这里不支持鼠标键';
    if (!keys.some((k) => !k.startsWith('mouse-'))) return '需要至少一个键盘键';
    if (!keys.some((k) => !isModKey(k) && !k.startsWith('mouse-'))) return '不能只用修饰键（Ctrl/Alt/Shift/Win）';
    return null;
  }

  /* ---------- 键帽录制框：设置·快捷键 与 语音页共用的同一个控件 ---------- */
  let captureCtl = null; // 当前录制会话（重复点击作废旧会话——侧键在窗外松开收不到
                         // mouseup 曾把录制态永久挂死；自愈 + 12 秒硬超时兜底）
  function keyCapBox(def, slot) {
    const box = document.createElement('button');
    box.type = 'button';
    box.className = 'hk-box';
    box.title = '点击后按下想要的组合键，松开后保存（Esc 取消）';
    const render = () => {
      const keys = HK.effectiveKeys(def, slot);
      const raw = HK.rawSlot(def, slot); // 原始槽值：区分「跟默认」与「已停用」
      box.innerHTML = '';
      box.classList.remove('rec');
      if (!keys.length) {
        const n = document.createElement('span');
        n.className = 'hk-none';
        n.textContent = '无（已停用，点此录制）';
        box.appendChild(n);
        return;
      }
      const combo = document.createElement('span');
      combo.className = 'hk-combo';
      if (raw === null) combo.title = '默认值，点击可录制自定义组合';
      keys.forEach((k, j) => {
        if (j > 0) {
          const p = document.createElement('span');
          p.className = 'hk-plus';
          p.textContent = '+';
          combo.appendChild(p);
        }
        const cap = document.createElement('kbd');
        cap.className = 'hk-key';
        cap.textContent = keyLabel(k);
        combo.appendChild(cap);
      });
      box.appendChild(combo);
      const x = document.createElement('span');
      x.className = 'hk-x';
      x.title = '停用这个快捷键';
      x.textContent = '✕';
      x.onclick = (e) => { e.stopPropagation(); HK.setSlot(def, slot, []); };
      box.appendChild(x);
    };
    box._hkRender = render;
    render();
    box.addEventListener('click', () => startCapture(box, def, slot));
    HK._boxes.push({ box, def, slot });
    return box;
  }

  // 录制流程：按下组合 → 全部松开时落定；Esc / 失焦 / 硬超时取消。got 收「按过」
  // 的键（累积不删），held 收「仍按住」的键（清空即落定）——只用 held 落定时
  // 全松开瞬间恰好清空，录出来永远是空组合，这是曾踩过的根因，别改回去。
  function startCapture(box, def, slot) {
    if (captureCtl) captureCtl.cancel();
    HK._recCount++;
    box.classList.add('rec');
    box.innerHTML = '';
    const tip = document.createElement('span');
    tip.className = 'hk-rec-tip';
    tip.textContent = '请按下组合键，松开后保存（Esc 取消）…';
    box.appendChild(tip);
    const got = new Set();
    const held = new Set();
    let finished = false;
    const ctl = {};
    captureCtl = ctl;
    const stop = () => {
      if (finished) return;
      finished = true;
      HK._recCount--;
      document.removeEventListener('keydown', kd);
      document.removeEventListener('keyup', ku);
      document.removeEventListener('mousedown', md);
      document.removeEventListener('mouseup', mu);
      window.removeEventListener('blur', onBlur);
      clearTimeout(hardTimer);
      if (captureCtl === ctl) captureCtl = null;
      box._hkRender();
    };
    ctl.cancel = stop;
    const finalize = () => {
      if (finished) return;
      const keys = Array.from(got);
      stop();
      if (!keys.length) return;
      const err = validateKeys(def, keys);
      if (err) { toast('不支持这个组合：' + err); return; }
      const owner = HK.comboInUse(keys, { defId: def.id, slot });
      if (owner) { toast('这个组合已被「' + owner.def.name + '」占用，请换一个'); return; }
      HK.setSlot(def, slot, keys);
    };
    const kd = (e) => {
      if (e.key === 'Escape') { stop(); return; }
      const k = normKey(e);
      if (k) {
        if (!e.repeat) { got.add(k); held.add(k); }
        e.preventDefault();
      }
    };
    const ku = (e) => {
      const k = normKey(e);
      if (k) held.delete(k);
      if (held.size === 0) finalize();
    };
    const md = (e) => {
      // 不支持鼠标键的热键：点在框外＝放弃录制（再点框内由 click 重新开始）
      if (!def.mouseKeys) { if (!box.contains(e.target)) stop(); return; }
      const m = normMouse(e);
      if (m) { got.add(m); held.add(m); e.preventDefault(); }
    };
    const mu = (e) => {
      if (!def.mouseKeys) return;
      const m = normMouse(e);
      if (m) held.delete(m);
      if (held.size === 0) setTimeout(finalize, 60); // 键盘松完的收尾也走这里
    };
    const onBlur = () => stop();
    document.addEventListener('keydown', kd);
    document.addEventListener('keyup', ku);
    document.addEventListener('mousedown', md);
    document.addEventListener('mouseup', mu);
    window.addEventListener('blur', onBlur);
    const hardTimer = setTimeout(stop, 12000);
  }

  /* ---------- 选项型快捷键（如发送键）：两枚键帽芯片，点击切换 ---------- */
  function choiceChips(def) {
    const wrap = document.createElement('span');
    wrap.className = 'hk-chips';
    const render = () => {
      wrap.innerHTML = '';
      const cur = HK.effectiveChoice(def);
      for (const c of def.choices || []) {
        const chip = document.createElement('button');
        chip.type = 'button';
        chip.className = 'hk-chip' + (c === cur ? ' cur' : '');
        chip.title = c.tip || '';
        (c.keys || []).forEach((k, j) => {
          if (j > 0) {
            const p = document.createElement('span');
            p.className = 'hk-plus';
            p.textContent = '+';
            chip.appendChild(p);
          }
          const cap = document.createElement('kbd');
          cap.className = 'hk-key';
          cap.textContent = keyLabel(k);
          chip.appendChild(cap);
        });
        if (c.label) {
          const t = document.createElement('span');
          t.className = 'hk-chip-label';
          t.textContent = c.label;
          chip.appendChild(t);
        }
        chip.onclick = () => HK.setChoice(def, c.v);
        wrap.appendChild(chip);
      }
    };
    wrap._hkRender = render;
    render();
    HK._boxes.push({ box: wrap, def, slot: 'choice' });
    return wrap;
  }

  /* ---------- 设置 · 快捷键：总表 ---------- */
  function renderTable(host) {
    host.innerHTML = '';
    const defs = HK.list();
    const rowNo = new Map(defs.map((d, i) => [d, i + 1])); // 行号从 1 起（冲突清单用它指代）
    const head = document.createElement('div');
    head.className = 'hk-head';
    const tip = document.createElement('span');
    tip.className = 'sf-desc';
    tip.textContent = '共 ' + defs.length + ' 个快捷键。点键帽框录制自定义组合（松开保存、Esc 取消），框内 ✕ 停用该槽，行末「清空」主副一起停用。方向键等仅当鼠标最后点在左侧栏或对话区时生效；输入框、右侧栏（浏览器）与弹窗内的按键不受影响。';
    const allBtn = document.createElement('button');
    allBtn.type = 'button';
    allBtn.className = 'hk-restore-all';
    allBtn.textContent = '全部恢复默认';
    allBtn.onclick = () => HK.restoreAll();
    head.appendChild(tip);
    head.appendChild(allBtn);
    host.appendChild(head);

    const scroll = document.createElement('div');
    scroll.className = 'hk-scroll';
    const table = document.createElement('div');
    table.className = 'hk-table';
    const hr = document.createElement('div');
    hr.className = 'hk-row hk-head-row';
    ['快捷键名称', '主快捷键', '副快捷键', '描述', '恢复默认', '清空'].forEach((t) => {
      const c = document.createElement('div');
      c.className = 'hk-cell';
      c.textContent = t;
      hr.appendChild(c);
    });
    table.appendChild(hr);
    // 卷入冲突的行（两侧任一）都变色，让人一眼看到要改谁
    const conflictDefs = new Set();
    for (const cf of HK._conflicts || []) {
      conflictDefs.add(cf.a.def);
      conflictDefs.add(cf.b.def);
    }
    for (const def of defs) table.appendChild(tableRow(def, rowNo.get(def), conflictDefs.has(def)));
    scroll.appendChild(table);
    host.appendChild(scroll);
    // 底部冲突清单：同一组合被多处占用时，按表格顺序在前的生效、靠后的不触发。
    // 一对一对地写行号+名称，用户直接知道改哪一个（改靠后的那项，或清空它）。
    if ((HK._conflicts || []).length) {
      const list = document.createElement('div');
      list.className = 'hk-conf-list';
      const h = document.createElement('div');
      h.className = 'hk-conf-title';
      h.textContent = '冲突 ' + HK._conflicts.length + ' 处';
      list.appendChild(h);
      for (const cf of HK._conflicts) {
        const item = document.createElement('div');
        item.className = 'hk-conf-item';
        const a = '第' + rowNo.get(cf.a.def) + '项「' + cf.a.def.name + '」的' + SLOT_LABEL[cf.a.slot];
        const b = '第' + rowNo.get(cf.b.def) + '项「' + cf.b.def.name + '」的' + SLOT_LABEL[cf.b.slot];
        item.textContent = a + ' 与 ' + b + ' 都是 ' + comboText(cf.keys) + '——' + a + '生效、' + b + '不触发，建议改掉靠后的';
        list.appendChild(item);
      }
      host.appendChild(list);
    }
  }

  function tableRow(def, no, conflict) {
    const row = document.createElement('div');
    row.className = 'hk-row' + (conflict ? ' conflict' : '');
    row.title = conflict ? '这项的快捷键与别处冲突（见下方冲突清单）' : '';
    // 序号 + 名称 + 来源徽标
    const c0 = document.createElement('div');
    c0.className = 'hk-cell hk-c0';
    const noEl = document.createElement('span');
    noEl.className = 'hk-no';
    noEl.textContent = no + '.';
    c0.appendChild(noEl);
    const nm = document.createElement('b');
    nm.textContent = def.name;
    c0.appendChild(nm);
    if (def.source) {
      const s = document.createElement('span');
      s.className = 'hk-src';
      s.textContent = def.source;
      s.title = '登记来源：' + def.source + (def.source === '界面' || def.source === '常规' ? '（程序内置）' : '（组件）');
      c0.appendChild(s);
    }
    // 主 / 副
    const c1 = document.createElement('div');
    c1.className = 'hk-cell hk-c1';
    const c2 = document.createElement('div');
    c2.className = 'hk-cell hk-c2';
    if (def.kind === 'choice') {
      c1.appendChild(choiceChips(def));
      const dash = document.createElement('span');
      dash.className = 'hk-dash';
      dash.title = '这项只有固定几档可选，没有副快捷键';
      dash.textContent = '—';
      c2.appendChild(dash);
    } else {
      c1.appendChild(keyCapBox(def, 'main'));
      c2.appendChild(keyCapBox(def, 'sub'));
    }
    // 描述
    const c3 = document.createElement('div');
    c3.className = 'hk-cell hk-c3';
    c3.textContent = def.desc || '';
    // 恢复默认
    const c4 = document.createElement('div');
    c4.className = 'hk-cell hk-c4';
    const rb = document.createElement('button');
    rb.type = 'button';
    rb.className = 'hk-restore';
    rb.textContent = '恢复默认';
    rb.title = def.kind === 'choice'
      ? '回到 ' + comboText((def.choices[0] || {}).keys)
      : '主：' + (def.defaults.main && def.defaults.main.length ? comboText(def.defaults.main) : '无')
        + '，副：' + (def.defaults.sub && def.defaults.sub.length ? comboText(def.defaults.sub) : '无');
    rb.onclick = () => HK.restoreDef(def);
    c4.appendChild(rb);
    // 清空（主副一起停用；选项型没有可清的键位）
    const c5 = document.createElement('div');
    c5.className = 'hk-cell hk-c5';
    if (def.kind === 'choice') {
      const dash = document.createElement('span');
      dash.className = 'hk-dash';
      dash.textContent = '—';
      c5.appendChild(dash);
    } else {
      const cb = document.createElement('button');
      cb.type = 'button';
      cb.className = 'hk-clr';
      cb.textContent = '清空';
      cb.title = '主、副快捷键一起停用（只想停一个用键帽框里的 ✕）';
      cb.onclick = () => HK.clearDef(def);
      c5.appendChild(cb);
    }
    row.append(c0, c1, c2, c3, c4, c5);
    return row;
  }

  /* ---------- 生效范围：鼠标最后一次点击落在哪个区 ----------
     只有左侧栏与对话区（以及「从未点击」）吃这些快捷键；右侧栏（浏览器/子对话）、
     弹窗、弹层一旦被点过，按键就还给它们。键盘焦点再兜一道底：Tab 也能把焦点
     挪进输入框/右侧栏，区域没变但按键同样不该抢。 */
  let lastRegion = 'none';
  document.addEventListener('mousedown', (e) => {
    const t = e.target;
    if (!(t instanceof Element)) return;
    if (t.closest('#rpanel')) lastRegion = 'right';
    else if (t.closest('.modal')) lastRegion = 'modal';
    else if (t.closest('#sidebar')) lastRegion = 'left';
    else if (t.closest('#main')) lastRegion = 'chat';
    else lastRegion = 'other'; // 弹层 popover 等
  }, true);

  function navKeysActive() {
    // 任何弹窗（设置/向导/确认框等）开着：按键归还弹窗——不只看鼠标区域，
    // JS 直开弹窗（openSettings 等）没有对应 mousedown，只盯 region 会漏
    if (document.querySelector('.modal:not(.hidden)')) return false;
    const ae = document.activeElement;
    if (ae instanceof Element) {
      if (ae.closest('#rpanel') || ae.closest('.modal')) return false;
      const tag = ae.tagName;
      if (tag === 'INPUT' || tag === 'TEXTAREA' || tag === 'SELECT' || ae.isContentEditable) return false;
    }
    if (typeof marketOpen === 'function' && marketOpen()) return false; // 市场占领主区
    if (typeof sideSearchOpen === 'function' && sideSearchOpen()) return false; // 侧栏搜索进行中
    return lastRegion === 'left' || lastRegion === 'chat' || lastRegion === 'none';
  }

  /* ---------- ↑↓：按「此刻」左侧列表切对话 ----------
     列表是活的（新建 / 归档 / 分组折叠 / 回收视图都会变），DOM 就是唯一真相——
     按键瞬间读 #session-list 里可见的 .session-item 顺序，找当前项上下移动；
     到头不环绕。当前对话不在列表里（如长期助理对话）时：向下进第一项、向上不动。 */
  function switchConversation(dir) {
    const box = $('session-list');
    if (!box) return;
    const items = Array.from(box.querySelectorAll('.session-item')).filter((el) => el.dataset.sid);
    if (!items.length) return;
    const idx = items.findIndex((el) => el.classList.contains('active'));
    const next = idx < 0 ? (dir > 0 ? 0 : -1) : idx + dir;
    if (next < 0 || next >= items.length) return;
    openSession(items[next].dataset.sid);
  }

  /* ---------- 引擎：keydown 精确匹配（修饰键须完整按下，单 ↑ 不会被 Ctrl+↑ 误触） ---------- */
  document.addEventListener('keydown', (e) => {
    if (HK._recCount > 0 || e.isComposing) return;
    if (!navKeysActive()) return;
    const k = normKey(e);
    if (!k) return;
    const combo = canonical([
      ...(e.ctrlKey ? ['ctrl'] : []),
      ...(e.altKey ? ['alt'] : []),
      ...(e.shiftKey ? ['shift'] : []),
      ...(e.metaKey ? ['meta'] : []),
      k,
    ]);
    const owner = HK._index.get(combo);
    if (!owner || owner.def.kind !== 'keys' || !owner.def.action) return;
    e.preventDefault();
    if (e.repeat) return; // 按住不放不连发
    try { owner.def.action(); } catch (err) { console.warn('[HK] 执行', owner.def.id, err); }
  });

  /* ---------- 界面内置快捷键（四个方向键导航 + 发送键登记） ---------- */
  HK.register({
    id: 'nav.prev', name: '上一个对话', source: '界面',
    desc: '按左侧列表此刻的顺序切到上一个对话，顶部不环绕；鼠标点在左侧栏或对话区时生效',
    defaults: { main: ['arrowup'], sub: [] },
    get: (slot) => builtinGet('nav.prev', slot),
    set: (slot, v) => builtinSet('nav.prev', slot, v),
    action: () => switchConversation(-1),
  });
  HK.register({
    id: 'nav.next', name: '下一个对话', source: '界面',
    desc: '按左侧列表此刻的顺序切到下一个对话，底部不环绕；鼠标点在左侧栏或对话区时生效',
    defaults: { main: ['arrowdown'], sub: [] },
    get: (slot) => builtinGet('nav.next', slot),
    set: (slot, v) => builtinSet('nav.next', slot, v),
    action: () => switchConversation(1),
  });
  HK.register({
    id: 'ui.toggleLeft', name: '展开/收起左侧栏', source: '界面',
    desc: '与顶栏「收起左侧栏」按钮同一开关',
    defaults: { main: ['arrowleft'], sub: [] },
    get: (slot) => builtinGet('ui.toggleLeft', slot),
    set: (slot, v) => builtinSet('ui.toggleLeft', slot, v),
    action: () => { if (typeof toggleSidebar === 'function') toggleSidebar(); },
  });
  HK.register({
    id: 'ui.toggleRight', name: '展开/收起右侧栏', source: '界面',
    desc: '与顶栏「展开右侧栏」按钮同一开关（展开回到上次看的视图）',
    defaults: { main: ['arrowright'], sub: [] },
    get: (slot) => builtinGet('ui.toggleRight', slot),
    set: (slot, v) => builtinSet('ui.toggleRight', slot, v),
    action: () => { if (typeof toggleRpanel === 'function') toggleRpanel(); },
  });
  // 发送键是「选项型」：两档互斥，不能录制任意组合（Enter 要留给换行/IME）。
  // 存储仍是 jx.sendkey，composerEnterKeydown 与占位提示链路一概不动。
  HK.register({
    id: 'chat.send', name: '发送消息', source: '常规',
    desc: '输入框里发送：Enter 档（Shift/Ctrl+Enter 换行）或 Ctrl+Enter 档（Enter 换行）；常规页同步可改',
    kind: 'choice',
    choices: [
      { v: 'enter', keys: ['enter'], tip: 'Enter 发送，Shift+Enter / Ctrl+Enter 换行' },
      { v: 'ctrl', keys: ['ctrl', 'enter'], tip: 'Ctrl+Enter 发送，Enter / Shift+Enter 换行' },
    ],
    getChoice: () => (lsGet('jx.sendkey', 'enter') === 'ctrl' ? 'ctrl' : 'enter'),
    setChoice: (v) => {
      lsSet('jx.sendkey', v);
      if (typeof updateSendKeyHint === 'function') updateSendKeyHint();
    },
  });
})();
