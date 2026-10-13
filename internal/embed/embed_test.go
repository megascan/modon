package embed

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestFitTruncatesAndNormalizes(t *testing.T) {
	v, err := Fit([]float32{3, 4, 12}, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 2 || math.Abs(float64(v[0])-0.6) > 1e-6 || math.Abs(float64(v[1])-0.8) > 1e-6 {
		t.Fatalf("Fit = %v, want [0.6 0.8]", v)
	}
	if _, err := Fit([]float32{float32(math.NaN()), 1}, 2); err == nil {
		t.Fatal("NaN embeddings must be rejected")
	}
	if _, err := Fit([]float32{1}, 2); err == nil {
		t.Fatal("short embeddings must be rejected")
	}
}

func TestPrefixes(t *testing.T) {
	if got := Query("wooden chair"); got != "task: search result | query: wooden chair" {
		t.Fatalf("Query = %q", got)
	}
	if got := Document("", "x"); got != "title: none | text: x" {
		t.Fatalf("Document = %q", got)
	}
	if got := Document("a|b", "x"); got != "title: a/b | text: x" {
		t.Fatalf("Document must not let titles break the prefix: %q", got)
	}
}

// A plain OpenAI-compatible server (no /modon/info) is text-only and may
// ignore "dimensions"; the client truncates.
func TestClientTextOnlyServer(t *testing.T) {
	var gotInput []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/embeddings" {
			http.NotFound(w, r)
			return
		}
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotInput = req.Input
		data := []map[string]any{}
		for i := range req.Input {
			data = append(data, map[string]any{"index": i, "embedding": []float32{1, 1, 0, 0, 5, 5}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	c := New(srv.URL, "", 4, 5*time.Second)
	vs, err := c.Embed(context.Background(), []Input{{Text: "a"}, {Text: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(vs) != 2 || len(vs[0]) != 4 {
		t.Fatalf("got %d vectors of %d dims", len(vs), len(vs[0]))
	}
	if s, ok := gotInput[0].(string); !ok || s != "a" {
		t.Fatalf("text inputs must be sent as plain strings, got %#v", gotInput[0])
	}
	if _, err := c.Embed(context.Background(), []Input{{Image: "x.png"}}); !errors.Is(err, ErrNoVision) {
		t.Fatalf("image against text-only server: err = %v, want ErrNoVision", err)
	}
}
