package sandbox

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

type PortMapping struct {
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
	var mappings []PortMapping
	if err := decoder.Decode(&mappings); err != nil {
		return nil, fmt.Errorf("invalid SBX port inventory: %w", err)
	}
	if mappings == nil {
		return nil, errors.New("SBX port inventory must be an array")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("trailing SBX port inventory data")
	}
	hosts := map[int]bool{}
	guests := map[int]bool{}
	for _, m := range mappings {
		if m.HostIP != "127.0.0.1" || m.Protocol != "tcp4" || m.HostPort < 1 || m.HostPort > 65535 || m.SandboxPort < 1 || m.SandboxPort > 65535 {
			return nil, errors.New("SBX mapping is not an explicit valid IPv4 loopback publication")
		}
		if hosts[m.HostPort] || guests[m.SandboxPort] {
			return nil, errors.New("duplicate SBX port mapping")
		}
		hosts[m.HostPort] = true
		guests[m.SandboxPort] = true
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
func (w *Worker) verifyMappingsLocked(ctx context.Context, s *managedSandbox, expected *publication, requireExpected bool) error {
	mappings, err := w.Runtime.Mappings(ctx, s.RuntimeName)
	if err != nil {
		return err
	}
	found := false
	for _, m := range mappings {
		p := w.managed.Publications[pubKey(s.ID, m.SandboxPort)]
		if p == nil || p.HostPort != m.HostPort || m.HostIP != "127.0.0.1" || m.Protocol != "tcp4" {
			return errors.New("sandbox has an unregistered or mismatched port publication")
		}
		if expected != nil && m.SandboxPort == expected.Port && m.HostPort == expected.HostPort {
			found = true
		}
	}
	if requireExpected && !found {
		return errors.New("SBX did not confirm the requested loopback publication")
	}
	return nil
}

func (w *Worker) unpublishIfPresentLocked(ctx context.Context, s *managedSandbox, p *publication) error {
	mappings, err := w.Runtime.Mappings(ctx, s.RuntimeName)
	if err != nil {
		return err
	}
	for _, m := range mappings {
		if m.HostPort == p.HostPort && m.SandboxPort == p.Port {
			if err := w.Runtime.Unpublish(ctx, s.RuntimeName, p.Port, p.HostPort); err != nil {
				return err
			}
			remaining, err := w.Runtime.Mappings(ctx, s.RuntimeName)
			if err != nil {
				return err
			}
			for _, current := range remaining {
				if current.HostPort == p.HostPort && current.SandboxPort == p.Port {
					return errors.New("SBX did not remove the revoked loopback publication")
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

// unpublishRemovedLocked clears host ports of removed publications that SBX
// may have restored on resume. It only touches the guest when such a
// publication exists.
func (w *Worker) unpublishRemovedLocked(ctx context.Context, s *managedSandbox) error {
	for _, p := range w.managed.Publications {
		if p.SandboxID == s.ID && p.State == "removed" && p.HostPort != 0 {
			if err := w.unpublishIfPresentLocked(ctx, s, p); err != nil {
				return err
			}
			// Retain the removed mapping identity: SBX may restore it on resume.
		}
	}
	return nil
}
