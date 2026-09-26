package installer

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// destroyBuildVMs destroys the smoke-test clones, including ones a failed run left.
func (in *Installer) destroyBuildVMs(ctx context.Context) error {
	vms, err := in.vms(ctx)
	if err != nil {
		return err
	}
	for _, vm := range tagged(vms, RunnerPool, vmtags.Build) {
		if err := in.destroyVM(ctx, vm.VMID); err != nil {
			return err
		}
	}
	return nil
}

// CheckNetwork clones a worker from the newest template into a reserved VMID and checks from inside it that it gets
// a DHCP lease and reaches GitHub, and can't reach the Proxmox API or the controller VM. The clone is tagged Build,
// so the controller leaves it alone, and is always destroyed.
func (in *Installer) CheckNetwork(ctx context.Context, s *settings.Settings, controller netip.Addr) (err error) {
	in.Out.Step("Worker network check")
	if err := in.destroyBuildVMs(ctx); err != nil {
		return err
	}
	vms, err := in.vms(ctx)
	if err != nil {
		return err
	}
	template, ok := vmtags.NewestTemplate(vms, RunnerPool)
	if !ok {
		return errors.New("there is no runner template to clone")
	}
	vmid, err := freeReservedVMID(vms, s.Proxmox.VMIDRange)
	if err != nil {
		return err
	}
	id := strconv.Itoa(vmid)
	if err := in.run(ctx, "qm", "clone", strconv.Itoa(template.VMID), id, "--name", "par-smoke-test", "--pool",
		RunnerPool); err != nil {
		return err
	}
	defer func() {
		if derr := in.destroyVM(ctx, vmid); derr != nil && err == nil {
			err = derr
		}
	}()
	if err := in.run(ctx, "qm", "set", id, "--tags", vmtags.Managed+";"+vmtags.Build, "--ciupgrade", "0"); err != nil {
		return err
	}
	if err := in.run(ctx, "qm", "start", id); err != nil {
		return err
	}
	if in.Change.DryRun {
		return nil
	}
	if err := in.waitBooted(ctx, vmid); err != nil {
		return err
	}
	pve, err := in.pveAddress(ctx, s)
	if err != nil {
		return err
	}
	var failures []string
	if _, err := in.vmIPv4(ctx, vmid); err != nil {
		failures = append(failures, "the worker got no DHCP lease")
	}
	if _, err := in.guestExec(ctx, vmid, 30*time.Second, nil, "curl", "-sS", "-o", "/dev/null", "--max-time", "15",
		"https://api.github.com/"); err != nil {
		failures = append(failures, "the worker can't reach GitHub")
	}
	if _, err := in.guestExec(ctx, vmid, 30*time.Second, nil, "curl", "-sk", "-o", "/dev/null", "--max-time", "5",
		"https://"+netip.AddrPortFrom(pve, 8006).String()+"/"); err == nil {
		failures = append(failures, "the worker can reach the Proxmox API")
	}
	if controller.IsValid() {
		if _, err := in.guestExec(ctx, vmid, 30*time.Second, nil, "timeout", "5", "bash", "-c",
			"</dev/tcp/"+controller.String()+"/22"); err == nil {
			failures = append(failures, "the worker can reach the controller VM")
		}
	}
	if len(failures) > 0 {
		return fmt.Errorf("worker network check: %s", strings.Join(failures, "; "))
	}
	in.Out.Say("    the worker got a lease, reaches GitHub, and can't reach the Proxmox API or the controller")
	return nil
}

// destroyVM stops and destroys a VM. One that is already gone counts as destroyed.
func (in *Installer) destroyVM(ctx context.Context, vmid int) error {
	id := strconv.Itoa(vmid)
	vms, err := in.vms(ctx)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		if vm.VMID != vmid {
			continue
		}
		if vm.Status == "running" {
			_ = in.run(ctx, "qm", "stop", id, "--skiplock", "1")
		}
		if err := in.run(ctx, "qm", "destroy", id, "--purge", "1"); err != nil {
			return fmt.Errorf("destroying VM %d failed: %w", vmid, err)
		}
	}
	return nil
}
