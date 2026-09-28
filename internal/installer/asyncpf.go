package installer

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
)

// On a node that is itself a VM on KVM, the host sends the node's kernel an async page fault when a page of the
// node's memory isn't ready, because it is swapped out or being moved, so that the node runs something else
// meanwhile. The node runs VMs of its own, the workers, and such a fault can then reach the node's kernel rather than
// a program: the node panics ("Host injected async #PF in kernel mode"), or a task waits forever for a page the host
// never reports ready. Booting with no-kvm-apf turns them off, and the host just pauses the vCPU until the page is
// ready. parcon adds it to the kernel command line, for GRUB in a settings file of its own and for systemd-boot in
// /etc/kernel/cmdline, which has no room for one, and uninstall takes it out again. It takes effect at the next
// boot: parcon never reboots the node.

// noKVMAPF is the kernel parameter that turns async page faults off.
const noKVMAPF = "no-kvm-apf"

// grubDropIn is parcon's GRUB settings file. update-grub reads every file in /etc/default/grub.d after
// /etc/default/grub.
const grubDropIn = "# Written by parcon, and removed by parcon uninstall: on a node that is itself a VM, KVM's\n" +
	"# async page faults can panic it.\n" +
	`GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX ` + noKVMAPF + `"` + "\n"

// AsyncPFState is where turning async page faults off stands.
type AsyncPFState struct {
	// Guest means the node is a VM on KVM; otherwise nothing is needed.
	Guest bool
	// Booted means the running kernel has no-kvm-apf.
	Booted bool
	// Grub and Cmdline are the boot loaders' settings the node has: GRUB's, and systemd-boot's /etc/kernel/cmdline.
	Grub, Cmdline bool
	// Pending is what is left to do for the next boot to have no-kvm-apf.
	Pending []string
	// grub and cmdline mean GRUB's settings and /etc/kernel/cmdline lack it.
	grub, cmdline bool
}

// hasNoKVMAPF reports whether a kernel command line has no-kvm-apf. Only its first line counts, as for
// proxmox-boot-tool.
func hasNoKVMAPF(cmdline string) bool {
	first, _, _ := strings.Cut(cmdline, "\n")
	return slices.Contains(strings.Fields(first), noKVMAPF)
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// AsyncPF reads where turning async page faults off stands.
func (in *Installer) AsyncPF(ctx context.Context) AsyncPFState {
	p := in.Sys.Paths
	st := AsyncPFState{Guest: in.Sys.KVMGuest(ctx)}
	if !st.Guest {
		return st
	}
	running, _ := os.ReadFile(p.ProcCmdline)
	st.Booted = hasNoKVMAPF(string(running))
	if st.Grub = fileExists(p.GrubDefaults); st.Grub {
		have, err := os.ReadFile(p.GrubDropIn)
		cfg, _ := os.ReadFile(p.GrubCfg)
		if err != nil || string(have) != grubDropIn || !strings.Contains(string(cfg), noKVMAPF) {
			st.grub = true
			st.Pending = append(st.Pending, p.GrubDropIn+" and update-grub")
		}
	}
	if kc, err := os.ReadFile(p.KernelCmdline); err == nil {
		st.Cmdline = true
		if !hasNoKVMAPF(string(kc)) {
			st.cmdline = true
			st.Pending = append(st.Pending, p.KernelCmdline)
		}
	}
	if len(st.Pending) > 0 && fileExists(p.BootUUIDs) {
		st.Pending = append(st.Pending, "proxmox-boot-tool refresh")
	}
	return st
}

func (in *Installer) checkAsyncPF(ctx context.Context, mode Mode) result {
	st := in.AsyncPF(ctx)
	switch {
	case !st.Guest:
		return pass("not needed: the node isn't a VM on KVM")
	case !st.Grub && !st.Cmdline:
		return fail("the node is a VM on KVM, and they can panic it, but it has neither GRUB nor %s to turn them "+
			"off with; boot it with %s", in.Sys.Paths.KernelCmdline, noKVMAPF)
	case len(st.Pending) == 0 && st.Booted:
		return pass("off: the node booted with %s", noKVMAPF)
	case len(st.Pending) == 0:
		return fail("%s is set, and turns them off once the node reboots; reboot it when no job runs", noKVMAPF)
	case mode == ModeInstall:
		return pass("the node is a VM on KVM: %s will turn them off from its next boot", noKVMAPF)
	default:
		return fail("the node is a VM on KVM, and they can panic it (%s left to set); parcon install and parcon "+
			"update turn them off", strings.Join(st.Pending, ", "))
	}
}

// asyncPFPlan is the plan's lines for tuneAsyncPF, if it has anything to do.
func (in *Installer) asyncPFPlan(ctx context.Context) []string {
	st := in.AsyncPF(ctx)
	if len(st.Pending) == 0 {
		return nil
	}
	return []string{
		fmt.Sprintf("turn off KVM async page faults from the next boot: add %s to the kernel command line (%s)",
			noKVMAPF, strings.Join(st.Pending, ", ")),
		"  on a node that is itself a VM, they can panic it; parcon doesn't reboot the node, you do",
	}
}

// tuneAsyncPF adds no-kvm-apf to the kernel command line of the node's next boot, if it is a VM on KVM.
func (in *Installer) tuneAsyncPF(ctx context.Context) error {
	st := in.AsyncPF(ctx)
	if !st.Guest || (len(st.Pending) == 0 && st.Booted) {
		return nil
	}
	in.Out.Step("KVM async page faults")
	p := in.Sys.Paths
	switch {
	case !st.Grub && !st.Cmdline:
		in.Out.Warn("the node has neither GRUB nor %s, so async page faults stay on and can panic it; boot it "+
			"with %s", p.KernelCmdline, noKVMAPF)
		return nil
	case len(st.Pending) == 0:
		in.Out.Say("    %s is set; reboot the node when no job runs for it to take effect", noKVMAPF)
		return nil
	}
	if st.grub {
		if have, err := os.ReadFile(p.GrubDropIn); err != nil || string(have) != grubDropIn {
			if err := in.Change.WriteFile(p.GrubDropIn, []byte(grubDropIn), 0o644); err != nil {
				return err
			}
		}
		if err := in.run(ctx, "update-grub"); err != nil {
			return err
		}
	}
	if st.cmdline {
		if err := in.editCmdline(func(params []string) []string { return append(params, noKVMAPF) }); err != nil {
			return err
		}
	}
	if fileExists(p.BootUUIDs) {
		if err := in.run(ctx, "proxmox-boot-tool", "refresh"); err != nil {
			return err
		}
	}
	in.Out.Say("    %s takes effect when the node reboots; reboot it when no job runs", noKVMAPF)
	return nil
}

// editCmdline rewrites the first line of /etc/kernel/cmdline, the one proxmox-boot-tool reads, with edit's
// parameters. The rest of the file and its mode stay.
func (in *Installer) editCmdline(edit func(params []string) []string) error {
	path := in.Sys.Paths.KernelCmdline
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	kc, err := os.ReadFile(path) //nolint:gosec // G304: the host's kernel command line.
	if err != nil {
		return err
	}
	first, rest, _ := strings.Cut(string(kc), "\n")
	return in.Change.WriteFile(path, []byte(strings.Join(edit(strings.Fields(first)), " ")+"\n"+rest),
		info.Mode().Perm())
}

// rebootNote reminds the user to reboot the node when no-kvm-apf is set but the running kernel lacks it.
func (in *Installer) rebootNote(ctx context.Context) {
	if st := in.AsyncPF(ctx); st.Guest && !st.Booted && len(st.Pending) == 0 && (st.Grub || st.Cmdline) {
		in.Out.Say("\nReboot the node when no job runs: KVM async page faults stay on, and can panic it, until "+
			"it boots with %s.", noKVMAPF)
	}
}

// asyncPFRemovePlan is the uninstall plan's line for removeAsyncPF, if there is anything to undo.
func (in *Installer) asyncPFRemovePlan() string {
	p := in.Sys.Paths
	var what []string
	if fileExists(p.GrubDropIn) {
		what = append(what, "remove "+p.GrubDropIn+" and run update-grub")
	}
	if kc, err := os.ReadFile(p.KernelCmdline); err == nil && hasNoKVMAPF(string(kc)) {
		what = append(what, "take "+noKVMAPF+" out of "+p.KernelCmdline)
	}
	if len(what) == 0 {
		return ""
	}
	return strings.Join(what, ", ") + ": KVM async page faults come back on at the next boot"
}

// removeAsyncPF takes no-kvm-apf out of the kernel command line of the next boot.
func (in *Installer) removeAsyncPF(ctx context.Context) error {
	p := in.Sys.Paths
	grub := fileExists(p.GrubDropIn)
	kc, err := os.ReadFile(p.KernelCmdline)
	cmdline := err == nil && hasNoKVMAPF(string(kc))
	if !grub && !cmdline {
		return nil
	}
	in.Out.Step("KVM async page faults")
	if grub {
		if err := in.Change.Remove(p.GrubDropIn); err != nil {
			return err
		}
		if fileExists(p.GrubDefaults) {
			if err := in.run(ctx, "update-grub"); err != nil {
				return err
			}
		}
	}
	if cmdline {
		if err := in.editCmdline(func(params []string) []string {
			return slices.DeleteFunc(params, func(s string) bool { return s == noKVMAPF })
		}); err != nil {
			return err
		}
	}
	if fileExists(p.BootUUIDs) {
		return in.run(ctx, "proxmox-boot-tool", "refresh")
	}
	return nil
}
