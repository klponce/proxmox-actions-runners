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
	// busy holds the runners running a job, from JobStarted (or GitHub refusing to remove the runner) until
	// JobCompleted.
	busy map[string]bool
	// retryAfter holds off new workers after a failed creation.
	retryAfter time.Time

	// For the status: the message session, and GitHub's last statistics.
	sessionOpen  bool
	sessionSince time.Time
	sessionErr   string
	stats        *github.Stats
	now          func() time.Time
}

func newScaleSetState(cfg config.ScaleSet, wake func(), now func() time.Time) *scaleSetState {
	return &scaleSetState{cfg: cfg, wake: wake, busy: map[string]bool{}, now: now}
}

// SessionOpened implements github.SessionObserver.
func (s *scaleSetState) SessionOpened() {
	s.mu.Lock()
	s.sessionOpen, s.sessionSince, s.sessionErr = true, s.now(), ""
	s.mu.Unlock()
}

// SessionEnded implements github.SessionObserver.
func (s *scaleSetState) SessionEnded(err error) {
	s.mu.Lock()
	s.sessionOpen, s.sessionSince, s.sessionErr = false, s.now(), err.Error()
	s.mu.Unlock()
}

// RecordStats implements github.StatsRecorder.
func (s *scaleSetState) RecordStats(st github.Stats) {
	s.mu.Lock()
	s.stats = &st
	s.mu.Unlock()
}

// DesiredRunners implements github.Handler. The target is minRunners idle workers on top of one per assigned job,
// capped at maxRunners.
func (s *scaleSetState) DesiredRunners(_ context.Context, assignedJobs int) (int, error) {
	target := min(s.cfg.MaxRunners, s.cfg.MinRunners+assignedJobs)
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
	s.busy[job.RunnerName] = true
	s.mu.Unlock()
	return nil
}

// JobCompleted implements github.Handler. The worker powers itself off after its job, and the next pass
// destroys it.
func (s *scaleSetState) JobCompleted(_ context.Context, job github.Job) error {
	s.mu.Lock()
	delete(s.busy, job.RunnerName)
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

// hasJob reports whether the controller knows the runner is running a job.
func (s *scaleSetState) hasJob(runner string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.busy[runner]
}

// markRunning records that a runner is in a job the controller didn't hear about: GitHub refused to remove it.
func (s *scaleSetState) markRunning(runner string) {
	s.mu.Lock()
	s.busy[runner] = true
	s.mu.Unlock()
}

// forget drops what the scale set knows about a runner whose worker is gone.
func (s *scaleSetState) forget(runner string) {
	s.mu.Lock()
	delete(s.busy, runner)
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

var (
	_ github.Handler         = (*scaleSetState)(nil)
	_ github.SessionObserver = (*scaleSetState)(nil)
	_ github.StatsRecorder   = (*scaleSetState)(nil)
)
