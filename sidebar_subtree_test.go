package main

// 左侧栏「父会话行内折叠子对话」的回归（阿柒 2026-11-16 定稿的形态）：
//   - 箭头只切展开/收起，绝不切当前对话；父行本身点击（打开对话）语义不变；
//   - 展开后在父行下缩进列出该父名下**全部未归档**子会话（不只运行中），
//     运行中的子行带运行标记，跑完仍是普通子行（不留运行残标）；
//   - 收起时父行给「运行中的子代理数量」（⟳N），展开后明细替代计数——
//     同一行永远只有一处运行指示，不叠两套展示；
//   - 展开态在列表刷新（loadSessions → renderSessionList）后保留。
//
// 不复制被测逻辑：用 goja 跑 web/app-left.js 里「左侧栏：会话行内子对话折叠」
// 那一节的真实代码（alertsound_ui_inline_test.go 同款套路），openSession /
// rpOpen / openSubSession / renderSessionList 用替身记录调用顺序。

import (
	"os"
	"strings"
	"testing"

	"github.com/dop251/goja"
)

// sidebarSubHarness 最小替身：DOM 元素/片段工厂、会话列表状态、侧效记录器。
// 父行本身（sessionItem）由别处负责，这里只要一个占位元素即可。
const sidebarSubHarness = `
function assert(ok, msg) { if (!ok) throw Error(msg); }
const calls = [];  // 侧效记录：openSession:父id / rpOpen:视图 / openSubSession:子id / renderSessionList

function mkEl(tag) {
  return { tagName: tag, className: '', innerHTML: '', textContent: '', title: '',
    style: {}, dataset: {}, onclick: null, children: [],
    classList: {
      _s: new Set(),
      add(c) { this._s.add(c); },
      remove(c) { this._s.delete(c); },
      contains(c) { return this._s.has(c); },
      toggle(c, on) {
        const want = on === undefined ? !this._s.has(c) : !!on;
        if (want) this._s.add(c); else this._s.delete(c);
      },
    },
    appendChild(el) { this.children.push(el); return el; } };
}
const document = { createElement: (t) => mkEl(t), createDocumentFragment: () => mkEl('#fragment') };

const state = { sessions: [], cur: null };
function esc(s) {
  return String(s == null ? '' : s).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
}
function relTime() { return '刚刚'; }
function sessionItem(s) { const el = mkEl('div'); el.dataset.sid = s.id; return el; }
function renderSessionList() { calls.push('renderSessionList'); }
function openSession(id) { calls.push('openSession:' + id); }
function rpOpen(v) { calls.push('rpOpen:' + v); }
function openSubSession(id) { calls.push('openSubSession:' + id); }

// 造会话：parentId 空＝顶层，status 空＝idle，archived＝已进回收站
function mkSess(id, parentId, status, updatedAt, title) {
  return { id: id, parentId: parentId || '', status: status || 'idle',
    updatedAt: updatedAt || '2026-11-16 10:00:00', title: title || id, archived: false };
}
`

// runSidebarSub 跑 app-left.js 里那一节真实代码，chunks 逐段执行（段间让出）。
func runSidebarSub(t *testing.T, chunks ...string) *goja.Runtime {
	t.Helper()
	raw, err := os.ReadFile("web/app-left.js")
	if err != nil {
		t.Fatalf("读 web/app-left.js 失败: %v", err)
	}
	src := string(raw)
	start := strings.Index(src, "/* ---------- 左侧栏：会话行内子对话折叠")
	end := strings.Index(src, "/* ---------- 回收站视图")
	if start < 0 || end <= start {
		t.Fatalf("app-left.js 里找不到「左侧栏：会话行内子对话折叠」那一节（start=%d end=%d）——父行内折叠子会话还没实现", start, end)
	}
	vm := goja.New()
	if _, err := vm.RunString(sidebarSubHarness); err != nil {
		t.Fatalf("harness 跑不起来: %v", err)
	}
	if _, err := vm.RunString(src[start:end]); err != nil {
		t.Fatalf("子对话折叠那一节跑不起来: %v", err)
	}
	for _, c := range chunks {
		if _, err := vm.RunString(c); err != nil {
			t.Fatal(err)
		}
	}
	return vm
}

// 有子会话的父行才出箭头；默认收起、不产子行；没有子会话的行不出箭头（标题占位对齐）。
func TestSidebarSubTreeCollapsedByDefault(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle', '2026-11-16 10:00:00', '父会话'),
			mkSess('k1', 'p', 'running', '2026-11-16 11:00:00', '在跑的子代理'),
			mkSess('k2', 'p', 'idle', '2026-11-16 09:00:00', '干完了的子代理')];
		const parent = subTreeOf(state.sessions[0], state.sessions, subOpen);
		assert(parent.has === true, '有子会话的父行要出折叠箭头');
		assert(parent.open === false, '默认必须收起');
		assert(parent.rows.length === 0, '收起时不产子行，实际 ' + parent.rows.length);
		assert(parent.runN === 1, '收起态要能算出运行中的子代理数量，实际 ' + parent.runN);
		const lone = subTreeOf(mkSess('x', '', 'idle'), state.sessions, subOpen);
		assert(lone.has === false, '没有子会话不该出箭头');
		assert(lone.runN === 0 && lone.rows.length === 0, '没有子会话不该有计数/子行');
	`)
}

// 展开后列出全部未归档子会话（含已完成的），按更新时间倒序，运行状态各自正确。
func TestSidebarSubTreeExpandedListsAllKids(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle', '2026-11-16 08:00:00', '父会话'),
			mkSess('k1', 'p', 'running', '2026-11-16 11:00:00', '在跑'),
			mkSess('k2', 'p', 'idle', '2026-11-16 09:00:00', '完成'),
			(() => { const s = mkSess('k3', 'p', 'idle', '2026-11-16 12:00:00', '已归档'); s.archived = true; return s; })()];
		toggleSubOpen('p');
		const tree = subTreeOf(state.sessions[0], state.sessions, subOpen);
		assert(tree.open === true, '点箭头后要展开');
		assert(tree.rows.length === 2, '展开后要列出全部未归档子会话（不只运行中），实际 ' +
			JSON.stringify(tree.rows.map((r) => r.id)));
		assert(tree.rows[0].id === 'k1' && tree.rows[1].id === 'k2', '按更新时间倒序，实际 ' +
			JSON.stringify(tree.rows.map((r) => r.id)));
		assert(tree.rows[0].running === true, '运行中的子行要带运行标记');
		assert(tree.rows[1].running === false, '已完成的子行不该带运行标记');
		assert(tree.rows.every((r) => r.parentId === 'p' && r.depth === 1), '子行要记直系父与缩进深度');
	`)
}

// 多级子会话按 parent 链平铺、逐级缩进（不另加一套嵌套箭头）。
func TestSidebarSubTreeMultiLevel(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle', '2026-11-16 08:00:00', '父'),
			mkSess('c1', 'p', 'idle', '2026-11-16 11:00:00', '子'),
			mkSess('g1', 'c1', 'running', '2026-11-16 12:00:00', '孙')];
		toggleSubOpen('p');
		const tree = subTreeOf(state.sessions[0], state.sessions, subOpen);
		assert(tree.rows.length === 2, '多级要一起平铺，实际 ' + JSON.stringify(tree.rows.map((r) => r.id)));
		assert(tree.rows[0].id === 'c1' && tree.rows[0].depth === 1, '第一级缩进 1');
		assert(tree.rows[1].id === 'g1' && tree.rows[1].depth === 2, '第二级缩进 2');
		assert(tree.runN === 1, '深层在跑的子代理也要计入数量');
	`)
}

// 箭头只切展开态：不打开/切换当前对话（父行点击语义原样保留）。
func TestSidebarSubToggleOnlyToggles(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle'), mkSess('k1', 'p', 'running')];
		toggleSubOpen('p');
		assert(subOpen.has('p') === true, '点一次应展开');
		toggleSubOpen('p');
		assert(subOpen.has('p') === false, '再点一次应收起');
		assert(calls.indexOf('renderSessionList') >= 0, '展开收起要就地重绘侧栏');
		const opened = calls.filter((c) => c.indexOf('openSession') === 0 || c.indexOf('openSubSession') === 0);
		assert(opened.length === 0, '箭头绝不能切当前对话，实际调了 ' + JSON.stringify(opened));
	`)
}

// 收起态给运行中的子代理数量（⟳N）；展开态明细在场，计数不再出现（不叠两套展示）。
func TestSidebarSubRunChipCollapsedOnly(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle', '2026-11-16 08:00:00'),
			mkSess('k1', 'p', 'running', '2026-11-16 11:00:00'),
			mkSess('k2', 'p', 'running', '2026-11-16 11:30:00')];
		const closed = subTreeOf(state.sessions[0], state.sessions, subOpen);
		assert(subRunChip(closed) === '⟳2', '收起且有名下在跑时要给数量，实际 ' + subRunChip(closed));
		toggleSubOpen('p');
		const opened = subTreeOf(state.sessions[0], state.sessions, subOpen);
		assert(subRunChip(opened) === '', '展开后明细就在下面，父行不再出计数胶囊，实际 ' + subRunChip(opened));
		assert(opened.runN === 2, '数量本身仍要对（供别处用）');
	`)
}

// 子行渲染：运行中的带运行标记，其余不带；标题转义；深度写进 dataset 供缩进。
func TestSidebarSubRowMarksRunningOnly(t *testing.T) {
	runSidebarSub(t, `
		const run = subRowHTML({ id: 'k1', title: '在跑', running: true, updatedAt: '2026-11-16 11:00:00' });
		const done = subRowHTML({ id: 'k2', title: '<done>', running: false, updatedAt: '2026-11-16 09:00:00' });
		assert(run.indexOf('s-run') >= 0, '运行中的子行要有运行标记');
		assert(done.indexOf('s-run') < 0, '已完成的子行不该留运行标记（结束即清残标）');
		assert(done.indexOf('&lt;done&gt;') >= 0, '标题要转义，实际 ' + done);
		assert(run.indexOf('在跑') >= 0 && done.indexOf('刚刚') >= 0, '子行要有标题与相对时间');
	`)
}

// 父行 + 展开时紧跟其后的缩进子行：三个列表视图共用这一处装配。
func TestSidebarSubBlockAssemblesKids(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle', '2026-11-16 08:00:00'),
			mkSess('k1', 'p', 'running', '2026-11-16 11:00:00'),
			mkSess('k2', 'p', 'idle', '2026-11-16 09:00:00')];
		const closed = sessionBlock(state.sessions[0]);
		assert(closed.children.length === 1, '收起时只有父行本身，实际 ' + closed.children.length);
		toggleSubOpen('p');
		const opened = sessionBlock(state.sessions[0]);
		assert(opened.children.length === 3, '展开后＝父行 + 两个子行，实际 ' + opened.children.length);
		assert(opened.children[0].dataset.sid === 'p', '第一行仍是父会话本身');
		assert(opened.children[1].dataset.sid === 'k1' && opened.children[1].dataset.depth === '1',
			'子行要紧跟父行、带缩进深度');
		assert(opened.children[1].className.indexOf('s-child') >= 0, '子行要有自己的样式类');
		assert(typeof opened.children[1].onclick === 'function', '子行要可点（点开落右栏那个子会话）');
	`)
}

// 子行点击：打开直系父会话 → 右栏切到子对话视图 → 定位该子会话；已在父会话则不重复打开。
func TestSidebarSubRowClickJumps(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle'), mkSess('k2', 'p', 'idle')];
		state.cur = { id: 'other' };
		openSubRow('k2');
		assert(calls.join('|') === 'openSession:p|rpOpen:subs|openSubSession:k2',
			'子行点击要先落到父会话、再在右栏定位子会话，实际 ' + calls.join('|'));
		calls.length = 0;
		state.cur = { id: 'p' };
		openSubRow('k2');
		assert(calls.join('|') === 'rpOpen:subs|openSubSession:k2',
			'已经在父会话时不该重复加载主区，实际 ' + calls.join('|'));
	`)
}

// 展开出来的子行不算方向键条目：子会话只在右栏显示、绝不切主对话区（app-hotkeys.js
// 的 switchConversation 要排除 .s-child——子行与顶层行同用 .session-item 样式类，
// 漏了这条过滤方向键就会把子会话开进主区）。
func TestSidebarSubNotInArrowNav(t *testing.T) {
	raw, err := os.ReadFile("web/app-hotkeys.js")
	if err != nil {
		t.Fatalf("读 web/app-hotkeys.js 失败: %v", err)
	}
	src := string(raw)
	if !strings.Contains(src, "!el.classList.contains('s-child')") {
		t.Fatalf("app-hotkeys.js 的方向键条目查询没排除 .s-child：子会话行会被 ↑↓ 开进主区（应只走 openSubRow）")
	}
}

// 列表刷新（重新拉一份 state.sessions 再算）后展开态保留；子会话跑完不留运行残标。
func TestSidebarSubKeepsOpenAcrossRefresh(t *testing.T) {
	runSidebarSub(t, `
		state.sessions = [mkSess('p', '', 'idle'), mkSess('k1', 'p', 'running', '2026-11-16 11:00:00')];
		toggleSubOpen('p');
		state.sessions = [mkSess('p', '', 'idle'), mkSess('k1', 'p', 'idle', '2026-11-16 11:00:00')];
		const after = subTreeOf(state.sessions[0], state.sessions, subOpen);
		assert(after.open === true, '列表刷新后展开态要保留（用户点开的不该自己收回去）');
		assert(after.rows.length === 1, '跑完的子会话仍保留为普通子行，实际 ' + after.rows.length);
		assert(after.rows[0].running === false, '跑完后不留运行标记');
		assert(after.runN === 0 && subRunChip(after) === '', '跑完后父行不再显示运行中的子代理数量');
	`)
}
