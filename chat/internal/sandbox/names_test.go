package sandbox

import (
	"context"
	"regexp"
	"strings"
	"testing"
)

// Two instances sharing one SBX namespace never see each other's
// sandboxes: their runtime names are disjoint under Owned, the default
// instance's included, although its prefix is a prefix of every other.
func TestRuntimeNamesOfInstancesAreDisjoint(t *testing.T) {
	ids := []string{"sb-1", "sb-2", "sb-3"}
	var inventory []string
	for _, instance := range []string{"", "dev", "dogfood-a"} {
		for _, id := range ids {
			inventory = append(inventory, RuntimeName(instance, id))
		}
		inventory = append(inventory, SpareName(instance))
	}
	inventory = append(inventory, "warden-copy-abc", "something-else", "wc-", "wc-dev-", "wc-spare-", "wc-spare-zz")
	seen := map[string]string{}
	for _, instance := range []string{"", "dev", "dogfood-a"} {
		owned := Owned(instance, inventory)
		if len(owned) != len(ids)+1 {
			t.Fatalf("instance %q owns %v, want %d names", instance, owned, len(ids)+1)
		}
		for _, name := range owned {
			if other, dup := seen[name]; dup {
				t.Fatalf("%s owned by %q and %q", name, other, instance)
			}
			seen[name] = instance
			if !strings.HasPrefix(name, RuntimePrefix(instance)) {
				t.Fatalf("%s lacks the prefix of %q", name, instance)
			}
		}
	}
	// "Dev" and "dev" are one instance (state directories are
	// case-insensitive on macOS; the name stays a DNS label).
	if RuntimeName("Dev", "sb-1") != RuntimeName("dev", "sb-1") {
		t.Fatal("instance names are not case-folded")
	}
	if got := Owned("", inventory); len(got) != len(ids)+1 {
		t.Fatalf("the default instance owns %v", got)
	}
	label := regexp.MustCompile(`^[a-z0-9-]{4,63}$`)
	for _, name := range inventory[:12] {
		if !label.MatchString(name) {
			t.Fatalf("%s is not a DNS label", name)
		}
	}
}

// The worker derives its names through the instance it is configured for.
func TestWorkerNamesCarryTheInstance(t *testing.T) {
	for _, instance := range []string{"", "dev"} {
		w := NewWorker(t.TempDir(), "/never-host-exec", "template")
		w.Instance, w.Runtime = instance, &testRuntime{}
		if err := w.initializeManaged(context.Background()); err != nil {
			t.Fatal(err)
		}
		w.mu.Lock()
		resp, err := w.bindLocked(context.Background(), Request{ProjectID: "proj-1", ChatID: "chat-1", SandboxID: "sb-1", PrincipalID: "user-1"})
		w.mu.Unlock()
		if err != nil {
			t.Fatal(err)
		}
		s := w.managed.Sandboxes["sb-1"]
		if s == nil || s.RuntimeName != RuntimeName(instance, "sb-1") || !strings.HasPrefix(s.RuntimeName, RuntimePrefix(instance)) {
			t.Fatalf("instance %q: bound %+v (%+v)", instance, s, resp)
		}
	}
}
