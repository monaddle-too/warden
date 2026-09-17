package config

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

// The Helm chart renders warden.json for the Kubernetes shape
// (docs/warden-kubernetes-plan.md, decision 13 and appendix A). This test
// renders the chart with the dev values and checks the file's shape. It
// does not go through Parse yet: the kind-kubernetes fields (runtime.kind,
// kubernetes.*, providers.<p>.secret) land with the driver track, and
// until then a strict decoder refuses them.
//
// TODO(k8s driver): once runtime.kind "kubernetes" validates, replace the
// key check with Parse and assert the transport URLs, the TLS paths and
// the kubernetes section, as TestOVHExampleParses does for the sbx shape.
func TestHelmChartRendersKubernetesConfig(t *testing.T) {
	helm, err := exec.LookPath("helm")
	if err != nil {
		t.Skip("helm not on PATH")
	}
	cmd := exec.Command(helm, "template", "warden", "../../../deploy/helm/warden",
		"--namespace", "warden",
		"-f", "../../../deploy/k8s/dev/values.yaml",
		"-s", "templates/configmap.yaml")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helm template: %v\n%s", err, out)
	}
	raw := configMapFile(t, string(out), "warden.json")
	var top map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &top); err != nil {
		t.Fatalf("warden.json from the chart is not JSON: %v\n%s", err, raw)
	}
	var version int
	if err := json.Unmarshal(top["version"], &version); err != nil || version != Version {
		t.Fatalf("version = %s, want %d", top["version"], Version)
	}
	for _, key := range []string{"runtime", "services", "tls", "kubernetes", "paths", "sandboxes", "previews", "auth", "providers"} {
		if _, ok := top[key]; !ok {
			t.Errorf("warden.json lacks %q", key)
		}
	}
	var runtime struct{ Kind string }
	if err := json.Unmarshal(top["runtime"], &runtime); err != nil || runtime.Kind != "kubernetes" {
		t.Errorf("runtime = %s, want kind kubernetes", top["runtime"])
	}
	var services Services
	if err := json.Unmarshal(top["services"], &services); err != nil {
		t.Fatal(err)
	}
	for name, s := range map[string]Service{"policy": services.Policy, "runner": services.Runner, "chat": services.Chat} {
		if !strings.HasPrefix(s.Listen, "tls://0.0.0.0:") || !strings.HasPrefix(s.Address, "tls://warden-"+name+":") {
			t.Errorf("services.%s = %+v, want tls:// listener and address", name, s)
		}
	}
	var tls TLS
	if err := json.Unmarshal(top["tls"], &tls); err != nil || tls.CAFile != "/etc/warden/tls/ca.crt" || tls.CertFile != "/etc/warden/tls/tls.crt" || tls.KeyFile != "/etc/warden/tls/tls.key" {
		t.Errorf("tls = %s", top["tls"])
	}
	var paths Paths
	if err := json.Unmarshal(top["paths"], &paths); err != nil || paths.State != "/var/lib/warden" {
		t.Errorf("paths = %s", top["paths"])
	}
}

// configMapFile extracts one literal-block data entry from a rendered
// ConfigMap without a YAML parser: the lines indented under
// "  <key>: |-".
func configMapFile(t *testing.T, rendered, key string) string {
	t.Helper()
	lines := strings.Split(rendered, "\n")
	for i, line := range lines {
		if line != "  "+key+": |-" && line != "  "+key+": |" {
			continue
		}
		var b strings.Builder
		for _, l := range lines[i+1:] {
			if strings.TrimSpace(l) != "" && !strings.HasPrefix(l, "    ") {
				break
			}
			b.WriteString(strings.TrimPrefix(l, "    "))
			b.WriteByte('\n')
		}
		return b.String()
	}
	t.Fatalf("no %q entry in the rendered ConfigMap:\n%s", key, rendered)
	return ""
}
