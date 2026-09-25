package proxmox

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestNewRejectsBadOptions(t *testing.T) {
	valid := Options{URL: "https://pve.example.com:8006/api2/json", TokenID: testTokenID, TokenSecret: testTokenSecret,
		Node: "pve1"}
	tests := []struct {
		name   string
		modify func(*Options)
		want   string
	}{
		{"plain HTTP", func(o *Options) { o.URL = "http://pve.example.com/api2/json" }, "https://"},
		{"no host", func(o *Options) { o.URL = "https:///api2/json" }, "https://"},
		{"no token ID", func(o *Options) { o.TokenID = "" }, "token ID and secret"},
		{"no token secret", func(o *Options) { o.TokenSecret = "" }, "token ID and secret"},
		{"no node", func(o *Options) { o.Node = "" }, "node is required"},
		{"short fingerprint", func(o *Options) { o.TLSFingerprint = "AA:BB" }, "SHA-256 fingerprint"},
		{"non-hex fingerprint", func(o *Options) { o.TLSFingerprint = strings.Repeat("ZZ:", 31) + "ZZ" }, "SHA-256"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := valid
			tt.modify(&opts)
			_, err := New(opts)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("New error = %v, want it to mention %q", err, tt.want)
			}
			if strings.Contains(err.Error(), opts.TokenSecret) && opts.TokenSecret != "" {
				t.Errorf("error leaks the token secret: %v", err)
			}
		})
	}
	if _, err := New(valid); err != nil {
		t.Fatalf("New(valid): %v", err)
	}
}

func TestTLSVerification(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodGet, "/version", reply{Data: map[string]any{"version": "9.0.3", "release": "9.0"}})

	wrong := strings.Repeat("00:", 31) + "00"
	tests := []struct {
		name        string
		fingerprint string
		wantErr     string
	}{
		{"pinned fingerprint", f.fingerprint(), ""},
		{"lowercase pinned fingerprint", strings.ToLower(f.fingerprint()), ""},
		{"wrong fingerprint", wrong, "doesn't match the pinned fingerprint"},
		{"no pin uses the system trust store", "", "certificate"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, err := New(Options{URL: f.server.URL + "/api2/json", TokenID: testTokenID, TokenSecret: testTokenSecret,
				TLSFingerprint: tt.fingerprint, Node: testNode})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			_, err = c.Version(context.Background())
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Version: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Version error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestRequestHeaders(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodGet, "/version", reply{Data: map[string]any{"version": "9.0.3", "release": "9.0"}})
	v, err := f.client().Version(context.Background())
	if err != nil {
		t.Fatalf("Version: %v", err)
	}
	if v.Major() != 9 {
		t.Errorf("Major = %d, want 9", v.Major())
	}
	req := f.last(http.MethodGet, "/version")
	if want := "PVEAPIToken=" + testTokenID + "=" + testTokenSecret; req.Auth != want {
		t.Errorf("Authorization = %q, want %q", req.Auth, want)
	}
	if req.Agent != "parcon-test" {
		t.Errorf("User-Agent = %q, want parcon-test", req.Agent)
	}
}

func TestAPIErrors(t *testing.T) {
	const secret = "jit-config-that-must-not-leak"
	tests := []struct {
		name  string
		reply reply
		check func(*testing.T, error)
	}{
		{
			name:  "message in the status line",
			reply: reply{Status: 500, Message: "VM 10001 already exists on node 'pve1'"},
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusInternalServerError ||
					apiErr.Message != "VM 10001 already exists on node 'pve1'" {
					t.Fatalf("error = %#v", err)
				}
				if !IsVMIDInUse(err) {
					t.Error("IsVMIDInUse = false")
				}
			},
		},
		{
			name:  "message in the body",
			reply: reply{Status: 403, BodyMessage: "Permission check failed (/vms/10001, VM.GuestAgent.FileWrite)\n"},
			check: func(t *testing.T, err error) {
				if !strings.Contains(err.Error(), "403 Permission check failed (/vms/10001, VM.GuestAgent.FileWrite)") {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "parameter errors",
			reply: reply{Status: 400, Message: "Parameter verification failed.",
				Errors: map[string]string{"file": "value must be absolute", "content": "too long"}},
			check: func(t *testing.T, err error) {
				want := "400 Parameter verification failed.; content: too long; file: value must be absolute"
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error = %v, want it to contain %q", err, want)
				}
			},
		},
		{
			name:  "guest agent not running",
			reply: reply{Status: 500, Message: "QEMU guest agent is not running"},
			check: func(t *testing.T, err error) {
				if !IsAgentNotReady(err) {
					t.Fatalf("IsAgentNotReady(%v) = false", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePVE(t)
			path := "/nodes/pve1/qemu/10001/agent/file-write"
			f.reply(http.MethodPost, path, tt.reply)
			err := f.client().AgentWriteFile(context.Background(), 10001, "/run/par/jitconfig", []byte(secret))
			if err == nil {
				t.Fatal("AgentWriteFile succeeded")
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), testTokenSecret) {
				t.Errorf("error leaks a secret: %v", err)
			}
			tt.check(t, err)
		})
	}
}

func TestErrorClassifiers(t *testing.T) {
	apiErr := func(status int, msg string) error { return &APIError{StatusCode: status, Message: msg} }
	tests := []struct {
		name string
		fn   func(error) bool
		err  error
		want bool
	}{
		{"in use: exists on node", IsVMIDInUse, apiErr(500, "VM 101 already exists on node 'pve1'"), true},
		{"in use: config file", IsVMIDInUse, apiErr(500, "unable to create VM 101: config file already exists"), true},
		{"in use: from a task", IsVMIDInUse, &TaskError{ExitStatus: "VM 101 already exists on node 'pve1'"}, true},
		{"in use: other error", IsVMIDInUse, apiErr(500, "storage full"), false},
		{"in use: plain error", IsVMIDInUse, errors.New("VM 101 already exists on node 'pve1'"), false},
		{"forbidden: 403", IsForbidden, apiErr(403, "Permission check failed (/vms/101, VM.Allocate)"), true},
		{"forbidden: other", IsForbidden, apiErr(500, "timeout"), false},
		{"forbidden: plain error", IsForbidden, errors.New("403 Permission check failed"), false},
		{"agent: VM stopped", IsAgentNotReady, apiErr(500, "VM 101 is not running"), true},
		{"agent: not configured", IsAgentNotReady, apiErr(500, "No QEMU guest agent configured"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.fn(tt.err); got != tt.want {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWaitTask(t *testing.T) {
	tests := []struct {
		name     string
		statuses []taskStatus
		wantErr  string
	}{
		{"succeeds", []taskStatus{running(), running(), stopped("OK")}, ""},
		{"succeeds with warnings", []taskStatus{stopped("WARNINGS: 2")}, ""},
		{"fails", []taskStatus{running(), stopped("clone failed: storage full")}, "clone failed: storage full"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePVE(t)
			upid := f.task("qmclone", tt.statuses...)
			err := f.client().WaitTask(context.Background(), upid)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("WaitTask: %v", err)
				}
				return
			}
			var taskErr *TaskError
			if !errors.As(err, &taskErr) || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("WaitTask error = %v, want a TaskError mentioning %q", err, tt.wantErr)
			}
		})
	}
}

func TestWaitTaskStopsWithContext(t *testing.T) {
	f := newFakePVE(t)
	upid := f.task("qmclone", running())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := f.client().WaitTask(ctx, upid); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitTask error = %v, want context.DeadlineExceeded", err)
	}
}

func TestListVMs(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodGet, "/cluster/resources", reply{Data: []map[string]any{
		{"type": "qemu", "vmid": 10002, "name": "par-w-2", "node": "pve1", "pool": "par-runners",
			"tags": "par-managed;par-worker", "template": 0, "status": "running", "uptime": 30, "maxdisk": 10},
		{"type": "qemu", "vmid": 10000, "name": "par-tpl", "node": "pve1", "pool": "par-runners",
			"tags": "par-managed;par-template;v1", "template": 1, "status": "stopped", "lock": ""},
		{"type": "qemu", "vmid": 200, "name": "other-node", "node": "pve2"},
		{"type": "lxc", "vmid": 300, "name": "container", "node": "pve1"},
		{"type": "qemu", "vmid": "10001", "name": "string-vmid", "node": "pve1", "template": true, "lock": "clone"},
	}})
	vms, err := f.client().ListVMs(context.Background())
	if err != nil {
		t.Fatalf("ListVMs: %v", err)
	}
	want := []VM{
		{VMID: 10000, Name: "par-tpl", Pool: "par-runners", Tags: []string{"par-managed", "par-template", "v1"},
			Template: true, Status: "stopped"},
		{VMID: 10001, Name: "string-vmid", Template: true},
		{VMID: 10002, Name: "par-w-2", Pool: "par-runners", Tags: []string{"par-managed", "par-worker"},
			Status: "running", MaxDiskBytes: 10},
	}
	if !reflect.DeepEqual(vms, want) {
		t.Errorf("ListVMs =\n%+v\nwant\n%+v", vms, want)
	}
	if !vms[0].HasTag("par-template") || vms[0].HasTag("par-worker") {
		t.Error("HasTag is wrong")
	}
	if got := f.last(http.MethodGet, "/cluster/resources").Params.Get("type"); got != "vm" {
		t.Errorf("type = %q, want vm", got)
	}
}

func TestStatus(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodGet, "/nodes/pve1/qemu/10002/status/current", reply{Data: map[string]any{
		"vmid": 10002, "name": "par-w-2", "status": "running", "qmpstatus": "running", "tags": "a;b",
		"template": "", "maxdisk": 16106127360, "uptime": 12, "agent": 1, "ha": map[string]any{"managed": 0},
	}})
	got, err := f.client().Status(context.Background(), 10002)
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	want := VMStatus{VMID: 10002, Name: "par-w-2", Status: "running", QMPStatus: "running", Tags: []string{"a", "b"},
		MaxDiskBytes: 16106127360, UptimeSeconds: 12, AgentEnabled: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Status = %+v, want %+v", got, want)
	}
}

func TestClone(t *testing.T) {
	tests := []struct {
		name       string
		opts       CloneOptions
		wantParams map[string]string
		absent     []string
	}{
		{
			name: "linked",
			opts: CloneOptions{SourceVMID: 10000, NewVMID: 10005, Name: "par-w-5", Pool: "par-runners",
				Storage: "ignored"},
			wantParams: map[string]string{"newid": "10005", "name": "par-w-5", "pool": "par-runners", "full": "0"},
			absent:     []string{"storage", "description"},
		},
		{
			name: "full",
			opts: CloneOptions{SourceVMID: 10000, NewVMID: 10006, Full: true, Storage: "local-lvm",
				Description: "managed by parcon"},
			wantParams: map[string]string{"newid": "10006", "full": "1", "storage": "local-lvm",
				"description": "managed by parcon"},
			absent: []string{"name", "pool"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePVE(t)
			f.taskReply(http.MethodPost, "/nodes/pve1/qemu/10000/clone", "qmclone", running(), stopped("OK"))
			if err := f.client().Clone(context.Background(), tt.opts); err != nil {
				t.Fatalf("Clone: %v", err)
			}
			params := f.last(http.MethodPost, "/nodes/pve1/qemu/10000/clone").Params
			for k, v := range tt.wantParams {
				if got := params.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
			for _, k := range tt.absent {
				if params.Has(k) {
					t.Errorf("%s is set to %q, want it absent", k, params.Get(k))
				}
			}
		})
	}
}

func TestCloneFailures(t *testing.T) {
	t.Run("VMID in use", func(t *testing.T) {
		f := newFakePVE(t)
		f.reply(http.MethodPost, "/nodes/pve1/qemu/10000/clone",
			reply{Status: 500, Message: "VM 10005 already exists on node 'pve1'"})
		err := f.client().Clone(context.Background(), CloneOptions{SourceVMID: 10000, NewVMID: 10005})
		if !IsVMIDInUse(err) {
			t.Fatalf("Clone error = %v, want IsVMIDInUse", err)
		}
	})
	t.Run("task fails", func(t *testing.T) {
		f := newFakePVE(t)
		f.taskReply(http.MethodPost, "/nodes/pve1/qemu/10000/clone", "qmclone",
			stopped("unable to create VM 10005: config file already exists"))
		err := f.client().Clone(context.Background(), CloneOptions{SourceVMID: 10000, NewVMID: 10005})
		if !IsVMIDInUse(err) {
			t.Fatalf("Clone error = %v, want IsVMIDInUse", err)
		}
	})
}

func TestSetConfig(t *testing.T) {
	f := newFakePVE(t)
	path := "/nodes/pve1/qemu/10005/config"
	f.reply(http.MethodPut, path, reply{})
	c := f.client()
	err := c.SetConfig(context.Background(), 10005, map[string]string{
		"cores": "2", "memory": "8192", "net0": "virtio,bridge=parnet", "ipconfig0": "ip=dhcp",
		"tags": FormatTags([]string{"par-managed", "par-worker"}),
	}, "ide2", "sshkeys")
	if err != nil {
		t.Fatalf("SetConfig: %v", err)
	}
	params := f.last(http.MethodPut, path).Params
	want := map[string]string{"cores": "2", "memory": "8192", "net0": "virtio,bridge=parnet", "ipconfig0": "ip=dhcp",
		"tags": "par-managed;par-worker", "delete": "ide2,sshkeys"}
	for k, v := range want {
		if got := params.Get(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}

	before := len(f.seen())
	if err := c.SetConfig(context.Background(), 10005, nil); err != nil {
		t.Fatalf("empty SetConfig: %v", err)
	}
	if len(f.seen()) != before {
		t.Error("empty SetConfig sent a request")
	}
}

func TestGrowDisk(t *testing.T) {
	f := newFakePVE(t)
	path := "/nodes/pve1/qemu/10005/resize"
	f.taskReply(http.MethodPut, path, "resize", stopped("OK"))
	c := f.client()
	if err := c.GrowDisk(context.Background(), 10005, "scsi0", 14); err != nil {
		t.Fatalf("GrowDisk: %v", err)
	}
	params := f.last(http.MethodPut, path).Params
	if params.Get("disk") != "scsi0" || params.Get("size") != "+14G" {
		t.Errorf("params = %v, want disk=scsi0 size=+14G", params)
	}
}

func TestPowerAndLifecycle(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		path       string
		call       func(*Client) error
		wantParams map[string]string
	}{
		{"start", http.MethodPost, "/nodes/pve1/qemu/10005/status/start",
			func(c *Client) error { return c.Start(context.Background(), 10005) }, nil},
		{"shutdown", http.MethodPost, "/nodes/pve1/qemu/10005/status/shutdown",
			func(c *Client) error { return c.Shutdown(context.Background(), 10005, 30) },
			map[string]string{"forceStop": "1", "timeout": "30"}},
		{"stop", http.MethodPost, "/nodes/pve1/qemu/10005/status/stop",
			func(c *Client) error { return c.Stop(context.Background(), 10005) }, nil},
		{"template", http.MethodPost, "/nodes/pve1/qemu/10005/template",
			func(c *Client) error { return c.ConvertToTemplate(context.Background(), 10005) }, nil},
		{"destroy", http.MethodDelete, "/nodes/pve1/qemu/10005",
			func(c *Client) error { return c.Destroy(context.Background(), 10005) },
			map[string]string{"purge": "1", "destroy-unreferenced-disks": "1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFakePVE(t)
			f.taskReply(tt.method, tt.path, tt.name, running(), stopped("OK"))
			if err := tt.call(f.client()); err != nil {
				t.Fatalf("%s: %v", tt.name, err)
			}
			params := f.last(tt.method, tt.path).Params
			for k, v := range tt.wantParams {
				if got := params.Get(k); got != v {
					t.Errorf("%s = %q, want %q", k, got, v)
				}
			}
		})
		t.Run(tt.name+" failure", func(t *testing.T) {
			f := newFakePVE(t)
			f.taskReply(tt.method, tt.path, tt.name, stopped("VM is locked (clone)"))
			var taskErr *TaskError
			if err := tt.call(f.client()); !errors.As(err, &taskErr) {
				t.Fatalf("%s error = %v, want a TaskError", tt.name, err)
			}
		})
	}
}

func TestAgentPing(t *testing.T) {
	f := newFakePVE(t)
	path := "/nodes/pve1/qemu/10005/agent/ping"
	f.reply(http.MethodPost, path, reply{Data: map[string]any{"result": map[string]any{}}})
	if err := f.client().AgentPing(context.Background(), 10005); err != nil {
		t.Fatalf("AgentPing: %v", err)
	}
}

func TestAgentWriteFile(t *testing.T) {
	f := newFakePVE(t)
	path := "/nodes/pve1/qemu/10005/agent/file-write"
	f.reply(http.MethodPost, path, reply{})
	c := f.client()
	content := []byte("line one\nline two with = & + signs\n")
	if err := c.AgentWriteFile(context.Background(), 10005, "/run/par/jitconfig", content); err != nil {
		t.Fatalf("AgentWriteFile: %v", err)
	}
	params := f.last(http.MethodPost, path).Params
	if params.Get("file") != "/run/par/jitconfig" || params.Get("content") != string(content) {
		t.Errorf("params = %v", params)
	}
	if params.Has("encode") {
		t.Error("encode is set; Proxmox must base64-encode the content itself")
	}
}

func TestAgentExec(t *testing.T) {
	f := newFakePVE(t)
	execPath := "/nodes/pve1/qemu/10005/agent/exec"
	statusPath := "/nodes/pve1/qemu/10005/agent/exec-status"
	f.reply(http.MethodPost, execPath, reply{Data: map[string]any{"pid": 4242}})
	polls := 0
	f.handle(http.MethodGet, statusPath, func(r request) reply {
		if r.Params.Get("pid") != "4242" {
			return reply{Status: 400, Message: "wrong pid"}
		}
		polls++
		if polls < 3 {
			return reply{Data: map[string]any{"exited": 0}}
		}
		return reply{Data: map[string]any{"exited": 1, "exitcode": 3, "out-data": "hello\n", "err-data": "oops\n",
			"out-truncated": 1}}
	})

	got, err := f.client().AgentExec(context.Background(), 10005, []string{"/bin/sh", "-c", "cat; exit 3"},
		[]byte("secret stdin"))
	if err != nil {
		t.Fatalf("AgentExec: %v", err)
	}
	want := ExecResult{ExitCode: 3, Stdout: []byte("hello\n"), Stderr: []byte("oops\n"), StdoutTruncated: true}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AgentExec = %+v, want %+v", got, want)
	}
	if polls != 3 {
		t.Errorf("polled %d times, want 3", polls)
	}
	params := f.last(http.MethodPost, execPath).Params
	if !reflect.DeepEqual(params["command"], []string{"/bin/sh", "-c", "cat; exit 3"}) {
		t.Errorf("command = %q", params["command"])
	}
	if params.Get("input-data") != "secret stdin" {
		t.Errorf("input-data = %q", params.Get("input-data"))
	}
}

func TestAgentExecRejects(t *testing.T) {
	f := newFakePVE(t)
	c := f.client()
	if _, err := c.AgentExec(context.Background(), 10005, nil, nil); err == nil {
		t.Error("AgentExec without a command succeeded")
	}
	if _, err := c.AgentExec(context.Background(), 10005, []string{"cat"}, make([]byte, MaxAgentStdinBytes+1)); err == nil {
		t.Error("AgentExec with oversized stdin succeeded")
	}
	if n := len(f.seen()); n != 0 {
		t.Errorf("sent %d requests, want none", n)
	}
}

func TestAgentExecWithoutStdin(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodPost, "/nodes/pve1/qemu/10005/agent/exec", reply{Data: map[string]any{"pid": 1}})
	f.reply(http.MethodGet, "/nodes/pve1/qemu/10005/agent/exec-status",
		reply{Data: map[string]any{"exited": true, "exitcode": 0}})
	if _, err := f.client().AgentExec(context.Background(), 10005, []string{"true"}, nil); err != nil {
		t.Fatalf("AgentExec: %v", err)
	}
	if f.last(http.MethodPost, "/nodes/pve1/qemu/10005/agent/exec").Params.Has("input-data") {
		t.Error("input-data is set without stdin")
	}
}

func TestStorageStatus(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodGet, "/nodes/pve1/storage/local-lvm/status", reply{Data: map[string]any{
		"type": "lvmthin", "active": 1, "enabled": 1, "shared": 0, "content": "images,rootdir",
		"total": 1000, "avail": 400, "used": 600,
	}})
	got, err := f.client().StorageStatus(context.Background(), "local-lvm")
	if err != nil {
		t.Fatalf("StorageStatus: %v", err)
	}
	want := StorageStatus{Type: "lvmthin", Active: true, Enabled: true, Content: []string{"images", "rootdir"},
		TotalBytes: 1000, AvailBytes: 400, UsedBytes: 600}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("StorageStatus = %+v, want %+v", got, want)
	}
	if !got.Accepts("images") || got.Accepts("iso") {
		t.Error("Accepts is wrong")
	}
}

func TestPermissions(t *testing.T) {
	f := newFakePVE(t)
	f.reply(http.MethodGet, "/access/permissions", reply{Data: map[string]any{
		"/":                         map[string]any{},
		"/pool/par-runners":         map[string]any{"VM.Allocate": 1, "VM.Clone": 1, "VM.Audit": 0},
		"/storage":                  map[string]any{"Datastore.Audit": 1},
		"/storage/local-lvm":        map[string]any{"Datastore.AllocateSpace": 0},
		"/sdn/zones/parzone/parnet": map[string]any{"SDN.Use": 0},
		"/sdn/zones/otherzone":      map[string]any{"SDN.Use": 1},
	}})
	perms, err := f.client().Permissions(context.Background())
	if err != nil {
		t.Fatalf("Permissions: %v", err)
	}

	tests := []struct {
		path, priv string
		want       bool
	}{
		{"/pool/par-runners", "VM.Allocate", true},
		{"/pool/par-runners", "VM.Audit", true},         // exact path, even without propagation
		{"/storage/local-lvm", "Datastore.Audit", true}, // propagated from /storage
		{"/storage/local-lvm", "Datastore.AllocateSpace", true},
		{"/storage/other", "Datastore.AllocateSpace", false}, // granted on a sibling without propagation
		{"/pool/par-runners", "VM.PowerMgmt", false},
		{"/vms/100", "VM.Audit", false},
		{"/sdn/zones/otherzone/othernet", "SDN.Use", true}, // propagated from the zone
	}
	for _, tt := range tests {
		if got := perms.Has(tt.path, tt.priv); got != tt.want {
			t.Errorf("Has(%s, %s) = %v, want %v", tt.path, tt.priv, got, tt.want)
		}
	}

	required := RequiredPrivileges("par-runners", "local-lvm", "parzone", "parnet")
	missing := perms.Missing(required[0])
	if len(missing) != len(poolPrivileges)-3 || missing[0] != "VM.Config.CPU" {
		t.Errorf("Missing on pool = %v", missing)
	}
	if m := perms.Missing(required[1]); len(m) != 0 {
		t.Errorf("Missing on storage = %v, want none", m)
	}
	if required[2].Path != "/sdn/zones/parzone/parnet" {
		t.Errorf("VNet requirement path = %q", required[2].Path)
	}
	if m := perms.Missing(required[2]); len(m) != 0 {
		t.Errorf("Missing on VNet = %v, want none", m)
	}
	if m := perms.Missing(RequiredPrivileges("par-runners", "local-lvm", "parzone", "nonet")[2]); !reflect.DeepEqual(m, vnetPrivileges) {
		t.Errorf("Missing on another VNet = %v, want %v", m, vnetPrivileges)
	}
}

func TestVersionMajor(t *testing.T) {
	tests := []struct {
		v    Version
		want int
	}{
		{Version{Release: "9.0", Version: "9.0.3"}, 9},
		{Version{Version: "8.4.1"}, 8},
		{Version{}, 0},
		{Version{Release: "x.y"}, 0},
	}
	for _, tt := range tests {
		if got := tt.v.Major(); got != tt.want {
			t.Errorf("%+v.Major() = %d, want %d", tt.v, got, tt.want)
		}
	}
}

func TestTags(t *testing.T) {
	tests := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a;b;c", []string{"a", "b", "c"}},
		{"a, b c;;", []string{"a", "b", "c"}},
	}
	for _, tt := range tests {
		if got := ParseTags(tt.in); !reflect.DeepEqual(got, tt.want) {
			t.Errorf("ParseTags(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
	if got := FormatTags([]string{"a", "b"}); got != "a;b" {
		t.Errorf("FormatTags = %q", got)
	}
}

func TestLenientJSON(t *testing.T) {
	var b pveBool
	for _, in := range []string{`"x"`, `2`} {
		if err := b.UnmarshalJSON([]byte(in)); err == nil {
			t.Errorf("pveBool accepted %s", in)
		}
	}
	var n pveInt
	if err := n.UnmarshalJSON([]byte(`"12.7"`)); err != nil || n != 12 {
		t.Errorf("pveInt(\"12.7\") = %d, %v", n, err)
	}
	if err := n.UnmarshalJSON([]byte(`"abc"`)); err == nil {
		t.Error("pveInt accepted \"abc\"")
	}
}
