// Package proxmox is a thin client for the parts of the Proxmox VE API the controller uses: cloning and configuring
// VMs, power management, listing by tag, task tracking, and the QEMU guest agent.
//
// A Client is bound to one node, because the project supports a single standalone node only. Methods that start a
// Proxmox task wait for it to finish and return an error unless it succeeded.
package proxmox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	defaultRequestTimeout  = 60 * time.Second
	defaultPollInterval    = 250 * time.Millisecond
	defaultMaxPollInterval = 2 * time.Second

	// maxResponseBytes bounds how much of a response body is read.
	maxResponseBytes = 32 << 20
)

// Options configures a Client.
type Options struct {
	// URL is the API base URL, such as https://pve.example.com:8006/api2/json.
	URL string
	// TokenID is the API token ID, such as par@pve!controller.
	TokenID string
	// TokenSecret is the token's secret. It is sent only in the Authorization header and never logged.
	TokenSecret string
	// TLSFingerprint pins the server certificate by its SHA-256 fingerprint (AA:BB:...). If empty, the system trust
	// store verifies the certificate instead.
	TLSFingerprint string
	// Node is the node every VM operation targets.
	Node string

	// RequestTimeout limits each HTTP request. Zero means 60 seconds.
	RequestTimeout time.Duration
	// PollInterval and MaxPollInterval control how often task and guest-exec status is polled: the interval starts
	// at PollInterval and doubles up to MaxPollInterval. Zero means 250ms and 2s.
	PollInterval    time.Duration
	MaxPollInterval time.Duration
	// UserAgent is sent with every request.
	UserAgent string
}

// Client talks to the Proxmox VE API on behalf of one API token and one node.
type Client struct {
	base            *url.URL
	authHeader      string
	node            string
	http            *http.Client
	pollInterval    time.Duration
	maxPollInterval time.Duration
	userAgent       string
}

// New returns a Client for opts. It doesn't contact the server.
func New(opts Options) (*Client, error) {
	base, err := url.Parse(opts.URL)
	if err != nil || base.Scheme != "https" || base.Host == "" {
		return nil, fmt.Errorf("proxmox URL %q must be an https:// URL", opts.URL)
	}
	if opts.TokenID == "" || opts.TokenSecret == "" {
		return nil, errors.New("proxmox token ID and secret are required")
	}
	if opts.Node == "" {
		return nil, errors.New("proxmox node is required")
	}

	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	if opts.TLSFingerprint != "" {
		want, err := parseFingerprint(opts.TLSFingerprint)
		if err != nil {
			return nil, err
		}
		pinCertificate(tlsConfig, want)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.TLSClientConfig = tlsConfig

	c := &Client{
		base:            base,
		authHeader:      "PVEAPIToken=" + opts.TokenID + "=" + opts.TokenSecret,
		node:            opts.Node,
		http:            &http.Client{Transport: transport, Timeout: orDefault(opts.RequestTimeout, defaultRequestTimeout)},
		pollInterval:    orDefault(opts.PollInterval, defaultPollInterval),
		maxPollInterval: orDefault(opts.MaxPollInterval, defaultMaxPollInterval),
		userAgent:       opts.UserAgent,
	}
	if c.maxPollInterval < c.pollInterval {
		c.maxPollInterval = c.pollInterval
	}
	return c, nil
}

// Node returns the node this client targets.
func (c *Client) Node() string { return c.node }

func orDefault(d, def time.Duration) time.Duration {
	if d <= 0 {
		return def
	}
	return d
}

func parseFingerprint(s string) ([]byte, error) {
	b, err := hex.DecodeString(strings.ReplaceAll(s, ":", ""))
	if err != nil || len(b) != sha256.Size {
		return nil, errors.New("proxmox TLS fingerprint must be a SHA-256 fingerprint: 32 hex bytes separated by colons")
	}
	return b, nil
}

// pinCertificate makes cfg accept exactly the certificate whose SHA-256 fingerprint is want. Proxmox hosts usually
// have self-signed certificates, so the pin replaces chain verification rather than adding to it.
func pinCertificate(cfg *tls.Config, want []byte) {
	// VerifyConnection below checks the pinned fingerprint instead of the chain.
	cfg.InsecureSkipVerify = true
	cfg.VerifyConnection = func(cs tls.ConnectionState) error {
		if len(cs.PeerCertificates) == 0 {
			return errors.New("proxmox server sent no certificate")
		}
		got := sha256.Sum256(cs.PeerCertificates[0].Raw)
		if !bytes.Equal(got[:], want) {
			return errors.New("proxmox server certificate doesn't match the pinned fingerprint")
		}
		return nil
	}
}

// nodePath returns /nodes/<node> followed by the escaped path segments.
func (c *Client) nodePath(segments ...string) string {
	return "/nodes/" + url.PathEscape(c.node) + "/" + joinSegments(segments...)
}

// vmPath returns /nodes/<node>/qemu/<vmid> followed by the escaped path segments.
func (c *Client) vmPath(vmid int, segments ...string) string {
	return c.nodePath(append([]string{"qemu", fmt.Sprint(vmid)}, segments...)...)
}

func joinSegments(segments ...string) string {
	escaped := make([]string, len(segments))
	for i, s := range segments {
		escaped[i] = url.PathEscape(s)
	}
	return strings.Join(escaped, "/")
}

// do sends one API request and decodes the response's data field into out, if out isn't nil. GET and DELETE send
// params in the query string; POST and PUT send them form-encoded. Parameters never appear in returned errors,
// because some of them (guest-agent file content and stdin) carry secrets.
func (c *Client) do(ctx context.Context, method, path string, params url.Values, out any) error {
	var body io.Reader
	query := ""
	switch method {
	case http.MethodPost, http.MethodPut:
		body = strings.NewReader(params.Encode())
	default:
		query = params.Encode()
	}
	target, err := c.requestURL(path, query)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	// The host is the operator-configured Proxmox API; only the path varies, built from escaped segments.
	req, err := http.NewRequestWithContext(ctx, method, target, body) //nolint:gosec // G704: see above.
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", c.authHeader)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if c.userAgent != "" {
		req.Header.Set("User-Agent", c.userAgent)
	}

	resp, err := c.http.Do(req) //nolint:gosec // G704: the request targets the configured Proxmox API.
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, redactURLError(err))
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s %s: read response: %w", method, path, err)
	}

	var envelope struct {
		Data    json.RawMessage   `json:"data"`
		Message string            `json:"message"`
		Errors  map[string]string `json:"errors"`
	}
	decodeErr := json.Unmarshal(data, &envelope)

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg := strings.TrimSpace(envelope.Message)
		if msg == "" {
			msg = strings.TrimSpace(strings.TrimPrefix(resp.Status, fmt.Sprint(resp.StatusCode)))
		}
		return &APIError{Method: method, Path: path, StatusCode: resp.StatusCode, Message: msg,
			ParamErrors: envelope.Errors}
	}
	if decodeErr != nil {
		return fmt.Errorf("%s %s: decode response: %w", method, path, decodeErr)
	}
	if out == nil || len(envelope.Data) == 0 || string(envelope.Data) == "null" {
		return nil
	}
	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("%s %s: decode data: %w", method, path, err)
	}
	return nil
}

// requestURL joins the base URL with path, which is already escaped segment by segment (see nodePath).
func (c *Client) requestURL(path, query string) (string, error) {
	raw := strings.TrimSuffix(c.base.EscapedPath(), "/") + path
	unescaped, err := url.PathUnescape(raw)
	if err != nil {
		return "", err
	}
	u := *c.base
	u.Path, u.RawPath, u.RawQuery = unescaped, raw, query
	return u.String(), nil
}

// redactURLError strips the request URL from a transport error. The URL holds no secrets, but query strings can
// hold parameters, and this keeps errors short.
func redactURLError(err error) error {
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		return urlErr.Err
	}
	return err
}

func (c *Client) get(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodGet, path, params, out)
}

func (c *Client) post(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodPost, path, params, out)
}

func (c *Client) put(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodPut, path, params, out)
}

func (c *Client) delete(ctx context.Context, path string, params url.Values, out any) error {
	return c.do(ctx, http.MethodDelete, path, params, out)
}
