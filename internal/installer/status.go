package installer

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"slices"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/controller"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// StatusSchema is the version of `parcon status --json`'s format.
const StatusSchema = 1

// StatusLevel is how a status line is judged.
type StatusLevel string

// Status levels.
const (
	OK   StatusLevel = "ok"
	WARN StatusLevel = "warn"
	FAIL StatusLevel = "fail"
)

// Finding is one judged line of the status.
type Finding struct {
	Level StatusLevel `json:"level"`
	Text  string      `json:"text"`
}

// Report is the state of the whole install, for `parcon status`.
type Report struct {
	Schema     int    `json:"schema"`
	Version    string `json:"version"`
	Node       string `json:"node"`
	PVEVersion string `json:"pveVersion"`
	// Installed is the release of the finished install; empty if the node has none.
	Installed string `json:"installed,omitempty"`
	// Latest is the newest release, or LatestError why it isn't known.
	Latest      string `json:"latest,omitempty"`
	LatestError string `json:"latestError,omitempty"`

	Host       []Finding                   `json:"host"`
	Gateway    *SystemVMStatus             `json:"gateway,omitempty"`
	Controller *SystemVMStatus             `json:"controller,omitempty"`
	GitHub     []Finding                   `json:"github"`
	ScaleSets  []controller.ScaleSetStatus `json:"scaleSets,omitempty"`
	Templates  []TemplateStatus            `json:"templates"`
	Workers    []WorkerStatus              `json:"workers"`
	MaxRunners int                         `json:"maxRunners"`
	Settings   []SettingStatus             `json:"settings,omitempty"`
}

// SystemVMStatus is the gateway or controller VM.
type SystemVMStatus struct {
	VMID     int       `json:"vmid"`
	Running  bool      `json:"running"`
	Release  string    `json:"release,omitempty"`
	Address  string    `json:"address,omitempty"`
	Findings []Finding `json:"findings"`
}

// TemplateStatus is a runner template.
type TemplateStatus struct {
	VMID          int       `json:"vmid"`
	RunnerVersion string    `json:"runnerVersion"`
	ImportedAt    time.Time `json:"importedAt"`
	Newest        bool      `json:"newest"`
}

// WorkerStatus is a worker VM.
type WorkerStatus struct {
	VMID      int       `json:"vmid"`
	Name      string    `json:"name"`
	ScaleSet  string    `json:"scaleSet"`
	Running   bool      `json:"running"`
	Ready     bool      `json:"ready"`
	CreatedAt time.Time `json:"createdAt"`
}

// SettingStatus is a config key's value.
type SettingStatus struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	IsDefault bool   `json:"isDefault"`
}

// Failed reports whether anything in the report failed.
func (r *Report) Failed() bool {
	all := slices.Concat(r.Host, r.GitHub)
	for _, vm := range []*SystemVMStatus{r.Gateway, r.Controller} {
		if vm != nil {
			all = append(all, vm.Findings...)
		}
	}
	return slices.ContainsFunc(all, func(f Finding) bool { return f.Level == FAIL })
}

// guestTimeout bounds each command status runs in a VM, so a hung guest can't hang status.
const guestTimeout = 15 * time.Second

// Status reports the state of the whole install.
func (in *Installer) Status(ctx context.Context) (*Report, error) {
	r := &Report{Schema: StatusSchema, Version: in.Version.String()}
	var err error
	if r.Node, err = in.nodeName(ctx); err != nil {
		return nil, err
	}
	r.PVEVersion, _ = in.Sys.PVEVersion(ctx)
	if v, ok, err := in.InstalledRelease(ctx); err != nil {
		return nil, err
	} else if ok {
		r.Installed = v.String()
	}
	s, err := settings.Load(in.SettingsPath)
	if err != nil {
		return nil, fmt.Errorf("%w; run parcon install", err)
	}
	r.MaxRunners = s.ScaleSet.MaxRunners
	for _, k := range settings.Keys {
		v, isDefault := k.Get(s)
		r.Settings = append(r.Settings, SettingStatus{Key: k.Name, Value: v, IsDefault: isDefault})
	}
	latest, err := release.Latest(ctx, in.Source, in.Version.IsPre())
	if err != nil {
		r.LatestError = err.Error()
	} else {
		r.Latest = latest.Version.String()
	}

	vms, err := in.vms(ctx)
	if err != nil {
		return nil, err
	}
	r.Host = in.hostFindings(ctx, s)
	if vm, ok := findTagged(vms, SystemPool, vmtags.Gateway); ok {
		r.Gateway = in.gatewayStatus(ctx, vm)
	} else {
		r.Host = append(r.Host, Finding{FAIL, "there is no gateway VM; run parcon install"})
	}
	var snapshot *controller.Status
	if vm, ok := findTagged(vms, SystemPool, vmtags.Controller); ok {
		r.Controller, snapshot = in.controllerStatus(ctx, vm, s)
	} else {
		r.Host = append(r.Host, Finding{FAIL, "there is no controller VM; run parcon install"})
	}
	r.GitHub = githubFindings(s, snapshot, in.Now())
	if snapshot != nil {
		r.ScaleSets = snapshot.ScaleSets
	}
	r.Templates, r.Workers = templatesAndWorkers(vms)
	r.GitHub = append(r.GitHub, in.runnerFinding(ctx, r.Templates))
	return r, nil
}

func findTagged(vms []proxmox.VM, pool, tag string) (proxmox.VM, bool) {
	t := tagged(vms, pool, tag)
	if len(t) == 0 {
		return proxmox.VM{}, false
	}
	return t[0], true
}

func (in *Installer) hostFindings(ctx context.Context, s *settings.Settings) []Finding {
	f := []Finding{{OK, fmt.Sprintf("parcon %s at %s, settings at %s", in.Version, in.BinaryPath, in.SettingsPath)}}
	var missing []string
	pools, _ := in.PVE.Pools(ctx)
	for _, p := range []string{SystemPool, RunnerPool} {
		if !slices.ContainsFunc(pools, func(x pvecli.Pool) bool { return x.ID == p }) {
			missing = append(missing, "pool "+p)
		}
	}
	if roles, _ := in.PVE.Roles(ctx); !slices.ContainsFunc(roles, func(r pvecli.Role) bool { return r.ID == Role }) {
		missing = append(missing, "role "+Role)
	}
	if tokens, _ := in.PVE.Tokens(ctx, PVEUser); !slices.Contains(tokens, TokenName) {
		missing = append(missing, "token "+PVEUser+"!"+TokenName)
	}
	for _, obj := range []string{"zones/" + Zone, "vnets/" + VNet} {
		if !in.PVE.SDNExists(ctx, obj) {
			missing = append(missing, "SDN "+obj)
		}
	}
	if len(missing) > 0 {
		f = append(f, Finding{FAIL, "missing " + strings.Join(missing, ", ") + "; run parcon install"})
	} else {
		f = append(f, Finding{OK, fmt.Sprintf("pools, role, token, and the worker network %s/%s", Zone, VNet)})
	}
	r := in.checkOffloads(ctx, s, ModeInstalled)
	level := OK
	if !r.ok {
		level = WARN
	}
	return append(f, Finding{level, "LAN NIC offloads: " + r.reason})
}

// guest runs a command in a VM for status, and returns its trimmed output.
func (in *Installer) guest(ctx context.Context, vmid int, argv ...string) (string, error) {
	out, err := in.guestExec(ctx, vmid, guestTimeout, nil, argv...)
	return strings.TrimSpace(string(out)), err
}

func (in *Installer) systemVMStatus(ctx context.Context, vm proxmox.VM) *SystemVMStatus {
	st := &SystemVMStatus{VMID: vm.VMID, Running: vm.Status == "running"}
	st.Release, _ = vmtags.String(vm, vmtags.ReleasePrefix)
	if !st.Running {
		st.Findings = append(st.Findings, Finding{FAIL, fmt.Sprintf("VM %d isn't running; start it with qm start %d",
			vm.VMID, vm.VMID)})
		return st
	}
	if err := in.PVE.AgentPing(ctx, vm.VMID); err != nil {
		st.Findings = append(st.Findings, Finding{FAIL, "its guest agent doesn't answer"})
		return st
	}
	if a, ok := in.agentIPv4(ctx, vm.VMID); ok {
		st.Address = a.String()
	}
	return st
}

func (in *Installer) gatewayStatus(ctx context.Context, vm proxmox.VM) *SystemVMStatus {
	st := in.systemVMStatus(ctx, vm)
	if len(st.Findings) > 0 {
		return st
	}
	st.Findings = append(st.Findings, Finding{OK, fmt.Sprintf("running, release %s, %s", orNone(st.Release), orNone(st.Address))})
	out, err := in.guest(ctx, vm.VMID, "systemctl", "is-active", "dnsmasq.service", "nftables.service")
	if err != nil {
		st.Findings = append(st.Findings, Finding{FAIL, "dnsmasq or nftables isn't active (" +
			strings.ReplaceAll(out, "\n", ", ") + "): workers get no network"})
	} else {
		st.Findings = append(st.Findings, Finding{OK, "DHCP, DNS, and NAT for the worker network are active"})
	}
	return st
}

func orNone(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

func (in *Installer) controllerStatus(ctx context.Context, vm proxmox.VM, s *settings.Settings) (
	*SystemVMStatus, *controller.Status,
) {
	st := in.systemVMStatus(ctx, vm)
	if len(st.Findings) > 0 {
		return st, nil
	}
	version, err := in.guest(ctx, vm.VMID, "runuser", "-u", "parcon", "--", "parcon", "version")
	if err != nil {
		version = "unknown"
	}
	st.Findings = append(st.Findings, Finding{OK, fmt.Sprintf("running, release %s, %s, parcon %s",
		orNone(st.Release), orNone(st.Address), version)})
	if version != in.Version.String() && in.Version != (release.Version{}) {
		st.Findings = append(st.Findings, Finding{WARN, fmt.Sprintf("the controller runs parcon %s and this host "+
			"parcon %s; run parcon update", version, in.Version)})
	}

	if out, err := in.guest(ctx, vm.VMID, "systemctl", "is-active", "parcon.service"); err != nil {
		st.Findings = append(st.Findings, Finding{FAIL, fmt.Sprintf("parcon.service is %s; see journalctl -u parcon "+
			"in VM %d", out, vm.VMID)})
	} else {
		st.Findings = append(st.Findings, Finding{OK, "parcon.service is active"})
	}

	if want, _, err := in.RenderConfig(ctx, s); err != nil {
		st.Findings = append(st.Findings, Finding{FAIL, "the settings don't render a config: " + err.Error()})
	} else if have, err := in.guest(ctx, vm.VMID, "runuser", "-u", "parcon", "--", "cat", config.DefaultPath); err != nil {
		st.Findings = append(st.Findings, Finding{FAIL, "can't read the controller's config: " + err.Error()})
	} else if have != strings.TrimSpace(string(want)) {
		st.Findings = append(st.Findings, Finding{WARN, "the controller's config doesn't match the settings; run " +
			"parcon config apply"})
	} else {
		st.Findings = append(st.Findings, Finding{OK, "its config matches the settings"})
	}

	out, err := in.guest(ctx, vm.VMID, "cat", controller.StatusPath)
	if err != nil {
		return st, nil
	}
	var snap controller.Status
	if json.Unmarshal([]byte(out), &snap) != nil || snap.Schema != controller.StatusSchema {
		return st, nil
	}
	if age := in.Now().Sub(snap.UpdatedAt); age > 2*time.Minute {
		st.Findings = append(st.Findings, Finding{WARN, fmt.Sprintf("its last pass was %s ago", ago(age))})
	}
	if snap.LastError != "" {
		st.Findings = append(st.Findings, Finding{WARN, "its last pass failed: " + snap.LastError})
	}
	return st, &snap
}

func githubFindings(s *settings.Settings, snap *controller.Status, now time.Time) []Finding {
	if s.GitHub.App.ClientID == "" || s.GitHub.App.InstallationID == 0 {
		return []Finding{{FAIL, "the GitHub App isn't set up yet; run parcon install"}}
	}
	f := []Finding{{OK, fmt.Sprintf("%s, App %s, installation %d", s.GitHub.ConfigURL, s.GitHub.App.ClientID,
		s.GitHub.App.InstallationID)}}
	if snap == nil {
		return append(f, Finding{WARN, "the controller reports no status yet"})
	}
	for _, ss := range snap.ScaleSets {
		switch {
		case ss.SessionOpen:
			f = append(f, Finding{OK, fmt.Sprintf("scale set %s (ID %d): listening for jobs for %s", ss.Name, ss.ID,
				ago(now.Sub(ss.SessionSince)))})
		case ss.SessionError != "":
			f = append(f, Finding{FAIL, fmt.Sprintf("scale set %s: no session with GitHub since %s ago: %s", ss.Name,
				ago(now.Sub(ss.SessionSince)), ss.SessionError)})
		default:
			f = append(f, Finding{WARN, fmt.Sprintf("scale set %s: no session with GitHub yet", ss.Name)})
		}
		if st := ss.Stats; st != nil {
			f = append(f, Finding{OK, fmt.Sprintf("  jobs: %d waiting, %d running; runners: %d (%d busy, %d idle)",
				st.AvailableJobs+st.AcquiredJobs, st.RunningJobs, st.RegisteredRunners, st.BusyRunners,
				st.IdleRunners)})
		}
	}
	return f
}

func templatesAndWorkers(vms []proxmox.VM) ([]TemplateStatus, []WorkerStatus) {
	var templates []TemplateStatus
	newest, hasNewest := vmtags.NewestTemplate(vms, RunnerPool)
	for _, vm := range vms {
		if vmtags.IsTemplate(vm, RunnerPool) {
			rv, _ := vmtags.String(vm, vmtags.RunnerVersionPrefix)
			tv, _ := vmtags.Int(vm, vmtags.TemplateVersionPrefix)
			templates = append(templates, TemplateStatus{VMID: vm.VMID, RunnerVersion: rv,
				ImportedAt: time.Unix(tv, 0).UTC(), Newest: hasNewest && vm.VMID == newest.VMID})
		}
	}
	workers := []WorkerStatus{}
	for _, vm := range vms {
		if vm.Pool != RunnerPool || !vm.HasTag(vmtags.Worker) {
			continue
		}
		created, _ := vmtags.Int(vm, vmtags.CreatedPrefix)
		ss, _ := vmtags.String(vm, vmtags.ScaleSetPrefix)
		workers = append(workers, WorkerStatus{VMID: vm.VMID, Name: vm.Name, ScaleSet: ss,
			Running: vm.Status == "running", Ready: vm.HasTag(vmtags.Ready), CreatedAt: time.Unix(created, 0).UTC()})
	}
	return templates, workers
}

// latestRunnerRelease is actions/runner's newest release. Tests replace it.
var latestRunnerRelease = github.LatestRunnerRelease

func (in *Installer) runnerFinding(ctx context.Context, templates []TemplateStatus) Finding {
	var newest *TemplateStatus
	for i := range templates {
		if templates[i].Newest {
			newest = &templates[i]
		}
	}
	if newest == nil {
		return Finding{FAIL, "there is no runner template; run parcon update"}
	}
	rel, err := latestRunnerRelease(ctx)
	if err != nil {
		return Finding{WARN, fmt.Sprintf("template %d has actions/runner %s; the latest release isn't known: %v",
			newest.VMID, newest.RunnerVersion, err)}
	}
	text := fmt.Sprintf("template %d has actions/runner %s, the latest", newest.VMID, newest.RunnerVersion)
	switch rel.Staleness(newest.RunnerVersion, in.Now()) {
	case github.Current:
		return Finding{OK, text}
	case github.BehindError:
		return Finding{FAIL, fmt.Sprintf("template %d has actions/runner %s, but %s came out %s ago: GitHub stops "+
			"accepting it %s after that; run parcon update", newest.VMID, newest.RunnerVersion, rel.Version,
			ago(in.Now().Sub(rel.PublishedAt)), ago(github.RunnerUpdateDeadline))}
	default:
		return Finding{WARN, fmt.Sprintf("template %d has actions/runner %s; %s is out: run parcon update when a "+
			"release carries it", newest.VMID, newest.RunnerVersion, rel.Version)}
	}
}

// ago prints a duration the way status shows ages: 45s, 12m, 3h, 4d.
func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Render prints the report for people.
func (r *Report) Render(w io.Writer, now time.Time) {
	p := func(format string, args ...any) { _, _ = fmt.Fprintf(w, format+"\n", args...) }
	line := func(f Finding) {
		label := map[StatusLevel]string{OK: "ok   ", WARN: "WARN ", FAIL: "FAIL "}[f.Level]
		p("  %s %s", label, f.Text)
	}
	installed := r.Installed
	if installed == "" {
		installed = "not finished"
	}
	p("proxmox-actions-runners %s on %s (%s)", installed, r.Node, r.PVEVersion)
	switch {
	case r.LatestError != "":
		p("  latest release unknown: %s", r.LatestError)
	case r.Latest != "" && r.Installed != "" && compareVersions(r.Latest, r.Installed) > 0:
		p("  release %s is out: run parcon update", r.Latest)
	case r.Latest != "":
		p("  up to date with the latest release, %s", r.Latest)
	}
	p("\nHost")
	for _, f := range r.Host {
		line(f)
	}
	for _, vm := range []struct {
		name string
		st   *SystemVMStatus
	}{{"Gateway", r.Gateway}, {"Controller", r.Controller}} {
		if vm.st == nil {
			continue
		}
		p("\n%s VM %d", vm.name, vm.st.VMID)
		for _, f := range vm.st.Findings {
			line(f)
		}
	}
	p("\nGitHub")
	for _, f := range r.GitHub {
		line(f)
	}
	ready := 0
	for _, wk := range r.Workers {
		if wk.Ready {
			ready++
		}
	}
	p("\nWorkers: %d of at most %d, %d ready", len(r.Workers), r.MaxRunners, ready)
	for _, wk := range r.Workers {
		state := "stopped"
		switch {
		case wk.Running && wk.Ready:
			state = "ready"
		case wk.Running:
			state = "booting"
		}
		p("  %-6d %-8s %-4s old  %s", wk.VMID, state, ago(now.Sub(wk.CreatedAt)), wk.Name)
	}
	p("\nSettings (parcon config get --all)")
	for _, s := range r.Settings {
		def := ""
		if s.IsDefault {
			def = " (default)"
		}
		p("  %-14s %s%s", s.Key, s.Value, def)
	}
}

func compareVersions(a, b string) int {
	va, err1 := release.ParseVersion(a)
	vb, err2 := release.ParseVersion(b)
	if err1 != nil || err2 != nil {
		return 0
	}
	return va.Compare(vb)
}
