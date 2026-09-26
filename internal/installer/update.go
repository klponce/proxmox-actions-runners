package installer

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// UpdateOptions are the choices for an update.
type UpdateOptions struct {
	// Pre includes pre-releases when looking for the newest release.
	Pre bool
	// Continue means this parcon was just installed by an update, which the user confirmed: it brings the node to
	// its own release without looking for a newer one.
	Continue bool
}

// execBinary replaces this process with another program. Tests replace it.
var execBinary = func(path string, args []string) error {
	return syscall.Exec(path, args, os.Environ()) //nolint:gosec // G204: the verified parcon just installed.
}

// Update updates the install to the newest release. A newer release than this parcon is verified and installed
// first, then run to do the rest, so each release's own code updates the node to it. Running it again continues an
// update that stopped partway.
func (in *Installer) Update(ctx context.Context, opts UpdateOptions) error {
	installed, ok, err := in.InstalledRelease(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("there is no finished install on this node; run parcon install")
	}
	if !opts.Continue && in.Assets == "" {
		in.Out.Step("Releases")
		latest, err := release.Latest(ctx, in.Source, opts.Pre)
		if err != nil {
			return fmt.Errorf("find the newest release: %w", err)
		}
		if latest.Version.Compare(in.Version) > 0 {
			return in.updateSelf(ctx, installed, latest.Version, opts)
		}
		in.Out.Say("The newest release is %s, and this parcon is %s.", latest.Version, in.Version)
	}
	return in.Upgrade(ctx, installed, opts.Continue)
}

// updateSelf installs the verified parcon of release v and runs it to update the node.
func (in *Installer) updateSelf(ctx context.Context, installed, v release.Version, opts UpdateOptions) error {
	in.Out.Step("Plan")
	in.Out.Say("Update proxmox-actions-runners %s to %s:", installed, v)
	in.Out.Say("  replace %s with parcon %s, from the release, once its signature checks out", in.BinaryPath, v)
	in.Out.Say("  then, run by parcon %s: its runner template, gateway VM, and controller, as it plans them", v)
	if in.Change.DryRun {
		return nil
	}
	if err := in.confirm(fmt.Sprintf("Update to %s?", v)); err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	in.Out.Step("parcon %s", v)
	dir, err := os.MkdirTemp(DownloadDir, "par-update.")
	if err != nil {
		unlock()
		return fmt.Errorf("make a download directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(dir) }()
	assets, err := release.Fetch(ctx, in.Source, v, dir, in.Keys)
	if err == nil {
		var bin string
		if bin, err = in.get(ctx, assets, BinaryAsset(v)); err == nil {
			in.Change.Log("install " + BinaryAsset(v) + " " + in.BinaryPath)
			err = release.ReplaceBinary(bin, in.BinaryPath)
		}
	}
	// The new parcon takes the lock itself.
	unlock()
	if err != nil {
		return err
	}
	args := []string{in.BinaryPath, "update", "--continue", "--yes"}
	if opts.Pre {
		args = append(args, "--pre")
	}
	return execBinary(in.BinaryPath, args)
}

// upgradeStep is one thing an upgrade does.
type upgradeStep struct {
	plan string
	do   func(ctx context.Context) error
}

// Upgrade brings the node from release installed to this parcon's release: the runner template, the gateway VM,
// the controller's parcon and config, and the host's NIC offloads. Each piece already at this release is left alone.
func (in *Installer) Upgrade(ctx context.Context, installed release.Version, confirmed bool) error {
	if installed.Compare(in.Version) > 0 {
		return fmt.Errorf("the node runs release %s, newer than this parcon %s; run parcon update", installed,
			in.Version)
	}
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	steps, err := in.upgradeSteps(ctx, s)
	if err != nil {
		return err
	}
	in.Out.Step("Plan")
	if len(steps) == 0 {
		in.Out.Say("proxmox-actions-runners is at %s, and nothing needs updating.", in.Version)
		return nil
	}
	in.Out.Say("Update proxmox-actions-runners %s to %s:", installed, in.Version)
	for _, st := range steps {
		in.Out.Say("  %s", st.plan)
	}
	if in.Change.DryRun {
		return nil
	}
	if !confirmed {
		if err := in.confirm("Update with this plan?"); err != nil {
			return err
		}
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	for _, st := range steps {
		if err := st.do(ctx); err != nil {
			return err
		}
	}
	in.Out.Step("Done")
	in.Out.Say("proxmox-actions-runners is at %s.", in.Version)
	return nil
}

// upgradeSteps works out what an upgrade to this release has to do. None means the node is at this release.
func (in *Installer) upgradeSteps(ctx context.Context, s *settings.Settings) ([]upgradeStep, error) {
	vms, err := in.vms(ctx)
	if err != nil {
		return nil, err
	}
	gateway, ok := findSystemVM(vms, vmtags.Gateway)
	if !ok {
		return nil, errors.New("the install has no gateway VM; run parcon uninstall and parcon install")
	}
	controller, ok := findSystemVM(vms, vmtags.Controller)
	if !ok || controller.Status != "running" {
		return nil, errors.New("the controller VM isn't running; start it, then run parcon update again")
	}
	hasTemplate := false
	for _, vm := range tagged(vms, RunnerPool, in.releaseTag()) {
		hasTemplate = hasTemplate || vm.Template
	}
	out, err := in.asParcon(ctx, controller.VMID, guestTimeout, nil, "parcon", "version")
	if err != nil {
		return nil, fmt.Errorf("ask the controller's parcon for its version: %w", err)
	}
	controllerVersion := strings.TrimSpace(string(out))
	want, _, err := in.RenderConfig(ctx, s)
	if err != nil {
		return nil, err
	}
	have, err := in.asParcon(ctx, controller.VMID, guestTimeout, nil, "cat", config.DefaultPath)
	configDrift := err != nil || strings.TrimSpace(string(have)) != strings.TrimSpace(string(want))
	needBinary := controllerVersion != in.Version.String()
	offloads := in.offloadPlan(ctx, s)

	// The release's assets are fetched once, by the first step that needs them, and removed by the last step.
	var assets *release.Assets
	cleanup := func() {}
	fetch := func(ctx context.Context) (*release.Assets, error) {
		if assets == nil {
			in.Out.Step("Release %s", in.Version)
			a, c, err := in.assets(ctx)
			if err != nil {
				return nil, err
			}
			assets, cleanup = a, c
		}
		return assets, nil
	}

	var steps []upgradeStep
	if !sameFile(in.Self, in.BinaryPath) {
		steps = append(steps, upgradeStep{plan: fmt.Sprintf("install parcon %s as %s", in.Version, in.BinaryPath),
			do: func(context.Context) error { return in.installBinary() }})
	}
	if !hasTemplate {
		steps = append(steps, upgradeStep{
			plan: fmt.Sprintf("import the %s runner template; the controller removes old ones once unused", in.Version),
			do: func(ctx context.Context) error {
				a, err := fetch(ctx)
				if err != nil {
					return err
				}
				return in.importTemplate(ctx, s, a)
			},
		})
	}
	if !gateway.HasTag(in.releaseTag()) {
		steps = append(steps, upgradeStep{
			plan: fmt.Sprintf("replace gateway VM %d with the new image (workers lose their network for a minute "+
				"or two)", gateway.VMID),
			do: func(ctx context.Context) error {
				a, err := fetch(ctx)
				if err != nil {
					return err
				}
				return in.replaceGateway(ctx, gateway.VMID, controller.VMID, s, a)
			},
		})
	}
	if needBinary {
		steps = append(steps, upgradeStep{
			plan: fmt.Sprintf("replace parcon %s in controller VM %d with %s", controllerVersion, controller.VMID,
				in.Version),
			do: func(ctx context.Context) error {
				a, err := fetch(ctx)
				if err != nil {
					return err
				}
				return in.upgradeControllerBinary(ctx, controller.VMID, a)
			},
		})
	}
	if needBinary || configDrift {
		steps = append(steps, upgradeStep{
			plan: "render the controller's config from the settings, restart the controller, and wait for its session",
			do: func(ctx context.Context) error {
				in.Out.Step("Controller config")
				if err := in.pushConfig(ctx, controller.VMID, s); err != nil {
					return err
				}
				return in.restartController(ctx, controller.VMID)
			},
		})
	}
	if offloads != nil {
		steps = append(steps, upgradeStep{plan: strings.Join(offloads, "\n  "),
			do: func(ctx context.Context) error { return in.tuneOffloads(ctx, s) }})
	}
	if len(steps) == 0 && controller.HasTag(in.releaseTag()) {
		return nil, nil
	}
	// The release tag goes on the controller last: it marks a finished update.
	return append(steps, upgradeStep{
		plan: fmt.Sprintf("tag controller VM %d with release %s", controller.VMID, in.Version),
		do: func(ctx context.Context) error {
			defer cleanup()
			tags := strings.Join(append(in.controllerSpec(s).tags, in.releaseTag()), ";")
			return in.run(ctx, "qm", "set", strconv.Itoa(controller.VMID), "--tags", tags)
		},
	}), nil
}

// replaceGateway recreates the gateway VM from the new image with the old one's VMID, NICs (their MACs too), and
// address, and configures it. The gateway holds no state beyond what the settings say.
func (in *Installer) replaceGateway(ctx context.Context, vmid, controller int, s *settings.Settings,
	a *release.Assets,
) error {
	in.Out.Step("Replace the gateway VM")
	image, err := in.get(ctx, a, gatewayImage(in.Version))
	if err != nil {
		return err
	}
	node, err := in.nodeName(ctx)
	if err != nil {
		return err
	}
	cfg, err := in.PVE.VMConfig(ctx, node, vmid)
	if err != nil {
		return err
	}
	id := strconv.Itoa(vmid)
	if err := in.run(ctx, "qm", "stop", id); err != nil {
		return err
	}
	if err := in.run(ctx, "qm", "destroy", id, "--purge", "1"); err != nil {
		return err
	}
	spec := in.gatewaySpec(in.Version, cfg["net0"], cfg["ipconfig0"], cfg["net1"])
	if err := in.createSystemVM(ctx, vmid, spec, image, s.Proxmox.Storage); err != nil {
		return err
	}
	if err := in.waitBooted(ctx, vmid); err != nil {
		return err
	}
	addr, _ := in.agentIPv4(ctx, controller)
	return in.configureGateway(ctx, vmid, s, addr)
}

// upgradeControllerBinary has the controller VM download the release's parcon itself, check it against the
// checksum from the verified SHA256SUMS, and put it in place. The binary is far too big for the guest agent to carry,
// and the controller's config and secrets never leave the VM. The service restarts with the new config next.
func (in *Installer) upgradeControllerBinary(ctx context.Context, vmid int, a *release.Assets) error {
	in.Out.Step("Upgrade the controller's parcon")
	name := BinaryAsset(in.Version)
	sum, ok := a.Sums.Hex(name)
	if !ok {
		return fmt.Errorf("release %s has no %s", in.Version, name)
	}
	if in.Assets != "" {
		return fmt.Errorf("the controller VM downloads its parcon from the release, which local assets can't give it")
	}
	in.Change.Log(fmt.Sprintf("controller VM %d: download %s, check its sha256, install it as /usr/local/bin/parcon",
		vmid, name))
	if in.Change.DryRun {
		return nil
	}
	_, err := in.guestExec(ctx, vmid, 5*time.Minute, nil, "sh", "-c", `set -e
		curl -fsSL --retry 3 -o /usr/local/bin/parcon.new "$1"
		echo "$2  /usr/local/bin/parcon.new" | sha256sum -c --quiet -
		chmod 0755 /usr/local/bin/parcon.new
		mv /usr/local/bin/parcon.new /usr/local/bin/parcon`, "sh", release.AssetURL(in.Version, name), sum)
	if err != nil {
		return fmt.Errorf("upgrading the controller's parcon failed: %w", err)
	}
	return nil
}

// sameFile reports whether two paths hold the same bytes: the host's parcon is this one.
func sameFile(a, b string) bool {
	if a == b {
		return true
	}
	da, err1 := os.ReadFile(a) //nolint:gosec // G304: parcon's own paths.
	db, err2 := os.ReadFile(b) //nolint:gosec // G304: parcon's own paths.
	return err1 == nil && err2 == nil && bytes.Equal(da, db)
}
