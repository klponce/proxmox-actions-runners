// Command parcon is the proxmox-actions-runners controller.
//
// Usage:
//
//	parcon version
//	parcon run [-config path] [-log-level level]
//	parcon check config [-config path]
//	parcon check proxmox [-config path]
//	parcon check github [-config path]
//	parcon check template [-config path]
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"runtime/debug"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

const usage = `usage:
  parcon version
  parcon run [-config path] [-log-level level]
                                        run the controller until SIGINT or SIGTERM
  parcon check config [-config path]    validate the config file
  parcon check proxmox [-config path]   check the Proxmox VE API, token privileges, and storage
  parcon check github [-config path]    check the GitHub App credentials and scale set registration
  parcon check template [-config path]  check the runner template and its actions/runner version
`

// errUsage marks a command-line mistake, which exits with status 2 like the flag package does.
var errUsage = errors.New("usage error")

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if !errors.Is(err, errUsage) {
			fmt.Fprintln(os.Stderr, "parcon:", err)
			os.Exit(1)
		}
		os.Exit(2)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	switch args[0] {
	case "version":
		fmt.Fprintln(stdout, buildVersion())
		return nil
	case "run":
		return runController(args[1:], stderr)
	case "check":
		return runCheck(args[1:], stdout, stderr)
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprintf(stderr, "unknown command %q\n%s", args[0], usage)
		return errUsage
	}
}

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
