package kube

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	api "warden/chat/internal/kube"
	"warden/chat/internal/policy"
)

// Options configure an Inspector. Client, Namespace, Tier, RuntimeClass,
// GuestImageDigest and GatewayPort are required.
type Options struct {
	Client *api.Client
	// Namespace is the sandbox namespace (kubernetes.namespace).
	Namespace string
	// CoreNamespace holds the policy pod; the gateway-egress policy must
	// point at it. Empty means the client's own namespace.
	CoreNamespace string
	// Tier is kata or gvisor (kubernetes.tier); RuntimeClass the pinned
	// runtimeClassName; Handlers overrides the tier's handler allow-list.
	Tier         string
	RuntimeClass string
	Handlers     []string
	// GuestImageDigest pins what a sandbox pod runs: a digest in the
	// spec's image reference must be it, and the running container's
	// imageID must carry it (an imported image is named by tag in the spec
	// and reports the digest once running). GuestImage, when set, is the
	// repository a tag reference must name.
	GuestImage       string
	GuestImageDigest string
	// GatewayPort is the shared gateway's port, which the gateway-egress
	// policy must allow and GrantEgress is asked for.
	GatewayPort int
	// TrustConfigMap is the only ConfigMap a sandbox pod may mount.
	TrustConfigMap string
	// State is the policy state directory; the identity pins live there.
	State string
	// StorageFailed is told when the pins cannot be written.
	StorageFailed func()
	// Canary configures the canary proof; zero values take the defaults.
	Canary CanaryOptions
	// RequireBindingLabel makes LabelBinding mandatory on sandbox pods.
	RequireBindingLabel bool
	// PassTTL is how long a passed cluster proof is reused before the
	// checks and canaries run again without a watch event; an hour when
	// zero. FailureTTL bounds how often a failing cluster is re-proved; a
	// minute when zero.
	PassTTL, FailureTTL time.Duration
	// Now is the clock; time.Now when nil.
	Now func() time.Time
}

// Inspector is the SandboxInspector of the kubernetes kind.
type Inspector struct {
	o        Options
	handlers []string
	pinsPath string
	now      func() time.Time

	mu   sync.Mutex // guards pins
	pins map[string]*identityPin

	// The cached cluster proof (clusterMu): computed lazily by ClusterFacts,
	// invalidated by watch events, the hourly timer and Invalidate.
	clusterMu   sync.Mutex
	clusterErr  error
	clusterAt   time.Time
	clusterOK   bool
	clusterDone bool

	// gateway is the address the canaries must reach; set by SetGateway
	// (the shared gateway's advertised Service IP) or Canary.GatewayHost.
	gatewayMu   sync.Mutex
	gatewayHost string

	// stopCtx ends with Start's context; a canary proof in flight is
	// abandoned (and its pods deleted) when the service stops.
	stopMu  sync.Mutex
	stopCtx context.Context
}

// identityPin is one runtime's pinned identity: the workspace volume UID
// and the pod UID of each generation (decision 3).
type identityPin struct {
	Volume      string            `json:"volume"`
	Generations map[string]string `json:"generations"`
}

const pinsFile = "kube-runtime-identities.json"

// New builds an inspector and loads its identity pins from State.
func New(o Options) (*Inspector, error) {
	if o.Client == nil {
		return nil, errors.New("kube inspector: a client is required")
	}
	if o.Namespace == "" || o.RuntimeClass == "" || o.GatewayPort <= 0 {
		return nil, errors.New("kube inspector: namespace, runtimeClass and gatewayPort are required")
	}
	if !policy.ValidImageDigest(o.GuestImageDigest) {
		return nil, errors.New("kube inspector: guestImageDigest must be sha256:<64 hex>")
	}
	handlers := o.Handlers
	if len(handlers) == 0 {
		handlers = DefaultHandlers[o.Tier]
	}
	if len(handlers) == 0 {
		return nil, errors.New("kube inspector: unknown tier " + o.Tier + " and no handler allow-list")
	}
	if o.CoreNamespace == "" {
		o.CoreNamespace = o.Client.Namespace()
	}
	if o.CoreNamespace == "" {
		return nil, errors.New("kube inspector: the core namespace is unknown")
	}
	if o.TrustConfigMap == "" {
		o.TrustConfigMap = "warden-guest-trust"
	}
	if o.PassTTL <= 0 {
		o.PassTTL = time.Hour
	}
	if o.FailureTTL <= 0 {
		o.FailureTTL = time.Minute
	}
	if o.State == "" {
		return nil, errors.New("kube inspector: a state directory is required")
	}
	i := &Inspector{o: o, handlers: handlers, pinsPath: filepath.Join(o.State, pinsFile), pins: map[string]*identityPin{}, now: o.Now}
	if i.now == nil {
		i.now = time.Now
	}
	if raw, err := os.ReadFile(i.pinsPath); err == nil {
		if err := json.Unmarshal(raw, &i.pins); err != nil {
			return nil, errors.New("kube inspector: " + pinsFile + ": " + err.Error())
		}
		for _, pin := range i.pins {
			if pin != nil && pin.Generations == nil {
				pin.Generations = map[string]string{}
			}
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return i, nil
}

// SetGateway records the address the labelled canary must reach: the
// shared gateway's advertised host (the warden-gateway Service IP).
func (i *Inspector) SetGateway(host string) {
	i.gatewayMu.Lock()
	defer i.gatewayMu.Unlock()
	i.gatewayHost = host
}

func (i *Inspector) gateway() string {
	i.gatewayMu.Lock()
	defer i.gatewayMu.Unlock()
	if i.gatewayHost != "" {
		return i.gatewayHost
	}
	return i.o.Canary.GatewayHost
}

// Tier is the configured isolation tier.
func (i *Inspector) Tier() string { return i.o.Tier }

// pin records the runtime's volume identity the first time and the pod UID
// of each generation, and refuses a change of either: a replaced volume
// under a registered name is fatal, as is a replaced pod within one
// generation. A new generation on the same volume is expected.
func (i *Inspector) pin(name, generation, volume, pod string) error {
	if volume == "" || pod == "" {
		return errors.New("missing runtime identity")
	}
	i.mu.Lock()
	defer i.mu.Unlock()
	current := i.pins[name]
	if current != nil {
		if current.Volume != volume {
			return errors.New("runtime identity changed: workspace volume replaced")
		}
		if pinned, ok := current.Generations[generation]; ok {
			if pinned != pod {
				return errors.New("runtime replaced within its generation")
			}
			return nil
		}
	}
	updated := map[string]*identityPin{}
	for k, v := range i.pins {
		copied := &identityPin{Volume: v.Volume, Generations: map[string]string{}}
		for g, p := range v.Generations {
			copied.Generations[g] = p
		}
		updated[k] = copied
	}
	if updated[name] == nil {
		updated[name] = &identityPin{Volume: volume, Generations: map[string]string{}}
	}
	updated[name].Generations[generation] = pod
	// Generations are unbounded in principle; keep the pins file small by
	// forgetting the oldest once a runtime has seen many.
	if len(updated[name].Generations) > 64 {
		keys := make([]string, 0, len(updated[name].Generations))
		for g := range updated[name].Generations {
			if g != generation {
				keys = append(keys, g)
			}
		}
		sort.Strings(keys)
		for _, g := range keys[:len(keys)-32] {
			delete(updated[name].Generations, g)
		}
	}
	raw, err := json.MarshalIndent(updated, "", "  ")
	if err != nil {
		return err
	}
	if err := atomicWrite(i.pinsPath, raw); err != nil {
		if i.o.StorageFailed != nil {
			i.o.StorageFailed()
		}
		return err
	}
	i.pins = updated
	return nil
}

func atomicWrite(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// findPod returns the runtime's pod: the one pod in the namespace labelled
// LabelSandbox=<runtimeName> that is not being deleted (a terminating pod
// of a stopped generation may linger beside its successor). Two live pods
// under one name is an error; none is nil.
func (i *Inspector) findPod(ctx context.Context, name string) (*api.Pod, error) {
	if name == "" {
		return nil, errors.New("missing runtime name")
	}
	var list api.List[api.Pod]
	if err := i.o.Client.List(ctx, api.Pods, i.o.Namespace, api.ListOptions{LabelSelector: LabelSandbox + "=" + name}, &list); err != nil {
		return nil, err
	}
	var live, terminating []*api.Pod
	for idx := range list.Items {
		pod := &list.Items[idx]
		if pod.Metadata.Labels[LabelSandbox] != name {
			continue
		}
		if pod.Metadata.DeletionTimestamp != nil {
			terminating = append(terminating, pod)
		} else {
			live = append(live, pod)
		}
	}
	switch {
	case len(live) > 1:
		return nil, errors.New("ambiguous runtime identity")
	case len(live) == 1:
		return live[0], nil
	case len(terminating) == 1:
		return terminating[0], nil
	case len(terminating) > 1:
		return nil, errors.New("ambiguous runtime identity")
	}
	return nil, nil
}

// Facts inspects the binding's pod: presence, the structural checks of
// decision 2, the image digest, the identity pins of decision 3 and the
// egress state of decision 4.
func (i *Inspector) Facts(ctx context.Context, identity map[string]string) (policy.RuntimeFacts, error) {
	facts := policy.RuntimeFacts{Tier: i.o.Tier, Egress: policy.EgressDenied}
	pod, err := i.findPod(ctx, identity["runtimeName"])
	if err != nil {
		return facts, err
	}
	if pod == nil {
		return facts, nil
	}
	facts.Present = true
	facts.Violations = i.violations(pod, identity)
	facts.ImageDigest = imageDigest(pod)
	volume := workspaceClaim(pod)
	if volume == "" {
		facts.Violations = append(facts.Violations, "pod has no workspace volume")
	} else {
		var pvc api.PersistentVolumeClaim
		if err := i.o.Client.Get(ctx, api.PersistentVolumeClaims, i.o.Namespace, volume, &pvc); err != nil {
			if !api.IsNotFound(err) {
				return facts, err
			}
			facts.Violations = append(facts.Violations, "pod's workspace volume does not exist")
		} else {
			if err := i.pin(identity["runtimeName"], identity["generation"], pvc.Metadata.UID, pod.Metadata.UID); err != nil {
				return facts, err
			}
			facts.Identity = pvc.Metadata.UID
		}
	}
	egress, err := i.egressState(ctx, pod)
	if err != nil {
		return facts, err
	}
	facts.Egress = egress
	return facts, nil
}

// violations are the structural findings on a pod (decision 2). Every
// finding is reported; the verifier fails on the first.
func (i *Inspector) violations(pod *api.Pod, identity map[string]string) []string {
	var out []string
	add := func(s string) { out = append(out, s) }
	spec := pod.Spec
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != i.o.RuntimeClass {
		add("pod runs an unpinned RuntimeClass")
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		add("pod shares a host namespace")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		add("pod automounts a service account token")
	}
	if len(spec.Containers) != 1 || len(spec.InitContainers) != 0 {
		add("pod runs extra containers")
	}
	for _, c := range append(append([]api.Container{}, spec.Containers...), spec.InitContainers...) {
		if c.SecurityContext != nil && c.SecurityContext.Privileged != nil && *c.SecurityContext.Privileged {
			add("pod runs a privileged container")
			break
		}
	}
	claims := 0
	for _, v := range spec.Volumes {
		switch {
		case v.HostPath != nil:
			add("pod mounts a hostPath volume")
		case v.Projected != nil:
			add("pod mounts a projected volume")
		case v.Secret != nil:
			add("pod mounts a Secret volume")
		case v.PersistentVolumeClaim != nil:
			claims++
		case v.ConfigMap != nil:
			if v.ConfigMap.Name != i.o.TrustConfigMap {
				add("pod mounts an unexpected ConfigMap")
			}
		case v.EmptyDir != nil:
		default:
			add("pod mounts an unknown volume type")
		}
	}
	if claims > 1 {
		add("pod mounts extra volumes")
	}
	if len(spec.Containers) > 0 {
		// The spec may pin the digest (a registry image) or name a tag the
		// node already holds (an imported image); the running container's
		// imageID is the digest either way (spike results) and is the pin.
		if spec := digestOf(spec.Containers[0].Image); spec != "" && spec != i.o.GuestImageDigest {
			add("pod runs an unpinned image")
		} else if spec == "" && i.o.GuestImage != "" && !sameRepository(pod.Spec.Containers[0].Image, i.o.GuestImage) {
			add("pod runs an unpinned image")
		} else if id := imageDigest(pod); id != "" && id != i.o.GuestImageDigest {
			add("pod runs an unpinned image")
		} else if id == "" && pod.Status.Phase == "Running" {
			add("pod image digest not reported")
		}
	}
	labels := pod.Metadata.Labels
	if labels[LabelSandbox] == "" {
		add("pod lacks the sandbox label")
	}
	if binding, ok := labels[LabelBinding]; ok {
		if binding != policy.BindingDigest(identity) {
			add("pod is labelled for another binding")
		}
	} else if i.o.RequireBindingLabel {
		add("pod lacks the binding label")
	}
	if value, ok := labels[LabelEgress]; ok && value != EgressGateway {
		add("pod carries an unexpected egress label")
	}
	if _, ok := labels[LabelCanary]; ok {
		add("pod is a canary")
	}
	if generation, ok := pod.Metadata.Annotations[AnnotationGeneration]; ok && generation != identity["generation"] {
		add("pod belongs to another generation")
	}
	return out
}

// workspaceClaim is the name of the pod's PVC (its identity), or "".
func workspaceClaim(pod *api.Pod) string {
	for _, v := range pod.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			return v.PersistentVolumeClaim.ClaimName
		}
	}
	return ""
}

// imageDigest is the sha256 digest of the pod's first container as its
// status reports it: "repo@sha256:..." from a registry, or the bare digest
// for an image imported into the node's containerd.
func imageDigest(pod *api.Pod) string {
	for _, s := range pod.Status.ContainerStatuses {
		if len(pod.Spec.Containers) > 0 && s.Name != pod.Spec.Containers[0].Name {
			continue
		}
		return digestOf(s.ImageID)
	}
	return ""
}

// digestOf extracts a sha256 digest from an image ID or reference; "" when
// it has none.
func digestOf(ref string) string {
	if idx := strings.LastIndex(ref, "@"); idx >= 0 {
		ref = ref[idx+1:]
	}
	if strings.HasPrefix(ref, "docker-pullable://") {
		ref = strings.TrimPrefix(ref, "docker-pullable://")
	}
	if policy.ValidImageDigest(ref) {
		return ref
	}
	return ""
}

// sameRepository reports whether two image references name the same
// repository, ignoring tags, digests and Docker Hub's implied prefixes.
func sameRepository(a, b string) bool {
	return repositoryOf(a) == repositoryOf(b)
}

func repositoryOf(ref string) string {
	if idx := strings.Index(ref, "@"); idx >= 0 {
		ref = ref[:idx]
	}
	slash := strings.LastIndex(ref, "/")
	if colon := strings.LastIndex(ref, ":"); colon > slash {
		ref = ref[:colon]
	}
	ref = strings.TrimPrefix(ref, "docker.io/")
	ref = strings.TrimPrefix(ref, "library/")
	return ref
}

// egressState derives the egress fact from the label and the policies that
// select the pod: EgressGateway when the label is set and only the static
// policies select it, EgressDenied when it is absent and nothing but the
// static policies select it, EgressOther otherwise.
func (i *Inspector) egressState(ctx context.Context, pod *api.Pod) (string, error) {
	var list api.List[api.NetworkPolicy]
	if err := i.o.Client.List(ctx, api.NetworkPolicies, i.o.Namespace, api.ListOptions{}, &list); err != nil {
		return policy.EgressOther, err
	}
	selecting := map[string]bool{}
	for _, np := range list.Items {
		if np.Spec.PodSelector.Matches(pod.Metadata.Labels) {
			if !StaticPolicies[np.Metadata.Name] {
				return policy.EgressOther, nil
			}
			selecting[np.Metadata.Name] = true
		}
	}
	if !selecting[PolicyDefaultDeny] {
		return policy.EgressOther, nil
	}
	labelled := pod.Metadata.Labels[LabelEgress] == EgressGateway
	switch {
	case labelled && selecting[PolicyGatewayEgress]:
		return policy.EgressGateway, nil
	case !labelled && !selecting[PolicyGatewayEgress]:
		return policy.EgressDenied, nil
	}
	return policy.EgressOther, nil
}

// GrantEgress sets the egress label on the binding's pod (or confirms it)
// and reads the pod back. The endpoint must be the shared gateway's port;
// the label admits exactly that destination.
func (i *Inspector) GrantEgress(ctx context.Context, identity map[string]string, gateway policy.GatewayEndpoint) error {
	if gateway.Port != i.o.GatewayPort {
		return errors.New("gateway endpoint is not the shared gateway")
	}
	pod, err := i.findPod(ctx, identity["runtimeName"])
	if err != nil {
		return err
	}
	if pod == nil {
		return errors.New("runtime absent")
	}
	if pod.Metadata.Labels[LabelEgress] != EgressGateway {
		var updated api.Pod
		if err := i.o.Client.Patch(ctx, api.Pods, i.o.Namespace, pod.Metadata.Name, api.MergePatchLabels(map[string]string{LabelEgress: EgressGateway}), &updated); err != nil {
			return err
		}
		pod = &updated
	}
	if pod.Metadata.Labels[LabelEgress] != EgressGateway {
		return errors.New("gateway policy transition failed")
	}
	return nil
}

// DenyEgress removes the egress label from the binding's pod and reads it
// back. An absent pod needs no deny.
func (i *Inspector) DenyEgress(ctx context.Context, identity map[string]string) error {
	pod, err := i.findPod(ctx, identity["runtimeName"])
	if err != nil || pod == nil {
		return err
	}
	if _, ok := pod.Metadata.Labels[LabelEgress]; !ok {
		return nil
	}
	var updated api.Pod
	if err := i.o.Client.Patch(ctx, api.Pods, i.o.Namespace, pod.Metadata.Name, api.MergePatchLabels(nil, LabelEgress), &updated); err != nil {
		if api.IsNotFound(err) {
			return nil
		}
		return err
	}
	if _, ok := updated.Metadata.Labels[LabelEgress]; ok {
		return errors.New("egress label not removed")
	}
	return nil
}
