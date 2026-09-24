package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

// VM is a QEMU VM or template on the client's node, as listed by the cluster resources index.
type VM struct {
	VMID     int
	Name     string
	Pool     string
	Tags     []string
	Template bool
	// Status is "running" or "stopped".
	Status string
	// Lock is the config lock, such as "clone" while a clone is in progress. Empty means unlocked.
	Lock string
	// MaxDiskBytes is the size of the root disk.
	MaxDiskBytes int64
	// UptimeSeconds is zero for a stopped VM.
	UptimeSeconds int64
}

// HasTag reports whether the VM has tag.
func (v VM) HasTag(tag string) bool {
	for _, t := range v.Tags {
		if t == tag {
			return true
		}
	}
	return false
}

type resource struct {
	Type     string  `json:"type"`
	VMID     pveInt  `json:"vmid"`
	Name     string  `json:"name"`
	Node     string  `json:"node"`
	Pool     string  `json:"pool"`
	Tags     string  `json:"tags"`
	Template pveBool `json:"template"`
	Status   string  `json:"status"`
	Lock     string  `json:"lock"`
	MaxDisk  pveInt  `json:"maxdisk"`
	Uptime   pveInt  `json:"uptime"`
}

// ListVMs returns the QEMU VMs and templates on the client's node that the token can see, sorted by VMID.
func (c *Client) ListVMs(ctx context.Context) ([]VM, error) {
	var resources []resource
	if err := c.get(ctx, "/cluster/resources", url.Values{"type": {"vm"}}, &resources); err != nil {
		return nil, fmt.Errorf("list VMs: %w", err)
	}
	vms := make([]VM, 0, len(resources))
	for _, r := range resources {
		if r.Type != "qemu" || r.Node != c.node {
			continue
		}
		vms = append(vms, VM{
			VMID:          int(r.VMID),
			Name:          r.Name,
			Pool:          r.Pool,
			Tags:          ParseTags(r.Tags),
			Template:      bool(r.Template),
			Status:        r.Status,
			Lock:          r.Lock,
			MaxDiskBytes:  int64(r.MaxDisk),
			UptimeSeconds: int64(r.Uptime),
		})
	}
	sort.Slice(vms, func(i, j int) bool { return vms[i].VMID < vms[j].VMID })
	return vms, nil
}

// VMStatus is a VM's current state.
type VMStatus struct {
	VMID int
	Name string
	// Status is "running" or "stopped".
	Status string
	// QMPStatus is QEMU's own run state, such as "running", "paused", or "prelaunch".
	QMPStatus     string
	Lock          string
	Tags          []string
	Template      bool
	MaxDiskBytes  int64
	UptimeSeconds int64
	// AgentEnabled reports whether the guest agent is enabled in the VM's config, not whether it is responding.
	AgentEnabled bool
}

// Status returns a VM's current state.
func (c *Client) Status(ctx context.Context, vmid int) (VMStatus, error) {
	var raw struct {
		VMID      pveInt  `json:"vmid"`
		Name      string  `json:"name"`
		Status    string  `json:"status"`
		QMPStatus string  `json:"qmpstatus"`
		Lock      string  `json:"lock"`
		Tags      string  `json:"tags"`
		Template  pveBool `json:"template"`
		MaxDisk   pveInt  `json:"maxdisk"`
		Uptime    pveInt  `json:"uptime"`
		Agent     pveBool `json:"agent"`
	}
	if err := c.get(ctx, c.vmPath(vmid, "status", "current"), nil, &raw); err != nil {
		return VMStatus{}, fmt.Errorf("status of VM %d: %w", vmid, err)
	}
	return VMStatus{
		VMID:          int(raw.VMID),
		Name:          raw.Name,
		Status:        raw.Status,
		QMPStatus:     raw.QMPStatus,
		Lock:          raw.Lock,
		Tags:          ParseTags(raw.Tags),
		Template:      bool(raw.Template),
		MaxDiskBytes:  int64(raw.MaxDisk),
		UptimeSeconds: int64(raw.Uptime),
		AgentEnabled:  bool(raw.Agent),
	}, nil
}

// CloneOptions describes a clone of a template.
type CloneOptions struct {
	// SourceVMID is the template to clone.
	SourceVMID int
	// NewVMID is the clone's ID. If it is taken, Clone fails with an error for which IsVMIDInUse is true.
	NewVMID int
	// Name is the clone's name. It must be a valid DNS name.
	Name string
	// Pool adds the clone to a resource pool. The token's VM.Allocate on the pool is what permits the clone.
	Pool string
	// Full makes a full copy instead of a linked clone.
	Full bool
	// Storage is the target storage for a full clone. It is ignored for linked clones, which stay on the
	// template's storage.
	Storage string
	// Description is the clone's notes. It must never contain secrets.
	Description string
}

// Clone clones a template and waits for the clone to finish.
func (c *Client) Clone(ctx context.Context, opts CloneOptions) error {
	if opts.SourceVMID <= 0 || opts.NewVMID <= 0 {
		return errors.New("clone: source and new VMID are required")
	}
	params := url.Values{"newid": {strconv.Itoa(opts.NewVMID)}}
	setIf(params, "name", opts.Name)
	setIf(params, "pool", opts.Pool)
	setIf(params, "description", opts.Description)
	if opts.Full {
		params.Set("full", "1")
		setIf(params, "storage", opts.Storage)
	} else {
		params.Set("full", "0")
	}
	err := c.runTask(ctx, func(upid *string) error {
		return c.post(ctx, c.vmPath(opts.SourceVMID, "clone"), params, upid)
	})
	if err != nil {
		return fmt.Errorf("clone VM %d to %d: %w", opts.SourceVMID, opts.NewVMID, err)
	}
	return nil
}

// SetConfig changes VM options, such as cores, memory, net0, ipconfig0, or tags, and removes the options listed in
// del. Proxmox applies the change synchronously. Values must never contain secrets: VM config is readable by
// anyone with VM.Audit.
func (c *Client) SetConfig(ctx context.Context, vmid int, settings map[string]string, del ...string) error {
	params := url.Values{}
	for k, v := range settings {
		params.Set(k, v)
	}
	if len(del) > 0 {
		params.Set("delete", strings.Join(del, ","))
	}
	if len(params) == 0 {
		return nil
	}
	if err := c.put(ctx, c.vmPath(vmid, "config"), params, nil); err != nil {
		return fmt.Errorf("configure VM %d: %w", vmid, err)
	}
	return nil
}

// GrowDisk adds addGiB gibibytes to a VM disk, such as "scsi0", and waits for the resize to finish.
func (c *Client) GrowDisk(ctx context.Context, vmid int, disk string, addGiB int) error {
	if addGiB <= 0 {
		return fmt.Errorf("grow disk %s of VM %d: size must be positive, got %d", disk, vmid, addGiB)
	}
	params := url.Values{"disk": {disk}, "size": {fmt.Sprintf("+%dG", addGiB)}}
	err := c.runTask(ctx, func(upid *string) error {
		return c.put(ctx, c.vmPath(vmid, "resize"), params, upid)
	})
	if err != nil {
		return fmt.Errorf("grow disk %s of VM %d: %w", disk, vmid, err)
	}
	return nil
}

// Start starts a VM and waits until QEMU is running. The guest OS and its agent may still be booting.
func (c *Client) Start(ctx context.Context, vmid int) error {
	return c.vmTask(ctx, vmid, "start", nil, "status", "start")
}

// Shutdown asks the guest OS to power off through ACPI or the guest agent, waiting up to timeoutSeconds, and then
// stops the VM hard if it is still running.
func (c *Client) Shutdown(ctx context.Context, vmid, timeoutSeconds int) error {
	params := url.Values{"forceStop": {"1"}, "timeout": {strconv.Itoa(timeoutSeconds)}}
	return c.vmTask(ctx, vmid, "shut down", params, "status", "shutdown")
}

// Stop stops a VM immediately, like pulling the plug.
func (c *Client) Stop(ctx context.Context, vmid int) error {
	return c.vmTask(ctx, vmid, "stop", nil, "status", "stop")
}

// ConvertToTemplate turns a stopped VM into a template.
func (c *Client) ConvertToTemplate(ctx context.Context, vmid int) error {
	return c.vmTask(ctx, vmid, "convert to template", nil, "template")
}

// Destroy deletes a stopped VM with all its disks, including disks with its VMID that its config no longer
// references, and removes it from backup, replication, and HA configuration.
func (c *Client) Destroy(ctx context.Context, vmid int) error {
	params := url.Values{"purge": {"1"}, "destroy-unreferenced-disks": {"1"}}
	err := c.runTask(ctx, func(upid *string) error {
		return c.delete(ctx, c.vmPath(vmid), params, upid)
	})
	if err != nil {
		return fmt.Errorf("destroy VM %d: %w", vmid, err)
	}
	return nil
}

// vmTask POSTs to a VM endpoint that starts a task and waits for the task.
func (c *Client) vmTask(ctx context.Context, vmid int, action string, params url.Values, segments ...string) error {
	err := c.runTask(ctx, func(upid *string) error {
		return c.post(ctx, c.vmPath(vmid, segments...), params, upid)
	})
	if err != nil {
		return fmt.Errorf("%s VM %d: %w", action, vmid, err)
	}
	return nil
}

func setIf(params url.Values, key, value string) {
	if value != "" {
		params.Set(key, value)
	}
}

// ParseTags splits a Proxmox tag list. Proxmox returns tags separated by semicolons and accepts commas and spaces.
func ParseTags(s string) []string {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' || r == ' ' })
	if len(fields) == 0 {
		return nil
	}
	return fields
}

// FormatTags joins tags for the tags config option.
func FormatTags(tags []string) string {
	return strings.Join(tags, ";")
}
