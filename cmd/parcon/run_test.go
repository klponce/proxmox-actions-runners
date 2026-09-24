package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/config"
)

func TestRunUsage(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want outcome
		msg  string
	}{
		{"bad log level", []string{"run", "-log-level", "loud"}, usageError, `invalid -log-level "loud"`},
		{"extra argument", []string{"run", "extra"}, usageError, "unexpected arguments"},
		{"bad flag", []string{"run", "-nope"}, usageError, "flag provided but not defined"},
		{"help", []string{"run", "-h"}, succeeded, "-log-level"},
		{"missing config", []string{"run", "-config", filepath.Join(t.TempDir(), "missing.yaml")}, failed, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			err := run(tt.args, &stdout, &stderr)
			if got := outcomeOf(err); got != tt.want {
				t.Fatalf("run error = %v, want outcome %s", err, tt.want)
			}
			if !strings.Contains(stderr.String(), tt.msg) {
				t.Errorf("stderr = %q, want it to contain %q", stderr.String(), tt.msg)
			}
		})
	}
}

func TestNewController(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	validKey := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	const secret = "00000000-0000-0000-0000-000000000000"

	tests := []struct {
		name       string
		secretMode os.FileMode
		key        string
		want       string // empty means success
	}{
		{"valid", 0o600, validKey, ""},
		{"token secret readable by others", 0o644, validKey, "proxmox token: secret file"},
		{"GitHub key not PEM", 0o600, "not a key", "not PEM-encoded"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			secretFile := filepath.Join(dir, "pve-token")
			keyFile := filepath.Join(dir, "github-app.pem")
			if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chmod(secretFile, tt.secretMode); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(keyFile, []byte(tt.key), 0o600); err != nil {
				t.Fatal(err)
			}
			cfgFile := filepath.Join(dir, "config.yaml")
			data := fmt.Sprintf(`proxmox:
  url: https://pve.example.com:8006/api2/json
  tokenId: par@pve!controller
  tokenSecretFile: %s
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
`, secretFile, keyFile)
			if err := os.WriteFile(cfgFile, []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.Load(cfgFile)
			if err != nil {
				t.Fatal(err)
			}

			c, err := newController(cfg, slog.New(slog.DiscardHandler))
			if tt.want == "" {
				if err != nil || c == nil {
					t.Fatalf("newController = %v, %v", c, err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("newController error = %v, want it to mention %q", err, tt.want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "PRIVATE KEY") {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
}
