package embed

import (
	"os/exec"
	"syscall"
)

// setPdeathsig stops the sidecar when modon exits, even on SIGKILL.
func setPdeathsig(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGTERM}
}
