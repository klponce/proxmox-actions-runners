package controller

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

// Tags mark what the controller owns and record everything it needs to rebuild its state after a restart
// (AGENTS.md, invariants 3 and 5). Proxmox tags may contain only letters, digits, and _ - + . characters.
const (
	// TagManaged marks every VM the project owns. The controller never touches a VM without it.
	TagManaged = "par-managed"
	// TagWorker marks a worker VM.
	TagWorker = "par-worker"
	// TagReady marks a worker whose runner has its JIT config. A worker without it is still being created, or was
	// left half-created by a crash.
	TagReady = "par-ready"
	// TagTemplate marks a runner template.
	TagTemplate = "par-template"
	// TagBuild marks a template build VM, which the template builder owns.
	TagBuild = "par-build"

	// tagScaleSetPrefix is followed by the worker's scale set name.
	tagScaleSetPrefix = "par-ss-"
	// tagCreatedPrefix is followed by the worker's creation time in Unix seconds.
	tagCreatedPrefix = "par-created-"
	// tagTemplateVersionPrefix is followed by a template's version: its build time in Unix seconds.
	tagTemplateVersionPrefix = "par-tv-"
)

// JITConfigPath is where the controller writes a worker's JIT config through the guest agent. The template's
// runner service waits for this file, runs one job with it, deletes it, and powers the VM off.
const JITConfigPath = "/run/par-runner/jitconfig"

// workerName returns the VM and runner name for a worker. It includes the creation time so a reused VMID never
// reuses a runner name that GitHub might still know.
func workerName(vmid int, created time.Time) string {
	return fmt.Sprintf("par-%d-%s", vmid, strconv.FormatInt(created.Unix(), 36))
}

// workerTags returns a worker's tags. ready adds TagReady.
func workerTags(scaleSet string, created time.Time, ready bool) []string {
	tags := []string{TagManaged, TagWorker, tagScaleSetPrefix + scaleSet, tagCreatedPrefix +
		strconv.FormatInt(created.Unix(), 10)}
	if ready {
		tags = append(tags, TagReady)
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

// parseWorker reads a worker's identity from its tags. It fails for a VM that has TagWorker but lacks a valid scale
// set or creation time, which only a crash or a person editing tags can cause.
func parseWorker(vm proxmox.VM) (worker, error) {
	w := worker{vm: vm, ready: vm.HasTag(TagReady)}
	for _, tag := range vm.Tags {
		switch {
		case strings.HasPrefix(tag, tagScaleSetPrefix):
			w.scaleSet = strings.TrimPrefix(tag, tagScaleSetPrefix)
		case strings.HasPrefix(tag, tagCreatedPrefix):
			sec, err := strconv.ParseInt(strings.TrimPrefix(tag, tagCreatedPrefix), 10, 64)
			if err != nil {
				return w, fmt.Errorf("VM %d has an invalid creation tag %q", vm.VMID, tag)
			}
			w.created = time.Unix(sec, 0)
		}
	}
	if w.scaleSet == "" || w.created.IsZero() {
		return w, fmt.Errorf("VM %d is tagged %s but has no scale set or creation time", vm.VMID, TagWorker)
	}
	w.name = workerName(vm.VMID, w.created)
	return w, nil
}

// templateVersion returns a template's version from its tags, or false if it has none.
func templateVersion(vm proxmox.VM) (int64, bool) {
	for _, tag := range vm.Tags {
		if v, ok := strings.CutPrefix(tag, tagTemplateVersionPrefix); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			return n, err == nil
		}
	}
	return 0, false
}
