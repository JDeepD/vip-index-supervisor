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
//
// `gone` is closed once the child's output pipe reaches EOF, which is the only
// reliable in-flight signal that the tree has exited: cmd.ProcessState stays
// nil until Wait() returns, and Wait() cannot run until the readers finish, so
// polling it here would always burn the full grace period and then SIGKILL a
// process that had already exited.
func terminateProcessTree(cmd *exec.Cmd, grace time.Duration, gone <-chan struct{}) {
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
	syscall.Kill(pgid, syscall.SIGKILL)
}
