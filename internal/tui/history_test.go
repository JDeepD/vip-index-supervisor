package tui

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/notify"
	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func historyFixture(t *testing.T) (*session, supervise.RunRecord) {
	t.Helper()
	sess := newSession()
	sess.stateDir = t.TempDir()
	sess.target.AppEnv = "@fake.local" // only local JSON and injected assessments, never invoked
	cfg := sess.config()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	r := supervise.RunRecord{Schema: 1, ID: "20260905T120000.000000000Z-0000000000000000",
		Config: cfg, WorkingDir: wd, StartedAt: time.Now(), UpdatedAt: time.Now(), Outcome: "failed", ExitCode: 1,
		Phases: []supervise.PhaseSnapshot{{Name: "post", Version: 1, LastObjectID: 700, Done: 300, Total: 1000, Attempt: 1, Status: supervise.PhaseFailed}}}
	writeHistoryFixture(t, r)
	return sess, r
}

func writeHistoryFixture(t *testing.T, r supervise.RunRecord) {
	t.Helper()
	data, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(r.Config.StateDir, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, r.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestHistoryReadsLocalRecordsAndFiltersTarget(t *testing.T) {
	sess, r := historyFixture(t)
	other := r
	other.ID = "20260905T120001.000000000Z-0000000000000000"
	other.Config.Target.AppEnv = "@other.local"
	writeHistoryFixture(t, other)
	s := newHistoryScreen(sess)
	msg := s.Init()()
	s.Update(msg)
	if s.loading || len(s.runs) != 1 || s.runs[0].ID != r.ID {
		t.Fatalf("history target filtering failed: %+v", s.runs)
	}
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("history selection did not open recovery")
	}
	pushed, ok := cmd().(pushMsg)
	if !ok {
		t.Fatal("not a screen transition")
	}
	if _, ok := pushed.screen.(*recoveryScreen); !ok {
		t.Fatal("history started indexing instead of opening recovery")
	}
}

func TestRecoveryRequiresAssessmentAndExplicitConfirmation(t *testing.T) {
	sess, r := historyFixture(t)
	sess.notifications = notify.Config{Endpoint: "https://example.invalid/not-sent"}
	s := newRecoveryScreen(sess, r.Config.StateDir, r.ID)
	s.inspect = func(context.Context, supervise.RunRecord) supervise.RecoveryReport {
		return supervise.RecoveryReport{CanResume: true, Verdict: "Ready", Remote: &vipsearch.IndexingStatus{Indexing: false}}
	}
	cmd := s.Init()
	if _, start := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); start != nil {
		t.Fatal("started before inspection")
	}
	s.Update(cmd())
	if _, start := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); start != nil || s.stage != "confirm" {
		t.Fatal("started without confirmation")
	}
	_, start := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if start == nil {
		t.Fatal("confirmed resume not offered")
	}
	pushed := start().(pushMsg)
	run, ok := pushed.screen.(*runScreen)
	if !ok || run.cfg.ResumeRunID != r.ID || run.cfg.Notifications != sess.notifications || run.cfg.IgnoreLock {
		t.Fatal("wrong saved-run settings")
	}
	// Do NOT call run.Init(): real targets are never used in tests.
}

func TestRecoveryUnknownAndStaleResultsCannotStart(t *testing.T) {
	sess, r := historyFixture(t)
	s := newRecoveryScreen(sess, r.Config.StateDir, r.ID)
	s.inspect = func(context.Context, supervise.RunRecord) supervise.RecoveryReport {
		return supervise.RecoveryReport{Verdict: "Unknown", Reasons: []string{"remote state unknown"}}
	}
	s.Update(s.Init()())
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || s.stage != "report" {
		t.Fatal("unknown state allowed resume")
	}
	s.Update(recoveryResultMsg{id: s.id - 1, run: r, report: supervise.RecoveryReport{CanResume: true}})
	if s.report.CanResume {
		t.Fatal("stale assessment replaced current state")
	}
}

func TestRecoveryRefusesForeignTargetBeforeAnyRemoteRead(t *testing.T) {
	sess, r := historyFixture(t)
	sess.target.AppEnv = "@different.local"
	s := newRecoveryScreen(sess, r.Config.StateDir, r.ID)
	s.inspect = func(context.Context, supervise.RunRecord) supervise.RecoveryReport {
		t.Fatal("foreign target contacted")
		return supervise.RecoveryReport{}
	}
	s.Update(s.Init()())
	if s.report.CanResume || !strings.Contains(s.View(), "does not match") {
		t.Fatal("foreign run accepted")
	}
}

func TestRecoveryExplainsCheckpointAndPinnedVersion(t *testing.T) {
	_, r := historyFixture(t)
	r.LastError = "index lock kept reappearing"
	r.Attempts = []supervise.AttemptRecord{{Phase: "post", Version: 1, Number: 1, Outcome: "lock refused"}}
	report := supervise.RecoveryReport{Verdict: "Investigate", Pins: map[string]int{"post": 1},
		Remote:  &vipsearch.IndexingStatus{Indexing: true, CurrentSync: &vipsearch.SyncItem{Indexable: "post", LastObjectID: 600}},
		Reasons: []string{"Wait for the worker."}}
	view := renderRecovery(r, report)
	for _, want := range []string{"checkpoint: 700", "pinned version: v1", "600 (informational", "lock refused", "Wait for the worker", "never clears"} {
		if !strings.Contains(view, want) {
			t.Fatalf("missing %q in %s", want, view)
		}
	}
}

func TestEmptyHistoryNavigationDoesNotPanic(t *testing.T) {
	sess := newSession()
	sess.stateDir = t.TempDir()
	s := newHistoryScreen(sess)
	s.Update(s.Init()())
	for _, key := range []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyEnter} {
		if _, cmd := s.Update(tea.KeyMsg{Type: key}); cmd != nil {
			t.Fatal("empty history opened a run")
		}
	}
	if !strings.Contains(s.View(), "No saved runs") {
		t.Fatal("empty state missing")
	}
}

func TestActionMenuKeepsRecoveryAndNotificationsVisibleInShortTerminal(t *testing.T) {
	s := newActionScreen(newSession())
	s.SetSize(80, 20)
	s.menu.cursor = 1
	if !strings.Contains(s.View(), "history / recovery") {
		t.Fatal("recovery action outside viewport")
	}
	s.menu.cursor = len(s.menu.Items) - 1
	view := s.View()
	if !strings.Contains(view, "notifications") || strings.Contains(view, "history / recovery") {
		t.Fatal("menu did not scroll to the selected last action")
	}
	if strings.Count(view, "\n") > 15 {
		t.Fatal("action menu exceeds short terminal height")
	}
}

func TestResumeConfirmationShowsExactVersionAndCheckpoint(t *testing.T) {
	sess, r := historyFixture(t)
	s := newRecoveryScreen(sess, r.Config.StateDir, r.ID)
	s.run = r
	for _, want := range []string{"@fake.local", "saved version 1", "checkpoint 700", "No --setup"} {
		if !strings.Contains(s.resumePrompt(), want) {
			t.Fatalf("resume confirmation missing %q", want)
		}
	}
}
