package kube

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestResourcePaths(t *testing.T) {
	cases := []struct {
		r          Resource
		ns, name   string
		want       string
		apiVersion string
	}{
		{Pods, "warden-sandboxes", "sbx-1", "/api/v1/namespaces/warden-sandboxes/pods/sbx-1", "v1"},
		{Pods, "warden-sandboxes", "", "/api/v1/namespaces/warden-sandboxes/pods", "v1"},
		{Pods, "", "", "/api/v1/pods", "v1"},
		{Namespaces, "", "warden-sandboxes", "/api/v1/namespaces/warden-sandboxes", "v1"},
		{NetworkPolicies, "ns", "default-deny", "/apis/networking.k8s.io/v1/namespaces/ns/networkpolicies/default-deny", "networking.k8s.io/v1"},
		{RuntimeClasses, "", "gvisor", "/apis/node.k8s.io/v1/runtimeclasses/gvisor", "node.k8s.io/v1"},
		{ValidatingAdmissionPolicyBindings, "", "warden", "/apis/admissionregistration.k8s.io/v1/validatingadmissionpolicybindings/warden", "admissionregistration.k8s.io/v1"},
		{Secrets, "warden", "a b", "/api/v1/namespaces/warden/secrets/a%20b", "v1"},
	}
	for _, c := range cases {
		if got := c.r.path(c.ns, c.name); got != c.want {
			t.Errorf("%s/%s/%s: got %s, want %s", c.r.Resource, c.ns, c.name, got, c.want)
		}
		if got := c.r.APIVersion(); got != c.apiVersion {
			t.Errorf("%s: apiVersion %s, want %s", c.r.Resource, got, c.apiVersion)
		}
	}
}

func TestNewClientRejectsBadHosts(t *testing.T) {
	for _, host := range []string{"", "ftp://x", "https://", "://x"} {
		if _, err := NewClient(&Config{Host: host}); err == nil {
			t.Errorf("host %q accepted", host)
		}
	}
	c, err := NewClient(&Config{Host: "10.43.0.1:443", Namespace: "warden"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Host() != "https://10.43.0.1:443" || c.Namespace() != "warden" {
		t.Fatalf("host %s namespace %s", c.Host(), c.Namespace())
	}
	if _, err := NewClient(&Config{Host: "https://x", CertData: []byte("junk"), KeyData: []byte("junk")}); err == nil {
		t.Error("bad client certificate accepted")
	}
	if _, err := NewClient(&Config{Host: "https://x", CAData: []byte("junk")}); err == nil {
		t.Error("bad CA accepted")
	}
}

func TestGetNotFound(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	var pod Pod
	err := c.Get(testContext(t), Pods, "ns", "missing", &pod)
	if !IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 404 || status.Reason != "NotFound" || status.Details == nil || status.Details.Name != "missing" {
		t.Fatalf("status %+v", status)
	}
	if !strings.Contains(err.Error(), `pods "missing" not found`) || !strings.Contains(err.Error(), "404 NotFound") {
		t.Fatalf("message %q", err)
	}
	if IsConflict(err) || IsForbidden(err) || IsAlreadyExists(err) || IsGone(err) || IsUnauthorized(err) {
		t.Fatal("wrong helper matched")
	}
	if err := c.Get(testContext(t), Pods, "ns", "", &pod); err == nil {
		t.Fatal("empty name accepted")
	}
}

func TestCreateGetUpdateDelete(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	ctx := testContext(t)
	pod := Pod{Metadata: ObjectMeta{Name: "sbx-1", Labels: map[string]string{"warden.monaddle.com/sandbox": "sbx-1"}}, Spec: PodSpec{
		RuntimeClassName:             String("gvisor"),
		AutomountServiceAccountToken: Bool(false),
		Containers:                   []Container{{Name: "guest", Image: "ghcr.io/example/guest@sha256:abc", Resources: ResourceRequirements{Limits: ResourceList{"memory": "1536Mi", "cpu": "1"}}}},
		Volumes:                      []Volume{{Name: "home", PersistentVolumeClaim: &PersistentVolumeClaimVolumeSource{ClaimName: "sbx-1"}}, {Name: "tmp", EmptyDir: &EmptyDirVolumeSource{}}},
	}}
	var created Pod
	if err := c.Create(ctx, Pods, "ns", pod, &created); err != nil {
		t.Fatal(err)
	}
	req := api.lastRequest()
	if req.Method != "POST" || req.Path != "/api/v1/namespaces/ns/pods" || req.ContentType != "application/json" {
		t.Fatalf("request %+v", req)
	}
	if req.Header.Get("Accept") != "application/json" || !strings.HasPrefix(req.Header.Get("User-Agent"), "warden-kube") {
		t.Fatalf("headers %v", req.Header)
	}
	var sent map[string]any
	if err := json.Unmarshal(req.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["apiVersion"] != "v1" || sent["kind"] != "Pod" || metaString(sent, "namespace") != "ns" {
		t.Fatalf("type meta not filled: %v", sent)
	}
	if created.Metadata.UID == "" || created.Metadata.ResourceVersion == "" || created.Metadata.Namespace != "ns" || created.Metadata.CreationTimestamp == nil {
		t.Fatalf("created %+v", created.Metadata)
	}
	if *created.Spec.RuntimeClassName != "gvisor" || *created.Spec.AutomountServiceAccountToken || created.Spec.Volumes[1].EmptyDir == nil {
		t.Fatalf("spec %+v", created.Spec)
	}
	if err := c.Create(ctx, Pods, "ns", pod, nil); !IsAlreadyExists(err) {
		t.Fatalf("want AlreadyExists, got %v", err)
	} else if IsConflict(err) {
		t.Fatal("AlreadyExists must not count as Conflict")
	}

	var got Pod
	if err := c.Get(ctx, Pods, "ns", "sbx-1", &got); err != nil {
		t.Fatal(err)
	}
	if got.Metadata.UID != created.Metadata.UID || got.Spec.Containers[0].Resources.Limits["memory"] != "1536Mi" {
		t.Fatalf("got %+v", got)
	}

	// A stale resourceVersion is a conflict; the current one updates.
	stale := got
	stale.Metadata.ResourceVersion = "999"
	stale.Metadata.Labels["warden.monaddle.com/spare"] = "true"
	if err := c.Update(ctx, Pods, "ns", "sbx-1", stale, nil); !IsConflict(err) {
		t.Fatalf("want Conflict, got %v", err)
	}
	got.Metadata.Labels["warden.monaddle.com/spare"] = "true"
	var updated Pod
	if err := c.Update(ctx, Pods, "ns", "sbx-1", got, &updated); err != nil {
		t.Fatal(err)
	}
	if req := api.lastRequest(); req.Method != "PUT" || req.Path != "/api/v1/namespaces/ns/pods/sbx-1" {
		t.Fatalf("request %+v", req)
	}
	if updated.Metadata.Labels["warden.monaddle.com/spare"] != "true" || updated.Metadata.ResourceVersion == got.Metadata.ResourceVersion || updated.Metadata.UID != got.Metadata.UID {
		t.Fatalf("updated %+v", updated.Metadata)
	}
	if err := c.Update(ctx, Pods, "ns", "other", got, nil); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched name accepted: %v", err)
	}

	// Delete with grace period, propagation and a UID precondition.
	err := c.Delete(ctx, Pods, "ns", "sbx-1", DeleteOptions{GracePeriodSeconds: Int64(10), Propagation: "Background", UID: "wrong"})
	if !IsConflict(err) {
		t.Fatalf("want Conflict on UID precondition, got %v", err)
	}
	if err := c.Delete(ctx, Pods, "ns", "sbx-1", DeleteOptions{GracePeriodSeconds: Int64(10), Propagation: "Background", UID: got.Metadata.UID}); err != nil {
		t.Fatal(err)
	}
	req = api.lastRequest()
	if req.Method != "DELETE" || req.ContentType != "application/json" {
		t.Fatalf("request %+v", req)
	}
	var options map[string]any
	if err := json.Unmarshal(req.Body, &options); err != nil {
		t.Fatal(err)
	}
	if options["kind"] != "DeleteOptions" || options["gracePeriodSeconds"] != float64(10) || options["propagationPolicy"] != "Background" || options["preconditions"].(map[string]any)["uid"] != got.Metadata.UID {
		t.Fatalf("delete options %v", options)
	}
	if err := c.Delete(ctx, Pods, "ns", "sbx-1", DeleteOptions{}); !IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
	if err := c.Get(ctx, Pods, "ns", "sbx-1", &got); !IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
}

func TestListSelectors(t *testing.T) {
	api := newFakeAPI(t)
	api.seed(Pods, "ns", podObject("a", map[string]string{"warden.monaddle.com/sandbox": "a", "warden.monaddle.com/egress": "gateway"}))
	api.seed(Pods, "ns", podObject("b", map[string]string{"warden.monaddle.com/sandbox": "b"}))
	api.seed(Pods, "other", podObject("c", map[string]string{"warden.monaddle.com/sandbox": "c", "warden.monaddle.com/egress": "gateway"}))
	c := api.client()
	ctx := testContext(t)

	var pods List[Pod]
	if err := c.List(ctx, Pods, "ns", ListOptions{}, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 2 || pods.Items[0].Metadata.Name != "a" || pods.Items[1].Metadata.Name != "b" || pods.Metadata.ResourceVersion == "" || pods.Kind != "PodList" {
		t.Fatalf("list %+v", pods)
	}
	if err := c.List(ctx, Pods, "ns", ListOptions{LabelSelector: "warden.monaddle.com/egress=gateway"}, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Metadata.Name != "a" {
		t.Fatalf("label selector: %+v", pods.Items)
	}
	if q := api.lastRequest().Query; q.Get("labelSelector") != "warden.monaddle.com/egress=gateway" {
		t.Fatalf("query %v", q)
	}
	if err := c.List(ctx, Pods, "", ListOptions{LabelSelector: "warden.monaddle.com/egress", FieldSelector: "metadata.name=c"}, &pods); err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 || pods.Items[0].Metadata.Namespace != "other" {
		t.Fatalf("all namespaces: %+v", pods.Items)
	}
	if req := api.lastRequest(); req.Path != "/api/v1/pods" || req.Query.Get("fieldSelector") != "metadata.name=c" {
		t.Fatalf("request %+v", req)
	}
	var generic List[Object]
	if err := c.List(ctx, Pods, "ns", ListOptions{}, &generic); err != nil {
		t.Fatal(err)
	}
	if len(generic.Items) != 2 || generic.Items[1].Meta().Name != "b" || generic.Items[1].Kind() != "Pod" {
		t.Fatalf("generic list %+v", generic.Items)
	}
}

func TestPatchLabels(t *testing.T) {
	api := newFakeAPI(t)
	api.seed(Pods, "ns", podObject("a", map[string]string{"warden.monaddle.com/sandbox": "a", "warden.monaddle.com/egress": "gateway"}))
	c := api.client()
	ctx := testContext(t)
	var pod Pod
	if err := c.Patch(ctx, Pods, "ns", "a", MergePatchLabels(map[string]string{"warden.monaddle.com/spare": "true"}, "warden.monaddle.com/egress"), &pod); err != nil {
		t.Fatal(err)
	}
	req := api.lastRequest()
	if req.Method != "PATCH" || req.ContentType != "application/merge-patch+json" {
		t.Fatalf("request %+v", req)
	}
	if string(req.Body) != `{"metadata":{"labels":{"warden.monaddle.com/egress":null,"warden.monaddle.com/spare":"true"}}}` {
		t.Fatalf("patch body %s", req.Body)
	}
	if _, ok := pod.Metadata.Labels["warden.monaddle.com/egress"]; ok || pod.Metadata.Labels["warden.monaddle.com/spare"] != "true" || pod.Metadata.Labels["warden.monaddle.com/sandbox"] != "a" {
		t.Fatalf("labels %v", pod.Metadata.Labels)
	}
	if err := c.Patch(ctx, Pods, "ns", "missing", MergePatchLabels(nil, "x"), nil); !IsNotFound(err) {
		t.Fatalf("want NotFound, got %v", err)
	}
	if err := c.Patch(ctx, Pods, "ns", "a", []byte("{"), nil); err == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestApply(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	ctx := testContext(t)
	bundle := ConfigMap{Data: map[string]string{"ca-certificates.crt": "PEM"}}
	var applied ConfigMap
	if err := c.Apply(ctx, ConfigMaps, "warden", "warden-guest-trust", bundle, ApplyOptions{FieldManager: "warden-policy"}, &applied); err != nil {
		t.Fatal(err)
	}
	req := api.lastRequest()
	if req.Method != "PATCH" || req.ContentType != "application/apply-patch+yaml" || req.Path != "/api/v1/namespaces/warden/configmaps/warden-guest-trust" {
		t.Fatalf("request %+v", req)
	}
	if req.Query.Get("fieldManager") != "warden-policy" || req.Query.Has("force") {
		t.Fatalf("query %v", req.Query)
	}
	var sent map[string]any
	if err := json.Unmarshal(req.Body, &sent); err != nil {
		t.Fatal(err)
	}
	if sent["apiVersion"] != "v1" || sent["kind"] != "ConfigMap" || metaString(sent, "name") != "warden-guest-trust" || metaString(sent, "namespace") != "warden" {
		t.Fatalf("apply body %s", req.Body)
	}
	if applied.Metadata.UID == "" || applied.Data["ca-certificates.crt"] != "PEM" {
		t.Fatalf("applied %+v", applied)
	}
	bundle.Data["ca-certificates.crt"] = "PEM2"
	if err := c.Apply(ctx, ConfigMaps, "warden", "warden-guest-trust", bundle, ApplyOptions{FieldManager: "warden-policy", Force: true}, &applied); err != nil {
		t.Fatal(err)
	}
	if q := api.lastRequest().Query; q.Get("force") != "true" {
		t.Fatalf("query %v", q)
	}
	if applied.Data["ca-certificates.crt"] != "PEM2" {
		t.Fatalf("applied %+v", applied)
	}
	if err := c.Apply(ctx, ConfigMaps, "warden", "x", bundle, ApplyOptions{}, nil); err == nil {
		t.Fatal("missing field manager accepted")
	}
	// A conflict on apply is IsConflict.
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		if r.Method == "PATCH" {
			writeStatus(w, http.StatusConflict, "Conflict", "Apply failed with 1 conflict: conflict with \"other\": .data.ca-certificates.crt")
			return true
		}
		return false
	})
	if err := c.Apply(ctx, ConfigMaps, "warden", "warden-guest-trust", bundle, ApplyOptions{FieldManager: "warden-policy"}, nil); !IsConflict(err) {
		t.Fatalf("want Conflict, got %v", err)
	}
}

func TestForbiddenAndNonStatusErrors(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	ctx := testContext(t)
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		switch {
		case strings.Contains(r.URL.Path, "secrets"):
			writeStatus(w, http.StatusForbidden, "Forbidden", `secrets "x" is forbidden: User "system:serviceaccount:warden:runner" cannot get resource "secrets"`)
		case strings.Contains(r.URL.Path, "configmaps"):
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte("<html><body>bad gateway from the ingress</body></html>"))
		default:
			return false
		}
		return true
	})
	var secret Secret
	err := c.Get(ctx, Secrets, "warden", "x", &secret)
	if !IsForbidden(err) || !strings.Contains(err.Error(), "cannot get resource") {
		t.Fatalf("want Forbidden, got %v", err)
	}
	var cm ConfigMap
	err = c.Get(ctx, ConfigMaps, "warden", "x", &cm)
	var status *StatusError
	if !errors.As(err, &status) || status.Code != 502 || status.Reason != "" || !strings.Contains(status.Message, "bad gateway") {
		t.Fatalf("want a 502 StatusError with the body, got %v", err)
	}
	if !strings.Contains(err.Error(), "502 Bad Gateway") {
		t.Fatalf("message %q", err)
	}
}

func TestTimeoutAndCancellation(t *testing.T) {
	api := newFakeAPI(t)
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	api.setOverride(func(w http.ResponseWriter, r *http.Request) bool {
		select {
		case <-release:
		case <-r.Context().Done():
		}
		return true
	})
	c := api.client()
	c.Timeout = 100 * time.Millisecond
	var pod Pod
	start := time.Now()
	err := c.Get(context.Background(), Pods, "ns", "slow", &pod)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want deadline exceeded, got %v", err)
	}
	if time.Since(start) > 5*time.Second {
		t.Fatal("timeout not applied")
	}
	c.Timeout = time.Minute
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	if err := c.Get(ctx, Pods, "ns", "slow", &pod); !errors.Is(err, context.Canceled) {
		t.Fatalf("want canceled, got %v", err)
	}
	// Retry hints are readable.
	if RetryAfter(&StatusError{Code: 429, Details: &StatusDetails{RetryAfterSeconds: 3}}) != 3*time.Second || RetryAfter(errors.New("x")) != 0 {
		t.Fatal("RetryAfter")
	}
}

func TestServerVersionAndGenericObject(t *testing.T) {
	api := newFakeAPI(t)
	c := api.client()
	ctx := testContext(t)
	v, err := c.ServerVersion(ctx)
	if err != nil || v.String() != "v1.34.1+k3s1" || v.Minor != "34" {
		t.Fatalf("version %+v %v", v, err)
	}
	quota := Resource{Group: "", Version: "v1", Resource: "resourcequotas", Kind: "ResourceQuota"}
	named := Object{"metadata": map[string]any{"name": "sandboxes"}, "spec": map[string]any{"hard": map[string]any{"pods": "8"}}}
	var created Object
	if err := c.Create(ctx, quota, "ns", named, &created); err != nil {
		t.Fatal(err)
	}
	if created.Kind() != "ResourceQuota" || created["apiVersion"] != "v1" {
		t.Fatalf("created %v", created)
	}
	var got Object
	if err := c.Get(ctx, quota, "ns", "sandboxes", &got); err != nil {
		t.Fatal(err)
	}
	meta := got.Meta()
	if meta.Name != "sandboxes" || meta.Namespace != "ns" || meta.UID == "" || got["spec"].(map[string]any)["hard"].(map[string]any)["pods"] != "8" {
		t.Fatalf("got %v", got)
	}
}

func TestTokenFileReread(t *testing.T) {
	api := newFakeAPI(t)
	api.setToken("one")
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	if err := os.WriteFile(tokenFile, []byte("one\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := api.config()
	cfg.Token, cfg.TokenFile = "", tokenFile
	c, err := NewClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	c.token.now = func() time.Time { return now }
	ctx := testContext(t)
	api.seed(Pods, "ns", podObject("a", nil))
	var pod Pod
	if err := c.Get(ctx, Pods, "ns", "a", &pod); err != nil {
		t.Fatal(err)
	}
	if api.lastRequest().Header.Get("Authorization") != "Bearer one" {
		t.Fatalf("token not sent: %v", api.lastRequest().Header)
	}
	// The server rotates; the file changes; the cached token is refused
	// once and the file is re-read.
	api.setToken("two")
	if err := os.WriteFile(tokenFile, []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	before := len(api.recorded())
	if err := c.Get(ctx, Pods, "ns", "a", &pod); err != nil {
		t.Fatal(err)
	}
	reqs := api.recorded()[before:]
	if len(reqs) != 2 || reqs[0].Header.Get("Authorization") != "Bearer one" || reqs[1].Header.Get("Authorization") != "Bearer two" {
		t.Fatalf("expected a 401 retry with the new token, got %d requests", len(reqs))
	}
	// Within a minute the file is not read again.
	if err := os.WriteFile(tokenFile, []byte("three"), 0o600); err != nil {
		t.Fatal(err)
	}
	api.setToken("two")
	before = len(api.recorded())
	if err := c.Get(ctx, Pods, "ns", "a", &pod); err != nil {
		t.Fatal(err)
	}
	if reqs := api.recorded()[before:]; len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer two" {
		t.Fatalf("token re-read too early: %d requests", len(reqs))
	}
	// After a minute it is, without a 401.
	now = now.Add(2 * time.Minute)
	api.setToken("three")
	before = len(api.recorded())
	if err := c.Get(ctx, Pods, "ns", "a", &pod); err != nil {
		t.Fatal(err)
	}
	if reqs := api.recorded()[before:]; len(reqs) != 1 || reqs[0].Header.Get("Authorization") != "Bearer three" {
		t.Fatalf("token not refreshed after a minute: %d requests %s", len(reqs), reqs[0].Header.Get("Authorization"))
	}
	// A wrong token that the file does not fix is IsUnauthorized after one retry.
	api.setToken("four")
	before = len(api.recorded())
	if err := c.Get(ctx, Pods, "ns", "a", &pod); !IsUnauthorized(err) {
		t.Fatalf("want Unauthorized, got %v", err)
	}
	if reqs := api.recorded()[before:]; len(reqs) != 2 {
		t.Fatalf("expected exactly one retry, got %d requests", len(reqs))
	}
	// A static token is never retried.
	api.setToken("static")
	static := api.client()
	before = len(api.recorded())
	if err := static.Get(ctx, Pods, "ns", "a", &pod); err != nil {
		t.Fatal(err)
	}
	api.setToken("rotated")
	if err := static.Get(ctx, Pods, "ns", "a", &pod); !IsUnauthorized(err) {
		t.Fatalf("want Unauthorized, got %v", err)
	}
	if reqs := api.recorded()[before:]; len(reqs) != 2 {
		t.Fatalf("static token retried: %d requests", len(reqs))
	}
}

func TestTypedDecoding(t *testing.T) {
	// A pod as the API server returns it, with the fields the verifier and
	// the driver read.
	raw := `{
	  "apiVersion": "v1", "kind": "Pod",
	  "metadata": {"name": "sbx-1", "namespace": "warden-sandboxes", "uid": "8f1c", "resourceVersion": "4242",
	    "labels": {"warden.monaddle.com/sandbox": "sbx-1", "warden.monaddle.com/egress": "gateway"},
	    "annotations": {"warden.monaddle.com/generation": "3"}, "creationTimestamp": "2026-09-16T10:00:00Z"},
	  "spec": {
	    "runtimeClassName": "gvisor", "automountServiceAccountToken": false, "enableServiceLinks": false,
	    "nodeSelector": {"kata": "true"}, "tolerations": [{"key": "kata", "operator": "Exists", "effect": "NoSchedule"}],
	    "securityContext": {"runAsUser": 1000, "runAsGroup": 1000, "fsGroup": 1000, "seccompProfile": {"type": "RuntimeDefault"}},
	    "containers": [{"name": "guest", "image": "ghcr.io/monaddle-too/warden-guest-base@sha256:abc",
	      "command": ["sleep", "infinity"], "env": [{"name": "HOME", "value": "/home/agent"}],
	      "volumeMounts": [{"name": "home", "mountPath": "/home/agent"}, {"name": "trust", "mountPath": "/opt/warden/trust", "readOnly": true}],
	      "resources": {"limits": {"cpu": "1", "memory": "1536Mi"}, "requests": {"cpu": "1", "memory": "1536Mi"}},
	      "securityContext": {"allowPrivilegeEscalation": true, "privileged": false, "capabilities": {"drop": ["ALL"]}}}],
	    "volumes": [{"name": "home", "persistentVolumeClaim": {"claimName": "sbx-1"}},
	      {"name": "trust", "configMap": {"name": "warden-guest-trust"}},
	      {"name": "tmp", "emptyDir": {"sizeLimit": "1Gi"}},
	      {"name": "bad", "hostPath": {"path": "/", "type": "Directory"}},
	      {"name": "token", "projected": {"sources": [{"serviceAccountToken": {"path": "token", "expirationSeconds": 3600}}]}},
	      {"name": "nfs", "nfs": {"server": "x", "path": "/y"}}],
	    "hostNetwork": false, "nodeName": "lima-warden-k8s"
	  },
	  "status": {"phase": "Running", "podIP": "10.42.0.9", "podIPs": [{"ip": "10.42.0.9"}], "hostIP": "192.168.5.15",
	    "conditions": [{"type": "Ready", "status": "True", "lastTransitionTime": "2026-09-16T10:00:05Z"}],
	    "containerStatuses": [{"name": "guest", "image": "ghcr.io/monaddle-too/warden-guest-base:latest",
	      "imageID": "ghcr.io/monaddle-too/warden-guest-base@sha256:abc", "containerID": "containerd://1", "ready": true, "started": true,
	      "restartCount": 0, "state": {"running": {"startedAt": "2026-09-16T10:00:04Z"}}}]}
	}`
	var pod Pod
	if err := json.Unmarshal([]byte(raw), &pod); err != nil {
		t.Fatal(err)
	}
	if pod.Metadata.UID != "8f1c" || pod.Metadata.Annotations["warden.monaddle.com/generation"] != "3" || pod.Metadata.CreationTimestamp.Year() != 2026 {
		t.Fatalf("metadata %+v", pod.Metadata)
	}
	if *pod.Spec.RuntimeClassName != "gvisor" || *pod.Spec.AutomountServiceAccountToken || pod.Spec.NodeSelector["kata"] != "true" || pod.Spec.Tolerations[0].Operator != "Exists" {
		t.Fatalf("spec %+v", pod.Spec)
	}
	if *pod.Spec.SecurityContext.RunAsUser != 1000 || pod.Spec.SecurityContext.SeccompProfile.Type != "RuntimeDefault" {
		t.Fatalf("pod security context %+v", pod.Spec.SecurityContext)
	}
	guest := pod.Spec.Containers[0]
	if !*guest.SecurityContext.AllowPrivilegeEscalation || *guest.SecurityContext.Privileged || guest.SecurityContext.Capabilities.Drop[0] != "ALL" || guest.VolumeMounts[1].ReadOnly != true {
		t.Fatalf("container %+v", guest)
	}
	v := pod.Spec.Volumes
	if v[0].PersistentVolumeClaim.ClaimName != "sbx-1" || v[1].ConfigMap.Name != "warden-guest-trust" || v[2].EmptyDir.SizeLimit != "1Gi" || v[3].HostPath.Path != "/" || v[4].Projected.Sources[0].ServiceAccountToken.Path != "token" {
		t.Fatalf("volumes %+v", v)
	}
	if nfs := v[5]; nfs.PersistentVolumeClaim != nil || nfs.ConfigMap != nil || nfs.EmptyDir != nil || nfs.Secret != nil || nfs.HostPath != nil || nfs.Projected != nil {
		t.Fatal("an unknown volume type must decode with every source nil")
	}
	if pod.Status.Phase != "Running" || pod.Status.PodIP != "10.42.0.9" || pod.Status.Conditions[0].Type != "Ready" || pod.Status.ContainerStatuses[0].ImageID != "ghcr.io/monaddle-too/warden-guest-base@sha256:abc" || !*pod.Status.ContainerStatuses[0].Started || pod.Status.ContainerStatuses[0].State.Running.StartedAt == nil {
		t.Fatalf("status %+v", pod.Status)
	}
	// Round trip keeps the fields and omits the unset ones.
	out, err := json.Marshal(Pod{Metadata: ObjectMeta{Name: "x"}, Spec: PodSpec{Containers: []Container{{Name: "c", Image: "i"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(out) != `{"metadata":{"name":"x"},"spec":{"containers":[{"name":"c","image":"i","resources":{}}]},"status":{}}` {
		t.Fatalf("marshal %s", out)
	}

	var policy NetworkPolicy
	if err := json.Unmarshal([]byte(`{"apiVersion":"networking.k8s.io/v1","kind":"NetworkPolicy","metadata":{"name":"gateway-egress"},
	  "spec":{"podSelector":{"matchLabels":{"warden.monaddle.com/egress":"gateway"}},"policyTypes":["Egress"],
	  "egress":[{"to":[{"podSelector":{"matchLabels":{"app":"warden-policy"}},"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"warden"}}},{"ipBlock":{"cidr":"10.43.0.0/16","except":["10.43.0.10/32"]}}],
	  "ports":[{"protocol":"TCP","port":7446},{"port":"gateway","endPort":7450}]}]}}`), &policy); err != nil {
		t.Fatal(err)
	}
	rule := policy.Spec.Egress[0]
	if !policy.Spec.PodSelector.Matches(map[string]string{"warden.monaddle.com/egress": "gateway", "x": "y"}) || policy.Spec.PolicyTypes[0] != "Egress" {
		t.Fatalf("policy %+v", policy.Spec)
	}
	if rule.To[0].NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "warden" || rule.To[1].IPBlock.Except[0] != "10.43.0.10/32" {
		t.Fatalf("peers %+v", rule.To)
	}
	if rule.Ports[0].Port.IntVal != 7446 || rule.Ports[0].Port.IsString || *rule.Ports[0].Protocol != "TCP" || rule.Ports[1].Port.StrVal != "gateway" || *rule.Ports[1].EndPort != 7450 {
		t.Fatalf("ports %+v", rule.Ports)
	}
	if out, _ := json.Marshal(rule.Ports); string(out) != `[{"protocol":"TCP","port":7446},{"port":"gateway","endPort":7450}]` {
		t.Fatalf("ports marshal %s", out)
	}

	var vap ValidatingAdmissionPolicy
	if err := json.Unmarshal([]byte(`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicy","metadata":{"name":"warden-sandbox"},
	  "spec":{"failurePolicy":"Fail","matchConstraints":{"resourceRules":[{"apiGroups":[""],"apiVersions":["v1"],"operations":["CREATE","UPDATE"],"resources":["pods"]}],
	  "namespaceSelector":{"matchExpressions":[{"key":"kubernetes.io/metadata.name","operator":"In","values":["warden-sandboxes"]}]}},
	  "validations":[{"expression":"object.spec.runtimeClassName == 'gvisor'","message":"runtime class"},{"expression":"!has(object.spec.hostNetwork) || !object.spec.hostNetwork"}]}}`), &vap); err != nil {
		t.Fatal(err)
	}
	if *vap.Spec.FailurePolicy != "Fail" || vap.Spec.MatchConstraints.ResourceRules[0].Resources[0] != "pods" || len(vap.Spec.Validations) != 2 || vap.Spec.Validations[0].Message != "runtime class" {
		t.Fatalf("policy %+v", vap.Spec)
	}
	if !vap.Spec.MatchConstraints.NamespaceSelector.Matches(map[string]string{"kubernetes.io/metadata.name": "warden-sandboxes"}) {
		t.Fatal("namespace selector")
	}
	var binding ValidatingAdmissionPolicyBinding
	if err := json.Unmarshal([]byte(`{"apiVersion":"admissionregistration.k8s.io/v1","kind":"ValidatingAdmissionPolicyBinding","metadata":{"name":"warden-sandbox"},
	  "spec":{"policyName":"warden-sandbox","validationActions":["Deny"],"matchResources":{"namespaceSelector":{"matchLabels":{"kubernetes.io/metadata.name":"warden-sandboxes"}}}}}`), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Spec.PolicyName != "warden-sandbox" || binding.Spec.ValidationActions[0] != "Deny" || binding.Spec.MatchResources.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] != "warden-sandboxes" {
		t.Fatalf("binding %+v", binding.Spec)
	}

	var pvc PersistentVolumeClaim
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"sbx-2","uid":"u2"},"spec":{"accessModes":["ReadWriteOnce"],"storageClassName":"local-path",
	  "resources":{"requests":{"storage":"20Gi"}},"dataSource":{"apiGroup":null,"kind":"PersistentVolumeClaim","name":"sbx-1"}},"status":{"phase":"Bound","capacity":{"storage":"20Gi"}}}`), &pvc); err != nil {
		t.Fatal(err)
	}
	if *pvc.Spec.StorageClassName != "local-path" || pvc.Spec.DataSource.Kind != "PersistentVolumeClaim" || pvc.Spec.DataSource.APIGroup != nil || pvc.Spec.Resources.Requests["storage"] != "20Gi" || pvc.Status.Phase != "Bound" {
		t.Fatalf("pvc %+v", pvc)
	}
	if out, _ := json.Marshal(pvc.Spec.DataSource); string(out) != `{"apiGroup":null,"kind":"PersistentVolumeClaim","name":"sbx-1"}` {
		t.Fatalf("dataSource %s", out)
	}

	var secret Secret
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"warden-codex-login"},"type":"Opaque","data":{"auth.json":"eyJ0b2tlbiI6MX0="}}`), &secret); err != nil {
		t.Fatal(err)
	}
	if string(secret.Data["auth.json"]) != `{"token":1}` {
		t.Fatalf("secret data %q", secret.Data["auth.json"])
	}
	var cm ConfigMap
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"x"},"data":{"a":"b"},"binaryData":{"c":"AQI="}}`), &cm); err != nil {
		t.Fatal(err)
	}
	if cm.Data["a"] != "b" || string(cm.BinaryData["c"]) != "\x01\x02" {
		t.Fatalf("configmap %+v", cm)
	}
	var rc RuntimeClass
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"gvisor"},"handler":"runsc","scheduling":{"nodeSelector":{"gvisor":"true"}}}`), &rc); err != nil {
		t.Fatal(err)
	}
	if rc.Handler != "runsc" || rc.Scheduling.NodeSelector["gvisor"] != "true" {
		t.Fatalf("runtime class %+v", rc)
	}
	var ns Namespace
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"warden-sandboxes","labels":{"pod-security.kubernetes.io/enforce":"baseline"}},"status":{"phase":"Active"}}`), &ns); err != nil {
		t.Fatal(err)
	}
	if ns.Metadata.Labels["pod-security.kubernetes.io/enforce"] != "baseline" || ns.Status.Phase != "Active" {
		t.Fatalf("namespace %+v", ns)
	}
}

func TestLabelSelectorMatches(t *testing.T) {
	labels := map[string]string{"app": "warden", "tier": "gvisor"}
	cases := []struct {
		sel  LabelSelector
		want bool
	}{
		{LabelSelector{}, true},
		{LabelSelector{MatchLabels: map[string]string{"app": "warden"}}, true},
		{LabelSelector{MatchLabels: map[string]string{"app": "other"}}, false},
		{LabelSelector{MatchLabels: map[string]string{"missing": ""}}, false},
		{LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "tier", Operator: "In", Values: []string{"kata", "gvisor"}}}}, true},
		{LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "tier", Operator: "NotIn", Values: []string{"gvisor"}}}}, false},
		{LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "missing", Operator: "NotIn", Values: []string{"x"}}}}, true},
		{LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "app", Operator: "Exists"}}}, true},
		{LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "app", Operator: "DoesNotExist"}}}, false},
		{LabelSelector{MatchExpressions: []LabelSelectorRequirement{{Key: "app", Operator: "Bogus"}}}, false},
	}
	for i, c := range cases {
		if got := c.sel.Matches(labels); got != c.want {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}

func TestIntOrString(t *testing.T) {
	var v struct {
		Port IntOrString `json:"port"`
	}
	if err := json.Unmarshal([]byte(`{"port":"http"}`), &v); err != nil || !v.Port.IsString || v.Port.String() != "http" {
		t.Fatalf("%+v %v", v, err)
	}
	if err := json.Unmarshal([]byte(`{"port":80}`), &v); err != nil || v.Port.IsString || v.Port.IntVal != 80 || v.Port.String() != "80" {
		t.Fatalf("%+v %v", v, err)
	}
	if out, _ := json.Marshal(FromString("x")); string(out) != `"x"` {
		t.Fatal(string(out))
	}
	if out, _ := json.Marshal(FromInt(7)); string(out) != `7` {
		t.Fatal(string(out))
	}
}
