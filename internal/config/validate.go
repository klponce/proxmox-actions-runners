package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// Limits enforced by validation. Names and other values Proxmox or GitHub check themselves are only required here.
const (
	minVMID = 100

	minCores       = 1
	minMemoryMiB   = 1024
	minFreeDiskGiB = 1

	minMaxLifetime = 10 * time.Minute
	// maxMaxLifetime is GitHub's job execution limit for self-hosted runners.
	maxMaxLifetime = 5 * 24 * time.Hour
)

// Scale set names become runs-on labels and Proxmox tags, so they are limited to characters both accept.
var scaleSetNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)

// ValidationError lists every problem found in a config, each prefixed with the field's path.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid config:\n  - " + strings.Join(e.Problems, "\n  - ")
}

type validator struct {
	problems []string
}

func (v *validator) addf(field, format string, args ...any) {
	v.problems = append(v.problems, field+": "+fmt.Sprintf(format, args...))
}

func (v *validator) required(field, value string) bool {
	if value == "" {
		v.addf(field, "is required")
		return false
	}
	return true
}

func (v *validator) absPath(field, value string) {
	if v.required(field, value) && !filepath.IsAbs(value) {
		v.addf(field, "%q must be an absolute path", value)
	}
}

func (v *validator) atLeast(field string, value, lo int) {
	if value < lo {
		v.addf(field, "%d is less than %d", value, lo)
	}
}

// validate checks a config that already has its defaults applied.
func (c *Config) validate() error {
	v := &validator{}
	c.Proxmox.validate(v)
	c.GitHub.validate(v)
	c.Worker.validate(v, "worker")
	c.validateScaleSets(v)
	if len(v.problems) > 0 {
		return &ValidationError{Problems: v.problems}
	}
	return nil
}

func (p Proxmox) validate(v *validator) {
	if v.required("proxmox.url", p.URL) {
		u, err := url.Parse(p.URL)
		switch {
		case err != nil:
			v.addf("proxmox.url", "%q is not a valid URL", p.URL)
		case u.Scheme != "https" || u.Host == "":
			v.addf("proxmox.url", "%q must be an https:// URL", p.URL)
		case u.Path != proxmoxAPIPath || u.RawQuery != "" || u.Fragment != "" || u.User != nil:
			v.addf("proxmox.url", "%q must have the path %s and nothing after it", p.URL, proxmoxAPIPath)
		}
	}
	v.required("proxmox.tokenId", p.TokenID)
	v.absPath("proxmox.tokenSecretFile", p.TokenSecretFile)
	v.required("proxmox.node", p.Node)
	v.required("proxmox.pool", p.Pool)
	v.required("proxmox.storage", p.Storage)
	v.required("proxmox.vnet", p.VNet)

	r := p.VMIDRange
	v.atLeast("proxmox.vmidRange.start", r.Start, minVMID)
	if r.Start > r.End {
		v.addf("proxmox.vmidRange", "start %d is after end %d", r.Start, r.End)
	}
}

func (g GitHub) validate(v *validator) {
	if v.required("github.configUrl", g.ConfigURL) {
		validateGitHubConfigURL(v, g.ConfigURL)
	}
	// The App is optional here: the installer checks Proxmox before it creates the App. RequireGitHubApp checks
	// that it is complete.
	if g.App.InstallationID < 0 {
		v.addf("github.app.installationId", "must be positive")
	}
	if g.App.PrivateKeyFile != "" {
		v.absPath("github.app.privateKeyFile", g.App.PrivateKeyFile)
	}
}

// RequireGitHubApp returns an error unless the config names a GitHub App: its Client ID, installation ID, and
// private key file. Parse accepts a config without one, because the installer writes the config and checks Proxmox
// before it creates the App; only the commands that talk to GitHub as the App need it.
func (c *Config) RequireGitHubApp() error {
	a := c.GitHub.App
	var missing []string
	if a.ClientID == "" {
		missing = append(missing, "github.app.clientId")
	}
	if a.InstallationID == 0 {
		missing = append(missing, "github.app.installationId")
	}
	if a.PrivateKeyFile == "" {
		missing = append(missing, "github.app.privateKeyFile")
	}
	if len(missing) > 0 {
		return fmt.Errorf("the GitHub App isn't set up yet: missing %s", strings.Join(missing, ", "))
	}
	return nil
}

// validateGitHubConfigURL accepts https://github.com/<org> and https://github.com/<owner>/<repo>, the shapes
// IsRepository tells apart. GitHub Enterprise Server and enterprise-level scale sets are out of scope (README,
// "Limitations").
func validateGitHubConfigURL(v *validator, raw string) {
	u, err := url.Parse(raw)
	var parts []string
	if err == nil {
		parts = strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	}
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" ||
		u.User != nil || len(parts) > 2 || slices.Contains(parts, "") {
		v.addf("github.configUrl", "%q must be https://github.com/<org> or https://github.com/<owner>/<repo>", raw)
	}
}

func (w Worker) validate(v *validator, prefix string) {
	v.atLeast(prefix+".cores", w.Cores, minCores)
	v.atLeast(prefix+".memoryMiB", w.MemoryMiB, minMemoryMiB)
	v.atLeast(prefix+".freeDiskGiB", w.FreeDiskGiB, minFreeDiskGiB)
}

func (c *Config) validateScaleSets(v *validator) {
	if len(c.ScaleSets) == 0 {
		v.addf("scaleSets", "at least one scale set is required")
		return
	}

	names := map[string]int{}
	labels := map[string]string{}
	totalMaxRunners := 0
	for i, s := range c.ScaleSets {
		prefix := fmt.Sprintf("scaleSets[%d]", i)
		if v.required(prefix+".name", s.Name) && !scaleSetNamePattern.MatchString(s.Name) {
			v.addf(prefix+".name", "%q is not a valid scale set name (lowercase letters, digits, '.', '_', '-'; "+
				"at most 63 characters)", s.Name)
		}
		if j, ok := names[s.Name]; ok && s.Name != "" {
			v.addf(prefix+".name", "%q is already used by scaleSets[%d]", s.Name, j)
		} else {
			names[s.Name] = i
		}

		// GitHub matches labels case-insensitively, so two scale sets can't share one in any case.
		for k, l := range s.Labels {
			key := strings.ToLower(l)
			if owner, ok := labels[key]; ok {
				v.addf(fmt.Sprintf("%s.labels[%d]", prefix, k), "%q is already used by %s", l, owner)
			} else {
				labels[key] = prefix
			}
		}

		if c.GitHub.IsRepository() && s.RunnerGroup != DefaultRunnerGroup {
			v.addf(prefix+".runnerGroup", "repository scale sets must use the %q runner group", DefaultRunnerGroup)
		}

		if s.MinRunners < 0 {
			v.addf(prefix+".minRunners", "must not be negative")
		}
		if s.MaxRunners < 1 {
			v.addf(prefix+".maxRunners", "must be at least 1")
		}
		if s.MinRunners > s.MaxRunners {
			v.addf(prefix+".minRunners", "%d is more than maxRunners %d", s.MinRunners, s.MaxRunners)
		}
		totalMaxRunners += max(s.MaxRunners, 0)

		if s.MaxLifetime < minMaxLifetime || s.MaxLifetime > maxMaxLifetime {
			v.addf(prefix+".maxLifetime", "%s is out of range [%s, %s]", s.MaxLifetime, minMaxLifetime,
				maxMaxLifetime)
		}
		s.Worker.validate(v, prefix+".worker")
	}

	// Only check capacity for a range that is otherwise valid; a broken range was already reported.
	r := c.Proxmox.VMIDRange
	if need := totalMaxRunners + ReservedVMIDs; r.Start >= minVMID && r.Start <= r.End && r.Size() < need {
		v.addf("proxmox.vmidRange", "has %d IDs but needs at least %d: the sum of maxRunners (%d) plus %d reserved",
			r.Size(), need, totalMaxRunners, ReservedVMIDs)
	}
}
