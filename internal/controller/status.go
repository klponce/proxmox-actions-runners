package controller

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// StatusPath is where parcon run writes the controller's status in the controller VM. `parcon status` on the host
// reads it through the guest agent. It is a report, not state: the controller never reads it back (AGENTS.md,
// invariant 3), and it holds nothing secret.
const StatusPath = "/run/parcon/status.json"

// StatusSchema is the status file's format version.
const StatusSchema = 1

// Status is the controller's view of its scale sets, written after each reconcile pass.
type Status struct {
	Schema    int       `json:"schema"`
	Version   string    `json:"version"`
	StartedAt time.Time `json:"startedAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// LastError is the last pass's error, if it failed.
	LastError string           `json:"lastError,omitempty"`
	ScaleSets []ScaleSetStatus `json:"scaleSets"`
}

// ScaleSetStatus is one scale set's state.
type ScaleSetStatus struct {
	Name string `json:"name"`
	// ID is GitHub's ID for the scale set, once it is registered.
	ID int `json:"id"`
	// SessionOpen reports whether the message session is open, since SessionSince; SessionError is why it last
	// ended.
	SessionOpen  bool      `json:"sessionOpen"`
	SessionSince time.Time `json:"sessionSince,omitzero"`
	SessionError string    `json:"sessionError,omitempty"`
	// Desired is how many workers GitHub's last message calls for, once one has arrived.
	Desired *int `json:"desired,omitempty"`
	// Workers are the scale set's worker VMs, Ready of them with their runner registered, Busy of them in a job.
	Workers int `json:"workers"`
	Ready   int `json:"ready"`
	Busy    int `json:"busy"`
	// Stats are GitHub's counts from its last message.
	Stats *github.Stats `json:"stats,omitempty"`
}

// countWorkers records each scale set's workers from a pass's view.
func (c *Controller) countWorkers(v view) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.status.ScaleSets = c.status.ScaleSets[:0]
	for name, s := range c.scaleSets {
		ss := ScaleSetStatus{Name: name, ID: s.id}
		for _, w := range v.workers[name] {
			ss.Workers++
			if w.ready {
				ss.Ready++
			}
			if s.hasJob(w.name) {
				ss.Busy++
			}
		}
		c.status.ScaleSets = append(c.status.ScaleSets, ss)
	}
	slices.SortFunc(c.status.ScaleSets, func(a, b ScaleSetStatus) int {
		if a.Name < b.Name {
			return -1
		}
		return 1
	})
}

// writeStatus writes the status after a pass that ended with err, if a StatusPath is set. Failing to write it is
// logged, never fatal: it is only a report.
func (c *Controller) writeStatus(ctx context.Context, err error) {
	if c.statusPath == "" {
		return
	}
	c.mu.Lock()
	st := c.status
	st.ScaleSets = slices.Clone(c.status.ScaleSets)
	c.mu.Unlock()
	st.UpdatedAt = c.now()
	st.LastError = ""
	if err != nil {
		st.LastError = err.Error()
	}
	for i := range st.ScaleSets {
		s := c.scaleSets[st.ScaleSets[i].Name]
		s.mu.Lock()
		ss := &st.ScaleSets[i]
		ss.SessionOpen, ss.SessionSince, ss.SessionError = s.sessionOpen, s.sessionSince, s.sessionErr
		if s.known {
			desired := s.desired
			ss.Desired = &desired
		}
		ss.Stats = s.stats
		s.mu.Unlock()
	}
	data, merr := json.MarshalIndent(st, "", "  ")
	if merr == nil {
		merr = writeFileAtomic(c.statusPath, append(data, '\n'))
	}
	if merr != nil {
		c.logger.WarnContext(ctx, "writing the status failed", slog.String("path", c.statusPath),
			slog.String("error", merr.Error()))
	}
}

func writeFileAtomic(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".status.*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(f.Name()) }()
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Chmod(0o644); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}
