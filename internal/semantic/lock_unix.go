//go:build unix

package semantic

import (
	"os"
	"syscall"
)

// tryLock takes an exclusive, non-blocking lock on path. It returns a
// release func, or nil when another process holds the lock.
func tryLock(path string) func() {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return func() {} // cannot lock: do not block embedding
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		f.Close()
	}
}
