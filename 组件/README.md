# 组件开发说明（组件/ 目录）

本目录是组件的本体。组件 = `组件/<名字>/` 一个目录，最低要求
`manifest.json` + `client.js`（`client.css` 可选）。本文件讲清楚**两档归属**、
三种组件形态、各自的边界与开发规矩——**新组件怎么选形态、放哪个目录，照此执行**。
方针在《AGENT.md》「组件管理」章节也有提要。

## 两档归属：内置组件 / 用户组件（走同一个组件桥）

| | 内置组件 | 用户组件 |
|---|---|---|
| 目录 | `组件/<id>/` | `组件/user/<id>/` |
| 来源 | 随安装包分发（打包必带） | 市场/发行渠道单个安装，或手动放进该目录 |
| 开关 | ✅ 可开关 | ✅ 可开关 |
| 删除 | ❌ 不可删除（只能关开关） | ✅ 可删除（设置 → 组件 → 用户组件） |
| 更新 | 随程序静默更新的「组件分块」热更新 | 由用户经市场重装（**不热更新**） |
| git / 打包 | 入库、进安装包 | **不入库、不进安装包**（`.gitignore` 记 `组件/user/`，打包 robocopy 排除、升级清理清单跳过） |

两档**只有一个组件桥**：注册表（components.go）同时扫两个目录，`builtin`
标记归属；worker 拉起、钩子订阅、端点 `/api/c/<id>/…`、资产
`/api/components/<id>/<file>`、热开关、会话级覆盖全部同一条路——组件自己
不需要知道归属。差别只在「谁持有目录」：内置由程序持有（升级自动回补/更新，
手动删目录只在下一次发版前有效），用户由用户持有（版本与去留都归用户，
静默更新遇到同名用户组件直接跳过，市场也不得覆盖同名内置组件）。

- 设置页一级页叫「组件」，内部分「内置组件 / 用户组件」两区（与「技能」页同款）。
- 新人写组件：**默认写进内置区**（`组件/<id>/`，随包分发）；只有「社区/市场
  单独发行、希望用户能自己删掉」的组件才走 `组件/user/`——那一区在本仓库里
  通常不存在，是运行期的用户数据。
- 发现机制：启动扫一次，**平时不扫**；打开 设置 → 组件 时带 `rescan=1` 重扫
  一次两个目录，运行中放进/删掉目录**打开设置页即见**，不必重启服务。

## 三种形态

### A. 纯浏览器半边组件（默认形态，新增组件一律优先这个）

目录里只有 manifest + client.js/css，全部逻辑跑在页面里，调主程序已有的
通用 API（/api/sessions、文件、市场……）。

- **开发循环零编译**：client.js 由服务端磁盘直读（每请求现读文件），改完
  F5 即生效——比核心前端（web/，需 -webdir 或重编）还短
- **增删即目录级**：市场安装（装进 `组件/user/<id>/` + Rescan + 前端热加载）、
  删目录=卸载（设置 → 组件 → 用户组件 的删除按钮，或手动删；内置组件只能关
  开关）；开关状态在 data/components.json 自动管理

### B. 宿主半边组件——存量「套皮」形态（不再新增，仅存量维护）

**todo-scheduler、git-panel、dangerous-cmd-guard、skill-market、sites-platform**
属于这类：后端是主程序的内置功能（根目录 todo.go / gitpanel.go / perm.go /
market.go / sitesproxy.go 里的完整实现，深度耦合 Store/会话/事件总线），组件
目录里只有皮肤（client 三件套），开关只控制 UI 显隐——后端接口保留但入口
消失（skill-market 与 sites-platform 是开源前按用户要求从本体抽出来的：当年
赶工直接做进本体，抽组件时后端 /api/market 与 /sites 代理保留在主程序，
入口、页面与设置区随组件开关出现/消失）。

- 历史原因：写的时候桥还不存在，且它们与主程序状态耦合太深，**现在也认定
  不可拆**——把状态访问全部 IPC 化的代价大于收益，维持现状
- 套皮的价值是「可关闭」：确实有用户不需要待办/Git 面板，开关是真实需求
- **规矩：不再新增这种形态**。新功能若有后端需求，走 C 形态

### C. 宿主半边组件——自包含 worker 形态（目标形态，voice 是样板，经声明式桥运行）

**voice（语音）** 是这个形态的样板：组件目录自包含一切——

```
组件/voice/
  manifest.json      # 组件声明
  client.js / css    # 浏览器半边（UI、录音、流式协议）
  asr_worker.py      # 后端 worker（推理逻辑全在这，跑共享便携运行时）
  models/            # 模型文件（gitignore 大文件，随包分发/首启下载）
```

主程序里只保留**薄胶水**（voiceasr.go：拉起子进程 + 端点桥接），不含业务
逻辑。后端的开发迭代（换模型、改识别逻辑）**不需要动主程序**。

第二个实例：**session-title**（AI 自动会话标题）——纯钩子 worker 的极简形态：
不声明任何 endpoint（`endpoints: []`），只订阅 `run.end` 通知型钩子，全部动作
经反向调用主程序 `/api/`（读消息 / 独立调模型 / 改标题）完成，worker 自身无
状态、无密钥。业务全在组件里（提示词、改写节奏、钉住规则），主程序只提供
通用原子端点（`/api/llm/ask` 单轮模型调用、消息轻量列表、PATCH 钉住语义）。

## 声明式桥（已实现，componentbridge.go）

manifest 顶层声明 `workers` 数组，主程序通用桥按声明拉起子进程并把端点挂到
`/api/c/<组件id>/<path>`——后端可以是脚本或**预编译产物**（小 exe），市场直装
（zip 落盘即生效，免编译免主程序更新）。声明示例：

```json
"workers": [
  { "name": "asr",
    "command": ["{python}", "asr_worker.py"],
    "endpoints": [
      { "method": "POST", "path": "asr/transcribe", "type": "call" },
      { "method": "GET",  "path": "asr/stream",    "type": "ws" } ] }
]
```

- **占位符**：`{python}`=共享便携运行时（技能/_runtime）；`{dir}`=组件绝对目录；
  命令其余参数原样，相对脚本路径自动锚到组件目录
- **协议**：json-lines 一行一答（call=HTTP body 原样进 worker stdin、应答行原样
  返回；ws=WebSocket 每帧一行双向哑转发，断开发 `{"cmd":"stream_end"}` 收尾）；
  桥是哑管，协议内容归组件
- **生命周期**：懒启动（首请求才 spawn）、死亡自动重启、组件关闭/卸载即杀
  worker（重开懒启复活）；进程退出由顶层 kill-on-close Job 兜底
- **限制**：哑管 worker 单飞行（一次一条请求/一个 ws 会话）；帧模式 worker
  （见下节）按 id 解复用可交织；请求上限 32MB、call 超时 180 秒；市场服务端
  对 workers/hooks 声明的校验待硬化（当前未知字段放行，可发布）

## 可分发能力：模型经统一入口 `component_call` 调用（方案 A，硬规矩）

组件的**动态能力不进工具目录**。用户定调的原因：每个组件能力各占一个工具定义，
加一个能力就改一次工具目录——请求前缀抖动、缓存失效、多模式目录维护成本全部
上升。所以工具定义只装基础工具，组件能力走「**环境提示词声明 + 统一入口执行**」：

- **入口**：基础工具 `component_call`（`componentcall.go`），参数
  `component`（组件 id）+ `capability`（能力名）+ `args`（参数对象）。它**全模式
  注册**（task/coding/office/beginner-coding/skill-create/aux/assistant 都在），
  能不能调由能力声明决定。
- **声明**：组件 `manifest.json` 顶层 `capabilities` 数组，声明的才是**白名单**
  （没声明的能力宿主不给调）：

```json
"capabilities": [
  {
    "name": "memory_query",
    "description": "查询用户记忆的全部字段（进模型可见的能力清单，一句中文说清干什么）",
    "parameters": { "type": "object", "properties": {} },
    "modes": ["task", "office", "coding", "beginner-coding", "skill-create"]
  },
  {
    "name": "memory_update",
    "description": "把「用户身份与喜好」记忆段整体替换为给定内容",
    "parameters": {
      "type": "object",
      "properties": {
        "memory": { "type": "string", "description": "新的记忆段全文（上限 1000 字）" }
      },
      "required": ["memory"]
    },
    "modes": ["task", "office", "coding", "beginner-coding", "skill-create"],
    "writes": true
  }
]
```

  | 字段 | 含义 |
  |---|---|
  | `name` | 能力名，组件内唯一（`component_call` 的 `capability` 参数） |
  | `description` | 一句中文，进环境快照的「组件能力」块——模型据此挑能力，写清**干什么**而不是怎么实现 |
  | `parameters` | 参数 JSON Schema；只有 `properties` 的键与 `description` 会压成一行提示进能力行（宿主不对复杂 schema 建模，参数语义归组件实现）；缺省＝无参数 |
  | `modes` | 可用模式白名单，**空＝全部模式**。沿用原专用工具的门控口径：只读答疑（aux）、长期助理（assistant）通常不声明 |
  | `writes` | 写入型标记：计划模式下暂停（报错指路，与 write/edit 同理）；只读能力不受影响 |

- **实现**：宿主注册表。组件后端代码在主程序里（B/C 形态都行——B 形态就是
  主程序的组件文件），在该文件的 `init()` 里登记实现：

```go
func init() {
    registerComponentCapability("user-memory", "memory_query", userMemoryCapabilityQuery)
    registerComponentCapability("user-memory", "memory_update", userMemoryCapabilityUpdate)
}

func userMemoryCapabilityUpdate(t *toolContext, args json.RawMessage) (string, error) {
    // args 是模型给的参数 JSON（可能 "{}"）；返回值是给模型看的文本回执
}
```

- **模型怎么知道有这些能力**：`envPrompt` 的 `componentCapabilityBlock`
  （`componentcall.go`）按「组件启用（会话视角）∩ 声明 ∩ 当前模式」渲染
  「组件能力」块——组件名 + 能力清单 + 参数要点 + 调用方式。**组件开关只影响
  这一块**（环境快照差分送达，前缀缓存不动），工具定义恒定。
- **门控链**（`runComponentCall`，报错都指路）：组件存在 → 本会话启用
  （会话级覆盖优先）→ 能力已声明（否则列出该组件已声明的能力名）→ 本模式
  可用（否则列出可用模式）→ `writes` 能力的计划模式拦截 → 宿主已实现 → 执行。
- **接一个新组件能力只要三步**：①manifest `capabilities` 加一条声明 →
  ②宿主 `init()` 里 `registerComponentCapability` 登记实现 → ③构建后确认
  环境快照的「组件能力」块出现，`component_call` 能调通。**不改 `toolsFor`、
  不改任何工具定义**。
- 参照实现：`usermemory.go` 的 `memory_query` / `memory_update`（原
  `user_memory` 专用工具已退役，拆成这两条能力）；自检
  `usermemory_test.go` 的 `TestComponentCallWhitelist`（白名单/模式/计划模式）
  与 `TestComponentCallToolCatalogStable`（开关前后工具目录逐字一致）。

## 组件钩子（worker ⇄ 主程序后端的双向通道）

哑管只有「前端 → worker」的单向请求。worker 需要参与主程序流程——拦截对话、
收到生命周期事件、反查主程序状态——就在 worker 声明里加 `hooks` 订阅主程序
钩子事件。**声明了钩子的 worker 自动启用帧协议**，没声明的维持哑管（存量组件
零影响）：

```json
"workers": [
  { "name": "w",
    "command": ["{python}", "worker.py"],
    "hooks": ["prompt.submit", "run.end"],
    "hooksTimeoutMs": 5000,
    "endpoints": [{ "method": "POST", "path": "do", "type": "call" }] }
]
```

- **事件表**（拦截型要等 worker 的决定，通知型送达即止）：
  - `prompt.submit`（**拦截型**）：用户消息提交，`data={sessionID, content}`。
    发射点在 handleChat、CC 插件兼容层 UserPromptSubmit **之前**（本栈组件优先）。
    worker 回 decision 三选一（字段可组合）：`{"block":true,"reason":"…"}`
    拒绝（403+理由）、`{"content":"…"}` 改写正文、`{"additionalContext":"…"}`
    附加上下文（用户消息后落一条系统注入行发给模型，与 CC hook 同口径）。
    多组件按 id 序串行；任一拒绝即止；改写依次应用（后面的组件看到的是改
    过的正文）。
  - `run.end`（**通知型**）：一轮执行结束（含排队续跑全部完成），
    `data={sessionID, status: done|stopped|error, error}`。
- **帧协议**：stdin/stdout 变成带 id 的 JSON 帧，双向解复用——
  - 桥 → worker：`{"type":"req","id":…,"body":…}`（call 端点请求，body=原请求
    JSON；应答等 `{"type":"res","id":…,"body":…}`，body 原样回 HTTP——前端
    调用方式不变）、`{"type":"event","id":…,"event":…,"data":{…}}`（钩子事件）、
    `{"type":"hostres","id":…,"status":…,"body":…}`（反向调用应答）
  - worker → 桥：`res`／`decision`（决定字段见上）／`{"type":"host","id":…,
    "method":"GET|POST","path":"/api/…","body":{…}}`（反向调用主程序 API）
- **反向调用**：worker 处理 req/event 时可向主程序要数据（读会话、查状态……），
  仅限 `/api/` 前缀，权限同前端（本地信任模型——能调的 API 与页面里的一样）；
  需要收紧时再加 manifest 权限声明。桥侧 60 秒兜底。
- **桥向组件提供的通用服务**（反向调用即可用，全部组件共享）：
  - `POST /api/llm/ask`（llmapi.go）——**独立单轮模型调用**：body
    `{session?, system?, user}`，一次发送一次输出；不进任何会话上下文、不落
    消息表、不带工具。调用目标跟随 `session` 指定的会话（缺省全局默认），
    密钥与路由由主程序统一管理——**worker 组件不需要也不应该持有模型密钥**。
    浏览器半边（client.js）也可直接 fetch 同一端点。首个使用方：session-title
    （会话标题生成）；先例：git 面板提交摘要（gitSummarize 同模式）。
  - `GET /api/sessions/{id}/messages`——会话消息轻量文本列表
    （`role` 过滤如 `user`、`limit` 从最新往回收；插话消息 origin=interject
    默认排除——按「正常轮次」计数的辅助场景用）。
- **超时与 fail-open**：拦截型等决定上限 `hooksTimeoutMs`（缺省 5000、上限
  30000）；超时/进程死/帧解析错一律**按放行处理**并记日志——组件故障不能
  拦住发送等主流程。
- **为什么不做「代码注入主程序后端」**：Go 没有安全的运行时代码加载；内嵌 JS
  引擎（goja）那条路 CC 插件兼容层的 mods 已有，属兼容层范式，组件不沿用。
  组件统一「子进程 + 消息协议」：一套范式、可用预编译产物、市场免编译直装、
  崩了只死自己。
- **前端侧的钩子**：发送前收尾等可等待钩子用 `JX.hook(ev, fn)` 注册（区别于
  `JX.on` 的同步通知）：`send()` 组装正文前 `await JX.awaitHooks('send', …)`，
  逐个等待、异常隔离、总超时 10 秒放行；组件 dispose 里 `JX.unhook` 对称注销。
  样板：voice 组件（发送前等听写尾段落定）。
- 自动化验证：`componentbridge_hook_test.go`（帧协议 call、拦截决定、超时
  fail-open、反向调用、订阅匹配——走真实桥链路，无 Python 运行时自动跳过）。

**这样设计的目的（开源后的通用性）**：

- **对社区开发者**：组件代码各归各的、独立开发，避免人人都改主程序——
  主程序改动是冲突和组件不通用 的根源
- **对终端用户**：组件是编译好的产物 + 桥自动发现直接运行，不需要具备
  任何编译能力，装上就能用（不更新主程序）

## 界面插槽（标准定位格式）

界面上一块有条目的方寸（比如左侧栏底部那几个入口）= 宿主里的一个**插槽**。
组件只「申请一个格子」，**排版和形态全归插槽**——这样组件不用为电脑/手机各写
一套 CSS，也不会因为漏写某个 class 就让整排放歪。

```js
// 申请一个底部入口（图标传内联 svg 字符串，标签是纯文本）
const entry = JX.slot('sidebar.foot').add({
  key: 'my-tool',        // 唯一，重挂（热开关/热重载）按它替换
  id: 'btn-my-tool',     // 可选：需要给样式/外部代码取的元素 id
  order: 4,              // 序号小的在下面、大的往上叠（内置回收站占 1）
  label: '我的工具',
  icon: '<svg viewBox="0 0 24 24" …></svg>',
  title: '悬停提示',
  onClick: () => { /* 打开自己的页面 */ },
});
entry.setActive(true);   // 可选：选中态
entry.remove();          // 卸载时撤干净（dispose 里调）
```

契约（`web/app-core.js` 的 `JX.slot` + `web/style.css` 的插槽段）：

- 组件**不拼条目 DOM、不写条目布局 CSS**：骨架（`button.foot-btn > svg + 标签
  span`）、排序、去重都在插槽里。裸文本节点拿不到「最多两行、超出裁断」的规则，
  会在手机三栏格里自由换行把格子挤变形——所以标签一律交给插槽包 span。
- 两套形态由插槽按屏幕决定：**电脑＝竖排整行；手机＝三列均分网格**（3 个一行，
  4~6 个自动两行，格高固定、标签最多两行超出裁掉）。
- 序号方向：`order` 小的在最下面（紧贴设置行），大的往上叠；同序号先注册的更靠下。
- 现有插槽：`sidebar.foot`（左侧栏底部入口格）。右栏页签是同一模式的既有实现
  （`app-right.js` 的 `rpRegister`）。
- 自检：控制台敲 `JX.slots()` 看每个插槽里有哪些条目、谁注册的、序号多少。

## 开发规矩（硬约束）

1. 新组件默认 A 形态；确需系统能力才上 C 形态；B 形态只存量维护
2. C 形态的后端逻辑放组件目录（worker + 资产），主程序只允许薄胶水；
   状态深耦合主程序的功能不算组件，算主程序内置功能
3. 运行时**共享**技能/_runtime（不每组件自带，除非有特定版本需求）
4. 模型等重资产归组件目录、gitignore，随安装包分发或首启下载
5. 用户端文案不出现模型/厂商名（内部型号与下载链接写在代码注释里）
6. 往界面上加条目一律走插槽（`JX.slot(...).add`），不自己拼 DOM、不锚定别人的按钮
7. **组件自己的偏好存 `localStorage['cx:<组件id>:*']`**：宿主的会话级配置合并层
   就拦这一层命名空间——当前对话「会话独立」时，读命中会话覆盖、未覆盖的键回落
   全局值，写只落会话覆盖（**组件不用改一行代码**）。存到别处（服务端全局配置、
   其它前缀）的设置不随对话，只算全局。写的时候注意：init 里无条件写默认值会被
   记成一次会话覆盖——按「读不到才给默认值、用户改了才写」的常规写法即可。
8. **目录归属即归属**：写进 `组件/<id>/` 就是内置组件（随包分发、不可删除、
   随程序热更新）；只有面向用户单独发行、允许用户自己删掉的才放
   `组件/user/<id>/`（不入库、不进包、不热更新）。不要把内置功能偷偷写进 user 区
   绕过打包校验，也不要往内置区放「市场才该装」的组件
