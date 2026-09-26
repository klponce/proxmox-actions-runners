package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/installer"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/term"
)

// onHost runs parcon as on a Proxmox host whose settings are s (none if nil).
func onHost(t *testing.T, s *settings.Settings, args ...string) (stdout, stderr string, err error) {
	t.Helper()
	dir := t.TempDir()
	meminfo := filepath.Join(dir, "meminfo")
	if werr := os.WriteFile(meminfo, []byte("MemTotal: 65011712 kB\n"), 0o644); werr != nil {
		t.Fatal(werr)
	}
	path := filepath.Join(dir, "settings.yaml")
	if s != nil {
		if serr := s.Save(path); serr != nil {
			t.Fatal(serr)
		}
	}
	defer func(h func() bool, n func(io.Writer, io.Writer, bool) (*installer.Installer, error)) {
		hostMode, newInstaller = h, n
	}(hostMode, newInstaller)
	hostMode = func() bool { return true }
	newInstaller = func(stdout, stderr io.Writer, _ bool) (*installer.Installer, error) {
		return &installer.Installer{
			Out:          &term.Out{W: stdout, Err: stderr},
			SettingsPath: path,
			Sys:          hostsys.System{Paths: hostsys.Paths{MemInfo: meminfo}},
		}, nil
	}
	var out, errOut bytes.Buffer
	err = run(args, &out, &errOut)
	return out.String(), errOut.String(), err
}

func TestConfigGet(t *testing.T) {
	s := settings.Default()
	s.Worker.MemoryMiB = 16384
	out, _, err := onHost(t, &s, "config", "get", "worker.memory")
	if err != nil || out != "16GiB\n" {
		t.Errorf("get = %q, %v", out, err)
	}
	out, _, err = onHost(t, &s, "config", "get", "--all")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"KEY            VALUE  DEFAULT  DESCRIPTION",
		"runners.max    1      1        the most worker VMs at once, and so the most jobs at once",
		"worker.cores   2      2        vCPUs per worker VM",
		"worker.memory  16GiB  8GiB     memory per worker VM",
		"parcon config describe <key>",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("get --all lacks %q:\n%s", want, out)
		}
	}
	if _, errOut, err := onHost(t, &s, "config", "get", "worker.disk"); outcomeOf(err) != usageError ||
		!strings.Contains(errOut, "the keys are runners.max, worker.cores, worker.memory") {
		t.Errorf("unknown key: %v, %s", err, errOut)
	}
	if _, _, err := onHost(t, nil, "config", "get", "worker.cores"); err == nil ||
		!strings.Contains(err.Error(), "run parcon install") {
		t.Errorf("before an install: %v", err)
	}
}

func TestConfigDescribeAndRefusedValues(t *testing.T) {
	s := settings.Default()
	describe, _, err := onHost(t, &s, "config", "describe", "worker.memory")
	if err != nil || !strings.HasPrefix(describe, "worker.memory  memory per worker VM\n  allowed:  a size in GiB or MiB") {
		t.Fatalf("describe = %q, %v", describe, err)
	}
	// set shows the same guidance for a refused value, after saying what is wrong.
	_, errOut, err := onHost(t, &s, "config", "set", "worker.memory", "16GB")
	if outcomeOf(err) != usageError {
		t.Errorf("set 16GB = %v", err)
	}
	want := "parcon: \"16GB\" isn't a valid worker.memory: use GiB or MiB, such as 8GiB or 12288MiB\n\n" +
		describe
	if errOut != want {
		t.Errorf("set 16GB printed\n%s\nwant\n%s", errOut, want)
	}

	all, _, err := onHost(t, &s, "config", "describe")
	if err != nil || strings.Count(all, "  allowed:  ") != len(settings.Keys) {
		t.Errorf("describe all = %q, %v", all, err)
	}
	for _, args := range [][]string{{"config"}, {"config", "set", "worker.cores"}, {"config", "frobnicate"}} {
		if _, _, err := onHost(t, &s, args...); outcomeOf(err) != usageError {
			t.Errorf("%v = %v", args, err)
		}
	}
}
