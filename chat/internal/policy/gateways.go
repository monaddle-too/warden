package policy

import (
	"net"
	"net/url"
)

// LoopbackGatewayHost is the name an SBX guest resolves to the host's
// loopback, where the per-binding gateways listen.
const LoopbackGatewayHost = "host.docker.internal"

// LoopbackPlaceholder is the bearer a guest presents on provider routes of
// a loopback gateway, whose listener already identifies the binding.
const LoopbackPlaceholder = "warden-proxy-managed"

// GatewayEndpoint is one binding's gateway as the guest reaches it: the
// address Begin advertises and, when the gateway identifies bindings by
// credential rather than by listener, the credential the guest presents
// (docs/warden-kubernetes-plan.md, decision 4 and its spike refinement).
type GatewayEndpoint struct {
	// Host and Port are what the guest dials: host.docker.internal and the
	// binding's loopback port for LoopbackGateways, the shared listener's
	// advertised host and port for SharedGateway.
	Host string
	Port int
	// BindingID and Capability are the proxy credential
	// (Proxy-Authorization: Basic bindingID:capability) and, joined by a
	// dot, the bearer placeholder on provider routes. Capability is empty
	// for a loopback gateway, whose listener is the identity.
	BindingID  string
	Capability string
}

// Credentialed reports whether the guest must present the binding
// credential (a shared gateway) rather than merely reach the endpoint.
func (e GatewayEndpoint) Credentialed() bool { return e.Capability != "" }

// BaseURL is the plain http://host:port origin of provider and document
// routes.
func (e GatewayEndpoint) BaseURL() string {
	return "http://" + net.JoinHostPort(e.Host, itoa(e.Port))
}

// ProxyURL is what HTTP_PROXY is set to: the origin, with the binding
// credential as userinfo when the gateway dispatches by it.
func (e GatewayEndpoint) ProxyURL() string {
	if !e.Credentialed() {
		return e.BaseURL()
	}
	u := url.URL{Scheme: "http", Host: net.JoinHostPort(e.Host, itoa(e.Port)), User: url.UserPassword(e.BindingID, e.Capability)}
	return u.String()
}

// Placeholder is the bearer the agent presents on provider routes: the
// constant for a loopback gateway, the binding credential for a shared one.
func (e GatewayEndpoint) Placeholder() string {
	if !e.Credentialed() {
		return LoopbackPlaceholder
	}
	return e.BindingID + "." + e.Capability
}

// LoopbackEndpoint is the endpoint of a per-binding loopback gateway on
// port.
func LoopbackEndpoint(port int) GatewayEndpoint {
	return GatewayEndpoint{Host: LoopbackGatewayHost, Port: port}
}

// Gateway runs the inspected gateways of the registry's bindings. Bind is
// called by the verifier with the registry lock held and must not block on
// anything that takes it.
type Gateway interface {
	// Bind starts (or confirms) the binding's gateway and returns the
	// endpoint Begin advertises for it.
	Bind(b *Binding) (GatewayEndpoint, error)
	// Healthy probes the binding's gateway with a fresh HMAC challenge over
	// its capability. Callers pass a snapshot of the binding.
	Healthy(b *Binding) bool
	Close()
}

// LoopbackGateways is today's per-binding gateway pool behind the Gateway
// interface: one MITM listener per binding on 127.0.0.1, its port kept in
// the binding's gateway-port.json, host.docker.internal advertised.
type LoopbackGateways struct{ *GatewayPool }

func NewLoopbackGateways(registry *Registry, networks []*net.IPNet) *LoopbackGateways {
	return &LoopbackGateways{NewGatewayPool(registry, networks)}
}

func (l *LoopbackGateways) Bind(b *Binding) (GatewayEndpoint, error) {
	if err := l.Ensure(b); err != nil {
		return GatewayEndpoint{}, err
	}
	return LoopbackEndpoint(b.GatewayPort), nil
}

func (l *LoopbackGateways) Healthy(b *Binding) bool { return GatewayHealthy(b) }
