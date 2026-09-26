package pvecli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestOSExecReportsStderrButNeverStdin(t *testing.T) {
	out, err := OSExec{}.Run(context.Background(), Cmd{Name: "sh", Args: []string{"-c", "cat; echo broken >&2; exit 3"},
		Stdin: []byte("s3cret")})
	if string(out) != "s3cret" {
		t.Errorf("stdout = %q", out)
	}
	var exitErr *ExitError
	if !errors.As(err, &exitErr) || exitErr.Stderr != "broken" {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Errorf("the error holds stdin: %v", err)
	}
}

func TestChangerLogsThenRuns(t *testing.T) {
	fake := (&FakeExec{}).Reply("pveum pool add", "")
	var log bytes.Buffer
	ch := &Changer{Exec: fake, Out: &log}
	cmd := C("pveum", "pool", "add", "par-system")
	cmd.Stdin = []byte("s3cret")
	if _, err := ch.Run(context.Background(), cmd); err != nil {
		t.Fatal(err)
	}
	if log.String() != "    $ pveum pool add par-system\n" {
		t.Errorf("log = %q", log.String())
	}
	if len(fake.Calls) != 1 || string(fake.Calls[0].Stdin) != "s3cret" {
		t.Errorf("calls = %+v", fake.Calls)
	}

	// A dry run only logs, for commands and files alike.
	dry := &Changer{Exec: fake, Out: &log, DryRun: true}
	path := filepath.Join(t.TempDir(), "rule")
	if _, err := dry.Run(context.Background(), C("qm", "destroy", "100")); err != nil {
		t.Fatal(err)
	}
	if err := dry.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if len(fake.Calls) != 1 {
		t.Errorf("dry run ran %v", fake.Lines())
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dry run wrote %s", path)
	}
}

func TestWriteFileAtomicAndRemove(t *testing.T) {
	path := filepath.Join(t.TempDir(), "90-par-offloads.rules")
	ch := &Changer{Exec: &FakeExec{}}
	if err := ch.WriteFile(path, []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := ch.WriteFile(path, []byte("two\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(data) != "two\n" || st.Mode().Perm() != 0o600 {
		t.Errorf("file = %q, mode %v", data, st.Mode())
	}
	entries, _ := os.ReadDir(filepath.Dir(path))
	if len(entries) != 1 {
		t.Errorf("temporary files left: %v", entries)
	}
	if err := ch.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := ch.Remove(path); err != nil {
		t.Errorf("removing a file that is gone: %v", err)
	}
}

func TestReads(t *testing.T) {
	fake := (&FakeExec{}).
		Reply("pvesh get /nodes ", `[{"node":"pve1"}]`).
		Reply("pvesh get /cluster/resources", `[{"type":"qemu","vmid":101,"node":"pve1","tags":"par-managed"},
			{"type":"qemu","vmid":102,"node":"pve2"}]`).
		Reply("pvesh get /nodes/pve1/qemu/101/config", `{"net0":"virtio=BC:24:11:00:00:01,bridge=vmbr0","memory":1024,
			"tags":"par-managed;par-gateway"}`).
		Reply("pveum pool list", `[{"poolid":"par-system","comment":"proxmox-actions-runners"}]`).
		Reply("pveum user token list par@pve", `[{"tokenid":"controller"}]`).
		Reply("pvesh get /cluster/sdn/zones --pending 1", `[{"zone":"parzone","state":"new"},{"zone":"other",
			"state":"changed"},{"zone":"quiet"}]`).
		Reply("pvesh get /cluster/sdn/vnets --pending 1", `[{"vnet":"parnet","state":"new"}]`).
		Reply("pvesh get /cluster/nextid", "\"105\"\n").
		Reply("pvesh get /cluster/sdn/zones/parzone", `{}`).
		Fail("pvesh get /cluster/sdn/vnets/parnet")
	p := PVE{Exec: fake}
	ctx := context.Background()

	if node, err := p.NodeName(ctx); err != nil || node != "pve1" {
		t.Errorf("NodeName = %q, %v", node, err)
	}
	if vms, err := p.VMs(ctx, "pve1"); err != nil || len(vms) != 1 || vms[0].VMID != 101 {
		t.Errorf("VMs = %+v, %v", vms, err)
	}
	cfg, err := p.VMConfig(ctx, "pve1", 101)
	if err != nil || cfg["memory"] != "1024" || !strings.Contains(cfg["net0"], "bridge=vmbr0") {
		t.Errorf("VMConfig = %v, %v", cfg, err)
	}
	if pools, err := p.Pools(ctx); err != nil || pools[0].Comment != "proxmox-actions-runners" {
		t.Errorf("Pools = %+v, %v", pools, err)
	}
	if tokens, err := p.Tokens(ctx, "par@pve"); err != nil || !slices.Equal(tokens, []string{"controller"}) {
		t.Errorf("Tokens = %v, %v", tokens, err)
	}
	if pending, err := p.PendingSDN(ctx, "parzone", "parnet"); err != nil || !slices.Equal(pending, []string{"other"}) {
		t.Errorf("PendingSDN = %v, %v", pending, err)
	}
	if id, err := p.NextID(ctx); err != nil || id != 105 {
		t.Errorf("NextID = %d, %v", id, err)
	}
	if !p.SDNExists(ctx, "zones/parzone") || p.SDNExists(ctx, "vnets/parnet") {
		t.Error("SDNExists")
	}
}

func TestGuestExec(t *testing.T) {
	fake := (&FakeExec{}).
		Reply("qm guest exec 101 --timeout 30 -- true", `{"exited":1,"exitcode":0,"out-data":"ok\n"}`).
		Reply("qm guest exec 101 --timeout 30 --pass-stdin 1 -- sh", `{"exited":1,"exitcode":2,"err-data":"no"}`).
		Reply("qm guest exec 101 --timeout 5 -- sleep", `{"pid":42}`)
	p := PVE{Exec: fake}
	ctx := context.Background()

	if r, err := p.GuestExec(ctx, 101, 30*time.Second, []string{"true"}, nil); err != nil || string(r.Stdout) != "ok\n" {
		t.Errorf("true = %+v, %v", r, err)
	}
	r, err := p.GuestExec(ctx, 101, 30*time.Second, []string{"sh", "-c", "cat >f"}, []byte("s3cret"))
	if err != nil || r.ExitCode != 2 || string(r.Stderr) != "no" {
		t.Errorf("sh = %+v, %v", r, err)
	}
	if got := fake.Calls[1]; string(got.Stdin) != "s3cret" || strings.Contains(got.String(), "s3cret") {
		t.Errorf("stdin call = %+v", got)
	}
	if _, err := p.GuestExec(ctx, 101, 5*time.Second, []string{"sleep", "60"}, nil); !errors.Is(err, ErrExecTimeout) {
		t.Errorf("timeout = %v", err)
	}
	if _, err := p.GuestExec(ctx, 101, time.Second, []string{"x"}, make([]byte, 70000)); err == nil {
		t.Error("oversized stdin: no error")
	}
}
