//go:build unix

package manager

import (
	"os/exec"
	"syscall"
)

// Kernels get their own process group, so that Ctrl-C does not reach them
// before go-notebooks has stopped them and child processes die with them.
func setProcGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(cmd *exec.Cmd) {
	syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	cmd.Process.Kill()
}
