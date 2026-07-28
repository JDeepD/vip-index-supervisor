// Package vipsearch builds and runs `wp vip-search` commands against a
// WordPress site, reached either through VIP-CLI or a local wp binary, and
// parses their output into typed results.
package vipsearch

import (
	"context"
	"errors"
	"os/exec"
	"regexp"
	"strings"
	"time"
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

// Base is the command prefix every vip-search invocation starts from.
func (t Target) Base() []string {
	if t.IsVIP() {
		return []string{"vip", t.AppEnv, "--yes", "--", "wp", "vip-search"}
	}
	wp := strings.Fields(t.WPCommand)
	if len(wp) == 0 {
		wp = []string{"wp"}
	}
	return append(wp, "vip-search")
}

var reProduction = regexp.MustCompile(`(?i)prod(uction)?\b`)

// LooksLikeProduction is deliberately loose: a false positive costs one extra
// confirmation, a false negative costs a live index.
func (t Target) LooksLikeProduction() bool {
	return reProduction.MatchString(t.AppEnv)
}

// RunResult carries a finished command's combined output and how it ended.
type RunResult struct {
	Output   string
	Err      error
	NotFound bool // the vip/wp binary is not on PATH
	TimedOut bool
}

// Run executes one vip-search subcommand to completion and returns everything
// it printed. stdout and stderr are combined because VIP-CLI and WP-CLI
// scatter useful lines across both.
func (t Target) Run(ctx context.Context, timeout time.Duration, args ...string) RunResult {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	full := append(t.Base(), args...)
	cmd := exec.CommandContext(ctx, full[0], full[1:]...)
	out, err := cmd.CombinedOutput()

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
	return lines
}
