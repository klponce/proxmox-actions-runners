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

	DefaultMetricsListen = "127.0.0.1:9465"
	DefaultLinkedClone   = true

	// DefaultRunnerGroup is GitHub's default runner group, the only one repository scale sets can use.
	DefaultRunnerGroup = "default"

	// ReservedVMIDs is how many IDs in the VMID range are kept for things other than workers: the current and
	// previous template, a template build VM, and a smoke-test clone.
	ReservedVMIDs = 4

	proxmoxAPIPath = "/api2/json"
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
	if c.Proxmox.LinkedClone == nil {
		linked := DefaultLinkedClone
		c.Proxmox.LinkedClone = &linked
	}
	c.GitHub.ConfigURL = strings.TrimSuffix(c.GitHub.ConfigURL, "/")
	if c.Metrics.Listen == "" {
		c.Metrics.Listen = DefaultMetricsListen
	}

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
