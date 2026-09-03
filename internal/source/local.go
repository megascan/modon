package source

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"modon/internal/config"
	"modon/internal/model"
	"modon/internal/preview"
	"modon/internal/store"
)

// Local serves assets from the on-disk SQLite index.
type Local struct {
	Store  *store.Store
	cfg    *config.Config
	render *preview.Renderer
}

func NewLocal(st *store.Store, cfg *config.Config) *Local {
	return &Local{Store: st, cfg: cfg, render: preview.New(cfg)}
}

func (l *Local) Name() string    { return model.SourceLocal }
func (l *Local) Available() bool { return true }

func (l *Local) Search(ctx context.Context, q model.Query) ([]model.Asset, error) {
	return l.Store.Search(q)
}

func (l *Local) Details(ctx context.Context, id string) (*model.Asset, error) {
	rowID, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("local asset ids are numeric, got %q", id)
	}
	a, err := l.Store.GetByID(rowID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// Preview returns the cached thumbnail, rendering one on demand when a
// renderer (blender/f3d) is available.
func (l *Local) Preview(ctx context.Context, a model.Asset) ([]byte, string, string, error) {
	if a.PreviewPath != "" {
		if data, mime, err := readImage(a.PreviewPath); err == nil {
			return data, mime, a.PreviewPath, nil
		}
	}
	if l.render.Available() == "" {
		return nil, "", "", fmt.Errorf("no preview available for %s and no renderer installed (blender/f3d)", a.Title)
	}
	if a.RowID == 0 {
		return nil, "", "", fmt.Errorf("asset is not indexed")
	}
	cache, err := config.CacheDir()
	if err != nil {
		return nil, "", "", err
	}
	out := filepath.Join(cache, "previews", fmt.Sprintf("%d.png", a.RowID))
	if err := l.render.Render(ctx, a.PrimaryFile, out, l.cfg.Preview.Size); err != nil {
		return nil, "", "", err
	}
	_ = l.Store.SetPreview(a.RowID, out, "ok")
	data, mime, err := readImage(out)
	return data, mime, out, err
}

func (l *Local) Download(ctx context.Context, a model.Asset, destDir, mode string) (*model.DownloadResult, error) {
	files := localAssetFiles(a)
	if len(files) == 0 {
		return nil, fmt.Errorf("asset files missing on disk: %s", a.PrimaryFile)
	}
	if mode == "reference" {
		return &model.DownloadResult{
			Status:     model.StatusReference,
			Files:      files,
			TotalBytes: a.FileSize,
			License:    a.License,
			Message:    "Asset referenced in place; no files copied.",
		}, nil
	}
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return nil, err
	}
	var out []string
	var total int64
	for _, f := range files {
		rel, err := filepath.Rel(filepath.Dir(a.PrimaryFile), f)
		if err != nil || strings.HasPrefix(rel, "..") {
			rel = filepath.Base(f)
		}
		dst := filepath.Join(destDir, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return nil, err
		}
		if mode == "symlink" {
			_ = os.Remove(dst)
			if err := os.Symlink(f, dst); err != nil {
				return nil, err
			}
		} else {
			if err := copyFile(f, dst); err != nil {
				return nil, err
			}
		}
		if st, err := os.Stat(f); err == nil {
			total += st.Size()
		}
		out = append(out, dst)
	}
	msg := fmt.Sprintf("Copied %d file(s) from the local index.", len(out))
	if mode == "symlink" {
		msg = fmt.Sprintf("Linked %d file(s) from the local index.", len(out))
	}
	return &model.DownloadResult{
		Status:     model.StatusDownloaded,
		Files:      out,
		TotalBytes: total,
		License:    a.License,
		Message:    msg,
	}, nil
}

func localAssetFiles(a model.Asset) []string {
	var files []string
	if a.PrimaryFile != "" {
		if _, err := os.Stat(a.PrimaryFile); err == nil {
			files = append(files, a.PrimaryFile)
		}
	}
	for _, f := range a.Files {
		if _, err := os.Stat(f); err == nil {
			files = append(files, f)
		}
	}
	return files
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
