package release

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// guestDir is deploy/guest relative to this package directory (go test runs
// with the package directory as its working directory).
const guestDir = "../../../deploy/guest"

// dockerfileArgs returns the NAME=VALUE pairs of every ARG line in a
// Dockerfile, in declaration order.
func dockerfileArgs(t *testing.T, name string) (map[string]string, []string) {
	t.Helper()
	file, err := os.Open(filepath.Join(guestDir, name))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	args := map[string]string{}
	var order []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "ARG ") {
			continue
		}
		name, value, ok := strings.Cut(strings.TrimSpace(strings.TrimPrefix(line, "ARG ")), "=")
		if !ok {
			continue // ARG TARGETARCH: a build-time value with no default
		}
		if _, dup := args[name]; dup {
			t.Fatalf("%s declares ARG %s twice", file.Name(), name)
		}
		args[name] = strings.Trim(value, `"`)
		order = append(order, name)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	return args, order
}

// dockerfileFrom returns the image reference of the first FROM line.
func dockerfileFrom(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(guestDir, name))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if fields := strings.Fields(line); len(fields) >= 2 && fields[0] == "FROM" {
			return fields[1]
		}
	}
	t.Fatalf("%s has no FROM line", name)
	return ""
}

// pinnedArgs are the ARGs both guest Dockerfiles must declare with the same
// values, mapped to the release constant each must equal.
func pinnedArgs() map[string]string {
	return map[string]string{
		"CODEX_VERSION":              CodexVersion,
		"CODEX_PACKAGE_SHA256_AMD64": CodexBundles[AMD64].SHA256,
		"CODEX_PACKAGE_SHA256_ARM64": CodexBundles[ARM64].SHA256,
		"CLAUDE_VERSION":             ClaudeVersion,
		"CLAUDE_SHA256_AMD64":        ClaudeExecutables[AMD64].SHA256,
		"CLAUDE_SHA256_ARM64":        ClaudeExecutables[ARM64].SHA256,
	}
}

// TestGuestDockerfilesPinReleaseRuntimes keeps deploy/guest/Dockerfile (the
// SBX variant) and deploy/guest/Dockerfile.base (the plain base image) on
// the runtimes this package pins: the Codex and Claude versions and all
// four SHA-256 values must equal each other and the constants here, so an
// image can never ship a runtime the runner would reject.
func TestGuestDockerfilesPinReleaseRuntimes(t *testing.T) {
	want := pinnedArgs()
	files := []string{"Dockerfile", "Dockerfile.base"}
	parsed := map[string]map[string]string{}
	for _, name := range files {
		args, order := dockerfileArgs(t, name)
		parsed[name] = args
		for arg, value := range want {
			got, ok := args[arg]
			if !ok {
				t.Errorf("%s: missing ARG %s", name, arg)
				continue
			}
			if got != value {
				t.Errorf("%s: ARG %s=%s, release pins %s", name, arg, got, value)
			}
		}
		for _, arg := range order {
			if _, known := want[arg]; !known {
				t.Errorf("%s: ARG %s is not a pinned runtime value this test knows; add it to pinnedArgs or drop it", name, arg)
			}
		}
	}
	for arg := range want {
		if parsed["Dockerfile"][arg] != parsed["Dockerfile.base"][arg] {
			t.Errorf("ARG %s differs: Dockerfile %s, Dockerfile.base %s", arg, parsed["Dockerfile"][arg], parsed["Dockerfile.base"][arg])
		}
	}
	if from := dockerfileFrom(t, "Dockerfile"); from != StockTemplate+"@"+StockTemplateDigest {
		t.Errorf("Dockerfile FROM %s; the SBX variant must build on %s@%s", from, StockTemplate, StockTemplateDigest)
	}
	if from := dockerfileFrom(t, "Dockerfile.base"); !strings.HasPrefix(from, "ubuntu:24.04@sha256:") || !validDigest(strings.TrimPrefix(from, "ubuntu:24.04@")) {
		t.Errorf("Dockerfile.base FROM %s; the base image must be ubuntu:24.04 pinned by digest", from)
	}
}

// TestGuestFetchScriptMatchesReleaseURLs expands the download URLs in
// deploy/guest/fetch-runtimes.sh, the script both Dockerfiles run, with the
// pinned versions and each architecture's target, and checks them against
// the URLs the installer downloads from.
func TestGuestFetchScriptMatchesReleaseURLs(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(guestDir, "fetch-runtimes.sh"))
	if err != nil {
		t.Fatal(err)
	}
	urlPattern := regexp.MustCompile(`curl\s.*"(https://[^"]+)"`)
	var codexURL, claudeURL string
	for _, line := range strings.Split(string(raw), "\n") {
		m := urlPattern.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch {
		case strings.Contains(m[1], "codex-package-"):
			codexURL = m[1]
		case strings.Contains(m[1], "claude-code-releases"):
			claudeURL = m[1]
		default:
			t.Errorf("unexpected download in fetch-runtimes.sh: %s", m[1])
		}
	}
	if codexURL == "" || claudeURL == "" {
		t.Fatalf("fetch-runtimes.sh: could not find both downloads (codex %q, claude %q)", codexURL, claudeURL)
	}
	expand := func(url string, vars map[string]string) string {
		for name, value := range vars {
			url = strings.ReplaceAll(url, "${"+name+"}", value)
		}
		return url
	}
	for arch, target := range map[string]string{AMD64: "x86_64-unknown-linux-musl", ARM64: "aarch64-unknown-linux-musl"} {
		if got := expand(codexURL, map[string]string{"CODEX_VERSION": CodexVersion, "CODEX_TARGET": target}); got != CodexBundles[arch].URL {
			t.Errorf("%s Codex URL from fetch-runtimes.sh %s, release %s", arch, got, CodexBundles[arch].URL)
		}
	}
	for arch, platform := range map[string]string{AMD64: "linux-x64", ARM64: "linux-arm64"} {
		if got := expand(claudeURL, map[string]string{"CLAUDE_VERSION": ClaudeVersion, "CLAUDE_PLATFORM": platform}); got != ClaudeExecutables[arch].URL {
			t.Errorf("%s Claude URL from fetch-runtimes.sh %s, release %s", arch, got, ClaudeExecutables[arch].URL)
		}
	}
	for _, name := range []string{"Dockerfile", "Dockerfile.base"} {
		file, err := os.ReadFile(filepath.Join(guestDir, name))
		if err != nil {
			t.Fatal(err)
		}
		for _, script := range []string{"fetch-runtimes.sh", "write-manifest.sh"} {
			if !strings.Contains(string(file), "source="+script+",") {
				t.Errorf("%s does not bind-mount %s; both variants must share it", name, script)
			}
		}
	}
}
