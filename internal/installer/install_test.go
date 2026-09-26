package installer

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

var defaultLookPath = exec.LookPath

// writeNodeCert writes a node CA and the node's certificate, as Proxmox makes them.
func writeNodeCert(t *testing.T, pveDir string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Proxmox Virtual Environment"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true,
		BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	must(t, err)
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "pve1"},
		DNSNames: []string{"localhost", "pve1"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	caCert, _ := x509.ParseCertificate(caDER)
	leafDER, err := x509.CreateCertificate(rand.Reader, leaf, caCert, &key.PublicKey, key)
	must(t, err)
	must(t, os.MkdirAll(filepath.Join(pveDir, "local"), 0o755))
	must(t, os.WriteFile(filepath.Join(pveDir, "pve-root-ca.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0o644))
	must(t, os.WriteFile(filepath.Join(pveDir, "local/pve-ssl.pem"),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0o644))
}

func defaultOptions() InstallOptions {
	s := settings.Default()
	return InstallOptions{Settings: &s, App: AppManifest}
}

// answerManifest answers the manifest flow's question with the line for the link install prints. The state in the
// link is random, so the answer is read from the output.
func (ti *testInstaller) answerManifest() { ti.Term = &linkAnswerer{ti: ti} }

// linkAnswerer answers the manifest flow's question with the state from the link install printed.
type linkAnswerer struct {
	ti      *testInstaller
	prompts []string
}

var stateInLink = regexp.MustCompile(`\?state=([A-Za-z0-9_-]+)`)

func (l *linkAnswerer) Confirm(q string) (bool, error) {
	l.prompts = append(l.prompts, q)
	return true, nil
}
func (l *linkAnswerer) Line(p string) (string, error) {
	l.prompts = append(l.prompts, p)
	m := stateInLink.FindStringSubmatch(l.ti.stdout.String())
	if m == nil {
		return "", errors.New("no link printed")
	}
	return m[1] + "." + manifestCode, nil
}
func (l *linkAnswerer) PEM(p string) (string, error) {
	l.prompts = append(l.prompts, p)
	return appKeyPEM, nil
}
func (l *linkAnswerer) Choose(p string, _ []string) (int, error) {
	l.prompts = append(l.prompts, p)
	return 0, nil
}

// assertNoSecrets checks that no secret reached a command line, the output, or the host's files.
func (ti *testInstaller) assertNoSecrets(t *testing.T, errs ...error) {
	t.Helper()
	secrets := []string{tokenSecret, manifestCode, "SENTINEL-app-key"}
	for _, l := range ti.node.lines() {
		for _, s := range secrets {
			if strings.Contains(l, s) {
				t.Errorf("a command line holds a secret: %s", l)
			}
		}
	}
	for _, text := range []string{ti.stdout.String(), ti.stderr.String()} {
		for _, s := range secrets {
			if strings.Contains(text, s) {
				t.Errorf("the output holds a secret:\n%s", text)
			}
		}
	}
	for _, err := range errs {
		for _, s := range secrets {
			if err != nil && strings.Contains(err.Error(), s) {
				t.Errorf("an error holds a secret: %v", err)
			}
		}
	}
	_ = filepath.WalkDir(ti.dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, _ := os.ReadFile(path)
		for _, s := range secrets {
			if strings.Contains(string(data), s) {
				t.Errorf("%s on the host holds a secret", path)
			}
		}
		return nil
	})
}

func (ti *testInstaller) controller(t *testing.T) *fakeVM {
	t.Helper()
	ids := ti.node.vmsTagged(vmtags.Controller)
	if len(ids) != 1 {
		t.Fatalf("controller VMs: %v", ids)
	}
	return ti.node.vms[ids[0]]
}

func TestInstall(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	ctx := context.Background()
	err := ti.Install(ctx, defaultOptions())
	if err != nil {
		t.Fatalf("Install: %v\n%s\n%s", err, ti.stdout, ti.stderr)
	}
	ti.assertNoSecrets(t, err)

	// parcon and its settings are on the host, with what the install learned.
	if data, _ := os.ReadFile(ti.BinaryPath); string(data) != "parcon 0.2.0" {
		t.Errorf("host binary = %q", data)
	}
	s, err := settings.Load(ti.SettingsPath)
	if err != nil {
		t.Fatal(err)
	}
	if s.GitHub.ConfigURL != "https://github.com/my-org" || s.GitHub.App.ClientID != "Iv23liEXAMPLE" ||
		s.GitHub.App.InstallationID != 7 {
		t.Errorf("settings github = %+v", s.GitHub)
	}

	// Proxmox objects.
	n := ti.node
	if n.pools[SystemPool] != PoolComment || n.pools[RunnerPool] != PoolComment {
		t.Errorf("pools = %v", n.pools)
	}
	if !strings.Contains(n.roles[Role], "VM.GuestAgent.FileWrite") || strings.Contains(n.roles[Role], "Unrestricted") {
		t.Errorf("role privileges = %s", n.roles[Role])
	}
	if !slices.Contains(n.tokens, TokenName) || len(n.acls) != 6 {
		t.Errorf("tokens %v, acls %v", n.tokens, n.acls)
	}

	// VMs: a template in the reserved IDs, the gateway and controller outside the range, no smoke-test clone left.
	tmpl := n.vmsTagged(vmtags.Template)
	if len(tmpl) != 1 || tmpl[0] != 10096 || !n.vms[10096].template ||
		!slices.Contains(n.vms[10096].tags, "par-rv-2.338.0") {
		t.Errorf("templates %v", tmpl)
	}
	if len(n.vmsTagged(vmtags.Build)) != 0 {
		t.Error("the smoke-test clone is left")
	}
	c := ti.controller(t)
	if c.id != 101 || !slices.Contains(c.tags, "par-release-0.2.0") || c.config["net0"] != "virtio,bridge=vmbr0" {
		t.Errorf("controller = %+v", c)
	}
	if g := n.vmsTagged(vmtags.Gateway); len(g) != 1 || n.vms[g[0]].config["net1"] != "virtio,bridge=parnet" {
		t.Errorf("gateway = %v", g)
	}

	// The controller's files: the config with the App, the token, and the CA.
	cfg, err := config.Parse([]byte(c.files[config.DefaultPath]))
	if err != nil {
		t.Fatalf("controller config: %v\n%s", err, c.files[config.DefaultPath])
	}
	if err := cfg.RequireGitHubApp(); err != nil || cfg.Proxmox.URL != "https://192.0.2.5:8006/api2/json" {
		t.Errorf("controller config: %v, %+v", err, cfg.Proxmox)
	}
	if c.files[config.TokenSecretFile] != tokenSecret+"\n" || c.files[config.CACertFile] == "" {
		t.Errorf("controller files: %v", slices.Collect(mapKeys(c.files)))
	}
	if _, left := c.files[config.DefaultPath+".new"]; left {
		t.Error("config.yaml.new is left")
	}
	if !strings.Contains(ti.stdout.String(), "runs-on: proxmox-ubuntu-26.04") {
		t.Errorf("summary:\n%s", ti.stdout)
	}

	// A second install refuses: the node has a finished install.
	if err := ti.Install(ctx, defaultOptions()); !errors.Is(err, ErrInstalled) {
		t.Errorf("second Install = %v", err)
	}
}

func mapKeys(m map[string]string) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

func TestInstallDryRunChangesNothing(t *testing.T) {
	ti := newTestInstaller(t)
	ti.Change.DryRun = true
	if err := ti.Install(context.Background(), defaultOptions()); err != nil {
		t.Fatal(err)
	}
	for _, l := range ti.node.lines() {
		if regexp.MustCompile(`^(pveum \S+ (add|modify|delete)|qm (create|set|start|destroy)|pvesh (create|set))`).
			MatchString(l) {
			t.Errorf("dry run ran %s", l)
		}
	}
	if _, err := os.Stat(ti.SettingsPath); !errors.Is(err, os.ErrNotExist) {
		t.Error("dry run wrote the settings")
	}
	out := ti.stdout.String()
	for _, want := range []string{"==> Plan", "install parcon as " + ti.BinaryPath, "gateway VM in par-system",
		"at most 1, each 2 vCPU and 8GiB"} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
}

func TestInstallContinuesWithTheSameApp(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	ctx := context.Background()
	// The first run stops while waiting for the App's installation.
	ti.node.guest = func(_ *fakeVM, argv []string, _ []byte) (int, string, string, bool) {
		if slices.Contains(argv, "wait-installation") {
			return 1, "", "timed out", true
		}
		return 0, "", "", false
	}
	if err := ti.Install(ctx, defaultOptions()); err == nil || !strings.Contains(err.Error(), "wasn't installed") {
		t.Fatalf("first Install = %v", err)
	}
	s, err := settings.Load(ti.SettingsPath)
	if err != nil || s.GitHub.App.ClientID != "Iv23liEXAMPLE" {
		t.Fatalf("settings after the first run: %+v, %v", s, err)
	}

	// The second run continues: no second App, the same VMs, and it refuses new settings.
	ti.node.guest = nil
	opts := defaultOptions()
	opts.FlagsGiven = true
	if err := ti.Install(ctx, opts); err == nil || !strings.Contains(err.Error(), "earlier install's settings") {
		t.Errorf("Install with new settings = %v", err)
	}
	creates := 0
	for _, l := range ti.node.lines() {
		if strings.Contains(l, "parcon github app create") {
			creates++
		}
	}
	vmsBefore := len(ti.node.vms)
	if err := ti.Install(ctx, defaultOptions()); err != nil {
		t.Fatalf("second Install: %v\n%s", err, ti.stderr)
	}
	after := 0
	for _, l := range ti.node.lines() {
		if strings.Contains(l, "parcon github app create") {
			after++
		}
	}
	if after != creates {
		t.Error("the second run created another App")
	}
	if len(ti.node.vms) != vmsBefore {
		t.Errorf("VMs: %d before, %d after", vmsBefore, len(ti.node.vms))
	}
	if !strings.Contains(ti.stdout.String(), "found an earlier install; continuing it") {
		t.Errorf("output:\n%s", ti.stdout)
	}
}

func TestInstallWithAnExistingApp(t *testing.T) {
	ti := newTestInstaller(t)
	ti.Term = &linkAnswerer{ti: ti}
	opts := defaultOptions()
	opts.App, opts.ClientID = AppManual, "Iv23liEXISTING"
	err := ti.Install(context.Background(), opts)
	if err != nil {
		t.Fatalf("Install: %v", err)
	}
	ti.assertNoSecrets(t, err)
	if ti.node.ran("qm guest exec 100 --timeout 60 -- runuser -u parcon -- parcon github app create") {
		t.Error("created an App")
	}
	var imported bool
	for _, c := range ti.node.calls {
		if strings.HasSuffix(c.String(), "parcon github app import") && string(c.Stdin) == appKeyPEM {
			imported = true
		}
	}
	if !imported {
		t.Error("the key wasn't imported on stdin")
	}
	if s, _ := settings.Load(ti.SettingsPath); s.GitHub.App.ClientID != "Iv23liEXISTING" {
		t.Errorf("client ID = %q", s.GitHub.App.ClientID)
	}
}

func TestInstallStopsOnFailedChecks(t *testing.T) {
	ti := newTestInstaller(t)
	ti.node.storage.Avail = 49 << 30
	ti.node.pending = []map[string]string{{"zone": "other", "state": "new"}}
	err := ti.Install(context.Background(), defaultOptions())
	if !errors.Is(err, errPreflight) {
		t.Fatalf("Install = %v", err)
	}
	out := ti.stdout.String()
	for _, want := range []string{"FAIL  free space on local-lvm: 49 GiB free, 50 GiB needed",
		"FAIL  no pending SDN changes: pending SDN changes to other"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if ti.node.ran("pveum pool add") {
		t.Error("a failed check still changed the node")
	}
}

func TestInstallNeedsATerminalOrYes(t *testing.T) {
	ti := newTestInstaller(t)
	ti.term.NoTTY = true
	err := ti.Install(context.Background(), defaultOptions())
	if err == nil || !strings.Contains(err.Error(), "--yes") {
		t.Errorf("Install = %v", err)
	}
	ti.term.Answers = []string{"n"}
	ti.term.NoTTY = false
	if err := ti.Install(context.Background(), defaultOptions()); !errors.Is(err, ErrCanceled) {
		t.Errorf("declined Install = %v", err)
	}
}

func TestUninstall(t *testing.T) {
	ti := newTestInstaller(t)
	ti.answerManifest()
	ctx := context.Background()
	must(t, ti.Install(ctx, defaultOptions()))
	// A worker the controller made, and a VM that isn't ours.
	ti.node.vms[10000] = &fakeVM{id: 10000, pool: RunnerPool, tags: []string{vmtags.Managed, vmtags.Worker},
		running: true, config: map[string]string{}}
	ti.node.vms[500] = &fakeVM{id: 500, pool: "", tags: []string{"mine"}, config: map[string]string{}}
	ti.node.zones = append(ti.node.zones, "other")

	ti.Change.DryRun = true
	must(t, ti.Uninstall(ctx))
	if !strings.Contains(ti.stdout.String(), "now: 100 101 10000 10096") {
		t.Errorf("dry-run plan:\n%s", ti.stdout)
	}
	if len(ti.node.vms) != 5 {
		t.Error("the dry run destroyed VMs")
	}

	ti.Change.DryRun = false
	ti.stdout.Reset()
	ti.node.calls = nil
	must(t, ti.Uninstall(ctx))
	if ids := slices.Collect(func(yield func(int) bool) {
		for id := range ti.node.vms {
			if !yield(id) {
				return
			}
		}
	}); len(ids) != 1 || ids[0] != 500 {
		t.Errorf("VMs left: %v", ids)
	}
	n := ti.node
	if len(n.pools) != 0 || len(n.roles) != 0 || len(n.users) != 0 || len(n.acls) != 0 ||
		!slices.Equal(n.zones, []string{"other"}) || len(n.vnets) != 0 {
		t.Errorf("left: pools %v roles %v users %v acls %v zones %v vnets %v", n.pools, n.roles, n.users, n.acls,
			n.zones, n.vnets)
	}
	// Clones before templates, and the scale set is deleted before the VMs go.
	lines := strings.Join(n.lines(), "\n")
	if i, j := strings.Index(lines, "qm destroy 10000"), strings.Index(lines, "qm destroy 10096"); i < 0 || j < i {
		t.Errorf("destroy order:\n%s", lines)
	}
	if i, j := strings.Index(lines, "parcon github scaleset delete"), strings.Index(lines, "qm destroy"); i < 0 || j < i {
		t.Error("the scale set wasn't deleted first")
	}
	for _, path := range []string{ti.SettingsPath, ti.BinaryPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is left", path)
		}
	}
}

func TestControllerStopTimeoutOutlastsTheService(t *testing.T) {
	unit, err := os.ReadFile("../../deploy/parcon.service")
	must(t, err)
	m := regexp.MustCompile(`(?m)^TimeoutStopSec=(\d+)(min|s)$`).FindSubmatch(unit)
	if m == nil {
		t.Fatal("parcon.service has no TimeoutStopSec")
	}
	d, _ := time.ParseDuration(string(m[1]) + map[string]string{"min": "m", "s": "s"}[string(m[2])])
	if ControllerStopTimeout <= d {
		t.Errorf("ControllerStopTimeout %s isn't longer than TimeoutStopSec %s", ControllerStopTimeout, d)
	}
}
