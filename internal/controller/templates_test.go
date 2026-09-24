package controller

import (
	"errors"
	"log/slog"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

func TestPruneTemplates(t *testing.T) {
	// Templates live in the reserved IDs at the end of the test range (10006-10009).
	worker := func(vmid, template int) proxmox.VM {
		return proxmox.VM{VMID: vmid, Pool: testPool, Status: "running",
			Tags: workerTags(testScaleSet, testStart, template, true)}
	}
	tests := []struct {
		name  string
		extra []proxmox.VM
		busy  map[int]string
		want  []int
	}{
		{
			name: "unreferenced older templates are removed",
			want: []int{10009},
		},
		{
			name:  "a template a worker was cloned from stays",
			extra: []proxmox.VM{worker(10000, 10007)},
			want:  []int{10000, 10007, 10009},
		},
		{
			name: "a worker without a template reference stops all pruning",
			extra: []proxmox.VM{{VMID: 10000, Pool: testPool, Status: "running",
				Tags: slices.DeleteFunc(workerTags(testScaleSet, testStart, 10007, true), func(tag string) bool {
					return strings.HasPrefix(tag, vmtags.TemplateRefPrefix)
				})}},
			want: []int{10000, 10006, 10007, 10009},
		},
		{
			name: "nothing is pruned while a worker is being created",
			busy: map[int]string{10001: opCreatePrefix + testScaleSet},
			want: []int{10006, 10007, 10009},
		},
		{
			name: "templates in other pools and unmanaged ones are left alone",
			extra: []proxmox.VM{
				{VMID: 9000, Pool: "other", Template: true, Tags: []string{vmtags.Managed, vmtags.Template, "par-tv-1"}},
				{VMID: 9001, Pool: testPool, Template: true, Tags: []string{vmtags.Template, "par-tv-1"}},
			},
			want: []int{9000, 9001, 10009},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newHarness(t, testConfig())
			h.pve.addTemplate(10006, 100)
			h.pve.addTemplate(10007, 200)
			h.pve.addTemplate(10009, 300) // the newest
			for _, vm := range tt.extra {
				h.pve.add(vm)
			}
			for vmid, op := range tt.busy {
				h.c.busy[vmid] = op
			}
			h.pass()
			if got := h.pve.ids(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("VMs left = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkersRecordTheirTemplate(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.addTemplate(testTemplateID, 1)
	h.want(1)
	h.pass()
	vm, _ := h.pve.snapshot(h.readyWorkers()[0])
	if ref, ok := vmtags.Int(vm.vm, vmtags.TemplateRefPrefix); !ok || ref != testTemplateID {
		t.Errorf("worker tags = %v, want a reference to template %d", vm.vm.Tags, testTemplateID)
	}
}

func TestPruneWithoutTemplate(t *testing.T) {
	h := newHarness(t, testConfig())
	h.pve.add(proxmox.VM{VMID: 10008, Pool: testPool, Template: true, Tags: []string{vmtags.Managed, vmtags.Template}})
	h.pass()
	if got := h.pve.ids(); !reflect.DeepEqual(got, []int{10008}) {
		t.Errorf("VMs left = %v; a template without a version was removed", got)
	}
}

func TestRunnerFreshness(t *testing.T) {
	released := testStart
	latest := github.RunnerRelease{Version: "2.338.0", PublishedAt: released}
	tests := []struct {
		name      string
		have      string
		age       time.Duration
		wantLevel slog.Level
		wantShow  bool
	}{
		{"current", "2.338.0", 40 * 24 * time.Hour, 0, false},
		{"newer than the latest", "2.339.0", 40 * 24 * time.Hour, 0, false},
		{"behind for 6 days", "2.337.0", 6 * 24 * time.Hour, slog.LevelInfo, true},
		{"behind for 7 days", "2.337.0", 7 * 24 * time.Hour, slog.LevelWarn, true},
		{"behind for 21 days", "2.337.0", 21 * 24 * time.Hour, slog.LevelError, true},
		{"no runner version tag", "", time.Hour, slog.LevelInfo, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			level, show := runnerFreshness(tt.have, latest, released.Add(tt.age))
			if show != tt.wantShow || (show && level != tt.wantLevel) {
				t.Errorf("runnerFreshness = %v, %v; want %v, %v", level, show, tt.wantLevel, tt.wantShow)
			}
		})
	}
}

func TestRunnerFreshnessCheckCadence(t *testing.T) {
	h := newHarness(t, testConfig())
	var logs strings.Builder
	h.c.logger = slog.New(slog.NewTextHandler(&logs, nil))
	h.pve.add(proxmox.VM{VMID: 10009, Pool: testPool, Template: true,
		Tags: []string{vmtags.Managed, vmtags.Template, "par-tv-1", "par-rv-2.337.0"}})
	h.gh.latest = github.RunnerRelease{Version: "2.338.0", PublishedAt: testStart.Add(-8 * 24 * time.Hour)}

	h.pass()
	if h.gh.releaseCalls != 1 {
		t.Fatalf("release lookups = %d, want 1", h.gh.releaseCalls)
	}
	if !strings.Contains(logs.String(), "level=WARN") || !strings.Contains(logs.String(), "latestRunner=2.338.0") {
		t.Errorf("no warning about the old runner:\n%s", logs.String())
	}

	h.clock.Advance(runnerCheckInterval - time.Minute)
	h.pass()
	if h.gh.releaseCalls != 1 {
		t.Errorf("release looked up again after %s", runnerCheckInterval-time.Minute)
	}
	h.clock.Advance(time.Minute)
	h.pass()
	if h.gh.releaseCalls != 2 {
		t.Errorf("release lookups = %d after %s, want 2", h.gh.releaseCalls, runnerCheckInterval)
	}

	// A failed lookup is retried sooner.
	h.gh.releaseErr = errors.New("GitHub is down")
	h.clock.Advance(runnerCheckInterval)
	h.pass()
	h.gh.releaseErr = nil
	h.clock.Advance(runnerCheckRetry)
	h.pass()
	if h.gh.releaseCalls != 4 {
		t.Errorf("release lookups = %d, want a retry an hour after the failure", h.gh.releaseCalls)
	}
}
