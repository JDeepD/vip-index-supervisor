//go:build !windows

package childproc

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// Use a private local process group. Descendants normally inherit it, but
// a process that deliberately detaches into another group is outside it.
func configure(cmd *exec.Cmd) {
	attr := syscall.SysProcAttr{}
	if cmd.SysProcAttr != nil {
		attr = *cmd.SysProcAttr
	}
	attr.Setpgid, attr.Pgid = true, 0
	cmd.SysProcAttr = &attr
}

func terminateSignal(cmd *exec.Cmd) error { return signalGroup(cmd, syscall.SIGTERM) }
func kill(cmd *exec.Cmd) error            { return signalGroup(cmd, syscall.SIGKILL) }
func cleanup(cmd *exec.Cmd) error         { return kill(cmd) }

func groupExists(cmd *exec.Cmd) bool {
	err := signalGroup(cmd, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func signalGroup(cmd *exec.Cmd, signal syscall.Signal) error {
	if cmd.Process == nil || cmd.Process.Pid <= 1 {
		return os.ErrProcessDone
	}
	// Never signal an arbitrary/shared group or the supervisor's own group.
	if cmd.SysProcAttr == nil || !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 {
		return os.ErrInvalid
	}
	err := syscall.Kill(-cmd.Process.Pid, signal)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}
