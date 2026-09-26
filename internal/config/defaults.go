package config

import (
	"net/url"
	"strings"
	"time"
)

// Defaults. This is the only place they are defined (AGENTS.md, invariant 6).
const (
	// DefaultCores, DefaultMemoryMiB, and DefaultFreeDiskGiB match GitHub's standard ubuntu-latest runner for
	// private repositories: 2 CPUs, 8 GB RAM, and 14 GB of free SSD space.
	DefaultCores       = 2
	DefaultMemoryMiB   = 8 * 1024
	DefaultFreeDiskGiB = 14

	// DefaultMaxLifetime matches the job time limit on GitHub-hosted runners.
	DefaultMaxLifetime = 6 * time.Hour

	// DefaultZone is the SDN zone the installer creates for the worker VNet.
	DefaultZone = "parzone"

	DefaultLinkedClone = true

	// DefaultRunnerGroup is GitHub's default runner group, the only one repository scale sets can use.
	DefaultRunnerGroup = "default"

	// ReservedVMIDs is how many IDs at the end of the VMID range are kept for things other than workers: runner
	// templates, while old ones wait for their last linked clones, and the installer's smoke-test clone.
	ReservedVMIDs = 4

	proxmoxAPIPath = "/api2/json"
)

// Install defaults: what `parcon install` sets up when the user doesn't choose otherwise. They live here with the
// controller's defaults so that every default has one home.
const (
	// DefaultScaleSetName is the scale set's name and its runs-on label.
	DefaultScaleSetName = "proxmox-ubuntu-26.04"
	// DefaultMinRunners and DefaultMaxRunners size the default install for one job at a time.
	DefaultMinRunners = 0
	DefaultMaxRunners = 1
	// DefaultStorage and DefaultBridge are a fresh Proxmox VE install's VM storage and LAN bridge.
	DefaultStorage = "local-lvm"
	DefaultBridge  = "vmbr0"
	// DefaultWorkerSubnet is the worker network behind the gateway VM: a /22 out of the way of common LANs.
	DefaultWorkerSubnet = "10.251.0.0/22"
	// DefaultVMIDStart and DefaultVMIDEnd bound the IDs of workers and templates.
	DefaultVMIDStart = 10000
	DefaultVMIDEnd   = 10099

	// DefaultPool is the resource pool that holds templates and workers, and DefaultVNet the worker network's VNet,
	// both created by the installer.
	DefaultPool = "par-runners"
	DefaultVNet = "parnet"
	// DefaultTokenID is the controller's API token, which the installer creates.
	DefaultTokenID = "par@pve!controller"
)

// DefaultWorker returns the GitHub-matching worker hardware.
func DefaultWorker() Worker {
	return Worker{
		Cores:       DefaultCores,
		MemoryMiB:   DefaultMemoryMiB,
		FreeDiskGiB: DefaultFreeDiskGiB,
	}
}

// withDefaults returns w with each unset field taken from base.
func (w Worker) withDefaults(base Worker) Worker {
	if w.Cores == 0 {
		w.Cores = base.Cores
	}
	if w.MemoryMiB == 0 {
		w.MemoryMiB = base.MemoryMiB
	}
	if w.FreeDiskGiB == 0 {
		w.FreeDiskGiB = base.FreeDiskGiB
	}
	return w
}

func (c *Config) applyDefaults() {
	// Only complete a URL that is otherwise valid, so error messages show what the user wrote.
	if u, err := url.Parse(c.Proxmox.URL); err == nil && u.Scheme == "https" && u.Host != "" &&
		(u.Path == "" || u.Path == "/") {
		u.Path = proxmoxAPIPath
		c.Proxmox.URL = u.String()
	}
	if c.Proxmox.Zone == "" {
		c.Proxmox.Zone = DefaultZone
	}
	if c.Proxmox.LinkedClone == nil {
		linked := DefaultLinkedClone
		c.Proxmox.LinkedClone = &linked
	}
	c.GitHub.ConfigURL = strings.TrimSuffix(c.GitHub.ConfigURL, "/")

	c.Worker = c.Worker.withDefaults(DefaultWorker())
	for i := range c.ScaleSets {
		s := &c.ScaleSets[i]
		if len(s.Labels) == 0 && s.Name != "" {
			s.Labels = []string{s.Name}
		}
		if s.RunnerGroup == "" {
			s.RunnerGroup = DefaultRunnerGroup
		}
		if s.MaxLifetime == 0 {
			s.MaxLifetime = DefaultMaxLifetime
		}
		s.Worker = s.Worker.withDefaults(c.Worker)
	}
}
