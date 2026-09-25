package driver

import (
	"os/exec"
	"syscall"
)

// setParentDeathSignal kills the supervisor if the driver dies, like the VM
// driver's PR_SET_PDEATHSIG.
func setParentDeathSignal(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
}
