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
// Process.Kill as the fallback.
func terminateProcessTree(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/T", "/F", "/PID", pid).Run(); err != nil {
		cmd.Process.Kill()
	}
}
