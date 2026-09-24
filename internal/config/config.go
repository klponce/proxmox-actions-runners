// Package config defines the controller's configuration file: its schema, defaults, and validation.
//
// The installer writes the file to /etc/proxmox-actions-runners/config.yaml in the controller VM. Secrets are never
// stored in it; it only names the files that hold them.
package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"go.yaml.in/yaml/v3"
)

// DefaultPath is where the installer writes the config file inside the controller VM.
const DefaultPath = "/etc/proxmox-actions-runners/config.yaml"

// Config is the controller's whole configuration.
type Config struct {
	Proxmox Proxmox `yaml:"proxmox"`
	GitHub  GitHub  `yaml:"github"`
	Metrics Metrics `yaml:"metrics"`
	// Worker is the controller-wide worker hardware. Fields left unset use the GitHub-matching defaults.
	Worker    Worker     `yaml:"worker"`
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
	// TLSFingerprint pins the API's certificate by its SHA-256 fingerprint (AA:BB:...). Empty means the system
	// trust store is used instead.
	TLSFingerprint string `yaml:"tlsFingerprint"`
	// Node is the standalone node that runs the workers.
	Node string `yaml:"node"`
	// Pool is the resource pool that holds templates and workers.
	Pool string `yaml:"pool"`
	// Storage is where clones are created.
	Storage string `yaml:"storage"`
	// VNet is the worker network's SDN VNet.
	VNet string `yaml:"vnet"`
	// VMIDRange is the range new VMs take their IDs from.
	VMIDRange VMIDRange `yaml:"vmidRange"`
	// LinkedClone chooses linked clones over full clones. Nil means DefaultLinkedClone; Parse always sets it.
	LinkedClone *bool `yaml:"linkedClone"`
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

// GitHub says which organization or repository the scale sets belong to and how to authenticate.
type GitHub struct {
	// ConfigURL is https://github.com/<org> or https://github.com/<owner>/<repo>.
	ConfigURL string    `yaml:"configUrl"`
	App       GitHubApp `yaml:"app"`
}

// GitHubApp holds the GitHub App credentials. The installer's App setup writes them.
type GitHubApp struct {
	ClientID       string `yaml:"clientId"`
	InstallationID int64  `yaml:"installationId"`
	// PrivateKeyFile is the absolute path of the App's PEM private key.
	PrivateKeyFile string `yaml:"privateKeyFile"`
}

// Metrics configures the metrics and health endpoint.
type Metrics struct {
	// Listen is the loopback address to serve on. nginx publishes it on the LAN over HTTPS.
	Listen string `yaml:"listen"`
}

// Worker is a worker VM's hardware. A zero field means "not set" and is filled from the level above.
type Worker struct {
	Cores     int `yaml:"cores"`
	MemoryMiB int `yaml:"memoryMiB"`
	// FreeDiskGiB is added on top of the template's disk size.
	FreeDiskGiB int `yaml:"freeDiskGiB"`
}

// ScaleSet is one GitHub runner scale set served by the controller.
type ScaleSet struct {
	Name string `yaml:"name"`
	// Labels are what workflows put in runs-on. Empty means just the name.
	Labels      []string      `yaml:"labels"`
	MinRunners  int           `yaml:"minRunners"`
	MaxRunners  int           `yaml:"maxRunners"`
	MaxLifetime time.Duration `yaml:"maxLifetime"`
	// Worker overrides the controller-wide worker hardware. After Parse, every field is set.
	Worker Worker `yaml:"worker"`
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
