package main

import (
	"strings"
	"testing"
)

func TestP1F24SummaryTransaction(t *testing.T) {
	for _, failure := range []string{"insert", "update", "none"} {
		t.Run(failure, func(t *testing.T) {
			a := newCompactTestApp(t)
			a.initRuntime()
			a.Cfg.Providers = []ProviderCfg{{ID: "test", BaseURL: "mock", Models: []ModelCfg{{ID: "model"}}}}
			a.Cfg.DefaultModel = ModelRef{Provider: "test", Model: "model"}
			sess := &SessionMeta{ID: "atomic", Mode: "task", CreatedAt: now(), UpdatedAt: now()}
			if err := a.Store.Create(sess); err != nil {
				t.Fatal(err)
			}
			seedRound(t, a, sess.ID, "original", false)
			for _, origin := range []string{"auto", "manual"} {
				if err := a.Store.AppendRow(sess.ID, &StoredMessage{Message: TextMessage("system", "old-"+origin), Kind: kindSummary, Origin: origin, CoversUpTo: 2, CoversFrom: 1, CreatedAt: now()}); err != nil {
					t.Fatal(err)
				}
			}
			seedRound(t, a, sess.ID, "new-original", false)
			if failure != "none" {
				trigger := `CREATE TRIGGER fail_summary BEFORE INSERT ON messages WHEN NEW.kind='summary' BEGIN SELECT RAISE(ABORT, 'injected summary insert failure'); END`
				if failure == "update" {
					trigger = `CREATE TRIGGER fail_summary BEFORE UPDATE OF superseded ON messages BEGIN SELECT RAISE(ABORT, 'injected summary update failure'); END`
				}
				if _, err := a.Store.db.Exec(trigger); err != nil {
					t.Fatal(err)
				}
			}
			_, err := a.doSummarize(sess, "auto", 0, 0)
			rows, rerr := a.Store.RawMessagesAll(sess.ID)
			if rerr != nil {
				t.Fatal(rerr)
			}
			var oldAutoSup, manualSup bool
			summaries, originals := 0, 0
			for _, r := range rows {
				if r.Kind == kindSummary {
					summaries++
				}
				if r.Text() == "old-auto" {
					oldAutoSup = r.Superseded
				}
				if r.Text() == "old-manual" {
					manualSup = r.Superseded
				}
				if r.Kind == kindChat {
					originals++
				}
			}
			t.Logf("failure=%s err=%v oldAutoSuperseded=%v manualSuperseded=%v summaries=%d originalRows=%d", failure, err, oldAutoSup, manualSup, summaries, originals)
			if originals != 4 || manualSup {
				t.Fatal("original history/manual summary changed")
			}
			if failure != "none" {
				if err == nil || !strings.Contains(err.Error(), "injected summary") || oldAutoSup || summaries != 2 {
					t.Fatal("failed persistence must return error and roll back old summary")
				}
			} else {
				if err != nil || !oldAutoSup || summaries != 3 {
					t.Fatal("auto replacement failed")
				}
				// Manual compression stacks without superseding any existing summary.
				seedRound(t, a, sess.ID, "manual-tail", false)
				if _, err := a.doSummarize(sess, "manual", 0, 0); err != nil {
					t.Fatal(err)
				}
				all, err := a.Store.SummariesOf(sess.ID)
				if err != nil {
					t.Fatal(err)
				}
				activeAuto, activeManual := 0, 0
				for _, r := range all {
					if !r.Superseded {
						if r.Origin == "auto" {
							activeAuto++
						}
						if r.Origin == "manual" {
							activeManual++
						}
					}
				}
				if activeAuto != 1 || activeManual != 2 {
					t.Fatalf("manual stacking changed: auto=%d manual=%d", activeAuto, activeManual)
				}
			}
		})
	}
}
