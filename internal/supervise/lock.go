package supervise

import (
	"fmt"
	"path/filepath"

	"github.com/gofrs/flock"
)

// stateLock is an advisory whole-directory lock held for the life of a run.
//
// Two supervisors sharing a state directory interleave their checkpoint
// writes and drive the same index concurrently; the ES-side index lock
// notices eventually, but only after both have corrupted the local resume
// point. An OS file lock is released by the kernel when the holder dies —
// exactly the semantics wanted after the OOM kill this tool exists to
// survive, so it can never go stale on its own.
type stateLock struct {
	fl *flock.Flock
}

// ErrLocked means another supervisor already holds the state directory.
var ErrLocked = fmt.Errorf("another supervisor already holds this state directory")

func acquireStateLock(stateDir string) (*stateLock, error) {
	fl := flock.New(filepath.Join(stateDir, "supervisor.lock"))
	ok, err := fl.TryLock()
	if err != nil {
		return nil, fmt.Errorf("could not acquire the state lock: %w", err)
	}
	if !ok {
		return nil, ErrLocked
	}
	return &stateLock{fl: fl}, nil
}

func (l *stateLock) Release() {
	if l != nil && l.fl != nil {
		l.fl.Unlock()
	}
}
