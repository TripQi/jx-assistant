package main

import (
	"errors"
	"reflect"
	"testing"
)

func TestProjectMetadataStaleSnapshotRejected(t *testing.T) {
	s := projectTestStore(t)
	initial := projectTestCreate(t, s, "metadata")
	before := projectTestKnowledge(t, s, initial.ID)
	archive := initial
	archive.Archived = true
	latest, err := s.UpdateProject(archive)
	if err != nil {
		t.Fatal(err)
	}
	stale := initial
	stale.Name = "stale rename"
	if _, err = s.UpdateProject(stale); !errors.Is(err, ErrProjectMetadataConflict) {
		t.Fatalf("stale metadata must conflict, got %v", err)
	}
	got, err := s.GetProject(initial.ID)
	if err != nil || !reflect.DeepEqual(got, latest) {
		t.Fatalf("stale update reverted archive/bindings: %+v %v", got, err)
	}
	if k := projectTestKnowledge(t, s, initial.ID); k != before {
		t.Fatal("metadata changed shared knowledge revision/content/time")
	}
}

func TestProjectMetadataPatchFieldsAndConflict(t *testing.T) {
	a := projectCapabilityTestApp(t)
	p := projectBindTest(t, a, "metadata-api", t.TempDir())
	before := projectTestKnowledge(t, a.Store, p.ID)
	w := projectAPIRequest(a, "PATCH", "/api/projects/"+p.ID, map[string]any{"archived": true, "expected_updated_at": p.UpdatedAt}, false)
	if w.Code != 200 {
		t.Fatalf("archive: %d %s", w.Code, w.Body.String())
	}
	latest, err := a.Store.GetProject(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	w = projectAPIRequest(a, "PATCH", "/api/projects/"+p.ID, map[string]any{"name": "stale", "expected_updated_at": p.UpdatedAt}, false)
	if w.Code != 409 {
		t.Fatalf("stale editor must receive conflict: %d %s", w.Code, w.Body.String())
	}
	w = projectAPIRequest(a, "PATCH", "/api/projects/"+p.ID, map[string]any{"name": "renamed", "expected_updated_at": latest.UpdatedAt}, false)
	if w.Code != 200 {
		t.Fatalf("rename: %d %s", w.Code, w.Body.String())
	}
	got, err := a.Store.GetProject(p.ID)
	if err != nil || got.Name != "renamed" || !got.Archived || !reflect.DeepEqual(got.Workspaces, p.Workspaces) {
		t.Fatalf("PATCH touched unsubmitted fields: %+v %v", got, err)
	}
	if k := projectTestKnowledge(t, a.Store, p.ID); k != before {
		t.Fatal("metadata changed knowledge revision")
	}
}
