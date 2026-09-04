package supervise

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// Supervisor runs the indexing phases, restarting after every survivable
// failure and checkpointing progress so a restart never loses work. It talks
// to the outside world only through its event channel.
type Supervisor struct {
	cfg    Config
	client *vipsearch.Client
	store  checkpointStore
	events chan Event

	mu         sync.Mutex
	phases     []PhaseSnapshot
	current    int
	child      *exec.Cmd
	childGone  chan struct{}
	stopped    bool
	forced     bool
	samples    []rateSample
	phaseStart time.Time
	startedAt  time.Time
	deadline   time.Time

	// Per-attempt counters from the child, which restarts counting at every
	// attempt. The phase-level Done/Total shown to the UI are derived from
	// these so the progress bar never falls back to 0% on a resume.
	attemptDone  int64
	attemptTotal int64

	logFile    *os.File
	eventsFile *os.File
}

type rateSample struct {
	at   time.Time
	done int64
}

// New prepares a supervisor. Nothing runs until Run is called.
func New(cfg Config) *Supervisor {
	cfg.Normalize()
	phases := make([]PhaseSnapshot, len(cfg.Indexables))
	for i, name := range cfg.Indexables {
		phases[i] = PhaseSnapshot{Name: name, Status: PhasePending,
			Done: vipsearch.NoValue, Total: vipsearch.NoValue, LastObjectID: vipsearch.NoValue}
	}
	return &Supervisor{
		cfg:     cfg,
		client:  vipsearch.NewClient(cfg.Target),
		store:   checkpointStore{dir: cfg.StateDir, postTypes: cfg.PostTypes},
		events:  make(chan Event, 1024),
		phases:  phases,
		current: -1,
	}
}

// Events is the stream the UI listens on; it closes after DoneEvent.
func (s *Supervisor) Events() <-chan Event { return s.events }

// RequestStop asks the run to end at the next safe point.
//
// The ordinary stop only raises the flag: the supervisor loop notices within a
// second and shuts the child down gracefully — asking the platform to stop
// indexing first, so ElasticPress can clear its own sync record instead of
// leaving debris that blocks the next run. force skips all of that and kills
// immediately, which is what a second Ctrl-C asks for.
func (s *Supervisor) RequestStop(force bool) {
	s.mu.Lock()
	s.stopped = true
	if force {
		s.forced = true
	}
	child, gone := s.child, s.childGone
	s.mu.Unlock()

	if force && child != nil {
		terminateProcessTree(child, 0, gone)
	}
}

func (s *Supervisor) forceRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.forced
}

// Run drives every phase to completion, blocking until done. Call it from a
// goroutine and consume Events; the exit code arrives as DoneEvent.
func (s *Supervisor) Run(ctx context.Context) {
	defer close(s.events)

	if err := s.prepareStateDir(); err != nil {
		s.events <- DoneEvent{ExitCode: 2, Message: err.Error()}
		return
	}
	defer s.closeLogs()

	if !s.cfg.IgnoreLock {
		lock, err := acquireStateLock(s.cfg.StateDir)
		if err != nil {
			s.events <- DoneEvent{ExitCode: 2, Message: fmt.Sprintf(
				"%v\nConcurrent runs corrupt each other's checkpoints. Wait for it to finish or use a different state dir.", err)}
			return
		}
		defer lock.Release()
	}

	if removed := cleanOldAttemptLogs(filepath.Join(s.cfg.StateDir, "logs")); removed > 0 {
		s.logf(LevelInfo, "removed %d attempt log(s) older than %d days", removed, attemptLogMaxAgeDays)
	}

	s.startedAt = time.Now()
	if s.cfg.MaxDuration > 0 {
		s.deadline = s.startedAt.Add(s.cfg.MaxDuration)
	}
	s.logf(LevelInfo, "===== supervisor start (target: %s, phases: %s, strategy: %s) =====",
		s.cfg.Target.Label(), strings.Join(s.cfg.Indexables, ", "), s.cfg.Strategy)

	if problems := s.preflight(ctx); len(problems) > 0 {
		for _, problem := range problems {
			s.logf(LevelError, "preflight: %s", problem)
		}
		s.events <- DoneEvent{ExitCode: 2, Message: "preflight failed — nothing was started"}
		return
	}
	s.logf(LevelInfo, "preflight ok — every indexable is registered")

	for i, indexable := range s.cfg.Indexables {
		if s.stopRequested() {
			break
		}
		s.beginPhase(i)
		if !s.runPhase(ctx, indexable) {
			s.logf(LevelError, "phase '%s' did not complete", indexable)
			s.events <- DoneEvent{ExitCode: 1, Message: fmt.Sprintf("phase '%s' did not complete", indexable)}
			return
		}
	}

	if s.stopRequested() {
		s.logf(LevelWarn, "interrupted by user")
		s.events <- DoneEvent{ExitCode: 130, Message: "interrupted — re-run to resume from the checkpoint"}
		return
	}
	elapsed := time.Since(s.startedAt).Round(time.Second)
	s.logf(LevelOK, "ALL PHASES COMPLETE (%s) in %s", strings.Join(s.cfg.Indexables, ", "), elapsed)
	s.events <- DoneEvent{ExitCode: 0, Message: "all phases complete in " + elapsed.String()}
}

// -- one phase --------------------------------------------------------------

func (s *Supervisor) runPhase(ctx context.Context, indexable string) bool {
	version, err := s.resolveVersion(ctx, indexable)
	if err != nil {
		s.logf(LevelError, "phase '%s' aborted: %v", indexable, err)
		return false
	}
	s.updatePhase(func(p *PhaseSnapshot) { p.Version = version; p.Status = PhaseRunning })

	checkpoint := s.resolveStartCheckpoint(ctx, indexable, version)
	if checkpoint > 0 {
		s.store.WriteCheckpoint(indexable, version, checkpoint)
		s.logf(LevelInfo, "phase '%s' starting (resume from id %s%s)",
			indexable, formatInt(checkpoint), versionNote(version))
	} else {
		s.logf(LevelInfo, "phase '%s' starting (from the top%s)", indexable, versionNote(version))
	}

	noProgress := 0
	lockErrors := 0
	clearedStuckSync := false
	backoff := s.cfg.BackoffBase

	for !s.stopRequested() {
		if s.pastDeadline() {
			s.logf(LevelError, "[%s] time budget exhausted — stopping. Re-run to resume from id %s.",
				indexable, formatInt(s.store.ReadCheckpoint(indexable, version)))
			return false
		}

		attempt := s.nextAttempt()
		if attempt > s.cfg.MaxRetries {
			s.logf(LevelError, "[%s] exceeded max retries — aborting", indexable)
			return false
		}

		if cp := s.store.ReadCheckpoint(indexable, version); cp > 0 {
			checkpoint = cp
		}
		args := s.buildIndexArgs(indexable, attempt, checkpoint, version)
		s.logAttemptStart(indexable, attempt, checkpoint, args)

		before := s.lastObjectID()
		outcome := s.runAttempt(ctx, indexable, version, args)

		switch {
		case outcome.success:
			return s.completePhase(ctx, indexable, version, outcome)
		case s.stopRequested():
			return false
		case outcome.fatal != "":
			s.updatePhase(func(p *PhaseSnapshot) { p.Status = PhaseFailed; p.StatusNote = outcome.fatal })
			s.logf(LevelError, "[%s] %s — not retryable, aborting. See %s", indexable, outcome.fatal, outcome.logPath)
			return false
		case outcome.deadline:
			s.logf(LevelError, "[%s] stopped: time budget exhausted", indexable)
			return false
		}

		s.updatePhase(func(p *PhaseSnapshot) { p.Restarts++ })

		if outcome.lockError {
			lockErrors++
			if lockErrors >= maxConsecutiveLockErrors {
				// delete-transient only clears the CLI lock. A dead
				// dashboard/cron sync blocks indexing from a different place
				// (its own sync state), which needs stop-indexing — but only
				// once, and only if the blocking sync is provably frozen.
				if !clearedStuckSync {
					cleared, diagnosed := s.clearStuckSyncIfFrozen(ctx, indexable)
					if cleared {
						clearedStuckSync = true
						lockErrors = 0
						if !s.sleep(ctx, s.cfg.BackoffBase) {
							return false
						}
						continue
					}
					if diagnosed {
						return false // the failure was already explained
					}
				}
				s.reportPersistentLock(ctx, indexable, clearedStuckSync)
				return false
			}
			lockWait := lockRetryDelay(lockErrors)
			s.setStatusNote("clearing stale index lock")
			s.logf(LevelWarn, "[%s] stale index lock — clearing (delete-transient), retrying in %s", indexable, lockWait)
			s.client.ClearIndexLock(ctx)
			if !s.sleep(ctx, lockWait) {
				return false
			}
			continue // a stale lock is not a failed attempt
		}
		lockErrors = 0

		after := s.lastObjectID()
		progressed := after != vipsearch.NoValue && (before == vipsearch.NoValue || after < before)
		if progressed {
			noProgress = 0
			backoff = s.cfg.BackoffBase
			s.logf(LevelWarn, "[%s] stopped (%s); progressed to id %s — retrying in %s",
				indexable, outcome.exitNote(), formatInt(after), backoff)
		} else {
			noProgress++
			s.logf(LevelWarn, "[%s] stopped (%s) with no progress (%d/%d)",
				indexable, outcome.exitNote(), noProgress, s.cfg.NoProgressAbort)
			if noProgress >= s.cfg.NoProgressAbort {
				s.logf(LevelError, "[%s] no progress after %d tries — aborting. See %s",
					indexable, noProgress, outcome.logPath)
				return false
			}
			backoff = min(backoff*2, s.cfg.BackoffMax)
		}

		s.setStatusNote("retrying in " + backoff.String())
		if !s.sleep(ctx, backoff) {
			return false
		}
	}
	return false
}

func (s *Supervisor) completePhase(ctx context.Context, indexable string, version int, outcome attemptOutcome) bool {
	switch {
	case s.cfg.Strategy == StrategyNewVersion:
		if !s.activateVersion(ctx, indexable, version) {
			return false
		}
	case s.cfg.Strategy == StrategyIntoVersion:
		// Building into an existing version never auto-activates: the user
		// chose that version deliberately and decides when it serves search.
		s.logf(LevelOK, "[%s] version %d built — activate it from the versions screen when ready", indexable, version)
	}
	s.store.ClearCheckpoint(indexable, version)

	s.mu.Lock()
	p := &s.phases[s.current]
	p.Status = PhaseComplete
	p.StatusNote = ""
	indexed := outcome.indexed
	if indexed == vipsearch.NoValue {
		indexed = p.Done
	}
	elapsed := time.Since(s.phaseStart)
	attempts := p.Attempt
	s.mu.Unlock()

	s.logf(LevelOK, "[%s] COMPLETE — %s objects in %s (%d attempt(s))",
		indexable, formatInt(indexed), elapsed.Round(time.Second), attempts)
	s.emitProgress()
	return true
}

// A genuinely stale lock clears on the first delete-transient. If the second
// attempt is refused too, the block is coming from somewhere the lock has no
// power over: either a live sync re-asserting it, or a dead run's orphaned
// sync record. Those need opposite responses, so probe rather than keep
// hammering delete-transient.
const (
	maxConsecutiveLockErrors = 3
	maxLockRetryDelay        = time.Minute
)

// lockRetryDelay is deliberately separate from the general failure backoff.
// A stale lock gets two bounded chances to disappear before the third error
// moves into the existing remote-sync diagnosis path.
func lockRetryDelay(consecutive int) time.Duration {
	if consecutive <= 0 {
		return 0
	}
	delay := 10 * time.Second
	if consecutive >= 2 {
		delay = 30 * time.Second
	}
	return min(delay, maxLockRetryDelay)
}

// syncFreezeProbeDelay is how long the two status reads are apart. A live
// bulk sync advances thousands of objects in this window; identical numbers
// mean the recorded sync is dead.
const syncFreezeProbeDelay = 15 * time.Second

// clearStuckSyncIfFrozen checks whether the sync blocking this phase is
// actually advancing. A provably frozen one is the debris of a killed run and
// gets cleared so the phase can retry; a live one is never touched.
//
// cleared says the block is gone; diagnosed says the situation was already
// explained to the user, so the caller must not add a second, vaguer verdict.
func (s *Supervisor) clearStuckSyncIfFrozen(ctx context.Context, indexable string) (cleared, diagnosed bool) {
	first := s.client.Status(ctx)
	if first == nil || !first.Indexing {
		return false, false
	}
	s.setStatusNote("blocking sync found — probing whether it is alive")
	s.logf(LevelInfo, "[%s] a %s sync reports in-progress — re-reading status in %s to see if it is advancing",
		indexable, syncMethod(first), syncFreezeProbeDelay)
	if !s.sleep(ctx, syncFreezeProbeDelay) {
		return false, false
	}
	second := s.client.Status(ctx)
	if second == nil {
		return false, false
	}
	if !second.Indexing {
		return false, false // it finished on its own; a plain retry will do
	}
	if syncFingerprint(first) != syncFingerprint(second) {
		return false, false // advancing — genuinely active, do not interfere
	}
	s.logf(LevelWarn,
		"[%s] the blocking %s sync is FROZEN (no movement in %s, stuck at %s) — it is the debris of a killed run; clearing its sync record",
		indexable, syncMethod(second), syncFreezeProbeDelay, syncFingerprint(second))
	return s.clearOrphanedSync(ctx, indexable), true
}

// clearOrphanedSync removes a dead run's sync-state record and verifies the
// platform now reports idle. stop-indexing is deliberately NOT used: it only
// raises an interrupt flag for a live process to act on, so against a dead
// sync it reports success and changes nothing.
func (s *Supervisor) clearOrphanedSync(ctx context.Context, indexable string) bool {
	s.logf(LevelWarn, "[%s] clearing the killed run's leftovers (ep_wpcli_sync transient + ep_index_meta record, regular and network)", indexable)
	res := s.client.ClearSyncRecord(ctx)
	s.client.ClearIndexLock(ctx)

	// Trust nothing: re-read the status rather than assume the delete worked.
	if st := s.client.Status(ctx); st != nil && !st.Indexing {
		s.logf(LevelOK, "[%s] sync record cleared — the platform now reports idle", indexable)
		return true
	}
	for _, line := range res.DescribeFailure() {
		s.logf(LevelWarn, "[%s] cleanup: %s", indexable, line)
	}
	wp := strings.Join(s.cfg.Target.BaseWP(), " ")
	s.logf(LevelError,
		"[%s] could not clear the killed run's leftovers automatically. Clear them by hand, then re-run:\n"+
			"      %s transient delete ep_wpcli_sync\n"+
			"      %s option delete ep_index_meta\n"+
			"      %s site option delete ep_index_meta\n"+
			"      %s cache delete alloptions options\n"+
			"      %s cache delete ep_index_meta options",
		indexable, wp, wp, wp, wp, wp)
	return false
}

func (s *Supervisor) reportPersistentLock(ctx context.Context, indexable string, alreadyCleared bool) {
	st := s.client.Status(ctx)
	switch {
	case st != nil && st.Indexing && alreadyCleared:
		s.logf(LevelError,
			"[%s] still locked even after clearing a frozen sync — investigate on the platform side (%s get-indexing-status), then re-run.",
			indexable, s.commandHint())
	case st != nil && st.Indexing:
		s.logf(LevelError,
			"[%s] the index lock persists and the platform reports an ACTIVE, advancing index — something else is indexing (a dashboard/cron sync or another CLI). Stop it (`stop` action / %s stop-indexing) or let it finish, then re-run.",
			indexable, s.commandHint())
	case st != nil:
		s.logf(LevelError,
			"[%s] the index lock keeps reappearing even though status reports idle — clear it manually (`unlock` action) and investigate before re-running.",
			indexable)
	default:
		s.logf(LevelError,
			"[%s] the index lock persists and the indexing status could not be read — investigate on the platform side, then re-run.",
			indexable)
	}
}

func syncFingerprint(st *vipsearch.IndexingStatus) string {
	var synced, lastID int64
	if st.CurrentSync != nil {
		synced, lastID = st.CurrentSync.Synced, st.CurrentSync.LastObjectID
	}
	return fmt.Sprintf("synced %s, last id %s", formatInt(max(synced, st.ItemsIndexed)), formatInt(lastID))
}

func syncMethod(st *vipsearch.IndexingStatus) string {
	if st.Method == "" {
		return "background"
	}
	return st.Method
}

// resolveStartCheckpoint picks where indexing resumes from. When building
// into a separate version, only our own checkpoint is meaningful: the live
// get-last-indexed-post-id tracks whatever last wrote to the *active* index,
// and trusting it would skip every object above that ID in a brand new,
// empty index.
func (s *Supervisor) resolveStartCheckpoint(ctx context.Context, indexable string, version int) int64 {
	if s.cfg.ResumeFrom > 0 {
		return s.cfg.ResumeFrom
	}
	if s.cfg.Strategy == StrategySetup {
		return 0 // fresh build starts from the top
	}
	local := s.store.ReadCheckpoint(indexable, version)
	if s.cfg.Strategy == StrategyNewVersion || s.cfg.Strategy == StrategyIntoVersion {
		return local
	}
	if indexable != "post" {
		return local
	}
	live := s.client.LastIndexedPostID(ctx)
	switch {
	case local > 0 && live > 0:
		return min(local, live)
	case live > 0:
		return live
	default:
		return local
	}
}

func (s *Supervisor) buildIndexArgs(indexable string, attempt int, checkpoint int64, version int) []string {
	args := []string{
		"index",
		"--indexables=" + indexable,
		"--per-page=" + strconv.Itoa(s.cfg.PerPage),
		"--skip-confirm",
	}
	if indexable == "post" && s.cfg.PostTypes != "" {
		args = append(args, "--post-type="+s.cfg.PostTypes)
	}
	if version > 0 {
		args = append(args, "--version="+strconv.Itoa(version))
	}
	if s.cfg.ShowErrors {
		args = append(args, "--show-errors")
	}
	// --setup only on the first attempt: a restart must never wipe a
	// partially built index.
	if s.cfg.Strategy == StrategySetup && attempt == 1 {
		args = append(args, "--setup")
	} else if checkpoint > 0 {
		args = append(args, "--upper-limit-object-id="+strconv.FormatInt(checkpoint, 10))
	}
	return args
}

// -- one attempt ------------------------------------------------------------

type attemptOutcome struct {
	success   bool
	lockError bool
	stalled   bool
	deadline  bool
	killed    bool // we ended it, rather than it exiting on its own
	fatal     string
	exitErr   error
	indexed   int64
	logPath   string
}

func (o attemptOutcome) exitNote() string {
	switch {
	case o.stalled:
		return "stalled"
	case o.exitErr != nil:
		return o.exitErr.Error()
	default:
		return "exited"
	}
}

func (s *Supervisor) runAttempt(ctx context.Context, indexable string, version int, args []string) attemptOutcome {
	outcome := attemptOutcome{indexed: vipsearch.NoValue}
	outcome.logPath = s.attemptLogPath(indexable)

	full := append(s.cfg.Target.Base(), args...)
	cmd := exec.Command(full[0], full[1:]...)
	configureProcessGroup(cmd)

	stdout, err := cmd.StdoutPipe()
	if err == nil {
		cmd.Stderr = cmd.Stdout // interleave, same as a terminal would
		err = cmd.Start()
	}
	if err != nil {
		// Retrying cannot conjure a missing binary; report the fatal it is.
		outcome.fatal = fmt.Sprintf("could not start %q: %v", full[0], err)
		return outcome
	}

	// Closed at stdout EOF, i.e. when the whole child tree has let go of the
	// pipe. Callers use it to stop waiting out a grace period early.
	gone := make(chan struct{})

	s.mu.Lock()
	s.child = cmd
	s.childGone = gone
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.child = nil
		s.childGone = nil
		s.mu.Unlock()
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
		defer close(gone)
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 64*1024), 1024*1024)
		for scanner.Scan() {
			lines <- scanner.Text()
		}
	}()

	attemptLog, _ := os.OpenFile(outcome.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if attemptLog != nil {
		defer attemptLog.Close()
	}

	lastOutput := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ctxDone := ctx.Done()
	var lastStallProbe string
	// Set once a shutdown is under way. Without it, ticks that land between
	// the kill and the pipe reaching EOF would re-run the whole stop sequence.
	stopping := false

reading:
	for {
		select {
		case line, open := <-lines:
			if !open {
				break reading
			}
			lastOutput = time.Now()
			if attemptLog != nil {
				fmt.Fprintln(attemptLog, line)
			}
			s.consumeLine(indexable, version, line, &outcome)
		case <-ticker.C:
			if stopping {
				s.emitProgress()
				continue
			}
			switch {
			case s.stopRequested():
				stopping = true
				s.stopChild(ctx, indexable, cmd, gone, !s.forceRequested())
				outcome.killed = true
			case s.pastDeadline():
				stopping = true
				s.logf(LevelWarn, "[%s] time budget exhausted — stopping the running index", indexable)
				s.stopChild(ctx, indexable, cmd, gone, true)
				outcome.deadline = true
				outcome.killed = true
			case time.Since(lastOutput) > s.cfg.StallTimeout:
				if s.remoteIsAdvancing(ctx, indexable, &lastStallProbe) {
					// Silent stdout but the platform is still making
					// progress: killing here is what manufactures a wedged
					// index and an orphaned sync record. Keep waiting.
					lastOutput = time.Now()
					break
				}
				s.logf(LevelWarn, "[%s] no output for %s and the platform reports no progress — stopping the stalled run",
					indexable, s.cfg.StallTimeout)
				stopping = true
				s.stopChild(ctx, indexable, cmd, gone, true)
				outcome.stalled = true
				outcome.killed = true
			}
			s.emitProgress()
		case <-ctxDone:
			terminateProcessTree(cmd, 0, gone)
			outcome.killed = true
			ctxDone = nil // fires once; a closed channel would spin this loop
		}
	}

	outcome.exitErr = cmd.Wait()
	if outcome.success {
		outcome.exitErr = nil
		return outcome
	}
	// An attempt that did not finish cannot delete its own sync record, and
	// whatever it left behind blocks the next attempt (and the next phase)
	// with "an index is already occurring".
	s.cleanupAfterFailedAttempt(ctx, indexable)
	return outcome
}

// stopChild ends the running attempt. When `graceful`, the platform is asked
// to stop indexing first so ElasticPress can wind down at a batch boundary and
// delete its own sync record — a hard kill denies it that chance, which is how
// a killed run leaves debris that blocks everything after it.
func (s *Supervisor) stopChild(ctx context.Context, indexable string, cmd *exec.Cmd, gone <-chan struct{}, graceful bool) {
	if !graceful {
		terminateProcessTree(cmd, 0, gone)
		return
	}
	s.setStatusNote("asking the platform to stop indexing")
	s.client.StopIndexing(ctx)
	select {
	case <-gone: // it wound down on its own; nothing to kill
		return
	case <-time.After(remoteStopGrace):
	}
	terminateProcessTree(cmd, 2*time.Second, gone)
}

// remoteStopGrace is how long a `stop-indexing` request is given to take
// effect before the process tree is signalled. ElasticPress checks the
// interrupt flag between batches, so this has to outlast a batch.
const remoteStopGrace = 20 * time.Second

// remoteIsAdvancing reports whether the platform's own indexing status has
// moved since the previous stall probe. Quiet stdout is not proof of a stall —
// VIP-CLI buffers, and a long batch prints nothing — so the platform gets a
// say before anything is killed.
func (s *Supervisor) remoteIsAdvancing(ctx context.Context, indexable string, previous *string) bool {
	st := s.client.Status(ctx)
	if st == nil || !st.Indexing {
		return false // cannot confirm progress; treat as stalled
	}
	current := syncFingerprint(st)
	if *previous == "" {
		// First probe: there is nothing to compare against yet. Record the
		// position and let one more stall interval pass — killing on a single
		// reading would be exactly the guess this check exists to avoid.
		*previous = current
		s.logf(LevelWarn, "[%s] no output for %s — platform is at %s; re-checking before deciding it is stuck",
			indexable, s.cfg.StallTimeout, current)
		return true
	}
	advanced := *previous != current
	*previous = current
	if advanced {
		s.logf(LevelInfo, "[%s] stdout still quiet, but the platform advanced to %s — not killing it",
			indexable, current)
	}
	return advanced
}

// cleanupAfterFailedAttempt clears the sync record a dead attempt left behind.
// A dashboard/cron sync is left alone: only a CLI record can be ours, and ours
// is the one that just died.
func (s *Supervisor) cleanupAfterFailedAttempt(ctx context.Context, indexable string) {
	st := s.client.Status(ctx)
	if st == nil || !st.Indexing {
		return // nothing left behind
	}
	if st.Method != "" && st.Method != "cli" {
		s.logf(LevelWarn, "[%s] a %s sync is registered as running — leaving it alone", indexable, st.Method)
		return
	}
	s.logf(LevelInfo, "[%s] clearing the sync record left by the attempt that just died", indexable)
	s.client.ClearSyncRecord(ctx)
	s.client.ClearIndexLock(ctx)
}

func (s *Supervisor) consumeLine(indexable string, version int, line string, outcome *attemptOutcome) {
	// The raw (still colourised) line has already been written to the attempt
	// log; parse a stripped copy, or the escape codes VIP-CLI embeds around
	// "Success:" would hide the completion marker — the exact bug that made
	// the predecessor script abort runs that had in fact finished.
	line = vipsearch.StripANSI(line)
	if strings.Contains(strings.ToLower(line), vipsearch.LockErrorMarker) {
		outcome.lockError = true
	} else if outcome.fatal == "" {
		// A stale lock is retryable and has its own handling, so it must
		// never be classified as fatal.
		outcome.fatal = vipsearch.ClassifyFatal(line)
	}
	if strings.Contains(line, vipsearch.SuccessMarker) {
		outcome.success = true
	}

	p := vipsearch.ParseProgress(line)
	if p.IndexedCount != vipsearch.NoValue {
		outcome.indexed = p.IndexedCount
	}
	s.applyProgress(indexable, version, p)
}

func (s *Supervisor) applyProgress(indexable string, version int, p vipsearch.Progress) {
	s.mu.Lock()
	ph := &s.phases[s.current]
	if p.Total != vipsearch.NoValue {
		s.attemptTotal = p.Total
		// The first attempt's total is the whole phase; later attempts
		// report only what REMAINS, so the largest total seen is the size
		// of the phase this run started with.
		ph.Total = max(ph.Total, p.Total)
	}
	if p.Done != vipsearch.NoValue {
		s.attemptDone = p.Done
	}
	if s.attemptTotal > 0 && s.attemptDone >= 0 && ph.Total > 0 {
		// Overall done = phase size minus what the current attempt still has
		// left. Restart-proof: a resumed attempt's smaller total and reset
		// counter cancel out. Clamped monotonic so corpus drift mid-run can
		// never walk the bar backwards.
		overall := ph.Total - (s.attemptTotal - s.attemptDone)
		if overall > ph.Done {
			ph.Done = overall
			s.samples = append(s.samples, rateSample{at: time.Now(), done: overall})
			s.trimSamplesLocked()
		}
	}
	checkpointID := int64(0)
	if p.LastObjectID != vipsearch.NoValue {
		if ph.LastObjectID == vipsearch.NoValue || p.LastObjectID < ph.LastObjectID {
			checkpointID = p.LastObjectID
		}
		ph.LastObjectID = p.LastObjectID
	}
	s.mu.Unlock()

	// Checkpoint outside the lock: it is a disk write, and only ever for a
	// strictly lower ID — overlap on resume is a harmless upsert, skipping
	// is not.
	if checkpointID > 0 {
		s.store.WriteCheckpoint(indexable, version, checkpointID)
	}
	s.emitProgress()
}

// -- state bookkeeping ------------------------------------------------------

func (s *Supervisor) beginPhase(i int) {
	s.mu.Lock()
	s.current = i
	s.samples = nil
	s.attemptDone = vipsearch.NoValue
	s.attemptTotal = vipsearch.NoValue
	p := &s.phases[i]
	p.Status = PhaseRunning
	p.StatusNote = "starting"
	s.phaseStart = time.Now()
	s.mu.Unlock()
	s.emitProgress()
}

func (s *Supervisor) nextAttempt() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.phases[s.current].Attempt++
	return s.phases[s.current].Attempt
}

func (s *Supervisor) lastObjectID() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.phases[s.current].LastObjectID
}

func (s *Supervisor) updatePhase(mutate func(*PhaseSnapshot)) {
	s.mu.Lock()
	mutate(&s.phases[s.current])
	s.mu.Unlock()
	s.emitProgress()
}

func (s *Supervisor) setStatusNote(note string) {
	s.updatePhase(func(p *PhaseSnapshot) { p.StatusNote = note })
}

func (s *Supervisor) trimSamplesLocked() {
	cutoff := time.Now().Add(-2 * time.Minute)
	firstFresh := 0
	for firstFresh < len(s.samples)-1 && s.samples[firstFresh].at.Before(cutoff) {
		firstFresh++
	}
	s.samples = s.samples[firstFresh:]
}

func (s *Supervisor) emitProgress() {
	s.mu.Lock()
	snap := Snapshot{Current: s.current, Phases: make([]PhaseSnapshot, len(s.phases))}
	copy(snap.Phases, s.phases)
	if s.current >= 0 {
		p := &snap.Phases[s.current]
		p.Elapsed = time.Since(s.phaseStart)
		p.Rate = s.rateLocked()
		if p.Rate > 0 && p.Total > 0 && p.Done >= 0 && p.Total > p.Done {
			p.ETA = time.Duration(float64(p.Total-p.Done)/p.Rate) * time.Second
		}
		s.phases[s.current].Elapsed = p.Elapsed
	}
	s.mu.Unlock()

	// Progress is advisory: drop it rather than stall indexing behind a busy UI.
	select {
	case s.events <- ProgressEvent{State: snap}:
	default:
	}
}

func (s *Supervisor) rateLocked() float64 {
	if len(s.samples) < 2 {
		return 0
	}
	first, last := s.samples[0], s.samples[len(s.samples)-1]
	dt := last.at.Sub(first.at).Seconds()
	dd := float64(last.done - first.done)
	if dt <= 0 || dd <= 0 {
		return 0
	}
	return dd / dt
}

func (s *Supervisor) stopRequested() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stopped
}

func (s *Supervisor) pastDeadline() bool {
	return !s.deadline.IsZero() && time.Now().After(s.deadline)
}

// sleep waits while keeping the UI ticking; false means the wait was cut
// short by a stop request, the deadline, or context cancellation.
func (s *Supervisor) sleep(ctx context.Context, d time.Duration) bool {
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if s.stopRequested() || s.pastDeadline() {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(200 * time.Millisecond):
		}
		s.emitProgress()
	}
	return !s.stopRequested() && !s.pastDeadline()
}

// -- logging ----------------------------------------------------------------

func (s *Supervisor) prepareStateDir() error {
	logDir := filepath.Join(s.cfg.StateDir, "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return fmt.Errorf("could not create state directory: %w", err)
	}
	var err error
	s.logFile, err = os.OpenFile(filepath.Join(logDir, "supervisor.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("could not open the supervisor log: %w", err)
	}
	s.eventsFile, _ = os.OpenFile(filepath.Join(logDir, "events.jsonl"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	return nil
}

func (s *Supervisor) closeLogs() {
	if s.logFile != nil {
		s.logFile.Close()
	}
	if s.eventsFile != nil {
		s.eventsFile.Close()
	}
}

// Attempt logs are forensic only — resume depends on checkpoint files, never
// on logs — so old ones can be pruned freely to keep the state dir bounded.
const attemptLogMaxAgeDays = 14

func cleanOldAttemptLogs(logDir string) int {
	entries, err := os.ReadDir(logDir)
	if err != nil {
		return 0
	}
	cutoff := time.Now().AddDate(0, 0, -attemptLogMaxAgeDays)
	removed := 0
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "attempt-") || !strings.HasSuffix(entry.Name(), ".log") {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if os.Remove(filepath.Join(logDir, entry.Name())) == nil {
			removed++
		}
	}
	return removed
}

func (s *Supervisor) attemptLogPath(indexable string) string {
	stamp := time.Now().Format("20060102-150405")
	return filepath.Join(s.cfg.StateDir, "logs", fmt.Sprintf("attempt-%s-%s.log", indexable, stamp))
}

func (s *Supervisor) logAttemptStart(indexable string, attempt int, checkpoint int64, args []string) {
	resumeNote := "from top"
	if s.cfg.Strategy == StrategySetup && attempt == 1 {
		resumeNote = "--setup (fresh)"
	} else if checkpoint > 0 {
		resumeNote = "from id " + formatInt(checkpoint)
	}
	s.setStatusNote("indexing — " + resumeNote)
	full := append(s.cfg.Target.Base(), args...)
	s.logf(LevelInfo, "[%s] attempt %d (%s): %s", indexable, attempt, resumeNote, strings.Join(full, " "))
}

// logf writes to the master log, the JSONL stream, and the UI. The JSONL
// stream carries the phase metrics alongside each event so run history stays
// queryable (`jq` over events.jsonl) without a database; append-only means a
// torn final line after a hard kill is discarded, not corrupting.
func (s *Supervisor) logf(level Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()

	if s.logFile != nil {
		// Go layout string: the reference time "2006-01-02 15:04:05-0700"
		// rendered with the actual timestamp, i.e. YYYY-MM-DD HH:MM:SS±TZ.
		fmt.Fprintf(s.logFile, "%s  %s\n", now.Format("2006-01-02 15:04:05-0700"), msg)
	}
	if s.eventsFile != nil {
		record := map[string]any{
			"ts": now.Unix(), "time": now.Format(time.RFC3339), "level": levelName(level), "message": msg,
		}
		s.mu.Lock()
		if s.current >= 0 {
			p := s.phases[s.current]
			record["phase"] = p.Name
			record["attempt"] = p.Attempt
			record["done"] = p.Done
			record["total"] = p.Total
			record["last_object_id"] = p.LastObjectID
			if p.Version > 0 {
				record["version"] = p.Version
			}
		}
		s.mu.Unlock()
		if data, err := json.Marshal(record); err == nil {
			fmt.Fprintln(s.eventsFile, string(data))
		}
	}

	s.events <- LogEvent{Time: now, Level: level, Message: msg}
}

func levelName(l Level) string {
	switch l {
	case LevelOK:
		return "ok"
	case LevelWarn:
		return "warn"
	case LevelError:
		return "err"
	default:
		return "info"
	}
}

func versionNote(version int) string {
	if version > 0 {
		return fmt.Sprintf(", version %d", version)
	}
	return ""
}

func formatInt(n int64) string {
	if n < 0 {
		return "?"
	}
	s := strconv.FormatInt(n, 10)
	var b strings.Builder
	for i, digit := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(digit)
	}
	return b.String()
}
