package hostsys

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

func testPaths(t *testing.T) Paths {
	dir := t.TempDir()
	return Paths{
		PVEDir:     filepath.Join(dir, "pve"),
		SysNet:     filepath.Join(dir, "sys/class/net"),
		MemInfo:    filepath.Join(dir, "meminfo"),
		Interfaces: filepath.Join(dir, "interfaces"),
		KVM:        filepath.Join(dir, "kvm"),
		UdevRule:   filepath.Join(dir, "90-par-offloads.rules"),
		Ethtool:    filepath.Join(dir, "ethtool"),
	}
}

func TestAddrsAndRoutes(t *testing.T) {
	fake := (&pvecli.FakeExec{}).
		Reply("ip -j -4 addr show", `[{"ifname":"lo","addr_info":[{"local":"127.0.0.1","prefixlen":8}]},
			{"ifname":"vmbr0","addr_info":[{"local":"192.0.2.5","prefixlen":24}]}]`).
		Reply("ip -j -4 route show", `[{"dst":"default","dev":"vmbr0"},{"dst":"192.0.2.0/24","dev":"vmbr0"},
			{"dst":"198.51.100.7","dev":"wg0"}]`)
	s := System{Exec: fake}
	addrs, err := s.Addrs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []Addr{{"lo", netip.MustParsePrefix("127.0.0.1/8")}, {"vmbr0", netip.MustParsePrefix("192.0.2.5/24")}}
	if !slices.Equal(addrs, want) {
		t.Errorf("Addrs = %v", addrs)
	}
	routes, err := s.Routes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wantRoutes := []Route{{"vmbr0", netip.MustParsePrefix("192.0.2.0/24")}, {"wg0", netip.MustParsePrefix("198.51.100.7/32")}}
	if !slices.Equal(routes, wantRoutes) {
		t.Errorf("Routes = %v", routes)
	}
}

// nic makes a NIC in a fake sysfs, on bridge if it isn't empty, with a device bound to driver if it isn't empty.
func nic(t *testing.T, p Paths, name, bridge, driver string) {
	t.Helper()
	must(t, os.MkdirAll(filepath.Join(p.SysNet, name), 0o755))
	if bridge != "" {
		must(t, os.MkdirAll(filepath.Join(p.SysNet, bridge, "bridge"), 0o755))
		must(t, os.MkdirAll(filepath.Join(p.SysNet, bridge, "brif"), 0o755))
		must(t, os.WriteFile(filepath.Join(p.SysNet, bridge, "brif", name), nil, 0o644))
	}
	if driver != "" {
		drv := filepath.Join(filepath.Dir(p.SysNet), "drivers", driver)
		must(t, os.MkdirAll(drv, 0o755))
		must(t, os.MkdirAll(filepath.Join(p.SysNet, name, "device"), 0o755))
		must(t, os.Symlink(drv, filepath.Join(p.SysNet, name, "device", "driver")))
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestSysfs(t *testing.T) {
	p := testPaths(t)
	nic(t, p, "nic0", "vmbr0", "virtio_net")
	nic(t, p, "tap100i0", "vmbr0", "")
	s := System{Paths: p}
	if !s.IsBridge("vmbr0") || s.IsBridge("nic0") {
		t.Error("IsBridge")
	}
	if ports := s.BridgePorts("vmbr0"); !slices.Equal(ports, []string{"nic0", "tap100i0"}) {
		t.Errorf("BridgePorts = %v", ports)
	}
	if s.Driver("nic0") != "virtio_net" || s.Driver("tap100i0") != "" {
		t.Error("Driver")
	}
	if !s.Exists("nic0") || s.Exists("nic9") {
		t.Error("Exists")
	}
}

func TestMemTotal(t *testing.T) {
	p := testPaths(t)
	must(t, os.WriteFile(p.MemInfo, []byte("MemTotal:       65839360 kB\nMemFree:  1 kB\n"), 0o644))
	if mib, err := (System{Paths: p}).MemTotalMiB(); err != nil || mib != 64296 {
		t.Errorf("MemTotalMiB = %d, %v", mib, err)
	}
}

func TestEthtool(t *testing.T) {
	p := testPaths(t)
	s := System{Paths: p, Exec: (&pvecli.FakeExec{}).Reply("ethtool -k nic0", `Features for nic0:
rx-checksumming: on [fixed]
tx-checksumming: on
	tx-checksum-ipv4: off [fixed]
tcp-segmentation-offload: off
	tx-tcp-segmentation: off
generic-receive-offload: on
`)}
	if _, err := s.Ethtool(); !errors.Is(err, ErrNoEthtool) {
		t.Errorf("Ethtool without it = %v", err)
	}
	must(t, os.WriteFile(p.Ethtool, nil, 0o755))
	if path, err := s.Ethtool(); err != nil || path != p.Ethtool {
		t.Errorf("Ethtool = %q, %v", path, err)
	}
	f, err := s.Features(context.Background(), "nic0")
	if err != nil {
		t.Fatal(err)
	}
	if !f["tx-checksumming"] || f["tcp-segmentation-offload"] || !f["generic-receive-offload"] || !f["rx-checksumming"] {
		t.Errorf("Features = %v", f)
	}
	if _, ok := f["tx-tcp-segmentation"]; ok {
		t.Error("sub-features are included")
	}
}
