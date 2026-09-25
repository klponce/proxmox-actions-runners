package config

import (
	"os"
	"regexp"
	"slices"
	"testing"
)

// TestInstallerScaleSetPattern keeps install/install.sh's check of the scale set name in step with the config's: a
// name the installer accepts but the config rejects fails the install only after it has changed the host.
// TestQuotedLabelsStayStrings covers how install.sh writes labels: quoted, because unquoted YAML turns null into no
// label at all, and the scale set then falls back to its name.
func TestQuotedLabelsStayStrings(t *testing.T) {
	c, err := Parse([]byte(`proxmox:
  url: https://192.0.2.1:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: /etc/proxmox-actions-runners/pve-token
  node: pve1
  pool: par-runners
  storage: local-lvm
  vnet: parnet
  vmidRange: { start: 10000, end: 10099 }
github:
  configUrl: https://github.com/my-org
scaleSets:
  - name: "ubuntu"
    labels: ["null", "true", "1.5"]
    maxRunners: 2
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got, want := c.ScaleSets[0].Labels, []string{"null", "true", "1.5"}; !slices.Equal(got, want) {
		t.Errorf("labels = %q, want %q", got, want)
	}
}

func TestInstallerScaleSetPattern(t *testing.T) {
	script, err := os.ReadFile("../../install/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`\nreadonly SCALE_SET_PATTERN='([^']*)'`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh has no SCALE_SET_PATTERN")
	}
	if got, want := string(m[1]), scaleSetNamePattern.String(); got != want {
		t.Errorf("install.sh checks scale set names with %s, the config with %s", got, want)
	}
}
