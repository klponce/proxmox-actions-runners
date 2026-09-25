package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// fakeScaleSets knows some scale sets by name and records what is deleted.
type fakeScaleSets struct {
	ids       map[string]int
	deleted   []int
	deleteErr error
}

func (f *fakeScaleSets) FindScaleSet(_ context.Context, spec github.ScaleSetSpec) (*github.ScaleSet, error) {
	id, ok := f.ids[spec.Name]
	if !ok {
		return nil, nil
	}
	return &github.ScaleSet{ID: id, Name: spec.Name}, nil
}

func (f *fakeScaleSets) DeleteScaleSet(_ context.Context, id int) error {
	f.deleted = append(f.deleted, id)
	return f.deleteErr
}

func writeScaleSetConfig(t *testing.T, app string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := `proxmox:
  url: https://127.0.0.1:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: /etc/pve-token
  tlsFingerprint: "` + strings.Repeat("AA:", 31) + `AA"
  node: pve1
  pool: par-runners
  storage: local-lvm
  vnet: parnet
  vmidRange: {start: 10000, end: 10999}
github:
  configUrl: https://github.com/my-org
` + app + `
scaleSets:
  - {name: small, maxRunners: 2}
  - {name: large, maxRunners: 1}
`
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func useFakeScaleSets(t *testing.T, f *fakeScaleSets) {
	t.Helper()
	old := newScaleSetClient
	t.Cleanup(func() { newScaleSetClient = old })
	newScaleSetClient = func(*config.Config) (scaleSetClient, error) { return f, nil }
}

func TestScaleSetDelete(t *testing.T) {
	f := &fakeScaleSets{ids: map[string]int{"small": 42}}
	useFakeScaleSets(t, f)
	path := writeScaleSetConfig(t,
		"  app: {clientId: Iv23liEXAMPLE0000000, installationId: 1, privateKeyFile: /etc/key.pem}")

	var stdout, stderr bytes.Buffer
	if err := run([]string{"github", "scaleset", "delete", "-config", path}, &stdout, &stderr); err != nil {
		t.Fatalf("run: %v\n%s", err, stderr.String())
	}
	for _, want := range []string{"scale set small: deleted (ID 42)", "scale set large: not registered"} {
		if !strings.Contains(stdout.String(), want) {
			t.Errorf("output doesn't contain %q:\n%s", want, stdout.String())
		}
	}
	if len(f.deleted) != 1 || f.deleted[0] != 42 {
		t.Errorf("deleted = %v, want [42]", f.deleted)
	}
}

func TestScaleSetDeleteFailures(t *testing.T) {
	withApp := "  app: {clientId: Iv23liEXAMPLE0000000, installationId: 1, privateKeyFile: /etc/key.pem}"
	tests := []struct {
		name    string
		app     string
		fake    *fakeScaleSets
		wantErr string
	}{
		// The real client builder, which requires the App.
		{"no App yet", "", nil, "the GitHub App isn't set up yet"},
		{"GitHub refuses", withApp, &fakeScaleSets{ids: map[string]int{"small": 42},
			deleteErr: errors.New("403 Forbidden")}, "scale set small: 403 Forbidden"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.fake != nil {
				useFakeScaleSets(t, tt.fake)
			}
			var stdout, stderr bytes.Buffer
			err := run([]string{"github", "scaleset", "delete", "-config", writeScaleSetConfig(t, tt.app)},
				&stdout, &stderr)
			if outcomeOf(err) != failed || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want one containing %q", err, tt.wantErr)
			}
		})
	}
}
