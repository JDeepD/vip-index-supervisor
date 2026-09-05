//go:build windows

package childproc

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func Configure(cmd *exec.Cmd) {}

// Terminate kills the child and its descendants. Windows has no
// process groups in the POSIX sense; taskkill /T walks the tree for us, with
// Process.Kill as the fallback. There is no graceful signal to send, so the
// grace period only decides how long to let an already-exiting tree finish.
func Terminate(cmd *exec.Cmd, grace time.Duration, gone <-chan struct{}) {
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
	Kill(cmd)
}

// Kill makes a bounded best-effort tree kill, falling back to the parent.
func Kill(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return os.ErrProcessDone
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	kill := exec.CommandContext(ctx, "taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid))
	kill.WaitDelay = time.Second
	if err := kill.Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
