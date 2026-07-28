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

// version is stamped by release.sh via -ldflags "-X main.version=vX.Y.Z".
var version = "dev"

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "--version", "-v", "version":
			fmt.Println("vip-index-supervisor " + version)
			return
		default:
			fmt.Fprintln(os.Stderr,
				"vip-index-supervisor is an interactive TUI — run it without arguments (supported: --version)")
			os.Exit(2)
		}
	}
	if !isTerminal() {
		fmt.Fprintln(os.Stderr,
			"vip-index-supervisor is an interactive TUI and needs a real terminal.\n"+
				"Run it from a shell (ideally inside tmux on a persistent host).")
		os.Exit(2)
	}
	program := tea.NewProgram(tui.NewApp(version), tea.WithAltScreen())
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
