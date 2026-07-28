package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdeepd/vip-index-supervisor/internal/supervise"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// The versions flow: pick an indexable, browse its versions, then act on one —
// activate it, delete it, or build (resume indexing) into it.

// -- indexable picker ---------------------------------------------------------

func newVersionsIndexableScreen(sess *session) *versionsIndexableScreen {
	return &versionsIndexableScreen{sess: sess, menu: NewMenu([]MenuItem{
		{Value: "post", Label: "post"},
		{Value: "term", Label: "term", Desc: "needs the 'terms' feature active"},
		{Value: "user", Label: "user", Desc: "needs the 'users' feature active"},
		{Value: "comment", Label: "comment", Desc: "needs the 'comments' feature active"},
	})}
}

type versionsIndexableScreen struct {
	sess *session
	menu *Menu
}

func (s *versionsIndexableScreen) Title() string { return "versions" }
func (s *versionsIndexableScreen) Init() tea.Cmd { return nil }

func (s *versionsIndexableScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if key.String() == "q" {
		return s, tea.Quit
	}
	if s.menu.Update(key) {
		return s, push(newVersionListScreen(s.sess, s.menu.Selected().Value))
	}
	return s, nil
}

func (s *versionsIndexableScreen) View() string {
	return styleHeading.Render("Versions of which indexable?") + "\n\n" +
		s.menu.View() +
		styleHelp.Render("↑/↓ move · enter select · esc back · q quit")
}

// -- version list -------------------------------------------------------------

type versionsLoadedMsg struct {
	id      int64
	rows    []vipsearch.IndexVersion
	failure []string
}

type versionListScreen struct {
	id        int64
	sess      *session
	indexable string
	spin      spinner.Model
	loading   bool
	rows      []vipsearch.IndexVersion
	failure   []string
	cursor    int
}

func newVersionListScreen(sess *session, indexable string) *versionListScreen {
	sp := newSpinner()
	return &versionListScreen{
		id:        screenSerial.Add(1),
		sess:      sess,
		indexable: indexable,
		spin:      sp,
		loading:   true,
	}
}

func (s *versionListScreen) Title() string { return s.indexable }

func (s *versionListScreen) Init() tea.Cmd {
	return tea.Batch(s.spin.Tick, s.fetch())
}

// Resumed refetches whenever this screen surfaces again: a screen above it
// may have activated or deleted a version, and showing yesterday's list next
// to a "deleted ✓" the user just saw is worse than a brief spinner.
func (s *versionListScreen) Resumed() tea.Cmd {
	if s.loading {
		return nil
	}
	return tea.Batch(s.spin.Tick, s.fetch())
}

func (s *versionListScreen) fetch() tea.Cmd {
	s.loading = true
	id, sess, indexable := s.id, s.sess, s.indexable
	return func() tea.Msg {
		client := sess.client()
		rows := client.Versions(context.Background(), indexable)
		var failure []string
		if len(rows) == 0 {
			failure = client.LastRun.DescribeFailure()
		}
		return versionsLoadedMsg{id: id, rows: rows, failure: failure}
	}
}

func (s *versionListScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case versionsLoadedMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.loading = false
		s.rows, s.failure = msg.rows, msg.failure
		if s.cursor >= len(s.rows) {
			s.cursor = max(0, len(s.rows)-1)
		}
		return s, nil
	case spinner.TickMsg:
		if !s.loading {
			return s, nil
		}
		var cmd tea.Cmd
		s.spin, cmd = s.spin.Update(msg)
		return s, cmd
	case tea.KeyMsg:
		return s.handleKey(msg)
	}
	return s, nil
}

func (s *versionListScreen) handleKey(key tea.KeyMsg) (Screen, tea.Cmd) {
	switch key.String() {
	case "q":
		return s, tea.Quit
	case "r":
		if !s.loading {
			return s, tea.Batch(s.spin.Tick, s.fetch())
		}
	case "up", "k":
		if s.cursor > 0 {
			s.cursor--
		}
	case "down", "j":
		if s.cursor < len(s.rows)-1 {
			s.cursor++
		}
	case "enter":
		if !s.loading && len(s.rows) > 0 {
			return s, push(newVersionActionScreen(s.sess, s.indexable, s.rows, s.cursor))
		}
	}
	return s, nil
}

func (s *versionListScreen) View() string {
	if s.loading {
		return s.spin.View() + " loading index versions for " + styleAccent.Render(s.indexable) + "…" +
			styleHelp.Render("\n\nesc back · q quit")
	}
	var b strings.Builder
	b.WriteString(styleHeading.Render("Index versions — "+s.indexable) + "\n\n")

	if len(s.rows) == 0 {
		b.WriteString(styleWarn.Render("  (none parsed)") + "\n")
		for _, line := range s.failure {
			b.WriteString(styleDim.Render("    "+line) + "\n")
		}
		b.WriteString(styleHelp.Render("r retry · esc back · q quit"))
		return b.String()
	}

	b.WriteString(styleDim.Render("    "+padRight("version", 9)+padRight("state", 10)+padLeft("documents", 15)) + "\n")
	for i, v := range s.rows {
		cursor := "  "
		if i == s.cursor {
			cursor = styleCursor.Render("❯ ")
		}
		state := styleDim.Render("—")
		if v.Active {
			state = styleOK.Render("● active")
		}
		docs := groupInt(v.Documents)
		if v.Active && v.Documents == 0 {
			docs = styleErr.Render(docs + "  ⚠ EMPTY")
		}
		num := fmt.Sprintf("v%d", v.Number)
		if i == s.cursor {
			num = styleCursor.Render(padRight(num, 9))
		} else {
			num = padRight(num, 9)
		}
		b.WriteString("  " + cursor + num + padRight(state, 10) + padLeft(docs, 15) + "\n")
	}
	b.WriteString(styleHelp.Render("↑/↓ move · enter actions · r refresh · esc back · q quit"))
	return b.String()
}

// -- per-version action menu --------------------------------------------------

type versionActionScreen struct {
	sess      *session
	indexable string
	rows      []vipsearch.IndexVersion
	selected  vipsearch.IndexVersion
	menu      *Menu
}

func newVersionActionScreen(sess *session, indexable string, rows []vipsearch.IndexVersion, cursor int) *versionActionScreen {
	selected := rows[cursor]
	active := vipsearch.ActiveVersion(rows)

	var items []MenuItem
	if !selected.Active {
		items = append(items, MenuItem{
			Value: "activate",
			Label: fmt.Sprintf("Activate v%d", selected.Number),
			Desc:  "make this version serve search" + comparisonNote(selected, active),
		})
	}
	items = append(items, MenuItem{
		Value: "build",
		Label: fmt.Sprintf("Build into v%d", selected.Number),
		Desc:  "run supervised indexing into this version (no activation)",
	})
	if !selected.Active {
		items = append(items, MenuItem{
			Value: "delete",
			Label: fmt.Sprintf("Delete v%d", selected.Number),
			Desc:  "permanently remove the version and its " + groupInt(selected.Documents) + " documents",
		})
	}
	return &versionActionScreen{
		sess: sess, indexable: indexable, rows: rows, selected: selected,
		menu: NewMenu(items),
	}
}

func comparisonNote(selected vipsearch.IndexVersion, active *vipsearch.IndexVersion) string {
	if active == nil || active.Number == selected.Number {
		return ""
	}
	return fmt.Sprintf(" (%s docs vs %s in active v%d)",
		groupInt(selected.Documents), groupInt(active.Documents), active.Number)
}

func (s *versionActionScreen) Title() string { return fmt.Sprintf("v%d", s.selected.Number) }
func (s *versionActionScreen) Init() tea.Cmd { return nil }

func (s *versionActionScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	key, ok := msg.(tea.KeyMsg)
	if !ok {
		return s, nil
	}
	if key.String() == "q" {
		return s, tea.Quit
	}
	if !s.menu.Update(key) {
		return s, nil
	}
	switch s.menu.Selected().Value {
	case "activate":
		return s, push(newVersionMutateScreen(s.sess, s.indexable, s.rows, s.selected, mutateActivate))
	case "delete":
		return s, push(newVersionMutateScreen(s.sess, s.indexable, s.rows, s.selected, mutateDelete))
	case "build":
		return s, s.startBuildWizard()
	}
	return s, nil
}

// startBuildWizard reuses the index wizard from the post-types step onward,
// with the target version fixed to the selected one.
func (s *versionActionScreen) startBuildWizard() tea.Cmd {
	s.sess.indexables = []string{s.indexable}
	s.sess.strategy = supervise.StrategyIntoVersion
	s.sess.intoVersion = s.selected.Number
	if s.indexable == "post" {
		return push(newPostTypesScreen(s.sess, func(sess *session) Screen {
			return newOptionsScreen(sess)
		}))
	}
	return push(newOptionsScreen(s.sess))
}

func (s *versionActionScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render(fmt.Sprintf("v%d of %s", s.selected.Number, s.indexable)))
	if s.selected.Active {
		b.WriteString("  " + styleOK.Render("● active"))
	}
	b.WriteString("\n")
	b.WriteString(styleDim.Render(fmt.Sprintf("  %s documents", groupInt(s.selected.Documents))))
	if s.selected.Created != "" {
		b.WriteString(styleDim.Render("  · created " + s.selected.Created))
	}
	b.WriteString("\n\n")
	b.WriteString(s.menu.View())
	b.WriteString(styleHelp.Render("↑/↓ move · enter select · esc back"))
	return b.String()
}

// -- activate / delete execution ----------------------------------------------

type mutateKind int

const (
	mutateActivate mutateKind = iota
	mutateDelete
)

type mutateDoneMsg struct {
	id      int64
	ok      bool
	message string
}

// versionMutateScreen confirms and executes an activate or delete. Both are
// irreversible in effect, and every command carries --skip-confirm, so the
// confirmation has to happen here or it happens nowhere. Against a
// production-looking environment it additionally requires typing the
// environment name.
type versionMutateScreen struct {
	id        int64
	sess      *session
	indexable string
	kind      mutateKind
	selected  vipsearch.IndexVersion
	active    *vipsearch.IndexVersion
	menu      *Menu
	guard     textinput.Model
	guardErr  string
	stage     string // "confirm" | "guard" | "running" | "done"
	spin      spinner.Model
	ok        bool
	message   string
}

func newVersionMutateScreen(sess *session, indexable string, rows []vipsearch.IndexVersion, selected vipsearch.IndexVersion, kind mutateKind) *versionMutateScreen {
	sp := newSpinner()
	verb := "Activate"
	if kind == mutateDelete {
		verb = "Delete"
	}
	ti := textinput.New()
	ti.CharLimit = 100
	ti.Width = 50
	return &versionMutateScreen{
		id:        screenSerial.Add(1),
		sess:      sess,
		indexable: indexable,
		kind:      kind,
		selected:  selected,
		active:    vipsearch.ActiveVersion(rows),
		spin:      sp,
		guard:     ti,
		stage:     "confirm",
		menu: NewMenu([]MenuItem{
			{Value: "cancel", Label: "Cancel", Desc: "change nothing"},
			{Value: "go", Label: fmt.Sprintf("%s v%d", verb, selected.Number)},
		}),
	}
}

func (s *versionMutateScreen) Title() string {
	if s.kind == mutateDelete {
		return "delete"
	}
	return "activate"
}

func (s *versionMutateScreen) Init() tea.Cmd { return nil }

// OwnsEsc everywhere except the initial confirm: while running, Esc must not
// abandon the screen that will report the outcome; in the guard it cancels
// back to confirm; and after completion it must take the same
// back-past-the-stale-action-menu route as enter — a plain single pop would
// resurface an action menu for a version that may no longer exist.
func (s *versionMutateScreen) OwnsEsc() bool { return s.stage != "confirm" }

func (s *versionMutateScreen) execute() tea.Cmd {
	s.stage = "running"
	id, sess, indexable, kind, version := s.id, s.sess, s.indexable, s.kind, s.selected.Number
	return tea.Batch(s.spin.Tick, func() tea.Msg {
		client := sess.client()
		var res vipsearch.RunResult
		if kind == mutateActivate {
			res = client.ActivateVersion(context.Background(), indexable, version)
		} else {
			res = client.DeleteVersion(context.Background(), indexable, version)
		}
		if !res.Succeeded() {
			return mutateDoneMsg{id: id, ok: false,
				message: strings.Join(res.DescribeFailure(), "\n  ")}
		}
		return mutateDoneMsg{id: id, ok: true, message: strings.TrimSpace(res.Output)}
	})
}

func (s *versionMutateScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case mutateDoneMsg:
		if msg.id != s.id {
			return s, nil
		}
		s.stage = "done"
		s.ok, s.message = msg.ok, msg.message
		return s, nil
	case spinner.TickMsg:
		if s.stage != "running" {
			return s, nil
		}
		var cmd tea.Cmd
		s.spin, cmd = s.spin.Update(msg)
		return s, cmd
	case tea.KeyMsg:
		return s.handleKey(msg)
	}
	if s.stage == "guard" {
		var cmd tea.Cmd
		s.guard, cmd = s.guard.Update(msg)
		return s, cmd
	}
	return s, nil
}

func (s *versionMutateScreen) handleKey(key tea.KeyMsg) (Screen, tea.Cmd) {
	switch s.stage {
	case "confirm":
		if key.String() == "q" {
			return s, tea.Quit
		}
		if !s.menu.Update(key) {
			return s, nil
		}
		if s.menu.Selected().Value == "cancel" {
			return s, pop()
		}
		if s.sess.target.LooksLikeProduction() {
			s.stage = "guard"
			s.guard.Focus()
			return s, textinput.Blink
		}
		return s, s.execute()
	case "guard":
		switch key.String() {
		case "esc":
			s.stage = "confirm"
			return s, nil
		case "enter":
			if strings.TrimSpace(s.guard.Value()) == s.sess.target.AppEnv {
				return s, s.execute()
			}
			s.guardErr = "that does not match " + s.sess.target.AppEnv
			return s, nil
		}
		var cmd tea.Cmd
		s.guard, cmd = s.guard.Update(key)
		s.guardErr = ""
		return s, cmd
	case "done":
		switch key.String() {
		case "q":
			return s, tea.Quit
		case "enter", "esc":
			// Back to the list, past the action menu whose version row is now
			// stale; the list refetches itself on resurfacing (Resumed).
			return s, tea.Sequence(pop(), pop())
		}
	}
	return s, nil
}

func (s *versionMutateScreen) warnings() []string {
	var warns []string
	if s.kind == mutateActivate {
		if s.selected.Documents == 0 {
			warns = append(warns, "this version holds 0 documents — search would return NOTHING")
		} else if s.active != nil && s.active.Documents > 0 {
			ratio := float64(s.selected.Documents) / float64(s.active.Documents)
			if ratio < 0.9 {
				warns = append(warns, fmt.Sprintf(
					"it holds only %.0f%% of the documents in active v%d — an incomplete build?",
					ratio*100, s.active.Number))
			}
		}
		if s.active != nil && s.selected.Number < s.active.Number {
			warns = append(warns, fmt.Sprintf(
				"v%d is older than the active v%d — this is a rollback", s.selected.Number, s.active.Number))
		}
	} else {
		warns = append(warns, "deletion is permanent — the version and its documents cannot be recovered")
	}
	return warns
}

func (s *versionMutateScreen) View() string {
	verb := "Activate"
	if s.kind == mutateDelete {
		verb = "Delete"
	}
	var b strings.Builder
	b.WriteString(styleHeading.Render(fmt.Sprintf("%s v%d of %s", verb, s.selected.Number, s.indexable)) + "\n\n")
	b.WriteString(fmt.Sprintf("  this version   %s documents\n", groupInt(s.selected.Documents)))
	if s.active != nil && s.active.Number != s.selected.Number {
		b.WriteString(fmt.Sprintf("  active now     v%d — %s documents\n", s.active.Number, groupInt(s.active.Documents)))
	}
	for _, w := range s.warnings() {
		b.WriteString(styleWarn.Render("  ⚠ "+w) + "\n")
	}
	b.WriteString("\n")

	switch s.stage {
	case "confirm":
		b.WriteString(s.menu.View())
		b.WriteString(styleHelp.Render("↑/↓ move · enter select · esc back"))
	case "guard":
		b.WriteString(styleErr.Render("  production environment — type its name to confirm") + "\n\n")
		b.WriteString(s.guard.View() + "\n")
		if s.guardErr != "" {
			b.WriteString(styleErr.Render("  ✗ "+s.guardErr) + "\n")
		}
		b.WriteString(styleHelp.Render("enter confirm · esc cancel"))
	case "running":
		b.WriteString(s.spin.View() + " " + strings.ToLower(verb) + " running…")
	case "done":
		if s.ok {
			b.WriteString(styleOK.Render("✓ ") + s.message + "\n")
		} else {
			b.WriteString(styleErr.Render("✗ the command failed:") + "\n  " + s.message + "\n")
		}
		b.WriteString(styleHelp.Render("enter/esc back to the list · q quit"))
	}
	return b.String()
}
