package installer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/release"
)

// kvmGuest makes the node a VM on KVM, booted with running as its kernel command line. grub gives it GRUB's
// settings, a non-empty cmdline gives it /etc/kernel/cmdline, and pbt has proxmox-boot-tool keep ESPs in sync.
func (ti *testInstaller) kvmGuest(t *testing.T, running string, grub bool, cmdline string, pbt bool) {
	t.Helper()
	p := ti.Sys.Paths
	ti.node.kvmGuest = true
	write := func(path, content string) {
		must(t, os.MkdirAll(filepath.Dir(path), 0o755))
		must(t, os.WriteFile(path, []byte(content), 0o644))
	}
	write(p.ProcCmdline, running+"\n")
	if grub {
		write(p.GrubDefaults, `GRUB_CMDLINE_LINUX_DEFAULT="quiet"`+"\n")
		must(t, os.MkdirAll(filepath.Dir(p.GrubDropIn), 0o755))
		write(p.GrubCfg, "linux /boot/vmlinuz-7.0.2-6-pve "+running+"\n")
	}
	if cmdline != "" {
		write(p.KernelCmdline, cmdline)
	}
	if pbt {
		write(p.BootUUIDs, "ABCD-0123\n")
	}
}

// reboot boots the node with the command line its boot loader has now.
func (ti *testInstaller) reboot(t *testing.T) {
	t.Helper()
	p := ti.Sys.Paths
	running := "BOOT_IMAGE=/boot/vmlinuz-7.0.2-6-pve root=/dev/mapper/pve-root ro quiet"
	if cfg, err := os.ReadFile(p.GrubCfg); err == nil && strings.Contains(string(cfg), noKVMAPF) {
		running += " " + noKVMAPF
	}
	if kc, err := os.ReadFile(p.KernelCmdline); err == nil {
		running, _, _ = strings.Cut(string(kc), "\n")
	}
	must(t, os.WriteFile(p.ProcCmdline, []byte(running+"\n"), 0o644))
}

func TestAsyncPFOnANodeThatIsNotAKVMGuest(t *testing.T) {
	ti := newTestInstaller(t)
	ctx := context.Background()
	if c := ti.checkAsyncPF(ctx, ModeCheck); !c.ok || c.reason != "not needed: the node isn't a VM on KVM" {
		t.Errorf("check = %+v", c)
	}
	if plan := ti.asyncPFPlan(ctx); plan != nil {
		t.Errorf("plan = %q", plan)
	}
	must(t, ti.tuneAsyncPF(ctx))
	if ti.stdout.Len() != 0 || ti.node.ran("update-grub") {
		t.Errorf("changed:\n%s", ti.stdout)
	}
}

func TestAsyncPFWithGRUB(t *testing.T) {
	ti := newTestInstaller(t)
	ti.kvmGuest(t, "BOOT_IMAGE=/boot/vmlinuz-7.0.2-6-pve root=/dev/mapper/pve-root ro quiet", true, "", false)
	ctx := context.Background()
	p := ti.Sys.Paths

	if c := ti.checkAsyncPF(ctx, ModeInstall); !c.ok || !strings.Contains(c.reason, "no-kvm-apf will turn them off") {
		t.Errorf("install check = %+v", c)
	}
	if c := ti.checkAsyncPF(ctx, ModeCheck); c.ok || !strings.Contains(c.reason, "they can panic it ("+p.GrubDropIn+
		" and update-grub left to set)") {
		t.Errorf("check before = %+v", c)
	}
	if plan := ti.asyncPFPlan(ctx); len(plan) != 2 || !strings.Contains(plan[0], "add no-kvm-apf to the kernel "+
		"command line ("+p.GrubDropIn+" and update-grub)") {
		t.Errorf("plan = %q", plan)
	}

	must(t, ti.tuneAsyncPF(ctx))
	if dropIn, _ := os.ReadFile(p.GrubDropIn); !strings.Contains(string(dropIn),
		`GRUB_CMDLINE_LINUX="$GRUB_CMDLINE_LINUX no-kvm-apf"`) {
		t.Errorf("drop-in = %s", dropIn)
	}
	if !ti.node.ran("update-grub") || ti.node.ran("proxmox-boot-tool") {
		t.Errorf("calls: %v", ti.node.lines())
	}
	if !strings.Contains(ti.stdout.String(), "no-kvm-apf takes effect when the node reboots") {
		t.Errorf("output:\n%s", ti.stdout)
	}

	// Set, but the node hasn't rebooted: nothing left to do, and the check says to reboot.
	if plan := ti.asyncPFPlan(ctx); plan != nil {
		t.Errorf("plan after = %q", plan)
	}
	if c := ti.checkAsyncPF(ctx, ModeInstalled); c.ok || !strings.Contains(c.reason, "reboot it when no job runs") {
		t.Errorf("check after = %+v", c)
	}
	ti.stdout.Reset()
	ti.rebootNote(ctx)
	if !strings.Contains(ti.stdout.String(), "Reboot the node when no job runs") {
		t.Errorf("reboot note:\n%s", ti.stdout)
	}

	// Rebooted: the check passes, and another run changes nothing.
	ti.reboot(t)
	if c := ti.checkAsyncPF(ctx, ModeInstalled); !c.ok || c.reason != "off: the node booted with no-kvm-apf" {
		t.Errorf("check after reboot = %+v", c)
	}
	ti.node.calls = nil
	ti.stdout.Reset()
	must(t, ti.tuneAsyncPF(ctx))
	ti.rebootNote(ctx)
	if ti.stdout.Len() != 0 || ti.node.ran("update-grub") {
		t.Errorf("second run:\n%s", ti.stdout)
	}

	// Uninstall removes the drop-in and regenerates GRUB's config.
	if line := ti.asyncPFRemovePlan(); !strings.Contains(line, "remove "+p.GrubDropIn+" and run update-grub") {
		t.Errorf("remove plan = %q", line)
	}
	ti.node.calls = nil
	must(t, ti.removeAsyncPF(ctx))
	if _, err := os.Stat(p.GrubDropIn); !errors.Is(err, os.ErrNotExist) {
		t.Error("drop-in left")
	}
	if cfg, _ := os.ReadFile(p.GrubCfg); !ti.node.ran("update-grub") || strings.Contains(string(cfg), noKVMAPF) {
		t.Errorf("grub.cfg = %s; calls: %v", cfg, ti.node.lines())
	}
	if line := ti.asyncPFRemovePlan(); line != "" {
		t.Errorf("remove plan after = %q", line)
	}
}

func TestAsyncPFWithSystemdBoot(t *testing.T) {
	ti := newTestInstaller(t)
	const cmdline = "root=ZFS=rpool/ROOT/pve-1 boot=zfs\n"
	ti.kvmGuest(t, "initrd=\\EFI\\proxmox\\initrd.img root=ZFS=rpool/ROOT/pve-1 boot=zfs", false, cmdline, true)
	p := ti.Sys.Paths
	must(t, os.Chmod(p.KernelCmdline, 0o600))
	ctx := context.Background()

	if plan := ti.asyncPFPlan(ctx); len(plan) != 2 || !strings.Contains(plan[0],
		"("+p.KernelCmdline+", proxmox-boot-tool refresh)") {
		t.Errorf("plan = %q", plan)
	}
	must(t, ti.tuneAsyncPF(ctx))
	if kc, _ := os.ReadFile(p.KernelCmdline); string(kc) != "root=ZFS=rpool/ROOT/pve-1 boot=zfs no-kvm-apf\n" {
		t.Errorf("cmdline = %q", kc)
	}
	if st, _ := os.Stat(p.KernelCmdline); st.Mode().Perm() != 0o600 {
		t.Errorf("cmdline mode = %v", st.Mode())
	}
	if !ti.node.ran("proxmox-boot-tool refresh") || ti.node.ran("update-grub") {
		t.Errorf("calls: %v", ti.node.lines())
	}
	ti.reboot(t)
	if c := ti.checkAsyncPF(ctx, ModeInstalled); !c.ok {
		t.Errorf("check after reboot = %+v", c)
	}

	// Uninstall takes the parameter out again, and leaves the rest of the line as it was.
	if line := ti.asyncPFRemovePlan(); !strings.Contains(line, "take no-kvm-apf out of "+p.KernelCmdline) {
		t.Errorf("remove plan = %q", line)
	}
	ti.node.calls = nil
	must(t, ti.removeAsyncPF(ctx))
	if kc, _ := os.ReadFile(p.KernelCmdline); string(kc) != cmdline {
		t.Errorf("cmdline after uninstall = %q", kc)
	}
	if !ti.node.ran("proxmox-boot-tool refresh") {
		t.Errorf("calls: %v", ti.node.lines())
	}
}

func TestAsyncPFWithoutABootLoader(t *testing.T) {
	ti := newTestInstaller(t)
	ti.kvmGuest(t, "root=/dev/vda1", false, "", false)
	ctx := context.Background()
	if c := ti.checkAsyncPF(ctx, ModeInstall); c.ok || !strings.Contains(c.reason, "has neither GRUB nor") {
		t.Errorf("check = %+v", c)
	}
	must(t, ti.tuneAsyncPF(ctx))
	if !strings.Contains(ti.stderr.String(), "async page faults stay on") {
		t.Errorf("stderr = %s", ti.stderr)
	}
}

func TestAsyncPFDryRunChangesNothing(t *testing.T) {
	ti := newTestInstaller(t)
	ti.kvmGuest(t, "root=/dev/mapper/pve-root ro quiet", true, "", false)
	ti.Change.DryRun = true
	must(t, ti.tuneAsyncPF(context.Background()))
	if _, err := os.Stat(ti.Sys.Paths.GrubDropIn); !errors.Is(err, os.ErrNotExist) || ti.node.ran("update-grub") {
		t.Error("a dry run changed the node")
	}
}

// The update after a release with this fix sets no-kvm-apf on a node that is already at the release otherwise.
func TestUpgradeTurnsAsyncPFOff(t *testing.T) {
	ti := installed(t)
	ti.kvmGuest(t, "root=/dev/mapper/pve-root ro quiet", true, "", false)
	ti.Self = ti.BinaryPath
	ti.stdout.Reset()
	v, _ := release.ParseVersion("0.2.0")
	must(t, ti.Upgrade(context.Background(), v, true))
	out := ti.stdout.String()
	for _, want := range []string{"turn off KVM async page faults from the next boot", "$ update-grub",
		"Reboot the node when no job runs"} {
		if !strings.Contains(out, want) {
			t.Errorf("update lacks %q:\n%s", want, out)
		}
	}
	if _, err := os.Stat(ti.Sys.Paths.GrubDropIn); err != nil {
		t.Error(err)
	}
}
