package kube

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	api "warden/chat/internal/kube"
)

// selfSigned makes one CA certificate in PEM.
func selfSigned(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: cn}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// The bundle is the system CAs followed by the gateway CA, once each; an
// empty system bundle or a malformed gateway CA is refused.
func TestAssembleBundle(t *testing.T) {
	a, b, gateway := selfSigned(t, "Root A"), selfSigned(t, "Root B"), selfSigned(t, "Warden Gateway CA")
	system := append(append([]byte("# comment\n"), a...), b...)
	bundle, err := AssembleBundle(system, gateway)
	if err != nil {
		t.Fatal(err)
	}
	if got := pemCertificates(bundle); len(got) != 3 || !bytes.Equal(got[0], pemCertificates(a)[0]) || !bytes.Equal(got[2], pemCertificates(gateway)[0]) {
		t.Fatalf("bundle has %d certificates", len(got))
	}
	// Already present: not duplicated.
	again, err := AssembleBundle(bundle, gateway)
	if err != nil || !bytes.Equal(again, bundle) {
		t.Fatalf("re-assembly changed the bundle: %v", err)
	}
	if _, err := AssembleBundle(nil, gateway); err == nil || !strings.Contains(err.Error(), "no certificates") {
		t.Fatalf("empty system bundle: %v", err)
	}
	if _, err := AssembleBundle(system, []byte("not pem")); err == nil {
		t.Fatal("malformed gateway CA accepted")
	}
	if _, err := AssembleBundle(system, append(gateway, a...)); err == nil {
		t.Fatal("two gateway certificates accepted")
	}
	// Bundle reads the system file; a missing one is an error, never a
	// gateway-only bundle.
	dir := t.TempDir()
	p := &TrustPublisher{SystemBundle: filepath.Join(dir, "missing.crt")}
	if _, err := p.Bundle(gateway); err == nil || !strings.Contains(err.Error(), "system trust bundle") {
		t.Fatalf("missing system bundle: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), system, 0o644); err != nil {
		t.Fatal(err)
	}
	p.SystemBundle = filepath.Join(dir, "ca.crt")
	if got, err := p.Bundle(gateway); err != nil || !bytes.Equal(got, bundle) {
		t.Fatalf("bundle from file: %v", err)
	}
}

func newPublisher(t *testing.T, f *fakeAPI) (*TrustPublisher, []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ca.crt"), append(selfSigned(t, "Root A"), selfSigned(t, "Root B")...), 0o644); err != nil {
		t.Fatal(err)
	}
	return &TrustPublisher{Client: f.client(), Namespace: testNamespace, Name: "warden-guest-trust", SystemBundle: filepath.Join(dir, "ca.crt")}, selfSigned(t, "Warden Gateway CA")
}

// Publish updates the chart's ConfigMap in place (labels kept, no
// create), is a no-op when the content is current, and refuses a missing
// ConfigMap.
func TestPublishUpdatesTheExistingConfigMapOnly(t *testing.T) {
	f := cluster(t)
	f.put(api.ConfigMaps, testNamespace, &api.ConfigMap{Metadata: api.ObjectMeta{Name: "warden-guest-trust", Labels: map[string]string{"helm.sh/chart": "warden-0.1.0"}}})
	p, gateway := newPublisher(t, f)
	ctx := ctxT(t)
	if err := p.Publish(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	var cm api.ConfigMap
	f.object(api.ConfigMaps, testNamespace, "warden-guest-trust", &cm)
	want, _ := p.Bundle(gateway)
	if cm.Data[TrustBundleKey] != string(want) || cm.Metadata.Labels["helm.sh/chart"] != "warden-0.1.0" {
		t.Fatalf("published: %+v", cm)
	}
	if f.count("POST", "/configmaps") != 0 || f.count("PATCH", "/configmaps/warden-guest-trust") != 0 || f.count("PUT", "/configmaps/warden-guest-trust") != 1 {
		t.Fatalf("verbs: %v", f.recorded())
	}
	if err := p.Publish(ctx, gateway); err != nil || f.count("PUT", "/configmaps/warden-guest-trust") != 1 {
		t.Fatalf("no-op publish wrote: %v %d", err, f.count("PUT", "/configmaps/warden-guest-trust"))
	}
	// A rotated CA changes the bundle.
	rotated := selfSigned(t, "Warden Gateway CA 2")
	if err := p.Publish(ctx, rotated); err != nil {
		t.Fatal(err)
	}
	var after api.ConfigMap
	f.object(api.ConfigMaps, testNamespace, "warden-guest-trust", &after)
	if after.Data[TrustBundleKey] == cm.Data[TrustBundleKey] || !strings.Contains(after.Data[TrustBundleKey], strings.TrimSpace(string(rotated))) {
		t.Fatal("rotation not published")
	}
	f.remove(api.ConfigMaps, testNamespace, "warden-guest-trust")
	if err := p.Publish(ctx, gateway); err == nil || !strings.Contains(err.Error(), "does not exist") || f.count("POST", "/configmaps") != 0 {
		t.Fatalf("missing ConfigMap: %v", err)
	}
}

// A conflicting write is retried from a fresh read.
func TestPublishRetriesAConflict(t *testing.T) {
	f := cluster(t)
	p, gateway := newPublisher(t, f)
	conflicts := 0
	f.mu.Lock()
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut && conflicts < 2 {
			conflicts++
			writeStatus(w, http.StatusConflict, "Conflict", "the object has been modified")
			return true
		}
		return false
	}
	f.mu.Unlock()
	if err := p.Publish(ctxT(t), gateway); err != nil || conflicts != 2 {
		t.Fatalf("%v %d", err, conflicts)
	}
	var cm api.ConfigMap
	f.object(api.ConfigMaps, testNamespace, "warden-guest-trust", &cm)
	if !strings.Contains(cm.Data[TrustBundleKey], "BEGIN CERTIFICATE") {
		t.Fatal("not published after the retries")
	}
	f.mu.Lock()
	f.before = func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == http.MethodPut {
			writeStatus(w, http.StatusConflict, "Conflict", "the object has been modified")
			return true
		}
		return false
	}
	f.mu.Unlock()
	if err := p.Publish(ctxT(t), selfSigned(t, "Other")); err == nil || !api.IsConflict(err) {
		t.Fatalf("endless conflict: %v", err)
	}
}

// Keep republishes when the ConfigMap is changed underneath.
func TestKeepRepublishes(t *testing.T) {
	f := cluster(t)
	p, gateway := newPublisher(t, f)
	ctx := ctxT(t)
	if err := p.Publish(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.Keep(ctx, gateway) }()
	want, _ := p.Bundle(gateway)
	var cm api.ConfigMap
	f.object(api.ConfigMaps, testNamespace, "warden-guest-trust", &cm)
	cm.Data[TrustBundleKey] = "tampered"
	f.put(api.ConfigMaps, testNamespace, &cm)
	deadline := time.Now().Add(5 * time.Second)
	for {
		var now api.ConfigMap
		f.object(api.ConfigMaps, testNamespace, "warden-guest-trust", &now)
		if now.Data[TrustBundleKey] == string(want) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("tampered bundle not republished")
		}
		time.Sleep(20 * time.Millisecond)
	}
}
