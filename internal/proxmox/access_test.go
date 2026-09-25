package proxmox

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// TestInstallerGrantsRequiredPrivileges keeps install/install.sh's role in step with what the controller needs: the
// installer can't call Go, so it has its own copy of the list.
func TestInstallerGrantsRequiredPrivileges(t *testing.T) {
	script, err := os.ReadFile("../../install/install.sh")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?s)\nreadonly PRIVILEGES=\((.*?)\n\)`).FindSubmatch(script)
	if m == nil {
		t.Fatal("install.sh has no PRIVILEGES array")
	}
	granted := strings.Fields(string(m[1]))
	slices.Sort(granted)

	var want []string
	for _, req := range RequiredPrivileges("pool", "storage", "zone", "vnet") {
		want = append(want, req.Privileges...)
	}
	slices.Sort(want)
	want = slices.Compact(want)

	if !slices.Equal(granted, want) {
		t.Errorf("install.sh grants %v, the controller needs %v", granted, want)
	}
}
