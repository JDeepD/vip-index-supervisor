package tui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

func loadedFeaturesScreen() *featuresScreen {
	s := newFeaturesScreen(newSession())
	s.Update(featuresLoadedMsg{id: s.id, rows: []vipsearch.IndexingFeature{
		{Slug: "users", Indexable: "user", Registered: true},
		{Slug: "terms", Indexable: "term", Registered: true, Active: true},
		{Slug: "comments", Indexable: "comment"},
	}})
	return s
}

func TestFeaturesAvailableFromActionMenuAndIndexablePicker(t *testing.T) {
	sess := newSession()
	s := newActionScreen(sess)
	found := false
	for i, item := range s.menu.Items {
		if item.Value == "features" {
			s.menu.cursor, found = i, true
		}
	}
	if !found {
		t.Fatal("feature action missing")
	}
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil {
		t.Fatal("features action did not open a screen")
	}
	if _, ok := cmd().(pushMsg).screen.(*featuresScreen); !ok {
		t.Fatal("features action started something other than feature management")
	}
	picker := newIndexablesScreen(sess)
	_, cmd = picker.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'f'}})
	if cmd == nil {
		t.Fatal("feature shortcut missing")
	}
	if _, ok := cmd().(pushMsg).screen.(*featuresScreen); !ok || !reflect.DeepEqual(sess.indexables, []string{"post"}) {
		t.Fatal("opening feature management changed indexing selection")
	}
	// Do not call Init: these checks only navigate, never run a real CLI.
}

func TestFeaturesRequireExplicitConfirmation(t *testing.T) {
	s := loadedFeaturesScreen()
	for _, want := range []string{"users — inactive", "terms — active", "comments — unavailable"} {
		if !strings.Contains(s.View(), want) {
			t.Fatalf("missing %q: %s", want, s.View())
		}
	}
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || s.stage != "confirm" {
		t.Fatal("selection immediately activated a feature")
	}
	if s.confirm.Selected().Value != "cancel" || !strings.Contains(s.View(), "wp vip-search activate-feature users") {
		t.Fatal("confirmation did not default to cancel or show the exact command")
	}
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || s.stage != "list" {
		t.Fatal("default confirmation mutated the target")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.OwnsEsc() {
		t.Fatal("confirmation escape bypasses feature list")
	}
	s.Update(tea.KeyMsg{Type: tea.KeyEsc})
	if s.stage != "list" {
		t.Fatal("escape did not cancel confirmation")
	}
	for _, cursor := range []int{1, 2} {
		s.menu.cursor = cursor
		if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || s.stage != "list" {
			t.Fatal("active or unavailable feature offered activation")
		}
	}
}

func TestFeatureActivationProductionGuardAndMutationCannotBeAbandoned(t *testing.T) {
	s := loadedFeaturesScreen()
	s.sess.target.AppEnv = "@example-app.production" // display only; commands are never executed
	s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	s.confirm.cursor = 1
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.stage != "guard" || cmd == nil || !strings.Contains(s.View(), "@example-app.production") {
		t.Fatal("production activation skipped typed confirmation")
	}
	s.guard.SetValue("wrong")
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || s.stage != "guard" {
		t.Fatal("incorrect target name accepted")
	}
	s.guard.SetValue(s.sess.target.AppEnv)
	_, cmd = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if s.stage != "running" || cmd == nil || !s.OwnsEsc() || !s.OwnsCtrlC() {
		t.Fatal("confirmed activation not protected while running")
	}
	// The command is intentionally NOT executed against the displayed target.
	app := &App{stack: []Screen{s}}
	for _, key := range []tea.KeyMsg{{Type: tea.KeyEsc}, {Type: tea.KeyCtrlC}, {Type: tea.KeyEnter}, {Type: tea.KeyRunes, Runes: []rune{'q'}}} {
		if _, cmd := app.Update(key); cmd != nil || s.stage != "running" {
			t.Fatal("mutation could be abandoned or submitted twice")
		}
	}
	s.Update(featureActivatedMsg{id: s.id - 1, result: vipsearch.RunResult{Output: "Success: Feature activated"}})
	if s.stage != "running" {
		t.Fatal("stale activation result accepted")
	}
	s.Update(featureActivatedMsg{id: s.id, result: vipsearch.RunResult{Output: "Success: Feature activated\nWarning: requires a re-index"}})
	if s.stage != "done" || !strings.Contains(s.View(), "Feature active and indexable registered") || !strings.Contains(s.View(), "requires a re-index") {
		t.Fatal("result or CLI warning lost")
	}
	if s.OwnsCtrlC() || !reflect.DeepEqual(s.sess.indexables, []string{"post"}) {
		t.Fatal("activation changed indexing selection or kept quit blocked")
	}
	_, cmd = s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || s.stage != "loading" {
		t.Fatal("completed activation did not refresh feature state")
	}
}

func TestUnknownFeatureStateAndStaleMessagesCannotActivate(t *testing.T) {
	s := newFeaturesScreen(newSession())
	if _, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter}); cmd != nil || s.stage != "loading" {
		t.Fatal("activation before feature state loaded")
	}
	s.Update(featuresLoadedMsg{id: s.id - 1, rows: []vipsearch.IndexingFeature{{Slug: "users", Registered: true}}})
	if s.stage != "loading" {
		t.Fatal("stale list accepted")
	}
	s.Update(featuresLoadedMsg{id: s.id, err: errors.New("unrecognized output")})
	if !strings.Contains(s.View(), "unknown") {
		t.Fatal("unknown status hidden")
	}
	for _, key := range []tea.KeyType{tea.KeyUp, tea.KeyDown, tea.KeyEnter} {
		if _, cmd := s.Update(tea.KeyMsg{Type: key}); cmd != nil || s.stage != "list" {
			t.Fatal("unknown/empty feature list allowed activation")
		}
	}
	s.stage = "running"
	s.Update(featuresLoadedMsg{id: s.id, rows: []vipsearch.IndexingFeature{{Slug: "users", Registered: true}}})
	if s.stage != "running" {
		t.Fatal("late list abandoned mutation")
	}
	s.Update(featureActivatedMsg{id: s.id, result: vipsearch.RunResult{Output: "Success: Feature activated", Err: errors.New("verification failed")}})
	if s.result.Succeeded() || !strings.Contains(s.View(), "could not be confirmed") || !strings.Contains(s.View(), "verification failed") {
		t.Fatal("acknowledgement hid verification failure")
	}
}
