# AI 发话一律插话——改动说明与验证步骤

> 时间：2026-11-01　提交：`2a983b7c20`
> 背景：此前只有数字经理的 `send_to_session` 改成了插话；子对话→主对话的【派发回报】、
> 任务求助、待办转交仍按 `jx.midrun` 偏好（默认排队）投递，导致回报不及时、
> 发话传不上话。本次把 AI 侧投递全部统一为插话，并删除因此不再被引用的
> 「服务端 midrun 偏好记忆」死代码。

## 一、改了什么

### 1. AI 侧投递一律插话（runtime.go）

- `deliverToSessionMode`（带 forcedMode 参数）删除，与 `deliverToSession` 合并为一个：
  目标执行中一律 `pushPending(…, "steer", …)`——注入对方正在执行的这一轮，下一次
  调用模型就能看到；空闲照旧落库开跑。
- `deliverToSessionNoKick`（人为叫停回报专用）：忙时同样固定插话（空闲只落库、
  不拉起的语义不变）。
- `subQueueNote` 删掉 queue 分支，只保留插话文案。
- 受影响的 AI 投递点全部自动生效：
  - 【派发回报】（runSubAgent → 父会话，子对话回复不及时的根因）
  - 【数字助理转达/派发】（deliverSub：dispatch_task / dispatch_sub / send_to_session）
  - 【任务求助】（requestmgr.go → 数字经理）
  - 【待办任务派发】（todo.go → 经理 / 新任务会话）
  - 计划审批结果注入（interact.go resolvePersistedPlan）

### 2. 死代码清理

- 删 `midrunPref` 字段（server.go App 结构体）、`noteMidrunPref` / `setMidrunMode` /
  `midrunMode`（runtime.go）——它存在的唯一理由是「派发注入按前端偏好」，派发
  一律插话后无人引用。
- server.go：handleChat / handleInterject 不再解析、上报 `midrun` 字段。
- 前端 app-chat.js：两处请求体删掉 `midrun` 上报（用户自己的「对话中途发送行为」
  设置**保留**，它只控制用户自己执行中发消息的模式；`jx.midrun` localStorage 键
  与设置项不变）。老版本前端继续上报该字段也会被后端忽略，向后兼容。

### 3. 保留不动的

- 排队队列与排队坞（用户功能：排队消息自动续发、暂停/继续、拖拽排序、编辑）。
- `handleInterject` 的 mode=queue/steer（用户自己的选择，默认排队）。
- 重启中断回报（runowner.go 只落库不投递，原本就不走这条路径）。
- CC 插件 modSubmitPrompt（外部插件通道，保持「排队为独立一轮」原语义）。

### 4. 测试与文档

- runtime_queue_test.go：改为验证「AI 投递一律 steer 进插话队列、用户排队续发
  不回归、回报经插话注入父会话」；request_manager_test.go / orchestration_test.go
  断言同步更新。
- AGENT.md：send_to_session、派发回报、任务求助、requestmgr 模块表四处描述更新，
  并在「执行中发消息」设置段注明该设置只属于用户自己，AI 发话一律插话。

## 二、验证步骤

1. `go build ./...`、`go vet .` 通过。
2. 针对性测试（全部通过）：
   `go test -run 'TestQueueDrainSendsAllSources|TestQueueAutoSendSwitchOff|TestFinishRunResumesLeftoverQueue|TestSubAgentReportGoesThroughQueue|TestSteerInjectBroadcastsEmptyPending|TestRequestManager|TestSendToSession|TestStopSession|TestDispatchSub|TestDispatchTask|TestTodoDispatch' .`
3. 实际效果（跑起来看）：
   - 主对话执行中派发一个子任务，等它完成——【派发回报】应立刻出现在主对话
     正在跑的这一轮上下文里（不再等本轮结束排队发出）。
   - 任务会话里调 request_manager，经理正在忙时工具返回「已插话注入它当前这一轮」，
     且经理侧栏插话气泡立即出现。
   - 用户自己在执行中的会话发消息：设置选「等待排队」仍进排队坞（行为不变）。
