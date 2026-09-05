//go:build !windows

package childproc

import (
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Both helpers are direct children of the test, allowing the test to reap
// every fixture itself. The member shares the leader's group exactly as an
// ordinary grandchild would, but ignores TERM and holds no output pipes.
type groupFixture struct {
	process      *Process
	leader       *exec.Cmd
	member       *exec.Cmd
	waited       <-chan error
	memberWaited <-chan error
	dir          string
}

func newGroupFixture(t *testing.T, exitCode int) groupFixture {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := func(role string) *exec.Cmd {
		cmd := exec.Command(exe, "-test.run=^TestProcessHelper$", "--")
		cmd.Env = append(os.Environ(), "VIP_CHILDPROC_TEST_ROLE="+role,
			"VIP_CHILDPROC_TEST_DIR="+dir, "VIP_CHILDPROC_TEST_EXIT="+strconv.Itoa(exitCode),
			"GORACE=atexit_sleep_ms=0")
		return cmd
	}
	leader := command("leader")
	p := New(leader)
	if err := p.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- p.Wait() }()
	t.Cleanup(func() { _ = p.Kill(); _ = p.Wait() })
	waitForFile(t, filepath.Join(dir, "leader.ready"))
	member := command("member")
	member.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: leader.Process.Pid}
	if err := member.Start(); err != nil {
		t.Fatal(err)
	}
	memberWaited := make(chan error, 1)
	memberDone := make(chan struct{})
	go func() { memberWaited <- member.Wait(); close(memberDone) }()
	t.Cleanup(func() { _ = member.Process.Kill(); <-memberDone })
	waitForFile(t, filepath.Join(dir, "member.ready"))
	return groupFixture{p, leader, member, waited, memberWaited, dir}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("helper did not create %s", path)
}

func waitForExit(t *testing.T, waited <-chan error) error {
	t.Helper()
	select {
	case err := <-waited:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("helper did not exit/reap")
		return nil
	}
}

func assertReaped(t *testing.T, pid int) {
	t.Helper()
	var status syscall.WaitStatus
	if got, err := syscall.Wait4(pid, &status, syscall.WNOHANG, nil); !errors.Is(err, syscall.ECHILD) {
		t.Fatalf("PID %d was not already reaped: wait4=%d, %v", pid, got, err)
	}
}

func TestWaitCleansGroupOnEveryExit(t *testing.T) {
	for _, code := range []int{0, 7} {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			f := newGroupFixture(t, code)
			if err := os.WriteFile(filepath.Join(f.dir, "exit"), nil, 0600); err != nil {
				t.Fatal(err)
			}
			err := waitForExit(t, f.waited)
			if (err == nil) != (code == 0) {
				t.Fatalf("lost exit result: %v", err)
			}
			if err := waitForExit(t, f.memberWaited); err == nil {
				t.Fatal("lingering helper was not killed")
			}
			assertReaped(t, f.leader.Process.Pid)
			assertReaped(t, f.member.Process.Pid)
			if err := f.process.Kill(); !errors.Is(err, os.ErrProcessDone) {
				t.Fatalf("finished process signalled: %v", err)
			}
		})
	}
}

func TestGraceSurvivesLeaderExit(t *testing.T) {
	f := newGroupFixture(t, 0)
	started := time.Now()
	terminated := make(chan error, 1)
	go func() { terminated <- f.process.Terminate(500 * time.Millisecond) }()
	waitForFile(t, filepath.Join(f.dir, "terminated"))
	select {
	case <-f.process.Done():
		t.Fatal("leader exit skipped group grace and cleanup")
	case <-f.memberWaited:
		t.Fatal("member killed before its grace period")
	default:
	}
	_ = waitForExit(t, terminated)
	_ = waitForExit(t, f.waited)
	if elapsed := time.Since(started); elapsed < 450*time.Millisecond {
		t.Fatalf("grace ended early: %s", elapsed)
	}
	if err := waitForExit(t, f.memberWaited); err == nil {
		t.Fatal("TERM-ignoring helper survived")
	}
	assertReaped(t, f.leader.Process.Pid)
	assertReaped(t, f.member.Process.Pid)
}

func TestForceOverridesGraceAndWaitIsShared(t *testing.T) {
	f := newGroupFixture(t, 0)
	terminated := make(chan error, 1)
	go func() { terminated <- f.process.Terminate(20 * time.Second) }()
	waitForFile(t, filepath.Join(f.dir, "terminated"))
	var callers sync.WaitGroup
	for range 10 {
		callers.Go(func() { _ = f.process.Kill(); _ = f.process.Wait() })
	}
	_ = waitForExit(t, f.waited)
	_ = waitForExit(t, terminated)
	_ = waitForExit(t, f.memberWaited)
	callers.Wait()
	assertReaped(t, f.leader.Process.Pid)
	assertReaped(t, f.member.Process.Pid)
}

func TestGroupCleanupDoesNotSignalOtherCommands(t *testing.T) {
	a, b := newGroupFixture(t, 0), newGroupFixture(t, 0)
	_ = a.process.Kill()
	_ = waitForExit(t, a.waited)
	_ = waitForExit(t, a.memberWaited)
	select {
	case <-b.waited:
		t.Fatal("unrelated leader was stopped")
	case <-b.memberWaited:
		t.Fatal("unrelated group member was stopped")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestProcessStartFailureAndNoStart(t *testing.T) {
	for _, start := range []bool{false, true} {
		p := New(exec.Command(filepath.Join(t.TempDir(), "does-not-exist")))
		if start && p.Start() == nil {
			t.Fatal("missing binary started")
		}
		if p.Wait() == nil || p.Wait() == nil {
			t.Fatal("missing/unstarted command succeeded")
		}
		if p.Start() == nil {
			t.Fatal("process restarted after Wait")
		}
		if !errors.Is(p.Kill(), os.ErrProcessDone) || !errors.Is(p.Terminate(time.Second), os.ErrProcessDone) {
			t.Fatal("invalid process was signalled")
		}
		select {
		case <-p.Done():
		default:
			t.Fatal("Done was not closed")
		}
	}
}

func TestConfigureOwnsPrivateGroupAndPreservesAttributes(t *testing.T) {
	attr := &syscall.SysProcAttr{Pgid: 123, Setctty: true}
	cmd := exec.Command("unused")
	cmd.SysProcAttr = attr
	_ = New(cmd)
	if !cmd.SysProcAttr.Setpgid || cmd.SysProcAttr.Pgid != 0 || !cmd.SysProcAttr.Setctty {
		t.Fatalf("incorrect attributes: %+v", cmd.SysProcAttr)
	}
	if attr.Setpgid || attr.Pgid != 123 {
		t.Fatal("caller's attributes mutated")
	}
}

func TestProcessHelper(t *testing.T) {
	role := os.Getenv("VIP_CHILDPROC_TEST_ROLE")
	if role == "" {
		return
	}
	dir := os.Getenv("VIP_CHILDPROC_TEST_DIR")
	signals := make(chan os.Signal, 1)
	if role == "member" {
		signal.Ignore(syscall.SIGTERM)
	} else {
		signal.Notify(signals, syscall.SIGTERM)
	}
	if err := os.WriteFile(filepath.Join(dir, role+".ready"), nil, 0600); err != nil {
		os.Exit(2)
	}
	deadline := time.After(25 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-signals:
			if err := os.WriteFile(filepath.Join(dir, "terminated"), nil, 0600); err != nil {
				os.Exit(2)
			}
			os.Exit(0)
		case <-deadline:
			os.Exit(3)
		case <-ticker.C:
			if role == "leader" {
				if _, err := os.Stat(filepath.Join(dir, "exit")); err == nil {
					code, _ := strconv.Atoi(os.Getenv("VIP_CHILDPROC_TEST_EXIT"))
					os.Exit(code)
				}
			}
		}
	}
}
