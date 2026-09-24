package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRun(t *testing.T) {
	invalid := filepath.Join(t.TempDir(), "invalid.yaml")
	if err := os.WriteFile(invalid, []byte("scaleSets: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name       string
		args       []string
		want       outcome
		wantStdout string
		wantStderr string
	}{
		{"no arguments", nil, usageError, "", "usage:"},
		{"unknown command", []string{"frobnicate"}, usageError, "", `unknown command "frobnicate"`},
		{"help", []string{"help"}, succeeded, "usage:", ""},
		{"version", []string{"version"}, succeeded, "", ""},
		{"check without target", []string{"check"}, usageError, "", "usage:"},
		{"check unknown target", []string{"check", "github"}, usageError, "", "usage:"},
		{"check config extra argument", []string{"check", "config", "extra"}, usageError, "", "unexpected arguments"},
		{"check config bad flag", []string{"check", "config", "-nope"}, usageError, "", "flag provided but not defined"},
		{"check example config", []string{"check", "config", "-config", "../../deploy/config.example.yaml"}, succeeded,
			"OK, 1 scale set(s)", ""},
		{"check invalid config", []string{"check", "config", "-config", invalid}, failed, "", ""},
		{"check missing config", []string{"check", "config", "-config", invalid + ".missing"}, failed, "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr)
			if got := outcomeOf(err); got != tt.want {
				t.Fatalf("run error = %v, want outcome %s", err, tt.want)
			}
			if !strings.Contains(stdout.String(), tt.wantStdout) {
				t.Errorf("stdout = %q, want it to contain %q", stdout.String(), tt.wantStdout)
			}
			if !strings.Contains(stderr.String(), tt.wantStderr) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.wantStderr)
			}
		})
	}
}

// outcome classifies what run returned.
type outcome string

const (
	succeeded  outcome = "success"
	usageError outcome = "usage error"
	failed     outcome = "failure"
)

func outcomeOf(err error) outcome {
	switch {
	case err == nil:
		return succeeded
	case errors.Is(err, errUsage):
		return usageError
	default:
		return failed
	}
}
