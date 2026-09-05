package supervise

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

const (
	syncFreezeProbeDelay = 30 * time.Second
	maxRecoveryCycles    = 5
)

type recoveryDecision int

const (
	recoveryAbort recoveryDecision = iota
	recoveryReady
	recoveryAgain
)

var errRecoveryAlreadyIdle = errors.New("sync became idle during cleanup")

// cycles survives successful deletes and failed retries. Only confirmed
// indexing progress in runPhase resets it; five crash/recovery episodes that
// each advance the checkpoint must not be mistaken for one stuck lock.
func (s *Supervisor) recoverForRetry(ctx context.Context, indexable string, version int, lockReported bool, cycles *int) bool {
	for ctx.Err() == nil && !s.stopRequested() {
		if *cycles >= maxRecoveryCycles {
			s.blockRecovery(indexable, "five recovery cycles without indexing progress; the cause is unresolved. Inspect attempt logs and remote workers before resuming")
			return false
		}
		(*cycles)++
		s.logf(LevelInfo, "[%s] recovery cycle %d/%d — checking remote state before retrying", indexable, *cycles, maxRecoveryCycles)
		s.notifyRetry(indexable, fmt.Sprintf("Checking remote indexing state (recovery %d/%d).", *cycles, maxRecoveryCycles))
		switch s.recoverCycle(ctx, indexable, version, lockReported) {
		case recoveryReady:
			return true
		case recoveryAbort:
			return false
		}
	}
	return false
}

func (s *Supervisor) recoveryStatus(ctx context.Context) *vipsearch.IndexingStatus {
	if ctx.Err() != nil || s.stopRequested() {
		return nil
	}
	st := s.client.Status(ctx)
	// A stop can arrive while the remote read is in flight. Do not let its
	// result authorize cleanup after the user has already asked us to stop.
	if ctx.Err() != nil || s.stopRequested() {
		return nil
	}
	return st
}

func (s *Supervisor) blockRecovery(indexable, reason string) recoveryDecision {
	s.logf(LevelError, "[%s] recovery stopped: %s. No further remote state will be cleared.", indexable, reason)
	return recoveryAbort
}

func (s *Supervisor) recoverCycle(ctx context.Context, indexable string, version int, lockReported bool) recoveryDecision {
	first := s.recoveryStatus(ctx)
	if first == nil {
		return s.blockRecovery(indexable, "indexing status is unknown")
	}
	s.setStatusNote("checking remote progress — waiting 30s")
	s.logf(LevelInfo, "[%s] first recovery status: %s; re-reading in %s", indexable, recoveryPosition(first), syncFreezeProbeDelay)
	if !s.wait(ctx, syncFreezeProbeDelay) {
		return recoveryAbort
	}
	second := s.recoveryStatus(ctx)
	if second == nil {
		return s.blockRecovery(indexable, "second indexing status is unknown")
	}
	s.logf(LevelInfo, "[%s] second recovery status: %s", indexable, recoveryPosition(second))
	if second.Indexing {
		if !first.Indexing || syncFingerprint(first) != syncFingerprint(second) {
			return s.blockRecovery(indexable, "remote progress or worker identity changed; another worker may still be running")
		}
		if !s.cfg.AggressiveRecovery {
			return s.blockRecovery(indexable, "no movement in 30s is not proof the remote worker stopped; confirm it stopped before using unlock")
		}
		if !activeRecoveryEligible(second, indexable, version) {
			return s.blockRecovery(indexable, "active status lacks usable CLI progress or refers to a different indexing scope")
		}
		s.logf(LevelWarn, "[%s] AGGRESSIVE recovery: treating unchanged CLI state as suspected stale, not proven dead", indexable)
	} else if !lockReported {
		// A failed CLI with no lock and an idle platform needs only a resume.
		return recoveryReady
	}

	// Version reads can take time. Re-read status after them, immediately
	// before mutation, and refuse a new worker even in aggressive mode.
	if !s.retryVersionUnchanged(ctx, indexable, version) {
		return recoveryAbort
	}
	beforeClear := s.recoveryStatus(ctx)
	if beforeClear == nil {
		return s.blockRecovery(indexable, "status became unknown before cleanup")
	}
	if beforeClear.Indexing && (!second.Indexing || syncFingerprint(beforeClear) != syncFingerprint(second)) {
		return s.blockRecovery(indexable, "remote state changed before cleanup")
	}
	if ctx.Err() != nil || s.stopRequested() {
		return recoveryAbort
	}
	s.logf(LevelInfo, "[%s] running delete-transient; its acknowledgement alone will not authorize a retry", indexable)
	res := s.client.ClearIndexLock(ctx)
	afterClear := s.recoveryStatus(ctx)
	if !res.Succeeded() {
		return s.blockRecovery(indexable, "delete-transient did not succeed: "+strings.Join(res.DescribeFailure(), "; "))
	}
	if afterClear == nil {
		return s.blockRecovery(indexable, "could not verify status after delete-transient")
	}
	if !afterClear.Indexing {
		s.logf(LevelInfo, "[%s] delete-transient verified: status now reports idle", indexable)
		return recoveryReady
	}
	if !s.cfg.AggressiveRecovery || !beforeClear.Indexing || syncFingerprint(beforeClear) != syncFingerprint(afterClear) {
		return s.blockRecovery(indexable, "active state appeared or changed after delete-transient; not escalating to option cleanup")
	}

	// The same suspected-stale record remains. Observe for another full
	// window before escalating, and guard each individual option/cache write.
	s.logf(LevelWarn, "[%s] same CLI state remains after delete-transient; observing another 30s before guarded option/cache cleanup", indexable)
	if !s.wait(ctx, syncFreezeProbeDelay) {
		return recoveryAbort
	}
	guard := func(ctx context.Context) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if s.stopRequested() {
			return context.Canceled
		}
		if !s.retryVersionUnchanged(ctx, indexable, version) {
			return errors.New("index version changed or became unknown")
		}
		st := s.recoveryStatus(ctx)
		if st == nil {
			return errors.New("indexing status became unknown")
		}
		if !st.Indexing {
			return errRecoveryAlreadyIdle
		}
		if syncFingerprint(st) != syncFingerprint(afterClear) {
			return errors.New("progress or worker identity changed during option cleanup")
		}
		return nil
	}
	res = s.client.ClearSyncRecordGuarded(ctx, guard)
	if errors.Is(res.Err, errRecoveryAlreadyIdle) {
		s.logf(LevelInfo, "[%s] status became idle; remaining option/cache cleanup skipped", indexable)
		return recoveryReady
	}
	if res.Failed() {
		return s.blockRecovery(indexable, "option/cache cleanup stopped: "+strings.Join(res.DescribeFailure(), "; "))
	}
	final := s.recoveryStatus(ctx)
	if final == nil {
		return s.blockRecovery(indexable, "could not verify option/cache cleanup")
	}
	if !final.Indexing {
		s.logf(LevelInfo, "[%s] option/cache cleanup verified: status now reports idle", indexable)
		return recoveryReady
	}
	if syncFingerprint(final) != syncFingerprint(afterClear) {
		return s.blockRecovery(indexable, "remote state changed after option/cache cleanup")
	}
	s.logf(LevelWarn, "[%s] cleanup did not resolve the unchanged state; no new indexer will start until idle is verified", indexable)
	return recoveryAgain
}

func activeRecoveryEligible(st *vipsearch.IndexingStatus, indexable string, version int) bool {
	if st.Method != "cli" {
		return false
	}
	if st.CurrentSync != nil && st.CurrentSync.Indexable != "" && st.CurrentSync.Indexable != indexable {
		return false
	}
	// Some versions expose no last-object ID. A real counter is sufficient
	// for this opt-in heuristic; missing values (-1) are never compared as zero.
	usable := st.ItemsIndexed >= 0
	if cur := st.CurrentSync; cur != nil {
		usable = usable || cur.LastObjectID > 0 || cur.Synced >= 0 || cur.Skipped >= 0 || cur.Failed >= 0
	}
	if !usable {
		return false
	}
	for _, raw := range []map[string]any{st.Raw, rawCurrentSync(st)} {
		if name, ok := raw["indexable"]; ok && fmt.Sprint(name) != indexable {
			return false
		}
		for _, key := range []string{"items_indexed", "total_items", "synced", "skipped", "failed", "last_processed_object_id", "total"} {
			if value, ok := raw[key]; ok {
				n, err := strconv.ParseInt(strings.TrimSpace(fmt.Sprint(value)), 10, 64)
				if err != nil || n < -1 {
					return false
				}
			}
		}
		if v, ok := raw["version"]; ok {
			n, err := strconv.Atoi(strings.TrimSpace(fmt.Sprint(v)))
			if err != nil || n != version {
				return false
			}
		}
	}
	return true
}

func rawCurrentSync(st *vipsearch.IndexingStatus) map[string]any {
	cur, _ := st.Raw["current_sync_item"].(map[string]any)
	return cur
}

func recoveryPosition(st *vipsearch.IndexingStatus) string {
	if !st.Indexing {
		return "idle"
	}
	var id, synced, skipped, failed int64 = -1, -1, -1, -1
	if cur := st.CurrentSync; cur != nil {
		id, synced, skipped, failed = cur.LastObjectID, cur.Synced, cur.Skipped, cur.Failed
	}
	return fmt.Sprintf("method=%s, started=%s, indexed=%s, last ID=%s, synced=%s, skipped=%s, failed=%s", st.Method, st.StartDateTime, formatInt(st.ItemsIndexed), formatInt(id), formatInt(synced), formatInt(skipped), formatInt(failed))
}

func (s *Supervisor) retryVersionUnchanged(ctx context.Context, indexable string, version int) bool {
	if ctx.Err() != nil || s.stopRequested() {
		return false
	}
	rows := s.client.Versions(ctx, indexable)
	if ctx.Err() != nil || s.stopRequested() {
		return false
	}
	for _, row := range rows {
		if row.Number != version {
			continue
		}
		created := s.phases[s.current].VersionCreated
		if (created != "" && created != row.Created) ||
			((s.cfg.Strategy == StrategySetup || s.cfg.Strategy == StrategyResume) && !row.Active) ||
			(s.cfg.Strategy == StrategyNewVersion && row.Active) {
			break
		}
		return true
	}
	s.blockRecovery(indexable, "the selected index version changed, disappeared, or could not be verified")
	return false
}

// Recheck after retry backoff, not only just after cleanup. This reduces the
// check/start race but cannot replace a platform-side ownership/locking API.
func (s *Supervisor) retryReadyNow(ctx context.Context, indexable string, version int) bool {
	if !s.retryVersionUnchanged(ctx, indexable, version) {
		return false
	}
	st := s.recoveryStatus(ctx)
	if st == nil || st.Indexing {
		s.blockRecovery(indexable, "platform is no longer confirmed idle immediately before retry")
		return false
	}
	return true
}
