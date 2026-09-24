package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// JITConfigPath is where the controller writes a worker's JIT config through the guest agent. The template's
// runner service runs one job with it, deletes it, and powers the VM off.
const JITConfigPath = "/run/par-runner/jitconfig"

// JITReadyPath is the empty file the controller writes once JITConfigPath is complete. The template's runner service
// waits for it rather than for the config itself, because the guest agent creates a file before writing its content.
const JITReadyPath = "/run/par-runner/ready"

// workerName returns the VM and runner name for a worker. It includes the creation time so a reused VMID never
// reuses a runner name that GitHub might still know.
func workerName(vmid int, created time.Time) string {
	return fmt.Sprintf("par-%d-%s", vmid, strconv.FormatInt(created.Unix(), 36))
}

// workerTags returns a worker's tags: its scale set, creation time, and the template it was cloned from. ready adds
// vmtags.Ready.
func workerTags(scaleSet string, created time.Time, templateVMID int, ready bool) []string {
	tags := []string{
		vmtags.Managed,
		vmtags.Worker,
		vmtags.ScaleSetPrefix + scaleSet,
		vmtags.CreatedPrefix + strconv.FormatInt(created.Unix(), 10),
		vmtags.TemplateRefPrefix + strconv.Itoa(templateVMID),
	}
	if ready {
		tags = append(tags, vmtags.Ready)
	}
	return tags
}

// worker is what the tags of a worker VM say about it.
type worker struct {
	vm       proxmox.VM
	name     string
	scaleSet string
	created  time.Time
	ready    bool
}

// parseWorker reads a worker's identity from its tags. It fails for a VM that has vmtags.Worker but lacks a valid
// scale set or creation time, which only a crash or a person editing tags can cause.
func parseWorker(vm proxmox.VM) (worker, error) {
	w := worker{vm: vm, ready: vm.HasTag(vmtags.Ready)}
	for _, tag := range vm.Tags {
		switch {
		case strings.HasPrefix(tag, vmtags.ScaleSetPrefix):
			w.scaleSet = strings.TrimPrefix(tag, vmtags.ScaleSetPrefix)
		case strings.HasPrefix(tag, vmtags.CreatedPrefix):
			sec, err := strconv.ParseInt(strings.TrimPrefix(tag, vmtags.CreatedPrefix), 10, 64)
			if err != nil {
				return w, fmt.Errorf("VM %d has an invalid creation tag %q", vm.VMID, tag)
			}
			w.created = time.Unix(sec, 0)
		}
	}
	if w.scaleSet == "" || w.created.IsZero() {
		return w, fmt.Errorf("VM %d is tagged %s but has no scale set or creation time", vm.VMID, vmtags.Worker)
	}
	w.name = workerName(vm.VMID, w.created)
	return w, nil
}
