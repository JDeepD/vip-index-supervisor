package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// Deliberately read-only: a recovery assessment cannot stop workers, clear
// locks, register versions, or activate an index.
type RecoveryClient interface {
	Status(context.Context) *vipsearch.IndexingStatus
	Versions(context.Context, string) []vipsearch.IndexVersion
}

type RecoveryReport struct {
	CanResume bool
	Verdict   string
	Reasons   []string
	Remote    *vipsearch.IndexingStatus
	Versions  map[string][]vipsearch.IndexVersion
	Pins      map[string]int
	CheckedAt time.Time
}

func CheckStateLock(dir string) (bool, error) {
	if _, err := os.Stat(filepath.Join(dir, "supervisor.lock")); os.IsNotExist(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	l, err := acquireStateLock(dir)
	if errors.Is(err, ErrLocked) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	l.Release()
	return false, nil
}

func InspectRecovery(ctx context.Context, r RunRecord, client RecoveryClient) RecoveryReport {
	held, lockErr := CheckStateLock(r.Config.StateDir)
	report := inspectRecovery(ctx, r, client)
	if lockErr != nil {
		report.Reasons = append(report.Reasons, "Could not check the local supervisor lock.")
	}
	if held {
		report.Reasons = append(report.Reasons, "A local supervisor still holds the state directory. Wait for it to finish.")
	}
	return finalizeRecovery(report)
}

// Runtime calls this while already holding the local state lock. Every read
// is repeated; a UI assessment is a snapshot, not permission to ignore changes.
func inspectRecovery(ctx context.Context, r RunRecord, client RecoveryClient) RecoveryReport {
	report := RecoveryReport{CheckedAt: time.Now(), Versions: make(map[string][]vipsearch.IndexVersion), Pins: make(map[string]int)}
	if err := r.Validate(); err != nil {
		report.Reasons = append(report.Reasons, err.Error())
		return finalizeRecovery(report)
	}
	if r.Outcome == "completed" {
		report.Reasons = append(report.Reasons, "This run already completed; there is nothing to resume.")
	}
	if r.Config.IgnoreLock {
		report.Reasons = append(report.Reasons, "This run ignored the local lock; checkpoint ownership needs manual review.")
	}
	wd, err := os.Getwd()
	if err != nil || (!r.Config.Target.IsVIP() && wd != r.WorkingDir) {
		report.Reasons = append(report.Reasons, "Direct WP resume requires the original working directory: "+r.WorkingDir)
	}
	runs, warnings, err := ListRuns(r.Config.StateDir)
	if err != nil || len(warnings) > 0 {
		report.Reasons = append(report.Reasons, "Run history is incomplete or unreadable; repair it before using automatic recovery.")
	}
	latest, latestErr := latestRunID(r.Config.StateDir)
	if latestErr != nil {
		report.Reasons = append(report.Reasons, "Could not verify the latest saved run; inspect local history before resuming.")
	} else if latest != "" && latest != r.ID {
		report.Reasons = append(report.Reasons, "This run was superseded. Inspect the latest saved run: "+latest)
	}
	for _, other := range runs {
		if latest == "" && other.ID != r.ID && other.StartedAt.After(r.StartedAt) {
			report.Reasons = append(report.Reasons, "A newer run exists in this state directory. Inspect the newest run instead of replaying older state.")
			break
		}
	}
	report.Remote = client.Status(ctx)
	if report.Remote == nil {
		report.Reasons = append(report.Reasons, "Remote indexing state is unknown. Check connectivity/authentication and refresh; do not clear the lock.")
	} else if report.Remote.Indexing {
		report.Reasons = append(report.Reasons, "The platform reports an active indexer. Wait and inspect status; inactivity alone does not prove a stale lock.")
	}
	remaining := 0
	for _, p := range r.Phases {
		report.Pins[p.Name] = (checkpointStore{dir: r.Config.StateDir, postTypes: r.Config.PostTypes}).PinnedVersion(p.Name)
		if p.Status == PhaseComplete {
			continue
		}
		remaining++
		rows := client.Versions(ctx, p.Name)
		report.Versions[p.Name] = rows
		if len(rows) == 0 {
			report.Reasons = append(report.Reasons, p.Name+": registered versions could not be read.")
			continue
		}
		if p.RegistrationPending {
			report.Reasons = append(report.Reasons, p.Name+": version registration was interrupted. Inspect Versions and explicitly choose the created version, if any.")
			continue
		}
		if r.Config.Strategy == StrategySetup && !p.IndexingComplete && (p.Version <= 0 || p.LastObjectID <= 0 || p.Attempt <= 0) {
			report.Reasons = append(report.Reasons, p.Name+": --setup has no confirmed progress. Inspect its attempt log before explicitly choosing rebuild or resume.")
			continue
		}
		if p.Version == 0 {
			if p.Attempt > 0 {
				report.Reasons = append(report.Reasons, p.Name+": attempted indexing without a saved version; inspect Versions.")
			}
			continue // an untouched phase can follow the saved strategy
		}
		var found *vipsearch.IndexVersion
		for i := range rows {
			if rows[i].Number == p.Version {
				found = &rows[i]
			}
		}
		if found == nil {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%s: saved version %d is missing. Do not reuse its checkpoint for another version.", p.Name, p.Version))
			continue
		}
		if p.VersionCreated != "" && p.VersionCreated != found.Created {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%s: v%d creation metadata changed or is unavailable. It may have been recreated; do not reuse its checkpoint.", p.Name, p.Version))
		}
		if r.Config.Strategy == StrategyNewVersion && found.Active {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%s: saved build v%d is already active. Inspect activation before deciding whether any indexing is still needed.", p.Name, p.Version))
		}
		if (r.Config.Strategy == StrategySetup || r.Config.Strategy == StrategyResume) && !found.Active {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%s: the active version changed away from v%d. Choose the intended version explicitly.", p.Name, p.Version))
		}
		pin := report.Pins[p.Name]
		if r.Config.Strategy == StrategyNewVersion && pin != p.Version {
			report.Reasons = append(report.Reasons, fmt.Sprintf("%s: local pin v%d does not match saved v%d. Inspect local state before resuming.", p.Name, pin, p.Version))
		}
	}
	if remaining == 0 && r.Outcome != "completed" {
		report.Reasons = append(report.Reasons, "Every phase completed, but final bookkeeping was interrupted. Verify the result; no indexing needs replaying.")
	}
	if ctx.Err() != nil {
		report.Reasons = append(report.Reasons, "Recovery checks did not finish; refresh before resuming.")
	}
	return finalizeRecovery(report)
}

func finalizeRecovery(report RecoveryReport) RecoveryReport {
	report.CanResume = len(report.Reasons) == 0
	report.Verdict = "Needs investigation — automatic resume is blocked"
	if report.CanResume {
		report.Verdict = "Ready to resume the saved run (remote currently idle)"
	}
	if report.Remote != nil && report.Remote.Indexing {
		report.Verdict = "Another worker may still be running — wait"
	}
	return report
}

func (s *Supervisor) restoreRun(ctx context.Context) error {
	if s.cfg.ResumeRunID == "" {
		return nil
	}
	r, err := LoadRun(s.cfg.StateDir, s.cfg.ResumeRunID)
	if err != nil {
		return err
	}
	expected, err := ResumeConfig(r, s.cfg.Notifications)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(s.cfg, expected) {
		return errors.New("saved-run settings changed; reopen recovery to resume the exact configuration")
	}
	report := inspectRecovery(ctx, r, s.client)
	if !report.CanResume {
		return errors.New(strings.Join(report.Reasons, " "))
	}
	s.resumed = &r
	s.mu.Lock()
	s.phases = append([]PhaseSnapshot(nil), r.Phases...)
	for i := range s.phases {
		if s.phases[i].Status != PhaseComplete {
			s.phases[i].Status = PhasePending
			s.phases[i].StatusNote = ""
		}
		s.phases[i].Rate, s.phases[i].ETA = 0, 0
	}
	s.mu.Unlock()
	return nil
}
