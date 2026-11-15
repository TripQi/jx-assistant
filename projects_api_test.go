package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func projectAPIRequest(a *App, method, url string, body any, ai bool) *httptest.ResponseRecorder {
	b, _ := json.Marshal(body)
	r := httptest.NewRequest(method, url, bytes.NewReader(b))
	if ai {
		r.Header.Set("X-JX-Caller-Session", "assistant")
	}
	mux := http.NewServeMux()
	a.registerProjectRoutes(mux)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	return w
}
func TestProjectSettingsAPIValidationAndDraftConflict(t *testing.T) {
	a := projectCapabilityTestApp(t)
	w := projectAPIRequest(a, "POST", "/api/projects", map[string]any{"name": "设置项目"}, false)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	var p Project
	if e := json.Unmarshal(w.Body.Bytes(), &p); e != nil {
		t.Fatal(e)
	}
	dir := filepath.Join(t.TempDir(), "app")
	w = projectAPIRequest(a, "PATCH", "/api/projects/"+p.ID, map[string]any{"workspaces": []any{map[string]any{"workspace_kind": "local", "display_path": dir}}}, false)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = projectAPIRequest(a, "PUT", "/api/projects/"+p.ID+"/knowledge", map[string]any{"content": "中文", "budget_limit": 4, "expected_revision": 1}, false)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	before := projectTestKnowledge(t, a.Store, p.ID)
	for _, tc := range []struct {
		body any
		code int
	}{
		{map[string]any{"content": "draft", "expected_revision": 1}, 409},
		{map[string]any{"content": "中文a", "expected_revision": 2}, 400},
		{map[string]any{"content": "中文", "budget_limit": 3, "expected_revision": 2}, 400},
		{map[string]any{"content": "中文", "budget_limit": 0, "expected_revision": 2}, 400},
		{map[string]any{"content": "中文", "expected_revision": 2, "updated_at": "forged"}, 400},
	} {
		w = projectAPIRequest(a, "PUT", "/api/projects/"+p.ID+"/knowledge", tc.body, false)
		if w.Code != tc.code {
			t.Fatalf("want %d got %d %s", tc.code, w.Code, w.Body.String())
		}
		if got := projectTestKnowledge(t, a.Store, p.ID); got != before {
			t.Fatal("failed API changed state")
		}
	}
	w = projectAPIRequest(a, "POST", "/api/projects/budget", map[string]any{"content": "汉あア한𠀀abcde", "budget_limit": 10}, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"used":12`) {
		t.Fatal(w.Body.String())
	}
	w = projectAPIRequest(a, "POST", "/api/projects/resolve", map[string]any{"workspace_kind": "local", "display_path": filepath.Join(dir, "child")}, false)
	if w.Code != 200 || !strings.Contains(w.Body.String(), p.ID) {
		t.Fatal(w.Body.String())
	}
	w = projectAPIRequest(a, "DELETE", "/api/projects/"+p.ID, map[string]any{"confirm": false}, false)
	if w.Code != 400 {
		t.Fatal("unconfirmed deletion accepted")
	}
	w = projectAPIRequest(a, "DELETE", "/api/projects/"+p.ID, map[string]any{"confirm": true}, false)
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	w = projectAPIRequest(a, "GET", "/api/projects/"+p.ID+"/knowledge", nil, false)
	if w.Code != 404 {
		t.Fatal("deleted ID did not fail clearly")
	}
}
func TestProjectSettingsNoAICallAPIChannel(t *testing.T) {
	a := projectCapabilityTestApp(t)
	for _, tc := range []struct {
		method, url string
		body        any
	}{
		{"GET", "/api/projects", nil}, {"POST", "/api/projects", map[string]any{"name": "forbidden"}},
		{"PATCH", "/api/projects/fake", map[string]any{"name": "forbidden"}}, {"DELETE", "/api/projects/fake", map[string]any{"confirm": true}},
		{"PUT", "/api/projects/fake/knowledge", map[string]any{"content": "forbidden", "expected_revision": 1}},
	} {
		w := projectAPIRequest(a, tc.method, tc.url, tc.body, true)
		if w.Code != 403 {
			t.Fatalf("AI setting access %s %s =>%d", tc.method, tc.url, w.Code)
		}
	}
	a.registerProjectRoutes(http.NewServeMux())
	if len(a.apiMetas) != 0 {
		t.Fatal("project routes leaked to AI catalog")
	}
	list, e := a.Store.ListProjects(true)
	if e != nil || len(list) != 0 {
		t.Fatal("AI created project")
	}
}
