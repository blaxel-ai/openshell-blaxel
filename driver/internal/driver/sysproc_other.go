//go:build !linux

package driver

import "os/exec"

// setParentDeathSignal is Linux-only; elsewhere the parent-liveness pipe
// still stops the supervisor when the driver exits.
func setParentDeathSignal(*exec.Cmd) {}
