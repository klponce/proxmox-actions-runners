package controller

import (
	"slices"
	"testing"
	"time"
)

// These cover GitHub's runner API hanging, as it sometimes does for minutes: the controller must keep cleaning up
// and creating workers, not wait for it.

// slowGitHub is a harness whose GitHub calls give up after a few milliseconds instead of 30 seconds, with a ready
// worker already running.
func slowGitHub(t *testing.T) (*harness, int) {
	t.Helper()
	h := newHarness(t, testConfig())
	h.c.githubTimeout = 20 * time.Millisecond
	h.c.runnerCheckBudget = 50 * time.Millisecond
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	ready := h.readyWorkers()
	if len(ready) != 1 {
		t.Fatalf("ready workers = %v", ready)
	}
	return h, ready[0]
}

// timedPass runs a pass and fails the test if it takes long: a hung GitHub must not hold it up.
func (h *harness) timedPass() {
	h.t.Helper()
	start := time.Now()
	h.pass()
	if took := time.Since(start); took > 2*time.Second {
		h.t.Fatalf("a pass took %s while GitHub hung", took)
	}
}

// A worker that ran its job is destroyed, and its replacement created, while GitHub doesn't answer.
func TestFinishedWorkerIsDestroyedWhileGitHubHangs(t *testing.T) {
	h, vmid := slowGitHub(t)
	h.gh.setHang(true)
	h.pve.setStatus(vmid, "stopped")
	h.want(1) // another job is queued
	h.timedPass()

	if _, ok := h.pve.snapshot(vmid); ok {
		t.Errorf("the powered-off worker %d is still there", vmid)
	}
	h.timedPass()
	if ready := h.readyWorkers(); len(ready) != 1 || ready[0] == vmid {
		t.Errorf("ready workers = %v, want a new one", ready)
	}
}

// The runner check, which runs in the loop, gives up within its budget, so the pass still creates workers.
func TestRunnerCheckDoesNotStallTheLoop(t *testing.T) {
	h, vmid := slowGitHub(t)
	h.gh.setHang(true)
	h.clock.Advance(h.c.runnerCheckAfter + time.Minute) // the running worker's runner check is due
	h.want(2)
	h.timedPass()

	ready := h.readyWorkers()
	if len(ready) != 2 || !slices.Contains(ready, vmid) {
		t.Errorf("ready workers = %v, want %d and a new one", ready, vmid)
	}
}

// An idle worker's retirement gives up on a hung GitHub within the timeout, keeps the worker, and frees its slot;
// the next pass, with GitHub back, retires it.
func TestIdleRetirementGivesUpOnAHungGitHub(t *testing.T) {
	h, vmid := slowGitHub(t)
	h.gh.setHang(true)
	h.want(0)
	h.timedPass()
	if _, ok := h.pve.snapshot(vmid); !ok {
		t.Fatal("the worker was destroyed without its runner being unregistered")
	}
	if h.c.isBusy(vmid) {
		t.Fatal("the failed retirement still holds the worker")
	}

	h.gh.setHang(false)
	h.timedPass()
	if _, ok := h.pve.snapshot(vmid); ok {
		t.Error("the idle worker wasn't retired once GitHub answered")
	}
}
