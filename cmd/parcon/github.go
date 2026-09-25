package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// defaultKeyFile is where the installer keeps the GitHub App's private key in the controller VM.
const defaultKeyFile = "/etc/proxmox-actions-runners/github-app.pem"

// maxStdin bounds what the commands read from stdin: a manifest code or a PEM key.
const maxStdin = 64 << 10

// stdin supplies the manifest code and private keys, which must never appear on a command line. Tests replace it.
var stdin io.Reader = os.Stdin

// These talk to GitHub. Tests replace them.
var (
	convertManifest  = github.ConvertManifest
	findInstallation = github.FindInstallation
	// installationPollInterval is how often wait-installation asks GitHub.
	installationPollInterval = 5 * time.Second
)

var appCommands = map[string]func(args []string, stdout, stderr io.Writer) error{
	"create":            appCreate,
	"import":            appImport,
	"wait-installation": appWaitInstallation,
}

// runGitHub handles "parcon github app <command>" and "parcon github scaleset delete". The installer runs the app
// commands to set up the GitHub App before the config names it, so they take their settings from flags and stdin,
// not from the config file.
func runGitHub(args []string, stdout, stderr io.Writer) error {
	var cmd func(args []string, stdout, stderr io.Writer) error
	switch {
	case len(args) >= 2 && args[0] == "app":
		cmd = appCommands[args[1]]
	case len(args) >= 2 && args[0] == "scaleset" && args[1] == "delete":
		cmd = scaleSetDelete
	}
	if cmd == nil {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	err := cmd(args[2:], stdout, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return nil
	}
	return err
}

// parseFlags parses a command's flags and rejects positional arguments. It returns flag.ErrHelp for -h.
func parseFlags(fs *flag.FlagSet, args []string, stderr io.Writer) error {
	fs.SetOutput(stderr)
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return errUsage
	}
	return nil
}

// appCreate exchanges the manifest code read from stdin for the new App, writes its private key, and prints the
// App's client ID, App ID, and slug as JSON for the installer.
func appCreate(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app create", flag.ContinueOnError)
	keyFile := fs.String("key-file", defaultKeyFile, "where to write the App's private key")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	code, err := readStdin("manifest code")
	if err != nil {
		return err
	}
	var app github.App
	err = writeSecretFile(*keyFile, func() (string, error) {
		var key string
		var err error
		app, key, err = convertManifest(context.Background(), code)
		return key, err
	})
	if err != nil {
		return err
	}
	return json.NewEncoder(stdout).Encode(struct {
		ClientID string `json:"clientId"`
		AppID    int64  `json:"appId"`
		Slug     string `json:"slug"`
	}{app.ClientID, app.ID, app.Slug})
}

// appImport writes an existing App's private key, read from stdin, for installs that don't use the manifest flow.
func appImport(args []string, _, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app import", flag.ContinueOnError)
	keyFile := fs.String("key-file", defaultKeyFile, "where to write the App's private key")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	key, err := readStdin("private key")
	if err != nil {
		return err
	}
	if err := github.CheckPrivateKey(key); err != nil {
		return err
	}
	return writeSecretFile(*keyFile, func() (string, error) { return key, nil })
}

// appWaitInstallation waits until the App is installed on the target organization or repository and prints the
// installation's ID.
func appWaitInstallation(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github app wait-installation", flag.ContinueOnError)
	clientID := fs.String("client-id", "", "the App's client ID (required)")
	keyFile := fs.String("key-file", defaultKeyFile, "the App's private key")
	target := fs.String("target", "", "the organization or repository URL, such as https://github.com/my-org "+
		"(required)")
	timeout := fs.Duration("timeout", 15*time.Minute, "how long to wait for the installation")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	if *clientID == "" || *target == "" {
		fmt.Fprintln(stderr, "-client-id and -target are required")
		return errUsage
	}
	owner, repo, err := config.ParseGitHubURL(*target)
	if err != nil {
		fmt.Fprintf(stderr, "-target: %v\n", err)
		return errUsage
	}
	key, err := config.ReadSecretFile(*keyFile)
	if err != nil {
		return fmt.Errorf("GitHub App key: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	// Errors don't end the wait: a network blip, a GitHub 5xx, or a new App GitHub doesn't know yet all pass. Each
	// new error is shown right away, and the last one is repeated if the wait times out.
	var lastErr error
	for {
		id, err := findInstallation(ctx, *clientID, key, owner, repo)
		if id != 0 {
			fmt.Fprintln(stdout, id)
			return nil
		}
		// An error after the deadline is just the deadline.
		if err != nil && ctx.Err() == nil {
			if lastErr == nil || err.Error() != lastErr.Error() {
				fmt.Fprintf(stderr, "%v; still waiting\n", err)
			}
			lastErr = err
		}
		select {
		case <-ctx.Done():
			if lastErr != nil {
				return fmt.Errorf("the GitHub App isn't installed on %s after %s; last error: %w", *target, *timeout,
					lastErr)
			}
			return fmt.Errorf("the GitHub App isn't installed on %s after %s", *target, *timeout)
		case <-time.After(installationPollInterval):
		}
	}
}

// readStdin reads a secret from stdin and trims surrounding whitespace. Errors never include it.
func readStdin(what string) (string, error) {
	data, err := io.ReadAll(io.LimitReader(stdin, maxStdin))
	if err != nil {
		return "", fmt.Errorf("read the %s from stdin: %w", what, err)
	}
	s := strings.TrimSpace(string(data))
	if s == "" {
		return "", fmt.Errorf("no %s on stdin", what)
	}
	return s, nil
}

// writeSecretFile writes the secret that produce returns to path with mode 0600. It writes a temporary file in the
// same directory and renames it, so a reader never sees half a key. The temporary file is created first, so a path
// that can't be written fails before produce spends a single-use manifest code.
func writeSecretFile(path string, produce func() (string, error)) (err error) {
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
	secret, err := produce()
	if err != nil {
		return err
	}
	// os.CreateTemp creates the file with mode 0600.
	_, err = f.WriteString(strings.TrimSpace(secret) + "\n")
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = os.Rename(f.Name(), path)
	}
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}
