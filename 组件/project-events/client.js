// 项目事件设置页：跨项目总览 → 单目标时间线 → 来源会话/消息。
// 项目登记不在这里重做：用宿主提供的 JX.projectRegistry 视图（与项目知识库页同一份
// 登记服务），所以知识库组件关闭时，本页仍能新建/改名/绑定/归档/删除项目。
// 本页只做事件：概况、时间线、来源；不复制会话正文，也不写知识正文。
(function () {
  'use strict';
  var hosts = new Set();
  var disposed = false;

  var STATUS = [
    ['discussing', '讨论中'], ['processing', '处理中'], ['awaiting', '待验收'],
    ['paused', '暂停'], ['done', '已完成'], ['cancelled', '取消']
  ];
  var KINDS = [
    ['plan', '方案确认'], ['milestone', '阶段成果'], ['issue', '关键问题'],
    ['validation', '验证结论'], ['blocker', '阻碍变化'], ['acceptance', '用户验收'], ['other', '其他关键事实']
  ];
  var LIMIT = 20;

  async function api(path, method, body) {
    var res = await fetch('/api/project-events' + path, {
      method: method || 'GET',
      headers: body === undefined ? {} : { 'Content-Type': 'application/json' },
      body: body === undefined ? undefined : JSON.stringify(body),
      cache: 'no-store'
    });
    var text = await res.text(), data = null;
    try { data = text ? JSON.parse(text) : null; } catch (_) { /* 下方统一报错 */ }
    if (!res.ok) {
      var err = new Error(data && (data.error || data.message) ? String(data.error || data.message) : 'HTTP ' + res.status + (text ? '：' + text.slice(0, 300) : ''));
      err.status = res.status;
      throw err;
    }
    if (data === null) throw new Error('服务器未返回有效 JSON，请稍后重试。');
    return data;
  }

  function node(tag, parent, text, cls) {
    var e = document.createElement(tag);
    if (text !== undefined) e.textContent = text;
    if (cls) e.className = cls;
    if (parent) parent.appendChild(e);
    return e;
  }
  function button(parent, text, action, cls) {
    var b = node('button', parent, text, cls || 'mc-btn');
    b.type = 'button';
    b.onclick = action;
    return b;
  }
  function field(parent, label) {
    var box = node('label', parent, undefined, 'set-field');
    node('span', box, label, 'sf-label');
    var e = node('input', box, undefined, 'pf-input');
    e.type = 'text';
    return e;
  }
  function select(parent, label, options, value) {
    var box = node('label', parent, undefined, 'set-field');
    node('span', box, label, 'sf-label');
    var s = node('select', box, undefined, 'pf-input');
    options.forEach(function (o) {
      var opt = document.createElement('option');
      opt.value = o[0];
      opt.textContent = o[1];
      s.appendChild(opt);
    });
    if (value !== undefined) s.value = value;
    return s;
  }
  function time(value, note) {
    if (!value) return note || '未记录';
    var d = new Date(value);
    var text = isNaN(d.getTime()) ? String(value) : d.toLocaleString(undefined, { timeZoneName: 'short' });
    return note ? text + '（' + note + '）' : text;
  }
  function labelOf(list, key) {
    for (var i = 0; i < list.length; i++) if (list[i][0] === key) return list[i][1];
    return key || '';
  }
  function currentSessionId() {
    try {
      if (typeof state !== 'undefined' && state && state.cur && state.cur.id) return state.cur.id;
    } catch (_) { /* 没有宿主态就不预填 */ }
    return '';
  }
  function newKey() {
    return 'ui-' + Date.now().toString(36) + '-' + Math.random().toString(36).slice(2, 8);
  }

  function mount(el) {
    el.replaceChildren();
    if (el.closest('#cxsession') || !el.closest('#settings')) {
      node('p', el, '项目事件属于全局大目标索引。请到「设置 → 项目事件」查看与管理；本对话浮窗不提供。', 'sf-desc');
      return;
    }
    var host = { stop: function () { generation++; } };
    hosts.forEach(function (h) { if (!h.el.isConnected) { h.stop(); hosts.delete(h); } });
    host.el = el;
    hosts.add(host);

    var generation = 0;
    var alive = function () { return !disposed && el.isConnected; };
    var ui = {
      projects: [],
      scope: 'active',
      status: '',
      eventKeyword: '',
      projectKeyword: '',
      includeArchived: false,
      overview: null,
      view: null,        // {eventId, projectId, event, records:[], cursor, remaining}
      detailBox: null,
      registryMounted: false
    };

    node('p', el, '短而可靠的大目标索引：只记「目标是什么、现在怎样、哪些关键事实支撑判断、去哪里核实」。协议全文、代码、日志与会话原文仍留在原载体，本页不复制。', 'sf-desc');
    node('p', el, '分层读取：总览只给计数与概况，点开某目标才读时间线，点开某条记录才读来源；每个字段各有独立预算（标题 80 / 概况 240 / 每条事实 240 单元），超限会指出已用与上限，不静默截断。', 'sf-desc');
    var statusLine = node('p', el, '', 'sf-desc');
    statusLine.setAttribute('role', 'status');
    statusLine.setAttribute('aria-live', 'polite');
    function message(text, error, target) {
      if (!alive()) return;
      var box = target || statusLine;
      box.textContent = text || '';
      box.style.color = error ? 'var(--danger)' : '';
    }

    // ---------- 项目登记（宿主同一份视图） ----------
    var registryBox = node('div', el);
    var registryHost = node('div', registryBox);
    registryHost.hidden = true;
    var registryToggle = button(registryBox, '项目登记 ▸ 展开', function () {
      registryHost.hidden = !registryHost.hidden;
      registryToggle.textContent = registryHost.hidden ? '项目登记 ▸ 展开' : '项目登记 ▾ 收起';
      if (!registryHost.hidden && !ui.registryMounted) {
        if (window.JX && JX.projectRegistry) {
          JX.projectRegistry.mount(registryHost, {
            onChange: function (list) { ui.projects = list || []; renderOverview(); }
          });
          ui.registryMounted = true;
        } else {
          registryHost.replaceChildren();
          node('p', registryHost, '宿主未提供项目登记视图，请确认程序为当前版本；本页不另建登记表。', 'sf-desc');
        }
      }
    });
    node('p', registryBox, '项目身份与工作区绑定是全实例唯一的一套：本页直接用宿主登记视图管理与项目知识库同一份登记，不另建第二套。', 'sf-desc');

    // ---------- 总览 ----------
    var filterBox = node('div', el);
    filterBox.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:flex-end';
    var scopeSel = select(filterBox, '范围', [['active', '未结束'], ['history', '已完成/取消'], ['all', '全部']], 'active');
    var statusSel = select(filterBox, '状态', [['', '按范围']].concat(STATUS), '');
    var keywordInput = field(filterBox, '目标关键词');
    var projectKeywordInput = field(filterBox, '项目关键词');
    scopeSel.onchange = function () { ui.scope = scopeSel.value; loadOverview(); };
    statusSel.onchange = function () { ui.status = statusSel.value; loadOverview(); };
    keywordInput.onchange = function () { ui.eventKeyword = keywordInput.value.trim(); loadOverview(); };
    projectKeywordInput.onchange = function () { ui.projectKeyword = projectKeywordInput.value.trim(); loadOverview(); };
    var archivedLabel = node('label', filterBox, undefined, 'sf-label');
    var archived = node('input', archivedLabel);
    archived.type = 'checkbox';
    archived.onchange = function () { ui.includeArchived = archived.checked; loadOverview(); };
    node('span', archivedLabel, ' 含已归档项目');
    button(filterBox, '刷新', function () { loadOverview(); });

    var overviewBox = node('div', el);
    var createBox = node('div', el);

    // ---------- 详情 ----------
    var detailTitle = node('div', el);
    ui.detailBox = node('div', el);

    function projectName(id) {
      var p = ui.projects.find(function (x) { return x.id === id; });
      return p ? p.name : id;
    }

    async function loadProjects() {
      try {
        if (window.JX && JX.projectRegistry) ui.projects = await JX.projectRegistry.list();
      } catch (e) {
        if (!ui.projects.length) message('读取项目清单失败：' + e.message, true);
      }
    }

    async function loadOverview() {
      var gen = generation;
      message('正在读取总览…');
      try {
        var q = new URLSearchParams();
        q.set('scope', ui.scope);
        if (ui.status) q.set('status', ui.status);
        if (ui.eventKeyword) q.set('event_keyword', ui.eventKeyword);
        if (ui.projectKeyword) q.set('project_keyword', ui.projectKeyword);
        if (ui.includeArchived) q.set('include_archived', '1');
        q.set('project_limit', '12');
        var data = await api('/overview?' + q.toString());
        if (gen !== generation || !alive()) return;
        ui.overview = data;
        renderOverview();
        message('总览 ' + data.project_total + ' 个项目（显示 ' + data.project_shown + '，剩余 ' + data.project_remaining + '）。首页预览每项目最多 3 个目标，不是全量。');
      } catch (e) {
        if (gen !== generation || !alive()) return;
        message('读取总览失败：' + e.message, true);
      }
    }

    function statusBadge(parent, ev) {
      var b = node('span', parent, ev.status_label || labelOf(STATUS, ev.status));
      b.style.cssText = 'font-size:12px;padding:0 6px;border-radius:6px;border:1px solid var(--border);color:var(--text-2)';
      return b;
    }

    function eventCard(parent, ev, projectId) {
      var card = node('div', parent);
      card.style.cssText = 'border:1px solid var(--border);border-radius:8px;padding:8px 10px;margin:6px 0';
      var head = node('div', card);
      head.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
      var name = node('strong', head, ev.title);
      name.style.cssText = 'min-width:0;overflow-wrap:anywhere';
      statusBadge(head, ev);
      node('span', head, '记录 ' + ev.record_count + ' · 来源会话 ' + ev.session_count, 'sf-desc');
      node('div', card, ev.summary, 'sf-desc');
      var foot = node('div', card);
      foot.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;margin-top:6px;align-items:center';
      node('span', foot, '更新 ' + time(ev.updated_at), 'sf-desc');
      button(foot, '查看时间线', function () { openEvent(projectId, ev.event_id); });
    }

    function renderOverview() {
      overviewBox.replaceChildren();
      renderCreate();
      if (!ui.overview) return;
      var data = ui.overview;
      if (!data.projects.length) {
        node('p', overviewBox, '当前筛选下没有项目。若还没登记项目，展开上方「项目登记」新建并绑定工作区（本页即可完成，不需要去别的页）。', 'sf-desc');
        return;
      }
      data.projects.forEach(function (g) {
        var box = node('div', overviewBox);
        box.style.cssText = 'border:1px solid var(--border);border-radius:10px;padding:10px;margin:10px 0';
        var head = node('div', box);
        head.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
        var n = node('strong', head, g.name);
        if (g.archived) {
          var badge = node('span', head, '已归档（只读）', 'sf-desc');
          badge.style.color = 'var(--danger)';
        }
        node('span', head, '未结束 ' + g.counts.active + '（讨论 ' + g.counts.discussing + ' / 处理 ' + g.counts.processing +
          ' / 待验收 ' + g.counts.awaiting + ' / 暂停 ' + g.counts.paused + '）· 已完成/取消 ' + g.counts.history, 'sf-desc');
        if (g.counts.matched > g.events.length) {
          button(head, '本组更多目标（' + g.counts.matched + '）', function () { openProject(g.project_id); });
        }
        if (!g.events.length) {
          node('p', box, '该筛选下没有目标。', 'sf-desc');
        } else {
          g.events.forEach(function (ev) { eventCard(box, ev, g.project_id); });
        }
        if (g.events_remaining > 0) {
          node('p', box, '还有 ' + g.events_remaining + ' 个目标未展示（预览只给 3 个）。', 'sf-desc');
        }
      });
      if (data.has_more) {
        button(overviewBox, '显示更多项目', function () { loadMoreProjects(data.next_project_cursor); });
      }
    }

    async function loadMoreProjects(cursor) {
      var gen = generation;
      try {
        var q = new URLSearchParams();
        q.set('scope', ui.scope);
        if (ui.status) q.set('status', ui.status);
        if (ui.eventKeyword) q.set('event_keyword', ui.eventKeyword);
        if (ui.projectKeyword) q.set('project_keyword', ui.projectKeyword);
        if (ui.includeArchived) q.set('include_archived', '1');
        q.set('project_limit', '12');
        q.set('project_cursor', cursor || '');
        var data = await api('/overview?' + q.toString());
        if (gen !== generation || !alive()) return;
        var merged = ui.overview;
        merged.projects = merged.projects.concat(data.projects);
        merged.has_more = data.has_more;
        merged.next_project_cursor = data.next_project_cursor;
        merged.project_shown = merged.projects.length;
        merged.project_remaining = data.project_remaining;
        renderOverview();
      } catch (e) {
        message('加载更多项目失败：' + e.message, true);
      }
    }

    function renderCreate() {
      createBox.replaceChildren();
      if (!ui.projects.length) return;
      var box = node('div', createBox);
      box.style.cssText = 'border:1px dashed var(--border);border-radius:8px;padding:10px;margin:10px 0;display:grid;gap:8px';
      node('div', box, '新建目标', 'sf-label');
      var projSel = select(box, '项目', ui.projects.map(function (p) {
        return [p.id, p.name + '（' + (p.workspaces || []).length + ' 个绑定）' + (p.archived ? ' · 已归档只读' : '')];
      }));
      // 显式选第一个：不依赖浏览器的 <select> 默认选中行为，也避免提交空 project_id。
      if (ui.projects.length) projSel.value = ui.projects[0].id;
      var title = field(box, '标题（一句目标短语）');
      var summary = field(box, '概况（一句当前状态）');
      var status = select(box, '状态', STATUS, 'discussing');
      var line = node('p', box, '直接建成非讨论中状态时，请在下一步追加一条关键事实（时间线入口在目标里）。', 'sf-desc');
      button(box, '新建目标', async function () {
        try {
          var data = await api('', 'POST', {
            project_id: projSel.value,
            title: title.value,
            summary: summary.value,
            status: status.value
          });
          message('已新建「' + data.event.title + '」。');
          title.value = ''; summary.value = '';
          await loadOverview();
        } catch (e) {
          message('新建失败：' + e.message, true);
        }
      });
      line.setAttribute('aria-hidden', 'true');
    }

    // ---------- 单项目目标列表 ----------
    async function openProject(projectId) {
      ui.view = { projectId: projectId, events: [], cursor: '', remaining: 0, total: 0 };
      detailTitle.replaceChildren();
      ui.detailBox.replaceChildren();
      var head = node('div', detailTitle);
      head.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-top:12px';
      node('strong', head, '项目：' + projectName(projectId));
      button(head, '返回总览', function () { renderOverview(); ui.view = null; detailTitle.replaceChildren(); ui.detailBox.replaceChildren(); });
      var box = node('div', ui.detailBox);
      await loadProjectEvents(projectId, '', true, box, head);
    }

    async function loadProjectEvents(projectId, cursor, replace, box, head) {
      var gen = generation;
      try {
        var q = new URLSearchParams();
        q.set('project_id', projectId);
        q.set('scope', ui.scope);
        if (ui.status) q.set('status', ui.status);
        if (ui.eventKeyword) q.set('keyword', ui.eventKeyword);
        q.set('limit', String(LIMIT));
        if (cursor) q.set('cursor', cursor);
        var data = await api('?' + q.toString());
        if (gen !== generation || !alive()) return;
        if (replace) box.replaceChildren();
        ui.view = ui.view || {};
        ui.view.cursor = data.next_cursor || '';
        ui.view.remaining = data.remaining || 0;
        node('p', box, '本筛选共 ' + data.total + ' 个目标，已显示 ' + Math.max(0, data.total - data.remaining) + '，剩余 ' + data.remaining + '。', 'sf-desc');
        data.events.forEach(function (ev) { eventCard(box, ev, projectId); });
        if (data.has_more) {
          var more = button(box, '加载更多目标', function () {
            more.remove();
            loadProjectEvents(projectId, data.next_cursor, false, box, head);
          });
        }
        // 项目操作
        var tools = node(ui.detailBox, undefined);
      } catch (e) {
        message('读取项目目标失败：' + e.message, true);
      }
    }

    // ---------- 事件详情 ----------
    async function openEvent(projectId, eventId) {
      var gen = generation;
      detailTitle.replaceChildren();
      ui.detailBox.replaceChildren();
      var head = node('div', detailTitle);
      head.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center;margin-top:12px';
      node('strong', head, '目标详情');
      button(head, '返回总览', function () { ui.view = null; detailTitle.replaceChildren(); ui.detailBox.replaceChildren(); });
      var box = node('div', ui.detailBox);
      try {
        var data = await api('/' + encodeURIComponent(eventId));
        if (gen !== generation || !alive()) return;
        renderEvent(box, projectId, data.event);
        await loadRecords(box, data.event, '');
      } catch (e) {
        message('读取目标失败：' + e.message, true);
      }
    }

    function renderEvent(box, projectId, ev) {
      box.replaceChildren();
      var head = node('div', box);
      head.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
      node('strong', head, ev.title);
      statusBadge(head, ev);
      node('span', head, 'revision ' + ev.revision + ' · 记录 ' + ev.record_count + ' · 来源会话 ' + ev.session_count, 'sf-desc');
      node('p', box, ev.summary, 'sf-desc');
      node('p', box, '创建 ' + time(ev.created_at) + ' · 最后修改 ' + time(ev.updated_at), 'sf-desc');

      // 概况/状态编辑
      var edit = node('div', box);
      edit.style.cssText = 'border:1px solid var(--border);border-radius:8px;padding:10px;margin:8px 0;display:grid;gap:8px';
      node('div', edit, '修改概况（按 revision 校验；只改标题/概况不强制补事实，改状态需补一条事实）', 'sf-label');
      var title = field(edit, '标题');
      title.value = ev.title;
      var summary = field(edit, '概况');
      summary.value = ev.summary;
      var status = select(edit, '状态', STATUS, ev.status);
      var recBox = node('div', edit);
      recBox.style.cssText = 'display:grid;gap:8px';
      var recKind = select(recBox, '同事务追加的事实类型', KINDS, 'milestone');
      var recText = field(recBox, '事实（一句；留空则不追加）');
      var recSource = node('label', recBox, undefined, 'sf-label');
      var recUseSession = node('input', recSource);
      recUseSession.type = 'checkbox';
      var curSid = currentSessionId();
      recUseSession.checked = !!curSid;
      node('span', recSource, curSid ? ' 绑定当前会话作为来源（' + curSid + '）' : ' 当前没有可绑定的会话');
      recUseSession.disabled = !curSid;
      var act = node('div', edit);
      act.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap';
      button(act, '保存概况', async function () {
        try {
          var body = { expected_revision: ev.revision, title: title.value, summary: summary.value, status: status.value };
          if (recText.value.trim()) {
            body.record = {
              kind: recKind.value,
              text: recText.value,
              client_key: newKey(),
              sources: recUseSession.checked && curSid ? [{ session_id: curSid }] : []
            };
          }
          var out = await api('/' + encodeURIComponent(ev.event_id), 'PATCH', body);
          message(out.changed ? '已保存目标概况（revision ' + out.event.revision + '）。' : '内容与来源都没有变化，未刷新 revision。');
          await openEvent(projectId, ev.event_id);
          await loadOverview();
        } catch (e) {
          message(e.status === 409 ? '保存冲突：目标已被别处改动。已保留你的输入，请重新载入后手工合并。' : '保存失败：' + e.message, true);
        }
      });
      button(act, '删除目标', async function () {
        if (!window.confirm('永久删除目标「' + ev.title + '」及其全部记录与来源关系？无法恢复；不会删除来源会话、消息或代码。')) return;
        try {
          await api('/' + encodeURIComponent(ev.event_id), 'DELETE', { confirm: true });
          message('已删除目标。');
          ui.view = null;
          detailTitle.replaceChildren();
          ui.detailBox.replaceChildren();
          await loadOverview();
        } catch (e) {
          message('删除失败：' + e.message, true);
        }
      });

      // 追加事实
      var add = node('div', box);
      add.style.cssText = 'border:1px solid var(--border);border-radius:8px;padding:10px;margin:8px 0;display:grid;gap:8px';
      node('div', add, '追加一条关键事实（一句、单行；页码内按事实时间正序）', 'sf-label');
      var addKind = select(add, '类型', KINDS, 'milestone');
      var addText = field(add, '事实');
      var addAt = field(add, '事实时刻（可选，如 2026-11-16T09:30:00+08:00；不确定就留空）');
      var addSourceLine = node('div', add);
      addSourceLine.style.cssText = 'display:flex;gap:6px;flex-wrap:wrap;align-items:center';
      var addSession = node('input', addSourceLine, undefined, 'pf-input');
      addSession.type = 'text';
      addSession.placeholder = '来源会话 ID（可留空＝不绑来源）';
      addSession.style.flex = '1 1 220px';
      var addSeq = node('input', addSourceLine, undefined, 'pf-input');
      addSeq.type = 'number';
      addSeq.placeholder = '消息 seq（可空）';
      addSeq.style.width = '130px';
      if (curSid) {
        button(addSourceLine, '用当前会话', function () {
          addSession.value = curSid;
          addSeq.value = '';
        });
      }
      var addBtn = button(add, '追加事实', async function () {
        var sources = [];
        if (addSession.value.trim()) {
          sources.push({ session_id: addSession.value.trim(), message_seq: Number(addSeq.value) || 0 });
        }
        try {
          var out = await api('/' + encodeURIComponent(ev.event_id) + '/records', 'POST', {
            expected_event_revision: ev.revision,
            record: {
              kind: addKind.value,
              text: addText.value,
              occurred_at: addAt.value.trim(),
              client_key: newKey(),
              sources: sources
            }
          });
          message(out.changed ? '已追加事实。' : '这条事实此前已提交（幂等），未重复追加。');
          await openEvent(projectId, ev.event_id);
          await loadOverview();
        } catch (e) {
          message(e.status === 409 ? '追加冲突：目标已被别处改动，请重读后重试。' : '追加失败：' + e.message, true);
        }
      });
      addBtn.setAttribute('data-action', 'record-add');
      var recordsBox = node('div', box);
      recordsBox.id = 'pe-records';
      ui.view = { projectId: projectId, eventId: ev.event_id, event: ev, recordsBox: recordsBox };
    }

    // ---------- 时间线 ----------
    async function loadRecords(box, ev, cursor) {
      var gen = generation;
      var box2 = box.querySelector('#pe-records');
      if (!box2) return;
      try {
        var q = new URLSearchParams();
        q.set('limit', String(LIMIT));
        if (cursor) q.set('cursor', cursor);
        var data = await api('/' + encodeURIComponent(ev.event_id) + '/records?' + q.toString());
        if (gen !== generation || !alive()) return;
        if (!cursor) box2.replaceChildren();
        node('p', box2, '时间线：共 ' + data.total + ' 条记录，剩余更早 ' + data.remaining_older + ' 条' +
          (data.unknown_time ? '；另有 ' + data.unknown_time + ' 条事实时间未明确（不猜归期）' : ''), 'sf-desc');
        data.records.forEach(function (item) { recordRow(box2, ev, item, gen); });
        if (data.has_more) {
          var more = button(box2, '加载更早的事实', function () {
            more.remove();
            loadRecords(box, ev, data.next_cursor);
          });
        }
        if (!data.total) node('p', box2, '还没有推进记录。用上方「追加一条关键事实」开始。', 'sf-desc');
      } catch (e) {
        message('读取时间线失败：' + e.message, true);
      }
    }

    function recordRow(parent, ev, item, gen) {
      var rec = item.record;
      var row = node('div', parent);
      row.style.cssText = 'border-left:2px solid var(--border);padding:6px 0 6px 10px;margin:6px 0';
      var head = node('div', row);
      head.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
      node('span', head, time(rec.occurred_at || rec.created_at, item.time_note ? '记录时间' : ''), 'sf-desc');
      node('span', head, rec.kind_label || labelOf(KINDS, rec.kind), 'sf-label');
      node('span', head, rec.origin === 'user' ? '用户录入' : 'AI 记录', 'sf-desc');
      node('span', head, 'revision ' + rec.revision, 'sf-desc');
      var text = node('div', row, rec.text);
      text.style.cssText = 'overflow-wrap:anywhere;margin:4px 0';
      var tools = node('div', row);
      tools.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap';
      button(tools, '来源（' + (item.sources_total || 0) + '）', function () { showSources(tools, rec); });
      var editWrap = node('div', row);
      button(tools, '更正', function () { renderRecordEdit(editWrap, ev, rec); });
      button(tools, '删除这条', async function () {
        if (!window.confirm('删除这条记录及其来源关系？无法恢复；父事件 revision 会递增。')) return;
        try {
          await api('/records/' + encodeURIComponent(rec.record_id), 'DELETE', { confirm: true });
          message('已删除记录。');
          await openEvent(ui.view.projectId, ev.event_id);
        } catch (e) {
          message('删除失败：' + e.message, true);
        }
      });
      if (item.sources && item.sources.length) {
        var badges = node('div', row);
        badges.style.cssText = 'display:flex;gap:6px;flex-wrap:wrap;margin-top:4px';
        item.sources.forEach(function (s) {
          var b = node('span', badges, sourceStateText(s), 'sf-desc');
          b.style.cssText = 'border:1px solid var(--border);border-radius:6px;padding:0 6px';
        });
      }
      void gen;
    }

    function sourceStateText(s) {
      var map = {
        available: '依据可读', unreviewed: '依据未复核', verified_unchanged: '依据已复核',
        message_changed: '依据已改动', message_replaced: '位置已复用', message_missing: '消息已移除',
        session_missing: '会话已不存在', session_restricted: '会话归属已变化'
      };
      var t = map[s.state] || s.state || '未知';
      var tail = s.message_seq ? ' · seq ' + s.message_seq : ' · 整会话';
      return t + tail;
    }

    async function showSources(parent, rec) {
      var box = node('div', parent);
      box.style.cssText = 'border:1px solid var(--border);border-radius:8px;padding:8px;margin-top:6px;display:grid;gap:6px';
      node('div', box, '来源（按绑定时间倒序，不代表事实新旧；正文只在下面「复核」时定点比对）', 'sf-label');
      try {
        var data = await api('/records/' + encodeURIComponent(rec.record_id) + '/sources?limit=20');
        data.sources.forEach(function (s) {
          var line = node('div', box);
          line.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap;align-items:center';
          node('span', line, (s.session_title || '(已不可见)') + (s.message_seq ? ' · seq ' + s.message_seq : ' · 会话级'), 'sf-desc');
          node('span', line, sourceStateText(s), 'sf-label');
          if (s.message_seq) {
            button(line, '定位消息', function () {
              if (typeof closeSettingsUI === 'function') closeSettingsUI();
              if (typeof jumpToMessage === 'function') jumpToMessage(s.session_id, s.message_seq);
            });
          }
          button(line, '打开会话', function () {
            if (typeof closeSettingsUI === 'function') closeSettingsUI();
            if (typeof openSession === 'function') openSession(s.session_id);
          });
          button(line, '复核这条', async function () {
            try {
              var one = await api('/records/' + encodeURIComponent(rec.record_id) + '/sources?source_id=' + encodeURIComponent(s.source_id));
              var v = (one.sources || [])[0];
              message(v ? (v.note || sourceStateText(v)) : '复核没有返回这条来源。', false, box);
            } catch (e) {
              message('复核失败：' + e.message, true, box);
            }
          });
          if (s.note) node('p', line, s.note, 'sf-desc');
        });
        if (!data.sources.length) node('p', box, '这条记录没有绑定来源（口头结论或人工判断）：需要核实就补一条来源。', 'sf-desc');
        if (data.has_more) node('p', box, '还有 ' + data.remaining + ' 个来源未显示。', 'sf-desc');
      } catch (e) {
        message('读取来源失败：' + e.message, true, box);
      }
    }

    function renderRecordEdit(wrap, ev, rec) {
      wrap.replaceChildren();
      var box = node('div', wrap);
      box.style.cssText = 'border:1px dashed var(--border);border-radius:8px;padding:8px;margin-top:6px;display:grid;gap:6px';
      var text = field(box, '更正后的事实');
      text.value = rec.text;
      var kind = select(box, '类型', KINDS, rec.kind);
      var at = field(box, '事实时刻（留空＝未明确）');
      at.value = rec.occurred_at || '';
      var act = node('div', box);
      act.style.cssText = 'display:flex;gap:8px;flex-wrap:wrap';
      button(act, '保存更正', async function () {
        try {
          await api('/records/' + encodeURIComponent(rec.record_id), 'PATCH', {
            expected_record_revision: rec.revision,
            expected_event_revision: ev.revision,
            text: text.value,
            kind: kind.value,
            occurred_at: at.value.trim()
          });
          message('已更正记录。');
          await openEvent(ui.view.projectId, ev.event_id);
        } catch (e) {
          message(e.status === 409 ? '更正冲突：记录或父事件已被改动，请重读后再改。' : '更正失败：' + e.message, true);
        }
      });
      button(act, '取消', function () { wrap.replaceChildren(); });
    }

    (async function boot() {
      await loadProjects();
      if (!alive()) return;
      await loadOverview();
    })();

    host.stop = function () {
      generation++;
      if (ui.registryMounted && window.JX && JX.projectRegistry && typeof registryHost !== 'undefined') {
        // 登记视图由宿主持有；页面重挂时随 DOM 一起销毁，无需额外清理。
      }
    };
  }

  CX.register({
    id: 'project-events',
    init: function () {
      disposed = false;
      CX.registerSettings({
        id: 'project-events',
        title: '项目事件',
        pane: {
          label: '项目事件',
          // 线框图标：一条时间线 + 目标点，规格与设置页静态导航项一致（16px、取 currentColor）
          icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M5 4v16"/><path d="M9 7h8M9 12h10M9 17h6"/><circle cx="5" cy="7" r="1.6"/><circle cx="5" cy="12" r="1.6"/><circle cx="5" cy="17" r="1.6"/></svg>'
        },
        mount: mount
      });
    },
    dispose: function () {
      disposed = true;
      hosts.forEach(function (h) { h.stop(); });
      hosts.clear();
      CX.unregisterSettings('project-events');
    }
  });
})();
