package supervise

import (
	"context"
	"fmt"
	"regexp"
	"strconv"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// "Success: Registered and created new index version 2"
var reNewVersion = regexp.MustCompile(`(?mi)^\s*Success:\s*Registered and created new index version\s+(\d+)\s*$`)

// resolveVersion decides which index version this phase builds into: the one
// pinned by a previous run, or a freshly registered one. Pinning matters
// because a resume after a crash must target the same half-built version
// instead of stacking up a fresh one on every attempt.
func (s *Supervisor) resolveVersion(ctx context.Context, indexable string) (int, error) {
	rows := s.client.Versions(ctx, indexable)
	if len(rows) == 0 {
		return 0, fmt.Errorf("could not read registered versions for %s", indexable)
	}
	if s.resumed != nil {
		for _, p := range s.resumed.Phases {
			if p.Name != indexable || p.Version <= 0 {
				continue
			}
			for _, row := range rows {
				if row.Number == p.Version {
					if p.VersionCreated != "" && p.VersionCreated != row.Created {
						return 0, fmt.Errorf("saved version creation metadata changed or became unknown; do not reuse its checkpoint")
					}
					if s.cfg.Strategy == StrategyNewVersion && row.Active {
						return 0, fmt.Errorf("saved version became active; inspect Versions")
					}
					if (s.cfg.Strategy == StrategyResume || s.cfg.Strategy == StrategySetup) && !row.Active {
						return 0, fmt.Errorf("active version changed; inspect Versions")
					}
					return s.selectVersion(row), nil
				}
			}
			return 0, fmt.Errorf("saved version %d is no longer registered", p.Version)
		}
	}
	switch s.cfg.Strategy {
	case StrategyNewVersion:
		if pinned := s.store.PinnedVersion(indexable); pinned > 0 {
			for _, row := range rows {
				if row.Number == pinned && !row.Active {
					s.logf(LevelInfo, "[%s] resuming into pinned index version %d", indexable, pinned)
					return s.selectVersion(row), nil
				}
			}
			return 0, fmt.Errorf("pinned version %d is missing or already active; inspect Versions before choosing a build target", pinned)
		}
		return s.createVersion(ctx, indexable)
	case StrategyIntoVersion:
		for _, row := range rows {
			if row.Number == s.cfg.IntoVersion {
				return s.selectVersion(row), nil
			}
		}
		return 0, fmt.Errorf("version %d is not registered", s.cfg.IntoVersion)
	default:
		if active := vipsearch.ActiveVersion(rows); active != nil {
			return s.selectVersion(*active), nil
		}
		return 0, fmt.Errorf("no active version reported for %s", indexable)
	}
}

func (s *Supervisor) selectVersion(row vipsearch.IndexVersion) int {
	s.updatePhase(func(p *PhaseSnapshot) {
		p.Version, p.VersionCreated = row.Number, row.Created
		if p.VersionCreated == "-" {
			p.VersionCreated = ""
		}
	})
	return row.Number
}

func (s *Supervisor) createVersion(ctx context.Context, indexable string) (int, error) {
	s.updatePhase(func(p *PhaseSnapshot) { p.RegistrationPending = true })
	if err := s.persistHistory(); err != nil {
		return 0, fmt.Errorf("could not save version registration intent: %w", err)
	}
	s.logf(LevelInfo, "[%s] registering a new index version", indexable)
	res := s.client.AddVersion(ctx, indexable)

	// Accept only the add command's acknowledgement and a clean exit. An
	// inactive row from a subsequent list is not proof we created it.
	version := 0
	if m := reNewVersion.FindStringSubmatch(vipsearch.StripANSI(res.Output)); m != nil && res.Succeeded() {
		version, _ = strconv.Atoi(m[1])
	}
	if version == 0 {
		for _, line := range res.DescribeFailure() {
			s.logf(LevelError, "[%s] index-versions add: %s", indexable, line)
		}
		return 0, fmt.Errorf("could not confirm a new index version; inspect Versions and explicitly choose an existing version if desired")
	}
	s.updatePhase(func(p *PhaseSnapshot) { p.Version = version; p.RegistrationPending = false })
	if err := s.persistHistory(); err != nil {
		return 0, fmt.Errorf("could not save newly registered version: %w", err)
	}
	// A numeric slot can have old local state from a deleted version. A
	// newly created index is empty and must never inherit that checkpoint.
	if err := s.store.ClearCheckpoint(indexable, version); err != nil {
		return 0, fmt.Errorf("created v%d but could not clear its old checkpoint: %w", version, err)
	}
	if err := s.store.PinVersion(indexable, version); err != nil {
		return 0, fmt.Errorf("could not pin version %d: %w", version, err)
	}
	for _, row := range s.client.Versions(ctx, indexable) {
		if row.Number == version {
			s.selectVersion(row)
			break
		}
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
			if !s.wait(ctx, s.cfg.VerifyDelay) {
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
	if previous == nil {
		return false, "could not identify the active version this build would replace"
	}
	if built.Documents <= 0 {
		return false, "the new index reports zero or an unknown document count"
	}
	if previous != nil && previous.Documents < 0 {
		return false, "the active index document count is unknown"
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
	if ctx.Err() != nil || s.stopRequested() {
		return false
	}
	if indexable == "post" && s.cfg.PostTypes != "" {
		s.logf(LevelError, "[%s] filtered build complete; refusing automatic activation of a partial index. Inspect counts and activate manually if intended.", indexable)
		return false
	}
	ok, detail := s.verifyVersion(ctx, indexable, version)
	if !ok {
		s.logf(LevelError,
			"[%s] REFUSING to activate v%d: %s. The current index keeps serving search; investigate, then activate manually.",
			indexable, version, detail)
		return false
	}
	s.logf(LevelOK, "[%s] verified v%d — %s", indexable, version, detail)
	if ctx.Err() != nil || s.stopRequested() {
		return false
	}
	s.logf(LevelInfo, "[%s] activating index version %d", indexable, version)

	res := s.client.ActivateVersion(ctx, indexable, version)
	// Fail towards leaving the old index active: the worst case is a manual
	// activation, not an empty index serving search.
	if !res.Succeeded() {
		s.logf(LevelError,
			"[%s] could not confirm activation of version %d; check the active version on the platform before retrying.",
			indexable, version)
		return false
	}
	active := vipsearch.ActiveVersion(s.client.Versions(ctx, indexable))
	if active == nil || active.Number != version {
		s.logf(LevelError, "[%s] activation was acknowledged, but v%d could not be confirmed active; inspect Versions", indexable, version)
		return false
	}
	if err := s.store.UnpinVersion(indexable); err != nil {
		s.logf(LevelError, "[%s] v%d is active but its local pin could not be removed: %v", indexable, version, err)
		return false
	}
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
