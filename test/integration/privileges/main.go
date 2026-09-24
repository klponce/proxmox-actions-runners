// Command privileges prints the Proxmox VE privileges the controller's API token needs, one per line. The
// integration suite grants exactly these to its test role, so the suite can't drift from what parcon check proxmox
// enforces.
package main

import (
	"fmt"
	"slices"

	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
)

func main() {
	var privs []string
	for _, req := range proxmox.RequiredPrivileges("pool", "storage", "zone", "vnet") {
		privs = append(privs, req.Privileges...)
	}
	slices.Sort(privs)
	for _, p := range slices.Compact(privs) {
		fmt.Println(p)
	}
}
