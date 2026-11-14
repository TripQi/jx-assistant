package main

import (
	"encoding/json"
	"testing"
)

func TestP1F12ManualDefaultPinsKey(t *testing.T) {
	a := newDefaultModelsApp(t)
	a.Cfg.DefaultModel = ModelRef{Provider: "router", Model: "shared"}
	p, k, m, err := a.applySessionModelDefault("", "", "")
	if err != nil || p != "router" || k != "k1" || m != "shared" {
		t.Fatalf("manual default not pinned: %s/%s/%s %v", p, k, m, err)
	}
}

func TestP1F12TodoSavePinsKey(t *testing.T) {
	a := newDefaultModelsApp(t)
	s := &todoScheduler{app: a, busy: map[string]bool{}, tombs: map[string]bool{}, data: &TodoData{Version: todoDataVersion}}
	req := todoRequest{Title: "pinned", Description: "isolated", Target: targetTask, Schedule: &Schedule{Kind: schedManual}, ModelMode: todoModelCustom, Model: &ModelRef{Provider: "router", Model: "shared"}}
	v, err := s.create(req)
	if err != nil {
		t.Fatal(err)
	}
	if v.Model == nil || v.Model.ProviderKey != "k1" {
		t.Fatalf("saved custom reference must pin k1: %+v", v.Model)
	}
	var persisted TodoData
	if err := json.Unmarshal([]byte(a.Store.GetGlobalData(todoDataKey)), &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.Tasks) != 1 || persisted.Tasks[0].Model.ProviderKey != "k1" {
		t.Fatalf("disk reference not pinned: %+v", persisted.Tasks)
	}
	// Reordering same-name keys cannot alter either display or execution resolution.
	a.Cfg.Providers[1].Keys[0], a.Cfg.Providers[1].Keys[1] = a.Cfg.Providers[1].Keys[1], a.Cfg.Providers[1].Keys[0]
	view := s.toView(persisted.Tasks[0], 0)
	p, k, m, _, err := a.todoDispatchModel(persisted.Tasks[0])
	if err != nil || view.ModelView.ProviderKey != "k1" || k != "k1" || p != "router" || m != "shared" {
		t.Fatalf("display/execution diverged: view=%+v exec=%s/%s/%s %v", view.ModelView, p, k, m, err)
	}
	v, err = s.update(todoRequest{ID: v.ID, Model: &ModelRef{Provider: "router", Model: "shared"}})
	if err != nil || v.Model.ProviderKey != "k2" {
		t.Fatalf("update must pin current selection k2: %+v %v", v, err)
	}
}
