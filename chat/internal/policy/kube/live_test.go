package kube

import (
	"context"
	"os"
	"testing"
	"time"

	api "warden/chat/internal/kube"
)

// TestLiveCanaryProof runs the canary proof against a real cluster named by
// WARDEN_LIVE_KUBECONFIG (the dev VM of docs/warden-kubernetes-plan.md,
// appendix B), in the namespace WARDEN_LIVE_NAMESPACE (default
// warden-spike-sandboxes), against the gateway address WARDEN_LIVE_GATEWAY
// (host the labelled canary must reach on WARDEN_LIVE_GATEWAY_PORT, 7000).
// It creates two short-lived pods and deletes them. Skipped otherwise.
func TestLiveCanaryProof(t *testing.T) {
	kubeconfig := os.Getenv("WARDEN_LIVE_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("WARDEN_LIVE_KUBECONFIG unset")
	}
	namespace := os.Getenv("WARDEN_LIVE_NAMESPACE")
	if namespace == "" {
		namespace = "warden-spike-sandboxes"
	}
	gateway := os.Getenv("WARDEN_LIVE_GATEWAY")
	if gateway == "" {
		t.Fatal("WARDEN_LIVE_GATEWAY (the gateway address) is required")
	}
	cfg, err := api.LoadKubeconfig(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	i, err := New(Options{
		Client: client, Namespace: namespace, CoreNamespace: "warden-spike", Tier: TierGVisor, RuntimeClass: "gvisor",
		GuestImageDigest: testDigest, GatewayPort: 7000, State: t.TempDir(),
		Canary: CanaryOptions{GatewayHost: gateway, APIServer: "10.43.0.1:443", Timeout: 90 * time.Second},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	start := time.Now()
	err = i.checkCanaries(ctx)
	t.Logf("canary proof in %s: %v", time.Since(start).Round(time.Millisecond), err)
	if err != nil {
		t.Fatal(err)
	}
	// The static checks against the spike's objects: the namespace and the
	// runtime class pass; the policies' peers carry the spike's labels,
	// which the chart's shape check refuses.
	for name, check := range map[string]func(context.Context) error{"namespace": i.checkNamespace, "runtime class": i.checkRuntimeClass, "network policies": i.checkNetworkPolicies, "admission": i.checkAdmission} {
		t.Logf("%s: %v", name, check(ctx))
	}
}

// TestLiveTrustSecretsAndWatches exercises the publisher, the Secret store
// and the inspector's watches against the live cluster with throwaway
// objects it creates and deletes itself.
func TestLiveTrustSecretsAndWatches(t *testing.T) {
	kubeconfig := os.Getenv("WARDEN_LIVE_KUBECONFIG")
	if kubeconfig == "" {
		t.Skip("WARDEN_LIVE_KUBECONFIG unset")
	}
	cfg, err := api.LoadKubeconfig(kubeconfig)
	if err != nil {
		t.Fatal(err)
	}
	client, err := api.NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const sandboxNS, coreNS = "warden-spike-sandboxes", "warden-spike"
	// The chart-created (here: test-created) empty ConfigMap.
	if err := client.Create(ctx, api.ConfigMaps, sandboxNS, &api.ConfigMap{Metadata: api.ObjectMeta{Name: "warden-live-trust"}}, nil); err != nil && !api.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	defer client.Delete(context.Background(), api.ConfigMaps, sandboxNS, "warden-live-trust", api.DeleteOptions{})
	if err := client.Create(ctx, api.Secrets, coreNS, &api.Secret{Metadata: api.ObjectMeta{Name: "warden-live-login"}, Type: "Opaque", Data: map[string][]byte{CodexAuthKey: []byte("v1")}}, nil); err != nil && !api.IsAlreadyExists(err) {
		t.Fatal(err)
	}
	defer client.Delete(context.Background(), api.Secrets, coreNS, "warden-live-login", api.DeleteOptions{})

	dir := t.TempDir()
	system := selfSigned(t, "Root A")
	if err := os.WriteFile(dir+"/ca.crt", system, 0o644); err != nil {
		t.Fatal(err)
	}
	p := &TrustPublisher{Client: client, Namespace: sandboxNS, Name: "warden-live-trust", SystemBundle: dir + "/ca.crt"}
	gateway := selfSigned(t, "Warden Gateway CA")
	if err := p.Publish(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	var cm api.ConfigMap
	if err := client.Get(ctx, api.ConfigMaps, sandboxNS, "warden-live-trust", &cm); err != nil || len(pemCertificates([]byte(cm.Data[TrustBundleKey]))) != 2 {
		t.Fatalf("published bundle: %v %d", err, len(pemCertificates([]byte(cm.Data[TrustBundleKey]))))
	}
	if err := p.Publish(ctx, gateway); err != nil {
		t.Fatal(err)
	}
	p.Name = "warden-live-missing"
	if err := p.Publish(ctx, gateway); err == nil {
		t.Fatal("published into a missing ConfigMap")
	}
	t.Logf("trust: published %d certificates, update-only confirmed", 2)

	s := &SecretCredentials{Client: client, Namespace: coreNS}
	changes, err := s.Watch(ctx, "warden-live-login/auth.json")
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(500 * time.Millisecond)
	if err := s.Store(ctx, "warden-live-login/auth.json", []byte("v2")); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changes:
	case <-time.After(10 * time.Second):
		t.Fatal("rotation not signalled")
	}
	if data, err := s.Load(ctx, "warden-live-login/auth.json"); err != nil || string(data) != "v2" {
		t.Fatalf("load: %s %v", data, err)
	}
	t.Log("secrets: load, store and watch by name work")

	i, err := New(Options{Client: client, Namespace: sandboxNS, CoreNamespace: coreNS, Tier: TierGVisor, RuntimeClass: "gvisor", GuestImageDigest: testDigest, GatewayPort: 7000, State: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if err := i.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Log("watches: network policies, namespace, runtime class, admission policies and bindings opened")
}
