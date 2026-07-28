// Package tui is the interactive front-end: a stack of screens navigated
// with arrows, enter/space to select, and Esc to go back.
package tui

import "github.com/charmbracelet/lipgloss"

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
