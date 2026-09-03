package source

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/store"
)

func TestSplitID(t *testing.T) {
	src, rid, err := SplitID("local:42")
	if err != nil || src != "local" || rid != "42" {
		t.Fatalf("got %q %q %v", src, rid, err)
	}
	if _, _, err := SplitID("bogus"); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := SplitID("local:"); err == nil {
		t.Fatal("expected error for empty rid")
	}
}

func testRegistry(t *testing.T) (*store.Store, *Registry) {
	t.Helper()
	cfg := config.Defaults()
	cfg.PolyPizzaAPIKey = ""
	cfg.FabCookie = ""
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st, NewRegistry(cfg, st)
}

func TestRegistryLocalOwnedFirst(t *testing.T) {
	st, reg := testRegistry(t)
	if _, err := st.Upsert(model.Asset{
		PrimaryFile: "/tmp/owned_chair.obj",
		Title:       "owned wooden chair",
		Name:        "owned wooden chair",
		AssetType:   "model",
		Category:    "chair",
		Description: "owned",
	}); err != nil {
		t.Fatal(err)
	}
	// No remote credentials: restrict to local for a hermetic test
	// (Fab catalog search works unauthenticated and would hit the network).
	out, err := reg.Search(context.Background(), model.Query{Text: "chair", Limit: 5}, []string{"local"})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 1 || out.Results[0].Source != model.SourceLocal {
		t.Fatalf("results=%+v", out.Results)
	}
	if !out.Results[0].Owned {
		t.Fatal("local asset should be owned")
	}
}

func TestApplyPolicyDeny(t *testing.T) {
	cfg := config.Defaults()
	cfg.PurchasePolicy = "deny"
	st, err := store.Open(filepath.Join(t.TempDir(), "idx.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	reg := NewRegistry(cfg, st)
	paid := model.Asset{ID: "fab:x", Source: "fab", Title: "paid", Price: 10, Currency: "USD"}
	if got := reg.applyPolicy(paid); got != nil {
		t.Fatal("deny should drop paid assets")
	}
	cfg.PurchasePolicy = "ask"
	free := model.Asset{ID: "fab:y", Source: "fab", Title: "free", IsFree: true}
	if got := reg.applyPolicy(free); got == nil || got.PurchaseRequired {
		t.Fatalf("free should pass without flag: %+v", got)
	}
	paid2 := model.Asset{ID: "fab:z", Source: "fab", Title: "paid", Price: 5, Currency: "USD"}
	if got := reg.applyPolicy(paid2); got == nil || !got.PurchaseRequired {
		t.Fatalf("ask should flag paid: %+v", got)
	}
}

func TestFabSearchShapes(t *testing.T) {
	// Search-shape payload: priceTier is a tier-id string, thumbnails use
	// {images:[{url}]}, price lives in startingPrice.
	searchPayload := `{"results":[{"uid":"abc","title":"Test Chair","isFree":false,` +
		`"startingPrice":{"price":49.99},"licenses":[{"name":"Personal","offerId":"o1","priceTier":"tier_USD_4999"}],` +
		`"thumbnails":[{"images":[{"url":"https://example.com/a.jpg"}]}],` +
		`"user":{"sellerName":"Someone"},"category":{"name":"Props"},` +
		`"tags":[{"name":"chair"}],"averageRating":4.5,"reviewCount":3}]}`
	var searchResp struct {
		Results []fabListing `json:"results"`
	}
	if err := json.Unmarshal([]byte(searchPayload), &searchResp); err != nil {
		t.Fatalf("search shape: %v", err)
	}
	a := searchResp.Results[0].toAsset()
	if a.Price != 49.99 || a.Currency != "USD" {
		t.Fatalf("price=%v %q", a.Price, a.Currency)
	}
	if a.PreviewURL != "https://example.com/a.jpg" {
		t.Fatalf("preview=%q", a.PreviewURL)
	}
	if a.Creator != "Someone" || len(a.Tags) == 0 {
		t.Fatalf("creator/tags: %+v", a)
	}

	// Detail-shape payload: priceTier is an object; thumbnail uses mediaUrl.
	detailPayload := `{"uid":"abc","title":"Test Chair","isFree":false,` +
		`"licenses":[{"name":"Personal","offerId":"o1","priceTier":{"price":"19.99","currencyCode":"USD"}}],` +
		`"thumbnails":[{"mediaUrl":"https://example.com/b.png"}]}`
	var detail fabListing
	if err := json.Unmarshal([]byte(detailPayload), &detail); err != nil {
		t.Fatalf("detail shape: %v", err)
	}
	a2 := detail.toAsset()
	if a2.Price != 19.99 || a2.Currency != "USD" {
		t.Fatalf("detail price=%v %q", a2.Price, a2.Currency)
	}
	if a2.PreviewURL != "https://example.com/b.png" {
		t.Fatalf("detail preview=%q", a2.PreviewURL)
	}
}
