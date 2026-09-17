package kube

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	api "warden/chat/internal/kube"
)

func TestParseCanaryLog(t *testing.T) {
	good := "starting\nwarden-canary gateway closed\nwarden-canary apiserver closed\nnoise line\nwarden-canary dns closed\nwarden-canary external closed\nwarden-canary done\n"
	out, err := ParseCanaryLog(strings.NewReader(good))
	if err != nil || out["gateway"] != "closed" || out["external"] != "closed" || len(out) != 4 {
		t.Fatalf("%v %v", out, err)
	}
	bad := map[string]string{
		"no done":         "warden-canary gateway closed\nwarden-canary apiserver closed\nwarden-canary dns closed\nwarden-canary external closed\n",
		"missing probe":   "warden-canary gateway closed\nwarden-canary apiserver closed\nwarden-canary dns closed\nwarden-canary done\n",
		"unknown verdict": "warden-canary gateway maybe\nwarden-canary apiserver closed\nwarden-canary dns closed\nwarden-canary external closed\nwarden-canary done\n",
		"duplicate probe": "warden-canary gateway closed\nwarden-canary gateway open\nwarden-canary apiserver closed\nwarden-canary dns closed\nwarden-canary external closed\nwarden-canary done\n",
		"empty":           "",
	}
	for name, text := range bad {
		if _, err := ParseCanaryLog(strings.NewReader(text)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	// A probe that could not run reports unknown, which parses and fails
	// classification.
	out, err = ParseCanaryLog(strings.NewReader("warden-canary gateway closed\nwarden-canary apiserver closed\nwarden-canary dns unknown\nwarden-canary external closed\nwarden-canary done\n"))
	if err != nil || out["dns"] != "unknown" {
		t.Fatalf("unknown verdict: %v %v", out, err)
	}
}

func TestClassifyCanaries(t *testing.T) {
	deny, gateway := enforcedVerdicts(CanaryDeny), enforcedVerdicts(CanaryGateway)
	if err := ClassifyCanaries(deny, gateway); err != nil {
		t.Fatal(err)
	}
	with := func(base map[string]string, k, v string) map[string]string {
		out := map[string]string{}
		for kk, vv := range base {
			out[kk] = vv
		}
		out[k] = v
		return out
	}
	cases := []struct {
		name          string
		deny, gateway map[string]string
		want          string
	}{
		{"unlabelled reaches the gateway", with(deny, "gateway", "open"), gateway, "NetworkPolicy is not enforced: an unlabelled pod reached the gateway"},
		{"unlabelled reaches the API server", with(deny, "apiserver", "open"), gateway, "an unlabelled pod reached the apiserver"},
		{"unlabelled reaches DNS", with(deny, "dns", "open"), gateway, "an unlabelled pod reached the dns"},
		{"unlabelled reaches the internet", with(deny, "external", "open"), gateway, "an unlabelled pod reached the external"},
		{"unlabelled could not probe", with(deny, "dns", "unknown"), gateway, "the unlabelled canary could not probe the dns"},
		{"labelled cannot reach the gateway", deny, with(gateway, "gateway", "closed"), "the gateway-labelled pod could not reach the gateway"},
		{"labelled reaches the API server", deny, with(gateway, "apiserver", "open"), "the gateway label admits more than the gateway: a labelled pod reached the apiserver"},
		{"labelled reaches the internet", deny, with(gateway, "external", "open"), "a labelled pod reached the external"},
		{"labelled could not probe", deny, with(gateway, "external", "unknown"), "the labelled canary could not probe the external"},
		{"nothing reported", nil, nil, "the unlabelled canary could not probe the gateway"},
	}
	for _, tc := range cases {
		err := ClassifyCanaries(tc.deny, tc.gateway)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: got %v, want %q", tc.name, err, tc.want)
		}
	}
}

// The proof fails, naming the canary, when a canary pod fails, is not
// admitted, or never reports.
func TestCanaryPodFailures(t *testing.T) {
	t.Run("pod failed", func(t *testing.T) {
		f := cluster(t)
		f.mu.Lock()
		f.onCreate = func(res apiPath, obj map[string]any) {
			obj["status"] = map[string]any{"phase": "Failed", "containerStatuses": []any{map[string]any{"name": "canary", "state": map[string]any{"terminated": map[string]any{"exitCode": 1, "reason": "Error"}}}}}
		}
		f.mu.Unlock()
		i := newInspector(t, f, nil)
		err := i.ClusterFacts(ctxT(t))
		if err == nil || !strings.Contains(err.Error(), "canary proof: canary ") || !strings.Contains(err.Error(), "pod Error") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("image cannot be pulled", func(t *testing.T) {
		f := cluster(t)
		f.mu.Lock()
		f.onCreate = func(res apiPath, obj map[string]any) {
			obj["status"] = map[string]any{"phase": "Pending", "containerStatuses": []any{map[string]any{"name": "canary", "state": map[string]any{"waiting": map[string]any{"reason": "ImagePullBackOff"}}}}}
		}
		f.mu.Unlock()
		i := newInspector(t, f, nil)
		err := i.ClusterFacts(ctxT(t))
		if err == nil || !strings.Contains(err.Error(), "pod ImagePullBackOff") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("refused by admission", func(t *testing.T) {
		f := cluster(t)
		f.mu.Lock()
		f.before = func(w http.ResponseWriter, r *http.Request) bool {
			if r.Method == "POST" && strings.HasSuffix(r.URL.Path, "/pods") {
				writeStatus(w, 403, "Forbidden", "sandbox pods must set runtimeClassName gvisor")
				return true
			}
			return false
		}
		f.mu.Unlock()
		i := newInspector(t, f, nil)
		err := i.ClusterFacts(ctxT(t))
		if err == nil || !strings.Contains(err.Error(), "could not be created") {
			t.Fatalf("%v", err)
		}
	})
	t.Run("never finishes", func(t *testing.T) {
		f := cluster(t)
		f.mu.Lock()
		f.onCreate = func(res apiPath, obj map[string]any) { obj["status"] = map[string]any{"phase": "Running"} }
		f.mu.Unlock()
		i := newInspector(t, f, func(o *Options) { o.Canary.Timeout = 200 * time.Millisecond })
		err := i.ClusterFacts(ctxT(t))
		if err == nil || !strings.Contains(err.Error(), "did not finish in time") {
			t.Fatalf("%v", err)
		}
		var pods api.List[api.Pod]
		if err := f.client().List(ctxT(t), api.Pods, testNamespace, api.ListOptions{}, &pods); err != nil || len(pods.Items) != 0 {
			t.Fatalf("canaries not deleted after a timeout: %d", len(pods.Items))
		}
	})
	t.Run("gateway unknown", func(t *testing.T) {
		f := cluster(t)
		i := newInspector(t, f, func(o *Options) { o.Canary.GatewayHost = "" })
		if err := i.ClusterFacts(ctxT(t)); err == nil || !strings.Contains(err.Error(), "gateway address is unknown") {
			t.Fatalf("%v", err)
		}
		i.SetGateway(testGateway)
		i.Invalidate()
		if err := i.ClusterFacts(ctxT(t)); err != nil {
			t.Fatal(err)
		}
	})
}

// The canaries get the gateway, API server and external addresses as
// environment, and the script probes the pod's own resolver for DNS.
func TestCanaryEnvironment(t *testing.T) {
	f := cluster(t)
	var created []api.Pod
	f.mu.Lock()
	prev := f.onCreate
	f.onCreate = func(res apiPath, obj map[string]any) {
		var pod api.Pod
		raw := toMap(t, obj)
		_ = decode(raw, &pod)
		created = append(created, pod)
		prev(res, obj)
	}
	f.mu.Unlock()
	i := newInspector(t, f, func(o *Options) { o.Canary.External = "9.9.9.9:853" })
	i.SetGateway("10.43.0.42")
	if err := i.ClusterFacts(ctxT(t)); err != nil {
		t.Fatal(err)
	}
	if len(created) != 2 {
		t.Fatalf("created %d", len(created))
	}
	env := map[string]string{}
	for _, e := range created[0].Spec.Containers[0].Env {
		env[e.Name] = e.Value
	}
	if env["GATEWAY_HOST"] != "10.43.0.42" || env["GATEWAY_PORT"] != "7000" || env["API_HOST"] != "10.43.0.1" || env["API_PORT"] != "443" || env["EXTERNAL_HOST"] != "9.9.9.9" || env["EXTERNAL_PORT"] != "853" {
		t.Fatalf("env: %v", env)
	}
	if !strings.Contains(created[0].Spec.Containers[0].Command[2], "/etc/resolv.conf") {
		t.Fatal("DNS probe does not use the resolver")
	}
}

func decode(raw map[string]any, into any) error {
	b, err := json.Marshal(raw)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, into)
}
