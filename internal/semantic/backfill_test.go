package semantic

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
	"time"

	"modon/internal/config"
	"modon/internal/embed"
	"modon/internal/store"
)

func flakyServer(t *testing.T, maxBatch int, failFirst int32, requests *atomic.Int32) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/modon/info" {
			json.NewEncoder(w).Encode(map[string]any{"model": "fake", "vision": true, "dims": testDims})
			return
		}
		n := requests.Add(1)
		var req struct {
			Input []any `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		if n <= failFirst || len(req.Input) > maxBatch {
			http.Error(w, `{"error":{"message":"CUDA out of memory"}}`, http.StatusServiceUnavailable)
			return
		}
		var data []map[string]any
		for i, in := range req.Input {
			s, _ := in.(string)
			if strings.Contains(s, "bad") {
				http.Error(w, `{"error":{"message":"bad input"}}`, http.StatusBadRequest)
				return
			}
			data = append(data, map[string]any{"index": i, "embedding": fakeVec(s)})
		}
		json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func fastRetries(t *testing.T) {
	a, b := retryAttempts, retryBackoff
	retryAttempts, retryBackoff = 3, time.Millisecond
	t.Cleanup(func() { retryAttempts, retryBackoff = a, b })
}

func engineFor(t *testing.T, url string) *Engine {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	cfg := config.Defaults()
	cfg.Embedding.Endpoint, cfg.Embedding.Dims, cfg.Embedding.Model, cfg.Embedding.TimeoutSeconds = url, testDims, "fake", 5
	return New(cfg, st)
}

func TestEmbedAllRetriesAndSplits(t *testing.T) {
	fastRetries(t)
	var requests atomic.Int32
	e := engineFor(t, flakyServer(t, 4, 1, &requests).URL)
	ins := make([]embed.Input, 10)
	for i := range ins {
		ins[i] = embed.Input{Text: fmt.Sprintf("doc %d", i)}
	}
	ins[7].Text = "a bad doc"
	vecs, errs, fatal := e.embedAll(context.Background(), ins)
	if fatal != nil {
		t.Fatal(fatal)
	}
	for i := range ins {
		if i == 7 {
			if errs[i] == nil || vecs[i] != nil {
				t.Fatal("the bad input must fail alone")
			}
			continue
		}
		if errs[i] != nil || len(vecs[i]) != testDims {
			t.Fatalf("input %d: %v", i, errs[i])
		}
	}
}

func TestEmbedAllStopsWhenServerIsGone(t *testing.T) {
	fastRetries(t)
	e := engineFor(t, "http://127.0.0.1:1")
	_, errs, fatal := e.embedAll(context.Background(), []embed.Input{{Text: "a"}, {Text: "b"}, {Text: "c"}})
	if fatal == nil || errs[2] == nil {
		t.Fatalf("unreachable server must abort the run: fatal=%v", fatal)
	}
}

func TestRefreshIsTargeted(t *testing.T) {
	f := setup(t)
	ctx := context.Background()
	if _, err := f.eng.Backfill(ctx, BackfillOptions{Text: true, Images: true}); err != nil {
		t.Fatal(err)
	}
	before := *f.calls
	id := f.ids["rock big"]
	if _, err := f.st.UpdateMetadata(id, store.MetadataUpdate{Description: "a granite boulder"}); err != nil {
		t.Fatal(err)
	}
	f.eng.Refresh(ctx, id)
	if got := *f.calls - before; got != 1 {
		t.Fatalf("refresh embedded %d inputs, want 1", got)
	}
}
