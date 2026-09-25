package config

import (
	"os"
	"os/exec"
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

// TestInstallerConfigParses renders the config the way install/install.sh does at each stage of an install and parses
// it: before the App exists, once its key is in the controller, and once it is installed. A config the installer
// writes but parcon can't read stops the install only on a real node.
func TestInstallerConfigParses(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("needs bash")
	}
	// The functions that read the node are replaced; everything else is install.sh's own code.
	const stubs = `
pve_address() { echo 192.0.2.5; }
tls_config() { printf '  caCertFile: %s\n  tlsServerName: pve1.example.com\n' "$CA_FILE"; }
node_name() { echo pve1; }
linked_clones_supported() { return 0; }
apply_defaults
`
	tests := []struct {
		name    string
		render  string
		wantApp bool
	}{
		{"before the App", "render_config", false},
		{"with the App's Client ID", "render_config Iv23liEXAMPLE0000000", false},
		{"with the installed App", "PAR_GITHUB_URL=https://github.com/my-org render_config Iv23liEXAMPLE0000000 7890123",
			true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := exec.CommandContext(t.Context(), "bash", "-c", "PAR_INSTALL_SOURCED=1 && source install/install.sh && set +eu"+
				stubs+tt.render)
			cmd.Dir = "../.."
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("render_config: %v\n%s", err, out)
			}
			c, err := Parse(out)
			if err != nil {
				t.Fatalf("Parse: %v\n%s", err, out)
			}
			if err := c.RequireGitHubApp(); (err == nil) != tt.wantApp {
				t.Errorf("RequireGitHubApp = %v, want an error: %v\n%s", err, !tt.wantApp, out)
			}
		})
	}
}
