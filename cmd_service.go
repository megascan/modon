package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"modon/internal/config"
	"modon/internal/embed"
)

// ---- service ----

func cmdService(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: modon service install [--replace] | uninstall | status | print")
	}
	fs := flag.NewFlagSet("service", flag.ContinueOnError)
	replace := fs.Bool("replace", false, "stop a running ad-hoc embed-server first")
	if err := parseFlags(fs, args[1:]); err != nil {
		return err
	}
	switch args[0] {
	case "print":
		unit, err := renderUnit()
		if err != nil {
			return err
		}
		fmt.Print(unit)
		return nil
	case "install":
		return serviceInstall(*replace)
	case "uninstall":
		_ = systemctl("disable", "--now", embed.UnitName)
		p, err := embed.UnitPath()
		if err != nil {
			return err
		}
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		_ = systemctl("daemon-reload")
		fmt.Println("removed", p)
		return nil
	case "status":
		cmd := exec.Command("systemctl", "--user", "status", "--no-pager", embed.UnitName)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		_ = cmd.Run() // non-zero when inactive; the output says why
		return serverHealth()
	default:
		return fmt.Errorf("unknown service command %q (install, uninstall, status, print)", args[0])
	}
}

func renderUnit() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if p, err := filepath.EvalSymlinks(exe); err == nil {
		exe = p
	}
	cfg, err := config.Load()
	if err != nil {
		return "", err
	}
	uv := cfg.Embedding.UVPath
	if uv == "" {
		uv = "uv"
	}
	uvPath, err := exec.LookPath(uv)
	if err != nil {
		return "", fmt.Errorf("the embedding server needs uv (https://docs.astral.sh/uv/): %w", err)
	}
	home, _ := os.UserHomeDir()
	abs, _ := filepath.Abs(uvPath)
	return embed.Unit(embed.UnitOptions{Exe: exe, UVDir: filepath.Dir(abs), Home: home}), nil
}

func systemctl(args ...string) error {
	cmd := exec.Command("systemctl", append([]string{"--user"}, args...)...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	return cmd.Run()
}

// serviceInstall writes the unit without enabling it: clients start it on
// demand and it exits when idle. Units from older versions were enabled at
// login, so that is undone.
func serviceInstall(replace bool) error {
	unit, err := renderUnit()
	if err != nil {
		return err
	}
	p, err := embed.UnitPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(unit), 0o644); err != nil {
		return err
	}
	fmt.Println("wrote", p)
	if err := systemctl("daemon-reload"); err != nil {
		return fmt.Errorf("systemctl --user daemon-reload: %w", err)
	}
	_ = exec.Command("systemctl", "--user", "disable", embed.UnitName).Run()
	cache, err := config.CacheDir()
	if err != nil {
		return err
	}
	if pid, held := embed.LockHolder(embed.ServerLockPath(cache)); held && pid > 0 && !unitOwns(pid) {
		if !replace {
			fmt.Printf("note: an ad-hoc embed-server (pid %d) is running; the unit is used once it exits (or re-run with --replace)\n", pid)
		} else if err := stopPID(pid, 30*time.Second); err != nil {
			return err
		}
	}
	fmt.Println("installed", embed.UnitName, "- it starts when modon needs embeddings and stops when idle; check with: modon service status")
	return nil
}

// unitOwns reports whether pid is the unit's main process.
func unitOwns(pid int) bool {
	out, err := exec.Command("systemctl", "--user", "show", "-p", "MainPID", "--value", embed.UnitName).Output()
	return err == nil && strings.TrimSpace(string(out)) == fmt.Sprint(pid)
}

func stopPID(pid int, wait time.Duration) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := p.Signal(syscall.SIGTERM); err != nil {
		return nil // already gone
	}
	fmt.Printf("stopping ad-hoc embed-server (pid %d)...\n", pid)
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		if p.Signal(syscall.Signal(0)) != nil {
			return nil
		}
		time.Sleep(500 * time.Millisecond)
	}
	return fmt.Errorf("pid %d did not exit within %s", pid, wait)
}

func serverHealth() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	if cfg.Embedding.Endpoint == "" {
		fmt.Println("embedding.endpoint is not set (semantic search off)")
		return nil
	}
	cl := embed.New(cfg.Embedding.Endpoint, cfg.Embedding.Model, cfg.Embedding.Dims, 5*time.Second)
	info, err := cl.Info(context.Background())
	if err != nil {
		fmt.Printf("\nendpoint %s: not answering (%v)\n", cl.Endpoint, err)
		return nil
	}
	fmt.Printf("\nendpoint %s: up, device %s (prefers %s), %s, vision %v, %d OOM recoveries\n",
		cl.Endpoint, info.Device, orDash(info.PreferredDevice), info.Dtype, info.Vision, info.OOMEvents)
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}
