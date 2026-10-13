package semantic

import (
	"context"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"errors"
	"hash/fnv"
	"math"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/store"
)

const testDims = 256

// synonyms make the fake embedder "understand" a few paraphrases that
// keyword search cannot match.
var synonyms = map[string]string{"couch": "sofa", "settee": "sofa", "automobile": "car", "boulder": "rock"}

// generic words carry no meaning for the fake (as for the real model, where
// the neutral queries embed near the corpus centre rather than overlapping
// documents token by token).
var generic = map[string]bool{"a": true, "an": true, "3d": true, "model": true, "object": true, "thing": true, "asset": true}

// fakeVec is a bag-of-words embedding: text after the task prefix is
// tokenized, synonyms folded, each token hashed into a dimension.
func fakeVec(text string) []float32 {
	if i := strings.LastIndex(text, "query: "); i >= 0 {
		text = text[i+len("query: "):]
	} else if i := strings.Index(text, "text: "); i >= 0 {
		text = strings.TrimPrefix(text[:i], "title: ") + " " + text[i+len("text: "):]
	}
	v := make([]float32, testDims)
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9')
	}) {
		if s, ok := synonyms[tok]; ok {
			tok = s
		}
		if generic[tok] {
			continue
		}
		h := fnv.New32a()
		h.Write([]byte(tok))
		v[h.Sum32()%testDims] += 1
	}
	return norm(v)
}

// fakeImageVec hashes image bytes: identical pictures embed identically.
func fakeImageVec(data []byte) []float32 {
	sum := sha1.Sum(data)
	v := make([]float32, testDims)
	for i, b := range sum {
		v[i%testDims] += float32(b)
	}
	return norm(v)
}

func norm(v []float32) []float32 {
	var n float64
	for _, x := range v {
		n += float64(x * x)
	}
	if n == 0 {
		v[0] = 1
		return v
	}
	for i := range v {
		v[i] /= float32(math.Sqrt(n))
	}
	return v
}

func fakeServer(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/modon/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "vision": true, "dims": testDims})
		case "/v1/embeddings":
			var req struct {
				Input []any `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			*calls += len(req.Input)
			var data []map[string]any
			for i, in := range req.Input {
				var vec []float32
				switch x := in.(type) {
				case string:
					vec = fakeVec(x)
				case map[string]any:
					ref, _ := x["image"].(string)
					raw, _ := base64.StdEncoding.DecodeString(ref[strings.Index(ref, ",")+1:])
					vec = fakeImageVec(raw)
				}
				data = append(data, map[string]any{"index": i, "embedding": vec})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			http.NotFound(w, r)
		}
	}))
}

type fixture struct {
	eng   *Engine
	st    *store.Store
	ids   map[string]int64
	calls *int
}

func setup(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	calls := new(int)
	srv := fakeServer(t, calls)
	t.Cleanup(srv.Close)

	cfg := config.Defaults()
	cfg.Embedding.Endpoint = srv.URL
	cfg.Embedding.Dims = testDims
	cfg.Embedding.TimeoutSeconds = 5

	img := func(name string, content string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	// Fixed (non-temp) asset paths: folder names are part of the embedded
	// document, and random temp-dir digits would perturb the fake vectors.
	ids := map[string]int64{}
	for _, a := range []model.Asset{
		{PrimaryFile: "/assets/props/SM_Prop_01.fbx", Title: "SM Prop 01", Description: "a green leather sofa", Category: "furniture", PreviewPath: img("p1.png", "sofa-pixels")},
		{PrimaryFile: "/assets/props/SM_Prop_02.fbx", Title: "SM Prop 02", Description: "a red sports car", Category: "vehicle", PreviewPath: img("p2.png", "car-pixels")},
		{PrimaryFile: "/assets/props/SM_Prop_02_copy.glb", Title: "SM Prop 02 copy", Description: "duplicate export", Category: "vehicle", PreviewPath: img("p3.png", "car-pixels")},
		{PrimaryFile: "/assets/nature/rock_big.obj", Title: "rock big", Description: "mossy rock", Category: "rock"},
	} {
		id, err := st.Upsert(a)
		if err != nil {
			t.Fatal(err)
		}
		if a.PreviewPath != "" {
			_ = st.SetPreview(id, a.PreviewPath, "ok")
		}
		ids[a.Title] = id
	}
	cfg.Embedding.Model = "fake"
	return &fixture{eng: New(cfg, st), st: st, ids: ids, calls: calls}
}

func TestBackfillIsIncremental(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	s, err := f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Text != 4 || s.Images != 3 || s.NoPreview != 1 {
		t.Fatalf("first run: %+v", s)
	}
	s, err = f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Text != 0 || s.Images != 0 {
		t.Fatalf("second run should be a no-op: %+v", s)
	}
	// Editing metadata re-embeds just that asset's text.
	if _, err := f.st.UpdateMetadata(f.ids["rock big"], "a huge granite boulder", "", "", "", nil); err != nil {
		t.Fatal(err)
	}
	s, err = f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true})
	if err != nil {
		t.Fatal(err)
	}
	if s.Text != 1 || s.Images != 0 {
		t.Fatalf("after edit: %+v", s)
	}
}

func TestHybridSearchFindsParaphrases(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true}); err != nil {
		t.Fatal(err)
	}
	// "couch" appears nowhere in the index; keyword search alone finds nothing.
	if kw, _ := f.st.Search(model.Query{Text: "couch"}); len(kw) != 0 {
		t.Fatalf("keyword search unexpectedly matched: %v", kw)
	}
	res, err := f.eng.Search(ctx, model.Query{Text: "couch"})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].RowID != f.ids["SM Prop 01"] {
		t.Fatalf("semantic search for couch: %+v (lastErr=%q)", res, f.eng.lastErr)
	}
	if res[0].Score <= 0 || len(res[0].MatchedBy) == 0 || res[0].MatchedBy[0] != "text" {
		t.Fatalf("missing diagnostics: score=%v matched_by=%v", res[0].Score, res[0].MatchedBy)
	}

	// Facet filters still apply to vector hits.
	res, err = f.eng.Search(ctx, model.Query{Text: "couch", Category: "vehicle"})
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range res {
		if a.Category != "vehicle" {
			t.Fatalf("filter leaked %q", a.Category)
		}
	}
}

func TestSimilarFindsDuplicatesAndQueryByImage(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true}); err != nil {
		t.Fatal(err)
	}
	ref, err := f.st.GetByID(f.ids["SM Prop 02"])
	if err != nil {
		t.Fatal(err)
	}
	res, err := f.eng.Similar(ctx, Ref{Asset: ref}, model.Query{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].RowID != f.ids["SM Prop 02 copy"] {
		t.Fatalf("duplicate should rank first: %+v", res)
	}
	if res[0].Similarity < 0.999 {
		t.Fatalf("identical images should have similarity ~1, got %v", res[0].Similarity)
	}
	for _, a := range res {
		if a.RowID == ref.RowID {
			t.Fatal("reference must not match itself")
		}
	}

	// A remote reference with preview bytes is compared visually.
	res, err = f.eng.Similar(ctx, Ref{Asset: model.Asset{Title: "Sports car", Source: "fab"}, Image: []byte("car-pixels"), ImageMIME: "image/png"}, model.Query{Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || (res[0].RowID != f.ids["SM Prop 02"] && res[0].RowID != f.ids["SM Prop 02 copy"]) {
		t.Fatalf("remote reference: %+v", res)
	}

	// Query by image.
	p := filepath.Join(t.TempDir(), "q.png")
	_ = os.WriteFile(p, []byte("sofa-pixels"), 0o644)
	res, err = f.eng.Search(ctx, model.Query{Image: p})
	if err != nil {
		t.Fatal(err)
	}
	if len(res) == 0 || res[0].RowID != f.ids["SM Prop 01"] {
		t.Fatalf("image query: %+v", res)
	}
}

func TestDisabledEngineFallsBackToKeywords(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.Upsert(model.Asset{PrimaryFile: "/tmp/chair.obj", Title: "chair"}); err != nil {
		t.Fatal(err)
	}
	e := New(config.Defaults(), st)
	if e.Enabled() {
		t.Fatal("engine must be off without an endpoint")
	}
	res, err := e.Search(context.Background(), model.Query{Text: "chair"})
	if err != nil || len(res) != 1 {
		t.Fatalf("fallback search: %v %v", res, err)
	}
	if _, err := e.Search(context.Background(), model.Query{Image: "/tmp/x.png"}); err == nil {
		t.Fatal("image-only search without embeddings should explain itself")
	}
}

func TestDocText(t *testing.T) {
	for _, c := range []struct {
		a    model.Asset
		want string
	}{
		{model.Asset{Title: "SF Veh BatteringRam 01", PrimaryFile: "/a/SIMPLE Fantasy/SourceFiles/Fbx/SF_Veh_BatteringRam_01.fbx"}, "vehicle Battering Ram"},
		{model.Asset{Title: "SM Env Tree 01", Description: "a gnarled oak"}, "environment Tree; a gnarled oak"},
		{model.Asset{Title: "mesh 004", PrimaryFile: "/lib/Furniture/Chairs/FBX/mesh_004.fbx"}, "mesh; Furniture / Chairs"},
		{model.Asset{Title: "TV Stand"}, "TV Stand"},
	} {
		title, body := DocText(c.a)
		if title != "" || body != c.want {
			t.Errorf("DocText(%q) = %q, %q; want body %q", c.a.Title, title, body, c.want)
		}
	}
}

func TestRankZFloor(t *testing.T) {
	// 300 unrelated rows plus one close to the query: only the outlier
	// clears the cut, and filters do not move it.
	m := &store.Matrix{Dims: testDims}
	rng := rand.New(rand.NewPCG(1, 2))
	for i := 0; i < 300; i++ {
		v := make([]float32, testDims)
		for j := range v {
			v[j] = float32(rng.NormFloat64())
		}
		m.IDs = append(m.IDs, int64(i+1))
		m.Vecs = append(m.Vecs, norm(v)...)
	}
	m.IDs = append(m.IDs, 999)
	m.Vecs = append(m.Vecs, fakeVec("red sports car")...)
	q := fakeVec("task: search result | query: sports car")

	hits := rank(m, q, nil, nil, 0, 3, 10)
	if len(hits) != 1 || hits[0].id != 999 {
		t.Fatalf("z-floor hits = %+v, want only 999", hits)
	}
	if hits := rank(m, q, nil, map[int64]bool{999: true, 5: true}, 0, 3, 10); len(hits) != 1 {
		t.Fatalf("filtered z-floor hits = %+v", hits)
	}
	if hits := rank(m, q, nil, nil, 999, 0, 10); len(hits) != 10 {
		t.Fatalf("no floor should return k hits, got %d", len(hits))
	}
}

func TestFullBackfillIsExclusive(t *testing.T) {
	f := setup(t)
	release := tryLock(f.st.Path() + ".embed.lock")
	if release == nil {
		t.Fatal("first lock must succeed")
	}
	defer release()
	if _, err := f.eng.Backfill(context.Background(), BackfillOptions{Text: true}); !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent full backfill: err = %v, want ErrBusy", err)
	}
	// Targeted refreshes are not blocked.
	if _, err := f.eng.Backfill(context.Background(), BackfillOptions{Text: true, IDs: []int64{f.ids["rock big"]}}); err != nil {
		t.Fatalf("targeted backfill: %v", err)
	}
}

func TestSparseImageCoverageIsIgnored(t *testing.T) {
	big := &store.Matrix{Dims: 2}
	for i := 0; i < 100; i++ {
		big.IDs = append(big.IDs, int64(i))
	}
	few := &store.Matrix{Dims: 2, IDs: []int64{1, 2}}
	if usable(few, big) {
		t.Fatal("2 previews in a 100-asset library must not drive ranking")
	}
	if !usable(big, few) || !usable(few, &store.Matrix{IDs: []int64{1, 2, 3}}) {
		t.Fatal("well-covered signals and small libraries stay usable")
	}
}
