package semantic

import (
	"fmt"
	"slices"
	"testing"

	"modon/internal/model"
)

func TestLexicalFavorsRareTerms(t *testing.T) {
	f := setup(t)
	for i := 0; i < 40; i++ {
		f.st.Upsert(model.Asset{PrimaryFile: fmt.Sprintf("/lib/kit/Prop_Barrel_%02d.fbx", i), Title: fmt.Sprintf("Prop Barrel %02d", i)})
	}
	rad, _ := f.st.Upsert(model.Asset{PrimaryFile: "/lib/kit/Prop_Radioactive_01.fbx", Title: "Prop Radioactive 01"})
	both, _ := f.st.Upsert(model.Asset{PrimaryFile: "/lib/kit/Prop_RadioactiveBarrel_01.fbx", Title: "Prop RadioactiveBarrel 01"})
	hits, err := f.eng.lexical(model.Query{Text: "a radioactive barrel"})
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) < 3 || hits[0].id != both || hits[1].id != rad {
		t.Fatalf("lexical order: %+v (want %d then %d)", hits[:3], both, rad)
	}
	if hits[0].score < 1 || hits[1].score <= hits[2].score {
		t.Fatalf("coverage: %+v", hits[:3])
	}
	cov := map[int64]float64{rad: 0.5, both: hits[0].score}
	b := lexicalBonus(cov, []signal{{"keyword", 1, []int64{both, rad}}, {"text", 1, []int64{both}}})
	if b[both] != lexBoost {
		t.Fatalf("full coverage earns the full boost: %v", b[both])
	}
	alone := lexicalBonus(cov, []signal{{"keyword", 1, []int64{both, rad}}})
	withVec := lexicalBonus(cov, []signal{{"text", 1, []int64{rad}}})
	if !(alone[rad] < withVec[rad]) || alone[both] != lexBoost {
		t.Fatalf("keyword-only partial matches are damped: alone=%v withVec=%v", alone, withVec)
	}
}

func TestQueryTokensDropStopwords(t *testing.T) {
	if got := queryTokens("Heavy machine gun on a tripod, heavy"); !slices.Equal(got, []string{"heavy", "machine", "gun", "tripod"}) {
		t.Fatalf("tokens = %v", got)
	}
}

func TestDocTextSkipsHeuristicMetadata(t *testing.T) {
	a := model.Asset{Title: "SM Prop Barrel 01", Category: "prop", Tags: []string{"lowpoly", "fbx"}, PrimaryFile: "/lib/POLYGON - Orchard Pack/FBX/SM_Prop_Barrel_01.fbx"}
	if _, body := DocText(a); body != "Prop Barrel" {
		t.Fatalf("heuristic metadata must stay out of the vector document: %q", body)
	}
	a.Provenance = map[string]string{"category": "agent", "tags": "agent"}
	a.Category, a.Tags = "container", []string{"oil-drum", "fbx", "barrel"}
	if _, body := DocText(a); body != "Prop Barrel; container, oil drum" {
		t.Fatalf("agent metadata: %q", body)
	}
}
