package transport

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

// CA is one deployment's certificate authority: what `warden tls bootstrap`
// writes for a cluster without cert-manager, and what tests issue their
// peers from. Keys are ECDSA P-256.
type CA struct {
	Certificate *x509.Certificate
	key         *ecdsa.PrivateKey
	certPEM     []byte
}

// NewCA creates a CA valid from now for the given duration.
func NewCA(name string, now time.Time, validity time.Duration) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: name},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validity),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Certificate: cert, key: key, certPEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}, nil
}

// CertPEM is the CA certificate, what every peer verifies against.
func (ca *CA) CertPEM() []byte { return ca.certPEM }

// KeyPEM is the CA private key.
func (ca *CA) KeyPEM() ([]byte, error) {
	der, err := x509.MarshalECPrivateKey(ca.key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der}), nil
}

// Issue signs a certificate for one identity, usable as both a server and a
// client certificate: the identity is the Common Name and the first DNS
// name, and every extra name is added as a DNS name or, when it parses as
// one, an IP address.
func (ca *CA) Issue(identity string, extra []string, now time.Time, validity time.Duration) (certPEM, keyPEM []byte, err error) {
	if identity == "" {
		return nil, nil, errors.New("an identity is required")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: identity},
		DNSNames:     []string{identity},
		NotBefore:    now.Add(-5 * time.Minute),
		NotAfter:     now.Add(validity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
	}
	for _, name := range extra {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
		} else if name != "" && name != identity {
			template.DNSNames = append(template.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.Certificate, &key.PublicKey, ca.key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), nil
}

// Material issues a certificate for identity and writes ca.crt, tls.crt and
// tls.key (the keys of a Kubernetes TLS Secret) into dir, which is created
// owner-only. It returns the TLS naming them.
func (ca *CA) Material(dir, identity string, extra []string, now time.Time, validity time.Duration) (*TLS, error) {
	certPEM, keyPEM, err := ca.Issue(identity, extra, now, validity)
	if err != nil {
		return nil, err
	}
	if err = os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	t := &TLS{CAFile: filepath.Join(dir, "ca.crt"), CertFile: filepath.Join(dir, "tls.crt"), KeyFile: filepath.Join(dir, "tls.key")}
	for _, f := range []struct {
		path string
		data []byte
		mode os.FileMode
	}{{t.CAFile, ca.certPEM, 0o644}, {t.CertFile, certPEM, 0o644}, {t.KeyFile, keyPEM, 0o600}} {
		if err = os.WriteFile(f.path, f.data, f.mode); err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Bootstrap writes a new CA (ca.crt, ca.key) into dir and one directory per
// identity holding that service's ca.crt, tls.crt and tls.key; extra names
// are added to every certificate. It refuses to overwrite an existing CA.
func Bootstrap(dir string, identities, extra []string, now time.Time, validity time.Duration) (*CA, error) {
	if len(identities) == 0 {
		return nil, errors.New("at least one identity is required")
	}
	if _, err := os.Stat(filepath.Join(dir, "ca.key")); err == nil {
		return nil, fmt.Errorf("%s already holds a CA; remove it first to re-issue", dir)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	ca, err := NewCA("warden", now, 10*validity)
	if err != nil {
		return nil, err
	}
	keyPEM, err := ca.KeyPEM()
	if err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "ca.key"), keyPEM, 0o600); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, "ca.crt"), ca.certPEM, 0o644); err != nil {
		return nil, err
	}
	for _, id := range identities {
		if _, err = ca.Material(filepath.Join(dir, id), id, extra, now, validity); err != nil {
			return nil, err
		}
	}
	return ca, nil
}
