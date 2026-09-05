package tui

import (
	"strings"
	"testing"

	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func TestDashboardDisplaysCurrentMemoryOnItsOwnLine(t *testing.T) {
	s := newRunScreen(supervise.Config{StateDir: t.TempDir()})
	initialHeight := s.logHeight()
	s.apply(supervise.ProgressEvent{State: supervise.Snapshot{Current: 0,
		Phases: []supervise.PhaseSnapshot{{Name: "post", Attempt: 1, MemoryUsage: "171.99 MB", LastObjectID: -1}},
	}})
	view := vipsearch.StripANSI(s.progressView())
	if !strings.Contains(view, "\n  memory 171.99 MB\n") || strings.Contains(view, "Peak") {
		t.Fatalf("current memory not separately displayed: %s", view)
	}
	if s.logHeight() != initialHeight-2 { // one phase row and one memory row
		t.Fatal("dashboard did not reserve memory-line space")
	}
	s.state.Phases[0].MemoryUsage = ""
	view = vipsearch.StripANSI(s.progressView())
	if !strings.Contains(view, "memory —") || strings.Contains(view, "171.99") {
		t.Fatalf("missing reading shown as old or zero usage: %s", view)
	}
}
