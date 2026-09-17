// Package handshake is the startup version check between the Warden
// services. Every binary reports the same two facts, its build revision
// (release.Revision) and its protocol number (release.Protocol); warden-chat
// asks the runner and the policy service for theirs over their private
// transport (a Unix socket or mutual TLS, see package transport) before it
// serves anything, and `warden start` reads them from each binary's
// --version output before launching. A different protocol is
// a refusal with both revisions named; a different revision at the same
// protocol is a warning, because a deployment that updates one service at a
// time runs mixed revisions on purpose.
package handshake

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"warden/chat/internal/release"
	"warden/chat/internal/transport"
)

// Peer is one Warden binary's identity.
type Peer struct {
	Name     string
	Revision string
	Protocol int // 0 when the peer reports none
}

// Self is this binary's identity.
func Self(name string) Peer {
	return Peer{Name: name, Revision: release.Revision, Protocol: release.Protocol}
}

// String is the --version line every binary prints: "name revision protocol=N".
func (p Peer) String() string {
	if p.Protocol == 0 {
		return p.Name + " " + p.Revision
	}
	return fmt.Sprintf("%s %s protocol=%d", p.Name, p.Revision, p.Protocol)
}

var versionLine = regexp.MustCompile(`^(\S+) (\S+)(?: protocol=(\d+))?\s*$`)

// Parse reads a --version line. A binary that prints no protocol (an older
// build) yields Protocol 0.
func Parse(line string) (Peer, error) {
	first, _, _ := strings.Cut(strings.TrimSpace(line), "\n")
	m := versionLine.FindStringSubmatch(first)
	if m == nil {
		return Peer{}, fmt.Errorf("not a version line: %q", first)
	}
	p := Peer{Name: m[1], Revision: m[2]}
	if m[3] != "" {
		p.Protocol, _ = strconv.Atoi(m[3])
	}
	return p, nil
}

// Compare checks a peer against self. It returns an error when the
// protocols differ (the caller must not start) and a warning when only the
// revisions differ. A peer with Protocol 0 reported none, which is a
// mismatch: the caller cannot know whether it is compatible.
func Compare(self, peer Peer) (warning string, err error) {
	if peer.Protocol != self.Protocol {
		reported := "no protocol"
		if peer.Protocol != 0 {
			reported = "protocol " + strconv.Itoa(peer.Protocol)
		}
		return "", fmt.Errorf("%s (revision %s) reports %s; %s (revision %s) requires protocol %d: install matching Warden binaries", peer.Name, peer.Revision, reported, self.Name, self.Revision, self.Protocol)
	}
	if peer.Revision != self.Revision {
		return fmt.Sprintf("%s is revision %s while %s is %s (same protocol %d); mixed revisions run but should be updated together", peer.Name, peer.Revision, self.Name, self.Revision, self.Protocol), nil
	}
	return "", nil
}

// ErrNoHandshake reports a peer that answers but does not implement the
// version exchange: a build older than this package.
var ErrNoHandshake = errors.New("peer predates the version handshake")

// Runner asks a warden-runner at address (a unix:// or tls:// URL; t is the
// caller's material for tls://) for its identity with the worker
// protocol's "health" operation, which every runner build answers with its
// revision and protocol number.
func Runner(ctx context.Context, address string, t *transport.TLS) (Peer, error) {
	var response struct {
		Version  int    `json:"version"`
		Revision string `json:"revision"`
		Error    string `json:"error"`
	}
	request := map[string]any{"version": release.Protocol, "operation": "health"}
	if err := exchange(ctx, address, t, request, &response); err != nil {
		return Peer{}, err
	}
	p := Peer{Name: "warden-runner", Revision: response.Revision, Protocol: response.Version}
	if p.Revision == "" {
		p.Revision = "unknown"
	}
	return p, nil
}

// Policy asks a warden-policy control endpoint at address for its identity
// with the control protocol's "version" operation. A policy service that
// rejects the operation returns ErrNoHandshake.
func Policy(ctx context.Context, address string, t *transport.TLS) (Peer, error) {
	var response struct {
		OK       bool   `json:"ok"`
		Protocol int    `json:"protocol"`
		Revision string `json:"revision"`
	}
	request := map[string]any{"version": 1, "operation": "version"}
	if err := exchange(ctx, address, t, request, &response); err != nil {
		return Peer{}, err
	}
	if !response.OK {
		return Peer{Name: "warden-policy", Revision: "unknown"}, ErrNoHandshake
	}
	return Peer{Name: "warden-policy", Revision: response.Revision, Protocol: response.Protocol}, nil
}

// dialError marks a failure to reach the peer at all, which Verify retries
// while the peer may still be starting.
type dialError struct{ error }

func (e dialError) Unwrap() error { return e.error }

func exchange(ctx context.Context, address string, t *transport.TLS, request any, response any) error {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	conn, err := transport.Dial(ctx, address, transport.DialOptions{TLS: t})
	if err != nil {
		return dialError{err}
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if err = json.NewEncoder(conn).Encode(request); err != nil {
		return err
	}
	reader := bufio.NewReaderSize(conn, 65536)
	line, err := reader.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return err
	}
	return json.Unmarshal(line, response)
}

// Options control Verify.
type Options struct {
	// Wait is how long a peer that is not yet accepting connections is
	// retried before it is reported unreachable (it may still be starting).
	Wait time.Duration
	// TLS is this service's material, used for tls:// addresses.
	TLS *transport.TLS
	// Warn receives one line per warning: a revision mismatch, a peer that
	// predates the handshake, or a peer that stayed unreachable.
	Warn func(string)
}

// Verify checks the runner and the policy service (either address may be
// "" to skip it) against self. It returns an error only for a protocol
// mismatch; everything else is a warning, so a peer that is down or older
// than the handshake never stops a deployment that worked before.
func Verify(ctx context.Context, self Peer, runnerAddress, policyAddress string, o Options) ([]Peer, error) {
	warn := o.Warn
	if warn == nil {
		warn = func(string) {}
	}
	var peers []Peer
	checks := []struct {
		name, address string
		ask           func(context.Context, string, *transport.TLS) (Peer, error)
	}{
		{"warden-runner", runnerAddress, Runner},
		{"warden-policy", policyAddress, Policy},
	}
	for _, c := range checks {
		if c.address == "" {
			continue
		}
		peer, err := retry(ctx, o.Wait, func() (Peer, error) { return c.ask(ctx, c.address, o.TLS) })
		switch {
		case errors.Is(err, ErrNoHandshake):
			warn(fmt.Sprintf("%s at %s predates the version handshake (revision unknown); update it to %s", c.name, c.address, self.Revision))
			continue
		case err != nil:
			warn(fmt.Sprintf("could not verify the version of %s at %s: %v", c.name, c.address, err))
			continue
		}
		peers = append(peers, peer)
		warning, err := Compare(self, peer)
		if err != nil {
			return peers, err
		}
		if warning != "" {
			warn(warning)
		}
	}
	return peers, nil
}

func retry(ctx context.Context, wait time.Duration, ask func() (Peer, error)) (Peer, error) {
	deadline := time.Now().Add(wait)
	for {
		peer, err := ask()
		var dial dialError
		if err == nil || !errors.As(err, &dial) || time.Now().After(deadline) {
			return peer, err
		}
		select {
		case <-ctx.Done():
			return peer, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}
