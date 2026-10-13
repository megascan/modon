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

func TestVectorsRoundTripAndCascade(t *testing.T) {
	st := openTest(t)
	id, err := st.Upsert(model.Asset{PrimaryFile: "/tmp/v.obj", Title: "v"})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutVector(id, VecText, "m", "h1", []float32{0.6, 0.8}); err != nil {
		t.Fatal(err)
	}
	m, err := st.Vectors(VecText, "m", 2)
	if err != nil {
		t.Fatal(err)
	}
	if row := m.Row(id); len(row) != 2 || row[0] != 0.6 || row[1] != 0.8 {
		t.Fatalf("row = %v", row)
	}
	if other, _ := st.Vectors(VecText, "m", 4); len(other.IDs) != 0 {
		t.Fatal("vectors of another dimension must not load")
	}
	// The cache refreshes after writes.
	if err := st.PutVector(id, VecText, "m", "h2", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	m, _ = st.Vectors(VecText, "m", 2)
	if m.Row(id)[0] != 1 {
		t.Fatal("stale vector cache")
	}
	if h, _ := st.VectorHashes(VecText, "m", 2); h[id] != "h2" {
		t.Fatalf("hashes = %v", h)
	}
	if _, err := st.db.Exec(`DELETE FROM assets WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	if n := st.VectorCounts()[VecText]; n != 0 {
		t.Fatalf("vectors must cascade-delete with their asset, %d left", n)
	}
}

func TestSearchModeReportsStrictness(t *testing.T) {
	st := openTest(t)
	if _, err := st.Upsert(model.Asset{PrimaryFile: "/tmp/oak_tree.obj", Title: "oak tree"}); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ q, want string }{
		{"oak tree", MatchAll},
		{"tree", MatchAll},
		{"tree for a forest", MatchAny}, // only the loose OR fallback matches
		{"", MatchNone},
	} {
		_, mode, err := st.SearchMode(model.Query{Text: c.q})
		if err != nil || mode != c.want {
			t.Errorf("SearchMode(%q) = %q, %v; want %q", c.q, mode, err, c.want)
		}
	}
}

// A long-running MCP server must see vectors another process (modon embed)
// writes to the same index.
func TestVectorCacheSeesOtherProcesses(t *testing.T) {
	path := filepath.Join(t.TempDir(), "shared.db")
	server, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	cli, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer cli.Close()

	id, err := cli.Upsert(model.Asset{PrimaryFile: "/tmp/x.obj", Title: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if m, _ := server.Vectors(VecImage, "m", 2); len(m.IDs) != 0 {
		t.Fatal("expected an empty matrix first")
	}
	if err := cli.PutVector(id, VecImage, "m", "h", []float32{1, 0}); err != nil {
		t.Fatal(err)
	}
	if m, _ := server.Vectors(VecImage, "m", 2); len(m.IDs) != 1 {
		t.Fatalf("cache did not pick up another connection's write: %d rows", len(m.IDs))
	}
}
