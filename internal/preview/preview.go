// Package preview renders local thumbnails with Blender (headless Workbench
// render) or f3d when available. Remote previews are fetched by the sources
// themselves.
package preview

import (
	"context"
	_ "embed"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"modon/internal/config"
)

//go:embed render_preview.py
var blenderScript []byte

type Renderer struct {
	blender string
	f3d     string
}

// New resolves the configured renderer binaries against PATH.
func New(cfg *config.Config) *Renderer {
	r := &Renderer{}
	if cfg.Preview.Renderer == "none" {
		return r
	}
	if cfg.Preview.Renderer == "" || cfg.Preview.Renderer == "auto" || cfg.Preview.Renderer == "f3d" {
		if p := lookup(cfg.F3DPath); p != "" {
			r.f3d = p
		}
	}
	if cfg.Preview.Renderer == "" || cfg.Preview.Renderer == "auto" || cfg.Preview.Renderer == "blender" {
		if p := lookup(cfg.BlenderPath); p != "" {
			r.blender = p
		}
	}
	return r
}

func lookup(p string) string {
	if p == "" {
		return ""
	}
	if strings.ContainsRune(p, '/') { // explicit path
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
		return ""
	}
	if path, err := exec.LookPath(p); err == nil {
		return path
	}
	return ""
}

// Available reports which renderer will be used.
func (r *Renderer) Available() string {
	switch {
	case r.f3d != "":
		return "f3d"
	case r.blender != "":
		return "blender"
	default:
		return ""
	}
}

var outRe = regexp.MustCompile(`MODON_PREVIEW_DONE:(.+)`)

// Render writes a thumbnail for assetPath to outPNG. Remote formats that no
// renderer can open return an error quickly.
func (r *Renderer) Render(ctx context.Context, assetPath, outPNG string, size int) error {
	kind := r.Available()
	if kind == "" {
		return fmt.Errorf("no preview renderer available (install blender or f3d, or set blender_path/f3d_path)")
	}
	if err := os.MkdirAll(filepath.Dir(outPNG), 0o755); err != nil {
		return err
	}
	cctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	var cmd *exec.Cmd
	if kind == "f3d" {
		args := []string{assetPath, "--output", outPNG, "--resolution", fmt.Sprintf("%d %d", size, size)}
		cmd = exec.CommandContext(cctx, r.f3d, args...)
	} else {
		// The embedded script imports the asset by extension, so Blender
		// always starts empty (passing a mesh as the startup file fails with
		// "File format is not supported" for anything but .blend).
		args := []string{"--background", "--python", scriptPath(), "--", assetPath, outPNG, strconv.Itoa(size)}
		cmd = exec.CommandContext(cctx, r.blender, args...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		msg := strings.TrimSpace(string(out))
		if len(msg) > 400 {
			msg = msg[len(msg)-400:]
		}
		return fmt.Errorf("preview render failed (%s): %v: %s", kind, err, msg)
	}
	if kind == "blender" && !outRe.MatchString(string(out)) {
		return fmt.Errorf("preview render produced no output")
	}
	return nil
}

// scriptPath materializes the embedded script into the cache dir.
func scriptPath() string {
	dir, err := config.CacheDir()
	if err == nil {
		p := filepath.Join(dir, "render_preview.py")
		if err := os.WriteFile(p, blenderScript, 0o644); err == nil {
			return p
		}
	}
	// Fall back to a temp file if the cache dir is unusable.
	tmp := filepath.Join(os.TempDir(), "modon_render_preview.py")
	_ = os.WriteFile(tmp, blenderScript, 0o644)
	return tmp
}
