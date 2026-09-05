package supervise

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/childproc"
	"github.com/jdeepd/vip-index-supervisor/internal/notify"
	"github.com/jdeepd/vip-index-supervisor/internal/vipsearch"
)

// Supervisor runs the indexing phases, restarting after every survivable
// failure and checkpointing progress so a restart never loses work. It talks
// to the outside world only through its event channel.
type Supervisor struct {
	cfg           Config
	client        searchClient
	store         checkpointStore
	notifications *notify.Dispatcher
	history       *RunRecord
	resumed       *RunRecord
	events        chan Event
	// Internal boundaries keep retry policy testable without real waits or VIP.
	attempt func(context.Context, string, int, []string) attemptOutcome
	wait    func(context.Context, time.Duration) bool

	mu         sync.Mutex
	phases     []PhaseSnapshot
	current    int
	child      *exec.Cmd
	childGone  chan struct{}
	stopped    bool
	forced     bool
	cancelRun  context.CancelFunc
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

type searchClient interface {
	Versions(context.Context, string) []vipsearch.IndexVersion
	Status(context.Context) *vipsearch.IndexingStatus
	AddVersion(context.Context, string) vipsearch.RunResult
	ActivateVersion(context.Context, string, int) vipsearch.RunResult
	ClearIndexLock(context.Context) vipsearch.RunResult
	StopIndexing(context.Context) vipsearch.RunResult
	LastResult() vipsearch.RunResult
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
	s := &Supervisor{
		cfg:     cfg,
		client:  vipsearch.NewClient(cfg.Target),
		store:   checkpointStore{dir: cfg.StateDir, postTypes: cfg.PostTypes},
		events:  make(chan Event, 1024),
		phases:  phases,
		current: -1,
	}
	s.attempt, s.wait = s.runAttempt, s.sleep
	return s
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
	child, gone, cancel := s.child, s.childGone, s.cancelRun
	s.mu.Unlock()
	if cancel != nil && (force || child == nil) {
		cancel()
	}

	if force && child != nil {
		childproc.Terminate(child, 0, gone)
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
	defer s.closeLogs()
	defer s.closeNotifications()
	s.startNotifications()
	ctx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()
	s.mu.Lock()
	s.cancelRun = cancelRun
	s.mu.Unlock()
	defer func() { s.mu.Lock(); s.cancelRun = nil; s.mu.Unlock() }()
	if err := s.cfg.Validate(); err != nil {
		s.finish(DoneEvent{ExitCode: 2, Message: err.Error()})
		return
	}
	if s.cfg.MaxDuration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, s.cfg.MaxDuration)
		defer cancel()
	}

	if err := s.prepareStateDir(); err != nil {
		s.finish(DoneEvent{ExitCode: 2, Message: err.Error()})
		return
	}

	if !s.cfg.IgnoreLock {
		lock, err := acquireStateLock(s.cfg.StateDir)
		if err != nil {
			s.finish(DoneEvent{ExitCode: 2, Message: fmt.Sprintf(
				"%v\nConcurrent runs corrupt each other's checkpoints. Wait for it to finish or use a different state dir.", err)})
			return
		}
		defer lock.Release()
	}

	if err := s.restoreRun(ctx); err != nil {
		s.finish(DoneEvent{ExitCode: 2, Message: "saved-run recovery blocked: " + err.Error()})
		return
	}
	if err := s.startHistory(); err != nil {
		s.finish(DoneEvent{ExitCode: 2, Message: "could not create run history: " + err.Error()})
		return
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
	s.notifyChange("Run started", "Starting "+strings.Join(s.cfg.Indexables, ", ")+" ("+s.cfg.Strategy.String()+").", 3, "arrow_forward")

	if problems := s.preflight(ctx); len(problems) > 0 {
		if s.finishInterrupted(ctx) {
			return
		}
		for _, problem := range problems {
			s.logf(LevelError, "preflight: %s", problem)
		}
		s.finish(DoneEvent{ExitCode: 2, Message: "preflight failed — nothing was started"})
		return
	}
	s.logf(LevelInfo, "preflight ok — every indexable is registered")

	for i, indexable := range s.cfg.Indexables {
		if s.stopRequested() || ctx.Err() != nil {
			break
		}
		if s.phases[i].Status == PhaseComplete {
			s.logf(LevelInfo, "[%s] already completed in the saved run — skipping", indexable)
			continue
		}
		s.beginPhase(i)
		if !s.runPhase(ctx, indexable) {
			if s.finishInterrupted(ctx) {
				return
			}
			s.updatePhase(func(p *PhaseSnapshot) { p.Status = PhaseFailed })
			cause := ""
			if s.history != nil {
				cause = s.history.LastError
			}
			s.logf(LevelError, "phase '%s' did not complete", indexable)
			if cause != "" {
				s.history.LastError = cause
			}
			s.finish(DoneEvent{ExitCode: 1, Message: fmt.Sprintf("phase '%s' did not complete", indexable)})
			return
		}
	}

	if s.finishInterrupted(ctx) {
		return
	}
	elapsed := time.Since(s.startedAt).Round(time.Second)
	s.logf(LevelOK, "ALL PHASES COMPLETE (%s) in %s", strings.Join(s.cfg.Indexables, ", "), elapsed)
	s.finish(DoneEvent{ExitCode: 0, Message: "all phases complete in " + elapsed.String()})
}

func (s *Supervisor) finishInterrupted(ctx context.Context) bool {
	if s.stopRequested() || ctx.Err() == context.Canceled {
		s.logf(LevelWarn, "interrupted by user")
		s.finish(DoneEvent{ExitCode: 130, Message: "interrupted — re-run to resume from the checkpoint"})
		return true
	}
	if ctx.Err() == context.DeadlineExceeded || s.pastDeadline() {
		s.finish(DoneEvent{ExitCode: 1, Message: "time budget exhausted — re-run to resume from the checkpoint"})
		return true
	}
	return false
}

// -- one phase --------------------------------------------------------------

func (s *Supervisor) runPhase(ctx context.Context, indexable string) bool {
	version, err := s.resolveVersion(ctx, indexable)
	if err != nil {
		s.logf(LevelError, "phase '%s' aborted: %v", indexable, err)
		return false
	}
	s.updatePhase(func(p *PhaseSnapshot) { p.Version = version; p.Status = PhaseRunning })
	if err := s.persistHistory(); err != nil {
		s.logf(LevelError, "could not save selected version: %v", err)
		return false
	}
	if s.phases[s.current].IndexingComplete {
		s.logf(LevelInfo, "[%s] indexing already finished — retrying completion checks only", indexable)
		return s.completePhase(ctx, indexable, version, attemptOutcome{success: true, indexed: vipsearch.NoValue})
	}

	checkpoint := s.resolveStartCheckpoint(ctx, indexable, version)
	setupPending := s.cfg.Strategy == StrategySetup && s.resumed == nil
	if setupPending || (s.resumed != nil && checkpoint <= 0) {
		if err := s.store.ClearCheckpoint(indexable, version); err != nil {
			s.logf(LevelError, "[%s] cannot clear the old checkpoint before --setup: %v", indexable, err)
			return false
		}
	}
	if checkpoint > 0 {
		if err := s.store.WriteCheckpoint(indexable, version, checkpoint); err != nil {
			s.logf(LevelError, "[%s] cannot save starting checkpoint: %v", indexable, err)
			return false
		}
		s.updatePhase(func(p *PhaseSnapshot) { p.LastObjectID = checkpoint })
		s.logf(LevelInfo, "phase '%s' starting (resume from id %s%s)",
			indexable, formatInt(checkpoint), versionNote(version))
	} else {
		s.logf(LevelInfo, "phase '%s' starting (from the top%s)", indexable, versionNote(version))
	}

	noProgress := 0
	lockErrors := 0
	clearedIdleLock := false
	backoff := s.cfg.BackoffBase
	priorAttempts := s.phases[s.current].Attempt

	for !s.stopRequested() && ctx.Err() == nil {
		if s.pastDeadline() {
			s.logf(LevelError, "[%s] time budget exhausted — stopping. Re-run to resume from id %s.",
				indexable, formatInt(s.store.ReadCheckpoint(indexable, version)))
			return false
		}

		attempt := s.nextAttempt()
		if attempt-priorAttempts > s.cfg.MaxRetries {
			s.logf(LevelError, "[%s] exceeded max retries — aborting", indexable)
			return false
		}

		if cp := s.store.ReadCheckpoint(indexable, version); cp > 0 {
			checkpoint = cp
		}
		argsAttempt := attempt
		if setupPending {
			argsAttempt = 1
		} else if s.cfg.Strategy == StrategySetup {
			argsAttempt = max(2, argsAttempt) // saved-run recovery never repeats --setup
		}
		args := s.buildIndexArgs(indexable, argsAttempt, checkpoint, version)
		s.logAttemptStart(indexable, attempt, checkpoint, args)
		if err := s.recordAttempt(indexable, version, attempt); err != nil {
			s.logf(LevelError, "[%s] could not save attempt before starting: %v", indexable, err)
			return false
		}

		before := s.lastObjectID()
		outcome := s.attempt(ctx, indexable, version, args)
		if outcome.success {
			s.updatePhase(func(p *PhaseSnapshot) { p.IndexingComplete = true })
		}
		if err := s.finishAttempt(outcome); err != nil {
			s.logf(LevelError, "[%s] could not save attempt result: %v", indexable, err)
			return false
		}

		switch {
		case s.stopRequested() || ctx.Err() != nil:
			return false
		case outcome.fatal != "":
			s.updatePhase(func(p *PhaseSnapshot) { p.Status = PhaseFailed; p.StatusNote = outcome.fatal })
			s.logf(LevelError, "[%s] %s — not retryable, aborting. See %s", indexable, outcome.fatal, outcome.logPath)
			return false
		case outcome.deadline:
			s.logf(LevelError, "[%s] stopped: time budget exhausted", indexable)
			return false
		case outcome.success:
			return s.completePhase(ctx, indexable, version, outcome)
		}

		s.updatePhase(func(p *PhaseSnapshot) { p.Restarts++ })

		if outcome.lockError && !outcome.progressed {
			lockErrors++
			if lockErrors >= maxConsecutiveLockErrors {
				// Cleanup is bounded to once, and only after known-idle status.
				if !clearedIdleLock {
					cleared, diagnosed := s.diagnoseBlockingSync(ctx, indexable)
					if cleared {
						clearedIdleLock = true
						lockErrors = 0
						if !s.wait(ctx, s.cfg.BackoffBase) {
							return false
						}
						continue
					}
					if diagnosed {
						return false // the failure was already explained
					}
				}
				s.reportPersistentLock(ctx, indexable, clearedIdleLock)
				return false
			}
			lockWait := lockRetryDelay(lockErrors)
			s.setStatusNote("index lock reported — waiting")
			s.logf(LevelWarn, "[%s] index lock reported — retrying in %s without changing remote state", indexable, lockWait)
			s.notifyRetry(indexable, "Index lock reported; retrying in "+lockWait.String()+" without clearing remote state.")
			if !s.wait(ctx, lockWait) {
				return false
			}
			continue // a stale lock is not a failed attempt
		}
		lockErrors = 0
		if setupPending && !outcome.progressed {
			s.logf(LevelError, "[%s] --setup exited before confirming progress; setup may be incomplete. Inspect %s before choosing rebuild or resume.", indexable, outcome.logPath)
			return false
		}
		setupPending = false

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
		s.notifyRetry(indexable, "Indexing attempt ended; retrying in "+backoff.String()+" from the saved checkpoint.")
		if !s.wait(ctx, backoff) {
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
	if err := s.store.ClearCheckpoint(indexable, version); err != nil {
		s.logf(LevelError, "[%s] indexing completed but the checkpoint could not be removed: %v", indexable, err)
		return false
	}

	s.mu.Lock()
	p := &s.phases[s.current]
	p.Status = PhaseComplete
	p.StatusNote = ""
	p.IndexingComplete = true
	p.NotifiedPercent = 100
	indexed := outcome.indexed
	if indexed == vipsearch.NoValue {
		indexed = p.Done
	}
	elapsed := time.Since(s.phaseStart)
	attempts := p.Attempt
	s.mu.Unlock()
	if err := s.persistHistory(); err != nil {
		s.logf(LevelError, "[%s] could not save phase completion: %v", indexable, err)
		return false
	}

	s.logf(LevelOK, "[%s] COMPLETE — %s objects in %s (%d attempt(s))",
		indexable, formatInt(indexed), elapsed.Round(time.Second), attempts)
	// The final run notification supplies the final phase's 100% alert.
	if s.current < len(s.phases)-1 {
		s.notifyChange("100% — "+indexable, indexable+" completed in "+elapsed.Round(time.Second).String()+".", 3, "white_check_mark")
	}
	s.emitProgress()
	return true
}

// Lock refusals get two waits before diagnosis. They are not proof of a dead
// worker, so neither wait deletes any state.
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

// Identical status in this window is not proof that a remote process is dead.
const syncFreezeProbeDelay = 15 * time.Second

// diagnoseBlockingSync never deletes state reported as active or unknown.
// Two reads distinguish movement from apparent inactivity, not live from dead.
//
// cleared says the block is gone; diagnosed says the situation was already
// explained to the user, so the caller must not add a second, vaguer verdict.
func (s *Supervisor) diagnoseBlockingSync(ctx context.Context, indexable string) (cleared, diagnosed bool) {
	first := s.client.Status(ctx)
	if first == nil {
		return false, false
	}
	if !first.Indexing {
		return s.clearIdleLock(ctx, indexable), true
	}
	s.setStatusNote("blocking sync found — probing whether it is alive")
	s.logf(LevelInfo, "[%s] a %s sync reports in-progress — re-reading status in %s to see if it is advancing",
		indexable, syncMethod(first), syncFreezeProbeDelay)
	if !s.wait(ctx, syncFreezeProbeDelay) {
		return false, true
	}
	second := s.client.Status(ctx)
	if second == nil {
		return false, false
	}
	if !second.Indexing {
		return s.clearIdleLock(ctx, indexable), true
	}
	if syncFingerprint(first) != syncFingerprint(second) {
		s.logf(LevelError, "[%s] blocking sync changed during the probe; leaving it untouched. Let it finish before resuming.", indexable)
		return false, true
	}
	s.logf(LevelWarn,
		"[%s] blocking %s sync showed no movement in %s (%s). This does not prove it is dead; no remote state was cleared. Confirm no indexer is running before using unlock, then resume.",
		indexable, syncMethod(second), syncFreezeProbeDelay, syncFingerprint(second))
	return false, true
}

func (s *Supervisor) clearIdleLock(ctx context.Context, indexable string) bool {
	if ctx.Err() != nil || s.stopRequested() {
		return false
	}
	res := s.client.ClearIndexLock(ctx)
	if res.Succeeded() {
		s.logf(LevelInfo, "[%s] status reports idle; delete-transient acknowledged, retrying once more", indexable)
		return true
	}
	for _, line := range res.DescribeFailure() {
		s.logf(LevelWarn, "[%s] cleanup: %s", indexable, line)
	}
	return false
}

func (s *Supervisor) reportPersistentLock(ctx context.Context, indexable string, alreadyCleared bool) {
	st := s.client.Status(ctx)
	switch {
	case st != nil && st.Indexing && alreadyCleared:
		s.logf(LevelError,
			"[%s] still locked after an idle-state cleanup — investigate on the platform side (%s get-indexing-status), then re-run.",
			indexable, s.commandHint())
	case st != nil && st.Indexing:
		s.logf(LevelError,
			"[%s] the index lock persists and the platform reports indexing. Inspect it (%s get-indexing-status); do not clear its state without confirming the worker has stopped.",
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
	// Include sync identity and every counter independently; max(a,b) hides
	// movement in the smaller counter and changes between phases/workers.
	data, _ := json.Marshal(st)
	return string(data)
}

func syncMethod(st *vipsearch.IndexingStatus) string {
	if st.Method == "" {
		return "background"
	}
	return st.Method
}

// Only a local, scoped checkpoint is suitable for automatic resume. The
// platform's last-post ID is global across CLI versions and post-type filters.
func (s *Supervisor) resolveStartCheckpoint(ctx context.Context, indexable string, version int) int64 {
	if s.resumed != nil {
		for _, p := range s.resumed.Phases {
			if p.Name == indexable {
				if p.LastObjectID <= 0 && p.Attempt == 0 {
					return s.resumed.Config.ResumeFrom
				}
				return max(0, p.LastObjectID)
			}
		}
	}
	if s.cfg.ResumeFrom > 0 {
		return s.cfg.ResumeFrom
	}
	if s.cfg.Strategy == StrategySetup {
		return 0 // fresh build starts from the top
	}
	return s.store.ReadCheckpoint(indexable, version)
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
	success      bool
	lockError    bool
	progressed   bool
	commandError bool
	stalled      bool
	deadline     bool
	killed       bool // we ended it, rather than it exiting on its own
	fatal        string
	exitErr      error
	indexed      int64
	logPath      string
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
	childproc.Configure(cmd)

	attemptLog, err := os.OpenFile(outcome.logPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		outcome.fatal = fmt.Sprintf("could not create attempt log: %v", err)
		return outcome
	}
	defer attemptLog.Close()
	chunks := make(chan []byte)
	cmd.Stdout = &chunkWriter{chunks: chunks}
	cmd.Stderr = cmd.Stdout
	cmd.WaitDelay = 2 * time.Second
	if err := cmd.Start(); err != nil {
		// Retrying cannot conjure a missing binary; report the fatal it is.
		outcome.fatal = fmt.Sprintf("could not start %q: %v", full[0], err)
		return outcome
	}

	// Wait runs concurrently with output draining. EOF alone does not mean
	// the process exited, and a descendant may keep its output pipe open.
	gone := make(chan struct{})
	waited := make(chan error, 1)
	go func() {
		err := cmd.Wait()
		close(gone)
		waited <- err
	}()

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

	opCtx, cancelOperations := context.WithCancel(ctx)
	var operations sync.WaitGroup
	defer func() { cancelOperations(); operations.Wait() }()
	lastOutput := time.Now()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	ctxDone := ctx.Done()
	var lastStallProbe string
	// Set once a shutdown is under way. Without it, ticks that land between
	// the kill and the pipe reaching EOF would re-run the whole stop sequence.
	stopping := false
	stop := func(graceful bool) {
		if stopping {
			return
		}
		stopping, outcome.killed = true, true
		operations.Add(1)
		go func() {
			defer operations.Done()
			s.stopChild(opCtx, indexable, cmd, gone, graceful)
		}()
	}
	// Oversized diagnostic lines must not stop the pipe reader. Log every
	// byte, but cap the memory used to parse any one line.
	var pending string
	oversized := false
	consume := func(fragment string, end bool) {
		if !oversized {
			if len(pending)+len(fragment) > 1024*1024 {
				pending = ""
				oversized = true
			} else {
				pending += fragment
			}
		}
		if end {
			if !oversized {
				s.consumeLine(indexable, version, pending, &outcome)
			}
			pending, oversized = "", false
		}
	}
	type probeResult struct {
		advancing bool
		started   time.Time
	}
	probes := make(chan probeResult, 1)
	probing := false

reading:
	for {
		select {
		case chunk := <-chunks:
			lastOutput = time.Now()
			if _, err := attemptLog.Write(chunk); err != nil {
				outcome.fatal = fmt.Sprintf("could not write attempt log: %v", err)
			}
			parts := strings.Split(string(chunk), "\n")
			for i, part := range parts {
				consume(part, i < len(parts)-1)
			}
			if outcome.fatal != "" {
				stop(false)
			}
		case err := <-waited:
			outcome.exitErr = err
			if errors.Is(err, exec.ErrWaitDelay) {
				childproc.Terminate(cmd, 0, gone)
			}
			consume("", true)
			break reading
		case probe := <-probes:
			probing = false
			if stopping || lastOutput.After(probe.started) {
				continue
			}
			if probe.advancing {
				lastOutput = time.Now()
			} else {
				s.logf(LevelWarn, "[%s] no output for %s and remote progress could not be confirmed; stopping the local attempt (remote state will not be erased)", indexable, s.cfg.StallTimeout)
				outcome.stalled = true
				stop(false) // no ownership proof for a global stop-indexing request
			}
		case <-ticker.C:
			if stopping {
				s.emitProgress()
				continue
			}
			switch {
			case s.stopRequested():
				stop(!s.forceRequested())
			case s.pastDeadline():
				s.logf(LevelWarn, "[%s] time budget exhausted — stopping the running index", indexable)
				outcome.deadline = true
				stop(true)
			case !probing && time.Since(lastOutput) > s.cfg.StallTimeout:
				probing = true
				started := time.Now()
				operations.Add(1)
				go func() {
					defer operations.Done()
					probes <- probeResult{s.remoteIsAdvancing(opCtx, indexable, &lastStallProbe), started}
				}()
			}
			s.emitProgress()
		case <-ctxDone:
			childproc.Terminate(cmd, 0, gone)
			stopping, outcome.killed = true, true
			outcome.deadline = ctx.Err() == context.DeadlineExceeded
			ctxDone = nil // fires once; a closed channel would spin this loop
		}
	}

	outcome.success = outcome.success && outcome.exitErr == nil && !outcome.killed && outcome.fatal == "" && !outcome.commandError
	if !outcome.success {
		s.logf(LevelWarn, "[%s] attempt ended (%s); raw output: %s", indexable, outcome.exitNote(), outcome.logPath)
	}
	// Local CLI exit does not prove that the remote worker exited, nor that
	// a remaining CLI sync record belongs to us. Never erase it here.
	return outcome
}

type chunkWriter struct{ chunks chan<- []byte }

func (w *chunkWriter) Write(p []byte) (int, error) {
	w.chunks <- append([]byte(nil), p...)
	return len(p), nil
}

// stopChild ends the running attempt. When `graceful`, the platform is asked
// to stop indexing first so ElasticPress can wind down at a batch boundary and
// delete its own sync record — a hard kill denies it that chance, which is how
// a killed run leaves debris that blocks everything after it.
func (s *Supervisor) stopChild(ctx context.Context, indexable string, cmd *exec.Cmd, gone <-chan struct{}, graceful bool) {
	if !graceful {
		childproc.Terminate(cmd, 0, gone)
		return
	}
	select {
	case <-gone:
		return
	default:
	}
	s.setStatusNote("asking the platform to stop indexing")
	// This command runs alongside the output reader and any status probe;
	// use an independent client so LastRun is never shared concurrently.
	res := vipsearch.NewClient(s.cfg.Target).StopIndexing(ctx)
	if !res.Succeeded() && ctx.Err() == nil {
		s.logf(LevelWarn, "[%s] platform stop was not confirmed: %s", indexable, strings.Join(res.DescribeFailure(), "; "))
	}
	select {
	case <-gone: // it wound down on its own; nothing to kill
		return
	case <-time.After(remoteStopGrace):
	case <-ctx.Done():
	}
	childproc.Terminate(cmd, 2*time.Second, gone)
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
	st := vipsearch.NewClient(s.cfg.Target).Status(ctx)
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

func (s *Supervisor) consumeLine(indexable string, version int, line string, outcome *attemptOutcome) {
	// The raw (still colourised) line has already been written to the attempt
	// log; parse a stripped copy, or the escape codes VIP-CLI embeds around
	// "Success:" would hide the completion marker — the exact bug that made
	// the predecessor script abort runs that had in fact finished.
	line = vipsearch.StripANSI(line)
	if vipsearch.IsLockError(line) {
		outcome.lockError = true
	} else if outcome.fatal == "" {
		// A stale lock is retryable and has its own handling, so it must
		// never be classified as fatal.
		outcome.fatal = vipsearch.ClassifyFatal(line)
	}
	if (vipsearch.RunResult{Output: line}).Failed() {
		outcome.commandError = true
	}
	if vipsearch.IsIndexSuccess(line) {
		outcome.success = true
	}

	p := vipsearch.ParseProgress(line)
	if p.Done > 0 || p.LastObjectID > 0 {
		outcome.progressed = true
	}
	if p.IndexedCount != vipsearch.NoValue {
		outcome.indexed = p.IndexedCount
	}
	if err := s.applyProgress(indexable, version, p); err != nil {
		outcome.fatal = fmt.Sprintf("could not persist checkpoint: %v", err)
	}
}

func (s *Supervisor) applyProgress(indexable string, version int, p vipsearch.Progress) error {
	if p.Done == vipsearch.NoValue && p.Total == vipsearch.NoValue && p.LastObjectID == vipsearch.NoValue {
		return nil // diagnostic output must not flood the UI event queue
	}
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
	if p.LastObjectID > 0 {
		if ph.LastObjectID == vipsearch.NoValue || p.LastObjectID < ph.LastObjectID {
			checkpointID = p.LastObjectID
			ph.LastObjectID = p.LastObjectID
		}
	}
	s.mu.Unlock()

	// Checkpoint outside the lock: it is a disk write, and only ever for a
	// strictly lower ID — overlap on resume is a harmless upsert, skipping
	// is not.
	if checkpointID > 0 {
		if err := s.store.WriteCheckpoint(indexable, version, checkpointID); err != nil {
			return err
		}
	}
	if err := s.progressMilestones(); err != nil {
		return err
	}
	s.emitProgress()
	return nil
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
	s.attemptDone, s.attemptTotal = vipsearch.NoValue, vipsearch.NoValue
	s.samples = nil
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
	s.trimSamplesLocked()
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
	return ctx.Err() == nil && !s.stopRequested() && !s.pastDeadline()
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
	stamp := time.Now().Format("20060102-150405.000000000")
	return filepath.Join(s.cfg.StateDir, "logs", fmt.Sprintf("attempt-%s-%s.log", indexable, stamp))
}

func (s *Supervisor) logAttemptStart(indexable string, attempt int, checkpoint int64, args []string) {
	resumeNote := "from top"
	if containsSetup(args) {
		resumeNote = "--setup (fresh)"
	} else if checkpoint > 0 {
		resumeNote = "from id " + formatInt(checkpoint)
	}
	s.setStatusNote("indexing — " + resumeNote)
	full := append(s.cfg.Target.Base(), args...)
	s.logf(LevelInfo, "[%s] attempt %d (%s): %s", indexable, attempt, resumeNote, strings.Join(full, " "))
}

func containsSetup(args []string) bool {
	for _, arg := range args {
		if arg == "--setup" {
			return true
		}
	}
	return false
}

// logf writes to the master log, the JSONL stream, and the UI. The JSONL
// stream carries the phase metrics alongside each event so run history stays
// queryable (`jq` over events.jsonl) without a database; append-only means a
// torn final line after a hard kill is discarded, not corrupting.
func (s *Supervisor) logf(level Level, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	now := time.Now()
	if level == LevelError && s.history != nil {
		s.history.LastError = msg
		if len(s.history.LastError) > 4096 {
			s.history.LastError = s.history.LastError[:4096]
		}
	}

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
