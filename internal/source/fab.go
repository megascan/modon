package source

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"modon/internal/config"
	"modon/internal/model"
)

// Fab implements Epic's Fab.com internal web API:
//
//	GET https://www.fab.com/i/listings/search?keywords=&currency=&locale=&cursor=...
//	GET https://www.fab.com/i/listings/{uid}
//	GET https://www.fab.com/i/users/me/listings-states?listing_ids=...   (auth)
//	GET https://www.fab.com/i/listings/{uid}/asset-formats/{af}/files/{f}/download-info (auth)
//
// The API is undocumented and may change; parsing is defensive. Catalog
// search works unauthenticated; owned-asset download requires the user's
// fab.com session cookie (fab_cookie config or MODON_FAB_COOKIE env).
type Fab struct {
	cfg  *config.Config
	http *http.Client
	base string // overridable for tests
}

func NewFab(cfg *config.Config) *Fab {
	return &Fab{cfg: cfg, http: httpClient(cfg.HTTPTimeoutSeconds), base: "https://www.fab.com/i"}
}

func (f *Fab) Name() string    { return model.SourceFab }
func (f *Fab) Available() bool { return true }

func (f *Fab) headers() map[string]string {
	h := map[string]string{
		"Accept":           "application/json",
		"Accept-Language":  "en-US,en;q=0.9",
		"X-Requested-With": "XMLHttpRequest",
	}
	if cookie := f.cfg.FabCookie; cookie != "" {
		h["Cookie"] = cookie
		if csrf := extractCSRF(cookie); csrf != "" {
			h["X-CsrfToken"] = csrf
		}
	}
	return h
}

// extractCSRF pulls the csrf token out of a raw cookie header value.
func extractCSRF(cookie string) string {
	for _, part := range strings.Split(cookie, ";") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 && (kv[0] == "fab_csrftoken" || kv[0] == "csrftoken") {
			return kv[1]
		}
	}
	return ""
}

// flexFloat decodes a JSON number that may arrive as a number or a string
// (Fab's undocumented API is inconsistent here).
type flexFloat float64

func (f *flexFloat) UnmarshalJSON(b []byte) error {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	switch n := v.(type) {
	case float64:
		*f = flexFloat(n)
	case string:
		n = strings.TrimSpace(n)
		if n == "" {
			*f = 0
			return nil
		}
		var parsed float64
		if _, err := fmt.Sscanf(n, "%g", &parsed); err != nil {
			*f = 0
			return nil
		}
		*f = flexFloat(parsed)
	case nil:
		*f = 0
	default:
		*f = 0
	}
	return nil
}

// priceTierFlex decodes Fab's priceTier, which is a tier-id string in
// catalog search results but a {price, currencyCode, ...} object in
// listing-detail responses.
type priceTierFlex struct {
	Price        flexFloat
	CurrencyCode string
}

func (p *priceTierFlex) UnmarshalJSON(b []byte) error {
	trimmed := bytes.TrimSpace(b)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	if trimmed[0] == '"' {
		return nil // tier id string; price comes from startingPrice
	}
	var obj struct {
		Price        flexFloat `json:"price"`
		CurrencyCode string    `json:"currencyCode"`
	}
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil // defensive: unknown shape, treat as free/unknown
	}
	p.Price = obj.Price
	p.CurrencyCode = obj.CurrencyCode
	return nil
}

// fabThumbnail decodes Fab's thumbnail, which is {mediaUrl} in some
// responses and {images:[{url,...}]} in catalog search results.
type fabThumbnail struct {
	MediaURL string `json:"mediaUrl"`
	Images   []struct {
		URL string `json:"url"`
	} `json:"images"`
}

func (t fabThumbnail) url() string {
	if t.MediaURL != "" {
		return t.MediaURL
	}
	if len(t.Images) > 0 {
		return t.Images[0].URL
	}
	return ""
}

type fabListing struct {
	UID           string    `json:"uid"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	IsFree        bool      `json:"isFree"`
	ListingType   string    `json:"listingType"`
	AverageRating flexFloat `json:"averageRating"`
	ReviewCount   int       `json:"reviewCount"`
	Category      *struct {
		Name string `json:"name"`
	} `json:"category"`
	Tags []struct {
		Name string `json:"name"`
	} `json:"tags"`
	StartingPrice *struct {
		Price flexFloat `json:"price"`
	} `json:"startingPrice"`
	Licenses []struct {
		Name      string        `json:"name"`
		OfferID   string        `json:"offerId"`
		PriceTier priceTierFlex `json:"priceTier"`
	} `json:"licenses"`
	User *struct {
		SellerName string `json:"sellerName"`
	} `json:"user"`
	Thumbnails   []fabThumbnail `json:"thumbnails"`
	AssetFormats []struct {
		UID             string `json:"uid"`
		AssetFormatType *struct {
			Code string `json:"code"`
		} `json:"assetFormatType"`
		Files []struct {
			UID      string `json:"uid"`
			ID       string `json:"id"`
			Filename string `json:"filename"`
		} `json:"files"`
	} `json:"assetFormats"`
}

// freeOfferID returns the offer id of a zero-price license, if any.
func (l *fabListing) freeOfferID() string {
	for _, lic := range l.Licenses {
		if lic.PriceTier.Price == 0 {
			return lic.OfferID
		}
	}
	return ""
}

func (l *fabListing) toAsset() model.Asset {
	a := model.Asset{
		ID:          model.SourceFab + ":" + l.UID,
		Source:      model.SourceFab,
		Title:       l.Title,
		Description: stripHTML(l.Description),
		AssetType:   "model",
		URL:         "https://www.fab.com/listings/" + l.UID,
		Rating:      float64(l.AverageRating),
		ReviewCount: l.ReviewCount,
	}
	if l.User != nil {
		a.Creator = l.User.SellerName
	}
	if l.Category != nil {
		a.Category = strings.ToLower(l.Category.Name)
	}
	for _, t := range l.Tags {
		a.Tags = append(a.Tags, strings.ToLower(t.Name))
	}
	if strings.Contains(strings.ToLower(a.Category), "animation") || strings.EqualFold(l.ListingType, "animation") {
		a.AssetType = "animation"
	}
	var cur string
	for _, lic := range l.Licenses {
		price := float64(lic.PriceTier.Price)
		if price > 0 && (a.Price == 0 || price < a.Price) {
			a.Price = price
			cur = lic.PriceTier.CurrencyCode
		}
		if a.License == "" && lic.Name != "" {
			a.License = lic.Name
		}
	}
	if a.Price == 0 && l.StartingPrice != nil {
		a.Price = float64(l.StartingPrice.Price)
	}
	if cur == "" {
		cur = "USD"
	}
	a.Currency = cur
	a.IsFree = l.IsFree || a.Price == 0
	for _, th := range l.Thumbnails {
		if u := th.url(); u != "" {
			a.PreviewURL = u
			break
		}
	}
	for _, af := range l.AssetFormats {
		if af.AssetFormatType != nil && af.AssetFormatType.Code != "" {
			a.Formats = append(a.Formats, af.AssetFormatType.Code)
		}
	}
	return a
}

func stripHTML(s string) string {
	var b strings.Builder
	inTag := false
	for _, r := range s {
		switch {
		case r == '<':
			inTag = true
		case r == '>':
			inTag = false
		case !inTag:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// Search queries the Fab catalog. No authentication required.
func (f *Fab) Search(ctx context.Context, q model.Query) ([]model.Asset, error) {
	if q.Text == "" && q.Category == "" && len(q.Tags) == 0 {
		return nil, nil
	}
	keyword := q.Text
	if keyword == "" {
		keyword = strings.Join(append(q.Tags, q.Category), " ")
	}
	params := url.Values{}
	params.Set("keywords", keyword)
	params.Set("currency", f.cfg.Currency)
	params.Set("locale", "en-US")
	if q.Free != nil && *q.Free {
		params.Set("is_free", "true")
	}
	if q.Category != "" {
		params.Set("categories", q.Category)
	}

	body, code, _, err := httpGet(ctx, f.http, f.base+"/listings/search?"+params.Encode(), f.headers())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("fab search", code, body)
	}
	var resp struct {
		Results []fabListing `json:"results"`
		Next    string       `json:"next"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("fab: unexpected response: %w", err)
	}
	out := make([]model.Asset, 0, len(resp.Results))
	for i := range resp.Results {
		a := resp.Results[i].toAsset()
		if q.Animated != nil && *q.Animated && a.AssetType != "animation" &&
			!containsFold(a.Category+" "+strings.Join(a.Tags, " "), "animation") {
			continue
		}
		if q.Style != "" && !containsFold(a.Title+" "+a.Description+" "+strings.Join(a.Tags, " ")+" "+a.Category, q.Style) {
			continue
		}
		out = append(out, a)
	}
	// Detect owned assets when a session cookie is configured.
	if f.cfg.FabCookie != "" && len(out) > 0 {
		ids := make([]string, 0, len(out))
		for _, a := range out {
			ids = append(ids, strings.TrimPrefix(a.ID, model.SourceFab+":"))
		}
		if states, err := f.ListingStates(ctx, ids); err == nil {
			for i := range out {
				uid := strings.TrimPrefix(out[i].ID, model.SourceFab+":")
				out[i].Owned = states[uid]
				if out[i].Owned {
					out[i].PurchaseRequired = false
				}
			}
		}
	}
	return out, nil
}

// ListingStates checks acquired status for a batch of listing ids (requires
// the user's session cookie).
func (f *Fab) ListingStates(ctx context.Context, ids []string) (map[string]bool, error) {
	if f.cfg.FabCookie == "" {
		return nil, fmt.Errorf("fab: not authenticated (set fab_cookie)")
	}
	states := map[string]bool{}
	const batch = 24
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		params := url.Values{}
		for _, id := range ids[start:end] {
			params.Add("listing_ids", id)
		}
		body, code, _, err := httpGet(ctx, f.http, f.base+"/users/me/listings-states?"+params.Encode(), f.headers())
		if err != nil {
			return states, err
		}
		if code != http.StatusOK {
			return states, statusErr("fab listings-states", code, body)
		}
		var arr []struct {
			UID      string `json:"uid"`
			Acquired bool   `json:"acquired"`
		}
		if err := json.Unmarshal(body, &arr); err != nil {
			return states, err
		}
		for _, s := range arr {
			states[s.UID] = s.Acquired
		}
	}
	return states, nil
}

// Listing fetches one listing's details.
func (f *Fab) Listing(ctx context.Context, uid string) (*fabListing, error) {
	body, code, _, err := httpGet(ctx, f.http, f.base+"/listings/"+url.PathEscape(uid), f.headers())
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, statusErr("fab listing", code, body)
	}
	var l fabListing
	if err := json.Unmarshal(body, &l); err != nil {
		return nil, err
	}
	return &l, nil
}

func (f *Fab) Details(ctx context.Context, id string) (*model.Asset, error) {
	l, err := f.Listing(ctx, id)
	if err != nil {
		return nil, err
	}
	a := l.toAsset()
	if f.cfg.FabCookie != "" {
		if states, err := f.ListingStates(ctx, []string{id}); err == nil {
			a.Owned = states[id]
			if a.Owned {
				a.PurchaseRequired = false
			} else if !a.IsFree {
				a.PurchaseRequired = true
			}
		}
	}
	return &a, nil
}

func (f *Fab) Preview(ctx context.Context, a model.Asset) ([]byte, string, string, error) {
	if a.PreviewURL == "" {
		return nil, "", "", fmt.Errorf("fab asset %s has no preview image", a.ID)
	}
	data, code, _, err := httpGet(ctx, f.http, a.PreviewURL, nil)
	if err != nil || code != http.StatusOK {
		return nil, "", "", fmt.Errorf("preview fetch failed (HTTP %d): %v", code, err)
	}
	mime := sniffImage(data)
	path, _ := cachePreviewPath(model.SourceFab, a.ID, a.PreviewURL+extForMime(mime))
	if path != "" {
		_ = os.MkdirAll(filepath.Dir(path), 0o755)
		_ = os.WriteFile(path, data, 0o644)
	}
	return data, mime, path, nil
}

// Download resolves an owned Fab asset to files, or reports what the user
// needs to do (purchase / claim) when it is not owned.
func (f *Fab) Download(ctx context.Context, a model.Asset, destDir, mode string) (*model.DownloadResult, error) {
	uid := strings.TrimPrefix(a.ID, model.SourceFab+":")
	l, err := f.Listing(ctx, uid)
	if err != nil {
		return nil, err
	}
	a2 := l.toAsset()

	if f.cfg.FabCookie == "" {
		if !a2.IsFree {
			return &model.DownloadResult{
				Status:   model.StatusPurchaseRequired,
				Price:    a2.Price,
				Currency: a2.Currency,
				URL:      a2.URL,
				License:  a2.License,
				Message:  purchaseMessage(a2),
			}, nil
		}
		return &model.DownloadResult{
			Status:  model.StatusClaimRequired,
			URL:     a2.URL,
			License: a2.License,
			Message: "This free Fab asset must be added to your fab.com library before it can be downloaded. " +
				"Ask the user to claim it at " + a2.URL + " and configure fab_cookie (MODON_FAB_COOKIE) for automated downloads.",
		}, nil
	}

	states, err := f.ListingStates(ctx, []string{uid})
	if err != nil {
		return nil, err
	}
	owned := states[uid]
	if !owned && a2.IsFree {
		if f.cfg.FabAutoClaimFree {
			if err := f.claim(ctx, l); err != nil {
				return nil, fmt.Errorf("auto-claim failed: %w", err)
			}
			owned = true
		} else {
			return &model.DownloadResult{
				Status:  model.StatusClaimRequired,
				URL:     a2.URL,
				License: a2.License,
				Message: "Free asset is not in your Fab library yet. Claim it at " + a2.URL +
					" (or set fab_auto_claim_free=true to let modon claim it automatically), then retry.",
			}, nil
		}
	}
	if !owned {
		return &model.DownloadResult{
			Status:   model.StatusPurchaseRequired,
			Price:    a2.Price,
			Currency: a2.Currency,
			URL:      a2.URL,
			License:  a2.License,
			Message:  purchaseMessage(a2),
		}, nil
	}

	// Owned: walk asset formats → files → download-info → file URL.
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	var files []string
	var total int64
	for _, af := range l.AssetFormats {
		afid := af.UID
		if afid == "" && af.AssetFormatType != nil {
			afid = af.AssetFormatType.Code
		}
		for _, file := range af.Files {
			fid := file.UID
			if fid == "" {
				fid = file.ID
			}
			if afid == "" || fid == "" {
				continue
			}
			infoURL := fmt.Sprintf("%s/listings/%s/asset-formats/%s/files/%s/download-info?platform=windows",
				f.base, url.PathEscape(uid), url.PathEscape(afid), url.PathEscape(fid))
			body, code, _, err := httpGet(ctx, f.http, infoURL, f.headers())
			if err != nil || code != http.StatusOK {
				continue
			}
			var info struct {
				URL  string `json:"url"`
				Link string `json:"link"`
			}
			if json.Unmarshal(body, &info) != nil {
				continue
			}
			real := info.URL
			if real == "" {
				real = info.Link
			}
			if real == "" {
				continue
			}
			data, code, hdr, err := httpGet(ctx, f.http, real, nil)
			if err != nil || code != http.StatusOK {
				continue
			}
			name := file.Filename
			if name == "" {
				name = filenameFromURL(real, hdr, "fab_"+uid)
			}
			dst := filepath.Join(destDir, sanitizeFilename(name))
			if err := os.WriteFile(dst, data, 0o644); err != nil {
				return nil, err
			}
			files = append(files, dst)
			total += int64(len(data))
		}
	}
	if len(files) == 0 {
		return &model.DownloadResult{
			Status:  model.StatusUnavailable,
			URL:     a2.URL,
			Message: "Asset is owned but modon could not resolve its download URLs (Fab's download-info endpoint may have changed, or the asset must be downloaded through the Fab web UI / Epic Launcher). Listing: " + a2.URL,
		}, nil
	}
	return &model.DownloadResult{
		Status:     model.StatusDownloaded,
		Files:      files,
		TotalBytes: total,
		License:    a2.License,
		URL:        a2.URL,
		Message:    fmt.Sprintf("Downloaded %d owned Fab file(s).", len(files)),
	}, nil
}

// claim adds a free listing to the user's library (opt-in via config).
func (f *Fab) claim(ctx context.Context, l *fabListing) error {
	offerID := l.freeOfferID()
	if offerID == "" {
		return fmt.Errorf("no free offer found on listing %s", l.UID)
	}
	form := "--modonboundary\r\nContent-Disposition: form-data; name=\"offer_id\"\r\n\r\n" + offerID + "\r\n--modonboundary--\r\n"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.base+"/listings/"+url.PathEscape(l.UID)+"/add-to-library", strings.NewReader(form))
	if err != nil {
		return err
	}
	for k, v := range f.headers() {
		req.Header.Set(k, v)
	}
	req.Header.Set("Content-Type", "multipart/form-data; boundary=modonboundary")
	resp, err := f.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return statusErr("fab add-to-library", resp.StatusCode, b)
	}
	return nil
}

func purchaseMessage(a model.Asset) string {
	return fmt.Sprintf(
		"This Fab asset costs %s %.2f and is not in your library. "+
			"Ask the user whether they are willing to purchase it. If they agree, they must buy it at %s "+
			"(modon never purchases on the user's behalf); once owned, retry download_asset.",
		a.Currency, a.Price, a.URL)
}
