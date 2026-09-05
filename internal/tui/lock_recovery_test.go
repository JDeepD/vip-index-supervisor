package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
)

func TestAggressiveRecoveryIsExplicitAndVisible(t *testing.T) {
	sess := newSession()
	if sess.config().AggressiveRecovery {
		t.Fatal("aggressive recovery defaulted on")
	}
	s := newAdvancedScreen(sess)
	s.form.fields[advAggressiveRecovery].on = true
	if !strings.Contains(s.View(), "LIVE worker") {
		t.Fatal("risk not displayed beside opt-in")
	}
	if !s.apply() || !sess.config().AggressiveRecovery {
		t.Fatal("explicit option not applied")
	}
	confirm := newConfirmScreen(sess)
	if !strings.Contains(confirm.View(), "AGGRESSIVE") {
		t.Fatal("confirmation hides aggressive mode")
	}
	sess.target.AppEnv = "@example-app.production"
	if !confirm.needsProductionGuard() {
		t.Fatal("production aggressive recovery lacks confirmation guard")
	}
	_, cmd := confirm.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !confirm.guardOpen || cmd == nil {
		t.Fatal("production guard did not open")
	}
	if strings.Contains(confirm.View(), "--setup will DELETE") {
		t.Fatal("resume guard incorrectly claims setup")
	}
	s.form.fields[advIgnoreLock].on = true
	if s.apply() {
		t.Fatal("aggressive recovery allowed without local lock")
	}
}

func TestSavedAggressiveRecoveryWarnsBeforeResume(t *testing.T) {
	s := newRecoveryScreen(newSession(), t.TempDir(), "unused")
	s.run.Config = supervise.Config{AggressiveRecovery: true}
	if !strings.Contains(s.resumePrompt(), "Resuming preserves this setting") {
		t.Fatal("saved aggressive setting silently reused")
	}
}
