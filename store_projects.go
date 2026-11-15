package main

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"
)

const DefaultProjectKnowledgeBudget int64 = 4000

var (
	ErrProjectNotFound              = errors.New("项目不存在，请重新查询登记项目")
	ErrProjectArchived              = errors.New("项目已归档，请先由用户在设置页恢复")
	ErrProjectRevisionConflict      = errors.New("知识版本冲突，请重读当前正文后合并再保存")
	ErrProjectDeleteConfirmation    = errors.New("永久删除项目须由用户在设置页明确确认")
	ErrProjectAuthorizationRequired = errors.New("缺少项目知识写入授权")
)

type Project struct {
	ID         string             `json:"id"`
	Name       string             `json:"name"`
	Archived   bool               `json:"archived"`
	CreatedAt  string             `json:"created_at"`
	UpdatedAt  string             `json:"updated_at"`
	Workspaces []ProjectWorkspace `json:"workspaces"`
}

type ProjectWorkspace struct {
	ProjectID     string `json:"project_id"`
	WorkspaceKind string `json:"workspace_kind"`
	WorkspaceKey  string `json:"workspace_key"`
	DisplayPath   string `json:"display_path"`
}

type ProjectKnowledge struct {
	ProjectID   string `json:"project_id"`
	Content     string `json:"content"`
	Revision    int64  `json:"revision"`
	UpdatedAt   string `json:"updated_at"`
	BudgetLimit int64  `json:"budget_limit"`
}

type ProjectKnowledgeBudgetError struct{ Used, Limit int64 }

func (e *ProjectKnowledgeBudgetError) Error() string {
	return fmt.Sprintf("知识预算超限：已用 %d / %d，超出 %d；请删除过期、重复约束及进度流水，合并同类规则并精简背景后重试（不得截断）；确需扩容由用户在设置页调整", e.Used, e.Limit, e.Used-e.Limit)
}

// 按 code point 和四种脚本计数，非 CJK 全文仅取整一次；不复用 token 估算。
func ProjectKnowledgeBudget(content string) int64 {
	var cjk, other int64
	for _, r := range content {
		if unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r) || unicode.Is(unicode.Hangul, r) {
			cjk++
		} else {
			other++
		}
	}
	return 2*cjk + (other+3)/4
}

func projectTimestamp() string { return time.Now().UTC().Format(time.RFC3339Nano) }

type projectReader interface {
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func readProject(q projectReader, id string) (Project, error) {
	p := Project{Workspaces: []ProjectWorkspace{}}
	err := q.QueryRow(`SELECT id,name,archived,created_at,updated_at FROM projects WHERE id=?`, id).Scan(&p.ID, &p.Name, &p.Archived, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return p, ErrProjectNotFound
	}
	if err != nil {
		return p, err
	}
	rows, err := q.Query(`SELECT project_id,workspace_kind,workspace_key,display_path FROM project_workspaces WHERE project_id=? ORDER BY workspace_kind,workspace_key`, id)
	if err != nil {
		return p, err
	}
	defer rows.Close()
	for rows.Next() {
		var w ProjectWorkspace
		if err := rows.Scan(&w.ProjectID, &w.WorkspaceKind, &w.WorkspaceKey, &w.DisplayPath); err != nil {
			return p, err
		}
		p.Workspaces = append(p.Workspaces, w)
	}
	return p, rows.Err()
}

func (s *Store) GetProject(id string) (Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readProject(s.db, id)
}

func (s *Store) ListProjects(includeArchived bool) ([]Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := `SELECT id FROM projects`
	if !includeArchived {
		query += ` WHERE archived=0`
	}
	query += ` ORDER BY name,id`
	rows, err := s.db.Query(query)
	if err != nil {
		return nil, err
	}
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	result := make([]Project, 0, len(ids))
	for _, id := range ids {
		p, err := readProject(s.db, id)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, nil
}

func validateProject(p Project) error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("项目名称不能为空")
	}
	for _, w := range p.Workspaces {
		if w.WorkspaceKind != "local" && w.WorkspaceKind != "remote" {
			return errors.New("工作区类型须为 local 或 remote")
		}
		if w.WorkspaceKey == "" {
			return errors.New("工作区规范化标识不能为空")
		}
	}
	return nil
}

func insertProjectWorkspaces(tx *sql.Tx, p Project) error {
	for _, w := range p.Workspaces {
		if _, err := tx.Exec(`INSERT INTO project_workspaces(project_id,workspace_kind,workspace_key,display_path) VALUES(?,?,?,?)`, p.ID, w.WorkspaceKind, w.WorkspaceKey, w.DisplayPath); err != nil {
			return fmt.Errorf("项目绑定保存失败（同一绑定只能属于一个项目）: %w", err)
		}
	}
	return nil
}

func (s *Store) CreateProject(p Project) (Project, error) {
	if err := validateProject(p); err != nil {
		return Project{}, err
	}
	if p.ID == "" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Project{}, err
		}
		p.ID = "project-" + hex.EncodeToString(id[:])
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	p.CreatedAt = projectTimestamp()
	p.UpdatedAt = p.CreatedAt
	if _, err = tx.Exec(`INSERT INTO projects(id,name,archived,created_at,updated_at) VALUES(?,?,?,?,?)`, p.ID, p.Name, p.Archived, p.CreatedAt, p.UpdatedAt); err != nil {
		return Project{}, err
	}
	if err = insertProjectWorkspaces(tx, p); err != nil {
		return Project{}, err
	}
	if _, err = tx.Exec(`INSERT INTO project_knowledge(project_id,updated_at) VALUES(?,?)`, p.ID, p.CreatedAt); err != nil {
		return Project{}, err
	}
	result, err := readProject(tx, p.ID)
	if err != nil {
		return Project{}, err
	}
	if err = tx.Commit(); err != nil {
		return Project{}, err
	}
	return result, nil
}

// 元数据和绑定整体替换；归档/恢复不伪装为正文更新，重复保存不刷新时间。
func (s *Store) UpdateProject(p Project) (Project, error) {
	if err := validateProject(p); err != nil {
		return Project{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Project{}, err
	}
	defer tx.Rollback()
	current, err := readProject(tx, p.ID)
	if err != nil {
		return Project{}, err
	}
	equal := p.Name == current.Name && p.Archived == current.Archived && len(p.Workspaces) == len(current.Workspaces)
	existing := make(map[[2]string]string, len(current.Workspaces))
	for _, w := range current.Workspaces {
		existing[[2]string{w.WorkspaceKind, w.WorkspaceKey}] = w.DisplayPath
	}
	seen := make(map[[2]string]bool, len(p.Workspaces))
	for _, w := range p.Workspaces {
		key := [2]string{w.WorkspaceKind, w.WorkspaceKey}
		path, ok := existing[key]
		if !ok || path != w.DisplayPath || seen[key] {
			equal = false
		}
		seen[key] = true
	}
	if equal {
		return current, nil
	}
	if _, err = tx.Exec(`UPDATE projects SET name=?,archived=?,updated_at=? WHERE id=?`, p.Name, p.Archived, projectTimestamp(), p.ID); err != nil {
		return Project{}, err
	}
	if _, err = tx.Exec(`DELETE FROM project_workspaces WHERE project_id=?`, p.ID); err != nil {
		return Project{}, err
	}
	if err = insertProjectWorkspaces(tx, p); err != nil {
		return Project{}, err
	}
	result, err := readProject(tx, p.ID)
	if err != nil {
		return Project{}, err
	}
	if err = tx.Commit(); err != nil {
		return Project{}, err
	}
	return result, nil
}

// confirmed 仅是存储防误删，调用宿主仍须验证设置页用户身份。外键仅向项目子表级联，绝不删除会话。
func (s *Store) DeleteProject(id string, confirmed bool) error {
	if !confirmed {
		return ErrProjectDeleteConfirmation
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`DELETE FROM projects WHERE id=?`, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if count == 0 {
		return ErrProjectNotFound
	}
	return tx.Commit()
}

func readProjectKnowledge(q projectReader, id string) (ProjectKnowledge, error) {
	var k ProjectKnowledge
	err := q.QueryRow(`SELECT project_id,content,revision,updated_at,budget_limit FROM project_knowledge WHERE project_id=?`, id).Scan(&k.ProjectID, &k.Content, &k.Revision, &k.UpdatedAt, &k.BudgetLimit)
	if errors.Is(err, sql.ErrNoRows) {
		return k, ErrProjectNotFound
	}
	return k, err
}

func (s *Store) GetProjectKnowledge(id string) (ProjectKnowledge, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readProjectKnowledge(s.db, id)
}

// authorize 必须使用宿主当前会话状态，不能信任模型传入身份。回调在事务和写锁内执行，
// 不得再次调用 Store（单连接）；预算修改资格亦由宿主在此回调校验。
func (s *Store) SaveProjectKnowledge(id string, expectedRevision int64, content string, budgetLimit *int64, authorize func(Project) error) (ProjectKnowledge, error) {
	if authorize == nil {
		return ProjectKnowledge{}, ErrProjectAuthorizationRequired
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return ProjectKnowledge{}, err
	}
	defer tx.Rollback()
	p, err := readProject(tx, id)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	if err = authorize(p); err != nil {
		return ProjectKnowledge{}, err
	}
	if p.Archived {
		return ProjectKnowledge{}, ErrProjectArchived
	}
	k, err := readProjectKnowledge(tx, id)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	if k.Revision != expectedRevision {
		return ProjectKnowledge{}, ErrProjectRevisionConflict
	}
	limit := k.BudgetLimit
	if budgetLimit != nil {
		limit = *budgetLimit
	}
	if limit <= 0 {
		return ProjectKnowledge{}, errors.New("知识预算上限须为合法正整数")
	}
	used := ProjectKnowledgeBudget(content)
	if used > limit {
		return ProjectKnowledge{}, &ProjectKnowledgeBudgetError{Used: used, Limit: limit}
	}
	if k.Content == content && k.BudgetLimit == limit {
		return k, nil
	}
	if k.Revision == math.MaxInt64 {
		return ProjectKnowledge{}, errors.New("知识版本已达整数上限，拒绝保存")
	}
	k.Content = content
	k.BudgetLimit = limit
	k.Revision++
	k.UpdatedAt = projectTimestamp()
	result, err := tx.Exec(`UPDATE project_knowledge SET content=?,revision=?,updated_at=?,budget_limit=? WHERE project_id=? AND revision=?`, k.Content, k.Revision, k.UpdatedAt, k.BudgetLimit, id, expectedRevision)
	if err != nil {
		return ProjectKnowledge{}, err
	}
	count, err := result.RowsAffected()
	if err != nil {
		return ProjectKnowledge{}, err
	}
	if count != 1 {
		return ProjectKnowledge{}, ErrProjectRevisionConflict
	}
	if err = tx.Commit(); err != nil {
		return ProjectKnowledge{}, err
	}
	return k, nil
}
