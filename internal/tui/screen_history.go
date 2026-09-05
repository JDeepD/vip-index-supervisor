package tui

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
)

type historyResultMsg struct {
	id       int64
	runs     []supervise.RunRecord
	warnings []string
	err      error
}

type historyScreen struct {
	id      int64
	sess    *session
	dir     string
	runs    []supervise.RunRecord
	cursor  int
	message string
	loading bool
	height  int
}

func newHistoryScreen(sess *session) *historyScreen {
	return &historyScreen{id: screenSerial.Add(1), sess: sess, dir: sess.config().StateDir, height: 24}
}
func (s *historyScreen) Title() string { return "run history" }
func (s *historyScreen) SetSize(w, h int) {
	if h > 0 {
		s.height = h
	}
}
func (s *historyScreen) Init() tea.Cmd    { return s.refresh() }
func (s *historyScreen) Resumed() tea.Cmd { return s.refresh() }
func (s *historyScreen) refresh() tea.Cmd {
	s.id, s.loading = screenSerial.Add(1), true
	id, dir, target := s.id, s.dir, s.sess.target
	return func() tea.Msg {
		all, warnings, err := supervise.ListRuns(dir)
		var runs []supervise.RunRecord
		for _, r := range all {
			if r.Config.Target == target {
				runs = append(runs, r)
			}
		}
		return historyResultMsg{id: id, runs: runs, warnings: warnings, err: err}
	}
}
func (s *historyScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case historyResultMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.loading, s.runs, s.message = false, msg.runs, ""
		if msg.err != nil {
			s.message = "Could not read history: " + msg.err.Error()
		}
		if len(msg.warnings) > 0 {
			s.message += fmt.Sprintf("\n%d unreadable history entry(s); recovery will require investigation.", len(msg.warnings))
		}
		s.cursor = max(0, min(s.cursor, len(s.runs)-1))
	case tea.KeyMsg:
		switch msg.String() {
		case "r":
			return s, s.refresh()
		case "d":
			in := newInputScreen("history directory", "State directory to inspect", "Reads local run history; does not start indexing.", s.dir)
			in.submit = func(value string) tea.Cmd {
				if value != "" {
					if absolute, err := filepath.Abs(value); err == nil {
						s.dir = absolute
					}
				}
				return pop()
			}
			return s, push(in)
		case "up", "k":
			s.cursor = max(0, s.cursor-1)
		case "down", "j":
			s.cursor = min(max(0, len(s.runs)-1), s.cursor+1)
		case "enter":
			if !s.loading && len(s.runs) > 0 {
				return s, push(newRecoveryScreen(s.sess, s.dir, s.runs[s.cursor].ID))
			}
		}
	}
	return s, nil
}
func (s *historyScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Saved runs — "+s.sess.target.Label()) + "\n")
	b.WriteString(styleDim.Render(s.dir) + "\n\n")
	if s.loading {
		b.WriteString("Loading local history…\n")
	} else if len(s.runs) == 0 {
		b.WriteString("No saved runs for this target in this directory.\nOlder checkpoint-only runs cannot be reconstructed automatically.\n")
	}
	rows := max(1, (s.height-12)/3)
	start := max(0, s.cursor-rows+1)
	for i := start; i < min(len(s.runs), start+rows); i++ {
		r := s.runs[i]
		outcome := r.Outcome
		if outcome == "running" {
			outcome = "unfinished — check recovery"
		}
		line := fmt.Sprintf("%s  %s  %s", r.StartedAt.Local().Format("2006-01-02 15:04:05"), outcome, r.Config.Strategy)
		if i == s.cursor {
			line = styleCursor.Render("❯ " + line)
		} else {
			line = "  " + line
		}
		b.WriteString(line + "\n" + styleDim.Render("  "+strings.Join(r.Config.Indexables, ", ")+" · "+r.ID) + "\n\n")
	}
	if s.message != "" {
		b.WriteString(styleWarn.Render(s.message) + "\n")
	}
	b.WriteString(styleHelp.Render("↑/↓ select · enter details & recovery · r refresh · d change directory · esc back"))
	return b.String()
}

type recoveryResultMsg struct {
	id     int64
	run    supervise.RunRecord
	report supervise.RecoveryReport
	err    error
}

type recoveryScreen struct {
	id         int64
	sess       *session
	dir, runID string
	run        supervise.RunRecord
	report     supervise.RecoveryReport
	stage      string
	vp         viewport.Model
	inspect    func(context.Context, supervise.RunRecord) supervise.RecoveryReport
}

func newRecoveryScreen(sess *session, dir, id string) *recoveryScreen {
	s := &recoveryScreen{id: screenSerial.Add(1), sess: sess, dir: dir, runID: id, vp: viewport.New(76, 16)}
	s.inspect = func(ctx context.Context, r supervise.RunRecord) supervise.RecoveryReport {
		return supervise.InspectRecovery(ctx, r, sess.client())
	}
	return s
}
func (s *recoveryScreen) Title() string { return "recovery" }
func (s *recoveryScreen) SetSize(w, h int) {
	if w > 0 && h > 0 {
		s.vp.Width, s.vp.Height = max(30, w-6), max(4, h-10)
	}
}
func (s *recoveryScreen) Init() tea.Cmd    { return s.refresh() }
func (s *recoveryScreen) Resumed() tea.Cmd { return s.refresh() }
func (s *recoveryScreen) refresh() tea.Cmd {
	s.id, s.stage = screenSerial.Add(1), "loading"
	id, dir, runID, target, inspect := s.id, s.dir, s.runID, s.sess.target, s.inspect
	return func() tea.Msg {
		r, err := supervise.LoadRun(dir, runID)
		if err != nil {
			return recoveryResultMsg{id: id, err: err}
		}
		if r.Config.Target != target {
			return recoveryResultMsg{id: id, err: fmt.Errorf("saved target does not match this session")}
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		return recoveryResultMsg{id: id, run: r, report: inspect(ctx, r)}
	}
}
func (s *recoveryScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case recoveryResultMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.stage, s.run, s.report = "report", msg.run, msg.report
		if msg.err != nil {
			s.report.CanResume = false
			s.vp.SetContent("Recovery unavailable: " + msg.err.Error())
		} else {
			s.vp.SetContent(renderRecovery(msg.run, msg.report))
		}
		s.vp.GotoTop()
		return s, nil
	case tea.KeyMsg:
		switch msg.String() {
		case "r":
			if s.stage != "loading" {
				return s, s.refresh()
			}
		case "enter":
			if s.stage == "report" && s.report.CanResume {
				s.stage = "confirm"
				s.vp.SetContent(s.resumePrompt())
				s.vp.GotoTop()
				return s, nil
			}
			if s.stage == "confirm" && s.report.CanResume {
				cfg, err := supervise.ResumeConfig(s.run, s.sess.notifications)
				if err != nil {
					s.stage = "report"
					s.report.CanResume = false
					s.vp.SetContent(err.Error())
					return s, nil
				}
				return s, push(newRunScreen(cfg))
			}
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}
func (s *recoveryScreen) View() string {
	if s.stage == "loading" {
		return "Checking saved state, remote status, and versions (read-only)…\n\n" + styleHelp.Render("esc back")
	}
	if s.stage == "confirm" {
		return s.vp.View() + "\n\n" + styleHelp.Render("enter resume · ↑/↓ scroll · r re-check · esc back")
	}
	help := "↑/↓ scroll · r re-check · esc back"
	if s.report.CanResume {
		help = "enter resume this run · " + help
	}
	return s.vp.View() + "\n\n" + styleHelp.Render(help)
}

func (s *recoveryScreen) resumePrompt() string {
	var b strings.Builder
	b.WriteString("Resume this saved run?\n\n")
	if s.run.Config.AggressiveRecovery {
		b.WriteString("WARNING: This run uses AGGRESSIVE stale-lock recovery.\nUnchanged CLI state may be cleared while a worker is alive.\nResuming preserves this setting; continue only if you accept that risk.\n\n")
	}
	fmt.Fprintf(&b, "Target: %s\nOriginal strategy: %s\nPost types: %s\nPer page: %d\nBudget: %s (fresh allowance for this resume)\nState: %s\n\n",
		s.run.Config.Target.Label(), s.run.Config.Strategy, displayPostTypes(s.run.Config.PostTypes), s.run.Config.PerPage, displayBudget(s.run.Config.MaxDuration), s.run.Config.StateDir)
	for _, p := range s.run.Phases {
		if p.Status == supervise.PhaseComplete {
			fmt.Fprintf(&b, "%s: completed — skip\n", p.Name)
			continue
		}
		fmt.Fprintf(&b, "%s: saved version %d · checkpoint %s\n", p.Name, p.Version, savedCheckpoint(p.LastObjectID))
	}
	b.WriteString("\nCompleted phases will be skipped. Saved versions and checkpoints will be used.\n" +
		"No --setup will be repeated. No lock is cleared by the recovery checks.\n" +
		"Remote state will be checked again before indexing starts.\n" +
		"Current session notification settings will be used.\n")
	return b.String()
}

func displayPostTypes(value string) string {
	if value == "" {
		return "all"
	}
	return value
}
func displayBudget(value time.Duration) string {
	if value <= 0 {
		return "unlimited"
	}
	return value.String()
}

func renderRecovery(r supervise.RunRecord, report supervise.RecoveryReport) string {
	var b strings.Builder
	b.WriteString(report.Verdict + "\n\n")
	fmt.Fprintf(&b, "Run: %s\nTarget: %s\nStrategy: %s · post types: %s · per page: %d\nOutcome: %s · last saved: %s\n",
		r.ID, r.Config.Target.Label(), r.Config.Strategy, displayPostTypes(r.Config.PostTypes), r.Config.PerPage, r.Outcome, r.UpdatedAt.Local().Format(time.RFC3339))
	if r.Outcome == "running" {
		b.WriteString("Unfinished record: this alone does not prove that the remote worker stopped.\n")
	}
	if r.Message != "" {
		b.WriteString("\nResult: " + r.Message + "\n")
	}
	if r.LastError != "" {
		b.WriteString("Last recorded error: " + r.LastError + "\n")
	}
	b.WriteString("\nSaved phases:\n")
	storeDir := r.Config.StateDir
	for _, p := range r.Phases {
		state := "pending / interrupted"
		if p.Status == supervise.PhaseComplete {
			state = "completed — will skip"
		} else if p.IndexingComplete {
			state = "indexing finished — completion checks pending"
		}
		fmt.Fprintf(&b, "  %s · v%d · %s\n  checkpoint: %s · progress: %s / %s · attempts: %d · retries: %d\n",
			p.Name, p.Version, state, savedCheckpoint(p.LastObjectID), groupInt(p.Done), groupInt(p.Total), p.Attempt, p.Restarts)
		if p.Version == 0 {
			b.WriteString("  Version not yet selected; the saved strategy applies to this untouched phase.\n")
		}
		if pin := report.Pins[p.Name]; pin > 0 {
			fmt.Fprintf(&b, "  local pinned version: v%d\n", pin)
		}
		for _, v := range report.Versions[p.Name] {
			if v.Active || v.Number == p.Version {
				fmt.Fprintf(&b, "  remote v%d: active=%t · documents=%s\n", v.Number, v.Active, groupInt(v.Documents))
			}
		}
	}
	b.WriteString("\nRemote: ")
	if report.Remote == nil {
		b.WriteString("unknown\n")
	} else if report.Remote.Indexing {
		b.WriteString("indexing\n")
	} else {
		b.WriteString("reports idle (not a guarantee that the transient lock is absent)\n")
	}
	if report.Remote != nil && report.Remote.CurrentSync != nil {
		cur := report.Remote.CurrentSync
		fmt.Fprintf(&b, "  reported phase: %s · last object ID: %s (informational, not a resume checkpoint)\n", cur.Indexable, savedCheckpoint(cur.LastObjectID))
	}
	for _, reason := range report.Reasons {
		b.WriteString("\n• " + reason + "\n")
	}
	if report.CanResume {
		b.WriteString("\nNext: Resume this saved run. Do not rebuild merely because a lock error occurred.\n")
		b.WriteString("A phase without a checkpoint starts from the top without --setup.\n")
		b.WriteString("Review the last error first; resuming does not repair invalid settings.\n")
		b.WriteString("Do not reuse the checkpoint if this index was manually rebuilt outside the supervisor.\n")
	} else {
		b.WriteString("\nNext: address the reasons above, then refresh. The recovery assistant never clears remote state.\n")
	}
	fmt.Fprintf(&b, "\nRecent attempt history (%d older entries omitted):\n", r.OmittedAttempts)
	for _, a := range r.Attempts[max(0, len(r.Attempts)-10):] {
		fmt.Fprintf(&b, "  %s · %s v%d · attempt %d · %s · checkpoint %s\n", a.StartedAt.Local().Format("01-02 15:04:05"), a.Phase, a.Version, a.Number, a.Outcome, savedCheckpoint(a.Checkpoint))
		if a.LogPath != "" {
			b.WriteString("    log: " + a.LogPath + "\n")
		}
	}
	b.WriteString("\nLogs: " + filepath.Join(storeDir, "logs") + "\n")
	return b.String()
}
func savedCheckpoint(id int64) string {
	if id <= 0 {
		return "none"
	}
	return groupInt(id)
}
