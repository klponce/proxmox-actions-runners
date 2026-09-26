package installer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/release"
	"github.com/klponce/proxmox-actions-runners/internal/term"
)

// Secrets the fake node hands out. Tests check that none shows up in a command line, the output, or an error.
const (
	tokenSecret  = "SENTINEL-token-secret"
	manifestCode = "SENTINEL-manifest-code"
	appKeyPEM    = "-----BEGIN RSA PRIVATE KEY-----\nSENTINEL-app-key\n-----END RSA PRIVATE KEY-----\n"
)

type fakeVM struct {
	id       int
	name     string
	pool     string
	tags     []string
	template bool
	running  bool
	memMiB   int
	config   map[string]string
	// files are files in the guest, for the controller.
	files map[string]string
}

// fakeNode is a Proxmox node that interprets the commands parcon runs, keeping its state in memory.
type fakeNode struct {
	t   *testing.T
	mu  sync.Mutex
	sys string // the fake sysfs /sys/class/net

	pools   map[string]string // ID → comment
	roles   map[string]string // ID → privileges
	users   []string
	tokens  []string
	acls    []pvecli.ACL
	zones   []string
	vnets   []string
	pending []map[string]string // pending SDN objects of others: {"zone": "other", "state": "new"}
	vms     map[int]*fakeVM
	nextID  int
	storage pvecli.StorageStatus
	addrs   string // ip -j -4 addr show
	routes  string // ip -j -4 route show
	// offloadsOn are the NICs whose offloads are on.
	offloadsOn map[string]bool

	// guest handles commands run in a VM; nil means the defaults in guestDefault.
	guest func(vm *fakeVM, argv []string, stdin []byte) (exit int, stdout, stderr string, handled bool)
	// failOnce makes the first command whose line starts with a key fail.
	failOnce map[string]bool

	calls []pvecli.Cmd
}

func newFakeNode(t *testing.T, sysNet string) *fakeNode {
	return &fakeNode{
		t: t, sys: sysNet,
		pools: map[string]string{}, roles: map[string]string{}, vms: map[int]*fakeVM{}, nextID: 100,
		storage: pvecli.StorageStatus{Type: "lvmthin", Content: "images,rootdir", Active: 1, Enabled: 1,
			Avail: 200 << 30},
		addrs: `[{"ifname":"lo","addr_info":[{"local":"127.0.0.1","prefixlen":8}]},
			{"ifname":"vmbr0","addr_info":[{"local":"192.0.2.5","prefixlen":24}]}]`,
		routes:     `[{"dst":"default","dev":"vmbr0"},{"dst":"192.0.2.0/24","dev":"vmbr0"}]`,
		offloadsOn: map[string]bool{},
		failOnce:   map[string]bool{},
	}
}

func (n *fakeNode) Run(_ context.Context, c pvecli.Cmd) ([]byte, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.calls = append(n.calls, c)
	line := c.String()
	for prefix := range n.failOnce {
		if strings.HasPrefix(line, prefix) {
			delete(n.failOnce, prefix)
			return nil, &pvecli.ExitError{Cmd: line, Err: fmt.Errorf("exit status 1"), Stderr: "failing as asked"}
		}
	}
	out, err := n.run(c)
	if err != nil {
		return nil, &pvecli.ExitError{Cmd: line, Err: err}
	}
	return []byte(out), nil
}

func jsonOf(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// flag returns the value after name in args.
func flagOf(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func (n *fakeNode) vm(id string) (*fakeVM, error) {
	i, _ := strconv.Atoi(id)
	vm, ok := n.vms[i]
	if !ok {
		return nil, fmt.Errorf("VM %s does not exist", id)
	}
	return vm, nil
}

func (n *fakeNode) run(c pvecli.Cmd) (string, error) {
	a := c.Args
	switch name := filepath.Base(c.Name); name {
	case "pvesh":
		return n.pvesh(a)
	case "pveum":
		return n.pveum(a)
	case "qm":
		return n.qm(a, c.Stdin)
	case "ip":
		if slices.Contains(a, "addr") {
			return n.addrs, nil
		}
		return n.routes, nil
	case "ethtool":
		nic := a[1]
		if a[0] == "-K" {
			n.offloadsOn[nic] = a[3] == "on"
			return "", nil
		}
		s := "off"
		if n.offloadsOn[nic] {
			s = "on"
		}
		return fmt.Sprintf("Features for %s:\ntx-checksumming: %s\ntcp-segmentation-offload: %s\n"+
			"generic-segmentation-offload: %s\ngeneric-receive-offload: %s\n", nic, s, s, s, s), nil
	case "pveversion":
		return "pve-manager/9.1.2/0123456789abcdef (running kernel: 6.17.2-1-pve)\n", nil
	case "timedatectl":
		return "yes\n", nil
	case "systemd-detect-virt":
		return "", fmt.Errorf("none")
	case "dpkg-query":
		return "install ok installed", nil
	}
	return "", fmt.Errorf("the fake node doesn't know %s", c)
}

func (n *fakeNode) pvesh(a []string) (string, error) {
	path := a[1]
	switch {
	case a[0] == "get" && path == "/nodes":
		return `[{"node":"pve1"}]`, nil
	case a[0] == "get" && path == "/cluster/resources":
		var rs []map[string]any
		for _, vm := range n.vms {
			tmpl, status := 0, "stopped"
			if vm.template {
				tmpl = 1
			}
			if vm.running {
				status = "running"
			}
			rs = append(rs, map[string]any{"type": "qemu", "vmid": vm.id, "name": vm.name, "node": "pve1",
				"pool": vm.pool, "tags": strings.Join(vm.tags, ";"), "template": tmpl, "status": status,
				"maxmem": vm.memMiB << 20})
		}
		return jsonOf(rs), nil
	case a[0] == "get" && path == "/cluster/nextid":
		for n.vms[n.nextID] != nil {
			n.nextID++
		}
		return strconv.Itoa(n.nextID), nil
	case a[0] == "get" && strings.HasPrefix(path, "/nodes/pve1/storage/"):
		return jsonOf(n.storage), nil
	case a[0] == "get" && strings.HasPrefix(path, "/nodes/pve1/qemu/"):
		vm, err := n.vm(strings.Split(path, "/")[4])
		if err != nil {
			return "", err
		}
		return jsonOf(vm.config), nil
	case a[0] == "get" && slices.Contains(a, "--pending"):
		kind := strings.TrimSuffix(strings.TrimPrefix(path, "/cluster/sdn/"), "s")
		var out []map[string]string
		for _, p := range n.pending {
			if p[kind] != "" {
				out = append(out, p)
			}
		}
		return jsonOf(out), nil
	case a[0] == "get" && strings.HasPrefix(path, "/cluster/sdn/zones/"):
		if slices.Contains(n.zones, filepath.Base(path)) {
			return "{}", nil
		}
		return "", fmt.Errorf("no such zone")
	case a[0] == "get" && strings.HasPrefix(path, "/cluster/sdn/vnets/"):
		if slices.Contains(n.vnets, filepath.Base(path)) {
			return "{}", nil
		}
		return "", fmt.Errorf("no such vnet")
	case a[0] == "create" && path == "/cluster/sdn/zones":
		n.zones = append(n.zones, flagOf(a, "--zone"))
		return "", nil
	case a[0] == "create" && path == "/cluster/sdn/vnets":
		n.vnets = append(n.vnets, flagOf(a, "--vnet"))
		return "", nil
	case a[0] == "set" && path == "/cluster/sdn":
		for _, v := range n.vnets {
			_ = os.MkdirAll(filepath.Join(n.sys, v), 0o755)
		}
		return "", nil
	case a[0] == "delete" && strings.HasPrefix(path, "/cluster/sdn/zones/"):
		n.zones = slices.DeleteFunc(n.zones, func(z string) bool { return z == filepath.Base(path) })
		return "", nil
	case a[0] == "delete" && strings.HasPrefix(path, "/cluster/sdn/vnets/"):
		n.vnets = slices.DeleteFunc(n.vnets, func(v string) bool { return v == filepath.Base(path) })
		return "", nil
	}
	return "", fmt.Errorf("the fake node doesn't know pvesh %s", strings.Join(a, " "))
}

func (n *fakeNode) pveum(a []string) (string, error) {
	switch strings.Join(a[:2], " ") {
	case "pool list":
		var out []pvecli.Pool
		for id, c := range n.pools {
			out = append(out, pvecli.Pool{ID: id, Comment: c})
		}
		return jsonOf(out), nil
	case "pool add":
		n.pools[a[2]] = flagOf(a, "--comment")
		return "", nil
	case "pool delete":
		for _, vm := range n.vms {
			if vm.pool == a[2] {
				return "", fmt.Errorf("pool %s isn't empty", a[2])
			}
		}
		delete(n.pools, a[2])
		return "", nil
	case "role list":
		var out []pvecli.Role
		for id, p := range n.roles {
			out = append(out, pvecli.Role{ID: id, Privs: p})
		}
		return jsonOf(out), nil
	case "role add", "role modify":
		n.roles[a[2]] = flagOf(a, "--privs")
		return "", nil
	case "role delete":
		delete(n.roles, a[2])
		return "", nil
	case "user list":
		var out []map[string]string
		for _, u := range n.users {
			out = append(out, map[string]string{"userid": u})
		}
		return jsonOf(out), nil
	case "user add":
		n.users = append(n.users, a[2])
		return "", nil
	case "user delete":
		n.users = slices.DeleteFunc(n.users, func(u string) bool { return u == a[2] })
		return "", nil
	case "user token":
		switch a[2] {
		case "list":
			var out []map[string]string
			for _, t := range n.tokens {
				out = append(out, map[string]string{"tokenid": t})
			}
			return jsonOf(out), nil
		case "add":
			n.tokens = append(n.tokens, a[4])
			return jsonOf(map[string]string{"full-tokenid": a[3] + "!" + a[4], "value": tokenSecret}), nil
		case "remove":
			n.tokens = slices.DeleteFunc(n.tokens, func(t string) bool { return t == a[4] })
			return "", nil
		}
	case "acl list":
		return jsonOf(n.acls), nil
	case "acl modify":
		for _, u := range []struct{ typ, id string }{{"user", flagOf(a, "--users")}, {"token", flagOf(a, "--tokens")}} {
			acl := pvecli.ACL{Path: a[2], Type: u.typ, UGID: u.id, Role: flagOf(a, "--roles")}
			if !slices.Contains(n.acls, acl) {
				n.acls = append(n.acls, acl)
			}
		}
		return "", nil
	case "acl delete":
		n.acls = slices.DeleteFunc(n.acls, func(acl pvecli.ACL) bool { return acl.Path == a[2] && acl.UGID == a[4] })
		return "", nil
	}
	return "", fmt.Errorf("the fake node doesn't know pveum %s", strings.Join(a, " "))
}

func (n *fakeNode) qm(a []string, stdin []byte) (string, error) {
	switch a[0] {
	case "create":
		id, _ := strconv.Atoi(a[1])
		if n.vms[id] != nil {
			return "", fmt.Errorf("VM %d already exists", id)
		}
		mem, _ := strconv.Atoi(flagOf(a, "--memory"))
		vm := &fakeVM{id: id, name: flagOf(a, "--name"), pool: flagOf(a, "--pool"), memMiB: mem,
			tags: strings.Split(flagOf(a, "--tags"), ";"), config: map[string]string{}, files: map[string]string{}}
		for _, k := range []string{"net0", "net1"} {
			if v := flagOf(a, "--"+k); v != "" {
				vm.config[k] = v
			}
		}
		n.vms[id] = vm
		return "", nil
	case "set":
		vm, err := n.vm(a[1])
		if err != nil {
			return "", err
		}
		for i := 2; i+1 < len(a); i += 2 {
			k := strings.TrimPrefix(a[i], "--")
			vm.config[k] = a[i+1]
			if k == "tags" {
				vm.tags = strings.Split(a[i+1], ";")
			}
		}
		return "", nil
	case "template":
		vm, err := n.vm(a[1])
		if err != nil {
			return "", err
		}
		vm.template = true
		return "", nil
	case "start", "stop":
		vm, err := n.vm(a[1])
		if err != nil {
			return "", err
		}
		vm.running = a[0] == "start"
		return "", nil
	case "destroy":
		vm, err := n.vm(a[1])
		if err != nil {
			return "", err
		}
		delete(n.vms, vm.id)
		return "", nil
	case "clone":
		src, err := n.vm(a[1])
		if err != nil {
			return "", err
		}
		id, _ := strconv.Atoi(a[2])
		n.vms[id] = &fakeVM{id: id, name: flagOf(a, "--name"), pool: flagOf(a, "--pool"), tags: slices.Clone(src.tags),
			config: map[string]string{"scsi0": "clone"}, files: map[string]string{}}
		return "", nil
	case "guest":
		vm, err := n.vm(a[2])
		if err != nil {
			return "", err
		}
		if !vm.running {
			return "", fmt.Errorf("VM %d is not running", vm.id)
		}
		switch a[1] {
		case "cmd":
			if a[3] == "network-get-interfaces" {
				return jsonOf([]map[string]any{{"name": "eth0", "ip-addresses": []map[string]string{
					{"ip-address-type": "ipv4", "ip-address": fmt.Sprintf("192.0.2.%d", vm.id%200)}}}}), nil
			}
			return "{}", nil
		case "exec":
			i := slices.Index(a, "--")
			exit, stdout, stderr := n.exec(vm, a[i+1:], stdin)
			return jsonOf(map[string]any{"exited": 1, "exitcode": exit, "out-data": stdout, "err-data": stderr}), nil
		}
	}
	return "", fmt.Errorf("the fake node doesn't know qm %s", strings.Join(a, " "))
}

// exec runs a command in a guest.
func (n *fakeNode) exec(vm *fakeVM, argv []string, stdin []byte) (int, string, string) {
	if n.guest != nil {
		if exit, stdout, stderr, ok := n.guest(vm, argv, stdin); ok {
			return exit, stdout, stderr
		}
	}
	if len(argv) > 4 && argv[0] == "runuser" {
		argv = argv[4:]
	}
	line := strings.Join(argv, " ")
	switch {
	case line == "systemctl is-system-running --wait":
		return 0, "running\n", ""
	case strings.HasPrefix(line, "sh -c umask 077 && cat"):
		vm.files[argv[len(argv)-1]] = string(stdin)
		return 0, "", ""
	case argv[0] == "test":
		if vm.files[argv[2]] != "" {
			return 0, "", ""
		}
		return 1, "", ""
	case argv[0] == "mv":
		vm.files[argv[2]] = vm.files[argv[1]]
		delete(vm.files, argv[1])
		return 0, "", ""
	case argv[0] == "rm":
		delete(vm.files, argv[2])
		return 0, "", ""
	case strings.HasPrefix(line, "parcon check"), line == "systemctl enable --now parcon.service",
		line == "systemctl disable --now parcon.service", argv[0] == "/usr/local/sbin/par-gateway-configure":
		return 0, "", ""
	case line == "parcon github app create":
		if strings.TrimSpace(string(stdin)) != manifestCode {
			return 1, "", "bad code"
		}
		return 0, `{"clientId":"Iv23liEXAMPLE","appId":1,"slug":"my-runners","owner":"my-org","ownerType":"Organization"}`, ""
	case line == "parcon github app import":
		return 0, "", ""
	case strings.HasPrefix(line, "parcon github app wait-installation"):
		return 0, `{"installationId":7,"account":"my-org","accountType":"Organization"}`, ""
	case line == "parcon github scaleset delete":
		return 0, "scale set proxmox-ubuntu-26.04: deleted (ID 3)\n", ""
	case argv[0] == "journalctl":
		return 0, "starting\nscale set session opened\n", ""
	case argv[0] == "curl" && strings.Contains(line, "api.github.com"):
		return 0, "", ""
	case argv[0] == "curl", argv[0] == "timeout":
		return 7, "", "blocked"
	}
	n.t.Errorf("the fake guest doesn't know %q", line)
	return 127, "", "unknown command"
}

// lines returns the command lines run so far.
func (n *fakeNode) lines() []string {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]string, len(n.calls))
	for i, c := range n.calls {
		out[i] = c.String()
	}
	return out
}

// ran reports whether a command line starting with prefix ran.
func (n *fakeNode) ran(prefix string) bool {
	return slices.ContainsFunc(n.lines(), func(l string) bool { return strings.HasPrefix(l, prefix) })
}

func (n *fakeNode) vmsTagged(tag string) []int {
	n.mu.Lock()
	defer n.mu.Unlock()
	var ids []int
	for id, vm := range n.vms {
		if slices.Contains(vm.tags, tag) {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// fakeClock is a clock that Sleep moves forward.
type fakeClock struct{ now time.Time }

func (c *fakeClock) Now() time.Time { return c.now }

func (c *fakeClock) Sleep(ctx context.Context, d time.Duration) error {
	c.now = c.now.Add(d)
	return ctx.Err()
}

// testInstaller is an Installer on a fake node, with release 0.2.0's assets in a local directory.
type testInstaller struct {
	*Installer
	node   *fakeNode
	stdout *bytes.Buffer
	stderr *bytes.Buffer
	term   *term.Scripted
	dir    string
}

func newTestInstaller(t *testing.T) *testInstaller {
	t.Helper()
	dir := t.TempDir()
	paths := hostsys.Paths{
		PVEDir:     filepath.Join(dir, "pve"),
		SysNet:     filepath.Join(dir, "sys/class/net"),
		MemInfo:    filepath.Join(dir, "meminfo"),
		Interfaces: filepath.Join(dir, "interfaces"),
		KVM:        filepath.Join(dir, "kvm"),
		UdevRule:   filepath.Join(dir, "udev/90-par-offloads.rules"),
		Ethtool:    filepath.Join(dir, "sbin/ethtool"),
	}
	must(t, os.MkdirAll(filepath.Join(paths.PVEDir, "nodes/pve1"), 0o755))
	must(t, os.MkdirAll(filepath.Join(paths.SysNet, "vmbr0/bridge"), 0o755))
	must(t, os.MkdirAll(filepath.Join(paths.SysNet, "vmbr0/brif"), 0o755))
	must(t, os.MkdirAll(filepath.Dir(paths.UdevRule), 0o755))
	must(t, os.WriteFile(paths.MemInfo, []byte("MemTotal: 65011712 kB\n"), 0o644))
	must(t, os.WriteFile(paths.Interfaces, []byte("auto lo\nsource /etc/network/interfaces.d/*\n"), 0o644))
	must(t, os.WriteFile(paths.KVM, nil, 0o644))
	writeNodeCert(t, paths.PVEDir)

	node := newFakeNode(t, paths.SysNet)
	stdout, stderr := &bytes.Buffer{}, &bytes.Buffer{}
	out := &term.Out{W: stdout, Err: stderr}
	scripted := &term.Scripted{}
	clock := &fakeClock{now: time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)}
	v, _ := release.ParseVersion("0.2.0")
	assets := filepath.Join(dir, "assets")
	writeAssets(t, assets, v)
	self := filepath.Join(dir, "self-parcon")
	must(t, os.WriteFile(self, []byte("parcon 0.2.0"), 0o755))

	in := &Installer{
		PVE:          pvecli.PVE{Exec: node},
		Change:       &pvecli.Changer{Exec: node, Out: stdout},
		Sys:          hostsys.System{Paths: paths, Exec: node},
		Term:         scripted,
		Out:          out,
		SettingsPath: filepath.Join(dir, "etc/settings.yaml"),
		BinaryPath:   filepath.Join(dir, "bin/parcon"),
		Self:         self,
		Version:      v,
		Assets:       assets,
		Reach:        func(context.Context, string) error { return nil },
		Now:          clock.Now,
		Sleep:        clock.Sleep,
		LockPath:     filepath.Join(dir, "lock"),
	}
	must(t, os.MkdirAll(filepath.Dir(in.BinaryPath), 0o755))
	geteuid = func() int { return 0 }
	lookPath = func(file string) (string, error) { return "/usr/sbin/" + file, nil }
	t.Cleanup(func() { geteuid, lookPath = os.Geteuid, defaultLookPath })
	return &testInstaller{Installer: in, node: node, stdout: stdout, stderr: stderr, term: scripted, dir: dir}
}

// writeAssets writes a release's assets and their SHA256SUMS.
func writeAssets(t *testing.T, dir string, v release.Version) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o755))
	files := map[string]string{
		runnerImage(v):     "runner image",
		runnerManifest(v):  `{"builds":[{"custom_data":{"runner_version":"2.338.0"}}]}`,
		gatewayImage(v):    "gateway image",
		controllerImage(v): "controller image",
		BinaryAsset(v):     "parcon " + v.String(),
	}
	var sums strings.Builder
	for name, content := range files {
		must(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644))
		sum := sha256.Sum256([]byte(content))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
	}
	must(t, os.WriteFile(filepath.Join(dir, "SHA256SUMS"), []byte(sums.String()), 0o644))
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func netipMust(s string) netip.Addr { return netip.MustParseAddr(s) }
