// 项目登记是实例级用户管理：会话浮窗只引导，不发起任何项目读写请求。
// 登记本身不在本页重做：挂宿主同一份 JX.projectRegistry 视图（与项目事件页共用），
// 本页只管项目知识正文与预算。
// 草稿仅留在当前页面内存（含重挂/切换），不是知识历史或持久备份。
(function () {
  'use strict';
  var drafts = new Map();
  var hosts = new Set();
  var disposed = false;

  async function api(path, method, body) {
    var res = await fetch('/api/projects' + path, {
      method: method || 'GET',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store'
    });
    var text = await res.text(), data;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* 下方统一报错 */ }
    if (!res.ok) {
      var err = new Error(data && (data.error || data.message) ? String(data.error || data.message) : 'HTTP ' + res.status + (text ? '：' + text.slice(0, 300) : ''));
      err.status = res.status;
      throw err;
    }
    if (data === undefined || data === null) throw new Error('服务器未返回有效 JSON，请稍后重试。');
    return data;
  }
  // 项目清单复用宿主登记视图的同一份客户端（JX.projectRegistry.list），
  // 本页不再自带一套项目读取；未挂登记视图时退回同一 /api/projects 服务。
  function listProjects() {
    if (window.JX && window.JX.projectRegistry && typeof window.JX.projectRegistry.list === 'function') {
      return window.JX.projectRegistry.list();
    }
    return api('');
  }
  function node(tag, parent, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (cls) e.className = cls;
    if (parent) parent.appendChild(e);
    return e;
  }
  function button(parent, text, action) {
    var b = node('button', parent, text, 'mc-btn'); b.type = 'button'; b.onclick = action; return b;
  }
  function input(parent, label, type) {
    var box = node('label', parent, undefined, 'set-field');
    node('span', box, label, 'sf-label');
    var e = node(type === 'textarea' ? 'textarea' : 'input', box, undefined, 'pf-input');
    if (type !== 'textarea') e.type = type || 'text';
    return e;
  }
  // 与工作目录面板 renderWsPop 使用相同的线框 SVG 和 pop-item（15px / 9px 间距）。
  function icon(parent, remote) {
    var svg = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    [['viewBox', '0 0 24 24'], ['fill', 'none'], ['stroke', 'currentColor'], ['stroke-width', '2'], ['stroke-linejoin', 'round'], ['aria-hidden', 'true']].forEach(function (a) { svg.setAttribute(a[0], a[1]); });
    var path = document.createElementNS('http://www.w3.org/2000/svg', 'path');
    if (remote) {
      [4, 14].forEach(function (y) {
        var rect = document.createElementNS('http://www.w3.org/2000/svg', 'rect');
        [['x', '3'], ['y', String(y)], ['width', '18'], ['height', '7'], ['rx', '2']].forEach(function (a) { rect.setAttribute(a[0], a[1]); }); svg.appendChild(rect);
      });
      path.setAttribute('d', 'M7 8h.01M11 8h.01M7 18h.01M11 18h.01');
    } else path.setAttribute('d', 'M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z');
    svg.appendChild(path); parent.appendChild(svg);
  }
  function picker(parent, label) {
    var box = node('div', parent); box.style.cssText = 'min-width:0;max-width:100%';
    var trigger = button(box, '', function () { list.hidden = !list.hidden; trigger.setAttribute('aria-expanded', String(!list.hidden)); });
    trigger.className = 'pop-item pf-input'; trigger.setAttribute('aria-label', label); trigger.setAttribute('aria-expanded', 'false');
    var list = node('div', box); list.hidden = true;
    list.style.cssText = 'max-height:240px;overflow:auto;border:1px solid var(--border);border-radius:8px';
    var value = '', options = [], control = { onchange: null };
    // 触发器文案只由 value 决定：没有匹配项就退回 label 占位，占位文案不进选项列表——
    // 否则「请选择项目」会被当成一个可点条目再渲染一遍，看起来像第二个同名按钮。
    function caption() {
      trigger.replaceChildren(); var o = options.find(function (x) { return x.value === value; });
      icon(trigger, o && o.remote); var text = node('span', trigger, o ? o.label : label);
      text.style.cssText = 'min-width:0;overflow-wrap:anywhere'; node('span', trigger, '⌄', 'pop-check');
    }
    Object.defineProperty(control, 'value', { get: function () { return value; }, set: function (v) { value = v; caption(); } });
    control.setOptions = function (items, emptyText) {
      options = items; list.replaceChildren();
      if (!items.length && emptyText) { // 空面板给空状态提示，不留一块空白也不重复触发器
        var hint = node('div', list, emptyText);
        hint.style.cssText = 'padding:8px 10px;color:var(--text-3);font-size:12px;line-height:1.5;white-space:normal';
        hint.setAttribute('role', 'status');
      }
      items.forEach(function (o) {
        var b = button(list, '', function () { control.value = o.value; list.hidden = true; trigger.setAttribute('aria-expanded', 'false'); if (control.onchange) control.onchange(); });
        b.className = 'pop-item'; icon(b, o.remote);
        node('span', b, o.label).style.cssText = 'min-width:0;overflow-wrap:anywhere';
      }); caption();
    };
    box.onkeydown = function (e) { if (e.key === 'Escape') { list.hidden = true; trigger.setAttribute('aria-expanded', 'false'); trigger.focus(); } };
    return control;
  }
  function time(value) {
    if (!value) return '尚未更新';
    var d = new Date(value);
    return isNaN(d.getTime()) ? String(value) : d.toLocaleString(undefined, { timeZoneName: 'short' });
  }
  function dirty(d) {
    // 只比知识与预算：名称/绑定归宿主登记视图管，不在本页草稿里。
    return d && (d.content !== d.base.content || String(d.budget) !== String(d.base.budget_limit));
  }
  function mount(el) {
    el.replaceChildren();
    if (el.closest('#cxsession') || !el.closest('#settings')) {
      node('p', el, '项目知识库属于全局项目登记。请到「设置 → 项目知识库」管理；本对话浮窗不提供项目管理。', 'sf-desc');
      return;
    }
    var host = { stop: function () { generation++; previewSeq++; clearTimeout(timer); } };
    hosts.forEach(function (h) { if (!h.el.isConnected) { h.stop(); hosts.delete(h); } });
    host.el = el; hosts.add(host);
    var generation = 0, previewSeq = 0, timer, busy = false, loading = false;
    var projects = [], current = null, preview = null;
    function alive() { return !disposed && el.isConnected; }
    node('p', el, '短而可靠的项目入场须知：只记录背景和当前有效约束，不是开发日志。秘密和凭据请勿写入。', 'sf-desc');
    node('p', el, '只保留当前正文，不保留历史。保存覆盖后无法由组件恢复；revision 仅用于并发校验。Markdown 导出可作为手动备份。所有改动均使用本页按钮保存，底部全局「保存」不会保存本页草稿。', 'sf-desc');
    var status = node('p', el, '', 'sf-desc'); status.setAttribute('role', 'status'); status.setAttribute('aria-live', 'polite');
    function message(text, error) { if (!alive()) return; status.textContent = text; status.style.color = error ? 'var(--danger)' : ''; }
    var top = node('div', el); top.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
    var select = picker(top, '请选择项目');
    button(top, '刷新项目列表', function () { refresh(); });
    // 项目登记不在本页重做：挂宿主同一份 JX.projectRegistry 视图（与项目事件页共用），
    // 名称/绑定/归档/删除与路径规范化都走同一条服务端路径，不再各写一套再漂移。
    var registryBox = node('div', el);
    node('p', registryBox, '项目身份与工作区绑定是全实例唯一的一套：本页直接用宿主登记视图（与项目事件页同一份），不另建第二套登记。', 'sf-desc');
    var registryHost = node('div', registryBox);
    registryHost.hidden = true;
    var registryMounted = false;
    var registryToggle = button(registryBox, '项目登记 ▸ 展开', function () {
      registryHost.hidden = !registryHost.hidden;
      registryToggle.textContent = registryHost.hidden ? '项目登记 ▸ 展开' : '项目登记 ▾ 收起';
      if (registryHost.hidden || registryMounted) return;
      if (!(window.JX && window.JX.projectRegistry)) {
        registryHost.replaceChildren();
        node('p', registryHost, '宿主未提供项目登记视图，请确认程序为当前版本；本页不另建登记表。', 'sf-desc');
        return;
      }
      window.JX.projectRegistry.mount(registryHost, {
        onChange: function (list, id) {
          projects = list || [];
          refresh(id || '');
        }
      });
      registryMounted = true;
    });
    var editor = node('div', el);

    function leave() {
      return !dirty(current) || window.confirm('当前项目有未保存改动。是否离开？草稿暂留本页面内存，返回该项目可继续编辑；关闭页面会丢失。');
    }
    function lock() {
      el.querySelectorAll('input,textarea,select,button').forEach(function (e) {
        e.disabled = busy || loading || (e.closest('[data-pk-readonly]') !== null);
      });
    }
    async function perform(fn) {
      if (busy) return;
      busy = true; lock();
      try { await fn(); } catch (e) { message('操作失败：' + e.message + '。未保存草稿仍保留。', true); }
      finally { busy = false; if (alive()) lock(); }
    }
    function renderOptions(id) {
      select.setOptions(projects.map(function (p) { return { value: p.id, label: p.name + (p.archived ? '（已归档）' : '') + ' · ' + p.id }; }), '还没有项目：展开上方「项目登记」新建并绑定工作区（与项目事件页同一份登记）。');
      select.value = id || '';
    }
    async function refresh(id) {
      var seq = ++generation; loading = true; lock();
      try {
        var list = await listProjects();
        if (!alive() || seq !== generation) return;
        if (!Array.isArray(list)) throw new Error('项目列表格式不正确');
        projects = list;
        var chosen = id || (current && current.project.id) || '';
        renderOptions(chosen);
        if (id) await choose(id);
        else if (current && !projects.some(function (p) { return p.id === chosen; })) {
          message('当前项目已被删除或不可访问。草稿仍保留在内存，可先导出。', true);
        } else if (current) {
          var latest = projects.find(function (p) { return p.id === chosen; });
          // 已有正文草稿不能被列表刷新覆盖；归档状态以最新列表为准。
          current.project.archived = latest.archived;
          renderEditor();
        }
      } catch (e) { message('读取项目列表失败：' + e.message, true); }
      finally { if (seq === generation || id) { loading = false; if (alive()) lock(); } }
    }
    select.onchange = function () {
      var id = select.value;
      if (!leave()) { select.value = current ? current.project.id : ''; return; }
      choose(id);
    };
    async function choose(id) {
      var seq = ++generation; ++previewSeq; clearTimeout(timer); preview = null;
      if (!id) { current = null; editor.replaceChildren(); loading = false; lock(); return; }
      loading = true; lock(); message('读取项目知识…');
      try {
        var k = await api('/' + encodeURIComponent(id) + '/knowledge');
        if (!alive() || seq !== generation) return;
        var p = projects.find(function (x) { return x.id === id; });
        if (!p) throw new Error('项目已不在列表中，请刷新');
        var d = drafts.get(id);
        if (!d || !dirty(d)) {
          d = { project: p, base: k, content: k.content || '', budget: String(k.budget_limit), conflict: null };
          drafts.set(id, d);
        } else {
          d.project.archived = p.archived;
          if (d.base.revision !== k.revision) d.conflict = k;
        }
        current = d; renderEditor(); message(d.conflict ? '服务器版本已变化：草稿已保留，请查看最新正文并手工合并。' : '已读取。');
      } catch (e) {
        if (seq === generation) { select.value = current ? current.project.id : ''; message('读取失败：' + e.message + '。原草稿不变。', true); }
      } finally { if (seq === generation) { loading = false; if (alive()) lock(); } }
    }
    function limit(d) {
      var n = Number(d.budget);
      if (!/^\d+$/.test(String(d.budget)) || !Number.isSafeInteger(n) || n <= 0) throw new Error('预算上限必须是合法正整数');
      return n;
    }
    function knowledgePath(d) { return '/' + encodeURIComponent(d.project.id) + '/knowledge'; }
    function schedulePreview(d, line) {
      var seq = ++previewSeq; preview = null; clearTimeout(timer);
      line.textContent = '预算计算中…（以后端为准）';
      timer = setTimeout(async function () {
        try {
          var n = limit(d), content = d.content;
          var r = await api('/budget', 'POST', { content: content, budget_limit: n });
          if (!alive() || current !== d || seq !== previewSeq) return;
          preview = r;
          var used = Number(r.used);
          line.textContent = '预算用量：' + used + ' / ' + n + ' 单元（' + (used / n * 100).toFixed(1) + '%）' + (r.warning ? ' · ' + r.warning : '') + (used > n ? ' · 超限：请精简正文或调整额度，禁止截断保存。' : used === n ? ' · 已达 100%，请立即重构知识库。' : used * 5 >= n * 4 ? ' · 已达 80%，请删除或压缩过期约束。' : '');
          line.style.color = used * 5 >= n * 4 ? 'var(--danger)' : '';
        } catch (e) { if (alive() && current === d && seq === previewSeq) { line.textContent = '预算预览失败：' + e.message + '；保存时仍由服务端权威校验。'; line.style.color = 'var(--danger)'; } }
      }, 250);
    }
    function renderEditor() {
      editor.replaceChildren(); if (!current) return;
      var d = current, p = d.project;
      node('h4', editor, p.name + (p.archived ? '（已归档 · 只读）' : ''));
      node('p', editor, '项目 ID：' + p.id + ' · 创建：' + time(p.created_at) + ' · 项目更新：' + time(p.updated_at), 'sf-desc');
      if (p.archived) node('p', editor, '项目已归档（只读）：名称、工作区绑定、归档与删除请在上方「项目登记」里由用户处理；恢复后可继续维护知识正文。', 'sf-desc');
      node('p', editor, '知识 revision：' + d.base.revision + ' · 更新时间：' + time(d.base.updated_at) + (dirty(d) ? ' · 有未保存草稿' : ''), 'sf-desc');
      var contentBox = node('div', editor);
      if (p.archived) contentBox.setAttribute('data-pk-readonly', '');
      var budget = input(contentBox, '预算上限（正整数，默认 4000；80% 预警，100% 强提醒，超限拒绝）', 'number'); budget.min = '1'; budget.step = '1'; budget.value = d.budget;
      var content = input(contentBox, '知识正文（Markdown）', 'textarea'); content.rows = 18; content.style.cssText = 'min-height:260px;resize:vertical;line-height:1.6;font-family:inherit'; content.value = d.content;
      var usage = node('p', editor, '', 'sf-desc'); usage.setAttribute('aria-live', 'polite');
      content.oninput = function () { d.content = content.value; schedulePreview(d, usage); };
      budget.oninput = function () { d.budget = budget.value; schedulePreview(d, usage); };
      var save = button(contentBox, '保存知识正文与预算', function () {
        if (d.conflict) return message('存在版本冲突，请先查看最新版本并完成合并确认。', true);
        if (!window.confirm('保存将覆盖当前知识，不保留历史正文，组件无法恢复旧内容。确认保存？')) return;
        perform(async function () {
          var n = limit(d);
          try {
            var k = await api(knowledgePath(d), 'PUT', { content: d.content, budget_limit: n, expected_revision: d.base.revision });
            d.base = k; d.content = k.content; d.budget = String(k.budget_limit); d.conflict = null;
            renderEditor(); message('知识已保存 · revision ' + k.revision + (k.warning ? ' · ' + k.warning : ''));
          } catch (e) {
            if (e.status === 409) {
              try { d.conflict = await api(knowledgePath(d)); } catch (_) { d.conflict = { unavailable: true }; }
              renderEditor(); message('版本冲突，未覆盖服务器，也未丢弃草稿。请获取最新正文并手工合并后再保存。', true);
            } else throw e;
          }
        });
      });
      save.style.marginRight = '8px';
      button(editor, '取消本项目未保存改动', function () {
        if (!window.confirm('丢弃本项目的未保存草稿，并重新读取服务器当前内容？')) return;
        perform(async function () {
          // 成功读到完整快照后才丢草稿，网络失败不吞掉用户输入。
          var fresh = await api(knowledgePath(d));
          var list = await listProjects();
          var project = list.find(function (x) { return x.id === p.id; });
          if (!project) throw new Error('项目已删除，请先导出草稿');
          drafts.delete(p.id); projects = list;
          current = { project: project, base: fresh, content: fresh.content || '', budget: String(fresh.budget_limit), conflict: null };
          drafts.set(p.id, current); renderOptions(p.id); renderEditor(); message('已取消未保存改动，并读取最新内容。');
        });
      });
      var file = node('input', contentBox); file.type = 'file'; file.accept = '.md,.markdown,text/markdown,text/plain'; file.style.display = 'none';
      button(contentBox, '导入 Markdown 到草稿', function () { file.click(); });
      file.onchange = function () {
        var chosen = file.files && file.files[0]; file.value = ''; if (!chosen) return;
        perform(async function () {
          var text = await chosen.text();
          var n = limit(d), result = await api('/budget', 'POST', { content: text, budget_limit: n });
          if (Number(result.used) > n) throw new Error('导入正文超限：' + result.used + ' / ' + n + '，请先精简文件或调整额度；未截断，原草稿保持不变');
          if (!window.confirm('将用导入文件替换当前正文草稿（尚不会保存到服务器），是否继续？')) return;
          d.content = text; renderEditor(); message('Markdown 已导入草稿，请检查并点击本页保存。');
        });
      };
      button(editor, '导出当前正文草稿 .md', function () {
        var blob = new Blob([d.content], { type: 'text/markdown;charset=utf-8' });
        var url = URL.createObjectURL(blob), a = node('a', document.body);
        a.href = url; a.download = (p.name || p.id).replace(/[<>:"/\\|?*\x00-\x1f]/g, '_').slice(0, 100) + '.md'; a.click(); a.remove();
        setTimeout(function () { URL.revokeObjectURL(url); }, 1000);
        message('已导出当前正文草稿（不含项目绑定、额度或来源会话）。');
      });
      if (d.conflict) {
        var conflict = node('div', editor, undefined, 'gen-section');
        node('h4', conflict, '版本冲突：当前编辑框仍是你的完整草稿');
        if (!d.conflict.unavailable) {
          node('p', conflict, '服务器最新 revision：' + d.conflict.revision + ' · ' + time(d.conflict.updated_at) + ' · 预算上限：' + d.conflict.budget_limit, 'sf-desc');
          var pre = node('pre', conflict, d.conflict.content || '（空正文）'); pre.style.cssText = 'white-space:pre-wrap;overflow-wrap:anywhere;max-height:360px;overflow:auto;padding:12px;border:1px solid var(--border)';
          button(conflict, '已手工合并：使用此最新 revision 再保存', function () {
            if (!window.confirm('请确认已将上方最新正文与草稿手工合并，并核对预算上限。继续仅更新并发基线，不自动合并、不立即保存。')) return;
            d.base = d.conflict; d.conflict = null; renderEditor(); message('已更新并发基线，草稿保持不变；请检查后保存。');
          });
        }
        button(conflict, '重新获取最新版本（保留草稿）', function () {
          perform(async function () { d.conflict = await api(knowledgePath(d)); renderEditor(); message('最新正文已显示，草稿未修改。'); });
        });
      }
      schedulePreview(d, usage); lock();
    }
    refresh();
    // 浏览器离开前仍提示；不使用 localStorage 保存正文，避免形成隐性历史库。
    host.beforeUnload = function (e) { if (Array.from(drafts.values()).some(dirty)) { e.preventDefault(); e.returnValue = ''; } };
    window.addEventListener('beforeunload', host.beforeUnload);
    var stop = host.stop;
    host.stop = function () { stop(); window.removeEventListener('beforeunload', host.beforeUnload); };
  }
  CX.register({
    id: 'project-knowledge',
    init: function () {
      disposed = false;
      CX.registerSettings({
        id: 'project-knowledge', title: '项目知识库',
        // 图标规格照抄设置页静态导航项（.set-nav-item svg：16px、取 currentColor）：
        // 同一 viewBox / stroke-width / 圆角端点，纯描边线框，无填充无底色。
        pane: {
          label: '项目知识库',
          icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M12 4.6A3 3 0 0 0 7.4 3.4 3 3 0 0 0 4.2 7.7 3 3 0 0 0 3.3 13.4 3 3 0 0 0 6.5 18.5 3 3 0 0 0 12 19.5Z"/><path d="M12 4.6A3 3 0 0 1 16.6 3.4 3 3 0 0 1 19.8 7.7 3 3 0 0 1 20.7 13.4 3 3 0 0 1 17.5 18.5 3 3 0 0 1 12 19.5Z"/><path d="M8.4 9.6h1.5M8.4 13.4h1.5M14.1 9.6h1.5M14.1 13.4h1.5"/></svg>'
        },
        mount: mount
        // 本页项目按钮保存；不接全局 save，防止结构管理与正文重复提交。
      });
    },
    dispose: function () {
      disposed = true;
      hosts.forEach(function (h) { h.stop(); }); hosts.clear();
      CX.unregisterSettings('project-knowledge');
    }
  });
})();
