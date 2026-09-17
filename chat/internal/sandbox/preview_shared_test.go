package sandbox

import (
	"context"
	"crypto/tls"
	"io"
	"log"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/transport"
)

// With PreviewAddress set the runner serves every publication from one
// mutual-TLS server under /<publication ID> (docs/warden-kubernetes-plan.md,
// decisions 5 and 10): the attachment URL is that address, no loopback
// listener is bound, only the chat's certificate is admitted, and the
// availability, generation, Host and Origin checks of the loopback
// listeners apply unchanged.
func TestSharedPreviewServerOverMutualTLS(t *testing.T) {
	dir := t.TempDir()
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	material := func(ca *transport.CA, id string) *transport.TLS {
		m, err := ca.Material(filepath.Join(dir, ca.Certificate.Subject.CommonName, id), id, []string{"127.0.0.1"}, time.Now(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		return m
	}
	w, d, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	l, err := transport.Listen("tls://127.0.0.1:0", transport.ListenOptions{TLS: material(ca, transport.Runner), Peers: []string{transport.Chat}})
	if err != nil {
		t.Fatal(err)
	}
	host := l.Addr().String()
	w.PreviewListener, w.PreviewAddress = l, "tls://"+host
	server := w.previewServer()
	server.ErrorLog = log.New(io.Discard, "", 0) // the refused handshakes below
	go func() { _ = server.Serve(l) }()
	t.Cleanup(func() { server.Close() })

	a := attachFixture(t, w, r)
	w.mu.Lock()
	p := w.managed.Publications[pubKey(r.SandboxID, 3000)]
	w.mu.Unlock()
	if p == nil || p.listener != nil || p.server != nil || p.ProxyPort != 0 {
		t.Fatalf("shared mode bound a loopback listener: %+v", p)
	}
	if a.URL != "https://"+host+"/"+p.ID+"/" {
		t.Fatalf("attachment URL %q, want the shared address and the publication ID", a.URL)
	}
	if again := attachFixture(t, w, r); again.URL != a.URL {
		t.Fatal("retry changed the attachment URL", again.URL)
	}

	client := func(m *transport.TLS) *http.Client {
		var config *tls.Config
		if m != nil {
			if config, err = transport.ClientConfig(m, "127.0.0.1"); err != nil {
				t.Fatal(err)
			}
		}
		return &http.Client{Transport: &http.Transport{TLSClientConfig: config, DisableKeepAlives: true}, Timeout: 5 * time.Second}
	}
	chat := client(material(ca, transport.Chat))
	get := func(c *http.Client, url string, adjust func(*http.Request)) (*http.Response, string, error) {
		req, _ := http.NewRequestWithContext(context.Background(), "GET", url, nil)
		if adjust != nil {
			adjust(req)
		}
		res, err := c.Do(req)
		if err != nil {
			return nil, "", err
		}
		body, _ := io.ReadAll(res.Body)
		res.Body.Close()
		return res, string(body), nil
	}

	// The chat reaches the guest through the publication's path prefix;
	// the rest of the path and the query pass through, credentials do not.
	res, body, err := get(chat, a.URL+"assets/app.js?x=1", func(req *http.Request) {
		req.Header.Set("Authorization", "Bearer controller-secret")
		req.Header.Set("Cookie", "controller=secret")
		req.Header.Set("Origin", "https://"+host)
	})
	if err != nil || res.StatusCode != 200 || body != "counter" || res.Header.Get("Set-Cookie") != "" {
		t.Fatalf("%v %v %q", res, err, body)
	}
	d.mu.Lock()
	if d.requestURI != "/assets/app.js?x=1" || d.headers.Get("Authorization") != "" || d.headers.Get("Cookie") != "" {
		t.Fatalf("guest saw %q with %v", d.requestURI, d.headers)
	}
	d.mu.Unlock()
	if res, _, err = get(chat, "https://"+host+"/"+p.ID, nil); err != nil || res.StatusCode != 200 {
		t.Fatalf("bare publication path: %v %v", res, err)
	}
	d.mu.Lock()
	if d.requestURI != "/" {
		t.Fatalf("bare publication path reached the guest as %q", d.requestURI)
	}
	d.mu.Unlock()

	// Only a known publication ID routes; the root is nothing.
	for _, url := range []string{"https://" + host + "/", "https://" + host + "/" + strings.Repeat("0", 32) + "/", "https://" + host + "/" + p.ID + "x/"} {
		if res, _, err = get(chat, url, nil); err != nil || res.StatusCode != 404 {
			t.Fatalf("%s: %v %v", url, res, err)
		}
	}
	// Host and Origin must be the advertised address (what the chat
	// sends); the scheme of the origin is https.
	if res, _, err = get(chat, a.URL, func(req *http.Request) { req.Host = "evil.example:1" }); err != nil || res.StatusCode != 403 {
		t.Fatalf("foreign Host: %v %v", res, err)
	}
	for _, origin := range []string{"http://" + host, "https://evil.example"} {
		if res, _, err = get(chat, a.URL, func(req *http.Request) { req.Header.Set("Origin", origin) }); err != nil || res.StatusCode != 403 {
			t.Fatalf("origin %s: %v %v", origin, res, err)
		}
	}

	// Anything but the chat's certificate from the deployment CA is
	// refused at the handshake; plaintext never reaches the handler.
	other, err := transport.NewCA("other", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	for name, m := range map[string]*transport.TLS{"policy": material(ca, transport.Policy), "edge": material(ca, transport.Edge), "chat from another CA": material(other, transport.Chat)} {
		if _, _, err = get(client(m), a.URL, nil); err == nil {
			t.Fatalf("%s admitted", name)
		}
	}
	// (Go's TLS listener answers a plaintext request with 400 itself.)
	if res, body, err = get(&http.Client{Timeout: 5 * time.Second}, "http://"+host+"/"+p.ID+"/", nil); err == nil && (res.StatusCode != 400 || body == "counter") {
		t.Fatalf("plaintext admitted: %v %q", res, body)
	}
	if _, _, err = get(&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, Timeout: 5 * time.Second}, a.URL, nil); err == nil {
		t.Fatal("a client without a certificate admitted")
	}

	// The availability checks are the loopback listeners': a stopped
	// sandbox is 503, a removed publication 410.
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].State = "stopped"
	w.mu.Unlock()
	if res, _, err = get(chat, a.URL, nil); err != nil || res.StatusCode != 503 {
		t.Fatalf("stopped sandbox: %v %v", res, err)
	}
	w.mu.Lock()
	w.managed.Sandboxes[r.SandboxID].State = "running"
	w.mu.Unlock()
	remove := r
	remove.Operation = "preview.remove"
	remove.AttachmentID = a.ID
	if _, err = w.dispatch(context.Background(), remove); err != nil {
		t.Fatal(err)
	}
	if res, _, err = get(chat, a.URL, nil); err != nil || res.StatusCode != 410 {
		t.Fatalf("removed publication: %v %v", res, err)
	}
}

// Without PreviewAddress nothing changes for the sbx shapes: a loopback
// listener per publication and an http://127.0.0.1:<port>/ URL.
func TestLoopbackPreviewListenersWithoutSharedServer(t *testing.T) {
	w, _, _, r := managedFixture(t)
	prepareFixture(t, w, r)
	a := attachFixture(t, w, r)
	w.mu.Lock()
	p := w.managed.Publications[pubKey(r.SandboxID, 3000)]
	w.mu.Unlock()
	if p == nil || p.listener == nil || p.ProxyPort == 0 || a.URL != "http://127.0.0.1:"+fmtInt(p.ProxyPort)+"/" {
		t.Fatalf("%+v %q", p, a.URL)
	}
	res, err := http.Get(a.URL)
	if err != nil || res.StatusCode != 200 {
		t.Fatal(res, err)
	}
	res.Body.Close()
}
