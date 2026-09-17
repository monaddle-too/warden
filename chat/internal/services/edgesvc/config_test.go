package edgesvc

import (
	"os"
	"path/filepath"
	"testing"

	"warden/chat/internal/config"
	"warden/chat/internal/edge"
)

func TestEdgeLoadsBothConfigShapes(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(config.Env, "")
	legacy := filepath.Join(dir, "config.json")
	os.WriteFile(legacy, []byte(`{"origin":"https://warden.monaddle.com","previewSuffix":"preview.monaddle.com","clientID":"client","ownerEmails":"owner@gmail.com","upstream":"http://127.0.0.1:18780","upstreamHost":"127.0.0.1:18780","ownerTokenFile":"/var/lib/warden/app/endpoint.json","listen":"172.18.0.1:19081","loginsFile":"/var/lib/warden/edge/logins.json"}`), 0600)
	c, err := loadEdgeConfig(legacy)
	if err != nil || c.Mode != "" || c.Origin != "https://warden.monaddle.com" || c.ClientID != "client" || c.Listen != "172.18.0.1:19081" || c.LoginsFile != "/var/lib/warden/edge/logins.json" {
		t.Fatalf("%+v %v", c, err)
	}
	local := filepath.Join(dir, "warden.json")
	os.WriteFile(local, []byte(`{"version":1,"paths":{"state":"/tmp/w"}}`), 0600)
	c, err = loadEdgeConfig(local)
	if err != nil {
		t.Fatal(err)
	}
	want := edge.Config{Mode: edge.ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: "http://127.0.0.1:18780", UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: "/tmp/w/app/endpoint.json", Listen: "127.0.0.1:18781"}
	if c != want {
		t.Fatalf("%+v", c)
	}
	if _, err = edge.New(c); err != nil {
		t.Fatal("derived loopback edge config rejected:", err)
	}
	server := filepath.Join(dir, "server.json")
	os.WriteFile(server, []byte(`{"version":1,"paths":{"state":"/var/lib/warden"},"chat":{"listen":"127.0.0.1:18780"},
		"previews":{"mode":"public","hostSuffix":"preview.monaddle.com","edgeListen":"172.18.0.1:19081"},
		"auth":{"mode":"google","publicURL":"https://warden.monaddle.com","google":{"signInClientID":"client","owners":["owner@gmail.com"],"demoDomains":["example.com"]}}}`), 0600)
	c, err = loadEdgeConfig(server)
	if err != nil {
		t.Fatal(err)
	}
	want = edge.Config{Mode: edge.ModeGoogle, Origin: "https://warden.monaddle.com", PreviewSuffix: "preview.monaddle.com", ClientID: "client", OwnerEmails: "owner@gmail.com", DemoDomains: "example.com", Upstream: "http://127.0.0.1:18780", UpstreamHost: "127.0.0.1:18780", OwnerTokenFile: "/var/lib/warden/app/endpoint.json", LoginsFile: "/var/lib/warden/edge/logins.json", Listen: "172.18.0.1:19081"}
	if c != want {
		t.Fatalf("%+v", c)
	}
	t.Setenv(config.Env, local)
	if c, err = loadEdgeConfig(""); err != nil || c.Mode != edge.ModeOwner {
		t.Fatal("$WARDEN_CONFIG ignored", err)
	}
	t.Setenv(config.Env, "")
	if _, err = loadEdgeConfig(""); err == nil {
		t.Fatal("missing config accepted")
	}
}

// The OVH edge keeps its own edge.example.json shape; loading
// warden.example.json instead must derive exactly the same settings, so
// the unit can switch files later without a behaviour change.
func TestOVHExampleFilesAgree(t *testing.T) {
	t.Setenv(config.Env, "")
	dir := filepath.Join("..", "..", "..", "..", "deploy", "chat")
	legacy, err := loadEdgeConfig(filepath.Join(dir, "edge.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	derived, err := loadEdgeConfig(filepath.Join(dir, "warden.example.json"))
	if err != nil {
		t.Fatal(err)
	}
	legacy.Mode = edge.ModeGoogle // the original shape leaves it implicit
	if derived != legacy {
		t.Fatalf("warden.example.json derives\n%+v\nbut edge.example.json holds\n%+v", derived, legacy)
	}
}

// A warden.json with a tls:// chat address gives the edge that upstream and
// the tls section as its material; the loopback shape derives no material.
func TestEdgeDerivesTLSUpstream(t *testing.T) {
	t.Setenv(config.Env, "")
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/var/lib/warden"},
		"previews":{"mode":"public","hostSuffix":"preview.example.com","edgeListen":"10.0.0.5:8080"},
		"auth":{"mode":"google","publicURL":"https://warden.example.com","google":{"signInClientID":"client","owners":["owner@example.com"]}},
		"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444"},"chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
		"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"}}`), 0600)
	c, err := loadEdgeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Upstream != "tls://warden-chat:7445" || c.UpstreamHost != "warden-chat:7445" || c.UpstreamTLS == nil || c.UpstreamTLS.CAFile != "/etc/warden/tls/ca.crt" || c.UpstreamTLS.CertFile != "/etc/warden/tls/tls.crt" || c.UpstreamTLS.KeyFile != "/etc/warden/tls/tls.key" || c.OwnerTokenFile != "/var/lib/warden/app/endpoint.json" {
		t.Fatalf("%+v %+v", c, c.UpstreamTLS)
	}
}
