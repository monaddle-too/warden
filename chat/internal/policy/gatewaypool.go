package policy

import (
	"errors"
	"net"
	"strconv"
	"sync"
	"time"
)

// GatewayPool owns one in-process gateway per registered binding. Ensure is
// called by the verifier with the registry lock held.
type GatewayPool struct {
	registry *Registry
	networks []*net.IPNet
	mu       sync.Mutex
	gateways map[string]*poolEntry
	// Factory is replaceable in tests.
	Factory func(cfg GatewayConfig) (*Gateway, error)
}

type poolEntry struct {
	signature string
	gateway   *Gateway
}

func NewGatewayPool(registry *Registry, networks []*net.IPNet) *GatewayPool {
	return &GatewayPool{registry: registry, networks: networks, gateways: map[string]*poolEntry{}, Factory: NewGateway}
}

// Ensure starts (or restarts) the binding's gateway and waits for health.
func (p *GatewayPool) Ensure(b *Binding) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	sandbox := b.Identity["sandboxID"]
	signature := BindingDigest(b.Identity)
	if current := p.gateways[sandbox]; current != nil {
		if current.signature == signature && current.gateway.Running() {
			return nil
		}
		current.gateway.Stop()
		delete(p.gateways, sandbox)
	}
	var listener net.Listener
	var err error
	if b.GatewayPort == 0 {
		listener, err = net.Listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return err
		}
		port := listener.Addr().(*net.TCPAddr).Port
		if err = p.registry.SetGatewayPort(b, port); err != nil {
			listener.Close()
			return err
		}
	} else {
		listener, err = net.Listen("tcp4", "127.0.0.1:"+strconv.Itoa(b.GatewayPort))
		if err != nil {
			return errors.New("managed gateway failed to become ready")
		}
	}
	if p.registry.CA == nil {
		listener.Close()
		return errors.New("gateway CA unavailable")
	}
	// One host-wide CA; this gateway mints its own leaves from it.
	ca := p.registry.CA.Derive()
	capability := b.Capability
	gateway, err := p.Factory(GatewayConfig{BindingID: sandbox, Capability: capability, Port: b.GatewayPort, Listener: listener, CA: ca, Networks: p.networks,
		Control: func(message map[string]any) (map[string]any, error) {
			return p.registry.Proxy(sandbox, capability, message)
		}})
	if err != nil {
		listener.Close()
		return err
	}
	gateway.Start()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && gateway.Running() {
		if GatewayHealthy(b) {
			p.gateways[sandbox] = &poolEntry{signature: signature, gateway: gateway}
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	gateway.Stop()
	return errors.New("managed gateway failed to become ready")
}

// Close stops every gateway.
func (p *GatewayPool) Close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for sandbox, entry := range p.gateways {
		entry.gateway.Stop()
		delete(p.gateways, sandbox)
	}
}
