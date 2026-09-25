package controller

import (
	"context"
	"log/slog"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

const opPruneTemplate = "prune template"

// pruneTemplates destroys runner templates nothing needs anymore: every template in the pool except the newest,
// once no worker was cloned from it. Workers are linked clones, which depend on their template, and each records
// the template it came from (vmtags.TemplateRefPrefix). Proxmox refuses to delete a template that clones still use,
// and would refuse again on every pass, so pruning waits while a worker is being created, and while any managed VM
// in the pool lacks that record: a half-created clone, the installer's smoke-test clone, a new template whose
// template flag Proxmox reports late, or a worker from before the record existed.
func (c *Controller) pruneTemplates(ctx context.Context, vms []proxmox.VM, newest *proxmox.VM) {
	if newest == nil || c.creatingAny() {
		return
	}
	pool := c.cfg.Proxmox.Pool
	inUse := map[int]bool{}
	for _, vm := range vms {
		if vm.Template || vm.Pool != pool || !vm.HasTag(vmtags.Managed) {
			continue
		}
		ref, ok := vmtags.Int(vm, vmtags.TemplateRefPrefix)
		if !ok {
			return
		}
		inUse[int(ref)] = true
	}
	for _, vm := range vms {
		if !vmtags.IsTemplate(vm, pool) || vm.VMID == newest.VMID || inUse[vm.VMID] {
			continue
		}
		c.startOp(ctx, vm.VMID, opPruneTemplate, func(ctx context.Context) error {
			if err := c.destroy(ctx, vm.VMID); err != nil {
				return err
			}
			c.logger.InfoContext(ctx, "removed an old runner template", slog.Int("vmid", vm.VMID),
				slog.Int("newestVmid", newest.VMID))
			return nil
		})
	}
}

// creatingAny reports whether any worker creation is in flight.
func (c *Controller) creatingAny() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, op := range c.busy {
		if strings.HasPrefix(op, opCreatePrefix) {
			return true
		}
	}
	return false
}
