package supervise

import (
	"context"
	"fmt"
	"regexp"
	"strconv"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// "Success: Registered and created new index version 2"
var reNewVersion = regexp.MustCompile(`(?i)new index version\s+(\d+)`)

// resolveVersion decides which index version this phase builds into: the one
// pinned by a previous run, or a freshly registered one. Pinning matters
// because a resume after a crash must target the same half-built version
// instead of stacking up a fresh one on every attempt.
func (s *Supervisor) resolveVersion(ctx context.Context, indexable string) (int, error) {
	if s.cfg.Strategy == StrategyIntoVersion {
		return s.cfg.IntoVersion, nil
	}
	if s.cfg.Strategy != StrategyNewVersion {
		return 0, nil
	}
	if pinned := s.store.PinnedVersion(indexable); pinned > 0 {
		s.logf(LevelInfo, "[%s] resuming into pinned index version %d", indexable, pinned)
		return pinned, nil
	}
	return s.createVersion(ctx, indexable)
}

func (s *Supervisor) createVersion(ctx context.Context, indexable string) (int, error) {
	s.logf(LevelInfo, "[%s] registering a new index version", indexable)
	res := s.client.Target.Run(ctx, 5*time.Minute, "index-versions", "add", indexable)

	// `add` states the number it created; that is authoritative. The version
	// list is only consulted when the output does not say.
	version := 0
	if m := reNewVersion.FindStringSubmatch(res.Output); m != nil {
		version, _ = strconv.Atoi(m[1])
	} else {
		// VIP allows at most two versions per indexable, so at the limit
		// `add` fails. Falling back to the inactive slot keeps the rebuild
		// possible — but it REPLACES that version's contents, which deserves
		// saying out loud rather than a silently surprising version number.
		for _, v := range s.client.Versions(ctx, indexable) {
			if !v.Active && v.Number > version {
				version = v.Number
			}
		}
		if version > 0 {
			s.logf(LevelWarn,
				"[%s] could not register a new version (VIP allows two per indexable) — reusing inactive v%d; its previous contents will be replaced",
				indexable, version)
		}
	}
	if version == 0 {
		for _, line := range res.DescribeFailure() {
			s.logf(LevelError, "[%s] index-versions add: %s", indexable, line)
		}
		return 0, fmt.Errorf("could not determine the new index version")
	}
	if err := s.store.PinVersion(indexable, version); err != nil {
		return 0, fmt.Errorf("could not pin version %d: %w", version, err)
	}
	s.logf(LevelOK, "[%s] building into index version %d", indexable, version)
	return version, nil
}

// verifyVersion sanity-checks a freshly built version before it serves
// search. Activation is the one irreversible step, and also the cheapest
// place to catch a rebuild that finished but produced nothing. Counts are
// re-read a few times because ES document counts lag the end of a bulk index,
// and a stale zero here would abort a perfectly good build.
func (s *Supervisor) verifyVersion(ctx context.Context, indexable string, version int) (bool, string) {
	detail := "no document count reported"
	for attempt := 1; attempt <= s.cfg.VerifyAttempts; attempt++ {
		ok, d := s.checkVersionCounts(ctx, indexable, version)
		if ok {
			return true, d
		}
		detail = d
		if attempt < s.cfg.VerifyAttempts {
			s.logf(LevelWarn, "[%s] verify %d/%d: %s — re-checking in %s",
				indexable, attempt, s.cfg.VerifyAttempts, detail, s.cfg.VerifyDelay)
			if !s.sleep(ctx, s.cfg.VerifyDelay) {
				break
			}
		}
	}
	return false, detail
}

func (s *Supervisor) checkVersionCounts(ctx context.Context, indexable string, version int) (bool, string) {
	rows := s.client.Versions(ctx, indexable)
	var built, previous *vipsearch.IndexVersion
	for i := range rows {
		switch {
		case rows[i].Number == version:
			built = &rows[i]
		case rows[i].Active:
			previous = &rows[i]
		}
	}
	if built == nil {
		return false, fmt.Sprintf("version %d is not registered", version)
	}
	if built.Documents <= 0 {
		return false, "the new index reports 0 documents"
	}
	if previous != nil && previous.Documents > 0 {
		ratio := float64(built.Documents) / float64(previous.Documents)
		if ratio < s.cfg.MinDocumentRatio {
			return false, fmt.Sprintf(
				"%s documents vs %s in v%d — %.0f%% of the index it would replace, below the %.0f%% floor",
				formatInt(built.Documents), formatInt(previous.Documents), previous.Number,
				ratio*100, s.cfg.MinDocumentRatio*100)
		}
		return true, fmt.Sprintf("%s documents (%.0f%% of v%d)",
			formatInt(built.Documents), ratio*100, previous.Number)
	}
	return true, formatInt(built.Documents) + " documents"
}

// activateVersion verifies and then activates a built version. On any doubt
// it fails towards leaving the old index active: the worst case is a manual
// activation, not an empty index serving search.
func (s *Supervisor) activateVersion(ctx context.Context, indexable string, version int) bool {
	ok, detail := s.verifyVersion(ctx, indexable, version)
	if !ok {
		s.logf(LevelError,
			"[%s] REFUSING to activate v%d: %s. The current index keeps serving search; investigate, then activate manually.",
			indexable, version, detail)
		return false
	}
	s.logf(LevelOK, "[%s] verified v%d — %s", indexable, version, detail)
	s.logf(LevelInfo, "[%s] activating index version %d", indexable, version)

	res := s.client.ActivateVersion(ctx, indexable, version)
	// Fail towards leaving the old index active: the worst case is a manual
	// activation, not an empty index serving search.
	if !res.Succeeded() {
		s.logf(LevelError,
			"[%s] FAILED to activate version %d; the old index is still serving search. Activate manually once resolved.",
			indexable, version)
		return false
	}
	s.store.UnpinVersion(indexable)
	s.logf(LevelOK, "[%s] index version %d is now active", indexable, version)
	// Deleting the superseded version is irreversible, so leave it to a human.
	s.logf(LevelInfo, "[%s] previous version retained — remove it with: %s index-versions delete %s previous",
		indexable, s.commandHint(), indexable)
	return true
}

func (s *Supervisor) commandHint() string {
	if s.cfg.Target.IsVIP() {
		return "vip " + s.cfg.Target.AppEnv + " -- wp vip-search"
	}
	return "wp vip-search"
}
