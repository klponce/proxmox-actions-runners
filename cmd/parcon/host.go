package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/installer"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/term"
)

// hostMode reports whether parcon runs on the Proxmox host rather than in the controller VM. Tests replace it.
var hostMode = func() bool {
	st, err := os.Stat("/etc/pve/nodes")
	return err == nil && st.IsDir()
}

// newInstaller returns the Installer for this host. Tests replace it.
var newInstaller = func(stdout, stderr io.Writer, dryRun bool) (*installer.Installer, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find parcon's own path: %w", err)
	}
	// A development build has no release version; only install and update need one.
	v, _ := release.ParseVersion(version)
	return installer.New(&term.Out{W: stdout, Err: stderr}, self, v, dryRun)
}

// needRelease makes sure the Installer has a release version to install. A development build has none, and can
// install only from local assets, whose version it is given.
func needRelease(in *installer.Installer, assetsVersion string) error {
	if assetsVersion != "" {
		v, err := release.ParseVersion(assetsVersion)
		if err != nil {
			return err
		}
		in.Version = v
		return nil
	}
	if in.Version == (release.Version{}) {
		return errors.New("this parcon is a development build without a release version; install from a " +
			"release (see the README)")
	}
	return nil
}

// hostContext is canceled by SIGINT and SIGTERM, so a command stops between steps and cleans up.
func hostContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

// parseFlags parses a host command's flags, reporting mistakes as usage errors.
func parseHostFlags(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return flag.ErrHelp
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v (flags go before nothing else)\n", fs.Args())
		return errUsage
	}
	return nil
}

// settingsFlags are install's flags for the host settings.
type settingsFlags struct {
	s        settings.Settings
	vmids    string
	labels   string
	set      []string
	given    bool
	app      string
	clientID string
}

func (f *settingsFlags) register(fs *flag.FlagSet) {
	f.s = settings.Default()
	n := &f.s.Network
	fs.StringVar(&n.Bridge, "bridge", n.Bridge, "LAN bridge for the controller and gateway VMs")
	fs.IntVar(&n.VLAN, "vlan", 0, "VLAN tag on the LAN bridge (default none)")
	fs.StringVar(&n.PVEAddress, "pve-address", "", "this node's address the controller uses for the API "+
		"(default: its address on the bridge)")
	fs.StringVar(&n.GatewayIP, "gateway-ip", n.GatewayIP, "gateway VM's LAN address: dhcp or a CIDR such as 192.0.2.10/24")
	fs.StringVar(&n.ControllerIP, "controller-ip", n.ControllerIP, "controller VM's LAN address: dhcp or a CIDR")
	fs.StringVar(&n.LANGateway, "lan-gateway", "", "LAN router, needed with a static address")
	fs.StringVar(&n.WorkerSubnet, "worker-subnet", n.WorkerSubnet, "worker network")
	fs.StringVar(&f.s.Proxmox.Storage, "storage", f.s.Proxmox.Storage, "storage for VM disks")
	fs.StringVar(&f.vmids, "vmid-range", fmt.Sprintf("%d-%d", config.DefaultVMIDStart, config.DefaultVMIDEnd),
		"VMIDs for workers and templates, as FIRST-LAST")
	ss := &f.s.ScaleSet
	fs.StringVar(&ss.Name, "scale-set", ss.Name, "scale set name in lowercase, used in runs-on")
	fs.StringVar(&f.labels, "labels", "", "comma-separated runs-on labels (default: the scale set name)")
	fs.StringVar(&ss.RunnerGroup, "runner-group", ss.RunnerGroup, "GitHub runner group; repositories must use default")
	fs.IntVar(&ss.MinRunners, "min-runners", ss.MinRunners, "idle workers to keep booted")
	fs.StringVar(&f.s.GitHub.ConfigURL, "github-url", "", "the organization or repository the App must be "+
		"installed on, as https://github.com/<org> or https://github.com/<owner>/<repo> (default: where you "+
		"install the App)")
	fs.Func("set", "a config key's value, as KEY=VALUE (see parcon config describe); repeat for more",
		func(v string) error { f.set = append(f.set, v); return nil })
	fs.StringVar(&f.app, "app", string(installer.AppManifest), "how to get the GitHub App: manifest (create one in "+
		"a browser) or manual (use an existing App)")
	fs.StringVar(&f.clientID, "client-id", "", "the existing App's Client ID, with --app manual")
}

// settings applies the parsed flags. fs tells which settings the user gave.
func (f *settingsFlags) settings(fs *flag.FlagSet, limits settings.Limits) (*settings.Settings, error) {
	fs.Visit(func(fl *flag.Flag) {
		if fl.Name != "app" && fl.Name != "client-id" {
			f.given = true
		}
	})
	start, end, ok := strings.Cut(f.vmids, "-")
	first, err1 := strconv.Atoi(start)
	last, err2 := strconv.Atoi(end)
	if !ok || err1 != nil || err2 != nil {
		return nil, fmt.Errorf("--vmid-range %q must be FIRST-LAST, such as 10000-10099", f.vmids)
	}
	f.s.Proxmox.VMIDRange = config.VMIDRange{Start: first, End: last}
	if f.labels != "" {
		f.s.ScaleSet.Labels = strings.Split(f.labels, ",")
	}
	f.s.GitHub.ConfigURL = strings.TrimSuffix(f.s.GitHub.ConfigURL, "/")
	for _, kv := range f.set {
		key, value, ok := strings.Cut(kv, "=")
		if !ok {
			return nil, fmt.Errorf("--set %q must be KEY=VALUE", kv)
		}
		if err := settings.SetKey(&f.s, key, value, limits); err != nil {
			return nil, err
		}
	}
	switch installer.AppMode(f.app) {
	case installer.AppManifest, installer.AppManual:
	default:
		return nil, fmt.Errorf("--app must be manifest or manual, not %q", f.app)
	}
	if err := f.s.Validate(); err != nil {
		return nil, err
	}
	return &f.s, nil
}

// hostFlags are the flags every host command that changes the node shares.
type hostFlags struct {
	yes, dryRun   bool
	assets        string
	assetsVersion string
}

func (h *hostFlags) register(fs *flag.FlagSet, withAssets bool) {
	fs.BoolVar(&h.yes, "yes", false, "answer yes to every confirmation")
	fs.BoolVar(&h.dryRun, "dry-run", false, "run the checks, print the plan, and change nothing")
	if withAssets {
		// For the integration tests and development builds: use a directory of release assets instead of
		// downloading a release. Not in the usage text.
		fs.StringVar(&h.assets, "assets", "", "")
		fs.StringVar(&h.assetsVersion, "assets-version", "", "")
	}
}

func runInstall(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("install", flag.ContinueOnError)
	var h hostFlags
	h.register(fs, true)
	var sf settingsFlags
	sf.register(fs)
	stopAfter := fs.String("stop-after", "", "")
	if err := parseHostFlags(fs, args, stderr); err != nil {
		return usageOrNil(err)
	}
	if h.assets == "" && h.assetsVersion != "" {
		return fmt.Errorf("--assets-version needs --assets")
	}
	in, err := newInstaller(stdout, stderr, h.dryRun)
	if err != nil {
		return err
	}
	if err := needRelease(in, h.assetsVersion); err != nil {
		return err
	}
	in.Yes, in.Assets, in.StopAfter = h.yes, h.assets, *stopAfter
	s, err := sf.settings(fs, in.Limits())
	if err != nil {
		return err
	}
	ctx, cancel := hostContext()
	defer cancel()
	return in.Install(ctx, installer.InstallOptions{Settings: s, FlagsGiven: sf.given,
		App: installer.AppMode(sf.app), ClientID: sf.clientID})
}

func usageOrNil(err error) error {
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

func runUninstall(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	var h hostFlags
	h.register(fs, false)
	if err := parseHostFlags(fs, args, stderr); err != nil {
		return usageOrNil(err)
	}
	in, err := newInstaller(stdout, stderr, h.dryRun)
	if err != nil {
		return err
	}
	in.Yes = h.yes
	ctx, cancel := hostContext()
	defer cancel()
	return in.Uninstall(ctx)
}

// runHostCheck handles "parcon check" on the host: every check before an install, and the node's and the
// controller's health after one. "parcon check network" runs the worker network check, and the controller's own
// checks are forwarded into it.
func runHostCheck(args []string, stdout, stderr io.Writer) error {
	target := ""
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		target, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet("check", flag.ContinueOnError)
	if err := parseHostFlags(fs, args, stderr); err != nil {
		return usageOrNil(err)
	}
	in, err := newInstaller(stdout, stderr, false)
	if err != nil {
		return err
	}
	ctx, cancel := hostContext()
	defer cancel()
	switch target {
	case "":
		return in.Check(ctx)
	case "network":
		return in.CheckNetworkInstalled(ctx)
	case "config", "proxmox", "github", "template":
		return in.Forward(ctx, stdout, stderr, "check", target)
	default:
		fmt.Fprint(stderr, hostUsage)
		return errUsage
	}
}
