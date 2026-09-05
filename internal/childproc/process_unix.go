//go:build !windows

package childproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// Configure puts the child in its own process group, so a kill
// reaches the whole `vip -> wp` tree rather than orphaning the grandchild.
func Configure(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// Terminate SIGTERMs the child's process group, then SIGKILLs
// whatever lingers past the grace period. A zero grace skips straight to
// SIGKILL — what a second Ctrl-C asks for: the user has already waited once.
//
// `gone` closes after Wait completes. Output is drained concurrently while
// waiting for graceful shutdown, rather than blocking the child's last writes.
func Terminate(cmd *exec.Cmd, grace time.Duration, gone <-chan struct{}) {
	if cmd.Process == nil {
		return
	}
	pgid := -cmd.Process.Pid
	if grace > 0 {
		syscall.Kill(pgid, syscall.SIGTERM)
		select {
		case <-gone:
			return
		case <-time.After(grace):
		}
	}
	Kill(cmd)
}

// Kill is suitable for exec.Cmd.Cancel. An already-finished tree is not a
// cancellation failure and must be reported using exec's expected sentinel.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
