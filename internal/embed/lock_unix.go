//go:build unix

package embed

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

var errLocked = errors.New("locked")

// tryLock takes an exclusive, non-blocking flock on path. With writePID the
// holder's pid is written into the file for diagnostics. The lock lives as
// long as the returned release func is not called (or the process exits).
func tryLock(path string, writePID bool) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, errLocked
		}
		return nil, err
	}
	if writePID {
		_ = f.Truncate(0)
		_, _ = f.WriteAt([]byte(fmt.Sprintf("%d\n", os.Getpid())), 0)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}, nil
}

// setDetached puts a spawned server in its own session so terminal hangups
// and the caller's exit do not reach it.
func setDetached(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
