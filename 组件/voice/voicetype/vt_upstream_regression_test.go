//go:build windows

package main

import (
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestVoiceUpstreamPython(t *testing.T) {
	old := vtDir
	defer func() { vtDir = old }()
	root := t.TempDir()
	exeDir := filepath.Join(root, "安装 目录", "组件", "voice", "voicetype")
	shared := filepath.Clean(filepath.Join(exeDir, "..", "..", "..", "技能", "_runtime", "python", "python.exe"))
	local := filepath.Join(exeDir, "runtime", "python", "python.exe")
	pathPy := filepath.Join(root, "path", "python.exe")
	for _, p := range []string{shared, pathPy} {
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("fake executable"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", filepath.Dir(pathPy))
	t.Setenv("PATHEXT", ".EXE")
	vtDir = filepath.Join(root, "wrong-cwd")
	if got := findPythonAt(exeDir); got != shared {
		t.Fatalf("shared must beat PATH/cwd: got %q want %q", got, shared)
	}
	if err := os.MkdirAll(filepath.Dir(local), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(local, []byte("fake"), 0644); err != nil {
		t.Fatal(err)
	}
	if got := findPythonAt(exeDir); got != local {
		t.Fatalf("local must beat shared: %q", got)
	}
}

func TestVoiceUpstreamBudget(t *testing.T) {
	for _, c := range []struct{ audio, want int }{{0, 15}, {2, 15}, {7, 15}, {10, 18}, {30, 38}, {210, 218}, {270, 278}, {300, 308}, {1000, 1008}} {
		if got := vtAsrTimeoutSec(c.audio); got != c.want {
			t.Errorf("audio=%d got=%d want=%d", c.audio, got, c.want)
		}
	}
}

type voiceRoundTrip func(*http.Request) (*http.Response, error)

func (f voiceRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func voiceResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}
}
func voiceTransport(t *testing.T, f voiceRoundTrip) {
	old, ready := http.DefaultTransport, asrReady.Load()
	http.DefaultTransport = f
	t.Cleanup(func() { http.DefaultTransport = old; asrReady.Store(ready) })
}

func TestVoiceUpstreamDeadlineShared(t *testing.T) {
	deadline := time.Now().Add(2500 * time.Millisecond)
	calls := 0
	voiceTransport(t, func(r *http.Request) (*http.Response, error) {
		calls++
		if d, ok := r.Context().Deadline(); !ok || !d.Equal(deadline) {
			t.Errorf("attempt %d deadline=%v/%v want=%v", calls, d, ok, deadline)
		}
		if calls == 1 {
			return voiceResponse(500, "retry"), nil
		}
		return voiceResponse(200, `{"text":"ok"}`), nil
	})
	asrReady.Store(false)
	done := make(chan struct{})
	go func() { time.Sleep(60 * time.Millisecond); asrReady.Store(true); close(done) }()
	text, err := vtTranscribe(nil, deadline)
	<-done
	if err != nil || text != "ok" || calls != 2 {
		t.Fatalf("text=%q err=%v attempts=%d", text, err, calls)
	}
}

func TestVoiceUpstreamDeadlineExpired(t *testing.T) {
	voiceTransport(t, func(r *http.Request) (*http.Response, error) { return voiceResponse(200, `{"text":"late"}`), nil })
	asrReady.Store(false)
	done := make(chan struct{})
	go func() { time.Sleep(30 * time.Millisecond); asrReady.Store(true); close(done) }()
	text, err := vtTranscribe(nil, time.Now().Add(-time.Second))
	<-done
	if !errors.Is(err, vtErrTimeout) || text != "" {
		t.Fatalf("expired readiness must timeout: text=%q err=%v", text, err)
	}
}

type voiceSlowBody struct{ r *http.Request }

func (b voiceSlowBody) Read(p []byte) (int, error) {
	select {
	case <-b.r.Context().Done():
		return 0, b.r.Context().Err()
	case <-time.After(100 * time.Millisecond):
		return 0, context.DeadlineExceeded
	}
}
func (b voiceSlowBody) Close() error { return nil }
func TestVoiceUpstreamBodyTimeout(t *testing.T) {
	voiceTransport(t, func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: voiceSlowBody{r}, Header: make(http.Header)}, nil
	})
	asrReady.Store(true)
	_, err := vtTranscribe(nil, time.Now().Add(40*time.Millisecond))
	if !errors.Is(err, vtErrTimeout) {
		t.Fatalf("body timeout classification: %v", err)
	}
}

func TestVoiceUpstreamLateNoDelivery(t *testing.T) {
	finished := make(chan struct{})
	delivered := 0
	text, err := vtTranscribeUntil(nil, time.Now().Add(30*time.Millisecond), func(_ []byte, _ time.Time) (string, error) {
		time.Sleep(60 * time.Millisecond)
		defer close(finished)
		return "late", nil
	})
	if err == nil && text != "" {
		delivered++
	}
	<-finished
	if !errors.Is(err, vtErrTimeout) || text != "" || delivered != 0 {
		t.Fatalf("late result delivered=%d text=%q err=%v", delivered, text, err)
	}
	// An already expired deadline must not start work, including when both select arms could be ready.
	started := false
	_, err = vtTranscribeUntil(nil, time.Now().Add(-time.Second), func(_ []byte, _ time.Time) (string, error) { started = true; return "late", nil })
	if started || !errors.Is(err, vtErrTimeout) {
		t.Fatalf("expired work started=%v err=%v", started, err)
	}
}
