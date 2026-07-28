// Package tui is the interactive front-end: a stack of screens navigated
// with arrows, enter/space to select, and Esc to go back.
package tui

import (
	"os"
	"runtime"
	"strings"

	"github.com/charmbracelet/bubbles/spinner"
	"github.com/charmbracelet/lipgloss"
)

var (
	colorAccent = lipgloss.AdaptiveColor{Light: "#5A56E0", Dark: "#7D79F6"}
	colorDim    = lipgloss.AdaptiveColor{Light: "#8a8a8a", Dark: "#6c6c6c"}
	colorOK     = lipgloss.AdaptiveColor{Light: "#0a7d33", Dark: "#4eca69"}
	colorWarn   = lipgloss.AdaptiveColor{Light: "#a86500", Dark: "#f5a623"}
	colorErr    = lipgloss.AdaptiveColor{Light: "#c22f2f", Dark: "#ff6b6b"}

	styleTitle    = lipgloss.NewStyle().Bold(true).Foreground(colorAccent)
	styleHeading  = lipgloss.NewStyle().Bold(true)
	styleDim      = lipgloss.NewStyle().Foreground(colorDim)
	styleOK       = lipgloss.NewStyle().Foreground(colorOK)
	styleWarn     = lipgloss.NewStyle().Foreground(colorWarn)
	styleErr      = lipgloss.NewStyle().Foreground(colorErr)
	styleAccent   = lipgloss.NewStyle().Foreground(colorAccent)
	styleCursor   = lipgloss.NewStyle().Foreground(colorAccent).Bold(true)
	styleSelected = lipgloss.NewStyle().Foreground(colorOK).Bold(true)

	styleFrame = lipgloss.NewStyle().Padding(1, 2)

	styleHelp = lipgloss.NewStyle().Foreground(colorDim).MarginTop(1)
)

// uiASCII is true when the terminal cannot be trusted with multi-byte glyphs
// — a non-UTF-8 locale renders ▶ █ — ● as replacement junk. Overridable with
// VIP_INDEX_ASCII=1 (force plain) or =0 (force unicode).
var uiASCII = detectASCIIOnly()

func detectASCIIOnly() bool {
	if v := os.Getenv("VIP_INDEX_ASCII"); v != "" {
		return v != "0"
	}
	if runtime.GOOS == "windows" {
		return false // modern Windows terminals handle UTF-8
	}
	for _, key := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := os.Getenv(key); v != "" {
			return !strings.Contains(strings.ToLower(v), "utf")
		}
	}
	// No locale info at all (bare SSH sessions): mangled glyphs are worse
	// than plain ASCII, so degrade.
	return true
}

// asciiReplacer maps every decorative glyph to a SAME-DISPLAY-WIDTH ASCII
// stand-in, so applying it to a fully rendered frame cannot break alignment.
var asciiReplacer = strings.NewReplacer(
	"❯", ">", "▶", ">", "▸", ">", "›", ">", "→", ">",
	"✓", "*", "✔", "*", "✗", "x", "✘", "x",
	"⚠", "!", "●", "*", "○", "o",
	"█", "#", "░", "-", "▏", "|", "─", "-",
	"↑", "^", "↓", "v", "±", "~",
	"—", "-", "–", "-", "·", ".", "…", "~",
)

// newSpinner picks a spinner the terminal can actually draw: the default dot
// spinner is braille, which a non-UTF-8 locale turns into underscores.
func newSpinner() spinner.Model {
	frames := spinner.Dot
	if uiASCII {
		frames = spinner.Line
	}
	sp := spinner.New(spinner.WithSpinner(frames))
	sp.Style = styleAccent
	return sp
}
