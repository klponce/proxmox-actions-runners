//go:build integration

package proxmox

import (
	"context"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Integration tests run against a real Proxmox VE 9 node:
//
//	PAR_PVE_URL          https://pve.example.com:8006/api2/json
//	PAR_PVE_TOKEN_ID     par@pve!controller
//	PAR_PVE_TOKEN_SECRET the token secret
//	PAR_PVE_FINGERPRINT  optional; pins the node's certificate
//	PAR_PVE_NODE         pve1
//	PAR_PVE_STORAGE      local-lvm
//
// The lifecycle test also needs PAR_PVE_TEMPLATE_VMID, a template with the QEMU guest agent installed and enabled,
// PAR_PVE_TEST_VMID, a free VMID it may create and destroy, and optionally PAR_PVE_POOL. Run them with:
//
//	go test -tags integration ./internal/proxmox/
func integrationClient(t *testing.T) *Client {
	t.Helper()
	env := func(name string) string {
		v := os.Getenv(name)
		if v == "" {
			t.Skipf("%s is not set", name)
		}
		return v
	}
	c, err := New(Options{
		URL:            env("PAR_PVE_URL"),
		TokenID:        env("PAR_PVE_TOKEN_ID"),
		TokenSecret:    env("PAR_PVE_TOKEN_SECRET"),
		TLSFingerprint: os.Getenv("PAR_PVE_FINGERPRINT"),
		Node:           env("PAR_PVE_NODE"),
		UserAgent:      "parcon-integration-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func envVMID(t *testing.T, name string) int {
	t.Helper()
	v := os.Getenv(name)
	if v == "" {
		t.Skipf("%s is not set", name)
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return n
}

func TestIntegrationReadOnly(t *testing.T) {
	c := integrationClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	v, err := c.Version(ctx)
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	t.Logf("Proxmox VE %s", v.Version)

	perms, err := c.Permissions(ctx)
	if err != nil {
		t.Fatalf("Permissions: %v", err)
	}
	t.Logf("%d ACL paths", len(perms))

	vms, err := c.ListVMs(ctx)
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	t.Logf("%d visible VMs on %s", len(vms), c.Node())

	// The controller finds its VMs by pool, which Proxmox reports only to tokens with Pool.Audit.
	if pool, tpl := os.Getenv("PAR_PVE_POOL"), os.Getenv("PAR_PVE_TEMPLATE_VMID"); pool != "" && tpl != "" {
		want, err := strconv.Atoi(tpl)
		if err != nil {
			t.Fatalf("PAR_PVE_TEMPLATE_VMID: %v", err)
		}
		found := false
		for _, vm := range vms {
			if vm.VMID != want {
				continue
			}
			found = true
			if vm.Pool != pool || !vm.Template {
				t.Errorf("template %d is listed with pool %q and template %v; want pool %q (does the token have "+
					"Pool.Audit?)", want, vm.Pool, vm.Template, pool)
			}
		}
		if !found {
			t.Errorf("template %d isn't visible to the token", want)
		}
	}

	if storage := os.Getenv("PAR_PVE_STORAGE"); storage != "" {
		st, err := c.StorageStatus(ctx, storage)
		if err != nil {
			t.Fatalf("StorageStatus: %v", err)
		}
		t.Logf("storage %s: %+v", storage, st)
	}
}

// TestIntegrationLifecycle clones a template, configures, starts, and talks to the clone through the guest agent,
// then destroys it. It is what proves the client's requests match what Proxmox expects.
func TestIntegrationLifecycle(t *testing.T) {
	c := integrationClient(t)
	template := envVMID(t, "PAR_PVE_TEMPLATE_VMID")
	vmid := envVMID(t, "PAR_PVE_TEST_VMID")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	if err := c.Clone(ctx, CloneOptions{SourceVMID: template, NewVMID: vmid, Name: "par-integration-test",
		Pool: os.Getenv("PAR_PVE_POOL")}); err != nil {
		t.Fatalf("Clone: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()
		if err := c.Stop(ctx, vmid); err != nil {
			t.Logf("Stop: %v", err)
		}
		if err := c.Destroy(ctx, vmid); err != nil {
			t.Errorf("Destroy VM %d: %v; remove it by hand", vmid, err)
		}
	})

	if err := c.SetConfig(ctx, vmid, map[string]string{"tags": FormatTags([]string{"par-test"})}); err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	if err := c.GrowDisk(ctx, vmid, "scsi0", 1); err != nil {
		t.Fatalf("GrowDisk: %v", err)
	}
	if err := c.Start(ctx, vmid); err != nil {
		t.Fatalf("Start: %v", err)
	}

	err := c.poll(ctx, func() (bool, error) {
		err := c.AgentPing(ctx, vmid)
		if IsAgentNotReady(err) {
			return false, nil
		}
		return err == nil, err
	})
	if err != nil {
		t.Fatalf("waiting for the guest agent: %v", err)
	}

	const path = "/tmp/par-integration-test"
	content := "written through the guest agent\n"
	if err := c.AgentWriteFile(ctx, vmid, path, []byte(content)); err != nil {
		t.Fatalf("AgentWriteFile: %v", err)
	}
	res, err := c.AgentExec(ctx, vmid, []string{"/bin/sh", "-c", "cat " + path + "; cat; exit 7"}, []byte("stdin\n"))
	if err != nil {
		t.Fatalf("AgentExec: %v", err)
	}
	if res.ExitCode != 7 || string(res.Stdout) != content+"stdin\n" {
		t.Errorf("AgentExec = exit %d, stdout %q", res.ExitCode, res.Stdout)
	}

	st, err := c.Status(ctx, vmid)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Status != "running" || !strings.Contains(strings.Join(st.Tags, ";"), "par-test") {
		t.Errorf("Status = %+v", st)
	}
}
