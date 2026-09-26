package installer

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// On a node that is itself a VM, the LAN bridge's port is a virtio NIC, and its offloads are on by default. The NIC
// merges incoming packets (GRO) that the bridge then forwards to the gateway VM, and workers' downloads stall at a
// few KB/s while the host's own run at full speed. parcon turns those offloads off, now with ethtool and at each
// boot through a udev rule of its own, and uninstall turns them back on.

// offloads are the offloads turned off: their names in `ethtool -K`, and in `ethtool -k`.
var offloads = []struct{ flag, feature string }{
	{"gro", "generic-receive-offload"},
	{"gso", "generic-segmentation-offload"},
	{"tso", "tcp-segmentation-offload"},
	{"tx", "tx-checksumming"},
}

func offloadArgs(nic, state string) []string {
	args := []string{"-K", nic}
	for _, o := range offloads {
		args = append(args, o.flag, state)
	}
	return args
}

func offloadFlags() string {
	flags := make([]string, len(offloads))
	for i, o := range offloads {
		flags[i] = o.flag
	}
	return strings.Join(flags, " ")
}

// virtioPorts returns the LAN bridge's ports that are virtio NICs, which it has when the node is itself a VM.
func virtioPorts(sys hostsys.System, bridge string) []string {
	var ports []string
	for _, p := range sys.BridgePorts(bridge) {
		if sys.Driver(p) == "virtio_net" {
			ports = append(ports, p)
		}
	}
	return ports
}

// offloadRule is the udev rule that turns the offloads off on nics each time one appears. It matches NAME, not
// KERNEL, and sorts after 80-net-setup-link.rules, because that renames the NIC (to nic0, say) in the same event,
// and RUN runs once it has.
func offloadRule(ethtool string, nics []string) string {
	var b strings.Builder
	b.WriteString("# Written by parcon, and removed by parcon uninstall.\n")
	for _, nic := range nics {
		fmt.Fprintf(&b, "ACTION==\"add\", SUBSYSTEM==\"net\", NAME==\"%s\", RUN+=\"%s %s\"\n", nic, ethtool,
			strings.Join(offloadArgs(nic, "off"), " "))
	}
	return b.String()
}

var ruleNIC = regexp.MustCompile(`NAME=="([^"]*)"`)

// ruleNICs returns the NICs an offload rule names.
func ruleNICs(rule string) []string {
	var nics []string
	for _, m := range ruleNIC.FindAllStringSubmatch(rule, -1) {
		nics = append(nics, m[1])
	}
	return nics
}

// OffloadState is where the offload tuning stands.
type OffloadState struct {
	// NICs are the LAN bridge's virtio NICs; none means the node isn't a VM and nothing is needed.
	NICs []string
	// NoEthtool means ethtool is missing, so nothing can be done.
	NoEthtool bool
	// Pending is what is left to do: the udev rule, and each NIC with offloads on.
	Pending []string
	ethtool string
	rule    string
}

// Offloads reads where the offload tuning stands.
func (in *Installer) Offloads(ctx context.Context, s *settings.Settings) OffloadState {
	st := OffloadState{NICs: virtioPorts(in.Sys, s.Network.Bridge)}
	if len(st.NICs) == 0 {
		return st
	}
	ethtool, err := in.Sys.Ethtool()
	if err != nil {
		st.NoEthtool = true
		return st
	}
	st.ethtool, st.rule = ethtool, offloadRule(ethtool, st.NICs)
	if have, err := os.ReadFile(in.Sys.Paths.UdevRule); err != nil || string(have) != st.rule {
		st.Pending = append(st.Pending, "udev rule "+in.Sys.Paths.UdevRule)
	}
	for _, nic := range st.NICs {
		features, err := in.Sys.Features(ctx, nic)
		if err != nil {
			st.Pending = append(st.Pending, nic+": can't read its offloads")
			continue
		}
		var on []string
		for _, o := range offloads {
			if features[o.feature] {
				on = append(on, o.feature)
			}
		}
		if len(on) > 0 {
			st.Pending = append(st.Pending, fmt.Sprintf("%s: %s on", nic, strings.Join(on, " ")))
		}
	}
	return st
}

func (in *Installer) checkOffloads(ctx context.Context, s *settings.Settings, mode Mode) result {
	st := in.Offloads(ctx, s)
	switch {
	case len(st.NICs) == 0:
		return pass("not needed: no virtio NIC on %s", s.Network.Bridge)
	case st.NoEthtool:
		return fail("ethtool isn't installed, so they can't be turned off")
	case len(st.Pending) == 0:
		return pass("off on %s", strings.Join(st.NICs, " "))
	case mode != ModeInstall:
		return fail("the node is a VM, and they aren't off for good (%s); parcon install and parcon update turn "+
			"them off", strings.Join(st.Pending, "; "))
	default:
		return pass("the node is a VM: they will be turned off on %s", strings.Join(st.NICs, " "))
	}
}

// offloadPlan is the plan's lines for tuneOffloads, if it has anything to do.
func (in *Installer) offloadPlan(ctx context.Context, s *settings.Settings) []string {
	st := in.Offloads(ctx, s)
	if len(st.NICs) == 0 || st.NoEthtool || len(st.Pending) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("turn off offloads (%s) on %s, the virtio NIC under %s, now and at each boot", offloadFlags(),
			strings.Join(st.NICs, " "), s.Network.Bridge),
		fmt.Sprintf("  (udev rule %s): on a node that is itself a VM, they stall workers' downloads",
			in.Sys.Paths.UdevRule),
	}
}

// tuneOffloads turns the LAN bridge's virtio NICs' offloads off, now and at each boot.
func (in *Installer) tuneOffloads(ctx context.Context, s *settings.Settings) error {
	st := in.Offloads(ctx, s)
	if len(st.NICs) == 0 {
		return nil
	}
	in.Out.Step("LAN NIC offloads")
	if st.NoEthtool {
		in.Out.Warn("ethtool isn't installed, so the offloads of %s stay on and workers' downloads may be slow",
			strings.Join(st.NICs, " "))
		return nil
	}
	if have, err := os.ReadFile(in.Sys.Paths.UdevRule); err != nil || string(have) != st.rule {
		if err := in.Change.WriteFile(in.Sys.Paths.UdevRule, []byte(st.rule), 0o644); err != nil {
			return err
		}
	}
	for _, nic := range st.NICs {
		features, err := in.Sys.Features(ctx, nic)
		if err != nil {
			return err
		}
		for _, o := range offloads {
			if features[o.feature] {
				if _, err := in.Change.Run(ctx, pvecli.C(st.ethtool, offloadArgs(nic, "off")...)); err != nil {
					return err
				}
				break
			}
		}
	}
	return nil
}

// offloadRemovePlan is the uninstall plan's line for removeOffloads, if there is a rule.
func (in *Installer) offloadRemovePlan() string {
	rule, err := os.ReadFile(in.Sys.Paths.UdevRule)
	if err != nil {
		return ""
	}
	return fmt.Sprintf("remove the udev rule %s and turn the offloads of %s back on", in.Sys.Paths.UdevRule,
		strings.Join(ruleNICs(string(rule)), " "))
}

// removeOffloads removes the udev rule and turns the offloads back on for the NICs it names.
func (in *Installer) removeOffloads(ctx context.Context) error {
	rule, err := os.ReadFile(in.Sys.Paths.UdevRule)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	in.Out.Step("LAN NIC offloads")
	ethtool, ethErr := in.Sys.Ethtool()
	for _, nic := range ruleNICs(string(rule)) {
		if ethErr != nil || !in.Sys.Exists(nic) {
			continue
		}
		if _, err := in.Change.Run(ctx, pvecli.C(ethtool, offloadArgs(nic, "on")...)); err != nil {
			in.Out.Warn("turning the offloads of %s back on failed: %v", nic, err)
		}
	}
	return in.Change.Remove(in.Sys.Paths.UdevRule)
}
