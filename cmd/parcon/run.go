package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/controller"
)

// runController handles "parcon run": it runs the controller until SIGINT or SIGTERM.
func runController(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(stderr)
	path := fs.String("config", config.DefaultPath, "path of the config file")
	level := fs.String("log-level", "info", "log level: debug, info, warn, or error")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return errUsage
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(stderr, "unexpected arguments: %v\n", fs.Args())
		return errUsage
	}
	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*level)); err != nil {
		fmt.Fprintf(stderr, "invalid -log-level %q\n", *level)
		return errUsage
	}
	// JSON on stderr, which systemd sends to the journal.
	logger := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: lvl}))

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	c, err := newController(cfg, logger)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.InfoContext(ctx, "starting", slog.String("version", buildVersion()), slog.String("config", *path),
		slog.Int("scaleSets", len(cfg.ScaleSets)))
	if err := c.Run(ctx); err != nil {
		return err
	}
	logger.InfoContext(ctx, "stopped")
	return nil
}

// statusPath is where parcon run writes its status: in the runtime directory systemd makes for parcon.service. Tests
// replace it.
var statusPath = controller.StatusPath

// newController builds the controller and its clients from the config and the secret files it names.
func newController(cfg *config.Config, logger *slog.Logger) (*controller.Controller, error) {
	gh, err := newGitHubClient(cfg, logger.With(slog.String("component", "github")))
	if err != nil {
		return nil, err
	}
	pve, err := newProxmoxClient(cfg)
	if err != nil {
		return nil, err
	}

	owner, err := os.Hostname()
	if err != nil {
		owner = "parcon"
	}
	return controller.New(controller.Options{
		Config:     cfg,
		Proxmox:    pve,
		GitHub:     gh,
		Owner:      owner,
		Logger:     logger.With(slog.String("component", "controller")),
		StatusPath: statusPath,
		Version:    buildVersion(),
	})
}
