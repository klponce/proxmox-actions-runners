package github

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiBase+path, nil)
	if err != nil {
		return RunnerRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", systemName)
	resp, err := client.Do(req)
	if err != nil {
		return RunnerRelease{}, fmt.Errorf("latest runner release: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return RunnerRelease{}, fmt.Errorf("latest runner release: GET %s: %s", path, resp.Status)
	}
	var out struct {
		TagName     string    `json:"tag_name"`
		PublishedAt time.Time `json:"published_at"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&out); err != nil {
		return RunnerRelease{}, fmt.Errorf("latest runner release: decode: %w", err)
	}
	version := strings.TrimPrefix(out.TagName, "v")
	if _, ok := parseVersion(version); !ok {
		return RunnerRelease{}, fmt.Errorf("latest runner release: unexpected tag %q", out.TagName)
	}
	return RunnerRelease{Version: version, PublishedAt: out.PublishedAt}, nil
}

// NewerThan reports whether the release is newer than version, such as the runner version a template contains. A
// version that can't be parsed counts as older, so it gets reported rather than silently accepted.
func (r RunnerRelease) NewerThan(version string) bool {
	have, ok := parseVersion(version)
	if !ok {
		return true
	}
	latest, ok := parseVersion(r.Version)
	if !ok {
		return false
	}
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
