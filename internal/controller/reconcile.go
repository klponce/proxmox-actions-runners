package controller

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// maxRunnerChecksPerPass bounds the GitHub lookups one pass makes, so a large pool doesn't slow the loop down.
const maxRunnerChecksPerPass = 10

// view is what one pass sees in Proxmox.
type view struct {
	// template is the newest runner template, or nil if there is none.
	template *proxmox.VM
	// workers holds the workers of each scale set, by scale set name, including scale sets no longer configured.
	workers map[string][]worker
	// strays are managed VMs in the worker part of the VMID range that aren't workers: clones left half-created by
	// a crash, which still carry the template's tags, and workers with broken tags.
	strays []proxmox.VM
	// used holds every VMID the token can see.
	used map[int]bool
}

// observe sorts the VMs Proxmox lists into a view. It considers only VMs in the configured pool that carry
// vmtags.Managed, and never counts a VM outside the worker part of the VMID range as a worker or stray (AGENTS.md,
// invariant 5).
func (c *Controller) observe(vms []proxmox.VM) view {
	v := view{workers: map[string][]worker{}, used: map[int]bool{}}
	if t, ok := vmtags.NewestTemplate(vms, c.cfg.Proxmox.Pool); ok {
		v.template = &t
	}
	for _, vm := range vms {
		v.used[vm.VMID] = true
		if vm.Template || vm.Pool != c.cfg.Proxmox.Pool || !vm.HasTag(vmtags.Managed) {
			continue
		}
		// Only the worker part of the range holds workers and their half-created leftovers. The reserved part holds
		// templates and the installer's par-build clones, and a newly imported template shows template: 0 for about
		// 10s while it already has the template's tags, the same as a half-created clone; looking only at worker IDs
		// keeps it from being destroyed as a stray.
		if !c.cfg.Proxmox.VMIDRange.Workers().Contains(vm.VMID) {
			continue
		}
		if !vm.HasTag(vmtags.Worker) {
			v.strays = append(v.strays, vm)
			continue
		}
		w, err := parseWorker(vm)
		if err != nil {
			c.logger.Warn("destroying a worker with broken tags", slog.Int("vmid", vm.VMID),
				slog.String("error", err.Error()))
			v.strays = append(v.strays, vm)
			continue
		}
		v.workers[w.scaleSet] = append(v.workers[w.scaleSet], w)
	}
	return v
}

// reconcile runs one pass: it retires workers that are done or broken, removes leftovers, and creates or retires
// workers to match each scale set's target. Slow VM operations run in the background; the next pass sees their
// results.
func (c *Controller) reconcile(ctx context.Context) error {
	vms, err := c.pve.ListVMs(ctx)
	if err != nil {
		return fmt.Errorf("list VMs: %w", err)
	}
	now := c.now()
	v := c.observe(vms)
	c.pruneTemplates(ctx, vms, v.template)
	c.checkRunnerFreshness(ctx, v.template)

	for _, vm := range v.strays {
		c.startRetire(ctx, nil, vm, "", true, "left half-created")
	}

	checks := 0
	for name, workers := range v.workers {
		s := c.scaleSets[name] // nil for a scale set no longer configured
		for _, w := range workers {
			if c.isBusy(w.vm.VMID) {
				continue
			}
			if reason, force := c.retireReason(ctx, s, w, now, &checks); reason != "" {
				c.startRetire(ctx, s, w.vm, w.name, force, reason)
			}
		}
	}

	for _, s := range c.scaleSets {
		c.scale(ctx, s, v, now)
	}
	c.countWorkers(v)
	return nil
}

// retireReason says whether a worker should be retired, why, and whether to do it even if its runner is in the
// middle of a job. It returns "" to keep the worker. s is nil for a scale set that is no longer configured.
func (c *Controller) retireReason(ctx context.Context, s *scaleSetState, w worker, now time.Time,
	checks *int) (reason string, force bool) {
	// A removed scale set's lifetime is gone with its config; the default still bounds its workers (invariant 4).
	maxLifetime := config.DefaultMaxLifetime
	if s != nil {
		maxLifetime = s.cfg.MaxLifetime
	}
	age := now.Sub(w.created)
	switch {
	case age > maxLifetime:
		return fmt.Sprintf("older than maxLifetime %s", maxLifetime), true
	case w.vm.Status == "stopped" && w.ready:
		// A powered-off VM can't be running a job, whatever GitHub still thinks.
		return "powered off after its job", true
	case s == nil || !w.ready:
		// These workers go once their runner isn't in a job. The retirement asks GitHub, so while a job runs it is
		// retried only as often as the runner check.
		if !c.runnerCheckDue(w.vm.VMID, now) {
			return "", false
		}
		if s == nil {
			return "scale set removed from config", false
		}
		// Only a crash leaves a worker unready without an operation in flight, and a half-created worker is
		// destroyed rather than repaired.
		return "left half-created", false
	case w.vm.Status != "running":
		// Proxmox reports status from pvestatd, up to about 10s late: a VM it hasn't sampled yet is "unknown".
		// Only an explicit "stopped", handled above, says a worker is done; anything else is looked at again next
		// pass.
		return "", false
	}

	// A ready, running worker whose runner is gone from GitHub has nothing left to do: its runner finished a job
	// but the VM didn't power off, or someone removed the runner.
	if age < c.runnerCheckAfter || *checks >= maxRunnerChecksPerPass || !c.runnerCheckDue(w.vm.VMID, now) {
		return "", false
	}
	*checks++
	runner, err := c.gh.RunnerByName(ctx, w.name)
	if err != nil {
		c.logger.WarnContext(ctx, "looking up a worker's runner failed", slog.Int("vmid", w.vm.VMID),
			slog.String("runnerName", w.name), slog.String("error", err.Error()))
		return "", false
	}
	if runner == nil {
		return "runner no longer registered in GitHub", false
	}
	return "", false
}

func (c *Controller) runnerCheckDue(vmid int, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if last, ok := c.lastRunnerCheck[vmid]; ok && now.Sub(last) < c.runnerCheckEvery {
		return false
	}
	c.lastRunnerCheck[vmid] = now
	return true
}

// scale creates or retires workers so the scale set has as many as GitHub wants.
func (c *Controller) scale(ctx context.Context, s *scaleSetState, v view, now time.Time) {
	target, known := s.target()
	if !known || s.id == 0 {
		return
	}

	// Count workers that are alive or on their way: creations in flight, and listed workers that aren't being
	// retired, haven't stopped, and aren't about to be retired as half-created.
	creating := c.creatingFor(s.cfg.Name)
	active := creating
	var idle []worker
	for _, w := range v.workers[s.cfg.Name] {
		if c.isBusy(w.vm.VMID) || w.vm.Status == "stopped" || !w.ready {
			continue
		}
		active++
		if !s.hasJob(w.name) {
			idle = append(idle, w)
		}
	}

	switch {
	case active < target:
		if v.template == nil {
			c.logger.WarnContext(ctx, "no runner template; can't create workers", slog.String("scaleSet", s.cfg.Name),
				slog.String("pool", c.cfg.Proxmox.Pool))
			return
		}
		if !s.canCreate(now) {
			return
		}
		for range target - active {
			vmid, ok := c.allocateVMID(v.used)
			if !ok {
				c.logger.WarnContext(ctx, "no free VMID in the configured range", slog.String("scaleSet", s.cfg.Name))
				return
			}
			c.startCreate(ctx, s, *v.template, vmid)
		}

	case active > target && creating == 0:
		// Retire the oldest idle workers first; they are closest to maxLifetime anyway. A worker whose runner
		// picked up a job in the meantime refuses to go (RemoveRunner fails), stays, and counts as busy from then on.
		sort.Slice(idle, func(i, j int) bool { return idle[i].created.Before(idle[j].created) })
		for _, w := range idle[:min(active-target, len(idle))] {
			c.startRetire(ctx, s, w.vm, w.name, false, "more workers than GitHub wants")
		}
	}
}

// allocateVMID returns the lowest free VMID in the worker part of the configured range and reserves it.
func (c *Controller) allocateVMID(used map[int]bool) (int, bool) {
	r := c.cfg.Proxmox.VMIDRange.Workers()
	c.mu.Lock()
	defer c.mu.Unlock()
	for id := r.Start; id <= r.End; id++ {
		if used[id] || c.foreign[id] {
			continue
		}
		if _, busy := c.busy[id]; busy {
			continue
		}
		used[id] = true
		return id, true
	}
	return 0, false
}
