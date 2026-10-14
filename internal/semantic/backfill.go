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
	"strings"
	"time"

	"modon/internal/embed"
	importdir "modon/internal/importer"
	"modon/internal/model"
	"modon/internal/naming"
	"modon/internal/store"
)

// DocText builds the (title, body) document embedded for an asset.
//
// It is deliberately lean. Benchmarked on a 24.5k-asset library with
// synonym queries ("something to sit on" -> chair/bench/stool), embedding
// just the cleaned name (title "none") scored precision@20 0.78 against
// 0.70 for a document that also listed category, style, tags, type,
// materials and folders: that boilerplate is shared by thousands of assets
// and drowns out what the object is. A re-run with only heuristic category,
// filtered heuristic tags and the pack name confirmed it (precision@10 on
// descriptive queries 0.69 -> 0.63, on synonym queries 0.65 -> 0.50; the
// pack name alone 0.63 / 0.60).
//
// Agent-written metadata is the exception and is always embedded: the
// description, and the category and tags once an agent wrote them
// (provenance "agent"), so tagging improves semantic search. Heuristic
// category, style, tags and the pack name stay reachable through keyword
// search (they are in the full-text index) and facet filters. Folders are
// added only when the file name says nothing.
func DocText(a model.Asset) (title, body string) {
	name := naming.CleanName(a.Title)
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
	seen := map[string]bool{}
	for _, w := range strings.Fields(strings.ToLower(name)) {
		seen[w] = true
	}
	var extra []string
	add := func(ws ...string) {
		for _, w := range ws {
			w = strings.ReplaceAll(w, "-", " ")
			if lw := strings.ToLower(w); lw != "" && !seen[lw] && !boilerTags[lw] {
				seen[lw] = true
				extra = append(extra, w)
			}
		}
	}
	if a.Provenance["category"] == model.ProvenanceAgent {
		add(a.Category)
	}
	if a.Provenance["tags"] == model.ProvenanceAgent {
		add(a.Tags...)
	}
	if len(extra) > 0 {
		parts = append(parts, strings.Join(extra, ", "))
	}
	return "", strings.Join(parts, "; ")
}

// boilerTags are importer tags that describe the file, not the object.
var boilerTags = map[string]bool{"fbx": true, "obj": true, "glb": true, "gltf": true, "blend": true, "stl": true,
	"ply": true, "dae": true, "textured": true, "untextured": true, "static": true, "mesh": true, "model": true}

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
// Full runs (no IDs) are exclusive across processes sharing the index and
// end by refreshing the duplicate clusters.
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
		have, err := e.hashes(store.VecText, o.IDs)
		if err != nil {
			return nil, err
		}
		var jobs []job
		for _, a := range assets {
			doc, h := e.textHash(a)
			if have[a.RowID] == h {
				stats.UpToDate++
				continue
			}
			jobs = append(jobs, job{a.RowID, embed.Input{Text: doc}, h})
		}
		if o.Limit > 0 && len(jobs) > o.Limit {
			jobs = jobs[:o.Limit]
		}
		n, err := e.runJobs(ctx, store.VecText, jobs, textBatch, stats, func(done int) { progress("text", done, len(jobs)) })
		stats.Text += n
		if err != nil {
			return stats, fmt.Errorf("embed text: %w", err)
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

	if len(o.IDs) == 0 && ctx.Err() == nil {
		if _, err := e.Dedupe(); err != nil {
			stats.Errors = append(stats.Errors, "dedupe: "+err.Error())
		}
	}
	counts := e.st.VectorCounts()
	stats.TextTotal, stats.ImagesTotal = counts[store.VecText], counts[store.VecImage]
	return stats, ctx.Err()
}

// hashes loads stored content hashes: only the given assets for a targeted
// refresh (constant cost per edit), the whole index for a full run.
func (e *Engine) hashes(kind string, ids []int64) (map[int64]string, error) {
	if len(ids) > 0 {
		return e.st.VectorHashesFor(kind, e.modelID(), e.dims(), ids)
	}
	return e.st.VectorHashes(kind, e.modelID(), e.dims())
}

type job struct {
	id   int64
	in   embed.Input
	hash string
}

// runJobs embeds jobs in batches and stores the vectors. Failed inputs are
// counted (first errors kept) without stopping the run; it only aborts
// when the server stays unreachable.
func (e *Engine) runJobs(ctx context.Context, kind string, jobs []job, batch int, stats *BackfillStats, progress func(done int)) (int, error) {
	stored := 0
	for i := 0; i < len(jobs) && ctx.Err() == nil; i += batch {
		b := jobs[i:min(i+batch, len(jobs))]
		ins := make([]embed.Input, len(b))
		for j, x := range b {
			ins[j] = x.in
		}
		vecs, errs, fatal := e.embedAll(ctx, ins)
		for j, x := range b {
			if errs[j] != nil {
				stats.Failed++
				if len(stats.Errors) < 10 {
					stats.Errors = append(stats.Errors, fmt.Sprintf("local:%d: %v", x.id, errs[j]))
				}
				continue
			}
			if err := e.st.PutVector(x.id, kind, e.modelID(), x.hash, vecs[j]); err != nil {
				return stored, err
			}
			stored++
		}
		if fatal != nil {
			return stored, fatal
		}
		progress(min(i+batch, len(jobs)))
	}
	return stored, nil
}

// Retry policy for embedding requests (variables so tests can shorten it).
var (
	retryAttempts = 3
	retryBackoff  = 2 * time.Second
	restartWait   = 3 * time.Minute
)

// embedAll embeds a batch and survives server trouble instead of failing
// items: transient errors (server restarting, GPU out of memory) are
// retried with backoff, restarting a vanished server when autostart is on;
// a batch that still fails is split in halves, so an oversized batch
// shrinks until it fits and one bad input fails alone. fatal is set when
// the server stays unreachable or ctx ends.
func (e *Engine) embedAll(ctx context.Context, ins []embed.Input) (vecs [][]float32, errs []error, fatal error) {
	vecs, errs = make([][]float32, len(ins)), make([]error, len(ins))
	var rec func(lo, hi int)
	rec = func(lo, hi int) {
		if fatal != nil {
			for i := lo; i < hi; i++ {
				errs[i] = fatal
			}
			return
		}
		v, err := e.embedRetry(ctx, ins[lo:hi])
		switch {
		case err == nil:
			copy(vecs[lo:hi], v)
			return
		case ctx.Err() != nil:
			fatal = ctx.Err()
		case embed.Unreachable(err):
			fatal = err
		}
		if fatal != nil || hi-lo == 1 {
			for i := lo; i < hi; i++ {
				errs[i] = err
			}
			return
		}
		mid := (lo + hi) / 2
		rec(lo, mid)
		rec(mid, hi)
	}
	rec(0, len(ins))
	return vecs, errs, fatal
}

func (e *Engine) embedRetry(ctx context.Context, ins []embed.Input) ([][]float32, error) {
	backoff := retryBackoff
	for attempt := 0; ; attempt++ {
		v, err := e.cl.Embed(ctx, ins)
		if err == nil || !embed.Transient(err) || attempt >= retryAttempts || ctx.Err() != nil {
			return v, err
		}
		if embed.Unreachable(err) && e.cfg.Embedding.Autostart {
			if serr := e.ensure(ctx, restartWait); serr == nil {
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
	}
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

	have, err := e.hashes(store.VecImage, o.IDs)
	if err != nil {
		return err
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
			jobs = append(jobs, job{a.RowID, embed.Input{Image: a.PreviewPath}, h})
		}
	}
	if o.Limit > 0 && len(jobs) > o.Limit {
		jobs = jobs[:o.Limit]
	}
	n, err := e.runJobs(ctx, store.VecImage, jobs, imageBatch, stats, func(done int) { progress("image", done, len(jobs)) })
	stats.Images += n
	if err != nil {
		return fmt.Errorf("embed images: %w", err)
	}
	return nil
}

// Refresh re-embeds specific assets (after metadata edits or new previews),
// best-effort and quietly skipped when the server is down.
func (e *Engine) Refresh(ctx context.Context, ids ...int64) {
	if len(ids) == 0 || !e.available(ctx) {
		return
	}
	_, _ = e.Backfill(ctx, BackfillOptions{Text: true, Images: true, IDs: ids})
}
