package sandbox

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"warden/chat/internal/transport"
)

// GrantContext is constructed from worker records, never guest tool arguments.
type GrantContext struct {
	Provider    string `json:"provider,omitempty"`
	ProjectID   string `json:"projectID"`
	SandboxID   string `json:"sandboxID"`
	RuntimeName string `json:"runtimeName"`
	Generation  string `json:"generation"`
	ChatID      string `json:"chatID"`
	RunID       string `json:"runID"`
	PrincipalID string `json:"principalID"`
}

type Enforcement interface {
	Register(context.Context, GrantContext) error
	Check(context.Context, GrantContext, string) error
	Begin(context.Context, GrantContext) (BrokerConfig, error)
	Renew(context.Context, GrantContext) error
	End(context.Context, GrantContext) error
}

type BrokerConfig struct {
	CACertificate     string `json:"caCertificate,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Model             string `json:"-"`
	ThreadID          string `json:"-"`
	DocumentBaseURL   string `json:"documentBaseURL,omitempty"`
	APIKeyPlaceholder string `json:"apiKeyPlaceholder"`
	ProviderBaseURL   string `json:"providerBaseURL"`
	ProxyURL          string `json:"proxyURL"`
}

// PolicyEnforcement is the runner's client of the policy service's control
// protocol at Address, a unix:// socket (the sbx shapes) or a tls://
// host:port (Kubernetes, with TLS naming this service's material; the
// policy service admits warden-runner and warden-chat).
type PolicyEnforcement struct {
	Address string
	TLS     *transport.TLS
}

func (g *PolicyEnforcement) exchange(ctx context.Context, operation string, c GrantContext, phase string) (BrokerConfig, error) {
	var broker BrokerConfig
	if g == nil || g.Address == "" {
		return broker, errors.New("Warden enforcement is not configured")
	}
	// Two resident sandboxes share the policy verifier; allow its serialized
	// attestation to finish while staying below the 60-second worker lease.
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, g.Address, transport.DialOptions{TLS: g.TLS})
	if err != nil {
		return broker, fmt.Errorf("Warden unavailable: %w", err)
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	request := struct {
		Version   int          `json:"version"`
		Operation string       `json:"operation"`
		Context   GrantContext `json:"context"`
		Phase     string       `json:"phase,omitempty"`
	}{1, operation, c, phase}
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return broker, err
	}
	b, err := readLine(bufio.NewReader(conn), 65536)
	if err != nil {
		return broker, err
	}
	var response struct {
		Version int    `json:"version"`
		OK      bool   `json:"ok"`
		Ready   bool   `json:"ready"`
		Reason  string `json:"reason"`
		Error   string `json:"error"`
		BrokerConfig
	}
	if err = json.Unmarshal(b, &response); err != nil {
		return broker, err
	}
	if response.Version != 1 || !response.OK || ((operation == "check" || operation == "begin" || operation == "renew") && !response.Ready) {
		reason := response.Reason
		if reason == "" {
			reason = response.Error
		}
		if reason == "" {
			reason = "no verified enforcement readiness"
		}
		return broker, fmt.Errorf("Warden denied %s: %s", operation, reason)
	}
	broker = response.BrokerConfig
	if operation == "begin" && (broker.APIKeyPlaceholder == "" || broker.ProviderBaseURL == "" || broker.ProxyURL == "") {
		return broker, errors.New("Warden provider broker is not configured")
	}
	return broker, nil
}
func (g *PolicyEnforcement) call(ctx context.Context, op string, c GrantContext, phase string) error {
	_, err := g.exchange(ctx, op, c, phase)
	return err
}
func (g *PolicyEnforcement) Register(ctx context.Context, c GrantContext) error {
	return g.call(ctx, "register", c, "")
}
func (g *PolicyEnforcement) Check(ctx context.Context, c GrantContext, phase string) error {
	return g.call(ctx, "check", c, phase)
}
func (g *PolicyEnforcement) Begin(ctx context.Context, c GrantContext) (BrokerConfig, error) {
	return g.exchange(ctx, "begin", c, "")
}
func (g *PolicyEnforcement) Renew(ctx context.Context, c GrantContext) error {
	return g.call(ctx, "renew", c, "")
}
func (g *PolicyEnforcement) End(ctx context.Context, c GrantContext) error {
	return g.call(ctx, "end", c, "")
}
