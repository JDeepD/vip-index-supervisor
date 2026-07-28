package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// Screen is one layer of the UI. Screens are stacked; Esc pops back to the
// previous one.
type Screen interface {
	Init() tea.Cmd
	Update(msg tea.Msg) (Screen, tea.Cmd)
	View() string
	Title() string
}

// escOwner is a Screen that decides for itself what Esc does (e.g. the run
// dashboard, where backing out mid-run must not be a single accidental key).
type escOwner interface{ OwnsEsc() bool }

// ctrlCOwner is a Screen that intercepts Ctrl+C (the run dashboard turns it
// into a graceful stop before it ever means "quit").
type ctrlCOwner interface{ OwnsCtrlC() bool }

// sizable is a Screen that adapts to the terminal size.
type sizable interface{ SetSize(width, height int) }

// pushMsg and popMsg are how screens navigate. Only the root App touches the
// stack, so no screen can corrupt another's state.
type pushMsg struct{ screen Screen }
type popMsg struct{}

func push(s Screen) tea.Cmd { return func() tea.Msg { return pushMsg{s} } }
func pop() tea.Cmd          { return func() tea.Msg { return popMsg{} } }

// App is the root model: a screen stack plus terminal geometry.
type App struct {
	stack  []Screen
	width  int
	height int
}

// NewApp starts at the target-selection screen.
func NewApp() *App {
	return &App{stack: []Screen{newTargetScreen()}}
}

func (a *App) Init() tea.Cmd { return a.top().Init() }

func (a *App) top() Screen { return a.stack[len(a.stack)-1] }

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		for _, s := range a.stack {
			if sz, ok := s.(sizable); ok {
				sz.SetSize(msg.Width, msg.Height)
			}
		}
		return a, nil

	case pushMsg:
		a.stack = append(a.stack, msg.screen)
		if sz, ok := msg.screen.(sizable); ok {
			sz.SetSize(a.width, a.height)
		}
		return a, msg.screen.Init()

	case popMsg:
		if len(a.stack) > 1 {
			a.stack = a.stack[:len(a.stack)-1]
		}
		return a, nil

	case tea.KeyMsg:
		if cmd, handled := a.handleGlobalKey(msg); handled {
			return a, cmd
		}
	}

	top, cmd := a.top().Update(msg)
	a.stack[len(a.stack)-1] = top
	return a, cmd
}

func (a *App) handleGlobalKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch msg.String() {
	case "ctrl+c":
		if o, ok := a.top().(ctrlCOwner); ok && o.OwnsCtrlC() {
			return nil, false // the screen turns it into a graceful stop
		}
		return tea.Quit, true
	case "esc":
		if o, ok := a.top().(escOwner); ok && o.OwnsEsc() {
			return nil, false
		}
		if len(a.stack) > 1 {
			return pop(), true
		}
		return nil, true // nothing to go back to; swallow rather than glitch
	}
	return nil, false
}

func (a *App) View() string {
	var b strings.Builder
	b.WriteString(styleTitle.Render("vip-index supervisor"))
	b.WriteString(styleDim.Render("  ·  " + a.breadcrumb()))
	b.WriteString("\n\n")
	b.WriteString(a.top().View())
	return styleFrame.Render(b.String())
}

func (a *App) breadcrumb() string {
	titles := make([]string, 0, len(a.stack))
	for _, s := range a.stack {
		if t := s.Title(); t != "" {
			titles = append(titles, t)
		}
	}
	return strings.Join(titles, " › ")
}
