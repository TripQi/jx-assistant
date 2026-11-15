package main

// 项目身份属于宿主，不随任一组件开关消失；解析只看目标会话自己的工作区。
import (
	"fmt"
	"net"
	"path"
	"path/filepath"
	"strconv"
	"strings"
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
		// 与 RemoteWorkspace.Display 相同的 user@host[:port]:/dir；身份不含认证秘密。
		i := strings.Index(w.DisplayPath, ":/")
		if i < 0 {
			return w, fmt.Errorf("远程绑定格式为 user@host[:port]:/绝对目录")
		}
		target, dir := w.DisplayPath[:i], w.DisplayPath[i+1:]
		at := strings.LastIndex(target, "@")
		if at <= 0 || at == len(target)-1 {
			return w, fmt.Errorf("远程绑定需要 user@host")
		}
		user, hostport := target[:at], target[at+1:]
		host, port := hostport, 22
		if h, p, e := net.SplitHostPort(hostport); e == nil {
			host = h
			n, e := strconv.Atoi(p)
			if e != nil || n < 1 || n > 65535 {
				return w, fmt.Errorf("远程端口必须在 1—65535")
			}
			port = n
		} else if strings.Contains(hostport, ":") {
			return w, fmt.Errorf("远程端口格式无效，IPv6 地址请使用方括号")
		}
		if host == "" || strings.ContainsAny(host, " /\\\t\n") || strings.ContainsAny(user, " /\\\t\n") {
			return w, fmt.Errorf("远程连接目标无效")
		}
		w.WorkspaceKey = user + "@" + net.JoinHostPort(strings.ToLower(host), strconv.Itoa(port)) + ":" + path.Clean(dir)
	default:
		return w, fmt.Errorf("workspace_kind 只能是 local 或 remote")
	}
	return w, nil
}

func (a *App) sessionProjectWorkspace(sess *SessionMeta) (ProjectWorkspace, error) {
	if sess == nil {
		return ProjectWorkspace{}, fmt.Errorf("缺少有效会话")
	}
	if sess.Remote != nil {
		return normalizeProjectWorkspace("remote", sess.Remote.Display())
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

func (a *App) resolveProjectWorkspace(target ProjectWorkspace) (*Project, error) {
	projects, err := a.Store.ListProjects(true)
	if err != nil {
		return nil, err
	}
	var best *Project
	longest := -1
	for i := range projects {
		for _, w := range projects[i].Workspaces {
			if projectWorkspaceContains(w, target) && len(w.WorkspaceKey) > longest {
				p := projects[i]
				best = &p
				longest = len(w.WorkspaceKey)
			}
		}
	}
	// 归档绑定也参与最长匹配，绝不回退外层项目。
	return best, nil
}

func (a *App) resolveSessionProject(sess *SessionMeta) (*Project, error) {
	if sess == nil {
		return nil, nil
	}
	// 默认工作区必须有效才能参与归属；不将空值或相对路径猜成进程当前目录。
	usingDefault := sess.Remote == nil && sess.Workspace == ""
	if usingDefault && (a.Cfg == nil || strings.ContainsRune(a.Cfg.Workspace, 0) || !filepath.IsAbs(strings.TrimSpace(a.Cfg.Workspace))) {
		return nil, nil
	}
	w, err := a.sessionProjectWorkspace(sess)
	if err != nil {
		return nil, err
	}
	return a.resolveProjectWorkspace(w)
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
