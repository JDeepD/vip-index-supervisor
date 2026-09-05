package tui

import (
	"context"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/notify"
)

const (
	notifyEndpoint = iota
	notifyToken
	notifyRetries
	notifyRemember
)

type notificationTestMsg struct {
	id  int64
	err error
}

type notificationsScreen struct {
	id      int64
	sess    *session
	form    *form
	path    string
	message string
	failed  bool
	testing bool
	spin    spinner.Model
}

func newNotificationsScreen(sess *session) *notificationsScreen {
	path, _ := notify.SettingsPath()
	return &notificationsScreen{id: screenSerial.Add(1), sess: sess, path: path, spin: newSpinner(), form: &form{
		fields: []formField{
			{label: "ntfy topic URL (blank disables)", kind: kindText, text: sess.notifications.Endpoint, placeholder: "https://ntfy.sh/your-private-topic"},
			{label: "Access token (optional)", kind: kindText, text: sess.notifications.Token, secret: true},
			{label: "Include retry alerts (max 1/min/phase)", kind: kindToggle, on: sess.notifications.RetryAlerts},
			{label: "Remember on this computer", kind: kindToggle},
		}, actions: []string{"Apply ✓", "Send test"},
	}}
}

func (s *notificationsScreen) Title() string   { return "notifications" }
func (s *notificationsScreen) Init() tea.Cmd   { return nil }
func (s *notificationsScreen) OwnsEsc() bool   { return s.testing }
func (s *notificationsScreen) OwnsCtrlC() bool { return s.testing }

func (s *notificationsScreen) readConfig() (notify.Config, error) {
	cfg := notify.Config{Endpoint: strings.TrimSpace(s.form.fields[notifyEndpoint].text),
		Token: strings.TrimSpace(s.form.fields[notifyToken].text), RetryAlerts: s.form.fields[notifyRetries].on}
	if cfg.Endpoint == "" {
		cfg.Token = ""
	}
	return cfg, cfg.Validate()
}

func (s *notificationsScreen) apply(cfg notify.Config) bool {
	if s.form.fields[notifyRemember].on {
		if s.path == "" {
			s.message = "Could not locate the local configuration directory."
			s.failed = true
			return false
		}
		if err := notify.Save(s.path, cfg); err != nil {
			s.message = "Could not save notification settings: " + err.Error()
			s.failed = true
			return false
		}
	}
	s.sess.notifications = cfg
	s.sess.notificationLoadError = ""
	return true
}

func (s *notificationsScreen) test(cfg notify.Config) tea.Cmd {
	s.testing = true
	id := s.id
	return tea.Batch(s.spin.Tick, func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		publisher, err := notify.NewPublisher(cfg)
		if err == nil {
			err = publisher.Publish(ctx, notify.Message{Title: "Index supervisor: test", Body: "Phone notifications are configured. This test did not run a VIP command.", Priority: 3, Tags: "test_tube"})
		}
		return notificationTestMsg{id: id, err: err}
	})
}

func (s *notificationsScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case notificationTestMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.testing = false
		s.failed = msg.err != nil
		if msg.err != nil {
			s.message = "Test failed: " + msg.err.Error()
		} else {
			s.message = "ntfy accepted the test message. Check your iPhone, then Apply to use these settings."
		}
		return s, nil
	case spinner.TickMsg:
		if !s.testing {
			return s, nil
		}
		var cmd tea.Cmd
		s.spin, cmd = s.spin.Update(msg)
		return s, cmd
	case tea.KeyMsg:
		if s.testing {
			return s, nil
		}
		action := s.form.Update(msg)
		if action == "" {
			return s, nil
		}
		cfg, err := s.readConfig()
		if err != nil {
			s.message = err.Error()
			s.failed = true
			return s, nil
		}
		if action == "Send test" {
			if cfg.Endpoint == "" {
				s.message = "Enter a topic URL before sending a test."
				s.failed = true
				return s, nil
			}
			return s, s.test(cfg)
		}
		if !s.apply(cfg) {
			return s, nil
		}
		return s, pop()
	}
	return s, nil
}

func (s *notificationsScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Phone notifications via ntfy") + "\n\n")
	b.WriteString(s.form.View())
	b.WriteString(styleDim.Render("Subscribe to the same server/topic in the ntfy iPhone app.\nUse a private, unguessable topic or an authenticated server.\nAlerts include the environment and phase, not raw CLI output.\nProgress: 25%, 50%, 75%, and verified 100% per phase; no periodic heartbeat.\n"))
	if s.form.fields[notifyRemember].on {
		b.WriteString(styleWarn.Render("Endpoint and token will be saved locally, NOT encrypted.\nUnix file permissions are owner-only; protect the configuration file on Windows.\n"))
	} else {
		b.WriteString(styleDim.Render("Apply affects this session only; any previously saved settings remain unchanged.\n"))
	}
	if s.testing {
		b.WriteString("\n" + s.spin.View() + " sending test…\n")
	}
	if s.message != "" {
		style := styleOK
		if s.failed {
			style = styleErr
		}
		b.WriteString("\n" + style.Render(s.message) + "\n")
	}
	b.WriteString(styleHelp.Render("↑/↓ move · type to edit · ctrl+u clear field · space toggle · enter select · esc back"))
	return b.String()
}
