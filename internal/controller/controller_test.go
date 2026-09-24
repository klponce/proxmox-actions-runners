package controller

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

func TestWorkerNamingRoundTrip(t *testing.T) {
	created := testStart
	vm := proxmox.VM{VMID: 10003, Tags: workerTags("proxmox-ubuntu-26.04", created, true)}
	w, err := parseWorker(vm)
	if err != nil {
		t.Fatalf("parseWorker: %v", err)
	}
	if w.scaleSet != "proxmox-ubuntu-26.04" || !w.created.Equal(created) || !w.ready {
		t.Errorf("parseWorker = %+v", w)
	}
	if w.name != workerName(10003, created) || !strings.HasPrefix(w.name, "par-10003-") {
		t.Errorf("name = %q", w.name)
	}
	if workerName(10003, created) == workerName(10003, created.Add(time.Second)) {
		t.Error("a reused VMID reuses the runner name")
	}

	for _, tags := range [][]string{
		{TagManaged, TagWorker},
		{TagManaged, TagWorker, "par-ss-x"},
		{TagManaged, TagWorker, "par-ss-x", "par-created-soon"},
	} {
		if _, err := parseWorker(proxmox.VM{VMID: 1, Tags: tags}); err == nil {
			t.Errorf("parseWorker(%v) succeeded", tags)
		}
	}
}

func TestScaleUpCreatesReadyWorkers(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	h.pve.agentReadyAfter = 2
	h.want(2)
	h.pass()

	ready := h.readyWorkers()
	if !reflect.DeepEqual(ready, []int{10000, 10001}) {
		t.Fatalf("ready workers = %v, want [10000 10001]", ready)
	}
	for _, vmid := range ready {
		vm, _ := h.pve.snapshot(vmid)
		name := h.nameOf(vmid)
		if vm.vm.Name != name || vm.vm.Status != "running" || vm.vm.Pool != testPool {
			t.Errorf("VM %d = %+v", vmid, vm.vm)
		}
		want := map[string]string{"cores": "2", "memory": "8192", "net0": "virtio,bridge=parnet",
			"ipconfig0": "ip=dhcp", "onboot": "0", "agent": "1", "ciupgrade": "0"}
		for k, v := range want {
			if vm.config[k] != v {
				t.Errorf("VM %d %s = %q, want %q", vmid, k, vm.config[k], v)
			}
		}
		if vm.diskGrew != 14 {
			t.Errorf("VM %d disk grew %d GiB, want 14", vmid, vm.diskGrew)
		}
		if got := vm.files[JITConfigPath]; got != "jit-for-"+name {
			t.Errorf("VM %d JIT config = %q, want the one for %s", vmid, got, name)
		}
		if !h.gh.hasRunner(name) {
			t.Errorf("runner %s isn't registered", name)
		}
		if strings.Contains(vm.config["description"], "jit-for") {
			t.Errorf("VM %d description leaks the JIT config", vmid)
		}
	}

	// Already at the target: another pass changes nothing.
	calls := len(h.pve.calls)
	h.pass()
	if len(h.pve.calls) != calls {
		t.Errorf("a pass at the target made calls: %v", h.pve.calls[calls:])
	}
}

func TestMinRunnersAndMaxRunners(t *testing.T) {
	s := testScaleSetConfig()
	s.MinRunners, s.MaxRunners = 1, 3
	h := newHarness(t, testConfig(s))
	h.pve.addTemplate(testTemplateID, 1)

	h.want(0)
	h.pass()
	if n := len(h.readyWorkers()); n != 1 {
		t.Errorf("with no jobs: %d workers, want minRunners 1", n)
	}
	h.want(10)
	h.pass()
	if n := len(h.readyWorkers()); n != 3 {
		t.Errorf("with 10 jobs: %d workers, want maxRunners 3", n)
	}
}

func TestNoScalingUntilGitHubReports(t *testing.T) {
	s := testScaleSetConfig()
	s.MinRunners = 2
	h := newHarness(t, testConfig(s))
	h.pve.addTemplate(testTemplateID, 1)
	h.pass()
	if n := len(h.pve.workers()); n != 0 {
		t.Errorf("created %d workers before GitHub reported a target", n)
	}
}

func TestNoTemplate(t *testing.T) {
	h := newHarness(t, testConfig())
	h.want(1)
	h.pass()
	if n := len(h.pve.workers()); n != 0 {
		t.Errorf("created %d workers without a template", n)
	}
}

func TestNewestTemplateIsUsed(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(9000, 100)
	h.pve.addTemplate(9001, 300)
	h.pve.addTemplate(9002, 200)
	// Not ours: no managed tag, or another pool.
	h.pve.add(proxmox.VM{VMID: 9003, Pool: testPool, Template: true, Tags: []string{TagTemplate, "par-tv-999"}})
	h.pve.add(proxmox.VM{VMID: 9004, Pool: "other", Template: true,
		Tags: []string{TagManaged, TagTemplate, "par-tv-999"}})
	h.want(1)
	h.pass()
	if len(h.pve.calls) == 0 || h.pve.calls[0] != "clone 9001->10000 full=false" {
		t.Errorf("first call = %v, want a linked clone of template 9001", h.pve.calls)
	}
}

func TestNegativeTemplateVersion(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, -1)
	h.want(1)
	h.pass()
	if len(h.pve.calls) == 0 || h.pve.calls[0] != "clone 9000->10000 full=false" {
		t.Errorf("first call = %v, want a linked clone of template 9000", h.pve.calls)
	}
}

func TestFullClones(t *testing.T) {
	cfg := testConfig()
	full := false
	cfg.Proxmox.LinkedClone = &full
	h := newHarness(t, cfg)
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	if h.pve.calls[0] != "clone 9000->10000 full=true" {
		t.Errorf("first call = %q, want a full clone", h.pve.calls[0])
	}
}

func TestFinishedWorkerIsReplaced(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	first := h.readyWorkers()[0]
	name := h.nameOf(first)

	// The runner takes a job, finishes it, and the VM powers itself off.
	ctx := context.Background()
	s := h.c.scaleSets[testScaleSet]
	_ = s.JobStarted(ctx, github.Job{RunnerName: name, JobID: "job-1"})
	_ = s.JobCompleted(ctx, github.Job{RunnerName: name, JobID: "job-1", Result: "succeeded"})
	// GitHub hasn't caught up yet and still reports the runner busy; the VM is off, so it goes anyway.
	h.gh.setBusy(name, true)
	h.pve.setStatus(first, "stopped")

	h.clock.Advance(time.Minute)
	h.pass() // retires the stopped worker
	if _, ok := h.pve.snapshot(first); ok {
		t.Errorf("stopped worker %d wasn't destroyed", first)
	}
	h.pass() // replaces it; the next job keeps the target at 1
	if n := len(h.readyWorkers()); n != 1 {
		t.Errorf("%d ready workers after replacement, want 1", n)
	}
}

func TestMaxLifetimeIsForced(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	vmid := h.readyWorkers()[0]
	h.gh.setBusy(h.nameOf(vmid), true) // stuck in a job

	h.want(0)
	h.clock.Advance(6*time.Hour + time.Second)
	h.pass()
	if _, ok := h.pve.snapshot(vmid); ok {
		t.Errorf("worker %d past maxLifetime wasn't destroyed", vmid)
	}
}

func TestScaleDownRetiresOldestIdleWorkers(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	for range 3 {
		h.want(len(h.readyWorkers()) + 1)
		h.pass()
		h.clock.Advance(time.Minute)
	}
	ids := h.readyWorkers()
	if len(ids) != 3 {
		t.Fatalf("ready workers = %v, want 3", ids)
	}
	// The oldest runner is busy by GitHub's account but the controller missed the JobStarted message.
	h.gh.setBusy(h.nameOf(ids[0]), true)
	// The second is busy and the controller knows it.
	_ = h.c.scaleSets[testScaleSet].JobStarted(context.Background(), github.Job{RunnerName: h.nameOf(ids[1])})

	h.want(1)
	h.pass()
	if got := h.readyWorkers(); !reflect.DeepEqual(got, []int{ids[0], ids[1]}) {
		t.Errorf("workers after scale down = %v, want the two busy ones %v", got, ids[:2])
	}
	h.pass() // the busy worker refuses to go on every pass; nothing else changes
	if got := h.readyWorkers(); len(got) != 2 {
		t.Errorf("workers after another pass = %v", got)
	}
}

func TestHalfCreatedWorkersAreDestroyed(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	created := testStart.Add(-time.Minute)

	// A worker left unready by a crash, and one whose runner already took a job before the crash.
	idle := proxmox.VM{VMID: 10000, Pool: testPool, Status: "running", Tags: workerTags(testScaleSet, created, false)}
	working := proxmox.VM{VMID: 10001, Pool: testPool, Status: "running", Tags: workerTags(testScaleSet, created, false)}
	h.pve.add(idle)
	h.pve.add(working)
	workingName := workerName(10001, created)
	if _, err := h.gh.GenerateJITConfig(context.Background(), testScaleSetID, workingName); err != nil {
		t.Fatal(err)
	}
	h.gh.setBusy(workingName, true)
	// A clone that never got worker tags still carries the template's.
	h.pve.add(proxmox.VM{VMID: 10002, Pool: testPool, Tags: []string{TagManaged, TagTemplate, "par-tv-1"}})
	// A worker with broken tags.
	h.pve.add(proxmox.VM{VMID: 10003, Pool: testPool, Tags: []string{TagManaged, TagWorker}})

	h.pass()
	if got := h.pve.ids(); !reflect.DeepEqual(got, []int{testTemplateID, 10001}) {
		t.Errorf("VMs left = %v, want the template and the busy half-created worker", got)
	}
}

func TestOnlyOwnedVMsAreTouched(t *testing.T) {
	h := newHarness(t, testConfig())
	vms := []proxmox.VM{
		// Unmanaged VM in the pool and range.
		{VMID: 10000, Pool: testPool, Tags: []string{TagTemplate}},
		// Managed stray outside the VMID range.
		{VMID: 500, Pool: testPool, Tags: []string{TagManaged}},
		// Managed stray in another pool.
		{VMID: 10001, Pool: "other", Tags: []string{TagManaged}},
		// A template build VM.
		{VMID: 10002, Pool: testPool, Status: "running", Tags: []string{TagManaged, TagBuild}},
		// A stopped worker of another pool.
		{VMID: 10003, Pool: "other", Tags: workerTags(testScaleSet, testStart, true)},
	}
	for _, vm := range vms {
		h.pve.add(vm)
	}
	h.pass()
	if got := h.pve.ids(); !reflect.DeepEqual(got, []int{500, 10000, 10001, 10002, 10003}) {
		t.Errorf("VMs left = %v; the controller touched a VM it doesn't own", got)
	}
	for _, call := range h.pve.calls {
		t.Errorf("unexpected call %q", call)
	}
}

func TestRemovedScaleSet(t *testing.T) {
	h := newHarness(t, testConfig())
	gone := proxmox.VM{VMID: 10000, Pool: testPool, Status: "running", Tags: workerTags("old", testStart, true)}
	busy := proxmox.VM{VMID: 10001, Pool: testPool, Status: "running", Tags: workerTags("old", testStart, true)}
	h.pve.add(gone)
	h.pve.add(busy)
	busyName := workerName(10001, testStart)
	if _, err := h.gh.GenerateJITConfig(context.Background(), testScaleSetID, busyName); err != nil {
		t.Fatal(err)
	}
	h.gh.setBusy(busyName, true)

	h.pass()
	if got := h.pve.ids(); !reflect.DeepEqual(got, []int{10001}) {
		t.Errorf("VMs left = %v, want only the busy worker of the removed scale set", got)
	}
	h.gh.setBusy(busyName, false)
	h.pass() // GitHub is asked again only after RunnerCheckEvery
	if got := h.pve.ids(); !reflect.DeepEqual(got, []int{10001}) {
		t.Errorf("VMs left = %v, want the retirement retried only after RunnerCheckEvery", got)
	}
	h.clock.Advance(5 * time.Minute)
	h.pass()
	if got := h.pve.ids(); len(got) != 0 {
		t.Errorf("VMs left = %v after its job ended", got)
	}
}

func TestRemovedScaleSetKeepsDefaultMaxLifetime(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.add(proxmox.VM{VMID: 10000, Pool: testPool, Status: "running", Tags: workerTags("old", testStart, true)})
	name := workerName(10000, testStart)
	if _, err := h.gh.GenerateJITConfig(context.Background(), testScaleSetID, name); err != nil {
		t.Fatal(err)
	}
	h.gh.setBusy(name, true) // stuck in a job

	h.clock.Advance(config.DefaultMaxLifetime + time.Second)
	h.pass()
	if got := h.pve.ids(); len(got) != 0 {
		t.Errorf("VMs left = %v, want the worker past the default maxLifetime destroyed", got)
	}
}

func TestWorkerWithoutRunnerIsRetired(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	vmid := h.readyWorkers()[0]
	h.gh.forgetRunner(h.nameOf(vmid))

	h.pass()
	if _, ok := h.pve.snapshot(vmid); !ok {
		t.Fatal("worker retired before RunnerCheckAfter")
	}
	h.clock.Advance(11 * time.Minute)
	h.pass()
	if _, ok := h.pve.snapshot(vmid); ok {
		t.Error("worker without a runner wasn't retired")
	}
}

func TestFailedCreationIsCleanedUp(t *testing.T) {
	tests := []struct {
		name     string
		sabotage func(h *harness)
	}{
		{"configure fails", func(h *harness) { h.pve.fail["configure"] = errors.New("storage full") }},
		{"start fails", func(h *harness) { h.pve.fail["start"] = errors.New("no memory") }},
		{"JIT delivery fails", func(h *harness) { h.pve.fail["write"] = errors.New("agent error") }},
		{"runner registration fails", func(h *harness) { h.gh.jitErr = errors.New("GitHub is down") }},
		{"guest agent never answers", func(h *harness) {
			h.pve.agentReadyAfter = -1
			h.pve.onPing = func() { h.clock.Advance(time.Minute) }
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testConfig())
			h.pve.addTemplate(testTemplateID, 1)
			tt.sabotage(h)
			h.want(1)
			h.pass()

			if got := h.pve.ids(); !reflect.DeepEqual(got, []int{testTemplateID}) {
				t.Errorf("VMs left = %v, want the half-built worker destroyed", got)
			}
			h.gh.mu.Lock()
			runners := len(h.gh.runners)
			h.gh.mu.Unlock()
			if runners != 0 {
				t.Errorf("%d runners left registered", runners)
			}

			// The scale set backs off before trying again.
			h.pve.fail = map[string]error{}
			h.gh.jitErr = nil
			h.pve.agentReadyAfter, h.pve.onPing = 0, nil
			clones := strings.Count(strings.Join(h.pve.calls, "\n"), "clone ")
			h.pass()
			if n := strings.Count(strings.Join(h.pve.calls, "\n"), "clone "); n != clones {
				t.Errorf("retried during the backoff")
			}
			h.clock.Advance(2 * time.Minute)
			h.pass()
			if n := len(h.readyWorkers()); n != 1 {
				t.Errorf("%d ready workers after the backoff, want 1", n)
			}
		})
	}
}

func TestForeignVMIDIsSkipped(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	// VMID 10000 is taken by a VM the token can't see: the fake refuses the clone like Proxmox would.
	h.pve.fail["clone"] = &proxmox.APIError{StatusCode: http.StatusInternalServerError,
		Message: "VM 10000 already exists on node 'pve1'"}
	h.want(1)
	h.pass()
	h.pve.fail = map[string]error{}
	h.clock.Advance(2 * time.Minute)
	h.pass()
	if got := h.readyWorkers(); !reflect.DeepEqual(got, []int{10001}) {
		t.Errorf("ready workers = %v, want [10001]", got)
	}
}

func TestVMIDRangeExhausted(t *testing.T) {
	cfg := testConfig()
	// Two worker IDs, 10000 and 10001, and the four reserved ones.
	cfg.Proxmox.VMIDRange = config.VMIDRange{Start: 10000, End: 10005}
	h := newHarness(t, cfg)
	h.pve.addTemplate(testTemplateID, 1)
	h.want(5)
	h.pass()
	if got := h.readyWorkers(); !reflect.DeepEqual(got, []int{10000, 10001}) {
		t.Errorf("ready workers = %v, want [10000 10001]: the worker IDs, never the reserved ones", got)
	}
}

// Proxmox reports status from pvestatd, up to about 10s late, and "unknown" for a VM it hasn't sampled yet. A ready
// worker in that state must be left alone, not retired as not running.
func TestUnknownStatusKeepsWorker(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	ready := h.readyWorkers()
	if len(ready) != 1 {
		t.Fatalf("ready workers = %v, want one", ready)
	}
	vmid := ready[0]
	name := h.nameOf(vmid)

	for _, status := range []string{"unknown", ""} {
		h.pve.setStatus(vmid, status)
		h.pass()
		if _, ok := h.pve.snapshot(vmid); !ok {
			t.Fatalf("worker with status %q was destroyed", status)
		}
		if !h.gh.hasRunner(name) {
			t.Fatalf("worker with status %q lost its runner", status)
		}
	}

	// Once Proxmox says it stopped, it is retired as usual.
	h.pve.setStatus(vmid, "stopped")
	h.pass()
	if _, ok := h.pve.snapshot(vmid); ok {
		t.Error("stopped worker wasn't retired")
	}
}

// A new template shows template: 0 for about 10s while it already carries the template's tags, like a half-created
// worker clone. In the reserved part of the VMID range it must be left alone, and used only once Proxmox reports it
// as a template.
func TestTemplateInReservedIDsIsNeverAStray(t *testing.T) {
	h := newHarness(t, testConfig()) // VMID range 10000-10009; 10006-10009 are reserved
	h.pve.addTemplate(testTemplateID, 1)
	newTemplate := proxmox.VM{VMID: 10009, Pool: testPool, Tags: []string{TagManaged, TagTemplate, "par-tv-2"}}
	h.pve.add(newTemplate)
	// Something else the template builder owns in the reserved IDs, such as a smoke-test clone.
	h.pve.add(proxmox.VM{VMID: 10008, Pool: testPool, Status: "running", Tags: []string{TagManaged}})

	h.want(1)
	h.pass()
	if got := h.pve.ids(); !reflect.DeepEqual(got, []int{testTemplateID, 10000, 10008, 10009}) {
		t.Fatalf("VMs = %v, want the old template, one worker, and both reserved VMs untouched", got)
	}
	if h.pve.calls[0] != "clone 9000->10000 full=false" {
		t.Errorf("first call = %q, want a clone of the old template while the new one isn't reported yet",
			h.pve.calls[0])
	}

	// Proxmox catches up: the new template is used for the next worker.
	newTemplate.Template = true
	h.pve.add(newTemplate)
	h.want(2)
	h.pass()
	if !slices.Contains(h.pve.calls, "clone 10009->10001 full=false") {
		t.Errorf("calls = %v, want a clone of the new template 10009", h.pve.calls)
	}
}

func TestListFailure(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.fail["list"] = errors.New("API down")
	if err := h.c.reconcile(context.Background()); err == nil {
		t.Error("reconcile succeeded without a VM list")
	}
}

func TestRunRegistersListensAndLeavesWorkersOnShutdown(t *testing.T) {
	h := newHarness(t, testConfig())
	h.c.resync = 10 * time.Millisecond
	h.pve.addTemplate(testTemplateID, 1)
	for _, s := range h.c.scaleSets {
		s.id = 0 // Run must learn it
	}
	h.gh.ensureErr = []error{errors.New("GitHub is down")}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- h.c.Run(ctx) }()

	var handler github.Handler
	select {
	case handler = <-h.gh.handlers:
	case <-time.After(10 * time.Second):
		t.Fatal("Run never started listening")
	}
	if h.c.scaleSets[testScaleSet].id != testScaleSetID {
		t.Errorf("scale set ID = %d, want %d", h.c.scaleSets[testScaleSet].id, testScaleSetID)
	}
	if _, err := handler.DesiredRunners(ctx, 1); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for len(h.readyWorkers()) != 1 {
		if time.Now().After(deadline) {
			t.Fatal("Run never created the worker")
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run didn't stop")
	}
	if n := len(h.readyWorkers()); n != 1 {
		t.Errorf("%d workers after shutdown, want the running one left alone", n)
	}
}

func TestRunStopsWhileRegistering(t *testing.T) {
	h := newHarness(t, testConfig())
	h.gh.ensureErr = []error{errors.New("down"), errors.New("down"), errors.New("down")}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := h.c.Run(ctx); err == nil || !strings.Contains(err.Error(), "register scale set") {
		t.Fatalf("Run error = %v, want a registration error", err)
	}
}

func TestNewRequiresClients(t *testing.T) {
	if _, err := New(Options{Config: testConfig()}); err == nil {
		t.Error("New without clients succeeded")
	}
}
