// Package settings is the Proxmox host's record of how this node's runners are set up: the network, storage, and
// VMIDs the installer used, the GitHub target and App it learned, and the scale set and worker hardware the user
// chose. parcon keeps it in /etc/proxmox-actions-runners/settings.yaml on the host and renders the controller VM's
// config.yaml from it, so the settings are the source of truth and the controller's config follows them.
//
// Nothing secret is in them: the App's private key and the Proxmox token live only in the controller VM.
package settings

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"go.yaml.in/yaml/v3"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

// DefaultPath is where the settings live on the host. The directory has the same name as the controller VM's, but
// the file has a different one, so the two are never confused.
const DefaultPath = "/etc/proxmox-actions-runners/settings.yaml"

// Version is the settings file's format version.
const Version = 1

// DHCP is the value of an address setting that asks for DHCP.
const DHCP = "dhcp"

// Settings is the whole settings file.
type Settings struct {
	Version  int      `yaml:"version"`
	Network  Network  `yaml:"network"`
	Proxmox  Proxmox  `yaml:"proxmox"`
	GitHub   GitHub   `yaml:"github,omitempty"`
	ScaleSet ScaleSet `yaml:"scaleSet"`
	// Worker is the worker hardware the user set. A zero field means the GitHub-matching default in internal/config.
	Worker config.Worker `yaml:"worker,omitempty"`
}

// Network is the LAN side of the gateway and controller VMs, and the worker network behind the gateway.
type Network struct {
	// Bridge is the LAN bridge the gateway and controller VMs attach to.
	Bridge string `yaml:"bridge"`
	// VLAN is the VLAN tag on the bridge; 0 means none.
	VLAN int `yaml:"vlan,omitempty"`
	// PVEAddress is the address the controller reaches the API on. Empty means the host's first address on Bridge.
	PVEAddress string `yaml:"pveAddress,omitempty"`
	// GatewayIP and ControllerIP are "dhcp" or a static address with its prefix, such as 192.0.2.10/24.
	GatewayIP    string `yaml:"gatewayIP"`
	ControllerIP string `yaml:"controllerIP"`
	// LANGateway is the LAN's router, needed with a static address.
	LANGateway string `yaml:"lanGateway,omitempty"`
	// WorkerSubnet is the worker network's IPv4 subnet.
	WorkerSubnet string `yaml:"workerSubnet"`
}

// Proxmox is where VMs go.
type Proxmox struct {
	Storage   string           `yaml:"storage"`
	VMIDRange config.VMIDRange `yaml:"vmidRange"`
}

// GitHub is the organization or repository the runners serve and the App they use, as learned during the install.
type GitHub struct {
	ConfigURL string `yaml:"configUrl,omitempty"`
	App       App    `yaml:"app,omitempty"`
}

// App identifies the GitHub App. Its private key is only in the controller VM.
type App struct {
	ClientID       string `yaml:"clientId,omitempty"`
	InstallationID int64  `yaml:"installationId,omitempty"`
	// Slug is the App's name in its URLs, when parcon created it: it makes the App's install link.
	Slug string `yaml:"slug,omitempty"`
}

// ScaleSet is the one scale set the installer creates.
type ScaleSet struct {
	Name string `yaml:"name"`
	// Labels are the runs-on labels. Empty means just the name.
	Labels      []string `yaml:"labels,omitempty"`
	RunnerGroup string   `yaml:"runnerGroup"`
	MinRunners  int      `yaml:"minRunners"`
	MaxRunners  int      `yaml:"maxRunners"`
}

// Default returns the settings of a default install. The values come from internal/config, the one home of every
// default.
func Default() Settings {
	return Settings{
		Version: Version,
		Network: Network{
			Bridge:       config.DefaultBridge,
			GatewayIP:    DHCP,
			ControllerIP: DHCP,
			WorkerSubnet: config.DefaultWorkerSubnet,
		},
		Proxmox: Proxmox{
			Storage:   config.DefaultStorage,
			VMIDRange: config.VMIDRange{Start: config.DefaultVMIDStart, End: config.DefaultVMIDEnd},
		},
		ScaleSet: ScaleSet{
			Name:        config.DefaultScaleSetName,
			RunnerGroup: config.DefaultRunnerGroup,
			MinRunners:  config.DefaultMinRunners,
			MaxRunners:  config.DefaultMaxRunners,
		},
	}
}

// Load reads and validates the settings file.
func Load(path string) (*Settings, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the settings path.
	if err != nil {
		return nil, fmt.Errorf("read settings: %w", err)
	}
	s, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("settings %s: %w", path, err)
	}
	return s, nil
}

// Parse decodes and validates settings. Unknown fields are errors.
func Parse(data []byte) (*Settings, error) {
	var s Settings
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&s); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("settings are empty")
		}
		return nil, fmt.Errorf("decode: %w", err)
	}
	if s.Version != Version {
		return nil, fmt.Errorf("version %d isn't supported; this parcon reads version %d", s.Version, Version)
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	return &s, nil
}

const header = "# parcon's settings for this node. Change them with `parcon config set KEY VALUE`, which also updates\n" +
	"# the controller; see `parcon config get --all`.\n"

// Marshal encodes the settings as the file parcon writes.
func (s *Settings) Marshal() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteString(header)
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(s); err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("encode settings: %w", err)
	}
	return buf.Bytes(), nil
}

// Save writes the settings to path atomically, readable only by root.
func (s *Settings) Save(path string) error {
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("save settings: %w", err)
	}
	return pvecli.WriteFileAtomic(path, data, 0o600)
}

// ValidationError lists every problem in the settings, each prefixed with its field.
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid settings:\n  - " + strings.Join(e.Problems, "\n  - ")
}

var (
	// Names end up in Proxmox commands and in YAML, so each is kept to a strict pattern.
	idPattern          = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]*$`)
	labelPattern       = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)
	runnerGroupPattern = regexp.MustCompile(`^[A-Za-z0-9._ -]+$`)
	githubURLPattern   = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9-]+(/[A-Za-z0-9._-]+)?$`)
	slugPattern        = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// Validate checks every setting and reports every problem.
func (s *Settings) Validate() error {
	var p []string
	add := func(field, format string, args ...any) { p = append(p, field+": "+fmt.Sprintf(format, args...)) }

	n := s.Network
	if !idPattern.MatchString(n.Bridge) {
		add("network.bridge", "%q is not a bridge name", n.Bridge)
	}
	if n.VLAN < 0 || n.VLAN > 4094 {
		add("network.vlan", "%d must be 1 to 4094, or 0 for none", n.VLAN)
	}
	if a, err := netip.ParseAddr(n.PVEAddress); n.PVEAddress != "" && (err != nil || !a.Is4()) {
		add("network.pveAddress", "%q must be an IPv4 address", n.PVEAddress)
	}
	static := false
	for _, a := range []struct{ field, value string }{
		{"network.gatewayIP", n.GatewayIP}, {"network.controllerIP", n.ControllerIP},
	} {
		if a.value == DHCP {
			continue
		}
		static = true
		if p, err := netip.ParsePrefix(a.value); err != nil || !p.Addr().Is4() {
			add(a.field, "%q must be dhcp or an IPv4 address with its prefix, such as 192.0.2.10/24", a.value)
		}
	}
	if a, err := netip.ParseAddr(n.LANGateway); static && (err != nil || !a.Is4()) {
		add("network.lanGateway", "the LAN router's IPv4 address is required with a static address")
	}
	if sub, err := netip.ParsePrefix(n.WorkerSubnet); err != nil || !sub.Addr().Is4() {
		add("network.workerSubnet", "%q must be an IPv4 subnet, such as %s", n.WorkerSubnet, config.DefaultWorkerSubnet)
	} else if sub.Bits() < 8 || sub.Bits() > 29 || sub.Masked() != sub {
		add("network.workerSubnet", "%q must be a network address with a /8 to /29 prefix, such as %s",
			n.WorkerSubnet, sub.Masked())
	}

	if !idPattern.MatchString(s.Proxmox.Storage) {
		add("proxmox.storage", "%q is not a storage ID", s.Proxmox.Storage)
	}
	r := s.Proxmox.VMIDRange
	switch {
	case r.Start < 100 || r.End > 999999999:
		add("proxmox.vmidRange", "VMIDs must be 100 to 999999999")
	case r.Start > r.End:
		add("proxmox.vmidRange", "start %d is after end %d", r.Start, r.End)
	case r.Size() < s.ScaleSet.MaxRunners+config.ReservedVMIDs:
		add("proxmox.vmidRange", "has %d IDs but needs %d: maxRunners plus %d reserved", r.Size(),
			s.ScaleSet.MaxRunners+config.ReservedVMIDs, config.ReservedVMIDs)
	}

	g := s.GitHub
	if g.ConfigURL != "" && !githubURLPattern.MatchString(g.ConfigURL) {
		add("github.configUrl", "%q must be https://github.com/<org> or https://github.com/<owner>/<repo>",
			g.ConfigURL)
	}
	if g.App.ClientID != "" && !labelPattern.MatchString(g.App.ClientID) {
		add("github.app.clientId", "%q is not a Client ID", g.App.ClientID)
	}
	if g.App.Slug != "" && !slugPattern.MatchString(g.App.Slug) {
		add("github.app.slug", "%q is not an App's slug", g.App.Slug)
	}
	if g.App.InstallationID < 0 {
		add("github.app.installationId", "must not be negative")
	}

	ss := s.ScaleSet
	if !config.ScaleSetNamePattern.MatchString(ss.Name) {
		add("scaleSet.name", "%q must be 1 to 63 lowercase letters, digits, '.', '_', or '-', starting with a letter "+
			"or digit", ss.Name)
	}
	for _, l := range ss.Labels {
		if !labelPattern.MatchString(l) {
			add("scaleSet.labels", "%q must be letters, digits, '.', '_', or '-'", l)
		}
	}
	if !runnerGroupPattern.MatchString(ss.RunnerGroup) {
		add("scaleSet.runnerGroup", "%q has characters a runner group can't have", ss.RunnerGroup)
	} else if g.ConfigURL != "" && (config.GitHub{ConfigURL: g.ConfigURL}).IsRepository() &&
		ss.RunnerGroup != config.DefaultRunnerGroup {
		add("scaleSet.runnerGroup", "repository runners must use the runner group %q", config.DefaultRunnerGroup)
	}
	if ss.MinRunners < 0 {
		add("scaleSet.minRunners", "must not be negative")
	}
	if ss.MaxRunners < 1 {
		add("scaleSet.maxRunners", "must be at least 1")
	} else if ss.MinRunners > ss.MaxRunners {
		add("scaleSet.minRunners", "%d is more than maxRunners %d", ss.MinRunners, ss.MaxRunners)
	}

	if w := s.Worker; w.Cores != 0 && w.Cores < config.MinCores {
		add("worker.cores", "must be at least %d", config.MinCores)
	}
	if w := s.Worker; w.MemoryMiB != 0 && w.MemoryMiB < config.MinMemoryMiB {
		add("worker.memoryMiB", "must be at least %d", config.MinMemoryMiB)
	}
	if s.Worker.FreeDiskGiB < 0 {
		add("worker.freeDiskGiB", "must not be negative")
	}

	if len(p) > 0 {
		return &ValidationError{Problems: p}
	}
	return nil
}

// EffectiveWorker returns the worker hardware in effect: the settings' values, with defaults for those not set.
func (s *Settings) EffectiveWorker() config.Worker {
	w, d := s.Worker, config.DefaultWorker()
	if w.Cores == 0 {
		w.Cores = d.Cores
	}
	if w.MemoryMiB == 0 {
		w.MemoryMiB = d.MemoryMiB
	}
	if w.FreeDiskGiB == 0 {
		w.FreeDiskGiB = d.FreeDiskGiB
	}
	return w
}
