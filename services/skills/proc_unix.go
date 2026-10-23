//go:build unix

package skills

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcess puts git in its own process group and makes cancellation
// kill the whole group (ssh and helpers included).
func configureProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil && err != syscall.ESRCH {
			return err
		}
		return nil
	}
	cmd.WaitDelay = 2 * time.Second
}
