package installer

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// Level is how much a failed check matters.
type Level int

const (
	// Hard checks stop an install.
	Hard Level = iota
	// Warn checks are shown in the plan and need its confirmation.
	Warn
)

// Check is the result of one check.
type Check struct {
	Level  Level
	Desc   string
	OK     bool
	Reason string
}

// String is the check's line of output.
func (c Check) String() string {
	label := "ok   "
	switch {
	case !c.OK && c.Level == Hard:
		label = "FAIL "
	case !c.OK:
		label = "WARN "
	}
	if c.Reason == "" {
		return label + " " + c.Desc
	}
	return label + " " + c.Desc + ": " + c.Reason
}

// Mode says what the checks are for.
type Mode int

const (
	// ModeInstall checks a node before installing, or before continuing an install.
	ModeInstall Mode = iota
	// ModeCheck checks a node for `parcon check`, before an install.
	ModeCheck
	// ModeInstalled checks a node with a finished install for `parcon check`. The free space an install needs is
	// no longer the point, and the names and VMIDs are ours.
	ModeInstalled
)

// passed is a check result: reason is shown either way.
type result struct {
	ok     bool
	reason string
}

func pass(format string, args ...any) result { return result{true, fmt.Sprintf(format, args...)} }
func fail(format string, args ...any) result { return result{false, fmt.Sprintf(format, args...)} }

// Preflight runs every check, in order, and returns their results. It changes nothing.
func (in *Installer) Preflight(ctx context.Context, s *settings.Settings, mode Mode) []Check {
	type check struct {
		level Level
		desc  string
		fn    func() result
	}
	checks := []check{
		{Hard, "running as root", checkRoot},
		{Hard, "Proxmox VE 9.x", func() result { return in.checkPVE9(ctx) }},
		{Hard, "running on a Proxmox VE node", func() result { return in.checkPVENode(ctx) }},
		{Hard, "KVM available", func() result { return checkExists(in.Sys.Paths.KVM) }},
		{Hard, "required tools present", checkTools},
		{Hard, "standalone node", func() result { return in.checkStandalone() }},
		{Hard, "clock synchronized", func() result { return in.checkClock(ctx) }},
		{Hard, "outbound HTTPS to GitHub", func() result { return in.checkHTTPS(ctx) }},
		{Hard, "storage " + s.Proxmox.Storage, func() result { return in.checkStorage(ctx, s) }},
		{Warn, "linked clones on " + s.Proxmox.Storage, func() result { return in.checkLinkedClones(ctx, s) }},
		{Hard, "free space on " + s.Proxmox.Storage, func() result { return in.checkFreeSpace(ctx, s) }},
		{Hard, "free space for downloads in " + DownloadDir, func() result { return in.checkDownloadSpace() }},
		{Warn, "memory", func() result { return in.checkMemory(ctx, s) }},
		{Hard, "LAN bridge " + s.Network.Bridge, func() result { return in.checkBridge(ctx, s) }},
		{Warn, "LAN NIC offloads", func() result { return in.checkOffloads(ctx, s, mode) }},
		{Warn, "API certificate", func() result { return in.checkTLS() }},
		{Hard, "SDN available", func() result { return in.checkSDN(ctx) }},
		{Hard, "worker subnet " + s.Network.WorkerSubnet + " free", func() result { return in.checkWorkerSubnet(ctx, s) }},
		{Hard, "names free", func() result { return in.checkNames(ctx, mode) }},
		{Hard, fmt.Sprintf("VMIDs %d-%d free", s.Proxmox.VMIDRange.Start, s.Proxmox.VMIDRange.End),
			func() result { return in.checkVMIDs(ctx, s) }},
		{Hard, "no pending SDN changes", func() result { return in.checkPendingSDN(ctx) }},
	}
	out := make([]Check, 0, len(checks))
	for _, c := range checks {
		if mode == ModeInstalled && strings.HasPrefix(c.desc, "free space") {
			continue
		}
		r := c.fn()
		out = append(out, Check{Level: c.level, Desc: c.desc, OK: r.ok, Reason: r.reason})
	}
	return out
}

// Failed reports whether any hard check failed.
func Failed(checks []Check) bool {
	return slices.ContainsFunc(checks, func(c Check) bool { return !c.OK && c.Level == Hard })
}

// Warnings returns the warn checks that failed.
func Warnings(checks []Check) []Check {
	var w []Check
	for _, c := range checks {
		if !c.OK && c.Level == Warn {
			w = append(w, c)
		}
	}
	return w
}

// geteuid is the effective user ID. Tests replace it.
var geteuid = os.Geteuid

func checkRoot() result {
	if geteuid() != 0 {
		return fail("run it as root")
	}
	return pass("")
}

func (in *Installer) checkPVE9(ctx context.Context) result {
	v, err := in.Sys.PVEVersion(ctx)
	if err != nil {
		return fail("pveversion not found")
	}
	if !strings.HasPrefix(v, "pve-manager/9.") {
		return fail("%s", v)
	}
	return pass("%s", v)
}

func (in *Installer) checkPVENode(ctx context.Context) result {
	if st, err := os.Stat(filepath.Join(in.Sys.Paths.PVEDir, "nodes")); err != nil || !st.IsDir() {
		return fail("%s isn't mounted", in.Sys.Paths.PVEDir)
	}
	if in.Sys.InContainer(ctx) {
		return fail("running in a container")
	}
	return pass("")
}

func checkExists(path string) result {
	if _, err := os.Stat(path); err != nil {
		return fail("%s is missing", path)
	}
	return pass("")
}

// lookPath finds a command. Tests replace it.
var lookPath = exec.LookPath

func checkTools() result {
	var missing []string
	for _, tool := range []string{"qm", "pveum", "pvesh", "pvesm", "ip"} {
		if _, err := lookPath(tool); err != nil {
			missing = append(missing, tool)
		}
	}
	if len(missing) > 0 {
		return fail("missing %s", strings.Join(missing, " "))
	}
	return pass("")
}

func (in *Installer) checkStandalone() result {
	if _, err := os.Stat(filepath.Join(in.Sys.Paths.PVEDir, "corosync.conf")); err == nil {
		return fail("this node is in a cluster; clusters aren't supported")
	}
	return pass("")
}

func (in *Installer) checkClock(ctx context.Context) result {
	if !in.Sys.NTPSynced(ctx) {
		return fail("the clock isn't synchronized; GitHub App tokens fail when it drifts")
	}
	return pass("")
}

// httpsHosts are what the installer and the controller reach: GitHub, its API, and the release asset hosts.
var httpsHosts = []string{"github.com", "api.github.com", "objects.githubusercontent.com",
	"release-assets.githubusercontent.com"}

func reachHTTPS(ctx context.Context, host string) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "https://"+host+"/", nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

func (in *Installer) checkHTTPS(ctx context.Context) result {
	var failed []string
	for _, h := range httpsHosts {
		if in.Reach(ctx, h) != nil {
			failed = append(failed, h)
		}
	}
	if len(failed) > 0 {
		return fail("can't reach %s", strings.Join(failed, " "))
	}
	return pass("")
}

func (in *Installer) storage(ctx context.Context, s *settings.Settings) (pvecli.StorageStatus, error) {
	node, err := in.nodeName(ctx)
	if err != nil {
		return pvecli.StorageStatus{}, err
	}
	st, err := in.PVE.Storage(ctx, node, s.Proxmox.Storage)
	if err != nil {
		return pvecli.StorageStatus{}, fmt.Errorf("no storage %s on this node", s.Proxmox.Storage)
	}
	return st, nil
}

func (in *Installer) checkStorage(ctx context.Context, s *settings.Settings) result {
	st, err := in.storage(ctx, s)
	switch {
	case err != nil:
		return fail("%v", err)
	case st.Active == 0 || st.Enabled == 0:
		return fail("%s isn't active", s.Proxmox.Storage)
	case !slices.Contains(strings.Split(st.Content, ","), "images"):
		return fail("%s doesn't accept VM disks (content images)", s.Proxmox.Storage)
	}
	return pass("%s", st.Type)
}

// linkedCloneTypes are the storage types that can make linked clones.
var linkedCloneTypes = []string{"lvmthin", "zfspool", "rbd", "dir", "nfs", "cifs", "glusterfs", "btrfs"}

func (in *Installer) linkedClones(ctx context.Context, s *settings.Settings) (bool, string) {
	st, err := in.storage(ctx, s)
	if err != nil {
		return false, ""
	}
	return slices.Contains(linkedCloneTypes, st.Type), st.Type
}

func (in *Installer) checkLinkedClones(ctx context.Context, s *settings.Settings) result {
	if ok, typ := in.linkedClones(ctx, s); !ok {
		return fail("%s storage can't make linked clones; workers will be full clones (slower, more space)", typ)
	}
	return pass("")
}

func (in *Installer) checkFreeSpace(ctx context.Context, s *settings.Settings) result {
	st, err := in.storage(ctx, s)
	if err != nil {
		return fail("%v", err)
	}
	avail := int(st.Avail >> 30)
	return result{avail >= FreeGiB, fmt.Sprintf("%d GiB free, %d GiB needed", avail, FreeGiB)}
}

func (in *Installer) checkDownloadSpace() result {
	avail, err := in.Sys.FreeGiB(DownloadDir)
	if err != nil {
		return fail("%v", err)
	}
	return result{avail >= DownloadGiB, fmt.Sprintf("%d GiB free, %d GiB needed", avail, DownloadGiB)}
}

// systemVMsMiB is the memory of the gateway and controller VMs.
const systemVMsMiB = 1024 + 2048

func (in *Installer) checkMemory(ctx context.Context, s *settings.Settings) result {
	total, err := in.Sys.MemTotalMiB()
	if err != nil {
		return fail("%v", err)
	}
	vms, err := in.vms(ctx)
	if err != nil {
		return fail("%v", err)
	}
	var used int64
	for _, vm := range vms {
		// Our own gateway and controller are counted with systemVMsMiB, and workers come and go.
		if !vm.Template && vm.Pool != SystemPool && vm.Pool != RunnerPool {
			used += vm.MaxMemBytes
		}
	}
	need := int(used>>20) + s.ScaleSet.MaxRunners*s.EffectiveWorker().MemoryMiB + systemVMsMiB
	return result{total >= need, fmt.Sprintf("%d MiB RAM, %d MiB allocated to VMs with %d workers", total, need,
		s.ScaleSet.MaxRunners)}
}

func (in *Installer) checkBridge(ctx context.Context, s *settings.Settings) result {
	if !in.Sys.IsBridge(s.Network.Bridge) {
		return fail("no bridge %s", s.Network.Bridge)
	}
	a, err := in.pveAddress(ctx, s)
	if err != nil {
		return fail("%v", err)
	}
	return pass("%s", a)
}

func (in *Installer) checkTLS() result {
	info, err := in.Sys.TLS(in.Roots)
	if err != nil {
		return fail("%v", err)
	}
	switch info.Mode {
	case hostsys.TLSNodeCA:
		return pass("the node's own certificate, verified against the node's CA")
	case hostsys.TLSSystem:
		return pass("a certificate the system CAs trust, verified as %s", info.ServerName)
	default:
		return fail("a certificate from a CA the host doesn't trust: the controller pins it, and after it is " +
			"renewed, run parcon config apply to pin the new one")
	}
}

var sourceInterfacesD = regexp.MustCompile(`(?m)^[ \t]*source[ \t]+/etc/network/interfaces\.d/\*`)

func (in *Installer) checkSDN(ctx context.Context) result {
	if !in.Sys.PackageInstalled(ctx, "ifupdown2") {
		return fail("ifupdown2 isn't installed")
	}
	data, err := os.ReadFile(in.Sys.Paths.Interfaces)
	if err != nil || !sourceInterfacesD.Match(data) {
		return fail("%s doesn't source /etc/network/interfaces.d/*", in.Sys.Paths.Interfaces)
	}
	return pass("")
}

func (in *Installer) checkWorkerSubnet(ctx context.Context, s *settings.Settings) result {
	subnet, err := netip.ParsePrefix(s.Network.WorkerSubnet)
	if err != nil {
		return fail("%v", err)
	}
	addrs, err := in.Sys.Addrs(ctx)
	if err != nil {
		return fail("%v", err)
	}
	routes, err := in.Sys.Routes(ctx)
	if err != nil {
		return fail("%v", err)
	}
	var taken []netip.Prefix
	for _, a := range addrs {
		taken = append(taken, a.Prefix)
	}
	for _, r := range routes {
		// Our own VNet, from an earlier run, carries no address, but a route for it would be ours.
		if r.Dev == VNet {
			continue
		}
		taken = append(taken, r.Dst)
	}
	for _, p := range taken {
		if subnet.Overlaps(p) {
			return fail("%s overlaps %s on this host; choose another with --worker-subnet", subnet, p)
		}
	}
	for _, v := range []string{s.Network.GatewayIP, s.Network.ControllerIP} {
		if p, err := netip.ParsePrefix(v); err == nil && subnet.Overlaps(p) {
			return fail("%s overlaps the LAN %s", subnet, p)
		}
	}
	return pass("")
}

// ours reports whether an earlier run created our objects: it creates the system pool first, with PoolComment.
func (in *Installer) ours(ctx context.Context) (bool, error) {
	pools, err := in.PVE.Pools(ctx)
	if err != nil {
		return false, err
	}
	for _, p := range pools {
		if p.ID == SystemPool && p.Comment == PoolComment {
			return true, nil
		}
	}
	return false, nil
}

func (in *Installer) checkNames(ctx context.Context, mode Mode) result {
	if ours, err := in.ours(ctx); err != nil {
		return fail("%v", err)
	} else if ours && mode == ModeInstall {
		return pass("found an earlier install; continuing it")
	} else if ours {
		return pass("ours")
	}
	var taken []string
	if in.PVE.SDNExists(ctx, "zones/"+Zone) {
		taken = append(taken, "SDN zone "+Zone)
	}
	if in.PVE.SDNExists(ctx, "vnets/"+VNet) {
		taken = append(taken, "SDN VNet "+VNet)
	}
	pools, _ := in.PVE.Pools(ctx)
	for _, p := range pools {
		if p.ID == RunnerPool || p.ID == SystemPool {
			taken = append(taken, "pool "+p.ID)
		}
	}
	roles, _ := in.PVE.Roles(ctx)
	if slices.ContainsFunc(roles, func(r pvecli.Role) bool { return r.ID == Role }) {
		taken = append(taken, "role "+Role)
	}
	users, _ := in.PVE.Users(ctx)
	if slices.Contains(users, PVEUser) {
		taken = append(taken, "user "+PVEUser)
	}
	if len(taken) > 0 {
		return fail("already exist and weren't created by parcon: %s", strings.Join(taken, ", "))
	}
	return pass("")
}

func (in *Installer) checkVMIDs(ctx context.Context, s *settings.Settings) result {
	vms, err := in.vms(ctx)
	if err != nil {
		return fail("%v", err)
	}
	var clash []string
	for _, vm := range vms {
		if s.Proxmox.VMIDRange.Contains(vm.VMID) && vm.Pool != RunnerPool {
			clash = append(clash, fmt.Sprint(vm.VMID))
		}
	}
	if len(clash) > 0 {
		r := s.Proxmox.VMIDRange
		return fail("VMIDs %s in %d-%d belong to other VMs; choose another range with --vmid-range",
			strings.Join(clash, " "), r.Start, r.End)
	}
	return pass("")
}

func (in *Installer) checkPendingSDN(ctx context.Context) result {
	pending, err := in.PVE.PendingSDN(ctx, Zone, VNet)
	if err != nil {
		return fail("%v", err)
	}
	if len(pending) > 0 {
		return fail("pending SDN changes to %s would be applied too; apply or revert them first",
			strings.Join(pending, " "))
	}
	return pass("")
}

// errPreflight is a failed hard check.
var errPreflight = errors.New("preflight checks failed")
