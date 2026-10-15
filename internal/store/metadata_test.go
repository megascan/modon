package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"modon/internal/model"
)

func ptr[T any](v T) *T { return &v }

// Agent-written fields win over heuristics on re-import; heuristic fields
// refresh; agent tag lists (including removals) are kept verbatim.
func TestProvenanceSurvivesReimport(t *testing.T) {
	st := openTest(t)
	imp := model.Asset{PrimaryFile: "/lib/kit/FBX/Crate_01.fbx", Title: "Crate 01", Category: "prop", Style: "lowpoly",
		Tags: []string{"lowpoly", "prop", "fbx"}}
	id, err := st.Upsert(imp)
	if err != nil {
		t.Fatal(err)
	}
	a, err := st.UpdateMetadata(id, MetadataUpdate{Category: "container", Tags: []string{"wooden crate"}, RemoveTags: []string{"FBX"}})
	if err != nil {
		t.Fatal(err)
	}
	if a.Provenance["category"] != model.ProvenanceAgent || a.Provenance["tags"] != model.ProvenanceAgent {
		t.Fatalf("provenance = %v", a.Provenance)
	}
	if want := []string{"lowpoly", "prop", "wooden-crate"}; !slices.Equal(a.Tags, want) {
		t.Fatalf("tags = %v, want %v", a.Tags, want)
	}
	if a.NeedsReview {
		t.Fatal("agent category + tags should clear needs_review")
	}

	// Re-import with a changed heuristic style and different heuristic tags.
	imp.Style, imp.Category, imp.Tags = "stylized", "furniture", []string{"stylized", "fbx", "new"}
	if _, err := st.Upsert(imp); err != nil {
		t.Fatal(err)
	}
	a, _ = st.GetByID(id)
	if a.Category != "container" {
		t.Fatalf("agent category overwritten: %q", a.Category)
	}
	if a.Style != "stylized" {
		t.Fatalf("heuristic style should refresh, got %q", a.Style)
	}
	if want := []string{"lowpoly", "prop", "wooden-crate"}; !slices.Equal(a.Tags, want) {
		t.Fatalf("agent tags must survive re-import verbatim (removed fbx stays removed): %v", a.Tags)
	}
	if a.NeedsReview {
		t.Fatal("re-import must not re-flag a reviewed asset")
	}
}

func TestHeuristicFieldsRefreshAndAutoIsKept(t *testing.T) {
	st := openTest(t)
	imp := model.Asset{PrimaryFile: "/lib/x/Thing_01.obj", Title: "Thing 01", Tags: []string{"a"}}
	id, _ := st.Upsert(imp)
	if err := st.SetClassification(id, "rock", ""); err != nil {
		t.Fatal(err)
	}
	a, _ := st.GetByID(id)
	if a.Category != "rock" || a.Provenance["category"] != model.ProvenanceAuto {
		t.Fatalf("auto classification: %q %v", a.Category, a.Provenance)
	}
	imp.Tags = []string{"b"}
	st.Upsert(imp) // heuristic has no category: the zero-shot one stays
	a, _ = st.GetByID(id)
	if a.Category != "rock" || !slices.Equal(a.Tags, []string{"b"}) {
		t.Fatalf("after re-import: category %q tags %v", a.Category, a.Tags)
	}
	imp.Category = "boulder"
	st.Upsert(imp) // a heuristic category replaces the zero-shot guess
	if a, _ = st.GetByID(id); a.Category != "boulder" || a.Provenance["category"] != "" {
		t.Fatalf("heuristic should replace auto: %q %v", a.Category, a.Provenance)
	}
}

func TestTagModes(t *testing.T) {
	st := openTest(t)
	id, _ := st.Upsert(model.Asset{PrimaryFile: "/lib/t.obj", Title: "t", Tags: []string{"one", "two"}})
	a, err := st.UpdateMetadata(id, MetadataUpdate{Tags: []string{"three", "One"}})
	if err != nil || !slices.Equal(a.Tags, []string{"one", "two", "three"}) {
		t.Fatalf("merge: %v %v", a.Tags, err)
	}
	a, _ = st.UpdateMetadata(id, MetadataUpdate{Tags: []string{"x", "y"}, TagsMode: TagsReplace, RemoveTags: []string{"Y"}})
	if !slices.Equal(a.Tags, []string{"x"}) {
		t.Fatalf("replace + remove: %v", a.Tags)
	}
	a, _ = st.UpdateMetadata(id, MetadataUpdate{TagsMode: TagsReplace})
	if len(a.Tags) != 0 {
		t.Fatalf("replace with no tags clears: %v", a.Tags)
	}
	if _, err := st.UpdateMetadata(id, MetadataUpdate{Tags: []string{"z"}, TagsMode: "append"}); err == nil {
		t.Fatal("unknown tags_mode must be rejected")
	}
	a, _ = st.UpdateMetadata(id, MetadataUpdate{Reviewed: ptr(false)})
	if !a.NeedsReview {
		t.Fatal("reviewed=false must re-flag")
	}
}

func TestNeedsReviewNeedsCategoryAndTags(t *testing.T) {
	st := openTest(t)
	id, _ := st.Upsert(model.Asset{PrimaryFile: "/lib/r.obj", Title: "r", Category: "heuristic", Tags: []string{"h"}})
	a, _ := st.UpdateMetadata(id, MetadataUpdate{Category: "chair"})
	if !a.NeedsReview {
		t.Fatal("a category alone (with heuristic tags) is not a review")
	}
	a, _ = st.UpdateMetadata(id, MetadataUpdate{Tags: []string{"wooden"}})
	if a.NeedsReview {
		t.Fatal("agent category + agent tags clear needs_review")
	}
}

func seedReview(t *testing.T, st *Store, n int) []int64 {
	t.Helper()
	var ids []int64
	for i := 0; i < n; i++ {
		id, err := st.Upsert(model.Asset{PrimaryFile: fmt.Sprintf("/lib/a%02d.obj", i), Title: fmt.Sprintf("a%02d", i), FileSize: int64(1000 - i)})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	return ids
}

func pageIDs(p *UnclassifiedPage) []int64 {
	var out []int64
	for _, a := range p.Assets {
		out = append(out, a.RowID)
	}
	return out
}

func TestClaimsGiveDisjointBatches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claims.db")
	st1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st1.Close()
	st2, err := Open(path) // a second process
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	ids := seedReview(t, st1, 10)

	p1, err := st1.ListUnclassified(UnclassifiedOptions{Limit: 4, Worker: "w1"})
	if err != nil {
		t.Fatal(err)
	}
	p2, err := st2.ListUnclassified(UnclassifiedOptions{Limit: 4, Worker: "w2"})
	if err != nil {
		t.Fatal(err)
	}
	a, b := pageIDs(p1), pageIDs(p2)
	if len(a) != 4 || len(b) != 4 {
		t.Fatalf("pages: %v %v", a, b)
	}
	for _, id := range a {
		if slices.Contains(b, id) {
			t.Fatalf("workers got overlapping batches: %v %v", a, b)
		}
	}
	if p2.Remaining != 2 || p2.LeaseExpiresAt == "" {
		t.Fatalf("remaining = %d lease = %q", p2.Remaining, p2.LeaseExpiresAt)
	}
	// Unclaimed listing (and a worker's next call) skip claimed rows.
	if p, _ := st1.ListUnclassified(UnclassifiedOptions{Limit: 10}); len(p.Assets) != 2 {
		t.Fatalf("plain listing should skip claimed rows: %v", pageIDs(p))
	}
	// Updating releases the claim; the asset leaves the queue.
	if _, err := st1.UpdateMetadata(a[0], MetadataUpdate{Description: "done"}); err != nil {
		t.Fatal(err)
	}
	if n := st1.ActiveClaims()["w1"]; n != 3 {
		t.Fatalf("w1 claims after one update = %d", n)
	}
	// Releasing returns the rest to the pool.
	if n, _ := st1.ReleaseClaims("w1"); n != 3 {
		t.Fatalf("released %d", n)
	}
	if p, _ := st1.ListUnclassified(UnclassifiedOptions{Limit: 10}); len(p.Assets) != 5 {
		t.Fatalf("after release: %v", pageIDs(p))
	}
	// Expired leases free their rows.
	if _, err := st1.db.Exec(`UPDATE claims SET expires_at = ?`, time.Now().Add(-time.Second).Unix()); err != nil {
		t.Fatal(err)
	}
	if p, _ := st1.ListUnclassified(UnclassifiedOptions{Limit: 10}); len(p.Assets) != 9 {
		t.Fatalf("after expiry: %d rows", len(p.Assets))
	}
	_ = ids
}

func TestUnclassifiedOffsetAndOnePerCluster(t *testing.T) {
	st := openTest(t)
	ids := seedReview(t, st, 6)
	p, _ := st.ListUnclassified(UnclassifiedOptions{Limit: 2, Offset: 2})
	if got := pageIDs(p); !slices.Equal(got, ids[2:4]) {
		t.Fatalf("offset page = %v, want %v", got, ids[2:4])
	}
	// ids[0], ids[1], ids[2] are copies of one model.
	if _, err := st.SetClusters(map[int64]int64{ids[0]: ids[0], ids[1]: ids[0], ids[2]: ids[0]}); err != nil {
		t.Fatal(err)
	}
	p, _ = st.ListUnclassified(UnclassifiedOptions{Limit: 10, OnePerCluster: true, Worker: "w"})
	got := pageIDs(p)
	if !slices.Equal(got, []int64{ids[0], ids[3], ids[4], ids[5]}) {
		t.Fatalf("one per cluster = %v", got)
	}
	if len(p.Assets[0].Duplicates) != 2 {
		t.Fatalf("representative should list its copies: %v", p.Assets[0].Duplicates)
	}
	if n := st.ActiveClaims()["w"]; n != 6 {
		t.Fatalf("claiming a representative claims its cluster: %d claims", n)
	}
}

func TestCollapseDuplicates(t *testing.T) {
	st := openTest(t)
	ids := seedReview(t, st, 4)
	st.SetClusters(map[int64]int64{ids[1]: ids[1], ids[3]: ids[1]})
	all, _ := st.GetByIDs([]int64{ids[3], ids[0], ids[1], ids[2]})
	out, err := st.CollapseDuplicates(all, 10)
	if err != nil {
		t.Fatal(err)
	}
	if got := []int64{out[0].RowID, out[1].RowID, out[2].RowID}; len(out) != 3 || !slices.Equal(got, []int64{ids[3], ids[0], ids[2]}) {
		t.Fatalf("collapsed = %v", out)
	}
	if !slices.Equal(out[0].Duplicates, []string{fmt.Sprintf("local:%d", ids[1])}) || out[0].ClusterID != ids[1] {
		t.Fatalf("best-ranked copy keeps the others: %+v", out[0])
	}
	if out, _ := st.CollapseDuplicates(all, 2); len(out) != 2 {
		t.Fatalf("limit: %d", len(out))
	}
}

func TestVectorHashesFor(t *testing.T) {
	st := openTest(t)
	ids := seedReview(t, st, 3)
	for i, id := range ids {
		st.PutVector(id, VecText, "m", fmt.Sprint("h", i), []float32{1, 0})
	}
	h, err := st.VectorHashesFor(VecText, "m", 2, []int64{ids[1], 999})
	if err != nil || len(h) != 1 || h[ids[1]] != "h1" {
		t.Fatalf("hashes = %v %v", h, err)
	}
}

// An index written by the previous schema (v1) migrates in place:
// provenance columns, full-text index with split names, kept descriptions.
func TestMigrateFromV1(t *testing.T) {
	path := filepath.Join(t.TempDir(), "v1.db")
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`CREATE TABLE assets (id INTEGER PRIMARY KEY AUTOINCREMENT, path TEXT NOT NULL UNIQUE, title TEXT NOT NULL,
			name TEXT DEFAULT '', description TEXT DEFAULT '', asset_type TEXT DEFAULT 'model', category TEXT DEFAULT '',
			style TEXT DEFAULT '', tags TEXT DEFAULT '', animated INTEGER DEFAULT 0, animation_names TEXT DEFAULT '',
			tri_count INTEGER DEFAULT 0, vert_count INTEGER DEFAULT 0, formats TEXT DEFAULT '', related_files TEXT DEFAULT '',
			file_size INTEGER DEFAULT 0, texture_count INTEGER DEFAULT 0, material_names TEXT DEFAULT '', license TEXT DEFAULT '',
			creator TEXT DEFAULT '', preview_path TEXT DEFAULT '', preview_status TEXT DEFAULT 'none',
			needs_review INTEGER DEFAULT 1, imported_at TEXT DEFAULT '', updated_at TEXT DEFAULT '')`,
		`CREATE VIRTUAL TABLE assets_fts USING fts5(title, name, description, category, style, tags, creator,
			content='assets', content_rowid='id', tokenize='porter unicode61')`,
		`CREATE TRIGGER assets_fts_ai AFTER INSERT ON assets BEGIN
			INSERT INTO assets_fts(rowid,title,name,description,category,style,tags,creator)
			VALUES (new.id,new.title,new.name,new.description,new.category,new.style,new.tags,new.creator); END`,
		`INSERT INTO assets (path, title, name, description, category, tags, needs_review)
			VALUES ('/lib/POLYGON - Dock Pack/SM_Prop_FishCrate_01.fbx', 'SM Prop FishCrate 01', 'SM Prop FishCrate 01', 'a crate of fish', 'container', 'lowpoly fbx', 0)`,
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	db.Close()

	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if st.version != schemaVersion {
		t.Fatalf("user_version = %d", st.version)
	}
	res, err := st.Search(model.Query{Text: "fish crate dock"})
	if err != nil || len(res) != 1 {
		t.Fatalf("migrated FTS: %v %v", res, err)
	}
	a := res[0]
	if a.Provenance["description"] != model.ProvenanceAgent || a.Provenance["category"] != "" || a.NeedsReview {
		t.Fatalf("migrated provenance: %+v", a)
	}
	// Re-opening is a no-op.
	st.Close()
	if st, err = Open(path); err != nil {
		t.Fatal(err)
	}
	if res, _ := st.Search(model.Query{Text: "crate"}); len(res) != 1 {
		t.Fatalf("after reopen: %v", res)
	}
}

func TestSetHeuristicsRespectsProvenance(t *testing.T) {
	st := openTest(t)
	id, err := st.Upsert(model.Asset{PrimaryFile: "/lib/SK_Chr_Hair_01.fbx", Title: "SK Chr Hair 01",
		Category: "character", Style: "lowpoly", Tags: []string{"character", "fbx"}, NeedsReview: true})
	if err != nil {
		t.Fatal(err)
	}
	h := Heuristics{Category: "character-part", Style: "lowpoly", AssetType: "model", Tags: []string{"character-part", "character", "fbx"}}
	if changed, err := st.SetHeuristics(id, h); err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	a, _ := st.GetByID(id)
	if a.Category != "character-part" || len(a.Tags) != 3 || !a.NeedsReview {
		t.Fatalf("heuristic refresh: %+v", a)
	}
	if changed, _ := st.SetHeuristics(id, h); changed {
		t.Fatal("an identical refresh must report no change")
	}

	// Agent fields are never overwritten; an empty heuristic keeps the value.
	if _, err := st.UpdateMetadata(id, MetadataUpdate{Category: "hair", Tags: []string{"ponytail"}, TagsMode: TagsReplace}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetHeuristics(id, Heuristics{Category: "character", Style: "", AssetType: "model", Tags: []string{"x"}}); err != nil {
		t.Fatal(err)
	}
	a, _ = st.GetByID(id)
	if a.Category != "hair" || a.Style != "lowpoly" || len(a.Tags) != 1 || a.Tags[0] != "ponytail" {
		t.Fatalf("agent values must survive: %+v", a)
	}

	// A zero-shot value is replaced by a real heuristic value only.
	id2, _ := st.Upsert(model.Asset{PrimaryFile: "/lib/blob.fbx", Title: "blob"})
	if err := st.SetClassification(id2, "rock", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetHeuristics(id2, Heuristics{AssetType: "model"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetByID(id2); a.Category != "rock" || a.Provenance["category"] != model.ProvenanceAuto {
		t.Fatalf("auto value without a heuristic replacement: %+v", a)
	}
	if _, err := st.SetHeuristics(id2, Heuristics{Category: "creature", AssetType: "model"}); err != nil {
		t.Fatal(err)
	}
	if a, _ := st.GetByID(id2); a.Category != "creature" || a.Provenance["category"] != "" {
		t.Fatalf("auto value replaced by a heuristic: %+v", a)
	}
}
