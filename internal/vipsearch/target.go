// Package vipsearch builds and runs `wp vip-search` commands against a
// WordPress site, reached either through VIP-CLI or a local wp binary, and
// parses their output into typed results.
package vipsearch

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/childproc"
)

// Target names the WordPress installation the tool operates on.
//
// Two ways to reach it:
//   - through VIP-CLI: `vip <app-env> --yes -- wp vip-search ...`
//   - directly: `wp vip-search ...` (plus any extra args, e.g. --path=...)
type Target struct {
	// AppEnv is the VIP application environment, e.g. "@example-app.production".
	// Empty means direct wp-cli.
	AppEnv string
	// WPCommand is the wp-cli invocation for direct mode, e.g. "wp" or
	// "wp --path=/srv/site". Ignored when AppEnv is set.
	WPCommand string
}

// IsVIP reports whether commands go through VIP-CLI.
func (t Target) IsVIP() bool { return t.AppEnv != "" }

// Label is a short human-readable name, also used to scope the state dir.
func (t Target) Label() string {
	if t.IsVIP() {
		return t.AppEnv
	}
	return "local"
}

// BaseWP is the command prefix for any wp-cli command on this target.
func (t Target) BaseWP() []string {
	if t.IsVIP() {
		return []string{"vip", t.AppEnv, "--yes", "--", "wp"}
	}
	wp, _ := commandWords(t.WPCommand)
	if len(wp) == 0 {
		wp = []string{"wp"}
	}
	return wp
}

// Base is the command prefix every vip-search invocation starts from.
func (t Target) Base() []string {
	return append(t.BaseWP(), "vip-search")
}

// RunWP executes a wp-cli command that is NOT a vip-search subcommand.
func (t Target) RunWP(ctx context.Context, timeout time.Duration, args ...string) RunResult {
	if err := t.Validate(); err != nil {
		return RunResult{Err: err}
	}
	return t.runArgv(ctx, timeout, append(t.BaseWP(), args...))
}

var reProduction = regexp.MustCompile(`(?i)prod(uction)?\b`)

// LooksLikeProduction is deliberately loose: a false positive costs one extra
// confirmation, a false negative costs a live index.
func (t Target) LooksLikeProduction() bool {
	return reProduction.MatchString(t.AppEnv)
}

// RunResult carries a finished command's combined output and how it ended.
type RunResult struct {
	Output       string
	Err          error
	NotFound     bool // the vip/wp binary is not on PATH
	TimedOut     bool
	acknowledged bool // command-specific plain-text acknowledgement
}

// Run executes one vip-search subcommand to completion and returns everything
// it printed. stdout and stderr are combined because VIP-CLI and WP-CLI
// scatter useful lines across both.
func (t Target) Run(ctx context.Context, timeout time.Duration, args ...string) RunResult {
	if err := t.Validate(); err != nil {
		return RunResult{Err: err}
	}
	return t.runArgv(ctx, timeout, append(t.Base(), args...))
}

func (t Target) Validate() error {
	if t.IsVIP() {
		return nil
	}
	words, err := commandWords(t.WPCommand)
	if err != nil {
		return err
	}
	if len(words) > 0 && words[0] == "" {
		return fmt.Errorf("wp command executable cannot be empty")
	}
	return nil
}

// commandWords handles quotes and escaped separators without a shell or
// expansion. Other unquoted backslashes are literal, supporting Windows paths.
func commandWords(command string) ([]string, error) {
	var words []string
	var word strings.Builder
	var quote rune
	escaped, started := false, false
	runes := []rune(command)
	for i, ch := range runes {
		if escaped {
			word.WriteRune(ch)
			escaped = false
			started = true
			continue
		}
		if ch == '\\' && quote != '\'' {
			if quote == 0 && i+1 < len(runes) && !strings.ContainsRune("\\\"' \t\r\n", runes[i+1]) {
				word.WriteRune(ch)
				started = true
				continue
			}
			if quote == '"' && i+1 < len(runes) && !strings.ContainsRune("\\\"$`\n", runes[i+1]) {
				word.WriteRune(ch)
				continue
			}
			escaped = true
			started = true
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			} else {
				word.WriteRune(ch)
			}
			continue
		}
		switch ch {
		case '\'', '"':
			quote = ch
			started = true
		case ' ', '\t', '\r', '\n':
			if started {
				words = append(words, word.String())
				word.Reset()
				started = false
			}
		default:
			word.WriteRune(ch)
			started = true
		}
	}
	if quote != 0 || escaped {
		return nil, fmt.Errorf("wp command has an unfinished quote or escape")
	}
	if started {
		words = append(words, word.String())
	}
	return words, nil
}

func (t Target) runArgv(ctx context.Context, timeout time.Duration, full []string) RunResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	childproc.Configure(cmd)
	cmd.Cancel = func() error { return childproc.Kill(cmd) }
	// A descendant may inherit stdout after the CLI exits. Bound the wait
	// for those pipes too, including on cancellation.
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) {
		childproc.Kill(cmd)
	}

	// VIP-CLI colourises even when piped; strip once here so every parser
	// downstream (markers, tables, JSON extraction) sees clean text.
	res := RunResult{Output: StripANSI(string(out)), Err: err}
	if ctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	var execErr *exec.Error
	if errors.As(err, &execErr) && errors.Is(execErr.Err, exec.ErrNotFound) {
		res.NotFound = true
	}
	return res
}

// DescribeFailure turns a failed run into lines a human can act on, instead
// of an empty table or a bare "none reported".
func (r RunResult) DescribeFailure() []string {
	switch {
	case r.NotFound:
		return []string{"the command was not found on PATH"}
	case r.TimedOut:
		return []string{"the command timed out"}
	}
	out := strings.TrimSpace(r.Output)
	if out == "" {
		if r.Err != nil {
			return []string{"could not run the command: " + r.Err.Error()}
		}
		return []string{"the command produced no output at all"}
	}
	var lines []string
	for _, ln := range strings.Split(out, "\n") {
		if s := strings.TrimSpace(ln); s != "" {
			lines = append(lines, s)
		}
	}
	if len(lines) > 6 {
		lines = lines[len(lines)-6:]
	}
	if r.Err != nil {
		lines = append(lines, "command failed: "+r.Err.Error())
	}
	return lines
}
