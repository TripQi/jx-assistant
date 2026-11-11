# 坚祥数字助理（GO版）

完全自主编写的本地智能助手：**Go 后端（LLM 主循环 + 工具集 + 编排 + 上下文管理）+ 原生 JS 前端**。

- 后端依赖均为纯 Go（modernc.org/sqlite 无 CGo；golang.org/x/crypto 与 pkg/sftp 用于 SSH 远程工作区），前端无框架、无构建链。
- 构建产物为单个可执行文件（前端静态资源经 `go:embed` 内嵌），双击即用。
- 架构参考 pi / Reasonix / DSH 等开源 Agent 的主循环与编排思路，代码全部为新写实现。

## 运行

```sh
go build -o jx-assistant.exe .
.\jx-assistant.exe            # 首次启动自动生成 config.json 并打开浏览器
.\jx-assistant.exe -port 9000 -noopen
.\jx-assistant.exe -desktop   # 桌面模式：系统 WebView2 开原生窗口（不开浏览器）
.\jx-assistant.exe -webdir web   # 开发模式：前端从磁盘读取，改 web/ 后刷新浏览器即生效
go test -run TestXxx .             # 单测（只跑自己改动相关的，别全量——见「多 AI 并行开发」）
```

首次使用：左下角「设置」中填入 API Key；`baseURL` 兼容 OpenAI chat-completions
接口（渠道协议可选 Anthropic 原生 / OpenAI Responses，见「渠道协议」节）。
`baseURL` 填 `mock` 可进入**离线演示模式**，无需 Key 即可体验完整链路。

## 开发规矩（AI 开发前必读）

> 本项目主要由 AI 开发，节奏快，且经常有多个 AI 在同一台电脑上并行开发。
> 动手前先读完本节；功能与架构细节看下文各节与「存储与模块结构」。
> （本节由原《开发规范.md》并入——两文档曾长期重复，已合并至此。）

### 核心工作原理（最高优先级，先读这里）

> 全项目最核心的约定都在本节。后文与这里冲突时，以本节为准。

**写代码前的 7 个问题（中英对照，问完再动手）：**

1. Does this need to exist? → no: skip it (YAGNI)
   这功能需要存在吗？→ 不需要就不做
2. Already in this codebase? → reuse it, don't rewrite
   代码库里已经有了吗？→ 有就复用，不要重写
3. Stdlib does it? → use it
   标准库能做吗？→ 能就用标准库
4. Native platform feature? → use it
   平台原生能力有吗？→ 有就用
5. Installed dependency? → use it
   已装的依赖能做吗？→ 能就用
6. One line? → one line
   一行能写完吗？→ 一行就是一行
7. Only then: the minimum that works
   都不是，才动手：写能用的最小实现

英文原版与「放到本项目语境里」的解读见下一节。

**一、验证以编译与实际效果为准，自动化测试放松。** 每完成一步就 `go build`，
保证代码没有基础错误；最终修改要实际验证（跑起来看效果、做针对性验证）。
自动化测试不是主要验证手段：一般只跑自己写的、与自己改动相关的测试
（`go test -run TestXxx .`）；**只有确定近期没有其他 AI 在修改本项目时，
才可以跑全量测试**。

**二、禁止补丁套补丁。** 不写「只会用到一部分」的代码，不留死代码，不做
「第一层代码只用 50%，再套一层补丁又只用 50%」的层层叠加——每处修改要么
把逻辑写完整，要么复用已有实现；发现套娃式的补丁要回头整理，而不是继续
往上套。

**三、注释写人的核心设计意图。** 非必要的实现细节不用多写；**人**的
（设计人员的）「为什么这么做、解决什么问题」的核心意图必须写，并且
简洁明了——写的是人的设计意图，不是 AI 的。

**四、验证通过就立即提交。** 代码写得可以了（编译通过、预期效果达到）就
赶紧 commit，不攒改动——尽量减少与其他会话的改动混在一起。这里的「验证」
指编译 + 实际效果，不要求自动化测试全过；只提交自己本次的文件，一次提交
对应一件事。

### 写代码前的 7 个问题（在项目里的落法）

7 个问题的中英对照见上文「核心工作原理」，此处不重复——直接落到本项目：

- 动手新写之前，先搜一遍代码库：工具实现看 `tools.go` / `historytools.go`，
  上下文处理看 `context.go` / `contextengine.go`，扩展机制看 `components.go` /
  `adapters.go`，前端按区域看 `web/`（对话区 `app-chat.js`、左栏+顶栏
  `app-left.js`、右栏 `app-right.js`、设置 `app-settings.js`、桌面壳右键菜单
  `app-ctxmenu.js`、共享地基 `app-core.js`）——大概率已经有了，复用而不是重写。
- 优先用 Go 标准库。本项目直接依赖只有 3 个（`modernc.org/sqlite`、
  `golang.org/x/crypto`、`pkg/sftp`），全部是不可替代项；想引入新依赖，
  先自问前 3 条问不问得过去。
- 前端是原生 JS、无框架、无构建链，这是有意的架构决策（软著全自主实现 +
  单文件交付）。"引入框架/打包器让代码好写一点"在本项目里默认是错误答案。

### 运行时与依赖归属

Python 依赖按「谁用谁带走」归位，禁止混装：

1. **内置技能 / 内置组件依赖 → 共享运行时基线**（`技能/_runtime/python`，
   随仓库分发）：pdf / ocr / office / com / 微信 / 语音组件等内置能力的依赖
   （pdfplumber、rapidocr 全家、docx/pptx/openpyxl、pywin32/comtypes、
   sherpa-onnx 等）。要加包走技能管理器（技能 manifest 声明）。
2. **任务域专用依赖 → 该域自己的工具目录**：视频语音域在
   `D:\智能体工作区\工作目录\video_tools\`（视频语音任务域专用工具目录，
   ffmpeg / 模型 / venv311 自包含，faster-whisper、ctranslate2 与 GPU 的
   CUDA 库全在此）。
3. **用户技能依赖 → 跟技能自身走**（技能私有 deps），删技能连依赖一起删。
4. **禁止直接往共享运行时 pip 装东西**——它服务全部内置技能，装错难回退；
   任务域需求建独立工具目录 / venv。

### 开发流程：调研规划 → 执行 → 验证 → 提交

五步顺序不可颠倒，任何一步不许跳过：**了解项目 → 制定计划 → 执行 →
测试验证 → 提交**。

1. **了解项目**：动手前读完本节与直接相关的源码，想清楚影响面（例如改
   `agent.go` 的工具回填，会同时影响 SSE 事件流 `server.go` 与前端渲染
   `web/app-chat.js`）。不明白的地方先弄明白，禁止"边写边猜接口"。
2. **制定计划**：列出改哪些文件、按什么顺序、每步怎么验证。方案有分叉
   或影响面拿不准时，先把方案讲清楚再继续。
3. **执行**：小步前进，每完成一个可验证的小步骤就 `go build` 一次，
   保证随时可回退。
4. **测试验证**：`go build` 必须通过，最终修改要实际验证——前端改动要
   实际打开页面看一眼（布局、交互、组件/插件注入是否正常），完成标准是
   "看起来对"，不是"编译通过"。自动化测试放松：只做**针对性运行**——
   只跑自己本次改动相关的测试（`go test -run TestXxx .`），刚写的测试
   文件当然要跑；**除非确定近期没有其他 AI 在修改本项目（或用户明确
   指定），否则不跑全量测试**（`go test ./...` 禁自作主张执行）。原因：
   多 AI 并行时全量经常因别人改到一半而跑不起来，失败也不说明你的改动
   有问题；全量测试只在正式出包前执行。测试数据与辅助脚本只落本目录
   （`data/` 已 gitignore，用完即删），禁止在系统临时目录或其他位置留
   测试文件。
5. **提交**：验证通过就提交（编译通过、预期效果达到即可，不要求自动化
   测试全过）——没有 commit 的改动等同于没做完，攒着只会跟其他会话的
   改动混在一起。只提交本次任务自己的改动（明确 add 涉及的文件，禁止
   `git add -A` 一把梭）；一次提交对应一件事，提交信息一句中文说清做了
   什么；提交后推送到远程（当前工作分支：`go`）。

### 多 AI 并行开发的特别约定

- **同一台电脑上经常有多个 AI 同时开发本项目**：文件可能正在被别的会话
  修改——工作区里出现不是自己做的未提交改动是常态，别动它、更别顺手提交。
- **测试验证用已在运行的主实例，不另开演示实例**：各种改动基本是热加载的
  （`-webdir web` 下前端改完刷新页面即生效；组件/插件开关即点即生效）。
  确实需要重启就直接重启，但**尽量不要关闭实例**——可能把其他 AI 正在
  跑的工作给停掉。
- **构建/运行突然失败，先怀疑"别人改到一半"**：失败原因指向正在变动的文件
  时不要急着"修复"，等约 5 分钟再看——多半已能正常启动，或已被对方启动。
- **跑测试只跑自己的，全量靠边**：测试文件可以写、可以运行，但每个 AI
  只应运行自己刚写出来的、与本次改动相关的测试；除非确定近期没有其他
  AI 在修改本项目（或用户明确指定），不跑全量——多 AI 同时修改时全量
  经常跑不起来，失败也不说明你的改动有问题；全量测试只在正式版本出包
  前执行。

### 代码约定与常见陷阱

约定：注释、提交信息、UI 文案均为中文，错误信息要求用户可读；运行时产物
不入库（`data/`、`config.json`、打包产物等，见 `.gitignore`），唯一例外
`jx-assistant.exe`（启动程序，必须入库）。

陷阱（每条都来自真实踩坑）：

- `插件/` 目录是 DSH 插件原件，**一个字都不能改**；一切适配写在
  `插件兼容层/` 与 `adapter_*.go`。
- `config.json` 与 `data/` 是运行时产物（含 API Key 与全部会话记录），永不
  入库，也不要当源码引用。
- 前端没有构建链：`web/` 里的文件就是最终产物。
- 权限判断只在 `perm.go` 一处做，别在工具或服务器层复制权限逻辑；fail
  closed 是原则（过期/重用凭证一律拒绝）。
- 插件目录的 `node_modules` 层级极深，Windows 下会触发路径超长错误，已
  gitignore，不要加回来。
- **别把执行绑在 HTTP 请求上**：`r.Context()` 随浏览器刷新/关页取消；新增
  会阻塞的调用一律传执行体的 ctx（`sessionRun.ctx`），不要退回
  `r.Context()`。
- **`Store.SaveMeta` 刻意不写 `status` 列**：这一列只由 `UpdateStatus`
  维护；要改状态就调 `UpdateStatus`，别往 `SaveMeta` 里加回去。
- **异常中断标记（侧栏「⚠ 已中断」）存在 `meta` 表**（`laststop:<会话id>`），
  别挪进 `sessions` 的列：不进 `sessionCols` 就不可能被 `SaveMeta` 拿着陈旧对象
  回写（同 `status` 列踩过的坑），也免一次结构迁移。写入只认
  `Store.SetLastStop` 一处调用点（`runtime.go` 的终态收口 + 启动时的
  `recoverInterrupted`），值域与清除时机见该函数注释。
- **`sessions.updated_at` 只认两个时刻**：用户最后一次输入、模型最后一次最终输出
  （无 tool_calls 的 assistant 行）。侧栏/会话列表的「最近更新」排序只看这一列，
  判定收在 `StoredMessage.touchesActivity()` 一处（`store.go` 落库层与 `agent.go`
  的 `appendMessage/appendRow/appendMessageThinking` 都调它）。工具结果、带
  tool_calls 的中间步、env/prompt/summary/report 等注入行一律不刷新——它们在一轮
  任务里成串出现，会把会话反复顶到最前，几个任务并行时侧栏来回跳、点谁错谁。
  系统通知真要刷新时间，由调用方显式写 `UpdatedAt` 再 `SaveMeta`（上传/回退/交付
  /网页查看现在就是这么做的），别去改 `touchesActivity` 的口径。
- **子进程一律压掉控制台窗口**：桌面形态是 `-windowsgui` 构建（自己没有控制台可
  复用），拉起 cmd / powershell / sh / python / icacls 这类 console 程序时若不带
  `CREATE_NO_WINDOW`，Windows 会给每个子进程新开一个控制台窗口——用户桌面上就是
  「一直往外冒黑窗」。凡是 `exec.Command*` 起 console 程序，落 Start/Run/Output
  之前一律过一遍 `hideConsoleWindow`（`consolehide_windows.go`，非 Windows 是空
  实现）：run_command 的受限（`sandbox_windows.go` 的 CreateProcessAsUser）与
  不受限两条路、后台作业（`jobs.go`）、技能脚本与 python 探测、icacls、打开浏览器
  都已接上；`openHideWindow` / `gitHideWindow` 是同款历史副本，新增代码别再加第三份。
- **异常收尾必须让用户看见，但不要往对话里塞话**：真实失败走输入框上方的
  报错条（`showRunError`，带重试）并在服务端记 `runError`；对话区只放真实
  对话，别写「（本轮…）」这类系统交代——但也不能什么都不报。
- **平台适配走「文件后缀」，不建子目录、不开分支**：平台实现一律
  `<域>_<平台>.go`（windows / linux / android / other，`_other` 为必须可
  编译的兜底桩）；平台判断用 `isWindowsRuntime / isLinuxRuntime /
  isAndroidRuntime` 常量，禁止散落 `runtime.GOOS` 比较；差异文件索引与
  新增平台 SOP 见《平台适配》节。

### 能不造轮子就不造轮子

市面上有知名、成熟的工具或库，且体积不大、许可允许时，**直接用现成的**，
不要自己重复实现：协议细节、边界情况、安全问题人家都替你踩过了。例子：
二维码的生成与识别用成熟库（不要手写编码矩阵），文件下载用 aria2c
（`技能/downloader/` 内置的官方二进制），APK 解析用纯 Go 库
（`apkinfo` 子命令，androidbinary + pkcs7）。自研只保留给两种情况：
没有可靠现成方案，或现成方案的集成成本明显更高。判断口诀——
**先找轮子，再决定要不要造**；引入时连同许可文本一起入库
（GPL 等强许可尤其要附全文）。

## 平台适配（单包 + 文件后缀）

多平台支持不开分支、也不拆目录：全部代码同属一个 `package main`，平台差异由
**build tags 按文件名后缀在编译期选择**（`GOOS=android go build` 自动只挑
android/other/无后缀文件），构建脚本零改动。拆包（建 android/、linux/ 子目录）
意味着接口抽象层与全局状态重构，收益低于成本；仅当出现「可独立构建的平台
子系统」才建独立子目录（先例：`sandboxhelper/`）。

后缀与命名：

| 形式 | 含义 |
|---|---|
| 无后缀 | 全平台通用（跨平台默认逻辑） |
| `<域>_windows.go` / `_linux.go` / `_android.go` | 平台实现；写 `_linux.go` 记得排除 android（`runtime_linux.go` 的约束是 `linux && !android`） |
| `<域>_other.go` | 兜底桩：必须可编译，不支持的能力返回明确报错（参考 `fileopen_other.go` / `pickfolder_other.go`），不得静默失败 |
| `<域>_<平台>_stub.go` | 为其他平台构建保留符号（如 `sandbox_windows_stub.go`） |

平台判断统一用 `isWindowsRuntime / isLinuxRuntime / isAndroidRuntime`
（`runtime_{windows,linux,android,other}.go` 四件套）；**禁止**散落的
`runtime.GOOS` 直接比较（`fileopen_other.go` 内的 darwin/linux 细分属于
「非 Windows 世界内的细分」，可留在平台文件里）。短分叉（文案/单条件）可留在
通用文件，成块逻辑下沉平台文件。

### 差异文件索引（新增平台照此逐项过）

| 能力域 | 文件组 | 备注 |
|---|---|---|
| 运行时判定 | `runtime_{windows,linux,android,other}.go` | 三常量，新平台起点 |
| 桌面壳 | `desktop_windows.go` / `_other.go` | WebView2 壳仅 Windows，其余报错 |
| 浏览器面板 | `browser_panel_windows.go` / `_other.go` | 原生面板仅 Windows；其余回退无头宿主 |
| 密钥保护 | `dpapi_windows.go` / `_other.go` | 非 Windows 加密直通（明文落盘，如实文档） |
| Git 面板 | `gitpanel_windows.go` / `_other.go` | 差异仅「窗口隐藏」细节 |
| 打开文件 | `fileopen_windows.go` / `_other.go` | _other 内细分 darwin/linux，android 落明确报错 |
| 选择框 | `pickfolder_windows.go` / `_other.go` | 非 Windows 报「仅支持 Windows」 |
| 进程守护 | `processguard_windows.go` / `_other.go` | 顶层 kill-on-close Job 仅 Windows |
| 原子文件/路径围栏 | `fsatomic_*` / `fspath_*` | 差异为 ACL 复制与 junction 规范化 |
| 命令沙箱 | `sandbox_windows.go`(+`_stub`) / `sandbox_linux.go`(+`_stub`) + `sandbox.go` 分派 | android 分支：无 OS 沙箱、直接执行，隔离由应用沙箱承担 |
| 沙箱审计 | `sandbox_audit_windows.go` / `_other.go` | 常驻 ACE/密级标签仅 Windows，其余恒空 |
| 电脑操控 | `tool_computer_windows.go` / `_other.go` | 仅 Windows，其余注册时报错 |
| 演示记录器 | `recorder_windows.go` / `_other.go`（定义在 `recorder.go`，跨平台） | 创建技能模式专用、归用户直控（界面录制按钮 → `/api/demo-record/*` 端点，模型无录制工具）；仅 Windows 有真实现（全局钩子+截屏+waveIn 录音+VAD 分段随录随转），其余报「不支持」 |
| 进程退出信号 | `pdeathsig_android.go` / `_other.go` | android 用 PR_SET_PDEATHSIG |
| sqlite 兼容 | `sqlite_android_amd64.go` / `sqlite_android_stub.go` | 仅 android/amd64 打 seccomp 补丁 |
| 手机操控工具 | `tool_device_android.go` / `_other.go` | 仅安卓注册 device_*/adb_*（三件套，见 android/README.md）；桥端点 device_bridge.go 全平台编译（无壳轮询自然空闲） |
| ADB 主机客户端 | `adb.go`（平台中立） | 纯 Go 实现：RSA 认证 + shell,v2,raw（API 35 adbd 已移除 v1 shell:/exec:，报文字段小端、CNXN 零校验） |

通用文件中的平台分支共 4 文件 12 处（审计于 2026-09）：`sandbox.go`
72/79/81/100/119/143/203（沙箱分派与拒绝方言）、`agent.go` 217/225（Android
权限文案）、`perm.go` 398/472、`tools.go` 950——改造时逐处标注手机端语义，
能下沉到平台文件的逐步下沉。

### 新增平台接入 SOP

1. 加 `runtime_<平台>.go`（三常量）并复核 `_other.go` 的排除约束；
2. 按上表逐域过：需要实现的写 `<域>_<平台>.go`；不需要的确认 `_other.go`
   兜底报错对用户可读（参考 `pickfolder_other.go` 风格）；
3. 构建脚本加 `GOOS=<平台>` 目标（参考 `android/build-android.sh`）；
4. 编译矩阵全绿：`GOOS=windows / linux / android` 全部构建通过；
5. 真机/模拟器回归：本平台「不支持项」逐条确认报错文案。

### 手机端改造的新增文件预置（2026-09 计划）

| 新增 | 归属 |
|---|---|
| `termux_android.go` | Termux 快照装配、prefix 环境、进程组生命周期 |
| `tool_device_android.go` + `tool_device_other.go` | 无线 ADB / 截屏 / 无障碍的引擎侧工具 |
| Kotlin 桥（DeviceBridge、AccessibilityService 等） | `android/app/src/main/java/com/jianxiang/assistant/`（平台资产） |
| 快照制作脚本与选包清单 | `android/` 子目录或 `打包/`（非 Go 编译单元，可建子目录） |

## 桌面版（WebView2 壳）与安装包

- `-desktop` 参数（或安装包里的发行 exe **默认**形态，构建时
  `-ldflags "-H=windowsgui -X main.desktopDefault=1"` 注入：GUI 子系统
  双击无 CMD 黑窗，无控制台时日志改落 data\desktop.log）用系统自带的
  WebView2
  运行时开原生桌面窗口：标题「AI数字助理」（exe 文件名切换期：软件属性名/
  窗口标题已是新名，文件名一周内仍为「坚祥数字助理GO.exe」，见
  《exe名切换-AI数字助理-一周后操作清单.md》），窗口/任务栏图标取 exe
  内嵌 LOGO 资源（`打包/make_icon.py` 由 web/logo.png 生成多尺寸 ico
  （打包/app.ico），go-winres 按 `go-winres.json` 嵌入为 RT_GROUP_ICON #1；
  `-browser` 参数可强制回浏览器形态）。
  开发期间想要桌面形态：`powershell -File 打包\开发桌面版.ps1` 在仓库根
  产出 `坚祥数字助理GO.exe`（双击即窗口，数据照旧落仓库目录；切换期文件名
  跟随当期发行名）。
- 桌面模式行为：相对路径（config / data / 技能）锚到 exe 目录（开发目录
  有 go.mod 时不搬迁，避免扰动调试）；监听端口被占自动回退；命名互斥体
  单实例，二次启动唤起已有窗口；默认工作区不落在安装目录本身：改写为
  安装目录下的「默认工作区」子文件夹（模型工作区域与软件本体/配置隔离；
  目录不可写时回退「文档\坚祥数字助理」）并回写配置；旧版的「工作区」
  原地改名成「默认工作区」（装包时先做一次，启动时兜底重试，并把显式挂
  在旧路径上的历史会话一并改指新目录）；WebView2 运行时缺失时给官方下载
  指引。
- **托盘驻留与前端自愈**（2026-10-15）：桌面形态下「窗口的生死」与「进程的
  生死」解耦。用户关窗（✕/Alt+F4/任务栏右键关闭）被 `mainWndSubclassProc`
  拦截（WM_SYSCOMMAND+SC_CLOSE → `hideMainWindowToTray`）：窗口只隐藏，
  进程与后端（服务/会话/运行中任务）驻留托盘（`tray_windows.go`：图标
  常驻，单击/双击恢复窗口，右键菜单「打开主界面 / 刷新界面 / 退出」，首次
  隐藏气泡引导一次，explorer 重启自动重挂图标）。整体退出只有两条路：托盘
  菜单「退出」与自动冒烟 `JX_DESKTOP_AUTOCLOSE`（都置 `frontendQuitReq`，
  窗口循环据此收尾）。前端崩溃自愈：主 WebView2 挂 `ProcessFailed` 事件
  （`desktop_windows.go`，复用 browser_panel 的 evtHandler/vtbl 手法）——
  渲染进程死亡自动 reload（10 分钟内 3 次升级为重建前端）、无响应连续 2 次
  才刷新（防误伤长任务渲染）、浏览器主进程死亡直接重建前端窗口；窗口循环
  对意外返回一律自动重建（10 分钟内 5 次弹窗告知一次的风暴保护），后端
  永续。程序化 WM_CLOSE（热重启/托盘退出/autoclose）不被拦截，销毁语义
  不变。调试需要多实例并行时：`JX_DESKTOP_MUTEX` 环境变量覆盖单实例
  互斥体名（不带 `Local\` 前缀会自动补上）。
- **安装包**（Inno Setup，内置技能便携运行时，约 350MB 源经 LZMA2 压到
  约 103MB，装完离线全功能）：

  ```sh
  winget install JRSoftware.InnoSetup      # 一次性装打包工具
  powershell -NoProfile -ExecutionPolicy Bypass -File 打包\打包安装包.ps1
  # 产物：打包\输出\<exe基名>-安装包-<版本>.exe（当期基名默认 坚祥数字助理GO，
  # 一周后切 AI数字助理：-ExeBaseName 参数或改脚本默认值，安装包.iss 经
  # ISCC /D 自动跟随，且换名版自动清理旧名 exe/快捷方式）
  ```

  安装到当前用户 `%LOCALAPPDATA%\Programs`（免管理员），中文向导、
  桌面/开始菜单快捷方式、注册表卸载项；卸载默认保留会话数据。
  升级安装时归置默认工作区：建好 `{app}\默认工作区`，并把旧版的
  `{app}\工作区` 原地改名过来（用户文件不动；改名失败留给程序启动时
  重试，改名成功则由程序把挂过旧路径的历史会话改指新目录）。
  脚本默认现场 `go build`；要排除工作区未提交改动时，先在干净 worktree
  里构建，再以 `-ExeSource <exe路径>` 传入。打包产物（stage/、输出/）
  已 gitignore，`config.json` 与 `data/` 永不进包。

  **打包清单纪律**（`打包安装包.ps1` 强制校验，缺一拒绝出包）：
  ① 主程序 exe（注入桌面默认形态）；② `技能/`（含 `_runtime` 便携运行时，
  依赖逐 manifest 校验可导入）；③ **`组件/` 内置组件**——git 里跟踪的
  全部默认组件必须随包分发（危险命令护栏、待办定时、Git 面板、语音——本地语音
  识别的 Python worker `asr_worker.py` 与模型 `models/` 自包含在组件本体内，
  跑在共享便携运行时上；缺它时该档报「未找到语音组件 worker」，听写默认走云端不受影响），
  脚本会校验组件目录存在且至少含一个带 `manifest.json` 的组件；④
  `使用说明.md`；⑤ `md-viewer/` MD 查看器——单文件 exe 全自包含（WebView2 与
  模板已内嵌），随包只带 exe + 使用说明，装完由安装器 `/register` 静默关联 .md、
  卸载时 `/unregister` 还原（缺 exe 拒绝出包）。给打包脚本加新的暂存项时，
  同步更新本清单。

  **发布到 Gitea**（远程 `origin`＝your-org/your-repo，工作分支 `go`）：tag/标题
  沿用「Vx.y.z（日期）桌面版」（v1.0.x 序号递增），说明按上一版格式写改动
  清单，安装包 exe 挂附件；API 用个人访问令牌，存
  `C:\Users\54358\.gitea\jx-assistant.token`（网页 设置→应用 生成，权限
  write:repository），`curl -H "Authorization: token $(cat
  ~/.gitea/jx-assistant.token)"` 调 `/api/v1/repos/your-org/your-repo/releases`
  （建发布 → `releases/{id}/assets?name=…` 传附件，文件名需 URL 编码）。
  `releases/latest` 是程序内更新检查的源，发布后线上客户端自动收到横幅。
  注意 git 推送凭据（凭据管理器的 OAuth）**不能**调 API——两套体系，别再
  临时登服务器开令牌。

  **Linux 版发行与发版**（`打包/linux/`，2026-11-10 首版）：源文件入库的是
  `install.sh`（POSIX sh，dash 兼容）、`README-Linux.md`、`jx-assistant.service`、
  `打包-linux.ps1`；tar.gz 产物已 gitignore。出包在仓库根跑
  `powershell -File 打包\linux\打包-linux.ps1`（交叉编译 linux/amd64 → 平铺打包
  二进制+README+unit → 打印 sha256），产物 `jx-assistant-linux-amd64.tar.gz`
  （约 18.6 MB）。**发版时必须把该 tar.gz 作为 Release 附件一并上传**——更新源是
  jxai-server 的 `/updates/latest`（不是 Gitea：Gitea 仓库匿名不可见），附件经
  `publish-update` 登记；⚠️ 当前 jxai-server 的 publish-update 只有
  -engine/-component/-runtime/-skills/-installer 通道，**尚无通用附件参数**，
  需在 jxaiserver 补 `-extra <文件>`（平铺进 files/ 并入 assets，不参与 blocks
  快照语义）并部署后，发版命令才能带上 Linux 包。补齐前 Linux 用户用
  `install.sh --file jx-assistant-linux-amd64.tar.gz` 离线装（脚本两条路都实测过）。
  install.sh 从 `/updates/latest` 的 assets 里按文件名找 `jx-assistant-linux-amd64.tar.gz`，
  服务端通道一补即自动可用，脚本无需改。Linux 版功能边界（无桌面壳/托盘/语音
  输入法/DPAPI/静默自更新，升级=重跑 install.sh）见 `打包/linux/README-Linux.md`；
  WSL 验证记录在同目录 `验证记录-*.md`。

  **安装期清理（防旧文件残留出错）**：升级安装对「随包分发」的目录**整目录
  先删后装**（`[InstallDelete]`）：`技能\_runtime`、内置技能、内置组件、
  `md-viewer`——清理清单由 `打包安装包.ps1` 按 stage 现货自动生成
  （`打包\清理清单.iss`，构建产物已 gitignore），新增内置组件自动跟进。
  **用户的东西严格不碰**：`data\`（会话库/组件开关/市场记账/MCP/CC 插件）、
  `config.json`、`默认工作区\`（含旧名「工作区」）、`技能\user\`（用户技能）、
  `插件\`（用户装的 DSH 插件原件）、`组件\<非随包 id>`（市场装的组件）。
  历史遗留布局钉死在 iss 的静态清理段（如 v1.0.2 的 `asr\`）；**下架内置组件
  或改目录结构时，必须同步往静态段补一条**。热更路（组件块）天然合规：只
  `RemoveAll` 自己那个组件目录再整建，其余一概不碰；技能与 `_runtime` 不在
  分块体系内，只随安装包变更。

## Electron 壳（jx-desktop，迁移期新形态）

仓库 `electron-migration` 分支上的第三种桌面形态：壳是独立目录
`D:\智能体工作区\jx-desktop`（Electron 44，自带 Chromium，版本随安装包
锁定——不再被 WebView2 Evergreen 运行时回归偷袭）。Go 仓库只加了薄薄一层：

- **sidecar 模式**：`-electron` 参数＝桌面模式的环境准备（cwd 锚定/默认
  工作区迁移/禁开浏览器）但不开任何窗口，纯当本地 HTTP 服务跑，由 Electron
  主进程拉起（`-port` 随机分配）；`main.go` 的 `anchored` 汇总两形态。
  端口被占时**硬失败**（不静默回退随机端口——Electron 要感知失败换端口重拉）。
- **右栏面板桥**：`browser_panel_electron.go` 实现 browserTarget 的第三个
  形态（chromedp 宿主 / 原生 WebView2 面板 / 本桥）。Electron 主进程的
  `panel-bridge.js` 在 127.0.0.1 随机端口起带令牌 HTTP 服务驱动页面内
  `<webview id="bpw-webview" partition="persist:webview-browser">`；地址与
  令牌经 `JX_ELECTRON_BRIDGE`/`JX_ELECTRON_BRIDGE_TOKEN` 环境变量注入，
  `main.go` 发现即挂 `App.nativePanel`（缺省＝旧形态，互不影响）。工具栏
  路径不变（仍走 /api/browser/*）；导航到达由桥 POST
  `/api/browser/electron-report` → `onReport` 广播 browser_nav（与旧壳
  onNavCompleted 同一事件面）。
- **前端分支**：URL 带 `&electron=1` 时 app-core.js 给 `<html>` 加
  `electron-shell` 类并置 `window.JX_ELECTRON`；app-right.js 的 bpwReport
  矩形上报整体短路（webview 是 DOM 元素，无矩形同步）；style.css 控制
  webview 仅在该形态显示。**语音听写暂缺**（Web Speech API 在 Electron
  无免费识别通道）：按钮与快捷键提示在 electron 形态隐藏，`-browser` 与
  旧壳不受影响，等本地 ASR（SenseVoice 路线，《语音后端化》§七）落地放开。
- 已知 Electron webview 坑：**无 `src` 的 `<webview>` 不会创建 guest 进程**
  （did-attach-webview 不触发），标签必须带 `src="about:blank"`。
- 开发/打包：`打包\开发桌面版-Electron.ps1`（编引擎+npm+启动，含
  npmmirror 二进制补装）；jx-desktop\`打包-Electron.ps1` 出 NSIS 安装包
  （resources\go\ 是引擎自包含世界，与旧安装版同口径）。GUI 回归测试：
  壳加 `--remote-debugging-port=9223`，gui-test 的 CDP 脚本直接可用
  （Electron 不封调试端口），参考 electron-smoke.mjs / electron-panel.mjs。
- **旧 WebView2 壳保留不删**（回退与语音过渡载体）；Electron 版稳定 +
  语音落地后再清理 desktop_windows.go / browser_panel_windows.go。

## 自带的 MD 查看器（md-viewer/）

项目自带一个独立小应用「MD 查看器」：双击 .md 文件弹出独立窗口，用系统
WebView2 内核渲染（浏览器组件已全部内嵌进 exe，整个程序就
`MD查看器.exe` 一个文件），不依赖 Python，主线程序没在运行也能单独使用。

- 第一次使用：双击运行 `md-viewer/MD查看器.exe` 自动完成注册，之后
  双击 .md 文件选择「MD查看器」并勾选「始终使用此应用」即可；
  取消关联用 `MD查看器.exe /unregister`（或说明窗口里的「取消关联」按钮）。
- 未装 WebView2 运行时自动降级用系统浏览器打开；详见 `md-viewer/使用说明.txt`。
- **右栏也用同一套渲染**（`mdview.go`）：产品里的 md 不再显示原文，右栏打开
  的就是这份 `viewer-template.html`（`go:embed` 进 exe，灌标题/正文/副标题），
  与独立窗口同一套 marked 与样式、同一个目录侧栏。所以改模板（样式、断点、
  渲染行为）要改 `viewer-template.src.html` 再用 `node
  md-viewer/tools/assemble-template.js` 重新生成 `viewer-template.html`——
  两处（独立窗口与右栏）一次生效，别去右栏另写渲染器。

## 权限与执行模型（移植 DSH 官方沙箱机制，本栈重写）

与官方同构的完整沙箱栈——**OS 级强制隔离**，不是策略约定：

- **三档沙箱策略**（会话粒度，权限芯片选择）：
  `read-only`（沙箱内执行、禁止任何写入）/ `workspace-write`（默认；只能写
  工作区与沙箱私有临时目录，联网不限）/ `full`（不受限，官方
  danger-full-access 语义）。
- **全电脑只读档的提权申请**（`perm.go`，2026-10-13 补全）：拒绝驱动升级按目标
  给「够用的最宽档」——write/edit 在全电脑只读档被拒后，写围栏内目标可带
  `sandbox_permissions=workspace-write` 申请（「始终允许」= 会话切到工作区
  内读写，不再一跳到顶）、围栏外目标才要 `full`；另有 `request_permission`
  工具（interact.go）一次性申请切档，弹审批卡、批准立即生效并落「权限
  变更」注记——覆盖整档不可用的通道与可预判的连续写动作。全电脑只读档统一
  拦截的宿主直连通道（`requireNotReadOnly`）：git_sync、MCP 连接器
  （mcp_read_connector / mcp_call_tool / 插件 mcp__*）、register_skill、
  CC 插件 mod 写/执行——这些既不经命令沙箱也不走文件围栏，报错统一
  指路 request_permission / 权限芯片。审批等待区分「拒绝」与「超时」
  （requestApproval 第三返回值），报错分开让模型能如实转述。
- **文件工具三道防线**（`perm.go`/`fsatomic.go`，对齐官方 fs-sandbox/
  fs-observation-policy/fs-local writeFileAtomic；文件工具跑在主进程不经
  OS 沙箱，须自带边界）：① 进程内路径围栏 `writeFenceAllows`——写前
  GetFinalPathNameByHandle 原生规范化（junction/符号链接/subst 全解析；
  Go 的 EvalSymlinks 不解析 junction）+ 词法包含 + dev/ino 身份比对回退，
  可写根 = 工作区 + 系统临时目录（官方 writableRoots 同款）；② 观察策略
  ——既有文件本会话 read 过才能被 write 整体覆盖（防盲覆盖；刚创建/刚
  改过的视为已观察，会话间隔离）；③ 原子替换写——同目录临时文件 +
  rename 换指新 inode，工作区内指向外部文件的 NTFS 硬链接不被写穿
  （外部别名保持旧内容），Windows 上替换前尽力保留原文件安全描述符。
- **Windows 后端**（`sandbox_windows.go`，对齐官方 sandbox-windows-acl）：
  `CreateRestrictedToken` 造受限令牌（剥全部特权 + LUA + 写受限 Flags），
  restricting 列表 = 登录 SID + Everyone + 工作区/临时目录合成 SID（由路径
  SHA-256 派生，长期 ACE 授权工作区、一次性授权每运行私有临时目录）；子进程
  每次写都过内核两段检查。令牌默认 DACL 按官方 setTokenDefaultDaclGrant
  补授（临时 SID 优先，新建对象不携带共享工作区能力），保证管道/临时文件
  可创建。NTFS 硬链接写穿属官方自报缺口（密级关卡撤除后重新打开：受限
  进程打开工作区内硬链接名时，NTFS 懒传播会把继承授权 ACE 应用到共享
  SD，外部别名随之可写——首授后全树扫描摘 ACE 的收尾因懒传播在下次
  打开时重新应用而无效，已移除）。
- **密级关卡（已撤除）**：曾在官方之上叠加 Low 完整性级别封堵——受限
  令牌降为低完整性（S-1-16-4096）、工作区与私有临时目录打 Low 强制标签，
  内核 NO_WRITE_UP 不分路线统一拒绝「写/删密级更高的对象」。因常驻
  Low 标签污染真实工作区（把程序自身目录、数据文件一并降级，低级别
  进程链连锁出各种「权限过低」），2026-09 整体撤除，回到官方同款语义。
  由此接受官方已知盲区：删除走**父目录 FILE_DELETE_CHILD（清柜权）路线**
  时不触发写受限双检（%TEMP%/Documents/Desktop 等用户 FullControl 位置可
  被 非 -Force Remove-Item 与 cmd del 删穿，判定实验矩阵固化在
  sbx_delete_matrix_test.go，其中无清柜权位置仍四路全拒）；**Everyone
  可写位置**可写穿（ Everyone 在 restricting 列表内，pass-2 放行）。
  后续计划从工具层（命令限制/审批）补防。历史残留的 Low 标签由启动时
  `restoreStaleIntegrityLabels` 自动还原（WO 引导三步，见其注释），手动
  清理用 `sandbox acl-audit --revoke`。
- **Job Object 进程树兜底**（官方 createKillOnCloseJob 同款）：子进程挂起
  创建（CREATE_SUSPENDED）→ 分配 kill-on-close Job → 恢复线程；超时/取消
  TerminateJobObject 整树终止（含孙进程），主进程崩溃句柄关闭自动清场。
- **顶层进程树兜底**（processguard_windows.go）：主进程启动即挂入一个
  kill-on-close Job——沙箱外路径（完全访问模式、后台任务、chromedp
  浏览器、技能 python、`start` 派生的孙进程）全部继承成员身份；助手
  无论正常退出、被强杀还是崩溃，内核都会终止它启动过的一切，不留
  无人认领的后台残留。对照实验：有守护时父进程退出子进程即清，无守护
  时子进程成孤儿存活（Windows 默认语义）。正常退出路径先走**主动收口**
  （main.go → shutdownJobs，exitguard.go）：停全部后台作业并限时等死透
  （jobRun.stop 结算纪律），再记**退出自检日志**（exitguard_windows.go 查
  顶层 Job 剩余成员 PID）；被强杀/崩溃不经过收口，直接靠内核兜底。长驻
  服务的正道是 `run_in_background`（命令直接写服务本体，作业挂 kill-on-close
  Job、台账登记、进度面板可见可停）；`Start-Process` 类脱离 shell 的进程
  会随命令结束连坐清理——runUnconfinedLocal 的 ErrWaitDelay 提示会指路。
- **敏感字段静态加密**：API Key 与 SSH 密码落盘前 DPAPI 加密
  （`dpapi:v1:` 前缀，绑定当前 Windows 用户），读取时透明解密、首启自动
  迁移——沙箱读不受限语义下 `type config.json` 看到的只是密文。
- **同源校验**：HTTP 面去掉 `Access-Control-Allow-Origin: *`，Origin 与
  Host 不符一律 403——堵死本机恶意网页跨域点「允许」/切 full 绕过沙箱
  体系的 CSRF 面。
- **常驻授权审计**：工作区 ACE（及历史密级标签）是跨会话复用的常驻授权
  （官方同款设计，副作用是换工作区后留残留）。`jx-assistant.exe sandbox
  acl-audit [--revoke] [--yes]` 报告并确认后清理两代合成 SID（本产品
  S-1-80-* 与 DSH 官方 S-1-4-*）授权与 Low 标签残留。
- **Linux 后端**：额外放行 `/dev/null` sink（对齐官方 POSIX 后端），沙箱内
  `>/dev/null`、`2>/dev/null` 类重定向正常工作。
- **Linux 后端**（`sandboxhelper`，对齐官方 Landlock 方案）：非特权
  Landlock ruleset「handled」全部写类访问，workspace-write 下仅给工作区与
  私有临时目录加 path-beneath 规则；`PR_SET_NO_NEW_PRIVS` 防提权。本地
  Linux 主程序自重入施限；`build.bat` 交叉编译 amd64/arm64 helper 并内嵌。
- **SSH 远程沙箱**（对齐官方 sandbox-ssh「远端选择它安装的本地后端」）：
  首次执行时按远端架构把 helper 经 SFTP 上传到 `~/.jx-assistant/bin/`，
  命令在远端沙箱内运行；远端内核不支持 Landlock 时如实报
  `sandbox unavailable`（fail closed）。helper 带语义版本号：每次连接
  远端执行 `version` 比对，不一致自动重传。私有临时目录由外层包装创建
  并经 `trap` 清理（历史版本曾按次泄漏 /tmp/jx-sbx-* 目录，已修复）。
- **PS 5.1 编码注入（仅 PowerShell 5.1 路径）**：Windows PowerShell 5.1 的
  `$OutputEncoding` 默认 ASCII（PS→原生程序管道传中文丢字不可逆）、
  `[Console]::OutputEncoding` 默认 OEM 代码页，且 **`Get-Content` /
  `Select-String` / `Import-Csv` 读文件默认 ANSI(936)**——UTF-8 无 BOM 文件被
  解成乱码，而乱码是合法 UTF-8 字符串，输出侧解码（`decodeCmdOutput` 的
  GB18030 兜底）救不回来（2026-11-09 实际踩中）。三条命令构造点
  （`runUnconfinedLocal` / 沙箱 `buildShellCmdline` / 后台作业 `shellCmd`）
  统一前置 `ps51Prologue(mode)`：UTF-8 模式设输出 UTF-8 + 三个
  `$PSDefaultParameterValues[...]='utf8'`；**GBK 模式**（`sandboxRun.EncodingMode`）
  设 `GetEncoding(936)` 输出 + 三个 `'Default'` 读面，输出成 GBK 字节正好被
  `decodeCmdOutput` 接住。写面（`Out-File`/`Set-Content`）**故意不设默认值**：
  5.1 的 `-Encoding utf8` 会写 BOM，落到 `.bat/.cmd/.ps1` 上就是命令失败。
  并注入 `PYTHONUTF8=1`、`PYTHONIOENCODING=utf-8`（`appendEnvUnique` 先摘后加，
  环境块重复变量"先见为准"）。
  读面编码怎么定（`encoding.go`）：`detectEncodingMode` 从命令串里抓字面出现的
  文件路径（`extractPathCandidates`）→ 读 64KB 头探测（BOM→严格 UTF-8→GBK/
  GB18030 结构→UTF-16 启发→二进制）→ 全是 GB 族就整条切 GBK，混合按 UTF-8 并
  给一句提示。**探测不到的（程序内部自己读文件）靠输出乱码检测兜底**：
  `looksMojibake` 命中且 `isReadOnlyCommand` 且本地前台 → 换另一种编码
  **重跑一次**，两次都脏如实报告；写/安装/提交/启服务/后台作业**绝不重跑**
  （重跑=执行两遍），只附一句诊断。
  **cmd/pwsh/sh 分支与 Linux/安卓/SSH 远程零改动**
  （pwsh 6+ 全平台默认 UTF-8；Linux 文件名是字节流、grep/sed 字节匹配，
  locale 不影响中文；安卓 Termux 环境已有 LANG=en_US.UTF-8）。
- **write/edit/read 的编码语义**：`read` 探测解码后统一返回 UTF-8 文本（GBK/
  UTF-16 不再显示成替换符），文末注记原编码；`write` 永远 UTF-8 无 BOM（覆盖
  既有 GBK 文件会提示编码变化，想保留用 `edit`）；`edit` 按原编码解码匹配、
  **按原编码写回**（GBK 仍 GBK、BOM 保留），原编码编不出的字符（emoji 写 GBK）
  **直接报错、文件不动**，绝不静默替换成 `?`。入参永远是 Unicode 文本（JSON
  字符串），因此只存在一次转换（Unicode↔原编码），不会出现"GBK 当 UTF-8 再
  转 GBK"的叠加。
- **write/edit 回读注记**：`snapshotWrite` 把写后回读（verified=真的从盘上/
  SFTP 读回来）交给工具成功文案拼注记（`readBackNote`）——write 带
  "回读一致，首行：<截 60 字符>"，edit 只报一致/不一致，不一致/多出 BOM
  给 ⚠。防御的目的不是不出错，是出错时模型第一轮就看见（远程 SFTP 写入、
  外部程序干预、编码转换），而不是执行到第三步才发现。
- **拒绝驱动的升级**（对齐官方 bash 工具）：不做威胁命令猜测、不做执行前
  预检——命令先按会话策略进沙箱真实执行，被拒结果带
  `[sandbox: … denied under <mode> mode]` 标记；模型同轮带
  `sandbox_permissions`（够用的最宽档）+ 一句 `justification` 重试同一命令
  一次，弹**审批卡**由用户决定；拒绝后该命令本轮终局。
- **能力开关 = 常设授权**（权限面板右侧多选）：审批卡点「始终允许」时，命令
  升级点亮对应能力开关（如 `允许工作区外执行命令`），工作区外写入则把会话
  权限**提升为完全访问**。**`允许工作区外执行命令` 的语义（2026-10-19 起）=
  关闭命令沙箱**：勾选的会话里命令（run_command / run_python / 后台作业）
  默认不经沙箱、直接执行（`perm.go` exec 分支按能力把 policy 改写为 full），
  不再是旧的「升级时免审批」——它是修改工作区与完全访问之间的中间档：
  命令通道全开（可写任意位置），但 browser_* / computer_* / device_* 仍按
  各自能力开关把守、不捆绑点亮，所以不叫完全访问；危险命令护栏不受影响。
  切到完全访问会预设捆绑点亮
  `允许工作区外执行命令`、`操作浏览器`、`操作电脑` 三项能力（前端勾选联动
  置灰、不可单独关闭，权限引擎同样视为开启；切回其他档位时自动取消勾选，但
  **默认能力会补回来**——「修改工作区」默认开启 `操作浏览器`（`perm.go`
  `capDefaultsForMode`：新建会话、`dispatch_sub` / `dispatch_task` 派发、分支
  复制、以及从完全访问切回时一律补上；用户在权限芯片里显式取消后不再覆盖）。
  **能力开关与命令沙箱是两套门**：`browser_*` / `computer_*` / `device_*` 由
  助手进程直接驱动本机浏览器/桌面/手机，**不经命令沙箱**、也不需要
  `sandbox_permissions` 升级；反过来，沙箱内的命令启动不了浏览器（受限令牌下
  access denied），要看网页只能用 `browser_*`（前提是该会话已开启能力）。
  2026-10-19 起，非完全访问档下启动浏览器的命令（msedge/chrome/firefox 等，
  `perm.go` `browserLaunchHit`）在 `authorize` exec 分支**执行前预拦**：不跑、
  直接返回带 `[sandbox: browser launch blocked …]` 标记的引导错误（指路
  browser_*）——这不是安全预检（与危险命令护栏语义不同），是「确定性失败
  路径的快速失败」：沙箱内浏览器启动多为**静默秒退无输出**（实测 msedge
  --version / Start-Process msedge 均空输出返回），让命令白跑只会换来模型
  对着空输出反复重试打扰用户。full 档（用户知情批准的沙箱外执行）不拦。
  这条已写进环境快照的「能力」段（`agent.go` `envPrompt`：开关状态逐项列出，关着时
  说明后果与正确出路）。
  **能力门控有自己的升级参数**（`perm.go` `requireCapability`）：能力关着被拦后，
  模型同轮带 `request_capability=true` + `justification` 重试同一工具即弹审批卡——
  「允许」=本次放行（不点亮开关），「始终允许」=点亮对应能力开关（常设授权）。
  与 `sandbox_permissions` 同一套拒绝驱动语义；每个门控工具的描述都带权限提醒、
  schema 都带这对参数（`tools.go` `capRequestProps`）。
  点亮后该类动作不再逐次审批。
- **一次性授权凭证**：每次写文件/执行命令先签发 5 分钟时效的一次性凭证，
  执行器核销后才动手；过期/重用即拒绝（fail closed）。
- **受保护路径**：助手自身配置与数据目录（API Key、全部会话记录）任何模式
  禁止访问——确定性的路径边界，非命令内容猜测。
- `web_fetch` 在非完全访问模式拒绝内网/本机地址（防 SSRF）。
- **对抗验证**：sbx_adversarial_test.go（穿越/Junction/删外部/注册表/子进程
  继承/网络/硬链接双顺序九项，缺口项均为硬失败断言）+ sbx_delete_matrix_test.go
  （16 格删除矩阵）+ sbx_confinement_test.go。已知边界如实记录：读取、网络、
  进程可见性不限（官方同款语义）；多链接（硬链接）文件在沙箱内只读（共享
  SD 还原后外部别名与工作区内名都不可写，pnpm 等写普通单链接文件不受影响）。

## SSH 远程工作区（提前实现 DSH 的远程环境能力）

DSH 官方的远程工作区只在 mac/Linux 宿主上可用；本产品用纯 Go SSH 协议栈
（`remote.go`）实现了同等模型，**Windows 宿主同样可用**——把一套运行环境搬到
远程：

- 会话挂上远程工作区后：**文件工具走 SFTP、命令经 SSH 远程 shell 在远程目录执行**，
  权限引擎照常把关（工作区边界 = 远程目录，POSIX 路径规则）。
- 入口：加号菜单「SSH 远程工作区」→ 填主机/端口/用户/认证（私钥文件或密码）/
  远程目录 → 「测试连接」（拨号 + 命令通道 + SFTP + 目录检查，回显主机指纹）。
  两步式：第一步连接配置「保存」，第二步浏览远程目录「使用」（也支持直接输路径）。
- **登记与复挂**（remotelist.go）：「使用」成功后自动登记该远程工作区（密码 DPAPI
  加密落 `data/remote_workspaces.json`，接口不回密码）；工作目录选择面板出现
  「远程工作区」分组条目，点选即挂载（前端不带密码，建会话/测试/浏览/换挂载时
  服务端按 主机+端口+用户 回填凭据 `fillRemoteCreds`），条目悬停出 ✕ 可忘记。
- 主机密钥 OpenSSH 式 TOFU：首连记录指纹到 `data/ssh_known_hosts`，之后变更即
  拒连（防中间人）；会话内绑定指纹，换主机需重新测试保存。
- 认证支持无口令私钥文件与密码；连接按会话缓存 + keepalive，断线自动重拨。
- 数字助理派发的子任务自动继承主对话的远程工作区。
- **二号工作区＝派发远程**（"本地主对话 + 远程子对话"的混合形态）：dispatch_sub /
  dispatch_task 的 `remote_workspace` 参数（取值＝登记库 display，`resolveDispatchRemote`
  返回副本、凭据不出服务端）让子对话/工作任务挂到指定远程工作区——与 `workspace`
  互斥，缺省沿用继承。登记库非空时环境快照出现「可派发远程工作区」公告行
  （`dispatchRemoteLine`，追加式差分、缓存友好；aux 模式不给）。设计取舍：ZCode 与
  DSH 均为"一个会话一个工作区"（ZCode 多工作区＝多 Tab 各自会话；DSH workspace 对
  模型不可见），同会话双工作区（文件工具按参数选边）无参考实现且要动权限围栏/
  快照侧别/沙箱路由六层，留作二期；子代理路线复用已验证的纯远程会话链路，前缀
  缓存除工具 schema 升级一次性断外不受影响。
- **弹窗开合的两个坑（已修，别再犯）**：`openPopover` 是切换语义（已开着就收起），
  内容高度变化后重对齐位置要用 `reposPopover`（第一步→第二步曾误用 openPopover
  导致弹窗自关、远程工作区「怎么都建不起来」）；文档级「点外收起」的锚点豁免
  名单要含 `#remote-badge`（顶栏远程徽标也是弹层锚点）。

## 工作区模型

工作区即文件夹（或 SSH 远程目录）。每个会话挂载在一个工作区下（未单独设置时
挂默认工作区），**会话能写哪个目录由它挂载的工作区决定**：文件写入与 shell
执行都被限制在该目录内（权限引擎强制，见上节）。侧边栏按工作区分组展示顶层
会话；子会话/辅助对话不占侧栏，从对话内右栏进入。

## 界面（对齐 DSH 风格）

- **首页（新建对话）**：居中问候区 + 输入卡片；输入框上方两枚胶囊——**工作模式**
  在前（左，标准办公 / 编程 / 创建技能，决定该会话的系统提示词与交付倾向，
  `CHAT_MODES`；选「创建技能」时联动把权限默认提到完全访问——该模式要看懂
  用户演示并亲自验证技能步骤，见 `applyDraftMode`；切回别的模式且权限是联动
  自动设的，则还原为修改工作区），
  **工作目录**在后（右缘对齐输入卡；候选 = 默认目录 + 历史用过的目录，也支持输入
  任意绝对路径；目录只在新建时选）。手机端（窄屏 <500px）没有工作目录胶囊、
  加号菜单也没有 SSH 远程挂载项——工作区由引擎锚定在应用私有目录
  （`JX_WORKSPACE_DEFAULT`），目录/远程挂载到电脑端操作（CSS 媒体查询 +
  JS `isNarrow` 守卫双保险，见 style.css 窄屏节）。
  **左上角「新工作任务」不立即建会话**：点了回到首页并聚焦输入框（`goHomeNewTask`），
  发出第一条消息才真正创建（懒创建）——避免产生一堆空会话。（曾有「点一下就出会话」
  的实现，因空会话堆积已移除，见 git 历史 081c2273。）
- **输入区工具栏**：左下角加号菜单（文件上传 / 计划模式 / 目标模式 /
  导出日志（运行时间线 Markdown）/ 导出原始数据（最近发给模型的请求）/
  SSH 远程工作区）与权限芯片；
  发送按钮左侧是**模型芯片**（既是显示也是选择，按提供方分组）。
  **模式前端定制点**：基础前端一套不变，工作模式可对界面做定向增补——
  `CHAT_MODES` 自己那一项挂 `ui` 对象（`mount(view)` 首挂控件并返回根元素、
  常驻 DOM 只切显隐；`sync(view, s, el)` 随 `syncComposer` 刷状态，在基础
  刷新之后跑、可覆盖基础设置），`applyModeUI(view)` 是唯一分发点，未挂 `ui`
  的模式零影响；模式对主前端的改动代码全部收在模式那一项里。首个消费者：
  编程模式在权限芯片右侧挂「计划」胶囊 + 小号苹果开关（`.sw-mini`，点按切换
  计划模式，并接管「计划✕」胶囊的显示——开关已表达状态，胶囊不再重复出现）。
- **手机端设置页的两级返回**（`app-settings.js` 的「设置导航」节）：窄屏下
  设置＝一级菜单＋二级分区页，返回头/手势逐级退（二级→一级→关设置）。历史
  栈契约：返回键**UI 直切优先**（`setNav.level` 改完 `renderNav`，不依赖
  popstate），`history` 只做栈同步且**只退实际压入的层数**（`setHistPushed`
  与 `setHistDepth` 分开记账），`pushState` 一律走 `setHistPush` 的
  try/catch——宿主环境（远程外壳一类）不支持历史 API 时降级为纯 UI 导航，
  绝不让 `back()`/`go()` 越过自己压的栈把用户带出应用。左栏返回守卫的
  `backGuardArm` 同样容错（压哨兵失败＝守卫不生效，不向上抛）。
- **窄屏返回层级只有一套**（2026-10-22 收敛）：`web/app-left.js` 的 `jxNavBack`
  是唯一入口——设置页 → 3 秒内连按两次退出 → 弹出菜单 → 右栏 → 左栏 → 对话页
  呼出左栏。系统返回键（手机壳注入调用）、浏览器后退（popstate 哨兵）、左右滑
  手势（`jxPanelSet` 同一批开合原语）全走它，网页与 App 不再各写一套。手机壳只
  保留网页做不到的三件事：退系统全屏视频、网页回 `'exit'` 时 `finish()` 结束本页
  （3 秒双击窗口在网页里，壳里不再计时），以及**在壳内另开一层浏览器页**
  （`jx-link` 的 `BrowserActivity`：右栏浏览器/对话链接的页面都开在那里，返回键与
  右缘左滑由那一层自己收掉回到对话页）。后者是因为壳的 WebView 没有接管新窗口，
  网页侧 `window.open(…,'_blank')` 只能被丢掉或把电脑页 UI 顶掉、返回键再也出不来；
  那一层**不调** `jxNavBack`，网页的返回层级与它无关（前端出口：`app-chat.js` 的
  `jxOpenPage` / `jxOpenContent`，经壳注入的 `window.JXLink` 桥）。
- **附件区**（输入框正上方一条轨道，主对话与右栏子对话同款，渲染与采集见
  `app-chat.js` 的 `renderAttachments` / `stageFiles` / `flushAttachments`）：
  选择、粘贴（截图）、拖拽进来的文件先在这里**暂存**——图片给缩略图，其余给
  「图标 + 文件名 + 大小 + ✕」卡片，横向排卡、滚轮横向平移（对齐 DSH
  `ui-attachment` 的 rail/卡片）。**点发送才写进会话工作区**（远程经 SFTP），
  上传接口返回【文件上传】注记文本，由发送链路拼进**用户消息开头**的
  「用户附件：」块（ATTACH_HEAD，与用户引用块同款；`dispatch_task` 派发附件
  拼进任务消息开头，同形态）——注记随用户消息一起到达模型，插话/排队不再
  出现注记先到、消息后到；**部分上传失败时全部附件留在附件区**（成功项不清走：
  独立注记行已不存在，清走会丢注记；重试全量重传，判重幂等自愈）。
  形态是**输入卡内部第一层**（挂在 `.composer-card` 里、引用胶囊之上）：有附件时
  白卡自身向上长高「展开」，与下方输入区同底色同圆角连为一体、底部一条细分隔线
  ——不做浮在卡外的「第二层条带」（与卡断开、间距里漏页面底色，难看，已废弃）。
  几条边界：没附件时整块 `display:none`（CSS 层另有 `.attach-rail:empty` 兜底）——
  不占高度、不留白边、也没有空状态提示；
  落盘失败即中止本轮发送（失败卡片标红留在轨道上、输入内容也留着，不会出现
  「文件没带进去、消息却发出去了」；只挂附件不写字不给发（不替用户编消息）；
  换会话/新建对话/刷新即丢（暂存只在内存里）；注记不再独立落库（上传响应带回，
  失败中止时不丢——附件全量留区重试）。载荷沿用原有两条路：桌面壳的本地
  会话走原生选择框采本机真实路径（该路径可作 `deliver_result` 的 upload 交付目标），浏览器 /
  手机 / 远程 / 粘贴走 multipart（单文件 32MB 上限、一次最多 20 个）。
  **拖拽另有原件路径桥**（`dropbridge_windows.go` + `app-chat.js` 的
  `requestNativeDropPaths` / `JX.onNativeDrop`）：WebView2（Chromium）不给页面
  文件绝对路径——实测拖入后 `File.path` 不存在、`text/uri-list` 与 `text/plain`
  皆空、`webkitGetAsEntry().fullPath` 只有文件名，所以拖进来的文件过去只能按
  multipart 送内容、原件路径无人登记，交付工具自然送不回原处。宿主拿得到：页面
  把 File 对象经 `chrome.webview.postMessageWithAdditionalObjects(msg, files)`
  发过来，宿主从 `WebMessageReceived` 的 `AdditionalObjects` 里读
  `ICoreWebView2File.Path`（SDK 原文：obtain the path of a File dropped on
  WebView2），再回调 `JX.onNativeDrop({batch,paths})` 把本批附件就地升级成
  「路径型」（发送时走既有 JSON `{path}` 通路登记原件路径）。拖拽观感、悬停
  高亮、页内 HTML5 拖拽（排队坞排序等）全不受影响；宿主侧桥挂不上（老运行时
  没有 `AdditionalObjects` / 非 Windows）只落日志，拖拽退回 multipart，行为
  与从前一致。交付侧对应实现在 `deliver.go`（与门语义）：要搬运必须 `upload`
  （`findUpload` 认 uid / 原件路径 / 工作区副本路径）与 `filename` 成对显式
  提供，缺一在调用时立即报错；两者都不给＝只登记交付卡片不搬运（无自动反查）。
  引用解析不到（浏览器、手机、粘贴上传登记不了原件路径，或编号写错）报可读
  错误并列出可用编号，绝不猜一个目录去覆盖。
- **输入框标记胶囊**（`app-chat.js` 的「输入框标记胶囊」节，HTML/CSS 在
  `index.html` 的 `composer-input-wrap` 与 `style.css`）：引用标记（“引用N”）与
  附件标记（“报告fina….docx”，主名前 6 字 + … + 扩展名，不带编号——用户自己传的
  文件认得名字）在输入框里仍是纯文本，但由**背衬层**画成灰底胶囊——与输入框同排版
  的透明层垫在底下（字体/行高/内边距/换行规则从输入框复制，同语音灰字层手法），
  标记段垫灰底圆角色块，黑字仍是输入框自己的、正常编辑，两层逐字重合。输入框侧
  附件标记按暂存附件的 `a.mark` 认领**区间**（`inputMarkRanges`：一份附件认领一处、
  按出现顺序一对一分配，所以同名文件的两段相同文字各自成胶囊、背衬层与输入框永远
  等长；不猜格式，草稿恢复后没有对应附件就自然退回普通文字）；气泡侧
  `appendQuoteBodyText` 按「以 .扩展名（字母）
  结尾的引号串」启发式渲染灰胶囊（只看消息文本，纯视觉层）。防误改：光标落进
  标记内部弹到边界；退格贴标记末尾 / Delete 贴标记开头整颗删除（execCommand 保
  撤销栈）；选区整颗落在标记内扩成整颗；IME 组字期间不干预。程序化写值经
  textarea 实例上的 `value` 存取器拦截统一刷新背衬层。附件入列
  （`stageFiles`/`stagePaths`）即在光标处插入标记（`a.mark` 存完整标记），卡片 ✕
  联动摘掉；**发送时文件标记原样留在正文里**，全名与位置由消息开头的「用户附件：」
  块（注记逐字原样）带给模型；气泡里附件块渲染成灰色附件行，正文文件标记按
  `fileClipName(块内《全名》)` 对号成胶囊（对不上按普通文字，不硬猜）。
- **上传落位与判重**（`uploadpath.go`，路径型与 multipart 同一套规矩，`handleUpload`
  复用）：用户上传的文件统一存 `<工作区>/.jxai/uploads/`，不占工作区根目录；
  **上传永不覆盖任何现有文件**——重传内容一致的文件当上传成功处理、不重复落盘
  （判重＝大小 + FNV-1a 全内容哈希，现场比对 .jxai/uploads 里的**全部**文件、
  指纹不落库，所以「传 A → 传改过的 A（存成 A-2）→ 再传与第二次相同的 A」第三下
  能认出与 A-2 一致，模型改过工作区副本后重传原件也会如实按不同文件顺延，不静默
  丢数据）；同名不同内容顺延 `名字-2/-3`（序号插在扩展名前）存新文件。multipart
  上传也登记 uploads 表（orig_path 留空——交付工具只认非空原件路径的记录）。
  `.jxai/uploads` 对目录检索（list_dir/glob/grep）开放，snapshots/tmp/delivery-backups
  三个系统子目录仍隐藏（`tools.go` 的 `jxaiHiddenRel`）；对 uploads 里文件的
  write/edit 照常产生文件改动检查点（`snapshot.go` 的跳过规则只盖三个系统子目录）。
- **桌面壳右键菜单**（`web/app-ctxmenu.js`，仅 `desktop-shell` 形态注册；网页/
  手机模式保留浏览器原生右键）：go-webview2 库在 `Debug=false` 时把 WebView2
  默认右键菜单与 DevTools 一并禁了，桌面壳右键原本什么都不弹——这里补自定义
  菜单，弹层复用左栏 `openCtxMenu`（条目扩展了 `{sep}` 分隔线与 `{disabled}`
  置灰）。按右键目标组装：输入控件给 撤销/重做/剪切/复制/粘贴/全选（编辑动作
  对焦后走 `document.execCommand`，保留原生撤销栈）；对话区选区给
  复制/引用到输入框/问AI；消息气泡给 复制全文；网页链接给
  打开/在系统浏览器中打开/复制链接；文件路径链接与
  交付卡行给 预览/在系统中打开/在文件夹中定位/复制路径；工具调用截图给 在右栏
  查看/复制图片。空白处不弹。**每一类都再带一条「朗读」**（见下条）。
  **粘贴的剪贴板读取有平台分叉**：桌面壳里
  `navigator.clipboard.readText` 会永久挂起（WebView2 的剪贴板读权限停在
  prompt 且没有授权 UI，go-webview2 的 SetPermission 对它不生效——真机诊断
  见 `gui-test/ctxmenu-clipboard-diag.mjs`），所以桌面壳直接读本机端点
  `GET /api/clipboard`（`clipboard_windows.go` 读系统剪贴板：文本 +
  截图常带的 PNG 注册格式，文本优先、PNG 入附件与 Ctrl+V 同口径；
  剪贴板属敏感数据，loopbackOnly 仅回环可达）；网页模式 readText 行为正常
  走标准 API。配套端点：`POST /api/open-url`（仅 http/https）、
  `POST /api/sessions/{id}/open-path|reveal-path` 与 delivery 的 open·reveal、
  `/api/browser/open-view`（预览）——全是**用户手势链路：一律直通**，无工作区
  校验、无确认弹窗（这些路由不在 call_api 目录里，AI 调不到；对用户自己的
  点击做拦截违背设计意图），仅保留「路径不存在」这类自然错误，用户主动操作、
  不经模型权限门控（同交付卡片口径）。
- **右键菜单一律带「朗读」**（组件桥，应用本体不发音）：`app-core.js` 的
  `ctxSpeakItems(ctx)` 只负责把「这个目标念什么」交给组件
  （`JX.ctxMenuItems` ← `组件/voice/client.js` 的 `registerCtxMenu`），取值
  口径写在同文件「右键菜单的朗读取值」一节：**选区优先，其次被右键那条的
  可见文字**（`speechSelectionIn` / `speechTextOf`，跳过代码块/脚本/按钮；
  文件类用 `speechPathText` = 文件名 + 全路径）。已接 10 类目标：输入框
  （`edit`）、对话区选区（`selection`）、消息气泡（`bubble`）、网页链接
  （`link`）、文件路径链接（`file`）、交付卡行（`delivery`）、工具调用截图
  （`image`）、左栏会话项（`session`）、工作区头（`workspace`，正常视图与
  回收站两处）、右栏子对话项（`sub`）。**组件关掉即整条消失**（连分隔线一起
  省），句子剥壳后为空（如只选中一段代码）也不出现——不出现好过点了没反应；
  朗读中所有菜单该项都变「停止朗读」（单飞，与消息操作行按钮同一条链路）。
  桩测 `gui-test/ctxmenu-speech-test.js`（纯 node，不用起服务）。**右栏浏览器
  面板内**的右键是 WebView2 原生菜单，不归这套（要 `ICoreWebView2_11` 的
  `add_ContextMenuRequested`，绑定库未导出，未做）。
- **权限面板**（点权限芯片弹出，两列）：左列权限单选（全电脑只读 / 修改工作区 /
  完全访问），右列能力多选（允许工作区外执行命令 / 操作浏览器 / 操作电脑）。
  切到完全访问时三项能力联动勾选并置灰（不可单独关闭）。新建会话默认权限是
  修改工作区，因此**新建对话默认就勾着「操作浏览器」**（显式取消后不被覆盖；
  从完全访问切回修改工作区时会自动补回）。**例外：长期助理主对话（新装机
  首启自动创建的 `mode=assistant` 会话）默认完全访问**——派发「只能同等或降权」，
  总控自己站最高档，派出去的任务才有完整档位空间（任务默认仍是修改工作区；
  见 `createSession` 的权限兜底）。已建主对话的权限存在会话库里，不追改。
  **改档后胶囊即时跟随**：权限芯片与能力菜单都读各自视图的 `state.cur`
  （permission/tools），改档完成（本视图 PATCH 成功、或收到 `session_updated`
  广播/环境快照行）即经 `applySessionPermToViews` 写回并重画——主对话区与右栏
  子对话两处同时跟上，不依赖流式状态、也不等整包重取（原实现只在 `state.streaming`
  为真时同步、且只同步主对话区，右栏胶囊与空闲会话都要切走再切回才变）。
- **待决定的浮窗**（审批 / 助手提问 / 计划审批，渲染见 `app-chat.js` 的
  「待用户决定的浮窗」一节）：**按「这张卡属于谁」分两处落**，各司其职：
  1) **本对话的卡 → 输入框上方**（主对话 `#ask-bar` / 右栏子对话 `#sub-ask-bar`）：
     位置在附件区之上、输入卡之上，宽度与输入卡一致（吃 `--content-w`），就地处理
     最顺手；手机端只要还有待办，输入区滚动收起就不生效（否则卡跟着滑出屏幕）。
  2) **别的对话的卡 → 右下角角标堆**（`#ask-dock`）：落在**对话区右侧那段留白**里
     （锚在 `#main` 右缘内 12px，宽度取 `min(300, 留白−12)`），绝不压正文列、也不跟
     右栏抢空间。卡片顶部多一行「来自「某对话」」——点一下切到那个对话，不点也能就地
     审批/回答。生灭由留白决定（`dockMetrics`，与进度面板同一套 auto 模型，右栏开合 /
     拖把手 / 窗口缩放都会重算，防抖 150ms 等过渡落定）：
     · 留白 ≥240px 且纵向可用高 ≥220px → 自动展开（高度上限 560px，再多在堆内滚动）；
     · 内容本身塞不下当前可用高 → 也自动缩成胶囊（用户手动点开过就尊重他，展开态滚动）；
     · 留白不够展开 → 只留胶囊 `#ask-dock-pill`（只报条数，缩着时来了新的会脉冲一下）；
     · 连胶囊都放不下（右栏占光、留白 <96px）→ 整块不显示，靠左侧栏橙点标出谁在等。
     顶部「—」按钮可显式把整块缩成胶囊；窄屏退回窗口右下角、按视口宽度收窄（左右栏
     整屏浮层开着时不铺开）。
  另外两条（两处共用）：
  · **重绘不动用户的字**：轮询/事件重画按「卡片签名」比对——签名没变一个节点
    都不碰，只有内容真的变了才重建那一张卡，自由输入框的文字按请求 id 存草稿
    并回填（`askDraftGet/Set`）。十几秒一次的兜底轮询不再清空已输入的内容
    （旧实现每次把容器 `innerHTML` 清空重画，用户打的字被冲掉）。
  · **计划审批不在这里铺长文，整张卡可点**：`kind=plan` 的审批卡只给标题 + 首段，
    点卡片任何位置（输入框与三个按钮之外）→ 打开右栏「计划」视图看全文（Markdown 渲染）；
    三个动作是「修改（带反馈）/ 忽略计划 / 批准并开始执行」。
    **时间限制不一样**：普通提问与「进入计划模式」确认等 5 分钟（超时按「用户未回应」
    返回），**计划审批没有时限**——提交那一刻就落库，用户不处置它就一直在：哪怕关掉软件、
    重启进程之后再打开，那张卡仍挂在对话里等审批。唯一主动作废的入口是**停止**：等待
    审批的这轮被用户点「停止」（或数字助理代停）时，待审批计划随本轮一并作废（拒绝/
    撤销语义，浮窗收起、不再挂回）——停止就是明确说「这轮不要了」。
    「忽略计划」＝搁置：本轮对话就此停下，计划本身不撤销（与点「停止」的差别就在
    这里——停止会把挂着的待审批一并作废），之后随时能再批准或修改。
  · **对话流里另有一条常显的计划卡**（条目类型 `plancard`，跟着提交它的那一轮、不折进
    过程段）：标题 + 首段 + 一行「点击这张卡看计划全文」，整张卡可点 → 打开右栏并**定位到
    这一份**（按提交序号）。审批卡批了 / 驳了 / 离场之后，它就是「再点开看这份计划」的
    常住入口——所以每一份提交过的计划在对话里都留得下痕迹。
  · **计划审批的正文与来历**：`GET /api/sessions/{id}/plans` 读回历史里的每一次
    `exit_plan_mode` 提交（见 plans.go）；待审批的那一份另有落库副本（`store.go` 的
    `PendingPlan`，存 meta 表 `sessdata:pendingplan:<会话id>`），所以重启后
    `/api/questions` 仍能把它挂回来（会话被删、或已退出计划模式时自动作废清理）。
  兜底轮询只在事件流断连时才真的去拉（`app.js`）。事件经全局事件流实时推送；
  远程工作区会话顶栏显示「远程 user@host:dir」徽标。
- 文件上传：先暂存进输入框上方的**附件区**，点发送时才写进会话工作区（远程会话
  经 SFTP）并注入记录消息（见上一条）。**两种导出**
  统一向上下文引擎申请（引擎是记录者：它收到什么都落日志，发给模型的请求
  由它环形缓存），都取自 messages 表原始历史（append-only，与发给模型的
  压缩视图无关）：
  **导出日志**＝运行时间线——从会话创建起按时间排列：上下文组装（系统
  提示词 / 环境快照 / 工具定义全文，后续轮次自动对比并标出系统提示词与
  工具集的变更及差异行）、环境快照更新（行级 diff）、用户消息、每次模型
  调用的发送规模 / **前缀指纹**（系统提示词与环境快照的短 hash，指纹变化
  即缓存必不命中）/ 返回 / 用量 / **缓存命中率** / 耗时、模型逐步输出与
  工具结果、以及**不发给模型但影响判断的事件**（运行中报错——含网关错误
  响应摘要、用户停止、上下文超限重试、规则压缩与摘要生成、上下文事件——
  权限/模型/思考档位/模式开关/工作区/远程挂载变更，它们正是缓存命中率
  突降的常见原因；文首附调用一览表便于对照）；**导出原始数据**＝引擎缓存
  的**最近三轮**真正发给模型的完整请求体（发送前捕获、与线上逐字节同源，
  不含 API Key）+ 相邻两两对比（消息条数 / token 估算变化、工具目录与
  sys/env 指纹是否变化、**首条变化消息的位置**——它之前的前缀逐字相同、
  理论上仍命中缓存），用于核对「到底给模型发了什么、缓存为何变化」。
  控制台输出按 UTF-8/GB18030 解码入库，导出恒为合法 UTF-8。
- **对话区展示铁律「所见即所发」**：模型能收到的上下文，前端都要能显示出来
  （展开折叠后与发给模型的逐字一致，只允许折叠行数/按钮/排版差异），顺序也与
  模型收到的一致；绝不出现「模型收到了、用户看不到」。审计工件
  （request/audit）不发模型，不在对话区展示（有导出出口）。
  压缩不破例：折叠行原文在前端照常完整展示（前端 ≥ 模型所见），并在连续折叠
  段末尾的**真实压缩位置**常显「已折叠 · 此前 N 个回合…」标注行（groupTurns
  按 compact 标记算边界）；运行中改权限等 PATCH 落的环境快照行要同时进全局
  广播**和本会话执行流**，流式视图实时翻新，不能等下一轮才可见。
- **对话区的提示词材料按「变化」展示**：首轮在真实拼装顺序上完整交代——
  「工具定义」（请求体**顶层字段**，与 `messages` 平级、不属于消息序列；模型侧
  KV 前缀以它打头，所以排在最上）与「系统提示词已拼装」（`messages[0]`）在用户
  消息**上方**，「时间提示」紧跟用户消息（见下条），「运行环境」（追加式快照，
  作为 user 消息进消息序列：有变化才追加、追加在当轮用户消息之后，首轮它就是
  那次请求的最后一条消息）在用户消息下方，其后才是模型输出；展示顺序即模型
  侧真实顺序：工具定义 → 系统提示词 → 消息序列（消息后跟时间提示）→ 运行
  环境（尾部）。之后每轮内容不变就不再出现任何提示词卡片——只有系统提示词
  / 工具定义变更、「运行环境更新」（含行级 diff，如能力开关、权限模式、
  工作区、技能清单等**真实变化**）才在发生位置补一条折叠行。每次请求都会
  完整重发系统提示词（协议如此），「不变」指逐字节相同，前缀缓存据此命中。
- **时间不在环境快照里，走独立的 time 通道**（`context.go` 的 `App.timeAfter`）：
  每条用户侧对话消息——真人用户（`kind=chat`+user）与 AI 发出的（`kind=aiuser`）——
  后面跟一条只含发送时刻的短 user 消息（`（发送时间：…）`），**组装期从该消息
  自身的落库时刻派生、不落库**（历史查询工具里没有噪音行；同一份
  存储重复组装逐字节相同，前缀缓存不受影响）。**前端按同一规则派生并展示**：
  每条用户侧消息后面一条「时间提示」折叠行（与运行环境行同款）——所见即所发。
  用户在哪一刻说的话就记哪一刻——
  不是挂钟，模型自己执行期间不产生新的时间消息。环境快照因此只在真实环境变化
  （权限/能力/模式/工作区/技能）时才重发「环境更新」，用户发消息不再连带触发。
  **别把时间改回环境快照或挂钟**：前者让每条用户消息多一条环境差分，后者让
  「每分钟跨过分钟边界」变成一次环境变化、重发变成时间驱动。
- **前后端消息类型契约**：数据库怎么存（`messages.kind`）、SSE 怎么发、前端
  用什么样式展示，统一定义在**《消息类型规范.md》**（对话内容 chat·aiuser /
  上下文注入 prompt·env·summary·time / 系统注入 report / 审计工件 request·audit；
  report 与 aiuser 按 origin 分子类）；前端按 kind 分发渲染（`ROW_ITEM_BUILDERS` 注册表），
  改展示只动前端、加类型只动后端；**改类型必先改规范再动代码，禁止按正文
  前缀或 JSON 内容猜类型**（旧库兼容除外，清单见规范 §5）；通道/折叠/展示的
  **设计意图速览在规范 §8**，改规则前先对齐意图。
- **右侧边栏**（`#rpanel`，对齐数字助理2 ui-layout 的 auto 宽度模型）：弹出即
  占用对话区以外的剩余宽度——不动左侧栏、不动对话区宽度；拖左缘同时调宽对话区
  与右栏，落盘的只有对话区宽度（`jx.contentWidth`，与对话区两侧拖把手共享），
  右栏宽度从不记忆，关闭再打开重新占剩余宽，等效记住拖拽结果；窗口缩放 / 左侧栏
  折叠时右栏跟随重算，窄视口停在最小宽 300px 由对话列让步兜底。面板为通用外壳
  （`rpRegister` 视图注册 + 标签头），视图分两型：`native`（容器是 index.html
  静态 DOM，行为代码挂在本仓 js，如子对话与浏览器）与 `iframe`（独立面板页
  `web/panel/<name>`，惰性创建 iframe、首开加载此后常驻不重载）——面板页自成
  一体（自己的 HTML/JS/CSS + 直连同源 API），加新面板只需建目录 + 一行
  `rpRegister`。「浏览器」视图是 native 型：工具栏/状态条为本界面 DOM，网页
  区域是占位 div，桌面模式下原生 WebView2 控件按上报矩形精确覆盖（见下）。
- **右栏跟会话走（会话作用域状态，app-right.js 的 rpSess）**：右栏的开合、
  正在看的视图、各视图内的选择（子对话选中、计划选中、Git 仓库）
  都是**按对话独立**的状态——切换对话时整体切到目标对话自己的右栏（`openSession`
  的 `rpSnapshot`/`rpRestoreSession`；回首页走 `resetEmpty` 的空态恢复），跨对话
  共通的只有对话区宽度。视图经 `rpRegister` 的 `saveSession()`/`restoreSession(saved)`
  挂点交出/就位自己的会话态，**视图与组件不再各自监听会话切换**（git 面板曾经的
  `session:open` 监听、计划视图在 `loadSessionInto` 里的专有重置都已删掉，统一
  走这条）；`browser` 这类全局态视图不挂钩子，切对话时行为不变。晚于会话切换
  注册的视图（组件热加载）由 `rpRegister` 补恢复。状态只存内存：刷新页面回默认
  （收起），会话删除随 `removeSessionLocalData` 一并清掉。**待审批计划自动打开
  （`maybeAutoOpenPlan`）在恢复之后跑**，必要时把右栏翻到计划视图——审批优先于
  恢复。
- **两侧开合动画**：左右侧栏共用同一个 `slidePane()`（宽度过渡，起始宽先于
  `.anim` 提交保证起点正确；`transitionend` + 460ms 兜底双保险定版；拖拽调宽
  即时生效不经动画；`prefers-reduced-motion` 下自动禁用）；左侧栏宽度可拖
  （右缘把手，272~560，记忆 `jx.sbw` 经 `--sb-w` 生效，双击恢复默认；窄屏浮层
  不可拖），内容装在与栏同宽的 `.sb-inner` 里，收起时内容不回流、像抽屉一样
  滑出。时间线分组下每条对话行首带一枚工作目录小胶囊（目录名开头两三个字，
  塞不下从右缘直接裁断、不用省略号），悬停可见全名。
- **左侧栏的状态标记**（`app-left.js` 的 `sessionItem`）：一行 = 左侧圆点 + 标题 +
  时间位。圆点只放「要你做点什么」的三态（橙=待审批 / 绿=完成未查看 / 红=标为未读，
  打开即清）；**运行态占用最右的时间位**——`⟳ 运行中`（后台还在跑，切走不等于停掉）、
  `⚠ 已中断`（最近一轮没正常收口：达到步数上限 / 执行失败 / 服务重启，悬停给原因与
  下一步），两个都没有才回落到相对时间；同槽即天然互斥，渲染优先级
  运行中 > 已中断 > 时间。异常标记**打开会话不清**（「最近一轮没跑完」这个事实
  没变），重跑一轮、重试成功即消失，用户主动停止不算异常；值存 meta 表
  （`laststop:<会话id>` = step / error / restart），重启后仍在（`Store.SetLastStop`）。
  带标记的行与「运行中」完全同款：两个标记的字符、颜色类与悬停文案收在
  `app-left.js` 的 `STATUS_CELLS` + `sessionStatusCell()` 一处，样式只给 `.s-time`
  叠一个颜色类（**别给某一个状态单独写位置规则**，写歪两个状态就对不齐）；悬停时
  与时间文本一样让位给右侧行内按钮（`.row-acts` 钉在 `right:6px`）。
- **子对话视图**（右栏首个视图）：顶栏最右「收起/展开右侧栏」按钮开关右栏
  整体（收起后再展开回到上次看的视图；页签条切换子对话/浏览器等视图）；视图顶部下拉
  菜单选择某个子对话或**新建**（人工发起的分支克隆；助理派发的工作任务是
  顶层会话，不在此列）。子对话
  **只在右栏显示、绝不切换主对话区**：右栏内嵌完整的对话区+输入框，与主对话
  共用同一套渲染/发送代码（`renderMessages` / `send` / `handleEvent` 全部按
  `view` 参数化：`mainView` / `subView` 只差 DOM 挂点与状态），后台自动汇报
  的流式输出也实时进右栏。没选子对话时点发送会弹出下拉提示。
  **样式与底部控件也完全同款**：消息列与输入区（`.msg-col` / `.composer-wrap` /
  `.composer-card` / `.composer-input` / `.cbtn` / `.chip-btn` / `.ctx-ring` /
  `.mode-flags`）都是 class 共用规则，两侧各挂一套（右栏那套 id 加 `sub-` 前缀），
  `viewEl(view, id)` 按视图取元素；权限芯片、模型芯片、上下文圆环、加号菜单
  （上传/计划/目标/导出/远程）、语音与发送全部作用于各自视图的会话，弹层是单例
  但记住是谁打开的（`popView`）。插件经 `JX.addComposerControl` 注入的控件在
  右栏镜像一份（点击转发给原件、class 同步）。
- **计划视图**（右栏，`rpRegister('plan')`，标签顺序 `order`：子对话 1 / 计划 2 /
  浏览器 3）：把 `exit_plan_mode` 提交过的计划当文档看——**Markdown 渲染**（表格/
  清单/代码块都正常展开），顶部可选多份计划（下拉里标「待审批」）、**导出**、复制
  全文、重新读取，底部在待审批时钉着「批准并开始执行 / 反馈意见后修改 / 忽略计划」
  （忽略＝搁置，计划仍留着）。数据两路
  合一：历史走 `GET /api/sessions/{id}/plans`（后端从会话日志里读回那一次调用参数，
  与对话同寿命——刷新、切走、重启进程后都还在），待审批的走 `/api/questions`
  里 `kind=plan` 的提问（按正文前缀与历史配对，配不上先用提问自带正文兜底）。
  **导出**走 `POST /api/sessions/{id}/plans/export`：把当前这份写成会话工作区里的
  `计划-YYYYMMDD-HHMM-<标题>.md`（标题清掉文件名非法字符、限 40 字；本地直写 /
  远程经 SFTP，与「文件上传」同一条文件环境），**同名不覆盖**（顺延 `-2`、`-3`），
  视图顶部随即显示落盘路径 + 「打开所在文件夹」（走 `POST /api/open-folder`）。
  **选中哪一份**：优先「一次性定位」——刚点的那张计划卡（按提交序号）、刚到达的待审批
  计划（按提问 id）；其次是用户在下拉里显式选过的那份（它消失、或又来了新计划就作废）；
  再其次是**最新的那份待审批**（新计划提交后右栏默认翻到新的，不再停在上一版——这正是
  「提交了 V2、右栏还显示 V1」的修法）；最后才退到上次显示的那份 / 最新一份。
  **计划提问到达时自动打开**这个视图（用户手动关掉后该提问不再自动弹，点那张计划卡
  ——整卡可点——随时能再打开）；提问被批准/修改离场前先留一份正文副本，右栏不会
  突然空掉。
- **上下文用量圆环**（输入区，模型选择左侧）：小圆环以弧长动画展示当前会话
  下一次请求占上下文窗口的比例，中心显示百分比（≥60% 变琥珀、≥85% 变红）；
  鼠标悬停浮出详情卡：总量/窗口、分类占比（消息 / 系统工具 / 系统提示词 /
  摘要 / 环境快照 / 其他，堆叠进度条）与**缓存命中率**。纯展示无点击。数据来自
  `GET /api/sessions/{id}/context`：按 buildContext 同一组装规则估算各分类
  token；命中率来自每次模型调用流尾 `usage`（`stream_options.include_usage`，
  命中量按端点字段名归一，见 `Usage.CacheHit`：DeepSeek 的 `prompt_cache_hit_tokens`、OpenAI/智谱 GLM 的 `prompt_tokens_details.cached_tokens`、Anthropic 兼容层的 `cache_read_input_tokens`），按会话累计落库（sessions 表
  `usage_prompt/usage_cache_hit/usage_calls` 三列，迁移 0005）。
- **对话区选中文字浮窗**：在主对话区或右栏子对话区选中文字后，浮出深色胶囊
  工具条（对齐主流 AI 编程助手的选区交互）：**复制**、**引用到下方输入框**
  （插入主输入框光标处）、**在右侧辅助问答**——自动打开右栏子对话视图并
  找/建子对话，把选文填入子对话输入框由用户发出，全程不影响主对话区；
  点浮窗不冲掉选区，选区消失 / 滚动 / Escape 即收起。
- **会话进度面板**（`progress_write` 的渲染）是对话区右上角的一枚可折叠浮窗：
  折叠＝一枚「进度」胶囊，展开＝从同一个右上角往左下长出的卡片（右缘与顶边不动，
  原地展开不跳位）。宽度按对话区右侧留白算：放得下就塞进留白、不遮正文，放不下
  就用一张舒适宽度的卡；默认只在留白够的时候自动展开，窄屏/中等窗口一律折叠成
  胶囊，用户点过之后按用户的选择来（刷新复位）。清单超过半屏高卡内滚动。它永远
  是浮层（不占对话区高度、切换不触发重排），层级压在消息之上、输入区之下——报错
  条必须始终看得见。宽度算在 JS（pgMetrics），CSS 只管摆位与两种形态。面板下半部
  的「运行中的程序」每行带停止按钮：POST /api/sessions/{id}/stop-child
  （userstops.go）——后台作业优雅取消（与 job_kill 同路径）、前台子进程按进程树
  强杀（killSessionChildren 的单点版），停止后向该会话投【用户停止程序】report
  注记（不触发对话，AI 下轮自然看到「用户停止的，不是出错」）。

## 子对话：上下文继承与一级限制

子对话只有一级（无论 AI 派发还是人工新建，都不能再派发/再建子对话）；
数字助理派发的工作任务是顶层会话，不占一级名额。

- **人工新建＝分支克隆**：右栏「＋ 新建」建的子对话照抄父会话的模式、工作区、
  远程配置、模型、权限与能力开关（因此**系统提示词、工具目录、历史全部一致**），
  并把父会话消息逐行复制过去（含摘要与压缩标记，`covers_up_to` 按新 seq 重映射）
  ——子会话下一次请求的前缀与父会话逐条相同，命中模型 KV 前缀缓存，成本很低。
  计划/目标模式刻意重置，避免子对话替父会话自动续跑。老 `aux` 只读答疑会话原样保留。
- **继承段折叠显示**：复制内容前有一条 `kind='inherit'` 标记行（组装上下文时跳过），
  前端据此把整段历史折成一条「来自主对话 · N 条（含系统提示词）」折叠条，展开即
  完整对话流；父对话的系统提示词快照行也在其中。
- **AI 派发按专家预设**：`dispatch_sub` 的模式选择只有一条路径——`expert=名称`。
  内置两条系统专家（常开、不可修改）：**标准子对话**（不传 expert 时的默认，
  只收到一段自足的 task）、**继承上下文子对话**（完整继承本会话聊天记录，
  子会话保留自己的 task 系统提示词与工具，task 只用写「接下来要做什么」）。
  独立的一次性任务按默认派、写清描述更省 token（工具说明里写明了这条取舍）。
  `dispatch_task`（工作任务）**永远不带上下文**：像用户发消息一样只给一段
  自足描述，与派发方的聊天记录完全隔离。
- **子对话专家（专家团预设）**：用户可在设置页「子对话专家」预制定义专家——
  名称、描述、**追加提示词**（拼在子对话模式基础系统提示词之后）、默认模型
  （空 = 继承主对话）、是否继承上下文、启用开关。**名称即调用参数，与内置
  专家共用同一条 `expert=名称` 路径**：设置页展示的名称直接来自后端清单
  （`/api/experts` 的 `builtin` / `user`），模型侧的可用名单由常驻的
  `list_experts` 工具按需查询（**名单不嵌进工具描述**——工具目录恒定，
  专家增删不断前缀缓存；对齐 read_skill 的技能模式），所见名称 = 可填
  参数，100% 一致，不存在第二条按参数调用的路径（历史上的
  `inherit_context` 参数已删除，旧调用里传了也会被忽略、继承与否完全由预设
  决定）。`model` 参数仍可覆盖预设模型。**预设模型不可用自动回退**：专家会话
  调用失败且错误属于「该模型/提供方用不了」（余额或额度耗尽、鉴权失败、
  模型不存在、提供方连不上；纯限流不算）时，自动切回全局默认模型（= 主对话
  的默认模型）续跑并落库，工具区提示一条「已切回默认模型」；普通会话手选的
  模型不做这种静默降级——显式选择被拒时应如实报错。会话上只记专家名（`sessions.expert`
  列），提示词构建时按名解析拼接——停用专家只是不能再派（存量子对话的追加段
  保留），删除才会让这段消失。自定义专家不得与内置专家重名；存
  `config.json` 的 `experts` 键（`GET/POST /api/experts`、
  `POST /api/experts/delete`），老配置首次升级自动补种演示用的
  「对抗性审查编程助手」（继承上下文、模型随主对话），删掉后不再复活。
- **一级限制**：`POST /api/sessions` 校验父会话自身无 parent（400 拒绝），
  `dispatch_task` / `dispatch_sub` / `send_to_session` 在带 `ParentID` 的会话里
  调用被硬拦
  （人工把总控克隆成子对话后，那个子会话手里也有编排工具），环境快照里也会告知模型。
  父会话正在执行时只复制到最后一条完整答复，悬空工具调用不带进子对话。

## 会话的四个层级

| 模式 | 说明 | 工具 |
|---|---|---|
| `task` | 任务会话，实际干活 | 读/写/编辑/glob/grep/shell（含后台作业 job_*）/web_fetch/**web_search（联网搜索，经服务端搜索网关，见 websearch.go；服务端半边是 jxaiserver 的 search.go，协议改动需双侧同步）** + ask_user_question / exit_plan_mode / progress_write + **dispatch_sub（派子对话）** + **call_api（与前端同接口的通用直调，见 callapi.go）** + **request_manager（找长期助理协调，见 requestmgr.go）** + 历史查询 |
| `assistant` | 数字助理总控（左上角唯一入口，不占侧栏） | 编排：list_sessions / read_session_summary / **dispatch_task（派顶层工作任务）** / send_to_session（给已有会话发话：执行中＝**插话**注入当前轮、空闲＝直接开跑，可选 permission 只能同等或降权）/ stop_session（按用户指示分级停止：协作取消→3 秒未收口自动强制终结，与人工停止按钮同一停止函数、仅触发者不同）/ **call_api（与前端同接口的通用直调，见 callapi.go）** + deliver_result（把派发产出的成果文件登记为本对话的交付卡片，只展示不搬文件）+ **只读四件套 read/list_dir/glob/grep + web_fetch（答小问题：约十步内的查看自己看文件回答，大文件通读与批量分析仍派任务；写与执行类工具绝不给总控——纪律见 agent.go 的 assistantPrompt）**+ 历史查询 |
| `aux` | 辅助对话：早期的只读答疑小会话（右栏可见；新建子对话自 0007 起改为分支克隆，见下节） | 只读文件 + web_fetch/web_search + 历史查询 |
| `skill-create` | 创建技能：把「怎么做」沉淀成给后续 AI 照做的 SKILL.md 教程——用户点按钮录演示让 AI 分析，或直接派活让 AI 探路总结；**默认完全访问**（演示记录与亲自验证都刚需电脑/浏览器）；提示词核心是「顿悟操作逻辑 + 写大致步骤不写死细节」（推荐/避免方式、快捷键、抗界面变化，见 agent.go `skillPrompt`） | task 全量工具 + **register_skill（注册/覆盖技能）**；演示录制归用户直控（界面按钮经 `/api/demo-record/*`，材料以【演示记录】通知进对话） |

（各模式共有的常驻工具不再逐行列出：`read_skill` 对**所有模式**注册，
技能清单经环境快照对**所有模式**注入——见下文「技能」节与启动自检
`assertSkillVisibility`。）

**派发关系**：数字助理（总控）只有一个派发工具——`dispatch_task` 派出**顶层工作
任务**（进左侧栏，`dispatched_by` 记派发它的助理、作为回报链），**永远不带上下文**，
像用户发消息一样只给一段自足描述；`send_to_session` 给已有会话发话，默认语义是
**插话**——目标执行中就把话注入它正在执行的这一轮（模型调用中**立即打断**其
思考/正文输出、下一步带着话重调；工具执行中等下一步调用模型前注入；投递统一走
`deliverSub` → `deliverToSession`，AI 发话一律插话、不看排队偏好；等价 call_api 调
interject mode=steer），目标空闲则直接作为
新需求开跑；中途发话一律立即生效，想等对方干完再派下一个活就等【派发回报】来了
再发（可选 `permission`，与派发同一套规则：只能同等或降权，且不得高于目标会话
自己的档位——给已有会话提权会被拒绝，见 assistant.go 的 `runSendToSession`）。
**子对话（`dispatch_sub`）不在总控手里**：它是任务会话的并行手段——挂在当前会话下、
右栏抽屉可见，完成后回报并触发父会话继续，按专家预设决定是否继承上下文
（`expert=名称`，默认标准子对话不带）；子对话只有一级，对所有子对话生效。此外总控
还有 `list_sessions`（找任务）、`read_session_summary`（看最终答复等摘要）、
`read_session_messages` / `search_sessions`（翻查内容）与 `deliver_result`（汇报派发
成果时把文件登记成交付卡片展示在本对话——文件本身可能已被任务会话交付到位，总控只
负责让用户在总控对话里看得见、拿得到，不重复搬运；路径填绝对值，登记失败多半是文件
已被搬走，翻该会话的【交付结果】/【交付完成】注记找新位置）。

## 执行的生死与对话中途发消息

**执行体独立于发起它的 HTTP 请求。** `POST /chat` 只是「发起执行 + 接上这条流」，
后端 `executeRun` 在独立 goroutine 里持有自己的 `ctx`（`runtime.go`）：

- 浏览器刷新、关页、网络抖动只断开那条订阅流，**执行照旧跑完并落库**；
  重新打开这个会话时前端会自动 `GET /stream` 接回去，接着看实时输出。
- 只有 `POST /stop`（停止按钮）或进程退出才会终止执行；`sessions.status` 只由
  内存注册表与 `UpdateStatus` 维护（`SaveMeta` 刻意不写这一列，否则会被陈旧对象冲掉）。
- **中断不破坏历史（对齐 DSH 的 abort 语义）**：任何中断（人工停止/网络异常/
  进程崩溃）都会给未执行/被中止的工具调用补上合成结尾（「（会话被中断，…）」，
  `origin=interrupt`）；已在执行的调用不被 orphan（等它结算），未启动的不再启动，
  历史里绝不留下「有调用、无结果」的悬空配对（发送视图 `repairToolPairing` 还会
  兜底重排/修复；重试与新消息开轮前先扫尾补齐残留）。模型下一轮能看到这些结尾，
  自行决定要不要重做。

**执行中发消息（两种行为，设置页「对话中途发送行为」切换）：**

> 这套选择只属于**用户自己**在执行中发的消息；**AI 侧发话**（数字助理派发/转达、
> 派发回报、任务求助、待办转交）一律**插话**，不看这个设置——见 `deliverToSession`。

| 模式 | 生效时机 | 前端呈现 |
|---|---|---|
| 插话 `steer` | 模型调用中（思考/正文输出/重试等待）**立即打断**当前模型流：半截输出带「（被插话打断，输出提前结束）」标注落库，下一步带着插话重组上下文重新调模型；工具执行中不打断，等下一步调用模型前注入（原有行为） | 立刻贴在对话最底下（虚线气泡 +「插话中」角标），落库后换成正常气泡 |
| 排队 `queue` | 本轮结束后作为**独立一轮**自动发送 | 进输入框上方的排队坞；可逐条删除，或点 ↑ 提前为插话 |

两边的队列都在服务端（`PendingMsg`），刷新页面/换窗口不丢、多窗口一致；本轮结束时
还没注入的插话自动转为排队，消息不会卡在队列里。会话空闲时发来排队消息会自动拉起一轮。
**排队项与插话气泡都可点 ✏️ 编辑**（撤回式，参考 ZCode）：先从服务端队列权威删除
（复用 `remove`），成功后才把原文回填输入框当草稿——消息要么在队列、要么在输入框，
不会有两份；输入框里已有草稿时拒绝（防覆盖），改完重发按当前模式重新排队/插话（排到
队尾）。编辑只对用户自己发的消息开放（kind 空/chat），数字助理派发注入的不参与。

**排队坞增强**（对齐 ZCode 的队列面板）：多条消息可**拖拽排序**——行首把手拖动，
落在上半行=插到该行之前、下半行=插到该行之后，锚点 id 上送服务端定序（`move`），
顺序的唯一权威在服务端（触屏浏览器不支持 HTML5 拖拽，排序走桌面端）；头部带
**暂停/继续**开关（会话级、内存态，重启即清）：暂停后本轮照常跑完，排队消息保留
不自动续发（空闲时新来的也只排队不拉起执行），恢复时空闲即接着把队列发完；执行
停止/出错后队列同样保留，排队坞横幅给「继续发送」入口（= `resume`，空闲即拉起）。
暂停状态随 pending 快照（`paused` 字段）下发，多窗口一致。

**不静默、也不往对话里塞话**：对话区只放真实对话（用户消息 + 助手输出 + 工具过程）。
正常完成与用户手动停止**不产生任何附加内容**；模型调用因网络失败（连接失败/超时/
流中断/网关 5xx/限流 429）先**自动重试**：总共尝试 5 次（间隔 1s/2s/3s/4s 递增，
输入区上方提示条显示进度，期间可随时点停止），仍不通才收手；请求级错误（鉴权失败/
余额不足/模型不存在）与上下文超限不自动重试。只有真实的失败（模型/网关/网络——余额不足、
限流、鉴权失败、连不上、超时，以及模型一条都没输出、只出思考、达到最大步数）才显示在
**输入框上方的报错条**里：原样呈现模型侧返回的原因，配「重试」按钮（`POST /retry`——
不重复落库用户消息，直接带着当前上下文再跑一轮）与关闭按钮。重试开始时会先补齐历史里
残留的悬空工具结尾（`repairDanglingToolCalls`），保证重发的历史配对完整。报错原因存在
服务端会话上，刷新/切回来还在，新一轮开始自动清掉。异常收尾同时写日志，服务端不留无声的停摆。

**任务最大步数（设置·常规页「任务执行」）**：`config.maxSteps` 0/负数＝**不限制**，
这是默认——复杂任务一直跑到模型自己收尾（不再发起工具调用）或用户点停止为止，不再靠
猜一个整数。设置页就一个输入框（`min=0`），**「填 0 = 不限制」直接写在说明文字里**，
不另设开关（开关是冗余），回显 0 就显示 0、保存原样提交（含 0）：回显与保存都走
`app-settings.js` 的 `syncMaxSteps`，没有硬编码兜底默认值（旧的 `c.maxSteps || 200`
会把「不限制」显示成 200，再一保存又把 200 写回去）。老配置里还是历次默认
（30/100/200/1000，其中 1000 是旧输入框上限＝想拉满）的，`LoadConfig` 迁到不限制并
立即落盘；手填过其他值的一律保留。撞上限时仍按 `errMaxSteps` 收尾（侧栏「⚠ 已中断」、
报错条给「重试」）。

## 后台派发与自动回报

`dispatch_task` / `dispatch_sub` / `send_to_session` 立即返回，被派发的会话在后台
goroutine 并行执行；
进度经全局事件流 `GET /api/events` 实时推送（sub_start / sub_tool / sub_end /
report_ready / done / stopped），前端常驻订阅：父对话里的子任务卡片实时滚动工具
活动，完成后自动注入【派发回报】（助理主对话收到「向用户汇报」的指示、工作任务
收到「带着结果继续推进」的指示），父会话空闲则自动跑一轮消化回报，正忙则**插话**
注入它正在执行的那一轮（AI 发话一律插话，不排队）。**人为叫停是唯一例外**：用户（或数字助理按用户指示代停）叫停的任务，其回报走 deliverToSessionNoKick——只落库/插话，**不自动拉起**父会话那一轮（用户刚叫停，数字助理被回报自动叫醒正是它自作主张重派/续跑的来源；用户下次与它对话时自然看到回报）。正看着父
会话时会自动接上这轮流、实时看输出；没在看则侧栏亮「完成未查看」绿点，打开即清。
服务重启时未完成的执行会补
【系统中断】记录，发「继续」即可接续（原始历史完整保留）。

## 任务找经理协调（request_manager / requestmgr.go）

任务会话在「问自己看不到的情况」或「要一个同时只能一个人用的资源」时，可以
主动找长期助理（数字助理主对话）——这是任务侧**唯一**能触到经理的通道，也是
经理跨会话视野真正被用起来的地方（典型案例：多个会话改同一仓库，谁都不知道
现在合不合适跑测试）。

- **语义是「投一条消息」**：`request_manager` 把 topic/detail/resource/expectation
  组织成一条求助消息，走与用户消息完全相同的投递收口 `deliverToSession`
  （kind=aiuser、origin=help，前缀【任务求助｜会话标题（id）】）——经理空闲就
  落库并占住它这一轮（`kickSessionRound` 唤醒，与【派发回报】【待办任务派发】
  同一套机制），正忙就**插话**注入它正在执行的那一轮（模型调用中立即打断其
  当前输出、下一步就能看到，工具执行中等下一步调用模型；AI 发话一律插话
  不排队）。**工具立即返回，任务不等答复**：
  没有阻塞等待，就不存在「任务停在那等经理、经理又在等任务」的互相等死。
- **经理侧没有新工具**：它用既有的 `send_to_session`（中途发话默认插话）、
  `dispatch_task`（派新任务）、`list_sessions` / `read_session_summary`（看谁在跑、
  改了哪些文件）处理——回不回、什么时候回都由它按全局情况定（比如先不动，
  等另一个会话的【派发回报】到了、资源空出来再派测试任务）。
- **不成池、不加接口**：求助消息落在经理会话历史里就是记录（可搜、可导出、
  重启还在），因此没有求助池、没有新端点、也没有界面改动。
- **边界**：只挂五个执行模式（task/office/coding/beginner-coding/skill-create，
  启动自检 `assertManagerChannel` 看护）；子对话调用被硬拦（一级限制同口径）；
  计划模式暂停；只带文本、不携带权限——经理后续动作仍受它自己的权限档与用户
  审批约束，这条通道不构成提权路径。
- **提示词成对**：执行模式拿 agent.go 的 `managerAskPrompt`（什么时候找、找什么、
  不用等），数字助理拿 `managerCoordPrompt`（它们不等你、你可以插话/派活/先不动、
  排先后是你的事）；工具说明只留一句「这是求助用的，不要滥用」。

## 上下文管理（核心特色）

原始历史 append-only 永久保存在 SQLite；压缩只影响「发给模型的内容」。

- **历史查询工具**（所有模式可用）：`search_sessions` 跨会话全文搜索、
  `read_session_messages` 分页读任何会话的未压缩原文（`seq=N` 可整条直取——
  规则压缩的动作行里方括号编号就是 seq，回查一步直达）。
- **自动压缩：间隔制规则折叠 + 模型阈值兜底**。规则通道不再按窗口百分比触发，
  而是**间隔制**（CompactInterval，默认 40 万字）：自上次压缩边界起新增未压缩
  内容达到间隔就做一次零成本规则折叠；间隔与保留共用同一计量单位
  （CompactUnit：chars 字数，默认 / tokens 用模型返回的 usage.prompt_tokens
  真值差值——不估算，换模型时基线自动重建）。模型阈值（CompactLlmPct，
  默认 80% 对窗口）仍是最后防线：间隔触发折叠后重新组装、估算仍超才生成摘要
  （规则关闭/间隔未到/压不动都直接落到摘要）。间隔制天然节流（压完增量归零），
  **不再有阈值百分比与按轮冷却参数**；上下文真超限时溢出兜底把启用的路都用上
  （先规则折到只留 1 轮/全折，再摘要到只留 1 轮/全部）。
  CompactMode 只决定手动「压缩」按钮的默认行为（llm 摘要，默认 / rule 折叠）。
- **规则压缩**（零模型调用）：把保留窗口之外的历史折叠落库。保留窗口从最新
  往前累计 KeepChars 字（默认 10 万），预算耗尽处（命中点）所在的轮**整轮保留**
  ——轮首可以是用户消息，也可以是数字助理转发消息（report/aiuser，同权开轮），
  不必非数到用户亲发的消息；命中轮自身超预算（巨轮，单轮可达百万字）时回退
  行级窗口，只折巨轮前半段，防止压不动。折叠语义（v2 动作行制，2026-11）：
  用户输入与模型正文保留原文；工具调用折叠为**动作行** `[seq] 工具 目标 —— 意图`
  （seq 是数据库主键，read_session_messages(seq=N) 一步取回完整原文；意图取
  调用前正文首句，缺省不编造）；工具结果按类型分派——run_command/run_python
  输出**三段式**（头 2 行 +（中间已裁 N 行 · 全文取 seq）+ 尾 6 行，失败放宽
  到 10 行；单行超 300 字另有行内裁切），edit/write 成败一句话（成功「编辑
  成功/已写入」，失败保留原因），grep 留命中摘要（**零命中必须保留**），
  可再生类（read/glob/回查工具）省略，其余一行结论；**多模态消息的图片本体
  卸载**为标注行（文本部分保留，图片的信息价值由看图后的正文结论承载）；
  edit/write 的参数**默认原样保留**，单条超 2000 字才头 1600 + 中间标注 +
  尾 400；思考过程、环境快照丢弃。每次压缩完成落一条「压缩区结束 + 变更账本
  （区间内 edit/write 的路径与 seq）+ 结论摘录（正文中「结论：/根因：/原因：」
  起头的原文行子集）」的 report 行——账本随历史永存。二级只留用户输入与
  模型最终输出，仅在压缩范围内已没有新轮可折时才升级（手动重复点同一位置
  即二级；一级压得动绝不跳二级）。
- **模型摘要压缩**：估算 token 达模型阈值（CompactLlmPct）自动触发；也可在
  任意助手消息上点「压缩」手动触发（按设置的默认行为选路）。**手动/提前自动
  压缩的模型摘要在后台 goroutine 执行**（HTTP 立即 202 返回，完成与失败经
  session_updated 广播 reason=compact / compact_failed；会话级互斥拦重复点火，
  compactRunning）——绝不把一两分钟的摘要调用挂在请求上。手动摘要独立
  叠加、永不重压（重复点同一位置被 summarizePlan 预检同步拦下；摘要/审计等
  元数据行会把轮尾撑大一号，区间内只剩元数据行同样按「已压过」报错，不落
  空摘要）；自动摘要按 DSH 式合并为单一检查点，摘要区间与规则压缩同一
  保留窗口口径。
- **压缩后环境快照完整重发**（`resendEnvAfterCompact`）：环境快照对模型是
  差分机制（首条整份、后续只发变化行），而折叠/摘要会把差分所依附的完整
  基线埋掉——之后模型手里只剩孤立差分行或缺掉整段环境。所以每次压缩成功
  （规则折叠/二级升级/摘要落库）都**无条件**落一条当前完整快照：基线被埋
  时它成为保留区首条可见快照、组装整份渲染；环境没变时它同文于可见快照、
  差分为空零冗余（不做「基线是否被埋」的判断——差分行与完整行的可靠特征
  难找，压缩又是低频事件，无条件最简单可靠）。
- 组装管线：`[系统提示词] + [活跃摘要] + [边界后消息（规则折叠视图 + 保留窗口内的完整轮原文）]`。

## 多模型接入

设置页「模型服务」可配置多个 OpenAI 兼容提供方（预置 DeepSeek / 小米 MiMo /
智谱 GLM（按量与 Coding 套餐两个端点）/ 火山方舟·豆包（按量与 Coding 套餐）/
Kimi / 通义千问 / 百度千帆 / 腾讯混元 / MiniMax / 讯飞星火 / 阶跃星辰 / 硅基流动 /
魔搭 ModelScope / OpenAI / Ollama 本地 / 自定义），填 Key 后一键拉取模型清单；
预置模板带 Note 备注（计费方式、Key 获取入口），设置页卡片实时取模板展示——
同一厂商的套餐端点与按量端点地址不同，填错端点扣不到套餐额度，这是拆成
多条预置的原因；
会话顶栏可单独切换本会话模型；派发时可指定子会话模型（providerId/modelId）。

**API Key 的存储（各平台各用各家，原则：不回显、不被顺走）**：落盘前经
`protectSecret` 保护——Windows 用 DPAPI 加密（`dpapi:v1:` 前缀，绑定本机
账户）；安卓/Linux 没有系统级密钥服务，明文落盘但 config 文件一律 0600
（安卓上配置位于应用私有目录，系统层已隔离其他应用）。任何情况下接口层
都不回显明文：`GET /api/config` 只返回掩码（`maskKey`，前4+****+后4），
保存时空 Key = 保持不变。手机版后续接入各家的系统凭据服务（如
Android Keystore）时，只需替换 `dpapi.go` 的平台实现，文件格式不变。

## 技能（图片 / PDF / OCR / Office）与能力组件

在基础工具之上，能力分两类组织：

**技能（`技能/` 目录）**——文件类能力，标准技能包形态：
每个技能一个独立文件夹 = `SKILL.md`（标准 frontmatter 说明）+
`manifest.json`（Python 依赖清单）+ `scripts/`（执行脚本）。
自带三个技能：`pdf`（diagnose/text/table/image/embed 五模式，先诊断
文字层可信度再选通路）、`ocr`（离线 RapidOCR 识别图片文字）、
`office`（docx/xlsx/pptx 操作要点与结构校验），以及两个**不带 Python
的工具型技能**：`downloader`（内置 aria2c 官方二进制，Windows/安卓
下载落盘，缺 curl/wget 环境的兜底）、`apkinfo`（引擎子命令 `apkinfo`
读取 APK 包名/版本/签名指纹，安装前验包，纯 Go 实现）、`web-search-local`
（本机网络搜索：web_search 连不上服务端时的本机直调搜索 API 兜底，
2026-10 由用户技能提升为内置；存量机器 user/ 下的旧副本由迁移任务
20261030.builtin-skill-from-user 收走）。自带技能常开、不可
删除；`技能/user/` 下可放用户技能，设置页「技能」面板可添加（按钮选本机含
SKILL.md 的文件夹，或 .zip 压缩包/单个 .md 说明文件——.md 自动包装成
`<文件名>/SKILL.md` 再装，id 取文件名；**文件夹、.zip、.md 拖进技能页即装**
——文件夹经 webkitGetAsEntry 递归遍历走 `POST /api/skills/install-files`，路
径穿越拒收；单文件走 `POST /api/skills/upload`）、
开关、删除。SKILL.md frontmatter 认 name/displayName/description/version/tags：
**name 保持行业规范**（Agent Skills：小写字母/数字/中划线），**displayName 是
给人看的显示名（中文为主）**——技能清单、环境快照、设置页与市场卡片都按
`displayName → name → id` 逐级回退显示（对齐 VS Code 的 name/displayName 与
本仓库组件「英文 id + 中文 name」的既有惯例）；description 超过 200 字在环境
快照里截断（快照每会话注入，防超长描述污染上下文；全文在 read_skill），发布
侧按行业规范硬校验 name/displayName ≤64、description ≤1024（`market_publish.go`）。
安装口与发布口共用 `checkSkillMDBody` 内容体检：空 SKILL.md / 只有 frontmatter
的包两边都拦（装得上用不了的东西不进市场也不进本机）。**内部技能绝不随包
分发**（2026-10-30 事故与修复：20261028-1 及更早安装包把开发机 技能\user\
整目录误发给了客户，其中含密钥；三层修复——打包脚本 stage 删 user + iss
Excludes（`d1723e4671`）、go:embed 只嵌内置白名单目录（skills.go，exe 内嵌
母本不再含 user 树，`TestEmbeddedSkillsTreeIsWhitelistOnly` 防回归）、启动
清理 `purgeLeakedUserSkills`（skills.go，按 `leakedInternalSkills` 名单删
user/ 下误发副本，市场记账在的跳过；经迁移任务 20261030.purge-leaked-skills
随新引擎静默推给存量机器，跑一次记账）。要给用户
用的内部技能一律走市场正式发布）。用户技能按 **清单先行、
内容按需**送达模型：环境快照里只列 显示名（id）与一句话描述，要用哪个技能先
调用 `read_skill`（常驻工具，**所有模式注册**）读它的完整 SKILL.md 说明——
技能增减不改变工具定义；清单变化经环境快照补在对话**最底部**，并附一行
「技能变更：新增 X／移除 Y」明说增删了哪个技能，上文历史快照一律不动。
**技能清单是环境快照的固定条目，所有模式一律注入**（与操作系统/权限模式
同级，不挂模式谓词——技能功能晚于 office/coding 落地，曾因手写门控漏掉
这两个模式；现由启动自检 `assertSkillVisibility` 逐模式看护，任何模式丢了
清单注入或 read_skill 都拒绝启动，模式全集以 `sessionModes` 为单一事实源）。

**创建技能模式（`skill-create`）**——技能的生产端：用户演示操作（或直接派活
让 AI 探路），AI 观察学习后写成 SKILL.md 并注册进 `技能/user/`。闭环三件套：
①**演示记录器**（`recorder.go` 定义 + `recorder_windows.go` 实现，纯 syscall
无 cgo）：用户点输入框旁的录制按钮起录（`POST /api/demo-record/start`，模型
不持有录制工具）——WH_MOUSE_LL/WH_KEYBOARD_LL 全局钩子
（回调只投递 channel，慢活全在 worker；带 LLMHF_INJECTED 标记的自家
computer_* 注入事件被过滤）在每次点击、回车/Tab、窗口切换时截屏（复用
BitBlt 截屏、逐字节去重、上限 400 张），连续打字聚合成 text 事件（组合键
还原成 Ctrl+S / Alt+F4 / Win+E / Shift+Tab 整键记录，修饰键认主码+左右码、
同名 400ms 去重；中文输入法只记原始按键，上屏结果以截图为准），
waveIn 缓冲采集全程录 16kHz 单声道 16bit PCM（无麦克风自动降级）；
停录（按钮 / `POST /api/demo-record/stop`；桌面退出有 `recorderShutdown`
兜底：保音频、生成 INDEX）收录成 `工作区/演示记录/record-<时间戳>/{NNN.png,
events.jsonl, audio.wav, 讲解.srt, INDEX.md}`——INDEX.md 是时间线对照表（事件↔截图↔
字幕段），模型 read INDEX.md 定位、read_image 看图。停录即向所在会话追加
【演示记录】系统通知（同文件上传机制，origin=demo）；建技能会话时若工作区
已有材料则注入清单通知（`recorderRecentDirs`，首页「先录后聊」的入口）。
事件毫秒时间轴与录音/字幕同源（都取 time.Since(start)），天然对齐。仅
Windows 真实现，其他平台空桩报「不支持」（status 端点与环境快照提前公告，
模型引导用户口述替代）。②**随录随转（本地语音转写）**：录制期间同一路
PCM 喂 VAD 停顿分段——分段语义与语音组件听写线（组件/voice/client.js
makeLocalRec）逐常量对齐：350ms 起效 / 3 秒尾静音定稿 / 18 秒强制切段 /
静音垫 1.5 秒上限，能量判据 rms > max(0.008, 底噪×3.5)、底噪自适应。定稿段
经 `recorderASR` 钩子（`voiceasr.go`，NewApp 注册）直调语音组件的常驻
X-ASR worker（组件桥 callRaw，与前端听写同一通道），文本逐段落
`讲解.srt`（整文件重写，崩溃不丢已落盘段）；停录尾部不足 3 秒静音的余段
照转，等在途识别上限 8 秒。语音组件停用时优雅降级为纯录音（该段字幕丢弃），
停录通知与环境公告如实说明。③**注册闭环**：`register_skill(path[, update])` 走
`addUserSkill` 装进 `技能/user/`（update=true 先删旧再装，支持迭代改进），
`read_skill` 读回自查。权限默认完全访问（`handleCreateSession` 分支 +
前端 `applyDraftMode` 联动），用户主动点按钮起录与文件上传同哲学，不走模型
权限审批。

**运行环境自包含**：技能一律使用随软件分发的便携运行时
`技能/_runtime/python`，**不探测、不依赖用户机器上的系统 Python**——
面向非编程用户，安装包内置全部运行时与依赖，装完离线全功能。
开发目录缺 `_runtime` 时首次使用自动装配一次（python.org
embeddable，约 11MB + 清华镜像装 wheel），此后离线可用；装配进度只写
引擎日志（`skills.go` 的 `skillRuntimeNotice`），不进对话——曾经经 `tool_wait`
弹「技能运行时就绪（后续离线可用）」一类提示，被用户判为噪声、已下线；
打包脚本
会校验运行时依赖齐全，缺一不出包。`skillsDir` 配置可指定
技能目录位置（默认取可执行文件旁，不可写时落数据目录）。
**安卓走内置 Linux 运行时**（`skills.go` 的 `ensureTermuxSkillRuntime`）：
技能解释器 = Termux 快照里的 `python3`（执行带 `termuxEnv`，缺
`LD_LIBRARY_PATH` 动态链接器找不到 libpython）；office/pdf 依赖烘焙进
快照（清单与校验见 `android/runtime/MANIFEST.json`，PC 侧组装脚本
`android/runtime/pc-build/build-runtime-pc.sh`——WSL Ubuntu 里跑，不依赖
设备；设备路径备选 `make-runtime.ps1`，**制作机 ABI 即快照 ABI**，v1.0
误用 x86_64 模拟器产物导致 arm64 真机全 ENOEXEC，引擎装配期已加 ELF
架构防呆），快照缺的纯 Python 依赖现场经 pip 补装、装不上如实报错
（PyPI 无安卓轮子的 C 扩展依赖——如 onnxruntime/rapidocr——需加进快照
才能支持）；每次技能调用都核对依赖（探测结果缓存，常态零开销）。

**能力组件（与权限绑定的操作类能力）**：

- **图片查看**：`read_image` 工具 + 内容寻址附件库（`data/attachments/`）+
  `GET /attachments/` 端点。工具结果只带文本信封；前端在工具卡片直接
  渲染原图；视觉模型按预算内联 base64（单次最多 8 张），纯文本模型
  降级为信封占位不报错。
- **浏览器操作**（能力开关 `browser`）：`browser_open / content / click /
  type / scroll / viewport / evaluate / screenshot / close` 九件工具，工具层
  形态无关（`browser_ops.go` 的 browserTarget 接口），内嵌形态按运行形态取
  目标。三种形态可并存、各自独立进程与配置目录：**桌面版右栏原生面板**
  （`browser_open` 默认，mode=headless）——第二个 WebView2 控件真浏览器嵌入
  桌面壳窗口（`browser_panel_windows.go`），原生滚动/输入法/右键菜单/下载，
  AI 经控件官方 CDP 通道（`CallDevToolsProtocolMethod`）驱动同一控件，人机
  共操；**网页版后台无头**（chromedp 纯 Go CDP 优先驱动系统 Edge，异常缺失
  兜底 Chrome，仅供 AI 工具使用）；`mode=headed` 弹**独立可见窗口**（旧版
  行为）。其余工具作用于最近一次 `browser_open` 选定的实例，`browser_close`
  关当前实例并自动回退到仍在运行的另一个。**内嵌形态按会话隔离**（与有头
  独立窗口同款约定）：桌面右栏每个会话一个独立 WebView2 面板实例
  （`App.panels`，矩形上报按 session 路由，同一时刻只显示当前会话的面板、
  其余收屏外但实例保留，切回即恢复），网页形态每会话一个无头 chromedp
  实例；`browser_nav`/`browser_resize` 事件带 sessionId，前端只让本会话的
  事件刷工具栏/调布局。点击/输入前在目标页面内注入 AI
  位置标记（原生面板 HTML 无法浮于原生子窗口之上）；文本输入统一走
  `Input.insertText`（中文/输入法正确）；桌面面板登录态按会话落
  `data/webview-browsers/<会话>/`（无会话上下文用共享目录 `data/webview-browser`）
  与日常浏览器隔离。注意：新版 WebView2（Chromium 136
  起）封禁 `--remote-debugging-port`，chromedp 直连嵌入控件不可靠，官方替代
  即控件级 CDP 调用接口。地址口径：**不做 scheme 白名单**——浏览器能开
  什么由它自己决定（http/https/file:/// 照常，edge://version 这类内部页在
  chromedp 形态可真实打开、edge://settings 等 WebUI 禁调试只能看不能读写、
  WebView2 面板开不了 edge:// 是微软对控件的限制），未知 scheme 由浏览器层
  自然报错。交付卡有「右侧查看」显式按钮（任意文件进右栏），工具栏在
  file:// 页面提供「用系统应用打开 / 在文件夹中定位」配套；远程会话流式
  预览（/api/browser/view）按扩展名给真实 Content-Type。
  **站点权限预授权**：`browser_open` 的 `grants` 参数（每项
  `权限名@origin`，如 `automatic_downloads@https://mail.qq.com`——预授权
  站点自动下载多个文件，浏览器不再弹「想要下载多个文件」确认）写入该会话
  实例 profile 的 Preferences（`profile.content_settings.exceptions`，
  setting=1 允许，仅对列出站点生效、其余保持默认询问）。Preferences 只在
  浏览器启动时读取：`ApplyGrants` 会先停在跑的实例（等退出覆写结束）再
  注入，随后 Navigate 重开生效（登录态在 profile 无损）；右栏 WebView2
  面板同款（Chromium 同构 profile），Electron 桥面板不支持（静默跳过）。
  具体授什么权由各技能的 SKILL.md 写明，权限名是 Chromium content settings
  类型名（网上可查）。接口：`POST
  /api/browser/{panel-rect,navigate,viewport,open-view,close}`、`GET
  /api/browser/{state,view}`、`GET /api/md/{sid}/{path...}`（右栏 md 渲染页，
  见下条），body/query 带 session 字段路由到该会话的实例；
 事件 `browser_nav`（工具栏地址/标题）、
  `browser_resize`（AI 的视口宽度请求，前端让对话区让位调宽面板）。右栏收起
  只是隐藏面板（AI 继续操作）；右栏浏览器视图的 ✕ 是**真关闭**（POST /api/browser/close，userstops.go：销毁该会话的内嵌实例、打「用户关闭」标记（按会话记账）、向该会话投 report 注记；activeHost 据此把之后的报错说成用户主动关闭）；顶栏开合按钮仍只是收起。工具条的「在系统浏览器打开」把当前网址跳到
  用户日常浏览器（跳转后不受 AI 控制）。
- **聊天链接点开（人机共看）**：对话正文里的 markdown 链接与裸 http(s) 网址
  渲染为可点链接——桌面版左键点击一律在右栏浏览器面板打开（Ctrl/中键走
  浏览器默认新标签）；网页版没有原生面板，一律新标签页打开。这是用户操作，
  不经模型权限门控；打开时内嵌形态自动成为工具作用目标（active）。**不注入
  任何对话回报**（用户口径：打开了什么是个人的事）——模型需要看画面时用户
  会主动说，模型用 `panel_screenshot` 截右栏（见下）。本地 html 交付文件转
  `file://`（同目录相对资源可用），SSH 远程会话经 `/api/browser/view` 流式
  渲染（相对资源不加载）。**Markdown 的右栏渲染只在显式入口**：交付卡
  「右侧查看」、右键「预览（右栏）」传 md 时进右栏渲染页（`/api/md/{sid}/{path...}`，
  见上条）；默认「打开」仍是系统程序——md 交给系统关联的自带 MD 查看器
  （`md-viewer/MD查看器.exe`，独立窗口），不进右栏。两者是两个动作，别合并。
- **文件查看 = 系统默认应用**：右栏「文件」查看页（`fileview.go`，当年原生
  浏览器面板修不好时期的替代品，只支持文本、观感差）已整体移除。交付卡与
  聊天文件链接的「打开」两路路由：html/htm 与网址进右栏「浏览器」；其余
  （含 md——系统关联的自带 MD 查看器以独立窗口打开）一律 `ShellExecute` 系统
  默认应用（`/api/sessions/{id}/open-path` 与
  `/delivery/open`；用户手势直通——工作区内外都不校验、无确认弹窗，这些
  路由不在 call_api 目录里 AI 调不到，SSH 远程工作区无本机路径给直白报错）。
- **右栏 md 渲染页**（`mdview.go`，`GET /api/md/{sid}/{path...}`）：**显式右栏
  入口**（「右侧查看」/右键「预览（右栏）」）里的 md 在右栏渲染打开，用的就是
  自带 MD 查看器那套网页（`md-viewer/viewer-template.html`
  经 `go:embed` 进 exe，占位符 `__TITLE__/__B64__/__META__/__KEEP_RUNNING__`
  灌数据；不另写渲染器）。地址用**路径形态**而非查询参数：md 里的相对图片/
  附件按 URL 语义落到同一路由（页在 `…/docs/a.md`，`./img/x.png` →
  `…/docs/img/x.png`），不裂图、不改写正文。同一路由两种回法——`.md` 回渲染
  网页，其余按扩展名给真实 Content-Type；超过 4MB 的 md 退化成 text/plain
  原文（几 MB 铺成 DOM 会把右栏卡死）。渲染页带 CSP（`connect-src 'none'`
  等）：md 里的内联 HTML/脚本照旧执行（与 MD 查看器同策略），但拿不到产品
  同源 API（响应头 + `<meta>` 两份，后者覆盖手机外壳的 srcdoc 那条路）。
  前端分流：桌面/Electron 走原生面板导航（`bpwNav`）；网页与手机形态走右栏
  同源 `<iframe>`（`app-right.js` 的 `bpwShowFrame`，远程形态经隧道取 HTML 写
  `srcdoc`），窄屏按模板自带 500px 断点单列可读、宽表格与代码块各自横向滚。
- **`panel_screenshot`**（无能力门控，仅桌面版）：对右侧栏截图（GDI 截屏
  像素，浏览器页/任何右栏视图通用）。实现：注入主界面探针 JS 读
  `#rpanel` 矩形 POST `/api/shell/panel-view` 回传，Go 侧客户区原点+CSS×dpr
  换算屏幕坐标后 BitBlt 截图入附件；ExecuteScript 必须 UI 线程执行
  （runDesktop 里经 `w.Dispatch` 包了一层）。右栏收起时报「没有打开内容」。
- **电脑操作**（能力开关 `computer`）：`computer_screenshot / click /
  type / key / scroll / windows` 六件工具，win32 SendInput + GDI
  DIB Section 直调，无 cgo；点击坐标以截屏图片像素为准。

## 组件管理（原生外围扩展，热开关）

与插件（DSH 适配）相互独立的轻量扩展体系，全部为本栈原生态编写：

**组件三形态与开发方针（硬规矩，详见《组件/README.md》）**：①纯浏览器半边
（默认，新增组件一律优先）；②宿主半边·存量套皮（todo/git 面板/危险命令护栏/
企业技能包市场/小程序平台——后端是深度耦合主程序状态的内置功能，认定不可
拆，仅维护不再新增；skill-market 与 sites-platform 是开源前从本体抽出的，
后端 /api/market 与 /sites 代理保留、入口随组件开关）；③宿主
半边·自包含 worker（目标形态，voice 是样板：worker+模型自包含于组件目录、
跑共享便携运行时，经主程序的**声明式桥**（componentbridge.go，已实现）运行：
manifest 顶层 `workers` 数组声明命令与端点（挂 /api/c/<组件id>/…），主程序按
声明懒拉起子进程并哑转发（json-lines 一行一答；call 与 ws 两型）——组件后端为
脚本或预编译产物、市场直装免编译免主程序更新。worker 声明里还可加 `hooks`
订阅主程序钩子事件（`prompt.submit` 拦截型／`run.end` 通知型）：声明了钩子的
worker 的 stdin/stdout 升级为带 id 的帧协议（req/event/decision/host/hostres），
能收主程序事件、回决定（拒绝/改写/附加上下文）并反向调用主程序 API（仅
/api/ 前缀，权限同前端）；拦截型超时/组件故障一律按放行（fail-open）。前端
对应物是可等待钩子 `JX.hook('send', fn)`：send 在组装正文前 await（听写组件
靠它先落定尾段再发送），与 `JX.on` 的同步通知相区分。目的是开源后社区开发者
各归各开发（避免人人改主程序导致冲突与组件不通用）、终端用户免编译装上即用。
声明规范与协议详见《组件/README.md》。

- **`组件/<名字>/`**：`manifest.json`（id/名称/版本/描述/资产）+ `client.js`
  （`client.css` 可选）——纯浏览器半边，没有宿主代码、没有转译。
- **两档归属，一个组件桥（用户定调，2026-11-08）**：组件按来源分内置与用户两
  档，**两条路走同一个注册表、同一个组件桥（worker/钩子/端点/资产/热开关全
  共用），差别只在目录与可删除性**：
  - **内置组件**：`组件/<id>/`（随包分发、**不可删除只可开关**、随程序静默更新
    的组件分块热更新）。市场不得覆盖同名内置组件（install 直接拒），市场卡片
    对它们只显示「内置（随程序更新）」不给装/卸按钮。
  - **用户组件**：`组件/user/<id>/`（市场或发行渠道单个安装、**可开关可删除**、
    **不进打包、不上 git、不参与热更新**——版本与去留归用户）。与技能同一套
    约定（`技能/user/`）：打包脚本 robocopy 排除该目录、升级清理清单跳过它、
    `.gitignore` 记一条、静默更新拒绝把 `user` 当组件块发布。
  - 归属经 `ComponentStatus.BuiltIn` 下发；设置页一级页叫「组件」，内部按
    「内置组件 / 用户组件」两区（与「技能」页同款）；删除走
    `POST /api/components/delete`（对内置返回 400 指路关开关）。同名 id 时内置
    优先（用户那份被忽略并告警）。
- **发现与开关**：启动扫一次 `组件/*/` 与 `组件/user/*/` 的 `manifest.json`
  （损坏即跳过告警），之后**平时完全不自动扫**——清单接口是纯读；重扫只挂在
  「打开设置页 → 组件页」这一次（`GET /api/components?rescan=1`，前端开机渲染
  清单时带；不需要刷新按钮）。所以手工放进/删掉的组件目录**打开设置页即见，
  不必重启服务**（桌面版单实例常驻，重开窗口不等于重启服务，这正是 user-memory
  曾经「重启后仍不见」的原因），平时零扫描开销。开关状态存
  `data/components.json`（缺省启用，删目录即清记录）。接口 `GET/POST /api/components`（带 `?s=`/`session` 走会话
  级覆盖）、`POST /api/components/delete`（只删用户组件）、资产
  `GET /api/components/{id}/{file}`（仅启用时提供，按组件自身目录解析）。
  这两个接口与 `/api/plugins` 都登记进 call_api 可调目录（`apiMeta`），长期助理
  能直接查询与开关组件/插件（含会话级覆盖）。资产挂 `/api/` 下且
  `web/cx.js` 统一用 fetch 拿文本注入（不插相对 src 标签）——远程中继模式下
  `/api/` 前缀被 remote-boot 的 fetch 劫持自动走隧道，组件在远程同样可用。
- **按对话的组件/插件开关（会话级覆盖，2026-11-07）**：每个对话可以有自己的
  生效集，**优先于全局开关、全局开关不动**——别的对话关的这里可以开、开的
  这里可以关。入口在对话输入区加号菜单「本对话的组件与插件」：设置页同款
  浮窗（`#cxsession`，复用 settings-card/set-nav/set-pane 全套类），一级页只有
  「组件」「插件」，组件注册的独立设置页（如语音）排在它们之后。后端
  `sessioncx.go`：覆盖存会话附属数据（`sessdata:cx-overrides:<会话id>`，
  随会话删除清理；只记显式覆盖，拨回与全局一致即删记录）；`GET/POST
  /api/components、/api/plugins` 带 `session`（或查询参数 `?s=`）即会话视图/会话
  写入，响应带 `globalEnabled/override/sessionCustom`。前端 `cx.js` 记当前会话
  （`session:open` → `CX.setSession`，回首页置空），清单与资产 URL 带 `?s=`，
  `CX.toggle(id,on,session)` 与全局热开关同一条注入/卸载路径，切会话经
  `refresh()` 对账热装配。worker 进程与语音端点的存活口径是「全局开着或还有
  任何会话显式要它」（`cxoComponentLive`，请求不带会话——会话关掉的对话里组件
  UI 已热卸载，不会来请求）；CC 插件的斜杠命令按会话生效集过滤
  （`/api/cc-plugins/commands?s=` 与展开路径）。**模型工具目录仍按全局开关装配**
  （toolsFor 不认会话；git_sync/todo 工具在会话里关掉组件后仍在目录里，调用时
  按全局口径执行——完整会话化待 toolsFor 接收会话时再收口）。
- **会话级组件配置（合并层，2026-11-08 定稿）**：同一个浮窗里，组件设置也能按
  对话分开——「统一会话视角配置合并层」：覆盖从「组件开关（布尔）」扩成「任意
  配置项 KV」，**在组件自己的存储读取通道上按「会话覆盖 > 全局值」合并**喂给
  组件（`web/cx.js` 拦 `localStorage['cx:<组件id>:*']` 三个动词，组件零改动，
  读到的永远是该会话视角的值）；**不搞 manifest 声明**（不注入对话的设置会话化
  也无感知，机制自动覆盖全部组件的前端偏好）。全局值仍在组件自己那儿（独立
  不动），覆盖统一存会话附属数据（`SessionOverrides.Cfg`，随会话删除清理）。
  - 交互（定稿）：浮窗每行**两个开关**——上＝本对话启用/停用该组件（行级启停，
    原有）；下＝该组件设置「会话独立 / 跟随全局」。浮窗底部**总开关**「本对话
    使用独立的组件设置」＝一键切全场：开＝全场进入会话独立（缺省每行独立）；
    **关＝整行完全回归全局**（配置覆盖 `Cfg`/`CfgFollow` 与行级启停覆盖
    `Components` 一起清空）。恢复语义：行级切回「跟随全局」＝删该组件的配置
    记录（`Cfg[id]` 消失）。全局设置页开着时读取通道走全局（`_cfgBypassNow`），
    那里的改动就是全局值，两套视角不串。
  - 接口：`GET /api/components/cfg?s=` 读视图（总开关 + 每组件覆盖），
    `POST /api/components/cfg`（写/删一条键，`value` 缺省＝删），
    `POST /api/components/cfg/mode`（行级 独立/跟随）、
    `POST /api/components/cfg/scope`（总开关）；`GET /api/components?s=` 的响应
    顺带带 `cfg` 字段，`web/cx.js` 装配前先就位（组件 init 第一口读即会话视角），
    切会话时把已装组件全卸一遍按新会话配置重装。四条接口都登记进 call_api。
  - 覆盖范围与边界：合并层管的是**组件前端偏好**（localStorage 命名空间
    `cx:<id>:*`，语音的音色/听写模型等就是这类，新组件照这个命名空间存即自动
    支持）；组件存在服务端的**数据**（用户记忆 md、待办清单、市场服务器地址这类
    全局配置/资料）不随会话——那是数据与安装级配置，不是「组件设置」。
- **内置组件与用户组件的本质区别**（2026-11-06 客户事故的规范沉淀 + 2026-11-08
  两档目录约定）：内置组件（`组件/<id>/`）随热更新**必自动装上与更新**——清单
  带块即补装/升级（update_blocks.go planComponentBlocks，保证在场，不受「本机
  未装跳过」类保守规则拦截），用户只通过开关控制启用与否（开关可关、安装更新
  必须自动）；用户组件（`组件/user/<id>/`）才由用户自行安装、自行删除，**不进
  热更新**（清单里即使有同名块也跳过，不覆盖用户手上那版）。内置功能组件化后
  尤其如此：入口靠组件注入，组件不到场入口就消失、连市场入口同失（想再装得先
  有市场入口，死锁）。想彻底不要某内置组件用开关关闭（记账保持关、补装不翻
  开关）；手动删目录只在两次组件发版之间有效（发新版清单带块即装回，与安装包
  升级同款行为）。
- **热开关（核心特性）**：内嵌运行时 `web/cx.js` 随页面加载，负责发现注入与
  运行期开关。组件脚本调用 `CX.register({id, init, dispose})` 注册：
  `init()` 建立界面与状态，`dispose()` 完整撤销（删节点、卸样式、清监听）。
  设置页切换开关时**前端即时加载/卸载，不需要刷新页面、不需要重启服务**；
  组件未提供 `dispose` 时退化为提示刷新。
- **组件设置区**：组件在 `init` 里调 `CX.registerSettings({id, title, mount})` 把自己的
  设置交给运行时；`CX.unregisterSettings(id)` 可撤。注册/注销/热开关都会派发
  `window` 事件 `cx:change`，设置页收到后重挂。设置样式与偏好由组件自理
  （组件 CSS + `localStorage`）。呈现形态两种（`app-settings.js` 渲染，只渲染当前
  激活的组件）：①缺省＝「组件」页清单下方的设置段；②注册时带
  `pane: {label, icon}` ＝**独立设置页**——设置一级菜单出现专属入口（插在
  「组件」之后）+ 专属分区页（`set-pane-component-<id>`，动态创建于
  `.set-panes`），导航状态机按 data-pane 寻址与静态页同一条路径；组件停用时
  导航项与页面一并撤掉（正停在该页时退回常规页）。`voice` 组件用的是独立页
  （设置 → 语音）。**全局保存接入（2026-11-08）**：注册项可带
  `save: async () => boolean`——设置页右下角「保存」（`saveSettings`）经
  `CX.saveSettingsSections()` 逐个调激活组件的 save，任一返回 false/抛错即
  短路、设置窗不关（就地修正）；组件设置区**不要再放自己的保存按钮**，改动
  暂存在 mount 出的 DOM 草稿里、随全局保存统一落库（首个使用者：user-memory
  的「AI 记忆与拟人化」页）。注意「本对话的组件与插件」浮窗（app-cxsession.js）
  挂同一份 mount 但没有全局保存——那份挂载的草稿不会被收集，改动停留无害。
- **界面插槽＝标准定位格式（组件只「申请格子」，排版归插槽）**：界面上一块有条目
  的方寸（左侧栏底部入口格等）在宿主里是一个**插槽**，组件只交「图标 + 名字 +
  序号 + 点击」，不拼条目 DOM、不写条目布局 CSS——骨架、排序、以及同一份条目在
  **电脑与手机两套形态**下的排版全归插槽（`web/app-core.js` 的 `JX.slot` +
  `web/style.css` 的插槽段），组件两套形态都不用管。
  - 接口：`JX.slot('<插槽名>').add({ key, label, icon, order, title, onClick, id?, owner? })`
    → `{ el, setActive(on), remove() }`；同 key 重挂先撤旧的（热开关/热重载安全）。
    自检：控制台 `JX.slots()` 打印每个插槽里的条目（key / 序号 / 谁注册的 / 是否选中）。
  - 序号方向：**order 小的在最下面（紧贴设置行），大的往上叠**；同序号先注册的更靠下。
  - 现有插槽 `sidebar.foot`（容器 `#foot-apps`，`.foot-grid` 为 `display:contents`）：
    电脑＝竖排整行；手机（≤499.98px）＝三列均分网格 `repeat(3, minmax(0,1fr))`，
    3 个一行、4~6 个自动两行，每格高度固定、标签最多两行、超出裁掉（`minmax(0,…)`
    不能写回 `1fr`：1fr 的下限是内容 min-content，长名会把第一列撑开、后两列被挤成
    竖排——2026-11-06 用户报的现象）。内置「回收站」是第一位使用者（order 1，
    `app-left.js` 注册），组件从 2 起往上叠（skill-market 2、sites-platform 3）。
  - 加新插槽＝在 `app-core.js` 的 `SLOT_HOSTS` 登记容器与条目类名 + 在 `style.css`
    写它的两套形态（右栏页签 `rpRegister` 是同一模式的既有实现，见 `app-right.js`）。
- **组件往界面加东西＝独立挂载／走插槽，禁止锚定其他按钮**：插入位置一律用「向
  插槽注册」或「往自己的挂载容器 append（或 prepend）」表达（侧栏入口 `.actions`
  末尾、底部插槽 `JX.slot('sidebar.foot')` 等），不得按「插在某个按钮旁边」
  定位——锚定别人＝把自己的排序耦合进别人的 DOM，别人换位或下线时组件按钮
  被动搬家、掉进兜底路径（todo-scheduler 曾锚定数字助理入口，2026-11-05 入口
  换位即暴露，已改 `.actions` 末尾独立挂载；市场/小程序平台组件 2026-11-06 起
  改走底部插槽，不再各自拼按钮和排序）。
- **宿主半边可以落在主程序里**：组件本身是纯浏览器半边，但它掌管的功能若需要
  后端（持久化、定时、接口、模型工具），就写在主程序对应文件里、按
  `Components.IsEnabled(<id>)` 自查开关——组件关掉时那部分功能一并停。
- 组件随源码入库（`组件/` 目录全部跟踪），**打包时必须随安装包分发**（见
  「桌面版与安装包」一节的打包清单）。曾作为最小组件模板的 `input-counter`
  （字数统计）已删除，新建组件照「manifest.json + client.js（client.css 可选）」
  的最小结构手写即可。界面强调色（主题色）原为示例组件，现已并入主程序
  `web/app-settings.js`（常规 → 外观 → 主题色）。
- **组件能力分发＝基础工具 `component_call`（方案 A，2026-11-09 用户定调）**：
  **工具定义只装基础工具，组件的动态能力不进工具目录**——否则每加一个组件
  能力就要改一次工具定义，请求前缀抖动、缓存与多模式目录都得跟着维护。
  现在走两半：
  - **声明**：组件 `manifest.json` 顶层 `capabilities` 数组（一条一个能力：
    `name` 能力名、`description` 一句中文说明、`parameters` 参数 JSON Schema、
    `modes` 可用模式白名单〔空＝全部模式〕、`writes` 是否写入型〔计划模式下
    暂停〕）。**声明是白名单**：没声明的能力宿主不给调。
  - **实现**：宿主注册表——组件自己的 .go 文件在 `init()` 里
    `registerComponentCapability(组件id, 能力名, 函数)`（样板见 usermemory.go
    的 `memory_query` / `memory_update`）。声明了但没有实现的照样不可调（报错
    指路）。分发与门控都在 `componentcall.go`。
  - **调用**：模型调 `component_call(component, capability, args)`——它作为
    **基础工具全模式注册**（assistant/aux 也有，能不能调由声明的 modes 决定），
    参数说明不在工具定义里，而在**环境快照的「组件能力」块**（`envPrompt` →
    `componentCapabilityBlock`：逐个启用组件列出当前模式下可调的能力、参数
    要点、是否写入型）。组成链路：组件存在 → 本会话启用（会话级覆盖优先）→
    能力已声明 → 本模式可用 → 宿主已实现 → 执行。
  - **开关只影响环境提示词块，工具定义恒定**：组件开关在快照尾部以差分送达
    （前缀缓存不动）；`TestComponentCallToolCatalogStable` 钉住「开关前后
    `toolsFor` 名称与 schema 逐字一致」。
  - 新增组件能力的三步：①manifest `capabilities` 加一条声明 → ②宿主 `init()`
    登记实现 → ③确认「组件能力」块出现。**不改 `toolsFor`、不改任何工具定义**。
    格式与扩展方式详见《组件/README.md》。
- **现成组件：`todo-scheduler`（待办与定时任务——侧栏入口 + 持久清单 + 定时调度，
  宿主半边在 `todo.go`，关掉后入口、`/api/todos` 接口与调度一起停）、
  `git-panel`（Git 面板——右栏「Git」页签：同屏双区（默认上下堆叠、可切左右分栏、
  分隔条可拖）——更改区为 VSCode 式清单（暂存/取消暂存/提交/AI 摘要/逐行 diff；已暂存与未暂存的文件可右键「移出跟踪」变未跟踪），
  图谱区为分支泳道图（全部分支拓扑、合并分叉、引用徽标、HEAD 高亮、提交详情），
  顶栏可选仓库与切换本地分支；提交栏「同步」单按钮（随 ahead/behind 切换 ↓拉取/↑推送：fetch、快进或无冲突自动合并、push 当前分支；无上游/分叉冲突/被拒等一律原样退回并弹卡建议回对话新建工作任务），旁有「▾」下拉点开「使用教程」清单（列 Git 还能做的事，引导跟 AI 沟通操作）。宿主半边在 `gitpanel.go`，写边界只有暂存/取消暂存/
  提交/移出跟踪/切换本地分支/同步（上述安全范围），不做 reset/revert/discard/stash/rebase。
  组件还提供**模型工具 `git_sync`**（`gittools.go`，与面板共用 gitSync 执行缝）：
  沙箱内 git 远程操作全灭（MSYS 传输进程共享内存被拒 + schannel 拿不到 TLS
  凭据句柄，2026-10 实测，见《Git远程同步工具-改动说明.md》），本工具是
  修改工作区档下模型唯一的远程通道——宿主进程全令牌 fetch/pull/push/sync，
  报错原样回模型；全电脑只读档调用被拦；完全访问/已开「允许工作区外执行命令」时
  描述引导模型直接用命令行。工具注册随组件开关（todoTools 同款组件门控），
  环境快照公告一行「本地 git 直接 run_command、远程用 git_sync」）、
  `dangerous-cmd-guard`（危险命令硬拦截，只有 manifest、没有浏览器半边）、
  `voice`（语音（朗读与听写），**零配置**——朗读走后端、听写走浏览器，都不需要
  任何密钥。**朗读（TTS）**：宿主半边在 `voiceapi.go`，直连微软 Edge「大声朗读」
  通道（speech.platform.bing.com 的 readaloud，无密钥免配置，与 Edge/WebView2
  内置朗读同一套服务；WebSocket 协议逐项对齐聊天服务 chat-server 的 Node 实现
  ——Sec-MS-GEC 令牌/Edge 扩展 Origin/随机 muid；前端 POST `/api/voice/tts`
  → mp3 流回 blob 播放，音色清单 `/api/voice/voices` 同源 322 个神经音色、中文
  排前）。朗读不再用浏览器 speechSynthesis——部分手机浏览器没有它，旧版远程
  手机端连朗读设置区都不渲染；现在设置区恒渲染、任何浏览器行为一致。**听写
  （STT）两档，默认「云端」**：云端＝浏览器自带的 SpeechRecognition（WebView2/
  Edge/Chrome 内置的微软识别接口，即本组件一直以来的实现：灰字暂定层/断线退避重连/
  倒计时徽标、按钮三态，连接失败提示建议换 Edge）；本地＝`voiceasr.go` 通道的离线模型
  （说完一句停顿约 3 秒精修落黑字，本机走 ws 流式、远程 https 回退整段 POST），**需
  `技能/_runtime` 带 sherpa-onnx 且 `组件/voice/models`（xasr 整段 + xasr-streaming 流式）
  齐备**，缺任一即弹「识别通道错误」，前端引导切回云端。**有个坑**：worker 的脚本路径必须
  是绝对路径——`voiceDir()` 若只返回相对路径，调用处又会拿它当子进程的工作目录，Python 便
  按新工作目录把 `组件/voice/asr_worker.py` 再解析一遍，拼成
  `组件/voice/组件/voice/asr_worker.py` 秒退，引擎只看到「worker 无应答（已退出）」。
  环境没有浏览器识别能力时（部分远程手机端浏览器、Electron
  壳）自动落到本地档。发行版默认「云端」——装完即用，不依赖模型文件。
  **听写提示已精简**：识别期间不再报「已出 N 句（继续说，或点麦克风结束）」这类
  计数提示（用户判为噪声且常滞后——消息都发走了还挂着）；按停后到尾段精修回包
  这一秒（原先无任何反馈）按钮转圈 +「正在识别…」，收尾时提示一并收掉。
  **按停要等在途识别**：无 WS／通道挂掉时尾部那句走整段 POST，收尾必须等它回包
  （上限 8 秒）再判「有没有识别到」——否则会出现「刚按停就报未识别到语音内容、
  一秒后文字才落进输入框」的误报（组件 voice/client.js 的 pendingPost + settleThen）。**语音快捷键（主副双键，
  永久并存）**：默认鼠标侧键（主，前后两枚都算）与 Alt 键（副）**同时可触发**
  ——有侧键的用侧键、没有的自然用 Alt；独按语义：按住 Alt 期间按了别的键不
  触发，点按在独按松开时触发；设置页可录制自定义键盘单键/组合键、鼠标左右
  键同按（不允许单独的左键/右键）；设置页**主/副两个键帽框独立录制**（默认
  主=侧键、副=Alt；旧单快捷键偏好自动迁移），「清空」＝停用全部、「恢复默认」
  ＝回到侧键+Alt；点按切换与长按输入两种模式；仅窗口聚焦时
  生效（失焦自动松手结束长按）；主输入框占位灰字由基座 `sendKeyHint` 追加
  组件交出的
  `window.JXVoiceHint` 片段（子输入框组件自己拼）。**发送前自动停听写（可等待
  钩子）**：组件经 `JX.hook('send', fn)` 注册发送前钩子，`send()`（app-chat.js
  的 prepareSend）在组装消息正文前 `await JX.awaitHooks('send', …)`——听写组件
  走与用户按停相同的收尾路径（定稿尾段 + 等在途识别清零，上限 8 秒），落定后
  才放行发送，随说的内容随本条消息一起发出；钩子异常隔离、总超时 10 秒放行
  （组件内部上限 8 秒小于它，不会被提前放行），组件卡死不会卡死发送；
  回车/Ctrl+回车/发送按钮/子对话全覆盖。**远程手机端**
  （`window.JX_REMOTE` 且视口 <500）自动隐藏听写按钮（resize 跨阈值即时
  显隐），快捷键触发给提示不启动。设置＝**独立设置页「语音」**（设置 →
  语音：语速默认 1.2、Edge 音色、试听、听写时长上限、快捷键与触发方式）；
  偏好存 `localStorage` 的 `cx:voice:` 命名空间。关掉即热卸载：语音端点 404、
  按钮隐藏、朗读按钮撤掉、快捷键引擎与占位提示一并撤掉、设置页连导航项
  一起消失）、`session-title`（AI 自动会话标题——C 形态第二个实例、纯
  worker 组件（无 call 端点，只订阅 `run.end` 通知型钩子）：worker 反向调
  主程序 API 干活，模型请求走 `POST /api/llm/ask`（llmapi.go——组件机制的
  通用「独立单轮模型调用」服务，经桥的反向调用通道对所有组件开放，git 摘要
  同款模式，不进会话上下文、worker 不持有密钥）。节奏：第 1 轮结束命名、之后
  每满 3 轮修订一次（按非插话用户消息条数 n，n%3==1 才动手——插话消息在落库
  时带 `origin=interject`（handleInterject 打标、landUserSide 落库），轻量
  消息端点默认排除，既不计轮次也不用于改标题）；每次只送「当前标题＋最近
  3 条用户消息」（各截 400 字——标题是累积状态载体，最初主题已凝在核心段），
  增量演化规则写进组件里的提示词（「核心：描述」结构：
  核心段两段式「最近主题|最初主题」近期在前（侧栏窄，靠前的字先被看到）、
  半角竖线分隔、8-12 字，最初主题（竖线后）整段
  不变、主题延续保留原核心、新主题只换前段；冒号后一句话完整描述随进展
  重写、长度不设上限，由 PATCH 的 60 字截断兜底）。用户手动改名（PATCH title）即打钉（session
  data `title.pinned`），`autoTitle` 通道对该会话 409 停改，`autoTitleUnpin`
  解钉——钉住标记随会话列表/详情接口下发（`titlePinned`，会话右键菜单的
  「恢复自动命名」按它显隐，组件关掉即不出）；改名落库广播 `title` 事件，
  前端侧栏/顶栏实时跟进（组件改名常在流结束数秒后晚到，不广播的话当面永远
  看不见）；轻量消息读取走
  `GET /api/sessions/{id}/messages`）、`user-memory`（**AI 记忆与拟人化**——
  用户称呼、身份与喜好、AI 助理名字与身份的长期记忆 + AI 对话气泡颜色。
  全部内容存一个 md 文档 `data/user-memory.md`（组件自管，可手工编辑，
  **完整保存不截断**）；宿主半边在 `usermemory.go`。**注入走环境提示词**
  （envPrompt 的 userMemoryEnvBlock：组件开启且任一字段有内容才出现，变化经
  快照差分送达全部会话，关闭即整块消失——不进系统提示词、不进工具定义；
  注入侧对超长字段截断到 1000 字加尾注，防手工编辑的超长内容撑爆快照）。
  **权限分级（方案 A 后：经 `component_call` 调两条能力，不占工具目录）**：
  普通任务对话可调 `memory_query`（查询全部字段）与 `memory_update`（只更新
  「用户身份与喜好」记忆段，其余字段只读——称呼与 AI 名字/身份是用户的自我
  设定）；两条能力在组件 manifest 的 `capabilities` 里声明（`modes` ＝五个
  执行模式，aux/assistant 不含），实现在 usermemory.go 的
  `userMemoryCapabilityQuery/Update` 经 `registerComponentCapability` 登记；
  长期助理（assistant 模式）的环境快照里告知**整个记忆文件可读可改**——call_api 调
  `GET/POST /api/user-memory`（按字段保存，未给出的字段保持不变；文件在助手
  数据目录内、受保护路径拦文件工具，接口是唯一通道）。**长度约束在写入入口**
  （1000 字/字段，rune 计）：设置页前端超长红字告知、按截断版提交（浏览器
  保留完整草稿，不回填）；模型侧（`memory_update` 与 call_api POST）超长整单报错、
  不写入。设置页独立选项卡（设置 → AI 记忆与拟人化）由组件 client.js 注册；
  气泡颜色是纯前端设置：`--ai-bubble` CSS 变量注入 `<html>`
  （`web/style.css` 的 `.msg.user.ai` 气泡引用，缺省回退主题灰），保存后即时
  生效、组件卸载即还原。组件发现：平时不扫，打开 设置 → 组件 时前端带
  `rescan=1` 重扫一次，运行中放进/删掉组件目录打开设置页即见——不必重启服务；
  之后开关即点即生效。**会话级记忆合并（2026-11-08）**：环境注入与模型侧
  读写按**会话视角**——该会话开关覆盖关闭时整块不注入；「会话独立」（浮窗
  总开关 CfgScope 开且未标跟随，见 sessioncx.go）时四字段+颜色逐项
  「会话覆盖 > 全局值」（覆盖键=API 字段名，存会话附属数据），注入块与能力
  回执标注「本对话独立记忆」；`GET/POST /api/user-memory` 带 `?s=<会话id>`
  ＝会话读写（POST 在独立会话写覆盖、全局文档不动，与全局同值归一删记录），
  不带 s＝全局（设置浮窗、经理 call_api）；`memory_query/memory_update` 按
  所属会话的视角查写（component_call 分发时按会话开关核对）。envPrompt 每轮
  现算，覆盖变化经环境快照差分自然送达该会话）。

## 插件与插件兼容层

**统一架构（pluginlayers.go）**：目标是兼容各种插件体系（DSH、Claude
Code、…），各家后端逻辑一致。主体只认「插件兼容层」这一层抽象——启动时
`discoverPluginLayers` 发现全部兼容层（层在 init() 里 `registerPluginLayer`
自注册），此后只按 `PluginLayer` 契约调用（列清单 / 总开关），不认识任何
具体插件格式；兼容层自己去发现自己生态的插件。统一入口
`GET/POST /api/plugins`：GET 返回 `{layers, plugins}`（每条插件带 `layer`
来源标记），POST 总开关带 `{layer, id, enabled}`（layer 缺省=按 id 全层
唯一匹配）。设置页只有**一个「插件」页**：按体系分组展示，安装区在页内
（当前仅 Claude 层支持安装）。新增一个体系 = 写一个层实现并注册，主体与
设置页零改动（前端可按层 id 注册富行渲染器，缺省通用行）。

对 DSH 插件体系的手写兼容层（组件/开关机制从简：一体编译 + 扫描 + 开关）：

- **`插件/`**：DSH 插件原件，原样存放、不改一字、不参与编译——功能来源与对照标准。
- **`插件兼容层/<插件名>/`**：每个插件一份手写转译适配——`manifest.json`（id/名称/
  版本/描述/浏览器资产）+ `client.js` / `client.css`（浏览器半边）+ `README.md`
  （能力映射表：转译了什么、有意不做什么）。Go 半边在根目录 `adapter_<名>.go`
  （一体编译，按名与目录配对：可挂自己的 HTTP 端点、给系统提示词追加段）。
- **扫描与开关**：启动扫描 `插件兼容层/*/manifest.json`（损坏即跳过告警，不阻断）；
  `插件/` 里有原件但无适配器的在统一「插件」页灰显为「未适配」。开关状态存
  `data/plugins.json`（扁平映射，缺省启用），经统一 `/api/plugins` 落盘，
  **即时生效**：浏览器半边资产注入
  与 Go 半边端点每请求检查，界面部分刷新页面后生效，无需重启。
- **前端接入**：页面注入的插件脚本经 `JX` 接口接入原生界面——
  `JX.registerMessageAction`（消息操作行按钮）、`JX.registerSettingsSection`
  （设置分区）、`JX.addComposerControl`（输入区控件）、`JX.on/emit`（事件总线：
  send / message:done / session:open）；`JX.unregisterMessageAction` / `JX.off`
  是对应的注销口（组件/插件热卸载时把钩子摘干净，不留悬挂按钮）。
- 首个适配：**dsh-speech**（消息朗读 / 浏览器听写 / 转写润色），映射表见
  `插件兼容层/dsh-speech/README.md`。为下一个插件写适配 = 原件拷进 `插件/`，
  在 `插件兼容层/` 建同名目录写资产与说明，必要时加一个 `adapter_<名>.go`。

## Claude Code 插件兼容层（ccplugin*.go）

对 Anthropic Claude Code 插件格式（`.claude-plugin/plugin.json` + skills /
commands / agents / hooks / MCP / mods）的通用兼容层：一次实现吃全部该格式
插件，不是逐个手写适配。它是 pluginlayers.go 统一注册表里的一个层实现
（与 DSH 手写适配层平级：清单与总开关走统一 `/api/plugins`，组件级开关/
配置/装卸走本层自有 `/api/cc-plugins/*`）。设置页统一「插件」页的
Claude 分区管理；输入框 `/` 触发插件命令弹层。

- **目录布局**（全在受保护的数据区内）：`data/claude-plugins/cache/<市场>/<插件>/<版本>/`
  插件本体（`${CLAUDE_PLUGIN_ROOT}`）、`data/claude-plugins/data/<插件>@<市场>/`
  运行时数据与 mod store（`${CLAUDE_PLUGIN_DATA}`）、`marketplaces/<市场id>/`
  市场克隆、`plugins.json` 安装记录与开关。安装四来源：本地目录/zip、git/zip
  URL、市场条目三形态（`./相对路径` | `git-subdir` | `url`）。
- **组件映射**：skills → `read_skill` 按需读取 + 环境快照清单（id 带命名空间
  `插件名:技能名`）；commands → 斜杠命令注册表，handleChat 落库前展开
  （`$ARGUMENTS`/`$1..$9`/`\$` 转义）；agents → 动态子对话专家（experts.go
  三处合并，名称即 dispatch_sub 参数）；hooks（settings 形态）→ 事件桥 +
  命令/http 执行器（stdin JSON、exit 2 阻断、stdout JSON decision 协议：
  permissionDecision/updatedInput/updatedToolOutput/additionalContext）；
  `.mcp.json` → 纯 Go MCP 客户端（mcpclient.go，与独立连接器共用；stdio
  行帧 JSON-RPC 与 streamable HTTP），工具名 `mcp__<插件>_<服务器>__<工具>`
  保持 Claude 原样（插件 hooks 的 matcher 可直接匹配）。
- **mods（`{"modules":["./register.js"]}` 形态）**：goja 纯 Go 内嵌 JS 运行时
  （四平台一致含安卓）。ESM→CJS 薄转换（import/export 行级改写，mod 的
  import 形态受 Claude 规范约束故可行）；`register(on)` 装载时调用一次；
  事件：session.start / prompt.submit / tool.call / command.run /
  turn.complete（与 settings hooks 共用分发点，mods 在后）；matcher 支持
  值/数组/正则；`next` 链内联递归，返回 undefined=放行（e 取最后一次 next
  的参数）、返回对象=短路答案（deny/result/text/drop）。`$` API 十六个命名
  空间见 ccplugin_modapi.go；`$.command.register`/`$.tool.register` 注册的
  命令与工具（`mcp__<插件名>__<工具>`）经 command.run / tool.call 事件路由
  回 mod。
- **权限模型**（与 Claude 的有意差异，见下）：skills/commands/agents 静态组件
  随插件开关生效；**hooks/mods/mcp 执行型组件缺省关闭**，设置页逐类显式开启
  （显式组件开关 > 总开关 > 缺省）。mod 的 `$.fs.write`/`$.process.run` 过
  会话权限门控（read-only 拒绝）+ 危险命令护栏；settings hooks 的 shell
  命令同样过护栏。
- **接入点**：agent.go（工具循环 PreToolUse→tool.call→执行→PostToolUse、
  收尾 Stop/SubagentStop→turn.complete、开轮 SessionStart）；handleChat
  （斜杠展开→UserPromptSubmit→prompt.submit）；tools.go 的
  pluginAndMCPTools()（插件 MCP+mod 工具与独立连接器常驻工具的装配，
  只在重载边界变化——前缀缓存契约）；experts.go（动态专家）；
  skill_manager.go 的 read_skill（用户技能查不到回落插件技能）。
- **API**：`GET /api/cc-plugins`（清单+市场）、`POST /api/cc-plugins/toggle`
  （总开关/组件开关/userConfig 三合一）、`/install`、`/uninstall`、
  `/marketplace/add`、`GET /marketplace/{id}`（浏览）、`GET /commands`
  （弹层）。
- **测试**：ccplugin_test.go（frontmatter/占位符/decision 协议/matcher/加载/
  安装/真实插件冒烟——本机有 claude-plugins-official 克隆时装 25 个真插件
  验证）+ ccplugin_mod_test.go（goja 语法矩阵/ESM 转换/handler 链/mods
  端到端：官方 first-mod 示例原文全链路）。

## 独立 MCP 连接器（mcpservers*.go / mcpcatalog.go / mcpclient.go）

不装整个 CC 插件、直接添加一个 MCP 服务的注册表（stdio 命令或 http/sse
地址）。协议客户端在 mcpclient.go（与 CC 插件桥共用同一实现；宿主经
mcpHost 接口注入：插件侧 ccPluginHost 做 ${CLAUDE_PLUGIN_ROOT} 等替换，
独立侧 mcpPlainHost 只做 ${环境变量}）。

- **按需加载（技能式）**：一个连接器可带几十个工具，全量注册会撑爆工具
  目录、打断前缀缓存、误导模型（第一版正是这么做的，已废弃
  `mcp__<服务器>__<工具>` 全量注册）。现与用户技能同一纪律——清单先行、
  内容按需：环境快照只列启用的连接器（PromptBlock，名字+描述+工具数），
  模型按需 `mcp_read_connector(name)` 读工具清单与参数 schema、
  `mcp_call_tool(connector, tool, arguments)` 执行。两个工具常驻目录且
  定义恒定，连接器增删只动环境快照尾部，工具目录形状与前缀缓存不受扰动。
- **进程懒启动**：引擎启动期不拉起任何连接器进程（启动零 MCP 进程，
  无弹窗）；首次 `mcp_read_connector`/`mcp_call_tool`（调用自愈）或用户
  显式动作（设置页测试/开关启用/安装）才建立连接，连接后常驻复用、死亡
  自愈重启。stdio 子进程 spawn 过 hideConsoleWindow（mcpclient.go），
  Windows 桌面形态不弹控制台黑窗。
- **状态**：`<dataDir>/mcp-servers.json`（增删/开关/来源 manual 或
  catalog:<id>）；桥代码与各桥独立 venv 落 `<dataDir>/mcp-servers/<id>/`。
  信任边界与 CC 插件 mcp 组件一致：显式添加并启用 = 放行该连接器工具，
  不逐工具审批。不进统一插件页（有独立的 设置→MCP 分区，app-mcp.js）。
- **设计软件桥目录**（mcpcatalog.go，内置制造业常用软件的社区 MCP 桥，
  设置页一键安装）：solidworks（yimu0824/solidworks-mcp，COM）、multicad
  （AnCode666/multiCAD-mcp，中望 ZWCAD/AutoCAD/GstarCAD/BricsCAD）、blender
  （PyPI mcp-for-blender + addon.py）、freecad（PyPI freecad-mcp，需
  Python 3.12+，安装器检测拦截）、autocad（vigneshpbmenon，轻量 2D）。
  安装器走 GitHub codeload tar.gz（国内网络下 github.com 主站 git 协议常
  断，codeload/raw 一般可达）+ venv pip。上游依赖钉版与垫片：solidworks/
  multicad/autocad 均 import mcp 1.x 低层 API，pip 装 `mcp<2`
  （fastmcp<4）；autocad 的 requirements 写了不存在的 pyautocad>=2.0.0、
  `FastMCP(title=)` 老 kwarg，安装器直装可用版本并做一行替换。
- **API**：`GET /api/mcp`（清单+连通状态）、`POST /api/mcp`（增改）、
  `/delete`、`/test`（连通并列工具）、`POST /api/mcp/upload`（拖拽导入
  .json 配置，multipart）、`GET /api/mcp/catalog`（目录+已装标记）、
  `POST /api/mcp/catalog/install`（一键装，分钟级）。
- **拖拽导入**：MCP 页支持把 .json 配置直接拖入（行业三形态：
  `{"mcpServers":{...}}` / `{"名":{...}}` / 单服务器对象用文件名当名，
  parseMCPServersDoc；与服务端 readMCPPackage 同语义、两端各持一份最小
  实现）。
- **公司市场**：市场第四类型 mcp（服务端 jxaiserver 同步支持，包体为含
  mcp.json 的 zip，也接受裸 .json 直传）。客户端安装 = 下载解析后把每个
  服务器注册为连接器（来源标 `market:<条目id>`），卸载按来源整组注销
  （RemoveBySource）；已装标记按 HasSource。市场页有 MCP 标签。
- **测试**：mcpservers_test.go（名称域/状态回读/清单段与常驻工具恒定性/
  读取守卫/目录完整性/tar.gz 解压防穿越/三形态解析/按来源整组增删）；
  mcp_install_now_test.go 为手动实装入口
  （`JX_MCP_INSTALL_NOW=<目录id逗号列表> go test -run TestMCPInstallNow`，
  驱动真实下载安装+握手验证，缺省跳过）。

### 与 Claude Code 的已知差异（有意为之或暂不支持）

1. mod 敏感 API 过会话权限门控与危险命令护栏（Claude 对 mods 明确无沙箱，
   本引擎收紧）；`$.env.set` 限 `JXCC_<插件名>_` 前缀、`$.env.get` 拒读密钥类
   环境变量；`$.settings.read` 只回脱敏提示。
2. `.ts` mods 不支持（明确报错）；goja 覆盖 ES2017 常用语法，超新语法
   （类私有字段等）不保证。`.mcpb`/`.dxt` 打包、lspServers/outputStyles/
   themes/workflows/monitors/channels 组件忽略（扫描时记日志）。
3. UI 渲染类事件（ui.render/press/panes/blit…）不触发，注册了也收不到；
   `$.ui` 仅 toast/status/log 映射为全局事件流通知；`$.process.spawn`、
   `$.audio`、`$.telemetry` 调用即抛错。
4. settings hooks 事件子集：UserPromptSubmit / PreToolUse / PostToolUse /
   Stop / SubagentStop / SessionStart / PreCompact（其余 30+ 事件无对应物
   不触发）；prompt/agent/mcp_tool 型 handler 与 async 语义跳过；Stop 的
   block 决策不改变行为（只观察）。
5. `transcript_path` 提供简化 JSONL 导出（每行 {role,content}），非 Claude
   完整格式。
6. agents 的 model 别名（sonnet/opus）与 tools 白名单不生效；skills 的
   `!`cmd`` 动态注入、allowed-tools 免批、context:fork 不生效。
7. `$.model.fork` 不继承会话历史（与 complete 同路径）；`$.session.usage`
   未实现。
8. mods 无 `--plugin-dir` 热重载与 `claude plugin validate/test` CLI；装卸
   经设置页，重载即插件重载边界。

## 存储与模块结构

数据存于 `data/jx.db`（SQLite 单文件，WAL 模式）：`sessions`（会话元信息）、
`messages`（全层级共用消息表：seq/role/kind/compact/摘要边界）、`meta`（版本与
迁移标记）。旧版 JSONL 数据首次启动自动迁移，原文件保留为备份。

| 文件 | 职责 |
|---|---|
| `main.go` | 启动入口：配置、HTTP 服务、自动开浏览器、-webdir 开发模式 |
| `config.go` | 运行配置（含 providers 与压缩设置）的加载/保存/兼容折算 |
| `models.go` | 多 provider 模型清单、调用目标解析、模型列表拉取（按渠道协议适配端点与鉴权头） |
| `config.go` 的 `companyQuota` | **公司通道额度快照**（服务端随 `/announce`、`/provision` 的 `quota` 字段下发，客户端只存不算）：`sync.go` 的 `applyAnnounce` 与 `wizard.go` 的 `applyProvisionPayload` 写进 `Cfg.CompanyQuota`（`quotaEqual` 判变化才落盘）；`/api/models` 带 `companyQuota`（模型菜单里**公司通道那个分组头**上显示「剩余 N 积分」，悬停看明细）、`/api/config` 的公司通道带 `quota`（设置页公司通道**卡片头**的额度 chip + 展开后的明细与「刷新额度」）；设置页「刷新额度」走 `POST /api/company-sync`——复用 `companySyncOnce` 重拉一轮公告，**不为额度另开查询接口**（new-api 的 AI 接口没有余额端点，额度只能由服务端代读并随公告下发） || `llm.go` | OpenAI 兼容流式客户端（SSE 解析、工具调用增量聚合）+ mock 演示模型；`StreamChat` 是全部协议的统一入口（按 `LLMTarget.Protocol` 分发，主备地址故障转移/断开器登记/错误整理三协议共用） |
| `llmproto.go` | **渠道协议扩展**：`llmProtocol`（端点路径+鉴权头+请求转换+SSE 解析）三个实现——OpenAI 兼容 chat/completions（原行为原样）、Anthropic 原生 /v1/messages、OpenAI Responses /v1/responses。统一 Message/ToolCall 与协议格式互转（system 提取、tool_use/tool_result、function_call/function_call_output、图片 data URL）、思考档位映射（Anthropic budget_tokens：low 2k/enabled·medium 6k/high 12k/max 24k，max_tokens 不足自动抬升；Responses effort：low/medium 原样、high·max→high、enabled/disabled 不干预）、usage 归一（Anthropic 的 cache_read/cache_creation 映入 Usage 缓存字段）。看护测试 llmproto_test.go（转换/SSE/工具往返/mock 网关端到端/既有渠道报文回归） |
| `contextengine.go` | 上下文引擎：请求材料的唯一组装者 + 上下文事件总线。系统提示词（可被 `data/prompts/<mode>.md` 引用文件覆盖，引用文件附内容 hash 标记来源；版本号不进提示词——随发版变化会白白打断前缀缓存）、环境快照、工具目录、历史视图、视觉内联经 `Assemble` 成包（确定性：同一状态逐字节一致——前缀缓存命中的根基，有测试看护），token 估算与前缀指纹随包产出；每轮开轮的 prompt 展示快照行经 `OpenRound` 落库；需把上下文发给模型的侧路调用（模型摘要）由 `CallSummarizer` 拼装并发送（留痕 audit scope=summarize，不进请求环形缓存）；权限/模型/思考档位/计划目标/工作区/远程挂载等配置变更经 `Notify` 落时间线（audit type=ctx，含前后值）；技能增减由环境快照 diff 在尾部留痕，同一事实不重复记账 |
| `agent.go` | 主循环：每步经上下文引擎取包 → 流式请求 → 执行工具 → 回填（只做编排，不亲手拼装——发给模型的内容一律来自引擎 Assembly）；`envPrompt` 生成运行环境快照（不含时间——时间走 context.go 的 timeAfter 派生通道） |
| `perm.go` | 权限引擎：三档模式、授权凭证（签发/核销）、审批队列、受保护路径、危险命令拦截、沙箱内浏览器预拦（browserLaunchHit：非 full 档启动浏览器的命令执行前拦下并指路 browser_*，确定性失败路径的快速失败，不做组件开关） |
| `plans.go` | 计划读取端点：从会话日志里取回 exit_plan_mode 的正文（右栏「计划」视图的数据源） |
| `requestmgr.go` | 任务找经理协调（request_manager）：把求助整理成一条消息投进数字助理主对话（kind=aiuser、origin=help），**立即返回、不等答复**——经理空闲就落库并唤醒它跑一轮，忙则插话注入它正在执行的那一轮（AI 发话一律插话，与【派发回报】【待办任务派发】同一套投递）；经理侧**没有**专用回复工具，它用既有的 send_to_session / dispatch_task 处理。不建池、不加接口（消息本身即记录）；子对话调用被硬拦、计划模式暂停；启动自检 assertManagerChannel 看护「只有执行模式有」 |
| `interact.go` | 模型↔用户交互工具（ask_user_question / enter·exit_plan_mode / update_goal）；ask 提问限时（timeout_sec 设秒数，默认 300、钳 30-600、显式 0=不限时；分档准则在系统提示词「提问的分寸」段，状态公告在环境快照「提问等待」行；界面倒计时、悬停/触碰/按钮停表转不限时，超时视为放行）；久等声光提醒（默认开、240 秒、设置·常规可调：MessageBeep 叮声 + ask_alert 全屏闪烁，浮窗上 15 秒倒计时走完才 TTS 朗读；悬停/触碰/停表、或从任何入口打开对应计划页/浮窗（holdQuestion 也受理无时限的计划审批）连提醒一并作废，见 askAlertDue）；计划审批无时限且提交即落库，重启后由 resolvePersistedPlan 收口批准/修改/忽略 |
| `interact_plan_test.go` | 计划审批的时限与持久化语义测试（跨重启挂待审批、批准/修改/忽略三分支、plan 无 TTL、ask 的 timeout_sec 与停表） |
| `remote.go` | SSH 远程工作区：连接池、TOFU 指纹库、远程 exec、SFTP 文件操作、连接测试 |
| `remotelist.go` | 已登记远程工作区（工作目录面板的远程条目）：DPAPI 加密存储、凭据回填、清单/登记/忘记 API |
| `tools.go` | 任务会话工具集（接入权限引擎与本地/远程执行环境抽象；含 web_fetch） |
| `run_python.go` | run_python 工具：AI 交 Python 脚本全文，自动落盘 `.jxai/tmp/<会话id>/`（会话删除随 handleDeleteSession 回收、启动按天兜底清理）后用内置便携运行时执行；与 run_command 同一把权限锁/沙箱/编码注入（Shell 形态，不走绕过沙箱的 Argv 直跑）；提示词按「建议」引导 JSON/引号/多步场景优先用它（run_command 仍可用，无硬限制）；ensureSkillRuntime 缓存条件按 modules 核对（防裸运行时短路 office 依赖补装）。**后台执行**：run_in_background=true 立即返回作业 id（不限时），跑完由 startJob 的 onDone 回调投【后台脚本回报】（aiuser）并起汇报轮唤醒本会话（reportPythonJob；被终止的走 NoKick 不自动叫醒，对齐派发回报的叫停纪律） |
| `bridge.go` | **run_python 工具桥**（编排原语，对齐 DSH PTC / ZCode workflow 的思路）：每次 run_python 在会话脚本目录生成 `jx.py`（含一次性 token），脚本内 `import jx` 即可 `jx.call(tool, args)` 调白名单工具（read/glob/grep/list_dir/write/edit/snapshot_file/run_command/web_fetch/web_search/read_skill）、`jx.dispatch(task)` 派子任务、`jx.wait(sid)` 等回报、`jx.sessions()` 看状态。链路：脚本 → POST 127.0.0.1:`listenPort`/api/bridge/tool（loopbackOnly 闸）→ 以该会话 toolContext 走既有工具管线（权限/沙箱照常）。安全三层：token 一次性（前台随 runChild 返回注销、后台随作业收尾注销）；白名单+剥审批参数（sandbox_permissions/justification/run_in_background/visible 一律剥——桥上无交互上下文，绝不弹卡）；限额（总量 200 次/并发 4/活跃子任务 4）。**回报路由**：桥派子任务的【派发回报】在 runtime.go 投递点被 bridgeInterceptReport 拦截存表（不投父会话、不起轮），由 jx.wait 取走（取走即清）；脚本退出时 bridgeRelease 注销 token、把未取走的回报转交常规投递（startReportRound），主对话只被打扰一次、回报不丢 |
| `websearch.go` | 联网搜索工具 web_search 的客户端半边：调 jxai-server 的 /api/search 网关（服务端持上游 key，首版上游=智谱联网搜索 MCP）；服务端地址取 searchURL，空则市场同源；服务端走不通（未配置/不可达/网关报错）时错误文案附加改道指引，让模型转用「本机网络搜索」技能（内置技能 技能/web-search-local/，纯标准库 search.py 直调搜索 API：智谱/博查配 key 优先、Firecrawl 免 key 兜底）——指引仅在技能可用时附加（searchErrHint→skillAvailable，内置在位或用户技能形态存在且启用），技能不在场后指引自动消失，兜底能力不进工具定义、整体可插拔。设置入口在 设置·网络访问·「联网搜索」分区的「搜索服务」框（改 searchURL，即时生效、无需重启）：旁边原样显示后端算出的当前生效 base——`GET /api/config` 的 `searchURL`/`searchEndpoint`/`searchEndpointErr`（分别是配置值、此刻真要打的地址、算不出来的原因），让用户一眼看到搜索现在打哪儿；保存走既有单字段 `POST /api/config`，非法地址在保存时就 400（`searchURL 不合法`，与运行期同口径），留空即回到市场同源 |
| `browser_ops.go` | browserTarget 操作面：browser_* 工具与面板端点的形态无关接口 |
| `browser_host.go` | chromedp 浏览器宿主：有头独立窗口 + 网页形态的后台无头内嵌目标 |
| `browser_panel_windows.go` | 桌面右栏原生浏览器面板：第二个 WebView2 控件 + 控件级官方 CDP 通道 + 矩形同步 |
| `tool_browser.go` | browser_* 九件工具（形态无关，作用于 browserTarget） |
| `assistant.go` | 数字助理总控：编排工具 + 会话压缩摘要（建会话/模型选择走 sessioncreate.go 的 service） |
| `callapi.go` | call_api 通用直通工具（仅 assistant 模式注入）：路由元数据注册表（server.go apiRoute 注册即元数据，AI 可调接口目录是注册的副产品）+ 进程内直调执行器（httptest Recorder 打到与前端同一 mux）+ 最低权限档标注与危险接口审批（调用时按会话档位校验）；完整目录渲染进 call_api 工具定义（含档位与 ⚠ 标记，进程内缓存） |
| `sessioncreate.go` | 建会话/改权限的 service 单源：createSession（HTTP 人工建会话、dispatch_task、dispatch_sub 三入口共用：模式/权限默认、能力默认、分支克隆、历史继承（失败回滚）、skill-create 演示材料兜底）+ applyPermissionChange（权限档落装与能力联动，人工 PATCH 与 send_to_session 共用） |
| `assistant_versions.go` | 长期助理主对话的「版本」：每一版就是一个 assistant 顶层会话（会话 id/标题/工作目录/权限/模型各自独立），版本号落 sessdata、当前版本指针落 globaldata（**唯一口径 currentAssistant()**——设置页压缩/清空、待办转交、request_manager、前端入口都认它，不再按「最近更新的那个」猜）；新建一版从零开始、老版本原样保留，切换只动指针 |
| `historytools.go` | 跨会话历史查询工具（search / read_session_messages） |
| `context.go` | 上下文组装管线（环境快照**追加式**：首条整份、后续差分——纯新增发短补充、条目有减/换值发【移除】/【新增】中文标签变更清单（不用 +/- 记号：与条目行首 "- " 列表符撞车有歧义）、差分连续 5 条后完整重发一次校准（envRowRender 计数），已发送前缀绝不改写）+ 规则压缩 + 模型摘要 + 溢出兜底 + 压缩成功后无条件重发完整环境快照（resendEnvAfterCompact：基线被埋时保留区首条整份恢复，同文差分为空零冗余） |
| `runtime.go` | 后台运行时：执行注册表、全局事件总线、派发执行体、中断恢复 |
| `session.go` | 会话元信息与工具函数 |
| `store.go` | SQLite 存储层与旧数据迁移 |
| `adapters.go` | DSH 插件兼容层实现：扫描、开关联查、资产清单、Go 半边注册点 |
| `pluginlayers.go` | 插件兼容层注册表（主体第一层）：`PluginLayer` 契约 + 编译期自注册 + 统一清单/总开关（`/api/plugins` 的 layers+plugins） |
| `adapter_dsh_speech.go` | dsh-speech 的 Go 半边：润色端点（复用已配置模型）+ 听写提示词段 |
| `components.go` | 组件管理：扫描、开关状态、组件资产服务、`Rescan`（市场装卸后重扫） |
| `update.go` | 更新检查：定期拉发布源（updateURL，Gitea releases/latest 或 jxai-server 的 /updates/latest，两者同格式），tag_name 比当前新就经事件流推横幅；versionCompare 双体系（日期+V序号 恒新于 旧点分号） |
| `update_blocks.go` | 分块静默更新：release 附件带 blocks.json（快照式清单=每块当前应是什么版本）就走静默路——engine 块趁旧进程活着原地换 exe（改名交换，重启生效，`POST /api/update/restart` 用 PowerShell 助手逃逸 Job 接力重启）、组件块杀 worker 后整目录替换热生效；落后多版一步对齐每块最新，没有 blocks.json 的旧源回退整包横幅；`config.updateMode`（auto/manual） |
| `update_restart_windows.go` | 立即重启助手：经 WMI 托管一条纯 cmd 内联接力命令（不落地脚本文件——2026-10-15 实测部分机器拦 WMI 派生进程执行 .ps1 与 cmd→powershell 链；WMI 派生进程工作目录在 System32、不认裸名 powershell，一切路径必须绝对）到 Job 外，等约 5 秒拉起新 exe 并留痕 restart.log，ShowWindow=0 不闪窗；WMI 失败回退 CREATE_BREAKAWAY_FROM_JOB |
| `wizard.go` | 公司接入向导后端：POST /api/wizard/discover（UDP 广播找同公司服务端——浏览器发不了 UDP 故在 Go 侧做，255.255.255.255+子网定向广播、公司名=口令、registryURL 查询兜底）+ POST /api/wizard/connect（验证 healthz → POST /provision → 内部提供方 company-internal 置顶即默认、marketURL/updateURL/wizardDone 一键落位；重跑替换不堆积）；config 增 wizardDone/registryURL |
| `market.go` | 市场客户端半边：拉 jxai-server 清单（60s 缓存）、下载 zip+sha256 校验、三类型安装路由（技能→addUserSkill / 组件→组件目录+Rescan / 插件→ccInstallFromPath 落 `jx-market`）、装卸记账 `data/market_installed.json`、`GET /api/market` + `/install` + `/uninstall`；地址在 `config.marketURL`（设置·网络访问，环境快照尾部有「企业技能市场」一行：地址+自签 HTTPS 需 curl -k 的提示）。对服务端的请求一律带 `X-Jxai-User` 头（值=发布者姓名，没设不带）——服务端记条目归属 owner 与下载/安装明细；安装成功后上报 `POST /api/market/report-install` |
| `brand_sync.go` / `brand_sync_test.go` | 品牌下发客户端半边（服务端 jxaiserver brand.go → /announce.brand）：LOGO 与界面文字（侧栏/首页 hero/关于页/title）由服务端管理页统一配置，**客户端无配置入口**只消费；缓存于 `data/brand/`（brand.json + logo.png），渲染全程读本地（`/logo.png` 路由优先本地缓存图、`GET /api/brand` 出文字、前端 brand 事件刷新）；**LOGO 按 logoHash 比对只在该换时下载一次**（流量硬口径），nil（老服务端）不动本地，hash 空回退内置默认 |
| `migrations.go` / `migrations_test.go` | 迁移机制（Flyway 式）：版本化一次性任务 + 执行账本 `技能/.migrations.json`（id→执行时刻）。一次性动作（清理误发文件、目录搬家……）往 `appMigrations` 清单**尾部**追加条目，NewApp 启动时跑：执行过记账永不再跑，失败记日志不记账下次重试，不阻断启动。id 规范「日期.短名」；任务体尽力而为（小失败内部消化、返回 nil 即记账；SkillsDir 等基础设施错误才返回 error）。**账本必须与清理对象同域**：技能目录是程序目录域（开发版/隔离实例各有 dataDir 却共用一个技能目录），账本放数据目录会让每个新实例首跑重炒一遍（2026-11-09 事故：隔离实例重跑清理，删了市场正式安装的 4 个技能）；旧位置 `data/migrations.json` 只读合并、升级首跑自动搬进新账本。首个收编条目：泄漏技能清理与内置技能搬家（此前是「每次启动全量扫描、靠自身幂等」的散装自愈）。数据库 schema 迁移另有一套（store.go 的 migration，管 SQLite），两者各管各的域 |
| `market_publish.go` / `market_publish_test.go` / `market_publish_meta_test.go` | 市场·技能发布（**先装后传**）：只能发布已装到 `技能/user/` 的技能——后端从安装目录现场打 zip、走 inspectSkillZip/rewriteSkillZip 校验链（恰好一处 SKILL.md、内容体检、结构规整）、元信息按回退链补全（id 候选含中文→英文词表 `skillIDFromChinese`，词表只在 Go 一份），流式转发服务端用户端点 `/api/market/publish`；`GET /api/market/publish-meta` 给弹层预填；下架走 `/api/market/unpublish`——**归属校验与「挪进服务端回收站」都在服务端**，客户端只发消息（本地姓名比对只是按钮显隐的 UX） |
| `todo.go` / `todotools.go` | 待办与定时任务（组件 `todo-scheduler` 的宿主半边）：清单、调度、两类执行（AI 任务可选执行目标：长期助理/新工作任务，后者走 `createSession` 单源建会话）/ 模型工具 |
| `gitpanel.go`（+`gitpanel_windows.go`/`_other.go`） | Git 面板（组件 `git-panel` 的宿主半边）：仓库扫描、porcelain/图谱/分支解析、暂存/提交（白名单 argv 直调 git，永不过 shell） |
| `gittools.go` | git_sync 模型工具（组件 `git-panel`）：与面板共用 gitSync 执行缝的宿主通道远程同步，绕开沙箱内远程 git 必败的限制（背景与实测见《Git远程同步工具-改动说明.md》） |
| `server.go` | 路由、聊天 SSE、全局事件流、压缩接口、插件注入、内嵌前端 |
| `web/` | 前端：工作区分组侧栏、子对话抽屉、子任务卡片、模型选择器、设置页、市场页（`app-market.js`：左侧栏底部入口、主区整页视图 body.market-mode、三 tab+搜索+一键装卸；组件装卸后 `CX.refresh()` 热生效） |
> **服务端已独立成仓库**（`D:\智能体工作区\jxaiserver`，module jxai-server）：远程访问中继 + 技能/组件/插件市场（含 /admin 管理页）+ 更新分发（日期+V序号版本体系）+ 公司接入（登记/UDP 发现/按人发 new-api 令牌/每月额度）。本仓库只保留客户端半边（market.go 市场客户端、wizard.go 接入向导、update.go 更新检查——与服务端的协议契约见各自文件头注释）；其部署与用法见该仓库 README/部署。开发完全分开，两边接口变更需双侧同步。

## 渠道协议（provider protocol）

同一套 agent（主循环/上下文/工具）支持三种渠道协议，按**渠道类型**而非全局选择：

- **选型入口**：设置 → 模型 → 渠道卡片 →「协议类型」下拉。三选一：
  `OpenAI 兼容 · chat/completions`（默认，存量配置不受影响）、
  `Anthropic 原生 · /v1/messages`、`OpenAI Responses · /v1/responses`。
  预置的「Anthropic」提供方固定走原生协议、「OpenAI（Responses 协议）」
  固定走 Responses（同 BaseURL 同 Key，两种端点并存）；自定义渠道自由切换。
- **实现**：`ProviderCfg.Protocol`（空串=OpenAI 兼容）→ `LLMTarget.Protocol` →
  `StreamChat` 按 `llmProtocol`（llmproto.go）分发；mock 演示、主备地址故障
  转移、停止断开器、错误整理三协议共用；agent/上下文/工具循环零感知，
  消息仍是统一 Message/ToolCall 形态，转换只在发出前最后一步。
- **baseURL 约定**：三种协议一致——填到版本前缀（如
  `https://api.anthropic.com/v1`），端点路径由协议实现拼
  （/chat/completions、/messages、/responses）；「拉取模型」「测试连通」
  也按协议选端点与鉴权头（Anthropic 用 x-api-key + anthropic-version，
  其余 Bearer）。
- **思考档位（与模型彻底解绑）**：全系统固定六档 `none` / `low` / `medium` /
  `high` / `xhigh` / `max`（显示名 None/Low/Medium/High/Extra High/Max，
  前端 THINK_LABELS）。任何模型、任何渠道都显示同一套，由用户自己选——
  不再按模型名推断，也不采用服务端 `/models` 上报的
  `effort.supported_levels`（未知档位透传到前端只会「能选、后端 400」）。
  模型支不支持、服务端认不认都不在这里管：能自动降级就降级，报错让用户
  自己换档。老会话库里的档位兼容读：`disabled` → `none`、`enabled`（只开关
  不指深浅）→ 空串（跟随服务端默认），迁移 0016 一次性归一存量数据。

  | 渠道协议 | 请求报文 |
  |---|---|
  | OpenAI 兼容（chat/completions） | `reasoning_effort`：六档原样发（none…max）；空串不干预 |
  | OpenAI Responses（/v1/responses） | `reasoning.effort`：六档原样发（none…max）；空串不干预 |
  | Anthropic 原生（/v1/messages） | `none` → `thinking:{type:disabled}`；其余五档 → `thinking:{type:adaptive}` + 顶层 `output_config.effort` 原样发。不再用 `budget_tokens`（老映射已删，也不为思考抬 `max_tokens`）；adaptive 时不发 temperature |

  会话级档位 PATCH 白名单（server.go）= 六档 + 空串（`enabled`/`disabled`
  归一后接受），建会话路径同一套校验（sessioncreate.go）。
- **usage 归一**：Anthropic 的 `cache_read_input_tokens` /
  `cache_creation_input_tokens` 映入 Usage 的 CacheReadTokens /
  CacheCreationTokens（写入量不计入命中）；Responses 的
  `input_tokens_details.cached_tokens` 走 OpenAI 标准口径。取数一律走
  `CacheHit()/CacheMiss()/ReportsCache()`。
- **工具往返**：Anthropic 用 tool_use/tool_result block（连续 tool 消息
  合并进同一条 user 消息；assistant 的 reasoning_content 不回传）；Responses
  用 function_call/function_call_output 顶层 item；OpenAI 兼容照旧
  tool_calls/tool_call_id。三协议对上层暴露完全相同的 ToolCall。

## 前后端接口

下表是人类（前端）使用的接口。**AI 另有一条通用直通通道**：call_api 工具（仅 assistant 模式注入，其余模式一律没有）——同一批接口按「注册即元数据」开放（server.go 的 .apiRoute(..., apiMeta{...})，元数据缺席=不对 AI 开放；SSE 流、multipart 上传、二进制下载、组件桥等不适合同步直调的保持关闭），进程内直调与前端同一 handler（见 callapi.go）；完整目录（每条标注最低权限档与 ⚠ 危险标记）拼在 call_api 工具说明里静态随请求前缀，不进环境快照，会话档位不足在调用时被权限闸拒绝并提示需要的档位，危险接口（删除/清空类）调用前走审批卡。执行类工具与意图工具（dispatch_task 等）不受影响——它们与前端共用 service（sessioncreate.go / deliverToSession / stopRunBy / registerUploadPath / chatModes）。

| 方法与路径 | 说明 |
|---|---|
| `GET /api/status` | 服务状态 |
| `GET/POST /api/config` | 读取/修改配置（Key 回传掩码，providers/压缩设置） |
| `GET /api/models` / `POST /api/providers/fetch` | 模型清单 / 从提供方拉取模型 |
| `GET/POST /api/sessions` | 会话列表（`?parent=` 查子会话；列表项带 `status=running` 与异常中断标记 `lastStop`）/ 创建（mode/parent/provider/model） |
| `GET/PATCH/DELETE /api/sessions/{id}` | 详情（含 `running` / `pending` / `error`：执行中、待生效消息、最近一次失败原因）/ 改标题、工作区、模型 / 删除 |
| `POST /api/sessions/{id}/chat` | 发送消息；SSE 流式返回。**执行体独立于本请求**：浏览器刷新/关页只断开这条流，执行照旧跑完并落库 |
| `GET /api/sessions/{id}/stream` | 接上正在执行的会话流（刷新页面/换窗口后继续看实时输出）；未在执行时立刻回一帧 `done` |
| `POST /api/sessions/{id}/stop` | 显式停止本会话执行（流断开不再等于停止） |
| `POST /api/sessions/{id}/retry` | 失败后重试：不重复落库用户消息，带着当前上下文再跑一轮（SSE 流式返回，同 /chat） |
| `POST /api/sessions/{id}/interject` | 执行中发消息：`{content, mode}`，`steer`=插话（模型调用中立即打断当前流、下一步带着插话重调；工具执行中等下一步调用模型前注入）、`queue`=排队（本轮结束后作为独立一轮发送）；未在执行时按排队处理并顺手拉起执行 |
| `POST /api/sessions/{id}/pending` | 排队坞操作：`{id, action:'remove'\|'steer'}`（移出队列 / 提前为插话）、`{id, action:'move', before:锚点id\|空=队尾}`（拖拽排序，只动排队队列顺序）、`{action:'pause'\|'resume'}`（队列级暂停/继续，不需要 id；恢复时空闲且有排队消息顺手拉起执行）；前端「编辑排队/插话消息」＝ `remove` 后把原文回填输入框草稿，复用 remove，无新 action |
| `POST /api/sessions/{id}/compact` | 手动压缩：rule 模式同步折叠即回；llm 模式同步预检后转后台摘要（202 `{async:true}`，完成/失败经 session_updated 广播，重复点火 409） |
| `POST /api/sessions/{id}/autocompact` | 提前自动压缩：通道选路同阈值触发（rule 同步折叠 / llm 后台摘要，同上） |
| `GET /api/assistant/versions` | 长期助理主对话的版本清单（`{current, versions:[{sessionId,version,messages,updatedAt,running,current}]}`；当前版本指针存库，前端入口与设置页都按它走，见 assistant_versions.go） |
| `POST /api/assistant/versions/new` | 新建一版数字助理主对话（从零开始聊，沿用当前版本的工作目录/权限/模型等设置；老版本历史、快照、交付原样保留） |
| `POST /api/assistant/versions/switch` | 切换当前版本（`{sessionId}`）：只动指针，两边数据都不碰；切完主对话区显示那一版的历史 |
| `POST /api/assistant/compact` `/clear` | 对**当前这一版**压缩 / 清空历史（clear 已不在设置页露出——那里换成了「新建一版」，接口保留给彻底重置本版历史用） |
| `POST /api/sessions/{id}/remote` | 设置/解除会话的 SSH 远程工作区（`{remote:{…}}` 或 `{clear:true}`） |
| `GET /api/approvals` | 待决定的越权审批清单（轮询兜底） |
| `GET /api/questions` | 待回答的助手提问 / 计划审批清单（轮询兜底） |
| `POST /api/questions/{id}` | 回答提问；计划批准传 `__approve__`，修改传反馈文本，**忽略（搁置：本轮停下、计划仍留着）传 `__ignore__`**；内存里没有等待者的（重启后遗留的待审批计划）走 resolvePersistedPlan |
| `GET /api/sessions/{id}/progress` | 会话进度清单（progress_write 的持久化结果） |
| `POST /api/sessions/{id}/stop-child` | 停止会话的一个运行中程序（进度面板停止按钮；body: id=job-*/child-*，userstops.go） |
| `GET /api/sessions/{id}/plans` | 会话历史里提交过的计划（exit_plan_mode 的正文 + 出处，右栏「计划」视图的数据源；plans.go） |
| `POST /api/sessions/{id}/plans/export` | 把某份计划写成工作区里的 `计划-<时间>-<标题>.md`（`{seq}` 缺省最新一份；同名不覆盖、顺延 -2/-3） |
| `POST /api/approvals/{id}` | 审批决定：`{approved, always}`（always=本会话总是允许） |
| `POST /api/workspace/test` | 测试 SSH 远程工作区配置（拨号/SFTP/目录，回显主机指纹） |
| `POST /api/workspace/ls` | 浏览远程主机目录（连接规格 + path，path 空回用户主目录） |
| `GET /api/remotes` | 已登记的 SSH 远程工作区清单（remotelist.go；**不回密码**，key=display 供删除定位） |
| `POST /api/remotes` | 登记/刷新一个远程工作区（密码 DPAPI 落盘 `data/remote_workspaces.json`；密码缺席时按 主机+端口+用户 从登记库回填） |
| `POST /api/remotes/delete` | 忘记一个登记条目（`{key}`，不影响已有会话） |
| `GET /api/events` | 全局事件流（后台派发进度、回报就绪、会话更新、审批请求） |
| `GET/POST /api/plugins` | 统一插件清单（`{layers, plugins}`，带来源体系）/ 切换开关（`{layer,id,enabled}`，即时生效，界面部分刷新后生效） |
| `GET /_plugins/{id}/{file}` | 插件浏览器半边资产（仅启用时提供） |
| `GET/POST /api/components` | 组件清单 / 切换开关（前端热生效，无需刷新） |
| `GET /api/components/{id}/{file}` | 组件浏览器半边资产（仅启用时提供；挂 `/api/` 下让中继隧道覆盖，cx.js fetch 文本注入） |
| `GET /api/market` | 市场清单视图（jxai-server 清单 + 本机安装态；`?refresh=1` 绕过 60s 缓存；未配置时给建议地址） |
| `POST /api/market/install` `/uninstall` | 一键安装/更新/卸载 `{type: skill\|component\|plugin, id}`（下载 zip+sha256 校验后路由到三套体系；组件装卸后前端 `CX.refresh()` 热生效；安装成功后台异步上报服务端记 install 明细） |
| `GET /api/market/publish-meta` | 已装技能的发布元信息推导 `?skill=<id>`（弹层预填：id/显示名/版本/描述/标签 + 词表建议标注） |
| `POST /api/market/publish` | 发布**已安装**的用户技能 `{skillId, id, name, version, description, tags}`（后端从 `技能/user/` 打包转发市场服务器，身份=X-Jxai-User 头；先装后传） |
| `POST /api/market/unpublish` | 下架自己发布的条目 `{type, id}`（服务端判归属后挪进回收站，403 文案透传） |
| `POST /api/skills/install-files` | 技能文件夹拖拽安装（multipart files+paths，公共顶层目录=技能文件夹名；散文件/路径穿越拒收，走同一条 addUserSkill） |
| `GET/POST /api/todos` | 待办/定时任务清单 / 新建（组件 `todo-scheduler`；未启用时 404。**跨会话的全局清单，与会话内 `progress_write` 的进度清单是两回事**） |
| `POST /api/todos/update` `/delete` `/run` `/missed` | 修改 / 删除 / 立即执行 / 处理错过的槽点（`{id, action:'run'\|'dismiss'}`） |
| `GET /api/todos/workspaces` | 程序任务可选的工作目录（默认工作区 + 历史会话用过的目录） |
| `GET /api/git/status` `/diff` `/graph` `/commit` `/branches` | Git 面板读接口（组件 `git-panel`；未启用时 404。graph 为 `--all --topo-order` 的父母哈希+装饰引用，泳道由前端计算） |
| `POST /api/git/stage` `/unstage` `/untrack` `/commit` `/summarize` `/checkout` `/sync` | Git 面板写接口：暂存 / 取消暂存 / 移出跟踪变未跟踪（`rm --cached`，保留工作区文件）/ 提交 / AI 生成提交信息（只生成不提交）/ 切换本地分支（分支名限服务端列出的清单，改动会被覆盖时 git 拒绝）/ 远程同步（fetch → 快进或无冲突自动合并 → push 当前分支；需要决策时 `{needHelp:true, summary}` 原样退回，前端建议派工作任务）。仓库只能选服务端扫描出的候选，路径经 pathspec 校验 |
| `POST /api/plugins/dsh-speech/polish` | 转写润色（dsh-speech 适配器 Go 半边） |
| `GET /api/voice/voices` | Edge 神经音色清单（朗读音色下拉数据源；中文区排前，进程内缓存 6 小时；组件 `voice`，未启用时 404） |
| `POST /api/voice/tts` | `{text, rate, voice}` → mp3 音频流（后端走 Edge「大声朗读」无密钥通道；前端 blob 化 `<audio>` 播放）。听写不走后端：浏览器 SpeechRecognition |
| `GET /` | 内嵌前端页面（按开关注入插件资产） |

聊天 SSE 事件：`message` / `delta` / `tool_start` / `tool_wait`（等待审批）/ `tool_end` /
`retry`（重试开始，半截流式作废）/ `auto_retry`（自动重试提示）/ `sub_start` / `sub_end` /
`title` / `done` / `stopped` / `error`。全局事件流另含 `sub_tool`、
`report_ready`、`session_updated`、`approval_required` / `approval_resolved`（审批卡）、
`question_required` / `question_resolved`（助手提问 / 计划审批卡；required 带
deadlineMs 倒计时截止——ask_user_question 可用 timeout_sec 设秒数，默认 300、
钳 30-600、显式 0=不限时；`question_deadline`（held）是停表广播：用户悬停/触碰
卡片或点停止按钮即停表转不限时；`ask_alert` 是久等声光提醒——设置·常规的
询问提醒（默认开、240 秒）到点未答且未停表时：后端 MessageBeep 叮一声 +
广播此事件，前端提醒层先淡压暗＋光晕呼吸、再用 15 秒缓入（与倒计时同步）渐变成
**整屏主题色并常驻不自动收起**（关掉/被处理才恢复原界面），浮窗上走 15 秒
可见倒计时、倒计时结束才按配置文案走语音组件 TTS 朗读（15 秒内关掉/从任何
入口进去处理即取消朗读），停表与「已在处理」连提醒一并作废）、`todos`（会话待办刷新）、
`todo_tasks`（待办/定时任务清单变化，组件 `todo-scheduler`）
（均带 sessionId，前端按会话过滤渲染）。

## runtime 块版本格式（2026-11-01 定，有过一次事故）

- 现行：复合格式 `py<Python版本>-<发布版本>`（如 py3.14.8-20261101-1），打包脚本自动生成。
  原因：运行时包里除 Python 还有 pip 装的库，装了库哈希就变——只用 Python 版本会「同号不同哈希」。
- **计划（约 2026-11 中下旬）**：客户端里那道「包内 Python 版本 == 块版本前缀」校验已删（8a46312683），
  等装有该校验的客户端基本淘汰后，切换为**纯日期式**（与客户端版本同款 2026MMDD-N）。届时改
  `打包\发布静默更新.ps1` 的 `$RuntimeVersion` 一处即可。
- 铁律：**重打 runtime 块（哪怕一行没改）必须伴随新发布版本号**；绝不复用版本号发不同哈希。
