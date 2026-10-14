package store

import (
	"testing"

	"modon/internal/model"
)

func TestSplitNameWordsAreSearchable(t *testing.T) {
	st := openTest(t)
	id, _ := st.Upsert(model.Asset{PrimaryFile: "/lib/POLYGON - Harbor Pack/FBX/SM_Prop_WallLantern_01.fbx", Title: "SM Prop WallLantern 01"})
	for _, q := range []string{"wall lantern", "harbor"} {
		res, err := st.Search(model.Query{Text: q})
		if err != nil || len(res) != 1 || res[0].RowID != id {
			t.Fatalf("search %q: %v %v", q, res, err)
		}
	}
	df, total, err := st.TokenDF([]string{"lantern", "nothing"})
	if err != nil || df["lantern"] != 1 || df["nothing"] != 0 || total != 1 {
		t.Fatalf("TokenDF = %v %d %v", df, total, err)
	}
	m, err := st.TokenMatches([]string{"lantern", "lan"}, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if m["lantern"][id] != (TokenMatch{Name: 2, Any: 2}) || m["lan"][id] != (TokenMatch{Name: 1, Any: 1}) {
		t.Fatalf("matches = %+v", m)
	}
}
