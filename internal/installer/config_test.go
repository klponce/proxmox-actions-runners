package installer

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/controller"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

// installed returns a test installer on a node with a finished install.
func installed(t *testing.T) *testInstaller {
	t.Helper()
	ti := newTestInstaller(t)
	ti.answerManifest()
	must(t, ti.Install(context.Background(), defaultOptions()))
	ti.stdout.Reset()
	ti.stderr.Reset()
	ti.node.calls = nil
	return ti
}

func TestSetConfig(t *testing.T) {
	ti := installed(t)
	ctx := context.Background()
	must(t, ti.SetConfig(ctx, "worker.memory", "16GiB"))

	s, err := settings.Load(ti.SettingsPath)
	if err != nil || s.Worker.MemoryMiB != 16384 {
		t.Fatalf("settings = %+v, %v", s, err)
	}
	cfg, err := config.Parse([]byte(ti.controller(t).files[config.DefaultPath]))
	if err != nil || cfg.ScaleSets[0].Worker.MemoryMiB != 16384 {
		t.Errorf("controller config: %+v, %v", cfg, err)
	}
	for _, want := range []string{"parcon check config -config /etc/proxmox-actions-runners/config.yaml.new",
		"systemctl try-restart parcon.service", "journalctl -u parcon.service"} {
		if !ti.node.ran("qm guest exec 101") || !strings.Contains(strings.Join(ti.node.lines(), "\n"), want) {
			t.Errorf("didn't run %s", want)
		}
	}
	if out := ti.stdout.String(); !strings.Contains(out, "worker.memory: 8GiB -> 16GiB") ||
		!strings.Contains(out, "listening for jobs") {
		t.Errorf("output:\n%s", out)
	}

	// The same value again changes nothing.
	ti.node.calls = nil
	if err := ti.SetConfig(ctx, "worker.memory", "16384MiB"); !errors.Is(err, ErrUnchanged) || len(ti.node.calls) > 5 {
		t.Errorf("unchanged: %v, calls %v", err, ti.node.lines())
	}

	// A refused value is a ValueError and changes nothing.
	var verr *settings.ValueError
	if err := ti.SetConfig(ctx, "worker.memory", "16GB"); !errors.As(err, &verr) {
		t.Errorf("16GB: %v", err)
	}
	if err := ti.SetConfig(ctx, "worker.disk", "1"); !errors.Is(err, settings.ErrUnknownKey) {
		t.Errorf("unknown key: %v", err)
	}

	// Too much memory for the node is allowed, with a warning.
	must(t, ti.SetConfig(ctx, "runners.max", "8"))
	if !strings.Contains(ti.stderr.String(), "more than this node's") {
		t.Errorf("stderr:\n%s", ti.stderr)
	}
	// A key the warning isn't about doesn't repeat it; one it is about does.
	ti.stderr.Reset()
	must(t, ti.SetConfig(ctx, "worker.cores", "4"))
	if strings.Contains(ti.stderr.String(), "more than this node's") {
		t.Errorf("setting worker.cores warned about memory:\n%s", ti.stderr)
	}
	must(t, ti.SetConfig(ctx, "worker.memory", "9GiB"))
	if !strings.Contains(ti.stderr.String(), "more than this node's") {
		t.Errorf("setting worker.memory didn't warn:\n%s", ti.stderr)
	}
}

func TestSetConfigRefusesAnotherControllerVersion(t *testing.T) {
	ti := installed(t)
	ti.controller(t).files["parcon-version"] = "0.1.0"
	err := ti.SetConfig(context.Background(), "runners.max", "2")
	if err == nil || !strings.Contains(err.Error(), "run parcon update first") {
		t.Errorf("SetConfig = %v", err)
	}
	if s, _ := settings.Load(ti.SettingsPath); s.ScaleSet.MaxRunners != 1 {
		t.Error("the settings changed")
	}
}

func TestSetConfigRollsBackWhenTheControllerRefuses(t *testing.T) {
	ti := installed(t)
	ti.node.guest = func(_ *fakeVM, argv []string, _ []byte) (int, string, string, bool) {
		if strings.Contains(strings.Join(argv, " "), "parcon check config -config") {
			return 1, "", "invalid config:\n  - surprise", true
		}
		return 0, "", "", false
	}
	err := ti.SetConfig(context.Background(), "runners.max", "2")
	if err == nil || !strings.Contains(err.Error(), "refuses the new config") {
		t.Fatalf("SetConfig = %v", err)
	}
	c := ti.controller(t)
	if _, ok := c.files[config.DefaultPath+".new"]; ok {
		t.Error("config.yaml.new is left")
	}
	if cfg, _ := config.Parse([]byte(c.files[config.DefaultPath])); cfg.ScaleSets[0].MaxRunners != 1 {
		t.Error("the controller's config changed")
	}
	if s, _ := settings.Load(ti.SettingsPath); s.ScaleSet.MaxRunners != 1 {
		t.Error("the settings changed")
	}
}

func TestStatus(t *testing.T) {
	ti := installed(t)
	latestRunnerRelease = func(context.Context) (github.RunnerRelease, error) {
		return github.RunnerRelease{Version: "2.338.0"}, nil
	}
	t.Cleanup(func() { latestRunnerRelease = github.LatestRunnerRelease })
	c := ti.controller(t)
	desired := 1
	snap := controller.Status{Schema: controller.StatusSchema, Version: "0.2.0", UpdatedAt: ti.Now(),
		ScaleSets: []controller.ScaleSetStatus{{Name: "proxmox-ubuntu-26.04", ID: 3, SessionOpen: true,
			SessionSince: ti.Now().Add(-2 * time.Hour), Desired: &desired,
			Stats: &github.Stats{AvailableJobs: 1, RunningJobs: 1, RegisteredRunners: 1, BusyRunners: 1}}}}
	c.files[controller.StatusPath] = jsonOf(snap)
	ti.node.vms[10000] = &fakeVM{id: 10000, name: "par-proxmox-ubuntu-26-04-abcde", pool: RunnerPool, running: true,
		tags: []string{"par-managed", "par-worker", "par-ready", "par-ss-proxmox-ubuntu-26.04",
			"par-created-" + strconv.FormatInt(ti.Now().Add(-12*time.Minute).Unix(), 10)}, config: map[string]string{}}

	r, err := ti.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if r.Failed() {
		t.Errorf("a healthy install fails: %+v", r)
	}
	var out strings.Builder
	r.Render(&out, ti.Now())
	for _, want := range []string{
		"proxmox-actions-runners 0.2.0 on pve1",
		"up to date with the latest release, 0.2.0",
		"ok    pools, role, token, and the worker network parzone/parnet",
		"Gateway VM 100", "DHCP, DNS, and NAT for the worker network are active",
		"Controller VM 101", "parcon.service is active", "its config matches the settings",
		"scale set proxmox-ubuntu-26.04 (ID 3): listening for jobs for 2h",
		"jobs: 1 waiting, 1 running; runners: 1 (1 busy, 0 idle)",
		"template 10096 has actions/runner 2.338.0, the latest",
		"Workers: 1 of at most 1, 1 ready", "10000  ready    12m",
		"worker.memory  8GiB (default)",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("status lacks %q:\n%s", want, out.String())
		}
	}

	// A stopped controller, a config that drifted, and a stale runner fail or warn.
	c.files[config.DefaultPath] = "# edited by hand\n"
	latestRunnerRelease = func(context.Context) (github.RunnerRelease, error) {
		return github.RunnerRelease{Version: "2.339.0", PublishedAt: ti.Now().Add(-25 * 24 * time.Hour)}, nil
	}
	r, _ = ti.Status(context.Background())
	out.Reset()
	r.Render(&out, ti.Now())
	if !r.Failed() || !strings.Contains(out.String(), "WARN  the controller's config doesn't match the settings") ||
		!strings.Contains(out.String(), "FAIL  template 10096 has actions/runner 2.338.0, but 2.339.0 came out 25d ago") {
		t.Errorf("status:\n%s", out.String())
	}
}
