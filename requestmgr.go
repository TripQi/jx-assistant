package main

// 任务侧「找经理帮忙」通道（request_manager）：把一条求助/协调消息投给数字助理
// 主对话，**立即返回**——任务不等答复，经理按自己的判断用自己的老办法处理
//（send_to_session 插话/追加需求、dispatch_task 派新任务，或者先不动，等资源
// 空出来再安排）。
//
// 为什么是「投一条消息」而不是新开一条等待通道：
//   - 投递走既有唯一收口 deliverToSession（空闲落库并占执行权、忙则插话注入
//     它正在执行的那一轮——AI 发话一律插话，不排队），与【派发回报】【待办任务
//     派发】完全同路，不开会话间旁路；
//   - 消息本身即记录（kind=aiuser、origin=help）：搜得到、导得出、重启也在，
//     所以不需要求助池、不需要新接口，也不需要界面改动；
//   - 没有阻塞等待，就不存在「任务停在那等经理、经理又在等任务」的互相等死，
//     也不给任务侧添超时/兜底这类必须写对的参数；
//   - 经理侧不需要新工具：它已经有 send_to_session / dispatch_task，
//     「回不回、什么时候回、怎么回」由它按全局情况决定（准则见 agent.go 的
//     managerAskPrompt 与 managerCoordPrompt 两段提示词）。
//
// 设计边界（与既有约定一致）：
//   - 只有顶层任务会话能用：子对话拿到工具也调不通（调用时硬拦，与 dispatch_sub
//     的「子对话只有一级」同口径）；assistant / aux 模式根本不注册本工具；
//   - 求助消息只带文本，不携带任何权限或指令——经理后续动作仍受它自己的权限档
//     与用户审批约束，这条通道不构成提权路径，也不替代用户审批；
//   - 计划模式暂停（与 write / run_command 同列，见 agent.go 的 planSuspendedTask）。
// 「谁有这条通道」由启动自检 assertManagerChannel 逐模式看护（见本文件末尾）。

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
)

// 求助消息的长度上限（防呆，不是防滥用：模型真写长了也只截断不报错）。
const (
	reqTopicCap       = 120
	reqDetailCap      = 4000
	reqResourceCap    = 200
	reqExpectationCap = 300
)

// requestManagerTool 构造任务侧的求助工具（desc 见下，静态文案：不随环境变化，
// 保证工具目录逐字节稳定）。
func requestManagerTool() Tool {
	desc := "给长期助理（数字助理主对话）发一条求助/协调消息。它管着全局：别的会话在干什么、谁改了哪些文件，它知道；“同一时间只能一个人用”的东西（跑测试、编译、发布、占端口或设备），由它排先后。这两类事你自己看不到，找它就对了。\n" +
		"用得着的场合：\n" +
		"- 想知道自己看不到的情况：别的会话正在改哪些文件、改到哪一步了、现在合不合适轮到你动手——比如代码最后要统一做测试，而你不知道别的会话改完没有；\n" +
		"- 要用一个“同时只能一个人用”的资源：跑测试、编译、发布、占端口或设备——几个会话要一起排先后时，找它排。\n" +
		"其余的事自己做或派子对话（dispatch_sub）：查资料、查网页、做验收测试、自己权限内能办的事，都不用找经理。\n" +
		"发完就继续干，不用等：经理会按自己的判断处理——可能用插话回你一句，可能派新任务，也可能先不动，等资源空出来再安排。等它给你消息时，你自然就看到了。\n" +
		"参数：\n" +
		"- topic：一句话标题\n" +
		"- detail：你要做什么、卡在哪、涉及哪些文件或命令\n" +
		"- resource：可选，要协调的东西（如「仓库 D:\\x 的跑测试」「发布到生产」）\n" +
		"- expectation：可选，你希望它怎么帮你（给个判断 / 放行 / 排个顺序）\n" +
		"这是求助用的，不要滥用。"
	return Tool{
		Spec: toolSpec("request_manager", desc,
			objSchema(map[string]any{
				"topic":       strProp("一句话标题（经理和用户在清单/气泡里第一眼看到的就是它）"),
				"detail":      strProp("你要做什么、卡在哪、涉及哪些文件或命令（两三句话，写清背景，经理看不到你的对话记录）"),
				"resource":    strProp("可选：要协调的东西，如「仓库 D:\\x 的跑测试」「发布到生产」「端口 8080」；没有就不填"),
				"expectation": strProp("可选：你希望它怎么帮你（给个判断 / 放行 / 排个顺序）"),
			}, "topic", "detail")),
		Run: runRequestManager,
	}
}

// runRequestManager 把求助整理成一条消息投给长期助理。
//
// 投递结果分两种，都是「立即返回」：经理空闲 → 消息落库并占住它这一轮，
// 顺手唤醒它跑（与本产品所有唤醒路径同一套机制）；经理正忙 → 消息插话注入
// 它正在执行的这一轮，下一次调用模型就能看到。
func runRequestManager(t *toolContext, args json.RawMessage) (string, error) {
	var p struct {
		Topic       string `json:"topic"`
		Detail      string `json:"detail"`
		Resource    string `json:"resource"`
		Expectation string `json:"expectation"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return "", err
	}
	topic := strings.TrimSpace(p.Topic)
	detail := strings.TrimSpace(p.Detail)
	if topic == "" {
		return "", fmt.Errorf("topic 不能为空：一句话说清你要协调什么")
	}
	if detail == "" {
		return "", fmt.Errorf("detail 不能为空：写清你要做什么、卡在哪、涉及哪些文件或命令（经理看不到你的对话记录）")
	}
	// 子对话只有一级、只做交给它的那一件事：人工把总控克隆成子对话后工具还在手里，
	// 这里硬拦（与 dispatch_sub / dispatch_task / send_to_session 的子对话拦截同口径）。
	if t.sess.ParentID != "" {
		return "", fmt.Errorf("子对话不找经理协调（你只做交给自己的这一件事）：把要协调的事写进你的答复，由父会话或用户去安排")
	}
	mgr, err := t.app.assistantSession()
	if err != nil {
		return "", fmt.Errorf("找不到/建不出数字助理主对话，没法转达：%w", err)
	}
	if mgr.ID == t.sess.ID {
		return "", fmt.Errorf("本会话就是数字助理主对话，不用找自己")
	}
	text := formatManagerRequest(t.sess, topic, detail,
		strings.TrimSpace(p.Resource), strings.TrimSpace(p.Expectation))
	d := t.app.deliverToSession(mgr, text, kindAiUser, "help")
	if !d.Started {
		return "已发给长期助理（它正忙，这条已插话注入它当前这一轮，它下一次调用模型就能看到）。你继续干自己的，不用等它。", nil
	}
	kickSessionRound(t.app, mgr, d.Run)
	return "已发给长期助理（会话 " + mgr.ID + "），它会按自己的判断处理——可能给你插一句话、可能派新任务，也可能先不动，等资源空出来再安排。你继续干自己的，不用等它。", nil
}

// formatManagerRequest 组织落到经理会话里的那条求助消息。开头带固定前缀
// 【任务求助】+ 发起会话标题与 id（经理据此用 send_to_session 找得到人），
// 末尾一句提示经理「不必当场回」（它也可能早就知道该怎么办）——口径与
// 【派发回报】末尾那句 followUp 同一做法。
func formatManagerRequest(sess *SessionMeta, topic, detail, resource, expectation string) string {
	b := &strings.Builder{}
	b.WriteString("【任务求助】会话「" + sess.Title + "」（" + sess.ID + "）来找你协调：\n\n")
	b.WriteString("主题：" + truncateRunes(topic, reqTopicCap) + "\n")
	b.WriteString("情况：" + truncateRunes(detail, reqDetailCap) + "\n")
	if resource != "" {
		b.WriteString("要协调的资源：" + truncateRunes(resource, reqResourceCap) + "\n")
	}
	if expectation != "" {
		b.WriteString("它希望你：" + truncateRunes(expectation, reqExpectationCap) + "\n")
	}
	b.WriteString("\n（它发完就继续干、不等答复——不必当场回：你可以用 send_to_session 给它或相关会话插一句话、" +
		"也可以等资源空出来再派新任务，或者先不动。）")
	return b.String()
}

// kickSessionRound 唤醒一个「已被投递占住执行权」的会话跑一轮：调用方从
// deliverToSession 拿到 Started=true 的执行权后交到这里，由它起执行体。
//
// 与 runtime.go 的 runSubAgent（派发回报唤醒父会话）和 todo.go 的
// handToAssistant（待办转交唤醒经理）两处唤醒体同构：订阅流按会话 id 归位、
// 正常收尾后补发 done（未查看绿点）、异常只记日志（报错原因已在会话上报错条
// 里，用户看得见）。刻意不动那两处——它们各自的事件细节略有差异（待办那条
// 不发 run_start/done），合并留待专门的重构，本次只服务求助通道。
// assertManagerChannel 启动自检（NewApp 调用）：request_manager 的在场/缺席必须
// 与模式定位一致——五个执行模式必须有（发起方），assistant / aux 必须没有
//（数字助理是接收方，它用既有的 send_to_session / dispatch_task 处理）。漏挂或
// 错挂都直接拒绝启动：比模型运行期撞上「工具不存在」早得多，也明确得多。
func (a *App) assertManagerChannel() {
	shouldHave := map[string]bool{
		"task": true, "office": true, "coding": true, "beginner-coding": true, "skill-create": true,
	}
	for _, mode := range sessionModes {
		has := false
		for _, tl := range a.toolsFor(mode) {
			if tl.Spec.Function.Name == "request_manager" {
				has = true
				break
			}
		}
		if shouldHave[mode] && !has {
			panic("求助通道自检失败：模式 " + mode + " 的工具目录里没有 request_manager（tools.go 的 toolsFor 漏挂）")
		}
		if !shouldHave[mode] && has {
			panic("求助通道自检失败：模式 " + mode + " 不该有 request_manager（它是发起方的能力，不是接收方的）")
		}
	}
}

func kickSessionRound(a *App, m *SessionMeta, run *sessionRun) {
	go func() {
		defer a.finishRunOf(m.ID, run)
		run.publish(Event{Type: "run_start"})
		err := a.runRoundsWithQueue(m, run, func(ev Event) {
			ev.SessionID = m.ID
			run.observe(ev)
			run.publish(ev)
		}, true, a.queueAutoSendOn())
		if err == nil {
			run.markCleanEnd() // 正常收尾：收口时兜底续发窗口期内新挤进队列的消息
		} else {
			log.Printf("[求助] 唤醒会话 %s 的这一轮执行失败: %v", m.ID, err)
		}
		a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
		a.broadcast(Event{Type: "done", SessionID: m.ID})
	}()
}
