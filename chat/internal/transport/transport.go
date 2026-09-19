// Package transport is how the Warden services reach each other. Every
// listener and every address a client dials is a URL: unix://<path> for the
// single-host shapes (Mac, OVH), where the socket's file mode is the
// authentication, and tls://<host>:<port> for Kubernetes, where mutual TLS
// from one deployment CA is, and the peer's certificate names it. The
// line-JSON protocols above the transport are the same on either scheme.
//
// A tls:// listener requires and verifies the client certificate against
// the CA and admits only the identities it was given; a tls:// dial
// verifies the server against the CA under the URL's host name. The
// identity of a certificate is its Common Name (or, without one, its first
// DNS name); the four services use the names below.
package transport

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// Identities the Warden services carry in their certificates.
const (
	Policy = "warden-policy"
	Runner = "warden-runner"
	Chat   = "warden-chat"
	Edge   = "warden-edge"
)

// URL schemes.
const (
	SchemeUnix = "unix"
	SchemeTLS  = "tls"
)

// TLS names the mutual-TLS material of one service: the deployment CA every
// peer is verified against and this service's own certificate and key, as
// PEM files. The certificate and key are re-read at every handshake, so a
// renewed certificate is picked up without a restart; the CA is read once.
type TLS struct {
	CAFile   string `json:"caFile"`
	CertFile string `json:"certFile"`
	KeyFile  string `json:"keyFile"`
	// ServerName, when set, replaces the dialed host as the name the server
	// certificate must carry. Dialing only.
	ServerName string `json:"serverName,omitempty"`
}

// ListenOptions configure Listen.
type ListenOptions struct {
	// Mode is applied to a Unix socket once it is bound; zero keeps what
	// the umask produced.
	Mode os.FileMode
	// TLS is the listener's material, required for tls:// and ignored
	// otherwise.
	TLS *TLS
	// Peers are the identities a tls:// listener admits. Any other client
	// certificate, and any client without one, is refused during the
	// handshake. Required for tls://.
	Peers []string
}

// DialOptions configure Dial.
type DialOptions struct {
	// TLS is the dialer's material, required for tls:// and ignored
	// otherwise.
	TLS *TLS
	// Timeout bounds the connection and, for tls://, the handshake; zero
	// leaves both to ctx.
	Timeout time.Duration
}

// Address is a parsed URL.
type Address struct {
	Scheme string // SchemeUnix or SchemeTLS
	Path   string // the socket path (unix)
	Host   string // host:port (tls)
}

// String is the URL form.
func (a Address) String() string {
	if a.Scheme == SchemeUnix {
		return "unix://" + a.Path
	}
	return a.Scheme + "://" + a.Host
}

// Parse reads unix://<absolute path> or tls://<host>:<port>. The path is
// taken verbatim (no percent-decoding), so socket paths with spaces work.
func Parse(rawurl string) (Address, error) {
	scheme, rest, ok := strings.Cut(rawurl, "://")
	if !ok {
		return Address{}, fmt.Errorf("%q is not a unix:// or tls:// URL", rawurl)
	}
	switch scheme {
	case SchemeUnix:
		if !strings.HasPrefix(rest, "/") {
			return Address{}, fmt.Errorf("%q: a unix:// URL needs an absolute socket path", rawurl)
		}
		return Address{Scheme: SchemeUnix, Path: rest}, nil
	case SchemeTLS:
		if strings.ContainsAny(rest, "/?#@") {
			return Address{}, fmt.Errorf("%q: a tls:// URL is host:port only", rawurl)
		}
		if _, port, err := net.SplitHostPort(rest); err != nil {
			return Address{}, fmt.Errorf("%q: %w", rawurl, err)
		} else if n, err := strconv.Atoi(port); err != nil || n < 0 || n > 65535 {
			// Port 0 is accepted so a listener can take an ephemeral port
			// (tests); a dial to it fails at connect.
			return Address{}, fmt.Errorf("%q: invalid port", rawurl)
		}
		return Address{Scheme: SchemeTLS, Host: rest}, nil
	}
	return Address{}, fmt.Errorf("%q: unsupported scheme %q (unix or tls)", rawurl, scheme)
}

// IsTLS reports whether rawurl is a tls:// URL.
func IsTLS(rawurl string) bool { return strings.HasPrefix(rawurl, "tls://") }

// Listen binds the URL. A Unix socket left behind by an earlier process is
// removed first (callers hold a lock on their state directory, so the stale
// file is theirs), then bound and given o.Mode. A tls:// listener accepts
// TLS 1.3 connections whose client certificate the CA signed and whose
// identity is one of o.Peers; the handshake happens on the first read or
// write of an accepted connection, so a server's read deadline bounds it.
func Listen(rawurl string, o ListenOptions) (net.Listener, error) {
	a, err := Parse(rawurl)
	if err != nil {
		return nil, err
	}
	switch a.Scheme {
	case SchemeUnix:
		_ = os.Remove(a.Path)
		l, err := net.Listen("unix", a.Path)
		if err != nil {
			return nil, err
		}
		if o.Mode != 0 {
			if err = os.Chmod(a.Path, o.Mode); err != nil {
				l.Close()
				return nil, err
			}
		}
		return l, nil
	default:
		config, err := ServerConfig(o.TLS, o.Peers)
		if err != nil {
			return nil, err
		}
		return tls.Listen("tcp", a.Host, config)
	}
}

// Dial connects to the URL. For tls:// the handshake completes before Dial
// returns, so an error names a refused certificate as well as an
// unreachable host.
func Dial(ctx context.Context, rawurl string, o DialOptions) (net.Conn, error) {
	a, err := Parse(rawurl)
	if err != nil {
		return nil, err
	}
	dialer := &net.Dialer{Timeout: o.Timeout}
	switch a.Scheme {
	case SchemeUnix:
		return dialer.DialContext(ctx, "unix", a.Path)
	default:
		host, _, _ := net.SplitHostPort(a.Host)
		config, err := ClientConfig(o.TLS, host)
		if err != nil {
			return nil, err
		}
		return (&tls.Dialer{NetDialer: dialer, Config: config}).DialContext(ctx, "tcp", a.Host)
	}
}

// ServerConfig is the TLS configuration of a listener that admits the
// given peer identities: TLS 1.3, a client certificate required and
// verified against the CA, the identity checked during the handshake.
func ServerConfig(t *TLS, peers []string) (*tls.Config, error) {
	if len(peers) == 0 {
		return nil, errors.New("a tls:// listener needs the peer identities it admits")
	}
	pool, err := material(t)
	if err != nil {
		return nil, err
	}
	admitted := map[string]bool{}
	for _, p := range peers {
		admitted[p] = true
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		ClientAuth: tls.RequireAndVerifyClientCert,
		ClientCAs:  pool,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return pair(t)
		},
		VerifyConnection: func(cs tls.ConnectionState) error {
			id := IdentityOf(cs)
			if !admitted[id] {
				return fmt.Errorf("peer %q is not admitted here", id)
			}
			return nil
		},
	}, nil
}

// ClientConfig is the TLS configuration of a dialer: TLS 1.3, the server
// verified against the CA under serverName (or t.ServerName when set), this
// service's certificate presented on request.
func ClientConfig(t *TLS, serverName string) (*tls.Config, error) {
	pool, err := material(t)
	if err != nil {
		return nil, err
	}
	if t.ServerName != "" {
		serverName = t.ServerName
	}
	if serverName == "" {
		return nil, errors.New("tls:// dial needs a server name")
	}
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: serverName,
		GetClientCertificate: func(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
			return pair(t)
		},
	}, nil
}

// material loads the CA and checks the certificate pair once, so a
// misconfiguration is an error at start rather than at the first peer.
func material(t *TLS) (*x509.CertPool, error) {
	if t == nil || t.CAFile == "" || t.CertFile == "" || t.KeyFile == "" {
		return nil, errors.New("tls:// needs the TLS material (tls.caFile, tls.certFile and tls.keyFile)")
	}
	data, err := os.ReadFile(t.CAFile)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(data) {
		return nil, fmt.Errorf("%s: no CA certificate found", t.CAFile)
	}
	if _, err = pair(t); err != nil {
		return nil, err
	}
	return pool, nil
}

func pair(t *TLS) (*tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(t.CertFile, t.KeyFile)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// Identity names the service a certificate was issued to: its Common Name,
// or without one its first DNS name; "" for neither.
func Identity(c *x509.Certificate) string {
	if c == nil {
		return ""
	}
	if c.Subject.CommonName != "" {
		return c.Subject.CommonName
	}
	if len(c.DNSNames) > 0 {
		return c.DNSNames[0]
	}
	return ""
}

// IdentityOf is the identity of the peer certificate of a connection
// state, "" when the peer presented none (an http.Request's TLS field, for
// example).
func IdentityOf(cs tls.ConnectionState) string {
	if len(cs.PeerCertificates) == 0 {
		return ""
	}
	return Identity(cs.PeerCertificates[0])
}

// PeerIdentity is the identity the other side of a connection proved with
// its certificate: the client's on an accepted connection, the server's on
// a dialed one. It is "" on a Unix socket and after a failed handshake. On
// an accepted connection whose handshake has not run yet it runs it, so a
// server sets its deadline first.
func PeerIdentity(conn net.Conn) string {
	tc, ok := conn.(*tls.Conn)
	if !ok {
		return ""
	}
	if !tc.ConnectionState().HandshakeComplete {
		if err := tc.Handshake(); err != nil {
			return ""
		}
	}
	return IdentityOf(tc.ConnectionState())
}

// ProbeQuietLog is an http.Server ErrorLog that drops the handshake errors
// a TCP liveness or readiness probe causes on a TLS listener (the kubelet
// connects and closes: "TLS handshake error ... EOF" every few seconds)
// and passes everything else to the standard logger.
func ProbeQuietLog() *log.Logger {
	return log.New(probeFilter{}, "", 0)
}

type probeFilter struct{}

func (probeFilter) Write(p []byte) (int, error) {
	line := string(p)
	if strings.Contains(line, "TLS handshake error") && (strings.HasSuffix(strings.TrimSpace(line), "EOF") || strings.Contains(line, "connection reset by peer")) {
		return len(p), nil
	}
	log.Print(strings.TrimSuffix(line, "\n"))
	return len(p), nil
}
