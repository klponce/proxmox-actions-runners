package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// InstallOptions are the choices for an install.
type InstallOptions struct {
	// Settings are the settings for a new install: the defaults with the user's flags applied.
	Settings *settings.Settings
	// FlagsGiven reports whether the user gave any settings. An install that continues an earlier run uses the
	// saved settings, and refuses new ones.
	FlagsGiven bool
	// App is how to get the GitHub App, and ClientID the existing App's with AppManual.
	App      AppMode
	ClientID string
}

// ErrInstalled means the node already has a finished install.
var ErrInstalled = errors.New("already installed")

// Install installs the runners on this node, or continues an install that stopped partway.
func (in *Installer) Install(ctx context.Context, opts InstallOptions) error {
	if v, ok, err := in.InstalledRelease(ctx); err != nil {
		return err
	} else if ok {
		return fmt.Errorf("%w: release %s; run parcon update to update it, or parcon status to see it", ErrInstalled, v)
	}
	s := opts.Settings
	if saved, err := settings.Load(in.SettingsPath); err == nil {
		if opts.FlagsGiven {
			return fmt.Errorf("an earlier install's settings are in %s; run parcon install without settings to "+
				"continue it, or parcon uninstall to start over", in.SettingsPath)
		}
		s = saved
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := s.Validate(); err != nil {
		return err
	}
	if opts.App == AppManual && opts.ClientID == "" && s.GitHub.App.ClientID == "" {
		return errors.New("--app manual needs --client-id, the existing App's Client ID")
	}

	in.Out.Step("Preflight checks")
	checks := in.Preflight(ctx, s, ModeInstall)
	for _, c := range checks {
		in.Out.Say("%s", c)
	}
	if Failed(checks) {
		return errPreflight
	}
	if err := in.printPlan(ctx, s, Warnings(checks)); err != nil {
		return err
	}
	if in.Change.DryRun {
		return nil
	}
	if err := in.confirm(fmt.Sprintf("Install proxmox-actions-runners %s with this plan?", in.Version)); err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return in.install(ctx, s, opts)
}

func (in *Installer) install(ctx context.Context, s *settings.Settings, opts InstallOptions) error {
	in.Out.Step("parcon on the host")
	if err := in.installBinary(); err != nil {
		return err
	}
	if err := in.saveSettings(s); err != nil {
		return err
	}
	if err := in.createAccess(ctx); err != nil {
		return err
	}
	if err := in.createNetwork(ctx); err != nil {
		return err
	}
	if err := in.tuneOffloads(ctx, s); err != nil {
		return err
	}
	if in.stop("network") {
		return nil
	}

	in.Out.Step("Release %s", in.Version)
	assets, cleanup, err := in.assets(ctx)
	if err != nil {
		return err
	}
	defer cleanup()
	if err := in.importTemplate(ctx, s, assets); err != nil {
		return err
	}
	gateway, err := in.createGateway(ctx, s, func() (string, error) {
		return in.get(ctx, assets, gatewayImage(in.Version))
	})
	if err != nil {
		return err
	}
	controller, addr, err := in.createController(ctx, s, gateway, func() (string, error) {
		return in.get(ctx, assets, controllerImage(in.Version))
	})
	if err != nil {
		return err
	}
	if err := in.configureController(ctx, controller, s); err != nil {
		return err
	}
	if in.stop("configure") {
		return nil
	}
	if err := in.setupApp(ctx, controller, s, opts.App, opts.ClientID); err != nil {
		return err
	}
	started, err := in.startController(ctx, controller)
	if err != nil {
		return err
	}
	if err := in.CheckNetwork(ctx, s, addr); err != nil {
		return err
	}
	if err := in.waitSession(ctx, controller, started, 5*time.Minute); err != nil {
		return err
	}
	in.Out.Say("    the controller registered scale set %s and is listening for jobs", s.ScaleSet.Name)

	// The release tag goes on the controller last: it marks a finished install.
	if err := in.run(ctx, "qm", "set", fmt.Sprint(controller), "--tags",
		strings.Join(in.controllerSpec(s).tags, ";")+";"+in.releaseTag()); err != nil {
		return err
	}
	in.Out.Step("Done")
	labels := s.ScaleSet.Labels
	if len(labels) == 0 {
		labels = []string{s.ScaleSet.Name}
	}
	runsOn := labels[0]
	if len(labels) > 1 {
		runsOn = "[" + strings.Join(labels, ", ") + "]"
	}
	in.Out.Say(`Use the runners in a workflow in %s:

  runs-on: %s

Controller VM %d (%s), gateway VM %d.
Status:    parcon status
Settings:  parcon config get --all
Update:    parcon update
Uninstall: parcon uninstall`, s.GitHub.ConfigURL, runsOn, controller, addr, gateway)
	return nil
}

// stop reports whether the install stops after the named step.
func (in *Installer) stop(step string) bool {
	if in.StopAfter != step {
		return false
	}
	in.Out.Say("\nStopped after the %s step, as asked.", step)
	return true
}

// installBinary copies the running parcon to BinaryPath, unless it runs from there.
func (in *Installer) installBinary() error {
	if in.Self == in.BinaryPath {
		return nil
	}
	in.Change.Log("install " + in.Self + " " + in.BinaryPath)
	if in.Change.DryRun {
		return nil
	}
	return release.ReplaceBinary(in.Self, in.BinaryPath)
}

// printPlan prints what an install creates and changes.
func (in *Installer) printPlan(ctx context.Context, s *settings.Settings, warnings []Check) error {
	node, err := in.nodeName(ctx)
	if err != nil {
		return err
	}
	in.Out.Step("Plan")
	w := in.Out.W
	r := s.Proxmox.VMIDRange
	vlan := ""
	if s.Network.VLAN != 0 {
		vlan = fmt.Sprintf(" VLAN %d", s.Network.VLAN)
	}
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }
	p("Host:")
	p("  install parcon as %s, and its settings as %s", in.BinaryPath, in.SettingsPath)
	for _, l := range in.offloadPlan(ctx, s) {
		p("  %s", l)
	}
	p("Proxmox objects on node %s:", node)
	p("  pools %s and %s", SystemPool, RunnerPool)
	p("  role %s, user %s, token %s!%s, and ACLs for them on /pool/%s,", Role, PVEUser, PVEUser, TokenName, RunnerPool)
	p("    /storage/%s, and /sdn/zones/%s/%s", s.Proxmox.Storage, Zone, VNet)
	p("  SDN zone %s and VNet %s (no subnet, no host address), then apply the SDN config", Zone, VNet)
	p("  runner template in %s, VMIDs %d-%d reserved for templates", RunnerPool, r.End-config.ReservedVMIDs+1, r.End)
	p("  gateway VM in %s: 1 vCPU, 1 GiB, %d GiB disk, LAN %s%s (%s), %s", SystemPool, GatewayGiB, s.Network.Bridge,
		vlan, s.Network.GatewayIP, VNet)
	p("  controller VM in %s: 2 vCPU, 2 GiB, %d GiB disk, LAN %s%s (%s)", SystemPool, ControllerGiB, s.Network.Bridge,
		vlan, s.Network.ControllerIP)
	wk := s.EffectiveWorker()
	p("Workers: VMIDs %d-%d on %s (%s), at most %d, each %d vCPU and %s", r.Start, r.End-config.ReservedVMIDs, VNet,
		s.Network.WorkerSubnet, s.ScaleSet.MaxRunners, wk.Cores, config.FormatMiB(wk.MemoryMiB))
	target := s.GitHub.ConfigURL
	if target == "" {
		target = "the organization or repository you choose when you create and install the GitHub App"
	}
	p("GitHub: scale set %s for %s", s.ScaleSet.Name, target)
	warnAll(w, warnings, settings.Warnings(s, in.Limits()))
	return nil
}

func warnAll(w io.Writer, checks []Check, more []string) {
	if len(checks) == 0 && len(more) == 0 {
		return
	}
	_, _ = fmt.Fprintln(w, "Warnings:")
	for _, c := range checks {
		_, _ = fmt.Fprintf(w, "  %s: %s\n", c.Desc, c.Reason)
	}
	for _, m := range more {
		_, _ = fmt.Fprintf(w, "  %s\n", m)
	}
}
