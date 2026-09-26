package installer

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

// ExecError is a command in a VM that exited with a non-zero status.
type ExecError struct {
	VMID     int
	Command  string
	ExitCode int
	Stderr   string
}

func (e *ExecError) Error() string {
	msg := fmt.Sprintf("%s in VM %d exited with status %d", e.Command, e.VMID, e.ExitCode)
	if e.Stderr != "" {
		msg += ": " + e.Stderr
	}
	return msg
}

// guestExec runs argv in a VM through the guest agent and returns its stdout. stdin, which may hold a secret, goes
// only to the command. A non-zero exit is an *ExecError with the command's stderr.
func (in *Installer) guestExec(ctx context.Context, vmid int, timeout time.Duration, stdin []byte, argv ...string) (
	[]byte, error,
) {
	res, err := in.PVE.GuestExec(ctx, vmid, timeout, argv, stdin)
	if err != nil {
		return nil, err
	}
	if res.ExitCode != 0 || res.Signal != 0 {
		return res.Stdout, &ExecError{VMID: vmid, Command: argv[0], ExitCode: res.ExitCode,
			Stderr: strings.TrimSpace(string(res.Stderr))}
	}
	return res.Stdout, nil
}

// asParcon runs argv in the controller VM as the parcon user.
func (in *Installer) asParcon(ctx context.Context, vmid int, timeout time.Duration, stdin []byte, argv ...string) (
	[]byte, error,
) {
	return in.guestExec(ctx, vmid, timeout, stdin, append([]string{"runuser", "-u", "parcon", "--"}, argv...)...)
}

// writeControllerFile writes data to a file in the controller VM, owned by parcon with mode 0600. The data travels
// on stdin, so it may be a secret.
func (in *Installer) writeControllerFile(ctx context.Context, vmid int, path string, data []byte) error {
	in.Change.Log(fmt.Sprintf("controller VM %d: write %s", vmid, path))
	if in.Change.DryRun {
		return nil
	}
	_, err := in.asParcon(ctx, vmid, 30*time.Second, data, "sh", "-c", `umask 077 && cat >"$1"`, "sh", path)
	if err != nil {
		return fmt.Errorf("write %s in the controller VM: %w", path, err)
	}
	return nil
}

// waitAgent waits until a VM's guest agent answers.
func (in *Installer) waitAgent(ctx context.Context, vmid int) error {
	for deadline := in.Now().Add(5 * time.Minute); ; {
		if err := in.PVE.AgentPing(ctx, vmid); err == nil {
			return nil
		}
		if in.Now().After(deadline) {
			return fmt.Errorf("VM %d's guest agent didn't answer within 5 minutes", vmid)
		}
		if err := in.Sleep(ctx, 2*time.Second); err != nil {
			return err
		}
	}
}

// waitBooted waits until a new VM has finished booting. Its guest agent answers early in boot, while cloud-init
// and units waiting for the network are still starting, and a service restarted then waits behind them. A unit that
// failed ("degraded") doesn't stop the install; the steps that follow check what they need.
func (in *Installer) waitBooted(ctx context.Context, vmid int) error {
	if err := in.waitAgent(ctx, vmid); err != nil {
		return err
	}
	out, err := in.guestExec(ctx, vmid, 10*time.Minute, nil, "systemctl", "is-system-running", "--wait")
	var exitErr *ExecError
	if err != nil && !errors.As(err, &exitErr) {
		return fmt.Errorf("VM %d didn't finish booting within 10 minutes: %w", vmid, err)
	}
	switch state := strings.TrimSpace(string(out)); state {
	case "running":
		return nil
	case "degraded":
		in.Out.Warn("VM %d finished booting with a failed unit; see systemctl --failed in it", vmid)
		return nil
	default:
		if state == "" {
			state = "unknown"
		}
		return fmt.Errorf("VM %d didn't finish booting (state: %s)", vmid, state)
	}
}

// vmIPv4 returns a VM's first global IPv4 address on its first NIC, from the guest agent, waiting up to two minutes
// for one.
func (in *Installer) vmIPv4(ctx context.Context, vmid int) (netip.Addr, error) {
	for deadline := in.Now().Add(2 * time.Minute); ; {
		if a, ok := in.agentIPv4(ctx, vmid); ok {
			return a, nil
		}
		if in.Now().After(deadline) {
			return netip.Addr{}, fmt.Errorf("VM %d got no IPv4 address", vmid)
		}
		if err := in.Sleep(ctx, 2*time.Second); err != nil {
			return netip.Addr{}, err
		}
	}
}

func (in *Installer) agentIPv4(ctx context.Context, vmid int) (netip.Addr, bool) {
	out, err := in.PVE.Exec.Run(ctx, pvecli.C("qm", "guest", "cmd", strconv.Itoa(vmid), "network-get-interfaces"))
	if err != nil {
		return netip.Addr{}, false
	}
	var ifs []struct {
		Name  string `json:"name"`
		Addrs []struct {
			Type string `json:"ip-address-type"`
			IP   string `json:"ip-address"`
		} `json:"ip-addresses"`
	}
	if json.Unmarshal(out, &ifs) != nil {
		return netip.Addr{}, false
	}
	for _, i := range ifs {
		// The gateway's worker NIC is net1; only the LAN address matters.
		if i.Name == "lo" || i.Name == "net1" {
			continue
		}
		for _, a := range i.Addrs {
			ip, err := netip.ParseAddr(a.IP)
			if a.Type == "ipv4" && err == nil && ip.Is4() && !ip.IsLinkLocalUnicast() {
				return ip, true
			}
		}
	}
	return netip.Addr{}, false
}
