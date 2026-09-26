package release

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestParseAndCompareVersions(t *testing.T) {
	ordered := []string{"0.1.0-alpha", "0.1.0-alpha.1", "0.1.0-alpha.beta", "0.1.0-beta", "0.1.0-beta.2",
		"0.1.0-beta.11", "0.1.0-rc.1", "0.1.0", "0.1.1", "0.2.0", "1.0.0", "v10.0.0"}
	for i := range ordered {
		a, err := ParseVersion(ordered[i])
		if err != nil {
			t.Fatal(err)
		}
		if same, _ := ParseVersion(ordered[i]); a.Compare(same) != 0 {
			t.Errorf("%s != itself", a)
		}
		for j := i + 1; j < len(ordered); j++ {
			b, _ := ParseVersion(ordered[j])
			if a.Compare(b) != -1 || b.Compare(a) != 1 {
				t.Errorf("%s isn't older than %s", a, b)
			}
		}
	}
	v, _ := ParseVersion("v0.2.0-rc.1")
	if v.String() != "0.2.0-rc.1" || v.Tag() != "v0.2.0-rc.1" || !v.IsPre() {
		t.Errorf("v = %+v", v)
	}
	for _, bad := range []string{"", "0.1", "0.1.0-RC1", "01.1.0", "0.1.0-", "latest", "0.1.0 ", "0.1.0/../x"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("ParseVersion(%q): no error", bad)
		}
	}
}

// The fixture was signed with `openssl pkeyutl -sign -rawin`, as the release workflow signs, by a throwaway key.
func TestVerifyOpenSSLSignature(t *testing.T) {
	keys, err := loadKeys(os.DirFS("testdata"), ".")
	if err != nil || len(keys) != 1 {
		t.Fatalf("keys = %v, %v", keys, err)
	}
	sums, _ := os.ReadFile("testdata/SHA256SUMS")
	sig, _ := os.ReadFile("testdata/SHA256SUMS.sig")
	if err := VerifySignature(sums, sig, keys); err != nil {
		t.Fatal(err)
	}
	tampered := bytes.Replace(sums, []byte("3a6e"), []byte("3a6f"), 1)
	if err := VerifySignature(tampered, sig, keys); err == nil {
		t.Error("a tampered SHA256SUMS verifies")
	}
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if err := VerifySignature(sums, sig, []ed25519.PublicKey{other}); err == nil {
		t.Error("an untrusted key verifies")
	}
	if err := VerifySignature(sums, sig, nil); !errors.Is(err, ErrNoTrustedKeys) {
		t.Errorf("no keys: %v", err)
	}
}

// parcon must trust at least one key, or it refuses every release.
func TestEmbeddedKeysParse(t *testing.T) {
	keys, err := TrustedKeys()
	if err != nil || len(keys) == 0 {
		t.Fatalf("TrustedKeys = %d keys, %v", len(keys), err)
	}
	if _, err := loadKeys(fstest.MapFS{"k/bad.pem": {Data: []byte("nope")}}, "k"); err == nil {
		t.Error("a bad key file loads")
	}
}

func TestParseSums(t *testing.T) {
	sums, err := ParseSums([]byte(strings.Repeat("a", 64) + "  install.sh\n" + strings.Repeat("b", 64) + " *x.qcow2\n"))
	if err != nil || len(sums) != 2 {
		t.Fatalf("sums = %v, %v", sums, err)
	}
	if h, ok := sums.Hex("x.qcow2"); !ok || h != strings.Repeat("b", 64) {
		t.Errorf("Hex = %q", h)
	}
	for _, bad := range []string{"", "zz  install.sh\n", strings.Repeat("a", 64) + "  ../etc/passwd\n"} {
		if _, err := ParseSums([]byte(bad)); err == nil {
			t.Errorf("ParseSums(%q): no error", bad)
		}
	}
}

// fakeSource serves assets from memory.
type fakeSource struct {
	releases []Info
	assets   map[string][]byte
	opened   []string
}

func (f *fakeSource) Releases(context.Context) ([]Info, error) { return f.releases, nil }

func (f *fakeSource) Open(_ context.Context, v Version, asset string) (io.ReadCloser, error) {
	f.opened = append(f.opened, asset)
	data, ok := f.assets[v.String()+"/"+asset]
	if !ok {
		return nil, fmt.Errorf("download %s: 404 Not Found", asset)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

func TestLatest(t *testing.T) {
	v := func(s string) Info { x, _ := ParseVersion(s); return Info{Version: x, PublishedAt: time.Now()} }
	src := &fakeSource{releases: []Info{v("0.2.0-rc.2"), v("0.1.1"), v("0.1.0"), v("0.2.0-rc.1")}}
	if got, err := Latest(context.Background(), src, false); err != nil || got.Version.String() != "0.1.1" {
		t.Errorf("Latest = %v, %v", got.Version, err)
	}
	if got, err := Latest(context.Background(), src, true); err != nil || got.Version.String() != "0.2.0-rc.2" {
		t.Errorf("Latest with pre = %v, %v", got.Version, err)
	}
	if _, err := Latest(context.Background(), &fakeSource{releases: []Info{v("0.1.0-rc.1")}}, false); err == nil {
		t.Error("only pre-releases: no error")
	}
}

func signedRelease(t *testing.T, version string, assets map[string]string) (*fakeSource, ed25519.PublicKey) {
	t.Helper()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	var sums strings.Builder
	src := &fakeSource{assets: map[string][]byte{}}
	for name, content := range assets {
		sum := sha256.Sum256([]byte(content))
		fmt.Fprintf(&sums, "%s  %s\n", hex.EncodeToString(sum[:]), name)
		src.assets[version+"/"+name] = []byte(content)
	}
	src.assets[version+"/SHA256SUMS"] = []byte(sums.String())
	src.assets[version+"/SHA256SUMS.sig"] = ed25519.Sign(priv, []byte(sums.String()))
	return src, pub
}

func TestFetchAndGet(t *testing.T) {
	ctx := context.Background()
	v, _ := ParseVersion("0.2.0")
	src, pub := signedRelease(t, "0.2.0", map[string]string{"parcon-0.2.0-linux-amd64": "binary", "install.sh": "x"})
	dir := t.TempDir()
	a, err := Fetch(ctx, src, v, dir, []ed25519.PublicKey{pub})
	if err != nil {
		t.Fatal(err)
	}
	path, err := a.Get(ctx, "parcon-0.2.0-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(path); string(data) != "binary" {
		t.Errorf("asset = %q", data)
	}
	// A second Get reuses the file.
	src.opened = nil
	if _, err := a.Get(ctx, "parcon-0.2.0-linux-amd64"); err != nil || len(src.opened) != 0 {
		t.Errorf("second Get downloaded %v, %v", src.opened, err)
	}
	if _, err := a.Get(ctx, "par-runner-0.2.0.qcow2"); err == nil {
		t.Error("an asset the release doesn't list: no error")
	}

	// A corrupted download is refused and left nowhere.
	src.assets["0.2.0/install.sh"] = []byte("evil")
	if _, err := a.Get(ctx, "install.sh"); err == nil || !strings.Contains(err.Error(), "checksum") {
		t.Errorf("corrupted asset: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("files left: %v", entries)
	}
}

func TestFetchRefusesBadSignatures(t *testing.T) {
	ctx := context.Background()
	v, _ := ParseVersion("0.2.0")
	src, pub := signedRelease(t, "0.2.0", map[string]string{"install.sh": "x"})
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	if _, err := Fetch(ctx, src, v, t.TempDir(), []ed25519.PublicKey{other}); err == nil {
		t.Error("an untrusted signature: no error")
	}
	src.assets["0.2.0/SHA256SUMS.sig"][0] ^= 1
	if _, err := Fetch(ctx, src, v, t.TempDir(), []ed25519.PublicKey{pub}); err == nil {
		t.Error("a corrupted signature: no error")
	}
	delete(src.assets, "0.2.0/SHA256SUMS.sig")
	if _, err := Fetch(ctx, src, v, t.TempDir(), []ed25519.PublicKey{pub}); err == nil ||
		!strings.Contains(err.Error(), "without a signature") {
		t.Errorf("no signature: %v", err)
	}
}

func TestLocalAndReplaceBinary(t *testing.T) {
	dir := t.TempDir()
	sum := sha256.Sum256([]byte("new parcon"))
	if err := os.WriteFile(filepath.Join(dir, "parcon-0.2.0-linux-amd64"), []byte("new parcon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SHA256SUMS"),
		[]byte(hex.EncodeToString(sum[:])+"  parcon-0.2.0-linux-amd64\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	v, _ := ParseVersion("0.2.0")
	a, err := Local(dir, v)
	if err != nil {
		t.Fatal(err)
	}
	src, err := a.Get(context.Background(), "parcon-0.2.0-linux-amd64")
	if err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "parcon")
	if err := os.WriteFile(dst, []byte("old parcon"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ReplaceBinary(src, dst); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(dst)
	if data, _ := os.ReadFile(dst); string(data) != "new parcon" || st.Mode().Perm() != 0o755 {
		t.Errorf("dst = %q, %v", data, st.Mode())
	}
}

func TestAssetURL(t *testing.T) {
	v, _ := ParseVersion("0.2.0-rc.1")
	want := "https://github.com/klponce/proxmox-actions-runners/releases/download/v0.2.0-rc.1/parcon-0.2.0-rc.1-linux-amd64"
	if got := AssetURL(v, "parcon-0.2.0-rc.1-linux-amd64"); got != want {
		t.Errorf("AssetURL = %s", got)
	}
}
