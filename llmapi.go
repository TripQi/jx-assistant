package main

// llmapi.go —— 独立单轮模型调用的通用端点：POST /api/llm/ask。
//
// 一次发送（system + user）、一次输出：不进任何会话的上下文、不落消息表、
// 不带工具——供浏览器半边组件与钩子 worker 复用的「辅助生成」通道。首个
// 先例是 git 面板的提交摘要（gitSummarize，后端直调模型）；首个外部使用方是
// 「AI 自动会话标题」组件（组件/session-title 的 worker 经反向调用走本端点，
// worker 自身因此不持有任何模型密钥）。调用目标跟随 session 指定的会话
// （没给或会话不存在用全局默认），超时与输入上限对齐 git 摘要。

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

const (
	llmAskTimeout  = 45 * time.Second // 与 git 摘要同档：辅助调用不值得等更久
	llmAskMaxInput = 24000            // user 正文的防御上限（字节；正常由调用方自己控制预算）
)

func (a *App) handleLLMAsk(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Session string `json:"session"` // 可选：跟随该会话配置的模型
		System  string `json:"system"`  // 可选系统提示词
		User    string `json:"user"`    // 必填：单轮正文
		// Purpose 可选：这次辅助生成在用量统计里记成什么用途（title=AI 自动
		// 会话标题、compact=上下文压缩，其余一律 assist=其他辅助生成）。
		// 组件自己知道在干什么，记账口径由调用方给，主程序不猜。
		Purpose string `json:"purpose"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	user := strings.TrimSpace(body.User)
	if user == "" {
		writeErr(w, 400, "user 不能为空")
		return
	}
	if len(user) > llmAskMaxInput { // 防御截断：调用方负责预算，这里只兜底
		user = user[:llmAskMaxInput]
	}
	target, terr := a.DefaultTarget()
	if terr != nil {
		writeErr(w, 400, terr.Error())
		return
	}
	var sess *SessionMeta
	if body.Session != "" {
		if m, gerr := a.Store.Get(body.Session); gerr == nil && m != nil {
			if t, rerr := a.resolveTarget(m); rerr != nil {
				writeErr(w, 400, rerr.Error())
				return
			} else {
				target, sess = t, m
			}
		}
	}
	msgs := make([]Message, 0, 2)
	if s := strings.TrimSpace(body.System); s != "" {
		msgs = append(msgs, TextMessage("system", s))
	}
	msgs = append(msgs, TextMessage("user", user))
	ctx, cancel := context.WithTimeout(r.Context(), llmAskTimeout)
	defer cancel()
	var sb strings.Builder
	purpose := usagePurposeAssist
	switch strings.TrimSpace(body.Purpose) {
	case usagePurposeTitle:
		purpose = usagePurposeTitle
	case usagePurposeCompact:
		purpose = usagePurposeCompact
	}
	meterCb, finishUsage := a.meterUsage(purpose, sess, target, "", StreamCallbacks{
		OnText: func(s string) { sb.WriteString(s) },
	})
	err := StreamChat(ctx, target, ChatRequest{
		Model:    target.Model,
		Messages: msgs,
		Stream:   true,
	}, meterCb)
	finishUsage(err)
	if err != nil {
		writeErr(w, 502, "模型调用失败："+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"text": sb.String()})
}
