// Command parcon installs, runs, and manages proxmox-actions-runners.
//
// On the Proxmox host, run as root, it installs the runners, reports their state, changes their settings, updates
// them, and uninstalls them. In the controller VM, it runs the controller as a service, and does the steps that need
// the controller's secrets, which never leave that VM. The host reaches those through the guest agent.
//
// Usage on the Proxmox host:
//
//	parcon install [flags]
//	parcon status [-json]
//	parcon config get <key> | get --all | set <key> <value> | describe [<key>] | apply
//	parcon update [-pre] [-yes] [-dry-run]
//	parcon check [network|config|proxmox|github|template]
//	parcon uninstall [-yes] [-dry-run]
//	parcon version
//
// Usage in the controller VM:
//
//	parcon run [-config path] [-log-level level]
//	parcon check config|proxmox|github|template [-config path]
//	parcon github app create [-key-file path] < code
//	parcon github app import [-key-file path] < key.pem
//	parcon github app wait-installation -client-id id [-key-file path] [-timeout duration]
//	parcon github scaleset delete [-config path]
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/klponce/proxmox-actions-runners/internal/installer"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

const hostUsage = `usage on the Proxmox host, as root:
  parcon install [flags]                install the runners on this node, or continue an install that stopped;
                                        parcon install -h lists the flags
  parcon status [-json]                 the state of the whole install
  parcon config get <key>               print a setting
  parcon config get --all               print every setting, with its default and what it is
  parcon config set <key> <value>       change a setting; the controller restarts with it
  parcon config describe [<key>]        which values a setting takes, and when a change applies
  parcon config apply                   push the settings to the controller again
  parcon update [-pre] [-yes] [-dry-run]
                                        update to the newest release (-pre: or pre-release), once its signature
                                        checks out; running it again continues an update that stopped
  parcon uninstall [-yes] [-dry-run]    remove everything parcon created, and parcon itself
  parcon check                          check this node, and after an install, the controller too
  parcon check network                  check the worker network from a throwaway worker
  parcon check config|proxmox|github|template
                                        run one of the controller's checks in the controller VM
  parcon version
`

const vmUsage = `usage in the controller VM:
  parcon version
  parcon run [-config path] [-log-level level]
                                        run the controller until SIGINT or SIGTERM
  parcon check config [-config path]    validate the config file
  parcon check proxmox [-config path]   check the Proxmox VE API, token privileges, and storage
  parcon check github [-config path]    check the GitHub App credentials and scale set registration
  parcon check template [-config path]  check the runner template and its actions/runner version
  parcon github app create [-key-file path] < code
                                        create the GitHub App from the manifest code on stdin, write its key,
                                        and print its clientId, appId, slug, owner, and ownerType as JSON
  parcon github app import [-key-file path] < key.pem
                                        write an existing GitHub App's private key from stdin
  parcon github app wait-installation -client-id id [-key-file path] [-timeout duration]
                                        wait until the App is installed and print its installationId,
                                        account, accountType, and (for a personal account) repositories
                                        as JSON
  parcon github scaleset delete [-config path]
                                        remove the configured scale sets and their runners from GitHub
`

// usage is the usage text for where parcon runs.
func usageText() string {
	if hostMode() {
		return hostUsage
	}
	return vmUsage
}

// errUsage marks a command-line mistake, which exits with status 2 like the flag package does.
var errUsage = errors.New("usage error")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		var quiet errQuiet
		switch {
		case errors.Is(err, errUsage):
			os.Exit(2)
		case errors.As(err, &quiet):
			if errors.Is(err, installer.ErrCanceled) {
				fmt.Fprintln(os.Stderr, "parcon: canceled")
			}
		default:
			fmt.Fprintln(os.Stderr, "parcon:", err)
		}
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText())
		return errUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return nil
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usageText())
		return nil
	}
	if hostMode() {
		return runHost(args, stdout, stderr)
	}
	switch args[0] {
	case "run":
		return runController(args[1:], stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "github":
		return runGitHub(args[1:], stdout, stderr)
	case "install", "uninstall", "status", "update", "config":
		return fmt.Errorf("parcon %s runs on the Proxmox host, not in the controller VM", args[0])
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], vmUsage)
		return errUsage
	}
}

// runHost runs a command on the Proxmox host.
func runHost(args []string, stdout, stderr io.Writer) error {
	var err error
	switch args[0] {
	case "install":
		err = runInstall(args[1:], stdout, stderr)
	case "uninstall":
		err = runUninstall(args[1:], stdout, stderr)
	case "check":
		err = runHostCheck(args[1:], stdout, stderr)
	case "status":
		err = runStatus(args[1:], stdout, stderr)
	case "config":
		err = runConfig(args[1:], stdout, stderr)
	case "update":
		err = runUpdate(args[1:], stdout, stderr)
	case "run", "github":
		return fmt.Errorf("parcon %s runs in the controller VM; parcon install sets it up", args[0])
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], hostUsage)
		return errUsage
	}
	// A declined plan or a failed check has already said why.
	if errors.Is(err, installer.ErrCanceled) || errors.Is(err, installer.ErrChecksFailed) {
		return errQuiet{err}
	}
	return err
}

// errQuiet is a failure whose reason is already printed: parcon exits with status 1 without repeating it.
type errQuiet struct{ error }

// buildVersion returns the version set at build time, or the module version and VCS revision Go recorded.
func buildVersion() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	v := info.Main.Version
	for _, s := range info.Settings {
		if s.Key == "vcs.revision" && len(s.Value) >= 12 {
			v += " (" + s.Value[:12] + ")"
		}
	}
	return v
}
