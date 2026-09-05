package tui

import (
	"context"
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

type featuresLoadedMsg struct {
	id   int64
	rows []vipsearch.IndexingFeature
	err  error
}

type featureActivatedMsg struct {
	id     int64
	result vipsearch.RunResult
}

type featuresScreen struct {
	id       int64
	sess     *session
	stage    string // loading, list, confirm, guard, running, done
	spin     spinner.Model
	menu     *Menu
	confirm  *Menu
	rows     []vipsearch.IndexingFeature
	selected vipsearch.IndexingFeature
	guard    textinput.Model
	note     string
	result   vipsearch.RunResult
	output   viewport.Model
}

func newFeaturesScreen(sess *session) *featuresScreen {
	guard := textinput.New()
	guard.CharLimit, guard.Width = 100, 50
	return &featuresScreen{id: screenSerial.Add(1), sess: sess, stage: "loading",
		spin: newSpinner(), guard: guard, output: viewport.New(74, 8)}
}

func (s *featuresScreen) Title() string { return "features" }
func (s *featuresScreen) OwnsEsc() bool {
	return s.stage == "confirm" || s.stage == "guard" || s.stage == "running" || s.stage == "done"
}
func (s *featuresScreen) OwnsCtrlC() bool { return s.stage == "running" }
func (s *featuresScreen) SetSize(w, h int) {
	if w > 0 && h > 0 {
		s.output.Width, s.output.Height = max(20, w-6), max(3, h-14)
	}
}

func (s *featuresScreen) Init() tea.Cmd { return tea.Batch(s.spin.Tick, s.fetch()) }

func (s *featuresScreen) fetch() tea.Cmd {
	s.stage, s.note = "loading", ""
	s.id = screenSerial.Add(1)
	id, target := s.id, s.sess.target
	return func() tea.Msg {
		rows, err := vipsearch.NewClient(target).IndexingFeatures(context.Background())
		return featuresLoadedMsg{id: id, rows: rows, err: err}
	}
}

func (s *featuresScreen) activate() tea.Cmd {
	s.stage, s.note = "running", ""
	id, target, slug := s.id, s.sess.target, s.selected.Slug
	return func() tea.Msg {
		return featureActivatedMsg{id: id, result: vipsearch.NewClient(target).ActivateIndexingFeature(context.Background(), slug)}
	}
}

func (s *featuresScreen) Update(msg tea.Msg) (Screen, tea.Cmd) {
	switch msg := msg.(type) {
	case featuresLoadedMsg:
		if msg.id != s.id || s.stage != "loading" {
			return s, nil
		}
		s.stage, s.rows, s.menu = "list", msg.rows, nil
		if msg.err != nil {
			s.note = "Feature state is unknown: " + msg.err.Error()
			s.rows = nil
			return s, nil
		}
		var items []MenuItem
		for _, row := range s.rows {
			state := "inactive — enter to activate"
			if !row.Registered {
				state = "unavailable — not registered on this target"
			} else if row.Active {
				state = "active"
			}
			items = append(items, MenuItem{Value: row.Slug, Label: row.Slug + " — " + state, Desc: "indexable: " + row.Indexable})
		}
		s.menu = NewMenu(items)
		return s, nil
	case featureActivatedMsg:
		if msg.id != s.id || s.stage != "running" {
			return s, nil
		}
		s.stage, s.result = "done", msg.result
		body := strings.TrimSpace(vipsearch.StripANSI(msg.result.Output))
		if !msg.result.Succeeded() {
			body += "\n\n" + strings.Join(msg.result.DescribeFailure(), "\n")
		}
		s.output.SetContent(body)
		s.output.GotoTop()
		return s, nil
	case spinner.TickMsg:
		if s.stage != "loading" && s.stage != "running" {
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

func (s *featuresScreen) handleKey(key tea.KeyMsg) (Screen, tea.Cmd) {
	if s.stage == "running" {
		return s, nil // keep the command's result visible; never abandon a mutation
	}
	if key.String() == "q" && s.stage != "guard" {
		return s, tea.Quit
	}
	switch s.stage {
	case "list":
		if key.String() == "r" {
			return s, tea.Batch(s.spin.Tick, s.fetch())
		}
		if len(s.rows) == 0 || s.menu == nil || !s.menu.Update(key) {
			return s, nil
		}
		s.selected = s.rows[s.menu.cursor]
		if !s.selected.Registered || s.selected.Active {
			return s, nil
		}
		s.stage, s.note = "confirm", ""
		s.confirm = NewMenu([]MenuItem{{Value: "cancel", Label: "Cancel — change nothing"},
			{Value: "activate", Label: "Activate " + s.selected.Slug}})
	case "confirm":
		if key.String() == "esc" {
			s.stage = "list"
			return s, nil
		}
		if !s.confirm.Update(key) {
			return s, nil
		}
		if s.confirm.Selected().Value == "cancel" {
			s.stage = "list"
			return s, nil
		}
		if s.sess.target.LooksLikeProduction() {
			s.stage = "guard"
			s.guard.SetValue("")
			return s, s.guard.Focus()
		}
		return s, tea.Batch(s.spin.Tick, s.activate())
	case "guard":
		switch key.String() {
		case "esc":
			s.stage, s.note = "confirm", ""
			return s, nil
		case "enter":
			if strings.TrimSpace(s.guard.Value()) == s.sess.target.AppEnv {
				return s, tea.Batch(s.spin.Tick, s.activate())
			}
			s.note = "That does not match " + s.sess.target.AppEnv
			return s, nil
		}
		var cmd tea.Cmd
		s.guard, cmd = s.guard.Update(key)
		return s, cmd
	case "done":
		if key.String() == "enter" || key.String() == "esc" {
			return s, tea.Batch(s.spin.Tick, s.fetch())
		}
		var cmd tea.Cmd
		s.output, cmd = s.output.Update(key)
		return s, cmd
	}
	return s, nil
}

func (s *featuresScreen) View() string {
	var b strings.Builder
	b.WriteString(styleHeading.Render("Indexing features — "+s.sess.target.Label()) + "\n\n")
	switch s.stage {
	case "loading":
		b.WriteString(s.spin.View() + " reading registered and active features…\n")
	case "list":
		b.WriteString(styleDim.Render("Activation changes site settings. It does not start an indexing run.") + "\n\n")
		if s.menu != nil {
			b.WriteString(s.menu.View())
		}
		b.WriteString(styleHelp.Render("↑/↓ move · enter activate · r refresh · esc back · q quit"))
	case "confirm", "guard":
		b.WriteString(fmt.Sprintf("Activate %s to enable the %s indexable?\n\n", s.selected.Slug, s.selected.Indexable))
		b.WriteString(styleAccent.Render("$ "+strings.Join(append(s.sess.target.Base(), "activate-feature", s.selected.Slug), " ")) + "\n\n")
		b.WriteString(styleWarn.Render("This changes feature settings on the selected target.\nActivation requires idle indexing; no locks will be cleared.") + "\n\n")
		if s.stage == "confirm" {
			b.WriteString(s.confirm.View())
		} else {
			b.WriteString("Production target — type " + s.sess.target.AppEnv + " to confirm:\n\n" + s.guard.View() + "\n")
		}
		b.WriteString(styleHelp.Render("enter select/confirm · esc back"))
	case "running":
		b.WriteString(s.spin.View() + " checking, activating, and verifying " + s.selected.Slug + "…\n")
	case "done":
		if s.result.Succeeded() {
			b.WriteString(styleOK.Render("✓ Feature active and indexable registered.") + "\n")
			b.WriteString("Return to Index and select " + s.selected.Indexable + " when ready to index it.\n\n")
		} else {
			b.WriteString(styleErr.Render("Activation could not be confirmed; inspect the output before retrying.") + "\n\n")
		}
		b.WriteString(s.output.View() + "\n" + styleHelp.Render("↑/↓ scroll · enter/esc refresh features · q quit"))
	}
	if s.note != "" {
		b.WriteString("\n\n" + styleWarn.Render(s.note))
	}
	return b.String()
}
