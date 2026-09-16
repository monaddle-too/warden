package handshake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"warden/chat/internal/release"
)

// fakeSocket answers every connection with one JSON line and records the
// request it received.
func fakeSocket(t *testing.T, name string, reply func(request map[string]any) map[string]any) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "whs")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, name)
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			conn, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				line, _ := bufio.NewReader(conn).ReadBytes('\n')
				var request map[string]any
				_ = json.Unmarshal(line, &request)
				_ = json.NewEncoder(conn).Encode(reply(request))
			}()
		}
	}()
	return path
}

func runnerLike(protocol int, revision string) func(map[string]any) map[string]any {
	return func(request map[string]any) map[string]any {
		if request["operation"] != "health" {
			return map[string]any{"version": protocol, "error": "unsupported"}
		}
		return map[string]any{"version": protocol, "revision": revision, "output": "sbx protocol 2"}
	}
}

func policyLike(protocol int, revision string) func(map[string]any) map[string]any {
	return func(request map[string]any) map[string]any {
		if request["version"] != float64(1) || request["operation"] != "version" || len(request) != 2 {
			return map[string]any{"version": 1, "ok": false, "ready": false, "allow": false, "reason": "control_request_rejected"}
		}
		return map[string]any{"version": 1, "ok": true, "protocol": protocol, "revision": revision}
	}
}

func TestParseAndString(t *testing.T) {
	self := Self("warden-chat")
	if self.Revision != release.Revision || self.Protocol != release.Protocol {
		t.Fatalf("%+v", self)
	}
	p, err := Parse("warden-runner abc123 protocol=2\n")
	if err != nil || p != (Peer{Name: "warden-runner", Revision: "abc123", Protocol: 2}) {
		t.Fatalf("%+v %v", p, err)
	}
	if p.String() != "warden-runner abc123 protocol=2" {
		t.Fatal(p.String())
	}
	old, err := Parse("warden-policy development")
	if err != nil || old.Protocol != 0 || old.Revision != "development" || old.String() != "warden-policy development" {
		t.Fatalf("%+v %v", old, err)
	}
	if _, err = Parse("Usage of warden-chat:\n  -config string"); err == nil {
		t.Fatal("usage text parsed as a version")
	}
}

func TestCompareRefusesProtocolAndWarnsOnRevision(t *testing.T) {
	self := Peer{Name: "warden-chat", Revision: "v1.2.0", Protocol: 2}
	if w, err := Compare(self, Peer{Name: "warden-runner", Revision: "v1.2.0", Protocol: 2}); w != "" || err != nil {
		t.Fatal(w, err)
	}
	w, err := Compare(self, Peer{Name: "warden-runner", Revision: "v1.1.0", Protocol: 2})
	if err != nil || !strings.Contains(w, "v1.1.0") || !strings.Contains(w, "v1.2.0") || !strings.Contains(w, "same protocol 2") {
		t.Fatal(w, err)
	}
	_, err = Compare(self, Peer{Name: "warden-policy", Revision: "v0.9.0", Protocol: 1})
	if err == nil || !strings.Contains(err.Error(), "warden-policy (revision v0.9.0) reports protocol 1") || !strings.Contains(err.Error(), "warden-chat (revision v1.2.0) requires protocol 2") {
		t.Fatal(err)
	}
	if _, err = Compare(self, Peer{Name: "warden-runner", Revision: "old", Protocol: 0}); err == nil || !strings.Contains(err.Error(), "reports no protocol") {
		t.Fatal(err)
	}
}

func TestRunnerAndPolicyExchanges(t *testing.T) {
	ctx := context.Background()
	runner := fakeSocket(t, "worker.sock", runnerLike(2, "r1"))
	p, err := Runner(ctx, runner)
	if err != nil || p != (Peer{Name: "warden-runner", Revision: "r1", Protocol: 2}) {
		t.Fatalf("%+v %v", p, err)
	}
	policy := fakeSocket(t, "control.sock", policyLike(2, "p1"))
	p, err = Policy(ctx, policy)
	if err != nil || p != (Peer{Name: "warden-policy", Revision: "p1", Protocol: 2}) {
		t.Fatalf("%+v %v", p, err)
	}
	// A policy service older than the handshake rejects the operation.
	older := fakeSocket(t, "old.sock", func(map[string]any) map[string]any {
		return map[string]any{"version": 1, "ok": false, "ready": false, "allow": false, "reason": "control_request_rejected"}
	})
	if _, err = Policy(ctx, older); !errors.Is(err, ErrNoHandshake) {
		t.Fatal(err)
	}
	if _, err = Runner(ctx, filepath.Join(t.TempDir(), "absent.sock")); err == nil {
		t.Fatal("absent socket answered")
	}
}

func TestVerifyRefusesOnProtocolWarnsOtherwise(t *testing.T) {
	ctx := context.Background()
	self := Peer{Name: "warden-chat", Revision: "v2", Protocol: 2}
	var warnings []string
	warn := func(s string) { warnings = append(warnings, s) }

	// Same protocol, same revision: silent.
	runner := fakeSocket(t, "worker.sock", runnerLike(2, "v2"))
	policy := fakeSocket(t, "control.sock", policyLike(2, "v2"))
	peers, err := Verify(ctx, self, runner, policy, Options{Warn: warn})
	if err != nil || len(peers) != 2 || len(warnings) != 0 {
		t.Fatalf("%v %v %v", peers, err, warnings)
	}

	// Same protocol, other revisions (a rolling update): two warnings, start.
	runner = fakeSocket(t, "worker.sock", runnerLike(2, "v1"))
	policy = fakeSocket(t, "control.sock", policyLike(2, "v1"))
	warnings = nil
	if _, err = Verify(ctx, self, runner, policy, Options{Warn: warn}); err != nil || len(warnings) != 2 {
		t.Fatalf("%v %v", err, warnings)
	}
	for _, w := range warnings {
		if !strings.Contains(w, "v1") || !strings.Contains(w, "v2") {
			t.Fatal(w)
		}
	}

	// Another protocol anywhere: refuse, naming both revisions.
	runner = fakeSocket(t, "worker.sock", runnerLike(2, "v2"))
	policy = fakeSocket(t, "control.sock", policyLike(3, "v3"))
	_, err = Verify(ctx, self, runner, policy, Options{Warn: warn})
	if err == nil || !strings.Contains(err.Error(), "v3") || !strings.Contains(err.Error(), "v2") || !strings.Contains(err.Error(), "protocol 3") {
		t.Fatal(err)
	}
	runner = fakeSocket(t, "worker.sock", runnerLike(1, "legacy"))
	if _, err = Verify(ctx, self, runner, "", Options{Warn: warn}); err == nil || !strings.Contains(err.Error(), "legacy") {
		t.Fatal(err)
	}

	// A policy service without the handshake, or an unreachable peer, is a
	// warning: deployments that worked before keep starting.
	older := fakeSocket(t, "old.sock", func(map[string]any) map[string]any {
		return map[string]any{"version": 1, "ok": false, "reason": "control_request_rejected"}
	})
	warnings = nil
	peers, err = Verify(ctx, self, "", older, Options{Warn: warn})
	if err != nil || len(peers) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "predates the version handshake") {
		t.Fatalf("%v %v %v", peers, err, warnings)
	}
	warnings = nil
	start := time.Now()
	peers, err = Verify(ctx, self, filepath.Join(t.TempDir(), "absent.sock"), "", Options{Wait: 300 * time.Millisecond, Warn: warn})
	if err != nil || len(peers) != 0 || len(warnings) != 1 || !strings.Contains(warnings[0], "could not verify the version of warden-runner") {
		t.Fatalf("%v %v %v", peers, err, warnings)
	}
	if time.Since(start) < 300*time.Millisecond {
		t.Fatal("an absent socket was not retried for the wait period")
	}
}

func TestVerifyWaitsForAPeerThatIsStillStarting(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "whs")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "worker.sock")
	go func() {
		time.Sleep(400 * time.Millisecond)
		l, err := net.Listen("unix", path)
		if err != nil {
			return
		}
		conn, err := l.Accept()
		if err != nil {
			return
		}
		bufio.NewReader(conn).ReadBytes('\n')
		json.NewEncoder(conn).Encode(map[string]any{"version": 2, "revision": "late"})
		conn.Close()
		l.Close()
	}()
	peers, err := Verify(context.Background(), Peer{Name: "warden-chat", Revision: "late", Protocol: 2}, path, "", Options{Wait: 5 * time.Second})
	if err != nil || len(peers) != 1 || peers[0].Revision != "late" {
		t.Fatalf("%v %v", peers, err)
	}
}
