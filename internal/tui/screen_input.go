package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// inputScreen is a single-question text prompt. Enter submits, Esc goes back
// (handled by the root App).
type inputScreen struct {
	title    string
	question string
	hint     string
	input    textinput.Model
	errText  string
	// validate returns "" when the value is acceptable.
	validate func(string) string
	// submit turns the accepted value into the next navigation step.
	submit func(string) tea.Cmd
}

func newInputScreen(title, question, hint, initial string) *inputScreen {
	ti := textinput.New()
	ti.SetValue(initial)
	ti.CursorEnd()
	ti.Focus()
	ti.CharLimit = 200
	ti.Width = 60
	return &inputScreen{title: title, question: question, hint: hint, input: ti}
}

func (s *inputScreen) Title() string { return s.title }

func (s *inputScreen) Init() tea.Cmd { return textinput.Blink }

func (s *inputScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == "enter" {
		value := strings.TrimSpace(s.input.Value())
		if s.validate != nil {
			if s.errText = s.validate(value); s.errText != "" {
				return s, nil
			}
		}
		return s, s.submit(value)
	}
	var cmd tea.Cmd
	s.input, cmd = s.input.Update(msg)
	if s.errText != "" && s.validate != nil {
		// Live re-validation clears the error as soon as the value is fixed,
		// instead of leaving a stale complaint under a now-valid input.
		s.errText = s.validate(strings.TrimSpace(s.input.Value()))
	}
	return s, cmd
}

func (s *inputScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render(s.question) + "\n\n")
	b.WriteString(s.input.View() + "\n")
	if s.errText != "" {
		b.WriteString(styleErr.Render("  ✗ "+s.errText) + "\n")
	}
	if s.hint != "" {
		b.WriteString(styleDim.Render(s.hint) + "\n")
	}
	b.WriteString(styleHelp.Render("enter confirm · esc back"))
	return b.String()
}
