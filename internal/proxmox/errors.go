package proxmox

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// APIError is a non-2xx response from the Proxmox VE API.
type APIError struct {
	Method     string
	Path       string
	StatusCode int
	// Message is Proxmox's error message, taken from the response body or the HTTP status line.
	Message string
	// ParamErrors maps parameter names to validation messages, for 400 responses.
	ParamErrors map[string]string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s: %d %s", e.Method, e.Path, e.StatusCode, e.Message)
	if len(e.ParamErrors) > 0 {
		names := make([]string, 0, len(e.ParamErrors))
		for name := range e.ParamErrors {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			fmt.Fprintf(&b, "; %s: %s", name, strings.TrimSpace(e.ParamErrors[name]))
		}
	}
	return b.String()
}

// TaskError is a Proxmox task that finished with an exit status other than OK.
type TaskError struct {
	UPID       string
	Type       string
	ExitStatus string
}

func (e *TaskError) Error() string {
	return fmt.Sprintf("proxmox task %s (%s) failed: %s", e.Type, e.UPID, e.ExitStatus)
}

// messageContains reports whether err is an APIError or TaskError whose message contains substr.
func messageContains(err error, substr string) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) && strings.Contains(apiErr.Message, substr) {
		return true
	}
	var taskErr *TaskError
	return errors.As(err, &taskErr) && strings.Contains(taskErr.ExitStatus, substr)
}

// IsVMIDInUse reports whether a clone failed because its new VMID is already taken, which happens when two clones
// race for the same ID. The caller should retry with another ID.
//
// Proxmox reports this as "VM <id> already exists on node '<node>'" or "unable to create VM <id>: config file
// already exists", depending on when it notices.
func IsVMIDInUse(err error) bool {
	return messageContains(err, "already exists on node") || messageContains(err, "config file already exists")
}

// IsForbidden reports whether the API refused a request in its permission check (403).
//
// For a token whose rights come from a pool's ACL, as the controller's do, Proxmox also answers a request for a VM
// that no longer exists this way, rather than with IsNotFound: the missing VM isn't in the pool, so the permission
// check fails first. Only the VM list tells the two apart.
func IsForbidden(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusForbidden
}

// IsNotFound reports whether the API said a VM or other object doesn't exist. See IsForbidden for why a pool-scoped
// token never sees this for a VM that is gone.
func IsNotFound(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode == http.StatusNotFound ||
		(apiErr.StatusCode == http.StatusInternalServerError && strings.Contains(apiErr.Message, "does not exist"))
}

// IsAgentNotReady reports whether a guest-agent call failed because the VM or its agent isn't running yet, which
// Proxmox reports as "VM <id> is not running" or "QEMU guest agent is not running". The caller should retry after a
// while.
func IsAgentNotReady(err error) bool {
	return messageContains(err, "is not running")
}
