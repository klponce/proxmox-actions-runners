package controller

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

const (
	opRetire = "retire"
	// opCreatePrefix is followed by the scale set name.
	opCreatePrefix = "create:"

	// retireTimeout bounds one retirement. Retirements outlive a canceled controller context so shutdown doesn't
	// leave a VM half-destroyed.
	retireTimeout = 5 * time.Minute
	// agentPollInterval is how often a booting worker's guest agent is pinged.
	agentPollInterval = 2 * time.Second
)

// errRunnerBusy stops a retirement whose runner is in the middle of a job.
var errRunnerBusy = errors.New("runner is running a job")

func (c *Controller) isBusy(vmid int) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.busy[vmid]
	return ok
}

// creatingFor counts the creations in flight for a scale set.
func (c *Controller) creatingFor(scaleSet string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := 0
	for _, op := range c.busy {
		if op == opCreatePrefix+scaleSet {
			n++
		}
	}
	return n
}

// startOp runs fn in the background, holding a parallelism slot, unless vmid already has an operation in flight.
// When fn returns, the VMID is released and the loop is woken to look at the result.
func (c *Controller) startOp(ctx context.Context, vmid int, op string, fn func(context.Context) error) {
	c.mu.Lock()
	if _, busy := c.busy[vmid]; busy {
		c.mu.Unlock()
		return
	}
	c.busy[vmid] = op
	c.mu.Unlock()

	c.ops.Add(1)
	go func() {
		defer c.ops.Done()
		defer func() {
			c.mu.Lock()
			delete(c.busy, vmid)
			c.mu.Unlock()
			c.trigger()
		}()
		select {
		case c.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-c.slots }()
		if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
			level := slog.LevelWarn
			if errors.Is(err, errRunnerBusy) {
				level = slog.LevelInfo
			}
			c.logger.Log(ctx, level, op+" failed", slog.Int("vmid", vmid), slog.String("error", err.Error()))
		}
	}()
}

// startRetire retires a VM in the background. s is the worker's scale set, or nil for a VM without a configured one.
// runnerName is empty for a VM that never got a runner. Unless force is set, a VM whose runner is running a job is
// left alone.
func (c *Controller) startRetire(ctx context.Context, s *scaleSetState, vm proxmox.VM, runnerName string, force bool,
	reason string) {
	c.startOp(ctx, vm.VMID, opRetire, func(ctx context.Context) error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retireTimeout)
		defer cancel()
		c.logger.InfoContext(ctx, "retiring worker", slog.Int("vmid", vm.VMID), slog.String("runnerName", runnerName),
			slog.String("reason", reason))
		err := c.retire(ctx, vm.VMID, vm.Status == "running", runnerName, force)
		if errors.Is(err, errRunnerBusy) && s != nil {
			// The controller missed the job's start, for example across a restart. Count the worker as busy so that
			// scaling down picks another one.
			s.markRunning(runnerName)
		}
		return err
	})
}

// retire unregisters a VM's runner, if it has one, then stops and destroys the VM. Each step tolerates the thing
// already being gone, so a retirement interrupted by a crash is simply repeated.
func (c *Controller) retire(ctx context.Context, vmid int, running bool, runnerName string, force bool) error {
	if runnerName != "" {
		if err := c.removeRunner(ctx, runnerName); err != nil && !force {
			return err
		}
		for _, s := range c.scaleSets {
			s.forget(runnerName)
		}
	}
	if running {
		if err := c.pve.Stop(ctx, vmid); err != nil && !proxmox.IsNotFound(err) {
			// The VM may have powered itself off in the meantime; Destroy tells.
			c.logger.InfoContext(ctx, "stopping worker failed; destroying anyway", slog.Int("vmid", vmid),
				slog.String("error", err.Error()))
		}
	}
	if err := c.pve.Destroy(ctx, vmid); err != nil && !proxmox.IsNotFound(err) {
		return fmt.Errorf("destroy: %w", err)
	}
	c.mu.Lock()
	delete(c.lastRunnerCheck, vmid)
	c.mu.Unlock()
	c.logger.InfoContext(ctx, "worker destroyed", slog.Int("vmid", vmid), slog.String("runnerName", runnerName))
	return nil
}

// removeRunner unregisters a runner by name. It returns errRunnerBusy if the runner is running a job.
func (c *Controller) removeRunner(ctx context.Context, name string) error {
	runner, err := c.gh.RunnerByName(ctx, name)
	if err != nil {
		return fmt.Errorf("find runner: %w", err)
	}
	if runner == nil {
		return nil
	}
	err = c.gh.RemoveRunner(ctx, runner.ID)
	switch {
	case errors.Is(err, github.ErrJobStillRunning):
		return errRunnerBusy
	case err != nil:
		return fmt.Errorf("remove runner: %w", err)
	}
	return nil
}

// startCreate creates a worker in the background.
func (c *Controller) startCreate(ctx context.Context, s *scaleSetState, template proxmox.VM, vmid int) {
	c.startOp(ctx, vmid, opCreatePrefix+s.cfg.Name, func(ctx context.Context) error {
		err := c.create(ctx, s, template, vmid)
		if err != nil && !errors.Is(err, context.Canceled) {
			s.backOff(c.now().Add(c.retryBackoff))
		}
		return err
	})
}

// create builds one worker: clone the template, tag and size it, register its runner, boot it, and hand it the JIT
// config through the guest agent. The final step tags it ready. If any step fails, the half-built worker is
// destroyed on the spot; if the controller dies instead, the next start destroys it because it isn't ready.
func (c *Controller) create(ctx context.Context, s *scaleSetState, template proxmox.VM, vmid int) (err error) {
	created := c.now().Truncate(time.Second)
	name := workerName(vmid, created)
	log := c.logger.With(slog.String("scaleSet", s.cfg.Name), slog.Int("vmid", vmid), slog.String("runnerName", name))
	log.InfoContext(ctx, "creating worker", slog.Int("templateVmid", template.VMID))

	p := c.cfg.Proxmox
	cloneErr := c.pve.Clone(ctx, proxmox.CloneOptions{
		SourceVMID:  template.VMID,
		NewVMID:     vmid,
		Name:        name,
		Pool:        p.Pool,
		Full:        !p.UseLinkedClone(),
		Storage:     p.Storage,
		Description: "Runner for scale set " + s.cfg.Name + ". Managed by proxmox-actions-runners; destroyed after one job.",
	})
	if proxmox.IsVMIDInUse(cloneErr) {
		// Taken by a VM the token can't see. Never try this ID again.
		c.mu.Lock()
		c.foreign[vmid] = true
		c.mu.Unlock()
		return fmt.Errorf("clone: VMID %d is taken by a VM outside the runner pool: %w", vmid, cloneErr)
	}
	if cloneErr != nil {
		// A failed clone task normally removes its VM. If it leaves one, it carries the template's tags and the
		// next pass destroys it as a stray.
		return fmt.Errorf("clone: %w", cloneErr)
	}

	registered := false
	defer func() {
		if err == nil {
			return
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), retireTimeout)
		defer cancel()
		runner := ""
		if registered {
			runner = name
		}
		if cleanupErr := c.retire(cleanupCtx, vmid, true, runner, true); cleanupErr != nil {
			log.WarnContext(cleanupCtx, "cleaning up a failed worker failed; the next pass retries",
				slog.String("error", cleanupErr.Error()))
		}
	}()

	w := s.cfg.Worker
	settings := map[string]string{
		"tags":      proxmox.FormatTags(workerTags(s.cfg.Name, created, false)),
		"cores":     strconv.Itoa(w.Cores),
		"memory":    strconv.Itoa(w.MemoryMiB),
		"net0":      "virtio,bridge=" + p.VNet,
		"ipconfig0": "ip=dhcp",
		"onboot":    "0",
		// The controller reaches workers only through the guest agent.
		"agent": "1",
		// Proxmox's cloud-init upgrades all packages on first boot by default, which would delay every job.
		// Templates are rebuilt to pick up updates instead.
		"ciupgrade": "0",
	}
	if err := c.pve.SetConfig(ctx, vmid, settings); err != nil {
		return fmt.Errorf("configure: %w", err)
	}
	if err := c.pve.GrowDisk(ctx, vmid, rootDisk, w.FreeDiskGiB); err != nil {
		return fmt.Errorf("grow disk: %w", err)
	}

	jit, err := c.generateJIT(ctx, s.id, name)
	if err != nil {
		return err
	}
	registered = true

	if err := c.pve.Start(ctx, vmid); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	if err := c.waitForAgent(ctx, vmid, created.Add(c.bootTimeout)); err != nil {
		return err
	}
	if err := c.pve.AgentWriteFile(ctx, vmid, JITConfigPath, []byte(jit.Encoded())); err != nil {
		return fmt.Errorf("deliver JIT config: %w", err)
	}
	if err := c.pve.SetConfig(ctx, vmid, map[string]string{
		"tags": proxmox.FormatTags(workerTags(s.cfg.Name, created, true)),
	}); err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	log.InfoContext(ctx, "worker ready", slog.Duration("took", c.now().Sub(created)))
	return nil
}

// generateJIT registers the worker's runner. If a runner with the name already exists, which only a retried
// creation of the same VM can cause, it is removed and registered again.
func (c *Controller) generateJIT(ctx context.Context, scaleSetID int, name string) (github.JITConfig, error) {
	jit, err := c.gh.GenerateJITConfig(ctx, scaleSetID, name)
	if errors.Is(err, github.ErrRunnerExists) {
		if err := c.removeRunner(ctx, name); err != nil {
			return github.JITConfig{}, fmt.Errorf("replace existing runner: %w", err)
		}
		jit, err = c.gh.GenerateJITConfig(ctx, scaleSetID, name)
	}
	if err != nil {
		return github.JITConfig{}, fmt.Errorf("register runner: %w", err)
	}
	return jit, nil
}

// waitForAgent pings the worker's guest agent until it answers or deadline passes.
func (c *Controller) waitForAgent(ctx context.Context, vmid int, deadline time.Time) error {
	for {
		err := c.pve.AgentPing(ctx, vmid)
		if err == nil {
			return nil
		}
		if !proxmox.IsAgentNotReady(err) {
			return fmt.Errorf("guest agent: %w", err)
		}
		if !c.now().Before(deadline) {
			return fmt.Errorf("guest agent didn't answer within the boot timeout of %s", c.bootTimeout)
		}
		if !sleep(ctx, c.agentPoll) {
			return ctx.Err()
		}
	}
}
