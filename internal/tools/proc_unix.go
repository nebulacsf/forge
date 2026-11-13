//go:build unix

package tools

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcess puts the shell in its own process group so timeouts and
// cancellations terminate the entire command tree, not just the outer bash.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	cmd.WaitDelay = 2 * time.Second
}
