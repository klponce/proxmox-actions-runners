package main

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"github.com/klponce/proxmox-actions-runners/internal/installer"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
)

const configUsage = `usage:
  parcon config get <key>            print a key's value
  parcon config get --all            print every key with its value, default, and what it is
  parcon config set <key> <value>    change a key and apply it: the controller restarts with it
  parcon config describe [<key>]     say what a key is, which values it takes, and when a change applies
  parcon config apply                push the settings to the controller again and restart it
`

// runConfig handles "parcon config" on the host.
func runConfig(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, configUsage)
		return errUsage
	}
	in, err := newInstaller(stdout, stderr, false)
	if err != nil {
		return err
	}
	switch verb, rest := args[0], args[1:]; {
	case verb == "get" && len(rest) == 1 && isAll(rest[0]):
		return configGetAll(in, stdout)
	case verb == "get" && len(rest) == 1:
		return configGet(in, rest[0], stdout, stderr)
	case verb == "set" && len(rest) == 2:
		ctx, cancel := hostContext()
		defer cancel()
		err := in.SetConfig(ctx, rest[0], rest[1])
		return configError(in, err, rest[0], stderr)
	case verb == "describe" && len(rest) <= 1:
		return configDescribe(in, rest, stdout, stderr)
	case verb == "apply" && len(rest) == 0:
		ctx, cancel := hostContext()
		defer cancel()
		return in.ApplyConfig(ctx)
	default:
		fmt.Fprint(stderr, configUsage)
		return errUsage
	}
}

func isAll(arg string) bool { return arg == "--all" || arg == "-all" }

// configError turns what SetConfig returned into parcon's exit: an unchanged value is fine, and a refused value or
// an unknown key is a usage error with the key's guidance.
func configError(in *installer.Installer, err error, key string, stderr io.Writer) error {
	var verr *settings.ValueError
	switch {
	case err == nil, errors.Is(err, installer.ErrUnchanged):
		return nil
	case errors.As(err, &verr):
		fmt.Fprintf(stderr, "parcon: %s\n\n", err)
		if s, serr := in.Settings(); serr == nil {
			k, _ := settings.Lookup(key)
			fmt.Fprint(stderr, settings.Describe(k, s, in.Limits()))
		}
		return errUsage
	case errors.Is(err, settings.ErrUnknownKey):
		fmt.Fprintf(stderr, "parcon: %s\n", err)
		return errUsage
	}
	return err
}

func configGet(in *installer.Installer, key string, stdout, stderr io.Writer) error {
	s, err := in.Settings()
	if err != nil {
		return err
	}
	k, err := settings.Lookup(key)
	if err != nil {
		fmt.Fprintf(stderr, "parcon: %s\n", err)
		return errUsage
	}
	v, _ := k.Get(s)
	fmt.Fprintln(stdout, v)
	return nil
}

func configGetAll(in *installer.Installer, stdout io.Writer) error {
	s, err := in.Settings()
	if err != nil {
		return err
	}
	w := tabwriter.NewWriter(stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "KEY\tVALUE\tDEFAULT\tDESCRIPTION")
	for _, k := range settings.Keys {
		v, _ := k.Get(s)
		def, _, _ := strings.Cut(k.Default, ",")
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\n", k.Name, v, def, k.Summary)
	}
	if err := w.Flush(); err != nil {
		return err
	}
	fmt.Fprintln(stdout, "\nparcon config describe <key> says which values a key takes.")
	return nil
}

func configDescribe(in *installer.Installer, keys []string, stdout, stderr io.Writer) error {
	s, err := in.Settings()
	if err != nil {
		return err
	}
	all := settings.Keys
	if len(keys) == 1 {
		k, err := settings.Lookup(keys[0])
		if err != nil {
			fmt.Fprintf(stderr, "parcon: %s\n", err)
			return errUsage
		}
		all = []settings.Key{k}
	}
	for i, k := range all {
		if i > 0 {
			fmt.Fprintln(stdout)
		}
		fmt.Fprint(stdout, settings.Describe(k, s, in.Limits()))
	}
	return nil
}
