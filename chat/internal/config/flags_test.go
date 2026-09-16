package config

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"warden/chat/internal/release"
)

func TestResolveComputesDefaultsFromTheServiceDirectory(t *testing.T) {
	t.Setenv(Env, "")
	c, source, err := Resolve("", "/tmp/w/policy")
	if err != nil || source != "" || c.Paths.State != "/tmp/w" || c.PolicyState() != "/tmp/w/policy" {
		t.Fatal(c.Paths.State, source, err)
	}
	if _, _, err = Resolve("", ""); err == nil {
		t.Fatal("no file and no state accepted")
	}
	path := filepath.Join(t.TempDir(), "warden.json")
	os.WriteFile(path, []byte(`{"version":1,"paths":{"state":"/tmp/w"},"sandboxes":{"maxRunning":4}}`), 0600)
	t.Setenv(Env, path)
	c, source, err = Resolve("", "/elsewhere/policy")
	if err != nil || source != path || c.Sandboxes.MaxRunning != 4 || c.Paths.State != "/tmp/w" {
		t.Fatal(c.Paths.State, source, err)
	}
}

func TestOverridesFollowTheDisagreementRule(t *testing.T) {
	parse := func(source string, args ...string) (*Overrides, *string, *int) {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		s := fs.String("sbx", "", "")
		n := fs.Int("max", 0, "")
		if err := fs.Parse(args); err != nil {
			t.Fatal(err)
		}
		return NewOverrides(fs, source), s, n
	}
	// Defaults: a set flag overrides silently; an unset flag keeps the config value.
	o, s, n := parse("", "--sbx", "/usr/bin/sbx")
	if got := Override(o, "sbx", *s, "sbx.executable", "/opt/homebrew/bin/sbx"); got != "/usr/bin/sbx" || o.Set("max") {
		t.Fatal(got)
	}
	if got := Override(o, "max", *n, "sandboxes.maxRunning", 2); got != 2 || o.Err() != nil {
		t.Fatal(got, o.Err())
	}
	// File: agreement and filling an empty field are fine.
	o, s, n = parse("/etc/warden.json", "--sbx", "/usr/bin/sbx", "--max", "3")
	Override(o, "sbx", *s, "sbx.executable", "")
	Override(o, "max", *n, "sandboxes.maxRunning", 3)
	if o.Err() != nil || !o.FromFile() {
		t.Fatal(o.Err())
	}
	// File: disagreement names the flag, the field and the file.
	o, s, _ = parse("/etc/warden.json", "--sbx", "/usr/bin/sbx")
	Override(o, "sbx", *s, "sbx.executable", "/opt/homebrew/bin/sbx")
	err := o.Err()
	if err == nil || !strings.Contains(err.Error(), "--sbx") || !strings.Contains(err.Error(), "sbx.executable") || !strings.Contains(err.Error(), "/etc/warden.json") {
		t.Fatal(err)
	}
}

func TestGuestTemplateAndDigestDefaults(t *testing.T) {
	c := Defaults("/tmp/w")
	if c.GuestTemplate() != release.StockTemplate+"@"+release.StockTemplateDigest || c.GuestDigest() != release.StockTemplateDigest {
		t.Fatal(c.GuestTemplate())
	}
	c.SBX.GuestImage, c.SBX.GuestImageDigest = "ghcr.io/x/guest:1", "sha256:"+strings.Repeat("a", 64)
	if c.GuestTemplate() != "ghcr.io/x/guest:1@sha256:"+strings.Repeat("a", 64) || c.GuestDigest() != c.SBX.GuestImageDigest {
		t.Fatal(c.GuestTemplate())
	}
	// A template in the daemon's own store is named by tag; the digest is
	// still what the verifier admits.
	c.SBX.GuestImage, c.SBX.GuestImageDigest = "warden-guest:abc-arm64", "sha256:"+strings.Repeat("b", 64)
	if c.GuestTemplate() != "warden-guest:abc-arm64" || c.GuestDigest() != "sha256:"+strings.Repeat("b", 64) {
		t.Fatal(c.GuestTemplate())
	}
	if c.EdgePort() != "18781" {
		t.Fatal(c.EdgePort())
	}
}
