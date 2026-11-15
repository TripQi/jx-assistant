package main

import "encoding/json"

// pendingLanding 复用 audit 载荷，不加列、不改变 Message/模型协议。
// seq 指向实际用户消息；两行同事务，重启后无需任何内存索引即可恢复。
type pendingLanding struct {
	Type      string `json:"type"`
	PendingID string `json:"pendingId"`
	Seq       int64  `json:"seq"`
}

// 调用者已持有 Store.mu。
func (s *Store) appendPendingRowLocked(id string, sm *StoredMessage) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = appendMessageRow(tx, id, sm); err != nil {
		return err
	}
	b, err := json.Marshal(pendingLanding{Type: "pending_landed", PendingID: sm.PendingID, Seq: sm.Seq})
	if err != nil {
		return err
	}
	row := &StoredMessage{Message: TextMessage("system", string(b)), Kind: kindAudit, Origin: "pending_landed", CreatedAt: sm.CreatedAt}
	if err = appendMessageRow(tx, id, row); err != nil {
		return err
	}
	return tx.Commit()
}

// 仅给 UI 下发副本投影，普通存储读取/上下文组装不增加字段或内容。
func (s *Store) projectPendingIDs(id string, msgs []*StoredMessage) error {
	rows, err := s.db.Query(`SELECT content FROM messages WHERE session_id=? AND kind='audit' AND origin='pending_landed'`, id)
	if err != nil {
		return err
	}
	defer rows.Close()
	bySeq := make(map[int64]*StoredMessage, len(msgs))
	for _, sm := range msgs {
		if sm.Role == "user" {
			bySeq[sm.Seq] = sm
		}
	}
	for rows.Next() {
		var content string
		if err := rows.Scan(&content); err != nil {
			return err
		}
		var link pendingLanding
		if json.Unmarshal([]byte(content), &link) == nil && link.Type == "pending_landed" && link.PendingID != "" {
			if sm := bySeq[link.Seq]; sm != nil {
				sm.PendingID = link.PendingID
			}
		}
	}
	return rows.Err()
}
