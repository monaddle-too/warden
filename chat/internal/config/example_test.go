package config

import (
	"os"
	"testing"
)

// The OVH example in deploy/chat must stay a valid server-mode file.
func TestOVHExampleParses(t *testing.T) {
	b, err := os.ReadFile("../../../deploy/chat/warden.example.json")
	if err != nil {
		t.Fatal(err)
	}
	c, err := Parse(b)
	if err != nil {
		t.Fatal(err)
	}
	if c.Previews.Mode != PreviewPublic || c.Auth.Mode != AuthGoogle || c.GitHubMode() != "app" || c.PreviewScheme() != "https" || c.OwnerTokenFile() != "/var/lib/warden/app/endpoint.json" {
		t.Fatalf("%+v", c)
	}
}
