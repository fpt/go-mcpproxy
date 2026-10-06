//go:build unix

package wrapper

import (
	"os/exec"
	"syscall"
)

// killProcessGroup runs the command in its own process group and makes
// cancellation kill the whole group, so that children (e.g. those started
// by make) do not outlive a timeout.
func killProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
}
