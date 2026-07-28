// vip-index-supervisor supervises WordPress VIP Enterprise Search indexing:
// it drives `wp vip-search index` to completion, automatically resuming from
// the last indexed object ID after any interruption (deploy, OOM kill,
// stall), behind a fully interactive TUI.
//
// Run it on a persistent host (bastion / tmux), not inside the VIP container —
// a deploy is what kills the indexing process.
package main

import (
	"fmt"
	"os"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/jdeepd/vip-index-supervisor/internal/tui"
)

func main() {
	if !isTerminal() {
		fmt.Fprintln(os.Stderr,
			"vip-index-supervisor is an interactive TUI and needs a real terminal.\n"+
				"Run it from a shell (ideally inside tmux on a persistent host).")
		os.Exit(2)
	}
	program := tea.NewProgram(tui.NewApp(), tea.WithAltScreen())
	if _, err := program.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func isTerminal() bool {
	info, err := os.Stdout.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice != 0
}
