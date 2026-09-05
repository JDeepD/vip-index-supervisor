package supervise

import (
	"context"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/notify"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func milestoneServer(t *testing.T) (*httptest.Server, <-chan string) {
	t.Helper()
	alerts := make(chan string, 32)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		alerts <- r.Header.Get("Title") + "\n" + string(body)
	}))
	t.Cleanup(server.Close)
	return server, alerts
}

func expectAlert(t *testing.T, alerts <-chan string, want string) {
	t.Helper()
	select {
	case got := <-alerts:
		if !strings.Contains(got, want) {
			t.Fatalf("got alert %q, want %q", got, want)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("did not receive %q", want)
	}
}

func TestMilestonesUseCountsDeduplicateAndReserve100ForSuccess(t *testing.T) {
	server, alerts := milestoneServer(t)
	s := testSupervisor(t)
	s.cfg.Notifications.Endpoint = server.URL + "/local-only"
	s.startNotifications()
	defer s.closeNotifications()
	s.phases[0].Version = 1
	for _, done := range []int64{249, 250, 250, 300, 500, 750, 1000} {
		err := s.applyProgress("post", 1, vipsearch.Progress{Done: done, Total: 1000, LastObjectID: 10000 - done, IndexedCount: -1})
		if err != nil {
			t.Fatal(err)
		}
		switch done {
		case 250:
			if len(alerts) > 1 {
				t.Fatal("duplicate quarter alert")
			}
		}
	}
	expectAlert(t, alerts, "25%")
	expectAlert(t, alerts, "50%")
	expectAlert(t, alerts, "75%")
	s.finish(DoneEvent{ExitCode: 1, Message: "index command failed after reporting all objects"})
	s.closeNotifications()
	expectAlert(t, alerts, "Run failed")
	if len(alerts) != 0 {
		t.Fatal("100% or duplicate alerts sent despite failure")
	}
}

func TestMilestonesWaitForKnownCountsAndHandleHugeTotals(t *testing.T) {
	server, alerts := milestoneServer(t)
	s := testSupervisor(t)
	s.cfg.Notifications.Endpoint = server.URL + "/test"
	s.startNotifications()
	defer s.closeNotifications()
	if err := s.applyProgress("post", 1, vipsearch.Progress{Done: -1, Total: -1, LastObjectID: 250, IndexedCount: -1}); err != nil {
		t.Fatal(err)
	}
	if s.phases[0].NotifiedPercent != 0 {
		t.Fatal("ID mistaken for percentage")
	}
	s.phases[0].Total, s.phases[0].Done = math.MaxInt64, math.MaxInt64/2
	if err := s.progressMilestones(); err != nil {
		t.Fatal(err)
	}
	expectAlert(t, alerts, "25%")
	if s.phases[0].NotifiedPercent != 25 {
		t.Fatal("fraction rounded up or overflowed")
	}
	s.phases[0].Done++
	if err := s.progressMilestones(); err != nil {
		t.Fatal(err)
	}
	expectAlert(t, alerts, "50%")
}

func TestSavedRunMilestonesSurviveRetriesAndResume(t *testing.T) {
	server, alerts := milestoneServer(t)
	_, r := failedSavedRun(t, StrategyResume)
	r.Phases[0].NotifiedPercent = 25
	if err := saveRun(r); err != nil {
		t.Fatal(err)
	}
	cfg, err := ResumeConfig(r, notify.Config{Endpoint: server.URL + "/local-only"})
	if err != nil {
		t.Fatal(err)
	}
	s := New(cfg)
	s.client = localRecoveryClient()
	s.wait = func(context.Context, time.Duration) bool { return true }
	attempt := 0
	s.attempt = func(ctx context.Context, name string, version int, args []string) attemptOutcome {
		attempt++
		if attempt == 1 {
			expectAlert(t, alerts, "Run started")
			// 300 were already processed. This attempt reports only 700 left.
			if err := s.applyProgress(name, version, vipsearch.Progress{Done: 200, Total: 700, LastObjectID: 500, IndexedCount: -1}); err != nil {
				t.Fatal(err)
			}
			expectAlert(t, alerts, "50%")
			return attemptOutcome{progressed: true}
		}
		if err := s.applyProgress(name, version, vipsearch.Progress{Done: 250, Total: 500, LastObjectID: 250, IndexedCount: -1}); err != nil {
			t.Fatal(err)
		}
		expectAlert(t, alerts, "75%")
		return attemptOutcome{success: true, indexed: 500}
	}
	s.Run(context.Background())
	expectAlert(t, alerts, "100% complete")
	if len(alerts) != 0 {
		t.Fatal("duplicate milestone after saved-run resume")
	}
	last, err := LoadRun(cfg.StateDir, s.SavedRunID())
	if err != nil || last.Phases[0].NotifiedPercent != 100 || last.Outcome != "completed" {
		t.Fatalf("milestone state not persisted: %v %+v", err, last)
	}
}

func TestCompletionAlertWaitsForVerificationAndActivation(t *testing.T) {
	server, alerts := milestoneServer(t)
	s := testSupervisor(t)
	s.cfg.Strategy = StrategyNewVersion
	s.cfg.VerifyAttempts = 1
	s.cfg.Notifications.Endpoint = server.URL + "/local-only"
	if err := s.store.PinVersion("post", 2); err != nil {
		t.Fatal(err)
	}
	s.client.(*fakeSearch).rows[1].Documents = 0
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		expectAlert(t, alerts, "Run started")
		return attemptOutcome{success: true, indexed: 1000}
	}
	s.Run(context.Background())
	expectAlert(t, alerts, "Run failed")
	if len(alerts) != 0 {
		t.Fatal("100% sent before verification passed")
	}
	r, err := LoadRun(s.cfg.StateDir, s.SavedRunID())
	if err != nil || !r.Phases[0].IndexingComplete || r.Phases[0].Status == PhaseComplete {
		t.Fatalf("completion stages not saved: %v %+v", err, r)
	}
}
