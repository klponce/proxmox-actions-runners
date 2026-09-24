package github

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/actions/scaleset"
)

// ErrJobStillRunning means a runner can't be removed because it is running a job.
var ErrJobStillRunning = errors.New("runner is still running a job")

// WorkFolder is where runners check out and build, the same path as on GitHub-hosted runners. The runner template
// creates it (images/ubuntu-26.04/10-runner.sh).
const WorkFolder = "/home/runner/work"

// redacted replaces a JIT config wherever it could be printed.
const redacted = "[REDACTED]"

// JITConfig is a single-use runner registration for one worker. It is a credential until the runner uses it: pass
// it to the VM through the guest agent and nowhere else. Printing or logging a JITConfig shows only the runner's
// ID and name.
type JITConfig struct {
	RunnerID   int64
	RunnerName string
	encoded    string
}

// NewJITConfig returns a JITConfig holding encoded. It is for fakes of this package's Client; real configs come
// from GenerateJITConfig.
func NewJITConfig(runnerID int64, runnerName, encoded string) JITConfig {
	return JITConfig{RunnerID: runnerID, RunnerName: runnerName, encoded: encoded}
}

// Encoded returns the config the runner reads, which run.sh takes with --jitconfig. Never log it.
func (j JITConfig) Encoded() string { return j.encoded }

// String keeps the encoded config out of fmt output.
func (j JITConfig) String() string {
	return fmt.Sprintf("JITConfig{RunnerID: %d, RunnerName: %q, Encoded: %s}", j.RunnerID, j.RunnerName, redacted)
}

// GoString keeps the encoded config out of %#v output.
func (j JITConfig) GoString() string { return j.String() }

// LogValue keeps the encoded config out of slog output.
func (j JITConfig) LogValue() slog.Value {
	return slog.GroupValue(slog.Int64("runnerId", j.RunnerID), slog.String("runnerName", j.RunnerName))
}

// GenerateJITConfig registers a runner named runnerName in a scale set and returns its JIT config. The name must be
// unique.
func (c *Client) GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName string) (JITConfig, error) {
	cfg, err := c.ss.GenerateJitRunnerConfig(ctx,
		&scaleset.RunnerScaleSetJitRunnerSetting{Name: runnerName, WorkFolder: WorkFolder}, scaleSetID)
	if err != nil {
		return JITConfig{}, fmt.Errorf("generate JIT config for runner %q: %w", runnerName, err)
	}
	if cfg == nil || cfg.Runner == nil || cfg.EncodedJITConfig == "" {
		return JITConfig{}, fmt.Errorf("generate JIT config for runner %q: GitHub returned no config", runnerName)
	}
	return JITConfig{RunnerID: int64(cfg.Runner.ID), RunnerName: cfg.Runner.Name, encoded: cfg.EncodedJITConfig}, nil
}

// Runner is a runner registered with GitHub.
type Runner struct {
	ID         int64
	Name       string
	ScaleSetID int
}

// RunnerByName returns the registered runner with that name, or nil if there is none.
func (c *Client) RunnerByName(ctx context.Context, name string) (*Runner, error) {
	ref, err := c.ss.GetRunnerByName(ctx, name)
	if err != nil {
		return nil, fmt.Errorf("find runner %q: %w", name, err)
	}
	if ref == nil {
		return nil, nil
	}
	return &Runner{ID: int64(ref.ID), Name: ref.Name, ScaleSetID: ref.RunnerScaleSetID}, nil
}

// RemoveRunner unregisters a runner. Removing a runner that is already gone succeeds, so it is safe to retry. A
// runner in the middle of a job returns an error wrapping ErrJobStillRunning.
func (c *Client) RemoveRunner(ctx context.Context, id int64) error {
	err := c.ss.RemoveRunner(ctx, id)
	switch {
	case err == nil || errors.Is(err, scaleset.RunnerNotFoundError):
		return nil
	case errors.Is(err, scaleset.JobStillRunningError):
		// Callers use this package's sentinel, so they never need to import actions/scaleset.
		return fmt.Errorf("remove runner %d: %w: %w", id, ErrJobStillRunning, err)
	default:
		return fmt.Errorf("remove runner %d: %w", id, err)
	}
}
