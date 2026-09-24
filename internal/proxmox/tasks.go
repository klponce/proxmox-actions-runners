package proxmox

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Task exit statuses. Anything else is an error message.
const (
	taskOK = "OK"
	// taskWarnings prefixes the exit status of a task that succeeded with warnings, such as "WARNINGS: 2".
	taskWarnings = "WARNINGS"
)

type taskStatus struct {
	Status     string `json:"status"`
	ExitStatus string `json:"exitstatus"`
	Type       string `json:"type"`
}

// upidNode returns the node a task runs on. A UPID looks like UPID:<node>:<pid>:<pstart>:<start>:<type>:<id>:<user>:.
func upidNode(upid string) (string, error) {
	parts := strings.Split(upid, ":")
	if len(parts) < 9 || parts[0] != "UPID" || parts[1] == "" {
		return "", fmt.Errorf("invalid task ID %q", upid)
	}
	return parts[1], nil
}

// WaitTask polls a task until it stops. It returns nil if the task succeeded, including with warnings, and a
// *TaskError otherwise. If ctx ends first, the task keeps running in Proxmox and ctx's error is returned.
func (c *Client) WaitTask(ctx context.Context, upid string) error {
	node, err := upidNode(upid)
	if err != nil {
		return err
	}
	path := "/nodes/" + joinSegments(node, "tasks", upid, "status")
	return c.poll(ctx, func() (bool, error) {
		var st taskStatus
		if err := c.get(ctx, path, nil, &st); err != nil {
			return false, fmt.Errorf("task status: %w", err)
		}
		if st.Status != "stopped" {
			return false, nil
		}
		if st.ExitStatus == taskOK || strings.HasPrefix(st.ExitStatus, taskWarnings) {
			return true, nil
		}
		return true, &TaskError{UPID: upid, Type: st.Type, ExitStatus: st.ExitStatus}
	})
}

// runTask sends a request that starts a task and waits for the task. A response without a task ID means the
// request finished synchronously.
func (c *Client) runTask(ctx context.Context, request func(out *string) error) error {
	var upid string
	if err := request(&upid); err != nil {
		return err
	}
	if upid == "" {
		return nil
	}
	return c.WaitTask(ctx, upid)
}

// poll calls check until it reports done or fails, sleeping between calls with exponential backoff.
func (c *Client) poll(ctx context.Context, check func() (done bool, err error)) error {
	interval := c.pollInterval
	for {
		done, err := check()
		if done || err != nil {
			return err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		interval = min(interval*2, c.maxPollInterval)
	}
}
