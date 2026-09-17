package kube

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"warden/chat/internal/config"
	"warden/chat/internal/kube"
	"warden/chat/internal/sandbox"
)

var update = flag.Bool("update", false, "rewrite the golden files")

func golden(t *testing.T, name string, v any) {
	t.Helper()
	got, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	path := filepath.Join("testdata", name)
	if *update {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with -update to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from the golden file (run with -update to accept):\n%s", name, got)
	}
}

// The pod and claim specs are pure functions of the options and the
// runtime: the golden files are the contract with the chart's admission
// policy and LimitRange and with the verifier (plan decisions 2, 8, 9, 11).
func TestPodAndClaimSpecGolden(t *testing.T) {
	o := testOptions()
	o.NodeSelector = map[string]string{"warden.monaddle.com/pool": "sandboxes"}
	o.Tolerations = []config.Toleration{{Key: "sandbox.gke.io/runtime", Operator: "Equal", Value: "gvisor", Effect: "NoSchedule"}}
	golden(t, "pod.golden.json", PodSpec(o, sandbox.RuntimeSpec{Name: "wc-0123456789abcdef01234567", SandboxID: "sandbox-one", Generation: "gen-1"}, WorkspaceFresh))
	golden(t, "pod-spare.golden.json", PodSpec(testOptions(), sandbox.RuntimeSpec{Name: "wc-spare-0123456789abcdef", Spare: true}, WorkspaceFresh))
	golden(t, "claim.golden.json", ClaimSpec(o, "wc-0123456789abcdef01234567", "sandbox-one", "", false))
	golden(t, "claim-clone.golden.json", ClaimSpec(o, "wc-fork", "sandbox-two", "wc-0123456789abcdef01234567", false))
}

// The properties the admission policy and the spike require, checked by
// name so a golden refresh cannot lose them silently.
func TestPodSpecHardening(t *testing.T) {
	pod := PodSpec(testOptions(), sandbox.RuntimeSpec{Name: "wc-abc", SandboxID: "s", Generation: "g"}, WorkspaceFresh)
	spec := pod.Spec
	if spec.RuntimeClassName == nil || *spec.RuntimeClassName != "gvisor" {
		t.Fatal("runtimeClassName")
	}
	if spec.AutomountServiceAccountToken == nil || *spec.AutomountServiceAccountToken {
		t.Fatal("automountServiceAccountToken must be false")
	}
	if spec.HostNetwork || spec.HostPID || spec.HostIPC {
		t.Fatal("host namespaces")
	}
	if pod.Metadata.Labels[LabelSandbox] != "wc-abc" || pod.Metadata.Labels[LabelManagedBy] != ManagedBy || pod.Metadata.Labels[LabelSpare] != "" {
		t.Fatalf("labels %v", pod.Metadata.Labels)
	}
	if pod.Metadata.Annotations[AnnotationSandboxID] != "s" || pod.Metadata.Annotations[AnnotationGeneration] != "g" {
		t.Fatalf("annotations %v", pod.Metadata.Annotations)
	}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim == nil && v.ConfigMap == nil && v.EmptyDir == nil {
			t.Fatalf("volume %s is not a claim, ConfigMap or emptyDir", v.Name)
		}
		if v.HostPath != nil || v.Projected != nil || v.Secret != nil {
			t.Fatalf("volume %s has a forbidden source", v.Name)
		}
	}
	if len(spec.Containers) != 1 {
		t.Fatal("one container")
	}
	c := spec.Containers[0]
	if c.Image != "ghcr.io/monaddle-too/warden-guest-base@sha256:"+testOptions().GuestImageDigest[len("sha256:"):] {
		t.Fatalf("image %s", c.Image)
	}
	if len(c.Command) != 0 || len(c.Args) == 0 {
		t.Fatal("args, not command, so the image entrypoint stays")
	}
	if c.Resources.Limits["memory"] != "1024Mi" || c.Resources.Requests["memory"] != "1024Mi" || c.Resources.Limits["cpu"] != "1000m" || c.Resources.Requests["cpu"] != "1000m" {
		t.Fatalf("resources %+v", c.Resources)
	}
	sc := c.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation != nil || sc.Privileged != nil || sc.Capabilities == nil || len(sc.Capabilities.Add) != 0 {
		t.Fatalf("container security context %+v", sc)
	}
	dropped := map[string]bool{}
	for _, c := range sc.Capabilities.Drop {
		dropped[c] = true
	}
	// sudo and root's file operations need these; nothing is added.
	for _, kept := range []string{"SETUID", "SETGID", "CHOWN", "DAC_OVERRIDE", "FOWNER"} {
		if dropped[kept] || dropped["ALL"] {
			t.Fatalf("capability %s dropped: sudo in the guest would fail", kept)
		}
	}
	for _, gone := range []string{"NET_RAW", "MKNOD", "SYS_CHROOT", "SETFCAP", "SETPCAP"} {
		if !dropped[gone] {
			t.Fatalf("capability %s kept", gone)
		}
	}
	psc := spec.SecurityContext
	if psc == nil || psc.RunAsUser == nil || *psc.RunAsUser != 1000 || psc.RunAsGroup == nil || *psc.RunAsGroup != 1000 || psc.FSGroup == nil || *psc.FSGroup != 1000 || psc.RunAsNonRoot == nil || !*psc.RunAsNonRoot || psc.SeccompProfile == nil || psc.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("pod security context %+v", psc)
	}
	mounts := map[string]kube.VolumeMount{}
	for _, m := range c.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if mounts["/home/agent"].Name != "home" || mounts[TrustMountPath].Name != "trust" || !mounts[TrustMountPath].ReadOnly || mounts["/tmp"].Name != "tmp" {
		t.Fatalf("mounts %+v", c.VolumeMounts)
	}
	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != StopGraceSeconds {
		t.Fatal("grace period")
	}
	// A spare carries the spare label and no sandbox annotations.
	spare := PodSpec(testOptions(), sandbox.RuntimeSpec{Name: "wc-spare-1", Spare: true}, WorkspaceFresh)
	if spare.Metadata.Labels[LabelSpare] != "true" || spare.Metadata.Annotations[AnnotationSandboxID] != "" || spare.Metadata.Annotations[AnnotationGeneration] != "" {
		t.Fatalf("spare pod %+v", spare.Metadata)
	}
	// The claim: the configured class and size, labelled like the pod; a
	// fork names its source claim as the dataSource.
	claim := ClaimSpec(testOptions(), "wc-abc", "s", "", false)
	if claim.Spec.StorageClassName == nil || *claim.Spec.StorageClassName != "local-path" || claim.Spec.Resources.Requests["storage"] != "4Gi" || claim.Spec.DataSource != nil || claim.Metadata.Labels[LabelSandbox] != "wc-abc" || claim.Metadata.Labels[LabelManagedBy] != ManagedBy {
		t.Fatalf("claim %+v", claim)
	}
	if plain := ClaimSpec(Options{Namespace: "n"}, "wc-abc", "", "", false); plain.Spec.StorageClassName != nil || plain.Spec.Resources.Requests["storage"] != "20Gi" {
		t.Fatalf("default claim %+v", plain.Spec)
	}
	fork := ClaimSpec(testOptions(), "wc-fork", "s2", "wc-abc", false)
	if fork.Spec.DataSource == nil || fork.Spec.DataSource.Kind != "PersistentVolumeClaim" || fork.Spec.DataSource.Name != "wc-abc" || fork.Spec.DataSource.APIGroup != nil {
		t.Fatalf("fork claim %+v", fork.Spec.DataSource)
	}
}

func TestOptionsAndNames(t *testing.T) {
	for _, name := range []string{"wc-0123456789abcdef01234567", "wc-spare-0123456789abcdef"} {
		if err := validName(name); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range []string{"", "wc", "Wc-abc", "wc_abc", "wc-abc/x", "-wc-abc", "wc-abc-", "wc-" + string(bytes.Repeat([]byte("a"), 70))} {
		if err := validName(name); err == nil {
			t.Fatalf("%q accepted", name)
		}
	}
	api := newFakeAPI(t)
	if _, err := New(nil, testOptions()); err == nil {
		t.Fatal("nil client accepted")
	}
	bad := testOptions()
	bad.Tier = "runc"
	if _, err := New(api.client(), bad); err == nil {
		t.Fatal("unknown tier accepted")
	}
	bad = testOptions()
	bad.MemoryMB = 100
	if _, err := New(api.client(), bad); err == nil {
		t.Fatal("tiny memory accepted")
	}
	bad = testOptions()
	bad.GuestImageDigest = ""
	if _, err := New(api.client(), bad); err == nil {
		t.Fatal("missing digest accepted")
	}
	d := newTestDriver(t, api, testOptions())
	if d.launchOptions().CodexSandboxMode != "danger-full-access" {
		t.Fatal("gvisor tier must relax Codex's inner sandbox")
	}
	kata := testOptions()
	kata.Tier = config.TierKata
	if newTestDriver(t, api, kata).launchOptions().CodexSandboxMode != "" {
		t.Fatal("kata tier keeps Codex's inner sandbox")
	}
}
