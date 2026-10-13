// Package preview renders local thumbnails with Blender (headless Workbench
// render) or f3d when available. Remote previews are fetched by the sources
// themselves.
package preview

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
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

// Job is one thumbnail to render.
type Job struct {
	Asset string
	Out   string
}

var (
	startRe = regexp.MustCompile(`^MODON_PREVIEW_START:(\d+)`)
	errRe   = regexp.MustCompile(`^MODON_PREVIEW_ERROR:(\d+):(.*)`)
)

// RenderBatch renders many thumbnails, calling done once per job with its
// error (nil on success). Blender renders them in one session per batch,
// which removes the per-file startup cost; if Blender crashes or stalls on
// one asset, that job fails and the rest are retried in a fresh session.
// f3d renders one file at a time.
func (r *Renderer) RenderBatch(ctx context.Context, jobs []Job, size int, done func(Job, error)) {
	if r.Available() != "blender" {
		for _, j := range jobs {
			if ctx.Err() != nil {
				return
			}
			done(j, r.Render(ctx, j.Asset, j.Out, size))
		}
		return
	}
	for len(jobs) > 0 && ctx.Err() == nil {
		next := r.blenderBatch(ctx, jobs, size, done)
		if next >= len(jobs) {
			return
		}
		jobs = jobs[next:]
	}
}

// stallTimeout bounds a single asset inside a batch.
const stallTimeout = 2 * time.Minute

// blenderBatch runs one Blender session over jobs and returns how many jobs
// were settled (done called); the caller retries from there.
func (r *Renderer) blenderBatch(ctx context.Context, jobs []Job, size int, done func(Job, error)) int {
	pairs := make([][2]string, len(jobs))
	for i, j := range jobs {
		pairs[i] = [2]string{j.Asset, j.Out}
	}
	f, err := os.CreateTemp("", "modon-preview-*.json")
	if err != nil {
		for _, j := range jobs {
			done(j, err)
		}
		return len(jobs)
	}
	defer os.Remove(f.Name())
	_ = json.NewEncoder(f).Encode(pairs)
	f.Close()

	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(cctx, r.blender, "--background", "--factory-startup",
		"--python", scriptPath(), "--", "--batch", f.Name(), strconv.Itoa(size))
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		for _, j := range jobs {
			done(j, err)
		}
		return len(jobs)
	}
	cmd.Stderr = nil
	if err := cmd.Start(); err != nil {
		for _, j := range jobs {
			done(j, err)
		}
		return len(jobs)
	}

	// Watchdog: kill the session when one asset takes too long.
	progress := make(chan struct{}, 1)
	go func() {
		t := time.NewTimer(stallTimeout)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-progress:
				t.Reset(stallTimeout)
			case <-t.C:
				cancel()
				return
			}
		}
	}()

	settled, current := 0, -1
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		if m := startRe.FindStringSubmatch(line); m != nil {
			current, _ = strconv.Atoi(m[1])
			select {
			case progress <- struct{}{}:
			default:
			}
			continue
		}
		if m := outRe.FindStringSubmatch(line); m != nil && current >= 0 && current < len(jobs) {
			done(jobs[current], nil)
			settled = current + 1
			continue
		}
		if m := errRe.FindStringSubmatch(line); m != nil {
			i, _ := strconv.Atoi(m[1])
			if i >= 0 && i < len(jobs) {
				done(jobs[i], fmt.Errorf("preview render failed (blender): %s", strings.TrimSpace(m[2])))
				settled = i + 1
			}
		}
	}
	werr := cmd.Wait()
	if settled >= len(jobs) || ctx.Err() != nil {
		return settled
	}
	// The session died (crash, stall, OOM) on the job it was working on.
	reason := "blender exited"
	switch {
	case errors.Is(cctx.Err(), context.Canceled) && ctx.Err() == nil:
		reason = fmt.Sprintf("stalled for %s", stallTimeout)
	case werr != nil:
		reason = werr.Error()
	}
	if current >= settled && current < len(jobs) {
		done(jobs[current], fmt.Errorf("preview render failed (blender): %s", reason))
		return current + 1
	}
	// Died before starting any job: fail them all rather than loop.
	for _, j := range jobs[settled:] {
		done(j, fmt.Errorf("preview render failed (blender): %s", reason))
	}
	return len(jobs)
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
