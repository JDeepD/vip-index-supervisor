//go:build !windows

package supervise

import (
	"os/exec"
	"syscall"
	"time"
)

// configureProcessGroup puts the child in its own process group, so a kill
// reaches the whole `vip -> wp` tree rather than orphaning the grandchild.
func configureProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// terminateProcessTree SIGTERMs the child's process group, then SIGKILLs
// whatever lingers past the grace period. A zero grace skips straight to
// SIGKILL — what a second Ctrl-C asks for: the user has already waited once.
func terminateProcessTree(cmd *exec.Cmd, grace time.Duration) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	if grace > 0 {
		syscall.Kill(pgid, syscall.SIGTERM)
		deadline := time.Now().Add(grace)
		for time.Now().Before(deadline) {
			if cmd.ProcessState != nil {
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	syscall.Kill(pgid, syscall.SIGKILL)
}
