//go:build !unix

package claude

import (
	"os/exec"
	"time"
)

// configureProcess falls back to killing only the direct child.
func configureProcess(cmd *exec.Cmd) {
	cmd.WaitDelay = 2 * time.Second
}
