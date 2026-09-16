package release

import (
	"strings"
	"testing"
)

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") || len(value) != len("sha256:")+64 {
		return false
	}
	return strings.Trim(value[len("sha256:"):], "0123456789abcdef") == ""
}

func TestRuntimesArePinnedPerArchitecture(t *testing.T) {
	for arch, target := range map[string]string{AMD64: "x86_64-unknown-linux-musl", ARM64: "aarch64-unknown-linux-musl"} {
		codex := CodexBundles[arch]
		if !strings.Contains(codex.URL, "rust-v"+CodexVersion+"/codex-package-"+target+".tar.gz") || len(codex.SHA256) != 64 {
			t.Fatalf("%s Codex bundle: %+v", arch, codex)
		}
	}
	for arch, platform := range map[string]string{AMD64: "linux-x64", ARM64: "linux-arm64"} {
		claude := ClaudeExecutables[arch]
		if !strings.Contains(claude.URL, "/"+ClaudeVersion+"/"+platform+"/claude") || len(claude.SHA256) != 64 {
			t.Fatalf("%s Claude executable: %+v", arch, claude)
		}
	}
	if CodexBundles[AMD64].SHA256 == CodexBundles[ARM64].SHA256 || ClaudeExecutables[AMD64].SHA256 == ClaudeExecutables[ARM64].SHA256 {
		t.Fatal("architectures must not share a checksum")
	}
}

func TestStockTemplateDigests(t *testing.T) {
	if !validDigest(StockTemplateDigest) {
		t.Fatal(StockTemplateDigest)
	}
	for arch, digest := range StockTemplatePlatformDigests {
		if !validDigest(digest) || digest == StockTemplateDigest {
			t.Fatalf("%s: %s", arch, digest)
		}
	}
	if got := StockTemplateDigests(AMD64); len(got) != 1 || got[0] != StockTemplateDigest {
		t.Fatalf("amd64: %v", got)
	}
	if got := StockTemplateDigests(ARM64); len(got) != 2 || got[0] != StockTemplateDigest || got[1] != StockTemplatePlatformDigests[ARM64] {
		t.Fatalf("arm64: %v", got)
	}
	if got := StockTemplateDigests("mips"); len(got) != 1 {
		t.Fatalf("unknown: %v", got)
	}
}
