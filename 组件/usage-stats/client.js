// 组件 usage-stats「用量统计」的浏览器半边：
//   1. 设置 → 用量 独立页（CX.registerSettings 带 pane）：渠道/日期筛选、
//      KPI 卡片、每日趋势（手写 SVG 折线）、模型占比（环形图）、按模型消耗表、
//      用量明细分页表、单价与时段规则配置（规则按模型一条、可复制到其它模型）、按日期范围清理；
//   2. 输入框下方的实时输出速度条：`↑ 42.3 tok/s · 本轮 1.2k tok`——流式过程中
//      按已收字符估 token（与后端 estimateTokens 同一启发式 3/4），每步结束后用
//      后端推来的真实 usage 定格，定格约 3 秒淡出。首字延迟不在这里显示（在
//      用量记录表的「首字延迟」列里），这是用户定的口径。
//
// 数据来自主程序的 /api/usage/*（usageapi.go）。采集本身在主程序（usagestats.go），
// 由本组件的开关控制：关掉即停记、已有数据保留、页面与速度条一起消失——所以
// dispose 必须把自己造的元素与监听摘干净。
// 设置页的 DOM 全部用全站类名 + 本组件 client.css 的 us-* 类，深浅色主题与
// 窄视口随全站变量自适应，不依赖其它组件的样式。
(function () {
  'use strict';

  var ID = 'usage-stats';
  var PAGE_SIZE = 50;
  var SPEED_FADE_MS = 3000; // 结束定格时长（用户要求约 3 秒）
  var SPEED_FADE_ANIM = 500;

  var PURPOSE_LABEL = {
    main: '主对话',
    task: '任务会话',
    sub: '子任务',
    aux: '辅助对话',
    title: '标题生成',
    compact: '压缩',
    assist: '辅助生成',
  };

  var PROTO_LABEL = {
    chat: 'chat',
    responses: 'responses',
    anthropic: 'anthropic',
  };

  // 思考档位显示名：与主界面「思考档位」菜单同一套（models.go 的 thinkingLevels）。
  // 库里存内部值，空串＝这次调用没指定档位（跟随服务端默认），界面写「默认」。
  var THINK_LEVELS = ['none', 'low', 'medium', 'high', 'xhigh', 'max'];
  var THINK_LABELS = { none: 'None', low: 'Low', medium: 'Medium', high: 'High', xhigh: 'Xhigh', max: 'Max' };
  var THINK_DEFAULT = 'default'; // 筛选里「默认」档的哨兵值（后端 usageThinkingDefault）

  var DONUT_COLORS = ['#4d6bfe', '#1f9d55', '#e8a33d', '#9a6700', '#a855f7', '#0ea5e9', '#8b949e'];

  // 页面状态（模块级：切走再回来保留筛选与分页）
  var state = {
    range: '7d', // today | 7d | 30d | custom
    from: '',
    to: '',
    provider: '',
    key: '',
    thinking: '', // 推理强度筛选：'' 全部 / 'default' 未指定档位 / 六档之一
    page: 0,
    searchPage: 0,
    pricing: null,
    pricingDirty: false,
  };

  /* ================= 小工具 ================= */

  function el(tag, cls, text) {
    var n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text != null) n.textContent = text;
    return n;
  }

  function fmtInt(v) {
    v = Math.round(Number(v) || 0);
    return String(v).replace(/\B(?=(\d{3})+(?!\d))/g, ',');
  }

  // fmtNum 紧凑计数（KPI 用）：1.2k / 3.45M，四位以下原样
  function fmtNum(v) {
    v = Number(v) || 0;
    var a = Math.abs(v);
    if (a >= 1e9) return (v / 1e9).toFixed(2) + 'B';
    if (a >= 1e6) return (v / 1e6).toFixed(2) + 'M';
    if (a >= 1e4) return (v / 1e3).toFixed(1) + 'k';
    return fmtInt(v);
  }

  function fmtCost(v) {
    v = Number(v) || 0;
    if (v === 0) return '¥0';
    if (Math.abs(v) < 0.01) return '¥' + v.toFixed(4);
    if (Math.abs(v) < 1) return '¥' + v.toFixed(3);
    return '¥' + v.toFixed(2);
  }

  function fmtPct(v) {
    v = Number(v) || 0;
    return (Math.round(v * 10) / 10).toFixed(1) + '%';
  }

  function fmtMs(ms) {
    ms = Number(ms) || 0;
    if (ms <= 0) return '—';
    if (ms < 1000) return ms + ' ms';
    return (ms / 1000).toFixed(ms < 10000 ? 2 : 1) + ' s';
  }

  function fmtTime(ts) {
    var s = String(ts || '');
    // 'YYYY-MM-DD HH:MM:SS' → 'MM-DD HH:MM'（当年记录不必重复年份）
    if (s.length >= 16) return s.slice(5, 16);
    return s;
  }

  function todayStr(offsetDays) {
    var d = new Date();
    if (offsetDays) d.setDate(d.getDate() + offsetDays);
    var m = String(d.getMonth() + 1).padStart(2, '0');
    var day = String(d.getDate()).padStart(2, '0');
    return d.getFullYear() + '-' + m + '-' + day;
  }

  function dateRange() {
    switch (state.range) {
      case 'today': return { from: todayStr(0), to: todayStr(0) };
      case '30d': return { from: todayStr(-29), to: todayStr(0) };
      case 'custom': return { from: state.from || todayStr(-6), to: state.to || todayStr(0) };
      default: return { from: todayStr(-6), to: todayStr(0) };
    }
  }

  function apiErr(res, text) {
    try {
      var j = JSON.parse(text || '');
      if (j && j.error) return j.error;
    } catch (e) { /* 非 JSON 错误体 */ }
    return 'HTTP ' + res.status;
  }

  function apiGet(path) {
    return fetch(path, { cache: 'no-store' }).then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error(apiErr(res, t)); });
      return res.json();
    });
  }

  function apiPost(path, body) {
    return fetch(path, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body || {}),
    }).then(function (res) {
      if (!res.ok) return res.text().then(function (t) { throw new Error(apiErr(res, t)); });
      return res.json();
    });
  }

  /* ================= 设置页 ================= */

  // ctx 是本次 mount 的 DOM 句柄集合（每次打开设置重建，数据取 state）
  function mountSettings(root) {
    root.innerHTML = '';
    var wrap = document.createElement('div');
    wrap.className = 'us-wrap';
    root.appendChild(wrap);

    var ctx = { root: wrap };

    wrap.appendChild(el('p', 'us-note',
      '这里记录每次模型调用的明细（会话与模型都会产生记录），以及联网搜索/抽取的按次调用；' +
      '花费按本地单价估算，模型按 token 计、搜索按次计，两块各自合计后再相加。'));

    // ---- 筛选行 ----
    var filters = el('div', 'us-filters');
    var selChannel = el('select');
    selChannel.title = '按渠道筛选（多 Key 渠道按「渠道 · Key 备注」分开选）';
    var labCh = el('label', '', '渠道');
    labCh.appendChild(selChannel);
    filters.appendChild(labCh);

    var selRange = el('select');
    [['today', '今天'], ['7d', '近 7 天'], ['30d', '近 30 天'], ['custom', '自定义']].forEach(function (p) {
      var o = el('option', '', p[1]);
      o.value = p[0];
      selRange.appendChild(o);
    });
    var labRange = el('label', '', '日期');
    labRange.appendChild(selRange);
    filters.appendChild(labRange);

    // 推理强度筛选：全量选项固定六档 + 「默认」（没指定档位：老数据与辅助调用）
    var selThink = el('select');
    selThink.title = '按本次调用实际使用的思考档位筛选（「默认」＝调用时没指定档位，跟随服务端默认）';
    var oAllThink = el('option', '', '全部推理强度');
    oAllThink.value = '';
    selThink.appendChild(oAllThink);
    var oDefThink = el('option', '', '默认');
    oDefThink.value = THINK_DEFAULT;
    selThink.appendChild(oDefThink);
    THINK_LEVELS.forEach(function (lv) {
      var o = el('option', '', THINK_LABELS[lv]);
      o.value = lv;
      selThink.appendChild(o);
    });
    var labThink = el('label', '', '推理强度');
    labThink.appendChild(selThink);
    filters.appendChild(labThink);

    var inFrom = el('input');
    inFrom.type = 'date';
    var inTo = el('input');
    inTo.type = 'date';
    var customBox = el('label', '', '');
    customBox.appendChild(inFrom);
    customBox.appendChild(el('span', '', '至'));
    customBox.appendChild(inTo);
    filters.appendChild(customBox);

    var btnRefresh = el('button', 'us-btn', '刷新');
    btnRefresh.type = 'button';
    filters.appendChild(btnRefresh);
    wrap.appendChild(filters);

    // ---- KPI ----
    var kpis = el('div', 'us-kpis');
    wrap.appendChild(kpis);

    // ---- 趋势 ----
    var cardTrend = el('div', 'us-card');
    var hTrend = el('h4', '', '每日用量趋势');
    hTrend.appendChild(el('span', 'h-sub', '输入 / 输出 tokens'));
    cardTrend.appendChild(hTrend);
    var legend = el('div', 'us-legend');
    var li1 = el('span');
    li1.innerHTML = '<i style="background:#4d6bfe"></i>输入 tokens';
    var li2 = el('span');
    li2.innerHTML = '<i style="background:#1f9d55"></i>输出 tokens';
    legend.appendChild(li1);
    legend.appendChild(li2);
    cardTrend.appendChild(legend);
    var chartBox = el('div', 'us-chart');
    cardTrend.appendChild(chartBox);
    wrap.appendChild(cardTrend);

    // ---- 模型占比 + 按模型统计 ----
    var grid = el('div', 'us-grid-2');
    var cardDonut = el('div', 'us-card');
    cardDonut.appendChild(el('h4', '', '模型占比'));
    var donutBox = el('div', 'us-donut');
    cardDonut.appendChild(donutBox);
    var cardModel = el('div', 'us-card');
    cardModel.appendChild(el('h4', '', '按模型消耗统计'));
    var modelTable = el('div', 'us-table-wrap');
    cardModel.appendChild(modelTable);
    grid.appendChild(cardDonut);
    grid.appendChild(cardModel);
    wrap.appendChild(grid);
    wrap.appendChild(el('p', 'us-note', '费用为「按本地单价估算，不代表账单」：基础价按模型配置，再乘渠道倍率。'));

    // ---- 用量记录 ----
    var cardRecords = el('div', 'us-card');
    var hRec = el('h4', '', '用量记录');
    hRec.appendChild(el('span', 'h-sub', '每次模型调用一条，最新在前'));
    cardRecords.appendChild(hRec);
    var recTable = el('div', 'us-table-wrap');
    cardRecords.appendChild(recTable);
    var pager = el('div', 'us-pager');
    cardRecords.appendChild(pager);
    wrap.appendChild(cardRecords);

    // ---- 搜索调用（按次计费，与 token 类用量分开合计）----
    var cardSearch = el('div', 'us-card');
    var hSearch = el('h4', '', '搜索调用');
    hSearch.appendChild(el('span', 'h-sub', '按次计费 · 失败计数不计费'));
    cardSearch.appendChild(hSearch);
    cardSearch.appendChild(el('p', 'us-note',
      '联网搜索与正文抽取的供应商调用：每次回落尝试一行（第 1 家失败、第 2 家成功时是两行），' +
      '所以「次数」含失败与回落。这里自本次升级起统计（此前的搜索调用没有记录）。'));
    var searchKpis = el('div', 'us-kpis');
    cardSearch.appendChild(searchKpis);
    var searchProviderTable = el('div', 'us-table-wrap');
    cardSearch.appendChild(searchProviderTable);
    var searchRecTable = el('div', 'us-table-wrap');
    cardSearch.appendChild(searchRecTable);
    var searchPager = el('div', 'us-pager');
    cardSearch.appendChild(searchPager);
    wrap.appendChild(cardSearch);

    // ---- 单价与规则 ----
    var cardPrice = el('div', 'us-card');
    cardPrice.appendChild(el('h4', '', '单价与时段规则'));
    cardPrice.appendChild(el('p', 'us-note',
      '模型单价单位「元 / 百万 tokens」（填的是峰值价），由你自己填：程序不内置任何模型价，' +
      '没填之前费用一律显示「未配单价」，不给兜底价。计费口径：未命中输入×输入价 + 缓存读取×缓存读价 + ' +
      '缓存写入×缓存写价 + 输出×输出价，命中缓存的输入不再按输入价重复计费。' +
      '时段规则按模型配（高峰星期 / 高峰时段 / 节假日 / 空闲折扣），按调用发生的时刻判定；' +
      '一个时段都不配＝不分时段（全天按高峰价）。' +
      '渠道倍率默认 1.0，费用 = 算出的金额 × 倍率（同一模型挂不同中转站不必逐个配价）。'));
    var priceGrid = el('div', 'us-price-grid');
    cardPrice.appendChild(priceGrid);
    var statusLine = el('p', 'us-status');
    statusLine.textContent = '';
    cardPrice.appendChild(statusLine);
    wrap.appendChild(cardPrice);

    // ---- 清理 ----
    var cardClean = el('div', 'us-card us-danger-zone');
    cardClean.appendChild(el('h4', '', '清理用量数据'));
    cardClean.appendChild(el('p', 'us-note',
      '删除指定日期范围内（含首尾）的用量明细。删除后不可恢复，也无法重新生成——只影响用量统计，不影响会话与消息。'));
    var cleanBar = el('div', 'us-filters');
    var cFrom = el('input');
    cFrom.type = 'date';
    cFrom.value = todayStr(-30);
    var cTo = el('input');
    cTo.type = 'date';
    cTo.value = todayStr(0);
    var labClean = el('label', '', '范围');
    labClean.appendChild(cFrom);
    labClean.appendChild(el('span', '', '至'));
    labClean.appendChild(cTo);
    cleanBar.appendChild(labClean);
    var btnClean = el('button', 'us-btn danger', '删除该范围的用量数据');
    btnClean.type = 'button';
    cleanBar.appendChild(btnClean);
    cardClean.appendChild(cleanBar);
    var cleanStatus = el('p', 'us-status');
    cardClean.appendChild(cleanStatus);
    wrap.appendChild(cardClean);

    // ---- 事件 ----
    selChannel.onchange = function () {
      var v = selChannel.value || '';
      var parts = v.split('\u0000');
      state.provider = parts[0] || '';
      state.key = parts[1] || '';
      state.page = 0;
      refreshData(ctx);
    };
    selRange.onchange = function () {
      state.range = selRange.value;
      if (state.range === 'custom') {
        if (!inFrom.value) inFrom.value = todayStr(-6);
        if (!inTo.value) inTo.value = todayStr(0);
        state.from = inFrom.value;
        state.to = inTo.value;
      }
      customBox.style.display = state.range === 'custom' ? '' : 'none';
      state.page = 0;
      refreshData(ctx);
    };
    inFrom.onchange = inTo.onchange = function () {
      state.from = inFrom.value;
      state.to = inTo.value;
      state.page = 0;
      refreshData(ctx);
    };
    selThink.onchange = function () {
      state.thinking = selThink.value || '';
      state.page = 0;
      refreshData(ctx);
    };
    btnRefresh.onclick = function () { refreshAll(ctx); };
    btnClean.onclick = function () {
      var from = cFrom.value;
      var to = cTo.value;
      if (!from || !to) { setStatus(cleanStatus, '请先选完整的起止日期', true); return; }
      var lo = from <= to ? from : to;
      var hi = from <= to ? to : from;
      var ok = window.confirm('确定删除 ' + lo + ' 至 ' + hi + ' 的用量明细吗？\n\n删除后不可恢复。');
      if (!ok) return;
      btnClean.disabled = true;
      apiPost('/api/usage/cleanup', { from: lo, to: hi }).then(function (r) {
        setStatus(cleanStatus, '已删除 ' + fmtInt(r.deleted) + ' 条（' + lo + ' ~ ' + hi + '）', false);
        refreshData(ctx);
      }).catch(function (e) {
        setStatus(cleanStatus, '删除失败：' + (e && e.message ? e.message : e), true);
      }).then(function () { btnClean.disabled = false; });
    };

    // 初值：筛选控件按 state 回填
    selRange.value = state.range;
    selThink.value = state.thinking || '';
    if (state.range === 'custom') {
      var r0 = dateRange();
      inFrom.value = state.from || r0.from;
      inTo.value = state.to || r0.to;
      customBox.style.display = '';
    } else {
      customBox.style.display = 'none';
    }

    ctx.selChannel = selChannel;
    ctx.kpis = kpis;
    ctx.chartBox = chartBox;
    ctx.donutBox = donutBox;
    ctx.modelTable = modelTable;
    ctx.recTable = recTable;
    ctx.pager = pager;
    ctx.searchKpis = searchKpis;
    ctx.searchProviderTable = searchProviderTable;
    ctx.searchRecTable = searchRecTable;
    ctx.searchPager = searchPager;
    ctx.priceGrid = priceGrid;
    ctx.statusLine = statusLine;

    refreshAll(ctx);
  }

  function setStatus(node, text, isErr) {
    if (!node) return;
    node.textContent = text || '';
    node.className = 'us-status' + (text ? (isErr ? ' err' : ' ok') : '');
  }

  // refreshAll 先拉单价配置（渠道下拉与倍率表要用），再拉汇总与明细
  function refreshAll(ctx) {
    apiGet('/api/usage/pricing').then(function (p) {
      state.pricing = p;
      renderChannelOptions(ctx, p);
      renderPricing(ctx, p);
      return refreshData(ctx);
    }).catch(function (e) {
      setStatus(ctx.statusLine, '读取配置失败：' + (e && e.message ? e.message : e), true);
    });
  }

  function renderChannelOptions(ctx, p) {
    var sel = ctx.selChannel;
    var cur = state.provider ? state.provider + '\u0000' + state.key : '';
    sel.innerHTML = '';
    var oAll = el('option', '', '全部渠道');
    oAll.value = '';
    sel.appendChild(oAll);
    (p.providers || []).forEach(function (pv) {
      var keys = (pv.keys || []).filter(function (k) { return k.id; });
      if (keys.length > 1) {
        keys.forEach(function (k, i) {
          var o = el('option', '', pv.name + ' · ' + (k.label || ('Key' + (i + 1))));
          o.value = pv.id + '\u0000' + k.id;
          sel.appendChild(o);
        });
      } else {
        var o2 = el('option', '', pv.name);
        o2.value = pv.id + '\u0000';
        sel.appendChild(o2);
      }
    });
    // 回填当前选择；选择已不在清单里（渠道被删）就退回「全部渠道」
    var found = false;
    for (var i = 0; i < sel.options.length; i++) {
      if (sel.options[i].value === cur) { found = true; break; }
    }
    sel.value = found ? cur : '';
    if (!found) { state.provider = ''; state.key = ''; }
  }

  function refreshData(ctx) {
    var r = dateRange();
    var q = 'from=' + encodeURIComponent(r.from) + '&to=' + encodeURIComponent(r.to);
    if (state.provider) q += '&provider=' + encodeURIComponent(state.provider);
    if (state.key) q += '&key=' + encodeURIComponent(state.key);
    if (state.thinking) q += '&thinking=' + encodeURIComponent(state.thinking);

    apiGet('/api/usage/summary?' + q).then(function (d) {
      renderSummary(ctx, d);
    }).catch(function (e) {
      setStatus(ctx.statusLine, '读取汇总失败：' + (e && e.message ? e.message : e), true);
    });

    var rq = q + '&limit=' + PAGE_SIZE + '&offset=' + (state.page * PAGE_SIZE);
    apiGet('/api/usage/records?' + rq).then(function (d) {
      renderRecords(ctx, d);
    }).catch(function (e) {
      setStatus(ctx.statusLine, '读取明细失败：' + (e && e.message ? e.message : e), true);
    });

    // 搜索调用只按日期筛（渠道筛选是模型渠道，搜索供应商不是同一套 id）
    var sq = 'from=' + encodeURIComponent(r.from) + '&to=' + encodeURIComponent(r.to) +
      '&limit=' + PAGE_SIZE + '&offset=' + (state.searchPage * PAGE_SIZE);
    apiGet('/api/usage/search?' + sq).then(function (d) {
      renderSearch(ctx, d);
    }).catch(function (e) {
      setStatus(ctx.statusLine, '读取搜索用量失败：' + (e && e.message ? e.message : e), true);
    });
  }

  /* ---------- KPI ---------- */

  // costBreakdown 「估算花费」卡的副说明：高峰/空闲两档的次数与金额分开列。
  // 只给总额的话，时段规则有没有真生效是看不出来的（2026-11-12 用户实测的坑）。
  function costBreakdown(k) {
    var parts = [];
    if (!k.ruleReady) parts.push('未配置时段规则');
    parts.push('高峰 ' + fmtInt(k.peakCalls) + ' 次 ' + fmtCost(k.peakCost));
    parts.push('空闲 ' + fmtInt(k.idleCalls) + ' 次 ' + fmtCost(k.idleCost));
    if (!k.priced) parts.push('有 ' + fmtInt(k.unpricedCalls) + ' 次未配单价');
    return parts.join(' · ');
  }

  function renderSummary(ctx, d) {
    var k = d.kpi || {};
    var cards = [
      { label: '缓存命中率', val: fmtPct(k.cacheHitRate), sub: '命中读取 / 输入' },
      { label: '总 tokens', val: fmtNum(k.total), sub: '输入 + 输出' },
      { label: '输入 tokens', val: fmtNum(k.input), sub: '' },
      { label: '输出 tokens', val: fmtNum(k.output), sub: '' },
      { label: '缓存写入', val: fmtNum(k.cacheWrite), sub: 'cache_creation' },
      { label: '缓存读取', val: fmtNum(k.cacheRead), sub: '命中缓存' },
      { label: '总请求数', val: fmtInt(k.calls), sub: '' },
      {
        label: '成功率',
        val: k.calls > 0 ? fmtPct(k.successRate) : '—',
        sub: k.errors > 0 ? '错误 ' + fmtInt(k.errors) + ' 次' : '无错误',
        alert: k.errors > 0,
      },
      {
        // 未配单价且一分钱都算不出来：直接写「未配单价」，写 ¥0 会让人误以为免费
        label: '估算花费',
        val: (!k.priced && !k.cost) ? '未配单价' : (k.priced ? fmtCost(k.cost) : fmtCost(k.cost) + '+'),
        // 分时段分解：只写总额看不出时段规则有没有生效，这里把两档的次数与金额分开展示
        sub: costBreakdown(k),
        title: '模型调用的估算花费：未命中输入 + 缓存读写 + 输出各自按单价计，按每次调用发生的时间与' +
          '该模型的时段规则判定峰值/空闲价；不代表账单',
      },
      {
        // 总花费 = 模型（token）+ 搜索（按次），两边的口径完全不同，必须分开注明
        label: '总花费',
        val: fmtCost(k.totalCost),
        sub: '模型 ' + fmtCost(k.cost) + ' + 搜索 ' + fmtCost(k.searchCost),
        title: '模型调用的 token 费用 + 搜索/抽取的按次费用；两者各自合计后再相加，不代表账单',
      },
    ];
    ctx.kpis.innerHTML = '';
    renderKPICards(ctx.kpis, cards);

    renderChart(ctx.chartBox, d.daily || []);
    renderDonut(ctx.donutBox, d.byModel || []);
    renderModelTable(ctx.modelTable, d.byModel || []);
  }

  // renderKPICards 把卡片数组渲染进容器（模型用量与搜索分区共用一套样式）
  function renderKPICards(box, cards) {
    box.innerHTML = '';
    cards.forEach(function (c) {
      var cell = el('div', 'us-kpi' + (c.alert ? ' alert' : ''));
      if (c.title) cell.title = c.title;
      cell.appendChild(el('div', 'k-label', c.label));
      cell.appendChild(el('div', 'k-val', c.val));
      if (c.sub) cell.appendChild(el('div', 'k-sub', c.sub));
      box.appendChild(cell);
    });
  }

  /* ---------- 趋势图（手写 SVG，不引图表库） ---------- */

  function renderChart(box, daily) {
    box.innerHTML = '';
    if (!daily.length) {
      box.appendChild(el('div', 'us-empty', '这段时间没有调用记录'));
      return;
    }
    var W = 720, H = 190, padL = 46, padR = 14, padT = 12, padB = 26;
    var innerW = W - padL - padR;
    var innerH = H - padT - padB;
    var maxV = 1;
    daily.forEach(function (p) {
      maxV = Math.max(maxV, p.input || 0, p.output || 0);
    });
    // y 轴上限取整到「好看」的刻度
    var step = niceStep(maxV / 4);
    var top = Math.ceil(maxV / step) * step;

    var n = daily.length;
    var xOf = function (i) {
      if (n === 1) return padL + innerW / 2;
      return padL + (innerW * i) / (n - 1);
    };
    var yOf = function (v) {
      return padT + innerH - (innerH * (Number(v) || 0)) / top;
    };

    var svg = [];
    svg.push('<svg viewBox="0 0 ' + W + ' ' + H + '" role="img" aria-label="每日用量趋势">');
    // 网格与 y 轴刻度
    for (var i = 0; i <= 4; i++) {
      var v = step * i;
      if (v > top + 0.001) break;
      var y = yOf(v).toFixed(1);
      svg.push('<line x1="' + padL + '" y1="' + y + '" x2="' + (W - padR) + '" y2="' + y +
        '" stroke="currentColor" stroke-opacity="0.12" stroke-width="1"/>');
      svg.push('<text x="' + (padL - 8) + '" y="' + (Number(y) + 4).toFixed(1) +
        '" text-anchor="end" font-size="10" fill="currentColor" fill-opacity="0.55">' + fmtNum(v) + '</text>');
    }
    // 折线
    var line = function (key, color, fill) {
      var pts = daily.map(function (p, idx) {
        return xOf(idx).toFixed(1) + ',' + yOf(p[key]).toFixed(1);
      }).join(' ');
      var out = '';
      // 只有一天数据时面积会画成一个三角形（没有跨度），直接不填面积
      if (fill && n > 1) {
        out += '<polygon points="' + padL + ',' + yOf(0).toFixed(1) + ' ' + pts + ' ' +
          xOf(n - 1).toFixed(1) + ',' + yOf(0).toFixed(1) + '" fill="' + color +
          '" fill-opacity="0.10" stroke="none"/>';
      }
      out += '<polyline points="' + pts + '" fill="none" stroke="' + color + '" stroke-width="2" ' +
        'stroke-linejoin="round" stroke-linecap="round"/>';
      if (n <= 31) {
        daily.forEach(function (p, idx) {
          out += '<circle cx="' + xOf(idx).toFixed(1) + '" cy="' + yOf(p[key]).toFixed(1) +
            '" r="' + (n === 1 ? 3.4 : 2.4) + '" fill="' + color + '"><title>' + p.day + ' ' +
            fmtInt(p[key]) + ' tokens</title></circle>';
        });
      }
      return out;
    };
    svg.push(line('input', '#4d6bfe', true));
    svg.push(line('output', '#1f9d55', false));
    // x 轴日期（最多 6 个，抽稀显示）
    var every = Math.max(1, Math.ceil(n / 6));
    daily.forEach(function (p, idx) {
      if (idx % every !== 0 && idx !== n - 1) return;
      var label = String(p.day || '').slice(5);
      svg.push('<text x="' + xOf(idx).toFixed(1) + '" y="' + (H - 8) +
        '" text-anchor="middle" font-size="10" fill="currentColor" fill-opacity="0.55">' + label + '</text>');
    });
    svg.push('</svg>');
    box.innerHTML = svg.join('');
  }

  // niceStep 取「好看」的刻度步长（1/2/5 × 10^k）
  function niceStep(raw) {
    if (!(raw > 0)) return 1;
    var pow = Math.pow(10, Math.floor(Math.log10(raw)));
    var norm = raw / pow;
    var mult = norm <= 1 ? 1 : norm <= 2 ? 2 : norm <= 5 ? 5 : 10;
    return mult * pow;
  }

  /* ---------- 模型占比（环形图 + 图例） ---------- */

  function renderDonut(box, byModel) {
    box.innerHTML = '';
    var rows = byModel.map(function (m) {
      return { model: m.model, total: m.total || 0 };
    }).filter(function (m) { return m.total > 0; });
    rows.sort(function (a, b) { return b.total - a.total; });
    var total = rows.reduce(function (s, m) { return s + m.total; }, 0);
    if (!total) {
      box.appendChild(el('div', 'us-empty', '这段时间没有调用记录'));
      return;
    }
    // 前 5 名单独着色，其余合并成「其他」
    var top = rows.slice(0, 5);
    var rest = rows.slice(5);
    if (rest.length) {
      var otherTotal = rest.reduce(function (s, m) { return s + m.total; }, 0);
      top.push({ model: '其他（' + rest.length + ' 个模型）', total: otherTotal, other: true });
    }
    var R = 54, C = 2 * Math.PI * R, cx = 66, cy = 66;
    var svg = ['<svg viewBox="0 0 132 132" role="img" aria-label="模型占比">'];
    svg.push('<circle cx="66" cy="66" r="' + R + '" fill="none" stroke="currentColor" stroke-opacity="0.1" stroke-width="16"/>');
    var acc = 0;
    top.forEach(function (m, i) {
      var frac = m.total / total;
      var len = C * frac;
      var color = DONUT_COLORS[i % DONUT_COLORS.length];
      svg.push('<circle cx="66" cy="66" r="' + R + '" fill="none" stroke="' + color +
        '" stroke-width="16" stroke-dasharray="' + len.toFixed(2) + ' ' + (C - len).toFixed(2) +
        '" stroke-dashoffset="' + (-acc).toFixed(2) + '" transform="rotate(-90 66 66)">' +
        '<title>' + escapeXML(m.model) + '：' + fmtInt(m.total) + ' tokens（' + fmtPct(frac * 100) + '）</title></circle>');
      acc += len;
    });
    svg.push('<text x="66" y="62" text-anchor="middle" font-size="16" font-weight="600" fill="currentColor">' +
      fmtNum(total) + '</text>');
    svg.push('<text x="66" y="78" text-anchor="middle" font-size="9" fill="currentColor" fill-opacity="0.55">tokens</text>');
    svg.push('</svg>');
    var donut = el('div');
    donut.innerHTML = svg.join('');
    box.appendChild(donut.firstChild);

    var list = el('div', 'us-donut-list');
    top.forEach(function (m, i) {
      var row = el('div', 'row');
      var dot = el('i');
      dot.style.background = DONUT_COLORS[i % DONUT_COLORS.length];
      row.appendChild(dot);
      var nm = el('span', 'nm', m.model);
      nm.title = m.model;
      row.appendChild(nm);
      row.appendChild(el('span', 'vl', fmtNum(m.total) + ' · ' + fmtPct((m.total / total) * 100)));
      list.appendChild(row);
    });
    box.appendChild(list);
  }

  function escapeXML(s) {
    return String(s == null ? '' : s).replace(/[&<>"']/g, function (c) {
      return { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' }[c];
    });
  }

  /* ---------- 按模型消耗统计 ---------- */

  function renderModelTable(box, rows) {
    box.innerHTML = '';
    if (!rows.length) {
      box.appendChild(el('div', 'us-empty', '这段时间没有调用记录'));
      return;
    }
    var t = el('table', 'us-table');
    var thead = el('thead');
    var tr = el('tr');
    ['模型', '类型', '请求数', '输入', '输出', '缓存写', '缓存读', '总计', '估算花费'].forEach(function (h, i) {
      var th = el('th', i === 0 ? 'l' : '', h);
      tr.appendChild(th);
    });
    thead.appendChild(tr);
    t.appendChild(thead);
    var tb = el('tbody');
    rows.forEach(function (m) {
      var r = el('tr');
      var name = el('td', 'l', m.model || '（未知）');
      name.title = m.model || '';
      r.appendChild(name);
      r.appendChild(el('td', '', PROTO_LABEL[m.protocol] || m.protocol || 'chat'));
      r.appendChild(el('td', 'num', fmtInt(m.calls)));
      r.appendChild(el('td', 'num', fmtInt(m.input)));
      r.appendChild(el('td', 'num', fmtInt(m.output)));
      r.appendChild(el('td', 'num', fmtInt(m.cacheWrite)));
      r.appendChild(el('td', 'num', fmtInt(m.cacheRead)));
      r.appendChild(el('td', 'num', fmtInt(m.total)));
      var cost = el('td', 'num', m.priced ? fmtCost(m.cost) : '未配单价');
      if (!m.priced) cost.className = 'num muted';
      r.appendChild(cost);
      tb.appendChild(r);
    });
    t.appendChild(tb);
    box.appendChild(t);
  }

  /* ---------- 用量记录（分页） ---------- */

  function renderRecords(ctx, d) {
    var items = d.items || [];
    var total = Number(d.total) || 0;
    var pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
    ctx.recTable.innerHTML = '';
    if (!items.length) {
      ctx.recTable.appendChild(el('div', 'us-empty', '这段时间没有调用记录'));
    } else {
      var t = el('table', 'us-table us-records-table');
      var thead = el('thead');
      var htr = el('tr');
      ['时间', '模型', '类型', '推理强度', '渠道 · Key', '输入', '输出', '缓存写', '缓存读', '总计', '首字延迟', '耗时', '状态']
        .forEach(function (h) { htr.appendChild(el('th', '', h)); });
      thead.appendChild(htr);
      t.appendChild(thead);
      var tb = el('tbody');
      items.forEach(function (it) {
        var r = el('tr');
        // 这次调用命中峰值价还是空闲价：按明细时间戳判定（后端算好给 peak）
        r.title = '调用时段：' + (it.peak ? '高峰（按峰值价计费）' : '空闲（按空闲价计费）');
        r.appendChild(el('td', 'num l', fmtTime(it.ts)));
        var name = el('td', 'l', it.model || '（未知）');
        name.title = (it.purpose ? (PURPOSE_LABEL[it.purpose] || it.purpose) : '') +
          (it.sessionId ? ' · 会话 ' + it.sessionId : '');
        r.appendChild(name);
        r.appendChild(el('td', '', PROTO_LABEL[it.protocol] || it.protocol || 'chat'));
        // 推理强度：空＝这次调用没指定档位（跟随服务端默认，老数据也是空）
        var th = THINK_LABELS[it.thinking];
        if (th) r.appendChild(el('td', '', th));
        else {
          var thd = el('td', 'muted', '默认');
          thd.title = '这次调用没有指定思考档位（跟随服务端默认；老数据也记为默认）';
          r.appendChild(thd);
        }
        var ch = el('td', 'l', (it.providerName || it.providerId || '默认渠道') +
          (it.keyLabel ? ' · ' + it.keyLabel : ''));
        ch.title = (it.providerId || '') + (it.keyId ? ' / ' + it.keyId : '');
        r.appendChild(ch);
        r.appendChild(el('td', 'num', fmtInt(it.input)));
        r.appendChild(el('td', 'num', fmtInt(it.output)));
        r.appendChild(el('td', 'num', fmtInt(it.cacheWrite)));
        r.appendChild(el('td', 'num', fmtInt(it.cacheRead)));
        r.appendChild(el('td', 'num', fmtInt(it.total)));
        r.appendChild(el('td', 'num', fmtMs(it.ttftMs)));
        r.appendChild(el('td', 'num', fmtMs(it.durationMs)));
        var st = el('td', it.ok ? 'ok' : 'bad', it.ok ? '成功' : '失败');
        if (!it.ok && it.error) st.title = it.error;
        r.appendChild(st);
        tb.appendChild(r);
      });
      t.appendChild(tb);
      ctx.recTable.appendChild(t);
    }

    ctx.pager.innerHTML = '';
    ctx.pager.appendChild(el('span', '', '共 ' + fmtInt(total) + ' 条 · 第 ' + (state.page + 1) + '/' + pages + ' 页'));
    var prev = el('button', 'us-btn', '上一页');
    prev.type = 'button';
    prev.disabled = state.page <= 0;
    prev.onclick = function () {
      if (state.page <= 0) return;
      state.page--;
      refreshData(ctx);
    };
    var next = el('button', 'us-btn', '下一页');
    next.type = 'button';
    next.disabled = state.page + 1 >= pages;
    next.onclick = function () {
      if (state.page + 1 >= pages) return;
      state.page++;
      refreshData(ctx);
    };
    ctx.pager.appendChild(prev);
    ctx.pager.appendChild(next);
    if (state.page >= pages && state.page > 0) { // 数据变少（清理后）自动回退一页
      state.page = pages - 1;
      refreshData(ctx);
    }
  }

  /* ---------- 搜索调用（按次计费，与 token 类用量分开合计） ---------- */

  function renderSearch(ctx, d) {
    var k = d.kpi || {};
    renderKPICards(ctx.searchKpis, [
      { label: '搜索次数', val: fmtInt(k.calls), sub: '含失败与回落尝试' },
      { label: '成功次数', val: fmtInt(k.success), sub: '计费依据（失败不计费）' },
      {
        label: '失败次数',
        val: fmtInt(k.errors),
        sub: k.errors > 0 ? '失败只计数不计费' : '无失败',
        alert: k.errors > 0,
      },
      {
        label: '回落率',
        val: k.calls > 0 ? fmtPct(k.fallbackRate) : '—',
        sub: '第 2 家及以后才成功的占比',
        title: '回落次数 / 总次数：某家失败后由后面几家接手，说明前一家不稳（Key、额度或上游）',
      },
      { label: '估算搜索费用', val: fmtCost(k.cost), sub: '成功次数 × 单价', title: '只对成功调用计费；各家单价在下面的「搜索单价」里改' },
      { label: '平均耗时', val: fmtMs(k.avgDurationMs), sub: '含失败的那几次' },
    ]);

    // 按供应商（同一家的搜索与抽取分开成两行：能力位不同，单价与失败原因也不同）
    var rows = d.byProvider || [];
    ctx.searchProviderTable.innerHTML = '';
    if (!rows.length) {
      ctx.searchProviderTable.appendChild(el('div', 'us-empty', '这段时间没有搜索调用'));
    } else {
      var t = el('table', 'us-table');
      var thead = el('thead');
      var htr = el('tr');
      ['供应商', '能力', '次数', '成功', '失败', '回落', '平均耗时', '单价（元/次）', '费用', '说明']
        .forEach(function (h, i) { htr.appendChild(el('th', i === 0 ? 'l' : '', h)); });
      thead.appendChild(htr);
      t.appendChild(thead);
      var tb = el('tbody');
      rows.forEach(function (p) {
        var r = el('tr');
        r.appendChild(el('td', 'l', p.name || p.providerId));
        r.appendChild(el('td', '', p.kind === 'extract' ? '抽取' : '搜索'));
        r.appendChild(el('td', 'num', fmtInt(p.calls)));
        r.appendChild(el('td', 'num', fmtInt(p.success)));
        var errCell = el('td', 'num', fmtInt(p.errors));
        if (p.errors > 0) errCell.className = 'num bad';
        r.appendChild(errCell);
        r.appendChild(el('td', 'num', fmtInt(p.fallback)));
        r.appendChild(el('td', 'num', fmtMs(p.avgDurationMs)));
        r.appendChild(el('td', 'num', '¥' + (Number(p.unitPrice) || 0).toFixed(4)));
        r.appendChild(el('td', 'num', fmtCost(p.cost)));
        var note = el('td', 'l muted', p.note || '');
        r.appendChild(note);
        tb.appendChild(r);
      });
      t.appendChild(tb);
      ctx.searchProviderTable.appendChild(t);
    }

    // 明细（每次回落尝试一行）
    var items = d.items || [];
    var total = Number(d.total) || 0;
    var pages = Math.max(1, Math.ceil(total / PAGE_SIZE));
    ctx.searchRecTable.innerHTML = '';
    if (!items.length) {
      ctx.searchRecTable.appendChild(el('div', 'us-empty', '这段时间没有搜索调用（本页只统计升级之后的调用）'));
    } else {
      var t2 = el('table', 'us-table us-records-table');
      var th2 = el('thead');
      var htr2 = el('tr');
      ['时间', '供应商', '能力', '结果', '错误类别', '耗时', '顺位', '是否回落', '查询 / 目标摘要']
        .forEach(function (h) { htr2.appendChild(el('th', '', h)); });
      th2.appendChild(htr2);
      t2.appendChild(th2);
      var tb2 = el('tbody');
      items.forEach(function (it) {
        var r = el('tr');
        r.appendChild(el('td', 'num l', fmtTime(it.ts)));
        r.appendChild(el('td', 'l', it.providerName || it.providerId || ''));
        r.appendChild(el('td', '', it.kindLabel || (it.kind === 'extract' ? '抽取' : '搜索')));
        r.appendChild(el('td', it.ok ? 'ok' : 'bad', it.ok ? '成功' : '失败'));
        r.appendChild(el('td', 'l muted', it.ok ? '—' : (it.errKind || '未知')));
        r.appendChild(el('td', 'num', fmtMs(it.durationMs)));
        r.appendChild(el('td', 'num', '第 ' + fmtInt(it.rank) + ' 位'));
        r.appendChild(el('td', '', it.rank > 1 ? '是' : '否'));
        var det = el('td', 'l', it.detail || '');
        det.title = (it.detail || '') + (it.error ? '\n错误：' + it.error : '');
        r.appendChild(det);
        tb2.appendChild(r);
      });
      t2.appendChild(tb2);
      ctx.searchRecTable.appendChild(t2);
    }

    ctx.searchPager.innerHTML = '';
    ctx.searchPager.appendChild(el('span', '',
      '共 ' + fmtInt(total) + ' 条 · 第 ' + (state.searchPage + 1) + '/' + pages + ' 页'));
    var prev = el('button', 'us-btn', '上一页');
    prev.type = 'button';
    prev.disabled = state.searchPage <= 0;
    prev.onclick = function () {
      if (state.searchPage <= 0) return;
      state.searchPage--;
      refreshData(ctx);
    };
    var next = el('button', 'us-btn', '下一页');
    next.type = 'button';
    next.disabled = state.searchPage + 1 >= pages;
    next.onclick = function () {
      if (state.searchPage + 1 >= pages) return;
      state.searchPage++;
      refreshData(ctx);
    };
    ctx.searchPager.appendChild(prev);
    ctx.searchPager.appendChild(next);
    if (state.searchPage >= pages && state.searchPage > 0) {
      state.searchPage = pages - 1;
      refreshData(ctx);
    }
  }

  /* ---------- 单价与时段规则（用户手动配置：程序不内置任何模型价） ---------- */

  var DAY_NAMES = ['日', '一', '二', '三', '四', '五', '六'];

  // parseWindow 后端口径 "HH:MM-HH:MM" → {from, to}（脏值返回 null，保存时丢掉）
  function parseWindow(raw) {
    var p = String(raw || '').split('-');
    if (p.length !== 2) return null;
    var a = String(p[0]).trim();
    var b = String(p[1]).trim();
    if (!/^\d{1,2}:\d{2}$/.test(a) || !/^\d{1,2}:\d{2}$/.test(b)) return null;
    return { from: normTime(a), to: normTime(b) };
  }

  function normTime(s) {
    var p = String(s || '').split(':');
    var h = parseInt(p[0], 10);
    var m = parseInt(p[1], 10);
    if (!isFinite(h) || !isFinite(m)) return '';
    return ('0' + h).slice(-2) + ':' + ('0' + m).slice(-2);
  }

  // rowState 后端一行价表 → 页面行态（规则字段一律给数组，前端不用判 null）
  function rowState(m) {
    var numOf = function (v) { return typeof v === 'number' && isFinite(v) ? v : 0; };
    var wins = [];
    (m && m.peakWindows ? m.peakWindows : []).forEach(function (w) {
      var pw = parseWindow(w);
      if (pw) wins.push(pw);
    });
    return {
      id: m && m.id ? m.id : '',
      input: numOf(m && m.input),
      output: numOf(m && m.output),
      cacheRead: numOf(m && m.cacheRead),
      cacheWrite: numOf(m && m.cacheWrite),
      days: m && m.peakDays ? m.peakDays.slice() : [],
      windows: wins,
      holidays: m && m.holidays ? m.holidays.slice() : [],
      idle: m && m.idleFactor > 0 && m.idleFactor < 1 ? m.idleFactor : 1,
      expanded: false,
    };
  }

  // daysLabel 星期多选 → 人话（不勾＝每天，与后端口径一致）
  function daysLabel(days) {
    if (!days || !days.length) return '每天';
    var sorted = days.slice().sort();
    var key = sorted.join(',');
    if (key === '1,2,3,4,5') return '周一至周五';
    if (key === '0,6') return '周末';
    return '周' + sorted.map(function (d) { return DAY_NAMES[d]; }).join('、');
  }

  // ruleSummary 一行规则的摘要：**没配时段就明说**，别让人以为空闲价在生效
  function ruleSummary(row) {
    if (!row.windows.length) return '未配置时段规则（全按高峰价）';
    var parts = [daysLabel(row.days),
      row.windows.map(function (w) { return w.from + '–' + w.to; }).join('、')];
    parts.push(row.idle > 0 && row.idle < 1 ? '空闲 ×' + row.idle : '不分时段');
    if (row.holidays.length) parts.push('节假日 ' + row.holidays.length + ' 天');
    return parts.join(' · ');
  }

  function copyRule(from, to) {
    to.days = from.days.slice();
    to.windows = from.windows.map(function (w) { return { from: w.from, to: w.to }; });
    to.holidays = from.holidays.slice();
    to.idle = from.idle;
  }

  // fillRuleBox 渲染一条规则的编辑器（增删时段/节日后整块重建，价格输入不受影响）
  function fillRuleBox(box, row, api) {
    box.innerHTML = '';
    var rebuild = api.rebuild;

    // 高峰星期
    var lineDays = el('div', 'us-rule-line');
    lineDays.appendChild(el('span', 'us-rule-label', '高峰星期'));
    for (var d = 0; d < 7; d++) {
      (function (day) {
        var lab = el('label', 'us-day');
        var cb = el('input');
        cb.type = 'checkbox';
        cb.checked = row.days.indexOf(day) >= 0;
        cb.onchange = function () {
          var at = row.days.indexOf(day);
          if (cb.checked && at < 0) row.days.push(day);
          if (!cb.checked && at >= 0) row.days.splice(at, 1);
          api.refresh();
        };
        lab.appendChild(cb);
        lab.appendChild(el('span', '', '周' + DAY_NAMES[day]));
        lineDays.appendChild(lab);
      })(d);
    }
    lineDays.appendChild(el('span', 'us-note', '都不勾＝每天'));
    box.appendChild(lineDays);

    // 高峰时段（可多个；起止用时分选择器，跨天如 22:00 至 06:00）
    var lineWin = el('div', 'us-rule-line');
    lineWin.appendChild(el('span', 'us-rule-label', '高峰时段'));
    var winList = el('div', 'us-rule-list');
    row.windows.forEach(function (w, idx) {
      var item = el('div', 'us-rule-item');
      var from = el('input', 'us-time');
      from.type = 'time';
      from.value = w.from;
      from.onchange = function () { row.windows[idx].from = normTime(from.value); api.refresh(); };
      var to = el('input', 'us-time');
      to.type = 'time';
      to.value = w.to;
      to.onchange = function () { row.windows[idx].to = normTime(to.value); api.refresh(); };
      var del = el('button', 'us-btn tiny', '删除');
      del.type = 'button';
      del.onclick = function () { row.windows.splice(idx, 1); rebuild(); api.refresh(); };
      item.appendChild(from);
      item.appendChild(el('span', 'us-sep', '至'));
      item.appendChild(to);
      item.appendChild(del);
      winList.appendChild(item);
    });
    if (!row.windows.length) {
      winList.appendChild(el('span', 'us-note', '一个时段都没配＝不分时段'));
    }
    var addWin = el('button', 'us-btn tiny', '添加时段');
    addWin.type = 'button';
    addWin.onclick = function () { row.windows.push({ from: '09:00', to: '12:00' }); rebuild(); api.refresh(); };
    winList.appendChild(addWin);
    lineWin.appendChild(winList);
    box.appendChild(lineWin);

    // 节假日（全天按空闲）：MM-DD 每年重复，YYYY-MM-DD 只此一次
    var lineHol = el('div', 'us-rule-line');
    lineHol.appendChild(el('span', 'us-rule-label', '节假日'));
    var holList = el('div', 'us-rule-list');
    row.holidays.forEach(function (h, idx) {
      var item = el('div', 'us-rule-item');
      var inp = el('input', 'us-holiday');
      inp.type = 'text';
      inp.value = h;
      inp.placeholder = '10-01 或 2026-10-01';
      inp.onchange = function () { row.holidays[idx] = inp.value.trim(); api.refresh(); };
      var del = el('button', 'us-btn tiny', '删除');
      del.type = 'button';
      del.onclick = function () { row.holidays.splice(idx, 1); rebuild(); api.refresh(); };
      item.appendChild(inp);
      item.appendChild(del);
      holList.appendChild(item);
    });
    var addHol = el('button', 'us-btn tiny', '添加节假日');
    addHol.type = 'button';
    addHol.onclick = function () { row.holidays.push(''); rebuild(); api.refresh(); };
    holList.appendChild(addHol);
    lineHol.appendChild(holList);
    box.appendChild(lineHol);

    // 空闲折扣
    var lineIdle = el('div', 'us-rule-line');
    lineIdle.appendChild(el('span', 'us-rule-label', '空闲折扣'));
    var idleInp = el('input', 'us-idle');
    idleInp.type = 'number';
    idleInp.min = '0';
    idleInp.max = '1';
    idleInp.step = '0.05';
    idleInp.value = String(row.idle);
    idleInp.oninput = function () {
      var v = parseFloat(idleInp.value);
      // 只认 (0,1)：1 与非法值都当「不分时段」（与后端 normalize 同口径）
      if (isFinite(v) && v > 0 && v < 1) { row.idle = v; api.refresh(); }
    };
    idleInp.onchange = function () {
      var v = parseFloat(idleInp.value);
      row.idle = isFinite(v) && v > 0 && v < 1 ? v : 1;
      idleInp.value = String(row.idle);
      api.refresh();
    };
    lineIdle.appendChild(idleInp);
    lineIdle.appendChild(el('span', 'us-note', '空闲时段 = 峰值价 × 折扣；1＝不分时段，0.5＝半价'));
    box.appendChild(lineIdle);

    // 复制规则到其它模型（配好一个，其余一键同步，不必逐个重配）
    var lineCopy = el('div', 'us-rule-line');
    lineCopy.appendChild(el('span', 'us-rule-label', '复制规则'));
    var sel = el('select');
    var others = api.rows.filter(function (r) { return r !== row; });
    if (!others.length) {
      var only = el('option', '', '（没有其它模型）');
      only.value = '';
      sel.appendChild(only);
      sel.disabled = true;
    }
    others.forEach(function (r) {
      var o = el('option', '', r.id || '（未填 id 的行）');
      o.value = String(api.rows.indexOf(r));
      sel.appendChild(o);
    });
    var btnCopy = el('button', 'us-btn tiny', '复制');
    btnCopy.type = 'button';
    btnCopy.onclick = function () {
      var tgt = api.rows[parseInt(sel.value, 10)];
      if (!tgt) return;
      copyRule(row, tgt);
      if (tgt.rebuildRule) tgt.rebuildRule();
      api.refresh();
      api.status('已把时段规则复制到 ' + (tgt.id || '目标行'), false);
    };
    var btnCopyAll = el('button', 'us-btn tiny', '复制到全部');
    btnCopyAll.type = 'button';
    btnCopyAll.onclick = function () {
      var n = 0;
      api.rows.forEach(function (r) {
        if (r === row) return;
        copyRule(row, r);
        if (r.rebuildRule) r.rebuildRule();
        n++;
      });
      api.refresh();
      api.status(n ? ('已把时段规则复制到其它 ' + n + ' 个模型') : '没有其它模型可复制', !n);
    };
    lineCopy.appendChild(sel);
    lineCopy.appendChild(btnCopy);
    lineCopy.appendChild(btnCopyAll);
    box.appendChild(lineCopy);

    box.appendChild(el('p', 'us-note',
      '时段按「调用发生的时刻」判定（改规则后历史数据按新规则重算）：高峰星期不勾＝每天；' +
      '时段可配多个，跨天写 22:00 至 06:00（凌晨那段按调用当天的星期算）；' +
      '节假日的 MM-DD 每年重复、YYYY-MM-DD 只此一年，节假日当天全天按空闲；' +
      '时段一个都不配＝不分时段（全天按高峰价）。'));
  }

  function renderPricing(ctx, p) {
    var grid = ctx.priceGrid;
    grid.innerHTML = '';

    var rows = []; // 行态（价格、规则都在这；DOM 只是它的投影）

    var head = el('div', 'us-filters');
    head.appendChild(el('span', 'us-note', '模型单价（元 / 百万 tokens，填峰值价）与时段规则'));
    var btnSave = el('button', 'us-btn primary', '保存单价与规则');
    btnSave.type = 'button';
    var btnAdd = el('button', 'us-btn', '新增模型');
    btnAdd.type = 'button';
    var btnFill = el('button', 'us-btn', '从渠道模型添加空行');
    btnFill.type = 'button';
    btnFill.title = '把当前各渠道已配的模型 id 列成空行（不填价格、不配规则）；逐行填价后保存即生效';
    head.appendChild(btnSave);
    head.appendChild(btnAdd);
    head.appendChild(btnFill);
    head.style.justifyContent = 'flex-start';
    grid.appendChild(head);

    // 顶部提示：价表空 → 先说「未配单价」怎么办；有价没规则 → 说明现在全按高峰价
    var warnBox = el('div');
    grid.appendChild(warnBox);

    var wrapT = el('div', 'us-table-wrap');
    var t = el('table', 'us-table us-price-table');
    var thead = el('thead');
    var htr = el('tr');
    ['模型 id', '输入', '输出', '缓存读取', '缓存写入', '时段规则', ''].forEach(function (h) {
      htr.appendChild(el('th', h === '模型 id' ? 'l' : '', h));
    });
    thead.appendChild(htr);
    t.appendChild(thead);
    var tb = el('tbody');
    t.appendChild(tb);
    wrapT.appendChild(t);
    grid.appendChild(wrapT);

    function refreshWarn() {
      warnBox.innerHTML = '';
      if (!rows.length) {
        warnBox.appendChild(el('p', 'us-note us-warn',
          '价表是空的：程序不内置任何模型单价。点「从渠道模型添加空行」把当前渠道的模型列出来，' +
          '再逐行填单价——没填价之前，费用一律显示「未配单价」（不会用任何兜底价）。'));
        return;
      }
      var withRule = rows.filter(function (r) { return r.windows.length > 0; }).length;
      if (!withRule) {
        warnBox.appendChild(el('p', 'us-note us-warn',
          '未配置时段规则：所有调用都按高峰价计费。点某行的「规则」展开，配高峰星期/时段/节假日与空闲折扣' +
          '（配好一个再「复制到全部」，不用逐个配）。'));
      } else if (withRule < rows.length) {
        warnBox.appendChild(el('p', 'us-note',
          '还有 ' + (rows.length - withRule) + ' 个模型没配时段规则，它们按不分时段（全按高峰价）计费。'));
      }
    }

    // buildRuleRow 展开行：价格输入只在主行，规则整块重建不影响它们
    function buildRuleRow(row) {
      var tr = el('tr', 'us-rule-row');
      tr.style.display = row.expanded ? '' : 'none';
      var td = el('td');
      td.colSpan = 7;
      var box = el('div', 'us-rule');
      td.appendChild(box);
      tr.appendChild(td);
      var api = {
        rows: rows,
        status: function (msg, isErr) { setStatus(ctx.statusLine, msg, isErr); },
        refresh: function () { if (row.refreshSummary) row.refreshSummary(); refreshWarn(); },
        rebuild: function () { fillRuleBox(box, row, api); },
      };
      fillRuleBox(box, row, api);
      return tr;
    }

    // mountRow 把一行行态挂成 DOM（再挂它的规则展开行）
    function mountRow(row) {
      var tr = el('tr');
      var idCell = el('td', 'l');
      var idIn = el('input');
      idIn.type = 'text';
      idIn.value = row.id;
      idIn.dataset.kind = 'id';
      idIn.oninput = function () { row.id = idIn.value; refreshWarn(); };
      idCell.appendChild(idIn);
      tr.appendChild(idCell);
      [['input', '峰值输入单价（未命中部分，元/百万 tokens）'],
        ['output', '峰值输出单价'],
        ['cacheRead', '缓存读取单价（命中部分；留 0 表示不计费）'],
        ['cacheWrite', '缓存写入单价（Anthropic 的 cache_creation）']].forEach(function (pair) {
        var k = pair[0];
        var td = el('td');
        var inp = el('input');
        inp.type = 'number';
        inp.step = '0.01';
        inp.min = '0';
        inp.value = String(row[k]);
        inp.dataset.kind = k;
        inp.title = pair[1];
        inp.oninput = function () {
          var v = parseFloat(inp.value);
          row[k] = isFinite(v) && v >= 0 ? v : 0;
        };
        inp.onchange = function () {
          var v = parseFloat(inp.value);
          row[k] = isFinite(v) && v >= 0 ? v : 0;
          inp.value = String(row[k]); // 失焦时归整（输入过程中不改写，免得抢光标）
        };
        td.appendChild(inp);
        tr.appendChild(td);
      });
      var sumCell = el('td', 'us-rule-sum');
      var sumText = el('span', 'us-rule-text');
      var btnRule = el('button', 'us-btn tiny', '规则');
      btnRule.type = 'button';
      sumCell.appendChild(sumText);
      sumCell.appendChild(btnRule);      row.refreshSummary = function () {
        sumText.textContent = ruleSummary(row);
        sumCell.className = 'us-rule-sum' + (row.windows.length ? '' : ' warn');
      };
      row.refreshSummary();
      tr.appendChild(sumCell);
      var delCell = el('td');
      var del = el('button', 'us-btn tiny', '删除');
      del.type = 'button';
      del.onclick = function () {
        var at = rows.indexOf(row);
        if (at >= 0) rows.splice(at, 1);
        tr.remove();
        ruleTr.remove();
        refreshWarn();
      };
      delCell.appendChild(del);
      tr.appendChild(delCell);

      var ruleTr = buildRuleRow(row);
      // 规则块整体重建（增删时段/节假日、复制规则后调）：展开行一并换新
      row.rebuildRule = function () {
        var next = buildRuleRow(row);
        ruleTr.replaceWith(next);
        ruleTr = next;
      };
      btnRule.onclick = function () {
        row.expanded = !row.expanded;
        // 展开时重建一次：规则里的「复制规则到其它模型」要列出**当前**全部行
        //（本行挂载时后面的行可能还没挂上，列表会停在挂载那一刻）
        if (row.expanded) row.rebuildRule();
        ruleTr.style.display = row.expanded ? '' : 'none';
        btnRule.textContent = row.expanded ? '收起' : '规则';
      };
      tb.appendChild(tr);
      tb.appendChild(ruleTr);
      return row;
    }

    function addRow(vals) {
      var row = rowState(vals || null);
      rows.push(row);
      mountRow(row);
      refreshWarn();
      return row;
    }

    // 已有价表逐行挂上
    (p.models || []).forEach(function (m) {
      var row = rowState(m);
      rows.push(row);
      mountRow(row);
    });
    refreshWarn();

    btnAdd.onclick = function () {
      var row = addRow(null);
      var inputs = tb.querySelectorAll('tr:last-child input[data-kind="id"]');
      if (inputs.length) inputs[0].focus();
      return row;
    };

    btnFill.onclick = function () {
      // 从渠道模型添加空行：只落地模型 id，价格与规则一律留空/不配（用户口径）
      var have = {};
      rows.forEach(function (r) { if (r.id) have[r.id] = true; });
      var added = 0;
      (p.providers || []).forEach(function (pv) {
        (pv.models || []).forEach(function (id) {
          if (!id || have[id]) return;
          have[id] = true;
          addRow({ id: id });
          added++;
        });
      });
      setStatus(ctx.statusLine, added
        ? ('已添加 ' + added + ' 个模型空行（价格与规则都没填，逐行填价后保存）')
        : '当前渠道的模型都已在价表里', !added);
    };

    var collect = function () {
      var models = [];
      var badWin = '';
      var badHol = '';
      rows.forEach(function (r) {
        var id = String(r.id || '').trim();
        if (!id) return;
        var wins = [];
        r.windows.forEach(function (w) {
          if (!w.from || !w.to) { badWin = id; return; }
          wins.push(w.from + '-' + w.to);
        });
        var hols = [];
        r.holidays.forEach(function (h) {
          h = String(h || '').trim();
          if (!h) return;
          if (!/^\d{2}-\d{2}$/.test(h) && !/^\d{4}-\d{2}-\d{2}$/.test(h)) { badHol = id; return; }
          hols.push(h);
        });
        models.push({
          id: id,
          input: Number(r.input) || 0,
          output: Number(r.output) || 0,
          cacheRead: Number(r.cacheRead) || 0,
          cacheWrite: Number(r.cacheWrite) || 0,
          peakDays: r.days.slice().sort(),
          peakWindows: wins,
          holidays: hols,
          idleFactor: r.idle,
        });
      });
      return { models: models, badWin: badWin, badHol: badHol };
    };

    // ---- 渠道倍率 ----
    var mulBox = el('div', 'us-price-grid');
    mulBox.appendChild(el('h4', '', '渠道倍率'));
    var mulList = el('div', 'us-mul-list');
    (p.providers || []).forEach(function (pv) {
      var row = el('div', 'us-mul-row');
      row.appendChild(el('span', 'nm', pv.name || pv.id));
      var inp = el('input');
      inp.type = 'number';
      inp.step = '0.01';
      inp.min = '0';
      inp.value = String(pv.multiplier != null ? pv.multiplier : 1);
      inp.dataset.provider = pv.id;
      row.appendChild(inp);
      row.appendChild(el('span', '', '× 基础价'));
      mulList.appendChild(row);
    });
    if (!(p.providers || []).length) {
      mulList.appendChild(el('p', 'us-note', '还没有配置任何渠道。'));
    }
    mulBox.appendChild(mulList);
    mulBox.appendChild(el('p', 'us-note',
      '倍率作用于该渠道的全部模型：如中转站按官方价 1.5 倍计费，填 1.5；不填即 1.0。'));
    var saveBar = el('div', 'us-filters');
    var btnSave2 = el('button', 'us-btn primary', '保存单价与规则');
    btnSave2.type = 'button';
    saveBar.appendChild(btnSave2);
    mulBox.appendChild(saveBar);
    grid.appendChild(mulBox);

    // ---- 搜索单价（按次计费，与 token 价完全两套口径）----
    var searchBox = el('div', 'us-price-grid');
    searchBox.appendChild(el('h4', '', '搜索单价'));
    searchBox.appendChild(el('p', 'us-note',
      '搜索/抽取按「次」计费，只有成功调用计费，单位「元 / 次」。0 表示不计费：' +
      'Tavily / AnySearch 有免费额度、公司网关由服务端统一计费。改了这里，历史搜索费用按新价重算。'));
    var searchList = el('div', 'us-mul-list');
    (p.search || []).forEach(function (sv) {
      var row = el('div', 'us-mul-row');
      row.appendChild(el('span', 'nm', sv.name || sv.id));
      var inp = el('input');
      inp.type = 'number';
      inp.step = '0.0001';
      inp.min = '0';
      inp.value = String(sv.price != null ? sv.price : 0);
      inp.dataset.search = sv.id;
      inp.title = '单价（元/次，只对成功调用计费）；该类型公开默认价 ' +
        (Number(sv.default) || 0).toFixed(4) + ' 元/次';
      row.appendChild(inp);
      row.appendChild(el('span', '', '元 / 次（' + (sv.kind || '') + '）'));
      if (sv.note) row.appendChild(el('span', 'us-note', sv.note));
      searchList.appendChild(row);
    });
    if (!(p.search || []).length) {
      searchList.appendChild(el('p', 'us-note', '还没有配置任何搜索供应商（设置 · 网络访问 · 联网搜索）。'));
    }
    searchBox.appendChild(searchList);
    grid.appendChild(searchBox);

    var doSave = function () {
      var got = collect();
      if (got.badWin) {
        setStatus(ctx.statusLine, '「' + got.badWin + '」有没填完的时段（起止都要选），先补全再保存', true);
        return;
      }
      if (got.badHol) {
        setStatus(ctx.statusLine, '「' + got.badHol + '」的节假日格式不对：写 MM-DD（每年）或 YYYY-MM-DD（一次性）', true);
        return;
      }
      var multipliers = {};
      mulList.querySelectorAll('input[data-provider]').forEach(function (inp) {
        var v = parseFloat(inp.value);
        if (isFinite(v) && v > 0) multipliers[inp.dataset.provider] = v;
      });
      var searchPrices = {};
      searchList.querySelectorAll('input[data-search]').forEach(function (inp) {
        var v = parseFloat(inp.value);
        if (isFinite(v) && v >= 0) searchPrices[inp.dataset.search] = v;
      });
      var unpriced = got.models.filter(function (m) {
        return !(m.input || m.output || m.cacheRead || m.cacheWrite);
      }).length;
      setStatus(ctx.statusLine, '保存中…', false);
      apiPost('/api/usage/pricing', { models: got.models, multipliers: multipliers, searchPrices: searchPrices })
        .then(function (np) {
          state.pricing = np;
          renderPricing(ctx, np);
          setStatus(ctx.statusLine, '已保存（' + got.models.length + ' 个模型单价、' +
            Object.keys(multipliers).length + ' 个渠道倍率、' +
            Object.keys(searchPrices).length + ' 个搜索单价' +
            (unpriced ? '；其中 ' + unpriced + ' 个模型还没填价，费用显示「未配单价」' : '') + '）', false);
          refreshData(ctx); // 改了价或规则，费用与页面立刻按新口径重算
        }).catch(function (e) {
          setStatus(ctx.statusLine, '保存失败：' + (e && e.message ? e.message : e), true);
        });
    };
    btnSave.onclick = doSave;
    btnSave2.onclick = doSave;
  }

  /* ================= 输入框下方的实时速度条 ================= */

  // speed 状态按视图分开（主对话 / 右栏子对话各一条）
  var speed = {
    main: null,
    sub: null,
  };

  function hostFor(view) {
    // 速度条挂在该视图输入卡的正下方（与「输入框下方」的需求一致）：
    // 主对话是 #composer 之后，右栏子对话是 #sub-composer 之后
    return view === 'sub' ? document.getElementById('sub-composer') : document.getElementById('composer');
  }

  function ensureBar(view) {
    var host = hostFor(view);
    if (!host || !host.parentNode) return null;
    var id = 'us-speed-' + view;
    var bar = document.getElementById(id);
    if (!bar) {
      bar = el('div', 'us-speed hidden');
      bar.id = id;
      bar.dataset.usSpeed = view;
      bar.innerHTML = '<span class="us-arrow">↑</span><span class="us-tps">0.0 tok/s</span>' +
        '<span class="us-dot">·</span><span class="us-total">本轮 0 tok</span>';
      host.parentNode.insertBefore(bar, host.nextSibling);
    }
    return bar;
  }

  function speedState(view) {
    if (!speed[view]) {
      speed[view] = {
        chars: 0,       // 流式过程中已收到的字符数（估算 token 用）
        firstAt: 0,     // 首个输出增量的时刻
        lastAt: 0,
        realTokens: 0,  // 本步真实输出 tokens（后端 usage）
        realTps: 0,
        frozen: false,  // 已定格（等淡出）
        timer: 0,
        raf: 0,
      };
    }
    return speed[view];
  }

  // estimateTokensOf 与后端 estimateTokens 同一启发式（中文为主：字符 × 3/4）
  function estimateTokensOf(chars) {
    return Math.round((chars * 3) / 4);
  }

  function prettyTok(n) {
    n = Number(n) || 0;
    if (n >= 1000) return (n / 1000).toFixed(1) + 'k';
    return String(Math.round(n));
  }

  function renderSpeed(view) {
    var st = speedState(view);
    var bar = ensureBar(view);
    if (!bar) return;
    var tps, total;
    if (st.frozen) {
      tps = st.realTps;
      total = st.realTokens || estimateTokensOf(st.chars);
    } else {
      var span = (Date.now() - st.firstAt) / 1000;
      var est = estimateTokensOf(st.chars);
      // 首字刚到时分母太小会把速度算爆：至少按 0.3 秒算
      tps = est / Math.max(0.3, span);
      total = est;
    }
    bar.querySelector('.us-tps').textContent = tps.toFixed(1) + ' tok/s';
    bar.querySelector('.us-total').textContent = '本轮 ' + prettyTok(total) + ' tok';
    bar.classList.remove('hidden', 'fade');
  }

  function scheduleRender(view) {
    var st = speedState(view);
    if (st.raf) return;
    st.raf = setTimeout(function () {
      st.raf = 0;
      renderSpeed(view);
    }, 100); // 每 100ms 刷一次，够顺滑又不跟流式事件抢主线程
  }

  function onStream(payload) {
    if (!payload || !payload.text) return;
    var view = payload.view === 'sub' ? 'sub' : 'main';
    var st = speedState(view);
    if (!st.firstAt) st.firstAt = Date.now();
    st.lastAt = Date.now();
    st.chars += String(payload.text).length;
    st.frozen = false;
    st.realTps = 0;
    st.realTokens = 0;
    if (st.timer) { clearTimeout(st.timer); st.timer = 0; }
    scheduleRender(view);
  }

  function onUsage(payload) {
    if (!payload) return;
    var view = payload.view === 'sub' ? 'sub' : 'main';
    var st = speedState(view);
    var d = payload.data || {};
    var out = Number(d.outputTokens) || 0;
    if (!out && !st.chars) return; // 没输出也没记录：不打扰
    var durMs = Number(d.durationMs) || 0;
    var ttftMs = Number(d.ttftMs) || 0;
    var windowMs = durMs - ttftMs;
    st.realTokens = out;
    // 吐字速度按「首字之后的输出窗口」算：总耗时里混着排队与思考启动会把速度压低
    st.realTps = windowMs > 0 ? (out * 1000) / windowMs : 0;
    st.frozen = true;
    var bar = ensureBar(view);
    if (bar) {
      bar.querySelector('.us-tps').textContent = st.realTps.toFixed(1) + ' tok/s';
      bar.querySelector('.us-total').textContent = '本轮 ' + prettyTok(out) + ' tok';
      bar.classList.remove('hidden', 'fade');
    }
    startFade(view);
  }

  // startFade 定格约 3 秒后淡出；期间又来新的输出会取消淡出（onStream 已清 timer）
  function startFade(view) {
    var st = speedState(view);
    if (st.timer) clearTimeout(st.timer);
    st.timer = setTimeout(function () {
      st.timer = 0;
      var bar = ensureBar(view);
      if (!bar) return;
      bar.classList.add('fade');
      st.timer = setTimeout(function () {
        st.timer = 0;
        var b2 = ensureBar(view);
        if (!b2) return;
        b2.classList.add('hidden');
        b2.classList.remove('fade');
        st.chars = 0;
        st.firstAt = 0;
        st.frozen = false;
        st.realTokens = 0;
        st.realTps = 0;
      }, SPEED_FADE_ANIM);
    }, SPEED_FADE_MS);
  }

  function onStreamEnd(payload) {
    var view = payload && payload.view === 'sub' ? 'sub' : 'main';
    var st = speedState(view);
    if (!st.firstAt) return; // 本轮没吐过字（工具轮/纯思考被折叠）：不显示
    if (st.frozen) return;   // 已由 usage 定格
    // 没有 usage（如失败/取消）：把估算值定格，同样 3 秒后淡出
    st.frozen = true;
    st.realTokens = estimateTokensOf(st.chars);
    var span = (st.lastAt - st.firstAt) / 1000;
    st.realTps = span > 0 ? st.realTokens / Math.max(0.3, span) : 0;
    renderSpeed(view);
    startFade(view);
  }

  // teardownSpeed 组件关闭时把速度条与定时器全部摘掉（界面恢复原样）
  function teardownSpeed() {
    ['main', 'sub'].forEach(function (view) {
      var st = speed[view];
      if (st && st.timer) clearTimeout(st.timer);
      if (st && st.raf) clearTimeout(st.raf);
      var bar = document.getElementById('us-speed-' + view);
      if (bar) bar.remove();
    });
    speed.main = null;
    speed.sub = null;
  }

  /* ================= 注册 ================= */

  CX.register({
    id: ID,

    init() {
      JX.on('chat:stream', onStream);
      JX.on('chat:usage', onUsage);
      JX.on('chat:stream-end', onStreamEnd);

      CX.registerSettings({
        id: ID,
        title: '用量统计',
        pane: {
          label: '用量',
          icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 3v18h18"/><path d="M7 15l4-5 3 3 5-7"/></svg>',
        },
        mount: mountSettings,
      });
    },

    dispose() {
      JX.off('chat:stream', onStream);
      JX.off('chat:usage', onUsage);
      JX.off('chat:stream-end', onStreamEnd);
      CX.unregisterSettings(ID);
      teardownSpeed();
    },
  });
})();
