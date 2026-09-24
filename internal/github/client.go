// Package github is a thin wrapper over github.com/actions/scaleset, GitHub's client for the runner scale set APIs.
//
// It authenticates as a GitHub App, registers scale sets, generates JIT runner configs, and runs the message
// session that reports how many runners GitHub wants. The rest of the controller uses only this package's types, so
// changes to actions/scaleset, which is still in public preview, stay contained here.
package github

import (
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/actions/scaleset"
)

// systemName identifies this project to GitHub in the scale set client's User-Agent.
const systemName = "proxmox-actions-runners"

// Options configures a Client.
type Options struct {
	// ConfigURL is the organization or repository URL, such as https://github.com/my-org.
	ConfigURL string
	// ClientID identifies the GitHub App. Its App ID also works.
	ClientID string
	// InstallationID is the App's installation on the organization or repository.
	InstallationID int64
	// PrivateKeyPEM is the App's private key. It is used only to sign short-lived JWTs and is never logged.
	PrivateKeyPEM string
	// Version is parcon's version, reported to GitHub.
	Version string
	// Logger receives the scale set client's own logs. Nil discards them.
	Logger *slog.Logger

	// httpOptions lets tests trust a fake server and turn off retries.
	httpOptions []scaleset.HTTPOption
	// httpClient and apiBaseURL point LatestRunnerRelease at a fake server.
	httpClient *http.Client
	apiBaseURL string
}

// Client talks to GitHub on behalf of one GitHub App installation.
type Client struct {
	ss     *scaleset.Client
	logger *slog.Logger
	// http and apiBase are for LatestRunnerRelease, which reads GitHub's public REST API directly.
	http    *http.Client
	apiBase string
}

// New returns a Client for opts. It checks the private key but doesn't contact GitHub.
func New(opts Options) (*Client, error) {
	switch {
	case opts.ConfigURL == "":
		return nil, errors.New("github: config URL is required")
	case opts.ClientID == "":
		return nil, errors.New("github: App client ID is required")
	case opts.InstallationID <= 0:
		return nil, errors.New("github: App installation ID is required")
	}
	if err := checkPrivateKey(opts.PrivateKeyPEM); err != nil {
		return nil, err
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}

	httpOptions := append([]scaleset.HTTPOption{scaleset.WithLogger(logger)}, opts.httpOptions...)
	ss, err := scaleset.NewClientWithGitHubApp(scaleset.ClientWithGitHubAppConfig{
		GitHubConfigURL: opts.ConfigURL,
		GitHubAppAuth: scaleset.GitHubAppAuth{
			ClientID:       opts.ClientID,
			InstallationID: opts.InstallationID,
			PrivateKey:     opts.PrivateKeyPEM,
		},
		SystemInfo: scaleset.SystemInfo{System: systemName, Version: opts.Version, Subsystem: "controller"},
	}, httpOptions...)
	if err != nil {
		return nil, fmt.Errorf("github: %w", err)
	}
	c := &Client{ss: ss, logger: logger, http: opts.httpClient, apiBase: opts.apiBaseURL}
	if c.http == nil {
		c.http = &http.Client{Timeout: restTimeout}
	}
	if c.apiBase == "" {
		c.apiBase = "https://api.github.com"
	}
	return c, nil
}

// checkPrivateKey confirms that key is a PEM-encoded RSA private key, the kind GitHub issues for Apps, so a bad key
// fails at startup rather than on the first request. Errors never include the key.
func checkPrivateKey(key string) error {
	block, _ := pem.Decode([]byte(strings.TrimSpace(key)))
	if block == nil {
		return errors.New("github: App private key is not PEM-encoded")
	}
	switch block.Type {
	case "RSA PRIVATE KEY":
		if _, err := x509.ParsePKCS1PrivateKey(block.Bytes); err != nil {
			return errors.New("github: App private key is not a valid RSA key")
		}
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return errors.New("github: App private key is not a valid PKCS #8 key")
		}
		if _, ok := parsed.(*rsa.PrivateKey); !ok {
			return errors.New("github: App private key must be an RSA key")
		}
	default:
		return fmt.Errorf("github: App private key has PEM type %q; want an RSA private key", block.Type)
	}
	return nil
}
