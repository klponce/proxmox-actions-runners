// Package controller keeps each scale set's worker VMs matched to what GitHub wants.
//
// It is a reconciler (AGENTS.md, invariant 2): GitHub's message sessions report how many runners each scale set
// needs, and a single loop compares that with the worker VMs Proxmox has, then creates, retires, and cleans up VMs
// to converge. All durable state lives in Proxmox tags, so a restarted controller picks up where it left off.
package controller

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

// Proxmox is what the controller needs from the Proxmox VE API. *proxmox.Client implements it.
type Proxmox interface {
	ListVMs(ctx context.Context) ([]proxmox.VM, error)
	Clone(ctx context.Context, opts proxmox.CloneOptions) error
	SetConfig(ctx context.Context, vmid int, settings map[string]string, del ...string) error
	GrowDisk(ctx context.Context, vmid int, disk string, addGiB int) error
	Start(ctx context.Context, vmid int) error
	Stop(ctx context.Context, vmid int) error
	Destroy(ctx context.Context, vmid int) error
	AgentPing(ctx context.Context, vmid int) error
	AgentWriteFile(ctx context.Context, vmid int, path string, content []byte) error
}

// GitHub is what the controller needs from GitHub. *github.Client implements it.
type GitHub interface {
	EnsureScaleSet(ctx context.Context, spec github.ScaleSetSpec) (github.ScaleSet, error)
	GenerateJITConfig(ctx context.Context, scaleSetID int, runnerName string) (github.JITConfig, error)
	RunnerByName(ctx context.Context, name string) (*github.Runner, error)
	RemoveRunner(ctx context.Context, id int64) error
	Listen(ctx context.Context, opts github.ListenOptions, h github.Handler)
	LatestRunnerRelease(ctx context.Context) (github.RunnerRelease, error)
}

var (
	_ Proxmox = (*proxmox.Client)(nil)
	_ GitHub  = (*github.Client)(nil)
)

// Defaults for Options.
const (
	defaultResyncInterval   = 15 * time.Second
	defaultBootTimeout      = 10 * time.Minute
	defaultRunnerCheckAfter = 10 * time.Minute
	defaultRunnerCheckEvery = 5 * time.Minute
	defaultParallelism      = 3
	defaultRetryBackoff     = 10 * time.Second

	// rootDisk is the template's root disk, which each worker grows by its free disk space.
	rootDisk = "scsi0"
)

// Options configures a Controller. Only Config, Proxmox, and GitHub are required.
type Options struct {
	Config  *config.Config
	Proxmox Proxmox
	GitHub  GitHub
	// Owner names this controller in GitHub's session list, such as the controller VM's hostname.
	Owner  string
	Logger *slog.Logger

	// ResyncInterval is how often the loop runs without being woken. Zero means 15s.
	ResyncInterval time.Duration
	// BootTimeout is how long a started worker's guest agent may take to answer before the worker is destroyed. Zero
	// means 10m.
	BootTimeout time.Duration
	// RunnerCheckAfter and RunnerCheckEvery control the check that a ready worker's runner still exists in GitHub:
	// it starts this long after the worker was created and repeats at this interval. Zero means 10m and 5m.
	RunnerCheckAfter time.Duration
	RunnerCheckEvery time.Duration
	// Parallelism bounds how many workers are created or destroyed at once. Zero means 3.
	Parallelism int
	// RetryBackoff is how long a scale set waits after a failed worker creation before trying again. Zero means 10s.
	RetryBackoff time.Duration
	// Now returns the current time. Nil means time.Now.
	Now func() time.Time
	// StatusPath is where the controller writes its status after each pass, for `parcon status`. Empty means
	// nowhere.
	StatusPath string
	// Version is the controller's version, for the status.
	Version string
}

// Controller runs the reconcile loop for every configured scale set.
type Controller struct {
	cfg    *config.Config
	pve    Proxmox
	gh     GitHub
	owner  string
	logger *slog.Logger
	now    func() time.Time

	resync           time.Duration
	bootTimeout      time.Duration
	runnerCheckAfter time.Duration
	runnerCheckEvery time.Duration
	retryBackoff     time.Duration
	// agentPoll is how often a booting worker's guest agent is pinged.
	agentPoll time.Duration

	// scaleSets holds each configured scale set's state, by name.
	scaleSets map[string]*scaleSetState
	// wake asks the loop to run a pass now.
	wake chan struct{}
	// slots bounds concurrent VM operations.
	slots chan struct{}
	ops   sync.WaitGroup

	mu sync.Mutex
	// busy maps the VMIDs with an operation in flight to what it is, so a pass leaves them alone.
	busy map[int]string
	// foreign holds VMIDs Proxmox says are taken although the token can't see them.
	foreign map[int]bool
	// lastRunnerCheck is when each worker's runner was last looked up, by VMID.
	lastRunnerCheck map[int]time.Time

	// nextRunnerCheck is when the template's runner is next compared with the latest release.
	nextRunnerCheck time.Time

	statusPath string
	status     Status
}

// New returns a Controller. It doesn't contact Proxmox or GitHub.
func New(opts Options) (*Controller, error) {
	if opts.Config == nil || opts.Proxmox == nil || opts.GitHub == nil {
		return nil, errors.New("controller: config, Proxmox client, and GitHub client are required")
	}
	c := &Controller{
		cfg:              opts.Config,
		pve:              opts.Proxmox,
		gh:               opts.GitHub,
		owner:            opts.Owner,
		logger:           opts.Logger,
		now:              opts.Now,
		resync:           orDefault(opts.ResyncInterval, defaultResyncInterval),
		bootTimeout:      orDefault(opts.BootTimeout, defaultBootTimeout),
		runnerCheckAfter: orDefault(opts.RunnerCheckAfter, defaultRunnerCheckAfter),
		runnerCheckEvery: orDefault(opts.RunnerCheckEvery, defaultRunnerCheckEvery),
		retryBackoff:     orDefault(opts.RetryBackoff, defaultRetryBackoff),
		agentPoll:        agentPollInterval,
		scaleSets:        map[string]*scaleSetState{},
		wake:             make(chan struct{}, 1),
		busy:             map[int]string{},
		foreign:          map[int]bool{},
		lastRunnerCheck:  map[int]time.Time{},
		statusPath:       opts.StatusPath,
		status:           Status{Schema: StatusSchema, Version: opts.Version},
	}
	if c.logger == nil {
		c.logger = slog.New(slog.DiscardHandler)
	}
	if c.now == nil {
		c.now = time.Now
	}
	parallelism := opts.Parallelism
	if parallelism <= 0 {
		parallelism = defaultParallelism
	}
	c.slots = make(chan struct{}, parallelism)
	for _, s := range c.cfg.ScaleSets {
		c.scaleSets[s.Name] = newScaleSetState(s, c.trigger, c.now)
	}
	return c, nil
}

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

// trigger asks the loop to run a pass as soon as it can. Calls while a pass is pending coalesce.
func (c *Controller) trigger() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// Run registers the scale sets with GitHub, listens to their message sessions, and reconciles until ctx ends. On
// the way out it waits for VM operations in flight to finish, but leaves running workers alone: a restarted
// controller adopts them, so a controller upgrade doesn't cancel jobs.
func (c *Controller) Run(ctx context.Context) error {
	if !c.registerScaleSets(ctx) {
		return nil
	}

	var listeners sync.WaitGroup
	for _, s := range c.scaleSets {
		listeners.Add(1)
		go func() {
			defer listeners.Done()
			c.gh.Listen(ctx, github.ListenOptions{ScaleSetID: s.id, MaxRunners: s.cfg.MaxRunners, Owner: c.owner}, s)
		}()
	}

	ticker := time.NewTicker(c.resync)
	defer ticker.Stop()
	c.status.StartedAt = c.now()
	for {
		err := c.reconcile(ctx)
		if err != nil && ctx.Err() == nil {
			c.logger.WarnContext(ctx, "reconcile pass failed", slog.String("error", err.Error()))
		}
		c.writeStatus(ctx, err)
		select {
		case <-ctx.Done():
			c.logger.InfoContext(ctx, "stopping; running workers are left for the next start")
			listeners.Wait()
			// Creations stop with ctx, but a retirement runs to the end so it doesn't leave a VM half-destroyed.
			c.waitForOps(retireTimeout)
			return nil
		case <-ticker.C:
		case <-c.wake:
		}
	}
}

// registerScaleSets makes sure each scale set exists in GitHub and learns its ID, retrying until ctx ends, because
// GitHub may be unreachable while the controller VM boots. It reports false if ctx ended first.
func (c *Controller) registerScaleSets(ctx context.Context) bool {
	for _, s := range c.scaleSets {
		backoff := time.Second
		for {
			ss, err := c.gh.EnsureScaleSet(ctx, github.ScaleSetSpec{Name: s.cfg.Name, Labels: s.cfg.Labels,
				RunnerGroup: s.cfg.RunnerGroup})
			if err == nil {
				s.id = ss.ID
				c.logger.InfoContext(ctx, "scale set registered", slog.String("scaleSet", s.cfg.Name),
					slog.Int("scaleSetId", ss.ID))
				break
			}
			c.logger.WarnContext(ctx, "registering scale set failed; retrying", slog.String("scaleSet", s.cfg.Name),
				slog.Duration("backoff", backoff), slog.String("error", err.Error()))
			if !sleep(ctx, backoff) {
				return false
			}
			backoff = min(backoff*2, time.Minute)
		}
	}
	return true
}

// waitForOps waits up to timeout for VM operations in flight.
func (c *Controller) waitForOps(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		c.ops.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		c.logger.Warn("gave up waiting for VM operations; the next start cleans up after them")
	}
}

// sleep waits for d or until ctx ends, and reports whether it waited the whole time.
func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
