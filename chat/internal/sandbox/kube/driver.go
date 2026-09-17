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
	// cache is what Cluster and Pod answered last (cluster.go).
	cacheMu sync.Mutex
	cache   clusterCache
	// exec retries and waits are shortened by tests.
	execRetry time.Duration
	// resizeWait bounds how long a resize waits for the kubelet to apply
	// it before it counts as infeasible.
	resizeWait time.Duration
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
	return &Driver{client: client, opts: opts, runtimes: map[string]*runtime{}, execRetry: 500 * time.Millisecond, resizeWait: resizeWait}, nil
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
	if spec.Source != "" {
		if err := validName(spec.Source); err != nil {
			return fmt.Errorf("fork source: %w", err)
		}
	}
	if err := d.ensureRunning(ctx, spec); err != nil {
		return err
	}
	if spec.Source == "" {
		return nil
	}
	// A clone the storage did not perform (a provisioner that ignores
	// dataSource yields an empty volume) or a copy interrupted earlier
	// leaves the workspace directory missing: copy from the source pod.
	if _, err := d.run(ctx, spec.Name, nil, 0, "test", "-d", spec.Directory); err == nil {
		return nil
	}
	sandbox.Report(ctx, "copying the workspace from "+spec.Source)
	if err := d.copyHome(ctx, spec.Source, spec.Name); err != nil {
		return err
	}
	d.mu.Lock()
	if rt := d.runtimes[spec.Name]; rt != nil {
		rt.workspace = WorkspaceCopy
	}
	d.mu.Unlock()
	log.Printf("sandbox %s: workspace copied from %s", spec.Name, spec.Source)
	return nil
}

// Prepare makes a created runtime resident again: after a stop its claim
// is there and its pod is not, so the pod of this generation is created
// (decision 3); a running pod is confirmed. The handle is a no-op, since
// a pod stays up on its own.
func (d *Driver) Prepare(ctx context.Context, spec sandbox.RuntimeSpec) (io.Closer, error) {
	spec.Source = "" // the workspace was cloned or copied at creation
	if err := d.ensureRunning(ctx, spec); err != nil {
		return nil, err
	}
	return sandbox.NoResidency{}, nil
}

// ensureRunning is the claim, the pod, the wait and the manifest check.
func (d *Driver) ensureRunning(ctx context.Context, spec sandbox.RuntimeSpec) error {
	if err := validName(spec.Name); err != nil {
		return err
	}
	d.mu.Lock()
	trusted := d.trustReady
	d.mu.Unlock()
	if !trusted {
		sandbox.Report(ctx, "waiting for the guest trust bundle")
	}
	if err := d.awaitTrust(ctx); err != nil {
		return err
	}
	rt := d.record(spec.Name)
	if spec.Source != "" {
		sandbox.Report(ctx, "cloning the workspace volume from "+spec.Source)
	} else {
		sandbox.Report(ctx, "preparing the workspace volume")
	}
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
	sandbox.Report(ctx, "creating the sandbox pod")
	pod, err := d.ensurePod(ctx, spec, workspace)
	if err != nil {
		return err
	}
	pod, err = d.awaitRunning(ctx, spec.Name, pod.Metadata.UID)
	if err != nil {
		return err
	}
	d.mu.Lock()
	known := rt.podUID == pod.Metadata.UID
	d.mu.Unlock()
	if !known {
		sandbox.Report(ctx, "checking the guest image manifest")
		if err = d.checkManifest(ctx, spec.Name); err != nil {
			return err
		}
	}
	d.mu.Lock()
	if !known {
		rt.publications = nil
		rt.workspace = workspace
	}
	rt.podUID, rt.podIP = pod.Metadata.UID, pod.Status.PodIP
	if spec.Generation != "" {
		rt.generation = spec.Generation
	}
	d.mu.Unlock()
	if !known {
		log.Printf("sandbox %s: pod %s running at %s (claim %s, workspace %s)", spec.Name, pod.Metadata.UID, pod.Status.PodIP, claim.Metadata.UID, workspace)
	}
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
	desired := PodSpec(d.opts, spec, workspace)
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
		sandbox.Report(ctx, StartupDetail(pod))
		return false, nil
	})
	if err != nil && errors.Is(err, ctx.Err()) && last != "" {
		return nil, fmt.Errorf("sandbox %s: pod did not start (%s): %w", name, strings.TrimSpace(last), err)
	}
	return pod, err
}

// StartupDetail says, in the owner's words, what a pod that is not yet
// running with an address is waiting for: the scheduler (with its reason,
// which names a node being provisioned or a resource shortfall), the
// image pull, the container runtime (a Kata VM booting shows here), or
// an address. It is the chat's startup detail and the cluster page's
// reason column.
func StartupDetail(pod *kube.Pod) string {
	if pod == nil {
		return ""
	}
	if pod.Metadata.DeletionTimestamp != nil {
		return "the previous pod is still terminating"
	}
	scheduled := false
	for _, c := range pod.Status.Conditions {
		if c.Type == "PodScheduled" {
			scheduled = c.Status == "True"
			if !scheduled {
				// The scheduler's first sentence says what is missing; the
				// rest is its preemption reasoning.
				if message, _, _ := strings.Cut(strings.TrimSpace(c.Message), ". "); message != "" {
					return "waiting for a node: " + strings.TrimSuffix(message, ".")
				}
				return "waiting for a node"
			}
		}
	}
	for _, c := range pod.Status.ContainerStatuses {
		if w := c.State.Waiting; w != nil {
			switch w.Reason {
			case "ContainerCreating":
				return "starting the container"
			case "PodInitializing":
				return "initialising the pod"
			case "":
				return "waiting for the container"
			}
			if strings.Contains(w.Reason, "Image") || strings.Contains(w.Reason, "Pull") {
				if w.Message != "" {
					return "waiting for the container image: " + strings.TrimSpace(w.Message)
				}
				return "pulling the container image"
			}
			if w.Message != "" {
				return strings.TrimSpace(w.Reason + ": " + w.Message)
			}
			return w.Reason
		}
		if t := c.State.Terminated; t != nil {
			return fmt.Sprintf("container exited (%d %s)", t.ExitCode, strings.TrimSpace(t.Reason))
		}
	}
	switch pod.Status.Phase {
	case "Running":
		if pod.Status.PodIP == "" {
			return "waiting for the pod's address"
		}
		return ""
	case "Pending":
		if !scheduled {
			return "waiting for a node"
		}
		if len(pod.Status.ContainerStatuses) == 0 {
			return "waiting for the kubelet to start the container"
		}
		return "waiting for the container"
	}
	return strings.TrimSpace(pod.Status.Phase + " " + pod.Status.Reason)
}

// awaitPod watches one pod until done says so. The current state comes
// first (a watch without a version replays it), then changes; a watch the
// server ends is reopened, and ctx bounds the whole wait. done receives a
// nil pod with Deleted when the pod is absent at the start.
func (d *Driver) awaitPod(ctx context.Context, name string, done func(*kube.Pod, kube.EventType) (bool, error)) (*kube.Pod, error) {
	for {
		pod, finished, err := d.watchPod(ctx, name, done)
		if finished || err != nil {
			return pod, err
		}
	}
}

// watchPod is one round of awaitPod: a Get, then a watch that is cancelled
// (its connection closed) when this returns. finished false with a nil
// error means the server ended the watch and the caller should try again.
func (d *Driver) watchPod(ctx context.Context, name string, done func(*kube.Pod, kube.EventType) (bool, error)) (*kube.Pod, bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	{
		var current kube.Pod
		err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, name, &current)
		switch {
		case kube.IsNotFound(err):
			if ok, err := done(nil, kube.Deleted); ok {
				return nil, true, err
			}
		case err != nil:
			return nil, true, err
		default:
			if ok, err := done(&current, kube.Modified); ok {
				return &current, true, err
			}
		}
		events, err := d.client.Watch(ctx, kube.Pods, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + name, ResourceVersion: current.Metadata.ResourceVersion})
		if kube.IsGone(err) {
			events, err = d.client.Watch(ctx, kube.Pods, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + name})
		}
		if err != nil {
			if ctx.Err() != nil {
				return nil, true, ctx.Err()
			}
			return nil, true, err
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
					return nil, true, ctx.Err()
				}
				if !isStatus(ev.Err) {
					// The stream itself broke (the API server's front end
					// resets long watches; a pod on Autopilot can wait
					// minutes for its node): list and watch again.
					log.Printf("sandbox %s: pod watch interrupted (%v); watching again", name, ev.Err)
					sleep(ctx, watchRetry)
					return nil, false, nil
				}
				return nil, true, ev.Err
			}
			var pod kube.Pod
			if err := ev.Decode(&pod); err != nil {
				return nil, true, err
			}
			if ok, err := done(&pod, ev.Type); ok {
				return &pod, true, err
			}
		}
		if ctx.Err() != nil {
			return nil, true, ctx.Err()
		}
		return nil, false, nil
	}
}

// watchRetry is the pause before a broken pod watch is reopened.
const watchRetry = time.Second

// awaitGone waits for a pod or claim to be deleted.
func (d *Driver) awaitGone(ctx context.Context, r kube.Resource, name string) error {
	if r == kube.Pods {
		_, err := d.awaitPod(ctx, name, func(pod *kube.Pod, event kube.EventType) (bool, error) {
			return pod == nil || event == kube.Deleted, nil
		})
		return err
	}
	for {
		gone, err := d.watchGone(ctx, r, name)
		if gone || err != nil {
			return err
		}
	}
}

// watchGone is one round of awaitGone for a claim; the watch is cancelled
// when it returns.
func (d *Driver) watchGone(ctx context.Context, r kube.Resource, name string) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var obj kube.Object
	err := d.client.Get(ctx, r, d.opts.Namespace, name, &obj)
	if kube.IsNotFound(err) {
		return true, nil
	}
	if err != nil {
		return true, err
	}
	events, err := d.client.Watch(ctx, r, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + name, ResourceVersion: obj.Meta().ResourceVersion})
	if err != nil {
		return true, err
	}
	for ev := range events {
		if ev.Type == kube.Deleted {
			return true, nil
		}
		if ev.Type == kube.Error && !kube.IsGone(ev.Err) {
			return true, ev.Err
		}
	}
	if ctx.Err() != nil {
		return true, ctx.Err()
	}
	return false, nil
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

// Resize gives the running pod a new size in place through pods/resize
// (Kubernetes 1.33+) and waits until the kubelet reports the container
// running at it; the pod is never replaced here, so restarted is always
// false. A stopped runtime has no pod: the size takes effect through the
// spec of its next Prepare, and this returns at once. A resize the
// cluster refuses or cannot apply (an infeasible or deferred one, a
// cluster without the subresource) is sandbox.ErrResizeInfeasible, and
// the worker replaces the pod when nothing runs on it.
func (d *Driver) Resize(ctx context.Context, name string, r sandbox.Resources) (bool, error) {
	if err := validName(name); err != nil {
		return false, err
	}
	if r.CPUMilli <= 0 || r.MemoryMB <= 0 {
		return false, errors.New("resize needs a whole size")
	}
	var pod kube.Pod
	err := d.client.Get(ctx, kube.Pods, d.opts.Namespace, name, &pod)
	if kube.IsNotFound(err) || (err == nil && pod.Metadata.DeletionTimestamp != nil) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("sandbox %s: resize: %w", name, err)
	}
	if pod.Metadata.Labels[LabelSandbox] != name || pod.Metadata.Labels[LabelManagedBy] != ManagedBy {
		return false, fmt.Errorf("sandbox %s: an unmanaged pod holds its name", name)
	}
	if runningAt(&pod, r) {
		return false, nil
	}
	previous := sandbox.Resources{}
	for _, c := range pod.Spec.Containers {
		if c.Name == ContainerName {
			cpu, _ := kube.Milli(c.Resources.Limits["cpu"])
			memory, _ := kube.Bytes(c.Resources.Limits["memory"])
			previous = sandbox.Resources{CPUMilli: int(cpu), MemoryMB: int(memory >> 20)}
		}
	}
	if err = d.client.StrategicPatch(ctx, kube.PodsResize, d.opts.Namespace, name, ResizePatch(r), &pod); err != nil {
		if isStatus(err) {
			return false, fmt.Errorf("%w: %v", sandbox.ErrResizeInfeasible, err)
		}
		return false, fmt.Errorf("sandbox %s: resize: %w", name, err)
	}
	// A resize the kubelet defers (no room on the node now) is not going
	// to be waited for: a managed cluster does not grow a node for it, and
	// the worker holds its registry meanwhile. The bound is the driver's,
	// under the caller's context.
	waitCtx, cancel := context.WithTimeout(ctx, d.resizeWait)
	defer cancel()
	uid := pod.Metadata.UID
	// The conditions an earlier attempt left (Infeasible, the runtime's
	// error) are still on the pod when the watch starts; only a status
	// the kubelet writes after this patch says anything about it.
	first := true
	_, err = d.awaitPod(waitCtx, name, func(pod *kube.Pod, event kube.EventType) (bool, error) {
		if event == kube.Deleted || pod == nil || pod.Metadata.UID != uid || pod.Metadata.DeletionTimestamp != nil {
			return true, fmt.Errorf("sandbox %s: pod went away during the resize", name)
		}
		if runningAt(pod, r) {
			return true, nil
		}
		stale := first
		first = false
		if stale {
			return false, nil
		}
		return false, resizeFailed(pod)
	})
	if err != nil && errors.Is(err, waitCtx.Err()) {
		err = fmt.Errorf("%w: not applied within %s (deferred by the kubelet)", sandbox.ErrResizeInfeasible, d.resizeWait)
	}
	if errors.Is(err, sandbox.ErrResizeInfeasible) && previous.CPUMilli > 0 && previous.MemoryMB > 0 {
		// Put the spec back so a pod that stays (under a run) does not
		// carry a size it never got; best effort, the caller's answer is
		// the same either way.
		revertCtx, done := context.WithTimeout(context.Background(), 10*time.Second)
		if revertErr := d.client.StrategicPatch(revertCtx, kube.PodsResize, d.opts.Namespace, name, ResizePatch(previous), nil); revertErr != nil {
			log.Printf("sandbox %s: resize to %s not applied and not reverted to %s: %v", name, r, previous, revertErr)
		}
		done()
	}
	if err != nil {
		return false, err
	}
	log.Printf("sandbox %s: pod %s resized to %s", name, uid, r)
	return false, nil
}

// resizeWait is how long a resize may stay deferred or in progress before
// the driver gives up on the running pod: a kubelet that has the room
// applies one within seconds.
const resizeWait = 15 * time.Second

// runningAt reports whether the kubelet runs the guest container at r
// (status.containerStatuses[].resources, 1.33+), whatever the resize
// conditions say.
func runningAt(pod *kube.Pod, r sandbox.Resources) bool {
	for _, c := range pod.Status.ContainerStatuses {
		if c.Name != ContainerName || c.Resources == nil {
			continue
		}
		cpu, errCPU := kube.Milli(c.Resources.Limits["cpu"])
		memory, errMemory := kube.Bytes(c.Resources.Limits["memory"])
		if errCPU == nil && errMemory == nil && cpu == int64(r.CPUMilli) && memory == int64(r.MemoryMB)<<20 {
			return true
		}
	}
	return false
}

// resizeFailed is the resize condition that will never clear on its own:
// infeasible (the node cannot ever fit it) or the runtime's error (GKE
// Sandbox's gVisor: not implemented). Pending (deferred) and in progress
// are nil: still worth waiting for.
func resizeFailed(pod *kube.Pod) error {
	for _, c := range pod.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch {
		case c.Type == "PodResizePending" && c.Reason == "Infeasible":
			return fmt.Errorf("%w: %s", sandbox.ErrResizeInfeasible, strings.TrimSpace(c.Reason+" "+c.Message))
		case c.Type == "PodResizeInProgress" && c.Reason == "Error":
			return fmt.Errorf("%w: %s", sandbox.ErrResizeInfeasible, strings.TrimSpace(c.Message))
		}
	}
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
		published, err := d.watchTrust(ctx)
		if err != nil {
			return err
		}
		if published {
			mark()
			return nil
		}
	}
}

// watchTrust watches the trust ConfigMap until it holds a bundle; the
// watch is cancelled when it returns.
func (d *Driver) watchTrust(ctx context.Context) (bool, error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	events, err := d.client.Watch(ctx, kube.ConfigMaps, d.opts.Namespace, kube.WatchOptions{FieldSelector: "metadata.name=" + d.opts.TrustConfigMap})
	if err != nil {
		if ctx.Err() != nil {
			return false, fmt.Errorf("waiting for the guest trust bundle: %w", ctx.Err())
		}
		return false, fmt.Errorf("waiting for the guest trust bundle: %w", err)
	}
	for ev := range events {
		if ev.Type != kube.Added && ev.Type != kube.Modified {
			continue
		}
		var current kube.ConfigMap
		if ev.Decode(&current) == nil && trustPublished(current) {
			return true, nil
		}
	}
	if ctx.Err() != nil {
		return false, fmt.Errorf("waiting for the guest trust bundle: %w", ctx.Err())
	}
	return false, nil
}

// trustPublished reports a bundle under TrustBundleKey with content.
func trustPublished(cm kube.ConfigMap) bool {
	return strings.TrimSpace(cm.Data[TrustBundleKey]) != "" || len(cm.BinaryData[TrustBundleKey]) > 0
}

func isStatus(err error) bool {
	var e *kube.StatusError
	return errors.As(err, &e)
}
