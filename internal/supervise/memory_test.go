package supervise

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func TestMemoryTelemetryDoesNotAdvanceIndexing(t *testing.T) {
	s := testSupervisor(t)
	for _, tc := range []struct{ line, want string }{
		{"Memory Usage: 171.99mb (Peak: 173.43mb)", "171.99 MB"},
		{"Warning: ACF using it wrong", "171.99 MB"},
		{`#0 callback("Memory Usage: 999mb")`, "171.99 MB"},
		{"Memory Usage: 180.29mb (Peak: 181.77mb)", "180.29 MB"},
		{"Memory Usage: 150.25mb (Peak: 181.77mb)", "150.25 MB"},
	} {
		var outcome attemptOutcome
		s.consumeLine("post", 1, tc.line, &outcome)
		p := s.phases[0]
		if p.MemoryUsage != tc.want {
			t.Fatalf("latest current usage=%q want=%q", p.MemoryUsage, tc.want)
		}
		if outcome.progressed || outcome.success || outcome.lockError || outcome.fatal != "" ||
			p.Done != vipsearch.NoValue || p.Total != vipsearch.NoValue || p.LastObjectID != vipsearch.NoValue || p.NotifiedPercent != 0 || len(s.samples) != 0 {
			t.Fatalf("telemetry changed indexing state: %+v %+v", p, outcome)
		}
	}
	var outcome attemptOutcome
	s.consumeLine("post", 1, "Processed 300/1000. Last Object ID: 700", &outcome)
	if s.phases[0].MemoryUsage != "150.25 MB" || !outcome.progressed {
		t.Fatal("normal progress lost latest memory reading")
	}
	s.nextAttempt()
	if s.phases[0].MemoryUsage != "" {
		t.Fatal("new attempt inherited a previous worker's memory")
	}
	s.phases[0].MemoryUsage = "123 MB"
	s.beginPhase(0)
	if s.phases[0].MemoryUsage != "" {
		t.Fatal("new phase inherited memory")
	}
}

func TestMemoryOnlyOutputDoesNotResetRecoveryBudget(t *testing.T) {
	s := testSupervisor(t)
	attempts := 0
	s.attempt = func(context.Context, string, int, []string) attemptOutcome {
		attempts++
		o := attemptOutcome{lockError: true}
		s.consumeLine("post", 1, "Memory Usage: 171.99mb (Peak: 173.43mb)", &o)
		return o
	}
	if s.runPhase(context.Background(), "post") || attempts != 6 {
		t.Fatalf("memory renewed recovery budget: attempts=%d", attempts)
	}
	if s.phases[0].MemoryUsage != "" {
		t.Fatal("failed attempt left current memory on dashboard")
	}
}

func TestMemoryIsNotPersistedForResume(t *testing.T) {
	p := PhaseSnapshot{Name: "post", MemoryUsage: "171.99 MB"}
	data, err := json.Marshal(p)
	if err != nil || strings.Contains(string(data), "171.99") {
		t.Fatalf("live telemetry persisted: %s %v", data, err)
	}
	var restored PhaseSnapshot
	if err := json.Unmarshal(data, &restored); err != nil || restored.MemoryUsage != "" {
		t.Fatalf("old worker's memory restored: %+v %v", restored, err)
	}
}

func TestMemoryFromFakeCLIReachesDashboardEvents(t *testing.T) {
	s := helperSupervisor(t, "memory")
	s.attempt = s.runAttempt
	if !s.runPhase(context.Background(), "post") {
		t.Fatal("fake CLI did not complete")
	}
	seen := make(map[string]bool)
	for len(s.events) > 0 {
		if event, ok := (<-s.events).(ProgressEvent); ok {
			seen[event.State.Phases[0].MemoryUsage] = true
		}
	}
	if !seen["171.99 MB"] || !seen["176.08 MB"] || seen["999 MB"] {
		t.Fatalf("wrong memory events: %v", seen)
	}
	if s.phases[0].MemoryUsage != "" {
		t.Fatal("completed worker still shown as current")
	}
}
