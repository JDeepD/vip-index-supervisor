package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func frozenCLI() *vipsearch.IndexingStatus {
	return &vipsearch.IndexingStatus{Indexing: true, Method: "cli", StartDateTime: "run-A", ItemsIndexed: 300, TotalItems: 1000,
		CurrentSync: &vipsearch.SyncItem{Indexable: "post", Synced: 300, LastObjectID: 700}}
}

func TestRecoveryObservationAndSafety(t *testing.T) {
	idle := &vipsearch.IndexingStatus{Indexing: false}
	for _, scenario := range []string{"idle without lock", "idle with lock", "active strict", "unknown first", "unknown second", "new worker", "progress", "skipped", "failed", "raw identity", "wrong indexable", "wrong version", "web worker", "no counters", "cancel wait"} {
		t.Run(scenario, func(t *testing.T) {
			s := testSupervisor(t)
			f := s.client.(*fakeSearch)
			first, second := frozenCLI(), frozenCLI()
			s.cfg.AggressiveRecovery = scenario != "active strict"
			lock := true
			want := recoveryAbort
			wantClears := 0
			switch scenario {
			case "idle without lock":
				first, second, lock, want = idle, idle, false, recoveryReady
			case "idle with lock":
				first, second, want, wantClears = idle, idle, recoveryReady, 1
			case "unknown first":
				first = nil
			case "unknown second":
				second = nil
			case "new worker":
				second.StartDateTime = "run-B"
			case "progress":
				second.ItemsIndexed++
			case "skipped":
				second.CurrentSync.Skipped++
			case "failed":
				second.CurrentSync.Failed++
			case "raw identity":
				first.Raw = map[string]any{"worker_id": "A"}
				second.Raw = map[string]any{"worker_id": "B"}
			case "wrong indexable":
				first.CurrentSync.Indexable = "term"
				second.CurrentSync.Indexable = "term"
			case "wrong version":
				first.Raw = map[string]any{"version": "2"}
				second.Raw = map[string]any{"version": "2"}
			case "web worker":
				first.Method, second.Method = "web", "web"
			case "no counters":
				first.ItemsIndexed, second.ItemsIndexed = -1, -1
				first.CurrentSync, second.CurrentSync = nil, nil
			}
			f.statuses = []*vipsearch.IndexingStatus{first, second}
			var waits []time.Duration
			s.wait = func(_ context.Context, d time.Duration) bool {
				waits = append(waits, d)
				return scenario != "cancel wait"
			}
			if got := s.recoverCycle(context.Background(), "post", 1, lock); got != want {
				t.Fatalf("decision=%v want=%v", got, want)
			}
			if f.clears != wantClears || f.syncClears != 0 {
				t.Fatalf("unexpected mutations: %d/%d", f.clears, f.syncClears)
			}
			if first != nil && !reflect.DeepEqual(waits, []time.Duration{30 * time.Second}) {
				t.Fatalf("waits=%v", waits)
			}
		})
	}
}

func TestAggressiveCleanupVerifiesEveryStage(t *testing.T) {
	for _, scenario := range []string{"transient clears", "options clear", "options become idle", "unknown before transient", "changed before transient", "new worker after transient", "unknown after transient", "transient failed", "transient unacknowledged", "changed before options", "changed during options", "unknown during options", "options failed", "unknown after options", "changed after options", "still locked"} {
		t.Run(scenario, func(t *testing.T) {
			s := testSupervisor(t)
			s.cfg.AggressiveRecovery = true
			f := s.client.(*fakeSearch)
			f.statuses = []*vipsearch.IndexingStatus{frozenCLI()}
			idle := func() { f.statuses = []*vipsearch.IndexingStatus{{Indexing: false}} }
			unknown := func() { f.statuses = nil }
			changed := func() { st := frozenCLI(); st.StartDateTime = "run-B"; f.statuses = []*vipsearch.IndexingStatus{st} }
			want, clears, options := recoveryAbort, 1, 0
			switch scenario {
			case "transient clears":
				f.onClear = idle
				want = recoveryReady
			case "options clear":
				f.onSyncClear = func() {
					if f.syncClears == 6 {
						idle()
					}
				}
				want, options = recoveryReady, 6
			case "options become idle":
				f.onSyncClear = idle
				want, options = recoveryReady, 1
			case "unknown before transient":
				f.onStatus = func() {
					if f.statusCalls == 3 {
						unknown()
					}
				}
				clears = 0
			case "changed before transient":
				f.onStatus = func() {
					if f.statusCalls == 3 {
						changed()
					}
				}
				clears = 0
			case "new worker after transient":
				f.onClear = changed
			case "unknown after transient":
				f.onClear = unknown
			case "transient failed":
				f.clearResult = &vipsearch.RunResult{Err: errors.New("permission denied")}
			case "transient unacknowledged":
				f.clearResult = &vipsearch.RunResult{Output: "Warning: nothing confirmed"}
			case "changed before options":
				f.onStatus = func() {
					if f.statusCalls == 5 {
						changed()
					}
				}
			case "changed during options":
				f.onSyncClear = changed
				options = 1
			case "unknown during options":
				f.onSyncClear = unknown
				options = 1
			case "options failed":
				f.syncResult = &vipsearch.RunResult{Err: errors.New("failed mutation")}
				options = 1
			case "unknown after options":
				f.onSyncClear = func() {
					if f.syncClears == 6 {
						unknown()
					}
				}
				options = 6
			case "changed after options":
				f.onSyncClear = func() {
					if f.syncClears == 6 {
						changed()
					}
				}
				options = 6
			case "still locked":
				want, options = recoveryAgain, 6
			}
			if got := s.recoverCycle(context.Background(), "post", 1, true); got != want {
				t.Fatalf("decision=%v want=%v, clears=%d options=%d", got, want, f.clears, f.syncClears)
			}
			if f.clears != clears || f.syncClears != options {
				t.Fatalf("mutations=%d/%d want=%d/%d", f.clears, f.syncClears, clears, options)
			}
		})
	}
}

func TestRecoveryStopsAtFiveUnsuccessfulCycles(t *testing.T) {
	t.Run("cleanup never makes idle", func(t *testing.T) {
		s := testSupervisor(t)
		s.cfg.AggressiveRecovery = true
		f := s.client.(*fakeSearch)
		f.statuses = []*vipsearch.IndexingStatus{frozenCLI()}
		cycles := 0
		if s.recoverForRetry(context.Background(), "post", 1, true, &cycles) {
			t.Fatal("locked platform considered ready")
		}
		if cycles != 5 || f.clears != 5 || f.syncClears != 30 {
			t.Fatalf("unbounded recovery: cycles=%d clears=%d/%d", cycles, f.clears, f.syncClears)
		}
	})
	t.Run("cleanup success does not reset budget", func(t *testing.T) {
		s := testSupervisor(t)
		attempts := 0
		s.attempt = func(context.Context, string, int, []string) attemptOutcome {
			attempts++
			return attemptOutcome{lockError: true}
		}
		if s.runPhase(context.Background(), "post") {
			t.Fatal("failed indexing completed")
		}
		if attempts != 6 || s.client.(*fakeSearch).clears != 5 {
			t.Fatalf("cleanup reset budget: attempts=%d clears=%d", attempts, s.client.(*fakeSearch).clears)
		}
	})
}

func TestRecoveryBudgetResetsOnlyOnRealProgress(t *testing.T) {
	for _, realProgress := range []bool{false, true} {
		t.Run(fmt.Sprint(realProgress), func(t *testing.T) {
			s := testSupervisor(t)
			// Start from a known checkpoint. Reprinting it is not progress.
			if err := s.store.WriteCheckpoint("post", 1, 900); err != nil {
				t.Fatal(err)
			}
			attempts := 0
			s.attempt = func(_ context.Context, name string, version int, args []string) attemptOutcome {
				attempts++
				if attempts == 9 {
					return attemptOutcome{success: true}
				}
				o := attemptOutcome{lockError: true}
				if attempts == 4 {
					id := 900
					if realProgress {
						id = 800
					}
					s.consumeLine(name, version, fmt.Sprintf("Processed 300/1000. Last Object ID: %d", id), &o)
				}
				return o
			}
			if got := s.runPhase(context.Background(), "post"); got != realProgress {
				t.Fatalf("completion=%v, attempts=%d", got, attempts)
			}
			if !realProgress && attempts != 6 {
				t.Fatalf("replayed checkpoint reset budget: attempts=%d", attempts)
			}
		})
	}
}

func TestActiveRecoveryRequiresUsableScopeAndCounters(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  map[string]any
		want bool
	}{
		{"numeric strings", map[string]any{"items_indexed": " 300 ", "version": "1"}, true},
		{"JSON numbers", map[string]any{"items_indexed": json.Number("300"), "version": json.Number("1")}, true},
		{"malformed counter", map[string]any{"items_indexed": "not available"}, false},
		{"null counter", map[string]any{"items_indexed": nil}, false},
		{"fractional counter", map[string]any{"items_indexed": 3.5}, false},
		{"invalid negative counter", map[string]any{"items_indexed": -2}, false},
		{"unknown version", map[string]any{"version": nil}, false},
		{"different top-level indexable", map[string]any{"indexable": "term"}, false},
		{"malformed nested counter", map[string]any{"current_sync_item": map[string]any{"failed": "unknown"}}, false},
		{"different nested version", map[string]any{"current_sync_item": map[string]any{"version": "2"}}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := frozenCLI()
			st.Raw = tc.raw
			if got := activeRecoveryEligible(st, "post", 1); got != tc.want {
				t.Fatalf("eligibility=%v want=%v", got, tc.want)
			}
		})
	}
	st := frozenCLI()
	st.CurrentSync = nil // VIP versions with only an items_indexed counter.
	if !activeRecoveryEligible(st, "post", 1) {
		t.Fatal("required a last-object ID when a valid counter was available")
	}
}

func TestRecoveryStopDuringEveryStatusReadPreventsFurtherMutation(t *testing.T) {
	for _, stopKind := range []string{"context", "user"} {
		// Two observations, pre-delete, post-delete, six option guards, final read.
		for stopAt := 1; stopAt <= 11; stopAt++ {
			t.Run(fmt.Sprintf("%s/read-%d", stopKind, stopAt), func(t *testing.T) {
				s := testSupervisor(t)
				s.cfg.AggressiveRecovery = true
				f := s.client.(*fakeSearch)
				f.statuses = []*vipsearch.IndexingStatus{frozenCLI()}
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				f.onStatus = func() {
					if f.statusCalls == stopAt {
						if stopKind == "context" {
							cancel()
						} else {
							s.RequestStop(false)
						}
					}
				}
				if got := s.recoverCycle(ctx, "post", 1, true); got != recoveryAbort {
					t.Fatalf("stop ignored: decision=%v", got)
				}
				clears := 0
				if stopAt > 3 {
					clears = 1
				}
				if f.clears != clears || f.syncClears != max(0, stopAt-5) {
					t.Fatalf("mutation after stop: clears=%d options=%d", f.clears, f.syncClears)
				}
			})
		}
	}
}

func TestRecoveryCountOnlyProgressResetsBudget(t *testing.T) {
	s := testSupervisor(t)
	attempts := 0
	s.attempt = func(_ context.Context, name string, version int, _ []string) attemptOutcome {
		attempts++
		if attempts == 9 {
			return attemptOutcome{success: true}
		}
		o := attemptOutcome{lockError: true}
		if attempts == 4 {
			s.consumeLine(name, version, "Processed 300/1000", &o)
		}
		return o
	}
	if !s.runPhase(context.Background(), "post") || attempts != 9 {
		t.Fatalf("count-only progress did not reset recovery: attempts=%d", attempts)
	}
}

func TestNonLockFailureDiagnosedBeforeFirstRetry(t *testing.T) {
	s := testSupervisor(t)
	f := s.client.(*fakeSearch)
	attempts := 0
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		attempts++
		if attempts == 1 {
			return attemptOutcome{exitErr: errors.New("PHP memory failure")}
		}
		if f.statusCalls < 3 {
			t.Fatal("retry preceded observations and final status check")
		}
		return attemptOutcome{success: true}
	}
	if !s.runPhase(context.Background(), "post") || attempts != 2 || f.clears != 0 {
		t.Fatal("idle non-lock failure recovery failed or deleted remote state")
	}
}

func TestNoRecoveryAfterFatalCancellationOrAmbiguousSetup(t *testing.T) {
	for _, scenario := range []string{"fatal", "cancel", "stop", "setup"} {
		t.Run(scenario, func(t *testing.T) {
			s := testSupervisor(t)
			if scenario == "setup" {
				s.cfg.Strategy = StrategySetup
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			s.attempt = func(context.Context, string, int, []string) attemptOutcome {
				switch scenario {
				case "fatal":
					return attemptOutcome{fatal: "authorization"}
				case "cancel":
					cancel()
				case "stop":
					s.RequestStop(false)
				}
				return attemptOutcome{}
			}
			if s.runPhase(ctx, "post") {
				t.Fatal("unexpected completion")
			}
			f := s.client.(*fakeSearch)
			if f.statusCalls != 0 || f.clears != 0 || f.syncClears != 0 {
				t.Fatal("non-retryable failure entered recovery")
			}
		})
	}
}

func TestRecoveryRechecksVersionAndLateWorker(t *testing.T) {
	for _, scenario := range []string{"missing version", "recreated version", "changed active version", "new worker during backoff", "cancel during backoff"} {
		t.Run(scenario, func(t *testing.T) {
			s := testSupervisor(t)
			f := s.client.(*fakeSearch)
			f.rows[0].Created = "original"
			attempts := 0
			s.attempt = func(context.Context, string, int, []string) attemptOutcome {
				attempts++
				switch scenario {
				case "missing version":
					f.rows = nil
				case "recreated version":
					f.rows[0].Created = "new"
				case "changed active version":
					f.rows[0].Active = false
				}
				return attemptOutcome{lockError: true}
			}
			s.wait = func(_ context.Context, d time.Duration) bool {
				if d == s.cfg.BackoffBase {
					if scenario == "new worker during backoff" {
						f.statuses = []*vipsearch.IndexingStatus{frozenCLI()}
					}
					if scenario == "cancel during backoff" {
						s.RequestStop(false)
						return false
					}
				}
				return true
			}
			if s.runPhase(context.Background(), "post") || attempts != 1 {
				t.Fatalf("unsafe retry: attempts=%d", attempts)
			}
		})
	}
}
