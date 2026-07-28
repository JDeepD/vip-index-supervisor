// Package supervise drives `wp vip-search index` to completion, resuming
// after any interruption (deploy SIGTERM, OOM kill, stall) from the last
// checkpointed object ID.
package supervise

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// Strategy is how the index is (re)built.
type Strategy int

const (
	// StrategyResume continues into the version currently serving search.
	StrategyResume Strategy = iota
	// StrategyNewVersion builds into a new index version and activates it on
	// completion, so search keeps working for the whole rebuild.
	StrategyNewVersion
	// StrategySetup drops and rebuilds the live index in place; search
	// returns nothing until the rebuild finishes.
	StrategySetup
	// StrategyIntoVersion builds into an existing, already-registered version
	// (Config.IntoVersion). Nothing is created and nothing is activated —
	// activation stays a deliberate, separate step.
	StrategyIntoVersion
)

func (s Strategy) String() string {
	switch s {
	case StrategyNewVersion:
		return "new version"
	case StrategySetup:
		return "rebuild in place"
	case StrategyIntoVersion:
		return "into existing version"
	default:
		return "resume in place"
	}
}

// Config is everything one supervised run needs. Zero values are filled in by
// Normalize, so callers only set what they mean.
type Config struct {
	Target     vipsearch.Target
	Indexables []string
	PostTypes  string // comma-separated, post indexable only
	PerPage    int
	Strategy   Strategy
	// IntoVersion is the existing version StrategyIntoVersion builds into.
	IntoVersion int
	ShowErrors  bool

	StateDir string

	MaxRetries       int
	BackoffBase      time.Duration
	BackoffMax       time.Duration
	StallTimeout     time.Duration
	NoProgressAbort  int
	MaxDuration      time.Duration // 0 = unlimited
	MinDocumentRatio float64
	VerifyAttempts   int
	VerifyDelay      time.Duration
	IgnoreLock       bool
}

// Normalize fills defaults and derives the state directory from the target,
// so two environments can never share checkpoints.
func (c *Config) Normalize() {
	if len(c.Indexables) == 0 {
		c.Indexables = []string{"post"}
	}
	if c.PerPage <= 0 {
		c.PerPage = 350
	}
	if c.MaxRetries <= 0 {
		c.MaxRetries = 1000
	}
	if c.BackoffBase <= 0 {
		c.BackoffBase = 5 * time.Second
	}
	if c.BackoffMax <= 0 {
		c.BackoffMax = 5 * time.Minute
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = 10 * time.Minute
	}
	if c.NoProgressAbort <= 0 {
		c.NoProgressAbort = 5
	}
	if c.MinDocumentRatio <= 0 {
		c.MinDocumentRatio = 0.9
	}
	if c.VerifyAttempts <= 0 {
		c.VerifyAttempts = 3
	}
	if c.VerifyDelay <= 0 {
		c.VerifyDelay = 5 * time.Second
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir(c.Target.Label())
	}
}

var reUnsafe = regexp.MustCompile(`[^\w.-]+`)

func stateSlug(label string) string {
	slug := strings.TrimPrefix(label, "@")
	slug = reUnsafe.ReplaceAllString(slug, "-")
	slug = strings.ReplaceAll(slug, ".", "-")
	slug = strings.Trim(slug, "-")
	if slug == "" {
		return "default"
	}
	return slug
}

func defaultStateDir(label string) string {
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".vip-reindex", stateSlug(label))
}
