package github

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/actions/scaleset"
)

// defaultRunnerGroupID is the ID GitHub gives every default runner group. actions/scaleset's own example and ARC
// rely on it, and repository scale sets can't look runner groups up by name.
const defaultRunnerGroupID = 1

// ScaleSetSpec is the scale set the controller wants GitHub to have. The config fills in every field (see
// internal/config for the defaults).
type ScaleSetSpec struct {
	Name string
	// Labels are what workflows put in runs-on.
	Labels []string
	// RunnerGroup is the runner group's name.
	RunnerGroup string
}

// ScaleSet is a runner scale set registered with GitHub.
type ScaleSet struct {
	ID            int
	Name          string
	RunnerGroupID int
	Labels        []string
}

func fromScaleSet(s *scaleset.RunnerScaleSet) ScaleSet {
	labels := make([]string, len(s.Labels))
	for i, l := range s.Labels {
		labels[i] = l.Name
	}
	return ScaleSet{ID: s.ID, Name: s.Name, RunnerGroupID: s.RunnerGroupID, Labels: labels}
}

// runnerGroupID resolves a runner group name to its ID.
func (c *Client) runnerGroupID(ctx context.Context, name string) (int, error) {
	if name == scaleset.DefaultRunnerGroup {
		return defaultRunnerGroupID, nil
	}
	group, err := c.ss.GetRunnerGroupByName(ctx, name)
	if err != nil {
		return 0, fmt.Errorf("runner group %q: %w", name, err)
	}
	return group.ID, nil
}

// FindScaleSet returns the scale set named spec.Name in spec's runner group, or nil if there is none.
func (c *Client) FindScaleSet(ctx context.Context, spec ScaleSetSpec) (*ScaleSet, error) {
	groupID, err := c.runnerGroupID(ctx, spec.RunnerGroup)
	if err != nil {
		return nil, err
	}
	existing, err := c.ss.GetRunnerScaleSet(ctx, groupID, spec.Name)
	if err != nil {
		return nil, fmt.Errorf("find scale set %q: %w", spec.Name, err)
	}
	if existing == nil {
		return nil, nil
	}
	s := fromScaleSet(existing)
	return &s, nil
}

// EnsureScaleSet makes GitHub's scale set match spec: it creates the scale set if it doesn't exist and updates its
// labels and settings if they differ. Runner self-updates are always disabled, because the template pins the runner
// version and is rebuilt for each runner release.
func (c *Client) EnsureScaleSet(ctx context.Context, spec ScaleSetSpec) (ScaleSet, error) {
	groupID, err := c.runnerGroupID(ctx, spec.RunnerGroup)
	if err != nil {
		return ScaleSet{}, err
	}
	want := &scaleset.RunnerScaleSet{
		Name:          spec.Name,
		RunnerGroupID: groupID,
		Labels:        toLabels(spec.Labels),
		RunnerSetting: scaleset.RunnerSetting{DisableUpdate: true},
	}

	existing, err := c.ss.GetRunnerScaleSet(ctx, groupID, spec.Name)
	if err != nil {
		return ScaleSet{}, fmt.Errorf("find scale set %q: %w", spec.Name, err)
	}
	if existing == nil {
		created, err := c.ss.CreateRunnerScaleSet(ctx, want)
		if err != nil {
			return ScaleSet{}, fmt.Errorf("create scale set %q: %w", spec.Name, err)
		}
		c.logger.InfoContext(ctx, "created scale set", slog.String("scaleSet", spec.Name), slog.Int("scaleSetId", created.ID))
		return fromScaleSet(created), nil
	}

	if sameLabels(existing.Labels, want.Labels) && existing.RunnerSetting.DisableUpdate {
		return fromScaleSet(existing), nil
	}
	updated, err := c.ss.UpdateRunnerScaleSet(ctx, existing.ID, want)
	if err != nil {
		return ScaleSet{}, fmt.Errorf("update scale set %q: %w", spec.Name, err)
	}
	c.logger.InfoContext(ctx, "updated scale set", slog.String("scaleSet", spec.Name), slog.Int("scaleSetId", updated.ID))
	return fromScaleSet(updated), nil
}

// DeleteScaleSet removes a scale set from GitHub. Its runners are unregistered with it.
func (c *Client) DeleteScaleSet(ctx context.Context, id int) error {
	if err := c.ss.DeleteRunnerScaleSet(ctx, id); err != nil {
		return fmt.Errorf("delete scale set %d: %w", id, err)
	}
	return nil
}

func toLabels(names []string) []scaleset.Label {
	labels := make([]scaleset.Label, len(names))
	for i, n := range names {
		labels[i] = scaleset.Label{Name: n}
	}
	return labels
}

// sameLabels compares label names as sets, ignoring case, the way GitHub matches runs-on.
func sameLabels(a, b []scaleset.Label) bool {
	norm := func(labels []scaleset.Label) []string {
		out := make([]string, len(labels))
		for i, l := range labels {
			out[i] = strings.ToLower(l.Name)
		}
		slices.Sort(out)
		return slices.Compact(out)
	}
	return slices.Equal(norm(a), norm(b))
}
