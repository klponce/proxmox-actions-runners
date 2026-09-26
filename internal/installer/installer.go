// Package installer is parcon's host side: it installs, checks, updates, and uninstalls the runners on a standalone
// Proxmox VE node, and pushes the host's settings into the controller VM. It is install.sh's successor, and keeps its
// rules (docs/install.md): checks come first and change nothing, every change is logged and shown in a plan first,
// every step checks what already exists so a re-run continues, and secrets reach a VM only on a command's stdin.
package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/term"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// Proxmox objects. Uninstall finds everything by these names and tags.
const (
	SystemPool = "par-system"
	RunnerPool = config.DefaultPool
	Role       = "PARController"
	PVEUser    = "par@pve"
	TokenName  = "controller"
	Zone       = config.DefaultZone
	VNet       = config.DefaultVNet
	// PoolComment marks the system pool as ours. The installer creates it first, so it also marks an earlier,
	// unfinished install.
	PoolComment = "proxmox-actions-runners"
)

// Sizes, in GiB unless named otherwise.
const (
	// GatewayGiB and ControllerGiB are the gateway and controller images' and VMs' disks. Each image holds about 2.5
	// GiB when built, but a kernel update (unattended-upgrades) briefly needs two kernels and a new initramfs.
	GatewayGiB    = 6
	ControllerGiB = 6
	// FreeGiB is what the VM storage needs for the default install: the gateway, controller, and template disks,
	// and room for a worker.
	FreeGiB = 50
	// DownloadDir holds the images between download and import, on the host's root filesystem; DownloadGiB is
	// what the three need, with room to grow.
	DownloadDir = "/var/tmp"
	DownloadGiB = 6
)

// ControllerStopTimeout is how long parcon.service may take to stop or restart: longer than its TimeoutStopSec in
// deploy/parcon.service, since the controller finishes retirements in flight first. A test keeps the two in step.
const ControllerStopTimeout = 420 * time.Second

// HelperURL is the GitHub Pages helper for the GitHub App manifest flow.
const HelperURL = "https://klponce.github.io/proxmox-actions-runners/app/v1/"

// BinaryPath is where parcon lives on the host.
const BinaryPath = "/usr/local/bin/parcon"

// Installer runs host commands. Every field but Roots, Assets, and StopAfter must be set; New fills them for the
// real host.
type Installer struct {
	PVE    pvecli.PVE
	Change *pvecli.Changer
	Sys    hostsys.System
	Term   term.Terminal
	Out    *term.Out
	// Yes answers yes to every confirmation.
	Yes bool
	// SettingsPath is the host settings file, and BinaryPath where parcon is installed.
	SettingsPath string
	BinaryPath   string
	// Self is the running parcon executable, which install copies to BinaryPath.
	Self string
	// Version is this parcon's release. Install and update bring the node to it.
	Version release.Version
	// Source is where releases come from, and Keys the release signing keys parcon trusts.
	Source release.Source
	Keys   []ed25519.PublicKey
	// Assets, if set, is a directory holding the release's assets and SHA256SUMS, used instead of downloading.
	Assets string
	// Roots stands in for the system CAs when checking the API's certificate; nil means the real ones.
	Roots *x509.CertPool
	// Reach checks that the host reaches an HTTPS host.
	Reach func(ctx context.Context, host string) error
	// Now and Sleep are the clock, which tests speed up.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	// StopAfter stops an install after the named step, for the integration tests.
	StopAfter string
	// LockPath is the lock every command that changes the node holds.
	LockPath string

	node string
}

// New returns an Installer for this host, writing its progress to out.
func New(out *term.Out, self string, version release.Version, dryRun bool) (*Installer, error) {
	keys, err := release.TrustedKeys()
	if err != nil {
		return nil, err
	}
	exec := pvecli.OSExec{}
	return &Installer{
		PVE:          pvecli.PVE{Exec: exec},
		Change:       &pvecli.Changer{Exec: exec, Out: out.W, DryRun: dryRun},
		Sys:          hostsys.System{Paths: hostsys.DefaultPaths(), Exec: exec},
		Term:         term.TTY{},
		Out:          out,
		SettingsPath: settings.DefaultPath,
		BinaryPath:   BinaryPath,
		Self:         self,
		Version:      version,
		Source:       release.GitHub{},
		Keys:         keys,
		Reach:        reachHTTPS,
		Now:          time.Now,
		Sleep:        sleep,
		LockPath:     "/run/lock/parcon.lock",
	}, nil
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Lock takes the lock that keeps two commands from changing the node at once.
func (in *Installer) Lock() (unlock func(), err error) {
	if in.LockPath == "" {
		return func() {}, nil
	}
	f, err := os.OpenFile(in.LockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("lock %s: %w", in.LockPath, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, errors.New("another parcon command is changing this node; wait for it to finish")
	}
	return func() { _ = f.Close() }, nil
}

// confirm asks a yes/no question, or answers yes with Yes. Without a terminal it fails and names --yes.
func (in *Installer) confirm(question string) error {
	if in.Yes {
		return nil
	}
	ok, err := in.Term.Confirm(question)
	if errors.Is(err, term.ErrNoTerminal) {
		return fmt.Errorf("%s There's no terminal to answer on: run with --yes to answer yes", question)
	}
	if err != nil {
		return err
	}
	if !ok {
		return ErrCanceled
	}
	return nil
}

// ErrCanceled is a plan the user declined.
var ErrCanceled = errors.New("canceled")

// nodeName returns this node's name.
func (in *Installer) nodeName(ctx context.Context) (string, error) {
	if in.node == "" {
		n, err := in.PVE.NodeName(ctx)
		if err != nil {
			return "", err
		}
		in.node = n
	}
	return in.node, nil
}

// vms returns every VM on the node.
func (in *Installer) vms(ctx context.Context) ([]proxmox.VM, error) {
	node, err := in.nodeName(ctx)
	if err != nil {
		return nil, err
	}
	return in.PVE.VMs(ctx, node)
}

// tagged returns the VMs in pool that carry tag, by VMID.
func tagged(vms []proxmox.VM, pool, tag string) []proxmox.VM {
	var out []proxmox.VM
	for _, vm := range vms {
		if vm.Pool == pool && vm.HasTag(tag) {
			out = append(out, vm)
		}
	}
	return out
}

// systemVM returns the gateway or controller VM, by its tag.
func (in *Installer) systemVM(ctx context.Context, tag string) (proxmox.VM, bool, error) {
	vms, err := in.vms(ctx)
	if err != nil {
		return proxmox.VM{}, false, err
	}
	if t := tagged(vms, SystemPool, tag); len(t) > 0 {
		return t[0], true, nil
	}
	return proxmox.VM{}, false, nil
}

// releaseTag is this release's tag.
func (in *Installer) releaseTag() string { return vmtags.ReleasePrefix + in.Version.String() }

// InstalledRelease returns the release of a finished install: the controller VM's release tag. The controller gets
// it last, so an install that stopped partway has none.
func (in *Installer) InstalledRelease(ctx context.Context) (release.Version, bool, error) {
	vm, ok, err := in.systemVM(ctx, vmtags.Controller)
	if err != nil || !ok {
		return release.Version{}, false, err
	}
	s, ok := vmtags.String(vm, vmtags.ReleasePrefix)
	if !ok {
		return release.Version{}, false, nil
	}
	v, err := release.ParseVersion(s)
	if err != nil {
		return release.Version{}, false, fmt.Errorf("controller VM %d: %w", vm.VMID, err)
	}
	return v, true, nil
}

// loadSettings reads the host settings.
func (in *Installer) loadSettings() (*settings.Settings, error) {
	s, err := settings.Load(in.SettingsPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("no settings at %s: parcon isn't installed on this node; run parcon install",
			in.SettingsPath)
	}
	return s, err
}

// saveSettings writes the host settings, logged like any other change.
func (in *Installer) saveSettings(s *settings.Settings) error {
	data, err := s.Marshal()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(in.SettingsPath), 0o700); err != nil && !in.Change.DryRun {
		return fmt.Errorf("save settings: %w", err)
	}
	return in.Change.WriteFile(in.SettingsPath, data, 0o600)
}

// pveAddress is the address the controller reaches the API on: the settings' choice, or the host's first IPv4
// address on the LAN bridge.
func (in *Installer) pveAddress(ctx context.Context, s *settings.Settings) (netip.Addr, error) {
	if s.Network.PVEAddress != "" {
		return netip.ParseAddr(s.Network.PVEAddress)
	}
	addrs, err := in.Sys.Addrs(ctx)
	if err != nil {
		return netip.Addr{}, err
	}
	for _, a := range addrs {
		if a.Dev == s.Network.Bridge {
			return a.Prefix.Addr(), nil
		}
	}
	return netip.Addr{}, fmt.Errorf("%s has no IPv4 address; set --pve-address", s.Network.Bridge)
}

// Limits are the node's limits for config keys.
func (in *Installer) Limits() settings.Limits {
	mem, _ := in.Sys.MemTotalMiB()
	return settings.Limits{HostCPUs: in.Sys.CPUs(), HostMemMiB: mem}
}
