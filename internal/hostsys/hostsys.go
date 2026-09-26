// Package hostsys reads facts about the Proxmox host that parcon's host commands check and act on: its addresses
// and routes, its NICs, memory and disk space, clock, Proxmox version, and the certificate its API serves.
//
// Files come from Paths and commands run through a pvecli.Exec, so tests point both at fakes.
package hostsys

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

// Paths are the host files parcon reads and writes. Tests point them at temporary directories.
type Paths struct {
	PVEDir     string // /etc/pve
	SysNet     string // /sys/class/net
	MemInfo    string // /proc/meminfo
	Interfaces string // /etc/network/interfaces
	KVM        string // /dev/kvm
	UdevRule   string // the offload rule, /etc/udev/rules.d/90-par-offloads.rules
	Ethtool    string // /usr/sbin/ethtool
}

// DefaultPaths are the real paths.
func DefaultPaths() Paths {
	return Paths{
		PVEDir:     "/etc/pve",
		SysNet:     "/sys/class/net",
		MemInfo:    "/proc/meminfo",
		Interfaces: "/etc/network/interfaces",
		KVM:        "/dev/kvm",
		UdevRule:   "/etc/udev/rules.d/90-par-offloads.rules",
		Ethtool:    "/usr/sbin/ethtool",
	}
}

// System reads the host.
type System struct {
	Paths Paths
	Exec  pvecli.Exec
}

// Addr is an IPv4 address with its prefix, on an interface.
type Addr struct {
	Dev    string
	Prefix netip.Prefix
}

// Addrs returns the host's IPv4 addresses on every interface, loopback included.
func (s System) Addrs(ctx context.Context) ([]Addr, error) {
	out, err := s.Exec.Run(ctx, pvecli.C("ip", "-j", "-4", "addr", "show"))
	if err != nil {
		return nil, err
	}
	var ifs []struct {
		Name  string `json:"ifname"`
		Addrs []struct {
			Local  string `json:"local"`
			Prefix int    `json:"prefixlen"`
		} `json:"addr_info"`
	}
	if err := json.Unmarshal(out, &ifs); err != nil {
		return nil, fmt.Errorf("ip addr: decode: %w", err)
	}
	var addrs []Addr
	for _, i := range ifs {
		for _, a := range i.Addrs {
			ip, err := netip.ParseAddr(a.Local)
			if err != nil || !ip.Is4() {
				continue
			}
			addrs = append(addrs, Addr{Dev: i.Name, Prefix: netip.PrefixFrom(ip, a.Prefix)})
		}
	}
	return addrs, nil
}

// Route is an IPv4 route other than the default route.
type Route struct {
	Dev string
	Dst netip.Prefix
}

// Routes returns the host's IPv4 routes, except the default route.
func (s System) Routes(ctx context.Context) ([]Route, error) {
	out, err := s.Exec.Run(ctx, pvecli.C("ip", "-j", "-4", "route", "show"))
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Dst string `json:"dst"`
		Dev string `json:"dev"`
	}
	if err := json.Unmarshal(out, &raw); err != nil {
		return nil, fmt.Errorf("ip route: decode: %w", err)
	}
	var routes []Route
	for _, r := range raw {
		if r.Dst == "default" {
			continue
		}
		p, err := netip.ParsePrefix(r.Dst)
		if err != nil {
			a, aerr := netip.ParseAddr(r.Dst)
			if aerr != nil {
				continue
			}
			p = netip.PrefixFrom(a, a.BitLen())
		}
		routes = append(routes, Route{Dev: r.Dev, Dst: p})
	}
	return routes, nil
}

// IsBridge reports whether name is a Linux bridge.
func (s System) IsBridge(name string) bool {
	st, err := os.Stat(filepath.Join(s.Paths.SysNet, name, "bridge"))
	return err == nil && st.IsDir()
}

// BridgePorts returns the interfaces attached to a bridge.
func (s System) BridgePorts(bridge string) []string {
	entries, err := os.ReadDir(filepath.Join(s.Paths.SysNet, bridge, "brif"))
	if err != nil {
		return nil
	}
	ports := make([]string, len(entries))
	for i, e := range entries {
		ports[i] = e.Name()
	}
	return ports
}

// Driver returns the kernel driver of a NIC's device, such as virtio_net, or "" for an interface without a device,
// such as a VM's tap.
func (s System) Driver(nic string) string {
	target, err := os.Readlink(filepath.Join(s.Paths.SysNet, nic, "device", "driver"))
	if err != nil {
		return ""
	}
	return filepath.Base(target)
}

// Exists reports whether a network interface exists.
func (s System) Exists(nic string) bool {
	_, err := os.Stat(filepath.Join(s.Paths.SysNet, nic))
	return err == nil
}

// MemTotalMiB returns the host's memory.
func (s System) MemTotalMiB() (int, error) {
	data, err := os.ReadFile(s.Paths.MemInfo)
	if err != nil {
		return 0, fmt.Errorf("read memory size: %w", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if f := strings.Fields(sc.Text()); len(f) >= 2 && f[0] == "MemTotal:" {
			kib, err := strconv.Atoi(f[1])
			if err != nil {
				break
			}
			return kib / 1024, nil
		}
	}
	return 0, fmt.Errorf("%s has no MemTotal", s.Paths.MemInfo)
}

// CPUs returns the host's CPU threads.
func (s System) CPUs() int { return runtime.NumCPU() }

// FreeGiB returns the space available to root on the filesystem that holds path, in whole GiB.
func (s System) FreeGiB(path string) (int, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, fmt.Errorf("free space in %s: %w", path, err)
	}
	return int(st.Bavail * uint64(st.Bsize) >> 30), nil //nolint:gosec // G115: block sizes are small and positive.
}

// NTPSynced reports whether systemd-timesyncd or chrony has synchronized the clock.
func (s System) NTPSynced(ctx context.Context) bool {
	out, err := s.Exec.Run(ctx, pvecli.C("timedatectl", "show", "-p", "NTPSynchronized", "--value"))
	return err == nil && strings.TrimSpace(string(out)) == "yes"
}

// PVEVersion returns the pve-manager version, such as pve-manager/9.1.2/abcdef.
func (s System) PVEVersion(ctx context.Context) (string, error) {
	out, err := s.Exec.Run(ctx, pvecli.C("pveversion"))
	if err != nil {
		return "", err
	}
	return strings.Fields(string(out) + " ")[0], nil
}

// InContainer reports whether this host is a container rather than a node.
func (s System) InContainer(ctx context.Context) bool {
	_, err := s.Exec.Run(ctx, pvecli.C("systemd-detect-virt", "--container", "--quiet"))
	return err == nil
}

// PackageInstalled reports whether a Debian package is installed.
func (s System) PackageInstalled(ctx context.Context, pkg string) bool {
	out, err := s.Exec.Run(ctx, pvecli.C("dpkg-query", "-W", "-f", "${Status}", pkg))
	return err == nil && strings.Contains(string(out), "install ok installed")
}

// ErrNoEthtool means ethtool isn't installed.
var ErrNoEthtool = errors.New("ethtool isn't installed")

// Ethtool returns ethtool's path, or ErrNoEthtool.
func (s System) Ethtool() (string, error) {
	st, err := os.Stat(s.Paths.Ethtool)
	if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
		return "", ErrNoEthtool
	}
	return s.Paths.Ethtool, nil
}

// Features returns a NIC's offload features as `ethtool -k` names them, each on or off. Sub-features, which ethtool
// indents, are left out.
func (s System) Features(ctx context.Context, nic string) (map[string]bool, error) {
	out, err := s.Exec.Run(ctx, pvecli.C("ethtool", "-k", nic))
	if err != nil {
		return nil, err
	}
	features := map[string]bool{}
	sc := bufio.NewScanner(bytes.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		if line == "" || line[0] == ' ' || line[0] == '\t' {
			continue
		}
		name, value, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		features[name] = strings.HasPrefix(value, "on")
	}
	return features, nil
}
