// Package vmtags defines the Proxmox tags that mark what this project owns. The controller and the installer rebuild
// their view of the world from them (AGENTS.md, invariants 3 and 5), so they are defined once, here. Proxmox tags
// may contain only letters, digits, and _ - + . characters.
package vmtags

import (
	"strconv"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

// Tags that mark a VM's role.
const (
	// Managed marks every VM the project owns. Nothing without it is ever touched.
	Managed = "par-managed"
	// Worker marks a worker VM.
	Worker = "par-worker"
	// Ready marks a worker whose runner has its JIT config. A worker without it is still being created, or was
	// left half-created by a crash.
	Ready = "par-ready"
	// Template marks a runner template that workers are cloned from. The installer imports templates with it.
	Template = "par-template"
	// Build marks a VM the installer creates and destroys itself, such as its smoke-test clone. It lives in the
	// reserved VMIDs, and the controller leaves it alone.
	Build = "par-build"
)

// Tag prefixes, each followed by a value.
const (
	// ScaleSetPrefix is followed by a worker's scale set name.
	ScaleSetPrefix = "par-ss-"
	// CreatedPrefix is followed by a worker's creation time in Unix seconds.
	CreatedPrefix = "par-created-"
	// TemplateRefPrefix is followed by the VMID of the template a worker was cloned from. Linked clones depend on
	// their template, so a template is deleted only when no VM carries its reference.
	TemplateRefPrefix = "par-tpl-"
	// TemplateVersionPrefix is followed by a template's version: its import time in Unix seconds. The newest
	// template wins.
	TemplateVersionPrefix = "par-tv-"
	// RunnerVersionPrefix is followed by the actions/runner version a template contains, such as 2.337.0.
	RunnerVersionPrefix = "par-rv-"
)

// Int returns the number after prefix in the first tag that has it.
func Int(vm proxmox.VM, prefix string) (int64, bool) {
	s, ok := String(vm, prefix)
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}

// String returns the text after prefix in the first tag that has it.
func String(vm proxmox.VM, prefix string) (string, bool) {
	for _, tag := range vm.Tags {
		if v, ok := strings.CutPrefix(tag, prefix); ok && v != "" {
			return v, true
		}
	}
	return "", false
}

// IsTemplate reports whether vm is a runner template this project owns in pool: a Proxmox template tagged Managed
// and Template.
func IsTemplate(vm proxmox.VM, pool string) bool {
	return vm.Template && vm.Pool == pool && vm.HasTag(Managed) && vm.HasTag(Template)
}

// NewestTemplate returns the runner template in pool with the highest version (TemplateVersionPrefix), breaking
// ties by the higher VMID. Templates without a version are ignored. Workers are cloned from it.
func NewestTemplate(vms []proxmox.VM, pool string) (proxmox.VM, bool) {
	var newest proxmox.VM
	best, found := int64(-1), false
	for _, vm := range vms {
		if !IsTemplate(vm, pool) {
			continue
		}
		version, ok := Int(vm, TemplateVersionPrefix)
		if ok && (version > best || (version == best && vm.VMID > newest.VMID)) {
			newest, best, found = vm, version, true
		}
	}
	return newest, found
}
