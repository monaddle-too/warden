package edgesvc

import (
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"

	"warden/chat/internal/config"
	"warden/chat/internal/edge"
	"warden/chat/internal/release"
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
		c := edgeConfig(cfg)
		return c, validateListen(c.Listen, cfg.RuntimeKind())
	}
	var c edge.Config
	if err = json.Unmarshal(b, &c); err != nil {
		return edge.Config{}, err
	}
	return c, validateListen(c.Listen, config.RuntimeSBX)
}

// validateListen applies the listener rule of the shape: on the one-host
// shapes (and the original edge JSON, which is OVH's) the edge binds a
// loopback or private address, never a public interface; on Kubernetes the
// chart binds every interface of the pod (0.0.0.0:<port>), because what the
// pod exposes is the Service's and the NetworkPolicy's business.
func validateListen(listen, kind string) error {
	host, _, err := net.SplitHostPort(listen)
	ip := net.ParseIP(host)
	if err != nil || ip == nil {
		return errors.New("edge listener must be an IP address and port")
	}
	if ip.IsLoopback() || ip.IsPrivate() || (kind == config.RuntimeKubernetes && ip.IsUnspecified()) {
		return nil
	}
	if kind == config.RuntimeKubernetes {
		return errors.New("edge listener must be 0.0.0.0, a private or a loopback IP")
	}
	return errors.New("edge listener must be a private or loopback IP")
}

// edgeConfig maps warden.json onto the edge's settings: previews.*, auth.*,
// the chat's address (services.chat.address, http://<chat.listen> unless the
// file says tls://, in which case the tls section is the edge's material)
// and the owner capability file: the chat's, under paths.state/app, which
// the edge reads over the loopback upstream; the edge's own, under
// paths.state/edge, over tls://, where the chat writes none and the edge
// mints the capability itself in owner mode.
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
	// Which Warden this is, for the signed-out page (edge/owner.go); the
	// same pair the chat reports as agentOptions.instance. Owner mode only:
	// a public install tells an anonymous visitor nothing about its build.
	if cfg.Auth.Mode == config.AuthOwner {
		c.InstanceName, c.InstanceVersion = config.InstanceName(cfg.Paths.State), release.Revision
	}
	if transport.IsTLS(c.Upstream) {
		c.UpstreamTLS = cfg.TransportTLS()
		c.OwnerTokenFile = filepath.Join(cfg.EdgeState(), "endpoint.json")
	}
	if g := cfg.Auth.Google; g != nil && cfg.Auth.Mode == config.AuthGoogle {
		c.ClientID = g.SignInClientID
		c.OwnerEmails = strings.Join(g.Owners, ",")
		c.DemoDomains = strings.Join(g.DemoDomains, ",")
		c.LoginsFile = g.SignInLedger
	}
	// The bug-report receiver keeps its files beside the edge's other
	// state (docs/bug-reporting-plan.md); only an enabled one is passed on.
	if b := cfg.Edge.BugReports; b.Enabled {
		c.BugReports = &edge.BugReportsConfig{Enabled: true, Dir: filepath.Join(cfg.EdgeState(), "bug-reports"), RetentionDays: b.RetentionDays, MaxPerHour: b.MaxPerHour, MaxPerDay: b.MaxPerDay}
	}
	return c
}
