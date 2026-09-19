package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strings"
)

// Runtime names carry the instance (docs/host-dogfood-plan.md, decision
// 3): several Warden instances may share one SBX namespace, and each one
// must find only its own sandboxes in the daemon's inventory. The default
// instance keeps the historic wc-<hex> and wc-spare-<hex>; an instance
// named after its state directory (sandboxes.namePrefix, "dev" for
// ~/.warden-dev) names its sandboxes wc-dev-<hex> and wc-dev-spare-<hex>.
// The name is lower-cased so it stays a DNS label for the Kubernetes
// driver; state directories are case-insensitive on macOS anyway.

// RuntimePrefix is what every runtime name of instance starts with.
func RuntimePrefix(instance string) string {
	if instance == "" {
		return "wc-"
	}
	return "wc-" + strings.ToLower(instance) + "-"
}

// RuntimeName derives the runtime name of a sandbox from its registry ID.
func RuntimeName(instance, sandboxID string) string {
	hash := sha256.Sum256([]byte(sandboxID))
	return RuntimePrefix(instance) + hex.EncodeToString(hash[:12])
}

// SpareName names a warm spare guest of instance.
func SpareName(instance string) string {
	return RuntimePrefix(instance) + "spare-" + randomID()[:16]
}

var hexTail = regexp.MustCompile(`^(spare-)?[0-9a-f]+$`)

// Owned filters a daemon inventory (`sbx ls --quiet`) to the runtime names
// of instance: its prefix followed by the hex the worker derives (or
// spare-<hex>), and nothing else. The default instance's prefix is a
// prefix of every other instance's, so the tail must be hex alone: a
// "wc-dev-…" name is never the default instance's.
func Owned(instance string, inventory []string) []string {
	prefix := RuntimePrefix(instance)
	var owned []string
	for _, name := range inventory {
		if strings.HasPrefix(name, prefix) && hexTail.MatchString(strings.TrimPrefix(name, prefix)) {
			owned = append(owned, name)
		}
	}
	return owned
}
