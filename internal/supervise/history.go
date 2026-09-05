package supervise

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/jdeepd/vip-index-supervisor/internal/notify"
)

const historySchema = 1
const maxHistoryBytes = 8 << 20

type RunRecord struct {
	Schema          int             `json:"schema"`
	ID              string          `json:"id"`
	ParentID        string          `json:"parent_id,omitempty"`
	Config          Config          `json:"config"`
	WorkingDir      string          `json:"working_directory"`
	StartedAt       time.Time       `json:"started_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
	FinishedAt      time.Time       `json:"finished_at,omitempty"`
	Outcome         string          `json:"outcome"`
	ExitCode        int             `json:"exit_code"`
	Message         string          `json:"message,omitempty"`
	LastError       string          `json:"last_error,omitempty"`
	Phases          []PhaseSnapshot `json:"phases"`
	Attempts        []AttemptRecord `json:"attempts,omitempty"`
	OmittedAttempts int             `json:"omitted_attempts,omitempty"`
}

type AttemptRecord struct {
	Phase      string    `json:"phase"`
	Version    int       `json:"version"`
	Number     int       `json:"number"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at,omitempty"`
	Outcome    string    `json:"outcome"`
	Checkpoint int64     `json:"checkpoint"`
	LogPath    string    `json:"log_path,omitempty"`
}

var runIDPattern = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}\.[0-9]{9}Z-[0-9a-f]{16}$`)

func validRunID(id string) bool { return runIDPattern.MatchString(id) }

func runPath(dir, id string) string { return filepath.Join(dir, "runs", id+".json") }

func (r RunRecord) Validate() error {
	if r.Schema != historySchema || !validRunID(r.ID) || r.StartedAt.IsZero() || !filepath.IsAbs(r.Config.StateDir) || !filepath.IsAbs(r.WorkingDir) {
		return errors.New("unsupported or invalid saved run")
	}
	if err := r.Config.Validate(); err != nil {
		return fmt.Errorf("invalid saved settings: %w", err)
	}
	if len(r.Phases) == 0 || len(r.Phases) != len(r.Config.Indexables) {
		return errors.New("saved phases do not match the run settings")
	}
	for i, p := range r.Phases {
		if p.Name != r.Config.Indexables[i] || p.Version < 0 || p.Attempt < 0 || p.Restarts < 0 || p.LastObjectID < -1 || p.Done < -1 || p.Total < -1 || p.Status < PhasePending || p.Status > PhaseFailed || p.NotifiedPercent < 0 || p.NotifiedPercent > 100 || p.NotifiedPercent%25 != 0 {
			return errors.New("invalid saved phase")
		}
		if p.Version == 0 && (p.Attempt > 0 || p.LastObjectID > 0 || p.IndexingComplete) {
			return errors.New("saved progress has no version")
		}
		if p.Status == PhaseComplete && !p.IndexingComplete {
			return errors.New("completed phase lacks an indexing result")
		}
	}
	switch r.Outcome {
	case "running", "completed", "failed", "interrupted":
	default:
		return errors.New("invalid saved outcome")
	}
	return nil
}

func LoadRun(dir, id string) (RunRecord, error) {
	var r RunRecord
	if !validRunID(id) {
		return r, errors.New("invalid saved run ID")
	}
	f, err := os.Open(runPath(dir, id))
	if err != nil {
		return r, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, maxHistoryBytes+1))
	if err != nil || len(data) > maxHistoryBytes || json.Unmarshal(data, &r) != nil {
		return RunRecord{}, errors.New("saved run is unreadable or corrupt")
	}
	if r.ID != id {
		return RunRecord{}, errors.New("saved run ID does not match its filename")
	}
	if err := r.Validate(); err != nil {
		return RunRecord{}, err
	}
	absolute, err := filepath.Abs(dir)
	if err != nil || filepath.Clean(r.Config.StateDir) != filepath.Clean(absolute) {
		return RunRecord{}, errors.New("saved run belongs to a different state directory")
	}
	return r, nil
}

// A malformed entry is reported, not silently hidden from recovery checks.
func ListRuns(dir string) ([]RunRecord, []string, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "runs"))
	if os.IsNotExist(err) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	var runs []RunRecord
	var warnings []string
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		id := e.Name()[:len(e.Name())-5]
		r, err := LoadRun(dir, id)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: %v", e.Name(), err))
			continue
		}
		runs = append(runs, r)
	}
	latest, _ := latestRunID(dir)
	sort.Slice(runs, func(i, j int) bool {
		if (runs[i].ID == latest) != (runs[j].ID == latest) {
			return runs[i].ID == latest
		}
		return runs[i].ID > runs[j].ID
	})
	return runs, warnings, nil
}

func saveRun(r RunRecord) error {
	if err := r.Validate(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	if len(data) > maxHistoryBytes {
		return errors.New("saved run exceeds history size limit")
	}
	dir := filepath.Join(r.Config.StateDir, "runs")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	return writeHistoryFile(runPath(r.Config.StateDir, r.ID), data)
}

func writeHistoryFile(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".run-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// The pointer also detects superseded runs when the host clock moves backward.
func latestRunID(dir string) (string, error) {
	f, err := os.Open(filepath.Join(dir, "runs", "latest"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 128))
	id := strings.TrimSpace(string(data))
	if err != nil || !validRunID(id) {
		return "", errors.New("latest-run pointer is unreadable")
	}
	return id, nil
}

// Notification credentials are deliberately not part of saved run settings.
// Recovery uses the current session's notification preferences.
func ResumeConfig(r RunRecord, notifications notify.Config) (Config, error) {
	if err := r.Validate(); err != nil {
		return Config{}, err
	}
	cfg := r.Config
	cfg.Indexables = append([]string(nil), cfg.Indexables...)
	cfg.Notifications = notifications
	cfg.ResumeRunID = r.ID
	cfg.ResumeFrom = 0
	cfg.IgnoreLock = false
	return cfg, nil
}

func (s *Supervisor) startHistory() error {
	wd, err := os.Getwd()
	if err != nil {
		return err
	}
	var entropy [8]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return err
	}
	now := time.Now().UTC()
	cfg := s.cfg
	cfg.Notifications = notify.Config{}
	cfg.ResumeRunID = ""
	s.history = &RunRecord{Schema: historySchema, ID: now.Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(entropy[:]), ParentID: s.cfg.ResumeRunID,
		Config: cfg, WorkingDir: wd, StartedAt: now, Outcome: "running", ExitCode: -1}
	if err := s.persistHistory(); err != nil {
		return err
	}
	return writeHistoryFile(filepath.Join(s.cfg.StateDir, "runs", "latest"), []byte(s.history.ID+"\n"))
}

func (s *Supervisor) persistHistory() error {
	if s.history == nil {
		return nil
	}
	s.mu.Lock()
	s.history.Phases = append([]PhaseSnapshot(nil), s.phases...)
	s.mu.Unlock()
	s.history.UpdatedAt = time.Now().UTC()
	return saveRun(*s.history)
}

func (s *Supervisor) recordAttempt(indexable string, version, number int) error {
	if s.history == nil {
		return nil
	}
	if len(s.history.Attempts) >= 200 {
		s.history.Attempts = s.history.Attempts[1:]
		s.history.OmittedAttempts++
	}
	s.history.Attempts = append(s.history.Attempts, AttemptRecord{Phase: indexable, Version: version, Number: number, StartedAt: time.Now().UTC(), Outcome: "running"})
	return s.persistHistory()
}

func (s *Supervisor) finishAttempt(out attemptOutcome) error {
	if s.history == nil || len(s.history.Attempts) == 0 {
		return nil
	}
	a := &s.history.Attempts[len(s.history.Attempts)-1]
	a.FinishedAt, a.Checkpoint, a.LogPath = time.Now().UTC(), s.lastObjectID(), out.logPath
	a.Outcome = out.exitNote()
	if out.success {
		a.Outcome = "indexing completed"
	}
	if out.lockError {
		a.Outcome = "lock refused"
	}
	if out.fatal != "" {
		a.Outcome = "non-retryable error"
	}
	return s.persistHistory()
}

func (s *Supervisor) finishHistory(event DoneEvent) DoneEvent {
	if s.history == nil {
		return event
	}
	s.history.FinishedAt = time.Now().UTC()
	s.history.ExitCode, s.history.Message = event.ExitCode, event.Message
	s.history.Outcome = "failed"
	if event.ExitCode == 0 {
		s.history.Outcome = "completed"
	}
	if event.ExitCode == 130 {
		s.history.Outcome = "interrupted"
	}
	if err := s.persistHistory(); err != nil {
		s.logf(LevelError, "could not save run outcome: %v; inspect logs before resuming", err)
		if event.ExitCode == 0 {
			event.ExitCode, event.Message = 1, "indexing completed but run history could not be saved; inspect recovery"
		}
	}
	return event
}

// Only called by the UI after the event channel closes.
func (s *Supervisor) SavedRunID() string {
	if s.history == nil {
		return ""
	}
	return s.history.ID
}
