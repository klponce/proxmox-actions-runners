package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
)

// Proxmox limits for guest-agent parameters.
const (
	// MaxAgentFileBytes is the largest content AgentWriteFile accepts.
	MaxAgentFileBytes = 61440
	// MaxAgentStdinBytes is the largest stdin AgentExec accepts.
	MaxAgentStdinBytes = 65536
)

// AgentPing checks that the guest agent responds. Use IsAgentNotReady to tell "still booting" from other failures.
func (c *Client) AgentPing(ctx context.Context, vmid int) error {
	if err := c.post(ctx, c.vmPath(vmid, "agent", "ping"), nil, nil); err != nil {
		return fmt.Errorf("ping guest agent of VM %d: %w", vmid, err)
	}
	return nil
}

// AgentWriteFile writes content to path inside the guest through the guest agent, replacing any existing file.
// This is how secrets such as JIT configs reach a VM: the content is never logged or put in an error. The file is
// created with the guest agent's default permissions, so the guest should set tighter ones before using it.
func (c *Client) AgentWriteFile(ctx context.Context, vmid int, path string, content []byte) error {
	if len(content) > MaxAgentFileBytes {
		return fmt.Errorf("write %s in VM %d: content is %d bytes, more than the limit of %d",
			path, vmid, len(content), MaxAgentFileBytes)
	}
	// Proxmox base64-encodes the content for QEMU itself when encode is on, which is the default.
	params := url.Values{"file": {path}, "content": {string(content)}}
	if err := c.post(ctx, c.vmPath(vmid, "agent", "file-write"), params, nil); err != nil {
		return fmt.Errorf("write %s in VM %d: %w", path, vmid, err)
	}
	return nil
}

// ExecResult is the outcome of a command run through the guest agent.
type ExecResult struct {
	ExitCode int
	// Signal is set if the process was killed by a signal instead of exiting.
	Signal int
	Stdout []byte
	Stderr []byte
	// StdoutTruncated and StderrTruncated report output the guest agent didn't capture in full.
	StdoutTruncated bool
	StderrTruncated bool
}

// AgentExec runs a command inside the guest through the guest agent and waits for it to exit. stdin may be nil.
//
// Never put secrets in command: Proxmox includes the command line in its error messages and task logs. Pass secrets
// through stdin instead. A non-zero exit code is not an error; check ExecResult.ExitCode.
func (c *Client) AgentExec(ctx context.Context, vmid int, command []string, stdin []byte) (ExecResult, error) {
	if len(command) == 0 {
		return ExecResult{}, errors.New("guest exec: command is required")
	}
	if len(stdin) > MaxAgentStdinBytes {
		return ExecResult{}, fmt.Errorf("guest exec %q in VM %d: stdin is %d bytes, more than the limit of %d",
			command[0], vmid, len(stdin), MaxAgentStdinBytes)
	}
	params := url.Values{"command": command}
	if stdin != nil {
		params.Set("input-data", string(stdin))
	}
	var started struct {
		PID pveInt `json:"pid"`
	}
	if err := c.post(ctx, c.vmPath(vmid, "agent", "exec"), params, &started); err != nil {
		return ExecResult{}, fmt.Errorf("guest exec %q in VM %d: %w", command[0], vmid, err)
	}

	var result ExecResult
	statusPath := c.vmPath(vmid, "agent", "exec-status")
	query := url.Values{"pid": {strconv.FormatInt(int64(started.PID), 10)}}
	err := c.poll(ctx, func() (bool, error) {
		var st struct {
			Exited       pveBool `json:"exited"`
			ExitCode     pveInt  `json:"exitcode"`
			Signal       pveInt  `json:"signal"`
			OutData      string  `json:"out-data"`
			ErrData      string  `json:"err-data"`
			OutTruncated pveBool `json:"out-truncated"`
			ErrTruncated pveBool `json:"err-truncated"`
		}
		if err := c.get(ctx, statusPath, query, &st); err != nil {
			return false, err
		}
		if !st.Exited {
			return false, nil
		}
		// Proxmox decodes the agent's base64 output before returning it.
		result = ExecResult{
			ExitCode:        int(st.ExitCode),
			Signal:          int(st.Signal),
			Stdout:          []byte(st.OutData),
			Stderr:          []byte(st.ErrData),
			StdoutTruncated: bool(st.OutTruncated),
			StderrTruncated: bool(st.ErrTruncated),
		}
		return true, nil
	})
	if err != nil {
		return ExecResult{}, fmt.Errorf("guest exec %q in VM %d: wait for exit: %w", command[0], vmid, err)
	}
	return result, nil
}
