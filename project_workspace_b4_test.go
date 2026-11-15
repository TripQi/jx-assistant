package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// Settings bindings and persisted session identities must agree even when the
// legacy Display output is ambiguous (notably an unbracketed IPv6 host).
func TestB4RemoteSettingsSessionKnowledgeAndContext(t *testing.T) {
	cases := []struct {
		name, display, key string
		remote             RemoteWorkspace
	}{
		{"ipv6-default", "u@[2001:db8::1]:/srv/app/", "u@[2001:db8::1]:22:/srv/app", RemoteWorkspace{User: "u", Host: "2001:0DB8:0:0:0:0:0:1", Dir: "/srv/app/module"}},
		{"ipv6-bracketed-session", "u@[2001:db8::1]:22:/srv/app", "u@[2001:db8::1]:22:/srv/app", RemoteWorkspace{User: "u", Host: "[2001:0db8:0:0:0:0:0:1]", Port: 22, Dir: "/srv/app/module"}},
		{"ipv6-port", "u@[2001:0db8:0:0:0:0:0:1]:2222:/srv/app", "u@[2001:db8::1]:2222:/srv/app", RemoteWorkspace{User: "u", Host: "2001:db8::1", Port: 2222, Dir: "/srv/app/module"}},
		{"hostname-zero-port", "u@EXAMPLE.COM:22:/srv/app", "u@example.com:22:/srv/app", RemoteWorkspace{User: "u", Host: "example.com", Port: 0, Dir: "/srv/app/module"}},
		{"hostname-explicit-port", "u@example.com:/srv/app", "u@example.com:22:/srv/app", RemoteWorkspace{User: "u", Host: "EXAMPLE.COM", Port: 22, Dir: "/srv/app/module"}},
		{"display-zero-port", "u@example.com:0:/srv/app", "u@example.com:22:/srv/app", RemoteWorkspace{User: "u", Host: "example.com", Port: 22, Dir: "/srv/app/module"}},
		{"remote-default-user-dir", "root@example.com:/", "root@example.com:22:/", RemoteWorkspace{Host: "example.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a, sess, local := knowledgeEnvTestApp(t)
			response := projectAPIRequest(a, "POST", "/api/projects", map[string]any{"name": "B4 remote"}, false)
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			var p Project
			if err := json.Unmarshal(response.Body.Bytes(), &p); err != nil {
				t.Fatal(err)
			}
			response = projectAPIRequest(a, "PATCH", "/api/projects/"+p.ID, map[string]any{
				"expected_updated_at": p.UpdatedAt,
				"workspaces":          []any{map[string]any{"workspace_kind": "remote", "display_path": tc.display}},
			}, false)
			if response.Code != 200 {
				t.Fatal(response.Body.String())
			}
			stored, err := a.Store.GetProject(p.ID)
			if err != nil || len(stored.Workspaces) != 1 || stored.Workspaces[0].WorkspaceKey != tc.key {
				t.Fatalf("settings binding: %+v %v, want %s", stored, err, tc.key)
			}
			response = projectAPIRequest(a, "POST", "/api/projects/resolve", map[string]any{"workspace_kind": "remote", "display_path": tc.display}, false)
			if response.Code != 200 || !strings.Contains(response.Body.String(), p.ID) {
				t.Fatalf("settings resolve: %d %s", response.Code, response.Body.String())
			}
			saveKnowledgeEnvTest(t, a, p.ID, "B4 remote knowledge only")
			sess.Remote = &tc.remote
			// A valid local project remains present: remote identity must win.
			if err := a.Store.SaveMeta(sess); err != nil {
				t.Fatal(err)
			}
			persisted, err := a.Store.Get(sess.ID)
			if err != nil {
				t.Fatal(err)
			}
			resolved, err := a.resolveSessionProject(persisted)
			if err != nil || resolved == nil || resolved.ID != p.ID {
				t.Fatalf("persisted remote session resolve: %+v %v", resolved, err)
			}
			out, err := runProjectKnowledgeGet(&toolContext{app: a, sess: persisted}, json.RawMessage(`{}`))
			if err != nil || !strings.Contains(out, p.ID) || !strings.Contains(out, "B4 remote knowledge only") {
				t.Fatalf("persisted remote knowledge query: %s %v", out, err)
			}
			block, err := a.projectKnowledgeEnv(persisted)
			if err != nil {
				t.Fatal(err)
			}
			requireKnowledgeText(t, block, p.ID, "B4 remote knowledge only")
			if strings.Contains(block, local.ID) || strings.Contains(block, "整段约束") {
				t.Fatal("remote session leaked local knowledge")
			}
			if err := a.validateProjectKnowledgeContext(persisted, LLMTarget{ContextWindow: 1000000}); err != nil {
				t.Fatalf("remote knowledge context rejected: %v", err)
			}
			requireKnowledgeText(t, a.envPrompt(persisted), p.ID, "B4 remote knowledge only")
			// Credentials must never participate in project identity.
			persisted.Remote.Auth, persisted.Remote.Password, persisted.Remote.KeyPath = "password", "not-a-real-password", "unused-key"
			resolved, err = a.resolveSessionProject(persisted)
			if err != nil || resolved == nil || resolved.ID != p.ID {
				t.Fatalf("auth affected identity: %+v %v", resolved, err)
			}
		})
	}
}

func TestB4RemoteIdentityIsolationAndDirectoryBoundary(t *testing.T) {
	a, sess, _ := knowledgeEnvTestApp(t)
	identities := []struct {
		user, host string
		port       int
	}{
		{"u", "2001:db8::1", 22},
		{"other", "2001:db8::1", 22},
		{"u", "2001:db8::1", 2222},
		{"u", "2001:db8::2", 22},
		{"U", "2001:db8::1", 22},
	}
	for i, id := range identities {
		binding, err := normalizeProjectWorkspace("remote", fmt.Sprintf("%s@[%s]:%d:/srv/app", id.user, id.host, id.port))
		if err != nil {
			t.Fatal(err)
		}
		p, err := a.Store.CreateProject(Project{ID: fmt.Sprintf("b4-isolation-%d", i), Name: "remote", Workspaces: []ProjectWorkspace{binding}})
		if err != nil {
			t.Fatal(err)
		}
		sess.Remote = &RemoteWorkspace{User: id.user, Host: id.host, Port: id.port, Dir: "/srv/app/sub/../module/"}
		got, err := a.resolveSessionProject(sess)
		if err != nil || got == nil || got.ID != p.ID {
			t.Fatalf("target %d resolved %+v %v", i, got, err)
		}
		sess.Remote.Dir = "/srv/app-old"
		if got, err := a.resolveSessionProject(sess); err != nil || got != nil {
			t.Fatalf("directory boundary crossed: %+v %v", got, err)
		}
	}
}

func TestB4InvalidRemoteTargetsFailClosed(t *testing.T) {
	a, sess, local := knowledgeEnvTestApp(t)
	invalidDisplays := []string{
		"u@2001:db8::1:/srv/app", "u@[2001:db8::zz]:/srv/app", "u@[2001:db8::1:/srv/app",
		"u@[2001:db8::1]extra:/srv/app", "u@[2001:db8::1]::/srv/app", "u@[2001:db8::1]:/relative\x00",
		"u@host::/srv/app", "u@host:-1:/srv/app", "u@host:65536:/srv/app", "u@host:+22:/srv/app",
		"u@host:port:/srv/app", "u@@host:/srv/app", "u@:/srv/app", "@host:/srv/app",
		"u@bad host:/srv/app", "u@bad\rhost:/srv/app", "u@bad\x00host:/srv/app", "u@host:relative",
	}
	for _, display := range invalidDisplays {
		t.Run(display, func(t *testing.T) {
			if _, err := normalizeProjectWorkspace("remote", display); err == nil {
				t.Fatal("invalid display accepted")
			}
			response := projectAPIRequest(a, "PATCH", "/api/projects/"+local.ID, map[string]any{
				"workspaces": []any{map[string]any{"workspace_kind": "remote", "display_path": display}},
			}, false)
			if response.Code != 400 {
				t.Fatalf("invalid settings binding accepted: %d %s", response.Code, response.Body.String())
			}
			stored, err := a.Store.GetProject(local.ID)
			if err != nil || len(stored.Workspaces) != 1 || stored.Workspaces[0] != local.Workspaces[0] {
				t.Fatalf("invalid binding changed project: %+v %v", stored, err)
			}
		})
	}
	invalidRemotes := []RemoteWorkspace{
		{User: "u", Host: "2001:db8::zz", Dir: "/srv/app"},
		{User: "u", Host: "[2001:db8::1", Dir: "/srv/app"},
		{User: "u", Host: "host", Port: -1, Dir: "/srv/app"},
		{User: "u", Host: "host", Port: 65536, Dir: "/srv/app"},
		{User: "u@other", Host: "host", Dir: "/srv/app"},
		{User: "u\rname", Host: "host", Dir: "/srv/app"},
		{User: "u", Host: "host:2222", Dir: "/srv/app"},
		{User: "u", Host: "bad\x00host", Dir: "/srv/app"},
		{User: "u", Host: "", Dir: "/srv/app"},
		{User: "u", Host: "host", Dir: "relative"},
		{User: "u", Host: "host", Dir: "/srv/\x00app"},
	}
	for i := range invalidRemotes {
		sess.Remote = &invalidRemotes[i]
		if p, err := a.resolveSessionProject(sess); err == nil || p != nil {
			t.Fatalf("invalid remote %d fell back to project: %+v %v", i, p, err)
		}
		if _, err := a.projectKnowledgeEnv(sess); err == nil {
			t.Fatalf("invalid remote %d injected knowledge", i)
		}
		if err := a.validateProjectKnowledgeContext(sess, LLMTarget{ContextWindow: 1000000}); err == nil {
			t.Fatalf("invalid remote %d passed context validation", i)
		}
	}
}
