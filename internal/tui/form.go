package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
)

type fieldKind int

const (
	kindText fieldKind = iota
	kindToggle
)

type formField struct {
	label       string
	kind        fieldKind
	text        string
	placeholder string // shown dimmed when a text field is empty
	on          bool
	secret      bool // hide access tokens in the rendered form
}

// form is a column of editable fields followed by action rows, under one
// cursor. Enter on a field advances to the next row — never submits — so
// walking a form with enter reads every field before reaching an action;
// only enter on an action row triggers it.
type form struct {
	fields  []formField
	actions []string
	cursor  int
}

func (f *form) rowCount() int { return len(f.fields) + len(f.actions) }

func (f *form) focusedField() *formField {
	if f.cursor < len(f.fields) {
		return &f.fields[f.cursor]
	}
	return nil
}

// FocusField moves the cursor to a field by index (e.g. onto a failed one).
func (f *form) FocusField(i int) {
	if i >= 0 && i < len(f.fields) {
		f.cursor = i
	}
}

// Update handles one key. It returns the action label when enter (or space)
// lands on an action row, otherwise "".
func (f *form) Update(key tea.KeyMsg) string {
	switch key.String() {
	case "up", "shift+tab":
		if f.cursor > 0 {
			f.cursor--
		}
	case "down", "tab":
		if f.cursor < f.rowCount()-1 {
			f.cursor++
		}
	case "enter":
		if f.cursor >= len(f.fields) {
			return f.actions[f.cursor-len(f.fields)]
		}
		f.cursor++
	case " ":
		switch fld := f.focusedField(); {
		case fld == nil:
			return f.actions[f.cursor-len(f.fields)]
		case fld.kind == kindToggle:
			fld.on = !fld.on
		default:
			fld.text += " " // paths may contain spaces
		}
	case "backspace":
		if fld := f.focusedField(); fld != nil && fld.kind == kindText && fld.text != "" {
			runes := []rune(fld.text)
			fld.text = string(runes[:len(runes)-1])
		}
	case "ctrl+u":
		if fld := f.focusedField(); fld != nil && fld.kind == kindText {
			fld.text = ""
		}
	default:
		// KeyRunes may carry several runes at once — fast typing and pastes
		// arrive batched — so append them all, not just single keystrokes.
		if fld := f.focusedField(); fld != nil && fld.kind == kindText && key.Type == tea.KeyRunes {
			fld.text += string(key.Runes)
		}
	}
	return ""
}

func (f *form) View() string {
	var b strings.Builder
	for i := range f.fields {
		b.WriteString(f.fieldRow(i))
	}
	b.WriteString("\n")
	for i, action := range f.actions {
		cursor, label := "  ", styleAccent.Render(action)
		if f.cursor == len(f.fields)+i {
			cursor = styleCursor.Render("❯ ")
			label = styleCursor.Render(action)
		}
		b.WriteString(cursor + label + "\n")
	}
	return b.String()
}

func (f *form) fieldRow(i int) string {
	fld := f.fields[i]
	focused := f.cursor == i

	value := fld.text
	if fld.secret && value != "" {
		value = "[hidden]"
	}
	switch {
	case fld.kind == kindToggle && fld.on:
		value = styleOK.Render("on")
	case fld.kind == kindToggle:
		value = "off"
	case focused:
		// The edit caret marks exactly one field: the focused one. A caret on
		// an unfocused field reads as "the cursor is still up there".
		value += "▏"
	case value == "":
		value = styleDim.Render(fld.placeholder)
	}

	cursor, label := "  ", fld.label
	if focused {
		cursor = styleCursor.Render("❯ ")
		label = styleCursor.Render(label)
	}
	// Pad by display width: the focused label carries ANSI codes that byte
	// padding would count as width.
	return cursor + padRight(label, 39) + value + "\n"
}
