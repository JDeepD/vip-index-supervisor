package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

type fakeSearch struct {
	rows                      []vipsearch.IndexVersion
	statuses                  []*vipsearch.IndexingStatus
	add                       vipsearch.RunResult
	clears, adds, activations int
	statusCalls, syncClears   int
	onStatus                  func()
	onClear                   func()
	onSyncClear               func()
	clearResult, syncResult   *vipsearch.RunResult
}

func (f *fakeSearch) Versions(context.Context, string) []vipsearch.IndexVersion { return f.rows }
func (f *fakeSearch) Status(context.Context) *vipsearch.IndexingStatus {
	f.statusCalls++
	if f.onStatus != nil {
		f.onStatus()
	}
	if len(f.statuses) == 0 {
		return nil
	}
	st := f.statuses[0]
	if len(f.statuses) > 1 {
		f.statuses = f.statuses[1:]
	}
	return st
}
func (f *fakeSearch) AddVersion(context.Context, string) vipsearch.RunResult { f.adds++; return f.add }
func (f *fakeSearch) ActivateVersion(context.Context, string, int) vipsearch.RunResult {
	f.activations++
	return vipsearch.RunResult{Output: "Success: Activated"}
}
func (f *fakeSearch) ClearIndexLock(context.Context) vipsearch.RunResult {
	f.clears++
	if f.onClear != nil {
		f.onClear()
	}
	if f.clearResult != nil {
		return *f.clearResult
	}
	return vipsearch.RunResult{Output: "Success: Index cleared."}
}
func (f *fakeSearch) ClearSyncRecordGuarded(ctx context.Context, guard func(context.Context) error) vipsearch.RunResult {
	for range 6 {
		if err := guard(ctx); err != nil {
			return vipsearch.RunResult{Err: err}
		}
		f.syncClears++
		if f.onSyncClear != nil {
			f.onSyncClear()
		}
		if f.syncResult != nil && f.syncResult.Failed() {
			return *f.syncResult
		}
	}
	return vipsearch.RunResult{Output: "Success: Deleted."}
}
func (f *fakeSearch) StopIndexing(context.Context) vipsearch.RunResult {
	return vipsearch.RunResult{Output: "Success: Stop requested."}
}
func (f *fakeSearch) LastResult() vipsearch.RunResult {
	return vipsearch.RunResult{Output: "no versions"}
}

func testSupervisor(t *testing.T) *Supervisor {
	t.Helper()
	s := New(Config{StateDir: t.TempDir(), Indexables: []string{"post"}})
	s.client = &fakeSearch{rows: []vipsearch.IndexVersion{{Number: 1, Active: true, Documents: 1000}, {Number: 2, Documents: 1000}}, statuses: []*vipsearch.IndexingStatus{{Indexing: false}}}
	s.wait = func(ctx context.Context, _ time.Duration) bool { return ctx.Err() == nil && !s.stopRequested() }
	s.beginPhase(0)
	return s
}

func TestCheckpointsAreScopedAndMonotonic(t *testing.T) {
	s := testSupervisor(t)
	for _, line := range []string{"Processed 300/1000. Last Object ID: 700", "Processed 600/1000. Last Object ID: 900", "Processed 900/1000. Last Object ID: 800"} {
		var o attemptOutcome
		s.consumeLine("post", 1, line, &o)
		if o.fatal != "" {
			t.Fatal(o.fatal)
		}
	}
	if got := s.store.ReadCheckpoint("post", 1); got != 700 {
		t.Fatalf("checkpoint moved backwards: %d", got)
	}
	if s.lastObjectID() != 700 {
		t.Fatalf("UI ID lost low-water mark: %d", s.lastObjectID())
	}
	if err := s.store.WriteCheckpoint("post", 0, 100); err != nil {
		t.Fatal(err)
	}
	if got := s.resolveStartCheckpoint(context.Background(), "post", 2); got != 0 {
		t.Fatalf("foreign checkpoint reused: %d", got)
	}
	a := checkpointStore{dir: t.TempDir(), postTypes: "news,page"}
	b := checkpointStore{dir: a.dir, postTypes: "news_page"}
	if a.checkpointPath("post", 1) == b.checkpointPath("post", 1) {
		t.Fatal("different filters share checkpoint")
	}
	if a.versionPath("post") == b.versionPath("post") {
		t.Fatal("different filters share version pin")
	}
}

func TestCheckpointFailureIsFatal(t *testing.T) {
	s := testSupervisor(t)
	s.store.dir = filepath.Join(s.cfg.StateDir, "missing", "directory")
	var o attemptOutcome
	s.consumeLine("post", 1, "Processed 300/1000. Last Object ID: 700", &o)
	if !strings.Contains(o.fatal, "persist checkpoint") {
		t.Fatalf("write error ignored: %+v", o)
	}
}

func TestSetupDoesNotReuseOldCheckpointAndKeepsSetupAfterLockRefusal(t *testing.T) {
	s := testSupervisor(t)
	s.cfg.Strategy = StrategySetup
	if err := s.store.WriteCheckpoint("post", 1, 100); err != nil {
		t.Fatal(err)
	}
	var attempts [][]string
	s.attempt = func(ctx context.Context, indexable string, version int, args []string) attemptOutcome {
		attempts = append(attempts, append([]string(nil), args...))
		if len(attempts) < 3 {
			return attemptOutcome{lockError: true}
		}
		s.RequestStop(false)
		return attemptOutcome{}
	}
	s.runPhase(context.Background(), "post")
	if len(attempts) != 3 {
		t.Fatalf("attempts: %v", attempts)
	}
	for _, args := range attempts {
		if !containsArg(args, "--setup") || containsArg(args, "--upper-limit-object-id=100") {
			t.Fatalf("unsafe setup args: %v", args)
		}
	}
}

func TestProgressResetsLockSequence(t *testing.T) {
	s := testSupervisor(t)
	var waits []time.Duration
	s.wait = func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }
	i := 0
	s.attempt = func(_ context.Context, indexable string, version int, _ []string) attemptOutcome {
		i++
		switch i {
		case 1, 2, 4:
			return attemptOutcome{lockError: true}
		case 3:
			o := attemptOutcome{lockError: true}
			s.consumeLine(indexable, version, "Processed 300/1000. Last Object ID: 700", &o)
			return o
		default:
			s.RequestStop(false)
			return attemptOutcome{}
		}
	}
	s.runPhase(context.Background(), "post")
	var want []time.Duration
	for range 4 {
		want = append(want, syncFreezeProbeDelay, s.cfg.BackoffBase)
	}
	if !reflect.DeepEqual(waits, want) {
		t.Fatalf("lock errors accumulated across real progress: %v", waits)
	}
	if s.client.(*fakeSearch).clears != 4 {
		t.Fatal("idle lock refusals were not recovered individually")
	}
}

func TestThreeProgressingLockFailuresResumeFourthAttempt(t *testing.T) {
	s := testSupervisor(t)
	s.cfg.Strategy = StrategySetup
	ids := []int64{3708138, 3329124, 2704733}
	var waits []time.Duration
	s.wait = func(_ context.Context, d time.Duration) bool { waits = append(waits, d); return true }
	i := 0
	s.attempt = func(_ context.Context, indexable string, version int, args []string) attemptOutcome {
		if i == 0 {
			if !containsArg(args, "--setup") {
				t.Fatal("first attempt did not set up")
			}
		} else if containsArg(args, "--setup") || !containsArg(args, "--upper-limit-object-id="+strconv.FormatInt(ids[i-1], 10)) {
			t.Fatalf("resume must retain progress without rebuilding: %v", args)
		}
		if i == len(ids) {
			i++
			return attemptOutcome{success: true, indexed: 3872018}
		}
		o := attemptOutcome{exitErr: errors.New("test CLI failed")}
		s.consumeLine(indexable, version, fmt.Sprintf("Processed 300/1000. Last Object ID: %d", ids[i]), &o)
		s.consumeLine(indexable, version, "Error: An index is already occurring. Try again later.", &o)
		if !o.lockError || !o.progressed {
			t.Fatalf("fixture did not reproduce progressing lock failure: %+v", o)
		}
		i++
		return o
	}
	if !s.runPhase(context.Background(), "post") || i != 4 {
		t.Fatalf("three progressing failures prevented fourth resume: %d attempts", i)
	}
	var want []time.Duration
	for range 3 {
		want = append(want, syncFreezeProbeDelay, s.cfg.BackoffBase)
	}
	if !reflect.DeepEqual(waits, want) || s.client.(*fakeSearch).clears != 3 {
		t.Fatalf("progressing attempts treated as persistent startup lock: waits=%v, clears=%d", waits, s.client.(*fakeSearch).clears)
	}
}

func TestBlockingSyncNeverClearedFromInactivityAlone(t *testing.T) {
	for _, tc := range []struct {
		name      string
		statuses  []*vipsearch.IndexingStatus
		wantClear bool
	}{
		{"unknown", nil, false},
		{"idle", []*vipsearch.IndexingStatus{{Indexing: false}}, true},
		{"unchanged", []*vipsearch.IndexingStatus{{Indexing: true, Method: "cli", ItemsIndexed: 600}}, false},
		{"advancing", []*vipsearch.IndexingStatus{{Indexing: true, ItemsIndexed: 600}, {Indexing: true, ItemsIndexed: 900}}, false},
		{"finished during probe", []*vipsearch.IndexingStatus{{Indexing: true}, {Indexing: false}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testSupervisor(t)
			f := s.client.(*fakeSearch)
			f.statuses = tc.statuses
			cleared := s.recoverCycle(context.Background(), "post", 1, true) == recoveryReady
			if cleared != tc.wantClear || (f.clears > 0) != tc.wantClear {
				t.Fatalf("cleared=%v calls=%d", cleared, f.clears)
			}
		})
	}
}

func TestSyncFingerprintDoesNotHideProgress(t *testing.T) {
	a := vipsearch.IndexingStatus{Indexing: true, ItemsIndexed: 900, CurrentSync: &vipsearch.SyncItem{Synced: 100}}
	b := a
	b.CurrentSync = &vipsearch.SyncItem{Synced: 200}
	if syncFingerprint(&a) == syncFingerprint(&b) {
		t.Fatal("smaller counter movement masked")
	}
	b = a
	b.StartDateTime = "different sync"
	if syncFingerprint(&a) == syncFingerprint(&b) {
		t.Fatal("worker identity masked")
	}
}

func TestVersionSafety(t *testing.T) {
	ctx := context.Background()
	t.Run("active and missing pins", func(t *testing.T) {
		for _, pin := range []int{1, 99} {
			s := testSupervisor(t)
			s.cfg.Strategy = StrategyNewVersion
			if err := s.store.PinVersion("post", pin); err != nil {
				t.Fatal(err)
			}
			if _, err := s.resolveVersion(ctx, "post"); err == nil {
				t.Fatalf("unsafe pin %d accepted", pin)
			}
		}
	})
	t.Run("valid inactive pin", func(t *testing.T) {
		s := testSupervisor(t)
		s.cfg.Strategy = StrategyNewVersion
		if err := s.store.PinVersion("post", 2); err != nil {
			t.Fatal(err)
		}
		if got, err := s.resolveVersion(ctx, "post"); err != nil || got != 2 {
			t.Fatalf("%d %v", got, err)
		}
	})
	t.Run("failed add never reuses inactive slot", func(t *testing.T) {
		s := testSupervisor(t)
		s.cfg.Strategy = StrategyNewVersion
		s.client.(*fakeSearch).add = vipsearch.RunResult{Output: "Error: limit reached", Err: errors.New("exit 1")}
		if _, err := s.resolveVersion(ctx, "post"); err == nil {
			t.Fatal("failed add fell back to unrelated version")
		}
		if s.store.PinnedVersion("post") != 0 {
			t.Fatal("unrelated version pinned")
		}
	})
	t.Run("new slot discards old checkpoint", func(t *testing.T) {
		s := testSupervisor(t)
		if err := s.store.WriteCheckpoint("post", 2, 100); err != nil {
			t.Fatal(err)
		}
		s.client.(*fakeSearch).add = vipsearch.RunResult{Output: "Warning: acf [trace]\nSuccess: Registered and created new index version 2"}
		if version, err := s.createVersion(ctx, "post"); err != nil || version != 2 {
			t.Fatalf("%d %v", version, err)
		}
		if s.store.ReadCheckpoint("post", 2) != 0 {
			t.Fatal("empty version inherited stale progress")
		}
	})
	t.Run("unknown previous count", func(t *testing.T) {
		s := testSupervisor(t)
		s.client.(*fakeSearch).rows[0].Documents = -1
		if ok, _ := s.checkVersionCounts(ctx, "post", 2); ok {
			t.Fatal("unknown count passed safety check")
		}
	})
	t.Run("missing active version", func(t *testing.T) {
		s := testSupervisor(t)
		s.client.(*fakeSearch).rows = []vipsearch.IndexVersion{{Number: 2, Documents: 1}}
		if ok, _ := s.checkVersionCounts(ctx, "post", 2); ok {
			t.Fatal("partial list bypassed activation safety check")
		}
	})
	t.Run("partial build never autoactivates", func(t *testing.T) {
		s := testSupervisor(t)
		s.cfg.PostTypes = "news"
		if s.activateVersion(ctx, "post", 2) || s.client.(*fakeSearch).activations != 0 {
			t.Fatal("partial build activated")
		}
	})
	t.Run("acknowledgement without activation", func(t *testing.T) {
		s := testSupervisor(t)
		if err := s.store.PinVersion("post", 2); err != nil {
			t.Fatal(err)
		}
		if s.activateVersion(ctx, "post", 2) {
			t.Fatal("unverified activation accepted")
		}
		if s.store.PinnedVersion("post") != 2 {
			t.Fatal("pin removed before confirmation")
		}
	})
}

func TestRunStopAndCancellation(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		s := testSupervisor(t)
		ctx, cancel := context.WithCancel(context.Background())
		if cancelled {
			cancel()
		} else {
			s.attempt = func(context.Context, string, int, []string) attemptOutcome {
				s.RequestStop(false)
				return attemptOutcome{}
			}
		}
		s.Run(ctx)
		cancel()
		code := -1
		for event := range s.Events() {
			if done, ok := event.(DoneEvent); ok {
				code = done.ExitCode
			}
		}
		if code != 130 {
			t.Fatalf("stop reported as phase failure: %d", code)
		}
	}
	s := testSupervisor(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s.sleep(ctx, 0) {
		t.Fatal("zero delay ignored cancellation")
	}
}

func TestConfigValidationAndLocalStateScope(t *testing.T) {
	for _, cfg := range []Config{
		{ResumeFrom: 50, Strategy: StrategySetup}, {ResumeFrom: 50, Indexables: []string{"post", "user"}},
		{Indexables: []string{"../post"}}, {Indexables: []string{"post", "post"}}, {MaxDuration: -time.Second},
		{Strategy: StrategyIntoVersion},
		{AggressiveRecovery: true, IgnoreLock: true},
	} {
		cfg.Normalize()
		if cfg.Validate() == nil {
			t.Fatalf("bad config accepted: %+v", cfg)
		}
	}
	a := Config{Target: vipsearch.Target{WPCommand: "wp --path=/srv/a"}}
	b := Config{Target: vipsearch.Target{WPCommand: "wp --path=/srv/b"}}
	a.Normalize()
	b.Normalize()
	if a.StateDir == b.StateDir {
		t.Fatal("direct targets share checkpoint state")
	}
}

func containsArg(args []string, want string) bool {
	for _, arg := range args {
		if arg == want {
			return true
		}
	}
	return false
}

func TestAttemptCLIOutputAndExit(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		success bool
		last    int64
	}{
		{"noisy", true, 700}, {"long-warning", true, 700}, {"quoted-markers", false, 0},
		{"success-then-fail", false, 700}, {"success-then-error", false, 700},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			s := helperSupervisor(t, tc.mode)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			o := s.runAttempt(ctx, "post", 1, []string{"index"})
			if o.success != tc.success {
				t.Fatalf("outcome %+v", o)
			}
			if got := s.store.ReadCheckpoint("post", 1); got != tc.last {
				t.Fatalf("checkpoint %d, want %d", got, tc.last)
			}
			if tc.mode == "quoted-markers" && (o.lockError || o.fatal != "") {
				t.Fatalf("trace became command signal: %+v", o)
			}
			if tc.mode == "long-warning" {
				data, err := os.ReadFile(o.logPath)
				if err != nil || len(data) < 2*1024*1024 {
					t.Fatalf("raw warning lost: %d %v", len(data), err)
				}
			}
		})
	}
}

func TestGracefulStopDrainsFinalCheckpoint(t *testing.T) {
	s := helperSupervisor(t, "graceful")
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	done := make(chan attemptOutcome, 1)
	go func() { done <- s.runAttempt(ctx, "post", 1, []string{"index"}) }()
	limit := time.Now().Add(3 * time.Second)
	for s.store.ReadCheckpoint("post", 1) == 0 && time.Now().Before(limit) {
		time.Sleep(10 * time.Millisecond)
	}
	s.RequestStop(false)
	select {
	case o := <-done:
		if !o.killed || o.success {
			t.Fatalf("%+v", o)
		}
		if got := s.store.ReadCheckpoint("post", 1); got != 400 {
			t.Fatalf("lost final checkpoint during graceful stop: %d", got)
		}
	case <-ctx.Done():
		s.RequestStop(true)
		t.Fatalf("stop blocked while child wrote output; checkpoint=%d", s.store.ReadCheckpoint("post", 1))
	}
}

func TestClosedStdoutDoesNotBlockCancellation(t *testing.T) {
	s := helperSupervisor(t, "closed-stdout")
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	o := s.runAttempt(ctx, "post", 1, []string{"index"})
	if !o.killed || !o.deadline || time.Since(started) > 3*time.Second {
		t.Fatalf("cancellation not observed: %+v", o)
	}
}

func TestInheritedPipeCannotHangAttempt(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix process-group lifecycle test")
	}
	s := helperSupervisor(t, "inherited-pipe")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	o := s.runAttempt(ctx, "post", 1, []string{"index"})
	if o.success || !errors.Is(o.exitErr, exec.ErrWaitDelay) || time.Since(started) > 4*time.Second {
		t.Fatalf("inherited pipe was not bounded: %+v", o)
	}
}

func helperSupervisor(t *testing.T, mode string) *Supervisor {
	t.Helper()
	s := testSupervisor(t)
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s.cfg.Target = vipsearch.Target{WPCommand: strconv.Quote(exe) + " -test.run=^TestIndexHelper$ --"}
	t.Setenv("VIP_SUPERVISOR_INDEX_HELPER", mode)
	t.Setenv("VIP_SUPERVISOR_INDEX_DIR", s.cfg.StateDir)
	if err := os.MkdirAll(filepath.Join(s.cfg.StateDir, "logs"), 0755); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIndexHelper(t *testing.T) {
	mode := os.Getenv("VIP_SUPERVISOR_INDEX_HELPER")
	if mode == "" {
		return
	}
	if mode == "pipe-holder" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if mode == "inherited-pipe" {
		exe, err := os.Executable()
		if err != nil {
			os.Exit(2)
		}
		cmd := exec.Command(exe, "-test.run=^TestIndexHelper$", "--")
		cmd.Env = append(os.Environ(), "VIP_SUPERVISOR_INDEX_HELPER=pipe-holder")
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		if err := cmd.Start(); err != nil {
			os.Exit(2)
		}
		cmd.Process.Release()
	}
	for _, arg := range os.Args {
		if arg == "stop-indexing" {
			if err := os.WriteFile(filepath.Join(os.Getenv("VIP_SUPERVISOR_INDEX_DIR"), "stop-request"), nil, 0600); err != nil {
				os.Exit(2)
			}
			fmt.Println("Success: Stop requested.")
			os.Exit(0)
		}
	}
	if mode == "closed-stdout" {
		os.Stdout.Close()
		os.Stderr.Close()
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	if mode == "quoted-markers" {
		fmt.Println(`Warning: acf on line 403: an index is already occurring`)
		fmt.Println(`#0 call("Success: Done!")`)
		os.Exit(0)
	}
	if mode == "long-warning" {
		fmt.Println("Warning: " + strings.Repeat("x", 2*1024*1024))
	}
	fmt.Println("Warning: acf was called incorrectly in /plugins/acf.php on line 403\nStack trace:\n#0 callback([broken])\n#1 {main}")
	fmt.Println("Processed 300/1000. Last Object ID: 700")
	if mode == "memory" {
		fmt.Println("\x1b[32mMemory Usage:\x1b[0m 171.99mb (Peak: 173.43mb)")
		fmt.Println(`Warning: ACF using it wrong`)
		fmt.Println(`#0 callback("Memory Usage: 999mb")`)
		fmt.Println("Memory Usage: 176.08mb (Peak: 177.58mb)")
	}
	if mode == "graceful" {
		for i := 0; i < 500; i++ {
			if _, err := os.Stat(filepath.Join(os.Getenv("VIP_SUPERVISOR_INDEX_DIR"), "stop-request")); err == nil {
				fmt.Println(strings.Repeat("Warning: shutdown diagnostic\n", 10000))
				fmt.Println("Processed 600/1000. Last Object ID: 400")
				os.Exit(0)
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(3)
	}
	fmt.Print("\x1b[32mSuccess:\x1b[39m Done!")
	if mode == "success-then-fail" {
		os.Exit(7)
	}
	if mode == "success-then-error" {
		fmt.Println("\nError: final upload failed")
	}
	os.Exit(0)
}
