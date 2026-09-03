package importdir

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/store"
)

func TestDirectoryImport(t *testing.T) {
	dir := t.TempDir()
	obj := "o TestChair\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"
	if err := os.WriteFile(filepath.Join(dir, "test_chair.obj"), []byte(obj), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("ignore me"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	stats, err := Directory(context.Background(), st, cfg, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats.Added != 1 || stats.TotalFiles != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	res, err := st.Search(model.Query{Text: "chair"})
	if err != nil || len(res) != 1 {
		t.Fatalf("search: %v %+v", err, res)
	}
	if res[0].Category != "chair" {
		t.Fatalf("category=%q", res[0].Category)
	}
	// Re-import updates instead of duplicating.
	stats2, err := Directory(context.Background(), st, cfg, dir, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if stats2.Updated != 1 || stats2.Added != 0 {
		t.Fatalf("reimport stats=%+v", stats2)
	}
}
