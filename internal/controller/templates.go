package controller

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

const opPruneTemplate = "prune template"

// pruneTemplates destroys runner templates nothing needs anymore: every template in the pool except the newest,
// once no worker was cloned from it. Workers are linked clones, which depend on their template, and each records
// the template it came from (vmtags.TemplateRefPrefix). It does nothing while a worker is being created, or while
// any worker lacks that record, and Proxmox itself refuses to delete a template that clones still use.
func (c *Controller) pruneTemplates(ctx context.Context, vms []proxmox.VM, newest *proxmox.VM) {
	if newest == nil || c.creatingAny() {
		return
	}
	pool := c.cfg.Proxmox.Pool
	inUse := map[int]bool{}
	for _, vm := range vms {
		if vm.Template || vm.Pool != pool || !vm.HasTag(vmtags.Worker) {
			continue
		}
		ref, ok := vmtags.Int(vm, vmtags.TemplateRefPrefix)
		if !ok {
			return
		}
		inUse[int(ref)] = true
	}
	for _, vm := range vms {
		if !vmtags.IsTemplate(vm, pool) || vm.VMID == newest.VMID || inUse[vm.VMID] || c.isBusy(vm.VMID) {
			continue
		}
		c.startOp(ctx, vm.VMID, opPruneTemplate, func(ctx context.Context) error {
			if err := c.pve.Destroy(ctx, vm.VMID); err != nil && !proxmox.IsNotFound(err) {
				return fmt.Errorf("destroy: %w", err)
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
