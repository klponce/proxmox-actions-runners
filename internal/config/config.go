// Package config defines the controller's configuration file: its schema, defaults, and validation.
//
// parcon on the Proxmox host renders the file from its settings and writes it to
// /etc/proxmox-actions-runners/config.yaml in the controller VM. Secrets are never stored in it; it only names the
// files that hold them.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

// The controller VM's files (docs/install.md, "Controller VM contract"): the config, and the secrets it names. Only
// the parcon user can read them.
const (
	Dir = "/etc/proxmox-actions-runners"
	// DefaultPath is the config file.
	DefaultPath = Dir + "/config.yaml"
	// TokenSecretFile holds the Proxmox API token's secret.
	TokenSecretFile = Dir + "/pve-token"
	// AppKeyFile holds the GitHub App's private key.
	AppKeyFile = Dir + "/github-app.pem"
	// CACertFile is a copy of the node's CA, when the API serves the node's own certificate.
	CACertFile = Dir + "/pve-ca.pem"
)

// Config is the controller's whole configuration.
type Config struct {
	Proxmox Proxmox `yaml:"proxmox"`
	GitHub  GitHub  `yaml:"github,omitempty"`
	// Worker is the controller-wide worker hardware. Fields left unset use the GitHub-matching defaults.
	Worker    Worker     `yaml:"worker,omitempty"`
	ScaleSets []ScaleSet `yaml:"scaleSets"`
}

// Proxmox says how to reach the Proxmox VE API and where workers are created.
type Proxmox struct {
	// URL is the API base URL, for example https://pve.example.com:8006/api2/json. A URL without a path gets
	// /api2/json.
	URL string `yaml:"url"`
	// TokenID is the API token ID, for example par@pve!controller.
	TokenID string `yaml:"tokenId"`
	// TokenSecretFile is the absolute path of the file that holds the token secret.
	TokenSecretFile string `yaml:"tokenSecretFile"`
	// CACertFile is the absolute path of a PEM file with the CA certificates that verify the API's certificate, such
	// as a copy of the node's /etc/pve/pve-root-ca.pem. Empty means the system trust store. Unlike a pinned
	// fingerprint, it keeps working when the certificate is renewed.
	CACertFile string `yaml:"caCertFile,omitempty"`
	// TLSServerName is the name the API's certificate must be issued for, when the URL uses an IP address the
	// certificate doesn't list. Empty means the URL's host.
	TLSServerName string `yaml:"tlsServerName,omitempty"`
	// TLSFingerprint pins the API's certificate by its SHA-256 fingerprint (AA:BB:...) instead of verifying it, for
	// a certificate from a CA the controller can't trust. A renewed certificate breaks the pin.
	TLSFingerprint string `yaml:"tlsFingerprint,omitempty"`
	// Node is the standalone node that runs the workers.
	Node string `yaml:"node"`
	// Pool is the resource pool that holds templates and workers.
	Pool string `yaml:"pool"`
	// Storage is where clones are created.
	Storage string `yaml:"storage"`
	// Zone is the SDN zone that holds VNet. Empty means DefaultZone, the zone the installer creates.
	Zone string `yaml:"zone,omitempty"`
	// VNet is the worker network's SDN VNet.
	VNet string `yaml:"vnet"`
	// VMIDRange is the range new VMs take their IDs from.
	VMIDRange VMIDRange `yaml:"vmidRange"`
	// LinkedClone chooses linked clones over full clones. Nil means DefaultLinkedClone; Parse always sets it.
	LinkedClone *bool `yaml:"linkedClone,omitempty"`
}

// UseLinkedClone reports whether workers are created as linked clones.
func (p Proxmox) UseLinkedClone() bool {
	if p.LinkedClone == nil {
		return DefaultLinkedClone
	}
	return *p.LinkedClone
}

// VMIDRange is an inclusive range of VM IDs.
type VMIDRange struct {
	Start int `yaml:"start"`
	End   int `yaml:"end"`
}

// Size is the number of IDs in the range.
func (r VMIDRange) Size() int {
	if r.End < r.Start {
		return 0
	}
	return r.End - r.Start + 1
}

// Contains reports whether id is in the range.
func (r VMIDRange) Contains(id int) bool {
	return id >= r.Start && id <= r.End
}

// Workers returns the part of the range that worker VMs take their IDs from: everything but the last ReservedVMIDs
// IDs, which hold templates and the installer's smoke-test clones. Keeping those apart from workers lets the
// controller tell a new template, whose template flag Proxmox reports late, from a half-created worker clone, which
// carries the template's tags.
func (r VMIDRange) Workers() VMIDRange {
	return VMIDRange{Start: r.Start, End: r.End - ReservedVMIDs}
}

// GitHub says which organization or repository the scale sets belong to and how to authenticate.
type GitHub struct {
	// ConfigURL is https://github.com/<org> or https://github.com/<owner>/<repo>.
	ConfigURL string    `yaml:"configUrl,omitempty"`
	App       GitHubApp `yaml:"app,omitempty"`
}

// IsRepository reports whether ConfigURL names a repository rather than an organization.
func (g GitHub) IsRepository() bool {
	_, repo, err := ParseGitHubURL(g.ConfigURL)
	return err == nil && repo != ""
}

// ParseGitHubURL splits https://github.com/<org> or https://github.com/<owner>/<repo>, with or without a trailing
// slash, into the owner and, for a repository, its name. Anything else is an error: GitHub Enterprise Server and
// enterprise-level scale sets are out of scope (README, "Limitations").
func ParseGitHubURL(raw string) (owner, repo string, err error) {
	u, err := url.Parse(raw)
	if err == nil && u.Scheme == "https" && u.Host == "github.com" && u.RawQuery == "" && u.Fragment == "" &&
		u.User == nil {
		switch parts := strings.Split(strings.Trim(u.Path, "/"), "/"); {
		case len(parts) == 1 && parts[0] != "":
			return parts[0], "", nil
		case len(parts) == 2 && parts[0] != "" && parts[1] != "":
			return parts[0], parts[1], nil
		}
	}
	return "", "", fmt.Errorf("%q must be https://github.com/<org> or https://github.com/<owner>/<repo>", raw)
}

// GitHubApp holds the GitHub App credentials. The installer writes them once it has created the App; until then
// they are empty (see RequireGitHubApp).
type GitHubApp struct {
	ClientID       string `yaml:"clientId,omitempty"`
	InstallationID int64  `yaml:"installationId,omitempty"`
	// PrivateKeyFile is the absolute path of the App's PEM private key.
	PrivateKeyFile string `yaml:"privateKeyFile,omitempty"`
}

// Worker is a worker VM's hardware. A zero field means "not set" and is filled from the level above.
type Worker struct {
	Cores     int `yaml:"cores,omitempty"`
	MemoryMiB int `yaml:"memoryMiB,omitempty"`
	// FreeDiskGiB is added on top of the template's disk size.
	FreeDiskGiB int `yaml:"freeDiskGiB,omitempty"`
}

// ScaleSet is one GitHub runner scale set served by the controller.
type ScaleSet struct {
	Name string `yaml:"name"`
	// Labels are what workflows put in runs-on. Empty means just the name.
	Labels []string `yaml:"labels,omitempty"`
	// RunnerGroup is the GitHub runner group the scale set belongs to, which controls which repositories and
	// workflows may use it. Empty means DefaultRunnerGroup. Repository scale sets must use the default group.
	RunnerGroup string        `yaml:"runnerGroup,omitempty"`
	MinRunners  int           `yaml:"minRunners,omitempty"`
	MaxRunners  int           `yaml:"maxRunners"`
	MaxLifetime time.Duration `yaml:"maxLifetime,omitempty"`
	// Worker overrides the controller-wide worker hardware. After Parse, every field is set.
	Worker Worker `yaml:"worker,omitempty"`
}

// Marshal encodes a config as a YAML document that Parse reads back to the same config. Unset optional fields are
// left out, so defaults stay defaults.
func Marshal(c *Config) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(c); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode config: %w", err)
	}
	return buf.Bytes(), nil
}

// Load reads and parses the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the operator chooses the config path.
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	c, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	return c, nil
}

// Parse decodes a config document, fills in defaults, and validates the result. Unknown fields are errors, so a
// typo can't silently fall back to a default. A validation failure is a *ValidationError listing every problem.
func Parse(data []byte) (*Config, error) {
	var c Config
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("config is empty")
		}
		return nil, fmt.Errorf("decode: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("config must contain exactly one YAML document")
	}

	c.applyDefaults()
	if err := c.validate(); err != nil {
		return nil, err
	}
	return &c, nil
}
