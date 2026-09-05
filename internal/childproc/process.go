// Package childproc owns local commands from Start through Wait and cleanup.
package childproc

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Process must be started and waited through this wrapper, not through cmd.
// Every successful Start needs a Wait, including cancellation and failures.
// Unix cleanup covers the inherited local process group, not remote workers
// or descendants that deliberately detach. Only a child's parent can reap it.
type Process struct {
	cmd       *exec.Cmd
	mu        sync.Mutex
	started   bool
	attempted bool
	finished  bool
	deadline  time.Time
	done      chan struct{}
	force     chan struct{}
	forced    bool
	once      sync.Once
	err       error
}

func New(cmd *exec.Cmd) *Process {
	configure(cmd)
	return &Process{cmd: cmd, done: make(chan struct{}), force: make(chan struct{})}
}

func (p *Process) Start() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.attempted || p.finished {
		return errors.New("child process already started or waited")
	}
	p.attempted = true
	if err := p.cmd.Start(); err != nil {
		p.err, p.finished = err, true
		close(p.done)
		return err
	}
	p.started = true
	return nil
}

// Done closes after the direct child has been reaped and local cleanup has
// been attempted, not merely when stdout closes or the group leader exits.
func (p *Process) Done() <-chan struct{} { return p.done }

// Wait calls exec.Cmd.Wait exactly once, draining its output as usual, then
// cleans up lingering local helpers on every exit path, including success.
func (p *Process) Wait() error {
	p.once.Do(func() {
		p.mu.Lock()
		if !p.started {
			if !p.finished {
				p.err = errors.New("child process not started")
				p.finished = true
				close(p.done)
			}
			p.mu.Unlock()
			return
		}
		p.mu.Unlock()
		err := p.cmd.Wait()
		p.mu.Lock()
		deadline := p.deadline
		p.mu.Unlock()
		if !deadline.IsZero() {
			p.waitGrace(deadline)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if cleanupErr := cleanup(p.cmd); cleanupErr != nil && !errors.Is(cleanupErr, os.ErrProcessDone) {
			err = errors.Join(err, fmt.Errorf("local process cleanup failed: %w", cleanupErr))
		}
		p.err, p.finished = err, true
		close(p.done)
	})
	return p.err
}

func (p *Process) waitGrace(deadline time.Time) {
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for groupExists(p.cmd) {
		select {
		case <-p.force:
			return
		case <-timer.C:
			return
		case <-ticker.C:
		}
	}
}

// Kill is also suitable for exec.Cmd.Cancel. Finished commands cannot be
// signalled again by a delayed cancellation or second Ctrl-C.
func (p *Process) Kill() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.started || p.finished {
		return os.ErrProcessDone
	}
	if !p.forced {
		p.forced = true
		close(p.force)
	}
	return kill(p.cmd)
}

// Terminate gives the whole local group a grace period, then force-kills
// survivors. The caller must run Wait concurrently. Leader exit alone does
// not end the grace period; a concurrent Kill overrides it immediately.
func (p *Process) Terminate(grace time.Duration) error {
	if grace <= 0 {
		return p.Kill()
	}
	p.mu.Lock()
	if !p.started || p.finished {
		p.mu.Unlock()
		return os.ErrProcessDone
	}
	if p.forced {
		p.mu.Unlock()
		return nil
	}
	deadline := time.Now().Add(grace)
	if p.deadline.IsZero() || deadline.Before(p.deadline) {
		p.deadline = deadline
	}
	deadline = p.deadline
	err := terminateSignal(p.cmd)
	p.mu.Unlock()
	if err != nil && !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	timer := time.NewTimer(time.Until(deadline))
	defer timer.Stop()
	select {
	case <-p.done:
		return p.err
	case <-p.force:
		return nil
	case <-timer.C:
		return p.Kill()
	}
}
