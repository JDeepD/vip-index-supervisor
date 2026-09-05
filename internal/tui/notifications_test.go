package tui

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/notify"
)

func TestNotificationSettingsAreOptInAndTokenMasked(t *testing.T) {
	sess := newSession()
	s := newNotificationsScreen(sess)
	s.path = filepath.Join(t.TempDir(), "notifications.json")
	s.form.fields[notifyEndpoint].text = "https://example.com/secret-topic"
	s.form.fields[notifyToken].text = "secret-token"
	if strings.Contains(s.View(), "secret-token") {
		t.Fatal("token displayed")
	}
	cfg, err := s.readConfig()
	if err != nil {
		t.Fatal(err)
	}
	if !s.apply(cfg) || sess.config().Notifications != cfg {
		t.Fatal("settings not propagated to run")
	}
	if _, err := os.Stat(s.path); !os.IsNotExist(err) {
		t.Fatal("settings saved without opt-in")
	}
	s.form.fields[notifyRemember].on = true
	if !s.apply(cfg) {
		t.Fatal(s.message)
	}
	if saved, err := notify.Load(s.path); err != nil || saved != cfg {
		t.Fatalf("settings not remembered: %v", err)
	}
	s.form.fields[notifyEndpoint].text = ""
	disabled, err := s.readConfig()
	if err != nil || disabled.Token != "" {
		t.Fatal("disabling kept token")
	}
	if !s.apply(disabled) || sess.config().Notifications.Endpoint != "" {
		t.Fatal("disable did not take effect")
	}
}

func TestNotificationTestButtonUsesOnlyLocalHTTP(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "did not run a VIP command") {
			t.Errorf("unexpected test body: %q", body)
		}
	}))
	defer server.Close()
	sess := newSession()
	s := newNotificationsScreen(sess)
	s.form.fields[notifyEndpoint].text = server.URL + "/test"
	s.form.cursor = len(s.form.fields) + 1
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if cmd == nil || !s.testing {
		t.Fatal("test did not start")
	}
	// Bubble Tea's batch also includes a spinner tick; execute only the
	// finite commands returned by this test and deliver their messages.
	batch, ok := cmd().(tea.BatchMsg)
	if !ok {
		t.Fatal("missing command batch")
	}
	for _, command := range batch {
		if msg := command(); msg != nil {
			s.Update(msg)
		}
	}
	if s.testing || s.failed || requests.Load() != 1 {
		t.Fatalf("test result: %s", s.message)
	}
	if sess.notifications.Endpoint != "" {
		t.Fatal("test changed settings before Apply")
	}
}

func TestNotificationInvalidURLAndFailedSave(t *testing.T) {
	sess := newSession()
	s := newNotificationsScreen(sess)
	s.form.fields[notifyEndpoint].text = "https://example.com/"
	s.form.cursor = len(s.form.fields)
	_, cmd := s.Update(tea.KeyMsg{Type: tea.KeyEnter})
	if !s.failed || cmd != nil || sess.notifications.Endpoint != "" {
		t.Fatal("invalid URL applied")
	}
	s.path = ""
	s.form.fields[notifyRemember].on = true
	if s.apply(notify.Config{Endpoint: "https://example.com/topic"}) {
		t.Fatal("save succeeded without a settings path")
	}
	if sess.notifications.Endpoint != "" {
		t.Fatal("failed save partially applied settings")
	}
}

func TestOptionsKeepBudgetWhenReturningToNotificationSettings(t *testing.T) {
	sess := newSession()
	sess.maxDuration = 90 * time.Minute
	s := newOptionsScreen(sess)
	if !s.apply() || sess.maxDuration != 90*time.Minute {
		t.Fatal("revisiting options cleared the budget")
	}
}
