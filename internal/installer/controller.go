package installer

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/config"
	"github.com/klponce/proxmox-actions-runners/internal/hostsys"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
	"github.com/klponce/proxmox-actions-runners/internal/settings"
	"github.com/klponce/proxmox-actions-runners/internal/term"
)

// env reads what the controller's config needs from the node.
func (in *Installer) env(ctx context.Context, s *settings.Settings) (settings.Env, error) {
	node, err := in.nodeName(ctx)
	if err != nil {
		return settings.Env{}, err
	}
	addr, err := in.pveAddress(ctx, s)
	if err != nil {
		return settings.Env{}, err
	}
	tls, err := in.Sys.TLS(in.Roots)
	if err != nil {
		return settings.Env{}, err
	}
	linked, _ := in.linkedClones(ctx, s)
	return settings.Env{Node: node, PVEAddress: addr, TLS: tls, LinkedClone: linked}, nil
}

// RenderConfig renders the controller's config.yaml from the settings and the node.
func (in *Installer) RenderConfig(ctx context.Context, s *settings.Settings) ([]byte, settings.Env, error) {
	env, err := in.env(ctx, s)
	if err != nil {
		return nil, settings.Env{}, err
	}
	_, data, err := settings.ControllerConfig(s, env)
	return data, env, err
}

// pushConfig writes the controller's config, and the node's CA when the controller verifies the API against it. The
// new config goes next to the old one and replaces it only once the controller's own parcon accepts it, so a config
// its version can't read never takes its place.
func (in *Installer) pushConfig(ctx context.Context, vmid int, s *settings.Settings) error {
	data, env, err := in.RenderConfig(ctx, s)
	if err != nil {
		return err
	}
	if env.TLS.Mode == hostsys.TLSNodeCA {
		if err := in.writeControllerFile(ctx, vmid, config.CACertFile, env.TLS.CA); err != nil {
			return err
		}
	}
	next := config.DefaultPath + ".new"
	if err := in.writeControllerFile(ctx, vmid, next, data); err != nil {
		return err
	}
	in.Change.Log(fmt.Sprintf("controller VM %d: parcon check config, then mv %s %s", vmid, next, config.DefaultPath))
	if in.Change.DryRun {
		return nil
	}
	if out, err := in.asParcon(ctx, vmid, time.Minute, nil, "parcon", "check", "config", "-config", next); err != nil {
		_, _ = in.asParcon(ctx, vmid, 30*time.Second, nil, "rm", "-f", next)
		return fmt.Errorf("the controller's parcon refuses the new config: %w%s", err, indent(out))
	}
	if _, err := in.asParcon(ctx, vmid, 30*time.Second, nil, "mv", next, config.DefaultPath); err != nil {
		return fmt.Errorf("replace the controller's config: %w", err)
	}
	return nil
}

func indent(out []byte) string {
	s := strings.TrimSpace(string(out))
	if s == "" {
		return ""
	}
	return "\n  " + strings.ReplaceAll(s, "\n", "\n  ")
}

// configureController writes the config, gives the controller its API token, and checks Proxmox from the VM.
func (in *Installer) configureController(ctx context.Context, vmid int, s *settings.Settings) error {
	in.Out.Step("Configure the controller")
	if err := in.pushConfig(ctx, vmid, s); err != nil {
		return err
	}
	if err := in.ensureToken(ctx, vmid); err != nil {
		return err
	}
	if err := in.grantACLs(ctx, s); err != nil {
		return err
	}
	if out, err := in.asParcon(ctx, vmid, time.Minute, nil, "parcon", "check", "proxmox"); err != nil {
		return fmt.Errorf("parcon check proxmox failed in the controller VM: %w%s", err, indent(out))
	}
	return nil
}

// ensureToken gives the controller the API token. A token's secret is shown only when it is created, so a token
// the controller already holds is kept; otherwise a new one is made and its secret piped straight into the VM,
// never logged or written to the host's disk.
func (in *Installer) ensureToken(ctx context.Context, vmid int) error {
	tokens, err := in.PVE.Tokens(ctx, PVEUser)
	if err != nil {
		tokens = nil // the user has no tokens yet, or doesn't exist in a dry run
	}
	exists := slices.Contains(tokens, TokenName)
	if exists {
		if _, err := in.asParcon(ctx, vmid, 30*time.Second, nil, "test", "-s", config.TokenSecretFile); err == nil {
			in.Out.Say("    the controller already holds token %s!%s", PVEUser, TokenName)
			return nil
		}
		if err := in.run(ctx, "pveum", "user", "token", "remove", PVEUser, TokenName); err != nil {
			return err
		}
	}
	out, err := in.Change.Run(ctx, pvecli.C("pveum", "user", "token", "add", PVEUser, TokenName, "--privsep", "1",
		"--comment", "proxmox-actions-runners controller", "--output-format", "json"))
	if err != nil || in.Change.DryRun {
		return err
	}
	var tok struct {
		Value string `json:"value"`
	}
	if json.Unmarshal(out, &tok) != nil || tok.Value == "" {
		return errors.New("creating the API token returned no secret")
	}
	if err := in.writeControllerFile(ctx, vmid, config.TokenSecretFile, []byte(tok.Value+"\n")); err != nil {
		return err
	}
	in.Out.Say("    created token %s!%s and wrote its secret to the controller", PVEUser, TokenName)
	return nil
}

// AppMode is how install gets its GitHub App.
type AppMode string

const (
	// AppManifest creates a new App with GitHub's manifest flow, in a browser on any device.
	AppManifest AppMode = "manifest"
	// AppManual uses an existing App: its Client ID and private key.
	AppManual AppMode = "manual"
)

// setupApp creates or imports the GitHub App, waits for the user to install it, and learns from the installation
// which organization or repository the runners serve. Each ID is saved to the settings as soon as it is known, so a
// re-run after a failure continues with the same App instead of creating another.
func (in *Installer) setupApp(ctx context.Context, vmid int, s *settings.Settings, mode AppMode, clientID string) error {
	in.Out.Step("GitHub App")
	if s.GitHub.App.ClientID == "" {
		if mode == AppManual {
			if err := in.importAppKey(ctx, vmid); err != nil {
				return err
			}
		} else {
			var err error
			if clientID, s.GitHub.App.Slug, err = in.createApp(ctx, vmid); err != nil {
				return err
			}
		}
		s.GitHub.App.ClientID = clientID
		if err := in.saveAndPush(ctx, vmid, s); err != nil {
			return err
		}
	}
	if s.GitHub.App.InstallationID == 0 {
		in.askToInstall(s.GitHub.App)
		out, err := in.asParcon(ctx, vmid, 16*time.Minute, nil, "parcon", "github", "app", "wait-installation",
			"-client-id", s.GitHub.App.ClientID)
		if err != nil {
			return fmt.Errorf("the App wasn't installed; run parcon install again to keep waiting: %w", err)
		}
		var inst installation
		if err := json.Unmarshal(out, &inst); err != nil || inst.InstallationID == 0 {
			return fmt.Errorf("parcon github app wait-installation printed %q", out)
		}
		target, err := installationTarget(inst, s.GitHub.ConfigURL, in.Term)
		if err != nil {
			return err
		}
		if s.GitHub.ConfigURL != "" && s.GitHub.ConfigURL != target {
			return fmt.Errorf("the App is installed for %s, but --github-url is %s", target, s.GitHub.ConfigURL)
		}
		s.GitHub.ConfigURL = target
		s.GitHub.App.InstallationID = inst.InstallationID
		if err := s.Validate(); err != nil {
			return err
		}
		in.Out.Say("    the App is installed; the runners serve %s", target)
		if err := in.saveAndPush(ctx, vmid, s); err != nil {
			return err
		}
	}
	if out, err := in.asParcon(ctx, vmid, time.Minute, nil, "parcon", "check", "github"); err != nil {
		return fmt.Errorf("parcon check github failed in the controller VM with App %s; if the App was deleted in "+
			"GitHub, uninstall and install again: %w%s", s.GitHub.App.ClientID, err, indent(out))
	}
	return nil
}

// saveAndPush saves the settings and pushes the controller's config.
func (in *Installer) saveAndPush(ctx context.Context, vmid int, s *settings.Settings) error {
	if err := in.saveSettings(s); err != nil {
		return err
	}
	return in.pushConfig(ctx, vmid, s)
}

// installation is what `parcon github app wait-installation` prints.
type installation struct {
	InstallationID int64    `json:"installationId"`
	Account        string   `json:"account"`
	AccountType    string   `json:"accountType"`
	Repositories   []string `json:"repositories"`
}

// installationTarget returns the organization or repository the runners serve. An organization's runners serve the
// organization. A personal account can only have repository runners: with one repository, that's the one; with
// several, --github-url or the user picks.
func installationTarget(inst installation, wanted string, t term.Terminal) (string, error) {
	base := "https://github.com/" + inst.Account
	if inst.AccountType == "Organization" {
		return base, nil
	}
	switch len(inst.Repositories) {
	case 0:
		return "", fmt.Errorf("the App is installed on %s's personal account without a repository; add one under "+
			"the App's installation settings and run parcon install again", inst.Account)
	case 1:
		return base + "/" + inst.Repositories[0], nil
	}
	for _, r := range inst.Repositories {
		if wanted == base+"/"+r {
			return wanted, nil
		}
	}
	i, err := t.Choose(fmt.Sprintf("    The App can reach several of %s's repositories. Which one do the runners "+
		"serve?", inst.Account), inst.Repositories)
	if errors.Is(err, term.ErrNoTerminal) {
		return "", fmt.Errorf("the App can reach several of %s's repositories (%s); run parcon install again with "+
			"--github-url %s/<repo> for the one the runners serve", inst.Account,
			strings.Join(inst.Repositories, " "), base)
	}
	if err != nil {
		return "", err
	}
	return base + "/" + inst.Repositories[i], nil
}

// newState returns a random state for the helper page's link: 32 bytes from the kernel's cryptographic random
// source, in unpadded base64url, so nobody can guess the line install will accept.
func newState() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // crypto/rand.Read never fails on Linux.
	return base64.RawURLEncoding.EncodeToString(b)
}

// askToInstall tells the user to install the App while parcon waits for it. The install link is the very last line,
// so it is what the user sees while the terminal waits.
func (in *Installer) askToInstall(app settings.App) {
	in.Out.Say(`
    Now install the App. On an organization, install it for all repositories; on your personal account, choose
    the repository the runners serve. parcon waits up to 15 minutes, and carries on by itself once it is installed.`)
	if app.Slug == "" {
		// An existing App: parcon doesn't know its name, so it can't make the link.
		in.Out.Say(`
    Install App %s from its settings on GitHub (Settings > Developer settings > GitHub Apps > the App > Install
    App), unless it is installed already.`, app.ClientID)
		return
	}
	in.Out.Say(`
    Open this link to install it:

      https://github.com/apps/%s/installations/new`, app.Slug)
}

// createApp runs the manifest flow and returns the new App's Client ID and slug. The code the user pastes goes only
// into the controller VM, on stdin.
func (in *Installer) createApp(ctx context.Context, vmid int) (clientID, slug string, err error) {
	state := newState()
	in.Out.Say(`
    Create your GitHub App in a browser on any device:

      %s?state=%s

    Choose your organization or your personal account there, then create the App on GitHub. The page then shows
    one line to copy. Paste it here.
`, HelperURL, state)
	line, err := in.Term.Line("    Line from the page: ")
	if errors.Is(err, term.ErrNoTerminal) {
		return "", "", errors.New("creating the GitHub App needs a terminal; run parcon install in one, or use an " +
			"existing App with --app manual --client-id")
	}
	if err != nil {
		return "", "", err
	}
	got, code, ok := strings.Cut(strings.TrimSpace(line), ".")
	if !ok || got != state || code == "" {
		return "", "", errors.New("that line isn't from this install's link; run parcon install again")
	}
	out, err := in.asParcon(ctx, vmid, time.Minute, []byte(code+"\n"), "parcon", "github", "app", "create")
	if err != nil {
		return "", "", fmt.Errorf("creating the GitHub App failed: %w", err)
	}
	var app struct {
		ClientID string `json:"clientId"`
		Slug     string `json:"slug"`
		Owner    string `json:"owner"`
	}
	if err := json.Unmarshal(out, &app); err != nil || app.ClientID == "" {
		return "", "", fmt.Errorf("parcon github app create printed %q", out)
	}
	in.Out.Say("\n    Created GitHub App %s, owned by %s.", app.Slug, app.Owner)
	return app.ClientID, app.Slug, nil
}

// importAppKey reads an existing App's private key from the terminal and pipes it into the controller VM.
func (in *Installer) importAppKey(ctx context.Context, vmid int) error {
	key, err := in.Term.PEM("    Paste the App's private key (the whole PEM, ending with its END line):")
	if errors.Is(err, term.ErrNoTerminal) {
		return errors.New("--app manual reads the App's private key from the terminal; run parcon install in one")
	}
	if err != nil {
		return err
	}
	if _, err := in.asParcon(ctx, vmid, 30*time.Second, []byte(key), "parcon", "github", "app", "import"); err != nil {
		return fmt.Errorf("importing the key failed: %w", err)
	}
	return nil
}

// startController enables and starts parcon.service, and returns when it started.
func (in *Installer) startController(ctx context.Context, vmid int) (time.Time, error) {
	in.Out.Step("Start the controller")
	started := in.Now()
	in.Change.Log(fmt.Sprintf("controller VM %d: systemctl enable --now parcon.service", vmid))
	if _, err := in.guestExec(ctx, vmid, time.Minute, nil, "systemctl", "enable", "--now", "parcon.service"); err != nil {
		return started, fmt.Errorf("starting parcon.service failed: %w", err)
	}
	return started, nil
}

// waitSession waits for the controller to log that its scale set session is open, which it does once the scale
// set is registered and it is listening for jobs.
func (in *Installer) waitSession(ctx context.Context, vmid int, since time.Time, within time.Duration) error {
	for deadline := in.Now().Add(within); ; {
		out, err := in.guestExec(ctx, vmid, 30*time.Second, nil, "journalctl", "-u", "parcon.service", "--since",
			"@"+strconv.FormatInt(since.Unix(), 10), "-o", "cat")
		if err == nil && strings.Contains(string(out), "scale set session opened") {
			return nil
		}
		if in.Now().After(deadline) {
			return fmt.Errorf("the controller didn't open its scale set session within %s; see parcon status and "+
				"journalctl -u parcon in VM %d", within, vmid)
		}
		if err := in.Sleep(ctx, 5*time.Second); err != nil {
			return err
		}
	}
}
