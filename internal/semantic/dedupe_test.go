package semantic

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modon/internal/model"
	"modon/internal/source"
)

func dupFixture(t *testing.T) (*fixture, map[string]int64) {
	f := setup(t)
	dir := t.TempDir()
	img := func(name, content string) string {
		p := filepath.Join(dir, name)
		os.WriteFile(p, []byte(content), 0o644)
		return p
	}
	ids := map[string]int64{}
	for key, a := range map[string]model.Asset{
		"harbor.fbx":  {PrimaryFile: "/lib/POLYGON - Harbor Pack/FBX/SM_Prop_Crate_01.fbx", Title: "SM Prop Crate 01", PreviewPath: img("h1.png", "crate-textured")},
		"harbor.obj":  {PrimaryFile: "/lib/POLYGON - Harbor Pack/OBJ/SM_Prop_Crate_01.obj", Title: "SM Prop Crate 01", PreviewPath: img("h2.png", "crate-grey")},
		"orchard.fbx": {PrimaryFile: "/lib/POLYGON - Orchard Pack/FBX/SM_Prop_Crate_01.fbx", Title: "SM Prop Crate 01", PreviewPath: img("f1.png", "crate-textured")},
		"canyon.fbx":  {PrimaryFile: "/lib/POLYGON - Canyon Pack/FBX/SM_Prop_Crate_01.fbx", Title: "SM Prop Crate 01", PreviewPath: img("c1.png", "canyon-crate")},
	} {
		id, err := f.st.Upsert(a)
		if err != nil {
			t.Fatal(err)
		}
		f.st.SetPreview(id, a.PreviewPath, "ok")
		ids[key] = id
	}
	if _, err := f.eng.Backfill(context.Background(), BackfillOptions{Text: true, Images: true}); err != nil {
		t.Fatal(err)
	}
	return f, ids
}

func TestDedupeClusters(t *testing.T) {
	f, ids := dupFixture(t)
	get := func(k string) model.Asset {
		a, err := f.st.GetByID(ids[k])
		if err != nil {
			t.Fatal(err)
		}
		return a
	}
	c := get("harbor.fbx").ClusterID
	if c == 0 || get("harbor.obj").ClusterID != c || get("orchard.fbx").ClusterID != c {
		t.Fatalf("same-pack exports and the shared prop must cluster: %d %d %d", c, get("harbor.obj").ClusterID, get("orchard.fbx").ClusterID)
	}
	if get("canyon.fbx").ClusterID != 0 {
		t.Fatal("a same-named but different-looking crate from another pack is not a duplicate")
	}
	if a, _ := f.st.GetByID(f.ids["rock big"]); a.ClusterID != 0 {
		t.Fatal("singletons have no cluster")
	}
}

func TestSearchCollapsesDuplicates(t *testing.T) {
	f, ids := dupFixture(t)
	local := source.NewLocal(f.st, f.eng.cfg)
	local.Searcher = f.eng
	ctx := context.Background()
	res, err := local.Search(ctx, model.Query{Text: "crate", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	crates := 0
	for _, a := range res {
		if strings.Contains(a.Title, "Crate") {
			crates++
			if a.RowID != ids["canyon.fbx"] && len(a.Duplicates) != 2 {
				t.Fatalf("collapsed result should list its 2 copies: %+v", a)
			}
		}
	}
	if crates != 2 {
		t.Fatalf("want 2 distinct crates (cluster + canyon), got %d: %+v", crates, res)
	}
	res, _ = local.Search(ctx, model.Query{Text: "crate", Limit: 10, ExpandDuplicates: true})
	crates = 0
	for _, a := range res {
		if strings.Contains(a.Title, "Crate") {
			crates++
		}
	}
	if crates != 4 {
		t.Fatalf("expand_duplicates should return every copy, got %d", crates)
	}
}
