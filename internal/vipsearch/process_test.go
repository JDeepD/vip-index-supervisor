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
	"testing"
	"time"
)

func TestCommandProcessTreeCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group lifecycle assertion")
	}
	for _, mode := range []string{"cancel", "timeout", "orphan"} {
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
				if res.Err == nil {
					t.Fatalf("unexpected success: %+v", res)
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
		})
	}
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
	cmd := exec.Command(exe, "-test.run=^TestCommandTreeHelper$", "--")
	cmd.Env = append(os.Environ(), "VIP_SUPERVISOR_TREE_ROLE=child")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		os.Exit(2)
	}
	cmd.Process.Release()
	fmt.Println(`{"indexing":true}`)
	if os.Getenv("VIP_SUPERVISOR_TREE_MODE") == "orphan" {
		os.Exit(0)
	}
	time.Sleep(20 * time.Second)
	os.Exit(0)
}
