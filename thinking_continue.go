package main

import (
	"fmt"
	"strings"
)

const thinkingContinueMax = 2
const thinkingContinueInstruction = "上一响应仅返回思考，未产生正文或工具调用，该响应未执行工具。请基于原任务、已完成结果与可用思考材料，给出下一步有效工具调用或最终答复，避免重复推演。"

// 生命周期绑定 RunAgent，而非 step：工具成功或计划获批不能刷新续接额度。
type thinkingContinuation struct {
	used         int
	pending      bool
	rows         map[int64]string
	outputTokens int
	usageMissing bool
	baseURL      string
}

func (s *thinkingContinuation) eligible(protocol string, c StreamCompletion, text, reasoning string, calls int) bool {
	return (protocol == "" || protocol == "chat") && c.Ended && (c.Reason == "stop" || c.Reason == "length") &&
		!c.InvalidFrame && !c.HadText && !c.HadToolFragments && calls == 0 && strings.TrimSpace(text) == "" && strings.TrimSpace(reasoning) != ""
}

func (s *thinkingContinuation) schedule(sm *StoredMessage) error {
	if s.used >= thinkingContinueMax {
		return fmt.Errorf("本轮已自动续接 %d 次，仍仅返回思考，已停止，可手动继续", s.used)
	}
	if sm == nil || sm.Seq <= 0 {
		return fmt.Errorf("纯思考记录保存失败，未自动续接，可手动继续")
	}
	if s.rows == nil {
		s.rows = map[int64]string{}
	}
	s.rows[sm.Seq] = sm.Thinking
	s.used++
	s.pending = true
	return nil
}

func (s *thinkingContinuation) completeMaterials(msgs []Message) bool {
	seen := map[int64]int{}
	for _, m := range msgs {
		original, selected := s.rows[m.thinkingOverlaySeq]
		if !selected {
			continue
		}
		if m.Role != "assistant" || m.Text() != "" || len(m.ToolCalls) != 0 || m.ReasoningContent == nil || *m.ReasoningContent != original {
			return false
		}
		seen[m.thinkingOverlaySeq]++
	}
	for seq := range s.rows {
		if seen[seq] != 1 {
			return false
		}
	}
	return true
}

func (s *thinkingContinuation) hint(reason string) string {
	prefix := "本轮仅返回思考"
	if reason == "length" {
		prefix += "（输出达到长度限制）"
	}
	return fmt.Sprintf("%s，自动续接（%d/%d）", prefix, s.used, thinkingContinueMax)
}
