package config

import (
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.yaml.in/yaml/v3"
)

// validDoc returns the smallest valid config as a generic document that tests can modify.
func validDoc() map[string]any {
	return map[string]any{
		"proxmox": map[string]any{
			"url":             "https://pve.example.com:8006/api2/json",
			"tokenId":         "par@pve!controller",
			"tokenSecretFile": "/etc/proxmox-actions-runners/pve-token",
			"node":            "pve1",
			"pool":            "par-runners",
			"storage":         "local-lvm",
			"vnet":            "parnet",
			"vmidRange":       map[string]any{"start": 10000, "end": 10999},
		},
		"github": map[string]any{
			"configUrl": "https://github.com/my-org",
			"app": map[string]any{
				"clientId":       "Iv23liEXAMPLE0000000",
				"installationId": 7890123,
				"privateKeyFile": "/etc/proxmox-actions-runners/github-app.pem",
			},
		},
		"scaleSets": []any{
			map[string]any{"name": "proxmox-ubuntu-26.04", "maxRunners": 3},
		},
	}
}

// set changes the value at a dotted path such as "scaleSets.0.maxRunners". A nil value deletes the key.
func set(t *testing.T, doc map[string]any, path string, value any) {
	t.Helper()
	keys := strings.Split(path, ".")
	var cur any = doc
	for i, k := range keys {
		last := i == len(keys)-1
		switch node := cur.(type) {
		case map[string]any:
			if last {
				if value == nil {
					delete(node, k)
				} else {
					node[k] = value
				}
				return
			}
			if _, ok := node[k]; !ok {
				node[k] = map[string]any{}
			}
			cur = node[k]
		case []any:
			n, err := strconv.Atoi(k)
			if err != nil || n >= len(node) {
				t.Fatalf("bad index %q in path %q", k, path)
			}
			if last {
				node[n] = value
				return
			}
			cur = node[n]
		default:
			t.Fatalf("path %q goes through a non-container at %q", path, k)
		}
	}
}

func parseDoc(t *testing.T, doc map[string]any) (*Config, error) {
	t.Helper()
	data, err := yaml.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return Parse(data)
}

func TestLoadExample(t *testing.T) {
	c, err := Load("../../deploy/config.example.yaml")
	if err != nil {
		t.Fatalf("Load example: %v", err)
	}
	if got, want := len(c.ScaleSets), 1; got != want {
		t.Fatalf("scale sets = %d, want %d", got, want)
	}
	s := c.ScaleSets[0]
	if want := (Worker{Cores: 4, MemoryMiB: 16384, FreeDiskGiB: 50}); s.Worker != want {
		t.Errorf("scale set worker = %+v, want %+v", s.Worker, want)
	}
	if want := (Worker{Cores: 2, MemoryMiB: DefaultMemoryMiB, FreeDiskGiB: DefaultFreeDiskGiB}); c.Worker != want {
		t.Errorf("controller worker = %+v, want %+v", c.Worker, want)
	}
	if s.MaxLifetime != 6*time.Hour {
		t.Errorf("maxLifetime = %s, want 6h", s.MaxLifetime)
	}
}

func TestLoadMissingFile(t *testing.T) {
	if _, err := Load(t.TempDir() + "/missing.yaml"); err == nil {
		t.Fatal("Load of a missing file succeeded")
	}
}

func TestParseDefaults(t *testing.T) {
	c, err := parseDoc(t, validDoc())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if c.Worker != DefaultWorker() {
		t.Errorf("controller worker = %+v, want defaults %+v", c.Worker, DefaultWorker())
	}
	s := c.ScaleSets[0]
	if s.Worker != DefaultWorker() {
		t.Errorf("scale set worker = %+v, want defaults %+v", s.Worker, DefaultWorker())
	}
	if s.MaxLifetime != DefaultMaxLifetime {
		t.Errorf("maxLifetime = %s, want %s", s.MaxLifetime, DefaultMaxLifetime)
	}
	if len(s.Labels) != 1 || s.Labels[0] != s.Name {
		t.Errorf("labels = %v, want [%s]", s.Labels, s.Name)
	}
	if s.MinRunners != 0 {
		t.Errorf("minRunners = %d, want 0", s.MinRunners)
	}
	if s.RunnerGroup != DefaultRunnerGroup {
		t.Errorf("runnerGroup = %q, want %q", s.RunnerGroup, DefaultRunnerGroup)
	}
	if c.GitHub.IsRepository() {
		t.Error("an organization URL is reported as a repository")
	}
	if c.Proxmox.Zone != DefaultZone {
		t.Errorf("proxmox.zone = %q, want %q", c.Proxmox.Zone, DefaultZone)
	}
	if !c.Proxmox.UseLinkedClone() {
		t.Error("linked clones are off by default")
	}
}

func TestParseNormalizes(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		value any
		check func(*Config) (got, want any)
	}{
		{
			name:  "proxmox URL without a path gets the API path",
			path:  "proxmox.url",
			value: "https://pve.example.com:8006",
			check: func(c *Config) (any, any) { return c.Proxmox.URL, "https://pve.example.com:8006/api2/json" },
		},
		{
			name:  "proxmox URL with a bare slash gets the API path",
			path:  "proxmox.url",
			value: "https://pve.example.com:8006/",
			check: func(c *Config) (any, any) { return c.Proxmox.URL, "https://pve.example.com:8006/api2/json" },
		},
		{
			name:  "trailing slash is removed from the GitHub URL",
			path:  "github.configUrl",
			value: "https://github.com/my-org/my-repo/",
			check: func(c *Config) (any, any) { return c.GitHub.ConfigURL, "https://github.com/my-org/my-repo" },
		},
		{
			name:  "linked clones can be turned off",
			path:  "proxmox.linkedClone",
			value: false,
			check: func(c *Config) (any, any) { return c.Proxmox.UseLinkedClone(), false },
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := validDoc()
			set(t, doc, tt.path, tt.value)
			c, err := parseDoc(t, doc)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}
			if got, want := tt.check(c); got != want {
				t.Errorf("got %v, want %v", got, want)
			}
		})
	}
}

func TestParseWorkerInheritance(t *testing.T) {
	doc := validDoc()
	set(t, doc, "worker", map[string]any{"cores": 8, "memoryMiB": 32768})
	set(t, doc, "scaleSets", []any{
		map[string]any{"name": "inherits", "maxRunners": 1},
		map[string]any{"name": "overrides", "maxRunners": 1, "worker": map[string]any{"memoryMiB": 4096}},
	})
	c, err := parseDoc(t, doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	tests := []struct {
		name string
		got  Worker
		want Worker
	}{
		{"controller", c.Worker, Worker{Cores: 8, MemoryMiB: 32768, FreeDiskGiB: DefaultFreeDiskGiB}},
		{"inherits", c.ScaleSets[0].Worker, Worker{Cores: 8, MemoryMiB: 32768, FreeDiskGiB: DefaultFreeDiskGiB}},
		{"overrides", c.ScaleSets[1].Worker, Worker{Cores: 8, MemoryMiB: 4096, FreeDiskGiB: DefaultFreeDiskGiB}},
	}
	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s worker = %+v, want %+v", tt.name, tt.got, tt.want)
		}
	}
}

func TestParseValid(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		value any
	}{
		{"repository scope", "github.configUrl", "https://github.com/my-org/my.repo_1"},
		{"no TLS fingerprint", "proxmox.tlsFingerprint", nil},
		{"warm pool", "scaleSets.0.minRunners", 3},
		{"several labels", "scaleSets.0.labels", []any{"proxmox", "ubuntu-26.04", "x64"}},
		{"longest lifetime", "scaleSets.0.maxLifetime", "120h"},
		{"exactly enough VMIDs", "proxmox.vmidRange", map[string]any{"start": 100, "end": 106}},
		{"organization runner group", "scaleSets.0.runnerGroup", "Trusted builds"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := validDoc()
			set(t, doc, tt.path, tt.value)
			if _, err := parseDoc(t, doc); err != nil {
				t.Fatalf("Parse: %v", err)
			}
		})
	}
}

func TestParseInvalid(t *testing.T) {
	tests := []struct {
		name  string
		path  string
		value any
		want  string
	}{
		{"missing proxmox URL", "proxmox.url", nil, "proxmox.url: is required"},
		{"plain HTTP proxmox URL", "proxmox.url", "http://pve.example.com:8006/api2/json", "must be an https:// URL"},
		{"wrong proxmox path", "proxmox.url", "https://pve.example.com:8006/api", "must have the path /api2/json"},
		{"proxmox URL with query", "proxmox.url", "https://pve.example.com/api2/json?x=1", "must have the path"},
		{"relative token secret file", "proxmox.tokenSecretFile", "pve-token", "must be an absolute path"},
		{"missing token ID", "proxmox.tokenId", nil, "proxmox.tokenId: is required"},
		{"missing node", "proxmox.node", nil, "proxmox.node: is required"},
		{"missing VNet", "proxmox.vnet", nil, "proxmox.vnet: is required"},
		{"VMID below 100", "proxmox.vmidRange.start", 99, "proxmox.vmidRange.start"},
		{"VMID range backwards", "proxmox.vmidRange", map[string]any{"start": 2000, "end": 1000}, "is after end"},
		{"VMID range too small", "proxmox.vmidRange", map[string]any{"start": 100, "end": 105}, "needs at least 7"},

		{"missing GitHub URL", "github.configUrl", nil, "github.configUrl: is required"},
		{"GitHub Enterprise Server", "github.configUrl", "https://ghes.example.com/my-org", "https://github.com/<org>"},
		{"too deep GitHub URL", "github.configUrl", "https://github.com/a/b/c", "https://github.com/<org>"},
		{"GitHub URL without owner", "github.configUrl", "https://github.com/", "https://github.com/<org>"},
		{"negative installation ID", "github.app.installationId", -1, "github.app.installationId: must be positive"},
		{"relative private key", "github.app.privateKeyFile", "key.pem", "must be an absolute path"},

		{"controller memory in GiB by mistake", "worker.memoryMiB", 8, "worker.memoryMiB"},
		{"negative cores", "worker.cores", -1, "worker.cores"},
		{"negative scale set disk", "scaleSets.0.worker", map[string]any{"freeDiskGiB": -1}, "scaleSets[0].worker.freeDiskGiB"},

		{"no scale sets", "scaleSets", []any{}, "at least one scale set is required"},
		{"uppercase name", "scaleSets.0.name", "Proxmox", "scaleSets[0].name"},
		{"missing name", "scaleSets.0.name", nil, "scaleSets[0].name: is required"},
		{"duplicate label in any case", "scaleSets.0.labels", []any{"x64", "X64"}, "already used"},
		{"negative minRunners", "scaleSets.0.minRunners", -1, "must not be negative"},
		{"zero maxRunners", "scaleSets.0.maxRunners", 0, "must be at least 1"},
		{"minRunners above maxRunners", "scaleSets.0.minRunners", 4, "is more than maxRunners"},
		{"lifetime too short", "scaleSets.0.maxLifetime", "1m", "scaleSets[0].maxLifetime"},
		{"lifetime too long", "scaleSets.0.maxLifetime", "121h", "scaleSets[0].maxLifetime"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := validDoc()
			set(t, doc, tt.path, tt.value)
			_, err := parseDoc(t, doc)
			var verr *ValidationError
			if !errors.As(err, &verr) {
				t.Fatalf("Parse error = %v, want a *ValidationError", err)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error does not mention %q:\n%v", tt.want, err)
			}
		})
	}
}

func TestRequireGitHubApp(t *testing.T) {
	c, err := parseDoc(t, validDoc())
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if err := c.RequireGitHubApp(); err != nil {
		t.Errorf("RequireGitHubApp with an App = %v", err)
	}

	// Before the installer creates the App, the config has no App at all.
	doc := validDoc()
	set(t, doc, "github.app", nil)
	c, err = parseDoc(t, doc)
	if err != nil {
		t.Fatalf("Parse without an App: %v", err)
	}
	want := "the GitHub App isn't set up yet: missing github.app.clientId, github.app.installationId, " +
		"github.app.privateKeyFile"
	if err := c.RequireGitHubApp(); err == nil || err.Error() != want {
		t.Errorf("RequireGitHubApp without an App = %v, want %q", err, want)
	}

	doc = validDoc()
	set(t, doc, "github.app.installationId", nil)
	c, err = parseDoc(t, doc)
	if err != nil {
		t.Fatalf("Parse without an installation: %v", err)
	}
	if err := c.RequireGitHubApp(); err == nil || !strings.HasSuffix(err.Error(), "missing github.app.installationId") {
		t.Errorf("RequireGitHubApp without an installation = %v", err)
	}
}

func TestParseRepositoryRunnerGroup(t *testing.T) {
	doc := validDoc()
	set(t, doc, "github.configUrl", "https://github.com/my-org/my-repo")
	c, err := parseDoc(t, doc)
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !c.GitHub.IsRepository() {
		t.Error("a repository URL isn't reported as a repository")
	}

	set(t, doc, "scaleSets.0.runnerGroup", "builds")
	_, err = parseDoc(t, doc)
	want := `scaleSets[0].runnerGroup: repository scale sets must use the "default" runner group`
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("Parse error = %v, want it to mention %q", err, want)
	}
}

func TestParseDuplicateScaleSets(t *testing.T) {
	doc := validDoc()
	set(t, doc, "scaleSets", []any{
		map[string]any{"name": "a", "maxRunners": 1, "labels": []any{"shared"}},
		map[string]any{"name": "a", "maxRunners": 1, "labels": []any{"Shared"}},
	})
	_, err := parseDoc(t, doc)
	if err == nil {
		t.Fatal("Parse succeeded")
	}
	for _, want := range []string{
		`scaleSets[1].name: "a" is already used by scaleSets[0]`,
		`scaleSets[1].labels[0]: "Shared" is already used by scaleSets[0]`,
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %q:\n%v", want, err)
		}
	}
}

func TestParseMissingVMIDRangeReportedOnce(t *testing.T) {
	doc := validDoc()
	set(t, doc, "proxmox.vmidRange", nil)
	set(t, doc, "proxmox.url", "http://pve.example.com")
	_, err := parseDoc(t, doc)
	var verr *ValidationError
	if !errors.As(err, &verr) {
		t.Fatalf("Parse error = %v, want a *ValidationError", err)
	}
	want := []string{
		`proxmox.url: "http://pve.example.com" must be an https:// URL`,
		"proxmox.vmidRange.start: 0 is less than 100",
	}
	if strings.Join(verr.Problems, "\n") != strings.Join(want, "\n") {
		t.Errorf("problems =\n%s\nwant\n%s", strings.Join(verr.Problems, "\n"), strings.Join(want, "\n"))
	}
}

func TestVMIDRangeWorkers(t *testing.T) {
	tests := []struct {
		r, workers VMIDRange
	}{
		{VMIDRange{10000, 10999}, VMIDRange{10000, 10995}},
		// The smallest range validation allows for one runner.
		{VMIDRange{100, 104}, VMIDRange{100, 100}},
	}
	for _, tt := range tests {
		if got := tt.r.Workers(); got != tt.workers {
			t.Errorf("%+v.Workers() = %+v, want %+v", tt.r, got, tt.workers)
		}
		if tt.r.Size()-tt.r.Workers().Size() != ReservedVMIDs {
			t.Errorf("%+v: %d IDs reserved, want %d", tt.r, tt.r.Size()-tt.r.Workers().Size(), ReservedVMIDs)
		}
	}
}

func TestParseDecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		data string
		want string
	}{
		{"empty", "", "config is empty"},
		{"unknown field", "proxmox:\n  nodes: pve1\n", "field nodes not found"},
		{"two documents", "worker: {}\n---\nworker: {}\n", "exactly one YAML document"},
		{"duration without unit", "scaleSets:\n  - maxLifetime: 60\n", "decode"},
		{"wrong type", "scaleSets: yes\n", "decode"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.data))
			if err == nil {
				t.Fatal("Parse succeeded")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %v, want it to mention %q", err, tt.want)
			}
		})
	}
}
