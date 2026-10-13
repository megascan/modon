package semantic

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"modon/internal/embed"
	importdir "modon/internal/importer"
	"modon/internal/model"
	"modon/internal/store"
)

// DocText builds the (title, body) document embedded for an asset.
//
// It is deliberately lean. Benchmarked on a 24.5k-asset library with
// synonym queries ("something to sit on" -> chair/bench/stool), embedding
// just the cleaned name (title "none") scored precision@20 0.78 against
// 0.70 for a document that also listed category, style, tags, type,
// materials and folders: that boilerplate is shared by thousands of assets
// and drowns out what the object is. Style, pack and tags stay reachable
// through keyword search and facet filters. Agent-written descriptions are
// the best signal and are always included; folders are added only when the
// file name says nothing.
func DocText(a model.Asset) (title, body string) {
	name := CleanName(a.Title)
	parts := []string{name}
	if d := strings.TrimSpace(a.Description); d != "" {
		parts = append(parts, d)
	}
	if a.AssetType == "animation" && len(a.AnimationNames) > 0 {
		parts = append(parts, "animations: "+strings.Join(capList(a.AnimationNames, 8), ", "))
	}
	if uninformative(name) && a.PrimaryFile != "" {
		if f := folderHint(a.PrimaryFile); f != "" {
			parts = append(parts, f)
		}
	}
	return "", strings.Join(parts, "; ")
}

// Leading pack/asset-kind codes used by Synty and similar libraries
// (SM_ static mesh, SK_ skinned, SF_ Simple Fantasy, ...) and the type
// abbreviations that follow them.
var (
	namePrefixes = map[string]bool{"sm": true, "sk": true, "sf": true, "si": true, "sr": true, "ai": true,
		"fx": true, "pf": true, "bp": true, "sh": true, "sp": true, "se": true}
	nameCodes = map[string]string{"veh": "vehicle", "bld": "building", "wep": "weapon", "env": "environment",
		"chr": "character", "fx": "effect", "gen": "", "sh": ""}
	camelBoundary = regexp.MustCompile(`([a-z])([A-Z])`)
)

// CleanName turns a file-derived title like "SF Veh BatteringRam 01" into
// "vehicle Battering Ram": drops pack prefixes and variant numbers, expands
// type codes and splits CamelCase.
func CleanName(title string) string {
	var out []string
	for i, t := range strings.Fields(title) {
		tl := strings.ToLower(t)
		if i == 0 && namePrefixes[tl] {
			continue
		}
		if _, err := strconv.Atoi(t); err == nil {
			continue
		}
		if exp, ok := nameCodes[tl]; ok {
			if exp != "" {
				out = append(out, exp)
			}
			continue
		}
		out = append(out, camelBoundary.ReplaceAllString(t, "$1 $2"))
	}
	if len(out) == 0 {
		return strings.TrimSpace(title)
	}
	return strings.Join(out, " ")
}

var genericNames = map[string]bool{"mesh": true, "model": true, "object": true, "untitled": true,
	"scene": true, "export": true, "default": true, "asset": true, "new": true, "final": true}

// uninformative reports names that do not identify the object ("mesh",
// "untitled 3", "a"), where the folder has to carry the meaning.
func uninformative(name string) bool {
	for _, t := range strings.Fields(strings.ToLower(name)) {
		if len(t) > 2 && !genericNames[t] {
			return false
		}
	}
	return true
}

func capList(ss []string, n int) []string {
	if len(ss) > n {
		return ss[:n]
	}
	return ss
}

// folderHint keeps the last two meaningful directory names of a path,
// skipping export plumbing (FBX, SourceFiles, Models, ..._v2).
func folderHint(p string) string {
	var segs []string
	dir := filepath.Dir(p)
	for i := 0; i < 5 && len(segs) < 2 && dir != "/" && dir != "." && dir != ""; i++ {
		if b := filepath.Base(dir); !plumbingDir.MatchString(b) {
			segs = append([]string{b}, segs...)
		}
		dir = filepath.Dir(dir)
	}
	return strings.Join(segs, " / ")
}

var plumbingDir = regexp.MustCompile(`(?i)^(fbx|obj|gltf|glb|models?|meshes|mesh|assets|prefabs|export(s|ed)?)$|source[ _-]?files|_v\d+$`)

func hashOf(parts ...string) string {
	h := sha1.New()
	for _, p := range parts {
		h.Write([]byte(p))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

func (e *Engine) textHash(a model.Asset) (doc, hash string) {
	title, body := DocText(a)
	doc = embed.Document(title, body)
	return doc, hashOf(e.modelID(), fmt.Sprint(e.dims()), doc)
}

// imageHash identifies the preview file version; "" when there is none.
func (e *Engine) imageHash(a model.Asset) string {
	if a.PreviewPath == "" {
		return ""
	}
	st, err := os.Stat(a.PreviewPath)
	if err != nil || st.IsDir() {
		return ""
	}
	return hashOf(e.modelID(), fmt.Sprint(e.dims()), a.PreviewPath, fmt.Sprint(st.Size()), fmt.Sprint(st.ModTime().UnixNano()))
}

// BackfillOptions controls an embedding run.
type BackfillOptions struct {
	Text      bool
	Images    bool
	Render    bool // render missing previews (Blender/f3d) before embedding images
	MaxRender int  // cap on renders, 0 = unlimited
	Limit     int  // cap on assets embedded per kind, 0 = all pending
	IDs       []int64
	Progress  func(stage string, done, total int)
}

// BackfillStats summarizes an embedding run.
type BackfillStats struct {
	Text        int      `json:"text_embedded"`
	Images      int      `json:"images_embedded"`
	Rendered    int      `json:"previews_rendered"`
	UpToDate    int      `json:"up_to_date"`
	NoPreview   int      `json:"without_preview"`
	Failed      int      `json:"failed"`
	Errors      []string `json:"errors,omitempty"`
	TextTotal   int      `json:"text_vectors"`
	ImagesTotal int      `json:"image_vectors"`
}

const (
	textBatch  = 32
	imageBatch = 8
)

// ErrBusy means another modon process is already running a full backfill
// against this index.
var ErrBusy = errors.New("another modon process is embedding this index; try again when it finishes")

// Backfill embeds every asset whose text document or preview changed since
// it was last embedded. It is resumable: vectors are written per batch.
// Full runs (no IDs) are exclusive across processes sharing the index.
func (e *Engine) Backfill(ctx context.Context, o BackfillOptions) (*BackfillStats, error) {
	if !e.available(ctx) {
		return nil, fmt.Errorf("embedding server unavailable: %s", e.statusHint())
	}
	if len(o.IDs) == 0 {
		release := tryLock(e.st.Path() + ".embed.lock")
		if release == nil {
			return nil, ErrBusy
		}
		defer release()
	}
	var assets []model.Asset
	var err error
	if len(o.IDs) > 0 {
		assets, err = e.st.GetByIDs(o.IDs)
	} else {
		assets, err = e.st.All()
	}
	if err != nil {
		return nil, err
	}
	stats := &BackfillStats{}
	progress := o.Progress
	if progress == nil {
		progress = func(string, int, int) {}
	}

	if o.Text {
		have, err := e.st.VectorHashes(store.VecText, e.modelID(), e.dims())
		if err != nil {
			return nil, err
		}
		type job struct {
			id        int64
			doc, hash string
		}
		var jobs []job
		for _, a := range assets {
			doc, h := e.textHash(a)
			if have[a.RowID] == h {
				stats.UpToDate++
				continue
			}
			jobs = append(jobs, job{a.RowID, doc, h})
		}
		if o.Limit > 0 && len(jobs) > o.Limit {
			jobs = jobs[:o.Limit]
		}
		for i := 0; i < len(jobs) && ctx.Err() == nil; i += textBatch {
			batch := jobs[i:min(i+textBatch, len(jobs))]
			ins := make([]embed.Input, len(batch))
			for j, b := range batch {
				ins[j] = embed.Input{Text: b.doc}
			}
			vecs, err := e.cl.Embed(ctx, ins)
			if err != nil {
				return stats, fmt.Errorf("embed text: %w", err)
			}
			for j, b := range batch {
				if err := e.st.PutVector(b.id, store.VecText, e.modelID(), b.hash, vecs[j]); err != nil {
					return stats, err
				}
				stats.Text++
			}
			progress("text", min(i+textBatch, len(jobs)), len(jobs))
		}
	}

	if o.Images && ctx.Err() == nil {
		if !e.vision(ctx) {
			stats.Errors = append(stats.Errors, embed.ErrNoVision.Error())
		} else {
			if err := e.embedImages(ctx, assets, o, stats, progress); err != nil {
				return stats, err
			}
		}
	}

	counts := e.st.VectorCounts()
	stats.TextTotal, stats.ImagesTotal = counts[store.VecText], counts[store.VecImage]
	return stats, ctx.Err()
}

func (e *Engine) embedImages(ctx context.Context, assets []model.Asset, o BackfillOptions, stats *BackfillStats, progress func(string, int, int)) error {
	if o.Render {
		failed, err := e.st.PreviewFailed()
		if err != nil {
			return err
		}
		var missing []int64
		for _, a := range assets {
			if e.imageHash(a) == "" && !failed[a.RowID] {
				missing = append(missing, a.RowID)
			}
		}
		if o.Limit > 0 && len(missing) > o.Limit {
			missing = missing[:o.Limit]
		}
		if len(missing) > 0 {
			stats.Rendered = importdir.RenderPreviews(ctx, e.st, e.cfg, missing, o.MaxRender, func(done, total int, _ string) {
				progress("render", done, total)
			})
			// Reload so new preview paths are visible.
			ids := make([]int64, len(assets))
			for i, a := range assets {
				ids[i] = a.RowID
			}
			var err error
			if assets, err = e.st.GetByIDs(ids); err != nil {
				return err
			}
		}
	}

	have, err := e.st.VectorHashes(store.VecImage, e.modelID(), e.dims())
	if err != nil {
		return err
	}
	type job struct {
		id         int64
		path, hash string
	}
	var jobs []job
	for _, a := range assets {
		h := e.imageHash(a)
		switch {
		case h == "":
			stats.NoPreview++
		case have[a.RowID] == h:
			stats.UpToDate++
		default:
			jobs = append(jobs, job{a.RowID, a.PreviewPath, h})
		}
	}
	if o.Limit > 0 && len(jobs) > o.Limit {
		jobs = jobs[:o.Limit]
	}
	for i := 0; i < len(jobs) && ctx.Err() == nil; i += imageBatch {
		batch := jobs[i:min(i+imageBatch, len(jobs))]
		ins := make([]embed.Input, len(batch))
		for j, b := range batch {
			ins[j] = embed.Input{Image: b.path}
		}
		vecs, err := e.cl.Embed(ctx, ins)
		if err != nil {
			// One unreadable image should not sink the batch: retry singly.
			for j, b := range batch {
				v, err := e.cl.EmbedOne(ctx, ins[j])
				if err != nil {
					stats.Failed++
					if len(stats.Errors) < 10 {
						stats.Errors = append(stats.Errors, fmt.Sprintf("local:%d: %v", b.id, err))
					}
					continue
				}
				if err := e.st.PutVector(b.id, store.VecImage, e.modelID(), b.hash, v); err != nil {
					return err
				}
				stats.Images++
			}
		} else {
			for j, b := range batch {
				if err := e.st.PutVector(b.id, store.VecImage, e.modelID(), b.hash, vecs[j]); err != nil {
					return err
				}
				stats.Images++
			}
		}
		progress("image", min(i+imageBatch, len(jobs)), len(jobs))
	}
	return nil
}

// Refresh re-embeds specific assets (after metadata edits or new previews),
// best-effort and quietly skipped when the server is down.
func (e *Engine) Refresh(ctx context.Context, ids ...int64) {
	if len(ids) == 0 || !e.available(ctx) {
		return
	}
	if _, err := e.Backfill(ctx, BackfillOptions{Text: true, Images: true, IDs: ids}); err != nil {
		e.fail(err)
	}
}
