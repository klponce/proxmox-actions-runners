package controller

import (
	"context"
	"sync"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// scaleSetState is one scale set's view of what GitHub wants. It implements github.Handler: the listener reports
// into it, and the reconcile loop reads it. Handler methods only record and wake the loop, because the listener
// doesn't fetch the next message until they return.
type scaleSetState struct {
	cfg  config.ScaleSet
	wake func()
	// id is GitHub's ID for the scale set, set once before the listener starts.
	id int

	mu sync.Mutex
	// desired is how many workers GitHub's last message calls for, once known is true. Until the first message the
	// controller neither adds nor retires workers, so a restart doesn't disturb running ones.
	desired int
	known   bool
	// jobs maps runner names to the job each is running, from JobStarted until JobCompleted.
	jobs map[string]string
	// retryAfter holds off new workers after a failed creation.
	retryAfter time.Time
}

func newScaleSetState(cfg config.ScaleSet, wake func()) *scaleSetState {
	return &scaleSetState{cfg: cfg, wake: wake, jobs: map[string]string{}}
}

// DesiredRunners implements github.Handler. The target is minRunners idle workers on top of one per assigned job,
// capped at maxRunners.
func (s *scaleSetState) DesiredRunners(_ context.Context, assignedJobs int) (int, error) {
	target := min(s.cfg.MaxRunners, s.cfg.MinRunners+max(assignedJobs, 0))
	s.mu.Lock()
	changed := !s.known || s.desired != target
	s.desired, s.known = target, true
	s.mu.Unlock()
	if changed {
		s.wake()
	}
	return target, nil
}

// JobStarted implements github.Handler.
func (s *scaleSetState) JobStarted(_ context.Context, job github.Job) error {
	s.mu.Lock()
	s.jobs[job.RunnerName] = job.JobID
	s.mu.Unlock()
	return nil
}

// JobCompleted implements github.Handler. The worker powers itself off after its job, and the next pass
// destroys it.
func (s *scaleSetState) JobCompleted(_ context.Context, job github.Job) error {
	s.mu.Lock()
	delete(s.jobs, job.RunnerName)
	s.mu.Unlock()
	s.wake()
	return nil
}

// target returns the desired worker count and whether it is known yet.
func (s *scaleSetState) target() (int, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.desired, s.known
}

// jobFor returns the job a runner is running, if the controller has heard of one.
func (s *scaleSetState) jobFor(runner string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job, ok := s.jobs[runner]
	return job, ok
}

// forget drops what the scale set knows about a runner whose worker is gone.
func (s *scaleSetState) forget(runner string) {
	s.mu.Lock()
	delete(s.jobs, runner)
	s.mu.Unlock()
}

func (s *scaleSetState) backOff(until time.Time) {
	s.mu.Lock()
	s.retryAfter = until
	s.mu.Unlock()
}

func (s *scaleSetState) canCreate(now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return !now.Before(s.retryAfter)
}

var _ github.Handler = (*scaleSetState)(nil)
