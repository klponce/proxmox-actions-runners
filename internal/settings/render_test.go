package settings

import (
	"net/netip"
	"slices"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
)

var testEnv = Env{
	Node:        "pve1",
	PVEAddress:  netip.MustParseAddr("192.0.2.5"),
	TLS:         hostsys.TLSInfo{Mode: hostsys.TLSNodeCA, ServerName: "pve1"},
	LinkedClone: true,
}

func TestControllerConfigBeforeTheApp(t *testing.T) {
	s := Default()
	c, data, err := ControllerConfig(&s, testEnv)
	if err != nil {
		t.Fatal(err)
	}
	p := c.Proxmox
	if p.URL != "https://192.0.2.5:8006/api2/json" || p.TokenID != "par@pve!controller" || p.Node != "pve1" ||
		p.CACertFile != config.CACertFile || p.TLSServerName != "pve1" || p.Pool != "par-runners" ||
		p.VNet != "parnet" || !p.UseLinkedClone() || p.VMIDRange != (config.VMIDRange{Start: 10000, End: 10099}) {
		t.Errorf("proxmox = %+v", p)
	}
	if strings.Contains(string(data), "github") {
		t.Errorf("config has a github section before the App exists:\n%s", data)
	}
	if err := c.RequireGitHubApp(); err == nil {
		t.Error("RequireGitHubApp passes without an App")
	}
	ss := c.ScaleSets[0]
	if ss.Name != config.DefaultScaleSetName || ss.MaxRunners != 1 || ss.Worker != config.DefaultWorker() {
		t.Errorf("scale set = %+v", ss)
	}
	if !strings.HasPrefix(string(data), "# Written by parcon") {
		t.Errorf("no header:\n%s", data)
	}
}

func TestControllerConfigWithTheApp(t *testing.T) {
	s := Default()
	s.GitHub = GitHub{ConfigURL: "https://github.com/my-org", App: App{ClientID: "Iv23liEXAMPLE", InstallationID: 7}}
	s.ScaleSet.Labels = []string{"null", "true", "linux"}
	s.ScaleSet.MaxRunners = 4
	s.Worker.MemoryMiB = 16384
	env := testEnv
	env.TLS = hostsys.TLSInfo{Mode: hostsys.TLSPin, Fingerprint: "AA:BB"}
	env.LinkedClone = false
	c, _, err := ControllerConfig(&s, env)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RequireGitHubApp(); err != nil {
		t.Error(err)
	}
	if c.GitHub.App.PrivateKeyFile != config.AppKeyFile || c.GitHub.App.InstallationID != 7 {
		t.Errorf("app = %+v", c.GitHub.App)
	}
	if c.Proxmox.TLSFingerprint != "AA:BB" || c.Proxmox.CACertFile != "" || c.Proxmox.UseLinkedClone() {
		t.Errorf("proxmox = %+v", c.Proxmox)
	}
	ss := c.ScaleSets[0]
	if !slices.Equal(ss.Labels, []string{"null", "true", "linux"}) || ss.MaxRunners != 4 ||
		ss.Worker.MemoryMiB != 16384 || ss.Worker.Cores != config.DefaultCores {
		t.Errorf("scale set = %+v", ss)
	}
}

func TestControllerConfigRefusesWhatTheControllerWould(t *testing.T) {
	s := Default()
	s.ScaleSet.MaxRunners = 200 // more than the VMID range holds
	if _, _, err := ControllerConfig(&s, testEnv); err == nil || !strings.Contains(err.Error(), "vmidRange") {
		t.Errorf("err = %v", err)
	}
}
