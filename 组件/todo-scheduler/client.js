/* 组件：待办与定时任务 —— 浏览器半边。
 *
 * 侧栏入口区（.actions）末尾独立挂载一个入口按钮（与其他入口同一个 .side-btn 组件），
 * 点击后把待办页渲染进主列（#msg-col）——就是对话页的同一个容器，等同级页面而非
 * 浮窗：顶栏标题改成本页名字、底部输入区收起，内容随 #messages 自然滚动。
 *   - 一条持久化的任务清单：新建 / 编辑 / 删除（两段确认）/ 启用停用 / 立即执行 /
 *     标为完成（收进「已完成」页，调度随之停，可重新打开）；
 *   - 两类任务：AI 任务（到点按执行目标把描述发给长期助理或一个新的工作任务）
 *     与程序任务（到点运行命令）；
 *   - 顶部子页签：待执行 / 已完成——完成的任务收进「已完成」页低频查看，切换即时生效；
 *   - 执行记录（时间、触发方式、成败、摘要、「查看会话」跳转）；
 *   - 程序没开着时错过的执行不自动补跑，页面顶部提示「补跑 / 忽略」。
 *
 * 页面不是自己管生死的：点侧栏任何会话/新工作任务，app.js 会重画 #msg-col 把本页
 * 顶掉——挂在 #msg-col 上的 MutationObserver 察觉后只清自己的状态，不动 DOM。
 * 返回值走「从哪来回哪去」：进页前记住当前会话，关页时 openSession 回去，没有则回首页。
 *
 * 数据与调度全在宿主半边（todo.go 的 /api/todos*），这里只管界面与调用。
 * dispose 撤掉按钮、表单、事件流与全部监听——组件可热关闭。 */
(function () {
  if (!window.CX) return;

  const ID = 'todo-scheduler';
  const WEEK = ['周日', '周一', '周二', '周三', '周四', '周五', '周六'];
  const KINDS = ['manual', 'once', 'daily', 'weekly', 'weekdays', 'interval'];
  const KIND_LABEL = {
    manual: '不定时', once: '定时一次', daily: '每天',
    weekly: '每周某几天', weekdays: '仅工作日', interval: '每隔（分钟）',
  };
  const STATUS_LABEL = { ok: '完成', failed: '失败', running: '执行中' };
  // 待办「任务模型」三档（与 todo.go 的 todoModel* 常量对应）：空/未写＝跟随派发设置
  const MODEL_MODE_LABEL = { dispatch: '跟随派发设置', global: '全局默认模型', custom: '指定模型' };
  const TRIGGER_LABEL = { schedule: '定时', manual: '手动', 'catch-up': '补跑' };
  const PAGE_TITLE = '待办与定时任务';

  /* 运行期状态：清单 + 工作目录候选 + 界面开关。 */
  const st = {
    tasks: [], records: [], missed: [], store: '',
    models: [],       // /api/models 的清单（表单里「指定模型」下拉的分组来源）
    tab: 'todo',      // 子页签：todo=待执行（默认，现在的页面） | done=已完成
    workspaces: [], busy: false, armedDelete: '', expanded: {}, open: {}, editing: null,
    page: false,      // 本页是否正占着主列
    prevSession: null, // 进页前正在看的会话 id（关页时回去）
  };
  let btn = null, formEl = null, sse = null, colObserver = null, disposing = false;

  /* ---------- 小工具 ---------- */

  const $ = (id) => document.getElementById(id);

  const esc = (s) => String(s == null ? '' : s).replace(/[&<>"']/g, (c) => (
    { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c]
  ));

  function fmt(ms) {
    if (!ms || ms <= 0) return '—';
    const d = new Date(ms);
    if (isNaN(d.getTime())) return '—';
    const p = (n) => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + ' ' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  /* epoch 毫秒 → datetime-local 输入值（本地时区）。 */
  function toLocalInput(ms) {
    const d = new Date(ms);
    const p = (n) => String(n).padStart(2, '0');
    return d.getFullYear() + '-' + p(d.getMonth() + 1) + '-' + p(d.getDate()) + 'T' + p(d.getHours()) + ':' + p(d.getMinutes());
  }

  function scheduleText(s) {
    if (!s) return '—';
    switch (s.kind) {
      case 'manual': return '不定时';
      case 'once': return '定时一次 · ' + fmt(s.at);
      case 'daily': return '每天 · ' + s.time;
      case 'weekly': return '每周' + (s.days || []).slice().sort().map((d) => WEEK[d]).join('、') + ' · ' + s.time;
      case 'weekdays': return '仅工作日 · ' + s.time;
      case 'interval': return '每隔 ' + s.everyMinutes + ' 分钟';
    }
    return '—';
  }

  const isCmd = (t) => t.executor === 'command';

  /* ---------- 侧边栏入口 ---------- */

  function mountButton() {
    const actions = document.querySelector('#sidebar .actions');
    if (!actions) return;
    btn = document.createElement('button');
    btn.id = 'btn-todo-scheduler';
    btn.className = 'side-btn';
    btn.title = '待办清单与定时任务';
    // 文字结构与另两个入口一致（span.sb-label 全名 + data-short 短名）：
    // 手机端窄屏一行三枚按钮时全名藏掉、亮短名，更窄只剩图标
    btn.dataset.short = '待办';
    btn.innerHTML =
      '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round">' +
      '<circle cx="12" cy="13" r="8"/><path d="M12 9v4l2.5 1.5M5 3L2 6M19 3l3 3"/></svg>' +
      '<span class="sb-label">待办与定时任务</span>';
    btn.onclick = () => { if (st.page) closePage(); else openPage(); };
    // 位置：入口区末尾独立挂载——组件按钮不锚定其他按钮的 DOM 位置（锚定＝把
    // 自己的排序耦合进别人的 DOM，别人换位/下线时组件按钮被动搬家或落入兜底
    // 路径）。与其他入口平级自然排列（当前序：数字助理 → 派任务给新助理 → 待办）
    actions.appendChild(btn);
  }

  /* ---------- 主列页面 ---------- */

  /* 本页的根节点（页面可能被 app.js 重画顶掉，每次现取）。 */
  function pageEl() { return $('todo-page'); }

  function pageHTML() {
    return '<div id="todo-page" class="td-page">' +
        '<div class="td-head">' +
          '<div>' +
            '<h2 class="td-title">待办事项与定时任务</h2>' +
            '<p class="td-intro">一条一条记下要做的工作。AI 任务到点交给长期助理或一个新的工作任务执行，程序任务到点直接运行命令；结果和会话入口都在这里看。</p>' +
          '</div>' +
          '<div class="td-head-actions">' +
            '<button type="button" id="td-new" class="primary">＋ 新建任务</button>' +
          '</div>' +
        '</div>' +
        '<div class="td-tabs" id="td-tabs"></div>' +
        '<p id="td-notice" class="td-notice hidden"></p>' +
        '<div id="td-missed" class="td-missed hidden"></div>' +
        '<div id="td-list" class="td-list"></div>' +
        '<div class="td-foot"><span id="td-store"></span></div>' +
      '</div>';
  }

  /* 主页面的宿主状态：把主列装成这一页（等同首页/对话页的切换方式）。 */
  function appState() {
    try { return typeof state === 'object' && state ? state : null; } catch (e) { return null; }
  }
  const hide = (id, on) => { const e = $(id); if (e) e.classList.toggle('hidden', on); };

  function mountForm() {
    formEl = document.createElement('div');
    formEl.id = 'todo-form';
    formEl.className = 'modal hidden';
    formEl.innerHTML = '<div class="modal-card td-form-card" id="td-form-card"></div>';
    document.body.appendChild(formEl);
    formEl.onmousedown = (e) => { if (e.target === formEl) closeForm(); };
  }

  /* ---------- 进/出这一页 ---------- */

  /* 覆写主列前先把输入卡挪回底部容器（app.js 的 detachComposerToWrap）：首页时
     #composer-area 就挂在 #msg-col 里，直接覆写会连输入框一起销毁，之后 app.js
     的 placeComposer 会拿到 null 抛错，主列再也重画不出来。 */
  function clearCol() {
    if (typeof detachComposerToWrap === 'function') detachComposerToWrap();
  }

  function openPage() {
    const col = $('msg-col');
    if (!col) return;
    // 手机端：从侧栏浮层点进本页即收起浮层，不然整屏侧栏盖着新打开的页面，
    // 用户不知道要先收（与选中会话/进设置同一套，见 app-core.js narrowClosePanels）
    if (typeof narrowClosePanels === 'function') narrowClosePanels();
    const s = appState();
    st.prevSession = (s && s.cur && s.cur.id) || null;
    // 主列换成这一页；对话那套外壳（输入区、会话进度条、模式徽标、子对话按钮）收起
    clearCol();
    col.innerHTML = pageHTML();
    st.page = true;
    if (s) s.cur = null; // 本页不是任何会话：侧栏取消高亮，后台事件也不会把页面顶回对话
    if (btn) btn.classList.add('active');
    const title = $('sess-title');
    if (title) title.textContent = PAGE_TITLE;
    hide('composer-wrap', true);
    hide('progress-panel', true);
    hide('mode-badge', true);
    hide('btn-drawer', true);
    $('td-new').onclick = () => openForm(null);
    render();
    void load();
    openStream();
    watchCol();
  }

  function closePage() {
    const back = st.prevSession;
    forgetPage();
    closeForm();
    if (back && typeof openSession === 'function') { void openSession(back); return; }
    if (typeof goHomeNewTask === 'function') goHomeNewTask();
    else { clearCol(); const col = $('msg-col'); if (col) col.innerHTML = ''; }
  }

  /* 页面被顶掉（点了侧栏某个会话/新工作任务）：只清状态，DOM 归 app.js 管。 */
  function forgetPage() {
    st.page = false;
    if (btn) btn.classList.remove('active');
    closeStream();
    if (colObserver) { colObserver.disconnect(); colObserver = null; }
  }

  /* app.js 重画 #msg-col 会连本页一起换掉，察觉后清状态（不抢它的 DOM）。 */
  function watchCol() {
    const col = $('msg-col');
    if (!col || colObserver) return;
    colObserver = new MutationObserver(() => { if (!pageEl()) forgetPage(); });
    colObserver.observe(col, { childList: true });
  }

  async function load() {
    if (!st.page) return;
    try {
      const d = await api('/api/todos');
      if (disposing || !st.page) return;
      st.tasks = d.tasks || [];
      st.records = d.records || [];
      st.missed = d.missed || [];
      st.store = d.store || '';
      notice('');
    } catch (e) {
      notice('加载失败：' + (e.message || e));
    }
    if (st.page) render();
  }

  function notice(text) {
    const e = $('td-notice');
    if (!e) return;
    e.textContent = text || '';
    e.classList.toggle('hidden', !text);
  }

  function recordsOf(taskId) {
    return st.records.filter((r) => r.taskId === taskId);
  }

  /* 子页签：待执行 / 已完成（各带计数）。切换只重画本地列表，即时生效。 */
  function renderTabs(root) {
    const box = root.querySelector('#td-tabs');
    if (!box) return;
    const todo = st.tasks.filter((t) => !t.done).length;
    const done = st.tasks.length - todo;
    box.innerHTML =
      '<button type="button" class="td-tab' + (st.tab === 'todo' ? ' active' : '') + '" data-tab="todo">待执行 (' + todo + ')</button>' +
      '<button type="button" class="td-tab' + (st.tab === 'done' ? ' active' : '') + '" data-tab="done">已完成 (' + done + ')</button>';
    box.onclick = (e) => {
      const el = e.target.closest('button[data-tab]');
      if (!el || el.dataset.tab === st.tab) return;
      st.tab = el.dataset.tab;
      render();
    };
  }

  function render() {
    const root = pageEl();
    if (!root) return;
    const list = root.querySelector('#td-list');
    const missedBox = root.querySelector('#td-missed');
    if (!list || !missedBox) return;
    const missed = st.missed || [];
    // 展开/收起只重画列表，滚动位置得自己留住，否则每点一下都回到顶部
    const keepScroll = list.scrollTop;

    missedBox.classList.toggle('hidden', missed.length === 0 || st.tab !== 'todo');
    if (missed.length) {
      missedBox.innerHTML = '<div class="td-missed-title">服务没开着的时候，有 ' + missed.length +
        ' 条任务错过了执行时间（只提示最近一次，不会连环补跑）：</div>' +
        missed.map((m) => {
          const t = st.tasks.find((x) => x.id === m.taskId);
          return '<div class="td-missed-row"><span>' + esc(t ? t.title : m.taskId) + '｜' + fmt(m.slotTime) + '</span>' +
            '<span class="td-actions">' +
            '<button type="button" data-missed="run" data-id="' + esc(m.taskId) + '">补跑一次</button>' +
            '<button type="button" class="ghost" data-missed="dismiss" data-id="' + esc(m.taskId) + '">忽略</button>' +
            '</span></div>';
        }).join('');
    }

    renderTabs(root);
    const shown = st.tasks.filter((t) => (st.tab === 'done' ? t.done : !t.done));
    if (!shown.length) {
      list.innerHTML = '<div class="td-empty">' + (st.tab === 'done'
        ? '还没有已完成的任务。任务做完后在卡片上点「标为完成」，它就会收进这一页。'
        : (st.tasks.length ? '太好了，待执行的任务都清完了（已完成的收在右侧页签里）。' : '还没有任务。点右上角「新建任务」记一条。')) + '</div>';
    } else {
      list.innerHTML = shown.map(cardHTML).join('');
    }
    list.scrollTop = keepScroll;
    root.querySelector('#td-store').textContent = st.store ? '数据位置：' + st.store : '';

    // 列表内的事件：统一委托到容器
    list.onclick = onListClick;
    missedBox.onclick = onMissedClick;
  }

  /* 卡片只显示前三行：徽标行、标题行、创建时间+描述摘要行；第三行往下（完整描述、
     命令、元信息、上次执行、操作按钮、执行记录）全部折进 .td-more，默认收起、
     点卡片空白或三角展开。折叠靠 .td-task.open 这一个类切换，展开态记在 st.open
     里（重画不丢）；执行记录的二级展开（st.expanded）照旧。 */
  function cardHTML(t) {
    const cmd = isCmd(t);
    const recs = recordsOf(t.id);
    const expanded = !!st.expanded[t.id];
    const open = !!st.open[t.id];
    const badges =
      '<span class="td-badge kind">' + (cmd ? '程序任务' : 'AI 任务') + '</span>' +
      (!cmd && t.target === 'task' ? '<span class="td-badge kind">新的工作任务</span>' : '') +
      (t.done ? '<span class="td-badge done">已完成</span>' :
        '<span class="td-badge' + (t.enabled ? '' : ' off') + '">' + esc(scheduleText(t.schedule)) + '</span>' +
        (t.enabled ? '' : '<span class="td-badge off">已停用</span>')) +
      (cmd && t.permission === 'full' ? '<span class="td-badge warn">不受限</span>' : '');

    let html = '<div class="td-task' + (open ? ' open' : '') + '" data-id="' + esc(t.id) + '">';
    html += '<div class="td-task-head"><div class="td-task-main">';
    html += '<div class="td-badges">' + badges + '</div>';
    html += '<div class="td-name">' + esc(t.title) + '</div>';
    html += '</div><span class="td-caret" title="' + (open ? '收起' : '展开') + '详细信息">' + (open ? '▴' : '▾') + '</span>' +
      (t.done ? '' :
        '<label class="pr-toggle" title="启用 / 停用">' +
        '<input type="checkbox" data-act="toggle" data-id="' + esc(t.id) + '"' + (t.enabled ? ' checked' : '') + '>' +
        '<span></span></label>') + '</div>';

    // 常驻第三行：创建时间（系统生成的只读字段）+ 描述单行摘要（悬停看全文）
    html += '<div class="td-brief"><span class="td-created" title="创建时间（自动生成）">创建于 ' + fmt(t.createdAt) + '</span>' +
      (t.description ? '<span class="td-desc-clip" title="' + esc(t.description) + '">' + esc(t.description) + '</span>' : '') +
      '</div>';

    html += '<div class="td-more">';
    if (t.description) html += '<p class="td-desc">' + esc(t.description) + '</p>';
    if (cmd && t.command) html += '<p class="td-cmd">$ ' + esc(t.command) + '</p>';

    html += '<div class="td-meta">';
    html += '<span><b>' + (cmd ? '工作目录' : '执行目标') + '</b>：' +
      (cmd ? esc(t.dir || '未指定（编辑里选一个）') : esc(t.target === 'task' ? '新的工作任务' : '长期助理')) + '</span>';
    // AI 任务：模型三档 + 当前实际生效值（服务端算好；不可用时红字带原因）。
    // 点这一行＝打开编辑弹窗，直接改模型档位。
    if (!cmd) html += modelMetaHTML(t);
    if (t.enabled && t.schedule.kind === 'manual') html += '<span>不定时 · 手动执行</span>';
    if (t.enabled && t.nextRunAt) html += '<span><b>下次执行</b>：' + fmt(t.nextRunAt) + '</span>';
    html += '<span><b>上次执行</b>：' + fmt(t.lastRunAt) + '</span>';
    html += '</div>';

    if (t.lastStatus) {
      html += '<div class="td-lastline"><i class="td-dot ' + esc(t.lastStatus) + '"></i><span>' +
        esc(STATUS_LABEL[t.lastStatus] || t.lastStatus) + (t.lastError ? '：' + esc(t.lastError) : '') + '</span></div>';
    }

    // 操作按钮同样折在 .td-more 里（默认收起）；已完成页换「重新打开」，不再执行/启停
    const delBtn = '<button type="button" class="' + (st.armedDelete === t.id ? 'danger' : 'ghost') + '" data-act="delete" data-id="' + esc(t.id) + '">' +
      (st.armedDelete === t.id ? '删除这条任务？执行记录会保留' : '删除') + '</button>';
    const recBtn = '<button type="button" class="td-link" data-act="records" data-id="' + esc(t.id) + '">执行记录（' +
      recs.length + '）' + (expanded ? '▴' : '▾') + '</button>';
    const editBtn = '<button type="button" data-act="edit" data-id="' + esc(t.id) + '">编辑</button>';
    html += '<div class="td-actions">' +
      (t.done
        ? '<button type="button" class="primary" data-act="reopen" data-id="' + esc(t.id) + '">重新打开</button>' + editBtn + delBtn + recBtn
        : '<button type="button" class="primary" data-act="run" data-id="' + esc(t.id) + '">立即执行</button>' + editBtn +
          '<button type="button" class="ghost" data-act="done" data-id="' + esc(t.id) + '" title="任务做完：收进「已完成」页并停掉调度">标为完成</button>' +
          delBtn + recBtn) +
      '</div>';

    if (expanded) {
      const recent = recs.slice().sort((a, b) => b.startedAt - a.startedAt).slice(0, 5);
      html += '<div class="td-records">' +
        (recent.length === 0 ? '<div class="td-record">还没有执行记录。</div>' : recent.map((r) => {
        let line = '<div class="td-record"><span class="td-when">' + fmt(r.startedAt) + '</span>' +
          '<i class="td-dot ' + esc(r.status) + '"></i>' +
          '<span>' + esc(TRIGGER_LABEL[r.trigger] || r.trigger) + '</span>' +
          '<span class="td-sum">' + esc(r.summary || r.error || '') + '</span>';
        if (r.sessionId) {
          line += '<button type="button" class="td-link" data-act="session" data-sid="' + esc(r.sessionId) + '">查看会话</button>';
        }
        return line + '</div>';
      }).join('')) + '</div>';
    }
    html += '</div>';
    return html + '</div>';
  }

  /* 卡片「模型：跟随派发设置（生效：DC3 · deepseek-v4.1-flash-1017）」那一行。
     值全部来自服务端 modelView（三档取值链算出的当前实际生效值）；点一下打开编辑
     弹窗改档位。不可用时红字带原因（不静默）。 */
  function modelMetaHTML(t) {
    const mv = t.modelView || {};
    const mode = t.modelMode === 'global' || t.modelMode === 'custom' ? t.modelMode : 'dispatch';
    let line = '模型：' + MODEL_MODE_LABEL[mode];
    if (mv.text) line += '（' + (mv.available ? '生效：' + mv.text : '不可用：' + (mv.error || '')) + '）';
    else line += '（' + (mv.error || '未设置') + '）';
    return '<button type="button" class="td-link td-model' + (mv.available ? '' : ' bad') + '"' +
      ' data-act="edit" data-id="' + esc(t.id) + '" title="改任务模型">' + esc(line) + '</button>';
  }

  function onListClick(e) {
    const el = e.target.closest('button[data-act], input[data-act]');
    if (!el) {
      // 点卡片空白处 = 展开/收起；卡片里的按钮、开关、链接照旧各干各的
      if (e.target.closest('button, input, label, select, a, textarea')) return;
      const card = e.target.closest('.td-task');
      if (!card) return;
      st.open[card.dataset.id] = !st.open[card.dataset.id];
      render();
      return;
    }
    const id = el.dataset.id;
    const task = st.tasks.find((t) => t.id === id);
    switch (el.dataset.act) {
      case 'toggle': void act(() => api('/api/todos/update', {
        method: 'POST', body: JSON.stringify({ id, enabled: el.checked }),
      })); break;
      case 'done': void act(() => api('/api/todos/update', {
        method: 'POST', body: JSON.stringify({ id, done: true }),
      })); break;
      case 'reopen': void act(() => api('/api/todos/update', {
        method: 'POST', body: JSON.stringify({ id, done: false }),
      })); break;
      case 'run': void act(() => runNow(id, task)); break;
      case 'edit': openForm(task); break;
      case 'delete':
        if (st.armedDelete !== id) { st.armedDelete = id; render(); setTimeout(() => { if (st.armedDelete === id) { st.armedDelete = ''; render(); } }, 5000); return; }
        st.armedDelete = '';
        void act(() => api('/api/todos/delete', { method: 'POST', body: JSON.stringify({ id }) }));
        break;
      case 'records': st.expanded[id] = !st.expanded[id]; render(); break;
      case 'session':
        forgetPage();
        if (typeof openSession === 'function') openSession(el.dataset.sid);
        break;
    }
  }

  function onMissedClick(e) {
    const el = e.target.closest('button[data-missed]');
    if (!el) return;
    void act(() => api('/api/todos/missed', {
      method: 'POST', body: JSON.stringify({ id: el.dataset.id, action: el.dataset.missed }),
    }));
  }

  /* 业务动作的统一包装：忙碌态 + 事后刷新；runNow 的业务失败变成页面提示。 */
  async function act(fn) {
    if (st.busy) return;
    st.busy = true;
    notice('');
    try {
      const r = await fn();
      if (r && r.ok === false && r.error) notice(r.error);
    } catch (err) {
      notice(String(err.message || err));
    } finally {
      st.busy = false;
      await load();
    }
  }

  async function runNow(id, task) {
    const r = await api('/api/todos/run', { method: 'POST', body: JSON.stringify({ id }) });
    if (r.ok && r.sessionId && !isCmd(task || {})) {
      notice(task && task.target === 'task'
        ? '已新建工作任务会话（' + r.sessionId + '）并投递执行。'
        : '已转交长期助理（会话 ' + r.sessionId + '）。');
    }
    return r;
  }

  /* ---------- 表单 ---------- */

  /* 表单/卡片共用的模型下拉：按渠道分组（与设置页、对话区模型菜单同一种分组），
     value = provider|keyId|modelId（重名模型靠 Key 归属区分）。 */
  function modelOptions(picked) {
    let html = '<option value="">— 请选择渠道 + 模型 —</option>';
    const groups = new Map();
    for (const m of (st.models || [])) {
      const k = m.providerId + '\u0000' + (m.providerName || m.providerId) + '\u0000' + (m.providerOff ? '1' : '');
      if (!groups.has(k)) groups.set(k, []);
      groups.get(k).push(m);
    }
    for (const [k, list] of groups) {
      const [pid, pname, off] = k.split('\u0000');
      html += '<optgroup label="' + esc(pname) + (off ? '（已停用）' : '') + '">';
      for (const m of list) {
        const v = m.providerId + '|' + (m.keyId || '') + '|' + m.modelId;
        const label = (m.modelName || m.modelId) + (m.keyLabel ? '（' + m.keyLabel + '）' : '');
        html += '<option value="' + esc(v) + '"' + (v === picked ? ' selected' : '') + (off ? ' disabled' : '') + '>' + esc(label) + '</option>';
      }
      html += '</optgroup>';
    }
    return html;
  }

  function blankForm() {
    return {
      editingId: '', title: '', description: '', executor: 'assistant', target: 'manager', kind: 'manual',
      onceAt: '', time: '09:00', days: [false, true, true, true, true, true, false],
      everyMinutes: '30', dir: '', command: '', shell: 'powershell', permission: 'workspace-write',
      enabled: true, modelMode: 'dispatch', modelValue: '',
    };
  }

  function taskToForm(t) {
    const f = blankForm();
    f.editingId = t.id;
    f.title = t.title;
    f.description = t.description;
    f.executor = isCmd(t) ? 'command' : 'assistant';
    f.target = t.target === 'task' ? 'task' : 'manager';
    f.kind = t.schedule.kind;
    if (t.schedule.kind === 'once') f.onceAt = toLocalInput(t.schedule.at);
    if (t.schedule.time) f.time = t.schedule.time;
    if (t.schedule.kind === 'weekly') f.days = [0, 1, 2, 3, 4, 5, 6].map((d) => (t.schedule.days || []).indexOf(d) >= 0);
    if (t.schedule.kind === 'interval') f.everyMinutes = String(t.schedule.everyMinutes);
    f.dir = t.dir || '';
    f.command = t.command || '';
    f.shell = t.shell || 'powershell';
    f.permission = t.permission || 'workspace-write';
    f.enabled = t.enabled;
    // 任务模型三档（老数据没这字段＝跟随派发设置）；选定值存 provider|key|model
    f.modelMode = t.modelMode === 'global' || t.modelMode === 'custom' ? t.modelMode : 'dispatch';
    const mr = t.model || {};
    f.modelValue = mr.provider ? mr.provider + '|' + (mr.providerKey || '') + '|' + mr.model : '';
    return f;
  }

  function formRequest(f) {
    let schedule;
    if (f.kind === 'manual') schedule = { kind: 'manual' };
    else if (f.kind === 'once') schedule = { kind: 'once', at: new Date(f.onceAt).getTime() };
    else if (f.kind === 'interval') schedule = { kind: 'interval', everyMinutes: Number(f.everyMinutes) };
    else if (f.kind === 'weekly') schedule = { kind: 'weekly', time: f.time, days: f.days.map((on, d) => (on ? d : -1)).filter((d) => d >= 0) };
    else schedule = { kind: f.kind, time: f.time };
    const custom = f.executor === 'assistant' && f.modelMode === 'custom';
    const parts = custom && f.modelValue ? f.modelValue.split('|') : [];
    return {
      id: f.editingId, title: f.title.trim(), description: f.description.trim(),
      executor: f.executor, target: f.executor === 'assistant' ? f.target : '', schedule,
      dir: f.executor === 'command' ? f.dir : '',
      command: f.executor === 'command' ? f.command.trim() : '',
      shell: f.shell, permission: f.permission, enabled: f.enabled,
      // 任务模型：dispatch/global/custom；只有 custom 带具体指向（含 Key 归属）
      modelMode: f.executor === 'assistant' ? f.modelMode : '',
      model: custom ? { provider: parts[0] || '', providerKey: parts[1] || '', model: parts[2] || '' } : null,
    };
  }

  function formError(f) {
    if (!f.title.trim()) return '请填写标题';
    if (!f.description.trim()) return '请填写详细描述 / 要求';
    if (f.executor === 'assistant' && f.modelMode === 'custom' && !f.modelValue) return '「指定模型」需要选一个渠道 + 模型';
    if (f.executor === 'command') {
      if (!f.command.trim()) return '程序任务需要填写要运行的命令';
      if (!f.dir) return '程序任务必须选择工作目录';
    }
    if (f.kind === 'once' && !f.onceAt) return '请选择执行时间';
    if (f.kind === 'weekly' && !f.days.some((d) => d)) return '请至少勾选一个星期';
    if (f.kind === 'interval' && !(Number.isInteger(Number(f.everyMinutes)) && Number(f.everyMinutes) >= 1)) return '请填写 1 以上的间隔分钟数';
    return '';
  }

  function recalcOnceDefault(f) {
    if (!f.onceAt) {
      const d = new Date(Date.now() + 30 * 60 * 1000);
      d.setSeconds(0, 0);
      f.onceAt = toLocalInput(d.getTime());
    }
  }

  async function openForm(task) {
    st.editing = task ? taskToForm(task) : blankForm();
    if (st.editing.kind === 'once') recalcOnceDefault(st.editing);
    try {
      const d = await api('/api/todos/workspaces');
      st.workspaces = d.items || [];
    } catch (e) { st.workspaces = []; }
    // 模型清单：「指定模型」下拉按渠道分组（与设置页同一套模型清单）
    try {
      const d = await api('/api/models');
      st.models = d.models || [];
    } catch (e) { st.models = []; }
    if (!st.editing.dir && st.workspaces.length) st.editing.dir = st.workspaces[0].path;
    renderForm();
    formEl.classList.remove('hidden');
  }

  function closeForm() {
    st.editing = null;
    if (formEl) formEl.classList.add('hidden');
  }

  function renderForm() {
    const f = st.editing;
    if (!f) return;
    const cmd = f.executor === 'command';
    const card = formEl.querySelector('#td-form-card');
    // 字段容器用 div 而非 label：宿主 .modal-card label 的规则会把控件撑成整行、文字挤到下一行
    const field = (label, inner, span2) =>
      '<div class="td-field' + (span2 ? ' span2' : '') + '"><span class="sf-label">' + label + '</span>' + inner + '</div>';
    const kindOptions = KINDS.map((k) =>
      '<option value="' + k + '"' + (f.kind === k ? ' selected' : '') + '>' + KIND_LABEL[k] + '</option>').join('');
    const wsOptions = st.workspaces.map((w) =>
      '<option value="' + esc(w.path) + '"' + (f.dir === w.path ? ' selected' : '') + '>' + esc(w.path) + '</option>').join('');

    card.innerHTML =
      '<h3>' + (f.editingId ? '编辑任务' : '新建任务') + '</h3>' +
      '<div class="td-grid">' +
        field('任务类型',
          '<div class="td-seg">' +
            '<button type="button" class="' + (cmd ? '' : 'primary') + '" data-f="executor" data-v="assistant">AI 任务</button>' +
            '<button type="button" class="' + (cmd ? 'primary' : '') + '" data-f="executor" data-v="command">程序任务</button>' +
          '</div>' +
          '<p class="td-hint">' + (cmd
            ? '到点在工作目录直接运行命令（不经过模型），退出码和输出记进执行记录。'
            : '到点把描述交给「执行目标」处理：长期助理由它安排执行并汇报；新的工作任务则开一个独立会话直接执行。') + '</p>', true) +
        (!cmd
          ? field('执行目标',
              '<div class="td-seg">' +
                '<button type="button" class="' + (f.target === 'task' ? '' : 'primary') + '" data-f="target" data-v="manager">长期助理</button>' +
                '<button type="button" class="' + (f.target === 'task' ? 'primary' : '') + '" data-f="target" data-v="task">新的工作任务</button>' +
              '</div>' +
              '<p class="td-hint">' + (f.target === 'task'
                ? '到点自动创建一个新的工作任务会话，任务描述直接发给它执行；长期助理不被打扰，结果在工作任务会话和这里的执行记录里看。'
                : '到点把任务描述转交给长期助理（数字助理主对话），由它安排执行并在完成后汇报结果。') + '</p>', true)
          : '') +
        (!cmd ? field('任务模型',
          '<div class="td-seg">' +
            '<button type="button" class="' + (f.modelMode === 'dispatch' ? 'primary' : '') + '" data-f="modelmode" data-v="dispatch">跟随派发设置</button>' +
            '<button type="button" class="' + (f.modelMode === 'global' ? 'primary' : '') + '" data-f="modelmode" data-v="global">全局默认模型</button>' +
            '<button type="button" class="' + (f.modelMode === 'custom' ? 'primary' : '') + '" data-f="modelmode" data-v="custom">指定模型</button>' +
          '</div>' +
          (f.modelMode === 'custom'
            ? '<select id="td-f-model">' + modelOptions(f.modelValue) + '</select>'
            : '<p class="td-hint">' + (f.modelMode === 'global'
              ? '用「设置 → 模型 → 默认模型」里的全局默认模型。'
              : '按派发设置为准；派发设置是「跟随主会话」时（待办没有主会话可跟）落到全局默认模型。') + '</p>'), true)
          : '') +
        field('标题',
          '<input id="td-f-title" type="text" placeholder="例如：每周整理一次周报" value="' + esc(f.title) + '">', true) +
        field('详细描述 / 要求',
          '<textarea id="td-f-desc" rows="4">' + esc(f.description) + '</textarea>' +
          '<p class="td-hint">AI 任务：原样转交数字助理；程序任务：作为备注留在任务里。请写成自足的一段话。</p>', true) +
        field('执行方式', '<select id="td-f-kind">' + kindOptions + '</select>') +
        (f.kind === 'once'
          ? field('执行时间', '<input id="td-f-once" type="datetime-local" value="' + esc(f.onceAt) + '">')
          : (f.kind === 'daily' || f.kind === 'weekly' || f.kind === 'weekdays'
            ? field('时刻', '<input id="td-f-time" type="time" value="' + esc(f.time) + '">')
            : (f.kind === 'interval'
              ? field('间隔分钟数', '<input id="td-f-every" type="number" min="1" value="' + esc(f.everyMinutes) + '">')
              : ''))) +
        (f.kind === 'weekly'
          ? field('星期', '<div class="td-days">' + WEEK.map((name, d) =>
              '<label class="td-day"><input type="checkbox" data-d="' + d + '"' + (f.days[d] ? ' checked' : '') + '>' + name + '</label>').join('') +
            '</div>', true)
          : '') +
        (cmd
          ? field('命令', '<input id="td-f-cmd" type="text" placeholder="例如：git pull" value="' + esc(f.command) + '">') +
            field('终端', '<select id="td-f-shell">' +
              '<option value="powershell"' + (f.shell === 'powershell' ? ' selected' : '') + '>PowerShell</option>' +
              '<option value="cmd"' + (f.shell === 'cmd' ? ' selected' : '') + '>CMD</option></select>')
          : '') +
        (cmd
          ? field('工作目录',
              '<select id="td-f-dir-sel"><option value="">— 从常用目录里挑 —</option>' + wsOptions + '</select>' +
              '<div class="td-dir-row"><input id="td-f-dir" type="text" placeholder="工作目录绝对路径" value="' + esc(f.dir) + '">' +
              '<button type="button" id="td-f-pick">浏览…</button></div>', true) +
            field('权限', '<select id="td-f-perm">' +
              '<option value="workspace-write"' + (f.permission !== 'full' ? ' selected' : '') + '>沙箱内（只能写工作目录，推荐）</option>' +
              '<option value="full"' + (f.permission === 'full' ? ' selected' : '') + '>完全访问（不受限，谨慎）</option></select>', true)
          : '') +
        field('启用', '<label class="pr-toggle"><input id="td-f-enabled" type="checkbox"' + (f.enabled ? ' checked' : '') + '><span></span></label>') +
      '</div>' +
      '<p id="td-form-err" class="td-err hidden"></p>' +
      '<div class="modal-actions"><button type="button" id="td-form-cancel">取消</button>' +
      '<button type="button" id="td-form-save" class="accent">保存</button></div>';

    // 表单内的交互：输入即时回写 st.editing，结构变化（执行方式/类型）重绘
    card.querySelectorAll('input[type=text], input[type=password], textarea, input[type=number], input[type=time], input[type=datetime-local]')
      .forEach((el) => { el.oninput = () => { readForm(); }; });
    card.querySelectorAll('select').forEach((el) => { el.onchange = () => { readForm(); rerenderOnKind(); }; });
    card.querySelectorAll('input[type=checkbox][data-d]').forEach((el) => {
      el.onchange = () => { st.editing.days[Number(el.dataset.d)] = el.checked; };
    });
    card.querySelectorAll('button[data-f=executor]').forEach((el) => {
      el.onclick = () => { readForm(); st.editing.executor = el.dataset.v; renderForm(); };
    });
    card.querySelectorAll('button[data-f=target]').forEach((el) => {
      el.onclick = () => { readForm(); st.editing.target = el.dataset.v; renderForm(); };
    });
    card.querySelectorAll('button[data-f=modelmode]').forEach((el) => {
      el.onclick = () => { readForm(); st.editing.modelMode = el.dataset.v; renderForm(); };
    });
    const pick = card.querySelector('#td-f-pick');
    if (pick) {
      pick.onclick = async () => {
        try {
          readForm();
          const r = await api('/api/pick-folder' + (st.editing.dir ? '?start=' + encodeURIComponent(st.editing.dir) : ''));
          if (r && r.path) { st.editing.dir = r.path; renderForm(); }
        } catch (e) { showFormErr('无法打开文件夹选择框：' + (e.message || e)); }
      };
    }
    const dirSel = card.querySelector('#td-f-dir-sel');
    if (dirSel) {
      // 只作「常用目录」快捷填入，真正的值始终在工作目录输入框里
      dirSel.onchange = () => {
        if (!dirSel.value) return;
        readForm();
        st.editing.dir = dirSel.value;
        renderForm();
      };
    }
    card.querySelector('#td-form-cancel').onclick = closeForm;
    card.querySelector('#td-form-save').onclick = () => { void saveForm(); };
  }

  /* 从表单控件回读用户输入（重绘前必须先调用，否则输入丢失）。 */
  function readForm() {
    const f = st.editing;
    const card = formEl.querySelector('#td-form-card');
    const val = (id) => { const el = card.querySelector('#' + id); return el ? el.value : ''; };
    f.title = val('td-f-title');
    f.description = val('td-f-desc');
    f.kind = val('td-f-kind') || f.kind;
    if (f.kind === 'once' && card.querySelector('#td-f-once')) f.onceAt = val('td-f-once');
    if (card.querySelector('#td-f-time')) f.time = val('td-f-time');
    if (card.querySelector('#td-f-every')) f.everyMinutes = val('td-f-every');
    if (card.querySelector('#td-f-cmd')) f.command = val('td-f-cmd');
    if (card.querySelector('#td-f-shell')) f.shell = val('td-f-shell');
    if (card.querySelector('#td-f-perm')) f.permission = val('td-f-perm');
    if (card.querySelector('#td-f-dir')) f.dir = val('td-f-dir').trim();
    if (card.querySelector('#td-f-model')) f.modelValue = val('td-f-model');
    const on = card.querySelector('#td-f-enabled');
    if (on) f.enabled = on.checked;
  }

  /* 执行方式/任务类型变了就重绘（控件结构随之变化）；普通键入不重绘，避免打断输入。 */
  function rerenderOnKind() {
    readForm();
    if (st.editing.kind === 'once') recalcOnceDefault(st.editing);
    renderForm();
  }

  function showFormErr(msg) {
    const el = formEl.querySelector('#td-form-err');
    if (!el) return;
    el.textContent = msg;
    el.classList.toggle('hidden', !msg);
  }

  async function saveForm() {
    const f = st.editing;
    if (!f) return;
    const problem = formError(f);
    if (problem) { showFormErr(problem); return; }
    const body = formRequest(f);
    try {
      if (f.editingId) await api('/api/todos/update', { method: 'POST', body: JSON.stringify(body) });
      else await api('/api/todos', { method: 'POST', body: JSON.stringify(body) });
      closeForm();
      await load();
    } catch (e) {
      showFormErr(String(e.message || e));
    }
  }

  /* ---------- 事件流（调度触发的执行、模型改清单都即时反映；页签保持当前所在） ---------- */

  function openStream() {
    if (sse || typeof EventSource !== 'function') return;
    try {
      sse = new EventSource('/api/events');
      sse.onmessage = (e) => {
        try {
          const ev = JSON.parse(e.data);
          if (ev.type !== 'todo_tasks') return;
          if (st.page) void load();
        } catch (err) { /* 坏帧跳过 */ }
      };
    } catch (e) { sse = null; }
  }

  function closeStream() {
    if (sse) { try { sse.close(); } catch (e) { /* 已关闭 */ } sse = null; }
  }

  /* ---------- 全局键盘 ---------- */

  function onKey(e) {
    // 本页已经不是一个浮窗，Esc 只关表单弹窗，退出页面走侧栏（与对话页一致）
    if (e.key !== 'Escape') return;
    if (formEl && !formEl.classList.contains('hidden')) closeForm();
  }

  /* ---------- 注册 ---------- */

  CX.register({
    id: ID,
    init() {
      disposing = false;
      mountButton();
      mountForm();
      document.addEventListener('keydown', onKey);
    },
    dispose() {
      disposing = true;
      // 关组件时若正停在本页：把主列还给 app.js（回原会话或首页），再拆自己的东西
      if (st.page) {
        const back = st.prevSession;
        forgetPage();
        if (back && typeof openSession === 'function') void openSession(back);
        else if (typeof goHomeNewTask === 'function') goHomeNewTask();
      }
      closeStream();
      document.removeEventListener('keydown', onKey);
      if (btn) { btn.remove(); btn = null; }
      if (formEl) { formEl.remove(); formEl = null; }
      st.tasks = []; st.records = []; st.missed = []; st.editing = null;
      st.open = {}; st.expanded = {}; st.page = false; st.prevSession = null;
    },
  });
})();
