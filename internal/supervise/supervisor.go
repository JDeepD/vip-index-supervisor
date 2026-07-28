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
	stopped    bool
	forced     bool
	samples    []rateSample
	phaseStart time.Time
	startedAt  time.Time
	deadline   time.Time

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

// RequestStop asks the run to end at the next safe point. force skips the
// child's grace period — the user has already waited once.
func (s *Supervisor) RequestStop(force bool) {
	s.mu.Lock()
	s.stopped = true
	if force {
		s.forced = true
	}
	child := s.child
	forced := s.forced
	s.mu.Unlock()
	if child != nil {
		grace := 2 * time.Second
		if forced {
			grace = 0
		}
		terminateProcessTree(child, grace)
	}
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

	s.startedAt = time.Now()
	if s.cfg.MaxDuration > 0 {
		s.deadline = s.startedAt.Add(s.cfg.MaxDuration)
	}
	s.logf(LevelInfo, "===== supervisor start (target: %s, phases: %s, strategy: %s) =====",
		s.cfg.Target.Label(), strings.Join(s.cfg.Indexables, ", "), s.cfg.Strategy)

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
			s.setStatusNote("clearing stale index lock")
			s.logf(LevelWarn, "[%s] stale index lock — clearing (delete-transient)", indexable)
			s.client.ClearIndexLock(ctx)
			if !s.sleep(ctx, s.cfg.BackoffBase) {
				return false
			}
			continue // a stale lock is not a failed attempt
		}

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

// resolveStartCheckpoint picks where indexing resumes from. When building
// into a separate version, only our own checkpoint is meaningful: the live
// get-last-indexed-post-id tracks whatever last wrote to the *active* index,
// and trusting it would skip every object above that ID in a brand new,
// empty index.
func (s *Supervisor) resolveStartCheckpoint(ctx context.Context, indexable string, version int) int64 {
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

	s.mu.Lock()
	s.child = cmd
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.child = nil
		s.mu.Unlock()
	}()

	lines := make(chan string)
	go func() {
		defer close(lines)
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
			switch {
			case s.stopRequested():
				terminateProcessTree(cmd, s.killGrace())
			case s.pastDeadline():
				s.logf(LevelWarn, "[%s] time budget exhausted — stopping the running index", indexable)
				terminateProcessTree(cmd, 2*time.Second)
				outcome.deadline = true
			case time.Since(lastOutput) > s.cfg.StallTimeout:
				s.logf(LevelWarn, "[%s] no output for %s — killing stalled run", indexable, s.cfg.StallTimeout)
				terminateProcessTree(cmd, 2*time.Second)
				outcome.stalled = true
			}
			s.emitProgress()
		case <-ctxDone:
			terminateProcessTree(cmd, 0)
			ctxDone = nil // fires once; a closed channel would spin this loop
		}
	}

	outcome.exitErr = cmd.Wait()
	if outcome.success {
		outcome.exitErr = nil
	}
	return outcome
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
		ph.Total = p.Total
	}
	if p.Done != vipsearch.NoValue {
		ph.Done = p.Done
		s.samples = append(s.samples, rateSample{at: time.Now(), done: p.Done})
		s.trimSamplesLocked()
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

func (s *Supervisor) killGrace() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.forced {
		return 0
	}
	return 2 * time.Second
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
