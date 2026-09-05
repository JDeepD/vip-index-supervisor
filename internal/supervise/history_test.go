package supervise

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/notify"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func failedSavedRun(t *testing.T, strategy Strategy) (*Supervisor, RunRecord) {
	t.Helper()
	s := testSupervisor(t)
	s.cfg.Strategy = strategy
	s.cfg.PostTypes = "post,breaking-news"
	s.store.postTypes = s.cfg.PostTypes
	if strategy == StrategyNewVersion {
		if err := s.store.PinVersion("post", 2); err != nil {
			t.Fatal(err)
		}
	}
	s.attempt = func(ctx context.Context, name string, version int, args []string) attemptOutcome {
		if err := s.applyProgress(name, version, vipsearch.Progress{Done: 300, Total: 1000, LastObjectID: 700, IndexedCount: -1}); err != nil {
			t.Fatal(err)
		}
		return attemptOutcome{fatal: "test-only failure", progressed: true, logPath: filepath.Join(s.cfg.StateDir, "logs", "fake-attempt.log")}
	}
	s.Run(context.Background())
	r, err := LoadRun(s.cfg.StateDir, s.SavedRunID())
	if err != nil {
		t.Fatal(err)
	}
	if r.Outcome != "failed" || r.ExitCode != 1 {
		t.Fatalf("unexpected outcome: %+v", r)
	}
	return s, r
}

func localRecoveryClient() *fakeSearch {
	return &fakeSearch{rows: []vipsearch.IndexVersion{{Number: 1, Active: true, Documents: 1000}, {Number: 2, Documents: 1000}},
		statuses: []*vipsearch.IndexingStatus{{Indexing: false}}}
}

func TestRunHistoryRecordsSettingsProgressAndAttempts(t *testing.T) {
	s, r := failedSavedRun(t, StrategyResume)
	if r.Config.Target != s.cfg.Target || r.Config.PostTypes != s.cfg.PostTypes || r.Phases[0].Version != 1 || r.Phases[0].LastObjectID != 700 || r.Phases[0].Done != 300 || r.Phases[0].Total != 1000 {
		t.Fatalf("lost saved configuration/progress: %+v", r)
	}
	if len(r.Attempts) != 1 || r.Attempts[0].Outcome != "non-retryable error" || !strings.Contains(r.LastError, "test-only failure") {
		t.Fatalf("lost failure history: %+v", r)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(runPath(s.cfg.StateDir, r.ID))
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("history not private: %v", err)
		}
	}
	r.Config.Notifications = notify.Config{Endpoint: "https://example.invalid/secret-topic", Token: "secret-token"}
	if err := saveRun(r); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(runPath(s.cfg.StateDir, r.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-topic") || strings.Contains(string(data), "secret-token") {
		t.Fatal("notification secrets saved in history")
	}
	all, warnings, err := ListRuns(s.cfg.StateDir)
	if err != nil || len(warnings) != 0 || len(all) != 1 {
		t.Fatalf("list failed: %v %v", warnings, err)
	}
}

func TestSavedRunResumeUsesExactScopeWithoutRepeatingSetup(t *testing.T) {
	for _, strategy := range []Strategy{StrategySetup, StrategyResume, StrategyNewVersion, StrategyIntoVersion} {
		t.Run(strategy.String(), func(t *testing.T) {
			// IntoVersion needs an explicit target, so create its record from a
			// stopped in-place run and make that deliberate saved choice.
			initial := strategy
			if strategy == StrategyIntoVersion {
				initial = StrategyResume
			}
			_, r := failedSavedRun(t, initial)
			if strategy == StrategyIntoVersion {
				r.Config.Strategy, r.Config.IntoVersion = strategy, 1
				if err := saveRun(r); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := ResumeConfig(r, notify.Config{})
			if err != nil {
				t.Fatal(err)
			}
			s := New(cfg)
			s.client = localRecoveryClient()
			s.wait = func(context.Context, time.Duration) bool { return false }
			calls := 0
			s.attempt = func(ctx context.Context, name string, version int, args []string) attemptOutcome {
				calls++
				command := strings.Join(args, " ")
				if containsSetup(args) || !strings.Contains(command, "--upper-limit-object-id=700") || !strings.Contains(command, "--post-type=post,breaking-news") || version != r.Phases[0].Version {
					t.Fatalf("unsafe saved-run resume: version=%d %s", version, command)
				}
				if s.phases[0].Total != 1000 || s.phases[0].Done != 300 {
					t.Fatal("phase progress reset on resume")
				}
				return attemptOutcome{fatal: "test stop"}
			}
			// A different shared checkpoint must not override this run's record.
			if err := s.store.WriteCheckpoint("post", r.Phases[0].Version, 100); err != nil {
				t.Fatal(err)
			}
			s.Run(context.Background())
			if calls != 1 {
				t.Fatalf("resume did not start: %s", doneMessage(s))
			}
			newRun, err := LoadRun(cfg.StateDir, s.SavedRunID())
			if err != nil || newRun.ParentID != r.ID || newRun.Phases[0].Attempt != 2 {
				t.Fatalf("resume lineage/attempt count lost: %v %+v", err, newRun)
			}
		})
	}
}

func doneMessage(s *Supervisor) string {
	message := ""
	for e := range s.Events() {
		if d, ok := e.(DoneEvent); ok {
			message = d.Message
		}
	}
	return message
}

func TestRecoveryBlocksUnsafeAndUnknownStateWithoutMutations(t *testing.T) {
	for _, scenario := range []string{"active", "unknown", "missing version", "changed active", "version recreated", "ambiguous setup", "registration interrupted", "changed pin", "already active build", "completed", "different working directory", "ignored local lock", "newer run", "corrupt history", "local supervisor"} {
		t.Run(scenario, func(t *testing.T) {
			strategy := StrategyResume
			if scenario == "changed pin" || scenario == "already active build" {
				strategy = StrategyNewVersion
			}
			s, r := failedSavedRun(t, strategy)
			client := localRecoveryClient()
			switch scenario {
			case "active":
				client.statuses = []*vipsearch.IndexingStatus{{Indexing: true}}
			case "unknown":
				client.statuses = nil
			case "missing version":
				client.rows = []vipsearch.IndexVersion{{Number: 3, Active: true}}
			case "changed active":
				client.rows[0].Active, client.rows[1].Active = false, true
			case "version recreated":
				r.Phases[0].VersionCreated = "2026-01-01 00:00:00"
				client.rows[0].Created = "2026-09-01 00:00:00"
			case "ambiguous setup":
				r.Config.Strategy, r.Phases[0].LastObjectID = StrategySetup, -1
			case "registration interrupted":
				r.Phases[0].RegistrationPending = true
			case "changed pin":
				if err := s.store.PinVersion("post", 3); err != nil {
					t.Fatal(err)
				}
			case "already active build":
				client.rows[0].Active, client.rows[1].Active = false, true
			case "completed":
				r.Outcome = "completed"
			case "different working directory":
				r.WorkingDir = filepath.Join(r.WorkingDir, "other")
			case "ignored local lock":
				r.Config.IgnoreLock = true
			case "newer run":
				newer := r
				newer.StartedAt = r.StartedAt.Add(time.Second)
				newer.ID = newer.StartedAt.UTC().Format("20060102T150405.000000000Z") + "-0000000000000000"
				if err := saveRun(newer); err != nil {
					t.Fatal(err)
				}
				if err := writeHistoryFile(filepath.Join(r.Config.StateDir, "runs", "latest"), []byte(newer.ID)); err != nil {
					t.Fatal(err)
				}
			case "corrupt history":
				if err := os.WriteFile(filepath.Join(r.Config.StateDir, "runs", "broken.json"), []byte("{"), 0600); err != nil {
					t.Fatal(err)
				}
			case "local supervisor":
				lock, err := acquireStateLock(r.Config.StateDir)
				if err != nil {
					t.Fatal(err)
				}
				defer lock.Release()
			}
			report := InspectRecovery(context.Background(), r, client)
			if report.CanResume || len(report.Reasons) == 0 {
				t.Fatalf("unsafe resume allowed: %+v", report)
			}
			if client.clears != 0 || client.adds != 0 || client.activations != 0 {
				t.Fatal("recovery performed a mutation")
			}
		})
	}
}

func TestLatestRunPointerSurvivesClockRollback(t *testing.T) {
	_, first := failedSavedRun(t, StrategyResume)
	later := first
	later.StartedAt = first.StartedAt.Add(-time.Hour)
	later.ID = later.StartedAt.UTC().Format("20060102T150405.000000000Z") + "-0000000000000000"
	if err := saveRun(later); err != nil {
		t.Fatal(err)
	}
	if err := writeHistoryFile(filepath.Join(first.Config.StateDir, "runs", "latest"), []byte(later.ID)); err != nil {
		t.Fatal(err)
	}
	if InspectRecovery(context.Background(), first, localRecoveryClient()).CanResume {
		t.Fatal("clock rollback resurrected a superseded run")
	}
	if report := InspectRecovery(context.Background(), later, localRecoveryClient()); !report.CanResume {
		t.Fatalf("actual latest run blocked by clock rollback: %+v", report)
	}
	runs, _, err := ListRuns(first.Config.StateDir)
	if err != nil || runs[0].ID != later.ID {
		t.Fatal("latest run not shown first after clock rollback")
	}
}

func TestAmbiguousVersionRegistrationCannotBeRepeatedByRecovery(t *testing.T) {
	s := testSupervisor(t)
	s.cfg.Strategy = StrategyNewVersion
	s.client.(*fakeSearch).add = vipsearch.RunResult{Err: errors.New("fake CLI died")}
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		t.Fatal("indexing after unconfirmed registration")
		return attemptOutcome{}
	}
	s.Run(context.Background())
	r, err := LoadRun(s.cfg.StateDir, s.SavedRunID())
	if err != nil || !r.Phases[0].RegistrationPending {
		t.Fatalf("registration intent lost: %v", err)
	}
	cfg, _ := ResumeConfig(r, notify.Config{})
	resume := New(cfg)
	client := localRecoveryClient()
	resume.client = client
	resume.Run(context.Background())
	if client.adds != 0 || !strings.Contains(doneMessage(resume), "registration was interrupted") {
		t.Fatal("recovery repeated ambiguous registration")
	}
}

func TestRecoveryKnownIdleAndInterruptedRecord(t *testing.T) {
	_, r := failedSavedRun(t, StrategyResume)
	r.Outcome, r.FinishedAt, r.ExitCode = "running", time.Time{}, -1 // abrupt host exit
	report := InspectRecovery(context.Background(), r, localRecoveryClient())
	if !report.CanResume {
		t.Fatalf("idle saved run blocked: %+v", report)
	}
}

func TestResumeRevalidatesAndRejectsChangedSettings(t *testing.T) {
	for _, change := range []string{"remote became active", "post types changed", "version removed"} {
		t.Run(change, func(t *testing.T) {
			_, r := failedSavedRun(t, StrategyResume)
			if !InspectRecovery(context.Background(), r, localRecoveryClient()).CanResume {
				t.Fatal("initial inspection failed")
			}
			cfg, _ := ResumeConfig(r, notify.Config{})
			client := localRecoveryClient()
			switch change {
			case "remote became active":
				client.statuses = []*vipsearch.IndexingStatus{{Indexing: true}}
			case "post types changed":
				cfg.PostTypes = "other"
			case "version removed":
				client.rows = []vipsearch.IndexVersion{{Number: 3, Active: true}}
			}
			s := New(cfg)
			s.client = client
			s.attempt = func(context.Context, string, int, []string) attemptOutcome {
				t.Fatal("unsafe resume executed an attempt")
				return attemptOutcome{}
			}
			s.Run(context.Background())
			if !strings.Contains(doneMessage(s), "recovery blocked") {
				t.Fatal("runtime did not revalidate")
			}
			if s.SavedRunID() != "" {
				t.Fatal("failed resume superseded the valid history entry")
			}
		})
	}
}

func TestSavedRunSkipsCompletedPhasesAndDoesNotReindexCompletedAttempt(t *testing.T) {
	s := testSupervisor(t)
	s.cfg.Indexables = []string{"post", "term"}
	s.phases = append(s.phases, PhaseSnapshot{Name: "term", Done: -1, Total: -1, LastObjectID: -1})
	s.attempt = func(ctx context.Context, name string, v int, args []string) attemptOutcome {
		if name == "post" {
			return attemptOutcome{success: true, indexed: 1000}
		}
		return attemptOutcome{fatal: "test interruption"}
	}
	s.Run(context.Background())
	r, err := LoadRun(s.cfg.StateDir, s.SavedRunID())
	if err != nil {
		t.Fatal(err)
	}
	if r.Phases[0].Status != PhaseComplete {
		t.Fatal("completed phase not saved")
	}
	// Simulate a clean indexing exit followed by interrupted completion work.
	r.Phases[1].IndexingComplete = true
	if err := saveRun(r); err != nil {
		t.Fatal(err)
	}
	cfg, _ := ResumeConfig(r, notify.Config{})
	resume := New(cfg)
	resume.client = localRecoveryClient()
	resume.attempt = func(context.Context, string, int, []string) attemptOutcome {
		t.Fatal("completed work was indexed again")
		return attemptOutcome{}
	}
	resume.Run(context.Background())
	if msg := doneMessage(resume); !strings.Contains(msg, "all phases complete") {
		t.Fatalf("resume failed: %s", msg)
	}
}

func TestCorruptHistoryAndUnsafeIDsAreRejected(t *testing.T) {
	_, r := failedSavedRun(t, StrategyResume)
	for _, id := range []string{"../supervisor", "/tmp/other", "", "bad"} {
		if _, err := LoadRun(r.Config.StateDir, id); err == nil {
			t.Fatalf("invalid ID accepted: %q", id)
		}
	}
	if err := os.WriteFile(runPath(r.Config.StateDir, r.ID), []byte("{\"secret\":\"private\""), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRun(r.Config.StateDir, r.ID); err == nil || strings.Contains(err.Error(), "private") {
		t.Fatal("corrupt history accepted or exposed contents")
	}
	if _, warnings, err := ListRuns(r.Config.StateDir); err != nil || len(warnings) != 1 {
		t.Fatal("corrupt entry silently omitted")
	}
}

func TestHistoryWriteFailureStopsBeforeIndexing(t *testing.T) {
	s := testSupervisor(t)
	if err := os.WriteFile(filepath.Join(s.cfg.StateDir, "runs"), []byte("not a directory"), 0600); err != nil {
		t.Fatal(err)
	}
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		t.Fatal("indexing ran without writable history")
		return attemptOutcome{}
	}
	s.Run(context.Background())
	if !strings.Contains(doneMessage(s), "history") {
		t.Fatal("history error not surfaced")
	}
}

func TestHistoryLockProbeDoesNotClearOwner(t *testing.T) {
	dir := t.TempDir()
	lock, err := acquireStateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	held, err := CheckStateLock(dir)
	if err != nil || !held {
		t.Fatalf("lock not detected: %t %v", held, err)
	}
	if _, err := acquireStateLock(dir); !errors.Is(err, ErrLocked) {
		t.Fatalf("probe disturbed owner: %v", err)
	}
	lock.Release()
	if held, err = CheckStateLock(dir); err != nil || held {
		t.Fatalf("released lock seen as stale: %t %v", held, err)
	}
}
