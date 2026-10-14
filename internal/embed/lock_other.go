//go:build !unix

package embed

import (
	"errors"
	"os/exec"
)

var errLocked = errors.New("locked")

// Without flock, rely on the endpoint check alone.
func tryLock(string, bool) (func(), error) { return func() {}, nil }

func setDetached(*exec.Cmd) {}
