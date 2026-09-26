package hostsys

import (
	"crypto/sha256"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// TLSMode is how the controller verifies the certificate the Proxmox API serves. Each keeps working when the
// certificate is renewed, except TLSPin.
type TLSMode string

const (
	// TLSNodeCA is the node's own certificate, verified against the node's CA (pve-root-ca.pem), which renewals
	// keep.
	TLSNodeCA TLSMode = "ca"
	// TLSSystem is a custom or ACME certificate that the system CAs trust.
	TLSSystem TLSMode = "system"
	// TLSPin is a certificate from a CA the system doesn't trust: its fingerprint is pinned.
	TLSPin TLSMode = "pin"
)

// TLSInfo says how to verify the API's certificate.
type TLSInfo struct {
	Mode TLSMode
	// ServerName is a DNS name the certificate is issued for, other than localhost. The controller connects by IP
	// address and checks the certificate against it. Empty for TLSPin.
	ServerName string
	// Fingerprint is the certificate's SHA-256 fingerprint as AA:BB:..., for TLSPin.
	Fingerprint string
	// CA is the node's CA certificate in PEM, for TLSNodeCA.
	CA []byte
}

// TLS inspects the certificate the API serves: a custom or ACME certificate if one is installed
// (local/pveproxy-ssl.pem), otherwise the node's own (local/pve-ssl.pem). roots stands in for the system CAs; nil
// means the real ones.
func (s System) TLS(roots *x509.CertPool) (TLSInfo, error) {
	custom := filepath.Join(s.Paths.PVEDir, "local", "pveproxy-ssl.pem")
	path, mode := custom, TLSSystem
	if _, err := os.Stat(custom); errors.Is(err, os.ErrNotExist) {
		path, mode = filepath.Join(s.Paths.PVEDir, "local", "pve-ssl.pem"), TLSNodeCA
	}
	chain, err := readCerts(path)
	if err != nil {
		return TLSInfo{}, err
	}
	leaf := chain[0]

	info := TLSInfo{Mode: mode}
	if mode == TLSNodeCA {
		ca, err := os.ReadFile(filepath.Join(s.Paths.PVEDir, "pve-root-ca.pem"))
		if err != nil {
			return TLSInfo{}, fmt.Errorf("read the node's CA: %w", err)
		}
		info.CA = ca
	} else {
		if roots == nil {
			if roots, err = x509.SystemCertPool(); err != nil {
				return TLSInfo{}, fmt.Errorf("load the system CAs: %w", err)
			}
		}
		intermediates := x509.NewCertPool()
		for _, c := range chain[1:] {
			intermediates.AddCert(c)
		}
		if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, Intermediates: intermediates}); err != nil {
			sum := sha256.Sum256(leaf.Raw)
			hexes := make([]string, len(sum))
			for i, b := range sum {
				hexes[i] = fmt.Sprintf("%02X", b)
			}
			return TLSInfo{Mode: TLSPin, Fingerprint: strings.Join(hexes, ":")}, nil
		}
	}
	for _, name := range leaf.DNSNames {
		if name != "localhost" {
			info.ServerName = name
			break
		}
	}
	return info, nil
}

func readCerts(path string) ([]*x509.Certificate, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: the node's certificate.
	if err != nil {
		return nil, fmt.Errorf("read the API's certificate: %w", err)
	}
	var certs []*x509.Certificate
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			continue
		}
		c, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		certs = append(certs, c)
	}
	if len(certs) == 0 {
		return nil, fmt.Errorf("%s holds no certificate", path)
	}
	return certs, nil
}
