package release

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/klponce/proxmox-actions-runners/internal/github"
	"github.com/klponce/proxmox-actions-runners/internal/pvecli"
)

// Info is a published release.
type Info struct {
	Version     Version
	PublishedAt time.Time
}

// Source is where releases come from.
type Source interface {
	// Releases lists the published releases.
	Releases(ctx context.Context) ([]Info, error)
	// Open opens one of a release's assets.
	Open(ctx context.Context, v Version, asset string) (io.ReadCloser, error)
}

// GitHub is the project's releases on github.com.
type GitHub struct {
	HTTP *http.Client
	// DownloadBase is where assets are downloaded from. Empty means https://github.com.
	DownloadBase string
}

// Releases implements Source.
func (g GitHub) Releases(ctx context.Context) ([]Info, error) {
	rels, err := github.Releases(ctx, Repo)
	if err != nil {
		return nil, err
	}
	var out []Info
	for _, r := range rels {
		// A tag that isn't a version isn't a release parcon can install.
		if v, err := ParseVersion(r.Tag); err == nil {
			out = append(out, Info{Version: v, PublishedAt: r.PublishedAt})
		}
	}
	return out, nil
}

// assetURL is an asset's download URL.
func (g GitHub) assetURL(v Version, asset string) string {
	base := g.DownloadBase
	if base == "" {
		base = "https://github.com"
	}
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", base, Repo, v.Tag(), url.PathEscape(asset))
}

// Open implements Source.
func (g GitHub) Open(ctx context.Context, v Version, asset string) (io.ReadCloser, error) {
	client := g.HTTP
	if client == nil {
		// Images are gigabytes; the timeout is for a stalled download, not a slow one.
		client = &http.Client{Transport: &http.Transport{ResponseHeaderTimeout: time.Minute}}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, g.assetURL(v, asset), nil)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	if resp.StatusCode != http.StatusOK {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("download %s: %s", asset, resp.Status)
	}
	return resp.Body, nil
}

// AssetURL is the download URL of an asset of a release on github.com, for the controller VM, which downloads its
// own parcon.
func AssetURL(v Version, asset string) string { return GitHub{}.assetURL(v, asset) }

// Latest returns the newest release, or the newest release or pre-release with includePre.
func Latest(ctx context.Context, src Source, includePre bool) (Info, error) {
	rels, err := src.Releases(ctx)
	if err != nil {
		return Info{}, err
	}
	var best Info
	found := false
	for _, r := range rels {
		if r.Version.IsPre() && !includePre {
			continue
		}
		if !found || r.Version.Compare(best.Version) > 0 {
			best, found = r, true
		}
	}
	if !found {
		return Info{}, errors.New("no release is published yet")
	}
	return best, nil
}

// Assets are a release's verified checksums, and a directory to download its assets into.
type Assets struct {
	Version Version
	Sums    Sums
	Dir     string
	src     Source
}

// Fetch downloads a release's SHA256SUMS and SHA256SUMS.sig and checks the signature with keys. Assets are then
// downloaded into dir with Get.
func Fetch(ctx context.Context, src Source, v Version, dir string, keys []ed25519.PublicKey) (*Assets, error) {
	sums, err := readAll(ctx, src, v, "SHA256SUMS")
	if err != nil {
		return nil, err
	}
	sig, err := readAll(ctx, src, v, "SHA256SUMS.sig")
	if err != nil {
		return nil, fmt.Errorf("%w; a release without a signature can't be verified", err)
	}
	if err := VerifySignature(sums, sig, keys); err != nil {
		return nil, fmt.Errorf("release %s: %w", v, err)
	}
	parsed, err := ParseSums(sums)
	if err != nil {
		return nil, fmt.Errorf("release %s: %w", v, err)
	}
	return &Assets{Version: v, Sums: parsed, Dir: dir, src: src}, nil
}

// Local uses assets already in dir, with the checksums in dir/SHA256SUMS. Nothing is signed: whoever put the
// files there, as root, vouches for them. It is for the integration tests and development builds.
func Local(dir string, v Version) (*Assets, error) {
	data, err := os.ReadFile(filepath.Join(dir, "SHA256SUMS")) //nolint:gosec // G304: the assets directory.
	if err != nil {
		return nil, fmt.Errorf("local assets: %w", err)
	}
	sums, err := ParseSums(data)
	if err != nil {
		return nil, fmt.Errorf("local assets: %w", err)
	}
	return &Assets{Version: v, Sums: sums, Dir: dir}, nil
}

func readAll(ctx context.Context, src Source, v Version, asset string) ([]byte, error) {
	r, err := src.Open(ctx, v, asset)
	if err != nil {
		return nil, err
	}
	defer func() { _ = r.Close() }()
	data, err := io.ReadAll(io.LimitReader(r, 1<<20))
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", asset, err)
	}
	return data, nil
}

// Get returns the path of a verified asset in the assets directory, downloading it first unless a file with the
// right checksum is already there.
func (a *Assets) Get(ctx context.Context, name string) (string, error) {
	want, ok := a.Sums[name]
	if !ok {
		return "", fmt.Errorf("release %s has no %s", a.Version, name)
	}
	path := filepath.Join(a.Dir, name)
	if sum, err := fileSum(path); err == nil && sum == want {
		return path, nil
	}
	if a.src == nil {
		return "", fmt.Errorf("%s is missing or doesn't match its checksum", path)
	}
	r, err := a.src.Open(ctx, a.Version, name)
	if err != nil {
		return "", err
	}
	defer func() { _ = r.Close() }()
	part := path + ".part"
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600) //nolint:gosec // G304: our directory.
	if err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), r)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		_ = os.Remove(part)
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	if [32]byte(h.Sum(nil)) != want {
		_ = os.Remove(part)
		return "", fmt.Errorf("%s doesn't match its checksum in the release's SHA256SUMS", name)
	}
	if err := os.Rename(part, path); err != nil {
		return "", fmt.Errorf("download %s: %w", name, err)
	}
	return path, nil
}

func fileSum(path string) ([32]byte, error) {
	f, err := os.Open(path) //nolint:gosec // G304: our directory.
	if err != nil {
		return [32]byte{}, err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return [32]byte{}, err
	}
	return [32]byte(h.Sum(nil)), nil
}

// ReplaceBinary installs the executable at src as dst atomically, so a parcon running from dst keeps running and
// the next start gets the new one.
func ReplaceBinary(src, dst string) error {
	data, err := os.ReadFile(src) //nolint:gosec // G304: a verified asset.
	if err != nil {
		return fmt.Errorf("install %s: %w", dst, err)
	}
	return pvecli.WriteFileAtomic(dst, data, 0o755)
}
