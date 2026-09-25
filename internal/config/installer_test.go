package config

import (
	"os"
	"regexp"
	"testing"
)

// TestInstallerScaleSetPattern keeps install/install.sh's check of the scale set name in step with the config's: a
// name the installer accepts but the config rejects fails the install only after it has changed the host.
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
