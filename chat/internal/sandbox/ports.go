package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// sbxPortMapping is one row of `sbx ports --json`. Only explicit IPv4
// loopback publications are accepted: the SBX driver never exposes a guest
// port beyond the host, so any other host IP is an unregistered publication.
type sbxPortMapping struct {
	HostIP      string `json:"host_ip"`
	HostPort    int    `json:"host_port"`
	SandboxPort int    `json:"sandbox_port"`
	Protocol    string `json:"protocol"`
}

func parsePortMappings(raw []byte) ([]PortMapping, error) {
	if len(raw) > 1<<20 {
		return nil, errors.New("SBX port inventory exceeds limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var rows []sbxPortMapping
	if err := decoder.Decode(&rows); err != nil {
		return nil, fmt.Errorf("invalid SBX port inventory: %w", err)
	}
	if rows == nil {
		return nil, errors.New("SBX port inventory must be an array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing SBX port inventory data")
	}
	hosts := map[int]bool{}
	guests := map[int]bool{}
	mappings := []PortMapping{}
	for _, m := range rows {
		if m.HostIP != loopbackAddress || m.Protocol != "tcp4" || m.HostPort < 1 || m.HostPort > 65535 || m.SandboxPort < 1 || m.SandboxPort > 65535 {
			return nil, errors.New("SBX mapping is not an explicit valid IPv4 loopback publication")
		}
		if hosts[m.HostPort] || guests[m.SandboxPort] {
			return nil, errors.New("duplicate SBX port mapping")
		}
		hosts[m.HostPort] = true
		guests[m.SandboxPort] = true
		mappings = append(mappings, PortMapping{Address: m.HostIP, Port: m.HostPort, GuestPort: m.SandboxPort})
	}
	return mappings, nil
}
func (d *sbxRuntime) Mappings(ctx context.Context, name string) ([]PortMapping, error) {
	cmd := command(ctx, d.worker.Executable, "ports", name, "--json")
	var out bytes.Buffer
	cmd.Stdout = &limitedWriter{W: &out, N: 1 << 20}
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("could not inspect SBX port mappings: %w", err)
	}
	return parsePortMappings(out.Bytes())
}

// mapping is the publication as the driver reported it.
func (p *publication) mapping() PortMapping {
	return PortMapping{Address: p.Address, Port: p.HostPort, GuestPort: p.Port}
}

func (w *Worker) verifyMappingsLocked(ctx context.Context, s *managedSandbox, expected *publication, requireExpected bool) error {
	mappings, err := w.Runtime.Mappings(ctx, s.RuntimeName)
	if err != nil {
		return err
	}
	found := false
	for _, m := range mappings {
		p := w.managed.Publications[pubKey(s.ID, m.GuestPort)]
		if p == nil || p.mapping() != m {
			return errors.New("sandbox has an unregistered or mismatched port publication")
		}
		if expected != nil && m == expected.mapping() {
			found = true
		}
	}
	if requireExpected && !found {
		return errors.New("runtime did not confirm the requested port publication")
	}
	return nil
}

func (w *Worker) unpublishIfPresentLocked(ctx context.Context, s *managedSandbox, p *publication) error {
	mappings, err := w.Runtime.Mappings(ctx, s.RuntimeName)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.Port == p.HostPort && m.GuestPort == p.Port {
			if err := w.Runtime.Unpublish(ctx, s.RuntimeName, m); err != nil {
				return err
			}
			remaining, err := w.Runtime.Mappings(ctx, s.RuntimeName)
			if err != nil {
				return err
			}
			for _, current := range remaining {
				if current.Port == p.HostPort && current.GuestPort == p.Port {
					return errors.New("runtime did not remove the revoked port publication")
				}
			}
			return nil
		}
	}
	return nil
}
func (w *Worker) reconcileRemovedLocked(ctx context.Context, s *managedSandbox) error {
	if err := w.verifyMappingsLocked(ctx, s, nil, false); err != nil {
		return err
	}
	return w.unpublishRemovedLocked(ctx, s)
}

// unpublishRemovedLocked clears the endpoints of removed publications that
// the runtime may have restored on resume (SBX keeps a stopped guest's
// publications). It only touches the guest when such a publication exists.
func (w *Worker) unpublishRemovedLocked(ctx context.Context, s *managedSandbox) error {
	for _, p := range w.managed.Publications {
		if p.SandboxID == s.ID && p.State == "removed" && p.HostPort != 0 && p.Upstream != UpstreamHost {
			if err := w.unpublishIfPresentLocked(ctx, s, p); err != nil {
				return err
			}
			// Retain the removed mapping identity: SBX may restore it on resume.
		}
	}
	return nil
}
