package importdir

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"modon/internal/model"
	"modon/internal/store"
)

func TestReclassifyKeepsAgentEdits(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "lowpoly-pack - Space Kit", "FBX")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	obj := "o Mesh\nv 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ids := map[string]int64{}
	for _, name := range []string{"SM_Ship_Station_01", "SM_Ship_Station_02", "SK_Chr_Hair_01"} {
		p := filepath.Join(dir, name+".obj")
		if err := os.WriteFile(p, []byte(obj), 0o644); err != nil {
			t.Fatal(err)
		}
		// Seed the index with what an older importer guessed.
		id, err := st.Upsert(model.Asset{PrimaryFile: p, Title: name, Category: "watercraft", Tags: []string{"watercraft"}, NeedsReview: true})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
	}
	if _, err := st.UpdateMetadata(ids["SM_Ship_Station_02"], store.MetadataUpdate{Category: "watercraft"}); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	dry, err := Reclassify(ctx, st, ReclassifyOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if dry.CategoryMoves != 2 || dry.AgentProtected != 1 || dry.Changes["watercraft -> spacecraft"] != 1 ||
		dry.Changes["watercraft -> character-part"] != 1 {
		t.Fatalf("dry run: %+v", dry)
	}
	if a, _ := st.GetByID(ids["SM_Ship_Station_01"]); a.Category != "watercraft" {
		t.Fatal("a dry run must not write")
	}
	if _, err := Reclassify(ctx, st, ReclassifyOptions{Apply: true}); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]string{"SM_Ship_Station_01": "spacecraft", "SM_Ship_Station_02": "watercraft", "SK_Chr_Hair_01": "character-part"} {
		a, _ := st.GetByID(ids[name])
		if a.Category != want || !a.NeedsReview {
			t.Errorf("%s: %q needs_review=%v, want %q", name, a.Category, a.NeedsReview, want)
		}
	}
	again, _ := Reclassify(ctx, st, ReclassifyOptions{})
	if again.Changed != 0 {
		t.Fatalf("a second run should find nothing to change: %+v", again)
	}
}

func TestClassifyDirs(t *testing.T) {
	home, _ := os.UserHomeDir()
	got := ClassifyDirs(filepath.Join(home, "lib", "Vendor - Harbor Pack", "Source", "FBX", "Crate_01.fbx"))
	if len(got) != 3 || got[0] != "Vendor - Harbor Pack" {
		t.Fatalf("pack folder down: %v", got)
	}
	got = ClassifyDirs(filepath.Join(home, "a.fbx"))
	if len(got) != 0 {
		t.Fatalf("the home directory is never a hint: %v", got)
	}
	got = ClassifyDirs("/srv/x/y/props/crates/Crate_01.fbx")
	if len(got) != 3 || got[2] != "crates" {
		t.Fatalf("last three folders: %v", got)
	}
}
