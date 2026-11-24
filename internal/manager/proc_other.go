//go:build !unix

package manager

import "os/exec"

func setProcGroup(*exec.Cmd) {}

func killGroup(cmd *exec.Cmd) { cmd.Process.Kill() }
