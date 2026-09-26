// Package release finds, downloads, and verifies this project's releases, which `parcon update` installs.
//
// A release's SHA256SUMS lists every asset's checksum, and SHA256SUMS.sig is an Ed25519 signature of it made by the
// release workflow. parcon carries the public keys it trusts (keys/*.pem) and refuses a release whose signature none
// of them verifies, then checks every asset it downloads against SHA256SUMS.
package release

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Repo is the project's GitHub repository.
const Repo = "klponce/proxmox-actions-runners"

// Version is a release version: major.minor.patch with an optional pre-release suffix, such as 0.2.0-rc.1.
type Version struct {
	Major, Minor, Patch int
	Pre                 string
}

// Versions end up in file names, URLs, and Proxmox tags, which allow only lowercase letters, digits, and . -
var versionPattern = regexp.MustCompile(`^v?(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([0-9a-z.]+))?$`)

// ParseVersion parses a version, with or without a leading v.
func ParseVersion(s string) (Version, error) {
	m := versionPattern.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%q isn't a release version such as 0.2.0 or 0.2.0-rc.1", s)
	}
	var v Version
	for i, p := range []*int{&v.Major, &v.Minor, &v.Patch} {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return Version{}, fmt.Errorf("%q: %w", s, err)
		}
		*p = n
	}
	v.Pre = m[4]
	return v, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}

// Tag is the release's git tag.
func (v Version) Tag() string { return "v" + v.String() }

// IsPre reports whether v is a pre-release.
func (v Version) IsPre() bool { return v.Pre != "" }

// Compare returns -1, 0, or 1 as v is older than, the same as, or newer than w, by semantic versioning's rules: a
// pre-release is older than its release, and pre-release identifiers compare numerically when both are numbers.
func (v Version) Compare(w Version) int {
	for _, d := range [][2]int{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if d[0] != d[1] {
			return cmp(d[0], d[1])
		}
	}
	switch {
	case v.Pre == w.Pre:
		return 0
	case v.Pre == "":
		return 1
	case w.Pre == "":
		return -1
	}
	a, b := strings.Split(v.Pre, "."), strings.Split(w.Pre, ".")
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] == b[i] {
			continue
		}
		an, aerr := strconv.Atoi(a[i])
		bn, berr := strconv.Atoi(b[i])
		switch {
		case aerr == nil && berr == nil:
			return cmp(an, bn)
		case aerr == nil:
			return -1
		case berr == nil:
			return 1
		default:
			return strings.Compare(a[i], b[i])
		}
	}
	return cmp(len(a), len(b))
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
