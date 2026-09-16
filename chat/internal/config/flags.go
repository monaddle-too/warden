package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"

	"warden/chat/internal/release"
)

// Resolve is how a binary obtains its configuration. path (or $WARDEN_CONFIG)
// names the optional file. Without one, the configuration is Defaults for the
// state root derived from serviceState, the service's own private directory
// given by its legacy flag (the root is its parent: <root>/policy,
// <root>/runner, <root>/app). The returned source is the file path, or ""
// when defaults were computed.
func Resolve(path, serviceState string) (Config, string, error) {
	if path == "" {
		path = os.Getenv(Env)
	}
	if path != "" {
		c, err := Load(path, "")
		return c, path, err
	}
	if serviceState == "" {
		return Config{}, "", errors.New("a --config file, $" + Env + " or the service state directory flag is required")
	}
	abs, err := filepath.Abs(serviceState)
	if err != nil {
		return Config{}, "", err
	}
	c := Defaults(filepath.Dir(abs))
	return c, "", c.Validate()
}

// Overrides reconciles a binary's legacy flags with the loaded configuration.
// A flag the operator did not set never overrides. A set flag overrides a
// computed default silently, fills a field the file left empty, and is an
// error when it disagrees with a value the file provides.
type Overrides struct {
	source string
	set    map[string]bool
	errs   []string
}

// NewOverrides records which flags of fs were set explicitly. source is the
// loaded file path, or "" when the configuration is computed defaults.
func NewOverrides(fs *flag.FlagSet, source string) *Overrides {
	o := &Overrides{source: source, set: map[string]bool{}}
	fs.Visit(func(f *flag.Flag) { o.set[f.Name] = true })
	return o
}

// Set reports whether the named flag was given on the command line.
func (o *Overrides) Set(name string) bool { return o.set[name] }

// FromFile reports whether the configuration came from a file.
func (o *Overrides) FromFile() bool { return o.source != "" }

// Override returns the effective value for one flag/field pair.
func Override[T comparable](o *Overrides, flagName string, flagValue T, field string, current T) T {
	if !o.set[flagName] {
		return current
	}
	var zero T
	if o.source != "" && current != zero && flagValue != current {
		o.errs = append(o.errs, fmt.Sprintf("--%s %v disagrees with %s %v in %s", flagName, flagValue, field, current, o.source))
	}
	return flagValue
}

// Err returns every disagreement found so far.
func (o *Overrides) Err() error {
	if len(o.errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(o.errs, "; "))
}

// GuestTemplate is the SBX template the runner creates sandboxes from:
// sbx.guestImage pinned to sbx.guestImageDigest, or the stock shell template
// at its pinned digest when no guest image is configured.
func (c Config) GuestTemplate() string {
	image, digest := c.SBX.GuestImage, c.SBX.GuestImageDigest
	if image == "" {
		image = release.StockTemplate
		if digest == "" {
			digest = release.StockTemplateDigest
		}
	}
	// A registry reference (it has a repository path with a slash) is pinned
	// with @digest so sbx pulls exactly that manifest. A template loaded into
	// the daemon's own store (`sbx template load` / `sbx template save`, a
	// bare name:tag) must be named by tag alone: sbx treats name@digest as a
	// registry pull and fails. The verifier enforces the digest either way.
	if digest != "" && !strings.Contains(image, "@") && strings.Contains(image, "/") {
		image += "@" + digest
	}
	return image
}

// GuestDigest is the only image digest the verifier admits: the configured
// guest image digest, or the stock template's.
func (c Config) GuestDigest() string {
	if c.SBX.GuestImageDigest != "" {
		return c.SBX.GuestImageDigest
	}
	return release.StockTemplateDigest
}

// EdgePort is the port of previews.edgeListen, the port loopback preview
// URLs carry. It is "" when the address has none.
func (c Config) EdgePort() string {
	_, port, err := net.SplitHostPort(c.Previews.EdgeListen)
	if err != nil {
		return ""
	}
	return port
}
