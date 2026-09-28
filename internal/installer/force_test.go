package installer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// sameRelease makes ti an update of its node to the release it is already at, served signed as a real update
// would be, with the controller VM able to download its parcon.
func sameRelease(t *testing.T, ti *testInstaller) release.Version {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	src := &signedSource{files: map[string][]byte{}}
	v := src.publish(t, priv, ti.Version.String())
	ti.Source, ti.Keys, ti.Assets, ti.Yes = src, []ed25519.PublicKey{pub}, "", true
	ti.Self = ti.BinaryPath
	ti.node.guest = func(vm *fakeVM, argv []string, _ []byte) (int, string, string, bool) {
		if argv[0] == "sh" && strings.Contains(argv[2], "curl -fsSL --retry 3 -o /usr/local/bin/parcon.new") {
			vm.files["parcon-version"] = v.String()
			return 0, "", "", true
		}
		return 0, "", "", false
	}
	return v
}

func TestUpgradeForceRunsEveryStep(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	ctx := context.Background()
	templates := ti.node.vmsTagged(vmtags.Template)

	// Without --force, the node is at the release.
	must(t, ti.Upgrade(ctx, v, true, false))
	if !strings.Contains(ti.stdout.String(), "nothing needs updating") {
		t.Fatalf("without --force:\n%s", ti.stdout)
	}

	ti.stdout.Reset()
	must(t, ti.Upgrade(ctx, v, true, true))
	out := ti.stdout.String()
	for _, want := range []string{"import the 0.2.0 runner template again", "replace gateway VM 100 with the new image",
		"replace parcon 0.2.0 in controller VM 101 with 0.2.0", "render the controller's config",
		"check the LAN NIC's offloads", "check KVM async page faults", "tag controller VM 101 with release 0.2.0",
		"importing a new one anyway"} {
		if !strings.Contains(out, want) {
			t.Errorf("forced update lacks %q:\n%s", want, out)
		}
	}
	n := ti.node
	// The old template stays for the controller to remove once unused; a new one joins it.
	if now := n.vmsTagged(vmtags.Template); len(now) != len(templates)+1 || !slices.Contains(now, templates[0]) {
		t.Errorf("templates %v, then %v", templates, now)
	}
	if !n.ran("qm destroy 100 --purge 1 --destroy-unreferenced-disks 1") || !n.vms[100].running {
		t.Errorf("the gateway wasn't replaced: %v", n.lines())
	}
	if !n.ran("qm guest exec 101 --timeout 480 -- systemctl try-restart parcon.service") {
		t.Errorf("the controller wasn't restarted: %v", n.lines())
	}
}

// An update that stopped while it replaced the gateway left a VM with the release's tag but no disk and no
// address: --force replaces it all the same, with the address from the settings.
func TestUpgradeForceReplacesAHalfCreatedGateway(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	gw := ti.node.vms[100]
	want := gw.config["ipconfig0"]
	for _, k := range []string{"scsi0", "ide2", "boot", "ipconfig0"} {
		delete(gw.config, k)
	}

	must(t, ti.Upgrade(context.Background(), v, true, true))
	gw = ti.node.vms[100]
	if gw == nil || gw.config["scsi0"] == "" || gw.config["ipconfig0"] != want || !gw.running {
		t.Errorf("gateway after --force: %+v, want ipconfig0 %q", gw, want)
	}
}

func TestUpgradeForceCreatesAMissingGateway(t *testing.T) {
	ti := installed(t)
	v := sameRelease(t, ti)
	delete(ti.node.vms, 100)
	ctx := context.Background()

	if err := ti.Upgrade(ctx, v, true, false); err == nil || !strings.Contains(err.Error(), "parcon update --force") {
		t.Fatalf("without --force: %v", err)
	}
	must(t, ti.Upgrade(ctx, v, true, true))
	if g := ti.node.vmsTagged(vmtags.Gateway); len(g) != 1 || !ti.node.vms[g[0]].running {
		t.Errorf("gateways after --force: %v", g)
	}
	if !strings.Contains(ti.stdout.String(), "create the gateway VM from the new image") {
		t.Errorf("output:\n%s", ti.stdout)
	}
}

func TestUpdateForcePassesItOnToTheNewParcon(t *testing.T) {
	ti := installed(t)
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	src := &signedSource{files: map[string][]byte{}}
	src.publish(t, priv, "0.3.0")
	ti.Source, ti.Keys, ti.Assets, ti.Yes = src, []ed25519.PublicKey{pub}, "", true
	var execed []string
	execBinary = func(path string, args []string) error { execed = append([]string{path}, args...); return nil }
	t.Cleanup(func() { execBinary = defaultExecBinary })

	must(t, ti.Update(context.Background(), UpdateOptions{Force: true}))
	if !slices.Equal(execed, []string{ti.BinaryPath, ti.BinaryPath, "update", "--continue", "--yes", "--force"}) {
		t.Errorf("exec %v", execed)
	}
}
