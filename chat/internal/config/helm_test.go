package config

import (
	"os/exec"
	"strings"
	"testing"
)

// The Helm chart renders warden.json for the Kubernetes shape
// (docs/warden-kubernetes-plan.md, decision 13 and appendix A). This test
// renders the chart with the dev values and loads the result through
// Parse, as TestOVHExampleParses does for the sbx shape.
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
	c, err := Parse([]byte(raw))
	if err != nil {
		t.Fatalf("warden.json from the chart does not parse: %v\n%s", err, raw)
	}
	if c.RuntimeKind() != RuntimeKubernetes || c.GatewayMode() != GatewayShared {
		t.Fatalf("kind: %+v", c.Runtime)
	}
	for name, s := range map[string]Service{"policy": c.Services.Policy, "runner": c.Services.Runner.Service, "chat": c.Services.Chat} {
		if !strings.HasPrefix(s.Listen, "tls://0.0.0.0:") || !strings.HasPrefix(s.Address, "tls://warden-"+name+":") {
			t.Errorf("services.%s = %+v, want tls:// listener and address", name, s)
		}
	}
	// The runner's shared preview server (decisions 5 and 10) on its own
	// port, which the chat dials as https://warden-runner:<port>/<id>.
	if c.RunnerPreviewListen() != "tls://0.0.0.0:7446" || c.RunnerPreviewAddress() != "tls://warden-runner:7446" {
		t.Errorf("services.runner.previews = %+v", c.Services.Runner.Previews)
	}
	if c.TLS == nil || c.TLS.CAFile != "/etc/warden/tls/ca.crt" || c.TLS.CertFile != "/etc/warden/tls/tls.crt" || c.TLS.KeyFile != "/etc/warden/tls/tls.key" {
		t.Errorf("tls = %+v", c.TLS)
	}
	if c.Paths.State != "/var/lib/warden" {
		t.Errorf("paths = %+v", c.Paths)
	}
	k := c.Kubernetes
	if k == nil || k.Namespace != "warden-sandboxes" || k.Tier != TierGVisor || k.RuntimeClass != "gvisor" || k.GuestImage == "" || !digestShape(k.GuestImageDigest) || k.StorageClass != "local-path" || k.WorkspaceSizeGi != 4 || k.GatewayService != "warden-gateway" || k.GatewayPort != 7000 || k.TrustConfigMap != "warden-guest-trust" {
		t.Errorf("kubernetes = %+v", k)
	}
	if c.Providers.Codex == nil || c.Providers.Codex.Secret != "warden-codex-login" || c.Providers.GitHub == nil || c.Providers.GitHub.Secret != "warden-github-login" {
		t.Errorf("providers = %+v %+v", c.Providers.Codex, c.Providers.GitHub)
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
