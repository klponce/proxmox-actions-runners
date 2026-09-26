package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestMarshalRoundTrips(t *testing.T) {
	linked := false
	in := &Config{
		Proxmox: Proxmox{
			URL: "https://192.0.2.1:8006/api2/json", TokenID: "par@pve!controller",
			TokenSecretFile: "/etc/proxmox-actions-runners/pve-token", CACertFile: "/etc/proxmox-actions-runners/pve-ca.pem",
			TLSServerName: "pve1.example.com", Node: "pve1", Pool: DefaultPool, Storage: DefaultStorage,
			VNet: DefaultVNet, VMIDRange: VMIDRange{Start: 10000, End: 10099}, LinkedClone: &linked,
		},
		GitHub: GitHub{ConfigURL: "https://github.com/my-org", App: GitHubApp{
			ClientID: "Iv23liEXAMPLE", InstallationID: 42, PrivateKeyFile: "/etc/proxmox-actions-runners/github-app.pem",
		}},
		Worker: Worker{Cores: 4, MemoryMiB: 16384},
		ScaleSets: []ScaleSet{{
			Name: "ubuntu", Labels: []string{"null", "true", "1.5", "ubuntu"}, MaxRunners: 3, MinRunners: 1,
			MaxLifetime: 2 * time.Hour,
		}},
	}
	data, err := Marshal(in)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Parse(data)
	if err != nil {
		t.Fatalf("Parse(Marshal(c)): %v\n%s", err, data)
	}
	want := *in
	want.Proxmox.Zone = DefaultZone
	want.Worker.FreeDiskGiB = DefaultFreeDiskGiB
	want.ScaleSets = []ScaleSet{in.ScaleSets[0]}
	want.ScaleSets[0].RunnerGroup = DefaultRunnerGroup
	want.ScaleSets[0].Worker = want.Worker
	if !reflect.DeepEqual(got, &want) {
		t.Errorf("round trip:\n got %+v\nwant %+v\n%s", got, &want, data)
	}
}

// A config with no GitHub App yet and no hardware overrides leaves those sections out rather than writing zeros.
func TestMarshalLeavesUnsetFieldsOut(t *testing.T) {
	data, err := Marshal(&Config{
		Proxmox:   Proxmox{URL: "https://192.0.2.1:8006/api2/json", Node: "pve1"},
		ScaleSets: []ScaleSet{{Name: "ubuntu", MaxRunners: 1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"github", "worker", "caCertFile", "tlsFingerprint", "minRunners", "labels"} {
		if strings.Contains(string(data), absent) {
			t.Errorf("Marshal wrote %s:\n%s", absent, data)
		}
	}
}
