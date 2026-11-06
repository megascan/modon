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
	"strings"
	"syscall"
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
	IdleOffload int // seconds idle before the model leaves the GPU, 0 = never
	IdleExit    int // seconds idle before the server shuts down, 0 = never
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
	// Write-then-rename: a concurrent reader never sees a half-written script.
	tmp := fmt.Sprintf("%s.%d.tmp", p, os.Getpid())
	if err := os.WriteFile(tmp, serverScript, 0o644); err != nil {
		return "", err
	}
	return p, os.Rename(tmp, p)
}

// HostPort extracts the bind address from an endpoint URL.
func HostPort(endpoint string) (string, int) {
	host, port := "127.0.0.1", 8178
	if u, err := url.Parse(endpoint); err == nil && u.Host != "" {
		if h := u.Hostname(); h != "" {
			host = h
		}
		if p, err := strconv.Atoi(u.Port()); err == nil {
			port = p
		}
	}
	return host, port
}

// Command builds the `uv run` invocation for the sidecar. uv resolves the
// script's inline dependencies (torch, sentence-transformers) on first run.
// The Python process is tied to the calling process: it gets SIGTERM when
// its parent dies.
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
	host, port := HostPort(o.Endpoint)
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
	if o.IdleOffload > 0 {
		args = append(args, "--idle-offload", strconv.Itoa(o.IdleOffload))
	}
	if o.IdleExit > 0 {
		args = append(args, "--idle-exit", strconv.Itoa(o.IdleExit))
	}
	cmd := exec.CommandContext(ctx, uvPath, args...)
	cmd.Dir = o.CacheDir
	// Fewer fragmentation OOMs on a GPU shared with other applications.
	cmd.Env = append(os.Environ(), "PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True")
	setPdeathsig(cmd)
	// Stop gracefully on cancel; WaitDelay escalates to SIGKILL.
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	return cmd, nil
}

// ServerLockPath is the lock a running `modon embed-server` holds for its
// whole life, so at most one server per cache directory loads the model.
func ServerLockPath(cacheDir string) string { return filepath.Join(cacheDir, "embed-server.lock") }

// ErrServerRunning means another embed server already holds the lock.
var ErrServerRunning = errors.New("another modon embed-server is already running")

// LockHolder reports whether the server lock is held and by which pid.
func LockHolder(path string) (pid int, held bool) {
	release, err := tryLock(path, false)
	if err == nil {
		release()
		return 0, false
	}
	if raw, err := os.ReadFile(path); err == nil {
		pid, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}
	return pid, true
}

// ServeOptions configures a long-running embed server.
type ServeOptions struct {
	Sidecar SidecarOptions
	// Log, when set, receives the sidecar's output (otherwise it inherits
	// this process's stdout/stderr, which systemd sends to the journal).
	Log *os.File
}

// Serve runs the sidecar in the foreground until ctx ends or it exits. It
// refuses to start when another server holds the lock or something already
// answers at the endpoint, so concurrent starters never load the model
// twice.
func Serve(ctx context.Context, c *Client, o ServeOptions) error {
	release, err := tryLock(ServerLockPath(o.Sidecar.CacheDir), true)
	if err != nil {
		if errors.Is(err, errLocked) {
			pid, _ := LockHolder(ServerLockPath(o.Sidecar.CacheDir))
			return fmt.Errorf("%w (pid %d)", ErrServerRunning, pid)
		}
		return err
	}
	defer release()
	if c != nil && c.Ping(ctx) == nil {
		return fmt.Errorf("%w: something already answers at %s", ErrServerRunning, c.Endpoint)
	}
	cmd, err := Command(ctx, o.Sidecar)
	if err != nil {
		return err
	}
	if o.Log != nil {
		cmd.Stdout, cmd.Stderr = o.Log, o.Log
	} else {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	err = cmd.Run()
	if ctx.Err() != nil {
		return nil // stopped on purpose
	}
	return err
}

// EnsureOptions controls how a client brings a missing server up.
type EnsureOptions struct {
	CacheDir string
	LogPath  string        // log for a detached server
	Exe      string        // modon binary for a detached `embed-server` (default: this executable)
	Wait     time.Duration // how long to wait for it to answer; 0 = do not wait
}

// ErrStarting means a server is starting but not answering yet.
var ErrStarting = errors.New("embedding server is starting (loading the model)")

// Ensure makes sure an embedding server is running: it returns at once when
// one answers; otherwise it starts the systemd user unit when installed, or
// spawns a detached `modon embed-server` that outlives this process. The
// server lock keeps concurrent callers from starting duplicates. With Wait
// > 0 it then waits for the server to answer.
func Ensure(ctx context.Context, c *Client, o EnsureOptions) error {
	if c.Healthy(ctx) == nil {
		return nil
	}
	lock := ServerLockPath(o.CacheDir)
	if _, held := LockHolder(lock); !held {
		if err := startServer(ctx, o); err != nil {
			return err
		}
	}
	if o.Wait <= 0 {
		return ErrStarting
	}
	deadline := time.Now().Add(o.Wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(time.Second):
		}
		if c.Ping(ctx) == nil {
			return nil
		}
		if _, held := LockHolder(lock); !held {
			// The server gave up (crashed, or another one won the race and
			// exited): say so instead of waiting out the deadline.
			return fmt.Errorf("embedding server exited during startup (log: %s)", o.LogPath)
		}
	}
	return ErrStarting
}

func startServer(ctx context.Context, o EnsureOptions) error {
	if UnitInstalled() {
		if err := exec.CommandContext(ctx, "systemctl", "--user", "start", "--no-block", UnitName).Run(); err == nil {
			return nil
		}
		// Fall through: no user systemd session (e.g. ssh without lingering).
	}
	exe := o.Exe
	if exe == "" {
		var err error
		if exe, err = os.Executable(); err != nil {
			return err
		}
	}
	return SpawnDetached(exe, []string{"embed-server"}, o.LogPath)
}

// SpawnDetached starts exe in its own session with output appended to
// logPath, so it survives the caller.
func SpawnDetached(exe string, args []string, logPath string) error {
	cmd := exec.Command(exe, args...)
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return err
	}
	defer devnull.Close()
	cmd.Stdin = devnull
	if logPath != "" {
		f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return err
		}
		defer f.Close()
		cmd.Stdout, cmd.Stderr = f, f
	}
	setDetached(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
