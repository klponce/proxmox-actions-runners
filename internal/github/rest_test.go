package github

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestLatestRunnerRelease(t *testing.T) {
	published := time.Date(2026, 8, 26, 14, 33, 29, 0, time.UTC)
	tests := []struct {
		name    string
		release map[string]any
		want    RunnerRelease
		wantErr string
	}{
		{"found", map[string]any{"tag_name": "v2.338.0", "published_at": published},
			RunnerRelease{Version: "2.338.0", PublishedAt: published}, ""},
		{"odd tag", map[string]any{"tag_name": "nightly", "published_at": published}, RunnerRelease{},
			`unexpected tag "nightly"`},
		{"no release", nil, RunnerRelease{}, "404"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakeGitHub(t)
			f.runnerRelease = tt.release
			got, err := f.client().LatestRunnerRelease(context.Background())
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("LatestRunnerRelease error = %v, want it to mention %q", err, tt.wantErr)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("LatestRunnerRelease = %+v, %v; want %+v", got, err, tt.want)
			}
		})
	}
}

func TestRunnerReleaseNewerThan(t *testing.T) {
	r := RunnerRelease{Version: "2.338.0"}
	tests := []struct {
		have string
		want bool
	}{
		{"2.338.0", false},
		{"2.337.9", true},
		{"2.339.0", false},
		{"1.999.999", true},
		{"3.0.0", false},
		{"", true},
		{"2.338", true},
		{"v2.338.0", true},
	}
	for _, tt := range tests {
		if got := r.NewerThan(tt.have); got != tt.want {
			t.Errorf("%s.NewerThan(%q) = %v, want %v", r.Version, tt.have, got, tt.want)
		}
	}
}

func TestRunnerReleaseStaleness(t *testing.T) {
	released := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	r := RunnerRelease{Version: "2.338.0", PublishedAt: released}
	day := 24 * time.Hour
	tests := []struct {
		have string
		age  time.Duration
		want Staleness
	}{
		{"2.338.0", 40 * day, Current},
		{"2.339.0", 40 * day, Current},
		{"2.337.0", 7*day - time.Second, Behind},
		{"2.337.0", 7 * day, BehindWarn},
		{"2.337.0", 21*day - time.Second, BehindWarn},
		{"2.337.0", 21 * day, BehindError},
		{"", time.Hour, Behind},
	}
	for _, tt := range tests {
		if got := r.Staleness(tt.have, released.Add(tt.age)); got != tt.want {
			t.Errorf("Staleness(%q) after %s = %v, want %v", tt.have, tt.age, got, tt.want)
		}
	}
}
