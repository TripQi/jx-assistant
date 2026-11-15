// 项目登记是实例级用户管理：会话浮窗只引导，不发起任何项目读写请求。
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
  function time(value) {
    if (!value) return '尚未更新';
    var d = new Date(value);
    return isNaN(d.getTime()) ? String(value) : d.toLocaleString(undefined, { timeZoneName: 'short' });
  }
  function bindings(project) {
    return (project.workspaces || []).map(function (w) { return { workspace_kind: w.workspace_kind, display_path: w.display_path }; });
  }
  function dirty(d) {
    return d && (d.content !== d.base.content || String(d.budget) !== String(d.base.budget_limit) ||
      d.name !== d.project.name || JSON.stringify(d.workspaces) !== JSON.stringify(bindings(d.project)));
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
    var projects = [], current = null, preview = null, resolving = 0;
    function alive() { return !disposed && el.isConnected; }
    node('p', el, '短而可靠的项目入场须知：只记录背景和当前有效约束，不是开发日志。秘密和凭据请勿写入。', 'sf-desc');
    node('p', el, '只保留当前正文，不保留历史。保存覆盖后无法由组件恢复；revision 仅用于并发校验。Markdown 导出可作为手动备份。所有改动均使用本页按钮保存，底部全局「保存」不会保存本页草稿。', 'sf-desc');
    var status = node('p', el, '', 'sf-desc'); status.setAttribute('role', 'status'); status.setAttribute('aria-live', 'polite');
    function message(text, error) { if (!alive()) return; status.textContent = text; status.style.color = error ? 'var(--danger)' : ''; }
    var top = node('div', el); top.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
    var select = node('select', top, undefined, 'pf-input'); select.style.width = 'auto'; select.setAttribute('aria-label', '选择项目（含已归档）');
    button(top, '刷新项目列表', function () { refresh(); });
    var createName = input(el, '新项目名称');
    button(el, '新建项目', function () {
      var name = createName.value.trim();
      if (!name) return message('请输入项目名称。', true);
      if (!leave()) return;
      perform(async function () {
        var p = await api('', 'POST', { name: name });
        createName.value = '';
        await refresh(p.id);
        message('项目已新建。');
      });
    });
    var editor = node('div', el);
    var resolveBox = node('div', el, undefined, 'gen-section');
    node('h4', resolveBox, '检查工作区实际归属');
    var resolveKind = node('select', resolveBox, undefined, 'pf-input');
    [['local', '本地'], ['remote', '远程']].forEach(function (v) { var o = node('option', resolveKind, v[1]); o.value = v[0]; });
    var resolvePath = input(resolveBox, '绝对路径（远程：user@host:dir 或 user@host:port:dir）');
    var resolveResult = node('p', resolveBox, '', 'sf-desc');
    button(resolveBox, '解析归属', async function () {
      var path = resolvePath.value.trim(), kind = resolveKind.value, seq = ++resolving;
      if (!path) { resolveResult.textContent = '请输入工作区路径。'; return; }
      resolveResult.textContent = '解析中…';
      try {
        var result = await api('/resolve', 'POST', { workspace_kind: kind, display_path: path });
        if (!alive() || seq !== resolving || path !== resolvePath.value.trim() || kind !== resolveKind.value) return;
        resolveResult.textContent = result.project ? '实际归属：' + result.project.name + ' / ' + result.project.id + (result.project.archived ? '（已归档，不回退到外层项目）' : '（使用中）') : '未登记：不会回退到其他项目。';
      } catch (e) { if (alive() && seq === resolving) resolveResult.textContent = '解析失败：' + e.message; }
    });
    node('p', resolveBox, '嵌套绑定按最长目录匹配；本地与远程分别登记。更具体的归档绑定不会回退。目录搬迁只需改绑定；重绑不迁移旧事件。', 'sf-desc');

    function leave() {
      return !dirty(current) || window.confirm('当前项目有未保存改动。是否离开？草稿暂留本页面内存，返回该项目可继续编辑；关闭页面会丢失。');
    }
    function lock() {
      el.querySelectorAll('input,textarea,select,button').forEach(function (e) {
        e.disabled = busy || loading || (e.closest('[data-pk-readonly]') !== null);
      });
      select.disabled = busy || loading;
    }
    async function perform(fn) {
      if (busy) return;
      busy = true; lock();
      try { await fn(); } catch (e) { message('操作失败：' + e.message + '。未保存草稿仍保留。', true); }
      finally { busy = false; if (alive()) lock(); }
    }
    function renderOptions(id) {
      select.replaceChildren();
      var empty = node('option', select, '请选择项目'); empty.value = '';
      projects.forEach(function (p) { var o = node('option', select, p.name + (p.archived ? '（已归档）' : '') + ' · ' + p.id); o.value = p.id; });
      select.value = id || '';
    }
    async function refresh(id) {
      var seq = ++generation; loading = true; lock();
      try {
        var list = await api('');
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
          d = { project: p, base: k, name: p.name, workspaces: bindings(p), content: k.content || '', budget: String(k.budget_limit), conflict: null };
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
      var manage = node('div', editor);
      if (p.archived) manage.setAttribute('data-pk-readonly', '');
      var name = input(manage, '项目名称'); name.value = d.name; name.oninput = function () { d.name = name.value; };
      node('p', manage, '绑定路径（必须是绝对路径；远程使用 user@host:dir 或 user@host:port:dir）。删除一行仅解除绑定，不删除目录。', 'sf-desc');
      var rows = node('div', manage);
      function drawRows() {
        rows.replaceChildren();
        d.workspaces.forEach(function (w, i) {
          var row = node('div', rows); row.style.cssText = 'display:flex;gap:6px;margin-bottom:6px;flex-wrap:wrap';
          var kind = node('select', row, undefined, 'pf-input'); kind.style.width = 'auto'; kind.setAttribute('aria-label', '绑定类型');
          [['local', '本地'], ['remote', '远程']].forEach(function (v) { var o = node('option', kind, v[1]); o.value = v[0]; });
          kind.value = w.workspace_kind; kind.onchange = function () { w.workspace_kind = kind.value; };
          var path = node('input', row, undefined, 'pf-input'); path.style.cssText = 'flex:1;min-width:180px'; path.value = w.display_path; path.setAttribute('aria-label', '绑定绝对路径'); path.oninput = function () { w.display_path = path.value; };
          button(row, '解除此绑定', function () { d.workspaces.splice(i, 1); drawRows(); lock(); });
        });
      }
      drawRows();
      button(manage, '添加绑定', function () { d.workspaces.push({ workspace_kind: 'local', display_path: '' }); drawRows(); lock(); });
      button(manage, '保存名称与绑定', function () {
        perform(async function () {
          var newName = d.name.trim(); if (!newName) throw new Error('项目名称不能为空');
          var ws = d.workspaces.map(function (w) { if (!w.display_path.trim()) throw new Error('绑定路径不能为空'); return { workspace_kind: w.workspace_kind, display_path: w.display_path.trim() }; });
          var saved = await api('/' + encodeURIComponent(p.id), 'PATCH', { name: newName, workspaces: ws });
          d.project = saved; d.name = saved.name; d.workspaces = bindings(saved);
          projects = projects.map(function (x) { return x.id === saved.id ? saved : x; });
          renderOptions(saved.id); renderEditor(); message('名称与绑定已保存；知识正文请单独保存。');
        });
      });
      var actions = node('div', editor); actions.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;margin:12px 0';
      button(actions, p.archived ? '恢复使用' : '归档项目', function () {
        if (!leave()) return;
        if (!window.confirm(p.archived ? '恢复后该项目可正常注入和维护，确认恢复？' : '归档后停止自动注入并只读保留，不中断正在运行的会话。确认归档？')) return;
        perform(async function () {
          var saved = await api('/' + encodeURIComponent(p.id), 'PATCH', { archived: !p.archived });
          d.project = saved;
          projects = projects.map(function (x) { return x.id === saved.id ? saved : x; }); renderOptions(saved.id); renderEditor();
          message(saved.archived ? '已归档，未保存草稿仍留在内存；恢复使用后可继续编辑。' : '已恢复使用。');
        });
      });
      button(actions, '永久删除项目…', function () {
        if (!window.confirm('永久删除「' + p.name + '」？将删除项目、绑定、知识、项目事件及关联，无法恢复；不会删除来源会话、代码、附件或工作区文件。')) return;
        if (window.prompt('为确认不可恢复的删除，请输入项目名称：', '') !== p.name) return message('未匹配项目名称，已取消删除。');
        perform(async function () {
          await api('/' + encodeURIComponent(p.id), 'DELETE', { confirm: true });
          drafts.delete(p.id); current = null; editor.replaceChildren(); await refresh(); message('项目已永久删除，来源会话和工作区文件不受影响。');
        });
      });
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
          var list = await api('');
          var project = list.find(function (x) { return x.id === p.id; });
          if (!project) throw new Error('项目已删除，请先导出草稿');
          drafts.delete(p.id); projects = list;
          current = { project: project, base: fresh, name: project.name, workspaces: bindings(project), content: fresh.content || '', budget: String(fresh.budget_limit), conflict: null };
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
        a.href = url; a.download = (d.name || p.id).replace(/[<>:"/\\|?*\x00-\x1f]/g, '_').slice(0, 100) + '.md'; a.click(); a.remove();
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
        pane: { label: '项目知识库' },
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
