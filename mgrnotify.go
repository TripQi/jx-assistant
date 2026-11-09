package main

// 「审批/提问的经理代理」：经理触达过的会话里，审批卡/提问卡（含计划审批）
// 挂出 mgrNotifyAfterSec 秒（默认 30，设置 mgrNotify）仍无人处理时，自动给
// 数字经理主对话投一条通知——会话标题/id、卡片 id、问题或申请摘要、可选项
// 与 call_api 代批路径齐全；之后无论谁给出决议（用户在界面选 / 等待超时 /
// 经理经 call_api 代选）都回告经理一条，并标注来源（经理自己选的标「你已
// 代选」）。与「久等声光提醒用户」（askAlert，默认 4 分钟唤窗口+叮声+盖屏）
// 并行各管各：一个叫醒用户、一个叫醒经理，互不替代、互不干扰。
//
// 通知范围只限「经理触达过的会话」（防骚扰，用户自建且经理从未碰过的对话
// 不通知）：
//   - dispatch_task 派生：会话天生带 DispatchedBy（与回报链同源，直接当依据）；
//     待办调度器的系统派生（todoComponentID）除外——它明确不打扰数字经理；
//   - send_to_session 发过话：首次发话时补一条会话附属数据标记（mgrLinkKey）。
//     转达消息本身（origin=task）与普通会话的 dispatch_sub 投递无法区分，
//     只能在经理发话那一刻标。
//
// 通知/回告的投递与【任务求助】（requestmgr.go）同路：deliverToSession 统一
// 收口，忙则插话、空闲落库。差别在唤醒：通知要经理行动，空闲时唤醒它跑一轮；
// 回告只告知结果，空闲时只落库不唤醒（与人为叫停的回报同一考量，避免为一句
// 「已处理」白跑一轮模型）。

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// mgrLinkKey 会话附属数据键（sessdata:mgrlink:<会话id>）：send_to_session
// 首次发话补标「经理关联」。值固定 "1"——只表达存在性，不记哪一版经理
//（版本会切换，触达关系不该跟着丢）。
const mgrLinkKey = "mgrlink"

// mgrNotifyOrigin 经理代理通知与决议回告的消息来源标记（kind=aiuser）。
const mgrNotifyOrigin = "mgrcard"

// managerLinked 判定一个会话是否「经理触达过」。经理主对话自己不算（不会
// 通知自己）；待办调度器的系统派生不算（它的派生标记本就注明不打扰数字经理）。
func (a *App) managerLinked(m *SessionMeta) bool {
	if m == nil || m.Mode == "assistant" {
		return false
	}
	if m.DispatchedBy != "" && m.DispatchedBy != todoComponentID {
		return true
	}
	return a.Store.GetSessionData(m.ID, mgrLinkKey) != ""
}

// markManagerLinked 给会话补「经理关联」标记（幂等：重复发话写同值）。
func (a *App) markManagerLinked(m *SessionMeta) {
	_ = a.Store.SetSessionData(m.ID, mgrLinkKey, "1")
}

// mgrNotifyDelay 通知触发延时（设置的 mgrNotifyAfterSec，默认 30 秒）。包级
// 函数变量只为测试能换短延时（mgrnotify_test.go），产品路径永远是设置值——
// 与 approvalAlertDelay（perm.go）同一做法。
var mgrNotifyDelay = func(a *App) time.Duration {
	return time.Duration(a.Cfg.mgrNotifyAfterSec()) * time.Second
}

// notifyManagerOfApproval / notifyManagerOfQuestion 通知到点的发出体：还在等、
// 没人给过决定（提问卡还要求没停表——用户碰过卡片=有人在场，与声光提醒的
// askAlertDue 同口径）才发。判定与置位（notified）同锁完成：同一张卡只通知
// 一次，已有决议的卡到点不发。
func (a *App) notifyManagerOfApproval(id string) {
	a.permMu.Lock()
	ap, ok := a.approvals[id]
	if ok && ap.decided {
		ok = false
	}
	if ok {
		ap.notified = true
	}
	a.permMu.Unlock()
	if !ok {
		return
	}
	a.deliverManagerNotice(ap.SessionID, mgrApprovalNotice(ap, askAlertDurText(a.Cfg.mgrNotifyAfterSec())))
}

func (a *App) notifyManagerOfQuestion(id string) {
	a.permMu.Lock()
	q, ok := a.questions[id]
	if ok && (q.decided || q.TimerHeld) {
		ok = false
	}
	if ok {
		q.notified = true
	}
	a.permMu.Unlock()
	if !ok {
		return
	}
	a.deliverManagerNotice(q.SessionID, mgrQuestionNotice(q, askAlertDurText(a.Cfg.mgrNotifyAfterSec())))
}

// deliverManagerNotice 投一条要经理行动的通知：只投经理触达过的会话；经理
// 主对话不存在时建出来（要它处理，值得建）；忙则插话，空闲落库并唤醒。
func (a *App) deliverManagerNotice(sessionID, text string) {
	m, err := a.Store.Get(sessionID)
	if err != nil {
		return // 会话已删：无从判定关联，不打扰
	}
	if !a.managerLinked(m) {
		return
	}
	mgr, err := a.assistantSession()
	if err != nil {
		log.Printf("[经理代理] 找不到/建不出数字助理主对话，通知未发出: %v", err)
		return
	}
	d := a.deliverToSession(mgr, text, kindAiUser, mgrNotifyOrigin)
	if d.Started {
		kickSessionRound(a, mgr, d.Run)
	}
}

// deliverManagerReport 投决议回告：只落库不唤醒——经理在忙则插话可见，
// 空闲则用户下次与它对话时自然看到，不为一句结果白跑一轮模型。
func (a *App) deliverManagerReport(text string) {
	mgr, err := a.assistantSession()
	if err != nil {
		log.Printf("[经理代理] 找不到/建不出数字助理主对话，决议回告未发出: %v", err)
		return
	}
	a.deliverToSessionNoKick(mgr, text, kindAiUser, mgrNotifyOrigin)
}

// ---------- 决议来源标注（谁点的，回告里说清） ----------

// managerResolveCaller 判断这条决定请求是否数字经理自己经 call_api 代批的：
// 直调请求带调用者会话头（runCallAPI 注入），与当前经理主对话相符才算。
// 前端按钮与远程转发都不带这个头，一律算用户。这只是回告的来源标注，不是
// 鉴权——call_api 自己已过目录/权限/危险三道闸。
func managerResolveCaller(a *App, r *http.Request) bool {
	sid := r.Header.Get("X-JX-Caller-Session")
	if sid == "" {
		return false
	}
	cur := a.currentAssistant()
	return cur != nil && cur.ID == sid
}

// markApprovalByManager / markQuestionByManager 把一张卡标注为「经理代选」：
// 决议 handler（server.go）在登记决定之前调用，等待方醒来读到的就是已标注
// 的值（登记决定本身在 permMu 之外投 ch，标注先于它完成）。
func (a *App) markApprovalByManager(id string) {
	a.permMu.Lock()
	if ap, ok := a.approvals[id]; ok && !ap.decided {
		ap.byManager = true
	}
	a.permMu.Unlock()
}

func (a *App) markQuestionByManager(id string) {
	a.permMu.Lock()
	if q, ok := a.questions[id]; ok && !q.decided {
		q.byManager = true
	}
	a.permMu.Unlock()
}

// ---------- 决议回告（perm.go / interact.go 的等待收尾处调用） ----------

// reportApprovalOutcome 审批卡的决议回告。只有通知过经理（notified）的卡才
// 回告——没通知过的卡经理不知情，回告反而是骚扰。停止（ctx 取消）不回告：
// 整个会话都停了，经理自会收到带中断原因的【派发回报】。
func (a *App) reportApprovalOutcome(ap *Approval, approved, always, timedOut bool) {
	a.permMu.Lock()
	notified, byMgr := ap.notified, ap.byManager
	a.permMu.Unlock()
	if !notified {
		return
	}
	outcome := "等待超时未获批准（已按拒绝收卡）"
	switch {
	case timedOut:
	case approved && always:
		outcome = "已批准（始终允许：本会话后续同类免审）"
	case approved:
		outcome = "已批准"
	default:
		outcome = "已拒绝"
	}
	a.deliverManagerReport("【审批结果】会话「" + ap.SessionTitle + "」（" + ap.SessionID +
		"）的审批卡（" + ap.ID + "）已有结果：" + outcome + "。" + mgrOutcomeSourceLine(byMgr, timedOut))
}

// reportQuestionOutcome 提问/计划审批的决议回告（口径同审批）。
func (a *App) reportQuestionOutcome(q *UserQuestion, ans string, timedOut bool) {
	a.permMu.Lock()
	notified, byMgr := q.notified, q.byManager
	a.permMu.Unlock()
	if !notified {
		return
	}
	outcome := "等待超时未获回答（提问按超时收卡）"
	if !timedOut {
		outcome = "回答「" + mgrAnswerDisplay(ans) + "」"
	}
	a.deliverManagerReport("【提问结果】会话「" + q.SessionTitle + "」（" + q.SessionID +
		"）的提问卡（" + q.ID + "）已有结果：" + outcome + "。" + mgrOutcomeSourceLine(byMgr, timedOut))
}

// mgrOutcomeSourceLine 决议来源的回告口径：经理自己选的标「你已代选」；
// 超时是没人处理，不算谁的决议，不落来源。
func mgrOutcomeSourceLine(byManager, timedOut bool) string {
	if timedOut {
		return ""
	}
	if byManager {
		return "来源：你已代选。"
	}
	return "来源：用户在界面处理。"
}

// mgrAnswerDisplay 提问答案的回告展示：计划审批的两个哨兵值翻成人话，
// 其余答案原文（截 200 字防刷屏）。
func mgrAnswerDisplay(ans string) string {
	switch ans {
	case "__approve__":
		return "已批准（按计划执行）"
	case "__ignore__":
		return "已搁置（计划仍留在对话里等处置）"
	}
	return truncateRunes(ans, 200)
}

// ---------- 通知文案 ----------

// mgrApprovalNotice 审批卡的通知正文：申请与理由摘要 + 代批路径（卡片 id
// 直接给到，经理照着 POST 即可）。afterText 是等待时长的读法（如「30 秒」）。
func mgrApprovalNotice(ap *Approval, afterText string) string {
	b := &strings.Builder{}
	b.WriteString("【审批代理】会话「" + ap.SessionTitle + "」（" + ap.SessionID +
		"）的审批卡挂了 " + afterText + " 没人处理，请你过目：\n")
	b.WriteString("- 卡片：审批 " + ap.ID + "\n")
	if ap.Tool != "" {
		b.WriteString("- 动作：" + ap.Tool + "\n")
	}
	if ap.Summary != "" {
		b.WriteString("- 申请：" + truncateRunes(ap.Summary, 200) + "\n")
	}
	if ap.Reason != "" {
		b.WriteString("- 理由：" + truncateRunes(ap.Reason, 200) + "\n")
	}
	b.WriteString("\n需要你代批时用 call_api：GET /api/approvals 查清单；POST /api/approvals/" + ap.ID +
		`（params {"approved":true} 批准 / {"approved":false} 拒绝；always=true 为本会话后续同类免审）。` +
		"用户仍可能随时处理，谁先点都算数；决议出来会自动回告你。")
	return b.String()
}

// mgrQuestionNotice 提问/计划审批卡的通知正文：问题、可选项（计划审批附计划
// 摘要）+ 代答路径（计划审批的哨兵答案一并说明）。
func mgrQuestionNotice(q *UserQuestion, afterText string) string {
	kindText := "提问"
	planHint := "答案直接写选项原文，或按问题自由作答。"
	if q.Kind == askKindPlan {
		kindText = "计划审批"
		planHint = "批准填 __approve__、搁置填 __ignore__，也可直接给修改意见文本。"
	}
	b := &strings.Builder{}
	b.WriteString("【提问代理】会话「" + q.SessionTitle + "」（" + q.SessionID + "）的" +
		kindText + "卡挂了 " + afterText + " 没人回答，请你过目：\n")
	b.WriteString("- 卡片：" + kindText + " " + q.ID + "\n")
	b.WriteString("- 问题：" + truncateRunes(q.Question, 300) + "\n")
	if len(q.Options) > 0 {
		b.WriteString("- 可选项：" + strings.Join(q.Options, " / ") + "\n")
	}
	if q.Kind == askKindPlan && q.Plan != "" {
		b.WriteString("- 计划（前 400 字）：" + truncateRunes(q.Plan, 400) + "\n")
	}
	b.WriteString("\n需要你代答时用 call_api：POST /api/questions/" + q.ID + `（params {"answer":"..."}）；` +
		planHint + "用户仍可能随时回答，谁先答都算数；决议出来会自动回告你。")
	return b.String()
}
