package policysvc

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"

	"warden/chat/internal/kube"
	"warden/chat/internal/policy"
	kubepolicy "warden/chat/internal/policy/kube"
)

// kubernetesRuntime is the policy service's wiring of the kubernetes kind
// (docs/warden-kubernetes-plan.md, work item 5): the API client, the
// Secret credential store, the trust publisher, and, once the registry
// exists, the shared gateway and the inspector behind the verifier.
type kubernetesRuntime struct {
	settings  settings
	client    *kube.Client
	core      string // the core namespace: the policy pod's own
	store     *kubepolicy.SecretCredentials
	publisher *kubepolicy.TrustPublisher
	// gatewayHost is the address sandboxes reach the shared gateway at:
	// the warden-gateway Service's ClusterIP.
	gatewayHost string
	inspector   *kubepolicy.Inspector
	gateway     *policy.SharedGateway
}

// newKubernetesRuntime builds the client (in-cluster, or the kubeconfig
// named for development), the store and the publisher, and resolves the
// gateway Service's address.
func newKubernetesRuntime(s settings) (*kubernetesRuntime, error) {
	k := s.kubernetes
	if k == nil {
		return nil, errors.New("runtime.kind \"kubernetes\" requires the kubernetes section")
	}
	var cfg *kube.Config
	var err error
	if s.kubeconfig != "" {
		cfg, err = kube.LoadKubeconfig(s.kubeconfig)
	} else {
		cfg, err = kube.InClusterConfig()
	}
	if err != nil {
		return nil, errors.New("kubernetes client: " + err.Error())
	}
	client, err := kube.NewClient(cfg)
	if err != nil {
		return nil, errors.New("kubernetes client: " + err.Error())
	}
	client.UserAgent = "warden-policy"
	core := s.coreNamespace
	if core == "" {
		core = client.Namespace()
	}
	if core == "" {
		return nil, errors.New("the core namespace is unknown: run in a pod, or pass --kube-namespace with --kubeconfig")
	}
	host, err := gatewayHost(k.GatewayService)
	if err != nil {
		return nil, err
	}
	return &kubernetesRuntime{
		settings:    s,
		client:      client,
		core:        core,
		store:       &kubepolicy.SecretCredentials{Client: client, Namespace: core},
		publisher:   &kubepolicy.TrustPublisher{Client: client, Namespace: k.Namespace, Name: k.TrustConfigMap},
		gatewayHost: host,
	}, nil
}

// gatewayHost resolves the shared gateway's advertised address: an
// operator override ($WARDEN_GATEWAY_HOST, for development), else the
// Service's ClusterIP from the environment kubelet injects for Services
// that predate the pod (WARDEN_GATEWAY_SERVICE_HOST), else a DNS lookup of
// the Service name from inside the pod.
func gatewayHost(service string) (string, error) {
	if host := os.Getenv("WARDEN_GATEWAY_HOST"); host != "" {
		return host, nil
	}
	env := strings.ToUpper(strings.ReplaceAll(service, "-", "_")) + "_SERVICE_HOST"
	if host := os.Getenv(env); host != "" {
		return host, nil
	}
	addrs, err := net.LookupHost(service)
	if err != nil {
		return "", fmt.Errorf("gateway Service %s: neither $%s nor DNS resolves it (set $WARDEN_GATEWAY_HOST for development): %w", service, env, err)
	}
	for _, a := range addrs {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a, nil
		}
	}
	return addrs[0], nil
}

// credentials installs the Secret-backed provider sources on the registry
// (decision 8): the loaders read their key on every use, so a rotated
// Secret takes effect without a restart.
func (r *kubernetesRuntime) credentials(registry *policy.Registry) {
	s := r.settings
	if s.codexSecret != "" {
		registry.ProviderSource = &policy.CodexCredentials{Store: r.store, Name: kubepolicy.CredentialName(s.codexSecret, kubepolicy.CodexAuthKey)}
	}
	if s.claudeSecret != "" {
		registry.ClaudeSource = &policy.ClaudeCredentials{Store: r.store, Name: kubepolicy.CredentialName(s.claudeSecret, kubepolicy.ClaudeAuthKey)}
	}
}

// publishTrust writes the guest trust bundle (decision 9) before the
// control listener is served, then keeps it published. A failure is fatal:
// the runner creates no sandbox until the bundle is there, and a policy
// service that cannot publish it has nothing to offer.
func (r *kubernetesRuntime) publishTrust(ctx context.Context, ca *policy.GatewayCA) error {
	if err := r.publisher.Publish(ctx, ca.CertPEM); err != nil {
		return errors.New("guest trust bundle: " + err.Error())
	}
	go func() {
		if err := r.publisher.Keep(ctx, ca.CertPEM); err != nil && ctx.Err() == nil {
			fmt.Printf("guest trust bundle: watch ended: %v\n", err)
		}
	}()
	return nil
}

// enforce wires the shared gateway and the inspector into the registry
// and starts the verifier: one listener on every interface at the gateway
// port, advertised as the Service's ClusterIP; the inspector's watches
// (until ctx ends, which also abandons a canary proof in flight); the
// verifier's refresher, whose first cycle proves the cluster with the
// canaries.
func (r *kubernetesRuntime) enforce(ctx context.Context, registry *policy.Registry, networks []*net.IPNet) error {
	k := r.settings.kubernetes
	listener, err := net.Listen("tcp", net.JoinHostPort("0.0.0.0", strconv.Itoa(k.GatewayPort)))
	if err != nil {
		return errors.New("shared gateway: " + err.Error())
	}
	gateway, err := policy.NewSharedGateway(registry, networks, listener, r.gatewayHost)
	if err != nil {
		listener.Close()
		return errors.New("shared gateway: " + err.Error())
	}
	registry.Gateways = gateway
	inspector, err := kubepolicy.New(kubepolicy.Options{
		Client:           r.client,
		Namespace:        k.Namespace,
		CoreNamespace:    r.core,
		Tier:             k.Tier,
		RuntimeClass:     k.RuntimeClass,
		GuestImage:       k.GuestImage,
		GuestImageDigest: r.settings.guestDigest,
		GatewayPort:      k.GatewayPort,
		TrustConfigMap:   k.TrustConfigMap,
		State:            r.settings.state,
		StorageFailed:    func() { registry.StorageFailed = true },
		Canary:           kubepolicy.CanaryOptions{Image: os.Getenv("WARDEN_CANARY_IMAGE")},
	})
	if err != nil {
		return errors.New("inspector: " + err.Error())
	}
	inspector.SetGateway(r.gatewayHost)
	if err := inspector.Start(ctx); err != nil {
		return errors.New("inspector: " + err.Error())
	}
	// Decision 4's second check: a credential only from its own pod.
	gateway.Source = inspector.SourceAllowed
	verifier := policy.NewRuntimeVerifier(registry, inspector)
	registry.Verifier = verifier
	verifier.StartRefresher()
	r.inspector, r.gateway = inspector, gateway
	return nil
}
