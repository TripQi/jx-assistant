//go:build windows

package main

import (
	"errors"
	"net/http"
	"testing"
	"time"
)

func TestVoiceUpstreamWaitingTimeout(t *testing.T) {
	voiceTransport(t, func(r *http.Request) (*http.Response, error) {
		t.Error("HTTP must not run before ready")
		return voiceResponse(200, `{"text":"bad"}`), nil
	})
	asrReady.Store(false)
	done := make(chan struct{})
	go func() { time.Sleep(100 * time.Millisecond); asrReady.Store(true); close(done) }()
	start := time.Now()
	_, err := vtTranscribeUntil(nil, start.Add(40*time.Millisecond), func(p []byte, d time.Time) (string, error) { return vtTranscribe(p, d) })
	<-done
	if !errors.Is(err, vtErrTimeout) || time.Since(start) > time.Second {
		t.Fatalf("readiness timeout: %v elapsed=%v", err, time.Since(start))
	}
}

func TestVoiceUpstreamHTTPTimeout(t *testing.T) {
	voiceTransport(t, func(r *http.Request) (*http.Response, error) {
		select {
		case <-r.Context().Done():
			return nil, r.Context().Err()
		case <-time.After(100 * time.Millisecond):
			return voiceResponse(200, `{"text":"late"}`), nil
		}
	})
	asrReady.Store(true)
	text, err := vtTranscribe(nil, time.Now().Add(40*time.Millisecond))
	if !errors.Is(err, vtErrTimeout) || text != "" {
		t.Fatalf("HTTP timeout: text=%q err=%v", text, err)
	}
}
