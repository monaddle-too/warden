// Package kube is the Kubernetes runtime driver (docs/warden-kubernetes-plan.md,
// work item 4): a sandbox is a workspace PersistentVolumeClaim (its stable
// identity, decision 3) and a pod per generation on it, created from the
// pinned guest base image under the configured RuntimeClass (decision 2),
// with the trust bundle mounted (decision 9) and previews reached at the
// pod IP (decision 10). Guest operations run over pods/exec; nothing in
// the cluster is patched by this driver, so the labels it sets are set at
// creation and the policy service, which may patch, owns the rest.
package kube

import (
	"encoding/json"
	"fmt"
	"strconv"

	"warden/chat/internal/config"
	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

// Labels and annotations on sandbox pods and workspace claims. The sandbox
// label is what the sandbox namespace's admission policy requires and the
// policy service selects on; the binding label (the binding digest) and
// the egress label are the policy service's to set, since it holds patch.
const (
	LabelSandbox   = "warden.monaddle.com/sandbox"
	LabelSpare     = "warden.monaddle.com/spare"
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy marks the objects this driver created; the policy
	// service's canary pods (decision 12) share the namespace and the
	// sandbox label, not this value.
	ManagedBy = "warden-runner"

	AnnotationSandboxID  = "warden.monaddle.com/sandbox-id"
	AnnotationGeneration = "warden.monaddle.com/generation"
	AnnotationWorkspace  = "warden.monaddle.com/workspace" // "fresh", "clone" or "copy"

	// ContainerName is the one container of a sandbox pod.
	ContainerName = "guest"
	// TrustMountPath is where the guest image expects its trust bundle
	// directory (deploy/guest/README.md); TrustBundleKey is the ConfigMap
	// key, the file name the image's system bundle symlink points at.
	TrustMountPath = "/opt/warden/trust"
	TrustBundleKey = "ca-certificates.crt"
	// GuestManifestPath is the image's manifest, read after the first
	// container starts.
	GuestManifestPath = "/opt/warden/guest-manifest.json"

	// StopGraceSeconds is the pod's termination grace period: tini forwards
	// the signal and sleep exits, so a stop normally takes well under it.
	StopGraceSeconds = 10
)

// Options configure the driver from the kubernetes section of warden.json
// and the runner's sandbox settings.
type Options struct {
	Namespace        string
	Tier             string // config.TierGVisor or config.TierKata
	RuntimeClass     string
	GuestImage       string
	GuestImageDigest string
	// StorageClass is the workspace claims' class; empty is the cluster
	// default.
	StorageClass    string
	WorkspaceSizeGi int
	TrustConfigMap  string
	// MemoryMB and CPUMillis are the container's memory and CPU request
	// and limit for a spec that names no size (the runner's default, as
	// the namespace's LimitRange expects, decision 11); 1536 MiB and one
	// core when unset. A spec's Resources take precedence.
	MemoryMB     int
	CPUMillis    int
	NodeSelector map[string]string
	Tolerations  []config.Toleration
	// GuestUID and GuestGID are the account the container runs as and the
	// workspace's group, the base image's agent user (1000/1000 when zero);
	// Home is the mount point of the workspace claim (/home/agent when
	// empty). Both are checked against the image's manifest after the
	// first start, since the manifest lives inside the image.
	GuestUID, GuestGID int64
	Home               string
	// ImagePullPolicy is the container's pull policy; empty leaves the
	// kubelet's default (IfNotPresent for a digest reference).
	ImagePullPolicy string
}

func (o Options) guestUID() int64 {
	if o.GuestUID > 0 {
		return o.GuestUID
	}
	return 1000
}

func (o Options) guestGID() int64 {
	if o.GuestGID > 0 {
		return o.GuestGID
	}
	return 1000
}

func (o Options) home() string {
	if o.Home != "" {
		return o.Home
	}
	return "/home/agent"
}

func (o Options) memoryMB() int {
	if o.MemoryMB > 0 {
		return o.MemoryMB
	}
	return 1536
}

func (o Options) cpuMillis() int {
	if o.CPUMillis > 0 {
		return o.CPUMillis
	}
	return 1000
}

// resources is the container's request and limit for a size: the spec's,
// each field falling back to the options' default.
func (o Options) resources(r sandbox.Resources) kube.ResourceList {
	r = r.Fill(sandbox.Resources{CPUMilli: o.cpuMillis(), MemoryMB: o.memoryMB()})
	return kube.ResourceList{"cpu": strconv.Itoa(r.CPUMilli) + "m", "memory": strconv.Itoa(r.MemoryMB) + "Mi"}
}

// ResizePatch is the strategic merge patch to pods/resize that gives the
// guest container a new request and limit: only the container's name and
// resources, which is all that subresource accepts.
func ResizePatch(r sandbox.Resources) []byte {
	resources := kube.ResourceList{"cpu": strconv.Itoa(r.CPUMilli) + "m", "memory": strconv.Itoa(r.MemoryMB) + "Mi"}
	patch, _ := json.Marshal(map[string]any{"spec": map[string]any{"containers": []map[string]any{{
		"name":      ContainerName,
		"resources": kube.ResourceRequirements{Limits: resources, Requests: copyLabels(resources)},
	}}}})
	return patch
}

func (o Options) workspaceSizeGi() int {
	if o.WorkspaceSizeGi > 0 {
		return o.WorkspaceSizeGi
	}
	return 20
}

// Image is the pinned guest image reference, repository@digest.
func (o Options) Image() string { return o.GuestImage + "@" + o.GuestImageDigest }

// DroppedCapabilities are removed from the container's bounding set. The
// rest of the runtime's default set stays because sudo in the guest is a
// product feature (decision 11): a setuid binary gains only what the
// bounding set allows, so dropping ALL leaves sudo "unable to change to
// root gid" (checked on the dev cluster), and root in the guest needs
// CHOWN, DAC_OVERRIDE and FOWNER for the worker's chown/chmod/install
// steps. Nothing is added, which is what Pod Security baseline and the
// verifier require. NET_RAW goes so a guest cannot craft frames.
var DroppedCapabilities = []string{"AUDIT_WRITE", "FSETID", "MKNOD", "NET_RAW", "SETFCAP", "SETPCAP", "SYS_CHROOT"}

// PodSpec is the sandbox pod for a runtime, a pure function of the options
// and the spec: the pinned image under the RuntimeClass, the spec's size
// (else the options' default) as both request and limit, no service
// account token,
// the workspace claim at the home, the trust ConfigMap read-only, an
// emptyDir /tmp, the guest account with DroppedCapabilities removed and
// the runtime's default seccomp profile, and args (not command) so the
// image's tini + guest-init entrypoint stays. allowPrivilegeEscalation is
// left unset on purpose: sudo in the guest is a product feature (spike
// result under decision 2). The labels are the sandbox name, this driver's
// mark and the spare flag; the annotations record what the worker knows
// about the sandbox at creation. The pod's name is the runtime name.
func PodSpec(o Options, spec sandbox.RuntimeSpec, workspace string) kube.Pod {
	name, sandboxID, generation, spare := spec.Name, spec.SandboxID, spec.Generation, spec.Spare
	labels := map[string]string{LabelSandbox: name, LabelManagedBy: ManagedBy}
	if spare {
		labels[LabelSpare] = "true"
	}
	annotations := map[string]string{}
	if sandboxID != "" {
		annotations[AnnotationSandboxID] = sandboxID
	}
	if generation != "" {
		annotations[AnnotationGeneration] = generation
	}
	if workspace != "" {
		annotations[AnnotationWorkspace] = workspace
	}
	if len(annotations) == 0 {
		annotations = nil
	}
	uid, gid := o.guestUID(), o.guestGID()
	resources := o.resources(spec.Resources)
	var tolerations []kube.Toleration
	for _, t := range o.Tolerations {
		tolerations = append(tolerations, kube.Toleration{Key: t.Key, Operator: t.Operator, Value: t.Value, Effect: t.Effect, TolerationSeconds: t.TolerationSeconds})
	}
	return kube.Pod{
		Metadata: kube.ObjectMeta{Name: name, Namespace: o.Namespace, Labels: labels, Annotations: annotations},
		Spec: kube.PodSpec{
			RuntimeClassName:              kube.String(o.RuntimeClass),
			AutomountServiceAccountToken:  kube.Bool(false),
			RestartPolicy:                 "Always",
			TerminationGracePeriodSeconds: kube.Int64(StopGraceSeconds),
			EnableServiceLinks:            kube.Bool(false),
			NodeSelector:                  copyLabels(o.NodeSelector),
			Tolerations:                   tolerations,
			SecurityContext: &kube.PodSecurityContext{
				RunAsUser:      kube.Int64(uid),
				RunAsGroup:     kube.Int64(gid),
				RunAsNonRoot:   kube.Bool(true),
				FSGroup:        kube.Int64(gid),
				SeccompProfile: &kube.SeccompProfile{Type: "RuntimeDefault"},
			},
			Containers: []kube.Container{{
				Name:            ContainerName,
				Image:           o.Image(),
				ImagePullPolicy: o.ImagePullPolicy,
				Args:            []string{"sleep", "infinity"},
				Resources:       kube.ResourceRequirements{Limits: resources, Requests: copyLabels(resources)},
				SecurityContext: &kube.SecurityContext{
					Capabilities: &kube.Capabilities{Drop: append([]string(nil), DroppedCapabilities...)},
				},
				VolumeMounts: []kube.VolumeMount{
					{Name: "home", MountPath: o.home()},
					{Name: "trust", MountPath: TrustMountPath, ReadOnly: true},
					{Name: "tmp", MountPath: "/tmp"},
				},
			}},
			Volumes: []kube.Volume{
				{Name: "home", PersistentVolumeClaim: &kube.PersistentVolumeClaimVolumeSource{ClaimName: name}},
				{Name: "trust", ConfigMap: &kube.ConfigMapVolumeSource{Name: o.TrustConfigMap}},
				{Name: "tmp", EmptyDir: &kube.EmptyDirVolumeSource{}},
			},
		},
	}
}

// ClaimSpec is the workspace claim of a runtime: ReadWriteOnce, the
// configured class and size, labelled like its pod, and for a fork a
// dataSource naming the source runtime's claim (a CSI clone, decision 8).
// Its name is the runtime name.
func ClaimSpec(o Options, name, sandboxID, source string, spare bool) kube.PersistentVolumeClaim {
	labels := map[string]string{LabelSandbox: name, LabelManagedBy: ManagedBy}
	if spare {
		labels[LabelSpare] = "true"
	}
	var annotations map[string]string
	if sandboxID != "" {
		annotations = map[string]string{AnnotationSandboxID: sandboxID}
	}
	spec := kube.PersistentVolumeClaimSpec{
		AccessModes: []string{"ReadWriteOnce"},
		Resources:   kube.VolumeResourceRequirements{Requests: kube.ResourceList{"storage": strconv.Itoa(o.workspaceSizeGi()) + "Gi"}},
	}
	if o.StorageClass != "" {
		spec.StorageClassName = kube.String(o.StorageClass)
	}
	if source != "" {
		spec.DataSource = &kube.TypedLocalObjectReference{Kind: "PersistentVolumeClaim", Name: source}
	}
	return kube.PersistentVolumeClaim{Metadata: kube.ObjectMeta{Name: name, Namespace: o.Namespace, Labels: labels, Annotations: annotations}, Spec: spec}
}

func copyLabels(m map[string]string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// selector is the label selector of the objects this driver owns.
func selector() string { return LabelSandbox + "," + LabelManagedBy + "=" + ManagedBy }

// validName accepts the runtime names the worker derives (wc-<hex> and
// wc-spare-<hex>): a DNS label, since it names a pod and a claim.
func validName(name string) error {
	if len(name) < 4 || len(name) > 63 || name[0] == '-' || name[len(name)-1] == '-' {
		return fmt.Errorf("invalid runtime name %q", name)
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '-') {
			return fmt.Errorf("invalid runtime name %q", name)
		}
	}
	return nil
}
