package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func TestUnlockUnknownRequiresConfirmation(t *testing.T) {
	s := newUnlockScreen(newSession())
	_, cmd := s.Update(unlockStatusMsg{id: s.id, known: false})
	if s.stage != "confirm" || cmd != nil || s.menu.Selected().Value != "back" {
		t.Fatalf("unknown status initiated cleanup: stage=%s cmd=%v", s.stage, cmd != nil)
	}
	if !strings.Contains(s.View(), "unknown") {
		t.Fatal("unknown status hidden")
	}
	s.stage = "clearing"
	if !s.OwnsEsc() || !s.OwnsCtrlC() {
		t.Fatal("cleanup can be abandoned")
	}
	_, cmd = s.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'q'}})
	if cmd != nil {
		t.Fatal("quit during cleanup")
	}
}

func TestStatusUsesTopLevelProgressWhenSyncMissing(t *testing.T) {
	out := vipsearch.StripANSI(renderStatus(&vipsearch.IndexingStatus{Indexing: true, TotalItems: 1000, ItemsIndexed: 600}))
	if !strings.Contains(out, "600 / 1,000") {
		t.Fatalf("lost top-level progress: %s", out)
	}
}

func TestEmptyCountsAreNotHealthy(t *testing.T) {
	out := renderCounts(vipsearch.CountReport{Raw: "Warning: acf called incorrectly"})
	if strings.Contains(out, "every counted row matches") {
		t.Fatal("unparsed output reported healthy")
	}
}

func TestFailedCountsKeepPartialResultsAndError(t *testing.T) {
	out := renderCounts(vipsearch.CountReport{
		Failed: true, Raw: "Error: request failed midway", Rows: []vipsearch.CountRow{{Entity: "post", Type: "breaking-news", Version: 2, DB: 100, ES: 90, Diff: -10}},
	})
	for _, want := range []string{"incomplete", "breaking-news", "request failed midway"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in %s", want, out)
		}
	}
	if strings.Contains(out, "every counted row matches") {
		t.Fatal("failed validation reported healthy")
	}
}

func TestVersionMutationCannotBeAbandonedWithCtrlC(t *testing.T) {
	s := newVersionMutateScreen(newSession(), "post", nil, vipsearch.IndexVersion{Number: 2}, mutateActivate)
	s.stage = "running"
	app := &App{stack: []Screen{s}}
	_, cmd := app.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	if cmd != nil || s.stage != "running" {
		t.Fatal("Ctrl+C abandoned an in-flight mutation")
	}
	s.stage = "done"
	if s.OwnsCtrlC() {
		t.Fatal("Ctrl+C still blocked after completion")
	}
}

func TestBudgetParsing(t *testing.T) {
	for _, input := range []string{"-1", "-2h", "9223372036854775807", "999999999999999999999999", "1e20"} {
		if _, err := parseBudget(input); err == nil {
			t.Errorf("invalid budget %q accepted", input)
		}
	}
	for input, want := range map[string]time.Duration{"": 0, "0": 0, "-": 0, "60": time.Minute, "1h30m": 90 * time.Minute} {
		if got, err := parseBudget(input); err != nil || got != want {
			t.Errorf("%q: %v %v", input, got, err)
		}
	}
}
