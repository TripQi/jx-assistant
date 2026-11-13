# VoiceTypeGo —— 独立语音输入法（Go + WebView2）

零主程序依赖的本地语音输入法：按住热键说话 → 松开 → 识别文字注入当前输入框
（**没有聚焦输入框的窗口则只进剪贴板**，绝不往空白处打字）。模型本地推理
（小米 X-ASR + silero VAD），全程离线。

## 使用

1. 双击 `启动输入法.bat`（或 `voicetype_go.exe`）。单实例：已经在跑时再启动会自己退出
   （两个实例＝两套键盘钩子 + 两个悬浮窗 + 抢同一个 ASR 端口，会粘两遍）
2. 默认热键：**Alt 按住说话（两侧都算，≥300ms 才开麦），松开上屏**
   - `main`：主热键，如 `["alt"]`、`["ctrl","f2"]`、`["mouse-side"]`（鼠标侧键，前后两枚都算）
     —— 也可只绑某一枚：`["mouse-side-1"]`（侧键1·后退位）/ `["mouse-side-2"]`（侧键2·前进位），
     没绑的那枚完全放行（原生前进/后退照常）；修饰键写 `alt`/`ctrl` 表示**两侧都算**
     （与产品设置一致）；也可以只写一侧 `lalt`/`ralt`
   - `sub`：副热键（与主键并存触发），留空 `[]` 停用
   - `mode`：`tap`=点一下开始、再点一下结束（**点按上限 `tapMs`，默认 1000ms**：
 按住不超过它就算"点按"；超过也照样切换，只在日志里标注"按住较久"，不留死区）；
 `hold`=按住说话（**按住超过 `holdMs` 默认 300ms 才开麦**，松开结束，短按不录音）
   - `holdMs`：长按阈值（默认 300）。**短于它的点按不录音**——`hold` 模式下会把这次按键
     原样回放给前台程序（`replayShortTap`，默认开），所以"点一下 Alt"的原生行为照旧
   - `avoidHost` / `avoidHostNames`：前台是本产品主程序时不响应热键（把听写让给
     产品自己的按钮），默认开；产品由主程序拉起时还会注入 `VT_HOST_PID`，与进程名无关
3. 退出：任务管理器结束 `voicetype_go.exe`，或命令行 `taskkill /f /im voicetype_go.exe`

## 与主程序联动（热键 + 主题色都从主程序取）

输入法是独立进程，读不到主程序页面的 localStorage，所以约定一个**专用小文件**
`voicetype.json`（只含热键与颜色，任何密钥都不进它）：

```json
{ "hotkeys": {"main": ["alt"], "sub": ["mouse-side"]},
  "holdMs": 300, "accentColor": "#4d6bfe", "textColor": "#1f2328", "updatedAt": "…" }
```

- **来源优先级**：环境变量 → 本目录 `voicetype.json` → `..\坚祥数字助理GO\voicetype.json`
  → `D:\123\坚祥数字助理GO\voicetype.json` → 本目录 `hotkeys.json` → 内置默认
  - 环境变量：`VT_VOICETYPE_JSON`（文件路径）、`VT_HOTKEY`/`VT_SUB_HOTKEY`（如 `alt`、`ctrl+f2`）、
    `VT_ACCENT`、`VT_TEXT_COLOR`、`VT_HOLD_MS` —— 组件模式下主程序拉起 worker 时直接注入最可靠
- **热加载**：两个文件（`hotkeys.json` / `voicetype.json`）每秒看 mtime，改完 1 秒内生效
  （日志：`热键来源=主程序联动（...）`）
- **主题**：胶囊/气泡底色 = 主程序「点缀色」`accentColor`，文字 = 主程序主题文字色
  `textColor`（浅色主题 `#1f2328` / 深色 `#e6e8eb`）；`textColor` 缺失时按点缀色亮度
  自动挑一个对比度够的。日志：`主题: 点缀色=#4d6bfe 文字色=#1f2328（来源：…）`
- **主程序侧写入**（已完成，需重建主程序才生效）：`POST /api/voicetype/link`
  （`main`/`sub`/`accentColor`/`textColor` 均可选，合并写），调用点两处：
  听写快捷键变化（`web/app-hotkeys.js` 的 `HK._changed`）、点缀色保存
  （`web/app-settings.js` 的 `applyAccent`）；另外 `POST /api/config` 改 `accentColor` 时
  Go 侧也会顺手同步一次。实现见主程序 `voicetype_link.go`。

## 热键为什么要「吞键」

**旧版根本问题**：低级键盘钩子无论命中与否都 `CallNextHookEx` 放行 —— 按住的 Alt 被前台
程序收到，Windows 把它判成"点了一下 Alt" → **激活菜单栏、焦点从输入框跳到菜单** →
随后注入的 Ctrl+V 落在菜单上（`SendInput` 仍报成功，形成假成功）。所以会出现
"字没进输入框"，而且各家行为不一致（有的应用临时激活菜单、松手又还焦点，于是"有时能进"）。

现在的规则照 SayIt（`keyboard/mod.rs`）：

- 命中热键 → **吞掉**（`return 1`，不调 `CallNextHookEx`）：吞 down 与配对的 up；
  **Alt 类键的 up 无条件吞**（否则抬起仍会被判成"点了 Alt"）
- **两道闸门都在"切换/长按"之前**（与 SayIt 同一口径）：
  - **不到阈值（默认 300ms）一律不算数**：不开始、不切换，把这次按键**原样回放**给
    前台程序 —— 轻点 Alt 照常弹菜单、轻点鼠标侧键照常后退，**任何模式都一样**
  - **判定期内按了别的键 = 组合快捷键，整轮作废**：不录音，并把已经吞掉的那个键
    **原样还给前台**（只补 down，真实 up 照常放行）—— 否则 Alt+Tab 会变成光秃秃的 Tab、
    Ctrl+C 会变成光秃秃的 C。日志：`判定期内按下其它键 "tab"：按组合快捷键处理，本轮不录音`
  - 已经开麦之后按了别的键同样作废：**取消本轮**（音频丢弃、不识别、不注入），
    日志 `取消本轮录音（录音期间按下其它键 tab）：音频丢弃、不注入`
- 通过阈值才进入触发，此时才按 `mode` 决定松手行为：
  `hold` = 松开结束；`tap` = 免提切换，松开继续录、**再按一次**结束
- 按下时已有其它修饰键（Alt+F4、Ctrl+F2、AltGr 的左Ctrl+右Alt）→ **整条放行、不录音**
- 组合热键只吞主键，且要求修饰键先按住；修饰键自身永不吞（不制造孤立 up）
- 绑了鼠标侧键时侧键 down/up 都吞（否则每次说话都等于给浏览器一次"后退"）
- **宿主（本产品主程序）在前台时一个键都不吞**（避让逻辑不变；前台判定由 150ms 轮询
  提供，钩子回调里不查进程名，避免回调超时被系统摘钩）
- 判定绝不替用户按多余的键：注入前焦点落在菜单上（`menu_active`）就当"不在
  输入框"处理，只复制——不给用户 Esc 收菜单之类的画蛇添足
- 钩子看门狗：每 60s 重装一次钩子（`WH_KEYBOARD_LL` 回调超时会被系统**静默摘除**，
  之后热键全废且不报错），并留「上次按键回调多少秒前」的日志
- 键集自愈：漏 up 用 `GetAsyncKeyState` 校正，避免组合永久"按住"导致热键失灵

**取舍**：热键两侧都算时，按住期间该侧 Alt 由输入法接管——**短于 300ms 的轻点会原样回放**，
300ms 内按别的键会把按键还给前台，所以菜单栏、Alt+Tab、Ctrl 组合全都照常；只有在
"按住 Alt 超过 300ms 说话"这段时间里 Alt 不会弹菜单（这正是我们要的）。

## 连按不再像"有冷却期"

每轮录音开始自增**会话代数**（`vtSessionGen`），所有延时回调（结果提示 700ms、
流字气泡 1.4s、识别中的状态、延时收起）都带代数校验，过期一律丢弃。
提示该停留多久仍停留多久，只是它再也不会关掉/覆盖**下一轮**刚弹出的胶囊
（日志：`丢弃过期收起：代数=1 当前=2`）。

## 「上屏还是进剪贴板」的判定

时机：**识别完成、注入前一刻**才判定，录音全程不抢焦点。识别要几百毫秒到数秒，
录音开始/结束时的焦点都可能已变——历史 bug 就是"同一窗口沿用录音开始的结论"，
开始时在输入框、结束时焦点移到网页正文，仍判可输入注入，Ctrl+V 落空还显示
"已输入"。现在"检测焦点 → 分输入/复制"收拢在一个函数 `vtDeliverAfterAsr` 里：
识别完立刻检测此刻焦点并分发，不加任何人为延时，也**绝不替用户按多余的键**
（焦点落在菜单上=不在输入框，只复制）。录音结束时的现场只作悬浮窗抢前台时的
兜底（`vtPickTarget`，纯逻辑测试 `TestPickTargetPure` 钉住）。
判据四层，命中即给结论，**全部落空就只复制**（判不准宁可让用户手动粘贴，
绝不往随便哪里打一段字）：

| 层 | 判据 | 放行 |
|---|---|---|
| 0 | 前台窗口属于本进程（悬浮窗自己） | 否 |
| 1 | explorer 的桌面/图标列表（Progman / WorkerW / SHELLDLL_DefView / SysListView32） | 否 |
| 2 | `GetGUIThreadInfo` 有 caret（含 rcCaret 尺寸非 0） / 原生可编辑控件类名（Edit / RichEdit* / Scintilla / _WwG） | 是 |
| 3 | UIA 焦点元素：Edit、ComboBox 且启用非只读 → 是；Document/Custom/Group/Pane 带**可写** ValuePattern → 是；带 ValuePattern 但**只读**（网页正文就是这种）→ 否；Chromium 窗口里键盘可聚焦、**带 TextPattern** 且不是按钮/菜单/滚动条一类 → 是（contenteditable 富文本如 ql-editor 就是这种；滚动容器无 TextPattern，不放行） | 视情况 |
| 4 | 进程名兜底（只给 UIA 给不出信号的 Qt 自绘应用，如微信 4.x） | 是 |

为什么必须留着第 0 层和第 3 层（Go 版相对 Electron 版的两处差异，都是实测坑）：

- **第 0 层**：Electron 版悬浮窗是 `focusable:false`，永不抢焦点；Go 版悬浮窗是
  WebView2 原生窗口，主窗口类名恰好是 `webview`——老代码把 `webview` 当
  "Chromium 乐观放行"，悬浮窗一旦抢到前台就被判成"可输入"，Ctrl+V 粘到空处。
- **第 3 层**：Electron 版靠 `Chrome_WidgetWin_\d` 类名 + `focus>0` 放行；实测
  Chromium 窗口在网页正文聚焦时 `hwndFocus` 就非 0（等于窗口自己）、`caret=0`，
  那两条判据在浏览器里等于恒真，于是"没输入框也粘"。UIA 能区分：网页正文是
  `Document` + ValuePattern 只读，网页 textarea / WebView2 输入框是 `Edit` + 可写。

实测对照（2026-11-03，本机；最后一行 2026-11-11 补）：

| 场景 | 判定 | 结果 |
|---|---|---|
| 记事本 | `caret` | 注入 |
| 桌面 | `explorer_desktop` | 只复制 |
| Edge 网页正文（无输入框） | `uia_read_only` | 只复制 |
| Edge 网页里的 textarea | `uia_editable` | 注入 |
| 数字助理输入框（WebView2） | `uia_editable` | 注入 |
| 微信 4.x（Qt，UIA 无信号） | `process_allowlist` | 注入 |
| 网页滚动容器（DeepSeek 点空白处，焦点落 ds-virtual-list：Group、无 ValuePattern、**无 TextPattern**） | `no_signal` | 只复制 |
| contenteditable 富文本（ql-editor：Group、无 ValuePattern、**有 TextPattern**） | `chromium_optimistic` | 注入 |

> Group/Pane 无 ValuePattern 时，TextPattern 是 contenteditable（可输入）与滚动容器
> （不可输入）的唯一区分度——两者同为「分组 + 可聚焦」，2026-11-11 同机同页实测
> 确认。滚动容器放进剪贴板就是它落到 `no_signal`（浏览器刻意不在进程兜底名单）。

判定实现只有一份：`vt_target_windows.go` 的 `vtEditabilityGate`（别处不要再写
bool 版，两份判据迟早漂移）。`vt.log` 每次都会记一行 `目标判定: gate=... | 现场`，
`gate` 就是上表的层名——排查"该复制却注入了"看这一行即可。

## 调试钩子与测试

目录里放 `trigger.txt`（内容见下，处理完即删），用于自动化自测，不用真对着麦克风说话：

| 内容 | 作用 |
|---|---|
| `start` / `stop` | 直接开始/结束一轮录音（跳过 300ms 长按判定） |
| `tap` | 模拟一次点按（按下即松开）→ 应走 `holdMs` 以内的分支（不录音/回放） |
| `hold` | 模拟一次长按（按下 → 过阈值 → 松开）→ 应看到"录音开始（长按）" |
| `combo` | 模拟「判定期内按了别的键」→ 应看到"按组合快捷键处理，本轮不录音" |
| `combrec` | 模拟「录音期间按了别的键」→ 应看到"取消本轮录音…音频丢弃、不注入" |
| `probe` | 把当前前台的完整现场与判定结论写进 `vt.log`（排查"该复制却注入了"） |
| `dry 任意文字` | 走正式流程同一个判定 + 注入/复制实现输出这段文字（不录音不识别） |
| `reid n` | 重新识别历史第 n 条（0 起）：读留档音频重跑识别 → 更新记录 → 走完整分发（设置页「最近语音记录」的重新识别走的就是它） |

```
go test ./...                    # 判定逻辑 + 吞键规则 + 真窗口验收（记事本=注入、桌面=只复制）
$env:VT_PROBE_HWND="<hwnd>"; go test -run TestCaptureForeground -v   # 指定窗口的判定现场
```

## 目录里那些"陌生文件"不是本程序的

搜狗拼音输入法有个老毛病：会往**当前前台进程的工作目录**里丢自己的用户数据。本程序
（双击启动、CWD=程序目录）跑起来以后，目录里可能陆续出现：

| 文件 | 是什么 |
|---|---|
| `user.config` / `share.config` | 搜狗的用户配置（二进制，文件头 `45 A1 E1 E3`） |
| `inputstat.dat` | 搜狗的输入统计（文件头 `SIPQ` = Sogou Input PinYin Query） |
| `sgim_punc.bin` | 搜狗的标点/联想数据（`sgim` = Sogou IM） |
| `image.<版本>.db` | 搜狗的表情/图片索引库 |
| `籰╒翾user.config` 这类怪名字文件 | 搜狗按 GBK 写文件名、被当成 UTF-8 显示的重复副本 |

它们和语音输入法无关，**可以随手删**（删了搜狗会写回它自己的目录，不影响任何功能）。
本程序自己只写 `vt.log`、`history.json`、`history/`（历史音频，临时还有 `trigger.txt`、`asr.log`）。

## 最近语音记录（history.json + history/）

每轮语音结束都留一条（**识别失败、没听清的也在**——音频是重新识别的原料，
说过的话不该因为哪一环失败就丢）：`time`（时间）、`text`（文字，失败为空）、
`status`（injected=已输入 / copied=已复制 / asr_failed=识别失败 / empty=没听清）、
`audio`（原始语音 wav，`history/` 子目录）。滚动保留最近 3 条，被挤出的音频文件
一并删除。主程序设置页「语音」底部读它展示（试听 / 复制 / 重新识别，
`/api/voicetype/history*`，见主程序 `voicetype_history.go`）。

## 目录内容

| 文件 | 作用 |
|---|---|
| voicetype_go.exe | 主程序（键盘钩子 + waveIn 录音 + 悬浮窗 + 注入） |
| asr_server.py | 识别服务（python，启动时自动拉起；端口已有服务时直接复用） |
| models/ | 识别模型（xasr onnx + silero_vad） |
| overlay.html | 悬浮窗胶囊界面 |
| hotkeys.json | 热键配置（键位/长按阈值/点按回放/退避宿主），保存后自动热加载 |
| vt.log / history.json / history/ | 运行日志（含每次判定结论与吞键/代数痕迹）/ 最近 3 条语音记录（文字+结果）/ 原始语音 wav |

## python 依赖

启动时按顺序探测解释器：本目录 `runtime\python\python.exe` →
数字助理共享运行时（`D:\123\坚祥数字助理GO\技能\_runtime\python\python.exe`、
开发目录同款）→ PATH。需要包：`sherpa-onnx numpy onnxruntime`
（数字助理的共享运行时已带）。端口 8320 上已有服务时不会再拉一个（主程序被强杀
时子进程会留下孤儿，复用比再拉一份省 500MB 内存，也避免"日志说着就绪、其实用旧进程"）。

## 编译

```
cd VoiceTypeGo
go build -ldflags "-H windowsgui -s -w" -o voicetype_go.exe .
```
（go-webview2 为本地补丁版，随目录自带）

`-H windowsgui` 是必须的：不然是控制台子系统，会多出一个黑窗口，而且那个窗口
在刚启动时可能抢到前台（我们的判定会正确地判成"自己的窗口→只复制"，用户却会以为
输入法坏了）。`-s -w` 只是去掉符号表，体积减三分之一。

注意：WebView2 初始化在命令沙箱/受限令牌下会失败（`NewWindow` 拿不到 controller
会 panic），所以**不能用助手的后台作业直接跑本程序**做联调——要么双击
`启动输入法.bat`，要么用上面的 `trigger.txt` 钩子配合已启动的实例。
判定逻辑本身与 WebView 无关，`go test` 可全量覆盖。
