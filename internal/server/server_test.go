package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

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

// countingEmbedder is a stand-in embedding server that counts requests.
func countingEmbedder(t *testing.T, requests *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/modon/info":
			_ = json.NewEncoder(w).Encode(map[string]any{"model": "fake", "vision": true, "dims": 8})
		case "/v1/embeddings":
			requests.Add(1)
			var req struct {
				Input []any `json:"input"`
			}
			_ = json.NewDecoder(r.Body).Decode(&req)
			data := make([]map[string]any, len(req.Input))
			for i := range req.Input {
				v := make([]float32, 8)
				v[i%8] = 1
				data[i] = map[string]any{"index": i, "embedding": v}
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestApplyBatchEmbedsOnce(t *testing.T) {
	d, ids := testDeps(t)
	var requests atomic.Int32
	srv := countingEmbedder(t, &requests)
	d.Cfg.Embedding.Endpoint, d.Cfg.Embedding.Model, d.Cfg.Embedding.Dims = srv.URL, "fake", 8
	d.Sem = semantic.New(d.Cfg, d.St)
	ctx := context.Background()

	items := []UpdateItem{
		{ID: fmt.Sprintf("local:%d", ids[0]), Category: "lamp", Tags: []string{"desk lamp", "brass"}, TagsMode: "replace", ApplyToDuplicates: true},
		{ID: fmt.Sprintf("local:%d", ids[2]), Description: "A plain office desk", Category: "furniture"},
		{ID: "local:999", Category: "rock"},
		{Category: "rock"},
	}
	res := ApplyBatch(ctx, d, items)
	if res.Updated != 3 || len(res.Assets) != 3 {
		t.Fatalf("updated = %d, assets = %+v", res.Updated, res.Assets)
	}
	if len(res.Errors) != 2 || res.Errors[0].Item != 2 || res.Errors[1].Item != 3 {
		t.Fatalf("errors = %+v", res.Errors)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("a batch of 3 edits should re-embed in one request, got %d", n)
	}
	if res.Embedding == nil || res.Embedding.Text != 3 {
		t.Fatalf("embedding = %+v", res.Embedding)
	}
	for _, id := range ids {
		a, _ := d.St.GetByID(id)
		if a.NeedsReview || a.Provenance["category"] != model.ProvenanceAgent {
			t.Fatalf("local:%d = %+v", id, a)
		}
	}

	// The MCP tool takes the same edits as items.
	requests.Store(0)
	res2, out, _ := handleUpdateMetadata(ctx, d, UpdateMetadataInput{Items: []UpdateItem{
		{ID: fmt.Sprintf("local:%d", ids[2]), Tags: []string{"wooden"}},
		{ID: fmt.Sprintf("local:%d", ids[0]), RemoveTags: []string{"brass"}},
	}})
	if res2.IsError || out.(*BatchResult).Updated != 2 {
		t.Fatalf("items: %+v", res2.Content)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("items should re-embed in one request, got %d", n)
	}
}

func TestUpdateToolSchemaHasItems(t *testing.T) {
	d, _ := testDeps(t)
	ctx := context.Background()
	ct, st := mcp.NewInMemoryTransports()
	if _, err := NewMCPServer(d).Connect(ctx, st, nil); err != nil {
		t.Fatal(err)
	}
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "t"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	tools, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tl := range tools.Tools {
		if tl.Name != "update_asset_metadata" {
			continue
		}
		raw, _ := json.Marshal(tl.InputSchema)
		for _, p := range []string{`"items"`, `"category"`, `"apply_to_duplicates"`} {
			if !strings.Contains(string(raw), p) {
				t.Fatalf("schema lacks %s: %s", p, raw)
			}
		}
		return
	}
	t.Fatal("update_asset_metadata not registered")
}
