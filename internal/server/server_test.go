package server

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/semantic"
	"modon/internal/source"
	"modon/internal/store"
)

func testDeps(t *testing.T) (*Deps, []int64) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	d := &Deps{Cfg: cfg, St: st, Reg: source.NewRegistry(cfg, st), Sem: semantic.New(cfg, st)}
	var ids []int64
	for _, p := range []string{"/lib/kit/FBX/Lamp_01.fbx", "/lib/kit/OBJ/Lamp_01.obj", "/lib/kit/FBX/Desk_01.fbx"} {
		id, err := st.Upsert(model.Asset{PrimaryFile: p, Title: filepath.Base(p[:len(p)-4]), Tags: []string{"fbx"}})
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if _, err := semantic.Dedupe(st, nil); err != nil {
		t.Fatal(err)
	}
	return d, ids
}

func TestBulkTaggingFlow(t *testing.T) {
	d, ids := testDeps(t)
	ctx := context.Background()
	res, out, _ := handleUnclassified(ctx, d, UnclassifiedInput{Worker: "agent-1", OnePerCluster: true})
	if res.IsError {
		t.Fatalf("list: %+v", res.Content)
	}
	page := out.(map[string]any)
	if page["count"] != 2 || page["worker"] != "agent-1" {
		t.Fatalf("page = %+v", page)
	}
	res, _, _ = handleUpdateMetadata(ctx, d, UpdateMetadataInput{
		ID: fmt.Sprintf("local:%d", ids[0]), Category: "lamp", Tags: []string{"desk lamp"}, RemoveTags: []string{"fbx"},
		ApplyToDuplicates: true,
	})
	if res.IsError {
		t.Fatalf("update: %+v", res.Content)
	}
	for _, id := range ids[:2] {
		a, _ := d.St.GetByID(id)
		if a.Category != "lamp" || len(a.Tags) != 1 || a.Tags[0] != "desk-lamp" || a.NeedsReview {
			t.Fatalf("local:%d after cluster update: %+v", id, a)
		}
	}
	if a, _ := d.St.GetByID(ids[2]); a.Category == "lamp" {
		t.Fatal("other assets must be untouched")
	}
	_, out, _ = handleUnclassified(ctx, d, UnclassifiedInput{Worker: "agent-1", Release: true})
	if out.(map[string]any)["released"] != 1 {
		t.Fatalf("release: %+v", out)
	}
	if res, _, _ := handleUpdateMetadata(ctx, d, UpdateMetadataInput{ID: "local:1"}); !res.IsError {
		t.Fatal("an empty update must be rejected")
	}
}
