//go:build unix

package claude

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcess puts the child in its own process group and makes context
// cancellation kill the whole group, so no node helper processes are orphaned.
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		// Negative pid signals the process group (pgid == pid with Setpgid).
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	// Wait force-closes stdout/stderr copying this long after the process exits,
	// even if a grandchild that escaped the group (setsid) still holds the pipe.
	// This only works because Stdout is a non-*os.File writer (see Stream).
	cmd.WaitDelay = 2 * time.Second
}
