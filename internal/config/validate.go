package config

import (
	"fmt"
	"net"
	"net/url"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Limits enforced by validation. The upper bounds mostly catch unit mistakes, such as memory given in bytes.
const (
	minVMID = 100
	maxVMID = 999_999_999

	minCores       = 1
	maxCores       = 512
	minMemoryMiB   = 1024
	maxMemoryMiB   = 4 * 1024 * 1024
	minFreeDiskGiB = 1
	maxFreeDiskGiB = 64 * 1024

	minMaxLifetime = 10 * time.Minute
	// maxMaxLifetime is GitHub's job execution limit for self-hosted runners.
	maxMaxLifetime = 5 * 24 * time.Hour

	maxLabelLength = 256
)

var (
	// Proxmox token IDs look like user@realm!tokenname.
	tokenIDPattern     = regexp.MustCompile(`^[^\s@!]+@[^\s@!]+![A-Za-z][A-Za-z0-9._-]*$`)
	fingerprintPattern = regexp.MustCompile(`^[0-9A-Fa-f]{2}(:[0-9A-Fa-f]{2}){31}$`)
	nodePattern        = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]*[A-Za-z0-9])?$`)
	poolPattern        = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	storagePattern     = regexp.MustCompile(`^[a-z][a-z0-9._-]*[a-z0-9]$`)
	vnetPattern        = regexp.MustCompile(`^[a-z][a-z0-9]{0,6}[a-z0-9]$`)

	githubOwnerPattern = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,37}[A-Za-z0-9])?$`)
	githubRepoPattern  = regexp.MustCompile(`^[A-Za-z0-9._-]{1,100}$`)

	// Scale set names become runs-on labels and Proxmox tags, so they are limited to characters both accept.
	scaleSetNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,62}$`)
	labelPattern        = regexp.MustCompile(`^[^\s,]+$`)
	runnerGroupPattern  = regexp.MustCompile(`^[^\s/?&#%][^/?&#%]{0,63}$`)
)

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

func (v *validator) match(field, value string, pattern *regexp.Regexp, want string) {
	if v.required(field, value) && !pattern.MatchString(value) {
		v.addf(field, "%q is not %s", value, want)
	}
}

func (v *validator) absPath(field, value string) {
	if v.required(field, value) && !filepath.IsAbs(value) {
		v.addf(field, "%q must be an absolute path", value)
	}
}

func (v *validator) intRange(field string, value, lo, hi int) {
	if value < lo || value > hi {
		v.addf(field, "%d is out of range [%d, %d]", value, lo, hi)
	}
}

// validate checks a config that already has its defaults applied.
func (c *Config) validate() error {
	v := &validator{}
	c.Proxmox.validate(v)
	c.GitHub.validate(v)
	c.Metrics.validate(v)
	c.Worker.validate(v, "worker", nil)
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
	v.match("proxmox.tokenId", p.TokenID, tokenIDPattern, "a token ID like user@realm!name")
	v.absPath("proxmox.tokenSecretFile", p.TokenSecretFile)
	if p.TLSFingerprint != "" && !fingerprintPattern.MatchString(p.TLSFingerprint) {
		v.addf("proxmox.tlsFingerprint", "must be a SHA-256 fingerprint: 32 hex bytes separated by colons")
	}
	v.match("proxmox.node", p.Node, nodePattern, "a valid node name")
	v.match("proxmox.pool", p.Pool, poolPattern, "a valid pool name")
	v.match("proxmox.storage", p.Storage, storagePattern, "a valid storage ID")
	v.match("proxmox.vnet", p.VNet, vnetPattern, "a valid SDN VNet ID (up to 8 lowercase letters and digits)")

	r := p.VMIDRange
	v.intRange("proxmox.vmidRange.start", r.Start, minVMID, maxVMID)
	v.intRange("proxmox.vmidRange.end", r.End, minVMID, maxVMID)
	if r.Start > r.End {
		v.addf("proxmox.vmidRange", "start %d is after end %d", r.Start, r.End)
	}
}

func (g GitHub) validate(v *validator) {
	if v.required("github.configUrl", g.ConfigURL) {
		validateGitHubConfigURL(v, g.ConfigURL)
	}
	if v.required("github.app.clientId", g.App.ClientID) && strings.ContainsAny(g.App.ClientID, " \t\r\n") {
		v.addf("github.app.clientId", "must not contain whitespace")
	}
	if g.App.InstallationID <= 0 {
		v.addf("github.app.installationId", "is required and must be positive")
	}
	v.absPath("github.app.privateKeyFile", g.App.PrivateKeyFile)
}

// validateGitHubConfigURL accepts https://github.com/<org> and https://github.com/<owner>/<repo>. GitHub Enterprise
// Server and enterprise-level scale sets are out of scope (README, "Limitations").
func validateGitHubConfigURL(v *validator, raw string) {
	const field = "github.configUrl"
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.RawQuery != "" || u.Fragment != "" ||
		u.User != nil {
		v.addf(field, "%q must be https://github.com/<org> or https://github.com/<owner>/<repo>", raw)
		return
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	switch {
	case len(parts) < 1 || len(parts) > 2 || parts[0] == "":
		v.addf(field, "%q must be https://github.com/<org> or https://github.com/<owner>/<repo>", raw)
	case parts[0] == "enterprises":
		v.addf(field, "enterprise-level scale sets aren't supported; use an organization or repository")
	case !githubOwnerPattern.MatchString(parts[0]):
		v.addf(field, "%q is not a valid GitHub organization or user name", parts[0])
	case len(parts) == 2 && !githubRepoPattern.MatchString(parts[1]):
		v.addf(field, "%q is not a valid GitHub repository name", parts[1])
	}
}

// validate requires a loopback address, because nginx is what publishes the endpoint on the LAN.
func (m Metrics) validate(v *validator) {
	const field = "metrics.listen"
	host, port, err := net.SplitHostPort(m.Listen)
	if err != nil {
		v.addf(field, "%q must be host:port", m.Listen)
		return
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		v.addf(field, "%q has an invalid port", m.Listen)
	}
	if ip := net.ParseIP(host); host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		v.addf(field, "%q must be a loopback address; nginx publishes the endpoint on the LAN", m.Listen)
	}
}

// validate checks w. If inherited is non-nil, fields equal to it came from the controller-wide worker and were
// already checked there, so they are skipped to avoid reporting the same problem once per scale set.
func (w Worker) validate(v *validator, prefix string, inherited *Worker) {
	if inherited == nil || w.Cores != inherited.Cores {
		v.intRange(prefix+".cores", w.Cores, minCores, maxCores)
	}
	if inherited == nil || w.MemoryMiB != inherited.MemoryMiB {
		v.intRange(prefix+".memoryMiB", w.MemoryMiB, minMemoryMiB, maxMemoryMiB)
	}
	if inherited == nil || w.FreeDiskGiB != inherited.FreeDiskGiB {
		v.intRange(prefix+".freeDiskGiB", w.FreeDiskGiB, minFreeDiskGiB, maxFreeDiskGiB)
	}
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
		v.match(prefix+".name", s.Name, scaleSetNamePattern,
			"a valid scale set name (lowercase letters, digits, '.', '_', '-'; at most 63 characters)")
		if j, ok := names[s.Name]; ok && s.Name != "" {
			v.addf(prefix+".name", "%q is already used by scaleSets[%d]", s.Name, j)
		} else {
			names[s.Name] = i
		}

		// GitHub matches labels case-insensitively, so two scale sets can't share one in any case.
		for k, l := range s.Labels {
			field := fmt.Sprintf("%s.labels[%d]", prefix, k)
			switch {
			case !labelPattern.MatchString(l):
				v.addf(field, "%q must be non-empty with no spaces or commas", l)
			case len(l) > maxLabelLength:
				v.addf(field, "is longer than %d characters", maxLabelLength)
			}
			key := strings.ToLower(l)
			if owner, ok := labels[key]; ok {
				v.addf(field, "%q is already used by %s", l, owner)
			} else {
				labels[key] = prefix
			}
		}

		switch {
		case !runnerGroupPattern.MatchString(s.RunnerGroup) || strings.TrimSpace(s.RunnerGroup) != s.RunnerGroup:
			v.addf(prefix+".runnerGroup", "%q is not a valid runner group name", s.RunnerGroup)
		case c.GitHub.IsRepository() && s.RunnerGroup != DefaultRunnerGroup:
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
		s.Worker.validate(v, prefix+".worker", &c.Worker)
	}

	// Only check capacity for a range that is otherwise valid; a broken range was already reported.
	r := c.Proxmox.VMIDRange
	rangeValid := r.Start >= minVMID && r.End <= maxVMID && r.Start <= r.End
	if need := totalMaxRunners + ReservedVMIDs; rangeValid && r.Size() < need {
		v.addf("proxmox.vmidRange", "has %d IDs but needs at least %d: the sum of maxRunners (%d) plus %d reserved",
			r.Size(), need, totalMaxRunners, ReservedVMIDs)
	}
}
