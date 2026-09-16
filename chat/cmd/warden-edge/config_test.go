package main

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
	dir := filepath.Join("..", "..", "..", "deploy", "chat")
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
