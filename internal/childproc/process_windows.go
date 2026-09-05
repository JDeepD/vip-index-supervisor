//go:build windows

package childproc

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"time"
)

func configure(cmd *exec.Cmd) {}

// Windows has no POSIX graceful group signal. taskkill is only safe while
// the parent is owned: after Wait, its PID may have been reused and its
// orphaned descendants are no longer discoverable through taskkill /T.
func terminateSignal(cmd *exec.Cmd) error { return nil }
func groupExists(cmd *exec.Cmd) bool      { return false }
func cleanup(cmd *exec.Cmd) error         { return nil }

// Kill makes a bounded best-effort tree kill, falling back to the parent.
func kill(cmd *exec.Cmd) error {
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
