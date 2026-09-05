package tui

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/notify"
	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// session is the state the wizard accumulates, shared by pointer across
// screens. Each screen fills in its part; the confirm screen turns it into a
// supervise.Config.
type session struct {
	target vipsearch.Target

	indexables            []string
	postTypes             string
	strategy              supervise.Strategy
	intoVersion           int // set when strategy is StrategyIntoVersion
	perPage               int
	showErrors            bool
	maxDuration           time.Duration
	notifications         notify.Config
	notificationLoadError string

	// advanced options; zero values mean "use the defaults"
	resumeFrom         int64
	stateDir           string
	stallTimeout       time.Duration
	ignoreLock         bool
	aggressiveRecovery bool
}

func newSession() *session {
	return &session{indexables: []string{"post"}, perPage: 350, notifications: notify.Config{RetryAlerts: true}}
}

// Disk-backed preferences are loaded only when the user creates a session,
// not by the supervisor or tests constructing a plain session.
func newConfiguredSession() *session {
	s := newSession()
	path, err := notify.SettingsPath()
	if err == nil {
		s.notifications, err = notify.Load(path)
	}
	if err != nil {
		s.notificationLoadError = "Saved notification settings could not be loaded; notifications are off. Open notifications to configure them."
	}
	return s
}

func (s *session) client() *vipsearch.Client { return vipsearch.NewClient(s.target) }

func (s *session) config() supervise.Config {
	cfg := supervise.Config{
		Target:             s.target,
		Indexables:         s.indexables,
		PostTypes:          s.postTypes,
		PerPage:            s.perPage,
		Strategy:           s.strategy,
		IntoVersion:        s.intoVersion,
		ResumeFrom:         s.resumeFrom,
		ShowErrors:         s.showErrors,
		MaxDuration:        s.maxDuration,
		StateDir:           s.stateDir,
		StallTimeout:       s.stallTimeout,
		IgnoreLock:         s.ignoreLock,
		AggressiveRecovery: s.aggressiveRecovery,
		Notifications:      s.notifications,
	}
	cfg.Normalize()
	return cfg
}

// previewCommand is the index command one phase would run, shown before
// anything executes so the flags are learnable rather than hidden.
func (s *session) previewCommand(indexable string) string {
	parts := append(s.target.Base(),
		"index",
		"--indexables="+indexable,
		"--per-page="+strconv.Itoa(s.perPage),
		"--skip-confirm",
	)
	if indexable == "post" && s.postTypes != "" {
		parts = append(parts, "--post-type="+s.postTypes)
	}
	if s.showErrors {
		parts = append(parts, "--show-errors")
	}
	switch s.strategy {
	case supervise.StrategySetup:
		parts = append(parts, "--setup")
	case supervise.StrategyNewVersion:
		parts = append(parts, "--version=<new>")
	case supervise.StrategyIntoVersion:
		parts = append(parts, "--version="+strconv.Itoa(s.intoVersion))
	}
	return strings.Join(parts, " ")
}

// parseBudget reads "90m", "6h", "1h30m" or plain seconds. Zero means
// unlimited; an error means the text was not a duration.
func parseBudget(text string) (time.Duration, error) {
	text = strings.TrimSpace(strings.ToLower(text))
	if text == "" || text == "-" {
		return 0, nil
	}
	if secs, err := strconv.Atoi(text); err == nil {
		if secs < 0 || int64(secs) > math.MaxInt64/int64(time.Second) {
			return 0, fmt.Errorf("time budget is negative or too large")
		}
		return time.Duration(secs) * time.Second, nil
	}
	duration, err := time.ParseDuration(text)
	if err == nil && duration < 0 {
		err = fmt.Errorf("time budget cannot be negative")
	}
	return duration, err
}
