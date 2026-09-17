package policy

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// upstreamRequest is one request observed by the fake provider/GitHub server.
type upstreamRequest struct {
	method, host, path string
	headers            http.Header
	body               []byte
}

type gatewayFixture struct {
	t            *testing.T
	dir          string
	clock        *testClock
	verifier     *fixtureVerifier
	registry     *Registry
	value        map[string]any
	gateway      *Gateway
	port         int
	ca           *GatewayCA
	upstreamCA   *GatewayCA
	upstreamAddr string
	upstream     *http.Server
	handler      atomic.Value // func(http.ResponseWriter, *http.Request)
	resolveCalls atomic.Int32
	mu           sync.Mutex
	requests     []upstreamRequest
	dialTargets  []string
}

func newGatewayFixture(t *testing.T) *gatewayFixture {
	f := &gatewayFixture{t: t, dir: t.TempDir(), clock: &testClock{now: 1000, mono: 1000}, verifier: &fixtureVerifier{enabled: true}}
	f.registry = newTestRegistry(t, filepath.Join(f.dir, "state"), f.clock, f.verifier)
	f.verifier.registry = f.registry
	t.Cleanup(func() { f.registry.Close() })
	f.value = runContext(nil)
	if _, err := f.registry.Register(f.value); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.port = listener.Addr().(*net.TCPAddr).Port
	if _, err = f.registry.BindGateway(f.value, f.port); err != nil {
		t.Fatal(err)
	}
	if _, err = f.registry.ConfigureProvider(f.value, "synthetic-provider-fixture-secret"); err != nil {
		t.Fatal(err)
	}
	if r, err := f.registry.Begin(f.value, false); err != nil || !ready(r) {
		t.Fatalf("begin: %v %v", r, err)
	}
	f.ca = f.registry.CA.Derive()
	f.upstreamCA, err = LoadOrCreateGatewayCA(filepath.Join(f.dir, "upstream-ca"))
	if err != nil {
		t.Fatal(err)
	}
	f.handler.Store(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte("{}"))
	})
	upstreamListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.upstreamAddr = upstreamListener.Addr().String()
	tlsListener := tls.NewListener(upstreamListener, &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return f.upstreamCA.Certificate(hello.ServerName)
	}, NextProtos: []string{"http/1.1"}})
	f.upstream = &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		f.mu.Lock()
		f.requests = append(f.requests, upstreamRequest{r.Method, r.Host, r.URL.RequestURI(), r.Header.Clone(), body})
		f.mu.Unlock()
		f.handler.Load().(func(http.ResponseWriter, *http.Request))(w, r)
	})}
	go f.upstream.Serve(tlsListener)
	t.Cleanup(func() { f.upstream.Close() })
	binding := f.registry.Bindings["s1"]
	capability := binding.Capability
	gateway, err := NewGateway(GatewayConfig{BindingID: "s1", Capability: capability, Port: f.port, Listener: listener, CA: f.ca,
		Control: func(message map[string]any) (map[string]any, error) {
			return f.registry.Proxy("s1", capability, message)
		},
		Resolve: func(ctx context.Context, host string) ([]net.IP, error) {
			f.resolveCalls.Add(1)
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		},
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			f.mu.Lock()
			f.dialTargets = append(f.dialTargets, address)
			f.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, "tcp", f.upstreamAddr)
		},
		Roots: f.upstreamCA.Pool()})
	if err != nil {
		t.Fatal(err)
	}
	f.gateway = gateway
	gateway.Start()
	t.Cleanup(gateway.Stop)
	return f
}

func identityOfContext(value map[string]any) map[string]string {
	ctx, _ := ValidateContext(value)
	return ctx
}

func (f *gatewayFixture) client() *http.Client {
	proxyURL, _ := url.Parse("http://127.0.0.1:" + itoa(f.port))
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: f.ca.Pool()}, DisableKeepAlives: true},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (f *gatewayFixture) do(method, target string, headers map[string]string, body string) (*http.Response, []byte, error) {
	f.t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := f.client().Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer res.Body.Close()
	payload, readErr := io.ReadAll(res.Body)
	return res, payload, readErr
}

func (f *gatewayFixture) providerURL() string {
	return "http://host.docker.internal:" + itoa(f.port) + "/openai/v1/responses"
}

func (f *gatewayFixture) upstreamRequests() []upstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamRequest{}, f.requests...)
}

func (f *gatewayFixture) setHandler(h func(http.ResponseWriter, *http.Request)) { f.handler.Store(h) }

func (f *gatewayFixture) auditContains(needle string) bool {
	data, _ := os.ReadFile(filepath.Join(f.registry.Bindings["s1"].Engine.State, "audit", "events.jsonl"))
	return strings.Contains(string(data), needle)
}

var providerHeaders = map[string]string{"Content-Type": "application/json", "Authorization": "Bearer hostile-placeholder"}

func TestGatewayReverseProviderRouteInjectsOnlyAfterScopedAuthorization(t *testing.T) {
	f := newGatewayFixture(t)
	headers := map[string]string{"Content-Type": "application/json", "Authorization": "Bearer hostile-placeholder", "Cookie": "secret=cookie", "OpenAI-Project": "attacker"}
	res, _, err := f.do("POST", f.providerURL(), headers, `{"input":"synthetic"}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("response: %v %v", res, err)
	}
	reqs := f.upstreamRequests()
	if len(reqs) != 1 || reqs[0].host != "api.openai.com" || reqs[0].path != "/v1/responses" {
		t.Fatalf("upstream: %+v", reqs)
	}
	r := reqs[0]
	if r.headers.Get("Authorization") != "Bearer synthetic-provider-fixture-secret" || r.headers.Get("Cookie") != "" || r.headers.Get("OpenAI-Project") != "" || r.headers.Get("Accept-Encoding") != "identity" {
		t.Fatalf("headers: %v", r.headers)
	}
	f.mu.Lock()
	target := f.dialTargets[0]
	f.mu.Unlock()
	if target != "93.184.216.34:443" {
		t.Fatalf("dial target %s", target)
	}
	if len(f.registry.Bindings["s1"].Decisions) != 0 {
		t.Fatal("decision retained after completion")
	}
	if res.Header.Get("Alt-Svc") != "clear" {
		t.Fatal("Alt-Svc not cleared")
	}
}

func TestGatewayHostCodexSourceRoutesAndInjectsAccountOnlyOnExactPath(t *testing.T) {
	f := newGatewayFixture(t)
	f.registry.ProviderSource = fakeSource{available: true, routes: map[string][2]string{"/v1/responses": {"chatgpt.com", "/backend-api/codex/responses"}},
		headers: map[string]string{"Authorization": "Bearer synthetic-host-access-token", "ChatGPT-Account-ID": "synthetic-owner-account"}}
	f.registry.Bindings["s1"].ProviderSecret = ""
	headers := map[string]string{"Content-Type": "application/json", "ChatGPT-Account-ID": "guest-selected-account"}
	res, _, err := f.do("POST", f.providerURL(), headers, `{"input":"synthetic"}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("response: %v %v", res, err)
	}
	r := f.upstreamRequests()[0]
	if r.host != "chatgpt.com" || r.path != "/backend-api/codex/responses" || r.headers.Get("ChatGPT-Account-ID") != "synthetic-owner-account" || r.headers.Get("Authorization") != "Bearer synthetic-host-access-token" {
		t.Fatalf("upstream: %+v", r)
	}
	res, _, _ = f.do("POST", "http://host.docker.internal:"+itoa(f.port)+"/openai/v1/chat/completions", headers, `{}`)
	if res == nil || res.StatusCode != 503 {
		t.Fatalf("chat completions: %v", res)
	}
}

func TestGatewayUnknownDestinationDeniedBeforeDNSLookup(t *testing.T) {
	f := newGatewayFixture(t)
	res, _, err := f.do("GET", "https://exfiltration.attacker.example/test", nil, "")
	if err == nil && res.StatusCode != 403 {
		t.Fatalf("expected 403, got %v", res.StatusCode)
	}
	if f.resolveCalls.Load() != 0 {
		t.Fatal("DNS lookup performed")
	}
}

func TestGatewayWrongReversePathPortOrAuthorityDenied(t *testing.T) {
	f := newGatewayFixture(t)
	for _, target := range []string{"http://host.docker.internal:" + itoa(f.port) + "/openai/v1/files", "http://host.docker.internal:18765/openai/v1/responses",
		"http://host.docker.internal:" + itoa(f.port) + "/openai/v1/responses?url=evil", "http://host.docker.internal:" + itoa(f.port) + "/api/approve"} {
		res, _, err := f.do("POST", target, providerHeaders, `{}`)
		if err != nil || res.StatusCode != 403 {
			t.Fatalf("%s: %v %v", target, res, err)
		}
	}
	if f.resolveCalls.Load() != 0 || len(f.upstreamRequests()) != 0 {
		t.Fatal("upstream reached")
	}
}

func TestGatewayBrowserOriginAndOpaqueConnectRejected(t *testing.T) {
	f := newGatewayFixture(t)
	headers := map[string]string{"Content-Type": "application/json", "Origin": "http://127.0.0.1:3000"}
	res, _, err := f.do("POST", f.providerURL(), headers, `{}`)
	if err != nil || res.StatusCode != 403 {
		t.Fatalf("origin: %v %v", res, err)
	}
	for _, target := range []string{"127.0.0.1:443", "example.com:22", "swcdn.apple.com:8443"} {
		conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(f.port))
		if err != nil {
			t.Fatal(err)
		}
		conn.Write([]byte("CONNECT " + target + " HTTP/1.1\r\nHost: " + target + "\r\n\r\n"))
		line, _ := bufio.NewReader(conn).ReadString('\n')
		conn.Close()
		if !strings.Contains(line, " 403 ") {
			t.Fatalf("%s: %s", target, line)
		}
	}
	if f.resolveCalls.Load() != 0 {
		t.Fatal("DNS lookup performed")
	}
}

func TestGatewayEndRunRevokesInflightDecisionBeforeResponseDelivery(t *testing.T) {
	f := newGatewayFixture(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		f.registry.End(f.value)
		w.Write([]byte("synthetic upstream result"))
	})
	res, body, _ := f.do("POST", f.providerURL(), providerHeaders, `{}`)
	if res == nil || res.StatusCode != 403 || strings.Contains(string(body), "synthetic upstream result") {
		t.Fatalf("response: %v %s", res, body)
	}
}

func TestGatewayProviderCannotEchoBrokerSecretToGuest(t *testing.T) {
	f := newGatewayFixture(t)
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"echo":"synthetic-provider-fixture-secret"}`))
	})
	res, body, _ := f.do("POST", f.providerURL(), providerHeaders, `{}`)
	if res == nil || res.StatusCode != 502 || strings.Contains(string(body), "synthetic-provider-fixture-secret") {
		t.Fatalf("response: %v %s", res, body)
	}
}

func sseHandler(chunks []string, wait chan struct{}) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		flusher := w.(http.Flusher)
		for i, chunk := range chunks {
			if wait != nil && i > 0 {
				<-wait
			}
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	}
}

func (f *gatewayFixture) streamRequest(t *testing.T) (*http.Response, *bufio.Reader) {
	t.Helper()
	req, _ := http.NewRequest("POST", f.providerURL(), strings.NewReader(`{"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	res, err := f.client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return res, bufio.NewReader(res.Body)
}

func TestGatewayProviderStreamInheritsBrokerAuthorizationAndCleanup(t *testing.T) {
	f := newGatewayFixture(t)
	f.setHandler(sseHandler([]string{"data: hello\n\n", "data: world\n\n"}, nil))
	res, reader := f.streamRequest(t)
	defer res.Body.Close()
	if res.StatusCode != 200 || res.ContentLength != -1 || res.Header.Get("Alt-Svc") != "clear" {
		t.Fatalf("stream response: %v", res)
	}
	all, err := io.ReadAll(reader)
	if err != nil || string(all) != "data: hello\n\ndata: world\n\n" {
		t.Fatalf("stream body %q %v", all, err)
	}
	if f.upstreamRequests()[0].headers.Get("Authorization") != "Bearer synthetic-provider-fixture-secret" {
		t.Fatal("stream request not authorized")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(f.registry.Bindings["s1"].Decisions) != 0 {
		time.Sleep(10 * time.Millisecond)
	}
	if len(f.registry.Bindings["s1"].Decisions) != 0 {
		t.Fatal("decision retained after stream")
	}
	if !f.auditContains(`"phase":"completed"`) || f.auditContains("hello") {
		t.Fatal("audit summary")
	}
}

func TestGatewayProviderStreamBlocksSplitBrokerCredential(t *testing.T) {
	f := newGatewayFixture(t)
	release := make(chan struct{})
	f.setHandler(sseHandler([]string{"data: synthetic-provider-fixture-", "secret\n\n", "data: more\n\n"}, release))
	res, reader := f.streamRequest(t)
	defer res.Body.Close()
	first := make([]byte, 6)
	if _, err := io.ReadFull(reader, first); err != nil || string(first) != "data: " {
		t.Fatalf("first chunk %q %v", first, err)
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err == nil || strings.Contains(string(rest), "secret") {
		t.Fatalf("stream continued: %q %v", rest, err)
	}
	if !f.auditContains("provider response exposed a protected credential") {
		t.Fatal("interruption not audited")
	}
}

func TestGatewayEndRunInterruptsActiveProviderStream(t *testing.T) {
	f := newGatewayFixture(t)
	release := make(chan struct{})
	f.setHandler(sseHandler([]string{"data: hello\n\n", "data: too late\n\n"}, release))
	res, reader := f.streamRequest(t)
	defer res.Body.Close()
	line, err := reader.ReadString('\n')
	if err != nil || line != "data: hello\n" {
		t.Fatalf("first line %q %v", line, err)
	}
	f.registry.End(f.value)
	time.Sleep(700 * time.Millisecond)
	close(release)
	rest, err := io.ReadAll(reader)
	if err == nil || strings.Contains(string(rest), "too late") {
		t.Fatalf("stream survived end: %q %v", rest, err)
	}
	// After End the broker refuses every gateway action, including the
	// interruption event, exactly like the Python broker; the run's audit
	// trail ends with the lease.
	if f.registry.Bindings["s1"].Lease != nil || len(f.registry.Bindings["s1"].Decisions) != 0 {
		t.Fatal("lease or decisions survived end")
	}
}

func TestGatewayLostControlNeverForwardsOrResolves(t *testing.T) {
	f := newGatewayFixture(t)
	f.gateway.cfg.Capability = "wrong"
	f.gateway.cfg.Control = func(message map[string]any) (map[string]any, error) { return f.registry.Proxy("s1", "wrong", message) }
	res, _, err := f.do("POST", f.providerURL(), providerHeaders, `{}`)
	if err != nil || res.StatusCode != 503 {
		t.Fatalf("response: %v %v", res, err)
	}
	if f.resolveCalls.Load() != 0 || len(f.upstreamRequests()) != 0 {
		t.Fatal("forwarded without control")
	}
}

func (f *gatewayFixture) configureDocumentAPI(t *testing.T) (*DocumentAPI, *[]map[string]string) {
	t.Helper()
	key := filepath.Join(f.dir, "document-key")
	writePrivate(t, key, []byte("synthetic-document-key-at-least-32-characters"))
	api, err := NewDocumentAPI("http://localhost:18080", key)
	if err != nil {
		t.Fatal(err)
	}
	calls := &[]map[string]string{}
	api.Dispatch = func(method, path string, body []byte, headers map[string]string) (int, []byte, error) {
		record := map[string]string{"method": method, "path": path, "body": string(body)}
		for k, v := range headers {
			record[k] = v
		}
		*calls = append(*calls, record)
		return 200, []byte(`{"documents":[]}`), nil
	}
	f.registry.DocumentAPI = api
	return api, calls
}

func TestGatewayDocumentsUseBrokerHTTPStripGuestHeadersAndKeepSignaturesOffFlow(t *testing.T) {
	f := newGatewayFixture(t)
	_, calls := f.configureDocumentAPI(t)
	headers := map[string]string{"Cookie": "owner=forged", "X-Warden-Context": "forged", "X-Warden-Signature": "forged", "X-Forwarded-Host": "attacker", "Proxy-Authorization": "Bearer forged", "Authorization": "Bearer forged"}
	res, body, err := f.do("GET", "http://host.docker.internal:"+itoa(f.port)+"/workspace/v1/documents", headers, "")
	if err != nil || res.StatusCode != 200 || string(body) != `{"documents":[]}` {
		t.Fatalf("response: %v %s %v", res, body, err)
	}
	if len(*calls) != 1 {
		t.Fatalf("calls: %v", *calls)
	}
	call := (*calls)[0]
	if call["method"] != "GET" || call["path"] != "/agent/v1/documents" || call["body"] != "" || call["X-Warden-Signature"] == "forged" || call["Authorization"] != "" || call["Cookie"] != "" {
		t.Fatalf("dispatch: %v", call)
	}
	if strings.Contains(string(body), call["X-Warden-Signature"]) || f.resolveCalls.Load() != 0 {
		t.Fatal("signature or DNS leak")
	}
}

func TestGatewayDocumentBadRouteAuthorityOriginEncodingAndMissingLeaseDenied(t *testing.T) {
	f := newGatewayFixture(t)
	_, calls := f.configureDocumentAPI(t)
	base := "http://host.docker.internal:" + itoa(f.port)
	for _, c := range []struct {
		target  string
		headers map[string]string
	}{{base + "/workspace/v1/documents?projectID=other", nil}, {"http://host.docker.internal:19444/workspace/v1/documents", nil},
		{base + "/workspace/v1/documents", map[string]string{"Content-Encoding": "gzip"}}, {base + "/workspace/v1/documents", map[string]string{"Origin": "http://localhost:18080"}}} {
		res, _, err := f.do("GET", c.target, c.headers, "")
		if err != nil || res.StatusCode < 400 {
			t.Fatalf("%s: %v %v", c.target, res, err)
		}
	}
	f.registry.End(f.value)
	res, _, err := f.do("GET", base+"/workspace/v1/documents", nil, "")
	if err != nil || res.StatusCode != 503 {
		t.Fatalf("after end: %v %v", res, err)
	}
	if len(*calls) != 0 || f.resolveCalls.Load() != 0 {
		t.Fatal("dispatched")
	}
}

func TestGatewayClaudeLeaseRoutesOnlyClaudeAndStripsGuestCredentials(t *testing.T) {
	f := newGatewayFixture(t)
	f.registry.End(f.value)
	value := runContext(map[string]any{"runID": "claude-run", "provider": "claude"})
	f.registry.ClaudeSource = fakeSource{available: true, routes: map[string][2]string{"/v1/messages": {"api.anthropic.com", "/v1/messages"}, "/v1/messages/count_tokens": {"api.anthropic.com", "/v1/messages/count_tokens"}},
		headers: map[string]string{"Authorization": "Bearer synthetic-claude-host-secret"}}
	if r, err := f.registry.Begin(value, false); err != nil || !ready(r) {
		t.Fatalf("claude begin: %v %v", r, err)
	}
	headers := map[string]string{"Content-Type": "application/json", "x-api-key": "guest-key", "Cookie": "guest-cookie"}
	res, _, err := f.do("POST", "http://host.docker.internal:"+itoa(f.port)+"/anthropic/v1/messages?beta=true", headers, `{}`)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("claude: %v %v", res, err)
	}
	r := f.upstreamRequests()[0]
	if r.host != "api.anthropic.com" || r.path != "/v1/messages" || r.headers.Get("Authorization") != "Bearer synthetic-claude-host-secret" || r.headers.Get("x-api-key") != "" || r.headers.Get("Cookie") != "" {
		t.Fatalf("upstream: %+v", r)
	}
	res, _, _ = f.do("POST", f.providerURL(), providerHeaders, `{}`)
	if res == nil || res.StatusCode == 200 {
		t.Fatal("codex route under claude lease")
	}
	f.registry.End(value)
	res, _, _ = f.do("POST", "http://host.docker.internal:"+itoa(f.port)+"/anthropic/v1/messages", headers, `{}`)
	if res == nil || res.StatusCode == 200 {
		t.Fatal("claude route after end")
	}
}

func TestGatewayGoogleSharingInjectionAndRevocation(t *testing.T) {
	f := newGatewayFixture(t)
	google := &fakeGoogle{authorization: "Bearer synthetic-google-credential", canWrite: true, connected: true, configured: true}
	sharing, err := NewSharing(filepath.Join(f.dir, "sharing"), google, f.clock.wall, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	f.registry.Sharing = sharing
	r, _ := sharing.Dispatch("request", map[string]any{"chatID": "c1", "sandboxID": "s1", "callID": "tool-1", "reason": "Read plan"})
	sharing.Dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"doc-a"}, "duration": 900})
	release := make(chan struct{})
	f.setHandler(func(w http.ResponseWriter, req *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"documentId":"doc-a"}`))
	})
	type outcome struct {
		res *http.Response
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		res, _, err := f.do("GET", "https://docs.googleapis.com/v1/documents/doc-a", map[string]string{"Cookie": "hostile-cookie"}, "")
		done <- outcome{res, err}
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.upstreamRequests()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	reqs := f.upstreamRequests()
	if len(reqs) != 1 || reqs[0].headers.Get("Authorization") != "Bearer synthetic-google-credential" || reqs[0].headers.Get("Cookie") != "" {
		t.Fatalf("upstream: %+v", reqs)
	}
	sharing.Dispatch("revoke", map[string]any{"id": r["request_id"]})
	close(release)
	result := <-done
	if result.err == nil && result.res.StatusCode == 200 {
		t.Fatal("revoked grant delivered response")
	}
	res, _, err := f.do("GET", "https://docs.googleapis.com/v1/documents/doc-b", nil, "")
	if err != nil || res.StatusCode != 403 || len(f.upstreamRequests()) != 1 {
		t.Fatalf("doc-b: %v %v", res, err)
	}
}

func TestGatewayRepositorySharingInjectionAndRevocation(t *testing.T) {
	f := newGatewayFixture(t)
	github := NewGitHubAppCredentials([]string{"/broker"}, "owner", 123, NewRedactor(), nil)
	github.run = func(input []byte) ([]byte, error) {
		if strings.Contains(string(input), "repositories") {
			return mustJSON(map[string]any{"owner": "owner", "app_id": 123, "repositories": []any{map[string]any{"id": 9, "full_name": "owner/repo"}}, "next_page": nil}), nil
		}
		result := brokerResult("owner/repo", map[string]string{"metadata": "read"})
		result["token"] = "ghs_synthetic-github-credential-value-123456"
		result["repository_id"] = 9
		return mustJSON(result), nil
	}
	sharing, err := NewSharing(filepath.Join(f.dir, "sharing"), nil, f.clock.wall, github)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	f.registry.Sharing = sharing
	if _, err = sharing.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{"owner/repo"}}); err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	f.setHandler(func(w http.ResponseWriter, req *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"full_name":"owner/repo"}`))
	})
	done := make(chan *http.Response, 1)
	go func() {
		res, _, _ := f.do("GET", "https://api.github.com/repos/owner/repo", map[string]string{"Cookie": "forged", "Authorization": "Bearer forged"}, "")
		done <- res
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.upstreamRequests()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	reqs := f.upstreamRequests()
	if len(reqs) != 1 || reqs[0].headers.Get("Authorization") != "Bearer ghs_synthetic-github-credential-value-123456" || reqs[0].headers.Get("Cookie") != "" {
		t.Fatalf("upstream: %+v", reqs)
	}
	sharing.Dispatch("github_select", map[string]any{"chatID": "c1", "sandboxID": "s1", "repositories": []any{}})
	close(release)
	if res := <-done; res != nil && res.StatusCode == 200 {
		t.Fatal("revoked repository delivered response")
	}
	res, _, err := f.do("GET", "https://api.github.com/repos/owner/repo", nil, "")
	if err != nil || res.StatusCode < 400 || len(f.upstreamRequests()) != 1 {
		t.Fatalf("after revocation: %v %v", res, err)
	}
}

func TestGatewayHealthProbeAndExternalEgress(t *testing.T) {
	f := newGatewayFixture(t)
	if !GatewayHealthy(f.registry.Bindings["s1"]) {
		t.Fatal("health probe failed")
	}
	engine := f.registry.Bindings["s1"].Engine
	res, _, err := f.do("GET", "http://example.com/plain", nil, "")
	if err != nil || res.StatusCode != 403 {
		t.Fatalf("restricted plain http: %v %v", res, err)
	}
	policy := engine.PolicyCopy()
	policy["egress"] = map[string]any{"mode": "public", "destinations": []any{}}
	if err := engine.SavePolicy(policy); err != nil {
		t.Fatal(err)
	}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Alt-Svc", `h3=":443"; ma=86400`)
		w.Write([]byte("hello"))
	})
	res, body, err := f.do("GET", "https://example.com/?token=hidden", map[string]string{"Authorization": "Bearer external-secret"}, "")
	if err != nil || res.StatusCode != 200 || string(body) != "hello" || res.Header.Get("Alt-Svc") != "clear" {
		t.Fatalf("external: %v %s %v", res, body, err)
	}
	r := f.upstreamRequests()[0]
	if r.headers.Get("Authorization") != "Bearer external-secret" {
		t.Fatal("external credential stripped")
	}
	if f.auditContains("external-secret") || f.auditContains("hidden") || !f.auditContains("http.request.external") {
		t.Fatal("external audit leaked or missing")
	}
}

func TestGatewayStreamHeaderEchoEncodingAndTrailerFailBeforeStream(t *testing.T) {
	f := newGatewayFixture(t)
	for _, extra := range []map[string]string{{"X-Echo": "synthetic-provider-fixture-secret"}, {"Content-Encoding": "br"}, {"Trailer": "X-Echo"}} {
		headers := extra
		f.setHandler(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.WriteHeader(200)
			w.(http.Flusher).Flush()
			w.Write([]byte("data: x\n\n"))
		})
		res, _, err := f.do("POST", f.providerURL(), providerHeaders, `{"stream":true}`)
		if err != nil || res.StatusCode != 502 {
			t.Fatalf("%v: %v %v", extra, res, err)
		}
	}
}

func TestGatewayCompressionIsDecodedAndFramingHeadersRemoved(t *testing.T) {
	f := newGatewayFixture(t)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	gz.Write([]byte("data: yes\n\n"))
	gz.Close()
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(200)
		w.Write(buf.Bytes())
	})
	res, body, err := f.do("POST", f.providerURL(), providerHeaders, `{"stream":true}`)
	if err != nil || res.StatusCode != 200 || string(body) != "data: yes\n\n" || res.Header.Get("Content-Encoding") != "" {
		t.Fatalf("gzip stream: %v %q %v", res, body, err)
	}
}

func TestGatewayCodexMissingContentTypeRequiresExplicitStreamRequest(t *testing.T) {
	f := newGatewayFixture(t)
	f.registry.ProviderSource = fakeSource{available: true, routes: map[string][2]string{"/v1/responses": {"chatgpt.com", "/backend-api/codex/responses"}}, headers: map[string]string{"Authorization": "Bearer synthetic-host-access-token"}}
	f.registry.Bindings["s1"].ProviderSecret = ""
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header()["Content-Type"] = nil
		w.WriteHeader(200)
		w.Write([]byte("data: x\n\n"))
	})
	for _, c := range []struct {
		body     string
		streamed bool
	}{{`{"stream":true}`, true}, {`{"stream":false}`, false}, {`{"stream":"true"}`, false}, {`{}`, false}} {
		res, _, err := f.do("POST", f.providerURL(), map[string]string{"Content-Type": "application/json"}, c.body)
		if err != nil || res.StatusCode != 200 {
			t.Fatalf("%s: %v %v", c.body, res, err)
		}
		if (res.ContentLength == -1) != c.streamed {
			t.Fatalf("%s streamed=%v", c.body, res.ContentLength == -1)
		}
	}
}

func TestGatewayRevocationWatcherInterruptsBufferedFlow(t *testing.T) {
	f := newGatewayFixture(t)
	release := make(chan struct{})
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Write([]byte("late"))
	})
	done := make(chan error, 1)
	go func() {
		res, body, err := f.do("POST", f.providerURL(), providerHeaders, `{}`)
		if err == nil && res.StatusCode == 200 && string(body) == "late" {
			done <- errors.New("delivered after revocation")
			return
		}
		done <- nil
	}()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && len(f.upstreamRequests()) == 0 {
		time.Sleep(10 * time.Millisecond)
	}
	f.registry.End(f.value)
	time.Sleep(700 * time.Millisecond)
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if f.registry.Bindings["s1"].Lease != nil || len(f.registry.Bindings["s1"].Decisions) != 0 {
		t.Fatal("lease or decisions survived end")
	}
}

func TestGatewayReusesExistingCAAndRejectsBadTunnels(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreateGatewayCA(dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := LoadOrCreateGatewayCA(dir)
	if err != nil || !bytes.Equal(again.CertPEM, ca.CertPEM) {
		t.Fatal("CA regenerated")
	}
	leaf, err := ca.Certificate("api.openai.com")
	if err != nil {
		t.Fatal(err)
	}
	roots := ca.Pool()
	if _, err = leaf.Leaf.Verify(x509.VerifyOptions{DNSName: "api.openai.com", Roots: roots}); err != nil {
		t.Fatalf("leaf verification: %v", err)
	}
	if _, err = ca.Certificate("127.0.0.1"); err == nil {
		t.Fatal("IP literal certificate issued")
	}
	f := newGatewayFixture(t)
	conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(f.port))
	if err != nil {
		t.Fatal(err)
	}
	conn.Write([]byte("SSH-2.0-attacker\r\n\r\n"))
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	data, _ := io.ReadAll(conn)
	conn.Close()
	if len(data) > 0 && !strings.Contains(string(data), "400") {
		t.Fatalf("raw protocol answered: %s", data)
	}
	if decoded, _ := base64.StdEncoding.DecodeString(""); len(decoded) != 0 {
		t.Fatal("unreachable")
	}
}

// Open egress (sandboxes.egress = open): any public host is reachable, and a
// brokered host with no grant is reached anonymously, guest credentials
// stripped and nothing injected, instead of being refused. A disconnected
// network still denies everything.
func TestGatewayOpenEgressReachesBrokeredHostsAnonymously(t *testing.T) {
	f := newGatewayFixture(t)
	engine := f.registry.Bindings["s1"].Engine
	// Restricted: no selection, so api.github.com is refused before upstream.
	res, _, err := f.do("GET", "https://api.github.com/repos/owner/repo", map[string]string{"Authorization": "Bearer forged", "Cookie": "forged"}, "")
	if err != nil || res.StatusCode < 400 || len(f.upstreamRequests()) != 0 {
		t.Fatalf("restricted github without grant: %v %v", res, err)
	}
	policy := engine.PolicyCopy()
	policy["egress"] = map[string]any{"mode": "public", "destinations": []any{}}
	if err := engine.SavePolicy(policy); err != nil {
		t.Fatal(err)
	}
	f.setHandler(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"full_name":"owner/repo"}`))
	})
	res, body, err := f.do("GET", "https://api.github.com/repos/owner/repo", map[string]string{"Authorization": "Bearer forged", "Cookie": "forged"}, "")
	if err != nil || res.StatusCode != 200 || !strings.Contains(string(body), "owner/repo") {
		t.Fatalf("open github without grant: %v %s %v", res, body, err)
	}
	reqs := f.upstreamRequests()
	if len(reqs) != 1 || reqs[0].headers.Get("Authorization") != "" || reqs[0].headers.Get("Cookie") != "" {
		t.Fatalf("anonymous request carried credentials: %+v", reqs)
	}
	if !f.auditContains("http.request.external") || !f.auditContains(`"anonymous":true`) || f.auditContains("forged") {
		t.Fatal("anonymous audit missing or leaked the guest header")
	}
	// An unbrokered host is plain external egress with its own headers.
	res, _, err = f.do("GET", "https://example.com/page", map[string]string{"Authorization": "Bearer site-login"}, "")
	if err != nil || res.StatusCode != 200 || f.upstreamRequests()[1].headers.Get("Authorization") != "Bearer site-login" {
		t.Fatalf("open external: %v %v", res, err)
	}
	// Disconnected: open mode grants nothing.
	if err := engine.SetNetwork(false); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"https://api.github.com/repos/owner/repo", "https://example.com/page"} {
		res, _, err = f.do("GET", target, nil, "")
		if (err == nil && res.StatusCode < 400) || len(f.upstreamRequests()) != 2 {
			t.Fatalf("disconnected %s: %v %v", target, res, err)
		}
	}
}

// A Docs edit naming an attached image reaches Google with the image's
// one-off published URL instead of the placeholder, and the token is
// withdrawn once Google has answered.
func TestGatewayPublishesInlineImagesForOneEdit(t *testing.T) {
	f := newGatewayFixture(t)
	google := &fakeGoogle{authorization: "Bearer synthetic-google-credential", canWrite: true, connected: true, configured: true}
	sharing, err := NewSharing(filepath.Join(f.dir, "sharing"), google, f.clock.wall, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	sharing.PublicURL = "https://warden.example"
	f.registry.Sharing = sharing
	added, err := sharing.Dispatch("image_add", map[string]any{"chatID": "c1", "sandboxID": "s1", "caption": "Chart", "png": base64.StdEncoding.EncodeToString(testPNG)})
	if err != nil {
		t.Fatal(err)
	}
	r, _ := sharing.Dispatch("request", map[string]any{"chatID": "c1", "sandboxID": "s1", "callID": "tool-1", "reason": "Insert chart", "access": "structure"})
	sharing.Dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"doc-a"}, "duration": 900})
	var seen string
	f.setHandler(func(w http.ResponseWriter, req *http.Request) {
		body, _ := io.ReadAll(req.Body)
		var data map[string]any
		_ = json.Unmarshal(body, &data)
		seen, _ = data["requests"].([]any)[0].(map[string]any)["insertInlineImage"].(map[string]any)["uri"].(string)
		token := strings.TrimSuffix(strings.TrimPrefix(seen, "https://warden.example/published/"), ".png")
		if png, err := sharing.Images.Published(token); err != nil || len(png) == 0 {
			w.WriteHeader(500)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"documentId":"doc-a"}`))
	})
	edit := `{"requests":[{"insertInlineImage":{"uri":"warden-image:` + added["image_id"].(string) + `","location":{"index":1}}}]}`
	res, _, err := f.do("POST", "https://docs.googleapis.com/v1/documents/doc-a:batchUpdate", map[string]string{"Content-Type": "application/json"}, edit)
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("edit: %v %v", res, err)
	}
	if !strings.HasPrefix(seen, "https://warden.example/published/") || strings.Contains(seen, "warden-image:") {
		t.Fatalf("upstream uri %q", seen)
	}
	token := strings.TrimSuffix(strings.TrimPrefix(seen, "https://warden.example/published/"), ".png")
	if _, err := sharing.Images.Published(token); err == nil {
		t.Fatal("image still published after the edit")
	}
	res, _, err = f.do("POST", "https://docs.googleapis.com/v1/documents/doc-a:batchUpdate", map[string]string{"Content-Type": "application/json"}, `{"requests":[{"insertInlineImage":{"uri":"https://evil.test/x.png"}}]}`)
	if (err == nil && res.StatusCode < 400) || len(f.upstreamRequests()) != 1 {
		t.Fatalf("remote image: %v %v", res, err)
	}
}

// A document grant covers the spreadsheet API too: the same ID on
// sheets.googleapis.com gets the owner's credential, other IDs do not.
func TestGatewaySheetsShareTheDocumentGrant(t *testing.T) {
	f := newGatewayFixture(t)
	google := &fakeGoogle{authorization: "Bearer synthetic-google-credential", canWrite: true, connected: true, configured: true}
	sharing, err := NewSharing(filepath.Join(f.dir, "sharing"), google, f.clock.wall, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sharing.Close()
	f.registry.Sharing = sharing
	r, _ := sharing.Dispatch("request", map[string]any{"chatID": "c1", "sandboxID": "s1", "callID": "tool-1", "reason": "Read sheet", "access": "write"})
	sharing.Dispatch("resolve", map[string]any{"id": r["request_id"], "allow": true, "documents": []any{"sheet-a"}, "duration": 900})
	f.setHandler(func(w http.ResponseWriter, req *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"values":[["1"]]}`))
	})
	res, _, err := f.do("GET", "https://sheets.googleapis.com/v4/spreadsheets/sheet-a/values/Sheet1!A1:B2", map[string]string{"Authorization": "Bearer forged"}, "")
	if err != nil || res.StatusCode != 200 {
		t.Fatalf("sheet read: %v %v", res, err)
	}
	if reqs := f.upstreamRequests(); len(reqs) != 1 || reqs[0].headers.Get("Authorization") != "Bearer synthetic-google-credential" {
		t.Fatalf("upstream: %+v", reqs)
	}
	res, _, err = f.do("POST", "https://sheets.googleapis.com/v4/spreadsheets/sheet-a/values/Sheet1!A1:append?valueInputOption=RAW", map[string]string{"Content-Type": "application/json"}, `{"values":[["x"]]}`)
	if err != nil || res.StatusCode != 200 || len(f.upstreamRequests()) != 2 {
		t.Fatalf("sheet write: %v %v", res, err)
	}
	res, _, err = f.do("GET", "https://sheets.googleapis.com/v4/spreadsheets/sheet-b", nil, "")
	if (err == nil && res.StatusCode < 400) || len(f.upstreamRequests()) != 2 {
		t.Fatalf("unshared sheet: %v %v", res, err)
	}
}
