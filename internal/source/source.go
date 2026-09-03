// Package source defines the pluggable asset sources (local index,
// poly.pizza, Fab) and the coordinator that searches them in priority order.
package source

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/preview"
	"modon/internal/store"
)

// Source is one searchable/downloadable asset backend.
type Source interface {
	Name() string
	// Available reports whether the source is usable (e.g. API key present).
	Available() bool
	Search(ctx context.Context, q model.Query) ([]model.Asset, error)
	Details(ctx context.Context, id string) (*model.Asset, error)
	// Preview returns image bytes (with mime type) and/or a cached file path.
	Preview(ctx context.Context, a model.Asset) (data []byte, mime, path string, err error)
	Download(ctx context.Context, a model.Asset, destDir, mode string) (*model.DownloadResult, error)
}

// Registry wires sources together and implements cross-source behavior:
// owned-first ordering, purchase policy and price gating.
type Registry struct {
	Local     *Local
	PolyPizza *PolyPizza
	Fab       *Fab
	order     []string
	cfg       *config.Config
	render    *preview.Renderer
}

// NewRegistry builds the standard three sources.
func NewRegistry(cfg *config.Config, st *store.Store) *Registry {
	r := &Registry{
		Local:     NewLocal(st, cfg),
		PolyPizza: NewPolyPizza(cfg),
		Fab:       NewFab(cfg),
		order:     cfg.DefaultSources,
		cfg:       cfg,
		render:    preview.New(cfg),
	}
	return r
}

func (r *Registry) source(name string) Source {
	switch name {
	case model.SourceLocal:
		return r.Local
	case model.SourcePolyPizza:
		return r.PolyPizza
	case model.SourceFab:
		return r.Fab
	}
	return nil
}

// Outcome is the aggregated result of a multi-source search.
type Outcome struct {
	Results  []model.Asset `json:"results"`
	Searched []string      `json:"sources_searched"`
	Notes    []string      `json:"notes,omitempty"`
	Errors   []string      `json:"errors,omitempty"`
}

// Search runs the query against the requested sources in order (default:
// owned/local first, then poly.pizza, then Fab) and applies the purchase
// policy and price gates to paid results.
func (r *Registry) Search(ctx context.Context, q model.Query, sources []string) (*Outcome, error) {
	if len(sources) == 0 {
		sources = r.order
	}
	out := &Outcome{Searched: []string{}}
	limit := q.Limit
	if limit <= 0 {
		limit = 12
	}

	for _, name := range sources {
		src := r.source(strings.ToLower(strings.TrimSpace(name)))
		if src == nil {
			out.Notes = append(out.Notes, fmt.Sprintf("unknown source %q (use local, polypizza, fab)", name))
			continue
		}
		if !src.Available() {
			out.Notes = append(out.Notes, unavailableNote(src.Name(), r.cfg))
			continue
		}
		sq := q
		sq.Limit = limit
		res, err := src.Search(ctx, sq)
		if err != nil {
			out.Errors = append(out.Errors, fmt.Sprintf("%s: %v", src.Name(), err))
			continue
		}
		out.Searched = append(out.Searched, src.Name())

		kept := 0
		for _, a := range res {
			pa := r.applyPolicy(a)
			if pa == nil {
				continue
			}
			out.Results = append(out.Results, *pa)
			kept++
			if kept >= limit {
				break
			}
		}
	}
	return out, nil
}

// applyPolicy drops assets excluded by the purchase policy / price gates and
// flags the ones that need a user decision.
func (r *Registry) applyPolicy(a model.Asset) *model.Asset {
	if a.IsFree || a.Owned || a.Price == 0 {
		a.PurchaseRequired = false
		return &a
	}
	if r.cfg.PurchasePolicy == "deny" {
		return nil
	}
	if r.cfg.MaxPrice > 0 && a.Price > r.cfg.MaxPrice {
		return nil
	}
	if !a.Owned {
		a.PurchaseRequired = true
	}
	return &a
}

func unavailableNote(name string, cfg *config.Config) string {
	switch name {
	case model.SourcePolyPizza:
		return "polypizza skipped: no API key configured (modon config set polypizza_api_key <KEY>, or MODON_POLYPIZZA_API_KEY env)"
	case model.SourceFab:
		return "fab searched without account: owned assets cannot be detected; set fab_cookie (or MODON_FAB_COOKIE) to enable owned downloads"
	default:
		return name + " unavailable"
	}
}

// Get dispatches a namespaced id ("local:3", "polypizza:abc", "fab:uid") to
// its source and returns full details.
func (r *Registry) Get(ctx context.Context, id string) (*model.Asset, error) {
	srcName, rid, err := SplitID(id)
	if err != nil {
		return nil, err
	}
	switch srcName {
	case model.SourceLocal:
		return r.Local.Details(ctx, rid)
	case model.SourcePolyPizza:
		return r.PolyPizza.Details(ctx, rid)
	case model.SourceFab:
		return r.Fab.Details(ctx, rid)
	}
	return nil, fmt.Errorf("unknown source %q", srcName)
}

// SplitID parses "source:id".
func SplitID(id string) (src, rid string, err error) {
	i := strings.Index(id, ":")
	if i <= 0 || i == len(id)-1 {
		return "", "", fmt.Errorf("asset id %q must look like 'local:3', 'polypizza:abc' or 'fab:<uid>'", id)
	}
	return strings.ToLower(id[:i]), id[i+1:], nil
}

// Download dispatches a download request to the right source.
func (r *Registry) Download(ctx context.Context, id, destDir, mode string) (*model.Asset, *model.DownloadResult, error) {
	srcName, rid, err := SplitID(id)
	if err != nil {
		return nil, nil, err
	}
	var src Source
	switch srcName {
	case model.SourceLocal:
		src = r.Local
	case model.SourcePolyPizza:
		src = r.PolyPizza
	case model.SourceFab:
		src = r.Fab
	default:
		return nil, nil, fmt.Errorf("unknown source %q", srcName)
	}
	a, err := src.Details(ctx, rid)
	if err != nil {
		return nil, nil, err
	}
	a = r.applyPolicy(*a)
	if a == nil {
		return nil, nil, fmt.Errorf("asset excluded by purchase policy (max_price or purchase_policy=deny); adjust config to include it")
	}
	if destDir == "" {
		destDir, _ = defaultDownloadDir(r.cfg)
	}
	if mode == "" {
		mode = "copy"
	}
	res, err := src.Download(ctx, *a, destDir, mode)
	return a, res, err
}

func defaultDownloadDir(cfg *config.Config) (string, error) {
	if p := os.Getenv("MODON_DOWNLOADS"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ".", nil
	}
	return filepath.Join(home, ".local", "share", "modon", "downloads"), nil
}

// Preview fetches image bytes for any asset, rendering local thumbnails when
// needed and caching remote images on disk.
func (r *Registry) Preview(ctx context.Context, id string) (data []byte, mime, path string, err error) {
	srcName, rid, err := SplitID(id)
	if err != nil {
		return nil, "", "", err
	}
	var src Source
	switch srcName {
	case model.SourceLocal:
		src = r.Local
	case model.SourcePolyPizza:
		src = r.PolyPizza
	case model.SourceFab:
		src = r.Fab
	default:
		return nil, "", "", fmt.Errorf("unknown source %q", srcName)
	}
	a, err := src.Details(ctx, rid)
	if err != nil {
		return nil, "", "", err
	}
	return src.Preview(ctx, *a)
}

// cachePreviewPath picks a stable cache path for a remote preview image.
func cachePreviewPath(source, id, url string) (string, error) {
	dir, err := config.CacheDir()
	if err != nil {
		return "", err
	}
	ext := filepath.Ext(strings.SplitN(url, "?", 2)[0])
	if ext == "" || len(ext) > 5 {
		ext = ".img"
	}
	h := sha1.Sum([]byte(source + ":" + id))
	return filepath.Join(dir, "previews", source+"_"+hex.EncodeToString(h[:8])+ext), nil
}
