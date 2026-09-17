package kube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"

	"warden/chat/internal/config"
	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// Driver implements sandbox.RuntimeDriver over one sandbox namespace. It
// is safe for concurrent use. What it records about a runtime (the claim
// UID, the pod UID with its generation, the pod IP, the publications)
// lives in memory: the worker's registry is the durable state, the policy
// service pins the identities, and a restarted driver rebuilds its view in
// Reconcile.
type Driver struct {
	client *kube.Client
	opts   Options

	mu         sync.Mutex
	runtimes   map[string]*runtime
	trustReady bool
	// exec retries and waits are shortened by tests.
	execRetry time.Duration
}

// runtime is the driver's view of one guest.
type runtime struct {
	claimUID     string
	podUID       string
	podIP        string
	generation   string
	workspace    string // how the workspace was made: fresh, clone or copy
	publications []sandbox.PortMapping
}

// Runtime is the exported view of a guest the driver knows.
type Runtime struct {
	ClaimUID, PodUID, PodIP, Generation string
	// Workspace is "fresh", "clone" (a CSI volume clone) or "copy" (a tar
	// copy through exec from the source pod, decision 8's fallback).
	Workspace string
}

// Workspace provenance values.
const (
	WorkspaceFresh = "fresh"
	WorkspaceClone = "clone"
	WorkspaceCopy  = "copy"
)

// New builds a driver for the namespace and settings in opts.
func New(client *kube.Client, opts Options) (*Driver, error) {
	if client == nil {
		return nil, errors.New("kube driver: a client is required")
	}
	if opts.Namespace == "" || opts.RuntimeClass == "" || opts.GuestImage == "" || opts.GuestImageDigest == "" || opts.TrustConfigMap == "" {
		return nil, errors.New("kube driver: namespace, runtimeClass, guestImage, guestImageDigest and trustConfigMap are required")
	}
	if opts.Tier != config.TierGVisor && opts.Tier != config.TierKata {
		return nil, fmt.Errorf("kube driver: tier must be %q or %q", config.TierGVisor, config.TierKata)
	}
	if m := opts.memoryMB(); m < 512 || m > 16384 {
		return nil, errors.New("sandbox memory must be 512–16384 MiB")
	}
	return &Driver{client: client, opts: opts, runtimes: map[string]*runtime{}, execRetry: 500 * time.Millisecond}, nil
}

// Runtime reports what the driver knows about a guest.
func (d *Driver) Runtime(name string) (Runtime, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	rt := d.runtimes[name]
	if rt == nil {
		return Runtime{}, false
	}
	return Runtime{ClaimUID: rt.claimUID, PodUID: rt.podUID, PodIP: rt.podIP, Generation: rt.generation, Workspace: rt.workspace}, true
}

func (d *Driver) record(name string) *runtime {
	d.mu.Lock()
	defer d.mu.Unlock()
	rt := d.runtimes[name]
	if rt == nil {
		rt = &runtime{}
		d.runtimes[name] = rt
	}
	return rt
}

// launchOptions is the agent launch variation of the tier (decision 14).
func (d *Driver) launchOptions() sandbox.LaunchOptions {
	if d.opts.Tier == config.TierGVisor {
		return sandbox.LaunchOptions{CodexSandboxMode: "danger-full-access"}
	}
	return sandbox.LaunchOptions{}
}

// Create makes the runtime resident: the workspace claim (cloned from the
// source's claim for a fork, when the storage supports it), then a pod on
// it, waited for until it runs with an address, then the image's manifest
// read and checked. It is idempotent: an existing claim is reused and an
// existing pod with this driver's labels is adopted (a terminating one is
// waited out first). The first Create waits for the trust bundle
// (decision 9).
func (d *Driver) Create(ctx context.Context, spec sandbox.RuntimeSpec) error {
	if err := validName(spec.Name); err != nil {
		return err
	}
	if spec.Source != "" {
		if err := validName(spec.Source); err != nil {
			return fmt.Errorf("fork source: %w", err)
		}
	}
	if err := d.awaitTrust(ctx); err != nil {
		return err
	}
	rt := d.record(spec.Name)
	claim, err := d.ensureClaim(ctx, spec)
	if err != nil {
		return err
	}
	d.mu.Lock()
	if rt.claimUID != "" && rt.claimUID != claim.Metadata.UID {
		d.mu.Unlock()
		return fmt.Errorf("sandbox %s: workspace volume identity changed (%s, was %s)", spec.Name, claim.Metadata.UID, rt.claimUID)
	}
	rt.claimUID = claim.Metadata.UID
	d.mu.Unlock()
	workspace := WorkspaceFresh
	if spec.Source != "" {
		workspace = WorkspaceClone
	}
	pod, err := d.ensurePod(ctx, spec, workspace)
	if err != nil {
		return err
	}
	pod, err = d.awaitRunning(ctx, spec.Name, pod.Metadata.UID)
	if err != nil {
		return err
	}
	if err = d.checkManifest(ctx, spec.Name); err != nil {
		return err
	}
	if spec.Source != "" {
		// A clone the storage did not perform (a provisioner that ignores
		// dataSource yields an empty volume) or a copy interrupted earlier
		// leaves the workspace directory missing: copy from the source pod.
		if _, cerr := d.run(ctx, spec.Name, nil, 0, "test", "-d", spec.Directory); cerr != nil {
			if err = d.copyHome(ctx, spec.Source, spec.Name); err != nil {
				return err
			}
			workspace = WorkspaceCopy
		}
	}
	d.mu.Lock()
	rt.podUID, rt.podIP, rt.generation, rt.workspace = pod.Metadata.UID, pod.Status.PodIP, spec.Generation, workspace
	rt.publications = nil
	d.mu.Unlock()
	log.Printf("sandbox %s: pod %s running at %s (claim %s, workspace %s)", spec.Name, pod.Metadata.UID, pod.Status.PodIP, claim.Metadata.UID, workspace)
	return nil
}

// ensureClaim returns the runtime's workspace claim, creating it when
// missing. A fork's claim is created with a dataSource first; a server that
// refuses the clone gets a plain claim and Create copies instead.
func (d *Driver) ensureClaim(ctx context.Context, spec sandbox.RuntimeSpec) (*kube.PersistentVolumeClaim, error) {
	var claim kube.PersistentVolumeClaim
	err := d.client.Get(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, spec.Name, &claim)
	if err == nil && claim.Metadata.DeletionTimestamp != nil {
		// Removed and recreated in quick succession: let the deletion finish.
		if err = d.awaitGone(ctx, kube.PersistentVolumeClaims, spec.Name); err != nil {
			return nil, err
		}
		err = d.client.Get(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, spec.Name, &claim)
	}
	if err == nil {
		if claim.Metadata.Labels[LabelSandbox] != spec.Name || claim.Metadata.Labels[LabelManagedBy] != ManagedBy {
			return nil, fmt.Errorf("sandbox %s: an unmanaged volume claim holds its name", spec.Name)
		}
		return &claim, nil
	}
	if !kube.IsNotFound(err) {
		return nil, fmt.Errorf("sandbox %s: workspace claim: %w", spec.Name, err)
	}
	desired := ClaimSpec(d.opts, spec.Name, spec.SandboxID, spec.Source, spec.Spare)
	err = d.client.Create(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, desired, &claim)
	if err != nil && spec.Source != "" && !kube.IsAlreadyExists(err) && isStatus(err) {
		log.Printf("sandbox %s: volume clone from %s refused (%v); copying instead", spec.Name, spec.Source, err)
		desired = ClaimSpec(d.opts, spec.Name, spec.SandboxID, "", spec.Spare)
		err = d.client.Create(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, desired, &claim)
	}
	if kube.IsAlreadyExists(err) {
		err = d.client.Get(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, spec.Name, &claim)
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: workspace claim: %w", spec.Name, err)
	}
	return &claim, nil
}

// ensurePod returns the runtime's pod, creating it when missing. An
// existing pod must carry this driver's labels; one being deleted is
// waited out and replaced.
func (d *Driver) ensurePod(ctx context.Context, spec sandbox.RuntimeSpec, workspace string) (*kube.Pod, error) {
	var pod kube.Pod
	err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, spec.Name, &pod)
	if err == nil && pod.Metadata.DeletionTimestamp != nil {
		if err = d.awaitGone(ctx, kube.Pods, spec.Name); err != nil {
			return nil, err
		}
		err = d.client.Get(ctx, kube.Pods, d.opts.Namespace, spec.Name, &pod)
	}
	if err == nil {
		if pod.Metadata.Labels[LabelSandbox] != spec.Name || pod.Metadata.Labels[LabelManagedBy] != ManagedBy {
			return nil, fmt.Errorf("sandbox %s: an unmanaged pod holds its name", spec.Name)
		}
		return &pod, nil
	}
	if !kube.IsNotFound(err) {
		return nil, fmt.Errorf("sandbox %s: pod: %w", spec.Name, err)
	}
	desired := PodSpec(d.opts, spec.Name, spec.SandboxID, spec.Generation, spec.Spare, workspace)
	err = d.client.Create(ctx, kube.Pods, d.opts.Namespace, desired, &pod)
	if kube.IsAlreadyExists(err) {
		err = d.client.Get(ctx, kube.Pods, d.opts.Namespace, spec.Name, &pod)
	}
	if err != nil {
		return nil, fmt.Errorf("sandbox %s: pod creation refused: %w", spec.Name, err)
	}
	return &pod, nil
}

// awaitRunning watches the pod until it runs with an address. A pod that
// ends, or whose container the kubelet cannot start (a missing image, a
// refused configuration), fails at once with the reason; a scheduling or
// volume wait lasts until ctx ends and reports the last condition seen.
func (d *Driver) awaitRunning(ctx context.Context, name, uid string) (*kube.Pod, error) {
	last := ""
	pod, err := d.awaitPod(ctx, name, func(pod *kube.Pod, event kube.EventType) (bool, error) {
		if event == kube.Deleted || pod == nil {
			return true, fmt.Errorf("sandbox %s: pod disappeared before it ran", name)
		}
		if uid != "" && pod.Metadata.UID != uid {
			return true, fmt.Errorf("sandbox %s: pod was replaced while starting", name)
		}
		if pod.Metadata.DeletionTimestamp != nil {
			return true, fmt.Errorf("sandbox %s: pod is being deleted", name)
		}
		switch pod.Status.Phase {
		case "Running":
			if pod.Status.PodIP != "" {
				return true, nil
			}
			last = "running without an address"
		case "Succeeded", "Failed":
			return true, fmt.Errorf("sandbox %s: pod ended (%s %s)", name, pod.Status.Phase, strings.TrimSpace(pod.Status.Reason+" "+pod.Status.Message))
		}
		for _, c := range pod.Status.ContainerStatuses {
			if c.State.Waiting != nil {
				switch c.State.Waiting.Reason {
				case "ErrImagePull", "ImagePullBackOff", "InvalidImageName", "CreateContainerConfigError", "CreateContainerError", "RunContainerError", "ErrImageNeverPull":
					return true, fmt.Errorf("sandbox %s: container cannot start: %s: %s", name, c.State.Waiting.Reason, c.State.Waiting.Message)
				}
				last = c.State.Waiting.Reason + " " + c.State.Waiting.Message
			}
			if c.State.Terminated != nil {
				last = fmt.Sprintf("container exited %d %s", c.State.Terminated.ExitCode, c.State.Terminated.Reason)
			}
		}
		for _, c := range pod.Status.Conditions {
			if c.Type == "PodScheduled" && c.Status != "True" && c.Message != "" {
				last = c.Reason + ": " + c.Message
			}
		}
		return false, nil
	})
	if err != nil && errors.Is(err, ctx.Err()) && last != "" {
		return nil, fmt.Errorf("sandbox %s: pod did not start (%s): %w", name, strings.TrimSpace(last), err)
	}
	return pod, err
}

// awaitPod watches one pod until done says so. The current state comes
// first (a watch without a version replays it), then changes; a watch the
// server ends is reopened, and ctx bounds the whole wait. done receives a
// nil pod with Deleted when the pod is absent at the start.
func (d *Driver) awaitPod(ctx context.Context, name string, done func(*kube.Pod, kube.EventType) (bool, error)) (*kube.Pod, error) {
	for {
		var current kube.Pod
		err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, name, &current)
		switch {
		case kube.IsNotFound(err):
			if ok, err := done(nil, kube.Deleted); ok {
				return nil, err
			}
		case err != nil:
			return nil, err
		default:
			if ok, err := done(&current, kube.Modified); ok {
				return &current, err
			}
		}
		events, err := d.client.Watch(ctx, kube.Pods, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + name, ResourceVersion: current.Metadata.ResourceVersion})
		if kube.IsGone(err) {
			events, err = d.client.Watch(ctx, kube.Pods, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + name})
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		for ev := range events {
			switch ev.Type {
			case kube.Bookmark:
				continue
			case kube.Error:
				if kube.IsGone(ev.Err) {
					continue // the loop lists again
				}
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				return nil, ev.Err
			}
			var pod kube.Pod
			if err := ev.Decode(&pod); err != nil {
				return nil, err
			}
			if ok, err := done(&pod, ev.Type); ok {
				return &pod, err
			}
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
	}
}

// awaitGone waits for a pod or claim to be deleted.
func (d *Driver) awaitGone(ctx context.Context, r kube.Resource, name string) error {
	if r == kube.Pods {
		_, err := d.awaitPod(ctx, name, func(pod *kube.Pod, event kube.EventType) (bool, error) {
			return pod == nil || event == kube.Deleted, nil
		})
		return err
	}
	for {
		var obj kube.Object
		err := d.client.Get(ctx, r, d.opts.Namespace, name, &obj)
		if kube.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		events, err := d.client.Watch(ctx, r, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + name})
		if err != nil {
			return err
		}
		for ev := range events {
			if ev.Type == kube.Deleted {
				return nil
			}
			if ev.Type == kube.Error && !kube.IsGone(ev.Err) {
				return ev.Err
			}
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
}

// guestManifest is the part of the image's manifest the driver checks
// (deploy/guest/README.md). The file is guest-writable, so it is data: the
// driver refuses an image whose manifest lacks the paths object or names a
// home or account other than the pod's, and reads nothing else from it.
type guestManifest struct {
	Paths sandbox.GuestPaths `json:"paths"`
	User  struct {
		Name string `json:"name"`
		UID  *int64 `json:"uid"`
		GID  *int64 `json:"gid"`
	} `json:"user"`
}

// checkManifest reads the manifest of the pod's image after the first
// start and refuses an image the driver cannot run (decision 7: no copy
// fallback in this kind).
func (d *Driver) checkManifest(ctx context.Context, name string) error {
	raw, err := d.run(ctx, name, nil, 4096, "cat", GuestManifestPath)
	if err != nil {
		return fmt.Errorf("sandbox %s: guest image has no manifest at %s: %w", name, GuestManifestPath, err)
	}
	var m guestManifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fmt.Errorf("sandbox %s: guest image manifest is not JSON", name)
	}
	if m.Paths.Codex == "" || m.Paths.Claude == "" || m.Paths.Trust == "" || m.Paths.Home == "" {
		return fmt.Errorf("sandbox %s: guest image manifest lacks the paths object; the Kubernetes runtime needs the base image (deploy/guest/Dockerfile.base)", name)
	}
	if m.Paths.Home != d.opts.home() {
		return fmt.Errorf("sandbox %s: guest image home %q is not the mounted workspace %q", name, m.Paths.Home, d.opts.home())
	}
	if !strings.HasPrefix(m.Paths.Trust, TrustMountPath+"/") {
		return fmt.Errorf("sandbox %s: guest image trust bundle %q is outside the trust mount %s", name, m.Paths.Trust, TrustMountPath)
	}
	if m.User.UID != nil && *m.User.UID != d.opts.guestUID() {
		return fmt.Errorf("sandbox %s: guest image user %s is uid %d, the pod runs as %d", name, m.User.Name, *m.User.UID, d.opts.guestUID())
	}
	return nil
}

// Prepare returns a no-op handle: a pod stays resident on its own.
func (d *Driver) Prepare(context.Context, string) (io.Closer, error) { return sandbox.NoResidency{}, nil }

// Address is the pod IP (decision 10).
func (d *Driver) Address(ctx context.Context, name string) (string, error) {
	d.mu.Lock()
	rt := d.runtimes[name]
	if rt != nil && rt.podIP != "" {
		ip := rt.podIP
		d.mu.Unlock()
		return ip, nil
	}
	d.mu.Unlock()
	var pod kube.Pod
	if err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, name, &pod); err != nil {
		return "", fmt.Errorf("sandbox %s: %w", name, err)
	}
	if pod.Status.PodIP == "" {
		return "", fmt.Errorf("sandbox %s: pod has no address yet", name)
	}
	return pod.Status.PodIP, nil
}

// Stop deletes the pod with the grace period and waits for it to be gone;
// the workspace claim stays (decision 8). A pod already gone is stopped.
func (d *Driver) Stop(ctx context.Context, name string) error {
	if err := validName(name); err != nil {
		return err
	}
	err := d.client.Delete(ctx, kube.Pods, d.opts.Namespace, name, kube.DeleteOptions{GracePeriodSeconds: kube.Int64(StopGraceSeconds)})
	if err != nil && !kube.IsNotFound(err) {
		return fmt.Errorf("sandbox %s: stop: %w", name, err)
	}
	if err == nil {
		if err = d.awaitGone(ctx, kube.Pods, name); err != nil {
			return fmt.Errorf("sandbox %s: stop: %w", name, err)
		}
	}
	d.mu.Lock()
	if rt := d.runtimes[name]; rt != nil {
		rt.podUID, rt.podIP, rt.publications = "", "", nil
	}
	d.mu.Unlock()
	return nil
}

// Remove stops the runtime and deletes its workspace claim.
func (d *Driver) Remove(ctx context.Context, name string) error {
	if err := d.Stop(ctx, name); err != nil {
		return err
	}
	err := d.client.Delete(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, name, kube.DeleteOptions{})
	if err != nil && !kube.IsNotFound(err) {
		return fmt.Errorf("sandbox %s: remove workspace: %w", name, err)
	}
	d.mu.Lock()
	delete(d.runtimes, name)
	d.mu.Unlock()
	return nil
}

// Publish records the guest port at the pod IP and hands the mapping to
// reserve first; nothing in the cluster changes (decision 10: the runner's
// proxy is the only client and the static NetworkPolicy admits it).
func (d *Driver) Publish(ctx context.Context, name string, guestPort int, reserve func(sandbox.PortMapping) error) (sandbox.PortMapping, error) {
	if guestPort < 1 || guestPort > 65535 {
		return sandbox.PortMapping{}, errors.New("guest port must be 1–65535")
	}
	d.mu.Lock()
	rt := d.runtimes[name]
	if rt == nil || rt.podIP == "" {
		d.mu.Unlock()
		return sandbox.PortMapping{}, fmt.Errorf("sandbox %s is not running", name)
	}
	m := sandbox.PortMapping{Address: rt.podIP, Port: guestPort, GuestPort: guestPort}
	d.mu.Unlock()
	if reserve != nil {
		if err := reserve(m); err != nil {
			return sandbox.PortMapping{}, err
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	kept := rt.publications[:0]
	for _, p := range rt.publications {
		if p.GuestPort != guestPort {
			kept = append(kept, p)
		}
	}
	rt.publications = append(kept, m)
	return m, nil
}

// Unpublish forgets a mapping.
func (d *Driver) Unpublish(_ context.Context, name string, m sandbox.PortMapping) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	rt := d.runtimes[name]
	if rt == nil {
		return nil
	}
	kept := rt.publications[:0]
	for _, p := range rt.publications {
		if p != m {
			kept = append(kept, p)
		}
	}
	rt.publications = kept
	return nil
}

// Mappings returns the recorded publications once the pod is confirmed to
// be the one they were made on (its UID); a replaced or missing pod is an
// error, since its publications would name a different guest.
func (d *Driver) Mappings(ctx context.Context, name string) ([]sandbox.PortMapping, error) {
	d.mu.Lock()
	rt := d.runtimes[name]
	if rt == nil || rt.podUID == "" {
		d.mu.Unlock()
		return nil, fmt.Errorf("sandbox %s is not running", name)
	}
	uid := rt.podUID
	d.mu.Unlock()
	var pod kube.Pod
	if err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, name, &pod); err != nil {
		if kube.IsNotFound(err) {
			return nil, fmt.Errorf("sandbox %s: pod is gone", name)
		}
		return nil, fmt.Errorf("sandbox %s: %w", name, err)
	}
	if pod.Metadata.UID != uid {
		return nil, fmt.Errorf("sandbox %s: pod was replaced (%s, expected %s)", name, pod.Metadata.UID, uid)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]sandbox.PortMapping, 0, len(rt.publications))
	return append(out, rt.publications...), nil
}

// Reconcile is the startup pass: no sandbox pod is legitimately running
// when the worker starts (it has just stopped every registered sandbox and
// removed every registered spare), so every pod under this driver's labels
// is deleted; a workspace claim not registered is deleted when it was a
// spare's and kept, with a log line, otherwise.
func (d *Driver) Reconcile(ctx context.Context, registered []string) error {
	keep := make(map[string]bool, len(registered))
	for _, name := range registered {
		keep[name] = true
	}
	var pods kube.List[kube.Pod]
	if err := d.client.List(ctx, kube.Pods, d.opts.Namespace, kube.ListOptions{LabelSelector: selector()}, &pods); err != nil {
		return fmt.Errorf("list sandbox pods: %w", err)
	}
	for _, pod := range pods.Items {
		if pod.Metadata.DeletionTimestamp != nil {
			continue
		}
		err := d.client.Delete(ctx, kube.Pods, d.opts.Namespace, pod.Metadata.Name, kube.DeleteOptions{GracePeriodSeconds: kube.Int64(StopGraceSeconds), UID: pod.Metadata.UID})
		if err != nil && !kube.IsNotFound(err) && !kube.IsConflict(err) {
			return fmt.Errorf("delete stale sandbox pod %s: %w", pod.Metadata.Name, err)
		}
		log.Printf("sandbox %s: stale pod %s deleted at startup (registered=%v)", pod.Metadata.Name, pod.Metadata.UID, keep[pod.Metadata.Name])
	}
	var claims kube.List[kube.PersistentVolumeClaim]
	if err := d.client.List(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, kube.ListOptions{LabelSelector: selector()}, &claims); err != nil {
		return fmt.Errorf("list workspace claims: %w", err)
	}
	for _, claim := range claims.Items {
		name := claim.Metadata.Name
		if keep[name] || claim.Metadata.DeletionTimestamp != nil {
			continue
		}
		if claim.Metadata.Labels[LabelSpare] != "true" {
			log.Printf("workspace claim %s (%s) is not registered with this runner; kept", name, claim.Metadata.UID)
			continue
		}
		err := d.client.Delete(ctx, kube.PersistentVolumeClaims, d.opts.Namespace, name, kube.DeleteOptions{UID: claim.Metadata.UID})
		if err != nil && !kube.IsNotFound(err) && !kube.IsConflict(err) {
			return fmt.Errorf("delete stale spare claim %s: %w", name, err)
		}
		log.Printf("spare workspace claim %s deleted at startup", name)
	}
	return nil
}

// awaitTrust blocks the first creation until the trust ConfigMap holds a
// non-empty bundle (decision 9's readiness rule). A runner whose
// ServiceAccount may not read the ConfigMap proceeds after one log line:
// the policy service publishes the bundle before it serves the control
// listener the worker consulted before creating, so the gate is defence in
// depth there, and the pod's mount fails closed on an empty bundle.
func (d *Driver) awaitTrust(ctx context.Context) error {
	d.mu.Lock()
	ready := d.trustReady
	d.mu.Unlock()
	if ready {
		return nil
	}
	mark := func() {
		d.mu.Lock()
		d.trustReady = true
		d.mu.Unlock()
	}
	logged := false
	for {
		var cm kube.ConfigMap
		err := d.client.Get(ctx, kube.ConfigMaps, d.opts.Namespace, d.opts.TrustConfigMap, &cm)
		switch {
		case err == nil && trustPublished(cm):
			mark()
			return nil
		case kube.IsForbidden(err):
			log.Printf("trust bundle %s/%s is not readable by this runner (%v); relying on the policy service publishing it before it serves", d.opts.Namespace, d.opts.TrustConfigMap, err)
			mark()
			return nil
		case err != nil && !kube.IsNotFound(err):
			if ctx.Err() != nil {
				return fmt.Errorf("waiting for the guest trust bundle: %w", ctx.Err())
			}
			return fmt.Errorf("waiting for the guest trust bundle: %w", err)
		}
		if !logged {
			log.Printf("waiting for the guest trust bundle: %s/%s has no %s yet", d.opts.Namespace, d.opts.TrustConfigMap, TrustBundleKey)
			logged = true
		}
		events, err := d.client.Watch(ctx, kube.ConfigMaps, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + d.opts.TrustConfigMap})
		if err != nil {
			if ctx.Err() != nil {
				return fmt.Errorf("waiting for the guest trust bundle: %w", ctx.Err())
			}
			return fmt.Errorf("waiting for the guest trust bundle: %w", err)
		}
		for ev := range events {
			if ev.Type != kube.Added && ev.Type != kube.Modified {
				continue
			}
			var current kube.ConfigMap
			if ev.Decode(&current) == nil && trustPublished(current) {
				mark()
				return nil
			}
		}
		if ctx.Err() != nil {
			return fmt.Errorf("waiting for the guest trust bundle: %w", ctx.Err())
		}
	}
}

// trustPublished reports a bundle under TrustBundleKey with content.
func trustPublished(cm kube.ConfigMap) bool {
	return strings.TrimSpace(cm.Data[TrustBundleKey]) != "" || len(cm.BinaryData[TrustBundleKey]) > 0
}

func isStatus(err error) bool {
	var e *kube.StatusError
	return errors.As(err, &e)
}
