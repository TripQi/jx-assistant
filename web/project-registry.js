/* 宿主提供的「项目登记」管理视图（项目身份 + 多工作区绑定 + 归档/删除）。
 *
 * 为什么放在宿主而不是某个组件里：项目登记是全实例唯一的一套（Project 表 +
 * /api/projects 服务）。项目知识库页需要它，项目事件页也需要它——而项目事件
 * 组件在知识库组件关闭时仍要能登记和管理项目。把视图做进宿主、两个设置页都挂
 * 这一份，就不存在「第二套登记体系」，也不依赖另一个组件是否启用。
 *
 * 接口约定：JX.projectRegistry.mount(el, {onChange}) → {refresh()}；
 * JX.projectRegistry.list() → Promise<Project[]>（含归档，供调用方自己渲染下拉）。
 * 本视图不做知识正文，也不碰事件数据。
 */
(function () {
  'use strict';
  if (typeof window === 'undefined' || !window.JX) return;
  const JX = window.JX;

  async function api(path, method, body) {
    const res = await fetch('/api/projects' + path, {
      method: method || 'GET',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store'
    });
    const text = await res.text();
    let data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* 下方统一报错 */ }
    if (!res.ok) {
      const err = new Error(data && (data.error || data.message) ? String(data.error || data.message) : 'HTTP ' + res.status + (text ? '：' + text.slice(0, 300) : ''));
      err.status = res.status;
      throw err;
    }
    if (data === null) throw new Error('服务器未返回有效 JSON，请稍后重试。');
    return data;
  }

  function node(tag, parent, text, cls) {
    const e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (cls) e.className = cls;
    if (parent) parent.appendChild(e);
    return e;
  }

  function button(parent, text, action, cls) {
    const b = node('button', parent, text, cls || 'mc-btn');
    b.type = 'button';
    b.onclick = action;
    return b;
  }

  function field(parent, label) {
    const box = node('label', parent, undefined, 'set-field');
    node('span', box, label, 'sf-label');
    const e = node('input', box, undefined, 'pf-input');
    e.type = 'text';
    return e;
  }

  function bindingsText(p) {
    return (p.workspaces || []).map(function (w) {
      return (w.workspace_kind === 'remote' ? '远程 ' : '本地 ') + (w.display_path || '');
    }).join('；');
  }

  function normalizeBinding(kind, path) {
    kind = kind === 'remote' ? 'remote' : 'local';
    path = String(path || '').trim();
    if (!path) throw new Error('请填写工作区路径');
    if (kind === 'local' && !/^(?:[A-Za-z]:[\\/]|\\\\|\/)/.test(path)) {
      throw new Error('本地工作区要填绝对路径（如 D:\\work\\proj）；远程填 user@host:目录 或 host:目录');
    }
    return { workspace_kind: kind, display_path: path };
  }

  function mount(el, opts) {
    opts = opts || {};
    el.replaceChildren();
    const state = { projects: [], editing: null, draftBindings: [], busy: false };
    let disposed = false;
    function alive() { return !disposed && el.isConnected; }

    node('p', el, '项目登记是全实例唯一的一套：项目名、多工作区绑定与归档状态由这里管理，项目知识库与项目事件共用同一份登记。', 'sf-desc');
    const status = node('p', el, '', 'sf-desc');
    status.setAttribute('role', 'status');
    status.setAttribute('aria-live', 'polite');
    function message(text, error) {
      if (!alive()) return;
      status.textContent = text || '';
      status.style.color = error ? 'var(--danger)' : '';
    }

    const listBox = node('div', el);
    const formBox = node('div', el);
    const actions = node('div', el);
    actions.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;margin:8px 0';
    button(actions, '新建项目', function () { startCreate(); });

    async function reload(selectId) {
      try {
        state.projects = await api('', 'GET');
      } catch (e) {
        message('读取项目失败：' + e.message, true);
        return;
      }
      if (!alive()) return;
      renderList();
      if (selectId) {
        const p = state.projects.find(function (x) { return x.id === selectId; });
        if (p) startEdit(p);
      }
      if (opts.onChange) {
        try { opts.onChange(state.projects); } catch (e) { console.warn('[project-registry]', e); }
      }
    }

    function renderList() {
      listBox.replaceChildren();
      if (!state.projects.length) {
        node('p', listBox, '还没有登记任何项目。用下方「新建项目」填名称并绑定工作区即可；项目事件与项目知识库都认这份登记。', 'sf-desc');
        return;
      }
      state.projects.forEach(function (p) {
        const row = node('div', listBox);
        row.style.cssText = 'border:1px solid var(--border);border-radius:8px;padding:8px 10px;margin:6px 0;display:flex;gap:8px;flex-wrap:wrap;align-items:center';
        const main = node('div', row);
        main.style.cssText = 'flex:1 1 260px;min-width:0;overflow-wrap:anywhere';
        const title = node('div', main);
        node('strong', title, p.name);
        if (p.archived) {
          const badge = node('span', title, '已归档（只读）', 'sf-label');
          badge.style.cssText = 'margin-left:8px;color:var(--danger)';
        }
        node('div', main, bindingsText(p) || '未绑定工作区（事件与知识都无法归属）', 'sf-desc');
        const tools = node('div', row);
        tools.style.cssText = 'display:flex;gap:6px;flex-wrap:wrap';
        button(tools, '编辑', function () { startEdit(p); });
        button(tools, p.archived ? '恢复' : '归档', function () { toggleArchive(p); });
        button(tools, '删除', function () { removeProject(p); });
      });
    }

    function startCreate() {
      state.editing = null;
      state.draftBindings = [];
      renderForm({ name: '', workspaces: [] }, true);
      message('填写名称与工作区后点「创建项目」。');
    }

    function startEdit(p) {
      state.editing = p;
      state.draftBindings = (p.workspaces || []).map(function (w) {
        return { workspace_kind: w.workspace_kind, display_path: w.display_path };
      });
      renderForm(p, false);
      message('正在编辑「' + p.name + '」。保存只改名称与绑定，不动事件和知识。');
    }

    function renderForm(p, isNew) {
      formBox.replaceChildren();
      const box = node('div', formBox);
      box.style.cssText = 'border:1px solid var(--border);border-radius:8px;padding:10px;display:grid;gap:8px';
      node('div', box, isNew ? '新建项目' : '编辑项目', 'sf-label');
      const nameInput = field(box, '项目名称');
      nameInput.value = p.name || '';
      const bindBox = node('div', box);
      function renderBindings() {
        bindBox.replaceChildren();
        node('div', bindBox, '工作区绑定（同一项目可绑多个目录；归属按最长路径匹配）', 'sf-label');
        state.draftBindings.forEach(function (w, i) {
          const line = node('div', bindBox);
          line.style.cssText = 'display:flex;gap:6px;align-items:center;margin:4px 0';
          const kind = node('select', line, undefined, 'pf-input');
          [['local', '本地'], ['remote', '远程']].forEach(function (o) {
            const opt = document.createElement('option');
            opt.value = o[0];
            opt.textContent = o[1];
            kind.appendChild(opt);
          });
          kind.value = w.workspace_kind;
          kind.onchange = function () { w.workspace_kind = kind.value; };
          const path = node('input', line, undefined, 'pf-input');
          path.type = 'text';
          path.style.flex = '1 1 200px';
          path.value = w.display_path;
          path.oninput = function () { w.display_path = path.value; };
          button(line, '移除', function () { state.draftBindings.splice(i, 1); renderBindings(); });
        });
        const addLine = node('div', bindBox);
        addLine.style.cssText = 'display:flex;gap:6px;align-items:center';
        const newPath = node('input', addLine, undefined, 'pf-input');
        newPath.type = 'text';
        newPath.style.flex = '1 1 200px';
        newPath.placeholder = 'D:\\work\\proj 或 user@host:目录';
        const newKind = node('select', addLine, undefined, 'pf-input');
        [['local', '本地'], ['remote', '远程']].forEach(function (o) {
          const opt = document.createElement('option');
          opt.value = o[0];
          opt.textContent = o[1];
          newKind.appendChild(opt);
        });
        button(addLine, '添加绑定', function () {
          try {
            state.draftBindings.push(normalizeBinding(newKind.value, newPath.value));
            newPath.value = '';
            renderBindings();
          } catch (e) { message(e.message, true); }
        });
      }
      renderBindings();
      const foot = node('div', box);
      foot.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap';
      button(foot, isNew ? '创建项目' : '保存名称与绑定', function () { save(isNew, nameInput.value); });
      if (!isNew) button(foot, '取消编辑', function () { formBox.replaceChildren(); state.editing = null; message(''); });
    }

    async function save(isNew, name) {
      if (state.busy) return;
      name = String(name || '').trim();
      if (!name) return message('项目名称不能为空', true);
      if (!state.draftBindings.length) return message('至少绑定一个工作区，否则事件与知识无法归属', true);
      state.busy = true;
      try {
        if (isNew) {
          const p = await api('', 'POST', { name: name });
          await api('/' + p.id, 'PATCH', { expected_updated_at: p.updated_at, workspaces: state.draftBindings });
          message('已创建项目「' + name + '」。');
          formBox.replaceChildren();
          await reload(p.id);
        } else {
          const cur = state.editing;
          await api('/' + cur.id, 'PATCH', {
            expected_updated_at: cur.updated_at,
            name: name,
            workspaces: state.draftBindings
          });
          message('已保存「' + name + '」。');
          formBox.replaceChildren();
          state.editing = null;
          await reload();
        }
      } catch (e) {
        if (e.status === 409) {
          message('保存冲突：项目已在别处改动（updated_at 变了）。已保留你的输入，请重新载入该项目的当前值后手工合并再保存。', true);
        } else {
          message('保存失败：' + e.message, true);
        }
      } finally {
        state.busy = false;
      }
    }

    async function toggleArchive(p) {
      if (state.busy) return;
      const verb = p.archived ? '恢复' : '归档';
      if (!window.confirm(verb + '「' + p.name + '」？归档后停止自动注入并只读保留事件与知识，不中断正在运行的会话。')) return;
      state.busy = true;
      try {
        await api('/' + p.id, 'PATCH', { expected_updated_at: p.updated_at, archived: !p.archived });
        message('已' + verb + '「' + p.name + '」。');
        await reload();
      } catch (e) {
        message(verb + '失败：' + e.message, true);
      } finally {
        state.busy = false;
      }
    }

    async function removeProject(p) {
      if (state.busy) return;
      if (!window.confirm('永久删除「' + p.name + '」？将删除项目、绑定、知识、项目事件及其记录与来源关系，无法恢复；不会删除来源会话、消息、代码或工作区文件。')) return;
      const typed = window.prompt('为确认不可恢复的删除，请输入项目名称：', '');
      if (typed !== p.name) return message('未匹配项目名称，已取消删除。');
      state.busy = true;
      try {
        await api('/' + p.id, 'DELETE', { confirm: true });
        message('已删除「' + p.name + '」。');
        await reload();
      } catch (e) {
        message('删除失败：' + e.message, true);
      } finally {
        state.busy = false;
      }
    }

    reload();
    return {
      refresh: function (id) { return reload(id); },
      dispose: function () { disposed = true; }
    };
  }

  JX.projectRegistry = {
    mount: mount,
    list: function () { return api('', 'GET'); }
  };
})();
