package policy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// GatewayCA signs per-host leaf certificates for inspected TLS. It reuses a
// mitmproxy CA left by the previous gateway implementation so guests that
// already trust it keep working; otherwise it generates a Warden CA under
// the same file names.
type GatewayCA struct {
	cert   *x509.Certificate
	key    any
	mu     sync.Mutex
	leaves map[string]*tls.Certificate
	// CertPEM is the public certificate handed to guests.
	CertPEM []byte
}

const caCertFile = "mitmproxy-ca-cert.pem"
const caKeyFile = "mitmproxy-ca.pem"

// HostCADir holds the one gateway CA shared by every gateway of a policy
// installation, under the state directory. Guests trust this CA once; each
// gateway still mints its own per-host leaf certificates from it.
const HostCADir = "gateway-ca"

// DefaultGatewayCAMaxAge is how old the host CA may be before the service
// rotates it at startup.
const DefaultGatewayCAMaxAge = 365 * 24 * time.Hour

// Derive returns a CA sharing this CA's key and certificate with its own leaf
// cache, so one gateway's minted leaves never serve another.
func (ca *GatewayCA) Derive() *GatewayCA {
	return &GatewayCA{cert: ca.cert, key: ca.key, leaves: map[string]*tls.Certificate{}, CertPEM: ca.CertPEM}
}

// Age is how long the CA certificate has existed.
func (ca *GatewayCA) Age(now time.Time) time.Duration { return now.Sub(ca.cert.NotBefore) }

// Fingerprint is the SHA-256 of the DER certificate, as recorded by guests.
func (ca *GatewayCA) Fingerprint() string { return fmt.Sprintf("%x", sha256.Sum256(ca.cert.Raw)) }

// PrepareGatewayCA loads or creates the host CA under state and rotates it
// when it is older than maxAge (0 disables rotation). It reports whether a
// rotation happened.
func PrepareGatewayCA(state string, maxAge time.Duration, now time.Time) (*GatewayCA, bool, error) {
	dir := filepath.Join(state, HostCADir)
	ca, err := LoadOrCreateGatewayCA(dir)
	if err != nil {
		return nil, false, err
	}
	if maxAge <= 0 || ca.Age(now) <= maxAge {
		return ca, false, nil
	}
	ca, err = RotateGatewayCA(state, now)
	return ca, err == nil, err
}

// RotateGatewayCA replaces the host CA: a new CA is generated beside the old
// one, the old directory is retired (kept, never deleted) and the new one is
// moved into place. Guests reinstall the CA on their next run because its
// fingerprint changes; the policy service must not be running.
func RotateGatewayCA(state string, now time.Time) (*GatewayCA, error) {
	dir := filepath.Join(state, HostCADir)
	next := dir + ".next"
	if err := os.RemoveAll(next); err != nil {
		return nil, err
	}
	ca, err := LoadOrCreateGatewayCA(next)
	if err != nil {
		return nil, err
	}
	if _, err = os.Stat(dir); err == nil {
		if err = os.Rename(dir, dir+".retired-"+now.UTC().Format("20060102T150405Z")); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err = os.Rename(next, dir); err != nil {
		return nil, err
	}
	return ca, nil
}

// LoadOrCreateGatewayCA prepares the CA inside dir (created 0700).
func LoadOrCreateGatewayCA(dir string) (*GatewayCA, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return nil, err
	}
	if raw, err := os.ReadFile(filepath.Join(dir, caKeyFile)); err == nil {
		ca, err := parseCA(raw)
		if err == nil {
			ca.CertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.cert.Raw})
			if _, statErr := os.Stat(filepath.Join(dir, caCertFile)); statErr != nil {
				if err = os.WriteFile(filepath.Join(dir, caCertFile), ca.CertPEM, 0o600); err != nil {
					return nil, err
				}
			}
			return ca, nil
		}
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "Warden Gateway CA", Organization: []string{"Warden"}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.AddDate(10, 0, 0),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	if err = os.WriteFile(filepath.Join(dir, caKeyFile), append(keyPEM, certPEM...), 0o600); err != nil {
		return nil, err
	}
	if err = os.WriteFile(filepath.Join(dir, caCertFile), certPEM, 0o600); err != nil {
		return nil, err
	}
	return &GatewayCA{cert: cert, key: key, leaves: map[string]*tls.Certificate{}, CertPEM: certPEM}, nil
}

func parseCA(raw []byte) (*GatewayCA, error) {
	var key any
	var cert *x509.Certificate
	for {
		var block *pem.Block
		block, raw = pem.Decode(raw)
		if block == nil {
			break
		}
		switch block.Type {
		case "CERTIFICATE":
			if cert == nil {
				parsed, err := x509.ParseCertificate(block.Bytes)
				if err != nil {
					return nil, err
				}
				cert = parsed
			}
		case "PRIVATE KEY":
			parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			key = parsed
		case "RSA PRIVATE KEY":
			parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			key = parsed
		case "EC PRIVATE KEY":
			parsed, err := x509.ParseECPrivateKey(block.Bytes)
			if err != nil {
				return nil, err
			}
			key = parsed
		}
	}
	if key == nil || cert == nil || !cert.IsCA {
		return nil, errors.New("incomplete gateway CA")
	}
	if time.Now().After(cert.NotAfter) {
		return nil, errors.New("expired gateway CA")
	}
	return &GatewayCA{cert: cert, key: key, leaves: map[string]*tls.Certificate{}}, nil
}

// Certificate returns (and caches) a leaf certificate for a server name.
func (ca *GatewayCA) Certificate(host string) (*tls.Certificate, error) {
	host = strings.ToLower(strings.TrimRight(host, "."))
	if !ValidHost(host) {
		return nil, errors.New("invalid certificate host")
	}
	ca.mu.Lock()
	defer ca.mu.Unlock()
	if leaf, ok := ca.leaves[host]; ok && time.Now().Before(leaf.Leaf.NotAfter.Add(-24*time.Hour)) {
		return leaf, nil
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, err
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.AddDate(0, 0, 397),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{host},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca.cert, &key.PublicKey, ca.key)
	if err != nil {
		return nil, err
	}
	leafCert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	leaf := &tls.Certificate{Certificate: [][]byte{der, ca.cert.Raw}, PrivateKey: key, Leaf: leafCert}
	if len(ca.leaves) > 256 {
		ca.leaves = map[string]*tls.Certificate{}
	}
	ca.leaves[host] = leaf
	return leaf, nil
}

// Pool returns a cert pool trusting this CA (tests use it as a client).
func (ca *GatewayCA) Pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	return pool
}
