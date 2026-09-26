package controller

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/github"
)

func TestStatusFile(t *testing.T) {
	h := newHarness(t, testConfig())
	h.c.statusPath = filepath.Join(t.TempDir(), "status.json")
	h.c.status.Version = "0.2.0"
	h.pve.addTemplate(testTemplateID, 1)
	s := h.c.scaleSets[testScaleSet]
	s.SessionOpened()
	s.RecordStats(github.Stats{AssignedJobs: 2, BusyRunners: 1, RegisteredRunners: 2})
	h.want(2)
	h.pass()
	// A pass counts the workers it lists, before the creations it starts; the next pass sees them.
	h.pass()
	h.c.writeStatus(context.Background(), nil)

	data, err := os.ReadFile(h.c.statusPath)
	if err != nil {
		t.Fatal(err)
	}
	var st Status
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	if st.Schema != StatusSchema || st.Version != "0.2.0" || st.UpdatedAt != testStart || st.LastError != "" ||
		len(st.ScaleSets) != 1 {
		t.Fatalf("status = %+v", st)
	}
	ss := st.ScaleSets[0]
	if ss.Name != testScaleSet || ss.ID != testScaleSetID || !ss.SessionOpen || ss.Desired == nil || *ss.Desired != 2 ||
		ss.Workers != 2 || ss.Ready != 2 || ss.Stats == nil || ss.Stats.AssignedJobs != 2 {
		t.Errorf("scale set = %+v", ss)
	}
	if st, _ := os.Stat(h.c.statusPath); st.Mode().Perm() != 0o644 {
		t.Errorf("mode = %v", st.Mode())
	}

	// A failed pass and a closed session show up.
	s.SessionEnded(errors.New("GitHub is down"))
	h.c.writeStatus(context.Background(), errors.New("list VMs: timeout"))
	data, _ = os.ReadFile(h.c.statusPath)
	st = Status{}
	_ = json.Unmarshal(data, &st)
	if st.LastError != "list VMs: timeout" || st.ScaleSets[0].SessionOpen || st.ScaleSets[0].SessionError != "GitHub is down" {
		t.Errorf("status after failures = %+v", st)
	}
}

func TestNoStatusPathWritesNothing(t *testing.T) {
	h := newHarness(t, testConfig())
	h.c.writeStatus(context.Background(), nil) // must not panic or write anywhere
}
