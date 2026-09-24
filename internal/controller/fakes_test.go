package controller

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

const (
	testPool       = "par-runners"
	testTemplateID = 9000
	testScaleSet   = "proxmox"
	testScaleSetID = 42
)

var testStart = time.Date(2026, 9, 24, 12, 0, 0, 0, time.UTC)

// clock is a settable time source.
type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

// fakeVM is a VM in fakeProxmox.
type fakeVM struct {
	vm       proxmox.VM
	config   map[string]string
	diskGrew int
	files    map[string]string
	// pings counts guest-agent pings since the VM started.
	pings int
}

// fakeProxmox keeps VMs in memory and mimics the Proxmox behavior the controller relies on: clones copy the
// template's tags, a running VM can't be destroyed, and the guest agent answers some pings after boot.
type fakeProxmox struct {
	mu  sync.Mutex
	vms map[int]*fakeVM
	// agentReadyAfter is how many pings a started VM's agent ignores. Negative means it never answers.
	agentReadyAfter int
	// onClone and onPing run on every clone and ping, for example to advance a clock.
	onClone func()
	onPing  func()
	// fail maps an operation name ("clone", "configure", "grow", "start", "write", "destroy") to an error for it.
	fail  map[string]error
	calls []string
}

func newFakeProxmox() *fakeProxmox {
	return &fakeProxmox{vms: map[int]*fakeVM{}, fail: map[string]error{}}
}

func (f *fakeProxmox) add(vm proxmox.VM) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vm.Status == "" {
		vm.Status = "stopped"
	}
	f.vms[vm.VMID] = &fakeVM{vm: vm, config: map[string]string{}, files: map[string]string{}}
}

func (f *fakeProxmox) addTemplate(vmid int, version int64) {
	f.add(proxmox.VM{VMID: vmid, Name: fmt.Sprintf("par-tpl-%d", version), Pool: testPool, Template: true,
		Tags: []string{TagManaged, TagTemplate, fmt.Sprintf("%s%d", tagTemplateVersionPrefix, version)}})
}

func (f *fakeProxmox) record(format string, args ...any) error {
	call := fmt.Sprintf(format, args...)
	f.calls = append(f.calls, call)
	return f.fail[strings.SplitN(call, " ", 2)[0]]
}

func (f *fakeProxmox) get(vmid int) (*fakeVM, error) {
	vm, ok := f.vms[vmid]
	if !ok {
		return nil, &proxmox.APIError{StatusCode: http.StatusInternalServerError,
			Message: fmt.Sprintf("Configuration file 'nodes/pve1/qemu-server/%d.conf' does not exist", vmid)}
	}
	return vm, nil
}

func (f *fakeProxmox) snapshot(vmid int) (fakeVM, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vm, ok := f.vms[vmid]
	if !ok {
		return fakeVM{}, false
	}
	return *vm, true
}

func (f *fakeProxmox) setStatus(vmid int, status string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.vms[vmid].vm.Status = status
}

func (f *fakeProxmox) ids() []int {
	f.mu.Lock()
	defer f.mu.Unlock()
	ids := make([]int, 0, len(f.vms))
	for id := range f.vms {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

// workers returns the non-template VMs tagged as workers, by VMID.
func (f *fakeProxmox) workers() []proxmox.VM {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []proxmox.VM
	for _, vm := range f.vms {
		if vm.vm.HasTag(TagWorker) {
			out = append(out, vm.vm)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

func (f *fakeProxmox) ListVMs(context.Context) ([]proxmox.VM, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fail["list"]; err != nil {
		return nil, err
	}
	out := make([]proxmox.VM, 0, len(f.vms))
	for _, vm := range f.vms {
		cp := vm.vm
		cp.Tags = append([]string(nil), vm.vm.Tags...)
		out = append(out, cp)
	}
	return out, nil
}

func (f *fakeProxmox) Clone(_ context.Context, opts proxmox.CloneOptions) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("clone %d->%d full=%v", opts.SourceVMID, opts.NewVMID, opts.Full); err != nil {
		return err
	}
	src, err := f.get(opts.SourceVMID)
	if err != nil {
		return err
	}
	if _, exists := f.vms[opts.NewVMID]; exists {
		return &proxmox.APIError{StatusCode: http.StatusInternalServerError,
			Message: fmt.Sprintf("VM %d already exists on node 'pve1'", opts.NewVMID)}
	}
	if f.onClone != nil {
		f.onClone()
	}
	f.vms[opts.NewVMID] = &fakeVM{
		vm: proxmox.VM{VMID: opts.NewVMID, Name: opts.Name, Pool: opts.Pool, Status: "stopped",
			Tags: append([]string(nil), src.vm.Tags...)},
		config: map[string]string{"description": opts.Description},
		files:  map[string]string{},
	}
	return nil
}

func (f *fakeProxmox) SetConfig(_ context.Context, vmid int, settings map[string]string, _ ...string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("configure %d", vmid); err != nil {
		return err
	}
	vm, err := f.get(vmid)
	if err != nil {
		return err
	}
	for k, v := range settings {
		vm.config[k] = v
		if k == "tags" {
			vm.vm.Tags = proxmox.ParseTags(v)
		}
	}
	return nil
}

func (f *fakeProxmox) GrowDisk(_ context.Context, vmid int, disk string, addGiB int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("grow %d %s +%d", vmid, disk, addGiB); err != nil {
		return err
	}
	vm, err := f.get(vmid)
	if err != nil {
		return err
	}
	vm.diskGrew += addGiB
	return nil
}

func (f *fakeProxmox) Start(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("start %d", vmid); err != nil {
		return err
	}
	vm, err := f.get(vmid)
	if err != nil {
		return err
	}
	vm.vm.Status, vm.pings = "running", 0
	return nil
}

func (f *fakeProxmox) Stop(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("stop %d", vmid); err != nil {
		return err
	}
	vm, err := f.get(vmid)
	if err != nil {
		return err
	}
	vm.vm.Status = "stopped"
	return nil
}

func (f *fakeProxmox) Destroy(_ context.Context, vmid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("destroy %d", vmid); err != nil {
		return err
	}
	vm, err := f.get(vmid)
	if err != nil {
		return err
	}
	if vm.vm.Status == "running" {
		return &proxmox.TaskError{Type: "qmdestroy", ExitStatus: fmt.Sprintf("VM %d is running - destroy failed", vmid)}
	}
	delete(f.vms, vmid)
	return nil
}

func (f *fakeProxmox) AgentPing(_ context.Context, vmid int) error {
	f.mu.Lock()
	onPing := f.onPing
	vm, err := f.get(vmid)
	if err == nil && vm.vm.Status == "running" {
		vm.pings++
	}
	var ready bool
	if err == nil {
		ready = vm.vm.Status == "running" && f.agentReadyAfter >= 0 && vm.pings > f.agentReadyAfter
	}
	f.mu.Unlock()
	if onPing != nil {
		onPing()
	}
	switch {
	case err != nil:
		return err
	case !ready:
		return &proxmox.APIError{StatusCode: http.StatusInternalServerError, Message: "QEMU guest agent is not running"}
	}
	return nil
}

func (f *fakeProxmox) AgentWriteFile(_ context.Context, vmid int, path string, content []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.record("write %d %s", vmid, path); err != nil {
		return err
	}
	vm, err := f.get(vmid)
	if err != nil {
		return err
	}
	vm.files[path] = string(content)
	return nil
}

// fakeGitHub keeps runners in memory. Listen blocks until its context ends and hands its handler to the test.
type fakeGitHub struct {
	mu        sync.Mutex
	runners   map[string]int64
	nextID    int64
	busy      map[string]bool
	jitErr    error
	ensureErr []error
	handlers  chan github.Handler
	removed   []string
}

func newFakeGitHub() *fakeGitHub {
	return &fakeGitHub{runners: map[string]int64{}, busy: map[string]bool{}, nextID: 100,
		handlers: make(chan github.Handler, 10)}
}

func (f *fakeGitHub) EnsureScaleSet(_ context.Context, spec github.ScaleSetSpec) (github.ScaleSet, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.ensureErr) > 0 {
		err := f.ensureErr[0]
		f.ensureErr = f.ensureErr[1:]
		return github.ScaleSet{}, err
	}
	return github.ScaleSet{ID: testScaleSetID, Name: spec.Name}, nil
}

func (f *fakeGitHub) GenerateJITConfig(_ context.Context, _ int, name string) (github.JITConfig, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.jitErr != nil {
		return github.JITConfig{}, f.jitErr
	}
	f.nextID++
	f.runners[name] = f.nextID
	return newJIT(f.nextID, name), nil
}

func (f *fakeGitHub) RunnerByName(_ context.Context, name string) (*github.Runner, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	id, ok := f.runners[name]
	if !ok {
		return nil, nil
	}
	return &github.Runner{ID: id, Name: name, ScaleSetID: testScaleSetID}, nil
}

func (f *fakeGitHub) RemoveRunner(_ context.Context, id int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for name, rid := range f.runners {
		if rid == id {
			if f.busy[name] {
				return fmt.Errorf("remove: %w", github.ErrJobStillRunning)
			}
			delete(f.runners, name)
			f.removed = append(f.removed, name)
		}
	}
	return nil
}

func (f *fakeGitHub) Listen(ctx context.Context, _ github.ListenOptions, h github.Handler) {
	f.handlers <- h
	<-ctx.Done()
}

func (f *fakeGitHub) hasRunner(name string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.runners[name]
	return ok
}

func (f *fakeGitHub) setBusy(name string, busy bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.busy[name] = busy
}

func (f *fakeGitHub) forgetRunner(name string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.runners, name)
}

// newJIT returns the fake JIT config for a runner. Its content names the runner, so tests can check which config
// reached which VM.
func newJIT(id int64, name string) github.JITConfig {
	return github.NewJITConfig(id, name, "jit-for-"+name)
}

// harness wires a Controller to the fakes with a settable clock.
type harness struct {
	t     *testing.T
	c     *Controller
	pve   *fakeProxmox
	gh    *fakeGitHub
	clock *clock
	cfg   *config.Config
}

func testConfig(scaleSets ...config.ScaleSet) *config.Config {
	linked := true
	if len(scaleSets) == 0 {
		scaleSets = []config.ScaleSet{testScaleSetConfig()}
	}
	return &config.Config{
		Proxmox: config.Proxmox{Node: "pve1", Pool: testPool, Storage: "local-lvm", VNet: "parnet",
			VMIDRange: config.VMIDRange{Start: 10000, End: 10009}, LinkedClone: &linked},
		ScaleSets: scaleSets,
	}
}

func testScaleSetConfig() config.ScaleSet {
	return config.ScaleSet{Name: testScaleSet, Labels: []string{testScaleSet}, MinRunners: 0, MaxRunners: 5,
		MaxLifetime: 6 * time.Hour, RunnerGroup: "default",
		Worker: config.Worker{Cores: 2, MemoryMiB: 8192, FreeDiskGiB: 14}}
}

func newHarness(t *testing.T, cfg *config.Config) *harness {
	t.Helper()
	h := &harness{t: t, pve: newFakeProxmox(), gh: newFakeGitHub(), clock: &clock{now: testStart}, cfg: cfg}
	c, err := New(Options{Config: cfg, Proxmox: h.pve, GitHub: h.gh, Owner: "test", Now: h.clock.Now,
		RunnerCheckAfter: 10 * time.Minute, RunnerCheckEvery: 5 * time.Minute, RetryBackoff: time.Minute})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c.agentPoll = time.Millisecond
	for _, s := range c.scaleSets {
		s.id = testScaleSetID
	}
	h.c = c
	return h
}

// want reports assignedJobs to the test scale set's handler, as a GitHub message would.
func (h *harness) want(assignedJobs int) {
	h.t.Helper()
	if _, err := h.c.scaleSets[testScaleSet].DesiredRunners(context.Background(), assignedJobs); err != nil {
		h.t.Fatal(err)
	}
}

// pass runs one reconcile pass and waits for the operations it started.
func (h *harness) pass() {
	h.t.Helper()
	if err := h.c.reconcile(context.Background()); err != nil {
		h.t.Fatalf("reconcile: %v", err)
	}
	h.c.ops.Wait()
}

// readyWorkers returns the VMIDs of workers tagged ready.
func (h *harness) readyWorkers() []int {
	var ids []int
	for _, vm := range h.pve.workers() {
		if vm.HasTag(TagReady) {
			ids = append(ids, vm.VMID)
		}
	}
	return ids
}

// nameOf returns a worker VM's runner name.
func (h *harness) nameOf(vmid int) string {
	h.t.Helper()
	vm, ok := h.pve.snapshot(vmid)
	if !ok {
		h.t.Fatalf("VM %d doesn't exist", vmid)
	}
	w, err := parseWorker(vm.vm)
	if err != nil {
		h.t.Fatal(err)
	}
	return w.name
}
