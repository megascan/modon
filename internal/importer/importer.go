// Package importdir walks asset directories, parses files, classifies them
// and upserts them into the local index.
package importdir

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"modon/internal/classify"
	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/parse"
	"modon/internal/preview"
	"modon/internal/store"
)

// Stats summarizes an import run.
type Stats struct {
	Added      int            `json:"added"`
	Updated    int            `json:"updated"`
	Skipped    int            `json:"skipped"`
	Previews   int            `json:"previews_rendered"`
	ByFormat   map[string]int `json:"by_format"`
	Errors     []string       `json:"errors,omitempty"`
	TotalFiles int            `json:"total_scanned"`
}

// Options controls an import run.
type Options struct {
	Recursive       bool
	RenderPreviews  bool
	MaxPreviews     int // cap for preview rendering (0 = unlimited)
	Progress        func(done, total int, path string)
	PreviewProgress func(done, total int, path string)
}

type impAsset struct {
	rowID int64
	file  string
}

// Directory imports one directory into the index.
func Directory(ctx context.Context, st *store.Store, cfg *config.Config, dir string, opts Options) (*Stats, error) {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(dir)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}

	var files []string
	werr := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries
		}
		if d.IsDir() {
			if p != dir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "node_modules") {
				return fs.SkipDir
			}
			if !opts.Recursive && p != dir {
				return fs.SkipDir
			}
			return nil
		}
		if parse.Extensions[strings.ToLower(filepath.Ext(d.Name()))] {
			files = append(files, p)
		}
		return nil
	})
	if werr != nil {
		return nil, werr
	}
	sort.Strings(files)

	stats := &Stats{ByFormat: map[string]int{}}
	imported := make([]impAsset, 0, len(files))

	for i, f := range files {
		if ctx.Err() != nil {
			break
		}
		if opts.Progress != nil {
			opts.Progress(i+1, len(files), f)
		}
		rel, rerr := filepath.Rel(dir, f)
		if rerr != nil {
			rel = filepath.Base(f)
		}
		dirTokens := strings.FieldsFunc(filepath.Dir(rel), func(r rune) bool { return r == '/' || r == '\\' })

		meta, err := parse.File(f)
		if err != nil {
			stats.Skipped++
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		res := classify.Infer(f, dirTokens, meta)

		// Embed any preview image found inside the file (FBX thumbnails).
		var embeddedPath string
		if len(meta.EmbeddedPreview) > 0 {
			if cd, err := config.CacheDir(); err == nil {
				embeddedPath = filepath.Join(cd, "previews", fmt.Sprintf("embedded_%x.png", hashName(f)))
				if err := os.MkdirAll(filepath.Dir(embeddedPath), 0o755); err == nil {
					_ = os.WriteFile(embeddedPath, meta.EmbeddedPreview, 0o644)
				}
			}
		}

		existed, _ := st.ExistsByPath(f)
		a := model.Asset{
			Source:         model.SourceLocal,
			Title:          meta.Name,
			Name:           meta.Name,
			AssetType:      res.AssetType,
			Category:       res.Category,
			Style:          res.Style,
			Tags:           res.Tags,
			Animated:       len(meta.AnimationNames) > 0,
			AnimationNames: meta.AnimationNames,
			TriCount:       meta.TriCount,
			VertCount:      meta.VertCount,
			Formats:        []string{meta.Format},
			PrimaryFile:    f,
			Files:          meta.RelatedFiles,
			FileSize:       meta.FileSize,
			TextureCount:   meta.TextureCount,
			MaterialNames:  meta.MaterialNames,
			License:        "unspecified (local file)",
			PreviewPath:    embeddedPath,
			NeedsReview:    true,
		}
		rowID, err := st.Upsert(a)
		if err != nil {
			stats.Skipped++
			stats.Errors = append(stats.Errors, fmt.Sprintf("%s: %v", f, err))
			continue
		}
		if existed {
			stats.Updated++
		} else {
			stats.Added++
		}
		stats.ByFormat[meta.Format]++
		imported = append(imported, impAsset{rowID: rowID, file: f})
	}

	if opts.RenderPreviews {
		stats.Previews = renderPreviews(ctx, st, cfg, imported, opts)
	}
	stats.TotalFiles = len(files)
	return stats, nil
}

// renderPreviews renders thumbnails for imported assets that lack one.
func renderPreviews(ctx context.Context, st *store.Store, cfg *config.Config, assets []impAsset, opts Options) int {
	ids := make([]int64, len(assets))
	for i, a := range assets {
		ids[i] = a.rowID
	}
	return RenderPreviews(ctx, st, cfg, ids, opts.MaxPreviews, opts.PreviewProgress)
}

// RenderPreviews renders Blender/f3d thumbnails for the given local assets
// that have none yet and returns how many were rendered. Jobs go out in
// batches (one Blender session renders a whole batch), preview.workers
// sessions at a time. limit caps the number attempted (0 = all).
func RenderPreviews(ctx context.Context, st *store.Store, cfg *config.Config, ids []int64, limit int, progress func(done, total int, path string)) int {
	r := preview.New(cfg)
	if r.Available() == "" {
		return 0
	}
	cache, err := config.CacheDir()
	if err != nil {
		return 0
	}
	var jobs []preview.Job
	idFor := map[string]int64{}
	for _, id := range ids {
		if limit > 0 && len(jobs) >= limit {
			break
		}
		a, err := st.GetByID(id)
		if err != nil || a.PreviewPath != "" {
			continue
		}
		out := filepath.Join(cache, "previews", fmt.Sprintf("%d.png", id))
		jobs = append(jobs, preview.Job{Asset: a.PrimaryFile, Out: out})
		idFor[out] = id
	}
	if len(jobs) == 0 {
		return 0
	}
	workers := cfg.Preview.Workers
	if workers <= 0 {
		workers = 2
	}
	const batchSize = 25
	batches := make(chan []preview.Job)
	go func() {
		defer close(batches)
		for i := 0; i < len(jobs); i += batchSize {
			select {
			case batches <- jobs[i:min(i+batchSize, len(jobs))]:
			case <-ctx.Done():
				return
			}
		}
	}()

	var done, rendered atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range batches {
				r.RenderBatch(ctx, b, cfg.Preview.Size, func(j preview.Job, err error) {
					id := idFor[j.Out]
					if err != nil {
						_ = st.SetPreview(id, "", "failed")
					} else {
						_ = st.SetPreview(id, j.Out, "ok")
						rendered.Add(1)
					}
					if progress != nil {
						progress(int(done.Add(1)), len(jobs), j.Asset)
					}
				})
			}
		}()
	}
	wg.Wait()
	return int(rendered.Load())
}

func hashName(s string) uint64 {
	var h uint64 = 14695981039346656037
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}
