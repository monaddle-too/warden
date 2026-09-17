package policy

import (
	"context"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// SharedGateway is one listener serving every binding's gateway
// (docs/warden-kubernetes-plan.md, decision 4): the guest identifies its
// binding by credential, not by which port it reached. Proxied requests
// (CONNECT and absolute-URI requests) carry Proxy-Authorization: Basic
// bindingID:capability, which the guest's HTTP_PROXY URL supplies; provider
// and document routes carry the same credential as the bearer placeholder
// bindingID.capability. A request without a valid credential is challenged
// (407 with Proxy-Authenticate on proxied routes, since libcurl only sends
// proxy credentials after a challenge; 401 on origin-form routes) and never
// reaches a binding's handler, so nothing upstream is contacted.
//
// Each binding keeps its own BindingGateway (MITM CA leaves, secret
// injection, decisions, audit): the shared gateway is a dispatcher in front
// of them.
type SharedGateway struct {
	registry *Registry
	networks []*net.IPNet
	// Factory is replaceable in tests.
	Factory func(cfg GatewayConfig) (*BindingGateway, error)
	// Source, when set, is the second check of decision 4: a valid credential
	// is honoured only when the request comes from the address of the pod
	// running the binding's runtime (runtimeName). Requests from any other
	// address are refused with 403, credential notwithstanding.
	Source  func(runtimeName string, remote net.IP) bool
	host    string
	port    int
	probe   string // origin the health probe dials
	server  *http.Server
	mu      sync.Mutex
	entries map[string]*sharedEntry
	closed  bool
}

type sharedEntry struct {
	signature  string
	capability string
	runtime    string
	gateway    *BindingGateway
}

// NewSharedGateway serves listener and advertises host (the address guests
// dial, a Service IP or name) with the listener's port. It starts serving
// at once; bindings are admitted by Bind.
func NewSharedGateway(registry *Registry, networks []*net.IPNet, listener net.Listener, host string) (*SharedGateway, error) {
	if listener == nil || !ValidHost(strings.ToLower(host)) && net.ParseIP(host) == nil {
		return nil, errors.New("shared gateway needs a listener and an advertised host")
	}
	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || addr.Port == 0 {
		return nil, errors.New("shared gateway needs a TCP listener")
	}
	probe := addr.IP.String()
	if addr.IP == nil || addr.IP.IsUnspecified() {
		probe = "127.0.0.1"
	}
	s := &SharedGateway{registry: registry, networks: networks, Factory: NewBindingGateway, host: strings.ToLower(host), port: addr.Port,
		probe: "http://" + net.JoinHostPort(probe, strconv.Itoa(addr.Port)), entries: map[string]*sharedEntry{}}
	s.server = &http.Server{Handler: s, ReadHeaderTimeout: 30 * time.Second, MaxHeaderBytes: 1 << 20, ErrorLog: silentLogger(), TLSNextProto: map[string]func(*http.Server, *tls.Conn, http.Handler){}}
	go func() { _ = s.server.Serve(listener) }()
	return s, nil
}

// Endpoint is what a binding's guest is told.
func (s *SharedGateway) Endpoint(b *Binding) GatewayEndpoint {
	return GatewayEndpoint{Host: s.host, Port: s.port, BindingID: b.Identity["sandboxID"], Capability: b.Capability}
}

// Port is the shared listener's port.
func (s *SharedGateway) Port() int { return s.port }

// Bind admits the binding's credential and prepares its gateway. It is
// called with the registry lock held; the binding's gateway port becomes
// the shared port without a gateway-port.json, since no listener is chosen
// per binding.
func (s *SharedGateway) Bind(b *Binding) (GatewayEndpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return GatewayEndpoint{}, errors.New("shared gateway closed")
	}
	sandbox := b.Identity["sandboxID"]
	signature := BindingDigest(b.Identity)
	if current := s.entries[sandbox]; current != nil {
		if current.signature == signature && current.capability == b.Capability {
			b.GatewayPort, b.Endpoint = s.port, s.Endpoint(b)
			return b.Endpoint, nil
		}
		current.gateway.Stop()
		delete(s.entries, sandbox)
	}
	if s.registry.CA == nil {
		return GatewayEndpoint{}, errors.New("gateway CA unavailable")
	}
	capability := b.Capability
	gateway, err := s.Factory(GatewayConfig{BindingID: sandbox, Capability: capability, Port: s.port, Host: s.host, CA: s.registry.CA.Derive(), Networks: s.networks,
		Control: func(message map[string]any) (map[string]any, error) {
			return s.registry.Proxy(sandbox, capability, message)
		}})
	if err != nil {
		return GatewayEndpoint{}, err
	}
	// The credential is never audit text.
	gateway.Redactor.Register(capability)
	s.entries[sandbox] = &sharedEntry{signature: signature, capability: capability, runtime: b.Identity["runtimeName"], gateway: gateway}
	b.GatewayPort, b.Endpoint = s.port, s.Endpoint(b)
	return b.Endpoint, nil
}

// Healthy probes the shared listener with the binding's credential; the
// binding's own handler answers the HMAC challenge.
func (s *SharedGateway) Healthy(b *Binding) bool {
	if b.GatewayPort != s.port {
		return false
	}
	bindingID := b.Identity["sandboxID"]
	return gatewayAnswers(s.probe, "127.0.0.1:"+strconv.Itoa(s.port), map[string]string{"Proxy-Authorization": basicCredential(bindingID, b.Capability)}, bindingID, b.Capability)
}

// Close stops every binding's gateway and the listener.
func (s *SharedGateway) Close() {
	s.mu.Lock()
	s.closed = true
	entries := s.entries
	s.entries = map[string]*sharedEntry{}
	s.mu.Unlock()
	for _, entry := range entries {
		entry.gateway.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = s.server.Shutdown(ctx)
	_ = s.server.Close()
}

// lookup returns the binding's gateway when the presented credential is
// its capability, compared in constant time.
func (s *SharedGateway) lookup(bindingID, capability string) *sharedEntry {
	if bindingID == "" || capability == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.entries[bindingID]
	if entry == nil || subtle.ConstantTimeCompare([]byte(entry.capability), []byte(capability)) != 1 {
		return nil
	}
	return entry
}

// fromSource applies the Source check to an admitted entry.
func (s *SharedGateway) fromSource(entry *sharedEntry, r *http.Request) bool {
	if s.Source == nil {
		return true
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	remote := net.ParseIP(host)
	return remote != nil && s.Source(entry.runtime, remote)
}

// ServeHTTP dispatches by Proxy-Authorization when present (it must then be
// valid), else by the bearer placeholder, else challenges. A credential
// presented from an address other than its binding's pod is refused.
func (s *SharedGateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var entry *sharedEntry
	if proxyAuth := r.Header.Get("Proxy-Authorization"); proxyAuth != "" {
		entry = s.lookup(parseBasicCredential(proxyAuth))
	} else if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		entry = s.lookup(parseBearerCredential(strings.TrimPrefix(auth, "Bearer ")))
	}
	if entry == nil {
		s.challenge(w, r)
		return
	}
	if !s.fromSource(entry, r) {
		s.forbidSource(w)
		return
	}
	entry.gateway.ServeHTTP(w, r)
}

// forbidSource refuses a valid credential presented from the wrong pod.
// No challenge: retrying with the same credential cannot succeed.
func (s *SharedGateway) forbidSource(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	payload := []byte(Dumps(map[string]any{"error": "Warden gateway credential presented from another sandbox"}))
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write(payload)
}

// challenge refuses a request no binding's credential admitted. Nothing of
// the request is logged: without a binding there is no audit to write to.
func (s *SharedGateway) challenge(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("Content-Type", "application/json")
	h.Set("Cache-Control", "no-store")
	h.Set("Connection", "close")
	status := http.StatusUnauthorized
	if r.Method == http.MethodConnect || r.URL.IsAbs() {
		status = http.StatusProxyAuthRequired
		h.Set("Proxy-Authenticate", `Basic realm="warden"`)
	} else {
		h.Set("WWW-Authenticate", `Bearer realm="warden"`)
	}
	payload := []byte(Dumps(map[string]any{"error": "Warden gateway credential required"}))
	h.Set("Content-Length", strconv.Itoa(len(payload)))
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}

// basicCredential encodes the proxy credential of a binding.
func basicCredential(bindingID, capability string) string {
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(bindingID+":"+capability))
}

// parseBasicCredential splits a Proxy-Authorization value into the binding
// ID and capability. Binding IDs may contain colons; capabilities do not,
// so the split is at the last one.
func parseBasicCredential(value string) (string, string) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(value), " ")
	if !ok || !strings.EqualFold(scheme, "Basic") {
		return "", ""
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(rest))
	if err != nil || len(decoded) > 1024 {
		return "", ""
	}
	i := strings.LastIndexByte(string(decoded), ':')
	if i <= 0 {
		return "", ""
	}
	return string(decoded[:i]), string(decoded[i+1:])
}

// parseBearerCredential splits the placeholder bindingID.capability at its
// last dot (binding IDs may contain dots; capabilities do not).
func parseBearerCredential(value string) (string, string) {
	if len(value) > 1024 {
		return "", ""
	}
	i := strings.LastIndexByte(value, '.')
	if i <= 0 {
		return "", ""
	}
	return value[:i], value[i+1:]
}
