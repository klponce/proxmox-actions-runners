package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// restTimeout bounds each REST call this package makes itself.
const restTimeout = 30 * time.Second

// GitHub requires self-hosted runners with automatic updates off to be updated within RunnerUpdateDeadline of a
// runner release. The controller warns from RunnerUpdateWarnAfter and reports an error from RunnerUpdateErrorAfter,
// leaving time to install a new runner image before the deadline.
const (
	RunnerUpdateDeadline   = 30 * 24 * time.Hour
	RunnerUpdateWarnAfter  = 7 * 24 * time.Hour
	RunnerUpdateErrorAfter = 21 * 24 * time.Hour
)

// RunnerRelease is a release of the GitHub Actions runner.
type RunnerRelease struct {
	// Version is the release's version without the leading v, such as 2.337.0.
	Version     string
	PublishedAt time.Time
}

// defaultAPIBase is GitHub's REST API.
const defaultAPIBase = "https://api.github.com"

// LatestRunnerRelease returns the latest release of actions/runner from github.com. Self-hosted runners with
// automatic updates off, as ours are, must be updated within 30 days of a release, so the controller compares it
// with the runner in the current template. The release is public, so this needs no credentials.
func LatestRunnerRelease(ctx context.Context) (RunnerRelease, error) {
	return latestRunnerRelease(ctx, &http.Client{Timeout: restTimeout}, defaultAPIBase)
}

// LatestRunnerRelease is the package-level LatestRunnerRelease, through the client's HTTP client.
func (c *Client) LatestRunnerRelease(ctx context.Context) (RunnerRelease, error) {
	return latestRunnerRelease(ctx, c.http, c.apiBase)
}

func latestRunnerRelease(ctx context.Context, client *http.Client, apiBase string) (RunnerRelease, error) {
	const path = "/repos/actions/runner/releases/latest"
	var out struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
	}
	status, err := restCall(ctx, client, apiBase, http.MethodGet, path, "", &out)
	switch {
	case err != nil:
		return RunnerRelease{}, fmt.Errorf("latest runner release: %w", err)
	case status != http.StatusOK:
		return RunnerRelease{}, fmt.Errorf("latest runner release: GET %s: %d %s", path, status,
			http.StatusText(status))
	}
	version := strings.TrimPrefix(out.TagName, "v")
	if _, ok := parseVersion(version); !ok {
		return RunnerRelease{}, fmt.Errorf("latest runner release: unexpected tag %q", out.TagName)
	}
	return RunnerRelease{Version: version, PublishedAt: out.PublishedAt}, nil
}

// Release is a published release of a repository.
type Release struct {
	Tag         string
	Prerelease  bool
	PublishedAt time.Time
}

// Releases lists the newest published releases of a public repository on github.com, such as
// klponce/proxmox-actions-runners, pre-releases included. It needs no credentials.
func Releases(ctx context.Context, repo string) ([]Release, error) {
	return releases(ctx, &http.Client{Timeout: restTimeout}, defaultAPIBase, repo)
}

func releases(ctx context.Context, client *http.Client, apiBase, repo string) ([]Release, error) {
	path := "/repos/" + repo + "/releases?per_page=50"
	var out []struct {
		TagName     string    `json:"tag_name"`
		Draft       bool      `json:"draft"`
		Prerelease  bool      `json:"prerelease"`
		PublishedAt time.Time `json:"published_at"`
	}
	status, err := restCall(ctx, client, apiBase, http.MethodGet, path, "", &out)
	switch {
	case err != nil:
		return nil, fmt.Errorf("releases of %s: %w", repo, err)
	case status == http.StatusForbidden || status == http.StatusTooManyRequests:
		return nil, fmt.Errorf("releases of %s: GitHub's rate limit for unauthenticated requests is used up; try "+
			"again in an hour", repo)
	case status != http.StatusOK:
		return nil, fmt.Errorf("releases of %s: GET %s: %d %s", repo, path, status, http.StatusText(status))
	}
	var rels []Release
	for _, r := range out {
		if !r.Draft {
			rels = append(rels, Release{Tag: r.TagName, Prerelease: r.Prerelease, PublishedAt: r.PublishedAt})
		}
	}
	return rels, nil
}

// restCall sends a request to GitHub's REST API at apiBase, with token as the bearer token if it isn't empty, and
// decodes a 2xx JSON response into out. It returns the status code. Errors never include the URL, whose path may
// hold a secret.
func restCall(ctx context.Context, client *http.Client, apiBase, method, path, token string, out any) (int, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, nil)
	if err != nil {
		return 0, errors.New("invalid request")
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", systemName)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		// A *url.Error quotes the URL; keep only the cause.
		if uerr, ok := errors.AsType[*url.Error](err); ok {
			err = uerr.Err
		}
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out); err != nil {
		return 0, fmt.Errorf("decode the response: %w", err)
	}
	return resp.StatusCode, nil
}

// Staleness is how far a runner is behind the latest release.
type Staleness int

const (
	// Current means the runner is the latest release or newer.
	Current Staleness = iota
	// Behind means a newer release came out less than RunnerUpdateWarnAfter ago.
	Behind
	// BehindWarn means a newer release came out at least RunnerUpdateWarnAfter ago.
	BehindWarn
	// BehindError means a newer release came out at least RunnerUpdateErrorAfter ago: install a new runner image
	// before RunnerUpdateDeadline.
	BehindError
)

// Staleness says how far a runner of version have, such as the one a template contains, is behind the release at
// now. The controller and parcon check template both judge by it.
func (r RunnerRelease) Staleness(have string, now time.Time) Staleness {
	if !r.NewerThan(have) {
		return Current
	}
	switch age := now.Sub(r.PublishedAt); {
	case age >= RunnerUpdateErrorAfter:
		return BehindError
	case age >= RunnerUpdateWarnAfter:
		return BehindWarn
	default:
		return Behind
	}
}

// NewerThan reports whether the release is newer than version. A version that can't be parsed counts as older, so it
// gets reported rather than silently accepted. The release's own version is valid: LatestRunnerRelease checks it.
func (r RunnerRelease) NewerThan(version string) bool {
	have, ok := parseVersion(version)
	if !ok {
		return true
	}
	latest, _ := parseVersion(r.Version)
	for i := range latest {
		if latest[i] != have[i] {
			return latest[i] > have[i]
		}
	}
	return false
}

// parseVersion parses major.minor.patch.
func parseVersion(v string) ([3]int, bool) {
	var out [3]int
	parts := strings.Split(v, ".")
	if len(parts) != 3 {
		return out, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return out, false
		}
		out[i] = n
	}
	return out, true
}
