package edgesvc

import (
	"encoding/json"
	"errors"
	"os"
	"strings"

	"warden/chat/internal/config"
	"warden/chat/internal/edge"
	"warden/chat/internal/transport"
)

// loadEdgeConfig reads --config (or $WARDEN_CONFIG). A warden.json (it has a
// "version") yields the edge settings derived below; the original edge JSON
// (deploy/chat/edge.example.json) still loads unchanged for OVH.
func loadEdgeConfig(path string) (edge.Config, error) {
	if path == "" {
		path = os.Getenv(config.Env)
	}
	if path == "" {
		return edge.Config{}, errors.New("--config or $" + config.Env + " is required")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return edge.Config{}, err
	}
	var probe struct {
		Version *int `json:"version"`
	}
	if json.Unmarshal(b, &probe) == nil && probe.Version != nil {
		cfg, err := config.Parse(b)
		if err != nil {
			return edge.Config{}, errors.New(path + ": " + err.Error())
		}
		return edgeConfig(cfg), nil
	}
	var c edge.Config
	if err = json.Unmarshal(b, &c); err != nil {
		return edge.Config{}, err
	}
	return c, nil
}

// edgeConfig maps warden.json onto the edge's settings: previews.*, auth.*,
// the chat's address (services.chat.address, http://<chat.listen> unless the
// file says tls://, in which case the tls section is the edge's material)
// and the owner capability file derived from paths.state.
func edgeConfig(cfg config.Config) edge.Config {
	c := edge.Config{
		Mode:           cfg.Auth.Mode,
		Origin:         cfg.Auth.PublicURL,
		PreviewSuffix:  cfg.Previews.HostSuffix,
		Upstream:       cfg.ChatAddress(),
		UpstreamHost:   config.HostOf(cfg.ChatAddress()),
		OwnerTokenFile: cfg.OwnerTokenFile(),
		Listen:         cfg.Previews.EdgeListen,
	}
	if transport.IsTLS(c.Upstream) {
		c.UpstreamTLS = cfg.TransportTLS()
	}
	if g := cfg.Auth.Google; g != nil && cfg.Auth.Mode == config.AuthGoogle {
		c.ClientID = g.SignInClientID
		c.OwnerEmails = strings.Join(g.Owners, ",")
		c.DemoDomains = strings.Join(g.DemoDomains, ",")
		c.LoginsFile = g.SignInLedger
	}
	return c
}
