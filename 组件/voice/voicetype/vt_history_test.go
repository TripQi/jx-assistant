//go:build windows

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func voiceHistoryTemp(t *testing.T) {
	t.Helper()
	old := vtDir
	vtDir = t.TempDir()
	t.Cleanup(func() { vtDir = old })
}

// Check the exact bytes consumed by playback and by reidentify, not just names.
func voiceHistoryAudio(t *testing.T, item vtHistoryItem, pcm []byte) {
	t.Helper()
	wav, err := os.ReadFile(filepath.Join(vtDir, filepath.FromSlash(item.Audio)))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wav, vtPCMToWav(pcm)) {
		t.Fatalf("playback WAV mismatch for %q: %x", item.Audio, wav)
	}
	if !bytes.Equal(vtWavToPCM(wav), pcm) {
		t.Fatalf("reidentify PCM mismatch for %q", item.Audio)
	}
}

func TestVoiceHistorySameMillisecond(t *testing.T) {
	voiceHistoryTemp(t)
	now := time.Date(2026, 11, 15, 11, 12, 13, 456000000, time.Local)
	first, second := []byte{1, 2, 3, 4}, []byte{5, 6, 7, 8}
	vtSaveHistoryAt(first, "first", "copied", now)
	vtSaveHistoryAt(second, "second", "asr_failed", now)
	items := vtReadHistory()
	if len(items) != 2 {
		t.Fatalf("got %d records, want 2", len(items))
	}
	if items[0].Audio == "" || items[1].Audio == "" || items[0].Audio == items[1].Audio {
		t.Fatalf("same-millisecond audio references must be distinct: %+v", items)
	}
	for i, want := range []struct{ text, status string }{{"first", "copied"}, {"second", "asr_failed"}} {
		if items[i].Text != want.text || items[i].Status != want.status || items[i].Time != "2026-11-15 11:12:13" {
			t.Fatalf("record %d mismatch: %+v", i, items[i])
		}
		if !strings.HasPrefix(items[i].Audio, "history/20261115-111213.456") || !strings.HasSuffix(items[i].Audio, ".wav") {
			t.Fatalf("unexpected audio reference: %q", items[i].Audio)
		}
	}
	voiceHistoryAudio(t, items[0], first)
	voiceHistoryAudio(t, items[1], second)
	files, err := os.ReadDir(filepath.Join(vtDir, "history"))
	if err != nil || len(files) != 2 {
		t.Fatalf("want two audio files, got %d: %v", len(files), err)
	}
}

func TestVoiceHistoryLegacyCompatibility(t *testing.T) {
	voiceHistoryTemp(t)
	if err := os.MkdirAll(filepath.Join(vtDir, "history"), 0755); err != nil {
		t.Fatal(err)
	}
	// Literal old JSON: both the pre-status injected field and current status field.
	legacy := `[
		{"time":"2026-11-15 11:12:13","text":"legacy injected","injected":true,"audio":"history/20261115-111213.456.wav"},
		{"time":"2026-11-14 10:00:00","text":"legacy copied","status":"copied","audio":"history/20261114-100000.000.wav"}
	]`
	if err := os.WriteFile(filepath.Join(vtDir, "history.json"), []byte(legacy), 0644); err != nil {
		t.Fatal(err)
	}
	first, second, fresh := []byte{10, 11}, []byte{12, 13}, []byte{14, 15}
	for name, pcm := range map[string][]byte{"20261115-111213.456.wav": first, "20261114-100000.000.wav": second} {
		if err := os.WriteFile(filepath.Join(vtDir, "history", name), vtPCMToWav(pcm), 0644); err != nil {
			t.Fatal(err)
		}
	}
	before := vtReadHistory()
	if len(before) != 2 || before[0].Text != "legacy injected" || before[1].Status != "copied" {
		t.Fatalf("legacy JSON not readable: %+v", before)
	}
	voiceHistoryAudio(t, before[0], first)
	voiceHistoryAudio(t, before[1], second)
	// The new recording even shares the old filename's millisecond prefix.
	vtSaveHistoryAt(fresh, "new", "copied", time.Date(2026, 11, 15, 11, 12, 13, 456000000, time.Local))
	after := vtReadHistory()
	if len(after) != 3 || after[0] != before[0] || after[1] != before[1] {
		t.Fatalf("legacy records/references changed: before=%+v after=%+v", before, after)
	}
	voiceHistoryAudio(t, after[0], first)
	voiceHistoryAudio(t, after[1], second)
	voiceHistoryAudio(t, after[2], fresh)
}
