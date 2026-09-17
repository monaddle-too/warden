package kube

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"log"
	"os"
	"time"

	api "warden/chat/internal/kube"
)

// TrustBundleKey is the ConfigMap key the runner mounts as
// /opt/warden/trust/ca-certificates.crt (deploy/guest/README.md).
const TrustBundleKey = "ca-certificates.crt"

// DefaultSystemBundle is where the policy container's own system CAs are
// (the server image installs ca-certificates).
const DefaultSystemBundle = "/etc/ssl/certs/ca-certificates.crt"

// TrustPublisher keeps the guest trust ConfigMap holding the complete
// bundle guests verify TLS against (decision 9): the system CAs of the
// policy container's own bundle with the gateway CA appended.
//
// The choice: the guest's mounted file replaces its system bundle, so it
// must be complete (deploy/guest/README.md). The base image's bundle is
// not readable from the policy pod, but the policy container runs the same
// Debian ca-certificates package family and its bundle is the public-CA
// set that matters; publishing it keeps the guest's public roots as
// current as the server image. Publishing the gateway CA alone would break
// every public TLS connection in the guest, so a missing or empty system
// bundle is an error, never a fallback.
//
// The ConfigMap is created by the chart and only updated here (the policy
// Role has no create on ConfigMaps): Publish reads it, replaces the key
// and writes it back with its resourceVersion, retrying a conflict.
type TrustPublisher struct {
	Client *api.Client
	// Namespace is the sandbox namespace; Name the ConfigMap
	// (kubernetes.trustConfigMap).
	Namespace, Name string
	// SystemBundle is the PEM bundle of public CAs to publish;
	// DefaultSystemBundle when empty.
	SystemBundle string
}

// Bundle assembles the guest bundle: every certificate of the system
// bundle, then the gateway CA unless the bundle already holds it.
func (p *TrustPublisher) Bundle(gatewayCA []byte) ([]byte, error) {
	path := p.SystemBundle
	if path == "" {
		path = DefaultSystemBundle
	}
	system, err := os.ReadFile(path)
	if err != nil {
		return nil, errors.New("system trust bundle: " + err.Error())
	}
	return AssembleBundle(system, gatewayCA)
}

// AssembleBundle is Bundle over the given system bundle. Certificates are
// normalised to one PEM block each; the gateway CA must be one CA
// certificate.
func AssembleBundle(system, gatewayCA []byte) ([]byte, error) {
	certs := pemCertificates(system)
	if len(certs) == 0 {
		return nil, errors.New("system trust bundle holds no certificates")
	}
	gateway := pemCertificates(gatewayCA)
	if len(gateway) != 1 {
		return nil, errors.New("the gateway CA must be one certificate")
	}
	var out bytes.Buffer
	seen := map[string]bool{}
	for _, der := range append(certs, gateway...) {
		sum := sha256.Sum256(der)
		key := hex.EncodeToString(sum[:])
		if seen[key] {
			continue
		}
		seen[key] = true
		_ = pem.Encode(&out, &pem.Block{Type: "CERTIFICATE", Bytes: der})
	}
	return out.Bytes(), nil
}

func pemCertificates(data []byte) [][]byte {
	var out [][]byte
	for {
		var block *pem.Block
		block, data = pem.Decode(data)
		if block == nil {
			return out
		}
		if block.Type == "CERTIFICATE" && len(block.Bytes) > 0 {
			out = append(out, block.Bytes)
		}
	}
}

// Publish writes the bundle for gatewayCA into the ConfigMap when it does
// not hold it already. A ConfigMap the chart has not created is an error.
func (p *TrustPublisher) Publish(ctx context.Context, gatewayCA []byte) error {
	bundle, err := p.Bundle(gatewayCA)
	if err != nil {
		return err
	}
	return p.publish(ctx, bundle)
}

func (p *TrustPublisher) publish(ctx context.Context, bundle []byte) error {
	for attempt := 0; ; attempt++ {
		var cm api.ConfigMap
		if err := p.Client.Get(ctx, api.ConfigMaps, p.Namespace, p.Name, &cm); err != nil {
			if api.IsNotFound(err) {
				return errors.New("trust ConfigMap " + p.Namespace + "/" + p.Name + " does not exist; the chart creates it and the policy service only updates it")
			}
			return err
		}
		if cm.Data[TrustBundleKey] == string(bundle) && len(cm.BinaryData[TrustBundleKey]) == 0 {
			return nil
		}
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[TrustBundleKey] = string(bundle)
		delete(cm.BinaryData, TrustBundleKey)
		err := p.Client.Update(ctx, api.ConfigMaps, p.Namespace, p.Name, &cm, nil)
		if err == nil {
			return nil
		}
		if !api.IsConflict(err) || attempt >= 4 {
			return err
		}
	}
}

// Keep republishes the bundle whenever the ConfigMap changes underneath
// (an edit, a Helm upgrade that reset it) until ctx ends. It returns when
// the watch cannot be opened.
func (p *TrustPublisher) Keep(ctx context.Context, gatewayCA []byte) error {
	bundle, err := p.Bundle(gatewayCA)
	if err != nil {
		return err
	}
	events, err := p.Client.ListWatch(ctx, api.ConfigMaps, p.Namespace, api.ListOptions{FieldSelector: "metadata.name=" + p.Name})
	if err != nil {
		return err
	}
	for ev := range events {
		switch ev.Type {
		case api.Added, api.Modified:
			var cm api.ConfigMap
			if ev.Decode(&cm) != nil || cm.Data[TrustBundleKey] == string(bundle) {
				continue
			}
		case api.Deleted:
		default:
			continue
		}
		if err := p.publish(ctx, bundle); err != nil && ctx.Err() == nil {
			log.Printf("trust bundle: republish: %v", err)
			select {
			case <-ctx.Done():
				return nil
			case <-time.After(10 * time.Second):
			}
		}
	}
	return nil
}
