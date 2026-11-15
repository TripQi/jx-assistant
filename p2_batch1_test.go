package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestP2L02CompactOutcome(t *testing.T) {
	for _, name := range []string{"empty", "short", "two-tools", "success", "corrected"} {
		t.Run(name, func(t *testing.T) {
			hops := 0
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				hops++
				w.Header().Set("Content-Type", "text/event-stream")
				tool := name == "two-tools" || (name == "corrected" && hops == 1)
				if tool {
					fmt.Fprint(w, "data: {\"choices\":[{\"finish_reason\":\"tool_calls\",\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c\",\"type\":\"function\",\"function\":{\"name\":\"read\",\"arguments\":\"{}\"}}]}}]}\n\n")
				} else {
					text := strings.Repeat("摘要", 30)
					if name == "empty" {
						text = " "
					}
					if name == "short" {
						text = "短摘要"
					}
					b, _ := json.Marshal(text)
					fmt.Fprintf(w, "data: {\"choices\":[{\"finish_reason\":\"stop\",\"delta\":{\"content\":%s}}]}\n\n", b)
				}
				fmt.Fprint(w, sseUsage(100, 80, 10))
			}))
			defer srv.Close()
			a := newUsageTestApp(t)
			a.Cfg.Providers = []ProviderCfg{{ID: "p", BaseURL: srv.URL, APIKey: "synthetic", Models: []ModelCfg{{ID: "m"}}}}
			a.Cfg.DefaultModel = ModelRef{Provider: "p", Model: "m"}
			sess := &SessionMeta{ID: "outcome", Mode: "task", CreatedAt: now(), UpdatedAt: now()}
			if err := a.Store.Create(sess); err != nil {
				t.Fatal(err)
			}
			_, err := a.ctxEngine().CallSummarizer(sess, []Message{TextMessage("user", "摘要")}, nil)
			success := name == "success" || name == "corrected"
			if (err == nil) != success {
				t.Fatalf("unexpected business result: %v", err)
			}
			waitUsageRows(t, a, hops)
			fees, total, ferr := a.Store.UsageRecords(usageFilter{}, 10, 0)
			if ferr != nil || total != hops {
				t.Fatalf("fees=%d hops=%d err=%v", total, hops, ferr)
			}
			for _, f := range fees {
				if !f.OK || f.Input != 100 || f.Output != 10 || f.Purpose != usagePurposeCompact {
					t.Fatalf("per-hop accounting changed: %+v", f)
				}
			}
			rows, _ := a.Store.RawMessagesAll(sess.ID)
			outcomes := 0
			for _, row := range rows {
				var p map[string]any
				if row.Kind != kindAudit || json.Unmarshal([]byte(row.Text()), &p) != nil || p["kind"] != "compact_outcome" {
					continue
				}
				outcomes++
				want := "success"
				if !success {
					want = "failure"
				}
				if p["outcome"] != want {
					t.Errorf("outcome=%v want=%s", p["outcome"], want)
				}
				if err != nil && (p["error"] != err.Error() || !strings.Contains(fmt.Sprint(p["detail"]), err.Error())) {
					t.Errorf("final reason lost: %v", p)
				}
			}
			if outcomes != 1 {
				t.Fatalf("compact outcome count=%d want=1 (fee rows=%d businessErr=%v)", outcomes, total, err)
			}
			timeline := a.ctxEngine().ExportTimeline(sess)
			if !strings.Contains(timeline, "compact_outcome") {
				t.Fatal("outcome missing from exported log")
			}
			t.Logf("outcome=%v feeRows=%d perHopOK=true finalError=%v", success, total, err)
		})
	}
}

func TestP2L03TodoFailedSource(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "zhipu", Model: "glm-5.3"}
	a.Cfg.DispatchModel = ModelRef{Provider: "router", ProviderKey: "k1", Model: "missing-B"}
	v := a.todoModelView(todoModelFollowDispatch, ModelRef{})
	if v.Available || v.Source != "派发任务默认模型" || v.Provider != "router" || v.ProviderKey != "k1" || v.Model != "missing-B" || !strings.Contains(v.Text, "missing-B") || !strings.Contains(v.Error, "missing-B") {
		t.Fatalf("source mismatch: %+v", v)
	}
	a.Cfg.DispatchModel = ModelRef{}
	a.Cfg.DefaultModel.Model = "missing-A"
	v = a.todoModelView(todoModelFollowDispatch, ModelRef{})
	if v.Source != "全局默认模型" || v.Model != "missing-A" || !strings.Contains(v.Error, "missing-A") {
		t.Fatalf("global source mismatch: %+v", v)
	}
	t.Log("failed dispatch displays router/k1/missing-B; failed global displays zhipu/missing-A")
}

func TestP2L09TodoPartialModelAndTypeSwitch(t *testing.T) {
	a, s := newDoneTestApp(t)
	a.Cfg.Providers = []ProviderCfg{{ID: "p", BaseURL: "mock", Models: []ModelCfg{{ID: "m"}, {ID: "n"}}}}
	v, err := s.create(todoRequest{Title: "type switch", Description: "x", Target: targetTask, Schedule: &Schedule{Kind: schedManual}})
	if err != nil {
		t.Fatal(err)
	}
	ref := &ModelRef{Provider: "p", Model: "m"}
	v, err = s.update(todoRequest{ID: v.ID, Model: ref})
	if err != nil || v.ModelMode != todoModelCustom || v.Model == nil || v.Model.Model != "m" {
		t.Fatalf("model-only partial update lost: view=%+v err=%v", v, err)
	}
	v, err = s.update(todoRequest{ID: v.ID, Title: "preserve"})
	if err != nil || v.Model == nil || v.Model.Model != "m" {
		t.Fatal("omitted model must preserve")
	}
	v, err = s.update(todoRequest{ID: v.ID, ModelMode: "dispatch", Model: ref})
	if err != nil || v.Model != nil || v.ModelMode != todoModelFollowDispatch {
		t.Fatal("explicit follow must ignore stale model")
	}
	v, err = s.update(todoRequest{ID: v.ID, Model: ref})
	if err != nil {
		t.Fatal(err)
	}
	v, err = s.update(todoRequest{ID: v.ID, Executor: execCommand, Command: "echo ok", Dir: a.Cfg.Workspace})
	if err != nil || v.Model != nil || v.ModelMode != "" || v.Thinking != "" {
		t.Fatalf("command retains AI fields: %+v %v", v, err)
	}
	if _, err = s.update(todoRequest{ID: v.ID, Model: ref}); err == nil {
		t.Fatal("command model update must be rejected")
	}
	v, err = s.update(todoRequest{ID: v.ID, Executor: execAssistant, Target: targetTask})
	if err != nil || v.Model != nil || v.Command != "" || v.Dir != "" || v.Shell != "" || v.Permission != "" {
		t.Fatalf("AI resurrected stale fields: %+v %v", v, err)
	}
	for _, target := range []string{targetManager, targetTask} {
		v, err := s.create(todoRequest{Title: "explicit follow", Description: "x", Target: target, Schedule: &Schedule{Kind: schedManual}, ModelMode: "dispatch", Model: ref})
		if err != nil || v.Model != nil {
			t.Fatalf("create follow retains stale model (%s): %+v %v", target, v, err)
		}
		v, err = s.update(todoRequest{ID: v.ID, Model: ref})
		if err != nil || v.ModelMode != todoModelCustom || v.Model == nil {
			t.Fatalf("model-only update (%s): %+v %v", target, v, err)
		}
		v, err = s.update(todoRequest{ID: v.ID, ModelMode: "dispatch"})
		if err != nil || v.ModelMode != todoModelFollowDispatch || v.Model != nil {
			t.Fatalf("frontend follow update (%s): %+v %v", target, v, err)
		}
	}
	t.Log("model-only => custom; explicit dispatch ignores stale model; command clears AI fields; AI clears command fields")
}

func TestP2L10TodoLoadValidation(t *testing.T) {
	for _, version := range []int{0, 1, 2, 3} {
		t.Run(fmt.Sprintf("null-task-v%d", version), func(t *testing.T) {
			a, s := newDoneTestApp(t)
			raw := fmt.Sprintf(`{"version":%d,"tasks":[null]}`, version)
			if err := a.Store.SetGlobalData(todoDataKey, raw); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			old := log.Writer()
			log.SetOutput(&buf)
			defer log.SetOutput(old)
			d := s.load()
			if len(d.Tasks) != 0 {
				t.Fatal("invalid tasks accepted")
			}
			if a.Store.GetGlobalData(todoDataKey) != raw {
				t.Fatal("bad original overwritten")
			}
			if !strings.Contains(buf.String(), "tasks[0]") {
				t.Fatalf("diagnostic lacks location: %s", buf.String())
			}
			t.Log("rejected tasks[0]=null; original unchanged")
		})
	}
	for _, raw := range []string{`null`, `{"version":3,"records":[null]}`, `{broken`} {
		a, s := newDoneTestApp(t)
		_ = a.Store.SetGlobalData(todoDataKey, raw)
		d := s.load()
		if len(d.Tasks) != 0 || len(d.Records) != 0 || a.Store.GetGlobalData(todoDataKey) != raw {
			t.Fatal("bad document accepted or overwritten")
		}
	}
}

func TestP2L10TodoVersionZeroMigration(t *testing.T) {
	for _, version := range []string{"", `"version":0,`, `"version":1,`, `"version":2,`} {
		a, s := newDoneTestApp(t)
		raw := `{` + version + `"tasks":[{"id":"old","executor":"assistant","model":{"provider":"p","model":"m"}}]}`
		_ = a.Store.SetGlobalData(todoDataKey, raw)
		d := s.load()
		if d.Version != todoDataVersion || len(d.Tasks) != 1 || d.Tasks[0].ModelMode != todoModelCustom || d.Tasks[0].Model == nil {
			t.Fatalf("legacy model migration skipped (%s): %+v", version, d.Tasks[0])
		}
		t.Logf("legacy %q => version=%d mode=%s model=%s", version, d.Version, d.Tasks[0].ModelMode, d.Tasks[0].Model.Model)
	}
}
