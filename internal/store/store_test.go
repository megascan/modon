package store

import (
	"path/filepath"
	"testing"

	"modon/internal/model"
)

func openTest(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestUpsertAndSearch(t *testing.T) {
	st := openTest(t)
	id1, err := st.Upsert(model.Asset{
		PrimaryFile: "/tmp/wooden_chair.obj",
		Title:       "wooden chair",
		Name:        "wooden chair",
		AssetType:   "model",
		Category:    "chair",
		Style:       "lowpoly",
		Tags:        []string{"lowpoly", "chair", "wood"},
		Description: "a low-poly wooden chair",
	})
	if err != nil || id1 == 0 {
		t.Fatalf("upsert: %v id=%d", err, id1)
	}
	id2, err := st.Upsert(model.Asset{
		PrimaryFile: "/tmp/red_car.fbx",
		Title:       "red car",
		Name:        "red car",
		AssetType:   "model",
		Category:    "vehicle",
		Description: "a red sports car",
	})
	if err != nil || id2 == 0 {
		t.Fatalf("upsert2: %v", err)
	}
	res, err := st.Search(model.Query{Text: "chair", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].Title != "wooden chair" {
		t.Fatalf("expected chair first, got %+v", res)
	}
	res, err = st.Search(model.Query{Text: "car"})
	if err != nil || len(res) == 0 {
		t.Fatalf("expected car, got %v %+v", err, res)
	}
	// Facet filter.
	res, err = st.Search(model.Query{Category: "vehicle"})
	if err != nil || len(res) != 1 || res[0].Title != "red car" {
		t.Fatalf("category filter: %v %+v", err, res)
	}
}

func TestUpdateMetadataClearsReview(t *testing.T) {
	st := openTest(t)
	id, err := st.Upsert(model.Asset{PrimaryFile: "/tmp/x.obj", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	un, err := st.ListUnclassified(10)
	if err != nil || len(un) != 1 {
		t.Fatalf("expected 1 unclassified, got %v %+v", err, un)
	}
	updated, err := st.UpdateMetadata(id, "a rich description", "chair", "lowpoly", "model", []string{"wood"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.NeedsReview {
		t.Fatal("needs_review should be false after description set")
	}
	if updated.Category != "chair" || updated.Style != "lowpoly" {
		t.Fatalf("metadata not stored: %+v", updated)
	}
	un, _ = st.ListUnclassified(10)
	if len(un) != 0 {
		t.Fatalf("expected 0 unclassified, got %d", len(un))
	}
}
