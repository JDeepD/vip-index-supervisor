package supervise

import "time"

// Level grades a log event for display.
type Level int

const (
	LevelInfo Level = iota
	LevelOK
	LevelWarn
	LevelError
)

// PhaseStatus is where one indexable currently stands.
type PhaseStatus int

const (
	PhasePending PhaseStatus = iota
	PhaseRunning
	PhaseComplete
	PhaseFailed
)

// PhaseSnapshot is an immutable view of one phase, safe to hand to the UI.
type PhaseSnapshot struct {
	Name                string
	Status              PhaseStatus
	StatusNote          string // "indexing", "clearing lock", "backoff 20s", ...
	Attempt             int
	Restarts            int
	Version             int    // 0 = the live index
	VersionCreated      string // when available, distinguishes a recreated numeric slot
	Done                int64
	Total               int64
	LastObjectID        int64
	Rate                float64 // objects/second, trailing window
	ETA                 time.Duration
	Elapsed             time.Duration
	IndexingComplete    bool // clean indexing exit; verification/activation may still be pending
	RegistrationPending bool // interrupted registration must not silently create another version
	NotifiedPercent     int  // milestones already queued; persisted across saved-run resumes
}

// Fraction is completion in [0,1], or 0 when the total is unknown.
func (p PhaseSnapshot) Fraction() float64 {
	if p.Total > 0 && p.Done >= 0 {
		f := float64(p.Done) / float64(p.Total)
		if f > 1 {
			return 1
		}
		return f
	}
	return 0
}

// Snapshot is the whole run's state at one moment.
type Snapshot struct {
	Phases  []PhaseSnapshot
	Current int // index into Phases, -1 when between phases
}

// Event is anything the supervisor tells the outside world. The UI receives
// these over a channel and never touches supervisor internals.
type Event interface{ isEvent() }

// LogEvent is one timestamped line for the event log.
type LogEvent struct {
	Time    time.Time
	Level   Level
	Message string
}

// ProgressEvent carries a fresh state snapshot.
type ProgressEvent struct{ State Snapshot }

// DoneEvent is the final event; the channel closes after it.
type DoneEvent struct {
	ExitCode int
	Message  string
}

func (LogEvent) isEvent()      {}
func (ProgressEvent) isEvent() {}
func (DoneEvent) isEvent()     {}
