// Command parcon is the proxmox-actions-runners controller.
//
// Usage:
//
//	parcon version
//	parcon check config [-config path]
package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"

	"github.com/klponce/proxmox-actions-runners/internal/config"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = ""

const usage = `usage:
  parcon version
  parcon check config [-config path]
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

func runCheck(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || args[0] != "config" {
		fmt.Fprint(stderr, usage)
		return errUsage
	}
	fs := flag.NewFlagSet("check config", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "path of the config file")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return errUsage
	}

	c, err := config.Load(*path)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s: OK, %d scale set(s)\n", *path, len(c.ScaleSets))
	return nil
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
