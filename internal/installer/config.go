package installer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// ErrUnchanged is a config set to the value it already has.
var ErrUnchanged = errors.New("unchanged")

// Settings returns the host settings.
func (in *Installer) Settings() (*settings.Settings, error) { return in.loadSettings() }

// SetConfig sets a config key and applies it: the controller gets the new config, checked by its own parcon before
// it replaces the old one, and restarts. Running workers keep going; a restarted controller adopts them. The
// settings change only once the controller has the new config. An invalid value is a *settings.ValueError.
func (in *Installer) SetConfig(ctx context.Context, key, value string) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	k, err := settings.Lookup(key)
	if err != nil {
		return err
	}
	before, _ := k.Get(s)
	if err := settings.SetKey(s, key, value, in.Limits()); err != nil {
		return err
	}
	after, _ := k.Get(s)
	if after == before {
		in.Out.Say("%s is already %s", key, after)
		return ErrUnchanged
	}
	for _, w := range settings.Warnings(s, in.Limits()) {
		in.Out.Warn("%s", w)
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	in.Out.Say("%s: %s -> %s", key, before, after)
	return in.apply(ctx, s)
}

// ApplyConfig pushes the settings to the controller again and restarts it, for a controller whose config drifted
// from the settings, or after the API's pinned certificate was renewed.
func (in *Installer) ApplyConfig(ctx context.Context) error {
	s, err := in.loadSettings()
	if err != nil {
		return err
	}
	unlock, err := in.Lock()
	if err != nil {
		return err
	}
	defer unlock()
	return in.apply(ctx, s)
}

// apply saves the settings and, if there is a controller, pushes its config and restarts it.
func (in *Installer) apply(ctx context.Context, s *settings.Settings) error {
	vm, ok, err := in.systemVM(ctx, vmtags.Controller)
	if err != nil {
		return err
	}
	if !ok || vm.Status != "running" {
		// Before the controller exists, or while it is stopped, the settings alone change; install and the next
		// apply bring the config along.
		in.Out.Warn("there is no running controller VM: the settings are saved, and the controller gets them " +
			"from parcon install or parcon config apply")
		return in.saveSettings(s)
	}
	if err := in.sameVersion(ctx, vm.VMID); err != nil {
		return err
	}
	if err := in.pushConfig(ctx, vm.VMID, s); err != nil {
		return err
	}
	if err := in.saveSettings(s); err != nil {
		return fmt.Errorf("the controller has the new config, but saving the settings failed; run parcon config "+
			"apply once it is fixed: %w", err)
	}
	return in.restartController(ctx, vm.VMID)
}

// sameVersion refuses to push a config to a controller that runs another parcon than this one: the config's format
// follows the host's parcon, and the controller's may not read it.
func (in *Installer) sameVersion(ctx context.Context, vmid int) error {
	out, err := in.asParcon(ctx, vmid, guestTimeout, nil, "parcon", "version")
	if err != nil {
		return fmt.Errorf("ask the controller's parcon for its version: %w", err)
	}
	if v := strings.TrimSpace(string(out)); v != in.Version.String() {
		return fmt.Errorf("the controller runs parcon %s and this host parcon %s; run parcon update first", v,
			in.Version)
	}
	return nil
}

// restartController restarts parcon.service if it runs, and waits for its scale set session.
func (in *Installer) restartController(ctx context.Context, vmid int) error {
	in.Change.Log(fmt.Sprintf("controller VM %d: systemctl try-restart parcon.service", vmid))
	if in.Change.DryRun {
		return nil
	}
	restarted := in.Now()
	if _, err := in.guestExec(ctx, vmid, ControllerStopTimeout+time.Minute, nil, "systemctl", "try-restart",
		"parcon.service"); err != nil {
		return fmt.Errorf("restarting the controller failed: %w", err)
	}
	if _, err := in.guestExec(ctx, vmid, guestTimeout, nil, "systemctl", "is-active", "parcon.service"); err != nil {
		in.Out.Say("The controller isn't running yet; it gets the new config when parcon install starts it.")
		return nil //nolint:nilerr // A controller that isn't enabled yet is fine.
	}
	if err := in.waitSession(ctx, vmid, restarted, 5*time.Minute); err != nil {
		return err
	}
	in.Out.Say("The controller restarted with the new config and is listening for jobs.")
	return nil
}
