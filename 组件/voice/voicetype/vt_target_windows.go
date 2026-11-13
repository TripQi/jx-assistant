//go:build windows

package main

// 「这段话该上屏还是该进剪贴板」的判定（VoiceTypeGo 的核心体验点）。
//
// 时机：与 VoiceType(Electron 版) 一致——**结束时**判定，录音全程不碰焦点。
// 判据：四层，从最可靠到最兜底，任何一层命中即给结论，全部落空就只复制：
//
//	0. 自己进程的窗口（悬浮窗/WebView 子窗口）→ 明确拒绝（绝不往自己身上粘）
//	   —— 这一层是 Go 版必须补的：我们的悬浮窗窗口类名就叫 "webview"，老代码
//	      把 "webview" 当"Chromium 乐观放行"，悬浮窗一旦抢到前台就会被判成
//	      "可输入"，Ctrl+V 发到空处，用户看到的就是"输入到哪里去了"。
//	1. explorer 的桌面/图标列表 → 明确拒绝（桌面有 focus 但那里不是输入目标）
//	2. caret（GetGUIThreadInfo 的 hwndCaret + rcCaret 尺寸）→ 可输入
//	   原生可编辑控件类名（Edit/RichEdit/Scintilla/_WwG…）→ 可输入
//	3. UIA 焦点元素（Chromium 系唯一看得清的信号）：
//	   Edit/ComboBox 且启用、非只读 → 可输入
//	   Document/Custom/Group/Pane 且带 ValuePattern 非只读 → 可输入（富文本）
//	   带 ValuePattern 但**只读** → 明确拒绝（网页正文就是这种：Document+只读）
//	   Chromium 窗口里键盘可聚焦、带 TextPattern、又不是明显非输入控件 → 乐观放行
//	   （TextPattern 是 contenteditable 与滚动容器的分水岭：网页点空白处焦点常落在
//	     滚动容器 Group 上，它无 TextPattern，不能放行）
//	4. 无 UIA 信号的 Qt 自绘应用（微信等）→ 进程名兜底
//
// 与门禁口径一致的取舍：**能确认才注入**。判不准就落剪贴板，用户手动粘贴，
// 绝不"往随便哪里打一段字"。

import (
	"encoding/binary"
	"fmt"
	"os"
	"strings"
	"unsafe"
)

// vtTarget 一次判定所需的全部现场（结束后采集一次）。
type vtTarget struct {
	Hwnd        uintptr // 前台窗口
	Pid         uint32
	ProcName    string // "msedge.exe"
	WindowClass string // "Chrome_WidgetWin_1"
	FocusHwnd   uintptr
	FocusClass  string
	HasCaret    bool
	Uia         vtUiaInfo
	When        string // "录音结束" / "注入前"（只进日志）
}

// vtGate 判定结论，同时记录「是哪一层给出的」——出问题只看日志就能定位。
type vtGate int

const (
	vtGateNoForeground       vtGate = iota // 没有前台窗口
	vtGateOwnWindow                        // 前台是我们自己（悬浮窗）
	vtGateExplorerDesktop                  // 桌面/图标列表
	vtGateCaret                            // 系统报告了文本插入符
	vtGateNativeClass                      // 命中原生可编辑控件类名
	vtGateUiaEditable                      // UIA：Edit/ComboBox 且非只读
	vtGateUiaRichEditor                    // UIA：Document/Custom/Group/Pane + 可写 ValuePattern
	vtGateUiaReadOnly                      // UIA：可写控件但只读（网页正文等）
	vtGateMenuActive                       // UIA：焦点落在菜单/菜单栏/菜单项（按键被吃成菜单）
	vtGateChromiumOptimistic               // Chromium 窗口里键盘可聚焦的非输入控件之外
	vtGateProcessAllowlist                 // 无 UIA 信号的 Qt 应用，按进程名兜底
	vtGateNoSignal                         // 四层判据全部落空
)

// editable 能不能往这里打字。
func (g vtGate) editable() bool {
	switch g {
	case vtGateNoForeground, vtGateOwnWindow, vtGateExplorerDesktop, vtGateUiaReadOnly,
		vtGateMenuActive, vtGateNoSignal:
		return false
	}
	return true
}

// String 稳定标识：写进日志与 history，改名等于改日志契约。
func (g vtGate) String() string {
	switch g {
	case vtGateNoForeground:
		return "no_foreground"
	case vtGateOwnWindow:
		return "own_window"
	case vtGateExplorerDesktop:
		return "explorer_desktop"
	case vtGateCaret:
		return "caret"
	case vtGateNativeClass:
		return "native_class"
	case vtGateUiaEditable:
		return "uia_editable"
	case vtGateUiaRichEditor:
		return "uia_rich_editor"
	case vtGateUiaReadOnly:
		return "uia_read_only"
	case vtGateMenuActive:
		return "menu_active"
	case vtGateChromiumOptimistic:
		return "chromium_optimistic"
	case vtGateProcessAllowlist:
		return "process_allowlist"
	}
	return "no_signal"
}

// 原生可编辑控件类名（小写，子串匹配）。Word 的文档区类名是 _WwG。
var vtNativeEditableClasses = []string{
	"edit", "richedit", "richedit20w", "richedit50w",
	"scintilla", "texteditorsid", "_wwg",
}

// explorer 桌面相关的窗口类名：这些窗口里"有焦点"不等于"能打字"。
var vtExplorerDesktopClasses = []string{
	"progman", "workerw", "shelldll_defview", "syslistview32",
}

// Chromium 系（浏览器/Electron/WebView2）窗口类名：这类窗口 Win32 侧看不到
// caret，只能靠 UIA，所以单独识别出来走乐观放行分支。
var vtChromiumClasses = []string{
	"chrome_widgetwin_", "chrome_renderwidgethostview", "intermediate d3d window",
	"webview", "mozillawindowclass",
}

// 进程名兜底名单（小写、不带 .exe）。只放**UIA 也给不出信号**的应用——
// 典型是 Qt 自绘的微信（4.x 起没有 Win32 caret、没有子控件、UIA 只见顶层窗口）。
// 浏览器**刻意不在名单里**：UIA 能可靠区分网页正文与输入框，宁可判不准落剪贴板，
// 也不往网页空白处乱粘。
var vtEditableProcs = []string{
	"notepad", "notepad++", "winword", "excel", "powerpnt", "outlook", "onenote",
	"code", "cursor", "trae", "windsurf", "idea64", "sublime_text",
	"weixin", "wechat", "wxwork", "qq", "dingtalk",
	"windowsterminal", "conhost", "cmd", "powershell", "pwsh",
	"mobaxterm", "putty", "xshell", "securecrt",
}

// 明确不是输入控件的 UIA 控件类型：Chromium 乐观放行时要排除掉。
var vtDefinitelyNotEditable = map[int32]bool{
	uiaCtButton: true, uiaCtMenu: true, uiaCtMenuBar: true, uiaCtMenuItem: true,
	uiaCtTab: true, uiaCtTabItem: true, uiaCtToolBar: true, uiaCtTitleBar: true,
	uiaCtScrollBar: true, uiaCtImage: true, uiaCtHyperlink: true, uiaCtStatusBar: true,
	uiaCtHeader: true, uiaCtHeaderItem: true, uiaCtSeparator: true, uiaCtProgressBar: true,
}

// vtCaptureTarget 采集前台现场。withUIA=false 时只取 Win32 便宜的几项
// （UIA 要起 COM，十几毫秒，录音开始那一刻不值得等）。
func vtCaptureTarget(withUIA bool, when string) vtTarget {
	t := vtTarget{When: when}
	t.Hwnd = vtForegroundWindow()
	if t.Hwnd == 0 {
		return t
	}
	procGetWindowThreadProcessId.Call(t.Hwnd, uintptr(unsafe.Pointer(&t.Pid)))
	t.WindowClass = vtWindowClass(t.Hwnd)
	t.ProcName = vtProcessName(t.Pid)

	// GUITHREADINFO(x64)：cbSize0 flags4 hwndActive8 hwndFocus16 hwndCapture24
	// hwndMenuOwner32 hwndMoveSize40 hwndCaret48 rcCaret56(4×int32)
	gti := make([]byte, 72)
	binary.LittleEndian.PutUint32(gti, 72)
	tid, _, _ := procGetWindowThreadProcessId.Call(t.Hwnd, 0)
	if tid != 0 {
		if r, _, _ := procGetGUIThreadInfo.Call(tid, uintptr(unsafe.Pointer(&gti[0]))); r != 0 {
			t.FocusHwnd = *(*uintptr)(unsafe.Pointer(&gti[16]))
			if t.FocusHwnd != 0 {
				t.FocusClass = vtWindowClass(t.FocusHwnd)
			}
			caret := *(*uintptr)(unsafe.Pointer(&gti[48]))
			w := int32(binary.LittleEndian.Uint32(gti[64:])) - int32(binary.LittleEndian.Uint32(gti[56:]))
			h := int32(binary.LittleEndian.Uint32(gti[68:])) - int32(binary.LittleEndian.Uint32(gti[60:]))
			// 有 caret 句柄且矩形非空才算「有文本光标」——只认句柄会误判
			// （部分窗口有 caret 但尺寸为 0，那是隐藏光标）。
			t.HasCaret = caret != 0 && (w > 0 || h > 0)
		}
	}
	if withUIA {
		t.Uia = vtUiaFocused()
	}
	return t
}

// vtEditabilityGate 判定结论（唯一实现，别在别处再写一份 bool 版）。
func vtEditabilityGate(t vtTarget) vtGate {
	if t.Hwnd == 0 {
		return vtGateNoForeground
	}
	// 0) 自己的窗口：悬浮窗主窗口类名就叫 "webview"，WebView2 子窗口是
	//    Chrome_WidgetWin_1——不按进程剔除的话，悬浮窗抢到前台就会被当成
	//    可编辑目标（历史 bug：Ctrl+V 粘到空处）。
	if t.Pid == uint32(os.Getpid()) {
		return vtGateOwnWindow
	}
	wc := strings.ToLower(t.WindowClass)
	fc := strings.ToLower(t.FocusClass)
	proc := strings.ToLower(strings.TrimSuffix(t.ProcName, ".exe"))

	// 1) 桌面/图标列表：explorer 的 Progman/WorkerW 有 focus，但不是输入目标
	if strings.Contains(proc, "explorer") {
		for _, c := range vtExplorerDesktopClasses {
			if fc == c || wc == c {
				return vtGateExplorerDesktop
			}
		}
	}

	// 2) caret / 原生可编辑控件
	if t.HasCaret {
		return vtGateCaret
	}
	for _, c := range vtNativeEditableClasses {
		if strings.Contains(fc, c) || strings.Contains(wc, c) {
			return vtGateNativeClass
		}
	}

	// 3) UIA
	u := t.Uia
	if u.OK {
		// 菜单优先判：焦点落在菜单上说明按键被前台程序吃成了"打开菜单"，
		// 这不是输入目标（此时 caret 也可能存在，所以必须排在 caret 之后但仍
		// 在进程兜底之前，否则微信这类按进程放行的应用会把字粘到菜单上）。
		if vtIsMenuControlType(u.ControlType) || strings.Contains(fc, "menu") {
			return vtGateMenuActive
		}
		switch u.ControlType {
		case uiaCtEdit, uiaCtComboBox:
			if u.Enabled && !u.Password {
				if u.HasValuePattern && u.ValueReadOnly {
					return vtGateUiaReadOnly // 只读输入框
				}
				return vtGateUiaEditable
			}
		case uiaCtDocument, uiaCtCustom, uiaCtGroup, uiaCtPane:
			if u.Enabled && u.HasValuePattern {
				if u.ValueReadOnly {
					return vtGateUiaReadOnly // 网页正文正是这种
				}
				return vtGateUiaRichEditor
			}
			// 乐观放行必须带 TextPattern（2026-11-11 实测，同一页面同机对照）：
			// contenteditable 富文本（ql-editor）与网页滚动容器（ds-virtual-list）
			// 都是 Group+无ValuePattern+可聚焦，唯一区分度是 TextPattern——
			// 前者 true、后者 false。不带这条，DeepSeek 页点空白处焦点落在
			// 滚动容器上会被判可输入，Ctrl+V 落空还显示"已输入"。
			if u.Enabled && u.KeyboardFocusable && u.HasTextPattern && vtIsChromiumWindow(wc, fc) &&
				!vtDefinitelyNotEditable[u.ControlType] {
				return vtGateChromiumOptimistic
			}
		}
		// 有 ValuePattern 但只读：无论控件类型都明确拒绝（在兜底之前）
		if u.HasValuePattern && u.ValueReadOnly {
			return vtGateUiaReadOnly
		}
	}

	// 4) 进程名兜底（只给 UIA 无信号的 Qt 自绘应用）
	for _, p := range vtEditableProcs {
		if proc == p || strings.HasPrefix(proc, p) {
			return vtGateProcessAllowlist
		}
	}
	return vtGateNoSignal
}

// vtIsMenuControlType UIA 控件类型是不是菜单类（菜单/菜单栏/菜单项）。
// 这三类是"按键被前台吃成了开菜单"最直接的证据。
func vtIsMenuControlType(ct int32) bool {
	switch ct {
	case uiaCtMenu, uiaCtMenuBar, uiaCtMenuItem:
		return true
	}
	return false
}

// vtIsChromiumWindow 前台窗口是不是 Chromium 系（浏览器/Electron/WebView2）。
func vtIsChromiumWindow(windowClass, focusClass string) bool {
	for _, c := range vtChromiumClasses {
		if strings.Contains(focusClass, c) || strings.Contains(windowClass, c) {
			return true
		}
	}
	return false
}

// describe 一行完整现场（进 vt.log）：结论 + 四层判据各自的输入。
func (t vtTarget) describe() string {
	u := t.Uia
	uiaDesc := "uia=未探测"
	if u.OK {
		uiaDesc = fmt.Sprintf("uia=%s(%d)", vtUiaControlTypeName(u.ControlType), u.ControlType)
		if u.LocalizedType != "" {
			uiaDesc += "/" + u.LocalizedType
		}
		if u.ClassName != "" {
			uiaDesc += " cls=" + u.ClassName
		}
		if u.HasValuePattern {
			uiaDesc += fmt.Sprintf(" 可写=%v", !u.ValueReadOnly)
		} else {
			uiaDesc += " 可写=无ValuePattern"
		}
		uiaDesc += fmt.Sprintf(" 文本模式=%v 可聚焦=%v 启用=%v", u.HasTextPattern, u.KeyboardFocusable, u.Enabled)
		if u.Password {
			uiaDesc += " 密码框"
		}
	}
	return fmt.Sprintf("%s采集: 窗口类=%q 进程=%q 焦点类=%q 有caret=%v %s",
		t.When, t.WindowClass, t.ProcName, t.FocusClass, t.HasCaret, uiaDesc)
}
