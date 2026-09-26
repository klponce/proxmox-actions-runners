// Package pvecli is how parcon on the Proxmox host reads and changes Proxmox: through the node's own command-line
// tools (pvesh, pveum, qm), as root. Importing a disk from a local file needs root@pam, which has no API token, and
// the tools need no credentials at all.
//
// Every change goes through a Changer, which logs the command before it runs it and only logs it in a dry run, so the
// user sees each change exactly as it is made. Secrets travel only on a command's stdin, which is never logged and
// never part of an error.
package pvecli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Cmd is one command to run.
type Cmd struct {
	Name string
	Args []string
	// Stdin is the command's standard input. It is how secrets reach a command, and it is never logged.
	Stdin []byte
}

// C builds a Cmd.
func C(name string, args ...string) Cmd { return Cmd{Name: name, Args: args} }

// String is the command line as it is logged: the name and arguments, never Stdin.
func (c Cmd) String() string {
	return strings.Join(append([]string{c.Name}, c.Args...), " ")
}

// Exec runs commands.
type Exec interface {
	// Run runs c and returns its standard output. A command that fails returns an *ExitError.
	Run(ctx context.Context, c Cmd) ([]byte, error)
}

// ExitError is a command that failed. Its message has the command line and what the command wrote to stderr.
type ExitError struct {
	Cmd    string
	Stderr string
	Err    error
}

func (e *ExitError) Error() string {
	if e.Stderr == "" {
		return fmt.Sprintf("%s: %v", e.Cmd, e.Err)
	}
	return fmt.Sprintf("%s: %v: %s", e.Cmd, e.Err, e.Stderr)
}

func (e *ExitError) Unwrap() error { return e.Err }

// OSExec runs commands on this host.
type OSExec struct{}

// Run implements Exec.
func (OSExec) Run(ctx context.Context, c Cmd) ([]byte, error) {
	cmd := exec.CommandContext(ctx, c.Name, c.Args...) //nolint:gosec // G204: parcon runs the Proxmox tools it names.
	if c.Stdin != nil {
		cmd.Stdin = bytes.NewReader(c.Stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), &ExitError{Cmd: c.String(), Stderr: strings.TrimSpace(stderr.String()), Err: err}
	}
	return stdout.Bytes(), nil
}

// Changer makes changes to the host: it logs each one as "    $ command" and runs it, or with DryRun only logs it.
type Changer struct {
	Exec   Exec
	Out    io.Writer
	DryRun bool
}

// Run logs c and runs it.
func (ch *Changer) Run(ctx context.Context, c Cmd) ([]byte, error) {
	ch.Log(c.String())
	if ch.DryRun {
		return nil, nil
	}
	return ch.Exec.Run(ctx, c)
}

// Log logs a change that isn't a host command, such as a file written in a VM. The caller makes the change, and
// skips it in a dry run.
func (ch *Changer) Log(line string) {
	if ch.Out != nil {
		_, _ = fmt.Fprintf(ch.Out, "    $ %s\n", line)
	}
}

// WriteFile logs and writes a file on the host atomically: a temporary file in the same directory, synced, then
// renamed over path.
func (ch *Changer) WriteFile(path string, data []byte, mode os.FileMode) error {
	ch.Log("write " + path)
	if ch.DryRun {
		return nil
	}
	return WriteFileAtomic(path, data, mode)
}

// Remove logs and removes a file. One that is already gone counts as removed.
func (ch *Changer) Remove(path string) error {
	ch.Log("rm " + path)
	if ch.DryRun {
		return nil
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", path, err)
	}
	return nil
}

// WriteFileAtomic writes data to path through a temporary file in the same directory, so a reader sees the old
// file or the new one, never a partial one.
func WriteFileAtomic(path string, data []byte, mode os.FileMode) (err error) {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	defer func() {
		if err != nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
		}
	}()
	if err := f.Chmod(mode); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if _, err := f.Write(data); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Sync(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(f.Name(), path); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
