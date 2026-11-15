package main

import "encoding/json"

// 请求标识只服务断连后的 UI 认领；用户行与关联审计同事务，避免误认旧的同文消息。
type chatRequestLanding struct {
	Type      string `json:"type"`
	RequestID string `json:"requestId"`
	Seq       int64  `json:"seq"`
}

// 调用者已持有 Store.mu；不增加数据库列，也不把标识带入 Message/LLM 协议。
func (s *Store) appendChatRequestRowLocked(id string, sm *StoredMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = appendMessageRow(tx, id, sm); err != nil {
		return err
	}
	b, err := json.Marshal(chatRequestLanding{Type: "req_landed", RequestID: sm.RequestID, Seq: sm.Seq})
	if err != nil {
		return err
	}
	row := &StoredMessage{Message: TextMessage("system", string(b)), Kind: kindAudit, Origin: "req_landed", CreatedAt: sm.CreatedAt}
	if err = appendMessageRow(tx, id, row); err != nil {
		return err
	}
	return tx.Commit()
}

// 只投影到 GET 下发的副本；普通存储读取和上下文组装保持原样。
func (s *Store) projectChatRequestIDs(id string, msgs []*StoredMessage) error {
	rows, err := s.db.Query(`SELECT content FROM messages WHERE session_id=? AND kind='audit' AND origin='req_landed'`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	bySeq := make(map[int64]*StoredMessage, len(msgs))
	for _, sm := range msgs {
		if sm.Role == "user" && sm.Kind == kindChat {
			bySeq[sm.Seq] = sm
		}
	}
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return err
		}
		var link chatRequestLanding
		if json.Unmarshal([]byte(content), &link) == nil && link.Type == "req_landed" && link.RequestID != "" {
			if sm := bySeq[link.Seq]; sm != nil {
				sm.RequestID = link.RequestID
			}
		}
	}
	return rows.Err()
}
