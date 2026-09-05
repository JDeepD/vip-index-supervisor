package vipsearch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestCommandProcessTreeCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group lifecycle assertion")
	}
	for _, mode := range []string{"cancel", "timeout", "orphan", "orphan-closed-pipes", "failed-closed-pipes"} {
		t.Run(mode, func(t *testing.T) {
			dir := t.TempDir()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			t.Setenv("VIP_SUPERVISOR_TREE_ROLE", "parent")
			t.Setenv("VIP_SUPERVISOR_TREE_DIR", dir)
			t.Setenv("VIP_SUPERVISOR_TREE_MODE", mode)
			target := Target{WPCommand: strconv.Quote(exe) + " -test.run=^TestCommandTreeHelper$ --"}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan RunResult, 1)
			timeout := 10 * time.Second
			if mode == "timeout" {
				timeout = 3 * time.Second
			}
			go func() { result <- target.Run(ctx, timeout, "get-indexing-status") }()
			ready := filepath.Join(dir, "child.pid")
			limit := time.Now().Add(5 * time.Second)
			var pid int
			for time.Now().Before(limit) {
				data, _ := os.ReadFile(ready)
				pid, _ = strconv.Atoi(string(data))
				if pid > 0 {
					break
				}
				time.Sleep(10 * time.Millisecond)
			}
			if pid <= 0 {
				cancel()
				t.Fatal("helper child did not start")
			}
			heartbeat := filepath.Join(dir, "heartbeat")
			// On regression, clean up the exact test child rather than leave
			// a failing test's worker running for its full safety timeout.
			t.Cleanup(func() {
				before, _ := os.ReadFile(heartbeat)
				time.Sleep(150 * time.Millisecond)
				after, _ := os.ReadFile(heartbeat)
				if string(before) != string(after) {
					if p, err := os.FindProcess(pid); err == nil {
						_ = p.Kill()
					}
				}
			})
			if mode == "cancel" {
				cancel()
			}
			select {
			case res := <-result:
				if (res.Err == nil) != (mode == "orphan-closed-pipes") {
					t.Fatalf("incorrect exit result: %+v", res)
				}
				if mode == "timeout" && !res.TimedOut {
					t.Fatalf("timeout lost: %+v", res)
				}
				if mode == "orphan" && !errors.Is(res.Err, exec.ErrWaitDelay) {
					t.Fatalf("inherited pipe not detected: %+v", res)
				}
			case <-time.After(6 * time.Second):
				cancel()
				t.Fatal("command did not return after cancellation/pipe expiry")
			}
			before, _ := os.ReadFile(heartbeat)
			time.Sleep(150 * time.Millisecond)
			after, _ := os.ReadFile(heartbeat)
			if string(before) != string(after) {
				t.Fatal("local grandchild kept running after command ended")
			}
			// No heartbeat alone cannot distinguish a dead/reaped process
			// from a zombie. Verify the exact test PIDs leave the process table.
			assertTreeHelperGone(t, pid)
			parent, err := os.ReadFile(filepath.Join(dir, "parent.pid"))
			if err != nil {
				t.Fatal(err)
			}
			parentPID, err := strconv.Atoi(string(parent))
			if err != nil || parentPID <= 1 {
				t.Fatalf("invalid helper PID: %q", parent)
			}
			assertTreeHelperGone(t, parentPID)
		})
	}
}

func assertTreeHelperGone(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var state string
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		out, err := exec.CommandContext(ctx, "ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
		cancel()
		var exitErr *exec.ExitError
		state = strings.TrimSpace(string(out))
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 && state == "" {
			return
		}
		if err != nil {
			t.Fatalf("cannot check helper PID %d: %v", pid, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("helper PID %d still present after cleanup (state %q, Z means zombie)", pid, state)
}

func TestCommandTreeHelper(t *testing.T) {
	role := os.Getenv("VIP_SUPERVISOR_TREE_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("VIP_SUPERVISOR_TREE_DIR")
	if role == "child" {
		if err := os.WriteFile(filepath.Join(dir, "heartbeat"), []byte("ready"), 0600); err != nil {
			os.Exit(2)
		}
		if err := os.WriteFile(filepath.Join(dir, "child.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
			os.Exit(2)
		}
		for i := 0; i < 300; i++ {
			if err := os.WriteFile(filepath.Join(dir, "heartbeat"), []byte(strconv.Itoa(i)), 0600); err != nil {
				os.Exit(2)
			}
			time.Sleep(50 * time.Millisecond)
		}
		os.Exit(0)
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(2)
	}
	if err := os.WriteFile(filepath.Join(dir, "parent.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		os.Exit(2)
	}
	mode := os.Getenv("VIP_SUPERVISOR_TREE_MODE")
	cmd := exec.Command(exe, "-test.run=^TestCommandTreeHelper$", "--")
	cmd.Env = append(os.Environ(), "VIP_SUPERVISOR_TREE_ROLE=child")
	if mode != "orphan-closed-pipes" && mode != "failed-closed-pipes" {
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	}
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	cmd.Process.Release()
	if mode == "orphan-closed-pipes" || mode == "failed-closed-pipes" {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(filepath.Join(dir, "child.pid")); err == nil {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	fmt.Println(`{"indexing":true}`)
	if mode == "failed-closed-pipes" {
		os.Exit(7)
	}
	if mode == "orphan" || mode == "orphan-closed-pipes" {
		os.Exit(0)
	}
	time.Sleep(20 * time.Second)
	os.Exit(0)
}
