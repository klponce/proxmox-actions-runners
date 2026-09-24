package github

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/actions/scaleset"
	"github.com/actions/scaleset/listener"
)

// Job describes a job in a scale set message.
type Job struct {
	// RunnerRequestID identifies the job's request for a runner.
	RunnerRequestID int64
	JobID           string
	// RunnerName and RunnerID are set once a runner has picked the job up.
	RunnerName  string
	RunnerID    int
	Owner       string
	Repository  string
	WorkflowRef string
	DisplayName string
	// Result is set for completed jobs, such as "succeeded", "failed", or "canceled".
	Result string
}

// Stats are GitHub's counts for a scale set, reported with every message.
type Stats struct {
	AvailableJobs     int
	AcquiredJobs      int
	AssignedJobs      int
	RunningJobs       int
	RegisteredRunners int
	BusyRunners       int
	IdleRunners       int
}

// Handler receives a scale set's messages. Calls come one at a time, and the next message isn't fetched until the
// current call returns, so each method must return quickly: record what changed and let the reconciler do slow work
// such as cloning VMs. A returned error ends the session, which Listen then re-creates.
type Handler interface {
	// DesiredRunners reports how many jobs are assigned to the scale set, and returns how many runners the
	// controller will aim for, typically assignedJobs plus minRunners, capped at maxRunners.
	DesiredRunners(ctx context.Context, assignedJobs int) (int, error)
	// JobStarted reports that a runner picked up a job.
	JobStarted(ctx context.Context, job Job) error
	// JobCompleted reports that a job finished, whatever its result. The runner exits after its one job.
	JobCompleted(ctx context.Context, job Job) error
}

// StatsRecorder is optionally implemented by a Handler to receive GitHub's statistics for metrics.
type StatsRecorder interface {
	RecordStats(Stats)
}

// ListenOptions configures Listen.
type ListenOptions struct {
	ScaleSetID int
	// MaxRunners is the capacity reported to GitHub.
	MaxRunners int
	// Owner names this controller in GitHub's session list, such as the controller VM's hostname.
	Owner string
	// MinBackoff and MaxBackoff bound the wait before re-creating a failed session. Zero means 1s and 1m.
	MinBackoff time.Duration
	MaxBackoff time.Duration
}

const (
	defaultMinBackoff = time.Second
	defaultMaxBackoff = time.Minute
	// healthySession is how long a session must run before a failure no longer counts toward backoff.
	healthySession = time.Minute
	// closeTimeout bounds deleting the session on the way out.
	closeTimeout = 30 * time.Second
)

// Listen runs the scale set's message session until ctx ends, passing messages to h. When the session fails, for
// example because GitHub is unreachable or a previous session still exists after a crash, Listen logs the error
// and opens a new session after an exponential backoff. It returns when ctx is canceled.
func (c *Client) Listen(ctx context.Context, opts ListenOptions, h Handler) {
	minBackoff := cmp.Or(opts.MinBackoff, defaultMinBackoff)
	maxBackoff := max(cmp.Or(opts.MaxBackoff, defaultMaxBackoff), minBackoff)

	backoff := minBackoff
	for {
		started := time.Now()
		err := c.listenOnce(ctx, opts, h)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			// actions/scaleset's listener only returns on failure; this keeps err.Error() below safe regardless.
			err = errors.New("session ended without an error")
		}
		if time.Since(started) >= healthySession {
			backoff = minBackoff
		}
		c.logger.WarnContext(ctx, "scale set session ended; reconnecting", slog.Int("scaleSetId", opts.ScaleSetID),
			slog.Duration("backoff", backoff), slog.String("error", err.Error()))

		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
		backoff = min(backoff*2, maxBackoff)
	}
}

// listenOnce runs one message session until it fails or ctx ends.
func (c *Client) listenOnce(ctx context.Context, opts ListenOptions, h Handler) error {
	session, err := c.ss.MessageSessionClient(ctx, opts.ScaleSetID, opts.Owner)
	if err != nil {
		return fmt.Errorf("open session: %w", err)
	}
	defer func() {
		closeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeTimeout)
		defer cancel()
		if err := session.Close(closeCtx); err != nil {
			c.logger.WarnContext(closeCtx, "closing scale set session", slog.Int("scaleSetId", opts.ScaleSetID),
				slog.String("error", err.Error()))
		}
	}()

	a := &adapter{h: h}
	var options []listener.Option
	if rec, ok := h.(StatsRecorder); ok {
		options = append(options, listener.WithMetricsRecorder(&statsAdapter{rec: rec}))
	}
	l, err := listener.New(session, listener.Config{
		ScaleSetID: opts.ScaleSetID,
		MaxRunners: opts.MaxRunners,
		Logger:     c.logger.With(slog.Int("scaleSetId", opts.ScaleSetID)),
	}, options...)
	if err != nil {
		return fmt.Errorf("create listener: %w", err)
	}
	if err := l.Run(ctx, a); err != nil {
		return fmt.Errorf("session: %w", err)
	}
	return nil
}

// adapter turns actions/scaleset's listener callbacks into Handler calls.
type adapter struct {
	h Handler
}

func (a *adapter) HandleDesiredRunnerCount(ctx context.Context, count int) (int, error) {
	return a.h.DesiredRunners(ctx, count)
}

func (a *adapter) HandleJobStarted(ctx context.Context, m *scaleset.JobStarted) error {
	job := jobFrom(m.JobMessageBase)
	job.RunnerName, job.RunnerID = m.RunnerName, m.RunnerID
	return a.h.JobStarted(ctx, job)
}

func (a *adapter) HandleJobCompleted(ctx context.Context, m *scaleset.JobCompleted) error {
	job := jobFrom(m.JobMessageBase)
	job.RunnerName, job.RunnerID, job.Result = m.RunnerName, m.RunnerID, m.Result
	return a.h.JobCompleted(ctx, job)
}

func jobFrom(m scaleset.JobMessageBase) Job {
	return Job{
		RunnerRequestID: m.RunnerRequestID,
		JobID:           m.JobID,
		Owner:           m.OwnerName,
		Repository:      m.RepositoryName,
		WorkflowRef:     m.JobWorkflowRef,
		DisplayName:     m.JobDisplayName,
	}
}

// statsAdapter passes GitHub's statistics to a StatsRecorder.
type statsAdapter struct {
	rec StatsRecorder
}

func (s *statsAdapter) RecordStatistics(st *scaleset.RunnerScaleSetStatistic) {
	if st == nil {
		return
	}
	s.rec.RecordStats(Stats{
		AvailableJobs:     st.TotalAvailableJobs,
		AcquiredJobs:      st.TotalAcquiredJobs,
		AssignedJobs:      st.TotalAssignedJobs,
		RunningJobs:       st.TotalRunningJobs,
		RegisteredRunners: st.TotalRegisteredRunners,
		BusyRunners:       st.TotalBusyRunners,
		IdleRunners:       st.TotalIdleRunners,
	})
}

func (s *statsAdapter) RecordJobStarted(*scaleset.JobStarted)     {}
func (s *statsAdapter) RecordJobCompleted(*scaleset.JobCompleted) {}
func (s *statsAdapter) RecordDesiredRunners(int)                  {}

var (
	_ listener.Scaler          = (*adapter)(nil)
	_ listener.MetricsRecorder = (*statsAdapter)(nil)
)
