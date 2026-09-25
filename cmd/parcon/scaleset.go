package main

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
)

// scaleSetClient is the part of the GitHub client that scaleset delete uses.
type scaleSetClient interface {
	FindScaleSet(ctx context.Context, spec github.ScaleSetSpec) (*github.ScaleSet, error)
	DeleteScaleSet(ctx context.Context, id int) error
}

// newScaleSetClient builds the GitHub client for scaleset delete. Tests replace it.
var newScaleSetClient = func(cfg *config.Config) (scaleSetClient, error) { return newGitHubClient(cfg, nil) }

// scaleSetDelete removes the configured scale sets from GitHub, which unregisters their runners. The installer runs
// it on uninstall, after it stops the controller, which would otherwise register them again.
func scaleSetDelete(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("github scaleset delete", flag.ContinueOnError)
	path := fs.String("config", config.DefaultPath, "path of the config file")
	if err := parseFlags(fs, args, stderr); err != nil {
		return err
	}
	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	client, err := newScaleSetClient(cfg)
	if err != nil {
		return fmt.Errorf("GitHub App: %w", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	for _, s := range cfg.ScaleSets {
		found, err := client.FindScaleSet(ctx, github.ScaleSetSpec{Name: s.Name, Labels: s.Labels,
			RunnerGroup: s.RunnerGroup})
		if err != nil {
			return fmt.Errorf("scale set %s: %w", s.Name, err)
		}
		if found == nil {
			fmt.Fprintf(stdout, "scale set %s: not registered\n", s.Name)
			continue
		}
		if err := client.DeleteScaleSet(ctx, found.ID); err != nil {
			return fmt.Errorf("scale set %s: %w", s.Name, err)
		}
		fmt.Fprintf(stdout, "scale set %s: deleted (ID %d)\n", s.Name, found.ID)
	}
	return nil
}
