package installer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// ErrChecksFailed means at least one check failed; the checks said which.
var ErrChecksFailed = errors.New("checks failed")

// settingsOrDefault returns the host settings, or the default install's before an install.
func (in *Installer) settingsOrDefault() (*settings.Settings, error) {
	s, err := settings.Load(in.SettingsPath)
	if errors.Is(err, os.ErrNotExist) {
		d := settings.Default()
		return &d, nil
	}
	return s, err
}

// Check runs `parcon check`: before an install, every check an install runs first, for the default settings; after
// one, the node's checks and the controller's own.
func (in *Installer) Check(ctx context.Context) error {
	s, err := in.settingsOrDefault()
	if err != nil {
		return err
	}
	_, installed, err := in.InstalledRelease(ctx)
	if err != nil {
		return err
	}
	mode := ModeCheck
	if installed {
		mode = ModeInstalled
	}
	in.Out.Step("Node")
	checks := in.Preflight(ctx, s, mode)
	failed := false
	for _, c := range checks {
		in.Out.Say("%s", c)
		failed = failed || !c.OK
	}
	if installed {
		if _, ok, err := in.systemVM(ctx, vmtags.Controller); err != nil {
			return err
		} else if ok {
			for _, target := range []string{"config", "proxmox", "github", "template"} {
				in.Out.Step("Controller: parcon check %s", target)
				if err := in.Forward(ctx, in.Out.W, in.Out.Err, "check", target); err != nil {
					failed = true
					if !errors.Is(err, ErrChecksFailed) {
						in.Out.Say("FAIL  %v", err)
					}
				}
			}
		}
	}
	if failed {
		return ErrChecksFailed
	}
	in.Out.Say("\nAll checks passed.")
	return nil
}

// Forward runs a parcon command in the controller VM, as the parcon user, and passes on its output. A command that
// fails there fails here with ErrChecksFailed.
func (in *Installer) Forward(ctx context.Context, stdout, stderr io.Writer, args ...string) error {
	vm, ok, err := in.systemVM(ctx, vmtags.Controller)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("there is no controller VM; run parcon install")
	}
	if vm.Status != "running" {
		return fmt.Errorf("the controller VM %d isn't running", vm.VMID)
	}
	res, err := in.PVE.GuestExec(ctx, vm.VMID, 2*time.Minute, append([]string{"runuser", "-u", "parcon", "--",
		"parcon"}, args...), nil)
	if err != nil {
		return err
	}
	_, _ = stdout.Write(res.Stdout)
	_, _ = stderr.Write(res.Stderr)
	if res.ExitCode != 0 {
		return ErrChecksFailed
	}
	return nil
}

// CheckNetworkInstalled runs the worker network check on an installed node.
func (in *Installer) CheckNetworkInstalled(ctx context.Context) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	controller, ok, err := in.systemVM(ctx, vmtags.Controller)
	if err != nil {
		return err
	}
	if !ok {
		return errors.New("there is no controller VM; run parcon install")
	}
	addr, err := in.vmIPv4(ctx, controller.VMID)
	if err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return in.CheckNetwork(ctx, s, addr)
}
