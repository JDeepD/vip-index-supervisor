package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/muesli/reflow/truncate"

	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
)

// runScreen is the live dashboard for a supervised run. The supervisor works
// in its own goroutine; this screen only consumes its event stream, so the UI
// can never corrupt the run.

type supervisorEventMsg struct {
	id     int64
	event  supervise.Event
	closed bool
}

type runScreen struct {
	id  int64
	cfg supervise.Config
	sup *supervise.Supervisor

	state    supervise.Snapshot
	log      []supervise.LogEvent
	logView  viewport.Model
	follow   bool // stick to the newest line unless the user scrolled up
	done     bool
	exitCode int
	doneMsg  string
	stops    int

	width  int
	height int
}

func newRunScreen(cfg supervise.Config) *runScreen {
	return &runScreen{
		id:      screenSerial.Add(1),
		cfg:     cfg,
		sup:     supervise.New(cfg),
		logView: viewport.New(76, 10),
		follow:  true,
		width:   80,
		height:  24,
	}
}

func (s *runScreen) Title() string { return "running" }

func (s *runScreen) SetSize(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	s.width, s.height = w, h
	s.logView.Width = max(40, w-6)
	s.logView.Height = s.logHeight()
	s.refreshLog()
}

// logHeight is whatever vertical space the fixed parts leave over, so a small
// terminal shows fewer lines instead of overflowing the screen.
func (s *runScreen) logHeight() int {
	reserved := 8 + len(s.state.Phases)
	return max(3, s.height-reserved)
}

// OwnsEsc while running: backing out of a live run must be a deliberate
// Ctrl+C, not one stray keypress.
func (s *runScreen) OwnsEsc() bool { return !s.done }

// OwnsCtrlC while running: the first Ctrl+C is a graceful stop, not a quit.
func (s *runScreen) OwnsCtrlC() bool { return !s.done }

func (s *runScreen) Init() tea.Cmd {
	go s.sup.Run(context.Background())
	return s.listen()
}

func (s *runScreen) listen() tea.Cmd {
	id, events := s.id, s.sup.Events()
	return func() tea.Msg {
		event, open := <-events
		if !open {
			return supervisorEventMsg{id: id, closed: true}
		}
		return supervisorEventMsg{id: id, event: event}
	}
}

func (s *runScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case supervisorEventMsg:
		if msg.id != s.id {
			return s, nil
		}
		if msg.closed {
			s.done = true
			return s, nil
		}
		s.apply(msg.event)
		return s, s.listen()
	case tea.KeyMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *runScreen) apply(event supervise.Event) {
	switch e := event.(type) {
	case supervise.ProgressEvent:
		s.state = e.State
	case supervise.LogEvent:
		s.log = append(s.log, e)
		if len(s.log) > 2000 {
			s.log = s.log[len(s.log)-2000:]
		}
		s.refreshLog()
	case supervise.DoneEvent:
		s.exitCode = e.ExitCode
		s.doneMsg = e.Message
	}
}

func (s *runScreen) refreshLog() {
	var b strings.Builder
	for _, e := range s.log {
		stamp := styleDim.Render(e.Time.Format(time.TimeOnly) + "  ")
		line := e.Message
		switch e.Level {
		case supervise.LevelOK:
			line = styleOK.Render(line)
		case supervise.LevelWarn:
			line = styleWarn.Render(line)
		case supervise.LevelError:
			line = styleErr.Render(line)
		}
		b.WriteString(truncateLine(stamp+line, s.logView.Width) + "\n")
	}
	s.logView.SetContent(b.String())
	if s.follow {
		s.logView.GotoBottom()
	}
}

func (s *runScreen) handleKey(key tea.KeyMsg) (Screen, tea.Cmd) {
	if s.done {
		switch key.String() {
		case "q", "ctrl+c":
			return s, tea.Quit
		case "enter", "esc":
			return s, pop()
		}
		return s.scroll(key)
	}
	if key.String() == "ctrl+c" {
		s.stops++
		// First Ctrl+C lets the child finish its batch and checkpoint; the
		// second stops asking nicely.
		s.sup.RequestStop(s.stops > 1)
		return s, nil
	}
	return s.scroll(key)
}

// scroll feeds navigation keys to the log viewport. Scrolling up detaches
// from the live tail; returning to the bottom re-attaches it.
func (s *runScreen) scroll(key tea.KeyMsg) (Screen, tea.Cmd) {
	switch key.String() {
	case "up", "down", "pgup", "pgdown", "home", "end", "k", "j":
		var cmd tea.Cmd
		s.logView, cmd = s.logView.Update(key)
		s.follow = s.logView.AtBottom()
		return s, cmd
	}
	return s, nil
}

func (s *runScreen) View() string {
	var b strings.Builder
	b.WriteString(s.headerView())
	b.WriteString(s.phasesView())
	b.WriteString(s.progressView())
	b.WriteString(s.renderLog())
	b.WriteString(s.helpView())
	return b.String()
}

func (s *runScreen) headerView() string {
	switch {
	case s.done && s.exitCode == 0:
		return styleOK.Render("✓ "+s.doneMsg) + "\n\n"
	case s.done:
		return styleErr.Render("✗ "+s.doneMsg) + "\n\n"
	case s.stops > 0:
		return styleWarn.Render("stopping — waiting for the current batch to checkpoint…") + "\n\n"
	default:
		return styleHeading.Render("Supervised indexing — "+s.cfg.Target.Label()) + "\n\n"
	}
}

func (s *runScreen) phasesView() string {
	var b strings.Builder
	for i, p := range s.state.Phases {
		marker, name := "  ○ ", p.Name
		switch p.Status {
		case supervise.PhaseComplete:
			marker = styleOK.Render("  ✓ ")
		case supervise.PhaseFailed:
			marker = styleErr.Render("  ✗ ")
		case supervise.PhaseRunning:
			marker = styleAccent.Render("  ▶ ")
			name = styleAccent.Render(name)
		default:
			marker = styleDim.Render("  ○ ")
			name = styleDim.Render(name)
		}
		b.WriteString(marker + name)
		if p.Version > 0 && p.Status != supervise.PhasePending {
			b.WriteString(styleDim.Render(fmt.Sprintf("  → v%d", p.Version)))
		}
		if i == s.state.Current && p.StatusNote != "" {
			b.WriteString(styleDim.Render("  · " + p.StatusNote))
		}
		b.WriteString("\n")
	}
	b.WriteString("\n")
	return b.String()
}

func (s *runScreen) progressView() string {
	if s.state.Current < 0 || s.state.Current >= len(s.state.Phases) {
		return ""
	}
	p := s.state.Phases[s.state.Current]
	var b strings.Builder

	barWidth := min(40, max(10, s.width-30))
	if p.Total > 0 {
		b.WriteString("  " + progressBar(p.Fraction(), barWidth) +
			fmt.Sprintf(" %5.1f%%  %s / %s\n", p.Fraction()*100, groupInt(p.Done), groupInt(p.Total)))
	} else {
		b.WriteString("  " + styleDim.Render("waiting for the first progress line…") + "\n")
	}

	details := fmt.Sprintf("  elapsed %s", formatDuration(p.Elapsed))
	if p.Rate > 0 {
		details += fmt.Sprintf("  ·  %.0f obj/s", p.Rate)
	}
	if p.ETA > 0 {
		details += "  ·  eta " + formatDuration(p.ETA)
	}
	if p.LastObjectID >= 0 {
		details += "  ·  last id " + groupInt(p.LastObjectID)
	}
	details += fmt.Sprintf("  ·  attempt %d", p.Attempt)
	if p.Restarts > 0 {
		details += fmt.Sprintf(" (%d restarts)", p.Restarts)
	}
	b.WriteString(styleDim.Render(details) + "\n\n")
	return b.String()
}

func (s *runScreen) renderLog() string {
	if s.logView.Height != s.logHeight() {
		s.logView.Height = s.logHeight()
	}
	scrollNote := ""
	if !s.follow {
		scrollNote = styleWarn.Render(fmt.Sprintf(" ── scrolled to %3.0f%% — end to re-follow ──", s.logView.ScrollPercent()*100)) + "\n"
	}
	return scrollNote + s.logView.View() + "\n"
}

func (s *runScreen) helpView() string {
	if s.done {
		return styleHelp.Render("↑/↓ scroll log · enter/esc back · q quit")
	}
	if s.stops > 0 {
		return styleHelp.Render("ctrl+c again to force-kill")
	}
	return styleHelp.Render("↑/↓ scroll log · ctrl+c stop gracefully (progress is checkpointed) · logs: " +
		s.cfg.StateDir)
}

// truncateLine cuts a styled line to the terminal width without slicing
// through an ANSI escape sequence — cutting mid-sequence leaks colour codes
// into everything rendered after it.
func truncateLine(line string, width int) string {
	if width < 4 {
		return line
	}
	return truncate.StringWithTail(line, uint(width), "…")
}
