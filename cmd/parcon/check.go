package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/proxmox"
	"github.com/klponce/proxmox-actions-runners/internal/vmtags"
)

// checkTimeout bounds each check command.
const checkTimeout = 30 * time.Second

// supportedPVEMajor is the only Proxmox VE major version the project supports (README, "Limitations").
const supportedPVEMajor = 9

var checks = map[string]func(ctx context.Context, cfg *config.Config, path string, out io.Writer) error{
	"config":   checkConfig,
	"proxmox":  checkProxmox,
	"github":   checkGitHub,
	"template": checkTemplate,
}

// runCheck handles "parcon check <target> [-config path]".
func runCheck(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 || checks[args[0]] == nil {
		fmt.Fprint(stderr, vmUsage)
		return errUsage
	}
	target := args[0]
	fs := flag.NewFlagSet("check "+target, flag.ContinueOnError)
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

	cfg, err := config.Load(*path)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), checkTimeout)
	defer cancel()
	return checks[target](ctx, cfg, *path, stdout)
}

func checkConfig(_ context.Context, cfg *config.Config, path string, out io.Writer) error {
	fmt.Fprintf(out, "%s: OK, %d scale set(s)\n", path, len(cfg.ScaleSets))
	return nil
}

// checker prints one line per check and counts failures.
type checker struct {
	out    io.Writer
	failed int
}

func (c *checker) ok(format string, args ...any) {
	fmt.Fprintf(c.out, "ok    %s\n", fmt.Sprintf(format, args...))
}

func (c *checker) fail(format string, args ...any) {
	c.failed++
	fmt.Fprintf(c.out, "FAIL  %s\n", fmt.Sprintf(format, args...))
}

func (c *checker) err() error {
	if c.failed > 0 {
		return fmt.Errorf("%d check(s) failed", c.failed)
	}
	return nil
}

// checkProxmox confirms that the controller can use the Proxmox VE API as configured: the token works, the
// version is supported, the token has exactly the privileges it needs, and the storage can hold VM disks.
func checkProxmox(ctx context.Context, cfg *config.Config, _ string, out io.Writer) error {
	p := cfg.Proxmox
	c := &checker{out: out}

	client, err := newProxmoxClient(cfg)
	if err != nil {
		c.fail("%v", err)
		return c.err()
	}

	// Nothing else can work if the API can't be reached with this token.
	v, err := client.Version(ctx)
	if err != nil {
		c.fail("reach %s as %s: %v", p.URL, p.TokenID, err)
		return c.err()
	}
	if v.Major() != supportedPVEMajor {
		c.fail("Proxmox VE %s: only %d.x is supported", v.Version, supportedPVEMajor)
	} else {
		c.ok("Proxmox VE %s at %s, token %s", v.Version, p.URL, p.TokenID)
	}

	perms, err := client.Permissions(ctx)
	if err != nil {
		c.fail("read the token's permissions: %v", err)
	} else {
		for _, req := range proxmox.RequiredPrivileges(p.Pool, p.Storage, p.Zone, p.VNet) {
			if missing := perms.Missing(req); len(missing) > 0 {
				// A privilege-separated token only gets what both it and its user are granted, so an ACL for the
				// token alone doesn't show up here.
				c.fail("privileges on %s: missing %s (grant it to both the token and its user)", req.Path,
					strings.Join(missing, ", "))
			} else {
				c.ok("privileges on %s", req.Path)
			}
		}
	}

	st, err := client.StorageStatus(ctx, p.Storage)
	switch {
	case err != nil:
		c.fail("storage %s on node %s: %v", p.Storage, p.Node, err)
	case !st.Enabled || !st.Active:
		c.fail("storage %s on node %s is not enabled and active", p.Storage, p.Node)
	case !st.Accepts("images"):
		c.fail("storage %s doesn't accept VM disks (content type \"images\")", p.Storage)
	default:
		c.ok("storage %s (%s) on node %s is active and accepts VM disks, %s free", p.Storage, st.Type, p.Node,
			formatGiB(st.AvailBytes))
	}

	vms, err := client.ListVMs(ctx)
	if err != nil {
		c.fail("list VMs: %v", err)
	} else {
		inPool := 0
		for _, vm := range vms {
			if vm.Pool == p.Pool {
				inPool++
			}
		}
		c.ok("list VMs: %d in pool %s on node %s", inPool, p.Pool, p.Node)
	}
	return c.err()
}

// newProxmoxClient builds the Proxmox VE client from the config.
func newProxmoxClient(cfg *config.Config) (*proxmox.Client, error) {
	p := cfg.Proxmox
	secret, err := config.ReadSecretFile(p.TokenSecretFile)
	if err != nil {
		return nil, fmt.Errorf("proxmox token: %w", err)
	}
	var ca []byte
	if p.CACertFile != "" {
		// A CA certificate isn't a secret, so it isn't held to ReadSecretFile's permission check.
		if ca, err = os.ReadFile(p.CACertFile); err != nil {
			return nil, fmt.Errorf("proxmox CA certificate: %w", err)
		}
	}
	return proxmox.New(proxmox.Options{
		URL:            p.URL,
		TokenID:        p.TokenID,
		TokenSecret:    secret,
		TLSFingerprint: p.TLSFingerprint,
		CACertPEM:      ca,
		ServerName:     p.TLSServerName,
		Node:           p.Node,
		UserAgent:      "parcon/" + buildVersion(),
	})
}

// newGitHubClient builds the client that talks to GitHub as the App. It fails if the config doesn't name the App
// yet: only run and check github need it, and the installer runs the other checks before it creates the App.
// logger may be nil.
func newGitHubClient(cfg *config.Config, logger *slog.Logger) (*github.Client, error) {
	if err := cfg.RequireGitHubApp(); err != nil {
		return nil, err
	}
	key, err := config.ReadSecretFile(cfg.GitHub.App.PrivateKeyFile)
	if err != nil {
		return nil, fmt.Errorf("GitHub App key: %w", err)
	}
	return github.New(github.Options{
		ConfigURL:      cfg.GitHub.ConfigURL,
		ClientID:       cfg.GitHub.App.ClientID,
		InstallationID: cfg.GitHub.App.InstallationID,
		PrivateKeyPEM:  key,
		Version:        buildVersion(),
		Logger:         logger,
	})
}

// checkGitHub confirms that the GitHub App credentials work and shows which scale sets are already registered.
// It only reads: the scale sets are created when the controller starts.
func checkGitHub(ctx context.Context, cfg *config.Config, _ string, out io.Writer) error {
	c := &checker{out: out}
	client, err := newGitHubClient(cfg, nil)
	if err != nil {
		c.fail("GitHub App: %v", err)
		return c.err()
	}

	// Each lookup authenticates, so bad App credentials fail every line.
	for _, s := range cfg.ScaleSets {
		found, err := client.FindScaleSet(ctx, github.ScaleSetSpec{Name: s.Name, Labels: s.Labels,
			RunnerGroup: s.RunnerGroup})
		switch {
		case err != nil:
			c.fail("scale set %s in runner group %s: %v", s.Name, s.RunnerGroup, err)
		case found == nil:
			c.ok("scale set %s in runner group %s: not registered yet; the controller creates it on start", s.Name,
				s.RunnerGroup)
		default:
			c.ok("scale set %s in runner group %s: registered with ID %d, labels %s", s.Name, s.RunnerGroup, found.ID,
				strings.Join(found.Labels, ", "))
		}
	}
	if c.failed == 0 {
		c.ok("GitHub App %s (installation %d) can manage runners on %s", cfg.GitHub.App.ClientID,
			cfg.GitHub.App.InstallationID, cfg.GitHub.ConfigURL)
	}
	return c.err()
}

// latestRunnerRelease looks up the latest actions/runner release. It needs no GitHub App credentials, so the check
// works before the App is set up. Tests replace it.
var latestRunnerRelease = github.LatestRunnerRelease

// checkTemplate shows the runner template workers are cloned from and whether its actions/runner is recent enough:
// GitHub stops accepting a runner that doesn't update itself 30 days after a newer release. It fails if there is no
// template, or if the template is github.RunnerUpdateErrorAfter behind.
func checkTemplate(ctx context.Context, cfg *config.Config, _ string, out io.Writer) error {
	c := &checker{out: out}
	client, err := newProxmoxClient(cfg)
	if err != nil {
		c.fail("%v", err)
		return c.err()
	}
	vms, err := client.ListVMs(ctx)
	if err != nil {
		c.fail("list VMs: %v", err)
		return c.err()
	}
	tpl, ok := vmtags.NewestTemplate(vms, cfg.Proxmox.Pool)
	if !ok {
		c.fail("no runner template in pool %s (a template tagged %s, %s and %s<unix time>)", cfg.Proxmox.Pool,
			vmtags.Managed, vmtags.Template, vmtags.TemplateVersionPrefix)
		return c.err()
	}
	now := time.Now()
	created, _ := vmtags.Int(tpl, vmtags.TemplateVersionPrefix)
	have, _ := vmtags.String(tpl, vmtags.RunnerVersionPrefix)
	if have == "" {
		have = "unknown"
	}
	c.ok("runner template %d, created %s ago, actions/runner %s", tpl.VMID, formatDays(now.Sub(time.Unix(created, 0))),
		have)

	latest, err := latestRunnerRelease(ctx)
	if err != nil {
		c.fail("latest actions/runner release: %v", err)
		return c.err()
	}
	age := now.Sub(latest.PublishedAt)
	deadline := latest.PublishedAt.Add(github.RunnerUpdateDeadline).UTC().Format(time.DateOnly)
	switch latest.Staleness(have, now) {
	case github.Current:
		c.ok("actions/runner %s is the latest release", latest.Version)
	case github.BehindError:
		c.fail("actions/runner %s was released %s ago; GitHub stops accepting older runners after %s: install a "+
			"newer runner image", latest.Version, formatDays(age), deadline)
	default:
		c.ok("actions/runner %s was released %s ago; install a newer runner image before %s", latest.Version,
			formatDays(age), deadline)
	}
	return c.err()
}

func formatDays(d time.Duration) string {
	return fmt.Sprintf("%d days", int(d.Hours()/24))
}

func formatGiB(bytes int64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/(1<<30))
}
