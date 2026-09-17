package kube

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// TypeMeta is the apiVersion/kind pair every object carries. Create and
// Apply fill it from the Resource when the caller leaves it empty.
type TypeMeta struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// ObjectMeta holds the metadata fields Warden reads and writes.
type ObjectMeta struct {
	Name              string            `json:"name,omitempty"`
	Namespace         string            `json:"namespace,omitempty"`
	UID               string            `json:"uid,omitempty"`
	ResourceVersion   string            `json:"resourceVersion,omitempty"`
	Labels            map[string]string `json:"labels,omitempty"`
	Annotations       map[string]string `json:"annotations,omitempty"`
	CreationTimestamp *time.Time        `json:"creationTimestamp,omitempty"`
	DeletionTimestamp *time.Time        `json:"deletionTimestamp,omitempty"`
}

// ListMeta is the metadata of a list response.
type ListMeta struct {
	ResourceVersion string `json:"resourceVersion,omitempty"`
	Continue        string `json:"continue,omitempty"`
}

// List is the list form of any typed resource, for example List[Pod].
type List[T any] struct {
	TypeMeta
	Metadata ListMeta `json:"metadata"`
	Items    []T      `json:"items"`
}

// Object is the untyped form of a resource, for kinds that have no typed
// struct here. It marshals as the object itself.
type Object map[string]any

// Meta decodes the object's metadata.
func (o Object) Meta() ObjectMeta {
	var meta ObjectMeta
	if raw, err := json.Marshal(o["metadata"]); err == nil {
		_ = json.Unmarshal(raw, &meta)
	}
	return meta
}

// Kind returns the object's kind.
func (o Object) Kind() string { s, _ := o["kind"].(string); return s }

// Status is the API server's error and result envelope.
type Status struct {
	TypeMeta
	Metadata ListMeta       `json:"metadata,omitempty"`
	Status   string         `json:"status,omitempty"` // Success or Failure
	Message  string         `json:"message,omitempty"`
	Reason   string         `json:"reason,omitempty"`
	Details  *StatusDetails `json:"details,omitempty"`
	Code     int            `json:"code,omitempty"`
}

// StatusDetails carries the causes of a Status; an exec session's exit code
// arrives as a cause with reason ExitCode.
type StatusDetails struct {
	Name              string        `json:"name,omitempty"`
	Group             string        `json:"group,omitempty"`
	Kind              string        `json:"kind,omitempty"`
	UID               string        `json:"uid,omitempty"`
	Causes            []StatusCause `json:"causes,omitempty"`
	RetryAfterSeconds int           `json:"retryAfterSeconds,omitempty"`
}

// StatusCause is one cause in a Status.
type StatusCause struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Field   string `json:"field,omitempty"`
}

// LabelSelector is a Kubernetes label selector. An empty selector matches
// everything, which is what an empty podSelector means in a NetworkPolicy.
type LabelSelector struct {
	MatchLabels      map[string]string          `json:"matchLabels,omitempty"`
	MatchExpressions []LabelSelectorRequirement `json:"matchExpressions,omitempty"`
}

// LabelSelectorRequirement is one matchExpressions entry: operator In,
// NotIn, Exists or DoesNotExist.
type LabelSelectorRequirement struct {
	Key      string   `json:"key"`
	Operator string   `json:"operator"`
	Values   []string `json:"values,omitempty"`
}

// Matches reports whether the labels satisfy the selector. An unknown
// operator never matches.
func (s LabelSelector) Matches(labels map[string]string) bool {
	for k, v := range s.MatchLabels {
		if got, ok := labels[k]; !ok || got != v {
			return false
		}
	}
	for _, r := range s.MatchExpressions {
		got, ok := labels[r.Key]
		switch r.Operator {
		case "In":
			if !ok || !contains(r.Values, got) {
				return false
			}
		case "NotIn":
			if ok && contains(r.Values, got) {
				return false
			}
		case "Exists":
			if !ok {
				return false
			}
		case "DoesNotExist":
			if ok {
				return false
			}
		default:
			return false
		}
	}
	return true
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// IntOrString is a value that the API encodes as either a JSON number or a
// JSON string (a NetworkPolicy port is a number or a named port).
type IntOrString struct {
	IsString bool
	IntVal   int32
	StrVal   string
}

// FromInt makes a numeric IntOrString.
func FromInt(v int32) IntOrString { return IntOrString{IntVal: v} }

// FromString makes a string IntOrString.
func FromString(s string) IntOrString { return IntOrString{IsString: true, StrVal: s} }

// String renders the value as the API would print it.
func (v IntOrString) String() string {
	if v.IsString {
		return v.StrVal
	}
	return strconv.Itoa(int(v.IntVal))
}

// MarshalJSON implements json.Marshaler.
func (v IntOrString) MarshalJSON() ([]byte, error) {
	if v.IsString {
		return json.Marshal(v.StrVal)
	}
	return json.Marshal(v.IntVal)
}

// UnmarshalJSON implements json.Unmarshaler.
func (v *IntOrString) UnmarshalJSON(data []byte) error {
	if len(data) > 0 && data[0] == '"' {
		v.IsString = true
		v.IntVal = 0
		return json.Unmarshal(data, &v.StrVal)
	}
	v.IsString = false
	v.StrVal = ""
	return json.Unmarshal(data, &v.IntVal)
}

// ResourceList maps a resource name (cpu, memory, storage) to a quantity in
// the API's string form ("1", "1536Mi", "20Gi").
type ResourceList map[string]string

// LocalObjectReference names an object in the same namespace.
type LocalObjectReference struct {
	Name string `json:"name,omitempty"`
}

// TypedLocalObjectReference names an object of a kind in the same namespace;
// a PVC clone's dataSource is one with kind PersistentVolumeClaim.
type TypedLocalObjectReference struct {
	APIGroup *string `json:"apiGroup"`
	Kind     string  `json:"kind"`
	Name     string  `json:"name"`
}

// Pod is a Kubernetes Pod with the fields Warden reads and writes.
type Pod struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     PodSpec    `json:"spec,omitempty"`
	Status   PodStatus  `json:"status,omitempty"`
}

// PodSpec is the part of a pod spec Warden builds and verifies.
type PodSpec struct {
	Containers                    []Container            `json:"containers"`
	InitContainers                []Container            `json:"initContainers,omitempty"`
	Volumes                       []Volume               `json:"volumes,omitempty"`
	RuntimeClassName              *string                `json:"runtimeClassName,omitempty"`
	SecurityContext               *PodSecurityContext    `json:"securityContext,omitempty"`
	AutomountServiceAccountToken  *bool                  `json:"automountServiceAccountToken,omitempty"`
	ServiceAccountName            string                 `json:"serviceAccountName,omitempty"`
	NodeSelector                  map[string]string      `json:"nodeSelector,omitempty"`
	Tolerations                   []Toleration           `json:"tolerations,omitempty"`
	HostNetwork                   bool                   `json:"hostNetwork,omitempty"`
	HostPID                       bool                   `json:"hostPID,omitempty"`
	HostIPC                       bool                   `json:"hostIPC,omitempty"`
	RestartPolicy                 string                 `json:"restartPolicy,omitempty"`
	TerminationGracePeriodSeconds *int64                 `json:"terminationGracePeriodSeconds,omitempty"`
	EnableServiceLinks            *bool                  `json:"enableServiceLinks,omitempty"`
	ImagePullSecrets              []LocalObjectReference `json:"imagePullSecrets,omitempty"`
	NodeName                      string                 `json:"nodeName,omitempty"`
}

// Container is one container of a pod.
type Container struct {
	Name            string               `json:"name"`
	Image           string               `json:"image,omitempty"`
	Command         []string             `json:"command,omitempty"`
	Args            []string             `json:"args,omitempty"`
	WorkingDir      string               `json:"workingDir,omitempty"`
	Env             []EnvVar             `json:"env,omitempty"`
	Ports           []ContainerPort      `json:"ports,omitempty"`
	VolumeMounts    []VolumeMount        `json:"volumeMounts,omitempty"`
	Resources       ResourceRequirements `json:"resources,omitempty"`
	SecurityContext *SecurityContext     `json:"securityContext,omitempty"`
	ImagePullPolicy string               `json:"imagePullPolicy,omitempty"`
	Stdin           bool                 `json:"stdin,omitempty"`
	TTY             bool                 `json:"tty,omitempty"`
}

// EnvVar is a literal environment variable.
type EnvVar struct {
	Name  string `json:"name"`
	Value string `json:"value,omitempty"`
}

// ContainerPort declares a container port.
type ContainerPort struct {
	Name          string `json:"name,omitempty"`
	ContainerPort int32  `json:"containerPort"`
	Protocol      string `json:"protocol,omitempty"`
}

// VolumeMount mounts a pod volume into a container.
type VolumeMount struct {
	Name      string `json:"name"`
	MountPath string `json:"mountPath"`
	SubPath   string `json:"subPath,omitempty"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// ResourceRequirements are a container's limits and requests.
type ResourceRequirements struct {
	Limits   ResourceList `json:"limits,omitempty"`
	Requests ResourceList `json:"requests,omitempty"`
}

// SecurityContext is a container's security context.
type SecurityContext struct {
	RunAsUser                *int64          `json:"runAsUser,omitempty"`
	RunAsGroup               *int64          `json:"runAsGroup,omitempty"`
	RunAsNonRoot             *bool           `json:"runAsNonRoot,omitempty"`
	Privileged               *bool           `json:"privileged,omitempty"`
	AllowPrivilegeEscalation *bool           `json:"allowPrivilegeEscalation,omitempty"`
	ReadOnlyRootFilesystem   *bool           `json:"readOnlyRootFilesystem,omitempty"`
	Capabilities             *Capabilities   `json:"capabilities,omitempty"`
	SeccompProfile           *SeccompProfile `json:"seccompProfile,omitempty"`
}

// Capabilities adds and drops Linux capabilities.
type Capabilities struct {
	Add  []string `json:"add,omitempty"`
	Drop []string `json:"drop,omitempty"`
}

// SeccompProfile selects a seccomp profile (RuntimeDefault, Unconfined or
// Localhost).
type SeccompProfile struct {
	Type             string  `json:"type"`
	LocalhostProfile *string `json:"localhostProfile,omitempty"`
}

// PodSecurityContext is the pod-level security context.
type PodSecurityContext struct {
	RunAsUser          *int64          `json:"runAsUser,omitempty"`
	RunAsGroup         *int64          `json:"runAsGroup,omitempty"`
	RunAsNonRoot       *bool           `json:"runAsNonRoot,omitempty"`
	FSGroup            *int64          `json:"fsGroup,omitempty"`
	SupplementalGroups []int64         `json:"supplementalGroups,omitempty"`
	SeccompProfile     *SeccompProfile `json:"seccompProfile,omitempty"`
}

// Toleration lets a pod schedule onto a tainted node.
type Toleration struct {
	Key               string `json:"key,omitempty"`
	Operator          string `json:"operator,omitempty"`
	Value             string `json:"value,omitempty"`
	Effect            string `json:"effect,omitempty"`
	TolerationSeconds *int64 `json:"tolerationSeconds,omitempty"`
}

// Volume is a pod volume. Exactly one source is set; a volume whose source
// is none of the ones typed here decodes with every source nil, which a
// verifier must treat as an unknown type.
type Volume struct {
	Name                  string                             `json:"name"`
	PersistentVolumeClaim *PersistentVolumeClaimVolumeSource `json:"persistentVolumeClaim,omitempty"`
	ConfigMap             *ConfigMapVolumeSource             `json:"configMap,omitempty"`
	EmptyDir              *EmptyDirVolumeSource              `json:"emptyDir,omitempty"`
	Secret                *SecretVolumeSource                `json:"secret,omitempty"`
	HostPath              *HostPathVolumeSource              `json:"hostPath,omitempty"`
	Projected             *ProjectedVolumeSource             `json:"projected,omitempty"`
}

// PersistentVolumeClaimVolumeSource mounts a PVC.
type PersistentVolumeClaimVolumeSource struct {
	ClaimName string `json:"claimName"`
	ReadOnly  bool   `json:"readOnly,omitempty"`
}

// ConfigMapVolumeSource mounts a ConfigMap as a directory.
type ConfigMapVolumeSource struct {
	Name        string      `json:"name,omitempty"`
	Items       []KeyToPath `json:"items,omitempty"`
	DefaultMode *int32      `json:"defaultMode,omitempty"`
	Optional    *bool       `json:"optional,omitempty"`
}

// SecretVolumeSource mounts a Secret as a directory.
type SecretVolumeSource struct {
	SecretName  string      `json:"secretName,omitempty"`
	Items       []KeyToPath `json:"items,omitempty"`
	DefaultMode *int32      `json:"defaultMode,omitempty"`
	Optional    *bool       `json:"optional,omitempty"`
}

// KeyToPath maps a ConfigMap or Secret key to a file.
type KeyToPath struct {
	Key  string `json:"key"`
	Path string `json:"path"`
	Mode *int32 `json:"mode,omitempty"`
}

// EmptyDirVolumeSource is a scratch volume.
type EmptyDirVolumeSource struct {
	Medium    string `json:"medium,omitempty"`
	SizeLimit string `json:"sizeLimit,omitempty"`
}

// HostPathVolumeSource mounts a node path; the verifier refuses it.
type HostPathVolumeSource struct {
	Path string  `json:"path"`
	Type *string `json:"type,omitempty"`
}

// ProjectedVolumeSource combines sources; a serviceAccountToken source is
// what the verifier refuses.
type ProjectedVolumeSource struct {
	Sources     []VolumeProjection `json:"sources"`
	DefaultMode *int32             `json:"defaultMode,omitempty"`
}

// VolumeProjection is one projected source; only the kinds Warden inspects
// are typed, others decode as raw JSON in Other.
type VolumeProjection struct {
	ServiceAccountToken *ServiceAccountTokenProjection `json:"serviceAccountToken,omitempty"`
	ConfigMap           *ConfigMapVolumeSource         `json:"configMap,omitempty"`
	Secret              *SecretVolumeSource            `json:"secret,omitempty"`
	DownwardAPI         json.RawMessage                `json:"downwardAPI,omitempty"`
}

// ServiceAccountTokenProjection projects a service account token.
type ServiceAccountTokenProjection struct {
	Audience          string `json:"audience,omitempty"`
	ExpirationSeconds *int64 `json:"expirationSeconds,omitempty"`
	Path              string `json:"path"`
}

// PodStatus is the observed state of a pod.
type PodStatus struct {
	Phase                 string            `json:"phase,omitempty"`
	Reason                string            `json:"reason,omitempty"`
	Message               string            `json:"message,omitempty"`
	HostIP                string            `json:"hostIP,omitempty"`
	PodIP                 string            `json:"podIP,omitempty"`
	PodIPs                []PodIP           `json:"podIPs,omitempty"`
	StartTime             *time.Time        `json:"startTime,omitempty"`
	Conditions            []PodCondition    `json:"conditions,omitempty"`
	InitContainerStatuses []ContainerStatus `json:"initContainerStatuses,omitempty"`
	ContainerStatuses     []ContainerStatus `json:"containerStatuses,omitempty"`
}

// PodIP is one of a pod's addresses.
type PodIP struct {
	IP string `json:"ip"`
}

// PodCondition is one pod condition (PodScheduled, Initialized, Ready,
// ContainersReady).
type PodCondition struct {
	Type               string     `json:"type"`
	Status             string     `json:"status"`
	Reason             string     `json:"reason,omitempty"`
	Message            string     `json:"message,omitempty"`
	LastTransitionTime *time.Time `json:"lastTransitionTime,omitempty"`
}

// ContainerStatus is the observed state of one container; ImageID is the
// pulled image's digest reference, which the verifier pins.
type ContainerStatus struct {
	Name         string         `json:"name"`
	Image        string         `json:"image,omitempty"`
	ImageID      string         `json:"imageID,omitempty"`
	ContainerID  string         `json:"containerID,omitempty"`
	Ready        bool           `json:"ready"`
	Started      *bool          `json:"started,omitempty"`
	RestartCount int32          `json:"restartCount"`
	State        ContainerState `json:"state,omitempty"`
	// Resources is what the kubelet has actually applied to the running
	// container (in-place resize): it lags a resized spec until the
	// resize is done, and is absent on an older kubelet.
	Resources *ResourceRequirements `json:"resources,omitempty"`
}

// ContainerState is one of waiting, running or terminated.
type ContainerState struct {
	Waiting    *ContainerStateWaiting    `json:"waiting,omitempty"`
	Running    *ContainerStateRunning    `json:"running,omitempty"`
	Terminated *ContainerStateTerminated `json:"terminated,omitempty"`
}

// ContainerStateWaiting says why a container has not started.
type ContainerStateWaiting struct {
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// ContainerStateRunning records when a container started.
type ContainerStateRunning struct {
	StartedAt *time.Time `json:"startedAt,omitempty"`
}

// ContainerStateTerminated records how a container ended.
type ContainerStateTerminated struct {
	ExitCode   int32      `json:"exitCode"`
	Reason     string     `json:"reason,omitempty"`
	Message    string     `json:"message,omitempty"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
}

// PersistentVolumeClaim is a workspace volume.
type PersistentVolumeClaim struct {
	TypeMeta
	Metadata ObjectMeta                  `json:"metadata"`
	Spec     PersistentVolumeClaimSpec   `json:"spec,omitempty"`
	Status   PersistentVolumeClaimStatus `json:"status,omitempty"`
}

// PersistentVolumeClaimSpec is a claim's request; DataSource names the
// source claim of a CSI clone.
type PersistentVolumeClaimSpec struct {
	AccessModes      []string                   `json:"accessModes,omitempty"`
	StorageClassName *string                    `json:"storageClassName,omitempty"`
	Resources        VolumeResourceRequirements `json:"resources,omitempty"`
	DataSource       *TypedLocalObjectReference `json:"dataSource,omitempty"`
	VolumeName       string                     `json:"volumeName,omitempty"`
	VolumeMode       *string                    `json:"volumeMode,omitempty"`
}

// VolumeResourceRequirements are a claim's storage requests and limits.
type VolumeResourceRequirements struct {
	Limits   ResourceList `json:"limits,omitempty"`
	Requests ResourceList `json:"requests,omitempty"`
}

// PersistentVolumeClaimStatus is a claim's observed state (Pending, Bound,
// Lost).
type PersistentVolumeClaimStatus struct {
	Phase       string       `json:"phase,omitempty"`
	AccessModes []string     `json:"accessModes,omitempty"`
	Capacity    ResourceList `json:"capacity,omitempty"`
}

// Secret holds credentials; Data is base64 on the wire and bytes here.
type Secret struct {
	TypeMeta
	Metadata   ObjectMeta        `json:"metadata"`
	Type       string            `json:"type,omitempty"`
	Immutable  *bool             `json:"immutable,omitempty"`
	Data       map[string][]byte `json:"data,omitempty"`
	StringData map[string]string `json:"stringData,omitempty"`
}

// ConfigMap holds configuration; the trust bundle is one.
type ConfigMap struct {
	TypeMeta
	Metadata   ObjectMeta        `json:"metadata"`
	Immutable  *bool             `json:"immutable,omitempty"`
	Data       map[string]string `json:"data,omitempty"`
	BinaryData map[string][]byte `json:"binaryData,omitempty"`
}

// Namespace is a cluster namespace; the sandbox namespace's Pod Security
// Admission labels live in Metadata.Labels.
type Namespace struct {
	TypeMeta
	Metadata ObjectMeta      `json:"metadata"`
	Status   NamespaceStatus `json:"status,omitempty"`
}

// NamespaceStatus is a namespace's phase (Active or Terminating).
type NamespaceStatus struct {
	Phase string `json:"phase,omitempty"`
}

// NetworkPolicy is a networking.k8s.io/v1 NetworkPolicy.
type NetworkPolicy struct {
	TypeMeta
	Metadata ObjectMeta        `json:"metadata"`
	Spec     NetworkPolicySpec `json:"spec,omitempty"`
}

// NetworkPolicySpec selects pods and lists their allowed ingress and egress.
type NetworkPolicySpec struct {
	PodSelector LabelSelector              `json:"podSelector"`
	PolicyTypes []string                   `json:"policyTypes,omitempty"`
	Ingress     []NetworkPolicyIngressRule `json:"ingress,omitempty"`
	Egress      []NetworkPolicyEgressRule  `json:"egress,omitempty"`
}

// NetworkPolicyIngressRule allows traffic from peers on ports.
type NetworkPolicyIngressRule struct {
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
	From  []NetworkPolicyPeer `json:"from,omitempty"`
}

// NetworkPolicyEgressRule allows traffic to peers on ports.
type NetworkPolicyEgressRule struct {
	Ports []NetworkPolicyPort `json:"ports,omitempty"`
	To    []NetworkPolicyPeer `json:"to,omitempty"`
}

// NetworkPolicyPort is a port or port range with a protocol.
type NetworkPolicyPort struct {
	Protocol *string      `json:"protocol,omitempty"`
	Port     *IntOrString `json:"port,omitempty"`
	EndPort  *int32       `json:"endPort,omitempty"`
}

// NetworkPolicyPeer selects pods, namespaces or an IP block.
type NetworkPolicyPeer struct {
	PodSelector       *LabelSelector `json:"podSelector,omitempty"`
	NamespaceSelector *LabelSelector `json:"namespaceSelector,omitempty"`
	IPBlock           *IPBlock       `json:"ipBlock,omitempty"`
}

// IPBlock is a CIDR with exceptions.
type IPBlock struct {
	CIDR   string   `json:"cidr"`
	Except []string `json:"except,omitempty"`
}

// RuntimeClass is a node.k8s.io/v1 RuntimeClass; Handler is the container
// runtime handler (runsc, kata-qemu) the verifier checks against the tier.
// Node is a cluster node with the fields the cluster page shows.
type Node struct {
	TypeMeta
	Metadata ObjectMeta `json:"metadata"`
	Spec     NodeSpec   `json:"spec,omitempty"`
	Status   NodeStatus `json:"status,omitempty"`
}

// NodeSpec is the part of a node spec Warden reads.
type NodeSpec struct {
	Unschedulable bool `json:"unschedulable,omitempty"`
}

// NodeStatus is the observed state of a node.
type NodeStatus struct {
	Capacity    ResourceList    `json:"capacity,omitempty"`
	Allocatable ResourceList    `json:"allocatable,omitempty"`
	Conditions  []NodeCondition `json:"conditions,omitempty"`
	NodeInfo    NodeSystemInfo  `json:"nodeInfo,omitempty"`
}

// NodeCondition is one node condition (Ready, MemoryPressure, ...).
type NodeCondition struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

// NodeSystemInfo identifies the node's software.
type NodeSystemInfo struct {
	KubeletVersion          string `json:"kubeletVersion,omitempty"`
	ContainerRuntimeVersion string `json:"containerRuntimeVersion,omitempty"`
	OSImage                 string `json:"osImage,omitempty"`
	KernelVersion           string `json:"kernelVersion,omitempty"`
	Architecture            string `json:"architecture,omitempty"`
	OperatingSystem         string `json:"operatingSystem,omitempty"`
}

// NodeMetricsItem is one node's live usage from metrics.k8s.io.
type NodeMetricsItem struct {
	Metadata ObjectMeta   `json:"metadata"`
	Usage    ResourceList `json:"usage,omitempty"`
}

// PodMetricsItem is one pod's live usage from metrics.k8s.io, per
// container.
type PodMetricsItem struct {
	Metadata   ObjectMeta         `json:"metadata"`
	Containers []ContainerMetrics `json:"containers,omitempty"`
}

// ContainerMetrics is one container's usage.
type ContainerMetrics struct {
	Name  string       `json:"name"`
	Usage ResourceList `json:"usage,omitempty"`
}

type RuntimeClass struct {
	TypeMeta
	Metadata   ObjectMeta  `json:"metadata"`
	Handler    string      `json:"handler"`
	Overhead   *Overhead   `json:"overhead,omitempty"`
	Scheduling *Scheduling `json:"scheduling,omitempty"`
}

// Overhead is the per-pod resource overhead of a RuntimeClass.
type Overhead struct {
	PodFixed ResourceList `json:"podFixed,omitempty"`
}

// Scheduling constrains where a RuntimeClass's pods run.
type Scheduling struct {
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`
	Tolerations  []Toleration      `json:"tolerations,omitempty"`
}

// ValidatingAdmissionPolicy is an admissionregistration.k8s.io/v1 policy.
type ValidatingAdmissionPolicy struct {
	TypeMeta
	Metadata ObjectMeta                    `json:"metadata"`
	Spec     ValidatingAdmissionPolicySpec `json:"spec,omitempty"`
}

// ValidatingAdmissionPolicySpec is the policy's constraints and validations.
type ValidatingAdmissionPolicySpec struct {
	ParamKind        *ParamKind       `json:"paramKind,omitempty"`
	MatchConstraints *MatchResources  `json:"matchConstraints,omitempty"`
	Validations      []Validation     `json:"validations,omitempty"`
	FailurePolicy    *string          `json:"failurePolicy,omitempty"`
	MatchConditions  []MatchCondition `json:"matchConditions,omitempty"`
	Variables        []Variable       `json:"variables,omitempty"`
}

// ParamKind names the kind of a policy's parameter objects.
type ParamKind struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
}

// MatchResources says which requests a policy or binding applies to.
type MatchResources struct {
	NamespaceSelector    *LabelSelector            `json:"namespaceSelector,omitempty"`
	ObjectSelector       *LabelSelector            `json:"objectSelector,omitempty"`
	ResourceRules        []NamedRuleWithOperations `json:"resourceRules,omitempty"`
	ExcludeResourceRules []NamedRuleWithOperations `json:"excludeResourceRules,omitempty"`
	MatchPolicy          *string                   `json:"matchPolicy,omitempty"`
}

// NamedRuleWithOperations is one resource rule of MatchResources.
type NamedRuleWithOperations struct {
	ResourceNames []string `json:"resourceNames,omitempty"`
	Operations    []string `json:"operations,omitempty"`
	APIGroups     []string `json:"apiGroups,omitempty"`
	APIVersions   []string `json:"apiVersions,omitempty"`
	Resources     []string `json:"resources,omitempty"`
	Scope         *string  `json:"scope,omitempty"`
}

// Validation is one CEL expression of a policy.
type Validation struct {
	Expression        string  `json:"expression"`
	Message           string  `json:"message,omitempty"`
	MessageExpression string  `json:"messageExpression,omitempty"`
	Reason            *string `json:"reason,omitempty"`
}

// MatchCondition is a CEL precondition of a policy.
type MatchCondition struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

// Variable is a named CEL expression a policy's validations may use.
type Variable struct {
	Name       string `json:"name"`
	Expression string `json:"expression"`
}

// ValidatingAdmissionPolicyBinding binds a policy to resources.
type ValidatingAdmissionPolicyBinding struct {
	TypeMeta
	Metadata ObjectMeta                           `json:"metadata"`
	Spec     ValidatingAdmissionPolicyBindingSpec `json:"spec,omitempty"`
}

// ValidatingAdmissionPolicyBindingSpec names the policy and what it binds to.
type ValidatingAdmissionPolicyBindingSpec struct {
	PolicyName        string          `json:"policyName,omitempty"`
	ParamRef          *ParamRef       `json:"paramRef,omitempty"`
	MatchResources    *MatchResources `json:"matchResources,omitempty"`
	ValidationActions []string        `json:"validationActions,omitempty"`
}

// ParamRef names a binding's parameter object.
type ParamRef struct {
	Name                    string         `json:"name,omitempty"`
	Namespace               string         `json:"namespace,omitempty"`
	Selector                *LabelSelector `json:"selector,omitempty"`
	ParameterNotFoundAction *string        `json:"parameterNotFoundAction,omitempty"`
}

// DeleteOptions shape a Delete call. GracePeriodSeconds nil keeps the
// object's default; Propagation is Background, Foreground or Orphan; UID,
// when set, is a precondition so a replacement object with the same name is
// not deleted by mistake.
type DeleteOptions struct {
	GracePeriodSeconds *int64
	Propagation        string
	UID                string
}

// ListOptions filter a List call.
type ListOptions struct {
	LabelSelector string
	FieldSelector string
}

// ApplyOptions shape a server-side apply. FieldManager is required; Force
// takes over fields another manager owns.
type ApplyOptions struct {
	FieldManager string
	Force        bool
}

// Version is the API server's /version answer.
type Version struct {
	Major      string `json:"major"`
	Minor      string `json:"minor"`
	GitVersion string `json:"gitVersion"`
	Platform   string `json:"platform"`
}

// String renders the version as kubectl prints it.
func (v Version) String() string {
	if v.GitVersion != "" {
		return v.GitVersion
	}
	return fmt.Sprintf("v%s.%s", v.Major, v.Minor)
}

// Int64 returns a pointer to v, for the *int64 option fields.
func Int64(v int64) *int64 { return &v }

// Int32 returns a pointer to v.
func Int32(v int32) *int32 { return &v }

// Bool returns a pointer to v, for the *bool spec fields.
func Bool(v bool) *bool { return &v }

// String returns a pointer to v, for the *string spec fields.
func String(v string) *string { return &v }
