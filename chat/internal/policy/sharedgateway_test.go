package policy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// sharedFixture runs one SharedGateway in front of two bindings, each with
// its own provider secret, against one fake TLS upstream.
type sharedFixture struct {
	t            *testing.T
	registry     *Registry
	verifier     *fixtureVerifier
	gateway      *SharedGateway
	ca           *GatewayCA
	upstreamCA   *GatewayCA
	upstreamAddr string
	values       map[string]map[string]any
	mu           sync.Mutex
	requests     []upstreamRequest
	dials        int
}

func newSharedFixture(t *testing.T) *sharedFixture {
	dir := t.TempDir()
	f := &sharedFixture{t: t, verifier: &fixtureVerifier{enabled: true}, values: map[string]map[string]any{}}
	clock := &testClock{now: 1000, mono: 1000}
	f.registry = newTestRegistry(t, filepath.Join(dir, "state"), clock, f.verifier)
	f.verifier.registry = f.registry
	t.Cleanup(func() { f.registry.Close() })
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.upstreamCA, err = LoadOrCreateGatewayCA(filepath.Join(dir, "upstream-ca"))
	if err != nil {
		t.Fatal(err)
	}
	upstreamListener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f.upstreamAddr = upstreamListener.Addr().String()
	upstream := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		f.mu.Lock()
		f.requests = append(f.requests, upstreamRequest{r.Method, r.Host, r.URL.RequestURI(), r.Header.Clone(), body})
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"seen":true}`))
	})}
	go upstream.Serve(tls.NewListener(upstreamListener, &tls.Config{GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return f.upstreamCA.Certificate(hello.ServerName)
	}, NextProtos: []string{"http/1.1"}}))
	t.Cleanup(func() { upstream.Close() })
	shared, err := NewSharedGateway(f.registry, nil, listener, "gateway.test")
	if err != nil {
		t.Fatal(err)
	}
	shared.Factory = func(cfg GatewayConfig) (*BindingGateway, error) {
		cfg.Resolve = func(context.Context, string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil }
		cfg.Dial = func(ctx context.Context, network, address string) (net.Conn, error) {
			f.mu.Lock()
			f.dials++
			f.mu.Unlock()
			return (&net.Dialer{}).DialContext(ctx, "tcp", f.upstreamAddr)
		}
		cfg.Roots = f.upstreamCA.Pool()
		return NewBindingGateway(cfg)
	}
	f.gateway = shared
	f.registry.Gateways = shared
	f.ca = f.registry.CA.Derive()
	for _, sandbox := range []string{"s1", "s2"} {
		value := runContext(map[string]any{"sandboxID": sandbox, "runtimeName": "sbx-" + sandbox, "chatID": "c-" + sandbox})
		f.values[sandbox] = value
		if _, err := f.registry.Register(value); err != nil {
			t.Fatal(err)
		}
		if _, err := f.registry.ConfigureProvider(value, "synthetic-secret-"+sandbox+"-only"); err != nil {
			t.Fatal(err)
		}
		f.registry.mu.Lock()
		_, err := shared.Bind(f.registry.Bindings[sandbox])
		f.registry.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *sharedFixture) endpoint(sandbox string) GatewayEndpoint {
	f.registry.mu.Lock()
	defer f.registry.mu.Unlock()
	return f.gateway.Endpoint(f.registry.Bindings[sandbox])
}

func (f *sharedFixture) begin(sandbox string) map[string]any {
	f.t.Helper()
	result, err := f.registry.Begin(f.values[sandbox], false)
	if err != nil || !ready(result) {
		f.t.Fatalf("begin %s: %v %v", sandbox, result, err)
	}
	return result
}

// client proxies through the shared gateway with the given proxy URL. The
// advertised name resolves to the local listener, as a Service name would
// in a cluster.
func (f *sharedFixture) client(proxy string) *http.Client {
	proxyURL, _ := url.Parse(proxy)
	return &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: &tls.Config{RootCAs: f.ca.Pool()}, DisableKeepAlives: true, DialContext: f.dial}}
}

func (f *sharedFixture) dial(ctx context.Context, network, address string) (net.Conn, error) {
	if host, port, err := net.SplitHostPort(address); err == nil && host == "gateway.test" {
		address = net.JoinHostPort("127.0.0.1", port)
	}
	return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, address)
}

func (f *sharedFixture) do(client *http.Client, method, target string, headers map[string]string, body string) (*http.Response, []byte) {
	f.t.Helper()
	req, err := http.NewRequest(method, target, strings.NewReader(body))
	if err != nil {
		f.t.Fatal(err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, []byte(err.Error())
	}
	defer res.Body.Close()
	payload, _ := io.ReadAll(res.Body)
	return res, payload
}

func (f *sharedFixture) openEgress(sandbox string) {
	engine := f.registry.Bindings[sandbox].Engine
	policy := engine.PolicyCopy()
	policy["egress"] = map[string]any{"mode": "public", "destinations": []any{}}
	if err := engine.SavePolicy(policy); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sharedFixture) upstream() []upstreamRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]upstreamRequest{}, f.requests...)
}

func (f *sharedFixture) dialCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dials
}

func TestSharedGatewayBeginAdvertisesCredentialedEndpoint(t *testing.T) {
	f := newSharedFixture(t)
	result := f.begin("s1")
	e := f.endpoint("s1")
	if e.Host != "gateway.test" || e.Port != f.gateway.Port() || e.BindingID != "s1" || e.Capability == "" {
		t.Fatalf("endpoint: %+v", e)
	}
	if result["proxyURL"] != "http://s1:"+e.Capability+"@gateway.test:"+itoa(e.Port) || result["apiKeyPlaceholder"] != "s1."+e.Capability ||
		result["providerBaseURL"] != "http://gateway.test:"+itoa(e.Port)+"/openai/v1" {
		t.Fatalf("begin: %v", result)
	}
	if strings.Contains(Dumps(result), "synthetic-secret") {
		t.Fatal("provider secret leaked")
	}
	if !f.gateway.Healthy(f.registry.Bindings["s1"]) || !f.gateway.Healthy(f.registry.Bindings["s2"]) {
		t.Fatal("bound bindings must be healthy")
	}
	stranger := &Binding{Identity: map[string]string{"sandboxID": "s3"}, Capability: "never-bound", GatewayPort: f.gateway.Port()}
	if f.gateway.Healthy(stranger) {
		t.Fatal("unbound binding answered the health probe")
	}
	if f.gateway.Healthy(&Binding{Identity: map[string]string{"sandboxID": "s1"}, Capability: f.registry.Bindings["s1"].Capability, GatewayPort: 1}) {
		t.Fatal("wrong port reported healthy")
	}
}

func TestSharedGatewayDispatchesProxiedRequestsByCredential(t *testing.T) {
	f := newSharedFixture(t)
	f.begin("s1")
	f.begin("s2")
	f.openEgress("s1")
	// s1 may reach external hosts; s2 (restricted, no destinations) may not:
	// the same request is decided by the binding the credential names.
	res, body := f.do(f.client(f.endpoint("s1").ProxyURL()), "GET", "https://example.com/page", map[string]string{"Authorization": "Bearer site-login"}, "")
	if res == nil || res.StatusCode != 200 || !strings.Contains(string(body), "seen") {
		t.Fatalf("s1 external: %v %s", res, body)
	}
	if reqs := f.upstream(); len(reqs) != 1 || reqs[0].headers.Get("Authorization") != "Bearer site-login" || reqs[0].headers.Get("Proxy-Authorization") != "" {
		t.Fatalf("upstream: %+v", reqs)
	}
	res, _ = f.do(f.client(f.endpoint("s2").ProxyURL()), "GET", "https://example.com/page", nil, "")
	if res != nil && res.StatusCode == 200 {
		t.Fatal("s2 reached an external host through s1's policy")
	}
	if dials := f.dialCount(); dials != 1 {
		t.Fatalf("upstream dialled %d times", dials)
	}
	audit := func(sandbox, needle string) bool {
		data, _ := os.ReadFile(filepath.Join(f.registry.Bindings[sandbox].Engine.State, "audit", "events.jsonl"))
		return strings.Contains(string(data), needle)
	}
	if !audit("s1", "http.request.external") || audit("s2", "http.request.external") {
		t.Fatal("audit did not land in the dispatched binding's engine")
	}
	if audit("s1", f.endpoint("s1").Capability) {
		t.Fatal("credential written to the audit")
	}
	// The plain HTTP form (absolute-URI) dispatches the same way: s2's
	// restricted policy answers, not a challenge.
	res, _ = f.do(f.client(f.endpoint("s2").ProxyURL()), "GET", "http://example.com/plain", nil, "")
	if res == nil || res.StatusCode != 403 {
		t.Fatalf("plain proxied request not dispatched: %v", res)
	}
}

func TestSharedGatewayChallengesMissingOrWrongProxyCredential(t *testing.T) {
	f := newSharedFixture(t)
	f.begin("s1")
	f.openEgress("s1")
	e := f.endpoint("s1")
	wrong := []string{e.BaseURL(), "http://s1:not-the-capability@" + net.JoinHostPort(e.Host, itoa(e.Port)), "http://s2:" + e.Capability + "@" + net.JoinHostPort(e.Host, itoa(e.Port))}
	for _, proxy := range wrong {
		// CONNECT: the raw exchange shows the challenge libcurl needs.
		conn, err := net.Dial("tcp", "127.0.0.1:"+itoa(f.gateway.Port()))
		if err != nil {
			t.Fatal(err)
		}
		u, _ := url.Parse(proxy)
		auth := ""
		if u.User != nil {
			password, _ := u.User.Password()
			auth = "Proxy-Authorization: " + basicCredential(u.User.Username(), password) + "\r\n"
		}
		conn.Write([]byte("CONNECT example.com:443 HTTP/1.1\r\nHost: example.com:443\r\n" + auth + "\r\n"))
		res, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodConnect})
		if err != nil || res.StatusCode != 407 || res.Header.Get("Proxy-Authenticate") != `Basic realm="warden"` {
			t.Fatalf("%s CONNECT: %v %v", proxy, res, err)
		}
		conn.Close()
		// Absolute-URI request through the proxy.
		res2, _ := f.do(f.client(proxy), "GET", "http://example.com/plain", nil, "")
		if res2 == nil || res2.StatusCode != 407 || res2.Header.Get("Proxy-Authenticate") != `Basic realm="warden"` {
			t.Fatalf("%s absolute: %v", proxy, res2)
		}
	}
	if f.dialCount() != 0 || len(f.upstream()) != 0 {
		t.Fatal("a challenged request reached upstream")
	}
	// A CONNECT that presents the credential after the challenge is served.
	res, body := f.do(f.client(e.ProxyURL()), "GET", "https://example.com/after", nil, "")
	if res == nil || res.StatusCode != 200 || !bytes.Contains(body, []byte("seen")) {
		t.Fatalf("credentialed CONNECT: %v %s", res, body)
	}
}

func TestSharedGatewayProviderRouteByBearerPlaceholder(t *testing.T) {
	f := newSharedFixture(t)
	r1, r2 := f.begin("s1"), f.begin("s2")
	direct := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, DialContext: f.dial}}
	origin := "http://127.0.0.1:" + itoa(f.gateway.Port())
	// Origin-form provider request at the advertised host, dispatched by
	// the bearer alone; the upstream sees that binding's provider secret.
	for _, sandbox := range []string{"s1", "s2"} {
		result := map[string]map[string]any{"s1": r1, "s2": r2}[sandbox]
		res, body := f.do(direct, "POST", "http://gateway.test:"+itoa(f.gateway.Port())+"/openai/v1/responses", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + result["apiKeyPlaceholder"].(string)}, `{"input":"synthetic"}`)
		reqs := f.upstream()
		if res == nil || res.StatusCode != 200 || len(reqs) == 0 || reqs[len(reqs)-1].headers.Get("Authorization") != "Bearer synthetic-secret-"+sandbox+"-only" || reqs[len(reqs)-1].host != "api.openai.com" {
			t.Fatalf("%s provider route: %v %s %+v", sandbox, res, body, reqs)
		}
	}
	// Through the proxy, as an agent with HTTP_PROXY set sends it.
	res, body := f.do(f.client(r1["proxyURL"].(string)), "POST", r1["providerBaseURL"].(string)+"/responses", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + r1["apiKeyPlaceholder"].(string)}, `{"input":"synthetic"}`)
	if reqs := f.upstream(); res == nil || res.StatusCode != 200 || reqs[len(reqs)-1].headers.Get("Authorization") != "Bearer synthetic-secret-s1-only" {
		t.Fatalf("proxied provider route: %v %s", res, body)
	}
	_ = origin
	before := len(f.upstream())
	for _, bearer := range []string{LoopbackPlaceholder, "s1.wrong", "s2." + f.endpoint("s1").Capability, ""} {
		headers := map[string]string{"Content-Type": "application/json"}
		if bearer != "" {
			headers["Authorization"] = "Bearer " + bearer
		}
		res, _ := f.do(direct, "POST", origin+"/openai/v1/responses", headers, `{}`)
		if res == nil || res.StatusCode != 401 || res.Header.Get("WWW-Authenticate") == "" {
			t.Fatalf("bearer %q: %v", bearer, res)
		}
	}
	if len(f.upstream()) != before {
		t.Fatal("a wrong bearer reached upstream")
	}
	for _, r := range f.upstream() {
		if strings.Contains(r.headers.Get("Authorization"), ".") || r.headers.Get("Proxy-Authorization") != "" {
			t.Fatalf("binding credential forwarded upstream: %v", r.headers)
		}
	}
}

func TestSharedGatewayConcurrentBindingsStayIsolated(t *testing.T) {
	f := newSharedFixture(t)
	results := map[string]map[string]any{"s1": f.begin("s1"), "s2": f.begin("s2")}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 8; i++ {
		for sandbox, result := range results {
			wg.Add(1)
			go func(sandbox string, result map[string]any) {
				defer wg.Done()
				res, body := f.do(f.client(result["proxyURL"].(string)), "POST", result["providerBaseURL"].(string)+"/responses", map[string]string{"Content-Type": "application/json", "Authorization": "Bearer " + result["apiKeyPlaceholder"].(string), "X-Warden-Test": sandbox}, `{"input":"synthetic"}`)
				if res == nil || res.StatusCode != 200 {
					errs <- sandbox + ": " + string(body)
				}
			}(sandbox, result)
		}
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
	reqs := f.upstream()
	if len(reqs) != 16 {
		t.Fatalf("%d upstream requests", len(reqs))
	}
	for _, r := range reqs {
		if r.headers.Get("Authorization") != "Bearer synthetic-secret-"+r.headers.Get("X-Warden-Test")+"-only" {
			t.Fatalf("request from %s carried %s", r.headers.Get("X-Warden-Test"), r.headers.Get("Authorization"))
		}
	}
	// Rebinding a replaced generation swaps the credential; the old one
	// is refused.
	old := results["s1"]
	replacement := runContext(map[string]any{"sandboxID": "s1", "runtimeName": "sbx-s1", "chatID": "c-s1", "generation": "2"})
	if _, err := f.registry.End(f.values["s1"]); err != nil {
		t.Fatal(err)
	}
	if _, err := f.registry.Register(replacement); err != nil {
		t.Fatal(err)
	}
	f.registry.mu.Lock()
	_, err := f.gateway.Bind(f.registry.Bindings["s1"])
	f.registry.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	f.values["s1"] = replacement
	fresh := f.begin("s1")
	if fresh["proxyURL"] == old["proxyURL"] {
		t.Fatal("replacement generation kept the old credential")
	}
	res, _ := f.do(f.client(old["proxyURL"].(string)), "GET", "http://example.com/plain", nil, "")
	if res == nil || res.StatusCode != 407 {
		t.Fatalf("old credential still admitted: %v", res)
	}
}

func TestSharedGatewayCredentialParsing(t *testing.T) {
	id, cap := parseBasicCredential(basicCredential("proj:sandbox.one", "c-a_p"))
	if id != "proj:sandbox.one" || cap != "c-a_p" {
		t.Fatal(id, cap)
	}
	if id, cap := parseBearerCredential("proj:sandbox.one.c-a_p"); id != "proj:sandbox.one" || cap != "c-a_p" {
		t.Fatal(id, cap)
	}
	for _, bad := range []string{"", "Bearer x", "Basic !!!", "Basic " + basicCredential("", "")[6:], "Basic bm9jb2xvbg=="} {
		if id, cap := parseBasicCredential(bad); id != "" || cap != "" {
			t.Fatalf("%q parsed as %q %q", bad, id, cap)
		}
	}
	if id, cap := parseBearerCredential("nodot"); id != "" || cap != "" {
		t.Fatal("bearer without a dot parsed")
	}
}
