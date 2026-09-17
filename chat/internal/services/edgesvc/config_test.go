package edgesvc

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/edge"
	"warden/chat/internal/transport"
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
// the tls section as its material, and the capability file becomes the
// edge's own (the chat writes none over tls://); the loopback shape derives
// no material and reads the chat's file.
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
	if c.Upstream != "tls://warden-chat:7445" || c.UpstreamHost != "warden-chat:7445" || c.UpstreamTLS == nil || c.UpstreamTLS.CAFile != "/etc/warden/tls/ca.crt" || c.UpstreamTLS.CertFile != "/etc/warden/tls/tls.crt" || c.UpstreamTLS.KeyFile != "/etc/warden/tls/tls.key" || c.OwnerTokenFile != "/var/lib/warden/edge/endpoint.json" {
		t.Fatalf("%+v %+v", c, c.UpstreamTLS)
	}
}

// kubernetesOwner is the chart's warden.json for auth.mode owner and
// loopback previews (deploy/helm/warden/templates/_helpers.tpl): kind
// kubernetes, tls:// services, the edge on every pod interface, the app
// reached through a port-forward on 127.0.0.1.
const kubernetesOwner = `{"version":1,"runtime":{"kind":"kubernetes"},"paths":{"state":"/var/lib/warden"},
	"services":{"policy":{"listen":"tls://0.0.0.0:7443","address":"tls://warden-policy:7443"},"runner":{"listen":"tls://0.0.0.0:7444","address":"tls://warden-runner:7444"},"chat":{"listen":"tls://0.0.0.0:7445","address":"tls://warden-chat:7445"}},
	"tls":{"caFile":"/etc/warden/tls/ca.crt","certFile":"/etc/warden/tls/tls.crt","keyFile":"/etc/warden/tls/tls.key"},
	"kubernetes":{"namespace":"warden-sandboxes","tier":"gvisor","runtimeClass":"gvisor","guestImage":"ghcr.io/monaddle-too/warden-guest-base","guestImageDigest":"sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
	"previews":{"mode":"loopback","hostSuffix":"localhost","edgeListen":"0.0.0.0:18781"},
	"auth":{"mode":"owner","publicURL":"http://127.0.0.1:18781"},
	"providers":{}}`

// The Kubernetes owner shape: the edge listens on 0.0.0.0 (the Service and
// the NetworkPolicy decide what reaches it), keeps the owner capability in
// its own state directory, and edge.New accepts the derived settings as a
// minting edge.
func TestEdgeDerivesKubernetesOwnerShape(t *testing.T) {
	t.Setenv(config.Env, "")
	dir := t.TempDir()
	path := filepath.Join(dir, "warden.json")
	os.WriteFile(path, []byte(kubernetesOwner), 0600)
	c, err := loadEdgeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	want := edge.Config{Mode: edge.ModeOwner, Origin: "http://127.0.0.1:18781", PreviewSuffix: "localhost", Upstream: "tls://warden-chat:7445", UpstreamHost: "warden-chat:7445", UpstreamTLS: c.UpstreamTLS, OwnerTokenFile: "/var/lib/warden/edge/endpoint.json", Listen: "0.0.0.0:18781"}
	if c != want || c.UpstreamTLS == nil || c.UpstreamTLS.CertFile != "/etc/warden/tls/tls.crt" {
		t.Fatalf("%+v %+v", c, c.UpstreamTLS)
	}
	// edge.New loads the material, so give it a real key pair; the file
	// paths above are the chart's.
	ca, err := transport.NewCA("test", time.Now(), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if c.UpstreamTLS, err = ca.Material(filepath.Join(dir, "tls"), transport.Edge, nil, time.Now(), time.Hour); err != nil {
		t.Fatal(err)
	}
	c.OwnerTokenFile = filepath.Join(dir, "edge", "endpoint.json")
	s, err := edge.New(c)
	if err != nil {
		t.Fatal("derived Kubernetes owner config rejected:", err)
	}
	if !s.MintsOwnerCapability() {
		t.Fatal("the Kubernetes owner edge does not mint the capability")
	}
}

// The listener rule by kind: 0.0.0.0 only for kind kubernetes; loopback
// and private addresses everywhere; a public address nowhere. The original
// edge JSON is OVH's and keeps the sbx rule.
func TestEdgeListenerByKind(t *testing.T) {
	t.Setenv(config.Env, "")
	dir := t.TempDir()
	sbx := func(listen string) string {
		return `{"version":1,"paths":{"state":"/tmp/w"},"previews":{"mode":"public","hostSuffix":"preview.example.com","edgeListen":"` + listen + `"},
			"auth":{"mode":"google","publicURL":"https://warden.example.com","google":{"signInClientID":"client","owners":["owner@example.com"]}}}`
	}
	kubernetes := func(listen string) string {
		return strings.Replace(kubernetesOwner, `"edgeListen":"0.0.0.0:18781"`, `"edgeListen":"`+listen+`"`, 1)
	}
	legacy := func(listen string) string {
		return `{"origin":"https://warden.monaddle.com","previewSuffix":"preview.monaddle.com","clientID":"client","ownerEmails":"owner@gmail.com","upstream":"http://127.0.0.1:18780","upstreamHost":"127.0.0.1:18780","ownerTokenFile":"/var/lib/warden/app/endpoint.json","listen":"` + listen + `"}`
	}
	cases := []struct {
		name    string
		file    string
		allowed bool
	}{
		{"sbx loopback", sbx("127.0.0.1:18781"), true},
		{"sbx private", sbx("172.18.0.1:19081"), true},
		{"sbx unspecified", sbx("0.0.0.0:18781"), false},
		{"sbx public", sbx("203.0.113.9:18781"), false},
		{"legacy private", legacy("172.18.0.1:19081"), true},
		{"legacy unspecified", legacy("0.0.0.0:19081"), false},
		{"legacy hostname", legacy("edge:19081"), false},
		{"kubernetes unspecified", kubernetes("0.0.0.0:18781"), true},
		{"kubernetes unspecified v6", kubernetes("[::]:18781"), true},
		{"kubernetes loopback", kubernetes("127.0.0.1:18781"), true},
		{"kubernetes private", kubernetes("10.42.0.7:18781"), true},
		{"kubernetes public", kubernetes("203.0.113.9:18781"), false},
		{"kubernetes no port", kubernetes("0.0.0.0"), false},
	}
	for i, tc := range cases {
		path := filepath.Join(dir, fmt.Sprintf("%d.json", i))
		os.WriteFile(path, []byte(tc.file), 0600)
		_, err := loadEdgeConfig(path)
		if (err == nil) != tc.allowed {
			t.Errorf("%s: %v", tc.name, err)
		}
	}
}
