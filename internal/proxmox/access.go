package proxmox

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// Version is the Proxmox VE version.
type Version struct {
	// Version is the full pve-manager version, such as "9.0.3".
	Version string
	// Release is the point release, such as "9.0".
	Release string
}

// Major returns the major version, or 0 if it can't be parsed.
func (v Version) Major() int {
	s := v.Release
	if s == "" {
		s = v.Version
	}
	major, _, _ := strings.Cut(s, ".")
	n, err := strconv.Atoi(major)
	if err != nil {
		return 0
	}
	return n
}

// Version returns the server's Proxmox VE version. Any valid token may call it.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var raw struct {
		Version string `json:"version"`
		Release string `json:"release"`
	}
	if err := c.get(ctx, "/version", nil, &raw); err != nil {
		return Version{}, fmt.Errorf("version: %w", err)
	}
	return Version{Version: raw.Version, Release: raw.Release}, nil
}

// StorageStatus is a storage's state on the client's node.
type StorageStatus struct {
	Type    string
	Active  bool
	Enabled bool
	Shared  bool
	// Content lists the content types the storage accepts, such as "images".
	Content    []string
	TotalBytes int64
	AvailBytes int64
	UsedBytes  int64
}

// Accepts reports whether the storage accepts a content type.
func (s StorageStatus) Accepts(content string) bool {
	for _, c := range s.Content {
		if c == content {
			return true
		}
	}
	return false
}

// StorageStatus returns a storage's state on the client's node. It needs Datastore.Audit or
// Datastore.AllocateSpace on the storage.
func (c *Client) StorageStatus(ctx context.Context, storage string) (StorageStatus, error) {
	var raw struct {
		Type    string  `json:"type"`
		Active  pveBool `json:"active"`
		Enabled pveBool `json:"enabled"`
		Shared  pveBool `json:"shared"`
		Content string  `json:"content"`
		Total   pveInt  `json:"total"`
		Avail   pveInt  `json:"avail"`
		Used    pveInt  `json:"used"`
	}
	if err := c.get(ctx, c.nodePath("storage", storage, "status"), nil, &raw); err != nil {
		return StorageStatus{}, fmt.Errorf("status of storage %s: %w", storage, err)
	}
	return StorageStatus{
		Type:       raw.Type,
		Active:     bool(raw.Active),
		Enabled:    bool(raw.Enabled),
		Shared:     bool(raw.Shared),
		Content:    strings.FieldsFunc(raw.Content, func(r rune) bool { return r == ',' }),
		TotalBytes: int64(raw.Total),
		AvailBytes: int64(raw.Avail),
		UsedBytes:  int64(raw.Used),
	}, nil
}

// Permissions maps ACL paths to the token's effective privileges there. A privilege's value says whether it
// propagates to paths below.
type Permissions map[string]map[string]bool

// Permissions returns the calling token's effective permissions on every path it has an ACL for, plus Proxmox's
// standard top-level paths. Any token may read its own permissions.
func (c *Client) Permissions(ctx context.Context) (Permissions, error) {
	var raw map[string]map[string]pveBool
	if err := c.get(ctx, "/access/permissions", nil, &raw); err != nil {
		return nil, fmt.Errorf("permissions: %w", err)
	}
	perms := make(Permissions, len(raw))
	for path, privs := range raw {
		perms[path] = make(map[string]bool, len(privs))
		for priv, propagate := range privs {
			perms[path][priv] = bool(propagate)
		}
	}
	return perms, nil
}

// Has reports whether priv is granted on path, either directly or by propagation from a parent path.
func (p Permissions) Has(path, priv string) bool {
	if _, ok := p[path][priv]; ok {
		return true
	}
	for parent := parentPath(path); parent != ""; parent = parentPath(parent) {
		if p[parent][priv] {
			return true
		}
	}
	return false
}

func parentPath(path string) string {
	if path == "/" || path == "" {
		return ""
	}
	i := strings.LastIndex(path, "/")
	if i <= 0 {
		return "/"
	}
	return path[:i]
}

// Requirement is a set of privileges the controller needs on one ACL path.
type Requirement struct {
	Path       string
	Privileges []string
}

// Required privileges. README.md ("Requirements") and install/install.sh must grant exactly these.
var (
	poolPrivileges = []string{
		"VM.Allocate", "VM.Clone",
		"VM.Config.CPU", "VM.Config.Memory", "VM.Config.Disk", "VM.Config.Network", "VM.Config.Cloudinit",
		"VM.Config.Options",
		"VM.PowerMgmt", "VM.Audit",
		"VM.GuestAgent.Audit", "VM.GuestAgent.FileWrite", "VM.GuestAgent.Unrestricted",
		// Without Pool.Audit, /cluster/resources leaves out each VM's pool, and the controller can't tell its own
		// VMs from others (found on PVE 9.2).
		"Pool.Audit",
	}
	storagePrivileges = []string{"Datastore.AllocateSpace", "Datastore.Audit"}
	vnetPrivileges    = []string{"SDN.Use"}
)

// RequiredPrivileges returns what the controller's token needs on the runner pool, the target storage, and the
// worker VNet in its SDN zone.
func RequiredPrivileges(pool, storage, zone, vnet string) []Requirement {
	return []Requirement{
		{Path: "/pool/" + pool, Privileges: poolPrivileges},
		{Path: "/storage/" + storage, Privileges: storagePrivileges},
		{Path: "/sdn/zones/" + zone + "/" + vnet, Privileges: vnetPrivileges},
	}
}

// Missing returns the privileges in req that p doesn't grant on req.Path.
func (p Permissions) Missing(req Requirement) []string {
	var missing []string
	for _, priv := range req.Privileges {
		if !p.Has(req.Path, priv) {
			missing = append(missing, priv)
		}
	}
	return missing
}
