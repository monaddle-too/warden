package policy

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Header sets shared with the Python gateway.
var stripHeaders = stringSet("authorization", "x-figma-token", "x-goog-api-key", "x-goog-user-project", "proxy-authorization", "cookie", "host", "connection", "proxy-connection", "transfer-encoding", "content-length", "keep-alive", "upgrade", "trailer", "te")
var forbidHeaders = stringSet("x-http-method-override", "x-method-override", "x-original-url", "x-rewrite-url", "x-forwarded-host", "forwarded")
var guestCredentialHeaders = []string{"Authorization", "Proxy-Authorization", "Cookie", "OpenAI-Organization", "OpenAI-Project", "ChatGPT-Account-ID", "x-api-key"}
var providerHosts = stringSet("api.openai.com", "chatgpt.com", "api.anthropic.com")
var hopByHop = stringSet("connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade", "trailer", "te")

const requestInspectionLimit = 8388608

// ReviewFunc produces a push review; InspectPush is the default.
type ReviewFunc func(ctx context.Context, repository string, body []byte, auth, upstream string, active func() (bool, error)) (map[string]any, []byte, error)

// GatewayConfig describes one binding's inspected loopback gateway.
type GatewayConfig struct {
	BindingID  string
	Capability string
	Port       int
	Listener   net.Listener
	Control    func(message map[string]any) (map[string]any, error)
	CA         *GatewayCA
	Networks   []*net.IPNet
	Resolve    func(ctx context.Context, host string) ([]net.IP, error)
	Dial       func(ctx context.Context, network, address string) (net.Conn, error)
	Roots      *x509.CertPool
	Review     ReviewFunc
}

// Gateway is the regular-mode explicit proxy with a narrow reverse route
// for providers and documents. There is no guest-selectable identity, raw
// TCP forwarding or TLS passthrough.
type Gateway struct {
	cfg          GatewayConfig
	Redactor     *Redactor
	server       *http.Server
	tunnelServer *http.Server
	reviewSlots  chan struct{}
	mu           sync.Mutex
	reviewCache  map[string]reviewCacheEntry
	reviewOrder  []string
	running      atomic.Bool
	done         chan struct{}
}

type reviewCacheEntry struct {
	expires time.Time
	review  map[string]any
	body    []byte
}

type tunnelInfo struct {
	host string
	sni  string
}

type tunnelKeyType struct{}

var tunnelKey tunnelKeyType

// LoadGitHubNetworks reads vendor/github-meta.json CIDR lists.
func LoadGitHubNetworks(path string) ([]*net.IPNet, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var data map[string]any
	if err = json.Unmarshal(raw, &data); err != nil {
		return nil, err
	}
	var networks []*net.IPNet
	for _, values := range data {
		list, ok := values.([]any)
		if !ok {
			continue
		}
		for _, value := range list {
			s, ok := value.(string)
			if !ok {
				continue
			}
			if _, network, err := net.ParseCIDR(s); err == nil {
				networks = append(networks, network)
			} else if ip := net.ParseIP(s); ip != nil {
				bits := 32
				if ip.To4() == nil {
					bits = 128
				}
				networks = append(networks, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			}
		}
	}
	return networks, nil
}

// NewGateway prepares a gateway on a pre-bound loopback listener.
func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	if cfg.Listener == nil || cfg.Control == nil || cfg.CA == nil || cfg.Port == 0 {
		return nil, errors.New("gateway configuration incomplete")
	}
	if cfg.Resolve == nil {
		cfg.Resolve = func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip", host)
		}
	}
	if cfg.Dial == nil {
		cfg.Dial = (&net.Dialer{Timeout: 30 * time.Second}).DialContext
	}
	if cfg.Review == nil {
		cfg.Review = InspectPush
	}
	g := &Gateway{cfg: cfg, Redactor: NewRedactor(), reviewSlots: make(chan struct{}, 2), reviewCache: map[string]reviewCacheEntry{}, done: make(chan struct{})}
	g.server = &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 20, ErrorLog: silentLogger(), TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	g.tunnelServer = &http.Server{Handler: g, ReadHeaderTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 20, ErrorLog: silentLogger(), TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){},
		ConnContext: func(ctx context.Context, c net.Conn) context.Context {
			if tc, ok := c.(*tunnelConn); ok {
				return context.WithValue(ctx, tunnelKey, &tunnelInfo{host: tc.host, sni: tc.Conn.(*tls.Conn).ConnectionState().ServerName})
			}
			return ctx
		}}
	return g, nil
}

// Start serves the listener in the background.
func (g *Gateway) Start() {
	g.running.Store(true)
	go func() {
		defer close(g.done)
		_ = g.server.Serve(g.cfg.Listener)
		g.running.Store(false)
	}()
}

// Running reports whether the listener is still served.
func (g *Gateway) Running() bool { return g.running.Load() }

// Stop closes the listener and active connections.
func (g *Gateway) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = g.server.Shutdown(ctx)
	_ = g.server.Close()
	_ = g.tunnelServer.Close()
	select {
	case <-g.done:
	case <-time.After(3 * time.Second):
	}
	g.running.Store(false)
}

// Port returns the loopback port.
func (g *Gateway) Port() int { return g.cfg.Port }

// flow is the per-request state of the inspected exchange.
type flow struct {
	g             *Gateway
	w             http.ResponseWriter
	r             *http.Request
	ctx           context.Context
	cancel        context.CancelFunc
	tunnel        *tunnelInfo
	requestID     string
	decisionID    string
	remaining     float64
	denied        bool
	wroteHeader   bool
	scheme        string
	host          string
	port          int
	sni           string
	method        string
	path          string
	httpVersion   string
	headers       [][]string // incoming pairs in order, Host first
	body          []byte
	git           bool
	authorization string // injected credential, cleared after delivery
	upstreamIP    string
	killed        atomic.Bool
	watchStop     chan struct{}
	stream        *ResponseStream
	streaming     bool
	accountHeader bool
}

// ServeHTTP handles direct proxy requests and requests inside tunnels.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tunnel, _ := r.Context().Value(tunnelKey).(*tunnelInfo)
	if r.Method == http.MethodConnect && tunnel == nil {
		g.handleConnect(w, r)
		return
	}
	ctx, cancel := context.WithCancel(r.Context())
	f := &flow{g: g, w: w, r: r, ctx: ctx, cancel: cancel, tunnel: tunnel, watchStop: make(chan struct{})}
	defer f.cleanup()
	f.handle()
}

func (f *flow) cleanup() {
	f.cancel()
	f.stopWatch()
	if f.stream != nil {
		f.stream.Clear()
	}
	f.authorization = ""
	if f.decisionID != "" {
		_, _ = f.g.cfg.Control(map[string]any{"action": "egress.finish", "decision_id": f.decisionID})
	}
}

func (f *flow) stopWatch() {
	select {
	case <-f.watchStop:
	default:
		close(f.watchStop)
	}
}

func (f *flow) control(message map[string]any) (map[string]any, error) {
	if message["action"] == "egress" {
		request, _ := message["request"].(map[string]any)
		copied := map[string]any{}
		for k, v := range request {
			copied[k] = v
		}
		copied["path"] = f.path
		message = map[string]any{"action": "egress", "request": copied}
	}
	result, err := f.g.cfg.Control(message)
	if err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("control request rejected")
	}
	return result, nil
}

func silentLogger() *log.Logger { return log.New(io.Discard, "", 0) }

// ----- denial and audit -----

func (f *flow) deny(reason string, status int, requestID string) {
	if requestID == "" {
		requestID = f.requestID
	}
	if requestID == "" {
		requestID = UUID4()
	}
	f.requestID = requestID
	f.denied = true
	f.authorization = ""
	message := map[string]any{"error": reason, "request_id": requestID}
	if status == 428 {
		message["approval"] = "Use the Warden control panel on the host. Retry after approval."
	}
	if !f.wroteHeader {
		h := f.w.Header()
		for key := range h {
			delete(h, key)
		}
		h.Set("Content-Type", "application/json")
		h.Set("Cache-Control", "no-store")
		payload := []byte(Dumps(message))
		h.Set("Content-Length", strconv.Itoa(len(payload)))
		f.w.WriteHeader(status)
		f.wroteHeader = true
		_, _ = f.w.Write(payload)
	}
	// Record early validation failures too, without retaining credentials
	// or reflecting arbitrary malformed header values into the audit.
	names := []any{}
	for _, pair := range f.headers {
		names = append(names, f.g.Redactor.Text(pair[0]))
	}
	query := ""
	if strings.Contains(f.path, "?") {
		query = "[REDACTED]"
	}
	_, _ = f.g.cfg.Control(map[string]any{"action": "event", "event_type": "proxy.error", "fields": map[string]any{
		"request_id": requestID, "status": status, "reason": f.g.Redactor.Text(reason), "hostname": f.sni,
		"request": map[string]any{"method": f.method, "host": f.host, "path": f.g.Redactor.Text(strings.SplitN(f.path, "?", 2)[0]),
			"query": query, "http_version": f.httpVersion, "header_names": names}}})
}

func (f *flow) audit(eventType string, fields map[string]any) error {
	result, err := f.control(map[string]any{"action": "event", "event_type": eventType, "fields": fields})
	if err != nil {
		return err
	}
	if recorded, _ := result["recorded"].(bool); !recorded {
		return errors.New("audit rejected")
	}
	return nil
}

func (f *flow) abort() {
	f.killed.Store(true)
	f.cancel()
	panic(http.ErrAbortHandler)
}

// ----- CONNECT -----

func (g *Gateway) handleConnect(w http.ResponseWriter, r *http.Request) {
	f := &flow{g: g, w: w, r: r, method: r.Method, httpVersion: r.Proto, path: r.RequestURI, watchStop: make(chan struct{})}
	f.ctx, f.cancel = context.WithCancel(r.Context())
	defer f.cancel()
	authority := r.Host
	if authority == "" {
		authority = r.URL.Host
	}
	host, port, err := net.SplitHostPort(authority)
	host = strings.ToLower(host)
	f.host = host
	if err != nil || !ValidHost(host) || port != "443" || r.Header.Get("Origin") != "" {
		f.deny("only inspected HTTPS CONNECT is supported", 403, "")
		return
	}
	decision, err := f.control(map[string]any{"action": "destination", "request": map[string]any{"host": host, "method": "GET", "scheme": "https"}})
	if err != nil {
		f.deny("gateway control unavailable", 503, "")
		return
	}
	if allow, _ := decision["allow"].(bool); !allow {
		f.deny("destination or active lease denied", 403, "")
		return
	}
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		f.deny("gateway control unavailable", 503, "")
		return
	}
	defer conn.Close()
	if _, err = conn.Write([]byte("HTTP/1.1 200 Connection established\r\n\r\n")); err != nil {
		return
	}
	// The regular-mode CONNECT hook does not dispatch a TCP connection
	// (lazy connection strategy). Each inner request still passes the
	// complete request checks, including SNI/authority agreement.
	tlsConn := tls.Server(conn, &tls.Config{
		MinVersion: tls.VersionTLS12,
		NextProtos: []string{"http/1.1"},
		GetCertificate: func(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
			name := hello.ServerName
			if name == "" {
				name = host
			}
			return g.cfg.CA.Certificate(name)
		},
	})
	handshakeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	err = tlsConn.HandshakeContext(handshakeCtx)
	cancel()
	if err != nil {
		_, _ = g.cfg.Control(map[string]any{"action": "event", "event_type": "proxy.error", "fields": map[string]any{
			"hostname": host, "reason": "TLS client handshake failed: " + truncate(g.Redactor.Text(err.Error()), 1024)}})
		return
	}
	listener := newSingleListener(&tunnelConn{Conn: tlsConn, host: host})
	_ = g.tunnelServer.Serve(listener)
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

type tunnelConn struct {
	net.Conn
	host   string
	once   sync.Once
	closed chan struct{}
}

func (c *tunnelConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

type singleListener struct {
	conn    *tunnelConn
	handed  bool
	mu      sync.Mutex
	closeCh chan struct{}
	once    sync.Once
}

func newSingleListener(conn *tunnelConn) *singleListener {
	conn.closed = make(chan struct{})
	return &singleListener{conn: conn, closeCh: make(chan struct{})}
}

func (l *singleListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.handed {
		l.handed = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	select {
	case <-l.conn.closed:
	case <-l.closeCh:
	}
	return nil, net.ErrClosed
}

func (l *singleListener) Close() error {
	l.once.Do(func() { close(l.closeCh) })
	return nil
}

func (l *singleListener) Addr() net.Addr { return l.conn.LocalAddr() }

// ----- request handling -----

func (f *flow) handle() {
	r := f.r
	f.method = r.Method
	f.httpVersion = r.Proto
	authority := r.Host
	if f.tunnel != nil {
		f.scheme = "https"
		f.sni = f.tunnel.sni
	} else if r.URL.IsAbs() {
		f.scheme = strings.ToLower(r.URL.Scheme)
	} else {
		f.scheme = "http"
	}
	f.path = r.URL.EscapedPath()
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		f.path += "?" + r.URL.RawQuery
	}
	if strings.HasPrefix(r.RequestURI, "/") && f.path == "" {
		f.path = r.RequestURI
	}
	host, port := splitAuthority(authority, f.scheme)
	f.host, f.port = host, port
	f.headers = append(f.headers, []string{"Host", authority})
	for key, values := range r.Header {
		for _, value := range values {
			f.headers = append(f.headers, []string{key, value})
		}
	}
	// requestheaders: authorization and inspection must see the complete request.
	if r.Header.Get("Origin") != "" {
		f.deny("browser origins are not allowed at the gateway", 403, "")
		return
	}
	if strings.HasPrefix(f.path, DocumentPrefix) && r.ContentLength > DocumentMaxBody {
		f.deny("document request exceeds limit", 413, "")
		return
	}
	if r.Header.Get("Upgrade") != "" || r.Method == http.MethodConnect || r.Method == http.MethodTrace {
		f.deny("tunnels and protocol upgrades are unsupported", 403, "")
		return
	}
	if r.ContentLength > requestInspectionLimit {
		f.deny("request exceeds inspection limit", 413, "")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, requestInspectionLimit+1))
	if err != nil {
		f.deny("invalid content length", 403, "")
		return
	}
	f.body = body
	if len(body) > requestInspectionLimit {
		f.deny("request exceeds inspection limit", 403, "")
		return
	}
	// Liveness must not recursively wait for the broker's lock.
	if r.Method == http.MethodGet && f.host == "127.0.0.1" && f.port == f.g.cfg.Port && strings.HasPrefix(f.path, "/__warden_sbx_health/") {
		nonce := strings.TrimPrefix(f.path, "/__warden_sbx_health/")
		if len(nonce) == 64 && isHex(nonce) {
			mac := hmac.New(sha256.New, []byte(f.g.cfg.Capability))
			mac.Write([]byte(nonce))
			f.respondJSON(200, map[string]any{"bindingID": f.g.cfg.BindingID, "mac": hex.EncodeToString(mac.Sum(nil))})
			return
		}
	}
	reverse := f.host == "host.docker.internal" || f.host == "localhost" || f.host == "127.0.0.1"
	if reverse && strings.HasPrefix(f.path, DocumentPrefix) {
		f.documentRequest()
		return
	}
	if reverse {
		prefix := "/openai"
		if strings.HasPrefix(f.path, "/anthropic/") {
			prefix = "/anthropic"
		}
		apiPath := strings.TrimPrefix(f.path, prefix)
		if prefix == "/anthropic" {
			apiPath = strings.TrimSuffix(apiPath, "?beta=true")
		}
		// A host alias only selects this exact provider; it is not a target
		// override or arbitrary reverse proxy. No cookies/guest auth survive.
		if f.scheme != "http" || f.port != f.g.cfg.Port || !strings.HasPrefix(f.path, prefix+"/") || !ProviderRoutes[apiPath] || r.Method != http.MethodPost {
			f.deny("unsupported provider route", 403, "")
			return
		}
		route, err := f.control(map[string]any{"action": "providerRoute", "path": apiPath})
		if err != nil {
			f.deny("gateway control unavailable", 503, "")
			return
		}
		routeHost, _ := route["host"].(string)
		routePath, _ := route["path"].(string)
		if allow, _ := route["allow"].(bool); !allow || !providerRouteTargets[[2]string{routeHost, routePath}] {
			f.deny("provider route unavailable", 503, "")
			return
		}
		f.path, f.scheme, f.host, f.port = routePath, "https", routeHost, 443
		f.setHeader("Host", routeHost)
		// The trusted reverse mapping fixes the upstream TLS authority. This
		// value is not guest TLS evidence; ordinary explicit flows retain
		// their actual SNI and are checked below.
		f.sni = routeHost
	}
	if providerHosts[f.host] || f.host == "docs.googleapis.com" {
		for _, name := range guestCredentialHeaders {
			f.removeHeader(name)
		}
	}
	decision, err := f.control(map[string]any{"action": "destination", "request": map[string]any{"host": f.host, "method": f.method, "scheme": f.scheme}})
	if err != nil {
		f.deny("gateway control unavailable", 503, "")
		return
	}
	if allow, _ := decision["allow"].(bool); !allow {
		f.deny("destination or active lease denied", 403, "")
		return
	}
	if !f.guardRequest() {
		return
	}
	if providerHosts[f.host] {
		result, err := f.control(map[string]any{"action": "provider", "decision_id": f.decisionID,
			"request": map[string]any{"host": f.host, "method": f.method, "scheme": f.scheme, "path": f.path}})
		if err != nil {
			f.deny("gateway control unavailable", 503, "")
			return
		}
		if allow, _ := result["allow"].(bool); !allow {
			f.deny("provider credential unavailable", 503, "")
			return
		}
		headers, _ := result["headers"].(map[string]any)
		auth, _ := headers["Authorization"].(string)
		if !strings.HasPrefix(auth, "Bearer ") {
			f.deny("invalid provider authorization", 503, "")
			return
		}
		f.g.Redactor.Register(strings.TrimPrefix(auth, "Bearer "))
		for key, value := range headers {
			text, ok := value.(string)
			if (key != "Authorization" && key != "ChatGPT-Account-ID") || !ok {
				f.deny("gateway control unavailable", 503, "")
				return
			}
			f.g.Redactor.Register(text)
			f.setHeader(key, text)
			if key == "ChatGPT-Account-ID" {
				f.accountHeader = true
			}
		}
		f.authorization = auth
	}
	f.dispatch()
}

func splitAuthority(authority, scheme string) (string, int) {
	host := authority
	port := 80
	if scheme == "https" {
		port = 443
	}
	if h, p, err := net.SplitHostPort(authority); err == nil {
		host = h
		if n, err := strconv.Atoi(p); err == nil {
			port = n
		} else {
			port = -1
		}
	}
	return strings.TrimRight(strings.ToLower(host), "."), port
}

func isHex(s string) bool {
	_, err := hex.DecodeString(s)
	return err == nil
}

func (f *flow) setHeader(name, value string) {
	f.removeHeader(name)
	f.headers = append(f.headers, []string{name, value})
}

func (f *flow) removeHeader(name string) {
	kept := f.headers[:0]
	for _, pair := range f.headers {
		if !strings.EqualFold(pair[0], name) {
			kept = append(kept, pair)
		}
	}
	f.headers = kept
}

func (f *flow) header(name string) (string, bool) {
	for _, pair := range f.headers {
		if strings.EqualFold(pair[0], name) {
			return pair[1], true
		}
	}
	return "", false
}

func (f *flow) respondJSON(status int, value map[string]any) {
	payload := []byte(Dumps(value))
	h := f.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	f.w.WriteHeader(status)
	f.wroteHeader = true
	_, _ = f.w.Write(payload)
}

// documentRequest brokers a guest document call; the broker signs it and
// the app's answer is delivered without any guest header surviving.
func (f *flow) documentRequest() {
	r := f.r
	encoding := strings.ToLower(r.Header.Get("Content-Encoding"))
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(r.Header.Get("Content-Type"), ";", 2)[0]))
	if f.scheme != "http" || f.port != f.g.cfg.Port || (encoding != "" && encoding != "identity") || len(f.body) > DocumentMaxBody || (r.Method == http.MethodPost && contentType != "application/json") {
		f.respondJSON(503, map[string]any{"error": "document gateway unavailable"})
		return
	}
	// A fresh header set also removes any forged Warden identity,
	// authentication, forwarding headers and browser cookies from flows.
	f.headers = nil
	result, err := f.g.cfg.Control(map[string]any{"action": "document", "method": r.Method, "path": f.path, "body": base64.StdEncoding.EncodeToString(f.body)})
	if err != nil || result == nil {
		f.respondJSON(503, map[string]any{"error": "document gateway unavailable"})
		return
	}
	if allow, _ := result["allow"].(bool); !allow {
		status, ok := asInt(result["status"])
		if !ok {
			status = 503
		}
		f.respondJSON(int(status), map[string]any{"error": "document access unavailable"})
		return
	}
	status, ok := asInt(result["status"])
	encoded, _ := result["body"].(string)
	payload, decodeErr := base64.StdEncoding.Strict().DecodeString(encoded)
	if !ok || decodeErr != nil {
		f.respondJSON(503, map[string]any{"error": "document gateway unavailable"})
		return
	}
	h := f.w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	f.w.WriteHeader(int(status))
	f.wroteHeader = true
	_, _ = f.w.Write(payload)
}

// guardRequest is the shared inspection: canonical host, transport, SNI,
// header hygiene, DNS validation, credential brokering and egress policy.
// It returns false when the flow was denied.
func (f *flow) guardRequest() bool {
	fail := func(err error) bool {
		// Fail closed on unavailable policy/audit service or malformed input.
		if isValueError(err) {
			f.deny(err.Error(), 403, "")
		} else {
			f.deny("inspection or control plane unavailable", 503, "")
		}
		return false
	}
	authority, _ := f.header("Host")
	host := strings.TrimRight(strings.ToLower(strings.SplitN(authority, ":", 2)[0]), ".")
	// Restrict to canonical DNS names; IP literals, HTTP authority tricks,
	// private routes and client-selected upstreams never get passthrough.
	if !ValidHost(host) {
		return fail(valueErr("a canonical public DNS hostname is required"))
	}
	if (f.scheme != "http" && f.scheme != "https") || (f.port != 80 && f.port != 443) || (f.scheme == "https") != (f.port == 443) {
		return fail(valueErr("unsupported transport"))
	}
	lowered := strings.TrimRight(strings.ToLower(authority), ".")
	if lowered != host && lowered != host+":"+strconv.Itoa(f.port) {
		return fail(valueErr("HTTP authority mismatch"))
	}
	if f.scheme == "https" && (f.sni == "" || strings.TrimRight(strings.ToLower(f.sni), ".") != host) {
		return fail(valueErr("TLS SNI and HTTP authority must agree"))
	}
	seen := map[string]bool{}
	for _, pair := range f.headers {
		key := strings.ToLower(pair[0])
		if forbidHeaders[key] {
			return fail(valueErr("HTTP routing overrides forbidden"))
		}
		if seen[key] {
			return fail(valueErr("duplicate headers unsupported"))
		}
		seen[key] = true
	}
	resolveCtx, cancel := context.WithTimeout(f.ctx, 10*time.Second)
	addresses, err := f.g.cfg.Resolve(resolveCtx, host)
	cancel()
	if err != nil {
		return fail(err)
	}
	if len(addresses) == 0 {
		return fail(valueErr("private or special-use destination blocked"))
	}
	protected := GitHubHost(host) || FigmaProtectedHost(host) || GoogleProtectedHost(host)
	upstream := ""
	for _, address := range addresses {
		if !isGlobalIP(address) {
			return fail(valueErr("private or special-use destination blocked"))
		}
		for _, network := range f.g.cfg.Networks {
			if network.Contains(address) {
				protected = true
			}
		}
	}
	for _, address := range addresses {
		if v4 := address.To4(); v4 != nil && upstream == "" {
			upstream = v4.String()
		}
	}
	// A client can send to any destination IP; upstream always goes to a
	// freshly resolved, validated address for the inspected authority.
	if upstream == "" {
		return fail(valueErr("IPv6-only upstream unsupported"))
	}
	f.upstreamIP = upstream
	f.host = host
	if len(f.body) > requestInspectionLimit {
		return fail(valueErr("request exceeds inspection limit"))
	}
	f.requestID = UUID4()
	if protected {
		// All guest credentials are discarded, including hop-by-hop
		// nominated headers. GitHub only sees the brokered identity.
		var filtered [][]string
		for _, pair := range f.headers {
			if !stripHeaders[strings.ToLower(pair[0])] {
				filtered = append(filtered, pair)
			}
		}
		var git *GitInspection
		if host == "github.com" && GitRouteFor(f.method, f.path) != nil {
			git, err = GitInspect(f.method, f.path, filtered, f.body)
			if err != nil {
				return fail(valueErr(err.Error()))
			}
		}
		var review map[string]any
		body := f.body
		// In open egress mode a brokered host with no grant is still
		// reachable, anonymously: the guest's own headers were stripped
		// above and nothing is injected. The request is then an ordinary
		// external one, subject to the same egress decision and audit.
		// forwarded: proceed upstream; denied: the flow was already
		// refused (audit failure); neither: apply the brokered denial.
		anonymous := func() (forwarded, denied bool) {
			decision, err := f.egressDecision(host)
			if err != nil || !allowed(decision) {
				return false, false
			}
			if !f.forwardExternal(host, filtered, decision, true) {
				return false, true
			}
			f.git = git != nil
			return true, false
		}
		if git != nil && git.Write {
			// Reading upstream objects is separately authorized. A read
			// session cannot dispatch a receive-pack request.
			read, err := f.control(map[string]any{"action": "authorize", "request": map[string]any{"method": "GET", "host": host,
				"path": "/" + git.Repository + ".git/info/refs?service=git-upload-pack", "scheme": f.scheme, "port": f.port, "headers": []any{}}})
			if err != nil {
				return fail(err)
			}
			if allow, _ := read["allow"].(bool); !allow {
				if forwarded, denied := anonymous(); forwarded || denied {
					return forwarded
				}
				reason, _ := read["reason"].(string)
				if reason == "" {
					reason = "repository read permission required"
				}
				status, ok := asInt(read["status"])
				if !ok {
					status = 403
				}
				id, _ := read["request_id"].(string)
				f.deny(reason, int(status), id)
				return false
			}
			readDecision, _ := read["decision_id"].(string)
			active := func() (bool, error) {
				result, err := f.control(map[string]any{"action": "active", "decision_id": readDecision})
				if err != nil {
					return false, err
				}
				a, _ := result["active"].(bool)
				return a, nil
			}
			cacheKey := git.Repository + "\x00" + sha256Hex(body)
			cached, hit := f.g.cachedReview(cacheKey)
			if hit {
				review, body = cached.review, cached.body
				ok, err := active()
				if err != nil {
					return fail(err)
				}
				if !ok {
					return fail(valueErr("repository read permission expired"))
				}
			} else {
				select {
				case f.g.reviewSlots <- struct{}{}:
				case <-time.After(5 * time.Second):
					return fail(errors.New("review slots exhausted"))
				}
				token, _ := read["authorization"].(string)
				credential := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+strings.TrimPrefix(token, "Bearer ")))
				review, body, err = f.g.cfg.Review(f.ctx, git.Repository, body, credential, upstream, active)
				<-f.g.reviewSlots
				if err != nil {
					return fail(err)
				}
				if len(body) > requestInspectionLimit {
					return fail(valueErr("canonical push exceeds 8 MiB; split the change"))
				}
				f.g.storeReview(cacheKey, review, body)
			}
		}
		request := map[string]any{"method": f.method, "host": host, "path": f.path, "scheme": f.scheme, "port": f.port, "headers": pairsToAny(filtered), "body_base64": base64.StdEncoding.EncodeToString(body)}
		if review != nil {
			request["git_review"] = review
		}
		decision, err := f.control(map[string]any{"action": "authorize", "request": request})
		if err != nil {
			return fail(err)
		}
		if id, _ := decision["request_id"].(string); id != "" {
			f.requestID = id
		}
		if allow, _ := decision["allow"].(bool); !allow {
			if forwarded, denied := anonymous(); forwarded || denied {
				return forwarded
			}
			reason, _ := decision["reason"].(string)
			if reason == "" {
				reason = "denied"
			}
			status, ok := asInt(decision["status"])
			if !ok {
				status = 403
			}
			id, _ := decision["request_id"].(string)
			f.deny(reason, int(status), id)
			return false
		}
		// Upstream destination is fixed to a supported GitHub authority; no
		// redirects are followed and no alternate-host injection.
		f.headers = filtered
		f.setHeader("Host", host)
		authorization, _ := decision["authorization"].(string)
		outgoing := authorization
		if git != nil {
			outgoing = "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+strings.TrimPrefix(authorization, "Bearer ")))
		}
		f.setHeader("Authorization", outgoing)
		f.authorization = outgoing
		f.g.Redactor.Register(outgoing)
		if parts := strings.SplitN(authorization, " ", 2); len(parts) == 2 {
			f.g.Redactor.Register(parts[1])
		}
		if _, ok := f.header("User-Agent"); !ok {
			f.setHeader("User-Agent", "Warden/0.1")
		}
		f.body = body
		f.git = git != nil
		f.decisionID, _ = decision["decision_id"].(string)
		f.remaining, _ = asNumber(decision["remaining_seconds"])
		activeNow, err := f.control(map[string]any{"action": "active", "decision_id": f.decisionID})
		if err != nil {
			return fail(err)
		}
		if a, _ := activeNow["active"].(bool); !a {
			f.deny("permission expired before dispatch", 403, "")
			return false
		}
		f.startWatch()
	} else {
		decision, err := f.egressDecision(host)
		if err != nil {
			return fail(err)
		}
		if !allowed(decision) {
			reason, _ := decision["reason"].(string)
			if reason == "" {
				reason = "destination denied"
			}
			f.deny(reason, 403, "")
			return false
		}
		if !f.forwardExternal(host, f.headers, decision, false) {
			return false
		}
	}
	if f.providerRequest() {
		f.setHeader("Accept-Encoding", "identity")
	}
	return true
}

func allowed(decision map[string]any) bool {
	allow, _ := decision["allow"].(bool)
	return allow
}

// egressDecision asks the destination policy about an ordinary
// (unbrokered) request to host.
func (f *flow) egressDecision(host string) (map[string]any, error) {
	return f.control(map[string]any{"action": "egress", "request": map[string]any{"host": host, "method": f.method, "scheme": f.scheme}})
}

// forwardExternal prepares an allowed external request: the given headers,
// the decision's lease and watch, and the audit entry. anonymous marks a
// brokered host reached without a grant in open egress mode. It denies the
// flow and returns false only when the audit entry cannot be written.
func (f *flow) forwardExternal(host string, headers [][]string, decision map[string]any, anonymous bool) bool {
	f.headers = headers
	f.setHeader("Host", host)
	f.decisionID, _ = decision["decision_id"].(string)
	f.remaining, _ = asNumber(decision["remaining_seconds"])
	f.startWatch()
	// Other sites retain their credentials for ordinary application
	// login, but logs contain only their scrubbed representations.
	query := ""
	if strings.Contains(f.path, "?") {
		query = "[REDACTED]"
	}
	summary := map[string]any{"method": f.method, "host": host, "path": f.g.Redactor.Text(strings.SplitN(f.path, "?", 2)[0]), "query": query,
		"headers": pairsToAny(f.g.Redactor.Headers(f.headers)), "body": f.g.Redactor.Body(len(f.body))}
	if anonymous {
		summary["anonymous"] = true
	}
	if err := f.audit("http.request.external", map[string]any{"request_id": f.requestID, "request": summary}); err != nil {
		if isValueError(err) {
			f.deny(err.Error(), 403, "")
		} else {
			f.deny("inspection or control plane unavailable", 503, "")
		}
		return false
	}
	return true
}

func (f *flow) providerRequest() bool {
	return f.scheme == "https" && f.port == 443 && f.method == http.MethodPost && ProviderEndpoints[[2]string{f.host, f.path}]
}

func (g *Gateway) cachedReview(key string) (reviewCacheEntry, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneReviewsLocked()
	entry, ok := g.reviewCache[key]
	return entry, ok
}

func (g *Gateway) storeReview(key string, review map[string]any, body []byte) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.pruneReviewsLocked()
	if len(g.reviewCache) >= 4 && len(g.reviewOrder) > 0 {
		delete(g.reviewCache, g.reviewOrder[0])
		g.reviewOrder = g.reviewOrder[1:]
	}
	if _, exists := g.reviewCache[key]; !exists {
		g.reviewOrder = append(g.reviewOrder, key)
	}
	g.reviewCache[key] = reviewCacheEntry{expires: time.Now().Add(120 * time.Second), review: review, body: body}
}

func (g *Gateway) pruneReviewsLocked() {
	now := time.Now()
	kept := g.reviewOrder[:0]
	for _, key := range g.reviewOrder {
		if entry, ok := g.reviewCache[key]; ok && entry.expires.After(now) {
			kept = append(kept, key)
		} else {
			delete(g.reviewCache, key)
		}
	}
	g.reviewOrder = kept
}

// startWatch polls the decision and kills the flow when it lapses.
func (f *flow) startWatch() {
	remaining := f.remaining
	if remaining > 3600 || remaining <= 0 {
		if remaining <= 0 {
			remaining = 0
		} else {
			remaining = 3600
		}
	}
	deadline := time.Now().Add(time.Duration(remaining * float64(time.Second)))
	decision := f.decisionID
	go func() {
		for {
			wait := 500 * time.Millisecond
			if until := time.Until(deadline); until < wait {
				wait = until
				if wait < 10*time.Millisecond {
					wait = 10 * time.Millisecond
				}
			}
			select {
			case <-f.watchStop:
				return
			case <-time.After(wait):
			}
			expired := !time.Now().Before(deadline)
			active := false
			if !expired {
				result, err := f.g.cfg.Control(map[string]any{"action": "active", "decision_id": decision})
				if err == nil {
					active, _ = result["active"].(bool)
				}
			}
			if expired || !active {
				f.killed.Store(true)
				f.cancel()
				return
			}
		}
	}()
}

var nonGlobalV4 = mustCIDRs("0.0.0.0/8", "10.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "172.16.0.0/12", "192.0.0.0/24", "192.0.2.0/24", "192.168.0.0/16", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4")
var nonGlobalV6 = mustCIDRs("::/128", "::1/128", "::ffff:0:0/96", "64:ff9b:1::/48", "100::/64", "2001::/23", "2001:db8::/32", "2002::/16", "fc00::/7", "fe80::/10", "ff00::/8")

func mustCIDRs(values ...string) []netip.Prefix {
	out := make([]netip.Prefix, 0, len(values))
	for _, v := range values {
		out = append(out, netip.MustParsePrefix(v))
	}
	return out
}

// isGlobalIP mirrors ipaddress.ip_address(...).is_global conservatively.
func isGlobalIP(ip net.IP) bool {
	address, ok := netip.AddrFromSlice(ip)
	if !ok {
		return false
	}
	address = address.Unmap()
	prefixes := nonGlobalV6
	if address.Is4() {
		prefixes = nonGlobalV4
	}
	for _, prefix := range prefixes {
		if prefix.Contains(address) {
			return false
		}
	}
	return true
}

// ----- upstream dispatch and response delivery -----

func (f *flow) transport() *http.Transport {
	pinned := net.JoinHostPort(f.upstreamIP, strconv.Itoa(f.port))
	return &http.Transport{
		Proxy: nil,
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return f.g.cfg.Dial(ctx, "tcp4", pinned)
		},
		TLSClientConfig:     &tls.Config{ServerName: f.host, RootCAs: f.g.cfg.Roots, MinVersion: tls.VersionTLS12},
		TLSHandshakeTimeout: 30 * time.Second,
		DisableCompression:  true,
		DisableKeepAlives:   true,
		ForceAttemptHTTP2:   false,
		TLSNextProto:        map[string]func(string, *tls.Conn) http.RoundTripper{},
	}
}

func (f *flow) dispatch() {
	target := f.scheme + "://" + f.host + f.path
	if (f.scheme == "https" && f.port != 443) || (f.scheme == "http" && f.port != 80) {
		target = f.scheme + "://" + net.JoinHostPort(f.host, strconv.Itoa(f.port)) + f.path
	}
	var body io.Reader
	if len(f.body) > 0 || (f.method != http.MethodGet && f.method != http.MethodHead) {
		body = bytes.NewReader(f.body)
	}
	req, err := http.NewRequestWithContext(f.ctx, f.method, target, body)
	if err != nil {
		f.deny("inspection or control plane unavailable", 503, "")
		return
	}
	req.Host = f.host
	if body != nil {
		req.ContentLength = int64(len(f.body))
	}
	for _, pair := range f.headers {
		key := strings.ToLower(pair[0])
		if key == "host" || key == "content-length" || key == "proxy-connection" || key == "proxy-authorization" || key == "transfer-encoding" || key == "connection" || key == "keep-alive" {
			continue
		}
		req.Header.Add(pair[0], pair[1])
	}
	transport := f.transport()
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := client.Do(req)
	if err != nil {
		f.transportError(err)
		return
	}
	defer res.Body.Close()
	f.response(res)
}

func (f *flow) transportError(err error) {
	if f.killed.Load() {
		f.interrupted("permission expired or revoked")
		return
	}
	f.authorization = ""
	_, _ = f.g.cfg.Control(map[string]any{"action": "event", "event_type": "proxy.error", "fields": map[string]any{
		"request_id": f.requestID, "hostname": f.sni, "reason": "upstream or transport error: " + truncate(f.g.Redactor.Text(err.Error()), 1024)}})
	if !f.wroteHeader {
		f.respondJSON(502, map[string]any{"error": "upstream or transport error", "request_id": f.requestID})
	}
}

func (f *flow) interrupted(reason string) {
	_ = f.audit("request.interrupted", map[string]any{"request_id": f.requestID, "decision_id": f.decisionID, "reason": reason})
	f.abort()
}

func (f *flow) response(res *http.Response) {
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(res.Header.Get("Content-Type"), ";", 2)[0]))
	// The Codex backend can omit Content-Type on streaming responses.
	// Restrict inference to that exact route and an explicit JSON stream flag.
	codexStream := false
	if contentType == "" && f.providerRequest() && f.host == "chatgpt.com" {
		var payload map[string]any
		if json.Unmarshal(f.body, &payload) == nil {
			codexStream = payload["stream"] == true
		}
	}
	candidate := f.providerRequest() && res.StatusCode >= 200 && res.StatusCode < 300 && (contentType == "text/event-stream" || codexStream)
	limit := 16 * 1024 * 1024
	if f.git {
		limit = 128 * 1024 * 1024
	}
	if res.ContentLength > int64(limit) {
		f.deny("response exceeds inspection limit", 413, "")
		return
	}
	if !candidate {
		f.bufferedResponse(res, limit)
		return
	}
	f.streamResponse(res)
}

func headerBytes(h http.Header) []byte {
	var b bytes.Buffer
	for key, values := range h {
		for _, value := range values {
			b.WriteString(key + ": " + value + "\n")
		}
	}
	return b.Bytes()
}

func (f *flow) headerPairs(h http.Header) [][]string {
	var pairs [][]string
	for key, values := range h {
		for _, value := range values {
			pairs = append(pairs, []string{key, value})
		}
	}
	return pairs
}

func (f *flow) bufferedResponse(res *http.Response, limit int) {
	payload, err := io.ReadAll(io.LimitReader(res.Body, int64(limit)+1))
	if err != nil {
		f.transportError(err)
		return
	}
	if f.killed.Load() {
		f.interrupted("permission expired or revoked")
		return
	}
	if providerHosts[f.host] {
		headers := headerBytes(res.Header)
		for _, secret := range f.g.Redactor.Secrets() {
			if bytes.Contains(payload, []byte(secret)) || bytes.Contains(headers, []byte(secret)) {
				f.deny("provider response exposed a protected credential", 502, "")
				return
			}
		}
	}
	if f.decisionID != "" {
		result, err := f.control(map[string]any{"action": "active", "decision_id": f.decisionID})
		if err != nil {
			f.deny("audit unavailable; response withheld", 503, "")
			return
		}
		if active, _ := result["active"].(bool); !active {
			f.deny("permission expired before response delivery", 403, "")
			return
		}
	}
	if len(payload) > limit {
		f.deny("response exceeds inspection limit", 413, "")
		return
	}
	// This gateway carries HTTPS over TCP. Clear origin advertisements for
	// QUIC/alternate ports, including ones already cached by clients.
	res.Header.Set("Alt-Svc", "clear")
	if err := f.audit("http.response", map[string]any{"request_id": f.requestID, "decision_id": f.decisionID, "status": res.StatusCode,
		"response": map[string]any{"headers": pairsToAny(f.g.Redactor.Headers(f.headerPairs(res.Header))), "body": f.g.Redactor.Body(len(payload))}}); err != nil {
		f.deny("audit unavailable; response withheld", 503, "")
		return
	}
	f.authorization = ""
	f.stopWatch()
	h := f.w.Header()
	for key, values := range res.Header {
		if hopByHop[strings.ToLower(key)] || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			h.Add(key, value)
		}
	}
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	f.w.WriteHeader(res.StatusCode)
	f.wroteHeader = true
	_, _ = f.w.Write(payload)
}

func (f *flow) streamFields(state *ResponseStream, headers http.Header, status int, phase string) map[string]any {
	return map[string]any{"request_id": f.requestID, "decision_id": f.decisionID, "status": status,
		"response": map[string]any{"headers": pairsToAny(f.g.Redactor.Headers(f.headerPairs(headers))),
			"body": map[string]any{"bytes": state.Decoded, "capture": "omitted_policy"}, "stream": state.Summary(phase)}}
}

func (f *flow) streamResponse(res *http.Response) {
	reject := func(reason string, status int) {
		f.deny(reason, status, "")
	}
	if f.decisionID == "" {
		reject("permission expired before response delivery", 403)
		return
	}
	result, err := f.control(map[string]any{"action": "active", "decision_id": f.decisionID})
	if err != nil {
		reject("stream inspection or audit unavailable", 503)
		return
	}
	if active, _ := result["active"].(bool); !active {
		reject("permission expired before response delivery", 403)
		return
	}
	// Capture the known secrets before any body bytes or headers leave.
	encoding := strings.ToLower(strings.TrimSpace(res.Header.Get("Content-Encoding")))
	if encoding == "" {
		encoding = "identity"
	}
	state, err := NewResponseStream(f.g.Redactor.Secrets(), encoding, ResponseLimit)
	if err != nil {
		reject(err.Error(), 502)
		return
	}
	headers := headerBytes(res.Header)
	for _, secret := range f.g.Redactor.Secrets() {
		if bytes.Contains(headers, []byte(secret)) {
			state.Clear()
			reject("provider response exposed a protected credential", 502)
			return
		}
	}
	// Go's client moves declared trailers from Header into res.Trailer.
	if res.Header.Get("Trailer") != "" || len(res.Trailer) > 0 {
		state.Clear()
		reject("response stream trailers unsupported", 502)
		return
	}
	res.Header.Set("Alt-Svc", "clear")
	res.Header.Del("Content-Encoding")
	res.Header.Del("Content-Length")
	res.Header.Del("Trailer")
	if err := f.audit("http.response.started", f.streamFields(state, res.Header, res.StatusCode, "started")); err != nil {
		state.Clear()
		reject("stream inspection or audit unavailable", 503)
		return
	}
	// A slow audit must not allow an expired lease to begin streaming.
	result, err = f.control(map[string]any{"action": "active", "decision_id": f.decisionID})
	if err != nil {
		state.Clear()
		reject("stream inspection or audit unavailable", 503)
		return
	}
	if active, _ := result["active"].(bool); !active || f.killed.Load() {
		state.Clear()
		reject("permission expired before response delivery", 403)
		return
	}
	f.stream = state
	f.streaming = true
	h := f.w.Header()
	for key, values := range res.Header {
		if hopByHop[strings.ToLower(key)] {
			continue
		}
		for _, value := range values {
			h.Add(key, value)
		}
	}
	f.w.WriteHeader(res.StatusCode)
	f.wroteHeader = true
	flusher, _ := f.w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, readErr := res.Body.Read(buf)
		if f.killed.Load() {
			f.abortStream(state, res, "permission expired or revoked")
			return
		}
		if n > 0 {
			output, err := state.Feed(buf[:n])
			if err != nil {
				reason := "response stream inspection failed"
				var rejected *StreamRejected
				if errors.As(err, &rejected) {
					reason = rejected.Reason
				}
				f.abortStream(state, res, reason)
				return
			}
			if len(output) > 0 {
				if _, err = f.w.Write(output); err != nil {
					f.abortStream(state, res, "response stream interrupted")
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			f.abortStream(state, res, "response stream interrupted")
			return
		}
	}
	output, err := state.Feed(nil)
	if err != nil {
		reason := "response stream inspection failed"
		var rejected *StreamRejected
		if errors.As(err, &rejected) {
			reason = rejected.Reason
		}
		f.abortStream(state, res, reason)
		return
	}
	if len(output) > 0 {
		_, _ = f.w.Write(output)
	}
	f.finishStream(state, res)
}

func (f *flow) abortStream(state *ResponseStream, res *http.Response, reason string) {
	if state.Reason == "" {
		state.Reason = reason
	}
	state.Clear()
	fields := f.streamFields(state, res.Header, res.StatusCode, "interrupted")
	fields["reason"] = state.Reason
	_ = f.audit("request.interrupted", fields)
	f.authorization = ""
	f.abort()
}

func (f *flow) finishStream(state *ResponseStream, res *http.Response) {
	f.stopWatch()
	if !state.Finished {
		state.Reason = "response stream interrupted"
	} else {
		result, err := f.control(map[string]any{"action": "active", "decision_id": f.decisionID})
		active := false
		if err == nil {
			active, _ = result["active"].(bool)
		}
		if !active {
			state.Reason = "permission expired or revoked"
		}
	}
	phase := "completed"
	eventType := "http.response"
	fields := f.streamFields(state, res.Header, res.StatusCode, phase)
	if state.Reason != "" {
		phase = "interrupted"
		eventType = "request.interrupted"
		fields = f.streamFields(state, res.Header, res.StatusCode, phase)
		fields["reason"] = state.Reason
	}
	err := f.audit(eventType, fields)
	state.Clear()
	f.authorization = ""
	if state.Reason != "" || err != nil {
		f.abort()
	}
}
