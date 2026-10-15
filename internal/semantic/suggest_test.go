package semantic

import (
	"context"
	"fmt"
	"testing"

	"modon/internal/store"
)

func TestSuggestManyUsesTaggedLookAlikes(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true}); err != nil {
		t.Fatal(err)
	}
	// An agent tagged one export; its copy (same preview) still needs review.
	if _, err := f.st.UpdateMetadata(f.ids["SM Prop 02"], store.MetadataUpdate{
		Category: "vehicle", Tags: []string{"sports-car", "red", "fbx"}, TagsMode: store.TagsReplace,
	}); err != nil {
		t.Fatal(err)
	}
	copyID := f.ids["SM Prop 02 copy"]
	sgs, err := f.eng.SuggestMany(ctx, []int64{copyID, 9999}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(sgs) != 2 || sgs[1].Error == "" {
		t.Fatalf("an unknown id gets an error entry, not a failed batch: %+v", sgs)
	}
	sg := sgs[0]
	if len(sg.Categories) != 3 || len(sg.Styles) != 3 || len(sg.Tags) == 0 || sg.Tags[0].Facet == "" {
		t.Fatalf("zero-shot lists: %+v", sg)
	}
	if len(sg.Neighbors) == 0 || sg.Neighbors[0].ID != fmt.Sprintf("local:%d", f.ids["SM Prop 02"]) {
		t.Fatalf("the tagged copy should be the closest neighbour: %+v", sg.Neighbors)
	}
	got := map[string]bool{}
	for _, s := range sg.NeighborTags {
		got[s.Label] = true
		if s.Count < 1 || s.Score <= 0 || s.Score > 1 {
			t.Fatalf("neighbour tag %+v", s)
		}
	}
	if !got["sports-car"] || !got["red"] || got["fbx"] {
		t.Fatalf("neighbour tags = %+v", sg.NeighborTags)
	}
	if len(sg.NeighborCategories) == 0 || sg.NeighborCategories[0].Label != "vehicle" {
		t.Fatalf("neighbour categories = %+v", sg.NeighborCategories)
	}
	one, err := f.eng.Suggest(ctx, copyID, 1)
	if err != nil || len(one.Categories) != 1 {
		t.Fatalf("Suggest k=1: %v %+v", err, one)
	}
}
