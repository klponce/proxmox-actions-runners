package controller

import (
	"context"
	"log/slog"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

const (
	// runnerCheckInterval is how often the template's runner is compared with the latest actions/runner release.
	runnerCheckInterval = 6 * time.Hour
	// runnerCheckRetry is how soon a failed comparison is tried again.
	runnerCheckRetry = time.Hour
	// runnerCheckTimeout bounds one comparison, which runs inside a reconcile pass.
	runnerCheckTimeout = 10 * time.Second
)

// checkRunnerFreshness compares the runner in the newest template with the latest actions/runner release, at most
// every runnerCheckInterval. Our runners don't update themselves, so GitHub requires a newer runner within 30 days of
// a release (github.RunnerUpdateDeadline); this makes a missed update visible in time to install a new runner image.
func (c *Controller) checkRunnerFreshness(ctx context.Context, template *proxmox.VM) {
	now := c.now()
	if template == nil || now.Before(c.nextRunnerCheck) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, runnerCheckTimeout)
	defer cancel()
	latest, err := c.gh.LatestRunnerRelease(ctx)
	if err != nil {
		c.nextRunnerCheck = now.Add(runnerCheckRetry)
		c.logger.WarnContext(ctx, "checking for a newer actions/runner release failed", slog.String("error", err.Error()))
		return
	}
	c.nextRunnerCheck = now.Add(runnerCheckInterval)

	have, _ := vmtags.String(*template, vmtags.RunnerVersionPrefix)
	level, behind := runnerFreshness(have, latest, now)
	if !behind {
		return
	}
	c.logger.Log(ctx, level, "the runner template's actions/runner is older than the latest release; install a "+
		"newer runner image (install.sh upgrade) before GitHub stops accepting it",
		slog.Int("templateVmid", template.VMID), slog.String("templateRunner", have),
		slog.String("latestRunner", latest.Version), slog.Time("released", latest.PublishedAt),
		slog.Time("deadline", latest.PublishedAt.Add(github.RunnerUpdateDeadline)))
}

// runnerFreshness says whether a template with runner version have is behind latest, and how loudly to say so: a
// warning from github.RunnerUpdateWarnAfter after the release, an error from github.RunnerUpdateErrorAfter.
func runnerFreshness(have string, latest github.RunnerRelease, now time.Time) (slog.Level, bool) {
	if !latest.NewerThan(have) {
		return 0, false
	}
	switch age := now.Sub(latest.PublishedAt); {
	case age >= github.RunnerUpdateErrorAfter:
		return slog.LevelError, true
	case age >= github.RunnerUpdateWarnAfter:
		return slog.LevelWarn, true
	default:
		return slog.LevelInfo, true
	}
}
