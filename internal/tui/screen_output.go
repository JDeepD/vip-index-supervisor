package tui

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// outputScreen runs one read-only command in the background and shows its
// rendered result in a scrollable viewport.

var screenSerial atomic.Int64

type outputResultMsg struct {
	id   int64
	text string
}

type outputScreen struct {
	id      int64
	sess    *session
	action  string
	spin    spinner.Model
	vp      viewport.Model
	loading bool
	width   int
	height  int
}

func newOutputScreen(sess *session, action string) *outputScreen {
	sp := newSpinner()
	return &outputScreen{
		id:      screenSerial.Add(1),
		sess:    sess,
		action:  action,
		spin:    sp,
		vp:      viewport.New(80, 20),
		loading: true,
	}
}

func (s *outputScreen) Title() string { return s.action }

func (s *outputScreen) SetSize(w, h int) {
	// A zero size means the terminal has not reported yet; keep the defaults
	// rather than clamping the viewport down to a sliver.
	if w <= 0 || h <= 0 {
		return
	}
	s.width, s.height = w, h
	// The frame adds padding and the header/help lines eat rows; keeping the
	// viewport inside those bounds is what prevents scroll jitter.
	s.vp.Width = max(40, w-6)
	s.vp.Height = max(5, h-8)
}

func (s *outputScreen) Init() tea.Cmd {
	return tea.Batch(s.spin.Tick, s.fetch())
}

func (s *outputScreen) fetch() tea.Cmd {
	id, sess, action := s.id, s.sess, s.action
	return func() tea.Msg {
		ctx := context.Background()
		client := sess.client()
		var text string
		switch action {
		case "status":
			if st := client.Status(ctx); st != nil {
				text = renderStatus(st)
			} else {
				text = renderStatusUnavailable(client)
			}
		case "info":
			text = renderInfo(ctx, client)
		case "health":
			text = renderHealth(ctx, client)
		case "counts":
			text = renderCounts(client.ValidateCounts(ctx, false))
		case "stop":
			res := client.StopIndexing(ctx)
			text = strings.TrimSpace(res.Output)
			if text == "" {
				text = "Stop requested."
			}
		}
		return outputResultMsg{id: id, text: text}
	}
}

func (s *outputScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case outputResultMsg:
		if msg.id != s.id {
			return s, nil // a stale result from a screen the user already left
		}
		s.loading = false
		s.vp.SetContent(msg.text)
		return s, nil
	case spinner.TickMsg:
		if !s.loading {
			return s, nil
		}
		var cmd tea.Cmd
		s.spin, cmd = s.spin.Update(msg)
		return s, cmd
	case tea.KeyMsg:
		switch msg.String() {
		case "q":
			return s, tea.Quit
		case "r":
			if !s.loading {
				s.loading = true
				s.id = screenSerial.Add(1)
				return s, tea.Batch(s.spin.Tick, s.fetch())
			}
		}
	}
	var cmd tea.Cmd
	s.vp, cmd = s.vp.Update(msg)
	return s, cmd
}

func (s *outputScreen) View() string {
	if s.loading {
		return s.spin.View() + " running " + styleAccent.Render(s.action) + styleDim.Render(" — this can take a while") +
			styleHelp.Render("\n\nesc back · q quit")
	}
	return s.vp.View() + styleHelp.Render("\n↑/↓ scroll · r refresh · esc back · q quit")
}

// -- watch --------------------------------------------------------------------

// watchScreen polls indexing status until it goes idle. One fetch per tick:
// the payload drives both the display and the idle test.

type watchTickMsg struct{ id int64 }
type watchResultMsg struct {
	id   int64
	st   *vipsearch.IndexingStatus
	text string
}

type watchScreen struct {
	id       int64
	sess     *session
	interval time.Duration
	body     string
	idle     bool
	fetching bool
	lastPoll time.Time
}

func newWatchScreen(sess *session) *watchScreen {
	return &watchScreen{
		id:       screenSerial.Add(1),
		sess:     sess,
		interval: 30 * time.Second,
		body:     styleDim.Render("first poll running…"),
	}
}

func (s *watchScreen) Title() string { return "watch" }

func (s *watchScreen) Init() tea.Cmd { return s.poll() }

func (s *watchScreen) poll() tea.Cmd {
	s.fetching = true
	id, sess := s.id, s.sess
	return func() tea.Msg {
		client := sess.client()
		st := client.Status(context.Background())
		var text string
		if st != nil {
			text = renderStatus(st)
		} else {
			text = renderStatusUnavailable(client)
		}
		return watchResultMsg{id: id, st: st, text: text}
	}
}

func (s *watchScreen) scheduleTick() tea.Cmd {
	id := s.id
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return watchTickMsg{id: id} })
}

func (s *watchScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case watchResultMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.fetching = false
		s.lastPoll = time.Now()
		s.body = msg.text
		s.idle = msg.st != nil && !msg.st.Indexing
		if s.idle {
			return s, nil
		}
		return s, s.scheduleTick()
	case watchTickMsg:
		if msg.id != s.id || s.idle {
			return s, nil
		}
		if !s.fetching && time.Since(s.lastPoll) >= s.interval {
			return s, s.poll()
		}
		return s, s.scheduleTick()
	case tea.KeyMsg:
		if msg.String() == "q" {
			return s, tea.Quit
		}
	}
	return s, nil
}

func (s *watchScreen) View() string {
	var b strings.Builder
	if s.idle {
		b.WriteString(styleOK.Render("Indexing is idle.") + "\n\n")
	} else if s.fetching {
		b.WriteString(styleDim.Render("polling…") + "\n\n")
	} else {
		wait := s.interval - time.Since(s.lastPoll)
		if wait < 0 {
			wait = 0
		}
		b.WriteString(styleDim.Render("next poll in "+formatDuration(wait)) + "\n\n")
	}
	b.WriteString(s.body)
	b.WriteString(styleHelp.Render("\nesc back · q quit"))
	return b.String()
}

// -- unlock -------------------------------------------------------------------

// unlockScreen clears the stale index-lock transient — but checks the live
// status first. A hard-killed run leaves the "indexing" flag set too, so the
// user must explicitly force when the platform still claims a run is active.

type unlockStatusMsg struct {
	id      int64
	running bool
	known   bool
}
type unlockDoneMsg struct {
	id   int64
	ok   bool
	text string
}

type unlockScreen struct {
	id      int64
	sess    *session
	spin    spinner.Model
	stage   string // "checking" | "confirm" | "clearing" | "done"
	menu    *Menu
	message string
	ok      bool
}

func newUnlockScreen(sess *session) *unlockScreen {
	sp := newSpinner()
	return &unlockScreen{
		id:    screenSerial.Add(1),
		sess:  sess,
		spin:  sp,
		stage: "checking",
		menu: NewMenu([]MenuItem{
			{Value: "back", Label: "Go back", Desc: "leave the lock alone"},
			{Value: "force", Label: "Clear it anyway", Desc: "I am sure nothing is actually running"},
		}),
	}
}

func (s *unlockScreen) Title() string { return "unlock" }

func (s *unlockScreen) Init() tea.Cmd {
	id, sess := s.id, s.sess
	check := func() tea.Msg {
		st := sess.client().Status(context.Background())
		return unlockStatusMsg{id: id, running: st != nil && st.Indexing, known: st != nil}
	}
	return tea.Batch(s.spin.Tick, check)
}

// clear removes BOTH things a killed run leaves behind: the lock transient
// and the orphaned sync-state record. Clearing only the transient looks like
// it worked ("Index cleared.") while get-indexing-status keeps reporting a
// sync in flight and every later index run is refused.
func (s *unlockScreen) clear() tea.Cmd {
	s.stage = "clearing"
	id, sess := s.id, s.sess
	return func() tea.Msg {
		ctx := context.Background()
		client := sess.client()
		syncRes := client.ClearSyncRecord(ctx)
		client.ClearIndexLock(ctx)

		// Report what the platform says now, not what the commands claimed.
		st := client.Status(ctx)
		switch {
		case st != nil && !st.Indexing:
			return unlockDoneMsg{id: id, ok: true, text: "Lock and sync record cleared — indexing status is now idle."}
		case st == nil:
			return unlockDoneMsg{id: id, ok: false,
				text: "Commands ran, but the indexing status could not be read — verify before starting a run."}
		default:
			detail := strings.Join(syncRes.DescribeFailure(), "\n  ")
			wp := strings.Join(sess.target.BaseWP(), " ")
			return unlockDoneMsg{id: id, ok: false, text: "The platform STILL reports indexing in progress.\n" +
				"  If a sync is genuinely running, let it finish. If it is the debris of a killed run, clear it by hand:\n" +
				"    " + wp + " transient delete ep_wpcli_sync\n" +
				"    " + wp + " option delete ep_index_meta\n" +
				"    " + wp + " site option delete ep_index_meta\n" +
				"    " + wp + " cache delete alloptions options\n" +
				"    " + wp + " cache delete ep_index_meta options\n" +
				"  the last cleanup said:\n  " + detail}
		}
	}
}

func (s *unlockScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case unlockStatusMsg:
		if msg.id != s.id {
			return s, nil
		}
		if msg.running {
			s.stage = "confirm"
			return s, nil
		}
		return s, s.clear()
	case unlockDoneMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.stage = "done"
		s.message = msg.text
		s.ok = msg.ok
		return s, nil
	case spinner.TickMsg:
		if s.stage != "checking" && s.stage != "clearing" {
			return s, nil
		}
		var cmd tea.Cmd
		s.spin, cmd = s.spin.Update(msg)
		return s, cmd
	case tea.KeyMsg:
		if msg.String() == "q" {
			return s, tea.Quit
		}
		if s.stage == "confirm" && s.menu.Update(msg) {
			if s.menu.Selected().Value == "force" {
				return s, tea.Batch(s.spin.Tick, s.clear())
			}
			return s, pop()
		}
	}
	return s, nil
}

func (s *unlockScreen) View() string {
	switch s.stage {
	case "checking":
		return s.spin.View() + " checking whether an index is running…" + styleHelp.Render("\n\nesc back")
	case "clearing":
		return s.spin.View() + " clearing the index lock (delete-transient)…"
	case "confirm":
		return styleWarn.Render("Status reports indexing in progress.") + "\n" +
			styleDim.Render("A hard-killed run also leaves this flag set — but so does a genuinely running index.") + "\n\n" +
			s.menu.View() +
			styleHelp.Render("↑/↓ move · enter select · esc back")
	default:
		if !s.ok {
			return styleErr.Render("✗ ") + s.message + styleHelp.Render("\n\nesc back · q quit")
		}
		return styleOK.Render("✓ ") + s.message + styleHelp.Render("\n\nesc back · q quit")
	}
}
