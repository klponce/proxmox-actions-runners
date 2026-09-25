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
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// These tests run the real controller against a real Proxmox VE node, with the in-memory GitHub fake from
// fakes_test.go standing in for GitHub. test/integration/run.sh sets them up. They use the same PAR_PVE_* variables
// as the proxmox package's integration tests (see internal/proxmox/integration_test.go), plus:
//
//	PAR_PVE_TEST_VMID             the tests use the 10 VMIDs after it; the proxmox package's test uses this one
//	PAR_PVE_VNET                  the worker VNet; defaults to parnet
//	PAR_PVE_IMAGE_POOL            optional: a pool holding the real runner image as its only template
//	PAR_PVE_IMAGE_TEMPLATE_VMID   that template's VMID
//
// The template in PAR_PVE_POOL must be tagged par-managed, par-template, and par-tv-<version>, have the QEMU guest
// agent enabled, and create /run/par-runner at boot, as the real runner template does. Run them with:
//
//	go test -tags integration ./internal/controller/

// TestIntegrationWorkerLifecycle creates a worker, checks what it received, restarts the controller, and retires the
// worker when it powers off and on scale-down.
func TestIntegrationWorkerLifecycle(t *testing.T) {
	h := newLive(t, "PAR_PVE_POOL")
	templateVMID := h.envInt("PAR_PVE_TEMPLATE_VMID")

	// 1. One job assigned: the controller creates one worker and hands it a JIT config.
	c := h.controller()
	h.want(c, 1)
	start := time.Now()
	h.pass(c)
	w, parsed := h.onlyWorker("after asking for one")
	t.Logf("worker %d (%s) ready in %s", w.VMID, parsed.name, time.Since(start).Round(time.Second))
	if !parsed.ready || w.Status != "running" || w.Pool != h.cfg.Proxmox.Pool || w.Name != parsed.name ||
		parsed.scaleSet != integrationScaleSet {
		t.Fatalf("worker = %+v, parsed %+v", w, parsed)
	}
	if ref, ok := vmtags.Int(w, vmtags.TemplateRefPrefix); !ok || int(ref) != templateVMID {
		t.Errorf("worker tags %v don't record template %d", w.Tags, templateVMID)
	}
	// The test template has no runner service, so both files stay where the controller put them: the config, and
	// the empty file that says it's complete.
	h.guestOutput(w.VMID, "jit-for-"+parsed.name, "cat", JITConfigPath)
	h.guestOutput(w.VMID, "0", "stat", "-c", "%s", JITReadyPath)

	// 2. A restarted controller adopts the worker instead of creating another.
	c = h.controller()
	h.want(c, 1)
	h.pass(c)
	if again, _ := h.onlyWorker("after a restart"); again.VMID != w.VMID {
		t.Fatalf("worker after a restart is %d, want %d", again.VMID, w.VMID)
	}

	// 3. The runner's job ends and the VM powers off: the worker is retired and its runner removed.
	if err := h.pve.Stop(h.ctx, w.VMID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	h.want(c, 0)
	h.pass(c)
	h.noWorkers("after the job")
	if h.gh.hasRunner(parsed.name) {
		t.Errorf("runner %s is still registered", parsed.name)
	}

	// 4. Scaling down retires an idle, running worker.
	h.want(c, 1)
	h.pass(c)
	h.onlyWorker("after asking for one again")
	h.want(c, 0)
	h.pass(c)
	h.noWorkers("after scaling to zero")
}

// TestIntegrationTemplatePruning makes an older template next to the test template and checks that a pass destroys
// the older one, which no worker uses, and keeps the newest.
func TestIntegrationTemplatePruning(t *testing.T) {
	h := newLive(t, "PAR_PVE_POOL")
	newest := h.envInt("PAR_PVE_TEMPLATE_VMID")
	// Templates live in the reserved VMIDs at the end of the range (AGENTS.md, template guidelines).
	old := h.cfg.Proxmox.VMIDRange.Reserved().End
	t.Cleanup(func() { h.destroyIfPresent(old) })

	if err := h.pve.Clone(h.ctx, proxmox.CloneOptions{SourceVMID: newest, NewVMID: old, Name: "par-it-old-template",
		Pool: h.cfg.Proxmox.Pool}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	tags := []string{vmtags.Managed, vmtags.Template, vmtags.TemplateVersionPrefix + "0"}
	if err := h.pve.SetConfig(h.ctx, old, map[string]string{"tags": proxmox.FormatTags(tags)}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := h.pve.ConvertToTemplate(h.ctx, old); err != nil {
		t.Fatalf("ConvertToTemplate: %v", err)
	}
	// Proxmox reports the template flag late, and the controller only prunes what it reports as a template.
	h.waitFor("Proxmox to report the older template", time.Minute, func() bool {
		vm, ok := h.vm(old)
		return ok && vm.Template
	})

	c := h.controller()
	h.pass(c)
	if _, ok := h.vm(old); ok {
		t.Errorf("older template %d wasn't pruned", old)
	}
	if vm, ok := h.vm(newest); !ok || !vm.Template {
		t.Errorf("newest template %d is gone: %+v", newest, vm)
	}

	// Destroying it again, as a repeated retirement or prune would, succeeds: Proxmox answers this token's request
	// for a VM that no longer exists with 403, which the controller checks against the VM list.
	if err := c.destroy(h.ctx, old); err != nil {
		t.Errorf("destroying the already-pruned template %d again: %v", old, err)
	}
}

// TestIntegrationRunnerImage runs a worker from the real runner image (images/runner) through its one-job flow. With
// the GitHub fake, the runner gets a JIT config GitHub would reject, so it exits at once, which is enough to check
// that the image runs it: the image powers the VM off when the runner exits, and the controller retires the worker.
func TestIntegrationRunnerImage(t *testing.T) {
	if os.Getenv("PAR_PVE_IMAGE_POOL") == "" {
		t.Skip("PAR_PVE_IMAGE_POOL is not set; test/integration/run.sh sets it when given PAR_IT_RUNNER_IMAGE")
	}
	h := newLive(t, "PAR_PVE_IMAGE_POOL")
	templateVMID := h.envInt("PAR_PVE_IMAGE_TEMPLATE_VMID")

	c := h.controller()
	h.want(c, 1)
	start := time.Now()
	h.pass(c)
	w, parsed := h.onlyWorker("after asking for one")
	t.Logf("worker %d from the runner image ready in %s", w.VMID, time.Since(start).Round(time.Second))
	if !parsed.ready {
		t.Fatalf("worker %d isn't ready: %v", w.VMID, w.Tags)
	}
	if ref, _ := vmtags.Int(w, vmtags.TemplateRefPrefix); int(ref) != templateVMID {
		t.Errorf("worker tags %v don't record the runner image template %d", w.Tags, templateVMID)
	}

	// par-runner.path starts the runner once the ready file exists; the runner exits, and the image powers off.
	h.waitFor("the worker to power itself off after its runner exits", 5*time.Minute, func() bool {
		vm, ok := h.vm(w.VMID)
		return ok && vm.Status == "stopped"
	})
	t.Logf("worker %d powered itself off %s after it was ready", w.VMID, time.Since(start).Round(time.Second))

	h.want(c, 0)
	h.pass(c)
	h.noWorkers("after the runner exited")
	if h.gh.hasRunner(parsed.name) {
		t.Errorf("runner %s is still registered", parsed.name)
	}
}

const integrationScaleSet = "par-integration"

// live runs a Controller against the node in the PAR_PVE_* variables.
type live struct {
	t   *testing.T
	ctx context.Context
	pve *proxmox.Client
	cfg *config.Config
	gh  *fakeGitHub
}

// newLive connects to the node, with the pool named by the poolVar variable, and destroys any workers the test
// leaves behind.
func newLive(t *testing.T, poolVar string) *live {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	t.Cleanup(cancel)
	h := &live{t: t, ctx: ctx, gh: newFakeGitHub()}

	first := h.envInt("PAR_PVE_TEST_VMID")
	pve, err := proxmox.New(proxmox.Options{
		URL:            h.env("PAR_PVE_URL", ""),
		TokenID:        h.env("PAR_PVE_TOKEN_ID", ""),
		TokenSecret:    h.env("PAR_PVE_TOKEN_SECRET", ""),
		TLSFingerprint: os.Getenv("PAR_PVE_FINGERPRINT"),
		Node:           h.env("PAR_PVE_NODE", ""),
		UserAgent:      "parcon-integration-test",
	})
	if err != nil {
		t.Fatalf("proxmox.New: %v", err)
	}
	h.pve = pve

	linked := true
	h.cfg = &config.Config{
		Proxmox: config.Proxmox{
			Node:    h.env("PAR_PVE_NODE", ""),
			Pool:    h.env(poolVar, ""),
			Storage: h.env("PAR_PVE_STORAGE", ""),
			VNet:    h.env("PAR_PVE_VNET", "parnet"),
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
			if !h.cfg.Proxmox.VMIDRange.Workers().Contains(vm.VMID) || !vm.HasTag(vmtags.Managed) || vm.Template {
				continue
			}
			t.Logf("cleanup: destroying leftover VM %d (%s, tags %s)", vm.VMID, vm.Name, strings.Join(vm.Tags, ";"))
			h.destroyIfPresent(vm.VMID)
		}
	})
	return h
}

func (h *live) env(name, def string) string {
	h.t.Helper()
	if v := os.Getenv(name); v != "" {
		return v
	}
	if def == "" {
		h.t.Skipf("%s is not set", name)
	}
	return def
}

func (h *live) envInt(name string) int {
	h.t.Helper()
	n, err := strconv.Atoi(h.env(name, ""))
	if err != nil {
		h.t.Fatalf("%s: %v", name, err)
	}
	return n
}

func (h *live) controller() *Controller {
	h.t.Helper()
	c, err := New(Options{Config: h.cfg, Proxmox: h.pve, GitHub: h.gh, Owner: "integration-test"})
	if err != nil {
		h.t.Fatalf("New: %v", err)
	}
	for _, s := range c.scaleSets {
		s.id = testScaleSetID
	}
	return c
}

// want reports assignedJobs to the scale set, as a GitHub message would.
func (h *live) want(c *Controller, assignedJobs int) {
	h.t.Helper()
	if _, err := c.scaleSets[integrationScaleSet].DesiredRunners(h.ctx, assignedJobs); err != nil {
		h.t.Fatal(err)
	}
}

// pass runs one reconcile pass and waits for the operations it started.
func (h *live) pass(c *Controller) {
	h.t.Helper()
	if err := c.reconcile(h.ctx); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	c.ops.Wait()
}

// vm returns a VM as Proxmox lists it.
func (h *live) vm(vmid int) (proxmox.VM, bool) {
	h.t.Helper()
	vms, err := h.pve.ListVMs(h.ctx)
	if err != nil {
		h.t.Fatalf("ListVMs: %v", err)
	}
	for _, vm := range vms {
		if vm.VMID == vmid {
			return vm, true
		}
	}
	return proxmox.VM{}, false
}

func (h *live) workers() []proxmox.VM {
	h.t.Helper()
	vms, err := h.pve.ListVMs(h.ctx)
	if err != nil {
		h.t.Fatalf("ListVMs: %v", err)
	}
	var out []proxmox.VM
	for _, vm := range vms {
		if vm.HasTag(vmtags.Worker) && vm.Pool == h.cfg.Proxmox.Pool && h.cfg.Proxmox.VMIDRange.Contains(vm.VMID) {
			out = append(out, vm)
		}
	}
	return out
}

// onlyWorker fails the test unless there is exactly one worker, and returns it.
func (h *live) onlyWorker(when string) (proxmox.VM, worker) {
	h.t.Helper()
	ws := h.workers()
	if len(ws) != 1 {
		h.t.Fatalf("workers %s: %+v, want one", when, ws)
	}
	parsed, err := parseWorker(ws[0])
	if err != nil {
		h.t.Fatalf("parseWorker: %v", err)
	}
	return ws[0], parsed
}

func (h *live) noWorkers(when string) {
	h.t.Helper()
	if ws := h.workers(); len(ws) != 0 {
		h.t.Fatalf("workers %s: %+v, want none", when, ws)
	}
}

// guestOutput runs a command in the guest through the guest agent and fails unless it prints want.
func (h *live) guestOutput(vmid int, want string, command ...string) {
	h.t.Helper()
	res, err := h.pve.AgentExec(h.ctx, vmid, command, nil)
	if err != nil {
		h.t.Fatalf("guest exec %v: %v", command, err)
	}
	if got := strings.TrimSpace(string(res.Stdout)); res.ExitCode != 0 || got != want {
		h.t.Fatalf("guest exec %v: exit %d, stdout %q, stderr %q; want %q", command, res.ExitCode, got, res.Stderr,
			want)
	}
}

// waitFor polls cond every 2s until it holds, failing the test after timeout.
func (h *live) waitFor(what string, timeout time.Duration, cond func() bool) {
	h.t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			h.t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		if !sleep(h.ctx, 2*time.Second) {
			h.t.Fatalf("waiting for %s: %v", what, h.ctx.Err())
		}
	}
}

// destroyIfPresent stops and destroys a VM, skipping one that is already gone. It checks the VM list first: for a
// token whose rights come from pool membership, Proxmox answers a VM that no longer exists with 403 Permission check
// failed, not "does not exist", because the missing VM isn't in the pool.
func (h *live) destroyIfPresent(vmid int) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	vms, err := h.pve.ListVMs(ctx)
	if err != nil {
		h.t.Errorf("cleanup: ListVMs: %v", err)
		return
	}
	for _, vm := range vms {
		if vm.VMID != vmid {
			continue
		}
		if vm.Status == "running" {
			if err := h.pve.Stop(ctx, vmid); err != nil {
				h.t.Logf("stop VM %d: %v", vmid, err)
			}
		}
		if err := h.pve.Destroy(ctx, vmid); err != nil {
			h.t.Errorf("destroy VM %d: %v; remove it by hand", vmid, err)
		}
		return
	}
}
