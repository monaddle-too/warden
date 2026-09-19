package sandbox

import (
	"strings"
	"testing"
)

// The agent's process tree is pointed at Warden's gateway in the proxy
// variables and, for the JVM that ignores them, in JAVA_TOOL_OPTIONS.
func TestProxyEnvironmentCoversTheJVM(t *testing.T) {
	broker := BrokerConfig{ProxyURL: "http://host.docker.internal:57348"}
	env := strings.Join(proxyEnvironment(broker), "\n")
	for _, want := range []string{
		"https_proxy=http://host.docker.internal:57348",
		"HTTPS_PROXY=http://host.docker.internal:57348",
		"JAVA_TOOL_OPTIONS=-Dhttp.proxyHost=host.docker.internal -Dhttp.proxyPort=57348 -Dhttps.proxyHost=host.docker.internal -Dhttps.proxyPort=57348 -Dhttp.nonProxyHosts=localhost|127.*|[::1]",
	} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing %q in\n%s", want, env)
		}
	}
	if got := javaProxyOptions(""); got != "" {
		t.Fatalf("empty proxy rendered %q", got)
	}
	if got := javaProxyOptions("http://proxy.example"); !strings.Contains(got, "-Dhttp.proxyPort=80") {
		t.Fatalf("default port missing: %q", got)
	}
	claude := strings.Join(claudeEnvironment(broker), "\n")
	if !strings.Contains(claude, "JAVA_TOOL_OPTIONS=") {
		t.Fatal("claude launch lacks JAVA_TOOL_OPTIONS")
	}
}
