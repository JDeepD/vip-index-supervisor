//go:build windows

package supervise

import (
	"os/exec"
	"strconv"
	"time"
)

func configureProcessGroup(cmd *exec.Cmd) {}

// terminateProcessTree kills the child and its descendants. Windows has no
// process groups in the POSIX sense; taskkill /T walks the tree for us, with
// Process.Kill as the fallback. There is no graceful signal to send, so the
// grace period only decides how long to let an already-exiting tree finish.
func terminateProcessTree(cmd *exec.Cmd, grace time.Duration, gone <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	if grace > 0 {
		select {
		case <-gone:
			return
		case <-time.After(grace):
		}
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run(); err != nil {
		cmd.Process.Kill()
	}
}
