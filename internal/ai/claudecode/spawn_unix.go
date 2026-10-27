//go:build !windows

package claudecode

import (
	"os/exec"
	"syscall"
)

// prepare puts the program in its own process group so that ending it ends
// everything it started.
func prepare(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func track(cmd *exec.Cmd) func() {
	pid := cmd.Process.Pid
	return func() { _ = syscall.Kill(-pid, syscall.SIGKILL) }
}
