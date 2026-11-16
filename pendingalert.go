package main

// 卡片刚出现时的即时提示（「久等提醒」的另一半：一个管出现，一个管等太久）。
//
// 问题：提问卡/计划审批/越权审批挂出来时后端只广播一条事件，用户把程序放在
// 后台就根本不知道有卡在等他——原来只有「等满 askAlertAfterSec（默认 4 分钟）
// 仍未处理」才叮一声 + 盖全屏蒙层，卡片刚出现那一刻一点动静都没有。
//
// 口径（用户定调）：
//   - 凡必须由用户决定的卡 → 一出现就轻量提示：响一声（设置里选的音效方案，
//     alertsound.go；默认 Windows 通知音）+ 任务栏按钮闪一下（复用 FlashWindowEx 那套）；
//   - 长期助理此刻可代批的卡不打扰用户：卡所属会话的经理轮授权仍在
//     （managerResolveAllowed 为真、主对话也在）＝ 那张卡经理那一轮自己会处置，
//     用户不必被叫醒；该轮随后收口/用户接管、代批资格没了，这时才转成通知用户；
//   - 同一张卡最多即时响一次：卡片事件重复、重连后重放（同一个 id 再来一遍）
//     都不再响；之后仍照旧走「等满 N 秒再提醒」那一套。
//
// 只做轻量唤起：不抢前台、不盖蒙层、不朗读、不发新事件（卡片事件本来就在广播
// 里，前端行为不动）——把那套留给久等提醒，别把用户正在做的事打断。

import "log"

// pendingCardAlertState 一张卡的即时提示状态（permMu 保护）：
//   - deferred=出现时判为「经理可代批」，那一次没打扰用户（等资格没了再补）；
//   - alerted=已经即时响过（含 deferred 转通知的那一次），同一张卡不再响。
type pendingCardAlertState struct {
	sessionID string
	deferred  bool
	alerted   bool
}

// pendingCardFlashHook 即时提示的「任务栏闪一下」入口（平台实现见
// alertfront_windows.go 的 pendingCardAlertFlash；测试里换桩计数）。
var pendingCardFlashHook = pendingCardAlertFlash

// alertPendingCardOnAppear 卡片刚进入等待时的即时提示入口（提问卡与审批卡两条
// 路径都调它，卡片必须已登记进等待清单）。
func (a *App) alertPendingCardOnAppear(id string) {
	if !a.Cfg.askAlertOnAppear() {
		return // 设置里关掉了「卡片出现时提示音」：保持旧的「等满 N 秒才提醒」
	}
	isApproval, sid, title, summary, ok := a.pendingCardOf(id)
	if !ok {
		return
	}
	// 先原子占位：同一张卡只即时提示一次（重复事件/重连重放到此为止）
	a.permMu.Lock()
	if a.pendingAlert == nil {
		a.pendingAlert = map[string]*pendingCardAlertState{}
	}
	if _, seen := a.pendingAlert[id]; seen {
		a.permMu.Unlock()
		return
	}
	a.pendingAlert[id] = &pendingCardAlertState{sessionID: sid}
	a.permMu.Unlock()

	if a.cardManagerCovers(isApproval, id) {
		// 经理可代批：此刻不打扰用户，标记住，等代批资格没了再补一次通知
		a.permMu.Lock()
		if st := a.pendingAlert[id]; st != nil {
			st.deferred = true
		}
		a.permMu.Unlock()
		log.Printf("[提醒] 卡片 %s 属长期助理投递开启的那一轮（经理可代批），不打扰用户", id)
		return
	}
	a.firePendingCardAlert(id, title, summary)
}

// firePendingCardAlert 真的响一次：响一声 + 任务栏闪一下。已经响过的卡直接跳过
// （deferred 转通知也走这里，保证同一张卡只响一次）。
func (a *App) firePendingCardAlert(id, title, summary string) {
	a.permMu.Lock()
	st := a.pendingAlert[id]
	if st == nil || st.alerted {
		a.permMu.Unlock()
		return
	}
	st.alerted, st.deferred = true, false
	a.permMu.Unlock()
	askAlertSound(a.Cfg)  // 按设置选的音效方案（默认 Windows 通知音，alertsound.go）
	pendingCardFlashHook() // 轻量唤起：任务栏按钮闪一下，不抢前台
	log.Printf("[提醒] 卡片刚出现，已即时提示用户：%s", alertFrontText(title, summary))
}

// cardManagerCovers 这张卡此刻是否由长期助理代批（＝不必打扰用户）：判定复用
// 决议入口同一套 managerResolveAllowed（轮次级授权，不另写近似逻辑），并要求
// 主对话确实存在（currentAssistant 是「当前主对话是谁」的唯一口径）——找不到
// 主对话就按「必须由用户决定」处理，宁可多响一次。
func (a *App) cardManagerCovers(isApproval bool, id string) bool {
	if !a.managerResolveAllowed(isApproval, id) {
		return false
	}
	return a.currentAssistant() != nil
}

// flushDeferredPendingCards 代批资格没了（那一轮收口 / 用户接管）就把当时没打扰
// 用户的卡补上一次即时提示：只补「还在等、还没响过」的那些。
func (a *App) flushDeferredPendingCards(sid string) {
	if !a.Cfg.askAlertOnAppear() {
		return
	}
	a.permMu.Lock()
	var ids []string
	for id, st := range a.pendingAlert {
		if st.sessionID == sid && st.deferred && !st.alerted {
			ids = append(ids, id)
		}
	}
	a.permMu.Unlock()
	for _, id := range ids {
		isApproval, _, title, summary, ok := a.pendingCardOf(id)
		if !ok {
			a.forgetPendingCardAlert(id) // 卡已收走：状态留着没意义
			continue
		}
		if a.cardManagerCovers(isApproval, id) {
			continue // 又回到可代批（保守处理：继续不打扰）
		}
		a.firePendingCardAlert(id, title, summary)
	}
}

// pendingCardOf 取一张还在等待的卡（提问/计划审批/越权审批都认）：返回是否审批卡、
// 所属会话、会话标题与摘要。卡片已决议/已收走返回 ok=false。
func (a *App) pendingCardOf(id string) (isApproval bool, sessionID, title, summary string, ok bool) {
	a.permMu.Lock()
	defer a.permMu.Unlock()
	if ap, hit := a.approvals[id]; hit && !ap.decided {
		return true, ap.SessionID, ap.SessionTitle, ap.Summary, true
	}
	if q, hit := a.questions[id]; hit && !q.decided {
		return false, q.SessionID, q.SessionTitle, q.Question, true
	}
	return false, "", "", "", false
}

// forgetPendingCardAlert 卡片收走时清掉即时提示状态（状态表不随卡片泄漏）。
func (a *App) forgetPendingCardAlert(id string) {
	a.permMu.Lock()
	delete(a.pendingAlert, id)
	a.permMu.Unlock()
}
