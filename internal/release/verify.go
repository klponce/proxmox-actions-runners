package release

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/x509"
	"embed"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"strings"
)

// keys holds the public keys of the release signing keys, one PEM file per key. See keys/README.md.
//
//go:embed keys
var keys embed.FS

// TrustedKeys returns the public keys parcon accepts release signatures from.
func TrustedKeys() ([]ed25519.PublicKey, error) {
	return loadKeys(keys, "keys")
}

func loadKeys(fsys fs.FS, dir string) ([]ed25519.PublicKey, error) {
	entries, err := fs.ReadDir(fsys, dir)
	if err != nil {
		return nil, fmt.Errorf("release keys: %w", err)
	}
	var out []ed25519.PublicKey
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".pem") {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(dir, e.Name()))
		if err != nil {
			return nil, fmt.Errorf("release key %s: %w", e.Name(), err)
		}
		k, err := ParsePublicKey(data)
		if err != nil {
			return nil, fmt.Errorf("release key %s: %w", e.Name(), err)
		}
		out = append(out, k)
	}
	return out, nil
}

// ParsePublicKey parses an Ed25519 public key in PEM, as `openssl pkey -pubout` writes it.
func ParsePublicKey(data []byte) (ed25519.PublicKey, error) {
	block, _ := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" {
		return nil, errors.New("not a PEM public key")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	ek, ok := k.(ed25519.PublicKey)
	if !ok {
		return nil, fmt.Errorf("a %T, not an Ed25519 key", k)
	}
	return ek, nil
}

// ErrNoTrustedKeys means this parcon was built without a release key, so it can't verify any release.
var ErrNoTrustedKeys = errors.New("this parcon trusts no release signing key, so it can't verify a release")

// VerifySignature checks that sig is a signature of sums by one of keys.
func VerifySignature(sums, sig []byte, keys []ed25519.PublicKey) error {
	if len(keys) == 0 {
		return ErrNoTrustedKeys
	}
	for _, k := range keys {
		if ed25519.Verify(k, sums, sig) {
			return nil
		}
	}
	return errors.New("SHA256SUMS.sig doesn't verify with any key this parcon trusts: the release may have been " +
		"tampered with")
}

// Sums are a release's asset checksums, by file name.
type Sums map[string][32]byte

var sumLine = regexp.MustCompile(`^([0-9a-f]{64}) [ *]([A-Za-z0-9._-]+)$`)

// ParseSums parses sha256sum's output.
func ParseSums(data []byte) (Sums, error) {
	sums := Sums{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		if sc.Text() == "" {
			continue
		}
		m := sumLine.FindStringSubmatch(sc.Text())
		if m == nil {
			return nil, fmt.Errorf("SHA256SUMS: not a checksum line: %q", sc.Text())
		}
		var sum [32]byte
		if _, err := hex.Decode(sum[:], []byte(m[1])); err != nil {
			return nil, fmt.Errorf("SHA256SUMS: %w", err)
		}
		sums[m[2]] = sum
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("SHA256SUMS: %w", err)
	}
	if len(sums) == 0 {
		return nil, errors.New("SHA256SUMS is empty")
	}
	return sums, nil
}

// Hex returns an asset's checksum in hex.
func (s Sums) Hex(name string) (string, bool) {
	sum, ok := s[name]
	if !ok {
		return "", false
	}
	return hex.EncodeToString(sum[:]), true
}
