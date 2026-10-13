//go:build !linux

package embed

import "os/exec"

func setPdeathsig(*exec.Cmd) {}
