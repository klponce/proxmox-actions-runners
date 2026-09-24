package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
)

func TestCheckTemplate(t *testing.T) {
	now := time.Now()
	template := func(vmid int, runner string) map[string]any {
		return map[string]any{"type": "qemu", "vmid": vmid, "node": "pve1", "pool": "par-runners", "template": 1,
			"tags": fmt.Sprintf("par-managed;par-template;par-tv-%d;par-rv-%s", now.Add(-50*time.Hour).Unix(), runner)}
	}
	tests := []struct {
		name     string
		vms      []map[string]any
		released time.Duration // how long ago the latest actions/runner was released
		lookup   error
		wantFail bool
		want     []string
	}{
		{
			name:     "current",
			vms:      []map[string]any{template(10999, "2.337.0")},
			released: 40 * 24 * time.Hour,
			want: []string{"ok    runner template 10999, created 2 days ago, actions/runner 2.337.0",
				"ok    actions/runner 2.337.0 is the latest release"},
		},
		{
			name:     "behind but within the grace period",
			vms:      []map[string]any{template(10999, "2.336.0")},
			released: 10*24*time.Hour + time.Hour,
			want:     []string{"ok    actions/runner 2.337.0 was released 10 days ago; install a newer runner image"},
		},
		{
			name:     "behind for too long",
			vms:      []map[string]any{template(10999, "2.336.0")},
			released: 22*24*time.Hour + time.Hour,
			wantFail: true,
			want:     []string{"FAIL  actions/runner 2.337.0 was released 22 days ago; GitHub stops accepting"},
		},
		{
			name:     "no template",
			vms:      []map[string]any{{"type": "qemu", "vmid": 10000, "node": "pve1", "pool": "par-runners"}},
			wantFail: true,
			want:     []string{"FAIL  no runner template in pool par-runners"},
		},
		{
			name:     "release lookup fails",
			vms:      []map[string]any{template(10999, "2.337.0")},
			lookup:   errors.New("rate limited"),
			wantFail: true,
			want:     []string{"FAIL  latest actions/runner release: rate limited"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			old := latestRunnerRelease
			t.Cleanup(func() { latestRunnerRelease = old })
			latestRunnerRelease = func(context.Context) (github.RunnerRelease, error) {
				return github.RunnerRelease{Version: "2.337.0", PublishedAt: now.Add(-tt.released)}, tt.lookup
			}
			srv := fakeProxmox(t, map[string]any{"/cluster/resources": tt.vms})
			cfg := writeCheckConfig(t, srv, 0o600)

			var stdout, stderr bytes.Buffer
			err := run([]string{"check", "template", "-config", cfg}, &stdout, &stderr)
			if got := outcomeOf(err); (got == failed) != tt.wantFail || got == usageError {
				t.Fatalf("run error = %v (outcome %s), wantFail %v\n%s", err, got, tt.wantFail, stdout.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output doesn't contain %q:\n%s", want, stdout.String())
				}
			}
		})
	}
}
