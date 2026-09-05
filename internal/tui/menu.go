package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

// MenuItem is one selectable row: a value the code uses, a label the user
// reads, and an optional description shown dimmed beneath it.
type MenuItem struct {
	Value string
	Label string
	Desc  string
}

// Menu is a single-select list driven by arrow keys. It is deliberately
// hand-rolled: full control over rendering is what keeps the UI glitch-free.
type Menu struct {
	Items  []MenuItem
	cursor int
}

func NewMenu(items []MenuItem) *Menu { return &Menu{Items: items} }

// Update moves the cursor. It reports whether enter was pressed.
func (m *Menu) Update(msg tea.KeyMsg) (chosen bool) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.Items)-1 {
			m.cursor++
		}
	case "home", "g":
		m.cursor = 0
	case "end", "G":
		m.cursor = len(m.Items) - 1
	case "enter":
		return true
	}
	return false
}

// Selected is the item under the cursor.
func (m *Menu) Selected() MenuItem { return m.Items[m.cursor] }

func (m *Menu) View() string {
	return m.ViewWindow(len(m.Items))
}

// ViewWindow keeps the selected action visible in a short terminal.
func (m *Menu) ViewWindow(rows int) string {
	var b strings.Builder
	rows = max(1, rows)
	start := max(0, m.cursor-rows+1)
	for i := start; i < min(len(m.Items), start+rows); i++ {
		item := m.Items[i]
		cursor, label := "  ", item.Label
		if i == m.cursor {
			cursor = styleCursor.Render("❯ ")
			label = styleCursor.Render(item.Label)
		}
		b.WriteString(cursor + label + "\n")
		if item.Desc != "" {
			b.WriteString(styleDim.Render("    "+item.Desc) + "\n")
		}
	}
	return b.String()
}

// MultiSelect is a menu where space toggles rows and enter confirms.
type MultiSelect struct {
	Items    []MenuItem
	cursor   int
	selected map[int]bool
}

func NewMultiSelect(items []MenuItem, preselected ...string) *MultiSelect {
	sel := make(map[int]bool)
	for i, item := range items {
		for _, v := range preselected {
			if item.Value == v {
				sel[i] = true
			}
		}
	}
	return &MultiSelect{Items: items, selected: sel}
}

// Update handles navigation and toggling. It reports whether enter was
// pressed with at least one row selected.
func (m *MultiSelect) Update(msg tea.KeyMsg) (confirmed bool) {
	switch msg.String() {
	case "up", "k":
		if m.cursor > 0 {
			m.cursor--
		}
	case "down", "j":
		if m.cursor < len(m.Items)-1 {
			m.cursor++
		}
	case " ":
		m.selected[m.cursor] = !m.selected[m.cursor]
	case "a":
		all := len(m.Selected()) < len(m.Items)
		for i := range m.Items {
			m.selected[i] = all
		}
	case "enter":
		return len(m.Selected()) > 0
	}
	return false
}

// Selected returns the chosen values in display order.
func (m *MultiSelect) Selected() []string {
	var out []string
	for i, item := range m.Items {
		if m.selected[i] {
			out = append(out, item.Value)
		}
	}
	return out
}

func (m *MultiSelect) View() string {
	var b strings.Builder
	for i, item := range m.Items {
		cursor := "  "
		if i == m.cursor {
			cursor = styleCursor.Render("❯ ")
		}
		box := "[ ] "
		label := item.Label
		if m.selected[i] {
			box = styleSelected.Render("[✓] ")
		}
		if i == m.cursor {
			label = styleCursor.Render(label)
		}
		b.WriteString(cursor + box + label)
		if item.Desc != "" {
			b.WriteString(styleDim.Render("  — " + item.Desc))
		}
		b.WriteString("\n")
	}
	return b.String()
}
