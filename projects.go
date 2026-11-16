package main

// 项目身份属于宿主，不随任一组件开关消失；解析只看目标会话自己的工作区。
import (
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"unicode"
)

const projectKnowledgeComponentID = "project-knowledge"

// normalizeProjectWorkspace 不访问文件系统：离线目录与显式符号链接绑定仍有效。
func normalizeProjectWorkspace(kind, display string) (ProjectWorkspace, error) {
	w := ProjectWorkspace{WorkspaceKind: kind, DisplayPath: strings.TrimSpace(display)}
	switch kind {
	case "local":
		if !filepath.IsAbs(w.DisplayPath) {
			return w, fmt.Errorf("本地绑定必须是绝对路径")
		}
		key := filepath.ToSlash(filepath.Clean(w.DisplayPath))
		if isWindowsRuntime {
			key = strings.ToLower(key)
		}
		w.WorkspaceKey = strings.TrimRight(key, "/")
		if w.WorkspaceKey == "" {
			w.WorkspaceKey = "/"
		}
	case "remote":
		// 展示输入只负责拆字段，身份校验与会话共用结构化核心。
		i := strings.Index(w.DisplayPath, ":/")
		if i < 0 {
			return w, fmt.Errorf("远程绑定格式为 user@host[:port]:/绝对目录")
		}
		target, dir := w.DisplayPath[:i], w.DisplayPath[i+1:]
		at := strings.Index(target, "@")
		if at <= 0 || at == len(target)-1 {
			return w, fmt.Errorf("远程绑定需要 user@host")
		}
		user, hostport := target[:at], target[at+1:]
		host, port := hostport, 0
		if strings.HasPrefix(hostport, "[") {
			end := strings.Index(hostport, "]")
			if end < 0 || net.ParseIP(hostport[1:end]) == nil {
				return w, fmt.Errorf("远程 IPv6 地址无效")
			}
			host = hostport[1:end]
			if suffix := hostport[end+1:]; suffix != "" {
				if !strings.HasPrefix(suffix, ":") {
					return w, fmt.Errorf("远程端口格式无效")
				}
				hostport = suffix[1:]
			} else {
				hostport = ""
			}
		} else if strings.Contains(hostport, ":") {
			if strings.Count(hostport, ":") != 1 {
				return w, fmt.Errorf("IPv6 地址请使用方括号")
			}
			host, hostport, _ = strings.Cut(hostport, ":")
		} else {
			hostport = ""
		}
		// 有冒号却无端口不能被当作省略端口。
		if hostport != "" || strings.HasSuffix(target, ":") {
			for _, c := range hostport {
				if c < '0' || c > '9' {
					return w, fmt.Errorf("远程端口格式无效")
				}
			}
			n, err := strconv.Atoi(hostport)
			if err != nil {
				return w, fmt.Errorf("远程端口格式无效")
			}
			port = n
		}
		return normalizeRemoteProjectWorkspace(user, host, port, dir, w.DisplayPath)
	default:
		return w, fmt.Errorf("workspace_kind 只能是 local 或 remote")
	}
	return w, nil
}

// 不经 Display 反解：IPv6 的冒号不是端口，展示也不应影响项目归属。
// 只规范身份，不读取认证字段或连接远端。
func normalizeRemoteProjectWorkspace(user, host string, port int, dir, display string) (ProjectWorkspace, error) {
	w := ProjectWorkspace{WorkspaceKind: "remote", DisplayPath: display}
	user, host, dir = strings.TrimSpace(user), strings.TrimSpace(host), strings.TrimSpace(dir)
	if user == "" {
		user = "root"
	}
	if port == 0 {
		port = 22
	}
	if dir == "" {
		dir = "/"
	}
	invalidTarget := func(s string) bool {
		return strings.ContainsAny(s, "@:/\\[]") || strings.ContainsFunc(s, func(c rune) bool { return unicode.IsSpace(c) || unicode.IsControl(c) })
	}
	if invalidTarget(user) || host == "" || port < 1 || port > 65535 {
		return w, fmt.Errorf("远程连接目标或端口无效")
	}
	if strings.HasPrefix(host, "[") && strings.HasSuffix(host, "]") {
		host = host[1 : len(host)-1]
		if net.ParseIP(host) == nil {
			return w, fmt.Errorf("远程 IP 地址无效")
		}
	}
	if ip := net.ParseIP(host); ip != nil {
		host = ip.String()
	} else {
		if invalidTarget(host) {
			return w, fmt.Errorf("远程主机无效")
		}
		for _, label := range strings.Split(strings.TrimSuffix(host, "."), ".") {
			if label == "" || len(label) > 63 || strings.HasPrefix(label, "-") || strings.HasSuffix(label, "-") {
				return w, fmt.Errorf("远程主机无效")
			}
			for _, c := range label {
				if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
					return w, fmt.Errorf("远程主机无效")
				}
			}
		}
		host = strings.ToLower(host)
	}
	if !path.IsAbs(dir) || strings.ContainsFunc(dir, unicode.IsControl) {
		return w, fmt.Errorf("远程目录必须是有效绝对路径")
	}
	w.WorkspaceKey = user + "@" + net.JoinHostPort(host, strconv.Itoa(port)) + ":" + path.Clean(dir)
	return w, nil
}

func (a *App) sessionProjectWorkspace(sess *SessionMeta) (ProjectWorkspace, error) {
	if sess == nil {
		return ProjectWorkspace{}, fmt.Errorf("缺少有效会话")
	}
	if sess.Remote != nil {
		r := sess.Remote
		return normalizeRemoteProjectWorkspace(r.User, r.Host, r.Port, r.Dir, r.Display())
	}
	return normalizeProjectWorkspace("local", a.workspaceOf(sess))
}

func projectWorkspaceContains(root, target ProjectWorkspace) bool {
	if root.WorkspaceKind != target.WorkspaceKind {
		return false
	}
	if root.WorkspaceKey == target.WorkspaceKey {
		return true
	}
	return strings.HasPrefix(target.WorkspaceKey, strings.TrimRight(root.WorkspaceKey, "/")+"/")
}

// resolveProjectWorkspaceReader 是「最长目录匹配」的唯一实现：既可走 Store 读锁，
// 也可在同一写事务里用 tx 查询复核——Store 是单连接，写事务内再调 Store 会重入死锁，
// 所以归属复核必须能在这个 reader 上做（见 store_project_events.go）。
func resolveProjectWorkspaceReader(q projectReader, target ProjectWorkspace) (*Project, error) {
	rows, err := q.Query(`SELECT p.id,p.name,p.archived,p.created_at,p.updated_at,w.workspace_kind,w.workspace_key,w.display_path
		FROM projects p JOIN project_workspaces w ON w.project_id=p.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byID := map[string]*Project{}
	var best *Project
	longest := -1
	for rows.Next() {
		var p Project
		var w ProjectWorkspace
		if err := rows.Scan(&p.ID, &p.Name, &p.Archived, &p.CreatedAt, &p.UpdatedAt, &w.WorkspaceKind, &w.WorkspaceKey, &w.DisplayPath); err != nil {
			return nil, err
		}
		w.ProjectID = p.ID
		cur := byID[p.ID]
		if cur == nil {
			p.Workspaces = []ProjectWorkspace{}
			cur = &p
			byID[p.ID] = cur
		}
		cur.Workspaces = append(cur.Workspaces, w)
		if projectWorkspaceContains(w, target) && len(w.WorkspaceKey) > longest {
			best = cur
			longest = len(w.WorkspaceKey)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// 归档绑定也参与最长匹配，绝不回退外层项目。
	return best, nil
}

func (s *Store) ResolveProjectForWorkspace(target ProjectWorkspace) (*Project, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return resolveProjectWorkspaceReader(s.db, target)
}

func (a *App) resolveProjectWorkspace(target ProjectWorkspace) (*Project, error) {
	return a.Store.ResolveProjectForWorkspace(target)
}

func (a *App) resolveSessionProject(sess *SessionMeta) (*Project, error) {
	w, ok, err := a.sessionProjectTarget(sess)
	if err != nil || !ok {
		return nil, err
	}
	return a.resolveProjectWorkspace(w)
}

// resolveSessionProjectIn 是归属解析的事务内形态：只用传入的 reader 查询，
// 供写事务复核「授权/归属是否在事务内仍然成立」（不能持 Store 锁重入 Store）。
func (a *App) resolveSessionProjectIn(q projectReader, sess *SessionMeta) (*Project, error) {
	w, ok, err := a.sessionProjectTarget(sess)
	if err != nil || !ok {
		return nil, err
	}
	return resolveProjectWorkspaceReader(q, w)
}

// sessionProjectTarget 取会话的归属目标工作区；ok=false 表示无法确认（默认工作区无效）。
func (a *App) sessionProjectTarget(sess *SessionMeta) (ProjectWorkspace, bool, error) {
	if sess == nil {
		return ProjectWorkspace{}, false, nil
	}
	// 默认工作区必须有效才能参与归属；不将空值或相对路径猜成进程当前目录。
	usingDefault := sess.Remote == nil && sess.Workspace == ""
	if usingDefault && (a.Cfg == nil || strings.ContainsRune(a.Cfg.Workspace, 0) || !filepath.IsAbs(strings.TrimSpace(a.Cfg.Workspace))) {
		return ProjectWorkspace{}, false, nil
	}
	w, err := a.sessionProjectWorkspace(sess)
	if err != nil {
		return ProjectWorkspace{}, false, err
	}
	return w, true, nil
}

func (a *App) projectKnowledgeEnabled(sess *SessionMeta) bool {
	if a.Components == nil || sess == nil {
		return false
	}
	if _, ok := a.Components.Manifest(projectKnowledgeComponentID); !ok {
		return false
	}
	return a.cxoComponentEnabled(sess.ID, projectKnowledgeComponentID)
}

func projectReadableMode(mode string) bool {
	switch mode {
	case "assistant", "task", "office", "coding", "beginner-coding", "skill-create", "aux":
		return true
	}
	return false
}
