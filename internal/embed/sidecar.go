package embed

import (
	"context"
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"time"
)

//go:embed embed_server.py
var serverScript []byte

// SidecarOptions configures the bundled Python embedding server.
type SidecarOptions struct {
	UV          string // uv binary (default "uv" in PATH)
	CacheDir    string // where the script is materialized
	Endpoint    string // host/port to bind are taken from this URL
	ImageTokens int    // vision token budget per image, 0 = model default (280)
	Device      string // cuda | cpu | mps, "" = auto
	TextOnly    bool
}

// ScriptPath writes the embedded sidecar script into dir and returns it.
func ScriptPath(dir string) (string, error) {
	if dir == "" {
		dir = os.TempDir()
	}
	p := filepath.Join(dir, "embed_server.py")
	if cur, err := os.ReadFile(p); err == nil && string(cur) == string(serverScript) {
		return p, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return p, os.WriteFile(p, serverScript, 0o644)
}

// Command builds the `uv run` invocation for the sidecar. uv resolves the
// script's inline dependencies (torch, sentence-transformers) on first run.
func Command(ctx context.Context, o SidecarOptions) (*exec.Cmd, error) {
	uv := o.UV
	if uv == "" {
		uv = "uv"
	}
	uvPath, err := exec.LookPath(uv)
	if err != nil {
		return nil, fmt.Errorf("the embedding sidecar needs uv (https://docs.astral.sh/uv/): %w", err)
	}
	script, err := ScriptPath(o.CacheDir)
	if err != nil {
		return nil, err
	}
	host, port := "127.0.0.1", 8178
	if u, err := url.Parse(o.Endpoint); err == nil && u.Host != "" {
		if h := u.Hostname(); h != "" {
			host = h
		}
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
	}
	args := []string{"run", "--quiet", "--script", script, "--host", host, "--port", strconv.Itoa(port)}
	if o.ImageTokens > 0 {
		args = append(args, "--image-tokens", strconv.Itoa(o.ImageTokens))
	}
	if o.Device != "" {
		args = append(args, "--device", o.Device)
	}
	if o.TextOnly {
		args = append(args, "--no-vision")
	}
	cmd := exec.CommandContext(ctx, uvPath, args...)
	cmd.Dir = o.CacheDir
	setPdeathsig(cmd)
	return cmd, nil
}

// Start launches the sidecar in the background (stderr goes to logPath) and
// waits up to wait for it to answer. The process dies with modon.
func Start(ctx context.Context, c *Client, o SidecarOptions, logPath string, wait time.Duration) (*exec.Cmd, error) {
	if c.Ping(ctx) == nil {
		return nil, nil // already running (another modon, or user-managed)
	}
	cmd, err := Command(ctx, o)
	if err != nil {
		return nil, err
	}
	if logPath != "" {
		if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
			cmd.Stdout, cmd.Stderr = f, f
		}
	}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		select {
		case err := <-exited:
			return nil, fmt.Errorf("embedding sidecar exited: %v (log: %s)", err, logPath)
		case <-ctx.Done():
			return cmd, ctx.Err()
		case <-time.After(time.Second):
		}
		c.Reset()
		if c.Ping(ctx) == nil {
			return cmd, nil
		}
	}
	return cmd, errors.New("embedding sidecar still starting (first run installs torch and downloads the model)")
}
