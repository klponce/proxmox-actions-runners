//go:build integration

package controller

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

// TestIntegrationWorkerLifecycle runs the real controller against a real Proxmox VE node, with the in-memory GitHub
// fake from fakes_test.go standing in for GitHub. It uses the same PAR_PVE_* variables as the proxmox package's
// integration tests (see internal/proxmox/integration_test.go), plus:
//
//	PAR_PVE_TEST_VMID  the test uses the 10 VMIDs after it; the proxmox package's test uses this one itself
//	PAR_PVE_VNET       the worker VNet; defaults to parnet
//
// The template in PAR_PVE_POOL must be tagged par-managed, par-template, and par-tv-<version>, have the QEMU guest
// agent enabled, and create /run/par-runner at boot, as the real runner template does. Run it with:
//
//	go test -tags integration ./internal/controller/
func TestIntegrationWorkerLifecycle(t *testing.T) {
	pve, cfg := integrationSetup(t)
	gh := newFakeGitHub()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	newController := func() *Controller {
		c, err := New(Options{Config: cfg, Proxmox: pve, GitHub: gh, Owner: "integration-test"})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		for _, s := range c.scaleSets {
			s.id = testScaleSetID
		}
		return c
	}
	want := func(c *Controller, jobs int) {
		t.Helper()
		if _, err := c.scaleSets[integrationScaleSet].DesiredRunners(ctx, jobs); err != nil {
			t.Fatal(err)
		}
	}
	pass := func(c *Controller) {
		t.Helper()
		if err := c.reconcile(ctx); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		c.ops.Wait()
	}
	workers := func() []proxmox.VM {
		t.Helper()
		vms, err := pve.ListVMs(ctx)
		if err != nil {
			t.Fatalf("ListVMs: %v", err)
		}
		var out []proxmox.VM
		for _, vm := range vms {
			if vm.HasTag(TagWorker) && cfg.Proxmox.VMIDRange.Contains(vm.VMID) {
				out = append(out, vm)
			}
		}
		return out
	}

	// 1. One job assigned: the controller creates one worker and hands it a JIT config.
	c := newController()
	want(c, 1)
	start := time.Now()
	pass(c)
	ws := workers()
	if len(ws) != 1 {
		t.Fatalf("workers after asking for one: %+v", ws)
	}
	w := ws[0]
	parsed, err := parseWorker(w)
	if err != nil {
		t.Fatalf("parseWorker: %v", err)
	}
	t.Logf("worker %d (%s) ready in %s", w.VMID, parsed.name, time.Since(start).Round(time.Second))
	if !parsed.ready || w.Status != "running" || w.Pool != cfg.Proxmox.Pool || w.Name != parsed.name ||
		parsed.scaleSet != integrationScaleSet {
		t.Fatalf("worker = %+v, parsed %+v", w, parsed)
	}
	res, err := pve.AgentExec(ctx, w.VMID, []string{"cat", JITConfigPath}, nil)
	if err != nil {
		t.Fatalf("reading the JIT config in the guest: %v", err)
	}
	if res.ExitCode != 0 || string(res.Stdout) != "jit-for-"+parsed.name {
		t.Fatalf("JIT config in the guest: exit %d, %q, stderr %q", res.ExitCode, res.Stdout, res.Stderr)
	}

	// 2. A restarted controller adopts the worker instead of creating another.
	c = newController()
	want(c, 1)
	pass(c)
	if ws := workers(); len(ws) != 1 || ws[0].VMID != w.VMID {
		t.Fatalf("workers after a restart: %+v", ws)
	}

	// 3. The runner's job ends and the VM powers off: the worker is retired and its runner removed.
	if err := pve.Stop(ctx, w.VMID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	want(c, 0)
	pass(c)
	if ws := workers(); len(ws) != 0 {
		t.Fatalf("workers after the job: %+v", ws)
	}
	if gh.hasRunner(parsed.name) {
		t.Errorf("runner %s is still registered", parsed.name)
	}

	// 4. Scaling down retires an idle, running worker.
	want(c, 1)
	pass(c)
	if ws := workers(); len(ws) != 1 {
		t.Fatalf("workers after asking for one again: %+v", ws)
	}
	want(c, 0)
	pass(c)
	if ws := workers(); len(ws) != 0 {
		t.Fatalf("workers after scaling to zero: %+v", ws)
	}
}

const integrationScaleSet = "par-integration"

// integrationSetup returns a Proxmox client and controller config for the node in the PAR_PVE_* variables, and
// destroys any workers the test leaves behind.
func integrationSetup(t *testing.T) (*proxmox.Client, *config.Config) {
	t.Helper()
	env := func(name, def string) string {
		if v := os.Getenv(name); v != "" {
			return v
		}
		if def == "" {
			t.Skipf("%s is not set", name)
		}
		return def
	}
	first, err := strconv.Atoi(env("PAR_PVE_TEST_VMID", ""))
	if err != nil {
		t.Fatalf("PAR_PVE_TEST_VMID: %v", err)
	}
	pve, err := proxmox.New(proxmox.Options{
		URL:            env("PAR_PVE_URL", ""),
		TokenID:        env("PAR_PVE_TOKEN_ID", ""),
		TokenSecret:    env("PAR_PVE_TOKEN_SECRET", ""),
		TLSFingerprint: os.Getenv("PAR_PVE_FINGERPRINT"),
		Node:           env("PAR_PVE_NODE", ""),
		UserAgent:      "parcon-integration-test",
	})
	if err != nil {
		t.Fatalf("proxmox.New: %v", err)
	}

	linked := true
	cfg := &config.Config{
		Proxmox: config.Proxmox{
			Node:    env("PAR_PVE_NODE", ""),
			Pool:    env("PAR_PVE_POOL", ""),
			Storage: env("PAR_PVE_STORAGE", ""),
			VNet:    env("PAR_PVE_VNET", "parnet"),
			// Start above PAR_PVE_TEST_VMID, which the proxmox package's lifecycle test may be using right now:
			// go test runs packages in parallel.
			VMIDRange:   config.VMIDRange{Start: first + 1, End: first + 10},
			LinkedClone: &linked,
		},
		ScaleSets: []config.ScaleSet{{
			Name: integrationScaleSet, Labels: []string{integrationScaleSet}, MaxRunners: 1,
			MaxLifetime: time.Hour, RunnerGroup: config.DefaultRunnerGroup,
			Worker: config.Worker{Cores: 2, MemoryMiB: 2048, FreeDiskGiB: 1},
		}},
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		vms, err := pve.ListVMs(ctx)
		if err != nil {
			t.Errorf("cleanup: ListVMs: %v", err)
			return
		}
		for _, vm := range vms {
			if !cfg.Proxmox.VMIDRange.Contains(vm.VMID) || !vm.HasTag(TagManaged) || vm.Template {
				continue
			}
			t.Logf("cleanup: destroying leftover VM %d (%s, tags %s)", vm.VMID, vm.Name, strings.Join(vm.Tags, ";"))
			if vm.Status == "running" {
				_ = pve.Stop(ctx, vm.VMID)
			}
			if err := pve.Destroy(ctx, vm.VMID); err != nil {
				t.Errorf("cleanup: destroy VM %d: %v; remove it by hand", vm.VMID, err)
			}
		}
	})
	return pve, cfg
}
