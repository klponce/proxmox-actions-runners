package hostsys

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"
)

type testCert struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

// newCert makes a CA certificate when issuer is nil, and otherwise a certificate for localhost, pve1,
// pve1.example.com, and 192.0.2.5 that issuer signs.
func newCert(t *testing.T, cn string, issuer *testCert) testCert {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
	}
	parent, signer := tmpl, key
	if issuer == nil {
		tmpl.IsCA, tmpl.BasicConstraintsValid, tmpl.KeyUsage = true, true, x509.KeyUsageCertSign
	} else {
		tmpl.DNSNames = []string{"localhost", "pve1", "pve1.example.com"}
		parent, signer = issuer.cert, issuer.key
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, parent, &key.PublicKey, signer)
	must(t, err)
	cert, err := x509.ParseCertificate(der)
	must(t, err)
	return testCert{cert, key, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func nodeCerts(t *testing.T) (System, testCert) {
	p := testPaths(t)
	must(t, os.MkdirAll(filepath.Join(p.PVEDir, "local"), 0o755))
	ca := newCert(t, "Proxmox Virtual Environment", nil)
	node := newCert(t, "pve1", &ca)
	must(t, os.WriteFile(filepath.Join(p.PVEDir, "pve-root-ca.pem"), ca.pem, 0o644))
	must(t, os.WriteFile(filepath.Join(p.PVEDir, "local", "pve-ssl.pem"), node.pem, 0o644))
	return System{Paths: p}, ca
}

func TestTLSNodeCA(t *testing.T) {
	s, ca := nodeCerts(t)
	info, err := s.TLS(x509.NewCertPool())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != TLSNodeCA || info.ServerName != "pve1" || string(info.CA) != string(ca.pem) {
		t.Errorf("TLS = %+v", info)
	}
}

func TestTLSSystemTrusted(t *testing.T) {
	s, _ := nodeCerts(t)
	public := newCert(t, "Some Public CA", nil)
	acme := newCert(t, "pve1", &public)
	must(t, os.WriteFile(filepath.Join(s.Paths.PVEDir, "local", "pveproxy-ssl.pem"), acme.pem, 0o644))
	roots := x509.NewCertPool()
	roots.AddCert(public.cert)
	info, err := s.TLS(roots)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != TLSSystem || info.ServerName != "pve1" || info.CA != nil || info.Fingerprint != "" {
		t.Errorf("TLS = %+v", info)
	}
}

func TestTLSPinned(t *testing.T) {
	s, _ := nodeCerts(t)
	private := newCert(t, "Private CA", nil)
	custom := newCert(t, "pve1", &private)
	must(t, os.WriteFile(filepath.Join(s.Paths.PVEDir, "local", "pveproxy-ssl.pem"), custom.pem, 0o644))
	info, err := s.TLS(x509.NewCertPool())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode != TLSPin || !regexp.MustCompile(`^([0-9A-F]{2}:){31}[0-9A-F]{2}$`).MatchString(info.Fingerprint) {
		t.Errorf("TLS = %+v", info)
	}
}
