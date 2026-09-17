package transport

import (
	"bufio"
	"context"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// echo answers every accepted connection with one line: the peer identity
// it saw, then the line it received.
func echo(t *testing.T, l net.Listener) {
	t.Helper()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				_ = c.SetDeadline(time.Now().Add(5 * time.Second))
				id := PeerIdentity(c)
				line, err := bufio.NewReader(c).ReadString('\n')
				if err != nil {
					return
				}
				_, _ = io.WriteString(c, id+" "+line)
			}()
		}
	}()
}

func exchange(t *testing.T, ctx context.Context, url string, o DialOptions, line string) (string, error) {
	t.Helper()
	conn, err := Dial(ctx, url, o)
	if err != nil {
		return "", err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
	if _, err = io.WriteString(conn, line+"\n"); err != nil {
		return "", err
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(reply, "\n"), nil
}

func TestParse(t *testing.T) {
	a, err := Parse("unix:///tmp/w/policy/sbx-control.sock")
	if err != nil || a.Scheme != SchemeUnix || a.Path != "/tmp/w/policy/sbx-control.sock" || a.String() != "unix:///tmp/w/policy/sbx-control.sock" {
		t.Fatalf("%+v %v", a, err)
	}
	a, err = Parse("unix:///Users/o/Library/Application Support/w.sock")
	if err != nil || a.Path != "/Users/o/Library/Application Support/w.sock" {
		t.Fatalf("spaces must survive verbatim: %+v %v", a, err)
	}
	a, err = Parse("tls://warden-policy:7443")
	if err != nil || a.Scheme != SchemeTLS || a.Host != "warden-policy:7443" || a.String() != "tls://warden-policy:7443" || !IsTLS("tls://x:1") || IsTLS("unix:///x") {
		t.Fatalf("%+v %v", a, err)
	}
	if a, err = Parse("tls://:7443"); err != nil || a.Host != ":7443" {
		t.Fatalf("all interfaces: %+v %v", a, err)
	}
	for _, bad := range []string{"", "/tmp/x.sock", "unix://relative.sock", "unix:relative", "tls://host", "tls://host:-1", "tls://host:70000", "tls://host:1/path", "tls://u@host:1", "http://127.0.0.1:1", "ftp://x:1"} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestUnixListenAndDial(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "wtr")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "w.sock")
	// A stale socket file from an earlier process is replaced.
	os.WriteFile(path, []byte("stale"), 0o600)
	url := "unix://" + path
	l, err := Listen(url, ListenOptions{Mode: 0o660})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o660 {
		t.Fatalf("socket mode: %v %v", info, err)
	}
	echo(t, l)
	reply, err := exchange(t, context.Background(), url, DialOptions{}, "hi")
	if err != nil || reply != " hi" {
		t.Fatalf("%q %v", reply, err)
	}
	if id := PeerIdentity(mustDial(t, url, DialOptions{})); id != "" {
		t.Fatal("unix sockets carry no identity:", id)
	}
	l.Close()
	if _, err = Dial(context.Background(), url, DialOptions{}); err == nil {
		t.Fatal("closed listener accepted a dial")
	}
	if _, err = Listen("unix://"+filepath.Join(dir, "missing", "w.sock"), ListenOptions{}); err == nil {
		t.Fatal("missing directory accepted")
	}
}

func mustDial(t *testing.T, url string, o DialOptions) net.Conn {
	t.Helper()
	conn, err := Dial(context.Background(), url, o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn
}

func issue(t *testing.T, ca *CA, dir, identity string) *TLS {
	t.Helper()
	m, err := ca.Material(filepath.Join(dir, identity), identity, []string{"127.0.0.1"}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestMutualTLSAdmitsOnlyNamedPeersFromTheCA(t *testing.T) {
	dir := t.TempDir()
	ca, err := NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	policy := issue(t, ca, dir, Policy)
	runner := issue(t, ca, dir, Runner)
	edge := issue(t, ca, dir, Edge)
	l, err := Listen("tls://127.0.0.1:0", ListenOptions{TLS: policy, Peers: []string{Runner, Chat}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	echo(t, l)
	url := "tls://" + l.Addr().String()
	reply, err := exchange(t, context.Background(), url, DialOptions{TLS: runner}, "hi")
	if err != nil || reply != Runner+" hi" {
		t.Fatalf("%q %v", reply, err)
	}
	if id := PeerIdentity(mustDial(t, url, DialOptions{TLS: runner})); id != Policy {
		t.Fatal("dialed server identity:", id)
	}
	// The edge holds a certificate from the same CA but is not admitted by
	// the policy service.
	if _, err = exchange(t, context.Background(), url, DialOptions{TLS: edge}, "hi"); err == nil {
		t.Fatal("unadmitted identity accepted")
	}
	// A certificate from another CA is refused by the server, and that
	// server is refused by a client trusting only the other CA.
	other, err := NewCA("other", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	impostor := issue(t, other, filepath.Join(dir, "other"), Runner)
	if _, err = exchange(t, context.Background(), url, DialOptions{TLS: impostor}, "hi"); err == nil {
		t.Fatal("certificate from another CA accepted")
	}
	// The server name must match: dialing by a name the certificate does
	// not carry fails even with the right CA.
	wrongName := *runner
	wrongName.ServerName = "warden-chat"
	if _, err = exchange(t, context.Background(), url, DialOptions{TLS: &wrongName}, "hi"); err == nil {
		t.Fatal("wrong server name accepted")
	}
	rightName := *runner
	rightName.ServerName = Policy
	if reply, err = exchange(t, context.Background(), url, DialOptions{TLS: &rightName}, "hi"); err != nil || reply != Runner+" hi" {
		t.Fatalf("server name override: %q %v", reply, err)
	}
	// No material at all is a configuration error, not a plaintext dial.
	if _, err = Dial(context.Background(), url, DialOptions{}); err == nil || !strings.Contains(err.Error(), "TLS material") {
		t.Fatal("plaintext dial to a tls:// URL accepted:", err)
	}
	// A plaintext client reaching the port is not answered.
	raw, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Close()
	_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
	io.WriteString(raw, "not a TLS record at all\n")
	if line, err := bufio.NewReader(raw).ReadString('\n'); err == nil {
		t.Fatalf("plaintext client answered: %q", line)
	}
	if _, err = Listen("tls://127.0.0.1:0", ListenOptions{TLS: policy}); err == nil {
		t.Fatal("tls:// listener without peers accepted")
	}
	if _, err = Listen("tls://127.0.0.1:0", ListenOptions{Peers: []string{Runner}}); err == nil {
		t.Fatal("tls:// listener without material accepted")
	}
}

func TestDialHonoursContext(t *testing.T) {
	dir := t.TempDir()
	ca, err := NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	server := issue(t, ca, dir, Chat)
	client := issue(t, ca, dir, Edge)
	// A listener that never completes a handshake: the raw TCP listener
	// accepts nothing, so the dial blocks in the connection or handshake.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	started := time.Now()
	_, err = Dial(ctx, "tls://"+l.Addr().String(), DialOptions{TLS: client})
	if err == nil || !errors.Is(err, context.DeadlineExceeded) && !strings.Contains(err.Error(), "deadline") {
		t.Fatal("dial did not stop with the context:", err)
	}
	if time.Since(started) > 5*time.Second {
		t.Fatal("dial ignored the context")
	}
	// DialOptions.Timeout bounds the handshake too.
	started = time.Now()
	_, err = Dial(context.Background(), "tls://"+l.Addr().String(), DialOptions{TLS: client, Timeout: 200 * time.Millisecond})
	if err == nil || time.Since(started) > 5*time.Second {
		t.Fatal("timeout ignored:", err)
	}
	_ = server
}

func TestIdentity(t *testing.T) {
	ca, err := NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	certPEM, _, err := ca.Issue(Edge, []string{"warden-edge.warden.svc", "10.0.0.5"}, time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := parsePEM(t, certPEM)
	if Identity(c) != Edge || len(c.DNSNames) != 2 || c.DNSNames[0] != Edge || len(c.IPAddresses) != 1 {
		t.Fatalf("%q %v %v", Identity(c), c.DNSNames, c.IPAddresses)
	}
	c.Subject.CommonName = ""
	if Identity(c) != Edge {
		t.Fatal("first DNS name is the fallback identity")
	}
	c.DNSNames = nil
	if Identity(c) != "" || Identity(nil) != "" {
		t.Fatal("no name yields no identity")
	}
}

func TestBootstrapWritesOneDirectoryPerIdentity(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "tls")
	if _, err := Bootstrap(dir, []string{Policy, Runner}, []string{"127.0.0.1"}, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	for _, f := range []string{"ca.crt", "ca.key", "warden-policy/ca.crt", "warden-policy/tls.crt", "warden-policy/tls.key", "warden-runner/tls.key"} {
		info, err := os.Stat(filepath.Join(dir, f))
		if err != nil {
			t.Fatal(err)
		}
		if strings.HasSuffix(f, ".key") && info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode %v", f, info.Mode())
		}
	}
	ca, err := os.ReadFile(filepath.Join(dir, "ca.crt"))
	if err != nil {
		t.Fatal(err)
	}
	if copy, _ := os.ReadFile(filepath.Join(dir, "warden-policy", "ca.crt")); string(copy) != string(ca) {
		t.Fatal("service directory holds a different CA")
	}
	// The material works end to end.
	policy := &TLS{CAFile: filepath.Join(dir, "warden-policy", "ca.crt"), CertFile: filepath.Join(dir, "warden-policy", "tls.crt"), KeyFile: filepath.Join(dir, "warden-policy", "tls.key")}
	runner := &TLS{CAFile: filepath.Join(dir, "warden-runner", "ca.crt"), CertFile: filepath.Join(dir, "warden-runner", "tls.crt"), KeyFile: filepath.Join(dir, "warden-runner", "tls.key")}
	l, err := Listen("tls://127.0.0.1:0", ListenOptions{TLS: policy, Peers: []string{Runner}})
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	echo(t, l)
	if reply, err := exchange(t, context.Background(), "tls://"+l.Addr().String(), DialOptions{TLS: runner}, "hi"); err != nil || reply != Runner+" hi" {
		t.Fatalf("%q %v", reply, err)
	}
	if _, err = Bootstrap(dir, []string{Policy}, nil, time.Now(), time.Hour); err == nil {
		t.Fatal("existing CA overwritten")
	}
	if _, err = Bootstrap(filepath.Join(t.TempDir(), "x"), nil, nil, time.Now(), time.Hour); err == nil {
		t.Fatal("no identities accepted")
	}
}

func parsePEM(t *testing.T, data []byte) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("no PEM block")
	}
	c, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return c
}
