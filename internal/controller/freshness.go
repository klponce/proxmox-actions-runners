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
	// runnerCheckTimeout bounds one comparison.
	runnerCheckTimeout = 10 * time.Second
)

// checkRunnerFreshness compares the runner in the newest template with the latest actions/runner release, at most
// every runnerCheckInterval. Our runners don't update themselves, so GitHub requires a newer runner within 30 days of
// a release (github.RunnerUpdateDeadline); this makes a missed update visible in time to install a new runner image.
// The lookup runs in the background, so a slow GitHub doesn't hold up the reconcile loop.
func (c *Controller) checkRunnerFreshness(ctx context.Context, template *proxmox.VM) {
	now := c.now()
	c.mu.Lock()
	due := template != nil && !now.Before(c.nextRunnerCheck)
	if due {
		// Hold off further lookups until this one finishes and sets the real time.
		c.nextRunnerCheck = now.Add(runnerCheckRetry)
	}
	c.mu.Unlock()
	if !due {
		return
	}
	tpl := *template
	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		next := c.compareRunner(ctx, tpl, now)
		c.mu.Lock()
		c.nextRunnerCheck = next
		c.mu.Unlock()
	}()
}

// compareRunner looks up the latest actions/runner release and logs whether template is behind it. It returns when
// to look again: runnerCheckInterval from now after a lookup, runnerCheckRetry after a failure.
func (c *Controller) compareRunner(ctx context.Context, template proxmox.VM, now time.Time) time.Time {
	ctx, cancel := context.WithTimeout(ctx, runnerCheckTimeout)
	defer cancel()
	latest, err := c.gh.LatestRunnerRelease(ctx)
	if err != nil {
		c.logger.WarnContext(ctx, "checking for a newer actions/runner release failed", slog.String("error", err.Error()))
		return now.Add(runnerCheckRetry)
	}

	have, _ := vmtags.String(template, vmtags.RunnerVersionPrefix)
	level, behind := runnerFreshness(have, latest, now)
	if !behind {
		return now.Add(runnerCheckInterval)
	}
	c.logger.Log(ctx, level, "the runner template's actions/runner is older than the latest release; install a "+
		"newer runner image (parcon update on the Proxmox host) before GitHub stops accepting it",
		slog.Int("templateVmid", template.VMID), slog.String("templateRunner", have),
		slog.String("latestRunner", latest.Version), slog.Time("released", latest.PublishedAt),
		slog.Time("deadline", latest.PublishedAt.Add(github.RunnerUpdateDeadline)))
	return now.Add(runnerCheckInterval)
}

// runnerFreshness says whether a template with runner version have is behind latest, and how loudly to say so: a
// warning from github.RunnerUpdateWarnAfter after the release, an error from github.RunnerUpdateErrorAfter.
func runnerFreshness(have string, latest github.RunnerRelease, now time.Time) (slog.Level, bool) {
	switch latest.Staleness(have, now) {
	case github.Current:
		return 0, false
	case github.BehindError:
		return slog.LevelError, true
	case github.BehindWarn:
		return slog.LevelWarn, true
	default:
		return slog.LevelInfo, true
	}
}
