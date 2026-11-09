package main

// 长期助理主对话的「版本」模型（设置 · 对话历史与压缩）。
//
// 历史：以前想让助理「重新开始」只能「清空」——真删历史，旧对话、文件快照、
// 交付记录一并没了。现在改成「换版本」：新建一版就是从零开始的全新数字助理，
// 老版本原样留着（历史、快照、交付、上传登记都在它自己名下），设置页里能一键
// 切回去接着那条线聊。
//
// 为什么不必新建数据结构：每一版就是一个普通的 assistant 主会话
//（mode=assistant、无父会话）——会话 id、标题、工作目录、权限、模型各自独立，
// 执行、压缩、定时任务转交、派发子对话本来就按会话 id 走。整套版本机制只加
// 两样东西，都落在已有的 meta 表里：
//
//  1. 版本号（第一版=1、第二版=2…）：按会话存 sessdata:assistant.version<id>，
//     一旦落号就不再变动——界面上的「第三版」不会因为刷新、切换、重新排序而
//     换号。升级前就存在的那一版没有号，第一次列版本时按创建时间补成第一版，
//     它里面的历史、快照、交付一条不动。
//  2. 「当前是哪一版」的指针：globaldata:assistant.current。**必须显式记在库里**：
//     老做法是「会话列表按更新时间倒序取第一个」，多版本下立刻失效——切回老
//     版本聊一句，它就变成「最近更新的那个」，压缩、定时任务转交、任务求助、
//     前端入口各取各的，版本当场错位。指针只有三处写（新建一版 / 切换版本 /
//     指针失效时的自愈兜底），读的地方一律走 currentAssistant()。

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	assistantCurrentKey = "assistant.current" // globaldata 键：当前版本的会话 id
	assistantVersionKey = "assistant.version" // sessdata 键：这一版的版本号（第一版=1）
	assistantMainTitle  = "数字助理主对话"         // 主对话的会话标题（界面显示名另由前端映射成「跟长期助理对话」）
)

// AssistantVersion 是设置页版本清单的一行：第几版、多少条消息、最后更新时间。
type AssistantVersion struct {
	SessionID string `json:"sessionId"`
	Version   int    `json:"version"`
	Title     string `json:"title"`
	Messages  int    `json:"messages"`
	CreatedAt string `json:"createdAt"`
	UpdatedAt string `json:"updatedAt"`
	Running   bool   `json:"running"`
	Current   bool   `json:"current"`
}

// isAssistantVersionSession 判定一个会话是不是「长期助理主对话的某一版」：
// assistant 模式且是顶层会话（子对话另有自己的标题与生命周期，不算一版）。
// 与前端 isAssistantMainSession 同一口径。
func isAssistantVersionSession(m *SessionMeta) bool {
	return m != nil && m.Mode == "assistant" && m.ParentID == ""
}

// assistantVersionMu 串行化「补版本号 / 新建一版」这两处改写。版本号是先读后写
// （取当前最大号 +1），并发点两下「新建一版」（或同一用户开着两个窗口）会算出
// 同一个号；同一把锁保证号只增不重。
var assistantVersionMu sync.Mutex

// assistantVersions 列出全部版本（版本号升序），顺手给没号的会话补号（升级兼容）。
func (a *App) assistantVersions() ([]*AssistantVersion, error) {
	assistantVersionMu.Lock()
	defer assistantVersionMu.Unlock()
	return a.assistantVersionsLocked()
}

// assistantVersionsLocked 是 assistantVersions 的实体（调用方已持锁）。
func (a *App) assistantVersionsLocked() ([]*AssistantVersion, error) {
	sessions, err := a.Store.List()
	if err != nil {
		return nil, err
	}
	live := make([]*SessionMeta, 0, 4)
	for _, m := range sessions {
		if isAssistantVersionSession(m) && !m.Archived {
			live = append(live, m)
		}
	}
	if len(live) == 0 {
		return []*AssistantVersion{}, nil
	}
	// 补号：先认已经落下的号（含被归档后恢复的版本），剩下的按创建时间从早到晚
	// 接着编——升级前那一版因此成为第一版。
	nums := make(map[string]int, len(live))
	maxNum := 0
	for _, m := range live {
		if n, err := strconv.Atoi(strings.TrimSpace(a.Store.GetSessionData(m.ID, assistantVersionKey))); err == nil && n > 0 {
			nums[m.ID] = n
			if n > maxNum {
				maxNum = n
			}
		}
	}
	unassigned := make([]*SessionMeta, 0, len(live))
	for _, m := range live {
		if _, ok := nums[m.ID]; !ok {
			unassigned = append(unassigned, m)
		}
	}
	sort.SliceStable(unassigned, func(i, j int) bool {
		if unassigned[i].CreatedAt != unassigned[j].CreatedAt {
			return unassigned[i].CreatedAt < unassigned[j].CreatedAt
		}
		return unassigned[i].ID < unassigned[j].ID
	})
	for _, m := range unassigned {
		maxNum++
		nums[m.ID] = maxNum
		_ = a.Store.SetSessionData(m.ID, assistantVersionKey, strconv.Itoa(maxNum))
	}
	cur := ""
	if m := a.currentAssistant(); m != nil { // 顺带自愈指针（升级后第一次列版本就走这里）
		cur = m.ID
	}
	out := make([]*AssistantVersion, 0, len(live))
	for _, m := range live {
		n, _ := a.Store.MessageCount(m.ID) // 条数只用于辨认，取不到就记 0
		out = append(out, &AssistantVersion{
			SessionID: m.ID, Version: nums[m.ID], Title: m.Title, Messages: n,
			CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt,
			Running: a.IsRunning(m.ID), Current: m.ID == cur,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Version < out[j].Version })
	return out, nil
}

// currentAssistantID 读「当前版本」指针；没设过、或指向的会话已不存在/不是主对话
//（被删、被归档、被改成别的模式）时返回空串，由 currentAssistant 兜底自愈。
func (a *App) currentAssistantID() string {
	id := strings.TrimSpace(a.Store.GetGlobalData(assistantCurrentKey))
	if id == "" {
		return ""
	}
	m, err := a.Store.Get(id)
	if err != nil || !isAssistantVersionSession(m) || m.Archived {
		return ""
	}
	return id
}

// currentAssistant 取当前版本的会话。设置页的压缩/清空、待办转交、任务求助
//（request_manager）、前端「跟长期助理对话」入口全都认它——这是「当前主对话是谁」
// 的唯一口径。
//
// 没有版本时返回 nil：设置页的动作不该顺手建出一个空对话（要建的地方显式走
// assistantSession）。指针丢失/失效时兜底：取最近更新的那一版并把它落成当前
// 版本（升级后第一次打开就走到这里——用户正在用的那一版自然成为当前版本）。
func (a *App) currentAssistant() *SessionMeta {
	if id := a.currentAssistantID(); id != "" {
		if m, err := a.Store.Get(id); err == nil {
			return m
		}
	}
	sessions, err := a.Store.List()
	if err != nil {
		return nil
	}
	for _, m := range sessions {
		if isAssistantVersionSession(m) && !m.Archived {
			if err := a.setCurrentAssistant(m.ID); err != nil {
				log.Printf("[长期助理] 落「当前版本」指针失败（会话 %s）：%v", m.ID, err)
			}
			return m
		}
	}
	return nil
}

// setCurrentAssistant 落「当前版本」指针（写入口：新建一版 / 切换版本 / 兜底自愈）。
func (a *App) setCurrentAssistant(id string) error {
	return a.Store.SetGlobalData(assistantCurrentKey, strings.TrimSpace(id))
}

// createAssistantVersion 新建一版数字助理（设置页「新建一版」）：历史从零开始，
// 但配置沿用当前版本（工作目录、权限、模型、思考档位、能力开关、远程挂载）——
// 换版本只为「重新开始聊」，不该让用户把设置再设一遍。
func (a *App) createAssistantVersion() (*AssistantVersion, error) {
	assistantVersionMu.Lock()
	defer assistantVersionMu.Unlock()
	versions, err := a.assistantVersionsLocked()
	if err != nil {
		return nil, err
	}
	next := 1
	if len(versions) > 0 {
		next = versions[len(versions)-1].Version + 1
	}
	p := createSessionParams{
		Mode: "assistant", Title: assistantMainTitle,
		Workspace: a.Cfg.Workspace, Permission: permFull,
	}
	if cur := a.currentAssistant(); cur != nil {
		p.Workspace = cur.Workspace
		p.Provider, p.Model = cur.Provider, cur.Model
		p.Permission, p.Thinking, p.Tools = cur.Permission, cur.Thinking, cur.Tools
		if cur.Remote != nil {
			r := *cur.Remote
			p.Remote = &r
		}
	}
	m, err := a.createSession(p) // 建会话单源（sessioncreate.go），与人工/派发建会话同一套字段构造
	if err != nil {
		return nil, err
	}
	if err := a.Store.SetSessionData(m.ID, assistantVersionKey, strconv.Itoa(next)); err != nil {
		return nil, fmt.Errorf("记版本号失败：%w", err)
	}
	if err := a.setCurrentAssistant(m.ID); err != nil {
		return nil, fmt.Errorf("切换当前版本失败：%w", err)
	}
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	log.Printf("[设置] 已新建长期助理第 %d 版（会话 %s），老版本原样保留", next, m.ID)
	return &AssistantVersion{
		SessionID: m.ID, Version: next, Title: m.Title, Messages: 0,
		CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt, Current: true,
	}, nil
}

// switchAssistantVersion 把当前版本切到指定的一版（设置页点某一版）。
// 只动指针：两边的会话、历史、工件一件都不碰，切回去还接着那条线聊。
func (a *App) switchAssistantVersion(id string) (*AssistantVersion, error) {
	m, err := a.Store.Get(strings.TrimSpace(id))
	if err != nil {
		return nil, errors.New("这一版不存在（可能已被删除）")
	}
	if !isAssistantVersionSession(m) {
		return nil, errors.New("只能切到长期助理主对话的某一版")
	}
	if m.Archived {
		return nil, errors.New("这一版已归档，先恢复再切过去")
	}
	if err := a.setCurrentAssistant(m.ID); err != nil {
		return nil, fmt.Errorf("切换当前版本失败：%w", err)
	}
	a.broadcast(Event{Type: "session_updated", SessionID: m.ID})
	log.Printf("[设置] 长期助理当前版本已切到会话 %s", m.ID)
	// 版本号由 assistantVersions 统一补号/排序得出，切换响应也照它的口径回（避免两处口径）
	versions, err := a.assistantVersions()
	if err != nil {
		return nil, err
	}
	for _, v := range versions {
		if v.SessionID == m.ID {
			return v, nil
		}
	}
	return nil, errors.New("切换后找不到这一版的版本号")
}

// handleAssistantVersions 版本清单 + 当前版本（设置 · 对话历史与压缩；
// 前端「跟长期助理对话」入口也靠它拿当前版本 id，不再自己按更新时间猜）。
func (a *App) handleAssistantVersions(w http.ResponseWriter, r *http.Request) {
	versions, err := a.assistantVersions()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"current": a.currentAssistantID(), "versions": versions})
}

// handleAssistantVersionNew 新建一版（从零开始的数字助理，老版本原样保留）。
func (a *App) handleAssistantVersionNew(w http.ResponseWriter, r *http.Request) {
	v, err := a.createAssistantVersion()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "sessionId": v.SessionID, "current": v.SessionID, "version": v.Version})
}

// handleAssistantVersionSwitch 切换当前版本（主对话区随之显示那一版的历史）。
func (a *App) handleAssistantVersionSwitch(w http.ResponseWriter, r *http.Request) {
	var body struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	v, err := a.switchAssistantVersion(body.SessionID)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"ok": 1, "sessionId": v.SessionID, "current": v.SessionID, "version": v.Version})
}
