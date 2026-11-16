package main

// 「审批/提问的经理代理」：经理（长期助理主对话）投递开启的那一轮执行里，
// 审批卡/提问卡（含计划审批）挂出 mgrNotifyAfterSec 秒（默认 30，设置
// mgrNotify）仍无人处理时，自动给经理投一条通知——会话标题/id、卡片 id、
// 问题或申请摘要、可选项与 call_api 代批路径齐全；之后无论谁给出决议（用户
// 在界面选 / 等待超时 / 经理经 call_api 代选）都回告经理一条，并标注来源
//（经理自己选的标「你已代选」）。与「久等声光提醒用户」（askAlert，默认
// 4 分钟唤窗口+叮声+盖屏）并行各管各：一个叫醒用户、一个叫醒经理，互不替代、
// 互不干扰。
//
// 代批资格是**轮次级授权**（mgrTurnGrant），不是会话级永久标记：只有
// dispatch_task / send_to_session 把需求注入会话、投递**开出的那一轮**才授予；
// 该轮收口（正常完成/失败/停止，含看门狗硬终结）或用户亲发消息（接管）即撤销。
// 谁能来代决也按身份卡：请求带 X-JX-Caller-Session 就是某个 AI 会话在调
//（有头即 AI），只有主对话自己、以及 parent 链落到主对话的派生子对话走这套
// 轮次授权；其余 AI 会话一律 403，绝不回落成「用户语义」放行（2026-11 踩坑：
// 旧判定只认与主对话逐字相等的调用者，经理派发的子对话带自己的会话 id 被当成
// 用户，可代决任意会话的卡）；不带头的才是真用户（界面按钮直连），照常可用。
// 用户自己发起的轮、经理发过话之后用户接手的会话，卡片一律不通知经理，经理
// 代决请求在决议入口被拒——绝不代决不属经理那一轮的卡片。（2026-11 踩坑：曾按
// 「会话级永久标记」实现——dispatch_task 派生记 DispatchedBy、send_to_session
// 首次发话补 mgrlink 持久化标记——经理派发过一次，之后用户在该会话自己发起的
// 全部工作，审批卡仍报给经理代决，被用户判为越权，遂改轮次级。旧的 mgrlink
// 持久化数据已无读取者，留库不动也不生效。）窗口外的卡按降噪口径不通知（用户
// 在场的轮用户自己看得见卡片），经理经 call_api 翻清单主动代决的由决议入口拒绝。
//
// 通知/回告的投递与【任务求助】（requestmgr.go）同路：deliverToSession 统一
// 收口，忙则插话、空闲落库。差别在唤醒：通知要经理行动，空闲时唤醒它跑一轮；
// 回告只告知结果，空闲时只落库不唤醒（与人为叫停的回报同一考量，避免为一句
// 「已处理」白跑一轮模型）。

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// mgrNotifyOrigin 经理代理通知与决议回告的消息来源标记（kind=aiuser）。
const mgrNotifyOrigin = "mgrcard"

// ---------- 轮次级授权（mgrTurnGrant） ----------

// mgrTurnGrant 经理代批的轮次级授权（App.mgrTurns，runMu 保护）：run 是授权
// 针对的那一轮执行体——指针即轮次身份（与 finishRunOf 按身份收口同一哲学），
// 该轮收口、换了新一轮，授权自然失效；gen 是开轮时的撤销代数快照——用户亲发
// 消息会递增代数并清授权，「投递已开出新轮、授权还没写、用户恰好在此刻插话」
// 竞态窗口里晚到的旧授权因 gen 落后自动作废。
type mgrTurnGrant struct {
	run *sessionRun
	gen int64
}

// grantManagerTurn 授予经理代批资格（dispatch_task / send_to_session 把需求注入
// 会话时调用）：只对投递开出的那一轮有效（run=nil 是插话——注入的是别人正在
// 跑的那一轮，不是经理发起的，不授权）。插话补充需求不撤销也不建立授权：
// 这一轮是谁发起的就按谁的资格走到底。
func (a *App) grantManagerTurn(sid string, r *sessionRun) {
	if r == nil {
		return
	}
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if a.runners[sid] != r || r.mgrTurnGen != a.mgrTurnGen[sid] {
		return // 只授权仍在执行且开轮后未被用户接管的原轮，不能用当前代数补发旧授权
	}
	a.mgrTurns[sid] = mgrTurnGrant{run: r, gen: r.mgrTurnGen}
}

// revokeManagerTurn 撤销经理代批资格（用户亲发消息=用户接管：handleChat /
// handleInterject / handleRetry）。代数递增让竞态窗口里晚到的旧授权作废。
func (a *App) revokeManagerTurn(sid string) {
	a.runMu.Lock()
	a.mgrTurnGen[sid]++
	delete(a.mgrTurns, sid)
	a.runMu.Unlock()
}

// dropManagerTurnOf 轮收口时按身份清授权（closeRun 调用，正常完成/失败/停止/
// 硬终结都走那里）：只清属于这一轮的条目——收口与下一次投递授权并发时，
// 不能误删已指向新一轮的授权。
func (a *App) dropManagerTurnOf(sid string, r *sessionRun) {
	a.runMu.Lock()
	if g, ok := a.mgrTurns[sid]; ok && g.run == r {
		delete(a.mgrTurns, sid)
	}
	a.runMu.Unlock()
}

// managerTurnActive 会话当前正在执行的那一轮是否由经理投递开启（经理代批
// 窗口）。授权指向的执行体还活着才算：轮已收口、换了新一轮、被用户接管或
// 从没授权过都不算。审批/提问卡的生命周期都在轮内（挂卡的等待就阻塞在那一轮
// 的工具执行里），所以「授权的那一轮正在跑」就等价于「卡片产生于经理那一轮」。
func (a *App) managerTurnActive(sid string) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	return a.managerTurnActiveLocked(sid)
}

// managerTurnActiveLocked 由持有 runMu 的调用方使用。
func (a *App) managerTurnActiveLocked(sid string) bool {
	g, ok := a.mgrTurns[sid]
	if !ok || g.gen != a.mgrTurnGen[sid] {
		return false
	}
	cur, running := a.runners[sid]
	return running && cur == g.run
}

// cardSession 取一张等待中的卡所属的会话 id（审批/提问共用；卡片不在等待
// 清单返回空串——已收卡，代决无从谈起）。
func (a *App) cardSession(isApproval bool, id string) string {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	if isApproval {
		if ap, ok := a.approvals[id]; ok {
			return ap.SessionID
		}
		return ""
	}
	if q, ok := a.questions[id]; ok {
		return q.SessionID
	}
	return ""
}

// managerResolveAllowed 经理此刻是否有权代决这张卡：卡片所属会话的经理轮授权
// 仍有效（同一轮还在执行）。
//
// 决议入口的第一道门（server.go 的 handleResolveApproval / handleResolveQuestion）：
// 通知已按降噪口径不投窗口外的卡，经理经 call_api 翻到清单主动代决同样不能
// 放行——越权的代价是替用户批准一次它没看见的越权操作。真正生效的裁决仍是
// commitManagerDecisionLocked（授权复核与提交同在一个撤销边界内，无 TOCTOU 窗口）；
// 这道门只是先一步把明显的越权挡在决议之前，并给出明确的 403 语义。
func (a *App) managerResolveAllowed(isApproval bool, id string) bool {
	sid := a.cardSession(isApproval, id)
	return sid != "" && a.managerTurnActive(sid)
}

// errManagerDecisionForbidden 保持经理窗口外的 HTTP 403 语义。
var errManagerDecisionForbidden = errors.New("这张卡不属于经理投递开启且正在执行的那一轮（经理代批是轮次级授权），请留给用户处理")

// commitManagerDecisionLocked 把授权检查和决定提交放在同一撤销边界内。
// 调用者先持有 permMu，再取 runMu；反向嵌套禁止。commit 只改卡片状态并投递
// 容量为 1 的等待通道，不做通知、持久化或其他 IO。revoke 先拿到 runMu 则拒绝；
// 本提交先拿到则决定已生效，之后的 revoke 不追溯它。
func (a *App) commitManagerDecisionLocked(sid string, commit func()) error {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if sid == "" || !a.managerTurnActiveLocked(sid) {
		return errManagerDecisionForbidden
	}
	commit()
	return nil
}

func (a *App) resolveManagerApproval(id string, approved, always bool) error {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	ap, ok := a.approvals[id]
	if !ok {
		return errManagerDecisionForbidden
	}
	return a.commitManagerDecisionLocked(ap.SessionID, func() {
		if !ap.decided {
			ap.byManager, ap.decided = true, true
			ap.ch <- approvalResult{approved: approved, always: always}
		}
	})
}

func (a *App) resolveManagerQuestion(id, answer string) error {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	q, ok := a.questions[id]
	if !ok {
		return errManagerDecisionForbidden // 不接续已无在跑执行体的持久化计划
	}
	return a.commitManagerDecisionLocked(q.SessionID, func() {
		if !q.decided {
			q.byManager, q.decided = true, true
			q.ch <- answer
		}
	})
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

// deliverManagerNotice 投一条要经理行动的通知：只投「经理投递开启的那一轮
// 还在执行」的会话（轮次级授权，见文件头——窗口外的卡不通知，用户在场的轮
// 用户自己看得见）；经理主对话不存在时建出来（要它处理，值得建）；忙则插话，
// 空闲落库并唤醒。
func (a *App) deliverManagerNotice(sessionID, text string) {
	if !a.managerTurnActive(sessionID) {
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

// resolveCallerKind 是决议请求的调用者身份三分类（server.go 两处决议入口据此
// 选授权路径）。
type resolveCallerKind int

const (
	// resolveCallerUser 不带调用者头：界面按钮、远程转发——真用户。用户永远有权
	// 决定任何一张卡，这条路必须保持照常可用。
	resolveCallerUser resolveCallerKind = iota
	// resolveCallerManager 长期助理主对话自己，或它派生的子对话（parent 链落到
	// 主对话，见 managerLineage）：走与经理完全相同的轮次级授权
	//（managerResolveAllowed 预检 + commitManagerDecisionLocked 原子提交），
	// 既不因「我是子对话」免掉轮次校验，也不能代批别的轮次的卡。
	resolveCallerManager
	// resolveCallerForeign 认不出身份的 AI 会话：既不是主对话、也不在它派生的
	// parent 链上（普通工作任务会话、aux、父会话已删除的孤儿等）——一律 403，
	// 绝不回落成用户语义。
	resolveCallerForeign
)

// errResolveCallerForeign 认不出身份的 AI 会话代决时给的可读原因。
var errResolveCallerForeign = errors.New("这条决议请求的调用者不是长期助理主对话、也不在它派生的子对话链上：AI 会话只能代决经理自己那一轮（经理经 call_api 代决）的卡，其它卡请留给用户处理")

// callerSessionHeaderName 是 AI 会话直调请求的调用者头：runCallAPI 给每个 AI
// 会话的直调请求注入它（值＝该会话自己的 id），子对话注入的就是子会话 id。
const callerSessionHeaderName = "X-JX-Caller-Session"

// callerSessionHeader 取一条请求的调用者头。present 只看「有没有」不看取值——
// 空值同样算带上：宁可让异常请求被拒，也不给 AI 留一条回落成用户语义的缝。
// sid 是去掉首尾空白后的值（可能为空）。
func callerSessionHeader(r *http.Request) (sid string, present bool) {
	for k, vs := range r.Header {
		if !strings.EqualFold(k, callerSessionHeaderName) {
			continue
		}
		for _, v := range vs {
			if s := strings.TrimSpace(v); s != "" {
				return s, true
			}
		}
		return "", true
	}
	return "", false
}

// resolveCallerOf 给一条决议请求的调用者分身份：**有头即 AI**（runCallAPI 给
// 每个 AI 会话的直调请求注入 X-JX-Caller-Session=它自己的会话 id，子对话注入的
// 就是子会话 id；与 projects_api.go 的 projectSettingsRequest 同一惯例），带上
// 头却认不出身份时报 foreign 而不是 user——回落成用户语义等于任何 AI 会话都能
// 经 call_api 跳过全部经理授权校验。无头的才是真用户。
func (a *App) resolveCallerOf(r *http.Request) resolveCallerKind {
	sid, _ := callerSessionHeader(r)
	if sid == "" {
		return resolveCallerUser
	}
	if a.managerLineage(sid) {
		return resolveCallerManager
	}
	return resolveCallerForeign
}

// managerLineage 判断 sid 是不是「经理本级，或经理派生的子对话」：主对话
//（currentAssistant，当前版本指针）自己算；其余沿 parent 链往上走，链落到主
// 对话才算——dispatch_sub 建子对话时 ParentID 就是派发者，经理派出去的子对话
// 天然在链上，经理才可能认得它、也才谈得上「子对话权力不超过经理本级」。
//
// 认不出的情形全按不认（调用方 403）：链上任一环取不到会话（父会话已删除的
// 孤儿）、链走空停在非主对话的顶层会话（普通工作任务 / aux，及它们派生的子
// 对话）。子对话只有一级，seen 只是脏数据成环的兜底（正常数据走不到下一级）。
func (a *App) managerLineage(sid string) bool {
	mgr := a.currentAssistant()
	if mgr == nil {
		return false
	}
	seen := map[string]bool{}
	for sid != "" && !seen[sid] {
		if sid == mgr.ID {
			return true
		}
		seen[sid] = true
		m, err := a.Store.Get(sid)
		if err != nil || m == nil {
			return false
		}
		sid = strings.TrimSpace(m.ParentID)
	}
	return false
}

// markQuestionByManager 供回告链路的独立测试标注来源；实际 HTTP 代决必须走
// resolveManagerQuestion，把来源与决定提交放在同一授权边界内。
// （身份三分类的地基是 resolveCallerOf / managerLineage：旧实现里只认「与主对话
// 逐字相等」的 managerResolveCaller 已并入三分类，不再单独存在。）
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

// mgrOutcomeSourceLine 决议来源的回告口径：经理自己选的标「长期助理代选」；
// 超时是没人处理，不算谁的决议，不落来源。
func mgrOutcomeSourceLine(byManager, timedOut bool) string {
	if timedOut {
		return ""
	}
	if byManager {
		return "来源：长期助理代选。"
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
