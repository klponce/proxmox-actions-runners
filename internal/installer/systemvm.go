package installer

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// vmSpec is a gateway or controller VM's hardware and identity.
type vmSpec struct {
	name             string
	cores, memoryMiB int
	diskGiB          int
	tags             []string
	net0, ipconfig0  string
	net1             string
}

// gatewaySpec is the gateway VM's hardware, defined in this one place: 1 vCPU and 1 GiB are plenty for NAT, DHCP,
// and DNS. net0, ipconfig0, and net1 come from the caller, since a replaced gateway keeps the old one's NICs.
func (in *Installer) gatewaySpec(v release.Version, net0, ipconfig0, net1 string) vmSpec {
	return vmSpec{name: "par-gateway", cores: 1, memoryMiB: 1024, diskGiB: GatewayGiB,
		tags: []string{vmtags.Managed, vmtags.Gateway, vmtags.ReleasePrefix + v.String()},
		net0: net0, ipconfig0: ipconfig0, net1: net1}
}

func (in *Installer) controllerSpec(s *settings.Settings) vmSpec {
	return vmSpec{name: "par-controller", cores: 2, memoryMiB: 2048,
		diskGiB: ControllerGiB, tags: []string{vmtags.Managed, vmtags.Controller},
		net0: netConfig(s), ipconfig0: ipConfig(s, s.Network.ControllerIP)}
}

// netConfig is a virtio NIC on the LAN bridge, with the VLAN tag if set.
func netConfig(s *settings.Settings) string {
	c := "virtio,bridge=" + s.Network.Bridge
	if s.Network.VLAN != 0 {
		c += ",tag=" + strconv.Itoa(s.Network.VLAN)
	}
	return c
}

// ipConfig is cloud-init's ipconfig for dhcp or a static address.
func ipConfig(s *settings.Settings, addr string) string {
	if addr == settings.DHCP {
		return "ip=dhcp"
	}
	return "ip=" + addr + ",gw=" + s.Network.LANGateway
}

// createSystemVM creates the gateway or controller VM from its image and starts it.
func (in *Installer) createSystemVM(ctx context.Context, vmid int, spec vmSpec, image, storage string) error {
	id := strconv.Itoa(vmid)
	args := []string{"create", id, "--name", spec.name, "--pool", SystemPool, "--memory", strconv.Itoa(spec.memoryMiB),
		"--cores", strconv.Itoa(spec.cores), "--cpu", "host", "--ostype", "l26", "--scsihw", "virtio-scsi-single",
		"--net0", spec.net0, "--agent", "enabled=1", "--onboot", "1", "--serial0", "socket", "--vga", "serial0",
		"--tags", strings.Join(spec.tags, ";")}
	if spec.net1 != "" {
		args = append(args, "--net1", spec.net1)
	}
	for _, a := range [][]string{
		args,
		{"set", id, "--scsi0", storage + ":0,import-from=" + image},
		{"set", id, "--ide2", storage + ":cloudinit", "--boot", "order=scsi0", "--ipconfig0", spec.ipconfig0,
			"--ciupgrade", "0"},
		{"start", id},
	} {
		if err := in.run(ctx, "qm", a...); err != nil {
			return err
		}
	}
	return nil
}

// systemVMID returns a free VMID for the gateway or controller VM outside the workers' and templates' range: the
// one Proxmox suggests, or the first free ID above the range when that falls inside it.
func (in *Installer) systemVMID(ctx context.Context, s *settings.Settings) (int, error) {
	id, err := in.PVE.NextID(ctx)
	if err != nil {
		return 0, err
	}
	r := s.Proxmox.VMIDRange
	if !r.Contains(id) {
		return id, nil
	}
	vms, err := in.vms(ctx)
	if err != nil {
		return 0, err
	}
	id = r.End + 1
	for slices.ContainsFunc(vms, func(vm proxmox.VM) bool { return vm.VMID == id }) {
		id++
	}
	return id, nil
}

// existingSystemVM returns the gateway or controller VM, if it exists. One that a failed run left without its disk
// is destroyed, and one that is stopped is started.
func (in *Installer) existingSystemVM(ctx context.Context, tag string) (int, bool, error) {
	vm, ok, err := in.systemVM(ctx, tag)
	if err != nil || !ok {
		return 0, false, err
	}
	node, err := in.nodeName(ctx)
	if err != nil {
		return 0, false, err
	}
	cfg, err := in.PVE.VMConfig(ctx, node, vm.VMID)
	if err != nil {
		return 0, false, err
	}
	id := strconv.Itoa(vm.VMID)
	if _, ok := cfg["scsi0"]; !ok {
		return 0, false, in.run(ctx, "qm", "destroy", id, "--purge", "1")
	}
	if vm.Status != "running" {
		if err := in.run(ctx, "qm", "start", id); err != nil {
			return 0, false, err
		}
	}
	return vm.VMID, true, nil
}

// createGateway creates the gateway VM unless it exists, and configures it.
func (in *Installer) createGateway(ctx context.Context, s *settings.Settings, image func() (string, error)) (int, error) {
	in.Out.Step("Gateway VM")
	vmid, ok, err := in.existingSystemVM(ctx, vmtags.Gateway)
	if err != nil {
		return 0, err
	}
	if !ok {
		if vmid, err = in.systemVMID(ctx, s); err != nil {
			return 0, err
		}
		path, err := image()
		if err != nil {
			return 0, err
		}
		spec := in.gatewaySpec(in.Version, netConfig(s), ipConfig(s, s.Network.GatewayIP), "virtio,bridge="+VNet)
		if err := in.createSystemVM(ctx, vmid, spec, path, s.Proxmox.Storage); err != nil {
			return 0, err
		}
	}
	if err := in.waitBooted(ctx, vmid); err != nil {
		return 0, err
	}
	return vmid, in.configureGateway(ctx, vmid, s, netip.Addr{})
}

// gatewayBlock returns what workers must not reach: the host's networks on the LAN bridge, every address the host
// has on any interface (the API listens on all of them), and the controller VM once it has an address. The gateway
// itself always blocks RFC 1918, CGNAT, and link-local ranges.
func (in *Installer) gatewayBlock(ctx context.Context, s *settings.Settings, controller netip.Addr) ([]string, error) {
	addrs, err := in.Sys.Addrs(ctx)
	if err != nil {
		return nil, err
	}
	var block []string
	for _, a := range addrs {
		if a.Dev == s.Network.Bridge {
			block = append(block, a.Prefix.Masked().String())
		}
		if !a.Prefix.Addr().IsLoopback() {
			block = append(block, a.Prefix.Addr().String())
		}
	}
	if pve, err := in.pveAddress(ctx, s); err == nil {
		block = append(block, pve.String())
	}
	if controller.IsValid() {
		block = append(block, controller.String())
	}
	if p, err := netip.ParsePrefix(s.Network.ControllerIP); err == nil {
		block = append(block, p.Addr().String())
	}
	slices.Sort(block)
	return slices.Compact(block), nil
}

// configureGateway gives the gateway its settings through par-gateway-configure, then checks that it reaches GitHub.
func (in *Installer) configureGateway(ctx context.Context, vmid int, s *settings.Settings, controller netip.Addr) error {
	block, err := in.gatewayBlock(ctx, s, controller)
	if err != nil {
		return err
	}
	in.Change.Log(fmt.Sprintf("gateway VM %d: par-gateway-configure (worker subnet %s, blocking %s)", vmid,
		s.Network.WorkerSubnet, strings.Join(block, " ")))
	if in.Change.DryRun {
		return nil
	}
	input := fmt.Sprintf("WORKER_SUBNET=%s\nBLOCK=%s\n", s.Network.WorkerSubnet, strings.Join(block, " "))
	if _, err := in.guestExec(ctx, vmid, 3*time.Minute, []byte(input), "/usr/local/sbin/par-gateway-configure"); err != nil {
		return fmt.Errorf("configuring the gateway failed: %w", err)
	}
	if _, err := in.guestExec(ctx, vmid, 30*time.Second, nil, "curl", "-sS", "-o", "/dev/null", "--max-time", "10",
		"https://api.github.com/"); err != nil {
		return fmt.Errorf("the gateway VM can't reach GitHub: %w", err)
	}
	return nil
}

// createController creates the controller VM unless it exists, waits for its address, and has the gateway block it.
func (in *Installer) createController(ctx context.Context, s *settings.Settings, gateway int,
	image func() (string, error),
) (int, netip.Addr, error) {
	in.Out.Step("Controller VM")
	vmid, ok, err := in.existingSystemVM(ctx, vmtags.Controller)
	if err != nil {
		return 0, netip.Addr{}, err
	}
	if !ok {
		if vmid, err = in.systemVMID(ctx, s); err != nil {
			return 0, netip.Addr{}, err
		}
		path, err := image()
		if err != nil {
			return 0, netip.Addr{}, err
		}
		if err := in.createSystemVM(ctx, vmid, in.controllerSpec(s), path, s.Proxmox.Storage); err != nil {
			return 0, netip.Addr{}, err
		}
	}
	if err := in.waitBooted(ctx, vmid); err != nil {
		return 0, netip.Addr{}, err
	}
	addr, err := in.vmIPv4(ctx, vmid)
	if err != nil {
		return 0, netip.Addr{}, fmt.Errorf("the controller VM got no IPv4 address: %w", err)
	}
	in.Out.Say("    controller %d at %s", vmid, addr)
	// The gateway was configured before the controller had an address; block it now.
	return vmid, addr, in.configureGateway(ctx, gateway, s, addr)
}
