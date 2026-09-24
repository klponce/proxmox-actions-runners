package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeProxmox serves canned JSON for the endpoints check proxmox calls.
func fakeProxmox(t *testing.T, responses map[string]any) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, ok := responses[strings.TrimPrefix(r.URL.Path, "/api2/json")]
		if !ok {
			http.Error(w, `{"data":null}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	srv.Config.ErrorLog = log.New(io.Discard, "", 0)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

func fingerprintOf(srv *httptest.Server) string {
	sum := sha256.Sum256(srv.Certificate().Raw)
	parts := make([]string, len(sum))
	for i, b := range sum {
		parts[i] = fmt.Sprintf("%02X", b)
	}
	return strings.Join(parts, ":")
}

// writeCheckConfig writes a config pointing at srv and a token secret file with the given mode.
func writeCheckConfig(t *testing.T, srv *httptest.Server, secretMode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	secret := filepath.Join(dir, "pve-token")
	if err := os.WriteFile(secret, []byte("00000000-0000-0000-0000-000000000000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, secretMode); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`proxmox:
  url: %s/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: %s
  tlsFingerprint: %q
  node: pve1
  pool: par-runners
  storage: local-lvm
  vnet: parnet
  vmidRange: {start: 10000, end: 10999}
github:
  configUrl: https://github.com/my-org
  app: {clientId: Iv23liEXAMPLE0000000, installationId: 1, privateKeyFile: /etc/key.pem}
scaleSets:
  - {name: proxmox-ubuntu-26.04, maxRunners: 2}
`, srv.URL, secret, fingerprintOf(srv))
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(cfg), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func allPrivileges() map[string]any {
	pool := map[string]any{}
	for _, p := range []string{"VM.Allocate", "VM.Clone", "VM.Config.CPU", "VM.Config.Memory", "VM.Config.Disk",
		"VM.Config.Network", "VM.Config.Cloudinit", "VM.Config.Options", "VM.PowerMgmt", "VM.Audit",
		"VM.GuestAgent.Audit", "VM.GuestAgent.FileWrite", "VM.GuestAgent.Unrestricted", "Pool.Audit"} {
		pool[p] = 1
	}
	return map[string]any{
		"/pool/par-runners":         pool,
		"/storage/local-lvm":        map[string]any{"Datastore.AllocateSpace": 1, "Datastore.Audit": 1},
		"/sdn/zones/parzone/parnet": map[string]any{"SDN.Use": 1},
	}
}

func healthyResponses() map[string]any {
	return map[string]any{
		"/version":            map[string]any{"version": "9.0.3", "release": "9.0"},
		"/access/permissions": allPrivileges(),
		"/nodes/pve1/storage/local-lvm/status": map[string]any{
			"type": "lvmthin", "active": 1, "enabled": 1, "content": "images,rootdir", "avail": 100 << 30,
		},
		"/cluster/resources": []map[string]any{
			{"type": "qemu", "vmid": 10000, "node": "pve1", "pool": "par-runners", "template": 1},
			{"type": "qemu", "vmid": 100, "node": "pve1", "pool": "other"},
		},
	}
}

func TestCheckProxmox(t *testing.T) {
	tests := []struct {
		name       string
		modify     func(map[string]any)
		secretMode os.FileMode
		wantFail   bool
		want       []string
	}{
		{
			name: "healthy",
			want: []string{
				"ok    Proxmox VE 9.0.3",
				"ok    privileges on /pool/par-runners",
				"ok    privileges on /storage/local-lvm",
				"ok    privileges on /sdn/zones/parzone/parnet",
				"ok    storage local-lvm (lvmthin) on node pve1 is active and accepts VM disks, 100.0 GiB free",
				"ok    list VMs: 1 in pool par-runners on node pve1",
			},
		},
		{
			name:     "old Proxmox VE",
			modify:   func(r map[string]any) { r["/version"] = map[string]any{"version": "8.4.1", "release": "8.4"} },
			wantFail: true,
			want:     []string{"FAIL  Proxmox VE 8.4.1: only 9.x is supported"},
		},
		{
			name: "missing privileges",
			modify: func(r map[string]any) {
				perms := allPrivileges()
				delete(perms["/pool/par-runners"].(map[string]any), "VM.GuestAgent.FileWrite")
				delete(perms, "/sdn/zones/parzone/parnet")
				r["/access/permissions"] = perms
			},
			wantFail: true,
			want: []string{
				"FAIL  privileges on /pool/par-runners: missing VM.GuestAgent.FileWrite (grant it to both the " +
					"token and its user)",
				"ok    privileges on /storage/local-lvm",
				"FAIL  privileges on /sdn/zones/parzone/parnet: missing SDN.Use (grant it to both the token and its user)",
			},
		},
		{
			name: "storage without VM disks",
			modify: func(r map[string]any) {
				r["/nodes/pve1/storage/local-lvm/status"] = map[string]any{"active": 1, "enabled": 1, "content": "iso"}
			},
			wantFail: true,
			want:     []string{`FAIL  storage local-lvm doesn't accept VM disks (content type "images")`},
		},
		{
			name:     "API unreachable with this token",
			modify:   func(r map[string]any) { delete(r, "/version") },
			wantFail: true,
			want:     []string{"FAIL  reach https://127.0.0.1:"},
		},
		{
			name:       "secret readable by others",
			secretMode: 0o644,
			wantFail:   true,
			want:       []string{"FAIL  token secret: secret file", "must not be accessible to group or others"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			responses := healthyResponses()
			if tt.modify != nil {
				tt.modify(responses)
			}
			srv := fakeProxmox(t, responses)
			mode := tt.secretMode
			if mode == 0 {
				mode = 0o600
			}
			cfg := writeCheckConfig(t, srv, mode)

			var stdout, stderr bytes.Buffer
			err := run([]string{"check", "proxmox", "-config", cfg}, &stdout, &stderr)
			if got := outcomeOf(err); (got == failed) != tt.wantFail || got == usageError {
				t.Fatalf("run error = %v (outcome %s), wantFail %v\n%s", err, got, tt.wantFail, stdout.String())
			}
			for _, want := range tt.want {
				if !strings.Contains(stdout.String(), want) {
					t.Errorf("output doesn't contain %q:\n%s", want, stdout.String())
				}
			}
			if strings.Contains(stdout.String()+stderr.String(), "00000000-0000-0000-0000-000000000000") {
				t.Error("output leaks the token secret")
			}
		})
	}
}

func TestCheckGitHubCredentialFailures(t *testing.T) {
	tests := []struct {
		name string
		key  string
		mode os.FileMode
		want string
	}{
		{"key readable by others", "-----BEGIN RSA PRIVATE KEY-----\n", 0o644,
			"must not be accessible to group or others"},
		{"key not PEM", "not a key", 0o600, "not PEM-encoded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			key := filepath.Join(dir, "github-app.pem")
			if err := os.WriteFile(key, []byte(tt.key), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(key, tt.mode); err != nil {
				t.Fatal(err)
			}
			cfg := filepath.Join(dir, "config.yaml")
			data := fmt.Sprintf(`proxmox:
  url: https://pve.example.com:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: /etc/pve-token
  node: pve1
  pool: par-runners
  storage: local-lvm
  vnet: parnet
  vmidRange: {start: 10000, end: 10999}
github:
  configUrl: https://github.com/my-org
  app: {clientId: Iv23liEXAMPLE0000000, installationId: 1, privateKeyFile: %s}
scaleSets:
  - {name: proxmox-ubuntu-26.04, maxRunners: 2}
`, key)
			if err := os.WriteFile(cfg, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}

			var stdout, stderr bytes.Buffer
			err := run([]string{"check", "github", "-config", cfg}, &stdout, &stderr)
			if outcomeOf(err) != failed {
				t.Fatalf("run error = %v, want a failure\n%s", err, stdout.String())
			}
			if !strings.Contains(stdout.String(), "FAIL  GitHub App: ") || !strings.Contains(stdout.String(), tt.want) {
				t.Errorf("output doesn't mention %q:\n%s", tt.want, stdout.String())
			}
		})
	}
}
